# Bug hunt r7 (sonnet)

2026-11-11. Audited the uncommitted master tree. Product code unchanged. Only unit tests: SQLite, httptest, the fake node and guest, temp dirs, short-lived `sh`, `head` and `yes` children. No cluster, VM or e2e test was started.

I took the CLI, guest, mux and node parts myself. Two sub-agents took the server:
- One took the authorization table, quotas and input sizes (`internal/server/bughunt_r7a_test.go`).
- One took the round-6 server fixes and the guest-event, drain and migration paths (`internal/server/bughunt_r7b_test.go`).

I re-ran all the tests below myself; each one fails. Run them with `go test -count=1 -run <Name> ./internal/<pkg>/`.

Test files:
- `internal/cli/bughunt_r7_test.go`
- `internal/guest/bughunt_r7_test.go`
- `internal/mux/bughunt_r7_test.go`
- `internal/server/bughunt_r7a_test.go`
- `internal/server/bughunt_r7b_test.go`

`TestR7aAuthzEveryRoute` is in the first server file and passes; it stays as a regression test.

Worst findings:
- With `--trust-proxy`, the client-controlled `X-Forwarded-For` bypasses the hosted-port firewall and the IP rate limit.
- `GET /nodes/{id}` leaks the node owner's IP and every tenant's sandbox ids to any account.
- Any node can write notices (mailed to the owner) and agent history into a sandbox on another node.
- The org-delete cascade added in round 6 lets an org's env secret files survive a foreign deploy.

## Bugs

### Security

**1. `X-Forwarded-For` spoofing bypasses the hosted-port firewall (`--trust-proxy`)**
- Severity: security.
- Test: `TestR7aTrustProxyXFFSpoofBypassesHostFirewall` (server).
- Where: `internal/server/util.go:82-88` (`clientIP`). It is used at `feat_host.go:653`, the IP rate limit, the audit log and node `remote_addr`.
- Observed: a client in a denied range (`deny 198.51.100.0/24`) gets 200 from the hosted URL by sending `X-Forwarded-For: 10.9.9.9`. The proxy appends the real address, so the server sees `10.9.9.9, 198.51.100.7`.
- Expected: 403.
- Cause: `clientIP` takes the leftmost entry, and the client controls it behind any appending proxy (nginx `$proxy_add_x_forwarded_for`, Caddy, Traefik). It should use the rightmost entry, the one the trusted proxy added.

**2. The same spoofing bypasses the per-IP rate limit**
- Severity: security.
- Test: `TestR7aTrustProxyXFFSpoofBypassesIPRateLimit` (server).
- Where: `util.go:82`, `router.go:140`.
- Observed: with `IPRateLimit=3`, 10 requests from one client are never limited, because every forged leftmost entry gets a fresh bucket.
- Expected: 429 after 3.
- Cause: same as 1. It also defeats the login and signup brute-force limits.

**3. `GET /nodes/{id}` leaks the owner's IP and all tenants' sandbox ids to any account**
- Severity: security (information leak).
- Test: `TestR7aNodeDetailLeaksOtherTenantsAndOwnerIP` (server).
- Where: `internal/server/nodes.go:972` calls `nodeJSON(n, true)` for every caller. The fields are set at `nodes.go:923` and `nodes.go:940`.
- Observed: any key with `account.read` gets the volunteer's `remoteAddr` and `sandboxIds` of every tenant on that node, including the victim's.
- Expected: detail fields only for the node owner or an admin (`canManageNode`).

**4. Any node can inject guest events (notices, mail, agent history) into a sandbox on another node**
- Severity: security.
- Test: `TestBughuntR7bGuestEventFromForeignNode` (server).
- Where: `nodes.go:634-639` calls `lifecycle.go:748` (`onGuestEvent`), which never checks `sb.NodeID == nodeID`.
- Observed: node B sends `file.skipped` for a sandbox that runs on node A. The server writes a `file_skipped` notice with B's text on it and mails the owner.
- Expected: the event is ignored.
- Cause: the same gap lets a node write `prompt.event` (stored agent history), `setup.status`, `disk.usage` and `agent.activity` into any sandbox. A community node owner can phish other accounts through the server's own mail. The new `file.skipped` notice put attacker text into the mail, which makes this worse.

**5. Org delete removes the env versions, so the org's secret files survive a foreign deploy**
- Severity: security (round-6 regression).
- Test: `TestBughuntR7bOrgDeleteSavedSnapshotLeaksEnvSecret` (server).
- Where: `internal/server/orgs.go:412-416` (the new `DELETE FROM environment_versions / environments`). The scrub reads those rows at `lifecycle_secure.go:323` (`ownerCredentialScrub`, used by `foreignDeployScrub`).
- Observed: a sandbox ran with an org env that has the secret file `.aws/r7bcreds`, and its org saved snapshot was taken. After the org is deleted, another account deploys that snapshot through a share code. The scrub list no longer holds `.aws/r7bcreds`, so the file stays in the new owner's sandbox. Without the org delete, the scrub does list it.
- Expected: the file is scrubbed.
- Cause: a normal env delete is soft (`deleted_at`) and keeps the versions. The round-6 cascade deletes them for good. The `--no-env` resume path still works, because it also uses `envManaged` saved on the sandbox.

### Stuck

**6. A drained node can never reconnect**
- Severity: stuck.
- Test: `TestBughuntR7bDrainedNodeCannotReconnect` (server).
- Where: `internal/server/nodes.go:428` refuses any node with `n.Disabled`. `nodes drain` sets that flag (`PATCH disabled=true`).
- Observed: after a drain and a node restart, the connect gets 403 `node_disabled`. The sandboxes on the node go to `node_offline` and stay there for as long as the drain lasts.
- Expected: a drain only stops new placements. The CLI text says running ones keep running, and round 6 sends `Drained` in the heartbeat for that reason, which can only reach a node that is connected.
- Cause: the drain flag doubles as a ban.

**7. A node blip during a foreign-deploy scrub leaves the sandbox "ready" but refusing every call**
- Severity: stuck.
- Test: `TestBughuntR7bNodeBlipDuringScrubStuckSecuring` (server).
- Where: `internal/server/lifecycle.go:673-679`, against `lifecycle_secure.go:159` (`secureAsync` gives up on `!sb.starting()`).
- Observed: the node drops during the scrub. When it comes back, the sandbox is `ready`, but every call gets 409 `sandbox_securing`. The scrub is never run again (still 1 scrub call 6 s after the node returned).
- Expected: the scrub resumes, then the sandbox becomes usable.
- Cause: the `node_offline` recovery goes straight to `readyState` and skips the `isSecuring` branch at `:663`. `envGuard` still blocks the API, so secrets do not leak that way. Hosted ports were not checked.

**8. Concurrent `store.Open` on a fresh data dir fails with `SQLITE_BUSY`**
- Severity: stuck (startup race, low).
- Test: `TestBughuntR7bConcurrentOpenMigrate` (server). Timing dependent: it failed every time without `-race`, and 2 of 3 runs with it.
- Where: `internal/server/store/store.go:36`.
- Observed: 6 concurrent opens of a new file fail with `database is locked`, without waiting for the busy timeout.
- Expected: they wait.
- Cause (a guess, not confirmed): the `journal_mode(WAL)` switch does not honour `busy_timeout`. The round-6 lock fixed concurrent `MigrateAll` on an existing file, which still holds.

### Wrong result and data

**9. After the output cap cuts a log, a client that reconnects with its offset gets the whole output again**
- Severity: wrong-result (duplicated output).
- Test: `TestBughuntR7AttachAfterCapReplaysFromZero` (guest).
- Where: `internal/guest/commands.go:481-483` (attach: `if v > fileSize(...) { v = 0 // log replaced }`) and `:747` (`readChunk`, used by `GET /commands/{pid}?stdoutOffset=`), interacting with `capFile` at `:163`.
- Observed: a detached command writes 100000 bytes with `outputCap` at 4 KiB. A client that read to offset 100000 reconnects with `stdoutOffset=100000`. It gets 4169 bytes of old output (the capped file and the marker), and so does the status call.
- Expected: no old output, since the client has already seen it.
- Cause: `capOutput` truncates the file to cap plus marker, below an offset a live client may already have consumed. The guest then reads "offset beyond the file" as "log replaced" and rewinds to 0. The attach stream asks for a reconnect every 10 minutes, so a long command that outputs more than 64 MiB (`crowdvm exec --timeout 0`, text mode now follows through attach) prints its first 64 MiB twice. A live follower whose offset is past the cut also never sees the marker.

**10. `exec` gives up after 21 network cuts even though every reconnect made progress**
- Severity: wrong-result.
- Test: `TestBughuntR7ExecAttachGivesUpAfter21Cuts` (cli).
- Where: `internal/cli/cmd_exec.go:263`, `:281-282`.
- Observed: the attach stream delivers data and is then cut, 30 times. The 22nd stream is never opened. The CLI exits 1 with `attach: stream keeps ending: EOF` after 21 streams, while the command keeps running (it is not killed).
- Expected: exit 0 after 30 cuts, with all output.
- Cause: `failures` is reset only on a `reconnect` frame, not when a stream delivered data. The help text promises "surviving network cuts". A flaky link during a multi-hour `--timeout 0` build hits it. Since text mode now always goes through `runLong`, the default 30 s exec has the same path.

**11. `extractTar` still creates symlinks that resolve outside the destination (ordering hole)**
- Severity: security (low, hardening).
- Test: `TestBughuntR7ExtractTarLinkChainEscapesDest` (cli).
- Where: `internal/cli/transfer.go:376` (`linkInside`) and the symlink case at `:480`.
- Observed: the entries `d/b -> a/..` and then `d/a -> .` both extract. `d/b` resolves to the destination's parent (`xfer/`, not `xfer/d`). Writes through it are still refused (`checkNoSymlinkParents`), but the link stays on disk, and `EvalSymlinks(dest/b)` is outside.
- Expected: the escaping link is skipped, as the doc comment promises ("symlinks that point outside dest are skipped").
- Cause: `linkInside` checks the target lexically against links that exist when the entry arrives. `a` does not exist yet when `b` is checked, and a later link can make the path resolve differently. The next tool that follows `b` (an `rm -r`, `cp -r`, an agent writing into the folder) leaves the destination. Fix: refuse any target with `..` after a name, or re-check all links after extraction.

### Leaks and bloat

**12. `mux.Join` leaks both connections of an abandoned session when the backend idles**
- Severity: leak (low).
- Test: `TestBughuntR7JoinClientGoneIdleBackendLeaks` (mux).
- Where: `internal/mux/mux.go:151-157`.
- Observed: the client closes completely and the backend does not read or write. `Join` was still running 2 s later, with 2 goroutines and both sockets.
- Expected: it ends, since nobody is left to read a reply.
- Cause: when one direction hits EOF and the other end has `CloseWrite`, the other direction is left to run until the peer closes. A backend that only writes on events (SSE, a log tail) is never told. This applies to direct TCP ports, where both ends are `CloseWrite`-capable (`internal/node/direct.go:295`). Wrappers that embed `net.Conn` (`withBuffered`, `hijacked`, `readerConn`) do not expose `CloseWrite` and keep the old behaviour. A half-close timeout, or closing both when the first end gets a read error rather than a clean EOF, would fix it.

**13. Pre-auth memory blow-up on the `/api/box/v1` alias**
- Severity: leak (availability).
- Test: `TestR7aBoxAliasParsesHugeBodyBeforeAuth` (server).
- Where: `internal/server/feat_legacy.go:102` (`serveBoxAlias`).
- Observed: one anonymous 15 MiB JSON POST to `/api/box/v1/sandboxes` makes the server allocate about 554 MB before it answers 401. The same request to `/api/v1/sandboxes` allocates about 0.
- Expected: authenticate before reading the body, or use a small bound.
- Cause: the raw handler reads up to 16 MiB, unmarshals into `any`, renames the keys and re-marshals before the inner route authenticates.

**14. Unbounded string fields**
- Severity: leak.
- Test: `TestR7aUnboundedStringFields` (server).
- Where: API key `name` (`apikeys.go` `handleCreateKey`), telemetry `endpoint` (`feat_otel.go:727`), org invite `email` (`orgs.go:640`).
- Observed: each accepts and stores 4 MiB. `GET /api-keys` then returns 4 MB for one key, and every key creation echoes the full list.
- Expected: 400, like the neighbouring fields (webhook url 2048, display name and org name 80, feedback 20000).

**15. No per-account caps on objects**
- Severity: leak.
- Test: `TestR7aNoPerAccountObjectCaps` (server).
- Where: `handleCreateKey`, environment create, org create and org invite create.
- Observed: one account creates 250 each of API keys, environments, orgs and invites with no refusal.
- Expected: a cap, as for webhooks (`whMaxURLs=10`), hosted ports (50) and direct ports (20).
- Cause: `GET /api-keys` is unpaginated and each create returns every key, so N creations cost O(N²).

**16. Org delete leaves the org's AI credential profiles behind**
- Severity: leak.
- Test: `TestBughuntR7bOrgDeleteLeavesCredentialProfiles` (server).
- Where: `internal/server/orgs.go:410-425`. The cascade covers environments and git sources only.
- Observed: after the delete, 1 `credential_profiles` row (`owner_id` = org id) and 1 `credential_accounts` row with the sealed API key remain.
- Expected: both go with the org.
- Not tested: `ci_links.org_id` stays `active` with its sealed token, and CI jobs then try to create sandboxes for the deleted org (`feat_ci.go:639`). Known since round 5.

### Cosmetic

**17. Only the first skipped secret file gets a notice**
- Severity: cosmetic.
- Test: `TestBughuntR7bFileSkippedSecondPathLost` (server).
- Where: `lifecycle.go:760` calls `lifecycle_notify.go:20-26`.
- Observed: two skipped files at one boot give one notice, for `.r7b/one` only.
- Expected: both paths reported.
- Cause: the 24 h dedupe is keyed on sandbox and kind, not on the path. The notice is never cleared either, even when the sandbox is deleted.

**18. Sandbox name truncation splits a multi-byte character**
- Severity: cosmetic.
- Test: `TestR7aSandboxNameTruncationSplitsRune` (server).
- Where: `internal/server/sandboxes.go:489` (`n = n[:120]`).
- Observed: a name over 120 bytes is cut mid-character, so the stored name ends in invalid UTF-8.
- Expected: cut on a rune boundary.

### Code only (no test)

- `crowdvm server sandboxes` (`internal/cli/cmd_server_admin.go:213`) still says "newest 500" and does not page. The server's default is now 100, with 200 max, so the CLI shows a different list than it claims.

## Checked and held

**Authorization of every route (`TestR7aAuthzEveryRoute`, passing)**
- 232 routes: 224 from the OpenAPI route table, plus 8 raw handlers (desktop ws, snapshot download inventory and blob, `/p/{id}/{port}`, ai-proxy, `/api/box/v1` sandboxes, environments and webhooks).
- Each is called unauthenticated, with the victim's own key missing the route's scope, as an unrelated user, as a user from another org, and as a plain member of the victim's org on `{org}` and `/admin` routes. It also checks org-scoped `account.admin` and `environment.write` writes.
- The victim has a sandbox, snapshot, saved snapshot and share, environment with a var, webhook, key, join token, node, org, invite, hosted port and AI key.
- Result: no 2xx with victim ids in the path, no victim data in a list, no `/admin` access for a normal user, no org-admin action by a plain member, and the victim's objects are intact afterwards. The only exception is node detail (bug 3).
- Public routes: signup, login, `email/start` and `verify`, `GET /auth/config`, `device/start` and `poll`, `POST /agent/identity`. All are account creation or login flows. Snapshot download inventory authenticates itself.
- Webhooks, telemetry, `POST /secrets` and the credential-profile routes act on the caller's personal account whatever the org scope, so a plain member can't change org settings through them.

**Input sizes:** Bind 8 MiB; `readBody` 8 MiB with 413; policy guard 8 MiB; commands 16 MiB with 413; prompt 16 MiB; ssh keys 1 MiB; mcp 1 MiB; device and oauth 1 MiB; node register 1 MiB; heartbeat 4 MiB; node events 16 MiB. Env contents are capped at 256 KB and 1000 vars, sandbox env at 64 KB, labels at 256, AI keys at 8192, webhook url at 2048. The IP limiter map is evicted every window. Left out: env versions are never pruned (owner-only), and `bench/upload` (`nodes.go:789`) reads without a limit but needs a node token.

**CLI**
- `extractTar` (apart from bug 11):
  - A root entry that is a symlink or any non-directory is refused, and a directory entry for the root is ignored.
  - Parent symlinks, including `dest` itself, are caught by `checkNoSymlinkParents`, so a later file is never written through a link.
  - Hardlinks: the source must be a regular file inside dest. A missing or filtered source is skipped; a self-link errors, only a hostile archive.
  - Files are written to a temp file and renamed, so a planted link at the target is replaced, not followed.
  - `..` and absolute names are neutralised.
  - Local TOCTOU between `Lstat` and the write needs another local process, and the sandbox cannot reach the host's disk.
  - `only` is passed `nil` by every caller.
- `exec` text mode: exit codes 124 (`--timeout`, with the stop call sent), 130 (interrupt), the exit status or `128+signal`, and an error for an `error` frame. A stream that ends without an exit frame is checked through the status call. `--json` keeps the sync path and its truncation flags. Known and not a bug: an adopted process after an agent restart has no exit code and shows 1.
- Config lock tokens: unlock removes the file only while it holds this process's token. A takeover of a lock older than 30 s works, and a crash between create and write (empty token) is handled. The Stat-then-Read window in the stale takeover stays open (named in the round-6 fix, and not testable without a hook).
- Waiters: the CLI, Python and TypeScript waiters all treat `error` with `node_offline` as passing and fail with that error at the timeout. Any other error fails at once. `stop --wait` and `stop_and_remove` also treat `error` as stopped, which is fine because their goal is a stopped or deleted sandbox.

**Guest**
- `meta.json`: `save` marshals, writes a unique temp file and renames, all under `h.mu`. There is no lock-order problem with `a.mu` (`prune` drops `h.mu` before taking `a.mu`).
- `capFile`: the prefix up to the cap is kept intact, the marker is rewritten idempotently, and the writer (opened `O_APPEND`) continues at the end after the truncate. The overshoot is at most one 100 ms tick. The only defect is the offset rewind (bug 9).
- Detached kill: SIGTERM to the process group, wait for the group to be gone, then SIGKILL. A timeout kills the group with SIGKILL. No zombie waits, because the agent is not a subreaper.
- `file.skipped`: the folder-exists case, `mkdirAll` errors and write errors all emit the event. The server-side problems are bugs 4 and 17. An absolute path outside home that fails as root, and every unprivileged skip, are only logged (cosmetic).

**Mux and node**
- Node transfer `once`: every waiter returns on its own context, the last one leaving cancels `fn` and marks the call abandoned. A new caller waits for `fn` to return before starting afresh, so two runs never overlap. `c.err` is written before `close(done)`. `-race` clean.
- `Join` keeps the half-close reply for plain TCP ends (round-6 bug 19 fixed); the leak above is the cost.
- Heartbeat `Drained`: the node stores it and shows it in `/api/status`. The scheduler, `couldEverPlace` and `sizeAvailability` skip disabled nodes. The problem is the reconnect (bug 6).

**Server round-6 fixes**
- Webhooks:
  - Leased rows have a future `next_attempt_at`, so they neither count toward the per-owner top 4 nor hide other due rows.
  - An owner cannot exceed 4 in flight across ticks, because the in-memory count covers the lease gap.
  - Ties are ordered by `(next_attempt_at, id)`.
  - An unreadable secret ends as `failed` without a retry.
  - Deliveries `limit`: 0, negative and non-numeric give 400, and values above 500 are clamped.
  - Design limit: 4 accounts can still fill the 16-slot pool.
- Org delete: atomic in one transaction. Personal environments are untouched. Sandboxes keep a dangling `environment_id`, and resume handles it. Minor: the running check counts `error` (`node_offline`) as not running, though its VM may still run.
- History cursor: scoped by sandbox id, so it can't probe other sandboxes. A teammate can test whether a private command id exists, because the cursor lookup skips `private=0`. `ts:` with an empty id is silently ignored.
- Events `limit` and `sort`: 400 on bad values. `namePrefix`: rune counting matches SQLite `substr`. Admin sandbox list: `pageInfo` is right, `stopped` maps to `archived`, and other filters are ignored.
- Migrations: the check inside the immediate transaction works, and a failed step rolls back its `schema_migrations` row. Six concurrent `MigrateAll` on an existing file pass.
- `onNodeOffline` recovery: `error` is cleared, health returns to ok, and the `sandbox_error` notice is removed. `sandbox.error` is still emitted on short disconnects (known since r6-opus 3).

## Not verified

- The Stat-then-Read race in the config-lock takeover.
- Windows and macOS behaviour of `extractTar`.
- Whether hosted ports are reachable while a sandbox is stuck in `securing` (bug 7).
- The cause of the `SQLITE_BUSY` in bug 8 (the WAL pragma is a guess).

## Tests that were kept but pass

- `TestR7aAuthzEveryRoute`, the authorization table.

**TL;DR:** 18 bugs, each with a failing test, plus one code-only note. The serious ones are the spoofable `X-Forwarded-For` under `--trust-proxy`, the node detail leak, nodes injecting events into other nodes' sandboxes, and the org-delete cascade dropping secret-file paths. The CLI/guest fixes mostly held. The new holes are the output cap's offset rewind (duplicated output), the attach retry counter, an extractTar symlink ordering hole, and the half-close leak in `Join`. The 232-route authorization table passes and stays as a regression test.

## Fixes (A)

Items 9-12. Their tests pass: guest without `-race`, cli and mux with `-race`; `go vet` is clean.

- **9 (fixed).** `internal/guest/commands.go`: an offset beyond the file (the cap cut it) is now clamped to the file size instead of rewound to 0, in the attach handler and in `readChunk`. A client that already read past the cut gets nothing again, and offsets stay monotonic. The file is still cut in place by `capFile`, so a live follower past the cut never sees the marker (unchanged).
- **10 (fixed).** `internal/cli/cmd_exec.go`: `attach` resets `failures` when a stream advanced the stdout or stderr offset, not just on a `reconnect` frame. It gives up only after 21 consecutive streams with no progress.
- **11 (fixed).** `internal/cli/transfer.go`: `linkInside` refuses a target with a `..` after a name (`a/..`). Leading `..` still goes through the inside-dest check. This closes the `b -> a/..`, `a -> .` chain.
- **12 (fixed).** `internal/mux/mux.go`: after one direction ends with a successful `CloseWrite`, `Join` starts a 1 s idle timer on the other direction. Each write in either direction resets it, and when it fires both ends close. A reply after `CloseWrite` still arrives (round-6 test passes). Trade-off: a backend that stays silent for more than 1 s after the client half-closes now gets cut. A full close and a half-close look the same on the wire, so the timeout is the only signal.

## Fixes (B)

Items 8, 13, 14, 15, 17, 18. Their tests pass; `go vet ./internal/server/...` is clean.

- **8 (fixed).** `store/store.go`: `busy_timeout` and WAL were already in the DSN. The failure is the first-time switch to WAL on a fresh file, which returns `SQLITE_BUSY` at once (the busy timeout does not apply) when another opener does the same. `Open` now retries the whole init (open, ping, `schema_migrations`) up to 100 times with a short backoff, and only on a busy/locked error. `TestBughuntR7bConcurrentOpenMigrate` passed 5/5.
- **13 (fixed).** `feat_legacy.go`: `serveBoxAlias` calls `authenticate` first and parses and renames the body only for an authenticated caller. The request body is wrapped in `http.MaxBytesReader` at 8 MiB, the same limit as `Ctx.bind`. An anonymous request goes through unread and the inner route answers 401. Trade-off: an anonymous call to a public route no longer gets the Box key rename in its body.
- **14 (fixed, partly).** API key name is limited to 120 bytes (`invalid_name`) and the telemetry endpoint to 2048 bytes (`invalid_endpoint`), both 400. The invite email in `orgs.go` is left to someone else, so `TestR7aUnboundedStringFields` still fails on that part.
- **15 (fixed, partly).** `handleCreateKey` returns 409 `api_key_limit` at 200 active (non-revoked) keys per user. `handleEnvCreate` returns 409 `environment_limit` at 100 environments per owner. Orgs and invites are not covered, so `TestR7aNoPerAccountObjectCaps` still fails for those two.
- **17 (fixed, no `lifecycle.go` change).** `lifecycle_notify.go`: for kind `file_skipped` the dedupe also matches the message, which names the path. Each skipped path gets its own notice and a repeat of the same path inside 24 h is still dropped. The call in `lifecycle.go:760` is unchanged. Not done: the notice is still not cleared when the sandbox is deleted.
- **18 (fixed).** `sandboxes.go` `validName`: after `n[:120]`, trailing bytes are dropped until the string is valid UTF-8.

Still failing in the full `./internal/server/...` run, all in other engineers' areas: `TestR7aNoPerAccountObjectCaps` (orgs, invites), `TestR7aUnboundedStringFields` (invite email), `TestBughuntR7bOrgDeleteSavedSnapshotLeaksEnvSecret`, `TestBughuntR7bOrgDeleteLeavesCredentialProfiles`. The `GuestEventFromForeignNode`, `NodeBlipDuringScrub` and `DrainedNodeCannotReconnect` tests failed in the first run and passed in the full run, so those are being fixed elsewhere.
