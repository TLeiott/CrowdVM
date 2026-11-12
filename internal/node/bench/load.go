package bench

import (
	"sync"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// LoadSampler measures the host's live load for the heartbeat. CPU busy is
// the share of non-idle CPU time since the previous Sample, so a call costs
// a few small reads and no waiting.
type LoadSampler struct {
	mu        sync.Mutex
	idle, tot uint64 // CPU times at the previous sample
	busy      float64
	have      bool // busy holds a measurement
}

// Sample returns the current host load, or nil where it cannot be measured
// (and on the first call, which has no interval to measure CPU over).
func (s *LoadSampler) Sample() *proto.HostLoad {
	total, avail := memInfo()
	idle, tot, ok := cpuTimes()
	if total == 0 || !ok {
		return nil
	}
	s.mu.Lock()
	if s.tot != 0 && tot > s.tot && idle >= s.idle {
		s.busy = min(max(1-float64(idle-s.idle)/float64(tot-s.tot), 0), 1)
		s.have = true
	}
	s.idle, s.tot = idle, tot
	busy, have := s.busy, s.have
	s.mu.Unlock()
	if !have {
		return nil
	}
	l := &proto.HostLoad{CPUBusy: busy, MemTotalMiB: int(total >> 20), MemAvailMiB: int(avail >> 20)}
	l.CPUPressure, l.MemPressure = pressure()
	return l
}
