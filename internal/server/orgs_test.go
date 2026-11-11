package server_test

import (
	"testing"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestOrgsAndSharing(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	alice, aliceKey := env.Admin, env.AdminKey
	_ = alice
	_, bobKey := env.NewUser("bob", false)
	_, carolKey := env.NewUser("carol", false)

	r := env.Do(aliceKey, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	if r.Status != 201 {
		t.Fatalf("create org: %s", r)
	}
	orgID := r.Str("org.id")
	r = env.Do(aliceKey, "POST", "/api/v1/orgs/acme/invites", map[string]any{"email": "bob@example.test"})
	if r.Status != 201 || r.Str("code") == "" {
		t.Fatalf("invite: %s", r)
	}
	if r := env.Do(bobKey, "POST", "/api/v1/org-invites/accept", map[string]any{"code": r.Str("code")}); r.Status != 200 {
		t.Fatalf("accept: %s", r)
	}
	r = env.Do(bobKey, "GET", "/api/v1/orgs", nil)
	if r.Status != 200 || r.Str("orgs.0.type") != "personal" || r.Str("orgs.1.id") != orgID || r.Str("orgs.1.role") != "member" || r.Str("activeWalletChosen") != "false" {
		t.Fatalf("orgs: %s", r)
	}
	if r := env.Do(carolKey, "PATCH", "/api/v1/orgs/active", map[string]any{"org": "acme"}); r.Status != 403 || r.Code() != "not_org_member" {
		t.Fatalf("non-member switch: %s", r)
	}
	if r := env.Do(bobKey, "PATCH", "/api/v1/orgs/active", map[string]any{"org": "ACME"}); r.Status != 200 || r.Str("active.id") != orgID {
		t.Fatalf("switch: %s", r)
	}
	if r := env.Do(bobKey, "GET", "/api/v1/limits", nil); r.Str("teamId") != orgID {
		t.Fatalf("limits follow active org: %s", r)
	}

	// Alice creates an org sandbox (shared by default) and a private one.
	// noEnv: a sandbox whose environment passes her logins starts private.
	shared := createSandbox(t, env, aliceKey, map[string]any{"org": "acme", "noEnv": true})
	private := createSandbox(t, env, aliceKey, map[string]any{"org": orgID, "visibility": "private", "noEnv": true})
	personal := createSandbox(t, env, aliceKey, nil)
	env.WaitState(aliceKey, shared, "ready")
	env.WaitState(aliceKey, private, "ready")
	if r := env.Do(aliceKey, "POST", "/api/v1/sandboxes", map[string]any{"visibility": "shared"}); r.Code() != "not_org_sandbox" {
		t.Fatalf("personal shared: %s", r)
	}

	// Bob (active org acme) lists the org's sandboxes, not Alice's personal one.
	r = env.Do(bobKey, "GET", "/api/v1/sandboxes", nil)
	seen := map[string]string{}
	for _, s := range r.Get("sandboxes").([]any) {
		m := s.(map[string]any)
		seen[m["id"].(string)] = m["access"].(string)
	}
	if seen[shared] != "use" || seen[private] != "view" || seen[personal] != "" {
		t.Fatalf("bob's view: %v", seen)
	}
	if r := env.Do(bobKey, "GET", "/api/v1/sandboxes/"+shared, nil); r.Str("sandbox.team.name") != "Acme" || r.Str("sandbox.visibility") != "shared" || r.Str("sandbox.createdBy") != "admin" {
		t.Fatalf("team fields: %s", r)
	}
	// use: run commands; view: refused; owner-only: refused.
	if r := env.Do(bobKey, "POST", "/api/v1/sandboxes/"+shared+"/commands", map[string]any{"command": "x"}); r.Status != 200 {
		t.Fatalf("bob uses shared: %s", r)
	}
	if r := env.Do(bobKey, "POST", "/api/v1/sandboxes/"+private+"/commands", map[string]any{"command": "x"}); r.Status != 403 || r.Code() != "sandbox_private" {
		t.Fatalf("bob uses private: %s", r)
	}
	if r := env.Do(bobKey, "POST", "/api/v1/sandboxes/"+shared+"/fork", nil); r.Status != 403 || r.Code() != "owner_only" {
		t.Fatalf("bob forks: %s", r)
	}
	if r := env.Do(bobKey, "PATCH", "/api/v1/sandboxes/"+shared, map[string]any{"name": "x"}); r.Code() != "owner_only" {
		t.Fatalf("bob renames: %s", r)
	}
	// Bob cannot stop a sandbox Alice made private (only org owner/admin).
	if r := env.Do(bobKey, "POST", "/api/v1/sandboxes/"+private+"/stop", nil); r.Status != 403 {
		t.Fatalf("bob stops private: %s", r)
	}
	// Carol sees nothing.
	if r := env.Do(carolKey, "GET", "/api/v1/sandboxes/"+shared, nil); r.Status != 404 {
		t.Fatalf("carol: %s", r)
	}
	if r := env.Do(aliceKey, "GET", "/api/v1/sandboxes/"+personal, nil); r.Str("sandbox.visibility") != "" || r.Get("sandbox.team") != nil {
		t.Fatalf("personal sandbox fields: %s", r)
	}

	// Share / unshare.
	if r := env.Do(bobKey, "POST", "/api/v1/sandboxes/"+private+"/share", nil); r.Code() != "owner_only" {
		t.Fatalf("bob shares: %s", r)
	}
	r = env.Do(aliceKey, "POST", "/api/v1/sandboxes/"+private+"/share", nil)
	if r.Status != 200 || r.Str("sandbox.visibility") != "shared" || r.Str("restartRequired") != "false" {
		t.Fatalf("share: %s", r)
	}
	if r := env.Do(bobKey, "POST", "/api/v1/sandboxes/"+private+"/commands", map[string]any{"command": "x"}); r.Status != 200 {
		t.Fatalf("bob after share: %s", r)
	}
	if r := env.Do(aliceKey, "POST", "/api/v1/sandboxes/"+shared+"/unshare", nil); r.Str("sandbox.visibility") != "private" {
		t.Fatalf("unshare: %s", r)
	}
	if r := env.Do(aliceKey, "POST", "/api/v1/sandboxes/"+personal+"/share", nil); r.Code() != "not_org_sandbox" {
		t.Fatalf("share personal: %s", r)
	}

	// Default visibility private.
	if r := env.Do(bobKey, "PATCH", "/api/v1/orgs/acme", map[string]any{"newSandboxesPrivate": true}); r.Status != 403 {
		t.Fatalf("member changes org: %s", r)
	}
	if r := env.Do(aliceKey, "PATCH", "/api/v1/orgs/acme", map[string]any{"newSandboxesPrivate": true}); r.Status != 200 {
		t.Fatalf("org default: %s", r)
	}
	r = env.Do(bobKey, "POST", "/api/v1/sandboxes", map[string]any{})
	if r.Status != 202 || r.Str("sandbox.visibility") != "private" || r.Str("sandbox.team.id") != orgID {
		t.Fatalf("bob org sandbox: %s", r)
	}

	bobSandbox := r.Str("sandbox.id")

	// Org keys bill and see only their org.
	r = env.Do(env.Session(env.Admin), "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "orgkey", "preset": "ci", "org": "acme"})
	orgKey := r.Str("secret")
	if r := env.Do(orgKey, "GET", "/api/v1/sandboxes/"+personal, nil); r.Status != 404 {
		t.Fatalf("org key sees personal: %s", r)
	}
	if r := env.Do(orgKey, "POST", "/api/v1/sandboxes", map[string]any{"org": "personal"}); r.Code() != "api_key_org_mismatch" {
		t.Fatalf("org key personal: %s", r)
	}

	// Members, roles, removal.
	r = env.Do(aliceKey, "GET", "/api/v1/orgs/acme/members", nil)
	if len(r.Get("members").([]any)) != 2 {
		t.Fatalf("members: %s", r)
	}
	bobID := env.Do(bobKey, "GET", "/api/v1/me", nil).Str("user.id")
	if r := env.Do(aliceKey, "PATCH", "/api/v1/orgs/acme/members/"+bobID, map[string]any{"concurrentCap": 0}); r.Status != 200 {
		t.Fatalf("cap: %s", r)
	}
	if r := env.Do(bobKey, "POST", "/api/v1/sandboxes", map[string]any{"org": "acme"}); r.Status != 429 || r.Code() != "member_limit_reached" {
		t.Fatalf("member cap: %s", r)
	}
	// Boat: the owner deletes an organization only as its last member (opus-15 B2).
	if r := env.Do(aliceKey, "DELETE", "/api/v1/orgs/acme", nil); r.Status != 409 || r.Code() != "org_has_members" {
		t.Fatalf("delete with members: %s", r)
	}
	env.WaitState(bobKey, bobSandbox, "ready")
	if r := env.Do(bobKey, "POST", "/api/v1/sandboxes/"+bobSandbox+"/stop", nil); r.Status != 200 && r.Status != 202 {
		t.Fatal(r)
	}
	env.WaitState(bobKey, bobSandbox, "archived")
	if r := env.Do(aliceKey, "DELETE", "/api/v1/orgs/acme/members/"+bobID, nil); r.Status != 200 {
		t.Fatalf("remove: %s", r)
	}
	if r := env.Do(bobKey, "GET", "/api/v1/sandboxes/"+shared, nil); r.Status != 404 {
		t.Fatalf("removed member still sees: %s", r)
	}
}
