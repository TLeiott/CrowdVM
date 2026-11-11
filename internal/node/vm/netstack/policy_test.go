package netstack

import (
	"net/netip"
	"testing"
)

func TestPolicyDefaults(t *testing.T) {
	r, err := NewRules(Policy{Pinholes: []string{"192.168.1.10:8420", "127.0.0.1:9000"}})
	if err != nil {
		t.Fatal(err)
	}
	r.LocalAddrs = func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("203.0.113.7")} }
	for _, c := range []struct {
		dst string
		ok  bool
	}{
		{"1.1.1.1:443", true},
		{"93.184.215.14:80", true},
		{"[2606:4700::1111]:443", true},
		{"127.0.0.1:8421", false},
		{"127.1.2.3:22", false},
		{"0.0.0.0:80", false},
		{"[::1]:80", false},
		{"[::ffff:127.0.0.1]:80", false},
		{"203.0.113.7:22", false}, // this machine's public address
		{"10.0.0.1:80", false},
		{"172.16.5.4:80", false},
		{"172.32.0.1:80", true}, // outside 172.16/12
		{"192.168.178.1:80", false},
		{"[::ffff:192.168.178.1]:80", false},
		{"100.78.214.33:22", false}, // Tailscale
		{"100.128.0.1:80", true},    // outside 100.64/10
		{"169.254.169.254:80", false},
		{"224.0.0.251:5353", false},
		{"255.255.255.255:67", false},
		{"[fd7a:115c:a1e0::1]:22", false}, // Tailscale ULA
		{"[fe80::1]:22", false},
		{"[ff02::1]:22", false},
		{"192.168.1.10:8420", true}, // server pinhole
		{"192.168.1.10:22", false},  // only the server port
		{"127.0.0.1:9000", true},
	} {
		ok, why := r.Check(netip.MustParseAddrPort(c.dst), nil)
		if ok != c.ok {
			t.Errorf("%s: got %v (%s), want %v", c.dst, ok, why, c.ok)
		}
	}
}

func TestPolicyOverrides(t *testing.T) {
	allow, _ := ParsePrefixes([]string{"192.168.178.0/24", "10.9.9.9", "203.0.113.7/32", "8.8.0.0/16"})
	deny, _ := ParsePrefixes([]string{"192.168.178.1", "8.0.0.0/8", "9.9.9.9"})
	r, err := NewRules(Policy{AllowLAN: false, Allow: allow, Deny: deny})
	if err != nil {
		t.Fatal(err)
	}
	local := []netip.Addr{netip.MustParseAddr("192.168.178.31"), netip.MustParseAddr("203.0.113.7")}
	r.LocalAddrs = func() []netip.Addr { return local }
	for _, c := range []struct {
		dst string
		ok  bool
	}{
		{"192.168.178.20:445", true},  // allowed LAN prefix
		{"192.168.178.1:80", false},   // more specific deny
		{"192.168.178.31:22", false},  // own address: a prefix does not open it
		{"203.0.113.7:22", true},      // own address opened by an exact /32
		{"10.9.9.9:80", true},         // bare address = /32
		{"10.9.9.10:80", false},       // rest of RFC 1918 still blocked
		{"8.8.8.8:53", true},          // allow /16 beats deny /8
		{"8.1.1.1:53", false},         // deny /8
		{"9.9.9.9:53", false},         // denied internet address
		{"127.0.0.1:80", false},       // loopback stays blocked
		{"169.254.169.254:80", false}, // metadata stays blocked
	} {
		ok, why := r.Check(netip.MustParseAddrPort(c.dst), nil)
		if ok != c.ok {
			t.Errorf("%s: got %v (%s), want %v", c.dst, ok, why, c.ok)
		}
	}

	lan, _ := NewRules(Policy{AllowLAN: true})
	lan.LocalAddrs = func() []netip.Addr { return local }
	for _, c := range []struct {
		dst string
		ok  bool
	}{
		{"192.168.178.20:445", true},
		{"10.1.2.3:80", true},
		{"100.78.214.33:22", true},
		{"[fd00::1]:80", true},
		{"192.168.178.31:22", false}, // still not this machine
		{"127.0.0.1:80", false},
		{"169.254.169.254:80", false},
		{"224.0.0.1:80", false},
	} {
		ok, why := lan.Check(netip.MustParseAddrPort(c.dst), nil)
		if ok != c.ok {
			t.Errorf("allow-lan %s: got %v (%s), want %v", c.dst, ok, why, c.ok)
		}
	}
}

func TestPolicySandbox(t *testing.T) {
	r, _ := NewRules(Policy{Pinholes: []string{"192.168.1.10:8420"}})
	r.LocalAddrs = func() []netip.Addr { return nil }
	only, _ := ParsePrefixes([]string{"140.82.112.0/20", "10.0.0.0/8"})
	deny, _ := ParsePrefixes([]string{"140.82.113.4"})
	sb := &Sandbox{Allow: only, Deny: deny}
	for _, c := range []struct {
		dst string
		ok  bool
	}{
		{"140.82.112.3:443", true},
		{"140.82.113.4:443", false}, // sandbox deny
		{"1.1.1.1:443", false},      // not in the sandbox allowlist
		{"10.0.0.1:80", false},      // sandbox cannot open what the node blocks
		{"192.168.1.10:8420", true}, // the server stays reachable
	} {
		ok, why := r.Check(netip.MustParseAddrPort(c.dst), sb)
		if ok != c.ok {
			t.Errorf("%s: got %v (%s), want %v", c.dst, ok, why, c.ok)
		}
	}
}

func TestParsePrefixes(t *testing.T) {
	p, err := ParsePrefixes([]string{" 10.0.0.1/8 ", "1.2.3.4", "::1", ""})
	if err != nil || len(p) != 3 || p[0].String() != "10.0.0.0/8" || p[1].String() != "1.2.3.4/32" || p[2].String() != "::1/128" {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := ParsePrefixes([]string{"10.0.0.0/33"}); err == nil {
		t.Fatal("want error")
	}
	if _, err := NewRules(Policy{Pinholes: []string{"nohost"}}); err == nil {
		t.Fatal("want pinhole error")
	}
}
