# Bug hunt r8 (opus): live cluster, 2026-11-11

Setup: `make build` at 15:12 local time. I copied the binaries to my own data dir (`~/.local/share/crowdvm-r8opus`), so later rebuilds could not change them mid-run. One `crowdvm-server` ran on 127.0.0.1:9960 with `--trust-proxy`. A second, short-lived server ran on 9969 with `--ip-rate-limit 3` for the rate-limit check. Two qemu nodes, `r8n1` and `r8n2`, had their UIs on 9961 and 9962, each with 6 vCPU, 14 GiB, 40 GiB and at most 3 sandboxes. Both got `ubuntu-24.04-crowdvm-v3` and `-minimal-v3` through `crowdvm-node image import`. Users were `admin`, `alice`, `bob` and `mal`, each CLI with its own HOME. The dashboard flows ran through a small Python client that does what `web/js/api.js` does: cookie session, `X-CrowdVM-CSRF: 1`, `X-CrowdVM-Org`.

Cleanup: all sandboxes are deleted (the admin list is empty). My server, nodes, the transient `r8opus-node2` user unit and every qemu of mine are stopped. The data dir and my scratch files are gone. The `/tmp/crowdvm-*-net.sock` files all predate my run.

## Bugs

### 1. A node installed with `install-service` kills every VM it started whenever the service restarts (data-loss, stuck)

Severity: data-loss and stuck.

Where: `internal/node/service/service_linux.go:41` writes `KillMode=mixed` into the systemd user unit. `docs/guide/node-owners.md:55` tells owners to apply config changes with `systemctl --user restart crowdvm-node`. `Restart=always` also restarts the unit after a crash.

The node is built to survive its own restart. A SIGTERM'd foreground node leaves its qemu processes running, and the next start adopts them (`qemu: adopted running VM ...`). Under the unit this never happens. With `KillMode=mixed`, systemd sends SIGTERM to the node and then SIGKILL to everything left in the unit's cgroup, and the qemu processes are in that cgroup. `Setpgid` (`internal/node/vm/qemu/sys_linux.go:34`) does not move them out.

Repro (the unit settings exactly as `install-service --dry-run` prints them):
1. `systemd-run --user --unit=X -p Type=simple -p Restart=always -p RestartSec=5 -p KillMode=mixed -p TimeoutStopSec=60 crowdvm-node run --data-dir ...`
2. `crowdvm new --type small --node <that node>`, then `crowdvm exec ID 'echo important > ~/data.txt; sync'`.
3. `systemctl --user restart X`.

Observed: the journal shows `r8opus-node2.service: Killing process 526491 (qemu-system-x86) with signal SIGKILL`. The new node logs `bx_pjk33uuh: qemu not running after node restart (overlay kept)`. The sandbox goes to `error`, `machine_error: qemu not running after node restart`, and stays there until the owner runs `crowdvm recover`. My synced file survived. Anything not yet synced, and every running process, is lost. VMs the node had adopted from an earlier foreground run were outside the unit's cgroup and survived. I confirmed the systemd behaviour on its own with a transient unit whose `setsid sleep` child died on `stop`.

Expected: a restart applies the config and the VMs keep running, as they do outside systemd. `KillMode=process`, or starting each qemu in its own scope (`systemd-run --user --scope`), would fix it.

### 2. A node restart during a lazy boot leaves the sandbox in `error node_offline` for good (stuck)

Severity: stuck.

Where:
- `internal/server/lifecycle.go:712-720`: `case proto.VMError` acts only `if sb.starting() || sb.Usable()`. A sandbox in `error` with `node_offline` is neither, so the node's boot-timeout report is dropped.
- `internal/server/nodes.go:820-833` checks for `VMError` only once, when the node connects. At that moment the adopted VM still says `starting`.
- `lifecycle.go:673` (the round-7 fix for the scrub blip) needs `GuestReady`, and that never comes.

Cause: a lazy boot reads the snapshot chain through qemu's curl driver from the node's local HTTP server (`internal/node/lazy.go:66-90`). The node keeps the same port across restarts. While it is down, though, every read fails at once (`curl: Failed to connect to 127.0.0.1:36809 after 0 ms`, 7 and 101 times in two runs). The guest gets I/O errors and never finishes booting. After 3 minutes the node reports `error (boot timeout: guest agent not healthy after 3m0s)` and kills qemu. The server never takes that up. `rebuildLost` does not apply either, because the node is online.

Repro (no scrub involved):
1. `crowdvm snapshot <sandbox on r8n2> lz2`, so that r8n1 lacks the layer.
2. `crowdvm new --from lz2 --node r8n1 --no-wait`.
3. As soon as `qemu-system... crowdvm-<id>` shows up, `kill -9` the r8n1 node. Start it again 5 s later.

Observed: `crowdvm info` shows `error`, `node_offline`, `healthReason node_offline`. Eight minutes later it was unchanged, with `updatedAt` still at the kill, while `crowdvm-node status` showed the same sandbox as `error (boot timeout ...)`. Calls get `sandbox_direct_failed ... being restored (node_offline) ... Retry in a few seconds`.

The same thing happened on a foreign deploy from a share code, which was the round-7 scrub-blip scenario: `securing: true`, `pendingScrub` kept. Every exec there answers `sandbox_securing ... Retry in a few seconds` for as long as it stays stuck. `crowdvm recover` gets out of it: the sandbox became `ready` in seconds, and the scrub ran (`.aws/r8creds` and `.ssh/id_*` were gone).

Expected: a `VMError` for a `node_offline` sandbox on a node that is back should move it to `machine_error`, or boot it again from its snapshot as `restartLostMachine` does. Better still, a lazy VM should survive a short node restart, for example by retrying curl reads.

### 3. `--no-monitoring`: one exec over ~780 MB fills `/run`, cuts its output with no message, and then every exec fails for 10 minutes (stuck, wrong-result)

Severity: stuck and wrong-result.

Where: `internal/guest/guest.go:164` puts the command history on `runDir()/history` (tmpfs) when monitoring is off or zero data retention is on. `guest.go:105` raised the per-stream output cap to 1 GiB in round 7. `history.go:41` keeps finished unmonitored records for 10 minutes. On a small sandbox `/run` is 784 MB, and it is RAM.

Repro:
1. `crowdvm new --type small --no-monitoring`.
2. `crowdvm exec ID 'yes 0123456789abcdef | head -c 600000000' > o3`: correct, 600 000 000 bytes. `/run` is now at 573 MB used and stays there for 10 minutes, which is 573 MB of the sandbox's 4 GB of RAM.
3. `crowdvm exec --timeout 200 ID 'yes ... | head -c 300000000; echo "rc=${PIPESTATUS[*]}" >&2' > o4`.
4. `crowdvm exec ID 'df -h /run'`.

Observed:
- Step 3 exits 1 with nothing on stderr, and `o4` holds 220 413 952 of 300 000 000 bytes. Stderr is on the same full tmpfs, so even the `head: write error` is lost.
- Step 4, and every exec after it: `error: could not start command: write /run/crowdvm/history/cmd_.../meta.json....tmp: no space left on device (command_failed, HTTP 500)`.
- `crowdvm ssh` still worked and showed `/run` at 100 %.
- It cleared at about 14:29, when the janitor pruned the records.

Expected: unmonitored output either stays within what `/run` can hold (the old 64 MiB cap, or a share of the tmpfs size), or it goes to the guest disk and is deleted when the command ends. A cut stream needs the truncation warning that bug 6 describes.

### 4. A foreign deploy keeps the saver's command history, with output, so the new owner can read secrets the scrub removed (security)

Severity: security.

Where: `internal/guest/apply.go:167` (`runScrub`) deletes files, folders, SSH keys and logins. It leaves the history in `/var/lib/crowdvm/history` (`guest.go:158`) alone, and that history is kept for 90 days (`history.go:39`). `foreignDeployScrub` (`internal/server/lifecycle_secure.go:365`) does not ask for it either.

Repro:
1. alice: an env with the secret file `.aws/r8creds`, then a sandbox on that env.
2. alice: `crowdvm exec ID 'cat ~/.aws/r8creds'`, then `crowdvm snapshot ID secsnap` and `crowdvm snapshot share secsnap`.
3. bob: `crowdvm new --from bsh_...`, then `crowdvm history NEWID --json`, then `crowdvm history NEWID cmd_2h8cw5t6z9cz`.

Observed: the scrub works, so bob's `~/.aws` is empty. But bob's history lists alice's commands, and `crowdvm history NEWID cmd_2h8cw5t6z9cz` prints `aws_secret=TOPSECRET`. `/var/lib/crowdvm/history/cmd_2h8cw5t6z9cz/stdout` holds it.

The same goes for the process history of alice's ssh sessions and agent runs. `security.md:75-83` warns that history keeps printed secrets, but not that it travels to other accounts. `sandboxes.md:93-97` lists what a foreign deploy removes and says "everything else on the disk, including files you wrote yourself, is copied as is". The history is CrowdVM's own record, not a file the user wrote, and nobody would think to clean it.

Expected: a foreign deploy (another account, or another org member) starts with an empty command and process history.

### 5. SIGQUIT to `crowdvm exec` leaves the command running past `--timeout` (leak)

Severity: leak.

Where: `internal/cli/app.go:80` now handles SIGINT, SIGTERM and SIGHUP. Go's default SIGQUIT handler dumps the stack and exits with code 2. `internal/cli/cmd_exec.go:207-208` still starts the detached command with `TimeoutSeconds = 0`, so the sandbox has no deadline of its own.

Repro: `setsid crowdvm exec --timeout 60 ID 'exec sleep 779' &`, then `kill -QUIT` it after 3 s. Ctrl-\ in a terminal does the same.

Observed: the CLI exits with 2 and a goroutine dump. History shows `exec sleep 779` running from 14:18:19 to 14:31:18 and `exited 0`, which is 12 min 59 s past a 60 s timeout. SIGKILL of the CLI (OOM killer, `kill -9`, a CI runner's hard cancel) leaks the same way.

Expected: SIGQUIT handled like SIGTERM. More importantly, a server-side deadline of `--timeout` plus a margin on the detached command, so no client death can leak it.

### 6. A cut exec stream writes the truncation marker into stdout (wrong-result, low)

Severity: wrong-result (low).

Where: `internal/guest/commands.go:144` (`outputMarker`) and `capFile` (`:170`) append the marker to the stdout log, and the CLI copies it to its stdout.

Repro: `crowdvm exec --timeout 300 ID 'yes 0123456789abcdef | head -c 1200000000' > o2`.

Observed: the round-7 fix works: exit 0, a stderr warning (`the command wrote more output than the sandbox keeps (1 GiB per stream); the rest was dropped`), and `outputTruncated: true` in history. But `o2` is 1 073 741 897 bytes, which is 1 GiB of data plus the 73-byte `[crowdvm: output truncated, ...]` line. A `crowdvm exec ID 'cat big.bin' > big.bin` gets a file with foreign text at the end.

Expected: the stderr warning is enough. Keep the marker out of stdout, or strip it in the CLI.

### 7. `crowdvm-server join-token create` prints a join command with a guessed URL, not the server's `--public-url` (cosmetic)

Severity: cosmetic.

Where: `cmd/crowdvm-server/main.go:390-392` uses `guessPublicURL(":8420")` unless the token command itself gets `--public-url`. `getting-started.md:56` shows the command without it.

Repro: start the server with `--listen 127.0.0.1:9960 --public-url http://127.0.0.1:9960`, then run `crowdvm-server join-token create`.

Observed: `crowdvm-node join --server http://192.168.178.31:8420 --token ...`, which is the wrong host and port. On the Tailscale setup in `getting-started.md:116`, the printed command points at the LAN IP. `crowdvm nodes join-token` and the dashboard print the right URL.

Expected: use the public URL the server runs with (store it in the settings table), or say that `--public-url` is needed.

### 8. `migrating-from-boat.md:55-56` contradicts itself on sandbox ids (docs)

Severity: docs.

The text says "Sandbox ids now start with `bx_`, so update patterns that match `bx_`." Line 24 of the same page says Boat ids are `bx_…` too. A reader cannot tell what to change. Probably a leftover from an older id prefix. Drop the sentence, or say that the ids are new but the prefix is the same.

### 9. The Boat key aliases do not cover the key that `crowdvm login` saves (docs, low)

Severity: docs (low).

Where: `migrating-from-boat.md:23` and `:47` say to send `boat_<the part after cvm_>`.

Observed: for an API key made with `crowdvm api-key create`, `boat_`, `box_` and `cvm_` forms all answer 200. The key that `crowdvm login` (password or device) writes to `~/.config/crowdvm/config.json` is a `cvms_...` key, and every Boat form of it gets 401: `boat_<rest>`, `box_<rest>`, `boat_s_<rest>` and `boats_<rest>`. A user who copies the key from their config, the obvious place to look, gets 401 with no hint.

Expected: the docs should say to make a key with `crowdvm api-key create` (or in the dashboard) for Boat tools. Alternatively, the alias should also map session keys.

### 10. `crowdvm nodes update --label` cannot clear all labels (cosmetic)

Severity: cosmetic.

Where: `internal/cli/cmd_server_admin.go:369-377` means to send `{}` when `--label` is given empty, but `parseKV` (`cmd_sandbox.go:55`) rejects `""` first. `node-owners.md:39-40` says `--label` replaces all labels.

Repro: `crowdvm nodes update r8n2 --label ""` gives `error: --label wants KEY=VALUE, got ""`. The dashboard can clear them (`PATCH /nodes/{id} {"labels":{}}` works).

## Round-7 fixes rechecked live: what held

- **Desktop on v3.** Three fresh sandboxes and one stop/resume:
  - Each has exactly one `Xvnc :0 ... -SecurityTypes None -BlacklistTimeout 0`, the unit is `active` with `NRestarts 0`, there is one `xfce4-session`, and the process count is about 150.
  - On resume the race actually happened: the journal shows the image's session start, then `Stopping`, `Stopped`, `Started` within one second. Afterwards only the new session's processes were left (checked by start time, one logind session).
  - 25 aborted websockets plus 10 aborts after reading the version: the next 3 handshakes succeed at 1920x1080.
  - 12 aborts with fresh desktop tokens, then a handshake: OK. Reusing the token: 401 `desktop_token_used`. The guest journal has no blacklist lines.
- **Fork bomb.**
  - Command cgroups get `pids.max` 3516 out of the shared 4688. With one bomb at its cap, `echo alive` works.
  - With two bombs, it still works after a `fork: retry`.
  - `DELETE /commands/<pid>` on both leaders (already exited) emptied both cgroups: `pids.current` went to 3.
- **Large output.**
  - 100 MB comes through byte-exact, `outputTruncated: false`.
  - 1.2 GB is kept to 1 GiB, with the stderr warning and `outputTruncated: true` (but see bug 6).
- **SIGHUP.** The CLI exits with 130, and `sleep 778` is gone from the sandbox. History shows `killed SIGTERM` after 3 s.
- **`--trust-proxy`.** The server now uses the rightmost entry:
  - Firewall `deny 198.51.100.0/24`:
    - `XFF: 198.51.100.7` gets 403.
    - `10.9.9.9, 198.51.100.7` (forged left, proxy-appended right) gets 403.
    - Two header lines with the denied one last get 403.
    - Denied left with allowed right gets 200, which is correct.
  - With `--ip-rate-limit 3`, `10.0.0.$i, 203.0.113.5` gets 429 from the 4th request.
- **`GET /nodes/{id}`.** mal gets `remoteAddr: null`, `sandboxIds: []`. The admin gets both.
- **Drain.**
  - A drained node restarted and reconnected. Its 2 sandboxes stayed `ready`, and `status` shows `DRAINED by the server admin`.
  - `new --node r8n2 --fail-fast` gets `no_ready_machine`. Undrain clears it.
- **Scrub blip.** The fix path itself works once the guest comes up (via `recover`, see bug 2): the scrub runs and removes the secret file and the SSH key.
- **Org delete.**
  - 0 credential profiles, 0 environments and 0 env versions remain for the org.
  - The org sandbox moved to the creator's personal account, and its org secret file was gone after resume.
  - API keys of the org: not checked live.
- **Per-account caps.** The 201st key gets 409 `api_key_limit` (the CLI login key counts). The 100th env gets 409, because `base` counts. The 51st org gets 409.

## New ground: what held

- **`getting-started.md`.**
  - The installer (`curl .../install | sh -s -- --node`, served from a `downloads import` of a hand-built linux/amd64 dist) put `crowdvm`, `crowdvm-node` and `crowdvm-guest` in `~/.local/bin`. It saved the server URL and added the shell function to `.bashrc` and fish. Without a tty it printed `Run: crowdvm onboard`, as the script intends.
  - Device login: `crowdvm login` printed the code, I approved it through `/auth/device/approve` with a dashboard session, and `crowdvm login --wait` signed in.
  - `join-token create`, `join`, `image import` and `image list` work, as do `crowdvm-node status` and the UI on its port. `install-service --dry-run` works.
- **`migrating-from-boat.md`.**
  - `BOAT_API_URL`, `ASCII_API_URL`, `CROWDVM_API_URL`, `CROWDVM_API_BASE`, `BOAT_API_KEY=boat_...` and `BOAT_ORG` work in the CLI.
  - `X-Boat-Org` works. `/api/box/v1/sandboxes` returns `boxes` with `Deprecation` and `Sunset` headers. The Sunset date (31 Oct 2026) is already past, but it copies Boat's header.
  - Inside a sandbox, `boat` and `host` resolve to `crowdvm`, `ASCII_TOKEN` equals `CROWDVM_TOKEN`, and `~/.config/ascii/boat` links to `../crowdvm`.
  - `~/.boat/pre-stop` ran on stop.
  - Ignore files apply at stop, not at push. In a folder with both, `.crowdvmignore` beats `.boatignore` (by design, Boat's order is the other way round). A sub-folder's `.boxignore` applied.
- **`node-owners.md`.**
  - `config set` for cpus, memory, disk and max-sandboxes, and `""` back to the defaults (8 / 32096 MiB / 125 GiB / 4 on this host): all work.
  - `net.allow-lan`, `net.allow` and `net.deny` work.
  - `paused true` shows `PAUSED` and refuses work. An unpinned `new` lands on the other node. A pinned `new --fail-fast` gets a `cancelled` sandbox whose error comes from the node, not an immediate `no_ready_machine`, because the server does not know about the pause.
  - `nodes update --label` (replaces), `--name`, `nodes info`, `credits` and `user grant-credits` work.
  - `bench --quick` takes 2.1 s, and the server sees the new score.
- **Dashboard API, all as `web/js` calls them.**
  - Session: login and `/auth/me`. Account: limits, credits, settings (`notifyEmail`), display name, password change, retention, connections.
  - Sandbox lifecycle: create with Idempotency-Key and `placement.node`, fork (the file came along), saved snapshot, stop to `archived`, resume (the hosted port came back).
  - Desktop and browser URLs, host, history, processes, usage, metrics, conversations, events, AI access.
  - Files: list, put, get, folder, move, download URL.
  - API keys: all 4 presets × 2 TTLs, rotate, usage, revoke. A `read-only` key creating a sandbox gets 403.
  - Orgs: create, invite, accept as another user, role, cap, rename, visibility, org-scoped list, envs and credits, leave, delete.
  - Admin: join token, node labels, users, invite code, retention. A non-admin gets 403 on each.
  - Webhooks: create refuses loopback without `--webhook-allow-local`.

## Not verified

- `image push`. It uploads 4.5 GB, beyond my budget.
- `nodes remove`. I did not want to remove my own nodes mid-run.
- The Windows steps.
- `dev-cluster.sh`. It is fixed to port 8420, outside my range. I only read it. Its "data in `~/.local/share/crowdvm-dev`" is really `.../crowdvm-dev/cluster`.
- Bug 1 after a real node crash under the unit (`Restart=always`). I only tested `restart`. By the same systemd rule a crash should kill the VMs too.
- Bug 4 for org members (shared org saved snapshots) and for process history from ssh sessions. I tested exec history across accounts only.
- Agent prompts. None were run.

## Mistakes on my side

- My first `api-key create` used `--name`, but the name is positional, so a few early alias probes ran with an empty key and got 401. I redid them with a real key; the results above are from the redo.
- I first read a bench score jump (693 to 765) as `--quick` inflating results. A full bench gave the same 766 once the host was idle. The 693 at join was measured under VM load, so that is not a bug.

## Fixes (CLI/docs)

- **#5 fixed.** `cancelSignals()` in `internal/cli/app.go` now adds SIGQUIT next to SIGHUP, and each is added only when not inherited as ignored (nohup rule kept). `runLong` (`cmd_exec.go`) now stops the detached command on any error that ends the follow loop (non-retryable API error, more than 20 failed reconnects, an error frame, a stream that keeps ending), not only on Ctrl-C and timeout. It skips the stop when the process is already `process_lost`. SIGKILL of the CLI still cannot be handled client-side; the server-side deadline from the expected-behaviour note is a server change and was not made here. Tests: `TestBughuntR8CancelSignals`, `TestBughuntR8ExecStopsCommandWhenFollowFails`.
- **#7 fixed.** `serve` writes the public URL to `<data-dir>/public-url`; `join-token create` uses `--public-url`, else that file, else the old guess (`cmd/crowdvm-server/main.go`). This avoids touching `internal/server` (the settings table is private to it). Test: `TestJoinServerURLUsesServePublicURL`.
- **#8 fixed.** `migrating-from-boat.md`: ids keep the `bx_` prefix but are new; replace stored ids.
- **#9 fixed (docs).** Server auth (`internal/server/auth.go`): `cvms_` tokens are accepted only as written; `boat_`/`box_` map to `cvm_` API keys only. The page now says to make the key with `crowdvm api-key create` or the dashboard, not to copy the one from `config.json`.
- **#10 fixed.** `crowdvm nodes update NODE --label ""` sends `{"labels":{}}` (the server takes a pointer to a map, so an empty map clears). Help and `node-owners.md` updated, `make docs` run. Test: `TestBughuntR8NodesUpdateClearLabels`.
