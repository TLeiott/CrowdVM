package node

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestUploadStopsRetryingWhenServerFull(t *testing.T) {
	h := start(t, nil)
	h.srv.putImage("img1", []byte("base"))
	call(t, h.sess, "POST", "/v1/vms", spec("bx_full"))
	h.waitState(t, "bx_full", proto.VMRunning)
	h.srv.mu.Lock()
	h.srv.fullPut = true
	h.srv.mu.Unlock()
	if resp, body := call(t, h.sess, "POST", "/v1/vms/bx_full/snapshot", proto.SnapshotRequest{SnapshotID: "snap_full"}); resp.StatusCode != 200 {
		t.Fatalf("snapshot: %d %s", resp.StatusCode, body)
	}
	waitFor(t, "snapshot.failed without retry backoff", func() bool { return len(h.srv.eventsOf(proto.EvSnapshotFailed)) > 0 })
	var failure struct{ Error string }
	if err := json.Unmarshal(h.srv.eventsOf(proto.EvSnapshotFailed)[0].Data, &failure); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failure.Error, "server blob store is full") {
		t.Fatalf("failure lost server error: %s", failure.Error)
	}
	h.srv.mu.Lock()
	puts := h.srv.puts
	h.srv.mu.Unlock()
	if puts != 1 {
		t.Fatalf("full server upload retried: %d PUTs", puts)
	}
	if !h.drv.Store().HasSnapshot("snap_full") || len(h.node.LocalSnapshots(true)) != 0 {
		t.Fatal("failed upload should retain the local snapshot, no longer in flight")
	}
}
