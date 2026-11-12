package agents

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// On-disk layout under DataDir:
//
//	state.json    conversations, prompt runs, steers (rewritten atomically)
//	events.jsonl  append-only event log, compacted when it outgrows
//	              maxEventBytes (oldest events dropped)
//
// Streaming partials (response events with is_streaming) live in memory only.
// Wipe forgets every conversation, prompt and event, in memory and on disk
// (a scrub: the sandbox's disk came from another account, bugs-r9-sonnet).
func (m *Manager) Wipe() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.evFile != nil {
		_ = m.evFile.Close()
		m.evFile = nil
	}
	m.st = state{}
	m.convs, m.prompts, m.steers = map[string]*conv{}, map[string]*promptRec{}, map[string]*steerRec{}
	m.events, m.evIndex, m.evBytes = nil, map[string]int{}, 0
	_ = os.RemoveAll(m.o.DataDir)
	_ = os.MkdirAll(m.o.DataDir, 0o700)
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *Manager) load() {
	_ = os.MkdirAll(m.o.DataDir, 0o700)
	if b, err := os.ReadFile(filepath.Join(m.o.DataDir, "state.json")); err == nil {
		_ = json.Unmarshal(b, &m.st)
	}
	for _, c := range m.st.Conversations {
		m.convs[c.ID] = c
	}
	for _, s := range m.st.Steers {
		m.steers[s.ID] = s
	}
	if f, err := os.Open(filepath.Join(m.o.DataDir, "events.jsonl")); err == nil {
		r := bufio.NewReader(f)
		for {
			line, err := r.ReadBytes('\n')
			var e Event
			if len(line) > 0 && json.Unmarshal(line, &e) == nil && e.ID != "" {
				m.evIndex[e.ID] = len(m.events)
				m.events = append(m.events, &e)
				m.lastTS = max(m.lastTS, e.Timestamp)
				m.evBytes += int64(len(line))
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
	m.evFile, _ = os.OpenFile(filepath.Join(m.o.DataDir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.zdr() { // retention turned on: drop what an earlier boot kept
		_ = m.evFile.Truncate(0)
	} else if m.evBytes > m.maxEventBytes() {
		m.compactLocked()
	}
	dirty := false
	for _, p := range m.st.Prompts {
		m.prompts[p.ID] = p
		if !p.done() { // the agent (or the VM) stopped under it
			m.finishLocked(p, "interrupted", "the sandbox agent restarted while this prompt was "+p.Status)
			dirty = true
		}
	}
	// Retention on: rewrite state.json without the text an earlier boot kept
	// (bugs-r17-sonnet 6).
	if dirty || m.zdr() {
		m.saveLocked()
	}
}

func (m *Manager) saveLocked() {
	m.st.Conversations = m.st.Conversations[:0]
	for _, c := range m.convs {
		m.st.Conversations = append(m.st.Conversations, c)
	}
	m.st.Prompts = m.st.Prompts[:0]
	for _, p := range m.prompts {
		m.st.Prompts = append(m.st.Prompts, p)
	}
	m.st.Steers = m.st.Steers[:0]
	for _, s := range m.steers {
		m.st.Steers = append(m.st.Steers, s)
	}
	st := m.st
	if m.zdr() { // no prompt text on disk
		st.Conversations, st.Prompts, st.Steers = nil, nil, nil
		for _, c := range m.st.Conversations {
			cc := *c
			cc.LastPromptPreview = ""
			st.Conversations = append(st.Conversations, &cc)
		}
		for _, p := range m.st.Prompts {
			pp := *p
			pp.Prompt = ""
			st.Prompts = append(st.Prompts, &pp)
		}
		for _, s := range m.st.Steers {
			ss := *s
			ss.Message = ""
			st.Steers = append(st.Steers, &ss)
		}
	}
	b, err := json.Marshal(&st)
	if err != nil {
		return
	}
	tmp := filepath.Join(m.o.DataDir, "state.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(m.o.DataDir, "state.json"))
	}
}

// appendLocked stores, persists, broadcasts and emits one event.
func (m *Manager) appendLocked(typ, convID, taskID string, data map[string]any) *Event {
	prefix := map[string]string{"response": "resp_", "prompt.status": "pst_"}[typ]
	if prefix == "" {
		prefix = "evt_"
	}
	return m.appendEventLocked(&Event{ID: api.NewID(prefix, 16), Type: typ, TaskID: taskID, ConversationID: convID, Data: data})
}

func (m *Manager) appendEventLocked(e *Event) *Event {
	ts := time.Now().UnixMilli()
	if ts < m.lastTS {
		ts = m.lastTS
	}
	m.lastTS, e.Timestamp = ts, ts
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	m.evIndex[e.ID] = len(m.events)
	m.events = append(m.events, e)
	b, _ := json.Marshal(e)
	m.evBytes += int64(len(b) + 1)
	if m.evFile != nil && !isPartial(e) && !m.zdr() {
		_, _ = m.evFile.Write(append(b, '\n'))
	}
	m.emitLocked(e)
	if m.evBytes > m.maxEventBytes() {
		m.compactLocked()
	}
	return e
}

func (m *Manager) zdr() bool { return m.o.ZeroDataRetention != nil && m.o.ZeroDataRetention() }

func isPartial(e *Event) bool {
	s, _ := e.Data["is_streaming"].(bool)
	return s && e.Type == "response"
}

// maxEventBytes caps events.jsonl (and the in-memory log): 64 MiB, or
// CROWDVM_AGENT_EVENTS_MAX_BYTES.
func (m *Manager) maxEventBytes() int64 {
	if n, err := strconv.ParseInt(m.envValue("CROWDVM_AGENT_EVENTS_MAX_BYTES"), 10, 64); err == nil && n > 0 {
		return n
	}
	return 64 << 20
}

// compactLocked drops streaming partials and the oldest events until the log
// is at most half the cap, then rewrites events.jsonl atomically. Cursors that
// point at dropped events still work (they fall back to their timestamp).
func (m *Manager) compactLocked() {
	limit := m.maxEventBytes() / 2
	lines := make([][]byte, len(m.events))
	keep := make([]bool, len(m.events))
	var total int64
	for i := len(m.events) - 1; i >= 0; i-- {
		e := m.events[i]
		if isPartial(e) && i < len(m.events)-50 { // old partials are superseded by their final text
			continue
		}
		b, _ := json.Marshal(e)
		if total+int64(len(b)+1) > limit {
			break
		}
		total += int64(len(b) + 1)
		lines[i], keep[i] = b, true
	}
	var kept []*Event
	var buf bytes.Buffer
	for i, e := range m.events {
		if !keep[i] {
			continue
		}
		kept = append(kept, e)
		if !isPartial(e) && !m.zdr() {
			buf.Write(lines[i])
			buf.WriteByte('\n')
		}
	}
	path := filepath.Join(m.o.DataDir, "events.jsonl")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return
	}
	if m.evFile != nil {
		m.evFile.Close()
	}
	m.evFile, _ = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	m.events, m.evBytes = kept, total
	m.evIndex = make(map[string]int, len(kept))
	for i, e := range kept {
		m.evIndex[e.ID] = i
	}
}

// emitLocked forwards the (overlaid) event to Options.Emit and wakes long-polls.
func (m *Manager) emitLocked(e *Event) {
	close(m.changed)
	m.changed = make(chan struct{})
	if m.o.Emit != nil {
		m.o.Emit(proto.GuestEvent{Time: time.UnixMilli(e.Timestamp), Type: "prompt.event", Data: mustJSON(m.viewLocked(e))})
	}
}

// viewLocked returns a copy of e with live state overlaid: prompt events carry
// the current prompt status, steer events their settled mode.
func (m *Manager) viewLocked(e *Event) *Event {
	v := *e
	switch e.Type {
	case "prompt":
		if p := m.prompts[e.TaskID]; p != nil {
			v.Data = cloneMap(e.Data)
			v.Data["status"] = p.Status
			if p.Error != "" {
				v.Data["error"] = p.Error
			}
		}
	case "steer":
		if s := m.steers[e.ID]; s != nil {
			v.Data = cloneMap(e.Data)
			v.Data["mode"] = s.Mode
			v.Data["native"] = s.Mode == "native" || s.Mode == "native-continued"
			if s.Mode == "failed" {
				v.Data["status"] = "failed"
			}
		}
	}
	return &v
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// eventQuery is GET /v1/events.
type eventQuery struct {
	limit  int
	cursor string
	asc    bool
	types  []string
	convs  []string
}

type pageInfo struct {
	NextCursor *string `json:"nextCursor"`
	HasMore    bool    `json:"hasMore"`
	Limit      int     `json:"limit"`
}

func encodeCursor(e *Event) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(e.Timestamp, 10) + ":" + e.ID))
}

// cursorBoundsLocked decodes a cursor into the first index newer than it
// (asc) and the last index older than it (desc). ok=false when undecodable.
func (m *Manager) cursorBoundsLocked(c string) (ascStart, descEnd int, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(c, "="))
	if err != nil {
		return 0, 0, false
	}
	ts, id, found := strings.Cut(string(b), ":")
	if !found {
		return 0, 0, false
	}
	if i, ok := m.evIndex[id]; ok {
		return i + 1, i - 1, true
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	first := func(pred func(*Event) bool) int {
		for i, e := range m.events {
			if pred(e) {
				return i
			}
		}
		return len(m.events)
	}
	return first(func(e *Event) bool { return e.Timestamp > t }), first(func(e *Event) bool { return e.Timestamp >= t }) - 1, true
}

func (m *Manager) queryLocked(q eventQuery) ([]*Event, pageInfo, bool) {
	start, end := 0, len(m.events)-1
	if q.cursor != "" {
		var ok bool
		if start, end, ok = m.cursorBoundsLocked(q.cursor); !ok {
			return nil, pageInfo{}, false
		}
	}
	match := func(e *Event) bool {
		return (len(q.types) == 0 || slices.Contains(q.types, e.Type)) &&
			(len(q.convs) == 0 || slices.Contains(q.convs, e.ConversationID))
	}
	var out []*Event
	more := false
	add := func(i int) bool {
		e := m.events[i]
		if !match(e) {
			return true
		}
		if len(out) == q.limit {
			more = true
			return false
		}
		out = append(out, m.viewLocked(e))
		return true
	}
	if q.asc {
		for i := start; i < len(m.events) && add(i); i++ {
		}
	} else {
		for i := min(end, len(m.events)-1); i >= 0 && add(i); i-- {
		}
	}
	pi := pageInfo{HasMore: more, Limit: q.limit}
	if more && len(out) > 0 {
		c := encodeCursor(out[len(out)-1])
		pi.NextCursor = &c
	}
	if out == nil {
		out = []*Event{}
	}
	return out, pi, true
}

// transcriptLocked renders a conversation's history as plain text, used when a
// conversation moves to another harness (or a harness without sessions).
func (m *Manager) transcriptLocked(convID, exceptTask string) string {
	var sb strings.Builder
	for _, e := range m.events {
		if e.ConversationID != convID || e.TaskID == exceptTask {
			continue
		}
		switch e.Type {
		case "prompt":
			sb.WriteString("\n[user]: " + str(e.Data["prompt"]) + "\n")
		case "steer":
			if s := m.steers[e.ID]; s != nil && s.Mode == "failed" {
				continue // never reached the agent
			}
			sb.WriteString("\n[user, mid-turn]: " + str(e.Data["message"]) + "\n")
		case "response":
			if isPartial(e) {
				continue
			}
			if c := str(e.Data["content"]); c != "" {
				sb.WriteString("\n[assistant]: " + c + "\n")
			}
			if tools, ok := e.Data["tools"].([]any); ok {
				for _, t := range tools {
					if tm, ok := t.(map[string]any); ok {
						if u, ok := tm["use"].(map[string]any); ok {
							sb.WriteString("[assistant tool call]: " + str(u["name"]) + " " + truncate(string(mustJSON(u["input"])), 300) + "\n")
						}
					}
				}
			}
		}
	}
	s := sb.String()
	if len(s) > 100_000 {
		s = "…" + s[len(s)-100_000:]
	}
	return s
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
