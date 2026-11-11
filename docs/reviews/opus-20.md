# Review opus-20: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, no sub-agents). Date: 2026-11-11, 00:50–01:45. Tree: uncommitted working tree on `master`. I built with `make build` at 00:51, after the last Go edit (`internal/server/snapshots.go`, 00:28). I copied the binaries to my own dir. No Go file changed while I tested.

## Verdict: AGREE

All three blockers from the last round are fixed, and I checked each one live behind a throttle of 40 MB/s per connection:

- opus-19 N1: a node restart mid-upload stranded a stopped sandbox.
- opus-19 N2: a forced stop after node loss kept an unstored head.
- sonnet-17 B1: a server restart during a live snapshot left an orphan overlay.

The new head rule (only `stop` layers become head before they are stored) caused no regression I could find. I found no new blocker. Three new minor findings follow (m1–m3). Each one either heals by itself or has a working way out, and none loses data beyond Boat's own "last good snapshot" rule.

## Method

1. **Read the fix code.**
   - `internal/server/stranded.go`: `uploadStranded`, `chainReachable`, `fallBackToStoredHead`, `HolderOfflineGrace`.
   - `ensureChain` and `runStop` (`internal/server/lifecycle.go:446-512`, `870-960`).
   - `recordSnapshot`, `recordTaken`, `onSnapshotUploaded` and `afterSnapshotStored` (`internal/server/snapshots.go:74-305`).
   - `RequestUpload` and `uploadAsync` (`internal/node/transfer.go:186-270`).
   - `Driver.Snapshot` and `stream` (`internal/node/vm/qemu/qemu.go:522-610`).
   - `reconcileNodeSnapshots` (`internal/server/deletion_ops.go:282`).
   - `handleDeleteNode` (`internal/server/nodes.go:969`).
2. **Build and tests.** `make build` is clean, and so is `go vet ./...`. `go test -count=1 ./...` is `ok` in every package. I did not run e2e. The team's e2e run23 (`e2e-run23.log`) started at 00:50 on the current tree and ended `EXIT=0` with 38 PASS (`ok test/e2e 1762.674s`).
3. **Own cluster** in `~/.local/share/review-opus-20`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated and ports 9960–9965 only:
   - `crowdvm-server` on 9960, `--snapshot-interval 20`, with `--public-url` set to the proxy.
   - My own Go TCP proxy on 9965. It paces each connection, in each direction, to 40 MB/s, and idle time earns at most 64 KiB of credit. Nodes joined through it, and the CLI talked to 9960. An image push measured 582 MB in 15.3 s, and a snapshot upload 90.8 MB in 2.35 s, both about 38 MB/s.
   - Nodes nA (UI 9961) and nB (9962): qemu/KVM, 6 vCPU, 12 GiB, 2 sandboxes, 20 s periodic snapshots, image `ubuntu-24.04-crowdvm-minimal-v3`. nC (9963) replaced nB after I removed nB.
   - One slip of mine at the start: I launched two node processes per data dir, so they kept replacing each other's sessions. I killed all four before any sandbox existed and restarted them cleanly. It does not count as a product result.
4. **Judging data.** Every test wrote random files in the guest with `sync` and kept their sha256 on the host. After every resume, fork or deploy I ran `sha256sum -c` inside the guest. Bookkeeping checks used the server DB (snapshot rows, `uploaded_at`, `snapshot_locations`, head, saved snapshots, deletion ops). I also ran an orphan audit on each node: every `vm/disks/*.qcow2` file must be referenced by a `vm/snapshots/*.json` or `vm/vms/*/vm.json` record.

## Fix status

| # | Claimed fix | Status | Evidence (live, 40 MB/s) |
|---|---|---|---|
| opus-19 N1, SIGTERM | Server asks the holder to upload stranded layers (`POST /v1/snapshots/{id}/upload`) | **Fixed** | Sandbox `8qyu` on nA: 800 MB written. `stop --wait` during the periodic upload, then SIGTERM nA. Stop layer `kp3a` (935 MB) was head and unstored. While nA was down, `resume --node nB` answered `409 snapshot_unavailable` in 5 ms, and `snapshot 8qyu sv1` stayed `saving`. I restarted nA. The server logged "asked a node to upload …" on connect, and `kp3a` was stored 25 s later. `sv1` went `ready`. `resume --node nB` was ready in 31 s, with 2 of 2 checksums matching. |
| opus-19 N1, `kill -9` + fork | Same | **Fixed** | `8qyu` on nB: 700 MB written, `stop --wait`, `kill -9` nB. `fork --node nA` answered 409 at once. I restarted nB and started `fork --node nA`, which waited for the upload. I `kill -9`ed nB again 5 s into that upload, kept it down 25 s, and restarted it. The server asked again (stop layer, then parents first), and the fork came up `ready` on nA with 4 of 4 checksums matching. |
| opus-19 N1, saves | A save built on a stranded layer finishes | **Fixed** | Stopped sandbox: see `sv1` above. Running sandbox: `wcs2` on nA, 900 MB written, SIGTERM nA mid-upload (qemu kept running and was adopted), restart, then `snapshot wcs2 svk`. The live layer's parent was the stranded periodic layer. `svk` went `ready` in about 60 s, and `new --from svk --node nC` matched its checksum. |
| HolderOfflineGrace | A boot that waits for an offline holder fails after 2 min | **Works** | `8qyu`: 900 MB written, `stop --wait`, `resume --node nC` (waiting for the upload), then `kill -9` nA 4 s later. The sandbox went `error` 123 s after the kill: "snapshot_unavailable: … the node that holds it is offline; retry once that node is back online". That replaces opus-19's 20-minute `boot_timeout`. I restarted nA and ran `resume --node nC` from `error`: ready in 45 s, 5 of 5 checksums matching. |
| opus-19 N2 | A forced stop falls back to the newest stored layer; live layers become head only once stored | **Fixed** | `hjt7` on nA: P1 `a3ap` with `b1` was stored and head. I wrote 800 MB (`b2`), ran `snapshot hjt7 sv5 --no-wait`, and `kill -9`ed nA during the upload of live layer `vezf`. The head stayed `a3ap`. `stop --force --wait` was immediate. `resume --node nB` was ready in 6 s: `b1` OK, `b2` absent, which Boat documents for a forced stop. I restarted nA: the stale adopted VM was destroyed, `vezf` was uploaded, and `sv5` went `ready`. `vezf` did **not** become the head of `hjt7`, which now ran on nB: its head moved to nB's own periodic layer. `new --from sv5 --node nA` had `b1` and `b2`. |
| sonnet-17 B1 | The QMP overlay switch is not cancelled with the request | **Fixed** | 8 trials of "1.5 GB `dd` in the guest, `stop`, `kill -9` the server after 0.3–2.5 s, restart". 4 hit sonnet-17's exact window (nB log: `sync before snapshot: … context canceled`). Every trial ended `archived`, and the orphan audit found **0** unreferenced overlays after every one. After deleting the sandbox, nB held nothing of it. The code path is now `context.WithoutCancel` around `blockdev-snapshot-sync` (`qemu.go:581`). |
| Head selection (regression check) | Only `stop` layers become head before they are stored | **No regression** | Checked: a live (save) layer never became head while unstored (N2). A late live layer from a returning node did not take the head of a sandbox that had moved. Periodic layers on the new node took the head normally. Stop layers were head at once, and resume and fork waited for them. A fork's lazy chain worked. |
| Deletion (regression check) | Delete leaves nothing | **Holds, see m2** | A fork, two sandboxes, a deploy and 2 saved snapshots were deleted. All ops ended `completed`, with 0 rows and 0 locations, and nA and nC held 0 records, 0 disk files and 0 VM dirs at the end. The exception is m2, which healed on the next reconnect. |

## Blocking findings

None.

## Minor findings (not blocking)

### m1. A stopped sandbox whose unstored stop layer sits on a node that is gone for good answers "retry once that node is back online" forever, even after `nodes remove` (`internal/server/stranded.go:152-168`, `internal/server/nodes.go:969-1003`)

**Repro.**
1. `8qyu` on nB: 800 MB written, `stop --wait`, `kill -9` nB. The head `g2kw` (stop) → `3vsr` (live) → `hba4` (1.6 GB periodic) were all unstored, while older layers of the chain were stored.
2. `nodes remove nB --force --yes` answered `snapshotCopiesLeft: 11`. Afterwards `resume`, `fork` and a second `resume` 35 s later all answered `409 snapshot_unavailable`, with `retryable: true` and "Retry once that node is back online". That node no longer exists. `stop --force` on the stopped sandbox is a no-op. `snapshot latest` still reports the lost head as `"status":"completed"`.
3. **Way out:** `crowdvm snapshot delete snap_g2kw… snap_3vsr… snap_hba4… --yes` moved the head back to the newest stored layer. `resume --node nA` was then ready in 3 s, with a1–a4 matching and a5 lost. Boat's rule is the same: "kept up to the last good snapshot" (`troubleshooting.md:22`, `zero-data-retention.md:86`).

**Why minor.** It needs a node to vanish for good within seconds of a stop (the upload window; about 10 s per GB on a 1 Gbps LAN). The data up to the last good snapshot can be recovered with documented commands, and Boat's own semantics for a failed machine lose the same writes.

**Suggested fix.** When a holder is removed, run `fallBackToStoredHead` for every sandbox whose chain needs a copy that only that node held. The same code m-a uses for running sandboxes would do. Or make the 409 non-retryable and name the `snapshot delete` way out.

### m2. A stop layer the server never recorded is not re-uploaded after its upload gives up, and outlives the sandbox's deletion until the node reconnects (`internal/node/vms.go:542`, `internal/node/transfer.go:217`, `internal/server/deletion_ops.go:294`)

**Repro.** Three of my server restarts hit the node's `StopVM` itself (delays of 0.6, 1.0 and 1.5 s on nA). The node took stop layers `ayqs`, `gfea` and `u4py`, and the server, which died, never recorded them. The stop then ended through the 404 path.
- Those uploads are not reported first (`periodic=false`). Each waited on a 2.5 GB parent upload that every later restart cut, so all three gave up after 5 attempts. The server has no row for them, so neither `uploadStranded` nor `ensureChain` ever asks for them.
- After `delete D`, the op said `completed` with 0 server rows, yet nA still held those 3 layers plus the 5 purged parents they pinned: 8 files, 6.6 GB.
- A node restart erased all 8: `reconcileNodeSnapshots` → `snapshotGone`, logged as "erasing a node's copy of deleted data".

**Data impact.** The lost layer holds only what the guest wrote between the pre-flight snapshot and the power-off. In this race the server restart also aborts prepare-stop (opus-19 m3), so that is close to nothing.

**Why minor.** It needs a server crash at the exact instant of a node's stop. On top of that, it needs either 5 upload failures in a row or a node restart before the upload ends. It heals on the next reconnect.

**Suggested fix.** In `reconcileNodeSnapshots`, record a local layer of a live sandbox that has no row (`recordTaken`) and ask for its upload. Or report server-requested layers too when the reply to the server fails.

### m3. Cosmetic

- A server restart during a stop sends the user a `stop_failing` notice ("node_offline") for the second or two before the nodes reconnect. The stop then completes by itself.
- After a forced stop, `last_snapshot_status` stays `in_progress` until the next resume.
- A deletion blocked by `node_removed` keeps the server's own blobs of that sandbox too. 14 blobs were left in my run, and the op says so honestly.

## Disputed or accepted classifications

- **409 `snapshot_unavailable` while the holder is offline.** Accepted. It is fast, retryable, and names the cause. A temporarily offline node is the normal case on a crowdsourced pool, and once the node is back the retry works (shown twice). The only gap is a node that is gone for good (m1).
- **Data written after the last stored layer is lost on node loss or a forced stop.** Accepted (N2 run). Boat documents this for `--force` (`snapshots__stop.md:146`).
- **A stop completes before its layer is uploaded.** Still accepted. Durable uploads (now fixed) and the fail-fast 409 make it safe for node restarts.
- **Deletion `blocked` / `node_removed` for a node removed while offline.** Correct and honest: the server cannot erase a disk it cannot reach, and the op tells the owner to wipe that node's data directory. One catch: `crowdvm delete` on such a sandbox waits for completion and did not return within several minutes in my run (I stopped it). A `--no-wait` or a "blocked" early exit would help. This is minor, and the op is readable through `crowdvm deletion status`.
- I did not re-grade PARITY.md row by row. Nineteen earlier rounds did. I skimmed `docs/gaps/` and opus-19's and sonnet-17's classifications, and found nothing new to dispute.

## Not verified

- e2e on my own (not allowed). The team's run23 passed on the same tree.
- The window where the server restarts *after* the node switched overlays but before it replies: this is the window that `WithoutCancel` (`blockdev-snapshot-sync`) covers. My trials hit the window just before it (sync/freeze cancelled). I verified the switch path by reading the code: `qemu.go:578-600`.
- A `block-stream` merge cancelled by a server restart (`qemu.go` `stream` uses the request context, and the qemu job keeps running). I read it as harmless for the data (the overlay just holds the streamed data), but did not hit it live.
- Windows nodes, the dashboard in a browser, ZDR, and account close/erase (opus-19 covered ZDR).

## What would flip this to DISAGREE

- Data loss beyond the last stored snapshot in any of the paths above.
- m1 or m2 shown to happen without a crash coinciding with the stop: for example, a stopped sandbox that cannot be recovered at all, or leftovers that a node restart does not erase.

## Teardown

- Sandboxes and saved snapshots: I deleted every sandbox and both saved snapshots. The final audit showed 0 records, 0 disk files and 0 VM dirs on nA and nC. The only remains were the expected `node_removed` copies on the removed nB, and those were inside my data dir.
- Processes: I stopped nA, nC, the server and the proxy by saved PID. nB had already been killed with `kill -9` for the test. No qemu process of mine was left, and ports 9960–9969 were free.
- Files: I deleted `~/.local/share/review-opus-20`. Copies of the server and node logs are in my scratchpad.
- Other work: I touched no other process, and the e2e run23 cluster and the other reviewer's cluster were left alone. The only repo file I wrote is this report.
