package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// sonnet-22 m1: a forced stop gave up on an offline node; when the node
// comes back, the machine it kept running there is removed.
func TestForcedStopLeftoverVMRemoved(t *testing.T) {
	defer func(d time.Duration) { server.ForcedStopLeftoverAge = d }(server.ForcedStopLeftoverAge)
	server.ForcedStopLeftoverAge = 0
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = 200 * time.Millisecond })
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	node.Disconnect()
	env.WaitState(key, id, "error")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	time.Sleep(300 * time.Millisecond)
	_, _ = env.S.DB.Exec(`UPDATE stops SET last_attempt_at=1 WHERE sandbox_id=?`, id)
	env.S.Reap()
	env.WaitState(key, id, "archived")
	if _, ok := node.VM(id); !ok {
		t.Fatal("test setup: the machine should still be on the node")
	}
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	env.Eventually("the left-behind machine removed", func() bool { _, ok := node.VM(id); return !ok })
}

// opus-23: the node dies under a boot (no answer to POST /v1/vms). The
// boot is placed again instead of ending in start_failed.
func TestBootPlacedAgainWhenNodeDies(t *testing.T) {
	env, a, _ := twoNodes(t, servertest.NodeOptions{})
	key := env.AdminKey
	a.DieOnNext("POST /v1/vms")
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.Eventually("the boot hit the dying node", func() bool { return env.S.NodeConn(a.ID) == nil })
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	r := env.WaitState(key, id, "ready", "error")
	if r.Str("sandbox.state") != "ready" {
		t.Fatalf("boot after the node died under it: %s", r)
	}
}

// opus-22 m3: the node restarted before the resumed machine ran. When it is
// back, the sandbox boots again from its last snapshot instead of keeping a
// stale node_offline error.
func TestNodeBackWithoutMachineBootsAgain(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	node.Disconnect()
	env.WaitState(key, id, "error")
	node.DropVM(id)
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	r := env.WaitState(key, id, "ready")
	if e := r.Str("sandbox.error"); e != "" {
		t.Fatalf("error after the reboot: %s", r)
	}
}

// The machine survived the node's restart only as an error: the sandbox's
// error says so instead of node_offline.
func TestNodeBackWithFailedMachine(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	node.Disconnect()
	env.WaitState(key, id, "error")
	node.SetVMState(id, proto.VMError, false, "qemu not running after node restart (overlay kept)")
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	env.Eventually("a machine error instead of node_offline", func() bool {
		return strings.HasPrefix(env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Str("sandbox.error"), "machine_error")
	})
}

// opus-21 m1: a layer only a removed node held is gone for good. The
// sandbox falls back to its newest stored layer, and a chain that still
// needs the lost layer fails for good instead of "retry".
func TestRemovedHolderFallsBack(t *testing.T) {
	env, a, _ := twoNodes(t, servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	var stored string
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&stored)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	a.HoldUploads()
	stopUntilArchived(t, env, key, id)
	var head string
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&head)
	if head == stored {
		t.Fatal("test setup: the new stop layer should be the head")
	}
	a.Disconnect()
	if r := env.Do(env.AdminKey, "DELETE", "/api/v1/nodes/"+a.ID+"?force=true", nil); r.Status/100 != 2 {
		t.Fatalf("remove node: %s", r)
	}
	_ = env.S.DB.QueryRow(`SELECT head_snapshot_id FROM sandboxes WHERE id=?`, id).Scan(&head)
	if head != stored {
		t.Fatalf("head %s after removing its only holder, want the stored %s", head, stored)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status != 202 {
		t.Fatalf("resume from the stored layer: %s", r)
	}
}

// bugs-r2-opus 1: a lost node's sandbox waits in the queue to be rebuilt
// (no room elsewhere); its node comes back with the machine still running.
// The sandbox takes that machine back (ready on that node) instead of
// turning ready with no node.
func TestQueuedRebuildAdoptsReturningMachine(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = 100 * time.Millisecond })
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	env.StartNode(servertest.NodeOptions{Name: "b", Paused: true}) // could host it, takes nothing now
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	_, _ = env.S.DB.Exec(`UPDATE sandboxes SET placement='{}' WHERE id=?`, id)
	a.Disconnect()
	env.WaitState(key, id, "error")
	time.Sleep(200 * time.Millisecond)
	env.S.Reap()
	env.Eventually("rebuild queued", func() bool {
		return env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Str("sandbox.state") == "provisioning"
	})
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	env.Eventually("the returning machine adopted", func() bool {
		r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil)
		return r.Str("sandbox.state") == "ready" && r.Str("sandbox.nodeId") == a.ID
	})
	if _, ok := a.VM(id); !ok {
		t.Fatal("the adopted machine was removed")
	}
}

// bugs-r2-opus 2: the node served a boot late, after the server placed it
// again on the same node: vm_exists replaces that machine and the boot
// succeeds instead of failing.
func TestBootReplacesExistingMachine(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	node.RefuseNext("POST /v1/vms", 409, map[string]any{"code": "vm_exists", "message": "vm already exists (starting)"})
	id := createSandbox(t, env, key, nil)
	r := env.WaitState(key, id, "ready", "error")
	if r.Str("sandbox.state") != "ready" || !node.Called("DELETE /v1/vms/"+id) {
		t.Fatalf("boot after vm_exists: %s", r)
	}
}

// bugs-r3-opus 2: deleting every snapshot of a running sandbox keeps the
// layers its machine builds on, so its next stop resumes.
func TestDeleteSnapshotsOfRunningSandboxKeepsItsChain(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	if _, err := node.PeriodicSnapshot(id); err != nil {
		t.Fatal(err)
	}
	env.Eventually("the periodic layer stored", func() bool {
		var n int
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE sandbox_id=? AND kind='periodic' AND uploaded_at IS NOT NULL`, id).Scan(&n)
		return n > 0
	})
	var ids []string
	rows, _ := env.S.DB.Query(`SELECT id FROM snapshots WHERE sandbox_id=?`, id)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		ids = append(ids, s)
	}
	rows.Close()
	for _, sn := range ids {
		if r := env.Do(key, "DELETE", "/api/v1/snapshots/"+sn, nil, "X-Ascii-Confirm-Delete: "+sn); r.Status/100 != 2 {
			t.Fatalf("delete %s: %s", sn, r)
		}
	}
	env.S.GCNow()
	var purged int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE sandbox_id=? AND purged_at IS NOT NULL`, id).Scan(&purged)
	if purged != 0 {
		t.Fatalf("%d layers under the running machine purged", purged)
	}
	stopUntilArchived(t, env, key, id)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	if r := env.WaitState(key, id, "ready", "error"); r.Str("sandbox.state") != "ready" {
		t.Fatalf("resume after deleting the running sandbox's snapshots: %s", r)
	}
}

// bugs-r3-opus 1: several reports of the returning machine at once (event
// and heartbeat): one adopts it, none deletes it as an orphan.
func TestQueuedRebuildConcurrentReports(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = 100 * time.Millisecond })
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	env.StartNode(servertest.NodeOptions{Name: "b", Paused: true})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	_, _ = env.S.DB.Exec(`UPDATE sandboxes SET placement='{}' WHERE id=?`, id)
	a.Disconnect()
	env.WaitState(key, id, "error")
	time.Sleep(200 * time.Millisecond)
	env.S.Reap()
	env.Eventually("rebuild queued", func() bool {
		return env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Str("sandbox.state") == "provisioning"
	})
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	st, _ := a.VM(id)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); env.S.ReconcileVMNow(a.ID, st) }()
	}
	wg.Wait()
	env.Eventually("adopted", func() bool {
		r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil)
		return r.Str("sandbox.state") == "ready" && r.Str("sandbox.nodeId") == a.ID
	})
	time.Sleep(100 * time.Millisecond)
	if _, ok := a.VM(id); !ok || a.Called("DELETE /v1/vms/"+id) {
		t.Fatal("the adopted machine was deleted as an orphan")
	}
}

// bugs-r5-sonnet: OTel forgets the CPU samples of sandboxes that stopped
// running or were deleted.
func TestOTelCPUSamplesPruned(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if n := server.OTelPruneCPU(env.S, []string{id, "bx_gone"}); n != 1 {
		t.Fatalf("%d samples kept, want the running sandbox's only", n)
	}
}

// bugs-r5-opus 5: one account's hanging collector does not hold up another
// account's lifecycle logs.
func TestOTelSlowCollectorIsolatesLogs(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	_, slowKey := env.NewUser("slowlog", false)
	_, fastKey := env.NewUser("fastlog", false)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/logs" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}))
	defer slow.Close()
	defer close(release) // runs first: the hanging handler returns before slow.Close waits for it
	var got atomic.Int32
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/logs" {
			got.Add(1)
		}
	}))
	defer fast.Close()
	for key, url := range map[string]string{slowKey: slow.URL, fastKey: fast.URL} {
		if r := env.Do(key, "PUT", "/api/v1/telemetry", map[string]any{"endpoint": url}); r.Status != 200 {
			t.Fatal(r)
		}
	}
	got.Store(0)
	createSandbox(t, env, slowKey, nil) // its logs hang at the slow collector
	time.Sleep(300 * time.Millisecond)
	id := createSandbox(t, env, fastKey, nil)
	env.WaitState(fastKey, id, "ready")
	deadline := time.Now().Add(3 * time.Second)
	for got.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got.Load() == 0 {
		t.Fatal("the other account's logs waited behind the hanging collector")
	}
}

// bugs-r8-opus 2: the node is back, then reports the sandbox's machine
// failed (a lazy boot broken by the restart): the sandbox boots again
// instead of staying node_offline.
func TestMachineFailedAfterNodeReturnBootsAgain(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	node.Disconnect()
	env.WaitState(key, id, "error")
	// The machine's boot did not come up (its guest never answered), so the
	// reconnect does not bring the sandbox back: a guest-ready report won
	// the race against the failure event under load, and the failure was
	// then an ordinary one.
	node.SetVMState(id, proto.VMRunning, false, "")
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	node.SetVMState(id, proto.VMError, false, "boot timeout: guest agent not healthy after 3m0s")
	r := env.WaitState(key, id, "ready", "error")
	if r.Str("sandbox.state") == "error" { // the first report may still be pending a reboot
		r = env.WaitState(key, id, "ready")
	}
	if e := r.Str("sandbox.error"); e != "" {
		t.Fatalf("after the machine failed on the returned node: %s", r)
	}
}

// bugs-r9-opus 1: resume right after the node dropped does not boot an
// older snapshot elsewhere (the node's machine may have newer data); after
// a forced stop, or once the node is given up, it does.
func TestResumeWaitsForRecentlyOfflineNode(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = time.Hour })
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	env.StartNode(servertest.NodeOptions{Name: "b"})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	stopUntilArchived(t, env, key, id)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	_, _ = env.S.DB.Exec(`UPDATE sandboxes SET placement='{}' WHERE id=?`, id)
	a.Disconnect()
	env.WaitState(key, id, "error")
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	if r.Status != 409 || r.Code() != "node_offline" || r.Get("error.details.retryable") != true {
		t.Fatalf("resume right after the node dropped: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", map[string]any{"force": true}); r.Status/100 != 2 {
		t.Fatalf("forced stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status != 202 {
		t.Fatalf("resume after the forced stop: %s", r)
	}
}
