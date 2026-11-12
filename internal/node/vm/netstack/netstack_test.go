package netstack

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// streamPair returns both ends of a QEMU-like stream connection.
func streamPair(t *testing.T) (qemuSide, nodeSide net.Conn) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

// fakeGuest is a gVisor stack standing in for the VM: 10.0.2.15/24, default
// route via 10.0.2.2, frames over a QEMU stream connection.
func fakeGuest(t *testing.T, conn net.Conn, mtu int) *stack.Stack {
	return fakeGuestMode(t, conn, mtu, false)
}

// fakeGuestMode: packet = QEMU "-netdev dgram" framing (one frame per message).
func fakeGuestMode(t *testing.T, conn net.Conn, mtu int, packet bool) *stack.Stack {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	mac, _ := tcpip.ParseMACAddress(GuestMAC)
	ep := channel.New(1024, uint32(mtu+header.EthernetMinimumSize), mac)
	if err := s.CreateNIC(1, ethernet.New(ep)); err != nil {
		t.Fatal(err)
	}
	s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpAddr(GuestIP), PrefixLen: 24}}, stack.AddressProperties{})
	sub, _ := tcpip.NewSubnet(tcpAddr(netip.MustParseAddr("10.0.2.0")), tcpip.MaskFromBytes([]byte{255, 255, 255, 0}))
	s.SetRouteTable([]tcpip.Route{{Destination: sub, NIC: 1}, {Destination: header.IPv4EmptySubnet, Gateway: tcpAddr(Gateway), NIC: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Close(); ep.Close() })
	go func() {
		if packet {
			buf := make([]byte, maxFrame)
			for {
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
				ep.InjectInbound(0, pkt)
				pkt.DecRef()
			}
		}
		r := bufio.NewReader(conn)
		var hdr [4]byte
		for {
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				return
			}
			b := make([]byte, binary.BigEndian.Uint32(hdr[:]))
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(b)})
			ep.InjectInbound(0, pkt)
			pkt.DecRef()
		}
	}()
	go func() {
		for {
			pkt := ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			b := make([]byte, 4+v.Size())
			binary.BigEndian.PutUint32(b, uint32(v.Size()))
			copy(b[4:], v.AsSlice())
			v.Release()
			pkt.DecRef()
			if packet {
				b = b[4:]
			}
			if _, err := conn.Write(b); err != nil {
				return
			}
		}
	}()
	return s
}

func echoServer(t *testing.T, network, addr string) string {
	if network == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		go func() {
			buf := make([]byte, 65536)
			for {
				n, from, err := c.ReadFrom(buf)
				if err != nil {
					return
				}
				c.WriteTo(append([]byte("echo:"), buf[:n]...), from)
			}
		}()
		return c.LocalAddr().String()
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

// hostIP returns a non-loopback IPv4 address of this machine, or skips.
func hostIP(t *testing.T) netip.Addr {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() && !ip.IsLoopback() {
				return ip.Unmap()
			}
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return netip.Addr{}
}

func dialTCP(s *stack.Stack, addr netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return gonet.DialContextTCP(ctx, s, tcpip.FullAddress{NIC: 1, Addr: tcpAddr(addr.Addr()), Port: addr.Port()}, ipv4.ProtocolNumber)
}

func roundTrip(c net.Conn, msg string) (string, error) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		return "", err
	}
	buf := make([]byte, 100)
	n, err := c.Read(buf)
	return string(buf[:n]), err
}

func TestNetwork(t *testing.T) {
	own := hostIP(t)
	tcpEcho := netip.MustParseAddrPort(echoServer(t, "tcp", "0.0.0.0:0")).Port()
	udpEcho := netip.MustParseAddrPort(echoServer(t, "udp", "0.0.0.0:0")).Port()
	server := echoServer(t, "tcp", "127.0.0.1:0")

	newNet := func(p Policy) (*Network, *stack.Stack) {
		rules, err := NewRules(p)
		if err != nil {
			t.Fatal(err)
		}
		var logs []string
		n, err := New(Config{Rules: rules, MTU: 9000,
			GatewayForwards: map[uint16]string{8420: server},
			Logf:            func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(n.Close)
		q, nodeSide := streamPair(t)
		n.Attach(nodeSide)
		return n, fakeGuest(t, q, 9000)
	}

	// Default policy: this machine is unreachable, except the server via the gateway.
	n, g := newNet(Policy{})
	if c, err := dialTCP(g, netip.AddrPortFrom(Gateway, 8420)); err != nil {
		t.Fatalf("server via gateway: %v", err)
	} else if got, err := roundTrip(c, "hello"); got != "hello" {
		t.Fatalf("server via gateway: %q %v", got, err)
	}
	if c, err := dialTCP(g, netip.AddrPortFrom(own, tcpEcho)); err == nil {
		c.Close()
		t.Fatalf("own address %s reachable", own)
	}
	if c, err := dialTCP(g, netip.AddrPortFrom(Gateway, tcpEcho)); err == nil {
		c.Close()
		t.Fatal("gateway port other than the server's reachable")
	}
	if c, err := dialTCP(g, netip.MustParseAddrPort("192.168.255.254:80")); err == nil {
		c.Close()
		t.Fatal("LAN reachable")
	}

	// Guest port forward from the node.
	gl, err := gonet.ListenTCP(g, tcpip.FullAddress{NIC: 1, Port: 7070}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer gl.Close()
	go func() {
		for {
			c, err := gl.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	n.Forward(l, 7070)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := roundTrip(c, "agent"); got != "agent" {
		t.Fatalf("forward: %q %v", got, err)
	}

	// Owner allow for this machine's exact address opens TCP and UDP.
	allow, _ := ParsePrefixes([]string{own.String()})
	_, g2 := newNet(Policy{Allow: allow})
	c, err = dialTCP(g2, netip.AddrPortFrom(own, tcpEcho))
	if err != nil {
		t.Fatalf("allowed own address: %v", err)
	}
	if got, err := roundTrip(c, "tcp"); got != "tcp" {
		t.Fatalf("tcp: %q %v", got, err)
	}
	uc, err := gonet.DialUDP(g2, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpAddr(own), Port: udpEcho}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := roundTrip(uc, "udp"); got != "echo:udp" {
		t.Fatalf("udp: %q %v", got, err)
	}
}

func TestDNSRelay(t *testing.T) {
	// Upstream that answers with the query id and a fixed body.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			k, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(append(append([]byte(nil), buf[:2]...), []byte("0123456789answer")...), from)
			_ = k
		}
	}()
	upstreams = func() []string { return []string{"127.0.0.1:1", pc.LocalAddr().String()} }
	t.Cleanup(func() { upstreams = nameservers })
	rules, _ := NewRules(Policy{})
	n, err := New(Config{Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	q, nodeSide := streamPair(t)
	n.Attach(nodeSide)
	g := fakeGuest(t, q, DefaultMTU)
	uc, err := gonet.DialUDP(g, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpAddr(DNS), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := roundTrip(uc, "\xab\xcdquestion0123"); got != "\xab\xcd0123456789answer" {
		t.Fatalf("dns: %q %v", got, err)
	}
}

func TestDHCP(t *testing.T) {
	msg := make([]byte, 240)
	msg[0], msg[1], msg[2] = 1, 1, 6
	copy(msg[4:8], "\x01\x02\x03\x04")
	copy(msg[28:34], "\x52\x54\x00\x12\x34\x56")
	binary.BigEndian.PutUint32(msg[236:], dhcpMagic)
	discover := append(append([]byte(nil), msg...), 53, 1, dhcpDiscover, 255)
	r := dhcpReply(discover, 1500)
	o := dhcpOptions(r[240:])
	if r[0] != 2 || string(r[4:8]) != "\x01\x02\x03\x04" || netip.AddrFrom4([4]byte(r[16:20])) != GuestIP ||
		o[53][0] != dhcpOffer || netip.AddrFrom4([4]byte(o[3])) != Gateway || netip.AddrFrom4([4]byte(o[6])) != DNS ||
		binary.BigEndian.Uint16(o[26]) != 1500 || string(r[28:34]) != "\x52\x54\x00\x12\x34\x56" {
		t.Fatalf("offer: % x", r)
	}
	request := append(append([]byte(nil), msg...), 53, 1, dhcpRequest, 50, 4, 10, 0, 2, 15, 255)
	if r := dhcpReply(request, 1500); dhcpOptions(r[240:])[53][0] != dhcpAck {
		t.Fatal("want ACK")
	}
	wrong := append(append([]byte(nil), msg...), 53, 1, dhcpRequest, 50, 4, 192, 168, 1, 50, 255)
	if r := dhcpReply(wrong, 1500); dhcpOptions(r[240:])[53][0] != dhcpNak || netip.AddrFrom4([4]byte(r[16:20])).IsValid() && r[16] != 0 {
		t.Fatal("want NAK")
	}
	if dhcpReply(msg[:100], 1500) != nil || dhcpReply(append(append([]byte(nil), msg...), 53, 1, 7, 255), 1500) != nil {
		t.Fatal("want no reply")
	}
}

// BenchmarkDownload measures guest download throughput through the stack
// (node-local server mapped on the gateway), without a VM.
func BenchmarkDownload(b *testing.B) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	const size = 256 << 20
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 1<<20)
				for sent := 0; sent < size; sent += len(buf) {
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	rules, _ := NewRules(Policy{})
	n, _ := New(Config{Rules: rules, GatewayForwards: map[uint16]string{8420: l.Addr().String()}})
	defer n.Close()
	t := &testing.T{}
	q, nodeSide := streamPair(t)
	n.Attach(nodeSide)
	g := fakeGuest(t, q, DefaultMTU)
	b.SetBytes(size)
	for b.Loop() {
		c, err := dialTCP(g, netip.AddrPortFrom(Gateway, 8420))
		if err != nil {
			b.Fatal(err)
		}
		if k, err := io.Copy(io.Discard, c); k != size {
			b.Fatal(k, err)
		}
		c.Close()
	}
}

// Secure layer: a VM whose DNS goes through the server gets no answers from
// the node's resolver (it would be a side channel around the server).
func TestDNSRelayOffForNoDNS(t *testing.T) {
	answered := make(chan struct{}, 1)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		for {
			k, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			answered <- struct{}{}
			pc.WriteTo(buf[:k], from)
		}
	}()
	upstreams = func() []string { return []string{pc.LocalAddr().String()} }
	t.Cleanup(func() { upstreams = nameservers })
	rules, _ := NewRules(Policy{})
	n, err := New(Config{Rules: rules, Sandbox: &Sandbox{NoDNS: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	q, nodeSide := streamPair(t)
	n.Attach(nodeSide)
	g := fakeGuest(t, q, DefaultMTU)
	uc, err := gonet.DialUDP(g, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpAddr(DNS), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := roundTrip(uc, "\xab\xcdquestion0123"); err == nil {
		t.Fatalf("the node resolver answered: %q", got)
	}
	select {
	case <-answered:
		t.Fatal("the query reached the node's upstream resolver")
	default:
	}
}

// Datagram NIC (the fd-store socketpair) and an in-process gateway service
// (the boot ticket): frames cross intact, the service answers only inside
// the VM's network.
func TestPacketAttachAndGatewayService(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]net.Conn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "pair")
		if conns[i], err = net.FileConn(f); err != nil {
			t.Fatal(err)
		}
		f.Close()
		t.Cleanup(func() { conns[i].Close() })
	}
	rules, _ := NewRules(Policy{})
	n, err := New(Config{Rules: rules, MTU: 9000, GatewayServices: map[uint16]func(net.Conn){7072: func(c net.Conn) {
		c.Write([]byte("ticket-xyz\n"))
	}}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	n.AttachPacket(conns[0])
	g := fakeGuestMode(t, conns[1], 9000, true)
	c, err := dialTCP(g, netip.AddrPortFrom(Gateway, 7072))
	if err != nil {
		t.Fatalf("gateway service: %v", err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(c)
	if string(b) != "ticket-xyz\n" {
		t.Fatalf("gateway service answered %q", b)
	}
	if c, err := dialTCP(g, netip.AddrPortFrom(Gateway, 7073)); err == nil {
		c.Close()
		t.Fatal("an unserved gateway port answered")
	}
}
