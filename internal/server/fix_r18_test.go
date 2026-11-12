package server_test

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r18-opus 4: a deploy of a saved snapshot that waited for capacity did
// not keep that snapshot's layer alive: removing the saved snapshot (and
// its sandbox) purged it, and the deploy failed once placed.
func TestR18QueuedDeployKeepsItsParent(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 16, MemoryMiB: 65536, DiskGiB: 1000, MaxSandboxes: 1}})
	src := createSandbox(t, env, key, nil)
	env.WaitState(key, src, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": src, "name": "qd"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/qd", "snapshot.status", 5*time.Second, "ready")
	// The node is full: the deploy waits for capacity.
	cp := createSandbox(t, env, key, map[string]any{"from": "qd"})
	env.Eventually("deploy queued", func() bool {
		sb, err := env.S.SandboxByID(cp)
		return err == nil && sb.Extra["pendingBoot"] != nil
	})
	if r := env.Do(key, "DELETE", "/api/v1/saved-snapshots/qd", nil); r.Status/100 != 2 {
		t.Fatalf("remove saved: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/sandboxes/"+src, nil, "X-CrowdVM-Confirm-Delete: "+src); r.Status/100 != 2 {
		t.Fatalf("delete source: %s", r)
	}
	env.WaitState(key, cp, "ready", "error")
	if sb, _ := env.S.SandboxByID(cp); sb.State != "ready" {
		t.Fatalf("the queued deploy lost its parent: %s %s", sb.State, sb.Error)
	}
}
