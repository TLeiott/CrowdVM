package cli

// crowdvm prompt --local-login: send the caller's own login (Claude or
// ChatGPT subscription, or a vendor API key) with one prompt. Read fresh at
// every call and never cached. Only access tokens go out, never a refresh
// or id token, so this machine stays the only one that refreshes the login.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/client"
)

// localLogin is one credential read from this machine.
type localLogin struct {
	Provider  string
	Kind      string // subscription | api-key
	Source    string // env var or file it came from
	ExpiresAt time.Time
	Creds     map[string]string // env vars for the prompt's credentials map
}

// localLoginMinTTL: a subscription token that expires sooner is refused (it
// would die mid-turn and the sandbox cannot refresh it).
const localLoginMinTTL = 2 * time.Minute

// localLoginKeys: vendor API keys opencode and pi can run on.
var localLoginKeys = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "LLMGATEWAY_API_KEY", "DEEPSEEK_API_KEY",
	"MOONSHOT_API_KEY", "MISTRAL_API_KEY", "XAI_API_KEY", "OPENCODE_API_KEY", "GEMINI_API_KEY"}

// parseLocalLoginMode maps flag/env values to "" (off), auto, subscription or api-key.
func parseLocalLoginMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "off", "no":
		return "", nil
	case "1", "true", "on", "yes", "auto":
		return "auto", nil
	case "subscription":
		return "subscription", nil
	case "api-key", "apikey", "key":
		return "api-key", nil
	}
	return "", usagef("--local-login must be auto, subscription or api-key (got %q)", v)
}

func localProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "claude-code" {
		return "claude"
	}
	return p
}

// readLocalLogin finds the credential for provider (mode auto: subscription
// first, then an API key).
func readLocalLogin(provider, mode string, getenv func(string) string, now time.Time) (*localLogin, error) {
	home := getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	sub, key := mode != "api-key", mode != "subscription"
	var tried []string
	var found *localLogin
	try := func(f func() *localLogin, what string) {
		if found == nil {
			tried = append(tried, what)
			found = f()
		}
	}
	env := func(name, kind string) func() *localLogin {
		return func() *localLogin {
			if v := strings.TrimSpace(getenv(name)); v != "" {
				return &localLogin{Kind: kind, Source: "$" + name, Creds: map[string]string{name: v}}
			}
			return nil
		}
	}
	switch provider {
	case "claude":
		file := filepath.Join(firstNonEmpty(getenv("CLAUDE_CONFIG_DIR"), filepath.Join(home, ".claude")), ".credentials.json")
		if sub {
			try(env("CLAUDE_CODE_OAUTH_TOKEN", "subscription"), "$CLAUDE_CODE_OAUTH_TOKEN")
			try(func() *localLogin { return claudeFileLogin(file) }, file)
		}
		if key {
			try(env("ANTHROPIC_API_KEY", "api-key"), "$ANTHROPIC_API_KEY")
		}
	case "codex":
		file := filepath.Join(firstNonEmpty(getenv("CODEX_HOME"), filepath.Join(home, ".codex")), "auth.json")
		if sub {
			try(func() *localLogin { return codexFileLogin(file, false) }, file)
		}
		if key {
			try(func() *localLogin { return codexFileLogin(file, true) }, file+" (OPENAI_API_KEY)")
			try(env("OPENAI_API_KEY", "api-key"), "$OPENAI_API_KEY")
		}
	case "opencode", "pi":
		if !key {
			tried = append(tried, provider+" has no subscription login to pass; use --local-login=api-key")
			break
		}
		tried = append(tried, "vendor API key env vars ("+strings.Join(localLoginKeys, ", ")+")")
		creds, names := map[string]string{}, []string{}
		for _, k := range localLoginKeys {
			if v := strings.TrimSpace(getenv(k)); v != "" {
				creds[k] = v
				names = append(names, "$"+k)
			}
		}
		if len(creds) > 0 {
			found = &localLogin{Kind: "api-key", Source: strings.Join(names, ", "), Creds: creds}
		}
	default:
		return nil, usagef("--local-login supports --provider claude, codex, opencode and pi (got %q)", provider)
	}
	if found == nil {
		return nil, &client.Error{Code: "local_login_not_found", Message: fmt.Sprintf("no local %s login (%s) on this machine; looked at: %s",
			provider, firstNonEmpty(map[string]string{"auto": "subscription or API key"}[mode], mode), strings.Join(tried, "; ")),
			Details: map[string]any{"provider": provider, "mode": mode}}
	}
	found.Provider = provider
	if !found.ExpiresAt.IsZero() && found.ExpiresAt.Sub(now) < localLoginMinTTL {
		left := found.ExpiresAt.Sub(now).Round(time.Second)
		when := "expires in " + left.String()
		if left <= 0 {
			when = "expired " + (-left).String() + " ago"
		}
		return nil, &client.Error{Code: "local_login_expired", Message: fmt.Sprintf("the local %s login in %s %s; run `%s` once on this machine to refresh its login",
			provider, found.Source, when, provider), Details: map[string]any{"provider": provider, "source": found.Source, "expiresAt": found.ExpiresAt.UTC().Format(time.RFC3339)}}
	}
	return found, nil
}

// claudeFileLogin: claudeAiOauth.accessToken (+ expiresAt, ms) of Claude Code's credentials file.
func claudeFileLogin(path string) *localLogin {
	var f struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if b, err := os.ReadFile(path); err != nil || json.Unmarshal(b, &f) != nil || f.OAuth.AccessToken == "" {
		return nil
	}
	ll := &localLogin{Kind: "subscription", Source: path, Creds: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": f.OAuth.AccessToken}}
	if f.OAuth.ExpiresAt > 0 {
		ll.ExpiresAt = time.UnixMilli(f.OAuth.ExpiresAt)
	}
	return ll
}

// codexFileLogin: the ChatGPT access token + account id of Codex's auth.json
// (expiry from the JWT's exp), or with apiKey its OPENAI_API_KEY.
func codexFileLogin(path string, apiKey bool) *localLogin {
	var f struct {
		APIKey *string `json:"OPENAI_API_KEY"`
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if b, err := os.ReadFile(path); err != nil || json.Unmarshal(b, &f) != nil {
		return nil
	}
	if apiKey {
		if f.APIKey == nil || strings.TrimSpace(*f.APIKey) == "" {
			return nil
		}
		return &localLogin{Kind: "api-key", Source: path, Creds: map[string]string{"OPENAI_API_KEY": strings.TrimSpace(*f.APIKey)}}
	}
	if f.Tokens.AccessToken == "" {
		return nil
	}
	ll := &localLogin{Kind: "subscription", Source: path, ExpiresAt: jwtExpiry(f.Tokens.AccessToken),
		Creds: map[string]string{"CHATGPT_ACCESS_TOKEN": f.Tokens.AccessToken}}
	if f.Tokens.AccountID != "" {
		ll.Creds["CHATGPT_ACCOUNT_ID"] = f.Tokens.AccountID
	}
	return ll
}

// jwtExpiry reads a JWT's exp claim without verifying it (zero if absent).
func jwtExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	var c struct {
		Exp float64 `json:"exp"`
	}
	if err != nil || json.Unmarshal(b, &c) != nil || c.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(c.Exp), 0)
}

// info is what the queued event says about the login (never the token).
func (l *localLogin) info(now time.Time) map[string]any {
	m := map[string]any{"provider": l.Provider, "kind": l.Kind, "source": l.Source}
	if !l.ExpiresAt.IsZero() {
		m["expiresAt"] = l.ExpiresAt.UTC().Format(time.RFC3339)
		m["expiresInSeconds"] = int64(l.ExpiresAt.Sub(now).Seconds())
	}
	return m
}

func (l *localLogin) String(now time.Time) string {
	s := l.Provider + " " + l.Kind + " from " + l.Source
	if !l.ExpiresAt.IsZero() {
		s += " (expires in " + l.ExpiresAt.Sub(now).Round(time.Minute).String() + ")"
	}
	return s
}
