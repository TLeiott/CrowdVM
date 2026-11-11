package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// providerConfigured is the /prompt pre-flight (409 provider_not_configured):
// is there any credential the harness can use? It is deliberately generous —
// env keys (sandbox env, AI proxy placeholders, request credentials) and the
// harness's own sign-in files all count — so it never blocks a working setup.
// want names what would satisfy it.
func (m *Manager) providerConfigured(provider, model string, req map[string]string) (bool, string) {
	env := m.envMap(req)
	home := m.o.Home
	has := func(keys ...string) bool {
		for _, k := range keys {
			if strings.TrimSpace(env[k]) != "" {
				return true
			}
		}
		return false
	}
	exists := func(rel ...string) bool {
		for _, r := range rel {
			if _, err := os.Stat(filepath.Join(home, r)); err == nil {
				return true
			}
		}
		return false
	}
	// keys every multi-vendor harness can use
	vendorKeys := []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "LLMGATEWAY_API_KEY", "DEEPSEEK_API_KEY",
		"MOONSHOT_API_KEY", "MISTRAL_API_KEY", "XAI_API_KEY", "OPENCODE_API_KEY", "GEMINI_API_KEY", "AWS_BEARER_TOKEN_BEDROCK"}
	switch provider {
	case "claude":
		return has("ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "DEEPSEEK_API_KEY", "AWS_BEARER_TOKEN_BEDROCK",
				"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "ANTHROPIC_BASE_URL") ||
				exists(".claude/.credentials.json") || claudeSettingsAuth(home),
			"an Anthropic API key, a Claude sign-in, a DeepSeek key or Bedrock"
	case "codex":
		return has("OPENAI_API_KEY", "CODEX_API_KEY", "CHATGPT_ACCESS_TOKEN", "DEEPSEEK_API_KEY", "AWS_BEARER_TOKEN_BEDROCK", "OPENAI_BASE_URL") ||
				hasBedrock(env) || exists(".codex/auth.json"),
			"an OpenAI API key, a ChatGPT sign-in, a DeepSeek key or Bedrock"
	case "opencode":
		return true, "" // free OpenCode Zen models need no key
	case "pi":
		return has(vendorKeys...) || exists(".pi/agent/auth.json", ".pi/agent/models.json"), "any provider API key or a pi sign-in"
	case "prime-agent":
		return has(append(vendorKeys, "PRIME_API_KEY")...) || exists(".prime/agent/auth.json", ".prime/config.json"),
			"any provider API key or a Prime sign-in"
	case "kimi":
		return has("KIMI_CODE_ACCESS_TOKEN", "KIMI_API_KEY", "KIMI_MODEL_API_KEY", "MOONSHOT_API_KEY") ||
				nonEmptyDir(filepath.Join(home, ".kimi-code", "credentials")) || exists(".kimi-code/config.toml"),
			"a Kimi Code sign-in or a Moonshot API key"
	case "mistral":
		vibeHome := firstNonEmpty(env["VIBE_HOME"], filepath.Join(home, ".vibe"))
		cfg, _ := os.ReadFile(filepath.Join(vibeHome, "config.toml"))
		return has("MISTRAL_API_KEY") || fileExists(filepath.Join(vibeHome, ".env")) || fileExists(filepath.Join(vibeHome, "auth.json")) ||
				strings.Contains(string(cfg), "[[providers]]"), // a custom provider
			"a Mistral API key or a Mistral Vibe sign-in"
	}
	return true, ""
}

// claudeSettingsAuth: an apiKeyHelper or env block in ~/.claude/settings.json.
func claudeSettingsAuth(home string) bool {
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	return err == nil && (strings.Contains(string(b), "apiKeyHelper") || strings.Contains(string(b), "ANTHROPIC_"))
}

func nonEmptyDir(p string) bool {
	es, err := os.ReadDir(p)
	return err == nil && len(es) > 0
}

func defaultModel(provider string) string {
	for _, p := range Catalog() {
		if p.Provider == provider {
			return p.DefaultModel
		}
	}
	return ""
}

// Attachment limits (Boat): 12 files, 25 MB each, 64 MB in total.
const (
	maxAttachFiles = 12
	maxAttachFile  = 25 << 20
	maxAttachTotal = 64 << 20
)

func checkAttachments(paths []string, home string) error {
	if len(paths) > maxAttachFiles {
		return fmt.Errorf("at most %d attachments per prompt (got %d)", maxAttachFiles, len(paths))
	}
	var total int64
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(home, p)
		}
		st, err := os.Stat(p)
		if err != nil {
			continue // the harness reports missing files itself
		}
		if st.Size() > maxAttachFile {
			return fmt.Errorf("attachment %s is %d bytes; the limit is 25 MB per file", filepath.Base(p), st.Size())
		}
		total += st.Size()
	}
	if total > maxAttachTotal {
		return fmt.Errorf("attachments total %d bytes; the limit is 64 MB per prompt", total)
	}
	return nil
}

// friendlyError rewrites well-known harness failures into actionable messages.
func friendlyError(msg string, env map[string]string) string {
	l := strings.ToLower(msg)
	switch {
	case (hasBedrock(env) || env["CLAUDE_CODE_USE_BEDROCK"] == "1") && (strings.Contains(l, "accessdeniedexception") ||
		strings.Contains(l, "don't have access to the model") || strings.Contains(l, "do not have access to the model") ||
		strings.Contains(l, "model access") || strings.Contains(l, "not authorized to invoke")):
		region := firstNonEmpty(env["AWS_REGION"], env["AWS_DEFAULT_REGION"], "your region")
		return "Amazon Bedrock: model access not granted in " + region +
			". Enable the model under Bedrock → Model access in the AWS console, or pick another model. (" + msg + ")"
	case strings.Contains(l, "authentication required") || strings.Contains(l, "missing api key") ||
		strings.Contains(l, "invalid api key") || strings.Contains(l, "invalid x-api-key") ||
		strings.Contains(l, "no authenticated models") || strings.Contains(l, "not logged in") || strings.Contains(l, "please run /login"):
		return "provider_not_configured: " + msg
	}
	return msg
}
