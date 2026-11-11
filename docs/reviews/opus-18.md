# Review opus-18: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. I built it at 21:25, after the last source edit (`internal/node/transfer.go`, 21:23:40). No Go file changed while I tested. No sub-agents.

## Verdict: DISAGREE (four blockers, one root cause)

All three of opus-17's B1 cases are fixed live. Stop then delete, a ZDR stop, and delete during a periodic upload each show `waiting_for_uploads` until the upload ends. Each then completes with nothing left on the server or the node. Node restarts, a server restart, and an orphan-blob sweep with every blob aged to two hours kept all live data intact. m1 and m5 are fixed.

I ran every node through a proxy that limits each connection to 40 MB/s, the speed of a good Tailscale link or a busy LAN. A 400 MB layer then takes about 10 s to upload instead of under a second on loopback. That window exposes one design gap: **the server has no row for a periodic layer until its upload ends**, yet a stop, a save or a fork can build on that layer at once. Earlier reviews ran on unthrottled loopback, where the window is too short to hit by hand. Four failures follow from it:

- **B1. Deleting the source during a periodic upload destroys saved snapshots and forks built on it.** This is in the new fix code. The `ready` saved snapshot then fails to boot (fsck error).
- **B2. `stop --wait` then `resume` during a periodic upload fails** with non-retryable `409 resume_failed` ("disk … no longer exists"). Data is intact, and the same resume works 10 s later.
- **B3. A cross-node fork during a periodic upload fails** at once with `snapshot_not_found`.
- **B4. B1 residual.** If a sandbox moves to another node while a periodic upload runs, the old node keeps that layer. After the sandbox is deleted, its op says `completed` while the old node still holds its disk in plaintext. The data goes only when that node next reconnects.

B2, B3 and the cause of B4 predate opus-17. They appear in the 20:22 backup `lc.bak` too. They are still blocking: stop/resume and fork are core Boat features, and LAN or Tailscale upload speeds are the stated deployment.

## Method

1. **Read.** `opus-17.md`, `sonnet-15.md`, and the fix code: `deletion_ops.go` (whole file), `handleBlobPut` (`nodes.go:665-705`), `onSnapshotUploaded` (`snapshots.go:153-200`), `gcSnapshotsNeeded` (`snapshots.go:256-335`), `uploadAsync`/`LocalSnapshots` (`internal/node/transfer.go:186-260`), `handleDeleteNode`/`eraseNodeCopies` (`nodes.go:955-1019`), `Blobs.List` (`store/blob*.go`).
2. **Build and tests.** `go vet ./...` is clean. `go test -count=1 ./...` is `ok` in every package, `internal/server` in 42.9 s; e2e skips without `CROWDVM_E2E`. I did not run e2e. e2e run21 ended `EXIT=0` (37 PASS, 2 SKIP). Its build step ran at 21:23:35.7, five seconds before the last `transfer.go` edit, so it may not include that edit.
3. **Own cluster** (all in `~/.local/share/review-opus-18`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated):
   - `crowdvm-server` on 127.0.0.1:9960.
   - A Python TCP proxy on 9965 that limits node→server traffic to 40 MB/s per connection. Nodes joined via the proxy, and the CLI talked to 9960 directly.
   - Nodes `nA` (UI 9961) and `nB` (UI 9962), later `nC` (9963) and `nD` (9964). All qemu/KVM with 8 vCPU, 16 GiB, 3 sandboxes, `--snapshot-interval 20s`, image `ubuntu-24.04-crowdvm-minimal-v3`.
   - Users `admin` and `bob` (ZDR).
   - To find leftovers I wrote marker strings in the guests. Each marker was built with `$((6*7))`, so the literal string exists only in guest disks and not in command history. I grepped for them across the server's `blobs/` and the nodes' `vm/`. I also compared the server `snapshots` rows with node `vm/snapshots/*.json`.
4. **Teardown.** I deleted every sandbox and saved snapshot, then stopped the server, the nodes, the proxy and the qemu processes I started, by saved PID. No process matched my dir afterwards and 9960–9969 were free. I removed the data dir and my binaries. I touched no other process. The e2e run21 cluster on 37337 was left alone. The only repo file I wrote is this report.

## Fix status

| # | Claimed fix | Status | Evidence (live) |
|---|---|---|---|
| opus-17 B1, stop then delete | Op waits for uploads, then erases | **Fixed** | 300 MB + marker, `stop --wait`, delete. The periodic layer `snap_a6mv` (353 MB) finished uploading at 29.324. The op completed at 29.538. No blob, no node record, no overlay, 0 marker hits. |
| opus-17 B1, ZDR stop | Same | **Fixed** | Bob, 2 sandboxes (nA, nB), 250 MB each, ZDR on, `stop`. Both ops showed `processing / waiting_for_uploads` for 6 s and completed 0.1 s after the 300 MB uploads ended. Both nodes were left with only the base image and the server with 0 blobs, 0 `SECRET-BOB` hits. |
| opus-17 B1, delete during a periodic upload | Row tracked, waited for, erased | **Fixed** | 400 MB + marker, `DELETE` 0.5 s after "transfer started" (458 MB). Server log shows "tracking an upload of a deleted sandbox". Upload ended at 51.019 and the op completed at 51.350. 0 blobs, 0 node records, 0 marker hits. |
| Same, node killed mid-upload | Op waits for the node | **Works** | `kill -9` nA 1.5 s after the delete. The op stayed `processing`. Restart → `completed` within 13 s, overlay and record gone, 0 hits. |
| Orphan blob sweep | Erases only blobs with a purged or missing row | **Safe** | I aged all 19 blobs to 2 h and restarted the server. It erased exactly one blob, `snap_nzv8`, which had no row and which the merged layer `3jy22` supersedes. Afterwards x3, its fork f4 and deploy d5 stopped and resumed across nodes with every sha256 matching, and saved `sq` deployed fresh on nA. |
| Node reconcile on connect | Erases copies of deleted data only | **Safe** | Restarts of nA and nB during uploads (nB with `kill -9`, VM adopted) touched nothing live. The interrupted layer re-uploaded as the parent of the next periodic, and x3 then resumed on nA with all data. |
| New `trackDeletedUpload` path | Keeps live data | **Breaks it (B1)** | See below. |
| m1 | Never-booted `new --from` after `snapshot rm` | **Fixed** | Pool full, `new --from sq --no-wait`, `stop`, `snapshot rm sq`, `resume` → `409 snapshot_not_found` "This sandbox never booted, and the saved snapshot it was created from was removed". Residual: a re-save under the same name updates the same `saved_snapshots` row (`snapshots.go:888-892`), so such a resume would boot the newer version. Tiny. |
| m5 | `nodes remove` | **Fixed (offline), minor left (online)** | Offline: nC killed, `nodes remove nC --yes` → `snapshotCopiesLeft: 4`. Deleting h1 → op `blocked`, "node_removed: 2 snapshot copy(ies) stayed …". The server blobs were erased, and h2 resumed on nA with its data. `node-owners.md:138-142` documents this. Online: see m-a. |

## Blocking findings

### B1. A delete during a periodic upload purges the older layers that a saved snapshot or fork still needs (`deletion_ops.go:238-240`)

**Boat:** "boat keeps the data that a fork of this sandbox still needs [and] your saved snapshots from this sandbox. They exist to outlive it" (`data-retention__delete.md:163-166`).

**Repro (live, 1 of 1 for the save, 1 of 1 for the API fork):**

1. Sandbox x1 on nA. I wrote 50 MB + `DATA-L0` and waited until periodic layer `snap_tpza` was uploaded and recorded.
2. I wrote 400 MB + `DATA-L1` and waited for "transfer started" of periodic `snap_yv5d`.
3. `crowdvm snapshot x1 tplx --no-wait`. This takes live snapshot `snap_w2eu` with parent `snap_yv5d`.
4. 1.5 s later, `crowdvm delete x1 --yes`.

`processDeletionOps` saw `snap_yv5d` uploading and inserted its row through `trackDeletedUpload`, **without `parent_id`**. GC walks parents from the roots to find what is still needed. The walk stopped at `yv5d`, so `snap_tpza` counted as unneeded and was purged: server blob deleted, `purged_at` set. `tplx` went to `ready`. `crowdvm new --from tplx --node nB` lazily pulled a 2-layer chain (`yv5d`, `w2eu`) and stopped in initramfs:

```
cloudimg-rootfs: The filesystem size (according to the superblock) is 13631227 blocks
The physical size of the device is 786171 blocks
cloudimg-rootfs: UNEXPECTED INCONSISTENCY; RUN fsck MANUALLY.
```

The sandbox sat in `provisioning` for 3 min until I deleted it. The fork variant goes the same way. I made x2 with L0 = `snap_h38m`, forked it via `POST /fork` during the L1 upload, and deleted it. `snap_h38m` was purged and the tracked `snap_vgab` row again had no parent. That fork had already failed for B3, but a same-node fork would have kept a server chain with a hole in it, so a later resume on another node would hit the same fsck failure.

**Cause:** `INSERT OR IGNORE INTO snapshots(id,sandbox_id,owner_id,image_id,kind,chain_id,node_id,created_at,deleted_at)` writes no `parent_id` (`deletion_ops.go:238`). `proto.NodeSnapshot` (`proto.go:424-430`) does not carry the parent, so the server cannot know it. When the upload lands, `handleBlobPut` and `onSnapshotUploaded` find the row already there and never fill in `parent_id`. Before this fix the layer got no row at all, which gave the same result. The fix was the place to close it and does not.

**Why supportable:** bookkeeping only. Add `ParentID` to `NodeSnapshot` and write it in `trackDeletedUpload`. Have `handleBlobPut` set `parent_id` from the `X-Crowdvm-Snapshot` ref when the column is NULL. Better still, fix the root cause below.

### B2. `stop --wait` then `resume` during a periodic upload answers `409 resume_failed`, `retryable: false` (`lifecycle.go:33-44`, called at `lifecycle.go:1242`)

**Boat:** `boat stop` … `boat resume` "bring it back" (`quickstart.md:49-50`). `resume_failed` means the disk is gone (`api__v1.md:121`).

**Repro (live, 2 of 2):** y9 on nA. I wrote 400 MB and waited for "transfer started" (458 MB). `stop --wait` returned `stopped` in 1.0 s, then:

- `resume --node nD` → `409 resume_failed` "The sandbox's disk cannot be restored: snapshot snap_v5dj… (or its parent) no longer exists. Deploy a saved snapshot or create a new sandbox."
- The same again on nD with plain `resume` (same node) → the same error for `snap_t5bc`.

Both times the same `resume` succeeded once the upload ended, about 10 s later, with every marker present. `stop` without `--wait` followed by `resume` takes the resume-during-stop path, and that worked: ready in 6 s.

**Cause:** the stop's live snapshot has `parent_id` = the in-flight periodic layer. `chainRestorable` treats a parent without a row as "missing" and fails permanently. The error tells an agent its data is gone, so a reasonable agent will delete the sandbox and start over.

**Why supportable:** a layer that a connected node reports as `uploading` (`uploadsInFlight` already asks for this) is pending, not missing. Wait for it as `ensureChain` waits for `uploaded_at`, or answer a retryable 409 such as `sandbox_stopping`.

### B3. A fork onto another node during a periodic upload fails with `snapshot_not_found` (`lifecycle.go:461-463`)

**Repro (live, 2 of 2):** x2 and then x3 on nA, 400 MB each. During "transfer started", `POST /sandboxes/{id}/fork` with `placement.node = nB`. The fork went to `error` within 0.3 s: "sandbox failed … err=snapshot_not_found: snap_yj82…". The source was alive, and nothing had been deleted. Same-node forks are not affected, because the fork's live snapshot already has a location there.

**Cause:** `ensureChain` walks `snapshotChain(parent)`, which includes the in-flight periodic id, and `SnapshotByID` fails on it at once. It never reaches the 30-minute wait for `uploaded_at` a few lines below. The failed fork also leaves its live snapshot `snap_7kd7` as an undeleted row on the source, so the storage stays until the source is deleted.

**Why supportable:** same fix as B2. Treat an id that a node is uploading as "not uploaded yet" and wait.

### B4. The old node keeps a periodic layer of a sandbox that moved during its upload, and still holds it after the delete reports `completed` (`snapshots.go:170`)

This is opus-17 B1's acceptance criterion: no record or overlay on a node before the op reports `completed`. It fails on a different path.

**Repro (live):**

1. x3 on nA. A periodic layer `snap_nzv8` (422 MB, holding `DATA-X3B`) started uploading.
2. `stop --wait`, then `resume --node nB`. That resume happened to work, because the stop's live snapshot merged the in-flight layer.
3. `nzv8`'s upload ended while x3 was already placed on nB. `onSnapshotUploaded` hit `sb.NodeID != "" && sb.NodeID != nodeID` and returned without recording a row or dropping the copy.
4. The orphan sweep later erased the server blob, which is correct. nA kept the record and the overlay. Reconcile skips them, because the sandbox was alive at the last connect.
5. I deleted x3 later. Its op went `completed` at 21:52:29. nA still held `nzv8` plus its two hidden ancestors `7kd7` and `yj82`: 841 MB in `vm/disks/bx_yunb89kf-*`, with `DATA-X3` found by grep in 2 files.
6. Only a restart of nA (21:53:05) erased them ("erasing a node's copy of deleted data … snap_nzv8"). A node that stays connected for weeks keeps them for weeks.

**Why supportable:** in that branch, record the row with the node as location, so GC and the deletion op reach it, or drop the node copy when the server's chain does not use it. Also let a sandbox deletion op ask online nodes (`GET /v1/snapshots`, which already returns `sandboxId`) for records of its target and erase them before it reports `completed`. The data to do this is already there.

### Root cause and the one fix that closes B1–B4

The server learns of a periodic layer only when its upload ends. A stop, save or fork can take a live snapshot on top of it before then, and the sandbox can move to another node before then. If the node reports each periodic layer when it takes it (id, parent, sandbox), for example as a `snapshot.taken` event or through `GET /v1/snapshots` with `parentId`, the server can insert a pending row (`uploaded_at` NULL, location = node) right away. Then:

- `chainRestorable` and `ensureChain` see a pending row and wait or retry instead of failing (B2, B3).
- GC sees the real parent (B1).
- The moved-node case has a row and a location, so deletion reaches it (B4).

## Minor (not blocking)

- **m-a. `nodes remove --force` on an online node leaves its VMs running there** (`nodes.go:965-975`). x3 kept running on the removed nB. The node could not reconnect (401) but kept taking periodic snapshots it could never upload, 18 records by the end, while x3 also ran on nA after the resume. The response said `snapshotCopiesLeft: 0`. After I deleted x3, nB still held its disks, and no op was blocked for them. Fix: `DELETE /v1/vms/{id}` for each VM before closing the session, and count the overlays in `snapshotCopiesLeft`. CrowdVM-only feature.
- **m-b. A sandbox pinned to a removed node never resumes without `--node`.** Plain `resume` of x3 and h2 kept getting `503 out_of_capacity` with "nA: not the pinned node", and the CLI waited until my timeout. Drop pins to nodes that no longer exist. CrowdVM-only.
- **m-c. m1 re-save residual**, see the fix table.
- **m-d. The op's `lastError` said `waiting_for_uploads` while the node holding the upload was offline.** It stays stale until the next pass. Cosmetic.
- **m-e. A failed fork leaves its live snapshot as a live row on the source** (`snap_7kd7`), so the storage is held until the source is deleted.

## Sweep results

| Area | Result |
|---|---|
| B1 cases (stop+delete, ZDR ×2, periodic delete, node killed mid-upload) | Pass |
| Server restart + orphan sweep with all blobs aged 2 h | Pass (only an unneeded orphan erased) |
| Node restarts (graceful and `kill -9`) during uploads | Pass, VMs adopted, data intact |
| Running, stopped, fork, deploy, all moved across nodes after restarts | Pass, every sha256 OK |
| Saved snapshot after deleting its source mid-upload | **Fails (B1)** |
| Stop then resume during a periodic upload | **Fails (B2)** for `--wait`. Pass for resume-during-stop and for resume after the upload. |
| Cross-node fork during a periodic upload | **Fails (B3)** |
| Delete after a cross-node move during an upload | **Fails (B4)** until the old node reconnects |
| `nodes remove` offline | Pass (blocked op, documented) |
| `nodes remove --force` online | Minor m-a |
| `go vet`, `go test ./...` | Clean, all `ok` |

## Disputed or accepted classifications

- **B2–B4 predate opus-17.** I still count them as blocking. They break documented Boat behaviour (stop/resume, fork, delete semantics) under the deployment CrowdVM targets. They went unseen only because loopback uploads finish in under a second.
- **`blocked` ops after removing an offline node.** Accepted. The server cannot reach the machine, and `node-owners.md` tells the owner to wipe the data dir.

## Not verified

- e2e on the exact current tree. Run21's build may predate the 21:23:40 `transfer.go` edit, and I was not allowed to run e2e.
- B2 and B3 without a throttle on a real 1 Gbps LAN. Window there ≈ hashing time + layer size / 110 MB/s, a few seconds for a busy sandbox.
- Account erase, Windows nodes, the dashboard in a browser.

## What would flip this to AGREE

1. Periodic layers get a server row (with parent and location) when taken, or an equivalent fix, so that:
   - a saved snapshot and a fork survive deleting their source mid-upload, and deploy on another node with all data (B1);
   - `stop --wait` + `resume`, same node and other node, during an upload waits or answers a retryable error, never `resume_failed` (B2);
   - a cross-node fork during an upload waits for the layer (B3);
   - after a sandbox moved mid-upload and is deleted, no node holds a record or overlay of it when the op reports `completed` (B4).
2. Servertests for each of the four, with the fake node reporting an in-flight layer. One e2e check with throttled uploads, or a node test hook that delays the upload, covering stop+resume and save+delete.
