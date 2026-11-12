# Bug hunt r15 (opus): round-14 rechecks, encrypted-disk edges, guest egress, 2026-11-12

Setup: commit `ec65ad3`, built with `make build` from `git archive ec65ad3` into my own data dir (`~/.local/share/crowdvm-r15opus`). One `crowdvm-server` listened on 127.0.0.1:9960 with `--public-url http://127.0.0.1:9963`, `--public-domain r15.test`, `--log-level debug`, no start limits and (for the egress test) `--egress-allow 192.168.178.31:9968`. I restarted it several times, once with `--no-guest-channel` for the plain-sandbox test. Two qemu nodes, `r15n1` and `r15n2` (UI on :9961 and :9962), had 6 vCPU and 14 GiB at first, then 10 vCPU and 20 GiB so that a `large` sandbox fits, plus 40 GiB and at most 4 sandboxes each. Each ran as a transient systemd user service with the real unit's properties (`KillMode=process`, `NotifyAccess=main`, `FileDescriptorStoreMax=1024`, `FileDescriptorStorePreserve=yes`, `CROWDVM_FDSTORE=1`). Image: `ubuntu-24.04-crowdvm-minimal-v4` from `~/.local/share/crowdvm-dev/images/`, imported on both nodes. The agent ships on the config drive, so the image's age does not matter. Node↔server and guest↔server traffic went through a Python token-bucket proxy on :9963 at 40 MB/s per direction. Users: `admin` (owns both nodes) and `bob`. Other tools: a Python HTTP listener on :9967 as the LAN target, a TCP sink on :9968 that accepts and never reads, and headless Chromium (CDP on :9964, driven from Node) as the node owner's browser.

Cleanup: see the last section.

## Bugs

### 1. `crowdvm recover` always fails for a channel sandbox in `error`, so the advice in the crash notice loses the disk (data-loss)

Severity: data-loss. Every unannounced reboot of a channel sandbox stops it in `error`, and so does the node's reboot-loop guard (3 reboots in 2 minutes). The notice and the error text both say to run `crowdvm recover`, which "starts it again with its disk as it was". That call now always fails with `reset_failed … 404 not_running`. The owner is left with `crowdvm resume`, which boots the last snapshot and drops everything written since. This is a regression from `ec65ad3`: round 14 checked `recover` after a crash and it worked.

Where:
- `internal/server/guest_channel.go:369` (`handleGuestReboot`): the round-14 fix (bugs-r14-sonnet 1) refuses a ticket unless `sb.Usable()` (ready/idle/running).
- `internal/server/lifecycle.go:1906` (`handleRecover`) accepts `error` and `provisioned`, calls `expectReboot` (`:1930`) and asks the node to restart the VM. The node's re-ticket request then reaches the check above while the sandbox is still in `error`. The move to `provisioned` happens only after the node answers (`:1935`). `provisioned` fails the same way, so a sandbox stuck in its first boot cannot be recovered either.

Repro:
1. bob's `e1` (encrypted, on `r15n2`): `crowdvm exec e1 -- 'echo E > ~/markE; sync; sudo sh -c "echo b > /proc/sysrq-trigger"'`.
2. `crowdvm info e1` → `state error`, `new boot ticket: server: 403 unannounced_reboot: … crowdvm recover starts it again`.
3. `crowdvm recover e1`.
4. `crowdvm resume e1`, then `ls ~`.

Observed:
- Step 3: `{"code":"reset_failed","error":"The power-cycle failed: node: HTTP 500: … new boot ticket: server: 404 not_running: the sandbox is not running"}`. The state stays `error`.
- Step 4: `big1 big1.sum markA markB markD`. `markE` is gone.
- Same result for `f1`, whose VM I killed during LUKS format on its first boot, and for `g1` after three `sudo systemctl reboot`s in a row (`the machine rebooted 3 times within 2 minutes`).

Expected: a recover's re-ticket is accepted. For example, `handleGuestReboot` also accepts `error`/`provisioned` when the reboot was announced by `handleRecover` (`expectReboot` already marks it), or `handleRecover` moves the state to `provisioned` before it asks the node and the check allows `provisioned` with a recover pending.

### 2. A type change to a bigger disk never grows an encrypted sandbox's data partition (wrong-result)

Severity: wrong-result. An encrypted `small` sandbox resumed as `large` reports `diskGB 111` and is billed as large, but `/home` and `/` keep their 48 GB. No error appears anywhere.

Where: `internal/guest/preinit_linux.go:321` (`openData`, grow branch): `growpart` runs in preinit, where `/` (and so `/tmp`) is still the image's read-only root. `growpart` cannot make its temp dir and exits 2. The error is ignored, so `cryptsetup resize` and the later online `resize2fs` have nothing to grow.

Repro:
1. bob: `crowdvm new --type small --name e1 --node r15n1`, write 800 MB, `crowdvm stop e1`.
2. `crowdvm resume e1 --type large --node r15n2`.
3. In `e1`: `lsblk -b /dev/vda`, `df -h /home`, `sudo sfdisk -l /dev/vda`.
4. Cause check (in any sandbox): `sudo unshare -m sh -c "mount --bind /tmp /tmp && mount -o remount,bind,ro /tmp && growpart --dry-run /dev/vda 2"`.

Observed:
- `vda` is 121 332 826 112 bytes (113 GiB). `vda2` (crowdvm-data) still ends at sector 111 147 007 (49 G). `df /home` shows 48G. `cryptsetup status` shows `102725632 sectors`.
- In the running system, `growpart --dry-run /dev/vda 2` would grow it to 228 589 535 sectors.
- `console.log` has only preinit's normal lines. The agent logs `resize2fs overlay: exit status 1` (it tries the root, which is the overlay now).
- Step 4: `FAILED: failed to make temp dir`, rc 2 (`mkdir: cannot create directory '/tmp/growpart.1786': Read-only file system`).

Expected: preinit gives `growpart` a writable `TMPDIR` (mount a tmpfs, or point it at `/run`), or grows the partition with `sfdisk` itself, and logs a failed grow instead of ignoring it.

### 3. A sandbox's held egress connections make the server buffer up to 16 MiB each: one sandbox grew the server by 2.2 GB (crash)

Severity: crash. Round 14 capped DNS and the number of streams, and the DNS flood no longer moves the server (below). The 1024 TCP flows a sandbox may keep through server egress are still unbounded in memory. Each guest channel stream has a 16 MiB yamux receive window. When the destination stops reading, the server keeps up to 16 MiB per flow in its own heap, and keeps it for as long as the remote holds the connection. That is up to 16 GiB per sandbox, times every sandbox of every user. Any internet host that accepts and never reads is enough as a sink. I used an `--egress-allow` host because my test server has no public address.

Where: `internal/mux/mux.go:26` (`MaxStreamWindowSize = 16 << 20` for every channel session), `internal/server/guest_egress.go:31` (`maxEgressFlows = 1024`) and `:254` (`mux.Join` copies into a socket that does not drain, so yamux buffers up to the window).

Repro:
1. Server with `--egress-allow 192.168.178.31:9968`. On the host, a sink on :9968 with `SO_RCVBUF 4096` that accepts and never reads.
2. In bob's `e1` (as `user`): a Python script that opens 1000 TCP connections to `192.168.178.31:9968` and writes 64 KiB chunks to all of them for 60 s. A second run follows for 70 s.
3. Watch the server's `VmRSS` and `/healthz`.

Observed:
- Server RSS went 33 MB → 227 MB as the flows opened, then 287 MB, 430 MB, 773 MB, 1.17 GB and 1.56 GB (12:18:24–12:19:24). After the second run it was 2.26 GB.
- It stayed at 2.26 GB after both guest programs had exited (the guest kernel still drained their orphaned sockets into the channel), and after I killed the sink.
- The guest sent 2.4 GB in the first minute. The 40 MB/s proxy in the path is what kept the growth to ~25 MB/s.

Expected: per-flow memory on the server has a small bound. Use a smaller stream window for egress streams (for example 256 KiB, the yamux default), or a per-channel byte budget across flows, so a stalled destination applies back-pressure to the guest instead of to the server's heap.

### 4. A plain sandbox resumed bigger on a channel server reports `encrypted: true`, while all its earlier data stays in plaintext on the node, and it can use only about half its disk (security)

Severity: security, low. Sandboxes from before the secure layer (or made with `--no-guest-channel`) have their root partition grown to the whole disk, so preinit keeps them plain (`errNoRoom`). On a resume with a bigger type, the node grows the disk, and preinit finds more than 2 GiB free behind the root. It then creates a LUKS partition there and mounts the old root read-only as the overlay's lower layer, and `cp -a` copies `/home` and `/var/lib/docker` into the encrypted partition. From then on the API, the dashboard and the CLI say `"storage":{"encrypted":true}`. The user's whole earlier disk (their old home, Docker data, everything under `/`) stays plaintext in the chain, which every node that hosts the sandbox reads, and its free space can never be used again.

Where: `internal/guest/preinit_linux.go:78` (`part == ""` → `addDataPartition` on a disk that was plain before, with no check of what the server knows about the sandbox), `:399–405` (`cp -a` of the old `/home`), `internal/server/sandboxes.go:344` (`encrypted` is just the last boot's report).

Repro:
1. Server with `--no-guest-channel`. bob: `crowdvm new --type small --name p1` (on `r15n1`). `lsblk` shows `vda1 52G /`. Run `echo PLAINSECRET-r15-XYZ > ~/plain.txt` and `crowdvm stop p1`.
2. Restart the server without the flag. `crowdvm resume p1 --type large --node r15n2`.
3. In `p1`: `lsblk`, `df -h / /home`, and `sudo debugfs -R "cat /home/user/plain.txt" /dev/vda1`. Then `crowdvm info p1`.

Observed:
- `vda1 52G` (no mountpoint), and a new `vda2 60G` → `crowdvm-data` (crypt) on `/home`, `/var/lib/docker` and the rest. `df` shows 59G for `/` and `/home` on a 113 GiB disk.
- `debugfs` on the unencrypted `vda1` prints `PLAINSECRET-r15-XYZ`.
- `info` → `{"diskGB":111,"encrypted":true}`.
- `crowdvm snapshot pull` of the sandbox now answers 409 `snapshot_encrypted`. The older plain snapshot still pulls `plain.txt`.

Expected: a sandbox the server knows as plain stays plain, or is migrated for real. For example, preinit could add a data partition only on a first boot or when the server sends a key for a disk it knows is `luks`. Otherwise `storage.encrypted` should say that the lower layer is plaintext. The docs' upgrade section should say what happens to plain sandboxes.

### 5. After a server goes back from `--no-guest-channel` to the channel, a node's warm pool keeps booting a VM that no create can take (leak)

Severity: leak, low. The round-14 fix empties the pool only when a channel create arrives. The shape a legacy create recorded is kept in `warm.json` and survives node restarts. A node that gets no new create keeps a 2 vCPU / 4 GiB VM for good, and boots it again after every restart. Switching the flag back is the documented rollback path.

Where: `internal/node/warm.go:51` (`noteWarmShapeLocked` persists the shape to `warm.json`, `:43`). Only `vms.go`'s channel-create path clears it.

Repro: `crowdvm-node config set warm-pool 1` on `r15n1`. Server with `--no-guest-channel`, `crowdvm new` (`p1`) lands on `r15n1`, and the pool boots `warm_9n7gtkdu58j6`. Restart the server without the flag (p1 moves to `r15n2`), then `systemctl --user restart` the node.

Observed: `warm.json` still holds the `small` shape. After the restart the node booted a new pool VM, `warm_3u8w3mmqa342`, under the channel server. Every create the server sends now carries a channel, so none can take it.

Expected: the node clears the warm shape when its server says it uses the guest channel (the hello or the node's config from the server), not only on a channel create. Or it does not persist the shape at all.

## Round-14 fixes rechecked live

| r14 item | Result |
|---|---|
| DNS flood (opus 3) | Holds. The same 6 × 200 000 random `.invalid` queries from `e1`: server RSS 104 → 108 MB at peak, `/healthz` 200 throughout, 22 threads. The guest still resolved `example.com` afterwards, uptime unchanged. Other ways to grow it: idle streams and streams without headers are capped (2048 per channel, 30 s `ReadHeaderTimeout`/`IdleTimeout`, by code). Held TCP flows are not: bug 3. |
| Reboot ticket after a stop (sonnet 1) | Holds: after `crowdvm stop e1` (state `stopped`), `POST /node/v1/sandboxes/bx_…/reboot` with `r15n2`'s node token → `not_running`. A `sudo systemctl reboot` of a running sandbox still gets its ticket. The same check broke `recover` (bug 1). |
| Rebuild grace after a server restart (opus 4) | Holds, partly verified: `r15n1` stopped at 12:32:16, server stopped at 12:32:28 and started at 12:35:48. No rebuild happened, the node came back at 12:36:46, and `f1` kept `markF` and its uptime. `f1` was placed with `--node`, so it would not have been rebuilt anyway. The unpinned case is code-read only (`rebuildLost` returns while uptime < grace). |
| `resume` after a crash (opus 5) | Holds: sysrq-b one second after an 841 MB periodic snapshot started uploading. `crowdvm resume` booted that newer snapshot on the same node in 4 s (`markB` and the 800 MB file OK, `sha256sum -c` passed). `markC`, written after the snapshot, is lost as expected. `recover` was the way to keep it, and that fails now (bug 1). |
| `prepare-stop` from the node (opus 6) | Holds: with the ops token from `vm.json` → `{"removed":9,…}`, counts only, no paths. It still empties `/tmp` and `~/.cache` of a running sandbox, as the fix note says. `/v1/commands` → 403. |
| Warm pool with channel sandboxes (opus 7) | Holds under a channel server: `warm-pool 1` on `r15n1`, no pool VM after 60 s, none after a channel create. A shape left by a legacy create comes back (bug 5). |
| Node page, body-signed single-use requests (sonnet 5) | Works in headless Chromium: the page loads authorized and polls `/api/status` every 2 s (16 × 200). A double click on Pause sent two `paused=true` (both 200; "Paused · 1 still running"), and a third click resumed. The limits form saved `max-sandboxes 3`. Two parallel `call("/api/config",…)` with the same body both got 200 (distinct ms). `/api/logout` → 200, after which the page turned read-only and the next POST got 401 "Run `crowdvm-node open`…". |
| Lost welcome / nonce (sonnet 3) | Every boot I watched logged `guest channel connected` / `closed` / `connected` within 1–2 s (preinit's session, then the agent's), and every sandbox reached `ready`. I did not search the logs for `ticket_used` notices before deleting them. |

## New ground: what held

- **Guest egress after bbc8dad:** from `e1`, every LAN target was refused through the proxy (connection reset): the node's and server's LAN `192.168.178.31:9967`, `192.168.113.32`, `192.168.122.1`, Tailscale `100.78.214.33`, Docker's `172.17.0.1`. The server's log says why for each (`a network of the server (…)`, `a private (LAN) address`). `10.0.2.2:9967` was refused by the node, `10.0.2.2:9963` (the server) gave 200, and `https://example.com` gave 200. My listener logged nothing. As root: a dummy interface with `192.168.178.200/24` (so `inVMNetwork` takes the direct path) plus a host route via `10.0.2.2`, then flushing the nat redirect: `192.168.178.31` and `1.1.1.1` both got "Couldn't connect". The node's netstack stops direct traffic, so the guest-side rules are not the boundary. Other sandboxes are reachable only through their public URLs, as before.
- **Interrupted first boot:** I killed qemu (`kill -9`) when `console.log` showed `formatting the encrypted data partition`. The sandbox went to `error` (`qemu exited unexpectedly`). `resume` booted it cleanly: a fresh disk, LUKS formatted, `/home` empty, same key. `recover` fails (bug 1), so the "format again" path of preinit is reachable only through a reboot.
- **Node restart during a boot:** `systemctl --user restart` of `r15n1` 9 ms after a new VM's qemu started (before redemption). The VM was adopted from the fd store (`adopted running VM … state=starting`), redeemed its ticket from the memfd, formatted, and was `ready` 2.4 s after launch.
- **Many reboots:** two announced reboots in a row each got a ticket (`last10min=1`, `=2`). The third tripped the node's guard (`the machine rebooted 3 times within 2 minutes`) → `error`, by design, and then bug 1.
- **Disk shrink:** by code a disk never shrinks (`qemu.go` keeps the parent's size), and resume takes the parent's. Live, only the grow case was run (bug 2).
- **Snapshot pull of encrypted sandboxes:** `crowdvm snapshot pull <encrypted sandbox>` → 409 `snapshot_encrypted` with a clear message. A plain parent snapshot of the same chain still pulls (`home_user/plain.txt`).
- **Plain sandbox on v4 under the channel:** a `--no-guest-channel` sandbox resumed at the same size on the channel server would stay plain (root fills the disk, `errNoRoom`). A bigger size: bug 4.
- **`recover` in other states:** `ready` and reachable → 409 `sandbox_is_reachable`. `error` and `provisioned` → bug 1. I could not make a `ready` sandbox's agent stay dead (systemd restarted it), so `ready` + unreachable was not run.

## Not verified

- `recover` of a `ready` sandbox whose agent is down (the agent kept coming back). By code it works: `Usable()`, and the node retries 409 until the old channel closes.
- The rebuild grace for an unpinned sandbox (my test sandbox was pinned with `--node`).
- Fork of an encrypted sandbox, and a type change across a node without lazy chain.
- A kill during `luksFormat` itself, with a half-written header (`blank()` would then refuse to format, and the boot would fail closed). The window is shorter than my 20 ms poll.
- Bug 3 with more than one sandbox, and with a real internet sink. The 16 GiB per-sandbox bound is arithmetic (1024 × 16 MiB), not measured.
- A node that delays the stop report to keep the state `ready` while its guest announced a shutdown.

## Cleanup

Deleted every sandbox (`e1`, `f1`, `g1`, `p1`, `w1`). The server DB then had 0 live sandboxes and 0 saved snapshots. I stopped the server and both node units, killed my leftover qemu processes (the warm VM of bug 5 and a snapshot inspector VM), emptied the units' fd stores (`systemctl --user clean --what=fdstore`), and stopped the proxy, the listener, the sink and Chromium. Nothing of mine listens on 9960–9969, and no `r15opus-*` unit is loaded. `~/.local/share/crowdvm-r15opus` (binaries, source copy, node and server dirs, logs, pulls) is deleted. I did not touch the repo's working tree except for this report, and did not touch `crowdvm-field`, the e2e clusters or any worktree.

## Fixes

All 5 fixed; bug 1 by the same change as bugs-r15-sonnet 1. Tests: `internal/server/bughunt_r15_test.go` (1), `internal/guest/fix_r15_test.go` (4), `internal/node/fix_r14_test.go` (5). Bugs 2 and 3 have no unit test (preinit needs a VM; the window is a yamux setting); the e2e suite boots encrypted sandboxes through preinit.

2. **Bigger disk never grows** (`internal/guest/preinit_linux.go` `run`): preinit's commands get `TMPDIR=/run` (the root is read-only there), and a failed `growpart` is logged instead of ignored.
3. **Held egress flows** (`mux.GuestConfig`, `guest_channel.go`, `guest_egress.go`): the server's side of a guest channel uses a 1 MiB stream window instead of 16 MiB, and a sandbox may hold 256 egress flows instead of 1024, so stalled flows can hold at most ~256 MiB of server memory per sandbox (was up to 16 GiB). Guest→server transfers over the channel are bounded by that window per stream (about 20 MB/s at 50 ms round trip).
4. **Plain disk turned "encrypted"** (`preinit_linux.go` `bootedPlain`): the image ships an empty `/etc/machine-id`, and an encrypted sandbox writes its own on the data partition, so a machine-id on the image root means the disk already ran unencrypted: preinit leaves it plain (no new partition), and the server records `plain`.
5. **Warm shape after a rollback** (`proto.HeartbeatResponse.GuestChannel`, `nodes.go`, `node.go`, `warm.go`): the heartbeat tells the node that the server uses the guest channel, and the node then forgets the saved shape (and drops the pool).
