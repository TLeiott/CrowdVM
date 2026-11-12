package bench

import (
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// loadMaxGap: a CPU interval longer than this (the node was offline or
// asleep) says nothing about the host now, so it is not reported.
const loadMaxGap = 30 * time.Second

// LoadSampler measures the host's live load for the heartbeat. CPU busy is
// the share of busy CPU time since the previous Sample, so a call costs a
// few small reads and no waiting.
type LoadSampler struct {
	mu        sync.Mutex
	busy, tot uint64 // CPU times at the previous sample
	at        time.Time
}

// Sample returns the current host load, or nil where it cannot be measured
// (and on the first call, or after a long gap, which have no recent
// interval to measure CPU over).
func (s *LoadSampler) Sample() *proto.HostLoad {
	total, avail := memInfo()
	busy, tot, ok := cpuTimes()
	if total == 0 || !ok {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	fresh := !s.at.IsZero() && now.Sub(s.at) <= loadMaxGap && tot > s.tot && busy >= s.busy
	var share float64
	if fresh {
		share = min(float64(busy-s.busy)/float64(tot-s.tot), 1)
	}
	s.busy, s.tot, s.at = busy, tot, now
	s.mu.Unlock()
	if !fresh {
		return nil
	}
	l := &proto.HostLoad{CPUBusy: share, MemTotalMiB: int(total >> 20), MemAvailMiB: int(avail >> 20)}
	l.CPUPressure, l.MemPressure = pressure()
	return l
}
