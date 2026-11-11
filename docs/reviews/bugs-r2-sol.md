# CrowdVM bug hunt, round 2, sol

Two proven bugs. Only the test file and this report were added. No product code changed. Tests use the fake node; no clusters, VMs or e2e tests ran.

## 1. Old upload replaces the head after resume on the same node

- Severity: wrong-result.
- Failing test: `TestBughuntSolLateUploadAfterResumeOnSameNode`.
- Location: `internal/server/snapshots.go:299`, `afterSnapshotStored`.
- Reproduction: store a periodic root, hold a newer periodic upload, disconnect the node, force-stop the sandbox, drop its old machine, reconnect, resume on that node from the stored root, then release the old upload. The test waits for `snapshot.stored` before checking the head.
- Observed: the running machine booted from the root, but the head advances to the abandoned old machine's layer.
- Expected: store the old layer without changing the running machine's head. Its next snapshots build from the root, not from the abandoned layer.
- Cause: head selection guards against a different node ID, but the same node can host a replacement machine booted from an older fallback. The old layer passes both the node check and the direct-parent check. Neither check identifies the machine or branch that produced it.
- Impact: the recorded head describes a disk the running machine never booted. Recovery from that head can restore the abandoned branch.

## 2. Missing head reports retryable upload after its only holder was removed

- Severity: wrong-result.
- Failing test: `TestBughuntSolNoHeadRemovedHolderIsFinal`.
- Location: `internal/server/lifecycle.go:951`, `noHeadErr`.
- Reproduction: run a sandbox without a stored snapshot, hold its first periodic upload, disconnect and force-stop it, then permanently remove its node. Call both resume and fork.
- Observed: both return HTTP 409 `snapshot_unavailable`, `retryable: true`, and advise retrying in a minute.
- Expected: a permanent snapshot-loss error with `retryable: false`, as for a lost layer in a sandbox with a head.
- Cause: `noHeadErr` counts unuploaded rows without checking whether any holder still exists. Node removal preserves those rows. With no head, both handlers return before `chainReachable` can apply its removed-holder check.
- Impact: clients keep retrying an operation that cannot recover through retry.

## Verification

Each command exited 1 with the assertion described above:

```sh
go test -count=1 -run TestBughuntSolLateUploadAfterResumeOnSameNode ./internal/server/
go test -count=1 -run TestBughuntSolNoHeadRemovedHolderIsFinal ./internal/server/
```

The combined `go test -count=1 -run TestBughuntSol ./internal/server/` also failed on both tests. A speculative equal-timestamp restart test passed and was removed.

## Checks that held

These existing tests passed together with `-count=1`:

- `TestForcedStopAfterNodeLossUsesLastStoredSnapshot`: fallback and late uploads after resume on a different node.
- `TestRemovedHolderFallsBack`: removal restores a stored head when one exists.
- `TestBootPlacedAgainWhenNodeDies`: boot transport loss requeues successfully.
- `TestNodeBackWithoutMachineBootsAgain`: reconnect without the machine restarts it.
- `TestNodeRestartMidUploadThenResumeElsewhere`: stranded uploads complete the chain and saved snapshot.
- `TestStopWaitsForOfflineNode`: a normal stop waits for a disconnected node.
- `TestForcedStopLeftoverVMRemoved`: forced-stop leftovers are removed on return.
- `TestPeriodicLayerRecordedWhenTaken`: pending layers are recorded and retained before upload.

Code inspection also covered `runStop` and its retry/404 adoption paths, `transitionIf` and offline transitions, resume/fork callers, reconnect and node deletion, stranded-layer reachability/fallback, snapshot recording/head selection, and `LocalSnapshots` plus its API caller. `LocalSnapshots` copies upload bookkeeping under the mutex and includes stored metadata for non-uploading layers; no additional bug was proven there.

**TL;DR:** Two failing regressions: stale same-node head promotion and permanent loss mislabeled retryable.
