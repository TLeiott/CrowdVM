# Review sonnet-13: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. I used no sub-agents.

## Verdict: DISAGREE (one blocker: a saved-snapshot deploy can end up unresumable)

Both fixes claimed from `sonnet-12` hold in my live test, and the CI fix from `opus-14` holds in the code and its test. I found no regression next to them.

The realistic agent task found one data-loss bug in saved snapshots (`snapshot NAME` then `new --from NAME`). It needs nothing from the internet, and it is a single-node case.

- **B1.** On a node, the layer a saved snapshot is built on is dropped locally once a later compaction merges it. A later `new --from NAME` on that node then starts, but its disk keeps a backing-file name that no longer exists. That sandbox's next stop or resume fails: `stop: qemu-img rebase … Could not open backing file`, or `resume` → `sandbox_error`. `recover` fails too.

## Method

1. **Read.** `sonnet-12.md` and `opus-14.md` (claims and flip criteria), the node/lazy/store code, `feat_ci.go` and its test. For the agent sweep I read only `crowdvm agent-instructions`, `skills/crowdvm/SKILL.md` and `docs/guide/cli-for-agents.md`.
2. **Build and tests.**
   - `make build` is clean. I ran my cluster from copies of the four binaries in my scratchpad, so I did not rewrite `bin/` under the other reviewer.
   - `go vet ./...` is clean.
   - `go test -count=1` on every package except `test/e2e` is all `ok`, with 0 FAIL. `TestCIRunnerSwap` and `TestPausedNodeAndImpossibleSize` are among them.
   - I did not run e2e.
3. **Own cluster.**
   - One server on 127.0.0.1:9970 (starts limits off, 1000 signup credits).
   - One qemu/KVM node, UI on 9971, joined with a full benchmark (score 724, gold).
   - Image `ubuntu-24.04-crowdvm-v3`. The node was small on purpose: 6 vCPU, 12 GiB, 3 sandboxes, so `large` and `xlarge` cannot be placed.
   - Data in `~/.local/share/review-sonnet-13`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated.
4. **Fix checks.** Live, with the real CLI.
5. **Agent sweep.** Live, as an agent that had read only the three agent sources:
   1. create a sandbox;
   2. push a small Python web app (`crowdvm push`);
   3. run it detached;
   4. `host` it and `curl -L -c jar -b jar` it, as Boat's page says;
   5. save a snapshot;
   6. `new --from NAME --count 2`;
   7. fork one (a live fork, `--type small --wait`);
   8. stop and resume;
   9. read the history;
   10. clean up.
6. **Pages checked against the live product.**
   - Fewest earlier citations in `docs/reviews/`: `hosting__firewall`, `hosting__how-it-works`, `long-running-tasks__command-history`, `machines__virtualization`, `windows-apps__tested-apps`.
   - Also `cli-reference` (`new`, `host`, `list`, `resume`) and `snapshots__templates`.
7. **Teardown.** I deleted my sandboxes. Two qemu processes outlived the node's SIGTERM (it exited first); I confirmed by `/proc/<pid>/cmdline` that they used my data dir and killed them. I stopped the node and server by saved PID and deleted the data dir. Nothing of mine is left, and nothing listens on 9970–9979. I edited no repo file except this report. I did not touch the other reviewer's processes.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| sonnet-12 B1 | A paused node takes no new sandboxes (`failFast` → 503) | **Fixed (live)** | `crowdvm-node config set paused true` reached the server within the next heartbeat (`info.paused: true`). `crowdvm new --fail-fast` → at once `503 no_ready_machine`, nothing created (`crowdvm ls --all` empty). Plain `POST /sandboxes` → `503 out_of_capacity`, `details.nodes:["n1: paused by its owner"]`, `Retry-After: 30`. Code: `internal/server/scheduler.go:193` skips a paused node for new sandboxes only. After `paused false` the sizes came back within one heartbeat. |
| sonnet-12 B2 | An impossible size answers at once instead of retrying | **Fixed (live)** | On a 6 vCPU / 12 GiB node: `crowdvm new --type xlarge` and `--type large` (plain, no flag) → exit 1 in under 1 s with `409 size_unavailable`: "No node in this pool can host a large sandbox (8 vCPU, 16 GiB memory). Available sizes: small, default. See `crowdvm limits`." `crowdvm limits` shows `machineTypes` large and xlarge as `available:false, reason:"no node in the pool is big enough for large"`. `new --type huge` → `400 invalid_type`. `new --type large --fail-fast` → `503 no_ready_machine`, which is what Boat's fail-fast page promises. |
| opus-14 B1 | CI job ↔ sandbox follows the runner that ran the job | **Fixed (code and test; no GitHub here)** | `ghJob` now reads `runner_name` (`feat_ci.go:249`). `track()` re-points `sandbox_id` to `crowdvm-<sbID>` when the job goes `in_progress` (`:816`) and again before `completed` acts (`:845`). OOM attribution, `/debug` keeping and the delete all use the corrected sandbox. The 404 path skips the delete when another running job uses that sandbox (`runsOtherJob`, `:1490`). `TestCIRunnerSwap` (`feat_ci_test.go:337`) swaps the runners of two same-label jobs, finishes the first, and asserts that the sandbox still running the second survives. It passes. |
| — | Regression check next to the fixes | **None found** | `limits`, `nodes`, `new`, `fork` and `resume` behave as before. The full non-e2e suite is green. |

One residual in the CI fix, not blocking (see m6): a job that GitHub *cancels while queued* has an empty `runner_name`, so `track()` falls back to the sandbox booted for it. If runners were swapped, that sandbox may be running another job.

## New missing or broken supportable features

### B1. A saved-snapshot deploy can end up unresumable (single-node LAN pool, `snapshots__templates.md`, `cli-reference.md` "boat new --from")

- **What Boat promises.** `boat snapshot ID NAME` then `boat new --from NAME`: "Each new sandbox that you start from it gets the same disk", with no scheduled expiry, and the deployed sandbox is a normal one (stop, resume, fork).
- **What happens.** I ran the shortest flow that a template user runs, on one node, twice. The second run was from a clean cluster.
  1. `new --type small` (A); `exec "echo base > ~/base.txt"`; `snapshot A base2` → `ready`.
  2. `new --from base2` (B); `exec cat ~/base.txt` → `base`.
  3. Stop and resume B three times, writing a file each time. Node-side, `snapshots/snap_pyq3….json` for `base2`'s layer becomes `"deleted": true`, while its file stays (A's own layers still build on it).
  4. `new --from base2` (D) on the same node → `ready`, `cat ~/base.txt` → `base`. The log says `lazy boot, 1 of 1 chain layers remote … chain hydrated`.
  5. `qemu-img info` on D's overlay: `backing file: …/disks/snap-snap_pyq3ejwj4zspfeusdukz.qcow2`. No such file exists.
  6. `stop D` → `stopped`. `resume D` → `{"code":"sandbox_error","error":"… Could not open backing file: Could not open backing file …"}`. State `error`. `crowdvm recover D` → `reset_failed … vm not found`. Resume again → the same error.
  7. A saved snapshot taken from D while it ran (`second`) cannot be deployed either: `new --from second` → `sandbox_error`, same message.
- **First sighting.** In my first pass the same chain made `stop` itself fail on a running sandbox: `stop: qemu-img rebase: … Could not open backing file` (VM state `error`; node log `18:34:42`). That sandbox was made with `new --from hello-app --host … --file … --setup-file …` and then re-saved under the same name.
- **Not every deploy is hit.** A second deploy after the first (E) stopped and resumed fine, because the first deploy flipped the node's record back to "present". So a user can lose the *first* deploy after each compaction, and may not notice at deploy time.
- **Likely cause (from the code; I did not patch to test the fix).**
  - The node deletes every layer a new snapshot "supersedes", whatever its kind (`internal/node/transfer.go:197-199`). The server only soft-deletes `kind='periodic'` layers (`internal/server/snapshots.go:106-110`, `:178`), so the server keeps the saved snapshot's blob. The node marks it `Deleted` but keeps the file, because other layers still build on it.
  - `createLazy` then sees the layer as not present, so it names the overlay's backing file `disks/snap-<id>.qcow2` and reads the layer from the server URL (`internal/node/vm/qemu/lazy.go:84-89`, `:112`).
  - The download is thrown away: `ImportSnapshot` only "resurrects" the existing record and returns (`internal/node/vm/qemu/store.go:191-195`), and `pullSnapshot` removes the temp file (`internal/node/transfer.go:130`). The live VM works from the stored file in memory (`Hydrated`, `lazy.go:186-231`), but the overlay's on-disk header still names the missing `snap-` path. The next layer saved from it inherits that name.
- **Why it is supportable on a LAN.** Nothing external is involved. Fix, either one: make `createLazy` treat a kept-but-`Deleted` layer as present and use its real `sm.File` (checking the file exists); or make `ImportSnapshot` move the downloaded file to the `snap-` path when the record's file is the one the lazy overlays name. It would also help to stop the node from dropping layers that a saved snapshot needs: skip non-periodic ids in the `transfer.go:197` loop, as the server does.
- **Tests.** Unit and e2e tests cover lazy chains, `ImportSnapshot` and compaction apart (`e2e_lazy_test.go`, `qemu_test.go`), but none deploys a saved snapshot after a compaction that supersedes its layer, then stops and resumes the deploy.
- **Severity.** Blocker. It is silent data loss from the user's side on a one-node pool, in the template flow that the docs and the skill recommend. (With a second node, the resume may land elsewhere and fetch the chain from the server; I did not test that.)

## Minor (not blocking)

- **m1. `crowdvm limits` says "no node is online" for a paused node.** The node is online but paused. The reason string for every size should say "paused by its owner" (`scheduler.go:194` has the text). Cosmetic, but it is the one place an agent reads before it starts.
- **m2. `crowdvm info` hides why a sandbox waits.** For a sandbox waiting for capacity, `GET /sandboxes/{id}` has `waitingForCapacity`, `statusMessage` and `waitDeadline`. `crowdvm info` drops them (the `client.Sandbox` struct has no such fields) and shows `state: starting`. `new`, `new --count` and `resume` also say nothing while they wait up to 15 minutes. The skill tells the agent that they wait, so it is documented. These fields are CrowdVM's own, not a Boat spec.
- **m3. `size_unavailable` is not in the agent error tables.** `agent-instructions`, `SKILL.md` §5 and `cli-for-agents.md` list `no_ready_machine` but not the new `409 size_unavailable`. An agent that sees it will still read the message ("Available sizes: …"), so no harm.
- **m4. History of a stopped fork omits the commands it inherited.** After a stop, `history` of a fork lists 12 commands. After a resume the same call lists 18, with 5 from the source sandbox. A detached command shows `running` while the sandbox is stopped, and `lost` after a resume. Boat's page does not say whether a fork inherits history.
- **m5. Boat's UDP-block recipe starts with `sudo wg show wg0 endpoints`, which fails here** (`wg: command not found`). CrowdVM has no WireGuard tunnel (documented as ADAPTED in `PARITY.md` URL-020). The first `iptables` line of the recipe still applies, DNS and the sandbox stay healthy. The per-uid block for untrusted code (`useradd runner` + three rules) works verbatim: `sudo -u runner curl` fails with exit 7, `user` curl gets 200, health stays `ok`.
- **m6. CI: a job cancelled while queued uses the sandbox booted for it.** If GitHub swapped runners, that sandbox may be running another job. Use the same `runsOtherJob` guard on the `completed`/empty-`runner_name` path. Not verifiable here (no GitHub).
- **m7. `ls --json` shows the API state `archived`, `info` shows `stopped`.** Boat's docs say the CLI shows `stopped`; the text table does show `stopped`. Only the JSON of `list` differs. Cosmetic.

## Disputed or accepted classifications

- **Paused node + plain `new` waits.** It retries quietly up to 15 minutes. I accept that: a pause is transient, the response is the ordinary `out_of_capacity` with `retryAfterSeconds` and a reason, and the skill says that plain `new` waits. Only `--fail-fast` has to be instant, and it is.
- **Impossible size + `--fail-fast` answers `no_ready_machine`.** Accepted. It matches Boat's fail-fast page ("nothing started"). Plain `new` gives the more specific `size_unavailable`.
- **`--count` with insufficient memory.** `new --from X --count 2` on a node with room for one created both; one waited for a node with room. Boat's text is about start limits ("If the batch does not fit, boat creates none of it"), not about machine room. Accepted.
- **`--name` default.** CrowdVM names a plain `new` "Sandbox <date>"; Boat says `Box <date>`. Cosmetic.

## Checked and fine (the agent task against Boat's pages)

| Step | Result |
|---|---|
| `new --json` | `created`, `state`, `ready` lines; `ready` has the id (the `ip` field is empty). About 3 s. |
| `push .` | Sent 3 files: `server.py`, `.gitignore`, and the untracked `junk.txt`. `node_modules/` stays out. Matches the skill. |
| `exec --detach` / `--status` | `processId`, `logPath`; status `running`. |
| `host ID 3000` | Prints a bare URL; `--json` gives `url`, `hostUrl`, `pathUrl`, `isProtected`. `curl -L -c jar -b jar` works (302 + cookie → 200 "hello from …"). No token → 401. App not started → a clear `502` page saying so. `--localhost` and not passing it both worked for an app bound to `127.0.0.1`. `--public` drops the token. `host list` and `host hide` (→ 404) work. |
| `snapshot ID NAME`, `snapshots`, `snapshot info/rm/share` | Work. A repeat save while one runs → `409 save_in_progress`. |
| `new --from NAME --count 2` | Prints `created` per sandbox and returns, as Boat says. `ls` hides pending sandboxes unless `--all`, as Boat's filter table says. |
| `new --from … --host 3000 --host 8080:public --file workspace/.env=… --setup-file … --name "api worker"` | `hostedPorts` in the `created` line, `.env` in place, `setupStatus` `pending` → `done`, hostname `api-worker`. |
| `fork --type small --wait` of a running sandbox | `cloning` → `ready` in about 4 s; files kept; no processes (as Boat says). |
| `stop`, `resume --type …`, `resume` on a full node | The state changed. A resume waits for room (silently, m2) and survived my killing the CLI: the server finished it later. `resume --wait` does not exist, in Boat or here. |
| `history` | `--mode commands|process`, one command, `--stderr`, the API's `logs?stream=stdout&offset=0` (`done`, `nextOffset`), and reading after a stop all work. |
| `ssh ID "cmd"`, `/dev/kvm`, the base tools | Present: docker, git, gh, node, bun, python3, rustc, claude, codex; `kvm` group. |
| `boat` as a name | A symlink `boat → crowdvm` works outside a sandbox too. |

## Not verified

- CI against real GitHub or Gitea.
- B1 with a second node (does a resume elsewhere avoid the bug?), and a node restart after B1.
- Windows and macOS nodes; the node dashboard in a browser; `crowdvm prompt` with a real AI credential.
- `make e2e`. The task told me not to run it, so I do not know whether the latest e2e run covers B1 (it cannot, going by the test list in `docs/E2E.md`).

## What would flip this to AGREE

1. **B1.** After a saved snapshot's layer is merged into a descendant's compaction on the node, a deploy on that node (`new --from NAME`) must stop and resume cleanly. Add a test that: saves a snapshot; deploys and stops/resumes a child until a compaction supersedes the saved layer; deploys again; then stops and resumes the second deploy (and saves a saved snapshot from it, then deploys that).
2. Optionally m1 (say "paused" in `limits`) and m3 (list `size_unavailable` in the agent docs). They are cheap.
