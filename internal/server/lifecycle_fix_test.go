package server_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func makeOrg(t *testing.T, env *servertest.Env, ownerKey, name string, members ...string) string {
	t.Helper()
	r := env.Do(ownerKey, "POST", "/api/v1/orgs", map[string]any{"name": name})
	if r.Status/100 != 2 {
		t.Fatalf("org: %s", r)
	}
	for i := 0; i+1 < len(members); i += 2 {
		inv := env.Do(ownerKey, "POST", "/api/v1/orgs/"+strings.ToLower(name)+"/invites", map[string]any{"email": members[i]})
		if r := env.Do(members[i+1], "POST", "/api/v1/org-invites/accept", map[string]any{"code": inv.Str("code")}); r.Status != 200 {
			t.Fatalf("accept: %s", r)
		}
	}
	return r.Str("org.id")
}

// SNAP-023: deploying someone else's saved snapshot scrubs the saver's
// credentials before the sandbox is usable, and copies none of their env.
func TestForeignDeployScrub(t *testing.T) {
	g := newEnvRecGuest()
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{
		"secretFiles":    []map[string]any{{"path": ".aws/creds", "contents": "x"}},
		"repositories":   []map[string]any{{"repositoryId": "http://git.lan/team/app.git"}},
		"registryLogins": []map[string]any{{"registry": "reg.lan:5000", "username": "u", "password": "p"}},
	})
	id := createSandbox(t, env, key, map[string]any{"env": map[string]string{"MINE": "1"}})
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "tpl"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/tpl", "snapshot.status", 5*time.Second, "ready")

	// The saver's own deploy: no scrub, its env comes along.
	own := createSandbox(t, env, key, map[string]any{"from": "tpl"})
	env.WaitState(key, own, "ready")
	if gc := guestSpec(t, node, own); gc.Scrub != nil || gc.Env["MINE"] != "1" {
		t.Fatalf("own deploy: scrub=%+v env=%v", gc.Scrub, gc.Env)
	}

	// Another account through a share code.
	_, bobKey := env.NewUser("bob", false)
	code := env.Do(key, "POST", "/api/v1/saved-snapshots/tpl/shares", nil).Str("shareCode")
	if len(code) != len(api.PrefixShare)+52 {
		t.Fatalf("share code length %d (want 256 bits)", len(code))
	}
	g.mu.Lock()
	g.hold = make(chan struct{})
	g.mu.Unlock()
	bobs := createSandbox(t, env, bobKey, map[string]any{"from": code})
	env.Eventually("scrub started", func() bool { return g.last(bobs+" POST /v1/commands", "authorized_keys") != "" })
	if r := env.Do(bobKey, "GET", "/api/v1/sandboxes/"+bobs, nil); r.Str("sandbox.state") != "provisioned" || r.Get("sandbox.securing") != true {
		t.Fatalf("usable before the scrub: %s", r)
	}
	if r := env.Do(bobKey, "GET", "/api/v1/sandboxes/"+bobs+"/files?path=/home/user/.aws/creds", nil); r.Code() != "sandbox_securing" {
		t.Fatalf("files while securing: %s", r)
	}
	g.mu.Lock()
	close(g.hold)
	g.hold = nil
	g.mu.Unlock()
	env.WaitState(bobKey, bobs, "ready")
	gc := guestSpec(t, node, bobs)
	if gc.Env["MINE"] != "" {
		t.Fatalf("saver's env leaked: %v", gc.Env)
	}
	sc := gc.Scrub
	if sc == nil || !sc.SSHKeys || !sc.AuthorizedKeys || !sc.GitHubLogout || !sc.History || !slices.Contains(sc.Files, ".aws/creds") ||
		!slices.Contains(sc.Files, ".claude/.credentials.json") || !slices.Contains(sc.Dirs, "app") || !slices.Contains(sc.RegistryLogouts, "reg.lan:5000") {
		t.Fatalf("scrub: %+v", sc)
	}
	cmd := g.last(bobs+" POST /v1/commands", "authorized_keys")
	for _, p := range []string{".aws/creds", "rm -rf -- \\\"$HOME\\\"/app", "docker logout", ".ssh/id_*"} {
		if !strings.Contains(cmd, p) {
			t.Fatalf("fallback scrub lacks %s: %s", p, cmd)
		}
	}
	// Nothing to scrub on the next boot.
	env.Do(bobKey, "POST", "/api/v1/sandboxes/"+bobs+"/stop", nil)
	env.WaitState(bobKey, bobs, "archived")
	env.Do(bobKey, "POST", "/api/v1/sandboxes/"+bobs+"/resume", nil)
	env.WaitState(bobKey, bobs, "ready")
	if gc := guestSpec(t, node, bobs); gc.Scrub != nil {
		t.Fatalf("scrubbed twice: %+v", gc.Scrub)
	}
}

// A scrub that keeps failing fails the boot instead of leaving the sandbox usable.
func TestScrubFailureIsError(t *testing.T) {
	g := newEnvRecGuest()
	fail := &failCommands{h: g}
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: fail})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	fail.set(true)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"noEnv": true})
	r := env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", 15*time.Second, "error")
	if !strings.HasPrefix(r.Str("sandbox.error"), "scrub_failed") {
		t.Fatalf("error: %s", r)
	}
}

type failCommands struct {
	h  http.Handler
	mu sync.Mutex
	on bool
}

func (f *failCommands) set(v bool) { f.mu.Lock(); f.on = v; f.mu.Unlock() }

func (f *failCommands) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	on := f.on
	f.mu.Unlock()
	if on && r.URL.Path == "/v1/commands" {
		api.WriteOK(w, 200, "command.result", map[string]any{"exitCode": 1, "stdout": "", "stderr": "boom"})
		return
	}
	f.h.ServeHTTP(w, r)
}

// SNAP-036: a sandbox with snapshots off is never snapshotted.
func TestSnapshotsOffNeverSaved(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"snapshots": false})
	env.WaitState(key, id, "ready")
	if sp, _ := node.Spec(id); !sp.NoSnapshots {
		t.Fatal("NoSnapshots not sent to the node")
	}
	snap, err := node.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	env.Eventually("node drops it", func() bool { return !node.HasSnapshot(snap) })
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/snapshots", nil); len(r.Get("snapshots").([]any)) != 0 {
		t.Fatalf("recorded: %s", r)
	}
	if _, err := env.S.Blobs.Stat("snapshot", snap); err == nil {
		t.Fatal("blob kept")
	}
	if hb := node.LastHeartbeatResponse(); hb.SnapshotIntervalSec != 60 {
		t.Fatalf("default snapshot interval %d", hb.SnapshotIntervalSec)
	}
}

// histGuest serves the guest's history export.
type histGuest struct {
	*servertest.FakeGuest
	mu     sync.Mutex
	acked  []string
	export map[string]proto.HistoryExport // sandbox -> pending export
}

func (g *histGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sb := r.Header.Get("X-Sandbox-Id")
	switch r.URL.Path {
	case "/v1/internal/history/export":
		g.mu.Lock()
		ex := g.export[sb]
		g.mu.Unlock()
		api.WriteOK(w, 200, "history.export", ex)
	case "/v1/internal/history/ack":
		var b struct{ Batch string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		g.mu.Lock()
		if ex, ok := g.export[sb]; ok && ex.Batch == b.Batch {
			delete(g.export, sb)
			g.acked = append(g.acked, b.Batch)
		}
		g.mu.Unlock()
		api.WriteOK(w, 200, "history.acked", nil)
	default:
		g.FakeGuest.ServeHTTP(w, r)
	}
}

// CMD-015..025, AGT-013: history, output, samples and agent events are
// readable while the sandbox is stopped.
func TestHistoryReadableWhileStopped(t *testing.T) {
	g := &histGuest{FakeGuest: servertest.NewFakeGuest(), export: map[string]proto.HistoryExport{}}
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	now := time.Now().UTC().Truncate(time.Millisecond)
	code := 0
	g.mu.Lock()
	g.export[id] = proto.HistoryExport{Batch: "b1",
		Records: []proto.CommandRecord{
			{CommandID: "cmd_a", SandboxID: id, Command: "echo hi", Mode: "sync", Status: "exited", ExitCode: &code, StartedAt: now.Add(-2 * time.Second)},
			{CommandID: "cmd_b", SandboxID: id, Command: "npm run dev", Mode: "detached", Status: "running", StartedAt: now.Add(-time.Second)},
		},
		Chunks: []proto.HistoryChunk{{CommandID: "cmd_a", Stream: "stdout", Offset: 0, Data: []byte("hel")}, {CommandID: "cmd_a", Stream: "stdout", Offset: 3, Data: []byte("lo\n")}},
		Samples: []proto.GuestSample{{Time: now, CPUUtilization: 0.5, MemoryUsedBytes: 1 << 30, MemoryTotalBytes: 8 << 30, DiskUsedBytes: 5 << 30, DiskTotalBytes: 50 << 30,
			Processes: []proto.ProcessInfo{{Pid: 42, Command: "node server.js", CPUPercent: 12}}}},
	}
	g.mu.Unlock()
	pt := now.UnixMilli()
	node.SendGuestEvent(id, "prompt.event", map[string]any{"id": "prompt_1", "type": "prompt", "timestamp": pt, "taskId": "prompt_1", "conversationId": "conv_1",
		"data": map[string]any{"prompt": "fix the bug", "status": "queued", "provider": "claude"}})
	node.SendGuestEvent(id, "prompt.event", map[string]any{"id": "pst_1", "type": "prompt.status", "timestamp": pt + 5, "taskId": "prompt_1", "conversationId": "conv_1",
		"data": map[string]any{"status": "completed"}})

	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	g.mu.Lock()
	acked := slices.Contains(g.acked, "b1")
	g.mu.Unlock()
	if !acked {
		t.Fatal("export not acked before the stop")
	}
	base := "/api/v1/sandboxes/" + id
	if r := env.Do(key, "GET", base+"/command-history", nil); r.Status != 200 || r.Str("commands.0.commandId") != "cmd_b" || r.Str("commands.1.commandId") != "cmd_a" {
		t.Fatalf("history: %s", r)
	}
	if r := env.Do(key, "GET", base+"/command-history?mode=sync&limit=1", nil); r.Str("commands.0.commandId") != "cmd_a" || len(r.Get("commands").([]any)) != 1 {
		t.Fatalf("history mode: %s", r)
	}
	if r := env.Do(key, "GET", base+"/command-history?limit=1", nil); r.Str("nextCursor") != "cmd_b" {
		t.Fatalf("cursor: %s", r)
	} else if r2 := env.Do(key, "GET", base+"/command-history?limit=1&cursor=cmd_b", nil); r2.Str("commands.0.commandId") != "cmd_a" {
		t.Fatalf("page 2: %s", r2)
	}
	if r := env.Do(key, "GET", base+"/command-history/cmd_a", nil); r.Str("command.command") != "echo hi" {
		t.Fatalf("record: %s", r)
	}
	if r := env.Do(key, "GET", base+"/command-history/cmd_a/logs?offset=1&limit=3", nil); r.Str("data") != "ell" || r.Str("nextOffset") != "4" || r.Str("size") != "6" || r.Get("done") != false {
		t.Fatalf("logs: %s", r)
	}
	if r := env.Do(key, "GET", base+"/command-history/cmd_a/logs", nil); r.Str("data") != "hello\n" || r.Get("done") != true {
		t.Fatalf("logs all: %s", r)
	}
	if r := env.Do(key, "GET", base+"/command-history/nope", nil); r.Status != 404 {
		t.Fatalf("unknown: %s", r)
	}
	if r := env.Do(key, "GET", base+"/processes", nil); r.Get("live") != false || r.Str("processes.0.pid") != "42" || r.Str("sampledAt") == "" {
		t.Fatalf("processes: %s", r)
	}
	if r := env.Do(key, "GET", base+"/events?sort=asc", nil); r.Str("events.0.id") != "prompt_1" || r.Str("events.0.data.status") != "completed" || r.Str("events.1.type") != "prompt.status" {
		t.Fatalf("events: %s", r)
	}
	if r := env.Do(key, "GET", base+"/events?limit=1", nil); r.Str("events.0.id") != "pst_1" || r.Get("pageInfo.hasMore") != true {
		t.Fatalf("events desc: %s", r)
	} else if r2 := env.Do(key, "GET", base+"/events?limit=1&cursor="+r.Str("pageInfo.nextCursor"), nil); r2.Str("events.0.id") != "prompt_1" {
		t.Fatalf("events page 2: %s", r2)
	}
	if r := env.Do(key, "GET", base+"/conversations", nil); r.Str("conversations.0.id") != "conv_1" || r.Str("conversations.0.prompts") != "1" || r.Str("conversations.0.lastHarness") != "claude" {
		t.Fatalf("conversations: %s", r)
	}
	if r := env.Do(key, "GET", base+"/prompts/prompt_1", nil); r.Str("promptRun.status") != "completed" || r.Get("promptRun.done") != true || r.Str("promptRun.finishedAt") == "" {
		t.Fatalf("prompt run: %s", r)
	}
	// Writes still need a running machine.
	if r := env.Do(key, "POST", base+"/commands", map[string]any{"command": "true"}); r.Code() != "machine_not_running" || r.Get("error.details.retryable") != false {
		t.Fatalf("exec while stopped: %s", r)
	}
	// Deleted with the sandbox.
	env.Do(key, "DELETE", base, nil, "X-CrowdVM-Confirm-Delete: "+id)
	env.Eventually("history purged", func() bool {
		var n int
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM command_history WHERE sandbox_id=?`, id).Scan(&n)
		return n == 0
	})
}

// LIFE-014: resuming a stopping sandbox keeps its machine.
func TestResumeWhileStopping(t *testing.T) {
	env := servertest.New(t)
	n1 := env.StartNode(servertest.NodeOptions{})
	n2 := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	r := env.WaitState(key, id, "ready")
	nodeID := r.Str("sandbox.nodeId")
	node := n1
	if nodeID == n2.ID {
		node = n2
	}
	// The save fails: the stop keeps retrying while the machine runs.
	node.FailNext("POST /v1/vms/" + id + "/stop")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.stop.status", 5*time.Second, "failing")
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	if r.Status != 202 || r.Get("stopCancelled") != true || r.Str("sandbox.state") != "ready" || r.Str("sandbox.nodeId") != nodeID {
		t.Fatalf("resume while stopping: %s", r)
	}
	if st, ok := node.VM(id); !ok || st.State != proto.VMRunning {
		t.Fatalf("machine replaced: %+v", st)
	}
	// Resizing while stopping still waits for the stop.
	node.FailNext("POST /v1/vms/" + id + "/stop")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.stop.status", 5*time.Second, "failing")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"type": "large"}); r.Code() != "sandbox_stopping" {
		t.Fatalf("resize while stopping: %s", r)
	}
	// Node offline mid-stop: the resume runs on the same node once the stop is done.
	node.Disconnect()
	env.Eventually("node offline", func() bool { return env.S.NodeConn(node.ID) == nil })
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status != 202 || r.Str("status") != "resuming" {
		t.Fatalf("resume during stop: %s", r)
	}
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	r = env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", 15*time.Second, "ready")
	if r.Str("sandbox.nodeId") != nodeID {
		t.Fatalf("resumed on %s, want %s", r.Str("sandbox.nodeId"), nodeID)
	}
}

// SNAP-015/017: a failed re-save keeps the last good disk.
func TestSavedSnapshotFailureKeepsLastGood(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "web"})
	good := env.WaitFor(key, "/api/v1/saved-snapshots/web", "snapshot.status", 5*time.Second, "ready").Str("snapshot.snapshotId")
	node.FailNext("POST /v1/vms/" + id + "/snapshot")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "web"}); r.Status != 202 {
		t.Fatalf("re-save: %s", r)
	}
	r := env.WaitFor(key, "/api/v1/saved-snapshots/web", "snapshot.status", 5*time.Second, "failed")
	if r.Str("snapshot.snapshotId") != good || r.Str("snapshot.error") == "" || r.Get("snapshot.available") != true {
		t.Fatalf("failed save: %s", r)
	}
	// Still deployable, and it can be saved again or removed.
	dep := createSandbox(t, env, key, map[string]any{"from": "web"})
	env.WaitState(key, dep, "ready")
	env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "web"})
	if r := env.WaitFor(key, "/api/v1/saved-snapshots/web", "snapshot.status", 5*time.Second, "ready"); r.Str("snapshot.snapshotId") == good {
		t.Fatalf("re-save kept the old disk: %s", r)
	}
}

// SNAP-022: team images belong to the organization.
func TestOrgSavedSnapshots(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	alice := env.AdminKey
	_, bob := env.NewUser("bob", false)
	_, carol := env.NewUser("carol", false)
	makeOrg(t, env, alice, "Acme", "bob@example.test", bob, "carol@example.test", carol)
	id := createSandbox(t, env, alice, map[string]any{"org": "acme"})
	env.WaitState(alice, id, "ready")
	if r := env.Do(alice, "POST", "/api/v1/saved-snapshots?org=acme", map[string]any{"sandboxId": id, "name": "img"}); r.Status != 202 || r.Str("snapshot.org") == "" {
		t.Fatalf("org save: %s", r)
	}
	env.WaitFor(alice, "/api/v1/saved-snapshots/img", "snapshot.status", 5*time.Second, "ready")
	// A personal snapshot with the same name does not collide.
	if r := env.Do(alice, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "img"}); r.Status != 202 || r.Get("snapshot.org") != nil {
		t.Fatalf("personal save: %s", r)
	}
	env.WaitFor(alice, "/api/v1/saved-snapshots/img", "snapshot.status", 5*time.Second, "ready")
	// A named scope lists only that wallet, like the by-name actions (opus-13 B1).
	for scope, wantOrg := range map[string]bool{"personal": false, "acme": true} {
		r := env.Do(alice, "GET", "/api/v1/saved-snapshots", nil, "X-CrowdVM-Org: "+scope)
		snaps, _ := r.Get("snapshots").([]any)
		if len(snaps) != 1 || (snaps[0].(map[string]any)["org"] != nil) != wantOrg {
			t.Fatalf("list scoped to %s: %s", scope, r)
		}
	}
	// --org personal finds your own one by name (opus-8 B1).
	if r := env.Do(alice, "GET", "/api/v1/saved-snapshots/img", nil, "X-CrowdVM-Org: personal"); r.Status != 200 || r.Get("snapshot.org") != nil {
		t.Fatalf("personal by name under --org personal: %s", r)
	}
	// --org acme new --from img deploys acme's image, not the personal one (opus-8 B2).
	orgImg := env.Do(alice, "GET", "/api/v1/saved-snapshots/img?org=acme", nil).Str("snapshot.id")
	fromOrg := createSandbox(t, env, alice, map[string]any{"org": "acme", "from": "img"})
	var srcID string
	_ = env.S.DB.QueryRow(`SELECT source_id FROM sandboxes WHERE id=?`, fromOrg).Scan(&srcID)
	if orgImg == "" || srcID != orgImg {
		t.Fatalf("--org acme --from img used %q, want the org image %q", srcID, orgImg)
	}
	// With acme active (org switch) and no --org, by-name actions act on the
	// org's image too, not the personal one (opus-9 B1).
	env.Do(alice, "PATCH", "/api/v1/orgs/active", map[string]any{"org": "acme"})
	if r := env.Do(alice, "GET", "/api/v1/saved-snapshots/img", nil); r.Str("snapshot.id") != orgImg {
		t.Fatalf("active org lookup: %s", r)
	}
	env.Do(alice, "PATCH", "/api/v1/orgs/active", map[string]any{"org": ""})
	// Members see and deploy it (scrubbed: alice saved it).
	if r := env.Do(bob, "GET", "/api/v1/saved-snapshots", nil); r.Str("snapshots.0.name") != "img" || r.Str("snapshots.0.org") == "" {
		t.Fatalf("member list: %s", r)
	}
	// Any member updates it.
	bsb := createSandbox(t, env, bob, map[string]any{"org": "acme"})
	env.WaitState(bob, bsb, "ready")
	if r := env.Do(bob, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": bsb, "name": "img", "org": "acme"}); r.Status != 202 {
		t.Fatalf("member update: %s", r)
	}
	r := env.WaitFor(bob, "/api/v1/saved-snapshots/img?org=acme", "snapshot.status", 5*time.Second, "ready")
	if r.Str("snapshot.sourceSandboxId") != bsb {
		t.Fatalf("member update: %s", r)
	}
	// Carol (neither saver nor admin) may not remove or share it.
	if r := env.Do(carol, "DELETE", "/api/v1/saved-snapshots/img?org=acme", nil); r.Status != 403 {
		t.Fatalf("member delete: %s", r)
	}
	if r := env.Do(carol, "POST", "/api/v1/saved-snapshots/img/shares?org=acme", nil); r.Status != 403 {
		t.Fatalf("member share: %s", r)
	}
	// The saver (bob) may.
	if r := env.Do(bob, "DELETE", "/api/v1/saved-snapshots/img?org=acme", nil); r.Status != 200 {
		t.Fatalf("saver delete: %s", r)
	}
	if r := env.Do(alice, "GET", "/api/v1/saved-snapshots/img", nil); r.Status != 200 || r.Get("snapshot.org") != nil {
		t.Fatalf("personal kept: %s", r)
	}
}

// ENV-007: secrets a newer version withholds are deleted at the next boot.
func TestWithheldSecretsDeletedOnResume(t *testing.T) {
	g := newEnvRecGuest()
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{
		"secretFiles":    []map[string]any{{"path": ".env", "contents": "S=1"}, {"path": "keep.txt", "contents": "k"}},
		"registryLogins": []map[string]any{{"registry": "reg.lan:5000", "username": "u", "password": "p"}},
	})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	env.Do(key, "DELETE", "/api/v1/environments/base/secret-files?path=.env", nil)
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"registryLogins": []any{}})
	env.Do(key, "POST", "/api/v1/environments/base/upgrade", nil)
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	gc := guestSpec(t, node, id)
	if gc.Scrub == nil || !slices.Contains(gc.Scrub.Files, ".env") || slices.Contains(gc.Scrub.Files, "keep.txt") || !slices.Contains(gc.Scrub.RegistryLogouts, "reg.lan:5000") || gc.Scrub.SSHKeys {
		t.Fatalf("stale scrub: %+v", gc.Scrub)
	}
	if cmd := g.last(id+" POST /v1/commands", "docker logout"); !strings.Contains(cmd, `rm -f -- \"$HOME\"/.env`) {
		t.Fatalf("fallback: %s", cmd)
	}
}

// LIFE-041: a sandbox whose node is lost is rebuilt on another node from its
// latest uploaded snapshot, and the owner is told.
func TestRebuildAfterNodeLoss(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.NodeLostGrace = 200 * time.Millisecond; c.NodeTimeout = time.Second })
	a := env.StartNode(servertest.NodeOptions{Name: "a"})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": "a"}})
	env.WaitState(key, id, "ready")
	snap, err := a.PeriodicSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	env.WaitFor(key, "/api/v1/sandboxes/"+id+"/snapshots/latest", "snapshot.uploaded", 5*time.Second, "true")
	b := env.StartNode(servertest.NodeOptions{Name: "b"})
	env.Do(key, "PATCH", "/api/v1/nodes/"+a.ID, map[string]any{"labels": map[string]string{}})
	a.Close()
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", 10*time.Second, "error")
	// The pin to node a would keep it waiting; drop it the way a user would
	// not need to: the test pins only to make the first placement certain.
	_, _ = env.S.DB.Exec(`UPDATE sandboxes SET placement='{}' WHERE id=?`, id)
	r := env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", 15*time.Second, "ready")
	if r.Str("sandbox.nodeId") != b.ID {
		t.Fatalf("rebuilt on %s", r.Str("sandbox.nodeId"))
	}
	if sp, _ := b.Spec(id); sp.ParentID != snap {
		t.Fatalf("rebuilt from %s, want %s", sp.ParentID, snap)
	}
	if r := env.Do(key, "GET", "/api/v1/notices", nil); !strings.Contains(string(r.Body), "rebuilt") {
		t.Fatalf("notices: %s", r)
	}
}

// SIZE-010: a resume to a type smaller than the data is refused; disk
// alerts reach the owner (LIFE-044).
func TestTypeTooSmallAndDiskAlert(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"type": "large"})
	env.WaitState(key, id, "ready")
	node.SendGuestEvent(id, "disk.usage", map[string]any{"level": 95, "percent": 96, "usedBytes": int64(100) << 30, "totalBytes": int64(104) << 30})
	env.Eventually("notice", func() bool {
		return strings.Contains(string(env.Do(key, "GET", "/api/v1/notices", nil).Body), "disk_usage_95")
	})
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"type": "small"}); r.Status != 409 || r.Code() != "type_too_small" {
		t.Fatalf("shrink: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "archived" || r.Str("sandbox.type") != "large" {
		t.Fatalf("changed: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"type": "xlarge"}); r.Status != 202 {
		t.Fatalf("grow: %s", r)
	}
}

// ENV-015: the neutral sandbox token exists even when the CLI key is withheld,
// and it only reaches its own sandbox.
func TestNeutralSandboxToken(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"noEnv": true})
	other := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	gc := guestSpec(t, node, id)
	tok := gc.Env["ASCII_TOKEN"]
	if tok == "" || gc.Env["CROWDVM_TOKEN"] != tok || gc.Env["CROWDVM_API_KEY"] != "" {
		t.Fatalf("env: %v", gc.Env)
	}
	if r := env.Do(tok, "GET", "/api/v1/sandboxes/"+id, nil); r.Status != 200 {
		t.Fatalf("own sandbox: %s", r)
	}
	if r := env.Do(tok, "GET", "/api/v1/sandboxes/"+other, nil); r.Status == 200 {
		t.Fatalf("other sandbox: %s", r)
	}
	if r := env.Do(tok, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "true"}); r.Status != 403 {
		t.Fatalf("exec with the neutral token: %s", r)
	}
}

// applyGuest implements the guest's POST /v1/internal/apply-config.
type applyGuest struct {
	*envRecGuest
	mu   sync.Mutex
	last proto.ApplyConfig
	n    int
}

func (g *applyGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/internal/apply-config" {
		g.mu.Lock()
		g.last = proto.ApplyConfig{}
		_ = json.NewDecoder(r.Body).Decode(&g.last)
		g.n++
		g.mu.Unlock()
		api.WriteOK(w, 200, "guest.config_applied", nil)
		return
	}
	g.envRecGuest.ServeHTTP(w, r)
}

// ENV-007: a live upgrade goes through the guest's apply-config (env patch,
// full secret-file and registry replacement, scrub of withheld files).
func TestLiveUpgradeApplyConfig(t *testing.T) {
	g := &applyGuest{envRecGuest: newEnvRecGuest()}
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{
		"envContents":    "FOO=1\nGONE=x\n",
		"secretFiles":    []map[string]any{{"path": ".env", "contents": "S=1"}},
		"registryLogins": []map[string]any{{"registry": "reg.lan:5000", "username": "u", "password": "p"}},
	})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"envContents": "FOO=2\n", "registryLogins": []any{},
		"secretFiles": []map[string]any{{"path": "new.json", "contents": "{}"}}})
	if r := env.Do(key, "POST", "/api/v1/environments/base/upgrade", nil); r.Str("upgradedLive") != "1" {
		t.Fatalf("upgrade: %s", r)
	}
	g.mu.Lock()
	ac, n := g.last, g.n
	g.mu.Unlock()
	if n != 1 || ac.EnvSet["FOO"] != "2" || !slices.Contains(ac.EnvUnset, "GONE") || ac.Env != nil {
		t.Fatalf("env patch: %+v", ac)
	}
	if len(ac.SecretFiles) != 1 || ac.SecretFiles[0].Path != "new.json" || ac.RegistryLogins == nil || len(ac.RegistryLogins) != 0 {
		t.Fatalf("replacements: %+v", ac)
	}
	if ac.Scrub == nil || !slices.Contains(ac.Scrub.Files, ".env") || slices.Contains(ac.Scrub.Files, "new.json") {
		t.Fatalf("scrub: %+v", ac.Scrub)
	}
	if g.envRecGuest.last("PUT /v1/files/raw?path=%2Fhome%2Fuser%2F.bashrc") != "" {
		t.Fatal("fell back to the bashrc rewrite")
	}
}

// DASH-004: the stored 30 s samples come back as a time series.
func TestSandboxMetrics(t *testing.T) {
	g := servertest.NewFakeGuest()
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Minute)
	var smp []proto.GuestSample
	for i := 0; i < 4; i++ { // 0, 30, 60, 90 s
		smp = append(smp, proto.GuestSample{Time: base.Add(time.Duration(i) * 30 * time.Second), CPUUtilization: float64(i) / 10,
			MemoryUsedBytes: int64(i+1) << 20, MemoryTotalBytes: 8 << 30, DiskUsedBytes: 5 << 30, DiskTotalBytes: 50 << 30, IOPressure: 0.02})
	}
	g.AddHistory(id, proto.HistoryExport{Samples: smp})
	env.Eventually("samples pulled", func() bool { return g.HistoryAcked(id) > 0 })

	from := base.Format(time.RFC3339)
	r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/metrics?from="+from+"&step=60", nil)
	if r.Status != 200 || r.Str("type") != "sandbox.metrics" || r.Str("stepSeconds") != "60" || len(r.Get("points").([]any)) != 2 {
		t.Fatalf("metrics: %s", r)
	}
	if !strings.HasPrefix(r.Str("points.0.at"), base.Format("2006-01-02T15:04:05")) || r.Str("points.0.samples") != "2" || r.Get("points.0.cpuUtilization").(float64) != 0.05 ||
		r.Get("points.1.memoryUsedBytes").(float64) != 3.5*(1<<20) || r.Get("points.1.diskTotalBytes").(float64) != 50<<30 || r.Get("live") != true {
		t.Fatalf("points: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/metrics", nil); r.Status != 200 || len(r.Get("points").([]any)) == 0 || r.Str("stepSeconds") != "30" {
		t.Fatalf("default range: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/metrics?step=5", nil); r.Code() != "invalid_step" {
		t.Fatalf("step: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/metrics?from=x", nil); r.Code() != "invalid_range" {
		t.Fatalf("range: %s", r)
	}
	off := createSandbox(t, env, key, map[string]any{"monitoring": false})
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+off+"/metrics", nil); r.Code() != "monitoring_off" {
		t.Fatalf("monitoring off: %s", r)
	}
}

// CMD-012: a guest older than the server's minimum answers 409 agent_outdated.
func TestAgentOutdated(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.MinGuestVersion = "1.2.0" })
	old := env.StartNode(servertest.NodeOptions{GuestVersion: "v1.1.9", Labels: map[string]string{"n": "old"}})
	env.StartNode(servertest.NodeOptions{GuestVersion: "1.2.0", Labels: map[string]string{"n": "new"}})
	key := env.AdminKey
	a := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": old.ID}})
	env.WaitState(key, a, "ready")
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+a+"/commands", map[string]any{"command": "echo hi"})
	if r.Status != 409 || r.Code() != "agent_outdated" || r.Str("error.details.guestVersion") != "v1.1.9" {
		t.Fatalf("old guest: %s", r)
	}
	b := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"labels": map[string]string{"n": "new"}}})
	env.WaitState(key, b, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+b+"/commands", map[string]any{"command": "echo hi"}); r.Status != 200 {
		t.Fatalf("current guest: %s", r)
	}
}

// URL-012: public hosted ports and requested TCP/UDP ports are published on
// the node's direct host, and sent again at every boot.
func TestDirectPorts(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{DirectHost: "192.168.1.5", Labels: map[string]string{"d": "1"}})
	plain := env.StartNode(servertest.NodeOptions{Labels: map[string]string{"d": "0"}})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": node.ID}})
	env.WaitState(key, id, "ready")
	base := "/api/v1/sandboxes/" + id
	if r := env.Do(key, "POST", base+"/host", map[string]any{"port": 3000, "public": true}); r.Status != 200 || r.Str("directEndpoint") != "192.168.1.5:3000" {
		t.Fatalf("host --public: %s", r)
	}
	if r := env.Do(key, "POST", base+"/host", map[string]any{"port": 4000}); r.Status != 200 || r.Get("directEndpoint") != nil {
		t.Fatalf("protected port published: %s", r)
	}
	if r := env.Do(key, "PUT", base+"/direct-ports", map[string]any{"ports": []any{map[string]any{"port": 7070}}}); r.Code() != "reserved_port" {
		t.Fatalf("agent port: %s", r)
	}
	r := env.Do(key, "PUT", base+"/direct-ports", map[string]any{"ports": []any{map[string]any{"port": 5353, "proto": "udp"}}})
	if r.Status != 200 || r.Str("directHost") != "192.168.1.5" || len(r.Get("ports").([]any)) != 2 || r.Str("ports.1.endpoint") != "192.168.1.5:5353" || r.Str("ports.1.source") != "requested" {
		t.Fatalf("put: %s", r)
	}
	// Converges: a heartbeat sent before the PUT may briefly carry the old set.
	env.WaitFor(key, base, "sandbox.directEndpoints.udp/5353", servertest.WaitTimeout, "192.168.1.5:5353")
	env.Do(key, "DELETE", base+"/host/3000", nil)
	if r := env.Do(key, "GET", base+"/direct-ports", nil); len(r.Get("ports").([]any)) != 1 {
		t.Fatalf("hidden port still published: %s", r)
	}
	env.Do(key, "POST", base+"/host", map[string]any{"port": 3000, "public": true})
	env.Do(key, "POST", base+"/stop", nil)
	env.WaitState(key, id, "archived")
	env.Do(key, "POST", base+"/resume", nil)
	env.WaitState(key, id, "ready")
	if sp, _ := node.Spec(id); len(sp.DirectPorts) != 2 || sp.DirectPorts[0] != (proto.DirectPort{Port: 3000, Proto: "tcp"}) || sp.DirectPorts[1].Proto != "udp" {
		t.Fatalf("boot spec: %+v", sp.DirectPorts)
	}
	// A node without a direct host: hosting still works, direct ports are refused.
	other := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": plain.ID}})
	env.WaitState(key, other, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+other+"/host", map[string]any{"port": 3000, "public": true}); r.Status != 200 || r.Get("directEndpoint") != nil {
		t.Fatalf("host on plain node: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/sandboxes/"+other+"/direct-ports", map[string]any{"ports": []any{map[string]any{"port": 25565}}}); r.Code() != "direct_disabled" {
		t.Fatalf("plain node: %s", r)
	}
}

// LIFE-048: POST /commands keeps Boat's shape (no per-call env).
func TestCommandEnvRefused(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "echo $X", "env": map[string]string{"X": "1"}}); r.Status != 400 || r.Str("error.details.field") != "env" {
		t.Fatalf("env: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "echo $X", "Env": map[string]string{"X": "1"}}); r.Status != 400 {
		t.Fatalf("Env: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "export X=1; echo $X"}); r.Status != 200 || r.Str("stdout") != "export X=1; echo $X\n" {
		t.Fatalf("plain: %s", r)
	}
}

// LIFE-025: a create that never found room ends "cancelled" (not error);
// resume retries it.
func TestCreateCancelled(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.CapacityWait = 300 * time.Millisecond })
	node := env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 16, MemoryMiB: 65536, DiskGiB: 1000, MaxSandboxes: 1}})
	key := env.AdminKey
	a := createSandbox(t, env, key, nil)
	env.WaitState(key, a, "ready")
	b := createSandbox(t, env, key, nil)
	r := env.WaitState(key, b, "cancelled")
	if !strings.HasPrefix(r.Str("sandbox.error"), "out_of_capacity") {
		t.Fatalf("cancelled: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+b+"/stop", nil); r.Status != 409 {
		t.Fatalf("stop cancelled: %s", r)
	}
	// A fork that never got room keeps its source's disk for the resume.
	env.Do(key, "POST", "/api/v1/sandboxes/"+a+"/stop", nil)
	env.WaitState(key, a, "archived")
	src := env.Do(key, "GET", "/api/v1/sandboxes/"+a, nil)
	env.Do(key, "POST", "/api/v1/sandboxes/"+b+"/resume", nil) // takes the only slot
	env.WaitState(key, b, "ready")
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+a+"/fork", nil)
	fk := r.Str("sandbox.id")
	if r.Status != 202 || fk == "" {
		t.Fatalf("fork: %s", r)
	}
	env.WaitState(key, fk, "cancelled")
	env.Do(key, "DELETE", "/api/v1/sandboxes/"+b, nil, "X-CrowdVM-Confirm-Delete: "+b)
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+fk+"/resume", nil); r.Status != 202 {
		t.Fatalf("resume cancelled fork: %s", r)
	}
	env.WaitState(key, fk, "ready")
	if sp, _ := node.Spec(fk); sp.ParentID == "" || sp.Guest.Boot != proto.BootFork || sp.ParentID != src.Str("sandbox.headSnapshotId") && src.Str("sandbox.headSnapshotId") != "" {
		t.Fatalf("fork boot: parent %q kind %s", sp.ParentID, sp.Guest.Boot)
	}
}

// SNAP-002: completed/available follow the stored head snapshot, verified
// its checksum; an upload in flight is in_progress.
func TestSnapshotStatusFields(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Labels: map[string]string{"up": "1"}})
	env.StartNode(servertest.NodeOptions{NoUpload: true, Labels: map[string]string{"up": "0"}})
	key := env.AdminKey
	ok := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"labels": map[string]string{"up": "1"}}})
	env.WaitState(key, ok, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+ok+"/stop", nil)
	env.WaitState(key, ok, "archived")
	env.Eventually("verified", func() bool {
		r := env.Do(key, "GET", "/api/v1/sandboxes/"+ok, nil)
		return r.Get("sandbox.snapshotAvailable") == true && r.Get("sandbox.snapshotVerifiedAt") != nil && r.Get("sandbox.snapshotCompletedAt") != nil &&
			r.Str("sandbox.lastSnapshotStatus") == "completed"
	})
	slow := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"labels": map[string]string{"up": "0"}}})
	env.WaitState(key, slow, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+slow+"/stop", nil)
	r := env.WaitState(key, slow, "archived")
	if r.Get("sandbox.snapshotAvailable") != false || r.Get("sandbox.snapshotCompletedAt") != nil || r.Get("sandbox.snapshotVerifiedAt") != nil || r.Str("sandbox.lastSnapshotStatus") != "in_progress" {
		t.Fatalf("not uploaded: %s", r)
	}
}

// SEC-014: placement.trust "own" keeps a sandbox on its owner's nodes,
// "trusted" also allows nodes an administrator marked trusted.
func TestPlacementTrust(t *testing.T) {
	env := servertest.New(t)
	adminNode := env.StartNode(servertest.NodeOptions{Name: "admin-box"})
	bob, bobKey := env.NewUser("bob", false)
	_, carolKey := env.NewUser("carol", false)
	_, tok, err := env.S.CreateJoinToken(server.JoinTokenParams{Name: "bob", CreatedBy: bob.ID, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	bobNode := env.StartNode(servertest.NodeOptions{Name: "bob-box", JoinToken: tok})
	own := map[string]any{"noEnv": true, "placement": map[string]any{"trust": "own"}}
	for _, c := range []struct{ key, node string }{{env.AdminKey, adminNode.ID}, {bobKey, bobNode.ID}} {
		id := createSandbox(t, env, c.key, own)
		if r := env.WaitState(c.key, id, "ready"); r.Str("sandbox.nodeId") != c.node {
			t.Fatalf("own placement: %s", r)
		}
	}
	if r := env.Do(carolKey, "POST", "/api/v1/sandboxes", own); r.Status != 503 || r.Str("error.details.reason") != "no_matching_node" {
		t.Fatalf("carol own: %s", r)
	}
	trusted := map[string]any{"noEnv": true, "placement": map[string]any{"trust": "trusted"}}
	if r := env.Do(carolKey, "POST", "/api/v1/sandboxes", trusted); r.Status != 503 {
		t.Fatalf("carol trusted before: %s", r)
	}
	if r := env.Do(bobKey, "PATCH", "/api/v1/nodes/"+bobNode.ID, map[string]any{"trust": "trusted"}); r.Status != 403 {
		t.Fatalf("owner trusts own node: %s", r)
	}
	if r := env.Do(env.AdminKey, "PATCH", "/api/v1/nodes/"+adminNode.ID, map[string]any{"trust": "trusted"}); r.Status != 200 || r.Str("node.trust") != "trusted" {
		t.Fatalf("admin trust: %s", r)
	}
	id := createSandbox(t, env, carolKey, trusted)
	if r := env.WaitState(carolKey, id, "ready"); r.Str("sandbox.nodeId") != adminNode.ID {
		t.Fatalf("carol trusted: %s", r)
	}
	if r := env.Do(carolKey, "POST", "/api/v1/sandboxes", map[string]any{"placement": map[string]any{"trust": "mine"}}); r.Code() != "invalid_placement" {
		t.Fatalf("bad trust: %s", r)
	}
}

// LIFE-013: a lazy-chain node boots without the pre-fetch; the sandbox is
// usable but not hydrated until the node reports the chain local.
func TestLazyChainHydration(t *testing.T) {
	env := servertest.New(t)
	a := env.StartNode(servertest.NodeOptions{Labels: map[string]string{"n": "a"}})
	key := env.AdminKey
	id := createSandbox(t, env, key, map[string]any{"placement": map[string]any{"node": a.ID}})
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	env.Eventually("stored", func() bool {
		return env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil).Get("sandbox.snapshotAvailable") == true
	})
	a.Close()
	lazy := env.StartNode(servertest.NodeOptions{LazyChain: true})
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", map[string]any{"placement": map[string]any{}}); r.Status != 202 {
		t.Fatalf("resume: %s", r)
	}
	r := env.WaitState(key, id, "ready")
	if r.Str("sandbox.nodeId") != lazy.ID || r.Get("sandbox.hydrated") != false || r.Get("sandbox.hydratedAt") != nil {
		t.Fatalf("lazy boot: %s", r)
	}
	if lazy.Called("POST /v1/snapshots/") {
		t.Fatal("chain pre-fetched on a lazy node")
	}
	if err := lazy.FinishHydration(id); err != nil {
		t.Fatal(err)
	}
	env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.hydrated", servertest.WaitTimeout, "true")
}

// LIFE-018: a deploy's recorded playbook goes with the saved snapshot to
// later deploys of the same wallet.
func TestSavedSnapshotPlaybook(t *testing.T) {
	g := servertest.NewFakeGuest()
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	src := createSandbox(t, env, key, nil)
	env.WaitState(key, src, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": src, "name": "stack"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/stack", "snapshot.status", servertest.WaitTimeout, "ready", "saved")
	d1 := createSandbox(t, env, key, map[string]any{"from": "stack"})
	env.WaitState(key, d1, "ready")
	env.Do(key, "PUT", "/api/v1/sandboxes/"+d1+"/files", map[string]any{"path": "/home/user/.crowdvm/playbook.json", "content": `{"paths":["/usr/bin/node","/home/user/app/package.json"]}`})
	node.SendGuestEvent(d1, "playbook.recorded", map[string]any{"path": "/home/user/.crowdvm/playbook.json", "paths": 2, "recorded": 2})
	env.Eventually("playbook learned", func() bool {
		var pb sql.NullString
		_ = env.S.DB.QueryRow(`SELECT playbook FROM saved_snapshots WHERE name='stack'`).Scan(&pb)
		return pb.Valid
	})
	d2 := createSandbox(t, env, key, map[string]any{"from": "stack"})
	env.WaitState(key, d2, "ready")
	if sp, _ := node.Spec(d2); len(sp.Guest.Playbook) != 2 || sp.Guest.Playbook[0] != "/usr/bin/node" {
		t.Fatalf("playbook not passed: %v", sp.Guest.Playbook)
	}
}

// CMD-020: stored output is capped per wallet and hour (commands and other
// processes separately); the rest is dropped and the record says so.
func TestOutputCap(t *testing.T) {
	g := servertest.NewFakeGuest()
	env := servertest.New(t, func(c *server.Config) { c.OutputCapPerHour = 10 })
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	now := time.Now().UTC()
	g.AddHistory(id, proto.HistoryExport{
		Records: []proto.CommandRecord{{CommandID: "cmd_a", Command: "yes", Mode: "sync", Status: "exited", StartedAt: now},
			{CommandID: "proc_1", Command: "nginx", Mode: "process", Status: "running", StartedAt: now}},
		Chunks: []proto.HistoryChunk{{CommandID: "cmd_a", Stream: "stdout", Offset: 0, Data: []byte("hello ")}, {CommandID: "cmd_a", Stream: "stdout", Offset: 6, Data: []byte("world!")},
			{CommandID: "proc_1", Stream: "stdout", Offset: 0, Data: []byte("started")}},
	})
	env.Eventually("pulled", func() bool { return g.HistoryAcked(id) > 0 })
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	base := "/api/v1/sandboxes/" + id + "/command-history/"
	if r := env.Do(key, "GET", base+"cmd_a", nil); r.Get("command.outputTruncated") != true {
		t.Fatalf("record: %s", r)
	}
	if r := env.Do(key, "GET", base+"cmd_a/logs", nil); r.Str("data") != "hello " {
		t.Fatalf("logs: %s", r)
	}
	if r := env.Do(key, "GET", base+"proc_1/logs", nil); r.Str("data") != "started" { // its own budget
		t.Fatalf("process logs: %s", r)
	}
}

// A fork waiting for a node still boots when its source is deleted meanwhile:
// the source's last snapshot is kept for it (opus-11 B5).
func TestQueuedForkSurvivesSourceDelete(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 4, MemoryMiB: 8192, DiskGiB: 200, MaxSandboxes: 4}})
	key := env.AdminKey
	src := createSandbox(t, env, key, nil)
	env.WaitState(key, src, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+src+"/stop", nil)
	env.WaitState(key, src, "archived")
	filler := createSandbox(t, env, key, nil) // the node is full now
	env.WaitState(key, filler, "ready")
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+src+"/fork", nil)
	if r.Status != 202 {
		t.Fatalf("fork: %s", r)
	}
	fork := r.Str("id")
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+fork, nil); r.Get("sandbox.waitingForCapacity") != true {
		t.Fatalf("fork not queued: %s", r)
	}
	env.Do(key, "DELETE", "/api/v1/sandboxes/"+src, nil, "X-Ascii-Confirm-Delete: "+src)
	env.S.GCNow()
	env.Do(key, "DELETE", "/api/v1/sandboxes/"+filler, nil, "X-Ascii-Confirm-Delete: "+filler)
	env.WaitState(key, fork, "ready")
}

// A paused node takes no new sandboxes: failFast answers 503 at once, a
// plain create waits for it (its owner can resume it) (sonnet-12 B1,
// opus-15 B1). A size no node can ever host is refused for good, not
// retried for minutes (sonnet-12 B2).
func TestPausedNodeAndImpossibleSize(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	env.StartNode(servertest.NodeOptions{Name: "paused", Paused: true})
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"failFast": true}); r.Status != 503 || r.Code() != "no_ready_machine" {
		t.Fatalf("failFast on a paused node: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "large"}); r.Status != 202 || r.Get("sandbox.waitingForCapacity") != true {
		t.Fatalf("large with only a paused big node should wait: %s", r)
	}
	env2 := servertest.New(t)
	key2 := env2.AdminKey
	small := proto.Capacity{VCPU: 4, MemoryMiB: 8192, DiskGiB: 200, MaxSandboxes: 4}
	env2.StartNode(servertest.NodeOptions{Name: "paused-small", Paused: true, Capacity: small})
	env2.StartNode(servertest.NodeOptions{Name: "small", Capacity: small})
	r := env2.Do(key2, "POST", "/api/v1/sandboxes", map[string]any{"type": "large"})
	if r.Status != 409 || r.Code() != "size_unavailable" || !strings.Contains(r.String(), `"availableTypes":["small","default"]`) {
		t.Fatalf("impossible size: %s", r)
	}
	if r := env2.Do(key2, "POST", "/api/v1/sandboxes", map[string]any{"failFast": true}); r.Status != 202 || r.Str("sandbox.nodeId") == "" {
		t.Fatalf("create on the unpaused node: %s", r)
	}
}

// A deploy from a saved snapshot that is stopped while it still waits for a
// node, then resumed, boots the saved snapshot, not an empty disk (sonnet-14).
func TestQueuedSavedDeployStopResume(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 4, MemoryMiB: 8192, DiskGiB: 200, MaxSandboxes: 4}})
	key := env.AdminKey
	src := createSandbox(t, env, key, nil)
	env.WaitState(key, src, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": src, "name": "qimg"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/qimg", "snapshot.status", 5*time.Second, "ready")
	snapID := ""
	_ = env.S.DB.QueryRow(`SELECT snapshot_id FROM saved_snapshots WHERE name='qimg'`).Scan(&snapID)
	// The node is full (src runs): the deploy waits.
	r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"from": "qimg"})
	if r.Status != 202 || r.Get("sandbox.waitingForCapacity") != true {
		t.Fatalf("deploy not queued: %s", r)
	}
	dep := r.Str("sandbox.id")
	env.Do(key, "POST", "/api/v1/sandboxes/"+dep+"/stop", nil)
	env.WaitState(key, dep, "archived")
	env.Do(key, "DELETE", "/api/v1/sandboxes/"+src, nil, "X-Ascii-Confirm-Delete: "+src)
	env.Do(key, "POST", "/api/v1/sandboxes/"+dep+"/resume", nil)
	env.WaitState(key, dep, "ready")
	if spec, ok := node.Spec(dep); !ok || spec.ParentID != snapID {
		t.Fatalf("resumed deploy booted from %q, want the saved snapshot %q", spec.ParentID, snapID)
	}
}

// bugs-r3-opus 15: a command run while the sandbox was private stays
// private to its creator when its record reaches the server after a share.
func TestHistoryPrivateBeforeShare(t *testing.T) {
	g := &histGuest{FakeGuest: servertest.NewFakeGuest(), export: map[string]proto.HistoryExport{}}
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	r := env.Do(key, "POST", "/api/v1/orgs", map[string]any{"name": "historg"})
	org := r.Str("org.id")
	if org == "" {
		org = r.Str("organization.id")
	}
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	_, _ = env.S.DB.Exec(`UPDATE sandboxes SET org_id=(SELECT id FROM orgs WHERE name='historg'), visibility='private', private_by_creator=1 WHERE id=?`, id)
	before := time.Now().UTC().Add(-time.Second)
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/share", nil); r.Status != 200 {
		t.Fatalf("share: %s", r)
	}
	after := time.Now().UTC().Add(time.Second)
	g.mu.Lock()
	g.export[id] = proto.HistoryExport{Batch: "b1", Records: []proto.CommandRecord{
		{CommandID: "cmd_priv", SandboxID: id, Command: "cat secret", Mode: "sync", Status: "exited", StartedAt: before},
		{CommandID: "cmd_pub", SandboxID: id, Command: "ls", Mode: "sync", Status: "exited", StartedAt: after},
	}}
	g.mu.Unlock()
	stopUntilArchived(t, env, key, id)
	for cmd, want := range map[string]int{"cmd_priv": 1, "cmd_pub": 0} {
		var p int
		if err := env.S.DB.QueryRow(`SELECT private FROM command_history WHERE sandbox_id=? AND command_id=?`, id, cmd).Scan(&p); err != nil || p != want {
			t.Fatalf("%s: private=%d (err %v), want %d", cmd, p, err, want)
		}
	}
}

// bugs-r4-opus 7: a guest clock ahead of the server's does not make a
// command from the private period public.
func TestHistoryPrivacyGuestClockAhead(t *testing.T) {
	g := &histGuest{FakeGuest: servertest.NewFakeGuest(), export: map[string]proto.HistoryExport{}}
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	env.Do(key, "POST", "/api/v1/orgs", map[string]any{"name": "skeworg"})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	_, _ = env.S.DB.Exec(`UPDATE sandboxes SET org_id=(SELECT id FROM orgs WHERE name='skeworg'), visibility='private', private_by_creator=1 WHERE id=?`, id)
	ran := time.Now()
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/share", nil); r.Status != 200 {
		t.Fatalf("share: %s", r)
	}
	ahead := time.Hour // the guest's clock
	g.mu.Lock()
	g.export[id] = proto.HistoryExport{Batch: "b1", Now: time.Now().Add(ahead), Records: []proto.CommandRecord{
		{CommandID: "cmd_priv", SandboxID: id, Command: "cat secret", Mode: "sync", Status: "exited", StartedAt: ran.Add(-time.Second).Add(ahead)},
	}}
	g.mu.Unlock()
	stopUntilArchived(t, env, key, id)
	var p int
	if err := env.S.DB.QueryRow(`SELECT private FROM command_history WHERE sandbox_id=? AND command_id='cmd_priv'`, id).Scan(&p); err != nil || p != 1 {
		t.Fatalf("private=%d (err %v), want 1", p, err)
	}
}

// bugs-r5-opus 2: a fork of a running sandbox that waits for a node copies
// the source as it was at the fork call, not when a node frees up.
func TestQueuedLiveForkSnapshotsAtCall(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Capacity: proto.Capacity{VCPU: 4, MemoryMiB: 8192, DiskGiB: 200, MaxSandboxes: 4}})
	key := env.AdminKey
	src := createSandbox(t, env, key, nil) // fills the node
	env.WaitState(key, src, "ready")
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+src+"/fork", nil)
	if r.Status != 202 {
		t.Fatalf("fork: %s", r)
	}
	fork := r.Str("id")
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+fork, nil); r.Get("sandbox.waitingForCapacity") != true {
		t.Fatalf("fork not queued: %s", r)
	}
	var snap string
	if err := env.S.DB.QueryRow(`SELECT id FROM snapshots WHERE sandbox_id=? AND kind='live'`, src).Scan(&snap); err != nil {
		t.Fatal("no live snapshot taken at the fork call")
	}
	env.Do(key, "POST", "/api/v1/sandboxes/"+src+"/stop", nil) // frees the node
	env.WaitState(key, fork, "ready")
	if sp, _ := node.Spec(fork); sp.ParentID != snap {
		t.Fatalf("fork booted from %q, want the snapshot of the fork call %s", sp.ParentID, snap)
	}
}
