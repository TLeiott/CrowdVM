package netstack

import (
	"net/netip"
	"testing"
)

// bugs-r11-opus 11: Azure's WireServer is a public address; the node must still refuse it.
func TestR11WireServerBlocked(t *testing.T) {
	r, err := NewRules(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	r.LocalAddrs = func() []netip.Addr { return nil }
	for _, dst := range []string{"168.63.129.16:80", "168.63.129.16:32526"} {
		if ok, why := r.Check(netip.MustParseAddrPort(dst), nil); ok {
			t.Errorf("%s allowed (%s)", dst, why)
		}
	}
	if ok, why := r.Check(netip.MustParseAddrPort("168.63.129.17:80"), nil); !ok {
		t.Errorf("neighbour address refused: %s", why)
	}
}
