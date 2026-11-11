package guest

// Secure layer, egress through the server (docs/design/secure-layer.md §5.6,
// v1 over the guest channel). With GuestConfig.Egress "server" the sandbox's
// internet traffic does not leave through the node: iptables sends every TCP
// connection to a transparent proxy in this agent and DNS to a resolver in
// this agent; both forward over the guest channel, and the server connects
// out. The node sees only the TLS channel. Its netstack lets this VM reach
// nothing but the server (VMSpec.Net), so removing these rules only cuts the
// sandbox off. Non-DNS UDP is refused (QUIC falls back to TCP); ICMP is not
// carried.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/mux"
)

const (
	egressProxyPort = 15001
	egressDNSPort   = 15053
	egressChain     = "CROWDVM_EGRESS"
	// channelMark marks the agent's own channel socket, which must not be
	// redirected into itself.
	channelMark = 0x2a
)

// startEgress sets up the proxy, the resolver and the iptables rules. It
// keeps the rules in place (Docker and ufw rewrite tables) until ctx ends.
func (a *Agent) startEgress(ctx context.Context) error {
	if a.channel == nil || a.unpriv {
		return nil
	}
	tl, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", egressProxyPort))
	if err != nil {
		return err
	}
	ul, err := net.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", egressDNSPort))
	if err != nil {
		tl.Close()
		return err
	}
	dl, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", egressDNSPort))
	if err != nil {
		tl.Close()
		ul.Close()
		return err
	}
	go func() { <-ctx.Done(); tl.Close(); ul.Close(); dl.Close() }()
	go a.serveEgressProxy(tl)
	go a.serveDNSUDP(ul)
	go a.serveDNSTCP(dl)
	if err := egressRules(); err != nil {
		return err
	}
	go func() { // re-apply when something (dockerd, ufw reset) dropped the jump rules
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !egressRulesPresent() {
					if err := egressRules(); err != nil {
						a.log.Printf("egress: rules: %v", err)
					}
				}
			}
		}
	}()
	a.log.Printf("egress: through the server (proxy :%d, dns :%d)", egressProxyPort, egressDNSPort)
	return nil
}

func ipt(args ...string) error {
	out, err := exec.Command("iptables", append([]string{"-w", "5"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func egressRulesPresent() bool {
	return ipt("-t", "nat", "-C", "OUTPUT", "-j", egressChain) == nil &&
		ipt("-t", "nat", "-C", "PREROUTING", "-j", egressChain) == nil &&
		ipt("-t", "filter", "-C", "OUTPUT", "-j", egressChain) == nil &&
		ipt("-t", "filter", "-C", "FORWARD", "-j", egressChain) == nil &&
		ipt("-t", "filter", "-C", "INPUT", "-j", egressChain+"_IN") == nil
}

// egressRules (re)builds the chains and puts their jumps first.
func egressRules() error {
	mark := "0x" + strconv.FormatInt(channelMark, 16)
	nat := [][]string{
		{"-m", "mark", "--mark", mark, "-j", "RETURN"},
		{"-p", "udp", "--dport", "53", "-j", "REDIRECT", "--to-ports", strconv.Itoa(egressDNSPort)},
		{"-p", "tcp", "--dport", "53", "-j", "REDIRECT", "--to-ports", strconv.Itoa(egressDNSPort)},
		{"-m", "addrtype", "--dst-type", "LOCAL", "-j", "RETURN"},
		{"-d", "127.0.0.0/8", "-j", "RETURN"},
		{"-d", "10.0.2.0/24", "-j", "RETURN"}, // the node's gateway: DHCP and the server pinhole
		{"-p", "tcp", "-j", "REDIRECT", "--to-ports", strconv.Itoa(egressProxyPort)},
	}
	filter := [][]string{
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "RETURN"}, // e.g. the resolver's answers to containers
		{"-m", "mark", "--mark", mark, "-j", "RETURN"},
		{"-d", "127.0.0.0/8", "-j", "RETURN"},
		{"-d", "10.0.2.0/24", "-j", "RETURN"},
		{"-p", "udp", "-m", "addrtype", "!", "--dst-type", "LOCAL", "-j", "REJECT", "--reject-with", "icmp-port-unreachable"},
	}
	// Redirected traffic of containers arrives on INPUT, which ufw drops by
	// default: let it reach the proxy and the resolver. Never from the
	// node's side of the NIC.
	input := [][]string{
		{"-s", "10.0.2.0/24", "!", "-d", "127.0.0.0/8", "-m", "addrtype", "!", "--src-type", "LOCAL", "-j", "RETURN"},
		{"-p", "tcp", "--dport", strconv.Itoa(egressProxyPort), "-j", "ACCEPT"},
		{"-p", "tcp", "--dport", strconv.Itoa(egressDNSPort), "-j", "ACCEPT"},
		{"-p", "udp", "--dport", strconv.Itoa(egressDNSPort), "-j", "ACCEPT"},
	}
	for _, t := range []struct {
		table, chain string
		rules        [][]string
		hooks        []string
	}{
		{"nat", egressChain, nat, []string{"OUTPUT", "PREROUTING"}},
		{"filter", egressChain, filter, []string{"OUTPUT", "FORWARD"}},
		{"filter", egressChain + "_IN", input, []string{"INPUT"}},
	} {
		if ipt("-t", t.table, "-N", t.chain) != nil {
			if err := ipt("-t", t.table, "-F", t.chain); err != nil {
				return err
			}
		}
		for _, r := range t.rules {
			if err := ipt(append([]string{"-t", t.table, "-A", t.chain}, r...)...); err != nil {
				return err
			}
		}
		for _, h := range t.hooks {
			for ipt("-t", t.table, "-D", h, "-j", t.chain) == nil {
			}
			if err := ipt("-t", t.table, "-I", h, "1", "-j", t.chain); err != nil {
				return err
			}
		}
	}
	return nil
}

// serveEgressProxy forwards each redirected connection over the channel.
func (a *Agent) serveEgressProxy(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go a.proxyEgress(c.(*net.TCPConn))
	}
}

func (a *Agent) proxyEgress(c *net.TCPConn) {
	// Only this machine and its containers: the node reaches this port
	// through the NIC and must not get a proxy into the server's network.
	if ra, ok := netip.AddrFromSlice(c.RemoteAddr().(*net.TCPAddr).IP); !ok || egressFromNIC(ra.Unmap()) {
		c.Close()
		return
	}
	dst, err := originalDst(c)
	if err != nil || dst.Port() == egressProxyPort && dst.Addr().IsLoopback() {
		c.Close()
		return
	}
	up, err := a.channel.dialEgress(dst.String())
	if err != nil {
		_ = c.SetLinger(0) // reset: the program sees "connection refused/reset" at once
		c.Close()
		return
	}
	_ = c.SetNoDelay(true)
	mux.Join(c, up)
}

// egressFromNIC: a connection from the node's side of the NIC.
func egressFromNIC(a netip.Addr) bool {
	return netip.MustParsePrefix("10.0.2.0/24").Contains(a) && a != netip.MustParseAddr("10.0.2.15")
}

// dialEgress opens addr through the server.
func (c *Channel) dialEgress(addr string) (net.Conn, error) {
	c.mu.Lock()
	sess := c.sess
	c.mu.Unlock()
	if sess == nil || sess.IsClosed() {
		return nil, fmt.Errorf("guest channel down")
	}
	st, err := sess.Open()
	if err != nil {
		return nil, err
	}
	_ = st.SetDeadline(time.Now().Add(20 * time.Second))
	conn, err := mux.DialUpgrade(st, "server", "/v1/egress?addr="+addr, nil)
	if err != nil {
		return nil, err
	}
	_ = st.SetDeadline(time.Time{})
	return conn, nil
}

// resolve forwards one DNS message through the server.
func (c *Channel) resolve(q []byte) ([]byte, error) {
	c.mu.Lock()
	sess := c.sess
	c.mu.Unlock()
	if sess == nil || sess.IsClosed() {
		return nil, fmt.Errorf("guest channel down")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://server/v1/egress/dns", bytes.NewReader(q))
	resp, err := (&http.Client{Transport: mux.Transport(sess)}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("dns: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<10))
}

func (a *Agent) serveDNSUDP(pc net.PacketConn) {
	uc := pc.(*net.UDPConn)
	enablePktinfo(uc)
	buf, oob := make([]byte, 64<<10), make([]byte, 512)
	for {
		n, oobn, _, from, err := uc.ReadMsgUDP(buf, oob)
		if err != nil {
			return
		}
		if ip, ok := netip.AddrFromSlice(from.IP); ok && egressFromNIC(ip.Unmap()) {
			continue
		}
		q, ctl := append([]byte(nil), buf[:n]...), replyOOB(oob[:oobn])
		go func() {
			if r, err := a.channel.resolve(q); err == nil {
				_, _, _ = uc.WriteMsgUDP(r, ctl, from)
			}
		}()
	}
}

func (a *Agent) serveDNSTCP(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			if ip, ok := netip.AddrFromSlice(c.RemoteAddr().(*net.TCPAddr).IP); !ok || egressFromNIC(ip.Unmap()) {
				return
			}
			br := bufio.NewReader(c)
			for {
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				var n uint16
				if binary.Read(br, binary.BigEndian, &n) != nil {
					return
				}
				q := make([]byte, n)
				if _, err := io.ReadFull(br, q); err != nil {
					return
				}
				r, err := a.channel.resolve(q)
				if err != nil {
					return
				}
				out := binary.BigEndian.AppendUint16(nil, uint16(len(r)))
				if _, err := c.Write(append(out, r...)); err != nil {
					return
				}
			}
		}()
	}
}
