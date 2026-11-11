# Bug hunt r6 (sonnet)

2026-11-11. Audited the uncommitted master tree. Product code unchanged. Only unit tests were used: SQLite, httptest, the fake node and guest, temp dirs and short-lived `sh`/`sleep` children. No cluster, VM or e2e test was started.

I audited the CLI, node, server credits/org/OTel/desktop/host code and the round-5 fixes myself. Two sub-agents covered the rest:
- One took the migrations, webhooks and pagination, and wrote `internal/server/bughunt_r6b_test.go`.
- The other took `internal/mux` and the guest exec and process code, and wrote `internal/mux/bughunt_r6_test.go` and `internal/guest/bughunt_r6_test.go`.

I re-ran every test below myself. All of them fail, also under `-race`, for the reasons given. Exception: `TestBughuntR6HistorySaveRace` is timing dependent and passed once under `-race`. Without `-race` it failed 5 of 5 runs.

Run each with `go test -count=1 -run <Name> ./internal/<pkg>/`. Test files:
- `internal/cli/bughunt_r6_test.go`
- `internal/node/bughunt_r6_test.go`
- `internal/mux/bughunt_r6_test.go`
- `internal/guest/bughunt_r6_test.go`
- `internal/server/bughunt_r6_test.go` (mine)
- `internal/server/bughunt_r6b_test.go` (sub-agent)

No security or high-severity bug turned up. The worst ones are silent data loss in `crowdvm scp -r` / download (hardlinks, long names) and a corrupt or stale `meta.json` in the guest history.

## Bugs

### 1. `extractTar` silently drops hardlinked files

- Severity: data-loss (silent).
- Test: `TestBughuntR6ExtractTarHardlinkDropped` (cli).
- Where: `internal/cli/transfer.go:443-452`. The `switch h.Typeflag` has `TypeDir`, `TypeSymlink` and `TypeReg`, and no `TypeLink`.
- Observed: a download archive with `d/a.txt` and `d/b.txt` as a `TypeLink` to `d/a.txt` extracts `a.txt` only. There is no error, and the file count is lower.
- Expected: `b.txt` exists with the same content.
- Cause: `downloadDir` runs plain `tar -czf` on the sandbox. GNU tar stores a second name of an already archived inode (`cp -al`, pnpm stores, git objects, `node_modules/.cache`) as a hardlink entry. The extractor ignores that entry type.

### 2. `writeFile` fails for file names above about 228 bytes, which aborts the whole download

- Severity: wrong-result.
- Test: `TestBughuntR6ExtractTarLongFileName` (cli).
- Where: `internal/cli/transfer.go:314`: `os.CreateTemp(dir, base+".crowdvm-partial-*")`.
- Observed: a regular 250-byte file name makes `extractTar` return `file name too long`. The error stops the extraction of every later entry.
- Expected: the file is written. 250 bytes is under NAME_MAX (255).
- Cause: the temp name appends `.crowdvm-partial-` and a random suffix to the target's base name, so it overflows NAME_MAX before the target does. `crowdvm scp ID:/file local` goes through the same function.

### 3. A unix socket in a pushed or copied tree aborts the whole upload

- Severity: wrong-result.
- Test: `TestBughuntR6WriteTarGzSocketAbortsUpload` (cli).
- Where: `internal/cli/transfer.go:102` (`tar.FileInfoHeader`). The caller is `writeTarGz`, used by `push` and `scp -r`.
- Observed: `archive/tar: sockets not supported`, and nothing is uploaded.
- Expected: the socket is skipped, as GNU tar does with a warning. A non-git `push` walks every file, so a dev server's `*.sock` or an `ssh-agent` socket in the directory is enough.
- Cause: the error is returned from the walk callback. There is no skip for non-regular, non-symlink, non-dir, non-fifo entries.

### 4. A taken-over config lock is removed by its old holder

- Severity: data-loss (rare). It needs a holder that stalls for over 30 s, for example a suspended terminal.
- Test: `TestBughuntR6ConfigUnlockRemovesTakenOverLock` (cli).
- Where: `internal/cli/config.go:143` (unlock is `os.Remove(path)`) against the takeover at `:149`.
- Observed: A takes the lock, B takes it over after 30 s, and A's unlock deletes B's lock file. A third process then enters the critical section next to B, and the read-modify-write is no longer exclusive.
- Expected: unlock removes the lock file only if it is still its own.
- Cause: the lock file has no owner token. Two waiters that both see the stale lock can also both delete it and both enter (not tested).
- Related, not tested: a lock left by a killed process blocks every command for up to 30 s. A process that waits 10 s fails with "config is locked".

### 5. Saving the config drops fields written by a newer crowdvm

- Severity: data-loss (low, version skew).
- Test: `TestBughuntR6ConfigSaveDropsUnknownFields` (cli).
- Where: `internal/cli/config.go:106` (`loadConfig` into the typed struct) and `:164` (`fromMap` starts from `Config{}`).
- Observed: an unknown top-level key and an unknown profile field are gone after any `updateConfig`, also when the command changed something unrelated.
- Expected: fields the binary does not know stay in the file. The round-5 `merge3` design already keeps fields a command did not touch, but only for the known ones.
- Cause: the merge works on `toMap(typed struct)`, not on the file's own JSON. An older binary on the same machine as a newer one (self-update, devcontainers, WSL) erases the newer settings.

### 6. Node: `once` lets a second run of the same transfer start while an abandoned one still runs

- Severity: leak (low).
- Test: `TestBughuntR6OnceOverlapsAbandonedRun` (node).
- Where: `internal/node/transfer.go:62`: `delete(m, key) // a later caller starts afresh`.
- Observed: the last waiter cancels and leaves, and the key is dropped from the map at once. The old goroutine keeps going if its `fn` does not honour the context. A new caller then starts `fn` again, and two runs for one blob overlap. In the test the peak is 2.
- Expected: the key stays until the old run has returned, or the new caller waits for it.
- Cause: the map entry is removed on cancel, not when `fn` returns. `Store.ImportImage` (`internal/node/vm/qemu/store.go:180`) and the `qemu-img convert` call take no context. Two imports of one image id move or convert onto the same `disks/image-<id>` path. The window is short, and both write identical content. The `transfers` progress key `dir/kind/id` is shared too, so the first to finish deletes the other's entry (cosmetic).

### 7. Org delete leaves everything keyed by the org id

- Severity: leak.
- Test: `TestBughuntR6OrgDeleteLeavesOrgDataBehind` (server).
- Where: `internal/server/orgs.go:365-415` (`handleDeleteOrg`).
- Observed: after `DELETE /orgs/{org}` returns 200, `environments.owner_id` still holds the org id (2 rows: `base` and `ci`). Their `environment_versions` (sealed vars and secret files) stay too. The test scans every table with an `owner_id` or `org_id` column. It skips `credit_ledger`, `audit_events` and `wallets`, which are history.
- Expected: the org's environments and secrets go with the org, as round 5 did for saved snapshots.
- Cause: the delete transaction handles sandboxes, runs, saved snapshots, active orgs and API keys. Other tables have no foreign key to `orgs`. `ci_links.org_id` was already noted in round 5.

### 8. Webhook: a delivery with an unreadable secret stays `pending` for ever

- Severity: stuck.
- Test: `TestBughuntR6bWebhookUnreadableSecretStuckPending` (server, sub-agent).
- Where: `internal/server/feat_webhooks.go:388-390`.
- Observed: after the stored secret is corrupted or `secret.key` is replaced, the delivery stays `pending` with `attempts=0` and no `last_error`. Nothing is sent. Every 15 s the lease expires and it is claimed again.
- Expected: the attempt is counted or the delivery ends as `failed` with an error visible in the delivery log.
- Cause: the branch logs and returns without updating the row. The user sees nothing, and the claim loop retries for ever.

### 9. Webhook: one account's hanging endpoints delay every other account's deliveries

- Severity: leak (availability, low).
- Test: `TestBughuntR6bWebhookSlowEndpointBlocksOthers` (server, sub-agent). The test shortens `WebhookTimeout` to 4 s.
- Where: `internal/server/feat_webhooks.go:329-352` (`whParallel = 16`, due list `ORDER BY next_attempt_at LIMIT 100`).
- Observed: with 16 deliveries to hanging endpoints of one account in flight, a healthy webhook of another account is delivered only after about 4.4 s, the full endpoint timeout.
- Expected: no account takes every slot for the others. A per-webhook or per-owner limit would do it.
- Cause: the slots are global, filled oldest first. Any user can register a tarpit endpoint and keep the pool busy. Backoff only pushes the hung rows out after their first failure.
- Related, not tested: pending retries go on after the webhook's event list or URL is edited, because the retry does not re-check the subscription.

### 10. Webhook deliveries log: invalid limits are accepted, big limits return fewer rows

- Severity: wrong-result.
- Test: `TestBughuntR6bWebhookDeliveriesLimit` (server, sub-agent).
- Where: `internal/server/feat_webhooks.go:827-829`.
- Observed: with 150 rows, `limit=500` returns 150, `limit=1000` returns 100, and `limit=0`, `-5` and `abc` return 200 with 100 rows.
- Expected: `limit=1000` gives at least as many rows as 500, and an invalid limit gives 400 `invalid_limit`, as the other list endpoints do.
- Cause: any value outside 1..500 resets to 100. There is also no cursor, so rows past the limit cannot be reached.

### 11. `GET /sandboxes?namePrefix=` never matches a non-ASCII prefix

- Severity: wrong-result.
- Test: `TestBughuntR6bNamePrefixNonASCII` (server, sub-agent).
- Where: `internal/server/sandboxes.go:531-533`: `substr(name,1,?)=?` with `len(v)`.
- Observed: for a sandbox named `élan-vital`, `namePrefix=élan` and `namePrefix=é` return an empty list.
- Expected: both return the sandbox.
- Cause: Go's `len` counts bytes. SQLite's `substr` on TEXT counts characters. `é` is 2 bytes and 1 character.

### 12. Command history: a bad or removed cursor gives an empty "last page" instead of an error

- Severity: wrong-result.
- Test: `TestBughuntR6bCommandHistoryBadCursor` (server, sub-agent).
- Where: `internal/server/feat_history.go:455-462`.
- Observed: `cursor=garbage` returns 200 with no commands and `nextCursor: null`.
- Expected: 400 `invalid_cursor`, like `ParsePage` for the other lists. A cursor whose row was removed by the retention delete (`feat_history.go:340`) also ends the paging with an empty page, so a client that pages across a retention run skips everything after it.
- Cause: an unknown cursor becomes `AND 0`.

### 13. Events list: `limit` and `sort` are not validated

- Severity: cosmetic.
- Test: `TestBughuntR6bEventsLimitSortValidation` (server, sub-agent).
- Where: `internal/server/feat_history.go:667-674`.
- Observed: `events?limit=1000` and `events?sort=bogus` both return 200. The sandbox and snapshot lists answer 400.
- Cause: the limit is clamped with `min(n,200)`, even though the error text says "limit must be 1..200". Any `sort` other than `asc` means `desc`.

### 14. Admin sandbox list: `state=stopped` returns nothing, and `limit`, `cursor` and `sort` are ignored

- Severity: wrong-result.
- Test: `TestBughuntR6bAdminSandboxesList` (server, sub-agent). The test proves the alias only.
- Where: `internal/server/account.go:331-346` (`handleAdminSandboxes`).
- Observed: with one archived sandbox, `/sandboxes?state=stopped` returns 1 row and `/admin/sandboxes?state=stopped` returns 0.
- Expected: the same result.
- Cause: the admin handler has no `stopped` to `archived` alias. It also has a hard `LIMIT 500` and no `pageInfo`, so an admin cannot see sandbox 501 on a large server (evident in the code, not tested).

### 15. Concurrent `MigrateAll` on one database file fails

- Severity: stuck (startup race).
- Test: `TestBughuntR6bConcurrentMigrate` (server, sub-agent).
- Where: `internal/server/store/store.go:107-120` (`Migrate`).
- Observed: two opens of the same file that call `MigrateAll` at once make one fail, for example with `migrate core v1: table settings already exists`.
- Expected: both succeed.
- Cause: the "already applied" `SELECT COUNT(*) FROM schema_migrations` runs outside the transaction that applies the SQL, so both callers can pass it. It matters for a supervisor that starts a second server process during a restart, or an admin command run next to the server.

### 16. Guest history: `meta.json` can be corrupt or stale after concurrent saves

- Severity: data-loss.
- Test: `TestBughuntR6HistorySaveRace` (guest, sub-agent). It runs 8 goroutines on one record. It fails in 5 of 5 runs without `-race`.
- Where: `internal/guest/history.go:102-110` (`save`).
- Observed: `meta.json` is invalid JSON (`invalid character '"' after top-level value`) or old (rev 200 on disk against 201 in memory).
- Expected: the file is valid and equals the final in-memory record.
- Cause: `json.Marshal` runs under `h.mu`, but `os.WriteFile(p+".tmp")` and `os.Rename` run after the lock is released. All writers share one `.tmp` path, so two saves interleave on it, and an older marshal can land after a newer one. Callers are `finish` (command exit), `ack` and `update`. After an agent restart, `openHistory` and `recoverProcs` drop or misread such a record, and the command's result is lost. It is the same pattern as `bugs-r5-sonnet` 11, in the guest.

### 17. Guest detached kill leaves SIGTERM-ignoring children alive

- Severity: leak.
- Test: `TestBughuntR6KillLeavesTermIgnoringChild` (guest, sub-agent). It fails every time. The command is `(trap '' TERM; exec sleep 300) & wait`.
- Where: `internal/guest/commands.go:506-516` (`handleCommandKill`).
- Observed: `DELETE /v1/commands/{pid}` answers `signaled: true`, and the `sleep` is still alive in the process group afterwards.
- Expected: the whole group is dead, as the endpoint's doc says ("a detached command and its children").
- Cause: SIGTERM goes to the group, then the code waits on `p.done`, which closes when the leader exits. The leader exits on SIGTERM, so the SIGKILL escalation is skipped while children survive. After that, `p.finished()` is true and the orphan can no longer be killed through the API. The existing kill test passes only because its children die on SIGTERM.
- Note: I found a stray `sleep 300` after running this test, and killed it by hand. The test leaves it behind by design, because it is the bug.

### 18. Guest `a.procs` is never pruned, so pruned commands are still served

- Severity: leak (cosmetic on the result side).
- Test: `TestBughuntR6PrunedDetachedStillServed` (guest, sub-agent).
- Where: `internal/guest/commands.go:119` and `:187` add to `a.procs`. Nothing deletes. `lookupProc` is at `:355`.
- Observed: after `hist.prune` removes the record, `GET /v1/commands/{pid}` still returns 200 with `known: true` and empty output. The map grows by one entry per detached command for the life of the agent.
- Expected: 404, as for an unknown pid.

### 19. `mux.Join` loses the reply after a TCP half-close

- Severity: wrong-result.
- Test: `TestBughuntR6JoinHalfCloseLosesReply` (mux, sub-agent). It fails every time.
- Where: `internal/mux/mux.go:145-151`.
- Observed: a client sends `req`, calls `CloseWrite`, and the backend replies after reading to EOF. The client receives `""`.
- Expected: `"reply to req"`.
- Cause: when either copy direction reaches EOF, `closeBoth` closes both ends. The other direction is never drained. This hits `printf req | nc host port` style clients over tunnels, direct ports and passthrough, because they all go through `Join`.

### 20. Guest stdout/stderr files of commands have no size cap (code only, no test)

- Severity: stuck / data-loss (code only).
- Where: `internal/guest/commands.go:96-101` creates stdout and stderr with plain `os.Create`. There is no `RLIMIT_FSIZE` and no rotation. `maxSyncOutput` caps only what is read back.
- Impact: `yes` as a detached command grows the log without limit. Unmonitored sandboxes keep history on tmpfs `/run` (`history.go` `openHistory` and `unmonitor`), so the growth eats RAM.

## Checked and held

Round-5 fixes:
- **CLI config:**
  - A save re-reads under the lock and applies only the changed fields (`merge3`). A logout, a login or a new `current` done meanwhile by another process survives.
  - A profile switch (`config use`) during a save is kept, because the `profile` key is untouched.
  - `CreateTemp` makes unique temp names, the file is 0600, and a corrupt `config.json` refuses the save instead of being replaced.
  - A temp file is removed on each failure path.
  - `sameServer` ignores case, default ports, a trailing `/` and `/api/v1`. It does not match `x.com` against `https://x.com`, a safe miss (the key is dropped). It never matches different hosts or schemes, including `https://x.com@evil.com`.
- **CLI `extractTar`:**
  - A symlink earlier in the archive is refused as a parent of later entries (`checkNoSymlinkParents`).
  - A later file entry replaces a planted `*.crowdvm-partial` symlink and does not follow it.
  - `..` and absolute names are cleaned and refused (`path.Clean("/"+name)` and the `Rel` check, which also catches backslash names on Windows).
  - Device and fifo entries are ignored.
  - A truncated stream removes its temp file and returns an error.
  - Not testable here: case-insensitive collisions on macOS and Windows, where the second entry silently overwrites the first. Also on Windows, `os.Rename` onto `config.json` can fail while another process has it open for a read (the readers take no lock).
- **CLI exec text mode:**
  - The stream path returns 124 for `timedOut`, `128+n` for a signal with no code, and an error for an `error` frame.
  - A stream that ends without an exit frame gives an error and exit 1.
  - `--json` keeps the buffered path with its truncation flags.
  - Not a bug: Ctrl-C on a stream returns the context error, where the long path returns 130. The sync path behaved the same before round 5.
- **Node transfer:** every waiter returns on its own context. Cancel happens when the last waiter leaves. A joiner whose caller was cancelled gets a fresh run. `c.err` is written before `close(done)`, so there is no race (`-race` clean). `.part` files are removed only in `Node.Run` at start, so they cannot race an active download. The only hole is bug 6.
- **Fork queued with a live snapshot:** `takeLiveSnapshot` runs on the request context before `queueBoot`. The layer is recorded under the source sandbox, and GC roots include every non-deleted snapshot. If the fork is deleted while queued, the layer stays with the source's chain, as for a non-queued fork. `tryPlace` skips deleted sandboxes. Not proven: `queueBoot`'s UPDATE has no `deleted_at` guard (`lifecycle_place.go:120`), so a fork deleted during the live snapshot can get `state='cloning'` on a deleted row. It is harmless because every reader filters `deleted_at`.
- **`walletDebit`:** the 5e-7 tolerance can only lower the amount to the exact balance. For a balance at or below zero it ends in `insufficient`, and the transaction rolls back. Credit and debit use the same `took`, so no money is created. `+Inf` fails `balance>=amount`, and NaN is refused by the handler.
- **OTel:**
  - Per-account flush goroutines are bounded by `flushing[owner]`, each ends with the loop context, and `defer wg.Wait()` waits for them.
  - `pruneCPU` runs after each metrics tick.
  - `DELETE /telemetry` clears logs, backlog and hold.
  - A failed non-retryable window is dropped.
- **Desktop:** a spent token is refused before the VNC dial. `claim` is atomic and expires after `exp+1`. Minor: two connects with one token at once both pass `spent` and both dial before one claim fails (cosmetic).
- **Org delete:** it moves sandboxes, open runs and tails, and saved snapshots with name de-duplication, and revokes keys. The only gap is bug 7. Member removal refuses while runs are open.
- **Service-account orphan rule:** `u` is only read when `err` is not "no rows", so there is no nil dereference. Admins and org owners and admins delete service-account and disabled-owner sandboxes, and API-key limits still apply.
- **`outbound_guard` `parseIP`:** zones are stripped and v4-mapped addresses are unmapped. Brackets are removed by `Hostname()`. `127.1` and other odd forms are resolved by the OS and then caught by the dial hook. Not covered, by design: a NAT64 prefix, and Azure's `168.63.129.16`, which is not on the metadata list.
- **`feat_host` `track`:** keys are per-closure pointers, the map is emptied on `done`, and a hide during the start wait cuts the request. The remaining window is between `load` and `track`, microseconds wide.
- **`lifecycle_holds` notice clearing:** hold warnings clear on recovery and delete. `forced_stop` and `stop_forced` stay on purpose.
- **`guest/desktop.go` `patchOldImage`:** it runs only when privileged, skips an already patched script, and replaces the first `-SecurityTypes None` only. The shipped image script and unit start the script directly (`ExecStart=...desktop-session`), so the drop-in works. It would break only if an old script quoted that text as one argument, which I could not confirm. It needs root paths, so there is no unit test.

Unaudited areas:
- **Migrations:**
  - A fresh database and one built by applying every feature in reverse name order give an identical `sqlite_master`, with the same tables, columns and indexes.
  - A second `MigrateAll` is a no-op.
  - The history versions registered as 1, 3, 2 apply in order.
  - The saved-snapshots v1 migration works on legacy rows with duplicate names, tied `created_at` and 63-character names.
  - The only defect is bug 15.
- **Webhooks:**
  - An endpoint that always returns 500 ends as `failed` with `attempts=8` and 8 hits.
  - Retry-to-success, signatures, rotate and delete all work (delete removes pending rows).
  - The `(webhook_id,event_id)` unique key and the lease stop double delivery.
  - Deleting an account removes its hooks and deliveries.
- **Pagination:** with five sandboxes at one `created_at`, and eight audit events at one timestamp, `limit=2` in both orders gave no duplicates or skips. The sandbox and snapshot lists filter `deleted_at` and owner or org. The audit list is scoped to the actor unless the caller is an admin on `/admin/`.
- **`internal/mux`:**
  - Five sessions of Accept, Dial, Serve and Client with requests left no goroutine after both sessions closed.
  - `SetReadLimit(-1)` is fine because `NetConn` streams and yamux bounds a frame by its 16 MiB window.
  - `HijackUpgrade` callers handle bytes already buffered in the bufio reader.
  - Not a bug I could prove: `DialUpgrade` has no deadline on the 101 read (`internal/node/vms.go:786`, `internal/server/nodes.go:198`). A peer that accepts and stays silent holds the call for ever. Fixing it needs an API change.
- **Guest exec:**
  - Every `cmd.Wait()` runs in a goroutine and stdout and stderr are files, so there are no zombies.
  - A timeout kills the group with SIGKILL and reports `timed_out`.
  - Exit codes map signals to `killed` and `Signal`.
  - `pidAlive` compares `starttime` and treats zombies as dead, so a reused PID is not mistaken for the old process.
  - `/proc/<pid>/stat` parsing is right, `commandEnv` de-duplicates and filters names, and the cwd is validated.
  - Not proven: a sync or stream client that disconnects leaves the command running until its timeout.

**TL;DR:** 19 bugs with failing tests and one code-only note, no security finding and nothing high-severity. The ones that lose data are the CLI download dropping hardlinked files and failing on long names (and a socket aborting a push), the guest `meta.json` race, and an old config lock holder removing a new owner's lock. The server issues are mostly small: org delete leaves environments and secrets behind, an unreadable webhook secret leaves deliveries pending for ever, one account can stall webhook delivery for others, a concurrent `Migrate` fails, and several list endpoints validate `limit`, cursor, state and non-ASCII prefix inconsistently. The round-5 fixes held, apart from the config lock takeover (bug 4) and the unknown-field loss on save (bug 5).

## Fixes (B)

Items 7–15, all in `internal/server/`. `go vet ./internal/server/...` and `go test -count=1 ./internal/server/...` pass.

- **7 org delete:** `handleDeleteOrg` now also deletes the org's environments, their versions (sealed vars and secret files), git repos and git sources in the same transaction (`orgs.go`).
- **8 unreadable webhook secret:** the delivery is marked `failed` with an error in the delivery log, not left `pending` (`feat_webhooks.go`).
- **9 hanging endpoints:** at most `whPerOwner` (4) attempts per account are in flight, and the due query takes at most that many oldest rows per owner, so one account cannot fill the `whParallel` pool (`feat_webhooks.go`).
- **10 deliveries `limit`:** `0`, negative and non-numeric values give 400 `invalid_limit`; values above 500 are clamped to 500.
- **11 `namePrefix`:** the prefix length is counted in characters (`utf8.RuneCountInString`), matching SQLite `substr` (`sandboxes.go`).
- **12 history cursor:** an unknown cursor gives 400 `invalid_cursor` (`feat_history.go`). A cursor whose row retention removed now gives the same error.
- **13 events:** `limit` outside 1..200 gives 400 `invalid_limit` (it is no longer clamped), and an unknown `sort` gives 400 `invalid_sort`.
- **14 admin sandbox list:** `state=stopped` maps to `archived`. The hard `LIMIT 500` is replaced by `ParsePage` pagination (`limit`, `cursor`, `sort`) with `pageInfo`, as in the user-facing list. The default page is now 100, not 500 (`account.go`).
- **15 concurrent `MigrateAll`:** the "already applied" check now runs inside the immediate-lock transaction that applies the step, so a second process waits and then skips it (`store/store.go`).

## Fixes (A)

Items 1–6, 16–19 and note 20. Product code only, in `internal/cli`, `internal/node`, `internal/guest` and `internal/mux`. `internal/server` was not touched.

- **1. Hardlinks (`cli/transfer.go`):** `extractTar` handles `TypeLink`. The link name goes through the same `stripName` and `checkNoSymlinkParents` checks as other entries (new helper `stripName`). The source must be a regular file inside dest, else the entry is skipped. It is recreated with `os.Link`, and with a copy through `writeFile` if linking fails.
- **2. Long names:** `writeFile` creates its temp file as `.cvm-<random>` in the target's directory.
- **3. Sockets, FIFOs, devices:** `writeTarGz` skips them with a warning on stderr and goes on.
- **4. Config lock:** the lock file holds a random token. Unlock removes it only while it still holds this token. A stale takeover removes it only if the token read is still the one in the file (the window between that read and the remove is not closed).
- **5. Unknown config fields:** `updateConfig` applies the change `fn` made (typed before/after through `toMap` and `merge3`) onto the file's own JSON map, so unknown top-level and profile fields survive.
- **6. Node `once`:** the last waiter leaving marks the call `abandoned` and cancels it, but the entry stays in the map until `fn` returns. A new caller waits for it (or its own context) and then starts afresh.
- **16. `meta.json`:** `history.save` marshals, writes a unique temp file (`CreateTemp`) and renames, all under `h.mu`.
- **17. Detached kill:** SIGTERM to the group, wait up to 3 s for the group to be gone (`kill(-pgid, 0)` gives ESRCH), then SIGKILL to the group. Before, it waited only for the leader.
- **18. `a.procs`:** `history.prune` removes the `a.procs` entries of the pruned commands.
- **19. `mux.Join`:** on EOF of one direction, the other end gets `CloseWrite` if it supports it, and the other direction keeps copying. An end without `CloseWrite` closes both at once (old behaviour). Both ends are closed when both directions are done.
- **20. Output files:** stdout and stderr are opened `O_APPEND`. `capOutput` (one goroutine per command, also for adopted commands) checks them every 100 ms and once more at exit. A file above the cap (`Agent.outputCap`, 64 MiB per stream) is cut to the cap and gets `outputMarker` after it, and later output is dropped. There was no existing guest-side cap to reuse (the `outputTruncated` cap is server-side, per wallet and hour). Limits: up to one tick of output can exceed the cap before it is cut, and a chunk already exported past the cap stays on the server. New test: `TestCommandOutputCapped`.

Results: `go vet` is clean on cli, client, node, guest, mux and `cmd/...`. All `bughunt_r6` tests of those four packages pass. `go test -count=1 -race` passes on cli, node and mux. On guest, it passes except `TestDockerRunningContainersComeBack` and `TestTracerQuietRunningContainer` (a data race on a global between the test setup in `docker_test.go:39` and a `Boot` goroutine of an earlier test, `docker.go:50`). They are unrelated to these fixes, and the package passes under `-race` with those two skipped, and without `-race`.
