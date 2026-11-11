package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// hostedApp is the service "inside" the sandbox: it echoes what it received.
func hostedApp(t *testing.T) *httptest.Server {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			typ, b, err := c.Read(r.Context())
			if err == nil {
				_ = c.Write(r.Context(), typ, append([]byte("echo:"), b...))
			}
			return
		case "/redir":
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path, "query": r.URL.RawQuery, "host": r.Host,
			"cookie": r.Header.Get("Cookie"), "prefix": r.Header.Get("X-Forwarded-Prefix")})
	}))
	t.Cleanup(app.Close)
	return app
}

// appTunnel connects guest ports 3000 and 4000 to app; others are closed.
func appTunnel(app *httptest.Server) func(string, int, net.Conn) {
	return func(_ string, port int, c net.Conn) {
		if port != 3000 && port != 4000 {
			c.Close()
			return
		}
		ac, err := net.Dial("tcp", app.Listener.Addr().String())
		if err != nil {
			c.Close()
			return
		}
		mux.Join(c, ac)
	}
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type hresp struct {
	*http.Response
	body string
	js   map[string]string
}

func hget(t *testing.T, env *servertest.Env, host, path string, hdr ...string) hresp {
	t.Helper()
	req, _ := http.NewRequest("GET", env.URL+path, nil)
	if host != "" {
		req.Host = host
	}
	for _, h := range hdr {
		k, v, _ := strings.Cut(h, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := hresp{Response: resp, body: string(b)}
	_ = json.Unmarshal(b, &r.js)
	return r
}

func tokenOf(t *testing.T, u string) string {
	t.Helper()
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return pu.Query().Get("_token")
}

func TestHostPublicURLs(t *testing.T) {
	env := servertest.New(t)
	app := hostedApp(t)
	env.StartNode(servertest.NodeOptions{Tunnel: appTunnel(app)})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	label := strings.ReplaceAll(id, "_", "-")
	host := label + "-3000.localhost"

	// Validation.
	for port, code := range map[int]string{0: "invalid_port", 70000: "invalid_port", 22: "reserved_port", 7070: "reserved_port"} {
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": port}); r.Code() != code {
			t.Fatalf("port %d: %s", port, r)
		}
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000, "firewall": map[string]any{"allow": []string{"nope"}}}); r.Code() != "invalid_firewall" {
		t.Fatalf("bad cidr: %s", r)
	}
	ro := env.Key(env.Admin, "read-only")
	if r := env.Do(ro, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000}); r.Status != 403 {
		t.Fatalf("read-only key: %s", r)
	}

	// Host a protected port.
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000, "title": "App"})
	if r.Status != 200 || r.Str("type") != "port.hosted" || r.Str("access") != "private" || r.Str("isProtected") != "true" || r.Str("localhost") != "false" {
		t.Fatalf("host: %s", r)
	}
	tok := tokenOf(t, r.Str("url"))
	if tok == "" || !strings.Contains(r.Str("url"), "/p/"+id+"/3000/") || !strings.HasPrefix(r.Str("hostUrl"), "http://"+host+":") {
		t.Fatalf("urls: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000, "localhost": true}); tokenOf(t, r.Str("url")) != tok || r.Str("localhost") != "true" || r.Str("title") != "App" {
		t.Fatalf("re-host must keep the URL and token: %s", r)
	}

	// Host routing: token → redirect + cookie; cookie → proxied; no token → 401.
	if r := hget(t, env, host, "/x"); r.StatusCode != 401 {
		t.Fatalf("no token: %d %s", r.StatusCode, r.body)
	}
	if r := hget(t, env, host, "/x?_token=wrong"); r.StatusCode != 401 {
		t.Fatalf("bad token: %d", r.StatusCode)
	}
	r2 := hget(t, env, host, "/x/y?a=1&_token="+tok)
	if r2.StatusCode != 302 || r2.Header.Get("Location") != "/x/y?a=1" || !strings.Contains(r2.Header.Get("Set-Cookie"), "_port_auth="+tok) {
		t.Fatalf("token redirect: %d %v", r2.StatusCode, r2.Header)
	}
	r2 = hget(t, env, host, "/x/y?a=1", "Cookie: other=1; _port_auth="+tok+"; crowdvm_session=secret")
	if r2.StatusCode != 200 || r2.js["path"] != "/x/y" || r2.js["query"] != "a=1" || r2.js["host"] != host || r2.js["cookie"] != "other=1" {
		t.Fatalf("proxied: %d %s", r2.StatusCode, r2.body)
	}
	if r := hget(t, env, "nope-3000.localhost", "/"); r.StatusCode != 404 {
		t.Fatalf("unknown label: %d", r.StatusCode)
	}
	if r := hget(t, env, label+"-4000.localhost", "/"); r.StatusCode != 404 {
		t.Fatalf("unhosted port: %d", r.StatusCode)
	}

	// Path routing: same token, prefix stripped, sandboxed, redirects rewritten.
	p := "/p/" + id + "/3000"
	if r := hget(t, env, "", p+"?q=1"); r.StatusCode != 301 || r.Header.Get("Location") != p+"/?q=1" {
		t.Fatalf("slash redirect: %d %v", r.StatusCode, r.Header)
	}
	r2 = hget(t, env, "", p+"/a?_token="+tok)
	if r2.StatusCode != 302 || !strings.Contains(r2.Header.Get("Set-Cookie"), "Path="+p+"/") {
		t.Fatalf("path token: %d %v", r2.StatusCode, r2.Header)
	}
	r2 = hget(t, env, "", p+"/a/b", "Cookie: _port_auth="+tok)
	if r2.StatusCode != 200 || r2.js["path"] != "/a/b" || r2.js["prefix"] != p || !strings.Contains(r2.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("path proxied: %d %s %v", r2.StatusCode, r2.body, r2.Header)
	}
	if r := hget(t, env, "", p+"/redir", "Cookie: _port_auth="+tok); r.StatusCode != 302 || r.Header.Get("Location") != p+"/target" {
		t.Fatalf("location rewrite: %d %v", r.StatusCode, r.Header)
	}

	// Websockets go through (path form, cookie auth).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(env.URL, "http")+p+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {"_port_auth=" + tok}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = ws.Write(ctx, websocket.MessageText, []byte("hi"))
	if _, b, err := ws.Read(ctx); err != nil || string(b) != "echo:hi" {
		t.Fatalf("ws echo: %q %v", b, err)
	}
	ws.CloseNow()

	// bugs-r4-opus 5: hiding the port closes its open websockets at once.
	ws, _, err = websocket.Dial(ctx, "ws"+strings.TrimPrefix(env.URL, "http")+p+"/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {"_port_auth=" + tok}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := env.Do(key, "DELETE", "/api/v1/sandboxes/"+id+"/host/3000", nil); r.Status != 200 {
		t.Fatalf("hide: %s", r)
	}
	start := time.Now()
	if _, _, err := ws.Read(ctx); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("websocket still open after hide: %v after %s", err, time.Since(start))
	}
	ws.CloseNow()

	// Public clears the token; protected again mints a new one.
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000, "public": true})
	if r.Str("access") != "public" || r.Str("isProtected") != "false" || strings.Contains(r.Str("url"), "_token") {
		t.Fatalf("public: %s", r)
	}
	if r := hget(t, env, host, "/pub"); r.StatusCode != 200 || r.js["path"] != "/pub" {
		t.Fatalf("public access: %d %s", r.StatusCode, r.body)
	}
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000})
	tok2 := tokenOf(t, r.Str("url"))
	if tok2 == "" || tok2 == tok {
		t.Fatalf("token not rotated: %s", r)
	}
	if r := hget(t, env, host, "/", "Cookie: _port_auth="+tok); r.StatusCode != 401 {
		t.Fatalf("old token still works: %d", r.StatusCode)
	}

	// Firewall (CIDR allow/deny); the test client is 127.0.0.1.
	set := func(fw map[string]any) {
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000, "firewall": fw}); r.Status != 200 {
			t.Fatalf("firewall: %s", r)
		}
	}
	set(map[string]any{"deny": []string{"127.0.0.0/8"}})
	if r := hget(t, env, host, "/", "Cookie: _port_auth="+tok2); r.StatusCode != 403 {
		t.Fatalf("denied: %d", r.StatusCode)
	}
	set(map[string]any{"allow": []string{"10.0.0.0/8"}})
	if r := hget(t, env, host, "/", "Cookie: _port_auth="+tok2); r.StatusCode != 403 {
		t.Fatalf("not allowed: %d", r.StatusCode)
	}
	set(map[string]any{"allow": []string{"10.0.0.0/8", "127.0.0.1"}})
	if r := hget(t, env, host, "/", "Cookie: _port_auth="+tok2); r.StatusCode != 200 {
		t.Fatalf("allowed: %d", r.StatusCode)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000}); r.Str("firewall.allow.0") != "10.0.0.0/8" {
		t.Fatalf("firewall kept when absent: %s", r)
	}
	set(map[string]any{})

	// Nothing listening → 502.
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 5000, "public": true})
	if r := hget(t, env, label+"-5000.localhost", "/"); r.StatusCode != 502 {
		t.Fatalf("closed port: %d", r.StatusCode)
	}

	// List, get, hide (token kept).
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/host", nil); len(r.Get("ports").([]any)) != 2 {
		t.Fatalf("list: %s", r)
	}
	if r := env.Do(key, "DELETE", "/api/v1/sandboxes/"+id+"/host/3000", nil); r.Str("type") != "port.hidden" {
		t.Fatalf("hide: %s", r)
	}
	if r := hget(t, env, host, "/", "Cookie: _port_auth="+tok2); r.StatusCode != 404 {
		t.Fatalf("hidden still routed: %d", r.StatusCode)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/host/3000", nil); r.Code() != "port_not_hosted" {
		t.Fatalf("get hidden: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000}); tokenOf(t, r.Str("url")) != tok2 {
		t.Fatalf("hide must keep the token: %s", r)
	}

	// Subdomain change: new label works, the old one stops; the sandbox JSON shows the label.
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Str("sandbox.subdomain") != label {
		t.Fatalf("default subdomain: %s", r)
	}
	if r := env.Do(key, "PATCH", "/api/v1/sandboxes/"+id, map[string]any{"subdomain": "my-app"}); r.Status != 200 {
		t.Fatalf("patch subdomain: %s", r)
	}
	if r := hget(t, env, "my-app-3000.localhost", "/s", "Cookie: _port_auth="+tok2); r.StatusCode != 200 || r.js["path"] != "/s" {
		t.Fatalf("custom subdomain: %d %s", r.StatusCode, r.body)
	}
	if r := hget(t, env, host, "/", "Cookie: _port_auth="+tok2); r.StatusCode != 404 {
		t.Fatalf("old label still works: %d", r.StatusCode)
	}

	// Limit of 50 ports.
	for p := 6000; p < 6048; p++ {
		if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": p}); r.Status != 200 {
			t.Fatalf("port %d: %s", p, r)
		}
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 7000}); r.Code() != "hosted_port_limit" {
		t.Fatalf("limit: %s", r)
	}

	// Stopped sandbox: same URL answers 503; hosting refuses.
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	if r := hget(t, env, "my-app-3000.localhost", "/", "Cookie: _port_auth="+tok2); r.StatusCode != 503 {
		t.Fatalf("stopped: %d", r.StatusCode)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/host", map[string]any{"port": 3000}); r.Code() != "machine_not_running" {
		t.Fatalf("host stopped: %s", r)
	}
}

func TestHostedPortsOnCreate(t *testing.T) {
	env := servertest.New(t)
	app := hostedApp(t)
	env.StartNode(servertest.NodeOptions{Tunnel: appTunnel(app), BootDelay: 300 * time.Millisecond})
	key := env.AdminKey
	// A duplicate or malformed entry refuses the whole create (Boat).
	for _, bad := range [][]any{{3000, map[string]any{"port": 3000}}, {"x"}, {0}} {
		if r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"hostedPorts": bad}); r.Status != 400 || r.Code() != "invalid_hosted_ports" {
			t.Fatalf("bad hostedPorts %v: %s", bad, r)
		}
	}
	r := env.Do(key, "POST", "/api/v1/sandboxes", map[string]any{"hostedPorts": []any{
		map[string]any{"port": 3000, "public": true, "title": "web"}, 22, 4000}})
	if r.Status != 202 {
		t.Fatalf("create: %s", r)
	}
	id := r.Str("sandbox.id")
	if r.Str("hostedPorts.0.access") != "public" ||
		r.Str("hostedPorts.1.error") != "reserved_port" || r.Str("hostedPorts.2.access") != "private" {
		t.Fatalf("hostedPorts: %s", r)
	}
	// Routed before ready: the request waits for the machine.
	if r := hget(t, env, strings.ReplaceAll(id, "_", "-")+"-3000.localhost", "/early"); r.StatusCode != 200 || r.js["path"] != "/early" {
		t.Fatalf("early request: %d %s", r.StatusCode, r.body)
	}
	// Deleting the sandbox drops its routes.
	env.Do(key, "DELETE", "/api/v1/sandboxes/"+id, nil, "X-Ascii-Confirm-Delete: "+id)
	env.Eventually("routes dropped", func() bool {
		var n int
		_ = env.S.DB.QueryRow(`SELECT COUNT(*) FROM hosted_ports WHERE sandbox_id=?`, id).Scan(&n)
		return n == 0
	})
}

func TestSSHKeyEndpoint(t *testing.T) {
	env := servertest.New(t)
	g := http.NewServeMux()
	g.HandleFunc("POST /v1/sshkey", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "ssh_key.configured", "success": true, "machineIp": nil, "sshEndpoint": nil, "hostKey": "ssh-ed25519 AAAA"})
	})
	env.StartNode(servertest.NodeOptions{Guest: g})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/sshkey", map[string]any{"key": "ssh-ed25519 AAAA me"})
	ep := r.Str("sshEndpoint")
	host, _, err := net.SplitHostPort(ep)
	if r.Status != 200 || r.Str("type") != "ssh_key.configured" || err != nil || r.Str("machineIp") != host || r.Str("hostKey") != "ssh-ed25519 AAAA" ||
		r.Str("sshUser") != "user" || r.Str("proxyCommand") != "crowdvm tunnel "+id+" 22" {
		t.Fatalf("sshkey: %s", r)
	}
	if r := env.Do(env.Key(env.Admin, "read-only"), "POST", "/api/v1/sandboxes/"+id+"/sshkey", map[string]any{"key": "x"}); r.Status != 403 {
		t.Fatalf("read-only key: %s", r)
	}
}
