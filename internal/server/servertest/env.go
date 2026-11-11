// Package servertest runs a real crowdvm server on httptest with an
// in-process fake node, for this repo's server tests and other components'
// end-to-end tests.
//
//	env := servertest.New(t)
//	node := env.StartNode(servertest.NodeOptions{})
//	r := env.Do(env.AdminKey, "POST", "/api/v1/sandboxes", map[string]any{"type": "small"})
//	id := r.Str("sandbox.id")
//	env.WaitState(env.AdminKey, id, "ready")
package servertest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/server"
)

// Env is a running test server.
type Env struct {
	T        testing.TB
	S        *server.Server
	HTTP     *httptest.Server
	URL      string // http://127.0.0.1:port
	Admin    *server.User
	AdminKey string // admin-preset API key of Admin
	nodes    []*FakeNode
}

// New starts a server with test-friendly settings: unlimited start/IP
// limits, fast loops, 100 signup credits. cfg tweaks the config.
func New(t testing.TB, cfg ...func(*server.Config)) *Env {
	t.Helper()
	e := NewEmpty(t, cfg...)
	e.Admin, e.AdminKey = e.NewUser("admin", true)
	return e
}

// NewEmpty is New without the admin account (for signup tests).
func NewEmpty(t testing.TB, cfg ...func(*server.Config)) *Env {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	level := slog.LevelError
	if os.Getenv("CROWDVM_TEST_LOG") != "" {
		level = slog.LevelDebug
	}
	c := server.Config{
		DataDir: t.TempDir(), PublicURL: url, SignupCredits: 100, NodeEarnRate: 1,
		StartWait: 5 * time.Second, ReaperInterval: 50 * time.Millisecond, BillingInterval: time.Hour,
		NodeTimeout: 10 * time.Second, RetryAfter: 1,
		WebhookAllowLocal: true, // receivers are httptest servers on 127.0.0.1
		Logger:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})),
	}
	for _, f := range cfg {
		f(&c)
	}
	s, err := server.New(c)
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	hs := httptest.NewUnstartedServer(s.Handler())
	hs.Listener.Close()
	hs.Listener = ln
	hs.Start()
	e := &Env{T: t, S: s, HTTP: hs, URL: url}
	t.Cleanup(e.Close)
	return e
}

// Close stops nodes and the server (also registered with t.Cleanup).
func (e *Env) Close() {
	for _, n := range e.nodes {
		n.Close()
	}
	e.nodes = nil
	if e.HTTP != nil {
		e.HTTP.CloseClientConnections()
		e.HTTP.Close()
		e.HTTP = nil
		_ = e.S.Close()
	}
}

// NewUser creates an account and returns it with an admin-preset API key.
func (e *Env) NewUser(username string, admin bool) (*server.User, string) {
	e.T.Helper()
	u, err := e.S.CreateUser(server.NewUserParams{Username: username, Password: "password-" + username, Email: username + "@example.test", Admin: admin})
	if err != nil {
		e.T.Fatal(err)
	}
	return u, e.Key(u, "admin")
}

// Key creates an API key for u with a preset ("" = admin).
func (e *Env) Key(u *server.User, preset string) string {
	e.T.Helper()
	tok, err := e.S.NewSession(u.ID, "cli", "servertest")
	if err != nil {
		e.T.Fatal(err)
	}
	body := map[string]any{"name": "test " + preset, "ttl": "1d"}
	if preset != "" {
		body["preset"] = preset
	}
	r := e.Do(tok, "POST", "/api/v1/api-keys/scoped", body)
	if r.Status != 201 {
		e.T.Fatalf("create key: %d %s", r.Status, r.Body)
	}
	return r.Str("secret")
}

// Session returns a CLI session token (cvms_...) for u.
func (e *Env) Session(u *server.User) string {
	tok, err := e.S.NewSession(u.ID, "cli", "servertest")
	if err != nil {
		e.T.Fatal(err)
	}
	return tok
}

// JoinToken creates a join token owned by the admin.
func (e *Env) JoinToken() string {
	owner := ""
	if e.Admin != nil {
		owner = e.Admin.ID
	}
	_, tok, err := e.S.CreateJoinToken(server.JoinTokenParams{Name: "test", CreatedBy: owner, TTL: time.Hour})
	if err != nil {
		e.T.Fatal(err)
	}
	return tok
}

// Resp is a decoded HTTP response.
type Resp struct {
	Status int
	Header http.Header
	Body   []byte
	JSON   map[string]any
}

// Do sends a request with token as Bearer ("" = none). body may be nil, a
// []byte/string (sent raw) or any JSON value. hdr adds headers ("K: V").
func (e *Env) Do(token, method, path string, body any, hdr ...string) *Resp {
	e.T.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		js, _ := json.Marshal(b)
		rd = bytes.NewReader(js)
	}
	req, err := http.NewRequest(method, e.URL+path, rd)
	if err != nil {
		e.T.Fatal(err)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, h := range hdr {
		k, v, _ := strings.Cut(h, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := &Resp{Status: resp.StatusCode, Header: resp.Header, Body: raw}
	_ = json.Unmarshal(raw, &r.JSON)
	return r
}

// Get returns the value at a dotted path ("sandbox.state", "sandboxes.0.id").
func (r *Resp) Get(path string) any { return Lookup(r.JSON, path) }

// Str returns the value at path as a string ("" when absent).
func (r *Resp) Str(path string) string {
	v := r.Get(path)
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Code returns the error code of an error envelope.
func (r *Resp) Code() string { return r.Str("code") }

func (r *Resp) String() string { return fmt.Sprintf("%d %s", r.Status, r.Body) }

// Lookup walks a dotted path through maps and slices.
func Lookup(v any, path string) any {
	if path == "" {
		return v
	}
	for _, p := range strings.Split(path, ".") {
		switch t := v.(type) {
		case map[string]any:
			v = t[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(t) {
				return nil
			}
			v = t[i]
		default:
			return nil
		}
	}
	return v
}

// WaitTimeout bounds WaitState and Eventually. It is generous on purpose:
// under `go test -race ./...` every package competes for the CPU, and a
// wait only takes this long when it fails.
var WaitTimeout = 45 * time.Second

// WaitState polls GET /api/v1/sandboxes/{id} until its state is one of
// states (WaitTimeout) and returns the last response.
func (e *Env) WaitState(token, id string, states ...string) *Resp {
	e.T.Helper()
	return e.WaitFor(token, "/api/v1/sandboxes/"+id, "sandbox.state", WaitTimeout, states...)
}

// WaitFor polls path until the value at field is one of values.
func (e *Env) WaitFor(token, path, field string, d time.Duration, values ...string) *Resp {
	e.T.Helper()
	deadline := time.Now().Add(d)
	var r *Resp
	for {
		r = e.Do(token, "GET", path, nil)
		got := r.Str(field)
		for _, v := range values {
			if got == v {
				return r
			}
		}
		if time.Now().After(deadline) {
			e.T.Fatalf("timeout waiting for %s %s in %v; last: %s", path, field, values, r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Eventually retries cond until it is true (WaitTimeout).
func (e *Env) Eventually(what string, cond func() bool) {
	e.T.Helper()
	deadline := time.Now().Add(WaitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.T.Fatalf("timeout: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
