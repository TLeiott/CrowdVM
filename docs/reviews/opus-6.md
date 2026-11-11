# Review opus-6: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, round 5). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE

opus-5's blocker N1 is mostly fixed. The guest's eBPF tracer records ssh sessions (with and without a tty), what they leave running, tmux panes, and processes started by an integrated agent. I reproduced each of these live on my own KVM node. Exec'd commands, services and containers are each recorded once. Of the claimed opus-5 minor fixes, m-1, m-3, m-4, m-5 and m-8 hold. m-6 doesn't: the README it changed now gives a key form that fails (see the table).

Five supportable, documented behaviours are still wrong. I reproduced every one live:

1. **B1 (rest of N1).** Output a process sends to a socket with `send`/`sendto`/`sendmsg` is never recorded. That includes the journal, which is Boat's own example. Neither the BPF probe nor the journald follower catches it from ssh sessions or agent-started processes.
2. **B2.** Process records never carry an exit code. Boat says each one has "its command line, its process id and its exit code".
3. **B3.** A key created while an organization is active (CLI without `--org`, or the dashboard) becomes an account key, with access to account settings.
4. **B4.** `crowdvm info --json` has a different shape from `boat info --json`. All seven of Boat's documented `jq -r .sandbox.state`/`.hydrated`/`.setupStatus` wait loops spin forever.
5. **B5.** `crowdvm api-key list --all` never shows the in-sandbox keys, because the client never asks the server for them.

Each fix is small. B1 and B3 matter most.

## Method

1. **Read** opus-5 (findings, fix list and flip criteria), the CMD-021/022 rows in `docs/PARITY.md:173-174` and `docs/gaps/A.md:232-233`, and the new code: `internal/guest/tracer_bpf.go`, `internal/guest/bpf/writer.c`, `internal/guest/tracer.go`, `test/e2e/process_history_test.go`.
2. **Build and tests.**
   - I built all four binaries (with `CGO_ENABLED=0`) into my scratchpad, not `bin/`, so the running e2e was left alone.
   - `go vet ./...` is clean.
   - `go test ./...` passes; `test/e2e` skips without `CROWDVM_E2E`.
   - I didn't run e2e. I read `…/232e4b2f…/scratchpad/e2e-run4.log`: `--- PASS: TestE2E (646.24s)`, `EXIT=0`. `process-history` passed in 23.7 s, and its agent step ran for real (`agent-started process recorded: cmd_ynxsyvs33t9w "/bin/bash -c echo agent-out-…"`, log line 51). Every group passed except `bootloop`, which was skipped.
3. **Own cluster** under `~/.local/share/review-opus-6`.
   - `crowdvm-server` on `127.0.0.1:9963`; one `crowdvm-node` (qemu, KVM, UI on `:9964`, 6 vCPU / 10 GiB / 2 sandboxes); image `ubuntu-24.04-crowdvm-v3`.
   - A first attempt on port 9930 collided with another reviewer's short-lived server. I stopped my node, deleted that data dir and started again on 9963/9964.
   - Teardown: I deleted both sandboxes and stopped the node and the server. No QEMU or other process of mine was left, ports 9963/9964 were free, and I deleted the data dir.
4. **N1 from new angles** (below), each with a unique marker, read back through `GET /command-history` and `/logs`.
5. **Independent sweep.** One read-only Opus sub-reviewer covered the pages earlier reviews cited least: agent-auth, ai-credentials, ai-tools, the four api-keys pages, the five organizations pages, the six snapshots pages, the six windows-apps pages, the three migrating-from-box pages, concepts, faq, security, setup, quickstart, account, free-trial, compliance and cli-reference. I re-checked its three high or medium findings live (B3, B4, B5) and the rest in code.
6. **Regression sample.** I re-checked several earlier fixes live (see the table).

## Fix status

F = fixed. "live" = reproduced on my node.

| # | Finding | Status | Evidence |
|---|---|---|---|
| opus-5 N1 | Process history misses ssh sessions and agent-started processes | **Mostly F (live), see B1 and B2** | See "N1 checked from new angles" below. |
| opus-5 m-1 | `PromptEvent.data.attachments` never filled; "Attached files" written twice | **F (live)** | `internal/guest/agents/http.go:165-166` fills `eventAttachments`. The CLI no longer edits the prompt text (`internal/cli/cmd_agent.go:105`); only the guest adds the line (`run.go:332-333`). Live: `prompt --attach note.txt` gave a `prompt` event with `attachments:[{filename:"note.txt",mimeType:"text/plain…",base64Data:"aGVsbG8gYXR0YWNoCg=="}]` and the prompt text left alone. |
| opus-5 m-3 | TS SDK can't be `require`d on Node 18–20.18 | **F (code)** | `sdk/typescript/package.json` sets `engines.node` to `>=20.19`, where `require(esm)` works. |
| opus-5 m-4 | No "delete all snapshots" in the dashboard | **F (code)** | `web/js/tabs/snapshots.js:15,46-52` (`Delete all…` → `DELETE /sandboxes/{id}/snapshots`). Live: the same endpoint created `bdop_` operations that reached `completed`. |
| opus-5 m-5 | Harbor ignores `BOX_API_KEY` | **F (code)** | `integrations/harbor/harbor_crowdvm_environment.py:88-89`. |
| opus-5 m-6 | AgentEnv README asks you to patch the prefix check | **Not F: new doc bug (live)** | `integrations/agentenv/README.md:34` now says to put `boat_` in front of the key: `boat_cvm_...`. The server swaps `boat_` for `cvm_` (`internal/server/auth.go:242-245`), so that key becomes `cvm_cvm_…`. Live: `boat_cvm_…` → **401**, `boat_<rest>` → 200, `box_<rest>` → 200. `docs/guide/migrating-from-boat.md:23,47` is correct. Non-blocking. |
| opus-5 m-8 | `runs-on: boat` not accepted | **F (code)** | `internal/server/feat_ci.go:340-355` (`ciNorm` maps `boat*` → `crowdvm*`); test at `feat_ci_test.go:332`. |
| opus-4 B2 | Scoped-key list paginates before filtering | **F (live, holds)** | A read-only key scoped to one of two sandboxes: `limit=1/2/100` each returned exactly that sandbox. |
| opus-4 m1 | `billing extra-starts on --yes` | **F (live, holds)** | Returned `billing.extra_starts`, `enabled:true`. |
| opus-3 N1 | Snapshot deletion ops never complete | **F (live, holds)** | `DELETE /sandboxes/{id}/snapshots` → `bdop_00f8…` went from `pending` to `completed` in about 3 s. |
| sonnet-3 N1 | `/healthz` | **F (live, holds)** | `{"ok":true,"version":"dev"}`. |
| opus-3 N2 | `bx_` id alphabet | **F (holds)** | Minted `bx_muhw2dpv`, `bx_yycuk7z3`. |
| CMD-021 flush at stop | Output still on the sandbox is saved before the stop | **F (live)** | I wrote `…-stop-final` from ssh, then ran `stop --wait` at once. While the sandbox was stopped, history had it under `cat` and `bash` process records. |
| CMD-025 | `ps` on a stopped sandbox | **F (live)** | `live:false` with the last 12-process sample. |

### N1 checked from new angles (sandbox `bx_muhw2dpv`, `tracer: eBPF output capture on` in the guest log)

| Case | Result |
|---|---|
| Real tty ssh session (`script -qc "crowdvm ssh -t ID -- 'echo M; tty'"`, `/dev/pts/0`) | Recorded (`bash -c echo …-ttyreal; tty`). |
| tmux session left running by ssh, pane printing every 2 s | Recorded, all five lines (`bash -c for i in 1 2 3 4 5…`). The two tmux processes get their own near-empty records. That is harmless noise. |
| Background pipe writer left by ssh (`setsid sh -c '… \| cat'`) | Recorded (also covered by e2e). |
| Process started through an integrated agent (OpenCode, `opencode/beam-free`, bash tool running my script) | The script's direct output is recorded (`/bin/sh /home/user/rv6.sh`). So is the background pipe writer it left (`sh -c for i in 1 2 3 4…`, all four lines). |
| Same agent script: `logger M` and a Python `sendto` to `/dev/log` | **Not recorded** (B1). |
| ssh: `logger M` | **Not recorded** (B1). The journal has it with `_SYSTEMD_UNIT=session-6.scope`, `_TRANSPORT=syslog`. |
| ssh: Python TCP `s.send()` / `s.sendall()` to a local listener | **Not recorded** (B1). The same session's `echo` is recorded. |
| `exec` sync, `exec --detach` with a backgrounded child writer | Each is one command record. The child's output is in the detached record. No `process` record. CMD-022 correct. |
| `systemd-run --unit rv6svc sh -c 'echo M'` (from exec) | One `process` record (`/usr/bin/sh -c "echo …-svc; sleep 2"`), not two. |
| `docker run -d alpine sh -c 'echo M; sleep 5; echo M2'` (from exec) | One `docker container rv6c2 (alpine:3.20)` record with both lines. Nothing extra for the docker CLI under exec. |
| Every exited `process` record | `exitCode: null` (B2). |

## Blocking findings

### B1. Socket output sent with `send`/`sendto`/`sendmsg` is not recorded, including the journal (CMD-021, rest of opus-5 N1)

**What Boat documents:**
- `long-running-tasks__command-history.md:89`: "boat records what each process writes to its terminal, to a pipe, or to a socket such as the journal."
- Line 14 names ssh sessions and agent-started processes as sources.

**What CrowdVM does:**
- The BPF program attaches only to `sys_enter_write` and `sys_enter_writev` (`internal/guest/tracer_bpf.go:72-73`, `internal/guest/bpf/writer.c:210,221`). Nothing hooks `sendto` or `sendmsg`.
  - `syslog(3)`, `logger`, `systemd-cat` and `sd_journal_*` reach the journal with `sendmsg`; I confirmed this with strace on `logger`.
  - Python's `socket.send` uses `sendto`.
- The journald follower, which would otherwise see the journal entries, drops units starting with `session-` and `crowdvm-` (`internal/guest/tracer.go:57,65,163-166`). That covers ssh sessions (`session-N.scope`) and agent processes (`crowdvm-workloads.service/agents`).
- So for both sources Boat names, journal output is lost by both paths. The same goes for socket output from programs that use `send*`.

**Live:** `logger` from ssh; `logger` and `sendto(/dev/log)` from an agent-started script; Python TCP `send`/`sendall` from ssh. None produced a record within 40 s, while `echo` in the same sessions did.

**Classification:** `PARITY.md:173` says CMD-021 is "Done … write(2)/writev(2) to tty/pipe/socket fds". The "Done" overstates it, because Boat's own example socket isn't covered.

**Why it's supportable:** it is all inside the guest. Attaching `sys_enter_sendto` and `sys_enter_sendmsg` (walking `msg_iov` like `trace_writev` does) is about 30 lines of the same code.

The other option is to let the journald follower keep entries whose `_SYSTEMD_CGROUP` is an sshd session scope or `…/crowdvm-workloads.service/agents…`. That doesn't double-record, because BPF never sees `sendmsg`. Then add `sendto` for TCP/UDP. Either way, add an e2e step that runs `logger M` from ssh and from the agent step.

### B2. Process records never carry an exit code (CMD-021)

**What Boat documents** (`long-running-tasks__command-history.md:89`): "Each process is one `process` record, with its command line, its process id and its exit code." The spec gives `exitCode` on every command record.

**What CrowdVM does:** `tracer.finished` sets only `status` and `finishedAt` (`internal/guest/tracer.go:284-286`). The kernel exit event (`writer.c:263-281`) doesn't read `task_struct->exit_code`.

**Live:** every exited `process` record had `exitCode: null`, for example `GET …/command-history/cmd_gps339bhgdsz` → `"status":"exited","exitCode":null`. That includes `/bin/sh rv6.sh`, which exited 0.

**Why it's supportable:** add `exit_code` to the CO-RE `task_struct` stub, read it in `trace_exit` (`>>8` for the status, `&0x7f` for the signal), and store it in `ended()`. For containers, Docker's `State.ExitCode` in `config.v2.json` gives the same.

This is the smallest of the five. On its own I'd call it borderline, but it is a stated, testable field of a REQUIRED feature, so I list it.

### B3. A key created while an organization is active becomes an account key (KEY-002)

**What Boat documents** (`api-keys.md:24`): an organization key is made from "the dashboard with that organization selected, `boat --org <org> api-key create`, `boat api-key create` while that organization is active, or `org` on `POST /api-keys/scoped`". Organization keys are refused on account settings (`403 api_key_org_forbidden`). Line 27: "The dashboard lists the keys of the selected organization."

**What CrowdVM does:**
- `handleCreateKey` takes the org only from the body or the org header (`internal/server/apikeys.go:452-455`). It never falls back to the active wallet that `Ctx.Scope` uses (`internal/server/orgs.go:131-139`).
- The CLI without `--org` sends neither.
- The dashboard's create dialog posts `{name, ttl, preset…}` with no org (`web/js/pages/keys.js:86-93`) and sends no org header.

**Live:**
1. `org create acme6`, then `org switch acme6` → `org.active_updated`.
2. `api-key create orgk` → `GET /account/data-retention` with that key returned **200**, so it is an account key.
3. `api-key create orgk2 --org acme6` → **403**, which is correct.

**Why it matters:** a member who switched to an org and made a key "for the org" gets a key that can read and change account settings and webhooks, and bills whatever wallet is active later. That is a scoping and security difference, and organizations are fully implemented on a LAN.

**Fix:** when no org is given and the caller is a session, default `orgRef` to the active org. Have the dashboard send its selected org. Filter the key list by the selected org.

### B4. `crowdvm info --json` isn't Boat's `info --json`, so Boat's documented wait loops never end (CLI-054)

**What Boat documents:** `boat info ID --json | jq -r .sandbox.state` with the CLI state names. It appears in:
- `snapshots__stop.md:60`, `faq.md:163`, `troubleshooting.md:110`, `pricing.md:36` and `machines__change-size.md:38`, as `until … = "stopped"`;
- `readiness.md:82,145`, as `.sandbox.hydrated`;
- `setup.md:103,106`, as `.sandbox.setupStatus` and `.setupError`.

`cli-reference.md:446` says the CLI shows `stopping`/`stopped` and the API `archiving`/`archived`.

**What CrowdVM does:** `info` prints the bare API sandbox object, `a.result(s, …)` (`internal/cli/cmd_sandbox.go:343`). There is no `sandbox` wrapper, and the state is the API name. `crowdvm stop` itself prints `"state":"stopped"` (`cmd_sandbox.go:552`, through `displayState`), so the CLI isn't even consistent with itself.

**Live:** `stop --wait` printed `{"event":"stopped",…,"state":"stopped"}`. Then `info --json` printed `{"id":"bx_muhw2dpv",…,"state":"archived",…}` with no `.sandbox`. Boat's loop gets `null` forever.

**Why it's supportable:** it's CLI output only. The in-sandbox `boat` alias and the migration guide invite exactly these copied recipes, and coding agents learn them from Boat's docs. Print `{"sandbox": {...}}` with `displayState` applied, or at least add the wrapper and keep top-level fields for existing callers.

### B5. `crowdvm api-key list --all` never shows the in-sandbox keys (CLI-028, KEY-017)

**What Boat documents:** `api-keys__scopes.md:62`: the in-sandbox key "is not listed by `boat api-key list` unless you add `--all`". The same is in `api-keys__create-and-list.md:121`.

**What CrowdVM does:** the server leaves machine keys out unless it gets `?all=1` (`internal/server/apikeys.go:318-320,393`). `Client.ListAPIKeys` always calls a bare `GET /api-keys` (`internal/client/account.go:263-264`). The CLI's `--all` only filters what came back (`internal/cli/cmd_admin.go:62-69`), so it has nothing more to show. The dashboard doesn't send `all=1` either.

**Live:** `api-key list --all --json` gave `pg, rev6b, rev6`. `GET /api-keys?all=1` gave those plus `sandbox bx_muhw2dpv` and `sandbox token bx_muhw2dpv`.

**Fix:** add `all=1` to the request when `--all` is set. It's a one-line fix.

## Non-blocking findings

| ID | What | Evidence |
|---|---|---|
| n-1 | m-6 regression: the AgentEnv README tells users to send `boat_cvm_…`, which gets 401. | `integrations/agentenv/README.md:34` against `internal/server/auth.go:242-245`. Live 401/200 above. |
| n-2 | The platform's own computer-use MCP server (`cua-driver mcp`, started by the harness) is recorded as a `process` record holding its JSON-RPC traffic (5919 bytes per prompt). Boat leaves out "the services that boat itself runs". The likely cause: the agents-cgroup rule only skips direct children of the guest (`internal/guest/tracer_bpf.go:319`). For the same reason, harnesses started through a wrapper, such as `codex` (`/usr/local/bin/codex` is a node script that spawns the native binary), would probably have their protocol stdout recorded. **I checked this in code only.** | Live: `cmd_tmsmjxzgquph` and `cmd_xu3wtrtucnhk`, `/opt/crowdvm/cua-driver/cua-driver mcp --socket …`. |
| n-3 | A detached command that was running when the sandbox stopped still reads `status:"running"` in history while the sandbox is stopped. It becomes `lost` only at the next boot. The spec defines `lost` as "the sandbox stopped … while the command ran". | `internal/guest/commands.go:181-183`. Live: `cmd_wxqn4y6u82nw` (`sleep 100`), `running`, `finishedAt:null` on the archived sandbox. |
| n-4 | Container `process` records stay `running` for up to 10 minutes after the container exits, with no exit code. | `internal/guest/tracer.go:270`. Live: `docker container rv6c2` still `running` about 40 s after it exited. |
| n-5 | `crowdvm snapshots` lists only automatic snapshots. Boat lists saved snapshots first and shows the saved-snapshot count and cost (`snapshots__templates.md:34,187`). The data is already in `GET /saved-snapshots` (`allowance`). | `internal/cli/cmd_snapshot.go:15-63,104-125` (code only, from the sub-reviewer). |
| n-6 | The legacy Box `Deprecation` header is `@1757894400` (2025-09-15). Boat's is `@1789430400` (2026-09-15). The code comment itself says 2026. | `internal/server/feat_legacy.go:22`. Boat: `migrating-from-box__steps.md:82`. |
| n-7 | `api-key list` text output has no live-resources column and no expired or expiring labels. The API already returns `resources`, `expired` and `expiringSoon`. | `internal/cli/cmd_admin.go:76-84`. Boat: `api-keys__create-and-list.md:113-115`. |
| n-8 | `crowdvm --org ACME org` compares the name case-sensitively and can print "active: personal". Requests resolve correctly. | `internal/cli/cmd_admin.go:305` (code only). |
| carried | opus-5 m-2 (`citations`, `closureContactEmail`), m-7 (shrinking keeps the bigger disk), m-9 (`chrome_profile_unsupported`). No fix was claimed, and I didn't re-check them. | opus-5. |

## Disputed classifications

- **CMD-021 "Done"** (`docs/PARITY.md:173`, `docs/gaps/A.md:232`): overstated (B1 and B2). The CMD-022 row is right; I confirmed it live.
- **KEY-002, CLI-054, CLI-028/KEY-017:** marked done, but broken as B3, B4 and B5 describe.
- **API-138 (N/A, `--box` key alias):** the reason given, "no legacy installs", isn't one of the constraints. It's harmless, and accepting `--box` as an alias would be cheap. I don't count it as a finding.
- The sub-reviewer re-checked the other N/A and ADAPTED rows in its area and found them justified: KEY-019, ORG-019/022/023, AUTH-001/004/019, SNAP-007/009/012/014 and WIN-006/008. I accept opus-5's position on SSH-004/007/008 and URL-012.

## What would flip this to AGREE

1. **B1:** record socket output sent with `send`/`sendto`/`sendmsg`, at least journal and syslog writes, from ssh sessions and agent-started processes. Add an e2e `logger` step in `process-history`, from ssh and from the agent.
2. **B2:** store the exit code (and signal) on exited `process` records.
3. **B3:** keys created while an org is active (CLI and dashboard) become that org's keys, and the dashboard lists the selected org's keys.
4. **B4:** `crowdvm info --json` gives `{"sandbox": {...}}` with the CLI state names (`stopped`/`stopping`), so Boat's documented `jq -r .sandbox.state`/`.hydrated`/`.setupStatus` loops end.
5. **B5:** `api-key list --all` sends `all=1`.

The n-items would be nice to have and don't block AGREE.

## Not verified

- Windows nodes, VFIO GPU passthrough, real Claude/Codex/Bedrock credentials, real GitHub/Gitea CI, SMTP, `--direct-host`.
- n-2 for the `codex` wrapper. I had no Codex credentials, so this is from code only.
- The desktop session's tty-only tracing mode.
- The rate-limit and `outputTruncated` path of the BPF tracer under load. The opt-in `CROWDVM_E2E_OVERHEAD` probe wasn't run.
- The dashboard beyond the code I read.

## TL;DR

DISAGREE. opus-5's N1 is mostly fixed. Live, the eBPF tracer records ssh output (tty and non-tty), tmux panes, writers left behind, and processes an integrated agent starts. Exec'd commands, services and containers are each recorded once, and e2e run 4 passed with `EXIT=0`. m-1, m-3, m-4, m-5 and m-8 hold, but the README m-6 changed now gives a key form that returns 401. Five documented, supportable behaviours are still wrong, all reproduced live:
- socket and journal output sent with `send*` isn't recorded;
- process records have no exit code;
- keys made while an org is active become account keys;
- `info --json` doesn't match Boat's shape, so its wait loops never end;
- `api-key list --all` never shows in-sandbox keys.

Each is a small fix.
