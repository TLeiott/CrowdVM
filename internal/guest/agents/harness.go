package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// harness describes how to drive one coding-agent CLI.
type harness struct {
	name         string // normalized provider name (Boat's lastHarness values)
	bin          string
	sessions     bool // can resume a native session
	pickSession  bool // we choose the id of a new session (Claude --session-id, pi --session-id)
	nativeSteer  bool // steers join the running turn (stdin line or driver.steer)
	noteInPrompt bool // no system-prompt option: the platform note goes into the first prompt
	command      func(m *Manager, t *turnSpec) (bin string, args []string, stdin []byte)
	parse        func(r *run, line []byte) // stdout parser (non-driver harnesses)
	driver       func() driver             // bidirectional protocol (replaces parse)
	steerLine    func(msg string) []byte
	env          func(m *Manager, t *turnSpec, env map[string]string)
}

var aliases = map[string]string{
	"claude-code": "claude", "prime": "prime-agent", "kimi-code": "kimi",
	"vibe": "mistral", "mistral-vibe": "mistral",
}

var harnesses = map[string]*harness{
	"claude": {name: "claude", bin: "claude", sessions: true, pickSession: true, nativeSteer: true,
		command: claudeCommand, parse: parseClaude, steerLine: claudeUserLine, env: claudeEnv},
	"codex": {name: "codex", bin: "codex", sessions: true, nativeSteer: true, command: codexCommand,
		driver: func() driver { return &codexDriver{} }},
	"opencode": {name: "opencode", bin: "opencode", sessions: true, noteInPrompt: true, command: opencodeCommand,
		parse: parseOpencode, env: opencodeEnv},
	"pi": {name: "pi", bin: "pi", sessions: true, pickSession: true, nativeSteer: true, command: piCommand,
		driver: func() driver { return &piDriver{} }},
	"prime-agent": {name: "prime-agent", bin: "prime-agent", sessions: true, nativeSteer: true, command: primeCommand,
		driver: func() driver { return &piDriver{} },
		env:    func(_ *Manager, _ *turnSpec, env map[string]string) { env["PRIME_AGENT_TELEMETRY"] = "0" }},
	"kimi": {name: "kimi", bin: "kimi", sessions: true, noteInPrompt: true,
		command: func(*Manager, *turnSpec) (string, []string, []byte) { return "kimi", []string{"acp"}, nil },
		driver:  func() driver { return &acpDriver{} },
		env:     func(_ *Manager, _ *turnSpec, env map[string]string) { env["KIMI_DISABLE_TELEMETRY"] = "1" }},
	"mistral": {name: "mistral", bin: "vibe-acp", sessions: true, noteInPrompt: true,
		command: func(*Manager, *turnSpec) (string, []string, []byte) { return "vibe-acp", nil, nil },
		driver:  func() driver { return &acpDriver{passMCP: true} }},
	// fake-fallback: the fake harness without a mid-turn channel (tests the
	// interrupt-and-continue steer path).
	"fake-fallback": {name: "fake-fallback", sessions: true, pickSession: true,
		command: fakeCommand, parse: parseClaude,
		env: func(_ *Manager, _ *turnSpec, env map[string]string) { env[fakeChildEnv] = "1" }},
	"fake-codex": fakeProto("codex", harness{nativeSteer: true, driver: func() driver { return &codexDriver{} }}),
	"fake-acp":   fakeProto("acp", harness{noteInPrompt: true, driver: func() driver { return &acpDriver{passMCP: true} }}),
	"fake": {name: "fake", sessions: true, pickSession: true, nativeSteer: true,
		command: fakeCommand, parse: parseClaude, steerLine: claudeUserLine,
		env: func(_ *Manager, _ *turnSpec, env map[string]string) { env[fakeChildEnv] = "1" }},
}

// normalizeProvider maps Boat provider names/aliases to a harness name.
func normalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if a, ok := aliases[p]; ok {
		return a
	}
	return p
}

const systemNote = "You are running headless inside a CrowdVM sandbox: a persistent Ubuntu 24.04 VM where you are " +
	"the user `user` with passwordless sudo. Home is /home/user; files the user attached are in ~/attachments. " +
	"A Linux desktop with Chrome runs on DISPLAY=:0; use the `computer` MCP tools to see and drive it. " +
	"The `crowdvm` CLI (also `host` and `boat`) works from here: `host <port>` prints a URL for a server you " +
	"start, `crowdvm info self` describes this sandbox; the `crowdvm` skill has the details. Only the disk " +
	"survives a stop: run long-lived programs as systemd services. " +
	"Nobody can answer questions during a turn, so make reasonable assumptions and finish the task."

// ---------------------------------------------------------------------------
// Claude Code: claude -p --input-format stream-json --output-format stream-json
// ---------------------------------------------------------------------------

func claudeArgs(t *turnSpec) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--replay-user-messages", "--include-partial-messages"}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.Effort != "" && t.Effort != "none" {
		args = append(args, "--effort", t.Effort)
	}
	if t.Session != "" {
		args = append(args, "--resume", t.Session)
	} else if t.NewSession != "" {
		args = append(args, "--session-id", t.NewSession)
	}
	return args
}

func claudeCommand(m *Manager, t *turnSpec) (string, []string, []byte) {
	tt := *t
	env := m.envMap(t.Env)
	switch claudeRoute(t.Model, env, m.o.Home) {
	case "bedrock":
		if t.Model != "" || env["ANTHROPIC_MODEL"] == "" { // the server may set the default (ANTHROPIC_MODEL)
			tt.Model = bedrockClaudeModel(t.Model)
		}
	case "deepseek":
		if tt.Model == "" {
			tt.Model = deepseekDefault
		}
	}
	// MCP resource tools off (Boat): they list/read resources of every server.
	args := append(claudeArgs(&tt), "--dangerously-skip-permissions", "--append-system-prompt", systemNote,
		"--disallowedTools", "ListMcpResourcesTool,ReadMcpResourceTool")
	if t.Fast != nil {
		// "fastMode" is Claude Code's settings key behind /fast (flag settings win).
		args = append(args, "--settings", string(mustJSON(map[string]bool{"fastMode": *t.Fast})))
	}
	if c := computerServer(); c != nil {
		args = append(args, "--mcp-config", string(mustJSON(map[string]any{
			"mcpServers": map[string]any{"computer": map[string]any{"command": c.Command, "args": c.Args}}})))
	}
	return "claude", args, claudeUserLine(t.Prompt)
}

const deepseekDefault = "deepseek-v4-pro"

// claudeRoute picks the backend Claude Code talks to: "deepseek" (DeepSeek's
// Anthropic-compatible API), "bedrock" (CLAUDE_CODE_USE_BEDROCK) or "" (Anthropic).
func claudeRoute(model string, env map[string]string, home string) string {
	if env["DEEPSEEK_API_KEY"] != "" && (strings.HasPrefix(model, "deepseek") || (model == "" && !hasAnthropicCreds(env, home) && env["CLAUDE_CODE_USE_BEDROCK"] == "")) {
		return "deepseek"
	}
	if env["CLAUDE_CODE_USE_BEDROCK"] == "1" {
		return "bedrock"
	}
	return ""
}

func hasAnthropicCreds(env map[string]string, home string) bool {
	return env["ANTHROPIC_API_KEY"] != "" || env["ANTHROPIC_AUTH_TOKEN"] != "" || env["CLAUDE_CODE_OAUTH_TOKEN"] != "" ||
		fileExists(filepath.Join(home, ".claude", ".credentials.json"))
}

// bedrockClaudeModel maps catalog ids to Claude Code's Bedrock aliases (Claude
// Code resolves them to the region's inference profiles). Default: Sonnet.
func bedrockClaudeModel(m string) string {
	switch {
	case m == "":
		return "sonnet"
	case strings.HasPrefix(m, "claude-opus"):
		return "opus"
	case strings.HasPrefix(m, "claude-sonnet"):
		return "sonnet"
	case strings.HasPrefix(m, "claude-haiku"):
		return "haiku"
	}
	return m // a Bedrock model id or inference profile
}

// claudeEnv points Claude Code at DeepSeek (ANTHROPIC_BASE_URL + AUTH_TOKEN,
// empty API key) or keeps Bedrock exclusive.
func claudeEnv(m *Manager, t *turnSpec, env map[string]string) {
	switch claudeRoute(t.Model, env, m.o.Home) {
	case "deepseek":
		base := strings.TrimRight(firstNonEmpty(env["DEEPSEEK_BASE_URL"], "https://api.deepseek.com"), "/")
		model := firstNonEmpty(t.Model, deepseekDefault)
		env["ANTHROPIC_BASE_URL"] = base + "/anthropic"
		env["ANTHROPIC_AUTH_TOKEN"] = env["DEEPSEEK_API_KEY"]
		env["ANTHROPIC_API_KEY"] = ""
		env["ANTHROPIC_MODEL"], env["ANTHROPIC_SMALL_FAST_MODEL"], env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = model, model, model
		delete(env, "CLAUDE_CODE_OAUTH_TOKEN")
		delete(env, "CLAUDE_CODE_USE_BEDROCK")
	case "bedrock":
		delete(env, "ANTHROPIC_BASE_URL") // the AI proxy URL is for the Anthropic API only
	}
}

func claudeUserLine(msg string) []byte {
	return jsonLine(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": msg}})
}

func parseClaude(r *run, line []byte) {
	var ev struct {
		Type      string  `json:"type"`
		Subtype   string  `json:"subtype"`
		SessionID string  `json:"session_id"`
		Model     string  `json:"model"`
		IsReplay  bool    `json:"isReplay"`
		Parent    *string `json:"parent_tool_use_id"`
		IsError   bool    `json:"is_error"`
		Result    string  `json:"result"`
		Queued    int     `json:"queued_turn_count"`
		Errors    []any   `json:"errors"`
		Message   struct {
			Model   string          `json:"model"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "stream_event":
		if ev.Parent == nil && ev.Event.Type == "content_block_delta" && ev.Event.Delta.Type == "text_delta" {
			r.partial(ev.Event.Delta.Text)
		}
	case "system":
		if ev.Subtype == "init" {
			r.setSession(ev.SessionID)
			r.setModel(ev.Model)
		}
	case "assistant":
		if ev.Parent != nil { // subagent traffic
			return
		}
		r.setModel(ev.Message.Model)
		for _, b := range blocks(ev.Message.Content) {
			switch b.Type {
			case "text":
				r.text(b.Text)
			case "tool_use":
				r.toolUse(b.ID, b.Name, b.Input)
			}
		}
	case "user":
		if ev.IsReplay {
			r.ackSteer(contentText(ev.Message.Content))
			return
		}
		if ev.Parent != nil {
			return
		}
		for _, b := range blocks(ev.Message.Content) {
			if b.Type == "tool_result" {
				r.toolResult(b.ToolUseID, b.IsError, contentText(b.Content))
			}
		}
	case "result":
		msg := ev.Result
		if ev.IsError && msg == "" {
			msg = ev.Subtype
			if len(ev.Errors) > 0 {
				msg += ": " + string(mustJSON(ev.Errors))
			}
		}
		r.result(ev.IsError, msg, ev.Queued)
	}
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     any             `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

func blocks(raw json.RawMessage) []block {
	var bs []block
	_ = json.Unmarshal(raw, &bs)
	return bs
}

// contentText flattens a string-or-blocks content value.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []string
	for _, b := range blocks(raw) {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		default:
			if len(b.Content) > 0 {
				parts = append(parts, contentText(b.Content))
			}
		}
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------------------
// Codex: codex app-server (see codexDriver). Config overrides go on the
// command line; thread and turn settings through the protocol.
// ---------------------------------------------------------------------------

func codexCommand(m *Manager, t *turnSpec) (string, []string, []byte) {
	args := []string{"app-server"}
	env := m.envMap(t.Env)
	switch codexRoute(t.Model, env, m.o.Home) {
	case "chatgpt":
		// A ChatGPT sign-in passed with the request (crowdvm prompt --local-login):
		// Codex has no env var for it, so a provider sends the access token as
		// the bearer to the ChatGPT Codex backend (as the AI proxy does).
		args = append(args, "-c", `model_provider="chatgpt"`,
			"-c", `model_providers.chatgpt={name="ChatGPT",base_url="https://chatgpt.com/backend-api/codex",env_key="CHATGPT_ACCESS_TOKEN",wire_api="responses",env_http_headers={"ChatGPT-Account-Id"="CHATGPT_ACCOUNT_ID"}}`)
	case "deepseek":
		base := strings.TrimRight(firstNonEmpty(env["DEEPSEEK_BASE_URL"], "https://api.deepseek.com"), "/")
		args = append(args, "-c", `model_provider="deepseek"`,
			"-c", "model_providers.deepseek={name=\"DeepSeek\",base_url="+tomlStr(base)+",env_key=\"DEEPSEEK_API_KEY\",wire_api=\"responses\"}",
			"-c", `web_search="disabled"`)
		if t.Model == "" {
			t.Model = deepseekDefault
		}
	case "bedrock":
		// Codex's built-in Amazon Bedrock provider (Mantle): AWS_REGION plus
		// AWS_BEARER_TOKEN_BEDROCK or IAM access keys.
		args = append(args, "-c", `model_provider="amazon-bedrock"`)
		if !strings.Contains(t.Model, ".") || strings.HasPrefix(t.Model, "gpt-") {
			t.Model = "openai." + firstNonEmpty(t.Model, "gpt-6.1-sol")
		}
	default:
		// Codex ignores OPENAI_BASE_URL; route it through a custom provider (AI proxy, ...).
		if base := env["OPENAI_BASE_URL"]; base != "" {
			args = append(args, "-c", `model_provider="crowdvm"`,
				"-c", "model_providers.crowdvm={name=\"crowdvm\",base_url="+tomlStr(base)+",env_key=\"OPENAI_API_KEY\",wire_api=\"responses\"}")
		}
	}
	// Codex's shell snapshot dumps the whole environment (sandbox keys,
	// CROWDVM_API_KEY), which the guest's process tracer would record: off
	// for every turn.
	args = append(args, "-c", "features.shell_snapshot=false")
	if t.Env["CHATGPT_ACCESS_TOKEN"] != "" || t.Env["OPENAI_API_KEY"] != "" {
		// Request credentials stay in the harness process; its tool
		// commands do not inherit them.
		args = append(args, "-c", `shell_environment_policy.exclude=["CHATGPT_*","OPENAI_API_KEY","CODEX_API_KEY"]`)
	}
	if c := computerServer(); c != nil {
		args = append(args, "-c", "mcp_servers.computer={command="+tomlStr(c.Command)+",args="+tomlArr(c.Args)+"}")
	}
	return "codex", args, nil
}

// codexRoute: "chatgpt" (a ChatGPT access token in the env), "deepseek",
// "bedrock" (only without an OpenAI credential) or "".
func codexRoute(model string, env map[string]string, home string) string {
	if env["CHATGPT_ACCESS_TOKEN"] != "" && !strings.HasPrefix(model, "deepseek") {
		return "chatgpt"
	}
	openai := env["OPENAI_API_KEY"] != "" || env["CODEX_API_KEY"] != "" || fileExists(filepath.Join(home, ".codex", "auth.json"))
	if env["DEEPSEEK_API_KEY"] != "" && (strings.HasPrefix(model, "deepseek") || strings.Contains(env["OPENAI_BASE_URL"], "deepseek") ||
		(model == "" && !openai && !hasBedrock(env))) {
		return "deepseek"
	}
	if hasBedrock(env) && !openai {
		return "bedrock"
	}
	return ""
}

// hasBedrock: a Bedrock API key, or IAM access keys with a region.
func hasBedrock(env map[string]string) bool {
	return env["AWS_BEARER_TOKEN_BEDROCK"] != "" ||
		(env["AWS_ACCESS_KEY_ID"] != "" && env["AWS_SECRET_ACCESS_KEY"] != "" && env["AWS_REGION"] != "")
}

func tomlStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func tomlArr(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = tomlStr(s)
	}
	return "[" + strings.Join(q, ",") + "]"
}

// ---------------------------------------------------------------------------
// OpenCode: opencode run --format json [-s session] -- prompt
// ---------------------------------------------------------------------------

// opencodeModel maps catalog ids to OpenCode's provider/model ids:
// "openrouter:x/y" → "openrouter/x/y", "opencode-go:m" → "opencode-go/m".
func opencodeModel(m string) string {
	if v, rest, ok := strings.Cut(m, ":"); ok && !strings.Contains(v, "/") {
		return v + "/" + rest
	}
	if m == "" || strings.Contains(m, "/") {
		return m
	}
	switch {
	case strings.HasPrefix(m, "claude"):
		return "anthropic/" + m
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "openai/" + m
	case strings.HasPrefix(m, "mistral"), strings.HasPrefix(m, "devstral"), strings.HasPrefix(m, "codestral"):
		return "mistral/" + m
	case strings.HasPrefix(m, "deepseek"):
		return "deepseek/" + m
	}
	return "opencode/" + m
}

func opencodeCommand(m *Manager, t *turnSpec) (string, []string, []byte) {
	args := []string{"run", "--format", "json", "--auto"}
	if t.Model != "" {
		args = append(args, "-m", opencodeModel(t.Model))
	}
	if t.Effort != "" && t.Effort != "none" {
		args = append(args, "--variant", t.Effort)
	}
	if t.Session != "" {
		args = append(args, "-s", t.Session)
	}
	for _, a := range t.Attachments {
		args = append(args, "-f", a)
	}
	return "opencode", append(args, "--", t.Prompt), nil
}

// opencodeEnv injects the computer MCP server and AI-proxy base URLs through
// OPENCODE_CONFIG_CONTENT (merged over the user's opencode.json).
func opencodeEnv(m *Manager, _ *turnSpec, env map[string]string) {
	if env["OPENCODE_CONFIG_CONTENT"] != "" {
		return
	}
	cfg := map[string]any{}
	if c := computerServer(); c != nil {
		cfg["mcp"] = map[string]any{"computer": map[string]any{"type": "local", "command": append([]string{c.Command}, c.Args...)}}
		// OpenCode forwards MCP schemas verbatim; Anthropic rejects cua-driver's browser_prepare.
		cfg["tools"] = map[string]any{"computer_browser_prepare": false}
	}
	if m.aiProxy() != nil {
		prov := map[string]any{}
		for id, k := range map[string]string{"anthropic": "ANTHROPIC", "openai": "OPENAI", "openrouter": "OPENROUTER", "mistral": "MISTRAL", "xai": "XAI", "opencode": "OPENCODE", "deepseek": "DEEPSEEK"} {
			if base := env[k+"_BASE_URL"]; base != "" {
				if id == "anthropic" || id == "deepseek" {
					base += "/v1"
				}
				prov[id] = map[string]any{"options": map[string]any{"baseURL": base, "apiKey": env[k+"_API_KEY"]}}
			}
		}
		cfg["provider"] = prov
	}
	if len(cfg) > 0 {
		env["OPENCODE_CONFIG_CONTENT"] = string(mustJSON(cfg))
	}
}

func parseOpencode(r *run, line []byte) {
	var ev struct {
		Type      string         `json:"type"`
		SessionID string         `json:"sessionID"`
		Part      map[string]any `json:"part"`
		Error     map[string]any `json:"error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	r.setSession(ev.SessionID)
	p := ev.Part
	switch ev.Type {
	case "text":
		r.text(str(p["text"]))
	case "tool_use":
		st, _ := p["state"].(map[string]any)
		out := str(st["output"])
		if st["status"] == "error" {
			out = str(st["error"])
		}
		r.tool(str(p["callID"]), str(p["tool"]), st["input"], st["status"] == "error", out)
	case "error":
		msg := str(ev.Error["name"])
		if d, ok := ev.Error["data"].(map[string]any); ok && str(d["message"]) != "" {
			msg = str(d["message"])
		}
		r.fail(msg)
	}
}

// ---------------------------------------------------------------------------
// pi / Prime Agent: `<bin> --mode rpc` (see piDriver)
// ---------------------------------------------------------------------------

// piThinking maps reasoningEffort to pi's --thinking levels.
func piThinking(e string) string {
	if e == "none" {
		return "off"
	}
	return e
}

func piCommand(_ *Manager, t *turnSpec) (string, []string, []byte) {
	args := []string{"--mode", "rpc", "--append-system-prompt", systemNote}
	if t.Model != "" {
		args = append(args, "--model", opencodeModel(t.Model)) // pi takes "provider/id" (or a fuzzy pattern)
	}
	if t.Effort != "" {
		args = append(args, "--thinking", piThinking(t.Effort))
	}
	// --session-id opens the session with that id in the cwd, creating it otherwise.
	if id := firstNonEmpty(t.Session, t.NewSession); id != "" {
		args = append(args, "--session-id", id)
	}
	return "pi", args, nil
}

func primeCommand(_ *Manager, t *turnSpec) (string, []string, []byte) {
	args := []string{"--mode", "rpc", "--append-system-prompt", systemNote}
	if t.Model != "" {
		if p, id, ok := strings.Cut(opencodeModel(t.Model), "/"); ok && !strings.HasPrefix(t.Model, "opencode") {
			args = append(args, "--provider", p, "--model", id)
		} else {
			args = append(args, "--model", t.Model)
		}
	}
	if t.Effort != "" {
		args = append(args, "--thinking", piThinking(t.Effort))
	}
	if t.Session != "" {
		args = append(args, "--resume", t.Session)
	}
	return "prime-agent", args, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func isImage(p string) bool {
	p = strings.ToLower(p)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}
