package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r10-sonnet 1: ciState.track (feat_ci.go:851) trusts the job's
// runner_name ("crowdvm-<sandbox id>") from the CI provider and, when the job
// completes, deletes THAT sandbox with no check that it is a sandbox CI booted
// for this link. The runner name is chosen by whoever runs the job (a
// self-hosted runner named crowdvm-<id> in the linked repository), so a user
// who links their own GitHub account can delete any sandbox whose id they
// know (shared ids, logs, support threads), in any account.
func TestBughuntR10CIRunnerNameDeletesForeignSandbox(t *testing.T) {
	gh := newFakeGitHub(t)
	guest := &cmdGuest{FakeGuest: servertest.NewFakeGuest()}
	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = 50 * time.Millisecond })
	env.StartNode(servertest.NodeOptions{Guest: guest})
	key := env.AdminKey
	_, victim := env.NewUser("r10victim", false)
	victimSB := createSandbox(t, env, victim, nil)
	env.WaitState(victim, victimSB, "ready")

	linkID := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_test", "apiUrl": gh.srv.URL}).Str("link.id")
	env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"linkId": linkID, "confirm": true})
	gh.addJob("3001", 1, "alice/api", "crowdvm")
	env.Eventually("job has a sandbox", func() bool {
		return env.Do(key, "GET", "/api/v1/ci/jobs/3001", nil).Str("job.sandboxId") != ""
	})
	now := time.Now().UTC().Format(time.RFC3339)
	// The attacker's own runner took the job and calls itself after the victim's sandbox.
	gh.set("3001", "status", "completed", "conclusion", "success", "started_at", now, "completed_at", now, "runner_name", "crowdvm-"+victimSB)
	env.WaitFor(key, "/api/v1/ci/jobs/3001", "job.state", 30*time.Second, "success")
	time.Sleep(500 * time.Millisecond)
	if r := env.Do(victim, "GET", "/api/v1/sandboxes/"+victimSB, nil); r.Status != 200 {
		t.Fatalf("another account's sandbox was deleted by a CI job it has nothing to do with: %s", r)
	}
}

// bugs-r10-sonnet 2: ci_jobs.id is the provider's job id alone
// (feat_ci.go:484 dedupes on it across all links). Job ids are only unique per
// provider instance (Gitea counts 1, 2, 3 on each instance), so a second
// link's job with the same id is silently dropped: it never runs and the
// provider keeps it queued.
func TestBughuntR10CIJobIDCollidesAcrossLinks(t *testing.T) {
	gh1, gh2 := newFakeGitHub(t), newFakeGitHub(t)
	guest := &cmdGuest{FakeGuest: servertest.NewFakeGuest()}
	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = 50 * time.Millisecond })
	env.StartNode(servertest.NodeOptions{Guest: guest})
	a := env.AdminKey
	_, b := env.NewUser("r10carol", false)
	for _, x := range []struct{ key, url string }{{a, gh1.srv.URL}, {b, gh2.srv.URL}} {
		id := env.Do(x.key, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_test", "apiUrl": x.url}).Str("link.id")
		if r := env.Do(x.key, "POST", "/api/v1/ci/connect", map[string]any{"linkId": id, "confirm": true}); r.Status != 200 {
			t.Fatalf("confirm: %s", r)
		}
	}
	gh1.addJob("1", 1, "alice/api", "crowdvm")
	env.Eventually("first instance's job 1 has a sandbox", func() bool {
		return env.Do(a, "GET", "/api/v1/ci/jobs/1", nil).Str("job.sandboxId") != ""
	})
	gh2.addJob("1", 1, "alice/api", "crowdvm") // another instance, another account: its own job 1
	time.Sleep(time.Second)
	r := env.Do(b, "GET", "/api/v1/ci", nil)
	if jobs, _ := r.Get("jobs").([]any); len(jobs) == 0 {
		t.Fatalf("the second account's queued job was dropped because another instance has a job with the same id: %s", r)
	}
}

// bugs-r10-sonnet 3: handleCreateSandbox checks the concurrency limit
// (sandboxes.go:900) and the start window (:904) long before it inserts the
// sandbox (:983) and records the start (:1005), with no lock or transaction
// between. Requests that arrive together all pass the check, so the limits
// (maxActive, starts per minute/hour/day, and the credit/extra-start gates)
// can be exceeded at will by a script that sends its creates in parallel.
func TestBughuntR10ConcurrentCreatesBypassMaxActive(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.MaxActive = 1 })
	env.StartNode(servertest.NodeOptions{})
	_, key := env.NewUser("r10racer", false)
	const n = 12
	res := make(chan int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			res <- env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{}).Status
		}()
	}
	close(start)
	ok := 0
	for i := 0; i < n; i++ {
		if st := <-res; st == 202 {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("maxActive is 1 but %d of %d parallel creates were accepted", ok, n)
	}
}

// bugs-r10-sonnet 4: the allowed-sizes policy (acct_billing.go:833) looks only
// at the request's "type". A fork (lifecycle.go:1668 takes src.Size) and a
// create from a saved snapshot (sandboxes.go:860 takes src.Size) name no type,
// so they inherit the source's size and skip the policy: the account gets a
// "large" sandbox the administrator's policy does not allow.
func TestBughuntR10ForkIgnoresAllowedSizesPolicy(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	u, key := env.NewUser("r10sizes", false)
	id := createSandbox(t, env, key, map[string]any{"type": "large"})
	env.WaitState(key, id, "ready")
	if r := env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/"+u.Username, map[string]any{"allowedSizes": []string{"small", "default"}}); r.Status != 200 {
		t.Fatalf("policy: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "large"}); r.Status != 403 {
		t.Fatalf("policy not enforced on create with a type: %s", r)
	}
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/fork", map[string]any{})
	if r.Status/100 == 2 {
		t.Fatalf("fork without a type created a %q sandbox although the policy allows only small and default: %s", r.Str("sandbox.type"), r)
	}
}

// bugs-r10-sonnet 5: POST /account/erase ("GDPR erasure: data and identity go
// now", feat_retention.go:366) purges sandboxes, saved snapshots, webhooks and
// telemetry, but keeps the account's AI credential profiles (provider API keys
// and sign-in tokens) and its CI link with the GitHub/Gitea token, which the
// CI poller keeps using.
func TestBughuntR10EraseKeepsCredentialsAndCIToken(t *testing.T) {
	gh := newFakeGitHub(t)
	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = 50 * time.Millisecond })
	u, key := env.NewUser("r10erase", false)
	browser, err := env.S.NewSession(u.ID, "browser", "test")
	if err != nil {
		t.Fatal(err)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/anthropic-api-key", map[string]any{"value": "sk-ant-erase-me-0001"}); r.Status != 200 {
		t.Fatalf("key: %s", r)
	}
	link := env.Do(key, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_test", "apiUrl": gh.srv.URL, "confirm": true})
	if link.Status != 200 || link.Str("link.status") != "active" {
		t.Fatalf("connect: %s", link)
	}
	if r := env.Do(browser, "POST", "/api/v1/account/erase", map[string]any{"confirmation": "erase my account"}); r.Status != 200 {
		t.Fatalf("erase: %s", r)
	}
	var secrets, tokens int
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM credential_accounts a JOIN credential_profiles p ON p.id=a.profile_id WHERE p.owner_id=?`, u.ID).Scan(&secrets)
	_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM ci_links WHERE owner_id=? AND token!=''`, u.ID).Scan(&tokens)
	if secrets != 0 {
		t.Errorf("%d stored provider credential(s) of the erased account remain", secrets)
	}
	if tokens != 0 {
		t.Errorf("the erased account's CI link still holds its GitHub token (%d link(s))", tokens)
	}
}

// bugs-r10-sonnet 6: admin merge moves "everything of USER" (docs/guide,
// cli-reference.md:124) but mergeMoves (acct_settings.go:412) leaves out the
// OpenTelemetry export (telemetry.owner_id) and others. The merged account's
// sandboxes now belong to the target, whose lifecycle logs and metrics no
// longer reach the collector the sandbox owner configured.
func TestBughuntR10MergeLeavesTelemetryBehind(t *testing.T) {
	col := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer col.Close()
	env := servertest.New(t)
	_, from := env.NewUser("r10from", false)
	if r := env.Do(from, "PUT", "/api/v1/telemetry", map[string]any{"endpoint": col.URL}); r.Status != 200 {
		t.Fatalf("telemetry: %s", r)
	}
	_, into := env.NewUser("r10into2", false)
	if r := env.Do(env.AdminKey, "POST", "/api/v1/admin/users/r10from/merge", map[string]any{"into": "r10into2"}); r.Status != 200 {
		t.Fatalf("merge: %s", r)
	}
	r := env.Do(into, "GET", "/api/v1/telemetry", nil)
	if r.Get("telemetry") == nil {
		t.Fatalf("the merged account's OpenTelemetry export did not move to the target: %s", r)
	}
}

// orgGitHub is a GitHub whose org "acme" is administered by alice only. Any
// token can read the public /users/acme and /orgs/acme/repos (as on GitHub);
// only alice's token sees a membership.
func orgGitHub(t *testing.T) *httptest.Server {
	js := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	me := func(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok_") }
	m := http.NewServeMux()
	m.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if u := me(r); u == "alice" || u == "mallory" {
			js(w, map[string]any{"login": u, "type": "User"})
			return
		}
		w.WriteHeader(401)
	})
	m.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("login") == "acme" {
			js(w, map[string]any{"login": "acme", "type": "Organization"})
			return
		}
		w.WriteHeader(404)
	})
	m.HandleFunc("GET /orgs/acme/repos", func(w http.ResponseWriter, r *http.Request) { js(w, []any{}) })
	member := func(w http.ResponseWriter, r *http.Request) {
		if me(r) == "alice" {
			js(w, map[string]any{"role": "admin", "state": "active"})
			return
		}
		w.WriteHeader(404)
	}
	m.HandleFunc("GET /user/memberships/orgs/acme", member)
	m.HandleFunc("GET /orgs/acme/memberships/{u}", member)
	m.HandleFunc("GET /orgs/acme/actions/runners", member)
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return srv
}

// bugs-r10-sonnet 7: ciState.connect (feat_ci.go:1010-1047) verifies for a
// personal account that the token is its own (account_mismatch), but for an
// organization it only checks that /users/{login} exists. Any user with any
// valid GitHub token can link (and confirm) any organization's login; the
// 409 ci_account_linked then locks the real administrator out until the
// squatter unlinks.
func TestBughuntR10CILinkSquatsForeignOrg(t *testing.T) {
	gh := orgGitHub(t)
	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = time.Hour })
	_, mallory := env.NewUser("r10mallory", false)
	_, alice := env.NewUser("r10alice", false)
	body := func(tok string) map[string]any {
		return map[string]any{"token": "tok_" + tok, "apiUrl": gh.URL, "account": "acme", "confirm": true}
	}
	_ = env.Do(mallory, "POST", "/api/v1/ci/connect", body("mallory")) // not a member of acme
	r := env.Do(alice, "POST", "/api/v1/ci/connect", body("alice"))
	if r.Status != 200 {
		t.Fatalf("the organization's own administrator cannot link it after a non-member linked it first: %s", r)
	}
}

// bugs-r10-sonnet 8: ciState.public (feat_ci.go:1154) picks the first active
// link by login among ALL accounts' links. Logins are only unique per API
// URL (a GitHub "alice" and a Gitea "alice"), so the owner of the second link
// is refused (403) for their own account, and can only change it by link id.
func TestBughuntR10CIPublicPicksAnotherAccountsLink(t *testing.T) {
	gh1, gh2 := newFakeGitHub(t), newFakeGitHub(t)
	env := servertest.New(t, func(c *server.Config) { c.CIPollInterval = time.Hour })
	a := env.AdminKey
	_, b := env.NewUser("r10dave", false)
	for _, x := range []struct{ key, url string }{{a, gh1.srv.URL}, {b, gh2.srv.URL}} {
		if r := env.Do(x.key, "POST", "/api/v1/ci/connect", map[string]any{"token": "ghp_test", "apiUrl": x.url, "confirm": true}); r.Status != 200 {
			t.Fatalf("connect: %s", r)
		}
	}
	r := env.Do(b, "POST", "/api/v1/ci/public", map[string]any{"account": "alice", "allow": true})
	if r.Status != 200 {
		t.Fatalf("the owner of the second 'alice' link cannot change it by login: %s", r)
	}
}

// bugs-r10-sonnet 9: chargeExtraStart (acct_billing.go:268) writes the
// extra_start ledger row with no actor. spentThisMonth(wallet, member)
// (credits.go:96, acct_billing.go:164, enforceCaps) counts 'extra_start' rows
// per actor, so a member's organization usage cap never sees the extra starts
// the member bought, and the member can keep starting past it.
func TestBughuntR10MemberCapIgnoresExtraStarts(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.StartsPerMinute = 1 })
	env.StartNode(servertest.NodeOptions{})
	admin := env.AdminKey
	bob, key := env.NewUser("r10bob", false)
	r := env.Do(admin, "POST", "/api/v1/orgs", map[string]any{"name": "R10Org"})
	oid := r.Str("org.id")
	invite := env.Do(admin, "POST", "/api/v1/orgs/r10org/invites", map[string]any{"email": "r10bob@example.test"})
	if r := env.Do(key, "POST", "/api/v1/org-invites/accept", map[string]any{"code": invite.Str("code")}); r.Status != 200 {
		t.Fatalf("accept: %s", r)
	}
	if err := env.S.GrantCredits(oid, 10, "test", ""); err != nil {
		t.Fatal(err)
	}
	if r := env.Do(admin, "PUT", "/api/v1/billing/extra-starts", map[string]any{"enabled": true, "org": "r10org"}); r.Status != 200 {
		t.Fatalf("extra starts: %s", r)
	}
	// 0.04 credits: less than one extra start (0.05).
	if r := env.Do(admin, "PATCH", "/api/v1/orgs/r10org/members/"+bob.ID, map[string]any{"usageCapCredits": 0.04}); r.Status != 200 {
		t.Fatalf("cap: %s", r)
	}
	for i := 0; i < 2; i++ { // the second is an extra start, paid by the org
		if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"org": "r10org", "noEnv": true}); r.Status != 202 {
			t.Fatalf("create %d: %s", i, r)
		}
	}
	var spent float64
	_ = env.S.DB.QueryRow(`SELECT COALESCE(-SUM(delta),0) FROM credit_ledger WHERE wallet_id=? AND kind='extra_start'`, oid).Scan(&spent)
	if spent < 0.05-1e-9 {
		t.Fatalf("setup: no extra start was charged (%v)", spent)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"org": "r10org", "noEnv": true}); r.Status != 402 || r.Code() != "team_member_cap_reached" {
		t.Fatalf("the member spent %.2f credits on extra starts, over the 0.04 cap, and can still start: %s", spent, r)
	}
}

// bugs-r10-sonnet 10: handleAdminPolicyPut (acct_billing.go:751) range-checks
// the integer limits but not the float ones. A negative creditCapMonthly is
// stored (HTTP 200) and then makes checkCreditCap true for every wallet use
// (used >= -1): all starts are refused with credit_cap_reached and
// enforceCaps stops the wallet's running sandboxes on the next billing tick.
func TestBughuntR10PolicyAcceptsNegativeCreditCap(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	_, key := env.NewUser("r10cap", false)
	r := env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/r10cap", map[string]any{"creditCapMonthly": -1})
	if r.Status == 200 {
		if c := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{}); c.Status == 402 {
			t.Fatalf("a negative creditCapMonthly was accepted (%d) and now refuses every start: %s", r.Status, c.Code())
		}
		t.Fatalf("a negative creditCapMonthly was accepted: %s", r)
	}
}

// bugs-r10-sonnet 11: two admin grants of 1.7e308 add up to +Inf in SQLite;
// every response that carries the wallet then drops all its fields (status 200,
// body just {ok,type}: GET /credits, the grant response, transfers) and the
// balance cannot be corrected through the API. handleAdminGrant
// (account.go:229) validates only amount != 0.
func TestBughuntR10GrantOverflowBreaksWalletResponses(t *testing.T) {
	env := servertest.New(t)
	_, key := env.NewUser("r10inf", false)
	var last *servertest.Resp
	for i := 0; i < 2; i++ {
		last = env.Do(env.AdminKey, "POST", "/api/v1/admin/credits", map[string]any{"user": "r10inf", "amount": 1.7e308})
		if last.Status/100 != 2 {
			return // refused: fine
		}
	}
	if last.Get("wallet") == nil {
		t.Errorf("the grant answered %d without the wallet: %s", last.Status, last)
	}
	if r := env.Do(key, "GET", "/api/v1/credits", nil); r.Get("wallet") == nil || r.Get("ledger") == nil {
		t.Errorf("GET /credits answers %d without wallet and ledger once the balance overflowed: %s", r.Status, r)
	}
}

// bugs-r10-sonnet 12: the outbound guard (outbound_guard.go:25) blocks the
// AWS, GCP/Azure-IMDS and Alibaba metadata addresses but not Azure's
// WireServer 168.63.129.16, the host agent endpoint that serves a VM's
// extension settings and protected secrets. A webhook, OTel endpoint or CI
// apiUrl may point there (and webhook test deliveries return the first 512
// bytes of the answer).
func TestBughuntR10WireServerNotBlocked(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.WebhookAllowLocal = false })
	r := env.Do(env.AdminKey, "POST", "/api/v1/webhooks", map[string]any{"url": "http://168.63.129.16/machine?comp=goalstate", "events": []string{"sandbox.ready"}})
	if r.Status == 201 {
		t.Fatalf("a webhook for Azure's WireServer 168.63.129.16 was accepted: %s", r)
	}
}
