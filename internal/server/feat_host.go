package server

// Public URLs ("host"): a stable URL per sandbox port, served by the
// coordinator and reverse-proxied over the node tunnel into the guest.
//
//	http://<label>-<port>.<public-domain>[:<server port>]/   host routing
//	http://<server>/p/<sandboxId>/<port>/                     path routing
//
// <label> is the sandbox's subdomain, or its id with "_" → "-". Protected
// ports need ?_token= (answered with a redirect that sets the _port_auth
// cookie) or Cookie: _port_auth=. Optional per-port CIDR allow/deny lists
// (CrowdVM extension) are checked first. Path-routed answers carry a CSP
// sandbox so a hosted app cannot use the dashboard's origin.

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

const (
	maxHostedPorts = 50
	portAuthCookie = "_port_auth"
	portTokenParam = "_token"
	maxCIDRs       = 50
	// pathSandboxCSP isolates path-routed apps from the dashboard origin.
	pathSandboxCSP = "sandbox allow-scripts allow-forms allow-popups allow-popups-to-escape-sandbox allow-modals allow-downloads allow-pointer-lock allow-presentation"
)

// reservedPorts are used by CrowdVM itself inside the guest (URL-008).
var reservedPorts = []int{22, 5900, 5901, 5902, 6080, 7070}

func init() {
	store.Register("hosting", store.Migration{Version: 1, SQL: `
CREATE TABLE hosted_ports (
	sandbox_id  TEXT NOT NULL,
	port        INTEGER NOT NULL,
	title       TEXT NOT NULL DEFAULT '',
	public      INTEGER NOT NULL DEFAULT 0,
	localhost   INTEGER NOT NULL DEFAULT 0,
	token       TEXT,
	hidden      INTEGER NOT NULL DEFAULT 0,
	allow_cidrs TEXT NOT NULL DEFAULT '[]',
	deny_cidrs  TEXT NOT NULL DEFAULT '[]',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	PRIMARY KEY (sandbox_id, port)
);`})
	RegisterFeature("hosting", func(s *Server) error { return newHosting(s).setup() })
}

type hosting struct {
	s          *Server
	scheme     string // of PublicURL
	hostPort   string // ":8420" of PublicURL ("" for the default port)
	domain     string // wildcard domain for host routing
	hostRouted bool   // url = host-routed URL (else the path form)
	tr         *http.Transport

	liveMu sync.Mutex
	live   map[string]map[*context.CancelFunc]bool // "sandbox:port" -> proxied requests (websockets) in flight
}

// cut closes every proxied request and websocket of a port: hiding it or
// hosting it again (a new token, other access) ends existing connections
// (bugs-r4-opus 5; Boat: "closes its open connections").
func (h *hosting) cut(sbID string, port int) {
	h.liveMu.Lock()
	defer h.liveMu.Unlock()
	for c := range h.live[fmt.Sprintf("%s:%d", sbID, port)] {
		(*c)()
	}
}

// track registers a proxied request of a port; the returned func ends it.
func (h *hosting) track(ctx context.Context, sbID string, port int) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	key := fmt.Sprintf("%s:%d", sbID, port)
	h.liveMu.Lock()
	if h.live == nil {
		h.live = map[string]map[*context.CancelFunc]bool{}
	}
	if h.live[key] == nil {
		h.live[key] = map[*context.CancelFunc]bool{}
	}
	h.live[key][&cancel] = true
	h.liveMu.Unlock()
	return ctx, func() {
		cancel()
		h.liveMu.Lock()
		delete(h.live[key], &cancel)
		if len(h.live[key]) == 0 {
			delete(h.live, key)
		}
		h.liveMu.Unlock()
	}
}

func newHosting(s *Server) *hosting {
	h := &hosting{s: s, scheme: "http"}
	if u, err := url.Parse(s.Cfg.PublicURL); err == nil {
		if u.Scheme != "" {
			h.scheme = u.Scheme
		}
		if p := u.Port(); p != "" && !(u.Scheme == "http" && p == "80") && !(u.Scheme == "https" && p == "443") {
			h.hostPort = ":" + p
		}
	}
	h.domain, h.hostRouted = publicDomain(s.Cfg)
	h.tr = &http.Transport{
		DialContext:         h.dial,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}
	return h
}

// publicDomain returns the host-routing domain and whether it is reachable
// from other machines (an explicit domain or <ip>.nip.io).
func publicDomain(cfg Config) (string, bool) {
	if d := strings.Trim(strings.ToLower(cfg.PublicDomain), "."); d != "" {
		return d, d != "localhost"
	}
	if u, err := url.Parse(cfg.PublicURL); err == nil {
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return ip.String() + ".nip.io", true
		}
	}
	return "localhost", false
}

func (h *hosting) setup() error {
	s := h.s
	s.API("POST /sandboxes/{sandboxId}/host", "host", h.handleHost)
	s.API("GET /sandboxes/{sandboxId}/host", "host", h.handleList)
	s.API("GET /sandboxes/{sandboxId}/host/{port}", "host", h.handleGetPort)
	s.API("DELETE /sandboxes/{sandboxId}/host/{port}", "host", h.handleHide)
	s.Router().HandleFunc("/p/{sandboxId}/{port}", h.handlePath)
	s.Router().HandleFunc("/p/{sandboxId}/{port}/{rest...}", h.handlePath)
	s.Router().Intercept(h.intercept)
	s.Hooks.HostPorts = h.createHook
	s.Hooks.SandboxJSON = append(s.Hooks.SandboxJSON, func(sb *Sandbox, m map[string]any) {
		m["subdomain"] = hostLabel(sb)
	})
	s.Bus.Subscribe(func(e Event) {
		if e.Type == EvSandboxDeleted && e.SandboxID != "" {
			_, _ = s.DB.Exec(`DELETE FROM hosted_ports WHERE sandbox_id=?`, e.SandboxID)
		}
	})
	return nil
}

// hostLabel is the DNS label of a sandbox's URLs.
func hostLabel(sb *Sandbox) string {
	if sb.Subdomain != "" {
		return sb.Subdomain
	}
	return strings.ReplaceAll(sb.ID, "_", "-")
}

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

type hostedPort struct {
	SandboxID, Title, Token   string
	Port                      int
	Public, Localhost, Hidden bool
	Allow, Deny               []string
	CreatedAt, UpdatedAt      time.Time
}

func (h *hosting) load(sbID string, port int) (*hostedPort, error) {
	return scanHosted(h.s.DB.QueryRow(`SELECT sandbox_id,port,title,public,localhost,COALESCE(token,''),hidden,allow_cidrs,deny_cidrs,created_at,updated_at
		FROM hosted_ports WHERE sandbox_id=? AND port=?`, sbID, port))
}

func scanHosted(sc interface{ Scan(...any) error }) (*hostedPort, error) {
	hp := &hostedPort{}
	var allow, deny string
	var c, u int64
	if err := sc.Scan(&hp.SandboxID, &hp.Port, &hp.Title, &hp.Public, &hp.Localhost, &hp.Token, &hp.Hidden, &allow, &deny, &c, &u); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(allow), &hp.Allow)
	_ = json.Unmarshal([]byte(deny), &hp.Deny)
	hp.CreatedAt, hp.UpdatedAt = store.Time(c), store.Time(u)
	return hp, nil
}

func (h *hosting) list(sbID string, all bool) ([]*hostedPort, error) {
	q := `SELECT sandbox_id,port,title,public,localhost,COALESCE(token,''),hidden,allow_cidrs,deny_cidrs,created_at,updated_at FROM hosted_ports WHERE sandbox_id=?`
	if !all {
		q += ` AND hidden=0`
	}
	rows, err := h.s.DB.Query(q+` ORDER BY port`, sbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*hostedPort
	for rows.Next() {
		hp, err := scanHosted(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, hp)
	}
	return out, rows.Err()
}

// hostReq is the body of POST /sandboxes/{id}/host and one hostedPorts entry.
type hostReq struct {
	Port      int     `json:"port"`
	Public    bool    `json:"public"`
	Localhost bool    `json:"localhost"`
	Title     *string `json:"title"`
	// Firewall (CrowdVM extension): client CIDRs allowed / denied. Absent =
	// keep the current lists; {} clears them.
	Firewall *struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	} `json:"firewall"`
}

func (r *hostReq) validate() error {
	if r.Port < 1 || r.Port > 65535 {
		return Err(400, "invalid_port", "port must be 1 to 65535.")
	}
	if slices.Contains(reservedPorts, r.Port) {
		return ErrD(400, "reserved_port", fmt.Sprintf("Port %d is used by CrowdVM inside the sandbox. Run your app on another port.", r.Port),
			map[string]any{"reservedPorts": reservedPorts})
	}
	if r.Title != nil && len(*r.Title) > 200 {
		return Err(400, "invalid_title", "title has at most 200 characters.")
	}
	if r.Firewall != nil {
		for _, l := range [][]string{r.Firewall.Allow, r.Firewall.Deny} {
			if len(l) > maxCIDRs {
				return Err(400, "invalid_firewall", fmt.Sprintf("At most %d CIDRs per list.", maxCIDRs))
			}
			for _, c := range l {
				if _, ok := parseCIDR(c); !ok {
					return Err(400, "invalid_firewall", "Not an IP address or CIDR: "+c)
				}
			}
		}
	}
	return nil
}

func parseCIDR(c string) (*net.IPNet, bool) {
	c = strings.TrimSpace(c)
	if !strings.Contains(c, "/") {
		ip := net.ParseIP(c)
		if ip == nil {
			return nil, false
		}
		bits := 128
		if ip.To4() != nil {
			ip, bits = ip.To4(), 32
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, true
	}
	_, n, err := net.ParseCIDR(c)
	return n, err == nil
}

func cidrsMatch(list []string, ip net.IP) bool {
	for _, c := range list {
		if n, ok := parseCIDR(c); ok && n.Contains(ip) {
			return true
		}
	}
	return false
}

// host registers or updates a hosted port (Boat semantics: same URL and
// token on re-run; public clears the token; a later protected call mints a
// new one; every call sets localhost again and un-hides the port).
func (h *hosting) host(sb *Sandbox, req hostReq) (*hostedPort, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	old, err := h.load(sb.ID, req.Port)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if old == nil {
		var n int
		_ = h.s.DB.QueryRow(`SELECT COUNT(*) FROM hosted_ports WHERE sandbox_id=?`, sb.ID).Scan(&n)
		if n >= maxHostedPorts {
			return nil, Err(409, "hosted_port_limit", fmt.Sprintf("A sandbox can host up to %d ports. Remove one first.", maxHostedPorts))
		}
		old = &hostedPort{SandboxID: sb.ID, Port: req.Port, Allow: []string{}, Deny: []string{}}
	}
	hp := *old
	hp.Public, hp.Localhost, hp.Hidden = req.Public, req.Localhost, false
	if req.Public {
		hp.Token = ""
	} else if hp.Token == "" {
		hp.Token = randToken(24)
	}
	if req.Title != nil {
		hp.Title = strings.TrimSpace(*req.Title)
	}
	if req.Firewall != nil {
		hp.Allow, hp.Deny = nonNil(req.Firewall.Allow), nonNil(req.Firewall.Deny)
	}
	now := store.Now()
	var tok any
	if hp.Token != "" {
		tok = hp.Token
	}
	_, err = h.s.DB.Exec(`INSERT INTO hosted_ports(sandbox_id,port,title,public,localhost,token,hidden,allow_cidrs,deny_cidrs,created_at,updated_at)
		VALUES(?,?,?,?,?,?,0,?,?,?,?)
		ON CONFLICT(sandbox_id,port) DO UPDATE SET title=excluded.title, public=excluded.public, localhost=excluded.localhost,
		token=excluded.token, hidden=0, allow_cidrs=excluded.allow_cidrs, deny_cidrs=excluded.deny_cidrs, updated_at=excluded.updated_at`,
		sb.ID, hp.Port, hp.Title, boolInt(hp.Public), boolInt(hp.Localhost), tok, mustJSON(hp.Allow), mustJSON(hp.Deny), now, now)
	if err != nil {
		return nil, err
	}
	h.cut(sb.ID, req.Port)
	return h.load(sb.ID, req.Port)
}

func nonNil(l []string) []string {
	out := []string{}
	for _, c := range l {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// URLs and JSON
// ---------------------------------------------------------------------------

func (h *hosting) hostURL(sb *Sandbox, port int) string {
	return fmt.Sprintf("%s://%s-%d.%s%s/", h.scheme, hostLabel(sb), port, h.domain, h.hostPort)
}

func (h *hosting) pathURL(sb *Sandbox, port int) string {
	return fmt.Sprintf("%s/p/%s/%d/", h.s.Cfg.PublicURL, sb.ID, port)
}

func withToken(u, tok string) string {
	if tok == "" {
		return u
	}
	return u + "?" + portTokenParam + "=" + url.QueryEscape(tok)
}

// directEndpoint is the node's raw TCP endpoint of a public port (URL-012).
func directEndpoint(sb *Sandbox, hp *hostedPort) any {
	if v := sb.Direct["tcp/"+strconv.Itoa(hp.Port)]; v != "" && hp.Public && !hp.Hidden && sb.Usable() {
		return v
	}
	return nil
}

func (h *hosting) portJSON(sb *Sandbox, hp *hostedPort) map[string]any {
	hu, pu := withToken(h.hostURL(sb, hp.Port), hp.Token), withToken(h.pathURL(sb, hp.Port), hp.Token)
	u := pu
	if h.hostRouted {
		u = hu
	}
	access := "private"
	if hp.Public {
		access = "public"
	}
	return map[string]any{
		"success": true, "sandboxId": sb.ID, "port": hp.Port, "url": u, "access": access, "isProtected": !hp.Public,
		"localhost": hp.Localhost, "title": nz(hp.Title),
		// CrowdVM extensions
		"hostUrl": hu, "pathUrl": pu, "hidden": hp.Hidden, "firewall": map[string]any{"allow": nonNil(hp.Allow), "deny": nonNil(hp.Deny)},
		"directEndpoint": directEndpoint(sb, hp),
		"createdAt":      ts(hp.CreatedAt), "updatedAt": ts(hp.UpdatedAt),
	}
}

// ---------------------------------------------------------------------------
// API
// ---------------------------------------------------------------------------

func (h *hosting) sandbox(c *Ctx) (*Sandbox, error) {
	sb, err := c.Sandbox("host", false)
	if err != nil {
		return nil, err
	}
	if h.s.accessOf(c.P, sb) == AccessView {
		return nil, Err(403, "sandbox_private", "This sandbox is private to its creator.")
	}
	return sb, nil
}

func (h *hosting) handleHost(c *Ctx) error {
	var req hostReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	sb, err := h.sandbox(c)
	if err != nil {
		return err
	}
	if !sb.Active() {
		return Err(409, "machine_not_running", "The sandbox is not running. Resume it first, then host the port.")
	}
	hp, err := h.host(sb, req)
	if err != nil {
		return err
	}
	// URL-012: a public port is also published raw on the node's direct
	// host (best effort: nodes without one publish nothing).
	if err := h.s.syncDirect(c.R.Context(), sb); err != nil && !errors.Is(err, errDirectDisabled) {
		h.s.Log.Warn("direct ports not updated", "sandbox", sb.ID, "err", err)
	}
	return c.OK(200, "port.hosted", h.portJSON(sb, hp))
}

func (h *hosting) handleList(c *Ctx) error {
	sb, err := h.sandbox(c)
	if err != nil {
		return err
	}
	all := c.Query("all") == "1" || c.Query("all") == "true"
	list, err := h.list(sb.ID, all)
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, hp := range list {
		out = append(out, h.portJSON(sb, hp))
	}
	return c.OK(200, "port.list", map[string]any{"sandboxId": sb.ID, "ports": out, "hostedPorts": out, "domain": h.domain})
}

func (h *hosting) portParam(c *Ctx) (int, error) {
	p, err := strconv.Atoi(c.Param("port"))
	if err != nil || p < 1 || p > 65535 {
		return 0, Err(400, "invalid_port", "port must be 1 to 65535.")
	}
	return p, nil
}

func (h *hosting) handleGetPort(c *Ctx) error {
	sb, err := h.sandbox(c)
	if err != nil {
		return err
	}
	port, err := h.portParam(c)
	if err != nil {
		return err
	}
	hp, err := h.load(sb.ID, port)
	if err != nil || hp.Hidden {
		return Err(404, "port_not_hosted", fmt.Sprintf("Port %d is not hosted. Host it first.", port))
	}
	return c.OK(200, "port.hosted", h.portJSON(sb, hp))
}

// handleHide is `host hide`: the route goes away, the token is kept.
func (h *hosting) handleHide(c *Ctx) error {
	sb, err := h.sandbox(c)
	if err != nil {
		return err
	}
	port, err := h.portParam(c)
	if err != nil {
		return err
	}
	res, err := h.s.DB.Exec(`UPDATE hosted_ports SET hidden=1, localhost=0, updated_at=? WHERE sandbox_id=? AND port=?`, store.Now(), sb.ID, port)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Err(404, "port_not_hosted", fmt.Sprintf("Port %d is not hosted.", port))
	}
	h.cut(sb.ID, port)
	_ = h.s.syncDirect(c.R.Context(), sb)
	return c.OK(200, "port.hidden", map[string]any{"success": true, "sandboxId": sb.ID, "port": port})
}

// checkHostedPorts refuses a malformed entry or a duplicate port before the
// create (Boat: 400 invalid_hosted_ports). Per-port refusals such as a
// reserved port still come back in the answer's hostedPorts.
func checkHostedPorts(ports []json.RawMessage) error {
	seen := map[int]bool{}
	for _, raw := range ports {
		var req hostReq
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			req.Port = n
		} else if err := json.Unmarshal(raw, &req); err != nil {
			return Err(400, "invalid_hosted_ports", "Each hostedPorts entry is a port number or {port, public?, localhost?, title?}.")
		}
		if req.Port < 1 || req.Port > 65535 {
			return Err(400, "invalid_hosted_ports", fmt.Sprintf("Port %d is not between 1 and 65535.", req.Port))
		}
		if seen[req.Port] {
			return Err(400, "invalid_hosted_ports", fmt.Sprintf("Port %d is listed twice.", req.Port))
		}
		seen[req.Port] = true
	}
	return nil
}

// createHook answers create-time hostedPorts (routed before the sandbox is ready).
func (h *hosting) createHook(c *Ctx, sb *Sandbox, ports []json.RawMessage) []map[string]any {
	out := []map[string]any{}
	seen := map[int]bool{}
	for _, raw := range ports {
		var req hostReq
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			req.Port = n
		} else if err := json.Unmarshal(raw, &req); err != nil {
			out = append(out, map[string]any{"port": nil, "error": "invalid_hosted_ports", "message": "Each hostedPorts entry is {port, public?, localhost?, title?}."})
			continue
		}
		if seen[req.Port] {
			out = append(out, map[string]any{"port": req.Port, "error": "invalid_hosted_ports", "message": fmt.Sprintf("Port %d is listed twice.", req.Port)})
			continue
		}
		seen[req.Port] = true
		hp, err := h.host(sb, req)
		if err != nil {
			ae := toAPIError(err)
			out = append(out, map[string]any{"port": req.Port, "error": ae.Err.Code, "message": ae.Err.Message})
			continue
		}
		m := h.portJSON(sb, hp)
		delete(m, "success")
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// Routing and proxy
// ---------------------------------------------------------------------------

// intercept serves requests whose Host is <label>-<port>.<domain>.
func (h *hosting) intercept(w http.ResponseWriter, r *http.Request) bool {
	host := strings.ToLower(r.Host)
	if hn, _, err := net.SplitHostPort(host); err == nil {
		host = hn
	}
	var first string
	for _, d := range []string{h.domain, "localhost"} {
		if rest, ok := strings.CutSuffix(host, "."+d); ok && rest != "" {
			first = rest
			break
		}
	}
	if first == "" || strings.Contains(first, ".") {
		return false
	}
	i := strings.LastIndexByte(first, '-')
	if i <= 0 {
		return false
	}
	port, err := strconv.Atoi(first[i+1:])
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	sb := h.sandboxByLabel(first[:i])
	if sb == nil {
		hostPage(w, 404, "No sandbox answers at this address.")
		return true
	}
	h.serve(w, r, sb, port, "")
	return true
}

func (h *hosting) sandboxByLabel(label string) *Sandbox {
	if rest, ok := strings.CutPrefix(label, strings.TrimSuffix(api.PrefixSandbox, "_")+"-"); ok {
		if sb, err := h.s.SandboxByID(api.PrefixSandbox + rest); err == nil && sb.DeletedAt == nil && sb.Subdomain == "" {
			return sb
		}
	}
	sb, err := scanSandbox(h.s.DB.QueryRow(`SELECT `+sbCols+` FROM sandboxes WHERE subdomain=? AND deleted_at IS NULL`, label))
	if err != nil {
		return nil
	}
	return sb
}

// handlePath serves /p/{sandboxId}/{port}/...
func (h *hosting) handlePath(w http.ResponseWriter, r *http.Request) {
	id, ps := r.PathValue("sandboxId"), r.PathValue("port")
	port, err := strconv.Atoi(ps)
	if err != nil || port < 1 || port > 65535 {
		hostPage(w, 404, "Not a hosted port.")
		return
	}
	prefix := "/p/" + id + "/" + ps
	if r.URL.Path == prefix {
		u := prefix + "/"
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, u, http.StatusMovedPermanently)
		return
	}
	sb, err := h.s.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil {
		if sb = h.sandboxByLabel(id); sb == nil {
			hostPage(w, 404, "No sandbox answers at this address.")
			return
		}
	}
	h.serve(w, r, sb, port, prefix)
}

func hostPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "CrowdVM: %s\n", msg)
}

func isWebsocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// serve checks firewall, token and state, then proxies to the guest port.
// prefix is "" for host routing, "/p/<id>/<port>" for path routing.
func (h *hosting) serve(w http.ResponseWriter, r *http.Request, sb *Sandbox, port int, prefix string) {
	hp, err := h.load(sb.ID, port)
	if err != nil || hp.Hidden {
		hostPage(w, 404, fmt.Sprintf("Port %d of this sandbox is not hosted. Run `crowdvm host %s %d` first.", port, sb.ID, port))
		return
	}
	if len(hp.Allow) > 0 || len(hp.Deny) > 0 {
		ip := net.ParseIP(h.s.clientIP(r))
		if ip == nil || cidrsMatch(hp.Deny, ip) || (len(hp.Allow) > 0 && !cidrsMatch(hp.Allow, ip)) {
			hostPage(w, 403, "Your address is not allowed by this URL's firewall rules.")
			return
		}
	}
	q := r.URL.Query()
	tok, hadTok := q.Get(portTokenParam), q.Has(portTokenParam)
	q.Del(portTokenParam)
	if !hp.Public {
		if tok != "" {
			if !tokenEq(tok, hp.Token) {
				hostPage(w, 401, "The access token of this URL is not valid. Ask its owner for the current URL.")
				return
			}
			if !isWebsocket(r) && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				cpath := "/"
				if prefix != "" {
					cpath = prefix + "/"
				}
				http.SetCookie(w, h.authCookie(hp.Token, cpath))
				loc := r.URL.EscapedPath()
				if enc := q.Encode(); enc != "" {
					loc += "?" + enc
				}
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, loc, http.StatusFound)
				return
			}
		} else if ck, err := r.Cookie(portAuthCookie); err != nil || !tokenEq(ck.Value, hp.Token) {
			hostPage(w, 401, "This URL is protected. Open it with its ?_token= link, or send Cookie: _port_auth=<token>.")
			return
		}
	}
	// Tracked from the access check on, so a hide while the request waits
	// for a starting sandbox cuts it too (bugs-r5-sonnet).
	ctx, done := h.track(r.Context(), sb.ID, port)
	defer done()
	r = r.WithContext(ctx)
	if sb.starting() {
		if sb, err = h.s.WaitUsable(r.Context(), sb.ID, h.s.Cfg.StartWait); err != nil {
			hostPage(w, 503, "The sandbox is starting. Retry in a few seconds.")
			return
		}
	}
	if !sb.Usable() {
		if sb.starting() {
			w.Header().Set("Retry-After", "3")
			hostPage(w, 503, "The sandbox is still starting. Retry in a few seconds.")
		} else {
			hostPage(w, 503, "The sandbox is not running ("+sb.State+"). Resume it to use this URL.")
		}
		return
	}
	outPath := r.URL.EscapedPath()
	if prefix != "" {
		outPath = strings.TrimPrefix(outPath, prefix)
		if outPath == "" {
			outPath = "/"
		}
	}
	rawQuery := r.URL.RawQuery // keep the client's encoding unless _token was removed
	if hadTok {
		rawQuery = q.Encode()
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetXForwarded()
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", dialHost(sb.ID, port)
			pr.Out.URL.RawPath = outPath
			if p, err := url.PathUnescape(outPath); err == nil {
				pr.Out.URL.Path = p
			} else {
				pr.Out.URL.Path, pr.Out.URL.RawPath = outPath, ""
			}
			pr.Out.URL.RawQuery = rawQuery
			pr.Out.Host = r.Host
			if prefix != "" {
				pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			}
			stripCookies(pr.Out.Header, portAuthCookie, SessionCookie)
			pr.Out.Header.Del(CSRFHeader)
		},
		Transport:     h.tr,
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if prefix != "" {
				pathScopeCookies(resp.Header, prefix)
				resp.Header.Add("Content-Security-Policy", pathSandboxCSP)
				if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
					resp.Header.Set("Location", prefix+loc)
				}
			} else {
				hostScopeCookies(resp.Header) // bugs-r10-opus 4
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			hostPage(w, 502, fmt.Sprintf("Nothing answered on port %d in the sandbox. Start the app (listening on 0.0.0.0 or localhost) and retry.", port))
		},
	}
	rp.ServeHTTP(w, r)
}

// pathScopeCookies: a path-routed app shares the server's origin. Its
// cookies stay under its own prefix and never use the server's names, and it
// cannot clear the origin's data (bugs-r9-sonnet).
func pathScopeCookies(h http.Header, prefix string) {
	dropOriginHeaders(h)
	var keep []string
	for _, line := range h.Values("Set-Cookie") {
		ck, err := http.ParseSetCookie(line)
		if err != nil || ownCookieName(ck.Name) {
			continue
		}
		ck.Path, ck.Domain = prefix+"/", ""
		if v := ck.String(); v != "" {
			keep = append(keep, v)
		}
	}
	h.Del("Set-Cookie")
	for _, v := range keep {
		h.Add("Set-Cookie", v)
	}
}

// hostScopeCookies: a host-routed app has its own host, but a Domain cookie
// reaches the public domain's siblings, the dashboard included. Host-only
// cookies stay (bugs-r10-opus 4).
func hostScopeCookies(h http.Header) {
	dropOriginHeaders(h)
	var keep []string
	for _, line := range h.Values("Set-Cookie") {
		ck, err := http.ParseSetCookie(line)
		if err != nil || ck.Domain != "" || ownCookieName(ck.Name) {
			continue
		}
		keep = append(keep, line)
	}
	h.Del("Set-Cookie")
	for _, v := range keep {
		h.Add("Set-Cookie", v)
	}
}

// dropOriginHeaders: response headers that act on the whole origin.
func dropOriginHeaders(h http.Header) {
	for _, k := range originHeaders {
		h.Del(k)
	}
}

// originHeaders act on the receiving origin (or its reporting), not on the
// response (bugs-r10-opus 4: NEL and Report-To added).
var originHeaders = []string{"Clear-Site-Data", "Refresh", "NEL", "Report-To"}

// ownCookieName: names a guest never sets, as they are the server's.
func ownCookieName(name string) bool {
	return name == SessionCookie || name == portAuthCookie || strings.HasPrefix(strings.ToLower(name), "crowdvm")
}

func (h *hosting) authCookie(tok, path string) *http.Cookie {
	ck := &http.Cookie{Name: portAuthCookie, Value: tok, Path: path, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if h.scheme == "https" {
		ck.Secure, ck.SameSite = true, http.SameSiteNoneMode // works in third-party iframes where allowed
	}
	return ck
}

func tokenEq(a, b string) bool {
	return b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// stripCookies removes the named cookies from a Cookie header.
func stripCookies(hdr http.Header, names ...string) {
	var keep []string
	for _, line := range hdr.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name != "" && !slices.Contains(names, name) {
				keep = append(keep, strings.TrimSpace(part))
			}
		}
	}
	hdr.Del("Cookie")
	if len(keep) > 0 {
		hdr.Set("Cookie", strings.Join(keep, "; "))
	}
}

// dialHost encodes a sandbox port as a transport address; dial decodes it.
func dialHost(sbID string, port int) string {
	return strings.ReplaceAll(sbID, "_", "-") + ".sandbox:" + strconv.Itoa(port)
}

func (h *hosting) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(ps)
	id := strings.Replace(strings.TrimSuffix(host, ".sandbox"), "-", "_", 1)
	sb, err := h.s.SandboxByID(id)
	if err != nil || sb.DeletedAt != nil {
		return nil, errors.New("sandbox not found")
	}
	return h.s.DialSandbox(ctx, sb, port)
}
