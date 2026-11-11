package agents

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func TestMain(m *testing.M) {
	os.Setenv("CROWDVM_FAKE_HARNESS", "1")
	os.Exit(m.Run())
}

type env struct {
	t   *testing.T
	m   *Manager
	srv *httptest.Server

	mu      sync.Mutex
	emitted []proto.GuestEvent
}

func newEnv(t *testing.T, dataDir string) *env {
	e := &env{t: t}
	e.m = New(Options{Home: t.TempDir(), Unprivileged: true, DataDir: dataDir, SandboxID: "bx_test",
		Emit: func(ev proto.GuestEvent) { e.mu.Lock(); e.emitted = append(e.emitted, ev); e.mu.Unlock() }})
	mux := http.NewServeMux()
	e.m.Register(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) do(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) prompt(body map[string]any) (promptID, convID string) {
	e.t.Helper()
	code, out := e.do("POST", "/v1/prompt", body)
	if code != 202 {
		e.t.Fatalf("prompt: %d %v", code, out)
	}
	return out["promptId"].(string), out["conversationId"].(string)
}

func (e *env) waitStatus(id string, want ...string) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, out := e.do("GET", "/v1/prompts/"+id, nil)
		run, _ := out["promptRun"].(map[string]any)
		for _, w := range want {
			if run != nil && run["status"] == w {
				return run
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, out := e.do("GET", "/v1/prompts/"+id, nil)
	e.t.Fatalf("prompt %s never reached %v: %v", id, want, out)
	return nil
}

func (e *env) events(query string) []map[string]any {
	e.t.Helper()
	code, out := e.do("GET", "/v1/events?"+query, nil)
	if code != 200 {
		e.t.Fatalf("events: %d %v", code, out)
	}
	var evs []map[string]any
	for _, x := range out["events"].([]any) {
		evs = append(evs, x.(map[string]any))
	}
	return evs
}

func responses(evs []map[string]any, task string) (texts []string, tools []any) {
	for _, e := range evs {
		if e["type"] != "response" || (task != "" && e["taskId"] != task) {
			continue
		}
		d := e["data"].(map[string]any)
		if d["is_streaming"] == true {
			continue
		}
		if c, _ := d["content"].(string); c != "" {
			texts = append(texts, c)
		}
		if t, ok := d["tools"].([]any); ok {
			tools = append(tools, t...)
		}
	}
	return
}

func TestPromptEventsConversationsSteerInterruptPersistence(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir)

	// --- prompt -> events
	p1, c1 := e.prompt(map[string]any{"provider": "fake", "prompt": "hello"})
	run := e.waitStatus(p1, "finished")
	if run["conversationId"] != c1 || run["done"] != true {
		t.Fatalf("run: %v", run)
	}
	evs := e.events("sort=asc&conversation=" + c1)
	if evs[0]["type"] != "prompt" || evs[0]["id"] != p1 || evs[0]["data"].(map[string]any)["status"] != "finished" {
		t.Fatalf("first event: %v", evs[0])
	}
	texts, tools := responses(evs, p1)
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "echo: hello (session=") || !strings.HasSuffix(texts[0], "resumed=false)") {
		t.Fatalf("texts: %v", texts)
	}
	tool := tools[0].(map[string]any)
	if tool["use"].(map[string]any)["name"] != "Bash" || tool["result"].(map[string]any)["content"] != "hi\n" {
		t.Fatalf("tool: %v", tool)
	}
	for _, ev := range evs {
		if ev["conversationId"] != c1 {
			t.Fatalf("event without conversation: %v", ev)
		}
	}
	session := strings.TrimSuffix(strings.SplitN(texts[0], "session=", 2)[1], " resumed=false)")

	// --- continue the current conversation: same native session, resumed
	p2, c := e.prompt(map[string]any{"prompt": "again"})
	e.waitStatus(p2, "finished")
	if c != c1 {
		t.Fatalf("expected current conversation %s, got %s", c1, c)
	}
	texts, _ = responses(e.events("sort=asc"), p2)
	if want := "echo: again (session=" + session + " resumed=true)"; len(texts) != 1 || texts[0] != want {
		t.Fatalf("got %v want %q", texts, want)
	}

	// --- new conversation, list
	p3, c2 := e.prompt(map[string]any{"provider": "fake", "new": true, "prompt": "fail please"})
	if c2 == c1 {
		t.Fatal("new conversation reused id")
	}
	run = e.waitStatus(p3, "failed")
	if run["error"] != "fake failure" {
		t.Fatalf("failed run: %v", run)
	}
	_, out := e.do("GET", "/v1/conversations", nil)
	convs := out["conversations"].([]any)
	if len(convs) != 2 || convs[0].(map[string]any)["id"] != c2 || convs[0].(map[string]any)["current"] != true ||
		convs[1].(map[string]any)["prompts"].(float64) != 2 || convs[1].(map[string]any)["lastHarness"] != "fake" {
		t.Fatalf("conversations: %v", convs)
	}

	// --- steer with no running turn
	code, out := e.do("POST", "/v1/steer", map[string]any{"message": "x", "conversation": c1})
	if code != 409 || out["code"] != "no_running_turn" {
		t.Fatalf("steer idle: %d %v", code, out)
	}

	// --- native steer
	p4, _ := e.prompt(map[string]any{"conversationId": c1, "prompt": "sleep:1500"})
	e.waitStatus(p4, "running")
	if !e.m.Busy() {
		t.Fatal("Busy() false while running")
	}
	out = e.steer(c1, "also do X")
	if out["native"] != true || out["mode"] != "native" || out["promptId"] == "" {
		t.Fatalf("steer: %v", out)
	}
	e.waitStatus(p4, "finished")
	evs = e.events("sort=asc&type=steer,response&conversation=" + c1)
	texts, _ = responses(evs, p4)
	if strings.Join(texts, "|") != "steered: also do X|slept 1500ms" {
		t.Fatalf("steer texts: %v", texts)
	}
	var steerEv map[string]any
	for _, ev := range evs {
		if ev["type"] == "steer" {
			steerEv = ev
		}
	}
	if steerEv == nil || steerEv["data"].(map[string]any)["mode"] != "native" || steerEv["taskId"] != p4 {
		t.Fatalf("steer event: %v", steerEv)
	}

	// --- fallback steer (harness without a mid-turn channel): interrupt + continue same session
	p5, c3 := e.prompt(map[string]any{"provider": "fake-fallback", "new": true, "prompt": "sleep:5000"})
	e.waitStatus(p5, "running")
	for i := 0; e.session(c3) == "" && i < 500; i++ { // harness reported its session
		time.Sleep(10 * time.Millisecond)
	}
	out = e.steer(c3, "use tabs")
	if out["native"] != false || out["mode"] != "fallback" {
		t.Fatalf("fallback steer: %v", out)
	}
	e.waitStatus(p5, "finished")
	texts, _ = responses(e.events("sort=asc"), p5)
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "echo: Continue what you were doing. The user sent a new instruction: use tabs") ||
		!strings.HasSuffix(texts[0], "resumed=true)") {
		t.Fatalf("fallback texts: %v", texts)
	}

	// --- interrupt one conversation; queued prompts are dropped too
	p6, _ := e.prompt(map[string]any{"conversationId": c1, "prompt": "sleep:20000"})
	p7, _ := e.prompt(map[string]any{"conversationId": c1, "prompt": "queued behind"})
	e.waitStatus(p6, "running")
	if s := e.waitStatus(p7, "queued"); s["done"] != false {
		t.Fatal(s)
	}
	start := time.Now()
	code, out = e.do("POST", "/v1/interrupt?conversation="+c1, nil)
	if code != 200 || out["type"] != "sandbox.interrupted" || out["conversationId"] != c1 {
		t.Fatalf("interrupt: %d %v", code, out)
	}
	e.waitStatus(p6, "interrupted")
	e.waitStatus(p7, "interrupted")
	if time.Since(start) > 5*time.Second {
		t.Fatal("interrupt too slow")
	}
	time.Sleep(50 * time.Millisecond)
	if e.m.Busy() {
		t.Fatal("Busy() after interrupt")
	}

	// --- pagination, ETag
	all := e.events("sort=asc&limit=200")
	var walked []map[string]any
	cursor := ""
	for {
		code, out := e.do("GET", "/v1/events?limit=3&cursor="+cursor, nil)
		if code != 200 {
			t.Fatal(out)
		}
		for _, x := range out["events"].([]any) {
			walked = append(walked, x.(map[string]any))
		}
		pi := out["pageInfo"].(map[string]any)
		if pi["hasMore"] != true {
			break
		}
		cursor = pi["nextCursor"].(string)
	}
	if len(walked) != len(all) || walked[0]["id"] != all[len(all)-1]["id"] {
		t.Fatalf("desc walk %d events, asc %d", len(walked), len(all))
	}
	// client-computed cursor (Boat docs: base64url of "<timestamp>:<id>")
	mid := all[len(all)/2]
	cur := base64.RawURLEncoding.EncodeToString([]byte(jsonNum(mid["timestamp"]) + ":" + mid["id"].(string)))
	if rest := e.events("sort=asc&cursor=" + cur); len(rest) != len(all)-len(all)/2-1 {
		t.Fatalf("after cursor: %d", len(rest))
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/events", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	if resp, _ = http.DefaultClient.Do(req); resp.StatusCode != 304 {
		t.Fatalf("etag: %d", resp.StatusCode)
	}

	e.mu.Lock()
	nEmit := len(e.emitted)
	e.mu.Unlock()
	if nEmit < len(all) || e.emitted[0].Type != "prompt.event" {
		t.Fatalf("emitted %d events for %d", nEmit, len(all))
	}

	// --- persistence across a Manager restart
	e2 := newEnv(t, dir)
	if got := e2.events("sort=asc&limit=200"); len(got) != len(all) {
		t.Fatalf("after restart: %d events, want %d", len(got), len(all))
	}
	_, out = e2.do("GET", "/v1/conversations", nil)
	if n := len(out["conversations"].([]any)); n != 3 {
		t.Fatalf("after restart: %d conversations", n)
	}
	if run := e2.waitStatus(p4, "finished"); run["done"] != true {
		t.Fatal(run)
	}
	p8, c := e2.prompt(map[string]any{"conversationId": c1, "prompt": "after restart"})
	e2.waitStatus(p8, "finished")
	texts, _ = responses(e2.events("sort=asc"), p8)
	if c != c1 || len(texts) != 1 || texts[0] != "echo: after restart (session="+session+" resumed=true)" {
		t.Fatalf("resume after restart: %v", texts)
	}
}

func (e *env) steer(conv, msg string) map[string]any {
	e.t.Helper()
	for i := 0; i < 200; i++ {
		code, out := e.do("POST", "/v1/steer", map[string]any{"message": msg, "conversation": conv})
		if code == 200 {
			return out
		}
		if d, _ := out["error"].(map[string]any)["details"].(map[string]any); d["reason"] != "turn_not_started" {
			e.t.Fatalf("steer: %d %v", code, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("turn never started")
	return nil
}

func (e *env) session(conv string) string {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	return e.m.convs[conv].Session
}

func jsonNum(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestRestartMarksRunningInterrupted(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir)
	p, _ := e.prompt(map[string]any{"provider": "fake", "prompt": "sleep:10000"})
	e.waitStatus(p, "running")
	e2 := newEnv(t, dir)
	run := e2.waitStatus(p, "interrupted")
	if !strings.Contains(run["error"].(string), "restarted") {
		t.Fatal(run)
	}
	e.do("POST", "/v1/interrupt", nil) // clean up the old manager's process
	e.waitStatus(p, "interrupted")
}

func TestBackgroundChildDoesNotHangTurn(t *testing.T) {
	e := newEnv(t, t.TempDir())
	start := time.Now()
	p, _ := e.prompt(map[string]any{"provider": "fake", "prompt": "bg"})
	e.waitStatus(p, "finished")
	if d := time.Since(start); d > 7*time.Second {
		t.Fatalf("turn took %v", d)
	}
}

func TestValidation(t *testing.T) {
	e := newEnv(t, t.TempDir())
	cases := []struct {
		body map[string]any
		code int
		err  string
	}{
		{map[string]any{"provider": "fake"}, 400, "prompt_required"},
		{map[string]any{"provider": "nope", "prompt": "x"}, 400, "invalid_provider"},
		{map[string]any{"provider": "fake", "prompt": "x", "new": true, "conversationId": "abc"}, 400, "invalid_request"},
		{map[string]any{"provider": "fake", "prompt": "x", "conversationId": "abc"}, 404, "conversation_not_found"},
		{map[string]any{"provider": "claude", "model": "claude-sonnet-5-5", "fast": true, "prompt": "x"}, 400, "fast_mode_not_supported"},
		{map[string]any{"provider": "kimi-code", "prompt": "x", "env": map[string]string{"PATH": "/nonexistent"}}, 409, "provider_not_installed"},
	}
	for _, c := range cases {
		code, out := e.do("POST", "/v1/prompt", c.body)
		if code != c.code || out["code"] != c.err {
			t.Errorf("%v: got %d %v", c.body, code, out["code"])
		}
	}
	if code, _ := e.do("GET", "/v1/prompts/prompt_nope", nil); code != 404 {
		t.Error("unknown prompt", code)
	}
	if code, out := e.do("GET", "/v1/events?cursor=!!", nil); code != 400 {
		t.Error("bad cursor", code, out)
	}
	if code, out := e.do("GET", "/v1/events", nil); code != 200 || len(out["events"].([]any)) != 0 || out["type"] != "events.list" {
		t.Error("empty events", code, out)
	}
}

func TestProxyEnvAndCatalog(t *testing.T) {
	m := New(Options{Home: t.TempDir(), Unprivileged: true, AIProxy: &proto.AIProxy{BaseURL: "http://10.0.2.2:8420/ai-proxy/", Token: "tok"}})
	pe := m.ProxyEnv()
	if pe["ANTHROPIC_BASE_URL"] != "http://10.0.2.2:8420/ai-proxy/anthropic" || pe["OPENAI_API_KEY"] != "tok" {
		t.Fatal(pe)
	}
	env := m.envMap(map[string]string{"ANTHROPIC_API_KEY": "real"})
	if env["ANTHROPIC_API_KEY"] != "real" || env["DISPLAY"] == "" || env["HOME"] != m.o.Home {
		t.Fatal("env precedence")
	}
	_, args, _ := codexCommand(m, &turnSpec{Prompt: "x", Session: "s1"})
	if !strings.Contains(strings.Join(args, " "), `base_url="http://10.0.2.2:8420/ai-proxy/openai/v1"`) || args[0] != "app-server" {
		t.Fatal(args)
	}
	m.SetAIProxy(&proto.AIProxy{BaseURL: "http://other/p", Token: "t2"})
	if pe := m.ProxyEnv(); pe["OPENAI_BASE_URL"] != "http://other/p/openai/v1" || pe["OPENAI_API_KEY"] != "t2" {
		t.Fatal("SetAIProxy", pe)
	}
	seen := map[string]bool{}
	for _, p := range Catalog() {
		seen[p.Provider] = true
		if harnesses[p.Provider] == nil {
			t.Errorf("catalog provider %s has no harness", p.Provider)
		}
	}
	for _, p := range []string{"claude", "codex", "opencode", "pi", "prime-agent", "kimi", "mistral"} {
		if !seen[p] {
			t.Errorf("catalog lacks %s", p)
		}
	}
}

// parserRun feeds recorded harness output through a parser.
func parserRun(t *testing.T, h *harness, lines string) (*Manager, *run) {
	m := New(Options{Home: t.TempDir(), Unprivileged: true})
	c := &conv{ID: "c"}
	m.convs["c"] = c
	p := &promptRec{ID: "p", ConversationID: "c"}
	r := &run{m: m, c: c, p: p, h: h, done: make(chan struct{})}
	for _, l := range strings.Split(strings.TrimSpace(lines), "\n") {
		h.parse(r, []byte(l))
	}
	r.flushTools()
	return m, r
}

func TestParseOpencode(t *testing.T) {
	// recorded from `opencode run --format json` (opencode 1.19.5)
	m, r := parserRun(t, harnesses["opencode"], `
{"type":"step_start","timestamp":1794152053631,"sessionID":"ses_e44","part":{"type":"step-start"}}
{"type":"tool_use","timestamp":1794152053840,"sessionID":"ses_e44","part":{"type":"tool","tool":"bash","callID":"call-78","state":{"status":"completed","input":{"command":"echo hello"},"output":"hello\n","metadata":{"exit":0},"title":"echo hello"}}}
{"type":"step_finish","timestamp":1794152053840,"sessionID":"ses_e44","part":{"reason":"tool-calls","type":"step-finish"}}
{"type":"text","timestamp":1794152054857,"sessionID":"ses_e44","part":{"type":"text","text":"DONE"}}
`)
	if r.session != "ses_e44" || m.convs["c"].Session != "ses_e44" || len(m.events) != 2 {
		t.Fatalf("session %q events %d", r.session, len(m.events))
	}
	u := m.events[0].Data["tools"].([]any)[0].(map[string]any)
	if u["use"].(map[string]any)["name"] != "bash" || u["result"].(map[string]any)["content"] != "hello\n" || m.events[1].Data["content"] != "DONE" {
		t.Fatal(m.events[0].Data, m.events[1].Data)
	}
}
