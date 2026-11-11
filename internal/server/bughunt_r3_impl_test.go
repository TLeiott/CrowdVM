package server_test

import (
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"testing"
)

func TestR3SecretFileConflicts(t *testing.T) {
	e := servertest.New(t)
	for _, paths := range [][]string{{"a/b", "a/b/c"}, {"a/b/c", "~/a/b"}, {"bad\nname"}} {
		files := []any{}
		for _, p := range paths {
			files = append(files, map[string]any{"path": p, "contents": "x"})
		}
		created := e.Do(e.AdminKey, "POST", "/api/v1/environments", map[string]any{"name": "conflict"})
		r := e.Do(e.AdminKey, "PUT", "/api/v1/environments/"+created.Str("environment.id"), map[string]any{"secretFiles": files})
		e.Do(e.AdminKey, "DELETE", "/api/v1/environments/"+created.Str("environment.id"), nil)
		if r.Status != 400 || r.Code() != "invalid_secret_files" {
			t.Fatalf("%v: %s", paths, r)
		}
	}
}
func TestR3HugeKeyTTL(t *testing.T) {
	e := servertest.New(t)
	for _, ttl := range []string{"106752d", "9223372036854775807s"} {
		r := e.Do(e.AdminKey, "POST", "/api/v1/api-keys", map[string]any{"name": "overflow", "ttl": ttl})
		if r.Status != 400 || r.Code() != "invalid_ttl" {
			t.Fatalf("%s: %s", ttl, r)
		}
	}
}
func TestR3OrgSafeguards(t *testing.T) {
	e := servertest.New(t)
	e.StartNode(servertest.NodeOptions{})
	bob, key := e.NewUser("bob", false)
	r := e.Do(e.AdminKey, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	oid := r.Str("org.id")
	if oid == "" {
		t.Fatal(r)
	}
	invite := e.Do(e.AdminKey, "POST", "/api/v1/orgs/acme/invites", map[string]any{"email": "bob@example.test"})
	if r := e.Do(key, "POST", "/api/v1/org-invites/accept", map[string]any{"code": invite.Str("code")}); r.Status != 200 {
		t.Fatal(r)
	}
	for _, body := range []map[string]any{{"concurrentCap": -5}, {"usageCapCredits": -1}} {
		r := e.Do(e.AdminKey, "PATCH", "/api/v1/orgs/acme/members/"+bob.ID, body)
		if r.Status != 400 || r.Code() != "invalid_member_cap" {
			t.Fatal(r)
		}
	}
	id := createSandbox(t, e, key, map[string]any{"org": "acme", "noEnv": true})
	// A queued start also must not escape org billing via member removal.
	r = e.Do(e.AdminKey, "DELETE", "/api/v1/orgs/acme/members/"+bob.ID, nil)
	if r.Status != 409 || r.Code() != "member_has_running_sandboxes" {
		t.Fatalf("%s: %s", id, r)
	}
	// Remove the test sandbox and close its run without a VM or cluster.
	if _, err := e.S.DB.Exec(`UPDATE sandboxes SET state='archived' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.S.DB.Exec(`UPDATE sandbox_runs SET ended_at=started_at WHERE sandbox_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if r := e.Do(e.AdminKey, "DELETE", "/api/v1/orgs/acme/members/"+bob.ID, nil); r.Status != 200 {
		t.Fatal(r)
	}
	if err := e.S.GrantCredits(oid, 10, "test", ""); err != nil {
		t.Fatal(err)
	}
	r = e.Do(e.AdminKey, "DELETE", "/api/v1/orgs/acme", nil)
	if r.Status != 409 || r.Code() != "org_has_balance" {
		t.Fatal(r)
	}
	if e.S.Balance(oid) != 10 {
		t.Fatal("balance lost")
	}
}
func TestR3RestrictedKeyUsage(t *testing.T) {
	e := servertest.New(t)
	r := e.Do(e.AdminKey, "POST", "/api/v1/api-keys", map[string]any{"name": "restricted", "ttl": "1h", "sandboxIds": []string{"bx_none"}, "actions": []string{"account.read"}})
	secret := r.Str("secret")
	if secret == "" {
		t.Fatal(r)
	}
	other := e.Do(e.AdminKey, "POST", "/api/v1/api-keys", map[string]any{"name": "other", "ttl": "1h"})
	r = e.Do(secret, "GET", "/api/v1/api-keys/"+other.Str("apiKey.id")+"/usage", nil)
	if r.Status != 403 {
		t.Fatal(r)
	}
}
