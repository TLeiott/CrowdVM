# Review opus-5: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, round 4). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (narrowly, one blocker)

Every opus-4 item the team claimed is fixed. I reproduced B1 and B2 on my own KVM node and both now behave correctly. m1 works live, and the CLI half of m2 is in the code. The opus-3 and sonnet-3 fixes still hold. Unit tests pass, and the other session's full e2e run ended `EXIT=0`, including the new `disk-cleanup` group.

One supportable REQUIRED feature that no earlier review exercised is only partly implemented, and I reproduced the gap live:

- **N1. Process output capture (CMD-021) misses ssh sessions and processes that agents or shells start.** Boat records "any other process that writes output: an `ssh` session, a systemd service, a docker container, a process that an agent starts" (`long-running-tasks__command-history.md:14`). CrowdVM follows only the systemd journal and Docker logs (`internal/guest/tracer.go:19-29`). Output from an ssh session, a `ssh -t` session, and a backgrounded pipe writer produced no `process` record. The gap tracker calls this ADAPTED, but no constraint applies: the shipped guest kernel has BTF, `bpftrace` is in the image, and I ran an eBPF `write(2)` trace in a sandbox.

Everything else I found is minor (listed below).

## Method

1. **Read** opus-4, the fix-status rows of opus-3 and sonnet-3, `docs/gaps/A.md` (CMD-021 rows), and the PARITY rows each finding touches.
2. **Build and tests.**
   - I built every binary into my scratchpad with the Makefile's `go build` flags, not `make build`, so the running e2e's `bin/` was left alone. Clean.
   - `go vet ./...` is clean, and `go test` on every package except `test/e2e` passes.
   - I did not run e2e. I read `…/232e4b2f…/scratchpad/e2e-run3.log`. It ends `--- PASS: TestE2E (638.62s)` and `EXIT=0`. `disk-cleanup` passed in 34.69 s. Every group passed except `bootloop`, which was skipped.
3. **Code check** of B1, B2, m1 and m2, and a re-check of opus-3 N1–N6/N9 and sonnet-3 N1–N3, with file:line references below.
4. **Own cluster**, all data under `~/.local/share/review-opus-5-crowdvm`.
   - `crowdvm-server` on `127.0.0.1:9910` (`--webhook-allow-local`).
   - One `crowdvm-node`: qemu driver, KVM, UI on `:9911`, 6 vCPU / 10 GiB / 3 sandboxes.
   - Image `ubuntu-24.04-crowdvm-v3`, imported and pushed.
   - Driven with the `crowdvm` CLI and curl against `/api/v1`.
   - Teardown: I deleted the sandbox and stopped the node. The node leaves running VMs alive by design (`internal/node/vm/qemu/qemu.go:9`) and reaps leftover inspector VMs on its next start (`internal/node/node.go:230`). Because I would not restart it, I stopped its two snapshot-inspector QEMU processes myself, then the server, and deleted the data dir. Afterwards no process of mine was left and ports 9910/9911 were free.
5. **Independent sweep** with two read-only Opus sub-reviewers, aimed at the pages earlier reviews cited least.
   - **(A)** integrations (AgentEnv, Eve, Harbor), evals, recipes, the four platform-guide pages, the three SDK pages, all data-retention pages, and integrated-agents customize, computer-use and conversations. It ran all 56 Python/TypeScript doc samples against the fake server and matched the Python and TypeScript method tables.
   - **(B)** desktop-streaming, machines, hosting, environments versions/access/choose, CI with its API pages, OpenTelemetry, webhooks, the long-running-tasks pages, SSH, editors and limits, checked against the routes and boat-v1.yaml operations.
   - I re-checked every finding I kept.
6. **Live spot checks** of REQUIRED rows no earlier review exercised (listed below).

## Fix status

F = fixed. "live" = I reproduced the fix on my own node.

| # | Finding | Status | Evidence |
|---|---|---|---|
| opus-4 B1 / opus-3 N6 | Low-disk cleanup deletes images built in the sandbox | **F (live)** | See "B1 in detail" below the table. |
| opus-4 B2 | Scoped-key `GET /sandboxes` paginates before filtering | **F (live)** | See "B2 in detail" below the table. |
| opus-4 m1 | `billing extra-starts on --yes` rejected | **F (live)** | `internal/cli/cmd_billing.go:144` adds `--yes` as a no-op. Live: `extra-starts on --yes` and `off --yes` both returned `billing.extra_starts`. |
| opus-4 m2 (CLI half) | `prompt --attach` doesn't send `attachments` | **F (code)** | `internal/cli/cmd_agent.go:105` appends each uploaded path to `req.Attachments`, sent as `attachments` (`internal/client/types.go:490`). The server forwards the raw body (`acct_prompt.go:104-128`). The guest checks the files and hands them to harnesses with native attachments (`guest/agents/http.go:116,139`, `rpc.go:219`, `pi.go:47`, `acp.go:95`). The event half is still open: `PromptEvent.data.attachments` is never filled (`guest/agents/http.go:160-162`). Non-blocking, see m-1. |
| opus-3 N1 | Snapshot deletion ops never complete | **F (holds)** | `internal/server/deletion_ops.go:65-67`. |
| opus-3 N2 | `bx_` id alphabet | **F (holds)** | `internal/api/api.go:133-134`. Every id I minted conforms (`bx_nvrkq2zb`, `bx_sk6kep2h`, `bx_zbqjnkbm`). |
| opus-3 N3 | `run --personal` | **F (holds)** | `internal/cli/cmd_exec.go:380-382`. |
| opus-3 N4 | Bedrock IAM keys | **F (holds)** | `internal/server/feat_ai.go:1514-1516`. |
| opus-3 N5 | `org migrate` visibility | **F (holds)** | `internal/server/acct_misc.go:242-245`. |
| opus-3 N9 | `org members` pending invites | **F (holds)** | `internal/cli/cmd_admin.go:488-499`. |
| sonnet-3 N1 | `/healthz` | **F (live)** | `internal/server/routes.go:23`. Live: `{"ok":true,"version":"dev"}`. |
| sonnet-3 N2 | `bsh_` share codes | **F (holds)** | `internal/api/api.go:127-129`. |
| sonnet-3 N3 | Steer on a stopped sandbox | **F (holds)** | `internal/server/passthrough.go:207-209`. |

### B1 in detail

**Code.**
- `internal/guest/docker.go:244-261` (`pulledFilter`) detects the containerd image store from `docker info` `DriverStatus`.
- On that store an image counts as pulled only when `ctr -n moby content ls` shows a `containerd.io/distribution.source.*` label for its id (`registrySourced`, `:265-273`).
- If `ctr` fails, nothing is deleted (`:254-257`).
- The unit test uses containerd-store output (`docker_test.go:122-128`).

**Live, on `bx_nvrkq2zb` (Docker 29.9.3, `io.containerd.snapshotter.v1`).** The content store showed the label on the pulled `alpine:3.20` and `busybox:1.36` digests and not on the built `mybuilt:1`. I ran a `keepme` container on busybox and filled `/` to 97% with `fallocate`. The server logged `kind=disk_usage_95`. Within about 10 s the images were `busybox:1.36 mybuilt:1`: the unused pulled alpine was deleted, and the built image and the in-use image were kept.

**e2e.** `disk-cleanup` (`test/e2e/review_test.go:222-248`) passed in run 3.

### B2 in detail

**Code.**
- When the caller is an API key, `internal/server/sandboxes.go:543-547` removes the SQL `LIMIT`.
- `:553-563` reads rows until `limit+1` rows pass `AllowsSandbox`.
- `PageResult` then sets `hasMore` and the cursor from a full page.
- Test at `internal/server/apikeys_test.go:78-81`.

**Live.** I used key `pg3` (`--sandbox bx_nvrkq2zb --preset read-only`), with two newer sandboxes (`bx_sk6kep2h`, `bx_zbqjnkbm`) that the key may not see. `limit=1`, `limit=2` and `limit=100` each returned `[bx_nvrkq2zb]` with `hasMore:false`. Before the fix, `limit=1` returned `[]`.

## Blocking finding

### N1. Process output capture covers only journald and Docker, not ssh sessions or agent-started processes (CMD-021)

**What Boat documents** (`long-running-tasks__command-history.md`):
- `:14`: the `process` mode records "Any other process that writes output: an `ssh` session, a systemd service, a docker container, a process that an agent starts".
- `:89`: "boat records what each process writes to its terminal, to a pipe, or to a socket such as the journal. You do not change your programs for this."
- `boat history ID --mode process` and `GET /command-history?mode=process` are how you read it.

**What CrowdVM does.** `internal/guest/tracer.go:19-29` follows `journalctl -f` and Docker's json-file logs only. Its own comment says "Output written only to a terminal (an interactive ssh session) or to files is not captured." That leaves out:
- ssh sessions;
- a dev server an agent's shell tool started with `&`;
- anything writing to a pipe or tty outside systemd and Docker.

These are two of Boat's four named sources.

**Live, on `bx_nvrkq2zb`.** I ran three things:
- `crowdvm ssh ID -- 'for i in 1 2 3; do echo hello-from-ssh-$i; done; (setsid sh -c "… echo piped-tick-$i …| tee /dev/null >/dev/null" &)'`, which writes from an ssh session and starts a backgrounded pipe writer for 30 s;
- `crowdvm ssh -t ID -- 'echo tty-session-output-xyz'`.

More than 60 s later, past Boat's "about 30 seconds", `GET /command-history?mode=process` held one record: `xfce4-notifyd`, which came from the journal. There was no record for the ssh session, the tty session or the pipe writer.

**Classification.**
- PARITY CMD-021 (`docs/PARITY.md:173`) is still **REQUIRED**, with the plan "Guest eBPF tracer on write(2) to tty/pipe/socket fds (root in VM). Hard."
- `docs/gaps/A.md:232` changed it to "FIXED, ADAPTED (journald follower instead of eBPF …; tty-only output not captured)".
- That ADAPTED rests on implementation effort. None of the stated constraints applies: not LAN-only, not self-hosted, not payments, not OAuth, not one machine.

**Why supportable on a LAN.** It is all guest-side, in a VM where the agent is root. In the shipped image:
- the kernel is `6.8.0-147-generic` with `CONFIG_DEBUG_INFO_BTF=y` and `/sys/kernel/btf/vmlinux` present;
- `/usr/bin/bpftrace` is installed;
- live, `bpftrace -e 'tracepoint:syscalls:sys_enter_write …'` in the sandbox counted the writes of a backgrounded `bash | cat` pipeline (`@w[bash, 1]: 3`, `@w[cat, 1]: 3`).

**Fix.** Use a CO-RE program (for example `cilium/ebpf`) or a supervised `bpftrace` on `sys_enter_write` that:
- keeps only fds that are a tty, a pipe or a socket;
- excludes the guest's own process tree and exec'd command sessions (the cgroup/session check at `tracer.go:160` already exists);
- feeds the existing per-process records, rate limiter and 30 s export.

The journald follower can stay as a fallback when BPF is unavailable.

## Non-blocking findings

| ID | What | Evidence |
|---|---|---|
| m-1 | `PromptEvent.data.attachments` is never filled (the event half of opus-4 m2). `prompt --attach` also writes "Attached files" into the prompt text twice: the CLI adds it at `internal/cli/cmd_agent.go:109`, and the guest adds it again at `internal/guest/agents/run.go:332-333`. | Spec: `boat-v1.yaml:2174-2181`. |
| m-2 | `PromptEvent.data.citations` and `/me` `user.closureContactEmail` are still never produced. Carried over from opus-4 m3. These are spec-only fields. | |
| m-3 | The TypeScript SDK can't be `require`d on Node 18 through 20.18. Boat's TS page shows CommonJS `require`, and the SDK declares `engines: node >=18`. The package is `"type": "module"` with only a `default` (ESM) export condition. | `sdk/typescript/package.json`. Fix: add a CJS build and a `require` condition, or raise `engines` to `>=20.19`. |
| m-4 | The dashboard has no "select all, then delete" for a sandbox's automatic snapshots. Boat lists it in `data-retention.md:56`. The CLI (`snapshot delete --all`) and the API already do it. | `web/js/tabs/snapshots.js:40-43` deletes one row at a time. |
| m-5 | The Harbor `BoxEnvironment` alias ignores `BOX_API_KEY` (`integrations__harbor.md:124`). The Eve adapter already falls back to it. | `integrations/harbor/harbor_crowdvm_environment.py:77-88`. |
| m-6 | `integrations/agentenv/README.md` tells you to patch AgentEnv's prefix check. The server already accepts a `boat_` key prefix (`internal/server/auth.go:241-246`). | Docs only. |
| m-7 | Shrinking a sandbox with `resume --type` or `fork --type` keeps the larger disk. `df -h /` keeps showing the old size, while the scheduler reserves the new one. `type_too_small` is enforced correctly. | `internal/node/vm/qemu/qemu.go:332-339`. Boat: `machines__change-size.md`. |
| m-8 | `runs-on: boat` is not accepted, only the `crowdvm*` label family, so Boat workflow files need a one-word edit. The in-sandbox `boat` command and `boat_` key aliases already exist, so accepting `boat` as a second prefix would be cheap. | `internal/server/feat_ci.go:63`. Boat: `ci.md:12`. |
| m-9 | `chrome_profile_unsupported` is never returned. On an image without the `crowdvm-browser` user, Chrome runs unisolated (`isolated:false`) instead. This only matters for old images. | `internal/guest/browser.go:180-181`. |

Side effect to report: sub-reviewer A ran `integrations/run_tests.sh`. Its `npm test` reran `tsc`, which rewrote `sdk/typescript/dist` and the Eve package's build output. These are untracked build artifacts and the rebuild should be identical, but they are not source files I meant to touch.

## Live spot checks of REQUIRED rows no earlier review exercised (all passed)

- **SIZE-011 `failFast`.** `POST /sandboxes {"type":"xlarge","failFast":true}` on a full node returned `503 no_ready_machine` at once.
- **ENV-024.**
  - `env:{"1BAD":"x"}` → `400 invalid_env`.
  - `PATH` is accepted, which is correct: Boat reserves only its internal names.
- **LIFE-048.** `POST /commands` with `env` → `400 invalid_request`, with the "prefix the command" hint.
- **CMD-005.** A `stream:true` command gave NDJSON `started`, `stdout`, `stderr`, then an `exit` frame with `exitCode:3` and the full result fields.
- **FILE-015.** `GET /artifacts?path=` returned a 1000-byte random file with the same md5.
- **DATA-007.** `Cache-Control: no-store` is set on `/account/data-retention` and `/deletion-operations/{id}`.
- **SNAP-029.** On the next periodic snapshot:
  - `GET /snapshots/{id}/files?path=sf.txt` → `snapfile-abc`;
  - `?path=/etc/hostname` → `b1`;
  - `?path=__dockervol__/vv/v.txt` → `volfile`, a file inside a Docker volume;
  - `/tree` lists the home dir.
- **LIFE-008.**
  - After `resume`, a command sent during `provisioning` was held and then ran (`held-ok`, 3.4 s).
  - A command to an `archived` or `archiving` sandbox → `409 machine_not_running` with `retryable:false` and `state`.
  - Boat's own docs disagree on this code. `troubleshooting.md:169-175` gives `machine_not_running` with "Sandbox … is stopped", while `readiness.md:27` and the spec give `sandbox_not_ready`. Either code is defensible, so this is not a finding.
- **Resume while stopping.** It is accepted ("starts again on the same machine right after"). The sandbox went archiving → provisioning → ready.
- **In-sandbox CLI.** `boat --version`, `crowdvm info self` and `crowdvm stop self` work from inside the sandbox.

The sub-reviewers also checked these in code and tests, and found them fine:
- desktop token handling and takeover;
- machine sizes, nested KVM, `sandbox_running_stop_first`;
- host/hide/url and subdomain rules;
- environment versions, upgrade and access;
- TTL, reaper and pre-stop hook;
- detached command attach and kill;
- history while stopped;
- all six webhook events, headers and retries;
- OTel metric and attribute names;
- all five CI operations;
- limits headers and `Idempotency-Key`;
- all 56 Boat SDK/evals/platform doc samples;
- the Python and TypeScript method tables;
- the AgentEnv, Eve and Harbor adapters;
- conversations, including `ASCII_MAX_PARALLEL_CONVERSATIONS`;
- skills and the computer MCP in every harness config;
- ZDR, snapshots off and monitoring off, `bdop_` operations, and account close.

## Disputed classifications

- **CMD-021.** The REQUIRED row in PARITY is right. The "ADAPTED" in `docs/gaps/A.md:232` is not justified by any constraint (N1).
- **SSH-004/007/008 and URL-012 (ADAPTED).** Sub-reviewer B disputed these. `machineIp`, `sshEndpoint` and `ip` are `null` unless the node owner passes `--direct-host` (`internal/server/feat_host_ssh.go:61-67`), so Boat's raw-port and stock-OpenSSH recipes need either that opt-in or the `crowdvm tunnel` ProxyCommand. **I accept the ADAPTED label.** On a crowdsourced pool, publishing guest ports on a volunteer's LAN or Tailscale IP should be that owner's opt-in. The feature exists when it is enabled (`docs/guide/getting-started.md:129`). I did not run a `--direct-host` node live.
- **N/A rows.** I did not re-read all 26. The sub-reviewers raised no dispute in their scope, and opus-4 accepted all of them.
- **Other ADAPTED rows.** No new objection.

## What would flip this to AGREE

1. **N1:** capture tty, pipe and socket output from every process in the sandbox (ssh sessions and processes started by agents or shells), not just journald and Docker, as `process` records within about 30 s. An eBPF `write(2)` tracer fits; the shipped image already has BTF and bpftrace.
2. **Regression test for N1:** an e2e step that writes from an `ssh` session and from a backgrounded `sh -c '… | cat'` started by `exec --detach`'s child, then expects both under `history --mode process`. Note that exec'd commands themselves stay excluded (CMD-022).

m-1 to m-9 would be nice to have. They do not block AGREE.

## Not verified

- Windows nodes and VFIO GPU passthrough.
- Real Claude, Codex or Bedrock credentials.
- Real GitHub or Gitea CI.
- SMTP.
- A `--direct-host` node, and the `ip`/`sshEndpoint` it reports.
- Vision-model use of `attachments` inside a real harness. I checked only the code path.
- The dashboard beyond what the sub-reviewers read in code.
- Whether a built image that is later `docker push`ed gets a distribution-source label, and so becomes deletable at 95% disk. If it does, that is arguably correct, because it can be downloaded again.

## TL;DR

DISAGREE, narrowly, with one blocker. B1, B2, m1 and the CLI half of m2 are fixed. I confirmed B1 and B2 live: the built image survived a full disk, and the scoped key's list paginates correctly. The e2e run, including `disk-cleanup`, ended `EXIT=0`, and the opus-3 and sonnet-3 fixes still hold. The blocker is CMD-021, a REQUIRED feature no earlier review exercised: process history captures only journald and Docker output, not ssh sessions or processes started by agents or shells. I reproduced the gap live, and an eBPF write tracer, which the shipped image already supports, would close it.
