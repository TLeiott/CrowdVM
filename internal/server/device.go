package server

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Device-code login (CLI) and agent self-registration (auth.md adapted for
// a LAN server). Both use device_codes rows:
//
//	kind "cli":   POST /auth/device/start -> user approves at /device -> poll returns a CLI session token (cvms_...)
//	kind "agent": POST /agent/identity (email) or /agent/identity/claim -> user approves -> /oauth2/token returns an API key (cvm_...)

const (
	deviceTTL      = 10 * time.Minute
	deviceInterval = 2
)

func (s *Server) deviceRoutes() {
	s.Route("POST /auth/device/start", ActionPublic, s.handleDeviceStart)
	s.Route("POST /auth/device/poll", ActionPublic, s.handleDevicePoll)
	s.Route("POST /auth/device/approve", ActionSession, s.handleDeviceApprove)
	s.Route("GET /auth/device/lookup", ActionSession, s.handleDeviceLookup)
}

type deviceRow struct {
	ID, UserCode, Kind, ClientName, Email, AgentUserID, Status, UserID, Token, TokenType string
	ExpiresAt                                                                            int64
	Interval                                                                             int
}

func (s *Server) deviceBy(col, v string) (*deviceRow, error) {
	d := &deviceRow{}
	err := s.DB.QueryRow(`SELECT id,user_code,kind,client_name,COALESCE(email,''),COALESCE(agent_user_id,''),status,COALESCE(user_id,''),COALESCE(token,''),COALESCE(token_type,''),expires_at,interval_s FROM device_codes WHERE `+col+`=?`, v).
		Scan(&d.ID, &d.UserCode, &d.Kind, &d.ClientName, &d.Email, &d.AgentUserID, &d.Status, &d.UserID, &d.Token, &d.TokenType, &d.ExpiresAt, &d.Interval)
	return d, err
}

// newDevice creates a device code row and returns (deviceCode, userCode).
func (s *Server) newDevice(kind, client, email, agentUser string) (string, string, error) {
	dc := randToken(40)
	var uc string
	if kind == "agent" {
		uc = randDigits(6)
	} else {
		const alpha = "BCDFGHJKLMNPQRSTVWXZ"
		b := []byte(randToken(8))
		for i := range b {
			b[i] = alpha[int(b[i])%len(alpha)]
		}
		uc = string(b[:4]) + "-" + string(b[4:])
	}
	now := store.Now()
	_, err := s.DB.Exec(`INSERT INTO device_codes(id,user_code,kind,client_name,email,agent_user_id,status,created_at,expires_at,interval_s) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		hashToken(dc), uc, kind, client, nz(email), nz(agentUser), "pending", now, now+deviceTTL.Milliseconds(), deviceInterval)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return s.newDevice(kind, client, email, agentUser)
	}
	return dc, uc, err
}

func normUserCode(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	c = strings.ReplaceAll(c, " ", "")
	if len(c) == 8 && !strings.Contains(c, "-") && strings.Trim(c, "0123456789") != "" {
		c = c[:4] + "-" + c[4:]
	}
	return c
}

func (s *Server) handleDeviceStart(c *Ctx) error {
	var req struct {
		ClientName string `json:"clientName"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.ClientName == "" {
		req.ClientName = c.R.UserAgent()
	}
	dc, uc, err := s.newDevice("cli", req.ClientName, "", "")
	if err != nil {
		return err
	}
	vu := s.Cfg.PublicURL + "/device"
	return c.OK(200, "device.started", map[string]any{
		"deviceCode": dc, "userCode": uc, "verificationUrl": vu,
		"verificationUrlComplete": vu + "?code=" + url.QueryEscape(uc),
		"interval":                deviceInterval, "expiresIn": int(deviceTTL.Seconds()),
	})
}

// handleDevicePoll answers {status: pending|approved|denied|expired}; on
// approved it carries the CLI session token exactly once.
func (s *Server) handleDevicePoll(c *Ctx) error {
	var req struct {
		DeviceCode string `json:"deviceCode"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	d, err := s.deviceBy("id", hashToken(req.DeviceCode))
	if err != nil || d.Kind != "cli" {
		return errNotFound("Device code")
	}
	_, _ = s.DB.Exec(`UPDATE device_codes SET last_poll_at=? WHERE id=?`, store.Now(), d.ID)
	if d.ExpiresAt < store.Now() && d.Status == "pending" {
		return c.OK(200, "device.poll", map[string]any{"status": "expired"})
	}
	switch d.Status {
	case "approved":
		// Only the poll that marks the code consumed gets a session.
		if res, err := s.DB.Exec(`UPDATE device_codes SET status='consumed' WHERE id=? AND status='approved'`, d.ID); err != nil {
			return err
		} else if n, _ := res.RowsAffected(); n != 1 {
			return c.OK(200, "device.poll", map[string]any{"status": "expired"})
		}
		tok, err := s.NewSession(d.UserID, sessionCLI, d.ClientName)
		if err != nil {
			return err
		}
		u, err := s.UserByID(d.UserID)
		if err != nil {
			return err
		}
		return c.OK(200, "device.poll", map[string]any{"status": "approved", "token": tok, "tokenType": "session", "user": s.userJSON(u)})
	case "consumed":
		return c.OK(200, "device.poll", map[string]any{"status": "expired"})
	default:
		return c.OK(200, "device.poll", map[string]any{"status": d.Status, "interval": d.Interval})
	}
}

// codeMisses counts each user's wrong device codes. Agent claim codes are
// six digits: without a cap, a signed-in user could guess pending ones and
// claim other people's agents into their own account (bugs-r13-sonnet).
type codeMisses struct {
	mu sync.Mutex
	m  map[string]*ipWin
}

const (
	codeMissMax    = 20
	codeMissWindow = 10 * time.Minute
)

// check refuses a user with too many wrong codes in the window.
func (cm *codeMisses) check(user string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if e := cm.m[user]; e != nil && time.Since(e.start) < codeMissWindow && e.n >= codeMissMax {
		return ErrD(429, "too_many_attempts", "Too many wrong codes. Try again in a few minutes.", map[string]any{"retryAfter": int((codeMissWindow - time.Since(e.start)).Seconds()) + 1})
	}
	return nil
}

func (cm *codeMisses) miss(user string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.m == nil {
		cm.m = map[string]*ipWin{}
	}
	now := time.Now()
	for k, e := range cm.m {
		if now.Sub(e.start) >= codeMissWindow {
			delete(cm.m, k)
		}
	}
	e := cm.m[user]
	if e == nil {
		e = &ipWin{start: now}
		cm.m[user] = e
	}
	e.n++
}

func (s *Server) handleDeviceLookup(c *Ctx) error {
	if err := s.codeMiss.check(c.P.User.ID); err != nil {
		return err
	}
	d, err := s.deviceBy("user_code", normUserCode(c.Query("code")))
	if err != nil || d.ExpiresAt < store.Now() {
		s.codeMiss.miss(c.P.User.ID)
		return errNotFound("Code")
	}
	return c.OK(200, "device.info", map[string]any{"kind": d.Kind, "clientName": d.ClientName, "status": d.Status, "email": nz(d.Email)})
}

func (s *Server) handleDeviceApprove(c *Ctx) error {
	var req struct {
		UserCode string   `json:"userCode"`
		Approve  *bool    `json:"approve"`
		Preset   string   `json:"preset"` // agent claims: API key preset (default "ci")
		Actions  []string `json:"actions"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if err := s.codeMiss.check(c.P.User.ID); err != nil {
		return err
	}
	d, err := s.deviceBy("user_code", normUserCode(req.UserCode))
	if err != nil {
		s.codeMiss.miss(c.P.User.ID)
	} else {
		// One approval per code: parallel ones would each mint a key
		// (bugs-r13-sonnet 6). Read the row again under the lock.
		unlock := s.sbLocks.Lock("device:" + d.ID)
		defer unlock()
		d, err = s.deviceBy("id", d.ID)
	}
	if err != nil || d.ExpiresAt < store.Now() || d.Status != "pending" {
		return Err(404, "invalid_code", "The code is wrong, expired or already used.")
	}
	// bugs-r11-sonnet 7: users.email is proven (code entered) unless an admin set it, so this match is safe.
	if d.Email != "" && !strings.EqualFold(d.Email, c.P.User.Email) {
		return Err(403, "wrong_account", "This agent asked for the account "+d.Email+". Sign in as that account to approve it.")
	}
	if c.P.User.Kind == "agent" {
		return Err(403, "forbidden", "Agents cannot approve logins.")
	}
	status := "approved"
	if req.Approve != nil && !*req.Approve {
		status = "denied"
	}
	var token, tokType string
	if status == "approved" && d.Kind == "agent" {
		preset := req.Preset
		if preset == "" && len(req.Actions) == 0 {
			preset = "ci"
		}
		name := "agent"
		if d.ClientName != "" {
			name = "agent: " + d.ClientName
		}
		exp := time.Now().Add(90 * 24 * time.Hour)
		k, secret, err := s.createKey(c.P.User, keySpec{Name: name, Preset: preset, Actions: req.Actions, ExpiresAt: &exp})
		if err != nil {
			return err
		}
		_ = k
		token, tokType = secret, "api_key"
		if d.AgentUserID != "" {
			_, _ = s.DB.Exec(`UPDATE users SET claimed_by=? WHERE id=? AND kind='agent'`, c.P.User.ID, d.AgentUserID)
			_, _ = s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, store.Now(), d.AgentUserID)
		}
	}
	if _, err := s.DB.Exec(`UPDATE device_codes SET status=?, user_id=?, token=?, token_type=? WHERE id=?`, status, c.P.User.ID, nz(token), nz(tokType), d.ID); err != nil {
		return err
	}
	return c.OK(200, "device."+status, map[string]any{"kind": d.Kind, "clientName": d.ClientName, "status": status})
}

// ---------------------------------------------------------------------------
// Agent self-registration
// ---------------------------------------------------------------------------

func (s *Server) agentAuthRoutes() {
	s.router.HandleFunc("GET /auth.md", s.handleAuthMD)
	s.router.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleWellKnownResource)
	s.router.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleWellKnownAS)
	s.router.HandleFunc("GET /api/v1/.well-known/oauth-protected-resource", s.handleWellKnownResource)
	s.Route("POST /agent/identity", ActionPublic, s.handleAgentIdentity)
	s.Route("POST /agent/identity/claim", ActionAny, s.handleAgentClaim)
	s.router.HandleFunc("GET /agent/identity/claim", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/device?"+r.URL.RawQuery, http.StatusFound)
	})
	s.router.HandleFunc("POST /oauth2/token", s.handleOAuthToken)
	s.router.HandleFunc("POST /oauth2/revoke", s.handleOAuthRevoke)
}

func (s *Server) handleWellKnownResource(w http.ResponseWriter, r *http.Request) {
	writeRawJSON(w, 200, map[string]any{
		"resource":                 s.Cfg.PublicURL + "/api/v1",
		"authorization_servers":    []string{s.Cfg.PublicURL},
		"bearer_methods_supported": []string{"header"},
		"resource_documentation":   s.Cfg.PublicURL + "/auth.md",
	})
}

func (s *Server) handleWellKnownAS(w http.ResponseWriter, r *http.Request) {
	u := s.Cfg.PublicURL
	writeRawJSON(w, 200, map[string]any{
		"issuer":                        u,
		"token_endpoint":                u + "/oauth2/token",
		"revocation_endpoint":           u + "/oauth2/revoke",
		"device_authorization_endpoint": u + "/auth/device/start",
		"grant_types_supported":         []string{"urn:ietf:params:oauth:grant-type:device_code"},
		"agent_auth": map[string]any{
			"registration_endpoint": u + "/agent/identity",
			"claim_endpoint":        u + "/agent/identity/claim",
			"methods":               []string{"anonymous", "email"},
			"documentation":         u + "/auth.md",
		},
	})
}

func (s *Server) handleAuthMD(w http.ResponseWriter, r *http.Request) {
	u := s.Cfg.PublicURL
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(`# CrowdVM agent registration

CrowdVM is a self-hosted sandbox server on this network. An AI agent can get
its own API key without a pasted secret. A human on this server approves it.

## Email method (you know your human's account email)

1. POST ` + u + `/agent/identity  {"method":"email","email":"<their email>","name":"<agent name>"}
   -> {"device_code","user_code","verification_uri","interval","expires_in"}
2. Tell your human: open verification_uri, sign in, type user_code.
3. Poll POST ` + u + `/oauth2/token  {"grant_type":"urn:ietf:params:oauth:grant-type:device_code","device_code":"..."}
   every interval seconds. While waiting: 400 {"error":"authorization_pending"}.
   When approved: {"access_token":"cvm_...","token_type":"Bearer"}.
4. Call ` + u + `/api/v1 with "Authorization: Bearer <access_token>".

## Anonymous method

1. POST ` + u + `/agent/identity {"method":"anonymous","name":"<agent name>"}
   -> {"access_token":"cvm_..."} that can only call GET /api/v1/me and GET /api/v1/limits.
2. POST ` + u + `/agent/identity/claim with that token -> user_code + verification_uri.
3. Your human approves it as above; poll /oauth2/token for the real key.

Revoke a token: POST ` + u + `/oauth2/revoke {"token":"cvm_..."}.
API reference: the endpoints mirror Boat's v1 API under ` + u + `/api/v1.
`))
}

func (s *Server) handleAgentIdentity(c *Ctx) error {
	var req struct {
		Method string `json:"method"`
		Type   string `json:"type"`
		Email  string `json:"email"`
		Name   string `json:"name"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	m := req.Method
	if m == "" {
		m = req.Type
	}
	if m == "" && req.Email != "" {
		m = "email"
	}
	switch m {
	case "email", "service_auth":
		email := strings.ToLower(strings.TrimSpace(req.Email))
		if !emailRe.MatchString(email) {
			return Err(400, "invalid_email", "A valid email is required for the email method.")
		}
		dc, uc, err := s.newDevice("agent", req.Name, email, "")
		if err != nil {
			return err
		}
		s.Log.Info("agent registration pending", "email", email, "agent", req.Name, "code", uc)
		s.sendMail(email, "An agent asks for CrowdVM access", "Agent "+req.Name+" asks for access to your CrowdVM account. If you started it, open "+s.Cfg.PublicURL+"/device and enter "+uc+".")
		return c.OK(200, "agent.identity", s.deviceGrant(dc, uc))
	case "anonymous", "":
		name := req.Name
		uname := s.freeUsername("agent-" + randToken(6))
		zero := 0.0
		u, err := s.CreateUser(NewUserParams{Username: uname, DisplayName: firstNonEmpty(name, uname), Kind: "agent", ConnectionMethod: "agent", Credits: &zero})
		if err != nil {
			return err
		}
		_, secret, err := s.createKey(u, keySpec{Name: "agent identity", Actions: []string{"account.read"}})
		if err != nil {
			return err
		}
		return c.OK(200, "agent.identity", map[string]any{
			"access_token": secret, "token_type": "Bearer", "agent_id": u.ID, "claimed": false,
			"claim_endpoint": s.Cfg.PublicURL + "/agent/identity/claim",
		})
	default:
		return Err(400, "unsupported_method", "method must be anonymous or email (identity assertions are not accepted).")
	}
}

func (s *Server) deviceGrant(dc, uc string) map[string]any {
	vu := s.Cfg.PublicURL + "/device"
	return map[string]any{
		"device_code": dc, "user_code": uc, "verification_uri": vu,
		"verification_uri_complete": vu + "?code=" + url.QueryEscape(uc),
		"interval":                  deviceInterval, "expires_in": int(deviceTTL.Seconds()),
	}
}

func (s *Server) handleAgentClaim(c *Ctx) error {
	u := c.P.User
	if u.Kind != "agent" {
		return Err(400, "not_an_agent", "Only an anonymous agent identity can start a claim.")
	}
	if u.ClaimedBy != "" {
		return Err(409, "already_claimed", "This agent is already claimed.")
	}
	dc, uc, err := s.newDevice("agent", u.DisplayName, "", u.ID)
	if err != nil {
		return err
	}
	s.Log.Info("agent claim pending", "agent", u.Username, "code", uc)
	return c.OK(200, "agent.claim", s.deviceGrant(dc, uc))
}

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	writeRawJSON(w, status, map[string]any{"error": code, "error_description": desc})
}

func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	params := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m map[string]any
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&m)
		for k, v := range m {
			if sv, ok := v.(string); ok {
				params[k] = sv
			}
		}
	} else {
		_ = r.ParseForm()
		for k := range r.PostForm {
			params[k] = r.PostForm.Get(k)
		}
	}
	if gt := params["grant_type"]; gt != "" && gt != "urn:ietf:params:oauth:grant-type:device_code" && gt != "device_code" {
		oauthErr(w, 400, "unsupported_grant_type", "Only the device_code grant is supported.")
		return
	}
	d, err := s.deviceBy("id", hashToken(params["device_code"]))
	if err != nil || d.Kind != "agent" {
		oauthErr(w, 400, "invalid_grant", "Unknown device_code.")
		return
	}
	switch {
	case d.Status == "pending" && d.ExpiresAt < store.Now():
		oauthErr(w, 400, "expired_token", "The code expired. Register again.")
	case d.Status == "pending":
		oauthErr(w, 400, "authorization_pending", "Waiting for the human to approve.")
	case d.Status == "denied":
		oauthErr(w, 400, "access_denied", "The human denied the request.")
	case d.Status == "approved" && d.Token != "":
		// Only the poll that consumes the code gets the key.
		var n int64
		if res, err := s.DB.Exec(`UPDATE device_codes SET status='consumed', token=NULL WHERE id=? AND status='approved'`, d.ID); err == nil {
			n, _ = res.RowsAffected()
		}
		if n != 1 {
			oauthErr(w, 400, "invalid_grant", "The code was already used.")
			return
		}
		writeRawJSON(w, 200, map[string]any{"access_token": d.Token, "token_type": "Bearer", "expires_in": int((90 * 24 * time.Hour).Seconds())})
	default:
		oauthErr(w, 400, "invalid_grant", "The code was already used.")
	}
}

func (s *Server) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	tok := ""
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var m struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&m)
		tok = m.Token
	} else {
		_ = r.ParseForm()
		tok = r.PostForm.Get("token")
	}
	if tok != "" {
		_, _ = s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE secret_hash=? AND revoked_at IS NULL`, store.Now(), hashToken(tok))
		_, _ = s.DB.Exec(`DELETE FROM sessions WHERE id=?`, hashToken(tok))
	}
	writeRawJSON(w, 200, map[string]any{})
}

func writeRawJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

var _ = sql.ErrNoRows

// ---------------------------------------------------------------------------
// /device approval page (minimal; the dashboard may replace it)
// ---------------------------------------------------------------------------

var devicePage = template.Must(template.New("device").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>CrowdVM sign-in</title>
<style>
:root{color-scheme:light dark;font-family:system-ui,sans-serif}
body{max-width:26rem;margin:4rem auto;padding:0 1rem}
input,button{font:inherit;padding:.5rem .7rem;margin:.25rem 0;width:100%;box-sizing:border-box}
button{cursor:pointer}.row{display:flex;gap:.5rem}.err{color:#c33}.ok{color:#2a7}
code{font-size:1.2em;letter-spacing:.1em}
</style></head><body>
<h1>CrowdVM</h1>
<div id="login" hidden>
  <p>Sign in to approve the request.</p>
  <input id="u" placeholder="Username or email" autocomplete="username">
  <input id="p" type="password" placeholder="Password" autocomplete="current-password">
  <button id="lb">Sign in</button>
</div>
<div id="approve" hidden>
  <p>Signed in as <b id="who"></b>. Enter the code shown by the CLI or agent.</p>
  <input id="code" value="{{.Code}}" placeholder="ABCD-EFGH" autocapitalize="characters">
  <p id="info"></p>
  <div class="row"><button id="yes">Approve</button><button id="no">Deny</button></div>
</div>
<p id="msg"></p>
<script>
const $=id=>document.getElementById(id);
const msg=(t,c)=>{$("msg").textContent=t;$("msg").className=c||""};
async function call(m,p,b){const r=await fetch(p,{method:m,credentials:"same-origin",headers:{"Content-Type":"application/json","X-CrowdVM-CSRF":"1"},body:b?JSON.stringify(b):undefined});let j={};try{j=await r.json()}catch(e){}if(!r.ok)throw new Error(j.message||r.statusText);return j}
async function init(){try{const me=await call("GET","/auth/me");$("who").textContent=me.user.login;$("approve").hidden=false;$("login").hidden=true;lookup()}catch(e){$("login").hidden=false;$("approve").hidden=true}}
async function lookup(){const c=$("code").value.trim();if(!c)return;try{const d=await call("GET","/auth/device/lookup?code="+encodeURIComponent(c));$("info").textContent=(d.kind==="agent"?"Agent":"CLI")+" asking: "+(d.clientName||"unknown client")}catch(e){$("info").textContent=""}}
$("lb").onclick=async()=>{try{await call("POST","/auth/login",{username:$("u").value,password:$("p").value});msg("");init()}catch(e){msg(e.message,"err")}};
$("code").onchange=lookup;
async function decide(ok){try{const r=await call("POST","/auth/device/approve",{userCode:$("code").value,approve:ok});msg(r.status==="approved"?"Approved. You can close this page.":"Denied.",r.status==="approved"?"ok":"err");$("approve").hidden=true}catch(e){msg(e.message,"err")}}
$("yes").onclick=()=>decide(true);$("no").onclick=()=>decide(false);
init();
</script></body></html>`))

func (s *Server) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	_ = devicePage.Execute(w, map[string]string{"Code": r.URL.Query().Get("code")})
}
