# Round 3 implementation fixes

- #3 fixed. `TestR3EnvAccess` checks username removal and an empty `--only` restriction. Resolve usernames and emails through org membership before comparing stored IDs.
- #4 fixed. `TestR3UndrainLocalPause` checks the node-side unpause instruction, including JSON output. The node guide distinguishes local pause from server drain.
- #5 fixed. `TestR3ExecSignals` checks SIGKILL and SIGTERM in synchronous, streamed and attached commands. Guest command results and exit frames report Linux signal names with null exit codes; the CLI returns 128 plus the signal number.
- #6 fixed. `TestR3SecretFileConflicts` checks both ancestor orders and newline paths, with HTTP 400 `invalid_secret_files`.
- #7 fixed. `TestR3InfoFields` checks that CLI JSON retains `directEndpoints`, `storage` and `imageId`. Client types also retain `nodeId` and `placement`.
- #8 fixed. `TestR3StatusReadableMaps` checks sorted benchmark text and readable empty endpoints. Direct endpoints use the same formatter.
- #9 fixed. `TestR3ConfigRejectsUnusableValues` checks CPU capacity, sub-unit memory, invalid durations, addresses, URLs and non-finite sizes.
- #10 fixed. `TestDeleteBlockedIsNotDeleted` now checks the final error's `deletion_blocked` code and 409 status.
- #15 skipped. Correct privacy attribution needs a durable privacy boundary at share/unshare time. Both handlers are in reserved `internal/server/lifecycle.go`; changing only `feat_history.go` cannot reliably identify commands pulled after sharing.
- #16 fixed. `TestR3OrgSafeguards` checks refusal to remove a member with an active org sandbox and successful removal after its run closes. The transaction comment documents why removal requires stopping first. The existing org test now stops Bob's sandbox before removal.
- #17 fixed. `TestR3HugeKeyTTL` checks overflowing day and second values. Validate the multiplier bound before multiplication.
- #19 fixed. `TestR3OrgSafeguards` checks negative concurrent and credit caps. Validation precedes all member updates.
- #20 fixed. `TestR3RestrictedKeyUsage` checks that sandbox-limited keys cannot read another key's usage.
- #21 fixed. `TestR3OrgSafeguards` checks HTTP 409 `org_has_balance` and preservation of the balance. Deletion requires transferring remaining credits first.
- #22 fixed, with the webhook subitem skipped. `TestR3KeyPresetAndCredits` checks API-key presets and readable credits; `TestR3GrantReasonAfterPositionals` checks trailing `--reason`. `.ref/pages` has no Boat `webhook test` command. No CLI help text changed.
- #23 fixed. Python `test_repeated_labels` and TypeScript `r3 repeated labels` check repeated label parameters. Other array parameters retain their comma encoding.
- #24 fixed. Python `test_stream_eof_requires_exit` checks truncated command streams raise `APIConnectionError`.
- #25 fixed. Python `test_async_cancel_blocked_stream` checks cancellation during a blocking read and closure of the HTTP socket. Interrupt the socket before waiting for the reader thread and closing its generator.
- #26 fixed. TypeScript `r3 stream response headers time out` checks the header deadline; the deadline clears once streaming starts.
- #27 fixed. Python `test_command_null_timeout` checks an explicit null timeout reaches the API without a conversion error.
- #28 fixed. Python `test_early_write_error_keeps_server_response` checks that a connection reset during a body write still yields the server's HTTP 400 API error; `test_async_with_options` checks the async client type. The transport reads an early response after a broken pipe or connection reset.

Validation:

- `go vet ./...` passed.
- All requested regression tests passed.
- `go test -count=1 ./internal/... ./cmd/...` failed only at `TestSnapshotsOffNeverSaved`, which times out at `internal/server/lifecycle_fix_test.go:159` waiting for the node to drop a snapshot. The failure also occurred in the initial package run. The running-layer retention query in reserved `internal/server/stranded.go` now retains the unexpected layer even for a sandbox with snapshots disabled. Fixing that behavior requires the other engineer's reserved snapshot code; it was left untouched.
- Python suite passed, 54 tests.
- TypeScript suite passed, 39 tests, including compilation and type checks.
- `make docs` was not required because CLI help text did not change.

The service-account test now requests a 1h TTL, within its creating test key's 1d lifetime, to match the concurrent service-key scope fix. No clusters, VMs or e2e tests were started. Reserved files were not edited.
