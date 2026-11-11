package server

// Smaller account features:
//
//   - One-time dashboard login links (CLI-111, DASH-001): POST
//     /api/v1/auth/login-links (CLI session) -> /auth/link/{token}, one use,
//     15 minutes; the token cannot call the API.
//   - Feedback (CLI-023): POST /api/v1/feedback (5/min, 30/day), GET
//     /api/v1/admin/feedback; mailed to admins when SMTP is set.
//   - Move personal sandboxes into an org (ORG-022): POST
//     /api/v1/orgs/{org}/move-personal, Boat alias POST
//     /api/boat/billing/move-to-org {teamId}.
//   - Well-known agent-auth documents under /api/boat/ and /api/crowdvm/
//     (AUTH-011).
//   - Admin server settings (KEY-007): GET|PATCH /api/v1/admin/settings
//     {scopedKeyCreationEnabled}; when off, key creation answers 503
//     api_key_creation_disabled.
//   - The onboarding answer (AUTH-009) sets "safe for third parties" on the
//     account's default environment when it was never answered.

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

const loginLinkTTL = 15 * time.Minute

func init() {
	store.Register("acct_misc", store.Migration{Version: 1, SQL: `
CREATE TABLE login_links (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at INTEGER
);
CREATE TABLE feedback (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  text TEXT NOT NULL,
  sandbox_id TEXT,
  client TEXT,
  created_at INTEGER NOT NULL
);
CREATE INDEX feedback_user ON feedback(user_id, created_at);
`})
	RegisterFeature("acct_misc", func(s *Server) error {
		s.API("POST /auth/login-links", ActionSession, s.handleCreateLoginLink)
		s.router.HandleFunc("GET /auth/link/{token}", s.handleLoginLink)
		s.API("POST /feedback", ActionAny, s.handleFeedback)
		s.API("GET /admin/feedback", ActionAdmin, s.handleAdminFeedback)
		s.API("POST /orgs/{org}/move-personal", "account.admin", s.handleMovePersonal)
		s.Route("POST /api/boat/billing/move-to-org", "account.admin", s.handleMovePersonal)
		s.Route("POST /api/crowdvm/billing/move-to-org", "account.admin", s.handleMovePersonal)
		for _, pre := range []string{"/api/boat", "/api/crowdvm"} {
			s.router.HandleFunc("GET "+pre+"/.well-known/oauth-protected-resource", s.handleWellKnownResource)
			s.router.HandleFunc("GET "+pre+"/.well-known/oauth-authorization-server", s.handleWellKnownAS)
			s.router.HandleFunc("GET "+pre+"/auth.md", s.handleAuthMD)
		}
		s.API("GET /admin/settings", ActionAdmin, s.handleAdminSettings)
		s.API("PATCH /admin/settings", ActionAdmin, s.handleAdminSettingsPatch)
		s.Hooks.Guard = append(s.Hooks.Guard, s.keyCreationGuard)
		s.Go(func(ctx context.Context) {
			every(ctx, time.Hour, func() {
				_, _ = s.DB.Exec(`DELETE FROM login_links WHERE expires_at < ?`, store.Now()-24*3600_000)
			})
		})
		return nil
	})
}

// ---------------------------------------------------------------------------
// Login links
// ---------------------------------------------------------------------------

func (s *Server) handleCreateLoginLink(c *Ctx) error {
	tok := "cvml_" + randToken(40)
	now := store.Now()
	exp := now + loginLinkTTL.Milliseconds()
	if _, err := s.DB.Exec(`INSERT INTO login_links(token_hash,user_id,created_at,expires_at) VALUES(?,?,?,?)`, hashToken(tok), c.P.User.ID, now, exp); err != nil {
		return err
	}
	return c.OK(201, "auth.login_link", map[string]any{"url": s.Cfg.PublicURL + "/auth/link/" + tok, "expiresAt": tsMs(exp),
		"expiresInSeconds": int(loginLinkTTL.Seconds())})
}

func (s *Server) handleLoginLink(w http.ResponseWriter, r *http.Request) {
	h := hashToken(r.PathValue("token"))
	var uid string
	var exp int64
	var used sql.NullInt64
	now := store.Now()
	err := s.DB.QueryRow(`SELECT user_id,expires_at,used_at FROM login_links WHERE token_hash=?`, h).Scan(&uid, &exp, &used)
	if err != nil || used.Valid || exp < now {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>CrowdVM</title><p>This login link is used or expired. Run <code>crowdvm dashboard</code> again, or <a href="/">sign in</a>.</p>`))
		return
	}
	res, _ := s.DB.Exec(`UPDATE login_links SET used_at=? WHERE token_hash=? AND used_at IS NULL`, now, h)
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "link already used", http.StatusGone)
		return
	}
	u, err := s.UserByID(uid)
	if err != nil || u.Disabled {
		http.Error(w, "account unavailable", http.StatusForbidden)
		return
	}
	tok, err := s.NewSession(uid, sessionBrowser, r.UserAgent())
	if err != nil {
		http.Error(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	s.setSessionCookie(w, tok)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/", http.StatusFound)
}

// ---------------------------------------------------------------------------
// Feedback
// ---------------------------------------------------------------------------

func (s *Server) handleFeedback(c *Ctx) error {
	var req struct {
		Text      string `json:"text"`
		Message   string `json:"message"`
		SandboxID string `json:"sandboxId"`
		Client    string `json:"client"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	text := strings.TrimSpace(firstNonEmpty(req.Text, req.Message))
	if text == "" {
		return Err(400, "text_required", "text is required.")
	}
	if len(text) > 20000 {
		return Err(400, "text_too_long", "Feedback can be at most 20000 characters.")
	}
	now := store.Now()
	for _, lim := range []struct {
		n   int
		dur time.Duration
		w   string
	}{{5, time.Minute, "minute"}, {30, 24 * time.Hour, "day"}} {
		var n int
		var oldest int64
		_ = s.DB.QueryRow(`SELECT COUNT(*), COALESCE(MIN(created_at),0) FROM feedback WHERE user_id=? AND created_at>?`, c.P.User.ID, now-lim.dur.Milliseconds()).Scan(&n, &oldest)
		if n >= lim.n {
			retry := int((oldest+lim.dur.Milliseconds()-now)/1000) + 1
			return ErrD(429, "rate_limited", "Too much feedback: at most 5 per minute and 30 per day.", map[string]any{"window": lim.w, "limit": lim.n, "retryAfterSeconds": retry})
		}
	}
	id := "fb_" + randToken(12)
	if _, err := s.DB.Exec(`INSERT INTO feedback(id,user_id,text,sandbox_id,client,created_at) VALUES(?,?,?,?,?,?)`,
		id, c.P.User.ID, text, nz(req.SandboxID), nz(firstNonEmpty(req.Client, c.R.UserAgent())), now); err != nil {
		return err
	}
	for _, addr := range s.walletIDs(`SELECT email FROM users WHERE is_admin=1 AND disabled=0 AND email IS NOT NULL`) {
		s.sendMail(addr, "[CrowdVM] Feedback from "+c.P.User.Username, text)
	}
	return c.OK(201, "feedback.created", map[string]any{"feedback": map[string]any{"id": id, "createdAt": tsMs(now)}})
}

func (s *Server) handleAdminFeedback(c *Ctx) error {
	rows, err := s.DB.Query(`SELECT f.id,f.user_id,COALESCE(u.username,''),f.text,COALESCE(f.sandbox_id,''),COALESCE(f.client,''),f.created_at
		FROM feedback f LEFT JOIN users u ON u.id=f.user_id ORDER BY f.created_at DESC LIMIT 500`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, uid, login, text, sb, client string
		var at int64
		if rows.Scan(&id, &uid, &login, &text, &sb, &client, &at) == nil {
			out = append(out, map[string]any{"id": id, "userId": uid, "login": login, "text": text, "sandboxId": nz(sb), "client": nz(client), "createdAt": tsMs(at)})
		}
	}
	return c.OK(200, "admin.feedback", map[string]any{"feedback": out})
}

// ---------------------------------------------------------------------------
// Move personal sandboxes into an organization
// ---------------------------------------------------------------------------

func (s *Server) handleMovePersonal(c *Ctx) error {
	var req struct {
		TeamID          string `json:"teamId"`
		Org             string `json:"org"`
		TransferCredits any    `json:"transferCredits"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	ref := firstNonEmpty(c.Param("org"), req.TeamID, req.Org)
	if ref == "" {
		return Err(400, "org_required", "Name the organization (teamId).")
	}
	o, err := s.resolveOrgRef(c.P.User, ref)
	if err != nil {
		return err
	}
	if o == nil {
		return Err(400, "org_required", "Name an organization, not Personal.")
	}
	if k := c.P.Key; k != nil && k.OrgID != "" && o.ID != k.OrgID {
		return Err(403, "api_key_org_mismatch", "This organization key works only for its own organization.")
	}
	if s.memberRole(o.ID, c.P.User.ID) != "owner" {
		return Err(403, "owner_only", "Only the owner of the organization can move personal sandboxes into it.")
	}
	uid := c.P.User.ID
	var amount float64
	all := false
	switch v := req.TransferCredits.(type) {
	case nil:
	case string:
		if v != "all" {
			return Err(400, "invalid_amount", `transferCredits must be a number or "all".`)
		}
		all = true // the balance is read in the transaction
	case float64:
		if v < 0 || math.IsNaN(v) {
			return Err(400, "invalid_amount", "transferCredits must be positive.")
		}
		if round6(v) > round6(s.Balance(uid)) { // the shown (rounded) balance is fine
			return Err(402, "insufficient_credits", "Your personal wallet does not have that many credits.")
		}
		amount = v
	default:
		return Err(400, "invalid_amount", `transferCredits must be a number or "all".`)
	}
	ids := s.walletIDs(`SELECT id FROM sandboxes WHERE owner_id=? AND org_id IS NULL AND deleted_at IS NULL`, uid)
	var snaps int64
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			// Same rule as create: the org default applies, and a sandbox holding the
			// creator's logins stays private to them.
			if _, err := tx.Exec(`UPDATE sandboxes SET org_id=?, private_by_creator=?,
				visibility=CASE WHEN ?=1 OR holds_creator_logins=1 THEN 'private' ELSE 'shared' END, updated_at=? WHERE id=?`,
				o.ID, boolInt(o.NewSandboxesPrivate), boolInt(o.NewSandboxesPrivate), store.Now(), id); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE sandbox_runs SET wallet_id=? WHERE sandbox_id=? AND ended_at IS NULL`, o.ID, id); err != nil {
				return err
			}
		}
		if res, err := tx.Exec(`UPDATE saved_snapshots SET org_id=? WHERE owner_id=? AND org_id IS NULL`, o.ID, uid); err == nil {
			snaps, _ = res.RowsAffected()
		}
		if all {
			var bal float64
			_ = tx.QueryRow(`SELECT balance FROM wallets WHERE id=?`, uid).Scan(&bal)
			amount = math.Max(0, bal)
		}
		if amount > 0 {
			took, err := walletDebit(tx, uid, amount, "moved to "+o.ID, uid, Err(402, "insufficient_credits", "Your personal wallet does not have that many credits."))
			if err != nil {
				return err
			}
			amount = took
			return walletAdd(tx, o.ID, amount, "transfer", "moved from "+uid, "", "", uid)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if sb, err := s.SandboxByID(id); err == nil {
			s.Bus.Publish(Event{Type: EvSandboxUpdated, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, State: sb.State, Reason: "moved_to_org"})
		}
	}
	s.audit(c, "org.personal_moved", o.ID, map[string]any{"sandboxes": len(ids), "credits": amount})
	return c.OK(200, "org.personal_moved", map[string]any{"teamId": o.ID, "moved": map[string]any{"sandboxes": len(ids), "savedSnapshots": snaps},
		"transferredCredits": round6(amount), "wallet": s.walletJSON(o.ID)})
}

// ---------------------------------------------------------------------------
// Admin settings (scoped key creation switch)
// ---------------------------------------------------------------------------

func (s *Server) scopedKeysEnabled() bool { return s.getSetting("api_keys.creation_disabled") == "" }

func (s *Server) adminSettingsJSON() map[string]any {
	return map[string]any{"scopedKeyCreationEnabled": s.scopedKeysEnabled(), "providerCatalogSource": s.catalogJSON()["source"]}
}

func (s *Server) handleAdminSettings(c *Ctx) error {
	return c.OK(200, "admin.settings", s.adminSettingsJSON())
}

func (s *Server) handleAdminSettingsPatch(c *Ctx) error {
	var req struct {
		ScopedKeyCreationEnabled *bool `json:"scopedKeyCreationEnabled"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	if req.ScopedKeyCreationEnabled != nil {
		v := ""
		if !*req.ScopedKeyCreationEnabled {
			v = "1"
		}
		if err := s.putSetting("api_keys.creation_disabled", v); err != nil {
			return err
		}
	}
	return c.OK(200, "admin.settings", s.adminSettingsJSON())
}

// keyCreationGuard answers 503 when an admin switched key creation off.
func (s *Server) keyCreationGuard(c *Ctx) error {
	switch normPattern(c.R.Pattern) {
	case "POST /api/v1/api-keys", "POST /api/v1/api-keys/scoped":
		if !s.scopedKeysEnabled() {
			return ErrD(503, "api_key_creation_disabled", "An administrator turned API key creation off on this server. Existing keys keep working.",
				map[string]any{"retryable": false})
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Onboarding answer -> default environment (AUTH-009)
// ---------------------------------------------------------------------------

// applyUseCase makes the account's default personal environment safe for
// third parties ("platform") or not ("own"), unless that question was
// already answered on the environment.
func (s *Server) applyUseCase(uid, useCase string) {
	if useCase == "" {
		return
	}
	e, err := s.envDefault(uid, false)
	if err != nil || e == nil || e.SafetyAnsweredAt != nil {
		return
	}
	v, err := s.envLatest(e)
	if err != nil {
		return
	}
	cfg := v.Config
	cfg.SafeForThirdParties = useCase == "platform"
	if mustJSON(cfg) != mustJSON(v.Config) {
		if _, err := s.envMint(e, cfg); err != nil {
			s.Log.Warn("onboarding: environment update failed", "user", uid, "err", err)
			return
		}
	}
	_, _ = s.DB.Exec(`UPDATE environments SET safety_answered_at=? WHERE id=?`, store.Now(), e.ID)
}

// keyScopeEnvironments lists the environments a key can be limited to: the
// account's and those of its organizations (KEY-020).
func (s *Server) keyScopeEnvironments(u *User) []map[string]any {
	out := []map[string]any{}
	rows, err := s.DB.Query(`SELECT e.id, e.name, e.org, e.owner_id, COALESCE(o.name,'') FROM environments e LEFT JOIN orgs o ON o.id=e.owner_id
		WHERE e.deleted_at IS NULL AND (e.owner_id=? OR e.owner_id IN (SELECT org_id FROM org_members WHERE user_id=?)) ORDER BY e.org, e.name LIMIT 500`, u.ID, u.ID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, owner, oname string
		var org bool
		if rows.Scan(&id, &name, &org, &owner, &oname) == nil {
			m := map[string]any{"id": id, "name": name, "organizationId": nil, "organizationName": nil}
			if org {
				m["organizationId"], m["organizationName"] = owner, oname
			}
			out = append(out, m)
		}
	}
	return out
}
