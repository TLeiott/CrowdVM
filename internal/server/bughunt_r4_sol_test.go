package server_test

import (
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func r4OrgKey(t *testing.T) (*servertest.Env, string, string, string) {
	t.Helper()
	env := servertest.New(t)
	u, _ := env.NewUser("r4owner", false)
	sess := env.Session(u)
	for _, name := range []string{"r4alpha", "r4beta"} {
		r := env.Do(sess, "POST", "/api/v1/orgs", map[string]any{"name": name})
		if r.Status != 201 {
			t.Fatal(r)
		}
	}
	k := scopedKey(t, env, sess, map[string]any{"name": "alpha only", "preset": "admin", "org": "r4alpha"})
	return env, k, sess, u.ID
}

func TestBughuntR4SolOrgKeyCannotSwitchOtherWallet(t *testing.T) {
	env, key, sess, uid := r4OrgKey(t)
	if r := env.Do(sess, "PATCH", "/api/v1/orgs/active", map[string]any{"org": "r4alpha"}); r.Status != 200 {
		t.Fatal(r)
	}
	var before string
	if err := env.S.DB.QueryRow(`SELECT active_org FROM users WHERE id=?`, uid).Scan(&before); err != nil {
		t.Fatal(err)
	}
	r := env.Do(key, "PATCH", "/api/v1/orgs/active", map[string]any{"org": "r4beta"})
	var after string
	if err := env.S.DB.QueryRow(`SELECT active_org FROM users WHERE id=?`, uid).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if r.Status != 403 || after != before {
		t.Fatalf("Alpha-only key switched active wallet: %s -> %s; %s", before, after, r)
	}
}

func TestBughuntR4SolConcurrentMoveCreditsCannotOverdraw(t *testing.T) {
	env, _, sess, uid := r4OrgKey(t)
	if err := env.S.GrantCredits(uid, 10-env.S.Balance(uid), "repro", uid); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			env.Do(sess, "POST", "/api/v1/orgs/r4alpha/move-personal", map[string]any{"transferCredits": 10})
		}()
	}
	close(start)
	wg.Wait()
	if balance := env.S.Balance(uid); balance < 0 {
		t.Fatalf("concurrent move-personal overdraws wallet: balance=%g, initial=10", balance)
	}
}

func TestBughuntR4SolResharePreservesPreviouslySharedHistory(t *testing.T) {
	g := &histGuest{FakeGuest: servertest.NewFakeGuest(), export: map[string]proto.HistoryExport{}}
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	r := env.Do(key, "POST", "/api/v1/orgs", map[string]any{"name": "historyorg"})
	if r.Status != 201 {
		t.Fatal(r)
	}
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if _, err := env.S.DB.Exec(`UPDATE sandboxes SET org_id=?, visibility='shared', private_by_creator=0 WHERE id=?`, r.Str("org.id"), id); err != nil {
		t.Fatal(err)
	}
	// The command ran while the sandbox was shared, before the private interval.
	started := time.Now().Add(-time.Second)
	for _, action := range []string{"unshare", "share"} {
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/"+action, nil); r.Status != 200 {
			t.Fatal(r)
		}
	}
	g.mu.Lock()
	g.export[id] = proto.HistoryExport{Batch: "public-before-unshare", Records: []proto.CommandRecord{{CommandID: "cmd_public", SandboxID: id, Command: "echo public", Mode: "sync", Status: "exited", StartedAt: started}}}
	g.mu.Unlock()
	stopUntilArchived(t, env, key, id)
	var private int
	if err := env.S.DB.QueryRow(`SELECT private FROM command_history WHERE sandbox_id=? AND command_id='cmd_public'`, id).Scan(&private); err != nil {
		t.Fatal(err)
	}
	if private != 0 {
		t.Fatalf("previously shared command became private after unshare/share: private=%d, want=0", private)
	}
}

func TestBughuntR4SolOrgKeyCannotUnlinkOtherOrgCI(t *testing.T) {
	env, key, _, uid := r4OrgKey(t)
	var org string
	if err := env.S.DB.QueryRow(`SELECT id FROM orgs WHERE name='r4beta'`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if _, err := env.S.DB.Exec(`INSERT INTO ci_links(id,owner_id,org_id,api_url,login,account_type,token,status,created_at) VALUES('cil_r4_beta',?,?, 'https://api.github.com','beta-ci','User','','pending',?)`, uid, org, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	r := env.Do(key, "DELETE", "/api/v1/ci/links/cil_r4_beta", nil)
	var remaining int
	if err := env.S.DB.QueryRow(`SELECT COUNT(*) FROM ci_links WHERE id='cil_r4_beta'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if r.Status/100 == 2 || remaining != 1 {
		t.Fatalf("Alpha-only key deleted Beta CI link: remaining=%d; %s", remaining, r)
	}
}
