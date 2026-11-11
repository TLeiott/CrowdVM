# Review sonnet-10: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. I built the binaries into my scratchpad with the Makefile flags, so `bin/` was untouched.

## Verdict: DISAGREE (four blockers, all in the dashboard)

All ten claimed fixes hold, live on my own cluster. The server, the CLI and the SDKs behave as Boat documents in my focus areas (organizations, API keys, data retention). One server-side error code is only partly implemented, and I count it as minor.

What is missing is in `web/`. Four things that Boat's pages explicitly say the dashboard offers are absent or wrong, and each is a small, LAN-safe change:

- **B1.** The Organization tab has no Transfer or Delete, and its role dropdown offers `owner`, which the server rejects. Boat's `organizations.md:104` lists "Create, transfer, delete" under the Organization tab.
- **B2.** The AI credentials page has no "Who can use this profile" panel (`organizations.md:95`).
- **B3.** The sandbox list has no `view only` or `private` row labels (`organizations__shared-sandboxes.md`, "In the dashboard").
- **B4.** The sidebar's single organization select changes the active wallet. Boat keeps "Viewing" and "CLI and API keys bill … Change" as two separate controls (`organizations__active-wallet.md:14,70-71`).

## Method

1. **Read.** `sonnet-9.md`, `opus-11.md`, and the claims in `PARITY.md` for ORG, KEY, DATA and DASH. I also read all of `organizations*.md`, `api-keys*.md` and `data-retention*.md`.
2. **Build and tests.**
   - `go vet ./...` is clean.
   - `go test -count=1` on every package except `test/e2e` is all `ok`.
   - I did not run e2e.
   - I read `e2e-run12.log`: `EXIT=0`, no `--- FAIL`, `agent` and `bootloop` skipped.
3. **Own cluster.**
   - Ports 9970 (server), 9971 (node UI), 9975 (`git daemon` on the LAN address) and 9976 (webhook receiver). Data dir `~/.local/share/review-sonnet-10`.
   - One qemu/KVM node: 6 vCPU, 12 GiB, 3 sandboxes, image `ubuntu-24.04-crowdvm-v3`, `net.allow <LAN>/32`.
   - Users `admin`, `bob`, and throwaway `carol` and `dave`. The server ran with `--webhook-allow-local`.
4. **Webhook receiver.** My own Python HTTP server runs Boat's verbatim `verify_webhook` on every delivery. Every delivery verified (`ok:true`).
5. **Sweep.** Orgs, keys and retention through the CLI, curl and Python/TypeScript SDK calls. I took the dashboard from code (`web/js/**`) and from the server endpoints it calls, not from a rendered browser.
6. **Teardown.** I stopped the server, node, receiver and `git daemon` by saved PIDs. No qemu or process with my data dir remained, and 9970-9979 are free. I deleted my data dir. I did not touch the other reviewer's processes, did not run e2e, and used no sub-agents.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| sonnet-9 B1 | `sandbox.hydrated` only for a restore, once; none on stop | **Fixed (live + code)** | Code: `lifecycle_secure.go:273-277` (publish in `markReady` when `restoredBoot`), `:416-423` (`restoredBoot`: resume, fork, saved/share create). `snapshots.go:197-215` has no hydrated publish any more. Test: `feat_webhooks_test.go:106,154-159`. |
| | | | Live (all deliveries signature-verified): first create → `ready` only. `stop` → `archived` only. `resume` → `ready` + `hydrated` (same second), 1 hydrated and nothing more after 15 s. Fork of a stopped source → `ready` + `hydrated`. Live fork of a running source → `ready` + `hydrated`. Stop of that fork → `archived` only. Payload `{"data":{"sandbox":{id,name}}}`, no `state`/`previousState`. |
| opus-11 B1 | Repository clones wait out the guest's late network | **Fixed (live + code)** | Code: `setup.go:343-365` (up to 6 tries, 2 s doubling, logs "clone failed … retrying"). Live: env `r10` (repo `git://<LAN>:9975/app.git` + secret file `app/backend/.env`), six creates in a row. At `ready` each had `.git`, `README.md`, `backend/.env` (mode 600), 6/6. |
| opus-11 B2 | Ready gate covers a repository whose folder holds a secret file | **Fixed (live + code)** | `boot.go:138-145` now stats `.git`, not the folder. Live: same six creates, the clone was already complete when `crowdvm new` returned `ready` (`ls -a`: `.git`, `backend`, `README.md`; `git status`: `?? backend/.env`). |
| opus-11 B3 | Wallpaper recipe changes the desktop | **Fixed (live + code)** | `desktop.go:592,667-695` (`adoptSessionBus` reads the bus from the running `xfdesktop`/`xfce4-session`). Live: `crowdvm scp red.png ID:/home/user/.config/boat/wallpaper.png` on a `--no-env` sandbox, then `GET …/desktop/screenshot`: pixel (300,300) = `(255,0,0)`. After `rm` it was `(29,39,58)` again. |
| opus-11 B4 | `--fail-fast` answers at once | **Fixed (live + code)** | `client.go:317-318` (503 `no_ready_machine` not retried), `sdk/typescript/src/client.ts:137`, `sdk/python/src/crowdvm/client.py:138-139`. Live on a full node: `new`, `fork` and `resume --fail-fast` each returned `no_ready_machine` immediately. |
| opus-11 B5 | A queued fork survives deleting its source | **Fixed (live + code)** | `snapshots.go:273-277`: a fork with no head snapshot yet roots its source's head. Live: node full, `fork` of a stopped sandbox (marker file `MARK-A`) queued as `cloning`, source deleted, a slot freed, the fork reached `ready`, and `cat marker.txt` printed `MARK-A`. |
| Regressions | Neighbours | **None found** | Fork and resume still carry the data. Org sandboxes, caps and keys unchanged. `go test` all `ok`. |

## New findings

### Blocking (all dashboard)

| ID | What | Evidence | Why it is supportable on a LAN |
|---|---|---|---|
| **B1** | The Organization tab cannot transfer or delete an organization, and its role `<select>` offers `owner`, which fails. | Boat `organizations.md:104`: "Create, transfer, delete … Dashboard: Organization". `web/js/pages/org.js:68` builds the role options as `['member','admin','owner']`; no "Transfer ownership" or "Delete organization" appears anywhere in `web/js` (grep "transfer" and "Delete organ" is empty), although the server has `POST /orgs/{org}/transfer` and `DELETE /orgs/{org}` (`orgs.go:163,167`). Live: `PATCH /orgs/{org}/members/{id} {"role":"owner"}` → `400 invalid_role "role must be admin or member (use transfer for owner)"` (`orgs.go:409`), so picking `owner` in the dropdown shows an error toast. | The endpoints exist. It needs two buttons and a fix to the dropdown. |
| **B2** | No "Who can use this profile" panel on the AI credentials page. | Boat `organizations.md:95`: "open the environment page or the AI credentials page. The **Who can use this environment** or **Who can use this profile** panel…". `web/js/pages/environment.js:142,206-212` has the environment panel. `web/js/pages/ai.js` has no access panel (grep "access", "restricted", "Who can use" finds nothing for profiles). The CLI and API work: `ai profile access team-claude --only admin --org acme` → `{"access":{"restricted":true,…}}`. PARITY AIC-010 and DASH-007 claim it. | The API exists (`PUT /credential-profiles/{id} {access}`). It is the same dialog as the environment one. |
| **B3** | The sandbox list lacks Boat's row labels `view only` and `private`, and offers every row action to teammates. | Boat `organizations__shared-sandboxes.md` "In the dashboard": `by alex`, `view only`, `private`, `shared`. `web/js/pages/sandboxes.js:63` shows only a small "shared" icon (no text), `:69` shows the creator name in the Created cell, and nothing renders `private` or `access:'view'` (grep "view only" finds only the VNC toggle). `web/js/sbx.js:357` offers "Make private / Share with org" on any row with a `visibility`, and Fork, Rename and Delete are always offered. Live, the API gives the data (`access: view/use/owner`, `visibility`, `createdBy`); bob's `fork`, `delete`, `rename`, `extend` on admin's shared sandbox all returned `403 owner_only`. | Pure UI: the fields are already in the list response. |
| **B4** | The sidebar select merges "Viewing" and the active wallet. | Boat `organizations__active-wallet.md:14,70-71`: "The dashboard does not use [the active wallet]. The dashboard bills the organization selected in the sidebar." "Picking an organization in the select above never changes [the active wallet]", and "CLI and API keys bill … Change" is a separate control. `web/js/main.js:169-176` sends `PATCH /orgs/active` on every change of the sidebar select, and `web/js/api.js` has no org header, so the dashboard scope is the account-wide active wallet. Live: `PATCH /orgs/active` is what `crowdvm org switch` uses (`crowdvm org status` followed the change). A user who looks at an org in the dashboard silently changes what their next `crowdvm new` bills. PARITY ORG-024 and ORG-007 (REQUIRED) say the two are separate. | The API takes `?org=`/`X-Boat-Org` per request (verified: `GET /sandboxes?org=…` and `X-Boat-Org` both work). The dashboard can send its own selection (kept in `localStorage`) and leave `/orgs/active` to a second control. |

### Minor (do not block AGREE)

| ID | What | Evidence |
|---|---|---|
| m1 | `environment_passes_logins` is not raised for a fork, or for a resume of a `--no-env` sandbox. Boat (`organizations__shared-sandboxes.md`): "a resume or fork onto an environment that passes logins returns `409 environment_passes_logins`", for `--no-env` sandboxes too. | `feat_env.go:1150-1152` checks only `Op=="resume"` and `holds := !req.NoEnv && …`, and a resume of a `--no-env` sandbox passes `NoEnv=true` (`lifecycle.go:1181`). Live: `resume --environment agents` on a `--no-env` org sandbox → ready, environment now `agents`, **no logins arrived** (`LEAKVAR=none`, no secret file). Fork of a safe sandbox onto `agents` → created, `holdsCreatorLogins:true` (private). The security outcome is safe. Only the documented error is missing. The 409 does fire for a resume of a sandbox created on a `safeForThirdParties` environment (also `feat_env_test.go:391`). |
| m2 | Teammates see history only from the last `share` onward. A command recorded in an earlier shared period (`SHAREDMARK_A`, before an `unshare`) is hidden. Boat hides only what was recorded while private. | Live: admin's history had A, two privates B and C; bob's had only C. Stricter than Boat, no leak. |
| m3 | `data-retention` has no `disable` subcommand (Boat has only `enable` and `status`, so this is fine). `PATCH {"enabled":false}` works with a session. Noted only so nobody files it. | Live. |

I checked the unusual-looking `GET /sandboxes?org=<other org id>` result (it listed another org's sandboxes) and it is correct: Boat's org scope lists your own sandboxes plus the ones the org pays for, so the creator sees their own.

## Disputed classifications

- **DASH-012 / ORG-024** (`PARITY.md:1037`, `:530`; `docs/gaps/B.md:615` "DONE"): not true while B1 and B4 are open. The row says "roles, remove member, per-member caps, rename, default visibility, Viewing dropdown, 'CLI and API keys bill … Change'". Roles are partly broken (B1), and the two selectors are one (B4).
- **DASH-002** (`PARITY.md:1027`, `docs/gaps/B.md:605` "DONE"): the "labels by/view only/private/shared" part is missing (B3).
- **DASH-007 / AIC-010** (`PARITY.md:1032`, `:433`): the profile `access` limit is not in the dashboard (B2).
- **ORG-015** (`PARITY.md:521`, "DONE"): partly, see m1.
- I accept the N/A and ADAPTED rows I touched: ORG-019 (no seat purchase), ORG-022 (no refunds), KEY-001 (`cvm_` prefix; `boat_` and `box_` also work), ORG-002 (invite link or code), DATA-001 (retention job is enforced here, report-only in Boat, admin-switchable).

## What would flip this to AGREE

1. **B1.** The Organization tab has a working "Transfer ownership…" (to a member) and "Delete organization…" (confirm by name), and the role dropdown offers only `member` and `admin`.
2. **B2.** The AI credentials page has an "Everyone in <org> / Only the people ticked below" panel for an organization profile, owner only, calling `PUT /credential-profiles/{id} {access}`.
3. **B3.** Each row in an org scope shows `by <creator>`, `view only` (`access:'view'`), `private` (own, `visibility:'private'`) and `shared`. The row menu shows Make private / Share with org only for the creator, and Fork, Rename and Delete only when `access` is `owner`.
4. **B4.** Choosing an organization in the sidebar scopes the dashboard pages and the wallet of the sandboxes and keys it creates, without calling `PATCH /orgs/active`. A separate "CLI and API keys bill … Change" control sets the active wallet.

m1 and m2 are cheap and would close PARITY rows that claim more than the code does.

## Checked and fine

- **Organizations (CLI, curl, Python and TypeScript SDKs).**
  - `org create`, `switch`, `invite` (code + link), `join`, `members` (pending invites), `list`.
  - Two orgs named alike → `409 ambiguous_org`, and the id works.
  - Rename by `PATCH`: the old name stops resolving (`403 not_org_member`), the id is stable.
  - `default-visibility` read and set.
  - Sharing:
    - A teammate's `use` access: exec and files OK.
    - `view` access on a private sandbox: `info` OK; exec, ssh, history and prompt → `403 sandbox_private`; stop of a sandbox that holds logins is allowed.
    - Stop of a creator-private sandbox → `403 sandbox_private`.
    - Fork, delete, rename, extend and `stop --force` → `403 owner_only`.
    - `share` of a stopped logins sandbox gives `noEnv:true`; on resume, no logins reached the guest.
    - `share` of a running one → `restartRequired:true`.
    - `unshare` removes the teammate's SSH key (`authorized_keys` 1 → 0).
  - Caps (`PATCH /orgs/{org}/members/{id}`):
    - `concurrentCap:1` → second `new` and a fork → `429 member_limit_reached`.
    - Tiny `usageCapCredits` → `402 team_member_cap_reached`, and the sandbox was stopped.
    - After the cap was cleared, `resume` worked.
  - Environment access: `env access --only` / `--everyone`, a bad email → `400 invalid_access`, an excluded member does not see the environment and gets `403 environment_forbidden`.
  - The `shared-sandboxes.md` team recipe: org profile, `set-key`, `ai proxy on`, `env set --credential-profile`, `--org acme new --environment agents` → the sandbox got `ANTHROPIC_BASE_URL` and a placeholder key.
  - A member removed → their org sandbox is in their personal list, an org snapshot (`--org`) stays with the org, and `org switch acme` is refused for them.
  - SDKs: `list_organizations`, `set_active_organization`, `update_organization`, `sandboxes(org=…)`, `api_keys`, `get_data_retention` (Python); the same calls in TypeScript; `updateDataRetention` with a key → `403 session_required`.
- **API keys.**
  - TTL `never` and `365d` OK, `400d` → `400 invalid_ttl`, `--preset` plus `--actions` refused, no preset → `["*"]`.
  - An expiring key cannot mint a later-expiring or non-expiring key, or one with wider actions or sandboxes → `403 api_key_scope_exceeded`.
  - A `ci` key cannot create keys.
  - An environment-limited or sandbox-limited key gets `apiKeys: []`.
  - Read-only → `api_key_action_forbidden` on create and on `account.admin` routes.
  - `catalog` has `actions`, `presets`, `defaultTtl`, `maxTtl`.
  - A `ci` key's created sandbox is in `usage --verbose` (`createdResources`); `delete-own` forbids someone else's sandbox.
  - `rotate` kills the old secret; `revoke` (the key then gets 401) and `usage` still works afterwards.
  - Org key: bills its org, `api_key_org_mismatch` for another org or `personal`, `api_key_org_forbidden` for webhooks and keys.
  - The in-sandbox key is limited (`account.read` refused).
- **Data retention.**
  - ZDR:
    - `status`; a key `PATCH` → `403 session_required`; a wrong phrase → `400 confirmation_required`.
    - Enabling removed a stopped sandbox (resume → 404) and the saved snapshot.
    - A new saved snapshot → `409 zero_data_retention`.
    - A stop under ZDR removed the sandbox.
    - `GET /account/data-retention` is `Cache-Control: no-store`, and `/me` has `zeroDataRetention`.
    - `PATCH enabled:false` works.
  - `snapshotsOff` and `monitoringOff` toggles via session `PATCH`.
  - Close account:
    - A key → `403 session_required`; a wrong phrase → `400`.
    - A closed account: sandbox stopped and kept (`archived`), the key → 401, creates → `403 account_closed`, `purgeAt` 30 days out.
    - Reopen → `purged:false`, resume worked.
    - Erase → login refused afterwards.
- **Dashboard (code).** API keys page (presets, raw actions, sandbox and environment selectors, TTL presets incl. never, rotate, revoke, usage), Account page (ZDR, snapshots off, monitoring off, close, reopen, erase), Organization page (invite, members, caps, rename, default visibility, leave), environment access panel, `crowdvm dashboard` login link, `/`, `/org` and the JS modules are served.

## Not verified

- Anything rendered in a browser (layout, the actual look of B3's rows). Findings B1-B4 are from the code and the endpoints it calls.
- A second node, Windows nodes, GPU, SMTP-sent invites, real agent credentials and live agent turns (the free OpenCode model is out of quota).
- The 14-day retention job running (not waited out), the purge 30 days after close, delivery exhaustion after 8 attempts.
- e2e: not run, by instruction.

## TL;DR

DISAGREE, on four dashboard gaps. Every claimed fix holds live:

- `sandbox.hydrated` is sent once on resume and fork, and never on stop. I checked this with a receiver running Boat's own signature verifier.
- Repository clones retry until the network is up, and the ready gate waits for them (6/6 creates).
- The wallpaper recipe now turns the screen red.
- `--fail-fast` answers at once.
- A queued fork survives deleting its source.

My own sweep of orgs, keys and retention found the server, CLI and SDKs matching Boat. The only server-side gap is minor: `environment_passes_logins` is not raised for a fork or a `--no-env` resume, and no logins leak.

The dashboard is where parity breaks:

- No Transfer/Delete organization, and the role dropdown offers `owner`, which fails (`web/js/pages/org.js:68`).
- No "Who can use this profile" panel (`web/js/pages/ai.js`).
- No `view only` or `private` row labels (`web/js/pages/sandboxes.js:63`).
- The sidebar select changes the active wallet instead of just the view (`web/js/main.js:174`).

All four are UI changes on endpoints that already exist.
