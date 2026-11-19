package guest

import (
	"reflect"
	"testing"
)

func TestUDPAllowRules(t *testing.T) {
	got := udpAllowRules([]string{"192.0.2.7:27015", "198.51.100.0/24", "203.0.113.9", "[2001:db8::1]:53", "junk"})
	want := [][]string{
		{"-d", "192.0.2.7", "-p", "udp", "--dport", "27015", "-j", "RETURN"},
		{"-d", "198.51.100.0/24", "-p", "udp", "-j", "RETURN"},
		{"-d", "203.0.113.9", "-p", "udp", "-j", "RETURN"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}
