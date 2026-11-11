# Bug hunt r4 (opus): live cluster, 2026-11-11

Setup: one `crowdvm-server` on 127.0.0.1:9960 (`--webhook-allow-local --ai-custom-api-allow-private`, start limits off). Two qemu nodes, `r4n1` and `r4n2`, with status UIs on 9961 and 9962. Each had 6 vCPU, 14 GiB and 50 GiB, with image `ubuntu-24.04-crowdvm-v3`. Fake upstreams ran on 9967 and 9968. Users were `admin`, `alice`, `bob` and `mal`, plus orgs Alpha (alice owner, bob member) and Beta. The dashboard ran in a headless browser tab, signed in as alice and then as mal. Binaries came from `make build` at 10:55.

The tree changed during the run. `feat_history.go`, `lifecycle.go`, `orgs.go`, `credits.go`, `acct_misc.go`, `feat_ci.go` and `guest/files.go` changed at 11:03, after my build. I checked every bug below against the current source. Two bugs I reproduced are already fixed in the tree; they are listed under "Already fixed in the tree".

Everything I started is stopped: server, nodes, VMs and fake upstreams. The data dir `~/.local/share/crowdvm-r4opus` is deleted. Nothing was written to `~/.ssh`, because the CLI ran with its own HOME inside the data dir.

## Bugs

### 1. Every sandbox desktop becomes unusable a few seconds after boot (stuck)

Where: `internal/guest/desktop.go:616` (`watchWallpaper`, a 2 s ticker), `:577-580` (`applyWallpaper` calls `running()` first) and `:71` (`running` dials 127.0.0.1:5900 and closes at once).

Repro (4 of 4 sandboxes):
1. `POST /api/v1/sandboxes/ID/desktop`, then open the `wsUrl` with subprotocols `binary, crowdvm-token.<token>`.
2. The websocket opens. The first frame is `RFB 003.003`, followed by security type 0 and the reason `Too many security failures`. Then the server closes the stream.
3. In the guest, `journalctl` shows `Connections: blacklisted: 127.0.0.1` every 2 s, starting 12 s after Xvnc started. The 593 lines on one sandbox each followed an accept-and-reset from 127.0.0.1.

Observed: the desktop failed on sandboxes `s2`, `priv1`, `sfbox` and `late2` (alice) and on `malbox` (mal), and kept failing on every retry over a minute. The only success was one connection right after a resume. The failed attempt also spends the one-use token, so a retry with the same URL gets 401 `desktop_token_used`.

Expected: the desktop stream works for as long as the sandbox runs.

Suspected cause: TigerVNC counts each connection that closes before the handshake as a security failure. After 5 failures it blacklists the source address, and every connection, the relay included, comes from 127.0.0.1. Because the wallpaper poll reconnects every 2 s, the blacklist never expires. Check for a running desktop without opening a VNC connection: use the process, the X socket or `ss`. Alternatively, run Xvnc with `-BlacklistThreshold 0`.

### 2. `POST /ci/connect` lets any user make the server request any internal URL and read the reply (security)

Where: `internal/server/feat_ci.go:168` (`ciHTTP` is a plain `http.Client`), used through `ciGH.do` at `:176-203`; `connect` takes any `apiUrl` at `:986-995`.

Repro as `mal`, a normal non-admin user:
- `{"token":"x","apiUrl":"http://127.0.0.1:9961/nonexistent?"}` returns 400: `The github API refused the token: CI provider API 404: 404 page not found`. That is the node status UI's own body, read over loopback.
- `{"token":"x","apiUrl":"http://127.0.0.1:9969"}` returns `dial tcp 127.0.0.1:9969: connect: connection refused`, so ports can be scanned.
- `{"token":"x","apiUrl":"http://169.254.169.254/latest"}` makes the server connect to the metadata address. Here it timed out after 30 s; a cloud host would answer.
- `{"token":"x","apiUrl":"http://127.0.0.1:9967/internal-admin?"}`: the fake server logged `GET /internal-admin?/user` and `GET /internal-admin?/users/`, both with `Authorization: Bearer x`. A `?` at the end of `apiUrl` turns the appended path into a query, so any path can be hit. The call returned success and left a pending link. A confirmed link polls that URL every 15 s.

Expected: webhooks, OTel and AI custom APIs refuse loopback, link-local and metadata targets unless the operator opts in. CI should do the same: use `checkOutboundURL` plus the guarded `whClient` dialer, and do not echo upstream bodies for failed links. The same `ciHTTP` client serves the poller (`:803`) and job logs (`:1257`). The bug is still in the current tree.

### 3. The AI proxy forwards `..` path segments, which escape the provider base path along with the real secret (security)

Where: `internal/server/feat_ai_proxy.go:190`. It runs `url.PathUnescape(rest)` and joins the result onto `target.Path` without cleaning it.

Repro (mal's profile has custom API `MYGW`, base `http://127.0.0.1:9967/v1/scoped`, value `REALSECRET123`, proxy on). With the sandbox's `cvmai_` placeholder:
- `/ai-proxy/custom/MYGW/..%2F..%2Fadmin%2Fsecrets` arrives upstream as `GET /v1/scoped/../../admin/secrets` with `Authorization: Bearer REALSECRET123`.
- `/ai-proxy/custom/MYGW/%2E%2E/%2E%2E/admin` arrives as `/v1/scoped/../../admin`.

Observed: most HTTP servers and CDNs resolve dot segments, so the request reaches paths outside the configured base. The worst case is the ChatGPT subscription: its base is `https://chatgpt.com/backend-api/codex` (`:28`, `:157-158`), and `/ai-proxy/openai/v1/..%2Fconversations` reaches `chatgpt.com/backend-api/conversations` with the user's ChatGPT access token. Code in a sandbox, which by design never holds the token, can then read the user's whole ChatGPT history.

Expected: run `path.Clean` on the joined path, and refuse (400) any result that does not stay under the base path.

### 4. One tenant's sandbox stalls OpenTelemetry delivery for every account (stuck)

Where: `internal/server/feat_otel.go:239`. The single export loop calls `collectMetrics` for every destination serially, before it flushes any logs. `sample` at `:503` makes up to four guest file reads per sandbox, each with a 5 s timeout.

Repro: alice and mal both export to a fake collector. In mal's sandbox, as root: `mkfifo /run/f1 /run/f2 /run/f3; mount --bind /run/f1 /proc/stat; mount --bind /run/f2 /proc/meminfo; mount --bind /run/f3 /proc/pressure/io`. Then stop one of alice's sandboxes right after a metrics tick.

Observed: alice's stop event left the server at 186.29, 14.7 s after the stop at 171.59. Both accounts' metrics also went out 15 s after each tick, on two cycles in a row. Without the mounts, logs go out within the 1 s flush. Each such sandbox adds 15 s per cycle. Four or more sandboxes push the cycle past the 60 s interval, so every account's logs then lag by minutes.

Expected: each destination is collected independently, with a time bound per account, and log flushing does not wait for metric sampling.

### 5. Hiding a port or making it private does not close open websockets (security)

Where: `internal/server/feat_host.go:605-708` (`serve`). Access is checked only when a connection opens; nothing tracks or closes live upgraded connections.

Repro (python websocket echo server on 8090 in `s2`):
1. Open `/p/bx_7qa27942/8090/` with a valid `_port_auth` cookie. Then `crowdvm host hide bx_7qa27942 8090`. A new GET now returns 404, but the open websocket kept echoing for 14 of 14 s.
2. Host with `--public` and connect with no token. Then `crowdvm host … --private`, which rotates the token; a new GET without the token returns 401. The open websocket kept echoing for 10 of 10 s.

Expected: Boat's hosting guide says "`host hide <port>` and making the port private also turn the relay off" and that re-hosting "closes its open connections". An owner who hides a public terminal or dev server after spotting abuse cannot cut off an attacker who is already connected. Close the port's live proxied connections when it is hidden, its token rotates or its access changes.

### 6. Deleting a service account leaves its sandboxes undeletable by anyone (leak)

Where: `internal/server/acct_settings.go:387-401` (`handleDeleteServiceAccount`). It removes the membership and disables the user without the `member_has_running_sandboxes` check that `handleRemoveMember` got in round 3. Deletion is creator-only (`internal/server/sandboxes.go:194-210`).

Repro:
1. alice: `POST /orgs/Alpha/service-accounts {"name":"bot2","preset":"ci"}`. The bot creates `bx_qjhg2bat` (org Alpha, ready).
2. alice: `DELETE /orgs/Alpha/service-accounts/<bot id>` returns 200.
3. The sandbox stays `ready`, keeps billing Alpha and keeps its open run. alice can stop it.
4. `crowdvm delete bx_qjhg2bat` as alice (org owner) returns 403 `owner_only`. The server admin's key gets 404. The disabled creator cannot sign in.

Observed: the sandbox and its snapshots stay for good. It was the only sandbox left when I cleaned up.

Expected: refuse the delete while the service account has active sandboxes, as for members, and let org owners and admins delete sandboxes whose creator has left.

### 7. History privacy compares the guest clock with the server clock (leak, minor)

Where: `internal/server/feat_history.go` `store`/`privateAt` (current tree). `started` comes from the guest's `r.StartedAt`; `privateSince` and `privateSpans` come from `store.Now()` on the server.

Measured: in a fresh small sandbox the guest clock was 0.95–1.05 s behind the host. With the guest behind, a command run within about 1 s after `share` is hidden from teammates; I saw this live with `echo shared-period-2`. With the guest ahead, which the creator can arrange with `sudo date`, or which NTP drift or a resumed clock can cause, commands run just before `share` get a timestamp after the boundary and become readable by teammates. Boat says teammates "never read history recorded while the sandbox was private". Classify each record when its first export arrives, or use a guest-reported share time, instead of comparing two clocks.

### 8. Secret-file paths that name directories in the image are accepted, and the file silently fails or replaces the directory (cosmetic)

Where: `internal/server/feat_env.go:552` (`envValidateFiles`). It checks conflicts between secret files only.

Repro: `env set-file sf Desktop` and `env set-file sf .ssh` both succeed. In a new sandbox, `~/.ssh` stays a directory and the guest journal says `file /home/user/.ssh: rename … /home/user/.ssh: file exists`; the secret is missing without any error. `~/Desktop` became a 5-byte file, because the secret is written before Xfce creates the folder, so the desktop session has no Desktop directory. Control characters other than newline are accepted too: `tab\tx` and `a\b` both saved. Refuse well-known home directories, or report write failures in the env status.

### 9. A short node blip leaves permanent "Sandbox failed" notices, also for sandboxes deleted since (cosmetic)

Where: `internal/server/lifecycle_holds.go:40-51` (`onSandboxError`). Nothing removes or resolves the notice when the sandbox recovers or is deleted.

Repro: `kill -STOP` r4n2 for about 15 s, then `kill -CONT`. All three sandboxes were back to `ready` within seconds. 20 minutes later `/api/v1/notices` and the dashboard banner still showed three "Sandbox failed: The node hosting … went offline. CrowdVM waits for the node, then rebuilds the sandbox on another node …" entries, one of them for `bx_6uqefw9x`, which I had deleted. An email goes out too when SMTP is set.

### 10. Docker container history records end after 10 minutes without output while the container still runs (cosmetic)

Where: `internal/guest/tracer.go:272`. The comment says "no output for 10 minutes and not running", but the code checks only `now.Sub(tr.seen) > 10*time.Minute`. A quiet container shows as `exited` in `history --mode process`, and its next line opens a new record. Found in code; not run live, because it takes 10 min.

## Already fixed in the tree (reproduced on my 10:55 build)

- An Alpha-bound admin key ran `PATCH /orgs/active {"org":"<Beta>"}` and got 200, and alice's active wallet became Beta. This is the same bug as `bugs-r4-sol.md` 3. The current `orgs.go:213` refuses org keys.
- A command run while the sandbox was shared, then pulled after `unshare`, was stored `private=1`: `echo shared-period-1` was hidden from bob. This is the same bug as `bugs-r4-sol.md` 4, fixed by `privateSpans`. Bug 7 above remains.

## Round-3 fixes rechecked (adversarial)

- `snapshot delete --all` on a running sandbox, then stop/resume: held. The deleted base layer stayed unpurged while it was the newest layer. After the next periodic layer, stop and resume kept the files written before and after the delete, and a 50 MB file's md5 matched. The same held when the sandbox was stopped right after the delete, with no new layer, and resumed pinned to the other node. A fork of the running sandbox after `delete --all` had every file.
- Late boot adoption: held. With r4n2 SIGSTOPped, `new --node r4n2 --no-wait` was requeued as `node_offline`. After SIGCONT the server logged "adopting it", the sandbox became `ready` and `exec` worked. The server logs "node could not take the boot; placing it again" twice for one requeue, which is harmless.
- Service-account key bounds: held. An `account.admin`-only 1 h key got 403 `api_key_scope_exceeded` for presets admin/never, admin/30m, ci/30m and read-only/30m. Personal-key isolation from org keys held.
- `PATCH /orgs/active` from a read-only key: 403. For the org-key case, see above.
- Concurrent credit transfers: held. 20 parallel transfers of 10 from 50 gave 5 × 200 and 15 × 402, and the balance ended at exactly 0.
- Org-key boundaries: `GET /orgs/<Beta>` with an Alpha key returns 403 `api_key_org_mismatch`, and `GET /orgs` lists only Alpha plus Personal. A non-member with `X-CrowdVM-Org: <Alpha>` gets 403 `not_org_member` on sandboxes, credits and the org itself.
- Member removal while sandboxes run: guarded in code (`orgs.go:496`). The gap is service accounts (bug 6).
- Org delete with a balance: refused when the balance is above 0. A negative balance is not refused, so an org's debt disappears with it (`orgs.go:365`). This may be intended; I am noting it, not reporting it.
- `exec` signal exit codes: held. SIGKILL gives 137, SIGTERM 143, SIGSEGV 139 and SIGUSR1 138 in both sync and streaming mode. `exit 255` gives 255, `--attach` to a live or finished SIGKILLed process gives 137, `--attach` after `--kill` gives 143 and `--timeout` gives 124.
- Secret-file conflicts: held. `a//b/c`, `./a/b/c` and `/home/user/a/b/c` against `a/b`, and `x/y/z` against `x/y/`, all got 400. For paths that name directories in the image, see bug 8.
- CLI blocked delete: not re-run, because it needs a node removed while it holds a copy.

## New areas: what held

- Dashboard (`web/js`): I crawled all 16 routes as alice (with org Alpha in view) and as mal (no org). I put `<img src=x onerror=window.__xss=1>` into a sandbox name, the org name and an API-key name. Neither payload ran on any page, including titles and breadcrumbs: everything goes through `h()`/`textContent`. Env names are restricted server-side. The server sends no CORS headers, so the custom CSRF header holds. `?next=` cannot redirect off-origin, because `pushState` throws. No console errors or unhandled rejections.
- Public URLs: 40 parallel 5 MB GETs all matched the md5. 100 parallel websockets × 50 messages × 70 KB echoed byte-exact in 26 s, both path-routed and host-routed. Path routing adds the CSP sandbox header, the auth cookie is path-scoped and the session cookie is stripped before the guest. For the websocket gap, see bug 5.
- Desktop tokens: a token works once (second use gets 401 `desktop_token_used`), and a stop ends the stream. For the blacklist, see bug 1.
- AI proxy: the placeholder is swapped in headers and the query only for the custom API. `Cookie` is stripped and the token is bound to sandbox and profile. For the path escape, see bug 3.
- OTel: endpoint test export, headers sealed and only their names returned, metrics and log delivery. For the stall, see bug 4.
- Docker across stop/resume: a `--restart=always` container came back after resume, and named-volume data persisted. The tracer recorded `docker container ticker (busybox)`, marked the old record `lost` after resume and started a new one.
- Detached commands across stop/resume: `--status` and history show `lost` with a `finishedAt`. `--attach` streams the saved output, then exits 1 with `process_lost` and does not hang. `--kill` only signals processes the agent still owns, so a reused PID is safe.
- Leaks: deleting sandboxes and stopping the nodes left no qemu processes. The only leftover sandbox was the bug 6 orphan.

## Not verified

- CI job execution end to end: the GitHub/Gitea runner flow and the `integrations/` adapters. No reachable provider on the LAN.
- The CLI blocked-delete recheck, which needs a node removed.
- Bug 10 live, which needs 10 minutes without output.

**TL;DR:** 10 open bugs. The worst: desktops become unusable after a few seconds (guest wallpaper poll triggers the VNC blacklist); CI connect is an SSRF for any user; the AI proxy's `..` escape exposes ChatGPT history; one tenant can stall OTel for everyone; hide/private leaves open websockets running; deleting a service account orphans its sandboxes. The round-3 fixes held, apart from two org-key and history bugs that are already fixed in the current tree.
