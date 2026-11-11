# Review opus-3: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, round 2). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (narrowly)

The round-1 fixes are real. Of the 50 or so missing/broken findings in the four round-1 reviews, I count all but two as fixed. I re-ran most of the important ones on my own QEMU/KVM cluster:
- refused-stop and not-answering handling
- node-loss webhooks
- docs MCP and `llms.txt`
- in-sandbox skill
- Docker restart after resume
- `~/.cache` in forks
- `snapshot_data_expired`
- Boat-style Python SDK calls
- direct ports with the firewall
- Chrome first-run
- `lux`
- `push --to /srv`

Unit tests pass, and the full e2e run (another session's) ended `EXIT=0` with 25 groups passing.

The bar is still "every supportable feature", and I found supportable Boat behaviour that is still broken or missing:
- **Deletion operations for snapshots never complete.** I verified this live: they stay `processing/retrying` and turn `blocked` after 24 h.
- **About 40% of sandbox ids still fail Boat's id pattern.** The length is fixed, but the alphabet is not.
- **`run --personal` bills the active org.**
- **Bedrock IAM access keys are not supported.**
- **`org migrate` forces every migrated sandbox private and login-holding.**
- **The low-disk cleanup deletes Docker images built in the sandbox.**

Each is small, a few lines to a few dozen. None needs a redesign.

## Method

1. **Build and tests.** `make build` is clean. `go test` passes on every package except `test/e2e`, all 18 packages with tests OK. I did not run e2e myself. I read the other session's log at `…/232e4b2f…/scratchpad/e2e-run1.log`, which ends `--- PASS: TestE2E (611.61s)` and `EXIT=0`, with `bootloop` skipped. It includes the new groups `cli-fixes`, `docker-resume`, `agent-docs` and `boat-sdk-samples`.
2. **Own cluster.** All data was in `~/.local/share/crowdvm-dev/review-opus-3`, now deleted.
   - `crowdvm-server` on `127.0.0.1:9700`.
   - One `crowdvm-node`: qemu driver, KVM, UI on `:9701`, 6 vCPU / 14 GiB / 3 sandboxes.
   - Image `ubuntu-24.04-crowdvm-v3`, imported and pushed.
   - At most 3 VMs at once.
   - Driven with the `crowdvm` CLI, curl, and Boat-style Python code using the repo's `boat_sdk`.
   - Partway through I set `direct-host 127.0.0.1` on the node and restarted it, to test raw ports.
   - Teardown: all sandboxes deleted, server and node stopped (only my PIDs), no qemu left, data dir removed.
3. **Code check of every round-1 finding**, with file:line references below.
4. **Independent sweep.** Two read-only Opus sub-reviewers split the 219 Boat pages: pages a–l including `cli-reference`, and pages m–z plus request and response fields of `boat-v1.yaml`. They were told not to re-report round-1 items. I re-checked in code every sub-review finding I kept. The ones I could not test live are marked "code".

## Round-1 finding status

F = fixed, P = partly fixed, O = still open, A = accepted as a legitimate adaptation.

### opus-1

| # | Finding | Status | Evidence |
|---|---|---|---|
| 1 | Refused stop locks the sandbox | F | Pre-flight live save before prepare-stop. On failure, `refuseStop` puts it back to ready, sets the stop to `failing`, pauses billing and sends a notice (`internal/server/lifecycle.go:857-863`, `lifecycle_pause.go:101-112`). A failure after the pre-flight also keeps it usable when the machine answers (`lifecycle.go:899-903`). Not triggered live: I could not make a save fail. |
| 2 | Unresponsive: no billing pause or notice | F | `lifecycle.go:572-575` → `notAnswering` (`lifecycle_pause.go:140-147`). **Live:** SIGSTOP on `crowdvm-guest` gave `health: degraded`, `guest_unreachable`, `billingPaused: "not_answering"` and a notice. `crowdvm recover` brought it back with billing resumed, files and containers intact. |
| 3 | Node-loss webhooks | F | Offline emits `sandbox.degraded` + `sandbox.error` (`lifecycle.go:610-612`). Return emits `sandbox.ready` (reason `node_returned`) + `sandbox.recovered` (`lifecycle.go:553-559`). **Live:** a node restart produced the error notice and recovery. |
| 4 | `bx_` ids and `bsh_` share codes | **P** | The length is now 8 (`sandboxes.go:927`, `lifecycle.go:1397`). The alphabet is still `0123456789abcdefghjkmnpqrstvwxyz` (`internal/api/api.go:131`), but Boat's pattern is `[23456789abcdefghjkmnpqrstuvwxyz]` (`boat-v1.yaml:69,682`). So ids can contain `0`/`1` and never contain `u`: (30/32)^8 ≈ 60% match, 40% don't. Snapshot ids minted live contain `0` and `1` (`snap_ewfvbby1gnfx87510m5q`). Share codes are still `csh_` (`api.go:127`, `snapshots.go:976`), and only `csh_` is recognised. See new finding N2. |
| 5 | Python SDK positional calls and `boat_sdk` paths | F | `sdk/python/src/boat_sdk/{api/boat_api.py,exceptions.py,models}` exist. **Live:** with Boat's docs code (`from boat_sdk.api.boat_api import BoatApi`, `from boat_sdk.exceptions import NotFoundException`, `boat.host_port(sid, HostPortRequest(port=8080, public=True))`, `boat.write_file(sid, FileWriteRequest(...))`, `boat.command(sid, CommandRequest(...))`, `update`, `fork(fork_request=…)`, `read_text`, `write_text`, `exec_command`, `run_command`), every call worked. `NotFoundException` caught a 404. |
| 6 | llms.txt, docs MCP, skill | F | **Live:** `/llms.txt`, `/llms-full.txt` (160 KB), `/docs/index.md`, `/docs/quickstart.md` and `/docs/cli-reference.md` all return 200 Markdown. `POST /mcp tools/list` returns `search_docs`, `read_doc`, `list_docs`. In the VM, `~/.claude/skills`, `~/.codex/skills`, `~/.agents/skills`, `~/.config/opencode/skills`, `~/.pi/agent/skills`, `~/.kimi-code/skills` and `~/.prime/agent/skills` each contain `crowdvm` (`internal/guest/skills.go`, `skills/crowdvm/SKILL.md`). |
| 7 | Any key rotates/revokes itself | F | `ownKey` requires a session or a key-creating key, plus coverage (`internal/server/apikeys.go:490-503`). This matches Boat `api-keys__create-and-list.md:98`. |
| 8 | Live snapshots keep `~/.cache` | F (adapted) | The first boot of a disk from a live snapshot does the same cleanup (`internal/guest/boot.go:418-423`; PARITY SNAP-011). **Live:** I wrote `~/.cache/rev/f` in a running sandbox and forked it. The fork had no `~/.cache/rev/f` but kept `~/keep2.txt`. A raw `snapshot pull` of a live snapshot can still contain `~/.cache`, which is acceptable. |
| 9 | `snapshot pull <sandboxId>` | F | **Live:** `snapshot pull bx_… --path /home/user/keep.txt -o DIR` resolved the newest snapshot and wrote the file. |
| 10 | Deletion op `completed` too early | F for sandboxes, **broken for snapshots** | Sandbox ops now wait for the node to confirm (`deletion_ops.go:64-80`). Snapshot ops never complete: new finding N1. |
| 11 | Env name clash: org wins | F | User's own is looked up first (`feat_env.go:1156-1167`). |
| 12 | Webhook SSRF | F | Loopback, link-local and metadata targets are refused unless `--webhook-allow-local` is set (`outbound_guard.go:34-37,65,80,98`). |
| 13 | Raw ports / sshEndpoint only with `--direct-host` | A + F | Still opt-in per node. I accept that: it publishes ports on the contributor's LAN address, so the node owner must consent. **Live**, with direct-host on: `ip` and `sshEndpoint` filled in, `host 8000 --public` added `8000/tcp ALLOW` to the guest ufw, and `curl 127.0.0.1:8000` returned the page (`feat_direct.go:1-15`, `guest/firewall.go`). `crowdvm direct-ports ID 27015/udp` exists (`cmd_access.go:743`). |
| 14 | Error codes | F / A | `machine_not_running` (stopped) and `sandbox_not_ready` (error) now follow Boat `troubleshooting.md:161-175` (`passthrough.go:167-170`). Boat's own readiness page conflicts with that, so this is cosmetic. Steer has `delivery_failed` (`guest/agents/http.go:407-410`). The tree has `inventory_too_large` (`feat_snapfiles.go:412`). |
| 15 | `/limits` fields | F | `startWindowFull` is an object or null; `startLimitsWithWallet` is computed (`acct_billing.go:883-917`). |
| 16 | Org invites, private history, OTel limit | F | Invites are owner-only (`orgs.go:547`). History records carry a `private` flag and are hidden from teammates (`feat_history.go:185-204,343`). OTel limit uses the account limit (`feat_otel.go:457`). |
| s | Top-level `nested`, `--help` backticks, stale E2E.md | F | `sandboxes.go:920`. `new --help` shows `(see: crowdvm env list)`. E2E.md B1–B6 are moved to the fixed table (`docs/E2E.md:172-177`). |

### opus-2

| # | Finding | Status | Evidence |
|---|---|---|---|
| 1 | Resume after deleting all snapshots boots a blank disk | F | **Live:** stop, `snapshot delete --all`, then resume gave `409 snapshot_data_expired`, and fork gave the same. |
| 2 | `data-retention enable` wrong phrase | F | `internal/client/types.go:695` = `"delete archived sandbox data"`, the same as `feat_retention.go:30`. The e2e group `cli-fixes/data-retention-enable` passed. |
| 3 | `org default-visibility` show | F | **Live:** returns `{"defaultVisibility":"shared",…}`. |
| 4 | No skill | F | See opus-1 #6. |
| 5 | No agent docs | F | See opus-1 #6. |
| 6 | `host --public` blocked by firewall | F | See opus-1 #13 (live). |
| 7 | `push --to /srv/api` | F | **Live:** `pushed files:2 to /srv/api`. The directory was created and the symlink kept. |
| 8 | `env new` output, `run` cost, push count | F | **Live:** `env new` prints the real environment. `run` prints `exit 0 · 4s · small · 0.0001 credits · … deleted`. The push count is 2 for a file plus a symlink, which is correct. |
| — | Custom-API private flag undocumented | F | `crowdvm-server serve --ai-custom-api-allow-private` (`cmd/crowdvm-server/main.go:132`), documented in `docs/guide/getting-started.md:43`. |

### sonnet-1

| # | Finding | Status | Evidence |
|---|---|---|---|
| 1, 2 | Docs for agents, skill | F | See opus-1 #6. |
| 3 | `bx_` ids | **P** | See opus-1 #4 and N2. |
| 4 | `prompt_required`/`provider_required` | F | **Live:** `POST /prompt {}` → `400 prompt_required`. |
| 5 | Capacity: hang vs `no_ready_machine` | F / A | **Live:** `new --gpu --fail-fast` → `503 no_ready_machine`. Without fail-fast, a sandbox beyond capacity waits in `provisioning`, which is Boat's documented default. There is no "waiting for room" reason on the sandbox, which is cosmetic. |
| 6 | Gitea CI | F (code) | `ci/connect {provider:"gitea"}` path in `feat_ci.go` (PARITY CI-001). Not run: it needs a Gitea instance. |

### sonnet-2

| # | Finding | Status | Evidence |
|---|---|---|---|
| M1 | Docker containers not restarted | F | **Live:** `plain` (no policy) and `unless-stopped` were both `Up` after stop/resume. A container that had exited stayed exited (`internal/guest/docker.go`). |
| M2 | `--gpu` gives no GPU | F (code) | VFIO passthrough via `gpu.passthrough`, and `gpu.virgl` (`internal/node/vm/qemu/gpu.go:40-77`, `docs/guide/node-owners.md:78-95`). **Live:** a node without passthrough is not offered to `--gpu`, which returned `no_ready_machine`. VFIO itself is untested: this machine has one GPU. |
| M3 | Docs/skill | F | See opus-1 #6. |
| M4 | `lux` absent | F | `/usr/local/bin/lux` pointer exists in the v3 image (TOOL-007). |
| M5 | Chrome ToS dialog | F | `/opt/google/chrome/initial_preferences` has `require_eula:false, skip_first_run_ui:true` (`internal/guest/chrome.go`). **Live:** with a fresh profile, `google-chrome-stable https://example.com` opened straight to the "Example Domain - Google Chrome" window. |
| M6 | Subscription sign-in is paste-only | A | PARITY AGT-030 now states this honestly. Vendor OAuth clients are third-party sign-in, which is out of scope. |
| — | Network benchmark has no internet measure | F | Join now logs `internet: reachable, 5.2 ms`. |

**Summary:** everything is fixed or accepted except **ids (partial)** and **snapshot deletion ops (a new break found while checking opus-1 #10)**.

## New missing or broken supportable features

Ordered by impact. All were verified in code. N1 was also verified live.

### Blocking

**N1. Snapshot deletion operations never complete.** Boat page: `data-retention__delete`, DeletionOperation in `boat-v1.yaml`. PARITY: DATA-003.
- **Boat:** an operation reaches `completed` once the data is erased.
- **CrowdVM:** `processDeletionOps` sets `vmRemoved` only for `kind == "sandbox"` (`internal/server/deletion_ops.go:65`). The completion test is `o.vmRemoved && waiting == 0` (`deletion_ops.go:98`), and `newDeletionOp` inserts `vm_removed = 0` (`snapshots.go:342-345`). So every `kind: "snapshot"` operation loops in `processing/retrying` with "snapshot copies are still held by a node that is offline or did not confirm the deletion", and turns `blocked` after 24 h. The data is in fact purged.
- **Live:**
  - `snapshot delete --all bx_bbzk4q9q` created `bdop_d0949a…` and `bdop_2d6fbc…`.
  - Both snapshots had `purged_at` set within a minute.
  - 15 minutes later, with the node online, both ops were still `processing|retrying` after 6+ attempts.
  - Every `sandbox`-kind op in the same DB reached `completed`.
- **Why supportable:** pure server logic. Fix: treat non-sandbox ops as `vmRemoved = true`.

**N2. About 40% of sandbox ids fail Boat's id pattern.** Boat: `boat-v1.yaml:69,682` `^bx_[23456789abcdefghjkmnpqrstuvwxyz]{8}$`. PARITY: LIFE-001 and principle 1, wire compatibility.
- **CrowdVM:** `api.NewID` uses `0123456789abcdefghjkmnpqrstvwxyz` (`internal/api/api.go:131-142`): `0` and `1` allowed, `u` never.
- **Effect:** a client generated from Boat's spec validates the `sandboxId` path parameter. With pydantic-based generators that is the default, because the pattern sits on the shared parameter. Such a client rejects any id containing `0` or `1`, about 4 in 10.
- **Why supportable:** a one-constant change for `bx_`. Optionally also accept `bsh_` as an alias of `csh_` for `--from`.

**N3. `crowdvm run --personal` bills the active organization.** Boat page: `cli-reference.md:828` ("Bill your personal account when an organization wallet is active").
- **CrowdVM:** `run` only clears `c.Org` (`internal/cli/cmd_exec.go:380-382`). `new --personal` sends `req.Org = "personal"` (`cmd_sandbox.go:132-135`). With no org in the request, the server falls back to `User.ActiveOrg` (`internal/server/orgs.go:131-140`).
- **Effect:** after `crowdvm org switch acme`, `run --personal` bills Acme.
- **Why supportable:** a one-line fix.
- Verified in code, not live.

**N4. Bedrock with IAM access keys is missing.** Boat page: `integrated-agents__keys.md:194,198,203`: "IAM access keys also work: access key id, secret, and an optional session token". The sandbox gets `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`.
- **CrowdVM:** `claude-bedrock` and `codex-bedrock` store only `AWS_BEARER_TOKEN_BEDROCK` (`internal/server/feat_ai.go:144-145,1459`). `AWS_ACCESS_KEY_ID` appears nowhere in `internal/` or `web/js/`.
- **Why supportable:** sandboxes have outbound internet. It is three more env vars and a form field. PARITY AGT-035 lists only the bearer flow.

**N5. `org migrate` makes every moved sandbox private and login-holding.** Boat page: `organizations__shared-sandboxes.md:71`: the org default applies to sandboxes "moved in with `boat org migrate`". A sandbox is shared unless it holds the creator's logins or the creator made it private.
- **CrowdVM:** `UPDATE sandboxes SET … visibility='private', private_by_creator=1, holds_creator_logins=1` for every sandbox (`internal/server/acct_misc.go:242`). It ignores `no_env`, the real `holds_creator_logins` and the org's `newSandboxesPrivate`.
- **Effect:** a `--no-env` sandbox becomes unusable for teammates, and a later `share` reports `restartRequired` for logins it never had.
- **Why supportable:** server logic.

**N6. Low-disk cleanup deletes images built in the sandbox.** Boat page: `troubleshooting.md:123`: boat deletes "pulled Docker images that no container uses… boat never deletes your own files".
- **CrowdVM:** `freeDiskSpace` runs `docker image prune -af` (`internal/guest/sampler.go:181`). That removes images the user built (which cannot be downloaded again) and images of stopped Compose projects.
- **Contrast:** the stop path already keeps in-use and Compose images (`internal/guest/docker.go`, `pruneDocker`/`dockerImagesInUse`).
- **Why supportable:** reuse that logic and limit the prune to images with a registry origin.

### Non-blocking (low; fix when convenient)

- **N7. `scp -r` has no single-archive fast path.** Boat page: `cli-reference.md:1029`. CrowdVM always runs OpenSSH `scp -r` through the tunnel (`internal/cli/cmd_access.go`). `push` already has the tar path (`internal/cli/transfer.go`). This is performance only, but every file costs a round trip through the coordinator websocket.
- **N8. `current` without the shell function falls back to the profile's last sandbox.** Boat says `current` "does not work" in scripts. CrowdVM's fallback is at `internal/cli/app.go:505-513`, used from `app.go:455-462`. Parallel scripts on one profile can hit each other's sandbox, for example `delete current --yes`.
- **N9. `org members` hides pending invites.** Boat: `organizations.md:33` ("members and pending invites"). The CLI lists members only (`internal/cli/cmd_admin.go:488-504`). `GET /orgs/{org}/invites` exists. PARITY ORG-002 claims this works.
- **N10. The dashboard AI-credentials page has no "Who can use this profile" panel** (`web/js/pages/ai.js`). The CLI `ai profile access` and the API have it.
- **N11. Disposable email domains are not refused on `login --email`** (`internal/server/auth.go:530-596`). This is defensible for an invite-based self-hosted server, but AUTH-003 should say so.
- **N12. Notice noise.**
  - **Live:** `crowdvm run` deleting its sandbox raised a "Sandbox does not answer" notice for that sandbox (`ntc_kxfkve1rqrs2v5ex`, `bx_ck8z6q63`).
  - A 3-second node restart left an undismissed "Sandbox failed … rebuilds … from its latest snapshot" notice after the sandbox recovered by itself.
  - The behaviour is correct; the messages are wrong.

## Disputed classifications

- **LIFE-001 (REQUIRED):** "id keeps `bx_` shape" is only half true (N2).
- **DATA-003 (REQUIRED):** the note says an op "reports `completed` only when…". For snapshot ops it never does (N1).
- **CLI-075 (REQUIRED):** `--personal` is listed as done (N3).
- **AGT-035 (REQUIRED):** covers only the Bedrock API key and omits IAM keys (N4).
- **LIFE-044 (ADAPTED):** says "unused pulled images", but the code prunes every unused image (N6).
- **ORG-002 (ADAPTED):** "`org members` shows pending invites" is not true of the CLI (N9).
- **SSH-012 (ADAPTED):** drops Boat's tar fast path with no LAN reason (N7).
- **SNAP-007 (ADAPTED):** "resume prefers that node for a configurable window". No window setting exists in the server flags or the scheduler. Placement uses a snapshot-chain locality score (`scheduler.go:59`), and an explicit `Prefer` is set only for a resume queued during a stop (`lifecycle_place.go:254`). The behaviour is close enough, so only the wording is wrong.
- **AUTH-003 (ADAPTED):** should state that the disposable-domain refusal is dropped and why (N11).

**N/A rows:** the sub-reviewers and I agree with all 26: Stripe, wallet pay and cards, trials, Google/AgentID OAuth, SOC 2 and status page, support channels, Box migration, release channels. None hides a feature that works on a LAN.

**ADAPTED rows I accept:**
- credits instead of billing
- local accounts and device code
- host-routed URLs instead of public HTTPS
- noVNC instead of Moonlight
- CI polling instead of a GitHub App
- opt-in direct ports
- paste-in subscription tokens
- a `lux` pointer instead of Ascii's proprietary agent
- `service.name=crowdvm`

## What would flip this to AGREE

1. N1: let snapshot-kind deletion operations complete once their snapshots are purged (and add a test).
2. N2: mint `bx_` ids from Boat's alphabet `23456789abcdefghjkmnpqrstuvwxyz`.
3. N3: send `org: "personal"` from `run --personal`.
4. N4: accept Bedrock IAM keys (id, secret, optional session token) for Claude Code, and pass them to the sandbox.
5. N5: on `org migrate`, keep each sandbox's real login state and apply the org default visibility.
6. N6: in the low-disk cleanup, keep built and Compose images; prune only unused pulled images.

N7–N12 and the PARITY wording fixes would be nice to have. They don't block AGREE.

## Not verified

- Windows nodes.
- Real VFIO passthrough (one GPU here).
- Gitea and GitHub CI runners.
- Real Claude/Codex subscriptions.
- A refused stop with a real save failure (code read only).
- SMTP notices.
- The TypeScript SDK (the e2e `sdk` group passed).

## TL;DR

DISAGREE, narrowly. Round 1's findings are fixed or justified apart from the id alphabet, and I confirmed most fixes on a live KVM node. The other session's full e2e run passed. Six small supportable gaps remain:
- snapshot deletion ops never complete (live)
- 40% of ids fail Boat's pattern
- `run --personal` bills the org
- no Bedrock IAM keys
- `org migrate` forces everything private
- the disk cleanup deletes built Docker images

Each is a small fix.
