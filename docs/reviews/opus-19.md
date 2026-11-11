# Review opus-19: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. I built my binaries at 23:04, after the last Go edit (`internal/server/lifecycle_test.go`, 23:00). No Go file changed while I tested. No sub-agents.

## Verdict: DISAGREE (two blockers, one root cause)

Every claimed fix holds live, behind a throttle of 40 MiB/s per connection:

- opus-18's B1–B4: save + delete mid-upload, fork + delete mid-upload, `stop --wait` + resume (same node and other node), cross-node fork mid-upload, and move mid-upload then delete.
- sonnet-16's case: a server restart during a stop, 5 of 5 delays.
- Node removal (m-a, m-b).

Each one ended with the data intact (sha256 of every file) or, for deletions, with nothing left on the server or either node. Head selection works for periodic layers. ZDR waits for the in-flight upload and then leaves nothing.

The new pending-row design has one gap: **a layer that is recorded but never uploaded stays that way forever.** The node only uploads a layer from the goroutine that took it. A node restart (`kill -9` or a plain SIGTERM, as for a config change or update) drops that goroutine, and nothing re-queues the upload. The server cannot ask for it either: the node API has `fetch` and `DELETE` for snapshots, but no upload. The server waits for such a layer until the boot times out (20 minutes observed), then fails the sandbox. Two failures follow:

- **N1. A stopped sandbox whose node restarted during its stop upload cannot be resumed, forked or saved on any other node.** The resume waits about 20 minutes, then fails with `boot_timeout`, all while the node is online and holds the data. The sandbox heals only if it is resumed on the same node.
- **N2. A forced stop after node loss keeps an unstored `live` layer (from a save or fork) as head.** The resume then waits for a node that is gone, then fails. Boat promises the stop uses "its last good snapshot". A stored periodic layer was on the server the whole time.

## Method

1. **Read.** opus-18, sonnet-16, and the fix code:
   - `recordTaken`, `onSnapshotUploaded` and `afterSnapshotStored` (`snapshots.go:158-300`);
   - `handleSnapshotTaken` and `handleBlobPut` (`nodes.go:690-750`);
   - `chainRestorable` and `ensureChain` (`lifecycle.go:33-45`, `448-496`);
   - `runStop` and `finishStop` (`lifecycle.go:880-955`);
   - `handleDeleteNode` and `destroyNodeVMs` (`nodes.go:968-1024`);
   - `uploadAsync`, `UploadSnapshot` and `LocalSnapshots` (`internal/node/transfer.go:186-365`);
   - `writeErr` (`internal/node/api.go:253`).
2. **Build and tests.** `go vet ./...` is clean. `go test -count=1 ./...` is `ok` in every package (`internal/server` in 45.7 s). I did not run e2e. e2e run22 (`e2e-run22.log`) ended `EXIT=0`, with 38 PASS. It started at 23:03, after every source edit.
3. **Own cluster** in `~/.local/share/review-opus-19`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated and ports 9960–9965 only:
   - `crowdvm-server` on 9960.
   - A Go TCP proxy on 9965 that limits each connection, in each direction, to 40 MiB/s. Nodes joined through it, and the CLI talked to 9960. An image push measured 582 MB in 13.1 s, so the proxy works.
   - Nodes nA (UI 9961) and nB (9962): qemu/KVM, 6 vCPU, 14 GiB, 3 sandboxes, `--snapshot-interval 20s`, image `ubuntu-24.04-crowdvm-minimal-v3`. A third node nC (9963) was used for `nodes remove`.
   - Users `admin` and `bob` (ZDR).
4. **Judging data.** Every test wrote random files and recorded their sha256 in the guest. After each resume, fork or deploy I compared all of them. For deletions I compared server rows, `snapshot_locations`, server blobs, node `vm/snapshots/*.json` and node overlays. I also grepped (`grep -a`) for marker strings built with `$((6*7))` across the server's `blobs/` and both nodes' `vm/`. A positive control finds live markers in blobs, overlays and lazy caches.
5. **Teardown.** See the end of this report.

## Fix status

| # | Claimed fix | Status | Evidence (live, throttled) |
|---|---|---|---|
| opus-18 B1, save | Periodic layer reported with parent at take time | **Fixed** | x1: L0 50 MB stored, then L1 400 MB uploading (`snap_asjz`, 453 MB). `snapshot x1 tplx --no-wait`, then `delete x1` 1.5 s later. The rows were `asjz` (parent `3uzn`) and `2686` (parent `asjz`), none purged. `tplx` went `ready` at +10 s. `new --from tplx --node nB` matched all 4 checksums. |
| opus-18 B1, fork + B3 | Same, and cross-node fork waits | **Fixed** | x2: same setup. `fork x2 --node nB` during the 422 MB upload, then `delete x2` 1.5 s later. The fork waited about 25 s for the upload and came up `ready`, with all 4 checksums matching. The source op ended `completed`, and nothing in its chain was purged. |
| opus-18 B2 | Resume waits for a pending layer | **Fixed** | y9: `stop --wait` during a 2.0 GB upload returned in 1 s. Same-node `resume` was ready at +4 s. A second `stop --wait`, then `resume --node nA` while the upload still ran: it waited 45 s and came up `ready`. All 6 checksums matched both times. No `resume_failed`. |
| opus-18 B4 | Moved layer recorded as data held by the old node | **Fixed** | y9 on nB, 1.37 GB upload. `stop --wait`, then `resume --node nA`. The upload ended at 23:17:57 while y9 was placed on nA, and y9 was ready at 23:18:06 with its checksums. Then `delete y9`: the op went `completed`. Afterwards both nodes had 0 records and 0 overlays, the server had 0 rows and 0 locations, and 5 markers had 0 hits. |
| Head only once stored | `afterSnapshotStored` | **Fixed for periodic layers. Not for `live` layers (N2).** | z1: P1 100 MB stored and head, then P2 1.16 GB uploading. `kill -9` nA. The head stayed P1. `stop --force`, then `resume --node nB`: P1 checksums matched and P2 was absent, which is correct for a forced stop. |
| sonnet-16 B1 | Node answers 404 for "vm not found" | **Fixed** | r1: `stop`, then `kill -9` the server after 0.05, 0.2, 0.5, 1.0 and 1.5 s, and restart it. All 5 ended `archived`, never `ready`. Each was resumed, alternating nA and nB, with all checksums matching. One more run with unsynced writes kept them too. |
| m-a | `nodes remove --force` destroys the node's VMs | **Fixed** | c1 on nC, `nodes remove nC --force` → `snapshotCopiesLeft: 0`. The qemu process was gone within 3 s, and nC's `vm/` held only the base image. |
| m-b | Pins to a removed node are cleared | **Fixed** | c1's placement went from `{"node":"nC"}` to `{}`. After `stop --force`, a plain `resume` placed it on nA with its data. |
| opus-17 B1 (regression check) | Delete waits for uploads | **Holds** | z2: `kill -9` nA during a 992 MB upload, then `delete`. The op stayed `processing / retrying` while nA was down. After a restart it went `completed` within 20 s: no qemu process, records, overlays, rows or marker hits. |
| ZDR (regression check) | Same | **Holds** | bob, ZDR on, 780 MB uploading, `stop`. The op went `completed` (reason `zdr`) 0.4 s after the upload ended. 0 records, overlays, rows or marker hits. |
| Node loss mid-upload, then delete | — | **Holds** | z1 above: restart nA. The stale adopted VM was destroyed and the pending layer `qstm` stayed. Then `delete z1`: no records, overlays or rows remained, and 0 hits for either marker. |

## Blocking findings

### N1. A node restart during a stop upload strands the stopped sandbox on that node (`internal/node/transfer.go:186-246`, `internal/server/lifecycle.go:458-470`)

**Boat:** "A stop saves the disk … Run `boat resume` to get it back." "A resume starts a new machine with that disk." (`snapshots__stop.md:5,15`). A stopped sandbox can be resumed, forked and saved from any machine.

**Repro (live, 2 of 2):**

1. z3 on nA. Write 800 MB, wait for the periodic upload to start (992 MB), `stop --wait` (returns `stopped`), then `kill -9` nA. Rows: `zhks` periodic, `bfp2` live, `4mcp` stop, all `uploaded_at` NULL with location nA.
2. Restart nA. It reconnects and logs no upload for any of the three layers.
3. `resume z3 --node nB`. The sandbox sat in `provisioning`/`starting` on nB for 4 min with nothing uploaded. The CLI said "waiting for a node with free capacity", but nB had a free slot.
4. `stop`, then `resume --node nA` → ready, checksums OK. The next periodic layer on nA built on a flattened overlay, so the three stranded rows stay as dead weight until the sandbox is deleted.

z4 is the same with a **graceful SIGTERM** of nA (the guide tells owners to restart the daemon to apply config, `node-owners.md:55`). The node exits at once ("snapshot upload failed … attempt=1" for all three layers, then "disconnected"), and the restarted node uploads nothing. `resume z4 --node nB --no-wait` was issued at 23:37:27. Nothing was uploaded. At 23:57:28 the sandbox went `error` / `boot_timeout`, 20 min later. A new `resume --node nB --fail-fast` went back to waiting in `provisioning`: it did not fail fast, because nB has capacity. `stop`, then `resume --node nA` → ready in seconds with every checksum matching. Nothing was lost; it simply cannot leave nA.

**Cause:**

- `uploadAsync` is the only path that uploads a layer, and it lives in the process that took it.
- On restart, nothing compares the local snapshot records with the server's `uploaded=false` rows. `serverStored` already exists for exactly that check.
- The server has no way to ask a node to upload.
- `ensureChain` waits up to 30 min per missing layer for a node that will never send it. The boot timeout (20 min) ends it first.
- A saved snapshot built on such a layer stays `saving` and cannot be removed (`409 save_in_progress`) until the 2-hour save timeout. It then turns `failed`, although the node still holds the data.
- Plain `resume` prefers the node with the data (scheduler locality), so it usually goes back to nA and works. It fails when nA is full, drained or offline, or when the user or SDK picks another node. A fork or a saved snapshot of the stopped sandbox does the same: a save waits for the upload, which never comes.

**Why supportable:** the data is on an online node on the same LAN. Fix: on connect, the node uploads every local layer whose server row says `uploaded: false` (parents first, as `UploadSnapshot` already does). Or the server asks for it (`POST /v1/snapshots/{id}/upload`) when `ensureChain` or a save needs it.

### N2. A forced stop after node loss keeps an unstored `live` head, so the resume waits for a dead node instead of using the last good snapshot (`snapshots.go` `recordSnapshot` head update, `lifecycle.go:921-926`)

**Boat:** "The sandbox stops with its last good snapshot. You lose everything written after that snapshot." (`snapshots__stop.md:146`).

**Repro (live, 1 of 1):**

1. z5 on nA. P1 (`r66m`, 60 MB + marker) is stored.
2. Write 800 MB, then `snapshot z5 sv5 --no-wait`. Live layer `j7bf` (946 MB, parent `r66m`) starts uploading, and **head = `j7bf`** at once.
3. `kill -9` nA. `stop --force --wait` → `stopped`, and the head stays `j7bf`.
4. `resume --node nB` → `provisioning` on nB, waiting for `j7bf`. At 00:03:40, 20 min later, it went `error` / `boot_timeout`, with the head still `j7bf`. `sv5` was still `saving` 21 min after the save. nA was back online from 23:44 and never uploaded `j7bf` (N1), so a forced stop gave a sandbox that cannot start anywhere but nA. If nA had stayed gone, it could not start at all, although the stored `r66m` was on the server the whole time.

The fix "a layer is head only once stored" covers only periodic layers (`afterSnapshotStored`). `recordSnapshot` still sets a `live` (save or fork) or `stop` layer as head before its upload. For `stop` that is the design (the stop is the newest state). For a forced stop it is wrong: Boat's "last good snapshot" is the newest *stored* layer, here `r66m`.

**Why supportable:** bookkeeping. On a forced stop, or when `ensureChain` finds the head's only location offline, fall back to the newest ancestor with `uploaded_at` set, as Boat documents. Optionally answer a retryable 409 with "the node holding the newest data is offline" instead of a silent 30-minute wait.

## Minor (not blocking)

- **m1. Misleading wait message.** The CLI prints "waiting for a node with free capacity" for any sandbox in `provisioning` for more than 20 s (`internal/cli/helpers.go:72-80`), including one that is waiting for a snapshot upload on a node with free slots. Cosmetic.
- **m2. A stop layer recorded after a server crash is labelled `periodic`.** If the server dies before it records the stop's result, the layer arrives via `onSnapshotUploaded` → `recordTaken` with `kind='periodic'` (`snap_s9ta` in r1). Data and restore were fine. Kind-based rules such as the supersede rule in `onSnapshotUploaded` could treat it as an ordinary periodic layer.
- **m3. A server restart aborts the guest's prepare-stop.** The node's call fails with "context canceled", and the VM stops without the guest hook, Docker stop, fstrim and freeze. In my run unsynced writes still survived (`nosync.bin` checksum matched), so I saw no data loss.
- **m4. Stranded pending rows** (N1 step 4) stay until the sandbox is deleted. That costs storage only.

## Disputed or accepted classifications

- **N1 and N2 are blocking.** opus-18 counted B2 and B3 as blocking for a narrower window (an upload in flight). Here the window is the same, but the damage lasts: the stopped sandbox cannot leave its node, and a forced stop cannot reach data the server has. A node restart is a routine, documented owner action, and node loss is the normal failure of a crowdsourced pool.
- **A stop completes before its upload.** Boat refuses a stop until the save is stored (`snapshots__stop.md:140-148`). CrowdVM completes it once the node holds the stop layer. That adaptation is fine, but it makes durable uploads (N1) part of the stop guarantee.
- **`snapshot_not_ready` on `new --from` while a save is `saving`.** Accepted. Boat says to wait for `ready` (`snapshots__templates.md:84`, `faq.md:257`).
- **Data after the last stored layer is lost when a node dies.** Accepted (z1). It is the documented `--force` semantics and unavoidable on a crowdsourced node.
- The PARITY.md N/A rows I skimmed (payments, OAuth, trials, hosted-operator facts, legacy aliases) match the constraints.

## Not verified

- e2e on my own (not allowed). run22 is green on the current tree.
- Windows nodes, the dashboard in a browser, account close/erase.
- B2/B3 without the throttle on a real 1 Gbps link.

## What would flip this to AGREE

1. **N1.** After a node restart (SIGTERM or `kill -9`) during the upload of a stopped sandbox's chain, the node uploads the missing layers once it reconnects (or when the server asks). Then `resume --node <other>`, a cross-node fork, and a save of that sandbox all succeed with every checksum matching. Add a node test (records with `uploaded=false` re-queued on connect) and a servertest.
2. **N2.** After the node holding an unstored `live` head is lost, `stop --force` + resume on another node boots from the newest stored layer within seconds, as Boat documents. Add a servertest with a fake node that reports a save's live layer and then disconnects.

## Teardown

I deleted every sandbox. All 14 deletion ops ended `completed`, with no rows, blobs, node records or overlays left for those sandboxes. The only exception was z5's two layers, which `sv5` still held. `snapshot rm sv5` answers `409 save_in_progress` until the 2-hour save timeout (`snapshots.go:690`): N1 again, because nA held the layer the whole time.

I then stopped nA, nB, nC, the server and the proxy by saved PID. No qemu or other process of mine was left, and ports 9960–9969 were free. I deleted `~/.local/share/review-opus-19`, including a Go module cache that `go test` wrote under the isolated `HOME`. I touched no other process: the e2e run22 cluster and the other reviewer's cluster on 9970+ were left alone. The only repo file I wrote is this report.
