# Inside a CrowdVM sandbox

## Am I in one?

```sh
[ -n "$CROWDVM_SANDBOX_ID" ] || [ -f /etc/crowdvm/sandbox-id ] && echo inside
```

`/etc/crowdvm/sandbox-id` holds the id; it is also what `self` resolves to when the
variable is missing (cron jobs, services without the sandbox env).

## The machine

- Ubuntu 24.04 VM with its own kernel. Hostname `crowdvm` (changes are not kept).
- User `user` (uid 1000), home `/home/user` (mode 0750), passwordless `sudo`, groups
  `adm sudo kvm systemd-journal docker`. Locale `en_US.UTF-8`.
- Commands from `crowdvm exec` run as `user` in `bash -lc`, with `DISPLAY=:0`.
- `sudo apt-get install -y X` works at once (apt lists are kept). No snap.
- Tools: Docker CE with buildx and compose; Node.js LTS with npm and pnpm, nvm, Bun,
  Deno; Python 3 with pip, venv, pipx and uv; Go; Rust (rustup, as `user`); JDK 11,
  Maven, Gradle, Kotlin, Scala, sbt; Ruby; PHP and composer; Erlang and Elixir; .NET 8;
  R; gcc, clang, cmake, ninja; git, git-lfs, `gh`, jq, ripgrep, fd, tmux, htop, strace,
  sqlite3; Google Chrome, VS Code, Ghostty or xfce4-terminal; xdotool, wmctrl, xclip,
  ffmpeg, scrot, ImageMagick.
- Agent CLIs: `claude`, `codex`, `opencode`, `pi`, `prime-agent`, `kimi`, `vibe`,
  `hermes`, `openclaw`, `cursor-agent` / `agent`, `cua-driver`.
- The CLI: `/usr/local/bin/crowdvm` with the links `host`, `boat` and `box`. It is
  installed from the node at every boot; if it is missing, the node has no in-sandbox CLI
  configured.

## Network and firewall

- Outbound: internet is allowed. The node's LAN, RFC 1918 ranges, Tailscale
  (`100.64.0.0/10`), link-local and multicast are blocked by default; the CrowdVM server
  is always reachable. Only the node owner can open more.
- Inbound: `ufw` denies everything except `22/tcp` (SSH) and the guest agent port
  (`7070/tcp`). The rules are reset at every boot, so a resume or fork never keeps your
  `ufw allow` lines. Docker-published ports bypass ufw, as usual.
- Hosted URLs (`host PORT`) do not need a ufw rule: the server reaches the port from
  inside the sandbox through the agent tunnel, on `localhost` or `0.0.0.0`.
- Raw TCP: on nodes that run with a direct host, `host PORT --public` also publishes that
  TCP port on the node's LAN/Tailscale address and opens it in ufw (again after every
  resume). Raw UDP ports asked for through the API are published but not opened: run
  `sudo ufw allow PORT/udp` yourself, again after each resume (e.g. from a systemd unit).

## Environment variables

| Variable | Meaning |
|---|---|
| `CROWDVM_SANDBOX_ID` | This sandbox's id (`bx_...`) |
| `CROWDVM_API_URL` | The server's API base, `<server>/api/v1` |
| `CROWDVM_TOKEN`, `ASCII_TOKEN` | Neutral per-sandbox token: read this sandbox, host its ports. Always set |
| `CROWDVM_API_KEY` | Per-sandbox CLI key when the environment passes CLI credentials (`env set --crowdvm-credentials true`, the default). Scoped to this sandbox: read, update, stop, exec, files, SSH, desktop, host, prompt, snapshots. Not: create, fork, resume, delete |
| `DISPLAY` | `:0` |
| Your environment's variables | From `crowdvm env set-var` and `new -e KEY=VALUE` |

The guest also writes `~/.config/crowdvm/config.json` (profile `default`: server and
`CROWDVM_API_KEY`), so the CLI works in any shell. For actions the sandbox key lacks
(fork, new, delete), ask the human for a key and run
`crowdvm login --key-stdin <<< "$KEY"`; a key you add yourself is never overwritten.

## Commands about this sandbox

```sh
crowdvm info                  # state, size, IP, desktop, auto-stop
crowdvm ps self               # processes
crowdvm history self          # past commands and their output
host 3000 [--public]          # URL for a port; host list | host url 3000 | host hide 3000
crowdvm extend self --hours 4 | --no-auto-stop
crowdvm snapshot self NAME    # saved snapshot of this disk
crowdvm stop self             # ends this session; resume from outside
```

Auto-stop counts from the last create or resume. Commands, SSH and services do not
extend it; a running integrated-agent prompt does.

## What a snapshot keeps

A stop (and the snapshot behind a fork or `new --from`) keeps the whole disk except:

| Not kept | Why / what to do |
|---|---|
| `/tmp`, `~/.cache` | Emptied at stop; a fork or `new --from` of a running sandbox empties them at its first boot. Install tool caches elsewhere, e.g. `PLAYWRIGHT_BROWSERS_PATH=$HOME/.local/share/ms-playwright` on the first line of `~/.bashrc` |
| `.crowdvmignore` matches | Deleted at stop (and at the first boot of a fork of a running sandbox). gitignore syntax, rules apply to the folder the file is in, searched up to six levels below home (not inside `node_modules`, `.next`, `target`, `vendor`). `.boatignore`, `.boxignore` and `.oneignore` work too. `.git` is never deleted. CrowdVM does not read `.gitignore` for this |
| Docker build cache | Pruned at stop. Keep it with `--cache-to type=local,dest=$HOME/.buildcache,mode=max --cache-from type=local,src=$HOME/.buildcache` |
| Docker images that no container and no Compose project (with a container) uses | Deleted at stop. Keep one with `docker create --name keep-x IMAGE` |
| Running programs, memory, open ports | A resume is a reboot. Use systemd services |
| Journal, ufw rules, hostname, IP, SSH host keys, machine-id (fork) | Per boot. Set them from an enabled unit if you need them |

Kept: `/home/user` and other homes, folders you create at `/` (`/srv`, `/data`, ...),
your changes in `/etc`, `/usr`, `/opt`, `/root`, `/var` (packages, systemd units,
crontabs, database files), Docker named volumes, containers and the images they or
their Compose projects use. Containers that were running start again after a resume,
fork or deploy.
Deleted files stay deleted.

Periodic snapshots run every 60 seconds while something changes; resume and fork use the
newest one. There is no point-in-time restore. With `--no-snapshots`, a stop erases the
sandbox.

## Stop sequence

1. `~/.crowdvm/pre-stop` (else `~/.boat/pre-stop`) runs as `user` in the home folder,
   at most 60 s; failures are ignored; a non-executable file runs with `bash FILE`.
2. The running containers are recorded, then `docker stop -t 30` stops them.
3. Ignored paths, `~/.cache` and `/tmp` are deleted; build cache and unused images are
   pruned; `sync`; the filesystem is frozen and the VM is powered off. The OS does not
   shut down and no other program gets a signal.

`crowdvm stop --force` skips steps 1 and 2 and keeps only the last successful snapshot.

Example hook:

```sh
mkdir -p ~/.crowdvm
cat > ~/.crowdvm/pre-stop <<'EOF'
#!/bin/sh
cd ~/app && docker compose exec -T db pg_dump -U app app > ~/backup.sql
EOF
chmod +x ~/.crowdvm/pre-stop
```

## Keep things running across stops

Programs started by hand (`nohup`, `&`, `exec --detach`) are gone after a resume. Make
them enabled systemd services:

```sh
sudo tee /etc/systemd/system/myapp.service >/dev/null <<'EOF'
[Unit]
Description=My app
After=network-online.target docker.service

[Service]
User=user
WorkingDirectory=/home/user/myapp
ExecStart=/usr/bin/npm run start
Restart=always

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl enable --now myapp.service
```

User units (`~/.config/systemd/user/`, `systemctl --user enable --now X`) also start at
boot, because `user` lingers.

Docker: the containers that were running at the stop (or at the live snapshot a fork
or saved snapshot came from) start again by themselves at the next boot, whatever their
restart policy. Containers you stopped stay stopped. Service output (journald, Docker json-file
logs) shows up in `crowdvm history` as `process` records.

## Desktop and computer use

- Xvnc `:0` (1920x1080) with an Xfce session as `user`, running from boot. VNC listens on
  `127.0.0.1:5900` only; humans reach it through `crowdvm desktop ID`.
- `export DISPLAY=:0` in shells that lack it. `xdotool`, `wmctrl -l`, `xclip`,
  `scrot shot.png` or `import -window root shot.png`.
- `cua-driver` runs as a user service (`systemctl --user status cua-driver.service`) on
  `/run/ascii-cua/driver.sock`. Integrated agents get it as the MCP server `computer`
  (`cua-driver mcp --socket /run/ascii-cua/driver.sock`).
- Recording: `ascii-record-desktop start demo.mp4`, `ascii-record-desktop status`,
  `ascii-record-desktop stop` (H.264 MP4, 30 fps; `crowdvm-record-desktop` is the same
  tool). A recording stops when the sandbox stops.
- Wallpaper: put a PNG at `~/.config/crowdvm/wallpaper.png`.

## Customize the integrated agents

Config is per sandbox, kept in the home folder, and shared by every conversation.

House rules: one `~/AGENTS.md` plus one `~/CLAUDE.md` cover Claude Code, Codex, pi,
OpenCode, Prime Agent and Kimi Code. Mistral Vibe reads `AGENTS.md` only at a trusted
git repository root; give it a skill instead.

Skills directories (one folder per skill with a `SKILL.md`):

| Harness | Skills directory |
|---|---|
| Claude Code | `~/.claude/skills/<name>/SKILL.md` |
| Codex | `~/.codex/skills/<name>/SKILL.md` |
| pi | `~/.pi/agent/skills/<name>/SKILL.md` |
| OpenCode | `~/.config/opencode/skills/<name>/SKILL.md` |
| Prime Agent | `~/.prime/agent/skills/<name>/SKILL.md` |
| Kimi Code | `~/.kimi-code/skills/<name>/SKILL.md` (also `~/.agents/skills/`) |
| Mistral Vibe | `~/.vibe/skills/<name>/SKILL.md` (also `~/.agents/skills/`) |
| Hermes, OpenClaw, Cursor CLI | `~/.hermes/skills/`, `~/.openclaw/skills/`, `~/.cursor/skills/` |

This `crowdvm` skill is in all of them; the guest agent refreshes it at every boot unless
you edited or deleted a copy (yours wins).

MCP servers: `claude mcp add --scope user NAME -- CMD`, `codex mcp add NAME -- CMD`,
`prime-agent mcp add NAME -- CMD`; OpenCode, Kimi and Vibe use their config files.
To reach a service on the human's machine, they run
`crowdvm forward ID --reverse --local PORT` there; it appears at `127.0.0.1:PORT` in the
sandbox while the tunnel runs.

Any program on `PATH` (`~/.local/bin`, `/usr/local/bin`) is a tool for every harness.
