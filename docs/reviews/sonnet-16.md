# Review sonnet-16: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent, no sub-agents). Date: 2026-11-10. Tree: uncommitted working tree on `master`, binaries rebuilt at 21:24 from the current tree (`deletion_ops.go` 21:09, `snapshots.go` 21:10, `nodes.go` 21:06). I did not carry over any earlier verdict.

## Verdict: DISAGREE (one blocker)

The opus-17 blocker is fixed, and I could not break the new deletion, upload and sweep code. Several sandboxes went through stop/resume cycles across two nodes with 15 s periodic snapshots. I also took saved snapshots, deployed from them, forked, deleted some sandboxes while others ran, and restarted the server and the nodes in the middle. At the end every survivor resumed with its data, every saved snapshot deployed, and a layer-by-layer audit found nothing left from deleted sandboxes.

One restart case fails, though. **Restarting the server while a stop is in flight can leave the sandbox stuck in `ready` with no machine behind it.** The node has already stopped the VM and saved the data, but the server sees the retry fail with `vm not found`, takes that for a failed save, and "refuses the stop" for good. It happened on 2 of 2 attempts that hit the window. Only `crowdvm stop --force` frees it.

## Method

1. **Read.** `opus-17.md`, the deletion and sweep code (`deletion_ops.go`, `snapshots.go` GC / `onSnapshotUploaded` / `reconcileNodeSnapshots` / `sweepOrphanBlobs`, node `transfer.go` upload and `uploading` tracking, qemu `store.DeleteSnapshot`), `lifecycle.go` stop and resume, `lifecycle_pause.go`, `feat_retention.go`, and the retention and node-owner docs.
2. **Build and tests.** `make build` and `go vet ./...` are clean. `go test -count=1 ./...` is all `ok` (e2e skips without `CROWDVM_E2E`). I did not run e2e. The other reviewer's run21 log in the scratchpad ends `EXIT=0`. I did not check which tree it ran on.
3. **Own cluster** in `~/.local/share/review-sonnet-16`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated and ports 9970–9973 only.
   - One `crowdvm-server` on 9970.
   - Nodes nA1 (UI 9971) and nA2 (UI 9972): qemu/KVM, 4 vCPU, 8 GiB, `--snapshot-interval 15s`, image `ubuntu-24.04-crowdvm-minimal-v3`.
   - A third node nA3 (UI 9973) for the `nodes remove` test.
   - Users `admin` and `bob` (ZDR).
4. **How I judged leftovers.**
   - Every sandbox wrote a unique marker line into `/home/user/gen.txt`, plus 30 MB of random data per generation. After each step I compared the guest file with what I had written.
   - For deletions I grepped for the markers across both nodes' `vm/` dirs and the server's `blobs/snapshot`. A positive control confirmed the markers are found where they should be.
   - I compared server blob files, node snapshot records and DB rows. I recomputed independently which layers are still reachable from non-deleted rows, saved snapshots and heads.
5. **Teardown.** I deleted all sandboxes and saved snapshots, then stopped the server and the nodes by PID. No process is left that uses my dir, and 9970–9979 are free. I deleted the data dir. I touched no other process.

## Fix status (opus-17 B1 and m5)

| # | Claim | Status | Evidence |
|---|---|---|---|
| B1a | Stop, then delete at once, with data still uploading | **Fixed (live)** | 1.5 GB random data, `stop`, wait for `archived`, then `delete --no-wait`. The op showed `processing` / `waiting_for_uploads` ("1 snapshot(s) of it are still uploading from their node"), then `completed` about 4 s later. Afterwards: 0 server blobs, 0 node records, 0 overlays, 0 marker hits. A 300 MB run also left nothing. |
| B1b | ZDR stop | **Fixed (live), 2 of 2** | Bob enabled ZDR. Each sandbox got 800 MB plus a marker, then `stop`. Both ops were `completed` (reason `zdr`). After 10 s: 0 blobs, 0 node records, 0 marker hits. |
| B1c | Delete a running sandbox while it uploads | **Fixed (live), indirectly** | I deleted running sandboxes that had periodic layers every 15 s (s1, s3, d3, f1, d4, d5), 6 times. Every op was `completed` and the final audit was clean. I never caught an upload mid-flight for this exact case. `trackDeletedUpload` / `onSnapshotUploaded` (`deletion_ops.go`, `snapshots.go`) read correctly, and `retention_uploads_test.go` covers it. |
| B1d | Uploads that land for deleted data are refused (410) or erased | **Holds** | After 10 deletions the final audit had no server blob without an unpurged row and no node record without one. |
| m5 | `nodes remove` | **Works, with a note** | See the node-removal section. |
| tests | New tests for the above | Present | `retention_uploads_test.go` (the waiting stage, removal) and the deletion cases in `lifecycle_fix_test.go` pass. |

## Long-lived pool scenario

All of this ran on nA1 and nA2, with the server and nodes restarted partway through.

| Step | Result |
|---|---|
| 4 sandboxes (2 per node), 8 writes, 15 s periodic layers | Pass. Periodic layers were merged and uploaded. |
| Stop all, resume all, then again with `--node` crossing nodes | Pass. Data exact each time. |
| A queued resume (asked for a full node), then `stop`, then resume elsewhere | Pass. Data exact, no empty boot. |
| `crowdvm snapshot s1 snap-a`, `new --from snap-a`, `fork s2` | Pass. The fork got the source's data, the deploy got the saved data. |
| Delete the fork's source s2 while the fork runs and the server restarts | Pass. The fork kept its data and its layers survived. The op went `completed` after the restart. |
| Delete s1 (the source of saved snapshots `snap-a` and `snap-b`) | Pass. Both snapshots deployed afterwards. A second deploy of `snap-a` also worked. |
| Node restart (SIGTERM), VM running | Pass. The VM survived and the node reconnected. |
| Stop, then SIGKILL the node mid-upload, restart, resume | Pass. Data exact. The stop layer's interrupted upload was not retried. The next periodic layer is uploaded, and a later stop and cross-node resume worked. |
| Stop 4 sandboxes at once with the server killed 1 s later | 3 of 4 archived. **1 stuck, see B1.** |
| Simultaneous restart of server and both nodes with 4 running | Pass. All 4 intact. |
| Final audit | 36 unpurged rows = 36 server blobs. Every node record has a row. The 16 deleted-but-unpurged rows are all reachable from non-deleted rows (see below). 0 marker hits for 9 deleted sandboxes. |
| After deleting everything left plus both saved snapshots | 0 blobs, 0 node snapshot records, 0 overlays, 0 locations, all 16 deletion ops `completed`. |

The 16 unpurged rows that carry a `deleted_at` are intentional. Each live sandbox keeps a `live` and a `stop` row per stop cycle, and GC keeps whatever those rows chain to. Boat expects older snapshots to expire after about a day (`snapshots__download-and-delete.md:101`). CrowdVM's `retState.run` (`feat_retention.go:521`) does that once retention mode is on. This is not a leak.

## Node removal (live)

x1 ran on nA3 and x2 was stopped there, with data uploaded. `nodes remove nA3 --force --yes` replied `snapshotCopiesLeft: 0`. x1 went to `error` / `node_offline`, and x2 stayed `archived`. Both resumed with their data on nA1/nA2, x1 after `stop --force` with its last periodic layer. The doc note in `node-owners.md:139-142` matches what the server does.

I noticed two things:
- The removed node's app keeps running. It gets 401s on reconnect but does not stop its VM. x1's VM went on taking a snapshot every 15 s there (26 layers by the time I killed the node). The data of a removed node is the owner's to wipe, as documented. Having the node stop and wipe its VMs after repeated 401s on connect would be a nice-to-have. It is not a Boat feature.
- A sandbox created with `--node nA3` keeps that placement. After nA3 was removed, a plain `crowdvm resume` fails with `no_ready_machine` ("No machine is ready right now") even though other nodes had room. `resume --node nA2` works. The message should say the pinned node is gone. `--node` is a CrowdVM extension, so this is minor.

## Spot-check of the agent flows

All ran on the cluster above: `new`, `exec`, `push` (a directory with files, then `cat` in the guest), `host 8080` (the URL is protected and answers with the token notice), `snapshot ID NAME`, `new --from`, `fork`, `stop`, `resume`, `delete` (including `--no-wait`) and `snapshot rm`. All behave as documented.

## Blocking finding

### B1. A server restart during a stop can leave the sandbox stuck in `ready` with no machine (`lifecycle.go:889`, `lifecycle_pause.go:79`; Boat `snapshots__stop.md:129`)

**What Boat promises.** A stop saves the disk first. If the save fails, boat refuses the stop and retries. The sandbox keeps running because its work is still on its disk. That is a safeguard for a real failure of the save.

**What happens (live, 2 of 2 times it hit the window).**
1. `crowdvm stop` is accepted. The server sends `POST /v1/vms/{id}/stop` and the node stops the VM and stores the stop layer (`qemu: bx_mms37rjw stopped as snap_ff95… in 33ms`).
2. The server process is killed and restarted before it records the result.
3. The server retries the stop (`stop attempt failed … context canceled`, then `node_offline`). The retry runs `preflightSave`, which calls `POST /v1/vms/{id}/snapshot` on the node. The node still has a record for the stopped VM, so it answers **HTTP 500** `node_error: "<id>: vm not found"`, not 404.
4. `preflightSave` only treats a 404 as "no machine" (`lifecycle_pause.go:79`). With a 500, `runStop` calls `refuseStop(…"save_failed: …")` (`lifecycle.go:889-890`). The sandbox goes back to `ready` and the reaper retries the same call every few seconds, forever.

**Observed.**
- Run 1 (the 4-way stop): d3 stayed `ready` / `health ok`. `crowdvm exec` returned 409 and `crowdvm resume` printed `ready` and then hung until my timeout. Server log: `stop refused: the save failed; the sandbox keeps running … vm not found`, repeated every 5–60 s. The data was already on the server as a recorded layer.
- Run 2 (a single sandbox, server killed 0.3 s after `stop`): the same state, the same log lines. `exec` says "the sandbox is stopped: crowdvm resume", while the server still says `ready`.
- `crowdvm stop ID --force --wait` clears it (`archived`). The sandbox then resumed with all its data both times. That recovery only works because the stop layer had already been recorded. A user who forces it before then "loses everything written after the last good snapshot", which is Boat's wording for `--force`.

**Why this is supportable on a LAN.** It is pure control-plane bookkeeping.

**Fix options.**
- Make the node answer 404 for `POST /v1/vms/{id}/snapshot` on a stopped VM, as it already does for an unknown record. Or treat "vm not found" like 404 in `preflightSave`.
- Better: before the preflight, ask the node whether the VM is running. If it is stopped with a stop layer, adopt that layer and finish the stop.

**Tests to add.** A servertest with a fake node that answers the preflight with 500 `vm not found` after a completed stop, asserting the stop ends `archived`. An e2e case that restarts the server right after `stop`.

## Minor (not blocking)

- **m1. Pinned placement after node removal.** See the node-removal section.
- **m2. A removed node's app keeps its VM running.** See the node-removal section.
- **m3. Interrupted stop-layer upload.** After a SIGKILL of the node mid-upload, the stop layer `snap_6ksw…` stayed `uploaded_at NULL` and was not retried. The next layer's upload and a later stop made the chain whole, so I saw no data loss. A restarted node could re-queue unfinished uploads on connect.
- **m4. `stop --wait` under ZDR ends with `404 not_found`.** The stop and the delete both worked. This was already known.
- **m5. Test coverage.** No test pins B1, and no e2e case restarts the server mid-stop.

## Disputed or accepted classifications

- **Deleted rows still unpurged.** I accept these when a live row chains to them (see above). I checked that the set of unpurged rows exactly equals what is reachable from non-deleted rows and saved snapshots.
- **`waiting_for_uploads` is shown as `status: processing`, `expectedBy: null`.** Boat documents the stage; the missing `expectedBy` estimate is cosmetic.
- **`nodes remove` does not wipe the removed machine.** The server cannot reach it, and the guide says so. Accepted.
- **Account erase.** Not run (needs a browser session), as in opus-17. It uses the same deletion code as ZDR.

## Not verified

- A delete that lands exactly while an upload is in flight for a running sandbox (B1c). The path is covered by code reading and `retention_uploads_test.go`, not by a live catch.
- `make e2e`. I was not allowed to run it, and I did not match run21 to this tree.
- Windows nodes, the dashboard in a browser, CI integrations, live model turns.

## What would flip this to AGREE

1. **B1.** After the server restarts at any moment during a stop, the sandbox ends `archived` (with the stop layer recorded) or still running, never `ready` with no machine. `crowdvm stop --wait` then succeeds without `--force`. Add the servertest and the restart-mid-stop e2e case.
2. Optionally, the m1 message and m3 upload retry.
