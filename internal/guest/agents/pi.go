package agents

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pi and Prime Agent: `<bin> --mode rpc` (JSONL commands on stdin, responses
// and session events on stdout). Steers use the native `steer` command, which
// pi delivers before the next model call of the running turn.

type piDriver struct {
	w waiters
}

func (d *piDriver) cmd(r *run, typ string, fields map[string]any, timeout time.Duration) (json.RawMessage, error) {
	id, ch := d.w.add()
	msg := map[string]any{"id": id, "type": typ}
	for k, v := range fields {
		msg[k] = v
	}
	if err := r.write(jsonLine(msg)); err != nil {
		d.w.deliver(id, rpcReply{})
		return nil, err
	}
	return d.w.wait(r, id, ch, typ, timeout)
}

func (d *piDriver) begin(r *run, t *turnSpec) {
	res, err := d.cmd(r, "get_state", nil, 2*time.Minute)
	if err != nil {
		r.endTurn(true, err.Error())
		return
	}
	var st struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &st)
	r.setSession(st.SessionID)
	fields := map[string]any{"message": t.Prompt}
	if imgs := piImages(t.Attachments); len(imgs) > 0 {
		fields["images"] = imgs
	}
	if _, err := d.cmd(r, "prompt", fields, 2*time.Minute); err != nil {
		r.endTurn(true, err.Error())
	}
}

// piImages inlines image attachments (≤ 10 MiB each) as base64 ImageContent.
func piImages(paths []string) []any {
	var out []any
	for _, p := range paths {
		if !isImage(p) {
			continue
		}
		st, err := os.Stat(p)
		if err != nil || st.Size() > 10<<20 {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString(b),
			"mimeType": mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))})
	}
	return out
}

func (d *piDriver) line(r *run, b []byte) {
	var ev struct {
		ID       string          `json:"id"`
		Type     string          `json:"type"`
		Success  bool            `json:"success"`
		Data     json.RawMessage `json:"data"`
		Error    string          `json:"error"`
		Message  json.RawMessage `json:"message"`
		Assist   map[string]any  `json:"assistantMessageEvent"`
		CallID   string          `json:"toolCallId"`
		ToolName string          `json:"toolName"`
		Args     any             `json:"args"`
		Result   json.RawMessage `json:"result"`
		IsError  bool            `json:"isError"`
		Aborted  bool            `json:"aborted"`
		Final    string          `json:"finalError"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	switch ev.Type {
	case "response":
		rep := rpcReply{result: ev.Data}
		if !ev.Success {
			rep.err = errors.New(firstNonEmpty(ev.Error, "command failed"))
		}
		d.w.deliver(ev.ID, rep)
	case "message_update":
		if ev.Assist["type"] == "text_delta" {
			r.partial(str(ev.Assist["delta"]))
		}
	case "message_end":
		var msg struct {
			Role         string          `json:"role"`
			Content      json.RawMessage `json:"content"`
			StopReason   string          `json:"stopReason"`
			ErrorMessage string          `json:"errorMessage"`
		}
		_ = json.Unmarshal(ev.Message, &msg)
		switch msg.Role {
		case "user":
			r.ackSteer(contentText(msg.Content))
		case "assistant":
			for _, b := range blocks(msg.Content) {
				if b.Type == "text" {
					r.text(b.Text)
				}
			}
			if msg.StopReason == "error" {
				r.fail(msg.ErrorMessage)
			}
		}
	case "tool_execution_start":
		r.toolUse(ev.CallID, ev.ToolName, ev.Args)
	case "tool_execution_end":
		var res struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(ev.Result, &res)
		r.toolResult(ev.CallID, ev.IsError, contentText(res.Content))
	case "auto_retry_end":
		if !ev.Success && ev.Final != "" {
			r.fail(ev.Final)
		}
	case "agent_settled":
		r.endTurn(false, "")
	}
}

func (d *piDriver) steer(r *run, s *steerRec) bool {
	_, err := d.cmd(r, "steer", map[string]any{"message": s.Message}, 30*time.Second)
	return err == nil
}

func (d *piDriver) cancel(r *run) bool { return false }

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
