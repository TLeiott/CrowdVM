package server_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// postNodeEvent sends one event to the server as the given node (Bearer node
// token on the main listener, like the node's HTTP fallback).
func r8NodeEvent(t *testing.T, env *servertest.Env, n *servertest.FakeNode, ev proto.Event) {
	t.Helper()
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	r := env.Do(n.Token, "POST", "/node/v1/events", []proto.Event{ev})
	if r.Status != 200 {
		t.Fatalf("node event: %s", r)
	}
}

func r8Other(a, b *servertest.FakeNode, hostID string) *servertest.FakeNode {
	if hostID == b.ID {
		return a
	}
	return b
}

// reconcileVM records the VM start time a node reports (vmStarts) before it
// checks that the node hosts the sandbox. A foreign node (or an orphan
// machine on a node whose clock runs ahead) reporting a far-future StartedAt
// poisons it: every later "stopped"/"error" report of the real machine counts
// as "about an older machine" and is dropped, so a dead VM stays "ready".
func TestBughuntR8ForeignVMStartTimePoisonsStopReports(t *testing.T) {
	env := servertest.New(t)
	a := env.StartNode(servertest.NodeOptions{Name: "r8a"})
	b := env.StartNode(servertest.NodeOptions{Name: "r8b"})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	r := env.WaitState(key, id, "ready")
	host := a
	if r.Str("sandbox.nodeId") == b.ID {
		host = b
	}
	other := r8Other(a, b, host.ID)
	st := proto.VMStatus{SandboxID: id, State: proto.VMRunning, GuestReady: true, StartedAt: time.Now().Add(24 * time.Hour).UTC()}
	d, _ := json.Marshal(st)
	r8NodeEvent(t, env, other, proto.Event{SandboxID: id, Type: proto.EvVMState, Data: d})
	host.SetVMState(id, proto.VMError, false, "qemu died")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Str("sandbox.state") == "error" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("host node reported the VM as errored, but the sandbox is still %q: a foreign node's report poisoned vmStarts",
		env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Str("sandbox.state"))
}

// snapshot.failed from a node that does not host the sandbox: the handler
// updates sandboxes.last_snapshot_status by id only (the guest-event fix of
// round 7 added the node check to guest events only).
func TestBughuntR8ForeignSnapshotFailedMarksSandbox(t *testing.T) {
	env := servertest.New(t)
	a := env.StartNode(servertest.NodeOptions{Name: "r8a"})
	b := env.StartNode(servertest.NodeOptions{Name: "r8b"})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	r := env.WaitState(key, id, "ready")
	host := a
	if r.Str("sandbox.nodeId") == b.ID {
		host = b
	}
	other := r8Other(a, b, host.ID)
	d, _ := json.Marshal(map[string]any{"snapshotId": "snap_nope", "error": "x"})
	r8NodeEvent(t, env, other, proto.Event{SandboxID: id, Type: proto.EvSnapshotFailed, Data: d})
	var status string
	env.S.DB.QueryRow(`SELECT COALESCE(last_snapshot_status,'') FROM sandboxes WHERE id=?`, id).Scan(&status)
	if status == "failed" {
		t.Fatalf("node %s (not hosting %s) set its last_snapshot_status to failed", other.ID, id)
	}
}

// A CI link billed to an organization (ci_links.org_id) stays active, with
// its sealed GitHub token, after the organization is deleted: the delete
// cascade of round 6/7 covers environments, git sources and credential
// profiles but not the org's CI links. The poller keeps listing the link's
// jobs and every job then fails at "create sandbox" in a deleted org.
func TestBughuntR8OrgDeleteLeavesActiveCILink(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	orgID := makeOrg(t, env, key, "R8CI")
	var owner string
	env.S.DB.QueryRow(`SELECT id FROM users WHERE username='admin' OR is_admin=1 LIMIT 1`).Scan(&owner)
	if _, err := env.S.DB.Exec(`INSERT INTO ci_links(id,owner_id,org_id,api_url,login,account_type,token,token_user,status,created_at,confirmed_at,provider)
		VALUES('cil_r8',?,?,'https://api.github.com','r8org','Organization','sealed-token','r8',  'active',1,1,'github')`, owner, orgID); err != nil {
		t.Fatal(err)
	}
	if r := env.Do(key, "DELETE", "/api/v1/orgs/r8ci", nil); r.Status != 200 {
		t.Fatalf("delete org: %s", r)
	}
	var status, token string
	err := env.S.DB.QueryRow(`SELECT status, token FROM ci_links WHERE id='cil_r8'`).Scan(&status, &token)
	if err == nil && (status == "active" || token != "") {
		t.Fatalf("deleted org still has a CI link: status=%q, token stored=%v", status, token != "")
	}
}

// The 200-key cap of round 7 counts every non-revoked api_keys row of the
// user, including the neutral per-sandbox token (preset sandbox-token) the
// server mints for every sandbox and revokes only when the sandbox is
// deleted. A user with 200 stopped (archived) sandboxes has 200 such rows and
// can no longer create a single API key that they made themselves.
func TestBughuntR8KeyCapCountsSandboxTokens(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	_, bob := env.NewUser("r8bob", false)
	for i := 0; i < 200; i++ {
		id := createSandbox(t, env, bob, nil)
		env.WaitState(bob, id, "ready")
		env.Do(bob, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
		env.WaitState(bob, id, "archived")
	}
	_ = key
	r := env.Do(bob, "POST", "/api/v1/api-keys", map[string]any{"name": "mine", "preset": "read-only"})
	if r.Status == 409 {
		t.Fatalf("user with 200 stopped sandboxes and no keys of their own cannot create an API key: %s", r)
	}
}
