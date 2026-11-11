package server

import (
	"net/netip"
	"testing"
)

// Egress through the server reaches the internet and the server's own API,
// never the server's LAN, loopback, tailnet or metadata services.
func TestEgressPolicy(t *testing.T) {
	s := &Server{Cfg: Config{PublicURL: "http://192.168.1.10:8420", EgressAllow: []string{"192.168.1.50:9418", "10.0.5.0/24", "169.254.0.0/16"}}}
	for addr, ok := range map[string]bool{
		"93.184.216.34:443":     true,
		"192.168.1.10:8420":     true, // the server's API
		"192.168.1.10:22":       false,
		"192.168.1.1:80":        false,
		"10.0.0.5:443":          false,
		"172.16.3.4:443":        false,
		"100.101.102.103:443":   false,
		"127.0.0.1:8420":        false,
		"169.254.169.254:80":    false,
		"0.0.0.0:80":            false,
		"224.0.0.1:80":          false,
		"[::1]:443":             false,
		"[fd00::1]:443":         false,
		"[2606:4700::1111]:443": true,
		"192.168.1.50:9418":     true, // --egress-allow ip:port
		"192.168.1.50:22":       false,
		"10.0.5.7:443":          true, // --egress-allow CIDR
		"169.254.169.254:443":   false,
	} {
		if got := s.egressBlocked(netip.MustParseAddrPort(addr)) == ""; got != ok {
			t.Errorf("%s: allowed=%v, want %v (%s)", addr, got, ok, s.egressBlocked(netip.MustParseAddrPort(addr)))
		}
	}
}
