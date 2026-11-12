package server_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r12-opus 4: a move's boot that failed (snapshot_unavailable) while the
// evacuated node was away was never retried: the evacuation ended once the
// node uploaded its layers, and the sandbox stayed in error.
func TestR12FailedEvacMoveBootsAfterUpload(t *testing.T) {
	old := server.HolderOfflineGrace
	server.HolderOfflineGrace = 300 * time.Millisecond
	t.Cleanup(func() { server.HolderOfflineGrace = old })
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.StartNode(servertest.NodeOptions{Name: "b"})
	a.HoldUploads()
	if r := env.Do(key, "POST", "/api/v1/nodes/"+a.ID+"/evacuate", map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	env.Eventually("the move is booting on b", func() bool {
		sb, err := env.S.SandboxByID(id)
		return err == nil && sb.NodeID != "" && sb.NodeID != a.ID
	})
	a.Disconnect() // the evacuated node dies with the only copy of the layer
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", servertest.WaitTimeout, "error")
	if err := a.Connect(); err != nil {
		t.Fatal(err)
	}
	a.ReleaseUploads()
	env.Eventually("evacuation ends", func() bool { return nodeEvacuation(env, key, a.ID) == nil })
	env.WaitState(key, id, "ready")
}

// bugs-r12-opus 5: limits lowered below what runs showed negative free
// capacity.
func TestR12FreeCapacityNeverNegative(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Name: "n1"})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	hb, _ := json.Marshal(proto.Heartbeat{Info: &proto.NodeInfo{Name: "n1", Capacity: proto.Capacity{VCPU: 1, MemoryMiB: 512, DiskGiB: 1, MaxSandboxes: 0}}})
	if code, body := r11NodeReq(t, env, node.Token, "POST", "/node/v1/heartbeat", hb); code != 200 {
		t.Fatalf("heartbeat: %d %s", code, body)
	}
	time.Sleep(50 * time.Millisecond)
	r := env.Do(key, "GET", "/api/v1/nodes/"+node.ID, nil)
	for _, f := range []string{"vcpu", "memoryMiB", "diskGiB", "maxSandboxes"} {
		if v, _ := r.Get("node.free." + f).(float64); v < 0 {
			t.Fatalf("free.%s = %v: %s", f, v, r)
		}
	}
}
