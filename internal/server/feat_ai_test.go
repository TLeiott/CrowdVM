package server_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

func TestAICredentialProfiles(t *testing.T) {
	env := servertest.New(t)
	key := env.AdminKey
	r := env.Do(key, "GET", "/api/v1/credential-profiles", nil)
	if r.Status != 200 || r.Str("type") != "credential_profile.list" || r.Str("profiles.0.name") != "default" || r.Str("profiles.0.isDefault") != "true" ||
		r.Str("profiles.0.owner.kind") != "personal" || r.Str("profiles.0.canManage") != "true" || r.Str("credentials.0.getUrl") == "" {
		t.Fatalf("list: %s", r)
	}
	const k1, k2 = "sk-ant-first-0001", "sk-ant-second-0002"
	r = env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/anthropic-api-key", map[string]any{"value": k1})
	if r.Status != 200 || r.Str("profile.connected.0") != "anthropic-api-key" || strings.Contains(string(r.Body), k1) {
		t.Fatalf("set key: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/anthropic-api-key", map[string]any{"value": "has space"}); r.Code() != "invalid_key" {
		t.Fatalf("bad key: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/claude-subscription", map[string]any{"value": "x"}); r.Status != 400 || r.Code() != "not_an_api_key" {
		t.Fatalf("sign-in as key: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/nope", map[string]any{"value": "x"}); r.Status != 400 {
		t.Fatalf("unknown credential: %s", r)
	}
	// Accounts: add, switch, forget.
	r = env.Do(key, "POST", "/api/v1/credential-profiles/default/accounts/anthropic-api-key", map[string]any{"value": k2})
	if r.Status != 201 || len(r.Get("accounts").([]any)) != 2 || r.Str("accounts.0.active") != "true" || r.Str("accounts.1.label") != "…0002" {
		t.Fatalf("add account: %s", r)
	}
	second := r.Str("accounts.1.id")
	r = env.Do(key, "PUT", "/api/v1/credential-profiles/default/accounts/anthropic-api-key/"+second+"/active", nil)
	if r.Str("accounts.1.active") != "true" || r.Str("accounts.0.active") != "false" {
		t.Fatalf("switch: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/credential-profiles", nil); len(r.Get("profiles.0.accounts.anthropic-api-key").([]any)) != 2 {
		t.Fatalf("profile accounts: %s", r)
	}
	r = env.Do(key, "DELETE", "/api/v1/credential-profiles/default/accounts/anthropic-api-key/2", nil)
	if len(r.Get("accounts").([]any)) != 1 || r.Str("accounts.0.active") != "true" || r.Str("accounts.0.label") != "…0001" {
		t.Fatalf("forget active: %s", r)
	}
	// Sign-ins through the CrowdVM endpoint.
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/sign-ins/claude-subscription", map[string]any{"value": "sk-ant-oat-tok", "label": "me@x.dev"}); r.Status != 200 {
		t.Fatalf("sign-in: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/sign-ins/claude-bedrock", map[string]any{"value": "ABSK"}); r.Code() != "invalid_region" {
		t.Fatalf("bedrock region: %s", r)
	}
	// Custom APIs.
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/custom-apis/OPENAI_X", map[string]any{"value": "v", "baseUrl": "https://api.example.com"}); r.Code() != "invalid_custom_api_name" {
		t.Fatalf("reserved name: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/custom-apis/STRIPE_KEY", map[string]any{"value": "v", "baseUrl": "http://10.0.0.1:8080"}); r.Code() != "invalid_base_url" {
		t.Fatalf("private base url: %s", r)
	}
	r = env.Do(key, "PUT", "/api/v1/credential-profiles/default/custom-apis/STRIPE_KEY", map[string]any{"value": "sk_live", "baseUrl": "https://api.stripe.com/v1"})
	if r.Status != 200 || r.Str("profile.customApis.0") != "STRIPE_KEY" {
		t.Fatalf("custom api: %s", r)
	}
	// Profiles: copy (keys + custom APIs, not sign-ins), rename, default, delete rules.
	r = env.Do(key, "POST", "/api/v1/credential-profiles", map[string]any{"name": "ci", "copyFrom": "default"})
	if r.Status != 201 || r.Str("profile.isDefault") != "false" || r.Str("profile.customApis.0") != "STRIPE_KEY" || len(r.Get("profile.connected").([]any)) != 1 {
		t.Fatalf("copy: %s", r)
	}
	ci := r.Str("profile.id")
	if r := env.Do(key, "POST", "/api/v1/credential-profiles", map[string]any{"name": "ci"}); r.Code() != "profile_exists" {
		t.Fatalf("dup profile: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/ci", map[string]any{"name": "ci-bots", "isDefault": true}); r.Str("profile.name") != "ci-bots" || r.Str("profile.isDefault") != "true" {
		t.Fatalf("rename/default: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/credential-profiles/ci-bots", nil); r.Status != 409 || r.Code() != "is_default" {
		t.Fatalf("delete default: %s", r)
	}
	env.Do(key, "PUT", "/api/v1/credential-profiles/default", map[string]any{"isDefault": true})
	if r := env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"credentialProfileId": "ci-bots"}); r.Str("environment.credentialProfileId") != ci {
		t.Fatalf("env profile: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"credentialProfileId": "missing"}); r.Status != 404 || r.Code() != "credential_profile_not_found" {
		t.Fatalf("env unknown profile: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/credential-profiles/"+ci, nil); r.Status != 409 || r.Code() != "in_use" {
		t.Fatalf("delete in use: %s", r)
	}
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"credentialProfileId": nil})
	if r := env.Do(key, "DELETE", "/api/v1/credential-profiles/"+ci, nil); r.Status != 200 {
		t.Fatalf("delete: %s", r)
	}
	// Scope: profile calls need account.admin.
	if r := env.Do(env.Key(env.Admin, "read-only"), "GET", "/api/v1/credential-profiles", nil); r.Status != 403 {
		t.Fatalf("read-only profiles: %s", r)
	}
	// Proxy switch.
	r = env.Do(key, "GET", "/api/v1/ai-proxy", nil)
	if r.Status != 200 || r.Str("enabled") != "false" || r.Str("org") != "false" || !strings.HasSuffix(r.Str("proxyUrl"), "/ai-proxy") || r.Get("pricing") == nil {
		t.Fatalf("proxy get: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/ai-proxy", map[string]any{"enabled": true}); r.Str("enabled") != "true" || r.Str("enabledAt") == "" {
		t.Fatalf("proxy on: %s", r)
	}
	// Live catalog fields.
	r = env.Do(key, "GET", "/api/v1/provider-models", nil)
	for _, p := range r.Get("providers").([]any) {
		m := p.(map[string]any)
		if m["provider"] == "claude" && m["configured"] != true {
			t.Fatalf("claude not configured: %v", m)
		}
		if m["provider"] == "kimi" && m["configured"] != false {
			t.Fatalf("kimi configured: %v", m)
		}
	}
}

// aiUpstream is a fake vendor API: SSE (Anthropic shape), JSON (OpenAI
// shape), errors, and an echo of what it received.
type aiUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	seen    []*http.Request
	release chan struct{}
}

func newAIUpstream(t *testing.T) *aiUpstream {
	u := &aiUpstream{release: make(chan struct{}, 10)}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		u.mu.Lock()
		u.seen = append(u.seen, r.Clone(r.Context()))
		u.mu.Unlock()
		switch {
		case r.URL.Path == "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":7,\"cache_creation_input_tokens\":3,\"output_tokens\":1}}}\n\n")
			fl.Flush()
			select {
			case <-u.release:
			case <-time.After(5 * time.Second):
			}
			fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case r.URL.Path == "/v1/chat/completions":
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "gpt-6.1-sol", "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "prompt_tokens_details": map[string]any{"cached_tokens": 2}}})
		case strings.HasPrefix(r.URL.Path, "/custom/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": r.Header.Get("Authorization"), "q": r.URL.Query().Get("key"), "path": r.URL.Path})
		default:
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"boom"}`)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *aiUpstream) last() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.seen[len(u.seen)-1]
}

func proxyReq(t *testing.T, env *servertest.Env, method, path, body string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, env.URL+path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAIProxy(t *testing.T) {
	up := newAIUpstream(t)
	t.Setenv("CROWDVM_AI_UPSTREAM_ANTHROPIC", up.URL)
	t.Setenv("CROWDVM_AI_UPSTREAM_OPENAI", up.URL)
	t.Setenv("CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE", "1")
	t.Setenv("CROWDVM_AI_PROXY_CREDITS_PER_1000", "1")
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	const real, oai = "sk-ant-REAL-KEY-1", "sk-openai-REAL-2"
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/anthropic-api-key", map[string]any{"value": real})
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/openai-api-key", map[string]any{"value": oai})
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/custom-apis/MY_API", map[string]any{"value": "custom-secret", "baseUrl": up.URL + "/custom"})
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/sign-ins/claude-bedrock", map[string]any{"value": "ABSKbedrock", "region": "us-east-1"})
	env.Do(key, "PUT", "/api/v1/ai-proxy", map[string]any{"enabled": true})

	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	gc := guestSpec(t, node, id)
	if gc.AIProxy == nil || !strings.HasPrefix(gc.AIProxy.BaseURL, "http://10.0.2.2:") || !strings.HasSuffix(gc.AIProxy.BaseURL, "/ai-proxy") || !strings.HasPrefix(gc.AIProxy.Token, "cvmai_") {
		t.Fatalf("ai proxy config: %+v", gc.AIProxy)
	}
	tok := gc.AIProxy.Token
	for k, v := range gc.Env {
		if strings.Contains(v, "REAL") || strings.Contains(v, "custom-secret") || k == "AWS_BEARER_TOKEN_BEDROCK" {
			t.Fatalf("real secret in sandbox env: %s=%s", k, v)
		}
	}
	if gc.Env["MY_API"] != tok || gc.Env["MY_API_BASE_URL"] != gc.AIProxy.BaseURL+"/custom/MY_API" {
		t.Fatalf("custom api placeholders: %v", gc.Env)
	}

	// Anthropic SSE streams through, the real key goes only in x-api-key.
	resp := proxyReq(t, env, "POST", "/ai-proxy/anthropic/v1/messages", `{"model":"claude-opus-5-5","stream":true}`, "X-Api-Key", tok, "X-Echo", tok)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("sse status: %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	got := make(chan string, 1)
	go func() {
		var b strings.Builder
		for !strings.Contains(b.String(), "message_start") {
			l, err := br.ReadString('\n')
			b.WriteString(l)
			if err != nil {
				break
			}
		}
		got <- b.String()
	}()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("first SSE event was not streamed before the upstream finished")
	}
	up.release <- struct{}{}
	rest, _ := io.ReadAll(br)
	resp.Body.Close()
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("sse tail: %s", rest)
	}
	ur := up.last()
	if ur.Header.Get("X-Api-Key") != real || ur.Header.Get("Authorization") != "" || ur.Header.Get("X-Echo") != tok {
		t.Fatalf("upstream headers: %v", ur.Header)
	}
	// OpenAI JSON with usage, Bearer auth.
	resp = proxyReq(t, env, "POST", "/ai-proxy/openai/v1/chat/completions", `{"model":"gpt-6.1-sol"}`, "Authorization", "Bearer "+tok)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || up.last().Header.Get("Authorization") != "Bearer "+oai {
		t.Fatalf("openai: %d %v", resp.StatusCode, up.last().Header)
	}
	// Upstream error counts as an error.
	resp = proxyReq(t, env, "POST", "/ai-proxy/openai/v1/boom", `{"model":"gpt-6.1-sol"}`, "Authorization", "Bearer "+tok)
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Fatalf("boom: %d", resp.StatusCode)
	}
	// Custom API: secret in headers and query, never the path.
	resp = proxyReq(t, env, "GET", "/ai-proxy/custom/MY_API/v1/thing?key="+tok, "", "Authorization", "Bearer "+tok)
	var echo map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&echo)
	resp.Body.Close()
	if echo["auth"] != "Bearer custom-secret" || echo["q"] != "custom-secret" || echo["path"] != "/custom/v1/thing" {
		t.Fatalf("custom: %v", echo)
	}
	// bugs-r4-opus 3: never above the provider's base path with the secret.
	for _, p := range []string{"/ai-proxy/custom/MY_API/%2e%2e/admin", "/ai-proxy/custom/MY_API/v1/..%2f..%2fadmin", "/ai-proxy/openai/v1/%2E%2E/%2E%2E/backend-api/conversations"} {
		resp = proxyReq(t, env, "GET", p, "", "Authorization", "Bearer "+tok)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("traversal %s: %d", p, resp.StatusCode)
		}
	}
	// Bad tokens.
	if resp := proxyReq(t, env, "POST", "/ai-proxy/anthropic/v1/messages", "{}", "X-Api-Key", "cvmai_"+strings.Repeat("a", 40)); resp.StatusCode != 401 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}

	// Usage per sandbox and account.
	env.Eventually("usage recorded", func() bool {
		return env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/ai-usage?days=7", nil).Str("totals.requests") == "4"
	})
	r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/ai-usage?days=7", nil)
	if r.Str("type") != "sandbox.ai_usage" || r.Str("proxied") != "true" || r.Str("blocked") != "false" || r.Str("totals.errors") != "1" ||
		r.Str("totals.inputTokens") != "110" || r.Str("totals.outputTokens") != "47" || r.Str("totals.cacheReadTokens") != "9" || r.Str("totals.cacheWriteTokens") != "3" ||
		r.Str("dollars") != "0.004" {
		t.Fatalf("sandbox usage: %s", r)
	}
	models := map[string]bool{}
	for _, u := range r.Get("usage").([]any) {
		m := u.(map[string]any)
		models[m["credential"].(string)+"/"+m["model"].(string)] = true
	}
	if !models["anthropic-api-key/claude-opus-5-5"] || !models["openai-api-key/gpt-6.1-sol"] || !models["custom:MY_API/"] {
		t.Fatalf("usage rows: %v", models)
	}
	if r := env.Do(key, "GET", "/api/v1/ai-usage", nil); r.Str("sandboxes.0.sandboxId") != id || r.Str("totals.requests") != "4" || r.Str("accountId") != env.Admin.ID {
		t.Fatalf("account usage: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/ai-usage?days=0", nil); r.Status != 400 {
		t.Fatalf("days: %s", r)
	}

	// Block / unblock.
	if r := env.Do(key, "PUT", "/api/v1/sandboxes/"+id+"/ai-access", map[string]any{"blocked": true}); r.Str("blocked") != "true" || r.Str("type") != "sandbox.ai_access_updated" {
		t.Fatalf("block: %s", r)
	}
	if resp := proxyReq(t, env, "POST", "/ai-proxy/openai/v1/chat/completions", "{}", "Authorization", "Bearer "+tok); resp.StatusCode != 403 {
		t.Fatalf("blocked: %d", resp.StatusCode)
	}
	env.Do(key, "PUT", "/api/v1/sandboxes/"+id+"/ai-access", map[string]any{"blocked": false})
	if resp := proxyReq(t, env, "POST", "/ai-proxy/openai/v1/chat/completions", "{}", "Authorization", "Bearer "+tok); resp.StatusCode != 200 {
		t.Fatalf("unblocked: %d", resp.StatusCode)
	}
	_, otherKey := env.NewUser("mallory", false)
	if r := env.Do(otherKey, "PUT", "/api/v1/sandboxes/"+id+"/ai-access", map[string]any{"blocked": true}); r.Status != 404 {
		t.Fatalf("stranger block: %s", r)
	}

	// Placeholders die at stop; the next start rotates them.
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	env.Eventually("token revoked", func() bool {
		resp := proxyReq(t, env, "POST", "/ai-proxy/openai/v1/chat/completions", "{}", "Authorization", "Bearer "+tok)
		resp.Body.Close()
		return resp.StatusCode == 401
	})
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	if gc := guestSpec(t, node, id); gc.AIProxy == nil || gc.AIProxy.Token == tok {
		t.Fatalf("token not rotated: %+v", gc.AIProxy)
	}
}

func TestAIRealKeysAndSafeEnvironment(t *testing.T) {
	g := newEnvRecGuest()
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/anthropic-api-key", map[string]any{"value": "sk-one"})
	env.Do(key, "POST", "/api/v1/credential-profiles/default/accounts/anthropic-api-key", map[string]any{"value": "sk-two"})
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/mistral-api-key", map[string]any{"value": "mis-key"})
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/sign-ins/mistral-vibe-subscription", map[string]any{"value": "vibe-key"})
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/sign-ins/chatgpt-subscription", map[string]any{"value": `{"tokens":{"access_token":"at","account_id":"acct-1"}}`})
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/sign-ins/kimi-subscription", map[string]any{"value": "kimi-at", "refreshToken": "kimi-rt", "region": "global"})
	// Bedrock with IAM access keys instead of an API key.
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/sign-ins/claude-bedrock", map[string]any{"value": "iam-secret", "region": "eu-west-1", "accessKeyId": "AKIAEXAMPLE", "sessionToken": "sess-tok"}); r.Status != 200 {
		t.Fatalf("bedrock iam: %s", r)
	}
	if r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/openai-api-key", map[string]any{"value": "sk", "accessKeyId": "AKIA"}); r.Code() != "invalid_key" {
		t.Fatalf("accessKeyId on an api key: %s", r)
	}
	id := createSandbox(t, env, key, map[string]any{"env": map[string]string{"MISTRAL_API_KEY": "user-own"}})
	env.WaitState(key, id, "ready")
	gc := guestSpec(t, node, id)
	if gc.AIProxy != nil || gc.Env["ANTHROPIC_API_KEY"] != "sk-one" || gc.Env["MISTRAL_API_KEY"] != "user-own" || gc.Env["CHATGPT_ACCOUNT_ID"] != "acct-1" ||
		gc.Env["KIMI_CODE_ACCESS_TOKEN"] != "kimi-at" || gc.Env["KIMI_CODE_REFRESH_TOKEN"] != "kimi-rt" || gc.Env["KIMI_CODE_REGION"] != "global" {
		t.Fatalf("real keys: %v", gc.Env)
	}
	if gc.Env["AWS_ACCESS_KEY_ID"] != "AKIAEXAMPLE" || gc.Env["AWS_SECRET_ACCESS_KEY"] != "iam-secret" || gc.Env["AWS_SESSION_TOKEN"] != "sess-tok" ||
		gc.Env["AWS_REGION"] != "eu-west-1" || gc.Env["CLAUDE_CODE_USE_BEDROCK"] != "1" || gc.Env["AWS_BEARER_TOKEN_BEDROCK"] != "" {
		t.Fatalf("bedrock iam env: %v", gc.Env)
	}
	hasAuth := false
	for _, f := range gc.SecretFiles {
		hasAuth = hasAuth || (f.Path == "/home/user/.codex/auth.json" && strings.Contains(string(f.Content), `"access_token":"at"`))
	}
	if !hasAuth {
		t.Fatalf("codex auth.json: %+v", gc.SecretFiles)
	}
	// Switching the account reaches the running sandbox without a restart.
	r := env.Do(key, "PUT", "/api/v1/credential-profiles/default/accounts/anthropic-api-key/2/active", nil)
	if r.Str("accounts.1.active") != "true" {
		t.Fatalf("switch: %s", r)
	}
	env.Eventually("live key push", func() bool { // through the guest's apply-config
		return g.last(id+" POST /v1/internal/apply-config", `"ANTHROPIC_API_KEY":"sk-two"`) != ""
	})
	// Removing a key unsets it live.
	env.Do(key, "DELETE", "/api/v1/credential-profiles/default/keys/anthropic-api-key", nil)
	env.Eventually("live key removal", func() bool {
		ac := g.last(id+" POST /v1/internal/apply-config", `"envUnset":[`)
		_, unset, _ := strings.Cut(ac, `"envUnset":[`)
		unset, _, _ = strings.Cut(unset, "]")
		return strings.Contains(unset, `"ANTHROPIC_API_KEY"`)
	})

	// Safe for third parties withholds keys unless proxyAiCredentials (needs the proxy).
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/keys/anthropic-api-key", map[string]any{"value": "sk-three"})
	if r := env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"safeForThirdParties": true, "proxyAiCredentials": true}); r.Status != 400 || r.Code() != "ai_proxy_off" {
		t.Fatalf("ai_proxy_off: %s", r)
	}
	env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"safeForThirdParties": true, "envContents": "SECRET=1\n"})
	safe := createSandbox(t, env, key, nil)
	env.WaitState(key, safe, "ready")
	if gc := guestSpec(t, node, safe); gc.Env["ANTHROPIC_API_KEY"] != "" || gc.Env["SECRET"] != "" || gc.AIProxy != nil || gc.Env["CROWDVM_API_KEY"] != "" {
		t.Fatalf("safe env leaked: %v %+v", gc.Env, gc.AIProxy)
	}
	env.Do(key, "PUT", "/api/v1/ai-proxy", map[string]any{"enabled": true})
	if r := env.Do(key, "PUT", "/api/v1/environments/base", map[string]any{"proxyAiCredentials": true}); r.Status != 200 || r.Str("environment.effective.passAgentsCredentials") != "true" {
		t.Fatalf("proxyAiCredentials: %s", r)
	}
	proxied := createSandbox(t, env, key, nil)
	env.WaitState(key, proxied, "ready")
	if gc := guestSpec(t, node, proxied); gc.AIProxy == nil || gc.Env["SECRET"] != "" || gc.Env["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("safe+proxy: %v %+v", gc.Env, gc.AIProxy)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+proxied+"/ai-usage", nil); r.Str("proxied") != "true" || r.Str("totals.requests") != "0" {
		t.Fatalf("proxied usage: %s", r)
	}
}
