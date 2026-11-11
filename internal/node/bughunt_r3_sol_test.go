package node

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Repeated create requests race the monitor updating the existing VM.
func TestBughuntR3SolDuplicateCreateStatusRace(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n.diskAt = time.Now()
	n.disk = DiskState{HeadroomBytes: 4 << 30, BudgetGiB: 100, Overcommit: 4}
	r := &vmRec{id: "bx_existing", spec: proto.VMSpec{SandboxID: "bx_existing"}, status: proto.VMStatus{SandboxID: "bx_existing", State: proto.VMRunning}}
	n.vms[r.id] = r
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			n.setState(r, func(s *proto.VMStatus) { s.GuestReady = !s.GuestReady })
		}
	}()
	for i := 0; i < 2000; i++ {
		if _, err := n.CreateVM(r.spec); err == nil {
			t.Error("duplicate create succeeded")
		}
	}
	wg.Wait()
}
