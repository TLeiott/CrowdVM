package server_test

import (
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r20-opus 3: moving the month's quota to another wallet and back
// (two transfers, no usage) counted it as spent, so it never expired.
func TestR20QuotaMovedAwayAndBackStillExpires(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("r20quota", false)
	base := env.S.Balance(u.ID)
	if r := env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/r20quota", map[string]any{"monthlyCredits": 10}); r.Status != 200 {
		t.Fatalf("policy: %s", r)
	}
	env.S.BillNow() // this month's 10
	now := time.Now().UnixMilli() + 1
	for _, d := range []float64{-10, 10} { // out to another wallet and back: the balance is unchanged
		if _, err := env.S.DB.Exec(`INSERT INTO credit_ledger(wallet_id,delta,kind,reason,actor_id,created_at) VALUES(?,?,'transfer','test',?,?)`, u.ID, d, u.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.S.DB.Exec(`UPDATE wallet_state SET quota_month='2000-01' WHERE wallet_id=?`, u.ID); err != nil {
		t.Fatal(err)
	}
	env.S.BillNow()
	if b, want := env.S.Balance(u.ID), base+10; b != want {
		t.Fatalf("balance %v, want %v: the unused quota did not expire", b, want)
	}
}

// bugs-r20-opus 6: a browser profile of /home or /opt was accepted and the
// guest handed that folder to the browser user.
func TestR20BrowserProfileMustBeBelowATopLevelDir(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	id := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, id, "ready")
	for _, p := range []string{"/home", "/opt", "/mnt", "/srv", "/media"} {
		if r := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes/"+id+"/browser", map[string]any{"profile": p}); r.Status != 400 {
			t.Fatalf("profile %s: %s", p, r)
		}
	}
}

// bugs-r20-opus 2: a live upgrade to a version without repositories sent
// "repos": null, which the guest reads as "unchanged": it kept the old
// repos and rewrote their tokens. It must send an empty list.
func TestR20LiveUpgradeWithoutReposSendsEmptyList(t *testing.T) {
	g := newEnvRecGuest()
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"passAgentsCredentials": false}); r.Status/100 != 2 {
		t.Fatalf("change env: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/environments/base/upgrade", nil); r.Str("upgradedLive") != "1" {
		t.Fatalf("upgrade: %s", r)
	}
	if g.last(id+" POST /v1/internal/apply-config", `"repos":[]`) == "" {
		t.Fatalf("the live apply did not send an empty repo list: %q", g.last(id+" POST /v1/internal/apply-config", `"repos"`))
	}
}
