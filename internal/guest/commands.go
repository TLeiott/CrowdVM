package guest

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	defaultTimeout = 30
	maxTimeout     = 600
	maxSyncOutput  = 8 << 20 // per stream, in a synchronous answer
	maxLogRead     = 1 << 20
	maxCommandText = 64 << 10
)

// proc is a command the agent started (or adopted after a restart).
type proc struct {
	m        *meta
	pid      int
	cmd      *exec.Cmd // nil when adopted after an agent restart
	done     chan struct{}
	timedOut atomic.Bool
	killed   atomic.Bool
}

func (p *proc) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

type commandRequest struct {
	Command        string            `json:"command"`
	Cwd            string            `json:"cwd"`
	TimeoutSeconds *int              `json:"timeoutSeconds"`
	Detached       bool              `json:"detached"`
	Stream         bool              `json:"stream"`
	Env            map[string]string `json:"env"`
	// KillAfterSeconds (detached, CrowdVM extension): the sandbox kills the
	// command after this long. The CLI follows a detached command and
	// enforces its own --timeout; this is the backstop for a CLI that died
	// (kill -9) without stopping it (bugs-r9-opus 5).
	KillAfterSeconds int `json:"killAfterSeconds"`
}

// shellCmd builds `bash -lc command` running as the sandbox user with the
// sandbox environment, in its own session/process group.
func (a *Agent) shellCmd(command, dir string, extraEnv map[string]string) *exec.Cmd {
	cmd := exec.Command("/bin/bash", "-lc", command)
	cmd.Dir = dir
	cmd.Env = a.commandEnv(extraEnv)
	cmd.SysProcAttr = a.sysProcAttr()
	return cmd
}

func (a *Agent) sysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setsid: true}
	if !a.unpriv && os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(a.uid), Gid: uint32(a.gid), Groups: a.groups}
	}
	return attr
}

// startCommand starts a command whose stdout/stderr go straight to the
// history log files (so a detached command keeps logging if the agent dies).
func (a *Agent) startCommand(req commandRequest, cwdReal, cwdVirt, mode string) (*proc, error) {
	a.hist.makeRoom()
	text := req.Command
	if len(text) > maxCommandText {
		text = text[:maxCommandText]
	}
	m := &meta{CommandRecord: CommandRecord{
		CommandID: newCommandID(), SandboxID: a.cfg.SandboxID, Command: text, Cwd: ptr(cwdVirt),
		Mode: mode, Status: "running", StartedAt: time.Now().UTC(),
	}, BootID: a.bootID}
	if err := a.hist.create(m); err != nil {
		return nil, err
	}
	fail := func(err error) (*proc, error) {
		a.hist.update(m, func(m *meta) { m.Status = "failed"; m.FinishedAt = ptr(time.Now().UTC()) })
		return nil, err
	}
	// O_APPEND, so output stays at the end when capOutput truncates the file.
	create := func(stream string) (*os.File, error) {
		return os.OpenFile(a.hist.path(m.CommandID, stream), os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o666)
	}
	stdout, err := create("stdout")
	if err != nil {
		return fail(err)
	}
	defer stdout.Close()
	stderr, err := create("stderr")
	if err != nil {
		return fail(err)
	}
	defer stderr.Close()
	cmd := a.shellCmd(req.Command, cwdReal, req.Env)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := a.spawn(cmd, m.CommandID); err != nil {
		return fail(err)
	}
	p := &proc{m: m, pid: cmd.Process.Pid, cmd: cmd, done: make(chan struct{})}
	a.hist.update(m, func(m *meta) {
		if mode == "detached" {
			m.ProcessID = ptr(p.pid)
		}
		m.PID = p.pid
		m.PIDStart = pidStartTime(p.pid)
	})
	if mode == "detached" {
		a.mu.Lock()
		a.procs[p.pid] = p
		a.mu.Unlock()
		a.linkProcessLogs(p.pid, m.CommandID)
	}
	if !a.hist.unmonitor {
		a.Emit(EvCommandStarted, a.hist.record(m))
	}
	go a.capOutput(p)
	go func() {
		_ = cmd.Wait()
		ws, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
		a.finish(p, &ws)
	}()
	return p, nil
}

// A command's stdout and stderr files are capped at Agent.outputCap bytes each
// (a runaway `yes` must not fill the disk, or tmpfs when monitoring is off).
// What follows the cap is dropped. No marker goes into the output itself:
// it would land in whatever the user pipes the output to (bugs-r8-opus 6);
// the record's outputTruncated and the CLI's stderr warning say it instead.
const capOutputInterval = 100 * time.Millisecond

// outputFreezeShare: below this free share of the RAM disk (monitoring off)
// running commands' output stops growing.
const outputFreezeShare = 0.1

// capOutput watches p's output files until the command ends. The files are
// opened with O_APPEND, so truncating them keeps the writer at the end.
func (a *Agent) capOutput(p *proc) {
	t := time.NewTicker(capOutputInterval)
	defer t.Stop()
	marked := false // recorded as truncated (a local: p.m is the history's, under its lock)
	// frozen: with monitoring off, running commands share the RAM disk. When
	// it is nearly full their streams stop growing (and say so), so the
	// guest's own records and short commands still fit (bugs-r10-opus 6).
	frozen := map[string]int64{}
	for done := false; !done; {
		select {
		case <-p.done:
			done = true // one last pass
		case <-t.C:
		}
		for _, s := range []string{"stdout", "stderr"} {
			a.hist.makeRoom()
			path, limit := a.hist.path(p.m.CommandID, s), a.outputLimit()
			full := false // writes may have failed for lack of space
			if a.hist.unmonitor {
				free := a.hist.freeShare()
				_, ok := frozen[s]
				switch {
				case !ok && free < outputFreezeShare:
					frozen[s] = fileSize(path)
				case ok && free >= 0.25: // makeRoom freed it again
					delete(frozen, s)
				}
				if f, ok := frozen[s]; ok {
					limit = min(limit, f)
				}
				full = free < 0.01
			}
			if (capFile(path, limit) || full) && !marked {
				// Recorded, so history and the follower's exit frame say so
				// (bugs-r7-opus 2).
				marked = true
				a.hist.update(p.m, func(m *meta) { m.OutputTruncated = true })
			}
		}
	}
}

// outputLimit: the per-stream cap, never more than an eighth of the file
// system holding the history: with monitoring off that is the guest's RAM
// disk (/run), where 1 GiB does not fit (bugs-r8-opus 3).
func (a *Agent) outputLimit() int64 {
	limit := a.outputCap
	var st syscall.Statfs_t
	if syscall.Statfs(a.hist.dir, &st) == nil {
		if fs := int64(st.Blocks) * int64(st.Bsize) / 8; fs > 0 && fs < limit {
			limit = fs
		}
	}
	return limit
}

// capFile cuts path to limit bytes; true: it was over the limit.
func capFile(path string, limit int64) bool {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= limit {
		return false
	}
	_ = os.Truncate(path, limit)
	return true
}

// finish records the final state. ws is nil for adopted processes.
func (a *Agent) finish(p *proc, ws *syscall.WaitStatus) {
	a.hist.update(p.m, func(m *meta) {
		m.FinishedAt = ptr(time.Now().UTC())
		m.Status = "exited"
		if ws != nil {
			if ws.Signaled() {
				m.Signal = ptr(unix.SignalName(ws.Signal()))
				m.Status = "killed"
			} else {
				m.ExitCode = ptr(ws.ExitStatus())
			}
		}
		// OOM (LIFE-043): the kernel killed a process of this command's
		// cgroup. Boat reports exit 137 + oomKilled.
		if a.oomKills(m.CommandID) > 0 {
			m.OOMKilled = true
			killed9 := ws != nil && ws.Signaled() && ws.Signal() == syscall.SIGKILL
			if killed9 || (m.ExitCode != nil && *m.ExitCode == 137) {
				m.ExitCode, m.Signal = ptr(137), ptr("SIGKILL")
				m.Status = "killed"
			}
		}
		if p.killed.Load() {
			m.Status = "killed"
		}
		if p.timedOut.Load() {
			m.Status = "timed_out"
		}
	})
	close(p.done)
	if !a.hist.unmonitor {
		a.Emit(EvCommandExited, a.hist.record(p.m))
	}
}

// recoverProcs fixes records left "running" by a previous agent process.
// Same boot + still alive: adopt and watch. Otherwise lost / exited.
func (a *Agent) recoverProcs() {
	for _, m := range a.hist.all() {
		if m.Status != "running" {
			continue
		}
		pid := 0
		if m.ProcessID != nil {
			pid = *m.ProcessID
		}
		switch {
		case m.BootID != a.bootID || m.Mode != "detached":
			a.hist.update(m, func(m *meta) { m.Status = "lost"; m.FinishedAt = ptr(time.Now().UTC()) })
		case pid > 0 && pidAlive(pid, m.PIDStart):
			p := &proc{m: m, pid: pid, done: make(chan struct{})}
			a.mu.Lock()
			a.procs[pid] = p
			a.mu.Unlock()
			go a.capOutput(p)
			go func() {
				for pidAlive(pid, m.PIDStart) {
					time.Sleep(500 * time.Millisecond)
				}
				a.finish(p, nil)
			}()
		default:
			a.hist.update(m, func(m *meta) { m.Status = "exited"; m.FinishedAt = ptr(time.Now().UTC()) })
		}
	}
}

// pidStartTime returns field 22 of /proc/<pid>/stat (0 if unknown).
func pidStartTime(pid int) uint64 {
	st, err := readStat(pid)
	if err != nil {
		return 0
	}
	return st.startTime
}

func pidAlive(pid int, start uint64) bool {
	st, err := readStat(pid)
	if err != nil || st.state == "Z" || st.state == "X" {
		return false
	}
	return start == 0 || st.startTime == start
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// POST /v1/commands
func (a *Agent) handleCommand(w http.ResponseWriter, r *http.Request) {
	var req commandRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeErr(w, 400, "invalid_command", "command is required")
		return
	}
	timeout := defaultTimeout
	if req.TimeoutSeconds != nil {
		timeout = *req.TimeoutSeconds
		if timeout < 1 || timeout > maxTimeout {
			writeErr(w, 400, "invalid_timeout", "timeoutSeconds must be an integer in 1-600")
			return
		}
	}
	cwdReal, cwdVirt := a.home, "/home/user"
	if req.Cwd != "" {
		var err error
		if cwdReal, cwdVirt, err = a.resolve(req.Cwd, true); err != nil {
			writeErr(w, 400, "invalid_cwd", err.Error())
			return
		}
		if fi, err := os.Stat(cwdReal); err != nil || !fi.IsDir() {
			writeErr(w, 400, "invalid_cwd", "cwd is not a directory: "+cwdVirt)
			return
		}
	}
	mode := "sync"
	switch {
	case req.Detached:
		mode = "detached"
	case req.Stream:
		mode = "stream"
	}
	p, err := a.startCommand(req, cwdReal, cwdVirt, mode)
	if err != nil {
		if mode == "stream" {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": "command_failed", "message": err.Error(), "retryable": false})
			return
		}
		writeErr(w, 500, "command_failed", "could not start command: "+err.Error())
		return
	}
	if mode == "detached" {
		if req.KillAfterSeconds > 0 {
			time.AfterFunc(time.Duration(req.KillAfterSeconds)*time.Second, func() {
				if !p.finished() {
					p.timedOut.Store(true)
					_ = syscall.Kill(-p.pid, syscall.SIGKILL)
					a.killGroup(p.m.CommandID)
				}
			})
		}
		rec := a.hist.record(p.m)
		ok(w, "command.started", map[string]any{
			"success": true, "commandId": rec.CommandID, "processId": p.pid, "pid": p.pid,
			"command": req.Command, "cwd": cwdVirt, "startedAt": rec.StartedAt,
			"logPath": a.processLog(p.pid, ".log"), "errLogPath": a.processLog(p.pid, ".err.log"),
		})
		return
	}
	timer := time.AfterFunc(time.Duration(timeout)*time.Second, func() {
		if !p.finished() {
			p.timedOut.Store(true)
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		}
	})
	defer func() {
		if p.finished() {
			timer.Stop()
		}
	}()
	if mode == "stream" {
		a.streamCommand(w, r, p)
		return
	}
	<-p.done
	rec := a.hist.record(p.m)
	res := a.resultFields(rec, p.timedOut.Load())
	stdout, otr := readHead(a.hist.path(rec.CommandID, "stdout"), maxSyncOutput)
	stderr, etr := readHead(a.hist.path(rec.CommandID, "stderr"), maxSyncOutput)
	res["stdout"], res["stderr"] = string(stdout), string(stderr)
	res["stdoutTruncated"], res["stderrTruncated"] = otr, etr
	if otr || etr {
		a.hist.update(p.m, func(m *meta) { m.OutputTruncated = true })
	}
	ok(w, "command.finished", res)
}

func (a *Agent) resultFields(rec CommandRecord, timedOut bool) map[string]any {
	return map[string]any{
		"success": rec.ExitCode != nil && *rec.ExitCode == 0, "commandId": rec.CommandID,
		"exitCode": rec.ExitCode, "signal": rec.Signal, "oomKilled": rec.OOMKilled, "timedOut": timedOut,
		"cwd": rec.Cwd, "startedAt": rec.StartedAt, "finishedAt": rec.FinishedAt,
	}
}

// streamCommand writes CommandStreamFrame NDJSON until the command ends.
func (a *Agent) streamCommand(w http.ResponseWriter, r *http.Request, p *proc) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	nd := newNDJSON(w)
	nd.send(map[string]any{"type": "started", "commandId": p.m.CommandID})
	fs := []*follower{{path: a.hist.path(p.m.CommandID, "stdout")}, {path: a.hist.path(p.m.CommandID, "stderr")}}
	names := []string{"stdout", "stderr"}
	t := time.NewTicker(40 * time.Millisecond)
	defer t.Stop()
	for {
		final := p.finished()
		for i, f := range fs {
			for {
				b := f.next(final, 256<<10)
				if len(b) == 0 {
					break
				}
				if !nd.send(map[string]any{"type": names[i], "data": string(b)}) {
					return
				}
			}
		}
		if final {
			rec := a.hist.record(p.m)
			fr := a.resultFields(rec, p.timedOut.Load())
			fr["type"] = "exit"
			fr["stdoutTruncated"], fr["stderrTruncated"] = false, false
			nd.send(fr)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-p.done:
		case <-t.C:
		}
	}
}

// lookupProc finds a detached command by process id (live or from history).
func (a *Agent) lookupProc(r *http.Request) (*proc, *meta, int, bool) {
	pid, err := strconv.Atoi(r.PathValue("processId"))
	if err != nil || pid < 1 {
		return nil, nil, 0, false
	}
	a.mu.Lock()
	p := a.procs[pid]
	a.mu.Unlock()
	if p != nil {
		return p, p.m, pid, true
	}
	m := a.hist.byProcessID(pid)
	return nil, m, pid, m != nil
}

func apiStatus(s string) string {
	switch s {
	case "running", "lost":
		return s
	}
	return "exited"
}

// GET /v1/commands/{processId}
func (a *Agent) handleCommandStatus(w http.ResponseWriter, r *http.Request) {
	p, m, pid, found := a.lookupProc(r)
	if !found {
		writeErr(w, 404, "process_not_found", "no detached command with this process id")
		return
	}
	rec := a.hist.record(m)
	res := map[string]any{
		"success": true, "processId": pid, "pid": pid, "status": apiStatus(rec.Status), "known": p != nil,
		"running": rec.Status == "running", "exitCode": rec.ExitCode, "signal": rec.Signal, "oomKilled": rec.OOMKilled,
		"command": rec.Command, "cwd": rec.Cwd, "startedAt": rec.StartedAt, "finishedAt": rec.FinishedAt,
		"logPath": a.processLog(pid, ".log"), "errLogPath": a.processLog(pid, ".err.log"),
		"commandId": rec.CommandID,
	}
	q := r.URL.Query()
	if q.Has("stdoutOffset") || q.Has("stderrOffset") {
		more := false
		for _, s := range []string{"stdout", "stderr"} {
			off, okk := qInt(r, s+"Offset", 0)
			if !okk || off < 0 {
				writeErr(w, 400, "invalid_offset", s+"Offset must be a non-negative integer")
				return
			}
			data, start, next, size := readChunk(a.hist.path(rec.CommandID, s), off, maxLogRead)
			res[s], res[s+"Offset"], res[s+"NextOffset"] = string(data), start, next
			res[s+"Truncated"] = false
			more = more || next < size
		}
		res["more"] = more
	} else {
		tail, okk := qInt(r, "tailBytes", maxSyncOutput)
		if !okk || tail < 1 || tail > maxSyncOutput {
			writeErr(w, 400, "invalid_tail_bytes", "tailBytes must be in 1-8388608")
			return
		}
		for _, s := range []string{"stdout", "stderr"} {
			data, tr := readTail(a.hist.path(rec.CommandID, s), tail)
			res[s], res[s+"Truncated"] = string(data), tr
		}
	}
	ok(w, "command.status", res)
}

// GET /v1/commands/{processId}/stream
func (a *Agent) handleCommandAttach(w http.ResponseWriter, r *http.Request) {
	p, m, pid, found := a.lookupProc(r)
	if !found {
		writeErr(w, 404, "process_not_found", "no detached command with this process id")
		return
	}
	var offs [2]int64
	for i, s := range []string{"stdout", "stderr"} {
		v, okk := qInt(r, s+"Offset", 0)
		if !okk || v < 0 {
			writeErr(w, 400, "invalid_offset", s+"Offset must be a non-negative integer")
			return
		}
		if sz := fileSize(a.hist.path(m.CommandID, s)); v > sz {
			v = sz // the cap cut the log below what the client already read
		}
		offs[i] = v
	}
	fs := []*follower{{path: a.hist.path(m.CommandID, "stdout"), off: offs[0]}, {path: a.hist.path(m.CommandID, "stderr"), off: offs[1]}}
	names := []string{"stdout", "stderr"}
	done := func() bool {
		if p != nil {
			return p.finished()
		}
		return a.hist.record(m).Status != "running"
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	nd := newNDJSON(w)
	nd.send(map[string]any{"type": "attached", "processId": pid, "stdoutOffset": offs[0], "stderrOffset": offs[1]})
	deadline := time.Now().Add(a.attachMaxDuration)
	lastSent := time.Now()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		final := done()
		for i, f := range fs {
			for {
				b := f.next(final, 256<<10)
				if len(b) == 0 {
					break
				}
				if !nd.send(map[string]any{"type": names[i], "data": string(b), "nextOffset": f.emitted()}) {
					return
				}
				lastSent = time.Now()
			}
		}
		if final {
			rec := a.hist.record(m)
			if nd.send(map[string]any{"type": "exit", "status": apiStatus(rec.Status), "exitCode": rec.ExitCode,
				"signal": rec.Signal, "stdoutOffset": fs[0].emitted(), "stderrOffset": fs[1].emitted(), "outputTruncated": rec.OutputTruncated}) {
				a.hist.mu.Lock()
				m.exitSent = true // makeRoom may drop it now
				a.hist.mu.Unlock()
			}
			return
		}
		if time.Now().After(deadline) {
			nd.send(map[string]any{"type": "reconnect", "stdoutOffset": fs[0].emitted(), "stderrOffset": fs[1].emitted()})
			return
		}
		if time.Since(lastSent) >= a.heartbeatEvery {
			if !nd.send(map[string]any{"type": "heartbeat"}) {
				return
			}
			lastSent = time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

// killGrace is how long a killed command's group gets after SIGTERM.
const killGrace = 3 * time.Second

// waitGroupGone reports whether no process is left in group pgid within d.
func waitGroupGone(pgid int, d time.Duration) bool {
	for end := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(-pgid, 0) == syscall.ESRCH {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

// DELETE /v1/commands/{processId}
func (a *Agent) handleCommandKill(w http.ResponseWriter, r *http.Request) {
	p, m, pid, found := a.lookupProc(r)
	if !found {
		writeErr(w, 404, "process_not_found", "no detached command with this process id")
		return
	}
	signaled := false
	if p != nil && !p.finished() {
		signaled = true
		p.killed.Store(true)
		// The whole group, not just the leader: children may ignore SIGTERM.
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		if !waitGroupGone(pid, killGrace) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			waitGroupGone(pid, 5*time.Second)
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	}
	// Its children may outlive the first process (a fork bomb): what is
	// left of the command's cgroup goes too (bugs-r7-opus 4).
	if m != nil {
		a.killGroup(m.CommandID)
	}
	rec := a.hist.record(m)
	ok(w, "command.stopped", map[string]any{"processId": pid, "running": false, "signaled": signaled, "command": rec})
}

// GET /v1/command-history
func (a *Agent) handleHistoryList(w http.ResponseWriter, r *http.Request) {
	if a.monitoringOff(w) {
		return
	}
	q := r.URL.Query()
	modes := map[string]bool{}
	if s := q.Get("mode"); s != "" {
		for _, m := range strings.Split(s, ",") {
			switch m = strings.TrimSpace(m); m {
			case "commands":
				modes["sync"], modes["stream"], modes["detached"] = true, true, true
			case "sync", "stream", "detached", "process":
				modes[m] = true
			case "":
			default:
				writeErr(w, 400, "invalid_mode", "unknown mode: "+m)
				return
			}
		}
	}
	limit, okk := qInt(r, "limit", 20)
	if !okk || limit < 1 || limit > 100 {
		writeErr(w, 400, "invalid_limit", "limit must be in 1-100")
		return
	}
	if len(q.Get("q")) > 200 {
		writeErr(w, 400, "invalid_query", "q is at most 200 characters")
		return
	}
	cmds, next, found := a.hist.list(modes, q.Get("q"), q.Get("cursor"), int(limit))
	if !found {
		writeErr(w, 400, "invalid_cursor", "cursor names no command in the history; start without one")
		return
	}
	if cmds == nil {
		cmds = []CommandRecord{}
	}
	var nc any
	if next != "" {
		nc = next
	}
	ok(w, "command.list", map[string]any{"commands": cmds, "nextCursor": nc})
}

// GET /v1/command-history/{commandId}
func (a *Agent) handleHistoryGet(w http.ResponseWriter, r *http.Request) {
	if a.monitoringOff(w) {
		return
	}
	m := a.hist.get(r.PathValue("commandId"))
	if m == nil {
		writeErr(w, 404, "command_not_found", "no such command")
		return
	}
	ok(w, "command.info", map[string]any{"command": a.hist.record(m)})
}

// GET /v1/command-history/{commandId}/logs
func (a *Agent) handleHistoryLogs(w http.ResponseWriter, r *http.Request) {
	if a.monitoringOff(w) {
		return
	}
	m := a.hist.get(r.PathValue("commandId"))
	if m == nil {
		writeErr(w, 404, "command_not_found", "no such command")
		return
	}
	q := r.URL.Query()
	stream := q.Get("stream")
	if stream == "" {
		stream = "stdout"
	}
	if stream != "stdout" && stream != "stderr" {
		writeErr(w, 400, "invalid_stream", "stream must be stdout or stderr")
		return
	}
	enc := q.Get("encoding")
	if enc == "" {
		enc = "utf8"
	}
	if enc != "utf8" && enc != "base64" {
		writeErr(w, 400, "invalid_encoding", "encoding must be utf8 or base64")
		return
	}
	off, ok1 := qInt(r, "offset", 0)
	limit, ok2 := qInt(r, "limit", maxLogRead)
	if !ok1 || off < 0 || !ok2 || limit < 1 || limit > maxLogRead {
		writeErr(w, 400, "invalid_range", "offset must be >= 0 and limit in 1-1048576")
		return
	}
	rec := a.hist.record(m)
	p := a.hist.path(rec.CommandID, stream)
	size := fileSize(p)
	if off > size {
		off = size
	}
	data := readAt(p, off, limit)
	next := off + int64(len(data))
	s := string(data)
	if enc == "base64" {
		s = base64.StdEncoding.EncodeToString(data)
	}
	ok(w, "command.logs", map[string]any{"commandId": rec.CommandID, "stream": stream, "offset": off,
		"nextOffset": next, "size": size, "encoding": enc, "data": s,
		"done": rec.Status != "running" && next >= size})
}

// ---------------------------------------------------------------------------
// Log helpers
// ---------------------------------------------------------------------------

// follower tails a growing file and returns whole UTF-8 characters.
type follower struct {
	path string
	off  int64 // bytes read from the file
	pend []byte
}

func (f *follower) emitted() int64 { return f.off - int64(len(f.pend)) }

// next returns newly written bytes (at most max). Unless final, an incomplete
// trailing UTF-8 sequence is held back for the next call.
func (f *follower) next(final bool, max int) []byte {
	b := readAt(f.path, f.off, int64(max))
	f.off += int64(len(b))
	f.pend = append(f.pend, b...)
	emit := f.pend
	if !final {
		emit = cutUTF8(f.pend)
	}
	if len(emit) == 0 {
		return nil
	}
	out := append([]byte(nil), emit...)
	f.pend = append(f.pend[:0], f.pend[len(emit):]...)
	return out
}

// cutUTF8 returns the prefix of b without an incomplete trailing rune.
func cutUTF8(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			break
		}
	}
	return b
}

func readAt(p string, off, n int64) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, n)
	k, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return buf[:k]
}

// readChunk reads up to max bytes from off (clamped to the size if the cap cut the file),
// cut on a whole UTF-8 character. Returns data, start, next offset and size.
func readChunk(p string, off, max int64) ([]byte, int64, int64, int64) {
	size := fileSize(p)
	if off > size {
		off = size
	}
	b := readAt(p, off, max)
	if c := cutUTF8(b); len(c) > 0 || len(b) < utf8.UTFMax {
		b = c
	}
	return b, off, off + int64(len(b)), size
}

func readHead(p string, max int64) ([]byte, bool) {
	size := fileSize(p)
	return readAt(p, 0, max), size > max
}

func readTail(p string, n int64) ([]byte, bool) {
	size := fileSize(p)
	off := size - n
	if off < 0 {
		off = 0
	}
	b := readAt(p, off, n)
	if off > 0 { // don't start inside a rune
		for i := 0; i < len(b) && i < utf8.UTFMax; i++ {
			if utf8.RuneStart(b[i]) {
				b = b[i:]
				break
			}
		}
	}
	return b, off > 0
}

// ndjson writes one JSON object per line and flushes.
type ndjson struct {
	w   http.ResponseWriter
	enc *json.Encoder
	fl  http.Flusher
}

func newNDJSON(w http.ResponseWriter) *ndjson {
	fl, _ := w.(http.Flusher)
	return &ndjson{w: w, enc: json.NewEncoder(w), fl: fl}
}

func (n *ndjson) send(v any) bool {
	if err := n.enc.Encode(v); err != nil {
		return false
	}
	if n.fl != nil {
		n.fl.Flush()
	}
	return true
}

// processLogDir: Boat's user-readable log folder of detached commands
// (CMD-006): ~/.ascii/processes/<pid>.log (stdout) and <pid>.err.log.
func (a *Agent) processLogDir() string { return filepath.Join(a.home, ".ascii", "processes") }

// processLog returns the virtual path of a detached command's log link.
func (a *Agent) processLog(pid int, suffix string) string {
	return a.toVirt(filepath.Join(a.processLogDir(), strconv.Itoa(pid)+suffix))
}

// linkProcessLogs points ~/.ascii/processes/<pid>.{log,err.log} at the
// command's history files (replacing links of an earlier, reused pid).
func (a *Agent) linkProcessLogs(pid int, id string) {
	dir := a.processLogDir()
	if err := a.mkdirAll(dir, 0o755); err != nil {
		return
	}
	for suffix, stream := range map[string]string{".log": "stdout", ".err.log": "stderr"} {
		l := filepath.Join(dir, strconv.Itoa(pid)+suffix)
		_ = os.Remove(l)
		if os.Symlink(a.hist.path(id, stream), l) == nil {
			a.chown(l)
		}
	}
}
