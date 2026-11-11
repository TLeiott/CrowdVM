package bench

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// CPU reports the processor model/topology and two scores in "work units per
// second" (1 unit = 100,000 iterations of a mixed integer/float/L1-lookup loop):
// one goroutine, then one goroutine per logical CPU. Each phase runs for d.
func CPU(ctx context.Context, d time.Duration) proto.CPUBench {
	model, cores := cpuInfo(ctx)
	t := threads()
	if cores <= 0 {
		cores = t
	}
	return proto.CPUBench{
		Model:       model,
		Cores:       cores,
		Threads:     t,
		SingleScore: cpuRun(ctx, 1, d),
		MultiScore:  cpuRun(ctx, t, d),
	}
}

const unitIters = 100_000

var sink atomic.Uint64

func cpuRun(ctx context.Context, n int, d time.Duration) float64 {
	var units atomic.Int64
	deadline := time.Now().Add(d)
	start := time.Now()
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			var tbl [2048]uint64 // 16 KiB: stays in L1
			for j := range tbl {
				tbl[j] = uint64(j) * 0x9E3779B97F4A7C15
			}
			x := seed | 1
			f := 1.0
			for time.Now().Before(deadline) && ctx.Err() == nil {
				for range 4 { // 4 units (~1 ms) between clock checks
					for range unitIters {
						x ^= x << 13
						x ^= x >> 7
						x ^= x << 17
						x += tbl[x&2047]
						f = f*0.9999999 + float64(x&1023)*1e-9
					}
				}
				units.Add(4)
			}
			sink.Add(x + uint64(f))
		}(uint64(i)*7919 + 88172645463325252)
	}
	wg.Wait()
	el := time.Since(start).Seconds()
	if el <= 0 {
		return 0
	}
	return float64(units.Load()) / el
}
