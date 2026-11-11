package server_test

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r10-opus 1: an org saved snapshot's raw files (tree, files, download)
// hold the saver's env secrets, keys and history; a deploy by another member
// scrubs them, so the file routes must not hand them out to that member.
func TestR10OrgSavedSnapshotFilesOnlyForSaver(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	admin := env.AdminKey
	_, bob := env.NewUser("r10bob", false)
	if r := env.Do(admin, "POST", "/api/v1/orgs", map[string]any{"name": "r10acme"}); r.Status != 201 {
		t.Fatal(r)
	}
	invite := env.Do(admin, "POST", "/api/v1/orgs/r10acme/invites", map[string]any{"email": "r10bob@example.test"})
	if r := env.Do(bob, "POST", "/api/v1/org-invites/accept", map[string]any{"code": invite.Str("code")}); r.Status != 200 {
		t.Fatalf("accept: %s", r)
	}
	id := createSandbox(t, env, admin, map[string]any{"org": "r10acme"})
	env.WaitState(admin, id, "ready")
	if r := env.Do(admin, "POST", "/api/v1/saved-snapshots?org=r10acme", map[string]any{"sandboxId": id, "name": "orgsnap"}); r.Status != 202 {
		t.Fatal(r)
	}
	snap := env.WaitFor(admin, "/api/v1/saved-snapshots/orgsnap?org=r10acme", "snapshot.status", 5*time.Second, "ready").Str("snapshot.snapshotId")
	if r := env.Do(bob, "GET", "/api/v1/saved-snapshots/orgsnap?org=r10acme", nil); r.Status != 200 {
		t.Fatalf("bob should still see the org's saved snapshot: %s", r)
	}
	if r := env.Do(admin, "GET", "/api/v1/snapshots/"+snap+"/download", nil); r.Status != 200 {
		t.Fatalf("the saver lost access: %s", r)
	}
	for _, p := range []string{"/download", "/tree", "/files?path=.bashrc"} {
		if r := env.Do(bob, "GET", "/api/v1/snapshots/"+snap+p, nil); r.Status/100 == 2 {
			t.Fatalf("another org member read the saver's raw snapshot (%s): %s", p, r)
		}
	}
}

// bugs-r10-opus 2: a member removed from an org kept the org's AI key: their
// personal environment still named the org's credential profile, and new
// sandboxes got the org's key.
func TestR10RemovedMemberLosesOrgProfile(t *testing.T) {
	g := newEnvRecGuest()
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	admin := env.AdminKey
	bobU, bob := env.NewUser("r10bobp", false)
	makeOrg(t, env, admin, "R10Prof", "r10bobp@example.test", bob)
	r := env.Do(admin, "POST", "/api/v1/credential-profiles", map[string]any{"name": "orgprof", "org": "r10prof"})
	pid := r.Str("profile.id")
	if pid == "" {
		t.Fatalf("profile: %s", r)
	}
	const orgKey = "sk-ant-api03-r10orgsecretkeyvalue0123456789"
	if r := env.Do(admin, "PUT", "/api/v1/credential-profiles/orgprof/keys/anthropic-api-key?org=r10prof", map[string]any{"value": orgKey}); r.Status/100 != 2 {
		t.Fatalf("org key: %s", r)
	}
	if r := env.Do(bob, "PUT", "/api/v1/environments/base", map[string]any{"credentialProfileId": pid, "passAgentsCredentials": true}); r.Status/100 != 2 {
		t.Fatalf("bob's env on the org profile: %s", r)
	}
	if r := env.Do(admin, "DELETE", "/api/v1/orgs/r10prof/members/"+bobU.ID, nil); r.Status != 200 {
		t.Fatalf("remove: %s", r)
	}
	id := createSandbox(t, env, bob, nil)
	env.WaitState(bob, id, "ready")
	gc := guestSpec(t, node, id)
	var tokProfile string
	env.S.DB.QueryRow(`SELECT profile_id FROM ai_proxy_tokens WHERE sandbox_id=?`, id).Scan(&tokProfile)
	if gc.Env["ANTHROPIC_API_KEY"] == orgKey || tokProfile == pid {
		t.Fatalf("a removed member's new sandbox still gets the org's AI key (env key set: %v, proxy token profile %q)", gc.Env["ANTHROPIC_API_KEY"] != "", tokProfile)
	}
}

// bugs-r10-opus 3: a live `env upgrade` kept the AI proxy placeholder on the
// boot's profile: it went on serving the old profile's key after the profile
// changed, and any key after agent credentials were turned off.
func TestR10LiveUpgradeMovesProxyToken(t *testing.T) {
	g := newEnvRecGuest()
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/anthropic-api-key", map[string]any{"value": "sk-ant-api03-r10defaultkey0123456789"})
	p2 := env.Do(key, "POST", "/api/v1/credential-profiles", map[string]any{"name": "p2"}).Str("profile.id")
	env.Do(key, "PUT", "/api/v1/credential-profiles/p2/keys/anthropic-api-key", map[string]any{"value": "sk-ant-api03-r10p2key0123456789"})
	env.Do(key, "PUT", "/api/v1/ai-proxy", map[string]any{"enabled": true})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	tokProfile := func() (string, int) {
		var p string
		var n int
		env.S.DB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(profile_id),'') FROM ai_proxy_tokens WHERE sandbox_id=?`, id).Scan(&n, &p)
		return p, n
	}
	if _, n := tokProfile(); n != 1 {
		t.Fatalf("no proxy token at boot")
	}
	upgrade := func() {
		t.Helper()
		if r := env.Do(key, "POST", "/api/v1/environments/base/upgrade", nil); r.Str("upgradedLive") != "1" {
			t.Fatalf("upgrade: %s", r)
		}
	}
	if r := env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"credentialProfileId": p2}); r.Status/100 != 2 {
		t.Fatalf("set profile: %s", r)
	}
	upgrade()
	if p, _ := tokProfile(); p != p2 {
		t.Fatalf("after a live upgrade to profile p2 the placeholder still serves profile %q", p)
	}
	if r := env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"passAgentsCredentials": false}); r.Status/100 != 2 {
		t.Fatalf("agents off: %s", r)
	}
	upgrade()
	if _, n := tokProfile(); n != 0 {
		t.Fatalf("agent credentials are off but the sandbox's proxy placeholder still works")
	}
	if g.last(id+" POST /v1/internal/apply-config", `"clearAIProxy":true`) == "" {
		t.Fatalf("the guest was not told to drop its AI proxy settings")
	}
}
