# Bug hunt r5 (opus): live cluster, 2026-11-11

Setup: one `crowdvm-server` on 127.0.0.1:9960 (start limits off, `--ai-custom-api-allow-private`, no `--webhook-allow-local`). Two qemu nodes, `r5n1` and `r5n2`, with status UIs on 9961 and 9962. Each had 6 vCPU, 14 GiB and 60 GiB, with image `ubuntu-24.04-crowdvm-v3` from `~/.local/share/crowdvm-dev/images/`. Fake upstreams ran on 9966–9969, on loopback, `[::1]` and the LAN address 192.168.178.31. Users were `admin`, `alice`, `bob` and `mal`, plus orgs Alpha (alice owner, bob member) and Beta.

I built at 11:57. Between 12:01 and 12:09 another agent changed `feat_host.go`, `feat_otel.go`, `orgs.go`, `outbound_guard.go`, `lifecycle_holds.go` and two CLI files. I checked every bug below against the current source. One bug I reproduced is already fixed there; I rebuilt and confirmed the fix live (see "Already fixed in the tree").

Cleanup: every sandbox is deleted and the server, nodes, VMs and fake upstreams are stopped. No qemu processes are left. The data dir `~/.local/share/crowdvm-r5opus` and my scratch files are deleted. The CLI ran with its own HOME, so nothing went to `~/.ssh` or `~/.config`.

## Bugs

### 1. `scp --api -r` lets a sandbox overwrite any file on the user's machine (security)

Where: `internal/cli/transfer.go:312-313` (`writeFile` opens `<target>.crowdvm-partial` with `O_CREATE|O_TRUNC`, which follows symlinks) and `:401-405` (`extractTar` creates symlinks from the archive as they come). The archive is built by `tar` inside the sandbox (`downloadDir`, `:337`), so the sandbox controls it.

Repro, with stock GNU tar and no root:
1. On the host: `echo ORIGINAL > /tmp/.../victim.txt`.
2. In the sandbox: `mkdir ~/evil; cd ~/evil; for n in a b c d e f g h i j k l m n o p; do ln -s /tmp/.../victim.txt $n.txt.crowdvm-partial; echo PWNED-BY-SANDBOX > $n.txt; done`. Sixteen pairs make sure some symlink lands in the tar before its file (readdir order).
3. On the host: `crowdvm scp --api -r ID:/home/user/evil ./xfer/evil`.

Observed: `victim.txt` now holds `PWNED-BY-SANDBOX`. The path check at `:396` is lexical and passes, because `evil/c.txt` is inside the destination. But the temp file it writes through is a symlink the archive just created. The same works for `~/.bashrc` or `~/.ssh/authorized_keys`. The CLI falls back to this path on its own when no `scp` binary is installed (`cmd_access.go:302`).

Expected: code in a sandbox (an agent, a dependency's postinstall) can never write outside the download folder. Create the temp file with `O_EXCL` (or `os.CreateTemp` in the target dir), and refuse to write through any path component that is a symlink. `crowdvm snapshot pull` uses the same `extractTar` (`cmd_snapshot.go:356`). I could not trigger it there, because the server's tar is sorted (`a.txt` before `a.txt.crowdvm-partial`) and a second pull extracts into a fresh subfolder. I would still fix both.

### 2. A fork that waits for capacity copies the source from when it starts, not from when `fork` ran (wrong-result)

Where: `internal/server/lifecycle.go:1705-1710` queues the fork with `LiveFrom: src.ID`. When the fork is placed, `lifecycle_place.go:139-140` and `lifecycle.go:367` take a fresh live snapshot of the source. `Parent` (the head at fork time) is ignored for live forks.

Repro (both nodes full):
1. `exec W 'echo important > ~/checkpoint.txt'`, then wait 70 s so a periodic snapshot holds it.
2. `crowdvm fork W --name ckpt` returns at once in state `cloning` and waits for capacity.
3. `exec W 'rm ~/checkpoint.txt'`, the risky change the checkpoint was for.
4. Free a node. The fork becomes `ready`.

Observed: `ls ~/checkpoint.txt` in the fork fails with "No such file or directory". In a second run, a fork requested at progress counter 8 came up 3.5 min later with counter 210.

Expected: Boat says a fork of a running sandbox "uses its last background snapshot", meaning the one at the time of the call. CrowdVM's own guide (`docs/guide/sandboxes.md:20`) says the fork "takes a live snapshot … and starts the copy on the same node". Here the snapshot comes up to 15 min later, and the first fork went to the other node. "Fork, then try something risky" is a normal agent pattern. With a queued fork it silently captures the result of the risky change. Take the live snapshot when `fork` is called, or boot a queued fork from `Parent`.

### 3. The desktop still locks every user out after a few aborted connections (stuck)

Where: the round-4 fix added `-BlacklistThreshold 0` only to `images/rootfs/full/usr/local/lib/crowdvm/desktop-session:6` (changed today) and to the fallback at `internal/guest/desktop.go:166`. The guest prefers `crowdvm-desktop.service` from the image (`desktop.go:141-160`). In the shipped `ubuntu-24.04-crowdvm-v3` image, which every node has, that service runs `Xvnc … -SecurityTypes None -AlwaysShared -AcceptSetDesktopSize -nolisten tcp`, with no threshold. Separately, `internal/server/feat_desktop.go:450` dials Xvnc before it claims the one-use token at `:458`, so a reused token opens and drops a VNC connection before it gets 401.

The wallpaper poll no longer connects, which fixed the always-on lockout. But any five connections that close before the RFB handshake still blacklist 127.0.0.1, which is every relay connection:
- 5 × (fresh desktop URL, open websocket, close at once), as with a reload or a closed tab: the next proper connection got security type 0 ("Too many security failures") for about 10 s.
- 6 × reusing a spent token (each answered 401 `desktop_token_used`): the real desktop was locked out, and the guest journal shows `Connections: blacklisted: 127.0.0.1`.
- TigerVNC doubles the block (10 s, 20 s, 40 s …), and each block lets one connection through. A client that retries without finishing the handshake, like my first probe, burned those slots. The desktop stayed locked for about 90 s.

On a public desktop anyone can do this without a token, so the owner can be kept out for as long as the attacker likes.

Expected: aborted or refused connections never lock out the desktop. Claim the token before `DialSandbox`. Have the guest pass `-BlacklistThreshold 0` on images it did not build (for example, a systemd drop-in written before `systemctl start`), or refuse to rely on old images.

### 4. A service account whose key expired can never be cleaned up by its org (stuck, leak)

Where: `internal/server/sandboxes.go:197` (a sandbox counts as an orphan only when its creator is missing, disabled or no longer a member) and `internal/server/acct_settings.go:397-404` (the account cannot be deleted while it has sandboxes). There is no route to mint or rotate a service account's key, and the account's own key cannot call `/api-keys` (403 `api_key_org_forbidden`).

Repro:
1. alice: `POST /orgs/Alpha/service-accounts {"name":"bot1m","preset":"ci","ttl":"1m"}`.
2. With its key: `POST /sandboxes {"type":"small"}` gives `bx_x9q9zxd4`.
3. Wait 1 minute. The key now gets 403 `api_key_expired`.
4. alice (org owner): `DELETE /sandboxes/bx_x9q9zxd4` gives 403 `owner_only`. `DELETE /orgs/Alpha/service-accounts/<id>` gives 409 `member_has_sandboxes`. The server admin gets 404 on the sandbox.

Observed: the sandbox keeps running and billing Alpha, and nobody in the org can delete it or the account. The only way out was for a server admin to run `crowdvm admin suspend <id>`. That marks the creator disabled, and then alice could delete the sandbox. `ci` keys expire after 90 days by default, so every CI bot hits this eventually.

Expected: org owners and admins can delete a service account's sandboxes directly, or deleting the account deletes (or offers to delete) its sandboxes.

### 5. One tenant's slow OTel collector still delays every tenant's lifecycle logs (stuck)

Where: `internal/server/feat_otel.go:276-280`. The log loop runs `otelParallel` over all destinations and waits for all of them before the next flush. `otelPost` allows 10 s per request (`:323`). A collector that answers 200 after 9.5 s never triggers the retry hold.

Repro: alice exports to a fast fake collector. mal exports to one that sleeps 9.5 s and then answers 200. Stop mal's sandbox, and alice's sandbox 1 s later.

Observed: mal's `sandbox.archived` went out at once to the slow collector. alice's `sandbox.archived` reached her collector 8.5 s after the event. A tenant that keeps producing events holds every flush round to about 10 s. The round-4 fix made metric sampling parallel, and that part held.

Expected: each destination flushes on its own. Don't let one round wait for the slowest destination, or give log posts a short deadline that triggers the hold.

### 6. An org with a fractional balance can't transfer what the API shows, and then can't be deleted (stuck)

Where: `internal/server/orgs.go:366` refuses deletion when `round6(balance) > 0`. `internal/server/credits.go:42-43` (`walletDebit`) needs `balance >= amount`. The API shows `round6(balance)` (`credits.go:61-66`), which can round up.

Repro (I built the balance with admin grants; billing produces the same arbitrary floats):
1. Beta's balance is 0.99458293334. `crowdvm credits --org Beta` shows 0.994583.
2. `crowdvm credits transfer --from Beta --to alice --amount 0.994583` gives 402 `insufficient_credits`.
3. `--amount 0.994582` works and leaves 0.00000093334, shown as 0.000001.
4. `crowdvm org delete --org Beta` gives 409 `org_has_balance`. A transfer of 0.000001 gives 402.

Observed: the owner is stuck unless they guess an amount below what the API shows. Roughly every second balance rounds up like this.

Expected: a way to move "all" credits (the server debits the exact balance), or a delete that sweeps a sub-cent remainder to the owner.

### 7. A CI `apiUrl` ending in `#` still turns the appended path into nothing (security, minor)

Where: `internal/server/feat_ci.go:997` checks `u.Fragment != ""`, but `http://h/x#` parses with an empty fragment. `ciGH.do` then requests `http://h/x#/user`, and Go drops the fragment.

Repro as mal: `POST /ci/connect {"token":"t2","apiUrl":"http://192.168.178.31:9967/internal/admin/delete-everything#"}`. The fake server logged `GET /internal/admin/delete-everything` twice with `Authorization: Bearer t2`. The call returned 200 and a pending link with `accountLogin: ""`, so any JSON 2xx is accepted as a valid GitHub token.

Observed: the round-4 `?` check is bypassed. The outbound guard still blocks loopback and metadata (see "What held"), so this only reaches LAN hosts, which are allowed by design. Still, a user picks the exact path the server calls with a header they choose, and a confirmed link polls it every 15 s. Refuse a `#` anywhere in `apiUrl`, and require a non-empty `login` from `/user`.

### 8. Secret files still replace or silently skip directories the denylist misses (cosmetic, can break the desktop)

Where: `internal/server/feat_env.go:572` refuses a fixed list of top-level names only.

Repro: `env set-file sf` with `.dbus`, `.crowdvm`, `.local/bin`, `.config/xfce4`, `.gnupg` and `.ascii`. All six are accepted. In a new sandbox:
- `.config/xfce4`, `.dbus`, `.crowdvm`, `.gnupg` and `.ascii` became files. With `~/.config/xfce4` a file, `xfconfd` failed ("No backends could be started"), and no `xfdesktop`, `xfce4-panel` or `xfwm4` ran. The desktop is an empty X screen.
- `~/.ascii` is where the guest says detached logs live. `exec --detach` still worked, but it reports a `logPath` of `/home/user/.ascii/processes/1060.log` that cannot exist.
- `.local/bin` stayed a directory. The only trace is `rename … /home/user/.local/bin: file exists` in the guest journal. `info` shows nothing.

Expected: refuse any path that is a directory in the image, at any depth, or that the desktop or guest creates later. Or have the guest report failed writes in the sandbox status, as round 4 suggested.

### 9. `crowdvm info` drops `waitingForCapacity` and `statusMessage` (cosmetic)

Where: `internal/client/types.go:41-93`. `Sandbox` has `WaitingForSnapshot` but not `waitingForCapacity`, `waitingSince`, `waitDeadline` or `statusMessage`, which `internal/server/lifecycle_place.go:209-215` sends.

Repro: with both nodes full, `crowdvm new --name captest2` (or `fork`, or `resume --node r5n2`). `GET /sandboxes/ID` has `waitingForCapacity: true` and the "Waiting for a node with room …" message. `crowdvm info ID --json` shows only `"state":"starting"`, `"node":null`.

Expected: `docs/guide/troubleshooting.md:16-17` tells users to look for `waitingForCapacity: true` in `crowdvm info`. `fork` returns at once in `cloning` and never says it waits; the 20 s "waiting for a node" note in `internal/cli/helpers.go:72` only covers `provisioning`.

### 10. `exec` cuts stdout at 8 MiB without a word in text mode (cosmetic)

Where: `internal/cli/cmd_exec.go:145-149` writes `res.Stdout` and ignores `res.StdoutTruncated`.

Repro: `crowdvm exec ID 'head -c 20000000 /dev/zero | tr "\0" x | fold -w 100' > out.txt` writes 8 388 608 bytes, exit code 0, and nothing on stderr. `--json` shows `stdoutTruncated: true`, and `--stream` gets all 20 199 999 bytes.

Expected: the limit is documented (8 MiB per stream), but an agent reading text output can't tell it lost data. Print `crowdvm: stdout truncated at 8 MiB; use --stream or --detach` on stderr.

## Already fixed in the tree

- Hiding a port while a websocket waits for a starting sandbox. On my 11:57 build: stop `a1`, `resume --no-wait`, open the websocket with a valid `_port_auth`, then `host hide` 0.5 s later. The upgrade succeeded 2.9 s after the hide and echoed for 5 s, while a new GET got 404. The current `feat_host.go:689` tracks the request before `WaitUsable`. After rebuilding, the same steps gave 503 at 0.7 s. Same bug as `TestBughuntR5HideDuringStartWaitDoesNotCutWebsocket`.

## Round-4 fixes rechecked: what held

- Desktop (#1): no idle connections in the guest journal. A session held 95 s with framebuffer updates every 5 s (19 rounds, 576 KiB). Ten clean reconnects in a row all worked, also after stop/resume and a cross-node resume. For the remaining lockout, see bug 3.
- CI outbound guard (#2): refused `127.0.0.1`, `localtest.me` (resolves to `::1`), `127.0.0.1.nip.io`, `[::1]`, `[0:0:0:0:0:0:0:1]`, `[::ffff:127.0.0.1]`, `0.0.0.0`, `[::]`, `[::1%25lo]`, `[fe80::1%25lo]`, `LOCALHOST` and `localhost.`. Decimal, hex, octal and short forms (`2130706433`, `0x7f000001`, `017700000001`, `127.1`) don't resolve with the pure-Go resolver. A 302 from a LAN host to `127.0.0.1` was refused at dial time. For the `#` hole, see bug 7.
- AI-proxy traversal (#3): 400 for `..%2F..%2F`, `%2E%2E/`, `.%2e`, `%2e.`, `..%5c`, `%2e%2e%2f`, and with a query. `a/../../admin` and `./.././../` get a 307 from the mux's path cleaning and never reach the upstream. `%252e%252e`, `%C0%AE%C0%AE` and `..%00` reach the upstream still encoded under the base path. `..;/` passes as is, which only matters to Tomcat-style upstreams.
- `host hide` and `--private` (#5): an open websocket closed within 1 s of either one, both path-routed and host-routed. Normal websockets still work: one idle for 75 s then echoed a 200 KB message.
- History privacy (#7): with the guest clock 90 s ahead and NTP off, private-A, shared-B, private-C and shared-D, across share/unshare and a stop/resume, showed bob exactly B and D.
- OTel (#4): metric sampling runs in parallel, and I found no unsynchronized shared state in `collectMetrics` or `sample`. For log flushing, see bug 5.
- Service-account deletion (#6): refused with 409 while the account has a ready or a stopped sandbox. An org member gets 403 on an orphan; the org owner can delete it once the creator is disabled. For expired keys, see bug 4.
- Error notices (#9): after a 40 s SIGSTOP of r5n2, both "Sandbox failed" notices appeared and were gone 15 s after SIGCONT, once the sandboxes were ready again.
- Org delete with a balance is refused. For the rounding trap, see bug 6.

## Everyday agent workflows: what held

- Environment with `env add-repo work https://github.com/spf13/pflag.git`: cloned on boot, and `go build ./... && go test ./...` passed in 7 s.
- `exec --detach` with a 300 s job, `--status` and `--attach`. An attach kept streaming across a server restart (t1…t25, END, exit 0). A synchronous `exec` and an `exec --stream` across a server restart each ran once (checked with an append log) and returned the right exit code (0 and 3).
- A fork had the WIP commit, the files, and the source's detached process shown as `lost`. For queued forks, see bug 2.
- `snapshot W tmpl1` took 2 s. `new --from tmpl1` was ready in 4 s with the marker file, a 1 GiB file (md5 matched) and the git state.
- `scp`: 1 GiB up and 700 MiB down, md5-exact, in 3 s and 2 s through scp and 2.3 s and 1.5 s through `--api`. Directory trees kept modes. The `--api` mode keeps symlinks; plain scp follows them. For the `--api` download problem, see bug 1.
- `ssh-config` plus `ssh -L` to a dev server, `crowdvm forward --remote 8000` with 20 parallel requests, and `forward --reverse` from host to sandbox all worked.
- `host 8000 --public` served the dev server. After a stop the URL gave 503 "not running (archived)", which says `archived` where the CLI says `stopped`. After resume it gave 502 "Nothing answered on port 8000" until the server ran again. The hosted-port config survived.
- `push` (git-aware): ignored files stayed local, and a name with spaces and quotes, a symlink and edits went across. Renamed or deleted files stay on the sandbox, as documented.
- `prompt` with no credentials: a clear 409 `provider_not_configured` with the setup URL. With a fake Anthropic key: `Invalid API key`, status `failed`, exit 1, and the conversation was kept across stop/resume.
- Stop/resume took 3 s each way. Committed and uncommitted git changes, files and the 1 GiB file survived. So did a cross-node resume (`--node r5n2`), once capacity freed up.
- Inside a sandbox, the CLI's own key lists only that sandbox and gets 403 `api_key_sandbox_forbidden` on another.

## Not verified

- `snapshot pull` with crafted symlinks: same code as bug 1, not triggered (see there).
- Agent prompts with real model credentials.
- Docker container history after 10 min of silence (round-4 #10). It needs 10 minutes.
- Service-account creation racing its deletion. In `acct_settings.go:398-407` the count and the key revoke aren't in one transaction. I did not try to hit the window.

**TL;DR:** 10 bugs. The worst: a sandbox can overwrite any file on the user's machine through `crowdvm scp --api -r`. A fork that waits for capacity copies the source minutes late, after the change it was meant to guard against. Five aborted desktop connections still lock the desktop, because the shipped image lacks the Xvnc flag and the server dials before it checks the token. An org can't delete a service account's sandboxes once that account's key expires. Most round-4 fixes held. The host-hide race I reproduced is already fixed in the current tree.

## Fixes (CLI)

- **#1 fixed** (`internal/cli/transfer.go`). `writeFile` now writes to a fresh `os.CreateTemp` file in the target folder (exclusive create, so a planted `<name>.crowdvm-partial` symlink is never opened) and renames it over the target, which replaces a symlink at that name instead of following it. `extractTar` calls `checkNoSymlinkParents` for every entry: a path that escapes the destination, or whose parent directory under the destination is a symlink (one the archive made earlier), aborts with `unsafe path in archive`. Absolute paths and `..` were already neutralised by the `path.Clean("/"+name)` step. This also covers `snapshot pull`, which shares `extractTar`. Tests: `TestScpDownloadDirPartialSymlinkNotFollowed`, `TestScpDownloadDirSymlinkParentRefused`.
- **#9 fixed** (`internal/client/types.go`, `internal/cli/cmd_sandbox.go`). `Sandbox` now has `waitingForCapacity`, `waitingSince`, `waitDeadline` and `statusMessage`, so `info --json` keeps them. Text `info` prints a `waiting:` line (`for capacity (waitingForCapacity: true)`) and the `message:` line. Test: `TestInfoShowsWaitingForCapacity`.
- **#10 fixed** (`internal/cli/cmd_exec.go`). The 8 MiB cap is the sync endpoint's documented output limit (PARITY CMD-004) and lives in the server, which this round does not touch. Text-mode `exec` now uses the streaming path (the same as `--stream`), which has no cap, so nothing is lost and there is no memory buffering on the CLI. `--json` still uses the sync result and its `stdoutTruncated` flag. The stream path now also prints the `command timed out after Ns` line that sync text mode printed. `TestR3ExecSignals/sync*` now passes `--json` to stay on the sync endpoint. Test: `TestExecTextModeStreamsPastSyncCap`.
