# Review sonnet-6: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (narrowly: one blocker, a small CLI fix)

Both earlier blockers are fixed. I reproduced each one live on my own cluster:

- **sonnet-5 N1.** `X=$(host url N)`, `$(host N)`, `command("host url N").stdout` and `ssh ID "host N --private"` now print a bare URL.
- **opus-7 B1.** `--org personal` works while an org is active. It bills the personal wallet, and `--org personal api-key create` makes an account key.

I found no regression from either change. The team's e2e run 6 (`e2e-run6.log`) ended `PASS`, `ok … 662.004s`, `EXIT=0`.

One documented, supportable behaviour is still wrong, and it sits on the same code path as sonnet-5 N1:

**N1. Captured `host list` prints full access tokens as JSON.**
- Boat: "`host list` shows protected ports as `(gated)` … does not print the access token. To print the full URL with `_token=...`, use `host url <port>`" (`.ref/pages/hosting__host-cli.md:146-149`).
- CrowdVM's own checklist row says the same: URL-014 "(`(gated)`, `(localhost)`, no tokens)" (`docs/PARITY.md:264`).
- Every `host list` sample on Boat's page runs captured: curl `{"command":"host list"}`, `sandbox.command(... "host list")` and `CommandRequest(command="host list")`. Captured output goes to the JSON branch.
- That branch is `internal/cli/cmd_access.go:557-558`, `a.emit({"sandboxId", "ports": ports})`. The cause is the global piped-means-JSON rule at `internal/cli/app.go:308-309`, which `host list` does not opt out of the way `host` and `host url` now do (`cmd_access.go:516,589`). The token stripping and the `(gated)` marks exist only in the terminal-table branch (`:567-574`).
- Sonnet-5 filed this as M2 and it was not fixed. A second reviewer, a sub-agent working independently, reproduced it through the Python and TypeScript SDK `command()`.

I rate it a blocker because:
- It breaks an explicit, security-relevant sentence of the docs.
- The tokens land in the command history (90 days), which Boat warns about.
- The fix is a few lines.

It is the weakest of the three blockers I have seen on this surface, and I would not hold AGREE for anything smaller.

Everything else I found is minor.

## Method

1. **Read.** sonnet-5 and opus-7 (findings and flip criteria), `docs/PARITY.md` rows for URL-014, CLI-012, CLI-131 and API-138, and the code behind both fixes (`internal/cli/app.go`, `cmd_access.go`, `cmd_admin.go`).
2. **Build and tests.**
   - I built all four binaries into my scratchpad with the Makefile's flags, not into `bin/`.
   - `go vet ./...` is clean, and `go test ./...` passes. `test/e2e` skips without `CROWDVM_E2E`.
   - I did not run e2e. I read the end of `e2e-run6.log`: `PASS`, `EXIT=0`.
3. **Own cluster**, ports 9970–9972 only, data under `~/.local/share/review-sonnet-6`.
   - One server, two qemu/KVM nodes (`revnode`, `revnode2`), image `ubuntu-24.04-crowdvm-v3`, guest binary freshly built.
   - A second user (`bob`) for the saved-snapshot share-code check.
   - I restarted `revnode2` once with `--direct-host 127.0.0.1` to check the SSH editors path.
4. **Live checks.**
   - Both blockers, with captured and TTY output, the commands API, `--json`, and the org and key matrix.
   - A regression pass over the commands whose output the two changes could touch.
   - Boat recipes from `cli-reference.md`, `hosting__host-cli.md`, `long-running-tasks__always-on-services.md`, `machines__tools.md`, `machines__virtualization.md`, `desktop-streaming__drive-and-record.md`, `ssh-access__editors.md`, `snapshots__templates.md`, `security.md` and `api-keys*.md`.
5. **Two read-only Opus sub-agents**, no repo edits, each on its own scratch dir and `revnode2` only.
   - **A:** the SDK and API surface. Every operation in `boat-v1.yaml` against the router and both SDKs, and the SDK doc samples run live (Python and TypeScript).
   - **B:** the docs MCP server, the `crowdvm` skill, `agent-instructions` and the four integrations. It ran every skill command and the integrations' offline tests.
   - I re-checked their findings: A's N1 myself, and B's login finding live (see M1).
6. **Teardown.** I deleted every sandbox, stopped my server and both nodes by PID, deleted the data dir and confirmed ports 9970–9979 are free. The processes on 9960/9961 belong to the other reviewer and I did not touch them.

## Fix status

"live" = reproduced on my cluster.

| # | Finding | Status | Evidence |
|---|---|---|---|
| sonnet-5 N1 | In-sandbox `host` / `host url` print JSON when captured | **F (live)** | `cmd_access.go:516` (`host`) and `:589` (`host url`) call `a.rawOutput(cmd)`; `app.go:249-255` keeps explicit `--json`. See the table below. |
| opus-7 B1 | `--org personal` dropped by the CLI | **F (live)** | `app.go:417-421` sends the value as is. See the table below. |

### N1 live results

All inside the sandbox through `crowdvm exec ID -- bash -lc …`, and through `POST /api/v1/sandboxes/ID/commands`.

| Check | Result |
|---|---|
| `X=$(host url 3001)` | `X=[http://127.0.0.1:9970/p/bx_…/3001/?_token=…]` |
| `Y=$(host 3000)`, `Z=$(host 3000 --private)` | bare URL with `_token` |
| `host url 3001 \| cat` | bare URL |
| `host url 3001 > file` | bare URL |
| `POST /commands {"command":"host url 3001"}` | `stdout:"http://127.0.0.1:9970/p/bx_…/3001/?_token=…\n"`, `exitCode 0`. `jq -r .stdout` gives the bare URL. |
| `{"command":"host 3000"}` | bare URL with token |
| `{"command":"host 3000 --public"}` | bare URL without token |
| `crowdvm ssh ID "host 3000 --private"` (outside, captured) | bare URL |
| `host 3000 --json` (inside) | `{"sandboxId","port","access","isProtected","localhost"}`, which are the fields `cli-reference.md:1003` documents |
| `host url 3001 --json` | `{"port","sandboxId","url"}` |
| `host list` in a pty (`script`) | text table with `(gated)` and no token (correct) |
| `host list` captured (`$(host list)`, command API) | JSON with `?_token=` URLs (see N1) |
| `host hide 3000` captured | JSON `{"ok":true,"port":3000,…}`. No sample on Boat's page shows its stdout. |

### opus-7 B1 live results

Org `Acme6` active.

| Check | Result |
|---|---|
| `crowdvm --org personal api-key create` | `org:null` (account key). Its `GET /account/data-retention` is **200**. |
| `crowdvm api-key create` (org active) | org key. `GET /account/data-retention` is **403**. |
| `crowdvm --org Acme6 api-key create` | org key, 403 |
| `crowdvm --org personal new` | sandbox `team:null`, personal wallet |
| `crowdvm new` (org active) | `team:{id:team_…, name:Acme6}` |
| `crowdvm --org Acme6 new` while personal is active | `team:Acme6` |
| `crowdvm --org personal limits` | personal wallet (10 credits); `--org Acme6` and no flag show the org wallet |
| `crowdvm --org personal org --json` | `active:Personal`, `override:true` |
| `--org PERSONAL list` | works |
| `BOAT_ORG=personal`, `CROWDVM_ORG=personal` | accepted |
| Org key with `X-Boat-Org: personal` | **403 `api_key_org_mismatch`**, as `api-keys.md:24` says |
| Org key with `X-Boat-Org: acme6` or `Acme6` | 200 |
| Account key with `X-Boat-Org: personal` / `acme6` | personal / org wallet |
| `--org acme6 org default-visibility` (lower case) | works, so sonnet-5 M1 is fixed too |

### Regression pass

- `list`, `snapshots`, `env list`, `api-key list`, `webhook list`, `ai profiles` and `data-retention` return the same shape under no flag, `--org personal` and `--org Acme6`. `limits`, `org` and `org members` differ only as they should (wallet and scope).
- The other `rawOutput` users (`exec`, `ssh`, `run`, `ssh-config --print`, `completions`, `ci`) behave as before. `exec`, `ssh` and `run` pass their output through unchanged.
- `exec --detach` still gives JSON when captured, and `exec --status` works (`cli-reference.md:792`).
- `exit 7` passes through exec, `--timeout` gives 124, and `run -- bash -c 'exit 5'` gives rc 5 with the `exit 5 · 4s · default · 0.0002 credits · … deleted` line on stderr.
- The in-sandbox `boat info --json`, `boat info self` and `boat exec self` work, and `boat new` gives `403 api_key_action_forbidden` as `api-keys__scopes.md:60-70` says.

## New findings

### Blocker

| ID | What | Evidence |
|---|---|---|
| **N1** | Captured `host list` (commands API, SDK `command()`, an agent's bash tool) prints full tokenised URLs as JSON, not the `(gated)` table. | Docs: `hosting__host-cli.md:146-149`, `docs/PARITY.md:264`. Code: `cmd_access.go:557-558`, `app.go:308-309`. Live: `POST /commands {"command":"host list"}` returned `\"url\":\"http://127.0.0.1:9970/p/bx_…/3000/?_token=…\"`. Sub-agent A reproduced it through the SDKs. `test/e2e/basic_test.go:560-561` only checks that the output contains `8080`. |

Supportable on a LAN: pure output formatting. Fix: make in-sandbox `host list` (and `host hide`) call `a.rawOutput(cmd)` so they print text unless `--json` is given, and drop `_token` from the JSON that `host list` emits (`url` without the query, or an `isProtected` flag only).

### Minor (do not block AGREE)

| ID | What | Evidence |
|---|---|---|
| M1 | `login --wait` and `onboard --wait` ignore the server saved with the pending login. The skill's no-key flow (`login --json`, then `login --wait`) fails when the server was given with `--server` only the first time. Boat: "Run the first command again and it shows the same link and code. It never replaces a pending link" (`cli-reference.md:196`). | `internal/cli/cmd_auth.go:68`, `:342-349` (found by sub-agent B). Live, empty config dir: `login --server http://127.0.0.1:9970 --json` saved code `XNVV-TCRN`; `login --wait --json` then gave `Post "http://localhost:8420/auth/device/start": connection refused`, and with a reachable default it would have started a second code. It works when `--server` or `CROWDVM_API_BASE` is given both times. Fix: fall back to the saved pending server. |
| M2 | `BOAT_CURRENT_ID`, `BOAT_CLI_*` and the `BOX_*` twins are not exported in the sandbox. Only `BOAT_ID` and `CROWDVM_*` are. | Live `env` in the sandbox. Boat: `migrating-from-box__steps.md:112`, `migrating-from-box__renamed-names.md:165-172`. PARITY API-138 marks the whole migration set N/A ("no legacy installs"), which is defensible for `BOX_*`. `BOAT_CURRENT_ID` is a one-line export. |
| M3 | PARITY CLI-012 and CLI-131 promise a "clear not supported" error. The CLI prints `unknown flag: --google` / `unknown command "pay" for "crowdvm credits"`, exit 2. Fix the rows or add the messages. | Live: `login --google`, `onboard --google`, `wallet pay 50`, `wallet card`, `billing portal`. Same as sonnet-5 M5. |
| M4 | Fork of a running sandbox waits on its own node. Two forks of a running sandbox stayed `cloning` with `node:null` for about 9 minutes while the other node was idle. After I freed a slot, the fork became ready in seconds with its systemd service running. | `docs/PARITY.md` SIZE-011 and LIFE-022, `internal/server/scheduler.go:61` (`Only`). By design (live clone), and the docs do not promise otherwise. Boat forks from the latest snapshot, so a snapshot-based fallback to another node is possible. I do not count it. |
| M5 | Boat's `boat-ssh-config.sh` (`ssh-access__editors.md`) writes `HostName null` unless the node runs `--direct-host`. | Live: without `--direct-host`, `sshKey` returns `machineIp:null, sshEndpoint:null`; with `--direct-host 127.0.0.1`, Boat's script runs unchanged and `ssh -F ~/.ssh/boat/<id>.conf <id>` works. PARITY SSH-004 and SSH-008 document it and `crowdvm ssh-config` covers editors. A coordinator-side TCP port would remove the dependency, but the adaptation is stated. |
| M6 | Sub-agent B: integrations (Harbor, Eve, evals, agentenv) cannot pick a node or GPU. Harbor refuses GPU tasks (`harbor_crowdvm_environment.py:110,124-125`). | Boat has no placement, and it refuses GPUs too (`integrations__harbor.md:153`). Not a Boat feature. Listed for completeness. |

## Disputed classifications

- **URL-014** (`docs/PARITY.md:264`): marked done, but captured `host list` prints tokens (N1). Its own text says "no tokens".
- **CLI-012, CLI-131** (N/A): the N/A reasons hold (no third-party OAuth, no real payments). The "clear error" claim is false (M3).
- **API-138** (N/A): valid for the Box→Boat rename as a whole. `BOAT_CURRENT_ID` and the `BOAT_CLI_*` variables are cheap to export and are named on a Boat page, so I would move them out of N/A (M2).
- **SSH-004 / SSH-008** (ADAPTED): accepted. The direct-host path works with Boat's own script.
- **URL-014 / CMD-021 and the rest of the sample rows I ran:** accurate.

## What would flip this to AGREE

1. **N1:** in-sandbox `host list` prints the `(gated)`/`(localhost)` table when captured (unless `--json`), with no `_token` in any `host list` output, text or JSON. Add an e2e step that runs `crowdvm exec ID -- host list` captured and checks that it contains no `_token=`.

M1–M6 do not block AGREE.

## What I verified OK

Sandbox side, run live.

**Boat recipes.**
- Recipes that capture output, all as written: `id=$(boat fork … --wait --json | jq -r …)`, `pid=$(boat exec --detach … | jq -r .processId)`, the `until [ "$(boat info … | jq -r .sandbox.state)" = stopped ]` loop, and the `.sandbox.hydrated` loop.
- `exec` forms: `exec` shell-line and argv forms, `--cwd`, `--stream`, `--timeout` (exit 124), `--detach`/`--status`.
- Shell and transfer: `ssh` command and `-- bash -s < file` forms, `push` (git-aware, skips `.gitignore`d files), `run --push -e` (rc passthrough and the summary line), `scp`.
- Error output: JSON error lines and exit codes for not_found, `confirmation_required`, `invalid_type`, `snapshot_not_found`, `provider_not_configured` and `invalid_name`.

**Sandbox behaviour.**
- A systemd service from `always-on-services.md`: active after `stop` and `resume`, the same `host` URL and token, and active in a fork too.
- `ascii-record-desktop` start, status, stop, `scp` and the artifacts API give a valid 6.9 s MP4.
- The tool list in `machines__tools.md`: every command is present. `/dev/kvm`, Docker (`docker run alpine`), apt, internet access (an `https://example.com` request returned 200), passwordless `sudo`, and Ubuntu 24.04 / Linux 6.8.
- `host --localhost --title`: a service bound to `127.0.0.1` answers through the URL. A `_token` URL gives a redirect and cookie, no token gives 401, `--public` gives 200 and `host hide` gives 404.
- In-sandbox key limits per `api-keys__scopes.md`.

**Account and snapshots.**
- Saved snapshots: `snapshot`, `snapshots`, `snapshot share` / `shares` / `unshare`, and another account's `new --from <share code>` (file present, use count 1, revoked).

**Sub-agent A, live** (Python and TypeScript SDK samples):
- All 122 operations in `boat-v1.yaml` are routed and present in both SDKs.
- The lifecycle, command, file, host, snapshot, key, webhook (real delivery with Boat's signature verify), pagination and error-class samples pass.

**Sub-agent B, live and offline:**
- The docs MCP server at `/mcp` (initialize, tools, resources, batches).
- Every command and flag in the skill and `agent-instructions`, and the `skill install` outputs.
- The `agent-auth` endpoints, and the integrations' offline suites (46 + 36 + 7 + 9 + 2 + 6, all passing).

## Not verified

- Windows nodes, VFIO GPU passthrough, real Claude, Codex or Bedrock credentials, real GitHub or Gitea CI, SMTP.
- The integrations' live smokes (`hello_sandbox.py`, `hello-sandbox.mjs`, `check_provider.py`). They cannot be pinned to one node (M6).
- Sub-agent A did not run live `fork` or `set_active_organization`, `update_organization` and `update_data_retention`.
- Python 3.9 (sonnet-5 M6 still stands: `requires-python >=3.10`).
- The dashboard beyond what the API serves, and the Android emulator script of `machines__virtualization.md`.
- Sub-agent B created one anonymous agent account (`usr_0naw0m5gdzxeg65w`) on my throwaway server while testing agent-auth. It was deleted with the data dir.

## TL;DR

DISAGREE, narrowly. Both earlier blockers are fixed and I reproduced them live. `$(host url N)`, `$(host N)` and the commands-API `.stdout` give a bare URL, and `--org personal` bills the personal wallet and makes an account key. I found no regression. e2e run 6 passed (`EXIT=0`). One blocker remains on the same code path: captured `host list` prints full `_token` URLs as JSON instead of the tokenless `(gated)` table that `hosting__host-cli.md:146-149` and PARITY URL-014 promise (`internal/cli/cmd_access.go:557-558`, `internal/cli/app.go:308-309`). It is a few-line fix. Five minor gaps are listed above and do not block AGREE.
