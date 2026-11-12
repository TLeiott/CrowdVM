package server_test

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// sshKeyRows counts the rows the sshkey guard recorded for a sandbox id.
func sshKeyRows(t *testing.T, env *servertest.Env, sandboxID string) int {
	t.Helper()
	var n int
	if err := env.S.DB.QueryRow(`SELECT COUNT(*) FROM sandbox_ssh_keys WHERE sandbox_id=?`, sandboxID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// bugs-r13-sonnet 2: sshKeyGuard (acct_sshkeys.go:48) runs in Hooks.Guard,
// which wrap() calls after the key's scope check but before the handler's
// sandbox access check. It records (sandbox, user, key) for every POST to
// /sandboxes/{id}/sshkey, whatever the answer: a user with no access to the
// sandbox (404), a viewer (403) and a made-up id all leave a row behind. The
// rows of ids that never exist are never cleaned (only EvSandboxDeleted
// deletes them), so the table grows without bound, and the rows of a real
// sandbox name the outsider as the sender of a key (see the next test).
func TestBughuntR13SSHKeyRecordedForCallerWithoutAccess(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	id := createSandbox(t, env, env.AdminKey, map[string]any{"noEnv": true})
	env.WaitState(env.AdminKey, id, "ready")
	_, outsider := env.NewUser("mallory", false)
	const k = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMalloryMalloryMalloryMalloryMallory1 m"

	if r := env.Do(outsider, "POST", "/api/v1/sandboxes/"+id+"/sshkey", map[string]any{"key": k}); r.Status != 404 {
		t.Fatalf("outsider sshkey: %s", r)
	}
	if n := sshKeyRows(t, env, id); n != 0 {
		t.Fatalf("a refused (404) sshkey call left %d row(s) for someone else's sandbox in sandbox_ssh_keys", n)
	}
	if r := env.Do(outsider, "POST", "/api/v1/sandboxes/sbx_doesnotexist/sshkey", map[string]any{"key": k}); r.Status != 404 {
		t.Fatalf("unknown sandbox sshkey: %s", r)
	}
	if n := sshKeyRows(t, env, "sbx_doesnotexist"); n != 0 {
		t.Fatalf("a call for a sandbox that does not exist left %d row(s) that nothing ever deletes", n)
	}
}

// bugs-r13-sonnet 3: the guard inserts with ON CONFLICT(sandbox_id,key) DO
// NOTHING, so the first account that ever sends a key line owns it. A
// teammate who sends the creator's (public) key line first, or the same key
// shared by both, makes the creator's own key count as "teammate's": on
// unshare revokeTeammateKeys removes it from authorized_keys (the "a key the
// creator also sent stays" rule never applies because the creator's row was
// never written) and the creator loses ssh access with their own key.
func TestBughuntR13SSHKeySentFirstByTeammateIsRemovedFromCreatorOnUnshare(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	alice := env.AdminKey
	_, bob := env.NewUser("bob", false)
	env.Do(alice, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	code := env.Do(alice, "POST", "/api/v1/orgs/acme/invites", map[string]any{"email": "bob@example.test"}).Str("code")
	env.Do(bob, "POST", "/api/v1/org-invites/accept", map[string]any{"code": code})
	sb := createSandbox(t, env, alice, map[string]any{"org": "acme", "noEnv": true, "visibility": "shared"})
	env.WaitState(alice, sb, "ready")
	const shared = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAISharedSharedSharedSharedSharedShared1"
	env.Do(bob, "POST", "/api/v1/sandboxes/"+sb+"/sshkey", map[string]any{"key": shared + " bob"})
	env.Do(alice, "POST", "/api/v1/sandboxes/"+sb+"/sshkey", map[string]any{"key": shared + " alice"})
	if r := env.Do(alice, "PUT", "/api/v1/sandboxes/"+sb+"/files/raw?path=/home/user/.ssh/authorized_keys", shared+" alice\n"); r.Status != 200 {
		t.Fatalf("write keys: %s", r)
	}
	if r := env.Do(alice, "POST", "/api/v1/sandboxes/"+sb+"/unshare", nil); r.Status != 200 {
		t.Fatalf("unshare: %s", r)
	}
	// Wait until the revoke has run: the pending flag clears either way.
	env.Eventually("revoke ran", func() bool {
		s, err := env.S.SandboxByID(sb)
		if err != nil {
			return false
		}
		p, _ := s.Extra["sshKeysRevokePending"].(bool)
		return !p
	})
	r := env.Do(alice, "GET", "/api/v1/sandboxes/"+sb+"/files/raw?path=/home/user/.ssh/authorized_keys", nil)
	if r.Status != 200 || !strings.Contains(string(r.Body), "Shared") {
		t.Fatalf("the creator's own key (also sent by a teammate first) was removed on unshare: %d %q", r.Status, r.Body)
	}
}

// bugs-r13-sonnet 4: GET /audit-events (acct_audit.go handleAuditEvents) lists
// every record of the caller's account for any key that holds account.read,
// also when the key is restricted to some sandboxes (sandboxIds). Elsewhere
// a restricted key never sees account-wide data (keysJSON returns nothing,
// key usage is 403 api_key_restricted, saved snapshots go through
// keySeesSaved, sandbox lists filter by AllowsSandbox). The read-only
// preset includes account.read, so a read-only key for sandbox A reads the
// lifecycle events, request log (route, status, ip, key ids) of sandbox B and
// of everything else the user did.
func TestBughuntR13RestrictedKeyReadsWholeAuditLog(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	admin := env.AdminKey
	a := createSandbox(t, env, admin, map[string]any{"noEnv": true})
	b := createSandbox(t, env, admin, map[string]any{"noEnv": true})
	env.WaitState(admin, a, "ready")
	env.WaitState(admin, b, "ready")
	r := env.Do(env.Session(env.Admin), "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "only-a", "ttl": "1d", "preset": "read-only", "sandboxIds": []string{a}})
	if r.Status != 201 {
		t.Fatalf("create restricted key: %s", r)
	}
	key := r.Str("secret")
	if g := env.Do(key, "GET", "/api/v1/sandboxes/"+b, nil); g.Status != 404 && g.Status != 403 {
		t.Fatalf("the restricted key reaches sandbox b directly: %s", g)
	}
	log := env.Do(key, "GET", "/api/v1/audit-events?limit=200", nil)
	if log.Status == 403 {
		return // refusing restricted keys is a valid fix
	}
	if log.Status != 200 {
		t.Fatalf("audit-events: %s", log)
	}
	for _, e := range log.Get("events").([]any) {
		if tgt, _ := e.(map[string]any)["target"].(string); tgt == b {
			t.Fatalf("a key restricted to sandbox %s read the audit record of sandbox %s: %v", a, b, e)
		}
	}
}

// bugs-r13-sonnet 5: handleSaveSaved checks the allowance (savedCount <
// included, snapshots.go:952) and inserts the row ~20 lines later; the only
// lock is per name ("saved:<wallet>/<name>"), so saves of different names
// run side by side and all see the old count. A wallet with no credits and 1
// included saved snapshot gets several new ones from parallel requests,
// each of which should be 402 credit_required (the same race as the r10
// start limits, which lockLimits closed).
func TestBughuntR13ParallelSavesExceedAllowanceOfEmptyWallet(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.EnforceCredits = true })
	// Room for one sandbox per round.
	env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 256, MemoryMiB: 1 << 20, DiskGiB: 100000, MaxSandboxes: 50}})
	for round := 0; round < 10; round++ { // the window is narrow: a fresh wallet per round
		name := "saver" + string(rune('a'+round))
		u, key := env.NewUser(name, false)
		env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/"+name, map[string]any{"savedSnapshotAllowance": 1})
		id := createSandbox(t, env, key, map[string]any{"noEnv": true})
		env.WaitState(key, id, "ready")
		if err := env.S.GrantCredits(u.ID, -env.S.Balance(u.ID), "test", ""); err != nil {
			t.Fatal(err)
		}
		const n = 12
		var wg sync.WaitGroup
		start := make(chan struct{})
		var ok atomic.Int32
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "par-" + string(rune('a'+i))})
				if r.Status == 202 {
					ok.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if ok.Load() > 1 {
			t.Fatalf("round %d: %d parallel saves of new names were accepted by a wallet with no credits and 1 included snapshot", round, ok.Load())
		}
	}
}

// bugs-r13-sonnet 7: extra.evacuatedFrom (set by evacuationResume) is never
// cleared, and failedEvacMoves (nodes_evacuate.go) picks every sandbox in
// state error/snapshot_unavailable whose marker names the node, whatever
// evacuation moved it. Months after an evacuation of node A, a sandbox that
// went to error on its new node for an unrelated reason is booted by the
// next evacuation of A (even one that has nothing to move), on the strength
// of a marker that belongs to an evacuation that ended long ago.
func TestBughuntR13StaleEvacuatedFromMarkerResumesUnrelatedErroredSandbox(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.StartNode(servertest.NodeOptions{Name: "b"})
	if r := env.Do(key, "POST", "/api/v1/nodes/"+a.ID+"/evacuate", map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("evacuate: %s", r)
	}
	env.Eventually("moved to b and ready", func() bool {
		sb, err := env.S.SandboxByID(id)
		return err == nil && sb.NodeID != "" && sb.NodeID != a.ID && sb.State == "ready"
	})
	env.Eventually("first evacuation over", func() bool { return nodeEvacuation(env, key, a.ID) == nil })
	if sb, _ := env.S.SandboxByID(id); !strings.HasPrefix(fmt.Sprint(sb.Extra["evacuatedFrom"]), a.ID) {
		t.Skipf("marker not set: %v", sb.Extra)
	}
	// Later, for a reason of its own, the sandbox is in error (its holder
	// was offline when somebody resumed it).
	if _, err := env.S.DB.Exec(`UPDATE sandboxes SET state='error', error='snapshot_unavailable: its holder is offline' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	// An evacuation of a, which holds no sandbox any more. POST runs
	// evacuateStep itself, so what it did is visible when the call returns.
	if r := env.Do(key, "POST", "/api/v1/nodes/"+a.ID+"/evacuate", map[string]any{"graceSec": 0}); r.Status != 200 {
		t.Fatalf("second evacuate: %s", r)
	}
	if sb, _ := env.S.SandboxByID(id); sb.State != "error" || sb.Extra["pendingBoot"] != nil {
		t.Fatalf("an evacuation of an empty node booted a sandbox that was not on it and failed for another reason (state %s, pendingBoot %v)", sb.State, sb.Extra["pendingBoot"])
	}
}

// bugs-r13-sonnet 6: handleDeviceApprove (device.go) reads the code row
// (status "pending"), creates the API key and only then runs
// `UPDATE device_codes SET status=... WHERE id=?` without `AND
// status='pending'`. Parallel approvals of one code all pass the check; each
// mints a key (api_keys row, valid for 90 days) and only the last one's
// secret is stored for the agent's /oauth2/token poll. The other keys are
// valid, delivered to nobody, and their requests answered 200 as if the
// approval had worked (a second click on the page, a retrying client).
func TestBughuntR13ParallelDeviceApprovalsMintOneKeyEach(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("human", false)
	sess := env.Session(u)
	for round := 0; round < 8; round++ { // the window is short: a fresh code per round
		name := "botx" + string(rune('a'+round))
		id := env.Do("", "POST", "/agent/identity", map[string]any{"method": "anonymous", "name": name})
		claim := env.Do(id.Str("access_token"), "POST", "/agent/identity/claim", nil)
		code := claim.Str("user_code")
		if code == "" {
			t.Fatalf("claim: %s", claim)
		}
		const n = 12
		var wg sync.WaitGroup
		start := make(chan struct{})
		var ok atomic.Int32
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if r := env.Do(sess, "POST", "/auth/device/approve", map[string]any{"userCode": code, "approve": true}); r.Status == 200 {
					ok.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		var keys int
		if err := env.S.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id=? AND name=? AND revoked_at IS NULL`, u.ID, "agent: "+name).Scan(&keys); err != nil {
			t.Fatal(err)
		}
		if ok.Load() != 1 || keys != 1 {
			t.Fatalf("round %d: one device code was approved %d times and minted %d live API keys (want 1 and 1; only one secret is ever delivered)", round, ok.Load(), keys)
		}
	}
}
