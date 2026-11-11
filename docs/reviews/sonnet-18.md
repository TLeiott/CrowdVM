# Review sonnet-18: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent, no sub-agents). Date: 2026-11-11. Tree: uncommitted working tree on `master`. `bin/` was built at 00:50, after the last Go edit (`test/e2e/*_test.go`, 00:40). I did not carry over any earlier verdict, including my own sonnet-17.

## Verdict: DISAGREE (two narrow blockers)

Every earlier blocker is fixed in live runs behind a 40 MB/s link:

- sonnet-17's overlay orphan after a server restart in the live-snapshot window.
- opus-19 N1: a node restart mid-upload stranded a stopped sandbox.
- opus-19 N2: a forced stop kept an unstored head.
- The older stop, save, fork and delete mid-upload cases.

Every survivor resumed with its data. Every saved snapshot deployed. Deleted sandboxes left nothing on the nodes.

Two things still fail the bar I was given:

- **B1.** After a server restart that lands while a node reports a layer, a stopped sandbox answers a resume with `resume_failed`, "no longer exists … create a new sandbox". The error is marked final but is wrong. The data is intact and the same resume works a few seconds to minutes later.
- **B2.** A server restart during any snapshot upload leaves the half-received file `server/blobs/snapshot/.upload-*` on the server for good. It is guest plaintext and it survives the deletion of every sandbox and saved snapshot. Both `kill -9` and a plain `SIGTERM` leave it.

Both are node/server bookkeeping and fixable on a LAN with no new dependency.

## Method

1. **Build and tests.** `go vet ./...` is clean. `go test -count=1 ./...` is `ok` in every package (`internal/server` 48 s). I did not run `make e2e` or `go test ./test/e2e/...`.
2. **Own cluster** in `~/.local/share/review-sonnet-18`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated and ports 9970–9975 only (9970 server, 9971/9972 node UIs, 9975 proxy).
   - One `crowdvm-server`.
   - A Go TCP proxy with strict pacing (no banked credit) at 40 MB/s per connection and direction. The nodes joined through it and the server's `--public-url` points at it. The CLI talked to 9970. A 582 MB image push took 20 s, so the cap works.
   - Nodes `nA` and `nB`: qemu/KVM, 4 vCPU, 8 GiB, 2 sandboxes each, `--snapshot-interval 20s`, image `ubuntu-24.04-crowdvm-minimal-v3`.
3. **Judging data.** Every sandbox wrote random files (`dd` from `/dev/urandom`, then `sync`) and I recorded each sha256. After every move, resume, fork or deploy I recomputed all of them (`chk`).
   - "Upload in flight" was read from the node log (an upload id started and not finished since the node's last start).
   - Cleanup was judged per sandbox on both nodes (`vm/disks`, `vm/snapshots`, `vm/vms`, qemu processes) and on the server (unpurged rows, blob files, **including dot-files**).
4. **Teardown.** I deleted every sandbox and both saved snapshots. Everything on the nodes went (see B2 for the one thing on the server that stayed). I stopped nA, nB, the server and the proxy by saved PID. No process referenced my dir afterwards and 9970–9979 were free. I deleted the data dir. I touched no other process.

### Part 1: server restart in the live-snapshot window (sonnet-17 repro)

- **Per round.** Two sandboxes, one per node. Each had `dd … count=1500` running in the background. I ran `stop` on both, `kill -9`ed the server 0.05–1.2 s later and restarted it. I waited until both were `stopped` (re-issuing `stop`), deleted both, then audited.
- **Window.** The server went away between the stop request and the snapshot switch. In 29 of the 32 sandboxes the node logged `context canceled` (`sync before snapshot` or `prepare-stop`); the three exceptions were the latest delays (1.0–1.2 s), where the stop had already got past that point.
- **Round count.** 16 rounds of this plus about 14 more restarts in the other scenarios (4 rounds × 4 sandboxes before the script was simplified, plus the chain-gap runs).
- **Result.** In all 16 rounds, after both deletes: node `vm/disks` (without the base image), `vm/snapshots` and `vm/vms` were all empty. There were 0 unpurged server rows, 0 blob files and 0 qemu processes. All 32 stops ended `stopped`. In the 4-sandbox rounds each sandbox was then resumed and its sha256 matched, except as noted under "Disputed" below.
- **Why it holds.** `qemu.go:578-583` runs `blockdev-snapshot-sync` under `context.WithoutCancel`, so qemu's switch is recorded as a layer even though the caller left.

### Part 2: throttled pool session with node restarts

| Step | What I did | Result |
|---|---|---|
| A | s1 on nA. Wrote 60 MB, waited for a periodic tick, wrote 700 MB, waited for an upload in flight, `stop --wait`, **SIGTERM nA** while the stop's 3 layers were all pending, restarted nA, `resume --node nB` | Ready in 48 s. Both files matched. The server asked nA to upload the 3 layers (`stranded.go`). |
| B | s2 on nB, same, **kill -9 nB**. `snapshot` of the stopped sandbox, then `fork --node nA` | Fork ready in 55 s. Both files matched. (The `snapshot` call failed on my own uppercase name; I re-saved from a running sandbox later.) |
| C | f2 (the fork) on nA. Wrote 300 MB, saved `save-a --no-wait` during an upload, **kill -9 nA** 1.5 s later, then `stop --force --wait` while nA was down, then `resume --node nB` | Head fell back to the newest stored layer. Resume ready in **4 s**. `g1`/`g2` present, `h1` (written after the last stored layer) absent, as Boat's forced stop documents. After nA came back, `save-a` went `ready` in 10 s. A deploy of it on nB had all 3 files. |
| D | s4 on nA. Wrote 800 MB, `stop --wait`, **kill -9 nA** at once, `resume --node nB` for 200 s | Refused immediately with 409 `snapshot_unavailable`: "Part of this sandbox's disk (…) is not on the server yet, and the node that holds it is offline. Retry once that node is back online." After nA returned and uploaded, the resume completed with both files. |
| E | s5 on nA. 700 MB upload in flight, **kill -9 nA**, `delete` | The op stayed `processing/retrying` while nA was down. After the restart it went `completed` within 15 s. Nothing of s5 on either node or on the server. |
| F | f2 on nB. Wrote 500 MB, `save-b --no-wait` during an upload, **SIGTERM nB** 1 s later | `save-b` ready in 15 s. Deploy on nA had all 3 files. f2's adopted VM was intact. |
| G | s4 running on nB. Wrote 200 MB, then 300 MB with an upload in flight, **kill -9 nB**. `exec` while down, then a plain `stop`, then `resume --node nA` | `exec` said "being restored (node_offline) … Retry in a few seconds" (502 `sandbox_direct_failed`). The plain `stop` completed at once as `forced`. Resume on nA in 11 s with all files of the last stored layer. After nB restarted, its stale adopted VM was destroyed. |
| H | s1: `resume --node nB`, then **kill -9 nB** right after the lazy boot | After the restart the sandbox was `ready` and the data matched. |
| I | Server **SIGTERM** during a 600 MB upload, then restart | Nodes reconnected. The re-upload finished. **A 100 MB `.upload-*` stayed on the server (B2).** |
| Final | Deleted every sandbox, `snapshot rm save-a/save-b` | Node `vm/disks`, `vm/snapshots`, `vm/vms`: 0. Server rows 0, blob files 0. 0 qemu. **21 `.upload-*` files (2.4 GB) remained (B2).** |

Errors an agent saw, all judged for "retryable with a hint, or truly final":

- `snapshot_unavailable` (D): retryable, clear text.
- `sandbox_direct_failed 502 … Retry in a few seconds` (G): retryable, clear text.
- `409 sandbox_resuming`, "waiting for capacity" when a node was full (I hit it twice myself): fine.
- **`resume_failed` (B1 below): wrong, marked final.**

## Fix status

| # | Claim | Status | Evidence |
|---|---|---|---|
| sonnet-17 B1 | A cancelled `blockdev-snapshot-sync` leaves an untracked overlay that survives deletion | **Fixed** | 16 rounds, 32+ stops cut by `kill -9` of the server, plus ~14 more restarts. 0 overlays, records or qemu processes after every delete (Part 1). Code: `qemu.go:578-583`. |
| opus-19 N1 | A node restart mid-upload strands a stopped sandbox on that node | **Fixed** | A (SIGTERM) and B (kill -9): cross-node resume and fork both worked with data intact. D: node down, then back, the resume completed. `stranded.go` `uploadStranded` and `RequestUpload` do it. |
| opus-19 N2 | A forced stop after node loss keeps an unstored `live` head | **Fixed** | C: forced stop, then resume on the other node in 4 s from the newest stored layer. G: a plain stop with the node down did the same. `fallBackToStoredHead`. |
| opus-19 (retry hint) | Waiting on a dead node instead of a clear answer | **Fixed** | D: immediate 409 `snapshot_unavailable`, text says to retry when the node is back. |
| opus-18 B1–B4, sonnet-16 B1, m-a/m-b | Delete, fork, resume, move mid-upload; server restart during stop | **Hold** | E, F, the deletes in Part 1, and the stop-retry path in every round. |
| Delete while a node is down | Deletion waits and completes after restart | **Holds** | E. |

## Blocking findings

### B1. A resume while a layer's parent row is missing answers a false-final `resume_failed` (`internal/server/lifecycle.go:33-45`)

**Boat:** a stopped sandbox comes back with `resume` (`snapshots__stop.md`). The task's bar here: an error is either retryable with a hint or truly final.

**Repro (live, 3 of about 12 stop-in-window attempts with a periodic layer in flight):**

1. A sandbox on nA with 100–900 MB written. Run `stop` and `kill -9` the server 0.1–0.4 s later, then restart it.
2. A periodic or pre-flight layer was taken while the server was away. The node's report (`reportSnapshot`, `transfer.go:204-215`) failed, so the layer has **no row on the server** until its upload ends (`onSnapshotUploaded`, `snapshots.go:224-253`). The stop's own layers were recorded later, with that layer as their parent.
3. `sqlite3`: `select … from snapshots c where parent_id not in (select id from snapshots)` returns the live and stop layers.
4. While the parent uploads (3 s to 50 s on my 40 MB/s link), `resume` returns:
   ```
   {"code":"resume_failed","error":"The sandbox's disk cannot be restored: snapshot snap_be6k… (or its parent) no longer exists. Deploy a saved snapshot or create a new sandbox.","event":"error","status":409}
   ```
   `chainRestorable` treats the missing row as "purged" (`missing := err != nil || …`, line 38) and sets `retryable:false`. An agent that reads it will abandon a healthy sandbox.
5. After the upload ended the chain was whole. The same resume worked and the data matched. A node `kill -9` while the gap was open (3 of 3 in the one setup that I could time) also healed: the restarted node's parent-first upload recorded the missing layer. So this is a wrong answer inside a window, not data loss.

**Why supportable on a LAN:** pure bookkeeping.

- Cheapest fix: in `chainRestorable`, treat a row that is missing but referenced as some row's `parent_id` as "still arriving": answer a retryable 409 (`snapshot_unavailable`, `retryAfterSeconds`), as `chainReachable` already does for offline holders.
- Better fix: make the node re-report the layer once it reconnects (`transfer.go:211` runs `waitConnected` but ignores the error of a report that fails because the connection has not noticed the restart yet; retry it).

**Scope.** It needs a server restart or outage at the moment a periodic or pre-flight layer is reported, then a resume before that layer lands. It is rare in normal use, but it is exactly the restart-in-the-window case this task asks about. It is also an error that tells an agent to throw data away. I judge it blocking, but narrow.

### B2. A server restart during an upload leaves a half-received plaintext blob for good (`internal/server/store/blobs.go:62-66`, `127`; `internal/server/deletion_ops.go:310`)

**Boat:** deletion must erase the data (`data-retention__delete.md`). The bar for this task: deleted sandboxes leave nothing on the server or nodes.

**Repro (live; 21 files, 2.4 GB at the end of the run):**

1. Any snapshot upload in flight (the periodic ticks and every stop produce them). `kill -9` the server, or `kill -TERM` it, then restart.
2. `ls -A server/blobs/snapshot` (plain `ls` hides them) shows `.upload-<random>` files of 0.1–314 MB with the mtime of the kill. The SIGTERM case: a 100 MB file stayed from 01:58:23 even though the node re-uploaded the same layer and it was stored (`snap_h8nyyjpx`).
3. I deleted every sandbox and both saved snapshots. Rows 0, named blobs 0, node dirs empty, **but the 21 `.upload-*` files were still there**.

**Cause.** `Blobs.Put` writes to `CreateTemp(…, ".upload-*")` and relies on `defer os.Remove` (`blobs.go:62-69`). That runs only when the handler returns. Killing the process, or exiting on SIGTERM before the handler's defer, skips it. `Blobs.List` skips dot-names (`blobs.go:127`), so `sweepOrphanBlobs` never sees them either. Nothing else removes them.

**Why supportable on a LAN:** pure server bookkeeping. Remove `.upload-*` files on start (nothing can still be writing them), or have `sweepOrphanBlobs` erase dot-temps older than `orphanBlobAge`. A test: put a `.upload-x` file in `blobs/snapshot`, run the sweep, assert it is gone.

**Scope.** It needs a server restart during an upload. It loses no data and the layer re-uploads fine. But the leftover never goes away, it grows by up to a layer per restart, and it holds guest data after the owner deleted it. That fails the stated delete guarantee, so I count it as blocking even though the trigger is a restart.

## Minor (not blocking)

- **m1.** The CLI's `--json` error for `snapshot_unavailable` shows `code`, `error`, `status` only. The server body has `retryable` and `retryAfterSeconds` (`stranded.go`), but the CLI drops them. The message text says what to do, so this is cosmetic.
- **m2.** A plain `stop` on a sandbox whose node is offline completes at once as `forced:true` (G). That matches Boat's "last good snapshot" rule and is fine. The sandbox history says `forced`, not "node offline".
- **m3.** `resume --node nB` while nB is full waits in `provisioning` without end. I did this to myself twice; it took 6 minutes before I noticed. It is a wait for capacity, not an error, and the CLI says so.
- **m4.** A stop whose reply the server missed produces a stop layer recorded with `kind=periodic` (opus-19 m2). Unchanged and harmless.
- **m5.** A long-lived sandbox on nB left about 30 `bx_*-*.qcow2` overlay files between periodic ticks. All were erased on delete. I did not check how fast compaction removes them while the sandbox lives.

## Disputed or accepted classifications

- **My first run showed a "DATA MISMATCH" for one sandbox (round 1, `bx_fyvv24m3`).** It was my harness. I resumed it on the other node, the first `exec` timed out at 30 s while the lazy chain download of a 1 GB layer was still running on the throttled link, and my script then deleted it, which cancelled that download (`404 blob not found` in the node log). The resume was `ready` and nothing was lost. I changed the script to retry `exec` and re-read; no mismatch appeared again.
- **`save-a` deploy "missing n1".** My harness compared against a later file list. `save-a` was taken before `n1` existed. The earlier check of the same deploy (3 files `g1 g2 h1`) matched.
- **Plain `ls` hides `.upload-*`.** Earlier reviewers' blob audits may have used it. I used `ls -A` and `find`.
- **Data after the last stored layer is lost when a node dies** (C, G). Accepted: that is Boat's own forced-stop rule, and unavoidable on a node that is gone.

## Not verified

- `make e2e` (not allowed). The e2e run22 log ends `EXIT=0`, but it started before the last Go edits of 00:20–00:40, so I did not match it to this tree.
- ZDR users, `nodes remove`, Windows nodes, the dashboard in a browser, account close/erase, live model turns.
- A real 1 Gbps link without the throttle.
- Whether B1 also hits a `fork` or `new --from` of a stopped sandbox in the same window. `chainRestorable` is called at `lifecycle.go:1268` (resume); I did not check the other callers.

## What would flip this to AGREE

1. **B1.** After a server restart that makes a node's report of a periodic or pre-flight layer fail, no resume, fork or save of that sandbox returns a non-retryable `resume_failed` while the layer is still arriving. The answer is a retryable 409 with a hint, or the node re-reports after reconnecting. Add a servertest: a stop layer whose parent row does not exist yet, then a resume.
2. **B2.** After a server restart (SIGTERM and kill -9) in the middle of an upload, followed by deleting every sandbox and saved snapshot, `ls -A server/blobs/snapshot` is empty. Add a store or sweep test with a stray `.upload-*` file.

Everything else in the task's checklist held in my runs.
