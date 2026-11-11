package bench

import (
	"context"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Memory reports total/available RAM and the aggregate copy bandwidth
// (bytes copied per second, up to 8 parallel copiers on 32 MiB buffers).
func Memory(ctx context.Context, d time.Duration) proto.MemoryBench {
	total, avail := memInfo()
	return proto.MemoryBench{
		TotalMiB:      int(total >> 20),
		AvailableMiB:  int(avail >> 20),
		BandwidthMBps: memBandwidth(ctx, d),
	}
}

func memBandwidth(ctx context.Context, d time.Duration) float64 {
	const size = 32 << 20
	n := min(threads(), 8)
	bufs := make([][2][]byte, n)
	for i := range bufs {
		bufs[i] = [2][]byte{make([]byte, size), make([]byte, size)}
		for j := 0; j < size; j += 4096 { // fault pages in before timing
			bufs[i][0][j], bufs[i][1][j] = 1, 1
		}
	}
	deadline := time.Now().Add(d)
	start := time.Now()
	var mu sync.Mutex
	var copied int64
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(b [2][]byte) {
			defer wg.Done()
			var c int64
			for time.Now().Before(deadline) && ctx.Err() == nil {
				copy(b[1], b[0])
				c += size
			}
			mu.Lock()
			copied += c
			mu.Unlock()
		}(bufs[i])
	}
	wg.Wait()
	el := time.Since(start).Seconds()
	if el <= 0 {
		return 0
	}
	return float64(copied) / el / 1e6
}
