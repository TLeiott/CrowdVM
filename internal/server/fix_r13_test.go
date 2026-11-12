package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r13-sonnet (code only): agent claim codes are six digits and a
// signed-in user could guess them without limit, claiming other people's
// agents into their own account. Wrong codes are now capped per user.
func TestR13WrongDeviceCodesAreCappedPerUser(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("guesser", false)
	sess := env.Session(u)
	id := env.Do("", "POST", "/agent/identity", map[string]any{"method": "anonymous", "name": "victimbot"})
	code := env.Do(id.Str("access_token"), "POST", "/agent/identity/claim", nil).Str("user_code")
	if code == "" {
		t.Fatal("no claim code")
	}
	wrong := "000000"
	if wrong == code {
		wrong = "000001"
	}
	for i := 0; i < 20; i++ {
		if r := env.Do(sess, "POST", "/auth/device/approve", map[string]any{"userCode": wrong, "approve": true}); r.Status != 404 {
			t.Fatalf("guess %d: %s", i, r)
		}
	}
	if r := env.Do(sess, "POST", "/auth/device/approve", map[string]any{"userCode": code, "approve": true}); r.Status != 429 {
		t.Fatalf("after 20 wrong codes the right one still went through: %s", r)
	}
	if r := env.Do(sess, "GET", "/auth/device/lookup?code="+code, nil); r.Status != 429 {
		t.Fatalf("lookup after 20 wrong codes: %s", r)
	}
}

// bugs-r13-opus 3: the evacuated node died during the evacuation's stop.
// The forced stop fell back to the last stored snapshot and the move booted
// from it, although the node held a newer snapshot it uploaded on return.
func TestR13EvacMoveWaitsForNewerSnapshotOfDeadNode(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = 300 * time.Millisecond })
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	b := env.StartNode(servertest.NodeOptions{Name: "b"})
	a.HoldUploads()
	snap, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := env.S.SandboxByID(id)
	t.Logf("head after the unstored snapshot: %q (snapshot %s)", sb.HeadSnapshotID, snap)
	a.DieOnNext("POST /v1/vms/" + id)
	if r := env.Do(key, "POST", "/api/v1/nodes/"+a.ID+"/evacuate", map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	// Settled: the forced stop parked the move for the newer snapshot; or
	// the node-offline handling came first (error node_offline, the stop
	// waits for the node); or, without the fix, the move booted on b.
	env.Eventually("the stop gave up on the dead node", func() bool {
		sb, err := env.S.SandboxByID(id)
		return err == nil && (sb.State == "error" && (strings.HasPrefix(sb.Error, "snapshot_unavailable") || sb.Error == "node_offline") || sb.NodeID == b.ID)
	})
	time.Sleep(500 * time.Millisecond)
	if sb, _ := env.S.SandboxByID(id); sb.NodeID == b.ID {
		t.Fatalf("the move booted on b from %q while the newer snapshot %s was still on the dead node (state %s)", sb.HeadSnapshotID, snap, sb.State)
	}
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	a.ReleaseUploads()
	// When the node came back before the forced stop, its stop finished
	// normally with a snapshot of its own.
	env.Eventually("booted on b from the newer snapshot", func() bool {
		sb, err := env.S.SandboxByID(id)
		if err != nil || sb.NodeID != b.ID || sb.State != "ready" {
			return false
		}
		// The newer snapshot itself, or one taken after it.
		var newer bool
		_ = env.S.DB.QueryRow(`SELECT h.created_at>=s.created_at FROM snapshots h, snapshots s WHERE h.id=? AND s.id=?`, sb.HeadSnapshotID, snap).Scan(&newer)
		return newer
	})
}

// bugs-r13-opus 11: a node paused by its owner refuses boots before its
// next heartbeat tells the server; a failFast create failed although
// another node had room.
func TestR13FailFastTriesAnotherNodeAfterRefusal(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a", Bench: &proto.Benchmark{Tier: "gold", Score: 5000}})
	b := env.StartNode(servertest.NodeOptions{Name: "b"})
	a.RefuseNext("POST /v1/vms", 503, map[string]any{"code": "out_of_capacity", "message": "node is paused by its owner"})
	r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"failFast": true})
	if r.Status/100 != 2 {
		t.Fatalf("create: %s", r)
	}
	id := r.Str("sandbox.id")
	env.WaitState(key, id, "ready")
	if sb, _ := env.S.SandboxByID(id); sb.NodeID != b.ID {
		t.Fatalf("placed on %s, want b", sb.NodeID)
	}
	if !a.Called("POST /v1/vms") {
		t.Fatal("a was not asked first: the test proves nothing")
	}
}

// bugs-r13-opus 10: a sandbox an evacuation moved, still waiting for room,
// kept avoiding its old node after the evacuation ended.
func TestR13EvacuatedBootMayReturnAfterEvacuationEnds(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	// b is full: the move waits for room.
	b := env.StartNode(servertest.NodeOptions{Name: "b", Capacity: proto.Capacity{VCPU: 16, MemoryMiB: 65536, DiskGiB: 1000, MaxSandboxes: 1}})
	filler := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": b.ID}})
	env.WaitState(key, filler, "ready")
	if r := env.Do(key, "POST", "/api/v1/nodes/"+a.ID+"/evacuate", map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	env.Eventually("stopped and waiting for room", func() bool {
		sb, err := env.S.SandboxByID(id)
		return err == nil && sb.Extra["pendingBoot"] != nil
	})
	env.Eventually("evacuation over", func() bool { return nodeEvacuation(env, key, a.ID) == nil })
	if r := env.Do(key, "PATCH", "/api/v1/nodes/"+a.ID, map[string]any{"disabled": false}); r.Status/100 != 2 {
		t.Fatalf("undrain: %s", r)
	}
	env.WaitState(key, id, "ready")
	if sb, _ := env.S.SandboxByID(id); sb.NodeID != a.ID {
		t.Fatalf("on %s, want back on a", sb.NodeID)
	}
}
