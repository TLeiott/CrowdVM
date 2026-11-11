package server_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Round-6 org delete hard-deletes the org's environment_versions (a normal
// env delete is soft and keeps them). The org's saved snapshot, taken from a
// sandbox that ran with the org environment, goes to its saver. Another
// account deploying it must get the org env's secret file scrubbed
// (SNAP-023, ownerCredentialScrub reads the versions); after the org delete
// it is not. Without the org delete the same flow scrubs it.
func TestBughuntR7bOrgDeleteSavedSnapshotLeaksEnvSecret(t *testing.T) {
	g := newEnvRecGuest()
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	makeOrg(t, env, key, "R7bSnap")
	if r := env.Do(key, "POST", "/api/v1/environments?org=r7bsnap", map[string]any{"name": "r7bteam"}); r.Status != 200 {
		t.Fatalf("env create: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/environments/r7bteam?org=r7bsnap", map[string]any{
		"secretFiles": []map[string]any{{"path": ".aws/r7bcreds", "contents": "TOPSECRET"}},
	}); r.Status/100 != 2 {
		t.Fatalf("env: %s", r)
	}
	id := createSandbox(t, env, key, map[string]any{"org": "r7bsnap", "environment": "r7bteam"})
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots?org=r7bsnap", map[string]any{"sandboxId": id, "name": "r7bimg"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/r7bimg?org=r7bsnap", "snapshot.status", 5*time.Second, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	if r := env.Do(key, "DELETE", "/api/v1/orgs/r7bsnap", nil); r.Status != 200 {
		t.Fatalf("delete org: %s", r)
	}
	code := env.Do(key, "POST", "/api/v1/saved-snapshots/r7bimg/shares", nil).Str("shareCode")
	if code == "" {
		t.Fatal("no share code")
	}
	_, bob := env.NewUser("r7bbob", false)
	bobs := createSandbox(t, env, bob, map[string]any{"from": code})
	env.WaitState(bob, bobs, "ready")
	gc := guestSpec(t, node, bobs)
	if gc.Scrub == nil || !slices.Contains(gc.Scrub.Files, ".aws/r7bcreds") {
		files := []string{}
		if gc.Scrub != nil {
			files = gc.Scrub.Files
		}
		t.Fatalf("foreign deploy of the ex-org snapshot does not scrub the org env secret file .aws/r7bcreds (on its disk): scrub files=%v", files)
	}
}

// The round-6 org-delete cascade covers environments and git sources but not
// the org's AI credential profiles: the profile and its stored API key stay
// in the database, owned by an org id nobody can reach any more.
func TestBughuntR7bOrgDeleteLeavesCredentialProfiles(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	orgID := makeOrg(t, env, key, "R7bCred")
	if r := env.Do(key, "POST", "/api/v1/credential-profiles", map[string]any{"name": "r7bteam", "org": "r7bcred"}); r.Status/100 != 2 {
		t.Fatalf("profile: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/r7bteam/keys/anthropic-api-key?org=r7bcred", map[string]any{"value": "sk-ant-api03-r7bsecretkeyvalue0123456789"}); r.Status/100 != 2 {
		t.Logf("set key (ignored): %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/orgs/r7bcred", nil); r.Status != 200 {
		t.Fatalf("delete org: %s", r)
	}
	var profiles, accounts int
	env.S.DB.QueryRow(`SELECT COUNT(*) FROM credential_profiles WHERE owner_id=?`, orgID).Scan(&profiles)
	env.S.DB.QueryRow(`SELECT COUNT(*) FROM credential_accounts WHERE profile_id IN (SELECT id FROM credential_profiles WHERE owner_id=?)`, orgID).Scan(&accounts)
	if profiles > 0 || accounts > 0 {
		t.Fatalf("deleted org still owns %d credential profile(s) with %d stored credential(s)", profiles, accounts)
	}
}

// Several processes starting on one fresh data dir at once. With an existing
// file six concurrent MigrateAll calls pass (round-6 fix holds); on a fresh
// file store.Open itself fails with SQLITE_BUSY at once (the journal_mode=WAL
// switch does not wait for the busy timeout).
func TestBughuntR7bConcurrentOpenMigrate(t *testing.T) {
	for i := 0; i < 10; i++ {
		path := filepath.Join(t.TempDir(), "c.db")
		const n = 6
		errs := make(chan error, n)
		dbs := make(chan *store.DB, n)
		start := make(chan struct{})
		for j := 0; j < n; j++ {
			go func() {
				<-start
				d, err := store.Open(path)
				if err != nil {
					errs <- fmt.Errorf("open: %w", err)
					return
				}
				dbs <- d
				errs <- d.MigrateAll(context.Background())
			}()
		}
		close(start)
		var failed error
		for j := 0; j < n; j++ {
			if err := <-errs; err != nil && failed == nil {
				failed = err
			}
		}
		close(dbs)
		for d := range dbs {
			d.Close()
		}
		if failed != nil {
			t.Fatalf("round %d: %v", i, failed)
		}
	}
}

// "nodes drain" (PATCH disabled=true) promises "running ones keep running",
// and round 6 reports the drain to the node in the heartbeat. But a drained
// node is refused at /node/v1/connect (403 node_disabled), so any reconnect
// (node restart, network blip) cuts it off for good: its running sandboxes
// go to error node_offline and never come back while the drain lasts.
func TestBughuntR7bDrainedNodeCannotReconnect(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Name: "r7bdrain"})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "PATCH", "/api/v1/nodes/"+node.ID, map[string]any{"disabled": true}); r.Status != 200 {
		t.Fatalf("drain: %s", r)
	}
	env.Eventually("heartbeat reports drain", func() bool { return node.LastHeartbeatResponse().Drained })
	if err := node.Restart(); err != nil {
		t.Fatalf("drained node cannot reconnect after a restart: %v", err)
	}
	env.WaitState(key, id, "ready")
}

// Guest events are attributed to whatever sandbox id the node names: the
// server never checks that the sandbox runs on that node. Any node (a
// community node of another owner) can make the server write a round-6
// file_skipped notice, with text of its choice, into another account's
// sandbox (and mail it to them), and likewise store agent events there.
func TestBughuntR7bGuestEventFromForeignNode(t *testing.T) {
	env := servertest.New(t)
	a := env.StartNode(servertest.NodeOptions{Name: "r7ba"})
	b := env.StartNode(servertest.NodeOptions{Name: "r7bb"})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	r := env.WaitState(key, id, "ready")
	other := b
	if r.Str("sandbox.nodeId") == b.ID {
		other = a
	}
	other.SendGuestEvent(id, "file.skipped", map[string]any{"path": "r7b-forged", "error": "visit http://evil.example to fix"})
	time.Sleep(500 * time.Millisecond)
	var n int
	env.S.DB.QueryRow(`SELECT COUNT(*) FROM notices WHERE sandbox_id=? AND kind='file_skipped'`, id).Scan(&n)
	if n > 0 {
		t.Fatalf("node %s (not hosting %s) created %d file_skipped notice(s) on it", other.ID, id, n)
	}
}

// Two secret files skipped at one boot: the 24 h per-sandbox dedupe of the
// file_skipped notice keeps the first path only, so the owner never learns
// that the second secret file is missing too.
func TestBughuntR7bFileSkippedSecondPathLost(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	node.SendGuestEvent(id, "file.skipped", map[string]any{"path": ".r7b/one", "error": "a folder already exists at this path"})
	node.SendGuestEvent(id, "file.skipped", map[string]any{"path": ".r7b/two", "error": "a folder already exists at this path"})
	env.Eventually("first notice", func() bool { return r7bNotices(env, id, "file_skipped") != "" })
	time.Sleep(300 * time.Millisecond)
	all := r7bNotices(env, id, "file_skipped")
	if !strings.Contains(all, ".r7b/one") || !strings.Contains(all, ".r7b/two") {
		t.Fatalf("owner is told about only some skipped files: %q", all)
	}
}

// A foreign deploy (SNAP-023) is "securing" until the saver's credentials are
// scrubbed. If the node drops during the scrub, onNodeOffline moves the
// sandbox to error node_offline and secureAsync gives up (!starting). When
// the node comes back with the machine running, the node_offline recovery in
// reconcileVM makes it ready directly; nothing restarts the scrub. The
// sandbox then shows "ready" for good while every call into it answers
// 409 sandbox_securing "retry in a few seconds".
func TestBughuntR7bNodeBlipDuringScrubStuckSecuring(t *testing.T) {
	g := newEnvRecGuest()
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{
		"secretFiles": []map[string]any{{"path": ".aws/r7bcreds", "contents": "x"}},
	})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "r7btpl"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/r7btpl", "snapshot.status", 5*time.Second, "ready")
	_, bob := env.NewUser("r7bbob2", false)
	code := env.Do(key, "POST", "/api/v1/saved-snapshots/r7btpl/shares", nil).Str("shareCode")
	hold := make(chan struct{})
	g.mu.Lock()
	g.hold = hold
	g.mu.Unlock()
	bobs := createSandbox(t, env, bob, map[string]any{"from": code})
	env.Eventually("scrub started", func() bool { return g.last(bobs+" POST /v1/commands", "authorized_keys") != "" })
	go func() { time.Sleep(200 * time.Millisecond); close(hold) }()
	node.Disconnect()
	g.mu.Lock()
	g.hold = nil
	g.mu.Unlock()
	env.WaitState(bob, bobs, "error")
	time.Sleep(4 * time.Second) // secureAsync's retries end
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	env.WaitFor(bob, "/api/v1/sandboxes/"+bobs, "sandbox.state", 10*time.Second, "ready")
	time.Sleep(5 * time.Second)
	r := env.Do(bob, "GET", "/api/v1/sandboxes/"+bobs, nil)
	cmd := env.Do(bob, "POST", "/api/v1/sandboxes/"+bobs+"/commands", map[string]any{"command": "true"})
	if r.Str("sandbox.state") == "ready" && cmd.Code() == "sandbox_securing" {
		t.Fatalf("state=ready but every call still answers sandbox_securing 6 s after the node returned; the scrub is never resumed (%d scrub calls total)",
			len(r7bCalls(g, bobs+" POST /v1/commands", "authorized_keys")))
	}
}

func r7bCalls(g *envRecGuest, parts ...string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		ok := true
		for _, p := range parts {
			if !strings.Contains(c, p) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}

func r7bNotices(env *servertest.Env, sbID, kind string) string {
	rows, err := env.S.DB.Query(`SELECT message FROM notices WHERE sandbox_id=? AND kind=?`, sbID, kind)
	if err != nil {
		return ""
	}
	defer rows.Close()
	all := ""
	for rows.Next() {
		var m string
		rows.Scan(&m)
		all += m + "\n"
	}
	return all
}
