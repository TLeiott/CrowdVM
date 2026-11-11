package agents

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// turnSpec is what a harness needs to run one turn.
type turnSpec struct {
	Prompt      string
	Model       string
	Effort      string
	Fast        *bool
	Session     string // native session to resume ("" = new session)
	NewSession  string // id to give a new session, when the harness lets us pick it
	Cwd         string
	Attachments []string
	Env         map[string]string
}

// run is one harness process (one turn, or a turn plus its native
// continuations). Parsers call the sink methods (text, toolUse, ...).
type run struct {
	m       *Manager
	c       *conv
	p       *promptRec
	h       *harness
	cmd     *exec.Cmd
	model   string
	done    chan struct{} // closed when stdout is drained
	exited  chan struct{} // closed when the process has exited
	waitErr error
	out     *os.File
	closed  bool   // the turn's outcome was collected; late steers become new turns
	drv     driver // protocol harnesses (Codex app-server, pi RPC, ACP)

	wmu           sync.Mutex // serializes stdin writes
	mu            sync.Mutex
	stdin         io.WriteCloser // open only for native-steer and protocol harnesses
	pendingTools  []map[string]any
	steers        []*steerRec // native steers sent, waiting for the harness to take them
	fallback      []*steerRec // steers that need interrupt + continue
	sawResult     bool        // a result arrived since the last steer was sent
	userInterrupt bool
	errMsg        string
	session       string
	plain         strings.Builder // non-JSON stdout (generic harnesses)
	stream        strings.Builder // text of the message being streamed (is_streaming partials)
	lastPartial   time.Time
	stderr        tail
}

// trace logs raw harness stdout when CROWDVM_AGENT_TRACE names a file (debugging).
var trace = func() *log.Logger {
	p := os.Getenv("CROWDVM_AGENT_TRACE")
	if p == "" {
		return nil
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return log.New(f, "", log.LstdFlags|log.Lmicroseconds)
}()

// partialEvery throttles is_streaming partial events.
var partialEvery = 700 * time.Millisecond

// write sends one line to the harness stdin.
func (r *run) write(b []byte) error {
	r.mu.Lock()
	w := r.stdin
	r.mu.Unlock()
	if w == nil {
		return errors.New("the harness input is closed")
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	_, err := w.Write(b)
	return err
}

func (r *run) plainLine(s string) {
	r.mu.Lock()
	r.plain.WriteString(s + "\n")
	r.mu.Unlock()
}

// ---- sink methods used by parsers ----

func (r *run) setSession(id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	r.session = id
	r.mu.Unlock()
	r.m.mu.Lock()
	if r.c.Session != id || r.c.SessionHarness != r.h.name {
		r.c.Session, r.c.SessionHarness = id, r.h.name
		r.m.saveLocked()
	}
	r.m.mu.Unlock()
}

func (r *run) setModel(model string) {
	if model != "" {
		r.model = model
	}
}

func (r *run) text(s string) {
	r.mu.Lock()
	r.stream.Reset()
	r.mu.Unlock()
	if strings.TrimSpace(s) == "" {
		return
	}
	r.emit(map[string]any{"content": s})
}

// partial appends streamed text of the message in progress and, at most every
// partialEvery, emits it as a response with is_streaming=true (cumulative
// content). The final text arrives through text().
func (r *run) partial(delta string) {
	if delta == "" {
		return
	}
	r.mu.Lock()
	r.stream.WriteString(delta)
	content := r.stream.String()
	due := time.Since(r.lastPartial) >= partialEvery
	if due {
		r.lastPartial = time.Now()
	}
	r.mu.Unlock()
	if due && strings.TrimSpace(content) != "" {
		r.emit(map[string]any{"content": content, "is_streaming": true})
	}
}

func (r *run) toolUse(id, name string, input any) {
	r.mu.Lock()
	r.pendingTools = append(r.pendingTools, map[string]any{"id": id, "type": "tool_use", "name": name, "input": input})
	r.mu.Unlock()
}

func (r *run) toolResult(id string, isErr bool, content string) {
	r.mu.Lock()
	var use map[string]any
	for i, u := range r.pendingTools {
		if u["id"] == id {
			use = u
			r.pendingTools = append(r.pendingTools[:i], r.pendingTools[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
	if use == nil {
		use = map[string]any{"id": id, "type": "tool_use", "name": "unknown", "input": map[string]any{}}
	}
	r.emit(map[string]any{"content": "", "tools": []any{map[string]any{"use": use,
		"result": map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": truncate(content, 64<<10)}}}})
}

// toolDone records a call's result; the use is reported unless toolUse
// already recorded it (harnesses that announce calls when they start).
func (r *run) toolDone(id, name string, input any, isErr bool, output string) {
	r.mu.Lock()
	known := false
	for _, u := range r.pendingTools {
		if u["id"] == id {
			known = true
			break
		}
	}
	r.mu.Unlock()
	if !known {
		r.toolUse(id, name, input)
	}
	r.toolResult(id, isErr, output)
}

// tool records a call whose result is already known.
func (r *run) tool(id, name string, input any, isErr bool, output string) {
	r.toolUse(id, name, input)
	r.toolResult(id, isErr, output)
}

func (r *run) flushTools() {
	r.mu.Lock()
	pend := r.pendingTools
	r.pendingTools = nil
	plain := r.plain.String()
	r.plain.Reset()
	r.mu.Unlock()
	r.text(plain)
	for _, u := range pend {
		r.emit(map[string]any{"content": "", "tools": []any{map[string]any{"use": u}}})
	}
}

func (r *run) fail(msg string) {
	if msg == "" {
		msg = "harness reported an error"
	}
	r.mu.Lock()
	r.errMsg = msg
	r.mu.Unlock()
}

func (r *run) emit(data map[string]any) {
	data["model"] = nilIfEmpty(r.model)
	data["provider"] = r.h.name
	if _, ok := data["is_streaming"]; !ok {
		data["is_streaming"] = false
	}
	data["is_reverted"] = false
	r.m.mu.Lock()
	r.m.appendLocked("response", r.c.ID, r.p.ID, data)
	r.m.mu.Unlock()
}

// ackSteer is called when a native-steer harness echoes a user message back
// (Claude Code --replay-user-messages): the harness took it in.
func (r *run) ackSteer(text string) {
	r.mu.Lock()
	var s *steerRec
	for i, x := range r.steers {
		if x.Message == text {
			s = x
			r.steers = append(r.steers[:i], r.steers[i+1:]...)
			break
		}
	}
	mode := "native"
	if r.sawResult {
		mode = "native-continued"
	}
	r.mu.Unlock()
	if s != nil {
		r.m.settleSteer(s, mode)
	}
}

// result marks the end of one harness turn (Claude Code "result"). It closes
// stdin unless steers are still waiting or the harness has queued turns, so
// the process exits only after it acted on every steer.
func (r *run) result(isErr bool, msg string, queued int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sawResult = true
	if isErr {
		r.errMsg = msg
	} else {
		r.errMsg = ""
	}
	if len(r.steers) == 0 && queued == 0 {
		r.closeStdinLocked()
	}
}

// endTurn ends a protocol harness turn: records an error (if any) and closes
// stdin so the process exits. Unacknowledged steers then run as the next turn
// on the same session (native-continued).
func (r *run) endTurn(isErr bool, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sawResult = true
	if isErr {
		if msg == "" {
			msg = "harness reported an error"
		}
		r.errMsg = msg
	}
	r.closeStdinLocked()
}

// closeStdinLocked closes the harness input. A harness that does not exit on
// EOF within 20 s is terminated.
func (r *run) closeStdinLocked() {
	if r.stdin == nil {
		return
	}
	r.stdin.Close()
	r.stdin = nil
	exited := r.exited
	go func() {
		select {
		case <-exited:
		case <-time.After(20 * time.Second):
			if r.cmd.Process != nil {
				_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
				select {
				case <-exited:
				case <-time.After(5 * time.Second):
					_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
				}
			}
		}
	}()
}

// ---- execution ----

// execPrompt runs p to completion (including steer continuations).
func (m *Manager) execPrompt(c *conv, p *promptRec) {
	h := harnesses[p.Provider]
	m.mu.Lock()
	spec := &turnSpec{Prompt: p.Prompt, Model: p.Model, Effort: p.ReasoningEffort, Fast: p.Fast,
		Cwd: p.Cwd, Attachments: p.Attachments, Env: p.env}
	if c.SessionHarness == h.name && c.Session != "" && h.sessions {
		spec.Session = c.Session
	} else if prior := m.transcriptLocked(c.ID, p.ID); prior != "" {
		spec.Prompt = "This conversation started earlier (possibly in another coding agent). Transcript so far:\n" +
			prior + "\n--- end of transcript ---\n\nNew request from the user:\n" + p.Prompt
	}
	m.mu.Unlock()
	if len(p.Attachments) > 0 {
		spec.Prompt += "\n\nAttached files: " + strings.Join(p.Attachments, ", ")
	}
	if h.noteInPrompt && spec.Session == "" { // no system-prompt option: tell it once per session
		spec.Prompt = "<platform-note>" + systemNote + "</platform-note>\n\n" + spec.Prompt
	}
	if m.o.EnsureDesktop != nil { // computer-use tools need the desktop
		go func() { _ = m.o.EnsureDesktop() }()
	}

	status, errMsg := "finished", ""
	for {
		m.mu.Lock()
		stop := c.stop
		m.mu.Unlock()
		if stop {
			status = "interrupted"
			break
		}
		r, err := m.startRun(c, p, h, spec)
		if err != nil {
			status, errMsg = "failed", err.Error()
			break
		}
		werr := r.wait()
		r.flushTools()
		m.mu.Lock()
		c.run = nil
		m.mu.Unlock()

		r.mu.Lock()
		r.closed = true
		fallback, leftover, ui, rerr := r.fallback, r.steers, r.userInterrupt, r.errMsg
		session := r.session
		r.mu.Unlock()
		if ui {
			status, errMsg = "interrupted", ""
			for _, s := range append(fallback, leftover...) {
				m.settleSteer(s, "late")
			}
			break
		}
		if session != "" {
			spec.Session = session
		}
		// Steers that need another turn: fallback ones (we interrupted the turn)
		// and native ones the harness never took in (settled native-continued).
		var msgs []string
		for _, s := range fallback {
			msgs = append(msgs, s.Message)
		}
		for _, s := range leftover {
			m.settleSteer(s, "native-continued")
			msgs = append(msgs, s.Message)
		}
		if len(msgs) > 0 {
			switch {
			case spec.Session == "": // interrupted before the harness made a session: start over
				spec.Prompt += "\n\nAdditional instruction from the user: " + strings.Join(msgs, "\n")
			case len(fallback) > 0:
				spec.Prompt = "Continue what you were doing. The user sent a new instruction: " + strings.Join(msgs, "\n")
			default:
				spec.Prompt = strings.Join(msgs, "\n")
			}
			continue
		}
		switch {
		case rerr != "":
			status, errMsg = "failed", rerr
		case werr != nil && len(fallback) == 0:
			status, errMsg = "failed", fmt.Sprintf("%s exited: %v%s", h.name, werr, r.stderr.suffix())
		}
		break
	}
	if errMsg != "" {
		errMsg = friendlyError(errMsg, m.envMap(spec.Env))
	}
	m.mu.Lock()
	m.finishLocked(p, status, errMsg)
	m.saveLocked()
	m.mu.Unlock()
}

func (m *Manager) startRun(c *conv, p *promptRec, h *harness, spec *turnSpec) (*run, error) {
	if h.sessions && spec.Session == "" && h.pickSession {
		spec.NewSession = newUUID()
	}
	bin, args, stdin := h.command(m, spec)
	path, err := m.lookPath(bin, spec.Env)
	if err != nil {
		return nil, err
	}
	r := &run{m: m, c: c, p: p, h: h, model: spec.Model, done: make(chan struct{}), exited: make(chan struct{})}
	if h.driver != nil {
		r.drv = h.driver()
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = m.cwd(spec.Cwd)
	cmd.Env = m.runEnv(spec, h)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cred := m.credential(); cred != nil {
		cmd.SysProcAttr.Credential = cred
	}
	cmd.Stderr = &r.stderr
	// Our own pipe (not StdoutPipe): a background process the harness started
	// may keep stdout open after the harness exits; wait() then stops reading.
	out, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	r.out = out
	in, err := cmd.StdinPipe()
	if err != nil {
		out.Close()
		pw.Close()
		return nil, err
	}
	r.cmd = cmd
	if m.o.Spawn != nil {
		err = m.o.Spawn(cmd)
	} else {
		err = cmd.Start()
	}
	pw.Close()
	if err != nil {
		out.Close()
		return nil, fmt.Errorf("start %s: %w", h.name, err)
	}
	go func() { r.waitErr = cmd.Wait(); close(r.exited) }()
	if h.nativeSteer || r.drv != nil {
		_, _ = in.Write(stdin)
		r.stdin = in
	} else {
		go func() { _, _ = in.Write(stdin); in.Close() }()
	}
	m.mu.Lock()
	c.run = r
	stop := c.stop
	m.mu.Unlock()
	if stop { // interrupted while starting
		r.interrupt()
	}
	if r.drv != nil {
		go r.drv.begin(r, spec)
	}
	go func() {
		br := bufio.NewReaderSize(out, 64<<10)
		for {
			line, err := br.ReadBytes('\n')
			if l := strings.TrimSpace(string(line)); l != "" {
				if trace != nil {
					trace.Printf("%s %s", h.name, l)
				}
				if r.drv != nil {
					r.drv.line(r, []byte(l))
				} else {
					h.parse(r, []byte(l))
				}
			}
			if err != nil {
				break
			}
		}
		r.mu.Lock()
		r.closeStdinLocked()
		r.mu.Unlock()
		out.Close()
		close(r.done)
	}()
	return r, nil
}

func (r *run) wait() error {
	select {
	case <-r.done:
	case <-r.exited:
		select {
		case <-r.done:
		case <-time.After(3 * time.Second): // stdout held open by a leftover child
			r.out.Close() // unblocks the reader
			<-r.done
		}
	}
	<-r.exited
	return r.waitErr
}

// steer delivers a steer to the running process. It returns the immediate mode:
// "native" (written to the harness), "fallback" (turn interrupted, continues),
// or "late" (the process is already finishing).
func (r *run) steer(s *steerRec) string {
	r.mu.Lock()
	if r.h.nativeSteer {
		if r.stdin == nil {
			r.mu.Unlock()
			return "late"
		}
		r.steers = append(r.steers, s)
		r.sawResult = false
		r.mu.Unlock()
		var ok bool
		if r.drv != nil {
			ok = r.drv.steer(r, s)
		} else {
			ok = r.write(r.h.steerLine(s.Message)) == nil
		}
		if !ok {
			r.mu.Lock()
			for i, x := range r.steers {
				if x == s {
					r.steers = append(r.steers[:i], r.steers[i+1:]...)
					break
				}
			}
			ended := r.closed || r.stdin == nil
			r.mu.Unlock()
			if !ended {
				select {
				case <-r.exited:
					ended = true
				default:
				}
			}
			if ended {
				return "late" // the turn ended under us: run it as a new turn
			}
			return "failed" // the harness runs on but refused it (Boat delivery_failed)
		}
		return "native"
	}
	if r.closed || r.userInterrupt || !r.h.sessions || (r.drv != nil && r.stdin == nil) {
		r.mu.Unlock()
		return "late"
	}
	r.fallback = append(r.fallback, s)
	r.mu.Unlock()
	if r.drv == nil || !r.drv.cancel(r) {
		r.signal()
	}
	return "fallback"
}

// interrupt stops the turn for good (POST /interrupt).
func (r *run) interrupt() {
	r.mu.Lock()
	r.userInterrupt = true
	r.mu.Unlock()
	r.signal()
}

// signal sends SIGINT to the harness process group, then SIGKILL after 5 s.
func (r *run) signal() {
	if r.cmd.Process == nil {
		return
	}
	pid := r.cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGINT)
	done := r.exited
	go func() {
		select {
		case <-done:
			// harness exited; make sure stragglers in its group die too
			time.Sleep(500 * time.Millisecond)
		case <-time.After(5 * time.Second):
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}()
}

// ---- environment ----

func (m *Manager) cwd(c string) string {
	if c == "" {
		if m.o.DefaultCwd != nil {
			if d := m.o.DefaultCwd(); d != "" && dirExists(d) {
				return d
			}
		}
		return m.o.Home
	}
	if !filepath.IsAbs(c) {
		c = filepath.Join(m.o.Home, c)
	}
	return c
}

func (m *Manager) credential() *syscall.Credential {
	if m.o.Unprivileged || os.Geteuid() != 0 {
		return nil
	}
	u, err := user.Lookup(m.o.User)
	if err != nil {
		return nil
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	var groups []uint32
	if ids, err := u.GroupIds(); err == nil {
		for _, g := range ids {
			if n, err := strconv.Atoi(g); err == nil {
				groups = append(groups, uint32(n))
			}
		}
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}
}

// envMap builds the harness environment: process env < defaults < AI proxy <
// sandbox env < request env/credentials.
func (m *Manager) envMap(req map[string]string) map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	home := m.o.Home
	env["HOME"] = home
	if !m.o.Unprivileged {
		env["USER"], env["LOGNAME"], env["SHELL"] = m.o.User, m.o.User, "/bin/bash"
		if cred := m.credential(); cred != nil {
			if d := fmt.Sprintf("/run/user/%d", cred.Uid); dirExists(d) {
				env["XDG_RUNTIME_DIR"] = d
			}
		}
	}
	extra := []string{home + "/.local/bin", home + "/.npm-global/bin", home + "/.bun/bin", home + "/.cargo/bin", "/usr/local/bin", "/usr/bin", "/bin"}
	env["PATH"] = strings.Join(append(extra, env["PATH"]), ":")
	if env["DISPLAY"] == "" {
		env["DISPLAY"] = ":0" // the sandbox desktop (computer use)
	}
	env["IS_SANDBOX"] = "1"
	env["CROWDVM_SANDBOX"] = "1"
	for k, v := range m.ProxyEnv() {
		env[k] = v
	}
	for k, v := range m.o.Env() {
		env[k] = v
	}
	for k, v := range req {
		env[k] = v
	}
	requestCredsWin(env, req, m.ProxyEnv())
	return env
}

// requestCredsWin: a credential in the request (crowdvm prompt --local-login)
// wins for its turn over the sandbox's route for that vendor. A Claude sign-in
// drops the sandbox's other Anthropic auth, base URL and Bedrock/Vertex
// switches (an AI-proxy placeholder key would outrank the OAuth token); a
// ChatGPT sign-in drops the OpenAI key and base URL (codexRoute "chatgpt");
// a vendor API key drops that vendor's AI-proxy base URL, so the key goes
// straight to the vendor. Values the request sets itself are kept.
func requestCredsWin(env, req, proxy map[string]string) {
	drop := func(keys ...string) {
		for _, k := range keys {
			if _, own := req[k]; !own {
				delete(env, k)
			}
		}
	}
	if req["CLAUDE_CODE_OAUTH_TOKEN"] != "" {
		drop("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX")
	}
	if req["CHATGPT_ACCESS_TOKEN"] != "" {
		drop("OPENAI_API_KEY", "CODEX_API_KEY", "OPENAI_BASE_URL")
	}
	if req["ANTHROPIC_API_KEY"] != "" {
		drop("CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX")
	}
	for k, v := range req {
		if base := strings.TrimSuffix(k, "_API_KEY") + "_BASE_URL"; v != "" && strings.HasSuffix(k, "_API_KEY") && proxy[base] != "" && env[base] == proxy[base] {
			drop(base)
		}
	}
}

func (m *Manager) runEnv(t *turnSpec, h *harness) []string {
	env := m.envMap(t.Env)
	if h.env != nil {
		h.env(m, t, env)
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func (m *Manager) lookPath(bin string, req map[string]string) (string, error) {
	if filepath.IsAbs(bin) {
		if _, err := os.Stat(bin); err != nil {
			return "", errNotInstalled(bin)
		}
		return bin, nil
	}
	for _, dir := range filepath.SplitList(m.envMap(req)["PATH"]) {
		p := filepath.Join(dir, bin)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", errNotInstalled(bin)
}

type notInstalledError struct{ bin string }

func (e notInstalledError) Error() string {
	return fmt.Sprintf("harness binary %q is not installed in this sandbox", e.bin)
}

func errNotInstalled(bin string) error { return notInstalledError{bin} }

func isNotInstalled(err error) bool { return errors.As(err, new(notInstalledError)) }

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// tail keeps the last 4 KiB written (harness stderr, for error messages).
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	t.mu.Unlock()
	return len(b), nil
}

func (t *tail) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(string(t.buf))
	if s == "" {
		return ""
	}
	return ": " + s
}

func jsonLine(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}
