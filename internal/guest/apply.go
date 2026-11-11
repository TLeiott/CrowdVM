package guest

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// ---------------------------------------------------------------------------
// Managed state (ENV-007): what the guest wrote for the environment, so a
// newer config (at boot after an upgrade, or live via apply-config) deletes
// withheld secrets instead of hiding them.
// ---------------------------------------------------------------------------

type managedState struct {
	SecretFiles []string `json:"secretFiles,omitempty"` // normalized paths (absolute)
	Registries  []string `json:"registries,omitempty"`
}

func (a *Agent) managedPath() string { return filepath.Join(a.data, "managed.json") }

func (a *Agent) loadManaged() managedState {
	var m managedState
	if b, err := os.ReadFile(a.managedPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (a *Agent) writeManaged(m managedState) {
	b, _ := json.Marshal(m)
	_ = os.WriteFile(a.managedPath(), b, 0o600)
}

func normPath(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(proto.GuestHome, p)
	}
	return filepath.Clean(p)
}

func secretPaths(fs []proto.File) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, normPath(f.Path))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func registryNames(ls []proto.RegistryLogin) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Registry)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// removeStaleManaged deletes secret files written for an earlier config that
// cfg no longer lists, and records cfg's files as managed.
func (a *Agent) removeStaleManaged(cfg proto.GuestConfig) []string {
	old := a.loadManaged()
	keep := secretPaths(cfg.SecretFiles)
	keep = append(keep, secretPaths(cfg.Files)...)
	var removed []string
	for _, p := range old.SecretFiles {
		if slices.Contains(keep, p) {
			continue
		}
		if a.removeConfigPath(p, false) {
			removed = append(removed, p)
		}
	}
	old.SecretFiles = secretPaths(cfg.SecretFiles)
	a.writeManaged(old)
	return removed
}

// staleRegistries: registries logged in for an earlier config, not in cfg.
func (a *Agent) staleRegistries(cfg proto.GuestConfig) []string {
	var out []string
	cur := registryNames(cfg.RegistryLogins)
	for _, r := range a.loadManaged().Registries {
		if !slices.Contains(cur, r) {
			out = append(out, r)
		}
	}
	return out
}

func (a *Agent) saveManaged(cfg proto.GuestConfig) {
	a.writeManaged(managedState{SecretFiles: secretPaths(cfg.SecretFiles), Registries: registryNames(cfg.RegistryLogins)})
}

// removeConfigPath deletes a file (or with dir, a tree) given as an absolute
// or home-relative path. Paths in the sandbox roots are resolved safely;
// other absolute paths are removed as root (not when unprivileged).
func (a *Agent) removeConfigPath(p string, dir bool) bool {
	rm := os.Remove
	if dir {
		rm = os.RemoveAll
	}
	if real, _, err := a.resolve(p, false); err == nil {
		if real == a.home || real == a.tmp {
			return false
		}
		return rm(real) == nil
	}
	if a.unpriv || !filepath.IsAbs(p) {
		return false
	}
	p = filepath.Clean(p)
	if p == "/" || strings.Count(p, "/") < 2 {
		return false // never a top-level directory
	}
	return rm(p) == nil
}

func (a *Agent) registryLogouts(ctx context.Context, regs []string) []string {
	if len(regs) == 0 {
		return nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	var done []string
	for _, r := range regs {
		// as the user (~/.docker/config.json) and as root
		cmd := exec.CommandContext(ctx, "docker", "logout", r)
		cmd.Env, cmd.Dir, cmd.SysProcAttr = a.commandEnv(nil), a.home, a.sysProcAttr()
		err := cmd.Run()
		if !a.unpriv {
			_ = exec.CommandContext(ctx, "docker", "logout", r).Run()
		}
		if err == nil {
			done = append(done, r)
		}
	}
	return done
}

// ---------------------------------------------------------------------------
// Scrub (ENV-018, SNAP-023): synchronous, before the API serves
// ---------------------------------------------------------------------------

type scrubResult struct {
	ID    string `json:"id"`
	Done  bool   `json:"done"`
	Error string `json:"error,omitempty"`
}

func (a *Agent) scrubPath() string { return filepath.Join(a.data, "scrub.json") }

// runScrub deletes what s lists. It is idempotent and re-runs on every boot
// that carries it (the server drops it once confirmed).
func (a *Agent) runScrub(s *proto.GuestScrub) scrubResult {
	if s == nil {
		return scrubResult{}
	}
	res := scrubResult{ID: s.ID}
	var errs []string
	if s.History {
		a.hist.wipe()
		_ = os.RemoveAll(a.processLogDir())
		// The saver's agent prompts and events too (bugs-r9-sonnet).
		if a.agentsM != nil {
			a.agentsM.Wipe()
		} else {
			_ = os.RemoveAll(filepath.Join(a.data, "agents"))
		}
	}
	for _, f := range s.Files {
		if real, _, err := a.resolve(f, false); err == nil {
			if err := os.Remove(real); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err.Error())
			}
		} else if !a.unpriv && filepath.IsAbs(f) {
			if err := os.Remove(filepath.Clean(f)); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err.Error())
			}
		}
	}
	for _, d := range s.Dirs {
		real, v, err := a.resolve(d, false)
		if err != nil || v == proto.GuestHome || v == "/tmp" {
			errs = append(errs, "refusing to delete "+d)
			continue
		}
		if err := os.RemoveAll(real); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if s.SSHKeys {
		keys, _ := filepath.Glob(filepath.Join(a.home, ".ssh", "id_*"))
		for _, k := range keys {
			_ = os.Remove(k)
		}
	}
	if s.AuthorizedKeys {
		p := filepath.Join(a.home, ".ssh", "authorized_keys")
		if _, err := os.Stat(p); err == nil {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if s.GitHubLogout {
		// Logging out only edits hosts.yml; deleting it is the same and
		// never prompts or needs the network.
		_ = os.Remove(filepath.Join(a.home, ".config", "gh", "hosts.yml"))
	}
	if len(s.RegistryLogouts) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		a.registryLogouts(ctx, s.RegistryLogouts)
		cancel()
		for _, r := range s.RegistryLogouts { // without docker: edit the auths directly
			a.dropDockerAuth(filepath.Join(a.home, ".docker", "config.json"), r)
			if !a.unpriv {
				a.dropDockerAuth("/root/.docker/config.json", r)
			}
		}
	}
	res.Done = len(errs) == 0
	if !res.Done {
		res.Error = strings.Join(errs, "; ")
	}
	b, _ := json.Marshal(res)
	_ = os.WriteFile(a.scrubPath(), b, 0o600)
	return res
}

func (a *Agent) dropDockerAuth(p, registry string) {
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return
	}
	auths, _ := cfg["auths"].(map[string]any)
	if _, ok := auths[registry]; !ok {
		return
	}
	delete(auths, registry)
	out, _ := json.MarshalIndent(cfg, "", "\t")
	_ = a.writeFileAtomic(p, out, 0, false)
}

// GET /v1/internal/scrub -> {id, done, error}
func (a *Agent) handleScrub(w http.ResponseWriter, r *http.Request) {
	var res scrubResult
	if b, err := os.ReadFile(a.scrubPath()); err == nil {
		_ = json.Unmarshal(b, &res)
	}
	if s := a.config().Scrub; s != nil && s.ID != res.ID {
		res = scrubResult{ID: s.ID} // not run yet (cannot happen after Boot)
	}
	ok(w, "guest.scrub", res)
}

// ---------------------------------------------------------------------------
// POST /v1/internal/apply-config (ENV-007): hot-apply a new environment
// ---------------------------------------------------------------------------

var applyMu sync.Mutex

func (a *Agent) handleApplyConfig(w http.ResponseWriter, r *http.Request) {
	var req proto.ApplyConfig
	if !decodeJSON(w, r, &req) {
		return
	}
	applyMu.Lock()
	defer applyMu.Unlock()
	res := map[string]any{}
	if req.Scrub != nil {
		sr := a.runScrub(req.Scrub)
		a.updateConfig(func(c *proto.GuestConfig) { c.Scrub = req.Scrub })
		res["scrub"] = sr
	}
	var newRepos []proto.Repo
	a.updateConfig(func(c *proto.GuestConfig) {
		if req.Env != nil {
			c.Env = req.Env
		}
		if len(req.EnvSet) > 0 || len(req.EnvUnset) > 0 {
			env := maps.Clone(c.Env)
			if env == nil {
				env = map[string]string{}
			}
			for _, k := range req.EnvUnset {
				delete(env, k)
			}
			maps.Copy(env, req.EnvSet)
			c.Env = env
		}
		if req.SecretFiles != nil {
			c.SecretFiles = req.SecretFiles
		}
		if req.Repos != nil {
			for _, nr := range req.Repos {
				if !slices.ContainsFunc(c.Repos, func(o proto.Repo) bool { return repoVirtPath(o) == repoVirtPath(nr) }) {
					newRepos = append(newRepos, nr)
				}
			}
			c.Repos = req.Repos
		}
		if req.RegistryLogins != nil {
			c.RegistryLogins = req.RegistryLogins
		}
		if req.AIProxy != nil {
			c.AIProxy = req.AIProxy
		} else if req.ClearAIProxy {
			c.AIProxy = nil
		}
	})
	cfg := a.config()
	a.saveApplied(cfg)
	if a.agentsM != nil && (req.AIProxy != nil || req.ClearAIProxy) {
		a.agentsM.SetAIProxy(cfg.AIProxy)
	}
	// env: profile.d + ~/.bashrc block (new shells), commands and agents read
	// the config on every start.
	a.writeEnvFiles()
	keys := make([]string, 0, len(cfg.Env))
	for k := range a.sandboxEnv() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	res["envKeys"] = keys
	// secret files: delete the ones no longer listed, write the rest.
	var written []string
	removed := []string{}
	if req.SecretFiles != nil {
		removed = append(removed, a.removeStaleManaged(cfg)...)
		for _, f := range cfg.SecretFiles {
			a.writeConfigFile(f, 0o600)
			written = append(written, normPath(f.Path))
		}
	}
	res["filesWritten"], res["filesRemoved"] = nonNil(written), removed
	if len(req.SSHAuthorizedKeys) > 0 {
		if err := a.addAuthorizedKeys(req.SSHAuthorizedKeys); err != nil {
			writeErr(w, 500, "apply_failed", "authorized keys: "+err.Error())
			return
		}
		a.updateConfig(func(c *proto.GuestConfig) {
			c.SSHAuthorizedKeys = append(slices.Clone(c.SSHAuthorizedKeys), req.SSHAuthorizedKeys...)
		})
	}
	// registries
	var loggedOut, loggedIn []string
	if req.RegistryLogins != nil && !a.unpriv {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		loggedOut = a.registryLogouts(ctx, a.staleRegistries(cfg))
		a.registryLogins(ctx)
		cancel()
		loggedIn = registryNames(cfg.RegistryLogins)
	}
	if req.RegistryLogins != nil {
		m := a.loadManaged()
		m.Registries = registryNames(cfg.RegistryLogins)
		a.writeManaged(m)
	}
	res["registriesLoggedIn"], res["registriesLoggedOut"] = nonNil(loggedIn), nonNil(loggedOut)
	// repos: clone + set up the new ones in the background
	var started []string
	for _, nr := range newRepos {
		started = append(started, repoVirtPath(nr))
	}
	if len(newRepos) > 0 {
		a.markReposPending(newRepos)
		go a.runRepos(context.Background(), newRepos, true, nil)
	} else if req.Repos != nil {
		a.writeGitCredentials() // rotated tokens
	}
	res["reposStarted"] = nonNil(started)
	ok(w, "guest.config_applied", res)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// appliedConfig keeps what apply-config set for the rest of this boot, so an
// agent restart does not fall back to the (older) config drive and rewrite
// withheld secrets. The next boot's config drive carries the new version.
type appliedConfig struct {
	BootID            string                `json:"bootId"`
	SandboxID         string                `json:"sandboxId"`
	Env               map[string]string     `json:"env"`
	SecretFiles       []proto.File          `json:"secretFiles"`
	Repos             []proto.Repo          `json:"repos"`
	RegistryLogins    []proto.RegistryLogin `json:"registryLogins"`
	AIProxy           *proto.AIProxy        `json:"aiProxy"`
	SSHAuthorizedKeys []string              `json:"sshAuthorizedKeys"`
}

func (a *Agent) appliedPath() string { return filepath.Join(a.data, "applied-config.json") }

func (a *Agent) saveApplied(c proto.GuestConfig) {
	b, _ := json.Marshal(appliedConfig{BootID: a.bootID, SandboxID: a.cfg.SandboxID, Env: c.Env, SecretFiles: c.SecretFiles,
		Repos: c.Repos, RegistryLogins: c.RegistryLogins, AIProxy: c.AIProxy, SSHAuthorizedKeys: c.SSHAuthorizedKeys})
	if err := os.WriteFile(a.appliedPath()+".tmp", b, 0o600); err == nil {
		_ = os.Rename(a.appliedPath()+".tmp", a.appliedPath())
	}
}

// loadApplied overlays this boot's applied config onto a.cfg (New).
func (a *Agent) loadApplied() {
	b, err := os.ReadFile(a.appliedPath())
	if err != nil {
		return
	}
	var ac appliedConfig
	if json.Unmarshal(b, &ac) != nil || ac.BootID != a.bootID || ac.SandboxID != a.cfg.SandboxID || a.bootID == "" {
		_ = os.Remove(a.appliedPath())
		return
	}
	a.cfg.Env, a.cfg.SecretFiles, a.cfg.Repos = ac.Env, ac.SecretFiles, ac.Repos
	a.cfg.RegistryLogins, a.cfg.AIProxy, a.cfg.SSHAuthorizedKeys = ac.RegistryLogins, ac.AIProxy, ac.SSHAuthorizedKeys
}
