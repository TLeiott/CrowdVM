# Review sonnet-2: in-VM experience, agent features, crowd requirements

Reviewer: Sonnet 5.5, independent. Date: 2026-11-08. Machine: Ryzen 7 5700X, KVM, QEMU 11.1.3, RTX 3080, Tailscale interface present.

## Verdict: DISAGREE (narrowly)

CrowdVM is very close. Almost everything I probed in the sandbox, desktop, agent, snapshot and crowd areas works as Boat describes. But I found one verified broken Boat behaviour and several Boat features that are supportable on a LAN and are absent, while PARITY.md marks them REQUIRED or ADAPTED. There is no large structural hole. The gaps below are the delta between "very good" and "every supportable feature".

## Method

* `make build` clean. `GOOS=windows go build/vet ./internal/node/...` and `./cmd/crowdvm-node ./cmd/crowdvm` also clean.
* Own cluster on ports 9400–9411, data under `~/.local/share/crowdvm-dev/review-sonnet-2` (never 8420, at most 3 VMs). Real node join with benchmark, image import and push, then real VMs from `ubuntu-24.04-crowdvm-v2`.
* Read the Boat pages for tools, desktop streaming (all 5), windows-apps, integrated agents (all 7), ai-credentials, long-running tasks, snapshots, environments, setup, hosting, ssh-access. Compared them with PARITY rows and with live behaviour.
* Live probes (all through the `crowdvm` CLI or API):
  * Tools inventory (about 70 binaries).
  * Desktop, browser view, screenshot and screen recording.
  * Real agent prompts with the free OpenCode model, including `computer` MCP use.
  * Steer and interrupt.
  * Stop, resume, fork, saved snapshot and deploy.
  * Process-output capture and history while stopped.
  * `host`, including `--localhost`.
  * In-sandbox `crowdvm` CLI.
  * Boat's `install-wine.sh` run unchanged.
  * Node benchmark over loopback and over the Tailscale IP.
* E2E groups run (isolated dirs): `desktop`, `agent`, `nodes`, `network`, `ai-proxy`, `environments`, `saved-snapshots`, `webhooks`, `lifecycle`, `snapshots`, `host`, `ssh`. All PASS.
  * The first `lifecycle` run failed. This was a harness dependency, not a product bug: it needs the webhook created by the `webhooks` group. It passes when run together.
* Not verifiable here: any Windows node (no Windows machine), real Claude/Codex subscription sign-in, GPU passthrough.

### What I verified works

| Area | Evidence |
|---|---|
| Tool list (machines__tools) | Every row present: node, bun, deno, pnpm, uv, go, rust, java, kotlin, scala, ruby, php, erlang, dotnet, R, clang, docker 29, gh, rg, jq, Chrome 157, ffmpeg, xdotool, wmctrl, claude, codex, pi, opencode, hermes, openclaw, agent/cursor-agent, prime-agent, kimi, vibe, `host`, `boat`/`box`/`crowdvm`, `ascii-record-desktop`. `LANG=en_US.UTF-8`, passwordless sudo, `/dev/kvm` usable (`KVM_GET_API_VERSION`=12), real systemd. Only `lux` is missing (see M4). |
| Desktop | `desktop`/`browser --no-open` print URLs. Screenshot API returns a 1920×1080 PNG. `ascii-record-desktop start/stop/status` produces a 1920×1080 mp4. `DISPLAY=:0 wmctrl` works. `browser --profile /data/x` is accepted and `--profile /home/user/x` is refused with `invalid_profile`. Viewer reads `#token`, strips it, and supports `overlay=0` and `theme`. |
| Computer use | `cua-driver.service` running. `computer` MCP registered in `~/.claude.json`, `~/.codex/config.toml`, `~/.pi`, `~/.kimi-code`, opencode.json and the others. A real OpenCode turn called `list_windows` and returned the real window titles. A second turn opened example.com in Chrome and returned "Example Domain". |
| Agents | `crowdvm prompt --help` prints the live catalog. `provider_not_configured` 409 and `fast_mode_not_supported` 400 (with the model list) match Boat. Steer returns `mode:"fallback", native:false` for OpenCode. Interrupt works. Conversations and events list correctly, including while stopped. |
| Long-running | Detached command history, plus capture of a systemd service's stdout as a `process` record (nohup to `/dev/null` is correctly not captured). `history` and `ps` work while stopped (`live:false`). Pre-stop hook `~/.boat/pre-stop` ran. `/tmp` was not kept. Enabled systemd service came back after stop and resume. Resume takes about 3 s. |
| Snapshots | Stop, resume, live fork (4 s), saved snapshot, deploy `--from` (4 s), share and scrub (e2e). |
| Windows apps | Boat's `install-wine.sh` unchanged: EXIT=0, `wine-11.21 (Staging)`. Notepad opened as a window on `:0`. Saved snapshot of the Wine sandbox (5.9 GB) created in 2 s. |
| Hosting | `host`, `host --localhost` (302 with token redirect), in-sandbox `host` and the in-sandbox `crowdvm` CLI (`list`, `info`) via `CROWDVM_API_URL=http://10.0.2.2:<port>`. |
| Crowd | Node benchmark: CPU, memory, disk, network (to the server, `via: loopback` or `tailscale`), GPU (RTX 3080, VRAM, driver, compute capability), virtualization (kvm, nested). Categories, score and tier come out. Server listening on the Tailscale IP with a node joining over it works. Placement flags and `nodes` summary work. Windows: node cross-compiles. `sys_windows.go` implements disk and GPU probing (nvidia-smi and `Win32_VideoController`). WHPX accel is chosen on Windows. QMP uses TCP. There is an SCM service and `install.ps1`. |

## Missing or broken supportable features

### M1. Docker containers do not start again after stop/resume — BROKEN (high)

* Boat pages: `snapshots__whats-saved` (lines 79 and 94), `readiness` (line 47), `concepts`, `faq`, `snapshots`: "Docker containers that ran start again by themselves."
* Observed, live. Created `docker run -d --name plain nginx:alpine` (no restart policy) and `--restart unless-stopped`. After `crowdvm stop` and `crowdvm resume` and a 25 s wait, both were `Exited (0)`. Only a container with `--restart always` came back, and that is only a side effect of the policy.
* Cause: `handlePrepareStop` (`internal/guest/internal.go:128-136`, `dockerStopAll` at line 243) runs `docker stop -t 30` on every container. That marks them manually stopped, so Docker never restarts them. Nothing records "ran at stop" to restart them at boot. `grep` of `internal/guest` finds no code that starts containers on resume.
* Not covered by any test: `grep -ci docker test/e2e/*.go` is 0 in every file.
* Fix is small: record the IDs that were running before `docker stop`, and `docker start` them on boot (or use `docker stop` only after writing a list and start the list in the guest boot path).

### M2. `--gpu` is placement only: the VM gets no GPU (medium)

* Crowd requirement: nodes are benchmarked and categorized including GPU, and agents ask for more compute. `crowdvm new --gpu` is documented as "a node with a GPU" (`docs/guide/cli-for-agents.md:87`).
* Observed: on the RTX 3080 node the VM has only virtio devices. `lspci` shows no GPU, `/dev/dri` is absent, and no `nvidia-smi`. `grep -ri 'vfio\|virtio-gpu\|venus' internal` finds nothing.
* Boat itself has no GPU (`machines.md`), so this is not Boat parity. It is a crowd-specific expectation. Detection and categorization are done and good. Exposure is not (VFIO passthrough for a spare GPU, or at least an honest doc and CLI note).
* Not impossible under the LAN/one-machine constraint, only hardware-dependent. At minimum the docs and `--gpu` help should say the sandbox sees no GPU, or the feature should be listed as a gap. PARITY SIZE-005 says "no GPU" REQUIRED while README and the guides advertise GPU placement.

### M3. Docs-for-agents features are not served (low-medium)

* Boat page `ai-tools`. PARITY DOCS-001/002/003 are ADAPTED, promising:
  * `/llms.txt`, `/llms-full.txt` and `/docs/<page>.md` on the coordinator.
  * A docs MCP server at `/mcp`.
  * A `crowdvm` skill shipped in the image and in a repo `skills/` folder.
* Observed against my server:
  * `GET /llms.txt`, `/llms-full.txt` and `/docs/quickstart.md` all return 404. `/mcp` returns the SPA HTML, and POST gives 405.
  * No `skills/` folder in the repo.
  * No skill in any harness skills dir inside a VM: `~/.claude/skills`, `~/.codex/skills` and the others do not exist.
* Related, Boat page `integrated-agents__customize` ("boat adds a short system prompt ... and the `boat` skill"): the platform note exists (`internal/guest/agents/harness.go:70`), but it does not tell the agent that the `crowdvm` CLI exists in the sandbox. Also the skill is absent.
* `docs/gaps/*.md` do not audit DOCS-* rows. B.md lists the skill as "LEFT" only. The `crowdvm agent-instructions` command (DOCS-004) exists and works.

### M4. `lux` is absent (low)

* Boat `machines__tools`: `lux` "Desktop and browser automation with demo recording".
* `command -v lux` is empty. PARITY TOOL-007 says lux is replaced by `ascii-record-desktop` and cua-driver. That replaces the purpose well enough. Disputed only because there is no `lux` command and no migration alias or shim. Acceptable as ADAPTED if documented.

### M5. First Chrome launch shows a Terms-of-Service modal (low-medium UX)

* Boat pages `desktop-streaming__drive-and-record` and `integrated-agents__computer-use` use "Open example.com in Chrome and tell me the page title" as the canonical example.
* Observed: `xdg-open` or `google-chrome-stable` on the desktop opens Chrome 157's "Google Chrome and ChromeOS Additional Terms of Service" dialog. The image policy file `/etc/opt/chrome/policies/managed/crowdvm.json` only sets `DefaultBrowserSettingEnabled`, `MetricsReportingEnabled` and `PromotionalTabsEnabled`.
* The OpenCode agent did finish ("Example Domain") but spent about 20 tool calls dismissing the dialog and hitting `browser_consent_required`. The browser-only view passes `--no-first-run`, but the desktop launch path does not.
* Fix: a `google-chrome-stable` wrapper or `master_preferences` that sets `--no-first-run`.

### M6. Subscription sign-in is paste-only (disputed, see below)

* Boat `integrated-agents__keys`: click **Sign in**, approve on the vendor page, and the dashboard shows connected.
* CrowdVM: `crowdvm ai profile sign-in` and the dashboard take a pasted token (`claude setup-token`, `~/.codex/auth.json`, and so on). Renewal is real (`acct_ai_refresh.go`), and vendor device-code flows need only outbound internet, so a "Sign in" button is feasible.
* Acceptable as ADAPTED. PARITY AGT-030 wording ("dashboard runs the harness's own login in a utility sandbox") overstates what exists.

## Disputed classifications and stale claims

| Item | Claim | Reality |
|---|---|---|
| DOCS-001/002/003 | ADAPTED | Not implemented (M3). |
| AGT-025 | ADAPTED (system prompt + skill) | System note only; no skill; note omits the CLI. |
| AGT-030 | dashboard runs the harness login in a utility sandbox, or accepts pasted tokens | Paste only. |
| LIFE-011 / `snapshots__whats-saved` | REQUIRED, "natural with full-disk overlay boot" | Containers explicitly stopped do not restart (M1). |
| SIZE-005 vs README/guides | "no GPU" REQUIRED vs GPU placement advertised | Placement only (M2). |
| TOOL-007 `lux` | ADAPTED | No `lux`, no shim (M4). |
| Network benchmark | "internet speed" | Measures throughput and latency to the server only (`internal/node/bench/network.go`). Right for LAN-only, but it does not measure the node's outbound internet that guests use for packages and vendor APIs. Minor, and arguably within scope. |
| E2E.md B1, B2 | open product bugs | Stale. `setupStatus` reached `done` with a fast script, and `nodes --json` summary shows `maxSandboxes: 3`. They look fixed in the tree. |
| E2E.md B3–B6 | open minor bugs | Not re-tested. |
| Windows nodes | supported | Code, service, installer and WHPX path exist and cross-compile cleanly. Nothing has run on Windows. No test or CI exercises `GOOS=windows` at runtime. "Supported" is unverified, not disproved. |

## Notes for the team

* A deploy from a saved snapshot, or a second default sandbox, sits in `provisioning` with no reason shown when the node lacks capacity (14 GiB node, 2 × 8 GB). This is Boat-like waiting, but a `provisioning` reason or message would help. It is not a bug.
* Gap docs (`docs/gaps/*.md`) were largely accurate where I spot-checked: DESK-005/006/007/014/015/017, TOOL-006, process capture, history while stopped.

## Incident (mine)

I first put my node and server data dirs under `/tmp` (tmpfs). A 5.9 GB Wine saved snapshot filled it to 0 MB free for a few minutes, which may have produced transient write errors for other agents sharing `/tmp/claude-1000`. I killed my own processes (matched by my own data-dir path) and deleted only my own directories. Space recovered to about 29 GB free. I then moved all data under `~/.local/share/crowdvm-dev/review-sonnet-2`.

## Cleanup

All my sandboxes deleted, my server, node and QEMU processes stopped, ports 94xx free, my data and e2e dirs removed, no leftovers under `/tmp`. Only this file was written to the repo (plus the `make build` output in `bin/`).

## TL;DR

DISAGREE, narrowly. Core is excellent and verified live (tools, desktop, computer use, agents, steering, snapshots, Wine, hosting, benchmark over LAN/Tailscale; 12 e2e groups pass). Top issues: **M1** Docker containers without `--restart always` do not come back after stop/resume (verified, no test covers it); **M2** `--gpu` places on a GPU node but the VM sees no GPU; **M3** `/llms.txt`, docs MCP and the `crowdvm` skill are claimed but absent; **M5** Chrome's first-run ToS dialog blocks the canonical "open example.com" demo; **M4** no `lux`. Windows nodes cross-compile but are untested.
