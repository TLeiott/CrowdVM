# Review sonnet-17: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent, no sub-agents). Date: 2026-11-10/11. Tree: uncommitted working tree on `master`, built with `make build` at 23:03 (no Go file changed afterwards). I did not carry over any earlier verdict.

## Verdict: DISAGREE (one narrow blocker)

All four opus-18 blockers (B1–B4) and sonnet-16's B1 are fixed in live runs on a 40 MB/s link. The throttled long-lived-pool session passed every check: cross-node stop/resume, saved snapshots and their deploys, forks onto the other node, deletes while uploads ran, and restarts of the server and of a node mid-upload. Every survivor resumed with its data and every saved snapshot deployed. No error appeared that an agent would misread as permanent.

One thing fails the "deleted sandboxes must leave nothing on the nodes" bar. **A server restart during the stop pre-flight snapshot leaves an untracked qcow2 overlay on the node. It survives the sandbox's deletion, and the delete op still reports `completed`.** It holds guest data in plaintext (745 MB in my repro) and is erased only when the node app restarts. That is the same acceptance rule that opus-17 B1 and opus-18 B4 used, and it fails on a new path. The trigger is a restart at the wrong moment, and the cause is node bookkeeping, so it is supportable on a LAN.

## Method

1. **Build and tests.** `make build` is clean. `go vet ./...` is clean. `go test -count=1 ./...` is all `ok` (`internal/server` 46 s; e2e skips without `CROWDVM_E2E`). I did not run e2e.
2. **Own cluster** in `~/.local/share/review-sonnet-17`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated and ports 9970–9975 only (9970 server, 9971/9972 node UIs, 9975 proxy).
   - One `crowdvm-server`.
   - **A Go TCP throttling proxy** (`scratchpad/proxy/main.go`) between the nodes and the server at 40 MB/s per connection, both directions. The nodes joined through it, and the server's `--public-url` points at it. The CLI talked to 9970 directly.
   - **Proxy bug I fixed mid-run.** My first version let a long-idle keep-alive connection accumulate credit and burst at about 460 MB/s. I noticed it from a 421 MB upload that finished in 1 s, replaced the proxy with a strict pacing one, and **redid every timing-sensitive test** afterwards. Only the post-fix results below count. I measured a 582 MB image push at 14 s and a 422 MB layer at 14 s. Two layers uploading at once shared the link: about 39 s for 2 × 458 MB, so the effective limit is per node.
   - Nodes `nA` and `nB`: qemu/KVM, 4 vCPU, 8 GiB, so two `small` sandboxes each. `--snapshot-interval 20s`. Image `ubuntu-24.04-crowdvm-minimal-v3`.
3. **Judging data.**
   - Every sandbox wrote blobs of random data. A guest-side sha256 was recorded on the host, plus a marker line per blob. After every move, stop/resume, fork or deploy I recomputed all hashes inside the guest (`chk`).
   - "Upload in flight" was detected from the node log (an upload id started but not finished since the node's last start). I counted ids, not lines, after a first version was fooled by an upload cut short by a `kill -9`.
   - Audits compared server blobs with unpurged rows, node snapshot records with rows, and node `vm/disks` and `vm/vms` with the live sandboxes.
4. **Teardown.** I deleted every sandbox and both saved snapshots, then SIGTERMed the nodes, the server and the proxy by saved PID. No process referenced my dir afterwards, and 9970–9979 were free. I removed the data dir. I touched no other process; I saw the other reviewer's e2e cluster in `pgrep` and left it alone.
   - One slip: I stopped a background shell of mine with `TaskStop`, and that also killed my own server (it was a child of that shell). I restarted it. This does not count as a product test.

## Fix status

| # | Claim | Status | Evidence (live, 40 MB/s) |
|---|---|---|---|
| sonnet-16 B1 | Server restart during a stop leaves the sandbox `ready` with no machine | **Fixed** | 13 trials: kill -9 of the server 0.1–1.2 s after `stop`, with 30–400 MB written and synced first. In at least 8 of them the node had stopped the VM while the server was down. Node log: `stop: <id>: vm not found`, and the retry got a 404. Every run ended `stopped` (the API name for archived) within 7–40 s. After a same-node resume, and after a cross-node resume (`--node` the other node, 3 of 3), all hashes matched. The fix is `writeErr` in `internal/node/api.go:254`, which maps "vm not found" to 404. The node's unreported stop layer is later adopted as a layer of the sandbox when its upload lands. See the notes after this table. |
| opus-18 B1 | Delete during a periodic upload purged layers that a saved snapshot needs | **Fixed** | 400 MB written. After "transfer started", `snapshot S tplx --no-wait`, then `delete S` 1.2 s later (done twice; the second time on a fresh sandbox with a fully throttled link). The delete completed. `tplx` and `tply` went `ready` in 10 s. `new --from` on the other node ran with all blobs intact, twice for each snapshot. |
| opus-18 B2 | `stop --wait` then `resume` during an upload gave a non-retryable `resume_failed` | **Fixed** | Upload in flight, `stop --wait` returned in 2 s, `resume --node <other>` waited 52 s (it waited for the 860 MB stop layer at 40 MB/s) and returned ready with all hashes right. No error. |
| opus-18 B3 | Cross-node fork during an upload gave `snapshot_not_found` | **Fixed** | `fork --node <other> --wait` during an upload: ready after 23 s with all blobs (2 of 2). |
| opus-18 B4 | The old node kept a layer after a move during an upload; delete said `completed` | **Fixed for this path** | A sandbox moved A→B during an upload on A, then was deleted. Afterwards A and B had 0 overlays and 0 records for it, and the server had 0 rows. See the new blocker for a different path that leaks. |
| opus-17 B1 | Delete waits for uploads, then erases | **Holds** | x3 (a running sandbox, delete during an upload): op `processing / waiting_for_uploads` for 39 s, `completed` within 1 s after the upload ended. 0 rows, 0 records, 0 overlays. |
| Delete + node `kill -9` mid-upload | Op waits for the node | **Holds** | x4: op stayed `waiting_for_uploads`, then `completed` 2 s after the node restart, nothing left. |
| Delete + server restart mid-upload | Op survives | **Holds** | x5: `completed` 19 s after the restart, nothing left. |
| Server restart mid-upload | Node re-uploads | **Holds** | A layer cut at 99 MB was re-sent after the node reconnected. |
| Node `kill -9` mid-upload | VM adopted, layer re-sent | **Holds** | The VM was adopted, the layer re-uploaded after 20 s, and a stop and cross-node resume worked. |

**Notes on the sonnet-16 B1 fix.**
- After the retry gets 404 it ends the stop as `finishStop(..., forced=true)` on the last server snapshot (`lifecycle.go:920`). The node's own stop layer is not reported then. The server learns it when its upload lands, and records it as a `periodic` row (`onSnapshotUploaded`, `snapshots.go:218`). Its parent is the right chain, and the head moves once it is stored.
- This depends on that upload finishing. Every trial resumed correctly, including 400 MB runs with an immediate cross-node resume. In the windows I hit, the stop layer was a tiny delta on top of the pre-flight layer. If a resume lands in the gap between "stop ended without the layer" and "layer uploaded", the data of the stop layer alone (the guest's last writes during prepare-stop) could be missed. That is narrow and I saw no loss, so it is not a finding.
- Cosmetic: the op is marked `forced`, and the node's driver state for the stopped VM shows `error` with `stop: vm not found`.

## Long-lived pool session (throttled)

| Step | Result |
|---|---|
| Sandboxes on two nodes, 20 s periodic layers, 40–400 MB written per step | Pass. |
| Stop and cross-node resume with an upload in flight (twice), and swapped pairs of four sandboxes at once (resumed with `--node` the other node) | Pass, all hashes right. |
| Four sandboxes stopped at once, server `kill -9` 1.5 s later, then all four resumed across nodes | Pass, all hashes right. |
| Fork onto the other node during an upload (twice) | Pass. |
| Saved snapshots (two) while uploads ran, with the source deleted afterwards, then deploys (twice each, on the other node) | Pass. |
| Delete of a running sandbox during an upload (x1, x3), and with a node kill (x4) or a server restart (x5) | Pass. |
| Node restart (`kill -9`) mid-upload, VM running | Pass. |
| Final audit before the final deletion | 267 unpurged rows = 267 server blobs. Every node snapshot record has a live row. |
| After deleting all sandboxes and both saved snapshots | 0 blobs, 0 unpurged rows, 0 open ops, 0 node snapshot records, 0 qemu processes. `vm/disks` held only the base image, **plus the leak below**. |

## Blocking finding

### B1. A cancelled live snapshot leaves an untracked overlay on the node that survives deletion (`internal/node/vm/qemu/qemu.go:564-587`)

**Boat:** deletion must erase the data (`data-retention__delete.md`), and this task's bar is that deleted sandboxes leave nothing on the nodes.

**Repro (live; 3 of about 17 server restarts that fell into the window):**
1. Sandbox on nA with a heavy guest write in flight: `dd if=/dev/urandom of=big bs=1M count=1500` in the background.
2. `crowdvm stop ID`, then `kill -9` the server 0.2 s later, then restart it.
3. Node log: `qemu: <id> sync before snapshot: … context canceled`. The pre-flight snapshot was cancelled midway.
4. At that moment the node's record said the active overlay was `…-d6efacb7.qcow2`. The qemu process had a newer overlay `…-7afd7501.qcow2` open: the cancelled `blockdev-snapshot-sync` had still run in qemu.
5. The retry stop, a same-node resume and a cross-node resume all worked, and the 1.5 GB file hashed the same everywhere. No data was lost. The later merges folded the overlay into the chain.
6. `stop`, then `delete`: the op reported `completed`, the server had 0 rows and 0 blobs, **yet `nA/vm/disks/bx_…-7afd7501.qcow2` (745 MB of the sandbox's plaintext data) was still there.**
7. The same happened earlier with the first sandbox. Two small files (393 KB and 655 KB) from two cancelled pre-flight snapshots stayed on nA after its delete.
8. Restarting the node app removes them (`gcOrphans`). A node that stays up keeps them for as long as it runs.

**Cause.** `Driver.Snapshot` sends `blockdev-snapshot-sync` with a context that the server's disconnect cancels. `r.q.cmd` returns the context error while qemu carries on and switches overlays. The error path deletes `newRel` and leaves `rec.Disk` on the old overlay. That does three things:
- It orphans the new overlay file, which no record or snapshot owns.
- The VM keeps writing into a layer the node does not track until the next snapshot or merge.
- The orphan is never reached by deletion or by the periodic GC.

**Why supportable on a LAN.** Pure node bookkeeping. Either run the QMP command with `context.WithoutCancel` (the code already does this for the thaw right after it), or ask qemu which overlay is active (`query-block`) after a context error and adopt or remove it. Either way, have deletion (and `gcOrphans` on a timer, not only at start) erase `disks/<id>-*.qcow2` files that no record references.

**Impact.** Narrow: a server restart or a dropped connection at the instant of a snapshot. I saw no data loss. But it breaks a stated delete guarantee, and an overlay that is open in qemu but unknown to the node has no protection if the node (not just the app) goes down before the next snapshot, so I judge it worth fixing.

## Minor (not blocking)

- **m1.** The `waiting_for_uploads` text names a count ("1 snapshot(s)…") but shows no estimate. Cosmetic.
- **m2.** Placement: two parallel uploads from one node share a 40 MB/s link, so a deletion can wait about 40 s on a throttled link. Expected, and the op explains itself.
- **m3.** Stop ended through the 404 path is recorded `forced`, and the node shows the stopped VM as `error` until the next resume. Cosmetic.
- **m4.** Not re-tested: an interrupted stop-layer upload after a node `kill -9` is re-sent only by the next layer's parent logic (sonnet-16 m3). I saw the data intact.

## Disputed or accepted classifications

- **Deleted rows still unpurged while a saved snapshot needs them** (the source's layers on the old node after "delete s1"). Correct retention. They were all erased when the saved snapshots were removed; the final audit shows 0.
- **Slow cross-node resume or fork on a throttled link.** The CLI waits (52 s, 23 s). That is a wait, not an error. Accepted.
- **`stop` state name.** The API says `stopped`, not `archived`. Matches the docs.

## Not verified

- `make e2e` (not allowed). The e2e run21 log ends `EXIT=0`, but I did not match it to this tree.
- A resume that lands in the narrow gap after a stop that ended through the 404 path, before its stop layer uploaded.
- Server SIGTERM (not just `kill -9`) during a stop; ZDR stop; account erase; `nodes remove`; Windows nodes; the dashboard in a browser; live model turns.

## What would flip this to AGREE

1. **B1.** After a server restart or disconnect at any moment during a live snapshot (`stop`, `snapshot`, periodic), no overlay is left on the node that no record owns:
   - the node adopts or removes the overlay that qemu switched to, and
   - a delete reports `completed` only when `vm/disks` holds nothing of the sandbox, or GC erases orphan overlays at runtime and the deletion op waits for it.
2. A test for it: a node test with a QMP stub that executes `blockdev-snapshot-sync` after the context is cancelled, asserting the disk ends tracked or erased. Optionally one e2e step that restarts the server right after `stop` on a sandbox with dirty data and then deletes it, asserting `vm/disks` has no file of that sandbox.
