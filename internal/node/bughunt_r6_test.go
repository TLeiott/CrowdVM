package node

import (
	"context"
	"github.com/crowdvm/crowdvm/internal/proto"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// once() drops the key as soon as the last waiter leaves, but the transfer's
// goroutine may still run (for example in ImportImage, which ignores its
// context). A caller arriving then starts a second run of the same key, so two
// imports/uploads of one blob overlap: the deduplication no longer holds.
func TestBughuntR6OnceOverlapsAbandonedRun(t *testing.T) {
	n := &Node{fetches: map[string]*fetchCall{}}
	var running, peak atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	fn := func(context.Context) error {
		cur := running.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		started <- struct{}{}
		<-release // not interruptible, like a file move/convert
		running.Add(-1)
		return nil
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); n.once(ctxA, n.fetches, "image/x", fn) }()
	<-started
	cancelA() // the only waiter leaves; the run is still going
	time.Sleep(50 * time.Millisecond)
	go func() { defer wg.Done(); n.once(context.Background(), n.fetches, "image/x", fn) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
	}
	close(release)
	wg.Wait()
	if peak.Load() > 1 {
		t.Fatalf("%d runs of the same blob transfer overlapped", peak.Load())
	}
}

// bugs-r6-opus 7: a drain set on the server shows in the node's status.
func TestServerDrainShownInStatus(t *testing.T) {
	h := start(t, nil)
	h.node.applyServerSettings(proto.HeartbeatResponse{Drained: true})
	if !h.node.Status().Drained {
		t.Fatal("drain not shown")
	}
	h.node.applyServerSettings(proto.HeartbeatResponse{})
	if h.node.Status().Drained {
		t.Fatal("undrain not shown")
	}
}
