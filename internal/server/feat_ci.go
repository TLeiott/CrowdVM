package server

// CI: GitHub Actions jobs on CrowdVM sandboxes (Boat parity CI-001..016,
// API-025..029), ADAPTED for the LAN: there is no GitHub App and no inbound
// webhook. A controller on the coordinator polls GitHub with a user-supplied
// token (outbound only) for queued workflow jobs whose runs-on label starts
// with "crowdvm", boots a fresh sandbox per job (noEnv, named
// ci-<repo>-<job id>), registers an ephemeral JIT runner in it and deletes
// the sandbox when the job ends (failed /debug jobs are kept 30 minutes).
//
// Labels: crowdvm-small, crowdvm, crowdvm-large, crowdvm-xlarge, with options
// /from=<saved snapshot>, /debug and the legacy no-op /kvm.
//
// Gitea Actions (provider "gitea", Gitea 1.24+ whose REST API has the
// GitHub-shaped actions runs/jobs endpoints; Forgejo with the same API is
// expected to work but untested): the same poller and job model, with the
// API at <instance>/api/v1. Instead of a GitHub JIT config each job gets a
// runner registration token (POST /repos/{repo}/actions/runners/
// registration-token) and the sandbox runs an ephemeral act_runner
// (`register --ephemeral`, labels <label>:host) that takes one job and exits.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// CIPollInterval is how often the controller asks GitHub for queued jobs
// (default; Config.CIPollInterval overrides it per server).
var CIPollInterval = 15 * time.Second

// CIRetryInterval is how often a job waiting on the minute start limit retries (Boat: 15 s).
var CIRetryInterval = 15 * time.Second

// CIRunnerVersion is used when the latest actions/runner release cannot be read.
var CIRunnerVersion = "2.329.0"

// CIActRunnerVersion is the act_runner release Gitea jobs run (Gitea
// links); CIActRunnerURL overrides the download URL (%s = version), e.g. a
// LAN mirror.
var (
	CIActRunnerVersion = "0.2.13"
	CIActRunnerURL     = "https://dl.gitea.com/act_runner/%s/act_runner-%s-linux-amd64"
)

// CILabelPrefix is the runs-on label family the controller picks up.
var CILabelPrefix = "crowdvm"

const (
	ciMaxJob      = 6 * time.Hour
	ciKeepDebug   = 30 * time.Minute
	ciHourWait    = 15 * time.Minute
	ciLogMax      = 1 << 20
	ciDefaultAPI  = "https://api.github.com"
	ciSandboxTTL  = 6*3600 + 15*60
	ciLabelMarker = "crowdvm.ci.job"
)

func init() {
	store.Register("ci", store.Migration{Version: 1, SQL: `
CREATE TABLE ci_links (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  org_id TEXT,
  api_url TEXT NOT NULL,
  login TEXT NOT NULL,
  account_type TEXT NOT NULL,
  token TEXT NOT NULL,
  token_user TEXT NOT NULL DEFAULT '',
  allow_public INTEGER NOT NULL DEFAULT 0,
  public_users TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL,
  last_poll_at INTEGER,
  last_error TEXT,
  created_at INTEGER NOT NULL,
  confirmed_at INTEGER
);
CREATE TABLE ci_repos (
  link_id TEXT NOT NULL,
  full_name TEXT NOT NULL,
  private INTEGER NOT NULL,
  html_url TEXT NOT NULL,
  PRIMARY KEY (link_id, full_name)
);
CREATE TABLE ci_jobs (
  id TEXT PRIMARY KEY,
  link_id TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  repo TEXT NOT NULL,
  run_id TEXT NOT NULL DEFAULT '',
  job_name TEXT NOT NULL DEFAULT '',
  workflow_name TEXT,
  head_branch TEXT,
  head_sha TEXT,
  html_url TEXT,
  label TEXT NOT NULL,
  machine_class TEXT NOT NULL,
  from_snapshot TEXT,
  debug INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL,
  error TEXT,
  sandbox_id TEXT,
  sandbox_state TEXT,
  queued_at INTEGER NOT NULL,
  run_started_at INTEGER,
  finished_at INTEGER,
  next_try_at INTEGER NOT NULL DEFAULT 0,
  keep_until INTEGER,
  cost REAL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX ci_jobs_link ON ci_jobs(link_id, queued_at);
CREATE INDEX ci_jobs_state ON ci_jobs(state);
`}, store.Migration{Version: 2, SQL: `ALTER TABLE ci_links ADD COLUMN provider TEXT NOT NULL DEFAULT 'github';`},
		// gh_id: the provider's job id. It is unique per provider instance
		// only (Gitea counts 1, 2, 3 on each), so id, the key, may carry a
		// link suffix (bugs-r10-sonnet 2).
		store.Migration{Version: 3, SQL: `ALTER TABLE ci_jobs ADD COLUMN gh_id TEXT NOT NULL DEFAULT '';
UPDATE ci_jobs SET gh_id=id;
CREATE INDEX ci_jobs_gh ON ci_jobs(link_id, gh_id);`})
	RegisterFeature("ci", func(s *Server) error {
		ci := &ciState{s: s, busy: map[string]bool{}}
		s.API("GET /ci", "account.read", ci.overview)
		s.API("POST /ci/connect", "account.admin", ci.connect)
		s.API("POST /ci/public", "account.admin", ci.public)
		s.API("GET /ci/jobs/{jobId}", "account.read", ci.getJob)
		s.API("GET /ci/jobs/{jobId}/logs", "account.read", ci.jobLogs)
		s.API("DELETE /ci/links/{linkId}", "account.admin", ci.unlink) // CrowdVM extension
		s.Go(ci.loop)
		return nil
	})
}

type ciState struct {
	s *Server

	mu   sync.Mutex
	busy map[string]bool // job id -> a start goroutine runs
}

// ---------------------------------------------------------------------------
// GitHub REST client (token auth, outbound only)
// ---------------------------------------------------------------------------

type ciGH struct {
	api, token string
	gitea      bool // Gitea/Forgejo: "token" auth scheme, plain JSON
	// allowLocal: loopback, link-local and metadata targets are reachable
	// (--webhook-allow-local); LAN and Tailscale always are (bugs-r4-opus 2).
	allowLocal bool
}

type ciHTTPError struct {
	Status int
	Body   string
}

func (e *ciHTTPError) Error() string { return fmt.Sprintf("CI provider API %d: %s", e.Status, e.Body) }

// ciHTTP dials through the outbound guard, also on redirects.
var ciHTTP = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: guardedDial, TLSHandshakeTimeout: 10 * time.Second}}

// orgAdmin reports whether the token's user (login me) administers org.
func (g ciGH) orgAdmin(ctx context.Context, org, me string) bool {
	if g.gitea {
		var p struct {
			IsOwner bool `json:"is_owner"`
			IsAdmin bool `json:"is_admin"`
		}
		return g.do(ctx, "GET", "/users/"+url.PathEscape(me)+"/orgs/"+url.PathEscape(org)+"/permissions", nil, &p) == nil && (p.IsOwner || p.IsAdmin)
	}
	var m struct{ State, Role string }
	return g.do(ctx, "GET", "/user/memberships/orgs/"+url.PathEscape(org), nil, &m) == nil && m.State == "active" && m.Role == "admin"
}

func (g ciGH) do(ctx context.Context, method, path string, in, out any) error {
	_, err := g.doH(ctx, method, path, in, out)
	return err
}

// ciMaxPages bounds a paged listing (bugs-r20-sonnet 2).
const ciMaxPages = 20

// ciPaged reads a list one page after the other (bugs-r20-sonnet 2): it
// follows the Link header's rel="next" (GitHub, Gitea) and, when the answer
// has no Link header, goes on while a page comes back full. count and add
// read the decoded page; a failed page returns its error with the pages
// collected so far.
func ciPaged[T any](ctx context.Context, g ciGH, path string, perPage int, count func(*T) int, add func(*T)) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= ciMaxPages; page++ {
		var v T
		h, err := g.doH(ctx, "GET", path+sep+"page="+strconv.Itoa(page), nil, &v)
		if err != nil {
			return err
		}
		add(&v)
		more := count(&v) >= perPage
		if link := h.Get("Link"); link != "" {
			more = strings.Contains(link, `rel="next"`)
		}
		if !more {
			return nil
		}
	}
	return nil
}

func (g ciGH) doH(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(withOutboundPolicy(ctx, g.allowLocal), method, strings.TrimRight(g.api, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "CrowdVM-CI")
	if g.gitea {
		req.Header.Set("Accept", "application/json")
		if g.token != "" {
			req.Header.Set("Authorization", "token "+g.token)
		}
	} else {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if g.token != "" {
			req.Header.Set("Authorization", "Bearer "+g.token)
		}
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ciHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200)) // enough for "Bad credentials"; not a page of whatever answered
		return nil, &ciHTTPError{resp.StatusCode, strings.TrimSpace(string(b))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Header, nil
	}
	if w, ok := out.(*[]byte); ok {
		*w, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		return resp.Header, err
	}
	return resp.Header, json.NewDecoder(resp.Body).Decode(out)
}

type ghRepo struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	HTMLURL  string `json:"html_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type ghUser struct {
	Login     string `json:"login"`
	Type      string `json:"type"`
	AvatarURL string `json:"avatar_url"`
}

type ghRun struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Actor           ghUser `json:"actor"`
	TriggeringActor ghUser `json:"triggering_actor"`
}

type ghJob struct {
	ID           int64    `json:"id"`
	RunID        int64    `json:"run_id"`
	Name         string   `json:"name"`
	WorkflowName string   `json:"workflow_name"`
	HeadBranch   string   `json:"head_branch"`
	HeadSHA      string   `json:"head_sha"`
	HTMLURL      string   `json:"html_url"`
	Status       string   `json:"status"`
	Conclusion   string   `json:"conclusion"`
	Labels       []string `json:"labels"`
	RunnerName   string   `json:"runner_name"` // who ran it: crowdvm-<sandbox id>
	CreatedAt    string   `json:"created_at"`
	StartedAt    string   `json:"started_at"`
	CompletedAt  string   `json:"completed_at"`
}

func ghTime(v string) int64 {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return 0
	}
	return store.T(t)
}

// ---------------------------------------------------------------------------
// Links (accounts connected with a token)
// ---------------------------------------------------------------------------

type ciLink struct {
	ID, OwnerID, OrgID, APIURL, Login, AccountType, Status, Provider string
	AllowPublic, allowLocal                                          bool
	PublicUsers                                                      []string
	token                                                            string
}

const ciLinkCols = `id,owner_id,COALESCE(org_id,''),api_url,login,account_type,token,allow_public,public_users,status,provider`

func (ci *ciState) scanLink(sc interface{ Scan(...any) error }) (*ciLink, error) {
	l := &ciLink{allowLocal: ci.s.Cfg.WebhookAllowLocal}
	var sealed, users string
	if err := sc.Scan(&l.ID, &l.OwnerID, &l.OrgID, &l.APIURL, &l.Login, &l.AccountType, &sealed, &l.AllowPublic, &users, &l.Status, &l.Provider); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(users), &l.PublicUsers)
	if sealed == "" { // unlinked: the token is gone, the job history stays
		return l, nil
	}
	tok, err := ci.s.openSecret(sealed)
	if err != nil {
		return nil, err
	}
	l.token = tok
	return l, nil
}

func (ci *ciState) links(where string, args ...any) []*ciLink {
	rows, err := ci.s.DB.Query(`SELECT `+ciLinkCols+` FROM ci_links WHERE `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*ciLink
	for rows.Next() {
		if l, err := ci.scanLink(rows); err == nil {
			out = append(out, l)
		}
	}
	return out
}

func (l *ciLink) gh() ciGH {
	return ciGH{api: l.APIURL, token: l.token, gitea: l.Provider == "gitea", allowLocal: l.allowLocal}
}

// instanceURL is the web root of a Gitea link (its API is <root>/api/v1).
func (l *ciLink) instanceURL() string {
	return strings.TrimSuffix(strings.TrimRight(l.APIURL, "/"), "/api/v1")
}

// visibleLinks: what the caller linked plus what is linked to the org in
// scope. Unlinked accounts are included (their job history stays visible);
// callers that need a working token check Status.
func (ci *ciState) visibleLinks(c *Ctx) ([]*ciLink, *Org, error) {
	org, err := c.Scope("")
	if err != nil {
		return nil, nil, err
	}
	if org != nil {
		return ci.links(`status IN ('active','unlinked') AND (owner_id=? OR org_id=?)`, c.P.User.ID, org.ID), org, nil
	}
	return ci.links(`status IN ('active','unlinked') AND owner_id=?`, c.P.User.ID), nil, nil
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

type ciLabel struct {
	Raw, Size, From string
	Debug           bool
}

// ciNorm maps Boat's label family (runs-on: boat, boat-large, ...) onto
// CrowdVM's, so Boat workflow files work unchanged.
func ciNorm(l string) string {
	if rest, ok := strings.CutPrefix(l, "boat"); ok && (rest == "" || rest[0] == '-' || rest[0] == '/') {
		return CILabelPrefix + rest
	}
	return l
}

// ciMatch returns the first runs-on label of the CrowdVM (or Boat) family.
func ciMatch(labels []string) (string, bool) {
	for _, l := range labels {
		base, _, _ := strings.Cut(ciNorm(l), "/")
		if base == CILabelPrefix || strings.HasPrefix(base, CILabelPrefix+"-") {
			return l, true
		}
	}
	return "", false
}

var ciSizes = map[string]string{"": "default", "small": "small", "large": "large", "xlarge": "xlarge"}

func parseCILabel(l string) (ciLabel, error) {
	parts := strings.Split(ciNorm(l), "/")
	out := ciLabel{Raw: l}
	sz, ok := strings.CutPrefix(parts[0], CILabelPrefix)
	sz = strings.TrimPrefix(sz, "-")
	if out.Size, ok = ciSizes[sz]; !ok || (sz == "" && parts[0] != CILabelPrefix) {
		return out, fmt.Errorf("unknown label %s: use %s-small, %s, %s-large or %s-xlarge", parts[0], CILabelPrefix, CILabelPrefix, CILabelPrefix, CILabelPrefix)
	}
	for _, o := range parts[1:] {
		switch {
		case o == "debug":
			out.Debug = true
		case o == "kvm" || o == "":
		case strings.HasPrefix(o, "from="):
			out.From = strings.TrimPrefix(o, "from=")
			if out.From == "" {
				return out, errors.New("label option from= needs a saved snapshot name")
			}
		default:
			return out, fmt.Errorf("unknown label option /%s (known: /from=<saved snapshot>, /debug)", o)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Controller
// ---------------------------------------------------------------------------

func (ci *ciState) loop(ctx context.Context) {
	t := time.NewTicker(ci.s.Cfg.CIPollInterval)
	defer t.Stop()
	for {
		for _, l := range ci.links(`status='active'`) {
			if ctx.Err() != nil {
				return
			}
			ci.pollLink(ctx, l)
		}
		ci.retryWaiting(ctx)
		ci.cleanup()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (ci *ciState) setLinkError(id string, err error) {
	var msg any
	if err != nil {
		msg = err.Error()
	}
	_, _ = ci.s.DB.Exec(`UPDATE ci_links SET last_poll_at=?, last_error=? WHERE id=?`, store.Now(), msg, id)
}

func (ci *ciState) pollLink(ctx context.Context, l *ciLink) {
	gh := l.gh()
	var repos []ghRepo
	var err error
	// bugs-r20-sonnet 2: every page of the account's repositories. GitHub has
	// no limit parameter (per_page=100); Gitea pages with limit (50).
	perPage, size := 100, "per_page=100"
	if gh.gitea {
		perPage, size = 50, "limit=50"
	}
	var all []ghRepo
	count := func(p *[]ghRepo) int { return len(*p) }
	add := func(p *[]ghRepo) { all = append(all, *p...) }
	if l.AccountType == "Organization" {
		err = ciPaged(ctx, gh, "/orgs/"+url.PathEscape(l.Login)+"/repos?"+size, perPage, count, add)
		repos = all
	} else {
		err = ciPaged(ctx, gh, "/user/repos?affiliation=owner&"+size, perPage, count, add)
		for _, r := range all {
			if strings.EqualFold(r.Owner.Login, l.Login) {
				repos = append(repos, r)
			}
		}
	}
	if err != nil {
		ci.setLinkError(l.ID, err)
		return
	}
	ci.setLinkError(l.ID, nil)
	_ = ci.s.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, _ = tx.Exec(`DELETE FROM ci_repos WHERE link_id=?`, l.ID)
		for _, r := range repos {
			_, _ = tx.Exec(`INSERT OR REPLACE INTO ci_repos(link_id,full_name,private,html_url) VALUES(?,?,?,?)`, l.ID, r.FullName, boolInt(r.Private), r.HTMLURL)
		}
		return nil
	})
	for _, r := range repos {
		for _, status := range []string{"queued", "in_progress"} {
			// bugs-r20-sonnet 2: page the runs too; the pages read before a
			// failure are still handled.
			type runList struct {
				WorkflowRuns []ghRun `json:"workflow_runs"`
			}
			var runs runList
			_ = ciPaged(ctx, gh, "/repos/"+r.FullName+"/actions/runs?status="+status+"&per_page=50", 50,
				func(p *runList) int { return len(p.WorkflowRuns) }, func(p *runList) { runs.WorkflowRuns = append(runs.WorkflowRuns, p.WorkflowRuns...) })
			for _, run := range runs.WorkflowRuns {
				var jobs struct {
					Jobs []ghJob `json:"jobs"`
				}
				if gh.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", r.FullName, run.ID), nil, &jobs) != nil {
					continue
				}
				for _, j := range jobs.Jobs {
					if j.Status != "queued" {
						continue
					}
					if label, ok := ciMatch(j.Labels); ok {
						ci.newJob(ctx, l, r, run, j, label)
					}
				}
			}
		}
	}
	ci.track(ctx, l)
}

// newJob records a queued job seen for the first time and starts it.
func (ci *ciState) newJob(ctx context.Context, l *ciLink, r ghRepo, run ghRun, j ghJob, label string) {
	ghID := strconv.FormatInt(j.ID, 10)
	var n int
	if ci.s.DB.QueryRow(`SELECT COUNT(*) FROM ci_jobs WHERE link_id=? AND gh_id=?`, l.ID, ghID).Scan(&n) != nil || n > 0 {
		return
	}
	// Another link (another instance) may hold the same job id: this link's
	// job then gets a key of its own (bugs-r10-sonnet 2).
	id := ghID
	if ci.s.DB.QueryRow(`SELECT COUNT(*) FROM ci_jobs WHERE id=?`, id).Scan(&n) != nil || n > 0 {
		id = ghID + "-" + l.ID
	}
	lb, lerr := parseCILabel(label)
	size := lb.Size
	if size == "" {
		size = "default"
	}
	queued := ghTime(j.CreatedAt)
	if queued == 0 {
		queued = store.Now()
	}
	now := store.Now()
	_, err := ci.s.DB.Exec(`INSERT OR IGNORE INTO ci_jobs(id,gh_id,link_id,owner_id,repo,run_id,job_name,workflow_name,head_branch,head_sha,html_url,label,machine_class,from_snapshot,debug,state,queued_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, ghID, l.ID, l.OwnerID, r.FullName, strconv.FormatInt(j.RunID, 10), j.Name, nz(j.WorkflowName), nz(j.HeadBranch), nz(j.HeadSHA), nz(j.HTMLURL),
		label, size, nz(lb.From), boolInt(lb.Debug), "queued", queued, now)
	if err != nil {
		return
	}
	if fresh := ci.links(`id=?`, l.ID); len(fresh) == 1 {
		l = fresh[0] // public-repo settings may have changed during this poll
	}
	refuse := func(msg string) {
		_, _ = ci.s.DB.Exec(`UPDATE ci_jobs SET state='refused', error=?, finished_at=?, updated_at=? WHERE id=?`, msg, now, now, id)
		ci.postStatus(ctx, l, r.FullName, j.HeadSHA, j.Name, id, "error", "Not run on CrowdVM: "+msg)
	}
	switch {
	case lerr != nil:
		refuse(lerr.Error())
		return
	case !r.Private && !l.AllowPublic:
		refuse("Public repository: jobs from public repositories do not run on CrowdVM. Turn them on in the dashboard (CI → " + l.Login + " → Public repositories) or with `crowdvm ci public " + l.Login + " on`.")
		return
	case !r.Private && len(l.PublicUsers) > 0:
		author, starter := run.Actor.Login, run.TriggeringActor.Login
		if starter == "" {
			starter = author
		}
		if !ciUserIn(l.PublicUsers, author) || !ciUserIn(l.PublicUsers, starter) {
			refuse(fmt.Sprintf("Public repository: the run's author (%s) and the user who started it (%s) must both be allowed: add them in the dashboard (CI → %s → Public repositories) or with `crowdvm ci public %s --users ...`.", author, starter, l.Login, l.Login))
			return
		}
	}
	ci.start(ctx, id)
}

func ciUserIn(list []string, u string) bool {
	for _, x := range list {
		if strings.EqualFold(x, u) {
			return true
		}
	}
	return false
}

type ciJobRow struct {
	ID, GhID, LinkID, OwnerID, Repo, RunID, JobName, Label, Class, State string
	Workflow, Branch, SHA, HTMLURL, From, Error, SandboxID, SbState      sql.NullString
	Debug                                                                bool
	QueuedAt, UpdatedAt, NextTry                                         int64
	Started, Finished, KeepUntil                                         sql.NullInt64
	Cost                                                                 sql.NullFloat64
}

const ciJobCols = `id,gh_id,link_id,owner_id,repo,run_id,job_name,label,machine_class,state,workflow_name,head_branch,head_sha,html_url,from_snapshot,error,sandbox_id,sandbox_state,debug,queued_at,updated_at,next_try_at,run_started_at,finished_at,keep_until,cost`

func scanCIJob(sc interface{ Scan(...any) error }) (*ciJobRow, error) {
	j := &ciJobRow{}
	err := sc.Scan(&j.ID, &j.GhID, &j.LinkID, &j.OwnerID, &j.Repo, &j.RunID, &j.JobName, &j.Label, &j.Class, &j.State, &j.Workflow, &j.Branch, &j.SHA, &j.HTMLURL,
		&j.From, &j.Error, &j.SandboxID, &j.SbState, &j.Debug, &j.QueuedAt, &j.UpdatedAt, &j.NextTry, &j.Started, &j.Finished, &j.KeepUntil, &j.Cost)
	return j, err
}

func (ci *ciState) jobs(where string, args ...any) []*ciJobRow {
	rows, err := ci.s.DB.Query(`SELECT `+ciJobCols+` FROM ci_jobs WHERE `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*ciJobRow
	for rows.Next() {
		if j, err := scanCIJob(rows); err == nil {
			out = append(out, j)
		}
	}
	return out
}

func (ci *ciState) setJob(id string, sets map[string]any) {
	sets["updated_at"] = store.Now()
	parts, args := []string{}, []any{}
	for k, v := range sets {
		parts = append(parts, k+"=?")
		args = append(args, v)
	}
	_, _ = ci.s.DB.Exec(`UPDATE ci_jobs SET `+strings.Join(parts, ",")+` WHERE id=?`, append(args, id)...)
}

// retryWaiting restarts queued jobs that wait on a start limit, and (bugs-r20-sonnet 7)
// queued jobs recorded without a sandbox and without a retry time: a restart
// between the insert in newJob and the sandbox create leaves such a row; start
// skips a job a goroutine already works on (busy), so a live boot is not doubled.
// Not covered: a sandbox that exists (sandbox_id set) whose runner command was
// never sent, because the process stopped during WaitUsable or before the
// command. boot returns early for such a row (SandboxID valid) and nothing
// resumes it: the sandbox idles until its TTL / cleanup and the job stays queued.
func (ci *ciState) retryWaiting(ctx context.Context) {
	for _, j := range ci.jobs(`state='queued' AND sandbox_id IS NULL AND next_try_at<=?`, store.Now()) {
		ci.start(ctx, j.ID)
	}
}

// start creates the job's sandbox in the background.
func (ci *ciState) start(_ context.Context, id string) {
	ci.mu.Lock()
	if ci.busy[id] {
		ci.mu.Unlock()
		return
	}
	ci.busy[id] = true
	ci.mu.Unlock()
	ci.s.Go(func(ctx context.Context) {
		defer func() {
			ci.mu.Lock()
			delete(ci.busy, id)
			ci.mu.Unlock()
		}()
		ci.boot(ctx, id)
	})
}

// internalCall runs an /api/v1 handler in-process as user (no HTTP).
func (s *Server) internalCall(u *User, method, path string, body any, fn HandlerFunc) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := &Ctx{W: rec, R: req, S: s, P: &Principal{User: u}, RequestID: "internal"}
	if err := fn(c); err != nil {
		c.Fail(err)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (ci *ciState) boot(ctx context.Context, id string) {
	list := ci.jobs(`id=?`, id)
	if len(list) == 0 || list[0].State != "queued" || list[0].SandboxID.Valid {
		return
	}
	j := list[0]
	links := ci.links(`id=?`, j.LinkID)
	if len(links) == 0 {
		return
	}
	l := links[0]
	u, err := ci.s.UserByID(l.OwnerID)
	if err != nil || u.Disabled {
		ci.setJob(id, map[string]any{"state": "refused", "error": "The CrowdVM account that linked " + l.Login + " is not active.", "finished_at": store.Now()})
		return
	}
	org := l.OrgID
	if org == "" {
		org = "personal"
	}
	repoShort := j.Repo[strings.LastIndex(j.Repo, "/")+1:]
	name := "ci-" + repoShort + "-" + j.ID
	if len(name) > 120 {
		name = name[:120]
	}
	body := map[string]any{"name": name, "type": j.Class, "noEnv": true, "ttlSeconds": ciSandboxTTL, "org": org,
		"labels": map[string]string{ciLabelMarker: j.ID, "crowdvm.ci.repo": strings.ReplaceAll(j.Repo, "/", "_")}}
	if j.From.Valid {
		body["from"] = j.From.String
	}
	status, out := ci.s.internalCall(u, "POST", "/sandboxes", body, ci.s.handleCreateSandbox)
	if status/100 != 2 {
		code, _ := out["code"].(string)
		msg, _ := out["message"].(string)
		details, _ := lookupMap(out, "error", "details")
		now := time.Now()
		pause := func(msg string) { // CI-009: also shown on the commit
			ci.setJob(id, map[string]any{"state": "paused", "error": msg, "finished_at": store.T(now)})
			ci.postStatus(ctx, l, j.Repo, j.SHA.String, j.JobName, id, "error", msg)
		}
		switch {
		case status == 429 || code == "out_of_capacity" || code == "no_ready_machine":
			window, _ := details["window"].(string)
			switch {
			case window == "day":
				pause("Paused by plan limit: " + msg)
			case window == "hour" && now.Sub(store.Time(j.QueuedAt)) > ciHourWait:
				pause("Paused by plan limit: " + msg + " (no start freed within 15 minutes)")
			default:
				ci.setJob(id, map[string]any{"error": "Waiting: " + msg + " Retrying every 15 seconds.", "next_try_at": store.T(now.Add(ci.s.Cfg.CIRetryInterval))})
			}
		case status == 402:
			ci.setJob(id, map[string]any{"state": "refused", "error": "Insufficient credits: " + msg, "finished_at": store.T(now)})
		case j.From.Valid && (status == 404 || strings.Contains(code, "snapshot")):
			ci.setJob(id, map[string]any{"state": "refused", "error": "Saved snapshot " + j.From.String + " cannot be used: " + msg, "finished_at": store.T(now)})
		default:
			ci.setJob(id, map[string]any{"state": "failed", "error": "Could not create the sandbox: " + msg, "finished_at": store.T(now)})
		}
		return
	}
	sbID, _ := lookupStr(out, "sandbox", "id")
	ci.setJob(id, map[string]any{"sandbox_id": sbID, "sandbox_state": "live", "error": nil, "next_try_at": 0})
	fail := func(msg string) {
		ci.setJob(id, map[string]any{"state": "failed", "error": msg, "finished_at": store.Now()})
		ci.deleteSandbox(sbID)
	}
	sb, err := ci.s.WaitUsable(ctx, sbID, 10*time.Minute)
	if err != nil {
		fail("The sandbox did not start: " + err.Error())
		return
	}
	gh := l.gh()
	conn, err := ci.s.NodeForSandbox(sb)
	if err != nil {
		fail("The sandbox's node is unreachable.")
		return
	}
	var cmd map[string]any
	if l.Provider == "gitea" {
		var reg struct {
			Token string `json:"token"`
		}
		path := "/repos/" + j.Repo + "/actions/runners/registration-token"
		err := gh.do(ctx, "POST", path, nil, &reg)
		if err != nil || reg.Token == "" {
			err = gh.do(ctx, "GET", path, nil, &reg) // Gitea before 1.24
		}
		if err != nil || reg.Token == "" {
			fail(fmt.Sprintf("Could not get a runner registration token from Gitea (the token needs repository admin access): %v", err))
			return
		}
		cmd = map[string]any{"command": ciActRunnerScript, "detached": true, "env": map[string]string{
			"CROWDVM_RUNNER_TOKEN": reg.Token, "CROWDVM_INSTANCE": l.instanceURL(), "CROWDVM_RUNNER_LABEL": j.Label, "CROWDVM_RUNNER_NAME": "crowdvm-" + sbID,
			"CROWDVM_ACT_RUNNER_URL": fmt.Sprintf(CIActRunnerURL, CIActRunnerVersion, CIActRunnerVersion)}}
	} else {
		var jit struct {
			Config string `json:"encoded_jit_config"`
		}
		if err := gh.do(ctx, "POST", "/repos/"+j.Repo+"/actions/runners/generate-jitconfig",
			map[string]any{"name": "crowdvm-" + sbID, "runner_group_id": 1, "labels": []string{j.Label}, "work_folder": "_work"}, &jit); err != nil || jit.Config == "" {
			fail(fmt.Sprintf("Could not register a runner on GitHub (the token needs repository administration access): %v", err))
			return
		}
		version := CIRunnerVersion
		var rel struct {
			Tag string `json:"tag_name"`
		}
		if gh.do(ctx, "GET", "/repos/actions/runner/releases/latest", nil, &rel) == nil && rel.Tag != "" {
			version = strings.TrimPrefix(rel.Tag, "v")
		}
		cmd = map[string]any{"command": ciRunnerScript, "detached": true,
			"env": map[string]string{"CROWDVM_JIT_CONFIG": jit.Config, "CROWDVM_RUNNER_VERSION": version}}
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := conn.Do(cctx, "POST", "/v1/vms/"+sbID+"/guest/commands", cmd, nil); err != nil {
		fail("Could not start the runner in the sandbox: " + err.Error())
	}
}

// ciRunnerScript installs the actions runner (cached in a from= snapshot)
// and runs one job with the JIT config. Jobs get 4 GB of swap (CI-005).
const ciRunnerScript = `set -e
if [ ! -e /swapfile-ci ]; then (sudo fallocate -l 4G /swapfile-ci && sudo chmod 600 /swapfile-ci && sudo mkswap /swapfile-ci >/dev/null && sudo swapon /swapfile-ci) || true; fi
mkdir -p "$HOME/actions-runner" && cd "$HOME/actions-runner"
if [ ! -x ./run.sh ]; then
  arch=x64; [ "$(uname -m)" = aarch64 ] && arch=arm64
  curl -fsSL "https://github.com/actions/runner/releases/download/v$CROWDVM_RUNNER_VERSION/actions-runner-linux-$arch-$CROWDVM_RUNNER_VERSION.tar.gz" | tar xz
fi
jit="$CROWDVM_JIT_CONFIG"; unset CROWDVM_JIT_CONFIG
exec ./run.sh --jitconfig "$jit"`

// ciActRunnerScript registers an ephemeral act_runner for one Gitea job
// (labels <label>:host: steps run on the sandbox itself) and runs it.
const ciActRunnerScript = `set -e
if [ ! -e /swapfile-ci ]; then (sudo fallocate -l 4G /swapfile-ci && sudo chmod 600 /swapfile-ci && sudo mkswap /swapfile-ci >/dev/null && sudo swapon /swapfile-ci) || true; fi
mkdir -p "$HOME/act-runner" && cd "$HOME/act-runner"
if [ ! -x ./act_runner ]; then curl -fsSL "$CROWDVM_ACT_RUNNER_URL" -o act_runner.tmp && chmod +x act_runner.tmp && mv act_runner.tmp act_runner; fi
tok="$CROWDVM_RUNNER_TOKEN"; unset CROWDVM_RUNNER_TOKEN
rm -f .runner
./act_runner register --no-interactive --ephemeral --instance "$CROWDVM_INSTANCE" --token "$tok" --name "$CROWDVM_RUNNER_NAME" --labels "$CROWDVM_RUNNER_LABEL:host"
exec ./act_runner daemon`

func lookupMap(m map[string]any, keys ...string) (map[string]any, bool) {
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return map[string]any{}, false
		}
		cur = next
	}
	return cur, true
}

func lookupStr(m map[string]any, keys ...string) (string, bool) {
	if len(keys) == 0 {
		return "", false
	}
	p, ok := lookupMap(m, keys[:len(keys)-1]...)
	if !ok {
		return "", false
	}
	s, ok := p[keys[len(keys)-1]].(string)
	return s, ok
}

func (ci *ciState) deleteSandbox(id string) {
	if id == "" {
		return
	}
	if sb, err := ci.s.SandboxByID(id); err == nil && sb.DeletedAt == nil {
		_, _ = ci.s.DeleteSandbox(sb, "explicit")
	}
	_, _ = ci.s.DB.Exec(`UPDATE ci_jobs SET sandbox_state='deleted' WHERE sandbox_id=?`, id)
}

// track follows started jobs on GitHub until they end.
func (ci *ciState) track(ctx context.Context, l *ciLink) {
	gh := l.gh()
	for _, j := range ci.jobs(`link_id=? AND state IN ('queued','running')`, l.ID) {
		if ci.isBusy(j.ID) {
			continue
		}
		var g ghJob
		if err := gh.do(ctx, "GET", "/repos/"+j.Repo+"/actions/jobs/"+j.GhID, nil, &g); err != nil {
			var he *ciHTTPError
			if errors.As(err, &he) && he.Status == 404 {
				ci.setJob(j.ID, map[string]any{"state": "cancelled", "finished_at": store.Now()})
				if !ci.runsOtherJob(j.SandboxID.String, j.ID) {
					ci.deleteSandbox(j.SandboxID.String)
				}
			}
			continue
		}
		switch g.Status {
		case "in_progress":
			if j.State != "running" {
				sets := map[string]any{"state": "running", "run_started_at": nzTime(ghTime(g.StartedAt)), "sandbox_state": "busy"}
				if sb := ci.runnerSandbox(l, g.RunnerName); sb != "" {
					sets["sandbox_id"] = sb // the sandbox that took it (opus-14 B1)
				}
				ci.setJob(j.ID, sets)
			}
		case "completed":
			state := g.Conclusion
			switch state {
			case "success", "failure", "cancelled", "skipped":
			case "timed_out":
				state = "failure"
			default:
				state = "cancelled"
			}
			fin := ghTime(g.CompletedAt)
			if fin == 0 {
				fin = store.Now()
			}
			sets := map[string]any{"state": state, "finished_at": fin}
			started := ghTime(g.StartedAt)
			if started > 0 {
				sets["run_started_at"] = started
				if sp, ok := api.SizeByName(j.Class); ok && j.SandboxID.Valid {
					sets["cost"] = float64(fin-started) / 1000 / 3600 * sp.Multiplier
				}
			}
			// A runner takes any queued job with its label, not necessarily
			// the one its sandbox was booted for: act on the sandbox that
			// actually ran this job (opus-14 B1).
			if sb := ci.runnerSandbox(l, g.RunnerName); sb != "" && sb != j.SandboxID.String {
				j.SandboxID.String, j.SandboxID.Valid = sb, true
				sets["sandbox_id"] = sb
			}
			if !j.SandboxID.Valid {
				ci.setJob(j.ID, sets)
				continue
			}
			if state == "failure" && ci.oomKilled(ctx, j.SandboxID.String) { // CI-005
				sets["error"] = "Out of memory on CrowdVM: a process of this job was killed because the sandbox ran out of memory (4 GB of swap included). Use a larger runner, e.g. runs-on: " + CILabelPrefix + "-large."
				ci.postStatus(ctx, l, j.Repo, j.SHA.String, j.JobName, j.ID, "failure", "Out of memory on CrowdVM")
			}
			if state == "failure" && j.Debug {
				sets["sandbox_state"], sets["keep_until"] = "kept", store.T(time.Now().Add(ciKeepDebug))
				ci.setJob(j.ID, sets)
				continue
			}
			ci.setJob(j.ID, sets)
			ci.deleteSandbox(j.SandboxID.String)
		}
	}
}

// runnerSandbox is the sandbox a job's runner names ("crowdvm-<id>"), or ""
// unless CI booted that sandbox for this link. Whoever runs the runner picks
// its name, so it may name any sandbox (bugs-r10-sonnet 1).
func (ci *ciState) runnerSandbox(l *ciLink, runner string) string {
	id, ok := strings.CutPrefix(runner, "crowdvm-")
	if !ok || id == "" {
		return ""
	}
	var n int
	if ci.s.DB.QueryRow(`SELECT COUNT(*) FROM ci_jobs WHERE link_id=? AND sandbox_id=?`, l.ID, id).Scan(&n) != nil || n == 0 {
		return ""
	}
	return id
}

// oomKilled pulls the sandbox's last command history and reports whether
// the kernel killed a process of the runner for lack of memory (the guest
// records oomKilled per command cgroup).
func (ci *ciState) oomKilled(ctx context.Context, sbID string) bool {
	if sb, err := ci.s.SandboxByID(sbID); err == nil && sb.Usable() {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		ci.s.historyFlush(cctx, sb)
		cancel()
	}
	var n int
	_ = ci.s.DB.QueryRow(`SELECT COUNT(*) FROM command_history WHERE sandbox_id=? AND json_extract(record,'$.oomKilled')=1`, sbID).Scan(&n)
	return n > 0
}

// postStatus puts a commit status on the job's head commit (CI-005,
// CI-009). A token can write commit statuses ("Commit statuses: write" /
// repo:status); check runs and their annotations need a GitHub App, which
// CrowdVM does not have. Best effort.
func (ci *ciState) postStatus(ctx context.Context, l *ciLink, repo, sha, jobName, jobID, state, desc string) {
	if l == nil || l.token == "" || sha == "" {
		return
	}
	if r := []rune(desc); len(r) > 140 {
		desc = string(r[:139]) + "…"
	}
	body := map[string]any{"state": state, "context": "CrowdVM / " + firstNonEmpty(jobName, "job "+jobID), "description": desc,
		"target_url": strings.TrimRight(ci.s.Cfg.PublicURL, "/") + "/dashboard?tab=ci&job=" + url.QueryEscape(jobID)}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := l.gh().do(cctx, "POST", "/repos/"+repo+"/statuses/"+url.PathEscape(sha), body, nil); err != nil {
		ci.s.Log.Debug("ci: commit status not posted", "job", jobID, "err", err)
	}
}

func nzTime(ms int64) any {
	if ms == 0 {
		return store.Now()
	}
	return ms
}

func (ci *ciState) isBusy(id string) bool {
	ci.mu.Lock()
	defer ci.mu.Unlock()
	return ci.busy[id]
}

// cleanup deletes kept /debug sandboxes after 30 minutes and CI sandboxes
// that outlive their job or 6 hours (CI-010).
func (ci *ciState) cleanup() {
	now := store.Now()
	for _, j := range ci.jobs(`sandbox_state='kept' AND keep_until<=?`, now) {
		ci.deleteSandbox(j.SandboxID.String)
	}
	for _, j := range ci.jobs(`sandbox_state IN ('live','busy') AND queued_at<?`, now-(ciMaxJob+15*time.Minute).Milliseconds()) {
		if !ci.isBusy(j.ID) {
			if j.State == "queued" || j.State == "running" {
				ci.setJob(j.ID, map[string]any{"state": "failed", "error": "The job ran longer than 6 hours.", "finished_at": now})
			}
			ci.deleteSandbox(j.SandboxID.String)
		}
	}
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

func (ci *ciState) connect(c *Ctx) error {
	var req struct {
		Org     string `json:"org"`
		Account string `json:"account"`
		Token   string `json:"token"`
		APIURL  string `json:"apiUrl"`
		LinkID  string `json:"linkId"`
		// Provider: "github" (default) or "gitea" (Gitea Actions; apiUrl
		// required, e.g. http://gitea.lan:3000/api/v1). "forgejo" is "gitea".
		Provider string `json:"provider"`
		Confirm  bool   `json:"confirm"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	if req.LinkID != "" {
		// Confirmation of a pending link (the dashboard's confirm step).
		var owner, status, org string
		if ci.s.DB.QueryRow(`SELECT owner_id, status, COALESCE(org_id,'') FROM ci_links WHERE id=?`, req.LinkID).Scan(&owner, &status, &org) != nil || owner != c.P.User.ID || !keyReachesOrg(c, org) {
			return errNotFound("CI link")
		}
		if status == "unlinked" {
			return Err(409, "ci_link_unlinked", "This link was unlinked; connect the account again with a token.")
		}
		if !req.Confirm {
			return Err(400, "confirmation_required", "Send confirm: true to link the account.")
		}
		// bugs-r20-sonnet 6: the create path's duplicate check, atomic with the
		// activation (another active link of the same forge account refuses).
		res, err := ci.s.DB.Exec(`UPDATE ci_links SET status='active', confirmed_at=? WHERE id=? AND NOT EXISTS (
			SELECT 1 FROM ci_links o, ci_links me WHERE me.id=ci_links.id AND o.id<>me.id AND lower(o.login)=lower(me.login) AND o.api_url=me.api_url AND o.status='active')`, store.Now(), req.LinkID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 && status != "active" {
			return Err(409, "ci_account_linked", "That account is already linked to CrowdVM. Unlink it first.")
		}
		return ci.linkResponse(c, req.LinkID)
	}
	if strings.TrimSpace(req.Token) == "" {
		return ErrD(400, "token_required", "CrowdVM has no GitHub App: connect with a GitHub token (fine-grained: Actions read, Administration read/write, Metadata read on the repositories, plus Members read for an organization; classic: repo + admin:org for organizations).",
			map[string]any{"fields": []string{"token", "account", "org", "apiUrl"}})
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	switch provider {
	case "", "github":
		provider = "github"
	case "gitea", "forgejo":
		provider = "gitea"
		if req.APIURL == "" {
			return Err(400, "invalid_api_url", "A Gitea link needs apiUrl, the instance's REST API, e.g. http://gitea.lan:3000/api/v1.")
		}
		if !strings.HasSuffix(strings.TrimRight(req.APIURL, "/"), "/api/v1") {
			req.APIURL = strings.TrimRight(req.APIURL, "/") + "/api/v1"
		}
	default:
		return Err(400, "invalid_provider", "provider must be github or gitea.")
	}
	apiURL := ciDefaultAPI
	if req.APIURL != "" {
		u, err := url.Parse(req.APIURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Err(400, "invalid_api_url", "apiUrl must be an http(s) URL of a GitHub-compatible REST API.")
		}
		if strings.ContainsAny(req.APIURL, "?#") { // no query or fragment, even an empty one (bugs-r5-opus 7)
			return Err(400, "invalid_api_url", "apiUrl must be the API's base URL, without a query.")
		}
		if err := ci.s.checkOutboundURL(c.R.Context(), req.APIURL, "invalid_api_url"); err != nil {
			return err
		}
		apiURL = strings.TrimRight(req.APIURL, "/")
	}
	gh := ciGH{api: apiURL, token: strings.TrimSpace(req.Token), gitea: provider == "gitea", allowLocal: ci.s.Cfg.WebhookAllowLocal}
	var me ghUser
	if err := gh.do(c.R.Context(), "GET", "/user", nil, &me); err != nil {
		return ErrD(400, "invalid_token", "The "+provider+" API refused the token: "+err.Error(), nil)
	}
	login := req.Account
	if login == "" {
		login = me.Login
	}
	var acct ghUser
	if provider == "gitea" {
		// Gitea's user objects carry no type: an account is an
		// organization when /orgs/{name} knows it.
		var org struct {
			Name     string `json:"name"`
			UserName string `json:"username"`
		}
		if gh.do(c.R.Context(), "GET", "/orgs/"+url.PathEscape(login), nil, &org) == nil {
			acct = ghUser{Login: firstNonEmpty(org.UserName, org.Name, login), Type: "Organization"}
		} else if err := gh.do(c.R.Context(), "GET", "/users/"+url.PathEscape(login), nil, &acct); err != nil {
			return Err(404, "github_account_not_found", "No Gitea account "+login+".")
		}
	} else if err := gh.do(c.R.Context(), "GET", "/users/"+url.PathEscape(login), nil, &acct); err != nil {
		return Err(404, "github_account_not_found", "No GitHub account "+login+".")
	}
	if acct.Type != "Organization" {
		acct.Type = "User"
		if !strings.EqualFold(acct.Login, me.Login) {
			return Err(400, "account_mismatch", "A personal account can only be linked with its own token.")
		}
	} else if !gh.orgAdmin(c.R.Context(), acct.Login, me.Login) {
		// Anyone can read /users/{org}: without this, any user could link an
		// organization and lock its real admins out (bugs-r10-sonnet 7).
		return Err(403, "account_mismatch", "An organization can only be linked with a token of one of its admins (the token must also be allowed to read the organization's members: classic admin:org, fine-grained Members read).")
	}
	org, err := c.Scope(req.Org) // jobs bill this org, or the wallet in scope
	if err != nil {
		return err
	}
	if org != nil {
		if role := ci.s.memberRole(org.ID, c.P.User.ID); role != "owner" && role != "admin" {
			return Err(403, "org_admin_required", "Only an owner or admin of "+org.Name+" can bill CI jobs to it.")
		}
	}
	var other string
	if ci.s.DB.QueryRow(`SELECT owner_id FROM ci_links WHERE lower(login)=lower(?) AND api_url=? AND status='active'`, acct.Login, apiURL).Scan(&other) == nil {
		return Err(409, "ci_account_linked", acct.Login+" is already linked to CrowdVM. Unlink it first.")
	}
	sealed, err := ci.s.sealSecret(gh.token)
	if err != nil {
		return err
	}
	id := "cil_" + randHex(12)
	status := "pending"
	var confirmed any
	if req.Confirm {
		status, confirmed = "active", store.Now()
	}
	_, _ = ci.s.DB.Exec(`DELETE FROM ci_links WHERE owner_id=? AND lower(login)=lower(?) AND status='pending'`, c.P.User.ID, acct.Login)
	if _, err := ci.s.DB.Exec(`INSERT INTO ci_links(id,owner_id,org_id,api_url,login,account_type,token,token_user,status,created_at,confirmed_at,provider) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, c.P.User.ID, nz(orgID(org)), apiURL, acct.Login, acct.Type, sealed, me.Login, status, store.Now(), confirmed, provider); err != nil {
		return err
	}
	return ci.linkResponse(c, id)
}

func (ci *ciState) linkResponse(c *Ctx, id string) error {
	ls := ci.links(`id=?`, id)
	if len(ls) == 0 {
		return errNotFound("CI link")
	}
	l := ls[0]
	confirmURL := strings.TrimRight(ci.s.Cfg.PublicURL, "/") + "/dashboard?tab=ci&confirm=" + l.ID
	return c.OK(200, "ci.connect", map[string]any{"installUrl": confirmURL, "link": map[string]any{
		"id": l.ID, "accountLogin": l.Login, "accountType": l.AccountType, "teamId": nz(l.OrgID), "status": l.Status, "provider": l.Provider, "apiUrl": l.APIURL,
		"owner": c.P.User.Username, "confirmRequired": l.Status == "pending"}})
}

// keyReachesOrg: an organization key reaches only its own organization's
// CI links (bugs-r4-sol 5); other credentials reach all of the user's.
func keyReachesOrg(c *Ctx, orgID string) bool {
	k := c.P.Key
	return k == nil || k.OrgID == "" || k.OrgID == orgID
}

// unlink forgets the token and the repositories. Jobs still queued or
// running are cancelled (their sandboxes deleted); the job history stays
// readable (the link is kept as "unlinked"). A pending link is dropped.
func (ci *ciState) unlink(c *Ctx) error {
	id := c.Param("linkId")
	var owner, status, org string
	if ci.s.DB.QueryRow(`SELECT owner_id, status, COALESCE(org_id,'') FROM ci_links WHERE id=?`, id).Scan(&owner, &status, &org) != nil || owner != c.P.User.ID || status == "unlinked" || !keyReachesOrg(c, org) {
		return errNotFound("CI link")
	}
	if status == "pending" {
		_, _ = ci.s.DB.Exec(`DELETE FROM ci_links WHERE id=?`, id)
		return c.OK(200, "ci.unlinked", map[string]any{"id": id, "cancelledJobs": 0})
	}
	_, _ = ci.s.DB.Exec(`UPDATE ci_links SET status='unlinked', token='', last_error=NULL WHERE id=?`, id)
	_, _ = ci.s.DB.Exec(`DELETE FROM ci_repos WHERE link_id=?`, id)
	n := 0
	for _, j := range ci.jobs(`link_id=? AND state IN ('queued','running')`, id) {
		ci.setJob(j.ID, map[string]any{"state": "cancelled", "error": "The GitHub account was unlinked from CrowdVM.", "finished_at": store.Now(), "next_try_at": 0})
		ci.deleteSandbox(j.SandboxID.String)
		n++
	}
	for _, j := range ci.jobs(`link_id=? AND sandbox_state='kept'`, id) {
		ci.deleteSandbox(j.SandboxID.String)
	}
	return c.OK(200, "ci.unlinked", map[string]any{"id": id, "cancelledJobs": n})
}

var ciUserSplit = regexp.MustCompile(`[\s,]+`)

func parseCIUsers(raw json.RawMessage) ([]string, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var items []string
	var s string
	if json.Unmarshal(raw, &s) == nil {
		items = ciUserSplit.Split(s, -1)
	} else if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false, Err(400, "invalid_users", "users must be a string or a list of strings.")
	}
	var out []string
	for _, it := range items {
		it = strings.TrimSpace(it)
		for _, p := range []string{"https://github.com/", "http://github.com/", "github.com/", "@"} {
			it = strings.TrimPrefix(it, p)
		}
		it = strings.Trim(it, "/")
		if it != "" && !slices.Contains(out, it) {
			out = append(out, it)
		}
	}
	return out, true, nil
}

func (ci *ciState) public(c *Ctx) error {
	var req struct {
		Account string          `json:"account"`
		Allow   *bool           `json:"allow"`
		Users   json.RawMessage `json:"users"`
		DryRun  bool            `json:"dryRun"`
		Org     string          `json:"org"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	if req.Account == "" {
		return Err(400, "account_required", "account is required (the linked GitHub login).")
	}
	ls := ci.links(`status='active' AND (lower(login)=lower(?) OR id=?)`, req.Account, req.Account)
	if len(ls) == 0 {
		return errNotFound("Linked GitHub account " + req.Account)
	}
	// Logins are unique per instance only: take the caller's own link
	// (bugs-r10-sonnet 8).
	var l *ciLink
	for _, x := range ls {
		allowed := x.OwnerID == c.P.User.ID
		if !allowed && x.OrgID != "" {
			role := ci.s.memberRole(x.OrgID, c.P.User.ID)
			allowed = role == "owner" || role == "admin"
		}
		if allowed {
			l = x
			break
		}
	}
	if l == nil {
		return Err(403, "ci_link_forbidden", "Only the person who linked "+ls[0].Login+", or an owner or admin of its organization, can change this.")
	}
	users, set, err := parseCIUsers(req.Users)
	if err != nil {
		return err
	}
	if !set {
		users = l.PublicUsers
	}
	gh := l.gh()
	out := []map[string]any{}
	var canon []string
	for _, u := range users {
		var gu ghUser
		if err := gh.do(c.R.Context(), "GET", "/users/"+url.PathEscape(u), nil, &gu); err != nil {
			return ErrD(400, "unknown_github_user", "GitHub has no user "+u+".", map[string]any{"user": u})
		}
		canon = append(canon, gu.Login)
		out = append(out, map[string]any{"login": gu.Login, "avatarUrl": nz(gu.AvatarURL)})
	}
	allow := l.AllowPublic
	if req.Allow != nil {
		allow = *req.Allow
	}
	if canon == nil {
		canon = []string{}
	}
	if !req.DryRun {
		_, _ = ci.s.DB.Exec(`UPDATE ci_links SET allow_public=?, public_users=? WHERE id=?`, boolInt(allow), mustJSON(canon), l.ID)
	}
	return c.OK(200, "ci.public", map[string]any{"allowPublicRepos": allow, "publicRepoUsers": out, "dryRun": req.DryRun})
}

// ciSeconds is end-start in seconds, never negative (GitHub's and this
// server's clocks differ; a job may start "before" it was queued).
func ciSeconds(start, end int64) float64 { return max(0, float64(end-start)/1000) }

// dollars converts credits (machine hours of a default sandbox) to the
// Boat-compatible dollar amount.
func (s *Server) dollars(credits float64) float64 {
	return round6(credits * 3600 / float64(s.Cfg.SecondsPerDollar))
}

func (ci *ciState) jobJSON(j *ciJobRow) map[string]any {
	var wait, run any
	if j.Started.Valid {
		wait = ciSeconds(j.QueuedAt, j.Started.Int64)
		if j.Finished.Valid {
			run = ciSeconds(j.Started.Int64, j.Finished.Int64)
		}
	}
	var cost, credits any
	if j.Cost.Valid { // stored in credits
		cost, credits = ci.s.dollars(j.Cost.Float64), round6(j.Cost.Float64)
	}
	var sbState any
	if j.SbState.Valid {
		sbState = j.SbState.String
	}
	return map[string]any{"id": j.ID, "repo": j.Repo, "runId": j.RunID, "jobName": j.JobName, "workflowName": nullStr(j.Workflow),
		"headBranch": nullStr(j.Branch), "headSha": nullStr(j.SHA), "htmlUrl": nullStr(j.HTMLURL), "label": j.Label, "machineClass": j.Class,
		"state": j.State, "error": nullStr(j.Error), "sandboxId": nullStr(j.SandboxID), "sandboxState": sbState,
		"queuedAt": tsMs(j.QueuedAt), "runStartedAt": tsNull(j.Started), "finishedAt": tsNull(j.Finished),
		"waitSeconds": wait, "runSeconds": run, "costDollars": cost, "costCredits": credits}
}

func (ci *ciState) jobFor(c *Ctx) (*ciJobRow, *ciLink, error) {
	links, _, err := ci.visibleLinks(c)
	if err != nil {
		return nil, nil, err
	}
	js := ci.jobs(`id=?`, c.Param("jobId"))
	if len(js) == 0 {
		return nil, nil, errNotFound("CI job")
	}
	for _, l := range links {
		if l.ID == js[0].LinkID {
			return js[0], l, nil
		}
	}
	return nil, nil, errNotFound("CI job")
}

func (ci *ciState) getJob(c *Ctx) error {
	j, _, err := ci.jobFor(c)
	if err != nil {
		return err
	}
	return c.OK(200, "ci.job", map[string]any{"job": ci.jobJSON(j)})
}

func (ci *ciState) jobLogs(c *Ctx) error {
	j, l, err := ci.jobFor(c)
	if err != nil {
		return err
	}
	if !j.Started.Valid {
		return c.OK(200, "ci.job.logs", map[string]any{"log": ""})
	}
	if l.Status != "active" {
		return c.OK(200, "ci.job.logs", map[string]any{"log": "", "unavailable": "The GitHub account " + l.Login + " is unlinked: its logs stay on GitHub " + j.HTMLURL.String + "."})
	}
	var b []byte
	if err := l.gh().do(c.R.Context(), "GET", "/repos/"+j.Repo+"/actions/jobs/"+j.GhID+"/logs", nil, &b); err != nil {
		var he *ciHTTPError
		if errors.As(err, &he) && he.Status == 404 {
			return c.OK(200, "ci.job.logs", map[string]any{"log": ""})
		}
		return ErrD(502, "github_unavailable", "Could not read the log from GitHub: "+err.Error(), map[string]any{"retryable": true})
	}
	if len(b) > ciLogMax {
		b = b[len(b)-ciLogMax:]
	}
	return c.OK(200, "ci.job.logs", map[string]any{"log": string(b)})
}

func (ci *ciState) overview(c *Ctx) error {
	q := c.R.URL.Query()
	acct, repo, state := q.Get("account"), q.Get("repo"), q.Get("state")
	if state == "" {
		state = "all"
	}
	if state != "all" && state != "active" && state != "failed" {
		return Err(400, "invalid_state", "state must be all, active or failed.")
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return Err(400, "invalid_limit", "limit must be an integer from 1 to 500.")
		}
		limit = n
	}
	links, _, err := ci.visibleLinks(c)
	if err != nil {
		return err
	}
	pendingLinks := []map[string]any{}
	for _, l := range ci.links(`owner_id=? AND status='pending'`, c.P.User.ID) {
		if !keyReachesOrg(c, l.OrgID) {
			continue
		}
		pendingLinks = append(pendingLinks, map[string]any{"id": l.ID, "accountLogin": l.Login, "accountType": l.AccountType, "teamId": nz(l.OrgID),
			"confirmUrl": strings.TrimRight(ci.s.Cfg.PublicURL, "/") + "/dashboard?tab=ci&confirm=" + l.ID})
	}
	pending := len(pendingLinks)
	var linkIDs []any
	var shown []*ciLink // active links: installations
	active := 0
	for _, l := range links {
		if acct == "" || strings.EqualFold(acct, l.Login) {
			if l.Status == "active" {
				shown = append(shown, l)
			}
			linkIDs = append(linkIDs, l.ID)
		}
		if l.Status == "active" {
			active++
		}
	}
	var all []*ciJobRow
	weekAgo := store.T(time.Now().Add(-7 * 24 * time.Hour))
	if len(linkIDs) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(linkIDs)), ",")
		all = ci.jobs(`link_id IN (`+ph+`) ORDER BY queued_at DESC, id DESC`, linkIDs...)
	}
	var inScope []*ciJobRow
	for _, j := range all {
		if repo == "" || strings.EqualFold(repo, j.Repo) {
			inScope = append(inScope, j)
		}
	}
	isActive := func(j *ciJobRow) bool { return j.State == "queued" || j.State == "running" }
	isFailed := func(j *ciJobRow) bool {
		return j.State == "failure" || j.State == "failed" || j.State == "refused" || j.State == "paused"
	}
	// Jobs list, counts, stats (7 days).
	counts := map[string]int{"all": 0, "active": 0, "failed": 0}
	var waits, runs []float64
	var cost float64
	succ, done, running := 0, 0, 0
	perDay := map[string][3]int{}
	jobsOut := []map[string]any{}
	type problem struct {
		jobID, repo, msg string
		at               int64
		count            int
	}
	probs := map[string]*problem{}
	for _, j := range inScope {
		recent := j.QueuedAt >= weekAgo
		if recent {
			counts["all"]++
			if isActive(j) {
				counts["active"]++
				if j.State == "running" {
					running++
				}
			}
			if isFailed(j) {
				counts["failed"]++
			}
			day := store.Time(j.QueuedAt).UTC().Format("2006-01-02")
			d := perDay[day]
			switch j.State {
			case "success":
				d[0]++
				succ++
				done++
			case "failure", "failed":
				d[1]++
				done++
			default:
				d[2]++
			}
			perDay[day] = d
			if j.Started.Valid {
				waits = append(waits, ciSeconds(j.QueuedAt, j.Started.Int64))
				if j.Finished.Valid {
					runs = append(runs, ciSeconds(j.Started.Int64, j.Finished.Int64))
				}
			}
			if j.Cost.Valid {
				cost += j.Cost.Float64
			}
			if (j.State == "refused" || j.State == "failed" || j.State == "paused") && j.Error.Valid {
				k := j.Repo + "\x00" + j.Error.String
				if p := probs[k]; p != nil {
					p.count++
				} else {
					probs[k] = &problem{j.ID, j.Repo, j.Error.String, j.QueuedAt, 1}
				}
			}
		}
		if len(jobsOut) < limit && (state == "all" || (state == "active" && isActive(j)) || (state == "failed" && isFailed(j))) {
			jobsOut = append(jobsOut, ci.jobJSON(j))
		}
	}
	median := func(v []float64) any {
		if len(v) == 0 {
			return nil
		}
		sort.Float64s(v)
		return v[len(v)/2]
	}
	var rate any
	if done > 0 {
		rate = float64(succ) / float64(done)
	}
	var days []map[string]any
	for i := 6; i >= 0; i-- {
		day := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		d := perDay[day]
		days = append(days, map[string]any{"day": day, "success": d[0], "failure": d[1], "other": d[2]})
	}
	problems := []map[string]any{}
	for _, p := range probs {
		problems = append(problems, map[string]any{"jobId": p.jobID, "repo": p.repo, "message": p.msg, "at": tsMs(p.at), "count": p.count})
	}
	sort.Slice(problems, func(i, k int) bool { return problems[i]["at"].(string) > problems[k]["at"].(string) })

	// Installations with repository status.
	insts := []map[string]any{}
	repoCount := 0
	for _, l := range shown {
		rows, err := ci.s.DB.Query(`SELECT full_name, private, html_url FROM ci_repos WHERE link_id=? ORDER BY full_name`, l.ID)
		repos := []map[string]any{}
		if err == nil {
			for rows.Next() {
				var name, html string
				var priv bool
				if rows.Scan(&name, &priv, &html) != nil || (repo != "" && !strings.EqualFold(repo, name)) {
					continue
				}
				n7, lastState, lastAt, lastErr := 0, any(nil), any(nil), any(nil)
				for _, j := range all {
					if j.Repo != name {
						continue
					}
					if lastAt == nil {
						lastState, lastAt, lastErr = j.State, tsMs(j.QueuedAt), nullStr(j.Error)
					}
					if j.QueuedAt >= weekAgo {
						n7++
					}
				}
				st := "ok"
				switch {
				case !priv && !l.AllowPublic:
					st = "public-blocked"
				case n7 == 0:
					st = "no-jobs"
				case lastState == "failure" || lastState == "failed" || lastState == "refused" || lastState == "paused":
					st = "failing"
				}
				repos = append(repos, map[string]any{"fullName": name, "private": priv, "htmlUrl": html, "status": st, "jobs7d": n7,
					"lastState": lastState, "lastAt": lastAt, "lastError": lastErr})
			}
			rows.Close()
		}
		repoCount += len(repos)
		users := l.PublicUsers
		if users == nil {
			users = []string{}
		}
		web := strings.Replace(strings.Replace(l.APIURL, "://api.github.com", "://github.com", 1), "/api/v3", "", 1)
		insts = append(insts, map[string]any{"id": l.ID, "accountLogin": l.Login, "accountType": l.AccountType, "teamId": nz(l.OrgID),
			"allowPublicRepos": l.AllowPublic, "publicRepoUsers": users, "manageUrl": web + "/" + l.Login, "repositories": repos})
	}
	anyJob, anyPass := len(all) > 0, false
	for _, j := range all {
		if j.State == "success" {
			anyPass = true
			break
		}
	}
	steps := []map[string]any{
		{"id": "install", "title": "Connect GitHub with a token", "done": active > 0,
			"detail": map[bool]string{true: "Linked.", false: map[bool]string{
				true:  "Confirm the pending link (dashboard CI tab, or `crowdvm ci connect --link <link id> --yes`).",
				false: "Connect a GitHub token in the dashboard (CI tab → Connect) or run `crowdvm ci connect --token <GitHub token>`.",
			}[pending > 0]}[active > 0]},
		{"id": "repositories", "title": "Repositories visible to the token", "done": repoCount > 0, "detail": fmt.Sprintf("%d repositories.", repoCount)},
		{"id": "workflow", "title": "Use runs-on: " + CILabelPrefix + " in a workflow", "done": anyJob, "detail": "runs-on: " + CILabelPrefix + " (or " + CILabelPrefix + "-small, -large, -xlarge)."},
		{"id": "first-pass", "title": "A job passed", "done": anyPass, "detail": "The first green job on CrowdVM."},
	}
	var next any
	for _, st := range steps {
		if !st["done"].(bool) {
			next = st["id"]
			break
		}
	}
	return c.OK(200, "ci.overview", map[string]any{
		"setup":         map[string]any{"complete": next == nil, "next": next, "steps": steps, "problems": problems},
		"installations": insts,
		"pendingLinks":  pendingLinks,
		"filter":        map[string]any{"account": nz(acct), "repo": nz(repo), "state": state},
		"counts":        counts,
		"stats": map[string]any{"jobs": counts["all"], "running": running, "successRate": rate, "medianWaitSeconds": median(waits),
			"medianRunSeconds": median(runs), "costDollars": ci.s.dollars(cost), "costCredits": round6(cost), "perDay": days},
		"jobs": jobsOut,
	})
}

// runsOtherJob: another running job ran on sandbox sbID (a runner takes any
// queued job with its label), so it must not be deleted for jobID.
func (ci *ciState) runsOtherJob(sbID, jobID string) bool {
	if sbID == "" {
		return false
	}
	var n int
	_ = ci.s.DB.QueryRow(`SELECT COUNT(*) FROM ci_jobs WHERE sandbox_id=? AND id<>? AND state='running'`, sbID, jobID).Scan(&n)
	return n > 0
}
