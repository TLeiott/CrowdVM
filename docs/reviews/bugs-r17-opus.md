# Bug hunt r17 (opus): round-16 rechecks, organizations, agents, webhooks and OTel, 2026-11-12

Setup: commit `9ffe0fe`, built with `make build` in my own worktree (`/home/tl/dev/CrowdVM-r17opus`, detached at `9ffe0fe`). Binaries and all state lived in `~/.local/share/crowdvm-r17opus`. One `crowdvm-server` listened on :9960 with `--public-url http://127.0.0.1:9963`, `--public-domain r17.test`, `--log-level debug`, no start limits, `--default-image ubuntu-24.04-crowdvm-v4` (the full image), `--egress-allow 192.168.178.31:9967,192.168.178.31:9968`, and `CROWDVM_AI_UPSTREAM_ANTHROPIC` pointing at a fake Anthropic vendor (as the e2e suite does). Two qemu nodes, `r17n1` and `r17n2` (UI on :9961 and :9962), each with 8 vCPU, 20 GiB, 30 GiB of disk (raised to 60 GiB midway) ×4 overcommit, at most 5 sandboxes. Each ran as a transient systemd user unit with the real unit's properties (`KillMode=process`, `NotifyAccess=main`, `FileDescriptorStoreMax=1024`, `FileDescriptorStorePreserve=yes`, `CROWDVM_FDSTORE=1`, `Restart=always`). Images came from `~/.local/share/crowdvm-dev/images/`: `ubuntu-24.04-crowdvm-v4` and `-minimal-v4`, imported on `r17n1`, pushed and pulled to `r17n2`. Node↔server and guest↔server traffic went through a Python token-bucket proxy on :9963 at 40 MB/s per direction. Users: `admin` (owns the nodes), `bob`, `carol`, `dave`, each with their own `CROWDVM_CONFIG_DIR` and scratch `HOME`. Other tools on my ports: a webhook receiver (:9965), an OTLP/HTTP receiver (:9966), the fake vendor plus a file server (:9967), a TCP sink that accepts and never reads (:9968, `SO_RCVBUF 4096`), headless Chromium (Playwright's build, CDP on :9964) as a browser without a CrowdVM session, and one guest sshd published as a direct port on :9969. AI calls: only the fake vendor and the e2e suite's free OpenCode model (`opencode/beam-free`), three turns in all.

Cleanup: see the last section.

## Bugs

### 1. 256 stalled egress flows cut a running sandbox off from the network until it reboots (stuck)

Severity: stuck. Round 16's fix ends egress flows with their channel session, and stop and reboot now free them. Inside a live session a flow still never ends while its destination accepts and does not read: the stream→socket copy is blocked in `Write`, so it never sees the guest close its side, and nothing times it out. Each such flow keeps its slot in the per-session cap of 256. Once a program has parked 256 of them, the sandbox gets 429 for every new egress connection, also after that program exited. To the guest that looks like "connection reset" on every outbound TCP connection, the internet included. It lasts until the sandbox stops or reboots. Any remote host that stalls uploads (an overloaded API, a tarpit) can do this to a sandbox.

Where:
- `internal/server/guest_egress.go:244` counts the flow against `maxEgressFlows` for as long as `mux.Join` (`:270`) runs.
- `internal/mux/mux.go:223–257` (`Join`): the idle timer starts only after a half-close. A copy blocked in `Write` never gets to the half-close, and there is no write deadline.

Repro:
1. Server with `--egress-allow 192.168.178.31:9968`, plus a sink on :9968 that accepts and never reads.
2. In `h1`: `python3 hold.py 40 300` (opens 300 connections to `192.168.178.31:9968`, writes 64 KiB chunks for 40 s, exits).
3. After it exits: `curl http://192.168.178.31:9967/x` and `curl https://example.com` in `h1`.
4. `ss -tnp state established 'dport = :9968'` for the server's pid, then `crowdvm stop h1`.

Observed:
- Step 2: `connected 300`, `sent 893876608`, program exited.
- Step 3, 2.5 minutes after the program exited: both `curl`s fail (`(56) Recv failure: Connection reset by peer`, `(35) Send failure: Connection reset by peer`, http code `000`).
- Step 4: the server held exactly 256 flows with 458 752 000 bytes in their send queues, and RSS had gone 127 MB → 478 MB. `crowdvm stop` dropped them to 0 (the round-16 fix). After a resume, or after `sudo systemctl reboot` in the guest, egress worked again (200).

Expected: a flow whose guest side has closed, or whose destination has not taken a byte for a while, ends. For example, a write deadline on the egress socket that `touch` pushes back, or closing the egress socket when the guest stream ends even if the other copy is blocked. Then a stalled destination can delay but not disable a sandbox's network.

### 2. A member removed from an organization keeps their SSH key in teammates' shared sandboxes, and their open sessions keep running (security)

Severity: security. Making a sandbox private (`crowdvm unshare`) deletes teammates' keys from `~/.ssh/authorized_keys` (ORG-012). Removing a member does not. Their key stays in every shared sandbox they used, their open `ssh` sessions continue, and the `sandbox_ssh_keys` rows stay. The API refuses them at once (`crowdvm ssh` → 404), but any path to sshd that is not the API still lets them in: a direct port (`crowdvm direct-ports`), a port the owner hosts, or another sandbox on a shared network. The key also survives when the sandbox later moves to its creator's personal billing (the creator leaves or the org is deleted), so a person who is no longer in the org at all can still log in.

Where: `internal/server/orgs.go:546` (`handleRemoveMember`, `:579` deletes the membership) does not revoke the member's keys. `internal/server/acct_sshkeys.go:90` runs `revokeTeammateKeys` only on `unshared`.

Repro:
1. bob owns `acme`; carol (member) and dave (admin) join. carol: `crowdvm --org acme new --name cs1`, `crowdvm share cs1`, stop and resume it (it held her logins).
2. dave: `crowdvm ssh cs1 -- 'echo dave-in'`, then a long-lived `crowdvm ssh cs1 -- 'while :; do date >> /tmp/dave-alive; sleep 1; done'`.
3. bob: `DELETE /api/v1/orgs/acme/members/<dave>` → `org.member_removed`.
4. carol: `tail -1 /tmp/dave-alive; date; cat ~/.ssh/authorized_keys`.
5. Owner publishes a direct port (node with `direct-host 127.0.0.1`, a guest sshd on 9969, `crowdvm direct-ports cs1 9969`). dave: `ssh -i <dave's crowdvm key> -p 9969 user@127.0.0.1`.

Observed: step 4: dave's loop was still writing (`13:54:24` against `13:54:25`), and his `ssh-ed25519 AAAAC3…IINI` line was still in `authorized_keys`. `crowdvm ssh` and `crowdvm tunnel` for dave → 404. Step 5: `removed-member-dave-is-in`, `cs1`. After carol left the org (cs1 is now her personal sandbox), the key was still there.

Expected: removing a member (and a member leaving) runs the same revocation as `unshare` for every org sandbox they sent a key to, at once when running, else on the next ready. Their open guest sessions (ssh, tunnels, exec streams) end.

### 3. Removing a service account through the members endpoint orphans its sandboxes: no one in the org can see or delete them (leak)

Severity: leak. `DELETE /orgs/{org}/service-accounts/{id}` refuses while the account has sandboxes ("so deleting it cannot orphan disks or snapshots"). `DELETE /orgs/{org}/members/{id}` accepts a service account too, and only checks for running sandboxes. Its stopped sandboxes then go to the service account's personal billing (`org_id=NULL`), and its org key is revoked. From then on: the account has no working key, the org owner and admins get 404 for the sandbox (it is no longer in the org), the service-account delete answers 404 (it is no longer a member), and even the server admin gets 404 on `DELETE /sandboxes/{id}`, because the orphan rule needs `org_id` set or the owner disabled. The disk and snapshots stay for good. Only an admin who suspends or merges the account by hand can get at them.

Where: `internal/server/orgs.go:546` (`handleRemoveMember`, no `kind='service'` check), `internal/server/sandboxes.go:202` (`orphan` does not cover a service account outside any org).

Repro:
1. bob: `POST /orgs/acme/service-accounts {"name":"ci3","preset":"ci"}`. With its key: `POST /sandboxes {"type":"small"}` → `bx_m4hzu7t5`, write 200 MB.
2. bob: `crowdvm stop bx_m4hzu7t5`, then `DELETE /orgs/acme/members/<svc id>`.
3. `GET` and `DELETE /sandboxes/bx_m4hzu7t5` as bob (org owner) and as admin, `DELETE /orgs/acme/service-accounts/<svc id>`, `GET /sandboxes/bx_m4hzu7t5` with the service account's key.

Observed: step 2 `org.member_removed`. Step 3: 404 `not_found` for bob and for admin (GET and DELETE), 404 `Service account not found.`, 401 for the account's key. The DB still has `bx_m4hzu7t5|usr_28ns6wqk8mns1xes||archived` and two snapshots (1 339 490 304 and 8 716 288 bytes). The account stays enabled, with `activeOrg` still set to the org.

Expected: the members endpoint refuses service accounts (`use DELETE …/service-accounts`), or applies that endpoint's rule (no sandboxes left). And a sandbox whose owner is a service account outside any org counts as orphaned, so an admin can delete it.

### 4. `crowdvm credits transfer` ignores `--org`, `CROWDVM_ORG` and the active org, and takes the credits from the personal wallet (wrong-result)

Severity: wrong-result. `credits transfer --help` says `--from` defaults to "the one in scope". The server reads only the body's `from`, and the CLI sends it empty, which means personal. `crowdvm --org acme credits transfer --to dave --amount 1` therefore moves 1 credit from the caller's personal wallet to dave, not from acme's. The same happens with `CROWDVM_ORG=acme` and after `crowdvm org switch acme`. `--to bob` from bob's own scope answers `same_wallet`, which is how an owner emptying the org before `org delete` (it refuses with `org_has_balance`) finds out. Someone moving org money to a teammate pays from their own wallet without noticing.

Where: `internal/server/credits.go:274` (`resolveOrgRef(c.P.User, req.From)`, not `c.Scope`), `internal/cli/cmd_auth.go:683` (the help text).

Repro: bob owns `acme` (org wallet 99.839643, bob 509.591202). `crowdvm --org acme credits transfer --to dave --amount 1`, then `CROWDVM_ORG=acme crowdvm credits transfer --to dave --amount 1`, then `crowdvm org switch acme; crowdvm credits transfer --to dave --amount 1`.

Observed: after each, the org wallet still showed 99.839643, and bob's personal wallet went 509.591202 → 508.591202 → 507.591202 → 506.587035. `crowdvm --org acme credits transfer --to bob --amount 99.839643` → 400 `same_wallet`. Only `--from acme` moved org credits.

Expected: with no `from`, the server uses the request's scope (`c.Scope("")`: header, active org), as for every other wallet operation, and the CLI sends `--org` as the source. Or the help says that `--from` defaults to personal.

### 5. Concurrent ssh connections through `ssh-config` race on `known_hosts`: half fail with "Host key verification failed" (wrong-result)

Severity: wrong-result. Round 16's fix makes the `ssh-config` ProxyCommand (`crowdvm tunnel --pin`) rewrite the CLI's `known_hosts` on every connection. `setKnownHost` reads the file, then `os.WriteFile`s it (truncate, then write) with no lock and no atomic rename. When several connections start at once (VS Code Remote opens several, as do parallel `rsync`/`scp` jobs and Ansible), one ProxyCommand truncates the file while another ssh reads it, and read-modify-write cycles drop each other's lines. Sequential use works.

Where: `internal/cli/cmd_access.go:168–184` (`setKnownHost`).

Repro: `crowdvm ssh-config h1`, `crowdvm ssh-config e1`, `Include crowdvm/*.conf`. Then 12 × `ssh h1 true` and 12 × `ssh e1 true`, all at once.

Observed: 12 exited 0 and 12 exited 255: 11 × `No ED25519 host key is known for bx_s7vst8mw and you have requested strict checking.` and 11 × `Host key verification failed.`. Afterwards `known_hosts` held only `bx_hs2qp9re`'s line; `e1`'s was gone until the next pin.

Expected: write a temporary file in the same directory and `rename` it into place (readers then always see a whole file), under the config lock or an `flock` so concurrent pins do not drop each other's lines. Skip the write when the line is already current.

### 6. One invite code lets two people join (security)

Severity: security, low. An invite is single-use: a second accept answers `invalid_invite`. `handleAcceptInvite` reads the invite, inserts the member, and only then marks the invite accepted, in separate statements with no condition on `accepted_at`. Two accepts at the same moment both pass the check and both join, with the invite's role (an `admin` invite makes two admins).

Where: `internal/server/orgs.go:717` (`handleAcceptInvite`): the `SELECT` at `:727`, the `INSERT` at `:736`, the unconditional `UPDATE org_invites SET accepted_at=?` at `:740`.

Repro: bob: `POST /orgs/acme/invites {"email":"x@x.test"}` → `code`. admin and dave each send `POST /org-invites/accept {"code":…}` at the same time (two background `curl`s).

Observed: both answered `{"ok":true,"type":"org.joined",…}`, and `GET /orgs/acme/members` listed `admin:member` and `dave:member` from that one invite.

Expected: claim the invite first in one statement (`UPDATE org_invites SET accepted_at=?, accepted_by=? WHERE id=? AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at>?`, and go on only when one row changed), in the same transaction as the member insert.

### 7. With the AI proxy on, every harness vendor is routed through it: OpenCode's free models (and any vendor the profile has no key for) fail (wrong-result)

Severity: wrong-result. `crowdvm ai proxy on` makes the guest export proxy base URLs and the placeholder token for every vendor (`OPENCODE_BASE_URL`, `OPENAI_BASE_URL`, `OPENROUTER_BASE_URL`, …), whatever the profile holds. The proxy answers 409 `provider_not_configured` for a vendor with no key in the profile. OpenCode's free models need no key, and the server lists `opencode` as configured without one (`feat_ai.go:1614`). A user who turns the proxy on for Claude can no longer use them, and the turn shows `running` for over a minute before it fails. The same happens to a Codex or OpenCode user who logs in with their own key inside the sandbox: the exported base URL overrides it.

Where: `internal/guest/agents/agents.go:142–164` (`ProxyEnv`: all vendors), `internal/server/feat_ai_proxy.go:165`.

Repro: carol: `crowdvm ai profile set-key default anthropic-api-key <fake key>`, `crowdvm ai proxy on`, new sandbox `cp1`, `crowdvm prompt cp1 --provider opencode --model opencode/beam-free --new "…"`. Then `crowdvm ai proxy off`, stop and resume `cp1`, and the same prompt.

Observed: with the proxy: `prompt.status running`, then 71 s later `{"error":"The sandbox's credential profile has no credential for opencode.","status":"failed"}`. Without it, the guest's opencode reached `opencode.ai` itself (its log: `Free usage limit reached`, OpenCode's own quota that day, which the e2e suite also treats as an outside outage).

Expected: export a vendor's base URL and placeholder only when the profile has a credential for it, or have the proxy pass a request through without a key for vendors with free use (OpenCode Zen).

### 8. The CLI cannot make a public desktop private again, and `crowdvm desktop ID` then prints a URL that is still public (security)

Severity: security, low. After `crowdvm desktop ID --public`, a later `crowdvm desktop ID` (without `--public`) does not make it private. `DesktopRequest.PublicAccess` is a `bool` with `omitempty`, so `false` is never sent, and the server keeps the desktop public until the sandbox stops. The command prints the bare viewer URL, with no token and no word that it is public, while its help says "It carries a token: treat it as a secret". A user who shared a public link for a demo and then asks for a normal link has no CLI way to close the public one short of stopping the sandbox. The API does it (`{"publicAccess":false}` → the next anonymous visit goes to sign-in).

Where: `internal/client/types.go:484` (`omitempty`), `internal/cli/cmd_access.go:778` (sends only `--public`; no `--private`).

Repro: `crowdvm desktop e1 --public --no-open`, then `crowdvm desktop e1 --no-open` → `{"desktopUrl":"http://127.0.0.1:9963/desktop/bx_s7vst8mw",…}`. Open that URL in Chromium without a session.

Observed: the viewer connects anonymously (`RFB 003.008`, canvas drawn, "connected"). `POST /sandboxes/bx_s7vst8mw/desktop?vnc=1 {}` → `publicAccess: true`. After `{"publicAccess":false}` through the API, the same URL → `/login?next=%2Fdesktop%2Fbx_s7vst8mw`.

Expected: `crowdvm desktop ID` sends `publicAccess:false` (a `*bool`, or no `omitempty`), so the desktop is private again and the URL carries a token, or a `--private` flag. When the result is public, the CLI says so.

### 9. After a type change to a smaller type, the API reports (and the node reserves) the smaller disk while the sandbox keeps the bigger one (wrong-result)

Severity: wrong-result, low. By design a disk never shrinks: `e1`, grown to `large`, keeps its 123 GiB disk and 98 GB `/home` when resumed as `small`. The API still reports `storage.diskGB: 51` (the `small` spec), and the node's `usage`/`reserved` count 51 GiB for it. The user sees a wrong disk size, and a node's disk capacity is undercounted by the difference for every such sandbox, so the scheduler can overcommit beyond the owner's setting.

Where: `internal/server/sandboxes.go:344` (`"diskGB": spec.DiskGiB`), and the scheduler's reservation from the size spec.

Repro: `crowdvm new --type small --name e1`, `stop`, `resume --type large`, `stop`, `resume --type small --node r17n2`. Then `crowdvm info e1`, `blockdev --getsize64 /dev/vda` and `df -h /home` in the guest, and `crowdvm nodes` (admin).

Observed: `small {'diskGB': 51, 'encrypted': True}`, `/dev/vda` 132 070 244 352 bytes, `/home` 98G, node `r17n2` `usage.diskGiB 51` for its one VM.

Expected: the sandbox keeps a disk size of its own (the largest it has had), reported in `storage.diskGB` and used for placement and reservations.

### 10. `crowdvm exec … | head -1` prints "broken pipe" with a hint that the server may be unreachable, and exits 1 (cosmetic)

Severity: cosmetic. Round 16's fix works: the remote command is stopped when the reader goes away. But every such pipeline then prints `error: write /dev/stdout: broken pipe` and `hint: is the CrowdVM server reachable? check crowdvm status and --api-url / CROWDVM_API_BASE`, and the CLI exits 1. `syscall.Errno` satisfies `net.Error`, so any errno (EPIPE here) gets the network hint. Scripts with `set -o pipefail` see a failure for a normal `| head`.

Where: `internal/cli/app.go:214`.

Repro: `crowdvm exec h1 -- 'for i in $(seq 1 50); do echo line$i; sleep 0.1; done' | head -1; echo ${PIPESTATUS[0]}` (also with `--stream`).

Observed: `line1`, the two lines above, `rc=1`. The remote loop was gone 3 s later and never wrote its end marker.

Expected: a write error on stdout after the reader closed ends quietly (exit 0, or 141 like other tools), with no network hint. The hint only for real network errors (`*net.OpError`, `*url.Error`), not every `syscall.Errno`.

## Round-16 fixes rechecked live

| r16 item | Result |
|---|---|
| Grown encrypted disk never boots (opus 1) | Holds. `e1` (encrypted, `small`, 300 MB blob + `markA`) resumed as `large` on `r17n1`: `vda2` 207 618 008 sectors (a multiple of 8), `crowdvm-data` 207 585 240 sectors at 4096-byte sectors, `/home` 39G → 98G, blob sha256 OK. Then stopped and resumed as `small` on `r17n2` (2 min 9 s incl. the chain download), stopped, resumed on `r17n1`: every boot opened, `markA`/`markB` and the blob intact. A size quirk remains: bug 9. |
| A disk left unaligned by the old code | Holds. In `e1` I set `vda2` to end at the last usable sector with `sfdisk -N 2` (`size=207618015 mod8=7`, the state round 16's bug left), wrote `markC`, stopped and resumed: preinit rounded it back to 207 618 008 and opened it, all marks and the blob intact. |
| Egress flows outlive the session (opus 2) | Holds for the session end. 256 flows (exactly the cap) to the non-reading sink: `crowdvm stop` → 0 within 2 s, a guest `systemctl reboot` → 0, and egress works again after both. Within a live session they never end and cut the sandbox's network: bug 1. Server RSS was 536 MB after these runs; I did not watch it for longer. |
| Public desktop URL (opus 3) | Holds. `crowdvm desktop e1 --public` URL in headless Chromium without a session: websocket to `/desktop/ws`, `RFB 003.008`, canvas drawn, "connected". A private desktop's bare URL → `/login?next=…`, and its token link connects. Revoking public from the CLI: bug 8. |
| `exec` drops stdin (opus 4) | Holds. A pipe or a non-empty file on stdin prints the warning that points to `crowdvm ssh ID -- CMD`; `/dev/null` and an empty file print nothing. An empty pipe also warns (harmless). |
| `ssh-config` after a resume (opus 5) | Holds sequentially. After `stop` and `resume --node r17n2` (new host key), plain `ssh`, `scp` (5 MB, sha256 OK) and `rsync -e ssh` all work with no warning. Many connections at once: bug 5. |
| `run` hints and exit code (opus 6, sonnet 7) | Holds. Ctrl-C on a real pty during create: `failed · 2s · small · 0.0000 credits · bx_gmpbypds deleted`, `interrupted`, exit 130, no "Remove it with" hint. `--json`: `"exitCode":null,"deleted":true`. SIGTERM during the command: `exitCode 130`, deleted. (A background job in a non-interactive shell starts with SIGINT ignored, and the CLI keeps it ignored; that is the shell, not a bug.) |
| Reconnect on a spent link (opus 8) | Not rechecked (no time for a link that expires). |
| Closed stdout (sonnet 8) | Holds: `exec … \| head -1` and `exec --stream … \| head -1` stop the remote command. The message: bug 10. |
| TypeScript timeout (sonnet 11) | Holds. SDK built from `sdk/typescript` with `timeoutMs: 400` through the 40 MB/s proxy: `artifact()` of a 47 MB file took 1.15 s and arrived complete (sha256 matches the guest's). |

## New ground: what held

- **Invites and roles:** an owner invite gives a join code; joining with it twice answers `invalid_invite` (but two accepts at once: bug 6). `admin` invites make admins. Admins remove members; members cannot. A member's `concurrentCap 1` refuses a second org sandbox and a fork with 429 `member_limit_reached`; clearing it with `null` works.
- **Shared vs private:** a new org sandbox is private while it holds the creator's logins; `share` on it says teammates can only view it until stop/resume, and after the restart it is `shared`, `holdsCreatorLogins: false`, with no AI-proxy env and no environment. A teammate then gets in with `crowdvm ssh`. A teammate gets 403 `sandbox_private` before that. The org owner can stop a member's private org sandbox.
- **Removing a member with running sandboxes:** 409 `member_has_running_sandboxes`; after stopping, removal works, and the member's org sandbox comes back as their personal one (resumable, `team: null`). The removed member gets 404 for the org's sandboxes through `ssh` and `tunnel` (keys and sessions: bug 2).
- **Leaving and deleting:** leaving with running org sandboxes → 409; after stopping, `DELETE …/members/me` works and the sandboxes move to personal. `org delete` refuses with other members (`org_has_members`) and with a balance (`org_has_balance`); after removing the service account and moving the credits out with `--from acme` (bug 4), it deletes and the active org falls back to personal.
- **Service accounts:** a `full-sandbox` service account cannot create sandboxes (403, as its preset says); a `ci` one can, billed to the org. The proper delete endpoint refuses while it has sandboxes (the members endpoint: bug 3).
- **Org environments and secrets:** `env access prod --only carol` then `--remove carol` leaves it to owners and admins. carol then gets `environment_not_found` for `env info prod` and 403 `environment_forbidden` for `new --environment prod`, by name and by id. A shared sandbox gets no environment at all.
- **AI proxy:** the guest gets `ANTHROPIC_BASE_URL=http://10.0.2.2:9963/ai-proxy/anthropic` and a `cvmai_` placeholder; a call through it reaches the fake vendor with the real key. After a stop/resume the old placeholder answers 401. A live placeholder also works from outside the sandbox (the host); like the sandbox token, I took that as by design.
- **Prompts:** attachments arrive under `~/attachments` (equal base names overwrite each other: known from bugs-r16-sonnet). A prompt running when the sandbox stops ends `interrupted` with `the sandbox stopped while this prompt was running`, and the waiting CLI ends with `status: interrupted`. `steer` on a running turn answers `steered` (fallback mode); `interrupt` ends the turn (`interrupted`, no `opencode run` left). Conversations stay listed. The free model itself hit its daily quota, so no turn produced an answer.
- **Webhooks:** with the receiver answering 500, `sandbox.ready` and `sandbox.archived` were retried after 10 s and then 30 s (attempts 1–3, `X-CrowdVM-Attempt` counting), and delivered once it answered 204. Every delivery's `X-CrowdVM-Signature` verified against the secret over `delivery.timestamp.body`. A retried old event can arrive after a newer one (the old `archived` came after the new `ready`); the docs promise no order and the payload carries `createdAt`, so I did not count that as a bug.
- **OTel:** `telemetry set` sends a test export (200) and the `Authorization` header reaches the collector on every request. Metrics every 60 s (4 resources, 20 gauges). A lifecycle log record whose collector took 12 s (over the 10 s timeout) was retried after 20 s and again after 41 s, and delivered once the collector answered in time.
- **Desktop:** the private token link connects in Chromium without a session and the URL loses its token.

## Not verified

- Org wallets running out: the server ran without `--enforce-credits`, so an empty org wallet and `usageCapCredits` were not exercised.
- Webhook deliveries for org sandboxes: webhooks and OTel are per user (the sandbox creator's), so an org owner's webhook does not see members' org sandboxes. Boat's behavior here is unknown to me, so I did not report it.
- A full answered agent turn: OpenCode's free model was over its quota; Claude Code against the fake vendor was not tried (non-streaming fake).
- The reconnect on a spent desktop link (r16 opus 8), and `crowdvm exec` on a deleted sandbox's egress (delete goes through stop, which I tested).
- Bug 1 against a real internet host that stalls.

## Cleanup

Deleted every sandbox (`e1`, `h1`, `w1`, `cs1`, `cs3`, `cp1` and the service account's `bx_m4hzu7t5`, the last through an admin `suspend` of the service account so it counts as orphaned). Then I stopped both node units, the server, the 40 MB/s proxy, the receivers, the TCP sink and Chromium. Nothing of mine listens on 9960–9969, no process matches `crowdvm-r17opus`, no qemu of mine runs, and no `r17opus-*` unit is loaded. `~/.local/share/crowdvm-r17opus` (binaries, node and server dirs, images, logs, scratch homes) is deleted. The worktree `/home/tl/dev/CrowdVM-r17opus` is removed after this report was copied to `docs/reviews/bugs-r17-opus.md` in the shared checkout (untracked). I touched nothing else there, and did not touch `crowdvm-field`, the e2e clusters, other worktrees or `~/.npmrc`.

## Fixes

All 10 fixed; bug 5 by the same change as bugs-r17-sonnet 2. Tests: `internal/server/fix_r14_test.go` (`TestR17StalledEgressFlowEnds` 1, `TestR17SmallerTypeKeepsTheBiggerDisk` 9), `internal/server/fix_r17_test.go` (2, 3, 4, 6), `internal/cli/fix_r17_test.go` (8, 10), `internal/guest/agents/fix_r17_test.go` (7).

1. **Stalled egress flows** (`guest_egress.go` `stallConn`, `mux.Join`): an egress write the destination does not take within 2 minutes fails; the flow then ends at once (a copy error closes both ends) and the destination gets a reset. Its slot is free again.
2. **Removed member's SSH keys** (`acct_sshkeys.go` `revokeMemberKeys`, `orgs.go`): removing or leaving revokes the member's keys in the org's sandboxes (at once when running, else at the next ready), except keys the creator also sent. Open sessions are not ended.
3. **Service account via the members endpoint** (`orgs.go`): 409 `service_account`, pointing to `DELETE /orgs/{org}/service-accounts/{id}`.
4. **`credits transfer` scope** (`credits.go`): without `from`, the request's scope (`--org`, `CROWDVM_ORG`, the active org) is the source.
6. **One invite, two members** (`orgs.go` `handleAcceptInvite`): the invite is claimed with one conditional update in the same transaction as the member insert.
7. **AI proxy and vendors without a key** (`proto.AIProxy.Vendors`, `feat_ai.go`, `feat_env_guest.go`, `agents/agents.go`): the server names the vendors the profile holds keys for (also on live applies); the guest points only those at the proxy. Older servers: every vendor, as before.
8. **Public desktop stays public** (`client/types.go`, `cmd_access.go`): `crowdvm desktop ID` sends `publicAccess:false` unless `--public`; a public answer is said on stderr.
9. **Smaller type, bigger disk** (`scheduler.go` `diskSpec`, `lifecycle.go`, `lifecycle_place.go`, `sandboxes.go`, `nodes_evacuate.go`): a sandbox records the largest disk it booted with (`extra.diskGiB`); `storage.diskGB`, placement and node reservations use it. Billing stays by type, and the type_too_small check keeps the type's own size. A fork of a grown sandbox does not inherit the record yet.
10. **`exec | head` noise** (`cli/app.go`): the network hint only for real network errors; EPIPE on stdout exits 141 quietly.
