package server

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestAssess(t *testing.T) {
	c := proto.Capacity{VCPU: 8, MemoryMiB: 16384, DiskGiB: 500, MaxSandboxes: 4}
	sz := api.SizeSpec{Name: "t", VCPU: 2, MemoryMiB: 4096, DiskGiB: 50}
	idle := &liveLoad{CPU: 0.05, MemTotalMiB: 32768, MemAvailMiB: 30000, CPUPressure: -1, MemPressure: -1}

	if _, _, why := assess(c, proto.Usage{VCPU: 7}, sz, nil); why != "full" {
		t.Fatalf("over vCPU: %q", why)
	}
	if _, _, why := assess(c, proto.Usage{VMs: 4}, sz, nil); why != "full" {
		t.Fatalf("over max sandboxes: %q", why)
	}
	empty, _, _ := assess(c, proto.Usage{}, sz, nil)
	booked, _, _ := assess(c, proto.Usage{VCPU: 4, MemoryMiB: 8192}, sz, nil)
	if !(empty > booked) {
		t.Fatalf("an emptier node scores higher: %v vs %v", empty, booked)
	}
	// An idle host scores like its reservations; a busy one lower, and hot.
	if s, hot, _ := assess(c, proto.Usage{}, sz, idle); s != empty || hot {
		t.Fatalf("idle host: %v %v (want %v)", s, hot, empty)
	}
	busy := *idle
	busy.CPU = 0.95
	if s, hot, why := assess(c, proto.Usage{}, sz, &busy); why != "" || !hot || s >= empty {
		t.Fatalf("busy host: %v %v %q", s, hot, why)
	}
	stall := *idle
	stall.MemPressure = 30
	if _, hot, _ := assess(c, proto.Usage{}, sz, &stall); !hot {
		t.Fatal("memory stalls make a host hot")
	}
	// The owner's programs use the memory: no new sandbox there.
	short := *idle
	short.MemAvailMiB = 1200
	if _, _, why := assess(c, proto.Usage{}, sz, &short); why != "host low on memory" {
		t.Fatalf("low memory: %q", why)
	}
	// Live memory use counts even with no bookings.
	used := *idle
	used.MemAvailMiB = 9000
	if s, _, _ := assess(c, proto.Usage{}, sz, &used); s >= empty {
		t.Fatalf("host memory in use: %v vs %v", s, empty)
	}
}

func TestNodeLoadSmoothing(t *testing.T) {
	var l nodeLoad
	now := time.Now()
	l.observe(nil, now) // older node: nothing
	if !l.at.IsZero() {
		t.Fatal("nil load recorded")
	}
	l.observe(&proto.HostLoad{CPUBusy: 0.1, MemTotalMiB: 1}, now)
	if l.cpu != 0.1 {
		t.Fatalf("first reading: %v", l.cpu)
	}
	l.observe(&proto.HostLoad{CPUBusy: 1, MemTotalMiB: 1}, now.Add(10*time.Second))
	rose := l.cpu
	l.observe(&proto.HostLoad{CPUBusy: 0.1, MemTotalMiB: 1}, now.Add(20*time.Second))
	if rose < 0.6 || l.cpu < rose-0.3 {
		t.Fatalf("rises fast, falls slowly: rose to %v, fell to %v", rose, l.cpu)
	}
	l.observe(&proto.HostLoad{CPUBusy: 0.2, MemTotalMiB: 1}, now.Add(time.Hour))
	if l.cpu != 0.2 {
		t.Fatalf("a reading after a gap starts over: %v", l.cpu)
	}
	c := &NodeConn{load: l}
	if c.liveLoad(now.Add(time.Hour+time.Second)) == nil || c.liveLoad(now.Add(2*time.Hour)) != nil {
		t.Fatal("fresh load used, stale load ignored")
	}
	if (*NodeConn)(nil).liveLoad(now) != nil {
		t.Fatal("offline node has no load")
	}
}
