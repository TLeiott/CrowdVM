package server_test

// Tests for the server-side findings of docs/reviews/*.md (refused stops,
// unresponsive sandboxes, node-loss webhooks, expired snapshot data,
// capacity refusals, API key management, error codes, org suspension, the
// outbound guard, deletion operations, docs for agents and Gitea CI).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// LIFE-034/SNAP-008: a stop whose save fails is refused before the guest is
// touched; the sandbox stays usable, unmetered and the stop retries.
func TestRefusedStopKeepsSandboxUsable(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")

	node.FailNext("POST /v1/vms/" + id + "/snapshot")
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	if r.Status != 202 {
		t.Fatalf("stop: %s", r)
	}
	stopID := r.Str("stop.id")
	r = env.WaitFor(key, "/api/v1/sandboxes/"+id+"/stops/"+stopID, "stop.status", 5*time.Second, "failing")
	if r.Str("stop.error") == "" {
		t.Fatalf("failing stop without error: %s", r)
	}
	r = env.WaitState(key, id, "ready")
	if r.Str("sandbox.billingPaused") != "stop_refused" {
		t.Fatalf("billing not paused: %s", r)
	}
	if node.Called("POST /v1/vms/" + id + "/stop") {
		t.Fatal("the guest was stopped (prepare-stop) although the save failed")
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/commands", map[string]any{"command": "echo hi"}); r.Status != 200 {
		t.Fatalf("sandbox not usable during a refused stop: %s", r)
	}
	// A second stop returns the waiting one.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Str("stop.id") != stopID {
		t.Fatalf("second stop: %s", r)
	}
	// A resume cancels the stop and meters again.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status != 202 {
		t.Fatalf("resume: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/stops/"+stopID, nil); r.Str("stop.status") != "superseded" {
		t.Fatalf("resume must cancel the refused stop: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Get("sandbox.billingPaused") != nil || r.Str("sandbox.state") != "ready" {
		t.Fatalf("after resume: %s", r)
	}

	// Refused again; the retry (save works now) stops it for real.
	node.FailNext("POST /v1/vms/" + id + "/snapshot")
	stopID = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil).Str("stop.id")
	env.WaitFor(key, "/api/v1/sandboxes/"+id+"/stops/"+stopID, "stop.status", 5*time.Second, "failing")
	r = env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.state", 15*time.Second, "archived")
	if r.Get("sandbox.billingPaused") != nil {
		t.Fatalf("pause kept after the stop: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/stops/"+stopID, nil); r.Str("stop.status") != "completed" {
		t.Fatalf("retried stop: %s", r)
	}
}

// LIFE-040: an unresponsive sandbox pauses billing and notifies its owner.
func TestNotAnsweringPausesBilling(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	// The boot's own "guest ready" event can arrive after a heartbeat already
	// made the sandbox ready: report "not answering" until it sticks.
	var r *servertest.Resp
	env.Eventually("degraded", func() bool {
		node.SetVMState(id, proto.VMRunning, false, "")
		time.Sleep(50 * time.Millisecond)
		r = env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil)
		return r.Str("sandbox.health") == "degraded"
	})
	if r.Str("sandbox.billingPaused") != "not_answering" {
		t.Fatalf("billing not paused: %s", r)
	}
	n := env.Do(key, "GET", "/api/v1/notices", nil)
	if !strings.Contains(string(n.Body), "not_answering") {
		t.Fatalf("no notice: %s", n)
	}
	node.SetVMState(id, proto.VMRunning, true, "")
	r = env.WaitFor(key, "/api/v1/sandboxes/"+id, "sandbox.health", 5*time.Second, "ok")
	if r.Get("sandbox.billingPaused") != nil {
		t.Fatalf("pause kept after recovery: %s", r)
	}
}

// HOOK-006: node loss emits sandbox.error (and degraded); its return emits
// sandbox.ready and sandbox.recovered.
func TestNodeLossWebhookEvents(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	var mu sync.Mutex
	var events []string
	env.S.Bus.Subscribe(func(e server.Event) {
		mu.Lock()
		events = append(events, e.Type+":"+e.Reason)
		mu.Unlock()
	})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	has := func(ev string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range events {
			if strings.HasPrefix(e, ev) {
				return true
			}
		}
		return false
	}
	mu.Lock()
	events = nil
	mu.Unlock()
	node.Disconnect()
	env.WaitState(key, id, "error")
	env.Eventually("error and degraded", func() bool {
		return has(server.EvSandboxError+":node_offline") && has(server.EvSandboxDegraded+":node_offline")
	})
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	env.WaitState(key, id, "ready")
	env.Eventually("ready and recovered", func() bool { return has(server.EvSandboxReady) && has(server.EvSandboxRecovered) })
}

// SNAP: resume and fork after every snapshot was deleted answer
// 409 snapshot_data_expired instead of booting an empty disk.
func TestResumeAfterSnapshotsDeleted(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	if r := env.Do(key, "DELETE", "/api/v1/sandboxes/"+id+"/snapshots", nil, "X-Ascii-Confirm-Delete: "+id); r.Status != 202 {
		t.Fatalf("delete snapshots: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil); r.Status != 409 || r.Code() != "snapshot_data_expired" {
		t.Fatalf("resume: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", nil); r.Status != 409 || r.Code() != "snapshot_data_expired" {
		t.Fatalf("fork: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.state") != "archived" || r.Get("sandbox.dataExpiredAt") == nil {
		t.Fatalf("sandbox: %s", r)
	}
}

// SIZE-011: a node that refuses a boot for capacity (disk budget) queues
// the sandbox like a full pool; failFast cancels it with no_ready_machine.
func TestNodeCapacityRefusalQueues(t *testing.T) {
	old := server.NodeFullBackoff
	server.NodeFullBackoff = 100 * time.Millisecond
	t.Cleanup(func() { server.NodeFullBackoff = old })
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	full := map[string]any{"ok": false, "code": "out_of_capacity", "error": "not enough disk"}
	node.RefuseNext("POST /v1/vms", 503, full)
	id := createSandbox(t, env, key, nil)
	r := env.WaitState(key, id, "ready")
	if r.Str("sandbox.error") != "" {
		t.Fatalf("queued boot: %s", r)
	}
	n := 0
	for _, c := range node.Calls() {
		if c == "POST /v1/vms" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("boot attempts: %d", n)
	}

	node.RefuseNext("POST /v1/vms", 503, full)
	r = env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"failFast": true})
	if r.Status != 202 {
		t.Fatalf("create failFast: %s", r)
	}
	r = env.WaitState(key, r.Str("sandbox.id"), "cancelled")
	if !strings.HasPrefix(r.Str("sandbox.error"), "no_ready_machine") {
		t.Fatalf("failFast refusal: %s", r)
	}
}

// API keys: rotate and revoke need a session or a key that can create keys.
func TestAPIKeyManagementNeedsSessionOrAdminKey(t *testing.T) {
	env := servertest.New(t)
	u, _ := env.NewUser("kim", false)
	session := env.Session(u)
	ro := env.Do(session, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "ro", "preset": "read-only"})
	roID, roSecret := ro.Str("apiKey.id"), ro.Str("secret")
	if r := env.Do(roSecret, "POST", "/api/v1/api-keys/"+roID+"/rotate", nil); r.Status != 403 || r.Code() != "api_key_action_forbidden" {
		t.Fatalf("read-only key rotating itself: %s", r)
	}
	if r := env.Do(roSecret, "DELETE", "/api/v1/api-keys/"+roID, nil); r.Status != 403 {
		t.Fatalf("read-only key revoking itself: %s", r)
	}
	adm := env.Do(session, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "adm", "preset": "admin"})
	if r := env.Do(adm.Str("secret"), "POST", "/api/v1/api-keys/"+roID+"/rotate", nil); r.Status != 200 {
		t.Fatalf("admin key rotating a narrower key: %s", r)
	}
	if r := env.Do(session, "DELETE", "/api/v1/api-keys/"+roID, nil); r.Status != 200 {
		t.Fatalf("session revoking: %s", r)
	}
}

// api__v1 400s: prompt_required and provider_required.
func TestPromptErrorCodes(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{}); r.Status != 400 || r.Code() != "prompt_required" {
		t.Fatalf("empty prompt: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "hi", "provider": ""}); r.Status != 400 || r.Code() != "provider_required" {
		t.Fatalf("empty provider: %s", r)
	}
}

// 403 org_suspended: an admin suspends an org; it cannot start sandboxes.
func TestOrgSuspended(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	_, alice := env.NewUser("alice", false)
	if r := env.Do(alice, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"}); r.Status/100 != 2 {
		t.Fatalf("org: %s", r)
	}
	running := createSandbox(t, env, alice, map[string]any{"org": "acme"})
	env.WaitState(alice, running, "ready")
	if r := env.Do(env.AdminKey, "POST", "/api/v1/admin/orgs/acme/suspend", map[string]any{"reason": "unpaid"}); r.Status != 200 {
		t.Fatalf("suspend: %s", r)
	}
	if r := env.Do(alice, "POST", "/api/v1/sandboxes", map[string]any{"org": "acme"}); r.Status != 403 || r.Code() != "org_suspended" {
		t.Fatalf("create in suspended org: %s", r)
	}
	env.WaitState(alice, running, "archived")
	if r := env.Do(alice, "POST", "/api/v1/sandboxes/"+running+"/resume", nil); r.Code() != "org_suspended" {
		t.Fatalf("resume in suspended org: %s", r)
	}
	if r := env.Do(alice, "GET", "/api/v1/orgs", nil); !strings.Contains(string(r.Body), `"suspended":true`) {
		t.Fatalf("org list: %s", r)
	}
	env.Do(env.AdminKey, "POST", "/api/v1/admin/orgs/acme/unsuspend", nil)
	if r := env.Do(alice, "POST", "/api/v1/sandboxes", map[string]any{"org": "acme"}); r.Status != 202 {
		t.Fatalf("create after unsuspend: %s", r)
	}
	// Personal sandboxes were never affected.
	createSandbox(t, env, alice, nil)
}

// HOOK-004: webhook and OTel URLs may not target loopback, link-local or
// metadata addresses unless the admin allows it; LAN targets are fine.
func TestOutboundGuard(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.WebhookAllowLocal = false })
	key := env.AdminKey
	for _, u := range []string{"http://127.0.0.1:9/x", "http://localhost/x", "http://169.254.169.254/latest/meta-data", "http://[::1]/x"} {
		if r := env.Do(key, "POST", "/api/v1/webhooks", map[string]any{"name": "x", "url": u, "events": []string{"sandbox.ready"}}); r.Status != 400 || r.Code() != "invalid_url" {
			t.Fatalf("%s: %s", u, r)
		}
	}
	if r := env.Do(key, "POST", "/api/v1/webhooks", map[string]any{"name": "lan", "url": "http://192.168.1.10/hook", "events": []string{"sandbox.ready"}}); r.Status/100 != 2 {
		t.Fatalf("LAN target: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/telemetry", map[string]any{"endpoint": "http://169.254.169.254:4318"}); r.Code() != "invalid_endpoint" {
		t.Fatalf("otel metadata endpoint: %s", r)
	}
}

// DATA: a deletion completes only when the node confirmed its copies gone.
func TestDeletionOpWaitsForNode(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	node.Disconnect()
	r := env.Do(key, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-Ascii-Confirm-Delete: "+id)
	if r.Status != 202 {
		t.Fatalf("delete: %s", r)
	}
	op := r.Str("operation.id")
	r = env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.stage", 5*time.Second, "retrying")
	if r.Str("operation.status") == "completed" || r.Get("operation.completedAt") != nil {
		t.Fatalf("completed while the node holds a copy: %s", r)
	}
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	env.WaitFor(key, "/api/v1/deletion-operations/"+op, "operation.status", 5*time.Second, "completed")
	if !node.Called("DELETE /v1/snapshots/") {
		t.Fatal("the node's snapshot copies were never deleted")
	}
}

// DOCS-001/002: llms.txt, per-page Markdown and the docs MCP server, not
// swallowed by the dashboard's SPA fallback.
func TestDocsForAgents(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) {
		c.WebFS = fstest.MapFS{"index.html": {Data: []byte("<html>dashboard</html>")}}
	})
	get := func(p string) (int, string, string) {
		resp, err := http.Get(env.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	for p, want := range map[string]string{
		"/llms.txt":                    "/docs/getting-started.md",
		"/llms-full.txt":               "# Getting started",
		"/docs/quickstart.md":          "# Getting started",
		"/docs/sandboxes.md":           "# Sandboxes, snapshots and forks",
		"/docs/cli-reference.md":       "## crowdvm new",
		"/docs/api-reference.md":       "`/api/v1/sandboxes/{sandboxId}/resume`",
		"/docs":                        "## Docs",
		"/docs/getting-started":        "# Getting started",
		"/docs/index.md":               "# CrowdVM user guide",
		"/docs/troubleshooting.md":     "# Troubleshooting",
		"/docs/migrating-from-boat.md": "Boat",
	} {
		code, ctype, body := get(p)
		if code != 200 || strings.Contains(body, "dashboard</html>") || !strings.Contains(body, want) || strings.Contains(ctype, "html") {
			t.Fatalf("%s: %d %s %.200q", p, code, ctype, body)
		}
	}
	if code, _, _ := get("/docs/nope.md"); code != 404 {
		t.Fatalf("missing page: %d", code)
	}
	if code, _, body := get("/sandboxes"); code != 200 || !strings.Contains(body, "dashboard") {
		t.Fatalf("SPA fallback broken: %d %s", code, body)
	}

	mcp := func(body string) (int, map[string]any) {
		resp, err := http.Post(env.URL+"/mcp", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := mcp(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	if code != 200 || servertest.Lookup(out, "result.protocolVersion") != "2025-03-26" || servertest.Lookup(out, "result.serverInfo.name") != "crowdvm-docs" {
		t.Fatalf("initialize: %d %v", code, out)
	}
	if code, _ := mcp(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Fatalf("notification: %d", code)
	}
	_, out = mcp(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if tools, _ := servertest.Lookup(out, "result.tools").([]any); len(tools) != 3 {
		t.Fatalf("tools/list: %v", out)
	}
	_, out = mcp(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_docs","arguments":{"query":"public url vite"}}}`)
	if txt, _ := servertest.Lookup(out, "result.content.0.text").(string); !strings.Contains(txt, "public-urls") {
		t.Fatalf("search_docs: %v", out)
	}
	_, out = mcp(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_doc","arguments":{"page":"sandboxes"}}}`)
	if txt, _ := servertest.Lookup(out, "result.content.0.text").(string); !strings.HasPrefix(txt, "# Sandboxes") {
		t.Fatalf("read_doc: %v", out)
	}
	_, out = mcp(`{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"crowdvm-docs://cli-for-agents"}}`)
	if txt, _ := servertest.Lookup(out, "result.contents.0.text").(string); !strings.Contains(txt, "crowdvm") {
		t.Fatalf("resources/read: %v", out)
	}
	_, out = mcp(`{"jsonrpc":"2.0","id":6,"method":"nope"}`)
	if servertest.Lookup(out, "error.code") != float64(-32601) {
		t.Fatalf("unknown method: %v", out)
	}
	if resp, _ := http.Get(env.URL + "/mcp"); resp.StatusCode != 405 {
		t.Fatalf("GET /mcp: %d", resp.StatusCode)
	}
}

// CI-001: Gitea Actions jobs run on an ephemeral act_runner per job.
func TestGiteaCI(t *testing.T) {
	var mu sync.Mutex
	job := map[string]any{"id": 77, "run_id": 5, "name": "test", "status": "queued", "labels": []string{"crowdvm"}, "head_sha": "abc", "_repo": "acme/api"}
	regs := 0
	m := http.NewServeMux()
	js := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "token gitea_test" {
				w.WriteHeader(401)
				return
			}
			h(w, r)
		}
	}
	m.HandleFunc("GET /api/v1/user", auth(func(w http.ResponseWriter, r *http.Request) { js(w, map[string]any{"login": "alice"}) }))
	m.HandleFunc("GET /api/v1/orgs/{org}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("org") != "acme" {
			w.WriteHeader(404)
			return
		}
		js(w, map[string]any{"username": "acme"})
	}))
	m.HandleFunc("GET /api/v1/orgs/acme/repos", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, []any{map[string]any{"full_name": "acme/api", "private": true, "html_url": "http://gitea/acme/api", "owner": map[string]any{"login": "acme"}}})
	}))
	m.HandleFunc("GET /api/v1/repos/acme/api/actions/runs", auth(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		runs := []any{}
		if job["status"] == "queued" && r.URL.Query().Get("status") == "queued" {
			runs = append(runs, map[string]any{"id": 5})
		}
		js(w, map[string]any{"workflow_runs": runs})
	}))
	m.HandleFunc("GET /api/v1/repos/acme/api/actions/runs/5/jobs", auth(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		js(w, map[string]any{"jobs": []any{job}})
	}))
	m.HandleFunc("GET /api/v1/repos/acme/api/actions/jobs/77", auth(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		js(w, job)
	}))
	m.HandleFunc("POST /api/v1/repos/acme/api/actions/runners/registration-token", auth(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		regs++
		mu.Unlock()
		js(w, map[string]any{"token": "REGTOKEN"})
	}))
	m.HandleFunc("POST /api/v1/repos/acme/api/statuses/{sha}", auth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }))
	gitea := httptest.NewServer(m)
	defer gitea.Close()

	var cmdMu sync.Mutex
	var cmd map[string]any
	guest := servertest.NewFakeGuest()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/commands" {
			b, _ := io.ReadAll(r.Body)
			cmdMu.Lock()
			_ = json.Unmarshal(b, &cmd)
			cmdMu.Unlock()
			js(w, map[string]any{"ok": true, "processId": "p1"})
			return
		}
		guest.ServeHTTP(w, r)
	})
	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = 50 * time.Millisecond })
	env.StartNode(servertest.NodeOptions{Guest: h})
	_, alice := env.NewUser("alice", false)
	r := env.Do(alice, "POST", "/api/v1/ci/connect", map[string]any{"provider": "gitea", "apiUrl": gitea.URL, "account": "acme", "token": "gitea_test", "confirm": true})
	if r.Status != 200 || r.Str("link.provider") != "gitea" || r.Str("link.accountType") != "Organization" {
		t.Fatalf("connect: %s", r)
	}
	env.Eventually("runner started", func() bool {
		cmdMu.Lock()
		defer cmdMu.Unlock()
		return cmd != nil
	})
	cmdMu.Lock()
	envs, _ := cmd["env"].(map[string]any)
	script, _ := cmd["command"].(string)
	cmdMu.Unlock()
	if envs["CROWDVM_RUNNER_TOKEN"] != "REGTOKEN" || envs["CROWDVM_INSTANCE"] != gitea.URL || envs["CROWDVM_RUNNER_LABEL"] != "crowdvm" ||
		!strings.Contains(script, "act_runner register --no-interactive --ephemeral") {
		t.Fatalf("runner command: %v", cmd)
	}
	var sbID string
	env.Eventually("job sandbox", func() bool {
		sbID = env.Do(alice, "GET", "/api/v1/ci/jobs/77", nil).Str("job.sandboxId")
		return sbID != ""
	})
	mu.Lock()
	job["status"], job["conclusion"], job["started_at"], job["completed_at"] = "completed", "success", "2026-11-08T10:00:00Z", "2026-11-08T10:01:00Z"
	mu.Unlock()
	env.WaitFor(alice, "/api/v1/ci/jobs/77", "job.state", 5*time.Second, "success")
	env.Eventually("CI sandbox deleted", func() bool {
		return env.Do(alice, "GET", "/api/v1/sandboxes/"+sbID, nil).Status == 404
	})
}

// Placement: the top-level nested field places on a nested-capable node.
func TestTopLevelNestedPlacement(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	env.StartNode(servertest.NodeOptions{Name: "plain"})
	b := &proto.Benchmark{}
	b.Virtualization.Nested = true
	nested := env.StartNode(servertest.NodeOptions{Name: "nested", Bench: b})
	for i := 0; i < 3; i++ {
		r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"nested": true})
		if r.Status != 202 || r.Str("sandbox.nodeId") != nested.ID {
			t.Fatalf("nested create: %s", r)
		}
	}
}
