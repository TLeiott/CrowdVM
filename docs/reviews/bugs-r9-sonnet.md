# Bug hunt r9 (sonnet)

2026-11-11. Audited the uncommitted master tree. Product code unchanged. Unit tests only (servertest, the guest test env, a SDK fake server). No cluster, VM or e2e test.

I took the Go side (round-8 fix audit, guest error codes). One sub-agent took both SDKs. I re-ran every test below myself and each one fails. I also re-ran the existing suites: they pass apart from the new tests.

Test files:
- `internal/server/bughunt_r9_test.go`
- `internal/guest/bughunt_r9_test.go`
- `internal/cli/bughunt_r9_test.go`
- `sdk/python/tests/test_bughunt_r9.py`
- `sdk/typescript/test/bughunt_r9.test.mjs`

Run:
- Go: `go test -count=1 -run TestBughuntR9 ./internal/server/ ./internal/guest/ ./internal/cli/`
- Python: `cd sdk/python && PYTHONPATH=src python3 -m unittest tests.test_bughunt_r9` (the suite uses unittest, pytest is not installed).
- TypeScript: `cd sdk/typescript && node --test test/bughunt_r9.test.mjs`. These import `../dist/index.js`. `tsc` is not installed, so `dist` was not rebuilt. The `dist` code on the paths below matches `src`.

## Go: bugs proven by a failing test

### 1. A guest or hosted app can set and clear cookies on the dashboard's origin (security, medium)

Two tests, one cause: the proxies filter a few response headers and let the rest through.

- `TestBughuntR9GuestHeadersReachServerOrigin`
  - File: `internal/server/passthrough.go:266-270` (`ProxyToGuest`, `ModifyResponse` calls `guardGuestContent`, defined `:289-300`).
  - Observed: the signed file URL (`GET /files/signed?...`, no auth, the dashboard hands it out) returns the guest's `Set-Cookie: crowdvm_session=attacker; Path=/`, `Clear-Site-Data: "cookies", "storage"` and `Refresh: 0; url=https://evil.example/`.
  - Expected: none of them reach the server origin. `guardGuestContent` fixes `Content-Type`, `nosniff` and the CSP, but the round-8 fix stopped there.
- `TestBughuntR9PathRoutedAppSetsDashboardCookie`
  - File: `internal/server/feat_host.go:738-745` (the path-routed branch of `serve`; `ModifyResponse` only adds the CSP sandbox and rewrites a root-relative `Location`).
  - Observed: a public port at `/p/{id}/3000/` whose app answers `Set-Cookie: crowdvm_session=attackers-session; Path=/` and `Clear-Site-Data: "cookies"` passes both to the browser. The server strips only the *request* cookies (`stripCookies`, `:733`).
  - The CSP `sandbox` takes away scripts, not response headers.
- Cause: the sandbox's user controls the guest agent (passwordless sudo) and its hosted apps. Their responses go out on the dashboard's origin, where the session cookie is host-only and has no `__Host-` prefix.
- Impact: a link to a signed URL or a public `/p/` URL logs the victim into the attacker's account (login CSRF: the victim then creates sandboxes and pastes secrets into the attacker's account) or logs them out. Cookies the app sets with `Path=/` are also sent to every other path-routed app, so one user's app can read another's.
- Expected: drop `Set-Cookie`, `Clear-Site-Data`, `Refresh`, `Location` to other origins and similar from guest answers. For path-routed apps drop `Set-Cookie` (or force `Path=` under the prefix and refuse the session cookie's name), or only route by host.

### 2. A foreign deploy keeps the saver's coding-agent prompts and events (security, medium)

- Test: `TestBughuntR9ScrubHistoryKeepsAgentConversations` (`internal/guest`).
- File: `internal/guest/apply.go:173-176` (`runScrub`, `if s.History`). The agents store lives in `a.data/agents` (`agentsmgr.go:14`: `state.json`, `events.jsonl`).
- Observed: after `runScrub(&GuestScrub{History: true})`, `GET /v1/conversations` still has `lastPromptPreview: "use AWS_SECRET=TOPSECRET-R9 to deploy"`, and `GET /v1/events` still returns the prompt and the agent's answers in full.
- Expected: the round-8 fix (bugs-r8-opus 4) said the saver's history must not travel. It wipes the command history and `~/.ascii/processes` only. The prompts, conversations, steers and events travel to the new owner, and the server serves them (`conversations`, `events` routes).
- Cause: `history.wipe()` and `os.RemoveAll(a.processLogDir())` are the only things `History` clears. The agent manager keeps its own in-memory maps and files, and nothing resets them.

### 3. The replay of an Idempotency-Key create drops `hostedPorts` (wrong-result, medium-low)

- Test: `TestBughuntR9IdempotentReplayKeepsHostedPorts`.
- File: `internal/server/account.go:411-424` (`idemBegin` replay branch); the first answer is built at `sandboxes.go:1020-1033`.
- Observed: `POST /sandboxes` with `hostedPorts: [{port: 3000}]` and `Idempotency-Key: k` answers with `hostedPorts[0].url` (the access token is in it). The same call again returns 202 `idempotentReplay: true` with the same sandbox but no `hostedPorts`. A client that lost the first response cannot learn the URLs or their tokens.
- Same cause, code only: `sandboxes.go:1018` stores only `created[0].ID` (`idemFinish`), so a replay of a `count: N` create (clones from a snapshot) returns one sandbox, not N. The other N-1 sandboxes are running and billed, and the retrying client never sees them.
- Expected: store what the answer carries, or rebuild it (list the sandboxes made under the key; re-read the hosted ports).

### 4. A SIGINT inherited as ignored is switched back on (wrong-result, low)

- Test: `TestBughuntR9InheritedIgnoredSigintStaysIgnored` (`internal/cli`).
- File: `internal/cli/app.go:88-96` (`cancelSignals`). `os.Interrupt` is always in the list; SIGHUP and SIGQUIT are added only when not ignored.
- Observed: with SIGINT ignored, `cancelSignals()` still returns it. `signal.NotifyContext` then installs a handler and the ignore is gone.
- Cause: a background job of a non-interactive shell (`crowdvm exec ID long-job &` in a script or CI step) starts with SIGINT and SIGQUIT ignored so the script's Ctrl-C does not reach it. The round-8 nohup fix covers SIGHUP and SIGQUIT only. With SIGINT re-enabled, that Ctrl-C cancels the job and `runLong` stops the remote command.

### 5. An unknown history cursor ends the list silently (wrong-result, low)

- Test: `TestBughuntR9HistoryUnknownCursorIsSilentEnd` (`internal/guest`).
- File: `internal/guest/history.go:207-215` (`history.list`): `start = len(all)` when no record has the cursor's id.
- Observed: `GET /v1/command-history?cursor=cmd_pruned_away&limit=2` answers 200, `commands: []`, `nextCursor: null`.
- Expected: 400 `invalid_cursor`, as the events list (`agents/http.go:322`) and the server's own copy of the history (`feat_history.go:461`) answer. A record pruned or wiped between two page requests ends the client's pagination with no error, and the older commands vanish from its view. The answer also differs depending on whether the sandbox runs (guest) or sleeps (server copy).

## Go: code only (the exact line, no test)

### 6. `KillMode=process` leaves VMs running with no way to remove them (leak, medium)

- Files: `internal/node/service/service_linux.go:41-44` (unit), `:89-97` (`Uninstall`).
- A `systemctl --user stop crowdvm-node`, a plain `uninstall-service` or a node that is removed while offline now leaves every qemu process running. The old `KillMode=mixed` killed them. Nothing stops them afterwards:
  - `Uninstall` runs `disable --now` and deletes the unit. It does not stop or even list the machines.
  - There is no `crowdvm-node` subcommand that stops or reaps VMs. The commands are `join`, `run`, `bench`, `status`, `config`, `install-service`, `uninstall-service` and `version`.
  - `docs/guide/node-owners.md:134` says "`crowdvm nodes remove`, then `uninstall-service`". That destroys the VMs only while the node is online (`nodes.go:1072-1078`). Offline, they stay.
- The server also rebuilds a sandbox on another node after 3 minutes of node_offline, so a stopped node leaves a second live copy of it behind, with the user's data and network access, until the node starts again.
- Expected: `uninstall-service` (and ideally `stop`, via `ExecStop` or a `crowdvm-node` command) should drain or kill the machines, or at least print how many are left and how to stop them.

### 7. A second Ctrl-C is swallowed (stuck, low)

- File: `internal/cli/app.go:78-79`. `signal.NotifyContext`'s handler stays registered until `stop()` runs, and `stop` is deferred to the end of `Main`.
- After the first SIGINT the context is cancelled, and `runLong` then sends its stop request with a 15 s timeout (`cmd_exec.go`). A second Ctrl-C during that wait does nothing, so with the server unreachable the CLI cannot be interrupted for up to 15 s. Calling `stop()` once `ctx.Done()` fires would restore the default.

### 8. `self-update --check` calls a binary that cannot run "available" (cosmetic)

- File: `internal/cli/cmd_config.go:265-275`. The refusal added in round 8 is `offered == "" && !check`. With `--check` an unrunnable download falls through: `available := !sameHash(...)` is true, the version prints as "(unknown version)", and the command exits 1, which `self-update --check && echo up to date` reads as "update available".

### 9. Still open from r8: node events trusted for any sandbox (low)

- `nodes.go:630-633` (`EvTransfer` writes `hydrationProgress` for any sandbox id) and `snapshots.go:258-262` (`EvSnapshotUploaded` with `ref.Supersedes` marks periodic snapshots of any `sbID` deleted, no node check). Round 8 fixed `snapshot.failed` and left these two. They need ids a foreign node would have to know, so I did not write a test.

### 10. A binary file read with the default encoding is mangled (unconfirmed)

- `GET /v1/files?path=bin` on a file with bytes `ff fe 00` answers 200, `encoding: utf8`, `size: 3` and a content of replacement characters. The write of such a file through base64 round-trips, but the default read does not. The Boat page only says "UTF-8 text by default or base64 for binary content", so I did not count it. A 400 or an automatic base64 would be safer.

## SDK bugs (sub-agent, re-run by me)

All 11 fail as stated. Python: 5 failures and 6 errors, TypeScript: 4 failures.

| # | Severity | Test | File:line | What happens |
|---|---|---|---|---|
| S1 | wrong-result | TS `a command stream that ends without an exit frame is an error, as in Python` | `sdk/typescript/src/client.ts:237` (`lines`: `if (chunk.done) break;`) | `command({stream:true})` and `streamCommand` end cleanly after `started`/`stdout` frames with no `exit` or `error` frame. Python raises `APIConnectionError` (`test_r3.py::test_stream_eof_requires_exit`). A dropped connection looks like a finished command. `followCommand` has its own guard. |
| S2 | wrong-result | Py `test_truncated_json_body_is_a_connection_error`, `test_truncated_bytes_body_is_a_connection_error`; TS `a truncated body of a bytes operation is an APIConnectionError` | `client.py:186` (`_decode`, `resp.read()`), `client.py:271` (bytes branch); `client.ts:~254` (`await sent.res.blob()`) | A body cut short, or a read timeout mid-body, escapes as `http.client.IncompleteRead` (Python) or `TypeError: terminated` (TS), not `APIConnectionError` as both READMEs say. |
| S3 | stuck | Py `test_stream_waits_for_headers_only_as_long_as_timeout` | `client.py:194` (`_lines` default `timeout=None`), `:295` (`command()` passes none) | A server that accepts and never answers hangs `command(stream=True)` and `stream_command` for good. TS got this fix in r3 #26. |
| S4 | wrong-result | Py `test_with_options_base_url_is_normalised_like_the_constructor` | `client.py:87-94` (`with_options` uses a bare `setattr`) | `with_options(base_url="http://h:8420")` keeps it as is, so calls go to `/sandboxes` without `/api/v1`. A trailing slash gives `//`. TS `withOptions` normalises. |
| S5 | security (low) | Py `test_authorization_is_not_forwarded_to_another_origin_on_redirect` | `client.py` / `_transport.py` (urllib's default redirect handler) | A redirect to another origin forwards `Authorization: Bearer ...`. TS `fetch` strips it. |
| S6 | wrong-result | TS `a per-call header replaces a client default header of another case` | `client.ts:153,160` (`Object.assign(headers, ...)` on a plain object) | Client `{"x-boat-org": "a"}` plus a per-call `xBoatOrg: "b"` sends `x-boat-org: a, b`. The same for `authorization`: `Bearer k, Bearer other`. Python sends `b`. |
| S7 | wrong-result / security (low) | TS `path parameters that are dot segments do not change the endpoint`; Py `test_dot_segment_path_params_are_not_sent_raw` | `client.ts:262` (`encodeURIComponent`), `client.py:~238` (`quote(safe="")`) | `get({sandboxId: ".."})` requests `GET /api/v1/`, `"."` requests `GET /api/v1/sandboxes/`. Python puts `..` on the wire (the Go mux then redirects). Both libraries leave `.` alone, so an id or key from user input changes the endpoint. |
| S8 | crash (low) | Py `test_retry_after_garbage_values_do_not_crash` | `errors.py:62-66` (`float(h)` unchecked), `client.py:172` (`self._sleep(min(delay, max))`) | `Retry-After: -5` raises `ValueError: sleep length must be non-negative`; `nan` raises `ValueError: Invalid value NaN`. TS clamps. |
| S9 | crash (low) | Py `test_client_timeout_none_does_not_break_command` | `client.py:297` (`max(self.timeout, ...)`) | `CrowdVM(timeout=None)` ("no timeout" everywhere else) makes `command()` raise `TypeError`. |
| S10 | crash (cosmetic) | Py `test_async_client_can_be_copied` | `aio.py:50` (`__getattr__` reads `self.client` before `__init__` ran) | `copy.copy` and `copy.deepcopy` of `AsyncCrowdVM` hit `RecursionError`. |
| S11 | stuck / leak | Py `test_async_wait_cancelled_by_wait_for_does_not_keep_polling` | `aio.py:~47` (non-generator helpers run in `to_thread`) | `wait_for(cvm.wait_until_ready(..., timeout=4), 0.3)` takes 3.9 s: the thread cannot be cancelled and `asyncio.run` waits for it. With the default 300 s timeout that is up to 5 minutes. r3 #25 fixed streams only. |

Also noted by the sub-agent, no test:
- Both SDKs ignore an HTTP-date `Retry-After`. The server only emits seconds, so it matters only behind a proxy.
- A `Retry-After` above `max_retry_delay` is capped, so the client retries early.
- `503 api_key_creation_disabled` is permanent but gets 3 retries.
- Python `wait_until_ready(timeout=0)` times out at once, while TS `timeoutMs: 0` means never.
- `AsyncCrowdVM.paginate` is unreachable.
- Many concurrent async streams can starve the default thread pool.
- Python `request()` raises on 304; TS returns `null`.

## What held

- **`guardGuestContent`** covers every route that reaches the guest through `ProxyToGuest`: the pass-through prefixes, signed URLs (`files/signed`), `desktop/{status,screenshot,recording,...}` (`feat_desktop.go:398`), `prompt` (`acct_prompt.go:174`). Content type, `nosniff`, the CSP and the attachment disposition are right for html, svg, xml, javascript and xsl, including upper case and `inline; filename=`. A `..` in `rest` does not climb: the node's mux cleans and redirects. The routes that do not go through it (`feat_snapfiles.go`, `nodes.go` blobs, `feat_install.go`) all send `attachment` and `application/octet-stream`. Bug 1 is the headers it does not touch.
- **`web.go` framing headers.** `X-Frame-Options: DENY` and `frame-ancestors 'none'` on every dashboard path except `/desktop/`. The exempt SPA fallback cannot be steered to `/device`: the router reads `location.pathname`, and browsers resolve `..` and `%2e%2e` before sending.
- **`reconcileVM`.** `vmStarts` is keyed per node, so a foreign node's report cannot hide this node's crash. The VMError branch for `node_offline` works for both `boot timeout` (`restartLostMachine`) and other errors (`machine_error`, `sandbox_not_ready` from `guestReady`); existing tests cover both. Open nit: `vmStarts` is never pruned, and a node clock that steps back could drop a later crash report (needs an NTP step; not tested).
- **`nodes.go`.** `snapshot.failed` needs the node to run the sandbox, and `snapshotTakenOn` or `hasLocation` for the layer.
- **CLI signals.** SIGHUP and SIGQUIT are cancelled and stay ignored under nohup (r8 tests). During `crowdvm ssh` the child is not tied to the context. `runLong` stops the command on every end of the follow loop except `process_lost`.
- **`apikeys.go`.** The cap counts user keys only (`sandbox_id IS NULL`, not revoked, not expired; `expires_at` is epoch millis on both sides). `POST /api-keys/scoped` goes through the same handler, so it is capped.
- **`orgs.go`.** Deletion unlinks the CI links and drops their repos and tokens, revokes the org's API keys, removes credential profiles and environments (keeping versions a saved snapshot needs), and returns sandboxes and saved snapshots to their owners.
- **Installers.** `install.sh` and `install.ps1` match `dist.sh` and the node's file lookup; the `'` quoting in the PowerShell profile line is doubled. `downloadPath` rejects traversal.
- **`self-update`.** An unrunnable download is refused unless `--check`; a version that is not older is not installed without `--force`.
- **Guest `outputLimit` and the marker removal.** The cap is one eighth of the history file system, the marker is gone from stdout, and `outputTruncated` is recorded. `history.wipe` runs after `recoverProcs`, so records of commands that ran when the snapshot was taken are `lost` and wiped. A running command keeps its record.
- **Guest error codes.** About 90 requests against the guest: path outside home or `/tmp` (also via `..`, a symlink to a file, a symlink to a folder, absolute `/etc/...`) gives 400 `invalid_path` with the message "path must resolve under /home/user or /tmp" (names the roots, as `ssh-access.md` says). A missing path gives 404 `file_not_found`, an unreadable folder 403 `permission_denied`, a file over 5 MiB 400 `file_too_large`, a directory 400 `is_directory`, a file used as folder 400 `not_a_directory`, `/home/user` and `/tmp` cannot be deleted or moved, a move onto an existing path is 409 `destination_exists`, a bad mode 400 `invalid_mode`, a bad encoding 400 `invalid_encoding`, bad base64 400 `invalid_content`. Commands: `invalid_command`, `invalid_timeout` (1-600), `invalid_cwd`, `process_not_found`, `command_not_found`, `invalid_limit`, `prompt_required`, `invalid_provider`, `no_running_turn`. The Boat OpenAPI page lists only `400 BadRequest` for the file routes, so these are the only codes to compare with.
- **`public-url` file.** Written after `server.New`, trimmed of a trailing slash, read by `join-token create` ahead of the guess; `--public-url` wins.
- **SDKs.** Retries (429/503), Retry-After seconds, max attempts, no retry of POST on a connection error, one Idempotency-Key for all attempts of a call. Error envelope (`code`, `message`, `details`, `requestId`, `retryAfter`) and fallbacks for HTML, empty and non-object bodies. NDJSON with split UTF-8, CRLF, blank lines, last line without newline, 5 MB lines, error frames. Pagination field names (`nextCursor`, `hasMore`). 123 operations identical in Python and TS. Query encoding. Webhook signature (constant-time compare, tolerance, rotation).

## Not verified

- Anything that needs a live cluster: bug 6's real systemd behaviour on stop (read from the unit and the `Uninstall` code), a real browser for bug 1 (the headers are what the browser acts on, and the test checks them).
- Windows (`service_windows.go`, `install.ps1` under PowerShell).
- The TypeScript `dist` was not rebuilt (`tsc` missing).

## Mistakes on my side

- My first guest-test probe sent a move with the wrong field names (`from`/`to` instead of `source`/`destination`) and read the 400 as a bug. I redid it with the right names; the codes are fine.

**TL;DR:** 5 Go bugs with failing tests (Set-Cookie and friends reach the dashboard origin from guests and path-routed apps, the scrub keeps agent prompts, idempotent replay drops hostedPorts, SIGINT ignore is undone, unknown history cursor ends the list silently), 11 SDK bugs with failing tests, and 5 code-only findings (orphaned VMs after `uninstall-service`, the swallowed second Ctrl-C, `self-update --check`, two still-open node-event gaps, and a binary read mangled by the default encoding). The rest of the round-8 fixes held.

## Fixes (SDK)

All 11 SDK bugs fixed. Only `sdk/` changed. Python `python3 -m unittest`: 67 tests OK. TypeScript `npm test` (tsc, type check, `node --test`): 45 pass, 0 fail.

- **S1** TS `lines()` takes `requireExit`; `command({stream:true})` / `streamCommand` throw `APIConnectionError` when the stream ends without an `exit` or `error` frame. `attachCommand` is left alone (`followCommand` has its own guard and the `reconnect` frame ends it legitimately).
- **S2** Python `_read()` (JSON and bytes bodies) and TS bytes `blob()` map truncated bodies and mid-body timeouts to `APIConnectionError`.
- **S3** Python `command(stream=True)` passes `timeout=self.timeout` for connecting and the headers; right after the headers the socket timeout is reset to none (`set_read_timeout`), so a quiet command is not cut off.
- **S4** Python `with_options(base_url=...)` normalises like the constructor.
- **S5** Python opener has a redirect handler that drops `Authorization` when the origin (scheme + host) changes.
- **S6** TS `mergeHeaders` replaces a header of another case (client defaults, built-ins and per-call headers).
- **S7** Both SDKs refuse `.`, `..` and empty path params (`ValueError` / `TypeError`, "invalid path parameter ...") before sending, in `_op`/`split` and in `command()`; everything else stays percent-encoded. The two tests asserted the old behaviour (`ApiError`, `%2E%2E` on the wire; fetch would collapse that to the parent path), so I changed them to assert the refusal and that nothing is sent.
- **S8** Python `retry_after_seconds` ignores negative, NaN and infinite values (default backoff is used).
- **S9** Python `command()` works with `timeout=None`.
- **S10** `AsyncCrowdVM.__getattr__` raises `AttributeError` for `client` and dunder names, so `copy` / `deepcopy` work.
- **S11** `AsyncCrowdVM` non-generator calls set a `threading.Event` (context var) that is set when the awaiting task is cancelled; `_poll` waits on it instead of `time.sleep` and stops with `InterruptedError`. Other helpers that poll in generators were already covered by `_agen`.
