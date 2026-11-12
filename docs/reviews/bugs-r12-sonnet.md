# Bug hunt r12 (sonnet)

2026-11-11. Product code unchanged; I only added `internal/server/bughunt_r12_test.go` and this report. Unit tests only (`servertest`, the fake node). No cluster, VM or e2e test. I did the work myself, no sub-agents. Budget was tight, so the node package was audited by reading plus one throw-away fuzz run (not kept), not by new tests.

## What I audited

- **New commits** (`f2ef8d4..11c312e`): `schedule.go` (parser, `Active`, `NextChange`, hold, `EffectivePause`), `ui.go` (`localOnly`, the cookie/Bearer split, login codes, `SetConfig`, `UIHandler` routes), `ui.html` (what it escapes), `config.go` (`Set` validation of the live keys), `node.go`/`vms.go`/`warm.go` (pause state in the heartbeat and `CreateVM`), `awake.go`, `lazy.go` (http driver probe), and on the server `nodes_evacuate.go` (`handleEvacuateNode`, `handleCancelEvacuation`, `evacuateStep`, `onlyCopiesOn`, `evacUploadDue`, `evacuationResume`, `evacuationStranded`), the `requestStop`/`afterStopResume`/`tryPlace` hooks, `handleUpdateNode`/`handleDeleteNode`, and `nodeJSON`.
- **Schedule fuzz**: 20 000 random schedules (days, ranges, lists, `daily`, wrap past midnight, `24:00`, overlapping entries) times 20 random instants each, `Active` against an independent reference. No mismatch. `NextChange`, `holdFor` and `EffectivePause` read for DST and hold-boundary cases: no bug found.
- **Round 11 fixes** (`f3fa162`): `router.go` (authenticate before the limiter, principal reuse in `wrap`), `blobPutRefusal`, `uniqueNodeName` and its three callers, the node event queue (`emitEvent`, `dropFront`, the flush loop), and the concurrent snapshot loop (`snapBusy`, `snapSlots`, `noQuiesceUntil`, `wg`).
- **"Upload throttling"**: the only throttle in these commits is `evacUploadDue`/`evacUploadDone` (one request round per node, 30 s apart). There is no token bucket anywhere in the tree (`grep` for `rate.NewLimiter`, `bucket`, `throttle` finds only that function). It is correct as far as I can tell (see What held).

## How to run

```
go test -count=1 -run TestBughuntR12 ./internal/server/
```

All four fail today (3 runs each; the name-race test loops over 10 names inside the test and failed in every run). `go vet ./internal/server/` is clean. The rest of the package passes (`go test -skip 'TestBughuntR1[0-2]' ./internal/server/`, 130 s).

## Bugs proven by a failing test

All in `internal/server/bughunt_r12_test.go`.

### 1. A huge `graceSec` wraps the evacuation cap (wrong-result, low)

- Test: `TestBughuntR12EvacuateGraceOverflowSkipsCap` (`:24`)
- Code: `nodes_evacuate.go` `handleEvacuateNode`: `time.Duration(grace)*time.Second > MaxEvacuationGrace`, then `deadline := now + int64(grace)*1000`.
- Observed: `graceSec = 2^55` gives 200. `2^55 * 1e9 = 2^64 * 5^9`, which is 0 mod 2^64, so the cap check sees a zero duration. The deadline arithmetic wraps too (here to a time before now), so the evacuation is overdue at once and the "cancel until the deadline" rule (409 `evacuation_started`) already applies.
- Expected: 400 `invalid_grace`.
- Cause: the bounds check multiplies after the conversion instead of comparing `grace` with `int(MaxEvacuationGrace.Seconds())`. The owner could already pick grace 0, so the harm is a broken validation and nonsense deadlines (also a far-future one for other values), not a new privilege.

### 2. Nodes that register at the same time keep the same name (security, low-medium)

- Test: `TestBughuntR12ParallelRegistrationsShareNodeName` (`:38`)
- Code: `nodes.go:431` (`uniqueNodeName`: `SELECT COUNT(*)`), used at `:407` (register), `:583` (heartbeat), `:897` (connect); the INSERT/UPDATE comes afterwards.
- Observed: 12 parallel registrations (an unlimited join token) with one name leave 2 or 3 nodes with exactly that name. The r11 fix only holds for sequential registrations.
- Expected: names stay unique; pins by name (`placement.node`, `nodeByRef`) match one node.
- Cause: check and write are separate statements and there is no unique index. A unique index on `lower(name)` (with a retry loop on conflict) or doing the check inside the same transaction as the write fixes it. Heartbeat and connect renames have the same window.

### 3. A valid credential on a public route replaces the IP bucket (security, low-medium)

- Test: `TestBughuntR12CredentialOnPublicRouteEscapesIPLimit` (`:82`)
- Code: `router.go:~140-153` (`ServeHTTP`: any request with an `Authorization` header or a session cookie that authenticates is limited under `user:<id>`), the r11 opus 9 change.
- Observed: `IPRateLimit = 6`. Three anonymous agent identities (one request each, `POST /agent/identity`) are used as bearer tokens on `POST /auth/login` with a wrong password: 18 of 18 guesses from one IP are answered (not 429).
- Expected: public endpoints that ignore the credential (`/auth/login`, `/auth/email/*`, signup, `/agent/*`, `/oauth2/*`) keep counting against the IP; or the IP bucket is charged as well as the user bucket.
- Cause: the bucket is chosen by "does a credential authenticate", not by "does this route need one". Anonymous agent identities are free and unlimited per IP (`checkSignup` is not applied, see r11 Code only), so one request buys a new bucket. The password and email-code guessing endpoints therefore have no effective per-IP bound. Fix: charge the IP bucket always and add the user bucket on top for authenticated routes, or only use the user bucket on routes registered with an auth action.

### 4. The node's own name overwrites an admin's rename on every info heartbeat (wrong-result, low)

- Test: `TestBughuntR12AdminRenameRevertedByNextInfoHeartbeat` (`:111`)
- Code: `nodes.go:~583` (`handleHeartbeat`: `name=CASE WHEN ?<>'' THEN ? ELSE name END`) and `:~897` (`afterNodeConnect`, same).
- Observed: `PATCH /nodes/{id} {"name":"chosen-by-admin"}` returns 200; the next heartbeat that carries `Info` with the node's unchanged configured name puts `laptop-orig` back.
- Expected: the rename stays until the node reports a different name than before.
- Cause: the node's name is written unconditionally. Info is sent on every reconnect and, since the schedule feature, whenever the schedule crosses a boundary or the capacity changes (`ps != n.lastPause`), so a rename lasts at most a few hours. The server should compare the reported name with the previously stored `info.name` and only then change `nodes.name`.

## Code only

- **`keepAwakeLoop` uses `Pdeathsig` on the thread, not the process** (`awake_linux.go:10`, `awake.go:52`): Linux delivers `PR_SET_PDEATHSIG` when the OS thread that forked the child exits, and the Go runtime can retire that thread. The inhibitor then dies while the node runs, the loop logs "the sleep inhibitor ended", waits 10 minutes (`retryAt`) and the machine can suspend under running sandboxes. Needs `runtime.LockOSThread` around `Start`, or no `Pdeathsig` and an explicit kill on exit. Not testable without systemd and a thread exit.
- **`/api/images/{id}/import|push|pull`** (`ui.go`): `{id}` is `PathValue`, so `%2F` yields slashes and `ImportImage(id, path)` takes a caller path. Needs the Bearer token (the owner's CLI), so only a local-privilege concern; the id should be validated like image ids elsewhere. I did not read `ImportImage`'s path handling.
- **A browser cookie is the whole page credential** (`ui.go`): `sessionValue` is stable, `HttpOnly`, but browsers send localhost cookies to every port, and `SameSite` ignores ports, so another local web app on `localhost:<other>` that receives it can replay it (with the `X-Crowdvm-Ui` header it can set from a server side request) for `pause`, live limits, `schedule`, `restart` and `bench`. The design note accepts this; the page also sends no `X-Frame-Options`/`frame-ancestors`, so a page on another localhost port can frame it with the cookie attached (same site). A `frame-ancestors 'none'` costs nothing.
- **A schedule that fails to parse in a hand-edited `config.json` means "always offer"** (`schedule.go` `EffectivePause`: `err != nil || len(s) == 0` returns an empty `PauseState`). `Set` validates, `LoadConfig` does not. An owner who typos the file is online around the clock. Failing closed (paused, reason `schedule`) would match the intent.
- **`summary.free` counts schedule-paused nodes** (`nodes.go` `handleListNodes`): a node with `Info.Paused` contributes its free capacity to the pool's free total though `pickNode` skips it. Cosmetic.
- **Evacuation never ends for an offline node with sandboxes** (`evacuateStep`: `offline > 0`): by design (the sandboxes come back when the node does), but a node that never returns keeps the row, and the node stays drained, until someone removes it. Documented behavior, listed for completeness.

## What held

- **Schedule maths**: `Active` matches a reference for 400 000 random (schedule, instant) pairs, including midnight wrap, `24:00`, overlapping and whole-day entries. `NextChange` is minute-exact and DST-neutral (wall-clock `Hour/Minute` per probe, absolute `Add`). Equal start and end is rejected; an empty entry list means "always".
- **Holds**: `holdFor` ends at the next boundary; a stale hold is cleared when the schedule changes (`Set`), and `EffectivePause` extends the hold to the following change when both flip to the same state.
- **Live limits**: lowering `cpus`/`memory`/`disk`/`max-sandboxes` below usage only blocks new creates (`CreateVM` checks under `n.mu` together with the reservation, so two parallel creates cannot both pass); `fits` and `nodeJSON` clamp negative free values. Server and node disagreeing on time cannot matter: only the boolean and a reason cross the wire.
- **Local UI API**: non-GET calls need the custom header (never answered in a preflight); the Host check refuses non-IP, non-`localhost` names (DNS rebinding); login codes are 128-bit, single-use, expire in a minute and are removed on first use even when refused; the cookie never holds the token; the page's keys are an allowlist (`browserOK`, `liveKeys`); `ui.html` escapes every server- and node-supplied string it puts into `innerHTML`.
- **Evacuation state machine**: the marker is set only by a new stop of the evacuation's own and cleared by every other stop under `sbLocks`; a stale marker is dropped in `afterStopResume` when the sandbox is no longer on that node; deleted sandboxes are skipped; `handleUpdateNode` refuses un-draining during one; a restart keeps the row (and the next reaper pass continues); cancel is atomic with the deadline (`deadline>?`).
- **Upload throttling**: `evacUploadDue` marks a round as running (zero time) under `earnMu`, so rounds never overlap; `askUpload` is bounded to 15 s per holder, so a round cannot hang the node's slot.
- **r11 fixes**: `authenticate` has no side effects (no writes, no cookies), so running it before the limiter and reusing the principal in `wrap` is safe; `blobPutRefusal` image/snapshot rules and the never-replace rule hold for sequential PUTs; the event queue's guest accounting stays consistent (`dropFront` decrements exactly what it drops; both flush branches drop a front prefix); the snapshot loop starts no goroutine without a `wg.Add`, honours `ctx` while waiting for a slot, and `snapBusy` is cleared on every exit path.

## Not verified

- The node-side event queue, snapshot loop and UI handler were audited by reading; no failing tests were written for the node package (nothing wrong enough to prove). The `Pdeathsig` finding is from knowledge of the kernel/Go behavior, not from a run.
- Server restart in the middle of an evacuation was reasoned from the persisted rows (`node_evacuations`, `extra.evacuateFrom`, stop rows), not killed and restarted. A crash between a stop completing and `afterStopResume` running leaves a sandbox stopped with a stale marker (harmless, the next non-evacuation stop clears it), but I did not drive it.
- `handleBlobPut`'s concurrent-PUT case (two nodes writing the same new id at once) was not driven.
- No real node, QEMU, browser or DST time zone was used.

## Fixes

All 4 fixed; the tests in `internal/server/bughunt_r12_test.go` pass.

1. **Credential on a public route** (`router.go`): only `/api/` requests may move to a per-user bucket; `/auth/`, `/agent/` and `/oauth2/` always count per IP, so anonymous agent keys cannot multiply buckets.
2. **Parallel node names** (`nodes.go` `nodeNameMu`): the uniqueness check and the write it guards run under one lock at registration, heartbeat, connect and admin rename.
3. **Admin rename reverted** (`nodes.go` `reportedNodeName`): a node's reported name renames it only when that name changed since its last report, so an administrator's rename stays.
4. **Evacuation grace overflow** (`nodes_evacuate.go`): `graceSec` is compared in seconds against the 24 h cap.
