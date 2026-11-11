# Bug hunt r5 (sonnet)

2026-11-11. Audited the uncommitted master tree. Product code unchanged. Only unit-test fixtures were used: SQLite, httptest, fake node and guest, temp dirs. No cluster, VM or e2e test was started.

The node-transfer and CLI-config areas were audited by one sub-agent. I re-ran its six tests myself, and all six fail for the reasons below.

Tests:
- Server, in `internal/server/bughunt_r5_test.go` (external package). One zone-literal test sits in `internal/server/bughunt_r5_internal_test.go`, because it calls the unexported `guardedDial`.
- Node: `internal/node/bughunt_r5_test.go`.
- CLI: `internal/cli/bughunt_r5_test.go`.

Run each with `go test -count=1 -run <Name> ./internal/<pkg>/`. The server tests also fail under `-race`, with no race diagnostics.

## Bugs

### 1. The outbound guard does not check zoned IPv6 literals, so a redirect reaches loopback

- Severity: security.
- Test: `TestBughuntR5GuardedDialZonedLoopback` in `internal/server/bughunt_r5_internal_test.go`.
- Where: `internal/server/outbound_guard.go:96` (`guardedDial`). The redirect-following client is `ciHTTP` at `internal/server/feat_ci.go:172`.
- Observed: with `--webhook-allow-local` off, `guardedDial(ctx,"tcp","[::1%lo]:PORT")` connects to a loopback listener. The plain `[::1]:PORT` is refused.
- Expected: both forms are refused.
- Cause: the `Control` hook calls `net.ParseIP(host)` on the address Go hands it. `ParseIP` returns nil for any literal with a zone (`::1%lo`, `fe80::1%eth0`), so `blockedTarget` is skipped and the dial is allowed. `checkOutboundURL` (`:68`) also fails on `ParseIP`, but its DNS fallback strips the zone and blocks the address at save time. That check runs only on the first URL (`connect`, webhook or OTel save). `ciHTTP` follows redirects with the guarded dialer alone. A provider on any allowed address can answer `302 Location: http://[::1%25lo]:PORT/...` and reach the server's loopback services. This brings back the round-4 SSRF (`bugs-r4-opus.md` 2) for anything listening on `[::1]` or a link-local address. The first 200 body bytes come back in the `connect` error.
- Fix idea: parse the host with `netip.ParseAddr` and call `WithZone("")`, or take the IP from the `net.Conn` remote address.

### 2. Deleting an organization orphans its saved snapshots

- Severity: leak.
- Test: `TestBughuntR5OrgDeleteOrphansSavedSnapshots`.
- Where: `internal/server/orgs.go:365-379` (`handleDeleteOrg`). Org saved snapshots are inserted with `owner_id=<org id>` at `snapshots.go:959`.
- Observed: after `DELETE /orgs/{org}` returns 200, the `saved_snapshots` row is still there with `owner_id` set to the deleted org. No user can list, deploy or delete it: `GET /saved-snapshots` returns nothing.
- Expected: the org delete removes its saved snapshots, with their disk layers and share codes, or hands them to their creators.
- Cause: the transaction handles sandboxes, active orgs, API keys and the org row. There is no foreign key from `saved_snapshots` to `orgs`, so nothing cascades.
- Also: the snapshot layers stay pinned and are never collected. `chargeSavedExtras` (`feat_saved_allowance.go:88`) keeps counting the dead wallet and charges it daily once it is above the allowance.

### 3. Hiding a port does not close a websocket that was waiting for a starting sandbox

- Severity: security.
- Test: `TestBughuntR5HideDuringStartWaitDoesNotCutWebsocket`.
- Where: `internal/server/feat_host.go:688` (`WaitUsable` in `serve`) against `:749` (`h.track`). `cut` is at `:82`.
- Observed: a request passes the access check on a public port while the sandbox is `provisioned`, and waits there. `host hide` runs meanwhile, and its `cut` finds nothing tracked. When the sandbox turns ready, the websocket upgrade succeeds on the now-hidden port and echoes data.
- Expected: a hidden port never opens a new connection and cuts connections that were already admitted.
- Cause: `serve` loads the port record and checks it once, then may wait up to `StartWait` (60 s by default) before `track` registers the request. It does not re-load the record after the wait. This is the hole in the round-4 fix for `bugs-r4-opus` 5. The same window exists for token rotation and firewall changes.

### 4. The round-4 notice fix clears only `sandbox_error`; the hold warnings stay

- Severity: cosmetic.
- Test: `TestBughuntR5HoldWarningNoticeSurvivesRecovery`.
- Where: `internal/server/lifecycle_holds.go:59` (`clearSandboxErrorNotice`, `kind='sandbox_error'`). The notices are created at `:82-105` and in `lifecycle_notify.go`.
- Observed: a sandbox in error for 2 or more days gets a `forced_stop_warning` notice ("will be stopped in 1 day"). After the sandbox recovers to `ready`, or is deleted, the notice stays and is shown and mailed for a sandbox that is fine or gone.
- Expected: recovery and deletion remove every notice that describes the old state: `forced_stop_warning`, `forced_stop`, `stop_failing`, `stop_failing_warning`, `stop_forced`.
- Cause: the cleanup is hard-coded to one kind. `onSandboxError`, `EvSandboxDeleted` and state changes call only that query.

### 5. Usage billed after an organization was deleted lands on the deleted org's wallet

- Severity: wrong-result.
- Test: `TestBughuntR5OrgDeleteLeavesUnbilledRunOnDeletedWallet`.
- Where: `internal/server/orgs.go:369` (and `:506` for member removal). The billing loop is `credits.go:122` (`BillNow`).
- Observed: stop an org sandbox, delete the org, then run `BillNow`. A `usage` ledger row is written against the deleted org's wallet id, which is recreated as a negative wallet. The sandbox is now personal, and no personal wallet is charged.
- Expected: the usage is settled before the org is removed, or charged to the creator.
- Cause: `handleDeleteOrg` moves the sandboxes but keeps `sandbox_runs.wallet_id` on the org. `handleMovePersonal` updates the open runs (`acct_misc.go:252`), but deletion and member removal do not. The loss is at most one billing interval of usage per sandbox, plus a few ledger orphans.

### 6. OpenTelemetry keeps a CPU sample per sandbox forever

- Severity: leak. Proven in code, no test: `otelState` is private and the black-box tests cannot read it.
- Where: `internal/server/feat_otel.go:569` (`o.cpu[sb.ID] = cur`). It is the only write to the map, which is declared at `:92`. Nothing deletes from it.
- Cause: nothing removes the entry when the sandbox is deleted or stops. The map grows by one small entry per monitored sandbox ever sampled, for the life of the process. A resumed sandbox also compares its fresh `/proc/stat` against a stale entry. In that case `cur.total > prev.total` fails and the gauge falls back to the since-boot average until the next tick.

### 7. Node: one caller's cancelled context fails every caller sharing the download

- Severity: wrong-result.
- Test: `TestBughuntR5CancelledPullFailsJoinedPull` in `internal/node/bughunt_r5_test.go`.
- Where: `internal/node/transfer.go:39` (`c.err = fn()` in `once`).
- Observed: sandboxes A and B pull the same image at once. A's context is cancelled, for example because A was deleted. B, whose context is live, gets `download image ...: context canceled`, and its boot fails.
- Expected: B finishes the download or retries it.
- Cause: the first caller's context runs the shared download, and its error goes to every waiter. This affects `PullImage`, `pullSnapshot`, `UploadSnapshot` and `PushImage`.

### 8. Node: a waiter on a shared download ignores its own context

- Severity: stuck.
- Test: `TestBughuntR5JoinedPullIgnoresOwnContext`.
- Where: `internal/node/transfer.go:33` (`<-c.done`).
- Observed: a joined caller whose context expired after 200 ms was still blocked 3 s later.
- Expected: it returns `ctx.Err()` when its own context ends.
- Cause: the waiter blocks on the channel without a `select`. A deleted sandbox's boot can hang for the length of someone else's multi-GB transfer.

### 9. Node: partial downloads are never cleaned up

- Severity: leak.
- Test: `TestBughuntR5StalePartFilesRemovedOnStart`.
- Where: `internal/node/transfer.go:163` (`os.CreateTemp(dir, kind+"-"+id+"-*.part")`).
- Observed: `<data>/tmp/image-img1-123456.part` is still there after the node starts.
- Expected: stale `*.part` files are removed at start.
- Cause: nothing in `internal/node` or `cmd` ever sweeps that directory. A node killed mid-download keeps the partial image forever, and `dirSize` counts it against the disk budget.

### 10. CLI: a logout is undone by a command that was already running

- Severity: security.
- Test: `TestBughuntR5LogoutUndoneByConcurrentCommand` in `internal/cli/bughunt_r5_test.go`.
- Where: `internal/cli/app.go:371` (load once per run) and `:394-407` (`saveConfig`, whole-struct write).
- Observed: `crowdvm new` waits on its create call, `crowdvm logout` runs in another terminal, and `new` then saves. `config.json` again contains `apiKey "cvm_testkey"`.
- Expected: the key stays removed.
- Cause: the config is loaded once and written back whole on any later save (`setCurrent`, `setConversation`, `forgetSandbox`), with no lock and no re-read. The same lost update erases a newer login or another shell's `current`.

### 11. CLI: concurrent saves corrupt or fail on `config.json`

- Severity: data-loss.
- Test: `TestBughuntR5ConcurrentSaveConfig`.
- Where: `internal/cli/config.go:95` (fixed temp name `config.json.tmp`).
- Observed: four parallel saves per round over 200 rounds gave about 345 failures of the form `rename ...config.json.tmp: no such file`. In about 21 rounds the file was invalid JSON.
- Expected: every save succeeds and the file always parses.
- Cause: concurrent writers truncate and interleave the same temp file before the rename. A corrupt file is reported as "ignoring unreadable config" and treated as empty, so the next save wipes all profiles and credentials. `os.CreateTemp` plus a rename would fix the corruption.

### 12. CLI: `--api-url` / `CROWDVM_API_BASE` sends the saved key to any server

- Severity: security, partly by design.
- Test: `TestBughuntR5APIURLOverrideSendsSavedKeyElsewhere`.
- Where: `internal/cli/app.go:411` (`server()`) against `:417` (`apiKey()`).
- Observed: after logging in to server A, `crowdvm --api-url <B> list` sends `Authorization: Bearer <A's key>` to B.
- Expected: the saved key goes only to the server it was saved for. `config set server` already drops the key on a server change (`cmd_config.go:78`: "A key belongs to one server").
- Cause: flags and env override the server, but `apiKey()` returns the profile key without comparing `prof().Server`. A stray `CROWDVM_API_BASE`, for example from a project `.env`, leaks the key.

## Checked and held

- **Outbound guard:** `blockedTarget` blocks `::ffff:127.0.0.1`, `::ffff:169.254.169.254`, `0.0.0.0`, `::`, `100.100.100.200` and `fd00:ec2::254` through `ip.Equal` and `To4`. `localhost.`, `x.localhost` and names resolving to loopback are refused at save. Dials re-check the resolved address, so DNS rebinding between check and dial is stopped. `whClient` follows no redirects and uses no proxy. `ciHTTP` has no proxy and re-checks every redirect dial. Only the zone-literal hole in bug 1 was found.
- **AI proxy path validation:** `..` after `PathUnescape`, backslashes, `%2e%2e`, `%2F..%2F` and double-encoded dots are refused or contained, because `target.Path` is re-escaped on output. Only a mismatch of the escaped prefix for an encoded vendor name sends a wrong, contained path upstream (cosmetic).
- **`feat_host` `cut`/`track`:** the map key is the pointer of each `cancel`, so keys are unique. `done` deletes the key when the set is empty, so the live map does not leak. `cut` cancels under the lock. Hide and re-host close connections that were already tracked. The existing `TestHostPublicURLs` passes under `-race`. The gap is in bug 3.
- **History privacy:** `store` moves guest time onto server time through `ex.Now`. The `privateSince` and `privateSpans` classification is correct for share and unshare cycles. A sandbox moved into an org private gets no `privateSince`, which is read as "since 0", so all earlier history stays private.
- **OTel:** worker pool bounded to 8. `rms` is written under a mutex. Each destination's 10 s context and the 1 s budget per sandbox hold. The metrics goroutine ends with `ctx`, and `defer wg.Wait()` waits for it. `TestOpenTelemetry` and `TestR4TelemetrySlowSandboxIsolation` pass under `-race` with no diagnostic. The leak is in bug 6.
- **Service-account deletion and orphan delete:** `handleDeleteServiceAccount` refuses while any undeleted sandbox remains, stopped ones included. `authorizeSandbox` lets server admins and org owners and admins delete orphans, and ordinary members cannot.
- **Secret-file validation:** the path is cleaned before the home-directory list check, so `./.ssh`, `a/../.ssh`, `.ssh/`, `~/.ssh` and `/home/user/.ssh` are all refused. Duplicate and prefix conflicts are checked on the cleaned path. All three callers reach `envValidateFiles`.
- **Tracer containers:** quiet containers are checked against `docker ps --no-trunc` and kept when the daemon is unavailable. Short and full ids both match. Rotated logs reset the offset.
- **Org delete balance check:** refuses a positive balance and compares `round6`. A negative balance (debt) is not refused, so the debt vanishes with the org. I treated that as a policy question, as `bugs-r4-opus.md` did, and did not count it.
- **Scheduler and placement:** `reservedOn` counts every state that holds a machine, including `archiving`. `placeSandbox` assigns the node and state under `schedMu`. Pinned placement matches by id or name, and `fits` checks CPU, memory, disk and `MaxSandboxes`. Failed boots go back to `abortBoot` and free their reservation, because the state leaves the reserving set. Gap, not proven: `placeSandbox`'s `UPDATE` has no `state` or `deleted_at` guard (`scheduler.go:296`), so a delete or stop that lands between `tryPlace`'s checks and the update could be overwritten. The window is too narrow to reproduce deterministically.
- **Billing:** `BillNow` bills `min(now, ended_at)` and finishes the tail of closed runs on a later pass, so there is no double billing. `openRun` closes any open run before inserting. Every terminal transition I traced (stop, error, node offline, delete, cancel) closes its run. Concurrent `BillNow` was not tested, because production runs it from one loop.
- **Saved snapshots:** the save/update race (`finish` against upload completion) is idempotent. A failed re-save keeps the last good disk. Share codes check `status='active'`. `savedManage` allows the saver or an org owner or admin. A deploy from a share or another member's snapshot gets the foreign flag, which scrubs credentials. Not proven: the allowance check is not atomic across different names, so concurrent saves can exceed it (only affects empty wallets with enforcement on).
- **Node transfer:** the sha256 check removes corrupt temp files. A 410 on upload stops retries. Parents upload before children. The retry loop sleeps after the final failed attempt, which delays `snapshot.failed` by 32 s (no test, since proving it takes more than a minute of real sleeps).
- **CLI storage:** the config dir is 0700 and the file 0600 (temp file plus rename). `update-check.json` is 0600. The key never appears in `--json` output, the `config` view, errors or process arguments (`--key-stdin` and `--password-stdin` exist). `logout` clears the key and warns when `CROWDVM_API_KEY` is still set. `CROWDVM_API_KEY` outranks the profile key.
- **`guest/desktop.go` `listening` and `browser.go`:** `/proc/net/tcp` and `tcp6` parsing is correct for `0A` (LISTEN) and zero-padded ports, and falls back to a connect only when `/proc` is missing. `browser.go` uses it without probing the port. Not audited exhaustively: the browser isolation shell script.
- **Not audited:** the CI end-to-end job flow. In code only: an org delete leaves `ci_links.org_id` pointing at the deleted org, so later jobs of an active link fail with "Could not create the sandbox: ..." (`feat_ci.go:639`). No test was written for it.

**TL;DR:** 12 bugs, each with a failing test except bug 6 (a code-only leak). Four are security: the zoned-IPv6 guard bypass that revives the CI SSRF through redirects, a hidden port still opening websockets while the sandbox starts, the CLI logout that a running command undoes, and the saved CLI key that `--api-url` sends to any server. The rest: orphaned org saved snapshots, usage billed to a deleted org wallet, stale hold notices, three node-transfer bugs, and corrupted CLI config under concurrent writes.

## Fixes

1. **Logout undone by a concurrent command** (`internal/cli/app.go`, `config.go`): `App.saveConfig` now keeps the config as loaded (`cfgBase`), and under a lock file re-reads `config.json` and applies only the fields this command changed (`merge3`). Untouched fields keep the file's current value, so a logout done meanwhile stays.
2. **`--api-url` / `CROWDVM_API_BASE` sent the saved key elsewhere** (`app.go`): the saved key is used only when the server in effect equals the profile's server (`sameServer`: scheme, host, port, trailing `/api/v1`). An explicit `CROWDVM_API_KEY` still applies to any URL. The test needed no change.
3. **Concurrent `saveConfig`** (`config.go`): `updateConfig` takes the lock, writes a unique `os.CreateTemp` file in the same directory and renames it. A corrupt or unreadable `config.json` makes the save fail with an error instead of being replaced.
4. **Cancelled or ignored context on joined pulls** (`internal/node/transfer.go`): `once` runs the shared transfer on a context detached from the first caller, counts waiters and cancels the transfer only when the last one gives up. Each waiter returns when its own context ends.
5. **Stale `*.part` files** (`transfer.go`, `node.go`): `Node.Run` calls `removePartFiles` on start and deletes `<data>/tmp/*.part`.
