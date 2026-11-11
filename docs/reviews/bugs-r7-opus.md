# Bug hunt r7 (opus): live cluster, 2026-11-11

Setup: I ran `make build` at 14:15 and copied the binaries to my own data dir, so later rebuilds could not change them mid-run. One `crowdvm-server` ran on 127.0.0.1:9960. Two qemu nodes, `r7n1` and `r7n2`, had status UIs on 9961 and 9962, each with 6 vCPU, 14 GiB, 40 GiB and at most 3 sandboxes. Both used `ubuntu-24.04-crowdvm-v3` and `-minimal-v3`, imported with `crowdvm-node image import` (a reflink copy). Webhook receivers ran on 9967 (fast) and 9968 (hangs for 10 minutes). Users were `admin`, `alice`, `bob` and `mal`. Each CLI ran with its own HOME.

The tree changed under me. The sonnet r7 hunt fixed several files after my build (`cli/transfer.go`, `cli/cmd_exec.go`, `guest/commands.go`, `server/lifecycle_notify.go` and others, from 14:27 on). I re-read the current source for each bug below. Bugs that are already in `bugs-r7-sonnet.md` are listed only under "Also reproduced".

Cleanup: every sandbox is deleted. My server, nodes, VMs and receivers are stopped, `pgrep` finds none of them, and no qemu process of mine is left. The data dir `~/.local/share/crowdvm-r7opus` and my scratch files are deleted. The `/tmp/crowdvm-*-net.sock` files from 14:11 and 14:13 date from before my server started at 14:15, so they are not mine.

## Bugs

### 1. The desktop blacklist fix loses a race at boot: Xvnc keeps the blacklist, and the desktop unit restarts forever (stuck, leak)

Where: `internal/guest/desktop.go:131-152` (`patchOldImage`), with `crowdvm-desktop.service` from the v3 image (`PAMName=login`, `Restart=on-failure`).

`patchOldImage` runs in a goroutine after the guest starts listening. It writes the drop-in, runs `daemon-reload` and runs `try-restart`. When systemd has already started the desktop from the image's own script, the restart stops the unit but not the old Xvnc. Because of `PAMName=login`, Xvnc lives in the user's login session scope (`0::/user.slice/user-1000.slice/...`), not in the service's cgroup. The new instance then cannot get display `:0` and exits with status 1. `Restart=on-failure` starts it again every 2 s, forever, and each attempt leaves another `xfce4-session`. The old Xvnc, which has no `-BlacklistTimeout 0`, keeps serving the desktop.

Repro:
1. `crowdvm new --type small` three times, or `stop` then `resume` an existing sandbox.
2. In each sandbox: `ps -C Xvnc -o args=; systemctl is-active crowdvm-desktop; pgrep -c xfce4-session`.

Observed: 2 of 3 fresh sandboxes, and the sandbox after `resume`, show `Xvnc :0 ... -SecurityTypes None -AlwaysShared ...` without `-BlacklistTimeout 0`. The unit is `failed` or `activating`, and `systemctl status` lists the drop-in. In one sandbox `NRestarts` was 148 after 5 minutes and 647 after 25 minutes, with 23 `xfce4-session` processes and 411 processes in total. Only one fresh sandbox in four came up right. My first sandbox, a1, came up right on create and lost the race on resume.

Then the round-5 lockout comes back. With a fresh token for each connection, 6 websockets closed right after the upgrade lead to `RFB 003.003` plus "Too many security failures" on the next connections, and the guest journal shows `Connections: blacklisted: 127.0.0.1`. It recovers after about 10 s. On a sandbox that won the race, 25 aborts and 10 aborts after the version string changed nothing.

Expected: Xvnc always runs with the flag, and the unit does not restart in a loop. Two possible fixes:
- Write the drop-in before the unit can start. For example, a guest `ExecStartPre`, or the drop-in in the image's `/etc`, since the image is rebuilt anyway.
- Make the restart kill the old Xvnc. For example, `KillMode=control-group` together with `PAMName`, `pkill -u user Xvnc` before `try-restart`, or `StartLimitBurst`.

### 2. Text-mode `exec` silently cuts stdout at 64 MiB, exits 0, and history says not truncated (wrong-result, data-loss)

Where: `internal/cli/cmd_exec.go:129-140`. Since round 6, text mode runs every command detached and follows it, and the comment there says "no cap, no silent loss". Detached output goes through the guest's new 64 MiB per-stream cap: `internal/guest/commands.go:148-176` (`capOutput`, `capFile`) and `guest.go:105` (`outputCap: 64 << 20`). `capFile` never sets `OutputTruncated`. Only the sync path sets it (`commands.go:346`).

Repro: `crowdvm exec ID 'yes 0123456789abcdef | head -c 100000000' > out; echo $?`

Observed: exit 0, nothing on stderr. `out` is 67 108 937 bytes: 64 MiB, then `[crowdvm: output truncated, size limit reached, further output dropped]` written into the data itself. `crowdvm history ID --json` shows `"stdoutBytes":67108937,"outputTruncated":false`. 30 MB came through byte-exact. Before round 6 this command printed everything, since text mode used the stream endpoint. So this is a regression for `crowdvm exec ID 'cat big.log' > big.log` and similar data pipes.

Expected: either no cap on output that a client is following live, or a cap that cannot be missed: a non-zero exit or a stderr warning in the CLI, and `outputTruncated: true` in the record. At the least, `capFile` should set `OutputTruncated` when it cuts, and the CLI should not write the marker to stdout as if it were data.

### 3. `exec` killed by SIGHUP (closed terminal) leaves the command running for ever (leak)

Where: `internal/cli/app.go:78` handles only `os.Interrupt` and `SIGTERM`. Since round 6, text-mode exec starts the command detached with no server-side timeout (`cmd_exec.go:229`, `req.TimeoutSeconds = 0`). Only the CLI enforces `--timeout`, and only the CLI kills the command.

Repro: `crowdvm exec --timeout 60 ID 'exec sleep 778'` in its own session, then `kill -HUP` the CLI after 3 s.

Observed: the CLI dies at once. 64 s later `pgrep -af "sleep 778"` in the sandbox still shows it. It stays until someone kills it by hand. SIGINT and SIGTERM work: exit 130, and the process is gone. Before round 6, the guest killed a sync or stream command at `--timeout`. The round-6 fix notes accept this cost for SIGKILL. SIGHUP is catchable, though, and it is what closing a terminal or a dropped SSH session sends.

Expected: add `syscall.SIGHUP` (and `SIGQUIT`) to `NotifyContext`. Better, keep a server-side deadline on the detached command (a timeout of `--timeout` plus a margin) so that SIGKILL cannot leak it either.

### 4. One runaway command locks every later command out of the sandbox, and killing it by id does nothing (stuck)

Where: `internal/guest/cgroup.go:70-75` creates `crowdvm-workloads.service` with `systemd-run` and no `TasksMax`. systemd therefore applies its default (`pids.max=4688` in a v3 small sandbox), and that limit covers every command, setup script and agent together. `internal/guest/commands.go:564` (`handleCommandKill`) signals the process group only `if p != nil && !p.finished()`, that is, only while the leader is alive.

Repro:
1. `crowdvm exec --detach ID 'bomb(){ bomb|bomb& }; bomb'`
2. 15 s later: `crowdvm exec ID 'echo alive'`
3. `DELETE /api/v1/sandboxes/ID/commands/<pid of step 1>`, then step 2 again.

Observed: step 2 fails with `could not start command: fork/exec /bin/bash: resource temporarily unavailable (command_failed, HTTP 500)`. In `/sys/fs/cgroup/user.slice/crowdvm-workloads.service`, `pids.current` = `pids.max` = 4688. Step 3 answers 200 with the record (`status exited`, leader exit code 0) and kills nothing, so step 2 fails again. The owner's way out is `crowdvm ssh` (sshd is outside that cgroup), then writing `1` to `.../cmd_<id>/cgroup.kill`. `crowdvm recover` also works. The neighbour sandbox on the same node stayed fine (same `uptime`, exec in 60 ms). A normal `nohup server &` started by a command whose leader has exited is likewise impossible to kill by id.

Expected: a `pids.max` for each command's cgroup, below the workloads limit, so that one command cannot starve the others. A kill by id should write `cgroup.kill` for the command's group, even when the leader has exited.

## Also reproduced (already in bugs-r7-sonnet.md, fixed in the tree after my build)

- Sonnet 11, `extractTar` link chains: reproduced live with plain symlinks, no archive swapping needed. In the sandbox, `~/evil2` held `n3 -> n2/..`, `n2 -> n6/..` and `n6 -> .`, with names picked so that `tar` lists them in that order. `crowdvm scp --api -r ID:/home/user/evil2 ./xfer2/inner/evil2` exited 0 with no warning, and `xfer2/inner/evil2/n3/secret.txt` read the host's `xfer2/secret.txt`. For `snapshot pull` (archives sorted by name), a chain `a -> b/..` through `f -> .` in `~/evil4`, pulled with `snapshot pull SNAP -o pull/p1/out --path /home/user/evil4`, gave `a` resolving to the folder three levels above `-o`. Neither path writes through the links: `checkNoSymlinkParents` holds. The current `linkInside`, which refuses a `..` after a name, closes both orders. I re-read it, but did not re-run it live.
- Sonnet 17, only the first `file_skipped` notice: 4 paths skipped (`.pi/agent`, `.claude/skills/crowdvm`, `.config/opencode/skills`, `.local/share/uv`). The guest's event ring holds all 4 `file.skipped` events, but the server stored one notice. It was the same with 2 paths, and with 2 events I posted to `/node/v1/events` myself. The current `lifecycle_notify.go:24` keys the dedupe on the message.
- Sonnet 3, `GET /nodes/{id}`: my cross-tenant probe found it as the only route that returns 200 with another tenant's data.
- Note, not new: `.cache/xfce4` is still accepted as a secret file path, and it became a regular file. The desktop creates that folder only later, so neither the server list nor the guest check catches it. `desktopTrees` covers only `.config/xfce4/` and `.dbus/`.

## Round-6 fixes rechecked: what held

- Desktop (on a sandbox whose Xvnc has the flag, see bug 1 for the others):
  - 25 aborted websockets with fresh tokens, and then 10 aborts after reading the version: the next handshakes succeed (1920x1080).
  - A spent token gets 401 `desktop_token_used`, 3 times out of 3, and no token gets 401.
  - Public desktop: one anonymous handshake, 40 anonymous aborts, then 60 anonymous websockets held open. The owner connected each time, and again after release.
  - The API key of another user gets 404.
- Text-mode exec:
  - Exit codes: 42; 137 for `kill -9`; 143 for TERM; 124 plus `command timed out after 2s`.
  - stdin gives EOF, and stderr goes to stderr.
  - Binary output becomes U+FFFD as before, and a UTF-8 rune split across writes arrives whole.
  - 40 parallel execs of `seq 1 20000; exit i%5` all returned 20 000 lines and the right code.
  - SIGINT and SIGTERM (sent from a real session; a backgrounded shell job ignores SIGINT) give 130 and kill the command in the sandbox.
  - A server restart 5 s into a 25 s command lost no line, and the exit code 7 came through.
  - History shows every CLI exec as `mode: detached`, so `--mode sync` no longer finds them, and a client-side timeout is recorded as `killed SIGTERM`, not as a timeout. `crowdvm ps` is a process list and is not confused.
- Waiters across a `kill -9` node restart (1 s down): CLI `new` and `resume` printed `state error` and then `ready`, and exited 0. Python `wait_until_ready` became ready in 1.5 s. TypeScript `waitUntilReady` (from `dist`) became ready in 2.0 s. The server still sends `sandbox.error` and a `sandbox_error` notice on every blip (the notice clears itself). The round-6 fix notes already list this as server-side and open.
- Secret files: the deeper folders from round 6 are refused (`.cargo/bin`, `.config/xfce4/xfconf`, `.claude/skills`, `.local/share/applications`, `.ssh`). Existing folders that are not on the list are skipped with `a folder already exists at this path`, and the folder stays intact.
- Drain: `crowdvm-node status` shows `Accepting DRAINED by the server admin (no new sandboxes; crowdvm nodes undrain)`, the UI API shows `drained: true`, and both clear after undrain.
- Webhooks:
  - mal and bob had 16 webhooks on the hanging endpoint. alice's delivery still arrived 1.2 s after her `stop`. The hanging endpoint never saw more than 4 attempts per account in flight.
  - Deliveries `limit` 0, -1 and `abc` give 400. 1000 is clamped.
  - mal gets 404 on alice's webhook for read, test, patch, rotate and delete.
  - URL guard: loopback, `[::1]`, `0.0.0.0`, `169.254.169.254`, `127.0.0.1.nip.io` and `[::ffff:127.0.0.1]` are refused. `2130706433` is accepted, but the delivery fails with `no such host` (pure-Go resolver).
  - An account can have 10 webhooks at most.
- Org delete removes the org's environments, versions, webhooks, telemetry export, credential profiles, CI links, git sources and saved snapshots: 0 rows left.
- Admin sandbox list:
  - `limit=1` walks in both sort orders return all 6, with no duplicates.
  - `state=stopped` returns the archived ones.
  - Bad `limit`, `cursor` or `sort` give 400.
  - Non-admins get 403.
- `scp --api -r` otherwise: hardlinks are recreated as links (link count 3), a 250-byte name works, and a FIFO and a socket are skipped.

## New areas: what held

- Cross-tenant probe: I sent all 291 server routes with a path parameter to the server with mal's key and alice's ids. These covered sandbox, snapshot, saved snapshot and share, webhook, environment, API key, stop operation, org, host port, node and user ids. Only `GET /nodes/{id}` returned her data (sonnet 3). A few routes check the body before ownership, which gives 400 or 409. With valid bodies they answer 403 or 404: `PATCH /orgs/{org}`, env vars, secret files and repos, `ai-access`, `tunnel`. Hosted port 8080, path and subdomain form: 401 without the token, with mal's key, and even with alice's key (only the token or cookie opens it). Desktop websocket with mal's key: 404.
- Fork bomb: contained to its own sandbox (bug 4 is about the sandbox owner).
- Disk:
  - The node limits admission only. With the reserve raised so that 4 GB was left, a sandbox wrote 6 GB of random data unchecked: host free went to 135.8 GiB, below the 138 GiB "kept free". The node then refused new sandboxes (`out_of_capacity`), as `node-owners.md:58-60` describes. A tenant with 3 small sandboxes can still fill this host (3 × 63 GiB per disk against 141 GiB free), and the QEMU pause on ENOSPC would then stop every VM on it. I did not fill the host, so that last part is from code reading only (`disk.go:22-24`).
  - The guest disk is 63 GiB for a "51 GB" small sandbox. The extra space is the OS image (`qemu.go:332-335`), by design.
- Server snapshot storage under churn: 7 rounds of writing 1 GB of random data, deleting it and running `fstrim`, each followed by a periodic snapshot. The server's blob store moved in a sawtooth between 2.8 and 4.9 GB, and the node stayed at 5.4 GB. The 6 GB from the earlier test was reclaimed. It does not grow without bound.
- API object caps: webhooks are capped at 10. mal created 60 orgs with no cap (sonnet 15).

## Not verified

- Bug 1 on the minimal image (no desktop) and over many more boots. I saw the race in 4 of 5 boots of the full image.
- Actually filling the host disk to ENOSPC. My budget was a few GB.
- The sonnet fixes after 14:27. I re-read the code for the three that touch my areas (`linkInside`, `file_skipped` dedupe, attach offsets). I did not rebuild and re-run them.
- Agent prompts (Claude or opencode). Nothing in scope needed them.

## Mistakes on my side

- My first node-restart test ran against a full node: `new` only waited for capacity. Stopping that background shell with the harness also killed the restarted `r7n2` daemon. Its 3 sandboxes went to `error node_offline` and came back `ready` within 20 s of a clean start. I redid the test with the node restart in its own session.
- My first SIGINT test sent the signal to a background job of a non-interactive shell, which ignores SIGINT. The CLI then ran into its 30 s timeout, and the guest killed the command at the timeout as designed. The repeat from a real session group is the one reported above.
