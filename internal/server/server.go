// Package server is the CrowdVM coordinator: accounts, API keys, orgs,
// credits, the node registry and scheduler, the sandbox lifecycle, snapshots
// and the /api/v1 surface that mirrors Boat's v1 API.
//
// Extension points for further features (environments, webhooks, OTel, AI
// proxy, public URLs, desktop, CI):
//
//   - RegisterFeature: a setup func run by New after the core routes exist.
//   - Server.API / Server.Router: register handlers (with auth + scope check).
//   - store.Register: per-feature schema migrations.
//   - Server.Bus: lifecycle events (sandbox.ready, .error, .archived, ...).
//   - Server.Hooks: environment resolution, guest config, sandbox JSON fields.
//   - Server.NodeForSandbox / NodeConn.Do / ProxyToGuest / DialSandbox:
//     the sandbox -> node relay.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/server/store"
)

// Config holds the server settings (flags of `crowdvm-server serve`).
type Config struct {
	DataDir   string
	PublicURL string // as reachable by users/CLI, e.g. http://192.168.1.10:8420
	// PublicDomain: host-routed public URLs are <label>-<port>.<PublicDomain>
	// ("" = <ip>.nip.io when PublicURL's host is an IP, else localhost).
	PublicDomain string

	OpenSignup bool // anyone may sign up (otherwise the first user, then invite codes)
	OpenNodes  bool // any user may create node join tokens (otherwise admins only)

	// Credits: 1 credit = 1 hour of a "default" machine. Sandboxes spend
	// hours*multiplier; node owners earn NodeEarnRate of that. Zero values
	// mean zero (serve defaults: 10 signup credits, earn rate 1).
	EnforceCredits bool
	SignupCredits  float64
	NodeEarnRate   float64
	// SecondsPerDollar prices the Boat-compatible "dollars" fields.
	SecondsPerDollar int
	// Saved snapshots included per wallet (0 = 10; a wallet policy
	// overrides it) and the daily price of each one above (0 = $0.056).
	SavedSnapshotsIncluded          int
	SavedSnapshotExtraDollarsPerDay float64

	// Start limits per wallet (0 = unlimited) and concurrent sandboxes.
	StartsPerMinute int
	StartsPerHour   int
	StartsPerDay    int
	MaxActive       int
	IPRateLimit     int // requests per 60 s per IP (0 = unlimited)

	// SnapshotInterval is pushed to nodes in heartbeat replies
	// (0 = DefaultSnapshotInterval, <0 off).
	SnapshotInterval time.Duration
	DefaultImage     string        // base image id ("" = newest known)
	StartWait        time.Duration // guest calls wait this long while starting (60 s)
	SessionTTL       time.Duration
	RetryAfter       int // retryAfterSeconds for out_of_capacity

	SMTP SMTPConfig

	// DownloadsDir holds CLI/node binaries served at /downloads/{os}/{arch}/{name}
	// ("" = <DataDir>/downloads).
	DownloadsDir string

	// WebFS is the dashboard, served at / with SPA fallback (nil = none).
	WebFS fs.FS

	Logger *slog.Logger

	// CapacityWait: a create/resume/fork without failFast waits this long
	// for a node with room (15 min). NodeLostGrace: a sandbox on an offline
	// node waits this long before it is rebuilt elsewhere from its latest
	// uploaded snapshot (3 min).
	CapacityWait  time.Duration
	NodeLostGrace time.Duration

	// MinGuestVersion: guest agents older than this (semver, as the node
	// reads it from guest /v1/health) get 409 agent_outdated on calls into
	// the sandbox (CMD-012). "" = MinGuestVersion; "dev" builds always pass.
	MinGuestVersion string
	// OutputCapPerHour: stored command output per wallet and hour, and the
	// same again for other processes (CMD-020; 0 = 10 MiB, <0 = no cap).
	// Output beyond it is not kept and the record says outputTruncated.
	OutputCapPerHour int64

	// WebhookAllowLocal lets webhook and OTel export URLs point at
	// loopback, link-local and cloud metadata addresses (outbound_guard.go;
	// LAN and Tailscale addresses are always allowed).
	WebhookAllowLocal bool

	// Loop intervals (tests shorten them). Zero = the package defaults
	// (OTelMetricsInterval, WebhookBackoff, CIPollInterval, CIRetryInterval).
	OTelMetricsInterval time.Duration
	WebhookBackoff      []time.Duration
	CIPollInterval      time.Duration
	CIRetryInterval     time.Duration
	ReaperInterval      time.Duration
	BillingInterval     time.Duration
	NodeTimeout         time.Duration // no heartbeat for this long = offline
	TrustProxy          bool          // use X-Forwarded-For for client IPs
}

// SMTPConfig enables mailing login codes and invites (optional).
type SMTPConfig struct {
	Addr     string // host:port
	From     string
	Username string
	Password string
}

func (c *Config) defaults() {
	if c.DataDir == "" {
		home, _ := os.UserHomeDir()
		c.DataDir = filepath.Join(home, ".local", "share", "crowdvm", "server")
	}
	if c.PublicURL == "" {
		c.PublicURL = "http://localhost:8420"
	}
	if c.SecondsPerDollar == 0 {
		c.SecondsPerDollar = 100000
	}
	if c.StartWait == 0 {
		c.StartWait = 60 * time.Second
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 30 * 24 * time.Hour
	}
	if c.RetryAfter == 0 {
		c.RetryAfter = 30
	}
	if c.ReaperInterval == 0 {
		c.ReaperInterval = 2 * time.Second
	}
	if c.BillingInterval == 0 {
		c.BillingInterval = 15 * time.Second
	}
	if c.NodeTimeout == 0 {
		c.NodeTimeout = 45 * time.Second
	}
	if c.CapacityWait == 0 {
		c.CapacityWait = 15 * time.Minute
	}
	if c.NodeLostGrace == 0 {
		c.NodeLostGrace = 3 * time.Minute
	}
	if c.OTelMetricsInterval == 0 {
		c.OTelMetricsInterval = OTelMetricsInterval
	}
	if len(c.WebhookBackoff) == 0 {
		c.WebhookBackoff = WebhookBackoff
	}
	if c.CIPollInterval == 0 {
		c.CIPollInterval = CIPollInterval
	}
	if c.CIRetryInterval == 0 {
		c.CIRetryInterval = CIRetryInterval
	}
	if c.MinGuestVersion == "" {
		c.MinGuestVersion = MinGuestVersion
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// DefaultSnapshotInterval is the periodic live snapshot interval nodes get
// when Config.SnapshotInterval is 0 (Boat: every minute).
const DefaultSnapshotInterval = time.Minute

// DefaultSignupCredits are granted to new accounts by `crowdvm-server`.
const DefaultSignupCredits = 10

// DefaultLimits are the start/concurrency limits of `serve` (Boat's $20 plan).
var DefaultLimits = struct{ PerMinute, PerHour, PerDay, Active, IP int }{12, 60, 200, 100, 10000}

// Hooks let features customize the core without editing it.
type Hooks struct {
	// ResolveEnvironment maps a create/resume/fork request to what the guest
	// gets. nil = no environments: only "" / "base" are accepted.
	ResolveEnvironment func(ctx context.Context, req EnvRequest) (*ResolvedEnv, error)
	// PrepareGuest runs before every boot and may add to the guest config.
	PrepareGuest []func(ctx context.Context, sb *Sandbox, g *GuestPrep) error
	// SandboxJSON adds or replaces fields of the public sandbox object.
	SandboxJSON []func(sb *Sandbox, m map[string]any)
	// Mail is called for every outgoing mail (login codes, invites), in
	// addition to SMTP when configured.
	Mail func(to, subject, body string)
	// HostPorts answers create-time "hostedPorts" (public URLs feature).
	HostPorts func(c *Ctx, sb *Sandbox, ports []json.RawMessage) []map[string]any
	// EventTx runs inside the database transaction of a state change, once
	// per lifecycle event it publishes (e.ID is set); an error rolls the
	// change back. Used as the webhook/OTel outbox.
	EventTx []func(tx *sql.Tx, e Event) error
	// Guard runs on every API request after authentication and the scope
	// check (c.R.Pattern names the route); an error refuses the request.
	Guard []func(c *Ctx) error
	// NewSandbox runs on create and fork before the sandbox exists and may
	// change its snapshots/monitoring settings or refuse it.
	NewSandbox []func(c *Ctx, p *SandboxPolicy) error
	// UserJSON adds or replaces fields of the user object (GET /me).
	UserJSON []func(u *User, m map[string]any)
}

// SandboxPolicy is what NewSandbox hooks see and may change.
type SandboxPolicy struct {
	Op                       string // "create" or "fork"
	Owner                    *User
	SnapshotsReq, MonitorReq OptBool // what the request asked for
	Snapshots, Monitoring    bool    // the effective settings (hooks may change them)
}

// Feature is a setup function registered with RegisterFeature.
type Feature func(s *Server) error

var (
	featMu   sync.Mutex
	features []struct {
		name string
		fn   Feature
	}
)

// RegisterFeature adds a setup step that New runs after the core is ready
// (routes, hooks, Bus subscriptions, background loops via s.Go).
func RegisterFeature(name string, fn Feature) {
	featMu.Lock()
	defer featMu.Unlock()
	features = append(features, struct {
		name string
		fn   Feature
	}{name, fn})
}

// Server is the coordinator. Create with New, serve Handler(), call Start
// for background loops and Close at the end.
type Server struct {
	Cfg   Config
	DB    *store.DB
	Blobs *store.Blobs
	Bus   *Bus
	Log   *slog.Logger
	Hooks Hooks

	router *Router
	nodes  *nodeRegistry
	nodeH  http.Handler

	schedMu sync.Mutex
	limitMu sync.Mutex // lockLimits
	sbLocks keyedMutex
	ipLim   *ipLimiter
	usage   *keyUsage
	waiters *stateWaiters

	activityMu sync.Mutex
	deletionMu sync.Mutex                 // one deletion-operation pass at a time
	activity   map[string]map[string]bool // sandbox -> active prompt ids

	hist *histState // coordinator copy of command history (feat_history.go)

	earnMu      sync.Mutex
	evacUploads map[string]time.Time    // node -> last evacuation upload round (zero: running), guarded by earnMu
	earned      map[string]nodeEarnings // node id -> cached earnings (nodes_evacuate.go)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	start  sync.Once
	// startedAt (ms): nodes cannot reconnect while the server is down, so
	// their offline time counts from here (seenAt).
	startedAt int64
}

// New opens the data dir, migrates the database and builds the router.
func New(cfg Config) (*Server, error) {
	cfg.defaults()
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(cfg.DataDir, "crowdvm.db"))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		Cfg:      cfg,
		DB:       db,
		Blobs:    &store.Blobs{Dir: filepath.Join(cfg.DataDir, "blobs")},
		Bus:      NewBus(),
		Log:      cfg.Logger,
		nodes:    newNodeRegistry(),
		ipLim:    newIPLimiter(cfg.IPRateLimit, time.Minute),
		usage:    newKeyUsage(),
		waiters:  newStateWaiters(),
		activity: map[string]map[string]bool{},
		ctx:      ctx,
		cancel:   cancel,
	} // Partial uploads of a previous run (a crash mid-upload): none can be
	// in progress before this server serves.
	s.Blobs.CleanTemp(0)

	if err := db.MigrateAll(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s.router = newRouter(s)
	s.routes()
	featMu.Lock()
	fs := append(features[:0:0], features...)
	featMu.Unlock()
	for _, f := range fs {
		if err := f.fn(s); err != nil {
			db.Close()
			return nil, err
		}
	}
	s.router.finish()
	s.Bus.Subscribe(s.waiters.onEvent)
	return s, nil
}

// Handler is the root HTTP handler (API, auth, node endpoints, dashboard).
func (s *Server) Handler() http.Handler { return s.router }

// Router exposes the router for features that register raw handlers.
func (s *Server) Router() *Router { return s.router }

// Context is canceled by Close.
func (s *Server) Context() context.Context { return s.ctx }

// Go runs fn in a tracked goroutine (waited for by Close).
func (s *Server) Go(fn func(ctx context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn(s.ctx)
	}()
}

// Start launches the background loops (TTL reaper, billing, node watchdog,
// garbage collection). Idempotent.
func (s *Server) Start() {
	s.start.Do(func() {
		s.startedAt = store.Now()
		s.Go(s.reaperLoop)
		s.Go(s.billingLoop)
		s.Go(s.maintenanceLoop)
	})
}

// Close stops loops, disconnects nodes and closes the database.
func (s *Server) Close() error {
	s.cancel()
	s.nodes.closeAll()
	s.wg.Wait()
	s.usage.flush(s)
	s.Bus.Close()
	return s.DB.Close()
}

// every runs fn every d until ctx ends.
func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

func (s *Server) maintenanceLoop(ctx context.Context) {
	var lastSweep time.Time
	every(ctx, 30*time.Second, func() {
		s.usage.flush(s)
		s.gcIdempotency()
		s.processDeletionOps(ctx) // also purges unused snapshots
		s.gcSnapshots(ctx)
		s.uploadStranded(ctx, nil)
		if time.Since(lastSweep) > 10*time.Minute {
			lastSweep = time.Now()
			s.sweepOrphanBlobs()
		}
		s.nodes.expireStale(s)
		s.cleanupAuth()
	})
}

// keyedMutex serializes lifecycle operations per sandbox.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*kmEntry
}

type kmEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*kmEntry{}
	}
	e := k.m[key]
	if e == nil {
		e = &kmEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
