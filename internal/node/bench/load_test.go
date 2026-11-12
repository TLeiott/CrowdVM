package bench

import (
	"runtime"
	"testing"
	"time"
)

func TestLoadSampler(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("no load on this OS")
	}
	var s LoadSampler
	if s.Sample() != nil {
		t.Fatal("the first sample has no interval")
	}
	time.Sleep(50 * time.Millisecond)
	l := s.Sample()
	if l == nil || l.CPUBusy < 0 || l.CPUBusy > 1 || l.MemTotalMiB <= 0 || l.MemAvailMiB > l.MemTotalMiB {
		t.Fatalf("sample: %+v", l)
	}
	s.at = s.at.Add(-time.Minute) // the node was away
	if s.Sample() != nil {
		t.Fatal("an interval after a long gap is not reported")
	}
}
