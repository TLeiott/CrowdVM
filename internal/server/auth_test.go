package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// browser is a cookie-carrying client.
type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, c: &http.Client{Jar: jar}}
}

func (b *browser) do(method, path string, body any, csrf bool) (int, map[string]any) {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		js, _ := json.Marshal(body)
		rd = strings.NewReader(string(js))
	}
	req, _ := http.NewRequest(method, b.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(server.CSRFHeader, "1")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestSignupLoginSessions(t *testing.T) {
	env := servertest.NewEmpty(t)
	b := newBrowser(t, env.URL)

	if st, m := b.do("GET", "/auth/config", nil, false); st != 200 || m["firstUser"] != true {
		t.Fatalf("config: %d %v", st, m)
	}
	// First user becomes admin and gets a session cookie.
	st, m := b.do("POST", "/auth/signup", map[string]any{"username": "alice", "password": "secret-123", "email": "alice@x.test"}, false)
	if st != 200 || servertest.Lookup(m, "user.isAdmin") != true {
		t.Fatalf("signup: %d %v", st, m)
	}
	if st, m := b.do("GET", "/api/v1/me", nil, false); st != 200 || servertest.Lookup(m, "user.login") != "alice" || m["type"] != "user.info" {
		t.Fatalf("me via cookie: %d %v", st, m)
	}
	// Cookie mutations need the CSRF header.
	if st, m := b.do("POST", "/api/v1/sandboxes", map[string]any{}, false); st != 403 || m["code"] != "csrf_required" {
		t.Fatalf("csrf: %d %v", st, m)
	}
	// Second signup is closed.
	b2 := newBrowser(t, env.URL)
	if st, m := b2.do("POST", "/auth/signup", map[string]any{"username": "bob", "password": "secret-123"}, false); st != 403 || m["code"] != "signup_closed" {
		t.Fatalf("closed signup: %d %v", st, m)
	}
	// Invite code from the admin API.
	st, m = b.do("POST", "/api/v1/admin/invite-codes", map[string]any{"maxUses": 1}, true)
	if st != 201 {
		t.Fatalf("invite: %d %v", st, m)
	}
	code := m["code"].(string)
	if st, m := b2.do("POST", "/auth/signup", map[string]any{"username": "bob", "password": "secret-123", "inviteCode": code}, false); st != 200 || servertest.Lookup(m, "user.isAdmin") != false {
		t.Fatalf("invite signup: %d %v", st, m)
	}
	if st, _ := newBrowser(t, env.URL).do("POST", "/auth/signup", map[string]any{"username": "carol", "password": "secret-123", "inviteCode": code}, false); st != 403 {
		t.Fatalf("used invite should fail: %d", st)
	}
	// Login: wrong password, browser, CLI token.
	b3 := newBrowser(t, env.URL)
	if st, m := b3.do("POST", "/auth/login", map[string]any{"username": "alice", "password": "nope"}, false); st != 401 || m["code"] != "invalid_credentials" {
		t.Fatalf("bad login: %d %v", st, m)
	}
	// A signup email counts only once confirmed (bugs-r11-sonnet 7): log in by name.
	st, m = b3.do("POST", "/auth/login", map[string]any{"username": "alice", "password": "secret-123", "client": "cli"}, false)
	tok, _ := m["token"].(string)
	if st != 200 || !strings.HasPrefix(tok, server.SessionPrefix) {
		t.Fatalf("cli login: %d %v", st, m)
	}
	if r := env.Do(tok, "GET", "/auth/me", nil); r.Status != 200 || r.Str("authMethod") != "cli_session" {
		t.Fatalf("auth/me with token: %s", r)
	}
	// A CLI session can mutate without CSRF.
	if r := env.Do(tok, "POST", "/api/v1/api-keys/scoped", map[string]any{"name": "k", "preset": "ci"}); r.Status != 201 {
		t.Fatalf("key via session: %s", r)
	}
	if r := env.Do(tok, "POST", "/auth/logout", nil); r.Status != 200 {
		t.Fatalf("logout: %s", r)
	}
	if r := env.Do(tok, "GET", "/auth/me", nil); r.Status != 401 {
		t.Fatalf("after logout: %s", r)
	}
	if r := env.Do("", "GET", "/api/v1/me", nil); r.Status != 401 || r.Code() != "unauthorized" || r.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("unauthenticated: %s", r)
	}
}

func TestOpenSignupAndEmailCodes(t *testing.T) {
	var mu sync.Mutex
	var lastBody string
	env := servertest.New(t, func(c *server.Config) { c.OpenSignup = true })
	env.S.Hooks.Mail = func(to, subject, body string) { mu.Lock(); lastBody = body; mu.Unlock() }

	if r := env.Do("", "POST", "/auth/signup", map[string]any{"username": "dave", "password": "secret-123"}); r.Status != 200 {
		t.Fatalf("open signup: %s", r)
	}
	if r := env.Do("", "POST", "/auth/email/start", map[string]any{"email": "eve@x.test"}); r.Status != 200 {
		t.Fatalf("email start: %s", r)
	}
	mu.Lock()
	code := regexp.MustCompile(`\d{6}`).FindString(lastBody)
	mu.Unlock()
	if code == "" {
		t.Fatal("no code mailed")
	}
	if r := env.Do("", "POST", "/auth/email/verify", map[string]any{"email": "eve@x.test", "code": "000000", "client": "cli"}); r.Status != 401 && code != "000000" {
		t.Fatalf("wrong code: %s", r)
	}
	r := env.Do("", "POST", "/auth/email/verify", map[string]any{"email": "eve@x.test", "code": code, "client": "cli"})
	if r.Status != 200 || r.Str("user.login") != "eve" || r.Str("user.connectionMethod") != "email" {
		t.Fatalf("verify: %s", r)
	}
	if r := env.Do(r.Str("token"), "GET", "/api/v1/me", nil); r.Status != 200 {
		t.Fatalf("token: %s", r)
	}
}

func TestDeviceFlow(t *testing.T) {
	env := servertest.New(t)
	r := env.Do("", "POST", "/auth/device/start", map[string]any{"clientName": "crowdvm-cli test"})
	if r.Status != 200 || r.Str("deviceCode") == "" || !strings.Contains(r.Str("verificationUrl"), "/device") || r.Str("interval") == "" {
		t.Fatalf("start: %s", r)
	}
	dc, uc := r.Str("deviceCode"), r.Str("userCode")
	if r := env.Do("", "POST", "/auth/device/poll", map[string]any{"deviceCode": dc}); r.Str("status") != "pending" {
		t.Fatalf("poll pending: %s", r)
	}
	// The page renders.
	resp, err := http.Get(env.URL + "/device?code=" + url.QueryEscape(uc))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("device page: %v %v", err, resp)
	}
	resp.Body.Close()
	// Approve with a browser session.
	b := newBrowser(t, env.URL)
	if st, _ := b.do("POST", "/auth/login", map[string]any{"username": "admin", "password": "password-admin"}, false); st != 200 {
		t.Fatal("login")
	}
	if st, m := b.do("GET", "/auth/device/lookup?code="+url.QueryEscape(uc), nil, false); st != 200 || m["clientName"] != "crowdvm-cli test" {
		t.Fatalf("lookup: %d %v", st, m)
	}
	if st, m := b.do("POST", "/auth/device/approve", map[string]any{"userCode": strings.ToLower(strings.ReplaceAll(uc, "-", ""))}, true); st != 200 || m["status"] != "approved" {
		t.Fatalf("approve: %d %v", st, m)
	}
	r = env.Do("", "POST", "/auth/device/poll", map[string]any{"deviceCode": dc})
	if r.Str("status") != "approved" || !strings.HasPrefix(r.Str("token"), server.SessionPrefix) || r.Str("user.login") != "admin" {
		t.Fatalf("poll approved: %s", r)
	}
	if r2 := env.Do(r.Str("token"), "GET", "/api/v1/me", nil); r2.Status != 200 {
		t.Fatalf("device token: %s", r2)
	}
	if r := env.Do("", "POST", "/auth/device/poll", map[string]any{"deviceCode": dc}); r.Str("status") != "expired" {
		t.Fatalf("token handed out twice: %s", r)
	}
	// API keys cannot approve.
	r = env.Do("", "POST", "/auth/device/start", nil)
	if r2 := env.Do(env.AdminKey, "POST", "/auth/device/approve", map[string]any{"userCode": r.Str("userCode")}); r2.Code() != "session_required" {
		t.Fatalf("key approval: %s", r2)
	}
}

func TestAgentRegistration(t *testing.T) {
	env := servertest.New(t)
	if r := env.Do("", "GET", "/.well-known/oauth-authorization-server", nil); r.Status != 200 || r.Get("agent_auth.registration_endpoint") == nil {
		t.Fatalf("metadata: %s", r)
	}
	// Anonymous: read-only until claimed.
	r := env.Do("", "POST", "/agent/identity", map[string]any{"method": "anonymous", "name": "bot"})
	anon := r.Str("access_token")
	if r.Status != 200 || anon == "" {
		t.Fatalf("anonymous: %s", r)
	}
	if r := env.Do(anon, "GET", "/api/v1/me", nil); r.Status != 200 || r.Str("user.claimed") != "false" {
		t.Fatalf("anon me: %s", r)
	}
	if r := env.Do(anon, "GET", "/api/v1/limits", nil); r.Status != 200 || r.Str("canStart") != "false" {
		t.Fatalf("anon limits: %s", r)
	}
	if r := env.Do(anon, "POST", "/api/v1/sandboxes", map[string]any{}); r.Status != 403 || r.Code() != "claim_required" {
		t.Fatalf("anon create: %s", r)
	}
	r = env.Do(anon, "POST", "/agent/identity/claim", nil)
	if r.Status != 200 || len(r.Str("user_code")) != 6 {
		t.Fatalf("claim: %s", r)
	}
	dc, uc := r.Str("device_code"), r.Str("user_code")
	tokReq := func() *servertest.Resp {
		return env.Do("", "POST", "/oauth2/token", map[string]any{"grant_type": "urn:ietf:params:oauth:grant-type:device_code", "device_code": dc})
	}
	if r := tokReq(); r.Status != 400 || r.Str("error") != "authorization_pending" {
		t.Fatalf("pending: %s", r)
	}
	sess := env.Session(env.Admin)
	if r := env.Do(sess, "POST", "/auth/device/approve", map[string]any{"userCode": uc}); r.Status != 200 {
		t.Fatalf("approve: %s", r)
	}
	r = tokReq()
	key := r.Str("access_token")
	if r.Status != 200 || !strings.HasPrefix(key, "cvm_") {
		t.Fatalf("token: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/me", nil); r.Str("user.login") != "admin" {
		t.Fatalf("claimed key acts as the human: %s", r)
	}
	if r := env.Do(anon, "GET", "/api/v1/me", nil); r.Status != 401 {
		t.Fatalf("anonymous token should be revoked: %s", r)
	}
	// Email method: only that account may approve.
	r = env.Do("", "POST", "/agent/identity", map[string]any{"method": "email", "email": "admin@example.test", "name": "mailbot"})
	if r.Status != 200 || r.Str("verification_uri") == "" {
		t.Fatalf("email method: %s", r)
	}
	u2, _ := env.NewUser("other", false)
	if r2 := env.Do(env.Session(u2), "POST", "/auth/device/approve", map[string]any{"userCode": r.Str("user_code")}); r2.Code() != "wrong_account" {
		t.Fatalf("wrong account approve: %s", r2)
	}
	// Revoke.
	form := url.Values{"token": {key}}
	resp, err := http.PostForm(env.URL+"/oauth2/revoke", form)
	if err != nil || resp.StatusCode != 200 {
		t.Fatal("revoke")
	}
	resp.Body.Close()
	if r := env.Do(key, "GET", "/api/v1/me", nil); r.Status != 401 {
		t.Fatalf("revoked: %s", r)
	}
}
