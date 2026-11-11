# Review sonnet-20: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent, no sub-agents). Date: 2026-11-11, 04:12–05:50. Tree: uncommitted working tree on `master`. I did not carry over any earlier verdict, including my own sonnet-19.

**The tree changed while I was testing.** I built at 04:13 and ran my first 19 rounds on that build. The team then edited `internal/server/lifecycle.go` at 05:13. I rebuilt at 05:29, rebuilt my cluster and re-ran the node-restart rounds and the pool pass on that build. I judge the product as it is now, which is the 05:29 build. The 04:13 build results are kept as evidence for the fix-status table.

## Verdict: DISAGREE (one narrow blocker)

Both sonnet-19 findings are fixed on the stop/resume path. On the current build I saw no lost synced write and no false-final error on `resume`. One related defect remains:

- **B1. `fork` of a stopped sandbox whose layers are still uploading answers a false-final `snapshot_data_expired`** (`internal/server/lifecycle.go:1429`). I reproduced it live. In the same state `resume` correctly answers a retryable `snapshot_unavailable`. sonnet-19 N1 named this fork caller, and the fix did not cover it. It is a one-query change, and it breaks the rule "no error may claim data is gone while it is intact".

On the 04:13 build I also found a real data loss in the node-restart window. The 05:13 edit fixes it, and I confirmed that on the new build (see N2 in the table). I list it as fixed, not as a blocker.

## Method

1. **Build and tests.** `make build` and `go vet ./...` are clean. `go test -count=1 ./...` is `ok` in every package (`internal/server` 48.7 s). The e2e package only skips without `CROWDVM_E2E=1`. I did not run `make e2e` or any e2e cluster.
2. **Own cluster** in `~/.local/share/review-sonnet-20`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated, ports 9970–9975 only (9970 server, 9971/9972 node UIs, 9975 proxy). It was built twice: once on the 04:13 build, once on the 05:29 build.
   - One `crowdvm-server` with `--snapshot-interval 20`, `--public-url` pointing at my proxy.
   - My own Go TCP proxy pacing each connection and direction to 40 MB/s (`scratchpad/proxy/main.go`). The nodes joined through it. A 582 MB image push took 15 s and a pull 15 s.
   - Nodes `nd1`, `nd2`: qemu/KVM, 4 vCPU, 8 GiB, 2 small sandboxes each, 20 s periodic snapshots, image `ubuntu-24.04-crowdvm-minimal-v3`.
   - Another reviewer's e2e cluster ran at the same time on the machine. I touched none of its processes.
3. **Round harness** (`scratchpad/round.sh`, `lib.sh`). Two long-lived sandboxes, one per node at first. Each round:
   - Write a 300 MB random file and a 4 KB marker with `dd` and `head`, record their sha256 in `sums`, `sync`.
   - Wait 1–3 s, then `crowdvm stop` on both.
   - After 0.05–0.5 s, `kill -9` or `SIGTERM` the server. Restart it at once.
   - In some rounds, `kill -9` or `SIGTERM` the node of sandbox 1 right after the server, then restart it 1 s later (the "node in the same window" cases).
   - Re-issue `stop` until `stopped`. Read `stops.forced` from the DB.
   - Resume on the other node (`other`) or the same node (`same`), with retries. Log every error. Grep for `snapshot_data_expired` and `resume_failed`.
   - Judge: `sha256sum -c sums` must be all `OK`, and the line count must equal the expected count (2 per round). A missing marker line is lost synced data. After a failed round I reset the baseline to the actual count so a loss is not counted twice.
4. **Teardown.** I deleted every sandbox and saved snapshot, stopped the nodes, server and proxy by saved PID, confirmed no process referenced my dir, no qemu of mine ran and 9970–9979 were free, and deleted the data dir. Logs are in `/tmp/claude-1000/-home-tl-dev-CrowdVM/1b28bfd1-410c-40cd-9760-3a76f6e8e38b/scratchpad/logs/` (04:13 build) and `.../logs2/` (05:29 build).

### Part 1: stop cut by a server (and node) restart

| Build | Rounds | Stops | Node restarted in the window | Result |
|---|---|---|---|---|
| 04:13 | r1–r9 | 18 | no | 18 of 18 pass. Two server kill modes, delays 0.05–1.2 s. The node answered the retried stop with the same layer, `forced=0`. |
| 04:13 | r10–r18 | 18 | yes (`kill -9` ×6, `SIGTERM` ×3) | 17 of 18 pass, **1 real data loss** (r13, see N2). |
| 04:13 | r19 | 2 | no | pass |
| 05:29 | r1–r9 | 18 | no (my first node-kill code was broken: it hard-coded the old node ids, so r1–r5 killed no node and r6–r9 had a bad node lookup) | 18 of 18 pass |
| 05:29 | r10–r14 | 10 | yes (`kill -9` ×4, `SIGTERM` ×1; delays 0.05–0.5 s; same and other node) | **10 of 10 pass**. r10 ended with `forced=1` on both (node killed before it finished the stop) and still kept the marker. |

Total on the 05:29 build: 28 stop/resume attempts, 28 pass, 0 missing marker lines, 0 `snapshot_data_expired` or `resume_failed` on resume. 16 or more rounds were done on each build. Round 1 of the 04:13 build was finished by hand after a harness error (wrong state name), and its check passed.

**N1 window.** I made 4 attempts to hit "stop in progress, server restart, no stored layer yet": a fresh sandbox aged 1–21 s, a 300 MB write, stop, server kill 0.05–0.3 s later, then `resume` and `fork` probes. In every attempt the head was already set when the stop ended, so I did not reach the window naturally. I then reproduced the exact state with the DB (`uploaded_at=NULL`, `head_snapshot_id=NULL` on a stopped sandbox, the same state the team's `TestResumeWhileFirstLayerUploads` builds). See B1.

### Part 2: long-lived pool pass (throttled, same sandboxes)

| Step | What I did | Result |
|---|---|---|
| Forks | Fork of a stopped sandbox, and of a running one, to the other node | both forks have all 28 lines, all `OK` |
| Saved snapshot | `crowdvm snapshot s2 savea` (4.7 GB), 50 MB more data, `new --from savea` on another node | deploy has 28 of 28 lines and none of the post-save file |
| Node restarts | `SIGTERM` of the node that held the layers, restart, resume on the other node; then `stop --wait`, `kill -9` of the node, restart, resume on the other node | 28 of 28 lines both times |
| Delete under node kill | Write 300 MB, `delete` the running sandbox, `kill -9` its node 0.3 s later, restart it 8 s later | deletion finished, 0 live rows |
| Cleanup | `snapshot rm savea`, delete the other sandbox | server: 0 unpurged rows, 0 locations, 0 blobs, 0 `.upload-*` files. Nodes: only the base image (1 disk, 0 snapshot files, 0 VM dirs, about 555 MB each), 0 qemu processes. |

## Fix status

| # | Claim | Status | Evidence |
|---|---|---|---|
| sonnet-19 N1 (resume) | A stopped sandbox whose layers are still uploading answers a retryable error, not `snapshot_data_expired` | **Fixed for `resume`** | `lifecycle.go:1257-1263` counts unstored live layers and answers `409 snapshot_unavailable`, `retryable:true`, `retryAfterSeconds:30`. Live (DB-state probe above): `snapshot_unavailable … still being uploaded from its node. Retry in a minute.` I never saw a false-final error on `resume` in 56 resume attempts across both builds. |
| sonnet-19 N1 (fork) | Same guard at the `fork` caller | **Not fixed** | `lifecycle.go:1426-1430` has no pending-layer check. Live: same DB state, `fork` answered `snapshot_data_expired … retryable:false`. See B1. |
| sonnet-19 N2 / opus-21 N1 | A stop cut by a server restart must not lose synced writes | **Fixed on the 05:29 build; the 04:13 build still lost data when the node restarted in the same window** | 04:13 build: r13 lost the marker (N2 below). 05:29 build: one layer id per stop is kept across attempts (`lifecycle.go:942-949`), and a 404 adopts exactly that layer (`nodeLayer`, `lifecycle.go:997-1014`). 10 of 10 stops pass with the node restarted in the window. Servertests `TestStopAdoptsNodeLayerAfter404` (with an unrelated leaf on the node) and `TestStop404IgnoresStaleNodeLayer` cover the new logic. |
| Head-selection race | A live layer stored after the stop ended is not adopted | **Not isolated** | I did not build a probe for this. In the 04:13 build, r13 shows the case (the live layer was stored 1.8 s after the forced stop ended and was not adopted). On the 05:29 build nothing like it appeared in 28 attempts, but I cannot say that the fix targets it. |
| Pool regressions (forks, saved snapshots, deletes, node restarts) | | **Hold** | Part 2 above. |

## Blocking findings

### B1. `fork` answers a false-final `snapshot_data_expired` while the layers are still uploading (`internal/server/lifecycle.go:1429`)

**Boat:** a forked sandbox copies the latest snapshot (`snapshots__fork` in the Boat docs, `docs/PARITY.md` fork rows). The task's bar: an error is retryable with a hint, or truly final.

**Repro (live, 05:29 build):**
1. New sandbox, `stop --wait`, so it is `stopped` with a stored head.
2. Put it in the state of a stop with its first layers still in flight: `UPDATE snapshots SET uploaded_at=NULL WHERE sandbox_id=…`, `UPDATE sandboxes SET head_snapshot_id=NULL WHERE id=…` (this is what `TestResumeWhileFirstLayerUploads` does).
3. `crowdvm resume ID --no-wait` answered `409 snapshot_unavailable`, "This sandbox's snapshot is still being uploaded from its node. Retry in a minute." (`retryable:true`). Correct.
4. `crowdvm fork ID` answered `409 snapshot_data_expired`, "The snapshot data of this sandbox no longer exists (its snapshots were deleted or expired under the retention policy)…", `retryable:false`. False: the layers exist.

**Cause:** `lifecycle.go:1426-1430`: `!live && src.HeadSnapshotID == ""` with `FirstBootDone` returns `snapshotDataExpiredErr` without the pending-layer count that `resume` does at `lifecycle.go:1257-1263`.

**Why supportable on a LAN:** pure bookkeeping. Move the count into a helper and use it in both callers, then add a servertest for the fork. No network or payment dependency.

**Scope.** It needs a stopped sandbox with no stored layer yet, for example a stop that a server restart cut in the first seconds of a sandbox's life. I did not reach that state naturally in 4 live attempts, so this is a narrow window. I still count it as blocking: it is the exact false-final claim sonnet-19 N1 asked to remove, it was named for the fork caller, and an agent that reads it abandons a healthy sandbox.

## Fixed after I found it (not blocking now)

### N2 (04:13 build only). A node restarted in the same window as the server lost synced data (`lifecycle.go` `nodeStopLeaf`, replaced at 05:13)

Round 13 of the 04:13 build (`bx_yr7kdh77`): a 300 MB file and a marker, `sync`ed 3 s before `stop`. The stop reached the node, which wrote the stop layer `snap_heazgn…` at 04:29:56.756. The server was killed at +0.5 s and the node 0.15 s later; both were restarted. The retried stop got `404`, and the old `nodeStopLeaf` adopted a node layer only when the node held exactly one leaf layer of that sandbox. The sandbox had moved between the two nodes every round, so the node held 9 leaves (checked from its `vm/snapshots/*.json`). The code fell back to the old head, `finishStop(…, forced=true)`, and `stopped` was reported. The live layer `9dp2hn…` was stored 1.8 s later and was not adopted. The resumed sandbox lacked the marker (24 of 26 lines). The node still held the stop layer.

This is the loss I was asked to look for ("restart the node in the same window"). It is fixed in the 05:29 build by keeping one layer id per stop and adopting exactly that layer after a 404, so a node with many old leaves no longer matters. I confirmed it with 10 of 10 clean stops, but see "What would flip" for the sample size.

## Minor (not blocking)

- **m1.** On the 04:13 build, after I deleted all sandboxes and the saved snapshot, both nodes still held about 4.7 GB of layer files of the deleted sandbox (25 disks and 24 snapshot files each). They stayed for 8–11 minutes and were erased within 25 s of a node restart. The server DB was clean. On the 05:29 build the same steps left only the base image on both nodes. I did not find out which change fixed it, so treat it as unexplained. The node's `DeleteSnapshot` keeps a deleted layer's file while something still builds on it (`internal/node/vm/qemu/store.go:309`).
- **m2.** `crowdvm resume` right after a stop that a restart cut is accepted while the sandbox reads `stopping`, and it starts after the stop ends (LIFE-014). It worked in every probe.
- **m3.** A `stop_failing` / `node_offline` notice appears for a few seconds after a server restart (unchanged from opus-20 m3).
- **m4.** In r10 on the 05:29 build, a plain stop ended `forced=1` because I killed the node during the stop. Nothing was lost. That flag is honest in this case.

## Disputed or accepted classifications

- **Data written after the last stored layer when the node is gone for good** stays lost on a forced stop. Accepted: Boat's forced-stop rule.
- **My 05:29 harness errors.** r1–r9 of that build did not kill a node (the node-id mapping was stale). I do not count them as node-restart rounds. r10–r14 are the real ones.
- **N2 and the 05:13 edit.** I did not carry my old verdict forward, but I also did not discard a real loss: the 04:13 build lost data, the 05:29 build did not in 10 tries. Both facts are in this report.

## Not verified

- A natural N1 window (no stored layer at the end of a stop). I used the DB state for B1.
- The head-selection race on its own (see the fix-status table).
- Sample size: on the 04:13 build the loss showed in 1 of 18 node-restart stops. 10 clean stops on the 05:29 build would still appear about 55% of the time if the fix did nothing. The code and the two new servertests make me believe the fix, but my live evidence is weak. More node-restart rounds would settle it.
- A real 1 Gbps link without the throttle; `make e2e` (not allowed); Windows nodes; the dashboard in a browser; ZDR; account close/erase; `docs/PARITY.md` row by row.

## What would flip this to AGREE

1. **B1.** `fork` of a stopped sandbox with no head and at least one unstored live layer answers the same retryable `snapshot_unavailable` as `resume`. Add a servertest next to `TestResumeWhileFirstLayerUploads`. I would accept the fix on the code and test alone; a live DB-state check like mine takes a minute.
2. Optional, would make the N2 fix firm: 10 more node-restart-in-the-window rounds with zero missing marker lines.

Everything else in the checklist held in my runs on the 05:29 build.
