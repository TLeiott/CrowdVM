# Review opus-14: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (one blocker, in CI)

All four claimed fixes hold. I checked each one live on my own cluster. I found no regression in the hostname change, in fork or in create.

My sweep of desktop streaming, recording, computer use, Wine and OpenTelemetry found nothing that blocks. Every recipe I ran worked as Boat describes.

One blocker is in CI:

**B1. A CI job's sandbox is matched to the job it was booted for, not to the job GitHub actually gave its runner.** When jobs run in parallel, CrowdVM can delete a sandbox while it is still running another job, and `/debug` can keep the wrong sandbox. I found this from the code; GitHub is not reachable from here. A sub-agent raised it and I confirmed it line by line (details below).

## Method

1. **Read.**
   - `opus-13.md` and `sonnet-11.md`: their claims and flip criteria.
   - Citation counts per area in `docs/reviews/`, to see what earlier rounds covered least.
   - The fix code (`internal/guest/boot.go`, `docker.go`, `internal/cli/cmd_snapshot.go`, `internal/server/snapshots.go`) and the e2e `docker-resume` test (`test/e2e/review_test.go:87-120`).
   - Boat pages:
     - `desktop-streaming*.md` (5 pages), `integrated-agents__computer-use.md`;
     - `windows-apps*.md` (6 pages);
     - `opentelemetry.md` and the `/telemetry` spec;
     - `integrations__harbor.md`, `ci.md`.
2. **Build and tests.**
   - I built all four binaries into my scratchpad with `CGO_ENABLED=0`. I did not use `make build`, because the team's e2e run was using the repo at the same time and I did not want to rewrite `bin/` under it.
   - `go vet ./...` is clean.
   - `go test -count=1` on every package except `test/e2e` is all `ok`.
   - I did not run e2e.
3. **Own cluster.**
   - Server on 9960, node UI on 9961, OTLP receiver on 9962.
   - Data in `~/.local/share/review-opus-14`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated.
   - One qemu/KVM node: 6 vCPU, 12 GiB, 3 sandboxes, `--snapshot-interval 60s`, image `ubuntu-24.04-crowdvm-v3`, `net.allow 192.168.178.31/32`.
   - User `admin`, org `acme14`.
4. **Fix checks:** live, see the table below.
5. **Sweep.**
   - **Me, live:**
     - desktop and browser URLs, viewer token handling, `overlay=0`, the `desktop` key action;
     - Chrome `--profile`, the wallpaper through Boat's path, `ascii-record-desktop`;
     - the cua-driver daemon, its MCP tool list, and its registration in all harness configs;
     - Boat's `install-wine.sh` run verbatim as a detached API command, then Notepad;
     - an OTLP export to a local receiver;
     - the SDK samples of the 4 desktop pages and `opentelemetry` (a copy of `sdk/tools/doc_samples.py` in my scratchpad).
   - **One sub-agent (code plus live on my cluster):** `ci.md` and the CI API pages, `evals.md`, and the three `integrations__*` pages. I re-checked every finding it called blocking myself (see "Disputed").
6. **Teardown.**
   - I deleted every sandbox and stopped my node, server and OTLP receiver by their saved PIDs.
   - Afterwards, no process used my data dir and nothing listened on 9960–9969. I deleted the data dir.
   - I did not touch the other reviewer's cluster (9970/9971) or the team's e2e processes.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| opus-13 B1 | The Saved snapshots page lists only the viewed wallet | **Fixed (live)** | `handleListSaved` lists only `c.Scope("")`'s wallet when `?org=`, `?teamId=` or the header is set (`internal/server/snapshots.go:778-787`). By-name lookups use the same scope (`:647-664`). Live, with an admin key: `GET /saved-snapshots` with `X-CrowdVM-Org: personal` → `["rv-pers"]`; with the org id → `["rv-org"]`; with no header → both (the CLI behaviour is kept). `GET /saved-snapshots/rv-org/shares` → `200` with the org header, `404` with `personal`. Every row the page shows is now actionable in the scope it was listed in. Server test: `lifecycle_fix_test.go:389-396`. |
| sonnet-11 B1 | Boat's "Keep a hostname" recipe survives resume | **Fixed (live)** | `setHostname` returns early when an enabled unit in a `*.wants` folder runs `hostnamectl set-hostname` (`internal/guest/boot.go:227-230`, `:696-709`). Unit test: `setup_merge_test.go:31-45`. Live: Boat's unit verbatim, `systemctl enable --now` → `my-sandbox`; `stop`, then `resume --type default` (a new machine) → `hostname` and `/etc/hostname` are `my-sandbox`, and `sudo` works without a resolve warning. A fork of that sandbox → `my-sandbox`. |
| — | Regression check: no user unit | **No regression (live)** | New sandbox `plainbox` → hostname `plainbox`. Its fork → `plainbox-fork`, with `/etc/hosts` `127.0.1.1 plainbox-fork`. `/etc/crowdvm/sandbox-id` is still written before the early return (`boot.go:223-226`). Only an `ExecStart*=` line that calls `hostnamectl` directly is detected; a unit that sets the hostname from a script still races. That is outside Boat's recipe, so it is not a blocker. |
| sonnet-11 B2 | Images of profile-gated Compose services are kept | **Fixed (live)** | `docker compose … --profile "*" config --images` (`internal/guest/docker.go:330`). The guest has Compose v5.7.0, where `--profile "*"` lists `busybox:1.36` and `alpine:3`. Live: `web` up, `tools` (`profiles: ["tools"]`) only ever `run`. After stop and resume `busybox:1.36` and `alpine:3` are still there, `redis:alpine` (no container, no project) was pruned, and `dk-web-1` is running again. |
| sonnet-11 B3 | `snapshot pull` writes `docker/<volume>/` | **Fixed (live)** | The CLI lists the snapshot tree and pulls each `__dockervol__/<vol>` (`internal/cli/cmd_snapshot.go:333-343`), flattens `_data` (`:363-366`) and maps it to `docker/` (`localName`, `:389-397`). Only a missing `home/user` is fatal; a 404 on a volume path is skipped (`:349-351`). Live: `snapshot pull <id> -o DIR` → `DIR/home_user` and `DIR/docker/vol1/f` = `dv`. |

**E2E coverage claim.** `docker-resume` (`review_test.go:96-119`) checks all three guest/CLI fixes. But `e2e-run13.log` (EXIT=0) finished at 17:01, before the fixes: `boot.go` 17:36, `docker.go` 17:37, `cmd_snapshot.go` 17:42, `review_test.go` 17:44. So run13 does not cover them. `e2e-run14.log` was still running when I finished, and had printed no `docker-resume` result yet. My live checks above stand in for it.

## New missing or broken supportable features

### B1. CI job ↔ sandbox mix-up under parallel jobs (`ci.md:146`, `:66`, `:328`)

- **What Boat says.**
  - `ci.md:146`: "Each job is a normal sandbox named `ci-<repo>-<job id>`… Run `boat ssh <id>` on a running job, or on a failed `/debug` job."
  - `/debug` keeps the failed job's sandbox.
  - `ci.md:328` gives a matrix of 30 jobs as the normal case.
- **What CrowdVM does.**
  - For queued job X it creates a sandbox named `ci-<repo>-<X>` (`internal/server/feat_ci.go:637`).
  - It registers a repo-level JIT runner `crowdvm-<sbID>` that carries only the job's label (`:714-715`).
  - GitHub gives a runner any queued job whose labels match. A JIT runner is not tied to the job it was created for.
  - `track()` follows job X on GitHub and acts on `j.SandboxID`, the sandbox booted *for* X (`:796-850`):
    - when X completes it deletes that sandbox (`:849`);
    - when X disappears (404) it also deletes it (`:805`);
    - it attributes OOM to it (`:839`) and, for `/debug`, keeps it (`:844`).
  - `ghJob` (`:238-252`) does not read `runner_name`. A grep for `runner_name|RunnerName|runner_id` in `internal/` finds nothing.
- **What goes wrong.** Two jobs X and Y with label `crowdvm` are queued in one matrix. Their runners are rX (sandbox sX) and rY (sY). GitHub hands Y to rX and X to rY. X finishes first, so `track` deletes sX, which is still running Y. Y fails on GitHub with a lost runner. Other effects:
  - with `/debug`, a failed X keeps sX, which ran Y, while sY, which holds the real failure, is deleted when Y completes;
  - the `sandboxId` and cost of each job point at the wrong sandbox;
  - `boat ssh` into `ci-<repo>-<X>` lands in Y's job.
- **Tests.** The fake-GitHub tests (`internal/server/feat_ci_test.go`) never run two jobs with the same label at once, so they cannot catch this.
- **Not verified live.** No GitHub here. The scenario follows directly from the cited lines and from GitHub's label-based assignment.
- **Why it is supportable on a LAN:** the fix is server-side and uses the same GitHub API CrowdVM already polls. Read `runner_name` from `GET /repos/{repo}/actions/jobs/{id}`; the runner is already named `crowdvm-<sbID>`. Map the job to that sandbox before deleting, keeping or attributing anything. A runner that has not picked up a job yet should be deleted only when its own runner is idle or gone.
- **Gitea.** The Gitea path registers `act_runner` by label the same way (`:694-709`), so it most likely has the same problem. I did not check how Gitea assigns jobs.

## Minor (not blocking)

- **m1. CrowdVM's own Harbor adapter fails on Boat's `/workspace` example.** Verified live.
  - `integrations/harbor/harbor_box_environment.py` takes Boat's module name and says Boat's `-e harbor_box_environment:BoxEnvironment` "works unchanged".
  - Its `_put` writes through the files API (`harbor_crowdvm_environment.py:226-235`), which accepts only `/home/user` and `/tmp` (`internal/guest/guest.go:288`).
  - Live, the adapter's own `hello_sandbox.py` with its default `WORKDIR=/workspace` (`:19`) fails with `400 invalid_path: path must resolve under /home/user or /tmp`. Boat's sample (`integrations__harbor.md:82,88`) would fail the same way with this module.
  - **Not a blocker:** Boat's documented route, upstream `harbor-box` with `base_url=…/api/box/v1`, ran live with `/workspace` in opus-10 (`opus-10.md:118`). That is what `docs/guide/migrating-from-boat.md:58-61` recommends.
  - **Fix:** map `/workspace` as the Eve adapter does (`integrations/eve/src/index.ts:112-115`), or upload through a tar file in `/tmp` plus `sudo tar -C`. Also stop shadowing the upstream module name, or document that this module does shadow it.
- **m2. The CI poll can exhaust a token's GitHub quota on accounts with many repos.** From the code.
  - Every 15 s (`feat_ci.go:47`) it makes 1 call plus 2 per repo (`:419-448`), plus calls per run and per active job. That is about 240 × (1 + 2R) requests per hour. At R = 10 that is 5,040, above the 5,000/h limit of a token.
  - There are no conditional requests, and the interval is not exposed on the CLI (only `Config.CIPollInterval`, `server.go:107`).
  - Below that size it works. Boat's own step 1 is "Pick the repositories" (`ci.md:20`), and a fine-grained token limited to selected repos does the same here. So this is a scaling limit with a workaround, not a missing feature.
  - Use ETag requests (a 304 does not count against the limit), back off on `X-RateLimit-Remaining`, and document the limit in `ci connect --help`.
- **m3. Desktop URLs are single-use** (`feat_desktop.go:68`, `claim`; used at `:458`): reopening the same URL within 10 minutes gives `401 desktop_token_used`. Boat's table only says a reload *after* 10 minutes needs a new URL. But Boat also says "Ask for one on each page load", so this is stricter, not broken.
- **m4. CI items from the sub-agent that I did not re-check in depth.**
  - Refused jobs stay queued on GitHub, and there is no `gh run rerun --failed` hint.
  - `ci connect` has no `--provider gitea` flag; Gitea can only be connected through the API.
  - Org linking does not check that the token's user manages the GitHub org (Boat verifies this, `ci.md:27`).
  - None of these blocks the main CI flow.

## Disputed or accepted classifications

- **Sub-agent's B2 (CI rate limit)** → I downgraded it to m2. The reasons: Boat's own setup step limits repositories; a fine-grained token works around it; and it is a scaling limit, not an absent behaviour.
- **Sub-agent's B3 (Harbor `/workspace`)** → I downgraded it to m1. Boat's route, upstream `harbor-box` through the Box API, works. The failing code is a CrowdVM extra.
- **DESK-002/003 (ADAPTED): noVNC instead of Moonlight/WebRTC.**
  - Accepted. The API returns `desktopUrl` with `#token=`, `expiresInSeconds:600`, `width/height` 1920×1080, `viewerUrl` and `wsUrl`.
  - The viewer strips the token from the address bar (`web/js/pages/desktop.js:9-14`) and sends it as a WebSocket subprotocol, never in the URL (`web/js/vnc.js:8-21`, `feat_desktop.go:400-404`).
  - `overlay=0` hides the bar (`desktop.js:21`).
  - Frame rate is best-effort on a LAN.
- **OTEL-004/005/006 (ADAPTED).**
  - Accepted: `service.name` is `crowdvm`, every other `boat.*` metric and attribute name is kept, and plain HTTP and LAN collectors are allowed.
  - The payload is OTLP/HTTP **JSON** (`feat_otel.go:273`). That is a valid OTLP encoding for any self-hosted Collector. Some SaaS intakes prefer protobuf, but those need the internet anyway.

## Checked and fine

- **Desktop** (`desktop-streaming`, `urls-and-security`):
  - `POST /desktop?theme=light` and `crowdvm desktop` return a fragment-token URL.
  - A key without `desktop` → `403 api_key_action_forbidden` for both `/desktop` and `/browser`.
  - `GET /sandboxes/{id}` has `desktopAvailable:true` and `desktopUrl:null`, so no URL is cached.
  - SDK samples: `desktop-streaming#2:ts` passes.
- **Browser-only view:**
  - `browserUrl` uses a separate Xvnc `:2` with `-AcceptCutText=0 -SendCutText=0`, so no clipboard (`internal/guest/browser.go:174`).
  - `--profile /data/chrome-profile` creates the folder, owned by the browser user, and Chrome runs with `--user-data-dir=/data/chrome-profile`.
  - `/home/user/prof` and `/etc/prof` → `400 invalid_profile`, and a relative path is refused by the CLI.
- **Background:** a red PNG at Boat's `~/.config/boat/wallpaper.png` was on screen within 4 s; the pixel at (100,900) read `255 0 0`. The file survived a stop and resume. Sample `desktop-streaming__background#2:ts` passes.
- **Recording:**
  - `ascii-record-desktop start ~/demo.mp4`, `status` ("recording …") and `stop` ("saved …") work.
  - The file is H.264 1920×1080, 4.97 s.
  - `GET /artifacts?path=demo.mp4` → 200 with the same byte count.
  - Samples `drive-and-record#2,#6,#10:ts` pass.
- **Computer use:**
  - `cua-driver.service` is active, and active again after a stop and resume (new socket in `/run/ascii-cua/`).
  - `/opt/ascii/cua-driver/cua-driver status --socket /run/ascii-cua/driver.sock` → running.
  - The `computer` server is registered in `~/.claude.json`, `~/.codex/config.toml` (marked block), `opencode.json` (with `computer_browser_prepare:false`), pi, Kimi and Prime.
  - `tools/list` over stdio returns every tool in Boat's table (plus more), and `get_screen_size` → 1920x1080.
- **Windows apps:**
  - Boat's `install-wine.sh`, run verbatim as `POST /commands {detached:true}` and polled → `exited`, exit 0, `wine-11.21 (Staging)`.
  - `wine start notepad` → `Untitled - Notepad` in `wmctrl -l`.
  - The `UseXRandR` registry recipe works.
  - Saving the Wine snapshot and `new --from` it were covered live by sonnet-2, sonnet-7 and opus-11.
- **OpenTelemetry:**
  - `telemetry set http://…/v1/metrics -H x-test-key=…` strips `/v1/metrics`, returns `test{ok:true,status:200}` and lists header names only.
  - The receiver got `/v1/metrics` and `/v1/logs` with the header.
  - Sandbox resources carry `service.instance.id`, `boat.sandbox.{id,name,size,memory.size,disk.size}` and `boat.account.id`.
  - The 6 sandbox gauges have the right units. Each data point carries `boat.sandbox.id` and `boat.sandbox.name`.
  - The account resource has `sandboxes.active` and `sandboxes.limit`.
  - Lifecycle log records `sandbox.archived`, `sandbox.ready` and `sandbox.hydrated` have `eventName`, `event.name`, severity `INFO`, `boat.event.id` and `state`/`previous_state`.
  - `telemetry unset` → `{"telemetry":null}`.
  - SDK samples: 14 of 16 pass. The 2 failures are the sample's own `os.environ["DD_API_KEY"]` placeholder, and a `delete_telemetry()` with nothing set, which returns `404` exactly as `boat-v1.yaml` specifies.
- **Fork capacity:** a fork on a full node stays `cloning` until a slot frees, then starts. That matches Boat's "A resume or a fork waits for a machine when none is free" (`snapshots__resume-and-fork.md:136`).
- **Sub-agent (CI, evals, integrations):**
  - all 5 CI routes and their scopes;
  - labels, `/from=`, `/debug`, `/kvm`, and refusals with a reason;
  - start-limit behaviour, the 6 h cap and the 30-minute `/debug` cleanup;
  - public-repo gating with `dryRun`;
  - the CLI and SDK CI methods;
  - the `boat_sdk` compat imports from `evals.md`;
  - the adapter unit tests (Harbor 7, evals 9, AgentEnv 2, Eve 6).

## Not verified

- CI against real GitHub or Gitea (no access). B1 and m2 are from the code.
- Upstream `harbor-box`, AgentEnv and Eve packages (not installed here; opus-10 ran them).
- A real AI-backed `prompt` driving the desktop (no AI credential used). The computer tools were exercised directly over MCP.
- `e2e-run14.log`: unfinished when I stopped.
- Windows and macOS nodes.

## What would flip this to AGREE

1. **B1:** CI maps each GitHub job to the sandbox whose runner actually ran it (`runner_name` = `crowdvm-<sbID>`) before it deletes, keeps (`/debug`) or attributes OOM or cost. A runner whose job went elsewhere is cleaned up only once it is idle. Add a fake-GitHub test with two same-label jobs where GitHub swaps the runners. The X-finishes-first case must not delete the sandbox still running Y.
2. A finished `e2e-run14` (or later) with `docker-resume` PASS, to back the fix table with the team's own run.

m1 and m2 are cheap, and would remove a broken CrowdVM extra and an undocumented scaling limit.
