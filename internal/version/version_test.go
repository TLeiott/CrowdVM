package version

import "testing"

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v1.2.3", "v1.2.3", 0, true},
		{"v1.2.4", "v1.2.3", 1, true},
		{"1.10.0", "v1.9.9", 1, true},
		{"v1.2.3", "v1.3", -1, true},
		{"v1.2.3-4-gabc1234", "v1.2.3", 1, true},
		{"v1.2.3-4-gabc1234-dirty", "v1.2.3-5-gdef5678", -1, true},
		{"v1.2.3-rc.1", "v1.2.3", -1, true},
		{"v1.2.3", "v1.2.3-rc.1", 1, true},
		{"v1.2.3-rc.10", "v1.2.3-rc.9", 1, true},
		{"0.2.0-nightly.20261113.5", "0.2.0-nightly.20261112.9", 1, true},
		{"0.2.0-nightly.20261112.9", "0.2.0-nightly.20261112.9", 0, true},
		{"0.2.0-nightly.20261112.9", "0.2.0", -1, true},
		{"0.2.0-nightly.20261112.9", "0.1.0", 1, true},
		{"0.2.0-nightly.20261112.9-3-gabc1234", "0.2.0-nightly.20261112.9", 1, true},
		{"dev", "v1.0.0", 0, false},
		{"abc1234", "v1.0.0", 0, false},
		{"v1.0.0", "", 0, false},
	} {
		got, ok := Compare(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d, %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}
