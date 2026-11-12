# Bug hunt r15 (sonnet)

2026-11-12. Product code unchanged; I only added `internal/server/bughunt_r15_test.go`, `internal/node/vm/qemu/bughunt_r15_test.go` and this report. Unit tests only (`servertest`, the fake node, a fake systemd notify socket). No cluster, VM or e2e test. I did the work myself, no sub-agents. Tree: `main` at `ec65ad3`.

## What I audited

- **Round 14's fixes (`ec65ad3`)**: `guest_channel.go` (`forgetExpected`, the running-state check in `handleGuestReboot`, the nonce retry in `redeemGuest`, migration 3, the new "stale session" check in `handleGuestConnect`); `guest/channel.go` (`helloNonce`, `dnsTransport`, `dnsInFlight`); `snapshots.go` `onDiskOf` against forks, live forks, shared and saved deploys, rollbacks, lazy chains, evacuated and rebuilt sandboxes (every legitimate parent I could construct is on the sandbox's disk; no honest layer is refused except when its parent's row does not exist yet, see Code only); `mux.go` (`CloseWrite`, `ServeLimit`/`limitListener`, `IdleTimeout` 30 s against the 25 s client idle timeout, every `mux.Serve`/`mux.Transport` user, `Join`'s use of `CloseWrite`); `guest_egress.go` DNS slots; `lifecycle_place.go` `rebuildLost` against `markUnconnected` and `seenAt`; `lifecycle.go` `handleResume` with `newerSnapshotOn`; `guest.go`/`internal.go` ops caller (`channelHandler` is separate, so the server's own stop keeps the paths); `node/warm.go`; `node/ui_session.go` and `ui.go` (`used` map growth is bounded by valid signatures only, `access` runs once per request, so one-use signatures do not collide with themselves).
- **Secure layer, `e453353` and `bbc8dad`**: `fdstore_linux.go`, `qemu.go` (spawn, adopt, `checkFDVMs`, `relaunch`, `reticket`, `remove`, `kill`), `netstack.go` datagram NIC, `node.go` `guestChannel`/`reticket`, `service_linux.go` (fd store with systemd 254+), `guest/preinit_linux.go` (`redeem`, `openData`, fresh label), `guest/egress.go` (private TCP, `inVMNetwork`, restore loop), `server/guest_egress.go` (`localNets`, `apiAddrs`, `ParseEgressAllow`), `lifecycle.go` (`useChannel`, encrypted placement), `lifecycle_place.go`, `scheduler.go`, `nodes.go` (`grantChain` expiry), `feat_webhooks.go` `secretKey`.
- **Install scripts**: `internal/server/feat_install_files/install.sh` (run for real with `bash`, a fake `crowdvm` and a local HTTP server, `HOME` in a scratch dir) and `install.ps1` (read only; no PowerShell here), with `feat_install.go`.

## How to run

```
go test -count=1 -run TestBughuntR15 ./internal/server/ ./internal/node/vm/qemu/
```

All three tests fail today. `go vet ./internal/server/ ./internal/node/vm/qemu/` is clean and `gofmt -l` prints nothing. With `-skip TestBughuntR15`, the qemu package passes and so do the server tests matching `GuestChannel|Egress|Snapshot|Resume|Recover|TestR14|Bughunt` (69 s). I did not run the whole server package.

## Bugs proven by a failing test

### 1. `crowdvm recover` of a sandbox in `error` never gets its ticket: round 14's state check refuses the node's re-ticket request (stuck, medium)

- Test: `TestBughuntR15RecoverFromErrorGetsNoTicket` (`internal/server/bughunt_r15_test.go:56`)
- Code: `guest_channel.go:369` (`!sb.Usable()` → 404 `not_running`), `lifecycle.go:1901` (`handleRecover`: `expectReboot`, then the node's restart, then the state transition), `internal/node/node.go:183` (`reticket` gives up on any 4xx except 409), `internal/node/vm/qemu/qemu.go:1510` (`Restart` calls `reticket` before it relaunches).
- Observed: a channel sandbox in state `error` (a crash or an unannounced reboot), the owner runs `recover` as the notice says. The server has expected the node's request since `handleRecover` started, but the sandbox is still `error` while the node's driver asks for the ticket (`Restart` does it before `launch`). The node's `POST /node/v1/sandboxes/{id}/reboot` gets **404**, which the node treats as final: `Restart` fails and the VM goes to `error`; `recover` answers 409 `reset_failed`. The test leaves exactly that state by making the restart fail once, then sends the node's request: 404.
- Expected: 200 with a ticket. The comment on `handleGuestReboot` says a recover the server started gets a ticket; round 14's new check made that impossible for `error`.
- Cause: the check `!sb.Usable()` (ready, idle, running) also refuses `error`, which `handleRecover` allows (`sb.State == StateError`), and `provisioned`. `e453353`'s notice ("Run `crowdvm recover`") and the guide point owners to exactly this path. The existing guest-channel test does not catch it because the fake node's restart does not ask for a ticket, and the state has become `ready` again by the time the test asks. Opus r14 saw recover work after a crash on `5697bfe`; it does not on `ec65ad3`. Fix: let the check pass when the server expects the request (`takeExpected` succeeded) and the sandbox has a machine on that node (`error`, `provisioned` and usable states), and refuse only archived/starting-elsewhere.

### 2. A refused `resume` still moves the sandbox's head to a layer that is not on the server (data-loss risk, medium)

- Test: `TestBughuntR15RefusedResumeMovesTheHead` (`internal/server/bughunt_r15_test.go:15`)
- Code: `lifecycle.go:1559` (`handleResume`: `updateSandbox(head_snapshot_id = newerSnapshotOn(...))` runs before the credit, concurrency, start-limit, `chainRestorable`/`chainReachable`, `type_too_small` and placement checks).
- Observed: sandbox in `error` with a stored head `old` and a newer periodic layer its node is still uploading. `POST /resume` with a placement no node meets answers 503, and `head_snapshot_id` is now the unstored layer.
- Expected: a refused request changes nothing. `afterSnapshotStored` moves the head only to a stored layer ("so a node lost mid-upload leaves the head on stored data").
- Cause: the head is rewritten as the first step. Any later refusal (a credit or concurrency limit, `type_too_small`, `snapshot_unavailable` from `chainReachable`, no node fits) leaves the head on data that exists only on the failed node. If that node never comes back, the next `resume` hits `snapshot_unavailable` for ever (`newerSnapshotOn` finds nothing newer than the new head), and `rebuildLost` holds ("the newest data is only on the lost node") instead of rebuilding from the stored layer. `crowdvm stop --force` (`fallBackToStoredHead`) is the way out. Fix: compute the head, validate with it, and write it with the other `sets` after the sandbox is placed (or restore it on every error path).

### 3. Descriptors in systemd's fd store are never removed: after a few hundred launches the node can no longer adopt its VMs (leak, medium; a restart then kills them)

- Test: `TestBughuntR15FDStoreKeepsDestroyedVMsSockets` (`internal/node/vm/qemu/bughunt_r15_test.go:22`, Linux only)
- Code: `qemu.go:1152` and following (`storeFD` of `qmp-<pid>`, `net-<pid>`, `ticket-<pid>`), `qemu.go:955` `remove`, `qemu.go:1566` `kill`, `fdstore_linux.go:81` `remove` (only `put` ever calls it).
- Observed: with `NOTIFY_SOCKET` pointing at a datagram socket I own, destroying a machine (`d.remove`) sends systemd nothing. No code path anywhere sends `FDSTOREREMOVE`, not on destroy, not when qemu exits (reboot relaunch, recover, stop).
- Expected: the entries named after a qemu pid are removed when that qemu is gone.
- Cause: `e453353` names the entries by qemu pid ("stable across a rename"). Before, `put` replaced `qmp-<sandbox id>`, so the store grew by two per sandbox ever created; now each launch adds up to three that nothing replaces or removes, and `FileDescriptorStorePreserve=yes` keeps them even across a stop of the unit. `FileDescriptorStoreMax=1024` is reached after roughly 340 launches (each reboot of a channel guest, each recover and each resume launches). systemd then drops new `FDSTORE=1` messages without telling the sender (`notify` returns nil), so a VM started after that has no stored sockets and the next node restart fails in `dialAdopt` ("its QMP socket was not passed back"): `adoptAll` kills a qemu that is `ours` and marks the sandbox `error`. The node also starts with up to 1024 inherited descriptors (`LISTEN_FDS`) it never closes. Fix: remove the three names when a run ends (`watch`'s exit path) and in `remove`.

## Code only

- **`install.ps1` overwrites binaries in place** (`install.ps1:11`, `Invoke-WebRequest ... -OutFile` onto the final path; wrong-result/stuck, low-medium). On Windows a running `crowdvm.exe` or `crowdvm-node.exe` (the installed service) cannot be overwritten, so re-running the installer to upgrade stops at the first locked file (`$ErrorActionPreference = 'Stop'`) and leaves a half-upgraded set. A dropped download truncates the exe at once. `install.sh` does it right (`.tmp`, then `mv`). A script test is impractical (no PowerShell here). Fix: download to `<name>.tmp`, then `Move-Item -Force` (rename works for a running exe when the target is renamed away first), and set `$ProgressPreference = 'SilentlyContinue'` (Windows PowerShell 5.1 downloads the 50+ MB binaries many times slower with the progress bar).
- **Nothing the installers download is verified** (`install.sh:33`, `install.ps1:11`; security, low-medium, by design today). The scripts fetch binaries from `$SERVER/downloads/...`, which is plain `http://` in the documented one-liner, and run them (the CLI at once, the node as a service). The server publishes no checksum or signature. Anyone on the path (or who can write the downloads dir) owns the machine. A `SHA256SUMS` served from the same server only protects against corruption over http; with https it adds nothing the TLS does not. Worth a line in `docs/guide` (use https) rather than code.
- **`redeem` in preinit treats every `server refused` as final** (`internal/guest/preinit_linux.go:124`; stuck, low). `redeemGuest` also answers `config_unreadable` for a failed `UPDATE guest_channels` (a busy SQLite, a full disk for a moment). Preinit then powers the VM off, the node's re-ticket request gets 403 (unannounced), and the sandbox sits in `error` until `crowdvm recover`. A transient database error should be a 5xx, not a refusal the guest takes as final. Not tested: it needs a failing database write.
- **`recordTaken` drops a layer whose parent has no row yet** (`snapshots.go:195`; low). If the report of layer 1 was lost (it "does not wait for a connection") and layer 2's report arrives before layer 1's upload recorded it, layer 2 is refused at the report. It is recorded again when its upload ends (`onSnapshotUploaded`), so the layer is not lost in the normal serial order; the `snapshot.uploaded` of a layer that races the other one's upload (parallel uploads) leaves a stored blob with no row. Before round 14 it was recorded with generation 0. Only an ordering race; no test.
- **`install.sh` on macOS with bash**: it appends to `~/.bashrc` (created when `$SHELL` is bash), which Terminal.app's login shells never read (`.bash_profile`), so the function does not load. Cosmetic.
- **`install.sh` `--bin-dir=~/bin`** is taken literally (no tilde expansion after `=` in `sh`): a directory named `~` is created in the current directory. Cosmetic.
- **A hostile guest can hold all 64 DNS slots of its own channel** (`guest_egress.go:265`): the slot is taken before the body is read and `http.Server` has no read timeout past the headers, so a guest that sends headers and no body keeps its own DNS dead (and 64 of the 2048 stream slots). Only its own sandbox suffers. Cosmetic.
- **The one-use UI signature rejects a second tab** (`ui_session.go`): two tabs sharing the session (same localStorage) that sign the same `GET /api/status` in the same millisecond produce the same signature; the second gets 401 once. Each tab keeps its own `lastMs`. Cosmetic, one poll.

## What held

- **Round 14 reboot-ticket logic**: `forgetExpected` runs on every new ticket, `takeExpected` is consumed once, a stop clears nothing it should keep, and the `recover` of a usable (degraded) sandbox still gets its ticket (bug 1 is only the `error`/`provisioned` states).
- **Nonce retry**: the nonce travels inside TLS, only the guest knows it, a node's own redemption keeps getting `ticket_used`; two overlapping dials with one nonce end with the newest session (`handleGuestConnect`'s hash check drops the older); `issueGuestChannel` clears `hello_hash`, so a retry never crosses boots.
- **`onDiskOf`**: forks, live forks (the live layer belongs to the source and is in the child's `bootParent` chain), shared and saved deploys, rollbacks (own layers), lazy chains, evacuated and rebuilt sandboxes (their `bootParent` is set after every boot) all pass. A foreign parent outside `bootParent` and the head's chain is refused.
- **`mux`**: `CloseWrite` on `limitConn`, `bufConn`, `readerConn` and the yamux stream is right (`Close` is a half-close there); `Join` treats an error from `CloseWrite` as "close both". The 30 s `IdleTimeout` covers keep-alive waits only (no `ReadTimeout`; hijacked conns clear their deadlines), so long requests, streaming responses, tunnels and uploads are not cut; the 25 s client idle timeout sits below it for every transport made by `mux.Transport`. The other transports over yamux sessions do not dial streams for keep-alive (tunnels hijack).
- **DNS slots and guest transport**: the slot is released on every path (`defer`), the guest's cap and the server's cap are independent, `dnsTransport` swaps on a new session and closes the old idle conns.
- **`rebuildLost`'s start guard** composes with `markUnconnected` (nodes missing after the reconnect window get a fresh `degraded_since`).
- **Ops caller**: `channelHandler` is separate from `Handler`, so the server's own `prepare-stop` over the channel keeps its file list.
- **`ui_session`**: the `used` map holds only signatures that passed the HMAC and the nonce check, so memory cannot be driven by an attacker; entries live twice the skew, which covers a future-dated signature; `access` is evaluated once per request.
- **Secure layer**: `grantChain` expiry, `mayFetch` rules, the placement gate for encrypted disks in `pickNode`, `couldEverPlace`, `requeueBoot`, `tryPlace`, `handleResume`, `handleFork`, `handleCreateSandbox`; `secretKey` refusing to regenerate; `ParseEgressAllow`; the `localNets` /32 rule (CGNAT/tailnet stays blocked by `cgnat` in `egressBlocked`); `inVMNetwork` leaves `10.0.2.0/24` to the server path; `reticket` retries 409, 5xx and network errors in its 20 s budget; datagram NIC framing (`readPackets`, `writeFrame`, `writeLoop` under `wmu`); `adoptNet` and `dialAdopt` fall back to the old names; `saveRec`, lazy and create paths never write the ticket to `vm.json`; `checkFDVMs` blocks only when a live VM needs the store.
- **`install.sh`** (run with bash): fresh `HOME` gets `.zshrc` and the fish `conf.d` file when `$SHELL` is zsh and fish exists, idempotent on a second run (`grep` guard), `set -u` safe, atomic replace of the binaries, no root use, nothing downloaded for unknown options (`exit 2`).

## Not verified

- QEMU's `-netdev dgram,local.type=fd`, systemd's fd store limits and its behaviour at `FileDescriptorStoreMax` (bug 3 rests on systemd's documented refusal to store beyond the max and on `notify` not waiting for an answer; I proved the missing removal, not the refusal), systemd 254's `FileDescriptorStorePreserve`.
- `preinit`'s LUKS flows (`openData`, the fresh label, growing online), `blkid -p -s LABEL` on a LUKS2 header, the iptables rules and the restore loop: read, not run.
- `install.ps1` ran nowhere. `install.sh` ran on Linux with bash only (no dash on this machine, no macOS).
- `ui.html` in a browser (the second-tab collision and any automatic browser retry of a GET with the same header).
- Bug 1's real timing against the real driver: the test reproduces the server side (state `error`, reboot expected, node's request); the node side (`Restart` → `reticket` → no retry on 404) is from the code.
- Whether any production sandbox sits in `error` with a newer unstored layer on a lost node (bug 2's long-term effect is derived from `chainReachable` and `rebuildLost`).

## Fixes

All 3 fixed; the tests in `internal/server/bughunt_r15_test.go` and `internal/node/vm/qemu/bughunt_r15_test.go` pass. Bugs 1 and 2 were regressions of round 14's fixes.

1. **Recover from `error` gets no ticket** (`guest_channel.go` `handleGuestReboot`): a sandbox in `error` or `provisioned` (it still has its machine on the node) passes the state check too; stopped or starting-elsewhere sandboxes still get 404.
2. **Refused resume moves the head** (`lifecycle.go` `handleResume`): the newer head is used for the checks and written with the resume's other changes, only once the resume is accepted.
3. **fd store fills up** (`qemu.go` `forgetFDs`): `qmp-`, `net-` and `ticket-<pid>` are removed when a qemu run ends (not when the node itself shuts down) and when a VM is removed. Not fixed: descriptors passed back at start that no VM claims stay open until the node exits (a ticket may still be claimed after adoption).

Code only, also fixed:
- `install.ps1` downloads to `<name>.tmp`, renames a running binary away to `.old` and moves the new one in, and turns off the progress bar.
- A database error while a ticket is redeemed answers `retry_later`, which preinit retries instead of powering off.

Not fixed: unverified installer downloads (use https; noted for the guide), the parent-without-row ordering race in `recordTaken`, the macOS `.bashrc` and `--bin-dir=~` cosmetics, a guest holding its own DNS slots, and two tabs signing in the same millisecond.
