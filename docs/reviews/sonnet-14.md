# Review sonnet-14: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. I used no sub-agents.

## Verdict: AGREE

I found no blocker. All four fixes I was asked to verify hold live, including my own `sonnet-13` snapshot reproduction.

The new sweep was a long-lived-usage session. It ran one sandbox through 10 stop/resume cycles with a 20 s periodic snapshot (69 merges logged) and forks at 3 different generations. It took 4 saved snapshots and deployed each, several of them several times. It deleted saved snapshots, ordinary snapshots, a snapshot source, a fork's snapshots and every sandbox, and it ran a retention sweep. Every deploy, resume and fork held exactly the data it should. `snapshots`, `snapshot list`, `snapshot rm`, `snapshot delete` and `deletion status` made sense at each step. Node disk use was reclaimed down to the bare image at the end.

I found two issues. Neither blocks, and neither stops an agent from doing a Boat task:

- **n1.** A `new --from NAME` that is still waiting for capacity, if stopped and later resumed, boots an empty disk instead of the saved snapshot.
- **n2.** `crowdvm limits` says "no node in the pool is big enough" for a size that only a paused node could host.

## Method

1. **Read.** `sonnet-13.md` and `opus-15.md` (claims and flip criteria), the node snapshot code (`transfer.go`, `qemu/lazy.go`, `qemu/store.go`, `mergePlan` in `qemu.go`), the retention job (`feat_retention.go`), `orgs.go` delete, and `lifecycle_place.go`/`scheduler.go`.
2. **Build and tests.**
   - I built the four binaries with `go build` into my scratchpad (the same commands as `make build`), so I did not rewrite `bin/` under the other reviewer.
   - `go vet ./...` is clean.
   - `go test -count=1` on every package except `test/e2e`: all `ok`, 0 FAIL.
   - I did not run e2e. The e2e run16 log ends `EXIT=0` (37 PASS; SKIP only for `agent/opencode` and `bootloop`). Its fix files are older than that run (last server edit 19:09, run16 ended 19:19).
3. **Own cluster.**
   - One server on 127.0.0.1:9970 (limits off, 1000 signup credits).
   - Node `n1` (UI on 9971): qemu/KVM, 8 vCPU, 18 GiB, 4 sandboxes, image `ubuntu-24.04-crowdvm-v3`. `--snapshot-interval 20s`, so a level-1 merge happens about every minute of runtime.
   - A second node `n2big` (UI on 9972; 32 vCPU, 80 GiB nominal) used only to be a paused big node. No VM booted on it.
   - Data in `~/.local/share/review-sonnet-14`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated.
   - Users `admin` and `bob`. I deleted the data dir at the end.
4. **Long-lived session** (all live, real CLI; each sandbox got a small `gN.txt`, an 8 MB random `blobN.bin` and a journal line per generation; `verify` checked the exact file list, the journal, and that each blob's sha256 matched the one recorded at write time):
   1. **Sandbox A.** 10 generations, each followed by a 22–25 s wait (one or two periodic snapshots and a merge), `stop`, `resume`, verify. 69 `merged N layers` log lines overall.
   2. **Forks.** F3 at gen 3, F6 at gen 6, both from a live sandbox with `fork --type small --wait`. Each held exactly its generation (3 and 6) and not A's later writes.
   3. **Saved snapshots.** `s1` at gen 2, `s2` at gen 5, `s3` at gen 8. Deployed each as D1/D2/D3 and verified them.
   4. **Sonnet-13 repro.**
      - Wrote 4 more generations on D3 (merges, which used to drop the layer `s3` stood on).
      - Deployed `s3` again on the same node (D3b) and verified gen 8.
      - Stopped D3b. Resumed it. Verified. Wrote 2 more generations.
      - Saved `s3b` from D3b, deployed `s3b` (D3c, D3d), and stopped and resumed those.
      - The same flow also ran after `rm`, retention and a node restart (see below).
   5. **Deletion.**
      - `snapshot rm s2`.
        - `new --from s2` → `404 snapshot_not_found`.
        - D2 (a deploy of `s2`) still resumed with its data.
      - Deleted D3b, the source of `s3b`. `s3b` still deployed correctly.
      - `snapshot delete` of A's newest, a middle and an old ordinary snapshot.
        - Count went 28 → 25.
        - A still resumed with all data.
      - `snapshot delete --all` on F3 → F3 `resume` gives `409 snapshot_data_expired`, as Boat's page says.
      - `deletion status` for each op → `completed`.
   6. **Retention.**
      - `PUT /admin/retention {"olderHours":1}`. 0 is refused (`invalid_hours`). I then back-dated every snapshot row by 2 h in my own cluster's DB.
      - The 19:45 sweep logically deleted 158 of 205 rows (205 → 17 live, 158 purged). Node `disks/` went from 87 files to 52, and the server's `blobs/` from 6192 MB to 5725 MB.
      - After it, every sandbox resumed with its full data (A, F6, D1, D2, D3, D3c, D3d, E).
      - `s1`, `s3` and `s3b` deployed again with the exact data (gen 2, 8 and 10). Each deploy was stopped and resumed.
   7. **After the sweep.** `qemu-img info` on every node layer showed no backing file that is missing from disk. I did that scan three times: before the retention sweep, after it, and after the deploys.
   8. **Delete everything.** All sandboxes deleted (node `disks/` 81 → 16 files; 6419 → 4852 MB). Deploy of `s3` and `s1` still correct with their source sandbox gone. Then all saved snapshots removed. The node dir ended at 4293 MB (the base image alone), 1 file in `disks/`, the server's DB at 243 rows with 243 purged, 0 live.
   9. **Disk over time**, node dir MB (`du -sm`), image is 4309 MB:

      | Moment | MB | `disks/` files |
      |---|---|---|
      | after create | 4338 | 2 |
      | 8 stop/resume cycles with writes | 4394 → 5192 | — |
      | 9 cycles + 4 deploys + merges | 6148 | 86 |
      | before delete | 6419 | 81 |
      | after sandboxes deleted | 4852 | 16 |
      | after saved snapshots deleted | 4293 | 1 |

      The growth between cycles is about 100 MB per stop/resume. Those layers are all kept, as restore points, until retention (24 h) or a delete takes them. The jump from 6419 MB back to 4852 MB on sandbox deletion shows the layers are reference-counted and given back. The disk is bounded by the retention window plus the live chain (see "Not blocking" n3).
   10. **One 8-minute run with no stop.** A running sandbox overwrote a 30 MB file every 20 s. Node MB went 4338 → 4595, with 2–8 `disks/` files. That is the tiered merge's O(log n) depth, not unbounded growth.
5. **Other fix checks** (live): `org delete` with a member, paused big node (see the fix table). A node restart (kill, then run) kept saved-snapshot deploys and resumes working (`sz`, `new --from sz`, stop and resume).
6. **Teardown.** I deleted every sandbox and saved snapshot, stopped both nodes and the server by saved PID, and confirmed that no process mentions my data dir and nothing listens on 9970–9979. I deleted the data dir. One teardown mistake: a `pgrep -f` pattern in my own cleanup command matched my own shell and killed it. It touched nothing else, and I did not touch the other reviewer's processes (they were on other ports and other dirs). I edited no repo file except this report.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| sonnet-13 B1 | A saved-snapshot deploy lost its backing file after a layer merge | **Fixed (live)** | The node no longer deletes a superseded layer on upload (`internal/node/transfer.go:193-196`: "Superseded layers stay: the server deletes the ones nothing needs … and keeps saved snapshots"). The server only soft-deletes `periodic` layers it knows were absorbed. `createLazy` uses a layer from disk when present (`qemu/lazy.go:84-92` (`useLocal`, `:88`)). Live: the exact flow of `sonnet-13` (save → deploy → stop/resume the source until a merge → deploy again → stop/resume → save from the deploy → deploy that) gave correct data every time (D3b, D3c, D3d). Node snapshot records with the `deleted` flag set: 0 of 82. Missing backing files in the node's `disks/`: 0, three scans. |
| opus-15 B1 | A paused big node made a size look permanently unavailable | **Fixed (live)** | `n1` 8 vCPU; `n2big` 32 vCPU and paused. `POST /sandboxes {"type":"xlarge"}` → `202 Accepted`, queued (before: `409 size_unavailable`). `crowdvm new --type xlarge --fail-fast` → `503 no_ready_machine`. Deleting the queued sandbox worked. Residue: `crowdvm limits` still says "no node in the pool is big enough for xlarge" (n2 below). |
| opus-15 B2 | `org delete` ran with other members | **Fixed (live)** | Org `acme` with owner `admin` and member `bob`: `crowdvm --org acme org delete --yes` → `409 org_has_members` ("Remove the other members first … the owner deletes the organization as its last member", `orgs.go:331-333`). After the API `DELETE /orgs/{org}/members/{userId}` for bob, the delete went through (`{"deleted":true}`) and `org list` showed only Personal. I did not check what happens to the org's environments (n4). |
| team | "All fixed" (sonnet-12 B1/B2, opus-14 B1, first-create race) | **No regression found** | `limits`, `new`, `fork`, `resume`, `stop`, `snapshot`, `snapshots`, `deletion` behave as before. The non-e2e suite is green. |
| e2e | run16 | **EXIT=0** | 37 PASS, SKIP only for `agent/opencode` and `bootloop`. It has no test that deploys a saved snapshot after merges, but the unit tests in `internal/node/vm/qemu` and my live session cover the case. |

## New missing or broken supportable features

None blocking. Everything I tried from the Boat pages for stop, resume, fork, saved snapshots, snapshot delete and data retention behaved as documented.

## Not blocking (my own findings, each with a one-line fix)

- **n1. A queued `new --from NAME` that is stopped before it ever boots resumes as an empty disk.**
  - Evidence (live, twice; the first time was an accident: I started a deploy on a full node, the CLI waited, I stopped it):
    - Fill the node (4 running). `crowdvm new --from s1 --count 1 --type small` → the sandbox is created and `provisioning`; the CLI waits.
    - `crowdvm stop <id>` → `stopped`. Free a slot (stop another sandbox). `crowdvm resume <id>` → `ready`.
    - `exec` shows an empty home directory (`g*.txt` and `blob*.bin` all missing). The row says `boot_kind=create`, `source_kind=saved`.
  - Cause: the guard "Never boot a blank disk for a sandbox that already ran" (`lifecycle.go:1175`) checks `FirstBootDone`, which is false for a sandbox that never started. `resume` then queues a plain `create` boot and forgets the saved snapshot or fork source (`lifecycle.go:1270`).
  - Why not blocking: no data is destroyed (the saved snapshot is intact; deploy again); it needs the pool to be full and the owner to `stop` a sandbox that has not started. Boat's pages do not say what a stop of a waiting sandbox does. Fix: make `resume` of a never-booted sandbox use its original boot kind and source, or answer `409` on `stop` while the sandbox is `provisioning`.
- **n2. `crowdvm limits` shows "no node in the pool is big enough for xlarge" when the only node that fits is paused** (`scheduler.go:285-297`; the "paused" text exists but is only used when no node is online). The sandbox request queues correctly; only the hint is misleading. Cosmetic.
- **n3. Node disk grows by about 100 MB per stop/resume cycle until retention** (default 24 h; checked live by back-dating, `feat_retention.go:520-570`). A stop writes an 11 MB layer and a live layer of about 2 MB, even when the user changed one byte, and every stop-point is kept. That matches Boat's "older snapshots stay for less than a day". It is bounded by the window times the stop rate, not by a node quota. A node owner who sets `--disk 80G` is protected by the scheduler's accounting of reserved sandboxes (`diskGiB` in `free`), but the layer store itself has no hard cap. I do not count this as missing: Boat has no per-layer cap either, and the job works. If the retention window were set to e.g. 1 h on a node with limited disk, the sweep would reclaim it (I tested it).
- **n4. Org delete leaves the org's environments and profiles** (carried over from `opus-15`, B2's second half). I did not check it again after the fix.
- **n5. `new --from NAME --count 1` waits for capacity** while `--count 2` is documented as returning at once. The CLI waits only for the single sandbox it tracks. Cosmetic.
- **n6. `crowdvm info`/`ls --json` do not show `waitingForCapacity` or `statusMessage`** (same as `sonnet-13` m2). The field exists on the API; the CLI's `client.Sandbox` drops it. A waiting sandbox shows `provisioning`. This is how I found n1 only by accident.

## Checked and fine

| Area | Result |
|---|---|
| 10 stop/resume cycles with writes between | Pass. All files, the journal and all blob hashes were exact each time. Resume after stop took about 5 s. |
| Forks off different generations | Pass. F3 and F6 each held exactly their own generation. A fork that was stopped and resumed (F6) kept it. |
| Saved snapshots at gen 2/5/8 and re-saves from deploys | Pass. Each deploy had exactly its generation, a re-save from a deploy carried the deploy's own history, and deploys of a deploy worked. |
| Deploys after merges, after node restart, after retention, after the source was deleted | Pass. |
| `snapshot list` / `snapshots` after rm, delete and retention | Pass. `snapshot rm` shows `deleted` and the name vanishes from the list. `snapshots` shows the sandbox's ordinary snapshots and the saved ones with the plan allowance. After retention the list kept only the live chain. |
| `snapshot delete` of newest/middle/old, and `--all` | Pass. The count dropped by the number deleted. `deletion status` `completed`. After `--all`, resume gives `409 snapshot_data_expired`. |
| `new --from <deleted name>` | `404 snapshot_not_found`, clear message. |
| Disk after delete-all | Back to the base image only (4293 MB, 1 file). Nothing leaked on the node or the server. |
| Periodic snapshots on a running sandbox | Tiered: 2 → 8 layers over 8 minutes at 20 s, then merged. No unbounded chain. |

## Disputed or accepted classifications

- **A paused node only keeps new sandboxes away, it does not count as "never fits".** Accepted as fixed. A queued request on a paused big node waits, `--fail-fast` fails at once.
- **Retention at the default 24 h** does not clean quickly, so I used `olderHours:1` plus a back-dated DB. That is a faithful test of the job's logic, not of its clock.

## Not verified

- A resume on a second node (lazy chain across nodes). I had one real node.
- Server restart during a stop, and a node crash with a half-written layer.
- 24 h of real retention time; a cluster with a disk-full node.
- CI against real GitHub or Gitea; Windows and macOS nodes; the dashboard in a browser; live model turns.

## What would flip this to DISAGREE

A repeat of any earlier blocker, or the `n1` blank-disk resume reaching a flow that does not need a full pool. The cheap fix for n1 is above.
