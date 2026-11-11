package agents

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
)

// promptRequest is Boat's PromptRequest plus CrowdVM extensions (cwd,
// credentials, env, attachments).
type promptRequest struct {
	Provider        string            `json:"provider"`
	Model           *string           `json:"model"`
	ReasoningEffort *string           `json:"reasoningEffort"`
	Fast            *bool             `json:"fast"`
	New             bool              `json:"new"`
	ConversationID  *string           `json:"conversationId"`
	Prompt          string            `json:"prompt"`
	Cwd             string            `json:"cwd,omitempty"`
	Credentials     map[string]string `json:"credentials,omitempty"` // env vars, e.g. ANTHROPIC_API_KEY
	Env             map[string]string `json:"env,omitempty"`
	Attachments     []string          `json:"attachments,omitempty"` // paths in the sandbox (~/attachments/...)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

func fail(w http.ResponseWriter, status int, code, msg string, details map[string]any) {
	api.WriteError(w, api.NewError(status, code, msg, details))
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(v); err != nil && err.Error() != "EOF" {
		fail(w, 400, "invalid_request", "invalid JSON body: "+err.Error(), nil)
		return false
	}
	return true
}

func (m *Manager) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var req promptRequest
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		fail(w, 400, "prompt_required", "prompt is required: the natural-language task for the agent", nil)
		return
	}
	convID := deref(req.ConversationID)
	if req.New && convID != "" {
		fail(w, 400, "invalid_request", "new and conversationId are mutually exclusive", nil)
		return
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	for k, v := range req.Credentials {
		env[k] = v
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	var c *conv
	switch {
	case convID != "":
		if c = m.convs[convID]; c == nil {
			fail(w, 404, "conversation_not_found", "no conversation "+convID+" on this sandbox", nil)
			return
		}
	case !req.New:
		c = m.convs[m.st.Current]
	}
	provider := normalizeProvider(req.Provider)
	if provider == "" {
		if c != nil && c.LastHarness != "" {
			provider = c.LastHarness
		} else if provider = normalizeProvider(m.envValue("CROWDVM_DEFAULT_PROVIDER")); provider == "" {
			provider = "claude"
		}
	}
	h := harnesses[provider]
	if h == nil || (strings.HasPrefix(provider, "fake") && !fakeEnabled()) {
		fail(w, 400, "invalid_provider", "unknown provider "+strconv.Quote(req.Provider), map[string]any{"providers": providerNames()})
		return
	}
	model, effort := deref(req.Model), deref(req.ReasoningEffort)
	if req.Fast != nil && *req.Fast {
		check := model
		if check == "" { // the harness default model must support it too
			check = defaultModel(provider)
		}
		if mi, ok := findModel(provider, check); ok && !mi.FastMode {
			fail(w, 400, "fast_mode_not_supported", "model "+check+" has no fast mode; fast models: "+strings.Join(fastModels(), ", "),
				map[string]any{"model": check, "fastModels": fastModels()})
			return
		}
	}
	if err := checkAttachments(req.Attachments, m.o.Home); err != nil {
		fail(w, 400, "invalid_attachments", err.Error(), map[string]any{"maxFiles": maxAttachFiles, "maxFileBytes": maxAttachFile, "maxTotalBytes": maxAttachTotal})
		return
	}
	bin := h.bin
	if strings.HasPrefix(provider, "fake") {
		bin, _ = os.Executable()
	}
	if _, err := m.lookPath(bin, env); err != nil {
		fail(w, 409, "provider_not_installed", provider+": "+err.Error(), map[string]any{"provider": provider, "binary": bin})
		return
	}
	if ok, want := m.providerConfigured(provider, model, env); !ok {
		fail(w, 409, "provider_not_configured", "no usable credential for "+provider+" in this sandbox: add "+want+
			" (dashboard Agents & Proxying, or `crowdvm ai profile set-key`), or sign in inside the sandbox",
			map[string]any{"provider": provider, "accepts": want})
		return
	}
	if c == nil {
		now := time.Now().UTC()
		c = &conv{ID: newUUID(), CreatedAt: now}
		m.convs[c.ID] = c
	}
	p := m.newPromptLocked(c, provider, model, effort, req.Fast, req.Prompt, req.Cwd, req.Attachments, env)
	m.saveLocked()
	fast := req.Fast != nil && *req.Fast
	api.WriteOK(w, 202, "prompt.queued", map[string]any{"id": m.sandboxID(), "promptId": p.ID,
		"conversationId": c.ID, "promptRun": p.run(m.sandboxID()), "status": "queued", "provider": provider,
		"model": nilIfEmpty(model), "reasoningEffort": nilIfEmpty(effort), "fast": fast})
}

// newPromptLocked records a prompt (and its "prompt" event) and queues it.
func (m *Manager) newPromptLocked(c *conv, provider, model, effort string, fast *bool, text, cwd string, att []string, env map[string]string) *promptRec {
	now := time.Now().UTC()
	p := &promptRec{ID: api.NewID("prompt_", 16), ConversationID: c.ID, Provider: provider, Model: model,
		ReasoningEffort: effort, Fast: fast, Prompt: text, Status: "queued", CreatedAt: now, Cwd: cwd,
		Attachments: att, env: env}
	m.prompts[p.ID] = p
	c.Prompts++
	c.LastPromptAt, c.LastHarness, c.LastPromptPreview = now, provider, truncRunes(text, 80)
	if model != "" {
		c.LastModel = model
	}
	m.st.Current = c.ID
	data := map[string]any{"prompt": text, "status": "queued", "is_reverted": false, "provider": provider,
		"model": nilIfEmpty(model), "reasoningEffort": nilIfEmpty(effort)}
	if len(att) > 0 {
		data["attachments"] = eventAttachments(att, m.o.Home)
	}
	m.appendEventLocked(&Event{ID: p.ID, Type: "prompt", TaskID: p.ID, ConversationID: c.ID, Data: data})
	c.queue = append(c.queue, p)
	if !c.draining {
		c.draining = true
		go m.drain(c)
	}
	return p
}

// drain runs a conversation's prompts one at a time, under the sandbox cap.
func (m *Manager) drain(c *conv) {
	for {
		m.mu.Lock()
		if len(c.queue) == 0 {
			c.draining = false
			m.mu.Unlock()
			return
		}
		p := c.queue[0]
		c.queue = c.queue[1:]
		c.active, c.stop = p, false
		m.mu.Unlock()

		m.waitSetup(p)
		m.sem <- struct{}{}
		m.mu.Lock()
		if p.done() { // interrupted while waiting for a slot
			c.active = nil
			m.mu.Unlock()
			<-m.sem
			continue
		}
		p.Status, p.StartedAt = "running", time.Now().UTC()
		m.appendLocked("prompt.status", c.ID, p.ID, map[string]any{"status": "running"})
		m.saveLocked()
		m.mu.Unlock()

		m.execPrompt(c, p)
		<-m.sem
		m.mu.Lock()
		c.active = nil
		m.mu.Unlock()
	}
}

// waitSetup holds a prompt (still "queued") while a blocking repo setup runs.
func (m *Manager) waitSetup(p *promptRec) {
	if m.o.SetupBlocked == nil {
		return
	}
	for m.o.SetupBlocked() {
		m.mu.Lock()
		done := p.done()
		m.mu.Unlock()
		if done {
			return
		}
		time.Sleep(setupPoll)
	}
}

var setupPoll = time.Second

func (m *Manager) finishLocked(p *promptRec, status, errMsg string) {
	if p.done() {
		return
	}
	p.Status, p.Error, p.FinishedAt = status, errMsg, time.Now().UTC()
	p.env = nil
	data := map[string]any{"status": status}
	if errMsg != "" {
		data["error"] = errMsg
	}
	m.appendLocked("prompt.status", p.ConversationID, p.ID, data)
}

// settleSteer records a steer's mode. A pending (empty) mode is always set; a
// set mode only moves forward (native -> native-continued).
func (m *Manager) settleSteer(s *steerRec, mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.Mode == mode || (s.Mode != "" && !(s.Mode == "native" && mode == "native-continued")) {
		return
	}
	s.Mode = mode
	m.saveLocked()
	if i, ok := m.evIndex[s.ID]; ok {
		m.emitLocked(m.events[i])
	}
}

func (m *Manager) handlePromptRun(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.prompts[r.PathValue("id")]
	if p == nil {
		fail(w, 404, "prompt_not_found", "no prompt "+r.PathValue("id")+" on this sandbox", nil)
		return
	}
	api.WriteOK(w, 200, "prompt.run", map[string]any{"id": m.sandboxID(), "promptRun": p.run(m.sandboxID())})
}

func (m *Manager) handleConversations(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := make([]*conv, 0, len(m.convs))
	for _, c := range m.convs {
		cs = append(cs, c)
	}
	slices.SortFunc(cs, func(a, b *conv) int { return b.LastPromptAt.Compare(a.LastPromptAt) })
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		out = append(out, map[string]any{"id": c.ID, "createdAt": c.CreatedAt.Format(time.RFC3339Nano),
			"lastPromptAt": c.LastPromptAt.Format(time.RFC3339Nano), "prompts": c.Prompts, "running": c.busy(),
			"lastHarness": nilIfEmpty(c.LastHarness), "lastModel": nilIfEmpty(c.LastModel),
			"lastPromptPreview": nilIfEmpty(c.LastPromptPreview), "current": c.ID == m.st.Current})
	}
	api.WriteOK(w, 200, "conversation.list", map[string]any{"id": m.sandboxID(), "conversations": out})
}

func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// handleEvents: GET /v1/events?limit&cursor&sort&type&conversation[&wait=seconds].
// wait (CrowdVM extension) long-polls an empty ascending page for up to 60 s.
func (m *Manager) handleEvents(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	q := eventQuery{limit: 100, asc: qs.Get("sort") == "asc", cursor: qs.Get("cursor"),
		types: splitList(qs["type"]), convs: splitList(append(qs["conversation"], qs["convo"]...))}
	if s := qs.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			fail(w, 400, "invalid_request", "limit must be 1..200", nil)
			return
		}
		q.limit = min(n, 200)
	}
	wait, _ := strconv.Atoi(qs.Get("wait"))
	deadline := time.Now().Add(time.Duration(min(max(wait, 0), 60)) * time.Second)
	for {
		m.mu.Lock()
		evs, pi, ok := m.queryLocked(q)
		changed := m.changed
		m.mu.Unlock()
		if !ok {
			fail(w, 400, "invalid_cursor", "cursor is not a valid events cursor", nil)
			return
		}
		if len(evs) == 0 && q.asc && time.Now().Before(deadline) {
			select {
			case <-changed:
				continue
			case <-time.After(time.Until(deadline)):
				continue
			case <-r.Context().Done():
				return
			}
		}
		body, _ := json.Marshal(map[string]any{"ok": true, "type": "events.list", "id": m.sandboxID(), "events": evs, "pageInfo": pi})
		etag := fmt.Sprintf(`"%x"`, sha256.Sum256(body))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(body, '\n'))
		return
	}
}

func (m *Manager) handleSteer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message      string  `json:"message"`
		Conversation *string `json:"conversation"`
	}
	if !decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		fail(w, 400, "invalid_request", "message is required", nil)
		return
	}
	convID := deref(req.Conversation)
	if convID == "" {
		convID = r.URL.Query().Get("conversation")
	}
	m.mu.Lock()
	if convID == "" {
		convID = m.st.Current
	}
	c := m.convs[convID]
	noTurn := func(reason string, extra map[string]any) {
		m.mu.Unlock()
		d := map[string]any{"reason": reason}
		for k, v := range extra {
			d[k] = v
		}
		fail(w, 409, "no_running_turn", "the conversation has no running turn; use POST /prompt to queue work", d)
	}
	if c == nil {
		if req.Conversation != nil && deref(req.Conversation) != "" || r.URL.Query().Get("conversation") != "" {
			m.mu.Unlock()
			fail(w, 404, "conversation_not_found", "no conversation "+convID+" on this sandbox", nil)
			return
		}
		noTurn("no_turn", nil)
		return
	}
	if c.active == nil {
		var last *promptRec
		for _, p := range m.prompts {
			if p.ConversationID == c.ID && (last == nil || p.CreatedAt.After(last.CreatedAt)) {
				last = p
			}
		}
		if last == nil {
			noTurn("no_turn", nil)
		} else {
			noTurn("turn_ended", map[string]any{"lastTurn": map[string]any{"status": last.Status, "error": nilIfEmpty(last.Error)}})
		}
		return
	}
	if c.run == nil {
		noTurn("turn_not_started", nil)
		return
	}
	run, active := c.run, c.active
	s := &steerRec{ID: api.NewID("steer_", 16), ConversationID: c.ID, PromptID: active.ID, Message: req.Message, CreatedAt: time.Now().UTC()}
	m.steers[s.ID] = s
	m.appendEventLocked(&Event{ID: s.ID, Type: "steer", TaskID: active.ID, ConversationID: c.ID,
		Data: map[string]any{"message": req.Message, "status": "steered"}})
	m.saveLocked()
	m.mu.Unlock()

	mode := run.steer(s)
	if mode == "failed" { // Boat: 409 no_running_turn, reason delivery_failed
		m.settleSteer(s, mode)
		fail(w, 409, "no_running_turn", "the agent did not accept the steer; check the turn before sending more work",
			map[string]any{"reason": "delivery_failed", "promptId": active.ID})
		return
	}
	if mode == "late" { // the turn ended under us: run the message as a new turn
		m.mu.Lock()
		m.newPromptLocked(c, active.Provider, active.Model, active.ReasoningEffort, active.Fast, req.Message, active.Cwd, nil, active.env)
		m.saveLocked()
		m.mu.Unlock()
	}
	m.settleSteer(s, mode)
	api.WriteOK(w, 200, "prompt.steered", map[string]any{"id": m.sandboxID(), "conversationId": c.ID,
		"promptId": s.ID, "native": mode == "native", "mode": mode, "status": "steered"})
}

func (m *Manager) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Conversation *string `json:"conversation"`
	}
	if !decode(w, r, &req) {
		return
	}
	convID := r.URL.Query().Get("conversation")
	if convID == "" {
		convID = deref(req.Conversation)
	}
	m.mu.Lock()
	var targets []*conv
	if convID != "" {
		c := m.convs[convID]
		if c == nil {
			m.mu.Unlock()
			fail(w, 404, "conversation_not_found", "no conversation "+convID+" on this sandbox", nil)
			return
		}
		targets = []*conv{c}
	} else {
		for _, c := range m.convs {
			targets = append(targets, c)
		}
	}
	var runs []*run
	for _, c := range targets {
		for _, p := range c.queue {
			m.finishLocked(p, "interrupted", "")
		}
		c.queue = nil
		if c.active != nil {
			c.stop = true
			if c.active.Status == "queued" {
				m.finishLocked(c.active, "interrupted", "")
			}
			if c.run != nil {
				runs = append(runs, c.run)
			}
		}
	}
	m.saveLocked()
	m.mu.Unlock()
	for _, r := range runs {
		r.interrupt()
	}
	resp := map[string]any{"id": m.sandboxID(), "status": "interrupted"}
	if convID != "" {
		resp["conversationId"] = convID
	}
	api.WriteOK(w, 200, "sandbox.interrupted", resp)
}

// ---------------------------------------------------------------------------

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func truncRunes(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// maxParallel is the per-sandbox cap on concurrent turns: an explicit env
// value, else ~3 per GiB of RAM ("about two dozen" on 8 GiB).
func maxParallel(env func(string) string) int {
	for _, k := range []string{"CROWDVM_MAX_PARALLEL_CONVERSATIONS", "ASCII_MAX_PARALLEL_CONVERSATIONS"} {
		if n, err := strconv.Atoi(env(k)); err == nil && n > 0 {
			return n
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range bytes.Split(b, []byte("\n")) {
			if f := strings.Fields(string(line)); len(f) >= 2 && f[0] == "MemTotal:" {
				kb, _ := strconv.Atoi(f[1])
				return max(2, kb/(1<<20)*3)
			}
		}
	}
	return 8
}

// maxEventAttachInline: attachments up to this size are inlined (base64) in
// the prompt event, as Boat's PromptEvent.data.attachments; larger ones are
// listed by name and type only, to keep the event log small.
const maxEventAttachInline = 256 << 10

func eventAttachments(paths []string, home string) []map[string]any {
	out := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		full := p
		if strings.HasPrefix(full, "~/") {
			full = filepath.Join(home, full[2:])
		} else if !filepath.IsAbs(full) {
			full = filepath.Join(home, full)
		}
		a := map[string]any{"filename": filepath.Base(p)}
		mt := mime.TypeByExtension(filepath.Ext(p))
		if b, err := os.ReadFile(full); err == nil && len(b) <= maxEventAttachInline {
			if mt == "" {
				mt = http.DetectContentType(b)
			}
			a["base64Data"] = base64.StdEncoding.EncodeToString(b)
		}
		if mt == "" {
			mt = "application/octet-stream"
		}
		a["mimeType"] = mt
		out = append(out, a)
	}
	return out
}
