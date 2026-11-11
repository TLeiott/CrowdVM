package agents

import "slices"

// ProviderModels is one harness in the GET /provider-models catalog.
type ProviderModels struct {
	Provider     string   `json:"provider"` // value for PromptRequest.provider
	Name         string   `json:"name"`
	Aliases      []string `json:"aliases,omitempty"`
	Binary       string   `json:"binary"`
	DefaultModel string   `json:"defaultModel,omitempty"`
	// Steer is "native" (message joins the running turn) or "fallback"
	// (cancel the turn, then continue the same session with the message).
	Steer  string  `json:"steer"`
	Models []Model `json:"models"`
}

// Model is one model of a harness.
type Model struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	ReasoningEfforts       []string `json:"reasoningEfforts"` // accepted reasoningEffort values; empty = no control
	DefaultReasoningEffort string   `json:"defaultReasoningEffort,omitempty"`
	FastMode               bool     `json:"fastMode"`
}

var (
	claudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}
	codexEfforts  = []string{"none", "low", "medium", "high", "xhigh"}
	none          = []string{}
)

var multiVendor = []Model{
	{ID: "claude-opus-5-5", Name: "Claude Opus 5.5", ReasoningEfforts: claudeEfforts, DefaultReasoningEffort: "high"},
	{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", ReasoningEfforts: claudeEfforts, DefaultReasoningEffort: "high"},
	{ID: "gpt-6.1-sol", Name: "GPT-6.1 Sol", ReasoningEfforts: codexEfforts, DefaultReasoningEffort: "medium"},
	{ID: "openrouter:deepseek/deepseek-v4-pro", Name: "DeepSeek V4 Pro (OpenRouter)", ReasoningEfforts: none},
	{ID: "mistral-large-4", Name: "Mistral Large 4", ReasoningEfforts: none},
}

var catalog = []ProviderModels{
	{Provider: "claude", Name: "Claude Code", Aliases: []string{"claude-code"}, Binary: "claude", Steer: "native",
		DefaultModel: "claude-opus-5-5", Models: []Model{
			{ID: "claude-opus-5-5", Name: "Claude Opus 5.5", ReasoningEfforts: claudeEfforts, DefaultReasoningEffort: "high", FastMode: true},
			{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", ReasoningEfforts: claudeEfforts, DefaultReasoningEffort: "high"},
			{ID: "claude-haiku-5-5", Name: "Claude Haiku 5.5", ReasoningEfforts: claudeEfforts},
			{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro (DeepSeek key)", ReasoningEfforts: none},
		}},
	// Codex fast = service tier "priority"; every model below advertises it (codex 0.167 model catalog).
	{Provider: "codex", Name: "OpenAI Codex", Binary: "codex", Steer: "native", DefaultModel: "gpt-6.1-sol", Models: []Model{
		{ID: "gpt-6.1-sol", Name: "GPT-6.1 Sol", ReasoningEfforts: codexEfforts, DefaultReasoningEffort: "medium", FastMode: true},
		{ID: "gpt-6-sol", Name: "GPT-6 Sol", ReasoningEfforts: codexEfforts, DefaultReasoningEffort: "medium", FastMode: true},
		{ID: "gpt-6-astra", Name: "GPT-6 Astra", ReasoningEfforts: codexEfforts, DefaultReasoningEffort: "medium", FastMode: true},
		{ID: "gpt-6-luna", Name: "GPT-6 Luna", ReasoningEfforts: codexEfforts, DefaultReasoningEffort: "low", FastMode: true},
		{ID: "gpt-5.6-terra", Name: "GPT-5.6 Terra", ReasoningEfforts: codexEfforts, DefaultReasoningEffort: "medium", FastMode: true},
	}},
	{Provider: "opencode", Name: "OpenCode", Binary: "opencode", Steer: "fallback", DefaultModel: "opencode/beam-free",
		Models: append([]Model{
			{ID: "opencode/beam-free", Name: "Beam (free)", ReasoningEfforts: none},
			{ID: "opencode/deepseek-v4.1-flash-free", Name: "DeepSeek V4.1 Flash (free)", ReasoningEfforts: none},
			{ID: "opencode/glm-5.3-flash-free", Name: "GLM 5.3 Flash (free)", ReasoningEfforts: none},
		}, multiVendor...)},
	{Provider: "pi", Name: "pi", Binary: "pi", Steer: "native", Models: multiVendor},
	{Provider: "prime-agent", Name: "Prime Agent", Aliases: []string{"prime"}, Binary: "prime-agent", Steer: "native", Models: multiVendor},
	{Provider: "kimi", Name: "Kimi Code", Aliases: []string{"kimi-code"}, Binary: "kimi", Steer: "fallback", DefaultModel: "kimi-k3", Models: []Model{
		{ID: "kimi-k3", Name: "Kimi K3", ReasoningEfforts: none},
		{ID: "kimi-for-coding", Name: "Kimi for Coding", ReasoningEfforts: none},
		{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", ReasoningEfforts: none},
	}},
	{Provider: "mistral", Name: "Mistral Vibe", Aliases: []string{"vibe", "mistral-vibe"}, Binary: "vibe-acp", Steer: "fallback", DefaultModel: "mistral-medium-3.5", Models: []Model{
		{ID: "mistral-large-4", Name: "Mistral Large 4", ReasoningEfforts: none},
		{ID: "mistral-medium-3.5", Name: "Mistral Medium 3.5", ReasoningEfforts: none},
		{ID: "mistral-small-4", Name: "Mistral Small 4", ReasoningEfforts: none},
	}},
}

// Catalog returns the provider/model catalog served at GET /provider-models.
// Claude Code (stream-json stdin), Codex (app-server turn/steer), pi and Prime
// Agent (RPC steer) steer natively; OpenCode, Kimi Code and Mistral Vibe use
// the fallback (their protocols only have prompt and cancel).
func Catalog() []ProviderModels {
	out := slices.Clone(catalog)
	if fakeEnabled() {
		out = append(out, ProviderModels{Provider: "fake", Name: "Fake harness (tests)", Binary: "crowdvm-guest", Steer: "native",
			Models: []Model{{ID: "fake-1", Name: "Fake", ReasoningEfforts: none, FastMode: true}}})
	}
	return out
}

func findModel(provider, id string) (Model, bool) {
	for _, p := range Catalog() {
		if p.Provider == provider {
			for _, m := range p.Models {
				if m.ID == id {
					return m, true
				}
			}
		}
	}
	return Model{}, false
}

func fastModels() []string {
	var out []string
	for _, p := range catalog {
		for _, m := range p.Models {
			if m.FastMode {
				out = append(out, p.Provider+"/"+m.ID)
			}
		}
	}
	return out
}

func providerNames() []string {
	var out []string
	for _, p := range Catalog() {
		out = append(out, p.Provider)
	}
	return out
}
