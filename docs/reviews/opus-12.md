# Review opus-12: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (one blocker, a small server-side fix)

All six claimed fixes hold, and I checked each one live on my own cluster:
- opus-11 B1–B5;
- sonnet-9 B1.

I found no regression next to them.

My sweep found one documented, supportable behaviour that is wrong, and it is a security boundary.

**B1. A scoped API key can list, read and deploy saved snapshots of sandboxes outside its scope.** Through such a deploy it can read those sandboxes' files.

- **What Boat says.**
  - `api-keys__scopes.md:9-12`: a request is granted only when the action is in the key **and** "the target sandbox is in the sandbox set of the key, or in one of its environments."
  - `:16-23`:
    - "Create a sandbox from a saved snapshot | `sandbox.create` for the destination, and `snapshot.read` access to the source";
    - "Replace a saved snapshot | Access to its existing source, and access to the sandbox that you save";
    - "A destination environment does not give access to snapshots from other environments."
- **Code.**
  - `internal/server/sandboxes.go:826` checks only that the key holds the `snapshot.read` action. `resolveFrom` never checks the saved snapshot's source sandbox against the key's sandboxes or environments.
  - `handleListSaved` (`internal/server/snapshots.go:755`) and `handleGetSaved`/`savedLookup` (`:783`, `:639`) do not filter by key scope.
  - Automatic snapshots *are* filtered (`snapshots.go:428-433`, `AllowsSandbox`), and so is the snapshot download (live: `api_key_sandbox_forbidden`).
  - `handleSaveSaved` (`:791`) checks `snapshot.write` on the sandbox being saved (`:795`), not on the existing name's source. A scoped key can therefore overwrite a saved snapshot it cannot see.
  - No PARITY row covers this table. KEY-015 (`PARITY.md:496`) states the general rule.
- **Live, my cluster.**
  1. As alice I wrote `TOPSECRET` to `~/secret.txt` in `bx_5paqw5rr` and saved it as `rv-secret`.
  2. I created a key limited to a different sandbox: `--actions sandbox.create,sandbox.read,snapshot.read,exec --sandbox bx_ayswxrmk`.
  3. With that key:
     - `GET /sandboxes/bx_5paqw5rr` → `403 api_key_sandbox_forbidden` (correct);
     - `GET /snapshots` → only `bx_ayswxrmk` (correct);
     - **`GET /saved-snapshots` → `[{"name":"rv-secret","sourceSandboxId":"bx_5paqw5rr"}]`**;
     - **`POST /sandboxes {"from":"rv-secret",…}` → `bx_a7av97x2`**. The new sandbox counts as created by the key, so the key can `exec` there, and **`cat ~/secret.txt` → `TOPSECRET`, exit 0**.
  4. The in-sandbox key of `bx_jv85amjz` (which Boat limits to its own sandbox, KEY-017) also lists `rv-secret` and its source. Its `create` is refused (`api_key_action_forbidden`), so that path leaks metadata only.
  5. The sub-agent found the same thing independently with an environment-scoped key (`sub-e1`).
- **Why it is supportable on a LAN:** it is a pure server authorization check, with nothing external involved.
- **Fix:**
  - for a restricted key, filter `handleListSaved`/`handleGetSaved` and `resolveFrom` by `AllowsSandbox(source, "snapshot.read")` on the saved snapshot's `source_sandbox_id`;
  - in `handleSaveSaved`, require `snapshot.write` on the existing name's source as well;
  - add a server test next to the existing scope tests.

## Method

1. **Read.**
   - `opus-11.md` and `sonnet-9.md`: blockers, minor items, flip criteria and what each covered.
   - The citation counts of every `.ref/pages/*.md` name in `docs/reviews/`, to choose under-reviewed pages.
   - The code behind each fix:
     - `internal/guest/boot.go:138-146`, `internal/guest/setup.go:302-410`;
     - `internal/guest/desktop.go:576-612`, `:668-694`;
     - `internal/client/client.go:317-318`, `sdk/typescript/src/client.ts:137`, `sdk/python/src/crowdvm/client.py:137-139`;
     - `internal/server/snapshots.go:262-290`, `internal/server/lifecycle.go:328-353`;
     - `internal/server/lifecycle_secure.go:253-281`, `:416-424`.
2. **Build and tests.**
   - Built all four binaries into my scratchpad with `CGO_ENABLED=0` (not `bin/`).
   - `go vet ./...` is clean, and `go test ./...` passes (rc 0).
   - I did not run e2e. I read `e2e-run12.log`: 34 groups PASS, `agent` and `bootloop` SKIP, `EXIT=0`.
3. **Own cluster.**
   - Ports:
     - 9960: server, with `--webhook-allow-local` and the e2e harness flags;
     - 9961: node UI;
     - 9962: my webhook receiver;
     - 9965: `git daemon` on `192.168.178.31`.
   - Data in `~/.local/share/review-opus-12`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated.
   - One qemu/KVM node: 8 vCPU, 16 GiB, 4 sandboxes, `--snapshot-interval 60s`, `net.allow 192.168.178.31/32`, image `ubuntu-24.04-crowdvm-v3`.
   - Users `alice` (admin) and `bob` (for the sub-agent).
4. **Fix checks, live** (table below).
5. **Independent sweep.**
   - **Me, live:**
     - `api__v1` (562 lines, cited twice before): quick path verbatim, envelope, pagination, idempotency, `/limits`, and every one of its 113 endpoints matched against the routes;
     - `desktop-streaming__browser-only`, `desktop-streaming__urls-and-security`, `desktop-streaming__background`;
     - `snapshots__resume-and-fork`, `machines__virtualization`, `readiness`;
     - `sdks__typescript`: every `boat.*`/`sandbox.*` method used in a TS sample of any page exists on CrowdVM's `BoatApi`.
   - **Sub-agent (Opus, live on my cluster as `bob`/`alice`):**
     - `organizations__seats-and-limits`, `organizations__shared-sandboxes`;
     - `api-keys__store-rotate-revoke`, `api-keys__scopes`;
     - `data-retention__zero-data-retention`, `data-retention__close-account`;
     - `opentelemetry`, `environments__choose`;
     - `platform-guide__patterns`, `platform-guide__templates-and-sizing`, `recipes__parallel-agents`.
     - It found B1, which I reproduced with a different key shape, plus m1–m4.

## Fix status

"live" = verified on my cluster.

| # | Claim | Status | Evidence |
|---|---|---|---|
| opus-11 B1 | Repository clones survive a network that is not up yet | **F (live)** | `setup.go:343-366` retries a failed clone 5 times (2, 4, 8, 16, 32 s). |
| | | | Forced live: I stopped the git daemon, ran `new --environment rs`, and restarted the daemon 8 s later. The guest journal shows `clone failed (… Connection refused), retrying in 2s`, then `… in 4s`, then success. The node logged "waiting for the environment's repositories before ready" and became ready 6.5 s later, with `~/app/.git` and `README.md` in place. |
| | | | Three normal creates: all cloned before ready, and `setup.json` repos `status:"done"`. |
| opus-11 B2 | Secret file in the repository folder: ready waits for the clone | **F (live)** | `boot.go:141` now tests `<repo>/.git`. |
| | | | Env `rs` = `app.git` + secret file `app/backend/.env`. All 4 creates logged the gate wait before `ready=true`, and right after ready `~/app` held `.git`, `README.md`, `backend/app.py` and `backend/.env` (`SECRET=xyz`, mode 600, owner `user`). |
| opus-11 B3 | Wallpaper recipe changes the desktop | **F (live)** | `desktop.go:592` calls `adoptSessionBus` (`:670`), which reads `DBUS_SESSION_BUS_ADDRESS` from the running `xfdesktop`/`xfce4-session`. |
| | | | `crowdvm scp red.png ID:/home/user/.config/boat/wallpaper.png` turned three sampled pixels `#FF0000` within 5 s, and the full screenshot showed a red desktop. |
| | | | Deleting the file with `DELETE /files` brought back the default (`srgb(0,170,222)`). A non-PNG file keeps the default. |
| | | | After stop → resume the desktop came back red. |
| opus-11 B4 | `--fail-fast` answers at once in the clients | **F (live)** | `client.go:318`, `client.ts:137`, `client.py:137-139` skip retries on `no_ready_machine`. |
| | | | Node full (4/4): `new --fail-fast` 4 ms (JSON and text), `fork --fail-fast` 4 ms, Python `boat_sdk` `create(fail_fast=True)` 8 ms, TS `create`/`fork` 34 ms / 3 ms. All `503 no_ready_machine`, and the sandbox count stayed the same. |
| | | | `resume --fail-fast` with room: 202. My first two tries returned 503 while the sub-agent's parallel forks filled the node. I could not reproduce it afterwards, so it is not a finding. |
| opus-11 B5 | A queued fork survives deletion of its source | **F (live)** | GC root for waiting forks (`snapshots.go:273-276`); fallback to the source's head (`lifecycle.go:332-347`). |
| | | | I wrote `MARK-B5` in `bx_28cfasu4` and waited for a periodic snapshot. With the node full, I forked it (`bx_xwnqxyum`, `cloning`) and deleted the source. The server logged `live snapshot failed, forking from the last snapshot … vm not found`. The fork became `ready` and `cat ~/mark.txt` → `MARK-B5`. |
| sonnet-9 B1 | `sandbox.hydrated` on restore, not on stop | **F (live)** | `lifecycle_secure.go:273-278` (`restoredBoot`: resume, fork, saved, share); the publish in `afterSnapshotStored` is gone. |
| | | | Receiver log over 4 stops, 4 resumes and 2 forks: each stop → `sandbox.archived` only. Each resume and each fork (running source and queued fork) → `sandbox.ready` + one `sandbox.hydrated` in the same second, with `data` = `{sandbox}` only. |

## New findings

### Blocking

| ID | What | Evidence |
|---|---|---|
| **B1** | Scoped keys bypass scope on saved snapshots: list, get, deploy (then exec on the copy) and replace | `internal/server/sandboxes.go:826`, `internal/server/snapshots.go:755`, `:783`/`:639`, `:791-795`. Live: a key limited to `bx_ayswxrmk` read `TOPSECRET` from `bx_5paqw5rr` via `rv-secret`. Boat `api-keys__scopes.md:9-23`. |

### Minor (do not block AGREE)

| ID | What | Evidence |
|---|---|---|
| m1 | (sub-agent, code-checked by me) A resume or fork of a shared no-logins sandbox onto an environment that passes logins should return `409 environment_passes_logins` (`organizations__shared-sandboxes.md:83`; ORG-015). For a `--no-env` sandbox the check can never fire: `noEnv` is forced from the sandbox first (`lifecycle.go:1182`, check at `feat_env.go:1151`). Fork has no check. The requested environment is ignored silently; no logins leak. | Code; live by the sub-agent (`resume`/`fork {"environment":"base"}` → 200). |
| m2 | (sub-agent) `crowdvm list` text has no `by <creator>` / `view only: holds X's logins` labels (`organizations__shared-sandboxes.md:99-100`). The JSON has `createdBy`/`access`. | `internal/cli/cmd_sandbox.go:290-296`. |
| m3 | (sub-agent) The `403 sandbox_private` message does not name the creator (`organizations__shared-sandboxes.md:142`). Wording only. | `internal/server/sandboxes.go:200`. |
| m4 | (sub-agent) OTel `boat.account.sandboxes.active` counts by `COALESCE(org_id, owner_id)` and misses org-billed sandboxes, while the per-sandbox series include them. | `internal/server/feat_otel.go:455`, `ratelimit.go:187`. |
| m5 | The in-sandbox key (own sandbox only, KEY-017) can list every saved snapshot of the account, with names and source ids. It is part of B1's fix. | Live, from inside `bx_jv85amjz`. |
| m6 | `sandbox.hydrated` is sometimes delivered before `sandbox.ready`, in the same second (3 of 6 restores). Boat says hydrated comes "after `sandbox.ready` … can arrive at the same moment". Integrators who key on order could stumble. | Receiver log. |
| m7 | The Python SDK README still says `503 no_ready_machine` is retried (`sdk/python/README.md:341`); the code no longer does. Docs only. | — |
| m8 | A permanently failing repository URL now holds `ready` for about 62 s (the 5 retries) before the clone is reported `failed`. Boat documents no timing for this; it is the cost of the B1 fix. Retrying only on connect errors would avoid it. | `setup.go:360`. |

## Disputed classifications

- **KEY-015** (`PARITY.md:496`, REQUIRED, done): not true for saved snapshots (B1). Boat's "Snapshots and environments" table (`api-keys__scopes.md:16-23`) has no PARITY row; add one.
- **ORG-015** (`PARITY.md:521`): true for environment upgrades and for resume of a non-`--no-env` shared sandbox; not for `--no-env` sandboxes or fork (m1).
- `machine_not_running` vs `sandbox_not_ready` for commands to a stopped sandbox: Boat's pages disagree. `readiness.md:27` says `sandbox_not_ready`, while `api__v1.md:141` and `troubleshooting.md:169` say `machine_not_running` for a stopped sandbox. CrowdVM follows the API page (`passthrough.go:167-170`). No dispute.
- `nested:false` in sandbox JSON while `/dev/kvm` works: a CrowdVM-only request field, not in Boat's spec. No dispute.
- I accept LIFE-018's adaptation (`~/.crowdvm/playbook.json` instead of `~/.ascii/playbook.json`; it is a performance aid).
- I accept the sub-agent's out-of-scope list (seat purchase, plan tables, OTel HTTPS-only/public-address rule relaxed for LAN collectors).

## What would flip this to AGREE

1. **B1:**
   - With a key restricted to sandboxes or environments:
     - `GET /saved-snapshots` lists only saved snapshots whose source is in scope;
     - `GET /saved-snapshots/{name}` and `POST /sandboxes {"from":name}` return `403 api_key_sandbox_forbidden` for an out-of-scope source;
     - `POST /saved-snapshots` onto an existing name needs access to that name's current source.
   - The in-sandbox key sees only its own sandbox's saved snapshots.
   - Add server tests.
2. Nothing else blocks. m1 and m7 are cheap and close rows that claim more than the code does.

## Checked and fine

- **Quick path (`api__v1.md`):**
  - `POST /sandboxes` then an immediate `POST /commands`: held until ready, then `exitCode 0` after 4 s.
  - `ok`/`type` envelope and `X-Request-Id`.
  - Pagination:
    - `limit`, `sort=asc`, `pageInfo{nextCursor,hasMore,limit}`;
    - the cursor gives the next item;
    - `limit=500` → `invalid_limit`.
  - Idempotency:
    - same key and body → same sandbox (also after ready);
    - same key with a different body → `409 idempotency_key_reused`.
  - `GET /limits` `canStart`.
  - All 113 endpoints in the `api__v1` table are routed (25 through the guest passthrough).
- **Browser-only view:**
  - `browser ID --profile /data/chrome-profile` → `browserUrl`, `expiresAt` +10 min, `profile`.
  - Chrome runs as `crowdvm-browser` on its own `Xvnc :2` with `--user-data-dir=/data/chrome-profile`, `-AcceptCutText=0 -SendCutText=0`, and the directory is unreadable to `user`.
  - `/home/user/p` and `/etc/p` → `400 invalid_profile`; a relative path is refused by the CLI.
- **Desktop URLs:**
  - `?theme=light` is kept, and `viewerUrl`, `wsUrl` and `expiresInSeconds:600` are present.
  - A key without `desktop` → `403 api_key_action_forbidden` for desktop and browser.
  - The viewer strips the fragment (`web/js/pages/desktop.js:9-13`), supports `overlay=0` (`:21`), and sends the token as a WebSocket subprotocol, never in a URL (`web/js/vnc.js:7-18`).
  - List and info responses carry `desktopUrl:null`.
- **Resume and fork:**
  - `resume --type large` on a running sandbox → `409 sandbox_running_stop_first`.
  - A fork keeps the source's per-sandbox env (`FOO=src BAR=keep`); `fork -e FOO=mine` replaces it (`BAR` empty).
  - The fork TTL defaults to 3600 s, and `--ttl 600` works.
  - The `--setup-file` script did not run again in the fork (one marker line).
  - `snapshots ID` lists `base` + `incremental` with `origin:"captured"`.
  - `snapshot pull SNAP -o ./old` → `home_user/…` (11,480 files); `pull bx_… --path /home/user/setup-marker` → that one file.
- **Nested virtualization:** `test -e /dev/kvm && echo "KVM available"` prints it; `user` is in group `kvm`.
- **TypeScript SDK:** every method used in Boat's TS samples exists on `BoatApi`. The only two misses are Eve adapter methods (`run`, `writeTextFile`).
- **Sub-agent, live:**
  - API key lifecycle: rotate keeps the id, scope and expiry; revoke is immediate; usage still reads after revoke; the `api_key_expired` paths.
  - Presets and the action catalog, and the scope errors.
  - ZDR read and its session/confirmation guards; close, reopen and erase guards.
  - OTel set/get/unset, the test export, header rules, metric names and units, log records.
  - Shared sandboxes: `owner_only`, `sandbox_private` after unshare, `org default-visibility`.
  - Per-member caps (`member_limit_reached`, `team_member_cap_reached`); org environments and profiles.
  - Setup and templates (`--setup-file` once, `new --from … --no-env -e`).
  - The parallel-agents recipe: `fork --ttl 7200 --json | jq -r .id`, idempotent fork, `host 3000 --private` with `?_token=`.

## Not verified

- Live agent turns (OpenCode free model out of quota).
- Windows nodes, GPU passthrough, a second node (so no lazy cross-node chain or `sandbox.hydrated` with `lazy:true`), SMTP, browser-session-only flows.
- Turning ZDR on live (it would delete bob's sandboxes).
- `429 rate_limited` and the `RateLimit` headers: my server ran with start and IP limits off, as the e2e harness does.
- m1–m4 beyond the sub-agent's live checks and my reading of the cited code.

## Teardown

- I deleted every sandbox I created (`ls` empty, node `sandboxes:0`), the saved snapshot `rv-secret` and my webhook.
- The sub-agent reports it deleted its sandboxes, saved snapshot, keys, environments, org and telemetry export.
- I stopped my server, node, git daemon and receiver (no matching processes, nothing listening on 9960–9969), and removed `~/.local/share/review-opus-12`.
- I did not touch other reviewers' processes or the 9970–9979 cluster.
