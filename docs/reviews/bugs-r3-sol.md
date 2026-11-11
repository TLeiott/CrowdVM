# CrowdVM bug hunt, round 3, sol

Three proven bugs. Added only `bughunt_r3_sol_test.go` in server, guest and node, plus this report. No product code changed. Unit tests only; no VMs, clusters or e2e tests.

## 1. Organization key accesses another organization

- Severity: security.
- Failing test: `TestBughuntR3SolOrgKeyCannotAccessOtherOrg`, both `GET` and `PATCH` subtests.
- Location: `internal/server/orgs.go:232`, `orgParam`; callers at lines 281 and 301.
- Reproduction: a non-admin user owns Alpha and Beta. Create an admin-preset API key restricted to Alpha. Use it to GET and PATCH `/api/v1/orgs/<Beta ID>`.
- Observed: GET returns HTTP 200 with Beta's organization and wallet data. PATCH returns HTTP 200 and renames Beta.
- Expected: refuse both requests because the key belongs to Alpha, even though its user owns Beta.
- Cause: `orgParam` resolves membership and roles using the key's user without comparing the requested organization against `c.P.Key.OrgID`. These handlers do not call `Scope`, which enforces that comparison elsewhere. `acctGuard` refuses selected organization subroutes, but permits the base GET and PATCH routes.

## 2. Move onto the same file deletes its contents

- Severity: data-loss.
- Failing test: `TestBughuntR3SolMoveOntoSelfPreservesData`.
- Location: `internal/guest/files.go:551`, `handleMove`.
- Reproduction: write `valuable.txt`, then POST `/v1/files/move` with source `valuable.txt`, destination `./valuable.txt` and `overwrite: true`.
- Observed: HTTP 404 from the subsequent rename; the source file no longer exists.
- Expected: preserve the file, either by treating the move as a no-op or refusing it before deletion.
- Cause: both paths resolve to the same file. Overwrite unconditionally calls `os.RemoveAll(dst)` before `os.Rename(src, dst)`, deleting the source itself. There is no source/destination identity check.

## 3. Duplicate create reads VM status without its mutex

- Severity: wrong-result.
- Failing test: `TestBughuntR3SolDuplicateCreateStatusRace`, requires `-race`.
- Location: `internal/node/vms.go:142`, `CreateVM`.
- Reproduction: repeatedly create an already-running VM while the monitor updates its status through `setState`. The unit test installs a record directly and uses no driver or VM.
- Observed: the race detector reports reads at line 142 concurrent with writes in `setState`, including `GuestReady`, `Direct` and `GPU`. The test exits 1.
- Expected: copy the existing status while holding `n.mu`, then return that copy.
- Cause: the duplicate-create branch unlocks at line 141 before copying `old.status` and reading its state for the error. Writers correctly use `n.mu`, but those reads do not. A real duplicate POST can race driver notifications or health updates. The test proves the race; it does not claim a reproduced crash.

## Reproduction commands

Each exited 1 for the bug above:

```sh
go test -count=1 -run TestBughuntR3SolOrgKeyCannotAccessOtherOrg ./internal/server/
go test -count=1 -run TestBughuntR3SolMoveOntoSelfPreservesData ./internal/guest/
go test -race -count=1 -run TestBughuntR3SolDuplicateCreateStatusRace ./internal/node/
```

## Checks that held

The following existing tests passed with `-count=1`:

- Server: both round-2 `TestBughuntSol` regressions; queued rebuild adoption; boot placement after node loss; node return without a machine and with a failed machine; node capacity refusal; secret-file home-path normalization; organization sharing and sandbox access checks.
- Additional server checks: `TestBootReplacesExistingMachine`, `TestForcedStopLeftoverVMRemoved`, `TestRemovedHolderFallsBack`, `TestAPIKeyManagementNeedsSessionOrAdminKey`.
- CLI: `TestDeleteBlockedIsNotDeleted`.
- Guest: `TestFiles` and `TestMergeInto`.
- Node, also with `-race`: `TestChainPull`, `TestDiskOvercommit`, `TestLazyChain`, `TestUploadInFlightAndGone`, `TestPeriodicReportedBeforeUpload`, `TestUploadRequestedAfterRestart`.

Inspection covered the requested lifecycle fixes, `requeueBoot` cleanup preservation, `bootParent` and abandoned-layer selection, `noHeadErr`, environment file validation/set/delete and CLI deletion following. No further proven bug in those fixes. Node inspection covered VM lifecycle, transfer deduplication and retry, API error mapping, disk admission, and QEMU store/lazy-chain bookkeeping. Guest inspection covered command execution and process recovery, file handlers and path resolution, clone/setup, and stop preparation. Auth inspection covered authentication/action checks, API key scope checks, organization guards and sandbox authorization. This records the paths checked, not a claim that all interleavings were tested.

**TL;DR:** An org key crosses org boundaries; move-to-self deletes data; duplicate VM creation races status updates.
