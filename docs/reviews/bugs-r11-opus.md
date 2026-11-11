# Bug hunt r11 (opus): a hostile guest, hosted ports, desktop, round-10 rechecks, 2026-11-11

Setup: `make build` at 19:36 (commit `f2ef8d4`). I copied the binaries to my own data dir (`~/.local/share/crowdvm-r11opus`), so later rebuilds could not change them mid-run. One `crowdvm-server` listened on 127.0.0.1:9960, with `--public-url http://127.0.0.1:9963`, `--public-domain r11.test`, `--log-level debug`, and `CROWDVM_AI_UPSTREAM_ANTHROPIC` set to a fake vendor on 127.0.0.1:9966. Two qemu nodes, `r11n1` and `r11n2`, had their UIs on 9961 and 9962. Each had 6 vCPU, 14 GiB, 60 GiB and at most 4 sandboxes, plus `ubuntu-24.04-crowdvm-v3` and `-minimal-v3` from `crowdvm-node image import`. The default image was the full v3.

Node↔server and guest↔server traffic went through a Python token-bucket TCP proxy on 127.0.0.1:9963 (40 MB/s per direction). Listeners of my own served as probe targets: 127.0.0.1:9968 (loopback), 192.168.178.31:9967 (LAN), and a fake Gitea on 192.168.178.31:9969 for the CI check. Users were `admin` (server admin), `bob` and `carol`, plus an org `acme` (owner admin, bob joined, was removed and joined again). Each user's CLI had its own config dir. Headless Chromium (Playwright's `chrome-headless-shell`, driven over CDP from node, debugging port 9964) checked the browser bugs, with `*.r11.test` mapped to 127.0.0.1.

How I played the hostile guest: inside a sandbox I am root. The node talks to the guest agent through one multiplexed tunnel, so an HTTP proxy in front of port 7070 sees nothing useful. Instead I built a modified guest agent with `go build -overlay` (a patched copy of `internal/guest/guest.go` in my scratch dir; no product file was edited). For any path, it serves `/etc/crowdvm-evil/<path with / as _>` as the answer when that file exists, or runs it when it is executable. I uploaded it into admin's sandbox, stopped `crowdvm-guest.service` and started my binary with the same config via `systemd-run`. Restarting the unit itself does not work, because its `ExecStartPre` reinstalls the agent from the config drive.

Cleanup: every sandbox, saved snapshot and share code is deleted. The server DB showed 0 live sandboxes, 0 saved snapshots, 0 live snapshot rows and 0 shares before I stopped it, and both nodes' `vms.json` were `{}`. The server, both nodes, the proxy, the three receivers, the fake Gitea and Chromium are stopped, and no qemu of mine is left. I started no systemd user units on the host. The sandboxes' own transient units (`gmitm*`, `evilagent`, `wsterm`, `atk`, `ckapp`, `ticker`, `bobpoll`, `flood`) died with the sandboxes. My data dir (binaries, DBs, logs, scratch files, Chromium profile, fake `$HOME`) is deleted.

## Bugs

### 1. A sandbox can run commands on its user's computer through `crowdvm ssh-config` (security)

Severity: security. A hostile guest gets code execution on the laptop of whoever runs `crowdvm ssh-config` on it. With the `Include` line the docs ask for, the code runs on every later `ssh` command, to any host.

Where:
- `internal/server/feat_host_ssh.go:68`: the guest's answer to `POST /v1/sshkey` goes to the client unchanged, `sshUser` included.
- `internal/cli/cmd_access.go:103` takes `res.SSHUser` as is.
- `internal/cli/cmd_access.go:214` writes it into `~/.ssh/crowdvm/<id>.conf` with `fmt.Fprintf(&b, "Host %s\n  HostName %s\n  User %s\n", …)`. Nothing rejects newlines.
- `setKnownHost` (`:116`) writes `hostKey` the same way, so a guest can also add arbitrary `known_hosts` lines (`@cert-authority *`, …) to the CLI's `known_hosts`.

Repro:
1. In a sandbox, as root, answer the sshkey call with `"sshUser": "user\n  ProxyCommand sh -c \"id > …/PWNED-proxycommand; exit 1\"\n  User user"`. I used the replaced agent described in Setup.
2. `HOME=<fake home> crowdvm ssh-config bx_q9bxbajh --print`, then `ssh -F <printed file> bx_q9bxbajh true`.
3. A second payload: `"sshUser": "user\nMatch exec \"id > …/PWNED-matchexec\"\nHost bx_q9bxbajh\n  User user"`. Then `crowdvm ssh-config bx_q9bxbajh` (writes the file), and `ssh -F cfg -G github.com`, where `cfg` holds only `Include <fake home>/.ssh/crowdvm/*.conf`, as the CLI tells users to add at the top of `~/.ssh/config` (`cmd_access.go:201`, `:249`; `cli-reference.md:3165`).

Observed:
- The printed config has the injected `ProxyCommand` before CrowdVM's own. ssh uses the first value, so `ssh … true` ran it: `PWNED-proxycommand` holds `uid=1000(tl) …`.
- With the `Match exec` payload, `ssh -G github.com` ran the guest's command. That is a config dump for an unrelated host, with no connection to the sandbox.

Expected: the server and the CLI accept `sshUser` only as a plain user name (for example `^[a-z_][a-z0-9_.-]{0,31}$`), or ignore it and use `user`. Accept `hostKey` only as a single `type base64` line. The config writer should also refuse any value that contains a newline.

### 2. A public hosted app can drive another user's private hosted port over a websocket (security)

Severity: security. Cross-site websocket hijacking across tenants.

Where: `internal/server/feat_host.go:662` (`serve`). A private port accepts the `_port_auth` cookie on any request, websocket upgrades included, with no check of `Origin`. The cookie is `SameSite=Lax` (`authCookie`, `:817`). Every hosted app lives under the same public domain, so all of them are the same site, and the browser sends a Lax cookie on a websocket handshake from a sibling host. The server strips the cookie before proxying (good), but it has already authenticated the request.

Repro:
1. bob: in his sandbox, a small websocket "terminal" on 3000 that runs what it receives (stands in for ttyd, code-server or Jupyter), then `crowdvm host bx_3hvgry6n 3000` (private, token URL).
2. admin: an `index.html` on 3001 with `new WebSocket("ws://bx-3hvgry6n-3000.r11.test:9960/")`, `send("id -un; cat ~/bob-secret.txt")` and the answer written into `document.title`. Then `crowdvm host bx_q9bxbajh 3001 --public`.
3. In Chromium: bob opens his private URL with `?_token=` once (sets the cookie), then visits admin's public URL.

Observed: admin's page title became `STOLEN:user BOB-SECRET-R11-WS`. bob's app logged the handshake with `origin=http://bx-q9bxbajh-3001.r11.test:9963`.

Expected: for protected ports, refuse a websocket upgrade (and ideally any non-GET request) authenticated by cookie when `Origin` is present and is not the port's own host. Otherwise, say plainly in `public-urls.md` that the private-URL cookie does not protect against other apps on the same public domain.

### 3. The size and auto-stop policies are bypassed by writing `Type` instead of `type` (security)

Severity: security (limit bypass). Round-10 sonnet #4 is fixed for the lowercase key only.

Where: `internal/server/acct_billing.go:856` (`req["type"]`), `:875` (`req["ttlSeconds"]`), `:826` (`req["org"]`) and the `from` lookup in `policyGuard`. These read a `map[string]any`, so the lookups are case-sensitive. The handlers (`sandboxes.go:767`, the fork handler in `lifecycle.go`) decode into structs with `encoding/json`, which matches keys case-insensitively. Also `OptInt.UnmarshalJSON` (`util.go:186`) never clears `Null`, so when both keys arrive, an explicit null stays.

Repro (carol, admin policy set with `PUT /admin/policies/<carol>`):
- `allowedSizes: ["default"]`: `POST /sandboxes {"type":"small"}` gives `machine_class_plan_required`. `{"Type":"small"}` gives 202 and a `small` sandbox (`bx_mgeh5e3u`).
- `allowedSizes: ["small"]`: `POST /sandboxes/bx_hpuzqkkj/fork {"type":"default"}` gives `machine_class_plan_required`. `{"Type":"default"}` gives 202 and a `default` fork (`bx_x25z83mp`).
- `allowedSizes: ["small","default"]`: `{"Type":"large"}` passes the policy and fails only on capacity (`size_unavailable`). `{"type":"large"}` gives `machine_class_plan_required`.
- `maxTtlSeconds: 600`: `{"type":"small","ttlSeconds":null}` gives `auto_stop_required`. `{"type":"small","TtlSeconds":null}` gives 202, and the row has `ttl_seconds` and `archive_after` NULL: no auto-stop at all.

Expected: the guard reads the same decoded request as the handler (decode once into the struct, or reject keys that differ from the canonical ones only by case). The same applies to `org`/`teamId` (which wallet's policy applies) and `from` (the inherited size of a deploy).

### 4. A guest's history export is decoded and stored without limits (leak)

Severity: leak; server memory and disk. One sandbox can fill the server's disk within minutes and push its memory toward OOM.

Where:
- `internal/server/feat_env_guest.go:259`: `json.NewDecoder(resp.Body).Decode(out)` with no size limit. `maxBytes=4194304` in the query (`feat_history.go:149`) is only a request to the guest.
- `internal/server/feat_history.go:238`: every record is upserted with its full `command` text, twice (the `command` column and the `record` JSON), with no quota and no size cap.
- `feat_history.go:268`: samples are stored the same way.
- `feat_history.go:255`: the output quota is charged only for a new `(command, stream, offset)`. Replacing an existing chunk with a bigger one is free.

Repro (the replaced agent answers `GET /v1/internal/history/export`):
1. Each pull, one record under a new command id with a 64 MiB `command`. In 20 s the DB went from 5.6 MB to 1.6 GB (10 records, 640 MiB in each of the two columns). `output_usage` for admin did not change (20 500 bytes).
2. One pull with a 256 MiB `command` under a fixed id: server RSS peaked at 3.76 GB (`VmHWM`), from 150 MB.
3. A chunk of 1 MiB at offset 0 of `cmd_chunk_x`, then the same key with 40 MiB: `output_usage` grew by 1 MiB, and `command_logs` holds 41 943 040 bytes.

Expected:
- Read the export through `io.LimitReader` (a little over `maxBytes`).
- Cap `command` length and the record count per pull, and samples per pull and per sandbox.
- Charge a replaced chunk for its growth, or keep the first copy of a chunk.

### 5. One guest's event flood makes the node drop other sandboxes' events, so their setup status sticks at `pending` (wrong-result)

Severity: wrong-result, cross-tenant.

Where: `internal/node/node.go:582` (`emitEvent`). All VMs share one queue of `maxQueuedEvents = 10000` (`:37`). When it is full, the oldest events are dropped, whichever sandbox they belong to. `followGuestEvents` (`internal/node/vms.go:468`, `:508`) forwards every line a guest writes on its event stream, with no per-sandbox rate or share. Guest events such as `setup.status`, `prompt.event`, `file.skipped` and `boot.previous` exist only as events. Nothing re-reads them later.

Repro:
1. Baseline: bob `crowdvm new --type small --node r11n1 --setup-file setup.sh`. The DB shows `setup_status=done`.
2. admin's sandbox on the same node answers `/v1/internal/events/stream` with an endless stream of `{"type":"noise.flood",…}` lines. The node process went to 125 % CPU.
3. bob runs the same create on r11n1.

Observed: the script ran (`~/setup-ran.txt` exists), but `setupStatus` is `pending` in `crowdvm ls` and in the DB. It stayed `pending` after I stopped the flood.

Expected: a per-sandbox cap on queued guest events (drop that sandbox's own events, or close its stream), and a queue where node and lifecycle events cannot be pushed out by guest events. The server could also read the setup status once from `GET /v1/internal/setup-status` when the event is missing.

### 6. A foreign deploy still carries OpenCode's conversation database (security)

Severity: security. Round-10 opus #5 is fixed for Claude Code, pi and prime, not for OpenCode.

Where: `internal/server/lifecycle_secure.go:392` lists `.local/share/opencode/storage`, `log` and `snapshot`. The OpenCode in the full image stores sessions and messages in `~/.local/share/opencode/opencode.db` (SQLite; tables `session`, `message`, `part`, …).

Repro:
1. bob (fake vendor behind the proxy): `crowdvm prompt bx_gexj3m97 --provider X --new 'say hi R11MARK-X'` for claude, pi, opencode and prime. claude finished. The others failed at the vendor, but still wrote their session files.
2. bob: `crowdvm snapshot bx_gexj3m97 bobsnap`, `crowdvm snapshot share bobsnap`.
3. carol: `crowdvm new --from bsh_…`.

Observed:
- On carol's disk, `~/.claude/projects`, `~/.pi/agent/sessions` and `~/.prime/agent/sessions` are gone, so those paths are right.
- `~/.local/share/opencode/opencode.db` is there, and `strings` shows `…\n\nsay hi R11MARK-opencode`.
- `~/.prime/agent/session-artifacts/<session id>/` also survives (small, only `semantic-edges.jsonl`).

Also, by `strings` on the Codex binary in the image: Codex keeps `state_5.sqlite`, `logs_2.sqlite`, `history.sqlite` and `session_index.jsonl` under `~/.codex` (or `CODEX_SQLITE_HOME`). The list names only `sessions`, `archived_sessions`, `log` and `history.jsonl`. Not run live: there is no OpenAI upstream in this setup.

Expected: add `.local/share/opencode/opencode.db` (with `-wal`/`-shm`), `.prime/agent/session-artifacts`, and Codex's SQLite stores and `session_index.jsonl`. Better still, wipe each harness's data directory except its config and skills.

### 7. A guest that stalls sync and freeze delays every other sandbox's snapshots on its node (data-loss)

Severity: data-loss risk, cross-tenant. The recovery point of other users' sandboxes on the node gets older.

Where: `internal/node/vms.go:709` (`snapshotLoop`) snapshots the due VMs one after another. For each one, `diskChanged` calls `guestSync` (`:643`, 30 s timeout). The qemu driver then calls `/v1/internal/freeze` and, after that fails, `/v1/internal/sync` (`internal/node/vm/qemu/qemu.go:561`, `:563`), with a 1-minute timeout each. A guest that never answers holds the loop for about 150 s every time it is due.

Repro: bob's sandbox on r11n1 writes and syncs a line every 5 s. Its periodic snapshots came every ~70 s (`19:10:35, 19:11:45, 19:12:57, 19:14:06, 19:15:15`). At 19:15:11, admin's sandbox on the same node started answering `sync` and `freeze` with a `sleep 400`.

Observed: bob's snapshots came at `19:17:46, 19:18:55, 19:21:27, 19:22:36`, so the gaps alternate between ~151 s and ~69 s. One stalling guest about doubles the gap every other cycle. With up to 4 sandboxes per node, three stalling sandboxes would hold the loop for about 7.5 min per round.

Expected: snapshot VMs concurrently (or with a per-VM deadline that does not block the others), use short guest timeouts, and back off a guest that timed out (for example, snapshot it without sync/freeze, or skip it for a while).

### 8. A host-routed app can still set cookies for the whole public domain, by putting a space before `=` (security)

Severity: security, low. Cookie tossing into every other app on the public domain, and into the dashboard when it runs there. Round-10 opus #4 is fixed for the server's own cookie names and for `NEL`, `Report-To`, `Clear-Site-Data` and `Refresh`. The Domain filter can be bypassed.

Where: `internal/server/feat_host.go:785` (`hostScopeCookies`) keeps a line when Go's `http.ParseSetCookie` finds no `Domain`. Go splits the attribute at `=` without trimming the name, so `Domain =r11.test` becomes the unknown attribute `"domain "` and is ignored. Browsers trim it and apply the domain. The original line is kept as sent.

Repro: admin's app on 3002 sends `spaced=1; Domain =r11.test; Path=/`, `spaced2=1; Domain = .r11.test; Path=/`, `tabbed=1; Domain\t=r11.test; Path=/`, plus `plain=1; Domain=r11.test` and `hostonly=1`. It is hosted public and opened in Chromium.

Observed:
- `curl` shows `plain` dropped and the three spaced variants passed unchanged.
- Chromium's jar holds `spaced`, `spaced2` and `tabbed` for `.r11.test`.
- bob's private app on `bx-3hvgry6n-3000.r11.test` then received `Cookie: spaced=1; spaced2=1; tabbed=1`.
- `crowdvm_session` and `_port_auth` stay blocked (the name check).

Expected: rebuild each kept cookie from the parsed value without `Domain` (as `pathScopeCookies` does), or drop any line whose raw text has a `domain` attribute after trimming whitespace.

### 9. All guests on a node share the node's IP rate-limit bucket, so one guest locks the others out of the API (security, low)

Severity: security, low. A cross-tenant denial of service.

Where: `internal/server/router.go:139` limits `/api/`, `/auth/`, … per `clientIP` (`util.go:82`), 10 000 requests per 60 s by default. A guest reaches the server only through its node's forward (`netstack` `GatewayForwards`), so every sandbox on a node, and the node owner's own CLI on that machine, have one address.

Repro: bob's sandbox polled `GET /api/v1/sandboxes/<own id>` with its in-sandbox key every 2 s (45 calls). Meanwhile, admin's sandbox on the same node sent 30 000 unauthenticated `GET /api/v1/server`.

Observed: admin's flood got 19 966 × 401 and 10 031 × 429. bob's guest got 30 × 200 and 15 × 429. My host CLI was locked out too during the window. In this setup everything comes from 127.0.0.1. On a real cluster it is the node's IP.

Expected: count guest traffic per sandbox (the node can tag the forwarded connection, or the server can key the limit on the guest key or token), and count unauthenticated requests from a node's address separately from authenticated ones.

### 10. `--no-monitoring`: concurrent large commands still fail with ENOSPC, and one lost its warning (wrong-result)

Severity: wrong-result. Round-10 opus #6 is partly fixed: short probes now work.

Where: `internal/guest/commands.go:158` (`capOutput`). A command's stdout and stderr are files on the guest's RAM disk. The freeze below 10 % free and the 1 % "mark truncated" check run every 100 ms. Eight writers fill the last 78 MB much faster than that, so their own writes fail.

Repro (`crowdvm new --type small --no-monitoring`, 784 MB `/run`): run the r10 command 8 times at once, `crowdvm exec --timeout 200 ID "bash -c 'yes N | head -c 200000000 2>/tmp/err.N; echo \${PIPESTATUS[*]} > /tmp/rc.N; sleep 20'"`, plus 5 `echo probeN` during the run. I ran it twice.

Observed:
- The probes all printed their text, so that part is fixed.
- In both runs, all 8 commands had `/tmp/rc.N` = `141 1` and `head: error writing 'standard output': No space left on device`. `/run` peaked at 96 %.
- In the first run, `c6` ended at 89 169 920 bytes with no truncation warning. The other 7 had the warning.
- Between runs, `/run` stayed at 576 MB of 784 MB for several minutes after every follower had read its exit frame.

Expected: a command must not see ENOSPC because of the sandbox's own output capture. Write to a pipe and drop past the share, or reserve space per running command. When it happens anyway, always mark the record truncated.

### 11. Guests can reach Azure's WireServer (168.63.129.16) through the node (security, low)

Severity: security, low. Only matters for nodes on Azure.

Where: `internal/node/vm/netstack/policy.go:68`. `otherNets` blocks `169.254.0.0/16` and multicast. 168.63.129.16 is a public address, so it counts as "internet". Round-10 sonnet #12 added it to the server's outbound guard (`outbound_guard.go`), not to the node's guest policy. On Azure, the WireServer hands out a VM's goal state and extension settings (protected settings have been shown retrievable by requesters who supply their own certificate).

Repro: from a guest, `bash -c "exec 3<>/dev/tcp/168.63.129.16/80"` and the same for port 32526.

Observed: `ss` on the host shows `crowdvm-node` in `SYN-SENT` to `168.63.129.16:80`. The node logged no "blocked" line. For comparison, `169.254.169.254` and `100.100.100.200` were refused at once and logged as blocked.

Expected: add 168.63.129.16/32 (and the IMDS-style addresses of other providers that are not link-local) to the node's always-blocked list, and say so in `security.md`'s network table.

### 12. A custom AI API base URL can still reach LAN hosts through a DNS name (security, low)

Severity: security, low. This is what round-10 opus #7 left open.

Where: `internal/server/feat_ai.go:1008` (`aiValidBaseURL`) refuses LAN hosts only by their text (IP literals, `.lan`, `.local`, …). `aiCustomTransport` (`feat_ai_proxy.go:63`) uses `guardedDial`, which refuses loopback, link-local and metadata but allows private addresses. The flag help says LAN hosts need `-ai-custom-api-allow-private`.

Repro: bob's profile `p2` gets custom APIs with `baseUrl` `https://127.0.0.1.nip.io`, `https://192.168.178.31.nip.io`, `https://169.254.169.254.nip.io`, `https://0.0.0.0.nip.io` and `https://localtest.me`. All are accepted. Then, in a sandbox on that profile, `curl http://10.0.2.2:9963/ai-proxy/custom/NAME/x -H "Authorization: Bearer $ANTHROPIC_API_KEY"` for each.

Observed:
- The guest gets 502 "The provider did not answer." for each, with no dial details, so that part of the fix holds.
- The server log shows loopback, `::1`, `0.0.0.0` and metadata refused by the guard.
- For the LAN one: `dial tcp 192.168.178.31:443: connect: connection refused`. The server made a TCP connection to a LAN host for a sandbox, with the guest's request and the custom secret ready to send.

Expected: without the allow-private flag, refuse private addresses for custom APIs at dial time too (a stricter guard variant for this transport).

## Round-10 fixes rechecked live

| r10 item | Result |
|---|---|
| Org saved snapshot file routes, saver only (opus 1) | Holds. As bob (member of acme), with the org header: `GET /snapshots/<orgsnap id>/tree` and `/files?path=private.txt` gave 404; `/sandboxes/<private org sandbox>/snapshots` and `/latest` gave 403 `sandbox_private`; `GET /snapshots` lists only bob's own. |
| Removed member, org environment and AI profile (opus 2) | Holds for new sandboxes. bob's personal env pointing at the org's `orgprof`: after removal, a new sandbox has no `ANTHROPIC_API_KEY`. New ground: the access lists too. `ai profile access orgprof --only admin@…` gave no key in bob's new sandbox; `--everyone` plus stop/resume gave the key; `--only` again plus `env upgrade` removed it live from exec and login shells. Not rerun: repro B (org env on resume after removal). |
| Live `env upgrade` and the proxy placeholder (opus 3) | Holds. Placeholder `cvmai_30q97k0k…`: the vendor got `sk-ant-BOBKEY-R11`; after `--credential-profile p2` plus upgrade, the same placeholder reached the vendor with `sk-ant-BOBP2-R11`; after `--agents-credentials false` plus upgrade, 401 and the env vars were gone. `ai proxy off` plus upgrade of a proxied sandbox kept the placeholder, and no real key appeared on disk. Side note: a live upgrade that turns credentials on for a sandbox booted without them gives it nothing until the next boot. Not counted. |
| Host-routed cookies, NEL/Report-To (opus 4) | Partly. `NEL`, `Report-To` and `Clear-Site-Data` are dropped, `Domain=r11.test` is dropped, and the server's cookie names are dropped. `Domain =r11.test` gets through: bug 8. Path-routed cookies are rebuilt and scoped to `/p/<id>/<port>/` correctly. |
| Harness transcripts on foreign deploy (opus 5) | Partly. Claude Code (`~/.claude/projects`), pi (`~/.pi/agent/sessions/--home-user--/…jsonl`) and prime (`~/.prime/agent/sessions/…jsonl`) are the real paths and were removed. OpenCode writes `~/.local/share/opencode/opencode.db`, which survives: bug 6. kimi and vibe were not configured (`provider_not_configured`); from their bundles, `~/.kimi-code/…/sessions` and vibe's session logs look right but are unconfirmed. |
| `--no-monitoring`, concurrent commands and probes (opus 6) | Partly. The probes work now. The commands still fail with ENOSPC, and once the warning was missing: bug 10. |
| Custom AI API base URL to loopback/LAN/metadata (opus 7) | Partly. Loopback, `::1`, `0.0.0.0` and metadata are refused at dial time, and the guest no longer sees dial errors. LAN is still dialed: bug 12. |
| Parallel creates vs `maxActive` and start limits (sonnet 3) | Holds. `maxActive 2` (carol had 1): 16 parallel creates gave 1 × 202 and 15 × 429. `startsPerMinute 3`: 16 more gave 2 accepted (3 in the minute). `maxActive 2` and 10 parallel forks: 1 accepted. |
| Forks and deploys vs allowed sizes (sonnet 4) | Holds for the lowercase key: a fork with `{"type":"default"}` under `["small"]` and a create with `{"type":"large"}` were refused. Bypassed with `Type`: bug 3. The inherited size (fork or deploy with no type) was checked by code read only. |
| CI connect of an organization (sonnet 7) | Holds. A fake Gitea on the LAN (`/user` = mallory, `/orgs/acme` exists, permissions `is_admin:false`): `POST /ci/connect {provider:gitea, account:acme}` gave 403 `account_mismatch`, after `GET /api/v1/users/mallory/orgs/acme/permissions` with the given token. |

## New ground: what held

- **Guest network (netstack), probed from a guest:**
  - Refused: `10.0.2.2` on every port except the server's (9960, 9961, 22, 7070); `127.0.0.1:*` (the guest's own loopback, so the node UI and my loopback listener are out of reach); `0.0.0.0`; all of the host's addresses (`192.168.178.31`, `192.168.122.1`, Tailscale `100.78.214.33`, VPN `192.168.113.32`); `169.254.169.254`; `100.100.100.200`; other `10.0.2.x`; multicast and broadcast.
  - Reachable: the server forward `10.0.2.2:9963`, DNS on `10.0.2.3:53`, and the internet.
  - Each refusal was logged once per minute with its reason. The exception is Azure's address: bug 11.
- **In-sandbox key (`CROWDVM_API_KEY`) and sandbox token (`CROWDVM_TOKEN`)** of admin (a server admin), tried against admin's other sandbox, its saved snapshot and bob's sandbox:
  - The key gets 403 `api_key_sandbox_forbidden` on the other sandbox, 404 for the saved snapshot by name, its shares, its tree, and for saving it.
  - Lists (`/sandboxes`, `/snapshots`, `/saved-snapshots`, `/named-snapshots`) show only its own sandbox.
  - 403 on `/me`, environments, credential profiles, `/nodes` and join tokens, and for creating keys (normal or scoped).
  - The token can only read its own sandbox. Admin rights do not reach keys without `*` (`IsAdmin`).
- **Guest identity:** the node tags guest events with its own VM id, and the server drops guest events from a node that does not run the sandbox. A guest cannot speak for another sandbox. The history pull is keyed by the sandbox the server asked.
- **Desktop stream:**
  - A token for bob's sandbox gave an RFB handshake there. The same token on carol's sandbox was refused, and a second use was refused (401 `desktop_token_used`).
  - The signature binds sandbox, kind and guest token, so a restart ends it.
  - The tunnel route uses `websocket.Accept` with its default Origin check.
- **Hosted ports:**
  - The server strips `_port_auth` and `crowdvm_session` before proxying.
  - `NEL`, `Report-To` and `Clear-Site-Data` are dropped (host and path routing).
  - Path-routed cookies get the prefix path, and responses get the CSP sandbox.
- **Guest content on the API origin:** `ProxyToGuest` applies `guardGuestContent` (no cookies, CSP sandbox, active types as attachments).
- **Recursive `scp --api` download:** extracts through `extractTar`, which already treats the archive as hostile (code read).

## Not verified

- Bug 4 up to an actual OOM kill. I stopped at a 256 MiB record (3.76 GB RSS) so as not to take the shared machine down.
- Bug 7 with more than one stalling sandbox per node.
- Bug 9 with distinct client IPs. Here users, nodes and guests all came from 127.0.0.1. On a real cluster the bucket is the node's IP, which also holds the node owner's own CLI if they run it there.
- Bug 11 against a real Azure WireServer (no Azure host). Only the dial was observed.
- Codex, kimi and vibe transcripts live (no OpenAI, Moonshot or Mistral upstream). Codex's SQLite paths come from `strings` on its binary.
- Round-10 opus 2 repro B (org environment on resume after removal), and deploys from a saved snapshot under the size policy.
- DNS through `10.0.2.3`: it relays to the host's resolvers, so a guest can resolve names only the host's LAN DNS knows. I did not query LAN names, as the rules ask me not to contact other hosts.

## Mistakes on my side

- I stopped my first throttling proxy with `pkill -f '[t]bproxy.py 9963'` inside a command line that also contained `tbproxy.py 9963`, which killed my own shell. Later I used PID files. Inside a guest, `pkill -f "sleep 400"` killed my own exec the same way.
- My first hostile-guest attempt put a Python HTTP proxy in front of port 7070 with an iptables redirect. The guest's ufw dropped port 7071, and later the node's multiplexed tunnel bypassed the per-request parsing. Twice that left admin's sandbox unreachable, and I recovered it with `stop --force` and `resume`, which lost its unsnapshotted state (only my test files). Then I switched to the overlay-built agent.
- Twice I assumed `systemctl restart crowdvm-guest` would run my binary. Its `ExecStartPre` (`guest-prepare`) reinstalls the agent from the config drive.
- The first upgrade test failed because a resume keeps the pinned environment version. I needed `env upgrade` before stop/resume. Not a product bug.

## Fixes

11 of 12 fixed. Tests: `internal/server/fix_r11_test.go` (1, 2, 3, 8), `internal/server/fix_r11_opus_test.go` (4, 6, 9, 12), `internal/node/fix_r11_test.go` (5, 7), `internal/node/vm/netstack/fix_r11_test.go` (11), `internal/api/ssh_test.go` (1).

1. **ssh-config injection** (`internal/api/ssh.go`, `feat_host_ssh.go`, `internal/cli/cmd_access.go`): `sshUser` must be a plain Unix name (else `user`), `hostKey` one `type base64` line; checked by the server and again by the CLI, which refuses an invalid host key.
2. **Cross-site websockets on private ports** (`feat_host.go` `crossOriginWrite`): with cookie auth, a websocket upgrade or a non-GET/HEAD/OPTIONS request whose `Origin` is another host gets 403.
3. **Policy field case** (`acct_billing.go` `policyFields`): `type`, `ttlSeconds`, `org`, `teamId`, `from` must be spelled exactly (400 `invalid_field`); `OptInt`, `OptFloat`, `OptBool` reset on every decode, so a repeated key cannot keep an earlier null.
4. **History export** (`feat_history.go`, `feat_env_guest.go`): the export is read into at most 8 MiB (else refused and backed off); `command` is capped at 64 KiB, records and samples per pull are capped, samples per sandbox are pruned; a replaced chunk is charged for its growth; generic guest JSON answers are limited to 16 MiB.
5. **Node event queue** (`internal/node/node.go`): a guest's events are dropped past 500 queued for its sandbox or 8000 for all guests; node and lifecycle events are never pushed out by them.
6. **Scrub list** (`lifecycle_secure.go`, `internal/guest/apply.go`): adds OpenCode's `opencode.db` (and `-wal`/`-shm`), `.prime/agent/session-artifacts`, Codex's SQLite stores (as `state_*`, `logs_*`, `history*` patterns and current names) and `session_index.jsonl`; the guest scrub expands `*?[` patterns in `Files` (within one folder).
7. **Snapshot loop** (`internal/node/vms.go`, `vm/vm.go`, `qemu/qemu.go`): due VMs are snapshotted concurrently (at most 3); a guest whose sync fails is backed off for 10 minutes and snapshotted crash-consistent (`vm.WithoutQuiesce`).
8. **`Domain =x` cookies** (`feat_host.go` `hostScopeCookies`): kept cookies are rebuilt from Go's parse, which drops the unknown spaced attribute.
9. **Shared IP bucket** (`router.go`): a request that authenticates counts against its user; one that does not keeps the IP bucket. The principal is reused by `wrap`.
11. **WireServer via the node** (`internal/node/vm/netstack/policy.go`, `security.md`): `168.63.129.16/32` is always blocked.
12. **Custom AI API to LAN** (`outbound_guard.go` `withNoPrivate`, `feat_ai_proxy.go`): without the allow-private opt-in, private, CGNAT and ULA addresses are refused at dial time too.

Not fixed: **10** (`--no-monitoring`, concurrent commands writing GB/s still see ENOSPC on the RAM disk). The 100 ms watcher cannot react in time; only capturing output through a pipe (dropping past a share) prevents it, which is a redesign of the guest's output capture and costs detached commands their survival of a guest-agent restart. Left as a known limitation.
