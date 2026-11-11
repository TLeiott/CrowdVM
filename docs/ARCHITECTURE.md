# CrowdVM architecture

CrowdVM is a crowdsourced, self-hosted take on Boat (boat.dev): persistent Ubuntu
VMs for coding agents, with the same API surface (`/api/v1`, mirroring Boat's
v1 API) and a `crowdvm` CLI that mirrors `boat`. The VMs don't run in a cloud.
They run on **nodes**: ordinary Linux/Windows machines on a LAN or Tailnet
whose owners installed the CrowdVM node app. That app benchmarks the machine
and lends some of its compute to the pool.

Scope for now: LAN + Tailscale. No public internet, no TLS termination
(an optional `--tls-cert/--tls-key` on the server is enough), no payments.

Reference material (git-ignored, local only): `.ref/boat-v1.yaml` (Boat OpenAPI),
`.ref/pages/*.md` (every Boat docs page, one file per page), `.ref/llms-full.txt`.

## Components

```
 crowdvm CLI / SDKs / dashboard / agents
            │  HTTP /api/v1 (Bearer cvm_...)   websocket tunnels (ssh, ports, desktop)
            ▼
     crowdvm-server  (coordinator: accounts, keys, orgs, sandboxes, scheduler,
            │         snapshots store, environments, webhooks, OTel, AI proxy,
            │         public URLs, desktop, dashboard UI)
            │  one yamux session per node, over a websocket the NODE dials
            ▼
     crowdvm-node    (the "app" on each contributing machine: benchmark, capacity
            │         limits, VM driver, snapshot cache, local status UI)
            │  HTTP to 127.0.0.1:<fwd> → node netstack → guest :7070  (Bearer per-VM token)
            ▼
     crowdvm-guest   (agent inside every VM: commands, files, processes, history,
                      tunnels, desktop session, integrated coding agents, setup)
```

| Binary | Package(s) | Runs on |
|---|---|---|
| `crowdvm` | `cmd/crowdvm`, `internal/client`, `internal/cli` | user / agent machine |
| `crowdvm-server` | `cmd/crowdvm-server`, `internal/server/...`, `web/` | one machine on the LAN |
| `crowdvm-node` | `cmd/crowdvm-node`, `internal/node/...` | each contributing machine (Linux, Windows) |
| `crowdvm-guest` | `cmd/crowdvm-guest`, `internal/guest/...` | inside each VM (linux/amd64, static) |

`crowdvm-node install-service` runs the node in the background: a systemd user
unit on Linux; on Windows a real SCM service (`crowdvm-node`, automatic start,
LocalSystem, no logged-in user needed, restart on failure, logs to
`<data dir>\node.log`; needs an elevated prompt) or, with `--logon-task`, the
older per-user logon scheduled task.

## Contracts

* `internal/proto` — server⇄node and node⇄guest wire types. **The contract.**
* `internal/api` — public `/api/v1` envelope, errors, sizes, ids. Public JSON
  shapes follow `.ref/boat-v1.yaml` field-for-field where it makes sense.
* `internal/node/vm` — VM driver interface. Optional driver interfaces:
  `Dialer`, `NICDialer` (direct ports), `LazyStarter` (lazy chains),
  `Renamer` (warm pool), `Waiter`, `Notifier`, `ChangeTracker`.

### Public API → guest pass-through

Sandbox-scoped endpoints that act *inside* the machine are implemented by the
guest agent with Boat-compatible request/response bodies, and the server only
authenticates, checks state (waits up to 60 s while starting, else 409
`machine_not_running` / `sandbox_starting`), then reverse-proxies:

```
/api/v1/sandboxes/{id}/<rest>   (server, auth + state)
  → node  /v1/vms/{id}/guest/<rest>   (over yamux)
  → guest /v1/<rest>                  (Bearer guest token)
```

`<rest>` ∈ `commands…`, `command-history…`, `processes`, `files…` (except
`files/upload-url`, `files/download-url` which the server signs), `artifacts`,
`prompt`, `prompts/…`, `events`, `conversations`, `steer`, `interrupt`, `sshkey`.
Streaming responses (NDJSON) are flushed through every hop.

Guest-only internal endpoints (node uses them; not exposed publicly):
`GET /v1/health`, `GET /v1/tunnel?port=N` (HTTP Upgrade `crowdvm-tunnel`, raw
TCP to 127.0.0.1:N in the guest), `POST /v1/internal/prepare-stop`
(`proto.PrepareStopRequest{force,freeze,freezeTimeoutSeconds}`: pre-stop hook,
then record the running containers and `docker stop` them unless force, clean
unsaved paths, fstrim, sync, then fsfreeze when asked), `POST /v1/internal/freeze`
(`proto.FreezeRequest`; record the running containers, sync + fsfreeze for a live
snapshot, auto-thaw) and `POST /v1/internal/thaw` (after a failed stop it also
starts the containers prepare-stop stopped), `PUT /v1/internal/firewall`
(`{open:["8000/tcp"]}`: ufw rules for published direct ports, URL-012),
`POST /v1/internal/sync` (fsfreeze-free sync, fallback for older guests and
the periodic change check), `POST /v1/internal/desktop/start` (ensure the VNC
desktop session runs; returns its port), `GET /v1/internal/events/stream`
(NDJSON `proto.GuestEvent`), `GET /v1/internal/setup-status` (incl. `blocking`:
a `setupBlocking` repo is not done, so agent prompts wait), `GET /v1/internal/scrub`
(`GuestConfig.Scrub` outcome `{id,done,error}`; the scrub runs in Boot before
the API listens), `POST /v1/internal/apply-config` (`proto.ApplyConfig`: live
environment upgrade of env, secret files (stale ones deleted), repos, registry
logins, AI proxy), `GET /v1/internal/history/export` + `POST
/v1/internal/history/ack` (pull of command history, output chunks and 30 s
CPU/RAM/process samples as `proto.HistoryExport`; the server polls ~2 s and
flushes before a non-forced stop), `POST /v1/internal/browser/start` (Xvnc :2 +
Chrome as `crowdvm-browser` in a mount namespace without `/home/user`; port
5902), `POST /v1/internal/playbook/finish` (end this boot's startup playbook
recording, LIFE-018; the node calls it when a lazy chain is hydrated),
`POST /v1/internal/adopt` (warm pool VMs only, SIZE-012). `/v1/health` also
reports `version` (→ `VMStatus.GuestVersion`, CMD-012) and `reposCloning`
(LIFE-009). Details: `internal/guest/README.md`.

Server side of these contracts (`internal/server/lifecycle_*.go`, `feat_history.go`):

* **Securing before ready.** A pending scrub (`extra.pendingScrub`: no-env,
  deploy of another account's or org member's saved snapshot, secrets a newer
  environment version withholds) ships as `GuestConfig.Scrub`. After the guest
  answers, the sandbox stays `provisioned` with `securing:true` until the
  server confirms `GET internal/scrub` (or, for guests without it, runs the same
  scrub through the commands API). Then it becomes `ready`. Every call into the
  sandbox answers `409 sandbox_securing` until then. Three failures put the
  sandbox in `error` (`scrub_failed`).
* **Stored reads.** When the sandbox does not run, `GET command-history[/{id}[/logs]]`,
  `processes` (`live:false`, last sample), `events`, `conversations` and
  `prompts/{id}` are served from the server's copy. History comes from the
  export pull. Agent events are the `prompt.event` guest events.
* **Waiting boots.** Without `failFast`, create, resume and fork wait up to
  `CapacityWait` (15 min) for a node with room (`provisioning`/`cloning`, no
  node, `waitingForCapacity:true`). A resume during `archiving` cancels the stop
  if the machine still runs. Otherwise it boots again on the same node right
  after the stop. A sandbox whose node is offline longer than `NodeLostGrace`
  (3 min) is rebuilt on another node from its latest uploaded snapshot. Error
  states and failing stops are force-stopped after 3 days, with a warning one
  day before. Owner notices go to `GET /notices` (plus mail).
* **Refused stops and billing pauses** (`lifecycle_pause.go`). A non-forced
  stop of an answering guest first takes a live snapshot (pre-flight save);
  if it fails, nothing in the guest has changed: the sandbox goes back to
  `ready`, the stop stays `failing` (retried with backoff, forced after 3
  days) and the meter pauses (`extra.billingPaused = stop_refused`, shown as
  `billingPaused`). A guest that stops answering (`guest_unreachable`) pauses
  the meter too (`not_answering`) until it answers. `openRun` never meters a
  paused sandbox.
* **Deletion operations** (`deletion_ops.go`) complete only when the node
  confirmed the VM is removed and every snapshot copy (server blob, node
  caches) is purged or still needed; offline nodes keep them `retrying`.
* **Docs for agents** (`feat_docs.go`): `/llms.txt`, `/llms-full.txt`,
  `/docs/<page>.md` (docs/guide embedded via the `docs` package; the CLI
  reference is generated by `go generate ./docs`) and a docs MCP server at
  `/mcp`.
* `VMSpec.NoSnapshots` for `snapshots:false`, and the server drops any snapshot
  uploaded for such a sandbox. The heartbeat pushes a 60 s snapshot interval by
  default. Superseded periodic layers (`SnapshotRef.Supersedes`) are deleted
  and garbage-collected.
* Every sandbox gets a neutral `ASCII_TOKEN`/`CROWDVM_TOKEN` (key preset
  `sandbox-token`: `sandbox.read` + `host` on itself only), even when
  `CROWDVM_API_KEY` is withheld.

Guest workloads (commands, setup scripts, agent harnesses, browser view) run in
per-command cgroups under the delegated transient unit
`user.slice/crowdvm-workloads.service`; the guest caps `user.slice` at RAM −
512 MiB every boot (platform reserve, `oomKilled`), runs itself at
`oom_score_adj -1000` and resets workloads to 0.

### Server ⇄ node

1. `crowdvm-node join --server http://host:8420 --token <join token>` →
   `POST /node/v1/register` → node id + node token (saved in node config).
2. Node dials `GET /node/v1/connect` (websocket, `Authorization: Bearer <node token>`),
   wraps it as a `net.Conn` and runs a yamux session (node = yamux client).
3. **Both** sides serve plain HTTP/1.1 on streams they accept:
   * server → node: `GET /v1/info`, `POST /v1/vms`, `GET /v1/vms/{id}`,
     `POST /v1/vms/{id}/stop|snapshot|restart`, `DELETE /v1/vms/{id}`,
     `ANY /v1/vms/{id}/guest/{rest}`, `GET /v1/vms/{id}/tunnel?port=N` (Upgrade),
     `PUT /v1/vms/{id}/direct-ports` (`proto.DirectPortsRequest`, URL-012),
     `POST /v1/images/{id}/fetch`, `POST /v1/snapshots/{id}/fetch` (pull from server),
     `DELETE /v1/snapshots/{id}`.
   * node → server: `POST /node/v1/heartbeat`, `POST /node/v1/events`,
     `GET|PUT /node/v1/blobs/{image|snapshot}/{id}` (+ `GET /node/v1/snapshots/{id}` meta),
     `POST /node/v1/snapshots` (a periodic layer, reported when taken and before its
     upload: the server records it at once with its parent and the node as location,
     not stored yet, so stops, saves and forks that build on it see a whole chain),
     `GET /node/v1/bench/download?bytes=N`, `POST /node/v1/bench/upload` (network benchmark).
4. Node disconnect → server marks its VMs `error` (webhooks `degraded` and
   `error`, reason `node_offline`), and `ready` + `recovered` when they come back.
5. Guest health (node): while a VM runs the node probes guest `/v1/health`
   every 5 s (5 s timeout). Three misses in a row → `vm.state {state:running,
   guestReady:false}` (server: `degraded`, `recover` allowed); the next answer →
   `guestReady:true` (`recovered`). `GET /v1/vms/{id}` probes live when the
   monitor has not confirmed the guest in the last 6 s. Periodic snapshots
   skip a guest that does not answer. Each probe records the guest's
   `version` as `VMStatus.GuestVersion` (CMD-012: the server answers
   `409 agent_outdated` below its `--min-guest-version`).
   **Clone gate (LIFE-009):** the first time a VM's guest answers, the node
   reports `running` with `guestReady:false` (server: `provisioned`) until
   the guest's health says `reposCloning:false` (the environment's
   repositories of this boot are cloned; their setup scripts start after),
   at most 10 min. Later reboots, recovers and adopted VMs are not gated.
6. Capacity (node): `Capacity.DiskGiB` is **virtual** (thin provisioning):
   the real disk shared (`disk`, default 90% of free + already stored) ×
   `disk-overcommit` (default 4). The node also refuses VMs, and advertises no
   free disk, once real headroom (budget − stored, free − `disk-reserve`
   [10 GiB]) drops under 2 GiB; downloads that would eat into the reserve fail.

### Disks, snapshots, forks

* Base image (`ubuntu-24.04-crowdvm-<ver>`) = Ubuntu 24.04 cloud image +
  provisioning (`images/`): Docker, desktop (Xfce + TigerVNC), Chrome, VS Code,
  Ghostty (if packaged) or a fallback terminal, gh, Rust, Node.js, Bun, Python,
  Claude Code, Codex, OpenCode, the `crowdvm-guest.service` bootstrap unit.
  Built by `crowdvm-node image build`, pushed to the server, pulled by other nodes.
  Current: v3 (v2 kept). Things that must also reach existing images live in
  the guest instead (it ships with every boot): the `crowdvm` skill in every
  harness's skills dir, Chrome's `initial_preferences` (no first-run Terms
  dialog) when the image lacks it.
* Every VM runs on a qcow2 overlay. **Stop** (Boat order) = guest prepare-stop
  (pre-stop hook `~/.crowdvm/pre-stop`|`~/.boat/pre-stop`, `docker stop`,
  cleanup, sync, fsfreeze) → the node forwards the guest's last events (drains
  the event stream until idle) → QMP `stop` + `quit`: **no OS shutdown**, other
  programs get no signal → overlay becomes immutable snapshot → uploaded to the
  server (`hydrated`). `force` skips the hook and Docker (≤15 s). A guest that
  is degraded or still booting gets a forced prepare-stop when it answers a
  2 s health probe (clean, sync, freeze still run); one that does not answer
  is halted without prepare-stop. `VMSpec.NoSnapshots`
  (snapshots off): no periodic snapshots, and a stop without a snapshot id
  discards the disk.
  **Resume** = new overlay on the latest snapshot (any node; chain is pulled if
  missing). **Fork** = live snapshot of the source (QMP
  `blockdev-snapshot-sync`), new overlay for the fork on the same node, so
  forking a running sandbox is near-instant. Live snapshots freeze the guest
  filesystems only for the overlay switch (freeze → `blockdev-snapshot-sync` →
  thaw, ~80 ms). Periodic live snapshots every **60 s** by default (server
  `--snapshot-interval`, node `snapshot-interval`), skipped when the guest wrote
  nothing since the last one (QMP `query-blockstats`); each uploads only its new
  layer. **Chain compaction** (tiered, fanout 4): a new snapshot absorbs a run of
  3 same-level layers at the top of the chain (QMP `block-stream` live,
  `qemu-img rebase` at stop) and lists them in `SnapshotRef.Supersedes`; depth
  stays ≈15 after a day of minutes and a byte is re-uploaded ≤5×. The node
  drops superseded layers from its cache once the merged one is uploaded; the
  server may delete superseded periodic snapshots nothing else needs. Hard cap
  32 layers (then flattened onto the image).
* **Lazy chain fetch (LIFE-013).** On nodes with `NodeInfo.LazyChain` (qemu
  with the `http` and `copy-on-read` block drivers; `crowdvm-node config set
  lazy-chain false` turns it off) the server does not pre-fetch a chain: the
  node needs only the base image and boots at once on an explicit
  `-blockdev` graph (`internal/node/vm/qemu/lazy.go`): missing layers are
  qcow2 nodes over QEMU's http driver reading the node's loopback blob proxy
  (Range requests forwarded to `GET /node/v1/blobs/snapshot/{id}`; port kept
  in `<data dir>/lazy.json`, HMAC token per blob), under a copy-on-read
  filter that keeps what was read in `vms/<id>/lazy-cache.qcow2` (each block
  crosses the network once; the overlay still holds only guest writes). The
  node downloads the layers in the background (`node.transfer`), then the
  driver swaps each http node for the local file (`blockdev-reopen`) and
  `VMStatus.Hydrating` turns false (server: `hydrated`, `sandbox.hydrated`).
  Live compaction waits until the next boot; snapshots taken meanwhile build
  on the remote layers' future store paths. Downloads stay sha256-verified;
  on-demand reads are not (a mismatch fails the download, the VM keeps
  reading from the server).
* **Startup playbook (LIFE-018).** A guest booting from a snapshot (resume,
  fork, deploy) records the files opened in the first 2 min (or until
  `playbook/finish`) with fanotify into `~/.crowdvm/playbook.json` (this run
  first, ≤20 000 paths) and reads `GuestConfig.Playbook` + that list first
  at the next such boot (prefetch through the lazy chain / page cache).
  Event `playbook.recorded`; the server keeps a deploy's list with its saved
  snapshot.
* **Warm pool (SIZE-012, optional).** `crowdvm-node config set warm-pool N`
  keeps N VMs booted (`GuestConfig.Boot: warm`, ids `warm_…`) with the image,
  size and nested flag of the last plain create (`<data dir>/warm.json`).
  A create of that shape adopts one: guest `POST /v1/internal/adopt` with
  the sandbox's GuestConfig (the guest saves it as `adopted.json` and boots
  as that sandbox: hostname, machine-id, SSH host keys, env, repos), then
  the driver renames the VM (`vm.Renamer`). Pool VMs count against the
  node's capacity, are never reported to the server, are evicted first
  when a create needs room, and are destroyed when the node restarts.
* Disk size: the overlay's virtual size is the size's `diskGiB` **plus the
  base image's data** (OS excluded, Boat), never smaller than the parent's
  (filesystems are not shrunk; a smaller size on resume keeps the larger disk).
* Not saved (Boat semantics): `/tmp`, `~/.cache`, journal, docker build cache,
  Docker images no container and no Compose project (with a container) uses,
  `.crowdvmignore` matches — deleted by prepare-stop, then `fstrim`. A disk
  that comes from a live snapshot (fork, saved snapshot or deploy of a running
  sandbox, resume after a crash; the guest knows by its clean-stop marker) gets
  the same cleanup at its first boot: `~/.cache` swapped out before the API
  listens, ignore matches, build cache and unused images after dockerd is up.
* Docker (Boat snapshots/whats-saved#docker): the guest records the running
  containers in `/var/lib/crowdvm/docker-running.json` before every stop and
  every live snapshot, and starts the recorded ones again at the next boot from
  that disk (resume, fork, deploy, recover), whatever their restart policy
  (`docker stop` would otherwise leave `unless-stopped` and no-policy ones
  down). Named volumes stay on the disk. Event `docker.restarted`.
* Snapshot file access (`tree`, `files`, `download`) uses a throwaway inspector
  VM (BootInspect) started from the snapshot on a node that has it.

### Guest bootstrap (contract between image builder, VM driver and guest agent)

* Each boot the VM driver attaches a small read-only **config drive** labelled
  `CROWDVM` (ISO9660 or FAT, written without host tools so Windows nodes work)
  containing `config.json` (`proto.GuestConfig`), `crowdvm-guest` (linux/amd64
  static binary) and, when the node has one (`crowdvm-node --cli-binary`,
  default `crowdvm-linux-amd64` or on linux/amd64 `crowdvm` next to the node
  binary), `crowdvm`: the static in-sandbox CLI the guest installs as
  `/usr/local/bin/crowdvm` with `host`/`boat`/`box` links.
* The base image ships `/etc/systemd/system/crowdvm-guest.service` (enabled):
  mount `LABEL=CROWDVM` read-only at `/run/crowdvm/cfg`, install
  `crowdvm-guest` to `/usr/local/bin/`, run
  `/usr/local/bin/crowdvm-guest --config /run/crowdvm/cfg/config.json` as root,
  `Restart=always`. A new guest binary therefore ships with every boot.
* cloud-init is disabled in the final image. The guest agent handles per-boot
  identity itself: hostname, SSH host keys (regenerated on create, fork and
  resume, in the background: health answers first), growpart/resize2fs to the
  disk size, `/etc/machine-id` on create/fork (journald restarted), a firewall
  reset (ufw defaults; the image's `crowdvm-firewall-reset.service` does it on
  new images). A `BootInspect` guest does none of this and writes no env,
  secrets or services.
* Network: netplan DHCP on all `en*` interfaces (guest 10.0.2.15/24, gateway
  10.0.2.2, DNS 10.0.2.3, MTU 65520; see Networking). The guest agent listens
  on `:7070`, which the driver forwards to `127.0.0.1:<random>` on the node.
* User `user` (uid 1000, home `/home/user`, passwordless sudo, in `docker` group).
* Credential files the guest derives from the env at every boot and env
  change (`internal/guest/creds.go`): `~/.config/crowdvm/config.json`
  profile `default` = `CROWDVM_API_URL` (without `/api/v1`) +
  `CROWDVM_API_KEY` (KEY-017; removed when the key is withheld; legacy link
  `~/.config/ascii/boat`), `~/.claude/.credentials.json` `claudeAiOauth` from
  `CLAUDE_CODE_OAUTH_TOKEN` (ENV-016; no refresh token, no expiry). A value
  the user replaced (own `crowdvm login`, Claude `/login` with a refresh
  token) is never overwritten or removed; after such a Claude sign-in new
  shells drop `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_BASE_URL`,
  `ANTHROPIC_AUTH_TOKEN` and the proxy's placeholder `ANTHROPIC_API_KEY`
  (AIC-011; integrated-agent turns keep the profile).
* `crowdvm-guest` also runs **unprivileged on the host** for tests (process
  driver): `--config FILE --home DIR --unprivileged` (no hostname/disk/user
  changes, commands run as the current user, home = DIR).

### Networking (LAN / Tailscale)

* Each VM's NIC is a QEMU `-netdev stream` unix socket served by a private
  userspace network stack in the node process (`internal/node/vm/netstack`,
  gVisor netstack): DHCP, DNS relay to the host's resolvers, and TCP/UDP/ICMP-echo
  proxied through ordinary host sockets after an **egress policy** check.
  Default: internet allowed; this machine (loopback, own addresses), RFC 1918,
  CGNAT/Tailscale `100.64/10`, link-local (incl. `169.254.169.254`), multicast
  and IPv6 ULA/link-local blocked. The CrowdVM server's host:port (node config
  `server`) is always allowed and also mapped to `10.0.2.2:<server port>`, which
  is otherwise closed (unlike slirp, it is not the host's loopback). Owner
  overrides: `crowdvm-node config set net.allow-lan true`, `net.allow` /
  `net.deny` (CIDR lists; longest prefix wins; own addresses only open with an
  exact /32). Per-sandbox rules: `proto.VMSpec.Net` (can only narrow).
  `--net slirp` (QEMU user networking) remains as an insecure fallback. No IPv6
  in guests yet.
* The guest agent port is forwarded to the node's loopback only. Everything else
  (ssh, exposed ports, VNC) goes through guest `/v1/tunnel`.
* `crowdvm ssh` = `ssh -o ProxyCommand="crowdvm tunnel %h 22"`; `crowdvm scp`
  likewise. Public URLs: the server reverse-proxies
  `http://<sandboxId>-<port>.<public-domain>` (host routing, default domain
  `<server-ip>.nip.io` or `localhost`) and `http://server/p/<sandboxId>/<port>/`.
* Optional **direct ports**: a node with `direct-host` publishes per-sandbox
  ssh (agent tunnel to port 22, `Direct["ssh"]`) and the user ports the
  server asks for (`VMSpec.DirectPorts`, `PUT /v1/vms/{id}/direct-ports`,
  URL-012) on its LAN/Tailscale IP, the guest port number when free there:
  `Direct["tcp/3000"]`, `Direct["udp/5353"]`. User ports enter the guest's
  NIC from the gateway through the node's netstack (`vm.NICDialer`), so the
  guest firewall decides, like Boat's sandbox IP. The node opens the TCP
  ones itself (Boat: "`host --public` also opens that TCP port ... again
  after a resume"): guest `PUT /v1/internal/firewall` with the TCP
  DirectPorts whenever the set changes and whenever the guest's event stream
  (re)connects (each boot resets ufw). UDP ports stay the user's own
  `sudo ufw allow N/udp`, as in Boat's raw-port recipe. UDP is relayed per
  client address (2 min idle). Without a NIC (process
  driver, slirp) TCP falls back to the agent tunnel and UDP is unavailable.
* SSH without `direct-host` (SSH-004): `sshkey` answers `machineIp` /
  `sshEndpoint` null; stock OpenSSH and VS Code connect with the tunnel form
  `ssh -o ProxyCommand="crowdvm tunnel %h 22" user@<sandboxId>` (what
  `crowdvm ssh --config` writes). A coordinator-side TCP port mapping is not
  built.

### Accounts

Local accounts (username + password; email code printed to the server log or
sent via optional SMTP), browser sessions for the dashboard, device-code login for
the CLI (`crowdvm login` → approve in browser) and an agent self-registration
flow. API keys (`cvm_…`) with scopes, presets, expiry, rotation, usage, and
per-sandbox keys. Organizations with members, shared sandboxes, default
visibility, active org. "Billing" becomes **credits**: running sandboxes spend
credits at the size multiplier, nodes earn credits for hosted VM time, and
admins can grant credits. Enforcement is optional and off by default.

* Org API keys get `403 api_key_org_forbidden` on account settings (webhooks,
  telemetry, AI credentials, credits/billing, keys, members, notices). Routes
  registered with `ActionAny` refuse API keys unless `acct_guard.go` names
  the scope they need (default deny). `boat_…`/`box_…` in front of a key's
  body are accepted. Service accounts (`kind: service`) act only through org
  keys. Admin-set **wallet policies** (`/admin/policies/{wallet}`): monthly
  credit quota, concurrency/start limits, max TTL, allowed sizes,
  saved-snapshot allowance, monthly credit cap, low-credit alert. With
  enforcement on, an empty wallet gets a 24 h grace (notice + mail) before
  its sandboxes stop; caps (org member, wallet) apply always. Extra starts
  beyond the start limit cost credits when turned on. Notices:
  `GET /notices` (wallet and sandbox notices, one table).
* The served OpenAPI document (`/openapi/crowdvm-v1.yaml`,
  `/api/v1/openapi.{yaml,json}`) is generated from the route table at
  runtime; every `s.API`/`s.Route` registration appears in it.

### Guest-facing contract of the accounts layer (for guest / guest-agents)

* `GuestConfig.NoMonitoring` is also set when the owner's account has
  monitoring off or zero data retention; `GuestConfig.ZeroDataRetention`
  (account ZDR) additionally means: keep no prompt transcripts/events on
  disk after a turn ends and write no command output logs.
* `POST /prompt` reaches the guest with `provider`/`model`/`fast` already
  filled from the account's defaults when the caller omitted them, and only
  when the sandbox has a credential for the harness (else the server answers
  `409 provider_not_configured`). The server waits up to 10 min while the
  sandbox starts. Holding prompts while a `Repo.SetupBlocking` setup runs is
  the guest's job (queue the prompt, answer `202 prompt.queued` at once).
* Subscription sign-ins the server renews (Claude, ChatGPT; others when
  configured) reach the sandbox **without** refresh tokens:
  `~/.codex/auth.json` has no `tokens.refresh_token`, and
  `KIMI_CODE_REFRESH_TOKEN` is only set when the server cannot renew Kimi.
  Harnesses must not try to renew those; the server renews before every
  start and prompt and pushes the new token into running sandboxes.
* DeepSeek (a DeepSeek key and no credential of the harness's own vendor):
  Claude Code gets `ANTHROPIC_BASE_URL=<deepseek>/anthropic`,
  `ANTHROPIC_AUTH_TOKEN=<key>` and `ANTHROPIC_API_KEY=""` (empty on
  purpose: DeepSeek rejects `x-api-key`; config `Env` must win over the
  proxy placeholders in shells too). Codex gets `OPENAI_BASE_URL=<deepseek>/v1`,
  `DEEPSEEK_BASE_URL=<deepseek>` and `DEEPSEEK_API_KEY`; the guest writes a
  Codex `model_providers.deepseek` block (`base_url = DEEPSEEK_BASE_URL`,
  `env_key = "DEEPSEEK_API_KEY"`, `wire_api = "responses"`, web search off)
  when `OPENAI_BASE_URL` points at DeepSeek. `<deepseek>` is
  `https://api.deepseek.com` or the AI proxy route `…/ai-proxy/deepseek`.
* Claude Code on Bedrock without an Anthropic credential gets
  `ANTHROPIC_MODEL=us.anthropic.claude-sonnet-4-6` (default model).

### GPUs (node, qemu driver)

* **Passthrough (VFIO), what `--gpu` / `placement.gpu` gives.** The owner
  lists GPUs to lend: `crowdvm-node config set gpu.passthrough
  0000:0b:00.0[,…]`. At start the node checks each (`internal/node/gpu.go`):
  Linux, a display controller, IOMMU on, every non-bridge device of its
  IOMMU group bound to `vfio-pci`, one listed GPU per group, `/dev/vfio/<group>`
  and `/dev/vfio/vfio` read/write for the node's user, and a memlock limit
  that covers the shared memory (VFIO pins a GPU VM's RAM). Any failure
  **stops the node from starting** with the device and the fix in the error;
  nothing half-works. Only checked GPUs are advertised:
  `proto.NodeInfo.GPUs []PassthroughGPU{address, vendor, model, iommuGroup,
  functions, inUse}`; `Usage.GPUs` counts the ones in use. A VM with
  `VMSpec.GPU` gets a free one for itself (one VM per GPU, else 503
  `out_of_capacity` "no free GPU"); the node fills `VMSpec.GPUDevices` (the
  group's functions) and the driver attaches them behind a PCIe root port
  (`vfio-pci,host=…,multifunction=on`), without the balloon, KVM only.
  `VMStatus.GPU` is the GPU's address. GPU specs never use the warm pool.
  `Bench.GPUs` still lists every detected GPU (benchmark, `gpu` category).
* **virgl (separate, optional).** `crowdvm-node config set gpu.virgl on`
  (or a `/dev/dri/renderD*` path): every VM gets `virtio-gpu-gl-pci` with
  `-display egl-headless,rendernode=…` (OpenGL rendered on the host GPU:
  `/dev/dri/renderD128` with `+virgl` in the guest; no CUDA). The driver
  checks the render node and QEMU support when it starts. Advertised as
  `NodeInfo.VirGL`; not what placement `gpu` asks for.
* Verified here: virgl on a real VM (RTX 3080 render node); passthrough only
  by unit tests (fake sysfs, QEMU args, allocation) and the start check on
  this machine's GPU (refused: still bound to `nvidia`). A VFIO run needs a
  second GPU and root to bind it.

### Hand-offs to the server and CLI layers (guest/node agent, 2026-11-08)

* **Server, scheduler (`placement.gpu`):** place only on nodes with a free
  passthrough GPU: `len(node.Info.GPUs) > node.Usage.GPUs` (today it checks
  `Bench.GPUs`, which places on nodes that cannot give the VM a GPU), and
  set `VMSpec.GPU = true` for such sandboxes on create, resume and fork. A
  503 `out_of_capacity` from the node means "wait for capacity", not error.
  Show `gpus`/`virgl` in `crowdvm nodes`. `--gpu` help: "a GPU passed
  through to the sandbox (VFIO)".
* **Server:** `NetworkBench.Internet` (`reachable|unreachable`) and
  `internetLatencyMs` are new benchmark fields (the node's own internet,
  which its sandboxes use); usable for placement or display.
* **Server:** steer `reason: not_running` is the server's (the guest is by
  definition running); the guest now answers 409 `no_running_turn` with
  `reason: delivery_failed` when a harness refuses a steer, and 400
  `prompt_required` for an empty prompt.
* **CLI, `crowdvm skill install`:** the skills ship in the repo under
  `skills/<name>/` (`skills/crowdvm/SKILL.md` + `references/`), embedded as
  Go package `github.com/crowdvm/crowdvm/skills` (`skills.Names()`,
  `skills.Files()` → `"crowdvm/SKILL.md"` → bytes). Inside sandboxes the
  guest installs them into every harness's skills dir
  (`internal/guest/skills.go`: `~/.claude/skills`, `~/.codex/skills`,
  `~/.pi/agent/skills`, `~/.config/opencode/skills`, `~/.prime/agent/skills`,
  `~/.kimi-code/skills`, `~/.vibe/skills`, `~/.agents/skills`,
  `~/.hermes/skills`, `~/.openclaw/skills`, `~/.cursor/skills`), keeping
  the user's edits and deletions.
* **CLI, `push --to` outside home (opus-2 #7):** the extract script in
  `internal/cli/transfer.go` runs as `user`; the files API rightly stays
  limited to `/home/user` and `/tmp` (Boat spec). Fix there: when
  `mkdir -p "$d"` fails or `$d` is not writable, use `sudo -n mkdir -p "$d"`
  and `sudo -n tar -xzf A -C "$d" --no-same-owner`, then `sudo -n chown
  user:user "$d"` plus `tar -tzf A | sed 's|/$||' | (cd "$d" && sudo -n
  xargs -r -d '\n' chown -h user:user)` so only the pushed files change owner.
* **CLI (skill delegate's findings):** inside a sandbox an omitted id uses
  the profile's `current` before `CROWDVM_SANDBOX_ID`, so after an
  in-sandbox `crowdvm new`, `host 3000` targets the new sandbox; `host
  --localhost` is stored but changes nothing.

## Repo rules for contributors (and agents)

* Go 1.27, stdlib first. Do not run `go mod tidy` (other packages may not have
  imported their deps yet); use `go get <mod>` to add a dependency.
* Don't edit `internal/proto`, `internal/api/api.go` or `internal/node/vm/vm.go`
  without saying so in your report; additive changes only.
* Each package has `_test.go` tests runnable with `go test ./...` without root.
  Tests that need KVM/QEMU/network are gated behind `CROWDVM_E2E=1`.
* Runtime data lives under `--data-dir` (default `~/.local/share/crowdvm/{server,node}`),
  never in the repo.
