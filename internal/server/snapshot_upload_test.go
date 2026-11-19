package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

func TestFailedNamedSnapshot(t *testing.T) {
	for _, early := range []bool{false, true} {
		t.Run(map[bool]string{false: "after-recording", true: "before-recording"}[early], func(t *testing.T) {
			env := servertest.New(t)
			opts := servertest.NodeOptions{NoUpload: true}
			if early {
				opts.SnapshotUploadError = "server blob store is full"
			}
			n := env.StartNode(opts)
			key := env.AdminKey
			id := createSandbox(t, env, key, nil)
			env.WaitState(key, id, "ready")
			if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "broken"}); r.Status != 202 {
				t.Fatalf("save: %s", r)
			}
			if !early {
				env.Eventually("save's snapshot recorded", func() bool {
					_, err := env.S.SnapshotByID(pendingOf(env, "broken"))
					return err == nil
				})
				d, _ := json.Marshal(map[string]string{"snapshotId": pendingOf(env, "broken"), "error": "server blob store is full"})
				r8NodeEvent(t, env, n, proto.Event{SandboxID: id, Type: proto.EvSnapshotFailed, Data: d})
			}
			r := env.WaitFor(key, "/api/v1/saved-snapshots/broken", "snapshot.status", 5*time.Second, "failed")
			if r.Str("snapshot.available") != "false" || !strings.Contains(r.Str("snapshot.error"), "server blob store is full") {
				t.Fatalf("failed save: %s", r)
			}
			if r := env.Do(key, "GET", "/api/v1/saved-snapshots", nil); len(r.Get("snapshots").([]any)) != 0 || r.Str("allowance.used") != "0" {
				t.Fatalf("failed save listed or charged: %s", r)
			}
			if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"from": "broken"}); r.Status != 409 || r.Code() != "snapshot_failed" || !strings.Contains(r.Str("error.message"), "server blob store is full") {
				t.Fatalf("create from failed save: %s", r)
			}
		})
	}
}

func TestSavedSnapshotRequiresUploadedChain(t *testing.T) {
	env := servertest.New(t)
	n := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	parent := storedLayer(t, env, n, id)
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "template"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	r := env.WaitFor(key, "/api/v1/saved-snapshots/template", "snapshot.status", 5*time.Second, "ready")
	snap := r.Str("snapshot.snapshotId")
	for _, layer := range []string{snap, parent} {
		if _, err := env.S.DB.Exec(`UPDATE snapshots SET uploaded_at=NULL WHERE id=?`, layer); err != nil {
			t.Fatal(err)
		}
		if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"from": "template"}); r.Status != 409 || r.Code() != "snapshot_not_ready" || !strings.Contains(r.Str("error.message"), "has not finished uploading") {
			t.Fatalf("create with missing layer %s: %s", layer, r)
		}
		if _, err := env.S.DB.Exec(`UPDATE snapshots SET uploaded_at=created_at WHERE id=?`, layer); err != nil {
			t.Fatal(err)
		}
	}

	// A failed replacement keeps the last successfully uploaded disk usable.
	b := env.StartNode(servertest.NodeOptions{SnapshotUploadError: "server blob store is full"})
	replacement := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": b.ID}})
	env.WaitState(key, replacement, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": replacement, "name": "template"}); r.Status != 202 {
		t.Fatalf("re-save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/template", "snapshot.status", 5*time.Second, "failed")
	if r := env.Do(key, "GET", "/api/v1/saved-snapshots", nil); r.Str("snapshots.0.snapshotId") != snap || r.Str("snapshots.0.status") != "failed" {
		t.Fatalf("last good save lost: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"from": "template"}); r.Status != 202 {
		t.Fatalf("create from last good save: %s", r)
	}
}

type countedUpload struct{ reads atomic.Int32 }

func (r *countedUpload) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return len(p), nil
}

func TestSnapshotUploadChecksSpaceBeforeReadingBody(t *testing.T) {
	env := servertest.New(t)
	n := env.StartNode(servertest.NodeOptions{})
	client := &http.Client{Transport: &http.Transport{ExpectContinueTimeout: time.Second}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, contentLength := range []bool{true, false} {
		body := &countedUpload{}
		req, err := http.NewRequest("PUT", env.URL+"/node/v1/blobs/snapshot/snap_full", io.NopCloser(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+n.Token)
		req.Header.Set("Expect", "100-continue")
		if contentLength {
			req.ContentLength = 1 << 62
		} else {
			ref, _ := json.Marshal(proto.SnapshotRef{ID: "snap_full", SizeBytes: 1 << 62})
			req.Header.Set("X-Crowdvm-Snapshot", string(ref))
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusInsufficientStorage || !strings.Contains(string(b), "server blob store is full:") || !strings.Contains(string(b), "GiB free, upload needs") || !strings.Contains(string(b), "1 GiB reserve") {
			t.Fatalf("space refusal: %d %s", resp.StatusCode, b)
		}
		if body.reads.Load() != 0 {
			t.Fatal("upload body read before checking space")
		}
		if _, err := env.S.Blobs.Stat(store.BlobSnapshot, "snap_full"); err == nil {
			t.Fatal("refused upload stored a blob")
		}
	}
}
