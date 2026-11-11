# Review sonnet-19: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent, no sub-agents). Date: 2026-11-11, 02:15–03:45. Tree: uncommitted working tree on `master`. I ran `make build` at 02:15 (after the last Go edit, `internal/server/server.go` at 02:14) and copied the binaries to my own dir. I did not carry over any earlier verdict, including my own sonnet-18.

## Verdict: DISAGREE (one narrow blocker, two symptoms)

sonnet-18's B1 and B2 are fixed. I confirmed both live behind a 40 MB/s link. Everything else in my pool pass held.

One thing still breaks the bar I was given. When the server restarts while a stop is in progress, the stop can end by the "machine already gone" path (`lifecycle.go:952`) and the server forgets the stop layer the node took. Two symptoms follow:

- **N1. A resume answers a false-final error while the data is intact.** A stopped sandbox with no stored layer yet answers `409 snapshot_data_expired` ("its snapshots were deleted or expired under the retention policy"). The error is final. The layers were still arriving, and the same resume worked 8 s later. This is the "no resume may give a non-retryable error while the data is intact" rule from the task, and it failed in 1 of 30 attempts.
- **N2. A stop that reports `stopped` loses data synced before it.** In 10 of 16 sandboxes in rounds 8–15, the resumed sandbox lacked a file that the guest had written and `sync`ed 1–3 s before `stop`. The sandbox resumed from an older periodic layer. The node still held the newer stop layer; the server recorded it as "deleted or moved" (`snapshots.go:191`).

Both are server bookkeeping and fixable on a LAN with no new dependency. The trigger is rare (a server restart within seconds of a stop). But the loss is silent: the user sees `stopped`, and opus-20 m2 described the loss as "close to nothing", which my runs contradict.

## Method

1. **Build and tests.** `make build` is clean. `go vet ./...` is clean. `go test -count=1` on every package except `test/e2e` is `ok` (`internal/server` 49.9 s). I did not run `make e2e` or `go test ./test/e2e/...`.
2. **Own cluster** in `~/.local/share/review-sonnet-19`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated and ports 9970–9975 only (9970 server, 9971/9972 node UIs, 9975 proxy).
   - One `crowdvm-server`.
   - My own Go TCP proxy that paces each connection, in each direction, to 40 MB/s with no banked credit. The nodes joined through it and the server's `--public-url` points at it. The CLI talked to 9970. A 582 MB image push took 15 s, an image pull 17 s.
   - Nodes `nA` and `nB`: qemu/KVM, 4 vCPU, 8 GiB, 2 sandboxes each, `--snapshot-interval 20s`, image `ubuntu-24.04-crowdvm-minimal-v3`.
   - The other reviewer's cluster and an e2e run were active on the same machine. I touched none of their processes.
3. **Judging data.** Each guest wrote random files with `dd` from `/dev/urandom` and then `sync`. It recorded their sha256 in `sums`, and the sums file was `sync`ed too. After every resume I ran `sha256sum -c sums`. A missing line in `sums` after the resume means the data written after the last layer used for the resume is gone.
   - `.upload-*` files were found with `find … -name '.upload-*'`, so dot-files are not hidden.
   - A "gap" is a snapshot row whose `parent_id` has no row (sonnet-18's `sqlite3` query).
4. **Teardown.** I deleted every sandbox and saved snapshot, stopped `nA`, `nB`, the server and the proxy by saved PID, confirmed that no process referenced my dir and that 9970–9979 were free, and deleted the data dir. I touched no other process.

### Part 1: B1/B2 repro (server restart mid-stop, throttled)

One round: two sandboxes (one per node), each with 300 MB written and `sync`ed, a wait of 21 s so a periodic layer is uploading, then a further 500 MB written and `sync`ed. Then `stop` on both and `kill -9` (or `SIGTERM`) of the server 0.05–0.5 s later. Then restart the server, re-issue `stop` until `stopped`, and resume each sandbox on the **other** node with retries. Then verify data and `.upload-*` files, delete, and audit.

- **Rounds:** 15 rounds (30 stop/resume attempts): 12 `kill -9`, 3 `SIGTERM`. Round 1 was cut short by a harness bug of mine (a bare `wait`), but its checks ran. Rounds 2–7 used a version where the `sums` file itself was not synced; I do not count their `sums`-line results as data loss. Rounds 8–15 are the final harness.
- **Extra B1 rounds (3):** the server killed for 26 s across a periodic tick (so a node's report of a periodic layer fails), then `stop` right after the restart. Gap rows were 0 two seconds after the restart in all 3, and every resume and data check passed. The node re-reports after it reconnects.
- **B2 probe:** a 900 MB `snapshot --no-wait` upload, `kill -9`/`SIGTERM` of the server mid-upload, record the `.upload-*` names at death, restart, check whether any of them exists.
- **B1 surgery:** I stopped my own server and edited its DB (see "Fix status").

### Part 2: long-lived-pool pass (throttled, node restarts)

| Step | What I did | Result |
|---|---|---|
| P1 | s1 on nA: 60 MB, wait for a tick, 600 MB, `stop --wait`, **SIGTERM nA**, restart nA, `resume --node nB` | Try 1: retryable `snapshot_unavailable`. Try 2, 39 s later: ready. Both files matched. |
| P2 | s2 on nB: 100 MB, 700 MB, `stop --wait`, **kill -9 nB**, resume on nA at once, restart nB 8 s later | The resume was accepted and waited for the holder. Ready after nB came back. Both files matched. |
| P3b | s3 on nA (running): 100 MB, wait for a tick, 600 MB, `snapshot save-b --no-wait` during an upload, **kill -9 nA** 1.5 s later, `stop --force --wait`, `resume --node nB` | Resume ready in **6 s**. `h1` present, `h2` absent (documented for a forced stop, Boat `snapshots__stop.md`). After nA came back, `save-b` went `ready` in under 45 s. A deploy of it on nA had both `h1` and `h2`. |
| P4 | s4 on nA: 400 MB, 500 MB, **kill -9 nA**, `delete`, restart nA 10 s later | The delete completed. Nothing of s4 on the nodes. |
| Final | deleted every sandbox and saved snapshot | Node `vm/disks`, `vm/snapshots`, `vm/vms`: 0. Server rows 0, 0 qemu processes. The server blobs held 2 files, which were the orphan blob of my own DB surgery (the row was deleted by me); a later sweep erased them. |

My first P3 attempt was a harness mistake (I stopped the sandbox and then tried to write to it). I replaced it with P3b above. I did not re-run P5's separate deploy of `save-a` beyond what the table shows: it matched (`g1 g2`).

## Fix status

| # | Claim | Status | Evidence |
|---|---|---|---|
| sonnet-18 B2 | `.upload-*` left on the server after a restart mid-upload | **Fixed** | Code: `CleanTemp(0)` at start (`server.go:304`), `CleanTemp(time.Hour)` in the sweep (`deletion_ops.go:313`), test `TestCleanTemp`. Live: in 15 rounds, 1–2 `.upload-*` files existed at death; every post-restart file was a new upload (mtime after the restart), and after each quiesce plus delete `.upload files = 0` and `ls -A blobs/snapshot` was empty. The explicit probe: 4 restarts (2 `kill -9`, 2 `SIGTERM`) mid-upload with names recorded at death; **0 survived**. After settling: 0. |
| sonnet-18 B1 | A missing parent row answers a false-final `resume_failed` | **Fixed** | Code: `chainRestorable` answers `snapshot_unavailable` (`retryable:true`, `retryAfterSeconds:30`) for a row that is missing, and `resume_failed` only for `purged_at` set (`lifecycle.go:33-58`). Surgery on my server DB: a stopped sandbox whose head's parent row I deleted answered `409 snapshot_unavailable … not registered yet … Retry in a minute`. Round 8 also hit the real window (`gaprows=1` during a resume, which succeeded). In practice the node re-reports after reconnecting, so 3 extra rounds with a failed periodic report had 0 gap rows. |
| "Genuinely lost chain still gives a final error" | | **Holds** | Same surgery: a head layer marked `purged_at` answered `409 resume_failed` "no longer exists. Deploy a saved snapshot or create a new sandbox"; a purged mid-chain parent (`pkth…`) gave the same final `resume_failed`. |
| sonnet-18 Parts 1–2 (node restarts, forced stop, deletes) | | **Hold** | P1–P4 above. |

## Blocking findings

### N1. A stopped sandbox with no stored layer answers a false-final `snapshot_data_expired` (`internal/server/lifecycle.go:1214-1220`)

**Boat:** a stopped sandbox comes back with `resume` (`snapshots__stop.md`). The bar for this task: an error is either retryable with a hint or truly final.

**Repro (live, round 9 nB, `bx_erzyvzce`):**

1. A sandbox about 50 s old whose first periodic layer (`g2x6`) and pre-flight layer (`phhr`) were still uploading, so it had no stored layer and `head_snapshot_id` was NULL.
2. `stop`, then `kill -9` of the server 0.3 s later, then restart. The server's retried stop reached the node, got "vm not found", and ended through the 404 path (`lifecycle.go:952`, `finishStop(sb, op, "", true)`) with the head still NULL.
3. `resume --node nA` at once answered, with exit code 1:
   ```
   snapshot_data_expired: The snapshot data of this sandbox no longer exists (its snapshots were deleted or expired under the retention policy), so it cannot be resumed or forked. Deploy a saved snapshot or create a new sandbox.
   ```
   The same resume 8 s later was ready, with the data intact (`a1: OK`).
4. The code is `lifecycle.go:1214-1220`: `FirstBootDone && HeadSnapshotID == ""` returns `snapshotDataExpiredErr` with `retryable:false`. A head only becomes set when a layer is stored (`afterSnapshotStored`), so an unstored chain looks identical to an expired one.

**Why supportable on a LAN:** pure bookkeeping. If an unpurged, undeleted layer exists for the sandbox that is not stored yet, answer a retryable `snapshot_unavailable` (as `chainReachable` and `chainRestorable` already do). Reserve `snapshot_data_expired` for a sandbox with no live layers. The same guard is needed at the `fork` caller (`lifecycle.go:1385-1387`).

**Scope.** It needs a server restart while a stop is in progress on a sandbox that has no stored layer yet (usually a sandbox younger than about one upload). It is rare. But an agent that reads it will abandon a healthy sandbox, which is exactly what sonnet-18 B1 was about.

### N2. A stop cut by a server restart can end `stopped` on an older layer, silently losing data synced before the stop (`internal/server/lifecycle.go:952`, `internal/server/snapshots.go:191`)

**Boat:** `stop` snapshots the disk (`snapshots__stop.md`); only a `--force` stop is documented as keeping the last good snapshot and losing newer data.

**Repro (live; 10 of 16 sandboxes in rounds 8–15):**

1. Sandbox with a 300 MB file in an earlier layer, then a 500 MB file `a2` and its checksum line written and `sync`ed. A periodic layer in flight.
2. Plain `stop`. `kill -9` of the server 0.1 s later, then restart. Node log for `bx_rgkuc94n` (round 8):
   ```
   02:51:44.287 vm state state=stopping
   02:51:44.320 guest prepare-stop failed; stopping anyway … context canceled
   02:51:44.364 qemu: bx_rgkuc94n stopped as snap_jgk6… in 44ms
   ```
   So the node finished the stop and **holds the correct stop layer** `snap_jgk6…`.
3. The server restarted. Its retried stop reached the node and got "vm not found" (`02:51:54.042 … error err="stop: … vm not found"`). That is the 404 path: `finishStop(…, "", true)` "machine already gone: keep the last snapshot" (`lifecycle.go:952`). The head stays on the older stored periodic layer `6rrw…`, and `stopped` is reported.
4. After the sandbox was resumed on nB, the node's late report of `jgk6…` arrived (`02:52:00.186`: "recorded a snapshot of a deleted or moved sandbox as deleted") and was thrown away. The condition is `snapshots.go:191` (`sb.NodeID != "" && sb.NodeID != nodeID`).
5. The resumed sandbox's `sums` held only `a1`. The 500 MB `a2` and its `sync`ed checksum line were gone.

**Pattern:** the loss shows when the server's retry beats the node's report of the stop layer. In single-sandbox runs (4 trials, delays 0.1–0.4 s) the node's report arrived first, was recorded as a `periodic` head (opus-19 m2/sonnet-18 m4), and nothing was lost. With two sandboxes uploading through the throttled link, 10 of 16 lost data. The `sums` check in rounds 2–6 was unsynced, so I do not count those.

**Why supportable on a LAN:** pure bookkeeping. Options, any of which is enough:
- In the 404 path (`lifecycle.go:952`), ask the node for its newest layer of this sandbox and adopt it as head (the layer is already on the node, and its upload is requested by `uploadStranded`).
- In `recordTaken`, when the sandbox is stopped with no node and the reported layer's parent is the current head (or the head's chain) and it is newer than the head, adopt it as the head instead of recording it as deleted.
- Do not call the stop done (leave the sandbox in `archiving`/`failing`) until the node's answer is known.

**Scope and severity.** It needs a server crash or restart within seconds of a user's `stop`. It does not lose data written before the last stored layer, and the data that vanishes is whatever was written since the last stored periodic layer (up to the periodic interval, 60 s by default; more if uploads lag). I judge it blocking because it is data loss with a success status, and the task's bar allows "rare crash windows" only when they lose no data. It is the same defect class as opus-20 m2, whose "close to nothing" estimate does not hold here.

## Minor (not blocking)

- **m1.** A layer that the server never registered but whose blob is already stored is not re-registered by a node restart. My surgery (a deleted row) kept answering "not registered yet, retry in a minute" after a node restart. The real flow registers the layer when the upload ends (`onSnapshotUploaded`), so I could not reach this state without editing the DB.
- **m2.** `crowdvm resume` immediately after `kill -9` of the holder is accepted and waits (P2) instead of answering a fast `snapshot_unavailable` (sonnet-18 D). Both are acceptable: the wait ended when the node came back, and the 2-minute holder grace applies.
- **m3.** An orphan blob with no row (from my DB surgery) stayed for about 20 minutes and was then swept. Fine.
- **m4.** A server restart during a stop sends a `stop_failing` notice ("node_offline") for a few seconds, as opus-20 m3 said.

## Disputed or accepted classifications

- **Data written after the last stored layer is lost when the node is gone** (P3b): accepted, Boat's forced-stop rule.
- **sonnet-18 B1 "healed by itself":** I confirm the healing now happens at reconnect. 0 gap rows two seconds after restart in 3 rounds where the report failed.
- **My own `sums` check in rounds 2–6.** The `sums` file was not synced in those rounds, so a missing line can be unsynced page cache. I reran with a synced `sums` (rounds 8–15) before counting anything as data loss. Round 8 additionally shows the head on the old periodic layer, the stop layer dropped by the server, and the node's log of the stop.

## Not verified

- `make e2e` (not allowed). The e2e run23 log ends `EXIT=0`, but I did not match it to this exact tree.
- A real 1 Gbps link without the throttle. The N1/N2 timing depends on how fast the node reports after reconnecting.
- Whether N2 also hits the `fork` of a stopped sandbox in the same window (the `fork` caller reads the same head).
- ZDR users, `nodes remove`, Windows nodes, the dashboard in a browser, account close/erase, live model turns.
- I did not re-grade `docs/PARITY.md` row by row.

## What would flip this to AGREE

1. **N1.** A sandbox whose layers exist but are not stored yet never gets a non-retryable `snapshot_data_expired` or `resume_failed` on resume or fork. It gets a retryable `snapshot_unavailable` with a hint. A servertest: a sandbox with `FirstBootDone`, no head and one unstored live layer, then `resume`.
2. **N2.** After `stop` plus a server restart at 0.05–0.5 s with two sandboxes uploading, the resumed sandbox has every file that was `sync`ed before `stop` (the node's stop layer becomes the head, or the stop is not reported as done until it is). Repeat 10 or more rounds with a synced `sums` file and zero missing lines. A servertest: a stop that ends through the 404 path, then a late node report of a stop layer chained on the head.

Everything else in the task's checklist held in my runs.
