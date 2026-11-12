package server

import (
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/compat"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Version is the server version (set by the build).
var Version = "dev"

func (s *Server) handleMe(c *Ctx) error {
	return c.OK(200, "user.info", map[string]any{"user": s.userJSON(c.P.User)})
}

func (s *Server) handleLimits(c *Ctx) error {
	org, err := c.Scope("")
	if err != nil {
		return err
	}
	u := c.P.User
	w := walletOf(u, org)
	active := s.activeCount(w)
	maxActive := s.maxActiveFor(w)
	starts := s.startUsage(w)
	wins := s.startWindowsFor(w)
	canStart, reason := true, any(nil)
	if s.Cfg.EnforceCredits && s.Balance(w) <= 0 {
		canStart, reason = false, "billing_required"
	}
	if s.checkCreditCap(w) != nil {
		canStart, reason = false, "credit_cap_reached"
	}
	if maxActive > 0 && active >= maxActive {
		canStart, reason = false, "limit_reached"
	}
	if u.Kind == "agent" && u.ClaimedBy == "" {
		canStart, reason = false, "claim_required"
	}
	for _, k := range []string{"minute", "hour", "day"} {
		if win, ok := starts[k].(map[string]any); ok && win["remaining"] == 0 {
			canStart, reason = false, "rate_limited"
		}
	}
	avail := s.sizeAvailability()
	fits := s.fitsNow()
	var types []map[string]any
	for _, sz := range api.Sizes {
		r := avail[sz.Name]
		// fitsNow: how many of this size this caller can start right now
		// (pool room capped by the account's limits; an estimate).
		n := fits[sz.Name]
		if !canStart || r != "" {
			n = 0
		} else if maxActive > 0 {
			n = min(n, maxActive-active)
		}
		types = append(types, map[string]any{"type": sz.Name, "available": r == "", "reason": nz(r), "fitsNow": n})
	}
	nullLimit := func(v int) any {
		if v <= 0 {
			return nil
		}
		return v
	}
	var startLimits any
	if wins[0].limit > 0 || wins[1].limit > 0 || wins[2].limit > 0 {
		startLimits = map[string]any{"perMinute": wins[0].limit, "perHour": wins[1].limit, "perDay": wins[2].limit}
	}
	limits := map[string]any{"activeSandboxes": maxActive, "creationRatePerMinute": wins[0].limit,
		"creationRequestsPerHour": nullLimit(wins[1].limit), "creationRequestsPerDay": nullLimit(wins[2].limit)}
	var last24 float64
	now := time.Now()
	rows, err := s.DB.Query(`SELECT DISTINCT sandbox_id FROM sandbox_runs WHERE wallet_id=? AND (ended_at IS NULL OR ended_at>?)`, w, now.Add(-24*time.Hour).UnixMilli())
	if err == nil {
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			secs, _ := s.usageSeconds(id, now.Add(-24*time.Hour).UnixMilli(), now.UnixMilli())
			last24 += secs
		}
	}
	bal := s.Balance(w)
	out := map[string]any{
		"accessTier": "crowdvm", "blockedReason": reason, "currentLimits": limits, "standardLimits": limits,
		"canStart": canStart, "checkoutRequired": false, "startBlockedReason": reason, "contactMessage": nil,
		"activeSandboxes": active, "activeStates": activeStates, "maxActiveSandboxes": maxActive,
		"maxCreationRequestsPerMinute": wins[0].limit, "maxCreationRequestsPerDay": nullLimit(wins[2].limit),
		"startLimits": startLimits, "starts": starts, "billingStatus": "active",
		"wallet":           map[string]any{"balanceDollars": round6(bal * 3600 / float64(s.Cfg.SecondsPerDollar)), "balanceCredits": round6(bal)},
		"planIncluded":     nil,
		"overagePrices":    map[string]any{"vmHourDollars": 0, "savedSnapshotDayDollars": 0, "startBrackets": []any{}},
		"nextStartDollars": 0, "startLimitsWithWallet": nil,
		"machineTypes": types, "giftLimit": nil, "unlimited": startLimits == nil && maxActive == 0,
		"accountPlan": "user", "plan": nil, "planName": nil, "serviceAccount": false, "hasSeatPlan": false,
		"last24hUsageSeconds": int(last24), "creditSecondsPerDollar": s.Cfg.SecondsPerDollar,
		"credits": map[string]any{"balance": round6(bal), "enforced": s.Cfg.EnforceCredits, "unit": "default-machine hours"},
	}
	if org != nil {
		out["teamId"] = org.ID
		out["teamRole"] = s.memberRole(org.ID, u.ID)
	}
	s.limitsPolicy(w, out) // planNudge, extra starts, grace, restriction policy (acct_billing.go)
	return c.OK(200, "limits.info", out)
}

func (s *Server) handleServerInfo(c *Ctx) error {
	nodes := s.nodes.online()
	return c.OK(200, "server.info", map[string]any{
		"version": Version, "publicUrl": s.Cfg.PublicURL, "signupOpen": s.Cfg.OpenSignup, "openNodes": s.Cfg.OpenNodes,
		"creditsEnforced": s.Cfg.EnforceCredits, "nodesOnline": len(nodes), "defaultImage": nz(s.defaultImage()),
		"sizes": api.Sizes, "snapshotIntervalSeconds": int(s.Cfg.SnapshotInterval.Seconds()),
		// Compatibility (internal/compat): the client generations this
		// server accepts, and the release its downloads offer.
		"gen": compat.ServerGen, "apiGen": compat.APIGen, "minApiGen": compat.MinAPIGen, "requiredApiGen": compat.RequiredAPIGen,
		"latest": nz(s.latestRelease()),
	})
}

// ---------------------------------------------------------------------------
// Admin API (CrowdVM extension)
// ---------------------------------------------------------------------------

func (s *Server) adminRoutes() {
	s.API("GET /admin/users", ActionAdmin, s.handleAdminUsers)
	s.API("POST /admin/users", ActionAdmin, s.handleAdminCreateUser)
	s.API("PATCH /admin/users/{userId}", ActionAdmin, s.handleAdminUpdateUser)
	s.API("POST /admin/users/{userId}/credits", ActionAdmin, s.handleAdminGrant)
	s.API("POST /admin/credits", ActionAdmin, s.handleAdminGrant)
	s.API("GET /admin/invite-codes", ActionAdmin, s.handleListInviteCodes)
	s.API("POST /admin/invite-codes", ActionAdmin, s.handleCreateInviteCode)
	s.API("DELETE /admin/invite-codes/{codeId}", ActionAdmin, s.handleRevokeInviteCode)
	s.API("GET /admin/sandboxes", ActionAdmin, s.handleAdminSandboxes)
}

// UserRow is a user summary for admin lists.
func (s *Server) userRow(u *User) map[string]any {
	m := s.userJSON(u)
	m["credits"] = round6(s.Balance(u.ID))
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE owner_id=? AND deleted_at IS NULL AND state IN ('provisioning','provisioned','cloning','ready','idle','running')`, u.ID).Scan(&n)
	m["activeSandboxes"] = n
	return m
}

// Users lists all accounts.
func (s *Server) Users() ([]*User, error) {
	rows, err := s.DB.Query(`SELECT ` + userCols + ` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Server) handleAdminUsers(c *Ctx) error {
	users, err := s.Users()
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, u := range users {
		out = append(out, s.userRow(u))
	}
	return c.OK(200, "admin.users", map[string]any{"users": out})
}

func (s *Server) handleAdminCreateUser(c *Ctx) error {
	var req struct {
		Username, Password, Email, DisplayName string
		Admin                                  bool
		Credits                                *float64
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	u, err := s.CreateUser(NewUserParams{Username: req.Username, Password: req.Password, Email: req.Email, DisplayName: req.DisplayName, Admin: req.Admin, Credits: req.Credits})
	if err != nil {
		return err
	}
	return c.OK(201, "admin.user_created", map[string]any{"user": s.userRow(u)})
}

func (s *Server) handleAdminUpdateUser(c *Ctx) error {
	u, err := s.UserByLogin(c.Param("userId"))
	if err != nil {
		return errNotFound("User")
	}
	var req struct {
		Admin       *bool   `json:"admin"`
		Disabled    *bool   `json:"disabled"`
		Password    *string `json:"password"`
		DisplayName *string `json:"displayName"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.Admin != nil {
		if !*req.Admin && u.ID == c.P.User.ID {
			return Err(400, "cannot_demote_self", "You cannot remove your own admin rights.")
		}
		_, _ = s.DB.Exec(`UPDATE users SET is_admin=? WHERE id=?`, boolInt(*req.Admin), u.ID)
	}
	if req.Disabled != nil {
		_, _ = s.DB.Exec(`UPDATE users SET disabled=? WHERE id=?`, boolInt(*req.Disabled), u.ID)
		if *req.Disabled {
			_, _ = s.DB.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID)
		}
	}
	if req.Password != nil {
		if err := s.SetPassword(u.ID, *req.Password, ""); err != nil { // bugs-r11-sonnet 6, 10
			return err
		}
	}
	if req.DisplayName != nil {
		_, _ = s.DB.Exec(`UPDATE users SET display_name=? WHERE id=?`, *req.DisplayName, u.ID)
	}
	u, _ = s.UserByID(u.ID)
	return c.OK(200, "admin.user_updated", map[string]any{"user": s.userRow(u)})
}

func (s *Server) handleAdminGrant(c *Ctx) error {
	var req struct {
		User   string  `json:"user"`
		Org    string  `json:"org"`
		Amount float64 `json:"amount"`
		Reason string  `json:"reason"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	wallet := ""
	switch {
	case c.Param("userId") != "" || req.User != "":
		ref := firstNonEmpty(c.Param("userId"), req.User)
		u, err := s.UserByLogin(ref)
		if err != nil {
			return errNotFound("User")
		}
		wallet = u.ID
	case req.Org != "":
		var id string
		if s.DB.QueryRow(`SELECT id FROM orgs WHERE id=? OR name=? COLLATE NOCASE LIMIT 1`, req.Org, req.Org).Scan(&id) != nil {
			return errNotFound("Organization")
		}
		wallet = id
	default:
		return Err(400, "wallet_required", "Pass user or org.")
	}
	if req.Amount == 0 {
		return Err(400, "invalid_amount", "amount must not be 0.")
	}
	// bugs-r10-sonnet 11: refuse NaN/Inf, absurd grants and balances.
	var cur float64
	_ = s.DB.QueryRow(`SELECT balance FROM wallets WHERE id=?`, wallet).Scan(&cur)
	if math.IsNaN(req.Amount) || math.Abs(req.Amount) > 1e9 || math.Abs(cur+req.Amount) > 1e12 {
		return Err(400, "invalid_amount", "amount must be a finite number up to 1e9 and the balance must stay within 1e12.")
	}
	if err := s.GrantCredits(wallet, req.Amount, firstNonEmpty(req.Reason, "admin grant"), c.P.User.ID); err != nil {
		return err
	}
	return c.OK(200, "admin.credits_granted", map[string]any{"wallet": s.walletJSON(wallet)})
}

// CreateInviteCode creates a signup invite code.
func (s *Server) CreateInviteCode(createdBy, note string, maxUses int, ttl time.Duration) (string, string, error) {
	code := "cvi_" + randToken(24)
	id := "ic_" + randToken(12)
	var exp any
	if ttl > 0 {
		exp = store.Now() + ttl.Milliseconds()
	}
	if maxUses <= 0 {
		maxUses = 1
	}
	_, err := s.DB.Exec(`INSERT INTO invite_codes(id,code_hash,prefix,note,created_by,created_at,expires_at,max_uses) VALUES(?,?,?,?,?,?,?,?)`,
		id, hashToken(code), code[:12], note, nz(createdBy), store.Now(), exp, maxUses)
	return id, code, err
}

func (s *Server) handleCreateInviteCode(c *Ctx) error {
	var req struct {
		Note       string `json:"note"`
		MaxUses    int    `json:"maxUses"`
		TTLSeconds int    `json:"ttlSeconds"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	ttl := 7 * 24 * time.Hour
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	id, code, err := s.CreateInviteCode(c.P.User.ID, req.Note, req.MaxUses, ttl)
	if err != nil {
		return err
	}
	return c.OK(201, "admin.invite_code_created", map[string]any{"id": id, "code": code, "signupUrl": s.Cfg.PublicURL + "/signup?invite=" + code})
}

func (s *Server) handleListInviteCodes(c *Ctx) error {
	rows, err := s.DB.Query(`SELECT id,prefix,note,created_at,expires_at,max_uses,uses,revoked_at FROM invite_codes ORDER BY created_at DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, prefix, note string
		var created int64
		var exp, rev sql.NullInt64
		var maxU, uses int
		if rows.Scan(&id, &prefix, &note, &created, &exp, &maxU, &uses, &rev) == nil {
			out = append(out, map[string]any{"id": id, "prefix": prefix, "note": note, "createdAt": tsMs(created), "expiresAt": tsNull(exp),
				"maxUses": maxU, "uses": uses, "revokedAt": tsNull(rev)})
		}
	}
	return c.OK(200, "admin.invite_codes", map[string]any{"inviteCodes": out})
}

func (s *Server) handleRevokeInviteCode(c *Ctx) error {
	res, err := s.DB.Exec(`UPDATE invite_codes SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, store.Now(), c.Param("codeId"))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound("Invite code")
	}
	return c.OK(200, "admin.invite_code_revoked", map[string]any{"id": c.Param("codeId")})
}

func (s *Server) handleAdminSandboxes(c *Ctx) error {
	pg, err := ParsePage(c.R)
	if err != nil {
		return err
	}
	q := `SELECT ` + sbCols + ` FROM sandboxes WHERE deleted_at IS NULL`
	var args []any
	if v := c.Query("state"); v != "" {
		var ph []string
		for _, st := range strings.Split(v, ",") {
			if st = strings.TrimSpace(st); st == "stopped" {
				st = api.StateArchived
			}
			ph = append(ph, "?")
			args = append(args, st)
		}
		q += ` AND state IN (` + strings.Join(ph, ",") + `)`
	}
	if v := c.Query("node"); v != "" {
		q += ` AND node_id=?`
		args = append(args, v)
	}
	where, pargs, tail := pg.SQL("created_at", "id")
	rows, err := s.DB.Query(q+where+tail, append(args, pargs...)...)
	if err != nil {
		return err
	}
	var list []*Sandbox
	for rows.Next() {
		if sb, err := scanSandbox(rows); err == nil {
			list = append(list, sb)
		}
	}
	rows.Close()
	list, info := PageResult(pg, list, func(sb *Sandbox) (int64, string) { return store.T(sb.CreatedAt), sb.ID })
	out := []map[string]any{}
	for _, sb := range list {
		out = append(out, s.SandboxJSON(sb, nil))
	}
	return c.OK(200, "admin.sandboxes", map[string]any{"sandboxes": out, "pageInfo": pageJSON(info)})
}

// ---------------------------------------------------------------------------
// Idempotency (POST /sandboxes, POST /sandboxes/{id}/fork)
// ---------------------------------------------------------------------------

// idemBegin claims key for this request. When done is true the response was
// written (a replay) or err says why the key cannot be used.
func (s *Server) idemBegin(c *Ctx, key, route string, body []byte) (done bool, err error) {
	var canon any
	if json.Unmarshal(body, &canon) == nil {
		body = []byte(mustJSON(canon))
	}
	hash := sha256Hex(append([]byte(route+"\n"), body...))
	owner := c.P.User.ID
	now := store.Now()
	var h, status string
	var sbID sql.NullString
	var created int64
	err = s.DB.QueryRow(`SELECT body_hash,status,sandbox_id,created_at FROM idempotency WHERE owner_id=? AND key=?`, owner, key).Scan(&h, &status, &sbID, &created)
	if err == nil && now-created > 24*3600_000 {
		_, _ = s.DB.Exec(`DELETE FROM idempotency WHERE owner_id=? AND key=?`, owner, key)
		err = sql.ErrNoRows
	}
	if err == nil {
		if h != hash {
			return true, Err(409, "idempotency_key_reused", "This Idempotency-Key was used with a different request body.")
		}
		if status == "in_progress" {
			if now-created > 2*60_000 {
				_, _ = s.DB.Exec(`DELETE FROM idempotency WHERE owner_id=? AND key=?`, owner, key)
				return s.idemBegin(c, key, route, body)
			}
			return true, ErrD(409, "idempotency_in_progress", "A request with this Idempotency-Key is still running. Retry shortly.", map[string]any{"retryable": true})
		}
		sb, err := s.SandboxByID(sbID.String)
		if err != nil || sb.DeletedAt != nil {
			return true, errNotFound("Sandbox")
		}
		status := "provisioning"
		if !sb.starting() {
			status = "ready"
		}
		typ := "sandbox.created"
		fields := map[string]any{"status": status, "ttlSeconds": ttlOut(sb.TTLSeconds), "sandbox": s.SandboxJSON(sb, c.P), "idempotentReplay": true}
		if strings.HasSuffix(route, "/fork") {
			typ = "sandbox.forking"
			fields["id"] = sb.ID
			fields["status"] = "forking"
		}
		var saved string
		_ = s.DB.QueryRow(`SELECT COALESCE(response,'') FROM idempotency WHERE owner_id=? AND key=?`, owner, key).Scan(&saved)
		var first map[string]any
		if json.Unmarshal([]byte(saved), &first) == nil {
			if hp, ok := first["hostedPorts"]; ok {
				fields["hostedPorts"] = hp
			}
			if all, ok := first["sandboxes"].([]any); ok {
				var fresh []map[string]any
				for _, x := range all {
					if m, ok := x.(map[string]any); ok {
						if id, _ := m["id"].(string); id != "" {
							if b, err := s.SandboxByID(id); err == nil && b.DeletedAt == nil {
								fresh = append(fresh, s.SandboxJSON(b, c.P))
							}
						}
					}
				}
				fields["sandboxes"] = fresh
			}
		}
		c.W.Header().Set("Idempotent-Replayed", "true")
		return true, c.OK(202, typ, fields)
	}
	if _, err := s.DB.Exec(`INSERT INTO idempotency(owner_id,key,body_hash,status,created_at) VALUES(?,?,?,?,?)`, owner, key, hash, "in_progress", now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return true, ErrD(409, "idempotency_in_progress", "A request with this Idempotency-Key is still running. Retry shortly.", map[string]any{"retryable": true})
		}
		return true, err
	}
	return false, nil
}

func (s *Server) idemFinish(owner, key, sbID string) {
	_, _ = s.DB.Exec(`UPDATE idempotency SET status='done', sandbox_id=? WHERE owner_id=? AND key=?`, sbID, owner, key)
}

// idemResponse keeps the first answer's fields that a replay cannot rebuild
// (hosted port URLs with their tokens, every sandbox of a count>1 create),
// bugs-r9-sonnet.
func (s *Server) idemResponse(owner, key string, resp map[string]any) {
	_, _ = s.DB.Exec(`UPDATE idempotency SET response=? WHERE owner_id=? AND key=?`, mustJSON(resp), owner, key)
}

func init() {
	store.Register("idempotency_response", store.Migration{Version: 1, SQL: `ALTER TABLE idempotency ADD COLUMN response TEXT;`})
}

func (s *Server) idemRelease(owner, key string) {
	_, _ = s.DB.Exec(`DELETE FROM idempotency WHERE owner_id=? AND key=? AND status='in_progress'`, owner, key)
}

func (s *Server) gcIdempotency() {
	_, _ = s.DB.Exec(`DELETE FROM idempotency WHERE created_at < ?`, store.Now()-24*3600_000)
}
