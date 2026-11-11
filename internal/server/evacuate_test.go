package server_test

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func noticeCount(t *testing.T, env *servertest.Env, sbID, kind string) int {
	t.Helper()
	var n int
	if err := env.S.DB.QueryRow(`SELECT COUNT(*) FROM notices WHERE sandbox_id=? AND kind=?`, sbID, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEvacuateAuth(t *testing.T) {
	env := servertest.New(t)
	alice, aliceKey := env.NewUser("alice", false)
	_, bobKey := env.NewUser("bob", false)
	_, tok, err := env.S.CreateJoinToken(server.JoinTokenParams{Name: "a", CreatedBy: alice.ID, OwnerID: alice.ID, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	node := env.StartNode(servertest.NodeOptions{JoinToken: tok})
	path := "/api/v1/nodes/" + node.ID + "/evacuate"

	if r := env.Do(bobKey, "POST", path, map[string]any{"graceSec": 60}); r.Status != 403 {
		t.Fatalf("other user: %s", r)
	}
	if r := env.Do(aliceKey, "POST", path, map[string]any{"graceSec": -1}); r.Status != 400 {
		t.Fatalf("negative grace: %s", r)
	}
	if r := env.Do(aliceKey, "POST", path, map[string]any{"graceSec": 2 * 86400}); r.Status != 400 {
		t.Fatalf("grace over the cap: %s", r)
	}
	// An empty node: the evacuation is over at once (the node stays drained).
	r := env.Do(aliceKey, "POST", path, nil)
	if r.Status != 200 || r.Str("node.disabled") != "true" {
		t.Fatalf("owner evacuate: %s", r)
	}
	env.Eventually("empty evacuation ends", func() bool {
		r := env.Do(aliceKey, "GET", "/api/v1/nodes/"+node.ID, nil)
		return r.Get("node.evacuation") == nil && r.Str("node.disabled") == "true"
	})
	if r := env.Do(aliceKey, "DELETE", path, nil); r.Status != 404 {
		t.Fatalf("cancel without evacuation: %s", r)
	}
}

func TestEvacuateMovesSandboxes(t *testing.T) {
	old := server.EarningsCacheTTL
	server.EarningsCacheTTL = 0
	defer func() { server.EarningsCacheTTL = old }()
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{})
	snap := createSandbox(t, env, key, map[string]any{"name": "keep"})
	gone := createSandbox(t, env, key, map[string]any{"name": "scratch", "snapshots": false})
	env.WaitState(key, snap, "ready")
	env.WaitState(key, gone, "ready")
	b := env.StartNode(servertest.NodeOptions{})

	// Earnings reach the node in its heartbeat.
	now := time.Now().UnixMilli()
	_, _ = env.S.DB.Exec(`INSERT INTO credit_ledger(wallet_id,delta,kind,node_id,created_at) VALUES('w',2.5,'earn',?,?),('w',1,'earn',?,?)`,
		a.ID, now, a.ID, now-48*3600_000)
	env.Eventually("earnings in the heartbeat", func() bool {
		hb := a.LastHeartbeatResponse()
		return hb.EarnedCredits == 3.5 && hb.Earned24h == 2.5
	})

	path := "/api/v1/nodes/" + a.ID + "/evacuate"
	r := env.Do(key, "POST", path, map[string]any{"graceSec": 3600})
	if r.Status != 200 || r.Str("node.disabled") != "true" || r.Str("node.evacuation.deadline") == "" || r.Str("node.noSnapshotSandboxes") != "1" {
		t.Fatalf("evacuate: %s", r)
	}
	if noticeCount(t, env, snap, "node_evacuation") != 1 || noticeCount(t, env, gone, "node_evacuation") != 1 {
		t.Fatal("owners were not told")
	}
	env.Eventually("evacuation in the heartbeat", func() bool {
		hb := a.LastHeartbeatResponse()
		return hb.Evacuation != nil && hb.Drained
	})

	// Cancel during the grace period: the node stays drained, nothing moves.
	if r := env.Do(key, "DELETE", path, nil); r.Status != 200 || r.Get("node.evacuation") != nil || r.Str("node.disabled") != "true" {
		t.Fatalf("cancel: %s", r)
	}
	env.Eventually("heartbeat without evacuation", func() bool { return a.LastHeartbeatResponse().Evacuation == nil })
	time.Sleep(200 * time.Millisecond) // a few reaper passes
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+snap, nil); r.Str("sandbox.state") != "ready" || r.Str("sandbox.nodeId") != a.ID {
		t.Fatalf("cancelled evacuation moved a sandbox: %s", r)
	}

	// Now: the snapshot sandbox is stopped (disk saved) and resumes on b;
	// the --no-snapshots one is erased.
	if r := env.Do(key, "POST", path, map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("evacuate now: %s", r)
	}
	env.WaitFor(key, "/api/v1/sandboxes/"+snap, "sandbox.nodeId", servertest.WaitTimeout, b.ID)
	r = env.WaitState(key, snap, "ready")
	if r.Str("sandbox.nodeId") != b.ID {
		t.Fatalf("moved sandbox: %s", r)
	}
	if !a.Called("POST /v1/vms/" + snap + "/stop") {
		t.Fatal("the sandbox was not stopped through the normal stop")
	}
	env.WaitState(key, gone, "archived")
	if !a.Called("DELETE /v1/vms/" + gone) {
		t.Fatal("the --no-snapshots sandbox was not erased")
	}
	env.Eventually("evacuation ends", func() bool {
		r := env.Do(key, "GET", "/api/v1/nodes/"+a.ID, nil)
		return r.Get("node.evacuation") == nil && r.Str("node.disabled") == "true"
	})
	// The move is not counted as the owner's start.
	var starts int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM starts`).Scan(&starts)
	if starts != 2 {
		t.Fatalf("starts recorded: %d, want 2 (the creates)", starts)
	}
}

// A failed save refuses the evacuation's stop (the sandbox keeps running,
// the stop is retried); after the deadline the evacuation can't be
// cancelled. With no other node the stopped sandbox stays stopped.
func TestEvacuateRefusedAndStranded(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")

	a.FailNext("POST /v1/vms/" + id + "/snapshot")
	path := "/api/v1/nodes/" + a.ID + "/evacuate"
	if r := env.Do(key, "POST", path, map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	if r := env.Do(key, "DELETE", path, nil); r.Status != 409 {
		t.Fatalf("cancel after the deadline: %s", r)
	}
	env.Eventually("stop refused", func() bool { return noticeCount(t, env, id, "stop_failing") == 1 })

	// The retry saves the disk; no other node: stopped, owner told.
	r := env.WaitState(key, id, "archived")
	if r.Str("sandbox.nodeId") != a.ID {
		t.Fatalf("stranded sandbox: %s", r)
	}
	env.Eventually("stranded notice", func() bool { return noticeCount(t, env, id, "evacuation_stranded") == 1 })
	time.Sleep(200 * time.Millisecond)
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "archived" {
		t.Fatalf("stranded sandbox restarted: %s", r)
	}
}

// Other nodes exist but stay full: after CapacityWait the evacuated sandbox
// goes back to stopped (its disk is saved), not to error.
func TestEvacuateNoRoomStaysStopped(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.CapacityWait = 300 * time.Millisecond })
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	b := env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 16, MemoryMiB: 65536, DiskGiB: 1000, MaxSandboxes: 1}})
	if r := env.Do(key, "PATCH", "/api/v1/nodes/"+a.ID, map[string]any{"disabled": true}); r.Status != 200 {
		t.Fatalf("drain: %s", r)
	}
	other := createSandbox(t, env, key, nil)
	if r := env.WaitState(key, other, "ready"); r.Str("sandbox.nodeId") != b.ID {
		t.Fatalf("filler: %s", r)
	}

	if r := env.Do(key, "POST", "/api/v1/nodes/"+a.ID+"/evacuate", map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	env.Eventually("stranded notice", func() bool { return noticeCount(t, env, id, "evacuation_stranded") == 1 })
	if r := env.WaitState(key, id, "archived"); r.Get("sandbox.error") != nil {
		t.Fatalf("stranded sandbox: %s", r)
	}
}

func nodeEvacuation(env *servertest.Env, key, nodeID string) any {
	return env.Do(key, "GET", "/api/v1/nodes/"+nodeID, nil).Get("node.evacuation")
}

// The stop's layer uploads in the background: until it is on the server the
// evacuation goes on (the node page keeps saying so), and removing the node
// is refused unless forced.
func TestEvacuateWaitsForUploads(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	a.HoldUploads()
	if r := env.Do(key, "POST", "/api/v1/nodes/"+a.ID+"/evacuate", map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	env.WaitState(key, id, "archived")
	sb, err := env.S.SandboxByID(id)
	if err != nil || sb.HeadSnapshotID == "" {
		t.Fatalf("no head snapshot: %v", err)
	}
	head := sb.HeadSnapshotID
	env.Eventually("upload asked for", func() bool {
		for _, s := range a.UploadRequests() {
			if s == head {
				return true
			}
		}
		return false
	})
	if nodeEvacuation(env, key, a.ID) == nil {
		t.Fatal("evacuation ended while the only copy of a layer was on the node")
	}
	env.Eventually("evacuation in the heartbeat", func() bool { return a.LastHeartbeatResponse().Evacuation != nil })
	r := env.Do(key, "DELETE", "/api/v1/nodes/"+a.ID, nil)
	if r.Status != 409 || r.Code() != "node_holds_snapshots" {
		t.Fatalf("remove with an un-uploaded layer: %s", r)
	}

	a.ReleaseUploads()
	env.Eventually("evacuation ends after the upload", func() bool { return nodeEvacuation(env, key, a.ID) == nil })
	if r := env.Do(key, "DELETE", "/api/v1/nodes/"+a.ID, nil); r.Status != 200 {
		t.Fatalf("remove: %s", r)
	}
}

// A node that goes offline during the grace period does not end the
// evacuation: its sandboxes are still there.
func TestEvacuateNodeOffline(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/nodes/"+a.ID+"/evacuate", map[string]any{"graceSec": 3600}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	a.Disconnect()
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.error", servertest.WaitTimeout, "node_offline")
	time.Sleep(200 * time.Millisecond) // a few reaper passes
	if nodeEvacuation(env, key, a.ID) == nil {
		t.Fatal("the evacuation ended while the node was offline")
	}
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	env.WaitState(key, id, "ready")
	if nodeEvacuation(env, key, a.ID) == nil {
		t.Fatal("no evacuation after the node came back")
	}
}

// A stop the owner asked for stays a stop: not turned into a move.
func TestEvacuateKeepsUserStop(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{})
	refused := createSandbox(t, env, key, nil)
	during := createSandbox(t, env, key, nil)
	env.WaitState(key, refused, "ready")
	env.WaitState(key, during, "ready")
	b := env.StartNode(servertest.NodeOptions{})

	// The owner's stop is refused (save failed) and retried; the evacuation
	// starts meanwhile and leaves it alone.
	a.FailNext("POST /v1/vms/" + refused + "/snapshot")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+refused+"/stop", nil); r.Status/100 != 2 {
		t.Fatalf("stop: %s", r)
	}
	env.Eventually("stop refused", func() bool { return noticeCount(t, env, refused, "stop_failing") == 1 })
	// The owner stops one whose evacuation stop is under way.
	if err := env.S.SetSandboxExtra(during, "evacuateFrom", a.ID); err != nil {
		t.Fatal(err)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+during+"/stop", nil); r.Status/100 != 2 {
		t.Fatalf("stop: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/nodes/"+a.ID+"/evacuate", map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	env.WaitState(key, refused, "archived")
	env.WaitState(key, during, "archived")
	env.Eventually("evacuation ends", func() bool { return nodeEvacuation(env, key, a.ID) == nil })
	time.Sleep(200 * time.Millisecond)
	for _, id := range []string{refused, during} {
		if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "archived" {
			t.Fatalf("user-stopped sandbox moved: %s", r)
		}
	}
	if b.Called("POST /v1/vms") {
		t.Fatal("a user-stopped sandbox was booted on the other node")
	}
}

// Undraining an evacuating node is refused until the evacuation is
// cancelled.
func TestEvacuateUndrainRefused(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	path := "/api/v1/nodes/" + a.ID
	if r := env.Do(key, "POST", path+"/evacuate", map[string]any{"graceSec": 3600}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	if r := env.Do(key, "PATCH", path, map[string]any{"disabled": false, "name": "renamed"}); r.Status != 409 || r.Code() != "node_evacuating" {
		t.Fatalf("undrain while evacuating: %s", r)
	}
	if r := env.Do(key, "GET", path, nil); r.Str("node.name") == "renamed" {
		t.Fatal("a refused update was partly applied")
	}
	if r := env.Do(key, "DELETE", path+"/evacuate", nil); r.Status != 200 {
		t.Fatalf("cancel: %s", r)
	}
	if r := env.Do(key, "PATCH", path, map[string]any{"disabled": false}); r.Status != 200 || r.Str("node.disabled") != "false" {
		t.Fatalf("undrain after cancel: %s", r)
	}
}
