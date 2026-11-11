package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// promptGuest wraps the fake guest and records prompt bodies.
type promptGuest struct {
	*servertest.FakeGuest
	mu     sync.Mutex
	bodies []map[string]any
}

func (g *promptGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" && r.URL.Path == "/v1/prompt" {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		g.mu.Lock()
		g.bodies = append(g.bodies, m)
		g.mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(b)))
	}
	g.FakeGuest.ServeHTTP(w, r)
}

func (g *promptGuest) last() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 {
		return nil
	}
	return g.bodies[len(g.bodies)-1]
}

func TestOrgKeysDefaultDenyNoStore(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	if r := env.Do(env.AdminKey, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"}); r.Status != 201 {
		t.Fatalf("org: %s", r)
	}
	r := env.Do(env.Session(env.Admin), "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "org", "org": "acme"})
	orgKey := r.Str("secret")
	for _, p := range []string{"/api/v1/api-keys", "/api/v1/webhooks", "/api/v1/credential-profiles", "/api/v1/credits", "/api/v1/orgs/acme/members"} {
		if r := env.Do(orgKey, "GET", p, nil); r.Status != 403 || r.Code() != "api_key_org_forbidden" {
			t.Fatalf("org key %s: %s", p, r)
		}
	}
	if r := env.Do(orgKey, "GET", "/api/v1/sandboxes", nil); r.Status != 200 {
		t.Fatalf("org key sandboxes: %s", r)
	}
	// Default deny on ActionAny routes for narrow keys.
	ro := env.Key(env.Admin, "read-only")
	if r := env.Do(ro, "POST", "/api/v1/join-tokens", map[string]any{"name": "x"}); r.Status != 403 || r.Code() != "api_key_action_forbidden" {
		t.Fatalf("read-only key creates join token: %s", r)
	}
	if r := env.Do(ro, "GET", "/api/v1/nodes", nil); r.Status != 200 {
		t.Fatalf("read-only key lists nodes: %s", r)
	}
	// boat_/box_ prefixes in front of a CrowdVM key.
	if r := env.Do("box_"+strings.TrimPrefix(env.AdminKey, "cvm_"), "GET", "/api/v1/me", nil); r.Status != 200 {
		t.Fatalf("box_ key: %s", r)
	}
	// DELETE answers are not cached.
	id := createSandbox(t, env, env.AdminKey, map[string]any{"noEnv": true})
	env.WaitState(env.AdminKey, id, "ready")
	r = env.Do(env.AdminKey, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-CrowdVM-Confirm-Delete: "+id)
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("delete cache-control %q: %s", r.Header.Get("Cache-Control"), r)
	}
	// Audit log of the calls above.
	if r := env.Do(env.AdminKey, "GET", "/api/v1/audit-events?target="+id, nil); r.Status != 200 || !strings.Contains(r.String(), "sandbox.created") {
		t.Fatalf("audit: %s", r)
	}
	// Key creation switch (KEY-007).
	if r := env.Do(env.AdminKey, "PATCH", "/api/v1/admin/settings", map[string]any{"scopedKeyCreationEnabled": false}); r.Status != 200 {
		t.Fatalf("settings: %s", r)
	}
	if r := env.Do(env.Session(env.Admin), "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "x"}); r.Status != 503 || r.Code() != "api_key_creation_disabled" {
		t.Fatalf("creation disabled: %s", r)
	}
	if r := env.Do(env.AdminKey, "GET", "/api/v1/api-keys", nil); r.Str("catalog.scopedCreationEnabled") != "false" {
		t.Fatalf("catalog: %s", r)
	}
}

func TestAnonymousAgentLimited(t *testing.T) {
	env := servertest.New(t)
	r := env.Do("", "POST", "/agent/identity", map[string]any{"method": "anonymous", "name": "bot"})
	anon := r.Str("access_token")
	for _, p := range []string{"/api/v1/me", "/api/v1/limits"} {
		if r := env.Do(anon, "GET", p, nil); r.Status != 200 {
			t.Fatalf("anon %s: %s", p, r)
		}
	}
	for _, p := range []string{"/api/v1/orgs", "/api/v1/api-keys", "/api/v1/ci", "/api/v1/ai-usage", "/api/v1/credits", "/api/v1/nodes"} {
		if r := env.Do(anon, "GET", p, nil); r.Status != 403 || r.Code() != "claim_required" {
			t.Fatalf("anon %s: %s", p, r)
		}
	}
}

func TestPromptPreflightAndDefaults(t *testing.T) {
	env := servertest.New(t)
	g := &promptGuest{FakeGuest: servertest.NewFakeGuest()}
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "hi", "provider": "claude-code"})
	if r.Status != 409 || r.Code() != "provider_not_configured" || r.Str("error.details.provider") != "claude" || !strings.HasSuffix(r.Str("error.details.setupUrl"), "/#/ai") {
		t.Fatalf("preflight: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "hi", "provider": "opencode"}); r.Status != 202 {
		t.Fatalf("opencode needs no key: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "hi", "env": map[string]string{"ANTHROPIC_API_KEY": "sk-x"}}); r.Status != 202 {
		t.Fatalf("request env key: %s", r)
	}
	// crowdvm prompt --local-login: the request's credentials count (ChatGPT
	// sign-in for codex) and reach the guest untouched.
	creds := map[string]string{"CHATGPT_ACCESS_TOKEN": "at", "CHATGPT_ACCOUNT_ID": "acct"}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "hi", "provider": "codex", "credentials": creds}); r.Status != 202 {
		t.Fatalf("request credentials: %s", r)
	}
	if c, _ := g.last()["credentials"].(map[string]any); c["CHATGPT_ACCESS_TOKEN"] != "at" || c["CHATGPT_ACCOUNT_ID"] != "acct" {
		t.Fatalf("credentials not forwarded: %v", g.last())
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "hi", "provider": "claude", "credentials": map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oat"}}); r.Status != 202 {
		t.Fatalf("claude request credentials: %s", r)
	}
	// An account key reaches new sandboxes.
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/openai-api-key", map[string]any{"value": "sk-openai"}); r.Status != 200 {
		t.Fatalf("key: %s", r)
	}
	id2 := createSandbox(t, env, key, nil)
	env.WaitState(key, id2, "ready")
	// Account defaults fill provider/model/fast.
	r = env.Do(key, "PATCH", "/api/v1/account-settings", map[string]any{"agentDefaults": map[string]any{"provider": "codex", "model": "gpt-6.1-sol", "fast": true}})
	if r.Status != 200 || r.Str("agentDefaults.provider") != "codex" {
		t.Fatalf("settings: %s", r)
	}
	if r := env.Do(key, "PATCH", "/api/v1/account-settings", map[string]any{"agentDefaults": map[string]any{"model": "nope"}}); r.Status != 400 {
		t.Fatalf("unknown model accepted: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id2+"/prompt", map[string]any{"prompt": "hi"}); r.Status != 202 {
		t.Fatalf("prompt with defaults: %s", r)
	}
	if b := g.last(); b["provider"] != "codex" || b["model"] != "gpt-6.1-sol" || b["fast"] != true || b["prompt"] != "hi" {
		t.Fatalf("defaults not applied: %v", b)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id2+"/prompt", map[string]any{"prompt": "x", "fast": false}); r.Status != 202 || g.last()["fast"] != false {
		t.Fatalf("fast:false must win: %s %v", r, g.last())
	}
	if r := env.Do(env.AdminKey, "GET", "/api/v1/me", nil); r.Str("user.agentDefaults.provider") != "codex" {
		t.Fatalf("me: %s", r)
	}
	// A stopped sandbox is not promptable.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil); r.Status/100 != 2 {
		t.Fatalf("stop: %s", r)
	}
	env.WaitState(key, id, "archived")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/prompt", map[string]any{"prompt": "hi", "provider": "opencode"}); r.Status != 409 || r.Code() != "sandbox_not_promptable" {
		t.Fatalf("stopped: %s", r)
	}
	_ = node
}

func TestAISignInRenewalAndDeepSeek(t *testing.T) {
	var calls int
	var mu sync.Mutex
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		calls++
		mu.Unlock()
		if req["grant_type"] != "refresh_token" || req["refresh_token"] != "rt-1" {
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-2", "refresh_token": "rt-2", "expires_in": 3600})
	}))
	defer tokenSrv.Close()
	t.Setenv("CROWDVM_AI_REFRESH_URL_CLAUDE_SUBSCRIPTION", tokenSrv.URL)
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/sign-ins/claude-subscription", map[string]any{"value": "at-1", "refreshToken": "rt-1", "expiresIn": 60})
	if r.Status != 200 {
		t.Fatalf("sign-in: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/deepseek-api-key", map[string]any{"value": "sk-ds"}); r.Status != 200 {
		t.Fatalf("deepseek: %s", r)
	}
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	spec, _ := node.Spec(id)
	e := spec.Guest.Env
	if e["CLAUDE_CODE_OAUTH_TOKEN"] != "at-2" || calls != 1 {
		t.Fatalf("not renewed before start (calls %d): %v", calls, e["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	for k, v := range e {
		if strings.Contains(v, "rt-") {
			t.Fatalf("refresh token in sandbox env %s", k)
		}
	}
	// DeepSeek covers Codex (no OpenAI credential), not Claude (has a sign-in).
	if e["OPENAI_BASE_URL"] != "https://api.deepseek.com/v1" || e["DEEPSEEK_API_KEY"] != "sk-ds" || e["ANTHROPIC_BASE_URL"] != "" {
		t.Fatalf("deepseek mapping: %v", e)
	}
	// Per-sandbox DeepSeek key with --no-env sets up both harnesses.
	id2 := createSandbox(t, env, key, map[string]any{"noEnv": true, "env": map[string]string{"DEEPSEEK_API_KEY": "sk-user"}})
	env.WaitState(key, id2, "ready")
	spec2, _ := node.Spec(id2)
	if e := spec2.Guest.Env; e["ANTHROPIC_BASE_URL"] != "https://api.deepseek.com/anthropic" || e["ANTHROPIC_AUTH_TOKEN"] != "sk-user" || e["OPENAI_BASE_URL"] != "https://api.deepseek.com/v1" {
		t.Fatalf("per-sandbox deepseek: %v", e)
	}
}

func TestCreditPolicies(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.EnforceCredits = true })
	env.StartNode(servertest.NodeOptions{})
	u, key := env.NewUser("carl", false)
	admin := env.AdminKey
	r := env.Do(admin, "PUT", "/api/v1/admin/policies/carl", map[string]any{"allowedSizes": []string{"small", "default"}, "maxTtlSeconds": 600, "maxActive": 5, "startsPerHour": 1, "monthlyCredits": 50, "lowCreditAlert": 1000})
	if r.Status != 200 || r.Str("policy.maxTtlSeconds") != "600" {
		t.Fatalf("policy: %s", r)
	}
	if r := env.Do(admin, "PUT", "/api/v1/admin/policies/carl", map[string]any{"bogus": 1}); r.Status != 400 {
		t.Fatalf("unknown field: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"type": "large"}); r.Status != 403 || r.Code() != "machine_class_plan_required" {
		t.Fatalf("size: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"ttlSeconds": 3600}); r.Status != 400 || r.Code() != "auto_stop_required" {
		t.Fatalf("ttl: %s", r)
	}
	r = env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"noEnv": true})
	if r.Status != 202 || r.Str("sandbox.ttlSeconds") != "600" {
		t.Fatalf("ttl capped: %s", r)
	}
	// Start limit, then extra starts at credit cost.
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"noEnv": true}); r.Status != 429 || r.Code() != "rate_limited" {
		t.Fatalf("start limit: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/limits", nil); r.Str("planNudge.reason") == "" || r.Str("startWindowFull.window") == "" {
		t.Fatalf("limits nudge: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/billing/extra-starts", map[string]any{"enabled": true}); r.Status != 200 || r.Str("extraStarts.enabled") != "true" {
		t.Fatalf("extra starts: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"noEnv": true}); r.Status != 202 {
		t.Fatalf("extra start: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/billing", nil); r.Get("extraStarts.usedCreditsThisMonth").(float64) <= 0 {
		t.Fatalf("extra start not charged: %s", r)
	}
	// Monthly quota (granted once per month) and low-credit notice.
	before := env.S.Balance(u.ID)
	env.S.BillNow()
	env.S.BillNow()
	if got := env.S.Balance(u.ID) - before; got < 49.9 || got > 50.01 {
		t.Fatalf("quota grant %v", got)
	}
	if r := env.Do(key, "GET", "/api/v1/notices", nil); !strings.Contains(r.String(), "low_credit") {
		t.Fatalf("low credit notice: %s", r)
	}
	// Monthly credit cap: refuses starts, stops running sandboxes.
	if r := env.Do(admin, "PUT", "/api/v1/admin/policies/carl", map[string]any{"creditCapMonthly": 0.01, "startsPerHour": nil}); r.Status != 200 {
		t.Fatalf("cap: %s", r)
	}
	if _, err := env.S.DB.Exec(`INSERT INTO credit_ledger(wallet_id,delta,kind,reason,created_at) VALUES(?,?,?,?,?)`, u.ID, -1.0, "usage", "test", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"noEnv": true}); r.Status != 402 || r.Code() != "credit_cap_reached" {
		t.Fatalf("cap start: %s", r)
	}
	env.S.BillNow()
	env.Eventually("capped sandboxes stop", func() bool {
		r := env.Do(key, "GET", "/api/v1/limits", nil)
		return r.Str("activeSandboxes") == "0"
	})
	// Checkout stub.
	if r := env.Do(key, "POST", "/api/boat/billing/checkout", nil); r.Status != 200 || r.Str("checkoutRequired") != "false" {
		t.Fatalf("checkout: %s", r)
	}
}

func TestMemberCapStopsAndMove(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	alice := env.AdminKey
	_, bobKey := env.NewUser("bob", false)
	orgID := env.Do(alice, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"}).Str("org.id")
	code := env.Do(alice, "POST", "/api/v1/orgs/acme/invites", map[string]any{"email": "bob@example.test"}).Str("code")
	env.Do(bobKey, "POST", "/api/v1/org-invites/accept", map[string]any{"code": code})
	id := createSandbox(t, env, bobKey, map[string]any{"org": "acme", "noEnv": true})
	env.WaitState(bobKey, id, "ready")
	bobID := env.Do(bobKey, "GET", "/api/v1/me", nil).Str("user.id")
	if r := env.Do(alice, "PATCH", "/api/v1/orgs/acme/members/"+bobID, map[string]any{"concurrentCap": 0}); r.Status != 200 {
		t.Fatalf("cap: %s", r)
	}
	env.S.BillNow()
	env.WaitState(bobKey, id, "archived")
	if r := env.Do(bobKey, "GET", "/api/v1/notices", nil); !strings.Contains(r.String(), "member_cap_stopped") {
		t.Fatalf("member notice: %s", r)
	}
	// Move personal sandboxes into the org (owner only).
	p := createSandbox(t, env, alice, map[string]any{"noEnv": true})
	if r := env.Do(bobKey, "POST", "/api/v1/orgs/acme/move-personal", nil); r.Status != 403 {
		t.Fatalf("member moved: %s", r)
	}
	r := env.Do(alice, "POST", "/api/v1/orgs/acme/move-personal", map[string]any{"transferCredits": 5})
	if r.Status != 200 || r.Str("moved.sandboxes") != "1" || r.Str("transferredCredits") != "5" {
		t.Fatalf("move: %s", r)
	}
	if r := env.Do(alice, "GET", "/api/v1/sandboxes/"+p, nil); r.Str("sandbox.team.id") != orgID || r.Str("sandbox.visibility") != "shared" { // --no-env: no logins, org default shared
		t.Fatalf("moved sandbox: %s", r)
	}
}

func TestServiceAccountsMergeLinksFeedback(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	admin := env.AdminKey
	env.Do(admin, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	r := env.Do(admin, "POST", "/api/v1/orgs/acme/service-accounts", map[string]any{"name": "ci-bot", "ttl": "1h"})
	if r.Status != 201 || r.Str("user.serviceAccount") != "true" || r.Str("user.connectionMethod") != "service" {
		t.Fatalf("service account: %s", r)
	}
	svcKey := r.Str("secret")
	svcSandbox := env.Do(svcKey, "POST", "/api/v1/sandboxes", map[string]any{"noEnv": true})
	if r := svcSandbox; r.Status != 202 || r.Str("sandbox.team.name") != "Acme" {
		t.Fatalf("service key create: %s", r)
	}
	if r := env.Do(admin, "GET", "/api/v1/orgs/acme/service-accounts", nil); len(r.Get("serviceAccounts").([]any)) != 1 {
		t.Fatalf("list: %s", r)
	}
	env.WaitState(svcKey, svcSandbox.Str("sandbox.id"), "ready")
	if r := env.Do(svcKey, "DELETE", "/api/v1/sandboxes/"+svcSandbox.Str("sandbox.id"), nil, "X-CrowdVM-Confirm-Delete: "+svcSandbox.Str("sandbox.id")); r.Status != 202 {
		t.Fatalf("delete service sandbox: %s", r)
	}
	svcID := env.Do(svcKey, "GET", "/api/v1/me", nil).Str("user.id")
	if r := env.Do(admin, "DELETE", "/api/v1/orgs/acme/service-accounts/"+svcID, nil); r.Status != 200 {
		t.Fatalf("delete: %s", r)
	}
	if r := env.Do(svcKey, "GET", "/api/v1/me", nil); r.Status != 401 && r.Status != 403 {
		t.Fatalf("deleted service key works: %s", r)
	}
	// Merge.
	_, oldKey := env.NewUser("old", false)
	sb := createSandbox(t, env, oldKey, map[string]any{"noEnv": true})
	_, newKey := env.NewUser("new", false)
	if r := env.Do(admin, "POST", "/api/v1/admin/users/old/merge", map[string]any{"into": "new"}); r.Status != 200 || r.Str("moved.sandboxes") != "1" {
		t.Fatalf("merge: %s", r)
	}
	if r := env.Do(newKey, "GET", "/api/v1/sandboxes/"+sb, nil); r.Str("sandbox.createdBy") != "new" {
		t.Fatalf("merged owner: %s", r)
	}
	// One-time login link.
	r = env.Do(env.Session(env.Admin), "POST", "/api/v1/auth/login-links", nil)
	if r.Status != 201 || r.Str("expiresInSeconds") != "900" {
		t.Fatalf("link: %s", r)
	}
	if r := env.Do(admin, "POST", "/api/v1/auth/login-links", nil); r.Status != 403 {
		t.Fatalf("api key made a login link: %s", r)
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Get(r.Str("url"))
	if err != nil || resp.StatusCode != 302 || !strings.Contains(resp.Header.Get("Set-Cookie"), server.SessionCookie) {
		t.Fatalf("open link: %v %v", err, resp)
	}
	resp.Body.Close()
	if resp, _ := cl.Get(r.Str("url")); resp.StatusCode != 410 {
		t.Fatalf("reused link: %d", resp.StatusCode)
	}
	// Feedback: 5 per minute.
	for i := 0; i < 5; i++ {
		if r := env.Do(admin, "POST", "/api/v1/feedback", map[string]any{"text": "nice"}); r.Status != 201 {
			t.Fatalf("feedback: %s", r)
		}
	}
	if r := env.Do(admin, "POST", "/api/v1/feedback", map[string]any{"text": "again"}); r.Status != 429 {
		t.Fatalf("feedback limit: %s", r)
	}
	if r := env.Do(admin, "GET", "/api/v1/admin/feedback", nil); len(r.Get("feedback").([]any)) != 5 {
		t.Fatalf("admin feedback: %s", r)
	}
	// Onboarding answer makes the default environment safe for third parties.
	if r := env.Do(admin, "PATCH", "/api/v1/account-settings", map[string]any{"useCase": "platform"}); r.Status != 200 || r.Str("useCase") != "platform" {
		t.Fatalf("use case: %s", r)
	}
	if r := env.Do(admin, "GET", "/api/v1/environments", nil); !strings.Contains(r.String(), `"safeForThirdParties":true`) {
		t.Fatalf("env not safe: %s", r)
	}
}

func TestCatalogOpenAPIWellKnown(t *testing.T) {
	env := servertest.New(t)
	admin := env.AdminKey
	r := env.Do(admin, "PUT", "/api/v1/admin/provider-models", map[string]any{"providers": []any{
		map[string]any{"provider": "claude", "name": "Claude Code", "defaultModel": "m1", "models": []any{map[string]any{"id": "m1", "name": "M1", "fastMode": true}}}}})
	if r.Status != 200 || r.Str("source") != "admin" {
		t.Fatalf("catalog put: %s", r)
	}
	if r := env.Do(admin, "GET", "/api/v1/provider-models", nil); r.Str("providers.0.models.0.id") != "m1" || len(r.Get("providers").([]any)) != 1 {
		t.Fatalf("catalog: %s", r)
	}
	if r := env.Do(admin, "PUT", "/api/v1/admin/provider-models", map[string]any{"providers": []any{map[string]any{"provider": "x"}}}); r.Status != 400 {
		t.Fatalf("invalid catalog: %s", r)
	}
	if r := env.Do(admin, "DELETE", "/api/v1/admin/provider-models", nil); r.Str("source") != "builtin" {
		t.Fatalf("reset: %s", r)
	}
	r = env.Do("", "GET", "/openapi/crowdvm-v1.json", nil)
	if r.Status != 200 || r.Str("openapi") != "3.1.0" {
		t.Fatalf("openapi: %d %.200s", r.Status, r.Body)
	}
	for path, method := range map[string]string{"/sandboxes": "post", "/sandboxes/{sandboxId}/commands": "post", "/provider-models": "get",
		"/admin/provider-models": "put", "/billing": "get", "/sandboxes/{sandboxId}/prompt": "post"} {
		if r.Get("paths."+path+"."+method) == nil {
			t.Errorf("openapi lacks %s %s", method, path)
		}
	}
	if r.Str("paths./sandboxes.post.operationId") == "" || r.Str("paths./billing.get.x-crowdvm-extension") != "true" {
		t.Fatalf("openapi ops: %v", r.Get("paths./sandboxes"))
	}
	y := env.Do("", "GET", "/api/v1/openapi.yaml", nil)
	if y.Status != 200 || !strings.Contains(string(y.Body), `"openapi": "3.1.0"`) {
		t.Fatalf("yaml: %d %.200s", y.Status, y.Body)
	}
	for _, p := range []string{"/api/boat/.well-known/oauth-protected-resource", "/api/crowdvm/.well-known/oauth-authorization-server"} {
		if r := env.Do("", "GET", p, nil); r.Status != 200 {
			t.Fatalf("%s: %s", p, r)
		}
	}
}

func TestZDRGuestConfigAndUnshareKeys(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	alice := env.AdminKey
	if _, err := env.S.DB.Exec(`INSERT INTO account_policy(user_id,zdr) VALUES(?,1)`, env.Admin.ID); err != nil {
		t.Fatal(err)
	}
	id := createSandbox(t, env, alice, map[string]any{"noEnv": true})
	env.WaitState(alice, id, "ready")
	if sp, _ := node.Spec(id); !sp.Guest.ZeroDataRetention || !sp.Guest.NoMonitoring {
		t.Fatalf("zdr not in guest config: %+v", sp.Guest)
	}
	_, _ = env.S.DB.Exec(`DELETE FROM account_policy`)
	// Unshare removes teammates' SSH keys.
	_, bobKey := env.NewUser("bob", false)
	env.Do(alice, "POST", "/api/v1/orgs", map[string]any{"name": "Acme"})
	code := env.Do(alice, "POST", "/api/v1/orgs/acme/invites", map[string]any{"email": "bob@example.test"}).Str("code")
	env.Do(bobKey, "POST", "/api/v1/org-invites/accept", map[string]any{"code": code})
	sb := createSandbox(t, env, alice, map[string]any{"org": "acme", "noEnv": true, "visibility": "shared"})
	env.WaitState(alice, sb, "ready")
	aliceK := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAliceAliceAliceAliceAliceAliceAliceAlice alice"
	bobK := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBobBobBobBobBobBobBobBobBobBobBobBobBob1 bob"
	env.Do(alice, "POST", "/api/v1/sandboxes/"+sb+"/sshkey", map[string]any{"key": aliceK})
	env.Do(bobKey, "POST", "/api/v1/sandboxes/"+sb+"/sshkey", map[string]any{"key": bobK})
	if r := env.Do(alice, "PUT", "/api/v1/sandboxes/"+sb+"/files/raw?path=/home/user/.ssh/authorized_keys", aliceK+"\n"+bobK+"\n"); r.Status != 200 {
		t.Fatalf("write keys: %s", r)
	}
	if r := env.Do(alice, "POST", "/api/v1/sandboxes/"+sb+"/unshare", nil); r.Status != 200 {
		t.Fatalf("unshare: %s", r)
	}
	env.Eventually("bob's key removed", func() bool {
		r := env.Do(alice, "GET", "/api/v1/sandboxes/"+sb+"/files/raw?path=/home/user/.ssh/authorized_keys", nil)
		return r.Status == 200 && strings.Contains(string(r.Body), "alice") && !strings.Contains(string(r.Body), "bob")
	})
}

// RUN-008: an admin-set "restricted" policy caps auto-stop at 2 h with
// Boat's trial error code; GET's read-only "effective" is accepted by PUT.
func TestRestrictedPolicy(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	_, key := env.NewUser("trial", false)
	admin := env.AdminKey
	got := env.Do(admin, "GET", "/api/v1/admin/policies/trial", nil)
	pol := got.Get("policy").(map[string]any)
	pol["restricted"] = true
	if r := env.Do(admin, "PUT", "/api/v1/admin/policies/trial", pol); r.Status != 200 || r.Get("policy.restricted") != true || r.Get("policy.effective") == nil {
		t.Fatalf("put with effective: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"ttlSeconds": nil}); r.Status != 400 || r.Code() != "trial_auto_stop_required" || r.Str("error.details.maxTtlSeconds") != "7200" {
		t.Fatalf("no auto-stop: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"ttlSeconds": 7201}); r.Code() != "trial_auto_stop_required" {
		t.Fatalf("ttl > 2h: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"ttlSeconds": 3600, "noEnv": true}); r.Status != 202 {
		t.Fatalf("ttl 1h: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/limits", nil); r.Get("restrictions.restricted") != true || r.Str("restrictions.maxTtlSeconds") != "7200" {
		t.Fatalf("limits: %s", r)
	}
}

// SNAP-020: included saved snapshots, daily credits for extras, and
// credit_required for an extra from an empty wallet (enforcement on).
func TestSavedSnapshotAllowance(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.EnforceCredits = true })
	env.StartNode(servertest.NodeOptions{})
	u, key := env.NewUser("saver", false)
	env.Do(env.AdminKey, "PUT", "/api/v1/admin/policies/saver", map[string]any{"savedSnapshotAllowance": 1})
	id := createSandbox(t, env, key, map[string]any{"noEnv": true})
	env.WaitState(key, id, "ready")
	save := func(name string) *servertest.Resp {
		return env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": name})
	}
	for _, n := range []string{"one", "two"} {
		if r := save(n); r.Status != 202 {
			t.Fatalf("save %s: %s", n, r)
		}
		env.WaitFor(key, "/api/v1/saved-snapshots/"+n, "snapshot.status", servertest.WaitTimeout, "ready", "saved")
	}
	r := env.Do(key, "GET", "/api/v1/saved-snapshots", nil)
	if r.Str("allowance.included") != "1" || r.Str("allowance.used") != "2" || r.Str("allowance.extra") != "1" || r.Get("allowance.extraDailyDollars").(float64) != 0.056 {
		t.Fatalf("allowance: %s", r)
	}
	before := env.S.Balance(u.ID)
	env.S.BillNow()
	env.S.BillNow()
	charged := before - env.S.Balance(u.ID)
	if want := 0.056 * 100000 / 3600; charged < want || charged > want+0.5 { // + a little sandbox time
		t.Fatalf("charged %v, want about %v once", charged, want)
	}
	if err := env.S.GrantCredits(u.ID, -env.S.Balance(u.ID), "test", ""); err != nil {
		t.Fatal(err)
	}
	if r := save("three"); r.Status != 402 || r.Code() != "credit_required" {
		t.Fatalf("empty wallet: %s", r)
	}
	if r := save("two"); r.Status != 202 { // updating an existing name is not a new one
		t.Fatalf("update: %s", r)
	}
}
