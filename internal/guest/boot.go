package guest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// bootState makes boot tasks idempotent across agent restarts within one boot
// (the service has Restart=always and the config drive still says FirstBoot).
type bootState struct {
	BootID         string `json:"bootId"`
	SandboxID      string `json:"sandboxId"`
	FirstBootDone  string `json:"firstBootDone,omitempty"` // sandbox id whose first boot ran
	BackgroundDone bool   `json:"backgroundDone,omitempty"`
}

func (a *Agent) bootStatePath() string { return filepath.Join(a.data, "boot.json") }

func (a *Agent) loadBootState() bootState {
	var s bootState
	if b, err := os.ReadFile(a.bootStatePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (a *Agent) saveBootState(s bootState) {
	b, _ := json.Marshal(s)
	_ = os.WriteFile(a.bootStatePath(), b, 0o644)
}

// Boot runs the quick, synchronous boot tasks (before the API listens):
// the scrub (ENV-018), env files, secret and one-time files, authorized
// keys. Slow identity work (growpart/resize2fs, SSH host keys, firewall)
// runs in the background so the agent answers health sooner; /v1/sshkey
// and tunnels to port 22 wait for the host keys.
func (a *Agent) Boot(ctx context.Context) {
	if a.inspect() {
		// Inspector VM: serve the snapshot's files only. No env/bashrc
		// writes, no secrets, no services, no identity changes.
		a.markSSHReady()
		a.clonesFinished()
		a.fw.markReady()
		return
	}
	st := a.loadBootState()
	fresh := st.BootID != a.bootID || st.SandboxID != a.cfg.SandboxID
	if fresh {
		snap := fromSnapshot(st, a.cfg.Boot)
		a.startPlaybook(ctx, snap) // LIFE-018: record + prefetch from the first moment
		// A live snapshot (fork, deploy, resume after a crash) still holds
		// what a stop deletes: delete it now (SNAP-009/011).
		unsaved := snap && !a.savedCleanly(st.BootID)
		if unsaved {
			a.cleanUnsavedAtBoot()
		}
		go a.dockerAfterBoot(ctx, unsaved)
		st = bootState{BootID: a.bootID, SandboxID: a.cfg.SandboxID, FirstBootDone: st.FirstBootDone}
	}
	if reason := a.previousBoot(a.loadBootState().BootID); reason != "" && fresh {
		a.Emit(EvBootPrevious, map[string]any{"reason": reason, "boot": a.cfg.Boot})
	}
	cfg := a.config()
	a.runScrub(cfg.Scrub)
	if !a.unpriv && fresh {
		a.setHostname()
		if a.cfg.Boot == proto.BootCreate || a.cfg.Boot == proto.BootFork {
			a.newMachineID()
		}
		a.installCLI()
		a.ensureChromeFirstRun()
		go func() {
			if a.cfg.Boot != proto.BootRecover {
				// SSH-005: a new host key on every create, fork and resume
				// (pinned clients get it from POST /sshkey).
				a.regenSSHHostKeys()
			}
			a.markSSHReady()
			a.growRootFS()
			a.firewallReset()
			a.fw.markReady()
		}()
	} else {
		a.markSSHReady()
		a.fw.markReady()
	}
	a.writeEnvFiles()
	a.installSkills() // AGT-025, DOCS-003: the crowdvm skill in every harness
	a.removeStaleManaged(cfg)
	for _, f := range cfg.SecretFiles {
		a.writeConfigFile(f, 0o600)
	}
	firstBoot := a.cfg.FirstBoot && st.FirstBootDone != a.cfg.SandboxID
	if firstBoot {
		for _, f := range a.cfg.Files {
			a.writeConfigFile(f, 0o644)
		}
		if strings.TrimSpace(a.cfg.SetupScript) != "" {
			a.setSetup(SetupPending, "")
		}
	}
	if len(cfg.SSHAuthorizedKeys) > 0 {
		if err := a.addAuthorizedKeys(cfg.SSHAuthorizedKeys); err != nil {
			a.log.Printf("authorized keys: %v", err)
		}
	}
	if !st.BackgroundDone {
		a.markReposPending(cfg.Repos) // ENV-032: blocking repos hold prompts from now on
	}
	if st.BackgroundDone || !a.missingRepos(cfg.Repos, firstBoot || a.cfg.Boot == proto.BootFork) {
		a.clonesFinished() // nothing to clone this boot (or the agent restarted after)
	}
	a.saveBootState(st)
}

// clonesFinished marks the boot-time repository clones done: health stops
// reporting reposCloning, which the node waits for before ready (LIFE-009).
func (a *Agent) clonesFinished() { a.clonesOnce.Do(func() { close(a.clonesDone) }) }

// missingRepos: a repository BootBackground will clone.
func (a *Agent) missingRepos(repos []proto.Repo, clone bool) bool {
	for _, r := range repos {
		// .git, not the folder: a secret file (backend/.env) creates the folder first.
		if _, err := os.Stat(filepath.Join(a.real(repoVirtPath(r)), ".git")); err != nil && clone {
			return true
		}
	}
	return false
}

// reposCloning: this boot's environment repositories are still being cloned.
func (a *Agent) reposCloning() bool {
	select {
	case <-a.clonesDone:
		return false
	default:
		return true
	}
}

// markSSHReady closes sshReady once.
func (a *Agent) markSSHReady() {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.sshReady:
	default:
		close(a.sshReady)
	}
}

// waitSSHReady waits (bounded) for this boot's SSH host keys.
func (a *Agent) waitSSHReady(ctx context.Context) {
	select {
	case <-a.sshReady:
	case <-ctx.Done():
	case <-time.After(30 * time.Second):
	}
}

// BootBackground runs slow boot work after the API is up: registry logins
// (and logouts of registries a newer config dropped), repository clone +
// setup scripts, and the one-time setup script.
func (a *Agent) BootBackground(ctx context.Context) {
	if a.inspect() {
		return
	}
	st := a.loadBootState()
	if st.BackgroundDone {
		return // the agent restarted; this boot's work already ran
	}
	firstBoot := a.cfg.FirstBoot && st.FirstBootDone != a.cfg.SandboxID
	if firstBoot {
		st.FirstBootDone = a.cfg.SandboxID
	}
	st.BackgroundDone = true
	a.saveBootState(st)

	cfg := a.config()
	if !a.unpriv {
		a.registryLogouts(ctx, a.staleRegistries(cfg))
		a.registryLogins(ctx)
	}
	a.saveManaged(cfg)
	// Repos are cloned on create and fork (a fork into another environment
	// gets that environment's repos, ENV-027); setup scripts run every boot.
	a.runRepos(ctx, cfg.Repos, firstBoot || a.cfg.Boot == proto.BootFork, a.clonesFinished)
	if firstBoot && strings.TrimSpace(a.cfg.SetupScript) != "" {
		a.runSetupScript(ctx, a.cfg.SetupScript)
	}
}

// ---------------------------------------------------------------------------
// Identity (root only)
// ---------------------------------------------------------------------------

func (a *Agent) run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		a.log.Printf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(lastLines(string(out), 3)))
	}
	return err
}

func (a *Agent) setHostname() {
	if a.cfg.SandboxID != "" { // read by `crowdvm self` inside the sandbox
		_ = os.MkdirAll("/etc/crowdvm", 0o755)
		_ = os.WriteFile("/etc/crowdvm/sandbox-id", []byte(a.cfg.SandboxID+"\n"), 0o644)
	}
	h := a.cfg.Hostname
	if h == "" || userSetsHostname("/etc/systemd/system") {
		return // Boat's "Keep a hostname": an enabled unit of the user sets it
	}
	_ = os.WriteFile("/etc/hostname", []byte(h+"\n"), 0o644)
	if err := syscall.Sethostname([]byte(h)); err != nil {
		a.log.Printf("sethostname: %v", err)
	}
	b, _ := os.ReadFile("/etc/hosts")
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, "127.0.1.1") {
			lines[i], found = "127.0.1.1 "+h, true
		}
	}
	if !found {
		lines = append(lines, "127.0.1.1 "+h)
	}
	_ = os.WriteFile("/etc/hosts", []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

var partRe = regexp.MustCompile(`^(/dev/(?:[a-z]+|nvme\d+n\d+|mmcblk\d+))p?(\d+)$`)

// growRootFS grows the root partition and filesystem to the disk size.
func (a *Agent) growRootFS() {
	out, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "/").Output()
	if err != nil {
		return
	}
	src := strings.TrimSpace(string(out))
	if m := partRe.FindStringSubmatch(src); m != nil {
		if _, err := exec.LookPath("growpart"); err == nil {
			// exit 1 = NOCHANGE, which is fine
			_ = exec.Command("growpart", m[1], m[2]).Run()
		}
	}
	if _, err := exec.LookPath("resize2fs"); err == nil {
		_ = a.run("resize2fs", src)
	}
}

// newMachineID gives a created/forked sandbox its own /etc/machine-id.
// journald keeps writing to /var/log/journal/<old id> until it restarts,
// which made `journalctl` (it reads the current id's folder) look empty
// after a fork, and leaked the source's journal into the fork: restart it
// and drop the old folder.
func (a *Agent) newMachineID() {
	old, cur, err := a.replaceMachineID("/etc/machine-id")
	if err != nil {
		a.log.Printf("machine-id: %v", err)
	}
	if old == cur || cur == "" {
		return
	}
	go func() {
		_ = exec.Command("systemctl", "restart", "systemd-journald.service").Run()
		if old != "" && !strings.ContainsAny(old, "/.") {
			_ = os.RemoveAll(filepath.Join("/var/log/journal", old))
		}
	}()
}

// replaceMachineID writes a new random machine ID (a v4 UUID as 32 hex
// digits, as systemd makes them) to path in one rename, so a reader sees
// the old ID or the new one, never a missing or empty file. systemd-networkd
// derives its DHCP client ID (DUID-EN) from the machine ID when it sets up
// the NIC, at the same moment of the boot as this runs; without the file it
// fails the link for good ("Failed to set DUID-EN: No such file or
// directory"), the guest gets no address and the node never reaches it
// (boot timeout). A file PID1 mounted over path (first boot, ID not
// committed yet) holds an ID made this boot: it is kept.
func (a *Agent) replaceMachineID(path string) (old, cur string, err error) {
	b, _ := os.ReadFile(path)
	old = strings.TrimSpace(string(b))
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return old, "", err
	}
	id[6] = id[6]&0x0f | 0x40 // version 4
	id[8] = id[8]&0x3f | 0x80 // RFC 4122 variant
	cur = hex.EncodeToString(id[:])
	if err := a.writeFileAtomic(path, []byte(cur+"\n"), 0o444, false); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return old, old, nil
		}
		return old, "", err
	}
	return old, cur, nil
}

// installCLI installs the in-sandbox CLI from the config drive (TOOL-007,
// URL-013..015): /usr/local/bin/crowdvm plus the host, boat and box names
// (the binary dispatches on argv[0]). It authenticates with the
// CROWDVM_TOKEN / ASCII_TOKEN and CROWDVM_API_URL variables of the sandbox
// env. Replaced only when it differs; written via rename (an image hook may
// install it too).
func (a *Agent) installCLI() {
	src := a.cliSource
	if src == "" {
		return
	}
	b, err := os.ReadFile(src)
	if err != nil || len(b) == 0 {
		return
	}
	const dst = "/usr/local/bin/crowdvm"
	if cur, err := os.ReadFile(dst); err != nil || !bytes.Equal(cur, b) {
		if err := a.writeFileAtomic(dst, b, 0o755, false); err != nil {
			a.log.Printf("install crowdvm CLI: %v", err)
			return
		}
	}
	for _, n := range []string{"host", "boat", "box"} {
		l := filepath.Join("/usr/local/bin", n)
		if t, err := os.Readlink(l); err == nil && t == "crowdvm" {
			continue
		}
		if fi, err := os.Lstat(l); err == nil && fi.Mode()&os.ModeSymlink == 0 && fi.Size() > 0 {
			continue // a real program of that name: leave it
		}
		_ = os.Remove(l)
		_ = os.Symlink("crowdvm", l)
	}
}

// firewallReset (URL-019/022, TOOL-014): snapshots keep no firewall rules.
// Images with /usr/local/lib/crowdvm/firewall-reset reset ufw from a unit
// before ufw starts; the agent only adds its port when it is not 7070.
// Older images that have ufw get the same defaults from here.
func (a *Agent) firewallReset() {
	port := proto.GuestPort
	if _, p, err := net.SplitHostPort(a.cfg.ListenAddr); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			port = n
		}
	}
	const script = "/usr/local/lib/crowdvm/firewall-reset"
	if _, err := os.Stat(script); err == nil {
		if port != proto.GuestPort {
			_ = a.run(script, strconv.Itoa(port)+"/tcp")
		}
		return
	}
	if _, err := exec.LookPath("ufw"); err != nil {
		return
	}
	for _, args := range [][]string{{"--force", "reset"}, {"default", "deny", "incoming"}, {"default", "allow", "outgoing"},
		{"allow", "22/tcp"}, {"allow", strconv.Itoa(port) + "/tcp"}, {"--force", "enable"}} {
		if a.run("ufw", args...) != nil {
			return
		}
	}
	// ufw reset leaves backup copies of the old rules behind
	old, _ := filepath.Glob("/etc/ufw/*.rules.[0-9]*")
	for _, f := range old {
		_ = os.Remove(f)
	}
}

// ---------------------------------------------------------------------------
// Previous boot (LIFE-026): clean stop vs crash
// ---------------------------------------------------------------------------

const EvBootPrevious = "boot.previous" // {"reason":"clean|crash|unknown","bootId"}

func (a *Agent) cleanMarker() string { return filepath.Join(a.data, "clean-stop") }

// markCleanShutdown records that this boot ended through prepare-stop.
func (a *Agent) markCleanShutdown() {
	_ = os.WriteFile(a.cleanMarker(), []byte(a.bootID+"\n"), 0o644)
}

// unmarkCleanShutdown: the machine runs on after a prepare-stop (a stop
// that failed thaws it), so this boot did not end there. Reports whether a
// prepare-stop of this boot was undone.
func (a *Agent) unmarkCleanShutdown() bool {
	if b, err := os.ReadFile(a.cleanMarker()); err == nil && strings.TrimSpace(string(b)) == a.bootID {
		return os.Remove(a.cleanMarker()) == nil
	}
	return false
}

// savedCleanly: boot prevBootID of this disk ended through prepare-stop, so
// its cleanup already ran.
func (a *Agent) savedCleanly(prevBootID string) bool {
	b, err := os.ReadFile(a.cleanMarker())
	return err == nil && prevBootID != "" && strings.TrimSpace(string(b)) == prevBootID
}

// cleanUnsavedAtBoot deletes what a snapshot does not keep (Boat
// snapshots/whats-saved) from a disk that comes from a live snapshot, where
// prepare-stop did not run: the contents of ~/.cache and the ignore-file
// matches. ~/.cache is swapped for an empty folder at once and the old one
// is deleted in the background. /tmp is emptied by systemd-tmpfiles at boot;
// the Docker build cache and unused images go in dockerAfterBoot.
func (a *Agent) cleanUnsavedAtBoot() {
	cache := filepath.Join(a.home, ".cache")
	if fi, err := os.Stat(cache); err == nil && fi.IsDir() {
		if ents, _ := os.ReadDir(cache); len(ents) > 0 {
			trash := filepath.Join(a.data, "trash-cache-"+strconv.FormatInt(time.Now().UnixNano(), 36))
			if os.Rename(cache, trash) == nil {
				if err := os.Mkdir(cache, fi.Mode().Perm()); err == nil {
					a.chown(cache)
				}
				go os.RemoveAll(trash)
			} else {
				var errs []string
				a.emptyDir(cache, &errs)
			}
		}
	}
	skip := map[string]bool{a.data: true, cache: true}
	if n := len(cleanIgnored(a.home, skip)); n > 0 {
		a.log.Printf("boot from a live snapshot: deleted %d ignore-file matches", n)
	}
	if old, _ := filepath.Glob(filepath.Join(a.data, "trash-cache-*")); len(old) > 0 {
		go func() { // left over by a boot that ended during the delete
			for _, p := range old {
				_ = os.RemoveAll(p)
			}
		}()
	}
}

// dockerAfterBoot starts the containers that ran when this disk was saved
// again and, on a disk from a live snapshot, deletes the build cache and
// the images nothing uses.
func (a *Agent) dockerAfterBoot(ctx context.Context, unsaved bool) {
	if a.inspect() || !a.dockerOK() {
		return
	}
	a.restartDocker(ctx)
	if unsaved && a.waitDocker(ctx, dockerWait) {
		var errs []string
		a.pruneDocker(ctx, true, &errs)
		for _, e := range errs {
			a.log.Printf("boot: %s", e)
		}
	}
}

// previousBoot classifies how the previous boot of this disk ended.
func (a *Agent) previousBoot(prevBootID string) string {
	if prevBootID == "" || prevBootID == a.bootID {
		return ""
	}
	b, err := os.ReadFile(a.cleanMarker())
	switch {
	case err == nil && strings.TrimSpace(string(b)) == prevBootID:
		return "clean"
	case a.cfg.Boot == proto.BootFork || a.cfg.Boot == proto.BootCreate:
		return "" // live snapshot of another machine: not a reboot of this one
	default:
		return "crash"
	}
}

func (a *Agent) regenSSHHostKeys() {
	keys, _ := filepath.Glob("/etc/ssh/ssh_host_*")
	for _, k := range keys {
		_ = os.Remove(k)
	}
	if a.run("ssh-keygen", "-A") == nil {
		_ = exec.Command("systemctl", "try-restart", "ssh.service").Run()
	}
}

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// sandboxEnv is the AI proxy variables the integrated agents need, then the
// configured env, which wins (e.g. DeepSeek's deliberate empty
// ANTHROPIC_API_KEY over the proxy placeholder).
func (a *Agent) sandboxEnv() map[string]string {
	env := map[string]string{}
	for k, v := range a.aiProxyEnv() {
		env[k] = v
	}
	for k, v := range a.config().Env {
		if envNameRe.MatchString(k) {
			env[k] = v
		}
	}
	return env
}

// commandEnv is the process environment for commands run as the user.
func (a *Agent) commandEnv(extra map[string]string) []string {
	env := map[string]string{}
	if a.unpriv {
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
	} else {
		env["PATH"] = "/home/user/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/snap/bin"
		env["LANG"], env["LC_ALL"] = sysLocale(), sysLocale() // TOOL-010
		env["DISPLAY"] = desktopDisplay                       // DESK-020: wmctrl / X apps from exec
		env["XDG_RUNTIME_DIR"] = "/run/user/" + strconv.Itoa(a.uid)
		env["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=/run/user/" + strconv.Itoa(a.uid) + "/bus"
	}
	env["HOME"] = a.home
	env["USER"] = a.userName
	env["LOGNAME"] = a.userName
	env["SHELL"] = "/bin/bash"
	if a.unpriv {
		env["TMPDIR"] = a.tmp
	}
	if a.cfg.SandboxID != "" {
		env["CROWDVM_SANDBOX_ID"] = a.cfg.SandboxID
	}
	for k, v := range a.sandboxEnv() {
		env[k] = v
	}
	for k, v := range extra {
		if envNameRe.MatchString(k) {
			env[k] = v
		}
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func exportLines(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export " + k + "=" + shQuote(env[k]) + "\n")
	}
	return b.String()
}

const (
	bashrcBegin = "# >>> crowdvm env (managed by crowdvm-guest, rewritten every boot) >>>"
	bashrcEnd   = "# <<< crowdvm env <<<"
)

// writeEnvFiles exports the sandbox env in /etc/profile.d (login shells) and
// in a managed block at the top of ~/.bashrc (non-login shells: its first
// lines run before Ubuntu's "not interactive -> return").
func (a *Agent) writeEnvFiles() {
	env := a.sandboxEnv()
	exports := exportLines(env) + a.claudeLoginGuard(env)
	defer a.writeCredentialFiles(env) // KEY-017, ENV-016
	if !a.unpriv {
		p := "/etc/profile.d/crowdvm-env.sh"
		// DESK-020: X programs from ssh/login shells find the desktop
		// (an ssh -X session keeps its own DISPLAY).
		body := "# managed by crowdvm-guest\nexport DISPLAY=\"${DISPLAY:-" + desktopDisplay + "}\"\n" + exports
		if err := a.writeFileAtomic(p, []byte(body), 0o644, false); err != nil {
			a.log.Printf("profile.d: %v", err)
		}
	}
	p := filepath.Join(a.home, ".bashrc")
	cur, _ := os.ReadFile(p)
	rest := stripBlock(string(cur))
	block := ""
	if len(env) > 0 {
		block = bashrcBegin + "\n" + exports + bashrcEnd + "\n"
	}
	next := block + rest
	if next == string(cur) || (next == "" && len(cur) == 0) {
		return
	}
	if err := a.writeFileAtomic(p, []byte(next), 0o644, true); err != nil {
		a.log.Printf("bashrc: %v", err)
	}
}

func stripBlock(s string) string {
	i := strings.Index(s, bashrcBegin)
	if i < 0 {
		return s
	}
	j := strings.Index(s[i:], bashrcEnd)
	if j < 0 {
		return s
	}
	end := i + j + len(bashrcEnd)
	if end < len(s) && s[end] == '\n' {
		end++
	}
	return s[:i] + s[end:]
}

func (a *Agent) fileSkipped(path, why string) {
	a.log.Printf("file %s: %s", path, why)
	a.Emit(EvFileSkipped, map[string]any{"path": path, "error": why})
}

// writeConfigFile writes a config/secret file. Paths under /home/user or /tmp
// are owned by the user; other absolute paths are written as root (skipped
// when unprivileged).
func (a *Agent) writeConfigFile(f proto.File, defMode os.FileMode) {
	mode := defMode
	if f.Mode != 0 {
		mode = os.FileMode(f.Mode) & os.ModePerm
	}
	if real, _, err := a.resolve(f.Path, true); err == nil {
		// Never replace a folder; say so instead of failing silently
		// (bugs-r6-opus 4).
		if fi, err := os.Lstat(real); err == nil && fi.IsDir() {
			a.fileSkipped(f.Path, "a folder already exists at this path")
			return
		}
		if err := a.mkdirAll(filepath.Dir(real), 0o755); err != nil {
			a.fileSkipped(f.Path, err.Error())
			return
		}
		if err := a.writeFileAtomic(real, f.Content, mode, true); err != nil {
			a.fileSkipped(f.Path, err.Error())
			return
		}
		_ = os.Chmod(real, mode)
		return
	}
	if a.unpriv || !filepath.IsAbs(f.Path) {
		a.log.Printf("file %s: skipped (outside the sandbox home)", f.Path)
		return
	}
	p := filepath.Clean(f.Path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		a.log.Printf("file %s: %v", p, err)
		return
	}
	if err := a.writeFileAtomic(p, f.Content, mode, false); err != nil {
		a.log.Printf("file %s: %v", p, err)
	}
	_ = os.Chmod(p, mode)
}

func (a *Agent) registryLogins(ctx context.Context) {
	logins := a.config().RegistryLogins
	if len(logins) == 0 {
		return
	}
	if _, err := exec.LookPath("docker"); err != nil {
		a.log.Printf("registry logins: docker is not installed")
		return
	}
	for _, l := range logins {
		cmd := exec.CommandContext(ctx, "docker", "login", l.Registry, "-u", l.Username, "--password-stdin")
		cmd.Stdin = strings.NewReader(l.Password)
		cmd.Env = a.commandEnv(nil)
		cmd.Dir = a.home
		cmd.SysProcAttr = a.sysProcAttr()
		if out, err := cmd.CombinedOutput(); err != nil {
			a.log.Printf("docker login %s: %v: %s", l.Registry, err, strings.TrimSpace(lastLines(string(out), 2)))
		}
	}
}

// sysLocale is the image's default locale (/etc/default/locale), so exec'd
// commands match ssh sessions (TOOL-010). bash -lc does not read it.
var sysLocale = sync.OnceValue(func() string {
	b, _ := os.ReadFile("/etc/default/locale")
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "LANG="); ok {
			if v = strings.Trim(v, `"'`); v != "" {
				return v
			}
		}
	}
	return "C.UTF-8"
})

var hostnameCmdRe = regexp.MustCompile(`(?m)^\s*ExecStart(Pre|Post)?=.*\bhostnamectl\s+(set-)?hostname\s+\S`)

// userSetsHostname: an enabled systemd unit (linked into a *.wants folder)
// sets the hostname, as Boat's "Keep a hostname" recipe does. The guest then
// leaves the hostname to it instead of racing it at every boot.
func userSetsHostname(dir string) bool {
	links, _ := filepath.Glob(filepath.Join(dir, "*.wants", "*.service"))
	for _, l := range links {
		if b, err := os.ReadFile(l); err == nil && hostnameCmdRe.Match(b) {
			return true
		}
	}
	return false
}
