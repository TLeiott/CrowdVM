package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Org is an organization (team). Its id doubles as its wallet id.
type Org struct {
	ID, Name, OwnerID   string
	NewSandboxesPrivate bool
	CreatedAt           time.Time
}

const PrefixOrg = "team_"

func scanOrg(sc interface{ Scan(...any) error }) (*Org, error) {
	o := &Org{}
	var c int64
	if err := sc.Scan(&o.ID, &o.Name, &o.OwnerID, &o.NewSandboxesPrivate, &c); err != nil {
		return nil, err
	}
	o.CreatedAt = store.Time(c)
	return o, nil
}

const orgCols = `o.id,o.name,o.owner_id,o.new_sandboxes_private,o.created_at`

// OrgByID loads an org.
func (s *Server) OrgByID(id string) (*Org, error) {
	return scanOrg(s.DB.QueryRow(`SELECT `+orgCols+` FROM orgs o WHERE o.id=?`, id))
}

// memberRole returns the user's role in org ("" when not a member).
func (s *Server) memberRole(orgID, userID string) string {
	var r string
	_ = s.DB.QueryRow(`SELECT role FROM org_members WHERE org_id=? AND user_id=?`, orgID, userID).Scan(&r)
	return r
}

func (s *Server) userOrgs(userID string) ([]*Org, error) {
	rows, err := s.DB.Query(`SELECT `+orgCols+` FROM orgs o JOIN org_members m ON m.org_id=o.id WHERE m.user_id=? ORDER BY o.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Org
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// resolveOrgRef resolves an org reference (id, name, "personal", own user
// id) to an org the user belongs to; nil means personal.
func (s *Server) resolveOrgRef(u *User, ref string) (*Org, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.EqualFold(ref, "personal") || ref == u.ID || ref == "null" {
		return nil, nil
	}
	orgs, err := s.userOrgs(u.ID)
	if err != nil {
		return nil, err
	}
	var match []*Org
	for _, o := range orgs {
		if o.ID == ref {
			return o, nil
		}
		if strings.EqualFold(o.Name, ref) {
			match = append(match, o)
		}
	}
	switch len(match) {
	case 0:
		return nil, Err(403, "not_org_member", "You are not a member of an organization named "+ref+".")
	case 1:
		return match[0], nil
	default:
		return nil, Err(409, "ambiguous_org", "Several of your organizations are named "+ref+". Pass the id.")
	}
}

func orgHeader(r *http.Request) string {
	if v := r.Header.Get("X-CrowdVM-Org"); v != "" {
		return v
	}
	return r.Header.Get("X-Boat-Org")
}

// Scope resolves the wallet of a request: body "org" > ?org= / ?teamId= >
// X-CrowdVM-Org header > org key > active org > personal. nil = personal.
func (c *Ctx) Scope(bodyOrg string) (*Org, error) {
	ref, explicit := bodyOrg, true
	if ref == "" {
		ref = c.Query("teamId")
	}
	if ref == "" {
		ref = c.Query("org")
	}
	if ref == "" {
		ref = orgHeader(c.R)
	}
	if ref == "" {
		explicit = false
	}
	if k := c.P.Key; k != nil && k.OrgID != "" {
		if explicit {
			o, err := c.S.resolveOrgRef(c.P.User, ref)
			if err != nil {
				return nil, err
			}
			if o == nil || o.ID != k.OrgID {
				return nil, Err(403, "api_key_org_mismatch", "This organization key works only for its own organization.")
			}
		}
		o, err := c.S.OrgByID(k.OrgID)
		if err != nil || c.S.memberRole(k.OrgID, c.P.User.ID) == "" {
			return nil, Err(403, "not_org_member", "You are no longer a member of this key's organization.")
		}
		return o, nil
	}
	if !explicit {
		if c.P.User.ActiveOrg == "" {
			return nil, nil
		}
		o, err := c.S.OrgByID(c.P.User.ActiveOrg)
		if err != nil || c.S.memberRole(o.ID, c.P.User.ID) == "" {
			_, _ = c.S.DB.Exec(`UPDATE users SET active_org=NULL WHERE id=?`, c.P.User.ID)
			return nil, nil
		}
		return o, nil
	}
	return c.S.resolveOrgRef(c.P.User, ref)
}

func walletOf(u *User, o *Org) string {
	if o != nil {
		return o.ID
	}
	return u.ID
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) orgRoutes() {
	s.API("GET /orgs", "account.read", s.handleListOrgs)
	s.API("PATCH /orgs/active", "account.admin", s.handleSetActiveOrg) // it changes the wallet (bugs-r3-opus 13)
	s.API("PATCH /orgs/{org}", "account.admin", s.handleUpdateOrg)
	// CrowdVM extensions (Boat keeps these in its dashboard API).
	s.API("POST /orgs", "account.admin", s.handleCreateOrg)
	s.API("GET /orgs/{org}", "account.read", s.handleGetOrg)
	s.API("DELETE /orgs/{org}", "account.admin", s.handleDeleteOrg)
	s.API("GET /orgs/{org}/members", "account.read", s.handleListMembers)
	s.API("PATCH /orgs/{org}/members/{userId}", "account.admin", s.handleUpdateMember)
	s.API("DELETE /orgs/{org}/members/{userId}", "account.admin", s.handleRemoveMember)
	s.API("POST /orgs/{org}/transfer", "account.admin", s.handleTransferOrg)
	s.API("GET /orgs/{org}/invites", "account.read", s.handleListInvites)
	s.API("POST /orgs/{org}/invites", "account.admin", s.handleCreateInvite)
	s.API("DELETE /orgs/{org}/invites/{inviteId}", "account.admin", s.handleRevokeInvite)
	s.API("POST /org-invites/accept", "account.admin", s.handleAcceptInvite)
}

func (s *Server) orgJSON(o *Org, u *User, active bool) map[string]any {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM org_members WHERE org_id=?`, o.ID).Scan(&n)
	role := s.memberRole(o.ID, u.ID)
	m := map[string]any{"id": o.ID, "name": o.Name, "type": "org", "role": role, "memberCount": n, "active": active,
		"subscriptionStatus": "active", "newSandboxesPrivate": o.NewSandboxesPrivate, "ownerId": o.OwnerID, "createdAt": ts(o.CreatedAt),
		"suspended": false}
	if at, why := s.orgSuspension(o.ID); at != 0 {
		m["subscriptionStatus"], m["suspended"], m["suspendedAt"], m["suspendedReason"] = "suspended", true, tsMs(at), nz(why)
	}
	return m
}

func (s *Server) handleListOrgs(c *Ctx) error {
	u := c.P.User
	orgs, err := s.userOrgs(u.ID)
	if err != nil {
		return err
	}
	list := []map[string]any{{"id": u.ID, "name": "Personal", "type": "personal", "role": "owner", "memberCount": 1, "active": u.ActiveOrg == ""}}
	for _, o := range orgs {
		if c.P.Key != nil && c.P.Key.OrgID != "" && c.P.Key.OrgID != o.ID {
			continue
		}
		list = append(list, s.orgJSON(o, u, u.ActiveOrg == o.ID))
	}
	return c.OK(200, "org.list", map[string]any{"orgs": list, "activeWalletChosen": u.ActiveWalletChosen})
}

func (s *Server) handleSetActiveOrg(c *Ctx) error {
	var req struct {
		Org *string `json:"org"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	// The active wallet is an account setting every other key and session
	// follows: an organization key does not change it (bugs-r4-sol 3).
	if k := c.P.Key; k != nil && k.OrgID != "" {
		return Err(403, "api_key_org_mismatch", "An organization key cannot change the account's active wallet.")
	}
	ref := ""
	if req.Org != nil {
		ref = *req.Org
	}
	o, err := s.resolveOrgRef(c.P.User, ref)
	if err != nil {
		return err
	}
	var id any
	active := map[string]any{"id": c.P.User.ID, "name": "Personal", "type": "personal"}
	if o != nil {
		id = o.ID
		active = map[string]any{"id": o.ID, "name": o.Name, "type": "org"}
	}
	if _, err := s.DB.Exec(`UPDATE users SET active_org=?, active_wallet_chosen=1 WHERE id=?`, id, c.P.User.ID); err != nil {
		return err
	}
	return c.OK(200, "org.active_updated", map[string]any{"active": active})
}

// paramOrg resolves the {org} path parameter (nil: personal). An
// organization key reaches only its own organization, as in Scope
// (bugs-r3-sol 1).
func (c *Ctx) paramOrg() (*Org, error) {
	o, err := c.S.resolveOrgRef(c.P.User, c.Param("org"))
	if err != nil {
		return nil, err
	}
	if k := c.P.Key; k != nil && k.OrgID != "" && (o == nil || o.ID != k.OrgID) {
		return nil, Err(403, "api_key_org_mismatch", "This organization key works only for its own organization.")
	}
	return o, nil
}

// orgForAdmin resolves {org} and requires one of roles.
func (c *Ctx) orgParam(roles ...string) (*Org, string, error) {
	o, err := c.paramOrg()
	if err != nil {
		return nil, "", err
	}
	if o == nil {
		return nil, "", Err(400, "not_org", "Name an organization, not your personal account.")
	}
	role := c.S.memberRole(o.ID, c.P.User.ID)
	if len(roles) > 0 {
		ok := false
		for _, r := range roles {
			ok = ok || r == role
		}
		if !ok {
			return nil, "", Err(403, "forbidden", "Your role in "+o.Name+" cannot do this.")
		}
	}
	return o, role, nil
}

// Per-account limits on objects (bugs-r7-sonnet 15).
const (
	maxOwnedOrgs   = 50
	maxOpenInvites = 200
)

func (s *Server) handleCreateOrg(c *Ctx) error {
	var req struct {
		Name                string `json:"name"`
		NewSandboxesPrivate bool   `json:"newSandboxesPrivate"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 80 || strings.EqualFold(name, "personal") {
		return Err(400, "invalid_name", "Organization name must be 1-80 characters and not \"personal\".")
	}
	var owned int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM orgs WHERE owner_id=?`, c.P.User.ID).Scan(&owned)
	if owned >= maxOwnedOrgs { // bugs-r7-sonnet 15
		return Err(409, "org_limit", fmt.Sprintf("An account can own up to %d organizations. Delete one first.", maxOwnedOrgs))
	}
	o := &Org{ID: PrefixOrg + randToken(20), Name: name, OwnerID: c.P.User.ID, NewSandboxesPrivate: req.NewSandboxesPrivate, CreatedAt: time.Now()}
	err := s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO orgs(id,name,owner_id,new_sandboxes_private,created_at) VALUES(?,?,?,?,?)`, o.ID, o.Name, o.OwnerID, boolInt(o.NewSandboxesPrivate), store.T(o.CreatedAt)); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO org_members(org_id,user_id,role,joined_at) VALUES(?,?,?,?)`, o.ID, o.OwnerID, "owner", store.Now()); err != nil {
			return err
		}
		return walletAdd(tx, o.ID, 0, "grant", "organization created", "", "", c.P.User.ID)
	})
	if err != nil {
		return err
	}
	return c.OK(201, "org.created", map[string]any{"org": s.orgJSON(o, c.P.User, false)})
}

func (s *Server) handleGetOrg(c *Ctx) error {
	o, _, err := c.orgParam()
	if err != nil {
		return err
	}
	m := s.orgJSON(o, c.P.User, c.P.User.ActiveOrg == o.ID)
	m["wallet"] = s.walletJSON(o.ID)
	return c.OK(200, "org.info", map[string]any{"org": m})
}

func (s *Server) handleUpdateOrg(c *Ctx) error {
	var req struct {
		Name                *string `json:"name"`
		NewSandboxesPrivate *bool   `json:"newSandboxesPrivate"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.Name == nil && req.NewSandboxesPrivate == nil {
		return Err(400, "no_changes", "Pass name or newSandboxesPrivate.")
	}
	o, role, err := c.orgParam("owner", "admin")
	if err != nil {
		return err
	}
	out := map[string]any{"success": true}
	if req.Name != nil {
		if role != "owner" {
			return Err(403, "owner_only", "Only the owner renames the organization.")
		}
		n := strings.TrimSpace(*req.Name)
		if n == "" || len(n) > 80 || strings.EqualFold(n, "personal") {
			return Err(400, "invalid_name", "Organization name must be 1-80 characters.")
		}
		_, _ = s.DB.Exec(`UPDATE orgs SET name=? WHERE id=?`, n, o.ID)
		out["name"] = n
	}
	if req.NewSandboxesPrivate != nil {
		_, _ = s.DB.Exec(`UPDATE orgs SET new_sandboxes_private=? WHERE id=?`, boolInt(*req.NewSandboxesPrivate), o.ID)
		out["newSandboxesPrivate"] = *req.NewSandboxesPrivate
	}
	return c.OK(200, "org.updated", out)
}

func (s *Server) handleDeleteOrg(c *Ctx) error {
	o, _, err := c.orgParam("owner")
	if err != nil {
		return err
	}
	var members int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM org_members WHERE org_id=?`, o.ID).Scan(&members)
	if members > 1 { // Boat: only the owner as the last member deletes it
		return ErrD(409, "org_has_members", "Remove the other members first (dashboard → Organization); the owner deletes the organization as its last member.",
			map[string]any{"members": members})
	}
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE org_id=? AND deleted_at IS NULL AND state NOT IN ('archived','error','cancelled')`, o.ID).Scan(&n)
	if n > 0 {
		return Err(409, "org_has_running_sandboxes", "Stop the organization's sandboxes first.")
	}
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		// Refuse deletion while credits remain so deleting an org cannot destroy its balance.
		var balance float64
		if err := tx.QueryRow(`SELECT COALESCE((SELECT balance FROM wallets WHERE id=?),0)`, o.ID).Scan(&balance); err != nil {
			return err
		}
		if round6(balance) > 0 { // what the API shows: a sub-micro remainder of a transfer does not count
			return Err(409, "org_has_balance", "Transfer the organization's remaining credits before deleting it.")
		}
		// Sandboxes go back to their creators' personal billing.
		if _, err := tx.Exec(`UPDATE sandboxes SET org_id=NULL, visibility=NULL WHERE org_id=?`, o.ID); err != nil {
			return err
		}
		// So do usage not billed yet and saved snapshots: nothing stays on
		// the deleted organization (bugs-r5-sonnet).
		if _, err := tx.Exec(`UPDATE sandbox_runs SET wallet_id=(SELECT owner_id FROM sandboxes WHERE id=sandbox_runs.sandbox_id)
			WHERE wallet_id=? AND (ended_at IS NULL OR billed_until<ended_at)`, o.ID); err != nil {
			return err
		}
		// An organization's saved snapshots go to whoever saved them (else
		// the source sandbox's owner, else the deleting owner), renamed when
		// that name is taken.
		type saved struct{ id, name, to string }
		var list []saved
		rows, err := tx.Query(`SELECT ss.id, ss.name, COALESCE(NULLIF(ss.saved_by,''), (SELECT owner_id FROM sandboxes WHERE id=ss.source_sandbox_id), ?)
			FROM saved_snapshots ss WHERE ss.owner_id=? OR ss.org_id=?`, c.P.User.ID, o.ID, o.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var x saved
			if rows.Scan(&x.id, &x.name, &x.to) == nil {
				list = append(list, x)
			}
		}
		rows.Close()
		for _, x := range list {
			name := x.name
			for i := 1; ; i++ {
				var taken int
				_ = tx.QueryRow(`SELECT COUNT(*) FROM saved_snapshots WHERE owner_id=? AND name=? AND id<>?`, x.to, name, x.id).Scan(&taken)
				if taken == 0 {
					break
				}
				name = x.name + "-" + o.Name + "-" + strconv.Itoa(i)
			}
			if _, err := tx.Exec(`UPDATE saved_snapshots SET owner_id=?, org_id=NULL, name=? WHERE id=?`, x.to, name, x.id); err != nil {
				return err
			}
		}
		// Environments (with their sealed vars and secret files) and git
		// sources belong to the organization alone: they go with it.
		for _, q := range []string{
			// Versions a saved snapshot's sandbox ran with stay: a foreign
			// deploy of it must still scrub their secret files
			// (bugs-r7-sonnet 5). The rest go.
			`DELETE FROM environment_versions WHERE env_id IN (SELECT id FROM environments WHERE owner_id=?)
				AND env_id NOT IN (SELECT b.environment_id FROM saved_snapshots ss JOIN sandboxes b ON b.id=ss.source_sandbox_id WHERE b.environment_id IS NOT NULL)`,
			`DELETE FROM environments WHERE owner_id=?`,
			`DELETE FROM git_repos WHERE owner_id=? OR source_id IN (SELECT id FROM git_sources WHERE owner_id=?)`,
			`DELETE FROM git_sources WHERE owner_id=?`,
			// Its CI links: the token goes, like an unlink (bugs-r8-sonnet 7).
			`DELETE FROM ci_repos WHERE link_id IN (SELECT id FROM ci_links WHERE org_id=?)`,
			`UPDATE ci_links SET status='unlinked', token='', org_id=NULL WHERE org_id=?`,
			// AI credentials stored for the organization (bugs-r7-sonnet 16).
			`DELETE FROM credential_accounts WHERE profile_id IN (SELECT id FROM credential_profiles WHERE owner_id=?)`,
			`DELETE FROM credential_custom_apis WHERE profile_id IN (SELECT id FROM credential_profiles WHERE owner_id=?)`,
			`DELETE FROM credential_profiles WHERE owner_id=?`,
		} {
			args := make([]any, strings.Count(q, "?"))
			for i := range args {
				args[i] = o.ID
			}
			if _, err := tx.Exec(q, args...); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE users SET active_org=NULL WHERE active_org=?`, o.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE api_keys SET revoked_at=? WHERE org_id=? AND revoked_at IS NULL`, store.Now(), o.ID); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM orgs WHERE id=?`, o.ID)
		return err
	})
	if err != nil {
		return err
	}
	return c.OK(200, "org.deleted", map[string]any{"id": o.ID})
}

func (s *Server) handleListMembers(c *Ctx) error {
	o, _, err := c.orgParam()
	if err != nil {
		return err
	}
	rows, err := s.DB.Query(`SELECT u.id,u.username,COALESCE(u.email,''),u.display_name,m.role,m.joined_at,m.concurrent_cap,m.usage_cap_credits FROM org_members m JOIN users u ON u.id=m.user_id WHERE m.org_id=? ORDER BY m.joined_at`, o.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	members := []map[string]any{}
	for rows.Next() {
		var id, un, em, dn, role string
		var joined int64
		var capN sql.NullInt64
		var capU sql.NullFloat64
		if err := rows.Scan(&id, &un, &em, &dn, &role, &joined, &capN, &capU); err != nil {
			return err
		}
		m := map[string]any{"id": id, "login": un, "email": nz(em), "displayName": dn, "role": role, "joinedAt": tsMs(joined), "concurrentCap": nil, "usageCapCredits": nil}
		if capN.Valid {
			m["concurrentCap"] = capN.Int64
		}
		if capU.Valid {
			m["usageCapCredits"] = capU.Float64
		}
		members = append(members, m)
	}
	return c.OK(200, "org.members", map[string]any{"org": o.ID, "members": members})
}

func (s *Server) handleUpdateMember(c *Ctx) error {
	var req struct {
		Role            string   `json:"role"`
		ConcurrentCap   OptInt   `json:"concurrentCap"`
		UsageCapCredits *float64 `json:"usageCapCredits"`
		ClearUsageCap   bool     `json:"clearUsageCap"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	o, role, err := c.orgParam("owner", "admin")
	if err != nil {
		return err
	}
	target := c.Param("userId")
	trole := s.memberRole(o.ID, target)
	if trole == "" {
		return errNotFound("Member")
	}
	if trole == "owner" && role != "owner" {
		return Err(403, "owner_only", "Only the owner changes the owner's settings.")
	}
	if (req.ConcurrentCap.Set && !req.ConcurrentCap.Null && req.ConcurrentCap.Value < 0) || (req.UsageCapCredits != nil && *req.UsageCapCredits < 0) {
		return Err(400, "invalid_member_cap", "Member caps must be non-negative.")
	}
	if req.Role != "" {
		if req.Role != "admin" && req.Role != "member" {
			return Err(400, "invalid_role", "role must be admin or member (use transfer for owner).")
		}
		if trole == "owner" {
			return Err(400, "invalid_role", "Transfer ownership first.")
		}
		_, _ = s.DB.Exec(`UPDATE org_members SET role=? WHERE org_id=? AND user_id=?`, req.Role, o.ID, target)
	}
	if req.ConcurrentCap.Set {
		var v any
		if !req.ConcurrentCap.Null {
			v = req.ConcurrentCap.Value
		}
		_, _ = s.DB.Exec(`UPDATE org_members SET concurrent_cap=? WHERE org_id=? AND user_id=?`, v, o.ID, target)
	}
	if req.UsageCapCredits != nil || req.ClearUsageCap {
		var v any
		if req.UsageCapCredits != nil {
			v = *req.UsageCapCredits
		}
		_, _ = s.DB.Exec(`UPDATE org_members SET usage_cap_credits=? WHERE org_id=? AND user_id=?`, v, o.ID, target)
	}
	return s.handleListMembers(c)
}

func (s *Server) handleRemoveMember(c *Ctx) error {
	o, err := c.paramOrg()
	if err != nil {
		return err
	}
	if o == nil {
		return Err(400, "not_org", "Name an organization.")
	}
	target := c.Param("userId")
	role := s.memberRole(o.ID, c.P.User.ID)
	self := target == c.P.User.ID || target == "me"
	if self {
		target = c.P.User.ID
	} else if role != "owner" && role != "admin" {
		return Err(403, "forbidden", "Only owners and admins remove members.")
	}
	trole := s.memberRole(o.ID, target)
	if trole == "" {
		return errNotFound("Member")
	}
	if trole == "owner" {
		return Err(400, "owner_cannot_leave", "Transfer ownership before the owner leaves.")
	}
	// bugs-r17-opus 3: a service account's sandboxes would be orphaned here;
	// its own endpoint refuses while it still has sandboxes.
	if tu, err := s.UserByID(target); err == nil && tu.Kind == "service" {
		return Err(409, "service_account", "This is a service account: remove it with DELETE /orgs/"+o.ID+"/service-accounts/"+target+".")
	}
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		// Refuse removal until every org-billed run ends; moving only org_id
		// would leave an open run charging the organization's wallet.
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE org_id=? AND owner_id=? AND deleted_at IS NULL AND (state NOT IN ('archived','error','cancelled') OR EXISTS (SELECT 1 FROM sandbox_runs WHERE sandbox_id=sandboxes.id AND ended_at IS NULL))`, o.ID, target).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return Err(409, "member_has_running_sandboxes", "Stop this member's organization sandboxes before removing them.")
		}
		if _, err := tx.Exec(`DELETE FROM org_members WHERE org_id=? AND user_id=?`, o.ID, target); err != nil {
			return err
		}
		// Their sandboxes go back to personal billing (Boat semantics).
		if _, err := tx.Exec(`UPDATE sandboxes SET org_id=NULL, visibility=NULL WHERE org_id=? AND owner_id=?`, o.ID, target); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE users SET active_org=NULL WHERE id=? AND active_org=?`, target, o.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE api_keys SET revoked_at=? WHERE user_id=? AND org_id=? AND revoked_at IS NULL`, store.Now(), target, o.ID)
		return err
	})
	if err != nil {
		return err
	}
	s.revokeMemberKeys(o.ID, target) // bugs-r17-opus 2
	return c.OK(200, "org.member_removed", map[string]any{"org": o.ID, "userId": target})
}

func (s *Server) handleTransferOrg(c *Ctx) error {
	var req struct {
		User string `json:"user"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	o, _, err := c.orgParam("owner")
	if err != nil {
		return err
	}
	u, err := s.UserByLogin(req.User)
	if err != nil || s.memberRole(o.ID, u.ID) == "" {
		return Err(400, "not_member", "The new owner must be a member.")
	}
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE org_members SET role='member' WHERE org_id=? AND user_id=?`, o.ID, c.P.User.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE org_members SET role='owner' WHERE org_id=? AND user_id=?`, o.ID, u.ID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE orgs SET owner_id=? WHERE id=?`, u.ID, o.ID)
		return err
	})
	if err != nil {
		return err
	}
	return c.OK(200, "org.transferred", map[string]any{"org": o.ID, "ownerId": u.ID})
}

func (s *Server) handleListInvites(c *Ctx) error {
	o, _, err := c.orgParam()
	if err != nil {
		return err
	}
	rows, err := s.DB.Query(`SELECT id,email,role,created_at,expires_at,accepted_at,revoked_at FROM org_invites WHERE org_id=? ORDER BY created_at DESC`, o.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, em, role string
		var created, exp int64
		var acc, rev sql.NullInt64
		if err := rows.Scan(&id, &em, &role, &created, &exp, &acc, &rev); err != nil {
			return err
		}
		status := "pending"
		switch {
		case acc.Valid:
			status = "accepted"
		case rev.Valid:
			status = "revoked"
		case exp < store.Now():
			status = "expired"
		}
		out = append(out, map[string]any{"id": id, "email": nz(em), "role": role, "status": status, "createdAt": tsMs(created), "expiresAt": tsMs(exp)})
	}
	return c.OK(200, "org.invites", map[string]any{"org": o.ID, "invites": out})
}

func (s *Server) handleCreateInvite(c *Ctx) error {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	o, _, err := c.orgParam("owner") // Boat organizations: "The owner invites people"
	if err != nil {
		return err
	}
	if req.Role == "" {
		req.Role = "member"
	}
	if req.Role != "member" && req.Role != "admin" {
		return Err(400, "invalid_role", "role must be member or admin.")
	}
	if len(req.Email) > 254 { // bugs-r7-sonnet 14
		return Err(400, "invalid_email", "email can be at most 254 characters.")
	}
	var open int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM org_invites WHERE org_id=? AND expires_at>? AND accepted_at IS NULL AND revoked_at IS NULL`, o.ID, store.Now()).Scan(&open)
	if open >= maxOpenInvites { // bugs-r7-sonnet 15
		return Err(409, "invite_limit", fmt.Sprintf("An organization can have up to %d open invites. Revoke some first.", maxOpenInvites))
	}
	code := "cvi_" + randToken(32)
	id := "inv_" + randToken(16)
	exp := store.Now() + 14*24*3600_000
	if _, err := s.DB.Exec(`INSERT INTO org_invites(id,org_id,email,role,code_hash,invited_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, o.ID, strings.ToLower(strings.TrimSpace(req.Email)), req.Role, hashToken(code), c.P.User.ID, store.Now(), exp); err != nil {
		return err
	}
	link := s.Cfg.PublicURL + "/invite?code=" + code
	if req.Email != "" {
		s.sendMail(req.Email, "Join "+o.Name+" on CrowdVM", c.P.User.DisplayName+" invited you to "+o.Name+". Open "+link+" or run: crowdvm org join "+code)
		s.Log.Info("org invite", "org", o.Name, "email", req.Email, "code", code)
	}
	return c.OK(201, "org.invite_created", map[string]any{"invite": map[string]any{"id": id, "email": nz(req.Email), "role": req.Role, "status": "pending", "expiresAt": tsMs(exp)}, "code": code, "url": link})
}

func (s *Server) handleRevokeInvite(c *Ctx) error {
	o, _, err := c.orgParam("owner", "admin")
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(`UPDATE org_invites SET revoked_at=? WHERE id=? AND org_id=? AND accepted_at IS NULL`, store.Now(), c.Param("inviteId"), o.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound("Invite")
	}
	return c.OK(200, "org.invite_revoked", map[string]any{"id": c.Param("inviteId")})
}

func (s *Server) handleAcceptInvite(c *Ctx) error {
	var req struct {
		Code string `json:"code"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	var id, orgID, email, role string
	var exp int64
	var acc, rev sql.NullInt64
	err := s.DB.QueryRow(`SELECT id,org_id,email,role,expires_at,accepted_at,revoked_at FROM org_invites WHERE code_hash=?`, hashToken(strings.TrimSpace(req.Code))).
		Scan(&id, &orgID, &email, &role, &exp, &acc, &rev)
	if err != nil || acc.Valid || rev.Valid || exp < store.Now() {
		return Err(404, "invalid_invite", "The invite is wrong, used, revoked or expired.")
	}
	if email != "" && c.P.User.Email != "" && !strings.EqualFold(email, c.P.User.Email) {
		s.Log.Info("invite accepted by a different email", "invite", email, "user", c.P.User.Email)
	}
	// bugs-r17-opus 6: claim the invite with one conditional UPDATE, in the
	// same transaction as the insert, so two accepts cannot both join.
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		now := store.Now()
		res, err := tx.Exec(`UPDATE org_invites SET accepted_at=?, accepted_by=? WHERE id=? AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at>?`, now, c.P.User.ID, id, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return Err(404, "invalid_invite", "The invite is wrong, used, revoked or expired.")
		}
		// An existing member keeps their role (the claim above still burns the invite).
		_, err = tx.Exec(`INSERT INTO org_members(org_id,user_id,role,joined_at) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, orgID, c.P.User.ID, role, now)
		return err
	})
	if err != nil {
		return err
	}
	o, err := s.OrgByID(orgID)
	if err != nil {
		return err
	}
	return c.OK(200, "org.joined", map[string]any{"org": s.orgJSON(o, c.P.User, false)})
}
