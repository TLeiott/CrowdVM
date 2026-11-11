// Package netstack is the node-side userspace network for QEMU guests. Each
// VM's NIC is a QEMU "-netdev stream" socket whose frames go into a private
// gVisor network stack in the node process; nothing of the guest's traffic
// touches the host network except through the forwarders here, which apply
// the node's egress policy (see Policy) before dialing out with ordinary
// host sockets.
//
// The guest sees the same addresses as QEMU user networking: it is 10.0.2.15/24
// (DHCP), the gateway is 10.0.2.2 and DNS is 10.0.2.3. Unlike slirp, 10.0.2.2
// is not the host's loopback: only the TCP ports in Config.GatewayForwards
// (the CrowdVM server) are reachable there. DNS is relayed to the host's
// resolvers. Outbound TCP and UDP are proxied; ICMP echo is sent with
// unprivileged ping sockets (Linux) or IcmpSendEcho (Windows). IPv6 is not
// offered to the guest.
package netstack

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
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
	"gvisor.dev/gvisor/pkg/waiter"
)

// Guest-visible addresses (the same as QEMU user networking).
var (
	Subnet  = netip.MustParsePrefix("10.0.2.0/24")
	GuestIP = netip.MustParseAddr("10.0.2.15")
	Gateway = netip.MustParseAddr("10.0.2.2")
	DNS     = netip.MustParseAddr("10.0.2.3")
	// GuestMAC is the MAC address the driver gives the guest NIC.
	GuestMAC = "52:54:00:12:34:56"

	gatewayMAC = tcpip.LinkAddress("\x52\x55\x0a\x00\x02\x02")
)

const (
	nicID      = 1
	DefaultMTU = 65520 // like passt: fewer, bigger frames over the socket
	maxFrame   = 65535 + header.EthernetMinimumSize
)

// Config configures one VM's network.
type Config struct {
	Rules   *Rules   // node egress policy (required)
	Sandbox *Sandbox // optional per-VM narrowing
	// GatewayForwards maps TCP ports on the gateway (10.0.2.2) to host:port
	// targets the node dials, e.g. the CrowdVM server's port to the server.
	GatewayForwards map[uint16]string
	MTU             int // guest MTU (default DefaultMTU)
	MaxFlows        int // concurrent TCP+UDP flows (default 2048)
	Logf            func(format string, args ...any)
}

// Network is one VM's userspace network.
type Network struct {
	cfg    Config
	s      *stack.Stack
	link   *channel.Endpoint
	ctx    context.Context
	cancel context.CancelFunc
	flows  chan struct{} // semaphore
	pings  chan struct{} // semaphore

	cmu  sync.Mutex // guards conn
	conn net.Conn   // current QEMU connection
	wmu  sync.Mutex // guards w, wc; held while writing
	w    *bufio.Writer
	wc   net.Conn // the connection w writes to

	closeOnce sync.Once
	lmu       sync.Mutex
	listeners []net.Listener
	blocked   map[string]time.Time // log dedup
}

// New creates a network. Attach a QEMU stream connection to bring it up.
func New(cfg Config) (*Network, error) {
	if cfg.Rules == nil {
		return nil, errors.New("netstack: Rules required")
	}
	if cfg.MTU <= 0 {
		cfg.MTU = DefaultMTU
	}
	cfg.MTU = min(cfg.MTU, maxFrame-header.EthernetMinimumSize)
	if cfg.MaxFlows <= 0 {
		cfg.MaxFlows = 2048
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	sack := tcpip.TCPSACKEnabled(true)
	moderate := tcpip.TCPModerateReceiveBufferOption(true)
	for _, o := range []tcpip.SettableTransportProtocolOption{&sack, &moderate,
		&tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 4 << 20},
		&tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 4 << 20},
	} {
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, o); err != nil {
			return nil, fmt.Errorf("netstack: tcp option: %s", err)
		}
	}
	n := &Network{cfg: cfg, s: s, flows: make(chan struct{}, cfg.MaxFlows), pings: make(chan struct{}, 64), blocked: map[string]time.Time{}}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	n.link = channel.New(1024, uint32(cfg.MTU+header.EthernetMinimumSize), gatewayMAC)
	if err := s.CreateNIC(nicID, ethernet.New(n.link)); err != nil {
		return nil, fmt.Errorf("netstack: nic: %s", err)
	}
	s.SetPromiscuousMode(nicID, true) // accept every destination: the forwarders decide
	s.SetSpoofing(nicID, true)        // reply as the remote address
	for _, a := range []netip.Addr{Gateway, DNS} {
		if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpAddr(a), PrefixLen: Subnet.Bits()}}, stack.AddressProperties{}); err != nil {
			return nil, fmt.Errorf("netstack: address: %s", err)
		}
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcp.NewForwarder(s, 0, 512, n.handleTCP).HandlePacket)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udp.NewForwarder(s, n.handleUDP).HandlePacket)
	go n.writeLoop()
	return n, nil
}

// Close tears the network down: every guest connection is reset.
func (n *Network) Close() { n.closeOnce.Do(n.close) }

func (n *Network) close() {
	n.cancel()
	n.lmu.Lock()
	for _, l := range n.listeners {
		l.Close()
	}
	n.lmu.Unlock()
	n.cmu.Lock()
	c := n.conn
	n.conn = nil
	n.cmu.Unlock()
	if c != nil {
		c.Close() // unblocks a stuck write
	}
	n.s.Close()
	n.link.Close()
}

// Attach serves the guest NIC over conn, a QEMU "-netdev stream" connection
// (frames prefixed with a 4-byte big-endian length). It replaces a previous
// connection and returns at once; the network keeps its state when conn
// drops (e.g. the node restarts and attaches the VM again).
func (n *Network) Attach(conn net.Conn) {
	n.cmu.Lock()
	old := n.conn
	n.conn = conn
	n.cmu.Unlock()
	if old != nil {
		old.Close()
	}
	if n.ctx.Err() != nil {
		conn.Close()
		return
	}
	n.wmu.Lock()
	n.w, n.wc = bufio.NewWriterSize(conn, 256<<10), conn
	n.wmu.Unlock()
	go n.readLoop(conn)
}

func (n *Network) detach(conn net.Conn) {
	conn.Close()
	n.cmu.Lock()
	if n.conn == conn {
		n.conn = nil
	}
	n.cmu.Unlock()
	n.wmu.Lock()
	if n.wc == conn {
		n.w, n.wc = nil, nil
	}
	n.wmu.Unlock()
}

func (n *Network) readLoop(conn net.Conn) {
	defer n.detach(conn)
	r := bufio.NewReaderSize(conn, 256<<10)
	buf := make([]byte, maxFrame)
	for {
		if _, err := io.ReadFull(r, buf[:4]); err != nil {
			return
		}
		size := int(binary.BigEndian.Uint32(buf[:4]))
		if size > maxFrame {
			n.cfg.Logf("netstack: oversized frame (%d bytes); dropping connection", size)
			return
		}
		if _, err := io.ReadFull(r, buf[:size]); err != nil {
			return
		}
		n.handleFrame(buf[:size])
	}
}

// writeLoop moves frames from the stack to the QEMU connection, flushing
// whenever the stack has nothing more queued.
func (n *Network) writeLoop() {
	var hdr [4]byte
	for {
		pkt := n.link.ReadContext(n.ctx)
		if pkt == nil {
			return
		}
		n.wmu.Lock()
		if w := n.w; w != nil {
			binary.BigEndian.PutUint32(hdr[:], uint32(pkt.Size()))
			_, err := w.Write(hdr[:])
			for _, s := range pkt.AsSlices() {
				w.Write(s) // a bufio.Writer keeps its first error
			}
			if err == nil && n.link.NumQueued() == 0 {
				err = w.Flush()
			}
			n.writeErr(err)
		}
		n.wmu.Unlock()
		pkt.DecRef()
	}
}

// writeFrame sends a frame built outside the stack (DHCP, ICMP).
func (n *Network) writeFrame(frame []byte) {
	n.wmu.Lock()
	defer n.wmu.Unlock()
	if n.w == nil {
		return
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
	n.w.Write(hdr[:])
	n.w.Write(frame)
	n.writeErr(n.w.Flush())
}

// writeErr drops the connection after a failed write (wmu held); its read
// loop then detaches it.
func (n *Network) writeErr(err error) {
	if err != nil {
		n.wc.Close()
		n.w, n.wc = nil, nil
	}
}

func (n *Network) handleFrame(frame []byte) {
	if len(frame) < header.EthernetMinimumSize {
		return
	}
	eth := header.Ethernet(frame)
	switch eth.Type() {
	case header.IPv4ProtocolNumber:
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		if !ip.IsValid(len(ip)) {
			return
		}
		frag := ip.More() || ip.FragmentOffset() != 0
		switch ip.TransportProtocol() {
		case header.UDPProtocolNumber:
			if p := ip.Payload(); !frag && len(p) >= header.UDPMinimumSize && header.UDP(p).DestinationPort() == 67 {
				n.handleDHCP(eth.SourceAddress(), ip)
				return
			}
		case header.ICMPv4ProtocolNumber:
			// The stack would answer echo requests for any address (it is
			// promiscuous); only its own addresses may answer locally.
			if dst := netAddr(ip.DestinationAddress()); dst != Gateway && dst != DNS {
				if p := ip.Payload(); !frag && len(p) >= header.ICMPv4MinimumSize && header.ICMPv4(p).Type() == header.ICMPv4Echo {
					n.handleEcho(eth.SourceAddress(), ip)
				}
				return
			}
		}
	case header.ARPProtocolNumber:
	default:
		return // no IPv6
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
	n.link.InjectInbound(0, pkt)
	pkt.DecRef()
}

// ---------------------------------------------------------------------------
// Forwarding
// ---------------------------------------------------------------------------

func (n *Network) check(proto string, dst netip.AddrPort) bool {
	ok, why := n.cfg.Rules.Check(dst, n.cfg.Sandbox)
	if a := dst.Addr(); !ok && !a.IsMulticast() && a != netip.AddrFrom4([4]byte{255, 255, 255, 255}) { // no mDNS/SSDP noise
		key := proto + " " + dst.String()
		n.lmu.Lock()
		if len(n.blocked) > 1000 {
			clear(n.blocked)
		}
		last, seen := n.blocked[key]
		if !seen || time.Since(last) > time.Minute {
			n.blocked[key] = time.Now()
			n.cfg.Logf("netstack: blocked %s to %s: %s", proto, dst, why)
		}
		n.lmu.Unlock()
	}
	return ok
}

func (n *Network) acquire() bool {
	select {
	case n.flows <- struct{}{}:
		return true
	default:
		return false
	}
}

func (n *Network) release() { <-n.flows }

var dialer = net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// tcpTargets returns where a guest connection to dst goes (nil: refuse).
func (n *Network) tcpTargets(dst netip.AddrPort) []string {
	switch a := dst.Addr(); {
	case a == Gateway:
		if t, ok := n.cfg.GatewayForwards[dst.Port()]; ok {
			return []string{t}
		}
		return nil
	case a == DNS:
		if dst.Port() == 53 {
			return upstreams()
		}
		return nil
	case Subnet.Contains(a):
		return nil
	}
	if !n.check("tcp", dst) {
		return nil
	}
	return []string{dst.String()}
}

func (n *Network) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	targets := n.tcpTargets(netip.AddrPortFrom(netAddr(id.LocalAddress), id.LocalPort))
	if len(targets) == 0 || !n.acquire() {
		r.Complete(true)
		return
	}
	defer n.release()
	var out net.Conn
	var err error
	for _, t := range targets {
		if out, err = dialer.DialContext(n.ctx, "tcp", t); err == nil {
			break
		}
	}
	if err != nil {
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	r.Complete(false)
	if terr != nil {
		out.Close()
		return
	}
	splice(gonet.NewTCPConn(&wq, ep), out)
}

func (n *Network) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := netip.AddrPortFrom(netAddr(id.LocalAddress), id.LocalPort)
	isDNS := dst.Addr() == DNS && dst.Port() == 53
	if !isDNS && (Subnet.Contains(dst.Addr()) || !n.check("udp", dst)) {
		return false // ICMP port unreachable
	}
	if !n.acquire() {
		return true // drop
	}
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		n.release()
		return true
	}
	gc := gonet.NewUDPConn(&wq, ep)
	go func() {
		defer n.release()
		if isDNS {
			n.relayDNS(gc)
		} else {
			n.relayUDP(gc, dst)
		}
	}()
	return true
}

const udpIdle = 2 * time.Minute

func (n *Network) relayUDP(gc *gonet.UDPConn, dst netip.AddrPort) {
	defer gc.Close()
	hc, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dst))
	if err != nil {
		return
	}
	defer hc.Close()
	var last syncTime
	last.touch()
	pump := func(dst io.Writer, src net.Conn) {
		buf := make([]byte, 65536)
		for {
			src.SetReadDeadline(time.Now().Add(udpIdle))
			k, err := src.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() && time.Since(last.get()) < udpIdle && n.ctx.Err() == nil {
					continue
				}
				gc.Close()
				hc.Close()
				return
			}
			last.touch()
			dst.Write(buf[:k])
		}
	}
	go pump(hc, gc)
	pump(gc, hc)
}

type syncTime struct {
	mu sync.Mutex
	t  time.Time
}

func (s *syncTime) touch() { s.mu.Lock(); s.t = time.Now(); s.mu.Unlock() }
func (s *syncTime) get() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t
}

// splice copies both ways until both directions are done (half-close aware)
// or one fails, then closes both.
func splice(a, b net.Conn) {
	done := make(chan error, 2)
	cp := func(dst, src net.Conn) {
		// Wrapped so ReadFrom/WriteTo cannot swap in their 32 KiB buffer:
		// 64 KiB is ~25% faster (BenchmarkDownload) at 128 KiB per flow.
		_, err := io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, make([]byte, 64<<10))
		if cw, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- err
	}
	go cp(a, b)
	go cp(b, a)
	if err := <-done; err != nil {
		a.Close()
		b.Close()
	}
	<-done
	a.Close()
	b.Close()
}

// ---------------------------------------------------------------------------
// Guest access from the node
// ---------------------------------------------------------------------------

// DialGuest opens a TCP connection to the guest's port (on 10.0.2.15; the
// guest sees it coming from the gateway).
func (n *Network) DialGuest(ctx context.Context, port uint16) (net.Conn, error) {
	return gonet.DialContextTCP(ctx, n.s, tcpip.FullAddress{NIC: nicID, Addr: tcpAddr(GuestIP), Port: port}, ipv4.ProtocolNumber)
}

// DialGuestUDP opens a UDP "connection" to the guest's port (from the
// gateway address).
func (n *Network) DialGuestUDP(port uint16) (net.Conn, error) {
	return gonet.DialUDP(n.s, &tcpip.FullAddress{NIC: nicID, Addr: tcpAddr(Gateway)},
		&tcpip.FullAddress{NIC: nicID, Addr: tcpAddr(GuestIP), Port: port}, ipv4.ProtocolNumber)
}

// Forward relays connections accepted on l to the guest's port until the
// network closes (it closes l).
func (n *Network) Forward(l net.Listener, port uint16) {
	n.lmu.Lock()
	n.listeners = append(n.listeners, l)
	n.lmu.Unlock()
	if n.ctx.Err() != nil {
		l.Close()
		return
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
				g, err := n.DialGuest(ctx, port)
				cancel()
				if err != nil {
					c.Close()
					return
				}
				splice(c, g)
			}()
		}
	}()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func tcpAddr(a netip.Addr) tcpip.Address { return tcpip.AddrFrom4(a.As4()) }

func netAddr(a tcpip.Address) netip.Addr {
	if a.Len() == 4 {
		return netip.AddrFrom4(a.As4())
	}
	if a.Len() == 16 {
		return netip.AddrFrom16(a.As16()).Unmap()
	}
	return netip.Addr{}
}
