# Review sonnet-21: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent, no sub-agents). Date: 2026-11-11, 05:52–06:40. Tree: uncommitted working tree on `master`, built at 05:52 (`make build`; `internal/server/lifecycle.go` last modified 05:50:30). I did not carry over any earlier verdict, including my own sonnet-20.

## Verdict: DISAGREE (one blocking data-loss path)

All three fixes the team listed hold on this build. The sonnet-20 `fork` finding is gone, and I saw no lost write from the per-stop layer id or the 404 adoption. But I found a different path that loses synced data on a plain `stop`:

- **B1. A plain `stop` on a sandbox whose node is offline is silently forced, completes at once with no layer, and discards the node's newer disk** (`internal/server/lifecycle.go:849-851`, `:916`). I reproduced it deterministically. I also hit it by chance in kill round 15, which lost the marker file. In the repro the VM was still alive and healthy on the node when the stop discarded it. The marker had been synced 2 s before the stop. This is the blocking class: "data loss of synced writes".

Everything else I exercised passed (see the tables). I also list one non-blocking node disk leak (m1).

## Method

1. **Build and tests.** `make build` is clean. `go vet ./...` is clean. `go test -count=1 ./...` is `ok` in every package (`internal/server` 51.9 s). `test/e2e` skipped itself because `CROWDVM_E2E` was unset. I did not run `make e2e`.
2. **Own cluster** in `~/.local/share/review-sonnet-21`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated, ports 9970 (server), 9971 and 9972 (node UIs) and 9975 (throttle proxy). Nothing outside 9970–9979 was touched.
   - One `crowdvm-server` with `--snapshot-interval 20`.
   - A Go TCP proxy that paces each connection and direction to 40 MB/s. The nodes joined through it.
   - Nodes `nd1` and `nd2`, qemu/KVM, 4 vCPU, 8 GiB, 20 s periodic snapshots. Image `ubuntu-24.04-crowdvm-minimal-v3`.
   - Another reviewer's cluster ran at the same time; I touched none of its processes.
3. **`fork` repro.** Put a stopped sandbox in the state "no head, layers not yet uploaded" with the DB (`snapshots.uploaded_at=NULL`, `sandboxes.head_snapshot_id=NULL`). Then call `fork` and `resume`.
4. **Kill rounds** (`round.sh`, `round2.sh`, `drive.sh`). Two long-lived sandboxes, one per node. Each round:
   - Write a 300 MB random file and a 4 KB marker, record their sha256 in `sums`, `sync`.
   - Wait 1–3 s, then `crowdvm stop` on both.
   - After 0.01–1.6 s, kill the server (`kill -9` or `SIGTERM`).
   - Also kill a node (`kill -9` or `SIGTERM`): the node of sandbox 1 in `round.sh`, **both nodes** in `round2.sh`, and in `round2.sh` the nodes before the server. Restart server and node(s).
   - Re-issue `stop` until `stopped`. Read `stops.forced` from the DB.
   - Resume on the other node (most rounds) or the same node. Retry on errors and grep every error for `snapshot_data_expired` or `resume_failed`.
   - Check `sha256sum -c sums` is all OK and the line count is the expected one. A missing marker line is lost synced data.
   - The sandboxes swapped nodes each round, so each node ended up holding about 40–50 layer files (46 and 47 layers per sandbox on the server).
5. **Pool pass** (`p2a.sh`, `p2b.sh`), see below.
6. **Teardown.** I deleted every sandbox and saved snapshot, then stopped my server, nodes, proxy and qemu processes by pattern. No process of mine remained, 9970–9979 were free, and I deleted the data dir. Logs were copied to `/tmp/claude-1000/-home-tl-dev-CrowdVM/f671e4e8-658e-4715-be08-37c122d63c89/scratchpad/keep/`.

**Harness bug on my side (not a product finding).** My first batch of "rounds 2–15" (cluster 1) was invoked through zsh without word splitting, so the script got one combined argument. Those rounds ran with no delay, a `SIGTERM` server restart, no node kill and a same-node resume. I discarded them, and the follow-up `round2.sh` runs left duplicate node processes on the same data dirs. I killed everything, deleted the two sandboxes, built a fresh pair and re-ran all rounds with correct arguments. Only the second set below is evidence. (Side note: two `crowdvm-node` processes can run on one data dir without a lock. I treat that as operator error, not a finding.)

### Part 1: `fork` and `resume` of a stopped sandbox with layers still uploading

Sandbox stopped with a stored head, then put in the state "no head, layers uploading" through the DB.

| Call | Result |
|---|---|
| `crowdvm fork ID --json` | `409 snapshot_unavailable`, "still being uploaded from its node. Retry in a minute.", `details:{retryable:true, retryAfterSeconds:30}`. Correct. |
| `crowdvm resume ID --no-wait --json` | Same error, same details. Correct. |
| Control: all layers marked deleted | `fork` and `resume` both answer `snapshot_data_expired`, `retryable:false`. Correct (true final). |
| Restore the head and `uploaded_at` | `fork` works; the fork booted and had the file I wrote. |

The CLI `--json` error output includes `details` (seen above).

### Part 2: stop cut by server and node kills (valid cluster, 16 rounds, 32 sandbox stops)

| Rounds | Server kill | Node kill | Resume | Result |
|---|---|---|---|---|
| 1–7 (`round.sh`) | `-9`/`TERM`, +0.05–0.5 s | node of sandbox 1, `-9` ×4, `TERM` ×3 | other ×5, same ×2 | 14 of 14 pass. Rounds with a retried stop and a `snapshot_unavailable` resume retry (3 seen) resolved on their own. |
| 8, 10, 13, 15 (`round2.sh`) | `-9`/`TERM`, +0.2–1.6 s | **both nodes**, nodes killed first | other ×3, same ×1 | 7 of 8 pass: rounds 8, 10 and 13 pass (6 stops); in round 15 one stop passed and **one lost its marker** (B1). |
| 9, 11, 12, 14, 16 (`round.sh`) | `-9`/`TERM`, +0.15–1.2 s | node of sandbox 1, `-9` ×2, `TERM` ×3 | other ×5 | 10 of 10 pass. |
| **Total** | | | | **31 of 32 pass; 1 data loss** (round 15, sandbox `bx_46uysg4h`, 28 lines found, 30 expected). |

Other observations from these rounds:
- 4 stops ended `forced=1` (round 5, 14, 15, 16); the other 28 ended `forced=0`. In rounds 5, 14 and 16 the node was back in time, so the forced stop snapshotted what the node still had and nothing was lost. Round 15 is the loss.
- Zero `snapshot_data_expired` or `resume_failed` on any resume. The only resume error seen was the retryable `snapshot_unavailable` ("not registered yet … Retry in a minute").
- A synced write was lost in none of the other 31 stops. The stops that went through attempts 1–3 (server restart, node still offline, node back) ended with a stop layer and `forced=0`.

### Part 3: pool pass (throttled, same long-lived sandboxes)

| Step | Result |
|---|---|
| `fork` of a stopped sandbox, to the other node | all 30 lines OK |
| `fork` of a running sandbox, to the other node | all 32 lines OK |
| Saved snapshot of a running sandbox (5.4 GB), 50 MB more data, `new --from` on the other node | 32 of 32 lines OK; none of the post-save file |
| `SIGTERM` of the node that held the layers, restart, resume on the other node | 30 of 30 lines OK |
| `stop --wait`, `kill -9` of the other node, restart, resume | 30 of 30 lines OK |
| `delete` of a running sandbox, `kill -9` its node 0.3 s later, restart 8 s later | 0 live rows |
| `snapshot rm`, delete the last sandbox | server: 0 unpurged rows, 0 locations, 0 blobs, 0 `.upload-*`. 0 qemu processes. Node 2: base image only. Node 1: see m1. |

## Fix status

| # | Claim | Status | Evidence |
|---|---|---|---|
| sonnet-20 B1 | `fork` of a stopped sandbox whose layers still upload answers a retryable error | **Fixed** | Part 1: `snapshot_unavailable`, `retryable:true`, same as `resume`. `fork` (`lifecycle.go:1436`) and `resume` (`:1270`) both call `noHeadErr` (`:874`). Control with no layers still answers the final `snapshot_data_expired`. |
| `resume` still retryable | | **Holds** | Part 1. |
| Each stop keeps one layer id across attempts and restarts, and a 404 adopts exactly that layer | | **Holds in 31 of 32 stops** | `lifecycle.go:942-949`, `nodeLayer` `:1009`. 16 rounds with server and node kills, sandboxes with 40–50 layers on the node. 30 of the stops ran through 3 attempts. No stale or wrong layer adopted. I could not isolate which stops took the 404 branch itself (no log line), so I do not claim coverage of that branch from live evidence alone. |
| CLI `--json` errors include `details` | | **Fixed** | `fork`/`resume` errors above carry `details.retryable` and `details.retryAfterSeconds`. |
| Plain `stop` never loses a synced write | | **Not met** | B1. |

## Blocking finding

### B1. A plain `stop` on a node-offline sandbox is forced at once and discards the node's disk (`internal/server/lifecycle.go:849-851`, `:916`)

**Contract.** The code's own docs say a node-lost sandbox "keeps its machine and disk for 3 days" and that "CrowdVM waits for the node" (`lifecycle_holds.go:7-15`, `onSandboxError`). A stop that cannot save is retried and only forced after 3 days (`reapHolds`). Boat's plain stop never loses data either; only `--force` does.

**Cause.** When the node goes offline `onNodeOffline` moves the sandbox to `error` with `error:"node_offline"` (`lifecycle.go:659-675`). `requestStop` then does `if sb.State == api.StateError { force = true }` (`:849`) for every error, including `node_offline`. `runStop` takes the branch `node == nil && op.Forced` (`:916`) and completes the stop immediately with `finishStop(sb, op, "", true)`, falling back to the last stored head. The disk on the node is abandoned, and the live VM there is removed.

**Repro A (deterministic, live):**
1. New sandbox on `nd1`. Wait for a periodic snapshot. Write `MARKER-OFFLINE-1`, `sync`, `cat` it, wait 2 s.
2. `kill -9` `nd1`. The sandbox goes to `error`, `error:"node_offline"`.
3. `crowdvm stop ID --json` (no `--force`). Answer: `{"event":"stopping", "operation":{"id":"stop_9b85…","status":"pending","forced":true}}`. 3 s later the sandbox is `stopped`; the DB shows `stop_9b85… | completed | forced=1 | attempts=1 | snapshot_id=NULL`.
4. Restart `nd1`. Its log shows `qemu: adopted running VM bx_wzm42rtg (pid 1413762)` and `guest healthy`. **The VM with the synced marker was alive on the node.** The server then removed it.
5. `crowdvm resume ID --node nd2`: `cat /home/user/mk` → `No such file or directory`. The synced marker is gone.

**Repro B (natural, round 15 of `round2.sh`):** both nodes `kill -9`, server `SIGTERM` 0.1 s later. The first stop was refused by the dying server ("save failed … EOF"). 1.0 s after the server restarted my harness re-issued `stop` (a retry any agent would do) at 06:27:23.909, 7 ms before the node reconnected (06:27:23.916). The sandbox was in `error`/`node_offline`, so the stop was forced and finished at once (`stop_93248b1a…`, `forced=1`, no layer). The resumed sandbox lacked the marker (28 of 30 lines). The other sandbox of that round, whose `stop` landed after its node reconnected, was fine.

**Why it is supportable on a LAN.** The behavior already exists in the code for non-forced stops: a stop with the node absent calls `fail("node_offline: …")` (`lifecycle.go:922-925`) and the reaper retries it until the node is back (then the node re-adopts the VM and snapshots it, as in repro A step 4). So the fix is to not force the stop for `node_offline`. A user who really wants to give up the disk still has `crowdvm stop --force`, and after 3 days `reapHolds` forces it. No network, payment or OAuth dependency. Node loss is a CrowdVM-only situation (nodes are volunteer machines that reboot and drop off), so this is exactly the data-safety rule the product claims.

**Scope.** It needs a `stop` while the node is offline, not only a node reboot. Any agent that sees `error` and runs `stop`, and every plain stop issued during a node restart (like my harness), can hit it. A node reboot or network blip of a few seconds is enough. It loses all writes since the last stored layer, up to everything since the last periodic snapshot.

## New non-blocking findings

- **m1. A VM left in `error` on a node, whose sandbox was deleted or moved, is never cleaned up** (`internal/server/lifecycle.go:574-576`). The orphan handler only deletes orphan VMs in `running` or `starting`. Evidence: sandbox `bx_6bjjxcg9` was deleted at 06:17:42, apparently while its node was still offline (the node connected at 06:18:08); the node adopted `vm bx_6bjjxcg9 state=error` ("qemu not running after node restart (overlay kept)") at 5 or more node restarts over 15 minutes, and kept its VM dir and 5 layer chains (~5 GB) the whole time. The server DB had 0 unpurged rows. The same state arises when a node reboot kills its VMs and their sandboxes are rebuilt elsewhere (`lifecycle_place.go:246`). It does not lose data and does not hit Boat parity, but a volunteer's disk grows with every such event. Fix: also issue `DELETE /v1/vms/{id}` for orphan VMs in `error` and `stopped`. This is the same as sonnet-20 m1 (there it cleaned itself after a restart; here it did not, because the VM was adopted as `error`).
- **m2.** After a `kill -9` of a node, `qemu` of a sandbox sometimes survives (re-adopted as `running`) and sometimes does not (`qemu not running after node restart`, sandbox `error`, then a forced snapshot of the node's overlay). In every case where the node was back the data survived. This is just the source of the `forced=1` stops in rounds 5, 14, 16, and the forced flag is honest.
- **m3.** Same notice behavior as before: a `stop_failing` / `node_offline` notice appears for a few seconds after a restart.

## Disputed classifications

- **"Rare crash window" argument for B1.** I do not accept it as minor. It needs no crash timing: repro A is deterministic once the node is down and someone calls `stop`. The harm is permanent and silent (the answer says `stopped`, `forced:true` only if the caller reads the operation). The rule in the brief is explicit: data loss of synced writes is always blocking.
- **Data written after the last stored layer when the node is gone for good** stays lost on a forced stop. Accepted for an explicit `--force` or after the 3-day hold; not accepted for a plain stop while the node is merely offline.
- **My harness errors** in the first batch are excluded; see Method.

## Not verified

- Which stops took the `nodeLayer` adoption branch (no log line). The code, the two servertests the team added and 31 clean stops make me believe it, but the live evidence is indirect.
- A real 1 Gbps link without the throttle; Windows nodes; the dashboard in a browser; `docs/PARITY.md` row by row; ZDR; account close/erase.
- I did not run a TTL auto-stop while a node is offline. It probably goes through the same forced path if the reaper stops `error` sandboxes; check it when you fix B1.

## What would flip this to AGREE

1. **B1.** A plain `stop` of a `node_offline` sandbox must not be forced. Keep it pending/`failing` with `node_offline`, retry when the node returns, and snapshot the re-adopted VM. Add a servertest next to `nodes_test.go` that stops a node-offline sandbox and asserts it stays `archiving`/failing, not `stopped` with `forced`. Then re-run repro A (marker survives resume) and a few `round2.sh` rounds. I would accept the fix on code plus test plus repro A.
2. Optional: m1 (delete orphan `error`/`stopped` VMs on the node).

Everything else in the checklist held in my runs on this build.
