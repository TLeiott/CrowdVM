# Review sonnet-5: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (narrowly: one blocker, a small CLI fix)

Every sonnet-4 blocker (B1–B3) and every opus-6 blocker (B1–B5) is fixed. I reproduced each one live on my own KVM node. I found no regression from those changes. e2e run 5 (the team's log) ended `--- PASS: TestE2E (663.18s)`, `EXIT=0`.

One new documented, supportable behaviour is still wrong:

**N1. The in-sandbox `host` / `host url` print JSON, not the URL, whenever their output is captured.** That covers `$(host url 3001)`, the commands API and SDK `.stdout`, and an agent's bash tool. Boat's `hosting__host-cli.md` samples all expect a bare URL. The cause is the CLI's global "piped output gets JSON" rule. The fix is a few lines: `a.rawOutput(cmd)` already exists for this.

Everything else I found is minor.

## Method

1. **Read** sonnet-4, opus-6, the CMD-021/KEY/CLI rows in `docs/PARITY.md`, and the e2e log.
2. **Build and tests.**
   - I built all four binaries into my scratchpad with the Makefile's flags, not into `bin/`, so the running e2e was left alone.
   - `go vet ./...` is clean. `go test ./...` passes; `test/e2e` skips without `CROWDVM_E2E`.
   - The TS SDK suite passes (36 tests). `cd sdk/python && python3 -m unittest` passes (48 tests). `integrations/run_tests.sh` passes.
   - `python3 -I -m unittest discover -s tests` fails with import errors. That is my invocation, not the SDK; the README's documented command passes.
   - I did not run e2e. I read `e2e-run5.log`: every group passed, `bootloop` was skipped, `EXIT=0`.
3. **Own cluster**, ports 9970/9971 only. One `crowdvm-server`, one `crowdvm-node` (qemu, KVM, 6 vCPU / 10 GiB / 2 sandboxes), image `ubuntu-24.04-crowdvm-v3`, guest binary freshly built. Data under `~/.local/share/review-sonnet-5`.
4. **Live checks** of each blocker and of the agent recipes from Boat's `ai-tools.md` and `cli-reference.md`. See the tables.
5. **Two read-only Opus sub-reviewers**, no repo edits:
   - one for the agent-facing surface (SDKs, hosting, desktop, integrated agents, webhooks, OTel);
   - one for the CLI reference, machines, snapshots, retention, orgs, keys, spec schemas and the PARITY N/A and ADAPTED rows.

   I re-checked their blocking finding live. The minor ones I checked in code or live, as marked.
6. **Teardown.** I deleted my sandboxes, killed the node, the leftover QEMU and the server by PID, and confirmed ports 9970–9979 were free. I deleted the data dir last. I touched no process I didn't start.

## Fix status

"live" = reproduced on my own node.

### sonnet-4 blockers

| # | Finding | Status | Evidence |
|---|---|---|---|
| B1 | `info --json` is flat, with the API state names | **F (live)** | `internal/cli/cmd_sandbox.go:343-358` wraps in `{"sandbox": …}` and applies `displayState`. Live: `info ID --json \| jq .sandbox.state` → `ready`, `.sandbox.hydrated` → `true`. `archived`/`archiving` map to `stopped`/`stopping` (`helpers.go:21-31`). Unit test `cli_test.go:190-194` checks `.sandbox.state == "stopped"`. Inside a sandbox, `boat info --json \| jq .sandbox.state` and `boat info self` work. |
| B2 | `fork --no-snapshots` ignored | **F (live)** | The server takes the field (`lifecycle.go:1265,1390-1391,1402`). Live: `fork ID --no-snapshots --type small --json` → `sandbox.snapshots:false`. `stop` of that fork erased it (gone from `list`), as `cli-reference.md:613` says. |
| B3 | TS SDK turns a `Set` into `{}` | **F (live)** | `sdk/typescript/src/client.ts:155-156` (replacer), also in `dist/client.js:125`. Live: `createWebhook({events:new Set([...])})` and `updateWebhook({events:new Set(["sandbox.error"])})` returned `events:["sandbox.error","sandbox.ready"]` and `["sandbox.error"]`. |

### opus-6 blockers

| # | Finding | Status | Evidence |
|---|---|---|---|
| B1 | Output sent with `send`/`sendto`/`sendmsg`, including the journal, not recorded | **F (live)** | `bpf/writer.c:286-299` adds `sys_enter_sendto` and `sys_enter_sendmsg`. `tracer_bpf.go:78-79` attaches them. Live, from ssh: `logger jrn-N` → a `process` record whose output is `Nov 10 … user: jrn-N` (syslog priority stripped). Python TCP `sendall` → `tcpsend-N`. UDP `sendto` ×300 000 → one record, `outputTruncated:true`, 841 KB; the sandbox stayed `ok`. 3000 `logger` calls → 3000 records, no stall. Agent step (opencode `beam-free`, bash tool) → `/bin/bash -c echo agent-s5-marker; sh -c 'exit 2'` recorded. |
| B2 | Process records have `exitCode: null` | **F (live)** | `bpf/writer.c:337-353` reads `exit_code` from the kernel's exit event. `tracer.go:298` stores it. Live: `sh -c 'exit 7'` → `exitCode:7`; a backgrounded `sh -c '…; exit 3'` → `3`; clean processes → `0`; the agent's `exit 2` → `2`. The only `null` I saw is `xfce4-notifyd`, a platform service. |
| B3 | Key made while an org is active becomes an account key | **F (live)** | `apikeys.go:456-459` defaults to the active org. Live: after `org switch acme`, `api-key create orgk2` and `… --org acme` both gave keys that got **403** on `GET /account/data-retention` (`api_key_org_forbidden`). Dashboard filters by the selected org (`web/js/pages/keys.js:28-30`). |
| B4 | `info --json` shape | **F (live)** | Same as sonnet-4 B1. |
| B5 | `api-key list --all` never shows in-sandbox keys | **F (live)** | Live: `list --json` → 3 keys; `list --all --json` → those plus `sandbox bx_…` and `sandbox token bx_…` for each sandbox. |

### Other claimed fixes (sampled)

| Item | Status | Evidence |
|---|---|---|
| opus-6 n-1 (AgentEnv README `boat_cvm_…`) | **F (code)** | `integrations/agentenv/README.md:34-35` now says replace the `cvm_` prefix. |
| opus-6 n-2 (cua-driver recorded) | **F (code)** | `tracer_bpf.go:295` skips `/opt/crowdvm/` and `cua-driver`. |
| opus-6 n-5 (`snapshots` lists saved first) | **F (code)** | `cmd_snapshot.go:41-70`: saved snapshots, then the allowance. |
| opus-6 n-6 (legacy `Deprecation` header) | **F (code)** | `feat_legacy.go:22`: `@1789430400`. |
| sonnet-4 m-1 (Python exception classes) | **F (code)** | `sdk/python/src/boat_sdk/exceptions.py`: real per-status subclasses plus the `Api*Error` classes. |
| sonnet-4 m-2 (`invalid_hosted_ports`) | **F (code)** | `feat_host_test.go:295`. |
| sonnet-4 m-3 (`harbor_box_environment`) | **F** | `integrations/harbor/harbor_box_environment.py` exists. |
| sonnet-4 m-5 (`BOAT_ORG`, `BOAT_API_URL`) | **F (code)** | `internal/cli/app.go:407-419`. |
| sonnet-4 m-7 (`/me` `accountStatus` never `closed`) | **Not F, minor** | `auth.go:649-660` sets only `active` or `suspended`. The separate `closed`/`closedAt` fields exist (`feat_retention.go:289`). |
| opus-6 n-7 (CLI key list: created-resource count, expired label) | **Not F, minor** | `cmd_admin.go:76-84` still prints no live-resources column or expired label. `--json` and the dashboard have them. |
| opus-6 n-3, n-4, n-8 | **Not F, minor** | Not re-checked live; `commands.go:171-183` still marks `lost` only at the next boot. Sub-reviewer 2 re-found n-8 (see M1). |

### Agent recipes from Boat's `ai-tools.md` / `cli-reference.md`, live

| Recipe | Result |
|---|---|
| `id=$(crowdvm new --json \| jq -r 'select(.event=="ready") \| .id')` | Works (the `ready` line holds `id`). |
| `exec ID --cwd proj "npm test"` | Output streamed, exit code 3 passed through. |
| `exec ID --timeout 2 "sleep 10"` | `command timed out after 2s`, exit **124**. |
| `exec --timeout 0` | Works. |
| `pid=$(exec --detach "…" \| jq -r .processId)`, then `--status` / `--attach` | `--attach` followed L1–L3 and exited 6; `--status` gave `exited`, `exitCode:6`. |
| `push ID .` (git-tracked files) | `pushed 2 files` to `/home/user/proj`. |
| `run --push . -- npm test` | Output streamed, `exit 3 · 5s · small · 0.0002 credits · bx_… deleted`, exit 3. |
| `ssh ID "host 3000 --private"` | Gives a token URL, but as JSON (see N1). |
| `limits` | Full JSON. |
| In-sandbox `boat` alias, `BOAT_ID`, the `crowdvm` skill under `~/.claude/skills` and `~/.codex/skills` | Present. |
| `exec` history | One `sync` record with the exit code, no duplicate `process` record. |
| `history` modes and `--limit` | Match `cli-reference.md:852-875`. |

## Blocking finding

### N1. In-sandbox `host` and `host url` print JSON when output is captured (URL-014)

**What Boat documents** (`hosting__host-cli.md`):
- `host url <port>` "prints the full token-gated URL": `https://<sandbox-subdomain>-3001.on.boat.dev?_token=<access-token>`.
- Lines 150-201: `BACKEND_URL=$(host url 3001)`, `… | jq -r '.stdout'`, `result.stdout.trim()` and `….stdout.strip()` all expect a bare URL.
- Line 43: "Use `--json` to print a machine-readable object", so JSON is opt-in.
- `platform-guide__patterns.md:17,38,48`: `print(hosted.stdout)  # https://<subdomain>-3000…?_token=...`.

**What CrowdVM does.**
- `internal/cli/app.go:308-309` turns JSON on for any non-terminal output.
- `internal/cli/cmd_access.go:536` (`host PORT`) and `:612` (`host url`) call `a.result(…)`, so they print JSON in that mode. `rawOutput` (`app.go:251`) is used by other commands but not by these.
- The in-sandbox `host` is a symlink to `crowdvm` (`/usr/local/bin/host`).

**Live, in the sandbox:**
- `X=$(host url 3000)` gave `[{"port":3000,"sandboxId":"bx_…","url":"http://127.0.0.1:9970/p/…?_token=…"}]`.
- `POST /sandboxes/ID/commands {"command":"host url 3001"}` gave `stdout: "{\"port\":3001,\"sandboxId\":…,\"url\":\"http://…\"}\n"`. A client following Boat's `.stdout` sample gets a JSON string, not a URL.
- `crowdvm ssh ID "host 3000 --private"` printed the JSON object.
- CrowdVM's own help example, `BACKEND_URL=$(crowdvm host url bx_f7k2q9hd 3001)` (`docs/guide/cli-reference.md:1840`), breaks the same way.

**Why the tests miss it:** `test/e2e/basic_test.go:563` only checks that the output contains `/p/<id>/8080`, and JSON contains that too. `docs/PARITY.md:264` (URL-014) is marked done with no note.

**Why it's supportable:** it's output formatting. Call `a.rawOutput(cmd)` in `host`, `host url` and `host list` (or whenever argv[0] is `host`), keeping `--json` explicit.

## Non-blocking findings

| ID | What | Evidence |
|---|---|---|
| M1 | `crowdvm --org "<name>" org default-visibility` compares the org name case-sensitively. Boat: "Case does not matter" (`organizations__active-wallet.md:24`). Same as opus-6 n-8 (`org` status). | `internal/cli/cmd_admin.go:305,590`. Sub-reviewer 2, live: lower-case name → `no organization in scope`, exit 2; exact case works. |
| M2 | Captured `host list` prints the full access tokens in JSON. Boat: "`host list` does not print the access token" (`hosting__host-cli.md:146-148`). The text table is correct. The same fix as N1 mostly covers this. | `cmd_access.go:557`. Live: `crowdvm exec ID -- host list` printed both `_token=…` values. |
| M3 | A key without `account.read` makes `crowdvm status --check` exit 1 ("not signed in"), though the key works. `/me` does require `account.read` (`api-keys__scopes.md:45`), so the 403 is right. Only the signed-in verdict is off. The in-sandbox key hits this. | `cmd_auth.go:467`. Live in the sandbox: `signedIn:false`, rc=1. The skill's "outside" step uses `status --check`, so custom-action keys would be told to log in again. |
| M4 | `api-key usage` doesn't count requests refused for scope (`s.usage.hit` runs only after `checkAction`). Spec: "Requests authenticated with this key". | `internal/server/router.go:250-256` (sub-reviewer 2, live). |
| M5 | PARITY says `login --google`, `wallet pay` and `billing portal` print a clear "not supported" error (CLI-012, CLI-131). They give `unknown flag`/`unknown command`, exit 2. Either add the messages or fix the rows. | Live: `login --google`, `wallet pay 50`, `billing portal`. |
| M6 | The Python SDK sets `requires-python >=3.10`; Boat documents 3.9 (`sdks__python.md:95`). The sub-reviewer found no 3.10-only syntax but had no 3.9 interpreter. | `sdk/python/pyproject.toml:10`. Not run on 3.9. |
| carried | opus-6 n-3 (a detached command running at stop stays `running` until the next boot), n-4 (container records stay `running` up to 10 minutes), n-7 (CLI key table columns). | See above. |

## Disputed classifications

- **URL-014** (`docs/PARITY.md:264`): marked done, but captured output is JSON (N1, M2).
- **CLI-012, CLI-131** (N/A): the N/A reasons are valid constraints (no third-party OAuth, no payments), but the "clear error" claim is false (M5).
- **CMD-021** (`PARITY.md:173`): now accurate. The row matches the code and what I saw live.
- **N/A and ADAPTED rows in the sub-reviewer's slice:** its reasons all trace to a stated constraint. I found none to challenge.

## What would flip this to AGREE

1. **N1:** make in-sandbox `host`, `host url` and `host list` print their documented plain output (bare URL, `(gated)` table) even when piped or captured, with `--json` still available. Add an e2e step that checks `$(host url PORT)` is a bare `http…?_token=…` URL, and that `command("host url N").stdout` is too.

M1–M6 would be nice to have and don't block AGREE.

## Not verified

- Windows nodes, VFIO GPU passthrough, real Claude, Codex or Bedrock credentials, real GitHub or Gitea CI, SMTP, `--direct-host`.
- A stop-then-resume run of the `until [ "$(info … | jq -r .sandbox.state)" = stopped ]` loop on a live sandbox. I relied on `cli_test.go:190-194`, the `displayState` mapping (`helpers.go:21-31`) and the live `ready` read.
- The dashboard beyond the code I read.
- Python 3.9 (M6).
- Tracer behaviour under a loaded node. I ran one UDP and one `logger` flood and the sandbox stayed healthy.

## TL;DR

DISAGREE, narrowly. All eight earlier blockers (sonnet-4 B1–B3, opus-6 B1–B5) are fixed, and I reproduced them live:
- `info --json` now has `.sandbox.*` with CLI state names;
- `fork --no-snapshots` is honoured;
- a TS `Set` is serialised as an array;
- org-active keys are org keys;
- `api-key list --all` shows in-sandbox keys;
- the tracer records `logger`, TCP and UDP sends, with exit codes.

e2e run 5 passed. One new blocker: in-sandbox `host url` and `host` print JSON instead of the URL when captured, so Boat's `BACKEND_URL=$(host url 3001)` and SDK `.stdout` recipes break (`internal/cli/cmd_access.go:536,612`, `app.go:308`). It's a few-line fix.
