# Bug hunt r20 (sonnet)

2026-11-12. Product code unchanged. I only added `internal/server/bughunt_r20_test.go`, `internal/mux/bughunt_r20_test.go`, `internal/guest/agents/bughunt_r20_test.go` and this report. Unit tests only (the server's `servertest` environment with a fake node and fake GitHub servers, raw TCP pairs for `mux`, a bare `agents.Manager` for the guest). No cluster, VM, e2e test or sub-agent. Tree: `main` at `443337b`, in a detached worktree. `qemu-img` is installed here; I used it only to read the sparse-upload code against the existing test, not to add one.

## What I audited

- **Round 19's fixes (`443337b`)**
  - `guest_channel.go` `copyRekeyErr` and its callers (`handleCreateSandbox` saved/shared deploys, `handleFork` live/stopped, `diskRekeyed`, `ensureDiskKey`): holds.
  - `feat_legacy.go` raw-route detection: `files/raw` with and without a trailing slash, the `boxes/` rewrite before the check, the signed-URL upload (`files/signed` is an `/api/v1` URL, so it never goes through the alias), every other guest route that takes a body (`PUT /files` is JSON, `artifacts` and `files/raw` answer `application/octet-stream`). Holds. What is left of r19 bug 2 is in Code only.
  - `feat_snapfiles.go` `handleInventory`, `feat_retention.go` erase, close, reopen, purge: **bug 3**.
  - `acct_billing.go` quota expiry (ledger kinds `quota`/`grant`/`usage`/`transfer`/`earn`/`extra_start`, a wallet with no quota row, a quota raised mid-month, a policy removed and set again) and `policyGuard` for resume: holds (see What held).
  - `agents/store.go` `load`, `mux.Join` and every one of its users (`guest/egress.go`, `guest/internal.go`, `node/direct.go`, `node/api.go`, `server/guest_egress.go`, `server/passthrough.go`, `server/feat_desktop.go`): **bug 4**.
  - `sandboxes.go` subdomain reservation and `feat_saved_allowance.go`: holds.
  - The sparse upload (`qemu/store.go` `CompactSnapshot`, `node/transfer.go` `UploadSnapshot`, the server's `handleBlobPut`): read; one risk in Code only, nothing provable without a VM.
- **Least-read areas**
  - Environments and git: `feat_env.go`, `feat_env_repos.go`, `feat_env_dotenv.go`, `feat_env_crypto.go`, `feat_env_guest.go`, `lifecycle_secure.go` (what a fork, a saved snapshot and a share carry, the scrubs): **bugs 1, 3, 5**.
  - CI: `feat_ci.go` in full (poll, `newJob`, `boot`, `track`, `connect`, `public`, `unlink`, `overview`): **bugs 2, 6, 7**.
  - Guest: `internal/guest/docker.go`, `internal/guest/agents/computer.go`, `internal/guest/creds.go` and `writeGitCredentials` / `writeEnvFiles`: **bug 8**.

## How to run

```
go test -count=1 -run TestBughuntR20 ./internal/server/ ./internal/mux/ ./internal/guest/agents/
```

The 8 tests fail today (6 in `internal/server`, 1 each in `internal/mux` and `internal/guest/agents`). Two take about 6 to 8 s (they wait for a job that never starts); the rest are under a second. `go vet` of the three packages and `gofmt -l` are clean. I checked that the pagination test passes once `pollLink` asks for the next page (a throwaway edit of the worktree, reverted). The others fail on the missing behaviour itself.

## Bugs proven by a failing test

### 1. A git source's `apiUrl` is called from the server with no outbound guard, and the answer is echoed (security, medium)

- Test: `TestBughuntR20GitSourceReachesLoopbackAndEchoesTheAnswer` (`internal/server/bughunt_r20_test.go:23`)
- Code: `internal/server/feat_env_repos.go:111` (`handleGitSourceAdd`: only the *base* URL is parsed, `apiUrl` not at all, no `checkOutboundURL`), `:183` (`gitHTTP` is a plain `http.Client`, no `guardedDial`, redirects followed), `:185` (`gitGet`: a non-200 answer returns its first 512 bytes in the error), `:517` (`syncError` in `GET /repos`).
- Observed: with `webhook-allow-local` off, `POST /repos/sources {kind: github, baseUrl: https://github.com, apiUrl: http://127.0.0.1:<port>, token}` answers 201. `GET /repos?sync=true` then makes the server call that URL with `Authorization: Bearer <token>`, and the response says `syncError: "github.com: HTTP 403 internal-only-admin-page"`.
- Expected: the same guard webhooks, OTel exports, CI links and custom AI APIs have: refuse loopback, link-local and metadata targets when the source is saved and on every dial. A user with `account.admin` can reach the metadata service, the server's own loopback API and the node UIs, and read the body of any non-200 answer.
- Cause: the git-source code does not use `checkOutboundURL`/`withOutboundPolicy` anywhere.

### 2. CI polls the first 100 repositories of an account only (wrong-result, medium)

- Test: `TestBughuntR20CIOnlyPollsTheFirstHundredRepositories` (`internal/server/bughunt_r20_test.go:54`)
- Code: `internal/server/feat_ci.go:448` and `:451` (`/orgs/{login}/repos?per_page=100&limit=50`, `/user/repos?affiliation=owner&per_page=100&limit=50`: one request each; `limit=50` is not a GitHub parameter and `ciGH.do` does not follow `Link` headers).
- Observed: a fake GitHub with 101 repositories (page 1 holds 100, page 2 holds `alice/zzz-late`) and a queued `crowdvm` job in the 101st: after 8 s there is no CI job for it. The repository never shows in the overview either.
- Expected: every repository of the account is polled. GitHub sorts these lists by name by default, so for an organization with more than 100 repositories the ones after the 100th never get a runner: their jobs wait on GitHub for a day, with no error anywhere.
- Cause: no pagination. (The runs listing has the same single-page shape, 50 per status; see Code only.)

### 3. Erasing an account leaves its sole-owned organization, with its sealed environments, in the database (leak, medium)

- Test: `TestBughuntR20EraseLeavesSoleOwnedOrganizationSecrets` (`internal/server/bughunt_r20_test.go:111`)
- Code: `internal/server/feat_retention.go:395` (`erase` deletes the `org_members` rows) and `:368-390` (the r19 guard only refuses an owner whose organization has other members; `purge` deletes by `owner_id=uid`, an organization's data has `owner_id=org.ID`).
- Observed: `r20solo` creates the organization `R20Solo` (its only member), adds an organization environment `deploy` with `DEPLOY_TOKEN=...` and erases the account. `environments` still holds 2 rows for the organization and `environment_versions` 3 sealed versions; the same holds for its credential profiles, git sources, webhooks, wallet and saved snapshots. Nobody is a member any more, so nobody can reach or delete them.
- Expected: `handleDeleteOrg`'s cascade (environments and versions, git sources, credential profiles and accounts, CI links, saved snapshots handed to their savers) for every organization the account owns alone, in the same erase. "Erase my account" is the GDPR path; secrets of the account's own organization are the account's data.
- Cause: r19 bug 5 was closed for organizations with other members only.

### 4. The r19 abort in `mux.Join` only reaches a bare `*net.TCPConn`; every wrapper closes cleanly (wrong-result, low; partial r19 fix)

- Test: `TestBughuntR20JoinAbortIsLostOnWrappedConnections` (`internal/mux/bughunt_r20_test.go:20`)
- Code: `internal/mux/mux.go:261` (`dst.(interface{ SetLinger(int) error })`).
- Observed: a sender that resets mid-upload, joined through two `struct{ net.Conn }` wrappers (the shape of `node/api.go` `readerConn`, `guest/internal.go` `hijacked`, `server/passthrough.go` and `server/guest_egress.go` `readerConn`), still reaches the backend as a clean end of input (`io.EOF`). The same test with bare `*net.TCPConn` (the r19 test) passes.
- Expected: the receiver sees the abort. The wrappers embed the `net.Conn` *interface*, which has no `SetLinger` (nor `CloseWrite`), so neither is promoted; a yamux stream (the receiver in `guest/egress.go` and in the node and server tunnels) has neither either. In the product the bare TCP receiver is the final hop of the egress proxy only; a truncated upload into a sandbox, or out of one through two hops, still looks complete.
- Cause: the fix tests the receiver's dynamic type. The wrappers need to forward `SetLinger`/`CloseWrite` (or `Join` needs an abort hook for streams, for example a `Reset`/`CloseWithError`).

### 5. `PUT /environments/{id}` that answers 400 has already renamed the environment (wrong-result, low)

- Test: `TestBughuntR20EnvUpdateRefusedButAlreadyRenamed` (`internal/server/bughunt_r20_test.go:147`)
- Code: `internal/server/feat_env.go:845` (`UPDATE environments SET name=...` and `UPDATE sandboxes SET environment=...`) runs before `:850` (`envResolveAccess`, which refuses with 400) and before the mint at the end.
- Observed: `PUT {name: "r20after", access: {restricted: true, members: []}}` on a personal environment answers 400 (it has no access list), and `GET` then shows `name: "r20after"`. The sandboxes pinned to it show the new name too.
- Expected: a refused request changes nothing. The same order also leaves the name changed when the later `envMint` fails, and `isDefault` flipped when a later step fails.
- Cause: the non-versioned fields are written one by one as the handler goes; validation of the access list comes after the first write.

### 6. A second user can confirm a link to a GitHub account that is already linked (wrong-result, medium)

- Test: `TestBughuntR20CIConfirmAllowsTheSameAccountTwice` (`internal/server/bughunt_r20_test.go:172`)
- Code: `internal/server/feat_ci.go:997` (the `linkId` + `confirm` branch: no duplicate check) and the duplicate check in the main branch (`status='active'` only, so a pending link of someone else does not count).
- Observed: `r20ci1` and `r20ci2` both connect the same GitHub account (the same token on two accounts, or two admins of one organization on two CrowdVM accounts). Both links stay pending, both confirm, and `ci_links` holds 2 active rows for `alice`.
- Expected: the second confirmation is refused with 409 `ci_account_linked`. With two active links the account is polled twice: every queued job gets two sandboxes (`<id>` and `<id>-<linkId>`) and two runner registrations, both billed, and only one runner gets the job.
- Cause: `ci_account_linked` is checked when a link is created active, not when a pending one is activated.

### 7. A CI job recorded before a restart is never started (stuck, medium)

- Test: `TestBughuntR20CIJobQueuedBeforeARestartIsNeverStarted` (`internal/server/bughunt_r20_test.go:205`)
- Code: `internal/server/feat_ci.go:500` (`newJob` inserts the row and returns early on a job it knows), `:608` (`retryWaiting` restarts only `sandbox_id IS NULL AND next_try_at>0`), `:709` (`boot` stores `sandbox_id` before the runner command is sent), `cleanup` (6 h 15 min).
- Observed: a row `queued`, `sandbox_id` NULL, `next_try_at` 0 (what a process leaves when it stops between the insert and the sandbox create) with the job queued on GitHub: 6 s later `job.sandboxId` is still null and the state `queued`. Nothing starts it.
- Expected: after a restart the controller restarts rows that have no sandbox, and resumes or replaces a sandbox whose runner command was not sent (the sandbox exists with `sandbox_state: live` and no runner for up to 6 h 15 min). Restarts are routine (updates); the window is the whole boot of the sandbox, 1 to 2 minutes, plus the up-to-10-minute `WaitUsable`.
- Cause: the job's progress is kept in a goroutine (`busy`), not in the row.

### 8. The computer-use rewrite of `~/.codex/config.toml` glues two tables onto one line (wrong-result, medium)

- Test: `TestBughuntR20CodexBlockRewriteGluesNeighbouringTables` (`internal/guest/agents/bughunt_r20_test.go:15`)
- Code: `internal/guest/agents/computer.go:143` (`s = strings.TrimRight(s[:i], "\n") + strings.TrimLeft(s[end:], "\n")`).
- Observed: `config.toml` holds `model = "gpt-5"`, the first start appends the marked `[mcp_servers.computer]` block, `codex mcp add other` appends `[mcp_servers.other]` after it. The next agent start leaves `model = "gpt-5"[mcp_servers.other]` on one line, then a blank line and the block again. Codex no longer parses its own config, and the same happens at every later start.
- Expected: the text before and after the old block stays separated by a newline.
- Cause: the removal trims the newlines on both sides and joins the halves with nothing. It only works when the block is first or last in the file; the first start puts it after the user's own settings, and Codex's own tooling appends below it.

## Code only

- **Box alias: the remaining half of r19 bug 2** (`internal/server/feat_legacy.go:106`, `renameKeys` over every JSON body and response; cosmetic). Only `files/raw` was fixed. A user-chosen key in a real API body (an env map `{"box": ...}`, a label key `boxes`, a `metadata` object) is still renamed in the request and back in the response, so the Box client sees its own keys but the data is stored as `sandbox`/`sandboxes` and shows that way on `/api/v1`. A test would only show the stored name; I left it as a note.
- **A retried sparse upload can hit `409 blob_exists`** (`internal/node/transfer.go:422`, `internal/server/nodes.go:757`; stuck, low; not tested, it needs a VM and a failing `qemu-img`). The server never replaces a stored blob and compares the declared sha256. The first attempt can store the compacted copy (sha A) and lose the answer; the retry then uploads the original layer (sha B) when `CompactSnapshot` fails this time (`qemu-img` out of temp space in `<data>/tmp`, a changed `info`), or a copy whose bytes differ (`qemu-img convert` runs 8 coroutines by default and does not promise the same cluster order; the existing test compares two runs on a quiet file). `CompactSnapshot` should pass `-m 1` (and `-W` off) and a failed compaction of a layer that may already be stored should be retried, not uploaded as is. The reverse order (original stored by an older node, compacted copy on retry after an update) is the same 409.
- **A `codexComputerBlock` without its end marker deletes the rest of the file** (`computer.go:136-142`: `end = len(s)`). A user who removes only the closing comment loses every table below it at the next start (wrong-result, low).
- **CI lists 50 queued runs per repository** (`feat_ci.go:469`, `per_page=50`, one page). With more than 50 queued runs the later ones wait until the earlier ones start (delay, not loss).
- **`recordDocker` after a second prepare-stop** (`internal/guest/docker.go:98-104`; wrong-result, low). The record is overwritten with what runs *now*; a second prepare-stop on the same boot (the first one stopped the containers and its answer was lost) writes an empty list, so the containers do not come back at the next boot unless they have a restart policy. The node does not retry prepare-stop today, so I did not count it.
- **`mergeJSON` re-encodes `~/.claude.json` through `map[string]any`** (`computer.go:117`): integers above 2^53 lose digits and `<`, `>`, `&` are written as `<` and so on (cosmetic).

## What held

- **r19 re-key**: `copyRekeyErr` for a saved snapshot, a share, a stopped fork and a running fork; the sandbox's own `rekey_from` still gives the retryable 409. `diskRekeyed` clears sandbox and channel.
- **Quota expiry**: a wallet without a quota row or policy expires 0; a quota raised mid-month changes nothing until the next month (the grant only runs when `quota_month` differs); spending counts every debit since the last positive `quota` row, expiry rows themselves are excluded; a removed and re-added policy expires at most the old granted amount minus later spending. `policyGuard` for `resume` with and without `type`, spelling check and the injected `ttlSeconds` are consistent with `create`/`fork`.
- **Subdomain reservation**: `bx-` is refused except a sandbox's own id label; the regexp already forbids upper case, so `BX-` cannot dodge it. Existing `bx-` subdomains stay (known).
- **Saved allowance**: `savedCount` (disk or saving) and the daily charge (disk) agree for a re-save of an existing name; a failed first save costs nothing.
- **`handleInventory`** serves a deleted layer only while a saved snapshot holds it.
- **Environments**: sealed at rest with a per-data-dir AES-GCM key (race on creation handled), secret-file path rules (`..`, `~`, home folders, conflicts, size), `.env` parsing and `envSetVar` (duplicates, comments, quoted multi-line values), org access lists on use and on `envOf`, `envUpgrade` skipping shared sandboxes for login-passing versions, the boot-time and live scrub lists, `--no-env` and wipe, `foreignDeployScrub` (history, harness transcripts, keys), `writeEnvFiles` (the `.bashrc` block and `/etc/profile.d` are rewritten every boot, so a saved snapshot's env does not carry to a deployer).
- **Guest `creds.go`**: a sign-in the user made in the sandbox (hash check) is never overwritten or removed; `writeGitCredentials` removes the file when no token is left.
- **Docker**: `unusedImages`/`normImage` (registry with port, digests, multi-tag images), `restartDocker` (missing containers drop out of one inspect, a container Docker started meanwhile counts as started), prune keeps images of any container (stopped too) and of Compose projects, and keeps all when it cannot tell.
- **Computer-use server**: argument handling (`--` before text and keys, `moveArgs`), scroll buttons, `kill_app`, the `listWindows` fallback, the MCP envelope for unknown tools and errors.
- **CI otherwise**: the public-repository gate (`AllowPublic`, `PublicUsers` with author and starter), `runnerSandbox` ownership check, `track` for a runner that took another job, unlink (token and repos removed, kept sandboxes deleted), `keyReachesOrg`, label parsing and Boat aliases.

## Not verified

- Bug 2 against the real GitHub API (the page-2 request shape is the standard `page=` parameter; a fix could equally follow the `Link` header, the test accepts either only if page 2 is asked as `page=2`).
- Bug 7 across a real restart of `crowdvm-server`: the test writes the row the previous process leaves instead of killing one.
- The sparse upload (`CompactSnapshot`) on a real node, on Windows, with a chain whose parent is not local (known), with `qemu-img convert` determinism under load (see Code only), and its interaction with the node's own copy: the local layer keeps its sparse sha256 while the server stores the compacted one; I found nothing that compares them, but I did not trace every `snapshot_locations` consumer.
- A fork with `environment: <other>`: the fork row starts with an empty `envManaged`, so the source's secret files of the *old* environment are not in `prev.Files` and are not scrubbed (the plan only removes what the *fork's* own record lists). I did not decide whether that is intended (the fork's owner owns both environments).
- Members of an organization can read the CI jobs and logs of every organization link (`visibleLinks`, `jobLogs` with the linker's token), also logs of private repositories they have no GitHub access to. Looks like the Boat team model; not counted.
- The dashboard and the CLI for subdomains, the SDKs, Windows.

## Fixes

All 8 fixed; every `TestBughuntR20` test passes (the mux test's wrapper got the `Unwrap` method the product's wrappers now have).

1. **Git source without outbound guard** (`feat_env_repos.go`): `baseUrl` and `apiUrl` are checked with `checkOutboundURL` when saved (LAN and Tailscale stay allowed), every dial goes through `guardedDial` (redirects too), and a failed call reports the status only, not the remote body.
2. **CI polls 100 repositories** (`feat_ci.go` `ciPaged`): repositories and the runs listing are paged (`Link: rel="next"`, or full pages), up to 20 pages.
3. **Erase leaves sole-owned organizations** (`orgs.go` `deleteOrg`, `feat_retention.go`): the organization-deletion cascade is one function; erase runs it for every organization the account owns alone (the balance check skipped), then purges.
4. **Abort lost through wrappers** (`mux.go` `resetConn`; `Unwrap` on `readerConn` (server, node), `hijacked`, `bufConn`, `limitConn`, `stallConn`): the reset reaches the TCP conn below any wrapper. A yamux stream has no reset and still ends cleanly.
5. **Refused environment update renamed it** (`feat_env.go`): every check and the mint run before the first write.
6. **CI account linked twice** (`feat_ci.go`): a pending link is activated with one conditional update that fails (409 `ci_account_linked`) while another active link exists for the same account.
7. **CI job lost by a restart** (`feat_ci.go` `retryWaiting`): queued jobs without a sandbox are picked up again. Not covered: a sandbox created before the restart whose runner command was never sent idles until its TTL (a code comment says so).
8. **Codex config glued together** (`agents/computer.go`): the text around the removed block stays separated.
