# Review opus-10: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (one blocker, a small guest fix)

All four claimed fixes hold. I reproduced each one live and found no regression in the paths next to them:
- **opus-9 B1:** an active org with no `--org` now drives every by-name saved-snapshot action.
- **sonnet-7 B1:** a fork stays on its source's environment version.
- **opus-9 n-1:** the OWNER column is there.
- `info --json` now carries `noEnv` and `holdsCreatorLogins`.

My sweep covered the pages earlier reviews cited least. It found one documented, supportable behaviour that is broken.

**B1. A secret file whose path starts with a repository folder stops that repository from being cloned, and nothing reports it.**
- Boat, `environments__secrets.md:107`: "To put a file inside a clone, start the path with the repository folder name." The example is `web-app/backend/.env`.
- At boot the guest writes the secret files first (`internal/guest/boot.go:107-109`) and runs the clones after that (`boot.go:203`).
- `runRepos` skips any repository whose folder already exists (`internal/guest/setup.go:318`: `if _, err := os.Stat(real); err == nil || !clone { continue }`). The secret file has just created `~/<repo>/backend/`, so the clone never runs. No repository status is set, and `setupStatus`/`setupError` stay `null`.
- The live `apply-config` path (`internal/guest/apply.go:337` writes the files, `:374` runs the repos) has the same order.
- Live, on my cluster: environment `rv10r` with repository `https://github.com/octocat/Spoon-Knife.git` and secret file `Spoon-Knife/backend/.env`.
  - With the file: `new --environment rv10r` gives `~/Spoon-Knife/` holding only `backend/.env`. `git branch` prints `fatal: not a git repository`, and `setupStatus` is `null`.
  - Control, after `env rm-file`: the same environment clones on `* main`.
  - My sub-agent found it first and saw the same thing with a second repository (`Hello-World` cloned, `Spoon-Knife` did not).
- Why it matters: this is the documented way to give a cloned app its `.env` (the "`.env` files" section of the secrets page). Following Boat's recipe silently loses the repository, and the agent then works on an empty folder.
- Supportable on a LAN: it is ordering inside the guest. Fix options:
  - write secret files after the clones, or
  - clone when the folder is missing or holds only managed secret files (clone into a temp dir, then move the files in).

  Add a guest test: a repository plus a secret file under its folder, then assert that `.git` and the file both exist.

## Method

1. **Read.**
   - `opus-9.md` and `sonnet-7.md` (blockers, n-items, flip criteria).
   - The code behind each claimed fix:
     - `internal/server/snapshots.go` (`savedLookup` `:638-669`, `handleSaveSaved` `:790`, `resolveFrom` `:981`);
     - `internal/server/orgs.go` (`Ctx.Scope` `:101-141`);
     - `internal/server/feat_env.go` (`envResolve` `:1126-1155`);
     - `internal/server/lifecycle.go` (fork `:1336-1405`, fallback `:229`);
     - `internal/cli/cmd_snapshot.go`, `internal/client/types.go`.
   - The tests: `lifecycle_fix_test.go:398-404` (active-org lookup) and `feat_env_test.go:239-243` (fork version).
2. **Build and tests.**
   - I built all four binaries into my scratchpad with the Makefile's flags (guest for linux/amd64).
   - `go vet ./...` is clean and `go test ./...` passes.
   - The tree changed while I worked: `feat_env.go`, `feat_env_test.go` and `client/types.go` at 14:14, adding env `effective`/`warning` to the CLI JSON (sonnet-7 m1/m3). `envResolve` and `savedLookup` were unchanged. `go build ./...` and the targeted server tests still pass on the newer tree.
   - I did not run e2e. `e2e-run8.log` ends `ok … 632.230s`, `EXIT=0`, with no `FAIL`. `bootloop` was skipped.
3. **Own cluster**, ports 9960 (server) and 9961 (node UI) only, data under `~/.local/share/review-opus-10`.
   - One qemu/KVM node (8 vCPU / 16 GiB / 4 sandboxes), image `ubuntu-24.04-crowdvm-v3`, guest binary freshly built.
   - Users `alice` (admin) and `bob`, org `acme10`.
4. **Fix checks, live** (table below).
5. **Independent sweep.** I counted how often each `.ref/pages/*.md` name appears in `docs/reviews/` and `docs/gaps/`, then took the least-cited pages.
   - **Mechanical:** all 122 operations in `boat-v1.yaml` against the server's routes. The 25 not found by literal route are the guest passthrough prefixes (`internal/server/passthrough.go:28-29`), so none are missing.
   - **Me, live:**
     - `desktop-streaming__urls-and-security`: `?theme=light`, `overlay=0`, list and info carry no `desktopUrl`.
     - `platform-guide__patterns`: `ssh ID "host 3000 --private"`, `usage ID`, `delete`, saving a stopped sandbox.
     - `recipes__parallel-agents`: `id=$(fork … --ttl 7200 --json | jq -r .id)` gives exactly one id, `ttlSeconds:7200`.
     - `hosting__how-it-works`: `--localhost` relay to an app on `::1` only.
     - `long-running-tasks__always-on-services`: `ssh -- bash -s < setup.sh` installs a systemd unit. After stop/resume it is `active`, and `host url 8088` is byte-identical before and after.
     - `platform-guide__templates-and-sizing`: `new --from NAME --no-env -e TENANT_ID=acme` from a deleted source.
     - `cli-reference` `new … --host 3000 --host 8080:public --file workspace/.env=./f`: `hostedPorts` in the answer, file written.
     - `api-keys__scopes`, in-sandbox key: save snapshot OK; `fork`, `api-key list` and `env set-var` give typed 403s.
     - `organizations__seats-and-limits`: per-member caps, code read.
   - **Sub-agent 1** (Opus, live, ran the real upstream adapters): `integrations__agentenv`, `integrations__eve`, `integrations__harbor`, `integrated-agents__steering`, `integrated-agents__computer-use`. No blockers.
   - **Sub-agent 2** (Opus, live, local OTLP receiver on :9965): `opentelemetry`, `environments__secrets`, `environments__repositories`, `data-retention__zero-data-retention`, `data-retention__close-account`, `snapshots__resume-and-fork`. Found B1, which I then reproduced myself.

## Fix status

"live" = reproduced on my cluster.

| # | Claim | Status | Evidence |
|---|---|---|---|
| opus-9 B1 | With `org switch ORG` and no `--org`, by-name saved-snapshot actions use ORG's snapshot | **F (live)** | `snapshots.go:656-665`: the active org comes first after the explicit-scope branch (`:640-654`), then personal, then other orgs. This matches `handleSaveSaved` (`c.Scope`) and `resolveFrom` (`:981`). Test: `lifecycle_fix_test.go:398-404`. |
| | | | Live, with personal `img10` (marker `PERSONAL`) and acme10 `img10` (marker `ORG`), then `org switch acme10`, then `crowdvm snapshot bx_… img10` with marker `ORG2`:<br>• save: printed `ready` with the **org's** new `snapshotId` (`snap_mcuma…`), so the CLI poll found the org row.<br>• `snapshot info img10`: org; `--org personal snapshot info img10`: personal (`snap_us79…`).<br>• `snapshot share img10`: the share shows under `--org acme10 snapshot shares`, and `--org personal` shows `[]`.<br>• `unshare` revoked it.<br>• `new --from img10` deployed `ORG2`.<br>• `snapshot rm img10 --yes` deleted the **org** one; the personal one stayed (`snapshot list` shows one `personal` row). |
| sonnet-7 B1 | Fork without `--environment` keeps the source's environment version | **F (live)** | `feat_env.go:1133-1136` returns `nil` for `Op=="fork"` with a pinned source, so `lifecycle.go:229` carries the source's name, id and version. Test: `feat_env_test.go:239-243`. |
| | | | Live: source on `e10` v2 (`LATE=[]`), then `env set-var e10 LATE=1` (v3), then `fork`. The fork is on **v2** with `LATE=[]`. `fork --environment e10` → v3. `fork --no-env` → v2, `noEnv:true`, `holdsCreatorLogins:false`. |
| opus-9 n-1 | OWNER column in the saved-snapshot list | **F, cosmetic (live)** | `cmd_snapshot.go:149` and `:66` (in `snapshots`). It prints the org **id** (`team_e5w8ac0rbc6qafytk1y2`), not `acme10`: `client.SavedSnapshot` (`types.go:585`) drops the `orgName` the server sends. `--json` has `org` (id) only. Minor (m4). |
| sonnet-7 m2 | `info --json` has `noEnv`, `holdsCreatorLogins` | **F (live)** | `types.go:48-49`. `crowdvm info` shows both, e.g. the `--no-env` fork above. |
| Neighbours | resume, `env upgrade`, saved-snapshot list | **No regression (live)** | Stop then resume of the v2 source stays v2 (`LATE=[]`). `env upgrade e10 --sandbox <fork>` → v3 live, `LATE=[1]`. The saved-snapshot list shows both same-name rows. `new --from` under personal scope deployed `PERSONAL`. In-sandbox `boat snapshot $BOAT_ID insb` worked. |

## New findings

### Blocking

| ID | What | Evidence |
|---|---|---|
| **B1** | A secret file under a repository folder (Boat's documented `.env`-in-a-clone recipe) stops that repository from being cloned, with no error | Boat `environments__secrets.md:107`. CrowdVM `internal/guest/boot.go:107-109` then `:203`, skip at `internal/guest/setup.go:318`. Same order in `apply.go:337`/`:374`. Live: with the file, `fatal: not a git repository`; without it, `* main`. Details above. |

### Minor (do not block AGREE)

| ID | What | Evidence |
|---|---|---|
| m1 | opus-9 n-4 is still open: `~/.config/boat/` does not exist, so all four of Boat's wallpaper recipes fail (`desktop-streaming__background.md:13,20,26,34`). The guest watches that path (`internal/guest/desktop.go:540-541`). | Live: `crowdvm scp wp.png bx_…:/home/user/.config/boat/wallpaper.png` → `dest open … No such file or directory`, rc 1. `~/.config/ascii/boat` → `../crowdvm` exists, so the same kind of link or `mkdir` at boot fixes it. This is the second round it has been reported. A one-line fix; I'd fix it with B1. |
| m2 | opus-9 n-5 is still open: `/openapi/boat-v1.yaml` → 404 (`sdks__overview.md:242-243`). `/openapi/crowdvm-v1.yaml` → 200. | Live. |
| m3 | Without `--localhost`, an app bound only to `::1`/`127.0.0.1` still answers on its hosted URL. Boat says it does not (`hosting__how-it-works.md`, "An app on localhost … does not answer"). | Live: `python3 -m http.server --bind ::1 5174`, `host 5174 --private` (no `--localhost`) → 200. More permissive than Boat, not a missing feature. |
| m4 | The OWNER column and `snapshot list --json` show the org id, not the name | See the fix table. |
| m5 | Stale comment: `savedLookup` doc (`snapshots.go:635-637`) still says "else personal first". | Code. |
| m6 | (sub-agent) `env add-repo base owner/name` → `400 unknown_repository` unless the repository is in a synced host listing (`internal/server/feat_env_repos.go:404-417`). A git URL works. ENV-027/ENV-030 are ADAPTED, but a `https://github.com/owner/name` fallback is cheap. | Live (sub-agent). |
| m7 | (sub-agent) OTel `lastError` is never cleared after a successful export, and log exports don't update `lastExportAt` (`internal/server/feat_otel.go:363-370,408`). `telemetry set` prints `test export: ok=true <nil>` (`internal/cli/cmd_admin.go:249`). | Live (sub-agent). |
| m8 | (sub-agent) `BOX_ID`/`BOX_CLI_TOKEN` are not reserved env names (`internal/server/sandboxes.go:354-355`), and `BOX_ID` is not exported in the sandbox (`feat_env_guest.go:149`). Boat's renamed-names page says both spellings are exported. | Live (sub-agent): `env {"BOX_ID":"x"}` accepted; `echo $BOX_ID` → `x`. |
| m9 | (sub-agent) `crowdvm events --convo` prints tool entries as `[tool] <nil>` (`internal/cli/cmd_agent.go:297` reads `m["name"]`; entries are `{use:{name},result}`). | Live on a TTY (sub-agent). |

## Disputed classifications

- **ENV-021** (`PARITY.md:356`, REQUIRED, done): "secret files written under /home/user at relative path" is true. But the secrets page's "file inside a clone" case breaks the clone (B1). Add the ordering to the row, or a row under ENV-027.
- **DESK-013** (`PARITY.md:238`): still not done as documented (m1), same as opus-9.
- **SDK-009** (ADAPTED): the reason is branding only (m2), same as opus-9.
- I accept every N/A row (26). They are all payments, third-party identity, operator facts or documented absences. The ADAPTED rows I touched (URL-001/007, ENV-027/028/030, ORG-019/021, OTEL-004/005/006, EVAL-004/005, API-137) hold in their adapted form.

## What would flip this to AGREE

1. **B1:** a repository is cloned even when an environment secret file's path starts with its folder name, and the file is then present inside the clone. Fix this on boot (`boot.go`) and on the live `apply-config` path (`apply.go`). Add a guest or server test for it.
2. Nothing else blocks. I'd fix m1 (create `~/.config/boat`) at the same time, since all of Boat's documented wallpaper commands fail today.

## Checked and fine

- **Integrations (sub-agent, live, real upstream code):**
  - AgentEnv 0.9.1496 `boat` provider: create with smallest size, `noEnv`, `snapshots:false`, `ttlSeconds`, labels, `hostedPorts`; label filters; detached commands and stream; signed upload URL; `X-Ascii-Confirm-Delete`; resume of a snapshots-off sandbox → `409 snapshots_disabled`.
  - `@asciidev/eve-box` 0.1.6 and `harbor-box` 0.1.2 through `/api/box/v1`, including the refusals, `/workspace`, `stop(delete=True)` → `409 confirmation_required`. Box headers, envelope types, `X-Box-Org`, `box_` keys.
- **Steering and computer use (sub-agent, live):**
  - `409 no_running_turn` with `reason:no_turn`; 404/400 cases; OpenCode fallback steer within the same turn.
  - `cua-driver.service` and the socket; `computer` registered in all harness configs; 26 tools; it survives stop/resume and keeps the user's own MCP entries.
- **OpenTelemetry (sub-agent, live):** PUT/GET/DELETE `/telemetry`, suffix stripping, test export, all 9 gauges and resource attributes, log records with severity and attributes, retry with `Retry-After` on 503, drop on 400, 10-minute backlog, delete drops the queue.
- **Secrets, ZDR and close account (sub-agent):**
  - Vars and files per version; absolute and `..` paths skipped; per-sandbox env merges over the environment's; every `invalid_env` limit.
  - ZDR: `session_required`, `confirmation_required`, `zero_data_retention` on save (turned on and off on bob only).
  - Close account: `browser_session_required` for a CLI session.
- **Mine, live:** the desktop URL and iframe options; patterns 1 and 3 (`ssh … host 3000 --private`, `usage`, `delete`, save of a stopped sandbox); the `fork --json | jq -r .id` recipe; the systemd always-on recipe with the same URL after resume; template deploys with `--no-env -e` from a deleted source; create with `--host`/`--file`; in-sandbox key scopes.

## Not verified

- Windows nodes, GPU passthrough, real Claude/Codex/Bedrock credentials, SMTP, `--direct-host`, the dashboard.
- Saved-snapshot by-name actions for a non-admin member with an active org (same `savedLookup` path, with `savedManage` on top; inferred).
- A fork whose source has no `environmentId` (every new sandbox gets `base` today, so I could not produce one).

## Teardown

- I deleted every sandbox I created, my saved snapshots (`img10` personal and org, `user-42`, `insb`) and the `rv10r` environment. Both sub-agents deleted theirs: sub-agent 1 confirmed 404s, and sub-agent 2 removed its environment and telemetry export.
- I stopped my node and server by PID. One leftover inspect VM from my node (`crowdvm-insp_6xgxsa7x3anj8gfv`, my data dir in its argv) I killed by PID.
- I deleted `~/.local/share/review-opus-10` and my scratchpad binaries. Ports 9960–9969 are free.
- I touched no other process (the server on 9970 belongs to the other reviewer) and joined no other server.
- I edited no repo file except this report.

## TL;DR

DISAGREE, with one blocker. All four claimed fixes hold live:
- with an active org, by-name saved-snapshot actions (save, info, share, unshare, rm, `new --from`) hit the org's snapshot;
- a fork stays on its source's environment version;
- the OWNER column is there, though it shows the org id;
- `info --json` has `noEnv`/`holdsCreatorLogins`.

Resume and `env upgrade` didn't regress. New blocker: Boat's documented "secret file inside a clone" (`web-app/backend/.env`) stops the clone. The guest writes secret files before cloning and skips any repository whose folder exists (`internal/guest/setup.go:318`, order at `internal/guest/boot.go:107,203`). It fails silently, and I reproduced it live. The minor items include opus-9 n-4 and n-5, both still open.
