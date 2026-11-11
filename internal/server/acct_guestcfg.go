package server

// Account data-retention settings that reach the guest at every boot
// (DATA-009/011): account monitoring off or zero data retention set
// proto.GuestConfig.NoMonitoring (lifecycle.go sets it from the sandbox's
// own "monitoring" flag), ZDR also sets ZeroDataRetention.

import (
	"context"

	"github.com/crowdvm/crowdvm/internal/proto"
)

func init() {
	RegisterFeature("acct_guestcfg", func(s *Server) error {
		s.Hooks.PrepareGuest = append(s.Hooks.PrepareGuest, s.acctPrepareGuest)
		return nil
	})
}

func (s *Server) acctPrepareGuest(ctx context.Context, sb *Sandbox, g *GuestPrep) error {
	if g.Boot == proto.BootInspect || g.Config == nil {
		return nil
	}
	var zdr, monOff bool
	_ = s.DB.QueryRow(`SELECT zdr, monitoring_off FROM account_policy WHERE user_id=?`, sb.OwnerID).Scan(&zdr, &monOff)
	if !sb.Monitoring || monOff || zdr {
		g.Config.NoMonitoring = true
	}
	g.Config.ZeroDataRetention = zdr
	if sb.NoEnv && g.Config.Env != nil {
		aiDeepSeekFromEnv(g.Config.Env)
	}
	return nil
}

// aiDeepSeekFromEnv: one per-sandbox DEEPSEEK_API_KEY (--no-env -e) sets up
// Claude Code and Codex unless the user passed their own vendor's key.
func aiDeepSeekFromEnv(env map[string]string) {
	key := env["DEEPSEEK_API_KEY"]
	if key == "" {
		return
	}
	active := map[string]aiSecret{}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_BASE_URL"} {
		if env[k] != "" {
			active["anthropic-api-key"] = aiSecret{}
		}
	}
	for _, k := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL"} {
		if env[k] != "" {
			active["openai-api-key"] = aiSecret{}
		}
	}
	p := &envPlan{Env: map[string]string{}}
	aiDeepSeekHarnesses(p, active, key, firstNonEmpty(env["DEEPSEEK_BASE_URL"], "https://api.deepseek.com"))
	for k, v := range p.Env {
		if _, set := env[k]; !set || k == "ANTHROPIC_API_KEY" {
			env[k] = v
		}
	}
}
