# crowdvm-guest

The agent inside every VM (`cmd/crowdvm-guest`, package `internal/guest`). It runs
as root from `crowdvm-guest.service` and listens on `:7070` (`GuestConfig.ListenAddr`).
Every endpoint except `GET /v1/health` needs `Authorization: Bearer <GuestConfig.Token>`.
`X-Request-Id` is echoed. Errors use the `internal/api` envelope.

```
crowdvm-guest --config /run/crowdvm/cfg/config.json             # in the VM, as root
crowdvm-guest --config cfg.json --home DIR --unprivileged       # on a host (tests, process driver)
   [--data-dir DIR] [--listen 127.0.0.1:7070] [--version]
```

## Paths and state

| | VM | `--unprivileged --home DIR` |
|---|---|---|
| `/home/user` | `/home/user` | `DIR` |
| `/tmp` | `/tmp` | `DIR/.local/state/crowdvm-guest/tmp` |
| agent state (`--data-dir`) | `/var/lib/crowdvm` | `DIR/.local/state/crowdvm-guest` |
| commands run as | `user` (uid 1000, its groups), `bash -lc` | current user, `HOME=DIR`, `TMPDIR=` the mapped /tmp |

The API always speaks the virtual paths (`/home/user/...`, `/tmp/...`); relative paths
are relative to `/home/user`. Anything resolving outside those two roots (after
following symlinks) is `400 invalid_path`. Unprivileged mode also accepts the real
paths and maps them back.

State under the data dir (on the guest disk, so it survives stop/resume/fork):
`history/<cmd_id>/{meta.json,stdout,stderr}` (command history and the live log files of
every command), `setup.json`, `setup.log`/`setup.err`, `boot.json` (makes boot tasks
idempotent across agent restarts), `agents/` (integrated agents).

## Public endpoints (Boat-compatible bodies, see `.ref/boat-v1.yaml`)

`/sandboxes/{id}/<rest>` on the server maps to `/v1/<rest>` here.

| Endpoint | Notes |
|---|---|
| `POST /v1/commands` | `{command, cwd?, timeoutSeconds? (1-600, default 30), detached?, stream?, env?}`. Sync → `command.finished`; `stream:true` → NDJSON `started`/`stdout`/`stderr`/`exit`/`error`; `detached:true` → `command.started` (`processId` = pid, no time limit). `env` is a CrowdVM extension. A sync command does not wait for background children (`cmd &`). |
| `GET /v1/commands/{processId}` | `command.status`; `tailBytes`, or `stdoutOffset`/`stderrOffset` (+ `*NextOffset`, `more`). Also returns `commandId`. |
| `GET /v1/commands/{processId}/stream` | NDJSON attach: `attached`, `stdout`/`stderr` (+`nextOffset`), `heartbeat` (15 s idle), `exit`, `reconnect` (after 10 min). |
| `DELETE /v1/commands/{processId}` | SIGTERM to the process group, SIGKILL after 5 s → `command.stopped` with the record. |
| `GET /v1/command-history` | `mode` (`sync,stream,detached,process,commands`), `q`, `limit` (1-100), `cursor` → `{commands, nextCursor}` newest first. |
| `GET /v1/command-history/{commandId}` | `command.info` |
| `GET /v1/command-history/{commandId}/logs` | `stream`, `offset`, `limit` (≤ 1 MiB), `encoding` (`utf8`/`base64`). |
| `GET /v1/processes` | live `/proc` sample (250 ms CPU window), busiest first, ≤ 500, `commandId` when a process belongs to a running command. |
| `GET/PUT/DELETE /v1/files`, `GET /v1/files/{list,info,search,find}`, `POST /v1/files/{folder,move,permissions,replace}` | as in the spec (5 MiB read/write limit; files and created folders owned by `user`). `move` to an existing destination without `overwrite` is `409 destination_exists`. |
| `GET /v1/artifacts?path=` | a file ≤ 50 MiB as raw bytes; **a folder as a streamed `.tar.gz`** (extension). |
| `POST /v1/sshkey` | `{key}` → appended once to `~/.ssh/authorized_keys`; `machineIp`/`sshEndpoint` are `null` (the server fills them), `hostKey` from `/etc/ssh`. |
| `/v1/prompt`, `/v1/prompts/{id}`, `/v1/events`, `/v1/conversations`, `/v1/steer`, `/v1/interrupt` | served by `internal/guest/agents`, behind the same auth. An empty prompt is `400 prompt_required`; a steer the running harness refuses is `409 no_running_turn` with `reason: delivery_failed` (the steer event then says `status: failed`). |

## Internal endpoints (node / server only)

| Endpoint | Body / answer |
|---|---|
| `GET /v1/health` (no auth) | `{ok, type:"guest.health", version, sandboxId, boot, uptimeSeconds, setupStatus, setupBlocking, monitoring, unprivileged, reposCloning}` (memory only, never touches the disk). `reposCloning`: this boot's environment repositories are still being cloned; the node reports ready only after (LIFE-009). The node copies `version` into `VMStatus.GuestVersion` (CMD-012). |
| `GET /v1/tunnel?port=N` | `Upgrade: crowdvm-tunnel` → `101`, then raw TCP to `127.0.0.1:N`. Nothing listening → `502 port_unreachable` (before upgrading). Use `mux.DialUpgrade`. |
| `GET /v1/files/raw?path=` | raw bytes, no size limit, `Range` supported. For the server's signed `files/download-url`. |
| `PUT /v1/files/raw?path=` | raw body → file (parents created, replaced only when complete) → `file.written {path,size}`. For signed `files/upload-url`. |
| `POST /v1/internal/prepare-stop` | optional body `proto.PrepareStopRequest {force, freeze, freezeTimeoutSeconds}`. Unless `force`: runs the user's pre-stop hook `~/.crowdvm/pre-stop` (else `~/.boat/pre-stop`; as `user`, cwd home, 60 s cap, failures ignored; not executable → `bash FILE`), then records the running containers (`<data dir>/docker-running.json`, oldest first) and `docker stop -t 30`s them. With `force` it only records them (2 s cap). Then stops a desktop recording; deletes `.crowdvmignore` / `.boatignore` / `.boxignore` / `.oneignore` matches (gitignore syntax, one file per folder in that priority, searched ≤ 6 levels below home, not inside `node_modules`, `.next`, `target`, `vendor`; `.git` is never deleted), the contents of `~/.cache` and `/tmp`; root only: journal vacuum, `docker builder prune -af`, unless `force` the images that no container and no Compose project with a container uses (`docker compose config --images` per project from the container labels; a project that cannot be resolved keeps every image), `fstrim -av`; then `sync`; with `freeze` it FIFREEZEs the writable block filesystems (auto-thaw after `freezeTimeoutSeconds`, default 60; the node kills the VM right after). → `guest.prepared_stop {preStop: null\|{path, exitCode, durationMs, timedOut?, outputTail?}, dockerStopped, removed, removedPaths (≤ 200), frozen, frozenMounts?, errors?}`. Also records a clean stop (`boot.previous`). |
| `POST /v1/internal/freeze` | optional `proto.FreezeRequest {timeoutSeconds}` (0-600, default 60): records the running containers (2 s cap; unchanged list = no write), `sync` + FIFREEZE → `guest.frozen {mounts}` (live snapshots). Unprivileged: no-op. |
| `POST /v1/internal/thaw` | idempotent → `guest.thawed {mounts}`. After a prepare-stop of this boot (the stop failed, the machine runs on) it removes the clean-stop marker and starts the recorded containers again. |
| `PUT /v1/internal/firewall` | `{open: ["8000/tcp", "53/udp"]}` (URL-012): ufw `allow` for each spec, `delete allow` for specs the guest added before and that left the set; a rule the user already had ("Skipping") is never the guest's. Waits (≤ 60 s) for this boot's firewall reset. The node sends the TCP direct ports on every change and every event-stream (re)connect. → `guest.firewall {open, added, removed, active}`; unprivileged/no ufw: records only (`active:false`). |
| `POST /v1/internal/apply-config` | `proto.ApplyConfig` (nil field = unchanged): env (`env` replaces, `envSet`/`envUnset` patch; profile.d + `~/.bashrc` block; new commands, shells and agent turns), secret files (written; managed files no longer listed are deleted), repos (new ones cloned + set up in the background), registry logins (dropped ones `docker logout`), AI proxy (`agents.SetAIProxy`), extra authorized keys, optional `scrub` first → `guest.config_applied {envKeys, filesWritten, filesRemoved, registriesLoggedIn, registriesLoggedOut, reposStarted, scrub?}` (ENV-007) |
| `GET /v1/internal/scrub` | `guest.scrub {id, done, error}`: outcome of `GuestConfig.Scrub`, run synchronously in `Boot` before the API listens (ENV-018, SNAP-023) |
| `GET /v1/internal/history/export?flush=0\|1&maxBytes=N` | `history.export` = `proto.HistoryExport {batch, records, chunks, samples, more}`: changed records, new output (Boat cadence: stream 2 s, detached/process 30 s, sync when done ≤ 8 MiB/stream; `flush=1` all, ignored under ZDR) and 30 s samples not acked yet. Empty when monitoring is off. |
| `POST /v1/internal/history/ack` | `{batch}` → `history.acked`; unknown batch `404 batch_not_found`. Unacked data is exported again (idempotent by commandId / (commandId, stream, offset) / sample time). |
| `POST /v1/internal/browser/start` | `{profile?, width?, height?}` → `browser.started {running, port:5902, display:":2", profile, isolated, reused}`. Xvnc :2 (one viewer, no clipboard) + a loop keeping Chrome open on the profile (default `/var/lib/crowdvm/browser-profile`; must be outside `/home/user` and system dirs, `400 invalid_profile`). Runs as `crowdvm-browser` in a private mount namespace where `/home/user`, `/tmp`, `/var/lib/crowdvm`, `/run/crowdvm`, `/root` are empty tmpfs and the env file is `/dev/null` (`isolated:true`, DESK-017); without that user it runs as `user`. Same profile → reused; another profile → restarted. `409 browser_not_installed` without Xvnc/Chrome. |
| `GET /v1/internal/browser/status` | `browser.status {running, port, display, profile, isolated}` |
| `POST /v1/internal/playbook/finish` | ends this boot's startup playbook recording now and writes the file → `playbook.finished {recording, path?, paths?, recorded}` (the node calls it when a lazy chain is hydrated; LIFE-018) |
| `POST /v1/internal/adopt` | only on a warm pool VM (`GuestConfig.Boot: warm`, SIZE-012; bearer = the warm token): body = the sandbox's `GuestConfig` → `guest.adopted {sandboxId}`; the config is saved as `<data dir>/adopted.json` and the agent restarts as that sandbox (a fresh create). Later restarts and reboots use `adopted.json`. A warm guest serves only this and `GET /v1/health` (`warm:true`). |
| `POST /v1/internal/sync` | `sync(2)` → `guest.synced` |
| `GET /v1/internal/events/stream[?since=RFC3339]` | NDJSON `proto.GuestEvent`. `since` replays buffered events (last 512) newer than it; without it only new events. Types: `setup.status {status,error}`, `repo.setup {url,path,status,error}`, `command.started`/`command.exited` (CommandRecord; not with monitoring off), `desktop.started {port,display}`, `desktop.recording {recording,path}`, `disk.usage {level: 85\|95, percent, usedBytes, totalBytes}` (≤ hourly per level), `boot.previous {reason: clean\|crash, boot}`, `docker.restarted {started: [names], failed: [{container, error}]}`, plus `prompt.event` from the agents package. `repo.setup` also carries `blocking`. |
| `GET /v1/internal/setup-status` | `setup.status {setupStatus: null\|pending\|running\|done\|failed, setupError, startedAt?, finishedAt?, logPath?, blocking, repos?: [{url,path,status: pending\|cloning\|setup\|done\|failed,error?}]}`. `blocking`: a `setupBlocking` repo is not done yet this boot; integrated-agent prompts wait (ENV-032), commands and SSH do not. |

### Desktop (`/v1/internal/desktop/*`)

TigerVNC `Xvnc :0` (VNC on `127.0.0.1:5900`, `-SecurityTypes None`: reachable only through
the tunnel) + an Xfce session (`startxfce4`), as `user`, at `GuestConfig.Desktop`
(default 1920x1080). Programs use `DISPLAY=:0`.

| Endpoint | Body | Answer |
|---|---|---|
| `POST /desktop/start` | – | `desktop.started {port:5900, display:":0", width, height}`; idempotent. No Xvnc → `409 desktop_not_installed`. |
| `GET /desktop/status` | – | `desktop.status {installed, running, port, display, width, height, recording:{...}}` |
| `GET /desktop/screenshot` | – | `image/png` of the whole screen (ffmpeg x11grab, or ImageMagick `import`). Not running → `409 desktop_not_running`. |
| `POST /desktop/input` | `{actions:[...]}` (1-500, run in order) | `desktop.input {performed}`; a bad action → `400 invalid_action {index}` before anything runs; a failed xdotool call → `500 input_failed {index, performed}`. |
| `POST /desktop/recording/start` | `{path?: "demo.mp4" (default recording-<time>.mp4 in home), fps?: 30}` | `desktop.recording_started {path, fps, startedAt}`; one at a time (`409 recording_active`). ffmpeg x11grab → H.264 MP4. |
| `POST /desktop/recording/stop` | – | `desktop.recording_stopped {path, durationSeconds, sizeBytes}` (MP4 finalized); none → `409 not_recording`. |
| `GET /desktop/recording` | – | `desktop.recording {recording, path?, startedAt?, durationSeconds?}` |
| `POST /desktop/wallpaper` | `{content: base64 PNG}` or `{reset:true}` | `desktop.wallpaper {path\|null, applied}`. Limits as Boat: PNG, ≤ 16 MiB, ≤ 8192 px per side, ≤ 16,777,216 px. |

Input actions (`x`,`y` optional unless noted; buttons `left|middle|right`):

```json
{"type":"move","x":10,"y":20}                       // x,y required
{"type":"click","x":10,"y":20,"button":"left","count":1}
{"type":"double_click","x":10,"y":20}
{"type":"mouse_down","button":"left"}   {"type":"mouse_up"}
{"type":"drag","x":10,"y":20,"toX":300,"toY":400}  // all required
{"type":"scroll","direction":"up|down|left|right","amount":3}
{"type":"type","text":"hello","delayMs":12}
{"type":"key","keys":"ctrl+l Return"}              // xdotool key names
{"type":"wait","ms":500}                            // 0-60000
```

Wallpaper files are also watched (every 2 s while the desktop runs), like Boat:
`~/.config/crowdvm/wallpaper.png`, else `~/.config/boat/wallpaper.png`; an invalid or
deleted file restores the default backdrop (`xfconf-query`, style "zoomed").

## Workloads, OOM, monitoring

* **Workload cgroups** (LIFE-043, CMD-001/010): every exec'd command (`cmd_…`), setup script (`setup`), integrated-agent harness (`agents`, via `Agent.SpawnAgent` = `agents.Options.Spawn`) and the browser view runs in its own cgroup under the delegated transient unit `user.slice/crowdvm-workloads.service` (clone3 `CLONE_INTO_CGROUP`). Each boot the guest sets `user.slice` `MemoryMax` = RAM − 512 MiB (platform reserve). The agent runs with `oom_score_adj -1000`; workloads are reset to 0. A command whose cgroup saw an OOM kill reports `oomKilled:true` and exit 137 (`memory.events`). Detached commands live outside `crowdvm-guest.service`, so they survive agent restarts and upgrades. Unprivileged / without cgroup v2 + systemd: plain fork.
* `GET /v1/files` also reads `/proc/stat`, `/proc/meminfo`, `/proc/pressure/io` and `/run/crowdvm/df` (`"<used> <total>"` bytes of `/`, rewritten every 30 s) for the server's OpenTelemetry exporter.
* **Samples** (CMD-025/027): every 30 s VM CPU/RAM/disk/IO + the 12 busiest processes (`proto.GuestSample`, kept in memory until acked, ≤ 24 h), pulled through `history/export`. Disk ≥ 85 % / 95 % → `disk.usage`; at 95 % also `apt-get clean`, journal vacuum to 64 MiB, `docker image prune -af` (≤ hourly) (LIFE-044).
* **Other processes' output** (CMD-021/022, no eBPF/ptrace): the guest follows the journal (`journalctl -f -o json`). Output of systemd services (system and user units), syslog/journal writes and Docker containers using the journald log driver become `process` records: one per process or container, 64 KiB/s with 1 MiB bursts (excess dropped → `outputTruncated`), binary messages skipped, platform services (crowdvm-*, systemd-*, sshd, cron, dbus, desktop session helpers, …) and exec'd commands (their cgroup or session) excluded. Not captured: output written only to a terminal (interactive ssh) or to files. Records end when the process exits (containers: 10 min quiet).
* **Docker json-file logs** (CMD-021 extension): containers using Docker's default `json-file` driver are followed from `/var/lib/docker/containers/<id>/<id>-json.log` (polled every second; logs that exist when the agent starts are followed from their end, new containers from the start; rotation handled) and recorded like journald containers (`docker container <name> (<image>)`, same rate limit).
* **Retention** (CMD-015): finished commands are pruned after 90 days, process records after 72 hours.
* **Monitoring off** (`GuestConfig.NoMonitoring`, also `ZeroDataRetention`; DATA-011, CMD-023): history lives on tmpfs (`/run/crowdvm/history`), the on-disk history is deleted, finished records are dropped after 10 minutes, `command-history*` answers `409 monitoring_off`, no samples, no tracer, no `command.*` events, export is empty. Live output (sync/stream answers, `commands/{pid}`, attach) still works. **ZDR** (DATA-009): `export?flush=1` is treated as a regular export, so output after the last 30 s export is not saved.

## Boot tasks (`proto.GuestConfig`)

`BootInspect` (snapshot inspector VM) runs **none** of this: no env/bashrc writes, secrets,
repos, identity changes, workloads, desktop, sampling or tracing; only the API.

Synchronous, before listening: the `Scrub` (ENV-018), hostname (`/etc/hostname`, `/etc/hosts`,
root), a new `/etc/machine-id` on `create`/`fork` (replaced with one rename: systemd-networkd
reads it for its DHCP client ID at the same moment, and a missing file fails the NIC; then
journald is restarted and the old id's `/var/log/journal/<id>` removed, so `journalctl`
works in forks), the in-sandbox CLI
from the config drive (`crowdvm` next to `config.json` → `/usr/local/bin/crowdvm` + `host`,
`boat`, `box` symlinks; TOOL-007, URL-013..015). Every boot (also unprivileged): env
exported in `/etc/profile.d/crowdvm-env.sh` (root, also `DISPLAY=${DISPLAY:-:0}`) and a
managed block at the **top** of `~/.bashrc`; secret files a previous config wrote and this
one no longer lists are deleted (`managed.json`, ENV-007); `SecretFiles` (default mode
0600); `SSHAuthorizedKeys`; every repo marked `pending`. First boot only: `Files` (0644);
setup status `pending`.

In the background right after (root): SSH host keys regenerated on create, fork **and
resume** (SSH-005; `/v1/sshkey` and tunnels to port 22 wait for them, ≤ 30 s), `growpart` +
`resize2fs`, firewall reset (URL-019/022: images with `/usr/local/lib/crowdvm/firewall-reset`
do it from a unit; the agent only adds its port if not 7070; older images with `ufw` get
`ufw --force reset`, deny incoming, allow 22/tcp + the agent port, enable). So health
answers before them.

In the background after listening: `docker logout` of registries a previous config logged
in and this one dropped, `docker login` per `RegistryLogins` (root, as
`user`), repos (cloned on create and fork when missing, as `user`, https tokens via a git credential store
file in `~/.config/crowdvm/git-credentials`; each repo setup script every boot in the repo
folder with `set -e`, failures logged to `/tmp/routine-logs/<repo>.log`), then the
one-time `SetupScript` as `user` (`pending → running → done|failed`, `setupError` =
exit code + end of stderr). All clones run first, then the setup scripts (Boat: repositories
are cloned at ready, setup scripts start at ready); health reports `reposCloning` until the
clone phase ends (closed in `Boot` when nothing is to be cloned). `boot.json` keeps all of this from re-running when the agent
restarts within the same boot.

**Credential files** (`creds.go`, rewritten with the env at boot and apply-config):
`~/.config/crowdvm/config.json` profile `default` = `{server: CROWDVM_API_URL without /api/v1,
apiKey: CROWDVM_API_KEY}` (KEY-017; the key is removed when withheld; `~/.config/ascii/boat` →
`../crowdvm` when free), `~/.claude/.credentials.json` `claudeAiOauth = {accessToken:
CLAUDE_CODE_OAUTH_TOKEN, refreshToken: null, expiresAt: null, scopes: [user:inference]}`
(ENV-016; removed when the token goes). Values the guest wrote are tracked by sha256 in
`<data dir>/managed-creds.json`; a value the user replaced (own login) is never touched. After a
Claude `/login` inside the sandbox (the file holds a `refreshToken`) the env files' guard line
unsets `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN` and the proxy
placeholder `ANTHROPIC_API_KEY` in new shells (AIC-011); integrated-agent turns keep them.

**Startup playbook** (`playbook.go`, LIFE-018): on a boot from a snapshot (the disk has an
earlier `boot.json`, boot kind not recover/inspect; root only) the guest prefetches
`GuestConfig.Playbook` + `~/.crowdvm/playbook.json` (4 readers, ≤ 2 GiB, files ≤ 256 MiB) and
records, with fanotify `FAN_OPEN` on the root filesystem, the files other processes open, in
first-open order, for 2 min or until `playbook/finish`. Skipped: `/proc`, `/sys`, `/dev`, `/run`,
`/tmp`, `/var/tmp`, `/var/lib/crowdvm`, the journal, `~/.cache`. The run is merged into
`~/.crowdvm/playbook.json` (`{version, updatedAt, paths}`; this run first, then older paths;
≤ 20 000) and the event `playbook.recorded {path, paths, recorded}` is emitted.

**Docker and not-saved paths across boots** (`docker.go`, Boat snapshots/whats-saved):
on a fresh boot that is not an inspector, the containers in `docker-running.json` that do
not run are `docker start`ed once dockerd answers (≤ 3 min wait), whatever their restart
policy; removed ones are skipped. When the disk comes from a live snapshot (an earlier
boot exists and its clean-stop marker does not match: fork, saved snapshot or deploy of a
running sandbox, resume after a crash), the stop's cleanup runs at boot: `~/.cache` is
renamed into the data dir and replaced by an empty folder before the API listens (deleted
in the background), ignore-file matches are deleted, and after dockerd is up the build
cache and unused images are pruned. `/tmp` is emptied by systemd-tmpfiles.

**Skills** (`skills.go`, AGT-025, DOCS-003): every boot writes the skills of the repo's
`skills/` folder (`crowdvm`) into `~/.claude/skills`, `~/.codex/skills`, `~/.pi/agent/skills`,
`~/.config/opencode/skills`, `~/.prime/agent/skills`, `~/.kimi-code/skills`, `~/.vibe/skills`,
`~/.agents/skills`, `~/.hermes/skills`, `~/.openclaw/skills` and `~/.cursor/skills`, owned by
`user`. Files are tracked by sha256 in `<data dir>/managed-skills.json`: a copy the user
edited or deleted is left alone, files a newer guest no longer ships are removed when
unchanged.

**Chrome first run** (`chrome.go`): Chrome 15x shows an "Additional Terms of Service"
dialog on every new profile. v3 images ship `/opt/google/chrome/initial_preferences` with
`require_eula: false`; on older images the guest writes the same file at boot (root,
only when Chrome is installed and the file is missing).

The sandbox env given to commands, agents and shells = `GuestConfig.Env` + the AI proxy
variables from `agents.(*Manager).ProxyEnv()` + `CROWDVM_SANDBOX_ID`. Exec'd commands also
get `DISPLAY=:0` (DESK-020) and `LANG`/`LC_ALL` from `/etc/default/locale` (TOOL-010).
`apply-config` changes it at run time (read the config through `Agent.config()`).

## Image requirements

`bash`, coreutils, `git`, `openssh-server` (`ssh-keygen`), `cloud-guest-utils`
(`growpart`), `e2fsprogs`, `util-linux` (`findmnt`, `fstrim`); desktop: `tigervnc-standalone-server`,
`xfce4`, `dbus-x11`, `xdotool`, `ffmpeg`, `xfconf`. Root mode
needs cgroup v2 + systemd (`systemd-run`, `systemctl`) for workload cgroups, `journalctl` for
the process tracer, `util-linux` `unshare`/`setpriv`/`mount` and a `crowdvm-browser` user for
the isolated browser view. Detached commands survive an agent restart without
`KillMode=process` (they are not in the service's cgroup); the agent re-adopts them by
pid + start time.
