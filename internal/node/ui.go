package node

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/node/bench"
	"github.com/crowdvm/crowdvm/internal/proto"
)

//go:embed ui.html
var uiHTML []byte

// UIHeader must be present on state-changing local API calls: browsers can't
// send it cross-origin without a CORS preflight (which we never answer).
const UIHeader = "X-Crowdvm-Ui"

// Local control: state-changing calls also need the token in <data
// dir>/ui-token as a Bearer header (the CLI: every call), or the browser
// session cookie (the page: its own settings only). `crowdvm-node open`
// asks for a single-use login code (POST /api/login-code, Bearer) and
// opens /login?c=<code>, so the token never reaches a URL, the browser's
// history or a process list. The cookie holds a value derived from the
// token, not the token: browsers send localhost cookies to every port.
const (
	uiTokenFile  = "ui-token"
	uiCookieName = "crowdvm_node_ui"
	loginCodeTTL = time.Minute
)

// uiAccess is what a request may do.
type uiAccess int

const (
	accessNone    uiAccess = iota
	accessBrowser          // session cookie: pause, page settings, restart, bench
	accessFull             // Bearer token: everything
)

// sessionValue is the cookie for token: stable across restarts (a restart
// from the page keeps the page logged in), rotated with the token.
func sessionValue(token string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("crowdvm-node-ui-session-v1"))
	return hex.EncodeToString(m.Sum(nil))
}

// liveKeys apply without a restart: the capacity only limits new
// placements, so lowering it never touches running sandboxes.
var liveKeys = []string{"cpus", "memory", "disk", "max-sandboxes", "disk-overcommit", "disk-reserve",
	"paused", "schedule", "schedule-hold"}

// Status is the local status document (GET /api/status).
type Status struct {
	NodeID              string                   `json:"nodeId"`
	Name                string                   `json:"name"`
	Version             string                   `json:"version"`
	Server              string                   `json:"server"`
	Driver              string                   `json:"driver"`
	OS                  string                   `json:"os"`
	Arch                string                   `json:"arch"`
	DataDir             string                   `json:"dataDir"`
	DirectHost          string                   `json:"directHost,omitempty"`
	Connection          connState                `json:"connection"`
	Paused              bool                     `json:"paused"` // the owner's master switch
	Pause               PauseState               `json:"pause"`  // effective: switch, hold or schedule
	Schedule            string                   `json:"schedule,omitempty"`
	ScheduleHold        *ScheduleHold            `json:"scheduleHold,omitempty"`
	Drained             bool                     `json:"drained"` // by the owner or an admin, on the server
	Evacuation          *proto.Evacuation        `json:"evacuation,omitempty"`
	EarnedCredits       float64                  `json:"earnedCredits"`
	Earned24h           float64                  `json:"earned24h"`
	Benchmark           proto.Benchmark          `json:"benchmark"`
	BenchmarkRunning    bool                     `json:"benchmarkRunning"`
	Capacity            proto.Capacity           `json:"capacity"`
	Usage               proto.Usage              `json:"usage"`
	VMs                 []VMView                 `json:"vms"`
	Transfers           []proto.TransferProgress `json:"transfers"`
	Images              []string                 `json:"images"`
	SnapshotIntervalSec int                      `json:"snapshotIntervalSec"`
	Disk                DiskState                `json:"disk"`
	// Local control (filled per request by the UI handler).
	Authorized     bool              `json:"authorized"`
	CanRestart     bool              `json:"canRestart"`
	RestartPending []string          `json:"restartPending,omitempty"`
	Settings       map[string]string `json:"settings,omitempty"` // saved values of the page's keys
	Host           HostInfo          `json:"host"`
}

// HostInfo bounds the shared-resources form.
type HostInfo struct {
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memoryMiB"`
}

type VMView struct {
	proto.VMStatus
	Size proto.Size `json:"size"`
}

func (n *Node) Status() Status {
	c := n.config()
	info := n.Info()
	n.mu.Lock()
	cs := n.connState
	vms := make([]VMView, 0, len(n.vms))
	for _, r := range n.vms {
		vms = append(vms, VMView{VMStatus: r.status, Size: r.spec.Size})
	}
	earned, earned24h, evac := n.earned, n.earned24h, n.evacuation
	var pending []string
	for k := range n.restartKeys {
		pending = append(pending, k)
	}
	canRestart := n.restarter != nil
	n.mu.Unlock()
	sort.Slice(vms, func(i, j int) bool { return vms[i].SandboxID < vms[j].SandboxID })
	sort.Strings(pending)
	total, _ := bench.MemInfo()
	st := Status{
		NodeID: c.NodeID, Name: info.Name, Version: Version, Server: c.Server, Driver: info.Driver,
		OS: info.OS, Arch: info.Arch, DataDir: c.DataDir, DirectHost: c.DirectHost,
		Connection: cs, Paused: c.Paused, Pause: c.EffectivePause(time.Now()), Schedule: c.Schedule,
		Drained: n.drained.Load(), Evacuation: evac, EarnedCredits: earned, Earned24h: earned24h,
		Benchmark: info.Bench, BenchmarkRunning: n.benching.Load(),
		Capacity: info.Capacity, Usage: n.Usage(), VMs: vms, Transfers: n.Transfers(), Images: info.Images,
		SnapshotIntervalSec: int(max(n.snapshotInterval(), 0) / time.Second), Disk: n.Disk(),
		CanRestart: canRestart, RestartPending: pending,
		Host: HostInfo{CPUs: runtime.NumCPU(), MemoryMiB: int(total >> 20)},
	}
	if h := c.ScheduleHold; h != nil && time.Now().Before(h.Until) {
		st.ScheduleHold = h
	}
	return st
}

// SetConfig changes one setting while the daemon runs, so config.json has a
// single writer: it re-reads the file and changes only that key (the CLI or
// an editor may have changed others), and applies live keys at once. Other
// keys are saved and listed as needing a restart. applied reports which.
func (n *Node) SetConfig(key, val string) (applied bool, err error) {
	n.cfgMu.Lock()
	disk, err := LoadConfig(n.cfg.DataDir)
	if err == nil {
		err = disk.Set(key, val)
	}
	if err == nil {
		err = disk.Save()
	}
	applied = err == nil && slices.Contains(liveKeys, key)
	if applied {
		_ = n.cfg.Set(key, val) // validated above
		if key == "schedule-hold" {
			n.cfg.ScheduleHold = disk.ScheduleHold // the same deadline as saved
		}
	}
	n.cfgMu.Unlock()
	if err != nil {
		return false, err
	}
	n.mu.Lock()
	if !applied {
		if n.restartKeys == nil {
			n.restartKeys = map[string]bool{}
		}
		n.restartKeys[key] = true
	}
	n.mu.Unlock()
	n.markInfoDirty()
	return applied, nil
}

// SetPaused toggles the master switch for new sandboxes (persisted).
func (n *Node) SetPaused(p bool) error {
	_, err := n.SetConfig("paused", strconv.FormatBool(p))
	return err
}

// SetRestarter makes "Restart node" available: fn ends the daemon in a way
// its service manager restarts it. Only set it when something will.
func (n *Node) SetRestarter(fn func()) {
	n.mu.Lock()
	n.restarter = fn
	n.mu.Unlock()
}

// pageKeys are the settings the local page edits.
var pageKeys = []string{"cpus", "memory", "disk", "max-sandboxes", "schedule"}

func (n *Node) settings() map[string]string {
	c, err := LoadConfig(n.config().DataDir)
	if err != nil {
		cc := n.config()
		c = &cc
	}
	out := map[string]string{}
	for _, k := range pageKeys {
		out[k], _ = c.Get(k)
	}
	return out
}

// UIHandler serves the local status page and its small JSON API. token ""
// refuses every state-changing call.
func (n *Node) UIHandler(token string) http.Handler {
	eq := func(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
	access := func(r *http.Request) uiAccess {
		if token == "" {
			return accessNone
		}
		if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && eq(b, token) {
			return accessFull
		}
		if ck, err := r.Cookie(uiCookieName); err == nil && eq(ck.Value, sessionValue(token)) {
			return accessBrowser
		}
		return accessNone
	}
	var codesMu sync.Mutex
	codes := map[string]time.Time{} // single-use login codes -> expiry
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
		_, _ = w.Write(uiHTML)
	})
	m.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("c")
		codesMu.Lock()
		exp, ok := codes[c]
		delete(codes, c) // single use
		codesMu.Unlock()
		if token == "" || c == "" || !ok || time.Now().After(exp) {
			http.Error(w, "This login link is used up or expired. Run `crowdvm-node open` on this machine for a new one.", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: uiCookieName, Value: sessionValue(token), Path: "/", MaxAge: 30 * 24 * 3600,
			HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther) // the code leaves the address bar
	})
	m.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		st := n.Status()
		st.Authorized = access(r) != accessNone
		if st.Authorized {
			st.Settings = n.settings()
		}
		api.WriteJSON(w, 200, st)
	})
	ctl := http.NewServeMux()
	// browserOK: routes the page uses; everything else needs the Bearer token.
	browserOK := map[string]bool{"/api/pause": true, "/api/config": true, "/api/restart": true, "/api/bench": true}
	m.Handle("POST /api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := access(r)
		if a == accessNone || (a == accessBrowser && !browserOK[r.URL.Path]) {
			api.WriteError(w, api.NewError(401, "unauthorized", "Run `crowdvm-node open` on this machine to control the node.", nil))
			return
		}
		ctl.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), uiAccessKey{}, a)))
	}))
	ctl.HandleFunc("POST /api/login-code", func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(uiAccessKey{}) != accessFull {
			api.WriteError(w, api.NewError(401, "unauthorized", "Needs the control token.", nil))
			return
		}
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		code := hex.EncodeToString(b)
		now := time.Now()
		codesMu.Lock()
		for c, exp := range codes {
			if now.After(exp) {
				delete(codes, c)
			}
		}
		codes[code] = now.Add(loginCodeTTL)
		codesMu.Unlock()
		api.WriteJSON(w, 200, map[string]any{"code": code, "expiresInSec": int(loginCodeTTL / time.Second)})
	})
	ctl.HandleFunc("POST /api/pause", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Paused bool `json:"paused"`
		}
		if !decode(w, r, &req) {
			return
		}
		reply(w, map[string]bool{"paused": req.Paused}, n.SetPaused(req.Paused))
	})
	ctl.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if !decode(w, r, &req) {
			return
		}
		if r.Context().Value(uiAccessKey{}) != accessFull && !slices.Contains(liveKeys, req.Key) {
			// The page only changes these; the rest (driver, server, network,
			// binaries) stays with the CLI and its token.
			api.WriteError(w, api.NewError(403, "cli_only", "Change "+req.Key+" with `crowdvm-node config set` on this machine.", nil))
			return
		}
		applied, err := n.SetConfig(req.Key, req.Value)
		if err != nil {
			api.WriteError(w, api.NewError(400, "invalid_setting", err.Error(), nil))
			return
		}
		api.WriteJSON(w, 200, map[string]any{"key": req.Key, "applied": applied, "restartNeeded": !applied})
	})
	ctl.HandleFunc("POST /api/restart", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		fn := n.restarter
		n.mu.Unlock()
		if fn == nil {
			api.WriteError(w, api.NewError(409, "no_service_manager",
				"This node does not run as an installed service, so nothing would start it again. Restart it from a terminal.", nil))
			return
		}
		api.WriteJSON(w, 202, map[string]bool{"ok": true})
		go func() { time.Sleep(300 * time.Millisecond); fn() }() // let the reply go out
	})
	ctl.HandleFunc("POST /api/bench", func(w http.ResponseWriter, r *http.Request) {
		quick := r.URL.Query().Get("quick") != ""
		if r.URL.Query().Get("wait") == "" {
			go n.RunBenchmark(n.ctx, quick)
			api.WriteJSON(w, 202, map[string]bool{"ok": true})
			return
		}
		b, err := n.RunBenchmark(r.Context(), quick)
		reply(w, b, err)
	})
	ctl.HandleFunc("POST /api/images/{id}/push", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]bool{"ok": true}, n.PushImage(r.Context(), r.PathValue("id")))
	})
	ctl.HandleFunc("POST /api/images/{id}/pull", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]bool{"ok": true}, n.PullImage(r.Context(), "", r.PathValue("id")))
	})
	ctl.HandleFunc("POST /api/images/{id}/import", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		if !decode(w, r, &req) {
			return
		}
		err := n.drv.Store().ImportImage(r.PathValue("id"), req.Path)
		if err == nil {
			n.markInfoDirty()
		}
		reply(w, map[string]bool{"ok": true}, err)
	})
	return localOnly(m)
}

type uiAccessKey struct{}

// localOnly rejects DNS-rebinding hosts and cross-site writes.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			host = hh
		}
		if host != "localhost" && net.ParseIP(strings.Trim(host, "[]")) == nil {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != "GET" && r.Header.Get(UIHeader) == "" {
			http.Error(w, "missing "+UIHeader+" header", http.StatusForbidden)
			return
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

// UIToken returns the local control token, creating it (mode 0600) on first
// use. It stays the same across restarts; delete the file to rotate it.
func UIToken(dataDir string) (string, error) {
	p := filepath.Join(dataDir, uiTokenFile)
	if t, err := ReadUIToken(dataDir); err == nil {
		return t, nil
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	t := hex.EncodeToString(b)
	// Written whole then renamed, so a crash never leaves an empty file.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return t, nil
}

// ReadUIToken reads the local control token without creating it.
func ReadUIToken(dataDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, uiTokenFile))
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("empty " + uiTokenFile)
	}
	return t, nil
}

// ServeUI listens on addr until ctx is done.
func (n *Node) ServeUI(ctx context.Context, addr string) error {
	token, err := UIToken(n.config().DataDir)
	if err != nil {
		n.log.Warn("local control token unavailable: the status page is read-only", "err", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: n.UIHandler(token), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	n.log.Info("status UI", "url", "http://"+ln.Addr().String(), "control", "crowdvm-node open")
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
