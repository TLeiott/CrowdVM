# Review sonnet-22: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent, no sub-agents). Date: 2026-11-11, 07:05–07:35. Tree: uncommitted working tree on `master`, rebuilt with `make build` at 07:09 (`internal/server/lifecycle.go` last modified 06:38). I did not carry over any earlier verdict, including my own sonnet-21.

## Verdict: AGREE

My sonnet-21 blocker is fixed on this build. I reproduced it deterministically and the marker survived. In 17 kill rounds, 3 timing repros and a pool pass I saw no lost synced write and no false "data gone" error. I found one non-blocking leak (m1 below). I found no new missing or broken supportable feature.

## Method

1. **Build and tests.** `make build` is clean. `go test -count=1` over every package except `test/e2e` is `ok` in all packages (`internal/server` 49 s, `internal/node` 16 s). I did not run `make e2e` or `./test/e2e/...`. I read the fix: `lifecycle.go:848-855` (the stop request no longer forces `node_offline`), `:875-883` (`nodeLost`), `:930-945` (`runStop` waits for the node, then forces only after `NodeLostGrace` or if the node was removed). The new tests are in `stranded_test.go:213` and `:228`.
2. **Own cluster** in `~/.local/share/review-sonnet-22`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated, ports 9970 (server), 9971/9972 (node UIs) and 9975 (throttle proxy). Nothing outside 9970–9979 was touched.
   - One `crowdvm-server` with `--snapshot-interval 20`.
   - A Go TCP proxy I wrote that paces each connection and direction to 40 MB/s. Both nodes joined through it.
   - Nodes `nd1` and `nd2` (qemu/KVM, 4 vCPU, 8 GiB, 20 s periodic snapshots), image `ubuntu-24.04-crowdvm-minimal-v3`, one long-lived sandbox per node.
3. **Repro 1 (my sonnet-21 blocker)**: write a 4 KB random marker, `sync`, wait 2 s, `kill -9` the node (qemu survived), wait for `error node_offline`, send a plain `stop`, wait 8 s, restart the node, wait for `stopped`, resume on the other node, `sha256sum -c`.
4. **Repro 2 (node that never returns)**: `kill -9` the node and do not restart it. Send a plain `stop` on one sandbox and `stop --force --wait` on the other.
5. **Kill rounds (17).** Each round writes a 300 MB random file and a 4 KB marker with sha256 and `sync`, then waits 1–3 s. Both sandboxes are always handled, so a round has two sandboxes and I count both, and every sandbox moved nodes in most rounds. Resume goes to the other node unless noted, with retries. Every resume error is grepped for `snapshot_data_expired` / `resume_failed` (none appeared). `sha256sum -c` and the marker line count are checked on the resumed VM.
   - **Rounds 1–9 and 13, 15 (`round.sh`, `round2.sh`): plain `stop`, then a kill.** The server is killed 0.01–1.6 s after the stop request, with `kill -9` or `SIGTERM`, and one node or both nodes get `kill -9` or `SIGTERM` (round 5 is server-only). Resume on the same node in rounds 4 and 7.
   - **Rounds 10–12, 14 (`round3.sh`): stop sent while the node is offline.** Both nodes are killed first. When the server reports `error`, a plain `stop` is sent. Then the server is optionally killed (`kill -9` in round 11, `SIGTERM` in round 12), and nodes restart after 1–5 s. Resume on the same node in round 11.
   - **Rounds 16–17 (`round3.sh`, `KQ=1`): host-reboot case.** The nodes **and all qemu processes** are killed, so the VM is dead when the node returns. Round 17 also kills the server; resume on the same node in round 17.
   - Result: **34 of 34 sandbox-rounds PASS** (every file and marker intact, line count exact). The only `forced=1` stops in the DB are the two deliberate ones of Repro 2.
6. **Pool pass (throttled).** Fork of a running sandbox, fork of a stopped sandbox, saved snapshot + `new --from`, stop and resume of the forks on the other node, delete of forks / the deployed sandbox / the saved snapshot, `SIGTERM` server restart with running sandboxes, `SIGTERM` node restart with a running VM.
7. **Teardown.** I deleted every sandbox, stopped the server, nodes, proxy and qemu by pattern, confirmed ports 9970–9979 were free and no process of mine remained, and deleted the data dir. Logs are in `/tmp/claude-1000/-home-tl-dev-CrowdVM/923af605-63b9-4b99-855e-3c7b84e7e075/scratchpad/keep/`. Disk peaked at about 41 GB of test data (a long-lived sandbox grows by 300 MB per round); free space is back to 145 GB.

## Evidence

### Repro 1 (my sonnet-21 blocker)

```
07:10:21.558 marker written+synced
07:10:23.681 node1 killed -9; qemu: 1
07:10:23.690 state: error ; {"state":"error","error":"node_offline"}
07:10:26.698 after stop request: stopping; stops row: failing|0|1|node_offline: the node hosting this sandbox is offline
07:10:31.706 still waiting: stopping; failing|0|2
07:10:31.707 node1 restarted
07:10:39.735 end state stopped; stops: completed|0|3|snap_ex2f75bjbb78wwmabsvd|
07:10:44.761 resume on nd2 ok (try 1)
mkR1: OK
```

The stop stayed `failing` (not forced, `forced=0`) while the node was away, then completed on the third attempt with a real snapshot layer. The marker survived on the other node. This is the exact scenario that lost data in sonnet-21.

### Repro 2

- `stop --force --wait` while the node was gone returned after **0 s**, state `stopped`, `forced=1`.
- Plain `stop` of the other sandbox stayed pending and ended **195 s after the request (200 s after the node kill)**, `forced=1`, 7 attempts, no layer: it kept the last stored head, like the server's rebuild point (`NodeLostGrace` = 3 min). This matches "about 3 min".
- The stopped sandbox then resumed (same node, once the node was back) and the other one resumed on `nd1` with its earlier marker intact (`mkR1: OK`).

### Rounds

All 17 rounds are in the table above. The nodes' `kill -9` leaves qemu alive in most rounds (re-adopted by the restarted node) and the host-reboot variant (rounds 16, 17) leaves it dead (`qemu alive: 0`); both end `forced=0` with the marker intact, because the node re-adopts or snapshots the overlay it still holds.

### Pool pass

| Check | Result |
|---|---|
| Fork of a running sandbox | content identical (34 lines of `sums`, 0 mismatches) |
| Fork of a stopped sandbox | identical |
| Saved snapshot of a running sandbox and `new --from` | identical, 5.7 GB snapshot |
| Stop + resume of both forks on the other node | identical, 0 mismatches |
| Delete of forks, deployed snapshot sandbox, a queued (still `cloning`) fork, and `snapshot rm` | `deletion` ops `completed`, no VM or layer files of those ids left on either node |
| Server `SIGTERM` restart with a running sandbox | stayed `ready`, `exec` works |
| Node `SIGTERM` restart with a running VM | VM re-adopted, `ready`, data intact |
| Fork while both nodes are full (`max-sandboxes 2`) | stays `cloning`, no node; deletes cleanly (expected queueing) |

## Fix status

| Item | Status | Evidence |
|---|---|---|
| sonnet-21 B1: plain `stop` of a `node_offline` sandbox was forced and discarded the node's newer disk | **FIXED** | Repro 1: `forced=0`, marker survived; rounds 10–12, 14, 16, 17 (stop sent while the node is offline, with and without server kill and dead qemu) all PASS; code `lifecycle.go:848-855`, `:930-945`; tests `stranded_test.go:213`, `:228`. |
| Node that never returns | **OK** | Stop ends after `NodeLostGrace` (195 s), forced onto the last stored head. |
| `stop --force` | **OK** | Returns at once. |
| sonnet-21 m1: orphan VM in `error` never deleted | **FIXED for the cases it named** | `lifecycle.go:574-576` now deletes `running`/`starting`/`error` orphans of deleted or moved sandboxes. The same-node archived case is a new, smaller gap (m1 below). |
| sonnet-21 m2, m3 (honest `forced` flag, transient notice) | unchanged, minor | `forced=1` only when I asked for it. |

## New findings

### Non-blocking

- **m1. A VM that survives on its node while the sandbox is force-stopped (`archived`, same `node_id`) is neither deleted nor stopped when the node reconnects** (`internal/server/lifecycle.go:571-583`). The orphan check only fires for a deleted sandbox or a sandbox with a different `node_id`. I saw this in Repro 2: node `kill -9`, qemu kept running, the plain stop ran out the 3 min grace, the node came back at 07:14:20 and "adopted running VM"; the sandbox was `archived` and the VM kept running (6 qemu argv lines, same pid) for more than 2 min until I resumed it. A resume on that same node then failed on the first try with `vm_exists` ("vm bx_… already exists (running)", `sandbox_error`) and worked on the second try, because the error path cleaned the VM. Data is not at risk (the stop was forced and the head is the last stored layer), but a volunteer's node holds CPU, RAM and disk for a sandbox that is not running, and the first resume on that node errors once. Supportable: it is the same one-line rule as the orphan case (treat a `running`/`starting`/`error` VM as an orphan when the sandbox is `archived`, or when a resume sees `vm_exists`, delete and retry once). It needs a node that is away more than 3 min and comes back with the VM alive (a network drop or a laptop sleep), then a stop in that window, which is not common.
- **m2. A soft-deleted periodic snapshot stays unpurged while it is a parent** (6 rows after the pool pass, all periodic layers of the two live sandboxes). Looks like chain-parent retention, not a leak; I did not check it further.

## Disputed classifications

- **N/A rows in `docs/PARITY.md` (26 rows, matching the summary).** I read each one: Google and AgentID sign-in, Stripe wallet and plan changes, trials, SOC 2 / status page, support channels, legacy keys and the `box` migration are all third-party, payment or operator-process items. Accepted as unsupportable under the constraints.
- **m1 severity.** It loses no data and clears itself on the next resume (the error path cleans the stale VM), so it is minor, not blocking.
- **My sonnet-21 B1 reasoning** that a plain stop of `node_offline` must wait: the team's choice (wait up to 3 min, then force like the rebuild) matches what I asked for, and the 3-min fallback only ends a stop for a node that is not coming back soon. Accepted.

## Not verified

A real 1 Gbps link without the throttle; Windows nodes; a TTL auto-stop while the node is offline (it goes through the same `runStop` path, so I expect the same wait, but I did not run it); `docs/PARITY.md` row by row (I spot-checked N/A rows and the fix area only); the dashboard in a browser; ZDR; account close/erase.

## What would flip a DISAGREE to AGREE

Not needed. For the record, a DISAGREE would need a lost synced write or a false "data gone" error on a plain stop, or a plain stop of a `node_offline` sandbox that ends `forced` before `NodeLostGrace`. I saw none in 34 sandbox-rounds.

Optional polish: m1 (delete or stop a surviving VM of an `archived` sandbox on node reconnect).
