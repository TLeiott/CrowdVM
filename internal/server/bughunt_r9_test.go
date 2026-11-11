package server_test

import (
	"net/http"
	"net/http/httptest"

	"github.com/crowdvm/crowdvm/internal/server"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server/servertest"
)

// hostileGuest answers the file reads with extra response headers, as a guest
// agent replaced by the sandbox's user (passwordless sudo) can.
type hostileGuest struct{ *servertest.FakeGuest }

func (g hostileGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/files/raw" || r.URL.Path == "/v1/files") {
		h := w.Header()
		h.Set("Content-Type", "text/plain")
		h.Add("Set-Cookie", "crowdvm_session=attacker; Path=/")
		h.Set("Clear-Site-Data", `"cookies", "storage"`)
		h.Set("Refresh", "0; url=https://evil.example/")
		_, _ = w.Write([]byte("hi"))
		return
	}
	g.FakeGuest.ServeHTTP(w, r)
}

// bugs-r9-sonnet 2: guardGuestContent (passthrough.go:289) fixes the content
// type, CSP and nosniff, but the guest's other response headers still pass:
// Set-Cookie, Clear-Site-Data and Refresh act on the server's origin (the
// dashboard's) whatever the CSP sandbox says. The signed file URL needs no
// auth, so a link to it makes the victim's browser take the cookie.
func TestBughuntR9GuestHeadersReachServerOrigin(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: hostileGuest{servertest.NewFakeGuest()}})
	_, mallory := env.NewUser("r9mallory", false)
	id := createSandbox(t, env, mallory, map[string]any{})
	env.WaitState(mallory, id, "ready")
	r := env.Do(mallory, "POST", "/api/v1/sandboxes/"+id+"/files/download-url", map[string]any{"path": "x.txt", "ttlSeconds": 3600})
	u := r.Str("url")
	if u == "" {
		t.Fatalf("download-url: %s", r)
	}
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, h := range []string{"Set-Cookie", "Clear-Site-Data", "Refresh"} {
		if v := res.Header.Values(h); len(v) > 0 {
			t.Errorf("signed file URL passes the guest's %s header to the server origin: %q", h, v)
		}
	}
}

// bugs-r9-sonnet 3: a path-routed hosted port (/p/{id}/{port}/...) is served
// on the dashboard's origin, and ModifyResponse (feat_host.go:738) only adds
// the CSP sandbox and rewrites Location. The app's Set-Cookie passes, so any
// user's public port can set or clear the dashboard's session cookie
// (Path=/) of whoever opens the link: login CSRF into the attacker's
// account, or a forced logout. CSP sandbox does not limit response headers.
func TestBughuntR9PathRoutedAppSetsDashboardCookie(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: server.SessionCookie, Value: "attackers-session", Path: "/"})
		w.Header().Set("Clear-Site-Data", `"cookies"`)
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
	res := hget(t, env, "", "/p/"+id+"/3000/")
	if res.StatusCode != 200 {
		t.Fatalf("path-routed GET: %d %s", res.StatusCode, res.body)
	}
	for _, h := range []string{"Set-Cookie", "Clear-Site-Data"} {
		if v := res.Header.Values(h); len(v) > 0 {
			t.Errorf("path-routed app response (dashboard origin) passes its %s header: %q", h, v)
		}
	}
}

// bugs-r9-sonnet 6: the replay of an Idempotency-Key create (account.go
// idemBegin) rebuilds its answer from the sandbox row alone: it drops what
// the first answer carried besides it. With hostedPorts, the replay has no
// hostedPorts: the URLs with their access tokens only the first answer held
// are gone, and a client that retried after a lost response cannot learn them.
func TestBughuntR9IdempotentReplayKeepsHostedPorts(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{})
	key := env.AdminKey
	body := map[string]any{"hostedPorts": []any{map[string]any{"port": 3000, "title": "web"}}}
	first := env.Do(key, "POST", "/api/v1/sandboxes", body, "Idempotency-Key: r9-hosted-1")
	if first.Status != 202 || first.Str("hostedPorts.0.url") == "" {
		t.Fatalf("first create: %s", first)
	}
	replay := env.Do(key, "POST", "/api/v1/sandboxes", body, "Idempotency-Key: r9-hosted-1")
	if replay.Status != 202 || replay.Str("sandbox.id") != first.Str("sandbox.id") {
		t.Fatalf("replay: %s", replay)
	}
	if replay.Str("hostedPorts.0.url") == "" {
		t.Errorf("replay of the same create has no hostedPorts (first answer had %q): %s", first.Str("hostedPorts.0.url"), replay)
	}
}
