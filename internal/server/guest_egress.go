package server

// Secure layer, egress through the server (guest side: internal/guest/
// egress.go). A channel guest with egress "server" opens a stream on its
// channel for every outgoing TCP connection (GET /v1/egress?addr=ip:port,
// Upgrade) and every DNS query (POST /v1/egress/dns). The server connects out
// from its own network. Private, loopback, link-local, CGNAT and metadata
// addresses are refused: a sandbox reaches the internet and this server's
// API, not the server's LAN.

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crowdvm/crowdvm/internal/mux"
)

// maxEgressFlows caps one sandbox's open egress connections. With the
// guest channel's 1 MiB stream window, a sandbox holds at most this many
// MiB of the server's memory in flows whose destination stops reading
// (bugs-r15-opus 3).
const maxEgressFlows = 256

var (
	// cgnat: outbound_guard.go
	teredo     = netip.MustParsePrefix("2001::/32")
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
	v4compat   = netip.MustParsePrefix("::/96")
)

// embeddedIPv4: the IPv4 address an IPv6 address stands for.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	b := a.As16()
	switch {
	case nat64.Contains(a), v4compat.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixToFour.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

var localNetCache struct {
	sync.Mutex
	at   time.Time
	nets []netip.Prefix
}

// localNets: the networks on the server's interfaces (refreshed each minute).
func localNets() []netip.Prefix {
	c := &localNetCache
	c.Lock()
	defer c.Unlock()
	if time.Since(c.at) < time.Minute {
		return c.nets
	}
	c.at, c.nets = time.Now(), nil
	addrs, _ := net.InterfaceAddrs()
	for _, ad := range addrs {
		if n, ok := ad.(*net.IPNet); ok {
			if a, ok := netip.AddrFromSlice(n.IP); ok {
				a = a.Unmap()
				ones, _ := n.Mask.Size()
				if a.Is4() && !a.IsPrivate() && !a.IsLoopback() && !a.IsLinkLocalUnicast() {
					// A public IPv4 network (a VPS's /20) holds strangers:
					// only this server's own address.
					ones = 32
				}
				c.nets = append(c.nets, netip.PrefixFrom(a, ones).Masked())
			}
		}
	}
	return c.nets
}

// egressBlocked says why a sandbox may not connect to ap ("" = allowed).
func (s *Server) egressBlocked(ap netip.AddrPort) string {
	if ap.Addr().Zone() != "" {
		// Prefix.Contains is false for a zoned address while the kernel
		// ignores the zone of a global one (red team round 5).
		return "an address with a zone"
	}
	a := ap.Addr().Unmap()
	if s.isOwnAPI(ap) {
		return ""
	}
	for _, m := range metadataIPs {
		if net.IP(a.AsSlice()).Equal(m) {
			return "a cloud metadata address"
		}
	}
	for _, e := range s.Cfg.EgressAllow {
		if p, err := netip.ParseAddrPort(e); err == nil && p.Addr().Unmap() == a && p.Port() == ap.Port() {
			return ""
		}
		if p, err := netip.ParsePrefix(e); err == nil && p.Contains(a) {
			return ""
		}
	}
	if a.Is6() {
		if embedded, ok := embeddedIPv4(a); ok {
			// NAT64, 6to4 or IPv4-compatible: judge the IPv4 inside.
			if why := s.egressBlocked(netip.AddrPortFrom(embedded, ap.Port())); why != "" {
				return why + " (inside an IPv6 address)"
			}
		}
		if teredo.Contains(a) || nat64Local.Contains(a) {
			return "a tunnelled IPv4 address"
		}
	}
	for _, p := range localNets() {
		if p.Contains(a) {
			// The server's own networks: its LAN's global IPv6 /64 and its
			// own addresses are as private as 192.168.x.
			return "a network of the server (" + p.String() + ")"
		}
	}
	if ip := net.IP(a.AsSlice()); blockedTarget(ip) != "" {
		return blockedTarget(ip)
	}
	switch {
	case a.IsPrivate():
		return "a private (LAN) address"
	case cgnat.Contains(a):
		return "a CGNAT/Tailscale address"
	case a.Is4() && (a.As4()[0] == 0 || a.As4() == [4]byte{255, 255, 255, 255}):
		return "a reserved address"
	}
	return ""
}

// isOwnAPI: ap is this server's public address (the sandbox's CLI talks to
// the API through the channel too).
func (s *Server) isOwnAPI(ap netip.AddrPort) bool {
	u, err := url.Parse(s.Cfg.PublicURL)
	if err != nil {
		return false
	}
	port := u.Port()
	if port == "" {
		port = map[bool]string{true: "443", false: "80"}[u.Scheme == "https"]
	}
	if port != strconv.Itoa(int(ap.Port())) {
		return false
	}
	if a, err := netip.ParseAddr(u.Hostname()); err == nil {
		return a.Unmap() == ap.Addr().Unmap()
	}
	for _, a := range apiAddrs(u.Hostname()) {
		if a == ap.Addr().Unmap() {
			return true
		}
	}
	return false
}

var apiAddrCache struct {
	sync.Mutex
	host  string
	at    time.Time
	addrs []netip.Addr
}

// apiAddrs resolves the public URL's host, at most once a minute (every
// egress dial to its port asks).
func apiAddrs(host string) []netip.Addr {
	c := &apiAddrCache
	c.Lock()
	defer c.Unlock()
	if c.host == host && time.Since(c.at) < time.Minute {
		return c.addrs
	}
	c.host, c.at, c.addrs = host, time.Now(), nil
	ips, _ := net.LookupIP(host)
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip); ok {
			c.addrs = append(c.addrs, a.Unmap())
		}
	}
	return c.addrs
}

// ParseEgressAllow checks --egress-allow entries: ip:port, a CIDR, or a
// bare IP (taken as /32 or /128).
func ParseEgressAllow(list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	for _, e := range list {
		e = strings.TrimSpace(e)
		if _, err := netip.ParseAddrPort(e); err == nil {
			out = append(out, e)
		} else if _, err := netip.ParsePrefix(e); err == nil {
			out = append(out, e)
		} else if a, err := netip.ParseAddr(e); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()).String())
		} else {
			return nil, fmt.Errorf("--egress-allow %q: want ip:port, a CIDR or an IP", e)
		}
	}
	return out, nil
}

// guestStreams serves what a guest opens on its channel.
func (s *Server) guestStreams(g *GuestConn, egress bool) http.Handler {
	var flows atomic.Int64
	// DNS queries in flight per channel: a flood is answered 503 at once
	// instead of piling up goroutines and streams (bugs-r14-opus 3).
	dnsSlots := make(chan struct{}, maxEgressDNS)
	// Flows end with their channel session: one blocked on a destination
	// that never reads would otherwise outlive the sandbox, and each new
	// session could park 256 more (bugs-r16-opus 2).
	sessDone, endSess := context.WithCancel(context.Background())
	go func() { <-g.sess.CloseChan(); endSess() }()
	m := http.NewServeMux()
	m.HandleFunc("GET /v1/egress", func(w http.ResponseWriter, r *http.Request) {
		if !egress {
			http.Error(w, "egress through the server is off for this sandbox", 403)
			return
		}
		ap, err := netip.ParseAddrPort(r.URL.Query().Get("addr"))
		if err != nil {
			http.Error(w, "addr must be ip:port", 400)
			return
		}
		if why := s.egressBlocked(ap); why != "" {
			s.Log.Debug("egress refused", "sandbox", g.SandboxID, "addr", ap, "why", why)
			http.Error(w, "refused: "+why, 403)
			return
		}
		if flows.Add(1) > maxEgressFlows {
			flows.Add(-1)
			http.Error(w, "too many connections", 429)
			return
		}
		defer flows.Add(-1)
		out, err := net.DialTimeout("tcp", ap.String(), 10*time.Second)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		if tc, ok := out.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		conn, rw, err := mux.HijackUpgrade(w)
		if err != nil {
			out.Close()
			return
		}
		// A destination that takes nothing for egressStall ends the flow,
		// so stalled flows cannot hold all of a sandbox's slots
		// (bugs-r17-opus 1).
		out = &stallConn{Conn: out}
		defer context.AfterFunc(sessDone, func() {
			if sc, ok := out.(*stallConn); ok {
				if tc, ok := sc.Conn.(*net.TCPConn); ok {
					_ = tc.SetLinger(0) // reset: drop what the destination never read
				}
			}
			out.Close()
			conn.Close()
		})()
		mux.Join(&readerConn{Conn: conn, r: rw.Reader}, out)
	})
	m.HandleFunc("POST /v1/rebooting", func(w http.ResponseWriter, r *http.Request) {
		s.guests.expectReboot(g.SandboxID) // see handleGuestReboot
		w.WriteHeader(http.StatusNoContent)
	})
	m.HandleFunc("POST /v1/egress/dns", func(w http.ResponseWriter, r *http.Request) {
		if !egress {
			http.Error(w, "egress through the server is off for this sandbox", 403)
			return
		}
		select {
		case dnsSlots <- struct{}{}:
			defer func() { <-dnsSlots }()
		default:
			http.Error(w, "too many dns queries in flight", 503)
			return
		}
		q, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil || len(q) < 12 {
			http.Error(w, "bad query", 400)
			return
		}
		ans, err := dnsExchange(q)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(ans)
	})
	return m
}

// egressStall: how long an egress write may wait for its destination.
var egressStall = 2 * time.Minute

// stallConn fails a write the destination has not taken within egressStall
// (each write gets the full time again).
type stallConn struct {
	net.Conn
	stalled atomic.Bool
}

func (c *stallConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(egressStall))
	n, err := c.Conn.Write(p)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		c.stalled.Store(true)
	}
	return n, err
}

// Close resets a stalled flow: what the destination never took is dropped,
// not kept in the kernel until it gives up.
func (c *stallConn) Close() error {
	if tc, ok := c.Conn.(*net.TCPConn); ok && c.stalled.Load() {
		_ = tc.SetLinger(0)
	}
	return c.Conn.Close()
}

// CloseWrite keeps the half-close of the TCP conn (mux.Join).
func (c *stallConn) CloseWrite() error { return mux.CloseWrite(c.Conn) }

// maxGuestStreams caps the streams a guest channel has served at once: TCP
// flows (maxEgressFlows), DNS and its calls, with room to spare.
const maxGuestStreams = 2 * maxEgressFlows

// maxEgressDNS caps a channel's DNS queries in flight (bugs-r14-opus 3).
const maxEgressDNS = 64

var resolvers struct {
	once sync.Once
	list []string
}

// upstreamDNS: the server's nameservers (/etc/resolv.conf).
func upstreamDNS() []string {
	resolvers.once.Do(func() {
		f, err := os.Open("/etc/resolv.conf")
		if err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				fs := strings.Fields(sc.Text())
				if len(fs) >= 2 && fs[0] == "nameserver" {
					resolvers.list = append(resolvers.list, net.JoinHostPort(strings.SplitN(fs[1], "%", 2)[0], "53"))
				}
			}
			f.Close()
		}
		if len(resolvers.list) == 0 {
			resolvers.list = []string{"1.1.1.1:53", "8.8.8.8:53"}
		}
	})
	return resolvers.list
}

// dnsExchange sends a DNS message to the server's resolvers (UDP, then TCP
// when the answer is truncated).
func dnsExchange(q []byte) ([]byte, error) {
	var last error
	for _, ns := range upstreamDNS() {
		ans, err := dnsUDP(ns, q)
		if err == nil && len(ans) > 2 && ans[2]&0x02 != 0 { // TC
			ans, err = dnsTCP(ns, q)
		}
		if err == nil {
			return ans, nil
		}
		last = err
	}
	return nil, last
}

func dnsUDP(ns string, q []byte) ([]byte, error) {
	c, err := net.DialTimeout("udp", ns, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		if n >= 2 && buf[0] == q[0] && buf[1] == q[1] { // same id
			return buf[:n], nil
		}
	}
}

func dnsTCP(ns string, q []byte) ([]byte, error) {
	c, err := net.DialTimeout("tcp", ns, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...)); err != nil {
		return nil, err
	}
	var n uint16
	if err := binary.Read(c, binary.BigEndian, &n); err != nil {
		return nil, err
	}
	ans := make([]byte, n)
	_, err = io.ReadFull(c, ans)
	return ans, err
}
