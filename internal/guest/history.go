package guest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
	"github.com/crowdvm/crowdvm/internal/proto"
)

// CommandRecord is Boat's CommandRecord (shared with the server via proto).
type CommandRecord = proto.CommandRecord

// meta is what is persisted per command (record + agent-private fields).
type meta struct {
	CommandRecord
	BootID   string `json:"bootId,omitempty"`
	PID      int    `json:"pid,omitempty"`
	PIDStart uint64 `json:"pidStart,omitempty"` // /proc/<pid>/stat starttime, guards pid reuse

	// export state (history/export + ack)
	Rev    int      `json:"rev,omitempty"`    // bumped on every record change
	AckRev int      `json:"ackRev,omitempty"` // last revision the server acked
	AckOut [2]int64 `json:"ackOut,omitempty"` // stdout/stderr bytes the server acked

	lastExport time.Time // output last exported (cadence)
	exitSent   bool      // an attach follower got the exit frame (makeRoom)
}

// Retention (CMD-015): commands 90 days, other processes 72 hours; with
// monitoring off a finished record is dropped once nobody can still be
// reading its live output.
const (
	keepCommands     = 90 * 24 * time.Hour
	keepProcesses    = 72 * time.Hour
	keepUnmonitored  = 10 * time.Minute
	exportStreamWait = 2 * time.Second
	exportOtherWait  = 30 * time.Second
	exportChunkMax   = 1 << 20
)

// history persists command records and output under <dir>/<id>/
// {meta.json,stdout,stderr}. Monitored: on the guest disk (survives
// stop/resume/fork). Monitoring off: on tmpfs (/run), so nothing outlives the
// boot, and finished records are pruned after keepUnmonitored.
type history struct {
	dir       string
	unmonitor bool
	mu        sync.Mutex
	m         map[string]*meta
	a         *Agent

	batches map[string]exportBatch // pending export batches (not acked yet)
	order   []string
}

type exportBatch struct {
	revs    map[string]int
	out     map[string][2]int64
	samples int64 // last sample seq included
}

func openHistory(dir string, a *Agent, unmonitored bool) (*history, error) {
	h := &history{dir: dir, unmonitor: unmonitored, m: map[string]*meta{}, a: a, batches: map[string]exportBatch{}}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "meta.json"))
		if err != nil {
			continue
		}
		var m meta
		if json.Unmarshal(b, &m) != nil || m.CommandID != e.Name() {
			continue
		}
		h.m[m.CommandID] = &m
	}
	return h, nil
}

// wipe deletes every record with its output (a scrub: the sandbox's disk
// came from another account, bugs-r8-opus 4). Running commands keep theirs.
func (h *history) wipe() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, m := range h.m {
		if m.Status == "running" {
			continue
		}
		_ = os.RemoveAll(filepath.Join(h.dir, id))
		delete(h.m, id)
	}
	h.batches = map[string]exportBatch{}
	h.order = nil
}

func (h *history) path(id, stream string) string { return filepath.Join(h.dir, id, stream) }

func (h *history) create(m *meta) error {
	if err := os.MkdirAll(filepath.Join(h.dir, m.CommandID), 0o755); err != nil {
		return err
	}
	h.mu.Lock()
	m.Rev++
	h.m[m.CommandID] = m
	h.mu.Unlock()
	return h.save(m)
}

// save persists m (callers mutate m only under h.mu via update).
func (h *history) save(m *meta) error {
	// Marshal and write under the lock: an older marshal can never land after a
	// newer one, and each write has its own temp file.
	h.mu.Lock()
	defer h.mu.Unlock()
	b, _ := json.Marshal(m)
	p := filepath.Join(h.dir, m.CommandID, "meta.json")
	f, err := os.CreateTemp(filepath.Dir(p), "meta.json.*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func (h *history) update(m *meta, f func(*meta)) {
	h.mu.Lock()
	f(m)
	m.Rev++
	h.mu.Unlock()
	if err := h.save(m); err != nil {
		h.a.log.Printf("history: save %s: %v", m.CommandID, err)
	}
}

func (h *history) get(id string) *meta {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[id]
}

// record returns a snapshot of the public record with live byte counts.
func (h *history) record(m *meta) CommandRecord {
	h.mu.Lock()
	r := m.CommandRecord
	h.mu.Unlock()
	r.StdoutBytes = fileSize(h.path(r.CommandID, "stdout"))
	r.StderrBytes = fileSize(h.path(r.CommandID, "stderr"))
	return r
}

// byProcessID returns the newest detached record with this process id.
func (h *history) byProcessID(pid int) *meta {
	h.mu.Lock()
	defer h.mu.Unlock()
	var best *meta
	for _, m := range h.m {
		if m.Mode == "detached" && m.ProcessID != nil && *m.ProcessID == pid {
			if best == nil || m.StartedAt.After(best.StartedAt) {
				best = m
			}
		}
	}
	return best
}

func (h *history) all() []*meta {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*meta, 0, len(h.m))
	for _, m := range h.m {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].CommandID > out[j].CommandID
	})
	return out
}

// list implements filtering and cursor pagination (cursor = last commandId).
// ok is false for a cursor that names no record (400 invalid_cursor, like
// the server's history, bugs-r9-sonnet).
func (h *history) list(modes map[string]bool, q, cursor string, limit int) (out []CommandRecord, next string, ok bool) {
	q = strings.ToLower(q)
	all := h.all()
	start := 0
	if cursor != "" {
		start = -1
		for i, m := range all {
			if m.CommandID == cursor {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, "", false
		}
	}
	for _, m := range all[start:] {
		if len(modes) > 0 && !modes[m.Mode] {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(m.Command), q) {
			continue
		}
		if len(out) == limit {
			next = out[len(out)-1].CommandID
			break
		}
		out = append(out, h.record(m))
	}
	return out, next, true
}

// prune deletes records past their retention (never running ones).
func (h *history) prune(now time.Time) int {
	var drop []string
	h.mu.Lock()
	for id, m := range h.m {
		if m.Status == "running" || m.FinishedAt == nil {
			continue
		}
		keep := keepCommands
		switch {
		case h.unmonitor:
			keep = keepUnmonitored
		case m.Mode == "process":
			keep = keepProcesses
		}
		if now.Sub(*m.FinishedAt) > keep {
			drop = append(drop, id)
			delete(h.m, id)
		}
	}
	h.mu.Unlock()
	h.dropFiles(drop)
	return len(drop)
}

// dropFiles deletes the output of records already removed from h.m.
func (h *history) dropFiles(drop []string) {
	for _, id := range drop {
		_ = os.RemoveAll(filepath.Join(h.dir, id))
	}
	if len(drop) > 0 && h.a != nil {
		gone := map[string]bool{}
		for _, id := range drop {
			gone[id] = true
		}
		h.a.mu.Lock()
		for pid, p := range h.a.procs {
			if gone[p.m.CommandID] {
				delete(h.a.procs, pid)
			}
		}
		h.a.mu.Unlock()
	}
}

// freeShare: the free share (0..1) of the file system holding h (1 when
// unknown).
func (h *history) freeShare() float64 { return fsFreeShare(h.dir) }

// fsFreeShare is replaced in tests (a full tmpfs needs root).
var fsFreeShare = func(dir string) float64 {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil || st.Blocks == 0 {
		return 1
	}
	return float64(st.Bavail) / float64(st.Blocks)
}

// unfollowedGrace: how long makeRoom keeps a finished record whose exit no
// follower has read yet (the CLI attaches right after the start).
const unfollowedGrace = 2 * time.Minute

// makeRoom (monitoring off: output lives on the RAM disk /run): when less
// than a quarter is free, finished records go, oldest first, until 40% is
// free, so their 10-minute grace cannot fill it (bugs-r9-opus 3). Running
// commands keep theirs, and so do commands that just finished and whose
// follower has not read the exit yet (bugs-r10-opus 6).
func (h *history) makeRoom() {
	if !h.unmonitor || h.freeShare() >= 0.25 {
		return
	}
	h.mu.Lock()
	var done []*meta
	for _, m := range h.m {
		if m.Status != "running" && m.FinishedAt != nil && (m.exitSent || time.Since(*m.FinishedAt) >= unfollowedGrace) {
			done = append(done, m)
		}
	}
	h.mu.Unlock()
	sort.Slice(done, func(i, j int) bool { return done[i].FinishedAt.Before(*done[j].FinishedAt) })
	for _, m := range done {
		if h.freeShare() >= 0.4 {
			return
		}
		h.mu.Lock()
		delete(h.m, m.CommandID)
		h.mu.Unlock()
		h.dropFiles([]string{m.CommandID})
	}
}

// ---------------------------------------------------------------------------
// Export to the coordinator (CMD-017/018/019/025/027): pull + ack
// ---------------------------------------------------------------------------

// export collects what the server has not acked yet: changed records, new
// output due under Boat's cadence (or all of it with flush) and samples.
func (h *history) export(flush bool, maxBytes int64, samples []sampleRec) proto.HistoryExport {
	now := time.Now()
	ex := proto.HistoryExport{Records: []CommandRecord{}, Chunks: []proto.HistoryChunk{}, Samples: []proto.GuestSample{}, Now: time.Now()}
	if h.unmonitor {
		return ex
	}
	b := exportBatch{revs: map[string]int{}, out: map[string][2]int64{}}
	budget := maxBytes
	all := h.all()
	for i := len(all) - 1; i >= 0; i-- { // oldest first
		m := all[i]
		h.mu.Lock()
		rev, ackRev, ackOut, mode, status, last := m.Rev, m.AckRev, m.AckOut, m.Mode, m.Status, m.lastExport
		h.mu.Unlock()
		running := status == "running"
		due := flush || !running
		switch mode {
		case "stream":
			due = due || now.Sub(last) >= exportStreamWait
		case "detached", "process":
			due = due || now.Sub(last) >= exportOtherWait
		}
		end := ackOut
		sent := false
		if due {
			for si, stream := range []string{"stdout", "stderr"} {
				p := h.path(m.CommandID, stream)
				size := fileSize(p)
				if mode == "sync" && size > maxSyncOutput {
					size = maxSyncOutput
				}
				for end[si] < size && budget > 0 {
					n := min(size-end[si], exportChunkMax, budget)
					data := readAt(p, end[si], n)
					if len(data) == 0 {
						break
					}
					ex.Chunks = append(ex.Chunks, proto.HistoryChunk{CommandID: m.CommandID, Stream: stream, Offset: end[si], Data: data})
					end[si] += int64(len(data))
					budget -= int64(len(data))
					sent = true
				}
				if end[si] < size {
					ex.More = true
				}
			}
		}
		if sent || rev != ackRev {
			ex.Records = append(ex.Records, h.record(m))
			b.revs[m.CommandID] = rev
			b.out[m.CommandID] = end
		}
		if sent {
			h.mu.Lock()
			m.lastExport = now
			h.mu.Unlock()
		}
		if budget <= 0 || len(ex.Records) >= 500 {
			ex.More = true
			break
		}
	}
	for _, s := range samples {
		ex.Samples = append(ex.Samples, s.GuestSample)
		b.samples = s.Seq
	}
	if len(ex.Records) == 0 && len(ex.Samples) == 0 {
		return ex
	}
	ex.Batch = api.NewID("hb_", 12)
	h.mu.Lock()
	h.batches[ex.Batch] = b
	h.order = append(h.order, ex.Batch)
	for len(h.order) > 16 { // the server acks right away; drop stale batches
		delete(h.batches, h.order[0])
		h.order = h.order[1:]
	}
	h.mu.Unlock()
	return ex
}

// ack applies an export batch. Returns the last acked sample seq (0 = none)
// and false for an unknown batch.
func (h *history) ack(batch string) (int64, bool) {
	h.mu.Lock()
	b, ok := h.batches[batch]
	delete(h.batches, batch)
	var changed []*meta
	if ok {
		for id, rev := range b.revs {
			m := h.m[id]
			if m == nil {
				continue
			}
			m.AckRev = max(m.AckRev, rev)
			out := b.out[id]
			for i := range out {
				m.AckOut[i] = max(m.AckOut[i], out[i])
			}
			changed = append(changed, m)
		}
	}
	h.mu.Unlock()
	for _, m := range changed {
		_ = h.save(m)
	}
	return b.samples, ok
}

// GET /v1/internal/history/export?flush=0|1&maxBytes=N
func (a *Agent) handleHistoryExport(w http.ResponseWriter, r *http.Request) {
	maxBytes, okk := qInt(r, "maxBytes", 4<<20)
	if !okk || maxBytes < 1024 || maxBytes > 64<<20 {
		writeErr(w, 400, "invalid_max_bytes", "maxBytes must be in 1024-67108864")
		return
	}
	// ZDR (DATA-009): output written after the last regular (30 s) export is
	// not saved, so a flush at stop is ignored.
	flush := qBool(r, "flush") && !a.cfg.ZeroDataRetention
	var samples []sampleRec
	if !a.hist.unmonitor {
		samples = a.samples.pending(200)
	}
	ex := a.hist.export(flush, maxBytes, samples)
	ok(w, "history.export", ex)
}

// POST /v1/internal/history/ack {"batch"}
func (a *Agent) handleHistoryAck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Batch string `json:"batch"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	seq, found := a.hist.ack(req.Batch)
	if !found {
		writeErr(w, 404, "batch_not_found", "unknown or already acked batch")
		return
	}
	if seq > 0 {
		a.samples.ack(seq)
	}
	ok(w, "history.acked", map[string]any{"batch": req.Batch})
}

// monitoringOff answers 409 for history reads when monitoring is off.
func (a *Agent) monitoringOff(w http.ResponseWriter) bool {
	if a.hist.unmonitor {
		writeErr(w, 409, "monitoring_off", "monitoring is off for this sandbox: no command history is kept")
		return true
	}
	return false
}

func (a *Agent) historyJanitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		if a.hist.prune(time.Now()) > 0 {
			// drop ~/.ascii/processes links of pruned commands
			links, _ := filepath.Glob(filepath.Join(a.processLogDir(), "*.log"))
			for _, l := range links {
				if t, err := os.Readlink(l); err == nil && strings.HasPrefix(t, a.hist.dir) {
					if _, err := os.Stat(t); os.IsNotExist(err) {
						_ = os.Remove(l)
					}
				}
			}
		}
	}
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func newCommandID() string { return api.NewID("cmd_", 12) }
