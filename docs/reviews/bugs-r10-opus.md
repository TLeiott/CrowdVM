# Bug hunt r10 (opus): environments, AI credentials, outbound requests, 2026-11-11

Setup: `make build` at 18:33 (commit `11343ee`). Binaries were copied to my own data dir (`~/.local/share/crowdvm-r10opus`), so later rebuilds could not change them mid-run. One `crowdvm-server` listened on 127.0.0.1:9960. It ran with `--public-url http://127.0.0.1:9963`, and from 18:55 on with `--public-domain r10.test` (before that, `localhost`). Two qemu nodes, `r10n1` and `r10n2`, had their UIs on 9961 and 9962. Each had 6 vCPU, 14 GiB, 60 GiB and at most 4 sandboxes, plus `ubuntu-24.04-crowdvm-v3` and `-minimal-v3` from `crowdvm-node image import`. The server default image was the full v3, because the minimal image has no agent harness.

Node↔server traffic went through a Python token-bucket TCP proxy on 9963 (40 MB/s per direction). Guests reach the server only at the node's server address, so the public URL is the proxy port too, and guest↔server traffic was throttled as well. Two Python receivers logged every request:
- 127.0.0.1:9966: fake AI vendor, via `CROWDVM_AI_UPSTREAM_ANTHROPIC`.
- 192.168.178.31:9967 (LAN): webhooks, slow answers, redirects.

The server ran without `--webhook-allow-local`, so the outbound guard was live. Users were `admin` and `bob`, plus an org `acme` (owner admin, bob joined and was removed several times). Each user's CLI had its own config dir. For the uninstall recheck, node2 ran for a while as a transient systemd user unit (`r10opus-node2`) with the unit's `KillMode=process`. Headless Chromium (Playwright's `chrome-headless-shell`, driven over CDP from node) checked the cookie bug.

Cleanup: every sandbox, saved snapshot and share is deleted. The server DB shows 0 live sandboxes, 0 saved snapshots and 0 live snapshot rows. The last inspector VM (`insp_…` on r10n2) was destroyed by its idle timer before I stopped the nodes. The server, both nodes, the proxy, the receivers, Chromium and every qemu of mine are stopped. The transient unit is gone. My data dir and scratch files are deleted.

## Bugs

### 1. Any org member can download another member's secrets from an org saved snapshot, including a working in-sandbox API key (security)

Severity: security.

Where: `internal/server/feat_snapfiles.go:112` and `:124` (`snapForRead`). Any member of the owning org may read an org saved snapshot, raw, through `GET /snapshots/{id}/tree`, `/files` and `/download` (`crowdvm snapshot tree|pull`). Deploying the same snapshot as another member runs `foreignDeployScrub` (`lifecycle_secure.go:366`), which removes the saver's logins, keys, env secret files and history. The file routes skip all of that. They also accept `/abs` paths (`feat_snapfiles.go:13-16`), so `/var/lib/crowdvm/history` and the agent store can be read too.

Repro:
1. admin: `crowdvm env set-var base ADMINSECRET=ADMINVAL-R10-FFF`, `echo ADMINFILE-R10-GGG | crowdvm env set-file base .config/adminfile.txt`.
2. admin: `crowdvm new --org acme --environment base` (a private org sandbox, `holdsCreatorLogins: true`), then `crowdvm snapshot ID orgsnap --org acme`.
3. bob (member of acme): `crowdvm exec ID --org acme 'echo x'` → `sandbox_private, HTTP 403`, as it should.
4. bob: `crowdvm snapshot pull snap_… -o out --path .config/adminfile.txt --path .bashrc --org acme`.

Observed:
- `out/.config/adminfile.txt` is `ADMINFILE-R10-GGG`.
- `out/.bashrc` holds `ADMINSECRET='ADMINVAL-R10-FFF'`, admin's `CROWDVM_API_KEY='cvm_…'` and the AI placeholder `ANTHROPIC_API_KEY='cvmai_…'`.
- With that key, bob ran `POST /sandboxes/ID/commands {"command":"id -un; cat ~/.config/adminfile.txt"}` in admin's private sandbox: exit 0, output `user\nADMINFILE-R10-GGG`.
- With the placeholder, bob called `/ai-proxy/anthropic/v1/messages`. The vendor received admin's personal key (`sk-ant-P2KEY-R10`).
- For comparison, `crowdvm new --org acme --from orgsnap` as bob scrubs correctly: no `adminfile.txt`, no `ADMINVAL` in `.bashrc`, no admin history.

Expected: the file routes of a snapshot saved by someone else should give only what a foreign deploy would see, or be refused to anyone but the saver and org admins. At minimum, never serve the paths `foreignDeployScrub` lists, the `.bashrc` env block, or `/var/lib/crowdvm`.

### 2. A removed org member keeps using the org's environment and AI keys, even in new sandboxes (security)

Severity: security.

Where:
- `internal/server/feat_ai.go:1374` (`aiSandboxProfile`): uses `cfg.CredentialProfileID` whenever the profile exists. It never checks that the sandbox's owner may still use it.
- `internal/server/feat_env.go:1200` (`envResolve`): a resume stays on its pinned environment. `envOf` (`:1267`) and `envPlanFor` (`feat_env_guest.go:63`) deliver that environment at every boot with no membership or access check.
- `internal/server/orgs.go:546` (`handleRemoveMember`) changes neither.

Repro A, raw org key in a brand-new sandbox:
1. bob, while a member: `crowdvm env new bobenv`, then `crowdvm env set bobenv --credential-profile orgprof`. `orgprof` is the org's profile with `anthropic-api-key=sk-ant-ORGKEY-R10`, and the org's AI proxy is off.
2. admin: `DELETE /orgs/acme/members/bob` (200 `org.member_removed`). `crowdvm ai profiles` as bob now lists only `default`.
3. bob: `crowdvm new --environment bobenv`, then `crowdvm exec ID 'echo $ANTHROPIC_API_KEY'`.

Observed A: `sk-ant-ORGKEY-R10`, the org's real key, in a sandbox bob created after his removal. `crowdvm env info bobenv` still shows the org profile's id.

Repro B, org environment on resume:
1. bob, while a member: `crowdvm new --org acme --environment orgenv`. `orgenv` holds `ORGSECRET=ORGVAL-R10-DDD`, `.config/orgfile.txt` and `orgprof`, with the proxy on.
2. bob: `crowdvm stop --wait ID`.
3. admin removes bob. The sandbox becomes personal (`org_id` NULL) but keeps `environment_id` of `orgenv`.
4. bob: `crowdvm resume ID`.

Observed B: `echo $ORGSECRET` gives `ORGVAL-R10-DDD`, freshly written at this boot. A new proxy placeholder is minted for `orgprof`, and a call through it reached the vendor with `sk-ant-ORGKEY-R10`.

Expected: at boot (and on hot apply), drop an org environment and an org credential profile the owner can no longer use (not a member, or not on the access list). On member removal, also clear `credentialProfileId` in the member's personal environments when it points at the org's profiles.

### 3. A live `env upgrade` leaves the AI proxy placeholder valid for credentials the new version withholds or replaces (security)

Severity: security.

Where: `internal/server/feat_env_guest.go:387` (`envApplyLive` builds `ApplyConfig` with no `AIProxy` and no `ClearAIProxy`) and `internal/server/feat_ai.go:1439` (`aiPlan` with `boot=false` reuses `aiCurrentToken`). The `ai_proxy_tokens` row keeps the `profile_id` of the boot. Nothing deletes it when agent credentials are turned off, and nothing re-points it when the profile changes. The proxy (`feat_ai_proxy.go`) trusts that row.

Repro (proxy on, sandbox running on env `base` with profile `default` = `sk-ant-REALKEY-R10-ANTH1`):
1. `crowdvm env set base --agents-credentials false`, then `crowdvm env upgrade base`, which answers `upgradedLive: 1`.
2. `crowdvm exec ID 'env | grep -c cvmai_'` gives `7`. A `curl` to `$ANTHROPIC_BASE_URL/v1/messages` with the placeholder succeeds, and the vendor receives `sk-ant-REALKEY-R10-ANTH1`.
3. `crowdvm env set base --agents-credentials true --credential-profile p2` (key `sk-ant-P2KEY-R10`), then `crowdvm env upgrade base`.
4. The same `curl` still reaches the vendor with `sk-ant-REALKEY-R10-ANTH1`, the old profile's key. The token row still names profile `default`.

Expected: a live apply that turns agent credentials off (or `safeForThirdParties` without proxying) should delete the sandbox's proxy token and clear the guest's proxy env. One that changes the profile should mint a new token for the new profile and push it, or update the row's `profile_id`. Otherwise "upgrade" does not revoke anything until the next stop.

### 4. A host-routed app can set the dashboard's session cookie for the whole public domain (security)

Severity: security. Login CSRF, or a persistent logout, when the dashboard is reached on the public domain or under it.

Where: `internal/server/feat_host.go:738`. `ModifyResponse` filters response headers only for path-routed apps (`if prefix != ""`). Host-routed apps (`<label>-<port>.<public-domain>`) pass `Set-Cookie` with any `Domain=` unchanged, along with `Clear-Site-Data`, `Refresh`, `NEL` and `Report-To`. The session cookie `crowdvm_session` is host-only with no `__Host-` prefix (`auth.go:211`). The server reads the first cookie of that name (`auth.go:259`).

Repro (server with `--public-domain r10.test`; the server serves the dashboard on the bare `r10.test` host too, which answers 200 `text/html`):
1. In a sandbox, an app on 3001 answers `Set-Cookie: crowdvm_session=attacker; Domain=r10.test; Path=/api/`. Then `crowdvm host ID 3001 --public`.
2. Headless Chromium (host rules mapping `*.r10.test` and `r10.test` to 127.0.0.1) opens `http://bx-…-3001.r10.test:9960/`, then `http://r10.test:9966/api/probe` (my receiver; cookies ignore the port).

Observed:
- The cookie jar holds `crowdvm_session=attacker`, domain `.r10.test`, path `/api/`, and the receiver got `Cookie: crowdvm_session=attacker`.
- Chromium sends the longer-path cookie first.
- `curl -H "Cookie: crowdvm_session=<bob's>; crowdvm_session=<admin's>" /api/v1/me` answers as `bob`. So an app owner who sets their own valid session makes a victim's dashboard API calls run in the attacker's account. A garbage value logs the victim out for as long as the cookie lives.
- `docs/guide/public-urls.md:37` recommends `--public-domain crowdvm.example.lan` with a wildcard record, so a dashboard on `crowdvm.example.lan` is a normal setup. The same works for a dashboard on any host under the public domain.

Expected: for host-routed apps, drop `Set-Cookie` lines that carry a `Domain` attribute (or the server's cookie names), as `pathScopeCookies` does for path routing. Better still, rename the session cookie to `__Host-crowdvm_session`, which no sibling can set.

### 5. A foreign deploy still carries the saver's agent conversations, as Claude Code's own transcripts (security)

Severity: security. Round-9 sonnet #2 is only partly fixed.

Where: `internal/guest/apply.go:173` (`runScrub` with `History`) wipes the agent manager's store (`/var/lib/crowdvm/agents`) and the command history. `foreignDeployScrub` (`internal/server/lifecycle_secure.go:366`) lists no harness transcript paths. The harness CrowdVM runs for `crowdvm prompt` writes the full conversation to `~/.claude/projects/-home-user/<session>.jsonl`, and those files stay.

Repro:
1. admin (full image, fake vendor behind the AI proxy): `crowdvm prompt ID --provider claude --new "use AGENTSECRET=AGSEC-R10-CCC to deploy"`. The turn finishes with the vendor's answer.
2. admin: `crowdvm snapshot ID secsnap`, then `crowdvm snapshot share secsnap`.
3. bob: `crowdvm new --from bsh_…`.

Observed:
- `crowdvm conversations NEWID` and `crowdvm events NEWID` are empty, and `crowdvm history` lists only bob's own commands, so the round-9 fixes work.
- `~/.claude/projects/-home-user/5284f7d6-….jsonl` (155 KB) and a second transcript are on bob's disk. They begin with `{"type":"queue-operation",…,"content":"use AGENTSECRET=AGSEC-R10-CCC to deploy"}` and hold the user message and the model's answers.

Expected: with `History`, also remove the transcripts and session stores of the harnesses CrowdVM drives: `~/.claude/projects`, `~/.claude/sessions`, `~/.claude/history.jsonl`, `~/.codex/sessions`, `~/.codex/history.jsonl`, OpenCode's and the others'.

### 6. `--no-monitoring`: concurrent large commands still fill `/run`, cut output with no warning, and break every short exec meanwhile (wrong-result)

Severity: wrong-result. The round-9 fix (opus #3) holds for commands run one after another, not for commands that run at the same time.

Where:
- `internal/guest/history.go:297` (`makeRoom`) drops only finished records, so running commands can still use all of `/run`. Each stream is capped at one eighth (`commands.go:179`).
- `makeRoom` runs every 100 ms from every running command's `capOutput` loop (`commands.go:165`). Under pressure it deletes a short command's record the moment it finishes, before the CLI has followed it.

Repro (small sandbox, `crowdvm new --type small --no-monitoring`, 784 MB `/run`):
1. 8 at once: `crowdvm exec --timeout 200 ID "bash -c 'yes N | head -c 200000000 2>/tmp/err.N; echo \${PIPESTATUS[*]} > /tmp/rc.N; sleep 30'"`.
2. During that, `crowdvm ssh ID 'df -h /run'` shows `784M 769M 15M 99%`.
3. Afterwards `/tmp/rc.N` is `141 1` for all 8, and `/tmp/err.N` is `head: error writing 'standard output': No space left on device`.
4. On the client, 7 got the 102 652 416-byte cap plus the truncation warning. One (`c4`) got 86 736 896 bytes, exit 0, and no warning.
5. With 7 fillers instead of 8 (`/run` at 88 %, no ENOSPC), I sent three probes, `crowdvm exec ID 'echo probeN'`. The first failed with `error: no detached command with this process id (process_not_found, HTTP 404)`. The other two exited 0 and printed nothing. With 8 fillers, all three probes failed with `process_not_found`.

Expected:
- Never reap a record whose follower has not read the exit frame. That alone fixes the probes.
- Cap the total that running unmonitored commands may hold on `/run`, or spill to disk past a share of it. When output is lost because the file system is full, say so like the cap does (`outputTruncated`).

### 7. A custom AI API base URL may be any DNS name, so the server connects to loopback, LAN and metadata addresses for a sandbox (security, low)

Severity: security, low.

Where: `internal/server/feat_ai.go:1008` (`aiValidBaseURL`) checks the URL's text only: `https`, port 443, not an IP literal, not `.local`/`.lan`/…. `internal/server/feat_ai_proxy.go:48` (`aiTransport`) dials whatever the name resolves to, with no address guard. Webhooks and OTel use `guardedDial` (`outbound_guard.go`) for exactly this.

Repro:
1. `PUT /credential-profiles/p2/custom-apis/SSRF1` with `{"value":"CUSTOMSECRET-R10","baseUrl":"https://127.0.0.1.nip.io"}`. The same for `https://169.254.169.254.nip.io` and `https://192.168.178.31.nip.io`. All three answer 200.
2. In a sandbox on that profile: `curl http://10.0.2.2:9963/ai-proxy/custom/SSRF1/admin/x -H "Authorization: Bearer $ANTHROPIC_API_KEY"`.

Observed:
- 502 `The provider did not answer: dial tcp 127.0.0.1:443: connect: connection refused`, and the same for 192.168.178.31:443.
- The metadata name hangs until the client gives up.
- The server makes TLS connections, with the guest's method, path, headers and body plus the custom secret, to its own loopback and to LAN hosts. The error text tells the sandbox which internal ports are open.
- Certificate checks limit what an attacker can read, but this is the probing the outbound guard exists to stop.

Expected: dial custom API upstreams through `guardedDial` (refuse loopback, link-local, metadata and, unless `CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE=1`, private addresses at connect time), and do not echo dial errors to the guest.

## Round-9 fixes rechecked live

| r9 item | Result |
|---|---|
| Resume of a `node_offline` sandbox (opus 1) | Holds. After `kill -9` of r10n1, `resume` answered 409 `node_offline` ("retry … after 179 s, or `stop --force`") within a second. `resume --type default` and `recover` were refused too (`recover_unsupported`). A loop of 120 resumes raced the node's restart. 17 got 409, the rest got `ready` on the same node only after the node adopted the VM. `~/b.txt` (written and synced after the last snapshot) was intact, and `uptime -s` was unchanged. |
| Foreign deploy scrub: command history (opus 2) | Holds. bob's `crowdvm history` lists only his own commands, and `aws_secret=TOPSECRET-R10` is not on his disk. The `~/.ssh` key and env secret file are gone, and `.bashrc` has bob's env only. |
| Foreign deploy scrub: agent conversations and events (sonnet 2) | The agent store is wiped (`conversations` and `events` are empty). Claude Code's transcripts survive: bug 5. |
| Foreign deploy by an org member (r9 "not verified") | Holds for deploys: bob's deploy of the org's `orgsnap` had no `adminfile.txt`, no `ADMINVAL`, no admin history. The file routes bypass it: bug 1. |
| `--no-monitoring` filling `/run` (opus 3) | Sequential: holds. 10 runs of a 200 MB command in a row all exited 0 with the cap and the warning, and `/run` stayed at 51 %. Concurrent: not fixed, see bug 6. |
| Truncation warning text (opus 4) | Holds. It now says "the command wrote more output than the sandbox keeps per stream; the rest was dropped", with no size. |
| Guest-side deadline after `kill -9` of the CLI (opus 5) | Holds. `setsid crowdvm exec --timeout 15 ID 'exec sleep 783'`, killed after 3 s: 40 s later there was no `sleep 783`. The same with `trap "" TERM; sleep 784` and `--timeout 10`: gone after 45 s. |
| Set-Cookie / Clear-Site-Data / Refresh on `/p/{id}/{port}/` (sonnet 1) | Holds for those three. `crowdvm_session=` is dropped, and `x=1; Path=/` became `Path=/p/bx_…/3000/`. `NEL` and `Report-To` still pass on the dashboard's origin (origin-wide, `include_subdomains`). Browsers take NEL only over HTTPS, and I ran no TLS server, so not counted. Host-routed apps are not filtered at all: bug 4. |
| Signed file URLs (sonnet 1) | Not reached live: it needs a replaced guest agent. Code: `guardGuestContent` now deletes the three headers (`passthrough.go:291`). `NEL`/`Report-To` are not in the list. |
| Idempotency-Key replay (sonnet 3) | Holds. The replay of a create with `hostedPorts` returns the same `hostedPorts` (same tokens), and differs only by `idempotentReplay: true`. A `count: 3` create from a saved snapshot replays all three ids. The same key with another body gives 409 `idempotency_key_reused`. Four concurrent requests with one key created one sandbox (2 replays, 1 `idempotency_in_progress`). |
| `uninstall-service` and leftover VMs (sonnet 6) | Holds for the cgroup. I started node2 as a transient user unit with `KillMode=process` and placed a sandbox there with `--node r10n2`. `systemctl --user stop` left its qemu running (by design). Then `Uninstall`'s sequence (`kill --kill-whom=all SIGTERM`, 3 s, SIGKILL, `disable --now`) killed that qemu although the unit was already inactive. A VM the node adopted from an earlier run outside the unit (`bx_qnavrbkb`) is not in the unit's cgroup and would survive; only a manual `run` before `install-service` gets there, so not counted. |

## New ground: what held

- **Where env secrets land.** Vars go to `/etc/profile.d/crowdvm-env.sh`, the managed block of `~/.bashrc` and `/run/crowdvm/cfg/config.json` (the config drive). Secret files are mode 600 in the home. None of it appeared in server or node logs (debug level), for any of 11 marker strings, including `whsec_` and `cvmai_`. Node `vms.json` and `vm.json` hold the guest config in clear text, as `security.md` says (the node owner can read the VM). No entries of deleted sandboxes were left.
- **Rotation and removal.** `env rm-var` and `rm-file` plus a live `env upgrade` removed the var from exec, login and interactive shells, and the file from disk.
- **AI proxy.**
  - The placeholder works only through the proxy. The vendor gets the real key, never the placeholder, with no cookie.
  - `ai block` gives 403 `ai_access_blocked`, and `unblock` restores access.
  - `ai profile rm-key` gives 409 `provider_not_configured` at once on a running sandbox.
  - Placeholders die at stop (a new one after resume).
  - `--no-env` gets none.
- **Webhook outbound guard.** Refused at save: `127.0.0.1`, `127.0.0.1.nip.io`, `localtest.me` (::1), `[::ffff:127.0.0.1]`, `[::1]`, `169.254.169.254.nip.io`, `0.0.0.0`, `LOCALHOST.` and `foo.localhost.`. Accepted at save but failing at dial: `0`, `127.1`, `2130706433`, `127.0.0.1.` (no such host), `[::127.0.0.1]` and `[64:ff9b::7f00:1]` (unreachable). Nothing reached the loopback receiver. A 307 to `http://127.0.0.1:9966/stolen` is reported "redirects are not followed".
- **Webhook signature.** `X-CrowdVM-Signature` is `v1=HMAC-SHA256(secret, delivery.timestamp.body)` and verified for `sandbox.archived` and `sandbox.ready`. `X-Ascii-Signature` is identical.
- **Slow webhook receiver.** `webhooks/{id}/test` against a receiver that sleeps 40 s returns after 5 s, "timeout: no answer within 5s".
- **OTel.** `GET /telemetry` returns header names only, never values. The endpoint goes through the same guard and client (code read; not exported live).
- **Shared org sandbox snapshots.** After `crowdvm share`, bob's `snapshot pull` of the sandbox (which still holds the creator's logins) gets 403 `sandbox_private`, like exec. Bug 1 is specific to saved snapshots.

## Not verified

- NEL/Report-To over HTTPS in a real browser (needs a TLS server).
- Bug 4 with a dashboard on a sibling host (for example `crowdvm.example.lan` with `--public-domain apps.example.lan`). That one is not affected, because `Domain=apps.example.lan` does not cover it. Only a dashboard on the public domain or under it is.
- Bug 2 for an environment's org access list (`env access --only`) and a profile's access list (`ai profile access`). Same code path (no check at boot), not run.
- Bug 5 for Codex and OpenCode transcripts. Only Claude Code was run.
- Many concurrent slow webhook or OTel receivers across accounts (`whPerOwner = 4`, `whParallel = 16`): not run.
- The OTel export itself (only the code and the save-time guard).

## Mistakes on my side

- First server start: `--public-url` was the server port while nodes joined through the proxy port. Guests could not reach `CROWDVM_API_URL` or the AI proxy (connection refused on 10.0.2.2:9960), because the node lets guests reach only its own server address. I restarted with the proxy port as public URL. This is not a product bug: the public URL is what guests and nodes are meant to share.
- My first two `crowdvm delete` calls lacked `--yes` and did nothing (I had sent the output to `/dev/null`). Those sandboxes were counted by the first `env upgrade` (`upgraded: 3`). I deleted them properly afterwards.

## Fixes

All 7 fixed. Regression tests: `internal/server/fix_r10_test.go` (1-3), `internal/server/fix_r10_opus_test.go` (4, 5, 7), `internal/guest/fix_r10_test.go` (6).

1. **Org saved snapshot files** (`feat_snapfiles.go` `snapForRead`): a saved snapshot grants raw file access (tree, files, download) to its saver only. Other members deploy it (scrubbed). A live source sandbox the caller may read still grants access, as before. Test: `TestR10OrgSavedSnapshotFilesOnlyForSaver`.
2. **Removed member** (`feat_env.go` `orgCanUse`, `envOf`; `feat_ai.go` `aiSandboxProfile`): an org environment, and an org credential profile named by any environment, is used only while the sandbox's owner is a member on its access list. Otherwise the sandbox gets no org environment and the owner's own default profile. Test: `TestR10RemovedMemberLosesOrgProfile`.
3. **Live upgrade and the proxy placeholder** (`feat_env_guest.go` `envApplyLive`, `feat_ai.go` `aiPlan`): when agent credentials no longer reach the sandbox, the live apply deletes its proxy token and sends `clearAIProxy`; when the profile changed, the token row points at the new profile. Also: a live apply may switch a sandbox to proxied but never back to real keys (before, a live upgrade of a sandbox booted unproxied into a safe-for-third-parties environment wrote the real keys). Test: `TestR10LiveUpgradeMovesProxyToken`.
4. **Host-routed cookies** (`feat_host.go` `hostScopeCookies`, `passthrough.go`): host-routed apps lose `Set-Cookie` lines with a `Domain` or a server cookie name, and `Clear-Site-Data`, `Refresh`, `NEL`, `Report-To`; path-routed apps and guest file answers lose `NEL` and `Report-To` too. Tests: `TestR10OpusHostRoutedAppHeaders`, `TestR10OpusSignedFileURLDropsNEL`.
5. **Harness transcripts** (`lifecycle_secure.go` `foreignDeployScrub`): the scrub also removes the transcript and session stores of Claude Code, Codex and OpenCode (and the pi, prime, kimi and vibe session folders; those four paths are not confirmed against the tools). Test: `TestR10OpusForeignDeployScrubsHarnessTranscripts`.
6. **`/run` under concurrent commands** (`internal/guest/history.go` `makeRoom`, `commands.go` `capOutput`): a finished record stays until a follower read its exit frame (or 2 minutes); below 10% free, running commands' streams stop growing and are marked truncated, and grow again above 25%; a stream seen with under 1% free is marked truncated. Tests: `TestR10MakeRoomKeepsUnfollowedRecord`, `TestR10OutputFreezesWhenRunNearlyFull` (they fake the free share through `fsFreeShare`).
7. **Custom AI API SSRF** (`feat_ai_proxy.go`): custom APIs dial through `guardedDial` (loopback, link-local and metadata refused at connect time; `--webhook-allow-local` or `CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE=1` lift it) and no longer use `HTTP(S)_PROXY`; dial errors go to the log, the guest gets "The provider did not answer." Test: `TestR10OpusCustomAPIDoesNotDialLoopback`.

Also fixed this round, from the node-UI design review:
- `Node.SetPaused` saved the daemon's whole config, undoing `crowdvm-node config set` changes made while it ran; it now changes only `paused` in `config.json` (`TestSetPausedKeepsOtherConfigEdits`).
- "Drained by the server admin" (local page, `crowdvm-node status`) now says "by you or an admin".
- `security.md`: `config set paused true` applies at once (no restart).
