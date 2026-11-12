// Package guest is the agent that runs inside every CrowdVM VM (crowdvm-guest).
// It serves the sandbox-scoped Boat-compatible endpoints (commands, command
// history, processes, files, artifacts, sshkey), the guest-internal endpoints
// the node uses (health, tunnel, prepare-stop, sync, events, setup status,
// desktop), and runs the per-boot tasks described by proto.GuestConfig.
//
// With Options.Unprivileged it runs on a normal host for tests: root-only boot
// tasks are skipped, commands run as the current user and the virtual paths
// /home/user and /tmp map to directories under Options.Home.
package guest

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/guest/agents"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// Version is set at build time (-ldflags "-X .../internal/guest.Version=...").
var Version = "dev"

const defaultDataDir = "/var/lib/crowdvm"

type Options struct {
	Config proto.GuestConfig
	// Home is the real home directory. Default proto.GuestHome; required with
	// Unprivileged.
	Home string
	// DataDir holds command history and agent state. Default /var/lib/crowdvm,
	// or <Home>/.local/state/crowdvm-guest when unprivileged.
	DataDir      string
	Unprivileged bool
	Logger       *log.Logger
	// CLIBinary: the in-sandbox crowdvm CLI shipped on the config drive
	// (installed at boot when the file exists; root only).
	CLIBinary string
	// Channel: the guest channel the config came from (ConnectChannel); the
	// API is served on it, the local listener serves the ops subset only.
	Channel *Channel
}

type Agent struct {
	cfgMu  sync.RWMutex
	cfg    proto.GuestConfig // immutable fields read directly; use config() for the rest
	unpriv bool
	home   string // real home dir
	tmp    string // real /tmp
	data   string // real data dir
	log    *log.Logger

	uid, gid int
	groups   []uint32
	userName string
	bootID   string
	started  time.Time

	hist    *history
	samples *sampler
	tracer  *tracer
	events  *hub
	setup   *setupState
	desk    *desktop
	wl      workloads
	repoMu  sync.Mutex // serializes repository clone/setup runs

	mu    sync.Mutex
	procs map[int]*proc // detached commands by processId (pid)

	sshReady   chan struct{} // closed once SSH host keys exist (this boot)
	clonesDone chan struct{} // closed once this boot's repo clones ran (LIFE-009)
	clonesOnce sync.Once
	playbook   *playbookRun // this boot's startup playbook recording (LIFE-018), guarded by mu
	cliSource  string
	channel    *Channel
	frz        freezer
	fw         firewallState // direct-port rules (URL-012)
	browser    browserView
	stopCh     chan struct{} // closed when Run ends

	mux     *http.ServeMux
	agentsM *agents.Manager

	// test knobs
	attachMaxDuration time.Duration
	heartbeatEvery    time.Duration
	outputCap         int64 // per command and stream, in bytes
}

// New prepares an agent (directories, history) without running boot tasks or
// listening. Use Run for the full lifecycle, or Handler + Boot in tests.
func New(o Options) (*Agent, error) {
	a := &Agent{cfg: o.Config, unpriv: o.Unprivileged, log: o.Logger, started: time.Now(), cliSource: o.CLIBinary, channel: o.Channel,
		procs: map[int]*proc{}, events: newHub(),
		attachMaxDuration: 10 * time.Minute, heartbeatEvery: 15 * time.Second, outputCap: 1 << 30} // 1 GiB per stream: a disk guard, not a limit on what exec shows (bugs-r7-opus 2)
	if a.log == nil {
		a.log = log.New(os.Stderr, "crowdvm-guest: ", log.LstdFlags)
	}
	a.home = o.Home
	if a.home == "" {
		if o.Unprivileged {
			return nil, errors.New("unprivileged mode needs --home")
		}
		a.home = proto.GuestHome
	}
	var err error
	if a.home, err = filepath.Abs(a.home); err != nil {
		return nil, err
	}
	a.data = o.DataDir
	if a.unpriv {
		if a.data == "" {
			a.data = filepath.Join(a.home, ".local/state/crowdvm-guest")
		}
		a.tmp = filepath.Join(a.data, "tmp")
		a.uid, a.gid = os.Getuid(), os.Getgid()
		if u, err := user.Current(); err == nil {
			a.userName = u.Username
		}
	} else {
		if a.data == "" {
			a.data = defaultDataDir
		}
		a.tmp = "/tmp"
		a.userName = proto.GuestUser
		a.uid, a.gid = 1000, 1000
		if u, err := user.Lookup(proto.GuestUser); err == nil {
			a.uid, _ = strconv.Atoi(u.Uid)
			a.gid, _ = strconv.Atoi(u.Gid)
			if gids, err := u.GroupIds(); err == nil {
				for _, g := range gids {
					if n, err := strconv.Atoi(g); err == nil {
						a.groups = append(a.groups, uint32(n))
					}
				}
			}
		}
	}
	for _, d := range []string{a.home, a.tmp, a.data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	a.bootID = readBootID()
	if !a.inspect() {
		a.loadApplied()
	}
	histDir := filepath.Join(a.data, "history")
	if a.cfg.NoMonitoring || a.cfg.ZeroDataRetention {
		// Monitoring off (DATA-011, CMD-023): nothing is kept on the disk;
		// records recorded before are deleted. Live output of running
		// commands still works from tmpfs.
		_ = os.RemoveAll(histDir)
		histDir = filepath.Join(a.runDir(), "history")
	}
	if a.hist, err = openHistory(histDir, a, a.cfg.NoMonitoring || a.cfg.ZeroDataRetention); err != nil {
		return nil, err
	}
	a.samples = newSampler(a)
	a.tracer = newTracer(a)
	a.sshReady = make(chan struct{})
	a.fw.ready = make(chan struct{})
	a.clonesDone = make(chan struct{})
	a.setup = loadSetupState(filepath.Join(a.data, "setup.json"), a.bootID)
	a.desk = newDesktop(a)
	a.mux = http.NewServeMux()
	a.routes()
	a.agentsM = newAgentsManager(a)
	a.agentsM.Register(a.mux)
	a.recoverProcs()
	return a, nil
}

// config returns a snapshot of the guest config. apply-config replaces Env,
// SecretFiles, Repos, RegistryLogins, AIProxy and SSHAuthorizedKeys at run
// time (slices and maps are replaced wholesale, never mutated in place), so
// read those through config(). SandboxID, Token, Boot, FirstBoot, Hostname,
// ListenAddr and Desktop never change and may be read from a.cfg directly.
func (a *Agent) config() proto.GuestConfig {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

func (a *Agent) updateConfig(f func(c *proto.GuestConfig)) {
	a.cfgMu.Lock()
	f(&a.cfg)
	a.cfgMu.Unlock()
}

// opsCallerKey marks a request of a channel guest's node (the ops subset):
// its answers carry no sandbox content (bugs-r14-opus 6).
type opsCallerKey struct{}

func opsCaller(r *http.Request) bool { v, _ := r.Context().Value(opsCallerKey{}).(bool); return v }

// Handler returns the full HTTP handler (auth applied to everything except
// GET /v1/health). A channel guest's local listener serves only the node's
// ops subset (opsPaths): the rest of the API is on the channel.
func (a *Agent) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Request-Id"); id != "" {
			w.Header().Set("X-Request-Id", id)
		}
		if a.cfg.Channel != nil {
			if !opsPaths[r.Method+" "+r.URL.Path] {
				writeErr(w, http.StatusForbidden, "channel_only", "this sandbox's API is served only on its guest channel to the server")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), opsCallerKey{}, true))
		}
		if r.URL.Path != "/v1/health" && !a.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		a.mux.ServeHTTP(w, r)
	})
}

// channelHandler serves the whole API to the server over the guest channel
// (authenticated by the channel itself).
func (a *Agent) channelHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Request-Id"); id != "" {
			w.Header().Set("X-Request-Id", id)
		}
		a.mux.ServeHTTP(w, r)
	})
}

func (a *Agent) authorized(r *http.Request) bool {
	if a.cfg.Token == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.Token)) == 1
}

// inspect: a throwaway VM that only serves files out of a snapshot
// (BootInspect): no env, secrets, repos, desktop, sampling or tracing.
func (a *Agent) inspect() bool { return a.cfg.Boot == proto.BootInspect }

// runDir is the agent's tmpfs state (gone at the next boot).
func (a *Agent) runDir() string {
	if a.unpriv {
		return filepath.Join(a.data, "run")
	}
	return "/run/crowdvm"
}

// Run runs boot tasks, serves until ctx is done, then shuts down.
func (a *Agent) Run(ctx context.Context) error {
	addr := a.cfg.ListenAddr
	if addr == "" {
		addr = ":" + strconv.Itoa(proto.GuestPort)
	}
	a.stopCh = make(chan struct{})
	defer close(a.stopCh)
	if !a.unpriv && os.Geteuid() == 0 {
		// Keep the agent alive under memory pressure; workloads it starts are
		// reset to 0 (spawn) and are capped by user.slice anyway.
		_ = os.WriteFile("/proc/self/oom_score_adj", []byte("-1000"), 0o644)
	}
	if !a.inspect() {
		a.initWorkloads()
	}
	if a.cfg.Egress == "server" {
		// Before Boot: repo clones and logins need the network, and the
		// node lets this VM reach only the server.
		if err := a.startEgress(ctx); err != nil {
			a.log.Printf("egress through the server: %v (the sandbox has no internet)", err)
		}
	}
	a.Boot(ctx)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	a.log.Printf("listening on %s (boot=%s unprivileged=%v)", ln.Addr(), a.cfg.Boot, a.unpriv)
	if !a.inspect() {
		go a.BootBackground(ctx)
		go func() {
			a.desk.patchOldImage()
			a.desk.watchWallpaper(ctx)
		}()
		go a.samples.loop(a.stopCh)
		go a.historyJanitor()
		if !a.hist.unmonitor {
			go a.tracer.run(a.stopCh)
		}
	}
	chDone := make(chan struct{})
	if a.channel != nil {
		go func() { a.channel.Serve(ctx, a.channelHandler()); close(chDone) }()
	} else {
		close(chDone)
	}
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err = srv.Serve(ln)
	a.desk.shutdown()
	select { // the channel announces a shutdown to the server first
	case <-chDone:
	case <-time.After(4 * time.Second):
	}
	syscall.Sync()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// Paths: the API speaks virtual paths (/home/user/..., /tmp/...). In a VM they
// equal the real ones; unprivileged they map under Options.Home.
// ---------------------------------------------------------------------------

var errInvalidPath = errors.New("path must resolve under /home/user or /tmp")

// virt cleans p (absolute, or relative to /home/user; real paths are accepted
// too) and returns the virtual path.
func (a *Agent) virt(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(p, 0) {
		return "", errInvalidPath
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(proto.GuestHome, p)
	}
	p = filepath.Clean(p)
	if a.unpriv { // accept real paths (e.g. from `pwd` output)
		if r, ok := under(p, a.home); ok {
			return filepath.Join(proto.GuestHome, r), nil
		}
		if r, ok := under(p, a.tmp); ok {
			return filepath.Join("/tmp", r), nil
		}
	}
	if _, ok := under(p, proto.GuestHome); ok {
		return p, nil
	}
	if _, ok := under(p, "/tmp"); ok {
		return p, nil
	}
	return "", errInvalidPath
}

// real maps a virtual path to the real filesystem path.
func (a *Agent) real(v string) string {
	if r, ok := under(v, proto.GuestHome); ok {
		return filepath.Join(a.home, r)
	}
	if r, ok := under(v, "/tmp"); ok {
		return filepath.Join(a.tmp, r)
	}
	return v
}

// toVirt maps a real path back to its virtual form (used in responses).
func (a *Agent) toVirt(real string) string {
	if r, ok := under(real, a.home); ok {
		return filepath.Join(proto.GuestHome, r)
	}
	if r, ok := under(real, a.tmp); ok {
		return filepath.Join("/tmp", r)
	}
	return real
}

func (a *Agent) inRoots(real string) bool {
	_, h := under(real, a.home)
	_, t := under(real, a.tmp)
	if h || t {
		return true
	}
	// roots themselves may be symlinks (e.g. /tmp on some systems)
	for _, root := range []string{a.home, a.tmp} {
		if rr, err := filepath.EvalSymlinks(root); err == nil {
			if _, ok := under(real, rr); ok {
				return true
			}
		}
	}
	return false
}

// resolve validates p and returns (real, virtual). With follow, a symlink in
// the final component must also point inside the roots; the parent directory
// is always canonicalized.
func (a *Agent) resolve(p string, follow bool) (string, string, error) {
	v, err := a.virt(p)
	if err != nil {
		return "", "", err
	}
	r := a.real(v)
	check := filepath.Dir(r)
	if follow || v == proto.GuestHome || v == "/tmp" {
		check = r
	}
	// canonicalize the longest existing prefix
	cur, rest := check, ""
	for {
		ev, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if !a.inRoots(filepath.Join(ev, rest)) {
				return "", "", errInvalidPath
			}
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
	return r, v, nil
}

// under reports whether p is base or inside base, and the relative rest.
func under(p, base string) (string, bool) {
	if p == base {
		return "", true
	}
	if strings.HasPrefix(p, strings.TrimSuffix(base, "/")+"/") {
		return p[len(strings.TrimSuffix(base, "/"))+1:], true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Ownership helpers (no-ops when unprivileged)
// ---------------------------------------------------------------------------

func (a *Agent) chown(p string) {
	if !a.unpriv {
		_ = os.Lchown(p, a.uid, a.gid)
	}
}

// mkdirAll creates missing directories, owned by the sandbox user.
func (a *Agent) mkdirAll(dir string, mode os.FileMode) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return &os.PathError{Op: "mkdir", Path: dir, Err: syscall.ENOTDIR}
		}
		return nil
	}
	if err := a.mkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dir, mode); err != nil && !os.IsExist(err) {
		return err
	}
	a.chown(dir)
	return nil
}

// writeFileAtomic writes data via a temp file + rename, keeps an existing
// file's mode, and chowns to the user when owned is set.
func (a *Agent) writeFileAtomic(p string, data []byte, mode os.FileMode, owned bool) error {
	if fi, err := os.Stat(p); err == nil && mode == 0 {
		mode = fi.Mode().Perm()
	}
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".crowdvm-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil && owned {
		a.chown(tmp)
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	api.WriteError(w, api.NewError(status, code, msg, nil))
}

func writeErrDetails(w http.ResponseWriter, status int, code, msg string, d map[string]any) {
	api.WriteError(w, api.NewError(status, code, msg, d))
}

func ok(w http.ResponseWriter, typ string, fields any) { api.WriteOK(w, http.StatusOK, typ, fields) }

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func qInt(r *http.Request, name string, def int64) (int64, bool) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func qBool(r *http.Request, name string) bool {
	b, _ := strconv.ParseBool(r.URL.Query().Get(name))
	return b
}

func readBootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

func ptr[T any](v T) *T { return &v }
