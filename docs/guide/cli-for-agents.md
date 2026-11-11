# The CLI for agents

The `crowdvm` CLI is the easiest way for a coding agent to get more compute than the
machine it runs on. The agent creates a sandbox, pushes code, runs commands, copies
results back and stops the sandbox.

## Teach the agent

```sh
crowdvm agent-instructions >> AGENTS.md     # or CLAUDE.md
```

That prints a short section with the workflow below, the sizes and the error codes
worth handling. Keep it in the repository so every agent session sees it.

## Sign the agent in

On a developer machine the agent uses your CLI session (`crowdvm login`). For CI,
servers and long-running agents, create a scoped API key and pass it in the environment:

```sh
crowdvm api-key create ci-runner --preset ci --ttl 30d    # prints cvm_... once
export CROWDVM_API_BASE=http://192.168.1.10:8420
export CROWDVM_API_KEY=cvm_...
crowdvm status --check                                    # exit 1 = not signed in
```

Presets are `read-only`, `full-sandbox`, `ci` and `admin`. `--sandbox ID` limits a key to
specific sandboxes. Give an agent the smallest preset that works.

## The workflow

Always pass the sandbox id. The "current sandbox" is per shell and is unreliable in
scripts.

```sh
id=$(crowdvm new --type large --json | jq -r 'select(.event=="ready") | .id')
crowdvm push "$id" .                                     # git-aware copy to /home/user/<dir name>
crowdvm exec "$id" --cwd myrepo --timeout 0 -- make -j8 test
crowdvm scp "$id":/home/user/myrepo/report.xml ./
crowdvm stop "$id"                                       # keeps the disk; crowdvm resume "$id" later
```

- `exec` exits with the remote exit code. The default timeout is 30 s; `--timeout 0`
  removes it and keeps the output streaming across network drops. Exit 124 means the
  command timed out.
- Long jobs and servers: `crowdvm exec "$id" --detach "npm run dev"` returns a process id;
  `--status`, `--attach` and `--kill` take it.
- One-shot jobs: `crowdvm run --push . --type large -- make test` creates a sandbox,
  runs the command and deletes the sandbox.
- Parallel work: prepare one sandbox, then `crowdvm fork "$id" --wait` as often as you
  need. A fork of a running sandbox is a copy of its disk at that moment.
- Sizes: `small` (2 vCPU, 4 GB), `default` (4, 8), `large` (8, 16), `xlarge` (16, 32).
  The server may define others; `crowdvm limits` shows what you may start.
- Auto-stop: after 1 hour by default. `--ttl SECONDS`,
  `--no-auto-stop`, or `crowdvm extend "$id" --hours 4`.

Stop or delete sandboxes when the work is done. A running sandbox holds CPU and RAM on
someone's machine and spends credits.

## Hand a turn to an agent in the sandbox

`crowdvm prompt "$id" "…"` runs a coding agent (Claude Code, Codex, OpenCode, pi)
inside the sandbox and streams its work. It runs on the credentials the sandbox
already has. With `--local-login` it runs on **your own login on this machine** for
that one turn instead:

```sh
crowdvm prompt "$id" --provider codex --model gpt-6-luna --local-login "Fix the build"
crowdvm prompt "$id" --provider claude --local-login=api-key "Write the tests"
```

`--local-login` takes `auto` (the default: subscription first, then an API key),
`subscription` or `api-key`, and needs `--provider`. `CROWDVM_LOCAL_LOGIN=1` (or
`subscription`, `api-key`) turns it on for a whole agent session; `--local-login=off`
turns it off again for one call. The CLI reads the login at every call and caches
nothing:

| `--provider` | Read, in order | Sent |
|---|---|---|
| `claude` | `$CLAUDE_CODE_OAUTH_TOKEN`; `claudeAiOauth.accessToken` in `$CLAUDE_CONFIG_DIR/.credentials.json` (default `~/.claude/.credentials.json`); `$ANTHROPIC_API_KEY` | `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` |
| `codex` | `tokens.access_token` + `tokens.account_id` in `$CODEX_HOME/auth.json` (default `~/.codex/auth.json`); that file's `OPENAI_API_KEY`; `$OPENAI_API_KEY` | `CHATGPT_ACCESS_TOKEN` + `CHATGPT_ACCOUNT_ID`, or `OPENAI_API_KEY` |
| `opencode`, `pi` | vendor API key env vars (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, …) | the same names |

- Only the access token goes out, never the refresh token or the id token. This
  machine stays the only one that refreshes the login.
- In the sandbox the token lives only in that turn's harness process (in memory,
  dropped when the turn ends) and wins over the sandbox's own credentials. It is not
  written to disk, events, command history or logs. Codex and Claude Code keep a
  subscription token out of their tool commands; OpenCode and pi tool commands can
  see the API keys you pass.
- A subscription token that expires within about 2 minutes is refused with
  `local_login_expired`: run `claude` (or `codex`) once on this machine to refresh its
  login, then retry. The `queued` event shows `localLogin.kind`, `source` and
  `expiresInSeconds` (never the token).
- On macOS Claude Code keeps its login in the Keychain, not in `.credentials.json`;
  set `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) or use `--local-login=api-key`.

Use `--local-login` when the sandbox has no AI credentials of its own and you want
the sandbox agent to bill your own subscription or key. Without the flag, store a key
or sign-in in a credential profile (`crowdvm ai profile …`) instead.

## JSON output and errors

Every command takes `--json`, and JSON is on when output is piped. Commands that wait
(`new`, `fork --wait`, `resume`) print one JSON object per line as events; pick the one
you need with `jq 'select(.event=="ready")'`.

On failure the last line is `{"event":"error","error":"...","code":"...","status":...}`
and the exit code is not 0. Codes worth handling:

| Code | Meaning | What to do |
|---|---|---|
| `sandbox_starting` | The sandbox is still booting | Retry after a few seconds |
| `no_ready_machine` | No node has room (only with `--fail-fast`) | Retry, or use a smaller `--type` |
| `size_unavailable` | No node in the pool can ever host this `--type` (details list `availableTypes`) | Use one of the available sizes; `crowdvm limits` shows them |
| `machine_not_running` | The sandbox is stopped | `crowdvm resume "$id"` |
| `sandbox_securing` | The server is removing secrets before handing it over | Retry |
| `billing_required` | The wallet is out of credits (only when credits are enforced) | Ask a human |
| `not_signed_in` | No server or key configured | Ask a human, or `crowdvm login --json` and show the URL |
| `local_login_not_found` | `--local-login` found no login for that provider here | Sign in (`claude`, `codex login`), set the API key, or drop `--local-login` |
| `local_login_expired` | The local subscription token expires within ~2 minutes | Run `claude` (or `codex`) once on this machine, then retry |

Without `--fail-fast`, `new`, `fork` and `resume` wait up to 15 minutes for a node with
room instead of failing.

## Placement

Most work runs anywhere. When it does not, say what it needs:

```sh
crowdvm new --gpu                       # a node whose owner passes a GPU through (VFIO)
crowdvm new --nested                    # KVM inside the sandbox (Android emulators, VMs)
crowdvm new --min-tier gold             # a fast node (see Node owner guide)
crowdvm new --category cpu=high         # a minimum benchmark level for one dimension
crowdvm new --node-label site=berlin    # labels the node owners set
crowdvm nodes                           # what the pool has
```

`--gpu` is placement: the sandbox lands only on a node whose owner passed a GPU through
with VFIO, and only while that GPU is free (one sandbox per GPU). The sandbox then has
that GPU to itself. A node's own GPU that is not passed through, or its virgl 3D
desktop, does not count (see the Node owner guide).

## Updates

`crowdvm self-update` installs the CLI build the server offers when it is newer than
yours. After other commands the CLI checks the server's version at most every 6 hours
and prints a one-line notice on the terminal. It never prints the notice in JSON mode
or when `CI` is set, and never installs anything by itself. `--no-update` or
`CROWDVM_NO_UPDATE=1` turns the check off.

## Inside a sandbox

Each sandbox has the CLI as `crowdvm` (also `host` and `boat`) and a token that lets it
manage itself: `host 3000` prints a URL for port 3000 of the sandbox it runs in.
