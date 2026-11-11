package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
)

func fakeJWT(exp time.Time) string {
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return "eyJhbGciOiJub25lIn0." + p + ".sig"
}

func putFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func errCode(err error) string {
	if e, ok := client.AsError(err); ok {
		return e.Code
	}
	return ""
}

func TestReadLocalLogin(t *testing.T) {
	now := time.Now()
	home := t.TempDir()
	vars := map[string]string{"HOME": home}
	getenv := func(k string) string { return vars[k] }
	read := func(p, mode string) (*localLogin, error) { return readLocalLogin(p, mode, getenv, now) }

	// Nothing on this machine.
	if _, err := read("claude", "auto"); errCode(err) != "local_login_not_found" {
		t.Fatalf("claude without login: %v", err)
	}
	if _, err := read("codex", "auto"); errCode(err) != "local_login_not_found" {
		t.Fatalf("codex without login: %v", err)
	}
	if _, err := read("kimi", "auto"); err == nil {
		t.Fatal("kimi accepted")
	}

	// Claude: credentials file, then the env token wins, api-key mode picks the key.
	putFile(t, filepath.Join(home, ".claude", ".credentials.json"),
		fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-file","refreshToken":"sk-ant-ort-NEVER","expiresAt":%d}}`, now.Add(time.Hour).UnixMilli()))
	vars["ANTHROPIC_API_KEY"] = "sk-ant-api"
	ll, err := read("claude", "auto")
	if err != nil || ll.Creds["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-file" || len(ll.Creds) != 1 || ll.Kind != "subscription" {
		t.Fatalf("claude file: %+v %v", ll, err)
	}
	if in := ll.info(now)["expiresInSeconds"].(int64); in < 3500 || in > 3600 {
		t.Fatalf("expiresInSeconds %d", in)
	}
	if ll, _ = read("claude", "api-key"); ll.Creds["ANTHROPIC_API_KEY"] != "sk-ant-api" || ll.Creds["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
		t.Fatalf("claude api-key: %+v", ll)
	}
	vars["CLAUDE_CODE_OAUTH_TOKEN"] = "sk-ant-oat-env"
	if ll, _ = read("claude", "subscription"); ll.Creds["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-env" || !ll.ExpiresAt.IsZero() {
		t.Fatalf("claude env token: %+v", ll)
	}
	delete(vars, "CLAUDE_CODE_OAUTH_TOKEN")
	// CLAUDE_CONFIG_DIR moves the file.
	vars["CLAUDE_CONFIG_DIR"] = filepath.Join(home, "elsewhere")
	if ll, _ = read("claude", "auto"); ll.Kind != "api-key" {
		t.Fatalf("CLAUDE_CONFIG_DIR ignored: %+v", ll)
	}
	delete(vars, "CLAUDE_CONFIG_DIR")
	// About to expire: refused with the refresh hint, never falls back silently.
	putFile(t, filepath.Join(home, ".claude", ".credentials.json"),
		fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-old","expiresAt":%d}}`, now.Add(time.Minute).UnixMilli()))
	if _, err := read("claude", "auto"); errCode(err) != "local_login_expired" || !strings.Contains(err.Error(), "run `claude` once") {
		t.Fatalf("expiring claude token: %v", err)
	}

	// Codex: tokens from auth.json (exp from the JWT), never the refresh or id token.
	at := fakeJWT(now.Add(48 * time.Hour))
	putFile(t, filepath.Join(home, ".codex", "auth.json"),
		`{"OPENAI_API_KEY":null,"tokens":{"access_token":"`+at+`","account_id":"acct-1","refresh_token":"rt-NEVER","id_token":"id-NEVER"}}`)
	ll, err = read("codex", "auto")
	if err != nil || ll.Creds["CHATGPT_ACCESS_TOKEN"] != at || ll.Creds["CHATGPT_ACCOUNT_ID"] != "acct-1" || len(ll.Creds) != 2 {
		t.Fatalf("codex: %+v %v", ll, err)
	}
	if d := ll.ExpiresAt.Sub(now); d < 47*time.Hour || d > 49*time.Hour {
		t.Fatalf("codex expiry %v", d)
	}
	for _, v := range ll.Creds {
		if strings.Contains(v, "NEVER") {
			t.Fatal("refresh or id token sent")
		}
	}
	if _, err := read("codex", "api-key"); errCode(err) != "local_login_not_found" {
		t.Fatalf("codex api-key without key: %v", err)
	}
	vars["OPENAI_API_KEY"] = "sk-env"
	if ll, _ = read("codex", "api-key"); ll.Creds["OPENAI_API_KEY"] != "sk-env" {
		t.Fatalf("codex env key: %+v", ll)
	}
	// CODEX_HOME moves the file; its OPENAI_API_KEY beats the env.
	vars["CODEX_HOME"] = filepath.Join(home, "codexhome")
	putFile(t, filepath.Join(home, "codexhome", "auth.json"), `{"OPENAI_API_KEY":"sk-file","tokens":{"access_token":"`+fakeJWT(now.Add(-time.Hour))+`"}}`)
	if ll, _ = read("codex", "api-key"); ll.Creds["OPENAI_API_KEY"] != "sk-file" {
		t.Fatalf("codex file key: %+v", ll)
	}
	if _, err := read("codex", "auto"); errCode(err) != "local_login_expired" || !strings.Contains(err.Error(), "run `codex` once") {
		t.Fatalf("expired codex token: %v", err)
	}

	// opencode / pi: vendor API keys only.
	ll, err = read("pi", "auto")
	if err != nil || ll.Creds["OPENAI_API_KEY"] != "sk-env" || ll.Creds["ANTHROPIC_API_KEY"] != "sk-ant-api" || len(ll.Creds) != 2 {
		t.Fatalf("pi: %+v %v", ll, err)
	}
	if _, err := read("opencode", "subscription"); errCode(err) != "local_login_not_found" {
		t.Fatalf("opencode subscription: %v", err)
	}
}

func TestParseLocalLoginMode(t *testing.T) {
	for in, want := range map[string]string{"": "", "0": "", "off": "", "1": "auto", "auto": "auto", "subscription": "subscription", "api-key": "api-key"} {
		if got, err := parseLocalLoginMode(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := parseLocalLoginMode("bogus"); err == nil {
		t.Error("bogus accepted")
	}
}

func TestPromptLocalLogin(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("ready")
	home := t.TempDir()
	e.vars["HOME"] = home
	putFile(t, filepath.Join(home, ".codex", "auth.json"),
		`{"tokens":{"access_token":"`+fakeJWT(time.Now().Add(time.Hour))+`","account_id":"acct-9","refresh_token":"rt-NEVER"}}`)

	if r := e.run("prompt", id, "--local-login", "fix it"); r.code != 2 || !strings.Contains(r.err, "needs --provider") {
		t.Fatalf("no provider: %d %s", r.code, r.err)
	}
	r := e.mustRun("prompt", id, "--provider", "codex", "--local-login", "fix it")
	b := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/prompt"))
	creds, _ := b["credentials"].(map[string]any)
	if creds["CHATGPT_ACCOUNT_ID"] != "acct-9" || creds["CHATGPT_ACCESS_TOKEN"] == nil || len(creds) != 2 {
		t.Fatalf("credentials %v", b["credentials"])
	}
	q := r.lines(t)[0]
	ll, _ := q["localLogin"].(map[string]any)
	if q["event"] != "queued" || ll["kind"] != "subscription" || ll["expiresInSeconds"] == nil || strings.Contains(r.out, "eyJ") {
		t.Fatalf("queued %v", q)
	}
	// CROWDVM_LOCAL_LOGIN turns it on; --local-login=off turns it off again.
	e.vars["CROWDVM_LOCAL_LOGIN"] = "1"
	e.mustRun("prompt", id, "--provider", "codex", "again")
	if b := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/prompt")); b["credentials"] == nil {
		t.Fatal("CROWDVM_LOCAL_LOGIN ignored")
	}
	e.mustRun("prompt", id, "--provider", "codex", "--local-login=off", "again")
	if b := reqBody(t, e.f.Last("POST /api/v1/sandboxes/"+id+"/prompt")); b["credentials"] != nil {
		t.Fatal("--local-login=off ignored")
	}
	// No login: a clear error, nothing sent.
	n := len(e.f.Requests)
	r = e.run("prompt", id, "--provider", "claude", "--local-login", "x")
	if r.code != 1 || !strings.Contains(r.out+r.err, "local_login_not_found") || len(e.f.Requests) != n {
		t.Fatalf("missing login: %d %s %s", r.code, r.out, r.err)
	}
}
