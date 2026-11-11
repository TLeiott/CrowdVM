package agents

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// driver speaks a bidirectional protocol with a long-lived harness process
// (Codex app-server, pi/Prime RPC, ACP). The process stdin stays open; the
// driver ends the turn with r.endTurn, which closes stdin so the process exits.
type driver interface {
	// begin runs in its own goroutine after the process started: handshake,
	// session start/resume and the turn itself.
	begin(r *run, t *turnSpec)
	// line handles one stdout line.
	line(r *run, b []byte)
	// steer delivers a native steer; false = the turn is no longer running.
	steer(r *run, s *steerRec) bool
	// cancel stops the turn through the protocol (fallback steer); false = not
	// supported, the caller sends SIGINT instead.
	cancel(r *run) bool
}

// rpcReply is the outcome of one request.
type rpcReply struct {
	result json.RawMessage
	err    error
}

// waiters correlates requests with responses by id.
type waiters struct {
	mu sync.Mutex
	n  int
	m  map[string]chan rpcReply
}

func (w *waiters) add() (string, chan rpcReply) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]chan rpcReply{}
	}
	w.n++
	id := strconv.Itoa(w.n)
	ch := make(chan rpcReply, 1)
	w.m[id] = ch
	return id, ch
}

func (w *waiters) deliver(id string, rep rpcReply) bool {
	w.mu.Lock()
	ch := w.m[id]
	delete(w.m, id)
	w.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- rep
	return true
}

// wait blocks for the reply, the end of the process, or the timeout (0 = none).
func (w *waiters) wait(r *run, id string, ch chan rpcReply, what string, timeout time.Duration) (json.RawMessage, error) {
	var tc <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		tc = t.C
	}
	select {
	case rep := <-ch:
		return rep.result, rep.err
	case <-r.done:
		w.deliver(id, rpcReply{})
		return nil, fmt.Errorf("%s exited before answering %s%s", r.h.name, what, r.stderr.suffix())
	case <-tc:
		w.deliver(id, rpcReply{})
		return nil, fmt.Errorf("%s did not answer %s within %s", r.h.name, what, timeout)
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 over newline-delimited stdio (Codex app-server, ACP)
// ---------------------------------------------------------------------------

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	msg := e.Message
	if d, ok := e.Data.(string); ok && d != "" && !strings.Contains(msg, d) {
		msg += ": " + d
	}
	return msg
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type jsonRPC struct {
	r *run
	w waiters
}

func (c *jsonRPC) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id, ch := c.w.add()
	n, _ := strconv.Atoi(id)
	if err := c.r.write(jsonLine(map[string]any{"jsonrpc": "2.0", "id": n, "method": method, "params": params})); err != nil {
		c.w.deliver(id, rpcReply{})
		return nil, fmt.Errorf("%s: %s: %w", c.r.h.name, method, err)
	}
	return c.w.wait(c.r, id, ch, method, timeout)
}

func (c *jsonRPC) notify(method string, params any) {
	_ = c.r.write(jsonLine(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}))
}

func (c *jsonRPC) respond(id json.RawMessage, result any) {
	_ = c.r.write(jsonLine(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}))
}

func (c *jsonRPC) respondErr(id json.RawMessage, code int, msg string) {
	_ = c.r.write(jsonLine(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}))
}

// parse decodes a line. Responses are delivered to their caller (ok=false);
// requests (ID and Method set) and notifications are returned.
func (c *jsonRPC) parse(b []byte) (rpcMsg, bool) {
	var m rpcMsg
	if json.Unmarshal(b, &m) != nil {
		return m, false
	}
	if m.Method == "" {
		if len(m.ID) > 0 {
			rep := rpcReply{result: m.Result}
			if m.Error != nil {
				rep.err = m.Error
			}
			c.w.deliver(strings.Trim(string(m.ID), `"`), rep)
		}
		return m, false
	}
	return m, true
}

// ---------------------------------------------------------------------------
// Codex: codex app-server (JSON-RPC). Native steer via turn/steer.
// ---------------------------------------------------------------------------

type codexDriver struct {
	rpc      jsonRPC
	mu       sync.Mutex
	threadID string
	turnID   string
}

func (d *codexDriver) ids() (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.threadID, d.turnID
}

func (d *codexDriver) begin(r *run, t *turnSpec) {
	d.rpc.r = r
	if _, err := d.rpc.call("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "crowdvm", "title": "CrowdVM", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, 2*time.Minute); err != nil {
		r.endTurn(true, err.Error())
		return
	}
	d.rpc.notify("initialized", map[string]any{})
	cwd := r.m.cwd(t.Cwd)
	thread := map[string]any{"cwd": cwd, "approvalPolicy": "never", "sandbox": "danger-full-access",
		"developerInstructions": systemNote}
	if t.Model != "" {
		thread["model"] = t.Model
	}
	var res json.RawMessage
	var err error
	if t.Session != "" {
		thread["threadId"] = t.Session
		if res, err = d.rpc.call("thread/resume", thread, 2*time.Minute); err != nil {
			delete(thread, "threadId") // gone (deleted rollout): start over
			res, err = nil, nil
		}
	}
	if res == nil {
		res, err = d.rpc.call("thread/start", thread, 2*time.Minute)
	}
	if err != nil {
		r.endTurn(true, err.Error())
		return
	}
	var ts struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(res, &ts)
	d.mu.Lock()
	d.threadID = ts.Thread.ID
	d.mu.Unlock()
	r.setSession(ts.Thread.ID)

	turn := map[string]any{"threadId": ts.Thread.ID, "input": codexInput(t.Prompt, t.Attachments), "cwd": cwd,
		"approvalPolicy": "never", "sandboxPolicy": map[string]any{"type": "dangerFullAccess"}}
	if t.Model != "" {
		turn["model"] = t.Model
	}
	if t.Effort != "" {
		turn["effort"] = t.Effort
	}
	if t.Fast != nil {
		turn["serviceTier"] = codexServiceTier(*t.Fast)
	}
	res, err = d.rpc.call("turn/start", turn, 2*time.Minute)
	if err != nil {
		r.endTurn(true, err.Error())
		return
	}
	var tr struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(res, &tr)
	d.mu.Lock()
	if d.turnID == "" { // turn/started may have set it already
		d.turnID = tr.Turn.ID
	}
	d.mu.Unlock()
}

// codexServiceTier: Codex calls the fast tier "priority" (shown as "Fast").
// "default" is not advertised by any model, so Codex omits it from requests,
// which forces standard processing even on models whose default is priority.
func codexServiceTier(fast bool) string {
	if fast {
		return "priority"
	}
	return "default"
}

func codexInput(text string, attachments []string) []any {
	in := []any{map[string]any{"type": "text", "text": text}}
	for _, a := range attachments {
		if isImage(a) {
			in = append(in, map[string]any{"type": "localImage", "path": a})
		}
	}
	return in
}

func (d *codexDriver) line(r *run, b []byte) {
	m, ok := d.rpc.parse(b)
	if !ok {
		return
	}
	if len(m.ID) > 0 { // server → client request
		switch {
		case strings.HasSuffix(m.Method, "requestApproval"):
			d.rpc.respond(m.ID, map[string]any{"decision": "accept"})
		case m.Method == "applyPatchApproval" || m.Method == "execCommandApproval":
			d.rpc.respond(m.ID, map[string]any{"decision": "approved"})
		case m.Method == "mcpServer/elicitation/request":
			d.rpc.respond(m.ID, map[string]any{"action": "decline"})
		default:
			d.rpc.respondErr(m.ID, -32601, "not supported by the CrowdVM agent server: "+m.Method)
		}
		return
	}
	var p struct {
		ThreadID string         `json:"threadId"`
		TurnID   string         `json:"turnId"`
		Delta    string         `json:"delta"`
		Item     map[string]any `json:"item"`
		Turn     struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		WillRetry bool `json:"willRetry"`
	}
	_ = json.Unmarshal(m.Params, &p)
	thread, turn := d.ids()
	if p.ThreadID != "" && thread != "" && p.ThreadID != thread {
		return // a subagent thread
	}
	switch m.Method {
	case "turn/started":
		d.mu.Lock()
		if d.turnID == "" {
			d.turnID = p.Turn.ID
		}
		d.mu.Unlock()
	case "item/agentMessage/delta":
		r.partial(p.Delta)
	case "item/started":
		// Tool calls are announced here; unified-exec commands may never
		// get an item/completed, so the use is reported either way.
		if name, input := codexToolOf(p.Item); name != "" {
			r.toolUse(str(p.Item["id"]), name, input)
		}
	case "item/completed":
		codexItem(r, p.Item)
	case "error":
		if !p.WillRetry {
			r.fail(p.Error.Message)
		}
	case "turn/completed":
		if turn != "" && p.Turn.ID != turn {
			return
		}
		switch p.Turn.Status {
		case "failed":
			msg := "turn failed"
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				msg = p.Turn.Error.Message
			}
			r.endTurn(true, msg)
		default:
			r.endTurn(false, "")
		}
	}
}

// codexToolOf names a tool-call item (Boat event naming) and its input.
func codexToolOf(it map[string]any) (string, any) {
	switch it["type"] {
	case "commandExecution":
		return "Bash", map[string]any{"command": it["command"]}
	case "fileChange":
		return "Edit", map[string]any{"changes": it["changes"]}
	case "mcpToolCall":
		return "mcp:" + str(it["server"]) + "/" + str(it["tool"]), it["arguments"]
	case "dynamicToolCall":
		return str(it["tool"]), it["arguments"]
	case "webSearch":
		return "WebSearch", map[string]any{"query": it["query"]}
	}
	return "", nil
}

func codexItem(r *run, it map[string]any) {
	id := str(it["id"])
	switch it["type"] {
	case "agentMessage":
		r.text(str(it["text"]))
	case "userMessage":
		var parts []string
		if cs, ok := it["content"].([]any); ok {
			for _, c := range cs {
				if cm, ok := c.(map[string]any); ok && cm["type"] == "text" {
					parts = append(parts, str(cm["text"]))
				}
			}
		}
		r.ackSteer(strings.Join(parts, "\n"))
	default:
		name, input := codexToolOf(it)
		if name == "" {
			return
		}
		var out string
		isErr := it["status"] == "failed"
		switch it["type"] {
		case "commandExecution":
			code, _ := it["exitCode"].(float64)
			isErr = isErr || code != 0
			out = str(it["aggregatedOutput"])
		case "fileChange":
			out = str(it["status"])
		case "mcpToolCall":
			out = string(mustJSON(it["result"]))
			if it["error"] != nil {
				isErr, out = true, string(mustJSON(it["error"]))
			}
		case "dynamicToolCall":
			isErr = isErr || it["success"] == false
			out = string(mustJSON(it["contentItems"]))
		}
		r.toolDone(id, name, input, isErr, out)
	}
}

func (d *codexDriver) steer(r *run, s *steerRec) bool {
	thread, turn := d.ids()
	if thread == "" || turn == "" {
		return false
	}
	_, err := d.rpc.call("turn/steer", map[string]any{"threadId": thread, "expectedTurnId": turn,
		"input": codexInput(s.Message, nil), "clientUserMessageId": s.ID}, 30*time.Second)
	return err == nil
}

func (d *codexDriver) cancel(r *run) bool {
	thread, turn := d.ids()
	if turn == "" {
		return false
	}
	go func() {
		_, _ = d.rpc.call("turn/interrupt", map[string]any{"threadId": thread, "turnId": turn}, 30*time.Second)
	}()
	return true
}
