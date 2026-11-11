# Review opus-7: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, round 6). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE

All 8 blockers from opus-6 (B1–B5) and sonnet-4 (B1–B3, where sonnet-4 B1 is the same issue as opus-6 B4) are fixed. So are all 9 claimed non-blocking fixes. I reproduced almost all of them live on my own KVM node. The eBPF tracer now records socket output sent with `send`/`sendto`/`sendmsg` (the journal through `logger` and `/dev/log`, TCP, UDP) from ssh sessions and from processes an agent starts. Process records carry exit codes and signals, and the computer-use MCP server is no longer recorded.

One supportable, documented behaviour is still wrong, and I reproduced it live:

1. **B1. `crowdvm --org personal …` is ignored when an organization is active.**
   - The CLI drops the value `personal` before sending the request (`internal/cli/app.go:420-422`), so the server falls back to the active org.
   - `crowdvm --org personal new` bills the organization. Boat documents this exact command as "bill one create to your account while an org is sticky" (`cli-reference.md:357,1369`).
   - Combined with the opus-6 B3 fix, `crowdvm --org personal api-key create` now makes an **organization key**. Boat lists that command as the way to make an account key (`api-keys.md:25`). This regression came with the B3 fix: before it, such a key became an account key, if only by accident.

The fix is three lines. My sweep and two sub-reviews found 13 more gaps, all minor. Full e2e run 5 passed (`EXIT=0`).

## Method

1. **Read** opus-6 and sonnet-4 (findings, fix lists, flip criteria) and the code behind each claimed fix: `internal/guest/bpf/writer.c`, `internal/guest/tracer_bpf.go`, `internal/guest/tracer.go`, `internal/server/apikeys.go`, `internal/server/orgs.go`, `internal/server/lifecycle.go`, `internal/server/feat_host.go`, `internal/cli/*`, `internal/client/*`, the SDKs, `integrations/*` and `web/js/pages/keys.js`.
2. **Build and tests.**
   - I built all four binaries (`CGO_ENABLED=0`, the Makefile's targets) into my scratchpad, not `bin/`, so the running e2e was left alone.
   - `go vet ./...` is clean, and `go test ./...` passes (`test/e2e` skips without `CROWDVM_E2E`).
   - I did not run e2e. I read `…/232e4b2f…/scratchpad/e2e-run5.log`: `--- PASS: TestE2E (663.18s)`, `EXIT=0`. Every group passed, including `process-history` (32.08 s), `apikeys`, `orgs`, `sdk` and `boat-sdk-samples`. Only `bootloop` was skipped.
3. **Own cluster** under `~/.local/share/review-opus-7`.
   - `crowdvm-server` on `127.0.0.1:9960` and one `crowdvm-node` (qemu, KVM, UI on `127.0.0.1:9961`, 6 vCPU / 10 GiB / 2 sandboxes), image `ubuntu-24.04-crowdvm-v3`.
   - I made one long-lived sandbox (`bx_aqksqtck`) plus short-lived forks and creates.
   - Teardown is described at the end.
4. **Live checks** of every blocker fix and most minor fixes, each with a unique marker (below).
5. **Independent sweep.**
   - Two read-only Opus sub-reviewers covered the pages the earlier reviews cited least.
     - (A) opentelemetry, webhooks, all data-retention, environments, auto-stop, always-on services, background commands, hosting firewall and host-cli, and all desktop-streaming pages.
     - (B) the Python, TypeScript and overview SDK pages, migrating-from-box (renamed names), integrated-agents, platform-guide, recipes, agent-auth, ai-tools, machines fail-fast and tools, and ssh editors.
   - Myself:
     - every `boat … --json | jq …` recipe in the 219 pages, run against the CLI;
     - every `### boat …` heading of `cli-reference.md` and every `--flag` in it, checked against `crowdvm … --help`;
     - the org and key scoping around the B3 fix.
   - I re-checked every sub-review finding I kept.

## Fix status

F = fixed. "live" = reproduced on my node. "code" = read only.

| # | Finding | Status | Evidence |
|---|---|---|---|
| opus-6 B1 | Socket output via `send`/`sendto`/`sendmsg` (journal) not recorded | **F (live)** | See "Tracer" below. `writer.c:286-310` hooks `sys_enter_sendto`/`sys_enter_sendmsg` (msghdr iov walked by `send_iov`, `:218-243`). `tracer_bpf.go:78-79` attaches them, and `:182` (`message`) strips the syslog `<PRI>` prefix. |
| opus-6 B2 | No exit code on process records | **F (live)** | `writer.c:338-357` reads `task_struct->exit_code` on the last thread's exit. `tracer_bpf.go:141` turns it into a `WaitStatus`, and `tracer.go:288-300` sets `exitCode`, or `signal` plus `status:"killed"`. Live: `exit 3` → `exitCode:3`; `kill -TERM $$` → `killed`/`SIGTERM`; agent script `exit 7` → `exitCode:7`. |
| opus-6 B3 | Key made while an org is active becomes an account key | **F (live), but see new B1** | `apikeys.go:456-460` defaults to `ActiveOrg` for session callers. Live: `org switch acme7`, then `api-key create orgk7` → `GET /account/data-retention` **403**. After `org switch personal`, a new key → **200**. `GET /api-keys` returns `org: team_…`, and the dashboard filters on it (`web/js/pages/keys.js:28-30`). The fix also brought out the `--org personal` regression in new B1. |
| opus-6 B4 = sonnet-4 B1 | `info --json` is not Boat's `{"sandbox":…}` shape | **F (live)** | `cmd_sandbox.go:345-353`. Live, Boat's exact loops all ended: `until [ "$(crowdvm info $F --json \| jq -r .sandbox.state)" = "stopped" ]`, the `.sandbox.hydrated` check, and the `setup.md:100-109` `setupStatus`/`setupError` loop (a setup script that exits 4 → `failed`, `exit code 4`). Nothing in the repo still reads the old flat shape (grep of skills, guides, integrations, SDKs, e2e). |
| opus-6 B5 | `api-key list --all` never shows in-sandbox keys | **F (live)** | `client/account.go:263-268` sends `all=1`, and `cmd_admin.go:57` passes it. Live: `--all` lists `sandbox bx_aqksqtck` and `sandbox token bx_aqksqtck`; without `--all` they are hidden. |
| sonnet-4 B2 | `fork --no-snapshots` ignored | **F (live)** | `lifecycle.go:1265,1390-1391`. Live: fork `bx_p5uxfw6y` → `snapshots:false`. After `stop`, `resume` → `409 snapshots_disabled` ("its disk was not kept"). One cosmetic leftover: the stopped fork still reports `snapshotAvailable:true` (n-4). |
| sonnet-4 B3 | TS SDK sends `Set` as `{}` | **F (live)** | `sdk/typescript/src/client.ts:155-156` (and `dist/client.js:125`). Live through `node`: Boat's `createWebhook({…, events: new Set([…])})` → events `['sandbox.ready','sandbox.error']`. `updateWebhook({webhookId, events: new Set(["sandbox.error"])})` → updated. |
| opus-6 n-1 | AgentEnv README gives a key form that returns 401 | **F (live)** | `integrations/agentenv/README.md:39-40` now says to replace `cvm_` with `boat_`. Live with a real `cvm_` key: `boat_<rest>` 200, `box_<rest>` 200, `boat_cvm_…` 401 (the README no longer suggests that form). |
| opus-6 n-2 | Computer-use MCP server recorded as a process | **F (live, OpenCode)** | `tracer_bpf.go:285-303` (`platformOrProtocol`: `/opt/crowdvm/…`, `cua-driver`, or stdout on a guest-held pipe, which also covers a wrapped `codex`). Live: an OpenCode prompt (its config includes the `cua-driver mcp` server, `agents/harness.go:414`) produced exactly 3 process records: the script, `logger` and `python3`. None was for `cua-driver` or `opencode`. The codex-wrapper path is code only. |
| opus-6 n-5 = sonnet-4 m-4 | `crowdvm snapshots` lists only automatic snapshots | **F (live)** | `cmd_snapshot.go:45-70`. Live: after `snapshot bx_… rv7saved`, `crowdvm snapshots` gave `savedSnapshots:[rv7saved]` and `allowance{used:1, included:10…}`, then the automatic snapshots. |
| opus-6 n-6 | Box `Deprecation` date 2025 instead of 2026 | **F (live)** | `feat_legacy.go:22`. Live header: `Deprecation: @1789430400`. |
| sonnet-4 m-1 | Python `boat_sdk.exceptions` aliases every class to one | **F (run)** | `sdk/python/src/boat_sdk/exceptions.py:9-27`. With `python3 -I`: `NotFoundException is ConflictException` → False, both are subclasses of `ApiException`, and `ApiTypeError` & co. exist. |
| sonnet-4 m-2 | Bad or duplicate `hostedPorts` accepted | **F (live)** | `feat_host.go:461-480` (`checkHostedPorts`) runs before the insert (`sandboxes.go:814`). Live: `[3000,3000]` → `400 invalid_hosted_ports` "Port 3000 is listed twice", `["x"]` → 400, and no sandbox was created. |
| sonnet-4 m-3 | `harbor_box_environment` module missing | **F (code)** | `integrations/harbor/harbor_box_environment.py` re-exports `BoxEnvironment`. `pyproject.toml` `py-modules` lists it. |
| sonnet-4 m-5 | `BOAT_ORG`/`BOAT_API_URL` ignored | **F (live)** | `internal/cli/app.go:407,413,419`. Live: `BOAT_ORG=nonexistent-org crowdvm list` → `403 not_org_member`. The value `personal` is still dropped, though (new B1). |
| Regressions | Side effects of these fixes | **One (new B1)** | The flag sweep found no new misses: only `billing cancel --keep-disks`, `--google` and `--agentid`, all N/A. Every `### boat …` command exists. Boat's `fork … --json \| jq -r .id` recipe returns one id, and the `new --json \| jq 'select(.event=="ready")'` recipes work. |

### Tracer, live (sandbox `bx_aqksqtck`)

| Case | Result |
|---|---|
| ssh: `logger rv7-logger-ssh` | One record, `logger rv7-logger-ssh`, `exitCode:0`, text `Nov 10 11:01:46 user: rv7-logger-ssh`. No second copy from the journald follower. |
| ssh: Python TCP `send` + `sendall`, UDP `sendto` + `sendmsg([a,b])`, AF_UNIX `sendto("/dev/log")` | All recorded (`rv7b-tcp-send`, `rv7b-udp-sendto`, `rv7b-unix-devlog`; in t1 also `rv7-tcp-sendall` and `rv7-udp-sendmsg`, with the two iov parts joined). |
| ssh: `systemd-cat -t rv7tag echo …` | Recorded, but with the journal stream-protocol header as text (`rv7tag\n\n6\n1\n0\n0\n0\n`) ahead of the line. Cosmetic (n-2). |
| ssh: `sh -c 'exit 3'`, `sh -c 'kill -TERM $$'` | `exited`/`exitCode:3`; `killed`/`signal:"SIGTERM"`. |
| Agent (OpenCode, `opencode/beam-free`) runs a script that does `echo`, `logger`, a Python `/dev/log` `sendto` and `exit 7` | Three records: `/bin/sh /home/user/ag.sh` (exitCode 7, `rv7-agent-echo`), `logger rv7-agent-logger`, and `python3 -c …` (`rv7-agent-devlog`). No harness or MCP record. |
| `command-history` API: `mode`, `q`, `limit`, `cursor`; `/logs` `stream`/`offset`/`limit`/`encoding=base64` | All honoured (`nextCursor` pages correctly, and `offset=4&limit=10` returned 10 bytes with `nextOffset:14`). |

## Blocking findings

### B1. `--org personal` (and `CROWDVM_ORG`/`BOAT_ORG=personal`) is dropped by the CLI, so the active org wins (CLI-005/ORG-007, KEY-002)

**What Boat documents:**
- `cli-reference.md:357`: "To change the wallet for one create, run `boat --org <org> new` or `boat --org personal new`."
- `cli-reference.md:1369`: `boat --org personal new      # bill one create to your account while an org is sticky`.
- `api-keys.md:25`: an account key is made from "the dashboard with Personal selected, `boat --org personal api-key create`, or a request that names no organization".
- `organizations__active-wallet.md:25,45` lists `personal` as a valid org reference.

**What CrowdVM does:**
- `newClient` turns `personal` into an empty org: `if c.Org == "personal" { c.Org = "" }` (`internal/cli/app.go:420-422`). So no `X-CrowdVM-Org` header and no `?org=` are sent (`internal/client/client.go:369-370,402-403`).
- The server then picks the active org: `Ctx.Scope` (`internal/server/orgs.go:131-140`) for sandboxes, and the new B3 default (`internal/server/apikeys.go:458-460`) for keys.
- The server itself handles `personal` correctly (`orgs.go:66`). Live: `POST /api-keys/scoped` with `X-CrowdVM-Org: personal` while acme7 is active → account key (200 on `/account/data-retention`).

**Live** (with `org switch acme7` active):
1. `crowdvm --org personal new --type small` → `bx_puk45s4j`, and `GET /sandboxes/bx_puk45s4j` gives `"team":{"id":"team_tgh2…","name":"acme7"}`. The org was billed, not the user.
2. `crowdvm --org personal api-key create persk7b` → `GET /account/data-retention` with it → **403**, and `GET /api-keys` shows `persk7b org=team_tgh2…`.

**Why it matters:** this is a wallet and billing error, and it is also a security-scoping error. The user asks for an account key and gets an org key that bills and sees the org. The opposite mistake was opus-6 B3. It is all CLI-side and LAN-agnostic.

**Fix:** keep `personal` and send it as the header or query. `resolveOrgRef` already maps it to personal. Do the same in the one place that reads `c.Org` for display (`cmd_admin.go:305,322`). Add a CLI test: with the active org set, `--org personal new` → personal wallet, and `--org personal api-key create` → account key.

## Non-blocking findings

| ID | What | Evidence |
|---|---|---|
| n-1 | `crowdvm host --json` lacks `sandboxId`, and leaves out `localhost` when it is false. Boat's `hosting__host-cli.md:43` lists `sandboxId, port, url, access, isProtected, localhost`. The server sends `sandboxId`, but the typed client struct drops it. | Live: `{"port":8088,"url":…,"isProtected":true,"access":"private","hostUrl":…,"pathUrl":…,"firewall":…}`. `internal/cli/cmd_access.go:532`, `internal/client/types.go:439-445` (sub-reviewer A). |
| n-2 | Tracer cosmetics: (a) `systemd-cat` records carry the journal stream header (`rv7tag\n\n6\n1\n0\n0\n0`). (b) Output a forked child writes before `exec` stays under the pre-exec command line. Live, the bash heredoc and the following `python3 -` sends both landed in a record titled `bash /home/user/t1.sh`, because the record key is the pid and exec does not split it. | Live records `cmd_xyjxabqc4r37` and `cmd_csmjsmeu7wsn`. `tracer_bpf.go:127-134` (exec clears the verdict cache, but not `t.recs["pid:N"]`). |
| n-3 | Socket capture records plaintext IPC request text. For example, the `docker` CLI's HTTP calls to `/var/run/docker.sock`, including `X-Registry-Auth` headers, which hold registry credentials after `docker login`. Boat's text ("to a socket") covers this, and plain `write(2)` to sockets was already captured before this round, so it is not a parity gap. It is worth a redaction rule or a note in `docs/guide/security.md`. | Live record `cmd_4pufcuqqsbnp` (`docker run …`, 4926 bytes of `HEAD /_ping`/`POST /v1.56/containers/create` text). |
| n-4 | A stopped snapshots-off sandbox reports `snapshotAvailable:true` (the fork's create-time chain), although its disk was not kept. | Live `bx_p5uxfw6y`: `{"state":"stopped","snapshots":false,"snapshotAvailable":true}`, then resume → `409 snapshots_disabled`. |
| n-5 | `origin:"template"` snapshots are never listed (`data-retention__snapshots-and-monitoring-off.md:19`, spec enum `[captured, template]`). A snapshots-off fork lists nothing. This carries over sonnet-4 m-6, which no one claimed fixed. | `internal/server/snapshots.go:392`. Live: `crowdvm snapshots bx_p5uxfw6y --json` → `{"snapshots":[]}`. |
| n-6 | Desktop and browser URL tokens are single-use. Boat says to open the URL within 10 minutes and that a reload, or a reconnect after 10 minutes, needs a new URL, which implies a reconnect inside the window works (`desktop-streaming__urls-and-security.md:38-41`). An embedded viewer for someone not signed in can't reconnect after a blip. DESK-005's note ("one-time token") is stricter than Boat. | `internal/server/feat_desktop.go:455-461` (sub-reviewer A, code). |
| n-7 | The dashboard says auto-stop is "after last activity". Boat (`long-running-tasks__auto-stop.md:10`) and the server count from creation or the last resume. | `web/js/pages/sandbox.js:92` (sub-reviewer A, code). |
| n-8 | `crowdvm api-key list --json` drops the `org` field the API returns (typed struct). The CLI can't show which keys are org keys. | Live: the API gives `orgk7 team_…`, and the CLI JSON has no `org` key. `internal/client/types.go` `APIKey`. |
| n-9 | `PARITY.md:482` KEY-001 still says "no legacy prefixes", while `boat_`/`box_` are accepted (live 200). This is a stale note. | Live 200 with `boat_`/`box_` keys. |
| carried | opus-6 n-3 (detached command stays `running` while the sandbox is stopped), n-4 (container records with no exit code, still running for up to 10 min), n-7 (`api-key list` text columns), n-8 (`--org` name case). sonnet-4 m-7 (`accountStatus` never `closed`), m-8 / opus-5 m-7 (shrinking keeps the larger disk), m-9 (`citations`, `closureContactEmail`, `chrome_profile_unsupported`). None was claimed fixed, and I didn't re-check them. | See opus-6 and sonnet-4. |

### From sub-reviewer B (SDKs, migration, integrated agents, platform guide), all minor

Sub-reviewer B ran Boat's SDK doc samples live against an in-process server (`sdk/tools/doc_samples.py --base`). All 106 Python and 106 TypeScript methods and all 14 TS helpers exist with Boat's parameter names. Each failing sample traced to the servertest fake guest, not to CrowdVM. e2e `boat-sdk-samples` passed against real VMs in run 5.

| ID | What | Evidence |
|---|---|---|
| n-10 | `crowdvm api-key create --box` isn't accepted (`migrating-from-box__renamed-names.md:192`). It sits in API-138 (N/A), and a flag alias would be trivial. | Ran `crowdvm api-key create --help` (sub-reviewer B). opus-6 made the same call. |
| n-11 | The CLI's `sandbox_not_found` hint never fires: the server answers `404 not_found` (live, also on my server), while `internal/client/clienttest/fake.go:201`, `internal/cli/app.go:208` and `internal/cli/cli_test.go:201-205` expect `sandbox_not_found`. Boat contradicts itself: `migrating-from-box__steps.md:77` and `renamed-names.md:116` say `sandbox_not_found`, but its error table (`llms-full.txt`) says `not_found`. So this is a test-fake and hint mismatch, not a parity gap. | Live `GET /sandboxes/bx_missing00` → `{"status":404,"code":"not_found"}`. |
| n-12 | Claude Code's MCP resource tools are turned off only on `crowdvm prompt` runs (`internal/guest/agents/harness.go:112-114`). `claude` run over ssh keeps them. Boat (`integrated-agents__customize.md:138`) says "On a sandbox, …" and doesn't say which. | Code (sub-reviewer B). |
| n-13 | Boat's raw-API editors script (`ssh-access__editors.md:139-145`) writes `HostName null` on nodes without `--direct-host`, because `sshEndpoint`/`machineIp` are null (`internal/server/feat_host_ssh.go:61-67`). `crowdvm ssh-config` covers the need, and SSH-004/008 are ADAPTED for this reason. | Code. |

## Disputed classifications

- **CLI-005 / ORG-007 / KEY-002** (`--org`, active wallet, org keys): marked done, but `--org personal` is broken (B1).
- **DESK-005**: the note "one-time token with a 10-min first-use window" is stricter than Boat (n-6).
- **DATA-010**: correctly REQUIRED, but its template-origin clause isn't implemented (n-5).
- **KEY-001**: the ADAPTED note is stale (n-9). It is harmless.
- **API-138 (N/A)**: as opus-6 said, "no legacy installs" isn't one of the constraints, and the cheap parts (`api-key --box`) could be accepted. It is harmless, so not a finding.
- **DOCS-003 (ADAPTED)**: the note should say CrowdVM ships one `crowdvm` skill, not Boat's skill pack (`ai-tools.md:88`). This is wording only.
- **CMD-021** (`PARITY.md:173`) now matches the code and what I saw live. **CMD-022** holds: the agent and ssh runs produced no duplicate records.
- The N/A rows (26) and ADAPTED rows: I re-read the N/A list and the ADAPTED notes. I accept them, as opus-6 and sonnet-4 did, including SSH-004/007/008, URL-012 and API-138.

## What would flip this to AGREE

1. **B1:** `--org personal`, `CROWDVM_ORG=personal` and `BOAT_ORG=personal` reach the server as `personal`. With an org active, `crowdvm --org personal new` bills the personal account, and `crowdvm --org personal api-key create` makes an account key. Add a test.

The n-items would be nice to have and don't block AGREE.

## Not verified

- Windows nodes, VFIO GPU passthrough, real Claude/Codex/Bedrock credentials, real GitHub/Gitea CI, SMTP, `--direct-host`.
- n-2 (opus-6) for the `codex` wrapper. It is code only, because I had no Codex credentials.
- The desktop session's tty-only tracing mode, and the tracer under load (sonnet-4 ran one stress pass).
- The dashboard beyond reading the code.

## Teardown

I deleted every sandbox I created and stopped my node and server by PID. No QEMU of mine was left running, ports 9960/9961 are free, and I removed `~/.local/share/review-opus-7` and the scratchpad binaries.

## TL;DR

DISAGREE, narrowly. Every blocker from opus-6 (B1–B5) and sonnet-4 (B1–B3) is fixed, and so are all nine claimed minor fixes. I reproduced nearly all of them live:
- socket and journal output from ssh and agent processes is recorded;
- exit codes and signals are recorded;
- the MCP server is not recorded;
- org keys get 403 on account settings;
- Boat's `info --json` loops end;
- `--all` shows in-sandbox keys;
- `fork --no-snapshots` holds;
- the TS `Set` samples work.

One blocker remains: the CLI drops `--org personal`, so with an org active, `crowdvm --org personal new` bills the org, and `crowdvm --org personal api-key create` makes an org key. Boat documents both commands for the opposite. The fix is three lines in `internal/cli/app.go:420-422`.
