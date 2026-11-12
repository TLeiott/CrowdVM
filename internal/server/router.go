package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
)

// Router wraps http.ServeMux with the server middleware (request ids, IP
// rate limit, panic recovery) and remembers registered patterns so stubs can
// be replaced by later features.
type Router struct {
	s     *Server
	mux   *http.ServeMux
	pats  map[string]bool
	stubs []stubRoute
	pre   []func(http.ResponseWriter, *http.Request) bool
	apis  []apiRoute // API/Route registrations (served as OpenAPI, acct_openapi.go)
}

// apiRoute is one registration of Server.API (rel) or Server.Route.
type apiRoute struct {
	Method, Path, Action string
	Rel                  bool // Path is relative to /api/v1
}

type stubRoute struct {
	pattern, code, msg string
	status             int
}

func newRouter(s *Server) *Router {
	return &Router{s: s, mux: http.NewServeMux(), pats: map[string]bool{}}
}

// Handle registers a raw handler (Go ServeMux pattern syntax).
func (r *Router) Handle(pattern string, h http.Handler) {
	r.pats[normPattern(pattern)] = true
	r.mux.Handle(pattern, h)
}

var wildRe = regexp.MustCompile(`\{[^}.]*(\.\.\.)?\}`)

// normPattern drops wildcard names so "{id}" and "{sandboxId}" compare equal.
func normPattern(p string) string { return wildRe.ReplaceAllString(p, "{$1}") }

// Intercept registers fn to run before routing and the IP rate limit (e.g.
// host-routed public URLs). fn returns true when it handled the request.
func (r *Router) Intercept(fn func(http.ResponseWriter, *http.Request) bool) {
	r.pre = append(r.pre, fn)
}

// HandleFunc registers a raw handler func.
func (r *Router) HandleFunc(pattern string, fn func(http.ResponseWriter, *http.Request)) {
	r.Handle(pattern, http.HandlerFunc(fn))
}

// Has reports whether pattern (or one differing only in wildcard names) was registered.
func (r *Router) Has(pattern string) bool { return r.pats[normPattern(pattern)] }

// covered reports whether a feature serves a stub's routes: the same
// pattern, or for a method-less stub any route on its path (a "{rest...}"
// stub: any route below it). Unsupported methods and unknown sub-paths of a
// served feature then get the JSON 404 instead of 501 not_implemented.
func (r *Router) covered(stub string) bool {
	if r.Has(stub) {
		return true
	}
	if strings.Contains(stub, " ") {
		return false
	}
	n := normPattern(stub)
	prefix, wild := strings.CutSuffix(n, "{...}")
	for p := range r.pats {
		_, path, ok := strings.Cut(p, " ")
		if !ok {
			path = p
		}
		if path == n || (wild && len(path) > len(prefix) && strings.HasPrefix(path, prefix)) {
			return true
		}
	}
	return false
}

// Stub registers an error response for pattern at the end of setup, unless a
// feature registered the pattern itself.
func (r *Router) Stub(pattern string, status int, code, msg string) {
	r.stubs = append(r.stubs, stubRoute{pattern, code, msg, status})
}

func (r *Router) finish() {
	for _, st := range r.stubs {
		if r.covered(st.pattern) {
			continue
		}
		st := st
		r.Handle(st.pattern, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			api.WriteError(w, api.NewError(st.status, st.code, st.msg, nil))
		}))
	}
	r.s.mountWeb()
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rid := api.NewID(api.PrefixRequest, 20)
	w.Header().Set("X-Request-Id", rid)
	sw := &statusWriter{ResponseWriter: w}
	start := time.Now()
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			r.s.Log.Error("panic", "path", req.URL.Path, "err", v, "requestId", rid)
			if sw.status == 0 {
				api.WriteError(sw, api.NewError(500, "internal_error", "Internal server error.", nil))
			}
		}
		if r.s.Log.Enabled(req.Context(), -4) {
			r.s.Log.Debug("http", "method", req.Method, "path", req.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds(), "requestId", rid)
		}
	}()
	for _, fn := range r.pre {
		if fn(sw, req) {
			return
		}
	}
	if strings.HasPrefix(req.URL.Path, "/api/") && r.s.clientGate(sw, req) {
		return
	}
	if limited(req.URL.Path) {
		// bugs-r11-opus 9: a request that authenticates counts against its
		// user, not its IP (all guests of a node share the node's IP). One
		// that does not (no credentials, or invalid ones) keeps the IP bucket,
		// so random tokens cannot open buckets of their own.
		key := r.s.clientIP(req)
		// Only on /api/: the public routes under /auth/, /agent/ and
		// /oauth2/ (logins, codes, anonymous agent keys) ignore credentials,
		// so a key there must not open a bucket of its own (bugs-r12-sonnet 3).
		if strings.HasPrefix(req.URL.Path, "/api/") && (req.Header.Get("Authorization") != "" || hasSessionCookie(req)) {
			if p, err := r.s.authenticate(sw, req); err == nil {
				key = "user:" + p.User.ID
				req = req.WithContext(context.WithValue(req.Context(), principalKey{}, p))
			}
		}
		if !r.s.ipLim.allow(sw, key) {
			return
		}
	}
	r.mux.ServeHTTP(sw, req)
}

type principalKey struct{}

func hasSessionCookie(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	return err == nil && c.Value != ""
}

// authenticated is s.authenticate that reuses the principal the rate limit
// already resolved for this request (bugs-r11-opus 9).
func (s *Server) authenticated(w http.ResponseWriter, r *http.Request) (*Principal, error) {
	if p, ok := r.Context().Value(principalKey{}).(*Principal); ok {
		return p, nil
	}
	return s.authenticate(w, r)
}

// limited reports whether the IP rate limit applies to path.
func limited(p string) bool {
	for _, pre := range []string{"/api/", "/auth/", "/agent/", "/oauth2/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// statusWriter records the status and keeps Flush/Hijack working.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.status = 101
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ---------------------------------------------------------------------------
// API handlers
// ---------------------------------------------------------------------------

// HandlerFunc is an /api/v1 handler. Returning an *api.Error writes the error
// envelope; other errors become 500 internal_error.
type HandlerFunc func(c *Ctx) error

// Ctx is the per-request context of API handlers.
type Ctx struct {
	W         http.ResponseWriter
	R         *http.Request
	S         *Server
	P         *Principal // nil on public routes
	Action    string     // scope action the route requires
	RequestID string
}

// Action values with special meaning for API().
const (
	ActionPublic  = "public"  // no authentication
	ActionAny     = ""        // any authenticated principal
	ActionSession = "session" // browser/CLI session only (no API keys)
	ActionAdmin   = "admin"   // server administrators (session or "*" key)
)

// API registers an /api/v1 route. pattern is "METHOD /path" with /path
// relative to /api/v1 (e.g. "GET /sandboxes/{sandboxId}"). action is the API
// key scope action required (see scopes.go) or one of the Action* values.
func (s *Server) API(pattern, action string, fn HandlerFunc) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		path, method = method, ""
	}
	full := "/api/v1" + path
	if method != "" {
		full = method + " " + full
	}
	s.router.apis = append(s.router.apis, apiRoute{Method: method, Path: path, Action: action, Rel: true})
	s.router.Handle(full, s.wrap(action, fn))
}

// Route registers a handler on an absolute path with the same auth and error
// handling as API (e.g. "POST /auth/logout").
func (s *Server) Route(pattern, action string, fn HandlerFunc) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		path, method = method, ""
	}
	s.router.apis = append(s.router.apis, apiRoute{Method: method, Path: path, Action: action})
	s.router.Handle(pattern, s.wrap(action, fn))
}

func (s *Server) wrap(action string, fn HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := &Ctx{W: w, R: r, S: s, Action: action, RequestID: w.Header().Get("X-Request-Id")}
		if action != ActionPublic {
			p, err := s.authenticated(w, r)
			if err != nil {
				c.Fail(err)
				return
			}
			c.P = p
			if err := s.checkAction(p, r, action); err != nil {
				c.Fail(err)
				return
			}
			if p.Key != nil {
				s.usage.hit(p.Key.ID)
			}
			for _, g := range s.Hooks.Guard {
				if err := g(c); err != nil {
					c.Fail(err)
					return
				}
			}
		}
		if err := fn(c); err != nil {
			c.Fail(err)
		}
	})
}

// Fail writes err as an error envelope.
func (c *Ctx) Fail(err error) {
	var ae *api.Error
	if !errors.As(err, &ae) {
		c.S.Log.Error("request failed", "path", c.R.URL.Path, "err", err, "requestId", c.RequestID)
		ae = api.NewError(500, "internal_error", "Internal server error.", nil)
	}
	if ae.Status == 401 {
		c.W.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+c.S.Cfg.PublicURL+`/.well-known/oauth-protected-resource"`)
	}
	if ra, ok := ae.Err.Details["retryAfterSeconds"]; ok && (ae.Status == 429 || ae.Status == 503) {
		c.W.Header().Set("Retry-After", fmt.Sprint(ra))
	}
	api.WriteError(c.W, ae)
}

// OK writes a success envelope.
func (c *Ctx) OK(status int, typ string, fields any) error {
	api.WriteOK(c.W, status, typ, fields)
	return nil
}

// Bind decodes the JSON body into v (an empty body leaves v untouched).
func (c *Ctx) Bind(v any) error { return c.bind(v, false) }

// BindStrict is Bind that rejects unknown fields (400 unknown_field).
func (c *Ctx) BindStrict(v any) error { return c.bind(v, true) }

func (c *Ctx) bind(v any, strict bool) error {
	body, err := io.ReadAll(io.LimitReader(c.R.Body, 8<<20))
	if err != nil {
		return Err(400, "invalid_json", "Could not read the request body.")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		if strict && strings.Contains(err.Error(), "unknown field") {
			return Err(400, "unknown_field", "Request has an unknown field: "+strings.TrimPrefix(err.Error(), "json: "))
		}
		return Err(400, "invalid_json", "Request body must be valid JSON: "+err.Error())
	}
	return nil
}

// Param returns a path parameter.
func (c *Ctx) Param(name string) string { return c.R.PathValue(name) }

// Query returns a query parameter.
func (c *Ctx) Query(name string) string { return c.R.URL.Query().Get(name) }

// Err builds an API error.
func Err(status int, code, msg string) *api.Error { return api.NewError(status, code, msg, nil) }

// ErrD builds an API error with details.
func ErrD(status int, code, msg string, details map[string]any) *api.Error {
	return api.NewError(status, code, msg, details)
}

var (
	errNotFound     = func(what string) *api.Error { return Err(404, "not_found", what+" not found.") }
	errUnauthorized = func() *api.Error { return Err(401, "unauthorized", "Unauthorized") }
	errForbidden    = func(code, msg string) *api.Error { return Err(403, code, msg) }
)

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

// Page holds parsed cursor pagination parameters.
type Page struct {
	Limit int
	Asc   bool
	// Cursor position: (CreatedAt, ID) of the last item of the previous page.
	AfterTime int64
	AfterID   string
	HasCursor bool
}

// ParsePage reads limit/sort/cursor (limit 1..200, default 100).
func ParsePage(r *http.Request) (Page, error) {
	q := r.URL.Query()
	p := Page{Limit: 100}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return p, Err(400, "invalid_limit", "limit must be an integer from 1 to 200.")
		}
		p.Limit = n
	}
	switch q.Get("sort") {
	case "", "desc":
	case "asc":
		p.Asc = true
	default:
		return p, Err(400, "invalid_sort", "sort must be asc or desc.")
	}
	if v := q.Get("cursor"); v != "" && v != "null" {
		t, id, ok := decodeCursor(v)
		if !ok {
			return p, Err(400, "invalid_cursor", "cursor is not valid.")
		}
		p.AfterTime, p.AfterID, p.HasCursor = t, id, true
	}
	return p, nil
}

// SQL returns a WHERE fragment (with leading " AND ") and args that continue
// after the cursor, plus the ORDER BY/LIMIT tail (fetching Limit+1 rows).
func (p Page) SQL(timeCol, idCol string) (where string, args []any, tail string) {
	dir, cmp := "DESC", "<"
	if p.Asc {
		dir, cmp = "ASC", ">"
	}
	if p.HasCursor {
		where = fmt.Sprintf(" AND (%s %s ? OR (%s = ? AND %s %s ?))", timeCol, cmp, timeCol, idCol, cmp)
		args = []any{p.AfterTime, p.AfterTime, p.AfterID}
	}
	tail = fmt.Sprintf(" ORDER BY %s %s, %s %s LIMIT %d", timeCol, dir, idCol, dir, p.Limit+1)
	return
}

// Info trims items to the limit and returns the page info. key returns the
// (time, id) of an item.
func PageResult[T any](p Page, items []T, key func(T) (int64, string)) ([]T, api.PageInfo) {
	info := api.PageInfo{Limit: p.Limit}
	if len(items) > p.Limit {
		items = items[:p.Limit]
		info.HasMore = true
		t, id := key(items[len(items)-1])
		info.NextCursor = encodeCursor(t, id)
	}
	if items == nil {
		items = []T{}
	}
	return items, info
}

func encodeCursor(t int64, id string) string {
	return strings.TrimRight(b64.EncodeToString([]byte(strconv.FormatInt(t, 10)+"|"+id)), "=")
}

func decodeCursor(s string) (int64, string, bool) {
	for len(s)%4 != 0 {
		s += "="
	}
	b, err := b64.DecodeString(s)
	if err != nil {
		return 0, "", false
	}
	ts, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return 0, "", false
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	return t, id, err == nil
}

// pageJSON renders PageInfo with nextCursor null when absent (Boat shape).
func pageJSON(pi api.PageInfo) map[string]any {
	var nc any
	if pi.NextCursor != "" {
		nc = pi.NextCursor
	}
	return map[string]any{"nextCursor": nc, "hasMore": pi.HasMore, "limit": pi.Limit}
}
