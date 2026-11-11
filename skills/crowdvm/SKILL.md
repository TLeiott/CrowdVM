---
name: crowdvm
description: Use when you need more compute, isolation, Docker, a desktop or a long-running job and the `crowdvm` CLI is available. CrowdVM gives you persistent Ubuntu VMs ("sandboxes", ids `bx_` + 8 characters) on a team's LAN/Tailscale pool. Load this skill from outside a sandbox to create, drive, stop, fork and snapshot sandboxes (`crowdvm new`, `exec`, `push`, `scp`, `ssh`, `run`, `stop`, `resume`). Also load it whenever you run inside a CrowdVM sandbox (`CROWDVM_SANDBOX_ID` is set or `/etc/crowdvm/sandbox-id` exists): there the same CLI, also installed as `host`, `boat` and `box`, exposes ports on URLs, shows information about the sandbox, snapshots or stops it, drives the desktop for computer use, and explains what survives a stop.
---

# CrowdVM

CrowdVM runs persistent Ubuntu 24.04 VMs ("sandboxes") on machines a team lends to a
CrowdVM server. The `crowdvm` CLI mirrors Boat's `boat` CLI. Inside a sandbox the same
binary is also installed as `host`, `boat` and `box`, so Boat commands work unchanged.

Details: [references/cli.md](references/cli.md) (every command and flag),
[references/in-sandbox.md](references/in-sandbox.md) (the machine, env vars,
persistence), [references/api.md](references/api.md) (HTTP API and docs endpoints).

## 1. Find out where you are

```sh
echo "$CROWDVM_SANDBOX_ID"; cat /etc/crowdvm/sandbox-id 2>/dev/null
```

- Either one prints an id: you are **inside** sandbox `bx_...`. Go to section 3.
- Neither prints anything: you are **outside**. Go to section 2.

## 2. Outside: drive sandboxes

### Sign in

```sh
crowdvm status --check                         # exit 1 = not signed in
crowdvm login --server http://<server>:8420 --key-stdin <<< "$CROWDVM_API_KEY"
# or, with no flags at all: export CROWDVM_API_BASE=http://<server>:8420 CROWDVM_API_KEY=cvm_...
```

No key? Run `crowdvm login --json`, show the human the URL and code, then run
`crowdvm login --wait`. Never invent a key.

### Core loop

```sh
id=$(crowdvm new --type large --json | jq -r 'select(.event=="ready") | .id')
crowdvm push "$id" .                                  # git-aware copy to /home/user/<dir name>
crowdvm exec "$id" --cwd <dir name> --timeout 0 -- make -j8 test
crowdvm scp "$id":/home/user/<dir name>/report.xml ./
crowdvm stop "$id"                                    # or: crowdvm delete "$id" --yes
```

- `new --json` prints one JSON object per line (`created`, `state`, ..., `ready`). The
  `ready` line has the `id`. JSON is on automatically when output is piped.
- **Always pass the sandbox id.** `current` is per shell and unreliable in scripts.
- Sizes: `small` (2 vCPU / 4 GB), `default` (4 / 8), `large` (8 / 16), `xlarge` (16 / 32).
  The server may offer others; `crowdvm limits` shows what you may start.
- Auto-stop: 1 hour after create or resume by default. `--ttl SECONDS`,
  `--no-auto-stop`, or later `crowdvm extend "$id" --hours 4`. Commands, SSH and
  services do not move the deadline; a running agent prompt does.

### Run commands

| Need | Command |
|---|---|
| Short command (default timeout **30 s**, exit 124 on timeout) | `crowdvm exec "$id" "npm test"` |
| Long command, no limit, output streams and survives network drops | `crowdvm exec "$id" --timeout 0 -- make test` |
| Server or background job | `pid=$(crowdvm exec "$id" --detach "npm run dev" \| jq -r .processId)` |
| Check / follow / stop it | `crowdvm exec "$id" --status "$pid"` / `--attach "$pid"` / `--kill "$pid"` |
| Push a directory, then run in it | `crowdvm exec "$id" --push . --timeout 0 -- npm test` |
| One-shot: new sandbox, push, run, delete | `crowdvm run --push . --type large -- make test` |
| Interactive or full-screen | `crowdvm ssh -t "$id" -- tmux attach` |
| Past commands and their output | `crowdvm history "$id"`, `crowdvm history "$id" CMD_ID` |

`exec` exits with the remote exit code. One argument is a shell command line; more
arguments after `--` run as argv. `--cwd` is relative to `/home/user`.

### Files

- `crowdvm push "$id" DIR [--to PATH]`: in a git repo only tracked and untracked,
  not-ignored files go (no `.git`, no `node_modules`). Replaces files, deletes nothing.
- `crowdvm scp [-r] SRC... DST`: the remote side is `ID:/path`; relative remote paths
  start in `/home/user`. `--api` copies through the API instead of system `scp`.
- Read a stopped sandbox without resuming it: `crowdvm snapshot latest "$id"`, then
  `crowdvm snapshot pull SNAPSHOT_ID -o ./out --path /home/user/project`.

### Stop, delete, resume, fork, snapshot

| Command | Effect |
|---|---|
| `crowdvm stop "$id"` | Runs `~/.crowdvm/pre-stop`, stops Docker containers, snapshots the disk, frees the machine. Data is kept. |
| `crowdvm resume "$id"` | Boots the latest snapshot on any node with room. `--type` changes the size. |
| `crowdvm delete "$id" --yes` | Erases the sandbox and its snapshots. Cannot be undone. |
| `crowdvm fork "$id" --wait` | New sandbox from the newest state of `$id` (a running one is copied live). |
| `crowdvm snapshot "$id" NAME` | Saves the disk under NAME (replaces an older NAME). |
| `crowdvm new --from NAME [--count N]` | Starts new sandboxes from a saved snapshot. |

Stop or delete every sandbox when the work is done. A running sandbox holds CPU and RAM
on someone's machine and spends credits.

### Public URLs and ports

```sh
crowdvm exec "$id" --detach "npm run dev -- --port 3000"
crowdvm host "$id" 3000            # private URL with ?_token=  (share like a password)
crowdvm host "$id" 3000 --public   # no token
crowdvm forward "$id" --remote 3000   # localhost:3000 on this machine instead
```

URLs look like `http://bx-f7k2q9hd-3000.<domain>/` (host routing) or
`http://<server>/p/bx_f7k2q9hd/3000/` (path routing). The app may listen on
`localhost` or `0.0.0.0`. Vite and similar dev servers need the domain in
`allowedHosts`.

## 3. Inside a sandbox

You are user `user` (home `/home/user`, passwordless `sudo`). The CLI already knows the
server and this sandbox. With no id, `host`, `info` and `prompt` act on this sandbox;
commands that require an id take `self`.

```sh
host 3000                          # URL for port 3000 of this sandbox (= crowdvm host 3000)
host 3000 --public                 # no token
host list; host url 3000; host hide 3000
crowdvm info                       # state, size, IP, desktop, auto-stop time
crowdvm extend self --hours 4      # keep running longer
crowdvm snapshot self my-stack     # save this disk as a named snapshot
crowdvm stop self                  # stop yourself (your session ends)
crowdvm fork self --wait           # needs a key with sandbox.fork (see below)
```

What the in-sandbox CLI may do depends on its credentials:
`CROWDVM_TOKEN`/`ASCII_TOKEN` (always there) can only read this sandbox and host
ports. `CROWDVM_API_KEY` (present unless the environment withholds CLI credentials)
adds stop, extend, exec, files, desktop and snapshots, for this sandbox only. Creating,
forking or deleting sandboxes needs a broader key from the human
(`crowdvm login --api-key cvm_...`). A `403` means the key lacks the action.

### What survives a stop (and a resume, fork, or `new --from`)

| Saved | Not saved |
|---|---|
| `/home/user` and every other home | `/tmp`, `~/.cache` (emptied at stop, and at the first boot of a fork) |
| Folders you create at `/` (`/srv`, `/data`, ...) | Paths matched by `.crowdvmignore` files (deleted at stop) |
| Your changes in `/etc`, `/usr`, `/opt`, `/var`: packages, systemd units, databases | Running programs, memory, open ports |
| Docker named volumes, containers, images a container or Compose project uses | Docker build cache; images nothing uses |
| Enabled systemd services (they start again) | The journal; ufw rules; hostname, IP, SSH host keys |

A resume is like a reboot. To keep something running across stops, make it an
**enabled systemd service** (system unit, or a user unit; `user` lingers). Docker
containers that were running start again by themselves after a resume, fork or
`new --from` (whatever their restart policy); containers you stopped stay stopped.

Pre-stop hook: `~/.crowdvm/pre-stop` (or `~/.boat/pre-stop`) runs as `user` before
every normal stop, 60 s cap, failures ignored. Use it to flush databases or write state.
`crowdvm stop --force` skips it.

`.crowdvmignore` (gitignore syntax; `.boatignore` and `.boxignore` also work) leaves
rebuildable folders out of snapshots: `printf 'node_modules/\n.next/\n' > ~/app/.crowdvmignore`.
`.git` is never removed.

### Desktop and computer use

- An Xfce desktop runs on `DISPLAY=:0` (1920x1080) from boot. Commands already get
  `DISPLAY=:0`; set it yourself in other shells.
- Drive it with `xdotool`, `wmctrl`, `xclip`, screenshot with `scrot` or
  `import -window root shot.png`. Integrated agents have the `computer` MCP server
  (`cua-driver`).
- Record: `ascii-record-desktop start demo.mp4`, then `ascii-record-desktop stop`
  (`status` shows it). `crowdvm-record-desktop` is the same tool.
- A human watches with `crowdvm desktop ID` (noVNC link with a token, valid 10 minutes)
  or `crowdvm browser ID` (only Chrome).

### Integrated agents

From outside, hand work to a coding agent that runs inside the sandbox:

```sh
crowdvm prompt "$id" "Fix the failing tests"                         # streams its work
crowdvm prompt "$id" --provider codex --new "Refactor auth"
crowdvm events "$id" -f; crowdvm steer "$id" "also update docs"; crowdvm interrupt "$id"
```

Providers: `claude`, `codex`, `opencode`, `pi`, `prime-agent`, `kimi`, `mistral`. Missing
credentials give `provider_not_configured`.

If the sandbox has no AI credentials but the machine you run on is signed in to
Claude Code or Codex, add `--provider claude|codex --local-login`: that turn runs on
your local login (access token only, memory only in the sandbox, read fresh each
call). `local_login_expired` means: run `claude` (or `codex`) once on this machine,
then retry. `CROWDVM_LOCAL_LOGIN=1` turns it on for every prompt of a session.

## 4. Rules

- Always pass the sandbox id (or `self` inside a sandbox).
- Never quote prices or credit rates from memory. Run `crowdvm limits` and
  `crowdvm credits` for what you may start and spend.
- Run `crowdvm nodes` to see the pool. Placement flags (`--gpu`, `--nested`,
  `--min-tier`, `--category`, `--node`, `--node-label`) only land on nodes that offer
  that; `--gpu` needs a node that passes a GPU through. Without `--fail-fast`, `new`,
  `fork` and `resume` wait up to 15 minutes for room.
- Stop or delete sandboxes you no longer need. Never reuse a sandbox for another end user.
- Private URLs, desktop links and `cvm_` keys are secrets. Do not paste them into logs or
  public places.

## 5. Errors

In JSON mode the last line on failure is
`{"event":"error","error":...,"code":...,"status":...}` with a non-zero exit.

| Code | Do |
|---|---|
| `sandbox_starting`, `sandbox_securing` | Retry after a few seconds |
| `machine_not_running` | `crowdvm resume "$id"` |
| `no_ready_machine` | Retry, use a smaller `--type`, or drop placement flags |
| `size_unavailable` | No node can host this size: use a size from `crowdvm limits` |
| `not_signed_in` | Ask the human, or `crowdvm login --json` and show the URL |
| `billing_required`, `rate_limited` | Ask the human / wait `retryAfterSeconds` |

Start debugging with `crowdvm status`, `crowdvm notices`, `crowdvm info ID`,
`crowdvm ps ID`.
