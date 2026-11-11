package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Setup script statuses (Boat's setupStatus).
const (
	SetupPending = "pending"
	SetupRunning = "running"
	SetupDone    = "done"
	SetupFailed  = "failed"
)

type RepoStatus struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	Status string `json:"status"` // cloning | setup | done | failed
	Error  string `json:"error,omitempty"`
}

// SetupInfo is the body of GET /v1/internal/setup-status (plus ok/type).
type SetupInfo struct {
	Status     *string      `json:"setupStatus"` // null when there is no setup script
	Error      *string      `json:"setupError"`
	StartedAt  *time.Time   `json:"startedAt,omitempty"`
	FinishedAt *time.Time   `json:"finishedAt,omitempty"`
	LogPath    string       `json:"logPath,omitempty"`
	Repos      []RepoStatus `json:"repos,omitempty"`
	// Blocking: a repository marked setupBlocking is still pending, cloning
	// or running its setup script this boot (integrated-agent prompts wait).
	Blocking bool `json:"blocking"`
	// ReposBoot: boot id the repo statuses belong to (they are per boot).
	ReposBoot string `json:"reposBoot,omitempty"`
}

type setupState struct {
	path string
	mu   sync.Mutex
	info SetupInfo
}

func loadSetupState(path, bootID string) *setupState {
	s := &setupState{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.info)
	}
	if s.info.ReposBoot != bootID {
		s.info.Repos, s.info.ReposBoot = nil, bootID // per boot
	}
	for i, r := range s.info.Repos { // same boot: the agent restarted while repos ran
		if r.Status == "pending" || r.Status == "cloning" || r.Status == "setup" {
			s.info.Repos[i].Status = "failed"
			s.info.Repos[i].Error = "interrupted: the agent restarted while this repository was set up"
		}
	}
	if s.info.Status != nil && (*s.info.Status == SetupRunning || *s.info.Status == SetupPending) {
		// the agent or the machine stopped while it ran: it will not finish
		s.info.Status = ptr(SetupFailed)
		s.info.Error = ptr("interrupted: the machine stopped or the agent restarted while the setup script ran")
		s.info.FinishedAt = ptr(time.Now().UTC())
		s.save()
	}
	return s
}

func (s *setupState) get() SetupInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.info
	i.Repos = append([]RepoStatus(nil), s.info.Repos...)
	return i
}

// repoSetupBlocking reports whether integrated-agent prompts must wait: a repo
// with SetupBlocking is pending, cloning or running its setup script this
// boot (ENV-032). Boot marks every repo pending only when this boot's
// background work has not run yet, so an agent restart never blocks forever.
func (a *Agent) repoSetupBlocking() bool {
	blocking := map[string]bool{}
	for _, r := range a.config().Repos {
		if r.SetupBlocking {
			blocking[repoVirtPath(r)] = true
		}
	}
	if len(blocking) == 0 {
		return false
	}
	for _, r := range a.setup.get().Repos {
		if blocking[r.Path] && (r.Status == "pending" || r.Status == "cloning" || r.Status == "setup") {
			return true
		}
	}
	return false
}

func (s *setupState) save() {
	b, _ := json.Marshal(s.info)
	_ = os.WriteFile(s.path, b, 0o644)
}

func (s *setupState) update(f func(*SetupInfo)) SetupInfo {
	s.mu.Lock()
	f(&s.info)
	s.save()
	i := s.info
	s.mu.Unlock()
	return i
}

func (a *Agent) setSetup(status, errMsg string) {
	now := time.Now().UTC()
	a.setup.update(func(i *SetupInfo) {
		i.Status = ptr(status)
		i.Error = nil
		if errMsg != "" {
			i.Error = ptr(errMsg)
		}
		switch status {
		case SetupRunning:
			i.StartedAt = &now
		case SetupDone, SetupFailed:
			i.FinishedAt = &now
		}
	})
	a.Emit(EvSetupStatus, map[string]any{"status": status, "error": errMsg})
}

func (a *Agent) setRepo(rs RepoStatus) {
	a.setup.update(func(i *SetupInfo) {
		for k := range i.Repos {
			if i.Repos[k].Path == rs.Path {
				i.Repos[k] = rs
				return
			}
		}
		i.Repos = append(i.Repos, rs)
	})
	blocking := a.repoSetupBlocking()
	a.setup.update(func(i *SetupInfo) { i.Blocking = blocking })
	a.Emit(EvRepoSetup, map[string]any{"url": rs.URL, "path": rs.Path, "status": rs.Status, "error": rs.Error, "blocking": blocking})
}

// markReposPending records every repo as pending (before the background
// clone/setup runs), so blocking repos hold prompts from the first moment.
func (a *Agent) markReposPending(repos []proto.Repo) {
	for _, r := range repos {
		a.setRepo(RepoStatus{URL: r.URL, Path: repoVirtPath(r), Status: "pending"})
	}
}

// runSetupScript runs the one-time setup script as the user, in a login bash.
func (a *Agent) runSetupScript(ctx context.Context, script string) {
	a.setSetup(SetupRunning, "")
	logPath := filepath.Join(a.data, "setup.log")
	errPath := filepath.Join(a.data, "setup.err")
	a.setup.update(func(i *SetupInfo) { i.LogPath = logPath })
	code, err := a.runLogged(ctx, script, a.home, logPath, errPath)
	if err == nil && code == 0 {
		a.setSetup(SetupDone, "")
		return
	}
	msg := ""
	if err != nil {
		msg = "could not run setup script: " + err.Error()
	} else {
		tail, _ := readTail(errPath, 2048)
		msg = fmt.Sprintf("exit code %d", code)
		if t := strings.TrimSpace(string(tail)); t != "" {
			msg += ": " + t
		}
	}
	a.setSetup(SetupFailed, msg)
}

// runLogged runs script with `bash -lc` as the user in dir, stdout to
// outPath and stderr to errPath. Returns the exit code (-1 on a signal).
func (a *Agent) runLogged(ctx context.Context, script, dir, outPath, errPath string) (int, error) {
	out, err := os.Create(outPath)
	if err != nil {
		return -1, err
	}
	defer out.Close()
	errF, err := os.Create(errPath)
	if err != nil {
		return -1, err
	}
	defer errF.Close()
	cmd := a.shellCmd(script, dir, nil)
	cmd.Stdout, cmd.Stderr = out, errF
	if err := a.spawn(cmd, "setup"); err != nil {
		return -1, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()
	err = cmd.Wait()
	close(done)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// ---------------------------------------------------------------------------
// Repositories
// ---------------------------------------------------------------------------

func repoVirtPath(r proto.Repo) string {
	if r.Path != "" {
		if filepath.IsAbs(r.Path) {
			return filepath.Clean(r.Path)
		}
		return filepath.Join(proto.GuestHome, r.Path)
	}
	name := strings.TrimSuffix(filepath.Base(strings.TrimRight(r.URL, "/")), ".git")
	if i := strings.LastIndexAny(name, ":"); i >= 0 {
		name = name[i+1:]
	}
	return filepath.Join(proto.GuestHome, name)
}

func (a *Agent) gitCredFile() string { return filepath.Join(a.home, ".config/crowdvm/git-credentials") }

// writeGitCredentials stores https tokens for the configured repos and,
// with GH_TOKEN in the env (a GitHub source), for github.com (every boot and
// env change, so rotated tokens apply). github.com then gets a global
// credential helper so git push works outside cloned repos too (ENV-016).
func (a *Agent) writeGitCredentials() {
	var lines []string
	hosts := map[string]bool{}
	for _, r := range a.config().Repos {
		u, err := url.Parse(r.URL)
		if r.Token == "" || err != nil || u.Scheme != "https" {
			continue
		}
		lines = append(lines, "https://x-access-token:"+url.QueryEscape(r.Token)+"@"+u.Host)
		hosts[u.Host] = true
	}
	gh := a.config().Env["GH_TOKEN"]
	if gh != "" && !hosts["github.com"] {
		lines = append(lines, "https://x-access-token:"+url.QueryEscape(gh)+"@github.com")
	}
	p := a.gitCredFile()
	if len(lines) == 0 {
		if _, err := os.Stat(p); err == nil { // tokens withdrawn
			_ = os.Remove(p)
		}
		return
	}
	if err := a.mkdirAll(filepath.Dir(p), 0o700); err != nil {
		a.log.Printf("git credentials: %v", err)
		return
	}
	if err := a.writeFileAtomic(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600, true); err != nil {
		a.log.Printf("git credentials: %v", err)
	}
	if gh != "" {
		a.gitGlobal("credential.https://github.com.helper", "store --file="+p)
	}
}

// gitGlobal sets a value in the user's ~/.gitconfig (unchanged when equal).
func (a *Agent) gitGlobal(key, val string) {
	gc := filepath.Join(a.home, ".gitconfig")
	if out, err := exec.Command("git", "config", "--file", gc, "--get", key).Output(); err == nil && strings.TrimSpace(string(out)) == val {
		return
	}
	if out, err := exec.Command("git", "config", "--file", gc, key, val).CombinedOutput(); err != nil {
		a.log.Printf("git config %s: %v: %s", key, err, strings.TrimSpace(string(out)))
		return
	}
	a.chown(gc)
}

// runRepos clones missing repos (when clone is set: create, fork, or repos
// added by apply-config), then runs each repo setup script. All clones come
// first (Boat: the repositories are cloned at ready, setup scripts start at
// ready); cloned is called between the two phases (nil = no-op).
func (a *Agent) runRepos(ctx context.Context, repos []proto.Repo, clone bool, cloned func()) {
	if cloned == nil {
		cloned = func() {}
	}
	if len(repos) == 0 {
		cloned()
		return
	}
	a.repoMu.Lock() // apply-config and boot never set up repos concurrently
	defer a.repoMu.Unlock()
	a.writeGitCredentials()
	helper := "store --file=" + a.gitCredFile()
	failed := map[int]bool{}
	for i, r := range repos {
		virt := repoVirtPath(r)
		real := a.real(virt)
		rs := RepoStatus{URL: r.URL, Path: virt}
		if _, err := os.Stat(filepath.Join(real, ".git")); err == nil || !clone {
			continue // cloned already
		}
		// The folder can exist without a clone: a secret file such as
		// backend/.env is written into it first. Clone next to it, then merge
		// (the files already there win).
		dest := real
		if _, err := os.Stat(real); err == nil {
			dest = real + ".crowdvm-clone"
			_ = os.RemoveAll(dest)
		}
		rs.Status = "cloning"
		a.setRepo(rs)
		args := []string{"-c", "credential.helper=" + helper, "clone"}
		if r.Branch != "" {
			args = append(args, "--branch", r.Branch)
		}
		args = append(args, "--", r.URL, dest)
		if err := a.mkdirAll(filepath.Dir(real), 0o755); err != nil {
			rs.Status, rs.Error = "failed", err.Error()
			a.setRepo(rs)
			failed[i] = true
			continue
		}
		// Retried: a clone at boot can start before the network is up
		// ("Network is unreachable"), and a LAN git server can blip.
		var out bytes.Buffer
		var err error
		for try, wait := 0, 2*time.Second; ; try, wait = try+1, wait*2 {
			out.Reset()
			if dest != real {
				_ = os.RemoveAll(dest)
			}
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = a.home
			cmd.Env = append(a.commandEnv(nil), "GIT_TERMINAL_PROMPT=0")
			cmd.SysProcAttr = a.sysProcAttr()
			cmd.Stdout, cmd.Stderr = &out, &out
			if err = a.spawn(cmd, "setup"); err == nil { // a workload: user.slice, OOM-killable
				err = cmd.Wait()
			}
			if err == nil || try >= 5 || ctx.Err() != nil {
				break
			}
			a.log.Printf("repo %s: clone failed (%s), retrying in %s", virt, strings.TrimSpace(lastLines(out.String(), 1)), wait)
			if !sleepFor(ctx, wait) {
				break
			}
		}
		if err == nil && dest != real {
			err = mergeInto(dest, real)
			_ = os.RemoveAll(dest)
			if err != nil {
				out.WriteString("merging the clone into " + virt + ": " + err.Error())
			}
		}
		if err != nil {
			_ = os.RemoveAll(real + ".crowdvm-clone")
			rs.Status, rs.Error = "failed", "git clone: "+strings.TrimSpace(lastLines(out.String(), 5))
			a.setRepo(rs)
			failed[i] = true
			continue
		}
		if r.Token != "" {
			c := exec.Command("git", "-C", real, "config", "credential.helper", helper)
			c.Env, c.SysProcAttr = a.commandEnv(nil), a.sysProcAttr()
			_ = c.Run()
		}
		if strings.TrimSpace(r.SetupScript) != "" {
			rs.Status = "pending" // cloned; its setup script runs next
			a.setRepo(rs)
		}
	}
	cloned()
	for i, r := range repos {
		if failed[i] {
			continue
		}
		virt := repoVirtPath(r)
		real := a.real(virt)
		rs := RepoStatus{URL: r.URL, Path: virt}
		if strings.TrimSpace(r.SetupScript) == "" {
			rs.Status = "done"
			a.setRepo(rs)
			continue
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			rs.Status, rs.Error = "failed", "repository folder is missing: "+virt
			a.setRepo(rs)
			continue
		}
		rs.Status = "setup"
		a.setRepo(rs)
		logDir := filepath.Join(a.tmp, "routine-logs")
		_ = a.mkdirAll(logDir, 0o755)
		base := filepath.Join(logDir, filepath.Base(real))
		code, err := a.runLogged(ctx, "set -e\n"+r.SetupScript, real, base+".log", base+".err")
		switch {
		case err != nil:
			rs.Status, rs.Error = "failed", err.Error()
		case code != 0:
			tail, _ := readTail(base+".err", 1024)
			rs.Status, rs.Error = "failed", fmt.Sprintf("exit code %d: %s (logs in %s)", code, strings.TrimSpace(string(tail)), a.toVirt(base+".log"))
		default:
			rs.Status, rs.Error = "done", ""
			os.Remove(base + ".log")
			os.Remove(base + ".err")
		}
		a.setRepo(rs)
	}
}

// mergeInto moves the tree src into dst. Files dst already has are kept
// (src's copy is dropped); folders are merged. src's owner and modes stay.
func mergeInto(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if _, err := os.Lstat(target); err == nil {
			return nil // already there: keep it (a folder is walked into)
		}
		if err := os.Rename(p, target); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir // moved as a whole
		}
		return nil
	})
}
