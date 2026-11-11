package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// guestPrefixes are the sandbox sub-paths served by the guest agent.
var guestPrefixes = []string{"commands", "command-history", "processes", "files", "artifacts", "prompt", "prompts",
	"events", "conversations", "steer", "interrupt", "sshkey"}

// guestAction maps a guest sub-path and method to the scope action.
func guestAction(rest, method string) string {
	first, _, _ := strings.Cut(rest, "/")
	switch first {
	case "commands":
		return "exec"
	case "files":
		if method == http.MethodGet || method == http.MethodHead {
			return "file.read"
		}
		return "file.write"
	case "artifacts":
		return "file.read"
	case "prompt", "steer", "interrupt":
		return "agent.prompt"
	case "sshkey":
		return "ssh"
	}
	return "sandbox.read" // command-history, processes, prompts, events, conversations
}

func (s *Server) passthroughRoutes() {
	for _, p := range guestPrefixes {
		p := p
		h := s.wrap(ActionAny, func(c *Ctx) error {
			rest := p
			if more := c.R.PathValue("rest"); more != "" {
				rest += "/" + more
			}
			return s.handleGuest(c, rest)
		})
		s.router.Handle("/api/v1/sandboxes/{sandboxId}/"+p, h)
		s.router.Handle("/api/v1/sandboxes/{sandboxId}/"+p+"/{rest...}", h)
	}
	s.API("POST /sandboxes/{sandboxId}/files/download-url", "file.read", func(c *Ctx) error { return s.handleFileURL(c, "download") })
	s.API("POST /sandboxes/{sandboxId}/files/upload-url", "file.write", func(c *Ctx) error { return s.handleFileURL(c, "upload") })
	s.router.HandleFunc("/api/v1/sandboxes/{sandboxId}/files/signed", s.handleSignedFile)
	s.API("GET /sandboxes/{sandboxId}/tunnel", "ssh", s.handleTunnel)
}

// MinGuestVersion is the oldest guest agent whose API this server speaks
// (default of Config.MinGuestVersion). Guests ship with every boot, so an
// outdated one means its node carries an old crowdvm-guest binary.
var MinGuestVersion = "0.1.0"

// guestVersion is the guest agent version the node reported for sb ("" = unknown).
func (s *Server) guestVersion(sb *Sandbox) string {
	c := s.nodes.get(sb.NodeID)
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vms[sb.ID].GuestVersion
}

// checkGuestVersion answers 409 agent_outdated (CMD-012) for a guest older
// than Config.MinGuestVersion. Unknown and non-release versions ("dev")
// pass.
func (s *Server) checkGuestVersion(sb *Sandbox) error {
	v := s.guestVersion(sb)
	have, ok1 := parseSemver(v)
	want, ok2 := parseSemver(s.Cfg.MinGuestVersion)
	if !ok1 || !ok2 || !semverLess(have, want) {
		return nil
	}
	return ErrD(409, "agent_outdated", "The sandbox runs guest agent "+v+", older than "+s.Cfg.MinGuestVersion+" which this server needs. Stop and resume it to boot the current agent (its node may need `crowdvm-node update`).",
		map[string]any{"retryable": false, "guestVersion": v, "minGuestVersion": s.Cfg.MinGuestVersion})
}

// parseSemver reads "v1.2.3" / "1.2" / "1.2.3-rc1" (pre-release ignored).
func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "-")
	v, _, _ = strings.Cut(v, "+")
	parts := strings.Split(v, ".")
	if v == "" || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func semverLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// restoringErr (API-126): the call did not run because the sandbox's
// machine is being restored (its node went offline, it is rebuilt on
// another node or recovered); retry in 1-3 s.
func restoringErr(sb *Sandbox) error {
	return ErrD(502, "sandbox_direct_failed", "The sandbox is being restored ("+firstNonEmpty(sb.HealthReason, sb.Error, sb.State)+"); the call did not run. Retry in a few seconds.",
		map[string]any{"retryable": true, "message": "sandbox_restoring", "retryAfterSeconds": 2, "state": sb.State})
}

// restoring reports whether sb's machine is being brought back.
func (sb *Sandbox) restoring() bool {
	return sb.HealthReason == "rebuilding" || sb.HealthReason == "recovering" || (sb.State == api.StateError && sb.Error == "node_offline")
}

// guestReady loads the sandbox for a guest call: it waits while the sandbox
// starts and maps other states to Boat's errors.
func (s *Server) guestReady(ctx context.Context, sb *Sandbox) (*Sandbox, error) {
	if sb.starting() {
		var err error
		if sb, err = s.WaitUsable(ctx, sb.ID, s.Cfg.StartWait); err != nil {
			if ctx.Err() != nil {
				return nil, Err(499, "client_closed", "The client went away.")
			}
			return nil, err
		}
	}
	switch {
	case sb.Usable():
		if err := s.checkGuestVersion(sb); err != nil {
			return nil, err
		}
		return sb, nil
	case sb.starting() && isSecuring(sb):
		return nil, securingErr()
	case sb.restoring():
		return nil, restoringErr(sb)
	case sb.starting():
		return nil, ErrD(409, "sandbox_starting", "The sandbox is still starting. Retry in a few seconds.", map[string]any{"retryable": true, "retryAfterSeconds": 3, "state": sb.State})
	case sb.State == api.StateArchived || sb.State == api.StateArchiving:
		return nil, ErrD(409, "machine_not_running", "The sandbox has no running machine. Resume it first: crowdvm resume "+sb.ID, map[string]any{"retryable": false, "state": sb.State})
	default:
		return nil, ErrD(409, "sandbox_not_ready", "The sandbox is in state "+sb.State+". Resume or recover it first: crowdvm resume "+sb.ID+" (or crowdvm recover "+sb.ID+")", map[string]any{"retryable": false, "state": sb.State})
	}
}

func (s *Server) handleGuest(c *Ctx, rest string) error {
	action := guestAction(rest, c.R.Method)
	if err := s.checkAction(c.P, c.R, action); err != nil {
		return err
	}
	sb, err := c.Sandbox(action, false)
	if err != nil {
		return err
	}
	if s.accessOf(c.P, sb) == AccessView {
		return Err(403, "sandbox_private", "This sandbox is private to its creator.")
	}
	// History, processes and agent events of a sandbox that does not run
	// come from the coordinator's copy (feat_history.go).
	if (c.R.Method == http.MethodGet || c.R.Method == http.MethodHead) && !sb.Usable() && storedRoute(rest) {
		return s.serveStored(c, sb, rest)
	}
	// Teammates read command history from the server's copy, which knows
	// what was recorded while the sandbox was private.
	if (c.R.Method == http.MethodGet || c.R.Method == http.MethodHead) && strings.HasPrefix(rest, "command-history") && historyTeammate(c, sb) {
		fctx, cancel := context.WithTimeout(c.R.Context(), 5*time.Second)
		s.historyFlush(fctx, sb)
		cancel()
		return s.serveStored(c, sb, rest)
	}
	if c.R.Method == http.MethodPost && rest == "commands" {
		if err := rejectCommandEnv(c.R); err != nil {
			return err
		}
	}
	stopped := sb
	if sb, err = s.guestReady(c.R.Context(), sb); err != nil {
		var ae *api.Error
		if rest == "steer" && errors.As(err, &ae) && (ae.Code == "machine_not_running" || ae.Code == "sandbox_not_ready") {
			// Boat: a steer needs a running turn; a stopped sandbox has none.
			return ErrD(409, "no_running_turn", "The sandbox is not running, so no turn is in flight. Resume it (crowdvm resume "+stopped.ID+"), then send a prompt.",
				map[string]any{"reason": "not_running", "state": stopped.State, "retryable": false})
		}
		return err
	}
	s.onSandboxUsed(sb)
	s.ProxyToGuest(c.W, c.R, sb, rest)
	return nil
}

// rejectCommandEnv (LIFE-048): Boat commands have no per-call env field.
// The guest still accepts one for the coordinator's own calls (CI runner),
// so the public API refuses it instead of passing it on.
func rejectCommandEnv(r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20+1))
	if err != nil {
		return Err(400, "invalid_json", "Could not read the request body.")
	}
	if len(body) > 16<<20 {
		return Err(413, "request_too_large", "A command request is at most 16 MiB.")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) == nil {
		for k := range m { // the guest decodes field names case-insensitively
			if strings.EqualFold(k, "env") {
				return ErrD(400, "invalid_request", "POST /commands has no env field: prefix the command (export KEY=value; ...) or give the sandbox env vars at create or resume (env: {...}).",
					map[string]any{"field": k})
			}
		}
	}
	return nil
}

// ProxyToGuest reverse-proxies r to the guest agent of sb at /v1/<rest>
// (through the node), streaming the answer with immediate flushes.
func (s *Server) ProxyToGuest(w http.ResponseWriter, r *http.Request, sb *Sandbox, rest string) {
	node := s.nodes.get(sb.NodeID)
	if node == nil {
		api.WriteError(w, ErrD(502, "sandbox_direct_failed", "The node hosting this sandbox is offline; the call did not run. CrowdVM restores the sandbox when the node is back or on another node.",
			map[string]any{"retryable": true, "message": "sandbox_restoring", "retryAfterSeconds": 2}))
		return
	}
	isPrompt := r.Method == http.MethodPost && rest == "prompt"
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "node"
			pr.Out.URL.Path = "/v1/vms/" + sb.ID + "/guest/" + rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = "node"
			for _, h := range []string{"Authorization", "Cookie", CSRFHeader, "X-CrowdVM-Org", "X-Boat-Org"} {
				pr.Out.Header.Del(h)
			}
			pr.Out.Header.Set("X-Request-Id", w.Header().Get("X-Request-Id"))
		},
		Transport:     node.Transport(),
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("X-Request-Id")
			guardGuestContent(resp.Header)
			if isPrompt && resp.StatusCode/100 == 2 {
				s.setAgentState(sb.ID, true)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			retryable := r.Method != http.MethodPost || strings.HasPrefix(rest, "files")
			api.WriteError(w, ErrD(502, "sandbox_direct_failed", "The machine did not complete the call: "+err.Error(), map[string]any{"retryable": retryable, "message": err.Error()}))
		},
	}
	rp.ServeHTTP(w, r)
}

// guardGuestContent: anything a sandbox returns is served on the server's
// origin, next to the dashboard and its session cookie. It never runs there
// as a page: no sniffing, a sandboxed (opaque-origin) document, and active
// types (HTML, SVG, XML, scripts) only as downloads (bugs-r8-sonnet 1).
func guardGuestContent(h http.Header) {
	// Nor touch the origin's cookies or storage (bugs-r9-sonnet).
	h.Del("Set-Cookie")
	dropOriginHeaders(h) // bugs-r10-opus 4 (adds NEL, Report-To)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	ct := strings.ToLower(h.Get("Content-Type"))
	for _, active := range []string{"html", "svg", "xml", "javascript", "ecmascript", "xsl"} {
		if strings.Contains(ct, active) {
			h.Set("Content-Type", "application/octet-stream")
			if h.Get("Content-Disposition") == "" || strings.HasPrefix(strings.ToLower(h.Get("Content-Disposition")), "inline") {
				h.Set("Content-Disposition", "attachment")
			}
			break
		}
	}
}

// ---------------------------------------------------------------------------
// Signed file URLs
// ---------------------------------------------------------------------------

func guestPath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if !strings.HasPrefix(p, "/") {
		p = path.Join(proto.GuestHome, p)
	}
	p = path.Clean(p)
	if p == proto.GuestHome || strings.HasPrefix(p, proto.GuestHome+"/") || p == "/tmp" || strings.HasPrefix(p, "/tmp/") {
		return p, true
	}
	return "", false
}

func (s *Server) handleFileURL(c *Ctx, op string) error {
	var req struct {
		Path       string `json:"path"`
		TTLSeconds int    `json:"ttlSeconds"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	sb, err := c.Sandbox(map[string]string{"download": "file.read", "upload": "file.write"}[op], false)
	if err != nil {
		return err
	}
	if s.accessOf(c.P, sb) == AccessView {
		return Err(403, "sandbox_private", "This sandbox is private to its creator.")
	}
	p, ok := guestPath(req.Path)
	if !ok {
		return Err(400, "invalid_path", "path must resolve under /home/user or /tmp.")
	}
	ttl := req.TTLSeconds
	if ttl == 0 {
		ttl = 900
	}
	if ttl < 1 || ttl > 3600 {
		return Err(400, "invalid_ttl", "ttlSeconds must be 1 to 3600.")
	}
	exp := time.Now().Add(time.Duration(ttl) * time.Second).Unix()
	q := url.Values{"op": {op}, "path": {p}, "exp": {strconv.FormatInt(exp, 10)}}
	q.Set("sig", s.sign(fmt.Sprintf("file|%s|%s|%s|%d", sb.ID, op, p, exp)))
	method, typ := http.MethodGet, "file.download_url"
	if op == "upload" {
		method, typ = http.MethodPut, "file.upload_url"
	}
	return c.OK(200, typ, map[string]any{"url": s.Cfg.PublicURL + "/api/v1/sandboxes/" + sb.ID + "/files/signed?" + q.Encode(),
		"method": method, "path": p, "expiresAt": ts(time.Unix(exp, 0))})
}

func (s *Server) handleSignedFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, op, p := r.PathValue("sandboxId"), q.Get("op"), q.Get("path")
	exp, _ := strconv.ParseInt(q.Get("exp"), 10, 64)
	if !hmac.Equal([]byte(q.Get("sig")), []byte(s.sign(fmt.Sprintf("file|%s|%s|%s|%d", id, op, p, exp)))) {
		api.WriteError(w, Err(403, "invalid_signature", "The URL signature is not valid."))
		return
	}
	if time.Now().Unix() > exp {
		api.WriteError(w, Err(403, "url_expired", "The URL has expired."))
		return
	}
	if (op == "download" && r.Method != http.MethodGet && r.Method != http.MethodHead) || (op == "upload" && r.Method != http.MethodPut) {
		api.WriteError(w, Err(405, "method_not_allowed", "Use GET for download URLs and PUT for upload URLs."))
		return
	}
	sb, err := s.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil {
		api.WriteError(w, errNotFound("Sandbox"))
		return
	}
	if sb, err = s.guestReady(r.Context(), sb); err != nil {
		api.WriteError(w, toAPIError(err))
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = url.Values{"path": {p}}.Encode()
	s.ProxyToGuest(w, r2, sb, "files/raw")
}

func toAPIError(err error) *api.Error {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae
	}
	return Err(500, "internal_error", err.Error())
}

// ---------------------------------------------------------------------------
// Tunnel: websocket (or HTTP Upgrade crowdvm-tunnel) carrying raw TCP
// ---------------------------------------------------------------------------

// DialSandbox opens a raw TCP connection to port inside the sandbox.
func (s *Server) DialSandbox(ctx context.Context, sb *Sandbox, port int) (net.Conn, error) {
	node, err := s.NodeForSandbox(sb)
	if err != nil {
		return nil, err
	}
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := node.DialTunnel(sb.ID, port)
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, ErrD(502, "sandbox_direct_failed", "Could not open the tunnel: "+r.err.Error(), map[string]any{"retryable": true, "message": r.err.Error()})
		}
		return r.c, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (s *Server) handleTunnel(c *Ctx) error {
	port, err := strconv.Atoi(c.Query("port"))
	if err != nil || port < 1 || port > 65535 {
		return Err(400, "invalid_port", "port must be 1 to 65535.")
	}
	sb, err := c.Sandbox("ssh", false)
	if err != nil {
		return err
	}
	if s.accessOf(c.P, sb) == AccessView {
		return Err(403, "sandbox_private", "This sandbox is private to its creator.")
	}
	if sb, err = s.guestReady(c.R.Context(), sb); err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(c.R.Context(), 30*time.Second)
	tc, err := s.DialSandbox(dctx, sb, port)
	cancel()
	if err != nil {
		return err
	}
	upgrade := strings.ToLower(c.R.Header.Get("Upgrade"))
	switch {
	case upgrade == "websocket":
		ws, err := websocket.Accept(c.W, c.R, &websocket.AcceptOptions{})
		if err != nil {
			tc.Close()
			return nil
		}
		ws.SetReadLimit(-1)
		nc := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
		mux.Join(nc, tc)
	case upgrade == mux.UpgradeProto:
		hc, rw, err := mux.HijackUpgrade(c.W)
		if err != nil {
			tc.Close()
			return nil
		}
		mux.Join(&readerConn{Conn: hc, r: rw.Reader}, tc)
	default:
		tc.Close()
		return Err(400, "upgrade_required", "Open this endpoint as a websocket (or with Upgrade: "+mux.UpgradeProto+").")
	}
	return nil
}

// readerConn reads through a buffered reader (bytes read ahead by the
// HTTP server) and writes to the conn.
type readerConn struct {
	net.Conn
	r io.Reader
}

func (c *readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }
