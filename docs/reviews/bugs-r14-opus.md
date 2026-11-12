# Bug hunt r14 (opus): secure layer in use, round-13 rechecks, 2026-11-12

Setup: commit `5697bfe`. The repo's working tree held another session's uncommitted edits (`internal/node/vm/qemu/{qemu,lazy}.go`), so I built from `git archive 5697bfe` with `make build` into my own data dir (`~/.local/share/crowdvm-r14opus`). One `crowdvm-server` listened on 127.0.0.1:9960 with `--public-url http://127.0.0.1:9963`, `--public-domain r14.test`, `--log-level debug` and no start limits (restarted several times, once each with `--node-egress` and `--no-guest-channel` for the warm-pool tests). Two qemu nodes, `r14n1` and `r14n2` (UI on :9961 and :9962), had 6 vCPU, 14 GiB, 40 GiB and at most 4 sandboxes each. Each ran as a transient systemd user service (`systemd-run --user`) with the properties of the real unit: `KillMode=process`, `NotifyAccess=main`, `FileDescriptorStoreMax=1024`, `FileDescriptorStorePreserve=yes`, `CROWDVM_FDSTORE=1`. So QMP and the NIC were socketpairs kept in systemd's fd store. Image: `ubuntu-24.04-crowdvm-minimal-v4` from `~/.local/share/crowdvm-dev/images/`, imported on both nodes. I built no image. The e2e suite takes its image from `CROWDVM_E2E_IMAGE`. Node↔server and guest↔server traffic (the guest channel dials `10.0.2.2:9963`) went through a Python token-bucket TCP proxy on :9963 (40 MB/s per direction). Users: `admin` (server admin, owner of both nodes) and `bob`, each with their own CLI config dir. A Python HTTP listener on :9967 served as a LAN target. Headless Chromium (Playwright's `chrome-headless-shell`, CDP on :9964) played the node owner's browser. A Python header logger played the squatter on a node's UI port.

Cleanup: see the last section.

## Bugs

### 1. A share-code recipient gets the sharer's disk key for good: as a node owner, they decrypt everything the sharer's sandbox writes later, env secrets included (security)

Severity: security. The secure layer promises that a node owner cannot read a sandbox's disk. That breaks for any node owner who has ever received a share of the sandbox, or a fork of it. The sandbox's later writes, its `/etc/profile.d/crowdvm-env.sh` (`CROWDVM_API_KEY`, `GH_TOKEN`, every env secret) and its home directory become readable. The key never rotates.

Where:
- `internal/server/guest_channel.go:84` (`ensureDiskKey`, called from `lifecycle.go:489`): a boot from a snapshot takes the snapshot's `data_key`. A deploy from a share code (or a fork) therefore opens the same LUKS2 volume with the same passphrase and volume key as the source sandbox. The design calls a fork "the way to re-key" (§5.4), but neither a fork nor a deploy re-keys.
- Root in the recipient's own sandbox can read that key at its next boot: `/dev/vda1` (the image partition below the overlay) is writable with `debugfs -w`, and `preinit` runs `cryptsetup` from `PATH`.

Repro:
1. bob: `crowdvm new --type small -e GH_TOKEN=ghp_R14CANARYxyz -e MYSECRET=…` (`e1`), `crowdvm snapshot e1 bobenc`, `crowdvm snapshot share bobenc`.
2. admin: `crowdvm new --from <code> --type small --no-env` (`adep`). `cryptsetup luksDump /dev/vda2` in `adep` and in `e1` shows the same UUID (`0cfd1467-…`).
3. In `adep`, as root: use `debugfs -w /dev/vda1` to put `/usr/local/sbin` first in `/usr/local/lib/crowdvm/preinit`'s `PATH`, and add a `/usr/local/sbin/cryptsetup` that `tee`s its stdin to `/run/stolen.key` before calling the real one. Then `sudo reboot`. The reboot is announced, so a new ticket comes and `preinit` runs. `/run/stolen.key` now holds the 64-byte key.
4. bob, after the share: `echo POST-SHARE-SECRET-bob-77Q > ~/after-share.txt`.
5. On `r14n2` (admin's node), as the node user with no root: `qemu-img convert -U` of `e1`'s running chain to raw, `dd` the LUKS header of partition 2, then `cryptsetup luksDump --dump-volume-key --key-file stolen.key hdr.img`. Decrypt the data area in user space (AES-XTS, 4 KiB sectors) and read it with `debugfs`.

Observed:
- `luksDump` printed the volume key (`MK dump: 67 5b 62 a0 …`).
- `debugfs -R 'cat /binds/home/user/after-share.txt'` → `POST-SHARE-SECRET-bob-77Q`. `/upper/etc/profile.d/crowdvm-env.sh` → `ASCII_TOKEN='cvm_…'`, `CROWDVM_API_KEY='cvm_stpep…'` and bob's other variables.

Expected: a deploy from a saved or shared snapshot, and a fork, gets its own key. That needs a re-encryption on first boot (`cryptsetup reencrypt` online, or a copy into a fresh LUKS volume) before the sandbox is handed over, or a per-sandbox key layer. Until then, `docs/guide/security.md` should say that whoever receives a share or fork can read the source sandbox's disk on any node they own.

### 2. A `--no-env` deploy of a shared snapshot still holds the sharer's env, git token and live sandbox API key in deleted blocks (security)

Severity: security. Round 13 fixed `adopted.json` and checked the scrubbed files, but deleting or rewriting a file does not remove its old blocks. The recipient is root in the deploy and reads the raw disk.

Where: `internal/guest/boot.go:587` rewrites `/etc/profile.d/crowdvm-env.sh`, `setup.go:259/264` the git credentials, and `warm.go` `ClearAdopted` deletes `adopted.json`. All of this happens on the recipient's boot, in the recipient's overlay. The sharer's blocks stay in the snapshot layers below. Nothing on the sharer's side scrubs before a snapshot is saved for sharing. With the encrypted disk it is the same: the recipient holds the key (bug 1).

Repro (server `--no-guest-channel --node-egress`, so a warm-pool sandbox exists, but the env file and git credentials are in every sandbox):
1. bob: `crowdvm new --node r14n1 -e GH_TOKEN=ghp_LEGACYWARM -e MYSECRET=legacy-warm` (`lw`, adopted from the pool), `crowdvm snapshot lw bobwarm`, `crowdvm snapshot share bobwarm`.
2. admin: `crowdvm new --from <code> --no-env` (`adw`).
3. In `adw`: `sudo grep -a -o '[[:print:]]\{0,80\}LEGACYWARM[[:print:]]\{0,80\}' /dev/vda1`, and the same for `CROWDVM_API_KEY`.

Observed:
- `/var/lib/crowdvm/adopted.json` is gone (the round-13 fix works).
- The raw disk gives `export GH_TOKEN='ghp_LEGACYWARM'` (2×), `https://x-access-token:ghp_LEGACYWARM@github.com` and `CROWDVM_API_KEY='cvm_yxey5rk96nzsb5wc7577pnnyqwas83n6htz325m7'` (2×).
- `GET /api/v1/sandboxes` with that key lists bob's `lw`: it is bob's live per-sandbox key.

Expected: before a snapshot is saved, shared or published (and in the stop path), the agent overwrites credential files in place (zeros, then `fsync`) before it deletes or rewrites them, then runs `fstrim`. Or the secrets never touch the disk: an env file on tmpfs (`/run`), git credentials through a helper that asks the agent.

### 3. One sandbox's DNS flood through server egress grows the server by gigabytes and can leave it serving nothing (crash)

Severity: crash. One sandbox (any process in it, even as `user`) takes down the server for everyone. All nodes lose their connection, every sandbox goes `node_offline`, and the server answers no request until it is restarted.

Where:
- `internal/server/guest_egress.go:206`: `POST /v1/egress/dns` has no per-sandbox rate or concurrency cap. TCP has `maxEgressFlows` (1024).
- `internal/guest/egress.go:235`: `resolve` builds a new `http.Transport` (`mux.Transport(sess)`) for every query. Each query's yamux stream then stays open, idle, for the transport's 60 s `IdleConnTimeout`, with one `net/http` connection goroutine on the server (`mux.Serve` sets no `IdleTimeout`).

Repro:
1. In bob's `e1`: a Python script that sends UDP DNS queries for random names under `.invalid` to `1.1.1.1:53` as fast as it can (iptables redirects them to the agent's resolver on 15053, which forwards each one over the channel).
2. 2 processes × 100 000 queries (11:26:28): server RSS 170 MB → 737 MB within 3 s, and it stayed there. The API still answered.
3. 6 processes × 200 000 queries (10:48:44–58, 12 s): server RSS 128 MB → 3.7 GB, not released.

Observed (step 3):
- The guest agent stopped answering at 10:49:16, and the VM rebooted at 10:50:00 (its new-ticket request then timed out).
- Both nodes lost the server at 10:50:16/22, including `r14n1`, which hosted none of bob's sandboxes. Their reconnects timed out until 10:53:06.
- After that, the server wrote no more log lines. At 10:54:59, `curl http://127.0.0.1:9960/` straight to the server (not through the proxy) timed out after 10 s, with the server at 84–250 % CPU.
- A `SIGQUIT` dump at 10:55: 6 850 goroutines, 6 688 of them in `net/http.(*conn).serve` → `yamux.(*Stream).Read`, the guest channel's DNS streams.

Expected: a per-sandbox cap on DNS queries in flight (like `maxEgressFlows`) and a rate limit. The guest keeps one long-lived client (`mux.Client`) for DNS instead of a transport per query, or does DNS over a single stream. `mux.Serve` sets an `IdleTimeout`.

### 4. After a server restart, sandboxes whose node went offline shortly before the server stopped are rebuilt elsewhere at once, and their node's newer disk is thrown away (data-loss)

Severity: data-loss. This hits any server that hangs (as in bug 3) or crashes while a node is reconnecting. Once the server is back after more than 3 minutes, the sandboxes of a node that dropped seconds before are rebuilt from their last stored snapshot before that node can reconnect. When it does reconnect, its running VMs are removed. Recovering from bug 3 this way rebuilt both of `r14n2`'s sandboxes.

Where: `internal/server/lifecycle_place.go:349` (`rebuildLost`): its grace check is `COALESCE(degraded_since,updated_at) < now − NodeLostGrace`. That counts the server's own downtime as the node's offline time. `seenAt` (`lifecycle.go:1042`), which starts the clock at `startedAt` for this reason, is not used here, and neither is `nodeLost`.

Repro:
1. bob's `e1` on `r14n1`. Write `~/markerD.txt` (after the last uploaded periodic snapshot).
2. 11:04:55: `systemctl --user stop r14opus-n1` (the VMs keep running). The server marks `e1` `error node_offline`.
3. 11:05:07: stop the server (`SIGTERM`).
4. 11:08:19: start the server. `r14n2` reconnects at 11:08:28, `r14n1` at 11:08:48 (its backoff).

Observed:
- 11:08:21, 2 s after start: `rebuilding sandbox after node loss sandbox=bx_b83q9syj lost=node_7araawcuwyw3`, and the same for `adep`.
- Both booted on `r14n2` from their head snapshot.
- `r14n1` reconnected 20 s later and logged `vm removed (the server asked: deleted, or now running elsewhere)` for both.
- `~/markerD.txt` is gone (`markerA`–`C`, from before the last snapshot, are there).
- The node had been offline for 41 s of server uptime, against a 3-minute grace.
- When no other node was online at the restart, the server adopted the returning node's VMs instead (`node came back with the machine of a queued rebuild`), so the loss depends on which node reconnects first.

Expected: `rebuildLost` measures the grace from `max(went offline, startedAt)`, like `seenAt`, so nodes get the full grace after a server restart.

### 5. `crowdvm resume`, as the "unexpected restart" notice suggests, discards the VM's disk and boots an older snapshot while the node still uploads a newer one (data-loss)

Severity: data-loss. After any crash, kernel panic or OOM panic of a channel sandbox (the guest has `panic=-1`), the owner is told to run `crowdvm resume`. That loses everything since the last *stored* snapshot, even though the node holds the disk and a newer snapshot it is uploading. `crowdvm recover` keeps the disk.

Where:
- `internal/server/guest_channel.go:362`: the notice text says "Run `crowdvm resume …` (or `crowdvm recover`)". `docs/guide/security.md` says the same.
- `internal/server/lifecycle.go:1555` (`handleResume`): a resume from `error` boots `HeadSnapshotID` and makes the node delete the VM. It does not wait for, or use, a newer snapshot the node reported but has not uploaded yet. Round 13's fix for this (`newerSnapshotOn`) covers only evacuations.

Repro:
1. bob's `e1` on `r14n2`, periodic snapshots every ~70 s. Write files (`~/art`, `~/fifo`).
2. 10:44:47: `echo b > /proc/sysrq-trigger` in `e1` (`state=error … unannounced_reboot`).
3. 10:45:12: `crowdvm resume e1`.

Observed:
- The periodic snapshot `snap_sdb7…` was taken at 10:44:46 (1.5 GB, upload in progress since 10:44:47).
- The resume booted from `snap_5j6kr…` (10:43:36), and `r14n2` logged `vm removed`. `~/fifo` (10:43:58) and `~/art` are gone.
- `snap_sdb7…` finished uploading at 10:45:24, 9 s after the resumed VM booted, and was never used.
- The same crash followed by `crowdvm recover e1` kept a file written 2 s before the crash.

Expected: the notice and the docs point to `crowdvm recover` (the disk is still on the node). Resume from `unannounced_reboot` either recovers in place while the node still has the VM, or waits for the node's newest snapshot like the evacuation path does.

### 6. The node's ops token deletes `/tmp`, `~/.cache` and Docker state in a running sandbox and returns the deleted file names (security)

Severity: security, low. The secure layer says the node may only "check its health and pause or stop" a sandbox, and the ops subset "returns no sandbox content". `prepare-stop` is in that subset. Called on its own, without a stop, it deletes files in a sandbox that keeps running, and it returns their paths. The node user does not need to change CrowdVM: the token is in `vm.json`/`vms.json` (0600, the node user's), and the agent's ops port is forwarded to `127.0.0.1:<rec.port>`.

Where:
- `internal/guest/channel.go:375` (`opsPaths` includes `POST /v1/internal/prepare-stop`).
- `internal/guest/internal.go:122` (`handlePrepareStop`): runs the user's pre-stop hook, `docker stop`, empties `/tmp` and `~/.cache`, removes ignored files, prunes Docker, vacuums the journal, `fstrim`. `:162` returns `removedPaths` (up to 200).

Repro: in `e1`, `touch /tmp/acme-merger-q3-plan.docx; mkdir -p ~/.cache/x; touch ~/.cache/x/customer-list-secret.csv`. On the node: `T=$(jq -r .spec.guest.token vm.json); curl -H "Authorization: Bearer $T" -XPOST 127.0.0.1:39045/v1/internal/prepare-stop -d '{}'`.

Observed: `{"removed":9,"removedPaths":["/home/user/.cache/x",…,"/tmp/acme-merger-q3-plan.docx","/tmp/systemd-private-…-systemd-resolved.service-…",…]}`. The sandbox kept running with an empty `/tmp` (the PrivateTmp directories of resolved, logind and timesyncd included). `/v1/commands` on the same port with the same token got 403 `channel_only`, as intended.

Expected: for channel guests, the cleanup runs only when the server asked for the stop (over the channel), or the ops call does only sync and freeze. The answer to the node carries counts, not paths.

### 7. The warm pool never works for channel sandboxes: under server egress it never boots a VM, under `--node-egress` it keeps one nobody can take (leak)

Severity: leak, low. A node owner who sets `warm-pool` gets no faster starts in the default setup. With `--node-egress`, the pool holds a VM (2 vCPU / 4 GiB here) for good.

Where:
- `internal/node/warm.go:46`/`:52`: `warmMatch` and `noteWarmShapeLocked` require `spec.Net == nil`. Server egress sends `Net: {deny: [0.0.0.0/0], noDns: true}` with every spec, so the shape is never recorded and nothing matches.
- `internal/node/vms.go:157`: a spec with `Guest.Channel` never takes a pool VM.

Repro:
1. Default server: `crowdvm-node config set warm-pool 1` on `r14n1`, restart it, then `crowdvm new --node r14n1` twice. No pool VM after 60 s (`vm.json` `net` = `{"deny":["0.0.0.0/0"],"noDns":true}`).
2. Server with `--node-egress`: `net` is `null`. `warm pool: vm ready vm=warm_wsfx6djex72p` at 11:11:58. Then `crowdvm new --node r14n1` booted a fresh VM: no `sandbox adopted a pool vm`, and the pool VM kept running next to it.
3. Server with `--no-guest-channel --node-egress`: the next create adopted it in 103 ms.

Expected: either the pool works for channel sandboxes (a pool VM that waits for a stub over the ops port, then fetches its config over the channel), or the node does not boot pool VMs while its server uses the channel, and `config set warm-pool` says so.

### 8. A squatter on the node page's port can replay the owner's signed request with another body (security)

Severity: security, low. Round 13 replaced the page's session header with per-request signatures. The signature covers method, path and time, but not the body, and the daemon keeps no record of used signatures. One captured `POST /api/config` (any setting the owner changes) lets the squatter set any other key for 60 s.

Where: `internal/node/ui_session.go:105` (`validRequest`): HMAC over `method path ms`, ±60 s (`uiReqSkew`), no nonce cache. `ui.html:258` (`sessHeaders`) signs without the body.

Repro:
1. Open `crowdvm-node open --print`'s link in Chromium. The page polls `/api/status` with `X-Crowdvm-Ui-Session: <nonce>.<ms>.<hmac>`.
2. Stop `r14n2`'s daemon. A header logger listens on :9962. The owner clicks Pause.
3. Start the daemon and replay the logged header 16 s later: `curl -XPOST -H 'X-Crowdvm-Ui-Session: 0edb4111….1794477734841.834f9b18…' -H 'X-Crowdvm-Ui: 1' :9962/api/config -d '{"key":"max-sandboxes","value":"1"}'`.

Observed: the logger got `POST /api/config` with `{"key":"paused","value":"true"}`. The replay with the other body answered `{"applied":true,"key":"max-sandboxes"}`, and `config get max-sandboxes` → `1`.

Expected: the signature covers a hash of the body, and the daemon refuses a signature it has already seen (a cache for the 60 s window).

## Round-13 fixes rechecked live

| r13 item | Result |
|---|---|
| Node page signing (5) | Works in Chromium: the page loads, polls `/api/status` with a fresh signature each time, Pause sets `paused true`, the limits form saved `max-sandboxes 3`, and `POST /api/logout` ends the session (the next signed POST got 401). The page has no logout button; I called `/api/logout` from the page's own `call()`. A squatter gets only signed headers, but can replay a POST with another body (bug 8). `/api/status` answers without a session (node id, data dir, sandboxes). |
| `adopted.json` (1) | Gone in a `--no-env` deploy of a shared snapshot of a warm-pool sandbox (legacy channel-less server, the only mode where pool VMs are adopted, bug 7). Its contents and the other credentials are still in the raw disk (bug 2). |
| Evacuation, node dies mid-stop (3) | Holds, with encrypted sandboxes. `crowdvm nodes evacuate r14n1 --grace 0`, then `kill -9` of the daemon at the first upload (11:21:39). `adep` (stop snapshot already stored) resumed on `r14n2` at 11:24:55. `e1` waited in `error snapshot_unavailable …`. `r14n1` came back at 11:25:25, uploaded the 802 MB layer by 11:25:45, and `e1` booted on `r14n2` at 11:25:55 with both 800 MB files intact (`sha256sum -c` OK). |
| `find` on a huge line (4) | Holds: a 1.5 GB single line plus a needle on line 3 → found in 0.12 s, agent RSS high-water 39 MB. A needle past 64 KiB on the long line is not found, as documented. |
| FIFOs (7) | Hold: `GET /files`, the signed download URL and `replace` answer 400 `not_a_regular_file`. 30 more tries left the agent at 10 threads. `files/info` still reports a FIFO as `mode 0644`, `isDir false`. |
| Folder download of shrinking files (9) | Holds: truncating a 300 MB file 3 s into the download gave `http=200`, a valid gzip, and a tar with `a.bin` (zero-padded) and `z.txt`. |
| Guest calls across a reboot (6) | Holds: with sysrq-b during a `sleep 30` command, the call ended after 5 s with 502 `EOF`. A channel sandbox now stops on an unannounced reboot (`security_unannounced_reboot` notice), as documented; see bug 5 for what `resume` then does. |

## New ground: what held

- **Node side, nothing in plaintext:** `config.iso` holds the stub only (sandbox id, ops token, channel URL, pin, `ticketFrom: 10.0.2.2:7072`, an empty ticket). `vm.json` and `vms.json` hold the ops token and no ticket. `grep` for the env canaries, bob's API key and the ticket in both node logs, `console.log`, `qemu.log`, `vm.json`: 0 hits. `console.log` has only preinit's three lines and the login prompt. Server snapshot blobs: 0 canary hits.
- **QEMU under the fd store:** QMP is `-chardev socket,fd=3`, the NIC `-netdev stream,addr.type=fd`. No socket files, and the qemu cmdline holds no secret. `/proc/<qemu>/limits` shows core `0 0` (hard 0, so the node user cannot raise it). `NoNewPrivs: 1`, Yama `ptrace_scope` 1. `systemctl --user restart` of a node: both VMs adopted from the fd store (`NFileDescriptorStore=4` before and after), and channels reconnected in 4 s and 15 s. After `systemctl stop` the unit stays loaded with its 4 fds, and `start` adopts the VMs again.
- **Ops port:** `127.0.0.1:<port>` → guest :7070 answers `GET /v1/health` to anyone on the machine (sandbox id, uptime, versions). Every non-ops path, with the ops token, got 403 `channel_only` (`/v1/commands`, `/v1/health/../commands`). Bug 6 is the exception.
- **Lifecycle of encrypted sandboxes:** stop → resume on the other node (lazy chain): 800 MB file intact, an apt package (`sl`) and the env kept. Daemon restart, server restart (channels back in 2–5 s with the stored session), evacuation (above), `recover` after a crash (disk kept). A saved snapshot deploys to another account and reads the same files (bug 1 is the cost). `snapshot tree` of an encrypted snapshot → 409 `snapshot_encrypted`.
- **Server egress:** `apt-get install` and `curl https://example.com` (200) leave through the server. From the guest, these were refused: the host's LAN IP (`192.168.113.32:9967` and `:9963`), `169.254.169.254`, `100.100.100.100`, and `10.0.2.2` on any port but the server's. `0.0.0.0`, `127.0.0.1`, `[::1]`, `[::ffff:127.0.0.1]`, `[64:ff9b::7f00:1]`, `[2002:7f00:1::]` and `[::7f00:1]` stayed inside the guest or had no route. `10.0.2.2:9963` (the server) answered 200. My listener on :9967 logged nothing. The proxy and DNS ports refuse connections from the node side of the NIC (`egressFromNIC`). Docker traffic is covered (`PREROUTING`).
- **DNS (not a bug, a note):** the relay uses the server's resolvers, so a sandbox resolves the server's internal names: `_gateway`, the server's hostname (all its LAN, libvirt and Tailscale addresses), `.local` names and the company's split-DNS zone. Connections to those addresses are still refused. Node egress did the same with the node's resolvers before.
- **Ticket and reboot rules:** an unannounced reboot gets 403 and a notice. `sudo reboot` (announced) got a new ticket and kept the disk.

## Not verified

- Two servers or a replayed session against the channel, and a node that relays, delays or drops channel frames. I read `redeemGuest` (the session check, newest connection wins) but ran nothing.
- Pin rewriting and a MITM (documented class B), and the node user taking the fds out of systemd's fd store by changing the unit's `ExecStart`. Changing CrowdVM's unit is class B by the design's definition, so I did not try it.
- Fork of an encrypted sandbox. Not run; by the code it shares the key like a deploy (bug 1).
- A host reboot (fd store across a user-manager restart), and Windows nodes.
- `--egress-allow`: code-read only (an exact `ip:port` or a CIDR match comes before the private-range checks).
- Bandwidth limits on server egress: none in the code. A sandbox can fill the server's uplink, and on a slow node link it also slows the node's control traffic (seen during bug 3, but my 40 MB/s proxy was part of that path).
- Bug 3's exact trigger point between 200 k and 1.2 M queries, and whether the server recovers by itself after longer than 5 minutes.

## Cleanup

Deleted every sandbox (`e1`, `adep` and the warm-pool test sandboxes). Before stopping the server I removed the saved snapshots `bobenc`, `bobwarm` and `r14pull` (`crowdvm snapshot rm`, which also ends their share codes). The server DB then showed 0 live sandboxes, 0 saved snapshots and 0 live snapshot rows, and both nodes' `vms.json` were `{}`. I stopped the server, both node services (`systemctl --user stop`, then `reset-failed`), every qemu of mine, the proxy, the listeners and Chromium. Nothing of mine listens on 9960–9969, and no transient `r14opus-*` unit is loaded. My data dir `~/.local/share/crowdvm-r14opus` (binaries, source copy, node and server dirs, logs, the decrypted scratch images) is deleted. I did not touch the repo's working tree or `bin/`, except for this report.

## Fixes

6 of 8 fixed; bug 8 by the same change as bugs-r14-sonnet 5. Tests: `internal/server/fix_r14_test.go` (4, 5), `internal/mux/fix_r14_test.go` (3), `internal/guest/fix_r14_test.go` (6), `internal/node/fix_r14_test.go` (7), `internal/node/bughunt_r14_test.go` (8).

3. **DNS flood** (`guest_egress.go`, `mux.go`, `guest/egress.go`, `guest/channel.go`): the server answers 503 past 64 DNS queries in flight per channel; a guest channel serves at most 2048 streams at once (`mux.ServeLimit`); `mux.Serve` closes idle streams after 30 s (clients let go after 25 s); the agent keeps one DNS transport per session and drops queries past 128 in flight.
4. **Rebuild counts server downtime** (`lifecycle_place.go` `rebuildLost`): nothing is rebuilt before the server has been up for the node-lost grace, so a node that dropped just before a restart gets the whole grace to come back.
5. **Resume after a failure boots an older disk** (`lifecycle.go` `handleResume`): a resume from `error` boots from the newest snapshot the sandbox took since its last boot when its node still holds it (`newerSnapshotOn`); the boot waits for the upload. The notice already names `crowdvm recover` on the secure-layer review branch.
6. **`prepare-stop` paths to the node** (`guest.go`, `internal.go`): a channel guest answers its node with counts, not file names. The node can still run the cleanup without a stop; it can also stop the sandbox, which runs the same cleanup.
7. **Warm pool and the channel** (`internal/node/warm.go`): a create with a guest channel clears the pool's shape, so the pool drops its VMs and boots none while the server sends channel sandboxes.

Not fixed, need a design decision on the secure layer:
1. **Shared disk key**: a deploy or fork keeps the source's LUKS volume key, so its owner (as a node owner) can decrypt the source's later writes. Needs a re-encryption on first boot of a foreign deploy (or of every fork), or a per-sandbox key layer, or a documented limit.
2. **Deleted blocks**: secrets that were on the disk (env file, git credentials, `adopted.json`) stay readable in a share's free blocks. Needs secrets kept off the disk (tmpfs env file, a credential helper) or a scrub with overwrite and `fstrim` before a save or share.
