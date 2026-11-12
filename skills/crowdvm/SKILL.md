---
name: crowdvm
description: Add compute to this computer with CrowdVM sandboxes, remote persistent Ubuntu VMs with root, Docker and a desktop on the team's own machines. Use them in addition to local compute, not instead of it, unless the user says otherwise - keep running work here and put extra or parallel work on sandboxes (test shards, a second build, variants, other agents), and move work there entirely only when it needs Docker, root or a clean machine, runs untrusted or AI-generated code, must keep running after this session, or needs a browser, GUI or a URL other people can open. Use when local CPU or RAM is busy or short, a build or test suite can be split, work can run in parallel, or the user says to use CrowdVM as availability allows. Also use whenever the user mentions CrowdVM, the `crowdvm` CLI or a sandbox id (`bx_...`), and when you are running inside a CrowdVM sandbox (`CROWDVM_SANDBOX_ID` is set) and need to host a port, keep work across a stop, snapshot or stop the sandbox, or use its desktop.
---

# CrowdVM

A sandbox is a persistent Ubuntu 24.04 VM on a machine in the team's pool: user `user`,
home `/home/user`, passwordless `sudo`, Docker, Node.js, Python, Go, Rust, Chrome, a
desktop, outbound internet. The `crowdvm` CLI creates and drives it. Ids look like
`bx_f7k2q9hd`.

If `echo "$CROWDVM_SANDBOX_ID"` prints an id, you are inside a sandbox. Go to
[Inside a sandbox](#inside-a-sandbox).

## When to use a sandbox

Sandboxes add compute to this machine; they do not replace it. By default keep running
work locally as usual and use sandboxes on top, unless the user says to use only
sandboxes or only this machine.

Add sandboxes for extra capacity:

- run independent work in parallel while this machine is busy: test shards, a second
  build, variants, other agents (one sandbox each);
- take on a heavy build or test suite when local CPU or RAM is short (sizes go up to
  16 vCPU / 32 GB), while you keep working here.

Move work to a sandbox entirely only when it:

- needs Docker, root, system packages or a clean machine;
- runs untrusted, generated or destructive code;
- must keep running when this session ends;
- needs a browser or desktop, or a URL that other people can open.

Keep quick edits and short commands local, and work that needs files, devices or
services only this machine or its LAN has. A sandbox reaches the internet, not your
LAN or localhost (`crowdvm forward --reverse` lends it one local port).

## Sign in

```sh
crowdvm status --check        # exit 1 = not signed in
```

If that fails, ask the human to run `crowdvm login --server http://<server>:8420`. Or
run `crowdvm login --json` yourself (with `--server URL` when none is configured), show
them the URL and code, then run `crowdvm login --wait`. Never invent a key or a URL.

## One-shot job

```sh
crowdvm run --push . --type large -- make -j8 test
```

This creates a sandbox, copies this folder, runs the command there with live output,
deletes the sandbox and exits with the command's exit code.

## A sandbox you keep

```sh
id=$(crowdvm new --type large --json=false)               # prints only the id, once ready
crowdvm exec "$id" --push . --timeout 0 -- make -j8 test  # copy this folder, run in the copy
crowdvm exec "$id" --cwd <folder> "ls dist"               # --cwd is relative to /home/user
crowdvm scp -r "$id":/home/user/<folder>/dist ./dist      # copy results back
crowdvm stop "$id"                                        # or: crowdvm delete "$id" --yes
```

- Always pass the id. Without one the CLI uses the profile's `current` sandbox, which
  another session may have changed.
- `--push DIR` and `crowdvm push "$id" DIR` copy to `/home/user/<folder>` (the local
  directory's name; `--to PATH` changes it). In a git repo only tracked and untracked,
  not-ignored files go: no `.git`, no `node_modules`, no ignored `.env`. The copy is
  one way: it replaces files, deletes nothing, and nothing comes back by itself. Fetch
  results with `crowdvm scp`. To commit or push from the sandbox, clone the repository
  there instead.
- Sizes: `small` (2 vCPU / 4 GB), `default` (4 / 8), `large` (8 / 16), `xlarge`
  (16 / 32). `crowdvm limits` shows what you may start, `crowdvm nodes` the pool.
- A sandbox stops itself 1 hour after create or resume, and a job that runs longer
  dies with it. Pass `--ttl SECONDS` or `--no-auto-stop`, or run
  `crowdvm extend "$id" --hours 4`. Commands, SSH and services do not move the
  deadline. A running agent prompt does.
- For untrusted code or other people's work, pass `--no-env` to `run` or `new`. The
  sandbox then gets none of your secrets. Use one sandbox per end user and delete it
  afterwards.
- When the work is done, `crowdvm stop "$id"` saves the disk and frees the machine,
  and `crowdvm resume "$id"` boots it again like a reboot. `crowdvm delete "$id" --yes`
  erases it for good. A running sandbox holds CPU and RAM on someone's machine and
  spends credits, so leave none behind. `crowdvm list` shows what still runs.

## Run commands

| Need | Command |
|---|---|
| Short command (cut after 30 s, exit 124) | `crowdvm exec "$id" "npm test"` |
| No time limit, live output | `crowdvm exec "$id" --timeout 0 -- make test` |
| Long job or server, in the background | `pid=$(crowdvm exec "$id" --detach --json=false "npm run dev")` |
| Its state (`.status`: `running`, `exited`, `lost`; `.exitCode`; log tails) | `crowdvm exec "$id" --status "$pid"` |
| Follow its output from the start / stop it | `crowdvm exec "$id" --attach "$pid"` / `--kill "$pid"` |
| Pipe data in (`exec` has no stdin) | `crowdvm ssh "$id" -- bash -s < setup.sh` |
| Past commands, and the stored output of one | `crowdvm history "$id"`, `crowdvm history "$id" CMD_ID` |

- `exec` exits with the remote exit code and prints the command's output as is. One
  argument is a shell command line (`"cd app && make"`); arguments after `--` run as
  argv. Commands run as `user` in `/home/user`.
- Ending the CLI (Ctrl-C, or the time limit of the tool you run it with) stops the
  remote command. For anything longer than that limit use `--detach` and poll
  `--status`; `--attach` may be interrupted and repeated.

## Parallel work

```sh
base=$(crowdvm new --type large --json=false)
crowdvm exec "$base" --push . --timeout 0 "npm ci"       # prepare once
for i in 1 2 3; do
  f=$(crowdvm fork "$base" --wait --json=false)          # a copy with its own disk and machine
  crowdvm exec "$f" --detach --cwd <folder> "npm test -- --shard=$i/3"
done
```

- A fork copies the disk, not running programs. Start them again in the fork. Docker
  containers that ran and enabled systemd services start by themselves.
- Each sandbox needs room on a node. Without `--fail-fast`, `new`, `fork`, `run` and
  `resume` wait up to 15 minutes for room. To size the batch, see the next section.
- `crowdvm snapshot "$base" NAME` saves the disk; `crowdvm new --from NAME` starts from
  it later (`--count N` creates N at once and does not wait until they are ready).
- To put a coding agent to work inside a sandbox, run
  `crowdvm prompt "$f" --new --no-wait "task"`, then watch `running` in
  `crowdvm conversations "$f"` and read `crowdvm events "$f"`. Without `--no-wait`,
  `prompt` prints the agent's work until the turn ends. `--provider` picks `claude`,
  `codex`, `opencode`, `pi` and others. On `provider_not_configured`, add
  `--provider claude --local-login` (or `codex`) to use this machine's own login.

## Use what is free

The pool is other people's machines and fills up. When the user says to use CrowdVM
"as availability allows", or you split work without knowing how much room there is,
send out only what fits now and run the rest here. Never wait for room.

```sh
crowdvm limits --json | jq -c '.machineTypes[] | {type, fitsNow}'
```

- `fitsNow` is how many sandboxes of that size you can start right now: free room on
  the pool's nodes, capped by your account limits. It is an estimate; others start
  sandboxes too. The text form is `crowdvm limits` (`large (3 fit now)`).
- Send out at most `fitsNow` pieces. If the size you want fits 0, a smaller one may
  fit. If every size fits 0, run everything locally.
- Start each with `--fail-fast` (`new`, `fork`, `run`, `resume`). On
  `no_ready_machine`, `limit_reached` or `rate_limited`, run that piece locally
  instead and go on.
- Check `fitsNow` again before each new batch, and stop or delete each sandbox as soon
  as its piece is done, so its room goes back to the pool.
- `crowdvm nodes` shows the pool per node: free room, and live load in `--json`
  (`load.cpuBusy`, `load.hot`: the owner is using that machine).

## URLs and ports

```sh
crowdvm exec "$id" --detach "npm run dev -- --port 3000"
crowdvm host "$id" 3000               # prints the URL; its ?_token= works like a password
crowdvm host "$id" 3000 --public      # no token
crowdvm forward "$id" --remote 3000 & # localhost:3000 on this machine, until you kill it
```

The CrowdVM server serves these URLs (`http://bx-f7k2q9hd-3000.<domain>/` or
`http://<server>/p/bx_f7k2q9hd/3000/`), so they open from the team's LAN or Tailscale,
not from the internet. The app may listen on `localhost` or `0.0.0.0`. Vite and similar
dev servers need the domain in `allowedHosts`.

## Inside a sandbox

You are user `user` (home `/home/user`, passwordless `sudo`). The CLI already knows the
server and this sandbox, and is also installed as `host`, `boat` and `box`. With no id,
`host`, `info` and `prompt` act on this sandbox; commands that require an id take `self`.

```sh
host 3000                          # URL for port 3000 of this sandbox (= crowdvm host 3000)
host 3000 --public                 # no token
host list; host url 3000; host hide 3000
crowdvm info                       # state, size, IP, desktop, auto-stop time
crowdvm extend self --hours 4      # keep running longer
crowdvm snapshot self my-stack     # save this disk as a named snapshot
crowdvm stop self                  # stop yourself (your session ends)
```

- **Credentials.** `CROWDVM_TOKEN`/`ASCII_TOKEN` (always there) can only read this
  sandbox and host ports. `CROWDVM_API_KEY` (there unless the environment withholds CLI
  credentials) adds stop, extend, exec, files, desktop and snapshots, for this sandbox
  only. Creating, forking or deleting sandboxes needs a broader key from the human
  (`crowdvm login --api-key cvm_...`). A `403` means the key lacks the action.
- **What a stop keeps.** The disk, and the same holds for resume, fork and
  `new --from`. That covers every home, folders you create at `/`, your changes in
  `/etc`, `/usr`, `/opt` and `/var` (packages, systemd units, databases), Docker
  volumes, containers and the images they use.
- **What a stop drops.** Running programs, memory and open ports, `/tmp` and
  `~/.cache`, paths matched by a `.crowdvmignore` file (gitignore syntax), the Docker
  build cache and unused images, ufw rules.
- **A resume is a reboot.** To keep something running across stops, make it an enabled
  systemd service (system unit, or a user unit; `user` lingers). Docker containers
  that were running start again by themselves. Containers you stopped stay stopped.
- **Pre-stop hook.** `~/.crowdvm/pre-stop` runs as `user` before every normal stop,
  for at most 60 s, and a failure is ignored. Use it to flush databases.
- **Desktop.** Xfce on `DISPLAY=:0` (1920x1080) from boot; commands already get
  `DISPLAY=:0`. Drive it with `xdotool`, `wmctrl`, `xclip`; screenshot with
  `scrot shot.png`; record with `ascii-record-desktop start demo.mp4`, then
  `ascii-record-desktop stop`. Integrated agents have the `computer` MCP server. A
  human watches with `crowdvm desktop ID` or `crowdvm browser ID` (only Chrome).

## Output and errors

- Piped, which is how you run it, most commands print JSON, one object per line.
  `exec`, `run`, `ssh` and `host` print plain output. `--json=false` forces text
  (`new` and `fork` then print only the id), `--json` forces JSON.
- A failure exits non-zero. JSON commands end with
  `{"event":"error","error":...,"code":...,"status":...}` on stdout; the others print
  `error: MESSAGE (CODE, HTTP STATUS)` on stderr.

| Code | Do |
|---|---|
| `sandbox_starting`, `sandbox_securing` | Retry after a few seconds |
| `machine_not_running` | `crowdvm resume "$id"` |
| `no_ready_machine` | Retry, use a smaller `--type`, or drop placement flags |
| `size_unavailable` | No node can host this size: use a size from `crowdvm limits` |
| `not_signed_in` | See [Sign in](#sign-in) |
| `billing_required`, `rate_limited` | Ask the human / wait `retryAfterSeconds` |

Debug with `crowdvm status`, `crowdvm notices`, `crowdvm info ID`, `crowdvm ps ID`.

## Rules and more

- Never quote prices or credit rates from memory. Run `crowdvm limits` and
  `crowdvm credits`.
- Private URLs, desktop links and `cvm_` keys are secrets. Keep them out of logs,
  commits and public places.
- [references/cli.md](references/cli.md): every command and flag, placement (`--gpu`,
  `--nested`, `--node`), snapshots, environments, integrated agents, API keys.
- [references/in-sandbox.md](references/in-sandbox.md): the machine, network, env
  vars, what a snapshot keeps, systemd and pre-stop examples, desktop.
- [references/api.md](references/api.md): the HTTP API and the docs endpoints.
