package agents

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The fake harness (provider "fake", enabled by CROWDVM_FAKE_HARNESS=1) re-execs
// the current binary with fakeChildEnv set. The child speaks Claude Code's
// stream-json protocol, so tests exercise the real Claude parser, native steer
// and resume paths without any model. Deterministic behaviour per message:
//
//	"sleep:<ms> ..."  sleeps; steers received meanwhile are acked and answered
//	"fail ..."        result with is_error
//	"bg"              starts a child that keeps stdout open, then finishes
//	anything else     one Bash tool call + "echo: <prompt> (session=<id> resumed=<bool>)"
const fakeChildEnv = "CROWDVM_FAKE_HARNESS_CHILD"

func fakeEnabled() bool { return os.Getenv("CROWDVM_FAKE_HARNESS") == "1" }

func init() {
	switch os.Getenv(fakeChildEnv) {
	case "1":
		fakeMain(os.Args[1:])
		os.Exit(0)
	case "codex":
		fakeCodexMain()
		os.Exit(0)
	case "acp":
		fakeACPMain()
		os.Exit(0)
	}
}

// fakeProto returns a fake protocol harness (Codex app-server or ACP) that
// re-execs the test binary.
func fakeProto(mode string, h harness) *harness {
	h.name, h.sessions = "fake-"+mode, true
	h.command = func(*Manager, *turnSpec) (string, []string, []byte) {
		self, _ := os.Executable()
		return self, []string{mode}, nil
	}
	h.env = func(_ *Manager, _ *turnSpec, env map[string]string) { env[fakeChildEnv] = mode }
	return &h
}

func fakeCommand(_ *Manager, t *turnSpec) (string, []string, []byte) {
	self, _ := os.Executable()
	return self, append([]string{"fake-harness"}, claudeArgs(t)...), claudeUserLine(t.Prompt)
}

func fakeMain(args []string) {
	session, resumed, model := "", false, "fake-1"
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--session-id":
			session = args[i+1]
		case "--resume":
			session, resumed = args[i+1], true
		case "--model":
			model = args[i+1]
		}
	}
	out := json.NewEncoder(os.Stdout)
	emit := func(v map[string]any) { v["session_id"] = session; _ = out.Encode(v) }
	text := func(s string) {
		emit(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{
			"model": model, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": s}}}})
	}
	msgs := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			var ev struct {
				Message struct{ Content string } `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				msgs <- ev.Message.Content
			}
		}
		close(msgs)
	}()
	ack := func(s string) {
		emit(map[string]any{"type": "user", "isReplay": true, "parent_tool_use_id": nil,
			"message": map[string]any{"role": "user", "content": s}})
	}
	emit(map[string]any{"type": "system", "subtype": "init", "model": model, "cwd": mustCwd()})
	n := 0
	for msg := range msgs {
		ack(msg)
		n++
		switch {
		case strings.HasPrefix(msg, "sleep:"):
			f := strings.Fields(strings.TrimPrefix(msg, "sleep:"))
			ms := 0
			if len(f) > 0 {
				ms, _ = strconv.Atoi(f[0])
			}
			deadline := time.After(time.Duration(ms) * time.Millisecond)
			in := msgs
		loop:
			for {
				select {
				case s, ok := <-in:
					if !ok { // stdin closed: keep sleeping
						in = nil
						continue
					}
					ack(s)
					text("steered: " + s)
				case <-deadline:
					break loop
				}
			}
			text("slept " + strconv.Itoa(ms) + "ms")
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "ok"})
		case strings.HasPrefix(msg, "bg"): // leave a child holding stdout open
			c := exec.Command("sleep", "8")
			c.Stdout = os.Stdout
			_ = c.Start()
			text("started background child")
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "ok"})
		case strings.HasPrefix(msg, "fail"):
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "fake failure"})
		default:
			id := fmt.Sprintf("toolu_%d", n)
			first := strings.SplitN(msg, "\n", 2)[0]
			emit(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"model": model,
				"content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "echo hi"}}}}})
			emit(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"role": "user",
				"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": false, "content": "hi\n"}}}})
			text(fmt.Sprintf("echo: %s (session=%s resumed=%v)", first, session, resumed))
			emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "ok"})
		}
	}
}

func mustCwd() string { d, _ := os.Getwd(); return d }
