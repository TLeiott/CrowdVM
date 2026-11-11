package server_test

import (
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/crowdvm/crowdvm/internal/server"
	"github.com/crowdvm/crowdvm/internal/server/servertest"
	"github.com/crowdvm/crowdvm/web"
)

// webCall is one API request the dashboard (web/js) makes.
type webCall struct {
	file         string
	line         int
	method, path string // path normalised: /api/v1/..., params as {p}, no query
}

// Variables the dashboard builds API paths from (per file).
var webPathVars = map[string]map[string]string{
	"pages/ai.js":          {"pbase": "/credential-profiles/{p}"},
	"pages/environment.js": {"base()": "/environments/{p}", "base": "/environments/{p}"},
	"pages/org.js":         {"base": "/orgs/{p}"},
	"pages/snapshots.js":   {"path": "/saved-snapshots/{p}/shares"},
	"pages/sandbox.js":     {"base": "/sandboxes/{p}"},
}

// Calls whose path is a parameter of a generic wrapper (no fixed path).
var webWrapperCalls = map[string]bool{"pages/environment.js:11": true, "pages/environment.js:12": true, "pages/environment.js:13": true}

var webCallRe = regexp.MustCompile(`\b(?:e?api\.(get|post|put|patch|del)|(request)|(stream))\(`)
var webLitRe = regexp.MustCompile("['`](/api/v1/[^'`]*)['`]")
var identRe = regexp.MustCompile(`^[\w$.]+(\(\))?$`)

// jsArgs splits the call arguments starting at src[i] (just after "(").
func jsArgs(src string, i int) []string {
	var args []string
	depth, start := 0, i
	for ; i < len(src); i++ {
		switch c := src[i]; c {
		case '\'', '"', '`':
			i = jsSkipString(src, i)
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return append(args, strings.TrimSpace(src[start:i]))
			}
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(src[start:i]))
				start = i + 1
			}
		}
	}
	return args
}

// jsSkipString returns the index of the closing quote of the literal at src[i].
func jsSkipString(src string, i int) int {
	q := src[i]
	for i++; i < len(src); i++ {
		switch {
		case src[i] == '\\':
			i++
		case src[i] == q:
			return i
		case q == '`' && src[i] == '$' && i+1 < len(src) && src[i+1] == '{':
			i = jsSkipExpr(src, i+2)
		}
	}
	return i
}

// jsSkipExpr returns the index of the "}" closing a ${...} whose body starts at i.
func jsSkipExpr(src string, i int) int {
	depth := 0
	for ; i < len(src); i++ {
		switch src[i] {
		case '\'', '"', '`':
			i = jsSkipString(src, i)
		case '{':
			depth++
		case '}':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return i
}

// jsEval resolves a path expression to its possible values (nil = unknown).
func jsEval(e string, vars map[string]string) []string {
	e = strings.TrimSpace(e)
	if e == "" {
		return nil
	}
	if v, ok := vars[e]; ok {
		return []string{v}
	}
	switch e[0] {
	case '\'', '"':
		if jsSkipString(e, 0) == len(e)-1 {
			return []string{e[1 : len(e)-1]}
		}
		return nil
	case '`':
		if jsSkipString(e, 0) != len(e)-1 {
			return jsTernary(e, vars)
		}
		outs := []string{""}
		body := e[1 : len(e)-1]
		for i := 0; i < len(body); i++ {
			if body[i] == '$' && i+1 < len(body) && body[i+1] == '{' {
				end := jsSkipExpr(body, i+2)
				inner := body[i+2 : end]
				alts := jsTernary(inner, vars)
				if alts == nil {
					if v, ok := vars[strings.TrimSpace(inner)]; ok {
						alts = []string{v}
					} else {
						alts = []string{"{p}"}
					}
				}
				var next []string
				for _, o := range outs {
					for _, a := range alts {
						next = append(next, o+a)
					}
				}
				outs, i = next, end
				continue
			}
			for j := range outs {
				outs[j] += string(body[i])
			}
		}
		return outs
	}
	return jsTernary(e, vars)
}

// jsTernary resolves "c ? A : B" where A and B are literals.
func jsTernary(e string, vars map[string]string) []string {
	q := strings.Index(e, "?")
	if q < 0 {
		return nil
	}
	rest := e[q+1:]
	// Find the top-level ':' after the first branch.
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '\'', '"', '`':
			i = jsSkipString(rest, i)
		case ':':
			a, b := jsEval(rest[:i], vars), jsEval(rest[i+1:], vars)
			if a == nil || b == nil {
				return nil
			}
			return append(a, b...)
		}
	}
	return nil
}

func webCalls(t *testing.T) []webCall {
	root := filepath.Join("..", "..", "web", "js")
	var calls []webCall
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".js") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		src := string(b)
		lineOf := func(off int) int { return strings.Count(src[:off], "\n") + 1 }
		vars := webPathVars[rel]
		add := func(off int, method, path string) {
			if i := strings.IndexByte(path, '?'); i >= 0 {
				path = path[:i]
			}
			calls = append(calls, webCall{rel, lineOf(off), method, path})
		}
		for _, m := range webCallRe.FindAllStringSubmatchIndex(src, -1) {
			ln := lineOf(m[0])
			if ls := strings.LastIndexByte(src[:m[0]], '\n') + 1; strings.HasPrefix(strings.TrimSpace(src[ls:m[0]]), "//") {
				continue // comment
			}
			if webWrapperCalls[rel] || webWrapperCalls[rel+":"+itoa(ln)] {
				continue
			}
			args := jsArgs(src, m[1])
			var method, expr, prefix string
			switch {
			case m[2] >= 0:
				method = map[string]string{"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH", "del": "DELETE"}[src[m[2]:m[3]]]
				prefix = "/api/v1"
				if len(args) > 0 {
					expr = args[0]
				}
			case m[4] >= 0:
				if len(args) < 2 {
					continue
				}
				ms := jsEval(args[0], nil)
				if len(ms) != 1 && rel == "api.js" {
					continue
				}
				if len(ms) != 1 {
					t.Errorf("%s:%d: request() with a non-literal method %q", rel, ln, args[0])
					continue
				}
				method, expr = ms[0], args[1]
			default:
				method, prefix = "POST", "/api/v1"
				if len(args) > 0 {
					expr = args[0]
				}
			}
			paths := jsEval(expr, vars)
			if paths == nil && rel == "api.js" {
				continue // the generic helpers themselves
			}
			if paths == nil {
				t.Errorf("%s:%d: cannot resolve the API path %q", rel, ln, expr)
				continue
			}
			for _, p := range paths {
				if !strings.HasPrefix(p, "/") {
					p = "/" + p
				}
				add(m[0], method, prefix+p)
			}
		}
		// Other literal API URLs (links, websocket URLs): GET.
		if rel == "api.js" {
			return nil
		}
		for _, m := range webLitRe.FindAllStringSubmatchIndex(src, -1) {
			lit := src[m[0]:m[1]]
			lineStart := strings.LastIndexByte(src[:m[0]], '\n') + 1
			if strings.HasPrefix(strings.TrimSpace(src[lineStart:m[0]]), "//") {
				continue // comment
			}
			for _, p := range jsEval(lit, vars) {
				add(m[0], "GET", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return calls
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestR8WebAPIPathsMatchRoutes: every API path the dashboard calls is served
// by a registered route with that method. The request goes without
// credentials, so a real route answers 401 (or its own validation error)
// without side effects; a missing one answers the "Unknown API route" 404,
// a 405, the 501 not_implemented stub or the plain mux 404.
func TestR8WebAPIPathsMatchRoutes(t *testing.T) {
	calls := webCalls(t)
	if len(calls) < 150 {
		t.Fatalf("extracted only %d dashboard API calls; the extractor is broken", len(calls))
	}
	env := servertest.New(t)
	type key struct{ method, path string }
	seen := map[key][]string{}
	for _, c := range calls {
		k := key{c.method, c.path}
		seen[k] = append(seen[k], c.file+":"+itoa(c.line))
	}
	keys := make([]key, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].path+keys[i].method < keys[j].path+keys[j].method })
	for _, k := range keys {
		u := env.URL + strings.ReplaceAll(k.path, "{p}", "x1")
		req, _ := http.NewRequest(k.method, u, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", k.method, u, err)
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		res.Body.Close()
		ct := res.Header.Get("Content-Type")
		miss := ""
		switch {
		case res.StatusCode == 404 && strings.Contains(string(body), "Unknown API route"):
			miss = "404 Unknown API route"
		case res.StatusCode == 405:
			miss = "405 method not allowed"
		case res.StatusCode == 501:
			miss = "501 " + strings.TrimSpace(string(body))
		case res.StatusCode == 404 && !strings.Contains(ct, "json"):
			miss = "404 " + strings.TrimSpace(string(body))
		}
		if miss != "" {
			t.Errorf("%s: dashboard calls %s %s; the server has no such route (%s)", strings.Join(seen[k], ", "), k.method, k.path, miss)
		} else {
			t.Logf("ok %d %s %s", res.StatusCode, k.method, k.path)
		}
	}
}

// htmlGuest answers the guest file reads with an HTML page, as a guest
// agent replaced by the sandbox's user (passwordless sudo) can.
type htmlGuest struct{ *servertest.FakeGuest }

func (g htmlGuest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && (r.URL.Path == "/v1/files/raw" || r.URL.Path == "/v1/files") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<script>fetch('/api/v1/api-keys/scoped',{method:'POST',credentials:'same-origin',headers:{'X-CrowdVM-CSRF':'1'}})</script>`))
		return
	}
	g.FakeGuest.ServeHTTP(w, r)
}

// TestR8WebGuestFileServedAsHTMLOnServerOrigin: the files download URL the
// dashboard hands out (POST /files/download-url -> /files/signed, no auth
// needed) and GET /sandboxes/{id}/files proxy the guest's response headers
// verbatim. A sandbox's owner controls its guest, so a link to the signed
// URL renders attacker HTML on the dashboard's origin: script there runs
// with the victim's session cookie and passes the CSRF check (any header
// "X-CrowdVM-CSRF: 1"). Path-routed public URLs get "Content-Security-
// Policy: sandbox" for exactly this reason (feat_host.go); these do not,
// and there is no nosniff or attachment disposition either.
func TestR8WebGuestFileServedAsHTMLOnServerOrigin(t *testing.T) {
	env := servertest.New(t)
	env.StartNode(servertest.NodeOptions{Guest: htmlGuest{servertest.NewFakeGuest()}})
	_, mallory := env.NewUser("r8mallory", false)
	id := createSandbox(t, env, mallory, map[string]any{})
	env.WaitState(mallory, id, "ready")
	r := env.Do(mallory, "POST", "/api/v1/sandboxes/"+id+"/files/download-url", map[string]any{"path": "page.html", "ttlSeconds": 3600})
	u := r.Str("url")
	if u == "" {
		t.Fatalf("download-url: %s", r)
	}
	res, err := http.Get(u) // the victim's browser following the link
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.HasPrefix(u, env.URL+"/") {
		t.Fatalf("signed URL %s is not on the server origin", u)
	}
	ct, csp, disp := res.Header.Get("Content-Type"), res.Header.Get("Content-Security-Policy"), res.Header.Get("Content-Disposition")
	if res.StatusCode == 200 && strings.HasPrefix(ct, "text/html") && !strings.Contains(csp, "sandbox") && !strings.HasPrefix(disp, "attachment") {
		t.Errorf("GET %s (signed file URL, dashboard origin) serves guest-chosen Content-Type %q with no CSP sandbox and no attachment disposition; body %q runs as the server origin", u[len(env.URL):strings.Index(u, "?")], ct, body)
	}
}

// TestR8WebDashboardFramable: the dashboard sets no X-Frame-Options or CSP
// frame-ancestors. Its /device?code=… page pre-fills the attacker's code and
// focuses "Approve sign-in", so a framing page can clickjack one click into
// handing the attacker's CLI an API key acting as the victim. The server's
// own fallback /device page (no dashboard) sends X-Frame-Options: DENY for
// this reason (device.go handleDevicePage); with the dashboard mounted the
// SPA fallback serves /device without it.
func TestR8WebDashboardFramable(t *testing.T) {
	env := servertest.New(t, func(c *server.Config) { c.WebFS = web.FS })
	for _, p := range []string{"/device?code=ABCD-EFGH", "/sandboxes", "/"} {
		res, err := http.Get(env.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		xfo, csp := res.Header.Get("X-Frame-Options"), res.Header.Get("Content-Security-Policy")
		if res.StatusCode == 200 && xfo == "" && !strings.Contains(csp, "frame-ancestors") {
			t.Errorf("GET %s: dashboard page (%s) can be framed: no X-Frame-Options, no CSP frame-ancestors", p, res.Header.Get("Content-Type"))
		}
	}
}
