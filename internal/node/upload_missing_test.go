package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestUploadMissingSnapshotStopsQuietly(t *testing.T) {
	for _, parentMissing := range []bool{false, true} {
		name := "snapshot"
		if parentMissing {
			name = "parent"
		}
		t.Run(name, func(t *testing.T) {
			h := start(t, nil)
			ref := proto.SnapshotRef{ID: "snap_missing", SandboxID: "bx_missing"}
			if parentMissing {
				ref.ParentID = "snap_parent"
				// Seed the child directly: its parent has already been removed.
				h.drv.store.mu.Lock()
				h.drv.store.snaps[ref.ID] = ref
				h.drv.store.mu.Unlock()
			}
			assertQuietUpload(t, h, ref)
		})
	}
}

func TestUploadDeletedBeforeRetryStopsQuietly(t *testing.T) {
	h := start(t, nil)
	hold := make(chan struct{})
	defer close(hold)
	h.srv.mu.Lock()
	h.srv.holdPut = hold
	h.srv.mu.Unlock()
	ref := proto.SnapshotRef{ID: "snap_retry_deleted", SandboxID: "bx_missing"}
	path := filepath.Join(t.TempDir(), "snapshot")
	if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.drv.store.ImportSnapshot(ref, path); err != nil {
		t.Fatal(err)
	}
	h.node.uploadAsync(ref.SandboxID, ref, false)
	waitFor(t, "upload started", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		return h.srv.puts > 0
	})
	call(t, h.sess, "DELETE", "/v1/snapshots/"+ref.ID, nil)
	waitQuietUpload(t, h.node, ref.ID)
	if evs := h.srv.eventsOf(proto.EvSnapshotFailed); len(evs) != 0 {
		t.Fatalf("deleted upload reported failure: %+v", evs)
	}
}

func assertQuietUpload(t *testing.T, h *harness, ref proto.SnapshotRef) {
	t.Helper()
	// A separate node keeps emitted events queued so their absence is observable.
	n := New(h.cfg, h.drv, h.log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.ctx = ctx
	h.node.mu.Lock()
	n.client = h.node.client
	h.node.mu.Unlock()
	n.uploadAsync(ref.SandboxID, ref, false)
	waitQuietUpload(t, n, ref.ID)
}

func waitQuietUpload(t *testing.T, n *Node, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		_, uploading := n.uploading[id]
		events := append([]proto.Event(nil), n.events...)
		n.mu.Unlock()
		for _, ev := range events {
			if ev.Type == proto.EvSnapshotFailed {
				t.Fatalf("deleted upload reported failure: %+v", ev)
			}
		}
		if !uploading {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("missing snapshot upload kept retrying")
}
