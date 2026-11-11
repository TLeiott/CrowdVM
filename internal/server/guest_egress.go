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
	"encoding/binary"
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

// maxEgressFlows caps one sandbox's open egress connections.
const maxEgressFlows = 1024

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// egressBlocked says why a sandbox may not connect to ap ("" = allowed).
func (s *Server) egressBlocked(ap netip.AddrPort) string {
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
	ips, _ := net.LookupIP(u.Hostname())
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip); ok && a.Unmap() == ap.Addr().Unmap() {
			return true
		}
	}
	return false
}

// guestStreams serves what a guest opens on its channel.
func (s *Server) guestStreams(g *GuestConn, egress bool) http.Handler {
	var flows atomic.Int64
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
		mux.Join(&readerConn{Conn: conn, r: rw.Reader}, out)
	})
	m.HandleFunc("POST /v1/egress/dns", func(w http.ResponseWriter, r *http.Request) {
		if !egress {
			http.Error(w, "egress through the server is off for this sandbox", 403)
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
