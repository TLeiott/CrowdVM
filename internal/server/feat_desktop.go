package server

// Desktop streaming over noVNC.
//
//	POST /sandboxes/{id}/desktop[?vnc=1][&theme=light|dark] {publicAccess?}
//	  → desktopUrl  = <public>/desktop/{id}[?theme=]#token=<t>   (dashboard viewer page)
//	    wsUrl       = ws://<public>/api/v1/sandboxes/{id}/desktop/ws  (raw RFB, subprotocols "binary" + "crowdvm-token.<t>")
//	POST /sandboxes/{id}/browser {profile?}  → browserUrl (kiosk Chrome on its own display, VNC :5902)
//	GET  /sandboxes/{id}/desktop/ws          websocket ↔ guest VNC (token, public access, or API auth)
//	GET|POST /sandboxes/{id}/desktop/{status,screenshot,input,recording,recording/start,recording/stop,wallpaper,start}
//	  → guest /v1/internal/desktop/* (CrowdVM extension; API key action "desktop")
//
// Tokens are HMACs bound to the sandbox, the view kind and the current boot
// (the guest token changes on every boot), valid for 10 minutes and for one
// connection (DESK-005: a reload needs a new URL); open streams keep running. The websocket
// takes the token only as a Sec-WebSocket-Protocol entry
// "crowdvm-token.<token>" next to "binary" (never in the URL, DESK-006).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/mux"
)

const (
	desktopTokenTTL  = 10 * time.Minute
	desktopVNCPort   = 5901
	browserVNCPort   = 5902
	wsTokenProtocol  = "crowdvm-token."
	defaultBrowserDP = "/var/lib/crowdvm/browser-profile"
)

func init() {
	RegisterFeature("desktop", func(s *Server) error {
		d := &desktop{s: s, used: map[string]time.Time{}}
		s.API("POST /sandboxes/{sandboxId}/desktop", "desktop", d.handleDesktop)
		s.API("POST /sandboxes/{sandboxId}/browser", "desktop", d.handleBrowser)
		s.API("GET /sandboxes/{sandboxId}/desktop/{rest...}", "desktop", d.handleDrive)
		s.API("POST /sandboxes/{sandboxId}/desktop/{rest...}", "desktop", d.handleDrive)
		s.Router().HandleFunc("GET /api/v1/sandboxes/{sandboxId}/desktop/ws", d.handleWS)
		return nil
	})
}

type desktop struct {
	s *Server

	mu   sync.Mutex
	used map[string]time.Time // spent token nonces -> expiry (DESK-005)
}

// claim spends a verified token: each one opens one connection (Boat: a
// reload needs a new URL). It reports false for a token used before.
// spent: tok was used already (no claim).
func (d *desktop) spent(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) < 3 {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.used[parts[2]]
	return ok
}

func (d *desktop) claim(tok string) bool {
	parts := strings.Split(tok, ".")
	exp, _ := strconv.ParseInt(parts[1], 10, 64)
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, t := range d.used {
		if now.After(t) {
			delete(d.used, k)
		}
	}
	if _, spent := d.used[parts[2]]; spent {
		return false
	}
	d.used[parts[2]] = time.Unix(exp+1, 0) // verify accepts through second exp
	return true
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

func (d *desktop) tokenSig(sb *Sandbox, kind string, exp int64, nonce string) string {
	return d.s.sign(fmt.Sprintf("desktop|%s|%s|%d|%s|%s", kind, sb.ID, exp, nonce, sb.GuestToken))
}

func (d *desktop) mint(sb *Sandbox, kind string) (string, time.Time) {
	exp := time.Now().Add(desktopTokenTTL)
	nonce := randHex(8)
	return fmt.Sprintf("%s.%d.%s.%s", kind, exp.Unix(), nonce, d.tokenSig(sb, kind, exp.Unix(), nonce)), exp
}

// verify returns the view kind of a valid token for sb's current boot.
func (d *desktop) verify(sb *Sandbox, tok string) (string, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 4 || sb.GuestToken == "" {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	if !tokenEq(parts[3], d.tokenSig(sb, parts[0], exp, parts[2])) {
		return "", false
	}
	return parts[0], parts[0] == "desktop" || parts[0] == "browser"
}

// publicBoot is the marker stored while the desktop is public (this boot only).
func publicBoot(sb *Sandbox) string { return hashToken("desktop-public|" + sb.GuestToken) }

func (d *desktop) isPublic(sb *Sandbox) bool {
	v, _ := sb.Extra["desktopPublic"].(string)
	return v != "" && sb.GuestToken != "" && v == publicBoot(sb)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (d *desktop) sandbox(c *Ctx) (*Sandbox, error) {
	sb, err := c.Sandbox("desktop", false)
	if err != nil {
		return nil, err
	}
	if d.s.accessOf(c.P, sb) == AccessView {
		return nil, Err(403, "sandbox_private", "This sandbox is private to its creator.")
	}
	return sb, nil
}

// nodeAPIError turns a node/guest error answer back into an API error.
func nodeAPIError(err error) error {
	var ne *NodeError
	if errors.As(err, &ne) {
		var e api.Error
		if json.Unmarshal([]byte(ne.Body), &e) == nil && e.Code != "" {
			st := e.Status
			if st == 0 {
				st = ne.Status
			}
			return api.NewError(st, e.Code, e.Message, e.Err.Details)
		}
		return ErrD(502, "sandbox_direct_failed", "The machine did not complete the call: "+ne.Error(), map[string]any{"retryable": true})
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae
	}
	return ErrD(502, "sandbox_direct_failed", "The machine did not complete the call: "+err.Error(), map[string]any{"retryable": true})
}

// guestDo calls guest /v1/<rest> of sb through its node.
func (d *desktop) guestDo(ctx context.Context, sb *Sandbox, method, rest string, in, out any) error {
	node, err := d.s.NodeForSandbox(sb)
	if err != nil {
		return err
	}
	if err := node.Do(ctx, method, "/v1/vms/"+sb.ID+"/guest/"+rest, in, out); err != nil {
		return nodeAPIError(err)
	}
	return nil
}

// startDesktop makes sure the guest's VNC desktop runs (idempotent).
func (d *desktop) startDesktop(ctx context.Context, sb *Sandbox) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out := map[string]any{}
	if err := d.guestDo(ctx, sb, "POST", "internal/desktop/start", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *desktop) wsBase() string {
	u := d.s.Cfg.PublicURL
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// urls returns the viewer page URL and the websocket URL for a token ("" = public).
func (d *desktop) urls(sb *Sandbox, view, tok string, q url.Values) (viewer, ws string) {
	if view == "browser" {
		q.Set("view", "browser")
	}
	viewer = d.s.Cfg.PublicURL + "/desktop/" + sb.ID
	if enc := q.Encode(); enc != "" {
		viewer += "?" + enc
	}
	ws = d.wsBase() + "/api/v1/sandboxes/" + sb.ID + "/desktop/ws" // token: subprotocol crowdvm-token.<t>
	if tok != "" {
		viewer += "#token=" + tok
	}
	return viewer, ws
}

func (d *desktop) machineIP(sb *Sandbox) any {
	if n, err := d.s.NodeByID(sb.NodeID); err == nil && n.Info.DirectHost != "" {
		return n.Info.DirectHost
	}
	return nil
}

// ---------------------------------------------------------------------------
// POST /sandboxes/{id}/desktop
// ---------------------------------------------------------------------------

func (d *desktop) handleDesktop(c *Ctx) error {
	vnc := c.Query("vnc") == "1" || c.Query("vnc") == "true"
	theme := c.Query("theme")
	if theme != "" && theme != "light" && theme != "dark" {
		return Err(400, "invalid_theme", "theme must be light or dark.")
	}
	var req struct {
		PublicAccess *bool `json:"publicAccess"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	sb, err := d.sandbox(c)
	if err != nil {
		return err
	}
	provisioning := func(msg string) error {
		return c.OK(200, "desktop.provisioning", map[string]any{"success": true, "provisioning": true, "desktopUrl": nil, "mode": "vnc", "message": msg})
	}
	if sb.starting() {
		if vnc {
			return provisioning("The sandbox is starting. Preparing the VNC desktop…")
		}
		return ErrD(409, "desktop_not_ready", "The sandbox is still starting. Retry in a few seconds.", map[string]any{"retryable": true, "retryAfterSeconds": 3})
	}
	if !sb.Usable() {
		return Err(409, "machine_not_running", "The desktop works only while the sandbox runs. Resume it first.")
	}
	info, err := d.startDesktop(c.R.Context(), sb)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Status >= 500 && vnc {
			return provisioning("Preparing the VNC desktop…")
		}
		return err
	}
	if req.PublicAccess != nil && vnc {
		var v any
		if *req.PublicAccess {
			v = publicBoot(sb)
		}
		if err := d.s.SetSandboxExtra(sb.ID, "desktopPublic", v); err != nil {
			return err
		}
		sb.Extra["desktopPublic"] = v
	}
	public := vnc && d.isPublic(sb)
	q := url.Values{}
	if theme != "" {
		q.Set("theme", theme)
	}
	tok, exp := "", time.Time{}
	if !public {
		tok, exp = d.mint(sb, "desktop")
	}
	viewer, ws := d.urls(sb, "desktop", tok, q)
	out := map[string]any{
		"success": true, "desktopUrl": viewer, "ip": d.machineIP(sb), "mode": "vnc", "provisioning": false, "message": "",
		// CrowdVM extensions
		"viewerUrl": viewer, "wsUrl": ws, "token": nz(tok), "publicAccess": public, "expiresAt": ts(exp),
		"width": info["width"], "height": info["height"],
	}
	if !public {
		out["expiresInSeconds"] = int(desktopTokenTTL.Seconds())
	}
	return c.OK(200, "desktop.url", out)
}

// ---------------------------------------------------------------------------
// POST /sandboxes/{id}/browser
// ---------------------------------------------------------------------------

var systemDirs = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32", "/proc", "/root", "/run",
	"/sbin", "/snap", "/sys", "/usr", "/var", "/tmp", "/home/user"}

func validProfile(p string) (string, error) {
	if p == "" {
		return defaultBrowserDP, nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", Err(400, "invalid_profile", "profile must be an absolute path in the sandbox.")
	}
	p = path.Clean(p)
	for _, d := range systemDirs {
		if p == "/" || p == d || strings.HasPrefix(p, d+"/") {
			return "", Err(400, "invalid_profile", "profile must be outside /home/user and system directories, e.g. /data/chrome-profile.")
		}
	}
	if strings.ContainsAny(p, "\x00\n'") {
		return "", Err(400, "invalid_profile", "profile contains characters that are not allowed.")
	}
	return p, nil
}

func (d *desktop) handleBrowser(c *Ctx) error {
	var req struct {
		Profile string `json:"profile"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if q := c.Query("profile"); q != "" && req.Profile == "" {
		req.Profile = q
	}
	profile, err := validProfile(req.Profile)
	if err != nil {
		return err
	}
	sb, err := d.sandbox(c)
	if err != nil {
		return err
	}
	if sb.starting() {
		return ErrD(503, "browser_not_ready", "The sandbox is still starting. Retry in a few seconds.", map[string]any{"retryable": true, "retryAfterSeconds": 3})
	}
	if !sb.Usable() {
		return Err(409, "machine_not_running", "The browser view works only while the sandbox runs. Resume it first.")
	}
	// The guest runs Xvnc :2 + Chrome as crowdvm-browser in a mount
	// namespace without /home/user (DESK-017), idempotently per profile.
	ctx, cancel := context.WithTimeout(c.R.Context(), 60*time.Second)
	defer cancel()
	var res struct {
		Isolated bool `json:"isolated"`
	}
	if err := d.guestDo(ctx, sb, "POST", "internal/browser/start", map[string]any{"profile": profile, "width": 1920, "height": 1080}, &res); err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && ae.Status >= 500 {
			return ErrD(503, "browser_not_ready", "The browser view did not start: "+ae.Message, map[string]any{"retryable": true})
		}
		return err
	}
	tok, exp := d.mint(sb, "browser")
	viewer, ws := d.urls(sb, "browser", tok, url.Values{})
	return c.OK(200, "browser.url", map[string]any{"success": true, "browserUrl": viewer, "viewerUrl": viewer, "wsUrl": ws,
		"token": tok, "profile": profile, "isolated": res.Isolated, "expiresAt": ts(exp), "expiresInSeconds": int(desktopTokenTTL.Seconds()), "mode": "vnc"})
}

// ---------------------------------------------------------------------------
// Driving, recording, screenshots, wallpaper (guest pass-through)
// ---------------------------------------------------------------------------

var driveRoutes = map[string]string{
	"GET status": "", "GET screenshot": "start", "POST input": "start", "GET recording": "",
	"POST recording/start": "start", "POST recording/stop": "", "POST wallpaper": "start", "POST start": "",
}

func (d *desktop) handleDrive(c *Ctx) error {
	rest := strings.Trim(c.Param("rest"), "/")
	need, ok := driveRoutes[c.R.Method+" "+rest]
	if !ok {
		return Err(404, "not_found", "Unknown desktop route. Use status, screenshot, input, recording, recording/start, recording/stop, wallpaper or start.")
	}
	sb, err := d.sandbox(c)
	if err != nil {
		return err
	}
	if sb, err = d.s.guestReady(c.R.Context(), sb); err != nil {
		return err
	}
	if need == "start" {
		if _, err := d.startDesktop(c.R.Context(), sb); err != nil {
			return err
		}
	}
	d.s.ProxyToGuest(c.W, c.R, sb, "internal/desktop/"+rest)
	return nil
}

// ---------------------------------------------------------------------------
// GET /sandboxes/{id}/desktop/ws
// ---------------------------------------------------------------------------

func (d *desktop) handleWS(w http.ResponseWriter, r *http.Request) {
	sb, err := d.s.SandboxByID(r.PathValue("sandboxId"))
	if err != nil || sb.DeletedAt != nil {
		api.WriteError(w, errNotFound("Sandbox"))
		return
	}
	tok := ""
	for _, p := range websocketProtocols(r) {
		if t, ok := strings.CutPrefix(p, wsTokenProtocol); ok && tok == "" {
			tok = t
		}
	}
	kind, anyOrigin := "desktop", true
	switch {
	case tok != "":
		k, ok := d.verify(sb, tok)
		if !ok {
			api.WriteError(w, Err(401, "invalid_desktop_token", "The desktop token is invalid or expired. Ask for a new desktop URL."))
			return
		}
		kind = k
	case d.isPublic(sb):
	default:
		p, err := d.s.authenticate(w, r)
		if err == nil {
			err = d.s.checkAction(p, r, "desktop")
		}
		if err == nil {
			err = d.s.authorizeSandbox(p, sb, "desktop", false)
		}
		if err == nil && d.s.accessOf(p, sb) == AccessView {
			err = Err(403, "sandbox_private", "This sandbox is private to its creator.")
		}
		if err != nil {
			api.WriteError(w, toAPIError(err))
			return
		}
		anyOrigin = false // cookie sessions: same-origin only
	}
	if !sb.Usable() {
		api.WriteError(w, Err(409, "desktop_not_ready", "The sandbox is not running."))
		return
	}
	port := browserVNCPort
	if kind == "desktop" {
		port = desktopVNCPort
		info, err := d.startDesktop(r.Context(), sb)
		if err != nil {
			api.WriteError(w, toAPIError(err))
			return
		}
		if p, ok := info["port"].(float64); ok && p > 0 {
			port = int(p)
		}
	}
	// A spent token is refused before any VNC connection opens: each one
	// closed before its handshake counts toward Xvnc's blacklist of
	// 127.0.0.1, which locks every desktop user out (bugs-r5-opus 3).
	if tok != "" && d.spent(tok) {
		api.WriteError(w, Err(401, "desktop_token_used", "This desktop token was used already; each URL opens one session. Ask for a new desktop URL."))
		return
	}
	dctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	tc, err := d.s.DialSandbox(dctx, sb, port)
	cancel()
	if err != nil {
		api.WriteError(w, toAPIError(err))
		return
	}
	// Spend the token only for a connection that will open (a failed
	// attempt can be retried with the same URL).
	if tok != "" && !d.claim(tok) {
		tc.Close()
		api.WriteError(w, Err(401, "desktop_token_used", "This desktop token was used already; each URL opens one session. Ask for a new desktop URL."))
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}, InsecureSkipVerify: anyOrigin})
	if err != nil {
		tc.Close()
		return
	}
	ws.SetReadLimit(-1)
	mux.Join(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), tc)
}

func websocketProtocols(r *http.Request) []string {
	var out []string
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}
