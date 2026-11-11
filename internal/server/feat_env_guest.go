package server

// What an environment (plus the AI credential profile) puts into a sandbox:
// computed at every boot (PrepareGuest) and hot-applied to running
// sandboxes on upgrade / key changes through the guest's files and
// commands API. Withheld secrets are deleted from the disk, not hidden.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// envPlan is what one sandbox receives from its environment and profile.
type envPlan struct {
	Env         map[string]string
	Files       []proto.File // Path relative to the home
	Repos       []proto.Repo
	Registry    []proto.RegistryLogin
	AIProxy     *proto.AIProxy
	WithholdKey bool // drop the in-sandbox CrowdVM key (CROWDVM_API_KEY)
	GitCreds    bool // repositories carry tokens / a GitHub login is passed
	ProfileID   string
	Proxied     bool
}

// envManaged records what the server put into a sandbox (keys and paths
// only, never values) so a later upgrade or scrub can remove it.
type envManaged struct {
	Keys       []string `json:"keys"`
	Files      []string `json:"files"`
	Registries []string `json:"registries,omitempty"`
	GitCreds   bool     `json:"gitCreds"`
	ProfileID  string   `json:"profileId,omitempty"`
	Proxied    bool     `json:"proxied"`
}

func envManagedOf(sb *Sandbox) envManaged {
	var m envManaged
	if v, ok := sb.Extra["envManaged"]; ok {
		b, _ := json.Marshal(v)
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// envPlanFor computes the plan. boot: a new AI proxy token is minted.
func (s *Server) envPlanFor(sb *Sandbox, boot bool) (*envPlan, error) {
	p := &envPlan{Env: map[string]string{}}
	if sb.NoEnv || sb.WipePending {
		p.WithholdKey = true
		return p, nil
	}
	e, v := s.envOf(sb)
	cfg, orgEnv := envDefaultConfig(), false
	if v != nil {
		cfg, orgEnv = v.Config, e.Org
	}
	// A shared org sandbox never receives its creator's personal logins.
	personal := sb.OrgID == "" || sb.HoldsCreatorLogins
	safe := cfg.SafeForThirdParties
	p.WithholdKey = safe || !cfg.PassSandboxCredentials || !personal
	if !safe && cfg.PassSecrets && (personal || orgEnv) {
		for k, val := range envVars(cfg.EnvContents) {
			p.Env[k] = val
		}
		for _, f := range cfg.SecretFiles {
			if rel := envHomeRel(f.Path); rel != "" {
				p.Files = append(p.Files, proto.File{Path: rel, Content: []byte(f.Contents), Mode: 0o600})
			}
		}
		p.Registry = append(p.Registry, cfg.RegistryLogins...)
	}
	if !safe && cfg.PassGithub {
		for _, r := range cfg.Repos {
			pr := proto.Repo{URL: r.CloneURL, Branch: firstNonEmpty(r.BaseBranch, "main"), SetupScript: r.SetupScript, SetupBlocking: r.SetupBlocking}
			if personal {
				if tok, _ := s.gitTokenFor(sb.OwnerID, r.SourceID, r.CloneURL); tok != "" && strings.HasPrefix(r.CloneURL, "http") {
					pr.Token, p.GitCreds = tok, true
				}
			}
			p.Repos = append(p.Repos, pr)
		}
		if personal {
			srcs, _ := s.gitSources(sb.OwnerID)
			for _, g := range srcs {
				if g.Kind == "github" {
					host := gitHost(g.BaseURL)
					p.Env["GH_TOKEN"], p.Env["GITHUB_TOKEN"], p.GitCreds = g.Token, g.Token, true
					if host != "github.com" {
						p.Env["GH_HOST"] = host
					}
					p.Files = append(p.Files, proto.File{Path: ".config/gh/hosts.yml", Mode: 0o600,
						Content: []byte(host + ":\n    oauth_token: " + g.Token + "\n    git_protocol: https\n")})
					break
				}
			}
		}
	}
	if (!safe && cfg.PassAgentsCredentials) || (safe && cfg.ProxyAICredentials) {
		if err := s.aiPlan(sb, cfg, personal, safe, boot, p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// envGuestURL is the server URL as reachable from inside a VM: a loopback
// public URL becomes the QEMU user-net host alias 10.0.2.2.
func (s *Server) envGuestURL() string {
	u, err := url.Parse(s.Cfg.PublicURL)
	if err != nil {
		return strings.TrimRight(s.Cfg.PublicURL, "/")
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) {
		if port := u.Port(); port != "" {
			u.Host = "10.0.2.2:" + port
		} else {
			u.Host = "10.0.2.2"
		}
	}
	return strings.TrimRight(u.String(), "/")
}

func (s *Server) envPrepareGuest(ctx context.Context, sb *Sandbox, g *GuestPrep) error {
	if g.Boot == proto.BootInspect {
		return nil
	}
	cfg := g.Config
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	cfg.Env["BOAT_ID"], cfg.Env["CROWDVM_ID"] = sb.ID, sb.ID
	if _, ok := cfg.Env["CROWDVM_API_URL"]; ok {
		cfg.Env["CROWDVM_API_URL"] = s.envGuestURL() + "/api/v1"
	}
	// Placeholders rotate on every start; aiPlan mints new ones if proxied.
	_, _ = s.DB.Exec(`DELETE FROM ai_proxy_tokens WHERE sandbox_id=?`, sb.ID)
	p, err := s.envPlanFor(sb, true)
	if err != nil {
		return err
	}
	var added []string
	for k, v := range p.Env {
		if _, taken := cfg.Env[k]; taken { // per-sandbox env and core vars win
			continue
		}
		cfg.Env[k] = v
		added = append(added, k)
	}
	if p.WithholdKey && cfg.Env["CROWDVM_API_KEY"] != "" {
		delete(cfg.Env, "CROWDVM_API_KEY")
		_, _ = s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE sandbox_id=? AND preset<>'sandbox-token' AND revoked_at IS NULL`, store.Now(), sb.ID)
	}
	if k := cfg.Env["CROWDVM_API_KEY"]; k != "" && cfg.Env["ASCII_TOKEN"] == "" {
		cfg.Env["ASCII_TOKEN"], cfg.Env["CROWDVM_TOKEN"] = k, k
	}
	var paths []string
	for _, f := range p.Files {
		cfg.SecretFiles = append(cfg.SecretFiles, proto.File{Path: proto.GuestHome + "/" + f.Path, Content: f.Content, Mode: f.Mode})
		paths = append(paths, f.Path)
	}
	cfg.Repos = append(cfg.Repos, p.Repos...)
	cfg.RegistryLogins = append(cfg.RegistryLogins, p.Registry...)
	if p.AIProxy != nil {
		cfg.AIProxy = p.AIProxy
	}
	sort.Strings(added)
	var regs []string
	for _, r := range p.Registry {
		regs = append(regs, r.Registry)
	}
	fromDisk := g.Spec != nil && g.Spec.ParentID != ""
	// Withheld secrets are deleted, not hidden (ENV-007): what the previous
	// boot wrote and this plan drops goes before the sandbox is reachable.
	if prev := envManagedOf(sb); fromDisk {
		var stale proto.GuestScrub
		for _, f := range prev.Files {
			if !slices.Contains(paths, f) {
				stale.Files = append(stale.Files, f)
			}
		}
		if prev.GitCreds && !p.GitCreds {
			stale.Files = append(stale.Files, ".config/crowdvm/git-credentials", ".git-credentials", ".config/gh/hosts.yml")
		}
		for _, r := range prev.Registries {
			if !slices.Contains(regs, r) {
				stale.RegistryLogouts = append(stale.RegistryLogouts, r)
			}
		}
		s.addPendingScrub(sb.ID, stale)
	}
	_ = s.SetSandboxExtra(sb.ID, "envManaged", envManaged{Keys: added, Files: paths, Registries: regs, GitCreds: p.GitCreds, ProfileID: p.ProfileID, Proxied: p.Proxied})
	// --no-env on a disk that may hold owner secrets: scrub before use
	// (the core keeps the sandbox securing until it is done).
	scrubbed, _ := sb.Extra["envScrubbed"].(bool)
	if (sb.NoEnv || sb.WipePending) && !fromDisk {
		_ = s.SetSandboxExtra(sb.ID, "envScrubbed", true) // clean base image
	} else if (sb.NoEnv || sb.WipePending) && !scrubbed {
		sc := s.ownerCredentialScrub(sb.OwnerID, sb.EnvironmentID)
		sc.Files = append(sc.Files, envScrubFiles...)
		sc.SSHKeys, sc.GitHubLogout = true, true
		sc.RegistryLogouts = nil // --no-env keeps docker logins (Boat)
		sc.Dirs = nil            // and repository clones
		s.addPendingScrub(sb.ID, sc)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Guest calls (through the node)
// ---------------------------------------------------------------------------

func (s *Server) envGuestDo(ctx context.Context, sb *Sandbox, method, rest string, body io.Reader, out any) error {
	node, err := s.NodeForSandbox(sb)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://node/v1/vms/"+sb.ID+"/guest/"+rest, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := (&http.Client{Transport: node.Transport()}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 && out != nil {
		return errNotFoundGuest
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("guest %s %s: %d %s", method, rest, resp.StatusCode, bytes.TrimSpace(b))
	}
	if w, ok := out.(io.Writer); ok {
		_, err = io.Copy(w, io.LimitReader(resp.Body, 8<<20))
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

var errNotFoundGuest = errors.New("not found")

func (s *Server) envGuestWrite(ctx context.Context, sb *Sandbox, rel string, content []byte) error {
	return s.envGuestDo(ctx, sb, "PUT", "files/raw?path="+url.QueryEscape(proto.GuestHome+"/"+rel), bytes.NewReader(content), nil)
}

func (s *Server) envGuestRead(ctx context.Context, sb *Sandbox, rel string) (string, error) {
	var b bytes.Buffer
	err := s.envGuestDo(ctx, sb, "GET", "files/raw?path="+url.QueryEscape(proto.GuestHome+"/"+rel), nil, &b)
	if errors.Is(err, errNotFoundGuest) {
		return "", nil
	}
	return b.String(), err
}

// envGuestRun runs a shell command as the sandbox user (no secrets in it:
// commands are recorded in the command history).
func (s *Server) envGuestRun(ctx context.Context, sb *Sandbox, cmd string) error {
	var out struct {
		ExitCode *int   `json:"exitCode"`
		Stderr   string `json:"stderr"`
	}
	b, _ := json.Marshal(map[string]any{"command": cmd, "timeoutSeconds": 120})
	if err := s.envGuestDo(ctx, sb, "POST", "commands", bytes.NewReader(b), &out); err != nil {
		return err
	}
	if out.ExitCode != nil && *out.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", *out.ExitCode, strings.TrimSpace(out.Stderr))
	}
	return nil
}

func envShQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var envSafePathRe = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)

func envHomeArg(rel string) string {
	if envSafePathRe.MatchString(rel) {
		return `"$HOME"/` + rel
	}
	return `"$HOME"/` + envShQuote(rel)
}

// envScrubFiles: credential files that --no-env removes (relative to home).
var envScrubFiles = []string{".config/gh/hosts.yml", ".git-credentials", ".config/crowdvm/git-credentials", ".claude/.credentials.json",
	".codex/auth.json", ".config/crowdvm/config.json", ".config/ascii/boat/config.json", ".kimi-code/credentials/kimi-code.json"}

// envApplyLive hot-applies the sandbox's current plan (new environment
// version, changed AI keys). scrub additionally deletes every owner secret
// the disk may hold (--no-env). SSH and desktop answer sandbox_securing
// while it runs.
func (s *Server) envApplyLive(ctx context.Context, sb *Sandbox, scrub bool) error {
	if !scrub && (sb.NoEnv || sb.WipePending) {
		return nil // nothing of the environment reaches it; a pending scrub keeps "securing"
	}
	unlock := s.sbLocks.Lock("env-live:" + sb.ID)
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	p, err := s.envPlanFor(sb, false)
	if err != nil {
		return err
	}
	prev := envManagedOf(sb)
	_ = s.SetSandboxExtra(sb.ID, "securing", true)
	defer func() { _ = s.SetSandboxExtra(sb.ID, "securing", false) }()

	// Env vars: the guest's managed ~/.bashrc block and /etc/profile.d.
	desired := map[string]string{}
	for k, v := range p.Env {
		if _, own := sb.Env[k]; !own && !slices.Contains(reservedEnv, k) {
			desired[k] = v
		}
	}
	var unset []string
	for _, k := range prev.Keys {
		if _, keep := desired[k]; !keep {
			if _, own := sb.Env[k]; !own {
				unset = append(unset, k)
			}
		}
	}
	if p.WithholdKey {
		unset = append(unset, "CROWDVM_API_KEY") // the neutral ASCII_TOKEN/CROWDVM_TOKEN stay (ENV-015)
		_, _ = s.DB.Exec(`UPDATE api_keys SET revoked_at=? WHERE sandbox_id=? AND preset<>'sandbox-token' AND revoked_at IS NULL`, store.Now(), sb.ID)
	}
	var rm []string
	for _, f := range prev.Files {
		if !slices.ContainsFunc(p.Files, func(x proto.File) bool { return x.Path == f }) {
			rm = append(rm, f)
		}
	}
	if prev.GitCreds && !p.GitCreds {
		rm = append(rm, ".config/crowdvm/git-credentials", ".git-credentials", ".config/gh/hosts.yml")
	}
	if scrub {
		rm = append(rm, envScrubFiles...)
		rm = append(rm, s.envOwnerSecretPaths(sb)...)
	}
	slices.Sort(rm)
	rm = slices.Compact(rm)
	var regs []string
	for _, r := range p.Registry {
		regs = append(regs, r.Registry)
	}
	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var filePaths []string
	for _, f := range p.Files {
		filePaths = append(filePaths, f.Path)
	}
	saveManaged := func() error {
		return s.SetSandboxExtra(sb.ID, "envManaged", envManaged{Keys: keys, Files: filePaths, Registries: regs, GitCreds: p.GitCreds && !scrub,
			ProfileID: firstNonEmpty(p.ProfileID, prev.ProfileID), Proxied: prev.Proxied})
	}

	// Preferred: the guest applies it in place, which also reaches the env
	// of new exec'd commands and running integrated agents (not only new
	// shells). Secret files and registry logins are full replacements.
	ac := proto.ApplyConfig{EnvSet: desired, EnvUnset: unset, SecretFiles: []proto.File{}, RegistryLogins: []proto.RegistryLogin{}, Repos: p.Repos}
	ac.SecretFiles = append(ac.SecretFiles, p.Files...)
	ac.RegistryLogins = append(ac.RegistryLogins, p.Registry...)
	if len(rm) > 0 || scrub {
		ac.Scrub = &proto.GuestScrub{ID: "scr_" + randHex(8), Files: slices.DeleteFunc(slices.Clone(rm), func(f string) bool { return slices.Contains(filePaths, f) }),
			SSHKeys: scrub, GitHubLogout: scrub}
	}
	acBody, _ := json.Marshal(ac)
	err = s.envGuestDo(ctx, sb, "POST", "internal/apply-config", bytes.NewReader(acBody), nil)
	if err != nil && (errors.Is(err, errNotFoundGuest) || strings.Contains(err.Error(), ": 404 ") || strings.Contains(err.Error(), ": 405 ")) {
		// Guests ship with every boot: one without apply-config is outdated.
		// It gets the new version at its next boot (PrepareGuest).
		return ErrD(409, "agent_outdated", "The sandbox's guest agent cannot apply a configuration live; it gets the new environment version when it is stopped and resumed.",
			map[string]any{"retryable": false})
	}
	if err != nil {
		return err
	}
	return saveManaged()
}

// envOwnerSecretPaths lists every secret file path of the owner's
// environments (all versions) and of the sandbox's environment.
func (s *Server) envOwnerSecretPaths(sb *Sandbox) []string {
	var out []string
	rows, err := s.DB.Query(`SELECT v.config FROM environment_versions v JOIN environments e ON e.id=v.env_id WHERE e.owner_id=? OR e.id=?`, sb.OwnerID, sb.EnvironmentID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var sealed string
		if rows.Scan(&sealed) != nil {
			continue
		}
		b, err := s.envOpen(sealed)
		if err != nil {
			continue
		}
		var cfg envConfig
		if json.Unmarshal(b, &cfg) == nil {
			for _, f := range cfg.SecretFiles {
				if rel := envHomeRel(f.Path); rel != "" {
					out = append(out, rel)
				}
			}
		}
	}
	return out
}

// envPushProfile hot-applies changed AI credentials to the running
// sandboxes that use a profile.
func (s *Server) envPushProfile(profileID string) {
	rows, err := s.DB.Query(`SELECT id FROM sandboxes WHERE deleted_at IS NULL AND state IN ('ready','idle','running') AND json_extract(extra,'$.envManaged.profileId')=?`, profileID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		id := id
		s.Go(func(ctx context.Context) {
			if sb, err := s.SandboxByID(id); err == nil && sb.Usable() {
				if err := s.envApplyLive(ctx, sb, false); err != nil {
					s.Log.Warn("live credential update failed", "sandbox", id, "err", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Events and guard
// ---------------------------------------------------------------------------

// envSecuredPathRe: every route that reaches into a sandbox's machine.
var envSecuredPathRe = regexp.MustCompile(`^/api/v1/sandboxes/([^/]+)/(tunnel|desktop|browser|host|sshkey|commands|command-history|processes|files|artifacts|prompt|prompts|events|conversations|steer|interrupt)(/|$)`)

// envGuard refuses every call into a sandbox while it is being secured
// (boot-time scrub, live environment upgrade removing secrets).
func (s *Server) envGuard(c *Ctx) error {
	m := envSecuredPathRe.FindStringSubmatch(c.R.URL.Path)
	if m == nil {
		return nil
	}
	sb, err := s.SandboxByID(m[1])
	if err != nil {
		return nil
	}
	if isSecuring(sb) && (sb.Active() || sb.State == api.StateError) && sb.State != api.StateArchiving {
		return securingErr()
	}
	return nil
}
