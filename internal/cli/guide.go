package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

const agentGuide = `## Extra compute (CrowdVM)

This machine can borrow persistent Ubuntu 24.04 VMs ("sandboxes") from the
team's CrowdVM pool with the ` + "`crowdvm`" + ` CLI. Use a sandbox for heavy builds and
test suites, Docker, untrusted code, long jobs, and anything that needs more
CPU/RAM than this machine has. Keep quick edits and small commands local.
Sandboxes have Docker, git, gh, Node.js, Bun, Python, Rust, Claude Code and Codex.
User ` + "`user`" + `, home /home/user, passwordless sudo, outbound internet.

Check first: ` + "`crowdvm status --check`" + ` (exit 1 = not signed in; then ask the
human, or run ` + "`crowdvm login --json`" + ` and show them the URL and code).
` + "`crowdvm nodes`" + ` shows the machines in the pool; ` + "`crowdvm limits`" + ` and
` + "`crowdvm credits`" + ` show what you may start.

Workflow (always pass the sandbox id; ` + "`current`" + ` is unreliable in scripts):

- Create: ` + "`id=$(crowdvm new --type large --json | jq -r 'select(.event==\"ready\") | .id')`" + `
  Sizes: small 2 vCPU/4 GB, default 4/8, large 8/16, xlarge 16/32. Default
  auto-stop is 1 hour (` + "`--ttl SECONDS`" + `, ` + "`--no-auto-stop`" + `, later ` + "`crowdvm extend $id --hours 4`" + `).
- Send code: ` + "`crowdvm push \"$id\" .`" + ` (git-tracked + untracked-not-ignored files go to
  /home/user/<dir name>; no node_modules, no .git). Or clone inside the sandbox.
- Run: ` + "`crowdvm exec \"$id\" --cwd <dir name> --timeout 0 -- npm test`" + `
  Exits with the remote exit code. Default timeout 30 s; ` + "`--timeout 0`" + ` = no limit,
  output streams live and survives network drops; 124 = timed out.
- Background jobs/servers: ` + "`pid=$(crowdvm exec \"$id\" --detach \"npm run dev\" | jq -r .processId)`" + `,
  then ` + "`crowdvm exec \"$id\" --status $pid`" + ` / ` + "`--attach $pid`" + ` / ` + "`--kill $pid`" + `.
- One-shot: ` + "`crowdvm run --push . --type large -- make -j8 test`" + ` (create, run, delete).
- Copy results back: ` + "`crowdvm scp \"$id\":/home/user/<dir>/report.xml ./`" + ` (` + "`-r`" + ` for folders).
- Open a port: ` + "`crowdvm forward \"$id\" --remote 3000`" + ` (localhost:3000 here) or
  ` + "`crowdvm host \"$id\" 3000`" + ` (a URL others on the LAN can open).
- Look around: ` + "`crowdvm history \"$id\"`" + `, ` + "`crowdvm ps \"$id\"`" + `, ` + "`crowdvm ssh \"$id\" \"cmd\"`" + `.
- Parallel work: ` + "`crowdvm fork \"$id\" --wait`" + ` copies a prepared sandbox; ` + "`crowdvm snapshot \"$id\" my-stack`" + `
  then ` + "`crowdvm new --from my-stack`" + ` starts more from a saved state.
- When done: ` + "`crowdvm stop \"$id\"`" + ` (keeps the disk; ` + "`crowdvm resume \"$id\"`" + ` later) or
  ` + "`crowdvm delete \"$id\" --yes`" + ` (erases it). Never leave sandboxes running idle: they
  use someone's machine and your credits.
- Work for other people (one agent per customer, a playground): one sandbox per end
  user, created with ` + "`--no-env`" + ` and deleted afterwards. Never reuse a sandbox for
  another user; nothing is cleaned in between.

Scripting: every command takes ` + "`--json`" + ` (on automatically when piped). Errors in
JSON mode end with {"event":"error","error":...,"code":...,"status":...,"details":...} and a
non-zero exit. Codes worth handling: sandbox_starting (retry), no_ready_machine
(no capacity: retry or use a smaller --type), machine_not_running (resume it).
`

func (a *App) agentInstructionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "agent-instructions",
		Aliases: []string{"agents-md"},
		Short:   "Print an AGENTS.md section that teaches a coding agent to use crowdvm",
		Long: `Print a short guide for coding agents (Claude Code, Codex, Cursor, ...).
Append it to your project's AGENTS.md or CLAUDE.md:

  crowdvm agent-instructions >> AGENTS.md`,
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
