# Bug hunt r8 (sonnet)

2026-11-11. Audited the uncommitted master tree. Product code unchanged. Only unit tests: SQLite, httptest, the fake node, a re-exec'd CLI helper process, stub `uname`/`curl` for the install script. No cluster, VM or e2e test.

I took the round-7 fix audit myself. Two sub-agents took the rest:
- One took docs vs CLI and `scripts/`.
- One took `web/js` (routes, XSS, CSRF).

I re-ran every test below myself; each one fails. Run them with `go test -count=1 -run <Name> ./internal/<pkg>/`.

Test files:
- `internal/server/bughunt_r8_test.go`
- `internal/server/bughunt_r8_web_test.go`
- `internal/server/bughunt_r8_scripts_test.go`
- `internal/cli/bughunt_r8_test.go`
- `internal/cli/bughunt_r8_docs_test.go`

Passing regression tests that stay:
- `TestR8WebAPIPathsMatchRoutes`: 154 method+path pairs extracted from `web/js`, every one hits a real route.
- `TestR8DocsExtractor`: self-test of the docs checker.

## Bugs

### Security

**1. Guest file downloads are served as HTML on the dashboard's origin (stored XSS to account takeover).**
- Test: `TestR8WebGuestFileServedAsHTMLOnServerOrigin`.
- File: `internal/server/passthrough.go:245-283` (`ProxyToGuest`, `ModifyResponse` only drops `X-Request-Id`), reached from `handleSignedFile` (`passthrough.go:340-366`) and every guest pass-through GET.
- Observed: `GET /api/v1/sandboxes/{id}/files/signed?...` returns the guest's `Content-Type: text/html`, with no CSP `sandbox`, no `nosniff` and no attachment disposition. The signed URL needs no auth, and the dashboard hands it out (`web/js/tabs/files.js:99`).
- Expected: same `Content-Security-Policy: sandbox` that path-routed public URLs get (`feat_host.go:740`), or force `Content-Disposition: attachment` plus `nosniff`.
- Cause: the sandbox user controls the guest agent (passwordless sudo), so they pick the headers. An attacker sends a victim a signed link to their own sandbox file. The page runs on the dashboard origin with the victim's session cookie. The CSRF check is only the constant header `X-CrowdVM-CSRF: 1`, so the script can mint API keys.

**2. The dashboard can be framed (clickjacking of the device-approval page).**
- Test: `TestR8WebDashboardFramable` (`/device?code=…`, `/sandboxes`, `/`).
- File: `internal/server/web.go:23-51`.
- Observed: the SPA handler sets no `X-Frame-Options` and no CSP `frame-ancestors`. `/device?code=ATTACKER` pre-fills the code and focuses "Approve sign-in" (`web/js/pages/device.js:9-36`). The server's own fallback device page sets `X-Frame-Options: DENY` (`device.go:470`).
- Cause: the session cookie is SameSite=Lax. A sandbox's public URL on a domain that shares a registrable domain with the dashboard is same-site, so the cookie is sent. One click hands the attacker's CLI an API key acting as the victim.

**3. A foreign node can poison the VM start-time guard and hide a real crash (stuck).**
- Test: `TestBughuntR8ForeignVMStartTimePoisonsStopReports`.
- File: `internal/server/lifecycle.go:602-613`.
- Observed: `reconcileVM` stores `vmStarts[sandbox]` before it checks that the reporting node hosts the sandbox (the check is at `:624`). Node B reports `running` with a StartedAt 24 h ahead for a sandbox on node A. Later the host reports `error` ("qemu died"); the report is dropped as "about an older machine". The sandbox stays `ready` for ever. The control run without the foreign event passes.
- Cause: the round-7 node check went into `onGuestEvent` only. The same trust gap is open in `vm.state`. It needs no malicious node: an orphan machine on a node whose clock runs ahead does the same.
- Expected: record `vmStarts` only after `sb.NodeID == nodeID`.

**4. A foreign node can mark another node's sandbox "snapshot failed".**
- Test: `TestBughuntR8ForeignSnapshotFailedMarksSandbox`.
- File: `internal/server/nodes.go:617-628` (`EvSnapshotFailed`).
- Observed: `UPDATE sandboxes SET last_snapshot_status='failed' WHERE id=ev.SandboxID` with no node check.
- Cause: same family as bugs 3 and r7 4. Also unchecked, code only: `EvTransfer` writes `hydrationProgress` for any sandbox id (`nodes.go:629-633`). `EvSnapshotUploaded` honours `ref.Supersedes` for any `sbID` (`snapshots.go:258-264`). Both need ids the foreign node would have to know.

### Wrong result

**5. `nohup crowdvm ...` no longer survives the terminal.**
- Test: `TestBughuntR8NohupIsIgnored` (re-execs the test binary under `trap '' HUP` and runs the real `Main`).
- File: `internal/cli/app.go:78-80`.
- Observed: with SIGHUP ignored at start, a SIGHUP cancels the context and the command stops (exit 0).
- Cause: `signal.NotifyContext(..., SIGHUP)` installs a handler even for a signal inherited as ignored (documented Go behaviour). The round-7 fix for bug opus 3 therefore breaks `nohup crowdvm exec ID -- long-job &`.
- Fix: register SIGHUP only if `!signal.Ignored(syscall.SIGHUP)`.
- Not broken: `crowdvm ssh` (the terminal sends SIGHUP to ssh too, and `a.Run` ignores ctx).

**6. The 200-key cap counts the per-sandbox tokens the server mints.**
- Test: `TestBughuntR8KeyCapCountsSandboxTokens` (200 create+stop cycles, about 13 s).
- File: `internal/server/apikeys.go:426`, with the token rows from `lifecycle_secure.go:413`.
- Observed: a user with 200 archived sandboxes and no keys of their own gets 409 `api_key_limit` on the first key they create.
- Cause: `COUNT(*) FROM api_keys WHERE user_id=? AND revoked_at IS NULL` includes `preset='sandbox-token'` rows. They are revoked only when the sandbox is deleted (`lifecycle.go:1884`). Expired keys are counted too.
- Expected: count only user-created keys (`sandbox_id IS NULL`, not expired).

**7. Org delete leaves its CI link active, with the sealed GitHub token.**
- Test: `TestBughuntR8OrgDeleteLeavesActiveCILink` (the row is inserted the way `feat_ci.go:1060` does).
- File: `internal/server/orgs.go:424-437`.
- Observed: `ci_links.status` stays `active` and `token` stays set. The poller keeps listing jobs, and each one fails at "create sandbox" in a deleted org (`feat_ci.go:639-660`). The login also stays linked (`ci_account_linked`).
- Expected: do what unlink does (`feat_ci.go:1099`): `status='unlinked', token=''`.

**8. The `install.sh` arm64 node install gets no in-sandbox CLI.**
- Test: `TestR8InstallShArm64NodeGetsInSandboxCLI`.
- File: `scripts/install.sh:29-32`.
- Observed: `--node` on Linux arm64 fetches `crowdvm`, `crowdvm-node` and `crowdvm-guest`, not `crowdvm-linux-amd64`.
- Cause: on non-amd64 hosts the node looks for the in-sandbox CLI only under that name (`internal/node/config.go:215-217`). `install.ps1:18` does fetch it.

**9. `install.ps1` writes a profile line that is invalid PowerShell for paths with an apostrophe.**
- Test: `TestR8InstallPs1ProfileLineQuoting`.
- File: `scripts/install.ps1:38`.
- Observed: `'$Cli'` in single quotes with no doubling of `'`. `C:\Users\O'Brien\...` breaks the profile on every start.

**10. `crowdvm self-update` can install a binary that cannot run.**
- Code only.
- File: `internal/cli/cmd_config.go:262-266`.
- Observed: if the downloaded binary cannot run `--version` (wrong slot, noexec, corrupt), the offered version is `""`, the versions do not compare, the hash fallback sees a difference, and it replaces the working CLI.

### Stuck / leak (code only)

- `scripts/install.ps1:10-12`: `Fetch` writes straight to the final `.exe`. An upgrade while the node service runs fails with "file in use" after `crowdvm.exe` is already replaced (mixed install). `install.sh:26` uses a temp file and `mv`.
- `scripts/install.ps1:24-26`: `GetEnvironmentVariable('Path','User')` returns `%VAR%` entries already expanded, and `SetEnvironmentVariable` writes them back as REG_SZ on Windows PowerShell 5.1. Known .NET behaviour; not run here.
- `scripts/dev-cluster.sh:21-23, 36-37`: a second `up` starts another cluster and overwrites the pid files, so `down` cannot kill the first.
- `internal/guest/commands.go:158`: `p.m.OutputTruncated` is read without the history lock while `hist.update` (and the sync handler at `:356`) write it. A data race, low impact; the write path is correct.

### Docs

**11. `repos sources add` help example uses a flag that does not exist.**
- Tests: `TestR8DocsCommandsExist`, `TestR8DocsHelpExamples`.
- File: `internal/cli/cmd_env.go:490`, which feeds `docs/guide/cli-reference.md:2664`. The example is `--token-stdin < token.txt`; the command has no such flag (`cmd_env.go:518`).
- Expected: drop the flag. `--token` already reads stdin when omitted.
- This is the only mismatch among 645 command lines (197 distinct paths) in `docs/guide/*.md`, `README.md` and `skills/**/*.md`.

### Cosmetic

- `scripts/dev-cluster.sh:6`: the header says `env` prints exports for `CROWDVM_API_KEY`; it prints `CROWDVM_CONFIG_DIR` and `CROWDVM_API_BASE` (`:51-52`).
- `scripts/install.ps1:11`: `$ProgressPreference` is not set to `SilentlyContinue`, so downloads are slow on Windows PowerShell 5.1.

### Unconfirmed (no browser)

`ci_jobs.html_url` and `ci_repos.html_url` are stored from a linked GitHub Enterprise/Gitea API without a scheme check (`feat_ci.go:447, 497`) and used as `href` / `window.open` (`web/js/pages/ci.js:111, 170, 279`). A `javascript:` URL from a member's own Gitea could run in the dashboard. It depends on browser handling of `target=_blank rel=noopener`; not tested.

## Checked and held

- **`clientIP`** (`util.go:82`): rightmost entry of the last `X-Forwarded-For` header; an invalid last value falls back to `RemoteAddr`. A client cannot forge the address when the proxy appends. A proxy that appends `ip:port` or `[v6]:port` fails `ParseIP` and the client is keyed on the proxy address (shared rate limit; not an exploit).
- **`handleGetNode`**: a non-manager loses `remoteAddr`, `earnedCredits` and other tenants' sandbox ids. `nodeJSON` still carries `info` (name, version, images, `directHost`) for everyone, as the list route does.
- **`handleNodeConnect` and removed nodes**: `DELETE FROM nodes` removes the token row. `nodeByToken` then answers 401 on connect and on the HTTP node API. The drained node still connects, as intended.
- **`onGuestEvent`**: the node check holds for guest events. The scrub-resume path (`reconcileVM` node_offline recovery) re-runs the scrub. The holes are the other event types, bugs 3 and 4.
- **`recordTaken`**: a snapshot from a node that does not host the sandbox is recorded as deleted data, not as head.
- **`orgs.go` delete cascade**: credential profiles, accounts and custom APIs are removed. Env versions kept for saved-snapshot sources use `IS NOT NULL` correctly, so `NOT IN` is safe. Orphaned retained versions are by design (leak only after the snapshot is deleted).
- **`lifecycle_secure.go` scrub query**: `e.owner_id=? OR v.env_id=?` with an empty env id matches nothing. `scrubForBoot` drops paths the config writes itself.
- **`foreign deploy` race**: `secureAsync` clears `pendingScrub` after `runScrub` without the `scrub:` lock. A scrub added in the window is lost, but it needs a concurrent env update during a 3-minute scrub, so I did not pursue it.
- **`patchOldImage`**: the `pkill/pgrep -f "Xvnc :0( |$)"` pattern matches only the display-0 Xvnc command line. Stop, kill, clear the locks, start is the right order. It restarts the desktop on every guest start of an old image (loses open apps); it cannot be tested without systemd.
- **`cgroup.go`**: `sharedTaskLimit` skips `max`, falls back to `threads-max`. `killGroup` uses `cgroup.kill` with a per-pid fallback. Each cgroup gets 3/4 of the shared limit, so two runaway commands can still starve the rest (known trade-off).
- **`capFile`**: the bool return and the marker rewrite are correct. A genuine file of exactly `limit+len(marker)` bytes is treated as cut (edge case).
- **`mux.Join` / `halfCloseIdle`**: each write resets the timer, the reply after a half-close arrives. A backend silent for more than 1 minute after the half-close is cut, as documented.
- **`serveBoxAlias`**: `authenticate` before the body parse; an anonymous request goes through unread; the 8 MiB cap applies. `renameKeys` also renames user-supplied JSON keys (`box`, `boxes`) in legacy requests; legacy-only, not pursued.
- **`store.Open` retry**: `TestBughuntR7bConcurrentOpenMigrate` passes.
- **Environment cap** (`feat_env.go:698`): counts non-deleted environments of the owner, not tokens.
- **Dashboard (web/js)**:
  - `h()` builds nodes with text nodes and `setAttribute`. The only `innerHTML` sinks use constant icon and logo markup. No `document.write`, string `on*` attributes or `eval`.
  - CSRF holds: `authenticate` (`auth.go:259-266`) requires `X-CrowdVM-CSRF: 1` for unsafe methods on cookie auth, and `api.js`, `stream()` and the explorer's raw `fetch` all send it. Websockets check Origin.
  - No state-changing GET routes besides the login link.
- **Docs vs CLI**: `cli-reference.md` matches a fresh `gendocs` run exactly. All `crowdvm-node` and `crowdvm-server` commands and flags in the docs exist.
- **`install.sh`**: `set -eu`, correct quoting, temp file plus `mv`, no `sudo`, no duplicate shell-init lines, correct `/dev/tty` handling under `curl | sh`. `scripts/` and the embedded installers are identical. `dist.sh` layout matches `ImportDownloads`.

## Design note (not counted)

`install.sh`, `install.ps1` and `self-update` download binaries over the server's default `http://` URL with no checksum or signature. `dist.sh` writes `SHA256SUMS`, but `ImportDownloads` ignores it and the server never serves it. Checksums from the same server would not stop a LAN attacker anyway; it needs HTTPS or signing.

## Not verified

- The dashboard bugs (1, 2, unconfirmed `javascript:` URLs) in a real browser.
- `patchOldImage` and the cgroup code on a real guest.
- PowerShell findings (no `pwsh` here), `shellcheck` (not installed).
- Whether a node can combine bug 4's `Supersedes` path with a known snapshot id.

**TL;DR:** 11 bugs plus code-only notes. The worst: a guest file served as HTML on the dashboard origin (stored XSS to account takeover), a frameable dashboard, and foreign nodes still able to poison other nodes' sandboxes (`vmStarts`, `snapshot.failed`). The round-7 SIGHUP fix breaks `nohup`. The 200-key cap counts system-minted sandbox tokens. Org delete leaves an active CI link with its token. Docs match the CLI except one help example. Routes, DOM escaping and CSRF held.
