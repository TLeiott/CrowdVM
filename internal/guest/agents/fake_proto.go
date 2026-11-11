package agents

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake Codex app-server and ACP agents for tests (see fakeProto). Prompts:
// "sleep:<ms> ..." keeps the turn running (steers / cancel arrive meanwhile),
// anything else answers "echo: <prompt> (session=<id> resumed=<bool>)".

type fakeRPC struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (f *fakeRPC) send(v map[string]any) {
	v["jsonrpc"] = "2.0"
	f.mu.Lock()
	_ = f.enc.Encode(v)
	f.mu.Unlock()
}

func (f *fakeRPC) notify(method string, params any) {
	f.send(map[string]any{"method": method, "params": params})
}

func fakeLines(fn func(m rpcMsg)) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			fn(m)
		}
	}
}

func sleepMs(text string) (int, bool) {
	if !strings.HasPrefix(text, "sleep:") {
		return 0, false
	}
	f := strings.Fields(strings.TrimPrefix(text, "sleep:"))
	ms := 0
	if len(f) > 0 {
		ms, _ = strconv.Atoi(f[0])
	}
	return ms, true
}

func fakeCodexMain() {
	f := &fakeRPC{enc: json.NewEncoder(os.Stdout)}
	var mu sync.Mutex
	thread, resumed, turnN := "", false, 0
	steers := make(chan string, 16)
	stop := make(chan struct{}, 1)
	var wg sync.WaitGroup
	text := func(turn, s string) {
		f.notify("item/agentMessage/delta", map[string]any{"threadId": thread, "turnId": turn, "itemId": "m", "delta": s})
		f.notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"type": "agentMessage", "id": "m", "text": s}})
	}
	fakeLines(func(m rpcMsg) {
		var p map[string]any
		_ = json.Unmarshal(m.Params, &p)
		reply := func(res any) { f.send(map[string]any{"id": m.ID, "result": res}) }
		switch m.Method {
		case "initialize":
			reply(map[string]any{"userAgent": "fake"})
		case "thread/start":
			mu.Lock()
			thread = "thr_" + strconv.FormatInt(time.Now().UnixNano(), 36)
			mu.Unlock()
			reply(map[string]any{"thread": map[string]any{"id": thread}})
		case "thread/resume":
			mu.Lock()
			thread, resumed = str(p["threadId"]), true
			mu.Unlock()
			reply(map[string]any{"thread": map[string]any{"id": thread}})
		case "turn/start":
			mu.Lock()
			turnN++
			turn := "turn_" + strconv.Itoa(turnN)
			mu.Unlock()
			reply(map[string]any{"turn": map[string]any{"id": turn, "status": "inProgress"}})
			in, _ := p["input"].([]any)
			prompt := str(in[0].(map[string]any)["text"])
			wg.Add(1)
			go func() {
				defer wg.Done()
				f.notify("turn/started", map[string]any{"threadId": thread, "turn": map[string]any{"id": turn}})
				status := "completed"
				if ms, ok := sleepMs(prompt); ok {
					deadline := time.After(time.Duration(ms) * time.Millisecond)
				loop:
					for {
						select {
						case s := <-steers:
							f.notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{
								"type": "userMessage", "id": "u", "content": []any{map[string]any{"type": "text", "text": s}}}})
							text(turn, "steered: "+s)
						case <-stop:
							status = "interrupted"
							break loop
						case <-deadline:
							break loop
						}
					}
				}
				if status == "completed" {
					f.notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{
						"type": "commandExecution", "id": "c1", "command": "echo hi", "aggregatedOutput": "hi\n", "exitCode": 0, "status": "completed"}})
					text(turn, fmt.Sprintf("echo: %s (session=%s resumed=%v)", strings.SplitN(prompt, "\n", 2)[0], thread, resumed))
				}
				f.notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": turn, "status": status}})
			}()
		case "turn/steer":
			in, _ := p["input"].([]any)
			if txt := str(in[0].(map[string]any)["text"]); txt == "REJECT" { // tests: a harness that refuses a steer
				f.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "steer rejected"}})
				return
			}
			steers <- str(in[0].(map[string]any)["text"])
			reply(map[string]any{"turnId": p["expectedTurnId"]})
		case "turn/interrupt":
			stop <- struct{}{}
			reply(map[string]any{})
		}
	})
	wg.Wait()
}

func fakeACPMain() {
	f := &fakeRPC{enc: json.NewEncoder(os.Stdout)}
	var mu sync.Mutex
	session, loaded, mode := "", false, ""
	cancel := make(chan struct{}, 1)
	var wg sync.WaitGroup
	update := func(u map[string]any) {
		f.notify("session/update", map[string]any{"sessionId": session, "update": u})
	}
	sessionResult := func() map[string]any {
		return map[string]any{"sessionId": session, "configOptions": []any{map[string]any{"id": "mode", "category": "mode", "currentValue": "ask",
			"options": []any{map[string]any{"value": "ask", "name": "Ask"}, map[string]any{"value": "auto-approve", "name": "Auto Approve"}}}}}
	}
	fakeLines(func(m rpcMsg) {
		var p map[string]any
		_ = json.Unmarshal(m.Params, &p)
		reply := func(res any) { f.send(map[string]any{"id": m.ID, "result": res}) }
		switch m.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true}})
		case "session/new":
			mu.Lock()
			session = "ses_" + strconv.FormatInt(time.Now().UnixNano(), 36)
			mu.Unlock()
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "notice before prompt"}})
			reply(sessionResult())
		case "session/load":
			mu.Lock()
			session, loaded = str(p["sessionId"]), true
			mu.Unlock()
			update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "replayed history"}})
			reply(sessionResult())
		case "session/set_config_option":
			mode = str(p["value"])
			reply(map[string]any{})
		case "session/cancel":
			select {
			case cancel <- struct{}{}:
			default:
			}
		case "session/prompt":
			blocks, _ := p["prompt"].([]any)
			prompt := str(blocks[0].(map[string]any)["text"])
			id := m.ID
			wg.Add(1)
			go func() {
				defer wg.Done()
				reason := "end_turn"
				if ms, ok := sleepMs(prompt); ok {
					select {
					case <-cancel:
						reason = "cancelled"
					case <-time.After(time.Duration(ms) * time.Millisecond):
					}
				}
				if reason == "end_turn" {
					update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "Run echo", "kind": "execute", "status": "pending", "rawInput": map[string]any{"command": "echo hi"}})
					update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed",
						"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "hi"}}}})
					first := strings.SplitN(prompt, "\n", 2)[0]
					for _, part := range []string{"echo: ", first, fmt.Sprintf(" (session=%s resumed=%v mode=%s)", session, loaded, mode)} {
						update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": part}})
					}
				}
				f.send(map[string]any{"id": id, "result": map[string]any{"stopReason": reason}})
			}()
		}
	})
	wg.Wait()
}
