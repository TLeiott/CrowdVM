package server_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// opus-19: layers recorded but not stored live only on the node that took
// them; a node restart cuts their uploads off (N1), and a forced stop of a
// lost node must not leave such a layer as the head (N2).

func headOf(env *servertest.Env, id string) string {
	var h string
	_ = env.S.DB.QueryRow(`SELECT COALESCE(head_snapshot_id,'') FROM sandboxes WHERE id=?`, id).Scan(&h)
	return h
}

func pendingOf(env *servertest.Env, name string) string {
	var p string
	_ = env.S.DB.QueryRow(`SELECT COALESCE(pending_snapshot_id,'') FROM saved_snapshots WHERE name=?`, name).Scan(&p)
	return p
}

// storedLayer takes a periodic layer on n and waits until it is stored.
func storedLayer(t *testing.T, env *servertest.Env, n *servertest.FakeNode, id string) string {
	t.Helper()
	l, err := n.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	env.Eventually(l+" stored", func() bool { _, up, _, _ := snapRowMaybe(env, l); return up })
	return l
}

// N1: the node restarts while it uploads a stopped sandbox's chain (a
// periodic layer, a save's live layer and the stop layer). While it is
// offline, a resume elsewhere answers 409 snapshot_unavailable at once.
// Once it is back, the server asks it for the layers, oldest first; the
// resume elsewhere waits for them (and says so), then runs with the whole
// chain, and the save built on them finishes.
func TestNodeRestartMidUploadThenResumeElsewhere(t *testing.T) {
	env, a, b := twoNodes(t, servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	storedLayer(t, env, a, id)
	a.HoldUploads()
	p, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "tpl"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.Eventually("save's layer recorded", func() bool { return pendingOf(env, "tpl") != "" })
	live := pendingOf(env, "tpl")
	stop := stopUntilArchived(t, env, key, id)

	a.Disconnect()
	env.Eventually("a offline", func() bool { return env.S.NodeConn(a.ID) == nil })
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"placement": map[string]any{"node": "b"}})
	if r.Status != 409 || r.Code() != "snapshot_unavailable" || r.Get("error.details.retryable") != true {
		t.Fatalf("resume elsewhere with the holder offline: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{"placement": map[string]any{"node": "b"}}); r.Status != 409 || r.Code() != "snapshot_unavailable" {
		t.Fatalf("fork with the holder offline: %s", r)
	}

	if err := a.Restart(); err != nil { // the uploads in flight are lost; new ones stay held
		t.Fatal(err)
	}
	env.Eventually("a asked for its unstored layers", func() bool {
		req := a.UploadRequests()
		return slices.Contains(req, p) && slices.Contains(req, live) && slices.Contains(req, stop)
	})
	if req := a.UploadRequests(); !(slices.Index(req, p) < slices.Index(req, live) && slices.Index(req, live) < slices.Index(req, stop)) {
		t.Fatalf("upload requests not oldest first: %v (p %s, live %s, stop %s)", req, p, live, stop)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"placement": map[string]any{"node": "b"}}); r.Status != 202 {
		t.Fatalf("resume elsewhere with the holder back: %s", r)
	}
	env.Eventually("the wait is shown", func() bool {
		return env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Str("sandbox.waitingForSnapshot") != ""
	})
	a.ReleaseUploads()
	env.WaitState(key, id, "ready")
	if sb, _ := env.S.SandboxByID(id); sb.NodeID != b.ID || !b.HasSnapshot(p) || !b.HasSnapshot(live) || !b.HasSnapshot(stop) {
		t.Fatalf("resumed on %s; b holds p %v, live %v, stop %v", sb.NodeID, b.HasSnapshot(p), b.HasSnapshot(live), b.HasSnapshot(stop))
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/tpl", "snapshot.status", 5*time.Second, "ready")
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.waitingForSnapshot") != "" {
		t.Fatalf("still waiting once ready: %s", r)
	}
}

// N1: a boot elsewhere that waits for a layer whose holder goes offline
// fails with snapshot_unavailable soon, not after the boot timeout.
func TestBootFailsWhenHolderGoesOffline(t *testing.T) {
	defer func(g time.Duration) { server.HolderOfflineGrace = g }(server.HolderOfflineGrace)
	server.HolderOfflineGrace = 300 * time.Millisecond
	env, a, _ := twoNodes(t, servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	a.HoldUploads()
	if _, err := a.PeriodicSnapshot(id); err != nil {
		t.Fatal(err)
	}
	stopUntilArchived(t, env, key, id)
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"placement": map[string]any{"node": "b"}}); r.Status != 202 {
		t.Fatalf("resume: %s", r)
	}
	env.Eventually("waiting for the upload", func() bool {
		return env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Str("sandbox.waitingForSnapshot") != ""
	})
	a.Disconnect()
	r := env.WaitState(key, id, "error")
	if msg := r.Str("sandbox.error"); !strings.HasPrefix(msg, "snapshot_unavailable") {
		t.Fatalf("boot error: %s", r)
	}
}

// N2: a save's live layer is the head only once stored, and a forced stop
// of a lost node falls back to the newest stored layer, so a resume
// elsewhere boots at once. The unstored layer stays recorded: when the node
// comes back it is uploaded and the save finishes. It becomes the head
// then only if the sandbox did not move on.
func TestForcedStopAfterNodeLossUsesLastStoredSnapshot(t *testing.T) {
	for _, resumeFirst := range []bool{true, false} {
		env, a, b := twoNodes(t, servertest.NodeOptions{})
		key := env.AdminKey
		id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
		env.WaitState(key, id, "ready")
		p1 := storedLayer(t, env, a, id)
		a.HoldUploads()
		p2, err := a.PeriodicSnapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "sv"}); r.Status != 202 {
			t.Fatalf("save: %s", r)
		}
		env.Eventually("save's layer recorded", func() bool { return pendingOf(env, "sv") != "" })
		live := pendingOf(env, "sv")
		if h := headOf(env, id); h != p1 {
			t.Fatalf("head %s before its upload (want the stored %s)", h, p1)
		}
		// Even with a head on an unstored layer (as after a resume on the
		// same node), the forced stop falls back to the stored one.
		_, _ = env.S.DB.Exec(`UPDATE sandboxes SET head_snapshot_id=? WHERE id=?`, live, id)

		a.Disconnect() // node lost
		env.Eventually("a offline", func() bool { return env.S.NodeConn(a.ID) == nil })
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", map[string]any{"force": true}); r.Status/100 != 2 {
			t.Fatalf("forced stop: %s", r)
		}
		env.WaitState(key, id, "archived")
		if h := headOf(env, id); h != p1 {
			t.Fatalf("resumeFirst=%v: head after the forced stop %s, want the last stored %s", resumeFirst, h, p1)
		}
		if resumeFirst {
			if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"placement": map[string]any{"node": "b"}}); r.Status != 202 {
				t.Fatalf("resume: %s", r)
			}
			env.WaitState(key, id, "ready")
			if !b.HasSnapshot(p1) || b.HasSnapshot(live) {
				t.Fatalf("b holds p1 %v, the unstored live layer %v", b.HasSnapshot(p1), b.HasSnapshot(live))
			}
		}
		if err := a.Restart(); err != nil {
			t.Fatal(err)
		}
		a.ReleaseUploads()
		env.WaitFor(key, "/api/v1/saved-snapshots/sv", "snapshot.status", 5*time.Second, "ready")
		env.Eventually("p2 and live stored", func() bool {
			_, up2, _, _ := snapRowMaybe(env, p2)
			_, upl, _, _ := snapRowMaybe(env, live)
			return up2 && upl
		})
		want := live // came back before a resume: the newer data is the head again
		if resumeFirst {
			want = p1 // runs on b from p1 now: the old node's layers never become the head
		}
		if h := headOf(env, id); h != want {
			t.Fatalf("resumeFirst=%v: head %s once the layers landed, want %s (p1 %s, p2 %s, live %s)", resumeFirst, h, want, p1, p2, live)
		}
	}
}

// sonnet-21: a plain stop of a sandbox whose node is only offline is not
// forced. The machine may still run there with newer data: the stop waits
// for the node and then snapshots it, rather than keep an older layer.
func TestStopWaitsForOfflineNode(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	node.Disconnect()
	env.WaitState(key, id, "error")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	var stopID string
	var forced int
	env.Eventually("a stop waiting for the node", func() bool {
		return env.S.DB.QueryRow(`SELECT id,forced FROM stops WHERE sandbox_id=? AND status='failing'`, id).Scan(&stopID, &forced) == nil
	})
	if forced != 0 {
		t.Fatal("the stop of a node_offline sandbox was forced")
	}
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	_, _ = env.S.DB.Exec(`UPDATE stops SET last_attempt_at=1 WHERE id=?`, stopID)
	env.S.Reap()
	env.WaitState(key, id, "archived")
	var snap string
	_ = env.S.DB.QueryRow(`SELECT forced,COALESCE(snapshot_id,'') FROM stops WHERE id=?`, stopID).Scan(&forced, &snap)
	if forced != 0 || snap == "" || !node.Called("POST /v1/vms/"+id+"/stop") {
		t.Fatalf("stop after the node came back: forced %d, layer %q", forced, snap)
	}
}

// A stop waiting for an offline node gives up after NodeLostGrace, like the
// rebuild elsewhere: forced, on the last snapshot.
func TestStopGivesUpOnLostNode(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = 300 * time.Millisecond })
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	node.Disconnect()
	env.WaitState(key, id, "error")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	time.Sleep(400 * time.Millisecond)
	_, _ = env.S.DB.Exec(`UPDATE stops SET last_attempt_at=1 WHERE sandbox_id=?`, id)
	env.S.Reap()
	env.WaitState(key, id, "archived")
	var forced int
	_ = env.S.DB.QueryRow(`SELECT forced FROM stops WHERE sandbox_id=? ORDER BY requested_at DESC LIMIT 1`, id).Scan(&forced)
	if forced != 1 {
		t.Fatal("the stop of a lost node was not forced")
	}
}
