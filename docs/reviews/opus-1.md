# Review opus-1: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-08. Tree: the working tree at review time (no commits yet on `master`).

## Verdict: DISAGREE

CrowdVM comes close. All 122 operations in `.ref/boat-v1.yaml` are routed and none is a stub. Every `boat` CLI command in `cli-reference.md` has a `crowdvm` counterpart, except `billing cancel/portal` and the Stripe wallet subcommands. Most of the "FIXED" claims I checked hold up in code, and the ones I ran on a real QEMU/KVM node worked.

Still, several Boat features that work fine on a LAN are missing or behave differently from Boat's docs:
- refused-stop and unresponsive-sandbox behaviour
- webhooks around node loss
- Boat-style SDK calls and the `bx_` ids
- the agent docs, MCP server and in-sandbox skill
- the API-key rotation rule

The checklist itself calls some of these REQUIRED. So the claim "every supportable Boat feature is supported" does not hold yet. Most of the gaps are small or medium. None needs a redesign.

## Method

1. **Build and tests.** `make build`, `go vet ./...` and `go test ./...` all pass (17 test packages OK). The Python SDK unit tests pass (43/43).
2. **Live cluster on this machine.** I ran my own cluster in isolated dirs and removed it afterwards:
   - `crowdvm-server` on `127.0.0.1:9160`
   - one `crowdvm-node` with the **qemu** driver (KVM) and its UI on `:9161`
   - image `ubuntu-24.04-crowdvm-v2`, imported, then pushed to the server

   I drove it with the `crowdvm` CLI and with the Python SDK.
   - I did **not** run `make e2e`: another reviewer's E2E run was active, and the harness kills leftover runs.
   - I did not use port 8420.
3. **Code spot checks.** I checked about 25 rows marked DONE/FIXED in `docs/gaps/A.md` and `docs/gaps/B.md` (mostly the P1 items) by reading the handler, node and guest code.
4. **Three parallel read-only sub-reviews** (Opus):
   - Boat pages a–l, including `cli-reference.md`
   - Boat pages m–z
   - a scripted comparison of every OpenAPI operation, its fields and its error codes against the server routes

   I re-checked their main claims in code or live before keeping them. Claims I did not re-check are marked *(sub-review, not re-verified)*.

## Supportable Boat features that are missing or broken

Ordered by impact. "Verified" means I confirmed it in code (file:line) or on the live cluster.

| # | Boat page | What's missing or wrong | Evidence |
|---|---|---|---|
| 1 | snapshots__stop ("When a stop is refused"), troubleshooting | **A refused stop makes the sandbox unusable.** Boat keeps the sandbox running and usable, and pauses the meter while it retries. CrowdVM sets the state to `archiving` before the save, then marks the stop `failing` and retries. Every exec, files or agent call then gets `409 machine_not_running`. The guest's prepare-stop has also already deleted `~/.cache`, ignored paths and unused Docker images from the live disk. `docs/gaps/A.md` LIFE-034 says this too ("the sandbox still shows archiving while the stop fails") but still marks the row FIXED. | Verified: `internal/server/lifecycle.go:727` (transition to archiving), `:755-758` (`fail` leaves it there), `internal/server/passthrough.go:167-168` |
| 2 | troubleshooting ("not answering") | **An unresponsive sandbox gets no billing pause and no notice.** Boat sends an email and pauses billing while the sandbox doesn't answer. On `guest_unreachable`, CrowdVM only sets `health: degraded`. It does not call `closeRun`, and no `notifySandbox` caller covers this case. The gap audit lists both parts as still open, yet LIFE-040 is not flagged as open. | Verified: `internal/server/lifecycle.go:519-521`. The `notifySandbox` callers are only in `lifecycle_holds.go` (error, forced stop), `lifecycle_place.go:297` and `lifecycle_notify.go:61` |
| 3 | webhooks (Events table) | **Webhooks around node loss break Boat's contract.** When a node goes offline, its sandboxes move to `error`, but only `sandbox.degraded` is emitted, never `sandbox.error`. When the node returns, the sandbox goes from `error` back to ready with only `sandbox.recovered`, never `sandbox.ready`. Boat says `sandbox.error` fires whenever a sandbox enters `error`, and `sandbox.ready` "includes recovery from an error". Node loss is the most common failure for a crowd of contributor machines. | Verified: `internal/server/lifecycle.go:549-553` (offline), `:505-508` (recovery), `feat_webhooks.go:41,139-143` |
| 4 | concepts, API spec (`^bx_[…]{8}$`); PARITY principle 1 | **Sandbox ids are `bx_` + 10 characters, not Boat's `bx_` + 8.** The alphabet also differs: CrowdVM allows `0` and `1` but not `u`. Share codes are `csh_`, not `bsh_`. The ids fail the spec's path-parameter pattern, which breaks clients that validate it, likely including the generated Boat SDKs. PARITY's own principle 1 and LIFE-001 require `bx_`, and `gaps/A.md` LIFE-001 is still PARTIAL. | Verified: `internal/api/api.go:123,131`; `sandboxes.go:921`; live ids such as `bx_fcjkz56rgn` |
| 5 | sdks__python (recipes and method table) | **Boat's documented Python calls fail with the CrowdVM SDK.** The method table and the recipes pass request models positionally. Run live through `crowdvm.compat.BoatApi`: <br>• `boat.host_port(sid, HostPortRequest(port=8080, public=True))` → `400 invalid_json … hostReq.port of type int`<br>• `boat.write_file(sid, FileWriteRequest(...))` → `TypeError: missing … 'content'`<br>• Boat's module paths `boat_sdk.api.boat_api` and `boat_sdk.exceptions` have no counterpart (`crowdvm.api` does not exist), so imports change beyond the package name.<br>Keyword-style calls do work: the `evals.md` episode script ran live with only the import lines changed. `gaps/B.md` marks SDK-006 and SDK-008 DONE. | Verified live; `sdk/python/src/crowdvm/_ops.py:540` (`host_port(self, sandbox_id, port: int, …)`) |
| 6 | ai-tools, integrated-agents__customize | **Nothing for AI agents beyond the AGENTS.md snippet.** <br>• `/llms.txt`, `/llms-full.txt` and `/docs/<page>.md` return 404.<br>• `/mcp` returns the dashboard HTML, so there is no docs MCP server.<br>• No `crowdvm` skill exists in any harness skills dir inside the VM, and there is no `SKILL.md` anywhere in the repo.<br>PARITY rows DOCS-001/002/003 and the skill half of AGT-025 promise all of these as ADAPTED. Boat says every integrated agent "already has a `boat` skill". `gaps/B.md` doesn't audit the DOCS rows. Only `crowdvm agent-instructions` (DOCS-004) exists. | Verified: curl on the live server; `ls ~/.claude/skills ~/.codex/skills ~/.agents/skills ~/.config/opencode/skills` in the VM; `find . -iname SKILL.md` is empty |
| 7 | api-keys__store-rotate-revoke | **Any API key can rotate or revoke itself.** Boat: "Rotate needs a browser session", and revoke needs a session or a key that can create keys. CrowdVM opens both routes to every key and lets a key manage itself, read-only keys included. A leaked low-privilege key can therefore rotate itself and cut off the real owner's copy. | Verified: `internal/server/apikeys.go:486-495` (`c.P.Key.ID != k.ID` exemption), `internal/server/acct_guard.go:41-42` (`""` = any key) |
| 8 | snapshots__whats-saved | **Live snapshots keep `~/.cache`.** Boat's "Not saved" list includes `~/.cache` and the Docker build cache. CrowdVM removes them only in prepare-stop, so forks and saved snapshots of a *running* sandbox still contain them. Live test: I wrote `~/.cache/rev/f`, waited for a live snapshot and forked; the fork still had the file. `/tmp` was correctly gone. | Verified live; `internal/guest/internal.go:142-155` (cleanup runs only at stop) |
| 9 | snapshots__download-and-delete | **`snapshot pull <sandboxId>` (newest snapshot of a sandbox) returns 404.** Only snapshot ids work. | Verified live: `{"code":"not_found","error":"Snapshot not found."…}`; `internal/cli/cmd_snapshot.go:270-293` |
| 10 | data-retention__delete | **Deletion operations report `completed` before anything is confirmed erased.** The worker ignores the node's DELETE result, skips offline nodes, and always calls `completeDeletionOp`. The spec's `waiting_for_uploads`, `retrying` and `blocked` stages never appear. Running orphans are removed when the node reconnects (`reconcileVM`), but the status claims erasure too early. | Verified: `internal/server/lifecycle.go:1463-1472`, `:470-479` |
| 11 | environments__manage | **On a name clash, the org environment wins; Boat says yours wins.** CrowdVM looks in the org first, on purpose, yet `gaps/A.md` marks the row DONE. | Verified: `internal/server/feat_env.go:1149-1163`; Boat `environments__manage.md:122` |
| 12 | webhooks (HOOK-004 as adapted) | **Webhook URLs aren't restricted at all.** PARITY says "admin can restrict", but nothing does. Loopback and link-local targets (127.0.0.1, 169.254.169.254) are accepted, which lets any user make the coordinator send requests to its own host (SSRF). LAN targets are fine in scope; loopback is not needed. | Verified: `internal/server/feat_webhooks.go:594-606` |
| 13 | hosting ("Raw TCP or UDP ports"), ssh-access__editors | **Raw ports and plain-OpenSSH access exist only on nodes started with the opt-in `--direct-host`.** By default `ip`, `machineIp` and `sshEndpoint` are `null`, as on my cluster. Boat's own `ssh-config` and VS Code recipe then doesn't apply; `crowdvm ssh-config` with the CLI as ProxyCommand does work. Boat also says `host --public` opens the TCP port on the IP. CrowdVM publishes it but doesn't open the guest firewall, which denies inbound by default. Whether a default-off adaptation is acceptable is debatable. | Verified: `sandbox info` shows `ip/machineIp/sshEndpoint: null`; `internal/server/feat_direct.go:1-10` (guest firewall applies) |
| 14 | API spec, troubleshooting | **Some error codes and fields differ from the spec.** <br>• A command on a stopped sandbox gets `409 machine_not_running`; the spec and readiness.md say `sandbox_not_ready` (troubleshooting.md is itself inconsistent).<br>• The steer reasons `not_running` and `delivery_failed` are never produced.<br>• The snapshot-file and tree reasons `legacy_snapshot`, `snapshot_not_indexed`, `base_image_file` and `inventory_too_large` are never produced.<br>• `gateway_error` on a subdomain rename is never produced.<br>• Snapshot tree never sets `reason`. | Verified: `passthrough.go:167`. The other items are from the API sub-review; I spot-read the steer handler at `guest/agents/http.go:377-395` |
| 15 | limits | *(sub-review, not re-verified)* `GET /limits` returns `startWindowFull` as a boolean, `startLimitsWithWallet` is always `null`, and the `rate_limited` cause is hard-coded to `"plan"`. | `internal/server/ratelimit.go:157` |
| 16 | organizations | *(sub-review, not re-verified)* <br>• Org admins can invite; Boat allows the owner only.<br>• Teammates can read command history recorded while the sandbox was private.<br>• OTel `account.sandboxes.limit` reports the global `MaxActive`, not the wallet's limit. | `orgs.go:542`, `feat_history.go`, `feat_otel.go:456` |

Smaller items:
- The top-level `nested: true` create field (a CrowdVM extension) is stored but not used for placement; only `placement.nested` is (`sandboxes.go:923,947`, `scheduler.go:110`). The CLI uses `placement`, so it works there.
- `crowdvm new --help` renders `--environment crowdvm env list` (cobra read the backticks in the flag text as a placeholder; `cmd_sandbox.go:65`).
- `docs/E2E.md` still lists B1–B6 as failing. All six are fixed in code:
  - B1: `setupStatus` came back `done` live
  - B2: `summary.capacity.maxSandboxes` came back 2 live
  - B4: `cmd_auth.go:394-398`
  - B5: `credits.go:239-247`
  - B6: `cmd_sandbox.go:722`
  - B3: `hostUrl`/`pathUrl` now present

  So the results table is stale.

## Classifications I dispute

- **DOCS-001, DOCS-002, DOCS-003 (ADAPTED)**: the rows promise LAN versions that don't exist, and no gap audit covers them (finding 6). They should be tracked as MISSING.
- **AGT-025 (ADAPTED)**: the platform-note half is done; the "`crowdvm` skill in every harness skills dir" half is not.
- **LIFE-034 / SNAP-008 (ADAPTED → "FIXED")**: the adapted form drops Boat's main guarantee, that the sandbox keeps running and stays usable during a refused stop (finding 1).
- **LIFE-040 (ADAPTED)**: the billing pause and the notice are part of the adapted form and are not implemented (finding 2).
- **HOOK-004 (ADAPTED)**: "admin can restrict" describes code that doesn't exist (finding 12).
- **SSH-004/007/008 (ADAPTED)** *(from sub-review, consistent with what I saw)*: they describe a coordinator TCP mapping for SSH that doesn't exist. Without `--direct-host`, `sshEndpoint` is null.
- **LIFE-001 (REQUIRED, still PARTIAL)**: this contradicts the checklist's own principle 1 (finding 4). Either implement `bx_`, or reclassify it as ADAPTED and accept that strict Boat clients break.
- **SDK-001/006/008 (DONE in `gaps/B.md`)**: should be PARTIAL. Boat's documented positional call style and module paths fail (finding 5).
- **ENV name-clash row (DONE in `gaps/A.md`)**: it's an intentional deviation, so it should be ADAPTED with a reason, or fixed (finding 11).
- **CLI-121 / BILL-020 (N/A)**: low confidence. `billing cancel --keep-disks` is partly a retention feature (keep stopped disks 90 days instead of 14). A credits-world version is possible, but it is tied to a paid month, so N/A is defensible.
- **OTEL-006 (REQUIRED)** *(sub-review)*: `service.name` is `crowdvm`, not `boat`. The row should say ADAPTED.

I accept the other adaptations as preserving each feature's purpose:
- credits for billing, plans and seats
- local accounts and device-code login for OAuth
- host-routed `<id>-<port>.<domain>` URLs for public HTTPS
- a PAT-polling CI controller instead of the GitHub App
- node labels for regions
- `/dev/kvm` only on nodes with nested KVM

## Spot checks of claimed-DONE items

**Run live on the QEMU node (all passed):**
- **Create:** `new --type small` took about 4 s to ready. `df -h /` shows 49 GB free on a 60 GB root. 2 vCPU, ~4 GB RAM.
- **Sandbox environment:**
  - `sudo` works and Docker is active
  - `DISPLAY=:0` is set in exec (DESK-020)
  - all seven harness binaries are on PATH
  - `cua-driver` is active as a user unit, and `computer` is registered in `opencode.json`
- **TOOL-007:** `crowdvm`, `host`, `boat`, `box`, `ascii-record-desktop` and `crowdvm-record-desktop` exist in the VM. `ASCII_TOKEN`, `CROWDVM_TOKEN` and `BOAT_ID` are set. `host 8080` from inside the VM returned host and path URLs.
- **Hosted ports:** the token URL gives 200 through the `<sub>-8080.localhost` host route; without the token it gives 401.
- **LIFE-043:** a memory hog got `exitCode 137`, `signal SIGKILL`, `oomKilled: true`, and the sandbox stayed up.
- **SNAP-005/006:** `~/.crowdvm/pre-stop` ran as `user` on stop. After resume, `~/keep.txt` was kept and `/tmp/x` was gone. Stop and resume each took about 1–4 s.
- **CMD-017/018, CMD-025:** `history` (list and stored detached output `det1..det3`) and `ps` (`live:false`) both work while the sandbox is stopped.
- **AGT-002/003/013:** a `prompt` to OpenCode (`opencode/beam-free`) created the file, streamed events and listed the conversation.
- **Fork (LIFE-022):** returns 202 asynchronously, as in Boat; `--wait` is available. The fork had the source's files and its own hostname.
- **Saved snapshots:** `snapshot ID NAME` worked. The Python SDK `create(var_from="tmpl1", idempotency_key=…)`, `wait_until_ready`, `exec_command` and `delete_sandbox(x_ascii_confirm_delete=…)` all worked against the live server.
- **SIZE-011:** with the node full, a create waits for capacity instead of failing with 503. My first SDK run "hung" because of this, which is correct behaviour.
- **Network isolation (SEC):**
  - the guest cannot reach the host's LAN IP or the node UI through 10.0.2.2
  - it can reach the server through 10.0.2.2 (got 401) and the internet (got 200)
- **SSH:** `ssh ID -- cmd` and `ssh-config --print` (a ProxyCommand tunnel with a pinned known_hosts) work. `desktop --no-open` returns a viewer URL with a fragment token.
- **Node:** join, benchmark, score, tier (platinum), GPU and nested detection, and image import/push all work.

**Read in code (hold up):**
- **LIFE-014:** resume during stopping cancels the stop when the machine still runs, or restarts the sandbox on the same node afterwards (`lifecycle_place.go:173-228`).
- **LIFE-041:** a node-lost rebuild from a fully uploaded chain, a 3-day error hold with a day-2 warning, and notices (`lifecycle_place.go:256-300`, `lifecycle_holds.go:54-125`).
- **SNAP-023 / ENV-018:** foreign deploys are scrubbed before ready (`sandboxes.go:943-945`, `lifecycle_secure.go`).
- **SIZE-010:** `type_too_small`. **SNAP-001:** 60 s live snapshots by default. **LIFE-023:** live guest probe. **SEC-014:** trust placement (`scheduler.go:102-106`).
- **Webhooks:** the event list is exactly Boat's six, and the signing scheme matches.
- **CI:** a real GitHub polling controller with JIT runners (`feat_ci.go:351-850`). Gitea is not covered (CI-001 is honestly marked PARTIAL).
- **API coverage:** all 122 operations route to real handlers. The 501 stubs in `routes.go:53-67` are all replaced (`router.go:99-111`).

**Claimed DONE/FIXED but not true:** LIFE-034, SNAP-008 and LIFE-040 (findings 1–2); SDK-006 and SDK-008 (finding 5); the env name-clash row (finding 11).

## What would flip this to AGREE

In order:
1. Keep a sandbox usable and unmetered during a refused stop: don't set `archiving` until the save succeeds, and don't clean the live disk before the save is known to work.
2. Pause metering and send a notice while a sandbox is unresponsive.
3. Emit `sandbox.error` on node loss, and `sandbox.ready` on recovery from `error`.
4. Make the Python SDK accept Boat's positional request models and `boat_sdk`-style module paths.
5. Accept and mint `bx_` ids (and `bsh_` share codes).
6. Ship the in-sandbox `crowdvm` skill, `/llms.txt`, `/docs/*.md` and a docs MCP server.
7. Require a browser session for key rotation, and a key-creating credential for revoking other keys.
8. Smaller items: apply the "not saved" exclusions to live snapshots; support `snapshot pull <sandboxId>`; report honest deletion-operation status; make your own environment win a name clash; block loopback and link-local webhook targets.

## TL;DR

DISAGREE, narrowly. The API surface is complete (122/122 operations) and the CLI is nearly complete. Most claimed fixes are real and work on a live KVM node. But some supportable Boat behaviours are missing or wrong:
- a refused stop locks the sandbox
- an unresponsive sandbox keeps billing
- node-loss webhooks are wrong
- Boat-style Python SDK calls and `bx_` ids break
- the agent docs, MCP server and skill are absent
- any key can rotate itself

PARITY and the gap audits mark several of these as done.
