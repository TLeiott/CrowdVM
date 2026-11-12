package server

// Load-aware placement. Reservations (the sizes of the sandboxes placed on a
// node) stay the hard limit: a sandbox never goes where its size does not
// fit what the owner shares. Within that, the scheduler goes by what the
// host really does, because a node is someone's machine and its owner's own
// programs use it too:
//
//   - load = max(reserved share, live share) per resource, so neither idle
//     bookings nor a busy owner hide the other;
//   - score = free share of the bottleneck resource (CPU or memory) after
//     the new sandbox, plus the mean free share as a tie-breaker;
//   - a host short of memory takes nothing new (hard skip);
//   - a hot host (CPU busy, or tasks stalling on CPU or memory) is used only
//     when no cool node fits;
//   - host CPU is smoothed: it rises fast and falls slowly, so a busy host
//     is avoided at once and trusted again only once it stays quiet.
//
// Everything is in memory from the heartbeats: placement costs the server a
// few comparisons per node, no extra queries.

import (
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
)

const (
	loadRise       = 0.6              // smoothing weight of a higher CPU reading
	loadFall       = 0.2              // ... of a lower one
	loadStale      = 35 * time.Second // older load is ignored (3 missed heartbeats)
	hotCPU         = 0.90             // smoothed host CPU busy share: hot
	hotCPUPressure = 25.0             // PSI cpu "some avg10" %: hot
	hotMemPressure = 10.0             // PSI memory "some avg10" %: hot
	memFloorMiB    = 512              // host memory kept free beyond a quarter of the new sandbox's

	heartbeatWriteEvery = time.Minute // nodes row refresh without changes
)

// nodeLoad is a node's live host load (guarded by NodeConn.mu).
type nodeLoad struct {
	cpu  float64 // smoothed CPUBusy
	last proto.HostLoad
	at   time.Time // zero: never reported
}

func (l *nodeLoad) observe(h *proto.HostLoad, now time.Time) {
	if h == nil {
		return
	}
	switch {
	case l.at.IsZero() || now.Sub(l.at) > loadStale:
		l.cpu = h.CPUBusy
	case h.CPUBusy > l.cpu:
		l.cpu += loadRise * (h.CPUBusy - l.cpu)
	default:
		l.cpu += loadFall * (h.CPUBusy - l.cpu)
	}
	l.last, l.at = *h, now
}

// liveLoad is a fresh load reading for one placement.
type liveLoad struct {
	CPU                      float64 // smoothed busy share 0..1
	MemTotalMiB, MemAvailMiB int
	CPUPressure, MemPressure float64 // PSI avg10 %, -1 unknown
}

func (l liveLoad) hot() bool {
	return l.CPU >= hotCPU || l.CPUPressure >= hotCPUPressure || l.MemPressure >= hotMemPressure
}

// liveLoad returns the node's load, nil when it reports none or it is stale.
func (c *NodeConn) liveLoad(now time.Time) *liveLoad {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.load.at.IsZero() || now.Sub(c.load.at) > loadStale {
		return nil
	}
	h := c.load.last
	ll := &liveLoad{CPU: c.load.cpu, MemTotalMiB: h.MemTotalMiB, MemAvailMiB: h.MemAvailMiB, CPUPressure: -1, MemPressure: -1}
	if h.CPUPressure != nil {
		ll.CPUPressure = *h.CPUPressure
	}
	if h.MemPressure != nil {
		ll.MemPressure = *h.MemPressure
	}
	return ll
}

// loadJSON is the node's live load for the API (nil when unknown).
func (c *NodeConn) loadJSON() map[string]any {
	ll := c.liveLoad(time.Now())
	if ll == nil {
		return nil
	}
	m := map[string]any{"cpuBusy": round2(ll.CPU), "memTotalMiB": ll.MemTotalMiB, "memAvailMiB": ll.MemAvailMiB, "hot": ll.hot(),
		"cpuPressure": nil, "memPressure": nil}
	if ll.CPUPressure >= 0 {
		m["cpuPressure"] = ll.CPUPressure
	}
	if ll.MemPressure >= 0 {
		m["memPressure"] = ll.MemPressure
	}
	return m
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// assess decides whether a node with shared capacity c, reservations used
// and live load ll (nil: unknown) can take a sandbox of size sz. It returns
// why not, or the node's load score (0..150, higher = more room) and
// whether the host is hot.
func assess(c proto.Capacity, used proto.Usage, sz api.SizeSpec, ll *liveLoad) (score float64, hot bool, why string) {
	if c.VCPU <= 0 || c.MemoryMiB <= 0 ||
		used.VCPU+sz.VCPU > c.VCPU || used.MemoryMiB+sz.MemoryMiB > c.MemoryMiB ||
		(c.DiskGiB > 0 && used.DiskGiB+sz.DiskGiB > c.DiskGiB) ||
		(c.MaxSandboxes > 0 && used.VMs+1 > c.MaxSandboxes) {
		return 0, false, "full"
	}
	cpu := float64(used.VCPU+sz.VCPU) / float64(c.VCPU)
	mem := float64(used.MemoryMiB+sz.MemoryMiB) / float64(c.MemoryMiB)
	if ll != nil {
		if ll.MemTotalMiB > 0 {
			if ll.MemAvailMiB < sz.MemoryMiB/4+memFloorMiB {
				return 0, false, "host low on memory"
			}
			mem = max(mem, 1-float64(ll.MemAvailMiB-sz.MemoryMiB)/float64(ll.MemTotalMiB))
		}
		cpu = max(cpu, ll.CPU)
		hot = ll.hot()
	}
	cpu, mem = min(max(cpu, 0), 1), min(max(mem, 0), 1)
	return 100*(1-max(cpu, mem)) + 50*(1-(cpu+mem)/2), hot, ""
}
