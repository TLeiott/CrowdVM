package guest

import (
	"path/filepath"

	"github.com/crowdvm/crowdvm/internal/guest/agents"
)

// newAgentsManager wires the integrated coding agents (/v1/prompt, /v1/events,
// ...) into the guest mux, behind the same auth.
func newAgentsManager(a *Agent) *agents.Manager {
	return agents.New(agents.Options{
		Home: a.home, User: a.userName, Unprivileged: a.unpriv,
		DataDir:   filepath.Join(a.data, "agents"),
		Env:       a.sandboxEnv,
		Emit:      a.events.emit,
		AIProxy:   a.config().AIProxy,
		SandboxID: a.cfg.SandboxID,
		// Prompts wait while a setupBlocking repo clones or runs its setup (ENV-032).
		SetupBlocked: a.repoSetupBlocking,
		// The computer-use MCP tools act on the desktop, so bring it up first.
		EnsureDesktop: func() error { return a.desk.start() },
		// ENV-027: an environment with exactly one repo starts agents in it.
		DefaultCwd: func() string {
			if repos := a.config().Repos; len(repos) == 1 {
				return a.real(repoVirtPath(repos[0]))
			}
			return ""
		},
		Spawn: a.SpawnAgent,
		// Account zero data retention: no prompt text or events on disk.
		ZeroDataRetention: func() bool { return a.config().ZeroDataRetention },
	})
}

// aiProxyEnv: the variables that point harness CLIs at the AI credentials
// proxy (defined by the agents package).
func (a *Agent) aiProxyEnv() map[string]string {
	if a.agentsM == nil || a.config().AIProxy == nil {
		return nil
	}
	return a.agentsM.ProxyEnv()
}
