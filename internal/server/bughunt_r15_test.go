package server_test

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// A resume that is refused must not change the sandbox. handleResume (from
// error) moves head_snapshot_id to the newest snapshot its node holds (a
// layer not stored on the server yet) before any of its checks run, so a
// refused request (limits, a placement nothing meets, type_too_small, an
// unreachable chain) leaves the head on data that exists only on the failed
// node, in place of the stored layer a later resume or a rebuild would use.
func TestBughuntR15RefusedResumeMovesTheHead(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	old, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	env.Eventually("first snapshot is the head", func() bool {
		sb, _ := env.S.SandboxByID(id)
		return sb.HeadSnapshotID == old
	})
	a.HoldUploads()
	newer, err := a.PeriodicSnapshot(id) // still uploading: not stored on the server
	if err != nil {
		t.Fatal(err)
	}
	a.SetVMState(id, "error", false, "the machine crashed")
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", servertest.WaitTimeout, "error")
	// A placement no node meets: the resume is refused.
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"placement": map[string]any{"node": "no-such-node"}})
	if r.Status/100 == 2 {
		t.Fatalf("resume was accepted: %s", r)
	}
	sb, _ := env.S.SandboxByID(id)
	if sb.HeadSnapshotID != old {
		t.Fatalf("a refused resume (%d) moved the head to %s (stored: %s); unstored newer: %s", r.Status, sb.HeadSnapshotID, old, newer)
	}
	a.ReleaseUploads()
}

// handleGuestReboot gives a ticket only to a sandbox in a usable state
// (r14's fix for the stale announcement). The recover the owner is told to
// run after an unannounced reboot works on a sandbox in state "error": the
// server expects the node's request (handleRecover calls expectReboot), asks
// the node to restart the VM, and the node's driver asks for the new ticket
// inside that restart (qemu.Driver.Restart -> reticket), while the sandbox is
// still in "error". The new state check answers 404 not_running, which the
// node does not retry: the restart fails and the sandbox cannot be recovered.
func TestBughuntR15RecoverFromErrorGetsNoTicket(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	// The machine went down without announcing it (a crash).
	node.SetVMState(id, "error", false, "new boot ticket: unannounced_reboot")
	env.WaitState(key, id, "error")
	// The owner recovers it. The restart itself is made to fail here, which
	// leaves exactly the state the node's reticket call runs in: sandbox in
	// "error", reboot expected by the server.
	node.FailNext("POST /v1/vms/")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/recover", nil)
	if sb, _ := env.S.SandboxByID(id); sb.State != "error" {
		t.Fatalf("state %s, the test needs error", sb.State)
	}
	if code := nodeReboot(t, env, node, id); code != 200 {
		t.Fatalf("the node's re-ticket request during a recover of a sandbox in error: %d, want 200 (a 404 is final for the node: the recover fails)", code)
	}
}
