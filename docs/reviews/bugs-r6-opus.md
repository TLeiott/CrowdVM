# Bug hunt r6 (opus): live cluster, 2026-11-11

Setup: I built the tree at 13:08 and copied the binaries aside so later rebuilds could not change them mid-run. One `crowdvm-server` ran on 127.0.0.1:9960 with start limits off. Two qemu nodes, `r6n1` and `r6n2`, had status UIs on 9961 and 9962. They started with 6 vCPU, 14 GiB and 60 GiB each, and later ran with 12 vCPU, 24 GiB and 6 sandboxes. Both used `ubuntu-24.04-crowdvm-v3` and `-minimal-v3`, reflinked from `~/.local/share/crowdvm-dev/images/`. Fake upstreams for CI, OTel and key-leak checks ran on 9967 to 9969, on loopback and on the LAN address 192.168.113.32. Port 9963 was for two short-lived servers: an upgrade test on a copy of an older data dir, and a restore test. Users were `admin`, `alice`, `bob` and `mal`, with orgs Alpha (alice owner, bob admin), Beta and two throwaway orgs named Gamma.

Cleanup: every sandbox and saved snapshot is deleted. My server, nodes, VMs and fake upstreams are stopped, and no qemu process of mine is left. The data dir `~/.local/share/crowdvm-r6opus` and my scratch dir are deleted. The CLI ran with its own HOME, so nothing went to `~/.ssh` or `~/.config`. Another agent's e2e cluster was running under `~/.local/share/crowdvm-dev/e2e` the whole time. I did not touch it. The `/tmp/crowdvm-*-net.sock` files left in `/tmp` come in groups of three, two minutes apart, which matches its runs, not my VMs.

## Bugs

### 1. `-BlacklistThreshold 0` makes the desktop lock out after one aborted connection (stuck)

Where: `internal/guest/desktop.go:139` (the `patchOldImage` drop-in), `internal/guest/desktop.go:192` (fallback Xvnc), `internal/guest/browser.go:168` (browser stream) and `images/rootfs/full/usr/local/lib/crowdvm/desktop-session:6`. All four pass `-BlacklistThreshold 0`.

The round-5 fix assumes 0 turns the blacklist off. In TigerVNC 1.13.1, the version in the v3 image, it does the opposite: the threshold becomes zero marks, so the first connection that closes before its handshake blocks 127.0.0.1. Every desktop relay connection comes from 127.0.0.1. The default, 5, at least tolerated four.

The drop-in itself works. Right after boot and after stop/resume, `Xvnc` runs with the flag and `systemctl show` lists `/run/systemd/system/crowdvm-desktop.service.d/blacklist.conf`. The flag is the problem.

Repro A, through the relay:
1. `crowdvm new`, then open the desktop websocket with a fresh token and finish the RFB handshake. It works: `RFB 003.008`, auth OK, 1920x1080.
2. Open one more websocket with a fresh token and close it right after the upgrade, before reading the RFB version.
3. Connect properly with a fresh token. Xvnc answers `RFB 003.003` plus "Too many security failures". The guest journal shows `Connections: blacklisted: 127.0.0.1`. It recovers after about 10 s. Each further abort doubles the block, as in round 5.

Repro B, inside the guest, without CrowdVM: start `Xvnc :7 -rfbport 5977 -localhost -SecurityTypes None -BlacklistThreshold N`, complete one handshake, close N bare TCP connections, then try three handshakes.

| flags | aborts | next handshakes |
|---|---|---|
| `-BlacklistThreshold 0` | 1 | refused, refused, refused |
| `-BlacklistThreshold 0` | 6 | refused ×3 |
| default (5) | 1 | OK ×3 |
| default (5) | 6 | refused ×3 |
| `-BlacklistTimeout 0` | 20 | OK ×3 |
| `-BlacklistThreshold 1000000` | 20 | OK ×3 |
| `-BlacklistThreshold 0 -BlacklistTimeout 0` | 20 | OK ×3 |

On a public desktop (`crowdvm desktop ID --public`) no token is needed. One anonymous websocket opened and closed locked the owner out, and a script that does this every 10 s keeps them out. A reload, a closed tab or a flaky network during the handshake does the same to the owner's own session.

Expected: aborted connections never lock anyone out. Use `-BlacklistTimeout 0`, which I checked above, in all four places. `patchOldImage` keys on the string `BlacklistThreshold`, so it must also replace the old flag in session scripts that already carry it.

### 2. `scp --api -r` still lets a sandbox write outside the download folder (security)

Where: `internal/cli/transfer.go:373` writes the archive to `/tmp/crowdvm-download-<random>.tgz` in the sandbox. That folder is world-writable, and the archive belongs to `user`, the account agent code runs as. `extractTar` (`:424`) maps an entry named exactly like the root folder to `name == ""`, so `target == dest`. `checkNoSymlinkParents` returns nil for `target == dest` (`:343`), and for a deeper entry `rel` is `.`, so it never checks `dest` itself. A symlink entry for the root (`:443-447`) therefore replaces `dest` with a symlink, and every later file entry is written through it.

Repro, with no root in the sandbox:
1. Host: `mkdir victim; echo ORIGINAL > victim/keep.txt`.
2. Sandbox, as `user`: build `/tmp/crafted.tgz` holding `evil` (a symlink to the host's `victim` folder) and then `evil/pwned.txt`. Run a loop that, as soon as a new `/tmp/crowdvm-download-*.tgz` appears, does `cp /tmp/crafted.tgz /tmp/x.tgz && mv -f /tmp/x.tgz <that path>`. `tar` keeps writing to the unlinked inode, and the name now points at the crafted file. I started it with `crowdvm exec --detach ID 'python3 ~/swap.py'`.
3. Sandbox: `mkdir ~/evil; echo hello > ~/evil/a.txt`.
4. Host: `crowdvm scp --api -r ID:/home/user/evil ./xfer/evil`.

Observed: exit 0 and no warning. `./xfer/evil` is a symlink to `victim`, and `victim/pwned.txt` contains `PWNED-BY-SANDBOX`. With a path like `/home/<user>/.bashrc`, or with `../..` links relative to the destination, this is the same full escape as round-5 bug 1. The swap won on the second try. The first try polled `pgrep` to wait for `tar` and lost the race.

Expected: refuse a symlink, or any non-directory, for the root entry, and `Lstat` `dest` itself in `checkNoSymlinkParents`. Better still, don't trust a file the sandbox user can replace between `tar` and the download. Stream `tar` over the exec output, or write it somewhere `user` cannot write. `snapshot pull` uses the same `extractTar` (`cmd_snapshot.go:356`). I did not get a hostile tar through the snapshot reader, but the `dest` check belongs in the shared function.

### 3. A node restart of a few seconds turns every sandbox on it into `error`, and the CLI and SDKs give up (wrong-result, leak)

Where: `internal/server/lifecycle.go:718-740` (`onNodeOffline`) moves every starting or usable sandbox to `state=error, error=node_offline` the moment a node's connection drops, and emits `sandbox.error`. The server itself treats that state as passing (`passthrough.go:140`, and the reconnect brings the sandbox back to `ready`). The clients treat `error` as final: `internal/cli/helpers.go:35` (`isFinalBad`, used by `new`, `resume` and `fork --wait`), `sdk/python/src/crowdvm/helpers.py:23` and `sdk/typescript/src/helpers.ts:19` (`wait_until_ready` / `waitUntilReady` raise `SandboxFailed`).

Repro:
1. `crowdvm new --from bulk2 --node r6n2 --name k1 --json`. The 1 GB layer starts downloading on r6n2.
2. `kill -9` the r6n2 daemon and start it again a second later.

Observed: `new` printed `{"event":"state","state":"error"}` and `sandbox entered state error: node_offline`, then exited 1. Two seconds after the node came back, the node log shows `adopted vm`, the download finishing and `guest healthy`, and `crowdvm info` shows k1 `ready`, running and billing. An agent that retries `new` on failure now pays for two sandboxes and never deletes the first.

A clean restart does it too. Restarting both nodes with SIGTERM, the documented way to apply `config set cpus` (`node-owners.md:55`), took 3 s. In that window both running sandboxes of alice went to `error`, the server logged a `sandbox_error` notice for each, and webhook or OTel consumers got `sandbox.error`. The VMs never stopped: `uptime -s` was unchanged.

Expected: a short disconnect isn't an error. Keep the old state, or a passing health flag, until the 3-minute rebuild deadline, and send `sandbox.error` only then. At the least, the CLI and both SDKs should keep waiting on `error` with `error: node_offline`.

### 4. Secret files still replace or skip directories that are missing from the list (cosmetic, can break the desktop)

Where: `internal/server/feat_env.go:556` (`homeDirs`) and `:586` still compare against a fixed list, only longer than in round 5. Children of listed folders are allowed on purpose, and many of those children are themselves directories.

Repro: I listed every directory up to depth 3 in the home of a v3 sandbox and compared it with the list. 41 are missing, among them `.cargo/bin`, `.rustup/toolchains`, `.claude/skills`, `.codex/skills`, `.agents/skills`, `.config/opencode`, `.config/xfce4/xfconf`, `.config/xfce4/panel`, `.config/Thunar`, `.cua-driver`, `.local/share/applications` and `.dbus/session-bus`. I then ran `crowdvm env new sf2` and `env set-file sf2 P` for `.cargo/bin`, `.config/xfce4/xfconf`, `.claude/skills`, `.config/opencode` and `.cua-driver`. All five were accepted. Then `crowdvm new --type small --environment sf2`.

Observed:
- `~/.config/xfce4/xfconf` became a file, and neither `xfdesktop` nor `xfwm4` ran. The desktop is broken in the same way as in round 5.
- The other four stayed directories. The only trace is lines like `file /home/user/.cargo/bin: rename ... file exists` in the guest journal. `crowdvm info` shows nothing, and the secret never arrives.

Expected: decide in the guest, which sees the real disk. Refuse to replace an existing directory, and report the skipped path in the sandbox status. Or deny everything under the listed folders, not just the folders themselves.

### 5. A balance whose 7th decimal is exactly 5 still can't be moved as shown (wrong-result, minor)

Where: `internal/server/credits.go:47`. The round-5 fix takes the exact balance when `amount-bal <= 5e-7`. `round6` rounds half away from zero, so the largest round-up is exactly 5e-7, and in float64 the gap comes out as 5.0000000002e-7.

Repro:
1. `crowdvm credits transfer --to <new org> --amount 0.1234565`. The org shows 0.123457.
2. `crowdvm credits transfer --from <org> --to alice --amount 0.123457` gives 402 `insufficient_credits`.
3. `crowdvm org delete` gives 409 `org_has_balance`.

The round-5 case, 0.99458293334 shown as 0.994583, now works: the transfer and the delete both succeed. For the case above, two steps get out. Transfer 0.123456, which leaves 0.000001 as shown, then transfer 0.000001, which the clamp takes. Expected: compare `round6(amount) == round6(bal)`, or give the tolerance an epsilon.

### 6. Ctrl-C on `exec` leaves the command running until its timeout (wrong-result, minor)

Where: `internal/cli/cmd_exec.go:133-136`. For `--timeout` between 1 and 600, the default 30 included, text mode uses `runStream` (`:157`) and `--json` uses the sync call. Neither stops the command when the CLI is interrupted. Only `runLong` (`:216`, for `--timeout 0` or above 600) calls `StopCommand`.

Repro: `crowdvm exec --timeout 60 ID 'exec sleep 777'`, then SIGINT after 3 s. The CLI exits 130 and prints `interrupted`. `pgrep -a sleep` in the sandbox still shows `sleep 777`. With `--timeout 0` it is gone. With `--timeout 20` the guest killed it after 20 s, so the leak is bounded by the timeout, at most 10 minutes. The sync path behaved the same before round 5, so this is not a regression. Still, the same Ctrl-C kills the command with one timeout and leaves it running with another. Expected: call `StopCommand` when the context is cancelled in `runStream` and in the sync path, as `runLong` does.

### 7. The node never shows a server-side drain (cosmetic)

Where: no drain state reaches the node. `internal/node/ui.go:67` reports only the local `Paused`, and there is no drain field in `internal/proto` or `internal/node`.

Repro: `crowdvm nodes drain r6n2`. `crowdvm-node status` and `GET 127.0.0.1:9962/api/status` both look normal, with `paused: false` and nothing about a drain. The node gets no new sandboxes, and its owner cannot see why. `node-owners.md:130-131` tells owners to clear a server drain with `crowdvm nodes undrain`, so they need to be able to see one.

### 8. The security guide names the wrong key file for backups (cosmetic)

Where: `docs/guide/security.md:140` says environment secrets, AI credentials, telemetry headers and webhook secrets are encrypted "with a key in its data directory (`secret.key`)". Environment secrets use `env-secrets.key` (`internal/server/feat_env_crypto.go:16-25`). Webhooks use `secret.key` (`feat_webhooks.go:508`), which only appears once something needs it. An operator who backs up the database and the one key the guide names loses every environment secret. Name both files.

## Round-5 fixes rechecked: what held

- Desktop drop-in: the drop-in is in place right after boot and after stop/resume, and Xvnc runs with the patched script. It's the flag that's wrong (bug 1). A spent token gets 401 `desktop_token_used` before any VNC connection opens: 8 reuses of one token didn't touch Xvnc.
- `scp --api -r`: the round-5 planted `*.crowdvm-partial` symlinks are no longer followed, and `..` and absolute names are neutralised. Bug 2 is a new hole.
- Text-mode exec: exit codes 42 and 3, 137 for `kill -9`, 143 for TERM, 124 plus `command timed out after 2s` for `--timeout 2`, stderr on stderr. 20 199 999 bytes of text on stdout came through byte-exact past the old 8 MiB cap, with 5 MB on stderr at the same time. Stdin is not forwarded, so `cat` sees EOF. Boat says to use SSH for stdin. Binary output turns invalid UTF-8 into U+FFFD, which is the same as Boat's JSON API. Neither is a bug.
- CLI config: `logout` during three parallel `new` runs that each saved `current` stayed logged out, and `current` was kept. A separate profile with another user works, and logging it out leaves `default` signed in. `--api-url` with `/api/v1/`, a trailing `/` or upper-case `HTTP://` keeps the saved key. `localhost` instead of `127.0.0.1`, or a foreign server, gets no key: the fake server on 9967 saw only `auth=None` across list, status, credits, info, exec, ssh-config, desktop, dashboard and `CROWDVM_API_BASE`.
- Node shared downloads: two `new --from bulk --node r6n2` shared one 1.5 GB download. Deleting the first while it downloaded didn't stop the second, and it came up with the data. After `kill -9` mid-download, the restarted node removed the `.part` file on start.
- Queued fork: with both nodes full, `fork` waited (`waitingForCapacity: true`). I deleted `checkpoint.txt` in the source and wrote `after.txt`, then freed a node. The fork has `checkpoint.txt` and no `after.txt`.
- Service-account sandboxes: bob, an org admin, deleted a sandbox created by a service account. After the account's key expired (403 `api_key_expired`), alice deleted its other sandbox and then the account. Deleting an account that still has sandboxes gives a clear 409 `member_has_sandboxes`.
- OTel: mal's collector answered after 9.5 s. Alice's `sandbox.archived` reached her collector 1.8 s after `stop`, which is about how long a stop takes. Neither collector got the other tenant's sandbox ids.
- Rounded balance: the round-5 repro now transfers in full, and the org deletes. Bug 5 is the remaining edge.
- CI `apiUrl`: `#` and `?` are refused. `%23`, `%3F` and `/..` stay in the path the fake server receives, so the appended `/user` survives. `[::ffff:127.0.0.1]` and `localhost.` are refused, and `127.1`, `0` and `2130706433` fail the DNS lookup.
- Secret files: the round-5 names (`.dbus`, `.crowdvm`, `.local/bin`, `.config/xfce4`, `.gnupg`, `.ascii`) are refused now. Bug 4 is about the paths below them.

## New areas: what held

- Server restart: I stopped and restarted the server with 2 running sandboxes, a hosted port with a token URL, a share code, an API key, 2 orgs, 2 saved snapshots, an environment with 5 secret files and OTel configs. Afterwards everything matched except balances, which kept billing. The hosted URL still answered, and a detached command kept running and could still be followed.
- Upgrade: this tree's server started on a reflinked copy of the 2026-11-08 dev-cluster data dir. Migrations ran with no warnings or errors, `sb_` ids work in `list` and `info`, and old snapshots are listed. The three days of downtime were not billed: one charge of 0.000041 per sandbox, then nothing. Its sandboxes, whose nodes are gone, were rebuilt or marked `node_offline` as designed.
- Backup: a plain `cp -a` of the live data dir passed `pragma integrity_check`, started on another port, and kept the saved snapshots and decryptable env secrets.
- Node `pause` via `config set paused true` applied at once. The running sandbox kept running, `--node r6n1 --fail-fast` got `no_ready_machine`, and new sandboxes went to r6n2. After a server `drain` of r6n2, a resume moved the sandbox to r6n1 with its data. The status UI's POST routes require the custom header, so a web page cannot change them cross-site. Running VMs survived a SIGTERM restart of both nodes (same `uptime -s`).
- Scheduler with 12 parallel `new --type small`: 6 placed, 4 per node, and 6 waited. The limit was disk: each sandbox reserves 51 GB of the 240 GB the nodes allow (60 GB × overcommit 4). Deleting 2 placed 2 of the waiting ones within 12 s. After all were deleted, `free` went back to the exact baseline.
- Python SDK, 18 flows: create, `wait_until_ready`, `command` (exit code, timeout), `exec_command`, `stream_command`, `run_command`, detached plus `command_status`, `read_text`/`write_text`, signed upload and download of 300 kB of random bytes, `host_port` public, `get`, `limits`, saved snapshot, idempotent `create --from` and `fork`, `paginate` with `limit=1`, `NotFoundError`, browsing a stopped sandbox's snapshot. All passed.
- TypeScript SDK from `dist`, which is newer than `src`, 13 flows: the same set plus UTF-8 text and an `AbortSignal` that stopped a stream in 305 ms. All passed.
- Long uptime: 9.5 minutes of load ran about 4 300 rounds, each with 2 execs, a hosted fetch, an `scp` upload, a desktop URL and a full VNC handshake, plus a create/delete every minute. Nothing failed. Server: 20 fds and 20 threads throughout, RSS between 55 and 122 MB with no trend. node1: 27 to 32 fds, 21 threads, RSS 96 to 102 MB over the last 7 minutes. node2 was idle and flat. There is no pprof endpoint, so I counted threads, not goroutines.

## Not verified

- A hostile tar through `snapshot pull`: code reading only, see bug 2.
- An upgrade from a tagged release. The oldest data I had was the 3-day-old dev cluster.
- Goroutine counts. There is no pprof or debug endpoint, so threads stand in for them.
- Windows nodes, the macOS CLI, and agent prompts. Codex has no quota, and I had no reason to spend Claude prompts on these areas.

## Mistakes on my side

- I first put the images in `node*/images` instead of `node*/vm/images`, and then without the store's own JSON. The two failed creates that followed were my setup, not bugs. `crowdvm-node image import` is the supported way.
- An org-scoped `list --all` also lists personal sandboxes, which matches Boat, and I deleted alice's personal `w1` with it. No harm done; I created new ones.

## Fixes (CLI/SDK)

- **#2 (`scp --api -r`, `snapshot pull`)**: `extractTar` (`internal/cli/transfer.go`) no longer trusts the archive. An entry that names the destination root (`.`, `./`, the stripped folder) is ignored when it is a directory and refused (`unsafe path in archive`) for anything else. `checkNoSymlinkParents` now `Lstat`s `dest` itself and every folder below it, so a symlink made by an earlier entry or a replaced destination stops the extraction. A symlink whose target is absolute, climbs out of the destination or goes through another symlink is skipped with a warning (`linkInside`). A symlinked `dest` that the user chose is resolved once at the start. `snapshot pull` calls the same function. Tests: `TestExtractTarRootSymlinkRefused`, `TestExtractTarRootDirIgnored`, `TestExtractTarChainedSymlinks`, `TestExtractTarDestReplacedBySymlink`, `TestExtractTarUnsafeLinkTargetsSkipped`, `TestExtractTarSnapshotRootSymlinkRefused` (hand-built tars); the old `TestScpDownloadDirSymlinkParentRefused` now uses an in-tree link, since an absolute link is skipped instead of created. Not done: the report's second suggestion, that the archive should not sit in a folder the sandbox user can write. The extraction is safe with a swapped archive now.
- **#3 (node restart)**: `waitReady` (`internal/cli/helpers.go`), `wait_until_ready` / `wait_until_idle` (`sdk/python/src/crowdvm/helpers.py`) and `waitUntilReady` / `waitUntilIdle` (`sdk/typescript/src/helpers.ts`) treat `state == "error"` with an error starting `node_offline` as passing. They keep waiting until the normal timeout, then fail with that error (`SandboxFailed` in the SDKs, `sandbox_error` in the CLI). Any other error still fails at once. Tests: `TestResumeWaitsThroughNodeOffline`, `TestResumeFailsOnOtherError`, `TestResumeNodeOfflineTimesOut` (CLI); `test_wait_until_ready_waits_out_node_offline`, `test_wait_until_ready_node_offline_fails_at_timeout`, `test_wait_until_ready_other_error_fails_at_once` (Python); `waitUntilReady waits out node_offline`, `waitUntilReady: node_offline fails at the timeout, other errors at once` (TypeScript). The server still sends `sandbox.error` on a short disconnect; that part of the report is server side.
- **#6 (Ctrl-C on `exec`)**: partly fixed. The kill API needs a process id, and only a detached command has one: a sync or stream command returns none, and the guest's `DELETE /commands/{pid}` only finds detached records (`internal/guest/history.go` `byProcessID`). So text mode (the default, and `--stream`) now runs like the `--timeout 0` path: detached, followed, timeout kept on the client, `StopCommand` on Ctrl-C (`runCommand` in `internal/cli/cmd_exec.go`). Test: `TestExecInterruptKillsDefaultTimeoutCommand`. Existing exec tests now fake the attach stream (`attachFrames`). Two costs: the command has no server-side timeout any more, so a CLI killed with SIGKILL leaves it running; and `--json` (sync) and `--stream --json` still cannot kill the command on Ctrl-C, because their frames and `stdoutTruncated` flags come from the sync and stream endpoints. The complete fix is in the guest: kill the process group when a sync or stream request's context ends (`handleCommand` / `streamCommand` in `internal/guest/commands.go`).
