package server_test

import (
	"sync"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func scopedKey(t *testing.T, env *servertest.Env, sess string, body map[string]any) string {
	t.Helper()
	body["ttl"] = "1d"
	r := env.Do(sess, "POST", "/api/v1/api-keys/scoped", body)
	if r.Status != 201 {
		t.Fatalf("create key: %s", r)
	}
	return r.Str("secret")
}

// bugs-r3-opus 11/18: a key cannot mint a service account key with more
// access or a later expiry than its own; a bad preset or TTL creates
// nothing.
func TestServiceAccountKeyBoundedByCaller(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("owner1", false)
	sess := env.Session(u)
	if r := env.Do(sess, "POST", "/api/v1/orgs", map[string]any{"name": "acme"}); r.Status/100 != 2 {
		t.Fatalf("create org: %s", r)
	}
	k := scopedKey(t, env, sess, map[string]any{"name": "acct", "actions": []string{"account.admin"}})
	if r := env.Do(k, "POST", "/api/v1/orgs/acme/service-accounts", map[string]any{"name": "bot", "preset": "admin", "ttl": "never"}); r.Status != 403 {
		t.Fatalf("service account key above the caller's: %s", r)
	}
	for _, body := range []map[string]any{{"name": "bad1", "preset": "nope"}, {"name": "bad2", "ttl": "forever-ish"}} {
		if r := env.Do(sess, "POST", "/api/v1/orgs/acme/service-accounts", body); r.Status != 400 {
			t.Fatalf("bad service account %v: %s", body, r)
		}
	}
	var n int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE username LIKE 'svc-%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d service users left behind", n)
	}
}

// bugs-r3-opus 13: a read-only key cannot switch the active wallet.
func TestReadOnlyKeyCannotSwitchWallet(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("owner2", false)
	sess := env.Session(u)
	env.Do(sess, "POST", "/api/v1/orgs", map[string]any{"name": "acme2"})
	ro := env.Key(u, "read-only")
	if r := env.Do(ro, "PATCH", "/api/v1/orgs/active", map[string]any{"org": "acme2"}); r.Status != 403 {
		t.Fatalf("read-only key switched the wallet: %s", r)
	}
}

// bugs-r3-opus 14: transfers at once never overdraw the source wallet.
func TestConcurrentTransfersDoNotOverdraw(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("owner3", false)
	sess := env.Session(u)
	env.Do(sess, "POST", "/api/v1/orgs", map[string]any{"name": "acme3"})
	bal := env.S.Balance(u.ID)
	if bal <= 0 {
		t.Skip("no starting credits")
	}
	amount := bal / 3
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env.Do(sess, "POST", "/api/v1/credits/transfer", map[string]any{"from": "personal", "to": "acme3", "amount": amount})
		}()
	}
	wg.Wait()
	if b := env.S.Balance(u.ID); b < -1e-9 {
		t.Fatalf("wallet overdrawn: %v", b)
	}
}

// bugs-r4-opus 2: CI connect does not reach loopback or metadata addresses
// (unless the admin allows local targets), nor arbitrary paths via a query.
func TestCIConnectRefusesLocalTargets(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.WebhookAllowLocal = false })
	for _, u := range []string{"http://127.0.0.1:9/", "http://169.254.169.254/latest", "http://localhost:9", "http://192.168.1.5/api?", "http://192.168.1.5/api#"} {
		r := env.Do(env.AdminKey, "POST", "/api/v1/ci/connect", map[string]any{"token": "x", "apiUrl": u})
		if r.Status != 400 || r.Code() != "invalid_api_url" {
			t.Fatalf("connect %s: %s", u, r)
		}
	}
}

// bugs-r5-opus 6: the balance the API shows (rounded up) can be moved out,
// and the org can then be deleted.
func TestTransferShownBalanceThenDeleteOrg(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("owner6", false)
	sess := env.Session(u)
	r := env.Do(sess, "POST", "/api/v1/orgs", map[string]any{"name": "beta6"})
	org := r.Str("org.id")
	_, _ = env.S.DB.Exec(`INSERT INTO wallets(id,balance,updated_at) VALUES(?,?,0) ON CONFLICT(id) DO UPDATE SET balance=excluded.balance`, org, 0.0000015) // 7th decimal 5 (bugs-r6-opus 5)
	shown := env.Do(sess, "GET", "/api/v1/orgs/beta6", nil).Get("org.wallet.balanceCredits").(float64)
	if r := env.Do(sess, "POST", "/api/v1/credits/transfer", map[string]any{"from": "beta6", "to": "personal", "amount": shown}); r.Status != 200 {
		t.Fatalf("transfer the shown balance %v: %s", shown, r)
	}
	if r := env.Do(sess, "DELETE", "/api/v1/orgs/beta6", nil); r.Status != 200 {
		t.Fatalf("delete after moving everything: %s", r)
	}
}

// bugs-r5-opus 4: the org's owners delete a service account's sandbox
// (its key may have expired); plain members still cannot.
func TestOrgOwnerDeletesServiceAccountSandbox(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	_, ownerKey := env.NewUser("owner7", false)
	env.Do(ownerKey, "POST", "/api/v1/orgs", map[string]any{"name": "svcorg"})
	member, memberKey := env.NewUser("member7", false)
	_, _ = env.S.DB.Exec(`INSERT INTO org_members(org_id,user_id,role,joined_at) VALUES((SELECT id FROM orgs WHERE name='svcorg'),?,'member',0)`, member.ID)
	r := env.Do(ownerKey, "POST", "/api/v1/orgs/svcorg/service-accounts", map[string]any{"name": "bot", "preset": "ci", "ttl": "1h"})
	if r.Status != 201 {
		t.Fatal(r)
	}
	botKey := r.Str("secret")
	id := createSandbox(t, env, botKey, nil)
	env.WaitState(botKey, id, "ready")
	if r := env.Do(memberKey, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-CrowdVM-Confirm-Delete: "+id); r.Status/100 == 2 {
		t.Fatalf("member deleted the service account's sandbox: %s", r)
	}
	if r := env.Do(ownerKey, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-CrowdVM-Confirm-Delete: "+id); r.Status/100 != 2 {
		t.Fatalf("org owner deletes the service account's sandbox: %s", r)
	}
}

// bugs-r5-opus 8: nested folders the desktop and guest own are refused too.
func TestSecretFileNestedHomeDirs(t *testing.T) {
	env := servertest.New(t)
	for _, p := range []string{".config/xfce4", ".dbus", ".local/bin", ".ascii", ".gnupg", ".crowdvm"} {
		if r := env.Do(env.AdminKey, "PUT", "/api/v1/environments/base/secret-files", map[string]string{"path": p, "contents": "x"}); r.Status != 400 {
			t.Fatalf("%s accepted: %s", p, r)
		}
	}
	if r := env.Do(env.AdminKey, "PUT", "/api/v1/environments/base/secret-files", map[string]string{"path": ".local/bin/tool.env", "contents": "x"}); r.Status != 200 {
		t.Fatalf("a file inside .local/bin: %s", r)
	}
}
