package server

// Securing: credentials are removed from a sandbox's disk BEFORE it becomes
// usable (ENV-018 --no-env, SNAP-023 deploy of another account's saved
// snapshot, ENV-007 secrets a newer environment version withholds).
//
// Every boot whose sandbox has a pending scrub ships it to the guest
// (GuestConfig.Scrub, done synchronously before :7070 serves) and keeps the
// sandbox "provisioned" with extra.securing=true after the guest answers.
// The server then confirms the guest's scrub (GET /v1/internal/scrub) or,
// for guests without it, runs the same scrub through the commands API.
// Only then is the sandbox marked ready. Exec, files, prompts, SSH,
// desktop and public URLs need a usable sandbox, so none of them reach the
// disk earlier. A scrub that fails 3 times fails the boot (state error).

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

// pendingScrubOf returns the scrub waiting for sb's next boot (nil = none).
func pendingScrubOf(sb *Sandbox) *proto.GuestScrub {
	v, ok := sb.Extra["pendingScrub"]
	if !ok || v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	var sc proto.GuestScrub
	if json.Unmarshal(b, &sc) != nil || scrubEmpty(&sc) {
		return nil
	}
	return &sc
}

func scrubEmpty(sc *proto.GuestScrub) bool {
	return sc == nil || (len(sc.Files) == 0 && len(sc.Dirs) == 0 && !sc.SSHKeys && !sc.AuthorizedKeys && !sc.GitHubLogout && len(sc.RegistryLogouts) == 0 && !sc.History)
}

func uniqSorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// addPendingScrub merges add into sb's pending scrub. A scrub only grows
// until a boot completes it, so nothing withheld is ever forgotten.
func (s *Server) addPendingScrub(sbID string, add proto.GuestScrub) {
	if scrubEmpty(&add) {
		return
	}
	unlock := s.sbLocks.Lock("scrub:" + sbID)
	defer unlock()
	sb, err := s.SandboxByID(sbID)
	if err != nil {
		return
	}
	cur := pendingScrubOf(sb)
	if cur == nil {
		cur = &proto.GuestScrub{}
	}
	cur.ID = "" // a changed scrub gets a new id at the next boot
	cur.Files = uniqSorted(append(cur.Files, add.Files...))
	cur.Dirs = uniqSorted(append(cur.Dirs, add.Dirs...))
	cur.RegistryLogouts = uniqSorted(append(cur.RegistryLogouts, add.RegistryLogouts...))
	cur.SSHKeys = cur.SSHKeys || add.SSHKeys
	cur.AuthorizedKeys = cur.AuthorizedKeys || add.AuthorizedKeys
	cur.GitHubLogout = cur.GitHubLogout || add.GitHubLogout
	cur.History = cur.History || add.History // bugs-r9-opus 2: dropped here before
	_ = s.SetSandboxExtra(sbID, "pendingScrub", cur)
}

// scrubForBoot finalizes the pending scrub for a boot with config g: paths
// the config writes itself are dropped (they are the new owner's), the
// scrub gets an id, and the sandbox is marked securing.
func (s *Server) scrubForBoot(sbID string, g *proto.GuestConfig) {
	sb, err := s.SandboxByID(sbID)
	if err != nil {
		return
	}
	sc := pendingScrubOf(sb)
	if sc == nil {
		_ = s.SetSandboxExtra(sbID, "securing", false)
		return
	}
	rel := func(p string) string {
		if strings.HasPrefix(p, proto.GuestHome+"/") {
			return strings.TrimPrefix(p, proto.GuestHome+"/")
		}
		return p
	}
	written := map[string]bool{}
	for _, f := range append(slices.Clone(g.SecretFiles), g.Files...) {
		written[rel(path.Clean(f.Path))] = true
	}
	sc.Files = slices.DeleteFunc(sc.Files, func(p string) bool { return written[rel(path.Clean(p))] })
	repos := map[string]bool{}
	for _, r := range g.Repos {
		repos[rel(repoHomePath(r))] = true
	}
	sc.Dirs = slices.DeleteFunc(sc.Dirs, func(p string) bool { return repos[rel(path.Clean(p))] })
	regs := map[string]bool{}
	for _, r := range g.RegistryLogins {
		regs[r.Registry] = true
	}
	sc.RegistryLogouts = slices.DeleteFunc(sc.RegistryLogouts, func(r string) bool { return regs[r] })
	if sc.ID == "" {
		sc.ID = "scr_" + randHex(8)
	}
	_ = s.SetSandboxExtra(sbID, "pendingScrub", sc)
	_ = s.SetSandboxExtra(sbID, "securing", true)
	g.Scrub = sc
}

// repoHomePath is where the guest clones r (same rule as the guest).
func repoHomePath(r proto.Repo) string {
	if r.Path != "" {
		if strings.HasPrefix(r.Path, "/") {
			return path.Clean(r.Path)
		}
		return path.Join(proto.GuestHome, r.Path)
	}
	return path.Join(proto.GuestHome, repoDirName(r.URL))
}

func repoDirName(u string) string {
	name := strings.TrimSuffix(path.Base(strings.TrimRight(u, "/")), ".git")
	if i := strings.LastIndexAny(name, ":"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

func isSecuring(sb *Sandbox) bool {
	v, _ := sb.Extra["securing"].(bool)
	return v
}

var securingRun sync.Map // sandbox id -> struct{} while secure() runs

// secureAsync finishes securing a starting sandbox whose guest answers, then
// marks it ready.
func (s *Server) secureAsync(id string) {
	if _, busy := securingRun.LoadOrStore(id, struct{}{}); busy {
		return
	}
	s.Go(func(ctx context.Context) {
		defer securingRun.Delete(id)
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			sb, e := s.SandboxByID(id)
			if e != nil || sb.DeletedAt != nil || !sb.starting() {
				return
			}
			sc := pendingScrubOf(sb)
			if sc == nil {
				break
			}
			if err = s.runScrub(ctx, sb, sc); err == nil {
				_ = s.SetSandboxExtra(id, "pendingScrub", nil)
				if sb.NoEnv || sb.WipePending {
					_ = s.SetSandboxExtra(id, "envScrubbed", true)
				}
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
		}
		if err != nil {
			s.failSandbox(id, "scrub_failed: the credentials of the previous owner could not be removed: "+err.Error())
			return
		}
		_ = s.SetSandboxExtra(id, "securing", false)
		if sb, e := s.SandboxByID(id); e == nil && sb.starting() {
			s.markReady(sb)
		}
	})
}

// runScrub confirms the guest's own boot-time scrub, or performs it
// through the commands API when the guest has no scrub support.
func (s *Server) runScrub(ctx context.Context, sb *Sandbox, sc *proto.GuestScrub) error {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var st struct {
		ID    string `json:"id"`
		Done  bool   `json:"done"`
		Error string `json:"error"`
	}
	err := s.envGuestDo(cctx, sb, "GET", "internal/scrub", nil, &st)
	switch {
	case err == nil && st.ID == sc.ID && st.Done && st.Error == "":
		return nil
	case err == nil && st.ID == sc.ID && st.Error != "":
		s.Log.Warn("guest scrub failed; scrubbing through commands", "sandbox", sb.ID, "err", st.Error)
	case err != nil && !errors.Is(err, errNotFoundGuest) && !strings.Contains(err.Error(), ": 404 ") && !strings.Contains(err.Error(), ": 405 "):
		return err
	}
	return s.envGuestRun(cctx, sb, scrubCommand(sc))
}

// scrubCommand is the shell form of a scrub (run as the sandbox user; the
// command history records it, so it holds paths only, never values).
func scrubCommand(sc *proto.GuestScrub) string {
	var b strings.Builder
	b.WriteString("set +e\n")
	if sc.GitHubLogout {
		b.WriteString(`(command -v gh >/dev/null && for h in $(gh auth status 2>&1 | sed -n 's/^ *\([A-Za-z0-9.-]*\.[A-Za-z]*\)$/\1/p'; echo github.com); do gh auth logout --hostname "$h" </dev/null >/dev/null 2>&1; done) || true` + "\n")
	}
	if sc.SSHKeys {
		b.WriteString(`rm -f "$HOME"/.ssh/id_*` + "\n")
	}
	if sc.AuthorizedKeys {
		b.WriteString(`[ -f "$HOME"/.ssh/authorized_keys ] && : > "$HOME"/.ssh/authorized_keys` + "\n")
	}
	for _, f := range sc.Files {
		b.WriteString("rm -f -- " + scrubArg(f) + "\n")
	}
	for _, d := range sc.Dirs {
		if a := scrubArg(d); a != `"$HOME"/` {
			b.WriteString("rm -rf -- " + a + "\n")
		}
	}
	for _, r := range sc.RegistryLogouts {
		q := envShQuote(r)
		b.WriteString("(docker logout " + q + " >/dev/null 2>&1; sudo -n docker logout " + q + " >/dev/null 2>&1) || true\n")
	}
	b.WriteString("true\n")
	return b.String()
}

func scrubArg(p string) string {
	if strings.HasPrefix(p, proto.GuestHome+"/") {
		p = strings.TrimPrefix(p, proto.GuestHome+"/")
	}
	if strings.HasPrefix(p, "/") {
		return envShQuote(path.Clean(p))
	}
	return envHomeArg(path.Clean(p))
}

// markReady moves a starting sandbox whose guest answers to its usable
// state (first boot done, hydrated, wipe applied).
func (s *Server) markReady(sb *Sandbox) {
	now := store.Now()
	sets := map[string]any{"first_boot_done": 1, "hydrated": 1, "hydrated_at": now, "wipe_pending": 0,
		"error": nil, "health": "ok", "health_reason": nil, "degraded_since": nil}
	if s.vmHydrating(sb) { // LIFE-013: usable, but layers still arrive (markHydrated)
		delete(sets, "hydrated")
		delete(sets, "hydrated_at")
	}
	if sb.WipePending {
		sets["holds_creator_logins"] = 0
		sets["no_env"] = 1
	}
	if billingPause(sb) == pauseNotAnswering { // answers again after a recover
		_ = s.SetSandboxExtra(sb.ID, "billingPaused", nil)
	}
	// Every layer of the chain this machine booted on is on its node now.
	if _, now := sets["hydrated"]; now && sb.NodeID != "" {
		for _, id := range s.snapshotChain(sb.HeadSnapshotID) {
			s.addLocation(id, sb.NodeID)
		}
	}
	evs := []Event{{Type: EvSandboxReady}}
	if sb.HealthReason == "recovering" || sb.HealthReason == "rebuilding" {
		evs = append(evs, Event{Type: EvSandboxRecovered})
	}
	// Boat: sandbox.hydrated after ready when a restored sandbox (resume,
	// fork, a saved snapshot or share code) is fully materialized. A lazily
	// pulled chain sends it later (markHydrated).
	if _, now := sets["hydrated"]; now && restoredBoot(sb) {
		evs = append(evs, Event{Type: EvSandboxHydrated, Data: map[string]any{"snapshotId": sb.HeadSnapshotID}})
	}
	if _, ok := s.transition(sb.ID, startingStates, readyState(sb), sets, evs...); ok {
		s.ensureRun(sb.ID)
	}
}

// vmHydrating reports whether the node says sb's VM still reads layers of
// its snapshot chain from the server (lazy chain, LIFE-013).
func (s *Server) vmHydrating(sb *Sandbox) bool {
	c := s.nodes.get(sb.NodeID)
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vms[sb.ID].Hydrating
}

// markHydrated: a lazily booted chain is complete on nodeID. Its layers
// become locations of that node and sandbox.hydrated fires.
func (s *Server) markHydrated(sb *Sandbox, nodeID string) {
	res, _ := s.DB.Exec(`UPDATE sandboxes SET hydrated=1, hydrated_at=? WHERE id=? AND hydrated=0`, store.Now(), sb.ID)
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	for _, id := range s.snapshotChain(sb.HeadSnapshotID) {
		s.addLocation(id, nodeID)
	}
	s.Bus.Publish(Event{Type: EvSandboxHydrated, SandboxID: sb.ID, SandboxName: sb.Name, OwnerID: sb.OwnerID, OrgID: sb.OrgID, NodeID: nodeID,
		Data: map[string]any{"snapshotId": sb.HeadSnapshotID, "lazy": true}})
}

// ---------------------------------------------------------------------------
// What a scrub removes
// ---------------------------------------------------------------------------

// ownerCredentialScrub lists everything of owner's environments (all
// versions) plus env (the source sandbox's environment): secret files,
// repository clones and registry logins.
func (s *Server) ownerCredentialScrub(ownerID, envID string) proto.GuestScrub {
	var sc proto.GuestScrub
	// By env_id too: the environment row may be gone (its organization was
	// deleted) while a saved snapshot still needs its versions scrubbed.
	rows, err := s.DB.Query(`SELECT v.config FROM environment_versions v LEFT JOIN environments e ON e.id=v.env_id WHERE e.owner_id=? OR v.env_id=?`, ownerID, envID)
	if err != nil {
		return sc
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
		if json.Unmarshal(b, &cfg) != nil {
			continue
		}
		for _, f := range cfg.SecretFiles {
			if rel := envHomeRel(f.Path); rel != "" {
				sc.Files = append(sc.Files, rel)
			}
		}
		for _, r := range cfg.Repos {
			if n := repoDirName(firstNonEmpty(r.CloneURL, r.URL)); n != "" && n != "." && n != ".." {
				sc.Dirs = append(sc.Dirs, n)
			}
		}
		for _, r := range cfg.RegistryLogins {
			sc.RegistryLogouts = append(sc.RegistryLogouts, r.Registry)
		}
	}
	return sc
}

// foreignDeployScrub: a sandbox deployed from another account's (or org
// member's) saved snapshot loses the saver's logins, API keys, environment
// secret files, repository clones and SSH keys (SNAP-023).
func (s *Server) foreignDeployScrub(saverID, sourceSandboxID string) proto.GuestScrub {
	envID := ""
	if src, err := s.SandboxByID(sourceSandboxID); err == nil {
		envID = src.EnvironmentID
	}
	sc := s.ownerCredentialScrub(saverID, envID)
	sc.Files = append(sc.Files, envScrubFiles...)
	sc.SSHKeys, sc.AuthorizedKeys, sc.GitHubLogout = true, true, true
	// The saver's command history (with output: printed secrets) stays on
	// the guest disk for 90 days (bugs-r8-opus 4).
	sc.History = true
	return sc
}

// securingErr is the answer to sandbox calls while a scrub runs.
func securingErr() error {
	return ErrD(409, "sandbox_securing", "The sandbox is removing the previous owner's credentials. Retry in a few seconds.", map[string]any{"retryable": true, "retryAfterSeconds": 3})
}

// ensureRun opens a metering run for sb unless one is open. Metering starts
// once the machine is up (provisioned or ready, LIFE-005) and stops when the
// stop begins.
func (s *Server) ensureRun(id string) {
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sandbox_runs WHERE sandbox_id=? AND ended_at IS NULL`, id).Scan(&n)
	if n > 0 {
		return
	}
	if sb, err := s.SandboxByID(id); err == nil && sb.DeletedAt == nil {
		s.openRun(sb)
	}
}

// sandboxTokenPreset marks the neutral per-sandbox token (ENV-015).
const sandboxTokenPreset = "sandbox-token"

// sandboxToken mints (or rotates) the neutral per-sandbox token every
// sandbox gets as ASCII_TOKEN / CROWDVM_TOKEN, protected or not: it can
// read its own sandbox and manage its public URLs, nothing else. The
// broader in-sandbox CLI key (CROWDVM_API_KEY) is the one that
// passSandboxCredentials, no-env and safe-for-third-parties withhold.
func (s *Server) sandboxToken(sb *Sandbox) (string, error) {
	secret := newKeySecret()
	res, err := s.DB.Exec(`UPDATE api_keys SET secret_hash=?, key_prefix=?, last_four=? WHERE sandbox_id=? AND preset=? AND revoked_at IS NULL`,
		hashToken(secret), secret[:12], secret[len(secret)-4:], sb.ID, sandboxTokenPreset)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return secret, nil
	}
	_, err = s.DB.Exec(`INSERT INTO api_keys(id,secret_hash,user_id,org_id,name,key_prefix,last_four,actions,sandbox_ids,environment_ids,preset,sandbox_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"key_"+randToken(16), hashToken(secret), sb.OwnerID, nz(sb.OrgID), "sandbox token "+sb.ID, secret[:12], secret[len(secret)-4:],
		mustJSON([]string{"sandbox.read", "host"}), mustJSON([]string{sb.ID}), scopeJSON(nil), sandboxTokenPreset, sb.ID, store.Now())
	if err != nil {
		return "", err
	}
	return secret, nil
}

// restoredBoot: this boot materializes a snapshot (not a fresh base image,
// not a restart of the same running disk).
func restoredBoot(sb *Sandbox) bool {
	switch proto.BootKind(sb.BootKind) {
	case proto.BootResume, proto.BootFork:
		return true
	case proto.BootCreate:
		return sb.SourceKind == "saved" || sb.SourceKind == "share"
	}
	return false
}
