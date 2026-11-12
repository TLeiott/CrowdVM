# Bug hunt r11 (sonnet)

2026-11-11. Product code unchanged; I only added `internal/server/bughunt_r11_test.go` and this report. Unit tests only (`servertest`, the fake node and fake guest). No cluster, VM or e2e test. I did the work myself, no sub-agents.

## What I audited

- **Round-10 fixes** (`git show b9ac6f3`) and `b3178f2`: `lockLimits` (taken in create, resume and fork; checked for lock order against `sbLocks`, `schedMu`, `placeSandbox`, the `NewSandbox` hooks: no cycle, `limitMu` is a leaf for every caller), `recordStart(wallet, owner)`, the `policyGuard` size inheritance, `feat_ci.go` migration 3 (`gh_id`) and the new `runnerSandbox`, `markUnconnected`/`seenAt`/`nodeLost`, the new placement-as-change rule in `handleResume`. Holes found: the resume-while-stopping path (bug 4) still skips every limit check, and a `NewSandbox` hook refusal leaks the idempotency key (bug 5) in the very block the r10 fix edited. The migration is fine (`UPDATE ci_jobs SET gh_id=id` backfills; the suffix key is only used when a second link collides, and every provider call reads `gh_id`).
- **Authentication and identity**: `auth.go` (sessions, cookie and CSRF rule, password handling, email codes, signup), `device.go` (CLI device codes, agent registration and claim, `/oauth2/token|revoke`), `apikeys.go` (presets, `Allows`, `AllowsSandbox`, `covers`, creation, rotation, expiry, org keys through `acct_guard.go`).
- **How far the server trusts nodes**: `nodes.go` (registration and join tokens, heartbeat, the events handler, blob PUT/GET, snapshot reports, node renaming), `snapshots.go` (`recordTaken`, `onSnapshotUploaded`), `lifecycle.go` `reconcileVM`/`adoptQueuedMachine`/`onGuestEvent`, `feat_direct.go`, `stranded.go`, `lifecycle_place.go`, `scheduler.go` (`matchReason`, `defaultImage`).

## How to run

```
go test -count=1 -run TestBughuntR11 ./internal/server/
```

Every test fails today (I ran each 3 to 5 times; the three timing-dependent ones loop over several tokens or codes inside the test, and failed in every run). `go vet ./internal/server/` is clean and the rest of the package is untouched.

## Bugs proven by a failing test

All in `internal/server/bughunt_r11_test.go`.

### 1. Any node can replace a stored snapshot or image blob (security, high)

- Test: `TestBughuntR11NodeOverwritesStoredSnapshotBlob` (`:58`)
- Code: `nodes.go:672` (`handleBlobPut`), `store/blobs.go:55` (`Put`: "Existing blobs are replaced").
- Observed: node B `PUT /node/v1/blobs/snapshot/<id>` over a layer node A uploaded and the server stored (verified) gives 200, and the stored file and its `.sha256` are now B's bytes. The `snapshots` row keeps A's checksum (the SQL update is guarded by `uploaded_at IS NULL`), so nothing notices.
- Expected: blobs are immutable once stored (409), or at least only the node that took the layer may upload it.
- Cause: the handler authenticates the node but never asks whether it took or holds that snapshot, or whether the id is stored already. A community node learns layer ids by restoring a shared or saved snapshot (it needs the whole chain), then poisons that snapshot for every later deploy. The same path overwrites images (bug 14).

### 2. A single-use join token registers many nodes (security, low-medium)

- Test: `TestBughuntR11SingleUseJoinTokenRegistersManyNodes` (`:90`)
- Code: `nodes.go:364-405` (`handleNodeRegister`): the `uses`/`max_uses` check is a SELECT at the top and `uses=uses+1` runs at the end.
- Observed: 12 parallel registrations with a token of `maxUses: 1` (the default of `POST /join-tokens`): 4 to 8 succeed, each with its own node token.
- Expected: exactly one.
- Cause: check and increment are separate statements. `UPDATE join_tokens SET uses=uses+1 WHERE id=? AND (max_uses=0 OR uses<max_uses)` and a `RowsAffected` check fix it.

### 3. Node names are not unique, yet pins and lookups match by name (security, low-medium)

- Test: `TestBughuntR11NodeNamesAreNotUnique` (`:135`)
- Code: `nodes.go:405-420` (register takes `req.Info.Name`), heartbeat `UPDATE nodes SET name=...` (`nodes.go:~553`), `nodeByRef` (comment says "its (unique) name"), `scheduler.go:107` (`matchReason`: `strings.EqualFold(p.Node, n.Name)`).
- Observed: the admin's node `my-laptop` and a node owned by another user, also `my-laptop`, are both registered. A sandbox pinned with `--node my-laptop` can land on either.
- Expected: names are unique (refuse or suffix at registration and rename), or a pin by name that matches several nodes is refused.
- Cause: nothing enforces uniqueness. A user who pins to "my own laptop" by name can be placed on a contributor's machine, whose owner can read the disk (`placement.trust` is the only other guard and it is off by default).

### 4. Resume while stopping skips every start check (limit bypass, medium)

- Test: `TestBughuntR11ResumeDuringStopSkipsStartLimit` (`:158`)
- Code: `lifecycle.go:1472-1475` (the `StateArchiving` case returns `resumeDuringStop` before `checkCredits`, `checkConcurrent`, `checkStarts` at `:1523-1531`), `lifecycle_place.go:290` (`afterStopResume` only calls `recordStart`).
- Observed: `StartsPerMinute=1`, one create. `stop` (failing) then `resume` with the node offline: 202 `resuming`. Expected: 429 `rate_limited`.
- Expected: the same gates as any other resume.
- Cause: same cause as r10 sonnet 3 (limits not checked on a start path), on the one path r10 did not touch. Also bypasses `billing_required`, the credit cap and `limit_reached`: after `enforceCaps` stops a sandbox for the cap, `stop`+`resume` restarts it as soon as the stop finishes. (The test uses the start window because it needs no billing setup.)

### 5. A `NewSandbox` hook refusal leaves the Idempotency-Key "in progress" (wrong-result, low)

- Test: `TestBughuntR11HookRefusalLeavesIdempotencyKeyBusy` (`:182`)
- Code: `sandboxes.go:949-953` (`return err` where all neighbours use `fail(err)`).
- Observed: account has `snapshotsOff`; `POST /sandboxes {"snapshots":true}` with a key gives 409 `snapshots_off_for_account`. The same request again gives 409 `idempotency_in_progress`, for two minutes.
- Expected: the refusal again (the key is released).
- Cause: `idemRelease` is only called from `fail`. Hits `account_closed`, `snapshots_off_for_account`, `monitoring_off_for_account`.

### 6. Changing or resetting a password does not end other sessions (security, medium)

- Test: `TestBughuntR11PasswordChangeKeepsOtherSessions` (`:205`)
- Code: `auth.go:160` (`SetPassword`); callers `auth.go:522` (change), `account.go:217` (admin reset), `cmd/crowdvm-server/main.go:337` (`set-password`).
- Observed: two CLI sessions; the first changes the password; the second still answers 200.
- Expected: the other sessions of the account are deleted (the current one may stay). Close and suspend already do this (`feat_retention.go:335`, `account.go:213`).
- Cause: the change never touches `sessions`. Rotating a password after a leaked token (the usual reason) leaves the thief logged in for the whole session TTL. API keys are untouched too (the user revokes those on purpose).

### 7. An unverified email captures that person's later email login (security, medium)

- Test: `TestBughuntR11UnverifiedEmailCapturesLaterEmailLogin` (`:225`)
- Code: `auth.go:465-` (`handleUpdateMe` stores any email), `auth.go:553-` (`handleEmailVerify` signs in the account that holds the email).
- Observed: user `r11mallory` sets email `ceo@corp.test` (no proof). Later the owner of that address does "sign in with email": the code arrives in their mailbox, they enter it, and they are logged in as `r11mallory`: the attacker's account, which the attacker keeps using. Same hole for the agent email method: `handleDeviceApprove` trusts `c.P.User.Email` (`device.go:~140`), so that account can approve an agent registered for the real owner and the agent gets a key of the attacker's account.
- Expected: an email changes only after a code mailed to it is entered (the email-start/verify code already exists), or it never counts for email login or agent approval until verified.
- Cause: `email` is a free-text column with no verified flag.

### 8. The 5-attempt limit of email codes does not hold under parallel guesses (security, low)

- Test: `TestBughuntR11EmailCodeAttemptLimitRaces` (`:251`)
- Code: `auth.go:~565-575`: reads `attempts`, checks `>= 5`, and only then `attempts=attempts+1`; a correct code is checked against the same stale read.
- Observed: 60 parallel wrong guesses against one code leave `attempts` at 9 or more (limit 5). A hit among them would also win after the limit.
- Expected: at most 5 guesses per code. `UPDATE email_codes SET attempts=attempts+1 WHERE email=? AND attempts<5` first, then compare.
- Cause: check-then-increment. Also `handleEmailStart` resets `attempts` for any email on every call and is public with no per-email throttle (see Code only), so the budget is unbounded anyway.

### 9. A key that may only delete its own sandboxes mints a key that deletes any (security, medium)

- Test: `TestBughuntR11DeleteOwnKeyMintsDeleteKey` (`:300`)
- Code: `apikeys.go:58-68` (`Allows`: `case "sandbox.delete": return k.hasAction("sandbox.delete-own")`), used by `covers` (`:241`).
- Observed: key `{account.admin, sandbox.delete-own, sandbox.read}` creates a child `{sandbox.delete, sandbox.read}` (201). The child deletes a sandbox the narrow key did not create: 202.
- Expected: 403 `api_key_scope_exceeded`.
- Cause: `Allows` maps `sandbox.delete` onto `sandbox.delete-own` so the per-sandbox rule in `AllowsSandbox` can narrow it. `covers` reuses `Allows` to decide what a parent may grant, so the narrowing is lost. `covers` must compare actions exactly (or treat `sandbox.delete` as not covered by `delete-own`).

### 10. A password over 72 bytes is a 500 (wrong-result, low)

- Test: `TestBughuntR11LongPasswordIsAnInternalError` (`:323`)
- Code: `auth.go:~109` (`CreateUser`) and `:160` (`SetPassword`) pass bcrypt's `ErrPasswordTooLong` up.
- Observed: signup with a 116-byte passphrase: 500 `internal_error`. Same for `POST /auth/password` and the admin reset.
- Expected: 400 `weak_password` (or `password_too_long`), or the password is pre-hashed (SHA-256, then bcrypt) so long passphrases work.
- Cause: only the minimum length is checked.

### 11. Registering with another node's id takes that node over (security, high where tokens share an owner)

- Test: `TestBughuntR11RegisterTakesOverAnotherNode` (`:339`)
- Code: `nodes.go:381-392` (`old.OwnerID == owner` is the only check), `:394-399` re-keys the row.
- Observed: node X is online. A second join token of the same owner registers with `Info.NodeID = X`: the answer carries X's id and a new token; X's old token now gets 401.
- Expected: a new node id (or 409). Re-registration needs the node's current token, or at least the same join token id.
- Cause: join tokens the admin creates default to the admin as owner, so on a normal team server every contributor's node has the same `owner_id`. Any contributor can read node ids (`GET /nodes`) and take over, and DoS, any other node. The sandboxes and snapshot locations of that id then point to the attacker's machine.

### 12. A node can claim to hold any layer it names (security, low-medium)

- Test: `TestBughuntR11NodeClaimsHoldingForeignLayer` (`:364`)
- Code: `snapshots.go:178-181` (`recordTaken`: for a known id, `INSERT OR IGNORE INTO snapshot_locations ... SELECT id,? FROM snapshots WHERE id=? AND purged_at IS NULL`).
- Observed: node B posts "I took layer L" (`POST /node/v1/snapshots`) for a layer of a sandbox on node A: B is now in `snapshot_locations` for L.
- Expected: ignored unless the layer's `node_id` or its sandbox's node is the reporter.
- Cause: the existing-row shortcut skips the checks the new-row path has. B then counts as a holder: `pickNode` prefers it for restores, `chainReachable`/`holderOnline` count it for unstored layers, `requestUpload` asks it for the blob, and deletions of L wait for B to erase a copy it never had (the "blocked" report of `deletion_ops.go`).

### 13. A node can set the size of any layer (wrong-result, low)

- Test: `TestBughuntR11NodeSetsSizeOfForeignLayer` (`:389`)
- Code: `snapshots.go:255-257` (`onSnapshotUploaded`: `UPDATE snapshots SET size_bytes=? WHERE id=?`).
- Observed: node B sends `snapshot.uploaded` for A's layer with `sizeBytes: 1<<40`: the row changes from 40 to 1099511627776.
- Expected: only the node that holds or took the layer changes it (and the server could take the stored blob's size).
- Cause: the update runs before any ownership check; sizes feed the storage numbers and the saved-snapshot allowance.

### 14. Any node's image upload becomes the default image of every sandbox (security, medium)

- Test: `TestBughuntR11AnyNodeUploadBecomesDefaultImage` (`:422`)
- Code: `scheduler.go:322-330` (`defaultImage`: `SELECT id FROM images ORDER BY created_at DESC LIMIT 1`), `nodes.go:~699` (the upload upserts `images` with `created_at=now`).
- Observed: the admin's node uploads `base-v1`; a node owned by another user uploads `evil-v1`: `GET /node/v1/images` answers `default: evil-v1`. New sandboxes of all tenants take it as `imageId`.
- Expected: no automatic default from an arbitrary node's upload (use `--default-image`, or let only admin-owned or trusted nodes publish images), and no overwrite of an existing image id (bug 1).
- Cause: "newest upload wins" with no check of who uploaded.

### 15. A username can equal another user's id and wins the lookup (security, medium)

- Test: `TestBughuntR11UsernameCanBeAnotherUsersID` (`:454`)
- Code: `auth.go:91-93` (`UserByLogin`: `username=? OR email=? OR id=? LIMIT 1`, no `ORDER BY`), `usernameRe` (`auth.go:97`) accepts `usr_` plus 16 characters.
- Observed: signup with username `usr_<victim id>` is accepted, and `UserByLogin(victim.ID)` returns the squatter's account.
- Expected: usernames of the shape of an id are refused (or ids are matched first).
- Cause: ids are visible in org member lists. Everything that resolves a user from a free ref goes through `UserByLogin`: credit transfer `to` (`credits.go:282`, the credits go to the squatter), org invite (`orgs.go:609`), admin user routes by id (`account.go:191`, `:242`: a disable or grant hits the squatter), merge `from`/`into` (`acct_settings.go:439`, `:449`: merging into the victim id moves everything into the squatter), join-token owner (`nodes.go:1183`).

## Code only

- **No rate limit or lockout on login, email start, agent identity or device approval beyond the global IP window** (`auth.go` `handleLogin`, `handleEmailStart`, `device.go`): default `IP` is 10000 requests per 60 s per IP. Password guesses are not counted per account. `handleEmailStart` is public, resets `attempts`, and mails any address (an SMTP relay and a mail bomb); the code is also written to the log. `POST /agent/identity` (anonymous) creates a user row per call even with signup closed (`checkSignup` is not applied) and nothing prunes unclaimed agents. A test would need a throttle to compare against; I list the missing ones for the owner. Cosmetic to medium, depends on whether the server faces the internet.
- **Agent claim codes are 6 digits** (`device.go` `newDevice` with `randDigits(6)`, 10 minute life). Any signed-in user who guesses a pending code approves the claim, and the agent then receives a key of the guesser's account. 10000 requests per minute per IP make roughly one hit in ten windows with one pending code. CLI codes (8 letters) are not affected.
- **Device poll and `/oauth2/token` can hand out the token twice** (`device.go` `handleDevicePoll`, `handleOAuthToken`): the read of `status='approved'` and the `UPDATE ... 'consumed'` are not one statement, so two parallel polls both get a session or key. Only the holder of the device code can do it. Cosmetic. The agent token is also kept in clear text in `device_codes.token` until consumed.
- **A node can rename itself on every heartbeat** (`handleHeartbeat` `name=CASE WHEN ...`): see bug 3; the heartbeat path has no uniqueness check either.
- **Node `Info.Capacity`, `Bench`, `Labels`, `Direct` endpoints are self-reported and unchecked** (`nodes.go` heartbeat; `feat_direct.go:128`; `reconcileVM` `st.Direct`): by design, but `placement.labels`, `minTier`, `categories` and `gpu` are therefore advisory when the node's owner is not trusted, and the `sshEndpoint`/`directEndpoints` strings a node reports are shown to users as given. Admin labels override node labels, which is the only protection. I did not check what the CLI does with the ssh endpoint string.
- **Blob GET and snapshot metadata are open to every node** (`handleBlobGet`, `handleSnapshotMeta`): any node reads any layer whose id it knows. Ids are unguessable, and nodes learn the ids of chains they restore, so this only matters together with ids leaking through shared chains.
- **`afterStopResume` and the CI/limit lock**: `afterStopResume` still calls `recordStart` outside `lockLimits`; part of bug 4.
- **Admin-owned join tokens**: the default owner is the creator, so on a team server all nodes share an owner. This is what makes bug 11 reachable; `--owner` per contributor narrows it, but the re-register rule should not depend on it.

## What held

- **Lock order of `lockLimits`**: held in create, resume and fork. Inside it: DB statements, `placeSandbox` (takes `schedMu` for a short time), `Hooks.NewSandbox`, `insertSandbox`. Nothing under `schedMu` or `sbLocks` takes `limitMu`; resume takes `sbLocks(sb)` first and `limitMu` second, create and fork take no `sbLocks` under it. `queueBoot` only schedules `tryPlace` with `s.Go`. No cycle found.
- **r10 CI changes**: migration 3 backfills `gh_id` and adds the index; `newJob` dedupes per link; every provider call uses `GhID`; `runnerSandbox` requires a `ci_jobs` row of the same link.
- **b3178f2**: `markUnconnected` runs only after `nodeReconnectWindow`, and `onNodeOffline` moves a sandbox only while it is still on that node; `seenAt` floors at the server start; `samePlacement` treats nil and empty maps alike. I found no regression.
- **Node events are checked against the sender where it matters**: `reconcileVM` ignores a VM of a sandbox that is not on the reporting node (and asks that node to delete it); `adoptQueuedMachine` needs `pb.Cleanup == nodeID`; `onGuestEvent`, hydration progress and `EvSnapshotFailed` need the sandbox on the node or a location; `Supersedes` needs `node_id=nodeID`; `afterSnapshotStored` moves a head only for the sandbox's own node.
- **Keys**: org keys are refused on `/api-keys` and account settings by `acct_guard.go`; `covers` handles sandbox and environment lists, `*`, expiry and in-sandbox keys; rotation of an expired or revoked key is refused; stop and delete revoke in-sandbox keys; org member removal revokes org keys; disabled users cannot use keys or sessions.
- **Sessions and cookies**: cookie writes need `X-CrowdVM-CSRF: 1`; the cookie is HttpOnly, SameSite=Lax, Secure with an https public URL; logout deletes the server side session; the tunnel websocket keeps the default origin check.
- **Device flow**: user codes are single-use (`status` is checked), approval needs a browser or CLI session (not an API key), agents cannot approve, an email claim needs the matching email, poll returns the token once per code in the sequential case.

## Not verified

- Bug 1 for images was not run separately (same handler, same missing check; bug 14 shows an image PUT is accepted from any node).
- Bug 11 does not need two join tokens of the same owner in every setup; I only tested the default (admin-owned tokens).
- Bug 7's agent variant (`handleDeviceApprove` with an unverified email) was read, not run.
- I did not run the CLI against a hostile `sshEndpoint`/`directEndpoints` string.
- Login brute force and email bombing were read, not driven (no throttle to compare with).
- Two timing-dependent tests (bugs 2 and 8) loop over several tokens or codes inside the test; they failed in all runs here (5 and 8 runs).
- No real node, QEMU, SMTP or browser; the fake node stands in for the node API.

## Fixes

All 15 fixed; each test in `internal/server/bughunt_r11_test.go` passes.

1. **Blob overwrite** (`nodes.go` `handleBlobPut`, `blobPutRefusal`): a stored blob is never replaced (a retry with the same checksum gets the stored one back, anything else 409). A recorded layer is accepted only from the node that took it or holds it; a layer not recorded yet is taken as before (GC and deletions erase what nothing records).
2. **Join token uses** (`nodes.go` `handleNodeRegister`): the use is taken first with `uses=uses+1 WHERE ... uses<max_uses` and `RowsAffected`.
3. **Node names** (`nodes.go` `uniqueNodeName`): registration, heartbeats and connects keep names unique (case-insensitive; a later node gets `-2`, `-3`); an admin rename to a taken name is 409 `node_name_taken`.
4. **Resume while stopping** (`lifecycle_place.go` `resumeStartChecks`): credits, concurrency and the start window are checked under `lockLimits` before `resumeDuringStop`.
5. **Idempotency on hook refusal** (`sandboxes.go`): `fail(err)`.
6. **Password change** (`auth.go` `SetPassword`): ends the account's other sessions (the changing session stays); admin reset and `set-password` end all.
7. **Unproven email** (`auth.go`): `PATCH /auth/me` and signup no longer store an email; they mail a code, and `POST /auth/me/email/verify {email, code}` stores it. Admin-set emails and existing rows are unchanged. Documented in `security.md` ("Your account").
8. **Email code attempts** (`auth.go` `consumeEmailCode`): the attempt is counted atomically before the compare.
9. **delete-own child keys** (`apikeys.go` `covers`): exact action match (plus `account.admin` → `account.read`).
10. **Long passwords** (`auth.go` `hashPassword`): over 72 bytes is 400 `password_too_long`.
11. **Node take-over** (`nodes.go`, `proto.RegisterRequest.NodeToken`, `node/join.go`): a node keeps its id on re-join only with its current node token, which the node sends only to the server it is joined to.
12. **Foreign layer holders** (`snapshots.go` `recordTaken`): a location is recorded only for the node that took the layer or runs its sandbox.
13. **Foreign layer sizes** (`snapshots.go`): only the taking or holding node sets `size_bytes`.
14. **Default image** (`nodes.go` `blobPutRefusal`): only trusted nodes and nodes of an administrator publish images (others get 403), so neither the default nor an image id can be taken by a community node. The test accepts that 403.
15. **Id-shaped usernames** (`auth.go`): `usr_` prefixes are refused; `UserByLogin` prefers an exact id.

`auth_test.go` logs in by name now (a signup email is no longer stored before it is confirmed).
