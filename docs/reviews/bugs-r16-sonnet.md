# Bug hunt r16 (sonnet)

2026-11-12. Product code unchanged. I only added `internal/cli/bughunt_r16_test.go`, `internal/node/vm/qemu/bughunt_r16_test.go`, `sdk/typescript/test/bughunt_r16.test.mjs`, `sdk/python/tests/test_bughunt_r16.py` and this report. Unit tests only (the CLI's fake server `clienttest`, a fake systemd notify socket, the SDKs' scriptable fake servers). No cluster, VM or e2e test. I did the work myself, no sub-agents. Tree: `main` at `f80e0d4`, in a detached worktree.

## What I audited

- **Round 15's fixes (`f80e0d4`)**
  - `guest_channel.go` `handleGuestReboot`: the new state check lets `error` and `provisioned` through. `takeExpected` still gates the ticket (an announcement or a recover, 3 minutes, taken once), the 409 for a connected guest comes first, and a stopped or starting-elsewhere sandbox still gets 404. No path got easier for a node that kills its VM on its own.
  - `lifecycle.go` `handleResume`: `newHead` is only a local value until `sets`. Every return between its computation and `updateSandbox(sets)` is a refusal that writes nothing (credits, concurrency, start limits, `resolveEnv`, `chainRestorable`, `chainReachable`, `type_too_small`, `failFast`, `neverFitsErr`). The waiting path queues with `Parent` taken from `newHead`, and `sets` carries the head. The one row it still changes before a refusal is the superseded stop (harmless: the stop worker only reads `archiving`/usable sandboxes).
  - `qemu.go` `forgetFDs`: adopted runs (`r.proc` is the adopted pid, `r.exited` closes on the QMP hangup), relaunch after a reset (the old pid is forgotten before the new launch), node shutdown (`d.closed`: nothing is removed, the VMs stay adoptable), `remove`, and a reused pid (`put` removes the name first). One hole: a VM whose qemu died while the node was down (bug 9).
  - `mux.GuestConfig` (1 MiB window) and `maxEgressFlows` (256, `maxGuestStreams` 512): the window only limits the server's receive side (guest→server data: egress, files the server fetches); the guest side keeps 16 MiB. DNS slots, flows and `ServeLimit` are independent. Nothing else on the channel needs more than 1 MiB / RTT per stream on a LAN.
  - `preinit_linux.go` `bootedPlain`: forks, saved and shared deploys and snapshots of encrypted sandboxes keep the image root untouched (empty machine-id), so they stay encrypted; plain sandboxes get a machine-id from the agent on their first create/fork boot and stay plain; the image build (`images/scripts/finalize.sh`) truncates it. `TMPDIR=/run` works because `/run/crowdvm/cfg` is already mounted when preinit starts. No inspect-VM or warm-pool path reads `bootedPlain`.
  - `proto.HeartbeatResponse.GuestChannel` and the warm pool: `clearWarmShapeLocked` on every heartbeat is idempotent; only legacy creates teach a shape.
  - `install.ps1`: read only (see Code only).
- **CLI (`internal/cli`, `internal/client`)**: `config.go` and the lock (permissions 0600/0700, atomic rename, merge of concurrent saves: fine), `update.go` and `self-update`, `shell-init`, `tunnel`/`forward`, `ssh`, `ssh-config`, `scp`/`transfer.go` (tar extraction against a hostile archive: symlink and hardlink handling held), `exec`/`run`/`attach` (streaming, exit codes, output modes), `snapshot pull`, `host`, `direct-ports`, `prompt`, `login`/`logout`/`onboard`, `env`, `admin`, `billing`, `locallogin.go`, `complete.go`, the Go client's retry, idempotency, stream and upload code.
- **SDKs**: `client.ts`, `helpers.ts`, `errors.ts`, `webhooks.ts`, and `client.py`, `helpers.py`, `aio.py`, `compat.py`, `errors.py`, `webhooks.py`: retries, idempotency, pagination, error mapping, streaming, timeouts, wait helpers.

## How to run

```
go test -count=1 -run TestBughuntR16 ./internal/cli/ ./internal/node/vm/qemu/
cd sdk/python && python3 -m unittest tests.test_bughunt_r16
cd sdk/typescript && npm test            # builds dist, then node --test test/
```

The 10 Go tests, the 1 Python test and the 2 TypeScript tests all fail today. `go vet ./internal/cli/ ./internal/node/vm/qemu/` is clean and `gofmt -l` prints nothing. Without them, the rest of `./internal/cli/` and `./internal/client/` pass, the Python suite passes (65 tests) and the TypeScript suite passes (45). I did not run the whole qemu package (its e2e files need VMs).

## Bugs proven by a failing test

### 1. A nightly build never sees a newer nightly: `self-update` says "up to date" and the notice stays silent (wrong-result, medium)

- Test: `TestBughuntR16NightlyVersionsAreOrdered` (`internal/cli/bughunt_r16_test.go:23`)
- Code: `internal/cli/update.go:62` (`compareVersions`, the `pre` branch at `:73`), used by `updateNotice` (`:161`) and `selfUpdateCmd` (`cmd_config.go:274`).
- Observed: `compareVersions("0.1.1-nightly.20261112.3", "0.1.1-nightly.20261111.9")` is `0, true`; so are the same day's `.4` vs `.3` and `.10` vs `.9`.
- Expected: `1` for each (older = `-1`).
- Cause: `parseVersion` marks every suffix that is not a `git describe` one as `pre`, and `compareVersions` compares only `pre` against `pre` and `after` (0 for both). `scripts/release/version.mjs` names nightlies `<base>-nightly.<yyyymmdd>.<run>`, so two nightlies of one base compare equal. `crowdvm self-update` on a nightly (the GitHub pre-release archives or `install.sh` from a nightly server) reports `available: false` and installs nothing; the "newer crowdvm" line never appears either, until the base version moves. The same goes for `-rc.1` vs `-rc.2`. Fix: compare the dot-separated identifiers of the suffix (numeric ones as numbers).

### 2. `ssh-config` and `crowdvm ssh` write unquoted paths: a space in the config or home directory breaks host key pinning and the identity (wrong-result, medium on Windows)

- Tests: `TestBughuntR16SSHConfigQuotesPathsWithSpaces` (`bughunt_r16_test.go:44`), `TestBughuntR16SSHOptionQuotesKnownHostsWithSpaces` (`:227`)
- Code: `cmd_access.go:109` (`-o UserKnownHostsFile=` + path), `:227` (`IdentityFile %s`) and the `-o` copy loop above it.
- Observed: with the config directory `…/my config`, `ssh-config --print` prints `IdentityFile /…/my config/id_ed25519` and `UserKnownHostsFile /…/my config/known_hosts`; `crowdvm ssh` runs ssh with `-o UserKnownHostsFile=/…/my config/known_hosts`.
- Expected: values with a space in double quotes.
- Cause: OpenSSH parses `-o` values and config lines with the same splitter. `UserKnownHostsFile` takes several whitespace-separated files, so the pinned file is read as two nonexistent ones and `StrictHostKeyChecking=yes` (set because `setKnownHost` succeeded) refuses the host key; in a config file `IdentityFile /…/my config/…` is "garbage at end of line" and ssh refuses the whole file. On Windows the config dir is `%AppData%\crowdvm` (`C:\Users\First Last\…`) and the key is `~/.ssh/crowdvm_ed25519`, so every user whose name has a space is hit.

### 3. `snapshot pull` into a folder that already holds an earlier pull nests the new copy and keeps the stale files (wrong-result, medium)

- Test: `TestBughuntR16SnapshotPullTwiceKeepsLayout` (`bughunt_r16_test.go:63`)
- Code: `cmd_snapshot.go:356` (`extractTar(rc, dest, "", nil)`), then `flattenSingle` (`:363`, defined `:400`).
- Observed: `snapshot pull snap_1 -o out --path /home/user/project` twice. The server's tar is rooted at the folder's name (`project/…`). The first pull extracts into `out/home_user/project/project/` and `flattenSingle` moves it up because it is the only entry. The second finds `a.txt` already in `out/home_user/project/`, so it is not alone, nothing is flattened, and the file lands in `out/home_user/project/project/a.txt`; `out/home_user/project/a.txt` still holds the first pull's content.
- Expected: the second pull overwrites in place. The same applies to a plain `crowdvm snapshot pull SNAP -o DIR` (`home_user/user/…` after the second run) and to the docker volumes.
- Cause: the code extracts first and flattens afterwards. `extractTar` already has a `strip` argument for exactly this (`downloadDir` uses it). Fix: `extractTar(rc, dest, path.Base(rel), nil)` and drop `flattenSingle`.

### 4. `direct-ports ID PORT --clear` publishes the port (and opens the guest firewall for it) (wrong-result, medium; security-adjacent)

- Test: `TestBughuntR16DirectPortsClearRemovesNamedPort` (`bughunt_r16_test.go:100`)
- Code: `cmd_access.go:784-796` (`directPortsCmd`). `--clear` is only read in `len(args) == 1 && !clear`.
- Observed: `direct-ports bx_x 27015/udp --clear` sends `PUT …/direct-ports {"ports":[{"port":27015,"proto":"udp"}]}` and then runs `ufw allow 27015/udp` in the sandbox.
- Expected: the flag's own help is "remove the ports asked for" (`docs/guide/cli-reference.md:1461`, "Ports given replace the earlier list; --clear removes them"): the PUT must not contain 27015 (the current list minus the named ports) and the firewall stays shut.
- Cause: with ports given, the code builds the list from the arguments and PUTs it, whatever `--clear` says. A user who wants to close one port opens it instead. Only `--clear` alone works (empty list).

### 5. Two first-time `crowdvm` commands generate two SSH key pairs, and the loser authorizes a key whose private half was overwritten (wrong-result, low)

- Test: `TestBughuntR16SSHKeyGenerationIsRaceFree` (`bughunt_r16_test.go:128`)
- Code: `cmd_access.go:40-69` (`sshKey`): no lock, `os.WriteFile` for the private key and for the `.pub`.
- Observed: 8 goroutines call `sshKey()` on an empty config dir; at least one gets a public key that is not the one on disk (fails in round 0 of 30).
- Cause: each caller that finds no pair generates one and writes it over the other's. `crowdvm ssh-config id1 & crowdvm ssh-config id2 &` (or `ssh` in two terminals) on a new machine: the command that finished first `AddSSHKey`ed its own public key, and ssh then reads the other's private key. The server accepts several keys per sandbox, so a re-run fixes it, but the first attempt fails with `Permission denied`. Fix: create the pair under the config lock (`lockConfig`), or write the private key with `O_EXCL` and re-read on conflict.

### 6. `ssh-config` counts a commented-out `Include crowdvm/*.conf` as installed (cosmetic)

- Test: `TestBughuntR16SSHConfigIncludeInCommentIsNotIncluded` (`bughunt_r16_test.go:157`)
- Code: `cmd_access.go:253` (`strings.Contains(string(bb), "crowdvm/*.conf")`).
- Observed: `~/.ssh/config` holding `# Include crowdvm/*.conf` gives `"included": true` and no hint. ssh ignores the comment, so `ssh bx_…` does not find the host.
- Fix: look for a non-comment line that starts with `Include` and names `crowdvm/*.conf` (and outside a `Host`/`Match` block).

### 7. `run --json` reports `exitCode: 0` in its summary when the run itself failed (wrong-result, low-medium)

- Test: `TestBughuntR16RunSummaryDoesNotReportExitZeroForAFailedRun` (`bughunt_r16_test.go:176`)
- Code: `cmd_exec.go:435-458` (`code, runErr := func() …`: the failed closure returns `0, err`; the summary uses `code`). The text line prints `exit 0 · …` as well.
- Observed: the command POST answers 400; the run fails with process exit 1 and an error line, but the earlier `{"event":"summary","exitCode":0,…,"deleted":true}` says 0.
- Expected: no `exitCode 0` for a command that did not run (for example `-1`, or omit it).
- Cause: agents read the summary (the documented machine-readable result of `run`); a failed push, a sandbox that never answers or a broken follow loop reads as success.

### 8. `exec`/`run` keep the remote command running when their stdout reader is gone (`… | head`, `… | grep -m1 ready`) (leak, medium)

- Test: `TestBughuntR16ExecStopsRemoteCommandWhenOutputReaderIsGone` (`bughunt_r16_test.go:198`)
- Code: `cmd_exec.go:330` (`io.WriteString(w, f.Data)` in `readAttach`, and `:185` in `runStream`: errors dropped); `app.go:94` (`cancelSignals` has no SIGPIPE).
- Observed: stdout is a pipe whose reader closed. The first stdout frame fails to write, the CLI ignores it, follows the command to its exit frame and never sends `DELETE …/commands/{pid}`.
- Expected: the CLI stops following and stops the remote command, as it does for Ctrl-C, SIGHUP, SIGQUIT and SIGTERM (rounds 7 to 9).
- Cause: on a real stdout the Go runtime kills the process with SIGPIPE at the first write to the closed pipe (exit 141, no message): `runLong`'s cleanup never runs. With `--timeout 0` (or above 600) nothing else ever ends the command (`KillAfterSeconds` exists only with a timeout), so `crowdvm exec ID --timeout 0 "tail -f log" | grep -m1 ready` leaves `tail -f` running in the sandbox for good. The test covers the other half (a non-stdout fd, where the write error is dropped); the fix needs both: `signal.Notify` for SIGPIPE so the write fails instead of killing, and a write error that cancels the follow and runs the stop path.

### 9. A dead VM's boot-ticket memfd stays in systemd's fd store for ever (leak, low)

- Test: `TestBughuntR16AdoptOfDeadQemuForgetsItsFDStoreEntries` (`internal/node/vm/qemu/bughunt_r16_test.go:26`, Linux only)
- Code: `qemu.go:1626-1637` (`adoptAll`, the failed `dialAdopt` branch), `qemu.go:1192` (`storeFD("ticket-<pid>")`).
- Observed: a node restarts while the qemu of one VM is gone. `adoptAll` marks the VM `error` and `continue`s; no `FDSTOREREMOVE` goes to systemd for `qmp-/net-/ticket-<pid>`.
- Expected: round 15's own rule, "the entries named after a qemu pid are removed when that qemu is gone".
- Cause: the socket pairs hang up and systemd drops them itself, but the ticket is a memfd: it never hangs up, so it stays (and is passed back at every later start through `LISTEN_FDS`, where nothing claims it). `remove` only knows `m.rec.PID`, which a recover (`Restart` with `m.run == nil` → `launch`) replaces with the new pid. Small per event (one memfd), but `FileDescriptorStoreMax` is 1024. Fix: `d.forgetFDs(rec.PID)` in that branch, and on every `launch` for the pid being replaced.

### 10. `waitForDesktop` / `wait_for_desktop` wait out a 400 and hide its reason (wrong-result, low) — TypeScript and Python

- Tests: `bughunt r16: waitForDesktop fails at once on a 400 …` (`sdk/typescript/test/bughunt_r16.test.mjs:14`), `test_bughunt_r16_wait_for_desktop_fails_at_once_on_a_400` (`sdk/python/tests/test_bughunt_r16.py:9`)
- Code: `sdk/typescript/src/helpers.ts:76` (`[400, 409, 503]`), `sdk/python/src/crowdvm/helpers.py:97` (`(400, 409, 503)`).
- Observed: `theme: "neon"` makes the server answer `400 invalid_theme`; the helper polls to its timeout (120 s by default) and throws `WaitTimeout("timed out waiting for the desktop of …")`.
- Expected: the `ApiError` at once. The server's 400s on this route (`invalid_theme`, `invalid_profile`) are the caller's mistakes and never become a desktop. (The 409 `machine_not_running` for a stopped sandbox is waited out too, for 2 minutes; that one is arguably a choice.)

### 11. The TypeScript request timeout is one deadline for the whole request, so a download that makes progress is cut (wrong-result, medium for large files) — TypeScript

- Test: `bughunt r16: a bytes download that keeps making progress …` (`sdk/typescript/test/bughunt_r16.test.mjs:31`)
- Code: `sdk/typescript/src/client.ts:191` (the timer is cleared only `if (stream && timer)`), `:317` (`_op` reads the blob before `cleanup()` clears it).
- Observed: `timeoutMs: 400`, a server that sends a 4-byte chunk every 100 ms for 800 ms: `APIConnectionError: reading the response failed: This operation was aborted`.
- Expected: a timeout on waiting for the headers (as streams have) or on inactivity, like Python's per-read socket timeout (the Python SDK reads the same body without error). `artifact` (up to 50 MiB) and `getSnapshotFile` (a whole folder as tar, after an inspector VM boot) fail after 60 s on a slow link, with the default settings, even though bytes keep arriving. A large JSON response is cut the same way.

## Code only

- **`crowdvm push` / `scp -r` / `exec --push` hold the whole archive in memory** (`transfer.go:170`: `writeTarGz(&buf, …)` into a `bytes.Buffer`, then `Upload`; crash/leak, medium). A directory of a few GB (a dataset, an untracked `node_modules`, a submodule with its own ignored files) needs that much RAM before the first byte goes out; the signed-URL upload does not need the length up front. No test: it takes gigabytes. Fix: pipe the tar through `io.Pipe` into `Upload` (chunked PUT).
- **Shell ids are process ids and survive a reboot** (`app.go:582` `shellID`, `config.go` `Shells`; wrong-result, low). `shell-init` sets `CROWDVM_SHELL_ID=$$`. After a reboot a new terminal often gets the pid an older shell had (a few hundred to a few thousand are reused all the time), and `Shells[pid]` is kept for 14 days, so `crowdvm stop` / `exec` without an id act on the previous session's "current" sandbox. Also the 14-day cutoff measures the last *write* (`Seen` is only set by `saveConfig`), so a tmux shell that only reads its `current` for two weeks loses it. Fix: put the shell's start time (or the boot id) into the id. Not tested: needs a pid collision.
- **`install.ps1` fails on the second upgrade while the first one's old binary still runs** (`install.ps1` `Fetch`; stuck, low). A running `crowdvm-node.exe` (the service) or a `crowdvm.exe tunnel` (ssh ProxyCommand) is renamed to `<name>.old`; `Remove-Item … -ErrorAction SilentlyContinue` cannot delete it while it runs, and the next `Move-Item -Force $dest "$dest.old"` onto the locked `.old` throws; `$ErrorActionPreference = 'Stop'` ends the installer half done (new `.tmp` downloaded, binary not replaced; or, when the failure comes at the last `Move-Item`, no binary at all). No PowerShell here. Fix: pick a fresh `.old.<n>` name, and put the old file back if the final move fails.
- **`prompt --attach a/log.txt --attach b/log.txt` overwrite each other** (`cmd_agent.go:122`: `"/home/user/attachments/" + filepath.Base(f)`; wrong-result, low). Two attachments with the same base name end up as one file, and the prompt lists the same path twice.
- **`self-update` / `install` verify nothing** (known: bugs-r15-sonnet). `self-update` also *runs* the downloaded binary (`--version`) before it decides anything.
- **The SDKs never retry a network error of a POST, not even with an `Idempotency-Key`** (`client.ts:206`, `client.py:177`; cosmetic). The Go client does (`client.send`, key makes it safe). `create`/`fork` take `idempotencyKey`, but a dropped connection still surfaces after the server may have created the sandbox.
- **Python's raw `attach_command` raises `APIConnectionError` after a `reconnect` frame** while TypeScript's `attachCommand` just ends (`client.py` `_lines` vs `client.ts` `lines`; cosmetic parity). `follow_command`/`followCommand` handle it.

## What held

- **Round 15 fixes**: see "What I audited". In particular the `recover` of a sandbox in `error` gets its ticket, a stopped sandbox still does not, and a refused `resume` writes nothing to the sandbox row.
- **Config and credentials**: `config.json` is written 0600 through a temp file and `rename` under a lock file; an unreadable config is never replaced; fields from a newer version survive a save; `update-check.json`, `completion-cache.json`, `known_hosts` and the key are 0600, the config dir 0700. `login --key-stdin` keeps the key out of the arguments; a saved key is not sent to another server.
- **Extraction of a hostile archive** (`extractTar`, `checkNoSymlinkParents`, `linkInside`, hardlinks, `..` after a name): I tried absolute and escaping symlinks, a symlink to `.` then a file below it, hardlinks to files outside, and names that clean to the destination: none writes outside `dest`.
- **Exit codes**: signals map to 128+n, 124 for the CLI's own timeout, 130 for a cancel, 2 for usage; the attach loop reconnects with offsets without gaps or repeats.
- **`host`**: `--private` is the default and re-protects a public port (the server mints a new token), as the code comment says.
- **SDKs**: retry classes (429 windows, `no_ready_machine`, restoring, starting), `Retry-After` parsing, stream EOF handling, header merging, path-segment checks, pagination, webhook verification (the NaN/inf timestamp cannot be forged: the signed text contains it), `with_options`, the asyncio facade's cancellation.
- **Go client**: `CreateSandbox` and `ForkSandbox` always send an `Idempotency-Key`, so they are retried on network errors and 502/504; plain POSTs are not.

## Not verified

- `ssh`'s real parse of `UserKnownHostsFile=…` and `IdentityFile …` with spaces (from `ssh_config(5)` and my memory of `argv_split`; no Windows and no space-path ssh run here). Bug 2's Windows case is derived from `os.UserConfigDir`.
- Bug 8 on a real stdout: the Go runtime's SIGPIPE-on-fd-1/2 rule is from the `os/signal` documentation. A small run of the built binary into `head` did not reproduce it because the output was one write.
- The default `FileDescriptorStoreMax` effect of bug 9 (systemd's refusal at the limit, as in r15), and whether systemd drops the hung-up QMP and NIC ends of a dead qemu by itself (documented behavior, not observed).
- `install.ps1` (never run), `install.sh` on macOS.
- `preinit`'s LUKS flows and `growpart` with `TMPDIR=/run`: read, not run. The claim that `bootedPlain` is right for every disk rests on the image build (`finalize.sh`) and the agent writing the machine-id (`boot.go:269`).
- A real browser or Node-version difference for bug 11 (Node 26 `fetch`/undici; the abort is the SDK's own `AbortController`).

## Fixes

All 11 fixed; every `BughuntR16` test passes (Go, TypeScript, Python).

1. **Nightly order** (`update.go` `comparePre`): pre-release suffixes compare by dot-separated identifiers, numeric ones as numbers.
2. **Unquoted ssh paths** (`cmd_access.go` `sshQuote`): `IdentityFile` and `UserKnownHostsFile` values with a space are double-quoted, in `ssh-config` and in `crowdvm ssh`'s `-o`.
3. **Second snapshot pull nests** (`cmd_snapshot.go`): the tar's root folder (or a volume's `_data`) is stripped while extracting; `flattenSingle` is gone.
4. **`direct-ports PORT --clear` publishes** (`cmd_access.go`): it removes the named ports from the current list and opens no firewall.
5. **SSH key race** (`cmd_access.go` `sshKey`): the pair is created under the config lock, re-checked inside it.
6. **Commented Include** (`cmd_access.go`): only a non-comment `Include` line naming `crowdvm/*.conf` counts.
7. **`run --json` exit 0 on failure** (`cmd_exec.go`): `exitCode` is `null` and the text line says `failed` when the run itself failed.
8. **Closed stdout** (`app.go` `catchPipe`, `cmd_exec.go`): while exec/run stream, SIGPIPE is caught so a write to a closed stdout fails; the write error ends the follow and runs the stop path (the remote command is deleted). Only around those streams: elsewhere a closed stdout still ends the CLI as before.
9. **Ticket memfd of a dead VM** (`qemu.go` `adoptAll`): a VM whose qemu is gone at adoption (or whose network cannot be reattached) has its fd store entries removed.
10. **`waitForDesktop` waits out a 400** (`helpers.ts`, `helpers.py`): a 400 is raised at once; 409 and 503 are still waited out.
11. **TypeScript timeout cuts a progressing body** (`client.ts` `send`): `timeoutMs` bounds the wait for the headers and then each silence of the body, not the whole body.

Code only, also fixed: `install.ps1` renames a running binary to a fresh `<name>.<random>.old`, removes earlier `.old` files when it can, and puts the old binary back when the final move fails.

Not fixed: `crowdvm push` holding the archive in memory, pid-based shell ids, `prompt --attach` with equal base names, the SDKs not retrying POSTs with an idempotency key, Python's raw `attach_command` on a reconnect frame, unverified `self-update` downloads.
