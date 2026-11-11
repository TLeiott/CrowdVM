# Review opus-22: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, no sub-agents). Date: 2026-11-11, 04:10–05:15. Tree: uncommitted working tree on `master`. I ran `make build` at 04:13, after the last Go edit (`internal/server/snapshots.go`, 03:58), and copied the binaries into my own dir. No Go file changed while I tested.

## Verdict: DISAGREE (two blockers, both in the new stop adoption)

The node's `stopRef` fixes opus-21 N1 for a **server** restart. In 9 of 9 server-only rounds, every synced file was present after the resume, and no plain stop was recorded as forced. sonnet-19's false-final `snapshot_data_expired` is fixed too: it is now a retryable `snapshot_unavailable`, and the resume worked once the holder came back.

The fallback for a server **and** node restart (`nodeStopLeaf`) is wrong in both directions:

- **B1.** It refuses to adopt when a merge left the node with two leaves of the sandbox, which is the normal case after tiered compaction. The stop then ends `forced` on an older stored layer, and synced data is lost. This is opus-21 N1, still open on the both-restart path. I reproduced it live.
- **B2 (regression).** It adopts a stale leaf: an old layer from an earlier run on that node. The head then moves **back** past completed, stored stops made on the other node. I reproduced it live and in a servertest. It does not even need a server restart: one node crash during a resume onto a node that ran the sandbox before, then a `stop`.

## Method

1. **Read the fixes.**
   - `StopVM` and `stopRef` (`internal/node/vms.go:38-41,513-555`).
   - `runStop`'s 404 branch and `nodeStopLeaf` (`internal/server/lifecycle.go:946-1021`).
   - `recordSnapshot`'s stop-head update (`snapshots.go:125-128`).
   - The `afterSnapshotStored` compare-and-swap (`snapshots.go:283-322`).
   - The retryable branch of `resume` (`lifecycle.go:1262-1274`).
   - The qemu driver's `Stop`, `freeze`, `List` and `lock` (`internal/node/vm/qemu/qemu.go`).
   - `adoptVMs` (`node.go:227`).
   - The fake node's `GET /v1/snapshots` (pulled layers have `sandboxId` "").
2. **Build and tests.** `make build` and `go vet ./...` are clean. `go test -count=1` on every package except `test/e2e` is all `ok`, including the new `TestStopAdoptsNodeLayerAfter404` and `TestResumeWhileFirstLayerUploads`. I did not run e2e.
3. **Own cluster** in `~/.local/share/review-opus-22`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated and ports 9960–9965 only:
   - `crowdvm-server` on 9960 (`--snapshot-interval 20`, `--public-url` set to the proxy).
   - A Go TCP proxy on 9965 that paces each connection and direction to 40 MB/s. Both nodes joined through it. A 582 MB image push took 15 s.
   - n1 (UI 9961) and n2 (UI 9962): qemu/KVM, 6 vCPU, 12 GiB, 2 sandboxes each, 20 s periodic snapshots, image `ubuntu-24.04-crowdvm-minimal-v3`.
4. **One round.**
   1. Write a random file with `dd`, `sync` it and record its sha256 on the host. Wait until a large upload has started, then write and sync a second file.
   2. Run a plain `crowdvm stop`.
   3. Kill the server, triggered by the node log: at a delay after the stop was issued, after `state=stopping`, or after `stopped as`. SIGKILL or SIGTERM, and with `BOTH=1` also `kill -9` the hosting node and restart it.
   4. Restart, wait for `archived`, then resume on the other node (one round on the same node).
   5. Run `sha256sum -c` over every file written to that sandbox so far.
   6. Record the stop row (`forced`, `snapshot_id`) and the head from the DB.
5. **Harness note.** At 04:50 the host disk filled up (ENOSPC on my server's blob write, other clusters were running too). The guest of one round hung, and I discarded that round. I deleted everything, which freed 65 GB, and used 300+200 MB writes from then on.

## Fix status

| # | Claim | Status | Evidence (live, 40 MB/s) |
|---|---|---|---|
| opus-21 N1 / sonnet-19 N2: server restart mid-stop | The node answers a repeated stop with `stopRef` | **Fixed for a server-only restart** | 9 rounds: SIGKILL at issued+0.3 s, stopping+0.05/0.1/0.5/1 s and stopped+0/0.1 s; SIGTERM at stopping+0.2/2 s; one round resumed on the same node. **9 of 9** had every synced file present, `forced=0` and the stop layer as head. The logs show the path: "prepare-stop failed; stopping anyway", then the retry, then the node's earlier ref (for example `bx_8esb78a4`, 04:20:23: "stop attempt failed node_offline", then completed with `snap_u8t6…`, the layer the node had logged). |
| Same, with server **and** node restarted | `nodeStopLeaf` adopts the node's single newest layer | **Not fixed. Data loss (B1)** | 5 valid rounds: 2 OK, **1 lost 2 synced files**, 1 ended in `error` (m1), 1 ran on an already-errored sandbox (not counted). |
| sonnet-19 N1: false-final `snapshot_data_expired` | Retryable `snapshot_unavailable` while unstored layers exist | **Fixed** | A young sandbox with 500 MB and nothing stored yet. I killed its node and ran `stop --force`, so the head is NULL. `resume --node n1` answered `snapshot_unavailable` "still being uploaded … Retry in a minute" 5 times. I restarted n2, and the resume was accepted at +15 s and ready with `d_x1: OK`. |
| Head-selection compare-and-swap with same-ms tie-break | — | **Code reviewed, no regression seen** | The update is guarded by the head that was read, never steps back to the head's own parent, and stops once the head's chain contains the layer. In every round the head was the newest layer, except in B1 and B2, which come from the stop adoption, not from this code. |
| Fork, save, deploy, cross-node resume (opus-20 AGREE cases) | — | **Hold** | Two 200 MB files on A (n2). `snapshot A sv22` was ready. A running fork to n1 and `new --from sv22` on n1 each had 2 of 2 checksums matching. Then I stopped A and the fork, resumed A on n1 and the fork on n2: 2 of 2 each. A's head chain held 0 of the fork's layers. |
| Deletion audit | — | **Holds, with m2** | After deleting all: 8 of 8 deletion ops `completed`, 0 unpurged rows, 0 locations, 0 server blobs. n1 still held 2 layers of a deleted sandbox until it reconnected (m2). |

## Blocking findings

### B1. A server and node restart right after the node finished a stop still loses synced data (`internal/server/lifecycle.go:1014`, `:962`; `internal/node/vms.go:41`)

**Boat:** a plain stop keeps everything written before it. Only `--force` or a lost machine keeps "the last successful snapshot" (`snapshots__stop.md`).

**Repro (live).** Sandbox `bx_wvahrfqy` on n1:
1. 300 MB `d_r362` was written and synced, and its periodic upload had started. Then 200 MB `d_s11553` was written and synced.
2. Plain `stop`.
3. 0.02 s after n1 logged `stopped as snap_a7av…`, I ran `kill -9` on the server and on n1, then restarted both.
4. Result: the stop row was `forced=1 snapshot_id=NULL`, and the head stayed on the older stored `snap_3tqy`. After `resume --node n2`, `d_r362` and `d_s11553` were both missing (FAILED open or read).

**Cause.**
- `stopRef` lives only in memory, so the restarted node answers 404.
- n1's store then held two leaves of the sandbox:
  - the old periodic chain `v8eg→3tqy`,
  - and `hxp7→a7av`. The pre-flight save had merged 3 layers into `hxp7`, which has no parent and supersedes `fmp8, 3tqy, v8eg`.
- `nodeStopLeaf` requires `len(leaves) == 1`, so it gave up, and `finishStop(…, true)` forced the stop on `3tqy`.
- `hxp7` held both files. The server had a row for it, and it was stored 15 s later, but by then the sandbox had moved to n2.
- Merges are routine (tiered compaction), so the two-leaf state is common, not a corner case.

**Why it is supportable.** This is node and server bookkeeping. Either of these would do:
- Persist the last stop's ref next to the VM record (`vm/vms`), so a restarted node can still answer the repeated stop.
- In `nodeStopLeaf`, ignore leaves that another layer of the sandbox supersedes (the node knows `supersedes`), or pick the leaf that is newest by `createdAt` among those whose chain contains the head.

**Scope.** It needs both processes to die in the same second or two after a stop, for example a power loss or reboot of a machine that runs both server and node, which is a normal self-hosted setup. That is rare, but it loses synced data, and the task asked for exactly this window.

### B2. Regression: a stop that gets a 404 adopts a stale layer from an earlier run on that node, and the head moves back past completed stops (`internal/server/lifecycle.go:991-1021`, `:958`; `internal/server/snapshots.go:125-128`)

**Boat:** a stopped sandbox resumes with everything written before its last stop.

**Repro (live, `bx_gena9v84`).**
1. Created on n1. Wrote `d_a1`, then `stop --wait`. n1 now holds its own layers `68dx→sjpn`.
2. `resume --node n2`. Wrote and synced `d_b2` (200 MB) and `d_b3` (100 MB). `stop --wait`, and waited until all layers were stored. Head `snap_vtqk…`.
3. `resume --node n1`, and `kill -9` n1 at "lazy boot" (before qemu started). Restarted n1. The sandbox is `error node_offline`; n1 has no machine for it.
4. Plain `crowdvm stop`. It answered `stopped`. The head is now **`snap_sjpn…`**, n1's old stop layer from step 1, and the stop row has `snapshot_id=snap_sjpn…`.
5. `resume --node n2`: `d_a1: OK`, **`d_b2` and `d_b3` missing**. Data that a completed, uploaded stop had kept is lost.

**Deterministic servertest.** I wrote it in a scratch copy of the repo, not in the repo: `…/c076d78c-…/scratchpad/repo/internal/server/review22_test.go`.
- Steps: stop on node a, resume and stop on node b, resume on a. Then a's `/stop` and `/snapshot` answer 404, and the test runs `stop`.
- Result: `head after 404 stop: snap_msp6… (s1) forced=0`. The head went from b's `s2` back to a's stale `s1`, recorded as a clean stop.

**Cause.**
- `nodeStopLeaf` only counts layers whose node record carries this sandbox's id. Layers the node pulled have `sandboxId` "", so an old local chain looks like "the newest layer". The comment ("a newer head would be that leaf or build on it, both excluded above") does not hold: a head that builds on the leaf through pulled layers is not excluded.
- Then `recordSnapshot(…, "stop")` is called with no `CreatedAt`. `created` becomes now, so the "head is newer" guard in `snapshots.go:126-128` never fires, and the head is overwritten.
- Any 404 can trigger this: a driver without the machine (a failed start, or a node restart before qemu was up), on a node that ran the sandbox before. That holds for forced stops too, because the 404 branch runs before the `op.Forced` check.

**Why it is supportable.** Only adopt a leaf whose chain contains the current head. You can check that on the server by walking the node's parent links plus the server's rows, or by asking the node for the leaf's full chain. Also pass the node's `CreatedAt` into `recordSnapshot`, so the "newer head" guard works.

## Minor findings (not blocking)

- **m1.** If the node dies mid-freeze after it has already halted qemu, it comes back with the VM `error` ("qemu not running after node restart (overlay kept)").
  - The server's retried plain stop then re-runs the pre-flight save, which gets a 500 "vm not running", and the stop is **refused** ("the sandbox keeps running"), but the sandbox is `error`.
  - The overlay is intact: a second `stop` froze it (`snap_mn5r…`), and every file was present after the resume.
  - It needs a user action, but it loses nothing and the message points to `resume`/`recover`. A fix: skip the pre-flight save when the node reports the VM as not running, and freeze the kept overlay (`lifecycle.go:920`).
- **m2.** After a delete, a node can keep layers of the deleted sandbox until it reconnects.
  - `snap_a7av…` (the stop layer the server never recorded, from B1) and `snap_hxp7…` (its row was already `purged`) stayed on n1 for over 10 minutes after the deletion op completed.
  - Restarting n1 erased both ("erasing a node's copy of deleted data").
  - It heals by itself, but only on reconnect. B1's fix removes the main source.
- **m3.** A sandbox whose node restarted mid-resume (VM never started) stays `error node_offline` after the node is back online and connected (B2, step 3). The error message is stale.
- **m4 (carried over).** The CLI's `--json` error still drops `retryable` and `retryAfterSeconds`. The text says "Retry in a minute".

## Disputed or accepted classifications

- **B1 is blocking** although it needs two processes to die at once: synced data is lost and the user is told `stopped`. That is the rule the task sets.
- **B2 is blocking and is a regression** introduced by the stop adoption. Before it, the same 404 fell back to the stored head, which was correct here.
- **e2e run27** (built at 04:10 on this tree) ended `EXIT=1` with only `snapshots` failing (37 PASS).
  - `lifecycle_test.go:357` expected `snapshot_unavailable` but got 202.
  - The response shows `snapshotCompletedAt` set and the stop `completed`. The head chain was stored, and the one unstored layer it listed was off the head chain, so accepting the resume is correct.
  - I read it as the test's assumption racing the upload. It also ran while my throttled cluster loaded the machine. Not counted as a finding. run26 passed.
- I did not re-grade PARITY.md row by row. Nothing in this round touches feature classifications.

## Not verified

- A natural same-millisecond parent/child store race (I reviewed the code only).
- More than 5 both-restart rounds. SIGTERM with both restarted.
- Windows nodes, the dashboard, ZDR, and account close/erase.

## What would flip this to AGREE

1. **B2.** `nodeStopLeaf` never adopts a layer whose chain does not contain the current head, and the adopted layer keeps its node `createdAt`. Add a servertest like mine above. Then a live run: run on node a, run and stop on b, failed resume on a, `stop`, resume. Every file from b must be present.
2. **B1.** Either persist `stopRef` on the node, or make `nodeStopLeaf` handle merged/superseded leaves. Then run 8 or more rounds of "write and sync, plain `stop`, `kill -9` server **and** node 0–1 s after `stopped as`, restart both, resume on the other node", with at least half of them after a merge. Every round must keep every synced file, and none may be recorded `forced`.

## Teardown

- I deleted every sandbox (7 in total) and the saved snapshot `sv22`. Final audit: 0 unpurged rows, 0 locations, 0 server blobs, and only the base image on both nodes after n1's reconnect.
- I stopped n1, n2, the server and the proxy by saved PID. No process or qemu with my dir remained, and nothing listened on 9960–9969.
- I deleted `~/.local/share/review-opus-22`. Logs are in `/tmp/claude-1000/-home-tl-dev-CrowdVM/c076d78c-0856-499f-a244-dd35470b4351/scratchpad/logs/`, and the scratch repo copy with my test is in `…/scratchpad/repo/`.
- I touched no other process or cluster. The only repo file I wrote is this report.
