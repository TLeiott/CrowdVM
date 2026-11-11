package server

// Environments (Boat "sandbox environments"): named, versioned bundles of
// env vars, secret files, repositories with setup scripts, registry logins
// and credential toggles. Every change mints an immutable version; sandboxes
// stay pinned to the version they started on until POST .../upgrade.
//
// Owners are users or organizations (owner_id + org flag). Organization
// environments are selected with ?org= only (or an org API key).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

func init() {
	store.Register("environments", store.Migration{Version: 1, SQL: `
CREATE TABLE environments (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  org INTEGER NOT NULL DEFAULT 0,
  name TEXT NOT NULL,
  is_default INTEGER NOT NULL DEFAULT 0,
  latest_version INTEGER NOT NULL DEFAULT 0,
  access TEXT NOT NULL DEFAULT '{}',
  safety_answered_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER
);
CREATE INDEX environments_owner ON environments(owner_id, deleted_at);
CREATE TABLE environment_versions (
  id TEXT PRIMARY KEY,
  env_id TEXT NOT NULL,
  number INTEGER NOT NULL,
  config TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE(env_id, number)
);
CREATE TABLE git_sources (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  name TEXT NOT NULL,
  base_url TEXT NOT NULL,
  api_url TEXT NOT NULL,
  token TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE git_repos (
  database_id TEXT PRIMARY KEY,
  source_id TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  repo_id INTEGER NOT NULL DEFAULT 0,
  full_name TEXT NOT NULL,
  name TEXT NOT NULL,
  description TEXT,
  url TEXT NOT NULL,
  clone_url TEXT NOT NULL,
  private INTEGER NOT NULL DEFAULT 0,
  permissions TEXT NOT NULL DEFAULT '',
  pushed_at INTEGER,
  account_login TEXT NOT NULL DEFAULT '',
  account_type TEXT NOT NULL DEFAULT '',
  account_avatar TEXT NOT NULL DEFAULT '',
  synced_at INTEGER NOT NULL
);
CREATE INDEX git_repos_owner ON git_repos(owner_id);
`})
	RegisterFeature("environments", func(s *Server) error {
		s.envRoutes()
		s.Hooks.ResolveEnvironment = s.envResolve
		s.Hooks.PrepareGuest = append(s.Hooks.PrepareGuest, s.envPrepareGuest)
		s.Hooks.Guard = append(s.Hooks.Guard, s.envGuard)
		return nil
	})
}

func (s *Server) envRoutes() {
	s.API("GET /environments", "environment.read", s.handleEnvList)
	s.API("POST /environments", "environment.write", s.handleEnvCreate)
	s.API("GET /environments/{environmentId}", "environment.read", s.handleEnvGet)
	s.API("PUT /environments/{environmentId}", "environment.write", s.handleEnvUpdate)
	s.API("DELETE /environments/{environmentId}", "environment.write", s.handleEnvDelete)
	s.API("POST /environments/{environmentId}/upgrade", "environment.write", s.handleEnvUpgrade)
	s.API("PUT /environments/{environmentId}/vars/{key}", "environment.write", s.handleEnvSetVar)
	s.API("DELETE /environments/{environmentId}/vars/{key}", "environment.write", s.handleEnvDeleteVar)
	s.API("PUT /environments/{environmentId}/secret-files", "environment.write", s.handleEnvSetFile)
	s.API("DELETE /environments/{environmentId}/secret-files", "environment.write", s.handleEnvDeleteFile)
	s.API("POST /environments/{environmentId}/repos", "environment.write", s.handleEnvAddRepo)
	s.API("DELETE /environments/{environmentId}/repos/{repositoryId}", "environment.write", s.handleEnvDeleteRepo)
	s.API("GET /secrets", "environment.read", s.handleSecretsGet)
	s.API("POST /secrets", "environment.write", s.handleSecretsUpdate)
	s.envRepoRoutes()
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

type envAccess struct {
	Restricted bool     `json:"restricted"`
	Members    []string `json:"members"`
}

type envRow struct {
	ID, OwnerID, Name string
	Org, IsDefault    bool
	Latest            int
	Access            envAccess
	SafetyAnsweredAt  *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

type envSecretFile struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

type envRepo struct {
	RepositoryID  string `json:"repositoryId"` // databaseId
	SourceID      string `json:"sourceId,omitempty"`
	URL           string `json:"url"`      // web URL
	CloneURL      string `json:"cloneUrl"` // git URL
	FullName      string `json:"fullName"`
	Name          string `json:"name"`
	RepoID        int64  `json:"id,omitempty"`
	Private       bool   `json:"private,omitempty"`
	Description   string `json:"description,omitempty"`
	Permissions   string `json:"permissions,omitempty"`
	BaseBranch    string `json:"baseBranch"`
	SetupScript   string `json:"setupScript"`
	SetupBlocking bool   `json:"setupBlocking"`
}

// envConfig is the immutable content of one version (sealed at rest).
type envConfig struct {
	SafeForThirdParties    bool                  `json:"safeForThirdParties"`
	PassGithub             bool                  `json:"passGithub"`
	PassSecrets            bool                  `json:"passSecrets"`
	PassSandboxCredentials bool                  `json:"passSandboxCredentials"`
	PassAgentsCredentials  bool                  `json:"passAgentsCredentials"`
	ProxyAICredentials     bool                  `json:"proxyAiCredentials"`
	CredentialProfileID    string                `json:"credentialProfileId,omitempty"`
	EnvContents            string                `json:"envContents"`
	SecretFiles            []envSecretFile       `json:"secretFiles"`
	Repos                  []envRepo             `json:"repos"`
	RegistryLogins         []proto.RegistryLogin `json:"registryLogins"`
}

func envDefaultConfig() envConfig {
	return envConfig{PassGithub: true, PassSecrets: true, PassSandboxCredentials: true, PassAgentsCredentials: true}
}

type envVersion struct {
	ID        string
	Number    int
	Config    envConfig
	CreatedAt time.Time
}

const envCols = `id,owner_id,org,name,is_default,latest_version,access,safety_answered_at,created_at,updated_at,deleted_at`

func scanEnv(sc interface{ Scan(...any) error }) (*envRow, error) {
	e := &envRow{}
	var access string
	var safety, deleted sql.NullInt64
	var created, updated int64
	if err := sc.Scan(&e.ID, &e.OwnerID, &e.Org, &e.Name, &e.IsDefault, &e.Latest, &access, &safety, &created, &updated, &deleted); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(access), &e.Access)
	if e.Access.Members == nil {
		e.Access.Members = []string{}
	}
	e.SafetyAnsweredAt, e.DeletedAt = store.NullTime(safety), store.NullTime(deleted)
	e.CreatedAt, e.UpdatedAt = store.Time(created), store.Time(updated)
	return e, nil
}

func (s *Server) envByID(id string) (*envRow, error) {
	return scanEnv(s.DB.QueryRow(`SELECT `+envCols+` FROM environments WHERE id=?`, id))
}

// envList returns the owner's live environments, default first.
func (s *Server) envList(ownerID string) ([]*envRow, error) {
	rows, err := s.DB.Query(`SELECT `+envCols+` FROM environments WHERE owner_id=? AND deleted_at IS NULL ORDER BY is_default DESC, created_at, name`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*envRow
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// envEnsureBase creates the default environment "base" (v1) when the owner
// has no environment yet.
func (s *Server) envEnsureBase(ownerID string, org bool) error {
	// Two first creates of an account at once (CI jobs) must not both mint
	// "base", nor read it before its first version exists.
	unlock := s.sbLocks.Lock("env-base:" + ownerID)
	defer unlock()
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM environments WHERE owner_id=? AND deleted_at IS NULL`, ownerID).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := s.envCreate(ownerID, org, "base", true)
	return err
}

func (s *Server) envCreate(ownerID string, org bool, name string, isDefault bool) (*envRow, error) {
	id, now := envUUID(), store.Now()
	if _, err := s.DB.Exec(`INSERT INTO environments(id,owner_id,org,name,is_default,latest_version,access,created_at,updated_at) VALUES(?,?,?,?,?,0,'{"restricted":false,"members":[]}',?,?)`,
		id, ownerID, boolInt(org), name, boolInt(isDefault), now, now); err != nil {
		return nil, err
	}
	e, _ := s.envByID(id)
	if _, err := s.envMint(e, envDefaultConfig()); err != nil {
		return nil, err
	}
	return s.envByID(id)
}

func (s *Server) envDefault(ownerID string, org bool) (*envRow, error) {
	if err := s.envEnsureBase(ownerID, org); err != nil {
		return nil, err
	}
	e, err := scanEnv(s.DB.QueryRow(`SELECT `+envCols+` FROM environments WHERE owner_id=? AND deleted_at IS NULL ORDER BY is_default DESC, created_at LIMIT 1`, ownerID))
	if err == nil && !e.IsDefault {
		_, _ = s.DB.Exec(`UPDATE environments SET is_default=1 WHERE id=?`, e.ID)
		e.IsDefault = true
	}
	return e, err
}

func (s *Server) envVersionOf(envID string, n int) (*envVersion, error) {
	var v envVersion
	var sealed string
	var created int64
	if err := s.DB.QueryRow(`SELECT id,number,config,created_at FROM environment_versions WHERE env_id=? AND number=?`, envID, n).Scan(&v.ID, &v.Number, &sealed, &created); err != nil {
		return nil, err
	}
	plain, err := s.envOpen(sealed)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(plain, &v.Config); err != nil {
		return nil, err
	}
	v.CreatedAt = store.Time(created)
	return &v, nil
}

func (s *Server) envLatest(e *envRow) (*envVersion, error) { return s.envVersionOf(e.ID, e.Latest) }

// envMint stores cfg as the next version of e.
func (s *Server) envMint(e *envRow, cfg envConfig) (*envVersion, error) {
	b, _ := json.Marshal(cfg)
	sealed, err := s.envSeal(b)
	if err != nil {
		return nil, err
	}
	v := &envVersion{ID: envUUID(), Config: cfg, CreatedAt: time.Now().UTC()}
	err = s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT latest_version+1 FROM environments WHERE id=?`, e.ID).Scan(&v.Number); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO environment_versions(id,env_id,number,config,created_at) VALUES(?,?,?,?,?)`, v.ID, e.ID, v.Number, sealed, store.T(v.CreatedAt)); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE environments SET latest_version=?, updated_at=? WHERE id=?`, v.Number, store.T(v.CreatedAt), e.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	e.Latest = v.Number
	return v, nil
}

// envHoldsLogins: the version passes its sandbox creator's personal logins
// (GitHub token, CrowdVM key, personal AI profile, personal secrets).
func (s *Server) envHoldsLogins(e *envRow, cfg envConfig) bool {
	if cfg.SafeForThirdParties {
		return false
	}
	if !e.Org {
		return cfg.PassGithub || cfg.PassSecrets || cfg.PassSandboxCredentials || cfg.PassAgentsCredentials
	}
	return cfg.PassGithub || cfg.PassSandboxCredentials || (cfg.PassAgentsCredentials && !s.aiProfileIsOrg(cfg.CredentialProfileID))
}

// ---------------------------------------------------------------------------
// Scope and access
// ---------------------------------------------------------------------------

type envScope struct {
	OwnerID string
	Org     *Org
	Role    string // caller's org role; "" = personal
	User    *User
}

func (sc envScope) canManage() bool { return sc.Org == nil || sc.Role == "owner" || sc.Role == "admin" }

// envScopeOf: personal unless ?org= names an organization (or the key is an
// org key). X-Boat-Org and the active wallet do not select org environments.
func (s *Server) envScopeOf(c *Ctx) (envScope, error) {
	u := c.P.User
	var org *Org
	var err error
	if k := c.P.Key; k != nil && k.OrgID != "" {
		org, err = s.OrgByID(k.OrgID)
	} else if ref := c.Query("org"); ref != "" {
		org, err = s.resolveOrgRef(u, ref)
	}
	if err != nil {
		return envScope{}, err
	}
	if org == nil {
		return envScope{OwnerID: u.ID, User: u}, nil
	}
	role := s.memberRole(org.ID, u.ID)
	if role == "" {
		return envScope{}, Err(403, "not_org_member", "You are not a member of this organization.")
	}
	return envScope{OwnerID: org.ID, Org: org, Role: role, User: u}, nil
}

// envCanUse applies an org access list.
func envCanUse(a envAccess, role, userID string) bool {
	return !a.Restricted || role == "owner" || role == "admin" || slices.Contains(a.Members, userID)
}

// orgCanUse: userID is a member of orgID and on the access list a.
func (s *Server) orgCanUse(a envAccess, orgID, userID string) bool {
	role := s.memberRole(orgID, userID)
	return role != "" && envCanUse(a, role, userID)
}

// envFind resolves an environment id or name in the scope.
func (s *Server) envFind(sc envScope, ref string, manage bool) (*envRow, error) {
	if err := s.envEnsureBase(sc.OwnerID, sc.Org != nil); err != nil {
		return nil, err
	}
	e, err := scanEnv(s.DB.QueryRow(`SELECT `+envCols+` FROM environments WHERE owner_id=? AND deleted_at IS NULL AND (id=? OR name=?) ORDER BY id=? DESC LIMIT 1`, sc.OwnerID, ref, ref, ref))
	if err != nil {
		return nil, Err(404, "environment_not_found", "Environment not found.")
	}
	if sc.Org != nil && !envCanUse(e.Access, sc.Role, sc.User.ID) {
		return nil, Err(404, "environment_not_found", "Environment not found.")
	}
	if manage && !sc.canManage() {
		return nil, Err(403, "forbidden", "Only organization owners and admins can change organization environments.")
	}
	return e, nil
}

// envResolveAccess turns member ids/emails into user ids of active members.
func (s *Server) envResolveAccess(org *Org, upd *envAccessUpdate) (envAccess, error) {
	if org == nil {
		return envAccess{}, Err(400, "invalid_access", "Only organization environments and profiles have an access list.")
	}
	out := envAccess{Restricted: upd.Restricted, Members: []string{}}
	if !upd.Restricted {
		return out, nil
	}
	for _, m := range upd.Members {
		var id string
		err := s.DB.QueryRow(`SELECT u.id FROM users u JOIN org_members om ON om.user_id=u.id WHERE om.org_id=? AND (u.id=? OR lower(u.email)=lower(?) OR u.username=?)`, org.ID, m, m, m).Scan(&id)
		if err != nil {
			return out, ErrD(400, "invalid_access", "Not an active member of the organization: "+m+".", map[string]any{"member": m})
		}
		if !slices.Contains(out.Members, id) {
			out.Members = append(out.Members, id)
		}
	}
	return out, nil
}

type envAccessUpdate struct {
	Restricted bool     `json:"restricted"`
	Members    []string `json:"members"`
}

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

func envEffective(cfg envConfig) map[string]any {
	on := !cfg.SafeForThirdParties
	return map[string]any{
		"passGithub": on && cfg.PassGithub, "passSecrets": on && cfg.PassSecrets,
		"passSandboxCredentials": on && cfg.PassSandboxCredentials,
		"passAgentsCredentials":  (on && cfg.PassAgentsCredentials) || (!on && cfg.ProxyAICredentials),
		"proxyAiCredentials":     !on && cfg.ProxyAICredentials,
	}
}

// envWarning explains values a version withholds ("" = none).
func envWarning(cfg envConfig) string {
	hasSecrets := strings.TrimSpace(cfg.EnvContents) != "" || len(cfg.SecretFiles) > 0 || len(cfg.RegistryLogins) > 0
	switch {
	case cfg.SafeForThirdParties && (hasSecrets || len(cfg.Repos) > 0):
		return "This environment is safe for third parties: its variables, secret files and repositories reach no sandbox."
	case !cfg.PassSecrets && hasSecrets:
		return "The Secrets toggle is off: variables and secret files reach no sandbox."
	case !cfg.PassGithub && len(cfg.Repos) > 0:
		return "The GitHub repositories toggle is off: repositories are not cloned and setup scripts do not run."
	}
	return ""
}

func envRepoJSON(r envRepo) map[string]any {
	return map[string]any{
		"databaseId": r.RepositoryID, "id": r.RepoID, "fullName": r.FullName, "name": r.Name, "url": firstNonEmpty(r.URL, r.CloneURL),
		"cloneUrl": r.CloneURL, "private": r.Private, "description": nz(r.Description), "permissions": r.Permissions,
		"pushedAt": nil, "lastUserCommitAt": nil, "baseBranch": firstNonEmpty(r.BaseBranch, "main"),
		"setupScript": r.SetupScript, "setupBlocking": r.SetupBlocking, "setupRoutineId": nil,
	}
}

func envReposJSON(rs []envRepo) []map[string]any {
	out := []map[string]any{}
	for _, r := range rs {
		out = append(out, envRepoJSON(r))
	}
	return out
}

func (s *Server) envJSON(e *envRow, viewer *User) (map[string]any, error) {
	v, err := s.envLatest(e)
	if err != nil {
		return nil, err
	}
	cfg := v.Config
	counts := map[int]int{}
	rows, err := s.DB.Query(`SELECT environment_version, COUNT(*) FROM sandboxes WHERE environment_id=? AND owner_id=? AND deleted_at IS NULL AND environment_version IS NOT NULL GROUP BY environment_version`, e.ID, viewer.ID)
	if err == nil {
		for rows.Next() {
			var n, c int
			if rows.Scan(&n, &c) == nil {
				counts[n] = c
			}
		}
		rows.Close()
	}
	var versions []map[string]any
	vr, err := s.DB.Query(`SELECT id,number,created_at FROM environment_versions WHERE env_id=? ORDER BY number`, e.ID)
	if err != nil {
		return nil, err
	}
	for vr.Next() {
		var id string
		var n int
		var created int64
		if vr.Scan(&id, &n, &created) == nil {
			versions = append(versions, map[string]any{"id": id, "versionNumber": n, "sandboxCount": counts[n], "createdAt": tsMs(created)})
		}
	}
	vr.Close()
	files := []map[string]any{}
	for _, f := range cfg.SecretFiles {
		files = append(files, map[string]any{"path": f.Path, "contents": f.Contents})
	}
	regs := []map[string]any{}
	for _, r := range cfg.RegistryLogins {
		regs = append(regs, map[string]any{"registry": r.Registry, "username": r.Username, "password": r.Password})
	}
	var safety, profile any
	if e.SafetyAnsweredAt != nil {
		safety = ts(*e.SafetyAnsweredAt)
	}
	if cfg.CredentialProfileID != "" {
		profile = cfg.CredentialProfileID
	}
	owner := map[string]any{"id": e.OwnerID, "kind": "personal"}
	if e.Org {
		owner["kind"] = "org"
	}
	m := map[string]any{
		"id": e.ID, "name": e.Name, "isDefault": e.IsDefault, "latestVersionId": v.ID, "latestVersionNumber": v.Number,
		"safeForThirdParties": cfg.SafeForThirdParties, "passGithub": cfg.PassGithub, "passSecrets": cfg.PassSecrets,
		"passSandboxCredentials": cfg.PassSandboxCredentials, "passAgentsCredentials": cfg.PassAgentsCredentials,
		"proxyAiCredentials": cfg.ProxyAICredentials, "credentialProfileId": profile, "envContents": cfg.EnvContents,
		"secretFiles": files, "selectedRepositories": envReposJSON(cfg.Repos), "versions": versions,
		"access": e.Access, "safetyAnsweredAt": safety, "effective": envEffective(cfg),
		"registryLogins": regs, "owner": owner, "createdAt": ts(e.CreatedAt), "updatedAt": ts(e.UpdatedAt),
	}
	if w := envWarning(cfg); w != "" {
		m["warning"] = w
	}
	return m, nil
}

func (s *Server) envListJSON(sc envScope) ([]map[string]any, error) {
	if err := s.envEnsureBase(sc.OwnerID, sc.Org != nil); err != nil {
		return nil, err
	}
	list, err := s.envList(sc.OwnerID)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, e := range list {
		if sc.Org != nil && !envCanUse(e.Access, sc.Role, sc.User.ID) {
			continue
		}
		m, err := s.envJSON(e, sc.User)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

var envNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

func envValidateContents(c string) error {
	if len(c) > 256<<10 {
		return Err(400, "invalid_env", "envContents can be at most 256KB.")
	}
	vars := envVars(c)
	if len(vars) > 1000 {
		return Err(400, "invalid_env", "At most 1000 environment variables.")
	}
	for k := range vars {
		if slices.Contains(reservedEnv, k) || strings.HasPrefix(k, "CROWDVM_") {
			return Err(400, "invalid_env", k+" is reserved.")
		}
	}
	return nil
}

// homeDirs: folders under /home/user that the image, the desktop or the
// guest agent own. A secret file may go inside them, never replace one
// (bugs-r4-opus 8, bugs-r5-opus 8).
var homeDirs = []string{
	".ssh", ".cache", ".config", ".local", ".cargo", ".rustup", ".nvm", ".npm", ".bun", ".sdkman", ".prime",
	".claude", ".codex", ".agents", ".pi", ".cursor", ".kimi-code", ".vibe", ".hermes", ".openclaw",
	".gem", ".gradle", ".m2", ".dotnet", "go", "Desktop", "Documents", "Downloads", "Music", "Pictures",
	"Public", "Templates", "Videos",
	// guest agent, desktop session and tools
	".ascii", ".boat", ".crowdvm", ".dbus", ".gnupg", ".docker", ".vscode", ".mozilla", ".pki",
	".local/bin", ".local/share", ".local/state", ".config/xfce4", ".config/autostart", ".config/gh",
	".config/boat", ".config/crowdvm", ".config/ascii", ".config/dconf", ".config/pulse", ".config/systemd",
	// deeper ones (bugs-r6-opus 4); a file inside them is fine
	".cargo/bin", ".rustup/toolchains", ".claude/skills", ".codex/skills", ".agents/skills", ".config/opencode",
	".config/Thunar", ".cua-driver", ".local/share/applications",
}

// desktopTrees: the desktop session owns everything below these; a secret
// file there breaks it (bugs-r6-opus 4).
var desktopTrees = []string{".config/xfce4/", ".dbus/"}

func envValidateFiles(fs []envSecretFile) error {
	if len(fs) > 100 {
		return Err(400, "invalid_secret_files", "At most 100 secret files.")
	}
	total := 0
	seen := map[string]bool{}
	for i, f := range fs {
		if strings.TrimSpace(f.Path) == "" {
			return Err(400, "invalid_secret_files", "Each secret file needs a path.")
		}
		if strings.ContainsFunc(f.Path, unicode.IsControl) {
			return Err(400, "invalid_secret_files", "Secret file paths cannot contain control characters.")
		}
		rel := envHomeRel(f.Path)
		if rel == "" { // never written: refuse rather than skip it at boot (bugs-r2-opus 3)
			return Err(400, "invalid_secret_files", "Secret file paths are relative to /home/user (for example backend/.env): "+f.Path+" is not.")
		}
		// Guest writes use mkdir for parents and an atomic rename for the file.
		// These home directories belong to the image or desktop; children are OK.
		if slices.Contains(homeDirs, rel) || slices.ContainsFunc(desktopTrees, func(t string) bool { return strings.HasPrefix(rel, t) }) {
			return Err(400, "invalid_secret_files", "Secret file path names a home directory: "+rel+".")
		}
		fs[i].Path = rel
		if seen[rel] {
			return Err(400, "invalid_secret_files", "Duplicate secret file path "+rel+".")
		}
		for other := range seen {
			if strings.HasPrefix(rel, other+"/") || strings.HasPrefix(other, rel+"/") {
				return Err(400, "invalid_secret_files", "Secret file paths conflict: "+rel+" and "+other+".")
			}
		}
		seen[rel] = true
		total += len(f.Contents)
	}
	if total > 1<<20 {
		return Err(400, "invalid_secret_files", "Secret files can be at most 1MB in total.")
	}
	return nil
}

// envHomeRel maps a secret file path to a path relative to /home/user
// (Boat: "relative to /home/user"; /home/user/x and ~/x mean x), or "" when
// it is elsewhere or escapes the home.
func envHomeRel(p string) string {
	if strings.ContainsFunc(p, unicode.IsControl) {
		return ""
	}
	p = strings.TrimSpace(p)
	for _, pre := range []string{"/home/user/", "~/"} {
		if strings.HasPrefix(p, pre) {
			p = strings.TrimPrefix(p, pre)
			break
		}
	}
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return ""
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return ""
	}
	return c
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) handleEnvList(c *Ctx) error {
	sc, err := s.envScopeOf(c)
	if err != nil {
		return err
	}
	list, err := s.envListJSON(sc)
	if err != nil {
		return err
	}
	return c.OK(200, "environments.list", map[string]any{"environments": list})
}

func (s *Server) handleEnvGet(c *Ctx) error {
	sc, err := s.envScopeOf(c)
	if err != nil {
		return err
	}
	e, err := s.envFind(sc, c.Param("environmentId"), false)
	if err != nil {
		return err
	}
	m, err := s.envJSON(e, sc.User)
	if err != nil {
		return err
	}
	return c.OK(200, "environment.info", map[string]any{"success": true, "environment": m})
}

const maxEnvironments = 100

func (s *Server) handleEnvCreate(c *Ctx) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	sc, err := s.envScopeOf(c)
	if err != nil {
		return err
	}
	if !sc.canManage() {
		return Err(403, "forbidden", "Only organization owners and admins can create organization environments.")
	}
	if !envNameRe.MatchString(req.Name) {
		return Err(400, "invalid_name", "name must be 1-64 letters, digits, '.', '_' or '-'.")
	}
	if err := s.envEnsureBase(sc.OwnerID, sc.Org != nil); err != nil {
		return err
	}
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM environments WHERE owner_id=? AND name=? AND deleted_at IS NULL`, sc.OwnerID, req.Name).Scan(&n)
	if n > 0 {
		return Err(409, "environment_exists", "An environment named "+req.Name+" already exists.")
	}
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM environments WHERE owner_id=? AND deleted_at IS NULL`, sc.OwnerID).Scan(&n)
	if n >= maxEnvironments {
		return Err(409, "environment_limit", fmt.Sprintf("An account can have up to %d environments. Delete one first.", maxEnvironments))
	}
	e, err := s.envCreate(sc.OwnerID, sc.Org != nil, req.Name, false)
	if err != nil {
		return err
	}
	list, err := s.envListJSON(sc)
	if err != nil {
		return err
	}
	out := map[string]any{"environments": list}
	if fresh, err := s.envByID(e.ID); err == nil {
		if m, err := s.envJSON(fresh, sc.User); err == nil {
			out["environment"] = m // CrowdVM extension: the one just created (Boat returns only the list)
		}
	}
	return c.OK(200, "environments.list", out)
}

type envRepoReq struct {
	RepositoryID  string `json:"repositoryId"`
	BaseBranch    string `json:"baseBranch"`
	SetupScript   string `json:"setupScript"`
	SetupBlocking bool   `json:"setupBlocking"`
}

func (s *Server) handleEnvUpdate(c *Ctx) error {
	var req struct {
		Name                   *string                `json:"name"`
		IsDefault              *bool                  `json:"isDefault"`
		SafeForThirdParties    *bool                  `json:"safeForThirdParties"`
		PassGithub             *bool                  `json:"passGithub"`
		PassSecrets            *bool                  `json:"passSecrets"`
		PassSandboxCredentials *bool                  `json:"passSandboxCredentials"`
		PassCrowdVMCredentials *bool                  `json:"passCrowdvmCredentials"`
		PassAgentsCredentials  *bool                  `json:"passAgentsCredentials"`
		ProxyAICredentials     *bool                  `json:"proxyAiCredentials"`
		CredentialProfileID    json.RawMessage        `json:"credentialProfileId"`
		Access                 *envAccessUpdate       `json:"access"`
		EnvContents            *string                `json:"envContents"`
		SecretFiles            *[]envSecretFile       `json:"secretFiles"`
		Repositories           *[]envRepoReq          `json:"repositories"`
		RegistryLogins         *[]proto.RegistryLogin `json:"registryLogins"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	sc, err := s.envScopeOf(c)
	if err != nil {
		return err
	}
	e, err := s.envFind(sc, c.Param("environmentId"), true)
	if err != nil {
		return err
	}
	if req.Access != nil && sc.Org != nil && sc.Role != "owner" && sc.Role != "admin" {
		return Err(403, "forbidden", "Only owners and admins choose who can use an environment.")
	}
	cur, err := s.envLatest(e)
	if err != nil {
		return err
	}
	cfg := cur.Config
	set := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	set(&cfg.SafeForThirdParties, req.SafeForThirdParties)
	set(&cfg.PassGithub, req.PassGithub)
	set(&cfg.PassSecrets, req.PassSecrets)
	set(&cfg.PassSandboxCredentials, req.PassSandboxCredentials)
	set(&cfg.PassSandboxCredentials, req.PassCrowdVMCredentials)
	set(&cfg.PassAgentsCredentials, req.PassAgentsCredentials)
	set(&cfg.ProxyAICredentials, req.ProxyAICredentials)
	if len(req.CredentialProfileID) > 0 {
		var ref *string
		if err := json.Unmarshal(req.CredentialProfileID, &ref); err != nil {
			return Err(400, "invalid_json", "credentialProfileId must be a string or null.")
		}
		cfg.CredentialProfileID = ""
		if ref != nil && *ref != "" && *ref != "default" {
			id, err := s.aiProfileForEnv(sc, *ref)
			if err != nil {
				return err
			}
			cfg.CredentialProfileID = id
		}
	}
	if req.EnvContents != nil {
		if err := envValidateContents(*req.EnvContents); err != nil {
			return err
		}
		cfg.EnvContents = *req.EnvContents
	}
	if req.SecretFiles != nil {
		if err := envValidateFiles(*req.SecretFiles); err != nil {
			return err
		}
		cfg.SecretFiles = *req.SecretFiles
	}
	if req.RegistryLogins != nil {
		for _, r := range *req.RegistryLogins {
			if r.Registry == "" || r.Username == "" {
				return Err(400, "invalid_registry_login", "Each registry login needs registry, username and password.")
			}
		}
		cfg.RegistryLogins = *req.RegistryLogins
	}
	if req.Repositories != nil {
		var repos []envRepo
		for _, rr := range *req.Repositories {
			r, err := s.envResolveRepo(sc, rr.RepositoryID, cfg.Repos)
			if err != nil {
				return err
			}
			r.BaseBranch = firstNonEmpty(rr.BaseBranch, r.BaseBranch, "main")
			r.SetupScript, r.SetupBlocking = rr.SetupScript, rr.SetupBlocking
			if len(r.SetupScript) > 65536 {
				return Err(400, "invalid_setup_script", "A setup script can be at most 64KB.")
			}
			repos = slices.DeleteFunc(repos, func(x envRepo) bool { return x.RepositoryID == r.RepositoryID })
			repos = append(repos, r)
		}
		cfg.Repos = repos
	}
	if req.ProxyAICredentials != nil && *req.ProxyAICredentials && !s.aiProxyOnForProfile(sc, cfg.CredentialProfileID) {
		return Err(400, "ai_proxy_off", "proxyAiCredentials needs the AI credentials proxy on for the profile owner. Turn it on with PUT /ai-proxy.")
	}
	// Non-versioned fields.
	if req.Name != nil && *req.Name != e.Name {
		if !envNameRe.MatchString(*req.Name) {
			return Err(400, "invalid_name", "name must be 1-64 letters, digits, '.', '_' or '-'.")
		}
		var n int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM environments WHERE owner_id=? AND name=? AND deleted_at IS NULL AND id<>?`, sc.OwnerID, *req.Name, e.ID).Scan(&n)
		if n > 0 {
			return Err(409, "environment_exists", "An environment named "+*req.Name+" already exists.")
		}
		_, _ = s.DB.Exec(`UPDATE environments SET name=?, updated_at=? WHERE id=?`, *req.Name, store.Now(), e.ID)
		// Sandboxes pinned to it show the new name too (info, ls).
		_, _ = s.DB.Exec(`UPDATE sandboxes SET environment=? WHERE environment_id=?`, *req.Name, e.ID)
	}
	if req.Access != nil {
		acc, err := s.envResolveAccess(sc.Org, req.Access)
		if err != nil {
			return err
		}
		_, _ = s.DB.Exec(`UPDATE environments SET access=?, updated_at=? WHERE id=?`, mustJSON(acc), store.Now(), e.ID)
	}
	if req.IsDefault != nil && *req.IsDefault && !e.IsDefault {
		if err := s.DB.Tx(s.ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE environments SET is_default=0 WHERE owner_id=?`, sc.OwnerID); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE environments SET is_default=1 WHERE id=?`, e.ID)
			return err
		}); err != nil {
			return err
		}
	}
	if req.SafeForThirdParties != nil {
		_, _ = s.DB.Exec(`UPDATE environments SET safety_answered_at=? WHERE id=?`, store.Now(), e.ID)
	}
	if mustJSON(cfg) != mustJSON(cur.Config) {
		if _, err := s.envMint(e, cfg); err != nil {
			return err
		}
	}
	e, _ = s.envByID(e.ID)
	m, err := s.envJSON(e, sc.User)
	if err != nil {
		return err
	}
	return c.OK(200, "environment.updated", map[string]any{"success": true, "environment": m})
}

func (s *Server) handleEnvDelete(c *Ctx) error {
	sc, err := s.envScopeOf(c)
	if err != nil {
		return err
	}
	e, err := s.envFind(sc, c.Param("environmentId"), true)
	if err != nil {
		return err
	}
	if e.IsDefault {
		return Err(409, "is_default", "The default environment cannot be deleted. Make another environment the default first.")
	}
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM environments WHERE owner_id=? AND deleted_at IS NULL`, sc.OwnerID).Scan(&n)
	if n <= 1 {
		return Err(409, "last_environment", "At least one environment is always kept.")
	}
	if _, err := s.DB.Exec(`UPDATE environments SET deleted_at=?, updated_at=? WHERE id=?`, store.Now(), store.Now(), e.ID); err != nil {
		return err
	}
	return c.OK(200, "environment.deleted", map[string]any{"success": true, "environment": nil})
}

// envChange applies fn to the latest config, mints a version and answers
// with EnvironmentItemChangeResponse.
func (s *Server) envChange(c *Ctx, fn func(sc envScope, cfg *envConfig) error) error {
	sc, err := s.envScopeOf(c)
	if err != nil {
		return err
	}
	e, err := s.envFind(sc, c.Param("environmentId"), true)
	if err != nil {
		return err
	}
	cur, err := s.envLatest(e)
	if err != nil {
		return err
	}
	cfg := cur.Config
	cfg.SecretFiles = slices.Clone(cfg.SecretFiles)
	cfg.Repos = slices.Clone(cfg.Repos)
	if err := fn(sc, &cfg); err != nil {
		return err
	}
	v, err := s.envMint(e, cfg)
	if err != nil {
		return err
	}
	out := map[string]any{"success": true, "versionId": v.ID, "versionNumber": v.Number, "environmentId": e.ID}
	if w := envWarning(cfg); w != "" {
		out["warning"] = w
	}
	return c.OK(200, "environment.updated", out)
}

func (s *Server) handleEnvSetVar(c *Ctx) error {
	var req struct {
		Value *string `json:"value"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	key := c.Param("key")
	if !envKeyRe.MatchString(key) {
		return Err(400, "invalid_env", "Invalid environment variable name "+key+".")
	}
	if req.Value == nil {
		return Err(400, "missing_value", "value is required.")
	}
	return s.envChange(c, func(_ envScope, cfg *envConfig) error {
		next := envSetVar(cfg.EnvContents, key, req.Value)
		if err := envValidateContents(next); err != nil {
			return err
		}
		cfg.EnvContents = next
		return nil
	})
}

func (s *Server) handleEnvDeleteVar(c *Ctx) error {
	key := c.Param("key")
	return s.envChange(c, func(_ envScope, cfg *envConfig) error {
		if _, ok := envVars(cfg.EnvContents)[key]; !ok {
			return Err(404, "variable_not_found", "The environment has no variable "+key+".")
		}
		cfg.EnvContents = envSetVar(cfg.EnvContents, key, nil)
		return nil
	})
}

func (s *Server) handleEnvSetFile(c *Ctx) error {
	var req struct {
		Path     string  `json:"path"`
		Contents *string `json:"contents"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Path) == "" || req.Contents == nil {
		return Err(400, "invalid_secret_files", "path and contents are required.")
	}
	rel := envHomeRel(req.Path)
	if rel == "" {
		return Err(400, "invalid_secret_files", "Secret file paths are relative to /home/user (for example backend/.env): "+req.Path+" is not.")
	}
	return s.envChange(c, func(_ envScope, cfg *envConfig) error {
		// Same file under another spelling, or one stored before paths were
		// checked that no sandbox ever got: replaced / dropped.
		cfg.SecretFiles = slices.DeleteFunc(cfg.SecretFiles, func(f envSecretFile) bool { r := envHomeRel(f.Path); return r == rel || r == "" })
		cfg.SecretFiles = append(cfg.SecretFiles, envSecretFile{Path: rel, Contents: *req.Contents})
		return envValidateFiles(cfg.SecretFiles)
	})
}

func (s *Server) handleEnvDeleteFile(c *Ctx) error {
	p := c.Query("path")
	return s.envChange(c, func(_ envScope, cfg *envConfig) error {
		n := len(cfg.SecretFiles)
		cfg.SecretFiles = slices.DeleteFunc(cfg.SecretFiles, func(f envSecretFile) bool {
			return f.Path == p || (envHomeRel(p) != "" && envHomeRel(f.Path) == envHomeRel(p))
		})
		if len(cfg.SecretFiles) == n {
			return Err(404, "secret_file_not_found", "The environment has no secret file "+p+".")
		}
		return nil
	})
}

func (s *Server) handleEnvAddRepo(c *Ctx) error {
	var req envRepoReq
	if err := c.Bind(&req); err != nil {
		return err
	}
	if req.RepositoryID == "" {
		return Err(400, "invalid_repository", "repositoryId is required.")
	}
	return s.envChange(c, func(sc envScope, cfg *envConfig) error {
		r, err := s.envResolveRepo(sc, req.RepositoryID, cfg.Repos)
		if err != nil {
			return err
		}
		r.BaseBranch = firstNonEmpty(req.BaseBranch, "main")
		for i := range cfg.Repos {
			if cfg.Repos[i].RepositoryID == r.RepositoryID {
				cfg.Repos[i].BaseBranch = r.BaseBranch
				return nil
			}
		}
		cfg.Repos = append(cfg.Repos, r)
		return nil
	})
}

func (s *Server) handleEnvDeleteRepo(c *Ctx) error {
	id := c.Param("repositoryId")
	return s.envChange(c, func(_ envScope, cfg *envConfig) error {
		n := len(cfg.Repos)
		cfg.Repos = slices.DeleteFunc(cfg.Repos, func(r envRepo) bool {
			return r.RepositoryID == id || r.FullName == id || r.CloneURL == id || r.URL == id
		})
		if len(cfg.Repos) == n {
			return Err(404, "repository_not_found", "The environment has no repository "+id+".")
		}
		return nil
	})
}

func (s *Server) handleEnvUpgrade(c *Ctx) error {
	var req struct {
		AgentIDs   []string `json:"agentIds"`
		SandboxIDs []string `json:"sandboxIds"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	sc, err := s.envScopeOf(c)
	if err != nil {
		return err
	}
	e, err := s.envFind(sc, c.Param("environmentId"), false)
	if err != nil {
		return err
	}
	res := s.envUpgrade(c.R.Context(), c.P, e, append(req.AgentIDs, req.SandboxIDs...))
	v, _ := s.envLatest(e)
	res["success"] = true
	if v != nil {
		res["latestVersionId"] = v.ID
	}
	return c.OK(200, "environment.upgraded", res)
}

// envUpgrade repoints the caller's sandboxes on older versions of e to its
// latest version and hot-applies it to the running ones.
func (s *Server) envUpgrade(ctx context.Context, p *Principal, e *envRow, only []string) map[string]any {
	res := map[string]any{"upgraded": 0, "failed": 0, "upgradedLive": 0, "upgradedStopped": 0}
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE environment_id=? AND owner_id=? AND deleted_at IS NULL AND COALESCE(environment_version,0)<?`, e.ID, p.User.ID, e.Latest)
	if err != nil {
		return res
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && (len(only) == 0 || slices.Contains(only, id)) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	upgraded, failed, live, stopped := 0, 0, 0, 0
	latest, err := s.envLatest(e)
	if err != nil {
		return res
	}
	passesLogins := s.envHoldsLogins(e, latest.Config)
	for _, id := range ids {
		sb, err := s.SandboxByID(id)
		if err != nil {
			continue
		}
		if p.Key != nil && !p.Key.AllowsSandbox(sb, "sandbox.update") {
			continue
		}
		if sb.OrgID != "" && !sb.HoldsCreatorLogins && passesLogins && !sb.NoEnv {
			continue // a shared sandbox never receives personal logins later
		}
		unlock := s.sbLocks.Lock(id)
		_, err = s.DB.Exec(`UPDATE sandboxes SET environment_version=?, updated_at=? WHERE id=?`, e.Latest, store.Now(), id)
		sb, _ = s.SandboxByID(id)
		unlock()
		if err != nil || sb == nil {
			failed++
			continue
		}
		if sb.Usable() {
			if err := s.envApplyLive(ctx, sb, false); err != nil {
				s.Log.Warn("environment upgrade failed", "sandbox", id, "err", err)
				failed++
				continue
			}
			live++
		} else {
			stopped++
		}
		upgraded++
	}
	res["upgraded"], res["failed"], res["upgradedLive"], res["upgradedStopped"] = upgraded, failed, live, stopped
	return res
}

// ---------------------------------------------------------------------------
// Legacy /secrets (default environment)
// ---------------------------------------------------------------------------

func (s *Server) secretsJSON(e *envRow) (map[string]any, *envVersion, error) {
	v, err := s.envLatest(e)
	if err != nil {
		return nil, nil, err
	}
	files := []map[string]any{}
	for _, f := range v.Config.SecretFiles {
		files = append(files, map[string]any{"path": f.Path, "contents": f.Contents})
	}
	return map[string]any{"environmentId": v.ID, "subenvironmentId": e.ID, "envContents": v.Config.EnvContents, "secretFiles": files}, v, nil
}

func (s *Server) handleSecretsGet(c *Ctx) error {
	e, err := s.envDefault(c.P.User.ID, false)
	if err != nil {
		return err
	}
	m, _, err := s.secretsJSON(e)
	if err != nil {
		return err
	}
	return c.OK(200, "secrets.info", m)
}

func (s *Server) handleSecretsUpdate(c *Ctx) error {
	var req struct {
		EnvContents string          `json:"envContents"`
		SecretFiles []envSecretFile `json:"secretFiles"`
	}
	if err := c.Bind(&req); err != nil {
		return err
	}
	if err := envValidateContents(req.EnvContents); err != nil {
		return err
	}
	if err := envValidateFiles(req.SecretFiles); err != nil {
		return err
	}
	e, err := s.envDefault(c.P.User.ID, false)
	if err != nil {
		return err
	}
	cur, err := s.envLatest(e)
	if err != nil {
		return err
	}
	cfg := cur.Config
	cfg.EnvContents, cfg.SecretFiles = req.EnvContents, req.SecretFiles
	if _, err := s.envMint(e, cfg); err != nil {
		return err
	}
	res := s.envUpgrade(c.R.Context(), c.P, e, nil)
	m, _, err := s.secretsJSON(e)
	if err != nil {
		return err
	}
	m["success"] = true
	m["pushed"] = map[string]any{"updated": res["upgradedLive"], "failed": res["failed"]}
	return c.OK(200, "secrets.updated", m)
}

// ---------------------------------------------------------------------------
// ResolveEnvironment hook
// ---------------------------------------------------------------------------

func (s *Server) envResolve(ctx context.Context, req EnvRequest) (*ResolvedEnv, error) {
	u := req.User
	var e *envRow
	var err error
	switch {
	case req.Name == "" && req.Sandbox != nil && req.Op == "resume" && req.Sandbox.EnvironmentID != "":
		return nil, nil // stays on its pinned version (upgrade moves it)
	case req.Name == "" && req.Sandbox != nil && req.Op == "fork" && req.Sandbox.EnvironmentID != "":
		// Boat: a fork gets exactly its source's environment version, never
		// configuration the source did not have.
		return nil, nil
	case req.Name == "":
		if e, err = s.envDefault(u.ID, false); err != nil {
			return nil, err
		}
	default:
		if e, err = s.envByName(u, req.Org, req.Name); err != nil {
			return nil, err
		}
	}
	v, err := s.envLatest(e)
	if err != nil {
		return nil, err
	}
	holds := !req.NoEnv && s.envHoldsLogins(e, v.Config)
	if sb := req.Sandbox; sb != nil && req.Op == "resume" && req.Name != "" && sb.OrgID != "" && !sb.HoldsCreatorLogins && holds {
		return nil, Err(409, "environment_passes_logins", "This sandbox is shared with the organization and never receives logins; "+e.Name+" passes them. Pick an environment that passes no logins.")
	}
	n := v.Number
	return &ResolvedEnv{Name: e.Name, ID: e.ID, Version: &n, HoldsLogins: holds}, nil
}

// envByName finds an environment for a sandbox by id or name. A sandbox
// billed to an organization can name an organization environment (subject
// to its access list); when the user has an environment of the same name,
// the user's own wins (Boat environments__manage). A personal sandbox looks
// only in the user's own. An environment id is unambiguous.
func (s *Server) envByName(u *User, org *Org, name string) (*envRow, error) {
	if err := s.envEnsureBase(u.ID, false); err != nil {
		return nil, err
	}
	owners := []string{u.ID}
	if org != nil {
		_ = s.envEnsureBase(org.ID, true)
		owners = []string{u.ID, org.ID}
	}
	var forbidden error
	for _, byID := range []bool{true, false} {
		for _, owner := range owners {
			col := "name"
			if byID {
				col = "id"
			}
			e, err := scanEnv(s.DB.QueryRow(`SELECT `+envCols+` FROM environments WHERE owner_id=? AND `+col+`=? AND deleted_at IS NULL`, owner, name))
			if err != nil {
				continue
			}
			if owner != u.ID && !envCanUse(e.Access, s.memberRole(org.ID, u.ID), u.ID) {
				forbidden = Err(403, "environment_forbidden", "You cannot use this environment.")
				continue
			}
			return e, nil
		}
	}
	if forbidden != nil {
		return nil, forbidden
	}
	return nil, Err(400, "unknown_environment", "Unknown environment "+name+".")
}

// envOf returns the environment row and pinned version config of a sandbox
// (nil when it has none).
func (s *Server) envOf(sb *Sandbox) (*envRow, *envVersion) {
	if sb.EnvironmentID == "" {
		return nil, nil
	}
	e, err := s.envByID(sb.EnvironmentID)
	if err != nil {
		return nil, nil
	}
	// An org environment its owner may no longer use (removed from the org,
	// or from its access list) is not delivered (bugs-r10-opus 2).
	if e.Org && !s.orgCanUse(e.Access, e.OwnerID, sb.OwnerID) {
		return nil, nil
	}
	n := e.Latest
	if sb.EnvironmentVersion != nil {
		n = *sb.EnvironmentVersion
	}
	v, err := s.envVersionOf(e.ID, n)
	if err != nil {
		return e, nil
	}
	return e, v
}
