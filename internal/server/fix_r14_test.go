package server_test

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r14-sonnet 3: a retry is only accepted with the nonce of the first
// redemption; the node, which knows the ticket but not the nonce (inside
// TLS), still gets ticket_used.
func TestR14TicketRetryNeedsTheFirstNonce(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	spec, _ := node.Spec(id)
	ticket := spec.Guest.Channel.Ticket
	if _, w := rawChannelTryNonce(t, env, id, ticket, "guest-nonce"); w.Error != "" {
		t.Fatalf("first redemption: %s", w.Error)
	}
	for _, n := range []string{"", "node-guess"} {
		if _, w := rawChannelTryNonce(t, env, id, ticket, n); w.Error != "ticket_used" {
			t.Fatalf("redemption with nonce %q: %q, want ticket_used", n, w.Error)
		}
	}
	if _, w := rawChannelTryNonce(t, env, id, ticket, "guest-nonce"); w.Error != "" {
		t.Fatalf("the guest's own retry: %q", w.Error)
	}
}

// bugs-r14-opus 4: after a server restart, rebuildLost counted the server's
// own downtime as the node's: a node that dropped just before the restart
// had its sandboxes rebuilt elsewhere (and its newer disk dropped) before it
// could reconnect.
func TestR14RebuildLostWaitsAfterServerStart(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = 5 * time.Second })
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if _, err := a.PeriodicSnapshot(id); err != nil {
		t.Fatal(err)
	}
	env.Eventually("head stored", func() bool {
		sb, _ := env.S.SandboxByID(id)
		return sb.HeadSnapshotID != ""
	})
	env.StartNode(servertest.NodeOptions{Name: "b"})
	a.Disconnect()
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", servertest.WaitTimeout, "error")
	// It went offline long ago by the clock, but this server only just
	// started: the node never had the grace to come back.
	if _, err := env.S.DB.Exec(`UPDATE sandboxes SET degraded_since=?, updated_at=? WHERE id=?`, time.Now().Add(-time.Hour).UnixMilli(), time.Now().Add(-time.Hour).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if sb, _ := env.S.SandboxByID(id); sb.NodeID != a.ID || sb.State != "error" {
		t.Fatalf("rebuilt within the first second of the server (state %s on %s)", sb.State, sb.NodeID)
	}
}

// bugs-r14-opus 5: a resume after the machine failed booted the older head
// while its node still uploaded a newer snapshot.
func TestR14ResumeAfterFailureUsesNewerSnapshot(t *testing.T) {
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
	newer, err := a.PeriodicSnapshot(id) // still uploading
	if err != nil {
		t.Fatal(err)
	}
	a.SetVMState(id, "error", false, "the machine crashed")
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", servertest.WaitTimeout, "error")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status/100 != 2 {
		t.Fatalf("resume: %s", r)
	}
	if sb, _ := env.S.SandboxByID(id); sb.HeadSnapshotID != newer {
		t.Fatalf("resume booted from %s, want the newer %s (older %s)", sb.HeadSnapshotID, newer, old)
	}
	a.ReleaseUploads()
	env.WaitState(key, id, "ready")
}
