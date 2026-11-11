# Bug hunt r4 (sol)

2026-11-11. Audited the uncommitted master tree. Product code unchanged. Reproductions use only unit-test fixtures, SQLite, httptest and fake nodes/guests. No cluster or real VM was started.

## 1. Symlink bypass of move containment deletes the source

- Severity: data-loss.
- Test: `TestBughuntR4SolMoveViaSymlinkPreservesSource` in `internal/guest/bughunt_r4_sol_test.go`.
- Where: `internal/guest/files.go:553`, destructive removal at `:562`; `internal/guest/guest.go:389` returns the original path after validating its resolved parent.
- Reproduction: create `a/b/keep.txt`, make `alias` a symlink to `a`, then move `alias/b` onto `a` with overwrite enabled.
- Observed: HTTP 404 after `RemoveAll(a)` deletes the source directory and its irreplaceable contents. The subsequent rename cannot find its source.
- Expected: reject the containing-directory move before deleting anything; the original file remains intact.
- Cause: the round-3 containment check compares lexical paths. Parent symlinks pass root validation but remain unresolved in the paths used by `within`. `alias/b` therefore appears unrelated to `a`.

## 2. Another credit-transfer path still overdraws under concurrency

- Severity: wrong-result.
- Test: `TestBughuntR4SolConcurrentMoveCreditsCannotOverdraw` in `internal/server/bughunt_r4_sol_test.go`.
- Where: `internal/server/acct_misc.go:233`, debit at `:259`.
- Reproduction: give a personal wallet 10 credits; send 32 simultaneous authenticated `POST /orgs/r4alpha/move-personal` requests with `transferCredits: 10`.
- Observed: balance fell to -50 in the isolated race run and -30 in the combined race run. Multiple requests transfer the same available funds.
- Expected: one transfer succeeds, later transfers refuse insufficient funds, and the wallet remains nonnegative.
- Cause: `handleMovePersonal` reads the balance before its transaction and later uses unconditional `walletAdd` to debit. The atomic conditional debit added to `handleCreditTransfer` does not cover this endpoint. The `"all"` variant also reads its amount before the transaction.

## 3. An organization key can change the user's active wallet to another org

- Severity: security.
- Test: `TestBughuntR4SolOrgKeyCannotSwitchOtherWallet` in `internal/server/bughunt_r4_sol_test.go`.
- Where: `internal/server/orgs.go:214`, account mutation at `:224`.
- Reproduction: create Alpha and Beta with one owner, mint an Alpha-bound admin key, then use it for `PATCH /orgs/active {"org":"r4beta"}`.
- Observed: HTTP 200 and the user's persisted active organization changes from Alpha to Beta.
- Expected: HTTP 403; an Alpha key cannot select Beta or alter the account's default wallet across that boundary.
- Cause: changing the route to `account.admin` fixed the read-only-key case but did not enforce `Key.OrgID`. The handler calls `resolveOrgRef` directly, bypasses `Scope`/`paramOrg`, and is absent from the account-route org-key deny list. Other sessions and unbound keys subsequently inherit the changed default wallet.

## 4. Resharing hides commands that ran while shared

- Severity: wrong-result.
- Test: `TestBughuntR4SolResharePreservesPreviouslySharedHistory` in `internal/server/bughunt_r4_sol_test.go`.
- Where: `internal/server/lifecycle.go:1782`; privacy assignment at `internal/server/feat_history.go:208` and sticky update at `:207`.
- Reproduction: a command runs in a shared organization sandbox; unshare, share again, then import that command's delayed history export.
- Observed: the command is stored with `private=1`. Teammate history handlers filter it out.
- Expected: commands from the shared interval stay readable by teammates; only commands from private intervals are hidden.
- Cause: `privateUntil` represents every instant before the latest share, including earlier shared intervals. History imports classify every earlier command as private, and `MAX(existing.private, excluded.private)` makes that classification permanent. One timestamp cannot represent multiple private/shared intervals.

## 5. Organization key can delete another organization's CI link

- Severity: security.
- Test: `TestBughuntR4SolOrgKeyCannotUnlinkOtherOrgCI` in `internal/server/bughunt_r4_sol_test.go`.
- Where: `internal/server/feat_ci.go:1073`, deletion at `:1077`.
- Reproduction: create a Beta-owned pending CI link for the same user; send `DELETE /ci/links/cil_r4_beta` with an Alpha-bound admin key.
- Observed: HTTP 200 and the Beta CI link is deleted.
- Expected: reject the cross-organization mutation; retain the link.
- Cause: `unlink` authorizes solely by `owner_id`, without checking the link's `org_id` against the key's organization. `/ci` is absent from `orgKeyForbidden`. The active-link branch uses the same authorization and additionally clears credentials and cancels jobs, but the retained test proves pending-link deletion only.

## Verification

All five new tests were run individually with `go test -count=1 -run <Name> ./internal/<pkg>/`. Concurrent server tests used `-race`. The combined server run also used `-race`; its four failures were the expected product assertions, with no race-detector diagnostics. The guest test fails on the missing source file.

Only failing repro tests remain in the two `bughunt_r4_sol_test.go` files. Removed passing probes for org-key transfer and child-key scope escape, both refused by the account-route guard. Removed a concurrent `BillNow` probe because production invokes it from a single billing loop, so concurrent direct calls did not establish a production trigger.

## Checks that held

- Running snapshot ancestry and queued-machine adoption: read `snapshotsNeeded` and the adoption reload; existing `TestDeleteSnapshotsOfRunningSandboxKeepsItsChain`, `TestQueuedRebuildAdoptsReturningMachine`, and `TestQueuedRebuildConcurrentReports` pass under `-race`.
- Snapshot upload retention: `TestNodeRestartMidUploadThenResumeElsewhere`, `TestDeleteRunningWaitsForPeriodicUpload`, and `TestDeleteWaitsForStopUpload` pass under `-race`.
- Round-3 org path isolation, service-account action/expiry limits and invalid-input cleanup, read-only active-wallet denial, and ordinary credit-transfer concurrency: existing focused regressions pass under `-race`. Org-bound transfer and API-key creation requests receive `api_key_org_forbidden`.
- Guest move onto itself and lexical ancestor/descendant moves preserve data in the existing tests. Node duplicate-create status copying passes `TestBughuntR3SolDuplicateCreateStatusRace` under `-race`.
- First private-to-shared delayed history import passes `TestHistoryPrivateBeforeShare` under `-race`; the new failure requires an earlier shared interval.
- Public URL routing, token cookies, forwarding and isolation: `TestHostPublicURLs` passes under `-race`. Read the proxy's firewall, token, request-cookie stripping and path CSP code.
- AI proxy: `TestAIProxy` passes under `-race`, including placeholder replacement, usage and lifecycle token behavior. Read vendor/custom routing and token lookup.
- Webhooks: `TestWebhooks` passes under `-race`, including signatures, rotation and retry-to-success. Read the delivery lease and outbound transport. `TestOutboundGuard` passes for denied loopback/metadata and allowed LAN targets; LAN access is intentional in this repository.
- Telemetry: `TestOpenTelemetry` passes under `-race`; read endpoint normalization, header validation, guarded export, retry handling and queues.
- Session cookies/CSRF: read central cookie-authentication mutation checks; `TestSignupLoginSessions` passes under `-race`.
- Read SSH-key configuration and tunnel authorization, rate-limit counters and start/debit paths, name/label/subdomain validation, stored-history and event cursors, and CI connection/link/job access code. No additional confirmed finding from those reads. Integration adapters and the dashboard frontend were not exhaustively audited.

**TL;DR:** Five failing reproductions: one source-file deletion, one wallet overdraw, two organization-key boundary violations, and shared history incorrectly hidden.
