package server_test

import (
	"encoding/json"
	"github.com/crowdvm/crowdvm/internal/proto"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// opus-18: a periodic layer is recorded when the node takes it (parent,
// node as location, not stored yet), so whatever builds on it while it
// uploads (stop, save, fork) sees a whole chain.

func twoNodes(t *testing.T, o servertest.NodeOptions) (*servertest.Env, *servertest.FakeNode, *servertest.FakeNode) {
	env := servertest.New(t)
	o.Name = "a"
	a := env.StartNode(o)
	o.Name = "b"
	b := env.StartNode(o)
	return env, a, b
}

func snapRow(t *testing.T, env *servertest.Env, id string) (parent string, uploaded, deleted, purged bool) {
	t.Helper()
	if err := env.S.DB.QueryRow(`SELECT COALESCE(parent_id,''), uploaded_at IS NOT NULL, deleted_at IS NOT NULL, purged_at IS NOT NULL FROM snapshots WHERE id=?`, id).
		Scan(&parent, &uploaded, &deleted, &purged); err != nil {
		t.Fatalf("snapshot %s: %v", id, err)
	}
	return
}

// stopUntilArchived stops a sandbox and returns its stop snapshot.
func stopUntilArchived(t *testing.T, env *servertest.Env, key, id string) string {
	t.Helper()
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	var snap string
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&snap)
	return snap
}

func TestPeriodicLayerRecordedWhenTaken(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	l0, err := node.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	env.WaitFor(key, "/api/v1/sandboxes/"+id+"/snapshots/latest", "snapshot.uploaded", 5*time.Second, "true")
	node.HoldUploads()
	p, err := node.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	parent, uploaded, deleted, _ := snapRow(t, env, p)
	if parent != l0 || uploaded || deleted {
		t.Fatalf("in-flight layer: parent %q uploaded %v deleted %v", parent, uploaded, deleted)
	}
	var head string
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&head)
	if head != l0 {
		t.Fatalf("head %s moved to a layer not stored yet (want %s)", head, l0)
	}
	env.S.GCNow() // GC keeps it and its parent
	if _, _, _, purged := snapRow(t, env, l0); purged {
		t.Fatal("parent of an in-flight layer purged")
	}
	node.ReleaseUploads()
	env.Eventually("head moves once stored", func() bool {
		_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&head)
		return head == p
	})
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.lastSnapshotStatus") == "in_progress" {
		t.Fatalf("snapshot status: %s", r)
	}
}

// B2: stop then resume while a periodic layer uploads: same node at once,
// another node once the layer is stored; never resume_failed.
func TestResumeDuringPeriodicUpload(t *testing.T) {
	env, a, b := twoNodes(t, servertest.NodeOptions{})
	key := env.AdminKey
	for _, target := range []string{"a", "b"} {
		id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
		env.WaitState(key, id, "ready")
		a.HoldUploads()
		p, err := a.PeriodicSnapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		stop := stopUntilArchived(t, env, key, id)
		if !chainHas(env, stop, p) {
			t.Fatalf("stop snapshot %s does not build on %s", stop, p)
		}
		r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"placement": map[string]any{"node": target}})
		if r.Status != 202 {
			t.Fatalf("resume on %s during the upload: %s", target, r)
		}
		if target == "a" { // its chain is on a: no wait
			env.WaitState(key, id, "ready")
			a.ReleaseUploads()
			continue
		}
		time.Sleep(300 * time.Millisecond)
		if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "provisioning" {
			t.Fatalf("resume elsewhere before the upload ended: %s", r)
		}
		a.ReleaseUploads()
		env.WaitState(key, id, "ready")
		if sb, _ := env.S.SandboxByID(id); sb.NodeID != b.ID || !b.HasSnapshot(p) || !b.HasSnapshot(stop) {
			t.Fatalf("resumed on %s; b holds %s: %v, %s: %v", sb.NodeID, p, b.HasSnapshot(p), stop, b.HasSnapshot(stop))
		}
	}
}

// B3: a fork onto another node during a periodic upload waits for it.
func TestCrossNodeForkDuringPeriodicUpload(t *testing.T) {
	env, a, b := twoNodes(t, servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	a.HoldUploads()
	p, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{"placement": map[string]any{"node": "b"}})
	if r.Status != 202 {
		t.Fatalf("fork: %s", r)
	}
	fork := r.Str("sandbox.id")
	time.Sleep(300 * time.Millisecond)
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+fork, nil); r.Str("sandbox.state") == "error" {
		t.Fatalf("fork failed during the upload: %s", r)
	}
	a.ReleaseUploads()
	env.WaitState(key, fork, "ready")
	if sb, _ := env.S.SandboxByID(fork); sb.NodeID != b.ID || !b.HasSnapshot(p) {
		t.Fatalf("fork on %s; b holds %s: %v", sb.NodeID, p, b.HasSnapshot(p))
	}
}

// B1: deleting the source while a periodic layer uploads keeps what a saved
// snapshot and a fork built on it need, down to the oldest layer, also when
// the node never reported the layer (the deletion then tracks it with its
// parent).
func TestDeleteSourceDuringPeriodicUploadKeepsSavedAndFork(t *testing.T) {
	for _, report := range []bool{true, false} {
		env, a, b := twoNodes(t, servertest.NodeOptions{NoSnapshotReport: !report})
		key := env.AdminKey
		id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
		env.WaitState(key, id, "ready")
		l0, err := a.PeriodicSnapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		env.Eventually("l0 stored", func() bool { _, up, _, _ := snapRowMaybe(env, l0); return up })
		a.HoldUploads()
		p, err := a.PeriodicSnapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "tplx"}); r.Status != 202 {
			t.Fatalf("save: %s", r)
		}
		r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{"placement": map[string]any{"node": "a"}})
		if r.Status != 202 {
			t.Fatalf("fork: %s", r)
		}
		fork := r.Str("sandbox.id")
		env.WaitState(key, fork, "ready")
		op := deleteSandbox(t, env, key, id)
		env.S.GCNow()
		a.ReleaseUploads()
		env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")
		env.WaitFor(key, "/api/v1/saved-snapshots/tplx", "snapshot.status", 5*time.Second, "ready")
		env.S.GCNow()
		if parent, _, _, purged := snapRow(t, env, p); parent != l0 || purged {
			t.Fatalf("report=%v: in-flight layer %s: parent %q purged %v", report, p, parent, purged)
		}
		if _, _, _, purged := snapRow(t, env, l0); purged {
			t.Fatalf("report=%v: oldest layer %s purged under a saved snapshot and a fork", report, l0)
		}
		// The saved snapshot deploys on the other node with its whole chain.
		r = env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"from": "tplx", "placement": map[string]any{"node": "b"}})
		if r.Status != 202 {
			t.Fatalf("deploy: %s", r)
		}
		env.WaitState(key, r.Str("sandbox.id"), "ready")
		if !b.HasSnapshot(l0) || !b.HasSnapshot(p) {
			t.Fatalf("report=%v: deploy on b without its chain: %s %v, %s %v", report, l0, b.HasSnapshot(l0), p, b.HasSnapshot(p))
		}
		// The fork resumes on the other node with its whole chain too.
		stopUntilArchived(t, env, key, fork)
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+fork+"/resume", map[string]any{"placement": map[string]any{"node": "b"}}); r.Status != 202 {
			t.Fatalf("fork resume: %s", r)
		}
		env.WaitState(key, fork, "ready")
	}
}

func chainHas(env *servertest.Env, id, want string) bool {
	for i := 0; id != "" && i < 100; i++ {
		if id == want {
			return true
		}
		id, _, _, _ = snapRowMaybe(env, id)
	}
	return false
}

func snapRowMaybe(env *servertest.Env, id string) (parent string, uploaded, deleted, purged bool) {
	_ = env.S.DB.QueryRow(`SELECT COALESCE(parent_id,''), uploaded_at IS NOT NULL, deleted_at IS NOT NULL, purged_at IS NOT NULL FROM snapshots WHERE id=?`, id).
		Scan(&parent, &uploaded, &deleted, &purged)
	return
}

// B4: a sandbox moves to another node while a periodic layer uploads (the
// stop merged it), then is deleted: the old node's copy is erased before
// the deletion completes. Also without the report (recorded on upload as
// deleted data held by the old node).
func TestMovedDuringPeriodicUploadThenDeleted(t *testing.T) {
	for _, report := range []bool{true, false} {
		env, a, _ := twoNodes(t, servertest.NodeOptions{NoSnapshotReport: !report})
		key := env.AdminKey
		id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
		env.WaitState(key, id, "ready")
		if _, err := a.PeriodicSnapshot(id); err != nil {
			t.Fatal(err)
		}
		a.HoldUploads()
		p, err := a.PeriodicSnapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		a.LetNewUploadsThrough() // p stays in flight
		a.MergeNextSnapshot(id)
		stopUntilArchived(t, env, key, id)
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"placement": map[string]any{"node": "b"}}); r.Status != 202 {
			t.Fatalf("resume: %s", r)
		}
		env.WaitState(key, id, "ready")
		a.ReleaseUploads() // p lands while the sandbox runs on b
		env.Eventually("p stored", func() bool { _, err := env.S.Blobs.Stat("snapshot", p); return err == nil || !a.Uploading(p) })
		op := deleteSandbox(t, env, key, id)
		env.S.GCNow()
		env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")
		if a.HasSnapshot(p) {
			t.Fatalf("report=%v: the old node still holds %s after the deletion completed", report, p)
		}
		assertErased(t, env, a, id, p)
	}
}

// m-a, m-b: removing an online node with --force destroys its machines,
// and sandboxes pinned to it can be placed elsewhere.
func TestForceRemoveOnlineNode(t *testing.T) {
	env, a, b := twoNodes(t, servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	if _, err := a.PeriodicSnapshot(id); err != nil {
		t.Fatal(err)
	}
	stopped := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, stopped, "ready")
	stopUntilArchived(t, env, key, stopped)
	if r := env.Do(key, "DELETE", "/api/v1/nodes/"+a.ID+"?force=true", nil); r.Status != 200 {
		t.Fatalf("remove: %s", r)
	}
	if _, ok := a.VM(id); ok {
		t.Fatal("the removed node still runs the sandbox")
	}
	if sb, _ := env.S.SandboxByID(stopped); strings.Contains(mustJSONString(sb.Placement), `"node"`) {
		t.Fatalf("pin to the removed node kept: %+v", sb.Placement)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+stopped+"/resume", nil); r.Status != 202 {
		t.Fatalf("resume without --node: %s", r)
	}
	env.WaitState(key, stopped, "ready")
	if sb, _ := env.S.SandboxByID(stopped); sb.NodeID != b.ID {
		t.Fatalf("resumed on %s, want b", sb.NodeID)
	}
}

func mustJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A layer whose parent is not recorded yet (its node's report was lost to a
// server restart) is retryable, never resume_failed (sonnet-18 B1).
func TestResumeParentNotYetRecorded(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	var head string
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&head)
	_, _ = env.S.DB.Exec(`UPDATE snapshots SET parent_id='snap_notyetreported' WHERE id=?`, head)
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	if r.Status != 409 || r.Code() != "snapshot_unavailable" || r.Get("error.details.retryable") != true {
		t.Fatalf("missing parent: %s", r)
	}
}

// opus-21 N1 / opus-22 B1: the node stopped the VM into this stop's layer,
// but the answer never reached the server (both restarted). The retry gets
// a 404 and adopts exactly that layer, also when the node holds other
// leaves of the sandbox (a merge, an earlier run).
func TestStopAdoptsNodeLayerAfter404(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	var first string
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&first)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	node.RefuseNext("POST /v1/vms/"+id+"/stop", 500, map[string]any{"code": "internal"})
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	var stopID, layer string
	env.Eventually("a failed stop attempt", func() bool {
		return env.S.DB.QueryRow(`SELECT id,COALESCE(snapshot_id,'') FROM stops WHERE sandbox_id=? AND status='failing'`, id).Scan(&stopID, &layer) == nil
	})
	if layer == "" {
		t.Fatal("the stop's layer id was not kept for its retries")
	}
	// The node did stop into it; it also holds an unrelated leaf.
	node.AddLocalSnapshot(proto.SnapshotRef{ID: layer, SandboxID: id, ParentID: first})
	node.AddLocalSnapshot(proto.SnapshotRef{ID: "snap_otherleaf", SandboxID: id, ParentID: first})
	node.RefuseNext("POST /v1/vms/"+id+"/stop", 404, map[string]any{"code": "not_found"})
	_, _ = env.S.DB.Exec(`UPDATE stops SET last_attempt_at=1 WHERE id=?`, stopID)
	env.S.Reap()
	env.WaitState(key, id, "archived")
	var head string
	var forced int
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&head)
	_ = env.S.DB.QueryRow(`SELECT forced FROM stops WHERE id=?`, stopID).Scan(&forced)
	if head != layer || forced != 0 {
		t.Fatalf("head %q (forced %d), want the stop's layer %s", head, forced, layer)
	}
}

// opus-22 B2: a 404 on stop never adopts a layer this stop did not take (an
// old layer from an earlier run on that node): the head stays.
func TestStop404IgnoresStaleNodeLayer(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	var head string
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&head)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	node.AddLocalSnapshot(proto.SnapshotRef{ID: "snap_stale", SandboxID: id})
	node.RefuseNext("POST /v1/vms/"+id+"/snapshot", 404, map[string]any{"code": "not_found"})
	node.RefuseNext("POST /v1/vms/"+id+"/stop", 404, map[string]any{"code": "not_found"})
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	var now string
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&now)
	if now != head {
		t.Fatalf("head %q after the 404, want %s (the stale node layer was adopted)", now, head)
	}
}

// A stopped sandbox whose only layers are still uploading is retryable, not
// "snapshot data expired" (sonnet-19).
func TestResumeWhileFirstLayerUploads(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	var head string
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&head)
	_, _ = env.S.DB.Exec(`UPDATE snapshots SET uploaded_at=NULL WHERE id=?`, head)
	_, _ = env.S.DB.Exec(`UPDATE sandboxes SET head_snapshot_id=NULL WHERE id=?`, id)
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	if r.Status != 409 || r.Code() != "snapshot_unavailable" || r.Get("error.details.retryable") != true {
		t.Fatalf("resume while the first layer uploads: %s", r)
	}
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", nil) // sonnet-20
	if r.Status != 409 || r.Code() != "snapshot_unavailable" || r.Get("error.details.retryable") != true {
		t.Fatalf("fork while the first layer uploads: %s", r)
	}
}
