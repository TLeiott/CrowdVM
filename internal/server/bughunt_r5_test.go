package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// The round-4 fix for stale "Sandbox failed" notices clears only kind
// sandbox_error. The follow-up warnings of the same error hold
// (forced_stop_warning) stay after the sandbox recovers or is deleted.
func TestBughuntR5HoldWarningNoticeSurvivesRecovery(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	for _, deleted := range []bool{false, true} {
		id := createSandbox(t, env, env.AdminKey, nil)
		env.WaitState(env.AdminKey, id, "ready")
		old := time.Now().Add(-50 * time.Hour).UnixMilli() // 2 days in error: the 1-day warning is due
		env.S.DB.Exec(`UPDATE sandboxes SET state='error',error='boom',extra=json_set(extra,'$.errorSince',?) WHERE id=?`, old, id)
		env.S.Reap()
		count := func() int {
			var n int
			env.S.DB.QueryRow(`SELECT COUNT(*) FROM notices WHERE sandbox_id=? AND kind='forced_stop_warning'`, id).Scan(&n)
			return n
		}
		if count() != 1 {
			t.Fatalf("setup: expected the warning notice, got %d", count())
		}
		event := server.Event{Type: server.EvSandboxState, SandboxID: id, PreviousState: "error", State: "ready"}
		if deleted {
			env.S.DB.Exec(`UPDATE sandboxes SET deleted_at=1 WHERE id=?`, id)
			event.Type = server.EvSandboxDeleted
		} else {
			env.S.DB.Exec(`UPDATE sandboxes SET state='ready',error=NULL WHERE id=?`, id)
		}
		env.S.Bus.Publish(event)
		time.Sleep(300 * time.Millisecond)
		if n := count(); n != 0 {
			t.Fatalf("deleted=%v: \"Sandbox will be stopped in 1 day\" notice still present after the sandbox recovered/was deleted (%d)", deleted, n)
		}
	}
}

// Deleting an organization leaves its saved snapshots behind: they are
// owned by the org id (saved_snapshots.owner_id = org_id), so after the org is
// gone nobody can list, deploy or delete them, and their disk layers stay pinned.
func TestBughuntR5OrgDeleteOrphansSavedSnapshots(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	r := env.Do(key, "POST", "/api/v1/orgs", map[string]any{"name": "r5gone"})
	if r.Status != 201 {
		t.Fatal(r)
	}
	orgID := r.Str("org.id")
	id := createSandbox(t, env, key, map[string]any{"org": "r5gone"})
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots?org=r5gone", map[string]any{"sandboxId": id, "name": "img"}); r.Status != 202 {
		t.Fatal(r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/img?org=r5gone", "snapshot.status", 5*time.Second, "ready")
	stopUntilArchived(t, env, key, id)
	if r := env.Do(key, "DELETE", "/api/v1/orgs/r5gone", nil); r.Status != 200 {
		t.Fatalf("delete org: %s", r)
	}
	var n int
	env.S.DB.QueryRow(`SELECT COUNT(*) FROM saved_snapshots WHERE owner_id=? OR org_id=?`, orgID, orgID).Scan(&n)
	if n != 0 {
		var visible int
		if r := env.Do(key, "GET", "/api/v1/saved-snapshots", nil); r.Status == 200 {
			visible = len(r.Get("snapshots").([]any))
		}
		t.Fatalf("%d saved snapshot(s) still owned by the deleted org %s; visible to its former owner: %d", n, orgID, visible)
	}
}

// A request that passed the access check but waits for a starting sandbox is
// not tracked yet, so hiding the port meanwhile (cut) cannot close it: the
// websocket opens on a port that is no longer hosted.
func TestBughuntR5HideDuringStartWaitDoesNotCutWebsocket(t *testing.T) {
	env := servertest.New(t)
	app := hostedApp(t)
	node := env.StartNode(servertest.NodeOptions{Tunnel: appTunnel(app)})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000, "public": true}); r.Status != 200 {
		t.Fatal(r)
	}
	node.SetVMState(id, proto.VMStarting, false, "") // the guest is not up yet: the proxy waits
	env.S.DB.Exec(`UPDATE sandboxes SET state='provisioned' WHERE id=?`, id)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	type res struct {
		ws  *websocket.Conn
		err error
	}
	out := make(chan res, 1)
	go func() {
		ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(env.URL, "http")+"/p/"+id+"/3000/ws", nil)
		out <- res{ws, err}
	}()
	time.Sleep(400 * time.Millisecond)
	if r := env.Do(key, "DELETE", "/api/v1/sandboxes/"+id+"/host/3000", nil); r.Status != 200 {
		t.Fatalf("hide: %s", r)
	}
	node.SetVMState(id, proto.VMRunning, true, "")
	env.S.DB.Exec(`UPDATE sandboxes SET state='ready' WHERE id=?`, id)
	r := <-out
	if r.err != nil {
		return // refused: correct
	}
	defer r.ws.CloseNow()
	_ = r.ws.Write(ctx, websocket.MessageText, []byte("hi"))
	if _, b, err := r.ws.Read(ctx); err == nil {
		t.Fatalf("websocket to a hidden port was opened and served: %q", b)
	}
}

// Usage of an organization sandbox that is not billed yet when the org is
// deleted is charged to the deleted org's wallet afterwards: the delete moves
// the sandboxes to personal billing but leaves sandbox_runs.wallet_id on the
// org, so nobody pays for that tail and the ledger gets a wallet with no org.
func TestBughuntR5OrgDeleteLeavesUnbilledRunOnDeletedWallet(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	r := env.Do(key, "POST", "/api/v1/orgs", map[string]any{"name": "r5bill"})
	if r.Status != 201 {
		t.Fatal(r)
	}
	orgID := r.Str("org.id")
	id := createSandbox(t, env, key, map[string]any{"org": "r5bill"})
	env.WaitState(key, id, "ready")
	time.Sleep(1200 * time.Millisecond) // some metered time
	stopUntilArchived(t, env, key, id)  // closes the run; billing has not run yet
	if r := env.Do(key, "DELETE", "/api/v1/orgs/r5bill", nil); r.Status != 200 {
		t.Fatalf("delete org: %s", r)
	}
	env.S.BillNow()
	var n int
	env.S.DB.QueryRow(`SELECT COUNT(*) FROM credit_ledger WHERE wallet_id=? AND kind='usage'`, orgID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d usage ledger row(s) charged to the deleted org wallet %s after its deletion (the creator's personal wallet was not charged)", n, orgID)
	}
}
