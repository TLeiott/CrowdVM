package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// A git source (POST /repos/sources) takes an apiUrl that the coordinator then
// GETs with the stored token (GET /repos?sync=true), through a plain http
// client: no outbound guard on save, none on dial, redirects followed. The
// first 512 bytes of a non-200 answer come back in syncError. Webhooks, OTel
// exports, CI links and custom AI APIs all refuse loopback, link-local and
// metadata targets; this is the one user-supplied URL that does not.
func TestBughuntR20GitSourceReachesLoopbackAndEchoesTheAnswer(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(403)
		_, _ = w.Write([]byte("internal-only-admin-page"))
	}))
	defer internal.Close()
	env := servertest.New(t, func(c *server.Config) { c.WebhookAllowLocal = false })
	key := env.AdminKey
	r := env.Do(key, "POST", "/api/v1/repos/sources", map[string]any{"kind": "github", "baseUrl": "https://github.com", "apiUrl": internal.URL, "token": "tok_r20_secret"})
	if r.Status == 400 {
		return // refused when saved: fine
	}
	if r.Status != 201 {
		t.Fatalf("add source: %s", r)
	}
	r = env.Do(key, "GET", "/api/v1/repos?sync=true", nil)
	if hits.Load() > 0 {
		t.Fatalf("the server called a loopback URL on behalf of a git source (%d request(s)); answer echoed in syncError: %q", hits.Load(), r.Str("syncError"))
	}
	if strings.Contains(r.String(), "internal-only-admin-page") {
		t.Fatalf("an internal answer is echoed back: %s", r)
	}
}

// The CI controller lists an account's repositories with one request
// (/user/repos?per_page=100 or /orgs/{login}/repos?per_page=100) and never asks
// for the next page. GitHub caps a page at 100: the 101st repository of an
// account (sorted by name) is never polled, so a job queued there stays
// queued on GitHub for ever and CrowdVM never starts a runner for it.
func TestBughuntR20CIOnlyPollsTheFirstHundredRepositories(t *testing.T) {
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	repo := func(name string) map[string]any {
		return map[string]any{"full_name": "alice/" + name, "private": true, "html_url": "https://github.com/alice/" + name, "owner": map[string]any{"login": "alice"}}
	}
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) { js(w, map[string]any{"login": "alice", "type": "User"}) })
	mux.HandleFunc("GET /users/alice", func(w http.ResponseWriter, r *http.Request) { js(w, map[string]any{"login": "alice", "type": "User"}) })
	mux.HandleFunc("GET /user/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			js(w, []any{repo("zzz-late")})
			return
		}
		list := []any{}
		for i := 0; i < 100; i++ {
			list = append(list, repo(fmt.Sprintf("r%03d", i)))
		}
		js(w, list)
	})
	mux.HandleFunc("GET /repos/{o}/{r}/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("r") == "zzz-late" && r.URL.Query().Get("status") == "queued" {
			js(w, map[string]any{"workflow_runs": []any{map[string]any{"id": 7, "actor": map[string]any{"login": "alice"}}}})
			return
		}
		js(w, map[string]any{"workflow_runs": []any{}})
	})
	mux.HandleFunc("GET /repos/{o}/{r}/actions/runs/{run}/jobs", func(w http.ResponseWriter, r *http.Request) {
		js(w, map[string]any{"jobs": []any{map[string]any{"id": 9001, "run_id": 7, "name": "test", "status": "queued", "labels": []string{"crowdvm"},
			"created_at": time.Now().UTC().Format(time.RFC3339), "head_sha": "sha9001"}}})
	})
	mux.HandleFunc("GET /repos/{o}/{r}/actions/jobs/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	gh := httptest.NewServer(mux)
	defer gh.Close()

	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = 50 * time.Millisecond })
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	if r := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_x", "apiUrl": gh.URL, "confirm": true}); r.Status != 200 || r.Str("link.status") != "active" {
		t.Fatalf("connect: %s", r)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if r := env.Do(key, "GET", "/api/v1/ci/jobs/9001", nil); r.Status == 200 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("a queued job in the 101st repository of the account was never picked up: the repository list is read one page only")
}

// POST /account/erase refuses an owner whose organization has other members
// (round 19), but for an organization the account owns ALONE it only deletes
// the membership row. The organization stays: its environments (sealed
// variables and secret files), AI credentials, git sources and wallet are
// still in the database, and now nobody can reach, change or delete them.
// Deleting the organization (handleDeleteOrg) removes all of that; erasing the
// account that was its only member should too.
func TestBughuntR20EraseLeavesSoleOwnedOrganizationSecrets(t *testing.T) {
	env := servertest.New(t)
	owner, ownerKey := env.NewUser("r20solo", false)
	r := env.Do(ownerKey, "POST", "/api/v1/orgs", map[string]any{"name": "R20Solo"})
	if r.Status/100 != 2 {
		t.Fatalf("create org: %s", r)
	}
	var orgID string
	_ = env.S.DB.QueryRow(`SELECT id FROM orgs WHERE name='R20Solo'`).Scan(&orgID)
	r = env.Do(ownerKey, "POST", "/api/v1/environments?org=r20solo", map[string]any{"name": "deploy"})
	if r.Status != 200 {
		t.Fatalf("org environment: %s", r)
	}
	envID := r.Str("environment.id")
	if r := env.Do(ownerKey, "PUT", "/api/v1/environments/"+envID+"?org=r20solo", map[string]any{"envContents": "DEPLOY_TOKEN=r20-org-secret\\n"}); r.Status != 200 {
		t.Fatalf("set secret: %s", r)
	}
	browser, err := env.S.NewSession(owner.ID, "browser", "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := env.Do(browser, "POST", "/api/v1/account/erase", map[string]any{"confirmation": "erase my account"}); r.Status != 200 {
		t.Fatalf("erase: %s", r)
	}
	var envs, versions int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM environments WHERE owner_id=?`, orgID).Scan(&envs)
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM environment_versions WHERE env_id IN (SELECT id FROM environments WHERE owner_id=?)`, orgID).Scan(&versions)
	if envs > 0 || versions > 0 {
		t.Fatalf("the erased account's sole-owned organization keeps %d environment(s) and %d sealed version(s), with no member left to reach them", envs, versions)
	}
}

// PUT /environments/{id} applies its non-versioned fields one at a time as it
// goes: the new name is written before the access list is resolved, and the
// request is refused (400) afterwards. A request that answers 400 has still
// renamed the environment (and sandboxes pinned to it show the new name).
func TestBughuntR20EnvUpdateRefusedButAlreadyRenamed(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	r := env.Do(key, "POST", "/api/v1/environments", map[string]any{"name": "r20before"})
	if r.Status != 200 {
		t.Fatalf("create: %s", r)
	}
	id := r.Str("environment.id")
	// A personal environment has no access list: the update is refused.
	r = env.Do(key, "PUT", "/api/v1/environments/"+id, map[string]any{"name": "r20after", "access": map[string]any{"restricted": true, "members": []string{}}})
	if r.Status != 400 {
		t.Fatalf("setup: expected the access list to be refused, got %s", r)
	}
	if got := env.Do(key, "GET", "/api/v1/environments/"+id, nil).Str("environment.name"); got != "r20before" {
		t.Fatalf("the refused update (400) renamed the environment to %q", got)
	}
}

// POST /ci/connect refuses an account that another CrowdVM user already
// linked, but only looks at links that are 'active'. Pending links are not
// counted and the confirm step (linkId + confirm) has no check at all, so two
// users (two admins of one GitHub organization, or one person on two CrowdVM
// accounts) connect the same GitHub account and confirm both: it is polled
// twice, every queued job gets two sandboxes and two runner registrations
// (each billed), and the loser of the race idles until it is cleaned up.
func TestBughuntR20CIConfirmAllowsTheSameAccountTwice(t *testing.T) {
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) { js(w, map[string]any{"login": "alice", "type": "User"}) })
	mux.HandleFunc("GET /users/alice", func(w http.ResponseWriter, r *http.Request) { js(w, map[string]any{"login": "alice", "type": "User"}) })
	gh := httptest.NewServer(mux)
	defer gh.Close()
	env := servertest.New(t)
	_, k1 := env.NewUser("r20ci1", false)
	_, k2 := env.NewUser("r20ci2", false)
	r1 := env.Do(k1, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_x", "apiUrl": gh.URL})
	r2 := env.Do(k2, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_x", "apiUrl": gh.URL})
	if r1.Status != 200 || r2.Status != 200 {
		t.Fatalf("setup: %s / %s", r1, r2)
	}
	c1 := env.Do(k1, "POST", "/api/v1/ci/connect", map[string]any{"linkId": r1.Str("link.id"), "confirm": true})
	c2 := env.Do(k2, "POST", "/api/v1/ci/connect", map[string]any{"linkId": r2.Str("link.id"), "confirm": true})
	if c1.Status != 200 {
		t.Fatalf("first confirm: %s", c1)
	}
	if c2.Status == 200 {
		var n int
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM ci_links WHERE lower(login)='alice' AND status='active'`).Scan(&n)
		t.Fatalf("both confirmations succeeded: %d active links for the same GitHub account", n)
	}
}

// A CI job is recorded as 'queued' (sandbox_id NULL, next_try_at 0) before its
// sandbox is created, and a restart of the coordinator in that window (or at
// any point before the runner command is sent) leaves the row behind: newJob
// skips a job it already knows, retryWaiting only restarts rows with
// next_try_at > 0, and nothing else starts a runner. The job waits on GitHub
// with no runner (GitHub drops it after a day) and shows 'queued' on CrowdVM.
func TestBughuntR20CIJobQueuedBeforeARestartIsNeverStarted(t *testing.T) {
	gh := newFakeGitHub(t)
	guest := &cmdGuest{FakeGuest: servertest.NewFakeGuest()}
	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = 50 * time.Millisecond })
	env.StartNode(servertest.NodeOptions{Guest: guest})
	key := env.AdminKey
	r := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_test", "apiUrl": gh.srv.URL, "confirm": true})
	if r.Status != 200 {
		t.Fatalf("connect: %s", r)
	}
	linkID := r.Str("link.id")
	now := time.Now().UnixMilli()
	// What the previous process left: recorded, no sandbox, no retry time.
	if _, err := env.S.DB.Exec(`INSERT INTO ci_jobs(id,gh_id,link_id,owner_id,repo,run_id,job_name,label,machine_class,state,queued_at,updated_at)
		VALUES('5001','5001',?,?,'alice/api','1','test','crowdvm','default','queued',?,?)`, linkID, env.Admin.ID, now, now); err != nil {
		t.Fatal(err)
	}
	gh.addJob("5001", 1, "alice/api", "crowdvm")
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if env.Do(key, "GET", "/api/v1/ci/jobs/5001", nil).Str("job.sandboxId") != "" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("a recorded job with no sandbox is never started: %s", env.Do(key, "GET", "/api/v1/ci/jobs/5001", nil))
}
