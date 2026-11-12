# Bug hunt r16 (opus): round-15 rechecks, user-facing surfaces with the secure layer on, 2026-11-12

Setup: commit `f80e0d4`, built with `make build` in my own worktree (`/home/tl/dev/CrowdVM-r16opus`, detached at `f80e0d4`). Binaries and all state lived in `~/.local/share/crowdvm-r16opus`. One `crowdvm-server` listened on :9960 with `--public-url http://127.0.0.1:9963`, `--public-domain r16.test`, `--log-level debug` and no start limits. I restarted it several times: once with `--no-guest-channel` (rollback test), then with `--egress-allow 192.168.178.31:9968,192.168.178.31:9967`, and last with `--default-image ubuntu-24.04-crowdvm-v4` (the full image, for the desktop). Two qemu nodes, `r16n1` and `r16n2` (UI on :9961 and :9962), each had 8 vCPU, 20 GiB, 30 GiB of disk (×4 overcommit) and at most 5 sandboxes. Each ran as a transient systemd user unit with the real unit's properties (`KillMode=process`, `NotifyAccess=main`, `FileDescriptorStoreMax=1024`, `FileDescriptorStorePreserve=yes`, `CROWDVM_FDSTORE=1`, `Restart=always`). Images came from `~/.local/share/crowdvm-dev/images/`: `ubuntu-24.04-crowdvm-minimal-v4` on both nodes, and `ubuntu-24.04-crowdvm-v4` (full) on `r16n1`. Node↔server and guest↔server traffic went through a Python token-bucket proxy on :9963 at 40 MB/s per direction. Users: `admin` (owns both nodes) and `bob` (all sandboxes). Other tools on my ports: a Python HTTP server on `192.168.178.31:9967` (a 2 GB file), a TCP sink on `:9968` that accepts and never reads (`SO_RCVBUF 4096`), and headless Chromium (Playwright's build, CDP on :9964) as a user's browser without a CrowdVM session. The CLI ran with `CROWDVM_CONFIG_DIR` per user and, for `ssh-config`, a scratch `HOME`.

Cleanup: see the last section.

## Bugs

### 1. After a type change to a bigger disk, an encrypted sandbox never boots again (data-loss)

Severity: data-loss. Round 15's fix makes `growpart` work in preinit, so the data partition now grows to the end of the disk. The partition then has an odd number of 512-byte sectors, but the LUKS volume uses 4096-byte sectors. `cryptsetup resize` fails on that, and its error is ignored, so the first boot looks fine but `/home` keeps its old size. On every later boot, `cryptsetup open` of the grown partition fails. Preinit reports it as "the disk key was refused" and powers off. `resume` and `recover` both end in `error`, every time and on both nodes. The user's whole disk, including everything written after the type change, can no longer be opened.

The sector count is always odd: for a disk of whole GiB, the last usable GPT sector is `total − 34`. The data partition starts on a multiple of 8, so `growpart` makes it `total − 33 − start` sectors long, which is 7 mod 8.

Where:
- `internal/guest/preinit_linux.go:337` grows the partition to the end of the disk, unaligned. `:339` runs `cryptsetup resize` and throws the error away.
- `internal/guest/preinit_linux.go:318` (`cryptsetup open`): the LUKS2 segment is "dynamic" (to the end of the device), so the device-mapper table gets an unaligned length and the kernel refuses it (`reload ioctl … Invalid argument`). `:320` reports that as "the disk key was refused".
- `:340`: once the partition is grown, `growpart` answers `NOCHANGE` and the resize is never tried again.

Repro:
1. bob: `crowdvm new --type small --name e1 --node r16n1`, write 800 MB plus `~/markA`, then `crowdvm stop e1`.
2. `crowdvm resume e1 --type large --node r16n2`. In `e1`, run `lsblk -b /dev/vda; df -h /home; sudo cryptsetup status crowdvm-data`, then write `~/markB`.
3. `crowdvm stop e1`, then `crowdvm resume e1 --type small --node r16n1` (or `--type large`, or `crowdvm recover e1`).
4. The cause, in any sandbox: a loop device with a GPT, LUKS2 with `--sector-size 4096`, opened. Grow the backing file by 100 MiB + 3584 bytes, then `growpart`, `partx -u`, `cryptsetup resize --key-file=- tdata`.

Observed:
- Step 2: `vda2` is now 117 037 841 920 bytes (228 589 535 sectors), but `crowdvm-data` is still 52 595 523 584 bytes (`102725632 sectors`), and `df /home` shows 48G. `info` reports `diskGB 111`, `type large`. Nothing in `console.log` mentions the failure.
- Step 3: `state error`, `sandbox entered state error: guest powered off`. `console.log`: `crowdvm-preinit: the disk key was refused: cryptsetup open … /dev/vda2 crowdvm-data: exit status 1: device-mapper: reload ioctl on crowdvm-data (252:0) failed: Invalid argument`. The same happened for a resume at `large` and for `recover`.
- Step 4: `growpart=0 partx=0`, the partition is 612 326 sectors, `resize=1`, `Device size is not aligned to requested sector size.`
- The sandbox's pre-upgrade snapshots are still in the chain, but `resume` and `recover` only boot the head.

Expected: the grow keeps the partition a multiple of the LUKS sector size: `growpart`, then shrink the end to a multiple of 8 sectors, or pass `cryptsetup resize --size` an aligned value. Preinit logs a failed `cryptsetup resize` instead of ignoring it. As a repair for disks already grown, preinit can open the volume with an explicit aligned `--size` (or round the partition down) before `open`. Also, "the disk key was refused" should not be used for a device-mapper error.

### 2. Egress flows outlive their sandbox, and each new guest channel gets 256 more: the server's memory grows without bound (leak)

Severity: leak, with crash potential. Round 15 capped a sandbox at 256 egress flows and shrank the window to 1 MiB, and the cap holds inside one channel session. A flow whose destination accepts but never reads is never closed, though. It stays after the guest program exits, after `crowdvm stop`, after `crowdvm delete`, and 20 minutes later. The 256-flow counter belongs to the channel session, so the sandbox's next session (any reboot) starts from 0 and can park 256 more. Each stalled flow holds about 1.5 MiB of server RSS plus about 1.8 MB of kernel send buffer.

Where:
- `internal/mux/mux.go:223–257` (`Join`): the stream→socket copy blocks in `Write` to the non-draining socket and the socket→stream copy blocks in `Read`. Neither returns when the yamux session closes, and the idle timer starts only after a half-close.
- `internal/server/guest_egress.go:217–218`: `flows` is a local of `guestStreams`, created per channel session. `:238` checks it, and `:257` calls `mux.Join` with no deadline and no link to the session's life.

Repro:
1. Server with `--egress-allow 192.168.178.31:9968`, plus a sink on :9968 that accepts and never reads.
2. Four small sandboxes `h1`–`h4` (two per node). In each: `python3 hold.py 90 300`, which opens 300 connections to `192.168.178.31:9968` and writes 64 KiB chunks for 90 s.
3. `ss -tnp state established "dport = :9968"` for the server's pid, its `VmRSS`, and `/healthz`.
4. `crowdvm stop h4`, then `crowdvm delete h4`, then wait 60 s.
5. `sudo systemctl reboot` in `h1`. After it is ready, run `hold.py 40 300` again.

Observed:
- Step 2: all 300 connects succeeded in each guest (the guest's side accepts first). Each guest sent ~0.92 GB. Server RSS went 124 MB → 1.60 GB (13:27:37–13:28:52) and stayed there. `/healthz` 200 throughout.
- Step 3: the server holds exactly 1024 flows (4 × 256, so the cap works) with 1 835 008 000 bytes in their send queues, after all four guest programs had exited.
- Step 4: still 1024 flows and 1.66 GB RSS with `h4` deleted.
- Step 5: 1280 flows, RSS 1.80 GB. At 13:45:46 (18 minutes after step 2) it was still 1280 flows and 1.68 GB. Only the server restart freed them. Restarting just the guest agent did not add flows, but a reboot does.

Expected: a flow ends with its channel session. `guestStreams` can close every hijacked conn and its egress socket when the session ends (track them, or `context.AfterFunc` on the session's close). Then a stalled destination costs at most what one live session may hold. A write deadline (or `SO_SNDBUF` cap plus idle timeout) on the egress socket also bounds a stalled flow within a live session.

### 3. A public desktop URL (`crowdvm desktop --public`) sends every visitor to the sign-in page (wrong-result)

Severity: wrong-result. `--public` is documented as "allow access without a signed-in session" (and `docs/guide/security.md:100`: "Public desktop … until the sandbox stops"). The server side works: an anonymous websocket to `/api/v1/sandboxes/{id}/desktop/ws` gets `RFB 003.008`. But the URL the CLI prints for a public desktop has no token, and the viewer page sends any visitor without a token and without a session to `/login`. The feature cannot be used by anyone who would need it.

Where: `web/js/pages/desktop.js:19` (`if (!token && !ctx.session.me) return ctx.nav('/login?next=…')`). It does not ask the server whether the desktop is public.

Repro:
1. `crowdvm desktop d1 --public --no-open` → `{"desktopUrl":"http://127.0.0.1:9963/desktop/bx_ayjubhju", …}` (no token).
2. Open it in a browser without a CrowdVM session (headless Chromium).
3. Raw check: `new WebSocket("ws://127.0.0.1:9963/api/v1/sandboxes/bx_ayjubhju/desktop/ws", ["binary"])` from Node.

Observed: step 2 shows "Sign in to CrowdVM … Username Password". Step 3: `open`, then `RFB 003.008`.

Expected: the page connects without a session when no token is given, and falls back to sign-in only if the websocket answers 401. Or the API returns a public viewer URL the page can tell apart (for example `#public=1`).

### 4. `crowdvm exec` silently drops piped stdin (wrong-result)

Severity: wrong-result, low. `printf 'line1\nline2\n' | crowdvm exec h3 -- 'cat; echo EOF-seen'` prints only `EOF-seen` and exits 0. `crowdvm exec h3 -- sha256sum < file` prints the hash of empty input (`e3b0c442…`). A script that does `cat secrets.env | crowdvm exec id 'cat > .env'` writes an empty file and reports success. Neither `exec --help` nor the guide says that stdin is not passed. `crowdvm ssh ID -- cmd` does pass it (same file, correct hash).

Where: `internal/cli/cmd_exec.go:85` builds the `CommandRequest` without stdin, and `a.In` is never read in `exec`. The API has no stdin field.

Expected: either pass stdin (the guest runs the command with a pipe, or the CLI switches to the ssh path when stdin is not a terminal), or refuse with a clear error when stdin is a pipe or file with data, and say so in `exec --help`.

### 5. The `ssh-config` entry breaks after every resume with a man-in-the-middle warning (wrong-result)

Severity: wrong-result, low. By design (SSH-005) a sandbox gets a new host key on every create, fork and resume, and `crowdvm ssh` re-pins it through the API each time. `crowdvm ssh-config` writes a static entry (`StrictHostKeyChecking yes`, the CLI's `known_hosts`) and pitches it for VS Code, `rsync` and plain `ssh <id>`. After the next stop and resume, every one of those fails with "REMOTE HOST IDENTIFICATION HAS CHANGED! … IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!" until the user runs a `crowdvm` ssh command again. The guide does not mention it. Users who learn to dismiss that warning lose the protection the pin was for.

Where: `internal/cli/cmd_access.go:218` (`ssh-config` writes the pin once, via `sshOptions`), `internal/guest/boot.go:91–93` (new key on resume).

Repro: `crowdvm ssh-config h3`, `Include crowdvm/*.conf`, `ssh bx_pyxxr6h3` works. Then `crowdvm stop h3`, `crowdvm resume h3 --node r16n1`, `ssh bx_pyxxr6h3` → rc 255 with the warning above. `crowdvm ssh h3` works and rewrites `known_hosts` (`AAAAIP/v6…` → `AAAAIBz+…`), after which plain `ssh` works again.

Expected: the ProxyCommand refreshes the pin before it connects (for example `crowdvm tunnel --pin %h 22` writes the API's current key to `known_hosts`, then tunnels). Otherwise `ssh-config`'s help and the guide should say to run it again after each resume.

### 6. `crowdvm run` reports `exit 0` and "Remove it with: crowdvm delete" when it was interrupted or failed before the command (cosmetic)

Severity: cosmetic. Ctrl-C while `crowdvm run` waits for the sandbox prints `stopped waiting; bx_8ggqzzmp is still starting. Remove it with: crowdvm delete bx_8ggqzzmp --yes`, then `exit 0 · 1s · small · 0.0000 credits · bx_8ggqzzmp deleted`, then `interrupted`, and the CLI exits 130. With `--json`, the summary line is `{"event":"summary","exitCode":0,"deleted":true,…}` before the `interrupted` error. An agent that reads the summary sees exit code 0. The sandbox is in fact deleted (no VM left on the node).

Where: `internal/cli/cmd_exec.go:458` (`exitCode: code` is 0 whenever `runErr != nil`), `internal/cli/helpers.go:116` (the hint, also printed when `run` deletes the sandbox itself).

Expected: the summary carries no exit code (or `null`) when the command never ran, and `waitReady` does not print the delete hint for `run`.

### 7. `desktopAvailable` is true for sandboxes whose image has no desktop (cosmetic)

Severity: cosmetic. Every running sandbox on the minimal image reports `"desktopAvailable": true` in the API, the SDKs and `--json`. `crowdvm desktop` then answers 409 `desktop_not_installed` ("TigerVNC Xvnc not found"), and `crowdvm browser` answers 409 `browser_not_installed`. A Boat-compatible client that uses the field to show a desktop button shows a button that fails.

Where: `internal/server/sandboxes.go:335` (`"desktopAvailable": sb.Usable()`).

Expected: false when the sandbox's image has no desktop (the image's tool list names `tigervnc`, or the guest reports it).

### 8. The desktop page's Reconnect can never work on a CLI link, and its message blames the 10-minute expiry (cosmetic)

Severity: cosmetic. A `crowdvm desktop` URL's token is single-use. When the session drops (or the user clicks Reconnect), a viewer without a dashboard session sends the spent token again. That fails at once, and the page says "Could not connect. Desktop links are valid for 10 minutes and stop working when the sandbox stops." It also offers a big Reconnect button that fails the same way. I clicked Reconnect 10 s after opening a fresh link: `connected` → `failed`.

Where: `web/js/vnc.js:95` (`renew` only when signed in, so the spent token is reused) and `:109–110` (the message and button).

Expected: when the token is spent and the viewer cannot renew, say "This link was used; run `crowdvm desktop ID` for a new one" and drop the Reconnect button. Or let a CLI link renew once through a short-lived refresh token.

## Round-15 fixes rechecked live

| r15 item | Result |
|---|---|
| Bigger disk never grows (opus 2) | The partition grows now (`vda2` 49 G → 117 G), but LUKS and ext4 do not, and the next boot fails: bug 1. |
| Type change to a smaller type | By code the disk keeps the parent's size, and live the qcow2 stayed 113 GiB under `--type small`. The boot failed because of bug 1, not because of the shrink. I did not run a shrink on a disk that had never grown. |
| Plain disk turned "encrypted" (opus 4) | Holds. A `--no-guest-channel` sandbox (`p1`, `vda1 52G /`, `/etc/machine-id` set) resumed as `large` on `r16n2` under the channel server stays plain: no `vda2`, no `crowdvm-data`, `info` → `{"diskGB":111,"encrypted":false}`, `plain.txt` readable. The agent grew the root to 112G (`EXT4-fs (vda1): resized filesystem to 29359867`) a few seconds after ready. |
| `recover` of a crashed channel sandbox (sonnet 1) | Holds. `e2` (encrypted, `r16n2`): `echo E > ~/markE; sync; sysrq b` → `state error` (`403 unannounced_reboot … crowdvm recover starts it again`). `crowdvm recover e2` → `ready` in ~15 s with `markE` present and uptime 0 min. `recover` of `h2` after the reboot guard tripped also worked. |
| Egress memory bound (opus 3) | Within one channel session it holds: 4 sandboxes × 300 attempts gave exactly 1024 server-side flows. RSS reached 1.60 GB for those 1024 flows (about 1.5 MiB each against the 1 MiB window), plus 1.8 GB of kernel send queue. The flows never end: bug 2. Throughput is fine: 2 GB from the LAN HTTP server into `p1` in 53.7 s (= the 40 MB/s proxy limit), sha256 OK. 2 GB out of `p1`: `crowdvm scp --api` 53 s, system `scp` through the tunnel 84 s, both sha256 OK, server RSS 121 MB after. |
| fd store after many reboots (sonnet 3) | Holds. `r16n1`'s `NFileDescriptorStore` stayed at 6 (two VMs × 3) through 7 announced reboots of `h2` 70 s apart, and 2 more before the guard tripped. It went to 3 when the VM stopped in `error`, and to 3 again after `h1`/`h2` were deleted and `d1` started. |
| Warm pool after a rollback (opus 5) | Holds. `warm-pool 1` on `r16n1` under `--no-guest-channel`: `p1`'s create saved `warm.json` (small shape) and the pool booted `warm_q4fur8t6h5cy`. 25 s after the server came back with the channel, the VM and `warm.json` were gone. After `systemctl --user restart` of the node, no pool VM and no `warm.json`. |

## New ground: what held

- **`exec` streaming and exit codes:** `exit 42` → 42, `kill -9 $$` → 137, `kill -TERM $$` → 143. 100 MB of base64 output complete and byte-exact in text mode, `--stream`, and `--timeout 0` (background mode, 67 333 345 bytes, ends with the last line, exit 7 passed through). Ctrl-C (SIGINT) and SIGTERM to the CLI, with stdout to a file and on a pty, in `--timeout 30`, `--timeout 0` and `--stream --timeout 100`: exit 130 and the remote `sleep` is gone 2 s later. stdin: bug 4.
- **`run`:** `exit 3` → rc 3, summary `exit 3 · 7s · small · 0.0001 credits · … deleted`. Ctrl-C during the command → rc 130, sandbox deleted, no VM left on the node. Ctrl-C during create also deletes it, with the wrong summary of bug 6.
- **`ssh`:** `crowdvm ssh h3 'exit 7'` → 7, piped stdin works (`ABC`, a 5 MB file's sha256 matches). The host key from the API matches the guest's `/etc/ssh/ssh_host_ed25519_key.pub` and is pinned with `StrictHostKeyChecking yes`. `ssh-config` writes a 0600 file in `~/.ssh/crowdvm/` and reports `included:false` until the `Include` line exists. After a resume: bug 5.
- **File copy:** system `scp -r` through the `ssh-config` entry: 20 MB binary, NUL/0xFF bytes, 500 small files, a name with a space and `ü`, all identical (scp follows symlinks, as OpenSSH does). `crowdvm scp --api -r` up: same tree, symlinks kept as symlinks (`abslink -> /etc/passwd` is kept as a link, not the host's file), scp's semantics for an existing or new target dir. `--api -r` down: an executable keeps 0755, a relative symlink kept, the absolute one skipped with `warning: skipped 1 symlink(s) pointing outside the destination`, a FIFO ignored. `crowdvm scp NAME:/path` is refused (`exactly one side must be ID:/path`). Only ids match `remoteRe`, as documented.
- **Hosted ports:** the private URL `http://bx-…-8000.r16.test:9963/?_token=…` → 302 + `_port_auth` cookie (HttpOnly, SameSite=Lax), then 200. A 30 MB file through it had the right sha256, and without the cookie the answer was 401. `--public` → 200 without a token, also under `/p/<id>/8000/`. `host hide` → 404 for the bare URL, the token and the cookie. `host` without flags after `--public` makes it private again (401).
- **`tunnel` and `forward`:** an HTTP/1.0 request through `crowdvm tunnel h3 8000` returned the 30 MB file intact. `forward --remote 8000 --local 9966`: the 30 MB file intact, 20 parallel requests all 200, and one SIGINT or SIGTERM ends it with rc 0.
- **Desktop and browser (full image, encrypted `d1`):** the `crowdvm desktop` link connects in headless Chromium with no CrowdVM session (XFCE desktop rendered, RFB over `/desktop/ws` with the token as a subprotocol, token removed from the address bar). `crowdvm browser` streams Chrome alone (Google's start page loaded through server egress). Public and reconnect: bugs 3 and 8. Minimal image: bug 7.
- **TypeScript SDK (built from `sdk/typescript`) and Python SDK (`sdk/python/src`), against :9960 with an API key:** `get`, `command` (exit 3, stdout/stderr split), `streamCommand` (frames in order, exit 5), `runCommand` (`started` with a process id, output, `exit` with `signal: SIGTERM` or code 9), `writeText`/`readText` with `ü ñ` in the path, base64 `writeFile`/`readFile` of 300 kB random bytes (identical). Signed upload and download URLs moved 64 MiB (TS, 1.6 s each way) and 32 MiB (Python) with matching sha256. Also `listFiles`, `host_port(public=True)` and `processes`.

## Not verified

- Direct ports (`crowdvm direct-ports`, `--direct-host`): the node publishes ssh on a random host port, outside the ports I was allowed to use.
- A type change of an encrypted sandbox whose partition was never grown, to a smaller type, as its own case (bug 1 got in the way). By code the disk keeps the parent's size.
- Whether the pre-upgrade snapshots of a sandbox hit by bug 1 can be restored (a fork or `--from` of an older snapshot id). `resume` and `recover` only use the head.
- Bug 2 with a real internet sink and over hours. My sink was an `--egress-allow` LAN host, and I watched it for 18 minutes. Repeated reboots past 1280 flows were not run.
- WebSockets through a hosted port, `host --localhost`, and an org sandbox's `ssh-config` with `--org` or `CROWDVM_PROFILE` (the ProxyCommand carries `--profile` only when given as a flag).
- The public desktop after a stop/resume (by code `publicBoot` is tied to the guest token, so it ends with the boot).
- The dashboard's desktop button and the noVNC paste/keyboard tools.

## Cleanup

Deleted every sandbox (`d1`, `h3`, `p1`, `e2`, `e1`, plus `h1`, `h2`, `h4` and the `run` sandboxes earlier). `crowdvm list --all` was empty, the server DB had 0 live sandboxes, and both nodes' `vm/vms` were empty. I stopped both node units (the transient units unloaded, which drops their fd stores), the server, the 40 MB/s proxy, the TCP sink, the HTTP server and Chromium. Nothing of mine listens on 9960–9969, no process matches `crowdvm-r16opus`, no qemu of mine runs, and no `r16opus-*` unit is loaded. `~/.local/share/crowdvm-r16opus` (binaries, node and server dirs, images, logs, the scratch `HOME`) is deleted. The worktree `/home/tl/dev/CrowdVM-r16opus` is removed after this report was copied to `docs/reviews/bugs-r16-opus.md` in the shared checkout (untracked). I touched nothing else there, and did not touch `crowdvm-field`, the e2e clusters, other worktrees or `~/.npmrc`.

## Fixes

7 of 8 fixed. Tests: `internal/guest/fix_r15_test.go` `TestR16AlignPartition` (1), `internal/server/fix_r14_test.go` `TestR16EgressFlowEndsWithItsSession` (2), `internal/cli/fix_r16_test.go` (4, 5, 6). The web changes (3, 8) are checked by reading only (`web/` has no tests).

1. **Grown encrypted disk never boots again** (`preinit_linux.go` `growData`, `alignPartition`): the partition is grown before the LUKS volume is opened and its size rounded down to a multiple of 8 sectors, so the dynamic LUKS2 segment opens at the new size (no `cryptsetup resize`). The alignment runs before every open, which also repairs a partition an earlier grow left unaligned. An open failure no longer says "the disk key was refused".
2. **Egress flows outlive the session** (`guest_egress.go`): each flow ends with its channel session (both conns closed, the destination reset with `SO_LINGER 0`, which also frees its kernel send queue).
3. **Public desktop URL** (`web/js/pages/desktop.js`, `vnc.js`): without a token or session the viewer connects anonymously and goes to sign-in only when the websocket closes before RFB.
4. **`exec` drops stdin** (`cmd_exec.go` `warnStdin`): a pipe or non-empty file on stdin gives a warning that points to `crowdvm ssh ID -- CMD`; `exec --help` says so.
5. **ssh-config after a resume** (`cmd_access.go`): `crowdvm tunnel --pin` rewrites the host's known_hosts line with the current key before relaying, and the `ssh-config` ProxyCommand uses it.
6. **`run` hints** (`helpers.go`, `cmd_exec.go`): no "Remove it with" hint when `run` deletes the sandbox itself; `exitCode` is `null` when the run failed (bugs-r16-sonnet 7).
8. **Reconnect on a spent link** (`vnc.js`): a link-only viewer says the link was used or expired and offers no Reconnect.

Not fixed: 7 (`desktopAvailable` on images without a desktop): the server does not know which images have one; it needs the guest or the image catalog to report it.
