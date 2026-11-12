// Package node is the crowdvm-node daemon: it registers with a CrowdVM server,
// keeps one yamux session to it (node dials a websocket), serves the node API
// on that session (VM lifecycle, guest proxy, tunnels, image/snapshot fetch),
// enforces the owner's capacity limits, forwards events, uploads/downloads
// disk snapshots and serves a local status UI.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/crowdvm/crowdvm/internal/compat"
	"github.com/crowdvm/crowdvm/internal/mux"
	"github.com/crowdvm/crowdvm/internal/node/bench"
	"github.com/crowdvm/crowdvm/internal/node/vm"
	"github.com/crowdvm/crowdvm/internal/proto"
)

const (
	serverBase              = "http://server"  // host is ignored on the mux transport
	defaultSnapshotInterval = 60 * time.Second // Boat: "saved each minute"
	maxQueuedEvents         = 10000
	// bugs-r11-opus 5: one guest queues at most this many events, all guests
	// together leave room for the node's own (lifecycle) events.
	maxGuestEventsPerSandbox = 500
	maxQueuedGuestEvents     = maxQueuedEvents - 2000
)

var (
	HeartbeatInterval = 10 * time.Second
	ReconnectMin      = 1 * time.Second
	ReconnectMax      = 30 * time.Second
	EventFlushDelay   = 300 * time.Millisecond
	errNotConnected   = errors.New("not connected to the server")
)

// Node is the daemon state.
type Node struct {
	cfg   *Config
	cfgMu sync.Mutex
	drv   vm.Driver
	log   *slog.Logger

	ctx context.Context // daemon lifetime (set by Run)

	mu        sync.Mutex
	vms       map[string]*vmRec
	bench     proto.Benchmark
	infoDirty bool
	sess      *yamux.Session
	client    *http.Client
	connected chan struct{} // closed while connected
	connState connState
	transfers map[string]*proto.TransferProgress
	fetches   map[string]*fetchCall
	uploads   map[string]*fetchCall
	// uploading: snapshots taken here whose upload has not ended yet
	// (id -> sandbox), reported in GET /v1/snapshots so the server does not
	// erase or forget them mid-upload.
	uploading map[string]string

	events   []proto.Event
	eventsCh chan struct{}
	guestQ   map[string]int // queued EvGuest events per sandbox (bugs-r11-opus 5)
	guestN   int            // and in total

	snapEvery atomic.Int64 // ns; <0 disabled
	drained   atomic.Bool  // the server drained this node
	benching  atomic.Bool

	guestHTTP *http.Transport

	insp inspectors // snapshot inspector VMs (inspect.go)

	warm      map[string]*warmVM // warm pool VMs (warm.go), guarded by mu
	warmShape *proto.VMSpec

	lazyMu sync.Mutex // blob proxy for lazy chains (lazy.go)
	lazy   *lazyProxy

	gpus []proto.PassthroughGPU // validated passthrough GPUs (gpu.go), guarded by mu

	diskMu    sync.Mutex // Disk() cache (disk.go)
	disk      DiskState
	diskAt    time.Time
	lastCap   proto.Capacity // capacity last sent to the server
	lastPause PauseState     // effective pause last sent to the server

	// From the heartbeat response (guarded by mu): what this node earned,
	// and a running evacuation.
	earned, earned24h float64
	evacuation        *proto.Evacuation
	update            *proto.NodeUpdate // from the server: is this build current
	serverGen         int
	autoUpdateAt      time.Time // last auto-update attempt

	// Local control (ui.go): settings saved but only applied on restart,
	// and how to restart the daemon (nil: nothing would bring it back).
	restartKeys map[string]bool
	restarter   func()
}

type connState struct {
	Connected     bool      `json:"connected"`
	Since         time.Time `json:"since,omitzero"`
	LastHeartbeat time.Time `json:"lastHeartbeat,omitzero"`
	LastError     string    `json:"lastError,omitempty"`
	Attempts      int       `json:"attempts"`
}

// New creates a node from its config and VM driver. The benchmark is loaded
// from <data dir>/benchmark.json if present.
func New(cfg *Config, drv vm.Driver, log *slog.Logger) *Node {
	if log == nil {
		log = slog.Default()
	}
	n := &Node{
		cfg: cfg, drv: drv, log: log,
		ctx:       context.Background(),
		vms:       map[string]*vmRec{},
		warm:      map[string]*warmVM{},
		connected: make(chan struct{}),
		transfers: map[string]*proto.TransferProgress{},
		fetches:   map[string]*fetchCall{},
		uploads:   map[string]*fetchCall{},
		uploading: map[string]string{},
		eventsCh:  make(chan struct{}, 1),
		guestHTTP: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 16, IdleConnTimeout: 60 * time.Second, DisableCompression: true},
	}
	n.snapEvery.Store(int64(defaultSnapshotInterval))
	if nt, ok := drv.(vm.Notifier); ok {
		// Push-based state updates (the watcher poll stays as a fallback).
		ch := make(chan proto.VMStatus, 1024)
		nt.SetNotify(func(st proto.VMStatus) {
			select {
			case ch <- st:
			default:
			}
		})
		go func() {
			for st := range ch {
				n.onDriverStatus(st)
			}
		}()
	}
	if rt, ok := drv.(vm.Reticketer); ok {
		rt.SetReticket(n.reticket)
	}
	if b, err := LoadBenchmark(cfg.DataDir); err == nil {
		n.bench = b
	}
	return n
}

// guestChannel: this node can boot channel guests. Not the process driver
// (its "guests" are host processes, tests only), and not QEMU's slirp
// network: the guest dials the server through the netstack's gateway alias,
// and the ticket is served on the VM's netstack.
func (n *Node) guestChannel() bool {
	if n.drv.Name() == "process" {
		return false
	}
	if nm, ok := n.drv.(interface{ NetMode() string }); ok && nm.NetMode() != "netstack" {
		return false
	}
	return true
}

// reticket asks the server for a new boot ticket for a channel guest that
// rebooted. The server answers 409 until the old boot's channel has closed.
func (n *Node) reticket(ctx context.Context, id string) (*proto.GuestChannel, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		resp, err := n.serverDo(ctx, "POST", "/node/v1/sandboxes/"+id+"/reboot", nil, nil)
		if err == nil {
			var ch proto.GuestChannel
			err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&ch)
			resp.Body.Close()
			if err == nil && ch.Ticket != "" {
				return &ch, nil
			}
			return nil, fmt.Errorf("server: bad ticket answer: %v", err)
		}
		// 409: the old boot's channel is still up. Network errors and 5xx:
		// the server restarts or is busy. Both are retried in the budget.
		var he *httpError
		if errors.As(err, &he) && he.Status != 409 && he.Status < 500 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the previous boot's channel did not close: %w", err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// LoadBenchmark reads <dataDir>/benchmark.json.
func LoadBenchmark(dataDir string) (proto.Benchmark, error) {
	var b proto.Benchmark
	data, err := os.ReadFile(filepath.Join(dataDir, "benchmark.json"))
	if err != nil {
		return b, err
	}
	return b, json.Unmarshal(data, &b)
}

// SaveBenchmark writes <dataDir>/benchmark.json.
func SaveBenchmark(dataDir string, b proto.Benchmark) error {
	data, _ := json.MarshalIndent(b, "", "  ")
	return os.WriteFile(filepath.Join(dataDir, "benchmark.json"), data, 0o644)
}

func (n *Node) config() Config {
	n.cfgMu.Lock()
	defer n.cfgMu.Unlock()
	return *n.cfg
}

// Info returns the NodeInfo advertised to the server.
func (n *Node) Info() proto.NodeInfo {
	c := n.config()
	n.mu.Lock()
	b := n.bench
	n.mu.Unlock()
	imgs := n.drv.Store().Images()
	if imgs == nil {
		imgs = []string{}
	}
	cp, _ := n.capacity()
	lz, ok := n.drv.(vm.LazyStarter)
	n.mu.Lock()
	gpus := n.gpusLocked()
	n.mu.Unlock()
	vg, _ := n.drv.(interface{ VirGL() bool })
	ps := c.EffectivePause(time.Now())
	return proto.NodeInfo{
		NodeID: c.NodeID, Name: c.NodeName(), Version: Version, Gen: compat.NodeGen,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Driver: n.drv.Name(),
		Labels: c.Labels, Bench: b, Capacity: cp,
		DirectHost: c.DirectHost, Images: imgs,
		LazyChain: ok && lz.LazyChain() && !c.NoLazyChain,
		// Secure layer: guests fetch their config from the server over a
		// pinned channel; this node never sees their secrets. Not for the
		// process driver: its "guests" are host processes (tests only).
		GuestChannel: n.guestChannel(),
		GPUs:         gpus, VirGL: vg != nil && vg.VirGL(),
		Paused: ps.Paused, PauseReason: ps.Reason,
	}
}

// Run connects to the server (reconnecting with backoff) and runs the
// background loops until ctx is done.
func (n *Node) Run(ctx context.Context) error {
	c := n.config()
	if c.Server == "" || c.NodeToken == "" {
		return ErrNotJoined
	}
	n.ctx = ctx
	release, err := LockDataDir(c.DataDir) // before adopting VMs (bugs-r12-opus 3)
	if err != nil {
		return err
	}
	defer release()
	if err := n.initGPUs(realGPUHost()); err != nil {
		return err // the owner asked for passthrough that cannot work: say so, do not start
	}
	n.adoptVMs()
	removePartFiles(n.cfg.DataDir) // a node killed mid-download leaves these behind
	go n.flushEventsLoop(ctx)
	go n.snapshotLoop(ctx)
	go n.warmLoop(ctx)
	go n.keepAwakeLoop(ctx)
	if n.bench.RanAt.IsZero() || n.bench.Network.Via == "" {
		// First run (or joined without a network measurement): benchmark once
		// connected so the network test can run.
		go func() {
			wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, _ = n.waitConnected(wctx)
			cancel()
			if ctx.Err() == nil {
				_, _ = n.RunBenchmark(ctx, false)
			}
		}()
	}
	n.connectLoop(ctx)
	return nil
}

// adoptVMs registers VMs the driver already knows (e.g. after a node restart
// with a driver that persists VMs).
func (n *Node) adoptVMs() {
	if _, err := os.Stat(filepath.Join(n.cfg.DataDir, "lazy.json")); err == nil {
		if _, err := n.lazyURLs(); err != nil { // adopted lazy VMs read through it
			n.log.Warn("lazy chain: blob proxy", "err", err)
		}
	}
	specs := loadSpecs(n.cfg.DataDir)
	for _, st := range n.drv.List() {
		if strings.HasPrefix(st.SandboxID, inspectPrefix) || strings.HasPrefix(st.SandboxID, warmPrefix) { // leftover inspector / pool VM
			_ = n.drv.Destroy(context.Background(), st.SandboxID)
			continue
		}
		n.mu.Lock()
		if _, ok := n.vms[st.SandboxID]; ok {
			n.mu.Unlock()
			continue
		}
		r := &vmRec{id: st.SandboxID, status: st, spec: specs[st.SandboxID], lastSnap: time.Now(), gate: gateOpen} // no clone gate for a VM that already ran
		n.vms[st.SandboxID] = r
		live := st.State == proto.VMRunning || st.State == proto.VMStarting
		if live {
			n.startWatch(r)
		}
		n.mu.Unlock()
		if live {
			n.openDirect(r)
			go n.resumeLazy(r)
		}
		n.log.Info("adopted vm", "sandbox", st.SandboxID, "state", st.State)
	}
}

// VM specs (incl. guest tokens) are persisted so VMs that survive a node
// restart can still be proxied: <data dir>/vms.json, mode 0600.
func specsPath(dataDir string) string { return filepath.Join(dataDir, "vms.json") }

func loadSpecs(dataDir string) map[string]proto.VMSpec {
	m := map[string]proto.VMSpec{}
	if b, err := os.ReadFile(specsPath(dataDir)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// saveSpecs (n.mu held) persists the specs of all known VMs.
func (n *Node) saveSpecsLocked() {
	m := make(map[string]proto.VMSpec, len(n.vms))
	for id, r := range n.vms {
		if r.spec.SandboxID != "" {
			sp := r.spec
			sp.Guest = sp.Guest.WithoutTicket() // the boot ticket never goes to disk
			m[id] = sp
		}
	}
	b, _ := json.Marshal(m)
	tmp := specsPath(n.cfg.DataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, specsPath(n.cfg.DataDir))
	}
}

func connectURL(server string) string {
	u := strings.TrimRight(server, "/")
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	case !strings.Contains(u, "://"):
		u = "ws://" + u
	}
	return u + "/node/v1/connect"
}

func (n *Node) connectLoop(ctx context.Context) {
	backoff := ReconnectMin
	for ctx.Err() == nil {
		c := n.config()
		sess, err := n.dial(ctx)
		n.mu.Lock()
		n.connState.Attempts++
		n.mu.Unlock()
		if err != nil {
			if u := refusedUpdate(err); u != nil { // too old for this server
				n.mu.Lock()
				n.update = u
				n.mu.Unlock()
				n.maybeAutoUpdate(u)
				err = errors.New(u.Message)
				backoff = ReconnectMax
			}
			n.setConnErr(err)
			n.log.Warn("connect failed", "server", c.Server, "err", err, "retry", backoff)
			if !sleepCtx(ctx, jitter(backoff)) {
				return
			}
			backoff = min(backoff*2, ReconnectMax)
			continue
		}
		start := time.Now()
		n.log.Info("connected to server", "server", c.Server)
		n.runSession(ctx, sess)
		n.log.Warn("disconnected from server", "after", time.Since(start).Round(time.Second))
		if time.Since(start) > 30*time.Second {
			backoff = ReconnectMin
		}
		if !sleepCtx(ctx, jitter(backoff)) {
			return
		}
		backoff = min(backoff*2, ReconnectMax)
	}
}

func (n *Node) dial(ctx context.Context) (*yamux.Session, error) {
	c := n.config()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.NodeToken)
	hdr.Set("X-Crowdvm-Node-Version", Version)
	hdr.Set(compat.Header, strconv.Itoa(compat.NodeGen))
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return mux.Dial(dctx, connectURL(c.Server), hdr)
}

// refusedUpdate turns the server's 426 node_outdated answer to the
// connection into an incompatible NodeUpdate (nil for other errors).
func refusedUpdate(err error) *proto.NodeUpdate {
	var re *mux.RefusedError
	if !errors.As(err, &re) || re.Status != http.StatusUpgradeRequired {
		return nil
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Details struct {
				Latest  string `json:"latest"`
				Command string `json:"command"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(re.Body, &body)
	return &proto.NodeUpdate{State: compat.StateIncompatible, Version: Version, Latest: body.Error.Details.Latest,
		Command: firstNonEmpty(body.Error.Details.Command, "crowdvm-node update"),
		Message: firstNonEmpty(body.Error.Message, "This crowdvm-node is too old for its server. Run `crowdvm-node update`.")}
}

// ErrNotJoined is returned when the config has no server/node token.
var ErrNotJoined = errors.New("node is not joined: run `crowdvm-node join --server URL --token T` first")

// WithSession connects once (no reconnect), serves the node API without its
// machines (oneShotHandler), runs fn while connected and disconnects. For CLI
// commands that need the server while the daemon is not running.
func (n *Node) WithSession(ctx context.Context, fn func(context.Context) error) error {
	c := n.config()
	if c.Server == "" || c.NodeToken == "" {
		return ErrNotJoined
	}
	n.ctx = ctx
	sess, err := n.dial(ctx)
	if err != nil {
		return fmt.Errorf("connect %s: %w", c.Server, err)
	}
	defer sess.Close()
	n.mu.Lock()
	n.sess, n.client = sess, mux.Client(sess)
	n.connState.Connected = true
	close(n.connected)
	n.mu.Unlock()
	go mux.Serve(sess, oneShotHandler(n.Handler()))
	// Heartbeats keep the session past the server's node timeout: an image
	// push or pull of GiBs over a slow link or a busy disk outlasts it.
	hctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); n.heartbeatLoop(hctx) }()
	defer func() { stop(); <-done }()
	return fn(ctx)
}

func (n *Node) setConnErr(err error) {
	n.mu.Lock()
	n.connState.LastError = err.Error()
	n.mu.Unlock()
}

func (n *Node) runSession(ctx context.Context, sess *yamux.Session) {
	n.mu.Lock()
	n.sess = sess
	n.client = mux.Client(sess)
	n.connState.Connected = true
	n.connState.Since = time.Now()
	n.connState.LastError = ""
	close(n.connected)
	n.mu.Unlock()

	sctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-sctx.Done():
			sess.Close()
		case <-sess.CloseChan():
		}
	}()
	go func() {
		if err := mux.Serve(sess, n.Handler()); err != nil {
			n.log.Debug("serve ended", "err", err)
		}
		cancel()
	}()
	n.kickEvents()
	n.heartbeatLoop(sctx)
	cancel()
	sess.Close()

	n.mu.Lock()
	n.sess, n.client = nil, nil
	n.connState.Connected = false
	n.connected = make(chan struct{})
	n.mu.Unlock()
}

func (n *Node) heartbeatLoop(ctx context.Context) {
	first := true
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		if err := n.heartbeat(ctx, first); err != nil {
			if ctx.Err() != nil {
				return
			}
			n.log.Warn("heartbeat failed", "err", err)
		} else {
			first = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Heartbeat payload for the current state.
func (n *Node) heartbeatPayload(withInfo bool) proto.Heartbeat {
	hb := proto.Heartbeat{Usage: n.Usage(), VMs: n.Statuses()}
	cp, _ := n.capacity()
	ps := n.config().EffectivePause(time.Now())
	ps.Until = time.Time{}
	n.mu.Lock()
	// e.g. the disk filled up, or the schedule crossed a boundary
	withInfo = withInfo || n.infoDirty || cp != n.lastCap || ps != n.lastPause
	n.infoDirty = false
	n.mu.Unlock()
	if withInfo {
		info := n.Info()
		hb.Info = &info
		n.mu.Lock()
		n.lastCap = info.Capacity
		n.lastPause = PauseState{Paused: info.Paused, Reason: info.PauseReason}
		n.mu.Unlock()
	}
	return hb
}

func (n *Node) heartbeat(ctx context.Context, withInfo bool) error {
	hb := n.heartbeatPayload(withInfo)
	resp, err := n.serverDo(ctx, "POST", "/node/v1/heartbeat", jsonBody(hb), nil)
	if err != nil {
		if hb.Info != nil {
			n.markInfoDirty()
		}
		return err
	}
	defer resp.Body.Close()
	var hr proto.HeartbeatResponse
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&hr) == nil {
		n.applyServerSettings(hr)
	}
	n.mu.Lock()
	n.connState.LastHeartbeat = time.Now()
	n.mu.Unlock()
	return nil
}

func (n *Node) applyServerSettings(hr proto.HeartbeatResponse) {
	n.drained.Store(hr.Drained)
	n.mu.Lock()
	n.earned, n.earned24h, n.evacuation = hr.EarnedCredits, hr.Earned24h, hr.Evacuation
	if hr.GuestChannel {
		n.clearWarmShapeLocked() // bugs-r15-opus 5: a saved shape from before would boot a VM no create takes
	}
	n.update, n.serverGen = hr.Update, hr.ServerGen
	defer n.maybeAutoUpdate(hr.Update) // after the unlock below
	n.mu.Unlock()
	switch {
	case hr.SnapshotIntervalSec > 0:
		n.snapEvery.Store(int64(time.Duration(hr.SnapshotIntervalSec) * time.Second))
	case hr.SnapshotIntervalSec < 0:
		n.snapEvery.Store(-1)
	}
}

// snapshotInterval: local override, else server value, else 60 s. <=0 = off.
func (n *Node) snapshotInterval() time.Duration {
	c := n.config()
	if d, ok := c.SnapshotEvery(); ok {
		return d
	}
	return time.Duration(n.snapEvery.Load())
}

func (n *Node) markInfoDirty() {
	n.mu.Lock()
	n.infoDirty = true
	n.mu.Unlock()
}

// serverClient returns the mux HTTP client while connected.
func (n *Node) serverClient() (*http.Client, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.client == nil {
		return nil, errNotConnected
	}
	return n.client, nil
}

// waitConnected blocks until connected (or ctx done).
func (n *Node) waitConnected(ctx context.Context) (*http.Client, error) {
	for {
		n.mu.Lock()
		c, ch := n.client, n.connected
		n.mu.Unlock()
		if c != nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

// serverDo performs a request to the server over the session. Non-2xx
// responses are returned as errors (*httpError).
func (n *Node) serverDo(ctx context.Context, method, path string, body io.Reader, hdr http.Header) (*http.Response, error) {
	c, err := n.serverClient()
	if err != nil {
		return nil, err
	}
	return doReq(ctx, c, method, serverBase+path, body, hdr)
}

type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("server: %d %s", e.Status, e.Body) }

func isStatus(err error, code int) bool {
	var he *httpError
	return errors.As(err, &he) && he.Status == code
}

func doReq(ctx context.Context, c *http.Client, method, url string, body io.Reader, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &httpError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return resp, nil
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func (n *Node) emit(sandboxID, typ string, data any) {
	raw, ok := data.(json.RawMessage)
	if !ok && data != nil {
		raw, _ = json.Marshal(data)
	}
	n.emitEvent(proto.Event{Time: time.Now().UTC(), SandboxID: sandboxID, Type: typ, Data: raw})
}

func (n *Node) emitEvent(ev proto.Event) {
	n.mu.Lock()
	if ev.Type == proto.EvGuest {
		// A guest event past its sandbox's share (or past the room guests
		// share) is dropped; it never pushes another event out. The
		// guest's state is re-read later where it matters (bugs-r11-opus 5).
		if n.guestQ[ev.SandboxID] >= maxGuestEventsPerSandbox || n.guestN >= maxQueuedGuestEvents || len(n.events) >= maxQueuedEvents {
			n.mu.Unlock()
			return
		}
		if n.guestQ == nil {
			n.guestQ = map[string]int{}
		}
		n.guestQ[ev.SandboxID]++
		n.guestN++
	} else if len(n.events) >= maxQueuedEvents {
		n.dropFront(len(n.events) - maxQueuedEvents + 1)
	}
	n.events = append(n.events, ev)
	n.mu.Unlock()
	n.kickEvents()
}

// dropFront removes the k oldest queued events (n.mu held).
func (n *Node) dropFront(k int) {
	for _, ev := range n.events[:k] {
		if ev.Type != proto.EvGuest {
			continue
		}
		n.guestN--
		if n.guestQ[ev.SandboxID]--; n.guestQ[ev.SandboxID] <= 0 {
			delete(n.guestQ, ev.SandboxID)
		}
	}
	n.events = n.events[k:]
}

func (n *Node) kickEvents() {
	select {
	case n.eventsCh <- struct{}{}:
	default:
	}
}

func (n *Node) flushEventsLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.eventsCh:
		}
		if !sleepCtx(ctx, EventFlushDelay) { // batch bursts
			return
		}
		for {
			n.mu.Lock()
			batch := n.events[:min(len(n.events), 500)]
			n.mu.Unlock()
			if len(batch) == 0 {
				break
			}
			if _, err := n.waitConnected(ctx); err != nil {
				return
			}
			resp, err := n.serverDo(ctx, "POST", "/node/v1/events", jsonBody(batch), nil)
			if err != nil {
				n.log.Debug("event flush failed", "err", err)
				if !sleepCtx(ctx, 2*time.Second) {
					return
				}
				continue
			}
			resp.Body.Close()
			n.mu.Lock()
			// Drop what was sent (the queue may have been trimmed meanwhile).
			if len(n.events) >= len(batch) && &n.events[0] == &batch[0] {
				n.dropFront(len(batch))
			} else {
				n.dropFront(len(n.events) - len(dropSent(n.events, batch)))
			}
			n.mu.Unlock()
		}
	}
}

func dropSent(q, sent []proto.Event) []proto.Event {
	last := sent[len(sent)-1]
	for i := range q {
		if q[i].Time.Equal(last.Time) && q[i].Type == last.Type && q[i].SandboxID == last.SandboxID {
			return q[i+1:]
		}
	}
	return q
}

// ---------------------------------------------------------------------------
// Benchmark
// ---------------------------------------------------------------------------

// RunBenchmark runs the benchmark (network over the session when connected),
// stores it and flags the info for the next heartbeat.
func (n *Node) RunBenchmark(ctx context.Context, quick bool) (proto.Benchmark, error) {
	if !n.benching.CompareAndSwap(false, true) {
		return proto.Benchmark{}, errors.New("benchmark already running")
	}
	defer n.benching.Store(false)
	c := n.config()
	o := bench.Options{Quick: quick, Dir: c.DataDir, HostHint: hostOf(c.Server),
		Log: func(f string, a ...any) { n.log.Info("bench: " + fmt.Sprintf(f, a...)) }}
	if cl, err := n.serverClient(); err == nil {
		o.Client, o.ServerURL = cl, serverBase
	}
	b := bench.Run(ctx, o)
	n.mu.Lock()
	n.bench = b
	n.infoDirty = true
	n.mu.Unlock()
	if err := SaveBenchmark(c.DataDir, b); err != nil {
		n.log.Warn("save benchmark", "err", err)
	}
	return b, nil
}

func hostOf(server string) string {
	s := server
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s, _, _ = strings.Cut(s, "/")
	return s
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
