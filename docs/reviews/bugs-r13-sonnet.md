# Bug hunt r13 (sonnet)

2026-11-12. Product code unchanged; I only added `cmd/crowdvm-node/bughunt_r13_test.go`, `internal/server/bughunt_r13_test.go` and this report. Unit tests only (`servertest`, the fake node, a `drivers` map entry). No cluster, VM or e2e test. I did the work myself, no sub-agents.

## What I audited

- **Round 12's fixes (`dde46aa`)**: `internal/node/ui_session.go` and `ui.go` (nonce file, expiry and the 20-session cap, `revoke`, the HMAC, `/api/hello` as an oracle, `/login`), `cmd/crowdvm-node/daemon_hello.go` and `main.go` (`daemonBody`, `config set`, `runCmd`'s order of steps), `internal/node/lock*.go` and where `Run` takes the lock, `router.go` (per-user buckets only on `/api/`), `nodes.go` (`nodeNameMu`, `reportedNodeName`, all writers of `nodes.name`), `nodes_evacuate.go` (`failedEvacMoves`, `evacuatedFrom`, when `evacuateStep` calls them), and I read `qemu.New` / `adoptAll` / `gcOrphans`, which the second daemon runs.
- **New server areas**: `device.go` (CLI device login, agent identity and claim, `/oauth2/token`, `/oauth2/revoke`, the `/device` page), `acct_audit.go`, `acct_sshkeys.go`, `acct_guestcfg.go`, `acct_catalog.go`, `events.go` and its subscribers (audit, ssh keys, playbook, webhooks fan-out), `feat_saved_allowance.go` with its caller in `snapshots.go`, `feat_playbook.go`, `feat_install.go`.
- Pagination of `/audit-events` (25 rows with equal timestamps, both directions, page size 4): no row lost or repeated; that was a throw-away test, not kept.

## How to run

```
go test -count=1 -run TestBughuntR13 ./internal/server/ ./cmd/crowdvm-node/
```

All six server tests and the one node test fail today (the three parallel ones fail in every run I made: 8 of 8, 5 of 5, 3 of 3; each loops over several fresh wallets or codes inside the test). `go vet ./internal/server/ ./cmd/crowdvm-node/` is clean. With `-skip TestBughuntR13`, `cmd/crowdvm-node` passes, and so do the server tests matching `Evac|Saved|Device|Audit|ZDR|Agent|Oauth|Auth|Node` (40 s). I did not run the whole server package.

## Bugs proven by a failing test

### 1. The data-dir lock is taken after the VM driver already adopted, killed and garbage-collected VMs (data-loss, medium)

- Test: `TestBughuntR13SecondDaemonBuildsDriverBeforeLockRefuses` (`cmd/crowdvm-node/bughunt_r13_test.go:27`)
- Code: `cmd/crowdvm-node/main.go` `runCmd` → `daemon()`: `newDriver(cfg, log)` comes first, `n.Run(ctx)` (which calls `lockDataDir`, `internal/node/node.go:222`) comes last.
- Observed: with `<data>/node.lock` held by "the first daemon", `crowdvm-node run` still calls the driver constructor, and only then returns the lock error (the test's constructor returns `probe: driver constructed`, which is what `run` returns).
- Expected: the second daemon builds nothing: the error is "another crowdvm-node daemon is running" and the constructor is never called.
- Cause: the r12 fix put the lock "before adopting VMs", but `qemu.New` (`internal/node/vm/qemu/qemu.go:~215`) already does `d.adoptAll()` and `st.gcOrphans(true)`. `adoptAll` dials each VM's QMP socket (one client at a time, and the first daemon holds it), and when the dial fails for a VM it believes is "ours" it **kills the qemu process** ("alive but unreachable: hung"), and it `os.RemoveAll`s a VM dir whose `vm.json` it cannot read (for example while the first daemon is writing it). Then the deferred `drv.Close()` closes the second driver's netstack for the VMs it adopted. So the case r12 wanted to rule out (a service restart overlapping an old process, `crowdvm-node run` next to the installed service) can still kill running sandboxes, just not through the server session. Same ordering problem, smaller: `run --cpus 8` saves `config.json` (`main.go:212`) before the lock is tried. Fix: take the lock in `runCmd` before `newDriver` (and before the `cfg.Save()` of flags), and keep the check in `Run` or drop it.
- I could not test the kill itself without qemu; the test proves the order.

### 2. `sshKeyGuard` records a key for every caller, before any access check (security, low-medium; leak)

- Test: `TestBughuntR13SSHKeyRecordedForCallerWithoutAccess` (`internal/server/bughunt_r13_test.go:31`)
- Code: `acct_sshkeys.go:48` `sshKeyGuard` (in `Hooks.Guard`, which `wrap` runs after the scope check and before the handler's `c.Sandbox("ssh", false)`).
- Observed: a user with no access to a sandbox POSTs `/sandboxes/<id>/sshkey` with a key, gets 404, and `sandbox_ssh_keys` has a row `(<id>, <outsider>, <key>)`. The same for an id that does not exist.
- Expected: no row unless the call was allowed (404, 403 viewer, 409 not ready and so on leave nothing).
- Cause: the guard writes first and the handler decides later. Rows for ids that never exist are never deleted (`EvSandboxDeleted` only cleans real ids), so any authenticated account can grow the table without limit. Rows for a real sandbox put the outsider's name on keys, which feeds bug 3.

### 3. The first account that sends a key owns it, so the creator's own key can be removed on unshare (wrong-result, low-medium; lockout)

- Test: `TestBughuntR13SSHKeySentFirstByTeammateIsRemovedFromCreatorOnUnshare` (`internal/server/bughunt_r13_test.go:60`)
- Code: `acct_sshkeys.go:63` (`ON CONFLICT(sandbox_id,key) DO NOTHING`, primary key `(sandbox_id,key)`) and `revokeTeammateKeys`.
- Observed: bob (teammate) sends key K, then alice (creator) sends K and also has it in `authorized_keys`. After `unshare` the file no longer holds K: alice's own key was removed. With alice sending it first it stays (I ran that control).
- Expected: "a key the creator also sent stays", whoever was first.
- Cause: only one row per `(sandbox, key)`, attributed to the first sender, so the creator's row never exists. With bug 2 an outsider can do the same without any access, by sending the creator's public key (public on GitHub, usually) in advance. Fix: key the table by `(sandbox_id, user_id, key)`, or let the creator's insert take over the row.

### 4. A key restricted to some sandboxes reads the whole audit log (security, low-medium)

- Test: `TestBughuntR13RestrictedKeyReadsWholeAuditLog` (`internal/server/bughunt_r13_test.go:103`)
- Code: `acct_audit.go` `handleAuditEvents` (`account.read`, filter `actor_id = user`).
- Observed: a `read-only` key limited to sandbox A (`sandboxIds`) lists the `sandbox.created` record of sandbox B (and, by the same query, every request record of the user: route, ip, key ids, sandbox ids).
- Expected: 403 `api_key_restricted` (like `handleKeyUsage`, and `keysJSON` returns nothing), or only records whose target the key may see.
- Cause: no `Restricted()` or `OrgID` handling. An org-scoped key (`Key.OrgID`) has the same hole: it gets the user's personal events and those of other orgs (not tested).

### 5. Parallel saves of new names exceed an empty wallet's allowance (wrong-result, low)

- Test: `TestBughuntR13ParallelSavesExceedAllowanceOfEmptyWallet` (`internal/server/bughunt_r13_test.go:140`)
- Code: `snapshots.go:951-966` `handleSaveSaved`: `checkSavedAllowance(wallet)` and the `INSERT` are separate, the only lock is `saved:<wallet>/<name>`.
- Observed: wallet with 1 included snapshot, no credits, credit enforcement on: 12 parallel `POST /saved-snapshots` with different names return 202 several times (every round of 10).
- Expected: one 202, the rest 402 `credit_required`.
- Cause: same shape as the r10 start limits; `lockLimits()` (or a lock per wallet around check and insert) closes it.

### 6. Parallel approvals of one device code each mint an API key (leak, low)

- Test: `TestBughuntR13ParallelDeviceApprovalsMintOneKeyEach` (`internal/server/bughunt_r13_test.go:223`)
- Code: `device.go` `handleDeviceApprove`: the status check, `createKey`, then `UPDATE device_codes SET status=...  WHERE id=?` with no `AND status='pending'`.
- Observed: 12 parallel approvals of one agent claim code: several answer 200 and each leaves a live 90-day key `agent: <name>` in the approver's account (3 to 5 per round).
- Expected: one approval wins, the others get 404 `invalid_code`; one key.
- Cause: read-check-write without a guarded update. Only the last key's secret reaches the agent; the others are valid and delivered to nobody (a double click or a retrying client leaves them). Same pattern in `handleOAuthToken` (see Code only).

### 7. A stale `evacuatedFrom` marker lets an unrelated evacuation boot an errored sandbox (wrong-result, low)

- Test: `TestBughuntR13StaleEvacuatedFromMarkerResumesUnrelatedErroredSandbox` (`internal/server/bughunt_r13_test.go:182`)
- Code: `nodes_evacuate.go` `evacuationResume` (sets `extra.evacuatedFrom`, nothing clears it) and `failedEvacMoves` (matches every `error`/`snapshot_unavailable` sandbox with that marker).
- Observed: a sandbox moved off node A by a real evacuation (marker set, sandbox ready on B) later goes to `error: snapshot_unavailable` for its own reason (I set it in the DB). A second evacuation of A, which holds no sandbox, ends at once in `evacuateStep` and boots it (`state provisioning`, `pendingBoot` set; in the test the boot then fails again because the VM still exists on B).
- Expected: only moves of this evacuation are retried: the sandbox stays in `error`.
- Cause: the marker names the node, not the evacuation, and lives for ever. Fix: store the evacuation's `started_at` (or clear the marker when the boot succeeds or the sandbox leaves `error`) and match on it. Users and nodes cannot write `extra.evacuatedFrom` (only `evacuationResume` does), so this is not an attack, just a resume nobody asked for (it also sends the owner an "evacuation stranded" notice if no other node fits).

## Code only

- **`handleOAuthToken` and `handleDevicePoll` hand the token out twice under parallel polls** (`device.go`): read `approved` + `Token`, then `UPDATE ... status='consumed'`. Two polls both get the key (or two CLI sessions). Only the device-code holder can poll, so it is a hygiene issue; a guarded `UPDATE ... WHERE status='approved'` and checking `RowsAffected` fixes it.
- **The approved agent key sits in `device_codes.token` in clear text** until it is polled (or up to expiry + 1 h, `cleanupAuth`). Every other secret is hashed. An agent that never polls leaves a usable 90-day key in the database.
- **Agent user codes are 6 digits, no attempt counter** (`device.go` `newDevice`): 10^6 codes for 10 minutes, bounded only by the IP bucket. Any signed-in user can approve any claim code (the key is minted in the approver's account and delivered to the agent), and `GET /auth/device/lookup` returns the `email` of a pending email request. Needs a per-code failure count or a longer code.
- **Email-method agent registration is a mail relay** (`handleAgentIdentity`): anyone can make the server mail any address a message that contains the attacker's `name`, limited only by the IP bucket.
- **Catalog validation** (`acct_catalog.go` `handleAdminCatalogPut`): aliases are not checked for duplicates, nor against other providers' ids, and provider ids are not lower-cased although `catalogProvider` lower-cases the lookup. An admin entry `"Claude"` can never be found; an alias equal to another provider's id shadows it (first in the list wins). Admin-only.
- **Admin-set `savedSnapshotAllowance` is applied at the daily tick only**: the charge uses the count at the first tick after UTC midnight, so extras deleted before it and made again after it are never billed. By the "once per UTC day" design, noted for completeness.
- **A node never gets its name back** (`nodes.go` `reportedNodeName`): node B registered as `laptop-2` because A had `laptop`; when A is deleted, B keeps `laptop-2` since its reported name never changes (before r12 each heartbeat re-evaluated it). Cosmetic.
- **`config set` against a daemon the CLI cannot verify** (hello timeout of 3 s on a loaded node, an old daemon without `/api/hello`, a `ui-token` the CLI user cannot read) writes `config.json` from the CLI's copy while the daemon runs: a stale whole-file rewrite, with a `config.json.tmp` name shared with the daemon's own `Save` (two writers can interleave write and rename). `paused true` on a pre-upgrade daemon is reported as "saved, applies at next start" while the node keeps taking sandboxes; r12 removed the `/api/pause` fallback.
- **`/api/hello` is a relay target, not an oracle**: `HMAC(token, "crowdvm-node-hello:"+nonce)` cannot be turned into a session value (different message prefix), but the proof says nothing about the connection the token is then sent on. A local process that can answer on the address the CLI dials while another listener (reachable elsewhere) holds the real daemon could relay the hello and then take the token. Only possible when the daemon binds a wider address than `127.0.0.1` and the OS lets a second socket bind the specific address (Windows/BSD do, Linux does not). Not reproduced.
- **`playbook.json` paths are bounded in count (20 000) but not in length** (`feat_playbook.go`), and they go into every later deploy's guest config. A guest of the owning wallet can bloat the config of all deploys of that saved snapshot. The learned playbook is also not reset when the saved snapshot is replaced (`id = old.ID`), so deploys of the new disk start from the old one's list. Cosmetic.
- **`Bus.Publish` blocks once 4096 events are queued** while one dispatcher goroutine calls every subscriber in turn (`events.go`); a slow subscriber stalls every publisher, some of which hold sandbox locks. Not reproduced.
- **`audit_events` is never pruned**: one row per successful mutating call. By design ("records outlive"), listed for disk growth.

## What held

- **Session nonces** (`ui_session.go`): `valid` needs the HMAC and a live nonce; `save` prunes expired entries and keeps the newest 20 on every create and revoke, so the file stays bounded; `revoke` can only be reached with a valid session or the token, and a session can only name itself in the header it authenticates with; the tmp file is written under `s.mu`. Rotating `ui-token` kills every session (the HMAC changes).
- **Hello vs session HMAC**: the two messages start with different prefixes (`crowdvm-node-hello:` vs `crowdvm-node-ui-session-v2:`), so a hello proof never validates as a session.
- **`/login`**: the session is a hex nonce plus `.hex`, so the inline script cannot break out; the page is `no-store` with a CSP.
- **Rate-limit buckets**: only `/api/` requests move to `user:<id>`; no `ActionPublic` route lives under `/api/`, so a free agent key cannot buy buckets for the password and code endpoints. `authenticate` has no side effects.
- **Node names**: every writer of `nodes.name` (register, heartbeat, connect, admin rename) takes `nodeNameMu` around check and write; no other `UPDATE nodes SET name` exists. An admin's rename survives a heartbeat with the same reported name.
- **Evacuation marker**: only `evacuationResume` writes it; `failedEvacMoves` skips deleted sandboxes; `evacuateStep` runs it once, at the end, after every layer is stored.
- **Audit pagination**: no gaps or repeats with equal timestamps, asc and desc.
- **Device flow**: `Email` of an email request must match the approver; agents cannot approve; `Approve` is session-only (`ActionSession`); the `/device` page HTML-escapes the code (`html/template`) and sets `X-Frame-Options: DENY`; unclaimed agents can only call `/me`, `/limits` and the claim route; expired codes are refused.
- **`feat_install.go`**: `downloadPath` whitelists os, arch and name (no traversal through `{name}`, `.exe` only for Windows and not for the Linux-only binaries).
- **`acct_guestcfg.go`**: skips `BootInspect` and a nil config; reads the owner's policy.
- **`events.go` consumers**: webhook fan-out is keyed on the sandbox owner (documented behavior); audit lifecycle rows carry owner and org; no subscriber reads another account's rows for a request.

## Not verified

- Bug 1 was proven by order, not by qemu: I did not run two real daemons, so the kill of a "hung" qemu and the `RemoveAll` of an unreadable VM dir are read from `adoptAll`, not observed. QEMU's one-client QMP behavior is from knowledge of its monitor, not from a run.
- Bug 4's org-scoped key variant, and the `/audit-events` content seen by an org owner for org sandboxes created by members, were not tested.
- Bug 7 uses an error state written to the database; a natural one needs a holder going offline during a resume.
- The `/api/hello` relay and the Windows/BSD socket-binding behavior were not run.
- `feat_install.go`'s scripts (`install.sh`, `install.ps1`) were not read; only the Go handler.
- No real node, browser or e-mail transport; the `sendMail` relay is from the code.

## Fixes

All 7 fixed; the tests in `internal/server/bughunt_r13_test.go` and `cmd/crowdvm-node/bughunt_r13_test.go` pass (the allowance test's node got room for one sandbox per round). Extra tests: `internal/server/fix_r13_test.go`, `cmd/crowdvm-node/fix_r13_test.go`.

1. **Lock after the driver** (`internal/node/lock.go`, `cmd/crowdvm-node/drivers.go`, `main.go`): `node.LockDataDir` counts references within a process. `newDriver` takes the lock before it builds any driver (so `run`, `join`, `bench` and the `image` fallbacks all refuse while a daemon holds the dir), and `run` takes it before saving its flags. `Run` takes its own reference.
2. **Key recorded without access** (`acct_sshkeys.go`): the guard records a key only when the caller may use the handler (access, not a viewer).
3. **First sender owns the key** (`acct_sshkeys.go`, migration 2): one row per `(sandbox, user, key)`, so a key the creator also sent stays theirs.
4. **Restricted key reads the audit log** (`acct_audit.go`): 403 `api_key_restricted`. Org keys were already refused (`api_key_org_forbidden`).
5. **Parallel saves past the allowance** (`snapshots.go`): the allowance check and the insert of a new name run under `lockLimits`.
6. **Parallel device approvals** (`device.go`): approvals of one code run under a per-code lock and read the row again. Code only, also fixed: `/oauth2/token` and the CLI poll hand the token out only to the poll that marks the code consumed (`AND status='approved'`).
7. **Stale evacuation marker** (`nodes_evacuate.go` `evacMark`): the marker is `<node>@<evacuation start>`, so only moves of the current evacuation are retried.

Code only, also fixed:
- `config set` with no verified daemon refuses (nothing saved) when another process holds the data-dir lock, instead of rewriting `config.json` under it.
- Wrong device codes (`/auth/device/lookup`, `/auth/device/approve`) are capped at 20 per user per 10 minutes (429 `too_many_attempts`), so six-digit claim codes cannot be guessed.

Not fixed: the clear-text agent key in `device_codes.token` until polled (short-lived, needed to hand it over), the email relay of agent registration (IP-limited), catalog validation (admin-only), the daily allowance tick, node names never coming back (cosmetic), the hello relay (not on Linux), `playbook.json` path length, `Bus.Publish` back-pressure and audit growth (by design).
