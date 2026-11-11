# Review opus-17: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`, built at 20:24 (after the 20:22 edits to `lifecycle.go` and `lifecycle_secure.go`). No sub-agents.

## Verdict: DISAGREE (one blocker)

All the claimed fixes hold live. A lazily pulled chain now becomes a location of its new node, and deleting the sandbox erases it there. That covers resume, fork and saved-snapshot deploy onto a second node, and also a node that is offline at delete time. Neither stop-then-resume loop failed (10 + 10 cycles). sonnet-14 n1 and n2 are fixed.

The blocker is in the same area as the last two, data deletion, but on a different path:

- **B1. Data that is still uploading when its sandbox is deleted stays behind on the server, and sometimes on the node, while the deletion operation says `completed`.** I hit it three ways: stop then delete, a ZDR stop (which deletes on its own), and deleting a running sandbox while a periodic snapshot uploads. Server blobs of purged snapshots stay forever, because nothing sweeps them. In the periodic case the node also keeps a live record and the overlay, with my marker text in plaintext, and a node restart does not remove them. Boat has a stage for exactly this (`waiting_for_uploads`, `data-retention__delete.md:68`). PARITY DATA-003 lists that stage, but no code implements it.

At the end of my run, all 18 deletion ops said `completed` and 120/120 snapshot rows were purged. The server still held 4 snapshot blobs (771 MB) and node nA1 still held one 438 MB layer, all from deleted sandboxes.

## Method

1. **Read.** `opus-16.md`, `sonnet-14.md`, and the fix code: `ensureChain` (`lifecycle.go:440-495`), `markReady`/`markHydrated` (`lifecycle_secure.go:252-315`), `reconcileVM` (`lifecycle.go:520-630`), resume of a never-booted deploy (`lifecycle.go:1252-1287`), `afterStopResume` (`lifecycle_place.go:261-286`), `sizeAvailability` (`scheduler.go:280-310`), deletion ops (`deletion_ops.go`), GC (`snapshots.go:240-335`), blob upload (`nodes.go:649-672`, `snapshots.go:146-190`), ZDR and account purge (`feat_retention.go`).
2. **Build and tests.** `make build` and `go vet ./...` are clean. `go test -count=1 ./...` is all `ok`; e2e skips without `CROWDVM_E2E`. I did not run e2e.
3. **Own cluster.**
   - `crowdvm-server` on 127.0.0.1:9960.
   - Nodes `nA1` (UI 9961) and `nA2` (UI 9962): qemu/KVM, 4 vCPU, 8 GiB, 3 sandboxes, `--snapshot-interval 20s`, image `ubuntu-24.04-crowdvm-minimal-v3`. I pushed the image from nA1, and nA2 pulled it on first use.
   - Users `admin`, `bob` (ZDR) and `carol`.
   - Data in `~/.local/share/review-opus-17`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated.
   - To check what stayed behind, I grepped for unique marker strings written in the guests (`SECRET-…`) across both nodes' `vm/` dirs and the server's `blobs/`. I also compared `snapshot_locations` and the node `snapshots/*.json` records against the server rows.
4. **Teardown.** I deleted every sandbox and saved snapshot and stopped both nodes and the server by saved PID. Afterwards no crowdvm or qemu process used my dir and 9960–9969 were free. I deleted the data dir and my copied binaries. I touched no other process. The only repo file I edited is this report.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| opus-16 B1 | A lazily pulled chain is a location of its new node, so a delete erases it there | **Fixed (live)** | `ensureChain` now adds the location before the lazy fetch (`lifecycle.go:480-485`), and `markReady` records the chain when it sets `hydrated` itself (`lifecycle_secure.go:270-274`). **Resume:** W made on nA1, stop, `resume --node nA2`. The nA2 log shows "lazy boot, 3 of 3 chain layers remote", then "chain hydrated" 100 ms later. `snapshot_locations` lists both nodes. After writing on nA2, stop and delete, the op went `completed`, both nodes had 0 records, 0 overlays and 0 marker hits, and the server had 6/6 purged. The op completes only after the node `DELETE`s succeed (`snapshots.go:302-333`, `deletion_ops.go:89-102`). |
| opus-16 B1 (fork, deploy) | Same for fork and saved-snapshot deploy onto B | **Fixed (live)** | X on nA1. Live fork F onto nA2. Saved `sx` from X, deployed as D on nA2. I deleted F, then D, then ran `snapshot rm sx`, then deleted X. After each step only layers still read by a live sandbox stayed. After X: 0 records on both nodes, 0 marker hits, 0 server blobs. |
| opus-16 B1 (offline) | A node offline at delete time | **Works (live)** | I moved Q (chain merged to 4 layers) to nA2, stopped it, killed nA2 and deleted Q. The op showed `processing`, `retrying`, "node_offline …", and `crowdvm delete` kept following it, as Boat's CLI does (`data-retention__delete.md:23`). Two seconds after nA2 restarted the op went `completed`. The 7 Q layers left on each node were exactly the ones in the chains of two live deploys of `sq` (a saved snapshot of Q), which is correct. |
| race | Late VM-stopped report after stop + quick resume | **No failure in 20 cycles** | `stop --wait` then an immediate `resume`, 10×: all `ready`, generation file exact each time. `stop` (no wait) then an immediate `resume`, 10× (the resume-during-stop path): all `ready`, data exact. I could not reproduce the original run17 failure, as opus-16 could not either, so this shows no regression rather than proving the fix. No unit test covers the `VMStopped`-while-starting guard (`lifecycle.go:618-624`). |
| sonnet-14 n1 | A queued `new --from` that is stopped and resumed boots empty | **Fixed (live), one residual (m1)** | Pool full, `new --from sq` queued, `stop`, slot freed, `resume` → `ready` with `savedq.txt` and `gen.txt` present. Same result when the resume reaches `afterStopResume`: the pending boot carries `parent: snap_ekk9…`. |
| sonnet-14 n2 / opus-16 m3 | `limits` reason for a size only a paused node fits | **Fixed (live)** | nA1 4 vCPU active, nA2 8 vCPU paused: `large` → "only a node its owner paused can host large (a request waits until it is resumed)". `xlarge` → "no node in the pool is big enough". |
| e2e | run19 | **EXIT=0, but on an older tree** | run19 started at 20:15:30, before the 20:22 edits to `lifecycle.go` and `lifecycle_secure.go`, so it does not cover the current B1 code. 37 PASS, SKIP only for `agent/opencode` and `bootloop`. |

## Blocking finding

### B1. A snapshot upload that is still running when its sandbox is deleted survives the deletion (`data-retention__delete.md:58-70`, `data-retention__zero-data-retention.md:5,78`, PARITY DATA-003/DATA-008)

**What Boat promises.** A delete "erases the stored data in the background, when nothing reads it any more". `completed` means "boat erased every snapshot of it". If uploads are still in flight, the op sits in stage `waiting_for_uploads` until they can be erased (`data-retention__delete.md:68`). Under ZDR, "boat deletes the data of each sandbox when it stops". PARITY DATA-003 (`docs/PARITY.md:604`) claims that stage and says `completed` means every server blob and node copy is erased. `grep -rn waiting_for_uploads internal/` finds nothing.

**What happens (live, current tree).**

1. **Stop, then delete.** V on nA1. I wrote 300 MB of random data plus a marker file, then sent `POST /stop` and polled until the state read `archived`. `DELETE` followed 4 ms later. The op was `completed` 3 ms after that. Timeline for the stop's live layer `snap_7zad…` (338 MB): upload started 06.443, row purged 06.969, upload finished 07.093. The blob was still in `blobs/snapshot/` at teardown, with `SECRET-VV` inside. The row says `purged`, so GC never looks at it again.
2. **ZDR stop (2 of 2).** Bob enabled ZDR. Stopping a running sandbox deletes it at once (reason `zdr`, op `completed`). Both times the stop layer's upload finished after the purge: `snap_q92s…` (21 MB, holding `SECRET-BOB-D`) was purged at 30.985 and uploaded at 31.015, and `snap_27nb…` (9.5 MB) was purged 17 ms before its upload ended. Both blobs stayed. With ZDR every stop races this way, and leftover stop data is what ZDR promises never to keep.
3. **Deleting a running sandbox during a periodic upload.** P on nA1. I wrote 400 MB plus a marker, waited for "transfer started" of the periodic layer `snap_9xjd…` (438 MB), then sent `DELETE`. The op went `completed`. The upload finished 0.9 s later. Afterwards:
   - The server holds blob `snap_9xjd…` with **no snapshots row at all**, so no code path can ever delete it.
   - nA1 keeps `vm/snapshots/snap_9xjd….json` and `vm/disks/bx_c6ey27ez-184c2cfc.qcow2`, with `SECRET-PP` in plaintext. It was still there after I restarted nA1. The node owner is a third party, so this is the same privacy problem opus-16 B1 was.

**Cause (from the code).**

- GC purges a snapshot whose upload has not finished. It deletes the blob, which does not exist yet, then sets `purged_at` (`snapshots.go:329-333`). The deletion op counts only unpurged rows, so it completes (`deletion_ops.go:89-101`).
- `handleBlobPut` stores any snapshot blob a node sends, even for a purged row or a deleted sandbox (`nodes.go:649-672`). `onSnapshotUploaded` even sets `uploaded_at` on the purged row (`snapshots.go:184`).
- For a periodic layer the server has no row until the upload ends. `onSnapshotUploaded` then sees the sandbox is deleted and returns early (`snapshots.go:155`). It never deletes the blob or tells the node, although the snapshots-off branch a few lines below does both (`snapshots.go:157-168`).
- Nothing reconciles the blob store or a node's records against the DB afterwards.

**How often.** Every stop uploads one or two layers after the VM is down, and every running sandbox uploads a periodic layer each 60 s by default. On loopback an upload takes 0.1–1 s. Over a 1 Gbps LAN, or Tailscale at tens of Mbit/s, a few hundred MB takes seconds to minutes. Under ZDR every stop hits the window. Without ZDR, an agent that runs `stop` then `delete`, or deletes a busy running sandbox (as `crowdvm run` does at the end), lands in it whenever an upload is in flight.

**Why it is supportable on a LAN.** It is all server and node bookkeeping. Any of these closes it, and the first two together match Boat:

1. Don't purge a row whose `uploaded_at` is NULL while its node is connected. Report the op as `waiting_for_uploads` (with `expectedBy`) until the upload lands or fails, then erase.
2. When an upload lands for a purged or deleted row, or for a deleted sandbox with no row, delete the blob and send the node `DELETE /v1/snapshots/{id}`, as the snapshots-off branch already does. Or reject the PUT with 410 and have the node drop its copy.
3. Add a periodic sweep: delete server blobs with no unpurged row, and have nodes report their snapshot ids so the server can delete unknown or purged ones. This also cleans up what earlier builds leaked.

**Tests to add.** A servertest that deletes a sandbox between "snapshot taken" and "upload finished", for both a stop layer and a periodic layer, and asserts no blob is left and the node received a `DELETE`. An e2e check in `cluster`: write 300 MB, stop, delete at once, then assert the server's `blobs/snapshot` holds nothing of that sandbox.

## Minor (not blocking)

- **m1. Residual of sonnet-14 n1.** A queued `new --from sq` is stopped before it ever boots. If the owner then runs `snapshot rm sq` and resumes it, it reaches `ready` with an empty home (live: `savedq.txt` missing, `ls ~` shows only image dotfiles). The resume looks the saved snapshot up by id at resume time (`lifecycle.go:1273-1276`), and `rm` deletes the row (`snapshots.go:920`), so `parent` ends up empty and the boot is a plain create. A re-save under the same name would likewise give the resume the newer version. No data is lost (the user removed the snapshot), and the case needs a full pool plus stop plus rm. Fix: record the parent snapshot id on the sandbox at create time, or answer `409 snapshot_not_found` instead of booting blank.
- **m2. `resume --node X` sent while the sandbox is stopping ignores `--node`.** `changes` leaves out placement (`lifecycle.go:1148`), and `afterStopResume` prefers the previous node (`lifecycle_place.go:284`). Live: 5 such resumes asked for nA2 and all ran on nA1. Boat has no node placement, so this is a CrowdVM extension. It should either honour the flag or answer 409 like a size change does.
- **m3. Test coverage.** No unit test records a location for the hydrated-before-ready case or pins the stale-`stopped` guard. The latest green e2e (run19) predates the 20:22 edits.
- **m4. `stop --wait` under ZDR ends with `404 not_found`.** The sandbox was stopped and then deleted as designed, but the CLI reports an error. Cosmetic.
- **m5. `nodes remove` drops the node's locations** (`nodes.go:939`), so pending deletion ops complete while the data stays on the removed machine. The server cannot reach it anyway. A short note in `node-owners.md` saying that removal does not wipe the node's data dir would be honest. Not a Boat feature.

## Sweep results

| Area | Result | What I ran |
|---|---|---|
| Cross-node resume / fork / deploy, then delete | Pass | See the fix table. Marker grep on both nodes and the server after every delete. |
| Deletion with the node offline | Pass | Op `processing`/`retrying` while offline, `completed` 2 s after reconnect, layers erased there. |
| Deletion racing an upload | **Fails (B1)** | Stop then delete, ZDR stop ×2, delete during a periodic upload. |
| ZDR across nodes | Nodes pass, server **fails (B1)** | Bob: a sandbox moved nA1 → nA2 plus a saved snapshot deployed on nA2. `data-retention enable --yes` deleted the stopped one and removed the saved snapshot. Stopping the deploy deleted it. No `SECRET-BOB` on either node, but two stop-layer blobs stayed on the server. |
| Carol's sandboxes (cross-node) deleted explicitly | Pass | 0 marker hits on nodes and server afterwards. |
| Account erase | Not run | `POST /account/erase` with a CLI session → `403 browser_session_required`. The code path (`feat_retention.go:400-425`) uses the same `DeleteSandbox` + GC as ZDR, so B1 applies to it as well. |
| Queued saved deploy, stop, resume | Pass, residual m1 | Plain resume and resume-during-stop both boot the saved disk. |
| `limits` with a paused big node | Pass | See the fix table. |
| Node restart keeps leaked data | Confirmed | nA1 restarted. `snap_9xjd….json` and its overlay remained. |

## Disputed or accepted classifications

- **The op completes while layers stay that a live deploy or fork still reads.** I accept it. Boat says such data "goes when you delete those too" (`data-retention__delete.md:70`), and the 7 Q layers that stayed were exactly that set.
- **`crowdvm delete` follows the op even while a node is offline.** I accept it. Boat's CLI "follows the operation to completion", and `--no-wait` exists.
- **Account close/erase needs a browser session.** I accept it. Boat's close-account page uses the dashboard.

## Not verified

- e2e on the current tree. run19 predates the 20:22 edits, and I was not allowed to run e2e.
- Account erase end to end (needs a dashboard session).
- The original run17 race. I could not reproduce it, so 20 clean cycles show no regression rather than proving the fix.
- CI against GitHub or Gitea, Windows nodes, the dashboard in a browser, live model turns.

## What would flip this to AGREE

1. **B1.** A sandbox deleted while one of its snapshots is uploading ends with no blob on the server and no record or overlay on the node before its op reports `completed`. The op should show `waiting_for_uploads` meanwhile, as Boat documents. The same must hold for a ZDR stop and for a periodic upload of a running sandbox. Add the servertests and the e2e check described above. A one-time sweep of orphan blobs and node records would clean up leaks from earlier builds.
2. A green e2e on a tree that includes the 20:22 lifecycle edits.
