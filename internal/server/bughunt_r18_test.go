package server_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/compat"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// A copy made with newDiskKey asks for a key of its own. ensureDiskKey hands
// out a new key only when the parent snapshot carries no pending old key
// (`old == ""`). A snapshot of a copy that is still waiting for its own key
// (a periodic snapshot during the minutes the re-encryption runs, or the stop
// layer of a start whose confirmation did not arrive) carries one, so a copy
// of THAT snapshot made with newDiskKey silently takes the source copy's
// current key instead of a key of its own: the copy's owner (and anyone who
// runs a node for it) then holds the key the source keeps writing with.
func TestBughuntR18NewDiskKeyOfACopyOfAPendingCopy(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{GuestChannel: true})
	key := env.AdminKey
	boot := func(id string) string {
		t.Helper()
		spec, _ := node.Spec(id)
		_, w := rawChannel(t, env, id, spec.Guest.Channel.Ticket)
		return w.DataKey + "|" + w.OldDataKey
	}
	split := func(s string) (string, string) {
		for i := 0; i < len(s); i++ {
			if s[i] == '|' {
				return s[:i], s[i+1:]
			}
		}
		return s, ""
	}
	src := createSandbox(t, env, key, map[string]any{"type": "small"})
	env.WaitState(key, src, "ready")
	srcKey, _ := split(boot(src))
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": src, "name": "tplr18"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/tplr18", "snapshot.status", 5*time.Second, "ready")

	cp := createSandbox(t, env, key, map[string]any{"from": "tplr18", "newDiskKey": true})
	env.WaitState(key, cp, "ready")
	cpKey, cpOld := split(boot(cp))
	if cpKey == "" || cpKey == srcKey || cpOld != srcKey {
		t.Fatalf("setup: copy key %q old %q", cpKey, cpOld)
	}
	// The copy stops before its guest confirmed (its stop layer carries the pending old key).
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+cp+"/stop", nil); r.Status != 200 && r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, cp, "archived")

	// Fixed by refusing (409 disk_rekey_pending) any copy of a disk that is
	// still getting its own key: its snapshot may open with either key, and
	// a copy would carry the source's old key along. Without the fix the
	// fork took cp's key.
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+cp+"/fork", map[string]any{"newDiskKey": true})
	if r.Status == http.StatusAccepted {
		fk := r.Str("sandbox.id")
		if fk == "" {
			fk = r.Str("id")
		}
		env.WaitState(key, fk, "ready")
		if fkKey, _ := split(boot(fk)); fkKey == cpKey {
			t.Fatalf("a fork made with newDiskKey of a copy that waits for its own key got that copy's key, not one of its own")
		}
		return
	}
	if r.Status != http.StatusConflict || r.Str("error.code") != "disk_rekey_pending" {
		t.Fatalf("fork of a copy that waits for its own key: %s", r)
	}
	// Without the flag too: it would carry the source's old key along.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+cp+"/fork", nil); r.Status != http.StatusConflict {
		t.Fatalf("unflagged fork of a copy that waits for its own key: %s", r)
	}
}

// handleRemoveMember moves a leaving member's org sandboxes to personal
// billing (org_id and visibility cleared) and revokes the keys the leaver
// sent into OTHER people's sandboxes (revokeMemberKeys: owner_id != leaver).
// The keys of the remaining teammates inside the leaver's own sandboxes stay:
// the sandbox is personal now, they are no longer teammates of anything, but
// their keys are still in ~/.ssh/authorized_keys and in sandbox_ssh_keys and
// nothing is marked for revocation.
func TestBughuntR18LeaverSandboxKeepsTeammateKeys(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	alice := env.AdminKey
	_, carolKey := env.NewUser("carolr18", false)
	dave, daveKey := env.NewUser("daver18", false)
	env.Do(alice, "POST", "/api/v1/orgs", map[string]any{"name": "AcmeR18"})
	for _, k := range []string{carolKey, daveKey} {
		code := env.Do(alice, "POST", "/api/v1/orgs/acmer18/invites", map[string]any{"email": "x@example.test"}).Str("code")
		if r := env.Do(k, "POST", "/api/v1/org-invites/accept", map[string]any{"code": code}); r.Status != 200 {
			t.Fatalf("accept: %s", r)
		}
	}
	const daveSSH = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDaveDaveDaveDaveDaveDaveDaveDave9"
	const carolSSH = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICarolCarolCarolCarolCarolCarolCa3"
	sb := createSandbox(t, env, carolKey, map[string]any{"org": "acmer18", "noEnv": true, "visibility": "shared"})
	env.WaitState(carolKey, sb, "ready")
	env.Do(carolKey, "POST", "/api/v1/sandboxes/"+sb+"/sshkey", map[string]any{"key": carolSSH})
	env.Do(daveKey, "POST", "/api/v1/sandboxes/"+sb+"/sshkey", map[string]any{"key": daveSSH}) // the fake node has no sshkey route; the key is recorded before the call
	if r := env.Do(carolKey, "PUT", "/api/v1/sandboxes/"+sb+"/files/raw?path=/home/user/.ssh/authorized_keys", carolSSH+" carol\n"+daveSSH+" dave\n"); r.Status != 200 {
		t.Fatalf("write keys: %s", r)
	}
	if r := env.Do(carolKey, "POST", "/api/v1/sandboxes/"+sb+"/stop", nil); r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(carolKey, sb, "archived")
	if r := env.Do(carolKey, "DELETE", "/api/v1/orgs/acmer18/members/me", nil); r.Status != 200 {
		t.Fatalf("carol leaves: %s", r)
	}
	if r := env.Do(carolKey, "POST", "/api/v1/sandboxes/"+sb+"/resume", nil); r.Status != 202 {
		t.Fatalf("resume: %s", r)
	}
	env.WaitState(carolKey, sb, "ready")
	time.Sleep(1500 * time.Millisecond) // a revocation, if any, runs right after ready
	got := string(env.Do(carolKey, "GET", "/api/v1/sandboxes/"+sb+"/files/raw?path=/home/user/.ssh/authorized_keys", nil).Body)
	var n int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM sandbox_ssh_keys WHERE sandbox_id=? AND user_id=?`, sb, dave.ID).Scan(&n)
	if strings.Contains(got, "DaveDave") || n != 0 {
		t.Fatalf("a teammate's key is still in the sandbox of the member who left the org (authorized_keys has it: %v, rows: %d)", strings.Contains(got, "DaveDave"), n)
	}
}

// Round 17 stopped applyPromptDefaults for a prompt that names a
// conversation, so the guest picks that conversation's harness. The server's
// pre-flight (effectiveProvider + promptConfigured) still runs first and
// knows nothing of the conversation: with no provider in the body it checks
// the built-in default "claude". A user who works with Codex only (an OpenAI
// key, the account default provider codex) used to pass it, because the
// account default was written into the body; now a follow-up with
// conversationId is refused with 409 provider_not_configured for Claude.
func TestBughuntR18ContinuedConversationPreflightIgnoresAccountDefault(t *testing.T) {
	env := servertest.New(t)
	g := &promptGuest{FakeGuest: servertest.NewFakeGuest()}
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/openai-api-key", map[string]any{"value": "sk-openai"}); r.Status != 200 {
		t.Fatalf("key: %s", r)
	}
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "PATCH", "/api/v1/account-settings", map[string]any{"agentDefaults": map[string]any{"provider": "codex"}}); r.Status != 200 {
		t.Fatalf("settings: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "start"}); r.Status != 202 {
		t.Fatalf("a first prompt with the account default (codex) must work: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "go on", "conversationId": "conv-started-with-codex"}); r.Status != 202 {
		t.Fatalf("a follow-up to a Codex conversation was refused although only Codex is configured: %s", r)
	}
}

// The node never shrinks a disk below the one in the parent snapshot, for any
// parent (qemu.go create: size = max(want, base.vsize)). Round 17 made the
// server remember the largest disk a sandbox booted with (extra.diskGiB) for
// reporting, placement and reservations, but a fork (or a deploy of a saved
// snapshot) starts with no such record: the copy of a sandbox that grew to a
// bigger type, made at the smaller type, boots on the grown disk while the API
// reports, the scheduler places and the node's usage counts the smaller one.
func TestBughuntR18ForkOfGrownSandboxReportsItsRealDisk(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	src := createSandbox(t, env, key, map[string]any{"type": "small"})
	env.WaitState(key, src, "ready")
	const grown = 123 // GiB: what the sandbox once booted with at a larger type
	if err := env.S.SetSandboxExtra(src, "diskGiB", float64(grown)); err != nil {
		t.Fatal(err)
	}
	if got := num(env.Do(key, "GET", "/api/v1/sandboxes/"+src, nil).Get("sandbox.storage.diskGB")); int(got) != grown {
		t.Fatalf("setup: the source reports %v GB, want %d", got, grown)
	}
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+src+"/fork", map[string]any{})
	if r.Status != http.StatusAccepted {
		t.Fatalf("fork: %s", r)
	}
	fk := r.Str("sandbox.id")
	if fk == "" {
		fk = r.Str("id")
	}
	env.WaitState(key, fk, "ready")
	spec, _ := node.Spec(fk)
	got := num(env.Do(key, "GET", "/api/v1/sandboxes/"+fk, nil).Get("sandbox.storage.diskGB"))
	if int(got) < grown || spec.Size.DiskGiB < grown {
		t.Fatalf("the fork boots on the grown %d GiB disk (the node keeps the parent's size) but is reported as %v GB and sent to its node as %d GiB", grown, got, spec.Size.DiskGiB)
	}
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// currentNodeOnline (compat.go) decides whether outdated nodes may keep
// serving: only while no online, enabled node is current. A current node its
// owner PAUSED counts, although it takes no new sandbox (the scheduler skips
// paused nodes): with one paused current node and one outdated node the pool
// refuses every create ("crowdvm-node misses a required update" on one,
// "paused" on the other) and the outdated node's evacuation deadline runs
// out, so its sandboxes are stopped with nowhere to go.
func TestBughuntR18PausedCurrentNodeDoesNotCountAsCurrent(t *testing.T) {
	setGen(t, &compat.RequiredNodeGen, compat.NodeGen)
	env := servertest.New(t)
	old := 0
	env.StartNode(servertest.NodeOptions{Name: "r18-outdated", Gen: &old})
	env.StartNode(servertest.NodeOptions{Name: "r18-paused", Paused: true})
	time.Sleep(300 * time.Millisecond)
	id := createSandbox(t, env, env.AdminKey, nil)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r := env.Do(env.AdminKey, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") == "ready" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	r := env.Do(env.AdminKey, "GET", "/api/v1/sandboxes/"+id, nil)
	t.Fatalf("the only node that could take a sandbox is outdated and the current node is paused: the create waits (state %s, waitingForCapacity %s)", r.Str("sandbox.state"), r.Str("sandbox.waitingForCapacity"))
}
