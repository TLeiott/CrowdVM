// Package agents runs the integrated coding-agent harnesses (Claude Code, Codex,
// OpenCode, pi, Prime Agent, Kimi Code, Mistral Vibe) inside a sandbox VM and
// serves Boat's prompt / prompts / events / conversations / steer / interrupt
// endpoints under /v1 (the public /sandboxes/{id} prefix replaced by /v1).
//
// Conversations, prompt runs and events are persisted under DataDir, so they
// survive stop/resume/fork together with the harness session files in the home
// directory.
package agents

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

// Options configures a Manager.
type Options struct {
	Home, User   string // sandbox user's home and name (default proto.GuestHome / proto.GuestUser)
	Unprivileged bool   // run harnesses as the current user (tests / process driver)
	DataDir      string // persistent state (default Home/.crowdvm/agents)
	// Env returns the sandbox environment (exported for harnesses, may hold keys).
	Env func() map[string]string
	// Emit receives every event as proto.GuestEvent{Type:"prompt.event"}. Must not block.
	Emit    func(proto.GuestEvent)
	AIProxy *proto.AIProxy
	// SandboxID is echoed as "id" in responses. Optional: falls back to
	// CROWDVM_SANDBOX_ID from Env, then the hostname.
	SandboxID string
	// SetupBlocked reports whether a blocking repo setup is still running;
	// prompts stay queued meanwhile (Boat setupBlocking). Optional.
	SetupBlocked func() bool
	// EnsureDesktop starts the sandbox desktop (computer use). Called in the
	// background before each turn; errors are ignored. Optional.
	EnsureDesktop func() error
	// DefaultCwd is the harness working directory when a prompt names none
	// (the environment's single repo). "" or a missing directory = Home. Optional.
	DefaultCwd func() string
	// Spawn starts a harness process instead of cmd.Start (e.g. to place it in
	// a workload cgroup). Optional.
	Spawn func(cmd *exec.Cmd) error
	// ZeroDataRetention (account ZDR): keep no prompt text or events on disk.
	// Events live in memory only (lost at agent restart); state.json keeps
	// prompt runs without their text. Optional.
	ZeroDataRetention func() bool
}

// Manager owns conversations, the prompt queue and the harness processes.
type Manager struct {
	o Options

	proxyMu sync.Mutex
	proxy   *proto.AIProxy // Options.AIProxy, replaced by SetAIProxy

	mu       sync.Mutex
	st       state
	convs    map[string]*conv
	prompts  map[string]*promptRec
	steers   map[string]*steerRec
	events   []*Event
	evIndex  map[string]int
	evFile   *os.File
	evBytes  int64 // serialized size of m.events (compaction trigger)
	lastTS   int64
	changed  chan struct{} // closed and replaced on every change (long-poll)
	sem      chan struct{} // parallel turn cap
	maxTurns int
}

// New loads persisted state from DataDir. Prompts that were queued or running
// when the agent last stopped are marked interrupted.
func New(o Options) *Manager {
	if o.Home == "" {
		o.Home = proto.GuestHome
	}
	if o.User == "" {
		o.User = proto.GuestUser
	}
	if o.DataDir == "" {
		o.DataDir = filepath.Join(o.Home, ".crowdvm", "agents")
	}
	if o.Env == nil {
		o.Env = func() map[string]string { return nil }
	}
	m := &Manager{o: o, convs: map[string]*conv{}, prompts: map[string]*promptRec{},
		steers: map[string]*steerRec{}, evIndex: map[string]int{}, changed: make(chan struct{})}
	m.proxy = o.AIProxy
	m.maxTurns = maxParallel(m.envValue)
	m.sem = make(chan struct{}, m.maxTurns)
	m.load()
	m.registerComputer()
	return m
}

// SetAIProxy replaces the AI credentials proxy settings (live config update).
// New harness turns use them; running turns keep theirs.
func (m *Manager) SetAIProxy(p *proto.AIProxy) {
	m.proxyMu.Lock()
	m.proxy = p
	m.proxyMu.Unlock()
}

func (m *Manager) aiProxy() *proto.AIProxy {
	m.proxyMu.Lock()
	defer m.proxyMu.Unlock()
	return m.proxy
}

// Register mounts the agent endpoints on mux.
func (m *Manager) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/prompt", m.handlePrompt)
	mux.HandleFunc("GET /v1/prompts/{id}", m.handlePromptRun)
	mux.HandleFunc("GET /v1/events", m.handleEvents)
	mux.HandleFunc("GET /v1/conversations", m.handleConversations)
	mux.HandleFunc("POST /v1/steer", m.handleSteer)
	mux.HandleFunc("POST /v1/interrupt", m.handleInterrupt)
}

// Busy reports whether any prompt is queued or running (sandbox state "running"
// vs "idle").
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.convs {
		if c.busy() {
			return true
		}
	}
	return false
}

// ProxyEnv returns the env vars that point harness CLIs at the AI credentials
// proxy. Convention: {BaseURL}/{vendor} forwards the rest of the path verbatim
// to that vendor's API; the proxy token is the placeholder key for every vendor.
func (m *Manager) ProxyEnv() map[string]string {
	p := m.aiProxy()
	if p == nil || p.BaseURL == "" {
		return map[string]string{}
	}
	b := strings.TrimRight(p.BaseURL, "/")
	return map[string]string{
		"CROWDVM_AI_PROXY_URL": b,
		"ANTHROPIC_BASE_URL":   b + "/anthropic",
		"ANTHROPIC_API_KEY":    p.Token,
		"OPENAI_BASE_URL":      b + "/openai/v1",
		"OPENAI_API_KEY":       p.Token,
		"OPENROUTER_BASE_URL":  b + "/openrouter/api/v1",
		"OPENROUTER_API_KEY":   p.Token,
		"MISTRAL_BASE_URL":     b + "/mistral/v1",
		"MISTRAL_API_KEY":      p.Token,
		"MOONSHOT_BASE_URL":    b + "/moonshot/v1",
		"MOONSHOT_API_KEY":     p.Token,
		"XAI_BASE_URL":         b + "/xai/v1",
		"XAI_API_KEY":          p.Token,
		"OPENCODE_BASE_URL":    b + "/opencode/zen/v1",
		"OPENCODE_API_KEY":     p.Token,
	}
}

func (m *Manager) envValue(k string) string {
	if v, ok := m.o.Env()[k]; ok {
		return v
	}
	return os.Getenv(k)
}

func (m *Manager) sandboxID() string {
	if m.o.SandboxID != "" {
		return m.o.SandboxID
	}
	if v := m.envValue("CROWDVM_SANDBOX_ID"); v != "" {
		return v
	}
	h, _ := os.Hostname()
	return h
}

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

// Event is one Boat sandbox event (prompt, response, steer, prompt.status).
type Event struct {
	ID             string         `json:"id"`
	Type           string         `json:"type"`
	Timestamp      int64          `json:"timestamp"` // unix ms
	TaskID         string         `json:"taskId,omitempty"`
	ConversationID string         `json:"conversationId,omitempty"`
	Data           map[string]any `json:"data"`
}

type promptRec struct {
	ID              string    `json:"id"`
	ConversationID  string    `json:"conversationId"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model,omitempty"`
	ReasoningEffort string    `json:"reasoningEffort,omitempty"`
	Fast            *bool     `json:"fast,omitempty"`
	Prompt          string    `json:"prompt"`
	Status          string    `json:"status"` // queued running finished failed interrupted
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	StartedAt       time.Time `json:"startedAt,omitzero"`
	FinishedAt      time.Time `json:"finishedAt,omitzero"`
	Cwd             string    `json:"cwd,omitempty"`
	Attachments     []string  `json:"attachments,omitempty"`

	env map[string]string // request credentials/env: memory only, never persisted
}

func (p *promptRec) done() bool {
	return p.Status == "finished" || p.Status == "failed" || p.Status == "interrupted"
}

func (p *promptRec) run(sandboxID string) map[string]any {
	r := map[string]any{"id": p.ID, "promptId": p.ID, "sandboxId": sandboxID, "status": p.Status,
		"done": p.done(), "createdAt": p.CreatedAt.UTC().Format(time.RFC3339Nano),
		"model": nilIfEmpty(p.Model), "reasoningEffort": nilIfEmpty(p.ReasoningEffort),
		"fast": p.Fast, "conversationId": p.ConversationID, "provider": p.Provider}
	if !p.StartedAt.IsZero() {
		r["startedAt"] = p.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if !p.FinishedAt.IsZero() {
		r["finishedAt"] = p.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	if p.Error != "" {
		r["error"] = p.Error
	}
	return r
}

type steerRec struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversationId"`
	PromptID       string    `json:"promptId"` // the running prompt it steered
	Message        string    `json:"message"`
	Mode           string    `json:"mode"` // native native-continued fallback late
	CreatedAt      time.Time `json:"createdAt"`
}

type conv struct {
	ID                string    `json:"id"`
	CreatedAt         time.Time `json:"createdAt"`
	LastPromptAt      time.Time `json:"lastPromptAt"`
	Prompts           int       `json:"prompts"`
	LastHarness       string    `json:"lastHarness,omitempty"`
	LastModel         string    `json:"lastModel,omitempty"`
	LastPromptPreview string    `json:"lastPromptPreview,omitempty"`
	// Session is the native harness session id, valid for SessionHarness.
	Session        string `json:"session,omitempty"`
	SessionHarness string `json:"sessionHarness,omitempty"`

	queue    []*promptRec
	active   *promptRec
	run      *run
	draining bool
	stop     bool // interrupt requested for the active prompt
}

func (c *conv) busy() bool { return c.active != nil || len(c.queue) > 0 }

type state struct {
	Current       string       `json:"current"`
	Conversations []*conv      `json:"conversations"`
	Prompts       []*promptRec `json:"prompts"`
	Steers        []*steerRec  `json:"steers"`
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
