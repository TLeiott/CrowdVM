# Review opus-23: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, no sub-agents). Date: 2026-11-11, 05:52–06:55. Tree: uncommitted working tree on `master`.

## Verdict: AGREE (on the tree as of 06:38, built at 06:43)

The three fixes the team listed hold. I checked each one live, through a 40 MB/s throttle:

- **opus-22 B1** (server + node restart right after a stop): 24 rounds, every synced file kept, none `forced`. 17 of them took the 404 adoption path, and 9 of those 17 had a tiered merge in the same round.
- **opus-22 B2** (a 404 adopts a stale layer): the head stayed on the other node's stop, and every file was present. I checked this on both builds.
- **sonnet-20 B1** (`fork` with layers still uploading): retryable `snapshot_unavailable` in the no-head state and when the holder node is offline. `fork` succeeded once the holder was back.

One caveat on timing. My main run used the 05:52 build. That build still had sonnet-21's blocker: a plain `stop` while the node is offline was forced at once (`lifecycle.go:853` in its old form). The team changed `lifecycle.go` at 06:38. I rebuilt at 06:43 and checked three things live:

- sonnet-21's scenario: the stop waited for the node, ended `forced=0`, and the marker survived.
- The lost-node path: a node gone for good still ends the stop. It is forced on the last stored layer after about 3 minutes, which is Boat's "last good snapshot".
- B2, once more.

I found no new blocking gap. The minor items are below.

## Method

1. **Read the fixes.**
   - `runStop` keeps one layer id per stop in `stops.snapshot_id` (`internal/server/lifecycle.go:973-980`).
   - The 404 branch adopts only that id, via `nodeLayer` (`:990-1001`, `:1028`).
   - `noHeadErr` is shared by resume and fork (`:888`, `:1288`, `:1454`).
   - The CLI `details` field (`internal/cli/app.go:167`).
   - Later: the 06:38 change to the stop request's force rule (`:848-855`).
   - The node side: `StopVM` names its stop layer with the server's id (`internal/node/vms.go:513-555`). The qemu driver's stop merge writes into that same id (`qemu.go:414-431`).
2. **Build and tests.**
   - `make build` was clean at 05:52 and again at 06:43.
   - `go test -count=1` on every package except `test/e2e`: all `ok`, except one run of `internal/guest` `TestHistoryExportAck` (m4). That test passed 5 of 5 on a rerun.
   - On the 06:43 build, `internal/server` and `internal/node/...` were all `ok`.
   - The new servertests `TestStopAdoptsNodeLayerAfter404`, `TestStop404IgnoresStaleNodeLayer` and `TestResumeWhileFirstLayerUploads` (which includes the fork case) pass.
   - I did not run e2e.
3. **Cluster setup.** I ran my own cluster in `~/.local/share/review-opus-23`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated, on ports 9960–9965 only.
   - `crowdvm-server` on 9960 with `--snapshot-interval 20`.
   - A Go TCP proxy on 9965 that paces each connection and direction to 40 MB/s. Both nodes joined through it.
   - Nodes n1 (UI 9961) and n2 (UI 9962): qemu/KVM, 6 vCPU, 12 GiB, 2 sandboxes each, 20 s periodic snapshots, image `ubuntu-24.04-crowdvm-minimal-v3`.
4. **B1 rounds.** Each round went like this:
   1. Write a random file and `sync` it, recording its sha256. Wait for a large upload to start. Write and sync a second file.
   2. Plain `crowdvm stop`.
   3. Kill: 0–1 s after the node logs `stopped as`, `kill -9` the server **and** the hosting node, then restart both.
   4. Resume on the other node and run `sha256sum -c` over every file written so far.
   5. Record the stop row (`attempts`, `forced`, `snapshot_id`) and the head from the DB.

   The `stopped as` answer reaches the server about 15 ms after the log line. In my first 8 rounds (r3), only round 1 hit the 404 path. The other 7 had already completed their first attempt (`attempts=1`). For the remaining 16 rounds (r4), I froze the proxy with SIGSTOP once the node logged `state=stopping`. The node then finishes the stop, but its answer cannot reach the server, which is exactly opus-22's window. I thawed the proxy after both kills. Every r4 round shows `attempts=3`:
   - attempt 1 was cut;
   - attempt 2 was `node_offline`;
   - attempt 3 got the node's 404 and adopted the layer.

   To force merges, 5 of the r4 rounds first let 3 periodic layers pile up.
5. **Regression pass on opus-20's AGREE cases** (throttled):
   - forks of stopped and running sandboxes, across nodes;
   - a saved snapshot and `new --from`;
   - stopping the source and the fork, then resuming each on the other node;
   - a deletion audit.
6. **Disk.** One sandbox's tiered merge folded 34 layers into a 10.5 GB stop layer. My dir reached 86 GB and the host fell to 22 GB free. I deleted that sandbox right away (back to 100 GB free) and used 150–300 MB writes from then on.

## Fix status

| # | Claim | Status | Evidence |
|---|---|---|---|
| opus-22 B1: server + node restart right after a stop, after merges | One layer id per stop, stored before the node call; after a 404, adopt exactly that id if the node holds it | **Fixed** | 24 rounds, 24 pass, 0 recorded `forced`. **404 path: 17 rounds** (r3 round 1, plus 16 r4 rounds), every one `attempts=3 forced=0 snap=<the node's "stopped as" id>`, head = that id. **9 of the 17 had an in-round merge** ("merged 3/4/5/34 layers into the stopped overlay"). The other 7 r3 rounds were answered before the kill (5 had merges); all pass. 0 missing files in any round; the largest check covered 35 files. |
| opus-22 B2: a 404 adopts a stale layer and the head moves back | Only this stop's own layer can be adopted | **Fixed** (05:52 and 06:43 builds) | Same steps as opus-22: run on n1, run and stop on n2 (stored), resume on n1, `kill -9` n1 at "lazy boot", restart, `stop`, resume on n2. Head before and after the stop: `snap_gw7f…` (05:52) and `snap_ts6d…` (06:43), the same both times. The stop was recorded `forced=1 snap=-`, which keeps the last snapshot: correct, since n1 never ran the machine. Every file from n2 was `OK` (3 of 3, then 4 of 4). |
| sonnet-20 B1: `fork` false-final `snapshot_data_expired` | `fork` uses `noHeadErr` | **Fixed** | No-head state set in the DB: `fork` and `resume` both answer `409 snapshot_unavailable` with `details:{retryable:true,retryAfterSeconds:30}`. Natural case: I stopped a sandbox and killed its node while 3 unstored layers were uploading. `fork` and `resume` answered `snapshot_unavailable` "…node that holds it is offline. Retry once that node is back online", `retryable:true`. After restarting the node, `fork` was accepted on the first retry, and the fork had 2 of 2 files `OK`. |
| opus-22 m4: CLI `--json` errors drop `details` | `details` added | **Fixed** | Seen in all the outputs above. |
| sonnet-21 B1 (raised by another reviewer during my run): plain `stop` on an offline node is forced at once | 06:38: `error` with `node_offline` no longer forces; the stop waits up to `NodeLostGrace` | **Present on the 05:52 build. Fixed on the 06:43 build.** | 06:43 build: I wrote and synced `d_m1` 25 s after the last periodic, then ran `kill -9` on n1 (qemu kept running). The sandbox went `error node_offline`. A plain `stop` stayed `archiving`, with the stop `failing forced=0 err=node_offline`. I restarted n1, and 4 s later it was `completed forced=0` with the node's layer. After a resume on n2, `d_m1: OK`. **A node gone for good** (I killed the node and its qemu and did not restart them): the plain stop completed after 199 s as `forced=1` on the last stored layer, and the resume on n1 had 4 of 4 files `OK`. That matches `snapshots__stop.md:146-148` for a lost machine. |
| opus-20 AGREE cases | — | **Hold** | **Fork of a stopped sandbox** while its stop layer uploaded: accepted, ready, `d_f1: OK`. **Running fork** n1 → n2: 4 of 4 `OK`. **Saved snapshot** `sv23` and `new --from sv23`: the 3 files written before the save were `OK`. **Stop and cross-resume**: fork K on n1 has 5 of 5 `OK`, and source H on n2 has 5 of 5 `OK`. H's head chain has 0 of K's layers, and H has no `d_k1`. **Deletes**: 7 of 7 deletion ops `completed`, 0 unpurged rows, 0 locations, 0 server blobs. Only the base image was left on both nodes. While `sv23` existed, its chain, including the parent sandbox's layers, was correctly kept. |

## New missing or broken supportable features

None blocking on the 06:43 build.

### Minor (not blocking)

- **m1. A fork placed on a node in the moment after that node died fails instead of being placed elsewhere.**
  - What I saw: `fork` was issued about 0.6 s after `kill -9` of n1, before the server had logged `node disconnected`. The fork went to `error`, `start_failed: Post "http://node/v1/vms": EOF`, and stayed there.
  - Impact: no data is lost, the source is intact, and deleting the fork and forking again works.
  - It is a crash window. A retry of placement on `start_failed` with a transport error would close it.
- **m2 (opus-22 m3, still open).** After a node restarts during a resume, the sandbox stays `error` with `error=node_offline` although the node is back and connected. I saw it on both builds. The next `stop` handles it correctly, so only the message is stale.
- **m3. The adopted stop layer is recorded with only `{id, parent}`** (`lifecycle.go:996`).
  - The node's `createdAt`, `size` and `supersedes` are dropped.
  - I saw no effect: the size is filled in on upload, a fresh id cannot lose the head-age guard, and after 9 merge rounds there were 0 off-chain periodic rows.
  - It would matter only if GC came to depend on `supersedes`.
- **m4. A test flake.** `internal/guest` `TestHistoryExportAck` (`core_test.go:175`, "detached output exported before its 30 s cadence") failed once while the host was loaded by two clusters. It passed 5 of 5 on a rerun.
- **Not verified.** A node that comes back after its sandbox was force-stopped (lost-node grace) and resumed elsewhere adopts the old overlay in `error` state ("qemu not running after node restart (overlay kept)"). Five seconds later, n2 still had 6 non-image files of the deleted sandbox. I tore the cluster down before I could see whether reconnect cleanup removes them. It is the same class as opus-22 m2 and sonnet-21 m1, a node disk leak that should heal on reconnect. No user data is involved.
- **Performance only.** A 34-layer merge made one stop layer 10.5 GB. The cross-node resume then waited about 4.5 min for the upload and read it lazily at 40 MB/s. That is correct but slow, and not a finding.

## Disputed or accepted classifications

- **sonnet-21 B1 is blocking. I agree with it for the 05:52 build.** Boat: "Every stop saves the disk first … Only `boat stop --force` … skip the save" (`snapshots__stop.md:17`). A node that is offline for a few seconds is not a lost machine. My own B2 run on that build went through the same path (`error node_offline`, then `stop`, then `forced=1`). It lost nothing only because the VM really was gone. The 06:38 change fixes it, and I verified that live. The 3-minute `NodeLostGrace` before forcing a stop on a node that never returns matches Boat's lost-machine rule (`:146-148`), so I accept it.
- **Stop of a sandbox whose machine never started (B2 step 4) records `forced=1`.** Correct: there is nothing newer than the head to save.
- I did not re-grade `PARITY.md` row by row. Since opus-22, the only docs change is `docs/guide/security.md`, at 06:38. No feature classification changed.

## What would flip this to DISAGREE

- Any loss of synced data on a plain stop, in particular on the 06:38 path. My check there was one live repro plus the lost-node case, not a kill matrix.
- A regression in the e2e run on the 06:38 tree. The team's e2e-full30 log predates both the `fork` fix and the 06:38 change, and I was not allowed to run e2e.

## Teardown

- I deleted every sandbox (8) and the saved snapshot `sv23` on both clusters. The audit before each teardown showed 0 unpurged rows, 0 server blobs and every deletion op `completed`.
- I stopped n1, n2, the server and the proxy by saved PID, both times.
- No process with `review-opus-23` in its argv remained, and nothing listened on 9960–9969.
- I deleted `~/.local/share/review-opus-23`.
- Logs: `/tmp/claude-1000/-home-tl-dev-CrowdVM/cef8df39-aa73-4692-bf38-d314fc95ffd5/scratchpad/logs/` (05:52 build, with the DB) and `…/scratchpad/logs2/` (06:43 build).
- I touched no process or port of the other reviewer (9970–9979). The only repo file I wrote is this report.
