package agents

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Kimi Code (`kimi acp`) and Mistral Vibe (`vibe-acp`) speak the Agent Client
// Protocol (JSON-RPC over stdio). It has prompt and cancel but nothing in
// between, so steers use the fallback path: session/cancel, then the next
// harness process resumes the same session with the instruction.

type acpDriver struct {
	rpc jsonRPC
	// passMCP: register the computer server in session/new (Vibe has no MCP
	// config file of its own for it; Kimi reads ~/.kimi-code/mcp.json).
	passMCP bool

	mu        sync.Mutex
	sessionID string
	prompting bool // session/update before the prompt (load replay, notices) is ignored
	buf       strings.Builder
	tools     map[string]map[string]any // toolCallId → {title, kind, rawInput}
}

func (d *acpDriver) begin(r *run, t *turnSpec) {
	d.rpc.r = r
	res, err := d.rpc.call("initialize", map[string]any{"protocolVersion": 1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]any{"name": "crowdvm", "title": "CrowdVM", "version": "1"}}, 2*time.Minute)
	if err != nil {
		r.endTurn(true, err.Error())
		return
	}
	var caps struct {
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				Resume *struct{} `json:"resume"`
			} `json:"sessionCapabilities"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(res, &caps)
	cwd := r.m.cwd(t.Cwd)
	mcp := []any{}
	if d.passMCP {
		if c := computerServer(); c != nil {
			mcp = append(mcp, map[string]any{"name": "computer", "command": c.Command, "args": c.Args, "env": []any{}})
		}
	}
	var sess json.RawMessage
	sid := t.Session
	if sid != "" {
		params := map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": mcp}
		switch {
		case caps.AgentCapabilities.SessionCapabilities.Resume != nil:
			sess, err = d.rpc.call("session/resume", params, 2*time.Minute)
		case caps.AgentCapabilities.LoadSession:
			sess, err = d.rpc.call("session/load", params, 5*time.Minute)
		default:
			err = fmt.Errorf("no session resume")
		}
		if err != nil {
			sid, sess = "", nil // the session is gone: start a new one
		}
	}
	if sid == "" {
		if sess, err = d.rpc.call("session/new", map[string]any{"cwd": cwd, "mcpServers": mcp}, 2*time.Minute); err != nil {
			r.endTurn(true, err.Error())
			return
		}
		var ns struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(sess, &ns)
		sid = ns.SessionID
	}
	d.mu.Lock()
	d.sessionID = sid
	d.mu.Unlock()
	r.setSession(sid)
	if err := d.configure(sid, sess, t); err != nil {
		r.endTurn(true, err.Error())
		return
	}

	prompt := []any{map[string]any{"type": "text", "text": t.Prompt}}
	if caps.AgentCapabilities.PromptCapabilities.Image {
		for _, img := range piImages(t.Attachments) {
			m := img.(map[string]any)
			prompt = append(prompt, map[string]any{"type": "image", "data": m["data"], "mimeType": m["mimeType"]})
		}
	}
	d.mu.Lock()
	d.prompting = true
	d.mu.Unlock()
	res, err = d.rpc.call("session/prompt", map[string]any{"sessionId": sid, "prompt": prompt}, 0)
	d.flush(r)
	if err != nil {
		r.endTurn(true, err.Error())
		return
	}
	var pr struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(res, &pr)
	switch pr.StopReason {
	case "refusal":
		r.endTurn(true, "the model refused the request")
	case "max_tokens", "max_turn_requests":
		r.endTurn(true, "the turn stopped early: "+pr.StopReason)
	default: // end_turn, cancelled
		r.endTurn(false, "")
	}
}

// configure sets the session to never ask for permission and applies the
// model and reasoning effort through the session's config options (or the
// older modes/models fields).
func (d *acpDriver) configure(sid string, sess json.RawMessage, t *turnSpec) error {
	var s struct {
		ConfigOptions []struct {
			ID       string `json:"id"`
			Category string `json:"category"`
			Current  any    `json:"currentValue"`
			Options  []struct {
				Value string `json:"value"`
				Name  string `json:"name"`
			} `json:"options"`
		} `json:"configOptions"`
		Modes *struct {
			Available []struct {
				ID string `json:"id"`
			} `json:"availableModes"`
		} `json:"modes"`
		Models *struct {
			Available []struct {
				ModelID string `json:"modelId"`
				Name    string `json:"name"`
			} `json:"availableModels"`
		} `json:"models"`
	}
	_ = json.Unmarshal(sess, &s)
	set := func(id, value string) error {
		_, err := d.rpc.call("session/set_config_option", map[string]any{"sessionId": sid, "configId": id, "value": value}, time.Minute)
		return err
	}
	modelSet := t.Model == ""
	var available []string
	for _, o := range s.ConfigOptions {
		cat := strings.ToLower(firstNonEmpty(o.Category, o.ID))
		pick := func(match func(v, n string) bool) string {
			for _, op := range o.Options {
				if match(strings.ToLower(op.Value), strings.ToLower(op.Name)) {
					return op.Value
				}
			}
			return ""
		}
		switch {
		case cat == "mode":
			if v := pick(func(v, _ string) bool { return autoMode(v) }); v != "" && v != o.Current {
				_ = set(o.ID, v)
			}
		case cat == "model" && t.Model != "":
			want := strings.ToLower(t.Model)
			v := pick(func(v, n string) bool { return v == want || n == want })
			if v == "" {
				v = pick(func(v, n string) bool { return strings.Contains(v, want) || strings.Contains(n, want) })
			}
			for _, op := range o.Options {
				available = append(available, op.Value)
			}
			if v != "" {
				if err := set(o.ID, v); err != nil {
					return err
				}
				modelSet = true
			}
		case (cat == "thinking" || cat == "thought_level" || cat == "effort" || cat == "reasoning") && t.Effort != "":
			e := strings.ToLower(t.Effort)
			if e == "none" {
				e = "off"
			}
			if v := pick(func(v, n string) bool { return v == e || n == e }); v != "" {
				_ = set(o.ID, v)
			}
		}
	}
	if s.Modes != nil {
		for _, m := range s.Modes.Available {
			if autoMode(strings.ToLower(m.ID)) {
				_, _ = d.rpc.call("session/set_mode", map[string]any{"sessionId": sid, "modeId": m.ID}, time.Minute)
				break
			}
		}
	}
	if !modelSet && s.Models != nil {
		for _, m := range s.Models.Available {
			available = append(available, m.ModelID)
			if strings.EqualFold(m.ModelID, t.Model) || strings.EqualFold(m.Name, t.Model) {
				if _, err := d.rpc.call("session/set_model", map[string]any{"sessionId": sid, "modelId": m.ModelID}, time.Minute); err != nil {
					return err
				}
				modelSet = true
				break
			}
		}
	}
	if !modelSet && len(available) > 0 {
		return fmt.Errorf("model %q is not available in %s (available: %s)", t.Model, d.rpc.r.h.name, strings.Join(available, ", "))
	}
	return nil
}

// autoMode: the permission mode that never asks (Vibe auto-approve, Kimi auto/yolo).
func autoMode(v string) bool {
	return v == "auto" || v == "yolo" || v == "never-ask" || v == "bypasspermissions" || strings.Contains(v, "auto-approve") || strings.Contains(v, "auto_approve")
}

func (d *acpDriver) flush(r *run) {
	d.mu.Lock()
	s := d.buf.String()
	d.buf.Reset()
	d.mu.Unlock()
	r.text(s)
}

func (d *acpDriver) line(r *run, b []byte) {
	m, ok := d.rpc.parse(b)
	if !ok {
		return
	}
	if len(m.ID) > 0 { // agent → client request
		if m.Method == "session/request_permission" {
			var p struct {
				Options []struct {
					OptionID string `json:"optionId"`
					Kind     string `json:"kind"`
				} `json:"options"`
			}
			_ = json.Unmarshal(m.Params, &p)
			best := ""
			for _, kind := range []string{"allow_always", "allow_once"} {
				for _, o := range p.Options {
					if o.Kind == kind && best == "" {
						best = o.OptionID
					}
				}
			}
			if best != "" {
				d.rpc.respond(m.ID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": best}})
			} else {
				d.rpc.respond(m.ID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
			}
			return
		}
		d.rpc.respondErr(m.ID, -32601, "not supported by the CrowdVM agent server: "+m.Method)
		return
	}
	if m.Method != "session/update" {
		return
	}
	var p struct {
		Update struct {
			Kind       string          `json:"sessionUpdate"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"toolCallId"`
			Title      string          `json:"title"`
			ToolKind   string          `json:"kind"`
			Status     string          `json:"status"`
			RawInput   any             `json:"rawInput"`
			RawOutput  any             `json:"rawOutput"`
		} `json:"update"`
	}
	_ = json.Unmarshal(m.Params, &p)
	u := p.Update
	d.mu.Lock()
	if !d.prompting {
		d.mu.Unlock()
		return
	}
	if d.tools == nil {
		d.tools = map[string]map[string]any{}
	}
	switch u.Kind {
	case "agent_message_chunk":
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(u.Content, &c)
		d.buf.WriteString(c.Text)
		d.mu.Unlock()
		r.partial(c.Text)
		return
	case "tool_call", "tool_call_update":
		t := d.tools[u.ToolCallID]
		if t == nil {
			t = map[string]any{}
			d.tools[u.ToolCallID] = t
		}
		if u.Title != "" {
			t["title"] = u.Title
		}
		if u.ToolKind != "" {
			t["kind"] = u.ToolKind
		}
		if u.RawInput != nil {
			t["input"] = u.RawInput
		}
		if u.Status != "completed" && u.Status != "failed" {
			d.mu.Unlock()
			return
		}
		delete(d.tools, u.ToolCallID)
		d.mu.Unlock()
		d.flush(r)
		out := acpToolOutput(u.Content)
		if out == "" && u.RawOutput != nil {
			out = string(mustJSON(u.RawOutput))
		}
		name := firstNonEmpty(str(t["title"]), str(t["kind"]), "tool")
		r.tool(u.ToolCallID, name, t["input"], u.Status == "failed", out)
		return
	}
	d.mu.Unlock()
}

// acpToolOutput flattens ToolCallContent ([{type:"content",content:{...}} | {type:"diff",...}]).
func acpToolOutput(raw json.RawMessage) string {
	var items []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Path    string          `json:"path"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, it := range items {
		switch it.Type {
		case "content":
			var c struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(it.Content, &c)
			parts = append(parts, c.Text)
		case "diff":
			parts = append(parts, "edited "+it.Path)
		}
	}
	return strings.Join(parts, "\n")
}

func (d *acpDriver) steer(r *run, s *steerRec) bool { return false }

func (d *acpDriver) cancel(r *run) bool {
	d.mu.Lock()
	sid := d.sessionID
	d.mu.Unlock()
	if sid == "" {
		return false
	}
	d.rpc.notify("session/cancel", map[string]any{"sessionId": sid})
	return true
}
