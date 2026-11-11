# Review sonnet-9: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`, built from source into my scratchpad (same flags as `make build`; the binaries were byte-size identical to `bin/`).

## Verdict: DISAGREE (narrowly: one blocker on webhooks)

Both claimed fixes hold live, and I found no regression next to them:

- **sonnet-8 B1** (`env rename` left the old name on pinned sandboxes): fixed.
- **opus-10 B1** (a secret file under a repository folder stopped the clone): fixed, on both guest paths (boot and live `env upgrade`).

My sweep of integrated agents, long-running work and webhooks found that almost everything works as Boat documents. One documented, supportable behaviour is wrong:

**B1. `sandbox.hydrated` is sent at the wrong moments.** Boat defines it as "A restored sandbox finishes materializing" and says it arrives "after `sandbox.ready`, … can arrive at the same moment" (`webhooks.md`, Events and "After a restore").
- CrowdVM never sends it after a local resume or fork. `hydrated` flips to `true` at the moment of `ready` (`internal/server/lifecycle_secure.go:256`, `markReady`) with no event. The only restore path that emits it is the lazy cross-node chain pull (`lifecycle_secure.go:292-301`, `markHydrated`, called at `lifecycle.go:541`).
- It does send `sandbox.hydrated` when a sandbox is **stopped**: `internal/server/snapshots.go:210-212` publishes it whenever a snapshot of an archiving/archived sandbox is stored. This is what the team calls HOOK-013 ("hydrated on snapshot stored", `docs/gaps/B.md:209`).
- Live evidence, my own receiver with Boat's own Python `verify_webhook` (every delivery verified):

  | Action | Events delivered to a 6-event endpoint |
  | - | - |
  | `resume` of a stopped sandbox | `sandbox.ready` only. No `sandbox.hydrated` within 20 s, although `GET` already says `hydrated:true` at the same millisecond as `ready` |
  | `fork` of a stopped sandbox | `sandbox.ready` only; `hydrated:true`, `hydratedAt` equal to the ready time, no event |
  | `stop` | `sandbox.hydrated` (13:49:49.767), `sandbox.archived` (13:49:50.821), **`sandbox.hydrated` again** (13:49:50.897): two `hydrated` events for one stop, with different event ids and no `previousState`/`state` |

- Why it matters: Boat's docs steer integrators to "wait for `sandbox.hydrated` before work that needs Docker volumes on the real disk". On a single node (the common LAN case) that wait never ends after resume or fork, and the event that does arrive on stop means "a stop finished", which no Boat page says.
- Supportable on a LAN: pure server bookkeeping. Publish `EvSandboxHydrated` where `markReady` sets `hydrated=1` for a restore (`BootKind` resume or fork; not a first create), and drop or rename the publish at `snapshots.go:210-212`. The `docs/E2E.md:73` and `test/e2e/lifecycle_test.go:99` step ("stop (archived + hydrated webhooks)") and `internal/server/feat_webhooks_test.go:106-143` then change to "resume emits ready + hydrated".
- The rest of the webhook surface is correct (see "Checked and fine"), so this is a one-hour server fix. I count it as blocking because the event's meaning, not its spelling, is wrong in both directions. I accept that the team reads HOOK-013 as an adaptation; I do not think the doc text allows it, because the doc describes a restore and the adaptation moves it to a stop.

## Method

1. **Read.** `sonnet-8.md` and `opus-10.md` (blockers, flip criteria, what each covered). Then the code behind each fix: `feat_env.go:778-780` (rename updates the sandboxes' copy), `feat_env_test.go:231-237` (new test), `internal/guest/setup.go:315-360` (clone beside, then `mergeInto`), `apply.go:300-345`.
2. **Build and tests.** Built `crowdvm`, `crowdvm-server`, `crowdvm-node` and `crowdvm-guest` into my scratchpad (not `bin/`, because run 9 was using it). `go vet ./...` is clean. `go test -count=1 ./...`: one run hit `internal/node` `TestGuestHealth` ("timed out waiting for ready", 10 s), under load from three live clusters. The immediate full rerun was all `ok`, and the test alone passed twice (7.2 s). I treat it as a load flake. I did not run e2e. `e2e-run9.log` was still at `=== RUN TestE2E/nodes` when I last read it.
3. **Own cluster**, ports 9970 (server), 9971 (node UI), 9976 (webhook receiver), all in `~/.local/share/review-sonnet-9`. One server with `--webhook-allow-local`, one qemu/KVM node (6 vCPU, 12 GiB, 3 sandboxes), image `ubuntu-24.04-crowdvm-v3`, users `admin`, `bob`. Cluster setup was the e2e harness's flags.
4. **Fix checks, live** (table below).
5. **Independent sweep.** I skimmed the earlier reports for what they exercised (environments, snapshots, fork/versions, `exec`/`run`/`push`, SSH, always-on, files, desktop, integrations adapters, steering no-turn cases, OTel). Then I read all of `integrated-agents*.md`, `long-running-tasks*.md` and `webhooks.md` and ran their recipes with captured output, `--json` shapes and exit codes. I did not use Python's or TS's `doc_samples.py`; I wrote my own scripts against `boat_sdk` and the TS dist.
   - **Integrated agents:**
     - provider names and aliases (`prime`, `prime-agent`, `claude-code`, `kimi`, `mistral`, `pi`, unknown → `400 invalid_provider`), `409 provider_not_configured`, `fast_mode_not_supported` text;
     - OpenCode on the free model: `queued`/`prompt`/`prompt.status`/`response`/`done` shapes, text mode (`queued:` / `conversation:`) and JSON Lines;
     - conversation memory (`--new`, bare continue, `--resume ID`), a model switch inside one conversation (`opencode/beam-free` → `glm-5.3-flash-free`: the 4242 recall survived), `conversations` list (text and JSON);
     - parallel conversations through raw `POST /prompt`, scoped `interrupt --convo`, bare `interrupt`, prompt state via `GET /prompts/{id}` (`done`, `interrupted`);
     - `steer` with the OpenCode fallback (`native:false`, `mode:"fallback"`, the agent answered `STEERED`, the `steer` event is in `events`), `no_running_turn` with `details.reason`, `steer --convo BOGUS` → 404 `conversation_not_found`;
     - events API (`type=`, `limit`, `sort`, cursor), Boat's exact cursor loop (base64url of `<timestamp>:<id>`);
     - `ASCII_MAX_PARALLEL_CONVERSATIONS=1` as a per-sandbox `-e`: the second new conversation stays `queued`, one `opencode` process runs;
     - attachments (`--attach` repeated, names with spaces, `--image` alias);
     - per-sandbox key recipe from `integrated-agents__keys.md`: `new --no-env -e DEEPSEEK_API_KEY=sk-fake`. The sandbox gets `ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic`, `ANTHROPIC_AUTH_TOKEN`, empty `ANTHROPIC_API_KEY`, `OPENAI_BASE_URL`. `prompt --provider claude --model deepseek-flash` reaches DeepSeek and the turn ends `failed` with the vendor's 401 in the event. `--provider codex --model deepseek-v4-pro` posts to `https://api.deepseek.com/responses` and fails the same way;
     - `ai profile set-key` on a TTY prints the "get it at" link before asking;
     - after stop → resume, `--resume ID` still has the memory.
   - **Long-running work:**
     - `exec --detach` → `processId`, `--status`, `--attach`, `--kill`, `exec --timeout 0` (live output, exit code 7 passed through), `--timeout 2` → 124, `--timeout 601` accepted;
     - Boat's curl recipes: start, status with `stdoutOffset`/`stderrOffset`, `stream` frames (`attached`, `stdout`, `heartbeat`, `exit`), `DELETE` → `command.stopped` (took exactly 5.0 s on a process tree that ignores `SIGTERM`; the child `sleep` died too), `invalid_timeout` for 601 and -1, a sync timeout → `timedOut:true`;
     - `command-history`: `limit`, `mode=commands|process`, `nextCursor` paging, `invalid_limit`, `invalid_mode`, status values (`exited`, `timed_out`, `killed`, `lost`), `logs` with `stream`, `offset`, `encoding=base64`, `history ID CID [--stderr] [--follow]`;
     - `ps` (HISTORY column maps to `commandId`; stopped sandbox → `live:false` with a "last sample" line), the `process` record of a systemd service started by Boat's `ssh -- bash -s < setup.sh` recipe;
     - `extend --hours`, `--ttl`, `--ttl 99999999` (capped to 2592000), `--no-auto-stop`, `PATCH ttlSeconds` (value and `null`), `new --no-auto-stop --no-monitoring` (`monitoring_off` 409 for history and processes), fork of a no-auto-stop source → 3600 s, resume with `ttlSeconds`, deadline moves while a prompt runs (`archiveAfter` stepped by one TTL three times), a plain sandbox stopped about 5 s after its 45 s TTL;
     - a detached command running at stop shows `running` while stopped and `lost` after resume.
   - **Webhooks:**
     - CLI: `create` (`--event` repeated, comma list, short names, `sandbox.` prefix, none = all six, unknown → 400 `invalid_events`), `list`, `get`, `update`, `rotate`, `remove`, text and JSON, secret shown once;
     - API: no events → 400, bad event, `GET`/`PATCH`/`DELETE` and `404`, the 10-endpoint cap (`webhook_limit_reached`), `account.admin` required (a read-only key → `api_key_action_forbidden`), `GET /webhooks/{id}/deliveries`;
     - delivery: Boat's verbatim Python verifier against every delivery (`X-Ascii-*` headers, `X-CrowdVM-*` duplicates, `evt_<32hex>`, same id across retries), retries (attempt 1 → 2 after 10 s → 3 after 30 s), rotation;
     - events: ready, archived, error, degraded, recovered (killed my node process: `sandbox.degraded` + `sandbox.error` after about a minute with `reason:"node_offline"`; restarted: `sandbox.ready` + `sandbox.recovered`). I did not trigger a `sandbox.error` from a failed create.
   - **SDKs:** Python `boat_sdk` (`run_command`, `follow_command`, `command` detached + `command_status` loop with offsets, `stop_command`, `command_history`/`command_logs`, `processes`, all five webhook calls, `stream_prompt`). TypeScript `runCommand`, `commandStatus` loop, `createWebhook`/`updateWebhook`/`rotateWebhookSigningSecret`/`listWebhooks`/`deleteWebhook`, `commandHistory`.
6. **Free-model limit.** Around 13:34 OpenCode's free models (`beam-free`, `glm-5.3-flash-free`, `deepseek-v4.1-flash-free`) all started answering "Free usage limit reached" from upstream, so later OpenCode turns could not complete. Steering, parallel runs, memory and the model switch had already run. The SDK `stream_prompt` run therefore only got as far as `prompt`/`prompt.status running`. See m2.
7. **Teardown.** I deleted every sandbox and webhook, stopped my node, server and receiver by saved PIDs, confirmed no process with my data dir (including `qemu`) and no listener on 9970-9979, and removed `~/.local/share/review-sonnet-9`. I did not touch the other reviewer's processes (9960-9969) or run e2e. I used no sub-agents.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| sonnet-8 B1 | `env rename` updates the name on pinned sandboxes | **Fixed (live + code)** | Code: `internal/server/feat_env.go:780` (`UPDATE sandboxes SET environment=? WHERE environment_id=?`). Test: `feat_env_test.go:231-237` (rename, read sandbox, back). Live: `rn1` pinned at v4, `env rename rn1 rn2` → `info --json` `environment:"rn2"`, `environmentVersion:4`; `ls --json` `rn2`; `env list --json` `rn2` (latest 4); `env list` rows also carry `effective` now. |
| opus-10 B1 | A repository is cloned when a secret file path starts with its folder | **Fixed (live + code)** | Code: `internal/guest/setup.go:315-360`. An existing folder without `.git` gets a clone into `<folder>.crowdvm-clone`, then `mergeInto` (files already there win), then the temp dir is removed. Live (boot): environment `rn1` with repo `Spoon-Knife` and secret file `Spoon-Knife/backend/.env`: `new --environment rn1` → `* main`, `backend/.env` is `600`, content `SECRET=1`. Live (`apply-config`): `env add-repo … nocode --branch master` + `env set-file nocode/conf/.env`, then `env upgrade --sandbox` → `* master`, `conf/.env` present, `LICENSE`, `README.md`, `.git` all there. |
| Regressions | Neighbours of the two fixes | **None found** | Renamed environment still resolves for `new`, `env upgrade`, `env info`; a sandbox whose repo has no secret file clones as before; `~/.config/boat` now exists (opus-10 m1: also fixed); `/openapi/boat-v1.yaml` → 200 (opus-10 m2: also fixed). `go test ./...` passes (one load flake, above). |

## New findings

### Blocking

| ID | What | Evidence |
|---|---|---|
| **B1** | `sandbox.hydrated` is not sent after local resume or fork, and is sent (twice) when a sandbox is stopped | Boat `webhooks.md` (Events, "After a restore"). Code `lifecycle_secure.go:256` (set at ready, no event), `:292-301` (event only for a lazy chain), `snapshots.go:210-212` (event on a stored snapshot of an archiving/archived sandbox). Live: table above. Fix and tests listed above. |

### Minor (do not block AGREE)

| ID | What | Evidence |
|---|---|---|
| m1 | `--attach` and `--image` are two `StringArrayVar`s on one variable, so mixing them loses the first flag's files (`--attach "my trace.log" --image pic.png` saved only `pic.png`). Boat's own example (`--attach` twice) works; the alias is the only trigger. | `internal/cli/cmd_agent.go:147-148`. Live: `~/attachments` held one file; two `--attach` held both. |
| m2 | When OpenCode hits a terminal provider error ("Free usage limit reached"), the CLI prints nothing, the turn stays `running`, and no failure event is written; the error only reaches `~/.local/share/opencode/log/opencode.log`. Claude Code's and Codex's vendor errors do end the turn `failed` with the reason. Boat's lifecycle says "failed: harness or credential error (reason in the event)". I cannot tell whether Boat's own OpenCode adapter differs, because the free quota ran out upstream. | Live: `opencode run --format json` prints nothing for 25 s. `cv conversations` shows `running:true` for 10+ minutes. |
| m3 | A repository whose clone failed on a live sandbox (wrong branch) is never retried by a later `env upgrade`, because the guest already lists it as a known repo (`internal/guest/apply.go:303`). A new sandbox clones it fine. | Live: `Hello-World` added with the default `main` (it has only `master`), later fixed with `--branch master`: `env upgrade` left `~/Hello-World/` without `.git`. |
| m4 | A detached command that was running when its sandbox stopped shows `running` in `history` until the next resume (then `lost`). Boat has a `lost` status for exactly this. | Live (`cmd_872gh8b57xtv`). |
| m5 | `crowdvm conversations` prints the model twice (`opencode/opencode/beam-free`). `GET /events?limit=101` and `?sort=up` are accepted without error. Cosmetic. | `internal/cli/cmd_agent.go:412`. Live. |
| m6 | `TestGuestHealth` (`internal/node`) is load-sensitive: failed once under three concurrent clusters, passes alone. | `go test` output. |

## Disputed classifications

- **HOOK-013 / LIFE-012** (`PARITY.md:580`, `:84`; `docs/gaps/B.md:209` "DONE – hydrated on snapshot stored"): I do not accept the adaptation. The ADAPTED note says "`hydrated` fires when chain is local". On a stop it fires although no restore happened, and on a local restore the chain is local at `ready` yet it never fires. See B1. LIFE-012's "immediate when resumed on the node that holds it" should mean "emitted immediately", not "no event".
- Everything else I touched in `PARITY.md` (HOOK-001 to HOOK-012, AGT-025/041, long-running rows, KEY rows for per-sandbox keys) holds as written. HOOK-004 is ADAPTED and its text matches behaviour (loopback and metadata hosts refused without `--webhook-allow-local`; I ran with it so my receiver could listen on 127.0.0.1).

## What would flip this to AGREE

1. **B1.** After `resume` and after `fork` (also for a stopped source) of a sandbox on a node that already has the chain, an account webhook subscribed to `sandbox.hydrated` receives exactly one `sandbox.hydrated` event (`data.sandbox`, no `previousState`/`state`), after or together with `sandbox.ready`, and `stop` produces no `sandbox.hydrated`. Add a server test next to `feat_webhooks_test.go:106` and update the e2e step in `docs/E2E.md:73`.
2. Nothing else blocks. I would take m1 (a one-line alias fix) and m2 (turn the harness's terminal error into a `failed` turn) in the same pass.

## Checked and fine

- **Agents:** provider routing and aliases, credential gate, fast-mode refusal, per-sandbox DeepSeek key through to the vendor for Claude Code and Codex, conversation memory across a model switch and across stop/resume, parallel conversations and the per-sandbox cap, scoped and bare interrupt, steer fallback (`native:false`, `mode:fallback`, `steer` event), steer/interrupt/prompt error codes, Boat's cursor loop, text and JSON output shapes, attachments.
- **Long-running work:** every `exec` detached/status/attach/kill/timeout path in `long-running-tasks__background-commands.md` (including the 5 s `SIGKILL` and the process-group kill), command history and logs with paging, `base64`, `process` records for a systemd service, `ps` live and stopped, TTL semantics on create, `extend`, `PATCH`, `fork`, `resume`, the cap at 30 days, deadline moves for a running prompt, stop within about 5 s of the deadline, `--no-monitoring`.
- **Webhooks:** the whole CLI/API/SDK surface of `webhooks.md` except the one event above; signatures verified with Boat's own verifier; retries with backoff and stable ids; secret rotation; limit of 10; `account.admin` gate; a degraded/recovered cycle from a real node loss.
- **SDKs:** Python and TypeScript samples for `run_command`/`runCommand`, `follow_command`, `command_status` offset loops, `stop_command`, history/logs, webhooks.
- **Opus-10 and sonnet-8 leftovers:** `~/.config/boat` exists, `/openapi/boat-v1.yaml` is served.

## Not verified

- Real Claude/Codex/ChatGPT/Bedrock/Mistral credentials (only the DeepSeek key path to the vendor's 401), `pi`/`prime`/`kimi`/`mistral` turns (all answered `provider_not_configured`), OpenCode beyond the point its free quota ran out, SDK `stream_events` against a live turn.
- A failed-create `sandbox.error` webhook, delivery exhaustion after 8 attempts (I saw attempts 1-3), the 5 s receiver timeout (my single-threaded receiver made the evidence ambiguous), docker-container `process` history records, the dashboard Deliveries view.
- Windows nodes, GPU, SMTP, a second node (cross-node resume would exercise the lazy-chain `hydrated` path that does emit the event).
- e2e run 9 (still in progress).

## TL;DR

DISAGREE, narrowly. Both fixes hold live: `env rename` now renames the environment on pinned sandboxes (`feat_env.go:780`), and a secret file inside a repository folder no longer blocks the clone, at boot and on `env upgrade` (`guest/setup.go:315-360`). Integrated agents, long-running work and webhooks match Boat almost everywhere: parallel conversations, scoped interrupt, steer fallback, per-sandbox keys, detached commands, history/logs, auto-stop and TTL rules, signed webhooks, node-loss degraded/recovered. One blocker: `sandbox.hydrated` is never sent after a local resume or fork and is sent twice on stop (`lifecycle_secure.go:256`, `snapshots.go:210-212`), contrary to `webhooks.md`. Fix that, with a test, and I would agree.
