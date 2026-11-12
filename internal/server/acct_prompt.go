package server

// POST /sandboxes/{id}/prompt goes through here before the guest:
//
//   - Account defaults (AGT-004/006): a missing provider, model or fast flag
//     is filled from the caller's account settings (fast only for models
//     that have a fast mode; "fast": false always forces standard).
//   - Pre-flight (AGT-007): 409 provider_not_configured (details.provider,
//     details.setupUrl) when the sandbox has no credential the harness can
//     run on: no account credential passed in (directly or through the AI
//     proxy) and none in the sandbox's own env or the request's env or credentials.
//   - Queueing (AGT-021): a prompt to a starting sandbox waits until the
//     sandbox is usable (up to promptStartWait) instead of StartWait.
//
// Holding prompts while a blocking repo setup runs (AGT-020) is the guest's
// job: proto.Repo.SetupBlocking reaches it in the guest config.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/crowdvm/crowdvm/internal/api"
)

const promptStartWait = 10 * time.Minute

func init() {
	RegisterFeature("acct_prompt", func(s *Server) error {
		s.API("POST /sandboxes/{sandboxId}/prompt", "agent.prompt", s.handlePrompt)
		return nil
	})
}

// harnessEnvNames: env vars that carry a credential a harness can run on.
func harnessEnvNames(provider string) []string {
	var out []string
	for _, id := range aiHarnessCreds[provider] {
		if cr, ok := aiCredByID(id); ok && cr.EnvVar != "" {
			out = append(out, cr.EnvVar)
		}
	}
	switch provider {
	case "claude":
		out = append(out, "ANTHROPIC_AUTH_TOKEN")
	case "codex":
		out = append(out, "OPENAI_API_KEY", "CHATGPT_ACCESS_TOKEN") // a ChatGPT sign-in from crowdvm prompt --local-login
	}
	return out
}

// promptConfigured reports whether sb can run provider.
func (s *Server) promptConfigured(sb *Sandbox, provider string, reqEnv map[string]string) bool {
	if _, known := aiHarnessCreds[provider]; !known || provider == "opencode" {
		return true // opencode has free models; fake or unknown harnesses: the guest decides
	}
	for _, k := range harnessEnvNames(provider) {
		if sb.Env[k] != "" || reqEnv[k] != "" {
			return true
		}
	}
	if _, ok := sb.Extra["envManaged"]; !ok {
		return true // booted without the environment feature: cannot tell
	}
	m := envManagedOf(sb)
	for _, k := range harnessEnvNames(provider) {
		for _, have := range m.Keys {
			if have == k {
				return true
			}
		}
	}
	if provider == "codex" {
		for _, f := range m.Files {
			if f == ".codex/auth.json" {
				return true
			}
		}
	}
	if m.Proxied && m.ProfileID != "" {
		active := s.aiActive(m.ProfileID)
		for _, id := range aiHarnessCreds[provider] {
			if cr, _ := aiCredByID(id); cr.Vendor != "" {
				if _, ok := active[id]; ok {
					return true
				}
			}
		}
	}
	return false
}

func (s *Server) handlePrompt(c *Ctx) error {
	sb, err := c.Sandbox("agent.prompt", false)
	if err != nil {
		return err
	}
	if s.accessOf(c.P, sb) == AccessView {
		return Err(403, "sandbox_private", "This sandbox is private to its creator.")
	}
	body, err := io.ReadAll(io.LimitReader(c.R.Body, 16<<20))
	if err != nil {
		return Err(400, "invalid_json", "Could not read the request body.")
	}
	var req map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return Err(400, "invalid_json", "Request body must be valid JSON: "+err.Error())
		}
	}
	// Boat 400s: prompt is required (minLength 1); an explicit provider must
	// name a harness.
	if p, ok := req["prompt"].(string); !ok || strings.TrimSpace(p) == "" {
		return Err(400, "prompt_required", "prompt is required: the natural-language task for the agent.")
	}
	if v, set := req["provider"]; set && v != nil {
		if p, ok := v.(string); !ok || strings.TrimSpace(p) == "" {
			return ErrD(400, "provider_required", "provider must name a harness (codex, claude-code, pi, opencode, prime-agent, kimi, mistral), or be omitted to use your default.",
				map[string]any{"providers": []string{"codex", "claude-code", "pi", "opencode", "prime-agent", "kimi", "mistral"}})
		}
	}
	if req != nil {
		if s.applyPromptDefaults(c.P.User, sb, req) {
			body, _ = json.Marshal(req)
		}
		provider := s.effectiveProvider(sb, req)
		// The request's env and credentials count (crowdvm prompt --local-login
		// sends the caller's own sign-in). They are only read here: the body goes
		// to the guest untouched and is never logged or stored.
		reqEnv := map[string]string{}
		for _, field := range []string{"env", "credentials"} {
			if e, ok := req[field].(map[string]any); ok {
				for k, v := range e {
					if sv, ok := v.(string); ok {
						reqEnv[k] = sv
					}
				}
			}
		}
		// A named conversation without a provider keeps its own harness,
		// which only the guest knows: it reports a missing credential itself
		// (bugs-r18-sonnet 3).
		conv, _ := req["conversationId"].(string)
		prov, _ := req["provider"].(string)
		if !(conv != "" && prov == "") && !s.promptConfigured(sb, provider, reqEnv) {
			name := provider
			if p, ok := s.catalogProvider(provider); ok {
				name = p.Name
			}
			return ErrD(409, "provider_not_configured", "Prompting is locked until "+name+" is configured: add a key or sign-in on the dashboard (Agents & Proxying), or pass one with env.",
				map[string]any{"provider": provider, "setupUrl": s.Cfg.PublicURL + "/#/ai", "credentials": aiHarnessCreds[provider]})
		}
	}
	if sb.starting() {
		if sb, err = s.WaitUsable(c.R.Context(), sb.ID, promptStartWait); err != nil {
			if c.R.Context().Err() != nil {
				return Err(499, "client_closed", "The client went away.")
			}
			return err
		}
	}
	ready, err := s.guestReady(c.R.Context(), sb)
	if err != nil {
		var ae *api.Error
		if errors.As(err, &ae) && (ae.Code == "machine_not_running" || ae.Code == "sandbox_not_ready") {
			d := map[string]any{"state": sb.State, "retryable": false}
			return ErrD(409, "sandbox_not_promptable", "The sandbox is "+sb.State+"; resume it before prompting (crowdvm resume "+sb.ID+").", d)
		}
		return err
	}
	s.aiRenewForSandbox(c.R.Context(), ready) // renew expiring sign-ins before every prompt
	c.R.Body = io.NopCloser(bytes.NewReader(body))
	c.R.ContentLength = int64(len(body))
	c.R.Header.Set("Content-Length", strconv.Itoa(len(body)))
	s.ProxyToGuest(c.W, c.R, ready, "prompt")
	return nil
}

// effectiveProvider is the harness the guest will run (canonical id).
func (s *Server) effectiveProvider(sb *Sandbox, req map[string]any) string {
	p, _ := req["provider"].(string)
	if p == "" {
		p = firstNonEmpty(sb.Env["CROWDVM_DEFAULT_PROVIDER"], "claude")
	}
	if cp, ok := s.catalogProvider(p); ok {
		return cp.Provider
	}
	return strings.ToLower(p)
}

// applyPromptDefaults fills provider/model/fast from the caller's account
// settings; it reports whether req changed.
func (s *Server) applyPromptDefaults(u *User, sb *Sandbox, req map[string]any) bool {
	// The account's defaults are for new conversations: a prompt that names
	// one keeps that conversation's agent, which the guest knows
	// (bugs-r17-sonnet 4).
	if conv, _ := req["conversationId"].(string); conv != "" {
		return false
	}
	st := s.accountSettings(u.ID)
	changed := false
	provider, _ := req["provider"].(string)
	if provider == "" && st.DefaultProvider != "" && sb.Env["CROWDVM_DEFAULT_PROVIDER"] == "" {
		provider, req["provider"], changed = st.DefaultProvider, st.DefaultProvider, true
	}
	cp, known := s.catalogProvider(firstNonEmpty(provider, sb.Env["CROWDVM_DEFAULT_PROVIDER"], "claude"))
	model, _ := req["model"].(string)
	if model == "" && st.DefaultModel != "" && known && cp.Provider == firstNonEmpty(st.DefaultProvider, "claude") {
		if _, ok := cp.model(st.DefaultModel); ok {
			model, req["model"], changed = st.DefaultModel, st.DefaultModel, true
		}
	}
	if _, set := req["fast"]; !set && st.FastDefault && known {
		if m, ok := cp.model(firstNonEmpty(model, cp.DefaultModel)); ok && m.FastMode {
			req["fast"], changed = true, true
		}
	}
	return changed
}
