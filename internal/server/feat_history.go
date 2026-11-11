package server

// Coordinator copy of what happens inside a sandbox, so it stays readable
// while the sandbox is stopped (CMD-015..019, CMD-025, CMD-027, AGT-013):
//
//   - command history records + stdout/stderr logs and 30 s process / CPU /
//     RAM / disk samples are pulled from the guest
//     (GET /v1/internal/history/export, then POST /v1/internal/history/ack)
//     about every 2 s while it runs and once more (flush=1) right before a
//     stop;
//   - agent events (prompt.event) arrive on the guest event stream.
//
// While the sandbox runs, the guest answers these routes itself. Otherwise
// GET command-history[/{id}[/logs]], processes (live:false, last sample),
// events, conversations and prompts/{id} are served from here. Retention:
// exec/API commands 90 d, other processes 72 h, samples 7 d; everything goes
// with the sandbox. Monitoring off: the guest exports nothing and the
// retention guard answers 409 monitoring_off.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
	"github.com/crowdvm/crowdvm/internal/server/store"
)

const (
	histPollEvery   = 2 * time.Second
	histKeepCmds    = 90 * 24 * time.Hour
	histKeepProcs   = 72 * time.Hour
	histKeepSamples = 7 * 24 * time.Hour
	histMaxLogRead  = 1 << 20
)

func init() {
	store.Register("history", store.Migration{Version: 1, SQL: `
CREATE TABLE command_history (
  sandbox_id TEXT NOT NULL,
  command_id TEXT NOT NULL,
  mode TEXT NOT NULL DEFAULT '',
  command TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL,
  record TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (sandbox_id, command_id)
);
CREATE INDEX command_history_list ON command_history(sandbox_id, started_at);
CREATE TABLE command_logs (
  sandbox_id TEXT NOT NULL,
  command_id TEXT NOT NULL,
  stream TEXT NOT NULL,
  off INTEGER NOT NULL,
  data BLOB NOT NULL,
  PRIMARY KEY (sandbox_id, command_id, stream, off)
);
CREATE TABLE process_samples (
  sandbox_id TEXT NOT NULL,
  sampled_at INTEGER NOT NULL,
  data TEXT NOT NULL,
  PRIMARY KEY (sandbox_id, sampled_at)
);
CREATE TABLE agent_events (
  sandbox_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  type TEXT NOT NULL,
  conversation_id TEXT NOT NULL DEFAULT '',
  task_id TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL,
  PRIMARY KEY (sandbox_id, event_id)
);
CREATE INDEX agent_events_ts ON agent_events(sandbox_id, ts, event_id);
`}, store.Migration{Version: 3, SQL: `ALTER TABLE command_history ADD COLUMN private INTEGER NOT NULL DEFAULT 0;`}, store.Migration{Version: 2, SQL: `
CREATE TABLE output_usage (
  wallet_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  hour INTEGER NOT NULL,
  bytes INTEGER NOT NULL,
  PRIMARY KEY (wallet_id, kind, hour)
);`})
	RegisterFeature("history", func(s *Server) error {
		h := &histState{s: s}
		s.hist = h
		s.API("GET /sandboxes/{sandboxId}/metrics", "sandbox.read", s.handleSandboxMetrics)
		s.Bus.Subscribe(h.onEvent)
		s.Go(h.loop)
		return nil
	})
}

type histState struct {
	s           *Server
	busy        sync.Map // sandbox id -> struct{} while a pull runs
	unsupported sync.Map // sandbox id -> time.Time of the last 404
}

// ---------------------------------------------------------------------------
// Pull from the guest
// ---------------------------------------------------------------------------

func (h *histState) loop(ctx context.Context) {
	every(ctx, histPollEvery, func() {
		rows, err := h.s.DB.Query(`SELECT id FROM sandboxes WHERE deleted_at IS NULL AND monitoring=1 AND state IN ('ready','idle','running')`)
		if err != nil {
			return
		}
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			if t, ok := h.unsupported.Load(id); ok && time.Since(t.(time.Time)) < time.Minute {
				continue
			}
			id := id
			if _, busy := h.busy.LoadOrStore(id, struct{}{}); busy {
				continue
			}
			h.s.Go(func(ctx context.Context) {
				defer h.busy.Delete(id)
				if sb, err := h.s.SandboxByID(id); err == nil && sb.Usable() {
					_ = h.pull(ctx, sb, false)
				}
			})
		}
		if time.Now().Minute() == 0 && time.Now().Second() < 3 {
			h.prune()
		}
	})
}

// pull drains the guest's export (flush: everything, also partial output).
func (h *histState) pull(ctx context.Context, sb *Sandbox, flush bool) error {
	q := "internal/history/export?maxBytes=4194304"
	if flush {
		q += "&flush=1"
	}
	for i := 0; i < 64; i++ {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var ex proto.HistoryExport
		err := h.s.envGuestDo(cctx, sb, "GET", q, nil, &ex)
		cancel()
		if err != nil {
			if errors.Is(err, errNotFoundGuest) || strings.Contains(err.Error(), ": 404 ") {
				h.unsupported.Store(sb.ID, time.Now())
				return nil
			}
			return err
		}
		h.unsupported.Delete(sb.ID)
		h.store(sb.ID, &ex)
		if ex.Batch != "" {
			b, _ := json.Marshal(map[string]string{"batch": ex.Batch})
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := h.s.envGuestDo(cctx, sb, "POST", "internal/history/ack", bytes.NewReader(b), nil)
			cancel()
			if err != nil {
				return err
			}
		}
		if !ex.More || (len(ex.Records) == 0 && len(ex.Chunks) == 0 && len(ex.Samples) == 0) {
			return nil
		}
	}
	return nil
}

func skewIf(ok bool, d int64) int64 {
	if ok {
		return d
	}
	return 0
}

func (h *histState) store(sbID string, ex *proto.HistoryExport) {
	now := store.Now()
	// A command is private when it started in a private period of the
	// sandbox (the current one, from privateSince; earlier ones in
	// privateSpans), whenever its record reaches the server.
	var sb *Sandbox
	if b, err := h.s.SandboxByID(sbID); err == nil {
		sb = b
	}
	// Record times are on the guest's clock: move them onto the server's,
	// whose clock the privacy periods use (bugs-r4-opus 7).
	var skew int64
	if !ex.Now.IsZero() {
		skew = now - ex.Now.UnixMilli()
	}
	privateAt := func(t int64) bool {
		if sb == nil || sb.OrgID == "" {
			return false
		}
		if sb.Visibility == "private" {
			since, _ := sb.Extra["privateSince"].(float64)
			if t >= int64(since) {
				return true
			}
		}
		spans, _ := sb.Extra["privateSpans"].([]any)
		for _, x := range spans {
			if p, ok := x.([]any); ok && len(p) == 2 {
				a, _ := p[0].(float64)
				b, _ := p[1].(float64)
				if t >= int64(a) && t < int64(b) {
					return true
				}
			}
		}
		return false
	}
	for _, r := range ex.Records {
		if r.CommandID == "" {
			continue
		}
		started := store.T(r.StartedAt)
		if started == 0 {
			started = now
		}
		// A record the output cap truncated stays truncated (CMD-020).
		// private: recorded while the sandbox was private to its creator;
		// teammates never read it (Boat organizations__shared-sandboxes).
		_, _ = h.s.DB.Exec(`INSERT INTO command_history(sandbox_id,command_id,mode,command,status,started_at,record,updated_at,private) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(sandbox_id,command_id) DO UPDATE SET mode=excluded.mode, command=excluded.command, status=excluded.status,
record=CASE WHEN json_extract(command_history.record,'$.outputTruncated')=1 THEN json_set(excluded.record,'$.outputTruncated',json('true')) ELSE excluded.record END,
updated_at=excluded.updated_at, private=MAX(command_history.private, excluded.private)`,
			sbID, r.CommandID, r.Mode, r.Command, r.Status, started, mustJSON(r), now, boolInt(privateAt(started+skewIf(!r.StartedAt.IsZero(), skew))))
	}
	wallet := ""
	if sb, err := h.s.SandboxByID(sbID); err == nil {
		wallet = sb.Wallet()
	}
	truncated := map[string]bool{}
	for _, c := range ex.Chunks {
		if c.CommandID == "" || (c.Stream != "stdout" && c.Stream != "stderr") || len(c.Data) == 0 {
			continue
		}
		var exists int
		_ = h.s.DB.QueryRow(`SELECT COUNT(*) FROM command_logs WHERE sandbox_id=? AND command_id=? AND stream=? AND off=?`, sbID, c.CommandID, c.Stream, c.Offset).Scan(&exists)
		if exists == 0 && !h.s.takeOutputQuota(wallet, h.cmdMode(sbID, c.CommandID), len(c.Data)) {
			truncated[c.CommandID] = true
			continue
		}
		_, _ = h.s.DB.Exec(`INSERT OR REPLACE INTO command_logs(sandbox_id,command_id,stream,off,data) VALUES(?,?,?,?,?)`, sbID, c.CommandID, c.Stream, c.Offset, c.Data)
	}
	for id := range truncated {
		_, _ = h.s.DB.Exec(`UPDATE command_history SET record=json_set(record,'$.outputTruncated',json('true')) WHERE sandbox_id=? AND command_id=?`, sbID, id)
	}
	for _, smp := range ex.Samples {
		if smp.Time.IsZero() {
			continue
		}
		_, _ = h.s.DB.Exec(`INSERT OR REPLACE INTO process_samples(sandbox_id,sampled_at,data) VALUES(?,?,?)`, sbID, store.T(smp.Time), mustJSON(smp))
		if smp.DiskTotalBytes > 0 {
			h.s.onDiskUsage(sbID, smp.DiskUsedBytes, smp.DiskTotalBytes)
		}
	}
}

// cmdMode is the stored mode of a command ("" = unknown).
func (h *histState) cmdMode(sbID, cmdID string) string {
	var m string
	_ = h.s.DB.QueryRow(`SELECT mode FROM command_history WHERE sandbox_id=? AND command_id=?`, sbID, cmdID).Scan(&m)
	return m
}

// DefaultOutputCapPerHour is the stored output per wallet and hour, for
// commands and (separately) other processes (CMD-020, Boat: 10 MiB/h).
const DefaultOutputCapPerHour = 10 << 20

// takeOutputQuota books n bytes of stored output for wallet in this hour
// and reports whether they fit (Config.OutputCapPerHour; <0 = no cap).
// The cap counts the bytes the coordinator keeps (Boat counts compressed).
func (s *Server) takeOutputQuota(wallet, mode string, n int) bool {
	limit := s.Cfg.OutputCapPerHour
	if limit == 0 {
		limit = DefaultOutputCapPerHour
	}
	if limit < 0 || wallet == "" {
		return true
	}
	kind := "commands"
	if mode == "process" {
		kind = "processes"
	}
	hour := time.Now().Unix() / 3600
	var used int64
	_ = s.DB.QueryRow(`SELECT bytes FROM output_usage WHERE wallet_id=? AND kind=? AND hour=?`, wallet, kind, hour).Scan(&used)
	if used+int64(n) > limit {
		return false
	}
	_, _ = s.DB.Exec(`INSERT INTO output_usage(wallet_id,kind,hour,bytes) VALUES(?,?,?,?) ON CONFLICT(wallet_id,kind,hour) DO UPDATE SET bytes=bytes+excluded.bytes`, wallet, kind, hour, n)
	_, _ = s.DB.Exec(`DELETE FROM output_usage WHERE hour<?`, hour-24)
	return true
}

// historyFlush pulls the last output before the machine goes away.
func (s *Server) historyFlush(ctx context.Context, sb *Sandbox) {
	if s.hist == nil || !sb.Monitoring {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.hist.pull(cctx, sb, true); err != nil {
		s.Log.Warn("history flush before stop failed", "sandbox", sb.ID, "err", err)
	}
}

func (h *histState) onEvent(e Event) {
	if e.Type != EvSandboxDeleted || e.SandboxID == "" {
		return
	}
	id := e.SandboxID
	h.s.Go(func(ctx context.Context) {
		for _, t := range []string{"command_history", "command_logs", "process_samples", "agent_events"} {
			_, _ = h.s.DB.Exec(`DELETE FROM `+t+` WHERE sandbox_id=?`, id)
		}
	})
}

func (h *histState) prune() {
	now := time.Now()
	_, _ = h.s.DB.Exec(`DELETE FROM command_logs WHERE (sandbox_id, command_id) IN (SELECT sandbox_id, command_id FROM command_history WHERE (mode='process' AND started_at<?) OR started_at<?)`,
		store.T(now.Add(-histKeepProcs)), store.T(now.Add(-histKeepCmds)))
	_, _ = h.s.DB.Exec(`DELETE FROM command_history WHERE (mode='process' AND started_at<?) OR started_at<?`, store.T(now.Add(-histKeepProcs)), store.T(now.Add(-histKeepCmds)))
	_, _ = h.s.DB.Exec(`DELETE FROM process_samples WHERE sampled_at<?`, store.T(now.Add(-histKeepSamples)))
}

// storeAgentEvent keeps a prompt.event (upsert by id: the guest re-emits an
// event when its overlay changes).
func (s *Server) storeAgentEvent(sbID string, raw json.RawMessage) {
	var e struct {
		ID             string `json:"id"`
		Type           string `json:"type"`
		Timestamp      int64  `json:"timestamp"`
		TaskID         string `json:"taskId"`
		ConversationID string `json:"conversationId"`
	}
	if json.Unmarshal(raw, &e) != nil || e.ID == "" || e.Type == "" {
		return
	}
	if e.Timestamp == 0 {
		e.Timestamp = store.Now()
	}
	_, _ = s.DB.Exec(`INSERT INTO agent_events(sandbox_id,event_id,ts,type,conversation_id,task_id,data) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(sandbox_id,event_id) DO UPDATE SET data=excluded.data`, sbID, e.ID, e.Timestamp, e.Type, e.ConversationID, e.TaskID, string(raw))
}

// ---------------------------------------------------------------------------
// Reads while the sandbox does not run
// ---------------------------------------------------------------------------

// storedRoute reports whether GET <rest> can be answered from the store.
func storedRoute(rest string) bool {
	first, _, _ := strings.Cut(rest, "/")
	switch first {
	case "command-history", "processes", "events", "conversations":
		return true
	case "prompts":
		return strings.Count(rest, "/") == 1
	}
	return false
}

// historyTeammate: a member reading another member's org sandbox; history
// recorded while it was private is hidden from them.
func historyTeammate(c *Ctx, sb *Sandbox) bool {
	return sb.OrgID != "" && c.P.User.ID != sb.OwnerID
}

func (s *Server) serveStored(c *Ctx, sb *Sandbox, rest string) error {
	parts := strings.Split(rest, "/")
	if parts[0] == "command-history" && len(parts) >= 2 && historyTeammate(c, sb) {
		var priv int
		if s.DB.QueryRow(`SELECT private FROM command_history WHERE sandbox_id=? AND command_id=?`, sb.ID, parts[1]).Scan(&priv) == nil && priv == 1 {
			return Err(404, "command_not_found", "No command "+parts[1]+" in this sandbox's history.")
		}
	}
	switch {
	case parts[0] == "command-history" && len(parts) == 1:
		return s.storedHistoryList(c, sb)
	case parts[0] == "command-history" && len(parts) == 2:
		rec, err := s.storedRecord(sb.ID, parts[1])
		if err != nil {
			return err
		}
		return c.OK(200, "command.info", map[string]any{"command": rec})
	case parts[0] == "command-history" && len(parts) == 3 && parts[2] == "logs":
		return s.storedLogs(c, sb, parts[1])
	case parts[0] == "processes" && len(parts) == 1:
		return s.storedProcesses(c, sb)
	case parts[0] == "events" && len(parts) == 1:
		return s.storedEvents(c, sb)
	case parts[0] == "conversations" && len(parts) == 1:
		return s.storedConversations(c, sb)
	case parts[0] == "prompts" && len(parts) == 2:
		return s.storedPromptRun(c, sb, parts[1])
	}
	return errNotFound("Route")
}

func (s *Server) storedRecord(sbID, cmdID string) (json.RawMessage, error) {
	var rec string
	if s.DB.QueryRow(`SELECT record FROM command_history WHERE sandbox_id=? AND command_id=?`, sbID, cmdID).Scan(&rec) != nil {
		return nil, Err(404, "command_not_found", "No command "+cmdID+" in this sandbox's history.")
	}
	return json.RawMessage(rec), nil
}

func (s *Server) storedHistoryList(c *Ctx, sb *Sandbox) error {
	modes := map[string]bool{}
	for _, m := range strings.Split(c.Query("mode"), ",") {
		switch m = strings.TrimSpace(m); m {
		case "":
		case "commands":
			modes["sync"], modes["stream"], modes["detached"] = true, true, true
		case "sync", "stream", "detached", "process":
			modes[m] = true
		default:
			return Err(400, "invalid_mode", "unknown mode: "+m)
		}
	}
	limit := 20
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return Err(400, "invalid_limit", "limit must be in 1-100")
		}
		limit = n
	}
	qs := strings.ToLower(c.Query("q"))
	if len(qs) > 200 {
		return Err(400, "invalid_query", "q is at most 200 characters")
	}
	sqlq := `SELECT command_id,mode,command,started_at,record FROM command_history WHERE sandbox_id=?`
	args := []any{sb.ID}
	if historyTeammate(c, sb) {
		sqlq += ` AND private=0`
	}
	if cur := c.Query("cursor"); cur != "" {
		var st int64
		if s.DB.QueryRow(`SELECT started_at FROM command_history WHERE sandbox_id=? AND command_id=?`, sb.ID, cur).Scan(&st) == nil {
			sqlq += ` AND (started_at<? OR (started_at=? AND command_id<?))`
			args = append(args, st, st, cur)
		} else {
			return Err(400, "invalid_cursor", "cursor is not a valid command history cursor")
		}
	}
	rows, err := s.DB.Query(sqlq+` ORDER BY started_at DESC, command_id DESC`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	var next any
	var last string
	for rows.Next() {
		var id, mode, cmd, rec string
		var st int64
		if rows.Scan(&id, &mode, &cmd, &st, &rec) != nil {
			continue
		}
		if len(modes) > 0 && !modes[mode] {
			continue
		}
		if qs != "" && !strings.Contains(strings.ToLower(cmd), qs) {
			continue
		}
		if len(out) == limit {
			next = last
			break
		}
		out = append(out, json.RawMessage(rec))
		last = id
	}
	return c.OK(200, "command.list", map[string]any{"commands": out, "nextCursor": next, "live": false})
}

func (s *Server) storedLogs(c *Ctx, sb *Sandbox, cmdID string) error {
	rec, err := s.storedRecord(sb.ID, cmdID)
	if err != nil {
		return err
	}
	stream := firstNonEmpty(c.Query("stream"), "stdout")
	if stream != "stdout" && stream != "stderr" {
		return Err(400, "invalid_stream", "stream must be stdout or stderr")
	}
	enc := firstNonEmpty(c.Query("encoding"), "utf8")
	if enc != "utf8" && enc != "base64" {
		return Err(400, "invalid_encoding", "encoding must be utf8 or base64")
	}
	off, limit := int64(0), int64(histMaxLogRead)
	if v := c.Query("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return Err(400, "invalid_range", "offset must be >= 0 and limit in 1-1048576")
		}
		off = n
	}
	if v := c.Query("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > histMaxLogRead {
			return Err(400, "invalid_range", "offset must be >= 0 and limit in 1-1048576")
		}
		limit = n
	}
	var size int64
	_ = s.DB.QueryRow(`SELECT COALESCE(MAX(off+length(data)),0) FROM command_logs WHERE sandbox_id=? AND command_id=? AND stream=?`, sb.ID, cmdID, stream).Scan(&size)
	if off > size {
		off = size
	}
	end := min(off+limit, size)
	buf := make([]byte, 0, end-off)
	rows, err := s.DB.Query(`SELECT off,data FROM command_logs WHERE sandbox_id=? AND command_id=? AND stream=? AND off<? AND off+length(data)>? ORDER BY off`, sb.ID, cmdID, stream, end, off)
	if err != nil {
		return err
	}
	pos := off
	for rows.Next() {
		var o int64
		var d []byte
		if rows.Scan(&o, &d) != nil || o > pos {
			break // a gap: return what is contiguous
		}
		d = d[pos-o:]
		if n := end - pos; int64(len(d)) > n {
			d = d[:n]
		}
		buf = append(buf, d...)
		pos += int64(len(d))
		if pos >= end {
			break
		}
	}
	rows.Close()
	var r struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec, &r)
	next := off + int64(len(buf))
	data := string(buf)
	if enc == "base64" {
		data = base64.StdEncoding.EncodeToString(buf)
	}
	return c.OK(200, "command.logs", map[string]any{"commandId": cmdID, "stream": stream, "offset": off, "nextOffset": next,
		"size": size, "encoding": enc, "data": data, "done": r.Status != "running" && next >= size})
}

func (s *Server) storedProcesses(c *Ctx, sb *Sandbox) error {
	var raw string
	if s.DB.QueryRow(`SELECT data FROM process_samples WHERE sandbox_id=? ORDER BY sampled_at DESC LIMIT 1`, sb.ID).Scan(&raw) != nil {
		return c.OK(200, "process.list", map[string]any{"live": false, "sampledAt": nil, "total": 0, "processes": []any{}})
	}
	var smp proto.GuestSample
	_ = json.Unmarshal([]byte(raw), &smp)
	procs := smp.Processes
	if procs == nil {
		procs = []proto.ProcessInfo{}
	}
	return c.OK(200, "process.list", map[string]any{"live": false, "sampledAt": ts(smp.Time), "total": len(procs), "processes": procs,
		"cpuUtilization": smp.CPUUtilization, "memoryUsedBytes": smp.MemoryUsedBytes, "memoryTotalBytes": smp.MemoryTotalBytes,
		"diskUsedBytes": smp.DiskUsedBytes, "diskTotalBytes": smp.DiskTotalBytes})
}

// ---------------------------------------------------------------------------
// Agent events, conversations and prompt runs (AGT-013)
// ---------------------------------------------------------------------------

type storedEvent struct {
	ID, Type, ConversationID, TaskID string
	TS                               int64
	Data                             map[string]any
}

func (s *Server) agentEvents(sbID string) []*storedEvent {
	rows, err := s.DB.Query(`SELECT event_id,type,conversation_id,task_id,ts,data FROM agent_events WHERE sandbox_id=? ORDER BY ts, event_id`, sbID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*storedEvent
	status := map[string]map[string]any{} // prompt id -> latest prompt.status data
	for rows.Next() {
		e := &storedEvent{}
		var raw string
		if rows.Scan(&e.ID, &e.Type, &e.ConversationID, &e.TaskID, &e.TS, &raw) != nil {
			continue
		}
		var full struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal([]byte(raw), &full)
		e.Data = full.Data
		if e.Data == nil {
			e.Data = map[string]any{}
		}
		if e.Type == "prompt.status" && e.TaskID != "" {
			status[e.TaskID] = e.Data
		}
		out = append(out, e)
	}
	// Overlay the current prompt status, as the guest's view does.
	for _, e := range out {
		if e.Type != "prompt" {
			continue
		}
		if st := status[e.TaskID]; st != nil {
			d := map[string]any{}
			for k, v := range e.Data {
				d[k] = v
			}
			if v, ok := st["status"]; ok {
				d["status"] = v
			}
			if v, ok := st["error"]; ok {
				d["error"] = v
			}
			e.Data = d
		}
		if st, _ := e.Data["status"].(string); st == "queued" || st == "running" || st == "sending" {
			d := map[string]any{}
			for k, v := range e.Data {
				d[k] = v
			}
			d["status"], d["error"] = "interrupted", "the sandbox stopped while this prompt was "+st
			e.Data = d
		}
	}
	return out
}

func (e *storedEvent) json() map[string]any {
	m := map[string]any{"id": e.ID, "type": e.Type, "timestamp": e.TS, "data": e.Data}
	if e.TaskID != "" {
		m["taskId"] = e.TaskID
	}
	if e.ConversationID != "" {
		m["conversationId"] = e.ConversationID
	}
	return m
}

func eventCursor(e *storedEvent) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(e.TS, 10) + ":" + e.ID))
}

func (s *Server) storedEvents(c *Ctx, sb *Sandbox) error {
	q := c.R.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return Err(400, "invalid_limit", "limit must be an integer from 1 to 200.")
		}
		limit = n
	}
	asc := false
	switch q.Get("sort") {
	case "", "desc":
	case "asc":
		asc = true
	default:
		return Err(400, "invalid_sort", "sort must be asc or desc.")
	}
	types, convs := splitCSV(q["type"]), splitCSV(append(q["conversation"], q["convo"]...))
	all := s.agentEvents(sb.ID)
	var curTS int64
	var curID string
	if cur := q.Get("cursor"); cur != "" {
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(cur, "="))
		t, id, ok := strings.Cut(string(b), ":")
		n, err2 := strconv.ParseInt(t, 10, 64)
		if err != nil || !ok || err2 != nil {
			return Err(400, "invalid_cursor", "cursor is not a valid events cursor")
		}
		curTS, curID = n, id
	}
	if !asc {
		sort.SliceStable(all, func(i, j int) bool { return all[i].TS > all[j].TS || (all[i].TS == all[j].TS && all[i].ID > all[j].ID) })
	}
	out := []map[string]any{}
	more := false
	var last *storedEvent
	for _, e := range all {
		if curID != "" {
			after := e.TS > curTS || (e.TS == curTS && e.ID > curID)
			before := e.TS < curTS || (e.TS == curTS && e.ID < curID)
			if (asc && !after) || (!asc && !before) {
				continue
			}
		}
		if (len(types) > 0 && !contains(types, e.Type)) || (len(convs) > 0 && !contains(convs, e.ConversationID)) {
			continue
		}
		if len(out) == limit {
			more = true
			break
		}
		out = append(out, e.json())
		last = e
	}
	pi := map[string]any{"nextCursor": nil, "hasMore": more, "limit": limit}
	if more && last != nil {
		pi["nextCursor"] = eventCursor(last)
	}
	return c.OK(200, "events.list", map[string]any{"id": sb.ID, "events": out, "pageInfo": pi, "live": false})
}

func (s *Server) storedConversations(c *Ctx, sb *Sandbox) error {
	type conv struct {
		id                   string
		created, lastPrompt  int64
		prompts              int
		harness, model, prev string
	}
	convs := map[string]*conv{}
	var lastConv string
	var lastTS int64
	for _, e := range s.agentEvents(sb.ID) {
		if e.ConversationID == "" {
			continue
		}
		cv := convs[e.ConversationID]
		if cv == nil {
			cv = &conv{id: e.ConversationID, created: e.TS}
			convs[e.ConversationID] = cv
		}
		if e.Type == "prompt" {
			cv.prompts++
			cv.lastPrompt = e.TS
			if v, _ := e.Data["provider"].(string); v != "" {
				cv.harness = v
			}
			if v, _ := e.Data["model"].(string); v != "" {
				cv.model = v
			}
			if v, _ := e.Data["prompt"].(string); v != "" {
				r := []rune(v)
				if len(r) > 80 {
					r = r[:80]
				}
				cv.prev = string(r)
			}
			if e.TS >= lastTS {
				lastTS, lastConv = e.TS, e.ConversationID
			}
		}
	}
	list := make([]*conv, 0, len(convs))
	for _, cv := range convs {
		list = append(list, cv)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].lastPrompt > list[j].lastPrompt })
	out := []map[string]any{}
	for _, cv := range list {
		out = append(out, map[string]any{"id": cv.id, "createdAt": tsMs(cv.created), "lastPromptAt": tsMs(cv.lastPrompt), "prompts": cv.prompts,
			"running": false, "lastHarness": nz(cv.harness), "lastModel": nz(cv.model), "lastPromptPreview": nz(cv.prev), "current": cv.id == lastConv})
	}
	return c.OK(200, "conversation.list", map[string]any{"id": sb.ID, "conversations": out, "live": false})
}

func (s *Server) storedPromptRun(c *Ctx, sb *Sandbox, promptID string) error {
	var run map[string]any
	for _, e := range s.agentEvents(sb.ID) {
		switch {
		case e.Type == "prompt" && e.TaskID == promptID:
			st, _ := e.Data["status"].(string)
			run = map[string]any{"id": promptID, "promptId": promptID, "sandboxId": sb.ID, "status": st,
				"done": st != "queued" && st != "running" && st != "sending", "createdAt": tsMs(e.TS),
				"model": e.Data["model"], "reasoningEffort": e.Data["reasoningEffort"], "fast": e.Data["fast"] == true,
				"conversationId": e.ConversationID, "provider": e.Data["provider"]}
			if v, ok := e.Data["error"]; ok {
				run["error"] = v
			}
		case run != nil && e.Type == "prompt.status" && e.TaskID == promptID:
			switch e.Data["status"] {
			case "running":
				run["startedAt"] = tsMs(e.TS)
			case "queued", "sending":
			default:
				run["finishedAt"] = tsMs(e.TS)
			}
		}
	}
	if run == nil {
		return Err(404, "prompt_not_found", "No prompt "+promptID+" in this sandbox.")
	}
	return c.OK(200, "prompt.run", map[string]any{"id": sb.ID, "promptRun": run})
}

func splitCSV(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// GET /sandboxes/{id}/metrics (DASH-004)
// ---------------------------------------------------------------------------

const (
	metricsMaxPoints = 1000
	metricsMinStep   = 30 * time.Second // the guest samples every 30 s
)

// handleSandboxMetrics returns the stored 30 s samples as a time series:
// ?from=&to= (RFC 3339 or unix seconds; default the last hour, at most the
// 7-day retention) and ?step= (seconds or a Go duration; default so that
// at most 360 points come back, never under 30 s). Each point averages
// the samples in [at, at+step); totals are the bucket's maximum. Buckets
// without samples are left out (the sandbox did not run).
func (s *Server) handleSandboxMetrics(c *Ctx) error {
	sb, err := c.Sandbox("sandbox.read", false)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	to, from := now, time.Time{}
	if v := c.Query("to"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return Err(400, "invalid_range", "to must be RFC 3339 or unix seconds.")
		}
		to = t.UTC()
	}
	from = to.Add(-time.Hour)
	if v := c.Query("from"); v != "" {
		t, ok := parseTime(v)
		if !ok {
			return Err(400, "invalid_range", "from must be RFC 3339 or unix seconds.")
		}
		from = t.UTC()
	}
	if oldest := now.Add(-histKeepSamples); from.Before(oldest) {
		from = oldest
	}
	if !from.Before(to) {
		return Err(400, "invalid_range", "from must be before to.")
	}
	span := to.Sub(from)
	step := max(metricsMinStep, (span / 360).Round(time.Second))
	if v := c.Query("step"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			n, err2 := strconv.Atoi(v)
			if err2 != nil {
				return Err(400, "invalid_step", "step must be seconds or a duration like 5m.")
			}
			d = time.Duration(n) * time.Second
		}
		if d < metricsMinStep {
			return ErrD(400, "invalid_step", "step must be at least 30 seconds (the sampling interval).", map[string]any{"minStepSeconds": 30})
		}
		if (span+d-1)/d > metricsMaxPoints {
			return ErrD(400, "invalid_step", fmt.Sprintf("step is too small for this range: at most %d points.", metricsMaxPoints), map[string]any{"maxPoints": metricsMaxPoints})
		}
		step = d
	}
	rows, err := s.DB.Query(`SELECT sampled_at, data FROM process_samples WHERE sandbox_id=? AND sampled_at>=? AND sampled_at<? ORDER BY sampled_at`,
		sb.ID, store.T(from), store.T(to))
	if err != nil {
		return err
	}
	defer rows.Close()
	type bucket struct {
		at                  time.Time
		n                   int
		cpu, mem, disk, io  float64
		memTotal, diskTotal int64
	}
	var points []*bucket
	for rows.Next() {
		var at int64
		var raw string
		var smp proto.GuestSample
		if rows.Scan(&at, &raw) != nil || json.Unmarshal([]byte(raw), &smp) != nil {
			continue
		}
		t := store.Time(at)
		start := from.Add(t.Sub(from) / step * step)
		if len(points) == 0 || !points[len(points)-1].at.Equal(start) {
			points = append(points, &bucket{at: start})
		}
		b := points[len(points)-1]
		b.n++
		b.cpu += smp.CPUUtilization
		b.mem += float64(smp.MemoryUsedBytes)
		b.disk += float64(smp.DiskUsedBytes)
		b.io += smp.IOPressure
		b.memTotal, b.diskTotal = max(b.memTotal, smp.MemoryTotalBytes), max(b.diskTotal, smp.DiskTotalBytes)
	}
	out := make([]map[string]any, 0, len(points))
	for _, b := range points {
		n := float64(b.n)
		out = append(out, map[string]any{"at": ts(b.at), "samples": b.n,
			"cpuUtilization": round6(b.cpu / n), "memoryUsedBytes": int64(b.mem / n), "memoryTotalBytes": b.memTotal,
			"diskUsedBytes": int64(b.disk / n), "diskTotalBytes": b.diskTotal, "ioPressure": round6(b.io / n)})
	}
	return c.OK(200, "sandbox.metrics", map[string]any{"id": sb.ID, "from": ts(from), "to": ts(to), "stepSeconds": int(step / time.Second),
		"intervalSeconds": int(metricsMinStep / time.Second), "live": sb.Usable(), "points": out})
}
