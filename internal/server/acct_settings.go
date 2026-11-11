package server

// Account settings and account kinds:
//
//   - GET/PATCH /account-settings: default harness/model/fast for prompts
//     (AGT-004/006), the onboarding answer "own work" vs "platform for third
//     parties" (AUTH-009), the notification address (AUTH-005).
//   - Connections (AUTH-005): GET /account/connections, DELETE
//     /account/connections/{method} (at least one remains), PUT
//     /account/connections/primary.
//   - Service accounts (AUTH-022): org owners/admins create service users
//     (kind "service", connectionMethod "service") that act only through
//     organization API keys; admins can create them too.
//   - Admin account merge (AUTH-006): POST /admin/users/{userId}/merge.

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

func init() {
	store.Register("acct_settings", store.Migration{Version: 1, SQL: `
CREATE TABLE account_settings (
  user_id TEXT PRIMARY KEY,
  data TEXT NOT NULL DEFAULT '{}',
  updated_at INTEGER NOT NULL
);
`})
	RegisterFeature("acct_settings", func(s *Server) error {
		s.API("GET /account-settings", "account.read", s.handleGetAccountSettings)
		s.API("PATCH /account-settings", "account.admin", s.handlePatchAccountSettings)
		s.API("GET /account/connections", "account.read", s.handleConnections)
		s.API("DELETE /account/connections/{method}", ActionSession, s.handleRemoveConnection)
		s.API("PUT /account/connections/primary", ActionSession, s.handlePrimaryConnection)
		s.API("GET /orgs/{org}/service-accounts", "account.read", s.handleListServiceAccounts)
		s.API("POST /orgs/{org}/service-accounts", "account.admin", s.handleCreateServiceAccount)
		s.API("DELETE /orgs/{org}/service-accounts/{userId}", "account.admin", s.handleDeleteServiceAccount)
		s.API("POST /admin/service-accounts", ActionAdmin, s.handleAdminCreateServiceAccount)
		s.API("POST /admin/users/{userId}/merge", ActionAdmin, s.handleAdminMerge)
		s.Hooks.UserJSON = append(s.Hooks.UserJSON, s.acctUserJSON)
		return nil
	})
}

// acctSettings is the JSON in account_settings.data.
type acctSettings struct {
	DefaultProvider string `json:"defaultProvider,omitempty"`
	DefaultModel    string `json:"defaultModel,omitempty"`
	FastDefault     bool   `json:"fastDefault,omitempty"`
	// UseCase: "own" (only I use my sandboxes) or "platform" (I run
	// sandboxes for third parties: the safe default environment).
	UseCase     string `json:"useCase,omitempty"`
	NotifyEmail string `json:"notifyEmail,omitempty"`
}

func (s *Server) accountSettings(uid string) acctSettings {
	var raw string
	var st acctSettings
	if s.DB.QueryRow(`SELECT data FROM account_settings WHERE user_id=?`, uid).Scan(&raw) == nil {
		_ = json.Unmarshal([]byte(raw), &st)
	}
	return st
}

func (s *Server) saveAccountSettings(uid string, st acctSettings) error {
	_, err := s.DB.Exec(`INSERT INTO account_settings(user_id,data,updated_at) VALUES(?,?,?) ON CONFLICT(user_id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
		uid, mustJSON(st), store.Now())
	return err
}

func (st acctSettings) json() map[string]any {
	return map[string]any{
		"agentDefaults": map[string]any{"provider": nz(st.DefaultProvider), "model": nz(st.DefaultModel), "fast": st.FastDefault},
		"useCase":       nz(st.UseCase), "onboarded": st.UseCase != "", "notifyEmail": nz(st.NotifyEmail),
		"safeForThirdPartiesDefault": st.UseCase != "own",
	}
}

func (s *Server) acctUserJSON(u *User, m map[string]any) {
	st := s.accountSettings(u.ID)
	if u.Kind == "service" {
		m["serviceAccount"], m["accountPlan"] = true, "service"
	}
	m["useCase"] = nz(st.UseCase)
	m["agentDefaults"] = st.json()["agentDefaults"]
	m["notificationEmail"] = nz(firstNonEmpty(st.NotifyEmail, u.Email))
}

func (s *Server) handleGetAccountSettings(c *Ctx) error {
	return c.OK(200, "account.settings", s.accountSettings(c.P.User.ID).json())
}

func (s *Server) handlePatchAccountSettings(c *Ctx) error {
	var req struct {
		AgentDefaults *struct {
			Provider *string `json:"provider"`
			Model    *string `json:"model"`
			Fast     *bool   `json:"fast"`
		} `json:"agentDefaults"`
		UseCase     *string `json:"useCase"`
		NotifyEmail *string `json:"notifyEmail"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	st := s.accountSettings(c.P.User.ID)
	if d := req.AgentDefaults; d != nil {
		if d.Provider != nil {
			st.DefaultProvider = ""
			if v := strings.TrimSpace(*d.Provider); v != "" {
				p, ok := s.catalogProvider(v)
				if !ok {
					return ErrD(400, "unknown_provider", "Unknown harness "+v+". See GET /provider-models.", map[string]any{"provider": v})
				}
				st.DefaultProvider = p.Provider
			}
			if d.Model == nil {
				st.DefaultModel = "" // a new harness drops the old model
			}
		}
		if d.Model != nil {
			st.DefaultModel = strings.TrimSpace(*d.Model)
			if st.DefaultModel != "" {
				p, ok := s.catalogProvider(firstNonEmpty(st.DefaultProvider, "claude"))
				if ok {
					if _, ok := p.model(st.DefaultModel); !ok {
						return ErrD(400, "unknown_model", "Model "+st.DefaultModel+" is not in the catalog of "+p.Provider+".", map[string]any{"provider": p.Provider})
					}
				}
			}
		}
		if d.Fast != nil {
			st.FastDefault = *d.Fast
		}
	}
	if req.UseCase != nil {
		switch v := strings.TrimSpace(*req.UseCase); v {
		case "", "own", "platform":
			st.UseCase = v
		default:
			return Err(400, "invalid_use_case", "useCase must be own (only you use your sandboxes) or platform (sandboxes for third parties).")
		}
	}
	if req.NotifyEmail != nil {
		e := strings.ToLower(strings.TrimSpace(*req.NotifyEmail))
		if e != "" && !emailRe.MatchString(e) {
			return Err(400, "invalid_email", "notifyEmail must be an email address.")
		}
		st.NotifyEmail = e
	}
	if err := s.saveAccountSettings(c.P.User.ID, st); err != nil {
		return err
	}
	if req.UseCase != nil {
		s.applyUseCase(c.P.User.ID, st.UseCase)
	}
	return c.OK(200, "account.settings", st.json())
}

// notifyAddress is where account notices are mailed ("" = none).
func (s *Server) notifyAddress(uid string) string {
	u, err := s.UserByID(uid)
	if err != nil {
		return ""
	}
	return firstNonEmpty(s.accountSettings(uid).NotifyEmail, u.Email)
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

func (s *Server) connections(u *User) []map[string]any {
	out := []map[string]any{}
	if u.passwordHash != "" {
		out = append(out, map[string]any{"method": "password", "login": u.Username, "primary": u.ConnectionMethod == "password"})
	}
	if u.Email != "" {
		out = append(out, map[string]any{"method": "email", "address": u.Email, "primary": u.ConnectionMethod == "email"})
	}
	return out
}

func (s *Server) handleConnections(c *Ctx) error {
	u := c.P.User
	return c.OK(200, "account.connections", map[string]any{"connections": s.connections(u), "primary": u.ConnectionMethod,
		"notificationEmail": nz(firstNonEmpty(s.accountSettings(u.ID).NotifyEmail, u.Email))})
}

func (s *Server) handleRemoveConnection(c *Ctx) error {
	u := c.P.User
	m := c.Param("method")
	conns := s.connections(u)
	has := false
	for _, x := range conns {
		has = has || x["method"] == m
	}
	if !has {
		return Err(404, "connection_not_found", "This account has no "+m+" connection.")
	}
	if len(conns) < 2 {
		return Err(409, "last_connection", "An account keeps at least one way to sign in. Add another connection first.")
	}
	other := "email"
	switch m {
	case "password":
		_, _ = s.DB.Exec(`UPDATE users SET password_hash='' WHERE id=?`, u.ID)
	case "email":
		_, _ = s.DB.Exec(`UPDATE users SET email=NULL WHERE id=?`, u.ID)
		other = "password"
	}
	if u.ConnectionMethod == m {
		_, _ = s.DB.Exec(`UPDATE users SET connection_method=? WHERE id=?`, other, u.ID)
	}
	u, _ = s.UserByID(u.ID)
	return c.OK(200, "account.connections", map[string]any{"connections": s.connections(u), "primary": u.ConnectionMethod})
}

func (s *Server) handlePrimaryConnection(c *Ctx) error {
	var req struct {
		Method string `json:"method"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	u := c.P.User
	ok := false
	for _, x := range s.connections(u) {
		ok = ok || x["method"] == req.Method
	}
	if !ok {
		return Err(400, "connection_not_found", "Connect "+req.Method+" first.")
	}
	_, _ = s.DB.Exec(`UPDATE users SET connection_method=? WHERE id=?`, req.Method, u.ID)
	u, _ = s.UserByID(u.ID)
	return c.OK(200, "account.connections", map[string]any{"connections": s.connections(u), "primary": u.ConnectionMethod})
}

// ---------------------------------------------------------------------------
// Service accounts
// ---------------------------------------------------------------------------

func (s *Server) orgForManage(c *Ctx) (*Org, error) {
	o, err := c.paramOrg()
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, Err(400, "org_required", "Name an organization.")
	}
	if r := s.memberRole(o.ID, c.P.User.ID); r != "owner" && r != "admin" && !c.P.IsAdmin() {
		return nil, Err(403, "forbidden", "Only owners and admins manage service accounts.")
	}
	return o, nil
}

// createServiceAccount makes a service user, adds it to org (when set) and
// returns it with an API key (an org key when org is set).
func (s *Server) createServiceAccount(p *Principal, name string, org *Org, preset string, ttl string) (*User, map[string]any, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil, "", Err(400, "name_required", "name is required.")
	}
	// Preset and TTL are checked before anything is created (bugs-r3-opus
	// 18), and the key may not exceed the caller's own (bugs-r3-opus 11).
	if preset == "" {
		preset = "ci"
	}
	acts, ok := Presets[preset]
	if !ok {
		return nil, nil, "", Err(400, "invalid_preset", "preset must be read-only, full-sandbox, ci or admin.")
	}
	d, never, err := parseTTL(ttl)
	if err != nil {
		return nil, nil, "", err
	}
	var exp *time.Time
	if !never {
		t := time.Now().Add(d).UTC()
		exp = &t
	}
	if !covers(p, acts, nil, nil, exp) {
		return nil, nil, "", Err(403, "api_key_scope_exceeded", "A service account's key cannot have more actions or a later expiry than the key that creates it.")
	}
	zero := 0.0
	u, err := s.CreateUser(NewUserParams{Username: s.freeUsername("svc-" + name), DisplayName: name, Kind: "service", ConnectionMethod: "service", Credits: &zero})
	if err != nil {
		return nil, nil, "", err
	}
	orgID := ""
	if org != nil {
		orgID = org.ID
		if _, err := s.DB.Exec(`INSERT INTO org_members(org_id,user_id,role,joined_at) VALUES(?,?,?,?)`, org.ID, u.ID, "member", store.Now()); err != nil {
			return nil, nil, "", err
		}
		_, _ = s.DB.Exec(`UPDATE users SET active_org=?, active_wallet_chosen=1 WHERE id=?`, org.ID, u.ID)
	}
	k, secret, err := s.createKey(u, keySpec{Name: "service account " + name, Preset: preset, OrgID: orgID, ExpiresAt: exp})
	if err != nil {
		return nil, nil, "", err
	}
	return u, s.keyJSON(k), secret, nil
}

func (s *Server) handleCreateServiceAccount(c *Ctx) error {
	o, err := s.orgForManage(c)
	if err != nil {
		return err
	}
	var req struct {
		Name   string `json:"name"`
		Preset string `json:"preset"`
		TTL    string `json:"ttl"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	u, key, secret, err := s.createServiceAccount(c.P, req.Name, o, req.Preset, req.TTL)
	if err != nil {
		return err
	}
	return c.OK(201, "service_account.created", map[string]any{"user": s.userJSON(u), "apiKey": key, "secret": secret, "teamId": o.ID})
}

func (s *Server) handleAdminCreateServiceAccount(c *Ctx) error {
	var req struct {
		Name   string `json:"name"`
		Org    string `json:"org"`
		Preset string `json:"preset"`
		TTL    string `json:"ttl"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	var org *Org
	if req.Org != "" {
		var id string
		if s.DB.QueryRow(`SELECT id FROM orgs WHERE id=? OR name=? COLLATE NOCASE LIMIT 1`, req.Org, req.Org).Scan(&id) != nil {
			return errNotFound("Organization")
		}
		org, _ = s.OrgByID(id)
	}
	u, key, secret, err := s.createServiceAccount(c.P, req.Name, org, req.Preset, req.TTL)
	if err != nil {
		return err
	}
	return c.OK(201, "service_account.created", map[string]any{"user": s.userJSON(u), "apiKey": key, "secret": secret})
}

func (s *Server) handleListServiceAccounts(c *Ctx) error {
	o, err := c.paramOrg()
	if err != nil {
		return err
	}
	if o == nil {
		return Err(400, "org_required", "Name an organization.")
	}
	rows, err := s.DB.Query(`SELECT `+userCols+` FROM users WHERE kind='service' AND id IN (SELECT user_id FROM org_members WHERE org_id=?) ORDER BY created_at`, o.ID)
	if err != nil {
		return err
	}
	var users []*User
	for rows.Next() {
		if u, err := scanUser(rows); err == nil {
			users = append(users, u)
		}
	}
	rows.Close()
	out := []map[string]any{}
	for _, u := range users {
		m := s.userJSON(u)
		keys, _ := s.userKeys(u.ID, false)
		kl := []map[string]any{}
		for _, k := range keys {
			kl = append(kl, s.keyJSON(k))
		}
		m["apiKeys"] = kl
		out = append(out, m)
	}
	return c.OK(200, "service_account.list", map[string]any{"serviceAccounts": out, "teamId": o.ID})
}

func (s *Server) handleDeleteServiceAccount(c *Ctx) error {
	o, err := s.orgForManage(c)
	if err != nil {
		return err
	}
	u, err := s.UserByID(c.Param("userId"))
	if err != nil || u.Kind != "service" || s.memberRole(o.ID, u.ID) == "" {
		return errNotFound("Service account")
	}
	// Keep the service account usable until all its sandboxes are deleted,
	// including stopped ones, so deleting it cannot orphan disks or snapshots.
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE owner_id=? AND deleted_at IS NULL`, u.ID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return Err(409, "member_has_sandboxes", "Delete the service account's sandboxes first.")
	}
	now := store.Now()
	_, _ = s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL`, now, u.ID)
	_, _ = s.DB.Exec(`DELETE FROM org_members WHERE org_id=? AND user_id=?`, o.ID, u.ID)
	_, _ = s.DB.Exec(`UPDATE users SET disabled=1 WHERE id=?`, u.ID)
	return c.OK(200, "service_account.deleted", map[string]any{"id": u.ID})
}

// ---------------------------------------------------------------------------
// Admin account merge
// ---------------------------------------------------------------------------

// mergeMoves are (table, column) pairs that name an account.
var mergeMoves = [][2]string{
	{"sandboxes", "owner_id"}, {"snapshots", "owner_id"}, {"saved_snapshots", "owner_id"}, {"deletion_ops", "owner_id"},
	{"api_keys", "user_id"}, {"nodes", "owner_id"}, {"join_tokens", "owner_id"}, {"orgs", "owner_id"},
	{"environments", "owner_id"}, {"git_sources", "owner_id"}, {"git_repos", "owner_id"}, {"ci_links", "owner_id"},
	{"ci_jobs", "owner_id"}, {"webhooks", "owner_id"}, {"sandbox_runs", "owner_id"}, {"org_members", "user_id"},
	{"sandbox_ssh_keys", "user_id"},
}

func (s *Server) handleAdminMerge(c *Ctx) error {
	from, err := s.UserByLogin(c.Param("userId"))
	if err != nil {
		return errNotFound("User")
	}
	var req struct {
		Into string `json:"into"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	into, err := s.UserByLogin(req.Into)
	if err != nil {
		return Err(404, "not_found", "Target user not found (into).")
	}
	if into.ID == from.ID {
		return Err(400, "same_user", "into must be another account.")
	}
	moved := map[string]int64{}
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		for _, m := range mergeMoves {
			res, err := tx.Exec(`UPDATE OR IGNORE `+m[0]+` SET `+m[1]+`=? WHERE `+m[1]+`=?`, into.ID, from.ID)
			if err != nil {
				if strings.Contains(err.Error(), "no such table") {
					continue
				}
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				moved[m[0]] = n
			}
		}
		// Personal credential profiles move but stop being a default.
		if res, err := tx.Exec(`UPDATE credential_profiles SET owner_id=?, is_default=0 WHERE owner_id=? AND org=0`, into.ID, from.ID); err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				moved["credential_profiles"] = n
			}
		}
		_, _ = tx.Exec(`UPDATE sandbox_runs SET wallet_id=? WHERE wallet_id=? AND ended_at IS NULL`, into.ID, from.ID)
		var bal float64
		_ = tx.QueryRow(`SELECT balance FROM wallets WHERE id=?`, from.ID).Scan(&bal)
		if bal != 0 {
			if err := walletAdd(tx, from.ID, -bal, "transfer", "merged into "+into.ID, "", "", c.P.User.ID); err != nil {
				return err
			}
			if err := walletAdd(tx, into.ID, bal, "transfer", "merged from "+from.ID, "", "", c.P.User.ID); err != nil {
				return err
			}
		}
		if into.Email == "" && from.Email != "" {
			if _, err := tx.Exec(`UPDATE users SET email=NULL WHERE id=?`, from.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE users SET email=? WHERE id=?`, from.Email, into.ID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE users SET disabled=1 WHERE id=?`, from.ID)
		if err == nil {
			_, err = tx.Exec(`DELETE FROM sessions WHERE user_id=?`, from.ID)
		}
		return err
	})
	if err != nil {
		return err
	}
	into, _ = s.UserByID(into.ID)
	s.audit(c, "account.merged", from.ID, map[string]any{"into": into.ID, "moved": moved})
	return c.OK(200, "admin.user_merged", map[string]any{"user": s.userRow(into), "mergedUserId": from.ID, "moved": moved})
}
