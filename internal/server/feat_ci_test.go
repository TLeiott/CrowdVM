package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// fakeGitHub serves the few REST endpoints the CI controller uses.
type fakeGitHub struct {
	mu       sync.Mutex
	jobs     map[string]map[string]any // job id -> job
	jit      int
	statuses []map[string]any // commit statuses posted (sha, state, description, context)
	srv      *httptest.Server
}

// status returns the last commit status posted for sha (nil = none).
func (g *fakeGitHub) status(sha string) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.statuses) - 1; i >= 0; i-- {
		if g.statuses[i]["sha"] == sha {
			return g.statuses[i]
		}
	}
	return nil
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{jobs: map[string]map[string]any{}}
	m := http.NewServeMux()
	js := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer ghp_test" {
				w.WriteHeader(401)
				return
			}
			h(w, r)
		}
	}
	m.HandleFunc("GET /user", auth(func(w http.ResponseWriter, r *http.Request) { js(w, map[string]any{"login": "alice", "type": "User"}) }))
	m.HandleFunc("GET /users/{login}", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("login") {
		case "alice", "Bob":
			js(w, map[string]any{"login": r.PathValue("login"), "type": "User", "avatar_url": "http://x/a.png"})
		case "bob":
			js(w, map[string]any{"login": "Bob", "type": "User"})
		default:
			w.WriteHeader(404)
		}
	}))
	m.HandleFunc("GET /user/repos", auth(func(w http.ResponseWriter, r *http.Request) {
		js(w, []any{
			map[string]any{"full_name": "alice/api", "private": true, "html_url": "https://github.com/alice/api", "owner": map[string]any{"login": "alice"}},
			map[string]any{"full_name": "alice/site", "private": false, "html_url": "https://github.com/alice/site", "owner": map[string]any{"login": "alice"}},
		})
	}))
	m.HandleFunc("GET /repos/{o}/{r}/actions/runs", auth(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		runs := []any{}
		seen := map[float64]bool{}
		for _, j := range g.jobs {
			if strings.HasSuffix(j["_repo"].(string), r.PathValue("r")) && j["status"] == "queued" && r.URL.Query().Get("status") == "queued" && !seen[j["run_id"].(float64)] {
				seen[j["run_id"].(float64)] = true
				runs = append(runs, map[string]any{"id": j["run_id"], "actor": map[string]any{"login": "mallory"}, "triggering_actor": map[string]any{"login": "mallory"}})
			}
		}
		js(w, map[string]any{"workflow_runs": runs})
	}))
	m.HandleFunc("GET /repos/{o}/{r}/actions/runs/{run}/jobs", auth(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		out := []any{}
		for _, j := range g.jobs {
			if j["_repo"] == r.PathValue("o")+"/"+r.PathValue("r") {
				out = append(out, j)
			}
		}
		js(w, map[string]any{"jobs": out})
	}))
	m.HandleFunc("GET /repos/{o}/{r}/actions/jobs/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if j, ok := g.jobs[r.PathValue("id")]; ok {
			js(w, j)
			return
		}
		w.WriteHeader(404)
	}))
	m.HandleFunc("GET /repos/{o}/{r}/actions/jobs/{id}/logs", auth(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("2026-11-08T00:00:00Z hello from the job\n"))
	}))
	m.HandleFunc("POST /repos/{o}/{r}/actions/runners/generate-jitconfig", auth(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.jit++
		g.mu.Unlock()
		w.WriteHeader(201)
		js(w, map[string]any{"encoded_jit_config": "JITCONFIG", "runner": map[string]any{"id": 1}})
	}))
	m.HandleFunc("POST /repos/{o}/{r}/statuses/{sha}", auth(func(w http.ResponseWriter, r *http.Request) {
		var st map[string]any
		_ = json.NewDecoder(r.Body).Decode(&st)
		st["sha"] = r.PathValue("sha")
		g.mu.Lock()
		g.statuses = append(g.statuses, st)
		g.mu.Unlock()
		w.WriteHeader(201)
		js(w, st)
	}))
	m.HandleFunc("GET /repos/actions/runner/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"tag_name": "v2.399.0"})
	})
	g.srv = httptest.NewServer(m)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) addJob(id string, run int, repo, label string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.jobs[id] = map[string]any{"id": json.Number(id), "run_id": float64(run), "_repo": repo, "name": "test", "workflow_name": "ci", "head_branch": "main",
		"status": "queued", "labels": []string{label}, "created_at": time.Now().UTC().Format(time.RFC3339), "head_sha": "sha" + id, "html_url": "https://github.com/" + repo + "/actions/runs/1/job/" + id}
}

func (g *fakeGitHub) set(id string, kv ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := 0; i < len(kv); i += 2 {
		g.jobs[id][kv[i].(string)] = kv[i+1]
	}
}

// cmdGuest records commands run in sandboxes.
type cmdGuest struct {
	*servertest.FakeGuest
	mu   sync.Mutex
	cmds []map[string]any
}

func (g *cmdGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && r.URL.Path == "/v1/commands" {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		g.mu.Lock()
		g.cmds = append(g.cmds, m)
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"type":"command.started","processId":"1"}`))
		return
	}
	g.FakeGuest.ServeHTTP(w, r)
}

func TestCI(t *testing.T) {
	gh := newFakeGitHub(t)
	guest := &cmdGuest{FakeGuest: servertest.NewFakeGuest()}
	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = 50 * time.Millisecond })
	env.StartNode(servertest.NodeOptions{Guest: guest})
	key := env.AdminKey

	if r := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{}); r.Status != 400 || r.Code() != "token_required" {
		t.Fatalf("no token: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"token": "bad", "apiUrl": gh.srv.URL}); r.Status != 400 || r.Code() != "invalid_token" {
		t.Fatalf("bad token: %s", r)
	}
	r := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_test", "apiUrl": gh.srv.URL})
	if r.Status != 200 || r.Str("type") != "ci.connect" || r.Str("link.status") != "pending" || !strings.Contains(r.Str("installUrl"), "confirm=") {
		t.Fatalf("connect: %s", r)
	}
	linkID := r.Str("link.id")
	if r := env.Do(key, "GET", "/api/v1/ci", nil); r.Get("setup.next") != "install" || r.Str("pendingLinks.0.id") != linkID ||
		!strings.Contains(r.Str("pendingLinks.0.confirmUrl"), "confirm="+linkID) || !strings.Contains(r.Str("setup.steps.0.detail"), "dashboard") {
		t.Fatalf("overview before confirm: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"linkId": linkID, "confirm": true}); r.Status != 200 || r.Str("link.status") != "active" {
		t.Fatalf("confirm: %s", r)
	}

	// Jobs: one good, one with an unknown option, one on a public repo.
	gh.addJob("1001", 1, "alice/api", "crowdvm-small/debug")
	gh.addJob("1002", 2, "alice/api", "crowdvm/turbo")
	gh.addJob("1003", 3, "alice/site", "crowdvm")
	gh.addJob("1004", 4, "alice/api", "ubuntu-latest")

	var sbID string
	env.Eventually("runner started in sandbox", func() bool {
		r := env.Do(key, "GET", "/api/v1/ci/jobs/1001", nil)
		sbID = r.Str("job.sandboxId")
		guest.mu.Lock()
		defer guest.mu.Unlock()
		return sbID != "" && len(guest.cmds) > 0
	})
	guest.mu.Lock()
	cmd := guest.cmds[0]
	guest.mu.Unlock()
	envs, _ := cmd["env"].(map[string]any)
	if cmd["detached"] != true || !strings.Contains(cmd["command"].(string), "--jitconfig") || envs["CROWDVM_JIT_CONFIG"] != "JITCONFIG" || envs["CROWDVM_RUNNER_VERSION"] != "2.399.0" {
		t.Fatalf("runner command: %v", cmd)
	}
	r = env.Do(key, "GET", "/api/v1/sandboxes/"+sbID, nil)
	if r.Str("sandbox.name") != "ci-api-1001" || r.Str("sandbox.type") != "small" || r.Get("sandbox.noEnv") != true {
		t.Fatalf("ci sandbox: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/ci/jobs/1002", nil); r.Str("job.state") != "refused" || !strings.Contains(r.Str("job.error"), "/turbo") {
		t.Fatalf("unknown option: %s", r)
	}
	if st := gh.status("sha1002"); st == nil || st["state"] != "error" || !strings.Contains(st["description"].(string), "/turbo") || st["context"] != "CrowdVM / test" {
		t.Fatalf("refusal not posted as a commit status: %v", st)
	}
	env.Eventually("public repo refused", func() bool {
		r := env.Do(key, "GET", "/api/v1/ci/jobs/1003", nil)
		return r.Str("job.state") == "refused" && strings.Contains(r.Str("job.error"), "Public repository") && strings.Contains(r.Str("job.error"), "dashboard")
	})
	if r := env.Do(key, "GET", "/api/v1/ci/jobs/1004", nil); r.Status != 404 {
		t.Fatalf("non-crowdvm job tracked: %s", r)
	}

	// Running, then failed with /debug: sandbox kept.
	started := time.Now().UTC().Format(time.RFC3339)
	gh.set("1001", "status", "in_progress", "started_at", started)
	env.WaitFor(key, "/api/v1/ci/jobs/1001", "job.state", 30*time.Second, "running")
	if r := env.Do(key, "GET", "/api/v1/ci/jobs/1001/logs", nil); r.Status != 200 || !strings.Contains(r.Str("log"), "hello from the job") {
		t.Fatalf("logs: %s", r)
	}
	// The runner's cgroup saw an OOM kill (CI-005).
	guest.AddHistory(sbID, proto.HistoryExport{Records: []proto.CommandRecord{{CommandID: "cmd_runner", SandboxID: sbID, Command: "run.sh", Mode: "detached", Status: "exited", OOMKilled: true, StartedAt: time.Now()}}})
	gh.set("1001", "status", "completed", "conclusion", "failure", "completed_at", time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	r = env.WaitFor(key, "/api/v1/ci/jobs/1001", "job.state", 30*time.Second, "failure")
	if r.Str("job.sandboxState") != "kept" || r.Get("job.costDollars") == nil || r.Get("job.costCredits") == nil || r.Get("job.costDollars") == r.Get("job.costCredits") {
		t.Fatalf("debug keep: %s", r)
	}
	if !strings.Contains(r.Str("job.error"), "Out of memory on CrowdVM") {
		t.Fatalf("oom: %s", r)
	}
	if st := gh.status("sha1001"); st == nil || st["state"] != "failure" || st["description"] != "Out of memory on CrowdVM" {
		t.Fatalf("oom status: %v", st)
	}

	// A second job that succeeds: its sandbox is deleted.
	gh.addJob("1005", 5, "alice/api", "crowdvm-large")
	var sb2 string
	env.Eventually("second sandbox", func() bool {
		sb2 = env.Do(key, "GET", "/api/v1/ci/jobs/1005", nil).Str("job.sandboxId")
		return sb2 != ""
	})
	env.WaitState(key, sb2, "ready")
	gh.set("1005", "status", "completed", "conclusion", "success", "started_at", started, "completed_at", started)
	r = env.WaitFor(key, "/api/v1/ci/jobs/1005", "job.state", 30*time.Second, "success")
	if w, _ := r.Get("job.waitSeconds").(float64); w < 0 || r.Get("job.waitSeconds") == nil { // started_at is before created_at here
		t.Fatalf("negative wait: %s", r)
	}
	env.Eventually("sandbox deleted", func() bool { return env.Do(key, "GET", "/api/v1/sandboxes/"+sb2, nil).Status == 404 })

	// Overview.
	r = env.Do(key, "GET", "/api/v1/ci", nil)
	if r.Status != 200 || r.Str("type") != "ci.overview" || r.Get("setup.complete") != true || r.Str("installations.0.accountLogin") != "alice" {
		t.Fatalf("overview: %s", r)
	}
	repos := r.Get("installations.0.repositories").([]any)
	stat := map[string]string{}
	for _, x := range repos {
		m := x.(map[string]any)
		stat[m["fullName"].(string)] = m["status"].(string)
	}
	if stat["alice/site"] != "public-blocked" || stat["alice/api"] == "" {
		t.Fatalf("repo status: %v", stat)
	}
	if r.Get("counts.all").(float64) != 4 || len(r.Get("setup.problems").([]any)) < 2 {
		t.Fatalf("counts/problems: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/ci?state=failed", nil); len(r.Get("jobs").([]any)) != 3 {
		t.Fatalf("failed filter: %s", r)
	}

	// Public repos: users checked on GitHub.
	if r := env.Do(key, "POST", "/api/v1/ci/public", map[string]any{"account": "alice", "allow": true, "users": "@bob, nobody"}); r.Status != 400 || r.Code() != "unknown_github_user" {
		t.Fatalf("unknown user: %s", r)
	}
	r = env.Do(key, "POST", "/api/v1/ci/public", map[string]any{"account": "alice", "allow": true, "users": "@bob https://github.com/alice"})
	if r.Status != 200 || r.Get("allowPublicRepos") != true || r.Str("publicRepoUsers.0.login") != "Bob" {
		t.Fatalf("public: %s", r)
	}
	// The run's actor (mallory) is not listed: refused with the reason.
	gh.addJob("1006", 6, "alice/site", "crowdvm")
	env.Eventually("actor refused", func() bool {
		r := env.Do(key, "GET", "/api/v1/ci/jobs/1006", nil)
		return r.Str("job.state") == "refused" && strings.Contains(r.Str("job.error"), "mallory")
	})

	// Unlink: the token and repositories go, the job history stays.
	if r := env.Do(key, "DELETE", "/api/v1/ci/links/"+linkID, nil); r.Status != 200 || r.Str("type") != "ci.unlinked" {
		t.Fatalf("unlink: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/ci/jobs/1001", nil); r.Status != 200 || r.Str("job.state") != "failure" {
		t.Fatalf("job after unlink: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/ci/jobs/1001/logs", nil); r.Status != 200 || r.Str("unavailable") == "" {
		t.Fatalf("logs after unlink: %s", r)
	}
	r = env.Do(key, "GET", "/api/v1/ci", nil)
	if len(r.Get("installations").([]any)) != 0 || len(r.Get("jobs").([]any)) != 5 || r.Get("setup.next") != "install" {
		t.Fatalf("overview after unlink: %s", r)
	}
	env.Eventually("kept sandbox deleted", func() bool { return env.Do(key, "GET", "/api/v1/sandboxes/"+sbID, nil).Status == 404 })
	if r := env.Do(key, "DELETE", "/api/v1/ci/links/"+linkID, nil); r.Status != 404 {
		t.Fatalf("unlink twice: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"linkId": linkID, "confirm": true}); r.Status != 409 {
		t.Fatalf("confirm an unlinked link: %s", r)
	}
	// Linking the account again works.
	if r := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_test", "apiUrl": gh.srv.URL, "confirm": true}); r.Status != 200 || r.Str("link.status") != "active" {
		t.Fatalf("relink: %s", r)
	}
}

func TestCILabels(t *testing.T) {
	for label, ok := range map[string]bool{"crowdvm": true, "crowdvm-small": true, "crowdvm-xlarge/from=base/debug": true, "crowdvm/kvm": true,
		"crowdvm-huge": false, "crowdvm/turbo": false, "crowdvm-": false, "crowdvm/from=": false,
		"boat": true, "boat-large/debug": true, "boatx": false, "boat-huge": false} {
		r := server.CILabelOK(label)
		if r != ok {
			t.Errorf("%s: got %v", label, r)
		}
	}
}

// A runner takes any queued job with its label: when jobs swap runners, each
// job's completion deletes the sandbox that ran it, not the one booted for it
// (opus-14 B1).
func TestCIRunnerSwap(t *testing.T) {
	gh := newFakeGitHub(t)
	guest := &cmdGuest{FakeGuest: servertest.NewFakeGuest()}
	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = 50 * time.Millisecond })
	env.StartNode(servertest.NodeOptions{Guest: guest})
	key := env.AdminKey
	linkID := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_test", "apiUrl": gh.srv.URL}).Str("link.id")
	env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"linkId": linkID, "confirm": true})
	gh.addJob("2001", 1, "alice/api", "crowdvm")
	gh.addJob("2002", 2, "alice/api", "crowdvm")
	sb := map[string]string{}
	env.Eventually("both jobs have sandboxes", func() bool {
		for _, id := range []string{"2001", "2002"} {
			sb[id] = env.Do(key, "GET", "/api/v1/ci/jobs/"+id, nil).Str("job.sandboxId")
		}
		return sb["2001"] != "" && sb["2002"] != ""
	})
	now := time.Now().UTC().Format(time.RFC3339)
	gh.set("2001", "status", "in_progress", "started_at", now, "runner_name", "crowdvm-"+sb["2002"])
	gh.set("2002", "status", "in_progress", "started_at", now, "runner_name", "crowdvm-"+sb["2001"])
	env.WaitFor(key, "/api/v1/ci/jobs/2001", "job.state", 30*time.Second, "running")
	env.WaitFor(key, "/api/v1/ci/jobs/2002", "job.state", 30*time.Second, "running")
	gh.set("2001", "status", "completed", "conclusion", "success", "completed_at", now)
	env.WaitFor(key, "/api/v1/ci/jobs/2001", "job.state", 30*time.Second, "success")
	env.Eventually("the sandbox that ran 2001 is deleted", func() bool {
		return env.Do(key, "GET", "/api/v1/sandboxes/"+sb["2002"], nil).Status == 404
	})
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+sb["2001"], nil); r.Status != 200 {
		t.Fatalf("the sandbox still running 2002 was deleted: %s", r)
	}
}
