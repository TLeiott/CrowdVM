# Bug hunt r18 (sonnet)

2026-11-12. Product code unchanged. I only added `internal/server/bughunt_r18_test.go` and this report. Unit tests only (the server's `servertest` environment with a fake node, a fake guest and raw guest-channel clients). No cluster, VM, e2e test, download or real `cryptsetup`. I did the work myself, no sub-agents. Tree: `main` at `557c7cf`, in a detached worktree.

## What I audited

- **Round 17's fixes (`3073998`)**
  - `feat_ai_proxy.go`: the per-call `orgCanUse(p.Access, p.OwnerID, sb.OwnerID)` has the same arguments as `aiSandboxProfile`; a deleted profile answers 403, not a panic.
  - `cmd_access.go` / `config.go` `lockFile`, `setKnownHost`: lock, temp file, `Chmod`, `Rename`; a failed lock only turns the pin off (`accept-new`). Cross-built the CLI, node and server for windows/amd64 and darwin/arm64 (`go build`): fine.
  - `feat_history.go` `agentEvents` (`ORDER BY ts, rowid`): the upsert keeps the rowid, the table is a rowid table.
  - `acct_prompt.go` `applyPromptDefaults`: **regression, bug 3**.
  - `agents/preflight.go` / `http.go` (`attachPath`, `readInline`), `agents/store.go` `load`, `agents/agents.go` `ProxyEnv` + the server's `aiProxiedVendors` (vendor names match `proxyVendors`; live apply reaches `Manager.SetAIProxy` and the env files).
  - `acct_ai_refresh.go` (matches the renewed account by refresh token), `guest_egress.go` `stallConn` and `mux.Join` (every user: `guest/egress.go`, `guest/internal.go`, `node/api.go`, `node/direct.go`, `server/passthrough.go`, `feat_desktop.go`, `guest_egress.go`).
  - `acct_sshkeys.go` (`revokeMemberKeys`, the changed `revokeTeammateKeys` on shared, private and personal sandboxes): **bug 2**.
  - `orgs.go` (invite claim, service accounts), `credits.go` (scope), `proto.AIProxy.Vendors`, `scheduler.go` `diskSpec` / `reservedOn` (**bug 4**), `client/types.go` `PublicAccess`, `cli/app.go` error and exit handling.
- **The opt-in disk key for copies (`5cc82fa`, `557c7cf`)**: `ensureDiskKey`, `diskRekeyed`, `rekey_from` in `sandboxes`, `guest_channels` and `snapshots` (`snapDiskCols`), `redeemGuest` (both keys on a ticket and on a nonce retry), `POST /v1/rekeyed`, `placeSandbox`/`pickNode` (`Rekey`), `handleFork` / `handleCreateSandbox`, `guest/channel.go` (`ConfirmRekeyed`, `TakeOldDataKey`), `guest/preinit_linux.go` `rekeyData` (resume, repair, passphrase change, cut-offs), the CLI flag and the SDK fields. **Bug 1.**
- **Self-update and compatibility gating (`9ffe0fe..868c311`)**: `internal/node/update.go` (download, size and sha256, `binaryVersion`, `.old` hard links, rollback), `update_pending.go`, `autoupdate.go`, `lock*.go`, `service/service_linux.go` `Refresh`, `ui.go`/`ui.html` update routes, `cmd/crowdvm-node/update.go`, `internal/version`, `internal/compat`, `internal/server/compat.go`, `feat_install.go` (`/downloads`, manifest, `shaCache`), `nodes.go` / `nodes_evacuate.go` (outdated evacuations), `scheduler.go` (**bug 5**), `client/compat.go`, `cli/compat.go`, both SDKs' headers, `docs/guide/updates.md`.

## How to run

```
go test -count=1 -run TestBughuntR18 ./internal/server/
```

The 5 tests fail today (the first takes about 30 s: the fake guest never answers the stop). `go vet ./internal/server/` and `gofmt -l` are clean. I ran nothing else of `./internal/server/`; the new tests use only existing helpers (`createSandbox`, `rawChannel`, `promptGuest`, `setGen`).

## Bugs proven by a failing test

### 1. `newDiskKey` is silently ignored for a copy of a copy that still waits for its own key (security, medium)

- Test: `TestBughuntR18NewDiskKeyOfACopyOfAPendingCopy` (`internal/server/bughunt_r18_test.go:22`)
- Code: `internal/server/guest_channel.go:114` (`ensureDiskKey`, `case sealed != "" && newKey && old == "" && ...`).
- Observed: source `src` (encrypted), saved snapshot `tpl`, copy `cp` made with `newDiskKey` (its first boot gets a new key `K2` and `src`'s key as the old one, as designed). `cp` stops before its guest confirmed (stop layer: `data_key=K2`, `rekey_from=K_src`). A fork of `cp` made with `newDiskKey: true` boots with `DataKey == K2`, `cp`'s own key, and the old key `K_src` pending. It did not get a key of its own.
- Expected: the fork gets a third key `K3`, with `K_src` (the key the parent snapshot still opens with) as the old one.
- Cause: a new key is only handed out when the parent snapshot has no pending old key (`old == ""`). A snapshot of a copy that has not confirmed yet always carries one (`snapDiskCols` copies `rekey_from`), and a periodic snapshot is taken during the 2 to 3 minutes the re-encryption runs, so this is the normal state of a copy's first minutes. The flag is then dropped without a word, and the new copy shares `cp`'s key. The owner of the fork, or whoever runs a node for it, can read `cp`'s later writes: exactly bugs-r14-opus 1, for the flag's own users. Also, a recipient who deploys a shared snapshot of such a copy gets both the copy's key and `K_src` in the guest. The test uses `cp`'s stop layer; a live fork of a running `cp` takes a live snapshot with the same row. Fix: `old` from the parent snapshot is the key that the volume in that snapshot opens with; keep it and still generate the new key (`old` stays, `sealed` becomes new) instead of requiring `old == ""`.

### 2. A member who leaves (or is removed) leaves their teammates' SSH keys in the sandboxes that go back to personal billing (security, medium)

- Test: `TestBughuntR18LeaverSandboxKeepsTeammateKeys` (`internal/server/bughunt_r18_test.go:82`)
- Code: `internal/server/acct_sshkeys.go:110` (`revokeMemberKeys`: `b.owner_id!=?` selects only sandboxes of others), `internal/server/orgs.go:588` (`handleRemoveMember` moves the leaver's sandboxes to `org_id=NULL, visibility=NULL`).
- Observed: org `acmer18`, `carol` creates a shared org sandbox, `dave` sends it his SSH key (row in `sandbox_ssh_keys`, line in `authorized_keys`). The sandbox is stopped, `carol` leaves (`DELETE .../members/me`), resumes it: `dave`'s key is still in `~/.ssh/authorized_keys` and in `sandbox_ssh_keys`, and no `sshKeysRevokePending` marker was set.
- Expected: the sandbox is personal now, a teammate has no right to it any more (the API answers `dave` 404), and unsharing it (ORG-012) deletes teammates' keys; leaving must do the same for the leaver's own sandboxes (`revokeTeammateKeys` would do it for a personal sandbox: `stale` then matches every non-owner row).
- Cause: round 17 revoked the key *of* the departing member in other people's sandboxes, but not the keys of everyone else in the departing member's sandboxes, which change from shared to personal in the same transaction. The key stays reachable through a direct port or another sandbox on the same network, as in bugs-r17-opus 2. Same for an owner removing a member (`handleRemoveMember` is the only path). Fix: in `revokeMemberKeys` also mark every sandbox with `owner_id=userID` that had `org_id=orgID` (collect the ids before the UPDATE) and run `revokeTeammateKeys`.

### 3. Continuing a conversation by id is refused for a Codex-only user since round 17 (wrong-result, medium; regression)

- Test: `TestBughuntR18ContinuedConversationPreflightIgnoresAccountDefault` (`internal/server/bughunt_r18_test.go:132`)
- Code: `internal/server/acct_prompt.go:130` (`handlePrompt` calls `effectiveProvider` and `promptConfigured` after `applyPromptDefaults`), `acct_prompt.go:179` (`effectiveProvider`: no provider means the sandbox default or `"claude"`).
- Observed: a profile with an OpenAI key only, account default provider `codex`. `POST .../prompt {"prompt":"start"}` works (202). `POST .../prompt {"prompt":"go on","conversationId":"conv-started-with-codex"}` answers `409 provider_not_configured: Prompting is locked until Claude Code is configured`.
- Expected: 202. Before round 17 the account default was written into the body, so the pre-flight checked Codex.
- Cause: the fix for bugs-r17-sonnet 4 stops `applyPromptDefaults` for a prompt that names a conversation, so the guest keeps the conversation's harness, but the server's own pre-flight still derives the provider from the body alone and falls back to Claude. Everyone who works with one non-Claude harness and continues conversations by id (SDK users, `crowdvm prompt --conversation`) is locked out until they also configure Claude. Fix: skip the credential pre-flight when `conversationId` is set and no provider is given (the guest knows the harness and reports the real error), or look the conversation's harness up.

### 4. A fork or copy of a sandbox that grew keeps the grown disk, but the API, the scheduler and the node count the smaller one (wrong-result, low)

- Test: `TestBughuntR18ForkOfGrownSandboxReportsItsRealDisk` (`internal/server/bughunt_r18_test.go:160`)
- Code: `internal/server/scheduler.go:83` (`diskSpec`, reads only the sandbox's own `extra.diskGiB`), `internal/server/lifecycle.go:440` (`boot` records and sends it), `internal/server/sandboxes.go:245`; the node side `internal/node/vm/qemu/qemu.go:366` (`size := max(want, base.vsize)`).
- Observed: `src` is `small` and has `extra.diskGiB = 123` (it once ran at a larger type). A fork of it, made at `small`, reports `storage.diskGB = 51`, is sent to its node with `Size.DiskGiB = 51` (the node's usage counts that) and records `diskGiB = 51`.
- Expected: at least 123 for all three. The node never shrinks the disk of any parent, for a fork, a deploy of a saved snapshot and a resume alike, so the copy boots on 123 GiB.
- Cause: round 17 made the record per sandbox, but a new sandbox starts without one. The sandbox's real disk is a property of the parent snapshot's chain. The scheduler can overcommit a node by the difference for each such copy; the user sees a wrong size (the same defect bugs-r17-opus 9 fixed for the same sandbox). Fix: copy `extra.diskGiB` from the source into the fork and into a deploy (or record the chain's virtual size in the snapshot row).

### 5. A paused current node counts as "a current node is online", so with one outdated node the pool takes no sandboxes at all (stuck, low)

- Test: `TestBughuntR18PausedCurrentNodeDoesNotCountAsCurrent` (`internal/server/bughunt_r18_test.go:201`)
- Code: `internal/server/compat.go:147` (`currentNodeOnline`: online, not `Disabled`, not outdated; no check of `Info.Paused`), used by `pickNode` (`scheduler.go`, `haveCurrent`), `sizeAvailability`, `outdatedDue`, `slideOutdated`.
- Observed: two nodes, one outdated (generation 0), one current but paused by its owner. A create stays `provisioning`, `waitingForCapacity: true`, for as long as the node is paused: the outdated node is skipped ("misses a required update") and the paused one refuses.
- Expected: while no current node can take a sandbox, outdated nodes keep serving, as the docs promise ("while no online node is current, outdated nodes keep taking sandboxes and their grace period does not start"). The same check also lets the outdated node's evacuation deadline run out while its sandboxes have nowhere to go.
- Cause: a drained node is excluded but a paused one is not, and `couldEverPlace`-style waiting hides it. Fix: count only nodes that are not paused (`n.Info.Paused`), and arguably only those with capacity.

## Code only

- **A node that boots offline three times after an update is rolled back and the version is skipped for good** (`internal/node/update_pending.go:58-67`, `CheckUpdateStart`; stuck, low). The start counter rises on every daemon start until the first heartbeat; a laptop or a machine that comes up before its network (three reboots, or `Restart=always` after a crash unrelated to the update) is rolled back, `update-skip.json` then keeps auto-update from installing that release ("rolled back here after it failed"), and if the release was a required one the node stays outdated until its grace ends. A "start that reached the server" is the wrong signal; counting only starts that died before `Run` got going, or a long-enough run, would not punish a missing network. Not tested: it asserts the documented rule.
- **Stale-lock takeover in `lockFile` is not guarded** (`internal/cli/config.go:181`): `owned(string(b))` compares the file with the string just read from it, so it is always true. When two processes find the same stale lock, the slower one can read the *new* owner's token after the faster one re-created the lock and then delete a live lock; two writers then hold it. Pre-existing for `config.lock`, now also for `known_hosts.lock`. Needs a killed holder and a race of microseconds.
- **`mux.Join` ends the other direction at once when a write fails** (`internal/mux/mux.go:251`). A server that answers early and closes (413, 401 on a large upload) used to get its reply delivered (the failed write only half-closed); now a failed `a→b` write closes `a` while the reply may still be in flight, so the client sees a reset instead of the status. Whether the reply is already read depends on the kernel and the timing, so I did not write a deterministic test.
- **Snapshots recorded late carry the key state of the record time, not of the snapshot time** (`internal/server/snapshots.go:74` `snapDiskCols`). A periodic snapshot taken during a copy's re-encryption but recorded after the confirmation gets `rekey_from = ''`; its disk opens only with the old key, so a later fork or resume from it sends one key that does not open it. The node reports snapshots right after taking them, so the window is small. The record should use the sandbox's state when the snapshot was taken.
- **`crowdvm desktop ID` now makes a public desktop private for everyone** (`internal/cli/cmd_access.go:803`). Round 17 sends `publicAccess:false` whenever `--public` is not given, and the server lets anyone with desktop access and more than view access set it, so a teammate who only opens the desktop ends the owner's public demo link. Intended by the fix; mentioned because the effect reaches other users.
- **Zero data retention keeps earlier events in memory** (`internal/guest/agents/store.go:75`): `load` truncates `events.jsonl` after reading it into `m.events`; the earlier prompts stay served by `/v1/events`, `/v1/prompts` and `/v1/conversations` until the agent restarts. Round 17 fixed the file only.
- **`service.Refresh` writes the unit before `daemon-reload`** (`internal/node/service/service_linux.go`): if `systemctl --user daemon-reload` fails, the unit on disk is current, so the next start finds nothing to change and systemd keeps the old definition until something else reloads.

## What held

- **AI proxy**: the profile check uses the same arguments as `aiSandboxProfile`; live applies repoint the proxy and its vendor list; vendor names on both sides match.
- **Disk key**: the re-keyed copy keeps both keys on every ticket and on a nonce retry until the confirmation; a repeated or wrong confirmation answers 204 / 409; a stop layer, a reboot and a resume from a snapshot taken during the pending phase return the same pair; unflagged copies and plain disks are untouched; a copy that waits for its key is placed only on `DiskRekey` nodes (`placeSandbox` reads `rekey_from` / `newDiskKey` from the row). `rekey_from` in a snapshot taken during the pending phase is correct for that snapshot for ever (its blocks need the old key), so it is not a leak. `rekeyData`: each cut-off case (reencrypt, repair + resume, the passphrase change, both passphrases open) lands on a bootable state by reading the code; not run.
- **Self-update**: size-limited download, sha256 over the written file, `Sync` before the swap, `binaryVersion` of the new file before any rename, hard-linked `.old` so the name exists at every moment, rollback swaps back and skips the version, `.old` of untouched files is removed so a rollback returns to one state, a lock file against concurrent updates, `os.Executable()` after a swap still points to the (new) name (checked with a real binary on this machine: no "(deleted)"), the unit refresh replaces only units that carry the managed line or equal the old template. `TrustedUpdateServer` rejects `localhost.evil.com`, userinfo tricks and non-Tailscale 100.x names. `version.Compare` orders releases, nightlies, rc and describe builds as documented.
- **Compatibility gating**: `/downloads` is outside `/api/` and never gated, so an outdated CLI can still update; a node without the header counts as generation 0; nodes below `Min` get 426 with the update command; evacuations for outdated nodes slide while no current node is online and cannot be cancelled by the owner; owner-started evacuations on such a node keep their deadline (MIN) and end with the update by design (the test of round 16's author covers it).
- **Clients**: both SDKs send the generation header and warn once, never as an exception; browsers are not judged; the Go client sets it on every request.

## Not verified

- A real `cryptsetup reencrypt` run (interruption at every step, the `luksChangeKey` flags, the `--resume-only` of a half-written hot zone), `fstrim` afterwards, and the time the first start needs against the node's boot timeout.
- Bug 1 with a live fork of a running copy (the test uses the copy's stop layer; both rows carry `rekey_from`), and with a shared (foreign) snapshot of such a copy.
- Bug 4's consequence on a real node (the `size := max(want, base.vsize)` line is read, not run).
- Windows: `os.Rename` onto a `known_hosts` that ssh holds open, the Windows service restart after an update.
- The TypeScript and Python SDK suites (no `node_modules`, nothing run); `scripts/dist.sh` and `ImportDownloads` end to end.
- Root-run daemons in a user-writable install directory (a race between the checksum and the rename); the node runs as a user unit by default, so I did not follow it further.

## Fixes

All 5 fixed; the tests in `internal/server/bughunt_r18_test.go` pass (test 1 accepts the refusal below).

1. **newDiskKey of a copy that is still re-keying** (`guest_channel.go` `errRekeyPending`, `sandboxes.go`, `lifecycle.go` `handleFork`): no copy (with or without the flag) is made from a sandbox or snapshot that still waits for its own key: 409 `disk_rekey_pending`, retryable. Its snapshot may open with either key, and a copy would carry the source's old key along.
2. **Teammates' keys in a leaver's sandboxes** (`orgs.go` `handleRemoveMember`): the leaver's own org sandboxes (all stopped at that point) are marked so that every non-owner key is revoked at their next start.
3. **Continued conversation refused** (`acct_prompt.go`): no credential pre-flight for a prompt that names a conversation and no provider; the guest uses the conversation's harness and reports a missing credential itself.
4. **Copies of a grown sandbox** (`lifecycle.go` fork, `sandboxes.go` create, `scheduler.go` `snapshotDiskGiB`): a fork or deploy starts with its source's recorded disk size.
5. **Paused current node** (`compat.go` `currentNodeOnline`): a paused node does not count as a current node that takes sandboxes.

Code only, also fixed:
- `mux.Join` closes both ends only after a write timeout (a stalled egress destination); other copy errors half-close as before, so an early answer (a 413 to a large upload) reaches the client.
- Zero data retention drops earlier events from memory at agent start, not only from the file.

Not fixed: the offline-boot rollback counter of the node update, the stale-lock takeover race in `lockFile` (pre-existing, microseconds), late-recorded snapshots during a re-key, a teammate making the owner's public desktop private (intended), `service.Refresh` without a retried `daemon-reload`.
