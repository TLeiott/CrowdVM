# Review sonnet-4: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (narrowly: three small, cheap-to-fix blockers)

Every claimed fix from opus-3, opus-4 and opus-5 holds. I reproduced the opus-5 blocker fix live: the eBPF output tracer records ssh sessions, tty sessions, backgrounded pipe writers and an agent's tool process. I found no regression from the new changes.

My own sweep found supportable agent-facing features that are still wrong, and I reproduced three of them. All three are client-side or CLI-side, a few lines each, and none is blocked by the LAN, self-hosted, no-payments or no-OAuth constraints.

1. **B1. `crowdvm info --json` has a different shape from Boat's.** Eight Boat doc pages poll `boat info ID --json | jq -r .sandbox.state` until it equals `stopped` (also `.sandbox.hydrated` and `.sandbox.setupStatus`). On CrowdVM the output is flat with no `sandbox` wrapper, and a stopped sandbox reads `archived`. Those loops never end.
2. **B2. `crowdvm fork --no-snapshots` is silently ignored.** The fork keeps taking snapshots. Boat documents the flag at `cli-reference.md:598,613`.
3. **B3. The TypeScript SDK turns Boat's `events: new Set([...])` into `{}`.** The webhook create and update samples in `webhooks.md:51,113` fail with `400 invalid_json`.

## Method

1. **Read** opus-4, opus-5 and the opus-3/sonnet-3 fix tables, the CMD-021 rows in `docs/PARITY.md` and `docs/gaps/A.md`.
2. **Build and tests.**
   - I built all four binaries into my scratchpad with the Makefile's flags, not `make build`, so the running e2e's `bin/` was left alone.
   - `go vet ./...` is clean. `go test` on every package except `test/e2e` passes.
   - I did not run e2e. I read `.../232e4b2f.../scratchpad/e2e-run4.log`. It ends `--- PASS: TestE2E (646.24s)` and `EXIT=0`, with no `--- FAIL`. `disk-cleanup` and `process-history` both passed, and the latter logged "agent-started process recorded".
3. **Own cluster**, all data under `~/.local/share/review-sonnet-4-x7q`.
   - `crowdvm-server` on `127.0.0.1:9947`, one `crowdvm-node` (qemu driver, KVM, UI on `:9948`, 6 vCPU / 12 GiB / 3 sandboxes), image `ubuntu-24.04-crowdvm-v3`.
   - **Collision:** my first attempt used port 9930. Another reviewer's node joined my server there, so I stopped and deleted that server and node at once and restarted on a fresh port with a fresh data dir.
   - **Teardown:** I deleted my sandboxes, stopped my node and server by PID, and deleted the data dir. No process of mine is left, ports 9947/9948 are free, and no QEMU of mine is running.
4. **Route sweep.** I sent a request to each of the 122 operations in `boat-v1.yaml` against my server. None answered 501 or "Unknown API route".
5. **CLI sweep.** I ran a script over every `### boat ...` section of `cli-reference.md` and checked the flags against `crowdvm ... --help`.
   - The only misses were subcommand flags that sit on a deeper command, or Boat-only names that exist as hidden aliases (`--boat-credentials`).
   - The N/A flags are `--agentid` and `--google`.
6. **Two read-only Opus sub-reviewers**, no cluster, no repo edits. They drove an in-process server through `go test -overlay`.
   - **(A)** the SDK, MCP, skill, integration, agent and platform pages.
   - **(B)** `cli-reference` (every command), machines, hosting, ssh, snapshots, data retention, environments and organizations, plus the request/response schemas of the spec.
   - I re-verified every finding I kept, live where marked.
7. **Live checks** on my cluster. Listed below.

## Fix status

F = fixed. "live" = I reproduced the fix on my own node.

| # | Finding | Status | Evidence |
|---|---|---|---|
| opus-5 N1 | Process history misses ssh sessions and agent-started processes (CMD-021) | **F (live)** | See "N1 in detail" below the table. |
| opus-5 m-1 | `PromptEvent.data.attachments` unfilled, "Attached files" written twice | **F (code)** | `internal/guest/agents/http.go:166` fills `data["attachments"]` (`eventAttachments`, `:526`, inlines up to 256 KiB as base64). The CLI no longer writes the list itself: `cmd_agent.go:105` only appends to `req.Attachments`. The only remaining "Attached files" text is the guest's (`agents/run.go:333`). |
| opus-5 m-3 | TS SDK can't be `require`d on Node 18 to 20.18 | **F** | `sdk/typescript/package.json:7-9` has `engines.node >=20.19`, where `require(esm)` works. |
| opus-5 m-4 | No "delete all snapshots" in the dashboard | **F (code)** | `web/js/tabs/snapshots.js:15,47` adds a "Delete all…" button with a type-to-confirm dialog. |
| opus-5 m-5 | Harbor `BoxEnvironment` ignores `BOX_API_KEY` | **F (code)** | `integrations/harbor/harbor_crowdvm_environment.py:88-90` falls back to `BOX_API_KEY` and `BOX_API_BASE`. The module-name gap is m-3 below. |
| opus-5 m-8 | `runs-on: boat` not accepted | **F (code)** | `internal/server/feat_ci.go:341-342` maps `boat`, `boat-…` and `boat/…` to the `crowdvm` family. Test at `feat_ci_test.go:332`. |
| opus-4 B1 / opus-3 N6 | Low-disk cleanup deletes images built in the sandbox | **F (holds)** | opus-5 reproduced it live. `internal/guest/docker.go` has not changed since. e2e `disk-cleanup` passed in run 4 (35.72 s). |
| opus-4 B2 | Scoped-key `GET /sandboxes` paginates before filtering | **F (holds)** | opus-5 live. Unit test passes. |
| opus-4 m1 | `billing extra-starts on --yes` | **F (holds)** | `internal/cli/cmd_billing.go:144`. |
| opus-3 N1–N5, N9; sonnet-3 N1–N3 | Deletion ops, `bx_` alphabet, `run --personal`, Bedrock IAM, `org migrate`, `org members`, `/healthz`, `bsh_`, steer code | **F (holds)** | `/healthz` live: `{"ok":true,"version":"dev"}`. Every id I minted conforms (`bx_g3at8szn`, `bx_32tw2s2e`). The rest were re-read in code, unchanged since opus-5. |

### N1 in detail

**Code.**
- `internal/guest/bpf/writer.c` hooks `write(2)` and `writev(2)`, accepts only tty, pipe and socket inodes, and applies a per-process token bucket (64 KiB/s, 1 MiB burst).
- `internal/guest/tracer_bpf.go` loads it with `cilium/ebpf`. The generated Go and `.o` files are checked in, so a normal build needs no clang.
- `procClass` (`tracer_bpf.go:262-340`) records:
  - all writes from an ssh session (the logind scope of the `sshd` service, or a descendant of `sshd`);
  - all writes from descendants of an integrated-agent harness, in the `agents` workload cgroup;
  - only tty writes from other login sessions;
  - nothing from the guest, exec'd commands (CMD-022) or Docker/systemd services (their own followers).
- Without BPF it logs once and keeps journald/Docker.
- 194 lines of unit tests cover the classifier (`tracer_bpf_test.go`).

**Live, on `bx_g3at8szn`.**
- `ssh ID -- 'for i in 1 2 3; do echo hello-from-ssh-$i; done; (setsid sh -c "… echo piped-tick-$i … | tee /dev/null >/dev/null" &)'` and `ssh -t ID -- 'echo tty-session-output-xyz'`. Within 45 s, `history --mode process` held three records for the ssh command, the backgrounded pipe writer, and the tty session, plus the journald `xfce4-notifyd`. `history ID <commandId>` returned the stored text for each (`hello-from-ssh-1..3`, `piped-tick-1..6`).
- **Agent step:** `prompt --provider opencode --model opencode/beam-free --new "Use your bash tool to run exactly this command…"`. It finished. The process history then held `/bin/bash -c echo agent-out-25851` (16 bytes).
- **No duplicates:** an `exec`'d `bash -c 'echo exec-out-zzz; (echo bg-from-exec | cat)'` produced one `sync` record and no `process` record.
- **Stress:** `yes | head -c 300000000 | cat >/dev/null` from ssh produced one record per pipeline stage, `outputTruncated:true` at about 1 MB. The sandbox stayed `ready`/`ok`.
- **Stop and resume:** after both, a new ssh command was captured again and the old records were still there.

## Blocking findings

### B1. `crowdvm info --json` is not Boat's shape, so Boat's polling loops never end

- **Boat:** eight pages, with the same loop in each:
  - `until [ "$(boat info ID --json | jq -r .sandbox.state)" = "stopped" ]; do sleep 2; done` at `machines__change-size.md:38`, `pricing.md:36`, `snapshots__stop.md:60`, `faq.md:163`, `troubleshooting.md:110`;
  - `.sandbox.hydrated` at `readiness.md:82,145`;
  - `.sandbox.setupStatus` and `.sandbox.setupError` at `setup.md:103,106`.
- **CrowdVM:** `internal/cli/cmd_sandbox.go:344` prints the sandbox object directly (`a.result(s, …)`). Live:
  ```
  crowdvm info ID --json | jq -r '.sandbox.state, .state'   →  null, ready
  ```
  The flat object has no `sandbox` key. In `--json` the state of a stopped sandbox is the API's `archived`, not the CLI's `stopped`, which the text mode prints (`displayState`). Because `--json` turns on by itself when piped, every one of Boat's loops runs forever.
- **Why supportable:** purely a CLI output choice. Wrap the object (`{"sandbox": {…, "state": "<display state>"}}`) and keep the flat fields for existing scripts.
- **Evidence strength:** reproduced live. Sub-reviewer B also reproduced it independently.

### B2. `crowdvm fork --no-snapshots` is silently ignored

- **Boat:** `cli-reference.md:598` (`boat fork --no-snapshots --no-monitoring ID`) and `:613` ("boat never snapshots the fork. Stopping the fork erases it").
- **CrowdVM:** the CLI sends `snapshots:false` (`internal/cli/cmd_sandbox.go:652-654`, `internal/client/types.go:180`). The server's fork request struct has no `snapshots` field (`internal/server/lifecycle.go:1260-1271`), and the fork policy is fixed to `Snapshots: true` (`lifecycle.go:1389`). Create handles the same field correctly (`sandboxes.go:915-921`).
- **Live:** `crowdvm fork ID --no-snapshots --type small --json` → created `bx_32tw2s2e` with `sandbox.snapshots: true`.
- **Why it matters:** the flag exists for privacy. A user or agent who asks for no snapshots gets them anyway, and nothing says so.
- **Why supportable:** server logic. Add the field and let the existing snapshots-off path (`sandboxes.go`) apply.

### B3. TypeScript SDK serialises `Set` as `{}`

- **Boat:** `webhooks.md:51` (`createWebhook({ name, url, events: new Set([...]) })`) and `:113` (`updateWebhook({ webhookId, events: new Set(["sandbox.error"]) })`). The spec marks `events` as `uniqueItems`, so Boat's generated client takes a `Set`.
- **CrowdVM:** `sdk/typescript/src/client.ts:155` does `JSON.stringify(init.body)`, which turns a `Set` into `{}`. The server answers `400 invalid_json … cannot unmarshal object into … events of type []string`. Sub-reviewer A reproduced it. I confirmed the `JSON.stringify` line and the doc samples.
- **Why supportable:** client-side only. A `replacer` that maps `Set` to an array fixes it.

## Non-blocking findings

| ID | What | Evidence |
|---|---|---|
| m-1 | The Python `boat_sdk.exceptions` aliases every specific exception to the one class: `NotFoundException = ConflictException = ServiceException = … = ApiError`. So `except NotFoundException:` catches a 400 or a 409. Boat's generated SDK has real subclasses. No Boat doc sample uses them (they use `ApiException`). The `ApiTypeError`, `ApiValueError`, `ApiKeyError` and `ApiAttributeError` classes are missing too. | `sdk/python/src/boat_sdk/exceptions.py:4-6` (read; sub-reviewer A ran it). `crowdvm/errors.py` already has per-status classes to map to. |
| m-2 | `new` accepts bad or duplicate `hostedPorts` entries. The spec says 400 `invalid_hosted_ports`. CrowdVM creates the sandbox and reports per-port errors. | `internal/server/sandboxes.go:811` checks only the count. Entries are checked after the insert at `feat_host.go:461-485`. The test at `feat_host_test.go:299` encodes this behaviour. Sub-reviewer B ran it. |
| m-3 | Boat's `harbor run -e harbor_box_environment:BoxEnvironment` cannot import: only `harbor_crowdvm_environment` ships. The class alias exists, the module name doesn't. | `integrations/harbor/pyproject.toml:14`. Boat: `evals.md:105`, `integrations__harbor.md:39,64`. A one-line re-export module fixes it. |
| m-4 | `crowdvm snapshots` lists only automatic snapshots. Boat's `snapshots__templates.md:34,202` says it lists saved snapshots first and shows the allowance. `crowdvm snapshot list` has the saved ones. | `internal/cli/cmd_snapshot.go:14-63` (live: I saw only automatic rows after saving `mysaved`). |
| m-5 | The CLI reads only `CROWDVM_*` environment variables for its own settings. Boat's `BOAT_ORG` and `BOAT_API_URL` are ignored; only `BOAT_NO_UPDATE` works. A Boat-style `BOAT_ORG=acme crowdvm …` runs against the wrong wallet without a word. The rename is documented in `docs/guide/migrating-from-boat.md:27-31`, but accepting `BOAT_*` as fallbacks is cheap. | `internal/cli/app.go:407`. Live: `BOAT_ORG=nonexistent-org` is ignored, `CROWDVM_ORG=nonexistent-org` errors. |
| m-6 | `SnapshotSummary.origin` is always `captured`, and `fileCount`/`contentSizeBytes`/`contentFileCount` are always `null`. The spec allows null for the counts, but a snapshots-off sandbox should list its source's snapshots with `origin:"template"`. | `internal/server/snapshots.go:390-392`. |
| m-7 | `/me` `user.accountStatus` is never `closed` (the spec enum has it). | `internal/server/auth.go:649-660`; `feat_retention.go:282-287`. |
| m-8 | Carried over from opus-5: a shrink with `resume --type` or `fork --type` keeps the larger disk. | `internal/node/vm/qemu/qemu.go:332-339`. Live: a resume from small to default gave 4 vCPU / 8 GB, and `df -h /` still showed the old size. |
| m-9 | Carried over from opus-5: `PromptEvent.data.citations` and `/me` `closureContactEmail` are never produced; `chrome_profile_unsupported` is never returned. Spec-only. | |

## Live checks that passed

- `crowdvm scp -r` upload and download of a nested directory; `exec --detach` plus `host 8088`, which returned the token URL, `hostUrl` and `pathUrl`.
- `stop`, then `resume --type default` from a small sandbox: 4 vCPU / 8 GB. `resume --type large` on a node that cannot hold it returned `out_of_capacity`. A resume that cannot get capacity stays `provisioning` until room appears, then completes (Boat's behaviour).
- In the sandbox: `boat` and `crowdvm` in `/usr/local/bin`, the `crowdvm` skill under `~/.claude/skills` and `~/.codex/skills`, and `CROWDVM_*` plus `BOAT_ID` set. The docs MCP answers `tools/list`, and `/llms.txt` serves.
- `prompt --provider opencode …` with a free model: queued, ran a tool, `done`/`finished`.
- API-key creation, including `--json | jq .secret`.

The sub-reviewers also reported these as fine:
- the Python method table, and the Boat doc samples run against an in-process server;
- the CLI JSON shapes of `new`, `fork`, `list`, `run`, `exec --detach`, `status`, `host`, `desktop`, `login`, `api-key create` and `delete`;
- agent auth (`auth.md`, `.well-known`, `WWW-Authenticate`);
- the docs MCP, `llms-full.txt` and install scripts;
- API key scopes;
- steer modes and `no_running_turn`;
- create, resume, fork and PATCH validation;
- idempotency and deletion operations;
- the AgentEnv, Eve and Harbor adapters.

## Disputed classifications

- **CMD-021 (`docs/PARITY.md:173`).** The row is still worded as the old journald-only state. The code now matches the REQUIRED text. Update the row to DONE, and keep the note on ADAPTED for BPF-less kernels.
- **CLI `info` rows in PARITY.** The `info --json` shape is not Boat's (B1), so any row that claims script parity for `info` needs a note.
- **ADAPTED rows.** No new objection. I accept the same ones opus-4 and opus-5 accepted, including SSH-004/007/008 and URL-012, which depend on the node owner's `--direct-host`.
- **N/A rows.** I did not re-read them. Neither sub-reviewer raised any in scope.

## What would flip this to AGREE

1. **B1:** make `crowdvm info --json` emit `{"sandbox": {…}}` with the CLI state label (`stopped`), keeping the flat fields. Add a unit or e2e step that runs `info --json | jq -r .sandbox.state` on a stopped sandbox.
2. **B2:** accept `snapshots:false` in the fork request and honour it (a stop then erases the fork). Add a test.
3. **B3:** serialise `Set` as an array in the TypeScript client. Add a test with `events: new Set([...])`.

m-1 to m-9 are nice to have. They do not block AGREE.

## Not verified

- Windows nodes and VFIO GPU passthrough.
- Real Claude, Codex or Bedrock credentials.
- Real GitHub or Gitea CI, and SMTP.
- A `--direct-host` node.
- Low-disk cleanup live. I relied on opus-5's live run, the unchanged code and the passing e2e group.
- The dashboard beyond a code read of the new button.
- Tracer cost on a loaded node. I ran one stress pass only. Performance is not a parity finding.

## TL;DR

DISAGREE, narrowly. All earlier blockers are fixed. The eBPF tracer works live: ssh, tty, backgrounded pipe writers and an agent's tool process all produce `process` history, and exec'd commands are not duplicated. Three small supportable gaps remain:
- **B1:** `info --json` is not Boat's `.sandbox.*` shape, so Boat's polling loops never finish.
- **B2:** `fork --no-snapshots` is ignored by the server.
- **B3:** the TypeScript SDK sends a `Set` as `{}` and the webhook samples fail with 400.
