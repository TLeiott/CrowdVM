# Review opus-13: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (one blocker, a regression from the B4 dashboard fix)

All five claimed fixes hold:
- opus-12 B1, live on my own cluster and in `apikeys_test.go`;
- sonnet-10 B1–B4, in code. B4's saved-snapshot side is checked live through the dashboard's own session and header.

`--local-login` leaked nothing in my live test. I ran Claude Code against a mock Anthropic API inside the sandbox with a fake token, and the agent ran a real tool call. The token did not reach the tool's environment, the sandbox disk, process history, the server or the node.

My sweep found one blocker. It is a regression introduced by the B4 fix:

**B1. In the dashboard, the Saved snapshots page lists every wallet's saved snapshots, but its row actions now resolve the name only in the viewed organization.**
- Viewing **Personal**, "Share codes…" and "Delete…" on an organization's team image return `404 snapshot_not_found`. Viewing the organization, the same actions on a personal snapshot fail the same way.
- **Cause.**
  - B4's fix sends `X-CrowdVM-Org` on every dashboard request (`web/js/api.js:17-21`).
  - `savedLookupAny` treats that header as an explicit scope and looks only in that wallet (`internal/server/snapshots.go:649-663`). It is used by info, delete, shares list/create/revoke (`:804`, `:897`, `:924`, `:944`, `:970`).
  - `handleListSaved` ignores the header and lists the personal account plus every organization (`snapshots.go:773-777`).
  - `web/js/pages/snapshots.js:34` shows that whole list. `:69`, `:78` and `:83` then call the name endpoints with no per-row wallet.
  - Before the header existed, the unscoped lookup fell back across wallets, and these actions worked.
- **Live, my cluster.** Session cookie plus `X-CrowdVM-CSRF`, exactly as `api.js` sends them:
  - `GET /saved-snapshots` with `X-CrowdVM-Org: personal` listed `rv-org` (org `acme13`), `rv-a` and `rv-b`;
  - `GET /saved-snapshots/rv-org/shares` with `personal` → `404 snapshot_not_found`;
  - `DELETE /saved-snapshots/rv-org` with `personal` → `404`, and with the org id → `deleted`;
  - `GET /saved-snapshots/rv-a/shares` with the org id → `404`, and with `personal` → `200`;
  - the same calls with no header (the old dashboard) → `200`.
- **What Boat says.**
  - `organizations__active-wallet.md:72`: the Organization select "Picks the organization of every page: the sandboxes, **snapshots**, keys and billing shown".
  - `snapshots__templates.md:211`: the member who saved a team image, or an org owner/admin, "can remove it or make a share code".
  - So the page should show only the viewed wallet's snapshots, and every row it shows must be removable and shareable.
- **Why it is supportable on a LAN:** it is a pure server and UI scoping fix, with nothing external involved.
- **Fix (either):**
  - `handleListSaved` lists only `c.Scope("")`'s wallet when the request names one (`?org=`, header, org key). Unscoped API/CLI callers keep the cross-wallet list.
  - Or the page passes `?org=<row.org || 'personal'>` on each row action.
  - Add a server test with the header.

The sub-agent swept the integrated-agents pages live. It ran turns through scripted stand-in `claude` and `opencode` binaries on the guest's PATH, with no AI credential. It found no blocker. I checked its four minor items in code (m4–m7).

## Method

1. **Read.**
   - `opus-12.md` and `sonnet-10.md`: blockers, flip criteria and coverage.
   - Citation counts of every `.ref/pages/*.md` name in `docs/reviews/`, to choose under-reviewed pages.
   - The fix code:
     - `internal/server/snapshots.go:636-680`, `:764-890`;
     - `internal/server/sandboxes.go:826-840`, `internal/server/apikeys.go:70-91`;
     - `web/js/api.js`, `web/js/main.js:55-125`, `:195-210`;
     - `web/js/pages/org.js:55-135`, `web/js/pages/ai.js:100-125`, `web/js/pages/environment.js:19-40`;
     - `web/js/sbx.js:339-385`, `web/js/pages/sandboxes.js:55-75`;
     - `internal/cli/locallogin.go`, `internal/cli/cmd_agent.go:98-115`;
     - `internal/guest/agents/run.go:615-720`, `harness.go:99-180`, `:295-350`;
     - `internal/server/acct_prompt.go:95-170`, `test/e2e/features_test.go:134-200`.
2. **Build and tests.**
   - Built all four binaries into my scratchpad with `CGO_ENABLED=0` (not `bin/`).
   - `go vet ./...` is clean, and `go test` on every package except `test/e2e` passes (rc 0, 19 packages ok).
   - I did not run e2e. I read `e2e-run13.log`:
     - `EXIT=0`;
     - `agent/codex` and `agent/claude` PASS on `--local-login`;
     - `agent/opencode` and `bootloop` SKIP;
     - `boat-sdk-samples` 56/56.
3. **Own cluster.**
   - Ports:
     - 9960: server, with the e2e harness flags and `--webhook-allow-local`;
     - 9961: node UI.
   - Data in `~/.local/share/review-opus-13`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated.
   - One qemu/KVM node: 8 vCPU, 16 GiB, 4 sandboxes, `--snapshot-interval 60s`, image `ubuntu-24.04-crowdvm-v3`.
   - Users `alice` (admin) and `bob`, org `acme13`.
   - No real AI login was used anywhere.
4. **Fix checks** (table below).
5. **Independent sweep.**
   - **Me, live:**
     - the viewed-org header against every saved-snapshot route;
     - `snapshots__download-and-delete`: files by relative path, folder as tar, absolute `/etc` path, tree, saved snapshot by its `snapshotId`;
     - `long-running-tasks__always-on-services`: enabled unit back after stop/resume, same `host` URL and token, `--no-auto-stop` kept;
     - `environments__private-registries`: `NPM_TOKEN` + `.npmrc` with a literal `${NPM_TOKEN}`, mode 600, and `pip.conf`;
     - Wine image preconditions: `xdotool`, `wmctrl`, the ondrej PHP repo with its `libgd3`, `dl.winehq.org` reachable. The full install is covered by sonnet-2, sonnet-7 and opus-11.
   - **Me, automated:**
     - every `boat …` line in a code block of all 219 pages, parsed and mapped onto `crowdvm`'s real command tree, with every flag checked against that command's `--help`;
     - every request-body property of every operation in `boat-v1.yaml` checked against the server's JSON field names.
   - **Sub-agent (Opus, live on my cluster; the fake harness cannot be turned on from the sandbox env, so it put scripted stand-in `claude`/`opencode` binaries on the guest PATH):** `integrated-agents__conversations`, `integrated-agents__steering`, `integrated-agents__computer-use`, `platform-guide__agents`, `recipes__agent-on-repo`.

## Fix status

"live" = verified on my cluster.

| # | Claim | Status | Evidence |
|---|---|---|---|
| opus-12 B1 | Scoped keys no longer reach other sandboxes' saved snapshots | **F (live)** | Code: `keySeesSaved` (`snapshots.go:764-771`) filters list (`:792`), info, delete and shares through `savedLookup` (`:639-645`); overwrite checks the existing name's source (`:844-847`); deploy (`sandboxes.go:829-839`). Test: `apikeys_test.go:103-117`. |
| | | | Live: key `--actions sandbox.create,sandbox.read,snapshot.read,snapshot.write,exec --sandbox <A>`, with `TOPSECRET13` saved from B as `rv-b`: |
| | | | list → `['rv-a']` only; info `rv-b` → 404; deploy `{"from":"rv-b"}` → 404; deploy by the underlying `snap_…` id → 404; overwrite `rv-b` with A → `403 api_key_sandbox_forbidden`; shares list/create → 404; delete → 404; download of B's snapshot → 403; fork of B → 403. |
| | | | In scope: info and deploy of `rv-a` → 200. The in-sandbox key of A lists only `rv-a`, and `rv-b` → 404. |
| | | | Share code: alice made a `bsh_…` code for `rv-b`. bob deployed it and `cat ~/secret.txt` → `TOPSECRET13`. The scoped key can also deploy with the code, as a bearer grant (see Disputed). |
| sonnet-10 B1 | Organization tab: transfer, delete, role dropdown | **F (code)** | `org.js:70-71`: options are only `member` and `admin`, and the owner's row is not a select. `:98`, `:113-124`: "Transfer ownership…" to a member, then `POST /orgs/{id}/transfer`. `:100-101`, `:126-129`: "Delete…" with type-to-confirm, then `DELETE /orgs/{id}`. |
| sonnet-10 B2 | AI credentials "Who can use this profile" | **F (code)** | `ai.js:121-122` renders `accessCard` for an org profile, with `PUT /credential-profiles/{id} {access}`. It is the same component as the environment panel (`environment.js:19-40`). |
| sonnet-10 B3 | `by` / `view only` / `private` / `shared` on rows, and actions by access | **F (code)** | `sandboxes.js:61` shows `· by <creator>` for others' sandboxes, and `:62-63` shows `shareLabels` (`sbx.js:346-353`). The menu (`sbx.js:356-385`): Fork, Auto-stop, Rename and Delete are disabled unless `access=owner`; Share/Make private only for the creator; desktop, console and resume need `use`. |
| sonnet-10 B4 | Viewing vs. CLI/API-keys wallet are separate | **F (code + live), with regression B1** | `main.js:84-111`: the view is kept in `localStorage` and sent as `X-CrowdVM-Org`; it never calls `/orgs/active`. `main.js:113-121`, `:203-209`: a separate "CLI and API keys bill: X · Change" is the only `PATCH /orgs/active`. |
| | | | Live with the header: `GET /orgs` `active` flags unchanged; `GET /sandboxes/<personal id>` while viewing the org → 200; webhooks are not scoped. Environments (`environments.js:13-16`) and keys (`keys.js:28-30`) follow the view. Saved snapshots do not (B1). |
| `--local-login` | Per-prompt local token, no leaks, correct precedence | **Holds (live, fake token)** | I sent `POST /sandboxes/A/prompt {"provider":"claude","credentials":{"CLAUDE_CODE_OAUTH_TOKEN":"sk-ant-oat01-FAKE…","ANTHROPIC_BASE_URL":"http://127.0.0.1:8765"}}` to a mock Messages API inside A. The mock answered with a `Bash` tool_use running `env | grep …OAUTH_TOKEN… > /tmp/toolenv.txt`. |
| | | | The mock saw `Authorization: Bearer sk-ant-oat01-…`, so the request token won. The tool ran (`tool_result "ok"`), and `/tmp/toolenv.txt` was **empty**: Claude Code kept its token out of tool commands, as `cli-for-agents.md` says. |
| | | | Scans with the needle read from a file: 0 hits in `/home/user`, `/var/lib/crowdvm`, `/tmp`, `/var/log`, `/root` and `/etc`, in `history`, in the server data dir and logs, and in the node data dir. The one raw hit in A's qcow2 was my own 22-byte needle file (preceded by zeros, not by `sk-ant-oat01-`). |
| | | | Code: `promptRec.env` is memory-only (`agents.go:215`); `requestCredsWin` (`run.go:679-707`); Codex gets `features.shell_snapshot=false` on every turn and `shell_environment_policy.exclude` for request creds (`harness.go:334-342`). `features.shell_snapshot` is a real feature in the image's `codex-cli 0.167.0`, `stable true` by default, so the override matters. Claude Code 2.1.319's own snapshot writes only `PATH`, functions and aliases. |

## New findings

### Blocking

| ID | What | Evidence |
|---|---|---|
| **B1** | Dashboard Saved snapshots: the list shows every wallet, but Delete and Share codes resolve only in the viewed org, so cross-wallet rows answer 404 | `web/js/api.js:21`, `internal/server/snapshots.go:649-663` vs `:773-777`, `web/js/pages/snapshots.js:34`, `:69`, `:78`, `:83`. Live: viewing Personal, `DELETE /saved-snapshots/rv-org` → `404`. Boat `organizations__active-wallet.md:72`, `snapshots__templates.md:211`. |

No other blocker. The sub-agent's sweep of the integrated-agents pages found none.

### Minor (do not block AGREE)

| ID | What | Evidence |
|---|---|---|
| m1 | Still open from opus-12 m1 / sonnet-10 m1: no `409 environment_passes_logins` for a fork, or for a resume of a `--no-env` shared sandbox, onto a login-passing environment. No logins leak. | `internal/server/feat_env.go:1150-1152` (only `Op=="resume"`, and `holds` is false when `NoEnv`). |
| m2 | The Webhooks, Account and Telemetry pages do not say that they "are the same in every organization". Boat (`organizations__active-wallet.md:75`): "The page header says so". The behaviour is right; only the note is missing. | `web/js/pages/webhooks.js`, `account.js` (no such text). |
| m3 | The e2e `--local-login` turns tell the agent "Do not use any tools", so the suite never exercises a tool call on a request credential. My mock run covers Claude Code; Codex's tool path is untested. | `test/e2e/features_test.go:153`. |

| m4 | (sub-agent, code-checked) In a shell with no current conversation, a bare `crowdvm prompt` sends neither `new` nor `conversationId`, so the guest continues the sandbox's current conversation (`internal/guest/agents/http.go:91-92`). The CLI still prints `(new)` / `"new":true` (`internal/cli/cmd_agent.go:148`, `:159-160`). Boat's page contradicts itself: "Every later prompt continues it" (`integrated-agents__conversations.md:8`) vs "two shells … each one keeps its own thread" (`:41-42`). The spec's "omit both = most recent" matches the server. The wrong label is a client fix. | Sub-agent, live: shell Y printed `5fd6231c… (new)` for shell X's conversation. |
| m5 | (sub-agent, code-checked) With no `provider` and no account default, the server checks credentials for `claude` (`acct_prompt.go:179-183`), but the guest runs the conversation's last harness (`http.go:94-99`). A bare follow-up on an OpenCode conversation can get `409 provider_not_configured` for Claude Code. Boat: an omitted provider means the default harness (`integrated-agents__harnesses-and-models.md:5`). | Sub-agent, live. |
| m6 | (sub-agent, code-checked) `prompt --attach` images are not sent inline to Claude Code, only as "Attached files: <paths>" (`run.go:332-334`). Codex, pi and ACP harnesses do get them inline (`acp.go:97`, `harness.go:286`). Boat's guide says `--attach` "sends images inline to vision models" (`platform-guide__agents.md:21`). The image still reaches Claude through its Read tool, so I rate this minor. Stream-json image blocks would close it. | — |
| m7 | (sub-agent) Interrupt also marks the conversation's *queued* prompts `interrupted` (`http.go:458-461`). Boat says only that it "stops one turn" (`integrated-agents__conversations.md:76`), so confidence is low. | Sub-agent, live. |

## Disputed classifications

- **ORG-024 / DASH-012 / ORG-007** (B4): true for sandboxes, keys, environments and billing; not yet for saved snapshots (B1).
- **Scoped key + share code.** A key limited to sandbox A can deploy a share code of a saved snapshot whose source is outside its scope (`sandboxes.go:833`: "a share code is its own grant"). I accept it. A share code is a bearer secret that another account can deploy too (`snapshots__templates.md:218-224`), so holding it is the grant. Boat's scope table names saved snapshots, not codes.
- **404 vs 403 for out-of-scope saved snapshots.** opus-12 suggested `403 api_key_sandbox_forbidden`; the code answers `404 snapshot_not_found` for info and deploy, and 403 only for overwrite. Boat names no code for this case, and 404 hides existence. No dispute.
- I accept the N/A rows the CLI sweep touched:
  - `wallet pay`, `billing cancel --keep-disks`, `billing plan --move-personal` (no payments; `org migrate` exists);
  - `login --google` and `--agentid` (third-party sign-in).

## What would flip this to AGREE

1. **B1:** with `X-CrowdVM-Org` (or `?org=`), the Saved snapshots page shows only the viewed wallet's saved snapshots, and Delete and Share codes work on every row it shows. Add a server test for `GET /saved-snapshots` with the header.

2. Nothing else blocks. m1, m4 and m5 are cheap and would close rows that claim more than the code does.

## Checked and fine

- **CLI surface.** Every `boat …` example in all 219 pages maps onto an existing `crowdvm` command path, and every flag exists. That includes the hidden Boat aliases `--boat-credentials` and `convos`. The only misses are the N/A billing and OAuth flags above.
- **API request bodies.** Every request property of every operation in `boat-v1.yaml` is a field the server reads.
- **Snapshot files** (`snapshots__download-and-delete`):
  - `files?path=projects/app/.env` → the file;
  - `files?path=projects/app` → a tar with `.env` and `readme`;
  - `files?path=/etc/rvtest/conf` → `SYSFILE`;
  - `files?path=/home/user/projects/app/.env` → the file;
  - `tree` → JSON entries;
  - a saved snapshot downloads by `snapshot.snapshotId`.
- **Always-on services:**
  - Boat's systemd unit, adapted to `python3 -m http.server`, was `active` again after stop → resume;
  - `host 8080 --private` gave the same URL and `_token` before and after;
  - `extend --no-auto-stop` → `archiveAfter:null`, kept across the resume.
- **Private registries:**
  - `env set-var NPM_TOKEN` and `env set-file .npmrc` (stdin, literal `${NPM_TOKEN}`): the new sandbox has the variable and the file, mode 600, owner `user`;
  - `pip.conf` → `pip config list` shows the index.
- **Wine preconditions** in the image: `xdotool`, `wmctrl`, the ondrej PHP repo whose `libgd3` is the candidate (the reason for Boat's pin), `DISPLAY=:0`, `dl.winehq.org` → 200.
- **Share-code path:** create, then a cross-account deploy carries the data.

- **Integrated agents** (sub-agent, live with stand-in harness binaries):
  - the prompt errors `400 prompt_required`, `400 invalid_provider`, `409 provider_not_configured`, `new`+`conversationId` → 400, `404 conversation_not_found`;
  - `400 fast_mode_not_supported`, and `fast` reaching Claude as `--settings {"fastMode":true}`;
  - `GET /prompts/{id}` statuses, also while stopped;
  - the events cursor loop, `type` and `conversation` filters, `pageInfo`;
  - the conversation list fields; a harness switch carries the transcript; a model switch keeps the session; conversations survive a stop;
  - steer: native, `native-continued` and fallback ("Continue what you were doing…"), plus `409 no_running_turn`;
  - interrupt by conversation or all; the computer-use tools; `GET /provider-models`;
  - every CLI flag in those pages' examples.

## Not verified

- The dashboard rendered in a browser. B1–B4 are checked in code. B1 (mine) is reproduced with the dashboard's own session cookie, CSRF and org headers, not by clicking.
- `--local-login` with Codex on a tool call. I also could not check precedence when the sandbox already holds a ChatGPT `~/.codex/auth.json` and the request sends `OPENAI_API_KEY` without a base URL. Testing that needs real OpenAI endpoints, which I did not call. It is a CrowdVM extension, not a Boat feature.
- A real subscription turn (I used no real login, by instruction), Windows nodes, GPU, a second node, SMTP.
- A live `org migrate`: it would have moved the sub-agent's sandbox.
- e2e: not run, by instruction.

## Teardown

- I deleted every sandbox I and the sub-agent created (alice's and bob's lists are empty), and the saved snapshots `rv-a`, `rv-b`, `rv-dl` and `rv-org`.
- I stopped my node and server by their saved PIDs, then a leftover snapshot-inspection qemu (`insp_…`) whose disk was under my data dir.
- No qemu process references my data dir. Nothing listens on 9960–9969.
- I deleted `~/.local/share/review-opus-13`.
- I touched no other process, did not run e2e, and edited no repo file except this report. I used one sub-agent.

## TL;DR

DISAGREE, on one blocker. It is a small regression from the B4 dashboard fix:

- The Saved snapshots page lists every wallet's saved snapshots.
- The new `X-CrowdVM-Org` header makes Delete and Share codes look only in the viewed org, so rows from the other wallet answer 404.

Everything claimed holds:

- Scoped keys can no longer list, read, deploy, overwrite or share other sandboxes' saved snapshots, and share codes still work.
- The four dashboard gaps are fixed in code.
- `--local-login` leaked nothing in a live Claude Code tool call with a fake token.

My CLI and API-spec sweeps and the sub-agent's integrated-agents sweep found only minor issues.
