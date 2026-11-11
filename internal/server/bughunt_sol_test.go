package server_test

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// A fallback followed by a new boot abandons the old disk branch even when
// the next machine runs on the same node.
func TestBughuntSolLateUploadAfterResumeOnSameNode(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	root := storedLayer(t, env, node, id)
	node.HoldUploads()
	stale, err := node.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	node.Disconnect()
	env.WaitState(key, id, "error")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", map[string]any{"force": true}); r.Status/100 != 2 {
		t.Fatal(r)
	}
	env.WaitState(key, id, "archived")
	node.DropVM(id)
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status != 202 {
		t.Fatal(r)
	}
	env.WaitState(key, id, "ready")
	spec, ok := node.Spec(id)
	if !ok || spec.ParentID != root {
		t.Fatalf("setup: resumed from %q, want %q", spec.ParentID, root)
	}
	stored := make(chan struct{}, 1)
	unsubscribe := env.S.Bus.Subscribe(func(e server.Event) {
		if e.Type == server.EvSnapshotStored && e.Data["snapshotId"] == stale {
			select {
			case stored <- struct{}{}:
			default:
			}
		}
	})
	defer unsubscribe()
	node.ReleaseUploads()
	select {
	case <-stored:
	case <-time.After(5 * time.Second):
		t.Fatal("old layer never completed storage")
	}
	if head := headOf(env, id); head != root {
		t.Fatalf("late upload changed current disk head to abandoned layer %q, want running machine's parent %q", head, root)
	}
}

// Without any stored head, removing the only holder makes pending data
// permanently lost. Retrying the resume cannot make that node return.
func TestBughuntSolNoHeadRemovedHolderIsFinal(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	node.HoldUploads()
	if _, err := node.PeriodicSnapshot(id); err != nil {
		t.Fatal(err)
	}
	node.Disconnect()
	env.WaitState(key, id, "error")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", map[string]any{"force": true}); r.Status/100 != 2 {
		t.Fatal(r)
	}
	env.WaitState(key, id, "archived")
	if head := headOf(env, id); head != "" {
		t.Fatalf("setup: unexpected stored head %q", head)
	}
	if r := env.Do(key, "DELETE", "/api/v1/nodes/"+node.ID+"?force=true", nil); r.Status != 200 {
		t.Fatal(r)
	}
	for _, action := range []string{"resume", "fork"} {
		r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/"+action, nil)
		if r.Status != 409 || r.Get("error.details.retryable") != false {
			t.Errorf("%s after removing the only holder should report permanent snapshot loss, got %s", action, r)
		}
	}
}
