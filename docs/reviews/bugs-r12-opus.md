# Bug hunt r12 (opus): node owner page, schedules, live limits, evacuation, round-11 rechecks, 2026-11-11

Setup: `make build` at 22:22 (commit `f3fa162`). I copied the binaries to my own data dir (`~/.local/share/crowdvm-r12opus`), so later rebuilds could not change them mid-run. One `crowdvm-server` listened on 127.0.0.1:9960, with `--public-url http://127.0.0.1:9963`, `--public-domain r12.test` and `--log-level debug`. Two qemu nodes, `r12n1` and `r12n2`, had their local pages on 127.0.0.1:9961 and :9962. Each started with 6 vCPU, 14 GiB, 60 GiB and at most 4 sandboxes (changed during the tests), plus `ubuntu-24.04-crowdvm-v3` and `-minimal-v3` from `crowdvm-node image import`. Node↔server and guest↔server traffic went through a Python token-bucket TCP proxy on 127.0.0.1:9963 (40 MB/s per direction). Users were `admin` (server admin, owner of both nodes) and `bob`, each with their own CLI config dir. Headless Chromium (Playwright's `chrome-headless-shell`, driven over CDP on port 9964) played the node owner's browser. Python listeners on 127.0.0.1:9967 (logs request headers) and, when needed, on a node's UI port (answers 200 to everything and logs requests) played "another local process". For the round-11 ssh-config recheck I built a modified guest agent with `go build -overlay` (a patched copy of `internal/guest/internal.go` in my scratch dir; no product file was edited). It returns `/etc/crowdvm-evil/sshuser` and `/etc/crowdvm-evil/hostkey` when they exist.

Cleanup: every sandbox is deleted. The server DB showed 0 live sandboxes, 0 live snapshot rows and 0 saved snapshots before I stopped it, and both nodes' `vms.json` were `{}`. The server, both nodes, the proxy, the two Python listeners and Chromium are stopped, and no qemu of mine is left. I started no systemd user units on the host. The sandboxes' own transient units (`evilagent`, `restore`, `frz`, `stopagent`) died with the sandboxes. My data dir (binaries, DBs, logs, Chromium profile, fake `$HOME`s) and my scratch dir (the overlay build) are deleted.

## Bugs

### 1. The node page's session cookie goes to every port on 127.0.0.1, and whoever gets it can change the node's settings (security)

Severity: security, low. Any other local user or local process that runs a web server on 127.0.0.1 gets the owner's page session as soon as the owner's browser visits it. With that session it can change the live settings (cpus, memory, disk, max-sandboxes, disk-overcommit, disk-reserve, paused, schedule, schedule-hold), start benchmarks, and restart the node when it runs as a service.

Where:
- `internal/node/ui.go:261`: `/login` sets `crowdvm_node_ui` for host `127.0.0.1` (no port; browsers do not isolate cookies by port), valid for 30 days.
- `internal/node/ui.go:237`: that cookie alone gives `accessBrowser`, and `:274` lets it reach `/api/pause`, `/api/config` (all `liveKeys`), `/api/restart` and `/api/bench`.
- `:41` explains that the cookie holds an HMAC of the token, not the token. That keeps the CLI-only keys out of reach, but the value is still a working credential. It is the same for every login and never rotates until `ui-token` is deleted.

Repro:
1. Get a login code with the token (as `crowdvm-node open` does), and open `http://127.0.0.1:9961/login?c=<code>` in Chromium. `/api/status` then says `authorized: true`.
2. In the same browser, visit `http://127.0.0.1:9967/` (any local web server, here my header logger).
3. With the logged value, from a plain shell: `curl -XPOST -H 'Cookie: crowdvm_node_ui=<value>' -H 'X-Crowdvm-Ui: 1' http://127.0.0.1:9961/api/config -d '{"key":"max-sandboxes","value":"3"}'`.

Observed:
- The listener on 9967 logged `Cookie: crowdvm_node_ui=8c9aa586…6bc35afb`.
- The curl got `{"applied":true,"key":"max-sandboxes",…}`, and the node advertised `maxSandboxes: 3`.
- The same request without the cookie got 401. `net.allow-lan` with the cookie got 403 `cli_only`, so the CLI-only keys hold.
- Cross-origin `fetch` from the page on 9967 with the `X-Crowdvm-Ui` header failed (no CORS answer), so the CSRF check itself holds.

Expected: a credential that ports on the same host cannot read. For example, keep the session in `sessionStorage` and send it as a header (other origins cannot read it), or use a `__Host-` cookie on a unique hostname such as `crowdvm-node.localhost`. The session should also be revocable without rotating the CLI token.

### 2. The node CLI sends its full control token to whatever listens on the UI port, and reports changes as applied when nothing saved them (security)

Severity: security. When the node daemon is not running (stopped, restarting, crashed, or the service not yet started after boot), any local user can bind 127.0.0.1:8421 and collect the full control token from the owner's next `crowdvm-node status`, `config set` or `open`. With the token they can change the CLI-only settings: `guest-binary` (every new sandbox on the node then runs their agent), `cli-binary`, `net.allow-lan`/`net.allow` (sandboxes reach the owner's LAN), `server`, plus `/api/images/{id}/import` of any path the node user can read.

Where: `cmd/crowdvm-node/main.go:309`: `daemonBody` adds `Authorization: Bearer <ui-token>` to every request to `uiURL(cfg)`, including the first `GET /api/status` that only checks whether a daemon is up. It never checks that the listener is the node daemon. `config set` then trusts any 2xx and prints `Applied.`.

Repro (r12n1, idle):
1. Stop the r12n1 daemon. Start a listener on 127.0.0.1:9961 that answers 200 with `{"applied":true}` and logs requests.
2. As the owner: `crowdvm-node --data-dir n1 status`, `crowdvm-node --data-dir n1 config set cpus 6`, `crowdvm-node --data-dir n1 open --print`.
3. Stop the listener and start the real daemon. With the logged token: `POST /api/config {"key":"guest-binary","value":"…/attacker-guest"}` and `{"key":"net.allow-lan","value":"true"}`.

Observed:
- The listener logged `Authorization: Bearer 307219fa…28b0a57` on all four requests: `GET /api/status` ×2, `POST /api/config`, `POST /api/login-code`. That is the content of `n1/ui-token`.
- `config set cpus 6` printed `Applied.`, but `n1/config.json` has no `cpus` key: nothing saved it.
- `open --print` printed a login link with an empty code.
- On the real daemon, both stolen-token writes were saved (`"guestBinary": "…/attacker-guest"`, `"allowLan": true` in `config.json`), and they would apply at the next restart. I reverted them.

Expected: do not send the token before the peer has proven it is the daemon. For example, the daemon could listen on a Unix socket in the data dir (mode 0600) for CLI calls, or the CLI could first check a challenge signed with the token. When no daemon answers, `config set` should write `config.json` itself.

### 3. Two daemons can run on one data dir: the second runs without the UI, takes the VMs, and the two keep kicking each other off the server (wrong-result)

Severity: wrong-result. It is easy to hit: `crowdvm-node run` in a terminal while the installed service runs, or a service restart while an old process hangs on. The owner's page and `config set` then talk to the daemon that holds no VMs, and the node's server connection drops every 1 to 30 s.

Where:
- Nothing locks the data dir (`internal/node/node.go:216` `Run`; no `flock` anywhere in `internal/node` or `cmd/crowdvm-node`).
- `cmd/crowdvm-node/main.go:244`: when the UI port is taken, the daemon only logs `status UI disabled` and keeps running.

Repro:
1. With the r12n1 daemon running, start a second `crowdvm-node --data-dir …/n1 run`.
2. (First time, by accident.) Both daemons had been started a second apart. Then I created two sandboxes on r12n1.

Observed:
- The second daemon logs `status UI disabled … bind: address already in use` and stays up. The server log shows `node connected … r12n1` at 23:08:48.581, 23:08:49.503 and 23:08:50.155. Before I noticed, there were 11 `disconnected from server` lines in 4 minutes.
- In the accidental run, both qemu processes were children of the daemon without the UI (pid 1117477). `GET /api/status` on 9961, served by the other daemon, showed `vms: []` and `usage.vms: 0` while the server listed both sandboxes on r12n1. `config set cpus/memory/disk/max-sandboxes` went to the daemon with no VMs.

Expected: the daemon takes an exclusive lock on the data dir (for example `flock` on `<data dir>/lock`) and exits with a clear message when another daemon holds it. A daemon whose UI cannot bind should also say so loudly, because `config set` and `open` depend on it.

### 4. When a node dies mid-evacuation, its evacuated sandboxes stay in `error` after the node comes back and the evacuation ends (stuck)

Severity: stuck, low. No data is lost: the sandboxes resume intact by hand. But the evacuation reports itself finished while the sandboxes it promised to move sit in `error`, and nothing retries them.

Where:
- `internal/server/nodes_evacuate.go:296`: `evacuationResume` queues one boot on another node with the evacuated node's newest layer as parent.
- When that layer is only on the (now offline) evacuated node, the boot fails after `HolderOfflineGrace` (`lifecycle.go:534`, `snapshot_unavailable`), and the sandbox goes to `error`, with its `node_id` set to the target node.
- When the node returns, `evacuateStep` asks it to upload those layers, and at `:230` it deletes the evacuation once nothing is left on the node. Nobody resumes the failed sandboxes.

Repro:
1. bob: `c2` and `c3` (unpinned, small) on r12n1, each with a fresh 800 MB file (`sha256sum` saved). r12n2 has room.
2. `crowdvm nodes evacuate r12n1 --grace 0`. As soon as r12n1 logs `transfer started dir=upload` for the stop's layer (895 MB), `kill -9` the r12n1 daemon.
3. Wait 4 minutes, then start r12n1 again.

Observed:
- 22:40:10: both stops completed and `resuming evacuated sandbox on another node` was logged.
- 22:42:11 and 22:44:12: `sandbox failed … snapshot_unavailable: snapshot snap_… is not on the server yet and the node that holds it is offline`. Both went to `error`.
- After the restart at 22:45:16, r12n1 uploaded all four layers by 22:46:22, and `node_evacuations` became empty. Both sandboxes stayed `error` (checked until 22:48).
- `crowdvm resume` on each brought them up on r12n2 with `big: OK`.

Expected: either keep the evacuation's move pending until the layer is stored (and boot then), or, when an evacuation upload finishes a layer that an `error snapshot_unavailable` sandbox needs, queue its resume again.

### 5. Lowering a node's limits below what runs shows negative free capacity (cosmetic)

Severity: cosmetic.

Where: `internal/server/nodes.go:996` computes `free` as capacity minus reserved, without a floor. `crowdvm nodes` (`internal/cli/cmd_admin.go:736`) prints it as is.

Repro: r12n2 runs 5 small sandboxes. `crowdvm-node config set cpus 4`, `config set max-sandboxes 2`. Then `crowdvm nodes`.

Observed: `FREE -6 vCPU/4 GB of 4 vCPU/24 GB`. `nodes info r12n1 --json` showed `free: {vcpu: -2, memoryMiB: -4096, maxSandboxes: -1, diskGiB: -62}` after an earlier lowering.

Expected: show 0 free (and maybe "over limit"). The behavior itself is right: running sandboxes kept running, and nothing new was placed there.

## Round-11 fixes rechecked live

| r11 item | Result |
|---|---|
| ssh-config injection through `sshUser` / `hostKey` (1) | Holds. The overlay-built agent in bob's `c1` answered `sshkey` with `sshUser = "user\nMatch exec \"id > /tmp/PWNED\"\nHost x\n  User user"`. `crowdvm ssh-config c1 --print` printed `User user`, with no injected lines. With a two-line `hostKey` (`ssh-ed25519 …\n@cert-authority * …`), the server answered 503 `ssh_host_key_unavailable`, and `known_hosts` kept only the legitimate line. I did not get a hostile value past the server, so the CLI's second check was not exercised live. |
| Snapshot loop concurrency (7) | Holds. Four sandboxes on r12n2. `c2` and `d3` wrote and synced every 5 s and were snapshotted every ~70 s (`22:57:17, 22:58:26, 22:59:36, 23:00:46`, …). First I froze `c1`'s root fs (`fsfreeze -f /`, 6 min). That does not stall the agent: Linux `sync` skips frozen filesystems, so `c1` kept being snapshotted too. Then I SIGSTOPped `c1`'s agent for 5.5 min (23:03:50–23:09:20). `c2`/`d3` kept their cadence (`23:04:16, 23:05:26, 23:06:36, 23:07:46`). `c1` turned `degraded` after 3 probes and was skipped, by design (`vms.go:735`, no snapshots for a guest that does not answer). |

## New ground: what held

- **Node page, CSRF and rebinding:**
  - A POST without `X-Crowdvm-Ui` gets 403, and a cross-origin `fetch` with the header fails the preflight.
  - `Host` must be `localhost` or an IP literal.
  - Without the token or cookie, every POST gets 401. With the browser cookie, CLI-only keys get 403 `cli_only`.
  - `/api/status` without login shows status but not the settings.
  - Page values are validated: `cpus 1000` gives `cpus must be 0..16`, and `memory 4096G` gives `memory exceeds host RAM`.
- **Live limits:** with two sandboxes running on r12n1, I set `cpus 2`, `memory 4G`, `disk 10G` and `max-sandboxes 1`. All four applied without a restart. The node advertised the new capacity, and both sandboxes kept running with a 300 MB file intact. Raising them back also applied live.
- **Schedules:**
  - `daily 00:00-22:32` flipped the node to `paused`/`schedule` at 22:32:00 locally, and on the server within 8 s. Both running sandboxes kept running.
  - `schedule-hold offer` lifted the pause and survived a node restart (`until` saved in `config.json`). The page then showed the real next change: tomorrow 22:32, because the hold ends at 00:00 where the window opens anyway.
  - Clearing the schedule ended the pause.
- **Evacuation:**
  - Grace 0, the other node with room: both sandboxes moved with their data intact (`sha256sum -c` OK on 300, 400, 500 and 600 MB files), and a 1 s tick log lost nothing after the stop.
  - Sandboxes created with `--node r12n1` are stranded with a notice, as the code intends (pinned to the evacuated node). I resumed them on r12n2 by hand, with their data intact.
  - The other node full: the evacuated sandboxes waited (`provisioning`, no node). bob's `stop` of one while it waited kept it stopped, and it was not booted when capacity came. The other booted on r12n2 once I raised r12n2's limits, data intact.
  - Grace 45 s: bob's `stop` during the grace kept that sandbox stopped (not moved), and bob's `delete` during the grace worked. I restarted the server right after the deadline's `stopping sandbox … reason=evacuated`. After the restart, the third sandbox finished its stop and moved to r12n2 with its 500 MB file intact.
  - A `--no-snapshots` sandbox was stopped (erased) at a grace-0 deadline, and the node JSON counted it in `noSnapshotSandboxes`.
  - Each evacuation ended only after the node had uploaded every needed layer (`node_evacuations` empty, no un-uploaded layer left on the node).
  - bob cannot evacuate, or cancel the evacuation of, admin's node (403).
- **Evacuation upload throttle:** after the node restart in bug 4, the node uploaded the three missing layers once each, three at a time, in 65 s at 40 MB/s. The repeated requests from later evacuation rounds were no-ops (`RequestUpload` dedups in-flight uploads). Stops do not wait for uploads (each stop completed in 1.5 to 2.5 s).

## Not verified

- The node page's Restart button, and Windows/macOS service behavior. Installing the user unit would have collided with the `crowdvm-node` unit of the other cluster on this machine.
- Bug 1 with a real second Unix user. I used a listener of my own user; browsers send the cookie the same way to any 127.0.0.1 port.
- Bug 2's follow-up impact (a malicious `guest-binary` actually booting in a sandbox). I only showed that the stolen token saves it.
- Schedule behavior across a DST change or a timezone change while the node runs. The code uses wall-clock minutes in `time.Local`, fixed at process start. Not run.
- The full `CapacityWait` (15 min) expiry that strands a waiting evacuated sandbox; I made room before it ran out.
- The warm pool (off by default) under lowered limits. From the code, `warmLoop` drops pool VMs on pause, but not when cpus/memory are lowered below what they hold.
- The CLI-side `sshUser`/`hostKey` check, since the server already refused the payloads.

## Mistakes on my side

- I started both node daemons twice: a command that failed on a later step had already launched them. That is how I first saw bug 3. I then killed the duplicates (the qemu processes survived and were adopted) and reproduced the bug on purpose.
- `zsh` did not word-split `$kv` in a loop, so my first `config set` batch did nothing (`accepts 2 arg(s)`). My first join of r12n2 failed because my `grep` caught the token twice.
- `fsfreeze` did not stall the agent's sync (see the r11 table). I switched to SIGSTOP.

## Fixes

All 5 fixed. Tests: `internal/node/fix_r12_test.go` and `cmd/crowdvm-node/fix_r12_test.go` (1, 2), `internal/node/fix_r12_lock_test.go` (3), `internal/server/fix_r12_test.go` (4, 5).

1. **Page session cookie** (`internal/node/ui.go`, `ui_session.go`, `ui.html`): no cookie any more. `/login` hands the page a session `<nonce>.<hmac>` that it keeps in `localStorage` (per origin, so per port) and sends as `X-Crowdvm-Ui-Session`; nonces are listed in `<data dir>/ui-sessions` (30 days, newest 20), `POST /api/logout` ends one, rotating `ui-token` ends all. The old cookie is cleared. Owners log in once again after the upgrade (`crowdvm-node open`).
2. **CLI token to any listener** (`cmd/crowdvm-node/daemon_hello.go`, `main.go`, `ui.go`): the CLI first sends a nonce to `GET /api/hello` and sends the token only when the answer is `HMAC(token, "crowdvm-node-hello:"+nonce)`. Without a verified daemon, `config set` writes `config.json` itself ("applies at the next start"), and `status`/`open` report the daemon as not running. A daemon from before this change counts as not running until it restarts.
3. **Two daemons on one data dir** (`internal/node/lock*.go`, `node.go` `Run`): an exclusive lock on `<data dir>/node.lock` (flock; LockFileEx on Windows) before VMs are adopted; a second daemon exits with "another crowdvm-node daemon is running".
4. **Evacuation move failed while the node was away** (`nodes_evacuate.go` `failedEvacMoves`): a move remembers its source (`extra.evacuatedFrom`); when the evacuation's layers are all stored, moves left in `error snapshot_unavailable` boot again before the evacuation ends.
5. **Negative free capacity** (`nodes.go`): floored at 0.
