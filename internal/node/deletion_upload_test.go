package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestDeleteSnapshotCancelsUpload(t *testing.T) {
	h := start(t, nil)
	hold := make(chan struct{})
	defer close(hold)
	h.srv.mu.Lock()
	h.srv.holdPut = hold
	h.srv.mu.Unlock()
	ref := proto.SnapshotRef{ID: "snap_delete", SandboxID: "bx_delete"}
	path := filepath.Join(t.TempDir(), "snapshot")
	if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.drv.store.ImportSnapshot(ref, path); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.node.UploadSnapshot(h.node.ctx, ref.SandboxID, ref.ID) }()
	waitFor(t, "upload started", func() bool {
		h.srv.mu.Lock()
		defer h.srv.mu.Unlock()
		return h.srv.puts > 0
	})
	resp, body := call(t, h.sess, "DELETE", "/v1/snapshots/"+ref.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d %s", resp.StatusCode, body)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("upload: %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deleting the snapshot did not cancel its upload")
	}
}
