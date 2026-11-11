package cli

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/client/clienttest"
)

func TestConfigSetGetServer(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "CROWDVM_API_BASE")
	e.mustRun("config", "set", "server", "10.0.0.5:8420/")
	if v := e.mustRun("config", "get", "server").last(t)["value"]; v != "http://10.0.0.5:8420" {
		t.Fatalf("server = %v", v)
	}
	cfg, _ := loadConfig(e.dir)
	if cfg.Profiles["default"].Server != "http://10.0.0.5:8420" {
		t.Fatalf("not saved: %+v", cfg.Profiles["default"])
	}
	if r := e.run("config", "set", "nope", "x"); r.code != 2 {
		t.Fatalf("unknown key: exit %d", r.code)
	}
}

func TestShellInit(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		out, err := shellFunction(sh, "/opt/crowdvm/crowdvm")
		if err != nil || !strings.Contains(out, "CROWDVM_SHELL_ID") || !strings.Contains(out, "/opt/crowdvm/crowdvm") {
			t.Fatalf("%s: %q %v", sh, out, err)
		}
	}
	if _, err := shellFunction("tcsh", ""); err == nil {
		t.Fatal("tcsh accepted")
	}
}

func TestCurrentIsPerShell(t *testing.T) {
	e := newEnv(t)
	e.vars["CROWDVM_SHELL_ID"] = "111"
	id1 := e.mustRun("new").last(t)["id"].(string)
	e.vars["CROWDVM_SHELL_ID"] = "222"
	if r := e.run("info"); r.code != 2 {
		t.Fatalf("shell 222 has no current, got exit %d: %s", r.code, r.out)
	}
	id2 := e.mustRun("new").last(t)["id"].(string)
	e.vars["CROWDVM_SHELL_ID"] = "111"
	if got := e.mustRun("config", "get", "current").last(t)["value"]; got != id1 {
		t.Fatalf("shell 111 current = %v, want %s", got, id1)
	}
	delete(e.vars, "CROWDVM_SHELL_ID") // scripts: the profile's last
	if got := e.mustRun("config", "get", "current").last(t)["value"]; got != id2 {
		t.Fatalf("profile current = %v, want %s", got, id2)
	}
}

func TestPlacementFlags(t *testing.T) {
	e := newEnv(t)
	e.mustRun("new", "--gpu", "--node", "lab", "--node-label", "room=2", "--min-tier", "gold", "--category", "cpu=high,disk=medium")
	pl, _ := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes"))["placement"].(map[string]any)
	if pl["gpu"] != true || pl["node"] != "lab" || pl["minTier"] != "gold" ||
		pl["labels"].(map[string]any)["room"] != "2" || pl["categories"].(map[string]any)["disk"] != "medium" {
		t.Fatalf("placement %v", pl)
	}
	e.mustRun("new")
	if _, ok := reqBody(t, e.f.Find("POST", "/api/v1/sandboxes"))["placement"]; ok {
		t.Fatal("placement sent without flags")
	}
}

func TestListEmptyIsArray(t *testing.T) {
	e := newEnv(t)
	if r := e.mustRun("list"); !strings.Contains(r.out, `"sandboxes":[]`) {
		t.Fatalf("list: %s", r.out)
	}
}

func TestHostListURLHide(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	e.f.On("GET /api/v1/sandboxes/{id}/host", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "port.list", map[string]any{"ports": []any{map[string]any{"port": 3000, "url": "http://x/p/a/3000/?_token=t", "isProtected": true}}})
	})
	e.f.On("GET /api/v1/sandboxes/{id}/host/{port}", func(w http.ResponseWriter, r *http.Request) {
		clienttest.Error(w, 404, "port_not_hosted", "no")
	})
	e.f.On("POST /api/v1/sandboxes/{id}/host", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "port.hosted", map[string]any{"port": 3001, "url": "http://x/p/a/3001/?_token=u"})
	})
	e.f.On("DELETE /api/v1/sandboxes/{id}/host/{port}", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "port.hidden", map[string]any{"port": 3000})
	})
	if p := e.mustRun("host", "list", id, "--json").last(t)["ports"].([]any); len(p) != 1 || strings.Contains(p[0].(map[string]any)["url"].(string), "_token") {
		t.Fatalf("ports %v", p)
	}
	// Captured: the table, with (gated) and never the token (Boat).
	if out := e.mustRun("host", "list", id).out; !strings.Contains(out, "(gated)") || strings.Contains(out, "_token") {
		t.Fatalf("host list: %q", out)
	}
	// Captured output is the bare URL (Boat: BACKEND_URL=$(host url 3001)); --json gives the object.
	if out := strings.TrimSpace(e.mustRun("host", "url", id, "3001").out); out != "http://x/p/a/3001/?_token=u" {
		t.Fatalf("url %q", out)
	}
	if u := e.mustRun("host", "url", id, "3001", "--json").last(t)["url"]; u != "http://x/p/a/3001/?_token=u" {
		t.Fatalf("url --json %v", u)
	}
	e.mustRun("host", "hide", id, "3000")
	if e.f.Find("DELETE", "/api/v1/sandboxes/"+id+"/host/3000") == nil {
		t.Fatal("hide not sent")
	}
	// Inside a sandbox, no id means the sandbox itself (argv[0] = host).
	e.vars["CROWDVM_SANDBOX_ID"] = id
	e.mustRun(dispatchArgs("/usr/local/bin/host", []string{"hide", "3000"})...)
}

func TestBrowser(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	e.f.On("POST /api/v1/sandboxes/{id}/browser", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "browser.url", map[string]any{"browserUrl": "/desktop/view?t=1", "profile": "/data/p"})
	})
	ev := e.mustRun("browser", id, "--profile", "/data/p").last(t)
	if !strings.HasSuffix(ev["browserUrl"].(string), "/desktop/view?t=1") || !strings.HasPrefix(ev["browserUrl"].(string), "http") {
		t.Fatalf("event %v", ev)
	}
	if reqBody(t, e.f.Find("POST", "/api/v1/sandboxes/"+id+"/browser"))["profile"] != "/data/p" {
		t.Fatal("profile not sent")
	}
	if r := e.run("browser", id, "--profile", "rel"); r.code != 2 {
		t.Fatalf("relative profile: exit %d", r.code)
	}
}

func TestEmailLoginTwoStep(t *testing.T) {
	e := newEnv(t)
	delete(e.vars, "CROWDVM_API_KEY")
	e.f.On("POST /auth/email/start", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "auth.email_code_sent", map[string]any{"expiresInSeconds": 600})
	})
	e.f.On("POST /auth/email/verify", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "auth.login", map[string]any{"token": clienttest.Key, "user": map[string]any{"id": "usr_1", "login": "alice"}})
	})
	if ev := e.mustRun("login", "--email", "a@b.c").last(t); ev["event"] != "email_code_sent" || ev["email"] != "a@b.c" {
		t.Fatalf("start: %v", ev)
	}
	if ev := e.mustRun("login", "--email", "a@b.c", "--code", "123 456").last(t); ev["event"] != "signed_in" || ev["method"] != "email" {
		t.Fatalf("verify: %v", ev)
	}
	b := reqBody(t, e.f.Find("POST", "/auth/email/verify"))
	if b["code"] != "123456" || b["client"] != "cli" {
		t.Fatalf("verify body %v", b)
	}
}

func TestPromptHelpListsCatalog(t *testing.T) {
	e := newEnv(t)
	e.f.On("GET /api/v1/provider-models", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "provider.models", map[string]any{"providers": []any{map[string]any{"provider": "codex", "name": "Codex", "defaultModel": "m1",
			"models": []any{map[string]any{"id": "m1", "reasoningEfforts": []string{"low", "high"}, "fastMode": true}}}}})
	})
	r := e.mustRun("prompt", "--help")
	if !strings.Contains(r.out, "codex") || !strings.Contains(r.out, "m1 (default)") || !strings.Contains(r.out, "Fast mode (--fast):\n  codex/m1") {
		t.Fatalf("help:\n%s", r.out)
	}
}

func TestFeedbackAndBillingFallbacks(t *testing.T) {
	e := newEnv(t)
	r := e.run("feedback", "hello")
	if r.code != 1 || !strings.Contains(r.out, "feedback_unavailable") && !strings.Contains(r.out, "not_found") {
		t.Fatalf("feedback without server support: %d %s %s", r.code, r.out, r.err)
	}
	e.f.On("POST /api/v1/feedback", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "feedback.created", map[string]any{"feedback": map[string]any{"id": "fb_1"}})
	})
	e.mustRun("feedback", "fork", "is", "slow")
	if reqBody(t, e.f.Find("POST", "/api/v1/feedback"))["text"] != "fork is slow" {
		t.Fatal("feedback text")
	}
	e.f.On("GET /api/v1/billing", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "billing.status", map[string]any{"wallet": map[string]any{"balanceCredits": 7}, "policy": map[string]any{"maxActive": 3}})
	})
	if p := e.mustRun("billing", "plan").last(t)["policy"].(map[string]any); p["maxActive"] != float64(3) {
		t.Fatalf("plan %v", p)
	}
}

func TestCIAndAdmin(t *testing.T) {
	e := newEnv(t)
	e.f.On("GET /api/v1/ci", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "ci.overview", map[string]any{"jobs": []any{}, "installations": []any{map[string]any{"accountLogin": "me",
			"repositories": []any{map[string]any{"fullName": "me/x", "private": true}}}}})
	})
	e.mustRun("ci", "--repo", "me/x", "--state", "failed")
	if q := e.f.Last("GET /api/v1/ci").Query; !strings.Contains(q, "repo=me%2Fx") || !strings.Contains(q, "state=failed") {
		t.Fatalf("query %s", q)
	}
	if rs := e.mustRun("ci", "repos").last(t)["repositories"].([]any); len(rs) != 1 {
		t.Fatalf("repos %v", rs)
	}
	e.f.On("POST /api/v1/admin/credits", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "admin.credits_granted", map[string]any{"wallet": map[string]any{"balanceCredits": 100}})
	})
	e.mustRun("admin", "credits", "grant", "100", "--user", "alice")
	if b := reqBody(t, e.f.Find("POST", "/api/v1/admin/credits")); b["amount"] != float64(100) || b["user"] != "alice" {
		t.Fatalf("grant %v", b)
	}
	e.f.On("PATCH /api/v1/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		clienttest.OK(w, "node.updated", map[string]any{"node": map[string]any{"id": "n_1"}})
	})
	e.mustRun("nodes", "drain", "n_1")
	if reqBody(t, e.f.Find("PATCH", "/api/v1/nodes/n_1"))["disabled"] != true {
		t.Fatal("drain body")
	}
}

func TestIDCompletion(t *testing.T) {
	e := newEnv(t)
	id := e.f.AddSandbox("running")
	r := e.mustRun("__complete", "info", "")
	if !strings.Contains(r.out, id) || !strings.Contains(r.out, "current") {
		t.Fatalf("completion:\n%s", r.out)
	}
	n := e.f.Count("GET /api/v1/sandboxes")
	e.mustRun("__complete", "exec", "")
	if e.f.Count("GET /api/v1/sandboxes") != n {
		t.Fatal("second completion within 15s must use the cache")
	}
}

func TestAttachmentLimits(t *testing.T) {
	if err := checkAttachments(make([]string, 13)); err == nil {
		t.Fatal("13 attachments accepted")
	}
}
