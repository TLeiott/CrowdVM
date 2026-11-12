# Bug hunt r14 (sonnet)

2026-11-12. Product code unchanged; I only added `internal/server/bughunt_r14_test.go`, `internal/mux/bughunt_r14_test.go`, `internal/node/bughunt_r14_test.go` and this report. Unit tests only (`servertest`, the fake node, a guest-channel client written for the tests). No cluster, VM or e2e test. I did the work myself, no sub-agents.

The branch moved while I worked: I started on `5697bfe` (with someone's uncommitted secure-layer fixes in the tree), and `e453353` (`fix(security): close the production review findings for the secure layer`) landed on `fix/secure-review` before I finished. I ran every test below against a clean `git archive` of `5697bfe` and against the tree at `e453353`: all six fail on both. Line numbers are from `e453353`. Findings that `e453353` fixed meanwhile (deleted sandboxes keeping their channel and sealed config, `--no-guest-channel` with encrypted chains, a stale session taking a new boot's place, the one-shot event follower) are not in the bug list.

## What I audited

- **Round 13's fixes (`5697bfe`)**: `internal/node/lock.go` and every `newDriver`/`LockDataDir` caller in `cmd/crowdvm-node` (refcount, release on error, Windows `LockFileEx`, `config set`, `image` fallbacks); `ui_session.go` `validRequest` and `ui.html`'s signing; `warm.go` `warmOverCapacityLocked`; `netstack/keepalive.go`, `CloseGuestConns` and its call in `qemu.go` `watch`; `exitDetail`; `internal/guest/files.go` (`openFileNB`, `readCappedLine`, `writeTarGz`, `readSmallFile`); `ClearAdopted` with `cmd/crowdvm-guest/main.go`'s boot paths; `acct_sshkeys.go` migration 2 and the guard; `device.go` (per-code lock, `codeMisses`, guarded `UPDATE`s); `nodes_evacuate.go` (`newerSnapshotOn`, `evacMark`, `failedEvacMoves`, how `evacuateStep` ends); `lifecycle.go` `requeueBoot`; `lifecycle_place.go` exclusion drop; `snapshots.go` allowance lock.
- **Secure layer, server side (`30d3bbd`)**: `guest_channel.go` (ticket and session lifecycle, reconnect, reboot tickets, races with stop, resume, fork, delete, evacuation), `guest_egress.go` (address checks, flows, DNS), the `nodes.go` grants (`mayFetch`, `grantChain`, blob and snapshot routes), `lifecycle.go` `boot`/`ensureDiskKey`, `snapshots.go` `snapDiskCols`/`recordTaken`/`afterSnapshotStored`, `scheduler.go`, `feat_snapfiles.go`; guest side `channel.go`, `egress.go`, `egress_linux.go`, `preinit_linux.go` `Preinit`/`redeem`; `internal/mux` for the upgraded connections both sides use.

## How to run

```
go test -count=1 -run TestBughuntR14 ./internal/server/ ./internal/mux/ ./internal/node/
```

All six tests fail today, in every run (3 of 3 for the server package). `go vet ./internal/server/ ./internal/mux/ ./internal/node/` is clean and `gofmt -l` prints nothing. With `-skip TestBughuntR14`, `internal/mux` and `internal/node` pass, and so do the server tests matching `GuestChannel|Egress|Evac|Device|Saved|Snapshot` (27 s). I did not run the whole server package.

## Bugs proven by a failing test

### 1. A guest's reboot announcement is never cleared, so a node can get a boot ticket (config + disk key) after any stop (security, medium)

Two tests, one cause.

- Tests: `TestBughuntR14StoppedSandboxHandsItsNodeATicket` (`internal/server/bughunt_r14_test.go:172`) and `TestBughuntR14StaleRebootAnnouncementOutlivesTheBoot` (`:119`)
- Code: `guest_channel.go:142` `expectReboot`, `:152` `takeExpected`, `:344` `handleGuestReboot` (no state check, `:359`), `guest_egress.go:254` (`POST /v1/rebooting`), `lifecycle.go:1921` (`handleRecover`).
- Observed (a): the guest announces on its channel, goes down, the owner stops the sandbox (state `archived`, no VM). The node's `POST /node/v1/sandboxes/{id}/reboot` gets **200 with a new ticket**. (b): same announcement, then the owner resumes the sandbox within the 3 minutes. Boot 2 connects, its channel closes (the node killed the VM), and the node's reboot request gets 200 again, for a boot that announced nothing.
- Expected: 403 `unannounced_reboot` in (b); no ticket at all for a stopped sandbox in (a).
- Cause: the guest calls `announceReboot` whenever `systemctl is-system-running` says `stopping` (`internal/guest/channel.go`, `Serve`), which is true for a poweroff and for a reboot. So every stop leaves an announcement behind, and `takeExpected` only forgets it when someone takes it or after 3 minutes (`expect[id]` is also never cleared when a new boot issues its ticket). `handleGuestReboot` does not look at the sandbox's state or at the boot the announcement belonged to. A node owner can therefore wait for any stop, ask for a ticket, boot a modified guest from the stopped disk it holds, redeem the ticket and receive the full config (every secret) and the disk key: exactly what round 2 of the red team closed ("a node can kill its VM and ask for a new ticket"). A failed `recover` (`reset_failed`) leaves the same 3-minute window. Fix: `issueGuestChannel` and `handleGuestReboot` clear `expect[id]`; refuse a ticket unless the sandbox is in a state that has a running VM; tie the announcement to the boot's ticket hash.

### 2. A node chooses a snapshot's parent, and so reads another account's snapshot layers (security, medium for plain disks, low for ciphertext)

- Test: `TestBughuntR14NodeNamesAForeignSnapshotAsParentAndReadsIt` (`internal/server/bughunt_r14_test.go:227`)
- Code: `snapshots.go:179` `recordTaken` (takes `ref.ParentID` as sent, `chainPos` only reads the parent's generation), `:294` `afterSnapshotStored` (a stored layer becomes the head of the node's own sandbox), `nodes.go:1394` `mayFetch` (third rule: "in the chain of the head of a sandbox placed on the node").
- Observed: alice's sandbox is stopped, snapshot V is stored. Mallory's node, hosting only mallory's sandbox X, gets 404 for `GET /node/v1/blobs/snapshot/V` (control). It then reports a layer `{sandboxId: X, parentId: V}` (`POST /node/v1/snapshots`) and uploads it (`PUT /node/v1/blobs/snapshot/<id>`); the layer becomes X's head, and the same GET now answers **200**.
- Expected: 404. The secure layer's grants exist to stop a node from reading disks it was not given ("Any node token used to read any sandbox's disk").
- Cause: the third `mayFetch` rule trusts the chain of a head, and the chain is node-supplied. `recordTaken` should refuse a `parentId` that is not in the sandbox's own chain (or a layer of the same sandbox), and `mayFetch` should only grant chains the server built itself. It needs the victim's snapshot id; snapshot ids are not secret in this design (chain ids go to every node that boots a shared snapshot, `GET /node/v1/snapshots/{id}` prints a chain). v4 layers are ciphertext; older disks are plaintext. The same report with a made-up `sandboxId` of a sandbox on another node records a layer as that sandbox's, deleted (`recordTaken`'s `sb.NodeID != nodeID` case), which is only noise.

### 3. A lost welcome burns the boot ticket: the VM powers off and the owner gets a tampering notice (stuck, low-medium)

- Test: `TestBughuntR14LostWelcomeBurnsTheBootTicket` (`internal/server/bughunt_r14_test.go:278`)
- Code: `guest_channel.go:433` (`redeemGuest` marks the ticket redeemed and stores the session hash), `:499` (`handleGuestConnect` writes the welcome afterwards and does nothing if the guest never got it).
- Observed: the guest's hello reaches the server and the connection drops before the answer arrives. The retry of the same ticket is refused with `ticket_used`.
- Expected: a retry of a redemption whose welcome was never delivered works (or at least is not reported as tampering).
- Cause: redemption is not retry-safe. `internal/guest/preinit_linux.go` `redeem` gives up on `ticket_used` (the VM powers off), the node's `reboot` request then gets 403 (unannounced), and the sandbox stays down until its owner runs `crowdvm recover`; meanwhile `security_ticket_reused` ("Possible tampering on a node") goes to the owner. A flaky node network between hello and welcome is enough, and so is a guest that gave up at its 15 s dial deadline while the server was slow. The ticket-reuse alarm is the point of the design, so the fix has to tell "the first redemption never completed" (nothing was written to the guest, or no yamux session was ever opened for the session hash) from "someone else redeemed it".

### 4. The egress proxy loses the reply of a program that half-closes its connection (wrong-result, low-medium)

- Test: `TestBughuntR14UpgradedConnHalfCloseLosesReply` (`internal/mux/bughunt_r14_test.go:22`)
- Code: `mux.go:112` `DialUpgrade` returns a `bufConn` (`:138`) that embeds `net.Conn` and has no `CloseWrite`; the server's `readerConn` (`passthrough.go:483`) has none either. `mux.Join` (`:157`) "closes both at once" for an end without `CloseWrite`. Users: `internal/guest/egress.go:219` `Join(c, up)`, `guest_egress.go:251` `Join(readerConn, out)`, `dialGuestTunnel` and the node tunnels.
- Observed: a program writes its request and calls `shutdown(SHUT_WR)` (`printf req | nc -N host port`; the shape of `TestBughuntR6JoinHalfCloseLosesReply`). The server's end reads the request to EOF and answers. The program receives nothing: `got ""`.
- Expected: `reply to req`.
- Cause: round 6 fixed `Join` for conns that have `CloseWrite` (plain TCP). Every upgraded conn (a yamux stream behind `HijackUpgrade`/`DialUpgrade`) lacks it, so with server egress (the default on channel nodes) half-closing clients get nothing, and the same holds for `crowdvm tunnel` and direct-port tunnels over a channel. On a yamux stream `Close` is a half-close, so `bufConn` and `readerConn` can offer `CloseWrite` as `stream.Close()`.

### 5. The page's signature does not cover the request body (security, low)

- Test: `TestBughuntR14SignedRequestBodyIsNotCovered` (`internal/node/bughunt_r14_test.go:21`)
- Code: `ui_session.go:98` `uiReqSig` (method, path+query, ms), `:105` `validRequest`; `ui.html` `sessHeaders`.
- Observed: the header the page sends for "Pause" (`POST /api/pause`, `{"paused":true}`) is accepted with the body `{"paused":false}` (200).
- Expected: 401. Round 13 says a squatter on the UI port "sees only requests it cannot turn into other actions"; with the body free it can turn any captured `POST /api/pause` or `/api/config` (every key the page may set: cpus, memory, schedule and so on) into another value, for 60 s, as soon as the real daemon is up.
- Cause: the signed string has no body hash. Fix: add `sha256(body)` to the signed string and to `ui.html` (the page already has `crypto.subtle`). A one-use `ms` nonce would also end the plain 60 s replay that round 13 accepted.

## Code only

- **`handleGuestReboot` ticket count and `expect`/`reboots`/`guestLastEvent` maps are never pruned** (`guest_channel.go:137`, `:542`): one entry per sandbox that ever connected, kept until the server restarts. Small, grows with sandbox count.
- **The channel capability is self-reported** (`guest_channel.go:280` `channelNode`: `n.Info.GuestChannel`): a node that reports `false` (a patched node, or an honest old one) gets the legacy spec with the full guest config, every env secret and the guest token, in `POST /v1/vms`. There is no server switch that refuses legacy nodes for new sandboxes (the design's `--secure`, section 5.5 / 8.3, does not exist); only encrypted chains are kept off them, and the guide says "trust the node owners". Not a defect of the code as written, a hole in what the secure layer promises. A `--require-guest-channel` that stops placement on nodes without it would close it for new sandboxes.
- **`redeemGuest` takes the disk kind from the guest** (`h.Disk`, `guest_channel.go:437`): a node that boots its own guest with a ticket (bug 1) reports `plain` and flips `sandboxes.disk`, which `Encrypted: sb.Disk == "luks"` placement and the snapshot rows copy. Low on its own, relevant once bug 1 is open.
- **Every DNS query leaves a transport behind** (`internal/guest/egress.go:249` `resolve`): `mux.Transport(sess)` makes a new `http.Transport` per query, and each keeps its yamux stream (and the server's connection goroutines) for `IdleConnTimeout`, 60 s. A build that resolves a thousand names a minute holds up to ~1000 streams per side. One shared transport per session fixes it. The server's `POST /v1/egress/dns` and `dnsUDP` also have no cap on concurrent queries (the 1024 flow cap counts TCP only), and `maxEgressFlows` is per channel connection (a reconnect resets the counter) with no server-wide cap.
- **`upstreamDNS` reads `/etc/resolv.conf` once per process** (`guest_egress.go`): a changed resolver (DHCP, VPN) needs a server restart.
- **A sandbox resolves the server's LAN names**: DNS goes to the server's own resolvers, so `intranet.corp` answers (the connect is then refused). Design choice; mention for the guide.
- **`exitDetail` reads "killed by signal: killed"** (`qemu.go`, `r.waitErr.Error()` is `signal: killed`): bugs-r13-opus 12 asked for "killed by SIGKILL". Cosmetic.
- **`image build` can fail at its last step** after 20 to 120 minutes (`cmd/crowdvm-node/image.go:62`): with a running daemon that did not answer the local page, `newDriver` refuses on the lock after the build. The image stays in `build/`; `image import` later works. Cosmetic.
- **A fixed 10-minute window for wrong device codes** (`device.go` `codeMisses`): up to 40 guesses across a window boundary and one account per 20; with 10^6 six-digit codes this is not practical. Noted only.

## What held

- **Round 13's lock** (`lock.go`): the refcount is taken under `heldMu`, a failed `lockDataDir` leaves no entry, `newDriver` releases its reference when the driver cannot be built and `Run`, `runCmd` and `config set` each hold their own; two spellings of one dir are the same key because every caller passes `cfg.DataDir`. The Windows lock fails immediately like the Unix one.
- **Page signing**: the signed path is `r.URL.RequestURI()` and the page signs the same string for `/api/status` and every POST; the timestamp window is checked on both sides; the nonce must be live and unexpired; `revoke` only deletes the nonce it is given.
- **Evacuation**: `failedEvacMoves` runs only when the node's evacuation has no sandbox left (an `error: snapshot_unavailable` sandbox is not in `onNodeStates`, so the evacuation cannot deadlock on its own waiting sandbox); the mark of a later evacuation does not match an earlier one; `newerSnapshotOn` stores the head only for a layer that is uploaded.
- **`requeueBoot`**: one retry (`Retried`), `Exclude` is the refusing node, and a second refusal falls through to the `no_ready_machine` failure; `tryPlace` drops the exclusion only for `evacuated` boots whose evacuation is over.
- **Saved-snapshot allowance**: `lockLimits` is held across check and insert only, released once (`sync.Once`) on every path, and no other path takes `limitMu` while holding the `saved:` lock.
- **Device codes**: approval, the `consumed` update and both token endpoints use guarded `UPDATE`s; the per-code lock is taken after the row is found and the row is read again under it.
- **SSH key migration 2**: the copy keeps every row, the new primary key admits the old data, the guard records only callers the handler lets through.
- **Guest files**: `openFileNB` refuses FIFOs and devices without blocking, directories pass through to the callers that handle them, `readCappedLine` ends a line on `ReadSlice`'s `ErrBufferFull` loop without holding more than the cap, `writeTarGz` opens before the header and pads or cuts to the header's size; a write error aborts the response.
- **`ClearAdopted`**: every non-warm boot of `crowdvm-guest` removes `adopted.json` first; a warm VM's agent restart and reboot still find it (the config drive stays `warm`, so `LoadAdopted` runs). Channel sandboxes never take a warm VM (`vms.go`: `spec.Guest.Channel == nil`), so their config never reaches `adopted.json`. No other file in the guest keeps the config; `channel.json` is in `/run`.
- **Netstack keepalive and `CloseGuestConns`**: keepalive is set before the connect and on accepted endpoints; `track`/`untrack` pair on every path of `Forward`; `watch` calls `CloseGuestConns` before `waitBoot`, so a new boot's connections are not closed.
- **`warmOverCapacityLocked`**: ends when the pool is empty, drops unready VMs first.
- **Egress address checks** (`egressBlocked`): zones, mapped, NAT64, 6to4, v4-compatible, Teredo, metadata, the server's networks and the allow list are applied in a sound order; a connection to the server's own API is allowed before the LAN check on purpose. `flows` is released on every path after the increment.
- **Channel auth**: a ticket is redeemed with a guarded `UPDATE ... AND redeemed_at IS NULL`; a session needs its stored hash; a new boot clears the session hash, so an old agent cannot reconnect into the new boot (and gets `ticket_invalid`, not a false reuse alert, because the row's ticket changed).
- **Disk key flow** (`ensureDiskKey`, `snapDiskCols`): the key follows the snapshot row through stop, resume, fork and shared or saved deploys; a base-image boot gets a new key; `encryptedChain` keeps the inspector and non-channel nodes away from `luks` heads.

## Not verified

- No real guest, QEMU, LUKS or iptables: bug 4 is shown at the `mux` level with a yamux pair, not through the guest's `proxyEgress` (it needs `SO_ORIGINAL_DST`). The guest's `announceReboot` trigger (`systemctl is-system-running` = `stopping` on a poweroff) is read from the code; the tests send the announcement over a channel client.
- Bug 2 needs the victim's snapshot id; I did not look for places where ids leak to other accounts' nodes beyond the chain and meta routes. Bug 3's drop is simulated by closing the TCP connection after the hello.
- The Windows lock and `lock_windows.go` were not run.
- `preinit`'s boot order, `overlayRoot`, `switchRoot`, `openData` and image v4 scripts were not read in depth.
- Egress under load (the 1024-flow cap, DNS fan-out, descriptor limits) was read, not run.
- `ui.html` ran in no browser; `crypto.subtle` behavior on non-secure origins (the page is served on `http://127.0.0.1`, a secure context) was not tried.

## Fixes

All 6 fixed; the tests in `internal/server/bughunt_r14_test.go`, `internal/mux/bughunt_r14_test.go` and `internal/node/bughunt_r14_test.go` pass (the lost-welcome test now sends the nonce a fixed guest sends; the body test sends the captured header with the squatter's body first, then checks a replay). Extra test: `internal/server/fix_r14_test.go`.

1. **Reboot tickets after a stop / for the next boot** (`guest_channel.go`): a new boot's ticket clears any announcement (`forgetExpected` in `issueGuestChannel`), and `handleGuestReboot` gives a ticket only to a running sandbox (404 `not_running` otherwise, so the node stops asking).
2. **Node-chosen parent** (`snapshots.go` `recordTaken`, `onDiskOf`): a reported layer's parent must be a layer of the same sandbox or in the chain it booted from (or of its head); otherwise the layer is not recorded.
3. **Lost welcome** (`proto.GuestHello.Nonce`, `guest/channel.go`, `guest_channel.go` migration 3 `hello_hash`): the guest sends a random per-boot nonce with its ticket (inside TLS, so its node never sees it); a retry with the same nonce gets a new session instead of `ticket_used`. Any other second use still raises the tampering notice.
4. **Half-close over upgraded conns** (`mux.CloseWrite`, `bufConn`, server `readerConn`): they half-close (a yamux stream's `Close`, a TCP/TLS `CloseWrite`), so `Join` delivers the reply.
5. **Body not signed** (`ui_session.go`, `ui.go`, `ui.html`): the signature covers the body's sha256, and each signature is accepted once (the page never repeats a millisecond).

Not fixed (code only): the never-pruned guest registry maps, the self-reported channel capability (a design gap; needs a server switch), the guest-reported disk kind, the DNS transport per query in `internal/guest/egress.go` (being reworked on another branch), the cached resolv.conf, `exitDetail`'s wording, the post-build lock refusal of `image build`, the device-code window.
