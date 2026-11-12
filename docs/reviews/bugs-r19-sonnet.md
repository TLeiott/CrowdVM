# Bug hunt r19 (sonnet)

2026-11-12. Product code unchanged. I only added `internal/server/bughunt_r19_test.go`, `internal/guest/agents/bughunt_r19_test.go` and this report. Unit tests only (the server's `servertest` environment with a fake node, fake guest, fake inspector and raw guest-channel clients; the agent's own `newEnv`). No cluster, VM, e2e test or sub-agent. Tree: `main` at `8c27d15`, in a detached worktree.

## What I audited

- **Round 18's fixes (`8c27d15`)**
  - `errRekeyPending` and every copy path: `handleCreateSandbox` (saved and shared deploys), `handleFork` (live and stopped), saved snapshots of a re-keying sandbox, shares, `ensureDiskKey` for rebuild, evacuation and resume (same sandbox, both keys kept), rollbacks (there is no user rollback; the "rollback" row in `ensureDiskKey` is only a legacy row shape). **Bug 1.**
  - `orgs.go` `handleRemoveMember` (`ownIDs`, the marker, the tx; `revokeTeammateKeys` on a sandbox with `org_id=''`): holds. `handleDeleteOrg` goes through the same members-first path, so no new hole there. **Bug 5** is a different hole in the same area (erase).
  - `acct_prompt.go` pre-flight skip (`conv != "" && prov == ""`): holds; a brand-new conversation id with no provider also skips the check, the guest then reports the missing credential itself (fine).
  - `diskSpec` / `snapshotDiskGiB` for forks and deploys: works as designed. It reads the source sandbox's *current* `extra.diskGiB`, not the one at snapshot time (see Code only).
  - `compat.go` `currentNodeOnline`, `mux.Join` (timeout-only close), `agents/store.go` `load`: **bug 6** (the in-memory conversations were left out of the fix).
  - `stranded.go` `pendingBoot.parent`: nothing stays for ever by mistake. I tested the one candidate: a deploy that waits for capacity, is stopped by its owner (state `archived`, `pendingBoot` kept), and its saved snapshot and source are removed. The parent layer is kept, and that is right: `resume` of the stopped deploy boots from `pendingBoot.parent` (checked: the node gets `ParentID`). Both tests of that were dropped.
  - `qemu.go` `waitBoot` / `rekeyProgressing` and `cmd/crowdvm-node/main.go` early `CheckUpdateStart`: read only (hardware-bound). `CheckUpdateStart` with `&node.Config{DataDir: g.dataDir}` is enough for the rollback (`rollback` uses only `DataDir` and the exe paths; the flag default is `DefaultDataDir()`).
- **Boat compatibility layer**: `feat_legacy.go` (**bugs 2, 3**), `acct_openapi.go` (the cache is a package global, so two servers in one process share one document; irrelevant for one server per process; YAML writer and operation ids fine), `docs/PARITY.md` rows HOOK-001..013, SNAP-015..036, DATA-001..016, BILL-001..026, OTEL-001..011 against code.
- **Snapshot files and the inspect VM**: `feat_snapfiles.go` access rules (`snapForRead`), inspector lifecycle, tree cache, `snapPath` (no escape out of `/home/user` or the volume dir), signed layer and inventory links (**bug 4**).
- **Retention and deletion**: `feat_retention.go` (close, reopen, erase, purge, retention job, ZDR; **bug 5**), `deletion_ops.go`.
- **Webhooks and OTel**: `feat_webhooks.go` (outbox, leases, rotate, tests of the URL guard), `feat_otel.go`, `outbound_guard.go`: nothing proven (see What held).
- **Billing**: `credits.go`, `acct_billing.go` (**bugs 7, 8**), `feat_saved_allowance.go`, `ratelimit.go` (start windows, extra starts).

## How to run

```
go test -count=1 -run TestBughuntR19 ./internal/server/ ./internal/guest/agents/
```

The 8 tests fail today (7 in `internal/server`, 1 in `internal/guest/agents`), all in under a second each. `go vet` of both packages and `gofmt -l` are clean. The tests use only existing helpers (`createSandbox`, `rawChannel`, `newFakeInspector`, `hget`, `newEnv`).

## Bugs proven by a failing test

### 1. A saved snapshot of a copy that was still re-keying can never be deployed or shared, but the refusal says "retry in a few minutes" (stuck, medium; regression of the round-18 fix)

- Test: `TestBughuntR19SavedSnapshotOfRekeyingCopyIsRefusedForEver` (`internal/server/bughunt_r19_test.go:37`)
- Code: `internal/server/guest_channel.go:145` (`snapshotRekeyPending` reads `snapshots.rekey_from`), `internal/server/sandboxes.go:946`, `internal/server/lifecycle.go:1748`; `diskRekeyed` (`guest_channel.go:162`) clears `rekey_from` only on `sandboxes` and `guest_channels`.
- Observed: a copy `cp` made with `newDiskKey` is ready and its guest is connected; the owner saves it as `r19mid` while it re-encrypts (the row copies `rekey_from`). The guest confirms (`POST /v1/rekeyed`, 204) and `cp` is clean. `POST /api/v1/sandboxes {"from":"r19mid"}` still answers `409 disk_rekey_pending … Retry in a few minutes.` with `retryable: true, retryAfterSeconds: 60`. The same for a share code of it, with or without `newDiskKey`.
- Expected: either the snapshot can be deployed once its source has finished (it carries both keys, as before round 18), or the refusal says it is permanent. A saved snapshot is meant to live for weeks (SNAP-016); the first 2 to 3 minutes of every `--new-key` copy are the window, and a snapshot row's `rekey_from` never changes afterwards. The same holds for `handleFork` with a head snapshot taken in that window (a running source is refused until its next periodic snapshot, although the fork takes a live snapshot of its own).
- Cause: round 18 treats "this snapshot row has a `rekey_from`" as "its sandbox is still re-keying". That is true only for the sandbox's own state. For a saved snapshot (and for an old head) the row keeps the value for good. The 409 is also unconditional: unflagged copies, which only need the keys the snapshot already carries, are refused too. A fix could check the *sandbox's* state for forks and refuse a saved snapshot only for `newDiskKey` copies (and say so, non-retryable), or let the copy continue the re-encryption to the same key as before.

### 2. Through the Box alias, a raw file upload with a JSON content type is parsed, renamed and re-encoded (wrong-result, medium)

- Test: `TestBughuntR19BoxAliasRewritesRawFileUploads` (`internal/server/bughunt_r19_test.go:78`)
- Code: `internal/server/feat_legacy.go:107` (`strings.Contains(r.Header.Get("Content-Type"), "json")` rewrites every body of an authenticated caller).
- Observed: `PUT /api/box/v1/sandboxes/{id}/files/raw?path=/home/user/x.json` with `Content-Type: application/json` and the body `{ "box": "mine",  "boxes": [1, 2] }`. The file in the sandbox holds `{"sandbox":"mine","sandboxes":[1,2]}`: keys renamed, whitespace gone. The same upload on `/api/v1` keeps the bytes.
- Expected: file bytes are not touched. Only the routes that take a JSON API body (everything but `files/raw` and the other pass-through uploads) should be rewritten; the response rewrite already leaves streams alone. A client that uploads `package.json` or a `.json` fixture with its natural content type corrupts it silently. The same renames apply to user-chosen keys in real API bodies (an env var named `box`, a label key `boxes` becomes `sandbox` / `sandboxes` in the request and back in the response).
- Cause: the alias decides by header, not by route.

### 3. Through the Box alias, raw uploads over 8 MiB fail with a retryable 502 (wrong-result, medium)

- Test: `TestBughuntR19BoxAliasCapsRawUploads` (`internal/server/bughunt_r19_test.go:102`)
- Code: `internal/server/feat_legacy.go:103` (`http.MaxBytesReader(w, r.Body, 8<<20)` around every body).
- Observed: a 10 MiB `PUT …/files/raw` with `application/octet-stream` works on `/api/v1` (200, `size` 10485760) and answers `502 box_direct_failed … http: request body too large, retryable: true` on `/api/box/v1`.
- Expected: the alias has no tighter limit than the route behind it. The 8 MiB cap belongs to JSON binds (`Ctx.bind`, which enforces it itself). The harbor and eve adapters this alias exists for (EVAL-004/005) `upload` and `write` files; anything above 8 MiB fails, and the `retryable: true` makes a client retry for ever.
- Cause: the wrapper is applied before the route is known; the proxy then reports the read error as a node failure.

### 4. The inventory link in a saved snapshot's download manifest answers 404 (wrong-result, low)

- Test: `TestBughuntR19SavedSnapshotInventoryLinkWorks` (`internal/server/bughunt_r19_test.go:125`)
- Code: `internal/server/feat_snapfiles.go:882` (`handleInventory`: `sn.DeletedAt != nil` is 404), against `snapForRead` (`feat_snapfiles.go:115`), which lets the saver read a deleted layer (SNAP-032) and `handleDownload`, which builds the link.
- Observed: save a sandbox as `r19inv`, delete the sandbox (the layer gets `deleted_at`, the saved snapshot keeps it), `GET /snapshots/{snapshotId}/download` as the saver answers 200 with an `inventory.signedUrl`; fetching it answers `404 Snapshot not found`.
- Expected: 200 (the tree of the layer), or no inventory entry in the manifest.
- Cause: the unauthenticated handler re-checks the row's `deleted_at` without the saved-snapshot exception of the authenticated path. The layer URLs next to it (`handleBlob`) do work.

### 5. Erasing an account deletes its organization memberships, also the owner's: the organization is left without an owner (stuck, medium)

- Test: `TestBughuntR19EraseLeavesOrgWithoutOwner` (`internal/server/bughunt_r19_test.go:163`)
- Code: `internal/server/feat_retention.go:387` (`DELETE FROM org_members WHERE user_id=?` in `erase`, no role check).
- Observed: `r19owner` creates org `R19Org`, `r19member` joins. The owner erases the account (browser session, `erase my account`): 200. `org_members` then holds one row, `r19member` as `member`, and no `owner`.
- Expected: `409 owner_cannot_leave`-style refusal ("transfer ownership first") like `handleRemoveMember` and `handleDeleteOrg` use, or ownership passes on.
- Cause: `erase` removes memberships without the owner check. Everything that needs `owner` (rename, delete, transfer, credits out of the wallet, promoting an admin) is then impossible for ever; the org's wallet is stuck, and its other members keep working in an org nobody can manage. The erased user's org sandboxes are deleted too by `purge` (`owner_id=uid`), also those shared with the team.

### 6. Zero data retention: `GET /v1/conversations` still serves earlier prompt previews after the agent restarted (leak, low; incomplete round-18 fix)

- Test: `TestBughuntR19ZeroRetentionStillServesEarlierPromptsFromMemory` (`internal/guest/agents/bughunt_r19_test.go:16`)
- Code: `internal/guest/agents/store.go:78` (the new in-memory reset clears `m.events`), but `m.convs` is filled from `state.json` earlier in `load` and `m.prompts` right after (`store.go:84`), and only the *file* is rewritten without text (`saveLocked`).
- Observed: a prompt `r19-earlier-secret-prompt` runs without retention. A new `Manager` on the same data dir with `ZeroDataRetention` on: `GET /v1/conversations` answers `"lastPromptPreview":"r19-earlier-secret-prompt"`. `state.json` is clean.
- Expected: no text from before retention was on, in memory either (round 18's own wording: "drops earlier events from memory"). `/v1/prompts/{id}` carries no text, so the preview and the steer messages are what is left.
- Cause: the fix cleared one of three in-memory copies. `m.convs[...].LastPromptPreview` and `m.steers[...].Message` need the same treatment in `load` (and a live switch to retention should do the same).

### 7. The monthly quota expiry takes credits that were never quota (wrong-result, medium)

- Test: `TestBughuntR19QuotaExpiryEatsOtherCreditsAfterQuotaWasSpent` (`internal/server/bughunt_r19_test.go:192`)
- Code: `internal/server/acct_billing.go:371` (`expire := math.Max(0, math.Min(bal, st.QuotaGranted))`).
- Observed: wallet with a 10 credit monthly quota. The month's 10 are granted, then spent in full; an admin grants 100. At the next month change the balance is 200 (base 100, plus 100) where it should be 210 (everything held plus the new 10): the expiry removed 10 credits that were the admin's grant.
- Expected: only the *unused* part of the quota expires: `min(balance, granted - used this month)`, with the usage from the ledger (`spentThisMonth` has it).
- Cause: the code expires `min(balance, granted)` whatever was spent. Any wallet whose balance is above its quota (node earnings, grants, transfers, the part of a quota that was spent while other credits remain) loses up to a full quota every month. A node host with a quota policy pays it out of what the node earned.

### 8. A resume that names no type ignores the admin's `allowedSizes` policy (security, low; policy bypass)

- Test: `TestBughuntR19ResumeIgnoresAllowedSizesPolicy` (`internal/server/bughunt_r19_test.go:225`)
- Code: `internal/server/acct_billing.go:853` (`policyGuard`: `inherited` is set only for `fork` and for a create from a saved snapshot; for `resume` nothing, so `t == ""` skips the size check).
- Observed: the user has a stopped `large` sandbox. The admin sets `allowedSizes: [small, default]` for the wallet. `POST /sandboxes {type: large}` answers 403 `machine_class_plan_required`. `POST /sandboxes/{id}/resume` with no body answers 202 and starts the `large` machine.
- Expected: the same 403 (the sandbox starts at the size the policy forbids), with a hint to resume with `type: small`.
- Cause: bugs-r10-sonnet 4 closed this for fork and for `from`, not for the third start path. The wallet restriction (BILL-022, the trial replacement) can be dodged by anyone who created their sandboxes before the policy was set.

## Code only

- **A saved snapshot that is still `saving` survives enabling zero data retention** (`internal/server/feat_retention.go:188`, `applyZDR`: `DELETE … AND status!='saving'`; wrong-result, low). The save that started before the PATCH finishes later and the row stays, though ZDR forbids saved snapshots and the guard only stops *new* saves. I could not hold a save open with the fake node without a race, so no test. A fix cancels (or deletes at completion) saves that run when ZDR is on.
- **A client reset in the middle of a tunnelled upload now reaches the backend as a clean end of input** (`internal/mux/mux.go:256`, the round-18 change in `Join`). A read error on `a` (not EOF, not a timeout) is no longer "close both"; it falls through to `CloseWrite` on `b`. A backend that treats EOF as "the stream is complete" (`tar | nc`, chunked or length-less bodies, raw protocols) sees a truncated upload as finished. HTTP with a `Content-Length` notices. Whether this matters depends on the backend; a deterministic test needs a protocol that cannot tell. The round-18 trade-off (early 413 reaches the client) is right for write errors; for *read* errors close-both is still the safer choice.
- **`snapshotDiskGiB` and the fork copy take the source's disk size at deploy time, not at snapshot time** (`internal/server/scheduler.go:92`, `lifecycle.go:1867`). A sandbox that grew to 111 GiB after the saved snapshot was taken (`small`, 51 GiB at that time) gives every later deploy of the old snapshot a 111 GiB record and, on the node, a 111 GiB disk (`size := max(want, base.vsize)` follows what the server sends). Harmless for data; it over-reserves and shows a size the snapshot never had (cosmetic).
- **`otelSandboxResource` still reports the type's disk, not the recorded one** (`internal/server/feat_otel.go:131`; cosmetic). `boat.sandbox.disk.size` says 51 GiB for a sandbox that runs on 111 GiB (the API got the fix in round 17).
- **A second start of a node that fails on the data-dir lock counts as a start of the update** (`cmd/crowdvm-node/main.go:217`, the early `CheckUpdateStart`; stuck, low). Counting moved in front of `LockDataDir`. Within the seconds between an update and its first heartbeat, an overlapping start that dies on the lock (for example `Restart=always` racing a slow shutdown, or two units) adds to the 3 starts a new build gets, and a later start rolls the good build back and skips its version. Needs a supervised duplicate start in that window; not tested.
- **`snapshot_shares` rows are never removed with their saved snapshot** (`internal/server/snapshots.go:1033`, `handleDeleteSaved`; leak, cosmetic). They are harmless (`resolveFrom` ends in `snapshot_not_found`) but stay for ever.

## What held

- **Round 18 disk key**: both keys stay on every ticket, nonce retry, stop layer, rebuild, evacuation and resume of the *same* sandbox until the confirmation; `ensureDiskKey`'s first case covers a snapshot of the sandbox itself taken in the pending window; there is no rollback route that could revert a key. `placeSandbox` reads `rekey_from` / `newDiskKey` from the row (`scheduler.go:321`).
- **`handleRemoveMember`**: the running-sandbox check and the id collection are in one tx; `revokeTeammateKeys` on a sandbox with `org_id=''` treats every non-owner key as stale; a service account is refused; owner cannot leave. `handleDeleteOrg` needs the owner as last member, so every earlier member removal already revoked keys.
- **`stranded.go` pending parent**: kept exactly as long as the boot can still use it (resume of a stopped queued deploy boots from it).
- **Boat alias**: header, path and error-code renames; NDJSON and raw responses pass through; `Unwrap` keeps `Flush`/`Hijack`; 204 and empty bodies; anonymous requests are not read (round 7 fix).
- **OpenAPI**: Boat operation ids and parameters, wildcard expansion, unique ids, YAML escaping.
- **Snapshot files**: relative paths cannot leave `/home/user` (`..` refused, `__dockervol__` mapped under `volDir/_data`), absolute paths go through the read-only bind mount, symlinks are not followed, encrypted chains refuse the inspector, the tree cache is per snapshot and filled only on success, authorization for another user's snapshot is the sandbox's own access check.
- **Retention**: `erase` purges credentials, CI, webhooks, OTel, keys (round 10 list); close/reopen windows; ZDR on every stop; retention job marks expired stopped sandboxes and old layers, refcounted GC keeps what heads, forks, saved snapshots and queued boots need.
- **Webhooks**: outbox in the state-change transaction, leases and per-owner fairness, 8 attempts, no redirects, URL guard on save and on every dial (IPv4-mapped IPv6, zones, metadata addresses), delete removes queued deliveries; rotate signs new attempts with the new secret only (the grace is the receiver's, as documented).
- **OTel**: per-account flush, `Retry-After`, backlog limit, header names only, endpoint normalisation, the test export on save.
- **Billing**: transfer is one statement (no overdraw), rounded-balance clamp, scope of `from`; the credit cap and member cap read the ledger by actor; grace and early stop use the right wallet; start windows and extra-start refusals; policy guard spelling check (`Type` vs `type`).

## Not verified

- Bug 1 with a live fork of a *running* confirmed copy whose head is a periodic snapshot from the pending window (needs a node that reports a periodic snapshot; I tested the saved-snapshot case).
- Bug 2's effect on the real `harbor-box` and `eve-box` adapters (their raw uploads may send `application/octet-stream`; bug 3 holds for them either way above 8 MiB).
- Bug 5's later steps (what the dashboard shows for an ownerless org); the test stops at the membership table.
- The retention job with real clocks (`run` and `purgeClosed` read at the 15-minute tick; I read the code and did not move time).
- `qemu.go` `waitBoot` / `rekeyProgressing` on a real re-encryption (a console that keeps "re-encrypting" in its last 2000 bytes after a failed post-rekey boot would keep waiting up to 2 hours: read only).
- Windows, the TypeScript and Python SDKs, the dashboard.

## Fixes

All 8 fixed; every `TestBughuntR19` test passes.

1. **Saved snapshot of a re-keying copy refused for ever as "retry"** (`guest_channel.go` `copyRekeyErr`): a copy of a snapshot with a pending re-key is retryable (409 `disk_rekey_pending`) only while its sandbox still re-keys; afterwards it is refused for good with 409 `snapshot_taken_during_rekey` (not retryable, "save a new snapshot"). A fork of a running source is checked against the source only (it takes a live snapshot of its own).
2. **Box alias rewrote raw file uploads** and 3. **capped them at 8 MiB** (`feat_legacy.go`): `files/raw` bodies are passed through untouched, and the 8 MiB cap applies only to the JSON bodies the alias rewrites.
4. **Inventory link of a saved snapshot** (`feat_snapfiles.go` `handleInventory`): a deleted layer that a saved snapshot still holds is served.
5. **Erase left an org without an owner** (`feat_retention.go` `erase`): refused (409 `owner_of_organization`) while the account owns an organization with other members.
6. **Zero retention kept prompt text in memory** (`agents/store.go` `load`): previews, prompt texts and steer messages are cleared in memory at start too.
7. **Quota expiry took other credits** (`acct_billing.go`): only the unused part of the quota expires: the quota granted minus every debit since that grant.
8. **Resume ignored `allowedSizes`** (`acct_billing.go` `policyGuard`): a resume without a type is checked against the size it keeps.

Code only, also fixed: `mux.Join` aborts both ends when a read fails (the sender broke off) and resets a TCP receiver, so a truncated upload is not passed on as complete; only a write that failed without a timeout half-closes (an early answer still arrives).

Not fixed: a save running when zero retention is switched on, a deploy's disk size taken from the source at deploy time, the OTel disk size attribute, a lock-failed start counted for an update, `snapshot_shares` rows left after a delete.
