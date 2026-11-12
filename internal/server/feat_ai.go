package server

// AI credentials: credential profiles (sealed at rest, never returned),
// several accounts per credential with switch/forget, custom APIs, the AI
// credentials proxy switch per account/org, per-sandbox block, and usage
// counted by the proxy (feat_ai_proxy.go).

import (
	"database/sql"
	"encoding/json"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

func init() {
	store.Register("ai_credentials", store.Migration{Version: 1, SQL: `
CREATE TABLE credential_profiles (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  org INTEGER NOT NULL DEFAULT 0,
  name TEXT NOT NULL,
  is_default INTEGER NOT NULL DEFAULT 0,
  access TEXT NOT NULL DEFAULT '{"restricted":false,"members":[]}',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX credential_profiles_owner ON credential_profiles(owner_id);
CREATE TABLE credential_accounts (
  id TEXT PRIMARY KEY,
  profile_id TEXT NOT NULL,
  credential TEXT NOT NULL,
  label TEXT NOT NULL,
  active INTEGER NOT NULL DEFAULT 0,
  secret TEXT NOT NULL,
  seq INTEGER NOT NULL,
  added_at INTEGER NOT NULL
);
CREATE INDEX credential_accounts_profile ON credential_accounts(profile_id, credential);
CREATE TABLE credential_custom_apis (
  profile_id TEXT NOT NULL,
  name TEXT NOT NULL,
  value TEXT NOT NULL,
  base_url TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (profile_id, name)
);
CREATE TABLE ai_proxy_settings (
  account_id TEXT PRIMARY KEY,
  enabled INTEGER NOT NULL DEFAULT 0,
  enabled_at INTEGER,
  updated_at INTEGER NOT NULL
);
CREATE TABLE ai_proxy_tokens (
  token_hash TEXT PRIMARY KEY,
  sandbox_id TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  token TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX ai_proxy_tokens_sandbox ON ai_proxy_tokens(sandbox_id);
CREATE TABLE ai_usage (
  day TEXT NOT NULL,
  sandbox_id TEXT NOT NULL,
  wallet TEXT NOT NULL,
  profile_id TEXT NOT NULL,
  credential TEXT NOT NULL,
  model TEXT NOT NULL,
  requests INTEGER NOT NULL DEFAULT 0,
  errors INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, sandbox_id, profile_id, credential, model)
);
CREATE INDEX ai_usage_wallet ON ai_usage(wallet, day);
CREATE INDEX ai_usage_profile ON ai_usage(profile_id, day);
`})
	RegisterFeature("ai_credentials", func(s *Server) error {
		s.aiRoutes()
		s.Bus.Subscribe(s.aiOnEvent)
		return nil
	})
}

func (s *Server) aiRoutes() {
	const admin = "account.admin"
	s.API("GET /credential-profiles", admin, s.handleAIProfiles)
	s.API("POST /credential-profiles", admin, s.handleAIProfileCreate)
	s.API("PUT /credential-profiles/{profile}", admin, s.handleAIProfileUpdate)
	s.API("DELETE /credential-profiles/{profile}", admin, s.handleAIProfileDelete)
	s.API("PUT /credential-profiles/{profile}/keys/{credential}", admin, s.handleAISetKey)
	s.API("DELETE /credential-profiles/{profile}/keys/{credential}", admin, s.handleAIDeleteKey)
	s.API("PUT /credential-profiles/{profile}/sign-ins/{credential}", admin, s.handleAISetSignIn)
	s.API("GET /credential-profiles/{profile}/accounts/{credential}", admin, s.handleAIAccounts)
	s.API("POST /credential-profiles/{profile}/accounts/{credential}", admin, s.handleAIAddAccount)
	s.API("PUT /credential-profiles/{profile}/accounts/{credential}/{account}/active", admin, s.handleAISwitchAccount)
	s.API("DELETE /credential-profiles/{profile}/accounts/{credential}/{account}", admin, s.handleAIForgetAccount)
	s.API("PUT /credential-profiles/{profile}/custom-apis/{name}", admin, s.handleAISetCustomAPI)
	s.API("DELETE /credential-profiles/{profile}/custom-apis/{name}", admin, s.handleAIDeleteCustomAPI)
	s.API("GET /ai-proxy", admin, s.handleAIProxyGet)
	s.API("PUT /ai-proxy", admin, s.handleAIProxyPut)
	s.API("GET /ai-usage", "account.read", s.handleAIUsage)
	s.API("GET /sandboxes/{sandboxId}/ai-usage", "sandbox.read", s.handleAISandboxUsage)
	s.API("PUT /sandboxes/{sandboxId}/ai-access", "sandbox.update", s.handleAIAccess)
	s.router.HandleFunc("/ai-proxy/{vendor}/{rest...}", s.handleAIProxy)
	s.router.HandleFunc("/ai-proxy/{vendor}", s.handleAIProxy)
}

// ---------------------------------------------------------------------------
// Credential catalog
// ---------------------------------------------------------------------------

type aiCred struct {
	ID, Kind, Vendor, EnvVar, Link string // Kind: api-key | sign-in | bedrock; Vendor "" = not proxied
}

var aiCreds = []aiCred{
	{"anthropic-api-key", "api-key", "anthropic", "ANTHROPIC_API_KEY", "https://platform.claude.com/settings/keys"},
	{"openai-api-key", "api-key", "openai", "OPENAI_API_KEY", "https://platform.openai.com/api-keys"},
	{"openrouter-api-key", "api-key", "openrouter", "OPENROUTER_API_KEY", "https://openrouter.ai/settings/keys"},
	{"llmgateway-api-key", "api-key", "llmgateway", "LLMGATEWAY_API_KEY", "https://llmgateway.io/dashboard"},
	{"deepseek-api-key", "api-key", "deepseek", "DEEPSEEK_API_KEY", "https://platform.deepseek.com/api_keys"},
	{"moonshot-api-key", "api-key", "moonshot", "MOONSHOT_API_KEY", "https://platform.kimi.ai/console/api-keys"},
	{"mistral-api-key", "api-key", "mistral", "MISTRAL_API_KEY", "https://console.mistral.ai/api-keys"},
	{"xai-api-key", "api-key", "xai", "XAI_API_KEY", "https://console.x.ai/team/default/api-keys"},
	{"opencode-api-key", "api-key", "opencode", "OPENCODE_API_KEY", "https://opencode.ai/console"},
	{"claude-subscription", "sign-in", "anthropic", "CLAUDE_CODE_OAUTH_TOKEN", "https://claude.ai/upgrade"},
	{"chatgpt-subscription", "sign-in", "openai", "", "https://chatgpt.com/#pricing"},
	{"kimi-subscription", "sign-in", "", "KIMI_CODE_ACCESS_TOKEN", "https://www.kimi.ai/membership/pricing"},
	{"mistral-vibe-subscription", "sign-in", "mistral", "MISTRAL_API_KEY", "https://mistral.ai/pricing"},
	{"grok-subscription", "sign-in", "xai", "XAI_API_KEY", "https://grok.com/supergrok"},
	{"claude-bedrock", "bedrock", "", "AWS_BEARER_TOKEN_BEDROCK", "https://console.aws.amazon.com/bedrock/home#/api-keys"},
	{"codex-bedrock", "bedrock", "", "AWS_BEARER_TOKEN_BEDROCK", "https://console.aws.amazon.com/bedrock/home#/api-keys"},
}

func aiCredByID(id string) (aiCred, bool) {
	for _, c := range aiCreds {
		if c.ID == id {
			return c, true
		}
	}
	return aiCred{}, false
}

// aiVendorCreds: the credentials the proxy may use for a vendor, preferred first.
// aiProxiedVendors lists the proxy vendors active holds a credential for.
func aiProxiedVendors(active map[string]aiSecret) []string {
	out := []string{}
	for v, creds := range aiVendorCreds {
		for _, c := range creds {
			if _, ok := active[c]; ok {
				out = append(out, v)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

var aiVendorCreds = map[string][]string{
	"anthropic":  {"anthropic-api-key", "claude-subscription"},
	"openai":     {"openai-api-key", "chatgpt-subscription"},
	"openrouter": {"openrouter-api-key"},
	"llmgateway": {"llmgateway-api-key"},
	"deepseek":   {"deepseek-api-key"},
	"moonshot":   {"moonshot-api-key"},
	"mistral":    {"mistral-vibe-subscription", "mistral-api-key"},
	"xai":        {"grok-subscription", "xai-api-key"},
	"opencode":   {"opencode-api-key"},
}

// aiHarnessCreds: credentials each integrated harness can run on.
var aiHarnessCreds = map[string][]string{
	"claude":      {"anthropic-api-key", "claude-subscription", "deepseek-api-key", "openrouter-api-key", "claude-bedrock"},
	"codex":       {"openai-api-key", "chatgpt-subscription", "deepseek-api-key", "codex-bedrock"},
	"pi":          {"anthropic-api-key", "openai-api-key", "openrouter-api-key", "llmgateway-api-key", "mistral-api-key", "mistral-vibe-subscription", "xai-api-key", "grok-subscription", "opencode-api-key"},
	"prime-agent": {"anthropic-api-key", "openai-api-key", "openrouter-api-key", "llmgateway-api-key", "mistral-api-key", "mistral-vibe-subscription", "xai-api-key", "grok-subscription", "opencode-api-key"},
	"opencode":    {"anthropic-api-key", "openai-api-key", "openrouter-api-key", "llmgateway-api-key", "mistral-api-key", "mistral-vibe-subscription", "xai-api-key", "grok-subscription", "opencode-api-key"},
	"kimi":        {"moonshot-api-key", "kimi-subscription"},
	"mistral":     {"mistral-api-key", "mistral-vibe-subscription"},
}

type aiSecret struct {
	Value        string `json:"value"`
	RefreshToken string `json:"refreshToken,omitempty"`
	Region       string `json:"region,omitempty"`
	AccountID    string `json:"accountId,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"` // access token expiry, unix ms (0 = unknown)
	// Bedrock with IAM access keys: Value is the secret access key.
	AccessKeyID  string `json:"accessKeyId,omitempty"`
	SessionToken string `json:"sessionToken,omitempty"`
}

// ---------------------------------------------------------------------------
// Profiles
// ---------------------------------------------------------------------------

type aiProfile struct {
	ID, OwnerID, Name    string
	Org, IsDefault       bool
	Access               envAccess
	CreatedAt, UpdatedAt time.Time
}

const aiProfileCols = `id,owner_id,org,name,is_default,access,created_at,updated_at`

func scanAIProfile(sc interface{ Scan(...any) error }) (*aiProfile, error) {
	p := &aiProfile{}
	var access string
	var c, u int64
	if err := sc.Scan(&p.ID, &p.OwnerID, &p.Org, &p.Name, &p.IsDefault, &access, &c, &u); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(access), &p.Access)
	if p.Access.Members == nil {
		p.Access.Members = []string{}
	}
	p.CreatedAt, p.UpdatedAt = store.Time(c), store.Time(u)
	return p, nil
}

func (s *Server) aiProfileByID(id string) (*aiProfile, error) {
	return scanAIProfile(s.DB.QueryRow(`SELECT `+aiProfileCols+` FROM credential_profiles WHERE id=?`, id))
}

func (s *Server) aiProfileIsOrg(id string) bool {
	if id == "" {
		return false
	}
	p, err := s.aiProfileByID(id)
	return err == nil && p.Org
}

// aiDefaultProfile returns (creating) the owner's default profile.
func (s *Server) aiDefaultProfile(ownerID string, org bool) (*aiProfile, error) {
	p, err := scanAIProfile(s.DB.QueryRow(`SELECT `+aiProfileCols+` FROM credential_profiles WHERE owner_id=? ORDER BY is_default DESC, created_at LIMIT 1`, ownerID))
	if err == nil {
		if !p.IsDefault {
			_, _ = s.DB.Exec(`UPDATE credential_profiles SET is_default=1 WHERE id=?`, p.ID)
			p.IsDefault = true
		}
		return p, nil
	}
	if !store.IsNoRows(err) {
		return nil, err
	}
	return s.aiCreateProfile(ownerID, org, "default", true)
}

func (s *Server) aiCreateProfile(ownerID string, org bool, name string, isDefault bool) (*aiProfile, error) {
	id, now := envUUID(), store.Now()
	if _, err := s.DB.Exec(`INSERT INTO credential_profiles(id,owner_id,org,name,is_default,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		id, ownerID, boolInt(org), name, boolInt(isDefault), now, now); err != nil {
		return nil, err
	}
	return s.aiProfileByID(id)
}

// aiUsable lists the profiles u can use: personal, then organizations'.
func (s *Server) aiUsable(u *User) ([]*aiProfile, error) {
	if _, err := s.aiDefaultProfile(u.ID, false); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(`SELECT `+aiProfileCols+` FROM credential_profiles WHERE owner_id=? OR owner_id IN (SELECT org_id FROM org_members WHERE user_id=?) ORDER BY org, is_default DESC, created_at`, u.ID, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*aiProfile
	for rows.Next() {
		p, err := scanAIProfile(rows)
		if err != nil {
			return nil, err
		}
		if p.Org && !envCanUse(p.Access, s.memberRole(p.OwnerID, u.ID), u.ID) {
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) aiCanManage(u *User, p *aiProfile) bool {
	if !p.Org {
		return p.OwnerID == u.ID
	}
	return s.memberRole(p.OwnerID, u.ID) == "owner"
}

// aiFindProfile resolves {profile} (id or name) among the usable profiles.
func (s *Server) aiFindProfile(u *User, ref string, manage bool) (*aiProfile, error) {
	list, err := s.aiUsable(u)
	if err != nil {
		return nil, err
	}
	var hit *aiProfile
	for _, p := range list {
		if p.ID == ref {
			hit = p
			break
		}
	}
	if hit == nil {
		var named []*aiProfile
		for _, p := range list {
			if p.Name == ref {
				named = append(named, p)
			}
		}
		switch {
		case len(named) == 1 || (len(named) > 1 && !named[0].Org && named[1].Org):
			hit = named[0] // personal wins
		case len(named) > 1:
			return nil, Err(409, "ambiguous_profile", "Several profiles are named "+ref+". Pass the id.")
		}
	}
	if hit == nil {
		return nil, Err(404, "credential_profile_not_found", "Credential profile "+ref+" not found.")
	}
	if manage && !s.aiCanManage(u, hit) {
		return nil, Err(403, "forbidden", "Only the profile's owner can change it.")
	}
	return hit, nil
}

// aiProfileForEnv validates credentialProfileId on an environment.
func (s *Server) aiProfileForEnv(sc envScope, ref string) (string, error) {
	p, err := s.aiFindProfile(sc.User, ref, false)
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// aiProxyEnabled reports the proxy switch of an account (user or org).
func (s *Server) aiProxyEnabled(accountID string) bool {
	var on bool
	_ = s.DB.QueryRow(`SELECT enabled FROM ai_proxy_settings WHERE account_id=?`, accountID).Scan(&on)
	return on
}

func (s *Server) aiProxyOnForProfile(sc envScope, profileID string) bool {
	owner := sc.User.ID
	if profileID != "" {
		if p, err := s.aiProfileByID(profileID); err == nil {
			owner = p.OwnerID
		}
	}
	return s.aiProxyEnabled(owner) || (sc.Org != nil && s.aiProxyEnabled(sc.Org.ID))
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

type aiAccount struct {
	ID, ProfileID, Credential, Label string
	Active                           bool
	Seq                              int
	AddedAt                          time.Time
	sealed                           string
}

func (s *Server) aiAccounts(profileID, cred string) ([]*aiAccount, error) {
	q := `SELECT id,profile_id,credential,label,active,seq,added_at,secret FROM credential_accounts WHERE profile_id=?`
	args := []any{profileID}
	if cred != "" {
		q += ` AND credential=?`
		args = append(args, cred)
	}
	rows, err := s.DB.Query(q+` ORDER BY credential, seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*aiAccount
	for rows.Next() {
		a := &aiAccount{}
		var added int64
		if err := rows.Scan(&a.ID, &a.ProfileID, &a.Credential, &a.Label, &a.Active, &a.Seq, &added, &a.sealed); err != nil {
			return nil, err
		}
		a.AddedAt = store.Time(added)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Server) aiSecretOf(a *aiAccount) (aiSecret, error) {
	var sec aiSecret
	b, err := s.envOpen(a.sealed)
	if err != nil {
		return sec, err
	}
	return sec, json.Unmarshal(b, &sec)
}

// aiActive returns the active secret of every credential of a profile.
func (s *Server) aiActive(profileID string) map[string]aiSecret {
	out := map[string]aiSecret{}
	list, _ := s.aiAccounts(profileID, "")
	for _, a := range list {
		if a.Active {
			if sec, err := s.aiSecretOf(a); err == nil {
				out[a.Credential] = sec
			}
		}
	}
	return out
}

func aiLabel(sec aiSecret, label string) string {
	if label != "" {
		return label
	}
	v := sec.Value
	if sec.AccessKeyID != "" {
		v = sec.AccessKeyID // the key id, not the secret
	}
	if len(v) > 4 {
		v = v[len(v)-4:]
	}
	return "…" + v
}

// aiAddAccount stores a new account; active makes it the one in use.
func (s *Server) aiAddAccount(profileID, cred string, sec aiSecret, label string, active bool) (*aiAccount, error) {
	b, _ := json.Marshal(sec)
	sealed, err := s.envSeal(b)
	if err != nil {
		return nil, err
	}
	id := "acct_" + randToken(16)
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		var n, seq int
		if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(MAX(seq),0) FROM credential_accounts WHERE profile_id=? AND credential=?`, profileID, cred).Scan(&n, &seq); err != nil {
			return err
		}
		if n == 0 {
			active = true
		}
		if active {
			if _, err := tx.Exec(`UPDATE credential_accounts SET active=0 WHERE profile_id=? AND credential=?`, profileID, cred); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT INTO credential_accounts(id,profile_id,credential,label,active,secret,seq,added_at) VALUES(?,?,?,?,?,?,?,?)`,
			id, profileID, cred, aiLabel(sec, label), boolInt(active), sealed, seq+1, store.Now())
		return err
	})
	if err != nil {
		return nil, err
	}
	s.aiTouch(profileID)
	return &aiAccount{ID: id}, nil
}

func (s *Server) aiTouch(profileID string) {
	_, _ = s.DB.Exec(`UPDATE credential_profiles SET updated_at=? WHERE id=?`, store.Now(), profileID)
}

var aiKeyRe = regexp.MustCompile(`^[\x21-\x7e]+$`)

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

func (s *Server) aiOwnerJSON(p *aiProfile) map[string]any {
	kind, name := "personal", ""
	if p.Org {
		kind = "org"
		if o, err := s.OrgByID(p.OwnerID); err == nil {
			name = o.Name
		}
	} else if u, err := s.UserByID(p.OwnerID); err == nil {
		name = firstNonEmpty(u.Username, u.DisplayName)
	}
	return map[string]any{"id": p.OwnerID, "kind": kind, "name": name, "aiProxyEnabled": s.aiProxyEnabled(p.OwnerID)}
}

func aiAccountJSON(a *aiAccount) map[string]any {
	return map[string]any{"id": a.ID, "label": a.Label, "active": a.Active, "addedAt": ts(a.AddedAt)}
}

func (s *Server) aiProfileJSON(p *aiProfile, u *User) map[string]any {
	manage := s.aiCanManage(u, p)
	accts, _ := s.aiAccounts(p.ID, "")
	connected := []string{}
	byCred := map[string][]map[string]any{}
	for _, a := range accts {
		if !slices.Contains(connected, a.Credential) {
			connected = append(connected, a.Credential)
		}
		byCred[a.Credential] = append(byCred[a.Credential], aiAccountJSON(a))
	}
	accounts := map[string]any{}
	if manage {
		for k, v := range byCred {
			if len(v) > 1 {
				accounts[k] = v
			}
		}
	}
	custom := []string{}
	rows, err := s.DB.Query(`SELECT name FROM credential_custom_apis WHERE profile_id=? ORDER BY name`, p.ID)
	if err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				custom = append(custom, n)
			}
		}
		rows.Close()
	}
	return map[string]any{"id": p.ID, "name": p.Name, "isDefault": p.IsDefault, "owner": s.aiOwnerJSON(p), "canManage": manage,
		"connected": connected, "customApis": custom, "accounts": accounts, "access": p.Access,
		"createdAt": ts(p.CreatedAt), "updatedAt": ts(p.UpdatedAt)}
}

func (s *Server) aiProfileOK(c *Ctx, p *aiProfile, typ string) error {
	p, err := s.aiProfileByID(p.ID)
	if err != nil {
		return err
	}
	return c.OK(200, typ, map[string]any{"success": true, "profile": s.aiProfileJSON(p, c.P.User)})
}

// ---------------------------------------------------------------------------
// Profile handlers
// ---------------------------------------------------------------------------

func (s *Server) handleAIProfiles(c *Ctx) error {
	list, err := s.aiUsable(c.P.User)
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, p := range list {
		out = append(out, s.aiProfileJSON(p, c.P.User))
	}
	catalog := []map[string]any{}
	for _, cr := range aiCreds {
		catalog = append(catalog, map[string]any{"id": cr.ID, "kind": cr.Kind, "proxied": cr.Vendor != "", "envVar": nz(cr.EnvVar), "getUrl": cr.Link})
	}
	return c.OK(200, "credential_profile.list", map[string]any{"profiles": out, "credentials": catalog})
}

var aiProfileNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func (s *Server) handleAIProfileCreate(c *Ctx) error {
	var req struct {
		Name     string `json:"name"`
		Org      string `json:"org"`
		CopyFrom string `json:"copyFrom"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	u := c.P.User
	if !aiProfileNameRe.MatchString(req.Name) {
		return Err(400, "invalid_name", "name must be 1-64 letters, digits, '.', '_' or '-'.")
	}
	owner, isOrg := u.ID, false
	if req.Org != "" || (c.P.Key != nil && c.P.Key.OrgID != "") {
		org, err := c.Scope(req.Org)
		if err != nil {
			return err
		}
		if org != nil {
			if s.memberRole(org.ID, u.ID) != "owner" {
				return Err(403, "forbidden", "Only organization owners create organization profiles.")
			}
			owner, isOrg = org.ID, true
		}
	}
	var src *aiProfile
	if req.CopyFrom != "" {
		var err error
		if src, err = s.aiFindProfile(u, req.CopyFrom, true); err != nil {
			return err
		}
	}
	if !isOrg {
		if _, err := s.aiDefaultProfile(u.ID, false); err != nil {
			return err
		}
	}
	var n, total int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM credential_profiles WHERE owner_id=? AND name=?`, owner, req.Name).Scan(&n)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM credential_profiles WHERE owner_id=?`, owner).Scan(&total)
	if n > 0 {
		return Err(409, "profile_exists", "A profile named "+req.Name+" already exists.")
	}
	p, err := s.aiCreateProfile(owner, isOrg, req.Name, total == 0)
	if err != nil {
		return err
	}
	if src != nil {
		accts, _ := s.aiAccounts(src.ID, "")
		for _, a := range accts {
			if cr, _ := aiCredByID(a.Credential); cr.Kind != "api-key" {
				continue // sign-ins are not copied
			}
			if sec, err := s.aiSecretOf(a); err == nil {
				_, _ = s.aiAddAccount(p.ID, a.Credential, sec, a.Label, a.Active)
			}
		}
		_, _ = s.DB.Exec(`INSERT INTO credential_custom_apis(profile_id,name,value,base_url,updated_at) SELECT ?,name,value,base_url,? FROM credential_custom_apis WHERE profile_id=?`, p.ID, store.Now(), src.ID)
	}
	p, _ = s.aiProfileByID(p.ID)
	return c.OK(201, "credential_profile.created", map[string]any{"success": true, "profile": s.aiProfileJSON(p, u)})
}

func (s *Server) handleAIProfileUpdate(c *Ctx) error {
	var req struct {
		Name      *string          `json:"name"`
		IsDefault *bool            `json:"isDefault"`
		Access    *envAccessUpdate `json:"access"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	if req.Name != nil && *req.Name != p.Name {
		if !aiProfileNameRe.MatchString(*req.Name) {
			return Err(400, "invalid_name", "name must be 1-64 letters, digits, '.', '_' or '-'.")
		}
		var n int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM credential_profiles WHERE owner_id=? AND name=? AND id<>?`, p.OwnerID, *req.Name, p.ID).Scan(&n)
		if n > 0 {
			return Err(409, "profile_exists", "A profile named "+*req.Name+" already exists.")
		}
		_, _ = s.DB.Exec(`UPDATE credential_profiles SET name=?, updated_at=? WHERE id=?`, *req.Name, store.Now(), p.ID)
	}
	if req.Access != nil {
		var org *Org
		if p.Org {
			org, _ = s.OrgByID(p.OwnerID)
		}
		acc, err := s.envResolveAccess(org, req.Access)
		if err != nil {
			return err
		}
		_, _ = s.DB.Exec(`UPDATE credential_profiles SET access=?, updated_at=? WHERE id=?`, mustJSON(acc), store.Now(), p.ID)
	}
	if req.IsDefault != nil && *req.IsDefault && !p.IsDefault {
		if err := s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE credential_profiles SET is_default=0 WHERE owner_id=?`, p.OwnerID); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE credential_profiles SET is_default=1, updated_at=? WHERE id=?`, store.Now(), p.ID)
			return err
		}); err != nil {
			return err
		}
	}
	return s.aiProfileOK(c, p, "credential_profile.updated")
}

func (s *Server) handleAIProfileDelete(c *Ctx) error {
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	if p.IsDefault {
		return Err(409, "is_default", "The default profile cannot be deleted. Make another profile the default first.")
	}
	if s.aiProfileInUse(p.ID) {
		return Err(409, "in_use", "An environment names this profile. Change the environment first.")
	}
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM credential_accounts WHERE profile_id=?`, `DELETE FROM credential_custom_apis WHERE profile_id=?`, `DELETE FROM credential_profiles WHERE id=?`} {
			if _, err := tx.Exec(q, p.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return c.OK(200, "credential_profile.deleted", map[string]any{"success": true})
}

// aiProfileInUse: the latest version of a live environment names it.
func (s *Server) aiProfileInUse(id string) bool {
	rows, err := s.DB.Query(`SELECT v.config FROM environments e JOIN environment_versions v ON v.env_id=e.id AND v.number=e.latest_version WHERE e.deleted_at IS NULL`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var sealed string
		if rows.Scan(&sealed) != nil {
			continue
		}
		if b, err := s.envOpen(sealed); err == nil {
			var cfg envConfig
			if json.Unmarshal(b, &cfg) == nil && cfg.CredentialProfileID == id {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Keys, sign-ins, accounts
// ---------------------------------------------------------------------------

func (s *Server) aiCredParam(c *Ctx, wantKey bool) (aiCred, error) {
	cr, ok := aiCredByID(c.Param("credential"))
	if !ok {
		return cr, Err(400, "unknown_credential", "Unknown credential "+c.Param("credential")+".")
	}
	if wantKey && cr.Kind != "api-key" {
		return cr, Err(400, "not_an_api_key", cr.ID+" is a sign-in; connect it on the dashboard Agents & Proxying page (or PUT .../sign-ins/"+cr.ID+").")
	}
	return cr, nil
}

type aiKeyReq struct {
	Value        string `json:"value"`
	KeepCurrent  bool   `json:"keepCurrent"`
	Label        string `json:"label"`
	RefreshToken string `json:"refreshToken"`
	Region       string `json:"region"`
	AccountID    string `json:"accountId"`
	// ExpiresIn: seconds until the pasted access token expires (sign-ins
	// with a refreshToken are then renewed by the server).
	ExpiresIn int64 `json:"expiresIn"`
	// Bedrock IAM access keys: value is then the secret access key.
	AccessKeyID  string `json:"accessKeyId"`
	SessionToken string `json:"sessionToken"`
}

func (r *aiKeyReq) secret(cr aiCred) (aiSecret, error) {
	v := strings.TrimSpace(r.Value)
	if cr.ID == "chatgpt-subscription" && strings.HasPrefix(v, "{") {
		if !json.Valid([]byte(v)) || len(v) > 64<<10 {
			return aiSecret{}, Err(400, "invalid_key", "The ChatGPT sign-in must be the contents of ~/.codex/auth.json or an access token.")
		}
	} else if len(v) > 8192 || !aiKeyRe.MatchString(v) {
		return aiSecret{}, Err(400, "invalid_key", "A key is one token of up to 8192 printable characters, without spaces or line breaks.")
	}
	if cr.Kind == "bedrock" && r.Region == "" {
		return aiSecret{}, Err(400, "invalid_region", "Bedrock needs region (for example us-east-1).")
	}
	akid, st := strings.TrimSpace(r.AccessKeyID), strings.TrimSpace(r.SessionToken)
	if (akid != "" || st != "") && cr.Kind != "bedrock" {
		return aiSecret{}, Err(400, "invalid_key", "accessKeyId and sessionToken are for Bedrock IAM access keys only.")
	}
	if st != "" && akid == "" {
		return aiSecret{}, Err(400, "invalid_key", "sessionToken needs accessKeyId.")
	}
	if (akid != "" && !aiKeyRe.MatchString(akid)) || (st != "" && !aiKeyRe.MatchString(st)) {
		return aiSecret{}, Err(400, "invalid_key", "accessKeyId and sessionToken are single tokens without spaces or line breaks.")
	}
	sec := aiSecret{Value: v, RefreshToken: strings.TrimSpace(r.RefreshToken), Region: r.Region, AccountID: r.AccountID, AccessKeyID: akid, SessionToken: st}
	if r.ExpiresIn > 0 {
		sec.ExpiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).UnixMilli()
	}
	return sec, nil
}

// aiSetActive replaces the active account's secret (or adds an account
// when keepCurrent or none exists).
func (s *Server) aiSetActive(p *aiProfile, cr aiCred, req aiKeyReq) error {
	sec, err := req.secret(cr)
	if err != nil {
		return err
	}
	accts, _ := s.aiAccounts(p.ID, cr.ID)
	var active *aiAccount
	for _, a := range accts {
		if a.Active {
			active = a
		}
	}
	if active == nil || req.KeepCurrent {
		_, err = s.aiAddAccount(p.ID, cr.ID, sec, req.Label, true)
	} else {
		b, _ := json.Marshal(sec)
		sealed, serr := s.envSeal(b)
		if serr != nil {
			return serr
		}
		_, err = s.DB.Exec(`UPDATE credential_accounts SET secret=?, label=? WHERE id=?`, sealed, aiLabel(sec, req.Label), active.ID)
		s.aiTouch(p.ID)
	}
	if err == nil {
		s.envPushProfile(p.ID)
	}
	return err
}

func (s *Server) handleAISetKey(c *Ctx) error {
	var req aiKeyReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	cr, err := s.aiCredParam(c, true)
	if err != nil {
		return err
	}
	if err := s.aiSetActive(p, cr, req); err != nil {
		return err
	}
	return s.aiProfileOK(c, p, "credential_profile.updated")
}

// handleAISetSignIn (CrowdVM): stores a subscription sign-in or Bedrock
// credential from a token the user obtained with the harness's own login
// (claude setup-token, codex login, kimi login), since there is no hosted
// OAuth client.
func (s *Server) handleAISetSignIn(c *Ctx) error {
	var req aiKeyReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	cr, err := s.aiCredParam(c, false)
	if err != nil {
		return err
	}
	if cr.Kind == "api-key" {
		return Err(400, "not_a_sign_in", cr.ID+" is an API key; use PUT .../keys/"+cr.ID+".")
	}
	if err := s.aiSetActive(p, cr, req); err != nil {
		return err
	}
	return s.aiProfileOK(c, p, "credential_profile.updated")
}

func (s *Server) handleAIDeleteKey(c *Ctx) error {
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	cr, err := s.aiCredParam(c, false)
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(`DELETE FROM credential_accounts WHERE profile_id=? AND credential=?`, p.ID, cr.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Err(404, "credential_not_found", "The profile has no "+cr.ID+".")
	}
	s.aiTouch(p.ID)
	s.envPushProfile(p.ID)
	return s.aiProfileOK(c, p, "credential_profile.updated")
}

func (s *Server) aiAccountsOK(c *Ctx, p *aiProfile, cr aiCred, status int) error {
	accts, err := s.aiAccounts(p.ID, cr.ID)
	if err != nil {
		return err
	}
	out := []map[string]any{}
	for _, a := range accts {
		out = append(out, aiAccountJSON(a))
	}
	return c.OK(status, "credential_account.list", map[string]any{"success": true, "profileId": p.ID, "credential": cr.ID, "accounts": out})
}

func (s *Server) handleAIAccounts(c *Ctx) error {
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	cr, err := s.aiCredParam(c, false)
	if err != nil {
		return err
	}
	return s.aiAccountsOK(c, p, cr, 200)
}

func (s *Server) handleAIAddAccount(c *Ctx) error {
	var req aiKeyReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	cr, err := s.aiCredParam(c, true)
	if err != nil {
		return err
	}
	sec, err := req.secret(cr)
	if err != nil {
		return err
	}
	if _, err := s.aiAddAccount(p.ID, cr.ID, sec, req.Label, false); err != nil {
		return err
	}
	s.envPushProfile(p.ID)
	return s.aiAccountsOK(c, p, cr, 201)
}

func (s *Server) aiAccountParam(c *Ctx, p *aiProfile, cr aiCred) (*aiAccount, []*aiAccount, error) {
	accts, err := s.aiAccounts(p.ID, cr.ID)
	if err != nil {
		return nil, nil, err
	}
	ref := c.Param("account")
	for i, a := range accts {
		if a.ID == ref || strconv.Itoa(i+1) == ref || a.Label == ref || (strings.HasPrefix(ref, "…") && a.Label == ref) {
			return a, accts, nil
		}
	}
	return nil, accts, Err(404, "account_not_found", "Account "+ref+" not found.")
}

func (s *Server) handleAISwitchAccount(c *Ctx) error {
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	cr, err := s.aiCredParam(c, false)
	if err != nil {
		return err
	}
	a, _, err := s.aiAccountParam(c, p, cr)
	if err != nil {
		return err
	}
	if !a.Active {
		if err := s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE credential_accounts SET active=0 WHERE profile_id=? AND credential=?`, p.ID, cr.ID); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE credential_accounts SET active=1 WHERE id=?`, a.ID)
			return err
		}); err != nil {
			return err
		}
		s.aiTouch(p.ID)
		s.envPushProfile(p.ID)
	}
	return s.aiAccountsOK(c, p, cr, 200)
}

func (s *Server) handleAIForgetAccount(c *Ctx) error {
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	cr, err := s.aiCredParam(c, false)
	if err != nil {
		return err
	}
	a, all, err := s.aiAccountParam(c, p, cr)
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(`DELETE FROM credential_accounts WHERE id=?`, a.ID); err != nil {
		return err
	}
	if a.Active {
		for _, o := range all {
			if o.ID != a.ID {
				_, _ = s.DB.Exec(`UPDATE credential_accounts SET active=1 WHERE id=?`, o.ID)
				break
			}
		}
	}
	s.aiTouch(p.ID)
	s.envPushProfile(p.ID)
	return s.aiAccountsOK(c, p, cr, 200)
}

// ---------------------------------------------------------------------------
// Custom APIs
// ---------------------------------------------------------------------------

var (
	aiCustomNameRe   = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)
	aiReservedPrefix = []string{"ANTHROPIC_", "OPENAI_", "OPENROUTER_", "LLMGATEWAY_", "DEEPSEEK_", "MOONSHOT_", "MISTRAL_", "XAI_", "OPENCODE_",
		"AWS_", "CLAUDE_", "CODEX_", "CHATGPT_", "KIMI_", "GROK_", "GEMINI_", "GOOGLE_", "CROWDVM_", "BOAT_", "ASCII_", "GH_", "GITHUB_"}
)

func aiValidCustomName(n string) bool {
	if !aiCustomNameRe.MatchString(n) || strings.HasSuffix(n, "_BASE_URL") || slices.Contains(reservedEnv, n) || (n[0] >= '0' && n[0] <= '9') {
		return false
	}
	for _, p := range aiReservedPrefix {
		if strings.HasPrefix(n, p) {
			return false
		}
	}
	return true
}

// aiValidBaseURL: HTTPS, default port, public DNS name; with
// CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE=1 also http://, ports, LAN hosts and IPs.
func aiValidBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if os.Getenv("CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE") == "1" {
		return u.Scheme == "https" || u.Scheme == "http"
	}
	h := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || (u.Port() != "" && u.Port() != "443") || net.ParseIP(h) != nil || !strings.Contains(h, ".") {
		return false
	}
	for _, suf := range []string{".local", ".lan", ".internal", ".home", ".localhost", ".ts.net"} {
		if strings.HasSuffix(h, suf) {
			return false
		}
	}
	return true
}

func (s *Server) handleAISetCustomAPI(c *Ctx) error {
	var req struct {
		Value   string `json:"value"`
		BaseURL string `json:"baseUrl"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	name := c.Param("name")
	if !aiValidCustomName(name) {
		return Err(400, "invalid_custom_api_name", "A custom API name is A-Z, 0-9 and _ (up to 64), not a provider name and not ending in _BASE_URL.")
	}
	if req.Value == "" || len(req.Value) > 8192 {
		return Err(400, "invalid_value", "value is required (up to 8192 characters).")
	}
	if !aiValidBaseURL(req.BaseURL) {
		return Err(400, "invalid_base_url", "baseUrl must be an HTTPS URL on the default port with a public DNS name.")
	}
	sealed, err := s.envSeal([]byte(req.Value))
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(`INSERT INTO credential_custom_apis(profile_id,name,value,base_url,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(profile_id,name) DO UPDATE SET value=excluded.value, base_url=excluded.base_url, updated_at=excluded.updated_at`,
		p.ID, name, sealed, strings.TrimRight(req.BaseURL, "/"), store.Now()); err != nil {
		return err
	}
	s.aiTouch(p.ID)
	s.envPushProfile(p.ID)
	return s.aiProfileOK(c, p, "credential_profile.updated")
}

func (s *Server) handleAIDeleteCustomAPI(c *Ctx) error {
	p, err := s.aiFindProfile(c.P.User, c.Param("profile"), true)
	if err != nil {
		return err
	}
	res, err := s.DB.Exec(`DELETE FROM credential_custom_apis WHERE profile_id=? AND name=?`, p.ID, c.Param("name"))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Err(404, "custom_api_not_found", "The profile has no custom API "+c.Param("name")+".")
	}
	s.aiTouch(p.ID)
	s.envPushProfile(p.ID)
	return s.aiProfileOK(c, p, "credential_profile.updated")
}

type aiCustomAPI struct{ Name, Value, BaseURL string }

func (s *Server) aiCustomAPIs(profileID string) []aiCustomAPI {
	rows, err := s.DB.Query(`SELECT name,value,base_url FROM credential_custom_apis WHERE profile_id=? ORDER BY name`, profileID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []aiCustomAPI
	for rows.Next() {
		var a aiCustomAPI
		var sealed string
		if rows.Scan(&a.Name, &sealed, &a.BaseURL) != nil {
			continue
		}
		if b, err := s.envOpen(sealed); err == nil {
			a.Value = string(b)
			out = append(out, a)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Proxy switch, block, usage
// ---------------------------------------------------------------------------

func aiPricing() (daily, per1000 float64) {
	daily, _ = strconv.ParseFloat(os.Getenv("CROWDVM_AI_PROXY_DAILY_CREDITS"), 64)
	per1000, _ = strconv.ParseFloat(os.Getenv("CROWDVM_AI_PROXY_CREDITS_PER_1000"), 64)
	return
}

func (s *Server) aiProxyJSON(account string, org bool) map[string]any {
	var on bool
	var at sql.NullInt64
	_ = s.DB.QueryRow(`SELECT enabled, enabled_at FROM ai_proxy_settings WHERE account_id=?`, account).Scan(&on, &at)
	daily, per := aiPricing()
	var enabledAt any
	if on {
		enabledAt = tsNull(at)
	}
	return map[string]any{"success": true, "accountId": account, "org": org, "enabled": on, "enabledAt": enabledAt,
		"pricing":  map[string]any{"dailyDollars": daily, "dollarsPer1000Requests": per, "unit": "credits"},
		"proxyUrl": strings.TrimRight(s.Cfg.PublicURL, "/") + "/ai-proxy", "guestProxyUrl": s.envGuestURL() + "/ai-proxy"}
}

func (s *Server) handleAIProxyGet(c *Ctx) error {
	org, err := c.Scope("")
	if err != nil {
		return err
	}
	if org != nil {
		return c.OK(200, "ai_proxy.info", s.aiProxyJSON(org.ID, true))
	}
	return c.OK(200, "ai_proxy.info", s.aiProxyJSON(c.P.User.ID, false))
}

func (s *Server) handleAIProxyPut(c *Ctx) error {
	var req struct {
		Enabled *bool  `json:"enabled"`
		Org     string `json:"org"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.Enabled == nil {
		return Err(400, "missing_enabled", "enabled (true or false) is required.")
	}
	org, err := c.Scope(req.Org)
	if err != nil {
		return err
	}
	account := c.P.User.ID
	if org != nil {
		if s.memberRole(org.ID, c.P.User.ID) != "owner" {
			return Err(403, "forbidden", "Only an organization owner can change the organization's proxy switch.")
		}
		account = org.ID
	}
	now := store.Now()
	if _, err := s.DB.Exec(`INSERT INTO ai_proxy_settings(account_id,enabled,enabled_at,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET enabled_at=CASE WHEN excluded.enabled=1 AND ai_proxy_settings.enabled=0 THEN excluded.enabled_at ELSE ai_proxy_settings.enabled_at END,
		enabled=excluded.enabled, updated_at=excluded.updated_at`, account, boolInt(*req.Enabled), now, now); err != nil {
		return err
	}
	return c.OK(200, "ai_proxy.updated", s.aiProxyJSON(account, org != nil))
}

func (s *Server) handleAIAccess(c *Ctx) error {
	var req struct {
		Blocked *bool `json:"blocked"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.Blocked == nil {
		return Err(400, "missing_blocked", "blocked (true or false) is required.")
	}
	sb, err := s.SandboxByID(c.Param("sandboxId"))
	if err != nil || sb.DeletedAt != nil {
		return errNotFound("Sandbox")
	}
	// The creator, or a manager of the sandbox's profile.
	allowed := sb.OwnerID == c.P.User.ID
	if !allowed {
		if pid := envManagedOf(sb).ProfileID; pid != "" {
			if p, err := s.aiProfileByID(pid); err == nil && s.aiCanManage(c.P.User, p) {
				allowed = true
			}
		}
	}
	if !allowed {
		if s.accessOf(c.P, sb) == "" {
			return errNotFound("Sandbox")
		}
		return Err(403, "forbidden", "Only the sandbox's creator or the managers of its credential profile can block it.")
	}
	if c.P.Key != nil && !c.P.Key.AllowsSandbox(sb, "sandbox.update") {
		return Err(403, "api_key_sandbox_forbidden", "The API key is not allowed on this sandbox.")
	}
	if err := s.SetSandboxExtra(sb.ID, "aiBlocked", *req.Blocked); err != nil {
		return err
	}
	return c.OK(200, "sandbox.ai_access_updated", map[string]any{"success": true, "sandboxId": sb.ID, "blocked": *req.Blocked})
}

type aiTotals struct {
	Requests, Errors, Input, Output, CacheRead, CacheWrite int64
}

func (t *aiTotals) add(o aiTotals) {
	t.Requests += o.Requests
	t.Errors += o.Errors
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
}

func (t aiTotals) json() map[string]any {
	return map[string]any{"requests": t.Requests, "errors": t.Errors, "inputTokens": t.Input, "outputTokens": t.Output,
		"cacheReadTokens": t.CacheRead, "cacheWriteTokens": t.CacheWrite}
}

type aiUsageRow struct {
	Day, SandboxID, Credential, Model string
	aiTotals
}

func aiDays(c *Ctx) (int, string, error) {
	days := 30
	if v := c.Query("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			return 0, "", Err(400, "invalid_days", "days must be 1 to 365.")
		}
		days = n
	}
	return days, time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02"), nil
}

func (s *Server) aiUsageRows(where string, args ...any) ([]aiUsageRow, error) {
	rows, err := s.DB.Query(`SELECT day,sandbox_id,credential,model,SUM(requests),SUM(errors),SUM(input_tokens),SUM(output_tokens),SUM(cache_read_tokens),SUM(cache_write_tokens)
		FROM ai_usage WHERE `+where+` GROUP BY day,sandbox_id,credential,model ORDER BY day,sandbox_id,credential,model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aiUsageRow
	for rows.Next() {
		var r aiUsageRow
		if err := rows.Scan(&r.Day, &r.SandboxID, &r.Credential, &r.Model, &r.Requests, &r.Errors, &r.Input, &r.Output, &r.CacheRead, &r.CacheWrite); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// aiAggregate groups rows by day/credential/model.
func aiAggregate(rows []aiUsageRow) ([]map[string]any, aiTotals) {
	type key struct{ d, c, m string }
	sums := map[key]*aiTotals{}
	var order []key
	var tot aiTotals
	for _, r := range rows {
		k := key{r.Day, r.Credential, r.Model}
		if sums[k] == nil {
			sums[k] = &aiTotals{}
			order = append(order, k)
		}
		sums[k].add(r.aiTotals)
		tot.add(r.aiTotals)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].d < order[j].d })
	out := []map[string]any{}
	for _, k := range order {
		m := sums[k].json()
		m["day"], m["credential"], m["model"] = k.d, k.c, k.m
		out = append(out, m)
	}
	return out, tot
}

func aiDollars(requests int64) float64 {
	_, per := aiPricing()
	return math.Round(float64(requests)*per/1000*1e6) / 1e6
}

func (s *Server) handleAIUsage(c *Ctx) error {
	_, since, err := aiDays(c)
	if err != nil {
		return err
	}
	var rows []aiUsageRow
	var account, profile any
	if ref := c.Query("profile"); ref != "" {
		p, err := s.aiFindProfile(c.P.User, ref, true)
		if err != nil {
			return err
		}
		profile = p.ID
		rows, err = s.aiUsageRows(`profile_id=? AND day>=?`, p.ID, since)
		if err != nil {
			return err
		}
	} else {
		org, err := c.Scope("")
		if err != nil {
			return err
		}
		wallet := c.P.User.ID
		if org != nil {
			wallet = org.ID
		}
		account = wallet
		if rows, err = s.aiUsageRows(`wallet=? AND day>=?`, wallet, since); err != nil {
			return err
		}
	}
	bySb := map[string][]aiUsageRow{}
	var ids []string
	for _, r := range rows {
		if _, ok := bySb[r.SandboxID]; !ok {
			ids = append(ids, r.SandboxID)
		}
		bySb[r.SandboxID] = append(bySb[r.SandboxID], r)
	}
	sbs := []map[string]any{}
	for _, id := range ids {
		usage, tot := aiAggregate(bySb[id])
		var name any
		blocked := false
		if sb, err := s.SandboxByID(id); err == nil {
			name = sb.Name
			blocked, _ = sb.Extra["aiBlocked"].(bool)
		}
		sbs = append(sbs, map[string]any{"sandboxId": id, "name": name, "blocked": blocked, "usage": usage, "totals": tot.json(), "dollars": aiDollars(tot.Requests)})
	}
	usage, tot := aiAggregate(rows)
	return c.OK(200, "ai_usage.info", map[string]any{"accountId": account, "profileId": profile, "since": since, "sandboxes": sbs,
		"usage": usage, "totals": tot.json(), "dollars": aiDollars(tot.Requests)})
}

func (s *Server) handleAISandboxUsage(c *Ctx) error {
	sb, err := c.Sandbox("sandbox.read", false)
	if err != nil {
		return err
	}
	_, since, err := aiDays(c)
	if err != nil {
		return err
	}
	rows, err := s.aiUsageRows(`sandbox_id=? AND day>=?`, sb.ID, since)
	if err != nil {
		return err
	}
	usage, tot := aiAggregate(rows)
	blocked, _ := sb.Extra["aiBlocked"].(bool)
	var profile any
	if _, v := s.envOf(sb); v != nil && v.Config.CredentialProfileID != "" {
		profile = v.Config.CredentialProfileID
	}
	return c.OK(200, "sandbox.ai_usage", map[string]any{"sandboxId": sb.ID, "blocked": blocked, "proxied": envManagedOf(sb).Proxied,
		"credentialProfileId": profile, "since": since, "usage": usage, "totals": tot.json(), "dollars": aiDollars(tot.Requests)})
}

// ---------------------------------------------------------------------------
// Per-sandbox credentials (used by envPlanFor)
// ---------------------------------------------------------------------------

// aiSandboxProfile: the environment's profile, else the owner's default.
// An org profile counts only while the owner may use it (bugs-r10-opus 2).
func (s *Server) aiSandboxProfile(sb *Sandbox, cfg envConfig) (*aiProfile, error) {
	if cfg.CredentialProfileID != "" {
		if p, err := s.aiProfileByID(cfg.CredentialProfileID); err == nil && (!p.Org || s.orgCanUse(p.Access, p.OwnerID, sb.OwnerID)) {
			return p, nil
		}
	}
	return s.aiDefaultProfile(sb.OwnerID, false)
}

const aiTokenPrefix = "cvmai_"

func (s *Server) aiMintToken(sbID, profileID string) (string, error) {
	tok := aiTokenPrefix + randToken(40)
	sealed, err := s.envSeal([]byte(tok))
	if err != nil {
		return "", err
	}
	_, _ = s.DB.Exec(`DELETE FROM ai_proxy_tokens WHERE sandbox_id=?`, sbID)
	_, err = s.DB.Exec(`INSERT INTO ai_proxy_tokens(token_hash,sandbox_id,profile_id,token,created_at) VALUES(?,?,?,?,?)`, hashToken(tok), sbID, profileID, sealed, store.Now())
	return tok, err
}

func (s *Server) aiCurrentToken(sbID string) string {
	var sealed string
	if s.DB.QueryRow(`SELECT token FROM ai_proxy_tokens WHERE sandbox_id=? ORDER BY created_at DESC LIMIT 1`, sbID).Scan(&sealed) != nil {
		return ""
	}
	b, err := s.envOpen(sealed)
	if err != nil {
		return ""
	}
	return string(b)
}

// aiPlan adds the AI credentials of the sandbox's profile to p: proxy
// placeholders when the proxy applies, otherwise the real keys.
func (s *Server) aiPlan(sb *Sandbox, cfg envConfig, personal, forceProxy, boot bool, p *envPlan) error {
	prof, err := s.aiSandboxProfile(sb, cfg)
	if err != nil {
		return err
	}
	if !prof.Org && !personal {
		return nil // a shared org sandbox never gets personal credentials
	}
	p.ProfileID = prof.ID
	proxied := forceProxy || s.aiProxyEnabled(prof.OwnerID) || (sb.OrgID != "" && s.aiProxyEnabled(sb.OrgID))
	if !boot {
		// Turning the proxy off applies at the next start, but a live apply
		// never hands real keys to a sandbox that must be proxied now (safe
		// for third parties, proxy on): without a boot token it then gets
		// none (bugs-r10-opus 3).
		proxied = proxied || envManagedOf(sb).Proxied
	}
	p.Proxied = proxied
	active := s.aiActive(prof.ID)
	if boot { // renew expiring sign-ins before every start (AGT-031)
		var renewed bool
		if active, renewed = s.aiActiveFresh(prof.ID); renewed {
			s.envPushProfile(prof.ID)
		}
	}
	customs := s.aiCustomAPIs(prof.ID)
	if proxied {
		var tok string
		if boot {
			if tok, err = s.aiMintToken(sb.ID, prof.ID); err != nil {
				return err
			}
		} else if tok = s.aiCurrentToken(sb.ID); tok == "" {
			return nil
		}
		// Live applies repoint it too, so a key added later is proxied.
		p.AIProxy = &proto.AIProxy{BaseURL: s.envGuestURL() + "/ai-proxy", Token: tok, Vendors: aiProxiedVendors(active), VendorsSet: true}
		base := s.envGuestURL() + "/ai-proxy"
		// Vendors the guest's ProxyEnv does not cover.
		if _, ok := active["llmgateway-api-key"]; ok {
			p.Env["LLMGATEWAY_API_KEY"], p.Env["LLMGATEWAY_BASE_URL"] = tok, base+"/llmgateway/v1"
		}
		if _, ok := active["deepseek-api-key"]; ok {
			p.Env["DEEPSEEK_API_KEY"], p.Env["DEEPSEEK_BASE_URL"] = tok, base+"/deepseek"
			aiDeepSeekHarnesses(p, active, tok, base+"/deepseek")
		}
		for _, a := range customs {
			p.Env[a.Name], p.Env[a.Name+"_BASE_URL"] = tok, base+"/custom/"+a.Name
		}
		return nil // Bedrock and the Kimi sign-in are withheld behind the proxy
	}
	// Real keys: API keys first, so sign-ins (preferred) overwrite them.
	for _, cr := range aiCreds {
		sec, ok := active[cr.ID]
		if !ok {
			continue
		}
		sec = aiSandboxSecret(cr.ID, sec) // no refresh tokens the server renews
		switch cr.ID {
		case "chatgpt-subscription":
			p.Files = append(p.Files, proto.File{Path: ".codex/auth.json", Content: aiCodexAuth(sec), Mode: 0o600})
			if id := aiChatGPTAccount(sec); id != "" {
				p.Env["CHATGPT_ACCOUNT_ID"] = id
			}
		case "kimi-subscription":
			p.Env["KIMI_CODE_ACCESS_TOKEN"] = sec.Value
			if sec.RefreshToken != "" {
				p.Env["KIMI_CODE_REFRESH_TOKEN"] = sec.RefreshToken
			}
			if sec.Region != "" {
				p.Env["KIMI_CODE_REGION"] = sec.Region
			}
		case "claude-bedrock":
			aiBedrockEnv(p.Env, sec)
			p.Env["CLAUDE_CODE_USE_BEDROCK"] = "1"
			_, ak := active["anthropic-api-key"]
			_, cs := active["claude-subscription"]
			if !ak && !cs {
				// The plain sonnet/opus aliases point at models many AWS
				// accounts have not enabled: default to Sonnet 4.6.
				p.Env["ANTHROPIC_MODEL"] = aiBedrockClaudeDefault
			}
		case "codex-bedrock":
			aiBedrockEnv(p.Env, sec)
			_, oai := active["openai-api-key"]
			_, gpt := active["chatgpt-subscription"]
			if !oai && !gpt {
				p.Env["OPENAI_BASE_URL"] = "https://bedrock-mantle." + sec.Region + ".api.aws/v1"
			}
		default:
			p.Env[cr.EnvVar] = sec.Value
		}
	}
	if sec, ok := active["deepseek-api-key"]; ok {
		aiDeepSeekHarnesses(p, active, sec.Value, "https://api.deepseek.com")
	}
	for _, a := range customs {
		p.Env[a.Name], p.Env[a.Name+"_BASE_URL"] = a.Value, a.BaseURL
	}
	return nil
}

// aiBedrockEnv sets the region and the Bedrock API key, or the IAM access keys.
func aiBedrockEnv(env map[string]string, sec aiSecret) {
	env["AWS_REGION"] = sec.Region
	if sec.AccessKeyID == "" {
		env["AWS_BEARER_TOKEN_BEDROCK"] = sec.Value
		return
	}
	env["AWS_ACCESS_KEY_ID"], env["AWS_SECRET_ACCESS_KEY"] = sec.AccessKeyID, sec.Value
	if sec.SessionToken != "" {
		env["AWS_SESSION_TOKEN"] = sec.SessionToken
	}
}

// aiBedrockClaudeDefault is Claude Code's default model on Bedrock.
const aiBedrockClaudeDefault = "us.anthropic.claude-sonnet-4-6"

// aiDeepSeekHarnesses points Claude Code and Codex at DeepSeek when the
// profile has a DeepSeek key and no credential of the harness's own vendor
// (AGT-034). base is https://api.deepseek.com or its proxy route. Codex
// reads DEEPSEEK_API_KEY/DEEPSEEK_BASE_URL through the model_providers.deepseek
// block the guest writes when OPENAI_BASE_URL points at DeepSeek.
func aiDeepSeekHarnesses(p *envPlan, active map[string]aiSecret, key, base string) {
	has := func(ids ...string) bool {
		for _, id := range ids {
			if _, ok := active[id]; ok {
				return true
			}
		}
		return false
	}
	if !has("anthropic-api-key", "claude-subscription", "claude-bedrock") {
		p.Env["ANTHROPIC_BASE_URL"], p.Env["ANTHROPIC_AUTH_TOKEN"] = base+"/anthropic", key
		p.Env["ANTHROPIC_API_KEY"] = "" // Claude Code would send x-api-key, which DeepSeek rejects
	}
	if !has("openai-api-key", "chatgpt-subscription", "codex-bedrock") {
		p.Env["OPENAI_BASE_URL"], p.Env["DEEPSEEK_BASE_URL"], p.Env["DEEPSEEK_API_KEY"] = base+"/v1", base, key
	}
}

// aiCodexAuth renders ~/.codex/auth.json from a stored ChatGPT sign-in.
func aiCodexAuth(sec aiSecret) []byte {
	if strings.HasPrefix(sec.Value, "{") {
		return []byte(sec.Value)
	}
	b, _ := json.MarshalIndent(map[string]any{"OPENAI_API_KEY": nil, "tokens": map[string]any{
		"access_token": sec.Value, "refresh_token": nz(sec.RefreshToken), "account_id": nz(sec.AccountID)}}, "", "  ")
	return b
}

// aiChatGPTToken returns the access token and account id of a sign-in.
func aiChatGPTToken(sec aiSecret) (string, string) {
	if !strings.HasPrefix(sec.Value, "{") {
		return sec.Value, sec.AccountID
	}
	var a struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	_ = json.Unmarshal([]byte(sec.Value), &a)
	return a.Tokens.AccessToken, firstNonEmpty(sec.AccountID, a.Tokens.AccountID)
}

func aiChatGPTAccount(sec aiSecret) string {
	_, id := aiChatGPTToken(sec)
	return id
}

// aiOnEvent: placeholders die when a sandbox stops or is deleted.
func (s *Server) aiOnEvent(e Event) {
	if (e.Type == EvSandboxState && (e.State == "archiving" || e.State == "archived")) || e.Type == EvSandboxDeleted {
		_, _ = s.DB.Exec(`DELETE FROM ai_proxy_tokens WHERE sandbox_id=?`, e.SandboxID)
	}
}

// aiDecorateProviders adds live fields to GET /provider-models: whether the
// caller's default profile can run each harness, with which credentials.
func (s *Server) aiDecorateProviders(p *Principal, providers any) any {
	var list []map[string]any
	b, _ := json.Marshal(providers)
	if json.Unmarshal(b, &list) != nil || p == nil || p.User == nil {
		return providers
	}
	prof, err := s.aiDefaultProfile(p.User.ID, false)
	if err != nil {
		return providers
	}
	active := s.aiActive(prof.ID)
	proxied := s.aiProxyEnabled(prof.OwnerID)
	for _, m := range list {
		name, _ := m["provider"].(string)
		creds := []string{}
		for _, c := range aiHarnessCreds[name] {
			if _, ok := active[c]; ok {
				if cr, _ := aiCredByID(c); proxied && cr.Vendor == "" {
					continue
				}
				creds = append(creds, c)
			}
		}
		m["credentials"] = creds
		m["configured"] = len(creds) > 0 || name == "opencode" || name == "fake"
		m["proxied"] = proxied
		m["credentialProfileId"] = prof.ID
	}
	return list
}
