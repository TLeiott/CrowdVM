# CrowdVM ⇄ Boat feature parity checklist

Authoritative checklist for reviewers: does CrowdVM support every Boat feature that can be supported under its constraints (LAN + Tailscale only, self-hosted, no public internet exposure, no real payments, no third-party OAuth sign-in, developed and tested on one machine)?

Sources: all 219 pages in `.ref/pages/*.md` (one per docs.boat.dev page) and `.ref/boat-v1.yaml` (122 operations). The index at the end maps every page to its section(s). Design reference: `docs/ARCHITECTURE.md`.

## Summary

| Classification | Rows |
|---|---|
| REQUIRED | 611 |
| ADAPTED | 194 |
| N/A | 26 |
| **Total** | **831** |

| Area | REQUIRED | ADAPTED | N/A | Total |
|---|---|---|---|---|
| Sandboxes & lifecycle | 33 | 15 | 2 | 50 |
| Sizes / machines | 13 | 7 | 0 | 20 |
| Commands & processes | 27 | 0 | 0 | 27 |
| Files | 15 | 1 | 0 | 16 |
| SSH, SCP, port forwarding, editors | 10 | 5 | 0 | 15 |
| Desktop & browser streaming | 17 | 3 | 0 | 20 |
| Public URLs, ports, firewall | 12 | 9 | 1 | 22 |
| Long-running, auto-stop, always-on | 10 | 2 | 0 | 12 |
| Snapshots, saved snapshots & shares | 27 | 9 | 0 | 36 |
| Environments, secrets, repositories, registries, setup | 30 | 7 | 0 | 37 |
| Integrated agents (providers, conversations, steering, computer use, customize, keys) | 37 | 3 | 1 | 41 |
| AI credentials & proxy | 20 | 4 | 1 | 25 |
| Accounts, sign-in, agent auth | 14 | 6 | 3 | 23 |
| API keys & scopes | 16 | 3 | 1 | 20 |
| Organizations / teams | 17 | 7 | 1 | 25 |
| Billing & limits (→ credits) | 7 | 14 | 5 | 26 |
| Webhooks | 11 | 2 | 0 | 13 |
| OpenTelemetry | 8 | 3 | 0 | 11 |
| Data retention & deletion | 13 | 3 | 0 | 16 |
| CI | 11 | 6 | 1 | 18 |
| Evals / RL / Harbor / Eve / AgentEnv adapters | 5 | 2 | 0 | 7 |
| Windows apps (Wine) | 6 | 2 | 1 | 9 |
| SDKs (Python / TypeScript) | 8 | 1 | 0 | 9 |
| CLI (`crowdvm`, mirrors `boat`) | 98 | 29 | 4 | 131 |
| API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour) | 113 | 26 | 1 | 140 |
| Security & isolation | 8 | 4 | 2 | 14 |
| Pre-installed tools (base image) | 12 | 3 | 0 | 15 |
| Install scripts | 5 | 5 | 1 | 11 |
| Dashboard | 8 | 9 | 0 | 17 |
| Docs for AI agents (from ai-tools page) | 0 | 4 | 1 | 5 |
| **Total** | **611** | **194** | **26** | **831** |

## Classifications

* **REQUIRED**: works on LAN/Tailscale/one machine as Boat specifies. Implement it with Boat's names, shapes, limits and error codes.
* **ADAPTED**: supportable only in a LAN-appropriate form. The row states that form exactly. Reviewers check the adapted form, not Boat's original.
* **N/A**: impossible or meaningless under the constraints. The row gives the reason. Used only for real payments, third-party identity providers, hosted-operator facts (SLA, SOC 2, regions, support), legacy-customer migration and documentation-only statements.

## Adaptation principles

1. **Wire compatibility first.** `/api/v1` keeps Boat's paths, field names, enums, envelope (`ok`, `type`), error codes, id prefixes (`bx_`, `bsh_`, `shr_`, `bdop_`, `stop_`, `cmd_`, `evt_`, `whsec_`) and the legacy header names (`X-Ascii-*`, `X-Boat-Org`, `Idempotency-Key`), so the Boat SDKs, AgentEnv, Harbor and Eve can be pointed at CrowdVM by changing only the base URL and key. CrowdVM-branded aliases (`X-CrowdVM-*`, `CROWDVM_*`, `~/.config/crowdvm`, `.crowdvmignore`) are additions, never replacements.
2. **Branding.** `boat` → `crowdvm` (CLI, env vars, config paths, skill). API keys use the `cvm_` prefix. In-sandbox compatibility names that tools rely on (`ASCII_TOKEN`, `BOAT_ID`, `~/.ascii/processes`, `ascii-record-desktop`, `ASCII_MAX_PARALLEL_CONVERSATIONS`, `~/.boat/pre-stop`, `.boatignore`) are kept as aliases.
3. **Money → credits.** Dollars, plans, wallet, seats, extra-start prices and the AI-proxy fee become a credits ledger: running sandboxes spend `seconds × size multiplier`, nodes earn credits for hosted VM time, admins grant credits. Enforcement is optional and off by default. Fields named `dollars` keep their name and carry credits. Stripe, cards, invoices, refunds and trials are N/A.
4. **Public internet → LAN/Tailnet.** Public HTTPS URLs become host-routed URLs on the coordinator (`<sub>-<port>.<domain>`, default `<server-ip>.nip.io`, plus `/p/<id>/<port>/`), HTTP unless the server has `--tls-cert/--tls-key`. "Public IP"/raw ports become node direct ports on the node's LAN/Tailscale IP. Webhook, OTLP and custom-API targets may be private or `http://` addresses, because in scope those are the only addresses that exist.
5. **Third-party sign-in → local accounts.** GitHub/Google OAuth and AgentID are replaced by local accounts (password or email code via SMTP or the server log), browser sessions, CLI device-code login, and the auth.md agent self-registration flow. GitHub access for repositories and CI uses a user-supplied token (PAT or LAN Gitea/GitLab) instead of a GitHub App.
6. **Machines → nodes.** Boat's dedicated fleet is replaced by contributed Linux/Windows nodes that benchmark themselves and host QEMU VMs. Sizes are unchanged. Regions become node labels. Features that need host support (nested KVM) are offered on capable nodes and advertised per node.
7. **Same semantics, different mechanism is fine.** Block-level qcow2 snapshots may replace Boat's file-level restore if the observable behaviour (what is or is not saved, `hydrated`, stop/resume/fork rules, errors) matches. Where CrowdVM is strictly better (for example exact live forks), keep Boat's documented guarantees as the minimum.
8. **Vendor AI services stay external.** Harness credentials (API keys, subscription tokens) still go to the real providers over the node's outbound NAT. Only the hosted OAuth sign-in UI is adapted.

Row format: `| ID | Feature | Boat source page(s) | Classification | CrowdVM mapping / note |`. "(CrowdVM)" in the source column marks a substrate requirement with no Boat page.

## Sandboxes & lifecycle

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| LIFE-001 | Sandbox = full Linux VM with own kernel, disk, IP, id `bx_…` | concepts, machines | REQUIRED | QEMU VM on a node; ids match Boat's `^bx_[23456789abcdefghjkmnpqrstuvwxyz]{8}$` (`api.NewID`, `internal/api/id_test.go`). |
| LIFE-002 | API states `init, provisioning, provisioned, cloning, ready, idle, running, archiving, archived, error, cancelled`; CLI/dashboard aliases (`starting`, `stopping`, `stopped`) | concepts, readiness, boat-v1.yaml | REQUIRED | |
| LIFE-003 | `idle` vs `running` = integrated agent turn running or not | concepts | REQUIRED | Driven by guest agent conversation state. |
| LIFE-004 | State machine: Starting→Running→Stopping→Stopped; Stopping→Running on resume (same machine); Running→Error on machine loss; Error→Starting (resume) / Running (recover); any→Deleted | concepts | REQUIRED | Error on node disconnect (heartbeat loss). |
| LIFE-005 | Billing only from `provisioned` until `archiving`; not in error | concepts, readiness | ADAPTED | Credits meter uses the same windows. |
| LIFE-006 | Stopped disk kept ≥14 days; error disk held 3 days | concepts | ADAPTED | Retention windows configurable on coordinator; defaults 14 d / 3 d. |
| LIFE-007 | Ready = state `ready\|idle\|running`; `new/fork/resume` return after first command works | readiness | REQUIRED | |
| LIFE-008 | Command during start held ≤60 s then `409 sandbox_starting`; earlier `409 machine_not_running`; stopped/error → `409 sandbox_not_ready` | readiness, platform-guide | REQUIRED | Server waits on state before proxying (ARCHITECTURE). |
| LIFE-009 | At ready: env vars + secret files set, env repos cloned, /home/user listing visible | readiness | REQUIRED | |
| LIFE-010 | New IP at every resume/fork | readiness, concepts | ADAPTED | IP = node-assigned guest address / node LAN IP; may change when moved between nodes. |
| LIFE-011 | Background restore after resume/fork/deploy: Docker containers restarted, images loaded later, enabled systemd units start; boat never runs `docker compose up` | readiness | REQUIRED | Full-disk overlay boot keeps containers, volumes and images; the guest records running containers before every stop and live snapshot and `docker start`s them at the next boot whatever their restart policy (`internal/guest/docker.go`; real-VM test `TestVMDocker`). |
| LIFE-012 | `hydrated`, `hydratedAt`, `hydration{step,entriesDone,entriesTotal,percent}` + `sandbox.hydrated` webhook | readiness | ADAPTED | Hydration = snapshot chain fully present on the node (immediate when resumed on the node that holds it); progress = bytes/layers pulled. |
| LIFE-013 | Lazy per-file fetch: reading a not-yet-downloaded file fetches it first; ready in seconds regardless of disk size | readiness | ADAPTED | Lazy block fetch of missing chain layers from coordinator (or full pull before boot on first version; must report honest `hydrated=false` until done). |
| LIFE-014 | Resume during `stopping` reuses same machine, no download | readiness, concepts | REQUIRED | Same node, overlay still local. |
| LIFE-015 | Base-image files never downloaded; only diff vs image | readiness | REQUIRED | qcow2 backing chain on cached base image. |
| LIFE-016 | Restore needs no second copy of disk; restore limits count file size | readiness | ADAPTED | Node admission check uses chain size vs free disk. |
| LIFE-017 | Permissions, owners, times, xattrs restored | readiness | REQUIRED | Block-level snapshot preserves all metadata. |
| LIFE-018 | Startup playbook `~/.ascii/playbook.json` (first-open order, ≤20 000 paths), learned per saved snapshot from deploys, prefetched first | readiness | ADAPTED | `~/.crowdvm/playbook.json` recorded by guest (fanotify) and stored with saved snapshot; used to order prefetch where lazy fetch exists, otherwise kept as metadata only. |
| LIFE-019 | First outbound connection may fail in first second (documented caveat) | readiness | N/A | Documentation caveat, not a feature. |
| LIFE-020 | Stop = save disk then remove machine; no memory kept; no pause | concepts | REQUIRED | |
| LIFE-021 | Resume = same id, newest snapshot, new machine | concepts | REQUIRED | Any node with capacity; chain pulled if missing. |
| LIFE-022 | Fork = new independent sandbox from newest snapshot of source; source untouched; fork of running uses last background snapshot | concepts, cli-reference | ADAPTED | Live QMP snapshot gives an exact current state (better than Boat); still document "may miss unsynced writes". |
| LIFE-023 | Recover = power-cycle without touching disk | concepts, troubleshooting | REQUIRED | |
| LIFE-024 | Delete = erase sandbox and its unshared snapshots, irreversible | concepts | REQUIRED | |
| LIFE-025 | `cancelled` state for aborted creates | boat-v1.yaml | REQUIRED | |
| LIFE-026 | `lastReboot{at,reason: crash\|clean\|unknown}` on sandbox | cli-reference, boat-v1.yaml | REQUIRED | Guest detects previous boot end from journal. |
| LIFE-027 | `health`, `healthReason`, `degradedSince` fields; `sandbox.degraded` / `sandbox.recovered` | boat-v1.yaml, webhooks | REQUIRED | Degraded = node or guest unreachable. |
| LIFE-028 | Sandbox `labels` (create/fork), list filter `label` | boat-v1.yaml | REQUIRED | |
| LIFE-029 | Sandbox `name` (≤120) and rename (creator only) | cli-reference | REQUIRED | |
| LIFE-030 | `subdomain` updatable via PATCH (custom host label for URLs) | boat-v1.yaml | ADAPTED | Host label under the LAN public domain. |
| LIFE-031 | `Idempotency-Key` on create and fork (`idempotency_key_reused`, `idempotency_in_progress`) | api create/fork pages, boat-v1.yaml | REQUIRED | |
| LIFE-032 | Per-sandbox env tags for tenant mapping (`env:{TENANT_ID}`) — "two rules" of platform guide | platform-guide | REQUIRED | |
| LIFE-033 | Platform patterns: always-on per project, per user, stop/resume around usage | platform-guide__patterns | REQUIRED | Composition of LIFE/RUN/URL features; no extra code. |
| LIFE-034 | Stop refused when save fails: keeps running, auto-retry, owner emailed, meter paused, forced after 3 days of failed saves | platform-guide__patterns, snapshots__stop | ADAPTED | Before prepare-stop touches the guest, the server takes a pre-flight live snapshot; if that save fails the stop is refused: the sandbox goes back to `ready` and stays usable (caches, Docker and the pre-stop hook untouched), the stop is `failing` and retried with backoff, the meter pauses (`billingPaused: "stop_refused"`), the owner gets a notice (dashboard + optional SMTP instead of email). A save that fails after the pre-flight also keeps the sandbox usable when its machine still answers. A resume cancels the stop; a refused auto-stop meters again when the sandbox is used; forced after 3 days with a 1-day warning (`lifecycle_pause.go`). |
| LIFE-035 | Daemon pattern: user daemon as systemd service + private token URL | platform-guide__agents | REQUIRED | Works via RUN + URL features. |
| LIFE-036 | Concurrency + start-rate limits per account (`429 rate_limited` with `window`) | platform-guide__templates-and-sizing, limits | ADAPTED | Admin-configurable per-account limits; defaults generous; capacity bounded by nodes. |
| LIFE-037 | Templates = saved snapshots deployed with `--from` (not fork of stopped sandbox) | platform-guide__templates-and-sizing | REQUIRED | |
| LIFE-038 | `wipePendingUntilRestart`, `holdsCreatorLogins`, `cost`, `createdBy`, `createdById`, `team`, `access`, `stop` fields on Sandbox | boat-v1.yaml | REQUIRED | `cost` in credits. |
| LIFE-039 | `machineProvider` field | boat-v1.yaml | ADAPTED | Node id / hostname. |
| LIFE-040 | Unresponsive detection: sandbox marked not answering, owner notified, **billing paused**, auto-reconnect when it answers | troubleshooting | ADAPTED | Guest health misses → `health: degraded` (`guest_unreachable`), `sandbox.degraded`, credits meter paused (`billingPaused: "not_answering"`), owner notice (dashboard + optional SMTP); the meter and health come back when it answers (`sandbox.recovered`) or after `recover`. |
| LIFE-041 | Error state: billing stops; machine+disk held 3 days; auto-rebuild on another machine from snapshot newer than failure; forced stop to last snapshot after 3 days (1-day reminder) | troubleshooting | REQUIRED | Node loss → reschedule from latest uploaded snapshot. |
| LIFE-042 | Recover refusals: "responding normally", "already started, N minutes", "cannot be power-cycled" | troubleshooting | REQUIRED | |
| LIFE-043 | OOM: 512 MiB reserved for platform services; killed command → exit 137, `oomKilled:true`; sandbox stays up | troubleshooting | REQUIRED | Guest agent + cgroup for user workloads. |
| LIFE-044 | Disk alerts at 85% / 95% (≤1 per level per day) and automatic cache cleanup (apt cache, journal, unused pulled images) | troubleshooting | ADAPTED | Alerts in dashboard/SMTP; cleanup by guest deletes only pulled images that no container or Compose project uses, never images built in the sandbox: on Docker's containerd image store an image counts as pulled only when containerd recorded a registry source for it (`containerd.io/distribution.source.*`), on the classic store when it has a registry digest; when unsure nothing is deleted (`guest/docker.go` `pruneImages`/`pulledFilter`, e2e `disk-cleanup`). |
| LIFE-045 | `409 sandbox_stopping` when resuming with size/env change during `stopping` | troubleshooting | REQUIRED | |
| LIFE-046 | Error message texts with remediation command (`snapshot_data_expired`, `sandbox_starting`, `sandbox_not_ready`, `machine_not_running` with `retryable`, `rate_limited` options) | troubleshooting | REQUIRED | Texts reference `crowdvm` commands. |
| LIFE-047 | One image for all; no custom image/Dockerfile/kernel (use saved snapshots; nested VM for another kernel) | environments__choose, faq | REQUIRED | Same model. |
| LIFE-048 | Commands have no per-call `env` field (prefix `export`) | faq | REQUIRED | Keep API shape (don't add). |
| LIFE-049 | Shared vCPUs; perf varies by host; sustained 100% CPU loads discouraged | faq, machines__virtualization | ADAPTED | Contributor machines vary more; benchmark score exposed. |
| LIFE-050 | Region/latency (EU hosts, 100–200 ms from US/LatAm) | faq | N/A | No regions on a LAN/Tailnet; placement via node labels (SEC-008). |

## Sizes / machines

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| SIZE-001 | Four sizes `small` 2vCPU/4GB/51GB, `default` 4/8/51, `large` 8/16/111, `xlarge` 16/32/251; no custom sizes | machines, pricing | REQUIRED | Scheduler places only on nodes whose advertised capacity fits; disk = qcow2 virtual size. |
| SIZE-002 | Default type `default` when `--type` omitted | machines | REQUIRED | |
| SIZE-003 | Per-size hourly price | machines, pricing | ADAPTED | Credit multiplier per size (`billingMultiplier`: 0.5/1/2/5.55). |
| SIZE-004 | `large` gated on first payment, `xlarge` on $100 plan | machines | ADAPTED | Optional per-wallet allowed-sizes policy set by an admin (off by default); a disallowed size answers Boat's `403 machine_class_plan_required` with `details.policy: "admin"` and `allowedSizes`; `GET /limits` marks it `not_allowed_by_policy`. |
| SIZE-005 | All sizes x86_64, no GPU | machines | REQUIRED | Nodes must be x86_64 hosts (or TCG fallback flagged slow). Crowd extension: `--gpu` gives a VFIO-passed-through GPU on nodes whose owner set `gpu.passthrough` (checked at node start, `NodeInfo.GPUs`); `gpu.virgl` adds an OpenGL virtio-gpu (ARCHITECTURE "GPUs"). |
| SIZE-006 | "Disk" = user data limit, OS excluded; `df -h /` shows live | machines | REQUIRED | growpart/resize2fs to size at boot. |
| SIZE-007 | Size kept across stop/resume/fork; fork inherits source size | machines | REQUIRED | |
| SIZE-008 | Resize: `resume --type`; on running → `409 sandbox_running_stop_first` | machines__change-size | REQUIRED | |
| SIZE-009 | Fork to another size: `fork --type` | machines__change-size | REQUIRED | |
| SIZE-010 | Shrink refused with `type_too_small` when data exceeds target; sandbox unchanged | machines__change-size | REQUIRED | Compare used blocks vs target size. |
| SIZE-011 | `failFast` on create/deploy/fork/resume: ~1.5 s, `503 no_ready_machine`, nothing created/billed/counted | machines__fail-fast | REQUIRED | Fail when no online node has free capacity (+ image cached). Without `failFast` a boot waits (`waitingForCapacity`, `statusMessage`); a node that refuses the boot for its real disk budget (503 `out_of_capacity`) queues it the same way, or cancels it with `no_ready_machine` under `failFast`. |
| SIZE-012 | Warm pool: create/fork/resume land on pre-booted machines in <1 s | machines__fail-fast | ADAPTED | Optional per-node warm VM pool of base image; default off on small LANs. |
| SIZE-013 | `failFast` with `hostedPorts` / `count` refused (`fail_fast_with_hosted_ports`, `count_with_fail_fast`) | boat-v1.yaml, cli-reference | REQUIRED | |
| SIZE-014 | Nested virtualization: `/dev/kvm` in every sandbox | machines__virtualization | ADAPTED | Expose `/dev/kvm` (host-passthrough CPU) on nodes whose host has nested KVM; node label `nested-virt`; absent on Windows/WHPX nodes. |
| SIZE-015 | Dedicated hosts (Ryzen 9700X/9950X), shared CPU time, VT passthrough | machines__virtualization | ADAPTED | Contributor machines; node benchmark score shown per node/sandbox. |
| SIZE-016 | Android emulator runs (needs KVM, ≥default) | machines__virtualization | ADAPTED | Works only on nested-virt nodes. |
| SIZE-017 | Dashboard Type column + Type filter; API `type`, `vcpu`, `memoryGB`, `billingMultiplier` | machines, boat-v1.yaml | REQUIRED | |
| SIZE-018 | Root/sudo (`user`, passwordless), real systemd, Docker+BuildKit+Compose without sudo | machines | REQUIRED | |
| SIZE-019 | Public IPv4/IPv6 + outbound internet per sandbox | machines | ADAPTED | Outbound NAT via node (QEMU user net); inbound only via coordinator tunnels / optional node direct ports. No public IP. |
| SIZE-020 | Node scheduling, benchmark and capacity limits (CrowdVM-specific substrate for "machines") | ARCHITECTURE | REQUIRED | Node benchmark (CPU/RAM/disk/net), owner-set caps, heartbeat. |

## Commands & processes

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| CMD-001 | `POST /commands` sync: `{command,cwd,timeoutSeconds}` → `exitCode, signal, oomKilled, stdout, stderr, *Truncated, timedOut, cwd, startedAt, finishedAt, commandId` | api execute-sandbox-command, boat-v1.yaml | REQUIRED | Guest agent; runs as `user` via login shell. |
| CMD-002 | `timeoutSeconds` 1–600, default 30; out of range → `400 invalid_timeout`; timed out → `timedOut:true`, `retryable:false` | long-running-tasks__background-commands | REQUIRED | |
| CMD-003 | `cwd` relative to `/home/user` | cli-reference | REQUIRED | |
| CMD-004 | Sync output cap 8 MiB per stream (`stdoutTruncated`) | long-running-tasks__command-history | REQUIRED | |
| CMD-005 | `stream:true` → NDJSON frames `started/stdout/stderr/exit/error` | boat-v1.yaml (CommandStreamFrame) | REQUIRED | Flushed through server→node→guest. |
| CMD-006 | `detached:true` → `command.started{processId,pid,logPath}`; log at `~/.ascii/processes/<pid>.log` | long-running-tasks__background-commands | REQUIRED | Keep path `~/.ascii/processes/` for compat (or `~/.crowdvm/processes` + symlink). |
| CMD-007 | `GET /commands/{pid}` status: `running, status(running\|exited\|lost), known, exitCode, signal, stdout/stderr tail, tailBytes, stdoutOffset/stderrOffset → *NextOffset, more` | long-running-tasks__background-commands, boat-v1.yaml | REQUIRED | |
| CMD-008 | `GET /commands/{pid}/stream` attach NDJSON: `attached`, `stdout/stderr{data,nextOffset}`, `heartbeat` (15 s idle), `reconnect` (10 min, offsets), `exit`, `error`; resume w/o gap | long-running-tasks__background-commands | REQUIRED | |
| CMD-009 | `DELETE /commands/{pid}`: SIGTERM process group, SIGKILL after 5 s; returns history record (`killed`) | long-running-tasks__background-commands | REQUIRED | |
| CMD-010 | Detached processes run under the user's systemd manager; survive guest-agent restart; per-process records on disk; `lost` for unknown | long-running-tasks__background-commands | REQUIRED | `systemd-run --user --scope/unit`. |
| CMD-011 | Detached processes do not survive stop/resume/fork | long-running-tasks | REQUIRED | |
| CMD-012 | Live polling/attach require running sandbox; `409 agent_outdated` for old guest | long-running-tasks__background-commands | REQUIRED | Guest version check; guest updates on every boot. |
| CMD-013 | No automatic retry of command calls (not idempotent) | long-running-tasks__background-commands | REQUIRED | |
| CMD-014 | Rate limit 10 000 requests / 60 s per IP; a stream counts once | long-running-tasks__background-commands | REQUIRED | Coordinator rate limiter. |
| CMD-015 | Command history: exec/API commands kept 90 d (`sync`, `stream`, `detached`), other processes 72 h (`process`) | long-running-tasks__command-history | REQUIRED | Stored on coordinator (survives stop). |
| CMD-016 | History record: text (64 KiB), cwd, mode, processId, status `running/exited/timed_out/failed/killed/lost`, exit code, signal, times, byte counts, `outputTruncated` | long-running-tasks__command-history, boat-v1.yaml | REQUIRED | |
| CMD-017 | Output upload cadence: streamed every 2 s, detached every 30 s and at stop | long-running-tasks__command-history | REQUIRED | Guest pushes to coordinator via node. |
| CMD-018 | `GET /command-history?mode=&q=&limit=&cursor=` (works when stopped) | api list-command-history | REQUIRED | |
| CMD-019 | `GET /command-history/{id}` and `/logs?stream=stdout\|stderr&offset=&limit=&encoding=base64` (≤1 MiB per read, `nextOffset`, `done`) | long-running-tasks__command-history | REQUIRED | |
| CMD-020 | Per-account cap 10 MiB compressed command output/hour (and separate 10 MiB/h process cap) → `outputTruncated` | long-running-tasks__command-history | REQUIRED | Configurable. |
| CMD-021 | Capture every process's terminal/pipe/socket output without program changes (64 KiB/s per process, 1 MiB burst; excludes files, /dev/null, binary, platform services); visible within ~30 s; flushed at stop | long-running-tasks__command-history | REQUIRED | Done: journald + Docker json-file followers (services, containers) and a guest eBPF tracer on write(2)/writev(2) to tty/pipe/socket fds and send(2)/sendto(2)/sendmsg(2) to sockets (the journal via syslog/logger, syslog `<PRI>` stripped) for ssh sessions and agent-started processes; process records finish with the exit code or signal from the kernel's exit event (`internal/guest/tracer.go`, `tracer_bpf.go`, `bpf/writer.c`); e2e `process-history` (pipe, tty, background writer, `logger`, socketpair `send`, exit codes 0 and 3). |
| CMD-022 | Exec'd command children not duplicated as `process` records | long-running-tasks__command-history | REQUIRED | Done: the tracers skip the exec workload cgroups (`cmd_*`) and the guest's own tree; e2e `process-history` checks an exec'd command has no `process` record. |
| CMD-023 | History deleted with sandbox; ZDR queues it at stop; monitoring off → none kept | long-running-tasks__command-history | REQUIRED | |
| CMD-024 | `GET /processes`: busiest first, pid, parentPid, user, cpuPercent, memoryMB, startedAt, command, commandId; `live`, `total` | long-running-tasks__command-history, boat-v1.yaml | REQUIRED | |
| CMD-025 | Stopped sandbox → last sample (12 busiest every 30 s, kept 7 d), `live:false` | long-running-tasks__command-history | REQUIRED | |
| CMD-026 | Dashboard History view (⋯ menu): live output of background command, stop button | long-running-tasks__command-history | REQUIRED | |
| CMD-027 | CPU/RAM samples retained (monitoring) | data-retention__snapshots-and-monitoring-off | REQUIRED | |

## Files

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| FILE-001 | `GET /files?path=&encoding=utf8\|base64` read | api read-sandbox-file | REQUIRED | Guest agent. |
| FILE-002 | `PUT /files {path,content,encoding}` write/overwrite (creates parents) | api write-sandbox-file | REQUIRED | |
| FILE-003 | `DELETE /files?path=&recursive=true` | api delete-sandbox-file | REQUIRED | |
| FILE-004 | `GET /files/list?path=` entries `name,path,isDir,isSymlink,size,mode,modifiedAt` | api list-sandbox-files | REQUIRED | |
| FILE-005 | `GET /files/info?path=` | api get-sandbox-file-info | REQUIRED | |
| FILE-006 | `GET /files/search?path=&pattern=` glob on names | api search-sandbox-files | REQUIRED | |
| FILE-007 | `GET /files/find?path=&pattern=&regex=` text/regex line matches, `truncated`, `errors` | api find-in-sandbox-files | REQUIRED | |
| FILE-008 | `POST /files/folder {path,mode}` mkdir -p | api create-sandbox-folder | REQUIRED | |
| FILE-009 | `POST /files/move {source,destination,overwrite}` incl. between /home/user and /tmp | api move-sandbox-file | REQUIRED | |
| FILE-010 | `POST /files/permissions {path,mode}` | api set-sandbox-file-permissions | REQUIRED | |
| FILE-011 | `POST /files/replace {paths,pattern,newValue}` per-file results | api replace-in-sandbox-files | REQUIRED | |
| FILE-012 | Path rules: relative → /home/user; absolute must be under /home/user or /tmp; else `400 invalid_path` naming roots | ssh-access | REQUIRED | |
| FILE-013 | JSON file calls limited to 5 MiB | ssh-access | REQUIRED | |
| FILE-014 | `POST /files/upload-url` / `download-url {path,ttlSeconds≤3600}` → signed URL (15 min default), no auth header, any size, one path+direction; upload creates dirs, atomic replace | ssh-access, api create-upload-url | ADAPTED | URL points at coordinator (signed), streamed to guest; "sandbox serves itself" not possible without public guest IP. Scopes `file.read`/`file.write`. |
| FILE-015 | `GET /artifacts?path=` binary download | api download-sandbox-artifact, ssh-access | REQUIRED | |
| FILE-016 | Prompt attachments saved under `~/attachments` | cli-reference | REQUIRED | |

## SSH, SCP, port forwarding, editors

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| SSH-001 | `ssh <id>` creates key, authorizes it, verifies host key automatically | ssh-access | ADAPTED | Key `~/.ssh/crowdvm_ed25519`; connection via `ProxyCommand crowdvm tunnel %h 22` over coordinator websocket; host key pinned from API. |
| SSH-002 | Dashboard "Copy SSH" row action | ssh-access | REQUIRED | |
| SSH-003 | SSH only while running; transient `sandbox_restoring` / `sandbox_securing` during start, CLI retries | ssh-access | REQUIRED | |
| SSH-004 | `POST /sandboxes/{id}/sshkey {key}` → `machineIp`, `sshEndpoint` (host:port when no public IPv4), `hostKey` | ssh-access, api configure-sandbox-ssh-key | ADAPTED | `sshEndpoint`/`machineIp` = the node's direct ssh port on its LAN/Tailscale IP when the node runs with `--direct-host`; otherwise both are `null` (no coordinator TCP port mapping is built) and stock OpenSSH/VS Code use the tunnel form `ssh -o ProxyCommand="crowdvm tunnel %h 22" user@<id>` that `crowdvm ssh-config` writes. |
| SSH-005 | Host key delivered over the API (no TOFU); `HostKeyAlias` pinning; new host key per resume/fork | ssh-access | REQUIRED | Guest regenerates host keys on create/fork (and per boot per Boat semantics); reports public key. |
| SSH-006 | Authorized keys persist across stop/resume/fork | ssh-access | REQUIRED | |
| SSH-007 | Reverse tunnel with stock OpenSSH after authorizing key (reach your machine from sandbox) | api configure-sandbox-ssh-key | ADAPTED | Through `sshEndpoint` on direct-host nodes; elsewhere through the `crowdvm tunnel` ProxyCommand (or `crowdvm forward --reverse`). |
| SSH-008 | VS Code / Cursor Remote-SSH via `~/.ssh/config` Include + per-sandbox conf script | ssh-access__editors | ADAPTED | `crowdvm ssh-config <id>` writes a `~/.ssh/config` entry (ProxyCommand tunnel with a pinned known_hosts; direct endpoint on direct-host nodes) that VS Code / Cursor Remote-SSH use; no coordinator port mapping. |
| SSH-009 | Editor server in `~/.vscode-server` persists in snapshots | ssh-access__editors | REQUIRED | |
| SSH-010 | Open editor does not delay auto-stop | ssh-access__editors | REQUIRED | |
| SSH-011 | `forward` local and `--reverse`; loopback only; auto reconnect; API key needs `ssh` action | ssh-access, cli-reference | REQUIRED | Over guest `/v1/tunnel` (local) and an SSH `-R` or guest listener (reverse). |
| SSH-012 | `scp` with `bx_<id>:/path` remote syntax, `-r/--recursive`, tar fast path | ssh-access, cli-reference | ADAPTED | OpenSSH scp + ProxyCommand. |
| SSH-013 | `ssh -t` TTY for tmux/full-screen | cli-reference | REQUIRED | |
| SSH-014 | `push` over SSH archive | cli-reference | REQUIRED | |
| SSH-015 | CLI tunnel primitive used as SSH ProxyCommand | (CrowdVM) | REQUIRED | `crowdvm tunnel <id> <port>` = stdio ↔ guest TCP port (used as ProxyCommand). |

## Desktop & browser streaming

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| DESK-001 | Linux desktop per sandbox (Xfce-class), display `:0`, GUI apps, Chrome, Electron | desktop-streaming | REQUIRED | Xfce + TigerVNC in base image (ARCHITECTURE). |
| DESK-002 | `POST /sandboxes/{id}/desktop[?theme=light\|dark][&vnc=1] {publicAccess}` → `desktopUrl, ip, mode, provisioning, message`; `provisioning:true` → poll | desktop-streaming, api get-desktop-streaming-url | ADAPTED | URL = coordinator noVNC page proxied over websocket → guest VNC; `vnc=1` is the native mode. |
| DESK-003 | Stream transport Moonlight/WebRTC 1920×1080@60 fps | desktop-streaming | ADAPTED | noVNC over WebSocket at 1920×1080; frame rate best-effort (LAN). WebRTC/Moonlight not required. |
| DESK-004 | `desktop_not_ready` while starting; only while running | desktop-streaming | REQUIRED | |
| DESK-005 | Desktop URL must be opened within 10 min; open stream keeps playing; reload needs new URL; ends at stop | desktop-streaming__urls-and-security | REQUIRED | One-time token with 10-min first-use window. |
| DESK-006 | Token in URL fragment (`#token=`), viewer strips it and sends in headers / first WS frame, never in request URL | desktop-streaming__urls-and-security | REQUIRED | Implement in CrowdVM viewer page. |
| DESK-007 | `overlay=0` hides viewer side panel/stats | desktop-streaming__urls-and-security | REQUIRED | |
| DESK-008 | Embeddable in iframe (`allow=autoplay; fullscreen; clipboard-*`) | desktop-streaming__urls-and-security | REQUIRED | Frame-ancestors permissive for viewer page. |
| DESK-009 | List/info never return/cache desktop URL; API key needs `desktop` action | desktop-streaming__urls-and-security | REQUIRED | `desktopAvailable` bool only. |
| DESK-010 | Clipboard bridge only with active stream credentials | desktop-streaming__urls-and-security | REQUIRED | VNC clipboard via authenticated WS. |
| DESK-011 | CLI `desktop` prints `viewerUrl`; dashboard "open desktop" while running | desktop-streaming | REQUIRED | |
| DESK-012 | Desktop programs don't survive resume/fork; desktop daemon + computer tools auto-restart | desktop-streaming__urls-and-security | REQUIRED | |
| DESK-013 | Custom wallpaper `~/.config/boat/wallpaper.png` (PNG ≤16 MiB, ≤8192 px side, ≤16.7 Mpx; aspect kept; invalid → default; persists) | desktop-streaming__background | REQUIRED | Watch `~/.config/crowdvm/wallpaper.png` and `~/.config/boat/wallpaper.png`. |
| DESK-014 | Browser-only view `POST /sandboxes/{id}/browser` → `browserUrl`; CLI `browser`; dashboard "Browser" | desktop-streaming__browser-only | ADAPTED | Separate X display/VNC with kiosk Chrome (tabs+address bar, no close controls), noVNC viewer. Endpoint not in OpenAPI but documented → implement. |
| DESK-015 | `--profile <abs path>` Chrome user-data-dir (created, owned by browser user, outside /home/user and system dirs; persisted; Chrome restarts on change; `chrome_profile_unsupported` for old sandboxes) | desktop-streaming__browser-only, cli-reference | REQUIRED | |
| DESK-016 | Browser view semantics: last tab closed → new tab; re-run keeps stream; second viewer takes over; coexists with desktop; no clipboard; never falls back to desktop | desktop-streaming__browser-only | REQUIRED | |
| DESK-017 | Browser view hides `/home/user` from browser sandbox | cli-reference | REQUIRED | Run Chrome under separate user / mount namespace with tmpfs over /home/user. |
| DESK-018 | `ascii-record-desktop start <file.mp4>\|stop\|status` | desktop-streaming__drive-and-record | REQUIRED | ffmpeg x11grab wrapper in image (keep old name + `crowdvm-record-desktop`). |
| DESK-019 | Agents drive the same screen via computer tools; one screen shared by conversations | desktop-streaming__drive-and-record | REQUIRED | See AGT computer-use rows. |
| DESK-020 | `wmctrl`, `DISPLAY=:0` usable from exec | desktop-streaming__drive-and-record | REQUIRED | |

## Public URLs, ports, firewall

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| URL-001 | `POST /sandboxes/{id}/host {port,public,localhost,title}` → `url, access, isProtected, localhost, port` | hosting, hosting__host-cli | ADAPTED | URL `http(s)://<subdomain>-<port>.<public-domain>` routed by coordinator (host routing; default domain `<server-ip>.nip.io`) plus path form `http://server/p/<id>/<port>/`. HTTPS only if `--tls-cert/--tls-key` given. |
| URL-002 | Stable URL: same sandbox+port → same URL; persists across stop/resume | hosting | ADAPTED | Route table on coordinator keyed by sandbox+port. |
| URL-003 | Protected by default: `?_token=` → redirect setting `_port_auth` cookie; header `Cookie: _port_auth=` works; no sign-in/org check | hosting | REQUIRED | |
| URL-004 | `--public` clears token; later protected host mints a new token (token "rotation") | hosting | REQUIRED | |
| URL-005 | Up to 50 hosted ports per sandbox | hosting | REQUIRED | |
| URL-006 | HTTPS + WebSockets carried; HMR works | hosting, hosting__how-it-works | ADAPTED | HTTP(+TLS optional) + WebSocket upgrade proxied over yamux → guest tunnel. |
| URL-007 | `--localhost` relay to 127.0.0.1 then ::1; raw TCP relay; survives stop/resume/agent upgrade, not fork; reset by each host call; turned off by hide/private | hosting__how-it-works | REQUIRED | Guest tunnel dials 127.0.0.1/::1 directly; record flag per port. |
| URL-008 | Reserved ports 22, 6080, 8090, 8911, 47984–48010 | hosting | ADAPTED | CrowdVM reserved set: 22, 7070 (guest), 5900/6080 (VNC/noVNC); documented. |
| URL-009 | `PATCH subdomain` (3–40 chars, lowercase/digits/single hyphens, not `-desktop`/`-<n>` suffix; `409 subdomain_taken`; `sandbox_busy` unless running/stopped; old URLs stop, tokens kept) | hosting | ADAPTED | Label under LAN domain. |
| URL-010 | Custom domains not supported | hosting | ADAPTED | Same (documented); users may point their own LAN DNS/proxy at coordinator with Host header. |
| URL-011 | `on.ascii.dev` legacy suffix alias | hosting__how-it-works | N/A | Boat's historical public domain; no legacy domain in CrowdVM. |
| URL-012 | Raw TCP/UDP via sandbox IP after `ufw allow`; `host --public` also opens TCP port on IP | hosting | ADAPTED | Node "direct ports": node publishes per-sandbox TCP/UDP on its LAN/Tailscale IP (hostfwd); `ip`/`sshEndpoint` reflect it. The node opens the guest firewall for the published TCP ports (guest `PUT /v1/internal/firewall`, again after every boot); UDP stays the user's `ufw allow`, as in Boat. |
| URL-013 | In-sandbox `host <port> [--title] [--private\|--public] [--localhost]` using sandbox-scoped token (`ASCII_TOKEN`) | hosting__host-cli, hosting__how-it-works | REQUIRED | `host` binary in image talks to guest agent → coordinator with per-sandbox key; can only host own ports. |
| URL-014 | In-sandbox `host list` (`(gated)`, `(localhost)`, no tokens), `host url <port> [--public]` (waits until ready, prints full URL), `host hide <port>` (unregister route, relay off, tokens kept) | hosting__host-cli | REQUIRED | |
| URL-015 | `boat host --json` → `sandboxId, port, url, access, isProtected, localhost` | hosting__host-cli | REQUIRED | |
| URL-016 | `hostedPorts` on create (routed before ready) | cli-reference | REQUIRED | |
| URL-017 | Protected URL in third-party iframe caveat; use public or own proxy | hosting | REQUIRED | Same cookie semantics. |
| URL-018 | Vite `allowedHosts` guidance for the URL domain | hosting__how-it-works | ADAPTED | Doc names CrowdVM domain. |
| URL-019 | No network API; root firewall via ufw/iptables; firewall rules not kept in snapshots | hosting__firewall | REQUIRED | ufw in image; rules reset per boot. |
| URL-020 | Platform needs inbound agent port + WireGuard tunnel; blocking them → `health: degraded` + `sandbox.degraded`, recovered on removal; never stopped/errored for it | hosting__firewall | ADAPTED | CrowdVM reaches the guest via the node's loopback forward into the VM's private network (not guest network rules) so guest firewall cannot cut it off; degraded still raised when guest agent unreachable. |
| URL-021 | Per-uid outbound blocking recipe for untrusted code | hosting__firewall | REQUIRED | Works on any Linux guest. |
| URL-022 | Default-deny inbound firewall in sandbox | hosting__firewall | REQUIRED | ufw enabled in image, guest agent port excluded. |

## Long-running, auto-stop, always-on

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| RUN-001 | Auto-stop TTL default 3600 s, max 2 592 000 s (larger capped), counted from create/resume not activity | long-running-tasks__auto-stop | REQUIRED | Coordinator timer. |
| RUN-002 | `ttlSeconds:null` = no auto-stop (create, PATCH, resume) | long-running-tasks__auto-stop | REQUIRED | |
| RUN-003 | Running/queued agent prompt pushes deadline forward one TTL; exec/SSH/services do not | long-running-tasks__auto-stop | REQUIRED | |
| RUN-004 | `archiveAfter` = deadline (null when off); `ttlSeconds` = last set TTL | long-running-tasks__auto-stop | REQUIRED | |
| RUN-005 | Stop starts within ~5 s of deadline, same as `stop` (pre-stop hook runs) | long-running-tasks__auto-stop | REQUIRED | |
| RUN-006 | Extend = now + TTL, persisted for later resumes (`PATCH ttlSeconds`) | long-running-tasks__auto-stop | REQUIRED | |
| RUN-007 | Resume without TTL keeps setting; fork defaults to 1 h | long-running-tasks__auto-stop | REQUIRED | |
| RUN-008 | Trial: TTL ≤2 h, no-auto-stop refused with `trial_auto_stop_required` | long-running-tasks__auto-stop, free-trial | ADAPTED | Optional per-account "restricted" policy (admin-set) reusing the same error code; off by default. |
| RUN-009 | Dashboard "No auto-stop" control | long-running-tasks__auto-stop | REQUIRED | |
| RUN-010 | Enabled systemd services restart after resume/fork (always-on services) | long-running-tasks__always-on-services | REQUIRED | Full-disk snapshot + real boot. |
| RUN-011 | `host` same port after resume returns same URL + token | long-running-tasks__always-on-services | ADAPTED | Same LAN URL/token persisted on coordinator. |
| RUN-012 | `--timeout 0` / SDK `run_command`/`runCommand` run detached + attach until exit | long-running-tasks__background-commands | REQUIRED | |

## Snapshots, saved snapshots & shares

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| SNAP-001 | Automatic incremental snapshot every minute while running + one at stop; only changed data | snapshots, concepts | REQUIRED | Periodic QMP live snapshot (interval configurable, default 60 s); overlay chain = incremental. |
| SNAP-002 | Snapshot `kind` base/incremental; `lastSnapshotAttemptAt`, `lastSnapshotStatus`, `snapshotAvailable`, `snapshotCompletedAt`, `snapshotVerifiedAt` | boat-v1.yaml | REQUIRED | |
| SNAP-003 | Resume/fork always use newest snapshot; no point-in-time restore | snapshots__resume-and-fork | REQUIRED | |
| SNAP-004 | Older automatic snapshots listed briefly (~1 day) then expire; newest kept ≥14 d after stop; then `409 snapshot_data_expired` | snapshots__stop, snapshots__download-and-delete | ADAPTED | Coordinator GC with configurable windows; chains flattened beyond N layers. |
| SNAP-005 | Stop sequence: pre-stop hook → stop Docker containers (30 s each) → last snapshot → power off; OS not shut down; other programs get no signal | snapshots__stop | REQUIRED | Guest `prepare-stop` runs hook + `docker stop`, then node snapshots/poweroff. |
| SNAP-006 | Pre-stop hook `~/.boat/pre-stop` (as `user`, $HOME, 60 s cap, failures ignored; not on `--force` or saved-snapshot save) | snapshots__stop | REQUIRED | `~/.crowdvm/pre-stop`; also honour `~/.boat/pre-stop` for compat. |
| SNAP-007 | Same machine kept 10 min after `stopped`; plain resume reuses it unbilled | snapshots__stop | ADAPTED | Node keeps the final overlay locally; resume prefers that node (no pull) for a configurable window. |
| SNAP-008 | Refused stop: meter paused from first failure, auto-retry, email, forced stop after 3 days with 1-day warning; `forced:true` in reply | snapshots__stop | ADAPTED | As LIFE-034: pre-flight save, sandbox stays usable while the stop is `failing`, meter paused from the first refusal (`billingPaused`), retries with backoff, notice via dashboard + optional SMTP, forced stop after 3 days with 1-day warning; `forced:true` in reply. |
| SNAP-009 | Guidance: allow the agent service outbound or saves fail | snapshots__stop | ADAPTED | Snapshots are host-side (node), so guest firewalling cannot break saves; document this difference. |
| SNAP-010 | Saved: /home/user, other /home dirs, new top-level dirs, Docker volumes/containers/used images, changes under /etc /usr /opt /root /srv /var (owners/perms); deletions persist | snapshots__whats-saved | REQUIRED | Block-level overlay keeps all of it. |
| SNAP-011 | Not saved: OS base, hostname/IP/network/SSH host keys, /tmp, /var/log/journal, Docker internals except volumes, ~/.cache, build cache, unused images | snapshots__whats-saved | REQUIRED | Guest prepare-stop deletes those paths, then fstrim (ARCHITECTURE); identity regenerated per boot. Images kept: those a container or a Compose project (with a container) uses; the rest pruned. A disk from a live snapshot (fork/deploy of a running sandbox) gets the same cleanup at its first boot. |
| SNAP-012 | Registry images re-pulled by digest on restore | snapshots__whats-saved | ADAPTED | Images stay on the block snapshot; no re-pull needed (superset). |
| SNAP-013 | Restore = reboot: enabled systemd units and previously running containers restart; snap packages reinstalled | snapshots__whats-saved | REQUIRED | |
| SNAP-014 | `.boatignore` (gitignore syntax, per-folder scope, up to 6 levels under $HOME, skips node_modules/.next/target/vendor, `.git` always saved, also `.boxignore`/`.oneignore`) | snapshots__whats-saved | ADAPTED | `.crowdvmignore` + read `.boatignore`/`.boxignore`/`.oneignore`; matched paths deleted at prepare-stop (they are then gone on resume, same as Boat). |
| SNAP-015 | Saved snapshot: `snapshot <id> <name>` → 202 `saving`, poll to `ready\|failed`; running → fresh capture; stopped → last snapshot | snapshots__templates, api save-saved-snapshot | REQUIRED | Saved snapshot = pinned, flattened copy on coordinator store. |
| SNAP-016 | Saved snapshot independent of source (source may change/delete) | snapshots__templates | REQUIRED | Pin chain refs; GC aware. |
| SNAP-017 | Re-save same name replaces atomically on success; failure keeps last good | snapshots__templates | REQUIRED | |
| SNAP-018 | Deploy `--from <name>` in seconds; does not get source environment; does get source per-sandbox vars unless `env` sent | snapshots__templates | REQUIRED | |
| SNAP-019 | `--from` a `--no-env` sandbox's snapshot stays no-env | cli-reference | REQUIRED | |
| SNAP-020 | Saved-snapshot allowance (10 per $20/seat, min 10), $0.056/day extras, `credit_required`, 7-day grace + deletion emails; `allowance` in `GET /saved-snapshots` | snapshots__templates | ADAPTED | Allowance = admin-configured count; extras cost credits/day; `credit_required` only when enforcement on. |
| SNAP-021 | Cannot save during free trial / with ZDR on | snapshots__templates | ADAPTED | Trial N/A; ZDR rule REQUIRED. |
| SNAP-022 | Team images: saved under `--org` belong to org; any member deploys/updates; saver/owner/admin removes or shares; leaves → org keeps | snapshots__templates | REQUIRED | |
| SNAP-023 | Deploy by another member/account scrubs saver's Claude/Codex/GitHub logins, SSH keys, env secret files, API keys, repo clones | snapshots__templates | REQUIRED | Inspector/first-boot scrub via guest using a recorded list of injected paths. |
| SNAP-024 | Share codes `bsh_` (256-bit, shown once, hash stored) + share id `shr_`; list (use counts, status), revoke; re-save changes what code deploys; removing name removes codes | snapshots__templates, api share pages | REQUIRED | Works between accounts on the same coordinator. Codes are `bsh_` + 52 base32 chars; legacy `csh_` codes still resolve. |
| SNAP-025 | Wrong/revoked code → `404 snapshot_not_found` | snapshots__templates | REQUIRED | |
| SNAP-026 | Scopes: share create/revoke `snapshot.write`, list `snapshot.read`, deploy `sandbox.create`+`snapshot.read` | snapshots__templates | REQUIRED | |
| SNAP-027 | List snapshots across sandboxes (`GET /snapshots`, uncapped cursor) and per sandbox; `latest` | snapshots__download-and-delete, api | REQUIRED | |
| SNAP-028 | `GET /snapshots/{id}/tree` flat entries (file/dir/symlink, sizes), works while stopped | snapshots__download-and-delete | REQUIRED | Inspector VM or offline qcow2 reader on a node that has the chain. |
| SNAP-029 | `GET /snapshots/{id}/files?path=` file bytes or folder as tar; absolute paths for system files; `__dockervol__/` prefix; machine never contacted | snapshots__download-and-delete, api get-snapshot-file | REQUIRED | |
| SNAP-030 | `GET /snapshots/{id}/download` signed chunk URLs for full chain + inventory + `reconstruct`; `409 snapshot_compacted` | api get-snapshot-download | ADAPTED | Signed coordinator URLs to qcow2 layers (+ manifest); CLI `pull` reassembles via qemu-img/own reader. |
| SNAP-031 | `snapshot pull` writes `home_user/` and `docker/`; `--path` filter; Windows symlink skip | snapshots__download-and-delete | REQUIRED | |
| SNAP-032 | Download a saved snapshot via its `snapshot.snapshotId` | snapshots__download-and-delete | REQUIRED | |
| SNAP-033 | Delete one/many ordinary snapshots, `--all <sandbox>` keeps sandbox; data kept while a newer snapshot builds on it | cli-reference, data-retention__delete | REQUIRED | |
| SNAP-034 | `GET /named-snapshots*` (legacy alias of saved snapshots incl. allowance) | boat-v1.yaml | REQUIRED | Alias routes. |
| SNAP-035 | Snapshots tab in dashboard (browse, download) | snapshots__download-and-delete | REQUIRED | |
| SNAP-036 | Snapshots off per account / per sandbox (`snapshots:false`): never saved, stop erases | snapshots__stop, data-retention__snapshots-and-monitoring-off | REQUIRED | |

## Environments, secrets, repositories, registries, setup

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| ENV-001 | Environment = repos + vars/secret files + repo setup scripts + credentials + AI credential profile | environments | REQUIRED | |
| ENV-002 | Default environment `base` auto-created on first access; exactly one default; ≥1 always kept | environments, api list/delete-environment | REQUIRED | |
| ENV-003 | Create (`POST /environments {name}`; v1, all toggles on, safeForThirdParties off) | api create-sandbox-environment | REQUIRED | |
| ENV-004 | Update (`PUT /environments/{id}`: name, isDefault, safeForThirdParties, passGithub, passSecrets, passSandboxCredentials, passAgentsCredentials, credentialProfileId, proxyAiCredentials, access, envContents, secretFiles, repositories[]) | api update-sandbox-environment | REQUIRED | `repositories` PUT replaces list incl. `setupScript`, `setupBlocking`. |
| ENV-005 | Soft delete; pinned sandboxes keep running; default not deletable | environments__manage | REQUIRED | |
| ENV-006 | Immutable versions: every change mints a version; sandboxes pinned at start; no auto-upgrade | environments__versions | REQUIRED | |
| ENV-007 | `POST /environments/{id}/upgrade {agentIds?}`: live sandboxes hot-updated (withheld secrets deleted from disk), stopped ones at resume | environments__versions, api upgrade | REQUIRED | Guest applies config push; delete-not-hide semantics. |
| ENV-008 | List shows latest version, per-version sandbox counts (Versions panel), `effective` flags, `warning` when values are withheld | environments__access, environments__versions | REQUIRED | |
| ENV-009 | `environment` + `environmentVersion` on sandbox (`info` shows `prod (v2)`) | environments__versions | REQUIRED | |
| ENV-010 | Select per create/resume/fork by name; unknown name refused before any change; never auto-created | environments__manage | REQUIRED | |
| ENV-011 | Org environments via `?org=` only (not header/active wallet); org API key always uses org envs; member use, owner/admin manage; personal same-name wins | environments__manage | REQUIRED | |
| ENV-012 | `access` lists: everyone / only listed members | environments, cli-reference | REQUIRED | |
| ENV-013 | Safe-for-third-parties master switch: withholds vars, secret files, GitHub, credentials, agent logins, setup scripts | environments__access | REQUIRED | |
| ENV-014 | `noEnv` per sandbox (permanent; same as safe env); sticky across fork/resume/saved snapshot | environments__access | REQUIRED | |
| ENV-015 | Neutral vars in every sandbox: `BOAT_ID`, `ASCII_TOKEN` (acts on this sandbox only) | environments__access | ADAPTED | Inject `CROWDVM_ID`/`CROWDVM_TOKEN` and also `BOAT_ID`/`ASCII_TOKEN` aliases for tool compat. |
| ENV-016 | Normal sandbox gets: env vars, GitHub token, model creds (`OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, `CHATGPT_ACCOUNT_ID`), secret files, `~/.config/gh/hosts.yml`, git creds, `~/.claude/.credentials.json`, `~/.codex/auth.json`, in-sandbox CLI token | environments__access | ADAPTED | GitHub token = user-stored PAT (no GitHub App); CLI token at `~/.config/crowdvm/config.json`. |
| ENV-017 | Protected home defaults (Claude/Codex/OpenCode skills+MCP config, Chrome prefs, per-sandbox desktop key) with no credentials | environments__access | REQUIRED | Baked into image. |
| ENV-018 | Protect existing sandbox: `resume/fork --no-env` scrubs managed `.bashrc` blocks, gh hosts + `gh auth logout`, `~/.git-credentials`, `~/.ssh/id_*`, Claude/Codex creds, CLI token, secret files; keeps authorized_keys/known_hosts; leaves aws/gcloud/.netrc/.npmrc/docker logins; `sandbox_securing` while running; one-way | environments__access | REQUIRED | Guest scrub before marking reachable. |
| ENV-019 | Four toggles: `--github`, `--secrets`, `--boat-credentials`, `--agents-credentials` | environments__access | ADAPTED | `--github` = stored git credential + repo clones + setup scripts. |
| ENV-020 | `credentialProfileId` (null = owner default) and `proxyAiCredentials` (needs proxy on, else `400 ai_proxy_off`) | environments__access | REQUIRED | |
| ENV-021 | Env vars exported to exec, ssh and every shell; secret files written under /home/user at relative path; absolute/escaping paths skipped | environments__secrets | REQUIRED | |
| ENV-022 | `PUT/DELETE /environments/{id}/vars/{key}`, `PUT/DELETE /secret-files` (one item, new version) | environments__secrets, api | REQUIRED | |
| ENV-023 | Legacy `GET/POST /secrets {envContents, secretFiles}` replaces default env contents | environments__secrets, api get/update-boat-secrets-setup | REQUIRED | |
| ENV-024 | Per-sandbox `env` merged over environment (sandbox wins), also on no-env; key regex `[A-Za-z_][A-Za-z0-9_]*`, ≤128 chars, ≤100 vars, ≤64 KB, reserved names rejected (`invalid_env`) | environments__secrets | REQUIRED | |
| ENV-025 | Fork/deploy inherits per-sandbox vars unless own `env` sent; resume `-e` replaces | environments__secrets, cli-reference | REQUIRED | |
| ENV-026 | Private registries via vars/secret files: npm/pnpm `.npmrc`+`NPM_TOKEN`, pip conf/`PIP_INDEX_URL`, uv vars, Docker `~/.docker/config.json` or setup-script `docker login` | environments__private-registries | REQUIRED | Pure composition of ENV-021. |
| ENV-027 | Repositories cloned into `/home/user/<repo name>` at base branch (default `main`); never creates branches; agent cwd = repo when one repo | environments__repositories, recipes__agent-on-repo | ADAPTED | Repos = git URL (GitHub via user PAT, LAN Gitea/GitLab, or any reachable remote); clone uses stored credential. |
| ENV-028 | GitHub connection via GitHub App install ("Connect GitHub" for Google/email users) | environments__repositories | ADAPTED | User adds a git host + token in dashboard; no OAuth/GitHub App. |
| ENV-029 | `GET /repos?sync&limit&cursor&sort&q&selected` (repos grouped by installation, `databaseId`), `POST /repos {repositoryId,baseBranch}` (legacy, default env) | api list-github-repositories, api select-repository | ADAPTED | Lists repos from configured git host APIs using stored tokens (GitHub REST with PAT, Gitea API); installation = credential. |
| ENV-030 | `POST /environments/{id}/repos {repositoryId,baseBranch}`, `DELETE …/repos/{repositoryId}` | api add/delete-environment-repo | ADAPTED | Accept repo id from ENV-029 or raw git URL. |
| ENV-031 | Repo setup script: every start (create/resume/fork), repo cwd, login bash with `set -e`, after secrets, not when safe/no-env/GitHub off; failure output in `/tmp/routine-logs/` | setup | REQUIRED | |
| ENV-032 | Blocking setup script holds agent prompts until done (commands/SSH unaffected) | setup | REQUIRED | |
| ENV-033 | Dashboard editor for setup scripts (Setup button per repo, Blocking checkbox) | setup | REQUIRED | |
| ENV-034 | Per-sandbox `--setup-file`/`setupScript` (≤64 KB UTF-8, once, background after ready, as `user`; `setupStatus`/`setupError`; `invalid_setup_script`) | setup | REQUIRED | |
| ENV-035 | `files` on create (≤32 text files, 256 KB, before ready; `invalid_files`) | cli-reference | REQUIRED | |
| ENV-036 | Onboard answer "for other people" sets safe-for-third-parties on `base`; `safetyAnsweredAt` | environments, cli-reference | REQUIRED | |
| ENV-037 | Dashboard Environment tab (sections, toggles, repos, versions, upgrade) | environments | REQUIRED | |

## Integrated agents (providers, conversations, steering, computer use, customize, keys)

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| AGT-001 | Seven harnesses preinstalled and kept updated: Claude Code, Codex, pi, OpenCode, Prime Agent, Kimi Code, Mistral Vibe | integrated-agents | REQUIRED | Installed in base image; updated with image rebuilds (node `image build`). Harnesses not publicly installable are best-effort and must report `provider_not_configured`/unavailable cleanly. |
| AGT-002 | Always-on in-sandbox agent server; one harness process per conversation, as `user` in /home/user | integrated-agents | REQUIRED | Part of crowdvm-guest. |
| AGT-003 | `POST /prompt {prompt, provider, model, reasoningEffort, fast, new, conversationId}` → 202 `promptId`, `conversationId` | api prompt-sandbox-agent, boat-v1.yaml | REQUIRED | |
| AGT-004 | Provider ids `claude\|claude-code, codex, pi, opencode, prime\|prime-agent, kimi\|kimi-code, mistral`; default harness/model from dashboard | integrated-agents__harnesses-and-models, boat-v1.yaml | REQUIRED | |
| AGT-005 | `GET /provider-models` live catalog (models, reasoning levels, `fastMode`) used by `prompt --help` | integrated-agents__harnesses-and-models | REQUIRED | Not in OpenAPI; coordinator-maintained JSON catalog (admin-editable). |
| AGT-006 | `fast` mode only for fast-capable models; `400 fast_mode_not_supported` listing models; default from dashboard; `fast:false` forces standard | integrated-agents__harnesses-and-models | REQUIRED | |
| AGT-007 | `409 provider_not_configured` when no usable credential | integrated-agents, integrated-agents__keys | REQUIRED | |
| AGT-008 | Conversations: own memory (native harness session), one turn at a time per conversation, parallel across conversations | integrated-agents__conversations | REQUIRED | |
| AGT-009 | Per-sandbox parallel-turn cap scaled by RAM (~24 on default), queueing above it; override `ASCII_MAX_PARALLEL_CONVERSATIONS` | integrated-agents__conversations | REQUIRED | Honour `ASCII_MAX_PARALLEL_CONVERSATIONS` and `CROWDVM_MAX_PARALLEL_CONVERSATIONS`. |
| AGT-010 | Current conversation is per shell (CLI) / `current` = most recently prompted per sandbox (API) | integrated-agents__conversations | REQUIRED | |
| AGT-011 | `GET /conversations` (id, prompt count, running, last harness/model, last prompt preview, `current`) | api list-sandbox-conversations | REQUIRED | |
| AGT-012 | Prompt lifecycle `sending→queued→running→finished\|failed\|interrupted`; `GET /prompts/{promptId}` with `done` | integrated-agents__conversations, api get-prompt-run-status | REQUIRED | |
| AGT-013 | `GET /events?limit&cursor&sort&type&conversation` (+ `If-None-Match`/304): `prompt`, `response` (`content`, `is_streaming`, `tools`), `steer`, `error`, `completion`, unknown types; cursor = base64url(`<ts>:<id>`); ≤100 per page; no SSE — long-poll | integrated-agents__conversations, api list-sandbox-events | REQUIRED | Events persisted on coordinator so they survive stop. |
| AGT-014 | Conversations + harness sessions survive stop/resume/fork (on disk) | integrated-agents__conversations | REQUIRED | |
| AGT-015 | Switch harness/model mid-conversation: transcript frozen and replayed into new harness | integrated-agents__conversations | REQUIRED | |
| AGT-016 | `POST /steer {message, conversation}` → `native`, `mode`; native for Claude Code/Codex/pi/Prime; fallback (interrupt + continue) for OpenCode/Kimi/Vibe; `native-continued` / `late` modes; delivery guaranteed; `409 no_running_turn` | integrated-agents__steering, api steer | REQUIRED | |
| AGT-017 | `POST /interrupt[?conversation=]` (all by default; files kept, memory kept) | integrated-agents__steering, api interrupt | REQUIRED | |
| AGT-018 | Running prompt extends auto-stop | long-running-tasks__auto-stop | REQUIRED | |
| AGT-019 | Attachments (`--attach`) → `~/attachments`, images inline to vision models | cli-reference | REQUIRED | Upload via files API, then `attachments` on `POST /prompt`: harnesses with native attachments get them (images to vision models); the prompt event carries `attachments` (filename, mimeType, base64Data up to 256 KiB). |
| AGT-020 | Blocking repo setup script holds prompts | setup | REQUIRED | |
| AGT-021 | Prompts queue while sandbox starts | recipes__parallel-agents | REQUIRED | |
| AGT-022 | Computer use: Cua Driver MCP server `computer` registered for all 7 harnesses (files per harness, merged, re-written each start); daemon `cua-driver.service` user unit; socket under /run | integrated-agents__computer-use | REQUIRED | Ship cua-driver (open source) in image; keep `/opt/ascii/cua-driver` + `/run/ascii-cua/driver.sock` paths or symlinks. |
| AGT-023 | Tool set: get_window_state, get_desktop_state, get_screen_size, list_windows, list_apps, click, double_click, right_click, drag, scroll, move_cursor, type_text, press_key, hotkey, set_value, launch_app, kill_app, bring_to_front, set_window_frame, invoke_menu, browser_navigate/click/type, get_browser_state, browser_download, clipboard_read/write; background delivery + `delivery_mode:"foreground"` | integrated-agents__computer-use | REQUIRED | Provided by cua-driver. |
| AGT-024 | OpenCode `browser_prepare` disabled in `opencode.json` | integrated-agents__computer-use | REQUIRED | |
| AGT-025 | Platform system prompt (headless sandbox, CLI exists) + `boat` skill in every harness skills dir | integrated-agents__customize | ADAPTED | Platform note names the `crowdvm`/`host` CLI and the skill; the guest writes the `crowdvm` skill (repo `skills/crowdvm`) into every harness's skills dir at boot. |
| AGT-026 | House rules files per harness (`~/CLAUDE.md`, `~/AGENTS.md`, `APPEND_SYSTEM.md`, …) honored | integrated-agents__customize | REQUIRED | Native harness behaviour; agent server must start harness in /home/user (or the single repo). |
| AGT-027 | MCP servers via each harness's normal config; Claude Code MCP resource tools off | integrated-agents__customize | REQUIRED | |
| AGT-028 | Skills directories per harness; CLI tools on PATH; in-process extensions/hooks | integrated-agents__customize | REQUIRED | |
| AGT-029 | Bring your own harness: built-ins runnable directly; no locks | integrated-agents__customize | REQUIRED | |
| AGT-030 | Connect subscriptions (Claude Pro/Max, ChatGPT Plus/Pro/Team, Kimi Code, Mistral Le Chat, Grok) via vendor sign-in on dashboard | integrated-agents__keys | ADAPTED | No hosted OAuth client and no in-dashboard vendor sign-in button: the dashboard (Agents & Proxying) and `crowdvm ai profile sign-in` take what the harness's own login prints or writes (`claude setup-token`, `~/.codex/auth.json` from `codex login --device-auth`, the `kimi login` files) and store it as a credential; the server renews Claude/ChatGPT sign-ins (AIC-011). A sign-in flow run by the dashboard in a utility sandbox is not built. |
| AGT-031 | Server refreshes subscription tokens before every prompt/start; sandbox never gets refresh token where avoidable | integrated-agents__keys | ADAPTED | Refresh with stored refresh token where the vendor flow allows; else harness self-refresh. |
| AGT-032 | API keys per vendor: Anthropic, OpenAI, OpenRouter, LLM Gateway, DeepSeek, Moonshot, Mistral, xAI, OpenCode, Bedrock | integrated-agents__keys | REQUIRED | Stored encrypted, injected as env/auth files. |
| AGT-033 | Env var mapping: ANTHROPIC_API_KEY, OPENAI_API_KEY, OPENROUTER_API_KEY, LLMGATEWAY_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, MOONSHOT_API_KEY, MISTRAL_API_KEY, XAI_API_KEY, OPENCODE_API_KEY, KIMI_CODE_ACCESS/REFRESH_TOKEN, KIMI_CODE_REGION, DEEPSEEK_API_KEY, AWS_BEARER_TOKEN_BEDROCK+AWS_REGION(+CLAUDE_CODE_USE_BEDROCK) | integrated-agents__keys | REQUIRED | Agent server translates per harness. |
| AGT-034 | DeepSeek: Claude Code via `ANTHROPIC_BASE_URL`+`ANTHROPIC_AUTH_TOKEN` (empty API key), Codex via generated `model_providers.deepseek` block (responses wire API, web search off) | integrated-agents__keys | REQUIRED | |
| AGT-035 | Bedrock: Claude Code `CLAUDE_CODE_USE_BEDROCK`, restricted model list, default Sonnet 4.6; Codex `amazon-bedrock` provider via Mantle; clear "model access not granted" errors; Bedrock API key or IAM access keys (id, secret, optional session token) | integrated-agents__keys | REQUIRED | IAM keys: `accessKeyId`/`sessionToken` on the sign-in, `crowdvm ai profile sign-in --access-key-id/--session-token`, dashboard form; sandbox gets `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN`. |
| AGT-036 | Mistral (subscription preferred over API key), Grok/xAI (subscription preferred; access token only), OpenCode Go/pay-as-you-go model id prefixes | integrated-agents__keys | REQUIRED | |
| AGT-037 | "Get a key / Get a plan" links per credential row; CLI prints link before asking key | integrated-agents__keys | REQUIRED | |
| AGT-038 | Per-sandbox user keys with `--no-env -e …` | integrated-agents__keys, platform-guide__agents | REQUIRED | CrowdVM extension: `crowdvm prompt --local-login` (`CROWDVM_LOCAL_LOGIN`) sends the caller's own Claude/ChatGPT access token or vendor API key as the prompt's `credentials` for one turn (memory only in the guest, never a refresh token). |
| AGT-039 | Dashboard Agents & Proxying tab: connect, default harness/model, fast default, warnings when an environment blocks keys | integrated-agents__keys | REQUIRED | |
| AGT-040 | Daemon/your-own-loop patterns keep events, attachments, desktop, snapshots working | integrated-agents__customize | REQUIRED | |
| AGT-041 | Prompt-state webhooks/SSE explicitly absent (poll) | integrated-agents__conversations | N/A | Documented absence in Boat; nothing to implement. |

## AI credentials & proxy

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| AIC-001 | Credential profiles (encrypted at rest; API never returns secrets; `connected` list + custom API names) | ai-credentials | REQUIRED | |
| AIC-002 | One default profile per account; legacy credentials = profile `default`; sandbox gets owner default unless environment names one | ai-credentials | REQUIRED | |
| AIC-003 | `GET/POST /credential-profiles {name, org, copyFrom}`; `--copy-from` copies API keys + custom APIs, not sign-ins | ai-credentials, boat-v1.yaml | REQUIRED | |
| AIC-004 | `PUT /credential-profiles/{p} {name,isDefault,access}`; `DELETE` refused `409 is_default` / `409 in_use` | ai-credentials | REQUIRED | |
| AIC-005 | Credential ids: anthropic/openai/openrouter/llmgateway/deepseek/moonshot/mistral/xai/opencode `-api-key`; claude/chatgpt/kimi/mistral-vibe/grok `-subscription`; `claude-bedrock`, `codex-bedrock` | ai-credentials, boat-v1.yaml | REQUIRED | |
| AIC-006 | `PUT/DELETE /credential-profiles/{p}/keys/{cred} {value, keepCurrent}`; key = 1 token ≤8192 printable chars; sign-in sent to set-key → `400 not_an_api_key` | ai-credentials | REQUIRED | |
| AIC-007 | Running sandboxes on the profile get changed keys live | ai-credentials | REQUIRED | Hot config push via guest. |
| AIC-008 | Multiple accounts per credential: list (`active` first, email or last chars), add, switch (`PUT …/{account}/active`, all sandboxes switch at once without restart), forget (switch first) | ai-credentials | REQUIRED | |
| AIC-009 | Sign-ins renew only while in use | ai-credentials | ADAPTED | See AGT-031. |
| AIC-010 | Organization profiles (`owner.kind=org`, `canManage`), owner-only management, `access` limits | ai-credentials, organizations | REQUIRED | |
| AIC-011 | Claude credential env mapping (`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_BASE_URL/AUTH_TOKEN`); user `/login` inside sandbox suppresses these vars from next shell and is never removed | ai-credentials | REQUIRED | |
| AIC-012 | Custom APIs `PUT/DELETE …/custom-apis/{NAME} {value, baseUrl}` → `<NAME>` + `<NAME>_BASE_URL`; name `[A-Z0-9_]{1,64}`, reserved provider prefixes and `_BASE_URL` suffix rejected | ai-credentials | REQUIRED | |
| AIC-013 | Custom API base URL must be HTTPS, default port, public DNS name | ai-credentials | ADAPTED | Also allow LAN/Tailscale hosts, ports and `http://` when the admin starts the server with `--ai-custom-api-allow-private` (or `CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE=1`); otherwise `invalid_base_url` as in Boat. |
| AIC-014 | AI credentials proxy: placeholders + proxy base URL in sandbox; real keys only on coordinator; placeholders rotate per start and die at stop | ai-credentials | REQUIRED | Proxy runs on coordinator; sandbox reaches it via guest-forwarded loopback port (no public URL needed). |
| AIC-015 | Proxy sends each credential only to its own provider host; provider key only in `Authorization`/`x-api-key`; custom API secret in headers/query, never path | ai-credentials | REQUIRED | |
| AIC-016 | Proxy renews Claude/ChatGPT/Grok sign-ins; sandbox never sees renew token | ai-credentials | ADAPTED | Where the vendor token flow allows (AGT-031). |
| AIC-017 | Coverage: proxied = Claude/Anthropic, ChatGPT/OpenAI, OpenRouter, LLM Gateway, DeepSeek, Moonshot, Mistral (+Vibe), Grok/xAI, OpenCode, custom APIs; not proxied (withheld when proxy on) = Bedrock, Kimi sign-in | ai-credentials | REQUIRED | |
| AIC-018 | `GET/PUT /ai-proxy {enabled}` per account and per org (`?org=`/`X-Boat-Org`); org switch forces proxy for its sandboxes; applies at next start | ai-credentials | REQUIRED | |
| AIC-019 | `pricing` in proxy settings ($1/day on, $1 per 1000 requests) | ai-credentials | ADAPTED | Credits per day / per 1000 requests (default 0). |
| AIC-020 | Safe-for-third-parties env + `proxyAiCredentials` (needs proxy on else `400 ai_proxy_off`) | ai-credentials | REQUIRED | |
| AIC-021 | Usage per UTC day, credential, model: requests, errors, input/output/cache-read/cache-write tokens; `GET /ai-usage?days&profile&org`, `GET /sandboxes/{id}/ai-usage?days`; `dollars` = proxy charge | ai-credentials | REQUIRED | `dollars` in credits. |
| AIC-022 | Scopes: profiles/proxy need `account.admin`; account usage `account.read`; sandbox usage `sandbox.read` | ai-credentials | REQUIRED | |
| AIC-023 | `PUT /sandboxes/{id}/ai-access {blocked}`: proxy returns 403 for placeholders, works running or stopped; creator or profile managers only | ai-credentials | REQUIRED | |
| AIC-024 | Dashboard Agents & Proxying: profile picker, Proxying tab (switch, custom APIs, usage), per-sandbox "AI usage" menu | ai-credentials | REQUIRED | |
| AIC-025 | No automatic AI spend cap (documented) | ai-credentials | N/A | Documented absence. |

## Accounts, sign-in, agent auth

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| AUTH-001 | Sign-in with GitHub | account, quickstart | ADAPTED | Replaced by local accounts (username/email + password) on the coordinator; GitHub OAuth itself not offered (needs public callback + GitHub app). |
| AUTH-002 | Sign-in with Google | account | N/A | Third-party OAuth requiring public internet and a Google client; explicitly out of scope. Local accounts cover the need. |
| AUTH-003 | Sign-in with 6-digit email code; disposable domains refused | account, cli-reference | ADAPTED | Code delivered by optional SMTP or printed in server log; passwordless login kept. |
| AUTH-004 | Sign-in with AgentID (agentid.com / AgentMail inbox authorize) | account, agent-auth | N/A | Depends on third-party AgentID/AgentMail SaaS on the public internet. Agent self-registration (AUTH-010…) covers agents. |
| AUTH-005 | Connections: several methods on one account, add/remove (≥1 remains), primary connection = notification address | account | ADAPTED | Connections = password, email-code, passkey-less; primary email for notifications. |
| AUTH-006 | Unconnected method creates separate account; merges by support only | account | ADAPTED | Admin-only account merge tool on coordinator. |
| AUTH-007 | Browser sessions for dashboard; CLI device-code login (URL + code, 15 min) | cli-reference, account | REQUIRED | |
| AUTH-008 | `GET /me` → user (login, `connectionMethod` github\|google\|email\|agentid\|service, `accountStatus` active\|suspended\|closed) | boat-v1.yaml, api get-current-boat-user | ADAPTED | `connectionMethod` values `local`, `email`, `agent`, `service` (keep enum extensible). |
| AUTH-009 | Onboarding question "own work vs platform for third parties" (default platform → safe env) | account | REQUIRED | |
| AUTH-010 | Agent self-registration per auth.md (WorkOS protocol): `GET /auth.md` | agent-auth | REQUIRED | Served by coordinator with CrowdVM URLs. |
| AUTH-011 | `/.well-known/oauth-protected-resource` (RFC 9728) and `/.well-known/oauth-authorization-server` (RFC 8414 + `agent_auth`); also under `/api/boat/…` | agent-auth | REQUIRED | Also under `/api/crowdvm/…`. |
| AUTH-012 | `POST /agent/identity` anonymous (read-only token: only `/me`, `/limits`; create → `claim_required`) or email (`service_auth`: user_code + verification URL) | agent-auth | REQUIRED | |
| AUTH-013 | Claim ceremony `POST/GET /agent/identity/claim` (human signs in, types 6-digit code) | agent-auth | REQUIRED | Local account sign-in. |
| AUTH-014 | `POST /oauth2/token` (exchange/poll), `POST /oauth2/revoke` | agent-auth | REQUIRED | |
| AUTH-015 | Reject ID-JAG identity assertions | agent-auth | REQUIRED | |
| AUTH-016 | API keys cannot approve agent claims (browser session only) | api-keys__create-and-list | REQUIRED | |
| AUTH-017 | 401 carries `WWW-Authenticate: Bearer resource_metadata=…` | agent-auth | REQUIRED | |
| AUTH-018 | Agent trial via `POST /api/boat/billing/checkout` (Stripe) | agent-auth | ADAPTED | Claimed agent gets the account's credits; checkout endpoint returns "no payment required". |
| AUTH-019 | One trial per identity, trial-abuse checks | agent-auth, free-trial | N/A | No trials or payments in a self-hosted LAN pool. |
| AUTH-020 | Dashboard-only actions: connections, closing account, member roles, webhook delivery history, per-sandbox metrics; browser-session-only: rotate/revoke keys, enable ZDR | account | REQUIRED | |
| AUTH-021 | Account status suspended/closed enforcement | boat-v1.yaml | REQUIRED | Admin can suspend. |
| AUTH-022 | Service accounts (`serviceAccount`, `connectionMethod: service`) | boat-v1.yaml | REQUIRED | Admin-created service users. |
| AUTH-023 | Coordinator admin role (manage users, nodes, credits) | (CrowdVM) | REQUIRED | Needed to replace Boat staff/support actions. |

## API keys & scopes

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| KEY-001 | Bearer auth `Authorization: Bearer <key>`; prefix `boat_` (legacy `box_`) | api-keys | ADAPTED | Prefix `cvm_`; `boat_` and `box_` are accepted in place of `cvm_` (`auth.go`). |
| KEY-002 | Org keys vs account keys; org key restricted to its org (`403 api_key_org_mismatch`), no account settings (`403 api_key_org_forbidden`) | api-keys | REQUIRED | |
| KEY-003 | `POST /api-keys/scoped {name, ttl, preset\|actions, sandboxIds, environmentIds}` (+ `org`/`X-Boat-Org`) → secret once | api-keys__create-and-list | REQUIRED | |
| KEY-004 | TTL default 90 d, max 365 d, `never`; expiring key cannot mint non-expiring; new key ≤ creator's actions/sandboxes/expiry | api-keys__create-and-list | REQUIRED | |
| KEY-005 | No preset/actions → admin key | api-keys__create-and-list | REQUIRED | |
| KEY-006 | Creation needs browser session or `account.admin`/admin key | api-keys__create-and-list | REQUIRED | |
| KEY-007 | `catalog.scopedCreationEnabled` + typed 503 when creation disabled; catalog `actions, presets, defaultTtl, maxTtl, scopeResources` | api-keys__create-and-list, boat-v1.yaml | REQUIRED | Admin toggle. |
| KEY-008 | `GET /api-keys`: id, name, prefix, last4, scope, expiry/expired/expiringSoon, lastUsedAt, 30-day requests, live resources; limited keys get `[]`; `credentialLane` | api-keys__create-and-list, boat-v1.yaml | REQUIRED | |
| KEY-009 | `GET /api-keys/{id}/usage` (works after revoke; verbose split sandboxes/Agents + created list) | api-keys__create-and-list | REQUIRED | |
| KEY-010 | `POST /api-keys/{id}/rotate` (old secret dead, same id/scope/expiry; `409 api_key_expired`) | api-keys__store-rotate-revoke | REQUIRED | |
| KEY-011 | `DELETE /api-keys/{id}` revoke immediately; created sandboxes stay | api-keys__store-rotate-revoke | REQUIRED | |
| KEY-012 | Rotate/revoke: browser session or key that can create keys (may revoke itself/children) | api-keys__create-and-list | REQUIRED | |
| KEY-013 | Actions: sandbox.create/read/update/stop/resume/fork/delete/delete-own, agent.prompt, exec, file.read/write, ssh, desktop, host, snapshot.read/write, environment.read/write, account.read/admin, `*` | api-keys__scopes | REQUIRED | |
| KEY-014 | Presets read-only, full-sandbox, ci (incl. create/resume/fork/delete-own), admin; keys keep actions from creation time | api-keys__scopes | REQUIRED | |
| KEY-015 | Grant = action ∈ key AND target sandbox ∈ key sandboxes/environments; default deny; unknown routes refused for scoped keys | api-keys__scopes | REQUIRED | |
| KEY-016 | Errors `403 api_key_expired`, `api_key_action_forbidden`, `api_key_sandbox_forbidden` | api-keys__scopes | REQUIRED | |
| KEY-017 | In-sandbox per-sandbox key in `~/.config/ascii/boat/config.json` (own sandbox: read/rename/stop, prompt, exec, files, ssh, desktop, host, snapshots read/save; nothing else); hidden unless `list --all`; removed by safe/no-env/credentials toggle | api-keys__scopes | ADAPTED | `~/.config/crowdvm/config.json` (+ legacy path symlink). |
| KEY-018 | `ASCII_TOKEN` host-only token always present | api-keys__scopes | ADAPTED | `CROWDVM_TOKEN` + `ASCII_TOKEN` alias. |
| KEY-019 | Legacy unscoped keys with no expiry | api-keys | N/A | No legacy keys exist in a new system. |
| KEY-020 | Dashboard API Keys tab (expiry presets, action presets, raw picker, sandbox/env selectors, Revoke) | api-keys__create-and-list | REQUIRED | |

## Organizations / teams

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| ORG-001 | Create org (creator = owner), ids `team_…`; aliases org/team/teamId | organizations | REQUIRED | |
| ORG-002 | Invite by email (owner); invite link → sign in → member; `org members` shows pending invites | organizations | ADAPTED | Invite link shown in CLI/dashboard and optionally emailed via SMTP; accepted by a local account. |
| ORG-003 | Transfer ownership; delete org (`--yes`) | organizations, cli-reference | REQUIRED | |
| ORG-004 | Rename (owner) `PATCH /orgs/{org} {name}`; id stable; old name stops resolving; billing name/Stripe 502 | api update-organization | ADAPTED | Rename kept; invoice/Stripe parts dropped. |
| ORG-005 | Roles owner/admin/member; remove members, change roles (dashboard only) | organizations, account | REQUIRED | |
| ORG-006 | `GET /orgs` (personal first, then orgs: id, name, type, role, memberCount, active) | api list-organizations | REQUIRED | `subscriptionStatus` → credit/enforcement status. |
| ORG-007 | Active wallet `PATCH /orgs/active {org\|personal\|null}` shared by CLI/API; dashboard uses sidebar selection; org key bills its org; wallet fixed at create; fork keeps source wallet | organizations__active-wallet | ADAPTED | "Wallet" = credit account (personal or org). |
| ORG-008 | Org naming by id, case-insensitive name, `personal`; `409 ambiguous_org` | organizations__active-wallet | REQUIRED | |
| ORG-009 | Per-request override: `--org`, `X-Boat-Org`, `?org=`, body `org`/`teamId` | organizations__active-wallet | REQUIRED | |
| ORG-010 | Org scope lists every sandbox the org pays for with `createdBy`, `visibility`, `access` (owner/use/view) | organizations__shared-sandboxes | REQUIRED | |
| ORG-011 | `use` access = open, SSH, exec, files, prompt, ports, resume, stop; `view` = see + usage (+stop unless creator-private) | organizations__shared-sandboxes | REQUIRED | |
| ORG-012 | Share/unshare (`POST /share`, `/unshare`): creator only; unshare removes teammates' SSH keys; history recorded while private hidden from teammates | organizations__shared-sandboxes | REQUIRED | |
| ORG-013 | Sharing a sandbox holding creator logins → becomes noEnv, logins deleted at next start, `restartRequired:true` while running | organizations__shared-sandboxes | REQUIRED | |
| ORG-014 | Default visibility (shared default; owners/admins set private) via `PATCH /orgs/{org} {newSandboxesPrivate}`; fork of private is private | organizations__shared-sandboxes | REQUIRED | |
| ORG-015 | Sandboxes that pass no logins are shared and never receive logins later (`409 environment_passes_logins`) | organizations__shared-sandboxes | REQUIRED | |
| ORG-016 | Errors `403 sandbox_private`, `403 owner_only`; creator-only actions: fork, delete, settings, data-destroying stop, stop of private | organizations__shared-sandboxes | REQUIRED | |
| ORG-017 | Org environments/profiles with access lists (`access{restricted,members}`), `403 environment_forbidden`, `400 invalid_access` | organizations | REQUIRED | |
| ORG-018 | Member leaving → their sandboxes revert to personal billing; org keeps their team images | organizations, snapshots__templates | REQUIRED | |
| ORG-019 | Seats priced per plan; seat count purchase `PUT /api/boat/teams/{org}/seats`; seats raise limits | organizations__seats-and-limits | ADAPTED | No seat purchase. Org limits/credit quota set by admin; optional "seats" = member count only. |
| ORG-020 | Shared org pool for start/concurrency limits | organizations__seats-and-limits | REQUIRED | Per-org limits on coordinator. |
| ORG-021 | Per-member caps (usage cap per window, concurrent sandboxes) set by owner/admin; `402 team_member_cap_reached`, `429 member_limit_reached`; over-cap sandboxes snapshotted+stopped within ~1 min; counts against creator | organizations__seats-and-limits | ADAPTED | Usage cap in credits. |
| ORG-022 | Move personal plan into org (`org migrate`, `billing plan N --move-personal`, `POST /api/boat/billing/move-to-org`), refunds | organizations__move-personal-plan | ADAPTED | Moves personal sandboxes to org billing; optional credit transfer; no refunds. |
| ORG-023 | Organizations get no free trial | organizations | N/A | No trials. |
| ORG-024 | Dashboard org tab, Viewing dropdown, sidebar "CLI and API keys bill … Change", row labels by/view only/private/shared | organizations, organizations__active-wallet | REQUIRED | |
| ORG-025 | Account API `/api/boat/teams/{org}/…` (dashboard-internal) | organizations | ADAPTED | CrowdVM has its own internal dashboard API; not a parity surface. |

## Billing & limits (→ credits)

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| BILL-001 | Per-second metering while running (from `provisioned`), size multiplier small 0.5 / default 1 / large 2 / xlarge 5.56 | pricing, billing | ADAPTED | Credits ledger: running sandbox debits `seconds × multiplier`; nodes earn credits for hosted VM-seconds; admin grants. Enforcement optional, off by default (ARCHITECTURE). |
| BILL-002 | Plans ($20/$100/$500/$2000) with included VM time, Stripe subscription | pricing, billing | ADAPTED | Replaced by per-account/org credit quotas (admin-set). No subscriptions. |
| BILL-003 | Wallet in USD paying overage; can go negative during grace | billing | ADAPTED | Credit balance; may go negative during grace. |
| BILL-004 | Wallet pay ($15–$100 000), bonus table, auto top-up (cards, 10% trigger, 3-decline disable), card management | billing | N/A | Real-money payments are out of scope (no Stripe/cards). Admin credit grants replace them (BILL-005). |
| BILL-005 | (replacement) Admin grants/revokes credits; transfer between personal and org | (CrowdVM) | ADAPTED | Required substitute for BILL-004. |
| BILL-006 | Running out: 24 h grace, email, then stop (snapshot) running sandboxes; earlier stop past 25% plan / $50 new debt; resume after funding | billing | ADAPTED | Same flow on credit balance when enforcement is on; notification via dashboard/SMTP. |
| BILL-007 | `402 billing_required` with `billingUrl` | limits | ADAPTED | `billingUrl` → dashboard credits page; only when enforcement on. |
| BILL-008 | Concurrency limit per account/org ("at once"), `429 limit_reached`; stopped sandboxes don't count | limits | REQUIRED | Admin-configurable values. |
| BILL-009 | Start limits per minute/hour/day over rolling windows; `429 rate_limited {window, limit, cause, nextStartAt, retryAfterSeconds}` + `Retry-After`; failed start not counted; never queued | limits | REQUIRED | `cause` ∈ plan/cap/ceiling (no `trial`). |
| BILL-010 | Extra starts (priced brackets $0.03–$0.07, ≤6× included, ≤3×/min), paid by wallet or invoice (`extra-starts on`, monthly cap) | limits | ADAPTED | Extra starts cost credits; invoice option N/A (no invoices). |
| BILL-011 | `GET /limits` fields: canStart, startBlockedReason, activeSandboxes, maxActiveSandboxes, starts.{minute,hour,day}.remaining, startLimits, startLimitsWithWallet, nextStartDollars, startWindowFull, planNudge, machineTypes (allowed + reasons), wallet.balanceDollars, planIncluded.vmHours(Remaining), overagePrices, teamId/teamRole | limits, pricing, boat-v1.yaml | ADAPTED | Same shape; money fields expressed in credits; Stripe/subscription fields null. `startWindowFull` is an object only while a window is full (`window, limit, freesAt, paidBy, message, help`); `startLimitsWithWallet` = the extra-start ceilings when extra starts are on. |
| BILL-012 | Deprecated `pack…`/`creditBalanceHours` fields | pricing | N/A | Deprecated legacy fields; nothing to keep compatible with. |
| BILL-013 | `IP_RATE_LIMIT_EXCEEDED` 10 000/60 s per IP + `RateLimit-Policy`/`RateLimit` headers | limits | REQUIRED | |
| BILL-014 | `503 out_of_capacity` (retryAfterSeconds 120) | limits | REQUIRED | No node has capacity. |
| BILL-015 | Prompt attachments limit 12 files, 25 MB each, 64 MB total | limits | REQUIRED | |
| BILL-016 | Webhooks max 10 per account | limits | REQUIRED | |
| BILL-017 | `GET /sandboxes/{id}/usage?since&until` → seconds (multiplier applied), dollars, secondsPerDollar, sandboxType, billingMultiplier, running; refused-stop time excluded; month split | billing | ADAPTED | `dollars` = credits. |
| BILL-018 | `GET /sandboxes?include=cost,deletedCost` (month-to-date cost per sandbox + deleted total) | billing | ADAPTED | In credits. |
| BILL-019 | Dashboard Billing: Cost column, cost by sandbox this month, deleted-sandboxes line | billing | ADAPTED | Credits page in dashboard. |
| BILL-020 | Plan changes, proration, refunds, VAT, invoices, Stripe portal, cancel/keep-disks 90 d | billing__plan-changes | N/A | All tied to real payments (Stripe); no payment system in scope. |
| BILL-021 | Free trial (7 d, card, 25 h, 2 at once, 2 h auto-stop, small/default, no saved snapshots; trial error codes, `trial_blocked`) | free-trial | N/A | No paid plans → no trial. Equivalent restrictions available via BILL-022 policy. |
| BILL-022 | (replacement) Per-account restriction policy (max TTL, allowed sizes, saved-snapshot allowance, concurrency) | free-trial, limits | ADAPTED | Admin-set policy reusing Boat error codes where meaningful. |
| BILL-023 | `planNudge` emails / Alerts tab | limits | ADAPTED | Optional low-credit alert in dashboard. |
| BILL-024 | Price comparison / performance benchmarks vs other providers | pricing | N/A | Marketing content. Node benchmark scores (SIZE-015) are the CrowdVM analogue. |
| BILL-025 | No egress billing | pricing | REQUIRED | Not metered. |
| BILL-026 | Spend controls: per-member caps only, no personal $ cap | billing | ADAPTED | Optional per-account credit cap. |

## Webhooks

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| HOOK-001 | Account-wide webhooks: `POST/GET /webhooks`, `GET/PATCH/DELETE /webhooks/{id}`, `POST /webhooks/{id}/rotate`; `account.admin` | webhooks, api | REQUIRED | |
| HOOK-002 | API requires ≥1 event (full names); CLI accepts short names and defaults to all 6 | webhooks | REQUIRED | |
| HOOK-003 | Signing secret `whsec_…` shown once (create/rotate) | webhooks | REQUIRED | |
| HOOK-004 | Endpoint must be HTTPS:443 resolving only to public IPs; no redirects | webhooks | ADAPTED | Allow `http://` and private/LAN/Tailscale (100.64/10) addresses — the only addresses that exist in scope; loopback, link-local (incl. 169.254.169.254) and cloud-metadata targets are refused (400 `invalid_url`, checked on save and on every connection) unless the admin starts the server with `--webhook-allow-local`; never follows redirects. Same guard for OTel endpoints. |
| HOOK-005 | Max 10 endpoints per account | webhooks | REQUIRED | |
| HOOK-006 | Events `sandbox.ready`, `.hydrated`, `.error`, `.archived`, `.degraded` (~1 min after unreachable, `data.reason`), `.recovered` | webhooks | REQUIRED | Node loss: the sandbox enters `error` with `sandbox.degraded` + `sandbox.error` (`node_offline`); when the node returns, `sandbox.ready` + `sandbox.recovered`; a rebuild on another node ends with `sandbox.ready` + `sandbox.recovered`. |
| HOOK-007 | ready/error/archived created in same DB transaction as the state change (outbox) | webhooks | REQUIRED | |
| HOOK-008 | Payload `{id: evt_<32hex>, type, createdAt, data:{sandbox{id,name}, previousState, state, reason}}` | webhooks, boat-v1.yaml | REQUIRED | |
| HOOK-009 | Headers `X-Ascii-Event`, `X-Ascii-Delivery`, `X-Ascii-Timestamp`, `X-Ascii-Signature: v1=<hex HMAC-SHA256(delivery.timestamp.raw)>`, `X-Ascii-Attempt` (1–8) | webhooks, boat-v1.yaml | REQUIRED | Keep `X-Ascii-*` names for verifier compatibility (optionally duplicate as `X-CrowdVM-*`). |
| HOOK-010 | At-least-once, out-of-order allowed; 2xx within 5 s; exponential backoff up to 8 attempts | webhooks | REQUIRED | |
| HOOK-011 | Delete removes queued deliveries; rotation overlap documented | webhooks | REQUIRED | |
| HOOK-012 | Delivery records kept 30 days; dashboard Deliveries log (event, payload, ids, attempts, HTTP result/diagnostic, next retry) | webhooks | REQUIRED | |
| HOOK-013 | Hydrated sequence: containers' volume mounts moved in place (0.1 s pause) before `sandbox.hydrated` | webhooks | ADAPTED | Block-level restore has no volume move; `hydrated` fires when chain is local. |

## OpenTelemetry

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| OTEL-001 | One OTLP/HTTP endpoint per account: `PUT /telemetry {endpoint, headers}` (≤20 headers, encrypted, names only returned), `GET`, `DELETE` (drops queue); `account.admin` | opentelemetry, api | REQUIRED | |
| OTEL-002 | Test export on save returned in `test{ok,status,message}`; `lastExportAt`, `lastError` | opentelemetry | REQUIRED | |
| OTEL-003 | Base URL normalisation (strip `/v1/metrics`); paths `/v1/metrics`, `/v1/logs` | opentelemetry | REQUIRED | |
| OTEL-004 | Endpoint HTTPS on 443/4318, public addresses only | opentelemetry | ADAPTED | Allow LAN/Tailscale addresses and plain HTTP (local collectors). |
| OTEL-005 | Metrics every 60 s, gauges: `boat.sandbox.cpu.utilization`, `memory.utilization`, `memory.usage`, `filesystem.utilization`, `filesystem.usage`, `io.pressure`, `boat.account.sandboxes.active`, `.limit`, `boat.account.credit.balance` | opentelemetry | ADAPTED | Same metric names (keep `boat.` prefix for dashboards, or `crowdvm.` with config switch); `credit.balance` = credits. |
| OTEL-006 | Resource attrs `service.name`, `service.instance.id`, `boat.sandbox.id/name/size/memory.size/disk.size`, `boat.account.id` | opentelemetry | ADAPTED | Same attributes with Boat's `boat.` names (`boat.sandbox.*`, `boat.account.id`) so existing dashboards and alerts keep working; `service.name` is `crowdvm`, not `boat`. |
| OTEL-007 | Lifecycle events as OTLP log records with severity map and attrs (`state`, `previous_state`, `reason`, `boat.event.id`) | opentelemetry | REQUIRED | |
| OTEL-008 | Retry on 429/502/503/504/timeouts honouring Retry-After; drop on other errors; ≤10 min metric backlog; events retried 8× | opentelemetry | REQUIRED | |
| OTEL-009 | Monitoring-off sandboxes send no metrics (events still sent) | opentelemetry | REQUIRED | |
| OTEL-010 | No traces, no agent/AI token data | opentelemetry | REQUIRED | Same exclusion. |
| OTEL-011 | Dashboard Webhooks & OTel tab | opentelemetry | REQUIRED | |

## Data retention & deletion

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| DATA-001 | Retention table: newest snapshot while running / ≥14 d after stop; older snapshots <1 d; saved snapshots no expiry; history 90 d / 72 h; closed account 30 d | data-retention | ADAPTED | Same defaults, admin-configurable; retention job may run report-only (Boat's production mode) or enforce. |
| DATA-002 | `DELETE /sandboxes/{id}` with `X-Ascii-Confirm-Delete: <id>` (else 409), 202 + operation; running → stop without snapshot; billing ends | data-retention__delete | REQUIRED | Keep header name (also accept `X-CrowdVM-Confirm-Delete`). |
| DATA-003 | Deletion operations `bdop_…`: status pending/processing/blocked/completed, stage removing/waiting_for_uploads/retrying/completed, `expectedBy`, reason explicit/zdr/account; `GET /deletion-operations/{id}`; not cancellable | data-retention__delete, boat-v1.yaml | REQUIRED | An operation reports `completed` only when the node confirmed the machine is removed and every snapshot copy (server blob and node caches) is erased or still read by another snapshot, fork or saved snapshot (snapshot-kind operations have no machine to remove and complete once their snapshot is purged); an offline or failing node keeps it `processing`/`retrying` (retried every pass and on reconnect), `blocked` after 24 h (`deletion_ops.go`). While a node still uploads a snapshot of the target (node `GET /v1/snapshots?uploading=true`) the stage is `waiting_for_uploads` and GC does not purge it; an upload of a deleted sandbox the server has no row for gets a deleted row with the node as location; uploads of purged data or deleted sandboxes are refused (410) and the node erases its copy. Orphan blobs (no or purged row) are swept every 10 min, node copies of deleted data on reconnect. A node removed while offline leaves its copies recorded: the op turns `blocked` (`node_removed`). `expectedBy` stays null. `lastError` (extension) says why an op is not completed. |
| DATA-004 | Gone from lists immediately; data erased when nothing reads it (refcounted chains: forks, resumes, saved snapshots keep data) | data-retention__delete | REQUIRED | Coordinator + node blob GC by refcount. |
| DATA-005 | `DELETE /snapshots/{id}` and `DELETE /sandboxes/{id}/snapshots` with confirm header; `DELETE /saved-snapshots/{name}` without | data-retention__delete | REQUIRED | |
| DATA-006 | Saved-snapshot data erased ≥6 h after removal (upload-link expiry window) | data-retention__delete | ADAPTED | Window = max lifetime of node upload tokens (configurable, may be shorter). |
| DATA-007 | `Cache-Control: no-store` on deletion/retention responses | data-retention__delete | REQUIRED | |
| DATA-008 | ZDR: `GET /account/data-retention` (any key; also in `/me` `zeroDataRetention`), `PATCH {enabled, confirmation:"delete archived sandbox data"}` browser-session only (`403 session_required`); deletes stopped sandboxes, removes saved snapshots, every stop deletes; still snapshots while running; per-person in orgs | data-retention__zero-data-retention | REQUIRED | |
| DATA-009 | ZDR: process output after last 30 s read not saved | data-retention | REQUIRED | |
| DATA-010 | Snapshots off (account `snapshotsOff` via session-only PATCH, or per sandbox `snapshots:false`): never captured, stop erases, no resume/fork/save; template-origin snapshots listed with `origin:"template"` | data-retention__snapshots-and-monitoring-off | REQUIRED | |
| DATA-011 | Monitoring off (account `monitoringOff` or `monitoring:false`): no history/process list/metrics, no tracer; live output still works; `409 monitoring_off`; account setting refuses `monitoring:true` (`409 monitoring_off_for_account`); fork inherits, override with `monitoring:true` | data-retention__snapshots-and-monitoring-off | REQUIRED | |
| DATA-012 | Close account (dashboard session only): stops sandboxes (with save), revokes keys/sessions except current, confirmation email with purge date; reopen within 30 d; purge after | data-retention__close-account | ADAPTED | Subscription cancel part N/A; email via SMTP/dashboard notice. |
| DATA-013 | Permanent erasure (GDPR) immediately, not reopenable | data-retention__close-account | REQUIRED | |
| DATA-014 | Records kept after deletion (machine/node history, network attribution, audit events, usage records) | data-retention | REQUIRED | Node assignment history + audit log + credits ledger. |
| DATA-015 | Disk-full alerts and lost-sandbox cause from CPU/RAM samples | data-retention__snapshots-and-monitoring-off | REQUIRED | |
| DATA-016 | Dashboard Account → Data & privacy toggles | data-retention | REQUIRED | |

## CI

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| CI-001 | `runs-on: boat` GitHub Actions runners: fresh sandbox per job, deleted after | ci | ADAPTED | No GitHub App/inbound webhooks: a CI controller on the coordinator polls GitHub (user PAT, outbound only) for queued `workflow_job`s with `crowdvm*` labels (Boat's `boat`, `boat-large`, … are accepted as aliases) and boots an ephemeral JIT self-hosted runner per job. Gitea Actions on the LAN: `ci/connect {provider:"gitea", apiUrl}` with a Gitea token (Gitea ≥ 1.24 runs/jobs API); each job gets a fresh sandbox with an ephemeral `act_runner` (`register --ephemeral`, labels `<label>:host`) registered with a repo registration token. Forgejo with the same API is expected to work but untested. |
| CI-002 | Install/link GitHub App: `boat ci connect [--org] [--account]`, `POST /ci/connect {org, account}` (owner link confirmation) | ci, api connect-ci-github | ADAPTED | `ci connect` registers a git host account + token (PAT with `actions`/admin:org runner scope) instead of an App install; confirmation step kept. |
| CI-003 | Labels `boat-small`, `boat`, `boat-large`, `boat-xlarge` + options `/from=<saved>` (owner secrets scrubbed), `/debug` (keep 30 min on failure), legacy `/kvm`; unknown option → refused with reason | ci | ADAPTED | Labels `crowdvm-small`, `crowdvm`, `crowdvm-large`, `crowdvm-xlarge`; options identical. |
| CI-004 | Job runs in `$HOME/actions-runner/_work`; services:/container:/cache/upload-artifact work; Docker, KVM, Android emulator | ci | REQUIRED | Standard runner in VM. KVM per SIZE-014. |
| CI-005 | OOM → "Out of memory on Boat" annotation; 4 GB swap | ci | REQUIRED | |
| CI-006 | No Boat environment passed to jobs (Actions secrets only) | ci | REQUIRED | |
| CI-007 | Job sandbox named `ci-<repo>-<job id>`, visible in list, SSH-able | ci | REQUIRED | |
| CI-008 | Refusal reasons (no plan, unknown label, public repo, unlinked account, missing snapshot) shown in CLI/API/dashboard | ci | ADAPTED | "no plan" → "insufficient credits" when enforcement on. |
| CI-009 | Start-limit behaviour: minute → retry every 15 s; hour → wait ≤15 min; day → fail; "paused" state + "Paused by plan limit" on GitHub | ci | REQUIRED | Controller annotates via GitHub API (check run). |
| CI-010 | 6 h max; CI sandboxes outliving job deleted | ci | REQUIRED | |
| CI-011 | Public repos off by default; `boat ci public <account> on\|off [--users …] [--anyone]`, author+triggerer must be listed; `POST /ci/public {account, allow, users, dryRun, org}` | ci, api set-ci-public-repos | REQUIRED | |
| CI-012 | `boat ci [--account] [--repo] [--state all\|active\|failed] [--limit] [--all]`, `ci repos`, `ci job <id>`, `ci logs <id> [--follow]` | ci | REQUIRED | |
| CI-013 | `GET /ci?account&repo&state&limit` (setup.next steps, installations, repos status ok/no-jobs/failing/public-blocked, 7-day stats, jobs), `GET /ci/jobs/{id}` (state, wait, run time, cost, machineClass, sandbox, sandboxState, error), `GET /ci/jobs/{id}/logs` (≤1 MB from GitHub) | ci, boat-v1.yaml | ADAPTED | Cost in credits; logs fetched from GitHub/Gitea API. |
| CI-014 | Scopes: read `account.read`; connect/public `account.admin` | ci | REQUIRED | |
| CI-015 | Dashboard "CI on Boat VMs" tab | ci | REQUIRED | |
| CI-016 | CLI-from-any-CI recipe (static binary, `login --key-stdin`, new/scp/exec/delete, `boat run`) | ci | REQUIRED | Requires CI runner to reach the coordinator (LAN/Tailscale). |
| CI-017 | apt mirror advice (fr.archive.ubuntu.com) | ci | N/A | Network-location advice specific to Boat's hosts; CrowdVM may configure a LAN apt cache instead. |
| CI-018 | CI requires $20 plan | ci | ADAPTED | Requires credits only when enforcement on. |

## Evals / RL / Harbor / Eve / AgentEnv adapters

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| EVAL-001 | Episode pattern: saved snapshot per env version, `new --from` per episode, score, delete; fork to branch mid-episode | evals | REQUIRED | Composition of SNAP/LIFE/CMD; must hold under parallel load (start limits, Idempotency-Key, 429/503 retry). |
| EVAL-002 | Python SDK episode script (ThreadPoolExecutor, `exec_command`, `delete_sandbox(x_ascii_confirm_delete=…)`) runs unchanged except host/key | evals | REQUIRED | SDK parity (SDK rows). |
| EVAL-003 | AgentEnv built-in `boat` provider (`api_url` configurable): smallest-fitting size, hosted ports on URLs, delete on end, label `agentenv.managed=true` + `agentenv.attr.*`, find-by-label cleanup, retries ×6 on limits, no-env + snapshots off, refuses `image` override and egress policies | integrations__agentenv | REQUIRED | Works by pointing `api_url` at CrowdVM: requires API fidelity for create (type, labels, noEnv, snapshots:false, ttl), host, list `label=` filter, delete with confirm header. |
| EVAL-004 | Harbor adapter `harbor-box` (`BoxEnvironment`, `base_url` option, `BOX_API_KEY`; start/stop/exec/upload/download/dirs/is_file; no-env default; refuses docker_image, network policy, GPU) | integrations__harbor | ADAPTED | Adapter calls the legacy Box API: CrowdVM serves the Box alias layer (API-137) so `base_url=http://<server>/api/box/v1` works; optional maintained fork pointing at `/api/v1`. |
| EVAL-005 | Eve backend `@asciidev/eve-box` (`asciiBox({apiKey, baseUrl, name, ttlSeconds, noEnv, env, pollMs, commandTimeoutMs, networkPolicy})`: run/spawn/read/write/remove/resolvePath under /workspace, create/resume; only `allow-all` network policy) | integrations__eve | ADAPTED | Same as EVAL-004 via Box alias layer + `baseUrl`. |
| EVAL-006 | Terminal-Bench 2.0 not supported via adapter (docker_image refused); run via saved snapshot + exec | evals, integrations__harbor | REQUIRED | Same limitation; recipe works. |
| EVAL-007 | Network policies / egress allow-lists not enforced (adapters refuse) | integrations__eve, integrations__agentenv | REQUIRED | Same refusal behaviour (no false isolation). |

## Windows apps (Wine)

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| WIN-001 | Windows programs run on the sandbox desktop via Wine; visible in desktop stream; agents drive via computer tools | windows-apps, windows-apps__agents-and-forks | REQUIRED | Needs DESK + AGT computer use; Wine not preinstalled (same as Boat). |
| WIN-002 | `install-wine.sh` works unchanged: i386 multiarch, WineHQ staging (Wine ≥11), libgd3 pin (image ships a PHP repo), mesa-vulkan-drivers, winetricks corefonts vcrun2022 dxvk d3dcompiler_47 | windows-apps__install-wine | REQUIRED | Image must allow i386 + outbound to dl.winehq.org; the PHP-repo pin note only applies if the image has the same repo (TOOL rows). |
| WIN-003 | Install as detached command (API/SDK) or `ssh -- bash -s` | windows-apps__install-wine | REQUIRED | |
| WIN-004 | App recipes: `wine start /unix`, silent installers, Steam, Paint.NET 5.2 (xdotool loop), Sysinternals EULA reg keys, Direct3D via DXVK on software Vulkan, UseXRandR/UseXVidMode | windows-apps__run-apps | REQUIRED | Needs `xdotool`, `wmctrl` in image; software Vulkan (no GPU). |
| WIN-005 | Tested-app list (Notepad++, HxD, …, SkiFree) runs; CPU-Z / Paint.NET ≤5.1 / CCleaner do not | windows-apps__tested-apps | REQUIRED | Acceptance-test list for the desktop image. |
| WIN-006 | Saved snapshot of Wine sandbox (`~/.wine` + system packages); fork keeps unsaved Notepad++ text on disk; stop/resume timings (stop 4 s, resume 20–32 s for 16 GB) | windows-apps__agents-and-forks | ADAPTED | Functionally REQUIRED; timings depend on LAN/disk of nodes (no SLA). |
| WIN-007 | Troubleshooting table (libgd3, Mono window, DISPLAY, console handles, vcrun, modal dialogs, 16-bit slowdown, resolution) | windows-apps__troubleshooting | REQUIRED | Holds if image matches Boat's (DISPLAY=:0, X11 desktop). |
| WIN-008 | Licence guidance | windows-apps__troubleshooting | N/A | Legal guidance text, not a feature. |
| WIN-009 | No Windows runners/VMs (Wine only) | ci | ADAPTED | Same scope: Linux guests only. (Windows *nodes* host Linux VMs; Windows guests are out of scope.) |

## SDKs (Python / TypeScript)

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| SDK-001 | Boat SDKs (`boat-sdk`, `@boatdev/sdk`) work against CrowdVM by setting `host`/`basePath` to `http://<server>/api/v1` with a `cvm_` key | sdks__overview | REQUIRED | Acceptance test for API fidelity (shapes, `oneOf` command responses, NDJSON). |
| SDK-002 | CrowdVM-published SDK packages: Python (snake_case, sync, `ApiClient`/`Configuration`/`BoatApi`-equivalent client, `ApiException`) and TypeScript (camelCase, async, Node ≥18 fetch) covering the full v1 API | sdks__overview, sdks__python, sdks__typescript | ADAPTED | Generated from CrowdVM's OpenAPI (`crowdvm-sdk`, `@crowdvm/sdk`), distributed from the repo / served by coordinator (no public registry required). |
| SDK-003 | One method per operation with the documented names (sandboxes, create(idempotency_key, org, x_boat_org), get, update, delete_sandbox(x_ascii_confirm_delete), stop, share, recover, resume, fork, get_stop, usage, command*, files*, desktop, host_port, ssh_key, prompt/events/conversations/prompt_run_status/steer/interrupt, snapshots*, saved snapshots*, environments*, credential profiles*, ai proxy/usage/access, me, orgs, limits, api keys*, webhooks*, telemetry*, data retention*, CI*) | sdks__python, sdks__typescript | REQUIRED | Generated 1:1 from OpenAPI operationIds. |
| SDK-004 | Helpers: wait_until_ready / waitUntilReady, wait_until_idle, wait_for_desktop, wait_for_prompt, wait_for_prompt_done, stream_events, stream_prompt, stop_and_remove, read_text, write_text, exec_command (retries `sandbox_restoring` refusals), stream_command, run_command (no time limit, reconnect, first frame `started`), followCommand (TS) | sdks__python, sdks__typescript, migrating-from-box__steps | REQUIRED | Hand-written helper module in both SDKs. |
| SDK-005 | `signal` (AbortSignal) on TS streaming helpers | sdks__typescript | REQUIRED | |
| SDK-006 | Python `var_from` for `from` field; keyword-only request bodies; `actual_instance` wrapper on command() | sdks__python, snapshots__templates | REQUIRED | Generator conventions. |
| SDK-007 | asyncio usage via `asyncio.to_thread` (sync SDK) | sdks__python | REQUIRED | |
| SDK-008 | Recipes: push folder (git ls-files → tar → upload URL → untar), run-one-job-and-delete with cost, public URL, sizes, env, no-env, saved snapshots/forks, prompts, browse stopped sandbox, error handling | sdks__python, sdks__typescript | REQUIRED | Must run unchanged apart from host/key. |
| SDK-009 | Other languages: generate from published OpenAPI (`/openapi/boat-v1.yaml`) | sdks__overview, migrating-from-box | REQUIRED | Coordinator serves its OpenAPI at `/openapi/boat-v1.yaml` (Boat's path), `/openapi/crowdvm-v1.yaml` and `/api/v1/openapi.yaml`. |

## CLI (`crowdvm`, mirrors `boat`)

Naming rule for this whole section: `boat` → `crowdvm`, `BOAT_*` env → `CROWDVM_*`, `bx_`/`bsh_`/`bdop_`/`stop_`/`cmd_` id prefixes keep their shape. Every command supports `--json`; the API row it maps to is in the API section.

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| CLI-001 | Global `--api-url <URL>` / `CROWDVM_API_URL` | cli-reference | REQUIRED | Default = coordinator URL saved at login. |
| CLI-002 | Global `--org <id\|name>` / `CROWDVM_ORG` (one-command wallet scope; does not change active org) | cli-reference | REQUIRED | Sent as `X-Boat-Org`/`org` (accept `X-CrowdVM-Org` alias). |
| CLI-003 | Global `--json`; auto-on when stdout is piped/redirected | cli-reference | REQUIRED | |
| CLI-004 | Global `--no-update` / `CROWDVM_NO_UPDATE=1` | cli-reference | ADAPTED | Skips auto-update check against the coordinator's binary endpoint. |
| CLI-005 | JSON Lines output for `new`, `prompt`, `events` (±`--follow`), `conversations` | cli-reference | REQUIRED | |
| CLI-006 | `--json` failure: last line `{"event":"error","error","code","status"}` + non-zero exit; arg errors on stderr | cli-reference | REQUIRED | |
| CLI-007 | Sandbox id alias `current` (last sandbox created/forked in this shell, via shell function; default target of `ssh`/`exec`/`prompt`) | cli-reference | REQUIRED | Shell function installed by install script stores per-shell state (env var / per-PPID file). |
| CLI-008 | Sandbox id alias `self` (sandbox the CLI runs in) | cli-reference | REQUIRED | Guest config exposes own id + per-sandbox key. |
| CLI-009 | `completions bash\|zsh\|fish\|powershell` completing subcommands, live sandbox ids, `current`, `self`; 15 s result cache | cli-reference | REQUIRED | |
| CLI-010 | Static CLI binary download per platform/arch (`/api/boat/cli/download?platform=linux-x64…`) for Docker images | cli-reference | ADAPTED | Coordinator serves `/api/crowdvm/cli/download?platform=…` for linux-x64/arm64, windows-x64, darwin (cross-compiled). |
| CLI-011 | `onboard` (browser sign-in, save token, ask only missing steps; idempotent) | cli-reference | ADAPTED | Local-account device-code sign-in; "plan checkout" step replaced by showing credit balance; third-party-safety question kept. |
| CLI-012 | `onboard --google` | cli-reference | N/A | Google OAuth requires public internet + Google; out of scope (no third-party OAuth). Flag prints a clear "not supported, use --email/local" error. |
| CLI-013 | `onboard --email <addr>` / `login --email` (6-digit email code; disposable domains refused) | cli-reference | ADAPTED | Code delivered via optional SMTP or printed to server log; disposable-domain blocklist optional. |
| CLI-014 | `onboard --agentid` / `login --agentid` | cli-reference, agent-auth | N/A | AgentID/AgentMail is a third-party identity SaaS on the public internet. Agents use the auth.md self-registration flow instead (AUTH-010…). |
| CLI-015 | `onboard --wait --json` (wait for browser step, save session) | cli-reference | REQUIRED | |
| CLI-016 | `login` without key: prints URL + code, opens browser, waits ≤15 min | cli-reference | ADAPTED | Device-code flow against coordinator dashboard; default method = local username/password instead of GitHub. |
| CLI-017 | `login --json` non-interactive: prints URL, code, `expiresAt`, exits; re-run returns same pending link | cli-reference | REQUIRED | |
| CLI-018 | `login --new` (replace pending link, emits `login_replaced`) and `login --wait` | cli-reference | REQUIRED | |
| CLI-019 | `login <key>` and `login --key-stdin` (mutually exclusive) | cli-reference | REQUIRED | Keys are `cvm_…`. |
| CLI-020 | `login` GitHub default method | cli-reference, account | ADAPTED | Default = local account in browser. GitHub OAuth N/A. |
| CLI-021 | `logout` clears local token | cli-reference | REQUIRED | |
| CLI-022 | `status` (API health, signed-in account, plan, config path, binary path); `--check` exits 1 when signed out; JSON `account.signedIn` | cli-reference | ADAPTED | "plan" → credit balance / enforcement mode. |
| CLI-023 | `feedback "<text>"` / stdin; 5/min, 30/day | cli-reference | ADAPTED | Stored on coordinator, visible to admins in dashboard; optional SMTP forward. Rate limits kept. |
| CLI-024 | `self-update` (latest release on current channel) | cli-reference | ADAPTED | Downloads from coordinator; channel = coordinator version. |
| CLI-025 | Auto-update at most every 6 h after a command ends, never interactive | cli-reference | ADAPTED | Same, against coordinator. |
| CLI-026 | Local config file path (`~/.config/ascii/boat/config.json`) printed by `status` | cli-reference | ADAPTED | `~/.config/crowdvm/config.json` (`%APPDATA%\crowdvm` on Windows). |
| CLI-027 | `api-key create <name> [--ttl 90d\|…\|365d\|never] [--preset read-only\|full-sandbox\|ci\|admin] [--actions a,b] [--sandbox ID]… [--env ENV]…` | cli-reference, api-keys__* | REQUIRED | `--actions` and `--preset` mutually exclusive; secret shown once. Only browser session or admin-preset key may create. |
| CLI-028 | `api-key list [--all]` (prefix, last4, scope, expiry, 30-day requests, live resources; `--all` adds per-sandbox machine keys) | cli-reference | REQUIRED | |
| CLI-029 | `api-key usage <id> [--verbose]` (works after revoke) | cli-reference | REQUIRED | |
| CLI-030 | `api-key rotate <id>` / `revoke <id>` (browser session only) | cli-reference | REQUIRED | |
| CLI-031 | `webhook create <url> [--name N] [--event ready\|error\|archived\|hydrated\|degraded\|recovered]…` (repeat or comma list; none = all 6); `list`, `rotate <id>`, `remove <id>` | cli-reference | REQUIRED | Secret shown on create/rotate only. |
| CLI-032 | `telemetry` (show export + last result), `telemetry set <endpoint> -H k=v…` (test export), `telemetry unset` | cli-reference | REQUIRED | Header values encrypted, never shown. |
| CLI-033 | `new` emits JSONL events `created`, `state`, `ready`; returns id, IP, first state | cli-reference | REQUIRED | IP = guest/node address reachable on LAN or tunnel placeholder. |
| CLI-034 | `new --type small\|default\|large\|xlarge` | cli-reference | REQUIRED | Size → vCPU/RAM/disk table, scheduled on a node with capacity. |
| CLI-035 | `new --ttl SECONDS` (default 3600, max 2592000; not idle-based) | cli-reference | REQUIRED | |
| CLI-036 | `new --no-auto-stop` | cli-reference | REQUIRED | |
| CLI-037 | `new --env/-e KEY=VALUE`… (≤100 vars, 64 KB) | cli-reference | REQUIRED | |
| CLI-038 | `new --environment <name>` (unknown name refused before create) | cli-reference | REQUIRED | |
| CLI-039 | `new --no-env` | cli-reference | REQUIRED | |
| CLI-040 | `new --no-snapshots` | cli-reference | REQUIRED | |
| CLI-041 | `new --no-monitoring` | cli-reference | REQUIRED | |
| CLI-042 | `new --fail-fast` (~1.5 s answer, `no_ready_machine`, nothing billed/counted) | cli-reference | REQUIRED | "ready machine" = a node with free capacity and base image cached (warm VM optional). |
| CLI-043 | `new --setup-file ./setup.sh` (≤64 KB; runs once in background as `user`; `setupStatus`/`setupError`) | cli-reference | REQUIRED | |
| CLI-044 | `new --from <saved-snapshot\|bsh_ share code>` | cli-reference | REQUIRED | |
| CLI-045 | `new --from X --count N` (≤1000; all-or-nothing vs limits; not with `--fail-fast`) | cli-reference | REQUIRED | Batch scheduled across nodes. |
| CLI-046 | `new --personal` | cli-reference | REQUIRED | |
| CLI-047 | `new --name "…"` (≤120 chars; default `Box <date>` or snapshot name) | cli-reference | REQUIRED | |
| CLI-048 | `new --visibility private\|shared` (org sandboxes only) | cli-reference | REQUIRED | |
| CLI-049 | `new --host PORT[:public]`… (≤50; routed before ready; not with `--count`/`--fail-fast`) | cli-reference | ADAPTED | LAN host-routed URL (see URL section). |
| CLI-050 | `new --file SANDBOX_PATH=LOCAL_FILE`… (≤32 text files, 256 KB; written before `ready`) | cli-reference | REQUIRED | |
| CLI-051 | `list [--filter r\|s\|p\|t\|e (combinable)] [--all]`; shows shared/private for org sandboxes | cli-reference | REQUIRED | |
| CLI-052 | `rename ID NAME` (creator only) | cli-reference | REQUIRED | |
| CLI-053 | `extend ID --hours N \| --ttl S \| --no-auto-stop` (sets, not adds; max 30 d) | cli-reference | REQUIRED | |
| CLI-054 | `info ID` (state, IP, desktop availability, remaining TTL, `rebooted:` line, `org:` line, setup status) | cli-reference | REQUIRED | |
| CLI-055 | `stop ID [--yes]`; prints `stopping`→`stopped`, `operation.id` | cli-reference | REQUIRED | |
| CLI-056 | `stop ID --status <stop_id>` (pending/failing/completed/superseded) | cli-reference | REQUIRED | |
| CLI-057 | `stop ID --force` (discard data since last good snapshot) | cli-reference | REQUIRED | |
| CLI-058 | `delete ID [--yes]` (confirm; `--yes` mandatory with `--json`/no TTY; prints `bdop_` op and follows it); `delete self --yes` | cli-reference | REQUIRED | |
| CLI-059 | `deletion status <bdop_id>` | cli-reference | REQUIRED | |
| CLI-060 | `resume ID` waits for ready + no-op test command (≤30 min) | cli-reference | REQUIRED | |
| CLI-061 | `resume --no-wait / --fail-fast / --no-env / --type / -e / --environment / --ttl / --no-auto-stop` | cli-reference | REQUIRED | `-e` replaces per-sandbox vars; `--type/--environment/--no-env` on running → `sandbox_running_stop_first`; shrink refused if data won't fit. |
| CLI-062 | `recover ID` (power-cycle; refused if responsive, <10 min since last, or unsupported) | cli-reference | REQUIRED | Node kills/restarts QEMU on same overlay. |
| CLI-063 | `fork ID [--type] [--ttl\|--no-auto-stop] [--environment] [--no-env] [--no-snapshots] [--no-monitoring] [-e] [--fail-fast] [--wait]` | cli-reference | REQUIRED | Fork TTL defaults to 1 h; `-e` replaces inherited vars; no-env sticky. |
| CLI-064 | `events ID [-f/--follow] [--convo ID\|current]…` (alias `--conversation`) | cli-reference | REQUIRED | |
| CLI-065 | `conversations [ID]` / alias `convos`; markers `sandbox current`, `this shell` | cli-reference | REQUIRED | |
| CLI-066 | `steer [ID] MESSAGE [--convo ID\|current]`; `no_running_turn` | cli-reference | REQUIRED | |
| CLI-067 | `interrupt ID [--convo ID\|current]` | cli-reference | REQUIRED | |
| CLI-068 | `ssh ID [command]`, `ssh ID -- argv…`, `ssh -t/--tty`; managed key `~/.ssh/ascii_box_ed25519` | cli-reference | ADAPTED | `ProxyCommand` through coordinator tunnel; key `~/.ssh/crowdvm_ed25519`. |
| CLI-069 | `exec ID [cmd…]` (1 arg = shell line, >1 = argv; exits with remote code) | cli-reference | REQUIRED | |
| CLI-070 | `exec --cwd <dir>` (relative to /home/user) | cli-reference | REQUIRED | |
| CLI-071 | `exec --timeout <s>` (default 30, exit 124; `0`=no limit; >600 or 0 → background + live stream with reconnect; Ctrl-C kills) | cli-reference | REQUIRED | |
| CLI-072 | `exec --stream` (JSON frame per line with `--json`) | cli-reference | REQUIRED | |
| CLI-073 | `exec --detach` (prints `processId`), `--status <pid>`, `--kill <pid>`, `--attach <pid>` (reconnect every 10 min, exits with its code) | cli-reference | REQUIRED | |
| CLI-074 | `exec` while starting waits ≤60 s then `sandbox_starting` | cli-reference | REQUIRED | |
| CLI-075 | `run [--from] [--type] [--push DIR] [--cwd] [--timeout] [-e]… [--environment] [--keep] [--personal] [--no-env] -- cmd` (create, push, run, print time+cost, delete; exit code passthrough; summary line on stderr; JSON `summary` frame) | cli-reference | REQUIRED | Cost shown in credits. Auto-stop 15 min ahead, pushed forward while running; no snapshots unless `--keep`; Ctrl-C deletes; `--personal` bills the personal wallet even with an org active. |
| CLI-076 | `run` rejects unknown options before creating anything; `--` separator rule (also for `exec`/`ssh`) | cli-reference | REQUIRED | |
| CLI-077 | `push ID DIR [--to PATH]` (git-tracked + untracked-not-ignored files, single archive over SSH; keeps exec bits/symlinks; deletions not propagated) | cli-reference | REQUIRED | |
| CLI-078 | `history ID [cmd-id] [--limit ≤100] [--mode commands,process,sync,stream,detached] [--stderr] [-f/--follow]` (works stopped) | cli-reference | REQUIRED | |
| CLI-079 | `ps ID` (pid, CPU, mem, user, history record, cmdline; last sample when stopped) | cli-reference | REQUIRED | |
| CLI-080 | `env list / info <n> / new <n> / rename <n> <new> / default <n> / rm <n>` | cli-reference | REQUIRED | |
| CLI-081 | `env set <n> --safe-for-third-parties --github --secrets --boat-credentials --agents-credentials --credential-profile <name\|id\|default> --proxy-ai-credentials` | cli-reference | ADAPTED | `--github` → passes the user's stored git credential (PAT / local git host token), not GitHub App; `--boat-credentials` → `--crowdvm-credentials` (alias kept). |
| CLI-082 | `env … --org <org>`; `env access <env> [--everyone\|--only emails\|--add\|--remove]` | cli-reference | REQUIRED | |
| CLI-083 | `env set-var <env> K=V` / value from stdin; `rm-var` | cli-reference | REQUIRED | Warning on safe-for-third-parties envs. |
| CLI-084 | `env set-file <env> <path> [--from local]` / stdin; `rm-file` | cli-reference | REQUIRED | |
| CLI-085 | `env add-repo <env> owner/repo [--branch B]`; `rm-repo` | cli-reference | ADAPTED | Repos from any git URL reachable on LAN (local Gitea/GitLab/GitHub via PAT); see ENV rows. |
| CLI-086 | `env upgrade <env>` | cli-reference | REQUIRED | |
| CLI-087 | `host ID PORT [--title T] [--private (default)\|--public] [--localhost]`; same URL on re-run | cli-reference | ADAPTED | URL `http://<id>-<port>.<domain>` (LAN host routing) with `?_token=` redirect+cookie. |
| CLI-088 | `scp [--recursive] src dst` with `bx_<id>:/path`; tar fast-path for new dirs | cli-reference | ADAPTED | Uses OpenSSH `scp` with CrowdVM ProxyCommand. |
| CLI-089 | `forward ID --remote P [--local P] [--bind addr]` (sandbox → local) | cli-reference | REQUIRED | |
| CLI-090 | `forward ID --reverse --local P [--remote P] [--bind addr]` (local → sandbox 127.0.0.1) | cli-reference | REQUIRED | |
| CLI-091 | `forward` reconnects up to 5× after sandbox moves; startup failures not retried | cli-reference | REQUIRED | |
| CLI-092 | `desktop ID` opens viewer; JSON `viewerUrl` with token | cli-reference | ADAPTED | noVNC viewer served by coordinator instead of Moonlight/WebRTC 60 fps. |
| CLI-093 | `browser ID [--profile /abs/path]` (browser-only stream) | cli-reference | ADAPTED | Kiosk Chrome in a separate X display/VNC session; see DESK rows. |
| CLI-094 | `snapshots [ID] [--limit N] [--all]` | cli-reference | REQUIRED | |
| CLI-095 | `snapshot ID NAME` (save named; polls until ready); `snapshot self NAME`; `snapshot rm NAME` | cli-reference | REQUIRED | |
| CLI-096 | `snapshot share NAME` / `shares NAME` / `unshare NAME shr_…` | cli-reference | REQUIRED | Share codes work across accounts on the same coordinator. |
| CLI-097 | `snapshot latest ID` / `tree SNAP` / `pull SNAP -o DIR [--path P]…` (writes `home_user/`, `docker/`; Windows symlink skip count) | cli-reference | REQUIRED | |
| CLI-098 | `snapshot delete SNAP… [--yes]`, `snapshot delete --all ID` | cli-reference | REQUIRED | |
| CLI-099 | `prompt [ID] "text"` (default sandbox = current/self; default harness/model from dashboard; exit 1 when no credential) | cli-reference | REQUIRED | |
| CLI-100 | `prompt --provider claude\|codex\|pi\|opencode\|prime\|kimi\|mistral --model M --reasoning-effort L --fast` | cli-reference | REQUIRED | Harness availability depends on the image; providers still need the user's own key (see AGT rows). |
| CLI-101 | `prompt --new`, `--resume <id\|current>`; prints `conversation: <id> (new)` | cli-reference | REQUIRED | |
| CLI-102 | `prompt --attach/--image <path>`… (saved to `~/attachments`, images to vision models) | cli-reference | REQUIRED | |
| CLI-103 | `prompt --help` lists live providers/models/efforts and fast-capable models from the API | cli-reference | REQUIRED | Catalog served by coordinator. |
| CLI-104 | `ai accounts <cred> [--profile]`, `ai switch <cred> <n\|email\|last chars>`, `ai add-account <cred> [value\|-]`, `ai forget-account` | cli-reference | REQUIRED | |
| CLI-105 | `ai proxy [on\|off]` | cli-reference | REQUIRED | Price shown in credits. |
| CLI-106 | `ai profiles [--org-only]`; `ai profile new <n> [--copy-from p]` / `default` / `rename` / `access` / `rm` | cli-reference | REQUIRED | |
| CLI-107 | `ai profile set-key <p> <cred> [value\|-]`, `rm-key`, `set-api <p> NAME --base-url URL [--value]`, `rm-api` | cli-reference | REQUIRED | |
| CLI-108 | `ai usage [ID] [--days N]` (default 30), `ai block ID`, `ai unblock ID` | cli-reference | REQUIRED | |
| CLI-109 | `limits [--all\|--details] [--json]` (starts per minute/hour/day, plan money, wallet, allowed types + reasons) | cli-reference | ADAPTED | Credits balance replaces plan/wallet dollars; start limits and per-type availability (node capacity) kept. |
| CLI-110 | `usage ID [--since T] [--until T]` (ISO 8601 or Unix s; `seconds`, `dollars`, `secondsPerDollar`, `sandboxType`, `billingMultiplier`, `running`) | cli-reference | ADAPTED | `dollars` = credits spent (field kept for compatibility). |
| CLI-111 | `dashboard` (one-time 15-min login code link; cannot call API) | cli-reference | REQUIRED | |
| CLI-112 | `org` / `org status`, `org list`, `org switch <org\|personal>` | cli-reference | REQUIRED | |
| CLI-113 | `org create "Name"`, `org transfer <member>`, `org delete --yes` | cli-reference | REQUIRED | |
| CLI-114 | `org migrate` (move personal plan + sandboxes into org; refund) | cli-reference | ADAPTED | Moves personal sandboxes (and optionally credit balance) into org; no refunds. |
| CLI-115 | `org members`, `org invite <email>` (owner only) | cli-reference | ADAPTED | Invite delivered as link/code (log/SMTP) to a local account. |
| CLI-116 | `org default-visibility [private\|shared]` | cli-reference | REQUIRED | |
| CLI-117 | `share ID` / `unshare ID` | cli-reference | REQUIRED | |
| CLI-118 | `billing` / `billing status` / `billing --all` | cli-reference | ADAPTED | Shows credit balance, earn/spend rates, enforcement mode. |
| CLI-119 | `billing plan [tier]`, `billing plan N --move-personal` | cli-reference | ADAPTED | Tiers become admin-defined credit quotas (or N/A when enforcement off). No payment. |
| CLI-120 | `billing extra-starts on\|off` | cli-reference | ADAPTED | Toggle "allow starts beyond start limit at credit cost". |
| CLI-121 | `billing portal`, `billing cancel [--keep-disks]` | cli-reference | N/A | Stripe customer portal / subscription cancellation: no payments in scope. |
| CLI-122 | `wallet` (balance, auto top-up state, cards) | cli-reference | ADAPTED | Shows credit balance and ledger summary. |
| CLI-131 | `wallet pay N`, `wallet auto-top-up N\|off`, `wallet card`, `--org` owner wallet pay | cli-reference, billing | N/A | Real-money payments/cards out of scope; replaced by admin `crowdvm admin credits grant` (BILL-005). Commands exit with a clear "payments not supported" error. |
| CLI-123 | `data-retention status\|enable [--yes]` (browser session only) | cli-reference | REQUIRED | |
| CLI-124 | `ci [--account] [--repo] [--state all\|active\|failed] [--limit] [--all]`, `ci repos`, `ci job <id>`, `ci logs <id> [--follow]` | ci | ADAPTED | Against the CrowdVM CI controller (CI-001). |
| CLI-125 | `ci connect [--org] [--account]` | ci | ADAPTED | Registers git-host token (CI-002). |
| CLI-126 | `ci public <account> [on\|off] [--users …] [--anyone]` | ci | REQUIRED | |
| CLI-127 | Exit codes: `exec`/`run` propagate remote code; 124 on timeout; 1 on CLI errors; `status --check` 1 when signed out | cli-reference | REQUIRED | |
| CLI-128 | `stop` accepts `--yes/-y` (no-op) for script symmetry | cli-reference | REQUIRED | |
| CLI-129 | Windows CLI quirks: `snapshot pull` symlink skip report; PowerShell stdin piping caveat | cli-reference, setup | REQUIRED | |
| CLI-130 | Admin commands (users, nodes, join tokens, credit grants) | (CrowdVM) | REQUIRED | `crowdvm admin …`; replaces Boat staff/support actions. |

## API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour)

Default for every endpoint: path, params, body and response shapes follow the OpenAPI field-for-field under `/api/v1` on the coordinator; sandbox-internal routes are proxied to the guest agent (ARCHITECTURE). Rows below only add notes where the mapping differs.

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| API-001 | `GET /me` — Get current Boat user (`me`) | boat-v1.yaml | ADAPTED | `connectionMethod` local/email/agent/service instead of github/google; GitHub identity fields null. |
| API-002 | `GET /orgs` — List organizations (`listOrganizations`) | boat-v1.yaml | REQUIRED |  |
| API-003 | `PATCH /orgs/{org}` — Update organization settings (`updateOrganization`) | boat-v1.yaml | ADAPTED | Rename + `newSandboxesPrivate`; no Stripe billing-name sync (no 502 path). |
| API-004 | `PATCH /orgs/active` — Set the active organization (`setActiveOrganization`) | boat-v1.yaml | REQUIRED |  |
| API-005 | `GET /account/data-retention` — Get account data-retention policy (`getDataRetention`) | boat-v1.yaml | REQUIRED |  |
| API-006 | `PATCH /account/data-retention` — Update account data-retention policy (`updateDataRetention`) | boat-v1.yaml | REQUIRED | Browser session only (`403 session_required`); confirmation phrase. |
| API-007 | `GET /deletion-operations/{operationId}` — Get deletion operation (`getDeletionOperation`) | boat-v1.yaml | REQUIRED |  |
| API-008 | `GET /limits` — Get Boat limits (`limits`) | boat-v1.yaml | ADAPTED | Same shape; money fields in credits; trial/subscription fields null (BILL-011). |
| API-009 | `GET /repos` — List GitHub repositories available to Boat (`repos`) | boat-v1.yaml | ADAPTED | Lists repos from configured git-host tokens (GitHub PAT / Gitea), grouped by credential ("installation"). |
| API-010 | `POST /repos` — Select repository for sandboxes (`selectRepo`) | boat-v1.yaml | ADAPTED | Legacy: edits default environment; repo id from `GET /repos` or git URL. |
| API-011 | `GET /api-keys` — List API keys (`apiKeys`) | boat-v1.yaml | REQUIRED |  |
| API-012 | `POST /api-keys/scoped` — Create a scoped API key (`createScopedApiKey`) | boat-v1.yaml | REQUIRED |  |
| API-013 | `DELETE /api-keys/{apiKeyId}` — Revoke an API key (`revokeApiKey`) | boat-v1.yaml | REQUIRED |  |
| API-014 | `POST /api-keys/{apiKeyId}/rotate` — Rotate an API key (`rotateApiKey`) | boat-v1.yaml | REQUIRED |  |
| API-015 | `GET /api-keys/{apiKeyId}/usage` — Get API key usage (`apiKeyUsage`) | boat-v1.yaml | REQUIRED |  |
| API-016 | `GET /webhooks` — List webhooks (`listWebhooks`) | boat-v1.yaml | REQUIRED |  |
| API-017 | `POST /webhooks` — Create webhook (`createWebhook`) | boat-v1.yaml | REQUIRED |  |
| API-018 | `GET /webhooks/{webhookId}` — Get webhook (`getWebhook`) | boat-v1.yaml | REQUIRED |  |
| API-019 | `PATCH /webhooks/{webhookId}` — Update webhook (`updateWebhook`) | boat-v1.yaml | REQUIRED |  |
| API-020 | `DELETE /webhooks/{webhookId}` — Delete webhook (`deleteWebhook`) | boat-v1.yaml | REQUIRED |  |
| API-021 | `POST /webhooks/{webhookId}/rotate` — Rotate webhook signing secret (`rotateWebhookSigningSecret`) | boat-v1.yaml | REQUIRED |  |
| API-022 | `GET /telemetry` — Get OpenTelemetry export (`getTelemetry`) | boat-v1.yaml | REQUIRED |  |
| API-023 | `PUT /telemetry` — Set OpenTelemetry export (`setTelemetry`) | boat-v1.yaml | REQUIRED |  |
| API-024 | `DELETE /telemetry` — Remove OpenTelemetry export (`deleteTelemetry`) | boat-v1.yaml | REQUIRED |  |
| API-025 | `GET /ci` — Get CI overview (`getCiOverview`) | boat-v1.yaml | ADAPTED | CI controller data (CI rows); cost in credits. |
| API-026 | `POST /ci/connect` — Connect GitHub for CI (`connectCiGithub`) | boat-v1.yaml | ADAPTED | Registers git-host token for the CI controller instead of GitHub App install link. |
| API-027 | `POST /ci/public` — Set CI public repositories (`setCiPublicRepos`) | boat-v1.yaml | REQUIRED |  |
| API-028 | `GET /ci/jobs/{jobId}` — Get CI job (`getCiJob`) | boat-v1.yaml | ADAPTED | Cost in credits. |
| API-029 | `GET /ci/jobs/{jobId}/logs` — Get CI job log (`getCiJobLog`) | boat-v1.yaml | ADAPTED | Fetched from GitHub/Gitea API with stored token. |
| API-030 | `GET /credential-profiles` — List credential profiles (`listCredentialProfiles`) | boat-v1.yaml | REQUIRED |  |
| API-031 | `POST /credential-profiles` — Create a credential profile (`createCredentialProfile`) | boat-v1.yaml | REQUIRED |  |
| API-032 | `PUT /credential-profiles/{profile}` — Update a credential profile (`updateCredentialProfile`) | boat-v1.yaml | REQUIRED |  |
| API-033 | `DELETE /credential-profiles/{profile}` — Delete a credential profile (`deleteCredentialProfile`) | boat-v1.yaml | REQUIRED |  |
| API-034 | `PUT /credential-profiles/{profile}/keys/{credential}` — Set a credential profile API key (`setCredentialProfileKey`) | boat-v1.yaml | REQUIRED |  |
| API-035 | `DELETE /credential-profiles/{profile}/keys/{credential}` — Remove a credential profile credential (`deleteCredentialProfileKey`) | boat-v1.yaml | REQUIRED |  |
| API-036 | `GET /credential-profiles/{profile}/accounts/{credential}` — List the accounts of a credential (`listCredentialAccounts`) | boat-v1.yaml | REQUIRED |  |
| API-037 | `POST /credential-profiles/{profile}/accounts/{credential}` — Add another API key to a credential (`addCredentialAccount`) | boat-v1.yaml | REQUIRED |  |
| API-038 | `PUT /credential-profiles/{profile}/accounts/{credential}/{account}/active` — Switch a credential to another account (`switchCredentialAccount`) | boat-v1.yaml | REQUIRED |  |
| API-039 | `DELETE /credential-profiles/{profile}/accounts/{credential}/{account}` — Forget an account of a credential (`deleteCredentialAccount`) | boat-v1.yaml | REQUIRED |  |
| API-040 | `PUT /credential-profiles/{profile}/custom-apis/{name}` — Set a custom API (`setCredentialProfileCustomApi`) | boat-v1.yaml | REQUIRED |  |
| API-041 | `DELETE /credential-profiles/{profile}/custom-apis/{name}` — Remove a custom API (`deleteCredentialProfileCustomApi`) | boat-v1.yaml | REQUIRED |  |
| API-042 | `GET /ai-proxy` — Get AI credentials proxy (`getAiProxy`) | boat-v1.yaml | ADAPTED | `pricing` in credits. |
| API-043 | `PUT /ai-proxy` — Turn the AI credentials proxy on or off (`updateAiProxy`) | boat-v1.yaml | REQUIRED |  |
| API-044 | `GET /ai-usage` — Get AI usage (`getAiUsage`) | boat-v1.yaml | ADAPTED | `dollars` = credits charge. |
| API-045 | `GET /sandboxes/{sandboxId}/ai-usage` — Get sandbox AI usage (`getSandboxAiUsage`) | boat-v1.yaml | ADAPTED | `dollars` = credits charge. |
| API-046 | `PUT /sandboxes/{sandboxId}/ai-access` — Block or unblock sandbox AI access (`setSandboxAiAccess`) | boat-v1.yaml | REQUIRED |  |
| API-047 | `GET /secrets` — Get Boat secrets setup (`secrets`) | boat-v1.yaml | REQUIRED |  |
| API-048 | `POST /secrets` — Update Boat secrets setup (`updateSecrets`) | boat-v1.yaml | REQUIRED |  |
| API-049 | `GET /environments` — List Sandbox environments (`environments`) | boat-v1.yaml | REQUIRED |  |
| API-050 | `POST /environments` — Create a sandbox environment (`createEnvironment`) | boat-v1.yaml | REQUIRED |  |
| API-051 | `PUT /environments/{environmentId}` — Update a sandbox environment (`updateEnvironment`) | boat-v1.yaml | REQUIRED |  |
| API-052 | `DELETE /environments/{environmentId}` — Delete a sandbox environment (`deleteEnvironment`) | boat-v1.yaml | REQUIRED |  |
| API-053 | `POST /environments/{environmentId}/upgrade` — Upgrade sandboxes to an environment's latest version (`upgradeEnvironment`) | boat-v1.yaml | REQUIRED |  |
| API-054 | `PUT /environments/{environmentId}/vars/{key}` — Set one environment variable (`setEnvironmentVar`) | boat-v1.yaml | REQUIRED |  |
| API-055 | `DELETE /environments/{environmentId}/vars/{key}` — Remove one environment variable (`deleteEnvironmentVar`) | boat-v1.yaml | REQUIRED |  |
| API-056 | `PUT /environments/{environmentId}/secret-files` — Write one secret file (`setEnvironmentSecretFile`) | boat-v1.yaml | REQUIRED |  |
| API-057 | `DELETE /environments/{environmentId}/secret-files` — Remove one secret file (`deleteEnvironmentSecretFile`) | boat-v1.yaml | REQUIRED |  |
| API-058 | `POST /environments/{environmentId}/repos` — Add a repository to an environment (`addEnvironmentRepo`) | boat-v1.yaml | ADAPTED | Accepts repo id from `GET /repos` or a git URL. |
| API-059 | `DELETE /environments/{environmentId}/repos/{repositoryId}` — Remove a repository from an environment (`deleteEnvironmentRepo`) | boat-v1.yaml | ADAPTED | As above. |
| API-060 | `GET /sandboxes` — List sandboxes (`sandboxes`) | boat-v1.yaml | REQUIRED |  |
| API-061 | `POST /sandboxes` — Create sandbox (`create`) | boat-v1.yaml | REQUIRED | All body fields; `org`/`teamId`/`X-Boat-Org`; 402 only with credit enforcement; 503 `no_ready_machine`/`out_of_capacity`. |
| API-062 | `GET /sandboxes/{sandboxId}` — Get sandbox (`get`) | boat-v1.yaml | REQUIRED |  |
| API-063 | `PATCH /sandboxes/{sandboxId}` — Update sandbox (`update`) | boat-v1.yaml | REQUIRED |  |
| API-064 | `DELETE /sandboxes/{sandboxId}` — Permanently delete sandbox data (`deleteSandbox`) | boat-v1.yaml | REQUIRED | `X-Ascii-Confirm-Delete` must equal id. |
| API-065 | `POST /sandboxes/{sandboxId}/stop` — Stop and archive sandbox (`stop`) | boat-v1.yaml | REQUIRED |  |
| API-066 | `POST /sandboxes/{sandboxId}/share` — Share sandbox with the organization (`share`) | boat-v1.yaml | REQUIRED |  |
| API-067 | `POST /sandboxes/{sandboxId}/unshare` — Make an organization sandbox private (`unshare`) | boat-v1.yaml | REQUIRED |  |
| API-068 | `POST /sandboxes/{sandboxId}/recover` — Recover a sandbox that stopped responding (`recover`) | boat-v1.yaml | REQUIRED |  |
| API-069 | `POST /sandboxes/{sandboxId}/resume` — Resume sandbox (`resume`) | boat-v1.yaml | REQUIRED | 402 only with credit enforcement. |
| API-070 | `POST /sandboxes/{sandboxId}/fork` — Fork sandbox (`fork`) | boat-v1.yaml | REQUIRED | Idempotency-Key; 402 only with credit enforcement. |
| API-071 | `POST /sandboxes/{sandboxId}/prompt` — Prompt Sandbox (`prompt`) | boat-v1.yaml | REQUIRED |  |
| API-072 | `GET /sandboxes/{sandboxId}/events` — List boat events (`events`) | boat-v1.yaml | REQUIRED | Cursor long-poll, `If-None-Match`/304. |
| API-073 | `GET /sandboxes/{sandboxId}/conversations` — List boat conversations (`conversations`) | boat-v1.yaml | REQUIRED |  |
| API-074 | `GET /sandboxes/{sandboxId}/prompts/{promptId}` — Get prompt run status (`promptRunStatus`) | boat-v1.yaml | REQUIRED |  |
| API-075 | `GET /sandboxes/{sandboxId}/stops/{stopId}` — Get a stop by id (`getStop`) | boat-v1.yaml | REQUIRED |  |
| API-076 | `GET /sandboxes/{sandboxId}/files` — Read a file from a sandbox (`readFile`) | boat-v1.yaml | REQUIRED |  |
| API-077 | `PUT /sandboxes/{sandboxId}/files` — Write a file in a sandbox (`writeFile`) | boat-v1.yaml | REQUIRED |  |
| API-078 | `DELETE /sandboxes/{sandboxId}/files` — Delete a file or directory in a sandbox (`deleteFile`) | boat-v1.yaml | REQUIRED |  |
| API-079 | `GET /sandboxes/{sandboxId}/files/list` — List a directory in a sandbox (`listFiles`) | boat-v1.yaml | REQUIRED |  |
| API-080 | `GET /sandboxes/{sandboxId}/files/info` — Get file or directory details in a sandbox (`getFileInfo`) | boat-v1.yaml | REQUIRED |  |
| API-081 | `GET /sandboxes/{sandboxId}/files/search` — Search files by name in a sandbox (`searchFiles`) | boat-v1.yaml | REQUIRED |  |
| API-082 | `GET /sandboxes/{sandboxId}/files/find` — Find text in sandbox files (`findInFiles`) | boat-v1.yaml | REQUIRED |  |
| API-083 | `POST /sandboxes/{sandboxId}/files/folder` — Create a directory in a sandbox (`createFolder`) | boat-v1.yaml | REQUIRED |  |
| API-084 | `POST /sandboxes/{sandboxId}/files/move` — Move or rename a file or directory in a sandbox (`moveFile`) | boat-v1.yaml | REQUIRED |  |
| API-085 | `POST /sandboxes/{sandboxId}/files/permissions` — Set file permissions in a sandbox (`setFilePermissions`) | boat-v1.yaml | REQUIRED |  |
| API-086 | `POST /sandboxes/{sandboxId}/files/replace` — Replace text in sandbox files (`replaceInFiles`) | boat-v1.yaml | REQUIRED |  |
| API-087 | `POST /sandboxes/{sandboxId}/files/download-url` — Get a download URL for a sandbox file (`createDownloadUrl`) | boat-v1.yaml | ADAPTED | Signed URL served by coordinator (streams from guest), not by the sandbox directly. |
| API-088 | `POST /sandboxes/{sandboxId}/files/upload-url` — Get an upload URL for a sandbox file (`createUploadUrl`) | boat-v1.yaml | ADAPTED | Signed URL served by coordinator (streams to guest). |
| API-089 | `POST /sandboxes/{sandboxId}/commands` — Execute a command in a sandbox (`command`) | boat-v1.yaml | REQUIRED | sync / `stream` NDJSON / `detached`. |
| API-090 | `GET /sandboxes/{sandboxId}/commands/{processId}` — Get detached command status and logs (`commandStatus`) | boat-v1.yaml | REQUIRED |  |
| API-091 | `DELETE /sandboxes/{sandboxId}/commands/{processId}` — Stop a detached command (`stopCommand`) | boat-v1.yaml | REQUIRED |  |
| API-092 | `GET /sandboxes/{sandboxId}/commands/{processId}/stream` — Attach to a detached command (`attachCommand`) | boat-v1.yaml | REQUIRED | NDJSON attach frames. |
| API-093 | `GET /sandboxes/{sandboxId}/command-history` — List the command and process history (`commandHistory`) | boat-v1.yaml | REQUIRED |  |
| API-094 | `GET /sandboxes/{sandboxId}/command-history/{commandId}` — Get one command from the history (`getCommand`) | boat-v1.yaml | REQUIRED |  |
| API-095 | `GET /sandboxes/{sandboxId}/command-history/{commandId}/logs` — Read a command's stored output (`commandLogs`) | boat-v1.yaml | REQUIRED |  |
| API-096 | `GET /sandboxes/{sandboxId}/processes` — List the processes in a sandbox (`processes`) | boat-v1.yaml | REQUIRED |  |
| API-097 | `GET /sandboxes/{sandboxId}/artifacts` — Download a sandbox artifact (`artifact`) | boat-v1.yaml | REQUIRED |  |
| API-098 | `POST /sandboxes/{sandboxId}/steer` — Steer a running turn (`steer`) | boat-v1.yaml | REQUIRED |  |
| API-099 | `POST /sandboxes/{sandboxId}/interrupt` — Interrupt running work (`interrupt`) | boat-v1.yaml | REQUIRED |  |
| API-100 | `POST /sandboxes/{sandboxId}/desktop` — Get desktop streaming URL (`desktop`) | boat-v1.yaml | ADAPTED | noVNC viewer URL; `vnc=1`, `theme`, `publicAccess` honoured. |
| API-101 | `POST /sandboxes/{sandboxId}/host` — Expose a sandbox port on a public HTTPS URL (`hostPort`) | boat-v1.yaml | ADAPTED | LAN host-routed URL instead of public HTTPS on on.boat.dev. |
| API-102 | `POST /sandboxes/{sandboxId}/sshkey` — Configure sandbox SSH key (`sshKey`) | boat-v1.yaml | ADAPTED | `machineIp`/`sshEndpoint` = node direct port or coordinator TCP mapping. |
| API-103 | `GET /named-snapshots` — List named snapshots (`listNamedSnapshots`) | boat-v1.yaml | REQUIRED | Legacy alias of saved snapshots. |
| API-104 | `POST /named-snapshots` — Save a named snapshot (`saveNamedSnapshot`) | boat-v1.yaml | REQUIRED | Legacy alias. |
| API-105 | `GET /named-snapshots/{name}` — Get a named snapshot (`getNamedSnapshot`) | boat-v1.yaml | REQUIRED | Legacy alias. |
| API-106 | `DELETE /named-snapshots/{name}` — Remove a named snapshot (`deleteNamedSnapshot`) | boat-v1.yaml | REQUIRED | Legacy alias. |
| API-107 | `GET /saved-snapshots` — List saved snapshots (`listSavedSnapshots`) | boat-v1.yaml | REQUIRED |  |
| API-108 | `POST /saved-snapshots` — Save a saved snapshot (`saveSavedSnapshot`) | boat-v1.yaml | REQUIRED |  |
| API-109 | `GET /saved-snapshots/{name}` — Get a saved snapshot (`getSavedSnapshot`) | boat-v1.yaml | REQUIRED |  |
| API-110 | `DELETE /saved-snapshots/{name}` — Remove a saved snapshot (`deleteSavedSnapshot`) | boat-v1.yaml | REQUIRED |  |
| API-111 | `GET /saved-snapshots/{name}/shares` — List share codes of a saved snapshot (`listSnapshotShares`) | boat-v1.yaml | REQUIRED |  |
| API-112 | `POST /saved-snapshots/{name}/shares` — Share a saved snapshot (`createSnapshotShare`) | boat-v1.yaml | REQUIRED |  |
| API-113 | `DELETE /saved-snapshots/{name}/shares/{shareId}` — Revoke a share code (`revokeSnapshotShare`) | boat-v1.yaml | REQUIRED |  |
| API-114 | `GET /snapshots` — List snapshots (`listSnapshots`) | boat-v1.yaml | REQUIRED |  |
| API-115 | `GET /sandboxes/{sandboxId}/snapshots` — List boat snapshots (`listSandboxSnapshots`) | boat-v1.yaml | REQUIRED |  |
| API-116 | `DELETE /sandboxes/{sandboxId}/snapshots` — Permanently delete all snapshots of a sandbox (`deleteSandboxSnapshots`) | boat-v1.yaml | REQUIRED | Confirm header. |
| API-117 | `GET /sandboxes/{sandboxId}/snapshots/latest` — Get latest boat snapshot (`getLatestSandboxSnapshot`) | boat-v1.yaml | REQUIRED |  |
| API-118 | `GET /sandboxes/{sandboxId}/usage` — Get boat usage (`usage`) | boat-v1.yaml | ADAPTED | `dollars` = credits. |
| API-119 | `DELETE /snapshots/{snapshotId}` — Permanently delete snapshot data (`deleteSnapshot`) | boat-v1.yaml | REQUIRED | Confirm header. |
| API-120 | `GET /snapshots/{snapshotId}/tree` — Get snapshot file tree (`getSnapshotTree`) | boat-v1.yaml | REQUIRED |  |
| API-121 | `GET /snapshots/{snapshotId}/files` — Download a file or folder from a snapshot (`getSnapshotFile`) | boat-v1.yaml | REQUIRED |  |
| API-122 | `GET /snapshots/{snapshotId}/download` — Get snapshot download (`getSnapshotDownload`) | boat-v1.yaml | ADAPTED | Signed coordinator URLs for qcow2 layers + manifest/`reconstruct`. |
| API-123 | Webhook callback `deliverSandboxLifecycleEvent` (outbound POST to subscriber) | boat-v1.yaml | REQUIRED | See HOOK rows. |
| API-124 | Base path `/api/v1`, Bearer auth, JSON envelope `{ok, type, …}` with stable `type` discriminators (`sandbox.list`, `sandbox.created`, `prompt.queued`, …) | api__v1 | REQUIRED | Base `http(s)://<server>/api/v1`. |
| API-125 | Error envelope `{ok:false, type:"sandbox.error", status, code, message, error{code,message,status,details}, requestId}` and the documented status→code table | api__v1, boat-v1.yaml | REQUIRED | `details.setupUrl`/`billingUrl` point at the CrowdVM dashboard. |
| API-126 | `502 sandbox_direct_failed` with `details.retryable`; message `sandbox_restoring` = not run, retry 1–3 s | api__v1 | REQUIRED | Raised when node/guest hop fails. |
| API-127 | 503 codes `browser_not_ready`, `ssh_host_key_unavailable`; 403 `org_suspended`; 409 `account_not_ready`, `sandbox_not_promptable`, `resume_failed`, `fork_failed`, `sandbox_resuming`, `snapshots_disabled` | api__v1 | REQUIRED | `org_suspended`: admins suspend an organization (`POST /admin/orgs/{org}/suspend|unsuspend`); its creates, resumes, forks and CI jobs get 403 and its running sandboxes stop. `prompt_required`/`provider_required` (400) on `POST /prompt`. |
| API-128 | Cursor pagination: `limit` 1–200 (default 100), `sort` asc/desc, `pageInfo{nextCursor, hasMore, limit}` | api__v1 | REQUIRED | |
| API-129 | Idempotency: per-account keys kept 24 h; same body → same sandbox; `409 idempotency_in_progress` / `idempotency_key_reused`; failed create releases key in ~2 min | api__v1 | REQUIRED | |
| API-130 | `RateLimit-Policy` / `RateLimit` headers on every response; `Retry-After` on IP limit | api__v1, limits | REQUIRED | |
| API-131 | `GET /provider-models` live catalog | integrated-agents__harnesses-and-models, api__v1 | REQUIRED | Not in OpenAPI; add to CrowdVM OpenAPI. |
| API-132 | `POST /sandboxes/{id}/browser` → `browserUrl` | desktop-streaming__browser-only | ADAPTED | noVNC kiosk-browser view (DESK-014). |
| API-133 | Published OpenAPI document for client generation | api__v1, sdks__overview | ADAPTED | `/openapi/crowdvm-v1.yaml`, field-for-field with Boat where it makes sense. |
| API-134 | Docs "Try it" playground against production | api__v1 | ADAPTED | Optional: dashboard API explorer against the local coordinator. |
| API-135 | Account API `/api/boat/…` (teams, seats, billing, wallet, cli download, checkout) used by dashboard; not in v1 | organizations, billing, cli-reference | ADAPTED | CrowdVM internal dashboard API; only `/api/crowdvm/cli/download` and credits endpoints are needed. |
| API-136 | Agent-auth endpoints (`/auth.md`, well-known, `/agent/identity[/claim]`, `/oauth2/token`, `/oauth2/revoke`) | agent-auth | REQUIRED | See AUTH-010…017. |
| API-137 | Legacy Box API alias `/api/box/v1/boxes…` (21 paths), 14 JSON-key renames, 13 envelope types, 18 error codes, `box.*` webhook events, `X-Box-Org`, `box_` keys; `Deprecation`/`Sunset`/`Link` headers | migrating-from-box, migrating-from-box__renamed-names, migrating-from-box__steps | ADAPTED | Needed only so `harbor-box` and `@asciidev/eve-box` work with `base_url` → CrowdVM (EVAL-004/005): implement as a rename shim over `/api/v1`; webhooks with `box.*` names not needed. |
| API-138 | Box → boat customer migration (box CLI alias, `BOX_*` env fallbacks, `~/.config/ascii/box`, `.boxignore`, `box` skill removal, `on.ascii.dev`, `api-key --box`) | migrating-from-box* | N/A | Migration path for Boat's pre-rename customers; CrowdVM has no legacy installs. `.boxignore` is still honoured (SNAP-014). |
| API-139 | `X-Boat-Org` header (and `X-Box-Org` legacy) for wallet scope | organizations__active-wallet, migrating-from-box | REQUIRED | Accept `X-Boat-Org` (+ `X-CrowdVM-Org`). |
| API-140 | `Cache-Control: no-store` on deletion/retention responses | data-retention__delete | REQUIRED | |

## Security & isolation

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| SEC-001 | Each sandbox its own VM (kernel, disk, IP); no shared disk/processes; network-only reachability between sandboxes | security | ADAPTED | Separate QEMU VM per sandbox with its own qcow2 overlay; each VM has a private node-side userspace network (gVisor netstack over a `-netdev stream` socket), so guests cannot reach each other; its egress policy blocks this machine, LAN, tailnet, link-local and multicast by default (see ARCHITECTURE Networking) — contributor-host protection is a CrowdVM-specific requirement. |
| SEC-002 | Root in sandbox (passwordless sudo); boundary is between sandboxes/accounts | security | REQUIRED | |
| SEC-003 | One sandbox per end user guidance; no cleaning between users | security | REQUIRED | Documentation + behaviour. |
| SEC-004 | Who can reach: creator, org members per visibility, API key scopes, added SSH keys, token URL holders | security | REQUIRED | |
| SEC-005 | Credential inventory in sandbox (`ASCII_TOKEN`, in-sandbox key, env secrets, gh/git creds, AI logins) and what no-env removes | security | REQUIRED | See ENV/KEY rows. |
| SEC-006 | Inbound refused except SSH + platform ports; outbound open | security | ADAPTED | Inbound only via coordinator tunnels (and node direct ports when enabled); outbound NAT through the node. |
| SEC-007 | Token-carrying URLs table: protected URL, public URL, desktop (10 min), VNC desktop URL, signed file URLs (15 min–1 h) | security | REQUIRED | |
| SEC-008 | Data location Germany/Finland/France; snapshot copies outside EU | security, compliance | ADAPTED | Data on coordinator + nodes chosen by the operator; node labels (e.g. `site=…`) allow placement constraints instead of regions. |
| SEC-009 | SOC 2, 99.95% uptime, status page status.ascii.dev | compliance | N/A | Certification/SLA/hosted status page of a commercial operator; a self-hosted pool has none. Coordinator exposes `/healthz` and node health in dashboard instead (DASH rows). |
| SEC-010 | Security issue reporting via support | compliance | N/A | Operator process, not software. |
| SEC-011 | Contributor-host safety: VM cannot access host filesystem, host LAN-local services, or other VMs; resource caps enforced by node | (CrowdVM) | REQUIRED | Not in Boat (single operator) but mandatory for crowdsourced hosts. Network part: `internal/node/vm/netstack` egress policy (default deny host/LAN/tailnet/link-local; `net.allow-lan`, `net.allow`/`net.deny` owner overrides; server pinhole); verified by `TestE2ENet`. |
| SEC-012 | Guest agent auth: per-VM bearer token; agent port bound to node loopback | (CrowdVM) | REQUIRED | ARCHITECTURE contract. |
| SEC-013 | Secrets encrypted at rest (env secrets, credential profiles, telemetry headers, webhook secrets) | ai-credentials, opentelemetry, environments | REQUIRED | Coordinator master key. |
| SEC-014 | Node trust: nodes can read the disks of the VMs they host | (CrowdVM) | ADAPTED | Inherent to crowdsourcing; mitigate with node trust levels / owner-only placement option (`pin to my nodes`) and document clearly. |

## Pre-installed tools (base image)

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| TOOL-001 | Ubuntu 24.04 LTS, Linux 6.8, x86_64; user `user`, /home/user, passwordless sudo | machines__tools, machines | REQUIRED | |
| TOOL-002 | Node 24 (+nvm with 22, apt Node 20), npm, pnpm, bun/bunx, deno | machines__tools | REQUIRED | Exact versions may differ; "latest at image build". |
| TOOL-003 | Python 3.12 + pip/venv, uv | machines__tools | REQUIRED | |
| TOOL-004 | Go, Rust (rustup/cargo), Java 11 + Maven + Gradle, Kotlin, Scala + sbt, Ruby, PHP + composer, Erlang/Elixir, .NET 8, R, GCC/Clang/CMake/Ninja | machines__tools | REQUIRED | |
| TOOL-005 | Docker with BuildKit + Compose plugin; `user` in docker group | machines__tools | REQUIRED | |
| TOOL-006 | Coding agents: claude, codex, pi, opencode, hermes, openclaw, agent/cursor-agent, prime-agent, kimi, vibe | machines__tools | REQUIRED | Updated at image build (ARCHITECTURE: Claude Code, Codex, OpenCode baseline — the rest must be added for parity). |
| TOOL-007 | In-sandbox `host` CLI, `lux` (desktop/browser automation + recording), in-sandbox `boat` CLI (`box` alias) | machines__tools, migrating-from-box__steps | ADAPTED | `host` + `crowdvm` CLI (with `boat` alias for skills/scripts); `lux` is not obtainable (Ascii's proprietary agent on their hosted model): replaced by `ascii-record-desktop`/cua-driver, and v3 images ship a `lux` pointer that names them (exit 127). |
| TOOL-008 | git 2.43, gh | machines__tools | REQUIRED | |
| TOOL-009 | rg, jq, curl, google-chrome-stable, ffmpeg, xdotool, wmctrl | machines__tools | REQUIRED | |
| TOOL-010 | `LANG`/`LC_ALL=en_US.UTF-8` | machines__tools | REQUIRED | |
| TOOL-011 | Tools live in base image (`/usr/local`, `/opt`), take no snapshot space, user copies shadow them | machines__tools | REQUIRED | Base qcow2 backing file. |
| TOOL-012 | Tools updated "when boat prepares each machine" | machines__tools | ADAPTED | Updated when the base image is rebuilt (`crowdvm-node image build`) and distributed to nodes. |
| TOOL-013 | Desktop stack: Xorg `:0` 1920×1080, Chrome, VS Code, terminal; cua-driver | integrated-agents__computer-use, desktop-streaming | REQUIRED | Xfce + TigerVNC per ARCHITECTURE. |
| TOOL-014 | ufw firewall default-deny inbound; PHP apt repo present (affects Wine libgd3 pin) | hosting__firewall, windows-apps__install-wine | REQUIRED | |
| TOOL-015 | `/dev/kvm` in guest | machines__virtualization | ADAPTED | See SIZE-014. |

## Install scripts

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| INST-001 | `curl -fsSL https://boat.dev/install \| sh` (macOS/Linux) | quickstart, cli-reference | ADAPTED | `curl -fsSL http://<server>:8420/install \| sh` served by the coordinator, with the server URL baked in. |
| INST-002 | `irm https://boat.dev/install.ps1 \| iex` (Windows) | quickstart, cli-reference | ADAPTED | `irm http://<server>:8420/install.ps1 \| iex`. |
| INST-003 | Installer signs in via browser, asks "for you or your users", opens plan checkout | quickstart | ADAPTED | Device-code sign-in + third-party-safety question; no checkout. |
| INST-004 | Installer adds shell function (needed for `current`) and asks to open a new terminal | quickstart, cli-reference | REQUIRED | bash/zsh/fish/PowerShell. |
| INST-005 | Links binary into `~/.local/bin` for non-login shells (agents, CI) | cli-reference | REQUIRED | |
| INST-006 | macOS CLI support | quickstart | ADAPTED | darwin/amd64+arm64 CLI builds published by coordinator; untested in the single-machine setup. |
| INST-007 | Static CLI binaries per platform for Docker/CI | cli-reference, ci | ADAPTED | `/api/crowdvm/cli/download?platform=linux-x64\|linux-arm64\|windows-x64\|darwin-arm64`. |
| INST-008 | Second computer / reinstall finds existing account, skips completed steps | account | REQUIRED | |
| INST-009 | `boat-dev`/staging channels and `*_STAGING_*`/`*_DEV_*` env vars | migrating-from-box__renamed-names | N/A | Boat's internal release channels; a self-hosted coordinator is its own single channel. |
| INST-010 | Node app installer for contributor machines (Linux, Windows): join token, benchmark, capacity caps, service install | (CrowdVM) | REQUIRED | `crowdvm-node join --server … --token …`; CrowdVM substrate for Boat's "machines". |
| INST-011 | Coordinator install (`crowdvm-server`, optional `--tls-cert/--tls-key`) | (CrowdVM) | REQUIRED | |

## Dashboard

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| DASH-001 | Browser sign-in (local accounts) + `boat dashboard` one-time 15-min login link | cli-reference, account | ADAPTED | Local accounts instead of GitHub/Google. |
| DASH-002 | Sandboxes tab: list, state, Type column + filter, Cost column (month, UTC), labels by/view only/private/shared, Org scope | machines, billing, organizations__shared-sandboxes | ADAPTED | Cost in credits. |
| DASH-003 | Sandbox row menu: Copy SSH, open Desktop, Browser, History (live output, stop), AI usage, Recover, Make private / Share with org, Delete permanently…, No auto-stop | ssh-access, desktop-streaming*, long-running-tasks__command-history, ai-credentials, troubleshooting, data-retention__delete, long-running-tasks__auto-stop | REQUIRED | |
| DASH-004 | Per-sandbox metrics graphs (CPU/RAM/disk/IO) — dashboard only | account, data-retention__snapshots-and-monitoring-off | REQUIRED | From monitoring samples. |
| DASH-005 | Snapshots tab: browse/download files, delete snapshots, select all, saved snapshots + allowance | snapshots__download-and-delete, data-retention | REQUIRED | |
| DASH-006 | Environment tab: environments, safe switch, toggles, vars/secret files, repos + Setup scripts (Blocking), Versions panel + Upgrade, access panel, "Connect GitHub" | environments* , setup | ADAPTED | "Connect GitHub" → add git host token. |
| DASH-007 | Agents & Proxying tab: profile picker, sign-ins/keys/accounts (switch/add/forget), Get-a-key links, default harness/model/fast, Proxying tab (switch, custom APIs, usage), env-blocks-keys warnings | integrated-agents__keys, ai-credentials | ADAPTED | Subscription sign-in per AGT-030. |
| DASH-008 | API Keys tab (presets, raw actions, sandbox/env selectors, expiry presets, revoke, usage) | api-keys__create-and-list | REQUIRED | |
| DASH-009 | Webhooks & OTel tab (create, rotate, remove, Deliveries log; OTLP endpoint) | webhooks, opentelemetry | REQUIRED | |
| DASH-010 | CI on Boat VMs tab (setup steps, repos, jobs with wait/run/cost/log, refusal reasons, public-repo switch with user check, link existing installation) | ci | ADAPTED | Per CI-001/002. |
| DASH-011 | Account tab: connections (add/remove/primary), Data & privacy (ZDR, Snapshots off, Monitoring off), close account / reopen / GDPR erase | account, data-retention* | ADAPTED | Connections limited to local methods. |
| DASH-012 | Organization tab: members, invites, roles, remove member, per-member caps, rename, default visibility, Viewing dropdown, "CLI and API keys bill … Change" | organizations* | REQUIRED | |
| DASH-013 | Billing tab: plan, Pay, auto top-up, cards, Manage billing (Stripe), cost by sandbox, seats | billing*, organizations__seats-and-limits | ADAPTED | Becomes Credits page: balance, ledger (earned by nodes / spent / granted), cost by sandbox; payment controls N/A (BILL-004/020). |
| DASH-014 | Alerts tab (planNudge e-mails, low balance, disk 85/95%, unresponsive, refused stop) | limits, troubleshooting, snapshots__stop | ADAPTED | In-dashboard notifications + optional SMTP. |
| DASH-015 | Agent claim page (`/agent/identity/claim`), "Continue with AgentID" | agent-auth | ADAPTED | Claim page REQUIRED; AgentID button N/A (AUTH-004). |
| DASH-016 | Nodes page: contributed machines, benchmark, capacity, hosted VMs, earned credits, health; admin: join tokens, drain/evict | (CrowdVM) | REQUIRED | CrowdVM-specific operator surface. |
| DASH-017 | Node local status UI on each contributing machine | (CrowdVM) | REQUIRED | ARCHITECTURE: node local status UI. |

## Docs for AI agents (from ai-tools page)

| ID | Feature | Boat source | Class | CrowdVM mapping / note |
|---|---|---|---|---|
| DOCS-001 | `llms.txt`, `llms-full.txt`, `.md` per page | ai-tools | ADAPTED | The coordinator serves `/llms.txt` (index, one line per page), `/llms-full.txt` and `/docs/<page>.md` (`/docs` = index; aliases such as `quickstart`): the user guide embedded from `docs/guide`, `cli-reference.md` generated from the CLI's command tree (`go generate ./docs`, `make docs`) and an API reference rendered from the live OpenAPI document. Public, no auth; not shadowed by the dashboard (`feat_docs.go`). |
| DOCS-002 | Docs MCP server (`/mcp`, search + read pages) | ai-tools | ADAPTED | Docs MCP server on the coordinator at `/mcp` (streamable HTTP, JSON responses, stateless): tools `search_docs`, `read_doc`, `list_docs`; every page also as a resource `crowdvm-docs://<page>`. `claude mcp add --transport http crowdvm-docs http://<server>/mcp`. |
| DOCS-003 | Skill pack (`npx skills add …`, 13 skills) and in-sandbox `boat` skill | ai-tools, integrated-agents__customize | ADAPTED | Repo `skills/` folder (Go package `skills`, for `crowdvm skill install`); in sandboxes the guest installs it into every harness's skills dir. |
| DOCS-004 | AGENTS.md snippet for local agents | ai-tools | ADAPTED | Same snippet with `crowdvm` commands. |
| DOCS-005 | Support channels (email, Discord) | support | N/A | Operator/community process, not software. |

## Page index (all 219 Boat pages → sections)

| Boat page (`.ref/pages/<name>.md`) | PARITY section(s) |
|---|---|
| account | Accounts, sign-in, agent auth; CLI (`crowdvm`, mirrors `boat`); Dashboard; Install scripts; Organizations / teams |
| agent-auth | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Accounts, sign-in, agent auth; CLI (`crowdvm`, mirrors `boat`); Dashboard |
| ai-credentials | AI credentials & proxy; Dashboard; Security & isolation |
| ai-tools | Docs for AI agents (from ai-tools page) |
| api-keys | API keys & scopes |
| api-keys__create-and-list | API keys & scopes; Accounts, sign-in, agent auth; CLI (`crowdvm`, mirrors `boat`); Dashboard |
| api-keys__scopes | API keys & scopes; CLI (`crowdvm`, mirrors `boat`) |
| api-keys__store-rotate-revoke | API keys & scopes; CLI (`crowdvm`, mirrors `boat`) |
| api__reference__account__create-scoped-api-key | API (API-012: `POST /api-keys/scoped`) |
| api__reference__account__create-webhook | API (API-017: `POST /webhooks`) |
| api__reference__account__delete-webhook | API (API-020: `DELETE /webhooks/{webhookId}`) |
| api__reference__account__get-account-data-retention-policy | API (API-005: `GET /account/data-retention`) |
| api__reference__account__get-api-key-usage | API (API-015: `GET /api-keys/{apiKeyId}/usage`) |
| api__reference__account__get-boat-limits | API (API-008: `GET /limits`) |
| api__reference__account__get-boat-secrets-setup | API (API-047: `GET /secrets`) |
| api__reference__account__get-current-boat-user | API (API-001: `GET /me`) |
| api__reference__account__get-deletion-operation | API (API-007: `GET /deletion-operations/{operationId}`) |
| api__reference__account__get-opentelemetry-export | API (API-022: `GET /telemetry`) |
| api__reference__account__get-webhook | API (API-018: `GET /webhooks/{webhookId}`) |
| api__reference__account__list-api-keys | API (API-011: `GET /api-keys`) |
| api__reference__account__list-github-repositories-available-to-sandbox | API (API-009: `GET /repos`) |
| api__reference__account__list-organizations | API (API-002: `GET /orgs`) |
| api__reference__account__list-webhooks | API (API-016: `GET /webhooks`) |
| api__reference__account__remove-opentelemetry-export | API (API-024: `DELETE /telemetry`) |
| api__reference__account__revoke-api-key | API (API-013: `DELETE /api-keys/{apiKeyId}`) |
| api__reference__account__rotate-api-key | API (API-014: `POST /api-keys/{apiKeyId}/rotate`) |
| api__reference__account__rotate-webhook-signing-secret | API (API-021: `POST /webhooks/{webhookId}/rotate`) |
| api__reference__account__select-repository-for-sandboxes | API (API-010: `POST /repos`) |
| api__reference__account__set-active-organization | API (API-004: `PATCH /orgs/active`) |
| api__reference__account__set-opentelemetry-export | API (API-023: `PUT /telemetry`) |
| api__reference__account__update-account-data-retention-policy | API (API-006: `PATCH /account/data-retention`) |
| api__reference__account__update-boat-secrets-setup | API (API-048: `POST /secrets`) |
| api__reference__account__update-organization | API (API-003: `PATCH /orgs/{org}`) |
| api__reference__account__update-webhook | API (API-019: `PATCH /webhooks/{webhookId}`) |
| api__reference__agent__attach-command | API (API-092: `GET /sandboxes/{sandboxId}/commands/{processId}/stream`) |
| api__reference__agent__configure-sandbox-ssh-key | API (API-102: `POST /sandboxes/{sandboxId}/sshkey`) |
| api__reference__agent__create-download-url | API (API-087: `POST /sandboxes/{sandboxId}/files/download-url`) |
| api__reference__agent__create-sandbox-folder | API (API-083: `POST /sandboxes/{sandboxId}/files/folder`) |
| api__reference__agent__create-upload-url | API (API-088: `POST /sandboxes/{sandboxId}/files/upload-url`) |
| api__reference__agent__delete-sandbox-file | API (API-078: `DELETE /sandboxes/{sandboxId}/files`) |
| api__reference__agent__download-sandbox-artifact | API (API-097: `GET /sandboxes/{sandboxId}/artifacts`) |
| api__reference__agent__execute-sandbox-command | API (API-089: `POST /sandboxes/{sandboxId}/commands`) |
| api__reference__agent__expose-a-port | API (API-101: `POST /sandboxes/{sandboxId}/host`) |
| api__reference__agent__find-in-sandbox-files | API (API-082: `GET /sandboxes/{sandboxId}/files/find`) |
| api__reference__agent__get-command | API (API-094: `GET /sandboxes/{sandboxId}/command-history/{commandId}`) |
| api__reference__agent__get-command-status | API (API-090: `GET /sandboxes/{sandboxId}/commands/{processId}`) |
| api__reference__agent__get-desktop-streaming-url | API (API-100: `POST /sandboxes/{sandboxId}/desktop`) |
| api__reference__agent__get-prompt-run-status | API (API-074: `GET /sandboxes/{sandboxId}/prompts/{promptId}`) |
| api__reference__agent__get-sandbox-file-info | API (API-080: `GET /sandboxes/{sandboxId}/files/info`) |
| api__reference__agent__interrupt-running-agent-work | API (API-099: `POST /sandboxes/{sandboxId}/interrupt`) |
| api__reference__agent__list-command-history | API (API-093: `GET /sandboxes/{sandboxId}/command-history`) |
| api__reference__agent__list-sandbox-conversations | API (API-073: `GET /sandboxes/{sandboxId}/conversations`) |
| api__reference__agent__list-sandbox-events | API (API-072: `GET /sandboxes/{sandboxId}/events`) |
| api__reference__agent__list-sandbox-files | API (API-079: `GET /sandboxes/{sandboxId}/files/list`) |
| api__reference__agent__list-sandbox-processes | API (API-096: `GET /sandboxes/{sandboxId}/processes`) |
| api__reference__agent__move-sandbox-file | API (API-084: `POST /sandboxes/{sandboxId}/files/move`) |
| api__reference__agent__prompt-sandbox-agent | API (API-071: `POST /sandboxes/{sandboxId}/prompt`) |
| api__reference__agent__read-command-logs | API (API-095: `GET /sandboxes/{sandboxId}/command-history/{commandId}/logs`) |
| api__reference__agent__read-sandbox-file | API (API-076: `GET /sandboxes/{sandboxId}/files`) |
| api__reference__agent__replace-in-sandbox-files | API (API-086: `POST /sandboxes/{sandboxId}/files/replace`) |
| api__reference__agent__search-sandbox-files | API (API-081: `GET /sandboxes/{sandboxId}/files/search`) |
| api__reference__agent__set-sandbox-file-permissions | API (API-085: `POST /sandboxes/{sandboxId}/files/permissions`) |
| api__reference__agent__steer-a-running-turn | API (API-098: `POST /sandboxes/{sandboxId}/steer`) |
| api__reference__agent__stop-command | API (API-091: `DELETE /sandboxes/{sandboxId}/commands/{processId}`) |
| api__reference__agent__write-sandbox-file | API (API-077: `PUT /sandboxes/{sandboxId}/files`) |
| api__reference__ai-credentials__add-credential-account | API (API-037: `POST /credential-profiles/{profile}/accounts/{credential}`) |
| api__reference__ai-credentials__block-or-unblock-sandbox-ai-access | API (API-046: `PUT /sandboxes/{sandboxId}/ai-access`) |
| api__reference__ai-credentials__create-credential-profile | API (API-031: `POST /credential-profiles`) |
| api__reference__ai-credentials__delete-credential-profile | API (API-033: `DELETE /credential-profiles/{profile}`) |
| api__reference__ai-credentials__forget-credential-account | API (API-039: `DELETE /credential-profiles/{profile}/accounts/{credential}/{account}`) |
| api__reference__ai-credentials__get-ai-credentials-proxy | API (API-042: `GET /ai-proxy`) |
| api__reference__ai-credentials__get-ai-usage | API (API-044: `GET /ai-usage`) |
| api__reference__ai-credentials__get-sandbox-ai-usage | API (API-045: `GET /sandboxes/{sandboxId}/ai-usage`) |
| api__reference__ai-credentials__list-credential-accounts | API (API-036: `GET /credential-profiles/{profile}/accounts/{credential}`) |
| api__reference__ai-credentials__list-credential-profiles | API (API-030: `GET /credential-profiles`) |
| api__reference__ai-credentials__remove-credential-profile-credential | API (API-035: `DELETE /credential-profiles/{profile}/keys/{credential}`) |
| api__reference__ai-credentials__remove-custom-api | API (API-041: `DELETE /credential-profiles/{profile}/custom-apis/{name}`) |
| api__reference__ai-credentials__set-credential-profile-api-key | API (API-034: `PUT /credential-profiles/{profile}/keys/{credential}`) |
| api__reference__ai-credentials__set-custom-api | API (API-040: `PUT /credential-profiles/{profile}/custom-apis/{name}`) |
| api__reference__ai-credentials__switch-credential-account | API (API-038: `PUT /credential-profiles/{profile}/accounts/{credential}/{account}/active`) |
| api__reference__ai-credentials__turn-the-ai-credentials-proxy-on-or-off | API (API-043: `PUT /ai-proxy`) |
| api__reference__ai-credentials__update-credential-profile | API (API-032: `PUT /credential-profiles/{profile}`) |
| api__reference__ci__connect-ci-github | API (API-026: `POST /ci/connect`) |
| api__reference__ci__get-ci-job | API (API-028: `GET /ci/jobs/{jobId}`) |
| api__reference__ci__get-ci-job-log | API (API-029: `GET /ci/jobs/{jobId}/logs`) |
| api__reference__ci__get-ci-overview | API (API-025: `GET /ci`) |
| api__reference__ci__set-ci-public-repos | API (API-027: `POST /ci/public`) |
| api__reference__environments__add-environment-repo | API (API-058: `POST /environments/{environmentId}/repos`) |
| api__reference__environments__create-sandbox-environment | API (API-050: `POST /environments`) |
| api__reference__environments__delete-environment-repo | API (API-059: `DELETE /environments/{environmentId}/repos/{repositoryId}`) |
| api__reference__environments__delete-environment-secret-file | API (API-057: `DELETE /environments/{environmentId}/secret-files`) |
| api__reference__environments__delete-environment-var | API (API-055: `DELETE /environments/{environmentId}/vars/{key}`) |
| api__reference__environments__delete-sandbox-environment | API (API-052: `DELETE /environments/{environmentId}`) |
| api__reference__environments__list-sandbox-environments | API (API-049: `GET /environments`) |
| api__reference__environments__set-environment-secret-file | API (API-056: `PUT /environments/{environmentId}/secret-files`) |
| api__reference__environments__set-environment-var | API (API-054: `PUT /environments/{environmentId}/vars/{key}`) |
| api__reference__environments__update-sandbox-environment | API (API-051: `PUT /environments/{environmentId}`) |
| api__reference__environments__upgrade-sandbox-environment | API (API-053: `POST /environments/{environmentId}/upgrade`) |
| api__reference__sandboxes__create-sandbox | API (API-061: `POST /sandboxes`) |
| api__reference__sandboxes__fork-sandbox | API (API-070: `POST /sandboxes/{sandboxId}/fork`) |
| api__reference__sandboxes__get-sandbox | API (API-062: `GET /sandboxes/{sandboxId}`) |
| api__reference__sandboxes__get-sandbox-usage | API (API-118: `GET /sandboxes/{sandboxId}/usage`) |
| api__reference__sandboxes__get-stop | API (API-075: `GET /sandboxes/{sandboxId}/stops/{stopId}`) |
| api__reference__sandboxes__list-sandboxes | API (API-060: `GET /sandboxes`) |
| api__reference__sandboxes__permanently-delete-sandbox-data | API (API-064: `DELETE /sandboxes/{sandboxId}`) |
| api__reference__sandboxes__recover-sandbox | API (API-068: `POST /sandboxes/{sandboxId}/recover`) |
| api__reference__sandboxes__resume-sandbox | API (API-069: `POST /sandboxes/{sandboxId}/resume`) |
| api__reference__sandboxes__share-sandbox | API (API-066: `POST /sandboxes/{sandboxId}/share`) |
| api__reference__sandboxes__stop-and-archive-sandbox | API (API-065: `POST /sandboxes/{sandboxId}/stop`) |
| api__reference__sandboxes__unshare-sandbox | API (API-067: `POST /sandboxes/{sandboxId}/unshare`) |
| api__reference__sandboxes__update-sandbox | API (API-063: `PATCH /sandboxes/{sandboxId}`) |
| api__reference__snapshots__create-snapshot-share | API (API-112: `POST /saved-snapshots/{name}/shares`) |
| api__reference__snapshots__delete-sandbox-snapshots | API (API-116: `DELETE /sandboxes/{sandboxId}/snapshots`) |
| api__reference__snapshots__delete-saved-snapshot | API (API-110: `DELETE /saved-snapshots/{name}`) |
| api__reference__snapshots__get-latest-sandbox-snapshot | API (API-117: `GET /sandboxes/{sandboxId}/snapshots/latest`) |
| api__reference__snapshots__get-saved-snapshot | API (API-109: `GET /saved-snapshots/{name}`) |
| api__reference__snapshots__get-snapshot-download | API (API-122: `GET /snapshots/{snapshotId}/download`) |
| api__reference__snapshots__get-snapshot-file | API (API-121: `GET /snapshots/{snapshotId}/files`) |
| api__reference__snapshots__get-snapshot-tree | API (API-120: `GET /snapshots/{snapshotId}/tree`) |
| api__reference__snapshots__list-sandbox-snapshots | API (API-115: `GET /sandboxes/{sandboxId}/snapshots`) |
| api__reference__snapshots__list-saved-snapshots | API (API-107: `GET /saved-snapshots`) |
| api__reference__snapshots__list-snapshot-shares | API (API-111: `GET /saved-snapshots/{name}/shares`) |
| api__reference__snapshots__list-snapshots | API (API-114: `GET /snapshots`) |
| api__reference__snapshots__permanently-delete-snapshot-data | API (API-119: `DELETE /snapshots/{snapshotId}`) |
| api__reference__snapshots__revoke-snapshot-share | API (API-113: `DELETE /saved-snapshots/{name}/shares/{shareId}`) |
| api__reference__snapshots__save-saved-snapshot | API (API-108: `POST /saved-snapshots`) |
| api__v1 | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour) |
| billing | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Billing & limits (→ credits); CLI (`crowdvm`, mirrors `boat`); Dashboard |
| billing__plan-changes | Billing & limits (→ credits); Dashboard |
| ci | CI; CLI (`crowdvm`, mirrors `boat`); Dashboard; Install scripts; Windows apps (Wine) |
| cli-reference | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Accounts, sign-in, agent auth; CLI (`crowdvm`, mirrors `boat`); Commands & processes; Dashboard; Desktop & browser streaming; Environments, secrets, repositories, registries, setup; Files; Install scripts; Integrated agents (providers, conversations, steering, computer use, customize, keys); Organizations / teams; Public URLs, ports, firewall; SSH, SCP, port forwarding, editors; Sandboxes & lifecycle; Sizes / machines; Snapshots, saved snapshots & shares |
| compliance | Security & isolation |
| concepts | Sandboxes & lifecycle; Snapshots, saved snapshots & shares |
| data-retention | Dashboard; Data retention & deletion |
| data-retention__close-account | Dashboard; Data retention & deletion |
| data-retention__delete | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Dashboard; Data retention & deletion; Snapshots, saved snapshots & shares |
| data-retention__snapshots-and-monitoring-off | Commands & processes; Dashboard; Data retention & deletion; Snapshots, saved snapshots & shares |
| data-retention__zero-data-retention | Dashboard; Data retention & deletion |
| desktop-streaming | Dashboard; Desktop & browser streaming; Pre-installed tools (base image) |
| desktop-streaming__background | Dashboard; Desktop & browser streaming |
| desktop-streaming__browser-only | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Dashboard; Desktop & browser streaming |
| desktop-streaming__drive-and-record | Dashboard; Desktop & browser streaming |
| desktop-streaming__urls-and-security | Dashboard; Desktop & browser streaming |
| environments | Dashboard; Environments, secrets, repositories, registries, setup; Security & isolation |
| environments__access | Dashboard; Environments, secrets, repositories, registries, setup |
| environments__choose | Dashboard; Sandboxes & lifecycle |
| environments__manage | Dashboard; Environments, secrets, repositories, registries, setup |
| environments__private-registries | Dashboard; Environments, secrets, repositories, registries, setup |
| environments__repositories | Dashboard; Environments, secrets, repositories, registries, setup |
| environments__secrets | Dashboard; Environments, secrets, repositories, registries, setup |
| environments__versions | Dashboard; Environments, secrets, repositories, registries, setup |
| evals | Evals / RL / Harbor / Eve / AgentEnv adapters |
| faq | Sandboxes & lifecycle |
| free-trial | Accounts, sign-in, agent auth; Billing & limits (→ credits); Long-running, auto-stop, always-on |
| hosting | Public URLs, ports, firewall |
| hosting__firewall | Pre-installed tools (base image); Public URLs, ports, firewall |
| hosting__host-cli | Public URLs, ports, firewall |
| hosting__how-it-works | Public URLs, ports, firewall |
| integrated-agents | Integrated agents (providers, conversations, steering, computer use, customize, keys) |
| integrated-agents__computer-use | Integrated agents (providers, conversations, steering, computer use, customize, keys); Pre-installed tools (base image) |
| integrated-agents__conversations | Integrated agents (providers, conversations, steering, computer use, customize, keys) |
| integrated-agents__customize | Docs for AI agents (from ai-tools page); Integrated agents (providers, conversations, steering, computer use, customize, keys) |
| integrated-agents__harnesses-and-models | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Integrated agents (providers, conversations, steering, computer use, customize, keys) |
| integrated-agents__keys | Dashboard; Integrated agents (providers, conversations, steering, computer use, customize, keys) |
| integrated-agents__steering | Integrated agents (providers, conversations, steering, computer use, customize, keys) |
| integrations__agentenv | Evals / RL / Harbor / Eve / AgentEnv adapters |
| integrations__eve | Evals / RL / Harbor / Eve / AgentEnv adapters |
| integrations__harbor | Evals / RL / Harbor / Eve / AgentEnv adapters |
| limits | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Billing & limits (→ credits); Dashboard; Sandboxes & lifecycle |
| long-running-tasks | Commands & processes |
| long-running-tasks__always-on-services | Long-running, auto-stop, always-on |
| long-running-tasks__auto-stop | Dashboard; Integrated agents (providers, conversations, steering, computer use, customize, keys); Long-running, auto-stop, always-on |
| long-running-tasks__background-commands | Commands & processes; Long-running, auto-stop, always-on |
| long-running-tasks__command-history | Commands & processes; Dashboard |
| machines | Dashboard; Pre-installed tools (base image); Sandboxes & lifecycle; Sizes / machines |
| machines__change-size | Sizes / machines |
| machines__fail-fast | Sizes / machines |
| machines__tools | Pre-installed tools (base image) |
| machines__virtualization | Pre-installed tools (base image); Sandboxes & lifecycle; Sizes / machines |
| migrating-from-box | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); SDKs (Python / TypeScript) |
| migrating-from-box__renamed-names | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Install scripts |
| migrating-from-box__steps | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Pre-installed tools (base image); SDKs (Python / TypeScript) |
| opentelemetry | Dashboard; OpenTelemetry; Security & isolation |
| organizations | AI credentials & proxy; API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Dashboard; Organizations / teams |
| organizations__active-wallet | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); Dashboard; Organizations / teams |
| organizations__move-personal-plan | Dashboard; Organizations / teams |
| organizations__seats-and-limits | Dashboard; Organizations / teams |
| organizations__shared-sandboxes | Dashboard; Organizations / teams |
| platform-guide | Sandboxes & lifecycle |
| platform-guide__agents | Integrated agents (providers, conversations, steering, computer use, customize, keys); Sandboxes & lifecycle |
| platform-guide__patterns | Sandboxes & lifecycle |
| platform-guide__templates-and-sizing | Sandboxes & lifecycle |
| pricing | Billing & limits (→ credits); Sizes / machines |
| quickstart | Accounts, sign-in, agent auth; Install scripts |
| readiness | Sandboxes & lifecycle |
| recipes__agent-on-repo | Environments, secrets, repositories, registries, setup |
| recipes__parallel-agents | Integrated agents (providers, conversations, steering, computer use, customize, keys) |
| sdks__overview | API (every endpoint in `.ref/boat-v1.yaml`, plus API-wide behaviour); SDKs (Python / TypeScript) |
| sdks__python | SDKs (Python / TypeScript) |
| sdks__typescript | SDKs (Python / TypeScript) |
| security | Security & isolation |
| setup | CLI (`crowdvm`, mirrors `boat`); Dashboard; Environments, secrets, repositories, registries, setup; Integrated agents (providers, conversations, steering, computer use, customize, keys) |
| snapshots | Snapshots, saved snapshots & shares |
| snapshots__download-and-delete | Dashboard; Snapshots, saved snapshots & shares |
| snapshots__resume-and-fork | Snapshots, saved snapshots & shares |
| snapshots__stop | Dashboard; Sandboxes & lifecycle; Snapshots, saved snapshots & shares |
| snapshots__templates | Organizations / teams; SDKs (Python / TypeScript); Snapshots, saved snapshots & shares |
| snapshots__whats-saved | Snapshots, saved snapshots & shares |
| ssh-access | Dashboard; Files; SSH, SCP, port forwarding, editors |
| ssh-access__editors | SSH, SCP, port forwarding, editors |
| support | Docs for AI agents (from ai-tools page) |
| troubleshooting | Dashboard; Sandboxes & lifecycle |
| webhooks | Dashboard; Sandboxes & lifecycle; Webhooks |
| windows-apps | Windows apps (Wine) |
| windows-apps__agents-and-forks | Windows apps (Wine) |
| windows-apps__install-wine | Pre-installed tools (base image); Windows apps (Wine) |
| windows-apps__run-apps | Windows apps (Wine) |
| windows-apps__tested-apps | Windows apps (Wine) |
| windows-apps__troubleshooting | Windows apps (Wine) |
