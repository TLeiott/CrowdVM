package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Scope actions (Boat's catalog).
var Actions = []string{
	"sandbox.create", "sandbox.read", "sandbox.update", "sandbox.stop", "sandbox.resume", "sandbox.fork",
	"sandbox.delete", "sandbox.delete-own", "agent.prompt", "exec", "file.read", "file.write", "ssh",
	"desktop", "host", "snapshot.read", "snapshot.write", "environment.read", "environment.write",
	"account.read", "account.admin", "*",
}

var fullSandbox = []string{"sandbox.read", "sandbox.update", "sandbox.stop", "agent.prompt", "exec", "file.read", "file.write",
	"ssh", "desktop", "host", "snapshot.read", "snapshot.write", "environment.read", "account.read"}

// Presets maps preset names to actions.
var Presets = map[string][]string{
	"read-only":    {"sandbox.read", "file.read", "snapshot.read", "environment.read", "account.read"},
	"full-sandbox": fullSandbox,
	"ci":           append(append([]string{}, fullSandbox...), "sandbox.create", "sandbox.resume", "sandbox.fork", "sandbox.delete-own"),
	"admin":        {"*"},
}

// sandboxKeyActions are the rights of the in-sandbox (per-sandbox) key.
var sandboxKeyActions = []string{"sandbox.read", "sandbox.update", "sandbox.stop", "agent.prompt", "exec", "file.read",
	"file.write", "ssh", "desktop", "host", "snapshot.read", "snapshot.write"}

const (
	keyDefaultTTL = 90 * 24 * time.Hour
	keyMaxTTL     = 365 * 24 * time.Hour
)

// APIKey is a stored key (the secret is only known at creation/rotation).
type APIKey struct {
	ID, UserID, OrgID, Name, Prefix, LastFour, Preset, SandboxID, CreatedByKey, Lane string
	Actions                                                                          []string
	Sandboxes                                                                        []string // nil = all
	Environments                                                                     []string // nil = all
	CreatedAt                                                                        time.Time
	ExpiresAt, LastUsedAt, RevokedAt                                                 *time.Time
}

func (k *APIKey) hasAction(a string) bool { return slices.Contains(k.Actions, a) }

// Allows reports whether the key's actions cover action (sandbox scoping is
// checked separately by AllowsSandbox).
func (k *APIKey) Allows(action string) bool {
	if k.hasAction("*") || k.hasAction(action) {
		return true
	}
	switch action {
	case "account.read":
		return k.hasAction("account.admin")
	case "sandbox.delete":
		return k.hasAction("sandbox.delete-own") // narrowed per sandbox
	}
	return false
}

// Restricted reports a key limited to some sandboxes or environments.
func (k *APIKey) Restricted() bool {
	return k.Sandboxes != nil || k.Environments != nil || k.SandboxID != ""
}

// AllowsSandbox checks the key's sandbox/environment set and delete-own.
func (k *APIKey) AllowsSandbox(sb *Sandbox, action string) bool {
	if k.SandboxID != "" && k.SandboxID != sb.ID {
		return false
	}
	if k.Sandboxes != nil || k.Environments != nil {
		in := slices.Contains(k.Sandboxes, sb.ID) || (sb.EnvironmentID != "" && slices.Contains(k.Environments, sb.EnvironmentID)) ||
			(sb.Environment != "" && slices.Contains(k.Environments, sb.Environment)) || sb.CreatedByKey == k.ID
		if !in {
			return false
		}
	}
	if action == "sandbox.delete" && !k.hasAction("*") && !k.hasAction("sandbox.delete") {
		return sb.CreatedByKey == k.ID
	}
	return true
}

const keyCols = `id,user_id,COALESCE(org_id,''),name,key_prefix,last_four,actions,sandbox_ids,environment_ids,preset,COALESCE(sandbox_id,''),COALESCE(created_by_key,''),lane,created_at,expires_at,last_used_at,revoked_at`

func scanKey(sc interface{ Scan(...any) error }) (*APIKey, error) {
	k := &APIKey{}
	var acts, sbs, envs string
	var created int64
	var exp, used, rev sql.NullInt64
	if err := sc.Scan(&k.ID, &k.UserID, &k.OrgID, &k.Name, &k.Prefix, &k.LastFour, &acts, &sbs, &envs, &k.Preset,
		&k.SandboxID, &k.CreatedByKey, &k.Lane, &created, &exp, &used, &rev); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(acts), &k.Actions)
	if sbs != "*" {
		k.Sandboxes = []string{}
		_ = json.Unmarshal([]byte(sbs), &k.Sandboxes)
	}
	if envs != "*" {
		k.Environments = []string{}
		_ = json.Unmarshal([]byte(envs), &k.Environments)
	}
	k.CreatedAt = store.Time(created)
	k.ExpiresAt, k.LastUsedAt, k.RevokedAt = store.NullTime(exp), store.NullTime(used), store.NullTime(rev)
	return k, nil
}

func (s *Server) keyBySecret(secret string) (*APIKey, error) {
	return scanKey(s.DB.QueryRow(`SELECT `+keyCols+` FROM api_keys WHERE secret_hash=?`, hashToken(secret)))
}

func (s *Server) keyByID(id string) (*APIKey, error) {
	return scanKey(s.DB.QueryRow(`SELECT `+keyCols+` FROM api_keys WHERE id=?`, id))
}

type keySpec struct {
	Name         string
	Preset       string
	Actions      []string
	Sandboxes    []string // nil = all
	Environments []string // nil = all
	ExpiresAt    *time.Time
	OrgID        string
	SandboxID    string
	CreatedByKey string
}

func newKeySecret() string { return api.PrefixAPIKey + randToken(40) }

func scopeJSON(v []string) string {
	if v == nil {
		return "*"
	}
	return mustJSON(v)
}

// createKey stores a new key for u and returns it with its secret.
func (s *Server) createKey(u *User, sp keySpec) (*APIKey, string, error) {
	acts := sp.Actions
	if sp.Preset != "" {
		p, ok := Presets[sp.Preset]
		if !ok {
			return nil, "", Err(400, "invalid_preset", "Unknown preset "+sp.Preset+".")
		}
		acts = p
	}
	if len(acts) == 0 {
		acts, sp.Preset = []string{"*"}, "admin"
	}
	for _, a := range acts {
		if !slices.Contains(Actions, a) {
			return nil, "", Err(400, "invalid_action", "Unknown action "+a+".")
		}
	}
	secret := newKeySecret()
	id := "key_" + randToken(16)
	name := sp.Name
	if name == "" {
		name = "key " + time.Now().UTC().Format("2006-01-02 15:04")
	}
	_, err := s.DB.Exec(`INSERT INTO api_keys(id,secret_hash,user_id,org_id,name,key_prefix,last_four,actions,sandbox_ids,environment_ids,preset,sandbox_id,created_by_key,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, hashToken(secret), u.ID, nz(sp.OrgID), name, secret[:12], secret[len(secret)-4:], mustJSON(acts),
		scopeJSON(sp.Sandboxes), scopeJSON(sp.Environments), sp.Preset, nz(sp.SandboxID), nz(sp.CreatedByKey), store.Now(), store.NullMs(sp.ExpiresAt))
	if err != nil {
		return nil, "", err
	}
	k, err := s.keyByID(id)
	return k, secret, err
}

// rotateSandboxKey (re)issues the in-sandbox key of sb and returns its secret.
func (s *Server) rotateSandboxKey(sb *Sandbox) (string, error) {
	secret := newKeySecret()
	res, err := s.DB.Exec(`UPDATE api_keys SET secret_hash=?, key_prefix=?, last_four=? WHERE sandbox_id=? AND preset<>'sandbox-token' AND revoked_at IS NULL`,
		hashToken(secret), secret[:12], secret[len(secret)-4:], sb.ID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return secret, nil
	}
	u, err := s.UserByID(sb.OwnerID)
	if err != nil {
		return "", err
	}
	_, secret, err = s.createKey(u, keySpec{Name: "sandbox " + sb.ID, Actions: sandboxKeyActions, SandboxID: sb.ID, OrgID: sb.OrgID})
	return secret, err
}

// parseTTL reads "90d", "24h", "30m", seconds, or "never".
func parseTTL(v string) (time.Duration, bool, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return keyDefaultTTL, false, nil
	}
	if v == "never" {
		return 0, true, nil
	}
	mult := time.Second
	switch {
	case strings.HasSuffix(v, "d"):
		mult, v = 24*time.Hour, strings.TrimSuffix(v, "d")
	case strings.HasSuffix(v, "h"):
		mult, v = time.Hour, strings.TrimSuffix(v, "h")
	case strings.HasSuffix(v, "m"):
		mult, v = time.Minute, strings.TrimSuffix(v, "m")
	case strings.HasSuffix(v, "s"):
		v = strings.TrimSuffix(v, "s")
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false, Err(400, "invalid_ttl", "ttl must look like 90d, 24h, 3600 or never.")
	}
	if int64(n) > int64(keyMaxTTL/mult) {
		return 0, false, Err(400, "invalid_ttl", "ttl can be at most 365d.")
	}
	d := time.Duration(n) * mult
	if d > keyMaxTTL {
		return 0, false, Err(400, "invalid_ttl", "ttl can be at most 365d.")
	}
	return d, false, nil
}

// canManageKeys: a session, or a key with "*"/account.admin.
func canManageKeys(p *Principal) bool {
	return p.Key == nil || p.Key.hasAction("*") || p.Key.hasAction("account.admin")
}

// covers reports whether a key with (acts, sbs, envs, exp) is within p's key.
func covers(p *Principal, acts, sbs, envs []string, exp *time.Time) bool {
	if p.Key == nil {
		return true
	}
	k := p.Key
	if !k.hasAction("*") {
		for _, a := range acts {
			// bugs-r11-sonnet 9: exact match; Allows maps sandbox.delete onto sandbox.delete-own.
			if a == "*" || !(k.hasAction(a) || (a == "account.read" && k.hasAction("account.admin"))) {
				return false
			}
		}
	}
	if k.Sandboxes != nil {
		if sbs == nil {
			return false
		}
		for _, id := range sbs {
			if !slices.Contains(k.Sandboxes, id) {
				return false
			}
		}
	}
	if k.Environments != nil {
		if envs == nil {
			return false
		}
		for _, id := range envs {
			if !slices.Contains(k.Environments, id) {
				return false
			}
		}
	}
	if k.ExpiresAt != nil && (exp == nil || exp.After(*k.ExpiresAt)) {
		return false
	}
	return k.SandboxID == ""
}

func keyCovered(p *Principal, k *APIKey) bool {
	if p.Key != nil && p.Key.ID == k.ID {
		return true
	}
	return covers(p, k.Actions, k.Sandboxes, k.Environments, k.ExpiresAt)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) apiKeyRoutes() {
	s.API("GET /api-keys", "account.read", s.handleListKeys)
	s.API("POST /api-keys/scoped", ActionAny, s.handleCreateKey)
	s.API("POST /api-keys", ActionAny, s.handleCreateKey)
	s.API("DELETE /api-keys/{apiKeyId}", ActionAny, s.handleRevokeKey)
	s.API("POST /api-keys/{apiKeyId}/rotate", ActionAny, s.handleRotateKey)
	s.API("GET /api-keys/{apiKeyId}/usage", "account.read", s.handleKeyUsage)
}

func (s *Server) keyCatalog(p *Principal) map[string]any {
	c := map[string]any{
		"actions": Actions, "presets": Presets, "defaultTtl": "90d", "maxTtl": "365d", "scopedCreationEnabled": s.scopedKeysEnabled(),
	}
	if p.Key == nil {
		var sbs []map[string]any
		rows, err := s.DB.Query(`SELECT id,name,COALESCE(org_id,'') FROM sandboxes WHERE owner_id=? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 500`, p.User.ID)
		if err == nil {
			for rows.Next() {
				var id, name, org string
				_ = rows.Scan(&id, &name, &org)
				sbs = append(sbs, map[string]any{"id": id, "name": name, "organizationId": org, "organizationName": ""})
			}
			rows.Close()
		}
		if sbs == nil {
			sbs = []map[string]any{}
		}
		c["scopeResources"] = map[string]any{"sandboxes": sbs, "environments": s.keyScopeEnvironments(p.User)}
	}
	return c
}

func (s *Server) userKeys(userID string, all bool) ([]*APIKey, error) {
	q := `SELECT ` + keyCols + ` FROM api_keys WHERE user_id=? AND revoked_at IS NULL`
	if !all {
		q += ` AND sandbox_id IS NULL`
	}
	rows, err := s.DB.Query(q+` ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Server) keysJSON(p *Principal, all bool) []map[string]any {
	out := []map[string]any{}
	if p.Key != nil && p.Key.Restricted() {
		return out
	}
	keys, _ := s.userKeys(p.User.ID, all)
	for _, k := range keys {
		if p.Key != nil && p.Key.OrgID != "" && k.OrgID != p.Key.OrgID {
			continue
		}
		out = append(out, s.keyJSON(k))
	}
	return out
}

func (s *Server) keyJSON(k *APIKey) map[string]any {
	s.usage.flush(s)
	var reqs int
	since := time.Now().UTC().AddDate(0, 0, -29).Format("2006-01-02")
	_ = s.DB.QueryRow(`SELECT COALESCE(SUM(requests),0) FROM api_key_usage WHERE key_id=? AND day>=?`, k.ID, since).Scan(&reqs)
	var nsb int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandboxes WHERE created_by_key=? AND deleted_at IS NULL`, k.ID).Scan(&nsb)
	var sbs, envs any = "*", "*"
	if k.Sandboxes != nil {
		sbs = k.Sandboxes
	}
	if k.Environments != nil {
		envs = k.Environments
	}
	expired := k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now())
	soon := k.ExpiresAt != nil && !expired && time.Until(*k.ExpiresAt) < 7*24*time.Hour
	var exp, last any
	if k.ExpiresAt != nil {
		exp = ts(*k.ExpiresAt)
	}
	if k.LastUsedAt != nil {
		last = ts(*k.LastUsedAt)
	}
	m := map[string]any{
		"id": k.ID, "name": k.Name, "credentialLane": k.Lane, "keyPrefix": k.Prefix, "keyLastFour": k.LastFour,
		"sandboxId": nz(k.SandboxID), "createdAt": ts(k.CreatedAt), "lastUsedAt": last,
		"usage":     map[string]any{"requests": reqs, "windowDays": 30},
		"resources": map[string]any{"total": nsb, "sandboxes": nsb, "agents": 0},
		"expiresAt": exp, "expired": expired, "expiringSoon": soon,
		"scope":  map[string]any{"expiresAt": exp, "actions": k.Actions, "sandboxes": sbs, "environments": envs, "grandfathered": false},
		"preset": nz(k.Preset), "org": nz(k.OrgID),
	}
	if k.RevokedAt != nil {
		m["revokedAt"] = ts(*k.RevokedAt)
	}
	return m
}

func (s *Server) handleListKeys(c *Ctx) error {
	all := c.Query("all") == "1" || c.Query("all") == "true"
	return c.OK(200, "api_key.list", map[string]any{"apiKeys": s.keysJSON(c.P, all), "catalog": s.keyCatalog(c.P)})
}

const (
	maxKeyName        = 120
	maxKeysPerAccount = 200
)

func (s *Server) handleCreateKey(c *Ctx) error {
	var req struct {
		Name           string    `json:"name"`
		TTL            string    `json:"ttl"`
		Preset         string    `json:"preset"`
		Actions        []string  `json:"actions"`
		SandboxIDs     *[]string `json:"sandboxIds"`
		EnvironmentIDs *[]string `json:"environmentIds"`
		Org            string    `json:"org"`
	}
	if err := c.BindStrict(&req); err != nil {
		return err
	}
	if !canManageKeys(c.P) {
		return Err(403, "api_key_action_forbidden", "Creating keys needs a browser/CLI session or an admin key.")
	}
	if len(req.Name) > maxKeyName {
		return Err(400, "invalid_name", fmt.Sprintf("name must be at most %d bytes.", maxKeyName))
	}
	var nKeys int
	// Keys the user made: not the per-sandbox ones the server mints, not
	// expired ones (bugs-r8-sonnet 6).
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id=? AND revoked_at IS NULL AND sandbox_id IS NULL
		AND (expires_at IS NULL OR expires_at>?)`, c.P.User.ID, store.Now()).Scan(&nKeys)
	if nKeys >= maxKeysPerAccount {
		return Err(409, "api_key_limit", fmt.Sprintf("An account can have up to %d active API keys. Revoke one first.", maxKeysPerAccount))
	}
	if req.Preset != "" && len(req.Actions) > 0 {
		return Err(400, "preset_and_actions", "Pass preset or actions, not both.")
	}
	d, never, err := parseTTL(req.TTL)
	if err != nil {
		return err
	}
	var exp *time.Time
	if !never {
		t := time.Now().Add(d).UTC()
		exp = &t
	}
	acts := req.Actions
	if req.Preset != "" {
		p, ok := Presets[req.Preset]
		if !ok {
			return Err(400, "invalid_preset", "preset must be read-only, full-sandbox, ci or admin.")
		}
		acts = p
	}
	if len(acts) == 0 {
		acts = []string{"*"}
	}
	var sbs, envs []string
	if req.SandboxIDs != nil {
		sbs = *req.SandboxIDs
		if sbs == nil {
			sbs = []string{}
		}
	}
	if req.EnvironmentIDs != nil {
		envs = *req.EnvironmentIDs
		if envs == nil {
			envs = []string{}
		}
	}
	if !covers(c.P, acts, sbs, envs, exp) {
		return Err(403, "api_key_scope_exceeded", "A new key cannot have more actions, sandboxes or a later expiry than the key that creates it.")
	}
	orgRef := req.Org
	if orgRef == "" {
		orgRef = orgHeader(c.R)
	}
	// Boat: a key made while an organization is active is that org's key
	// (dashboard with the org selected, `api-key create` after `org switch`).
	if orgRef == "" && (c.P.Key == nil || c.P.Key.OrgID == "") && c.P.User.ActiveOrg != "" {
		orgRef = c.P.User.ActiveOrg
	}
	orgID := ""
	if c.P.Key != nil && c.P.Key.OrgID != "" {
		orgID = c.P.Key.OrgID
	}
	if orgRef != "" {
		o, err := s.resolveOrgRef(c.P.User, orgRef)
		if err != nil {
			return err
		}
		if o != nil {
			orgID = o.ID
		} else {
			orgID = ""
		}
	}
	k, secret, err := s.createKey(c.P.User, keySpec{Name: req.Name, Preset: req.Preset, Actions: acts, Sandboxes: sbs, Environments: envs,
		ExpiresAt: exp, OrgID: orgID, CreatedByKey: keyID(c.P)})
	if err != nil {
		return err
	}
	return c.OK(201, "api_key.created", map[string]any{"apiKey": s.keyJSON(k), "secret": secret, "apiKeys": s.keysJSON(c.P, false)})
}

func keyID(p *Principal) string {
	if p.Key != nil {
		return p.Key.ID
	}
	return ""
}

// ownKey loads a key the caller may rotate or revoke (Boat
// api-keys__create-and-list "What a key cannot do"): a browser or CLI
// session, or a key that can create keys, which then manages itself and
// the keys it could create. Any other key, including on itself, gets 403.
func (s *Server) ownKey(c *Ctx, verb string) (*APIKey, error) {
	k, err := s.keyByID(c.Param("apiKeyId"))
	if err != nil || k.UserID != c.P.User.ID {
		return nil, errNotFound("API key")
	}
	if !canManageKeys(c.P) {
		return nil, ErrD(403, "api_key_action_forbidden", "To "+verb+" an API key, sign in with a browser or CLI session (crowdvm login), or use a key that can create keys (admin scope).",
			map[string]any{"needs": "session or account.admin"})
	}
	if !keyCovered(c.P, k) {
		return nil, Err(403, "api_key_scope_exceeded", "This key can only "+verb+" itself and keys with no more access than it has.")
	}
	return k, nil
}

func (s *Server) handleRevokeKey(c *Ctx) error {
	k, err := s.ownKey(c, "revoke")
	if err != nil {
		return err
	}
	if k.RevokedAt == nil {
		_, _ = s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE id=?`, store.Now(), k.ID)
	}
	return c.OK(200, "api_key.revoked", map[string]any{"id": k.ID, "apiKeys": s.keysJSON(c.P, false)})
}

func (s *Server) handleRotateKey(c *Ctx) error {
	k, err := s.ownKey(c, "rotate")
	if err != nil {
		return err
	}
	if k.RevokedAt != nil {
		return errNotFound("API key")
	}
	if k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now()) {
		return Err(409, "api_key_expired", "The key has expired and cannot be rotated. Create a new key.")
	}
	secret := newKeySecret()
	if _, err := s.DB.Exec(`UPDATE api_keys SET secret_hash=?, key_prefix=?, last_four=? WHERE id=?`, hashToken(secret), secret[:12], secret[len(secret)-4:], k.ID); err != nil {
		return err
	}
	k, _ = s.keyByID(k.ID)
	return c.OK(200, "api_key.rotated", map[string]any{"apiKey": s.keyJSON(k), "secret": secret, "apiKeys": s.keysJSON(c.P, false)})
}

func (s *Server) handleKeyUsage(c *Ctx) error {
	if c.P.Key != nil && c.P.Key.Restricted() {
		return Err(403, "api_key_restricted", "Restricted keys cannot read API key usage.")
	}
	k, err := s.keyByID(c.Param("apiKeyId"))
	if err != nil || k.UserID != c.P.User.ID {
		return errNotFound("API key")
	}
	m := s.keyJSON(k)
	res := []map[string]any{}
	rows, err := s.DB.Query(`SELECT id,name,state,created_at FROM sandboxes WHERE created_by_key=? AND deleted_at IS NULL ORDER BY created_at DESC`, k.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, name, state string
		var created int64
		_ = rows.Scan(&id, &name, &state, &created)
		res = append(res, map[string]any{"kind": "sandbox", "id": id, "name": name, "state": state, "createdAt": tsMs(created)})
	}
	rows.Close()
	m["createdResources"] = res
	return c.OK(200, "api_key.usage", m)
}
