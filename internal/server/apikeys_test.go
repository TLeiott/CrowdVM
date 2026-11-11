package server_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestAPIKeyScopes(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	sess := env.Session(env.Admin)

	mk := func(body map[string]any) (string, string) {
		t.Helper()
		r := env.Do(sess, "POST", "/api/v1/api-keys/scoped", body)
		if r.Status != 201 {
			t.Fatalf("create key %v: %s", body, r)
		}
		return r.Str("apiKey.id"), r.Str("secret")
	}
	// A key made while an organization is active is that org's key (Boat KEY-002).
	if r := env.Do(sess, "POST", "/api/v1/orgs", map[string]any{"name": "Keyorg"}); r.Status/100 != 2 {
		t.Fatalf("org: %s", r)
	}
	env.Do(sess, "PATCH", "/api/v1/orgs/active", map[string]any{"org": "keyorg"})
	if r := env.Do(sess, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "o", "preset": "ci"}); r.Str("apiKey.org") == "" {
		t.Fatalf("key under an active org: %s", r)
	}
	if r := env.Do(sess, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "p", "preset": "ci", "org": "personal"}); r.Str("apiKey.org") != "" {
		t.Fatalf("explicit personal key: %s", r)
	}
	// The CLI's --org personal arrives as the org header.
	if r := env.Do(sess, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "h", "preset": "ci"}, "X-CrowdVM-Org: personal"); r.Status != 201 || r.Str("apiKey.org") != "" {
		t.Fatalf("personal key via header: %s", r)
	}
	if r := env.Do(sess, "POST", "/api/v1/sandboxes", map[string]any{}, "X-CrowdVM-Org: personal"); r.Status != 202 || r.Get("sandbox.team") != nil {
		t.Fatalf("personal sandbox via header: %s", r)
	} else {
		id := r.Str("sandbox.id")
		env.Do(sess, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-Ascii-Confirm-Delete: "+id)
	}
	env.Do(sess, "PATCH", "/api/v1/orgs/active", map[string]any{"org": ""})
	// Unknown fields are refused.
	if r := env.Do(sess, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "x", "presett": "ci"}); r.Status != 400 || r.Code() != "unknown_field" {
		t.Fatalf("unknown field: %s", r)
	}
	if r := env.Do(sess, "POST", "/api/v1/api-keys/scoped", map[string]any{"preset": "ci", "actions": []string{"exec"}}); r.Code() != "preset_and_actions" {
		t.Fatalf("preset+actions: %s", r)
	}
	if r := env.Do(sess, "POST", "/api/v1/api-keys/scoped", map[string]any{"ttl": "400d"}); r.Code() != "invalid_ttl" {
		t.Fatalf("ttl: %s", r)
	}

	_, ro := mk(map[string]any{"name": "ro", "preset": "read-only"})
	if r := env.Do(ro, "GET", "/api/v1/sandboxes", nil); r.Status != 200 {
		t.Fatalf("read-only list: %s", r)
	}
	if r := env.Do(ro, "POST", "/api/v1/sandboxes", map[string]any{}); r.Status != 403 || r.Code() != "api_key_action_forbidden" {
		t.Fatalf("read-only create: %s", r)
	}
	// A read-only key cannot create keys.
	if r := env.Do(ro, "POST", "/api/v1/api-keys/scoped", map[string]any{"preset": "read-only"}); r.Status != 403 {
		t.Fatalf("ro creates key: %s", r)
	}

	ciID, ci := mk(map[string]any{"name": "ci", "preset": "ci", "ttl": "90d"})
	own := createSandbox(t, env, ci, nil)
	other := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(ci, own, "ready")
	env.WaitState(env.AdminKey, other, "ready")
	// delete-own: may delete its own sandbox, not others.
	if r := env.Do(ci, "DELETE", "/api/v1/sandboxes/"+other, nil, "X-Ascii-Confirm-Delete: "+other); r.Status != 403 {
		t.Fatalf("ci deletes other: %s", r)
	}
	if r := env.Do(ci, "DELETE", "/api/v1/sandboxes/"+own, nil, "X-Ascii-Confirm-Delete: "+own); r.Status != 202 {
		t.Fatalf("ci deletes own: %s", r)
	}
	// Usage and created resources.
	if r := env.Do(sess, "GET", "/api/v1/api-keys/"+ciID+"/usage", nil); r.Status != 200 || r.Str("usage.requests") == "0" {
		t.Fatalf("usage: %s", r)
	}

	// Sandbox-restricted key.
	_, scoped := mk(map[string]any{"name": "one", "preset": "full-sandbox", "sandboxIds": []string{other}})
	if r := env.Do(scoped, "GET", "/api/v1/sandboxes/"+other, nil); r.Status != 200 {
		t.Fatalf("scoped read: %s", r)
	}
	if r := env.Do(scoped, "POST", "/api/v1/sandboxes/"+other+"/commands", map[string]any{"command": "x"}); r.Status != 200 {
		t.Fatalf("scoped exec: %s", r)
	}
	third := createSandbox(t, env, env.AdminKey, nil)
	if r := env.Do(scoped, "GET", "/api/v1/sandboxes/"+third, nil); r.Status != 403 || r.Code() != "api_key_sandbox_forbidden" {
		t.Fatalf("scoped other sandbox: %s", r)
	}
	if r := env.Do(scoped, "GET", "/api/v1/sandboxes", nil); r.Status != 200 || len(r.Get("sandboxes").([]any)) != 1 {
		t.Fatalf("scoped list: %s", r)
	}
	// Saved snapshots are reached only through their source sandbox (opus-12 B1).
	env.WaitState(env.AdminKey, third, "ready")
	if r := env.Do(env.AdminKey, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": third, "name": "secret-img"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(env.AdminKey, "/api/v1/saved-snapshots/secret-img", "snapshot.status", 5*time.Second, "ready")
	if r := env.Do(scoped, "GET", "/api/v1/saved-snapshots", nil); strings.Contains(r.String(), "secret-img") {
		t.Fatalf("scoped key lists an out-of-scope saved snapshot: %s", r)
	}
	if r := env.Do(scoped, "GET", "/api/v1/saved-snapshots/secret-img", nil); r.Status != 404 {
		t.Fatalf("scoped key reads an out-of-scope saved snapshot: %s", r)
	}
	_, creator := mk(map[string]any{"name": "creator", "actions": []string{"sandbox.create", "sandbox.read", "snapshot.read"}, "sandboxIds": []string{other}})
	if r := env.Do(creator, "POST", "/api/v1/sandboxes", map[string]any{"from": "secret-img"}); r.Status != 404 {
		t.Fatalf("scoped key deploys an out-of-scope saved snapshot: %s", r)
	}
	if r := env.Do(scoped, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": other, "name": "secret-img"}); r.Status != 403 {
		t.Fatalf("scoped key overwrites an out-of-scope saved snapshot: %s", r)
	}
	// Newer sandboxes it cannot see do not hide its own (opus-4 B2).
	createSandbox(t, env, env.AdminKey, nil)
	if r := env.Do(scoped, "GET", "/api/v1/sandboxes?limit=1", nil); r.Str("sandboxes.0.id") != other || r.Str("pageInfo.hasMore") != "false" {
		t.Fatalf("scoped list, limit 1: %s", r)
	}
	if r := env.Do(scoped, "GET", "/api/v1/api-keys", nil); r.Status != 200 || len(r.Get("apiKeys").([]any)) != 0 {
		t.Fatalf("restricted key list must be empty: %s", r)
	}

	// An admin key creates a key, but not one beyond its expiry.
	_, adm := mk(map[string]any{"name": "adm", "preset": "admin", "ttl": "1d"})
	if r := env.Do(adm, "POST", "/api/v1/api-keys/scoped", map[string]any{"preset": "ci", "ttl": "never"}); r.Code() != "api_key_scope_exceeded" {
		t.Fatalf("scope exceeded: %s", r)
	}
	if r := env.Do(adm, "POST", "/api/v1/api-keys/scoped", map[string]any{"preset": "ci", "ttl": "1h"}); r.Status != 201 {
		t.Fatalf("admin key creates key: %s", r)
	}

	// Rotate and revoke.
	r := env.Do(sess, "POST", "/api/v1/api-keys/"+ciID+"/rotate", nil)
	newCI := r.Str("secret")
	if r.Status != 200 || newCI == ci {
		t.Fatalf("rotate: %s", r)
	}
	if r := env.Do(ci, "GET", "/api/v1/me", nil); r.Status != 401 {
		t.Fatalf("old secret after rotate: %s", r)
	}
	if r := env.Do(newCI, "GET", "/api/v1/me", nil); r.Status != 200 {
		t.Fatalf("new secret: %s", r)
	}
	if r := env.Do(sess, "DELETE", "/api/v1/api-keys/"+ciID, nil); r.Status != 200 {
		t.Fatalf("revoke: %s", r)
	}
	if r := env.Do(newCI, "GET", "/api/v1/me", nil); r.Status != 401 {
		t.Fatalf("revoked key: %s", r)
	}
	// List shows metadata only.
	r = env.Do(sess, "GET", "/api/v1/api-keys", nil)
	if r.Status != 200 || r.Get("catalog.presets.ci") == nil || r.Str("apiKeys.0.keyPrefix") == "" || r.Get("apiKeys.0.secret") != nil {
		t.Fatalf("list: %s", r)
	}

	// Expired keys answer 403 api_key_expired.
	exp := time.Now().Add(-time.Minute).UnixMilli()
	_, short := mk(map[string]any{"name": "short", "preset": "read-only", "ttl": "1h"})
	if _, err := env.S.DB.Exec(`UPDATE api_keys SET expires_at=? WHERE name='short'`, exp); err != nil {
		t.Fatal(err)
	}
	if r := env.Do(short, "GET", "/api/v1/me", nil); r.Status != 403 || r.Code() != "api_key_expired" {
		t.Fatalf("expired: %s", r)
	}
}
