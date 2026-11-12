# Bug hunt r18 (opus): round-17 rechecks, `--new-key` copies, node self-update and compatibility gating, 2026-11-12

Setup: commit `557c7cf`, built with `make build VERSION=0.9.0-r18.1` in my own worktree (`/home/tl/dev/CrowdVM-r18opus`, detached at `557c7cf`). Binaries and all state lived in `~/.local/share/crowdvm-r18opus`. One `crowdvm-server` listened on :9960 with `--public-url http://127.0.0.1:9963`, `--public-domain r18.test`, `--log-level debug`, no start limits, `--default-image ubuntu-24.04-crowdvm-v4`, `--egress-allow 192.168.178.31:9967,192.168.178.31:9968`, `--node-update-grace 6m`, and `CROWDVM_AI_UPSTREAM_ANTHROPIC` pointing at a fake vendor. Two qemu nodes, `r18n1` and `r18n2` (UI on :9961 and :9962), each 8 vCPU, 20 GiB, 30 GiB disk (60 GiB from early on) ×4 overcommit, at most 5 sandboxes, each with its own binary directory (so one node's update does not touch the other's files). Both ran as transient systemd user units with the real unit's properties (`KillMode=process`, `NotifyAccess=main`, `FileDescriptorStoreMax=1024`, `FileDescriptorStorePreserve=yes`, `CROWDVM_FDSTORE=1`, `Restart=always`, `RestartSec=5`). `r18n1` ran as `crowdvm-node.service` (no such unit existed for this user), so it counts as supervised and can update itself; `r18n2` ran as `r18opus-n2.service` and is not supervised. Image `ubuntu-24.04-crowdvm-v4` from `~/.local/share/crowdvm-dev/images/`, imported on `r18n1`, pushed and pulled to `r18n2`. Node↔server and guest↔server traffic went through a Python token-bucket proxy on :9963: 40 MB/s, lowered to 2–5 MB/s while the host disk was at risk (see bug 2) and raised briefly for one 17 GB upload. Users: `admin` (owns the nodes), `bob`, `carol`, each with their own `CROWDVM_CONFIG_DIR` and scratch `HOME`. Helpers on my ports: the fake vendor plus a file, slow-upload, long-poll and drip server (:9967), a TCP sink (:9968).

For the update tests I built local releases only, from a copy of the source outside the repo (`~/.local/share/crowdvm-r18opus/src2`, `src3`): `0.9.0-r18.2` with `NodeGen = APIGen = RequiredNodeGen = RequiredAPIGen = 2`, and `0.9.0-r18.4`, the same plus a `run` command that fails right after loading its config (a stand-in for a bad build). Each was published with `crowdvm-server downloads import` from a hand-made `crowdvm_<v>_linux_amd64/` directory and `manifest.json` in the `scripts/dist.sh` layout. Nothing came from GitHub.

Cleanup: see the last section.

## Bugs

### 1. A `--new-key` copy of a copy whose re-encryption was cut off gets that copy's key, not one of its own (security)

Severity: security. `--new-key` promises the copy "a key of its own". When the source of the copy is itself a `--new-key` copy that was stopped during its first-boot re-encryption, its snapshot carries `rekey_from` (the original source's key). `ensureDiskKey` only makes a new key when the parent snapshot has no pending old key (`old == ""`), so the new copy takes the parent's sealed `data_key` and its `rekey_from` unchanged. Its first boot then resumes the parent's unfinished re-encryption to the parent's key and confirms it. The result is a disk that opens with the parent's key. So the fork, or a deploy of a snapshot saved from that stopped copy, shares a key with it, and during its first boot it also receives the original source's key. The user asked for `--new-key` and gets no separation, with no warning.

Where: `internal/server/guest_channel.go:114` (`case sealed != "" && newKey && old == "" && …`): with `old != ""` the copy keeps `sealed` (the parent's key) and `old`.

Repro:
1. bob: `crowdvm new --type small --name s1`, write a marker and a 500 MB blob, `crowdvm snapshot s1 ssave`, `crowdvm snapshot share ssave`.
2. carol: `crowdvm new --from <code> --new-key --name c3 --node r18n1`; as soon as the console shows `re-encrypting: 34%`, `crowdvm stop c3`.
3. carol: `crowdvm fork bx_kagyz9f4 --new-key --name f3 --node r18n1`.
4. `sqlite3 crowdvm.db "select name,substr(data_key,1,14),substr(rekey_from,1,14) from sandboxes …"`, and f3's console.

Observed:
- After step 2: `c3|archived|v1:1bBluUhhlx6|v1:aQFYAb638ty` (its own key, and s1's key still pending). Its stop snapshot `snap_2nrej…` has the same two values.
- After step 3: `f3|cloning|v1:1bBluUhhlx6|v1:aQFYAb638ty`: the identical sealed key of c3, and s1's key.
- f3's console: `resuming the interrupted re-encryption of the data partition (a copy's own disk key)`, `re-encrypting: 51% …`, `the server dropped the old disk key`. The server logged `encrypted disk re-keyed to its own key` for f3, and f3 ended `ready` with `data_key v1:1bBluUhhlx6`, still c3's key.

Expected: a copy made with `newDiskKey` always ends with a key no other sandbox holds. When the parent snapshot still has a pending re-encryption, the copy first finishes it to the parent's key and then re-encrypts again to a fresh one. The server could also refuse `newDiskKey` for such a parent until the parent has booted once. The share-and-deploy path (`new --from <snapshot of c3> --new-key`) goes through the same `ensureDiskKey` branch. I did not run it, because its re-encryption would have written another 42 GB on a nearly full host.

### 2. A re-keyed copy's first snapshot is uploaded and stored at its full apparent size (42 GB for `small`), so the server fills up and deleting the copy waits for it (leak)

Severity: leak. The re-encryption writes every block of the data partition, and the closing `fstrim` punches the space back out of the node's qcow2 overlay. The overlay is then sparse: 39 942 MiB apparent, 2 509 MiB allocated. The node uploads a snapshot by reading the file with `io.TeeReader` and `Content-Length` = its size, so it sends every hole as zeros. The server writes them out as real data. The first snapshot of every `--new-key` copy is therefore a 41.9 GB upload, against 2.6 GB of real data. At 40 MB/s that is 17 minutes per copy, and `crowdvm delete` of the copy waits for the upload to end (`waiting_for_uploads`). On this host free space fell from 93 GB to 31 GB within 25 minutes, and I had to throttle the link and restart the nodes to stop it. A server that gets several such copies runs out of disk. The doc's "its first snapshot holds all of its data" means 2.6 GB here, not 42.

Where: `internal/node/transfer.go:420` (`fileSHA256(path)` gives the apparent size) and `:469` (the raw file as the request body), together with `internal/guest/preinit_linux.go` (`rekeyData` + `fstrim` leave a sparse overlay).

Repro: carol: `crowdvm new --from <ssave code> --new-key --name c1 --node r18n1`, and `crowdvm fork c1 --new-key --name f1 --node r18n2`. After both are ready: `ls -ls` on the overlays, the nodes' `transfer started` lines, the server's `blobs/snapshot/.upload-*`, then `crowdvm delete f1 --yes`.

Observed:
- Overlays: `bx_pwx6y3tt-53fcb8bd.qcow2` (f1) and `bx_7pk4ze7a-3f1f45c3.qcow2` (c2): `2509M` allocated, `39942M` apparent.
- Node logs: `transfer started dir=upload kind=snapshot id=snap_zdkrn5dzej844w2wku8e bytes=41881763840` (f1), likewise `snap_dm7wfp…` and `snap_mwtjcp…` (41 881 763 840 bytes each).
- Server: `.upload-3768673375` at 11 694 276 608 bytes with 11.4 GB allocated after about 7 minutes, still growing.
- `crowdvm delete f1 --yes` sat for over 4 minutes. Its operation was `processing|waiting_for_uploads|… 3 snapshot(s) of it are still uploading from their node`.
- `c1`'s first upload was cut by a node restart. After the node merged the layers, the snapshot it uploaded was 2 630 483 968 bytes, which is the real size.

Expected: the node uploads only allocated data (compact the layer with `qemu-img convert` before the upload, or send a sparse-aware format), and the server stores it sparse. At the least, a re-keyed copy's first snapshot costs about its real data, not the disk size.

### 3. A `--new-key` copy whose re-encryption takes more than 3 minutes fails with "boot timeout", and resuming it starts the re-encryption over (stuck)

Severity: stuck. Preinit re-encrypts the data partition before the guest agent starts. The node gives every boot 3 minutes to report a healthy agent, the re-encryption included, then kills the VM and reports `error`. The doc says the first start "took 2 to 3 minutes on an NVMe node and takes longer on slower disks. A start that is cut off resumes where it stopped on the next start." Neither half holds at the limit:
- A `small` copy on a node that first has to fetch the chain lazily (40 MB/s) was at 76%, 55 s from the end, when it was killed.
- `crowdvm resume` of the failed copy booted from the head snapshot again, not from the killed boot's disk, and the re-encryption began at 0%.

A copy whose re-encryption needs more than 3 minutes on its node (a `large` copy's 110 GB at the ~650 MB/s seen here, or any copy on a slower disk) fails on every start and never becomes usable.

Where: `internal/node/vm/qemu/qemu.go:209` (`BootTimeout = 3 * time.Minute`), `:1562` (`waitBoot` kills the VM on the timeout, re-encryption or not).

Repro: carol: `crowdvm fork c1 --new-key --name f1 --node r18n2` while `c1` (on `r18n1`) was itself re-encrypting, so f1 booted from `ssave`'s chain fetched over the 40 MB/s link. Then `crowdvm resume f1`.

Observed: `lazy boot` at 16:50:25, `chain hydrated` at 16:51:48, then at 16:53:25 `boot timeout: guest agent not healthy after 3m0s`, with the console's last line `re-encrypting: 76% of 41.9 GB, 184 MB/s, 55s left`. f1: `error`. `crowdvm resume f1`: the console starts with `re-encrypting the data partition to a disk key of its own (a copy's first start …)` and `re-encrypting: 15% …`. It finished in 2m9s this time only because the chain was now local.

Expected: the boot timeout does not count a re-encryption that is making progress (preinit could report progress over the channel or console, or the node could allow time per GB for a boot that re-keys). A boot cut off during the re-encryption keeps its disk, so the next start really resumes.

### 4. Removing a shared snapshot while a deploy of it waits for capacity purges the layer it needs: the copy fails with a raw `qemu-img … 404` and cannot recover (data-loss)

Severity: data-loss for the recipient (the copy can never get its data). A deploy that waits for capacity has no `head_snapshot_id`, and its `bootParent` only counts once it has a node. So `snapshotsNeeded` does not count the saved snapshot it was made from. When the owner then removes the saved snapshot and the source sandbox, GC purges the layer at once. As soon as the deploy is placed, its node gets 404 for the layer. Unlike the delete of a saved snapshot that a fork still needs, nothing protects or warns about it.

Where: `internal/server/stranded.go:44–54` (`snapshotsNeeded`: `head_snapshot_id IS NOT NULL`, and `bootParent` only `AND node_id IS NOT NULL`).

Repro:
1. bob: `crowdvm new --type small --name s2 --node r18n1`, `crowdvm snapshot s2 s2save`, `crowdvm snapshot share s2save`. `r18n1` is full by disk reservation.
2. carol: `crowdvm new --from <code> --new-key --name d2 --node r18n1`, which waits for capacity.
3. bob: `crowdvm delete s2 --yes`, `crowdvm snapshot rm s2save`.

Observed: right after step 3, every snapshot of s2 had `deleted_at` and `purged_at` set, `snap_qga3dmk32f9t4cyjtf2n` (the saved one) included. Deleting s2 freed `r18n1`, d2 was placed within a second, and at once failed: `d2|error|qemu-img info http://127.0.0.1:38265/b/snapshot/snap_qga3dmk32f9t4cyjtf2n/…: exit status 1: … CURL: Error opening file: The requested URL returned error: 404`. carol's `crowdvm new` ended with `sandbox_error` and the same text. The layer is gone, so no resume can fix it.

Expected: a pending deploy (and a queued fork) keeps its parent chain alive, like a head does: count `bootParent`, or the saved snapshot's layer, for sandboxes still waiting for a node. Or `snapshot rm` refuses or warns while a deploy of it is pending, and the deploy then fails with a clear `snapshot_deleted` message instead of a qemu-img URL.

### 5. A node build that fails before its start is counted is never rolled back: the node crash-loops for good (stuck)

Severity: stuck. The doc says "A new build that never reaches the server is rolled back on its fourth start". The count happens in `CheckUpdateStart`, which a supervised daemon runs only after it has loaded and saved its config, taken the data-dir lock, set `NoDumps` and refreshed the unit. A new build that fails anywhere before that (a stricter config parser, a lock or migration change, a panic in an early step) never increments `starts`, so it is never rolled back. systemd restarts it every 5 seconds without end, and the node stays offline. Its VMs keep running unmanaged, and the server rebuilds them elsewhere as for a dead node. Rolling back then takes an operator on the machine.

Where: `cmd/crowdvm-node/main.go:209–250`: `g.config()` (`:209`), `f.apply`, `LockDataDir` (`:218`), `cfg.Save`, `service.Refresh` (`:242`), all before `node.CheckUpdateStart` (`:250`).

Repro: publish `0.9.0-r18.4`, whose `run` fails right after `g.config()` (`version` works, so the update's own check passes). On `r18n1` (as `crowdvm-node.service`): `crowdvm-node update --data-dir …/node1`.

Observed: `updated … to 0.9.0-r18.4`, `Restarting the node`. Then `error: simulated bad build: cannot read this config` on every start, and `Scheduled restart job, restart counter is at 9` after 40 s, still going. `update-pending.json` stayed `{"from":"0.9.0-r18.3","to":"0.9.0-r18.4","starts":0}`. `crowdvm-node update --rollback` by hand brought back r18.3, which reached the server at once and listed r18.4 in `update-skip.json`.

Expected: count the start (and roll back after the limit) as the very first thing a supervised `run` does, before anything the new build could fail at. For example, read `update-pending.json` straight from the data-dir path in the arguments, or roll back on the unit's own restart count.

### 6. After a manual `crowdvm-node update` on a node that is not a service, the node stays outdated while `update` says "up to date" (cosmetic)

Severity: cosmetic. A node that does not run as the systemd service cannot restart itself. `crowdvm-node update` swaps the files and prints `Restart the node to use it`. The running daemon still has the old generation, though, so the server, the node's own status page and the dashboard keep saying "misses a required update … Run `crowdvm-node update`". Running that command again answers `crowdvm-node is up to date (0.9.0-r18.2)`, exit 0, and `update --check` agrees. An operator who missed the one restart hint gets contradicting answers until the grace period moves their sandboxes away.

Where: `internal/node/update.go:119` (compares the files on disk, not the running build), `internal/server/compat.go:82` (`nodeUpdate`'s message), `cmd/crowdvm-node/update.go:70`.

Repro: `r18n2` (as `r18opus-n2.service`, not supervised), server on gen 2 with `0.9.0-r18.2` in its downloads. `crowdvm-node update --data-dir …/node2`, then `curl :9962/api/status`, then `crowdvm-node update` and `update --check` again.

Observed: `updated … to 0.9.0-r18.2`, `Restart the node to use it (its sandboxes keep running).` Status: `"version":"0.9.0-r18.1"`, `state outdated`, `deadline … 16:34:25Z`, `message … Run crowdvm-node update`. The second `update` and `update --check` both printed `crowdvm-node is up to date (0.9.0-r18.2)`, rc 0. After `systemctl --user restart` the node became `current` and the server logged `outdated evacuation ended`.

Expected: when the binary on disk is newer than the running daemon, `update`, `update --check` and the status page say "installed, restart the node to use it" (the local page knows the daemon's version), and the server's message says the same once the files are updated.

### 7. A sandbox pinned to an outdated node is told "no node has room" and waits 15 minutes, though the node only needs an update (cosmetic)

Severity: cosmetic. When an outdated node's grace period ends, a sandbox pinned to that node (`--node`) is stopped and left stranded, which matches the code comment. The notice and `resume` then give the wrong reason. The notice says "no other node can host it right now … Resume it when a node is available". A plain `crowdvm resume` waits "for a node with room for a small sandbox (vCPU, memory or disk)" with a 15-minute deadline, although the pinned node has room and is refused only for being outdated. Nothing tells the owner to update the node or to resume with `--node <other>`, which works at once.

Where: `internal/server/nodes_evacuate.go:393` (`evacuationStranded`'s text), `internal/server/lifecycle_place.go:230` (the capacity message for any placement failure).

Repro: admin: `crowdvm new --type small --name y1 --node r18n2`. Make the server gen 2; `r18n1` updates itself, `r18n2` cannot. After `--node-update-grace` (6 min): `crowdvm resume y1`, then `crowdvm info y1`.

Observed: `stopping sandbox reason=evacuated`, notice `evacuation_stranded` ("… no other node can host it right now, so it stays stopped. Resume it when a node is available."). `resume`: `waiting for a node with free capacity`; `info`: `Waiting for a node with room for a small sandbox (vCPU, memory or disk)…`, `waitDeadline` 15 minutes on. `crowdvm stop y1; crowdvm resume y1 --node r18n1` → ready.

Expected: when the only node a sandbox may use is outdated, the notice and the wait message say so ("its node r18n2 needs `crowdvm-node update`; resume with `--node` to move it"), and `resume` fails fast with that reason instead of waiting for capacity.

## Round-17 fixes rechecked live

| r17 item | Result |
|---|---|
| Stalled egress flows (opus 1) | Holds, and slow destinations survive. In `s1`, at once: a 6 MB POST to a server reading at 30 KB/s (`slow got 6000000 in 200s`, rc 0), a GET that waits 200 s before answering (`polled 200`, rc 0), and a response that sends 1 byte every 10 s for 180 s (all dots, `end`, rc 0). Not tried: a destination slower than about 270 B/s, where one 32 KB write could exceed the 2-minute deadline even though it moves. |
| Removed member's SSH keys (opus 2) | Holds. acme with carol (member, creator) and admin (member). admin `crowdvm ssh` into shared `k1` and `k2` (one line in `authorized_keys` each). Removing admin while both ran: both 0 lines. Rejoined, ssh'd into `k2`, stopped `k2`, removed admin, resumed `k2`: 0 lines. |
| Service account via the members endpoint (opus 3), `credits transfer` scope (opus 4), invite race (opus 6) | Not rechecked. |
| AI proxy vendor list (opus 7) | Holds. carol `ai proxy on` with an empty profile, `c1` resumed: no `*_BASE_URL` in the login env. `ai profile set-key default anthropic-api-key …` while `c1` ran: 5 s later `ANTHROPIC_BASE_URL=http://10.0.2.2:9963/ai-proxy/anthropic` and a `cvmai_` placeholder, a call through it reached the fake vendor with the real key, and still no OpenCode/OpenAI/OpenRouter base URL. |
| Public desktop stays public (opus 8) | Holds. `crowdvm desktop s1 --public` → `publicAccess: true` (and a stderr note). `crowdvm desktop s1` → `publicAccess: false`, a URL with `#token=…`. Not re-tested in a browser. |
| Smaller type, bigger disk (opus 9) | Holds. `r1` small → large → small: `storage.diskGB 111` (large's size), `/dev/vda` 132 070 244 352 bytes, `/home` 98G, and `r18n1 reserved.diskGiB 111`. |
| `exec \| head` (opus 10) | Holds: `line1`, nothing on stderr, `PIPESTATUS[0]` 141. |
| Concurrent `known_hosts` (sonnet 2) | Not rechecked. |

## New ground: what held

- **`--new-key`, the normal path:** a deploy from a share re-encrypts in preinit (`re-encrypted in 2m…`, `the server dropped the old disk key`), the server clears `rekey_from`, the marker and the blob's sha256 are intact, and `luksDump` shows one keyslot and 4096-byte sectors. A fork made while its source is still in its first boot (`provisioning`) uses the source's parent snapshot and gets a fresh key of its own (`f1`).
- **Stop during the re-encryption, then resume:** the copy's stop snapshot keeps both keys, and the next boot (here f3, from that snapshot) logs `resuming the interrupted re-encryption`, runs `repair` + `--resume-only`, finishes and confirms. The key it ends with is bug 1.
- **Node restart during the re-encryption:** `systemctl --user restart crowdvm-node` at 20%: the VM was adopted and kept going. The sandbox showed `error: node_offline` for about 2 minutes (and `crowdvm new` ended `state error`), then went `ready` on its own when the guest came up (`re-encrypted in 3m2s`). The adoption also reset the 3-minute boot clock, which saved it from bug 3.
- **Snapshot pull/tree of a re-keyed copy:** 409 `snapshot_encrypted` ("only the sandbox's own machine gets its key"), as designed.
- **A key extracted from a re-keyed copy:** after a completed re-encryption the copy's header holds one keyslot, for its own key, and `cryptsetup reencrypt` gave it a new volume key, so neither the copy's passphrase nor its volume key opens the source. The old key reaches preinit over pipes only (code read); I did not dump the guest's memory. Bug 1 is the exception.
- **`crowdvm recover` with a preinit power-off (the known issue):** I wiped `x1`'s keyslots from inside (`cryptsetup erase`), froze the agent until the server marked it `degraded`, then ran `crowdvm recover x1`. Preinit: `No usable keyslot is available.; powering off`; the node reported `guest powered off` 4 s after the relaunch, and the server set `state error`, `error guest powered off`. It did not stay `starting`. Only leftovers: `health degraded`, `healthReason recovering`. So the stuck `starting` needs something other than a preinit failure on a running node. My guess, not checked: a recover whose new boot ticket arrives while the node restarts.
- **Compatibility, server first:** importing r18.2's downloads into the r18.1 server made both nodes `available` (status page, `update --check` rc 1). Replacing the server with the gen-2 build made both `outdated` (`It still takes sandboxes while no updated node is online`), and the grace deadline slid while no node was current.
- **Auto-update:** supervised `r18n1` (http on loopback counts as trusted) logged `auto-update … in=2m51s`, downloaded node and guest, checked the sha256, swapped them (`.old` kept), restarted through `ErrRestart`, adopted its 3 running VMs, became `current`, and cleared `update-pending.json` after its first heartbeat. `r18n2` reported `autoUpdate.unavailable: not running as a service`.
- **Grace and evacuation:** once `r18n1` was current, `r18n2`'s deadline was set 6 minutes on, and at the deadline its sandbox was stopped with its disk saved (`reason=evacuated`). A pinned one stays stranded (bug 7). After `r18n2` was updated and restarted, `outdated evacuation ended`. The owner's `node_outdated` notice arrived 10 minutes after the node was first seen outdated, with the update command. With a grace shorter than 10 minutes it arrives after the move (my setting, not a default).
- **Rollback:** `crowdvm-node update --rollback` swapped node and guest back, listed the version it left in `update-skip.json`, and the service started the previous build. `update --force` exists for a skipped version. A downloaded node binary that does not run is refused before the swap by its `version` check (code read).
- **Old CLI:** with `X-Crowdvm-Gen: 1`, the server answers `X-Crowdvm-Client-Status: outdated; latest=0.9.0-r18.2`. On a terminal the r18.1 CLI prints `This crowdvm 0.9.0-r18.1 misses an update the server requires (latest: 0.9.0-r18.2). Run crowdvm self-update.` (plus the 6-hourly "newer crowdvm is available" line), and nothing with `--json`. `crowdvm self-update` on a scratch copy replaced it with r18.2.

## Not verified

- **A node with too little free disk for a re-encryption.** The node admits a VM with only 2 GiB of headroom (`internal/node/disk.go`, `minDiskHeadroom`) and does not check the ~42 GB a `small` copy writes before its `fstrim`. This host's free space fell by about 33 GB during two concurrent re-encryptions. I did not fill the disk on purpose: other clusters share this filesystem, and I had no way to make a small filesystem without root. The doc says that space is needed; whether QEMU's pause on ENOSPC leaves a resumable re-encryption is untested.
- **Evacuation during a re-encryption:** not run (another 42 GB write and, through bug 2, a 42 GB upload).
- **The share-and-deploy variant of bug 1:** follows the same `ensureDiskKey` branch as the fork. Stopped before its boot to protect the host disk.
- **An incompatible node (below `MinNodeGen`):** not built. A build that reaches the server but gets 426 never runs `confirmUpdate` and never restarts, so it would not be rolled back either. Code read only.
- **Slow uplinks:** at a shared 5 MB/s for both nodes, `r18n2` lost its server connection every 45 s to 2 min while uploading a snapshot (`heartbeat failed: EOF`, `could not ask a node to upload a snapshot … context deadline exceeded`), and its sandbox showed `error: node_offline` for a while. My proxy has one token bucket for all connections and adds its own buffering, so I cannot tell how much of this is the product. It is worth a test with a per-node `tc` limit.
- Round-17 opus 3, 4, 6 and sonnet 2 were not rechecked.

## Cleanup

Deleted every sandbox (`s1`, `s2`, `c1`–`c3`, `f1`, `f3`, `d2`, `k1`, `k2`, `x1`, `y1`, `r1`, `fa` and the extra `c3`) and the saved snapshots `ssave`, `s2save` and `c3save`; all 15 deletion operations ended `completed` and both nodes' `vms/` directories were empty. Stopped both node units with `systemctl --user kill --kill-whom=all` + `stop` + `reset-failed` (so their VMs went too), then the server, the throttle proxy, the fake vendor and the TCP sink. Nothing of mine listens on 9960–9969, no process matches `crowdvm-r18opus`, and no `crowdvm-node` or `r18opus-*` user unit is loaded. `~/.local/share/crowdvm-r18opus` (binaries, the patched source copies `src2`/`src3`, `dist2`/`dist3`, node and server dirs, logs, scratch homes) is deleted. The worktree `/home/tl/dev/CrowdVM-r18opus` is removed after this report was copied to `docs/reviews/bugs-r18-opus.md` in the shared checkout (untracked). I touched nothing else there, and did not touch `crowdvm-field`, the e2e clusters, other worktrees, the user's systemd units or `~/.npmrc`.

## Fixes

5 of 7 fixed; bug 1 by the same change as bugs-r18-sonnet 1. Tests: `internal/server/bughunt_r18_test.go` (1), `internal/server/fix_r18_test.go` (4), `internal/node/vm/qemu/fix_r18_test.go` (2, 3), `cmd/crowdvm-node/fix_r18_test.go` (5).

1. **Copy of a copy cut off mid re-encryption**: no copy of a sandbox or snapshot that still waits for its own key (409 `disk_rekey_pending`, retryable).
2. **Sparse first snapshot uploaded at full size** (`transfer.go` `UploadSnapshot`, qemu `store.CompactSnapshot`): a layer whose file is less than half allocated is copied with `qemu-img convert -O qcow2 -B <its backing>` into the node's `tmp/` and that copy is hashed and uploaded. Same backing reference and guest view, only the allocated clusters; the copy is deterministic, so a retried upload has the same sha256. If the copy fails (e.g. a lazy chain not yet local), the layer goes up as before. Live: a `--new-key` `small` copy's first layer, 52 620 820 480 bytes apparent and 1.53 GB allocated, went up and is stored as 1 530 789 888 bytes.
3. **Boot timeout kills a re-encryption** (`qemu.go` `waitBoot`, `rekeyProgressing`): while the serial console shows preinit's re-encryption progress (written in the last minute), the boot gets another boot timeout, up to 2 hours in total.
4. **A queued deploy loses its parent** (`stranded.go` `snapshotsNeeded`): the parent of a boot still waiting for a node (`extra.pendingBoot.parent`) is kept, like a head.
5. **A bad build crash-loops** (`cmd/crowdvm-node/main.go`): a supervised `run` counts the start of an updated build (and rolls it back after the limit) first, with only the data dir, before reading its config or anything else it could fail at.

Not fixed:
6. **`update` says up to date before the restart** and 7. **pinned to an outdated node, told "no room"**: cosmetic messages, kept for later.
