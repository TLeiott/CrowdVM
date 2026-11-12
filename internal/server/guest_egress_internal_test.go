package server

import (
	"net/netip"
	"strings"
	"testing"
)

// Egress through the server reaches the internet and the server's own API,
// never the server's LAN, loopback, tailnet or metadata services.
func TestEgressPolicy(t *testing.T) {
	s := &Server{Cfg: Config{PublicURL: "http://192.168.1.10:8420", EgressAllow: []string{"192.168.1.50:9418", "10.0.5.0/24", "169.254.0.0/16"}}}
	for addr, ok := range map[string]bool{
		"93.184.216.34:443":          true,
		"192.168.1.10:8420":          true, // the server's API
		"192.168.1.10:22":            false,
		"192.168.1.1:80":             false,
		"10.0.0.5:443":               false,
		"172.16.3.4:443":             false,
		"100.101.102.103:443":        false,
		"127.0.0.1:8420":             false,
		"169.254.169.254:80":         false,
		"0.0.0.0:80":                 false,
		"224.0.0.1:80":               false,
		"[::1]:443":                  false,
		"[fd00::1]:443":              false,
		"[2606:4700::1111]:443":      true,
		"192.168.1.50:9418":          true, // --egress-allow ip:port
		"192.168.1.50:22":            false,
		"10.0.5.7:443":               true, // --egress-allow CIDR
		"169.254.169.254:443":        false,
		"[64:ff9b::c0a8:0101]:22":    false, // NAT64 of 192.168.1.1
		"[2002:c0a8:0101::1]:22":     false, // 6to4 of 192.168.1.1
		"[::c0a8:0101]:22":           false, // IPv4-compatible
		"[2001:0:4136:e378::1]:80":   false, // Teredo
		"[64:ff9b::5db8:d822]:443":   true,  // NAT64 of a public address
		"[2606:4700::1111%eth0]:443": false, // zones are never accepted
	} {
		if got := s.egressBlocked(netip.MustParseAddrPort(addr)) == ""; got != ok {
			t.Errorf("%s: allowed=%v, want %v (%s)", addr, got, ok, s.egressBlocked(netip.MustParseAddrPort(addr)))
		}
	}
}

// A global IPv6 address on the server's own LAN is as private as 192.168.x:
// SLAAC gives every LAN host one.
func TestEgressBlocksServerNetworks(t *testing.T) {
	s := &Server{Cfg: Config{PublicURL: "http://192.168.1.10:8420"}}
	nets := localNets()
	if len(nets) == 0 {
		t.Skip("no interfaces")
	}
	for _, p := range nets {
		a := p.Addr()
		if a.IsLoopback() || a.IsLinkLocalUnicast() {
			continue
		}
		if s.egressBlocked(netip.AddrPortFrom(a, 22)) == "" {
			t.Errorf("%s (a server network) is reachable", a)
		}
		if a.Is6() && s.egressBlocked(netip.AddrPortFrom(a.WithZone("eth0"), 22)) == "" {
			t.Errorf("%s%%eth0 (a server network, zoned) is reachable", a)
		}
	}
}

func TestParseEgressAllow(t *testing.T) {
	got, err := ParseEgressAllow([]string{"10.0.5.3", "192.168.1.20:22", "10.0.5.0/24", "fd00::1"})
	if err != nil || strings.Join(got, ",") != "10.0.5.3/32,192.168.1.20:22,10.0.5.0/24,fd00::1/128" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseEgressAllow([]string{"gitserver:22"}); err == nil {
		t.Fatal("a host name was accepted")
	}
}
