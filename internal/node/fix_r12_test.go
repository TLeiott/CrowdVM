package node

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// bugs-r12-opus 1 and 2 regression tests.

// sessionTransport adds the page session header to every request, as the
// page's JS does: bugs-r13-opus 5 signs each request, it no longer sends the
// session itself.
type sessionTransport string

func (s sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return signedTransport{sess: string(s), at: time.Now}.RoundTrip(r)
}

var sessRe = regexp.MustCompile(`localStorage\.setItem\("crowdvm_node_session", "([^"]+)"\)`)

// loginSession reads the session the /login page hands to its JS.
func loginSession(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	m := sessRe.FindSubmatch(b)
	if m == nil {
		t.Fatalf("login body has no session: %s", b)
	}
	return string(m[1])
}

func newLogin(t *testing.T, base, token string) *http.Response {
	t.Helper()
	resp := uiPost(t, http.DefaultClient, base+"/api/login-code", token, "")
	var lc struct{ Code string }
	json.NewDecoder(resp.Body).Decode(&lc)
	resp.Body.Close()
	// no redirect following: what /login itself sends is what counts
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := c.Get(base + "/login?c=" + lc.Code)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUISessionHeaderNoCookie(t *testing.T) {
	h := start(t, nil)
	srv := httptest.NewServer(h.node.UIHandler("tok"))
	defer srv.Close()

	resp := newLogin(t, srv.URL, "tok")
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	for _, ck := range resp.Header["Set-Cookie"] { // only an expiry of the old cookie is allowed
		if !regexp.MustCompile(`^crowdvm_node_ui=;`).MatchString(ck) || !regexp.MustCompile(`Max-Age=0|expires=`).MatchString(ck) {
			t.Fatalf("login sets a cookie: %q", ck)
		}
	}
	if resp.Header.Get("Location") != "" {
		t.Fatalf("login redirects (the session would travel in the URL): %q", resp.Header.Get("Location"))
	}
	sess := loginSession(t, resp)

	// The page API takes the session header ...
	c := &http.Client{Transport: sessionTransport(sess)}
	if st := getStatus(t, c, srv.URL); !st.Authorized {
		t.Fatal("status with the session header is not authorized")
	}
	if r := uiPost(t, c, srv.URL+"/api/pause", "", `{"paused":true}`); r.StatusCode != 200 {
		t.Fatalf("pause with session: %d", r.StatusCode)
	}
	// ... and refuses requests without it, with a wrong one, or with the old cookie.
	if st := getStatus(t, http.DefaultClient, srv.URL); st.Authorized {
		t.Fatal("status without the session is authorized")
	}
	if r := uiPost(t, http.DefaultClient, srv.URL+"/api/pause", "", `{"paused":false}`); r.StatusCode != 401 {
		t.Fatalf("pause without session: %d", r.StatusCode)
	}
	bad := &http.Client{Transport: sessionTransport("00.11")}
	if r := uiPost(t, bad, srv.URL+"/api/pause", "", `{"paused":false}`); r.StatusCode != 401 {
		t.Fatalf("pause with a forged session: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/pause", nil)
	req.Header.Set(UIHeader, "1")
	req.AddCookie(&http.Cookie{Name: "crowdvm_node_ui", Value: sess})
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 401 {
		t.Fatalf("pause with the session as a cookie: %d", r.StatusCode)
	}
	// The session is not the token: CLI-only calls stay closed to it.
	if r := uiPost(t, c, srv.URL+"/api/login-code", "", ""); r.StatusCode != 401 {
		t.Fatalf("login code with session: %d", r.StatusCode)
	}
	// The CSRF header is still required.
	req, _ = http.NewRequest("POST", srv.URL+"/api/pause", nil)
	req.Header.Set(UISessionHeader, sess)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 403 {
		t.Fatalf("pause without %s: %d", UIHeader, r.StatusCode)
	}
}

func TestUISessionLogoutAndRestart(t *testing.T) {
	h := start(t, nil)
	srv := httptest.NewServer(h.node.UIHandler("tok"))
	defer srv.Close()
	s1 := loginSession(t, newLogin(t, srv.URL, "tok"))
	s2 := loginSession(t, newLogin(t, srv.URL, "tok"))
	if s1 == s2 {
		t.Fatal("two logins share a session")
	}
	c1 := &http.Client{Transport: sessionTransport(s1)}
	c2 := &http.Client{Transport: sessionTransport(s2)}
	if r := uiPost(t, c1, srv.URL+"/api/logout", "", ""); r.StatusCode != 200 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if getStatus(t, c1, srv.URL).Authorized {
		t.Fatal("session still valid after logout")
	}
	if !getStatus(t, c2, srv.URL).Authorized {
		t.Fatal("logout ended another login")
	}
	// A restart (a new handler on the same data dir) keeps the other login.
	srv2 := httptest.NewServer(h.node.UIHandler("tok"))
	defer srv2.Close()
	if !getStatus(t, c2, srv2.URL).Authorized || getStatus(t, c1, srv2.URL).Authorized {
		t.Fatal("sessions did not survive a handler restart as saved")
	}
	// A rotated token ends every session.
	srv3 := httptest.NewServer(h.node.UIHandler("tok2"))
	defer srv3.Close()
	if getStatus(t, c2, srv3.URL).Authorized {
		t.Fatal("session survived a token rotation")
	}
	if b, _ := os.ReadFile(filepath.Join(h.cfg.DataDir, uiSessionsFile)); len(b) == 0 {
		t.Fatal("sessions file not written")
	}
}

func TestUIHelloProof(t *testing.T) {
	h := start(t, nil)
	srv := httptest.NewServer(h.node.UIHandler("tok"))
	defer srv.Close()
	get := func(base, nonce string) (int, string) {
		resp, err := http.Get(base + "/api/hello?nonce=" + nonce)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ Proof string }
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Proof
	}
	code, proof := get(srv.URL, "abc123")
	if code != 200 || proof != HelloProof("tok", "abc123") || proof == HelloProof("tok2", "abc123") || proof == HelloProof("tok", "abc124") {
		t.Fatalf("hello: %d %q", code, proof)
	}
	if code, _ := get(srv.URL, ""); code == 200 {
		t.Fatal("hello without a nonce answers")
	}
	srv0 := httptest.NewServer(h.node.UIHandler(""))
	defer srv0.Close()
	if code, p := get(srv0.URL, "abc"); code == 200 || p != "" {
		t.Fatalf("a daemon without a token proves something: %d %q", code, p)
	}
}
