# Review opus-21: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, no sub-agents). Date: 2026-11-11, 02:14–03:20. Tree: uncommitted working tree on `master`. I built with `make build` at 02:14, after the last Go edits (the B1/B2 fixes in `lifecycle.go`, `server.go`, `deletion_ops.go` and `store/blobs.go`). I copied the binaries into my own dir. No Go file changed while I tested.

## Verdict: DISAGREE (one blocker)

Both sonnet-18 blockers are fixed, and I checked each one live behind a 40 MB/s throttle:

- **B1.** A resume during a parent gap is now retryable, and a purged layer is still final.
- **B2.** No `.upload-*` file survives a restart.

opus-20's main cases did not regress.

But the same server-restart-during-stop runs show a real data loss that I under-rated as minor m2 in opus-20:

- **N1.** A plain `stop` (not `--force`) that a server restart interrupts can end as `forced`. The sandbox then resumes from an older layer and loses synced files that the online node still held.
- I reproduced it 2 times in 4 comparable trials.
- No node was lost. The node uploaded the layer with the missing data 6–15 s later, but by then the server had already moved on without it.

That is data loss beyond Boat's "last good snapshot" rule, on a path the user never asked to force. It is pure bookkeeping, so it can be fixed on a LAN.

## Method

1. **Read the fix code.**
   - `chainRestorable` (`internal/server/lifecycle.go:31-55`) and its caller `lifecycle.go:1277`.
   - `snapshotChain` (`snapshots.go:49`).
   - `Blobs.Put` and `Blobs.CleanTemp` (`internal/server/store/blobs.go:52-160`).
   - The `CleanTemp(0)` call at start (`server.go:304`) and `CleanTemp(time.Hour)` in `sweepOrphanBlobs` (`deletion_ops.go:313`).
   - `uploadStranded` and `chainReachable` (`stranded.go`), `uploadAsync` (`internal/node/transfer.go:186`), `runStop` (`lifecycle.go:870-977`) and `StopVM` (`internal/node/vms.go:509`).
   - `grep` confirms that no code path hard-deletes `snapshots` rows. A purged layer always keeps its row with `purged_at` set, so it can never look "not yet recorded".
2. **Build and tests.** `make build` and `go vet ./...` are clean. `go test -count=1 ./...` is `ok` in every package (`internal/server` 48 s). The new tests `TestCleanTemp` and `TestResumeParentNotYetRecorded` are included. I did not run e2e.
3. **Own cluster** in `~/.local/share/review-opus-21`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated and ports 9960–9965 only:
   - `crowdvm-server` on 9960, `--snapshot-interval 20`, with `--public-url` set to the proxy.
   - My own Go TCP proxy on 9965: 40 MB/s per connection and direction, at most 64 KiB of credit. Nodes joined through it. An image push measured 582 MB in 15 s.
   - Nodes n1 (UI 9961) and n2 (9962): qemu/KVM, 6 vCPU, 12 GiB, 2 sandboxes, 20 s periodic snapshots, image `ubuntu-24.04-crowdvm-minimal-v3`.
4. **Judging data.** Every write was `dd` from `/dev/urandom`, then `sync` and `sha256sum` in the guest. I kept the sums on the host. After every resume, fork or deploy I ran `sha256sum -c` in the guest. Bookkeeping checks used the server DB (rows, parents, `uploaded_at`, `purged_at`, stops, deletion ops). Leftovers were checked with `ls -A` (dot-files included).
5. **Harness slips** (not product results):
   - My first `waitsrv` treated the 401 from `/api/v1/server` as "down", so the first restarts kept the server down 20 s instead of 0.2 s. I fixed this before the B1 rounds.
   - A zsh word-splitting bug made two rounds kill and restart the server without issuing a stop. They are not counted.
   - One file (`a3`/`b3`) was written twice by overlapping trial scripts, and only the later sum counts.

## Fix status

| # | Claimed fix | Status | Evidence (live, 40 MB/s) |
|---|---|---|---|
| sonnet-18 B2 | `CleanTemp` at server start, and in the orphan sweep | **Fixed** | 5 server kills during 0.7–1.2 GB periodic uploads on both nodes: 3× `kill -9`, 2× SIGTERM, kill delays 3–12 s into the upload. After `kill -9`, the 1–2 partial files (up to 336 MB) were on disk at death. After a SIGTERM they were gone once (graceful), and once two remained. **After every restart, none of the files present at death existed any more.** The only `.upload-*` names afterwards were new in-flight re-uploads, and those disappeared once the uploads finished. At the end of the run, after deleting everything, `ls -A server/blobs/snapshot` was empty (0 entries). |
| sonnet-18 B1 | A missing parent row is a retryable 409 `snapshot_unavailable`, and only a purged row is final | **Fixed** | Natural gap: in one stop-plus-`kill -9` round, a live layer `4cz2` had a missing parent `q5z5` for about 40 s. It was on a side branch, not on the head chain, so the resume was accepted and booted. The data matched. The gap closed by itself when n2's parent-first upload stored `q5z5` (02:44:31). I did not hit a head-chain gap naturally in about 8 rounds, so I drove the path on my own DB with the server down. Head chain `nj58→ywkm→nuwm→…`: I set `nuwm.parent_id` to a missing id, and the resume answered `409 snapshot_unavailable`, "not registered yet … Retry in a minute" (retryable). I restored it, and the resume was ready with 9 of 9 files. |
| B1 must not hide a lost chain | A purged layer is still final | **Holds** | Same chain, `purged_at` set on `nuwm`: the resume answered `409 resume_failed`, "snapshot … no longer exists. Deploy a saved snapshot or create a new sandbox." (code: `retryable:false`). Rows are never hard-deleted, so a purged layer cannot fall into the retryable "missing" branch. |
| opus-19 N1 (regression) | A stranded unstored stop layer is uploaded on request | **Holds** | 700 MB written, `stop --wait`, `kill -9` n2 at once. `resume --node n1` and `fork --node n1` each answered `409 snapshot_unavailable` ("node … is offline. Retry once that node is back online"). After I restarted n2, `resume --node n1` was ready, with 14 of 14 checksums matching. |
| opus-19 N2 (regression) | A forced stop after node loss falls back to the last stored layer | **Holds** | 600 MB written, `kill -9` n2 4 s into its upload, `stop --force --wait`, `resume --node n1`. Ready, with 9 of 9 older files present and the unstored `h1` absent (Boat's forced-stop rule). When n2 came back it adopted the stale qemu, and that qemu was gone 15 s later: only n1's instances remained. |
| Fork, save, deploy, delete (regression) | — | **Holds** | Saved snapshot `sv21` was `ready`. A fork to n2 and a `new --from sv21` on n2 each had 15 of 15 checksums matching. After 4 deletes and `snapshot rm`: 4 of 4 ops `completed`, 0 unpurged rows, 0 locations, 0 server blobs, 0 `.upload-*`, and on both nodes only the base image (0 overlays, 0 records, 0 VM dirs). |

## Blocking findings

### N1. A server restart during a plain `stop` turns it into a forced stop and drops data the online node holds (`internal/server/lifecycle.go:949-953`, `internal/node/vms.go:509-512`, `internal/server/snapshots.go:187-214`)

**Boat:** a plain stop snapshots the disk, and the sandbox resumes with everything written before the stop. Only `--force` keeps "the last successful snapshot" (`snapshots__stop.md`; also the CLI's own help: "--force: stop now and keep only the last successful snapshot (loses newer data)").

**Repro (live, 2 of 4 comparable rounds; the other 2 kept their data).** Each round:
1. A running sandbox. Write 300–900 MB, `sync`, then 300–500 MB more, `sync`.
2. Plain `crowdvm stop <id>`. `kill -9` the server 0.1–0.5 s later, then restart it at once.
3. Once the sandbox is `archived`, `resume --node <other>`.

| Round | Kill delay | Result |
|---|---|---|
| `bx_db2cqv6p`, 02:48:23 | 0.1 s | `d_s28894` (500 MB, synced before the stop) missing after the resume |
| `bx_m2mj2z2p`, 03:06:22 | 0.3 s | `d_r10080` (300 MB) and `d_s30456` (500 MB) missing |
| two other rounds | 0.1 s, 0.5 s | data intact, stop not forced |

In both losses, `stops.forced = 1` and `snapshot_id = NULL`. In the 02:48 round, the guest was writing nothing else and both nodes were online the whole time.

**What happens (node and server logs).**
1. The server records the stop's live layer (`mygw`, 526 MB, holding the new file). The node starts the stop.
2. The server dies. The node's `prepare-stop` gets `context canceled`, and the node logs "stopping anyway". It finishes the stop 77–281 ms later ("stopped as snap_e38x"), but its reply has nowhere to go.
3. The server restarts. The first retry gets `node_offline` (the node has not reconnected yet). The second retry, about 8 s later, gets `404 vm not found`, because the node dropped the VM after stopping it.
4. `runStop` reads the 404 as "machine already gone" and calls `finishStop(…, forced=true)` (`lifecycle.go:952`). The head stays on the newest **stored** layer. The live layer `mygw` is recorded but not yet stored (opus-19 N2 rule: live layers become head only once stored), and the stop layer `e38x` is unknown, so neither becomes head.
5. The resume is accepted from that older head.
6. 6–15 s later the node finishes uploading `mygw` and reports `e38x`. The server records `e38x` "as deleted" because the sandbox has moved (`snapshots.go:214`), and the data is gone for the user.

**Why it blocks.** It loses data a user wrote and synced before a plain stop. Nothing failed except a server restart, and the node holding the data stayed online. That is beyond Boat's "last good snapshot" rule, which only covers a forced stop or a lost machine. My opus-20 m2 saw the 404 path but called the loss "close to nothing". That holds only when the in-flight layers are tiny. Here the stop's own live layer held up to everything since the last stored periodic layer: 0.5–0.8 GB, the last 10–30 s of writes on this link. The window is the stop's prepare and snapshot phase (about 1–2 s per stop), not a few milliseconds.

**Why it can be fixed on a LAN.** It is pure node/server bookkeeping, and either of these would do:
- **Node:** remember the result of the last completed stop per sandbox (the ref is already in `vm/snapshots/*.json` with its sandbox id) and answer a repeated `POST /v1/vms/{id}/stop` with that same ref instead of 404, so the retry records it as the head.
- **Server:** on the 404, ask the node for its newest layer of this sandbox (`nodeSnapshots`, which `reconcileNodeSnapshots` already uses), record it with `recordTaken` and make it the head before finishing the stop. At minimum, make a recorded, unstored live or stop layer on an *online* holder the head instead of the stored fallback, and finish the stop as not forced.

**Test to add.** A servertest: the node completes a stop but the reply is lost (server restart), and the retry gets a 404. Assert that the head is the node's stop layer (or at least the recorded live layer), that `forced` is false, and that a resume on another node waits for and boots that layer.

## Minor findings (not blocking)

- **m1 (follow-up to B1).** A parent row that never arrives makes the resume answer a retryable 409 for ever. Today that needs the node to lose a layer it never reported. The orphan branch I saw healed in about 40 s through `uploadStranded`'s parent-first upload, and I found no path that leaves it permanent. A cap ("still missing after N minutes → final, deploy a saved snapshot") would make the error honest.
- **m2.** After a server restart during a stop, the user still gets a `stop_failing` / `node_offline` notice for the seconds before the nodes reconnect (opus-20 m3, unchanged).
- **m3.** The CLI's `--json` error drops `retryable` and `retryAfterSeconds` (sonnet-18 m1, unchanged). The message text says what to do.
- **m4 (opus-20 m1, unchanged).** A stop layer stranded on a node that is removed for good still answers "retry once that node is back online". I did not re-test it.

## Disputed or accepted classifications

- **opus-20 m2 is reclassified as blocking (N1 above).** I accepted it last round on a wrong estimate of the lost data.
- **B2 with SIGTERM.** A graceful shutdown sometimes removes the temp files itself and sometimes leaves them. Start-up `CleanTemp(0)` covers both cases. The sweep's one-hour rule is safe because an active upload keeps touching its file.
- **B1 is classified the way sonnet-18 asked.** The error is retryable with a hint while the row may still arrive, and final only when the row is purged.
- **The forced stop after node loss loses `h1`.** Accepted: that is Boat's rule for a lost machine.
- I did not re-grade PARITY.md row by row. Twenty earlier rounds did, and nothing in this round touches the classifications.

## Not verified

- A head-chain parent gap that arose naturally. I used a DB edit for the B1 head-chain check, and a natural gap (on a side branch) for the heal path.
- The team's newer e2e run24 (`e2e-run24.log`, built 02:14 on this tree) ended **`EXIT=1`**: `snapshots` and `process-history` failed, 36 PASS.
  - `process-history` timed out waiting for an "exit 3" record.
  - I did not find the failing assertion in `snapshots` from the log alone.
  - My cluster ran at the same time (02:14–02:34), and its throttled uploads and restarts loaded the machine, so both may be timing failures. I did not investigate further, and I am not counting them as findings. The last passing run (run23) predates the B1/B2 edits.
- Windows nodes, the dashboard, ZDR, and account close/erase.

## What would flip this to AGREE

1. **N1.** Run 8 or more rounds of "write and sync data, plain `stop`, `kill -9` the server 0.1–1.5 s later, restart, resume on the other node". Every round ends with every synced file present, and no plain stop is recorded `forced` while its node is online. Add the servertest above.
2. run24's two e2e failures are either fixed or shown to be timing flakes by a clean rerun on this tree.

## Teardown

- Sandboxes and saved snapshots: I deleted every sandbox (4) and the saved snapshot. The final audit showed 0 rows, 0 locations, 0 blobs and 0 `.upload-*` files on the server, and only the base image on n1 and n2.
- Processes: I stopped n1, n2, the server and the proxy by saved PID (SIGTERM, all exited). No qemu process with my data dir remained, and nothing listened on 9960–9969.
- Files: I deleted `~/.local/share/review-opus-21`. Copies of the logs are in my scratchpad (`/tmp/claude-1000/-home-tl-dev-CrowdVM/df1ba18d-ecde-4dee-a132-89469dce67e3/scratchpad/logs/`).
- Other work: I touched no other process or cluster. The only repo file I wrote is this report.
