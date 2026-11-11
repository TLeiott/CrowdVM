# Review sonnet-8: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`, built with `make build` at 13:51.

## Verdict: DISAGREE (narrowly: one small blocker)

Both claimed fixes hold. I reproduced each live and found no regression:

- **sonnet-7 B1** (fork ignores the source's pinned environment version): fixed.
- **opus-9 B1** (active org via `org switch`, by-name saved-snapshot actions hit the personal snapshot): fixed.

My own sweep found one documented, supportable behaviour that is still wrong. It sits on the environments surface:

**B1. After `env rename`, `info` and `list` show the old environment name on every sandbox pinned to it.**
- Boat, `cli-reference.md` (`boat env rename`): "Rename. Sandboxes stay pinned to their versions". `environments__versions.md` tells you to find a sandbox's version by comparing `boat info` (`environment`, `environmentVersion`) with `boat env list`.
- Live: `env new rn1`, `new --environment rn1` → sandbox `bx_mfxudyk7`, then `env rename rn1 rn2`.
  - `env list --json` shows `rn2` (id `f99dd353…`).
  - `crowdvm info bx_mfxudyk7 --json` still says `"environment":"rn1","environmentVersion":1`. `crowdvm ls --json` says `"environment":"rn1"`.
  - After this the user can no longer match a sandbox to a row of `env list` by name. The `versions` page's own recipe (`boat info` → `env: prod (v2)` vs `boat env list` → `prod latest v3`) gives a name that no environment has.
- Cause: the sandbox row stores a copy of the name. `internal/server/sandboxes.go:324` serves `sb.Environment`, and `sandboxes.go:93` and `:1076` load and save it. `lifecycle.go:1187` writes it at create. `feat_env.go:778` renames only the `environments` row. The sandbox's `environment_id` is already stored (`sandboxes.go:93`), so the fix is to look the name up by id when building the sandbox JSON (or to update the copy on rename).
- Supportable on a LAN: pure server bookkeeping.
- A smaller effect of the same copy: `internal/server/apikeys.go:82` also matches a key's `environments` list against `sb.Environment` (the name). Normal keys store ids, so the line only matters if a name is in the list, but it uses the same stale value.
- No test covers it: `feat_env_test.go` has no rename-then-info case.
- Fork, resume and delete are fine after a rename. A fork of a sandbox whose environment was renamed, or then deleted, stays on the source's version (`environmentVersion` 2 with `T1` present and the later `T2` absent). `env list` counts the sandbox by id.

Everything else I tried works. The minor items below do not block.

## Method

1. **Read.** `sonnet-7.md` and `opus-9.md` (findings, flip criteria, what each exercised). Then the code behind each fix: `envResolve` (`internal/server/feat_env.go:1124-1154`), `savedLookup` (`snapshots.go:635-675`), `handleFork` (`lifecycle.go:1256+`), `handleSaveSaved` / `resolveFrom`.
2. **Build and tests.** `make build` ran clean. `go vet ./...` is clean. `go test -count=1 ./...` passes (all packages `ok`; `test/e2e` skips without `CROWDVM_E2E`). I did not run e2e. I read `e2e-run8.log`: `PASS`, `ok … 632.230s`, `EXIT=0`, only `bootloop` skipped.
3. **Own cluster**, ports 9970–9979 only (server 9970, node UI 9971, a `git daemon` on 9975, a throwaway `http.server` on 9976), data in `~/.local/share/review-sonnet-8`.
   - One `crowdvm-server`, one `crowdvm-node` with the qemu driver (8 vCPU, 16 GiB, 3 sandboxes), image `ubuntu-24.04-crowdvm-v3`, fresh guest binary.
   - Users `admin`, `bob`, `carol`. Orgs `acme8`, `beta8`.
   - Later in the run I restarted the node twice (to apply `net.allow` and `direct-host`).
4. **Fix checks**, live (see the table).
5. **Independent sweep.** I started from the page-citation counts in `docs/reviews/*.md` and from what sonnet-7's "Checked and fine" and method sections already cover. I ran recipes captured, as an agent would:
   - **Environments surface** (`environments*.md`, `setup.md`):
     - registries: `NPM_TOKEN` + `.npmrc` with `${NPM_TOKEN}`, `PIP_INDEX_URL` / `pip.conf`, `UV_INDEX_ACME_*`, `.docker/config.json`, a secret file under a repo folder (`web-app/backend/.env`), file modes `600`;
     - paths: absolute and `..` secret-file paths are saved but never written in the sandbox;
     - variable rules: `BAD-KEY`, `BOAT_ID` / `CROWDVM_ID` / `ASCII_TOKEN` reserved, 100 variables, 128-char key, 64 KB total;
     - per-sandbox `-e` over an environment value; `--no-env` + `-e`; fork inherits per-sandbox env, and `fork -e` replaces it;
     - values in login shells, `bash -lc`, `sh -c`, `ssh`;
     - safe for third parties: nothing arrives, home holds no credentials, `BOAT_ID`/`ASCII_TOKEN` do; the in-sandbox CLI works only on that sandbox (every other call is `api_key_action_forbidden`); the API's `effective` and `warning` fields;
     - `--no-env` fork of a live sandbox strips `.ssh/id_*`, `.git-credentials`, configured secret files and the CLI token, keeps `~/.aws/credentials`;
     - org environments: `env new/set-var --org`, `env access --only EMAIL`, member list filtering, `environment_forbidden` for a member outside the list;
     - repositories: `env add-repo git://…`, `--branch`, `GET /repos?sync=true&q=`, `PUT /environments/{id}` with `repositories[]` and `setupScript` / `setupBlocking`;
     - repository setup scripts: run in the repo folder, in a login shell, at create, resume and fork; `set -e` (the `after` line does not run after a failure); stderr lands in `/tmp/routine-logs/<repo>.err`.
   - **Fork and versions:** fork after a version bump, `--environment base` (gets latest), `env upgrade --sandbox` then fork, fork after the environment was renamed or deleted, fork of a `--no-env` source.
   - **Agent-facing CLI (captured, `--json`, exit codes):**
     - `exec`: one-arg shell line, argv with quoting, `bash -lc`, exit-code passthrough, `--timeout 2` → 124, `--cwd` rules, `--stream --json` frames, captured `--json`, unknown flag → 2;
     - `run`: `--push`, `-e`, `--json` frames plus `summary`, `--timeout` → 124, unknown option creates no sandbox, stderr last line `exit N · …`;
     - `push` (tracked files, `.gitignore`, exec bit, symlink); `history` (`--mode`, `--limit 101` rejected, `--stderr`); `ps`; detached `--status`, `--attach`, `--kill`;
     - `ssh` forms (`"cmd"`, `-- bash -lc`, `-- bash -s < setup.sh`); `scp` single file and `--recursive` both ways;
     - `login --key-stdin`; `host`, `host url`, `--private` / `--public` token cycle.
   - **Always-on recipe:** a systemd unit, `host 3000`, stop, resume, fork. The service comes back and the hosted URL and token stay the same after resume.
   - **Boat's raw-SSH curl recipe:** `POST /sandboxes/{id}/sshkey` with my own key, then stock `ssh` to `sshEndpoint` (needs a node with `direct-host`), plus a reverse tunnel `-R 7777:…` that the sandbox could curl.
   - **Desktop and tools:** `ascii-record-desktop start/status/stop` → MP4, `scp` and `GET …/artifacts?path=` give identical bytes; Chrome on `:0`; `cua-driver` status and restart; every tool of `machines__tools.md` is on `PATH`; all seven harness skills dirs hold a skill; Docker works.
   - **Files API:** write (utf8 and base64), read, list, info, search, find (plain and regex), replace, permissions, folder, move (with and without `overwrite`), delete (with and without `recursive`), path guard.
   - **SDK:** Boat's own `parallel-agents` Python sample with `boat_sdk` imports (fork with `idempotency_key`, `wait_until_ready`, `command`, `prompt` → `provider_not_configured`).
   - **Mechanical:** every `--flag` in the table rows of `cli-reference.md` against the CLI's `--help`. All exist.
   - **Mechanical:** `crowdvm …` lines in `skills/`, `docs/guide/` and the integration READMEs, checked for command and flag. No stale ones.
6. **Teardown.** I deleted every sandbox, then stopped my `git daemon`, node and server by the PIDs I saved. I checked that nothing of mine was left: ports 9970–9979 are free and no process has my data dir in its argv. I removed `~/.local/share/review-sonnet-8`. I did not touch the other reviewer's processes (they run from a different scratchpad). I used no sub-agents.

## Fix status

"live" = reproduced on my cluster. "code" = read.

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| sonnet-7 B1 | A fork gets the source's pinned environment version | **Fixed (live + code)** | `internal/server/feat_env.go:1131-1134`: `Op == "fork"`, no `--environment`, source has an `EnvironmentID` → `return nil, nil`. The fallback at `lifecycle.go:228-230` carries the source's name, id and version. Test `feat_env_test.go:231-234`. |
| | | | Live: `base` has v1 (`EARLY=1`), source on v2. `env set-var base LATE=1` made v3 the latest. `crowdvm fork` → `environmentVersion` 2, `echo "[$EARLY] [$LATE]"` → `[1] []` (source the same). |
| | | | `fork --environment base` → v3. `env upgrade base --sandbox SRC`, then a plain fork → v3. Fork after `env rename` and after `env rm`: stays on v2, `[one] []`. Fork of a `--no-env` source: `noEnv:true`, no `NPM_TOKEN`, no `.npmrc`. |
| opus-9 B1 | By-name saved-snapshot commands under an active org (`org switch`) | **Fixed (live + code)** | `snapshots.go:657-665`: with no explicit scope, the active org's wallet comes first, then personal, then the caller's other orgs. Explicit `--org` / `?org=` / header / org key still take only that scope (`:640-655`). |
| | | | Live: `img8` saved in personal, `acme8`, `beta8` (three different snapshot ids). With `acme8` active: `snapshot info img8` → acme8's id; `--org personal` → personal's; `--org beta8` → beta's. `snapshot share img8` made a code; `shares img8` listed it, `--org personal shares img8` was empty. Re-save `snapshot ID img8` → the poll returned acme8's new id (`snap_dchf…`) and `ready`, not the personal one. `snapshot rm img8 --yes` removed acme8's only: personal and beta8 snapshots stayed. |
| Regressions | Side effects of the two fixes | **None found** | `new --from NAME` under the active org, `--org personal` and `--org beta8`; fork, resume, `env upgrade`, `--environment` all as before. Server tests pass. |

One more note on the fork fix: `envResolve` now returns early for **every** pinned source, including when the pinned environment was deleted. The fallback path handles that, and I confirmed it live (fork after `env rm` works and keeps its version).

## New findings

### Blocking

| ID | What | Evidence |
|---|---|---|
| **B1** | `env rename` leaves the old name on pinned sandboxes in `info`, `ls` and the sandbox JSON. | Boat `cli-reference.md` (`env rename`), `environments__versions.md` ("Find the version of a sandbox"). Code `internal/server/sandboxes.go:324`, `:93`, `:1076`, `feat_env.go:778`. Live details above. One-line fix: build `environment` from `environment_id`. Add a test next to `feat_env_test.go:231`. |

### Minor (do not block AGREE; most are carried over)

| ID | What | Evidence |
|---|---|---|
| m1 | `env list --json` and `env info --json` drop the `effective` and `warning` fields the API returns. `environments__access.md` names `effective` as the field that shows "what sandboxes really get". The text view of `env info` does show `no (withheld: safe for third parties)` and `set-var` prints the `warning:` line. A script that reads `boat env list --json` (as the docs' `jq` recipes do for other fields) gets `null`. Same as sonnet-7 m1/m3, still open. I would fix it together with B1. | Live: `curl $API/environments` has `effective` and `warning`; `crowdvm env info safe --json` gives `effective:null`, `warning:null`. |
| m2 | `crowdvm wallet pay`, `wallet card`, `billing portal`, `billing cancel`, `login --agentid`, `login --google`, `onboard --google` exit 2 with Cobra's `unknown command`. PARITY CLI-012/121/131 say they print a clear "not supported" message. The features are unsupportable (payments, third-party identity), so this is wording. | Live. Same as sonnet-7 m4. |
| m3 | `crowdvm env add-repo base octocat/hello-world` (Boat's own sample) fails with `unknown_repository` unless a GitHub source is connected (`repos sources add`). Boat also needs its GitHub connection (`environments__repositories.md`), so this is the same rule. A git URL works. After adding by URL, the stored name is host-prefixed (`github.com/octocat/hello-world`), so `rm-repo octocat/hello-world` says `repository_not_found`. Removing by the URL or the `databaseId` works. | Live on a throwaway environment. |
| m4 | `sshKey` returns `machineIp:null` and `sshEndpoint:null` on a node without `--direct-host`. Boat's raw-OpenSSH curl recipe then resolves to host `null`. The response carries `proxyCommand:"crowdvm tunnel ID 22"`, and with `direct-host` set the recipe works unchanged (host key pinned, `-R` reverse tunnel reached from the sandbox). Documented as ADAPTED (SSH-004/007). A product with no CLI on a node without direct-host has no stock-SSH route. Fine for a LAN where node owners set it. | Live, both modes. |
| m5 | `crowdvm-node config set net.allow …` does not apply to a running node. I saw `netstack: blocked tcp … this machine` until I restarted it. | Live. Node-local setting, not a Boat feature. Worth one doc line. |
| m6 | The in-sandbox skill is named `crowdvm`, not `boat`. Boat: "every integrated agent already has a `boat` skill". The skill directory exists for all seven harnesses plus `~/.agents/skills`. Cosmetic naming, the `boat` binary alias exists. | Live. |

## Disputed classifications

None of the PARITY rows I ran was mis-classified. `lux` (TOOL-007, ADAPTED): the only Boat text is one table line, and the in-sandbox `lux` prints a clear "not available" with the replacements (`cua-driver`, `ascii-record-desktop`). I agree it is not supportable. ENV-006 ("sandboxes pinned at start") is now true for resume, upgrade and fork. ENV-014 (rename) should note B1 once it is fixed.

## What would flip this to AGREE

1. **B1.** The sandbox JSON (`GET /sandboxes`, `GET /sandboxes/{id}`) reports the environment's **current** name, taken by `environment_id`. After `env rename a b` a pinned sandbox says `b`. Add a server test: create an environment, start a sandbox, rename, read the sandbox, assert the new name and an unchanged `environmentVersion`.
2. Nothing else blocks. m1 is cheap and I would take it in the same change.

## Checked and fine

- All of the environments recipes listed in the method section, including the registry recipes written exactly as in `environments__private-registries.md`.
- Limits: 101 variables → `invalid_env` "At most 100"; a 129-char key → rejected; 2 × 40,000 and 1 × 66,000 bytes → "exceed 64KB"; 100 variables of 10 bytes → accepted.
- `exec`/`run`/`push` exit codes and shapes as in `cli-reference.md`. `run` with `--push` copied 6 files (tracked plus untracked, not `node_modules`), kept the exec bit and the symlink, and printed `exit 4 · 4s · default · 0.0003 credits · bx_… deleted` to stderr.
- Detached processes, `history` (`--limit 101` → `invalid_limit`), `ps` (the `commandId` field and the HISTORY column).
- Always-on service survives stop, resume and fork. `host 3000` gives the same token after resume and a new one after `--public` then `--private`, as Boat says. The URL with `_token` sets the cookie and redirects.
- `ascii-record-desktop`, desktop Chrome, `cua-driver` health and restart (the commands on `integrated-agents__computer-use.md`), every tool on `machines__tools.md` present, `docker compose`, `LANG`/`LC_ALL=en_US.UTF-8`.
- Files API: all endpoints and error codes (`invalid_path`, `destination_exists`, `is_directory`).
- Python SDK: Boat's `parallel-agents` sample with `boat_sdk` imports ran unchanged against my server (same fork id on a retry with one `idempotency_key`).
- Org environment access lists, member visibility, `environment_forbidden`.
- Tests: `go vet`, `go test -count=1 ./...` and e2e run 8 (`EXIT=0`).

## Not verified

- Windows nodes, VFIO GPU passthrough, real GitHub / GitLab / Gitea hosts (`repos sources add` against a live host), real Claude/Codex credentials (`prompt` only up to `provider_not_configured`), SMTP.
- A second node: cross-node fork and resume were not exercised (my cluster had one node).
- Python / TypeScript SDK breadth beyond the one sample, the MCP server and the `integrations/` smokes: I relied on e2e run 8 and earlier reports. I did spot-check `skills/` and the READMEs mechanically.
- The dashboard UI.
- `crowdvm exec` reading stdin (`echo x | crowdvm exec ID cat` prints nothing): no Boat page promises it for `exec`, and `ssh` does stream stdin. Not counted.

## TL;DR

DISAGREE, narrowly. Both fixes hold live: a fork stays on its source's pinned environment version (`feat_env.go:1131-1134`), and under `org switch` the by-name saved-snapshot actions use the active org's snapshot (`snapshots.go:657-665`). `go test ./...` and e2e run 8 pass. One new blocker: `env rename` leaves the old name on pinned sandboxes in `info`/`ls` (`internal/server/sandboxes.go:324`, `feat_env.go:778`), so the `versions` page's "compare `boat info` with `boat env list`" recipe no longer matches. The fix is to read the name by `environment_id`, plus a test. The rest of the environments and agent-facing surface (registries, secrets and limits, safe-for-third-parties, `--no-env`, org access lists, repo setup scripts, `exec`/`run`/`push`/`history`, files API, SSH with and without direct-host, always-on services, desktop recording, Python SDK) worked as Boat documents.
