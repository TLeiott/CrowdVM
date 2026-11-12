# Bug hunt r13 (opus): guest files, warm pool, credentials, round-12 rechecks, 2026-11-12

Setup: `make build` at 07:22 (commit `dde46aa`). I copied the binaries to my own data dir (`~/.local/share/crowdvm-r13opus`), so later rebuilds could not change them mid-run. One `crowdvm-server` listened on 127.0.0.1:9960, with `--public-url http://127.0.0.1:9963`, `--public-domain r13.test` and `--log-level debug`. Two qemu nodes, `r13n1` and `r13n2`, had their local pages on 127.0.0.1:9961 and :9962. Each started with 6 vCPU, 14 GiB, 40 GiB and at most 4 sandboxes (changed during the tests), plus `ubuntu-24.04-crowdvm-v3` and `-minimal-v3` from `crowdvm-node image import`. Node↔server and guest↔server traffic went through a Python token-bucket TCP proxy on 127.0.0.1:9963 (40 MB/s per direction). Users were `admin` (server admin, owner of both nodes) and `bob`, each with their own CLI config dir. Headless Chromium (Playwright's `chrome-headless-shell`, CDP on 9964) played the node owner's browser. A Python listener that logs request headers and answers 200 `{"applied":true}` played "another local process" on a node's UI port, and on 127.0.0.1:9967 as an egress target.

Cleanup: every sandbox is deleted. The server DB showed 0 live sandboxes, 0 live snapshot rows and 0 saved snapshots (`bobstack`, `bobwarm` and their share codes removed) before I stopped it. Both nodes' `vms.json` were `{}`. The server, both nodes, the proxy, the listeners and Chromium are stopped, no qemu of mine is left, and nothing listens on 9960–9969. I started no systemd user units on the host; the units inside sandboxes (`tick`, `rsswatch`, `rb1`) died with them. My data dir and scratch files are deleted. `bin/` from `make build` is left in the repo, as in earlier rounds.

## Bugs

### 1. A warm-pool sandbox keeps its full config, secrets included, in `/var/lib/crowdvm/adopted.json`, and snapshots, saves and share codes pass it on (security)

Severity: security. Anyone who deploys a share code (another account) reads the sharer's environment: the live per-sandbox API key (exec in the sharer's sandbox), `GH_TOKEN`, every env secret, the AI tokens and the guest bearer token. `--no-env` on the deploy does not help: the file is on the disk.

Where:
- `internal/guest/warm.go:82`: adopt writes the sandbox's whole `GuestConfig` (env, token, repos) to `<data dir>/adopted.json`, which lives on the VM disk.
- Nothing ever deletes it. `cmd/crowdvm-guest/main.go:53` only reads it when the config drive says `warm`.
- The credential scrubbing for shared snapshots (`creds.go`, `--no-env`) covers `~/.config/crowdvm`, `~/.claude/.credentials.json` and the git credentials, not this file.

Repro:
1. `crowdvm-node --data-dir n2 config set warm-pool 1`, restart r13n2, and wait for `warm pool: vm ready`.
2. bob: `crowdvm new --type small -e GH_TOKEN=ghp_BOBWARM999 -e MYSECRET=bob-warm-secret`. The node logs `warm pool: sandbox adopted a pool vm`.
3. bob: `crowdvm snapshot <id> bobwarm`, then `crowdvm snapshot share bobwarm`.
4. admin: `crowdvm new --from <share code> --type small --no-env`, then in it `sudo cat /var/lib/crowdvm/adopted.json`.
5. From the host, with the key from that file: `POST /api/v1/sandboxes/<bob's id>/commands {"command":"echo pwned-by-leaked-key; whoami"}`.

Observed:
- admin's sandbox has no `GH_TOKEN` in its env, but `adopted.json` holds bob's sandbox id, guest token and env: `CROWDVM_API_KEY=cvm_eb93…`, `CROWDVM_TOKEN`, `GH_TOKEN=ghp_BOBWARM999`, `MYSECRET=bob-warm-secret`.
- The leaked key ran the command in bob's sandbox (`pwned-by-leaked-key`, `user`) and listed bob's sandboxes.
- The same path, with the same share, but from a sandbox that was not adopted from the pool (`bobstack`): no secret reached admin. So only warm-pool sandboxes leak.

Expected: the agent deletes `adopted.json` once it has started as the sandbox (or keeps it on tmpfs under `/run`), and the env never lands on the disk.

### 2. `crowdvm-node run`, `join`, `bench --local` (and `image` commands without a verified daemon) kill every running VM of a live data dir (data-loss)

Severity: data-loss. The round-12 lock does not protect the VMs, because these commands adopt the VMs before any lock. Each running qemu is killed after 10 s, the sandboxes go to `error`, and their processes and recent writes are lost. A second `crowdvm-node run` in a terminal while the service runs (the case round 12 fixed) now kills all sandboxes before it prints "another crowdvm-node daemon is running".

Where:
- `cmd/crowdvm-node/main.go:219` (`run`), `:155` (`join`), `:394` (`bench --local`, and `bench` whenever the daemon does not answer), and `image.go:63/110/149/204` (`image import/list/pull/push` when no verified daemon answers) call `newDriver`. `qemu.New` runs `adoptAll`.
- `internal/node/vm/qemu/qemu.go:1338`: the daemon already holds each VM's QMP socket, so the second process cannot connect, takes the VM for hung after 10 s, and `p.Kill()`s it.
- `internal/node/node.go:222` takes `node.lock` in `Run`, after the driver was created (its comment says "before adopting VMs", but `newDriver` has adopted them by then). No other subcommand checks the lock.

Repro:
1. bob's `f1` on r13n1 runs `while true; do date >> ~/tick.log; sync; sleep 1; done`.
2. `crowdvm-node --data-dir n1 bench --local --quick` (07:39:13).
3. Again later, with `f1` resumed: `crowdvm-node --data-dir n1 run` while the daemon runs (07:40:05).
4. On r13n2, with three sandboxes running: `crowdvm-node --data-dir n2 join --server … --token <new token> --skip-bench`.

Observed:
- 2: the bench finished at 07:39:25. r13n1 logged `qemu: bx_tktjzpfx: qemu exited unexpectedly` at 07:39:24, and the sandbox went to `error`. After `crowdvm resume`, `uptime` was 0 min and `tick.log` ended at 06:39:17 UTC, 7 s before the kill.
- 3: the second `run` printed `error: another crowdvm-node daemon is running` at 07:40:15. At 07:40:15.849 the daemon logged `state=error … qemu exited unexpectedly` for `f1`.
- 4: the join succeeded (same node id, new node token in `config.json`). All three qemu processes on r13n2 died, at 07:38:02, :12 and :22 (10 s apart).

Expected: every subcommand that opens the driver takes `node.lock` first (and refuses, or talks to the daemon, when it is held). `run` takes it before `newDriver`. A process that could not dial a VM's QMP socket never kills it, unless it holds the lock.

### 3. An evacuation whose node dies before the stops finish resumes the sandboxes from an empty disk, although the server holds their newer snapshot (data-loss)

Severity: data-loss. This is a variant round 12 did not cover: there, the stops had finished. Here the node died while the evacuation stopped the sandboxes.

Where:
- `internal/server/nodes_evacuate.go:301`: `evacuationResume` boots with `Parent: sb.HeadSnapshotID`. After the 3-minute offline wait, the failed stop leaves the head at the last stored snapshot, and for these sandboxes there was none, so they boot from the base image.
- The stop's own snapshot row (`snap_7bez…`, `snap_ey5m…`, 848 MB each, `node_id` = r13n1) exists. When r13n1 returns, the evacuation asks for and stores exactly those layers. Nothing sets them as head or boots from them (`failedEvacMoves` only picks sandboxes in `error snapshot_unavailable`, and these were `ready`).
- The same `Exclude: from` keeps a waiting move off the evacuated node even after the evacuation ended and the node was undrained (see bug 10).

Repro:
1. bob: `c2`, `c3` (unpinned, small) on r13n1, each with an 800 MB random file and its `sha256sum` (07:43:3x).
2. `crowdvm nodes evacuate r13n1 --grace 0` (07:43:52). As soon as r13n1 logs its first `transfer started dir=upload`, `kill -9` the r13n1 daemon (07:43:53).
3. Wait. Start r13n1 again at 07:47:40. `crowdvm nodes undrain r13n1`, then make room on r13n2.

Observed:
- 07:43:55 `stop_failing` notices. 07:47:09: `resuming evacuated sandbox on another node` for both, and `c3` was `ready` on r13n2 with no `~/big.bin`. Its `head_snapshot_id` was empty, and `snapshot latest` showed only a fresh 1 MiB snapshot from after the move.
- 07:48:16: r13n1 is back. The server asked it for `snap_7bez…` and `snap_ey5m…`, and both were uploaded (`uploaded_at` set) by 07:49.
- `c2` waited in `provisioning` until I made room on r13n2 at 07:55:01, then booted there (`uptime` 0 min) without `~/big.bin`, although its 848 MB snapshot had been stored on the server for 6 minutes.
- Side note: r13n1's three qemu processes kept running unattended for those 4 minutes, while `c3` also ran on r13n2. Their network was gone with the node process, so nothing got out.

Expected: a move whose source still holds a newer recorded snapshot waits for it, like the round-12 fix does for `error snapshot_unavailable`. Or, once that layer is stored, the sandbox gets it as head before (or instead of) a fresh boot. At least the owner should get a notice that the disk was reset.

### 4. Text search over a large single-line file makes the guest agent eat all memory, and the kernel panics and reboots the VM (crash)

Severity: crash. Every process in the sandbox dies, and unsynced work is lost. Anyone with `file.read` can trigger it with one `GET`, for example an agent or the dashboard searching a folder that holds a large minified bundle, a JSON dump or a log without newlines.

Where:
- `internal/guest/files.go:447`: `handleFileFind` reads each line with `br.ReadString('\n')`, unbounded. A 3 GB line is read into memory whole. Only the first 8000 bytes are checked for binary content.
- `internal/guest/guest.go:248`: the agent sets its `oom_score_adj` to -1000. The OOM killer kills everything else, then finds nothing it may kill, and the kernel panics (`kernel.panic = -1`, so it reboots at once).

Repro:
1. In bob's `f1` (4 GB): `mkdir ~/big && head -c 3000000000 /dev/zero | tr '\0' a > ~/big/oneline.txt`.
2. `GET /api/v1/sandboxes/<id>/files/find?path=/home/user/big&pattern=zzz`.
3. For evidence, a root unit appends the agent's `VmRSS` and `MemAvailable` to `~/rss.log` every 0.3 s with `sync`.

Observed:
- `rss.log`: agent RSS 36 MB at 06:30:27, then 1.0, 1.4, 1.8, 2.1, 2.5, 2.8, 3.3 and 3.7 GB by 06:30:34, with `MemAvailable` falling to 50 MB. Then nothing more.
- `uptime` after: 0 min, and `journalctl --list-boots` shows a new boot. The node logged `state=starting`, then `guest healthy 4m31s after launch`.
- The API call never answered (see bug 6). Same result on the first try at 06:27.

Expected: cap the line length (skip or cut longer lines, as `cutLine` already does for output) and bound the reader. More generally, an unkillable agent should cap its own memory, for example with a cgroup `MemoryMax`, so that a bug in it cannot panic the guest.

### 5. The node page sends its session to whatever listens on the node's UI port while the daemon is down (security)

Severity: security, low. This is the round-12 bug 2 threat model (another local user binds the port while the daemon is stopped or restarting), now aimed at the page instead of the CLI. The CLI got the hello proof, but the page did not. With the stolen session, the attacker changes the live settings, pauses or restarts the node, and starts benchmarks, until the owner logs out.

Where: `internal/node/ui.html:462` and `:470`: the open page polls `GET /api/status` every 2 s with `X-Crowdvm-Ui-Session` from `localStorage`, without checking that the listener is the daemon. `internal/node/ui.go:230` accepts that value from anyone. A page served by the squatter on the same origin can also read `localStorage` directly.

Repro:
1. `crowdvm-node --data-dir n1 open --print`, and open the link in Chromium. `ui-sessions` gains a nonce, and the tab stays open.
2. Stop the r13n1 daemon (SIGTERM, as a restart does). Start the header logger on 127.0.0.1:9961.
3. After 5 s, stop the logger and start the daemon. With the logged value: `curl -XPOST -H 'X-Crowdvm-Ui-Session: <value>' -H 'X-Crowdvm-Ui: 1' http://127.0.0.1:9961/api/config -d '{"key":"max-sandboxes","value":"3"}'`.

Observed:
- The logger got `X-Crowdvm-Ui-Session: a3a1accd…9201fc05…` twice in 5 s.
- The curl got `{"applied":true,"key":"max-sandboxes","restartNeeded":false}`. The same request without the header got 401.
- `POST /api/logout` with that session ended it (a later `/api/pause` got 401), so the owner can cut it off once they notice.

Expected: the page does not send a bearer-like value before the listener has proven itself. For example, it checks `/api/hello` against a key only the page and the daemon share, or it signs each request with a session key (an HMAC of a server nonce) instead of sending the key.

### 6. A guest call in flight when the VM reboots never answers (stuck)

Severity: stuck. The client waits until the node process restarts, which can be days. The dashboard and CLI calls hang the same way.

Where: `internal/node/vm/netstack/netstack.go:513`/`:535`: `Forward` splices the node-side connection to a guest TCP connection with no keepalive. When the guest kernel restarts, nothing ever sends a packet that could draw a RST. `internal/node/api.go:176` (the guest reverse proxy) and the server's `ProxyToGuest` have no timeout, and the node does not close a VM's guest connections when it sees the VM restart (`vm state starting`).

Repro: in bob's `f1`, start `POST /commands {"command":"sleep 20; echo done","timeoutSeconds":600}` (curl `--max-time 3600`). Two seconds later, `sudo systemd-run --unit rb1 bash -c "sleep 3; echo b > /proc/sysrq-trigger"`.

Observed: started at 07:34:45. The VM rebooted (`uptime` 0 min at 07:35:11). The call answered only at 07:36:56, with `502 sandbox_direct_failed … EOF`, when I stopped the r13n1 daemon for another test. Bug 4's `find` call and an earlier `sleep 100` call also hung until curl timed out (120 s, 150 s).

Expected: when the node sees the VM or its agent restart, it closes that VM's guest connections, and the netstack enables TCP keepalive on guest connections.

### 7. Reading a FIFO through the file API hangs, and each try leaks an agent thread forever (leak)

Severity: leak. The calls hang until the client gives up. In the agent, each one keeps a goroutine and an OS thread blocked in `open()` until someone opens the FIFO for writing. Go stops at 10 000 threads, so enough calls would crash the agent.

Where:
- `internal/guest/files.go:174` (`GET /files`), `:670` (`replace`), `:779` (`files/raw`, which serves signed download URLs) open or read the path before checking that it is a regular file. `os.Stat` says FIFO (size 0, not a directory), and opening it for reading blocks.
- The same pattern exists in `playbook.go` `prefetchFile` (opens, then checks `IsRegular`), from reading only.

Repro:
1. In bob's sandbox: `mkfifo ~/fifo`.
2. 100 × `GET /files?path=/home/user/fifo` with `--max-time 2`, then `POST /files/download-url {"path":"/home/user/fifo"}` and a GET of that URL.

Observed:
- Every read timed out with 0 bytes. The signed URL got nothing in 10 s.
- The agent's `Threads:` went from 11 to 109, and stayed at 110 thirty seconds after the clients had left.
- `files/info` reports the FIFO as `mode 0644`, `isDir false`, so a client cannot tell it apart from an empty file.

Expected: `Lstat`/`Stat` first, refuse anything that is not a regular file (`400 not_a_regular_file`), and open with `O_NONBLOCK`, so a FIFO swapped in between the checks cannot block either.

### 8. The warm pool keeps its VMs when the owner lowers cpus, memory or max-sandboxes (leak)

Severity: leak. The machine stays more loaded than the owner shares, until a create takes the pool VMs, the node pauses or the daemon restarts. Round 12 suspected this from the code; here it is live.

Where: `internal/node/warm.go:117`: `warmLoop` drops pool VMs only when the shape changed or the pool is larger than `warm-pool`. `warmFitsLocked` limits only adding a VM, not keeping one.

Repro:
1. r13n2 with `warm-pool 1`, two sandboxes and one ready pool VM (2 vCPU / 4 GiB each). `config set cpus 4`, `config set memory 8G`.
2. r13n1 with `warm-pool 2`, no sandboxes, two ready pool VMs. `config set max-sandboxes 1`.

Observed:
- 1: `crowdvm-node status` showed `Usage 4/4 vCPU 8192/8192 MiB`, and the pool VM was still running 22 s later (four ticks). Real use: 6 vCPU and 12 GiB, out of 4 and 8 shared.
- 2: both pool VMs were still running 12 s after `max-sandboxes 1`.
- `config set paused true` removed both within 12 s, as the code intends. A daemon restart also removed them.

Expected: each tick, `warmLoop` drops pool VMs until the real usage plus the pool fits the current capacity.

### 9. A folder download whose files shrink while it streams ends with a cut archive and HTTP 200 (wrong-result)

Severity: wrong-result. The client sees a successful download and gets a broken `.tar.gz`, with the remaining files missing. Folders that change while they are downloaded (logs, build output) hit this.

Where: `internal/guest/files.go:757`: `writeTarGz` copies `io.CopyN(tw, f, fi.Size())`. When the file is now shorter, this fails, and the walk stops. `:706` ignores the error, so the handler returns normally and the chunked response ends cleanly. The same happens when a file is deleted between its header and its `Open` (the header is written, then `return nil`, so the next header fails).

Repro: `~/art` holds `a.bin` (300 MB random) and `z.txt`. Start `curl … /api/v1/sandboxes/<id>/artifacts?path=/home/user/art -o art.tgz`, and 3 s later run `truncate -s 1000000 ~/art/a.bin` in the sandbox.

Observed: `http=200 size=143794770`, `curl exit 0`. `gzip -t art.tgz`: `unexpected end of file`. `tar tzvf`: `Unexpected EOF in archive`, and no `z.txt`.

Expected: pad a shrunk file with zeros and keep going, as GNU tar does (it warns "file shrank"), and skip files that cannot be opened before writing their header. On a real write error, abort the response (`panic(http.ErrAbortHandler)`) so the client sees a failed transfer.

### 10. An evacuated sandbox waiting for room never goes back to its own node, even after the evacuation ended and the node was undrained (stuck)

Severity: stuck, low.

Where: `internal/server/nodes_evacuate.go:301`: the queued boot carries `Exclude: from` for good. The evacuation ended at 07:49 (`node_evacuations` empty), and `crowdvm nodes undrain r13n1` cleared `disabled` at about 07:53, but the pending boot still excluded r13n1.

Repro: bug 3's `c2`, waiting in `provisioning` because r13n2 was full. Undrain r13n1 (6 vCPU free, no sandboxes).

Observed: `c2` stayed `provisioning`, with no node, until 07:55:01, when I deleted a sandbox on r13n2. It then booted on r13n2 at once. Left alone, it would have waited out `CapacityWait` and been stranded.

Expected: drop the exclusion once the evacuation is over or cancelled, or when the node is undrained.

### 11. `crowdvm new --fail-fast` fails with `no_ready_machine` for up to 10 s after an owner pauses a node, although another node has room (wrong-result)

Severity: wrong-result, low.

Where: a local pause (`crowdvm-node config set paused true`, the page, or a schedule edge) reaches the server only with the next heartbeat. `internal/server/lifecycle.go:300` (`requeueBoot`): with `FailFast`, the paused node's 503 aborts the create instead of trying the next node.

Repro: r13n1 empty, r13n2 empty. `crowdvm-node --data-dir n2 config set paused true` (prints `Applied.`), then at once `crowdvm new --type small --fail-fast` (twice).

Observed: both creates were cancelled with `no_ready_machine: the node had no room for this sandbox (… "node is paused by its owner (or outside its schedule)" …)`. `crowdvm nodes` showed r13n2 `paused: False` until 07:42:58, 11 s after the pause. Without `--fail-fast`, a create in the same window took 8 s and landed on r13n1.

Expected: a capacity refusal from one node moves a fail-fast create to the next node that fits (fail only when none does), and the node sends a heartbeat at once when its pause state changes.

### 12. A VM killed from outside reports qemu-img's "Formatting …" lines as its error, with the node's file paths (cosmetic)

Severity: cosmetic. The sandbox owner sees a misleading error, which also names the node owner's home directory.

Where: `internal/node/vm/qemu/qemu.go:1256`: `msg += ": " + logTail(r.log)`. `qemu.log` mostly holds the `qemu-img create` output of the overlays, and a killed qemu writes nothing.

Repro: any of bug 2's kills, then `GET /api/v1/sandboxes/<id>`.

Observed: `error: "qemu exited unexpectedly: size=67645734912 backing_file=/home/tl/.local/share/crowdvm-r13opus/n1/vm/disks/bx_tktjzpfx-0719fa5c.qcow2 … Formatting '/home/tl/.local/share/…/bx_tktjzpfx-efffde64.qcow2', fmt=qcow2 …"`.

Expected: report the exit status or signal (`killed by SIGKILL`). Show the log tail only when qemu wrote it, without the node's paths.

## Round-12 fixes rechecked live

| r12 item | Result |
|---|---|
| Page session header (1) | The cookie is gone (`/login` sets none, only clears the old one), so other ports no longer get the session; the value lives in the port's own `localStorage`. I did not load a page from another port to try. `POST /api/logout` ends the session (the next POST gets 401). `ui-sessions` lists one nonce per login (written 0600 in the code). Gap: the open page sends its session to a listener that took over the same port (bug 5). |
| CLI hello proof (2) | Holds. With the r13n2 daemon stopped and the header logger on :9962: `status` printed `Daemon not running`; `config set cpus 5` printed `Saved. No running node confirmed it, so it applies at the next start.` and `config.json` got `cpus: 5`; `open --print` refused with a clear error; `image list` read the store itself. The logger saw only four `GET /api/hello?nonce=…`, and no request carried the token (`grep` for the token: 0). |
| Data-dir lock (3) | A second `run` exits with `another crowdvm-node daemon is running`. After `kill -9` of the daemon, a new `run` starts at once (the lock does not go stale). But the second `run`, and `join`, `bench --local` and the store-opening `image` commands, kill the running VMs first (bug 2). |
| Evacuation after the node died (4) | Not the same path: here the node died before the stops finished, and the sandboxes came back from an empty disk (bug 3). The stopped-then-uploading case of round 12 was not reproduced again. After the node returned, the evacuation asked for and stored the missing layers, and `node_evacuations` emptied, as the fix says. |
| Negative free capacity (5) | Not rechecked live: I lowered r13n2 below its use (bug 8) but did not read `crowdvm nodes` at that moment. |

## New ground: what held

- **File API paths:** `GET /files` on a symlink to `/etc/shadow`, a symlink to `/proc/self/environ`, `/proc/self/environ` itself and `/home/user/../../etc/shadow` all got 400 `invalid_path`. The four metric files (`/proc/stat` and the others) are readable, as documented. From the code, `files/find` skips FIFOs and symlinks (`d.Type().IsRegular()`).
- **Path tricks on the server route:** `files/%2e%2e/internal/firewall`, `files%2f..%2finternal/firewall` and `commands/%2e%2e/internal/health` got 404. `files/..%2finternal%2ffirewall` got a 307 from the node's mux to `/v1/vms/<id>/guest/internal/firewall` on the server's host, where nothing answers.
- **Egress policy (node netstack):** from a sandbox on r13n2, these were all refused: 127.0.0.1:9967, 0.0.0.0, 127.9.9.9, the host's LAN, Tailscale and libvirt addresses, 10.0.2.2 on any port other than the server's, 169.254.169.254, `[::1]` and `[::ffff:127.0.0.1]`. 10.0.2.2:9963 (the server pinhole) answered 200, and 1.1.1.1:80 answered 301. My listener on 9967 logged nothing. The guest firewall (`firewall.go`) only opens published inbound ports in ufw; it is no outbound control, so there is nothing for the sandbox user to get around there.
- **Credentials on share (without the warm pool):** bob's sandbox with `GH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN` and `MYSECRET` was saved and shared. admin's deploy with `--no-env` had no `~/.config/crowdvm/config.json`, no `~/.claude/.credentials.json` and no git credentials. admin's deploy without `--no-env` got admin's own API key. `managed-creds.json` lives on the disk, so the hashes travel with the snapshot and the next boot replaces the values.
- **Browser-only view isolation (code plus a live look):** Chrome runs as `crowdvm-browser` (uid 999, no `adm`), with `/home/user`, `/tmp`, `/var/lib/crowdvm`, `/run/crowdvm` and `/root` on empty tmpfs and the env file bound to `/dev/null`. A root `grep` for bob's secrets outside those found only `adopted.json` (bug 1, hidden from the browser by the tmpfs) and my own `sudo` lines in `/var/log/auth.log` (mode 0640, group `adm`).
- **Warm pool:** adoption took 105 ms. Pause empties the pool, and a node restart destroys the pool VMs (the adopted one stays as the sandbox) and boots new ones.
- **Agent memory for ordinary calls:** `GET /files` refuses files over 5 MiB, `replace` too, and `artifacts` refuses single files over 50 MiB. The server streams signed uploads and downloads without buffering them.

## Not verified

- The thread-limit crash of bug 7 (10 000 hung calls). I only showed the growth (11 to 110 threads).
- Bug 4 with a folder of millions of files (`files/list` reads every entry before it truncates) or a deep `files/search`. Same root cause (an unkillable agent with no memory cap), not tried.
- Skills (`skills.go`): code-read only (user edits and deletes are kept, older managed files are dropped when unchanged). Not run live.
- Playbooks: the FIFO open in `prefetchFile` (bug 7's pattern) and a 20 000-path playbook from an org member were code-read, not run. `learnPlaybook` only learns from deploys in the saved snapshot's own wallet, which I confirmed in the code.
- Desktop recording and the desktop links. I read `desktop.go` (recording writes as the user under the resolved path); none of it was run.
- Fork and org sharing paths for bug 1. I showed it with a share code; a fork or an org deploy carries the same disk.
- The `--direct-host` ufw rules, `install-service` and the page's Restart button. Installing a user unit would collide with the other cluster's `crowdvm-node` unit on this machine.
- The full `CapacityWait` expiry in bug 10.

## Fixes

11 of 12 fixed; bug 2 by the same change as bugs-r13-sonnet 1. Tests: `internal/server/fix_r13_test.go` (3, 10, 11), `internal/guest/fix_r13_warm_test.go` (1), `internal/guest/fix_r13_test.go` (4, 7, 9), `internal/node/fix_r13_test.go` (5, 8), `internal/node/vm/netstack/fix_r13_test.go` (6), `internal/node/vm/qemu/fix_r13_test.go` (12), `cmd/crowdvm-node/fix_r13_test.go` (2).

1. **`adopted.json` with secrets** (`internal/guest/warm.go` `ClearAdopted`, `cmd/crowdvm-guest/main.go`): any boot that is not from the warm config drive (resume, fork, a deploy of a saved or shared snapshot) deletes it first. The agent comes from the node's config drive, so snapshots taken before the fix are cleaned when they boot.
2. **Commands kill a live node's VMs**: `newDriver` takes the data-dir lock (counted per process) before it builds a driver, so `run`, `join`, `bench` and the `image` fallbacks refuse while a daemon holds the dir; `run` takes it before saving its flags; the lock creates a missing data dir (`join`).
3. **Node dies during an evacuation's stop** (`nodes_evacuate.go` `newerSnapshotOn`): a move whose node holds a snapshot newer than the head it fell back to (taken since its last boot) waits in `error snapshot_unavailable` and boots from that snapshot once the evacuation has it stored. The owner can resume it from the last stored snapshot meanwhile.
4. **`find` over a huge line** (`files.go` `readCappedLine`): a line is matched within its first 64 KiB, the rest is skipped while read; reads of small files are bounded too. The agent's memory cap (cgroup) was not added.
5. **Page session to a squatter** (`ui_session.go` `validRequest`, `ui.html`): the page signs each request (`<nonce>.<ms>.<HMAC(session key, method path ms)>`, ±60 s) and never sends the session key. A page the squatter serves on the same port can still read `localStorage`: narrowed, not closed.
6. **Guest call hangs across a reboot** (`netstack/keepalive.go`, `netstack.go`, `qemu.go`): TCP keepalive on guest connections (~60 s), and the node closes a VM's forwarded connections on a guest reset.
7. **FIFO reads hang** (`files.go` `openFileNB`, `playbook.go`): reads open with `O_NONBLOCK` and refuse anything but a regular file (`400 not_a_regular_file`; per file in `replace`).
8. **Warm pool over lowered limits** (`internal/node/warm.go` `warmOverCapacityLocked`): each tick drops pool VMs until usage plus pool fits.
9. **Cut folder download** (`files.go` `writeTarGz`): files are opened before their header; a shrunk file is zero-padded, a grown one cut; a write error aborts the response.
10. **Exclusion outlives the evacuation** (`lifecycle_place.go`): a queued evacuation boot drops its exclusion once that node's evacuation is over.
11. **failFast after a fresh pause** (`lifecycle.go` `requeueBoot`): a failFast boot a node refuses is placed once more on another node with room before it fails.
12. **qemu-img lines in the error** (`qemu.go` `exitDetail`): the error names the exit status or signal; the log tail drops `Formatting`/`backing_file` lines and absolute paths.
