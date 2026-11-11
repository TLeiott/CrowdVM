# Bug hunt r9 (opus): live cluster and chaos run, 2026-11-11

Setup: `make build` at 16:04, binaries copied to my own data dir (`~/.local/share/crowdvm-r9opus`). One `crowdvm-server` on 127.0.0.1:9960. Two qemu nodes, `r9n1` and `r9n2`, with their UIs on 9961 and 9962. Both had `ubuntu-24.04-crowdvm-v3` and `-minimal-v3`, imported with `crowdvm-node image import`. Users were `admin` and `bob`, each CLI with its own config dir. For the rechecks, node2 ran as a transient systemd user unit with the exact settings `install-service --dry-run` prints. Node↔server traffic went through a Python token-bucket TCP proxy on 9963 (40 MB/s per direction). The chaos run used `--default-image ubuntu-24.04-crowdvm-minimal-v3` and nodes with 8 vCPU, 16 GiB and 60 GiB each.

Cleanup: every sandbox and saved snapshot is deleted. Afterwards both nodes held only the two base images (`vm/disks`, no VM dirs, no snapshots). The server had 0 blobs, 0 live snapshot rows, 0 snapshot locations and 0 live sandboxes. Server, nodes, proxy and every qemu of mine are stopped. The transient unit is gone. My data dir and scratch files are deleted.

## Bugs

### 1. `resume` during a short node outage reboots from an older snapshot and destroys the newer disk (data-loss)

Severity: data-loss.

Where: `internal/server/lifecycle.go:1359` (`handleResume`). A sandbox in `error`/`node_offline` falls through the `switch` at `:1403-1429` into the normal resume path. `placeSandbox` then picks another node, and `startBoot` boots the head snapshot with `Cleanup: prevNode` (`:1508-1509`, `:1546`). Nothing holds the resume back. The stop path does (`:953-960`: "Not when only its node is offline: the machine may still run there with newer data, so the stop waits for the node up to NodeLostGrace"), and so does `rebuildLost` (`lifecycle_place.go:301`, which waits `NodeLostGrace` and requires the chain to be uploaded). Resume does neither. When the old node reconnects, the cleanup removes the original VM and its overlay. That copy was still running, and it held the newest data.

Found by the chaos run: `bx_v3ckfcmh` lost the synced files f526 and f531 at 16:49 after `kill -9` of r9n1.

Minimal repro, with an unpinned sandbox so it may move:
1. `id=$(crowdvm new --type small --no-auto-stop --json | jq -r 'select(.event=="ready").id')` (it landed on r9n1).
2. `crowdvm exec $id 'echo A > ~/a.txt; sync'`, `crowdvm stop --wait $id`, `crowdvm resume $id`, `sleep 10` (gives the sandbox a head snapshot that is on the server).
3. `crowdvm exec $id 'echo B-synced > ~/b.txt; sync'`.
4. `kill -9` the r9n1 node process. The qemu keeps running.
5. As soon as `crowdvm info` shows `error`/`node_offline` (7 ms later here): `crowdvm resume $id`.
6. Restart r9n1 a few seconds later.

Observed:
- The resume returns `ready` 4 s after the kill, on r9n2. `ls ~` shows only `a.txt`, and `cat ~/b.txt` gives "No such file or directory".
- r9n1 comes back at 16:58:36 and logs `adopted running VM bx_8tjk4pq9`. Seconds later its VM dir is gone, with no log line, and only the old snapshot layers remain.
- No notice says data was lost. The only notice is `kind=sandbox_error`. `rebuildLost` would have sent "changes made after that snapshot are lost".

The same resume on a pinned sandbox (`placement.node`), or when no other node fits, queues instead. Node1 then comes back, `adoptQueuedMachine` takes the running machine back, and the data is intact (checked twice). So the loss needs a free second node, which is the normal case on a pool.

Expected: while the node is offline and within `NodeLostGrace`, resume of a `node_offline` sandbox should answer 409 (retryable, "the node is reconnecting") or queue with `Prefer`/`Cleanup` the way `restartLostMachine` does, never boot elsewhere at once. After the grace it should behave like `rebuildLost`: require the chain to be uploaded and send the "changes are lost" notice. An agent that sees `state: error` will try `resume`, so this is easy to hit.

### 2. A foreign deploy still keeps the saver's command history: the round-8 fix is dropped on the way to the guest (security)

Severity: security. Round-8 opus #4 is not fixed.

Where: `internal/server/lifecycle_secure.go:56-78` (`addPendingScrub`). It merges `Files`, `Dirs`, `RegistryLogouts`, `SSHKeys`, `AuthorizedKeys` and `GitHubLogout` into the pending scrub, but not `History`. So `sc.History = true` from `foreignDeployScrub` (`:375`) never reaches `pendingScrub`, the guest config, or `runScrub` (`internal/guest/apply.go:173`). `scrubEmpty` (`:45`) also ignores `History`. That does not matter here, because `SSHKeys` is set too.

Repro:
1. admin: `crowdvm new --type small`, then `crowdvm exec ID 'echo aws_secret=TOPSECRET-r9; mkdir -p ~/.ssh; echo k > ~/.ssh/id_ed25519'`, then `crowdvm ssh ID 'echo ssh-secret=SSHSECRET-r9'`.
2. admin: `crowdvm snapshot ID secsnap`, then `crowdvm snapshot share secsnap`.
3. bob: `crowdvm new --from bsh_...`, then `crowdvm history NEWID --json`, then `crowdvm history NEWID cmd_3dw93ytfvnmq`.

Observed:
- The scrub ran (`/var/lib/crowdvm/scrub.json` is `{"id":"scr_...","done":true}`, and `~/.ssh/id_ed25519` is gone).
- bob's history still lists the admin's commands with `sandboxId` of the source sandbox. `crowdvm history NEWID cmd_3dw93ytfvnmq` prints `aws_secret=TOPSECRET-r9`, and the ssh session's record prints `ssh-secret=SSHSECRET-r9`.
- The files are in `/var/lib/crowdvm/history/` on bob's disk. The result was unchanged 20 s later.

Expected: `cur.History = cur.History || add.History` in `addPendingScrub`, and `!sc.History` in `scrubEmpty`. The unit test for the round-8 fix presumably calls `runScrub` or `foreignDeployScrub` directly, which skips the merge.

### 3. `--no-monitoring`: several large commands still fill `/run`, and then every exec fails for 10 minutes (stuck, wrong-result)

Severity: stuck and wrong-result. The round-8 fix for opus #3 caps each command, not the total.

Where: `internal/guest/commands.go:172` (`outputLimit`) caps each stream at 1/8 of the tmpfs, which is ~98 MB of the 784 MB `/run` on a small sandbox. Finished unmonitored records still stay for `keepUnmonitored = 10 * time.Minute` (`internal/guest/history.go:41`). Eight capped commands in a row fill `/run`.

Repro: `crowdvm new --type small --no-monitoring`, then 10 times `crowdvm exec --timeout 120 ID 'yes 0123456789abcdef | head -c 200000000' > o.$i`.

Observed:
- Runs 1-7: exit 0 with 102 652 416 bytes each. `df /run` climbs by 98 MB per run (13 %, 26 % ... 88 %).
- Run 8: exit 1, 101 769 216 bytes, nothing on stderr, then `error: could not start command: write /run/crowdvm/history/cmd_.../meta.json....tmp: no space left on device (command_failed, HTTP 500)`.
- Runs 9-10, and every exec after them: the same ENOSPC error. This is the round-8 failure, reached with 8 commands instead of one.

Expected: cap the total of kept unmonitored output (for example, prune finished records oldest-first when `/run` passes a threshold), or drop a finished unmonitored record as soon as its follower has read the exit frame.

### 4. The truncation warning says "1 GiB per stream" when the cap was ~98 MB (cosmetic)

Severity: cosmetic.

Where: `internal/cli/cmd_exec.go:344` prints a fixed `(1 GiB per stream)`. Since the round-8 fix, the guest cap is `min(1 GiB, tmpfs/8)` (`internal/guest/commands.go:172`).

Repro: as in bug 3, run 1. stderr says `crowdvm: the command wrote more output than the sandbox keeps (1 GiB per stream); the rest was dropped`, and stdout is 102 652 416 bytes.

Expected: no size in the message, or the real limit sent in the exit frame.

### 5. `kill -9` of `crowdvm exec` still leaks the command past `--timeout` (leak)

Severity: leak. Round-8 opus #5 left this open on purpose ("the server-side deadline ... was not made here"). I list it because it still reproduces.

Where: `internal/cli/cmd_exec.go:207-208` starts the detached command with `TimeoutSeconds = 0`. The deadline lives only in the CLI.

Repro: `setsid crowdvm exec --timeout 15 ID 'exec sleep 783' &`, then `kill -9` it after 3 s.

Observed: 40 s later, `ps -o etimes -C sleep` in the sandbox shows `sleep 783` at 43 s, still running.

Expected: send `--timeout` plus a margin as the guest-side deadline.

## Round-8 fixes rechecked live

| r8 item | Result |
|---|---|
| `KillMode=process` (opus 1) | Holds. The unit is `KillMode=process`. `systemctl --user restart` kept the qemu (same pid 557683) and the node logged `adopted running VM`. `~/data.txt` (synced), `~/unsynced.txt` (not synced), a background `sleep 9999` and `uptime -s` all survived. `kill -9` of the unit's main pid gave the same result (`NRestarts=1`). |
| Node restart during a lazy boot (opus 2) | Holds. With a 1.5 GB layer through the 40 MB/s proxy, I killed the node 2 s into the lazy boot and restarted it 5 s later. The guest hit I/O errors as before (101 curl failures, emergency mode). After 3 min the sandbox went from `error node_offline` to `error machine_error: boot timeout: guest agent not healthy after 3m0s`, not stuck. `crowdvm recover` brought it up with the 1.5 GB file's sha256 intact. |
| Output cap, `--no-monitoring` (opus 3) | Single command: holds (capped at ~98 MB, warning, exit 0). Several commands: not fixed, see bugs 3 and 4. |
| Foreign deploy history (opus 4) | Not fixed, see bug 2. |
| Marker kept out of stdout (opus 6) | Holds. The cut outputs are exactly the cap with no marker text. |
| Signed file download headers (sonnet 1) | Holds. The signed URL for `x.html` and `x.svg` answers `Content-Type: application/octet-stream`, `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox; default-src 'none'`. |
| Dashboard framing (sonnet 2) | Holds. `/`, `/device?code=ABCD` and `/sandboxes` send `X-Frame-Options: DENY` and `frame-ancestors 'none'`. |
| Desktop URL in an iframe | Holds. `/desktop/{id}` sends no framing headers. A page on another origin (127.0.0.1:9968) embedding the desktop URL in an `<iframe>` showed `connected` and the live XFCE desktop in headless Chromium. Default site isolation gave a stale out-of-process frame in the screenshot. With it off, the frame rendered. |
| SIGQUIT and Ctrl-\ (opus 5) | Holds. `kill -QUIT` and a real pty `\x1c` both make the CLI exit 130, and the remote `sleep` is gone. |
| Join command URL (opus 7) | Holds. `join-token create` without `--public-url` prints `--server http://127.0.0.1:9960`, the serve `--public-url`. |
| Clearing node labels (opus 10) | Holds. `nodes update r9n2 --label ""` gives `"labels":{}`. |

## Chaos run

Driver: a Python script in my scratch dir that ran in ~9-minute foreground segments, chained through a state file. It used 2 worker threads on 4-7 sandboxes (pool target 5, plus forks and deploys). Ops:
- create
- exec writing a 1-6 MB random file plus `sync` plus `sha256sum`
- stop `--wait`, resume
- fork `--wait`
- save (`snapshot ID name`, keeping the newest 3 saves and removing older ones)
- deploy (`new --from save`)
- delete

A disruptor thread acted every 15-120 s, from segment 5 on 0.3-4 s after a stop, fork, save or deploy had started, against the node hosting that sandbox or the server. Disruptions were `kill -9`, SIGTERM followed by a restart 2-10 s later, or SIGSTOP of a node for 5-45 s followed by SIGCONT.

After each disruption the driver waited for in-flight ops, settled (all tracked sandboxes in `ready`/`archived`/`running`/`idle` within 300 s), then checked:
- states (not stuck)
- error texts (any "lost" counts as a data-loss error)
- qemu processes against server placement (orphans, duplicates, VMs of deleted or archived sandboxes)
- VM dirs of sandboxes deleted more than 90 s ago
- every confirmed file's sha256, resuming archived sandboxes first

Totals: about 46 minutes of driver time, 16:16-17:13.

| Item | Count |
|---|---|
| Operations | 1 827: 16 create, 301 resume, 657 write, 187 stop, 69 fork, 196 save, 192 save removals, 64 deploy, 145 delete |
| Failed operations | 143. 131 are writes, mostly `machine_not_running` on a stopped sandbox (expected) or connection refused while the server was down. 2 deploys and 1 fork timed out waiting for capacity before I raised node capacity. |
| Disruptions | 24: server kill9 3, server term 4; node1 kill9 3, term 4, pause 4; node2 kill9 1, term 1, pause 4 |
| Invariant checks | 31 |
| Synced files verified | 2 835 (148 sandbox verifications) |
| Violations | 1: bug 1 (data). After I found it, the driver skipped `resume` on `node_offline` sandboxes. The skip never triggered later. |

What held:
- Every other synced file was intact after every resume, fork and deploy, through server SIGTERM and `kill -9` during stops and forks, node SIGTERM and `kill -9` during saves and stops, and node pauses of up to 45 s.
- No sandbox stayed in `error`, `starting`, `provisioning`, `cloning` or `archiving` past the 300 s bound.
- No data-loss error text appeared.
- No orphan or duplicate qemu was seen at any check, and no VM dir was left for a deleted sandbox.
- Snapshot layers of deleted sandboxes stay on a node while a live chain or saved snapshot builds on them, and are removed after that. My first leak check flagged 10 of these and was wrong; the 10 are retracted. The final check confirmed nothing was left.
- Final state after deleting everything: only base images on both nodes, no blobs on the server, no live snapshot rows.

## Not verified

- Bug 1 through the dashboard and the SDKs. The CLI path is enough to show it.
- Bug 2 for org members and shared org snapshots. Same code path (`addPendingScrub`), not run.
- `--zero-data-retention` for bug 3. It shares the tmpfs history path, so it is very likely the same.
- A real browser for the desktop iframe (headless Chromium only), and a real systemd unit file (a transient unit with the same properties).
- Throttling was applied only on the node↔server session. Guests reached the server directly.
