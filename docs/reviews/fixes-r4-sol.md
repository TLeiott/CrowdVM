# Round 4 fixes

- #4 fixed. `TestR4TelemetrySlowSandboxIsolation`: bounded workers sample accounts and sandboxes concurrently. Each sandbox gets a one-second sampling budget; each account gets ten seconds. Logs have a separate export loop.
- #6 fixed. `TestR4ServiceAccountSandboxDeletion`: refuse service-account deletion while any undeleted sandbox remains, including stopped sandboxes. Org owners/admins and server admins can delete existing orphans; ordinary members cannot. Key scopes still apply. Updated `TestServiceAccountsMergeLinksFeedback` to delete its sandbox before its service account.
- #8 fixed. `TestR4SecretFileHomeDirectoriesAndControls`: both environment update and single-file APIs return 400 for home directory names and Unicode control characters, including before normalization. Children such as `.ssh/config` remain allowed. Guest writes create parent directories and atomically rename the file.
- #9 fixed. `TestR4SandboxErrorNoticeClears`: recovery and deletion remove sandbox error notices. A state check also handles recovery racing the asynchronous notification.
- #10 fixed. `TestTracerQuietRunningContainer`: quiet containers stay running in history until Docker confirms they stopped. An unavailable daemon preserves the record.

Validation:

- All five regression tests failed against the original implementations in an isolated temporary copy. For #10, the original reaper body was extracted unchanged so the test could invoke it without waiting ten minutes.
- `go vet ./...`: passed.
- `go test -count=1 ./internal/... ./cmd/...`: passed.
- `go test -count=1 ./internal/guest`: passed again after covering journald's short container IDs.
- CLI help unchanged; `make docs` unnecessary. No clusters, VMs, or e2e tests started.
- Restricted files untouched.

**TL;DR:** All five requested bugs fixed; regression and required checks pass.
