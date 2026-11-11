package agents

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func lastText(t *testing.T, e *env, task string) []string {
	t.Helper()
	texts, _ := responses(e.events("sort=asc&limit=200"), task)
	return texts
}

// A harness that refuses a steer while its turn runs: 409 no_running_turn,
// reason delivery_failed (Boat), and the steer event says failed.
func TestSteerDeliveryFailed(t *testing.T) {
	e := newEnv(t, t.TempDir())
	p, c := e.prompt(map[string]any{"provider": "fake-codex", "prompt": "sleep:3000"})
	e.waitStatus(p, "running")
	for i := 0; i < 300; i++ {
		e.m.mu.Lock()
		r := e.m.convs[c].run
		e.m.mu.Unlock()
		if r != nil {
			if d, ok := r.drv.(*codexDriver); ok {
				if _, turn := d.ids(); turn != "" {
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, out := e.do("POST", "/v1/steer", map[string]any{"message": "REJECT", "conversation": c})
	d, _ := out["error"].(map[string]any)["details"].(map[string]any)
	if code != 409 || d["reason"] != "delivery_failed" {
		t.Fatalf("steer: %d %v", code, out)
	}
	for _, ev := range e.events("sort=asc&type=steer") {
		if dd := ev["data"].(map[string]any); dd["status"] != "failed" || dd["mode"] != "failed" {
			t.Fatalf("steer event %v", ev)
		}
	}
	e.waitStatus(p, "finished")
}

// Codex app-server driver: session, native steer (turn/steer), streaming
// partials, resume.
func TestCodexDriverNativeSteer(t *testing.T) {
	e := newEnv(t, t.TempDir())
	old := partialEvery
	partialEvery = 0
	t.Cleanup(func() { partialEvery = old })

	p1, c := e.prompt(map[string]any{"provider": "fake-codex", "prompt": "hello"})
	e.waitStatus(p1, "finished")
	session := e.session(c)
	texts := lastText(t, e, p1)
	if !strings.HasPrefix(session, "thr_") || len(texts) != 1 || texts[0] != "echo: hello (session="+session+" resumed=false)" {
		t.Fatalf("session %q texts %v", session, texts)
	}
	evs := e.events("sort=asc&type=response")
	partials, tools := 0, 0
	for _, ev := range evs {
		d := ev["data"].(map[string]any)
		if d["is_streaming"] == true {
			partials++
		}
		if ts, ok := d["tools"].([]any); ok && len(ts) > 0 {
			tools++
		}
	}
	if partials == 0 || tools != 1 {
		t.Fatalf("partials %d tools %d: %v", partials, tools, evs)
	}

	p2, _ := e.prompt(map[string]any{"conversationId": c, "prompt": "sleep:1500"})
	e.waitStatus(p2, "running")
	for i := 0; i < 300; i++ { // until turn/start answered
		e.m.mu.Lock()
		r := e.m.convs[c].run
		e.m.mu.Unlock()
		if r != nil {
			if d, ok := r.drv.(*codexDriver); ok {
				if _, turn := d.ids(); turn != "" {
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	out := e.steer(c, "use tabs")
	if out["mode"] != "native" || out["native"] != true {
		t.Fatalf("steer: %v", out)
	}
	e.waitStatus(p2, "finished")
	texts = lastText(t, e, p2)
	if len(texts) != 2 || texts[0] != "steered: use tabs" || texts[1] != "echo: sleep:1500 (session="+session+" resumed=true)" {
		t.Fatalf("texts %v", texts)
	}
	for _, ev := range e.events("sort=asc&type=steer") {
		if ev["data"].(map[string]any)["mode"] != "native" {
			t.Fatalf("steer event %v", ev)
		}
	}
	// persisted log has no partials
	b, _ := os.ReadFile(filepath.Join(e.m.o.DataDir, "events.jsonl"))
	if strings.Contains(string(b), `"is_streaming":true`) {
		t.Fatal("partials persisted")
	}
}

// ACP driver: ignores updates outside a prompt, auto-approve mode, fallback
// steer through session/cancel + session/load.
func TestACPDriverFallbackSteer(t *testing.T) {
	e := newEnv(t, t.TempDir())
	p1, c := e.prompt(map[string]any{"provider": "fake-acp", "prompt": "hi"})
	e.waitStatus(p1, "finished")
	session := e.session(c)
	texts, tools := responses(e.events("sort=asc"), p1)
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "echo: <platform-note>") ||
		!strings.HasSuffix(texts[0], "(session="+session+" resumed=false mode=auto-approve)") || len(tools) != 1 {
		t.Fatalf("texts %q tools %v", texts, tools)
	}

	p2, _ := e.prompt(map[string]any{"conversationId": c, "prompt": "sleep:10000"})
	e.waitStatus(p2, "running")
	time.Sleep(300 * time.Millisecond) // session/prompt sent
	start := time.Now()
	out := e.steer(c, "use tabs")
	if out["mode"] != "fallback" {
		t.Fatalf("steer %v", out)
	}
	e.waitStatus(p2, "finished")
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel did not stop the turn")
	}
	texts = lastText(t, e, p2)
	want := "echo: Continue what you were doing. The user sent a new instruction: use tabs (session=" + session + " resumed=true mode=auto-approve)"
	if len(texts) != 1 || texts[0] != want {
		t.Fatalf("got %q want %q", texts, want)
	}
}

func TestSetupBlockedHoldsPrompts(t *testing.T) {
	var blocked atomic.Bool
	blocked.Store(true)
	old := setupPoll
	setupPoll = 20 * time.Millisecond
	t.Cleanup(func() { setupPoll = old })
	e := newEnv(t, t.TempDir())
	e.m.o.SetupBlocked = blocked.Load
	p, _ := e.prompt(map[string]any{"provider": "fake", "prompt": "x"})
	time.Sleep(300 * time.Millisecond)
	if s := e.waitStatus(p, "queued"); s["done"] != false {
		t.Fatal(s)
	}
	blocked.Store(false)
	e.waitStatus(p, "finished")
}

func TestPromptPreflight(t *testing.T) {
	e := newEnv(t, t.TempDir())
	// fast with no model: checked against the harness default
	catalog = append(catalog, ProviderModels{Provider: "fake", DefaultModel: "slow-1", Models: []Model{{ID: "slow-1"}}})
	t.Cleanup(func() { catalog = catalog[:len(catalog)-1] })
	code, out := e.do("POST", "/v1/prompt", map[string]any{"provider": "fake", "fast": true, "prompt": "x"})
	if code != 400 || out["code"] != "fast_mode_not_supported" {
		t.Fatalf("fast: %d %v", code, out)
	}
	// attachment limits
	var many []string
	for i := 0; i < 13; i++ {
		many = append(many, fmt.Sprintf("/nope/%d.txt", i))
	}
	code, out = e.do("POST", "/v1/prompt", map[string]any{"provider": "fake", "prompt": "x", "attachments": many})
	if code != 400 || out["code"] != "invalid_attachments" {
		t.Fatalf("attachments: %d %v", code, out)
	}
	big := filepath.Join(t.TempDir(), "big.bin")
	f, _ := os.Create(big)
	f.Truncate(26 << 20)
	f.Close()
	if err := checkAttachments([]string{big}, ""); err == nil {
		t.Fatal("26 MB attachment accepted")
	}
}

func TestProviderConfigured(t *testing.T) {
	for _, k := range []string{"MISTRAL_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "DEEPSEEK_API_KEY",
		"AWS_BEARER_TOKEN_BEDROCK", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_BASE_URL"} {
		t.Setenv(k, "")
	}
	m := New(Options{Home: t.TempDir(), Unprivileged: true})
	if ok, _ := m.providerConfigured("mistral", "", nil); ok {
		t.Fatal("mistral configured without a key")
	}
	if ok, _ := m.providerConfigured("mistral", "", map[string]string{"MISTRAL_API_KEY": "k"}); !ok {
		t.Fatal("request credential ignored")
	}
	if ok, _ := m.providerConfigured("claude", "", nil); ok {
		t.Fatal("claude configured without credentials")
	}
	os.MkdirAll(filepath.Join(m.o.Home, ".claude"), 0o755)
	os.WriteFile(filepath.Join(m.o.Home, ".claude", ".credentials.json"), []byte("{}"), 0o600)
	if ok, _ := m.providerConfigured("claude", "", nil); !ok {
		t.Fatal("claude sign-in file ignored")
	}
	if ok, _ := m.providerConfigured("opencode", "", nil); !ok {
		t.Fatal("opencode needs no key")
	}
	if got := friendlyError("AccessDeniedException: You don't have access to the model", map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "x", "AWS_REGION": "eu-west-1"}); !strings.Contains(got, "model access not granted in eu-west-1") {
		t.Fatal(got)
	}
}

func TestDeepSeekAndBedrockRouting(t *testing.T) {
	home := t.TempDir()
	m := New(Options{Home: home, Unprivileged: true})
	env := map[string]string{"DEEPSEEK_API_KEY": "dk", "ANTHROPIC_API_KEY": "ak"}
	claudeEnv(m, &turnSpec{Model: "deepseek-v4-pro"}, env)
	if env["ANTHROPIC_BASE_URL"] != "https://api.deepseek.com/anthropic" || env["ANTHROPIC_AUTH_TOKEN"] != "dk" || env["ANTHROPIC_API_KEY"] != "" {
		t.Fatal(env)
	}
	env = map[string]string{"DEEPSEEK_API_KEY": "dk", "ANTHROPIC_API_KEY": "ak"}
	claudeEnv(m, &turnSpec{Model: "claude-opus-5-5"}, env)
	if env["ANTHROPIC_API_KEY"] != "ak" || env["ANTHROPIC_AUTH_TOKEN"] != "" {
		t.Fatal("anthropic model rerouted", env)
	}
	if claudeRoute("", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_BEARER_TOKEN_BEDROCK": "x"}, home) != "bedrock" ||
		bedrockClaudeModel("") != "sonnet" || bedrockClaudeModel("claude-opus-5-5") != "opus" {
		t.Fatal("bedrock")
	}
	for k, want := range map[string]string{"DEEPSEEK_API_KEY": `model_provider="deepseek"`, "AWS_BEARER_TOKEN_BEDROCK": `model_provider="amazon-bedrock"`} {
		t.Setenv("OPENAI_API_KEY", "")
		t.Setenv("OPENAI_BASE_URL", "")
		t.Setenv("CODEX_API_KEY", "")
		spec := &turnSpec{Env: map[string]string{k: "x"}}
		_, args, _ := codexCommand(m, spec)
		if !strings.Contains(strings.Join(args, " "), want) {
			t.Fatalf("%s: %v", k, args)
		}
		if k == "AWS_BEARER_TOKEN_BEDROCK" && spec.Model != "openai.gpt-6.1-sol" {
			t.Fatal(spec.Model)
		}
	}
}

func TestEventCompaction(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CROWDVM_AGENT_EVENTS_MAX_BYTES", "20000")
	e := newEnv(t, dir)
	e.m.mu.Lock()
	for i := 0; i < 400; i++ {
		e.m.appendLocked("response", "c", "p", map[string]any{"content": strings.Repeat("x", 100), "n": i})
	}
	n := len(e.m.events)
	last := e.m.events[n-1]
	e.m.mu.Unlock()
	st, _ := os.Stat(filepath.Join(dir, "events.jsonl"))
	if n >= 400 || st.Size() > 20000 || last.Data["n"] != 399 {
		t.Fatalf("events %d file %d", n, st.Size())
	}
	e2 := newEnv(t, dir) // reload after compaction
	if len(e2.m.events) != n {
		t.Fatalf("reloaded %d want %d", len(e2.m.events), n)
	}
	evs := e2.events("sort=asc&limit=1")
	if evs[0]["data"].(map[string]any)["n"].(float64) == 0 {
		t.Fatal("oldest event kept")
	}
}

func TestComputerRegistrationAndMCP(t *testing.T) {
	t.Setenv("CROWDVM_COMPUTER_MCP", "1")
	if computerServer() == nil {
		t.Skip("xdotool not installed")
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("model = \"gpt-6.1-sol\"\n"), 0o600)
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"mine":{"command":"x"}},"numStartups":3}`), 0o600)
	New(Options{Home: home, Unprivileged: true})
	New(Options{Home: home, Unprivileged: true}) // idempotent
	var claude map[string]any
	b, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	json.Unmarshal(b, &claude)
	servers := claude["mcpServers"].(map[string]any)
	if servers["mine"] == nil || servers["computer"] == nil || claude["numStartups"].(float64) != 3 {
		t.Fatal(string(b))
	}
	toml, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if strings.Count(string(toml), "[mcp_servers.computer]") != 1 || !strings.HasPrefix(string(toml), "model = ") {
		t.Fatal(string(toml))
	}
	for _, f := range []string{".pi/agent/mcp.json", ".kimi-code/mcp.json", ".prime/agent/settings.json", ".config/opencode/opencode.json"} {
		b, err := os.ReadFile(filepath.Join(home, f))
		if err != nil || !strings.Contains(string(b), `"computer"`) {
			t.Fatalf("%s: %v %s", f, err, b)
		}
	}

	// the server itself, over pipes
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() { _ = ServeMCPComputer(inR, outW); outW.Close() }()
	sc := bufio.NewScanner(outR)
	sc.Buffer(nil, 4<<20)
	call := func(id int, method string, params any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		inW.Write(append(b, '\n'))
		if !sc.Scan() {
			t.Fatal("no response")
		}
		var m map[string]any
		json.Unmarshal(sc.Bytes(), &m)
		return m
	}
	init := call(1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if init["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "computer" {
		t.Fatal(init)
	}
	inW.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	tools := call(2, "tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, n := range []string{"get_desktop_state", "click", "type_text", "press_key", "scroll", "list_windows", "clipboard_write"} {
		if !names[n] {
			t.Errorf("missing tool %s", n)
		}
	}
	res := call(3, "tools/call", map[string]any{"name": "scroll", "arguments": map[string]any{"direction": "sideways"}})["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatal(res)
	}
	inW.Close()
}

func TestZeroDataRetention(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, dir)
	e.m.o.ZeroDataRetention = func() bool { return true }
	p, _ := e.prompt(map[string]any{"provider": "fake", "prompt": "secret-prompt-text"})
	e.waitStatus(p, "finished")
	if texts := lastText(t, e, p); len(texts) != 1 { // served from memory
		t.Fatal(texts)
	}
	for _, f := range []string{"events.jsonl", "state.json"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if strings.Contains(string(b), "secret-prompt-text") {
			t.Fatalf("%s keeps prompt text: %s", f, b)
		}
	}
}

func TestEventAttachments(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "attachments"), 0o755)
	os.WriteFile(filepath.Join(home, "attachments", "shot.png"), []byte("\x89PNG\r\n\x1a\nxx"), 0o644)
	big := make([]byte, maxEventAttachInline+1)
	os.WriteFile(filepath.Join(home, "attachments", "big.bin"), big, 0o644)
	got := eventAttachments([]string{"~/attachments/shot.png", filepath.Join(home, "attachments", "big.bin")}, home)
	if got[0]["filename"] != "shot.png" || got[0]["mimeType"] != "image/png" || got[0]["base64Data"] != "iVBORw0KGgp4eA==" {
		t.Fatalf("small: %v", got[0])
	}
	if _, ok := got[1]["base64Data"]; ok || got[1]["filename"] != "big.bin" || got[1]["mimeType"] == "" {
		t.Fatalf("big: %v", got[1])
	}
}

// crowdvm prompt --local-login: a request credential wins for its turn over
// the sandbox's AI proxy, Bedrock and other Anthropic auth.
func TestRequestCredentialsWin(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_API_KEY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	m := New(Options{Home: t.TempDir(), Unprivileged: true, AIProxy: &proto.AIProxy{BaseURL: "http://10.0.2.2:8420/ai-proxy", Token: "tok"},
		Env: func() map[string]string {
			return map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_BEARER_TOKEN_BEDROCK": "b"}
		}})
	env := m.envMap(map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oat"})
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK"} {
		if _, ok := env[k]; ok {
			t.Fatalf("%s kept next to a request sign-in", k)
		}
	}
	spec := &turnSpec{Model: "claude-haiku-5-5", Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oat"}}
	if _, args, _ := claudeCommand(m, spec); !strings.Contains(strings.Join(args, " "), "--model claude-haiku-5-5") {
		t.Fatalf("model rewritten for bedrock: %v", args)
	}
	if env["OPENAI_BASE_URL"] == "" || env["OPENAI_API_KEY"] != "tok" {
		t.Fatal("other vendors' proxy routes must stay", env)
	}
	// A request API key goes straight to its vendor, not to the AI proxy.
	if env := m.envMap(map[string]string{"OPENAI_API_KEY": "sk"}); env["OPENAI_BASE_URL"] != "" || env["ANTHROPIC_BASE_URL"] == "" {
		t.Fatal("request key still routed through the proxy", env)
	}
	// Codex + ChatGPT sign-in: the direct route, nothing for the tools to inherit.
	spec = &turnSpec{Model: "gpt-6-luna", Env: map[string]string{"CHATGPT_ACCESS_TOKEN": "at", "CHATGPT_ACCOUNT_ID": "acct"}}
	if env := m.envMap(spec.Env); env["OPENAI_API_KEY"] != "" || env["OPENAI_BASE_URL"] != "" {
		t.Fatal("proxy OpenAI route kept", env)
	}
	_, args, _ := codexCommand(m, spec)
	a := strings.Join(args, " ")
	for _, want := range []string{`model_provider="chatgpt"`, `base_url="https://chatgpt.com/backend-api/codex"`, `env_key="CHATGPT_ACCESS_TOKEN"`,
		`env_http_headers={"ChatGPT-Account-Id"="CHATGPT_ACCOUNT_ID"}`, "features.shell_snapshot=false", `"CHATGPT_*"`} {
		if !strings.Contains(a, want) {
			t.Fatalf("codex args lack %s: %v", want, args)
		}
	}
	if ok, _ := m.providerConfigured("codex", "", map[string]string{"CHATGPT_ACCESS_TOKEN": "at"}); !ok {
		t.Fatal("codex preflight ignores CHATGPT_ACCESS_TOKEN")
	}
}
