package api

import (
	"regexp"
	"testing"
)

// Sandbox ids match Boat's pattern (boat-v1.yaml).
func TestNewIDBoatPattern(t *testing.T) {
	re := regexp.MustCompile(`^bx_[23456789abcdefghjkmnpqrstuvwxyz]{8}$`)
	seen := map[byte]bool{}
	for i := 0; i < 2000; i++ {
		id := NewID(PrefixSandbox, 8)
		if !re.MatchString(id) {
			t.Fatalf("id %q fails Boat's pattern", id)
		}
		for j := 3; j < len(id); j++ {
			seen[id[j]] = true
		}
	}
	if len(seen) != len(idAlphabet) {
		t.Fatalf("only %d of %d characters used", len(seen), len(idAlphabet))
	}
}
