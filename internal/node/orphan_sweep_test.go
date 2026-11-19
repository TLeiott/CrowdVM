package node

import (
	"context"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/node/vm"
)

type sweepStore struct {
	vm.Store
	swept chan struct{}
}

func (s *sweepStore) SweepOrphans() {
	select {
	case s.swept <- struct{}{}:
	default:
	}
}

type sweepDriver struct {
	vm.Driver
	store vm.Store
}

func (d *sweepDriver) Store() vm.Store { return d.store }

func TestOrphanSweepRunsWithSnapshotsDisabled(t *testing.T) {
	old := orphanSweepInterval
	orphanSweepInterval = 5 * time.Millisecond
	defer func() { orphanSweepInterval = old }()
	st := &sweepStore{swept: make(chan struct{}, 10)}
	n := New(&Config{SnapshotInterval: "off"}, &sweepDriver{store: st}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.snapshotLoop(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	// More than one sweep verifies this is a recurring timer.
	for range 2 {
		select {
		case <-st.swept:
		case <-time.After(time.Second):
			t.Fatal("orphan sweep did not run while snapshots were disabled")
		}
	}
}
