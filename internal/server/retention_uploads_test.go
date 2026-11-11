package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// B1: a deletion must not complete while a snapshot of the sandbox is still
// uploading, and nothing of it may stay once it completes.

// assertErased checks that no snapshot of sandbox sb is left: every row is
// purged, no blob is on the server and the node holds none of them.
func assertErased(t *testing.T, env *servertest.Env, node *servertest.FakeNode, sb string, extra ...string) {
	t.Helper()
	ids := append([]string{}, extra...)
	rows, err := env.S.DB.Query(`SELECT id FROM snapshots WHERE sandbox_id=? AND purged_at IS NULL`, sb)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		t.Errorf("snapshot %s of %s is not purged", id, sb)
	}
	rows.Close()
	rows, _ = env.S.DB.Query(`SELECT id FROM snapshots WHERE sandbox_id=?`, sb)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := env.S.Blobs.Stat("snapshot", id); err == nil {
			t.Errorf("blob %s stayed on the server", id)
		}
		if node.HasSnapshot(id) {
			t.Errorf("node still holds %s", id)
		}
	}
}

func deleteSandbox(t *testing.T, env *servertest.Env, key, id string) string {
	t.Helper()
	r := env.Do(key, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-Ascii-Confirm-Delete: "+id)
	if r.Status != 202 {
		t.Fatalf("delete: %s", r)
	}
	return r.Str("operation.id")
}

// waitUploadsStage: the operation waits for the upload, not completed.
func waitUploadsStage(t *testing.T, env *servertest.Env, key, op string) {
	t.Helper()
	env.S.GCNow()
	r := env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.stage", 5*time.Second, "waiting_for_uploads")
	if r.Str("operation.status") != "processing" || r.Get("operation.completedAt") != nil || !strings.Contains(r.Str("operation.lastError"), "uploading") {
		t.Fatalf("operation while the upload runs: %s", r)
	}
	env.S.GCNow() // more passes change nothing while it uploads
	if r := env.Do(key, "GET", "/api/v1/deletion-operations/"+op, nil); r.Str("operation.status") == "completed" {
		t.Fatalf("completed during the upload: %s", r)
	}
}

func TestDeleteWaitsForStopUpload(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	node.HoldUploads()
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	var stopSnap string
	_ = env.S.DB.QueryRow(`SELECT id FROM snapshots WHERE sandbox_id=? AND kind='stop'`, id).Scan(&stopSnap)
	if stopSnap == "" || !node.Uploading(stopSnap) {
		t.Fatalf("stop snapshot %q should still be uploading", stopSnap)
	}
	op := deleteSandbox(t, env, key, id)
	waitUploadsStage(t, env, key, op)
	node.ReleaseUploads()
	env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")
	assertErased(t, env, node, id, stopSnap)
}

func TestDeleteRunningWaitsForPeriodicUpload(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	node.HoldUploads()
	snap, err := node.PeriodicSnapshot(id) // the server has no row for it yet
	if err != nil {
		t.Fatal(err)
	}
	op := deleteSandbox(t, env, key, id)
	waitUploadsStage(t, env, key, op)
	var deleted bool
	if err := env.S.DB.QueryRow(`SELECT deleted_at IS NOT NULL FROM snapshots WHERE id=?`, snap).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("the upload in flight is not tracked as deleted data: %v %v", err, deleted)
	}
	node.ReleaseUploads()
	env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")
	assertErased(t, env, node, id, snap)
}

func TestZDRStopLeavesNothing(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	if _, err := env.S.DB.Exec(`INSERT INTO account_policy(user_id,zdr) VALUES(?,1)`, env.Admin.ID); err != nil {
		t.Fatal(err)
	}
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	node.HoldUploads()
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	var op string
	env.Eventually("zdr deletion op", func() bool {
		return env.S.DB.QueryRow(`SELECT id FROM deletion_ops WHERE target_id=? AND reason='zdr'`, id).Scan(&op) == nil
	})
	waitUploadsStage(t, env, key, op)
	node.ReleaseUploads()
	env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")
	assertErased(t, env, node, id)
}

// nodePut sends a snapshot blob as the node would.
func nodePut(t *testing.T, env *servertest.Env, node *servertest.FakeNode, id string, ref proto.SnapshotRef) int {
	t.Helper()
	req, _ := http.NewRequest("PUT", env.URL+"/node/v1/blobs/snapshot/"+id, strings.NewReader("data-"+id))
	req.Header.Set("Authorization", "Bearer "+node.Token)
	if ref.ID != "" {
		req.Header.Set("X-Crowdvm-Snapshot", string(mustJSON(t, ref)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// An upload that lands after its sandbox was deleted is refused (with the
// sandbox in the ref) or erased with the node's copy (snapshot.uploaded).
func TestLateUploadOfDeletedSandboxIsErased(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	op := deleteSandbox(t, env, key, id)
	env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")

	if st := nodePut(t, env, node, "snap_late1", proto.SnapshotRef{ID: "snap_late1", SandboxID: id}); st != 410 {
		t.Fatalf("PUT for a deleted sandbox: %d, want 410", st)
	}
	if _, err := env.S.Blobs.Stat("snapshot", "snap_late1"); err == nil {
		t.Fatal("refused blob stored")
	}
	// A node that does not send its sandbox: stored, then erased on the event.
	if st := nodePut(t, env, node, "snap_late2", proto.SnapshotRef{}); st != 200 {
		t.Fatalf("PUT without a sandbox: %d", st)
	}
	node.AddLocalSnapshot(proto.SnapshotRef{ID: "snap_late2"})
	ev := proto.Event{Time: time.Now().UTC(), SandboxID: id, Type: proto.EvSnapshotUploaded, Data: mustJSON(t, proto.SnapshotRef{ID: "snap_late2"})}
	req, _ := http.NewRequest("POST", env.URL+"/node/v1/events", strings.NewReader(string(mustJSON(t, []proto.Event{ev}))))
	req.Header.Set("Authorization", "Bearer "+node.Token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("event: %v", err)
	}
	env.Eventually("late blob and node copy erased", func() bool {
		_, err := env.S.Blobs.Stat("snapshot", "snap_late2")
		return err != nil && !node.HasSnapshot("snap_late2")
	})
}

// Orphans of earlier builds: blobs without a row and node layers of deleted
// sandboxes go; live data stays.
func TestOrphanSweeps(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	live := createSandbox(t, env, key, nil)
	env.WaitState(key, live, "ready")
	head, err := node.PeriodicSnapshot(live)
	if err != nil {
		t.Fatal(err)
	}
	env.Eventually("head recorded", func() bool { _, err := env.S.Blobs.Stat("snapshot", head); return err == nil })
	gone := createSandbox(t, env, key, nil)
	env.WaitState(key, gone, "ready")
	op := deleteSandbox(t, env, key, gone)
	env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")

	// Server: a blob with no row.
	if _, err := env.S.Blobs.Put("snapshot", "snap_orphan", strings.NewReader("x"), ""); err != nil {
		t.Fatal(err)
	}
	env.S.SweepOrphanBlobsNow()
	if _, err := env.S.Blobs.Stat("snapshot", "snap_orphan"); err == nil {
		t.Fatal("orphan blob kept")
	}
	if _, err := env.S.Blobs.Stat("snapshot", head); err != nil {
		t.Fatal("live blob erased")
	}

	// Node: layers of a deleted and an unknown sandbox go on reconnect; an
	// unrecorded layer of a live sandbox (its upload may be retried) stays.
	node.AddLocalSnapshot(proto.SnapshotRef{ID: "snap_leak1", SandboxID: gone})
	node.AddLocalSnapshot(proto.SnapshotRef{ID: "snap_leak2", SandboxID: "bx_neverexisted"})
	node.AddLocalSnapshot(proto.SnapshotRef{ID: "snap_pending", SandboxID: live})
	node.AddLocalSnapshot(proto.SnapshotRef{ID: "snap_pulled"}) // sandbox unknown: kept
	node.Disconnect()
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	env.Eventually("leaked layers erased", func() bool { return !node.HasSnapshot("snap_leak1") && !node.HasSnapshot("snap_leak2") })
	for _, id := range []string{head, "snap_pending", "snap_pulled"} {
		if !node.HasSnapshot(id) {
			t.Fatalf("live layer %s erased", id)
		}
	}
}

// m5: removing a node erases its copies when it is online; when it is
// offline, deletions of data it held report blocked instead of completed.
func TestNodeRemoveAndRetention(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	var stopSnap string
	_ = env.S.DB.QueryRow(`SELECT id FROM snapshots WHERE sandbox_id=? AND kind='stop'`, id).Scan(&stopSnap)
	env.Eventually("stop layer stored", func() bool { _, err := env.S.Blobs.Stat("snapshot", stopSnap); return err == nil })
	r := env.Do(key, "DELETE", "/api/v1/nodes/"+a.ID, nil)
	if r.Status != 200 || r.Get("snapshotCopiesLeft") != float64(0) || a.HasSnapshot(stopSnap) {
		t.Fatalf("online removal: %s (node holds %s: %v)", r, stopSnap, a.HasSnapshot(stopSnap))
	}
	op := deleteSandbox(t, env, key, id)
	env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")

	// Offline removal: the copy stays on the machine, and the op says so.
	b := env.StartNode(servertest.NodeOptions{Name: "b"})
	id2 := createSandbox(t, env, key, nil)
	env.WaitState(key, id2, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id2+"/stop", nil)
	env.WaitState(key, id2, "archived")
	env.Eventually("stop layer stored", func() bool {
		var n int
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE sandbox_id=? AND uploaded_at IS NOT NULL`, id2).Scan(&n)
		return n > 0
	})
	b.Disconnect()
	env.Eventually("b offline", func() bool { return env.S.NodeConn(b.ID) == nil })
	if r := env.Do(key, "DELETE", "/api/v1/nodes/"+b.ID, nil); r.Status != 200 || r.Get("snapshotCopiesLeft") == float64(0) {
		t.Fatalf("offline removal: %s", r)
	}
	op = deleteSandbox(t, env, key, id2)
	env.S.GCNow()
	r = env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "blocked")
	if !strings.Contains(r.Str("operation.lastError"), "node_removed") {
		t.Fatalf("blocked op does not say why: %s", r)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
