package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// agentGuide is the rule `crowdvm agent-instructions` prints: when to use a
// sandbox, the core commands, and a pointer to the crowdvm skill (skills/crowdvm)
// for the rest. Keep it short; details belong in the skill.
const agentGuide = `## Remote sandboxes (CrowdVM)

Use CrowdVM sandboxes (remote Ubuntu VMs with root and Docker, up to 16 vCPU / 32 GB)
in addition to this computer, not instead of it, unless the user says otherwise. Keep
working locally as usual and add sandboxes for extra capacity: run independent work
(test shards, builds, variants, other agents) on them in parallel while this machine
is busy. Move work there entirely only when it needs Docker, root or a clean machine,
runs untrusted or generated code, or must keep running after this session. Keep short
commands local. Details: the ` + "`" + `crowdvm` + "`" + ` skill (` + "`" + `crowdvm skill install` + "`" + `) or
` + "`" + `crowdvm <command> --help` + "`" + `. Limits and credits: ` + "`" + `crowdvm limits` + "`" + `, never from memory.

- Signed in? ` + "`" + `crowdvm status --check` + "`" + ` (exit 1: ask the human to run ` + "`" + `crowdvm login` + "`" + `)
- One-shot job: ` + "`" + `crowdvm run --push . --type large -- make test` + "`" + ` (new sandbox, copy this folder, run, delete; exits with the command's exit code)
- Create: ` + "`" + `id=$(crowdvm new --type large --json=false)` + "`" + ` (prints the id when ready; sizes ` + "`" + `small` + "`" + `, ` + "`" + `default` + "`" + `, ` + "`" + `large` + "`" + `, ` + "`" + `xlarge` + "`" + `; stops itself after 1 hour unless ` + "`" + `--ttl SECONDS` + "`" + `)
- Run: ` + "`" + `crowdvm exec "$id" --push . --timeout 0 -- npm test` + "`" + ` (copies this folder to /home/user/<folder> and runs there; ` + "`" + `--timeout 0` + "`" + ` = no limit, the default cuts a command after 30 s)
- Long job or server: ` + "`" + `pid=$(crowdvm exec "$id" --detach --json=false "npm run dev")` + "`" + `, then ` + "`" + `crowdvm exec "$id" --status "$pid"` + "`" + `
- Files: ` + "`" + `crowdvm push "$id" .` + "`" + ` sends git-tracked and new, not-ignored files one way; ` + "`" + `crowdvm scp -r "$id":/home/user/<folder>/dist ./dist` + "`" + ` copies results back
- URL for a port: ` + "`" + `crowdvm host "$id" 3000` + "`" + ` (opens on the team's LAN or Tailscale)
- Untrusted code or other people's work: add ` + "`" + `--no-env` + "`" + `; one sandbox per end user
- As availability allows: ` + "`" + `crowdvm limits --json` + "`" + ` gives ` + "`" + `machineTypes[].fitsNow` + "`" + ` (how many of each size you can start now); send out at most that many pieces, start each with ` + "`" + `--fail-fast` + "`" + `, and run a piece locally on ` + "`" + `no_ready_machine` + "`" + ` instead of waiting
- Always pass the sandbox id. When done: ` + "`" + `crowdvm stop "$id"` + "`" + ` (keeps the disk; ` + "`" + `crowdvm resume "$id"` + "`" + ` later) or ` + "`" + `crowdvm delete "$id" --yes` + "`" + ` (erases it)
`

func (a *App) agentInstructionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "agent-instructions",
		Aliases: []string{"agents-md"},
		Short:   "Print the AGENTS.md rule that tells a coding agent when and how to use crowdvm",
		Long: `Print a short rule for coding agents (Claude Code, Codex, Cursor, ...): when
to add sandboxes to the compute of this computer, and the core commands. Append it to
your project's AGENTS.md or CLAUDE.md:

  crowdvm agent-instructions >> AGENTS.md

The rule points to the crowdvm skill for the details; install that with
'crowdvm skill install'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.jsonOut && a.jsonSet {
				a.emit(map[string]any{"markdown": agentGuide})
				return nil
			}
			a.printf("%s", strings.TrimLeft(agentGuide, "\n"))
			return nil
		},
	}
}
