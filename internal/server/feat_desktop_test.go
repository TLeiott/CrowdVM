package server_test

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// desktopGuest serves the guest's internal desktop endpoints and falls back
// to the default fake guest.
type desktopGuest struct {
	mu    sync.Mutex
	calls []string
	mux   *http.ServeMux
}

func newDesktopGuest() *desktopGuest {
	g := &desktopGuest{mux: http.NewServeMux()}
	fake := servertest.NewFakeGuest()
	g.mux.Handle("/", fake)
	g.mux.HandleFunc("POST /v1/internal/desktop/start", func(w http.ResponseWriter, r *http.Request) {
		api.WriteOK(w, 200, "desktop.started", map[string]any{"port": 5901, "display": ":1", "width": 1920, "height": 1080})
	})
	g.mux.HandleFunc("POST /v1/internal/browser/start", func(w http.ResponseWriter, r *http.Request) {
		api.WriteOK(w, 200, "browser.started", map[string]any{"running": true, "port": 5902, "display": ":2", "isolated": true})
	})
	g.mux.HandleFunc("GET /v1/internal/desktop/screenshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG fake"))
	})
	g.mux.HandleFunc("POST /v1/internal/desktop/input", func(w http.ResponseWriter, r *http.Request) {
		api.WriteOK(w, 200, "desktop.input", map[string]any{"performed": 1})
	})
	return g
}

func (g *desktopGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.calls = append(g.calls, r.Method+" "+r.URL.Path)
	g.mu.Unlock()
	g.mux.ServeHTTP(w, r)
}

func (g *desktopGuest) called(call string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if c == call {
			n++
		}
	}
	return n
}

// vncTunnel answers like an RFB server on 5901 (desktop) and 5902 (browser).
var vncDials atomic.Int32 // connections to the desktop's VNC port

func vncTunnel(_ string, port int, c net.Conn) {
	defer c.Close()
	switch port {
	case 5901:
		vncDials.Add(1)
		_, _ = c.Write([]byte("RFB 003.008\n"))
	case 5902:
		_, _ = c.Write([]byte("RFB browser\n"))
	default:
		return
	}
	buf := make([]byte, 64)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		_, _ = c.Write(buf[:n])
	}
}

func wsURL(env *servertest.Env, u string) string {
	if strings.HasPrefix(u, "/") {
		return "ws" + strings.TrimPrefix(env.URL, "http") + u
	}
	return u
}

// dialVNC opens the desktop websocket and returns the first message.
func dialVNC(t *testing.T, u string, opts *websocket.DialOptions) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, u, opts)
	if err != nil {
		if resp != nil {
			return "", resp.StatusCode
		}
		t.Fatal(err)
	}
	defer c.CloseNow()
	if c.Subprotocol() != "binary" {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), 101
}

func TestDesktop(t *testing.T) {
	env := servertest.New(t)
	g := newDesktopGuest()
	env.StartNode(servertest.NodeOptions{Guest: g, Tunnel: vncTunnel})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	env.WaitState(key, id, "ready")
	binary := &websocket.DialOptions{Subprotocols: []string{"binary"}}

	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/desktop?theme=blue", nil); r.Code() != "invalid_theme" {
		t.Fatalf("theme: %s", r)
	}
	ro := env.Key(env.Admin, "read-only")
	if r := env.Do(ro, "POST", "/api/v1/sandboxes/"+id+"/desktop", nil); r.Status != 403 {
		t.Fatalf("key without desktop action: %s", r)
	}

	r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/desktop?vnc=1&theme=dark", nil)
	if r.Status != 200 || r.Str("type") != "desktop.url" || r.Str("mode") != "vnc" || r.Str("provisioning") != "false" || g.called("POST /v1/internal/desktop/start") != 1 {
		t.Fatalf("desktop: %s", r)
	}
	du := r.Str("desktopUrl")
	if !strings.HasPrefix(du, env.URL+"/desktop/"+id+"?theme=dark#token=") || r.Str("viewerUrl") != du {
		t.Fatalf("desktopUrl: %s", du)
	}
	tok := strings.SplitN(du, "#token=", 2)[1]
	if !strings.HasSuffix(r.Str("wsUrl"), "/api/v1/sandboxes/"+id+"/desktop/ws") {
		t.Fatalf("wsUrl (no token in the URL): %s", r)
	}
	// List/info never carry the desktop URL.
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id, nil); r.Get("sandbox.desktopUrl") != nil || r.Str("sandbox.desktopAvailable") != "true" {
		t.Fatalf("info: %s", r)
	}

	// Websocket: the token goes only in Sec-WebSocket-Protocol (DESK-006).
	base := wsURL(env, r.Str("wsUrl"))
	withTok := func(tok string) *websocket.DialOptions {
		return &websocket.DialOptions{Subprotocols: []string{"binary", "crowdvm-token." + tok}}
	}
	if msg, code := dialVNC(t, base, withTok(tok)); code != 101 || msg != "RFB 003.008\n" {
		t.Fatalf("ws protocol token: %d %q", code, msg)
	}
	dials := vncDials.Load()
	if _, code := dialVNC(t, base, withTok(tok)); code != 401 { // single use (DESK-005)
		t.Fatalf("token reused: %d", code)
	}
	if vncDials.Load() != dials { // refused before any VNC connection (bugs-r5-opus 3)
		t.Fatal("a spent token opened a VNC connection")
	}
	if _, code := dialVNC(t, base+"?token="+url.QueryEscape(tok), binary); code != 401 {
		t.Fatalf("query token accepted: %d", code)
	}
	if _, code := dialVNC(t, base, withTok(tok+"x")); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
	if _, code := dialVNC(t, base, binary); code != 401 {
		t.Fatalf("no credentials: %d", code)
	}
	// An API key works without a token.
	if msg, code := dialVNC(t, base, &websocket.DialOptions{Subprotocols: []string{"binary"}, HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}}); code != 101 || msg != "RFB 003.008\n" {
		t.Fatalf("ws api key: %d %q", code, msg)
	}
	// A token is bound to its sandbox.
	other := createSandbox(t, env, key, nil)
	env.WaitState(key, other, "ready")
	if _, code := dialVNC(t, wsURL(env, "/api/v1/sandboxes/"+other+"/desktop/ws"), withTok(tok)); code != 401 {
		t.Fatalf("token on another sandbox: %d", code)
	}

	// Public access (vnc=1): no token needed until turned off.
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/desktop?vnc=1", map[string]any{"publicAccess": true})
	if r.Str("publicAccess") != "true" || strings.Contains(r.Str("desktopUrl"), "token") || strings.Contains(r.Str("wsUrl"), "token") {
		t.Fatalf("public: %s", r)
	}
	if _, code := dialVNC(t, base, binary); code != 101 {
		t.Fatalf("public ws: %d", code)
	}
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/desktop?vnc=1", map[string]any{"publicAccess": false})
	if _, code := dialVNC(t, base, binary); code != 401 {
		t.Fatalf("public off: %d", code)
	}

	// Driving endpoints map onto the guest; screenshots start the desktop first.
	before := g.called("POST /v1/internal/desktop/start")
	sr := hget(t, env, "", "/api/v1/sandboxes/"+id+"/desktop/screenshot", "Authorization: Bearer "+key)
	if sr.StatusCode != 200 || sr.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(sr.body, "\x89PNG") || g.called("POST /v1/internal/desktop/start") != before+1 {
		t.Fatalf("screenshot: %d %v", sr.StatusCode, sr.Header)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/desktop/input", map[string]any{"actions": []any{map[string]any{"type": "key", "keys": "Return"}}}); r.Str("performed") != "1" {
		t.Fatalf("input: %s", r)
	}
	if r := env.Do(key, "GET", "/api/v1/sandboxes/"+id+"/desktop/bogus", nil); r.Status != 404 {
		t.Fatalf("bogus: %s", r)
	}

	// Browser-only view: its own display, token kind enforced.
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/browser", map[string]any{"profile": "/home/user/p"}); r.Code() != "invalid_profile" {
		t.Fatalf("profile in home: %s", r)
	}
	r = env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/browser", map[string]any{"profile": "/data/chrome-profile"})
	if r.Status != 200 || r.Str("type") != "browser.url" || !strings.Contains(r.Str("browserUrl"), "view=browser#token=") || r.Str("profile") != "/data/chrome-profile" ||
		r.Get("isolated") != true || g.called("POST /v1/internal/browser/start") != 1 {
		t.Fatalf("browser: %s", r)
	}
	btok := strings.SplitN(r.Str("browserUrl"), "#token=", 2)[1]
	if msg, code := dialVNC(t, wsURL(env, r.Str("wsUrl")), withTok(btok)); code != 101 || msg != "RFB browser\n" {
		t.Fatalf("browser ws: %d %q", code, msg)
	}

	// Tokens die with the boot: stop + resume → 401.
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/stop", nil)
	env.WaitState(key, id, "archived")
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/desktop", nil); r.Code() != "machine_not_running" {
		t.Fatalf("stopped: %s", r)
	}
	env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/resume", nil)
	env.WaitState(key, id, "ready")
	if _, code := dialVNC(t, base, withTok(tok)); code != 401 {
		t.Fatalf("token after resume: %d", code)
	}
}

func TestDesktopWhileStarting(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: newDesktopGuest(), Tunnel: vncTunnel, BootDelay: 2 * time.Second})
	key := env.AdminKey
	id := createSandbox(t, env, key, nil)
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/desktop?vnc=1", nil); r.Str("type") != "desktop.provisioning" || r.Str("provisioning") != "true" {
		t.Fatalf("vnc while starting: %s", r)
	}
	if r := env.Do(key, "POST", "/api/v1/sandboxes/"+id+"/desktop", nil); r.Code() != "desktop_not_ready" {
		t.Fatalf("desktop while starting: %s", r)
	}
}
