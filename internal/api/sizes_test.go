package api

import "testing"

func TestSizeByName(t *testing.T) {
	for _, c := range []struct {
		in, name   string
		vcpu, gib  int
		disk       int
		multiplier float64
	}{
		{"", "default", 4, 8, 51, 1},
		{"small", "small", 2, 4, 51, 0.5},
		{"large", "large", 8, 16, 111, 2},
		{"xlarge", "xlarge", 16, 32, 251, 4},
		{"8c10g", "8c10g", 8, 10, 111, 1.625},
		{" 8C10G ", "8c10g", 8, 10, 111, 1.625},
		{"4c8g", "default", 4, 8, 51, 1}, // a catalog shape resolves to its name
		{"1c2g", "1c2g", 1, 2, 51, 0.25},
		{"20c8g", "20c8g", 20, 8, 251, 3}, // more vCPU than any named size
		{"64c256g", "64c256g", 64, 256, 251, 24},
	} {
		s, ok := SizeByName(c.in)
		if !ok || s.Name != c.name || s.VCPU != c.vcpu || s.MemoryMiB != c.gib*1024 || s.DiskGiB != c.disk || s.Multiplier != c.multiplier {
			t.Errorf("SizeByName(%q) = %+v, %v", c.in, s, ok)
		}
	}
	for _, bad := range []string{"huge", "0c4g", "8c1g", "65c8g", "8c257g", "8c", "c8g", "8g10c", "8c10gb", "-1c4g"} {
		if s, ok := SizeByName(bad); ok {
			t.Errorf("SizeByName(%q) = %+v, want rejected", bad, s)
		}
	}
}
