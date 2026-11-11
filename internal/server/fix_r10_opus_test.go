package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// bugs-r10-opus 4: a host-routed app (<label>-<port>.<public-domain>) must not
// set cookies for the whole public domain (the dashboard's session cookie
// included) nor act on the origin with Clear-Site-Data, Refresh, NEL or
// Report-To. Host-only cookies stay.
func TestR10OpusHostRoutedAppHeaders(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Set-Cookie", "crowdvm_session=attacker; Domain=localhost; Path=/api/")
		h.Add("Set-Cookie", "crowdvm_session=attacker2; Path=/")
		h.Add("Set-Cookie", "_port_auth=x; Path=/")
		h.Add("Set-Cookie", "wide=1; Domain=localhost; Path=/")
		h.Add("Set-Cookie", "own=ok; Path=/")
		h.Set("Clear-Site-Data", `"cookies"`)
		h.Set("Refresh", "0; url=https://evil.example/")
		h.Set("NEL", `{"report_to":"x","max_age":1}`)
		h.Set("Report-To", `{"group":"x","max_age":1,"endpoints":[{"url":"https://evil.example"}]}`)
		_, _ = w.Write([]byte("hi"))
	}))
	defer app.Close()
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Tunnel: appTunnel(app)})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000, "public": true}); r.Status != 200 {
		t.Fatalf("host: %s", r)
	}
	host := strings.ReplaceAll(id, "_", "-") + "-3000.localhost"
	for name, res := range map[string]hresp{
		"host-routed": hget(t, env, host, "/"),
		"path-routed": hget(t, env, "", "/p/"+id+"/3000/"),
	} {
		if res.StatusCode != 200 {
			t.Fatalf("%s GET: %d %s", name, res.StatusCode, res.body)
		}
		for _, hd := range []string{"Clear-Site-Data", "Refresh", "NEL", "Report-To"} {
			if v := res.Header.Values(hd); len(v) > 0 {
				t.Errorf("%s: %s passes: %q", name, hd, v)
			}
		}
		for _, c := range res.Header.Values("Set-Cookie") {
			if strings.Contains(c, "crowdvm_session") || strings.Contains(c, "_port_auth") || strings.Contains(strings.ToLower(c), "domain=") {
				t.Errorf("%s: dangerous Set-Cookie passes: %q", name, c)
			}
		}
	}
	// A host-only cookie belongs to the app's own host and stays.
	res := hget(t, env, host, "/")
	if v := res.Header.Values("Set-Cookie"); len(v) != 1 || v[0] != "own=ok; Path=/" {
		t.Errorf("host-routed Set-Cookie = %q, want only the host-only cookie", v)
	}
}

// navGuest answers file reads with NEL and Report-To (bugs-r10-opus 4:
// guardGuestContent drops them too).
type navGuest struct{ *servertest.FakeGuest }

func (g navGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/files/raw" || r.URL.Path == "/v1/files") {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("NEL", `{"report_to":"x","max_age":1}`)
		w.Header().Set("Report-To", `{"group":"x","max_age":1,"endpoints":[{"url":"https://evil.example"}]}`)
		_, _ = w.Write([]byte("hi"))
		return
	}
	g.FakeGuest.ServeHTTP(w, r)
}

func TestR10OpusSignedFileURLDropsNEL(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: navGuest{servertest.NewFakeGuest()}})
	id := createSandbox(t, env, env.AdminKey, nil)
	env.WaitState(env.AdminKey, id, "ready")
	u := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes/"+id+"/files/download-url", map[string]any{"path": "x.txt", "ttlSeconds": 3600}).Str("url")
	if u == "" {
		t.Fatal("no download url")
	}
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, hd := range []string{"NEL", "Report-To"} {
		if v := res.Header.Values(hd); len(v) > 0 {
			t.Errorf("signed file URL passes %s: %q", hd, v)
		}
	}
}

// bugs-r10-opus 5: a foreign deploy also removes the conversations the
// coding-agent harnesses keep in the home (not their config or auth files).
func TestR10OpusForeignDeployScrubsHarnessTranscripts(t *testing.T) {
	env := servertest.New(t)
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	if r := env.Do(key, "POST", "/api/v1/saved-snapshots", map[string]any{"sandboxId": id, "name": "tpl10"}); r.Status != 202 {
		t.Fatalf("save: %s", r)
	}
	env.WaitFor(key, "/api/v1/saved-snapshots/tpl10", "snapshot.status", 5*time.Second, "ready")
	code := env.Do(key, "POST", "/api/v1/saved-snapshots/tpl10/shares", nil).Str("shareCode")
	_, bobKey := env.NewUser("r10bob", false)
	bobs := createSandbox(t, env, bobKey, map[string]any{"from": code})
	env.WaitState(bobKey, bobs, "ready")
	sc := guestSpec(t, node, bobs).Scrub
	if sc == nil || !sc.History {
		t.Fatalf("scrub: %+v", sc)
	}
	for _, d := range []string{".claude/projects", ".claude/sessions", ".claude/todos", ".claude/shell-snapshots",
		".codex/sessions", ".local/share/opencode/storage", ".local/share/opencode/log", ".pi/agent/sessions"} {
		if !slices.Contains(sc.Dirs, d) {
			t.Errorf("scrub dirs miss %s: %v", d, sc.Dirs)
		}
	}
	for _, f := range []string{".claude/history.jsonl", ".codex/history.jsonl"} {
		if !slices.Contains(sc.Files, f) {
			t.Errorf("scrub files miss %s: %v", f, sc.Files)
		}
	}
	for _, p := range append(slices.Clone(sc.Files), sc.Dirs...) {
		if strings.HasPrefix(p, "/") || strings.Contains(p, "settings.json") || strings.Contains(p, "config.toml") {
			t.Errorf("unexpected scrub path %q", p)
		}
	}
}

// bugs-r10-opus 7: a custom API whose host resolves to a loopback address
// (set directly in the DB: the save check only reads the URL's text) is
// refused at connect time, answers a generic 502 and never reaches the
// target. The default allows LAN addresses; the dial error stays out of the
// answer.
func TestR10OpusCustomAPIDoesNotDialLoopback(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("reached"))
	}))
	defer target.Close()
	t.Setenv("CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE", "")
	env := servertest.New(t, func(c *server.Config) { c.WebhookAllowLocal = false })
	node := env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	env.Do(key, "PUT", "/api/v1/credential-profiles/default/custom-apis/SSRF1", map[string]any{"value": "CUSTOMSECRET-R10", "baseUrl": "https://api.example.com"})
	if _, err := env.S.DB.Exec(`UPDATE credential_custom_apis SET base_url=?`, target.URL); err != nil {
		t.Fatal(err)
	}
	env.Do(key, "PUT", "/api/v1/ai-proxy", map[string]any{"enabled": true})
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	tok := guestSpec(t, node, id).AIProxy.Token
	resp := proxyReq(t, env, "GET", "/ai-proxy/custom/SSRF1/x", "", "Authorization", "Bearer "+tok)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 502 || hits.Load() != 0 {
		t.Fatalf("status %d, target hits %d: %s", resp.StatusCode, hits.Load(), b)
	}
	if s := string(b); strings.Contains(s, "127.0.0.1") || strings.Contains(s, "refused") || strings.Contains(s, "dial") || !strings.Contains(s, "The provider did not answer.") {
		t.Errorf("answer leaks the dial error: %s", s)
	}
}
