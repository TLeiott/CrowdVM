package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/smtp"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Session cookie, CSRF header and token prefixes.
const (
	SessionCookie  = "crowdvm_session"
	CSRFHeader     = "X-CrowdVM-CSRF"
	SessionPrefix  = "cvms_" // CLI session tokens (Bearer)
	PrefixUser     = "usr_"
	sessionBrowser = "browser"
	sessionCLI     = "cli"
)

// User is a local account (kind "user") or a self-registered agent ("agent").
type User struct {
	ID                 string
	Username           string
	Email              string
	DisplayName        string
	IsAdmin            bool
	Kind               string
	ConnectionMethod   string
	ClaimedBy          string
	Disabled           bool
	ActiveOrg          string
	ActiveWalletChosen bool
	CreatedAt          time.Time
	passwordHash       string
}

// Session is a browser or CLI login.
type Session struct {
	ID     string // hash of the token
	UserID string
	Kind   string
}

// Principal is the authenticated caller.
type Principal struct {
	User      *User
	Session   *Session // set for browser/CLI sessions
	Key       *APIKey  // set for API keys
	ViaCookie bool
}

// IsSession reports a browser or CLI session (not an API key).
func (p *Principal) IsSession() bool { return p.Session != nil }

// IsAdmin reports a server admin acting with full rights.
func (p *Principal) IsAdmin() bool {
	if p == nil || p.User == nil || !p.User.IsAdmin {
		return false
	}
	return p.Key == nil || p.Key.hasAction("*")
}

const userCols = `id,username,COALESCE(email,''),display_name,password_hash,is_admin,kind,connection_method,COALESCE(claimed_by,''),disabled,COALESCE(active_org,''),active_wallet_chosen,created_at`

func scanUser(sc interface{ Scan(...any) error }) (*User, error) {
	u := &User{}
	var created int64
	err := sc.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.passwordHash, &u.IsAdmin, &u.Kind,
		&u.ConnectionMethod, &u.ClaimedBy, &u.Disabled, &u.ActiveOrg, &u.ActiveWalletChosen, &created)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = store.Time(created)
	return u, nil
}

// UserByID loads a user.
func (s *Server) UserByID(id string) (*User, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

// UserByLogin finds a user by username, email or id.
func (s *Server) UserByLogin(login string) (*User, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE username=? OR email=? OR id=? LIMIT 1`, login, login, login))
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,38}$`)

// NewUserParams creates an account (CLI admin commands, signup, tests).
type NewUserParams struct {
	Username, Password, Email, DisplayName string
	Admin                                  bool
	Kind, ConnectionMethod                 string
	Credits                                *float64 // nil = Cfg.SignupCredits
}

// CreateUser inserts a user and its wallet.
func (s *Server) CreateUser(p NewUserParams) (*User, error) {
	if !usernameRe.MatchString(p.Username) {
		return nil, Err(400, "invalid_username", "Username must be 2-39 letters, digits, dots, dashes or underscores.")
	}
	var hash string
	if p.Password != "" {
		if len(p.Password) < 8 {
			return nil, Err(400, "weak_password", "Password must have at least 8 characters.")
		}
		h, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
		if err != nil {
			return nil, err
		}
		hash = string(h)
	}
	if p.Kind == "" {
		p.Kind = "user"
	}
	if p.ConnectionMethod == "" {
		p.ConnectionMethod = "password"
	}
	if p.DisplayName == "" {
		p.DisplayName = p.Username
	}
	var email any
	if p.Email != "" {
		email = strings.ToLower(strings.TrimSpace(p.Email))
	}
	credits := s.Cfg.SignupCredits
	if p.Credits != nil {
		credits = *p.Credits
	}
	id := PrefixUser + randToken(16)
	err := s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE username=? OR (email IS NOT NULL AND email=?)`, p.Username, email).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return Err(409, "user_exists", "A user with that username or email already exists.")
		}
		if _, err := tx.Exec(`INSERT INTO users(id,username,email,display_name,password_hash,is_admin,kind,connection_method,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			id, p.Username, email, p.DisplayName, hash, boolInt(p.Admin), p.Kind, p.ConnectionMethod, store.Now()); err != nil {
			return err
		}
		return walletAdd(tx, id, credits, "grant", "signup credits", "", "", "")
	})
	if err != nil {
		return nil, err
	}
	return s.UserByID(id)
}

// SetPassword replaces a user's password.
func (s *Server) SetPassword(userID, password string) error {
	if len(password) < 8 {
		return Err(400, "weak_password", "Password must have at least 8 characters.")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`UPDATE users SET password_hash=? WHERE id=?`, string(h), userID)
	return err
}

func (s *Server) userCount() int {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE kind='user'`).Scan(&n)
	return n
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// NewSession creates a session and returns its token.
func (s *Server) NewSession(userID, kind, userAgent string) (string, error) {
	tok := SessionPrefix + randToken(40)
	now := store.Now()
	_, err := s.DB.Exec(`INSERT INTO sessions(id,user_id,kind,user_agent,created_at,expires_at,last_used_at) VALUES(?,?,?,?,?,?,?)`,
		hashToken(tok), userID, kind, userAgent, now, now+s.Cfg.SessionTTL.Milliseconds(), now)
	return tok, err
}

func (s *Server) sessionByToken(tok string) (*Session, error) {
	sess := &Session{}
	var exp, last int64
	err := s.DB.QueryRow(`SELECT id,user_id,kind,expires_at,last_used_at FROM sessions WHERE id=?`, hashToken(tok)).
		Scan(&sess.ID, &sess.UserID, &sess.Kind, &exp, &last)
	if err != nil {
		return nil, err
	}
	now := store.Now()
	if exp < now {
		return nil, sql.ErrNoRows
	}
	if now-last > 60_000 {
		_, _ = s.DB.Exec(`UPDATE sessions SET last_used_at=? WHERE id=?`, now, sess.ID)
	}
	return sess, nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(s.Cfg.PublicURL, "https://"), MaxAge: int(s.Cfg.SessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

// authenticate resolves the caller from a Bearer token or the session cookie.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*Principal, error) {
	p := &Principal{}
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok {
			tok, ok = strings.CutPrefix(h, "bearer ")
		}
		tok = strings.TrimSpace(tok)
		switch {
		case !ok || tok == "":
			return nil, errUnauthorized()
		case strings.HasPrefix(tok, SessionPrefix):
			sess, err := s.sessionByToken(tok)
			if err != nil {
				return nil, errUnauthorized()
			}
			p.Session = sess
		default:
			k, err := s.keyBySecret(tok)
			if err != nil {
				// Boat/Box-style prefixes (boat_, box_) in front of a CrowdVM key (API-137).
				for _, pre := range []string{"boat_", "box_"} {
					if rest, ok := strings.CutPrefix(tok, pre); ok && err != nil {
						k, err = s.keyBySecret(api.PrefixAPIKey + rest)
					}
				}
			}
			if err != nil {
				return nil, errUnauthorized()
			}
			if k.RevokedAt != nil {
				return nil, errUnauthorized()
			}
			if k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now()) {
				return nil, Err(403, "api_key_expired", "This API key has expired.")
			}
			p.Key = k
		}
	} else if ck, err := r.Cookie(SessionCookie); err == nil && ck.Value != "" {
		sess, err := s.sessionByToken(ck.Value)
		if err != nil {
			return nil, errUnauthorized()
		}
		if !isSafe(r.Method) && r.Header.Get(CSRFHeader) != "1" {
			return nil, Err(403, "csrf_required", "Cookie-authenticated requests that change state need the header "+CSRFHeader+": 1.")
		}
		p.Session, p.ViaCookie = sess, true
	} else {
		return nil, errUnauthorized()
	}
	uid := ""
	if p.Session != nil {
		uid = p.Session.UserID
	} else {
		uid = p.Key.UserID
	}
	u, err := s.UserByID(uid)
	if err != nil {
		return nil, errUnauthorized()
	}
	if u.Disabled {
		return nil, Err(403, "account_suspended", "This account is suspended.")
	}
	p.User = u
	return p, nil
}

// checkAction enforces the route's action on the principal.
func (s *Server) checkAction(p *Principal, r *http.Request, action string) error {
	if p.User.Kind == "agent" && p.User.ClaimedBy == "" {
		switch r.URL.Path {
		case "/api/v1/me", "/api/v1/limits", "/auth/me", "/agent/identity/claim":
		default:
			return ErrD(403, "claim_required", "This agent is not claimed yet. Start the claim with POST /agent/identity/claim and ask a human to approve it.",
				map[string]any{"claimUrl": s.Cfg.PublicURL + "/agent/identity/claim"})
		}
	}
	switch action {
	case ActionAny:
		return nil
	case ActionSession:
		if p.Key != nil {
			return Err(403, "session_required", "This action needs a browser or CLI session, not an API key.")
		}
		return nil
	case ActionAdmin:
		if !p.IsAdmin() {
			return Err(403, "admin_required", "This action needs a server administrator.")
		}
		return nil
	}
	if p.Key != nil && !p.Key.Allows(action) {
		return ErrD(403, "api_key_action_forbidden", "The API key does not allow "+action+".", map[string]any{"action": action})
	}
	return nil
}

func (s *Server) cleanupAuth() {
	now := store.Now()
	_, _ = s.DB.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now)
	_, _ = s.DB.Exec(`DELETE FROM email_codes WHERE expires_at < ?`, now)
	_, _ = s.DB.Exec(`DELETE FROM device_codes WHERE expires_at < ?`, now-3600_000)
}

// ---------------------------------------------------------------------------
// Handlers: /auth/*
// ---------------------------------------------------------------------------

func (s *Server) authRoutes() {
	s.Route("POST /auth/signup", ActionPublic, s.handleSignup)
	s.Route("POST /auth/login", ActionPublic, s.handleLogin)
	s.Route("POST /auth/logout", ActionAny, s.handleLogout)
	s.Route("GET /auth/me", ActionAny, s.handleAuthMe)
	s.Route("PATCH /auth/me", ActionSession, s.handleUpdateMe)
	s.Route("POST /auth/password", ActionSession, s.handleChangePassword)
	s.Route("POST /auth/email/start", ActionPublic, s.handleEmailStart)
	s.Route("POST /auth/email/verify", ActionPublic, s.handleEmailVerify)
	s.Route("GET /auth/config", ActionPublic, func(c *Ctx) error {
		return c.OK(200, "auth.config", map[string]any{
			"signupOpen": s.Cfg.OpenSignup || s.userCount() == 0, "firstUser": s.userCount() == 0,
			"emailLogin": true, "smtp": s.Cfg.SMTP.Addr != "", "publicUrl": s.Cfg.PublicURL,
		})
	})
	s.deviceRoutes()
	s.agentAuthRoutes()
}

type loginResult struct {
	User  map[string]any `json:"user"`
	Token string         `json:"token,omitempty"`
}

// startLogin creates a session: a cookie for browsers, a returned token for
// client "cli".
func (c *Ctx) startLogin(u *User, client, typ string) error {
	kind := sessionBrowser
	if client == "cli" {
		kind = sessionCLI
	}
	tok, err := c.S.NewSession(u.ID, kind, c.R.UserAgent())
	if err != nil {
		return err
	}
	res := loginResult{User: c.S.userJSON(u)}
	if kind == sessionCLI {
		res.Token = tok
	} else {
		c.S.setSessionCookie(c.W, tok)
	}
	return c.OK(200, typ, res)
}

func (s *Server) handleSignup(c *Ctx) error {
	var req struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		InviteCode  string `json:"inviteCode"`
		Client      string `json:"client"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	first := s.userCount() == 0
	if err := s.checkSignup(first, req.InviteCode); err != nil {
		return err
	}
	if req.Password == "" {
		return Err(400, "password_required", "password is required.")
	}
	u, err := s.CreateUser(NewUserParams{Username: req.Username, Password: req.Password, Email: req.Email, DisplayName: req.DisplayName, Admin: first})
	if err != nil {
		return err
	}
	s.useInvite(req.InviteCode, first)
	s.Log.Info("user signed up", "user", u.Username, "admin", u.IsAdmin)
	return c.startLogin(u, req.Client, "auth.signup")
}

// checkSignup allows the first user, open signup or a valid invite code.
func (s *Server) checkSignup(first bool, invite string) error {
	if first || s.Cfg.OpenSignup {
		return nil
	}
	if invite != "" {
		var n int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM invite_codes WHERE code_hash=? AND revoked_at IS NULL AND uses < max_uses AND (expires_at IS NULL OR expires_at > ?)`,
			hashToken(invite), store.Now()).Scan(&n)
		if n > 0 {
			return nil
		}
		return Err(403, "invalid_invite", "The invite code is not valid.")
	}
	return Err(403, "signup_closed", "Signup needs an invite code from an administrator.")
}

func (s *Server) useInvite(code string, first bool) {
	if code == "" || first || s.Cfg.OpenSignup {
		return
	}
	_, _ = s.DB.Exec(`UPDATE invite_codes SET uses=uses+1 WHERE code_hash=?`, hashToken(code))
}

func (s *Server) handleLogin(c *Ctx) error {
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Client   string `json:"client"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	login := req.Username
	if login == "" {
		login = req.Email
	}
	u, err := s.UserByLogin(login)
	if err != nil || u.passwordHash == "" || bcrypt.CompareHashAndPassword([]byte(u.passwordHash), []byte(req.Password)) != nil {
		return Err(401, "invalid_credentials", "Wrong username or password.")
	}
	if u.Disabled {
		return Err(403, "account_suspended", "This account is suspended.")
	}
	return c.startLogin(u, req.Client, "auth.login")
}

func (s *Server) handleLogout(c *Ctx) error {
	if c.P.Session != nil {
		_, _ = s.DB.Exec(`DELETE FROM sessions WHERE id=?`, c.P.Session.ID)
	}
	clearSessionCookie(c.W)
	return c.OK(200, "auth.logout", nil)
}

func (s *Server) handleAuthMe(c *Ctx) error {
	out := map[string]any{"user": s.userJSON(c.P.User), "authMethod": authMethod(c.P)}
	if c.P.Key != nil {
		out["apiKey"] = s.keyJSON(c.P.Key)
	}
	return c.OK(200, "auth.me", out)
}

func (s *Server) handleUpdateMe(c *Ctx) error {
	var req struct {
		DisplayName *string `json:"displayName"`
		Email       *string `json:"email"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.DisplayName != nil {
		n := strings.TrimSpace(*req.DisplayName)
		if n == "" || len(n) > 80 {
			return Err(400, "invalid_name", "displayName must be 1-80 characters.")
		}
		_, _ = s.DB.Exec(`UPDATE users SET display_name=? WHERE id=?`, n, c.P.User.ID)
	}
	if req.Email != nil {
		e := strings.ToLower(strings.TrimSpace(*req.Email))
		var v any
		if e != "" {
			if !emailRe.MatchString(e) {
				return Err(400, "invalid_email", "A valid email address is required.")
			}
			v = e
		}
		if _, err := s.DB.Exec(`UPDATE users SET email=? WHERE id=?`, v, c.P.User.ID); err != nil {
			return Err(409, "user_exists", "Another account uses that email.")
		}
	}
	u, err := s.UserByID(c.P.User.ID)
	if err != nil {
		return err
	}
	return c.OK(200, "auth.me", map[string]any{"user": s.userJSON(u)})
}

func authMethod(p *Principal) string {
	switch {
	case p.Key != nil:
		return "api_key"
	case p.ViaCookie:
		return "browser_session"
	default:
		return "cli_session"
	}
}

func (s *Server) handleChangePassword(c *Ctx) error {
	var req struct {
		Current string `json:"currentPassword"`
		New     string `json:"newPassword"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if c.P.User.passwordHash != "" && bcrypt.CompareHashAndPassword([]byte(c.P.User.passwordHash), []byte(req.Current)) != nil {
		return Err(401, "invalid_credentials", "The current password is wrong.")
	}
	if err := s.SetPassword(c.P.User.ID, req.New); err != nil {
		return err
	}
	return c.OK(200, "auth.password_changed", nil)
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)

func (s *Server) handleEmailStart(c *Ctx) error {
	var req struct {
		Email string `json:"email"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !emailRe.MatchString(email) {
		return Err(400, "invalid_email", "A valid email address is required.")
	}
	code := randDigits(6)
	now := store.Now()
	if _, err := s.DB.Exec(`INSERT INTO email_codes(email,code_hash,created_at,expires_at,attempts) VALUES(?,?,?,?,0)
		ON CONFLICT(email) DO UPDATE SET code_hash=excluded.code_hash, created_at=excluded.created_at, expires_at=excluded.expires_at, attempts=0`,
		email, hashToken(email+":"+code), now, now+10*60_000); err != nil {
		return err
	}
	s.Log.Info("email login code", "email", email, "code", code)
	s.sendMail(email, "Your CrowdVM login code", "Your CrowdVM login code is "+code+". It expires in 10 minutes.")
	return c.OK(200, "auth.email_code_sent", map[string]any{"email": email, "expiresInSeconds": 600})
}

func (s *Server) handleEmailVerify(c *Ctx) error {
	var req struct {
		Email      string `json:"email"`
		Code       string `json:"code"`
		Username   string `json:"username"`
		InviteCode string `json:"inviteCode"`
		Client     string `json:"client"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	var hash string
	var exp int64
	var attempts int
	err := s.DB.QueryRow(`SELECT code_hash,expires_at,attempts FROM email_codes WHERE email=?`, email).Scan(&hash, &exp, &attempts)
	if err != nil || exp < store.Now() || attempts >= 5 {
		return Err(401, "invalid_code", "The code is wrong or expired. Request a new one.")
	}
	if hashToken(email+":"+strings.TrimSpace(req.Code)) != hash {
		_, _ = s.DB.Exec(`UPDATE email_codes SET attempts=attempts+1 WHERE email=?`, email)
		return Err(401, "invalid_code", "The code is wrong or expired. Request a new one.")
	}
	_, _ = s.DB.Exec(`DELETE FROM email_codes WHERE email=?`, email)
	u, err := scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE email=?`, email))
	if err != nil {
		first := s.userCount() == 0
		if err := s.checkSignup(first, req.InviteCode); err != nil {
			return err
		}
		name := req.Username
		if name == "" {
			name = s.freeUsername(strings.Split(email, "@")[0])
		}
		u, err = s.CreateUser(NewUserParams{Username: name, Email: email, Admin: first, ConnectionMethod: "email"})
		if err != nil {
			return err
		}
		s.useInvite(req.InviteCode, first)
	}
	if u.Disabled {
		return Err(403, "account_suspended", "This account is suspended.")
	}
	return c.startLogin(u, req.Client, "auth.login")
}

var nonUser = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (s *Server) freeUsername(base string) string {
	base = nonUser.ReplaceAllString(base, "-")
	base = strings.Trim(base, ".-_")
	if len(base) < 2 {
		base = "user"
	}
	if len(base) > 30 {
		base = base[:30]
	}
	name := base
	for i := 2; ; i++ {
		var n int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE username=?`, name).Scan(&n)
		if n == 0 {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// sendMail sends via SMTP when configured (best effort, async).
func (s *Server) sendMail(to, subject, body string) {
	if s.Hooks.Mail != nil {
		s.Hooks.Mail(to, subject, body)
	}
	c := s.Cfg.SMTP
	if c.Addr == "" {
		return
	}
	go func() {
		host, _, _ := strings.Cut(c.Addr, ":")
		var auth smtp.Auth
		if c.Username != "" {
			auth = smtp.PlainAuth("", c.Username, c.Password, host)
		}
		from := c.From
		if from == "" {
			from = "crowdvm@" + host
		}
		msg := "From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
		if err := smtp.SendMail(c.Addr, auth, from, []string{to}, []byte(msg)); err != nil {
			s.Log.Warn("smtp send failed", "to", to, "err", err)
		}
	}()
}

// userJSON is the Boat MeResponse.user shape plus CrowdVM fields.
func (s *Server) userJSON(u *User) map[string]any {
	status := "active"
	if u.Disabled {
		status = "suspended"
	}
	methods := []string{u.ConnectionMethod}
	if u.passwordHash != "" && u.ConnectionMethod != "password" {
		methods = append(methods, "password")
	}
	m := map[string]any{
		"id": u.ID, "login": u.Username, "username": u.Username, "email": nz(u.Email),
		"displayName": u.DisplayName, "connectionMethod": u.ConnectionMethod, "connectionMethods": methods,
		"accountPlan": "user", "serviceAccount": false, "accountStatus": status, "suspended": u.Disabled,
		"suspendedAt": nil, "suspendedReason": nil, "closed": false, "closedAt": nil, "purgeAt": nil, "purgedAt": nil,
		"zeroDataRetention": false, "zeroDataRetentionEnabledAt": nil,
		"isAdmin": u.IsAdmin, "kind": u.Kind, "claimed": u.Kind != "agent" || u.ClaimedBy != "",
		"activeOrg": nz(u.ActiveOrg), "createdAt": ts(u.CreatedAt),
	}
	for _, fn := range s.Hooks.UserJSON {
		fn(u, m)
	}
	return m
}
