# Review sonnet-7: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. I rebuilt the server and CLI once mid-review, because `internal/server/snapshots.go` changed at 12:51 while I was reviewing (the active org now comes first in `savedLookup`). Every fix check below was re-run on that newer build.

## Verdict: DISAGREE (narrowly: one blocker, a one-line server fix)

All three claimed fixes hold. I reproduced each live and found no regression:

- **sonnet-6 N1.** `host list` prints the tokenless `(gated)` table when captured, and its JSON carries no `_token`. This holds in the sandbox, through `POST /commands` and through `boat host list` / `crowdvm host list` inside the sandbox.
- **opus-8 B1.** `--org personal` finds, shares and removes the account's own saved snapshots by name. `CROWDVM_ORG=personal` and `BOAT_ORG=personal` do the same.
- **opus-8 B2.** `--org ORG new --from NAME` deploys the org's snapshot, and `--org personal new --from NAME` deploys the personal one, when both share a name.

One documented, supportable behaviour is still wrong. It sits in a code path no earlier review exercised:

**B1. A fork gets the latest environment version, not the version its source is pinned to.**
- Boat, `cli-reference.md:621`: "Without `--environment`, the fork gets exactly the environment version of its source. A fork never gets configuration that its source did not have."
- The same sentence is the reason for `environments__versions.md`'s "a save never changes a running sandbox".
- CrowdVM's `envResolve` handles `Op == "fork"` by loading the source's environment and then taking `envLatest(e)` (`internal/server/feat_env.go:1131-1134`, `:1144`, `:1152-1153`). It keeps the source's version only when the environment was deleted.
- Live, on the rebuilt server. The source `bx_95cpe7w4` stays on `base` v10. I ran `env set-var base LATE=1`, which made v11 the latest, then `crowdvm fork bx_95cpe7w4`:

  | | `environmentVersion` | `echo "LATE=[$LATE]"` in the sandbox |
  |---|---|---|
  | source | 10 | `[]` |
  | fork | **11** | `[1]` |

- I saw the same gap earlier: source on v7, fork on v10, fork received `WHO=base`, source had none.
- Why it matters:
  - The fork receives a secret that its source never had.
  - It also gets any new repository setup script, and any `safeForThirdParties` / pass-toggle change. That is exactly the "configuration that its source did not have" Boat forbids.
  - Pinning is the whole point of environment versions (ENV-006).
- Supportable on a LAN: pure server-side bookkeeping.
- Fix: in `envResolve`, return `nil, nil` for `Op == "fork"` with no `req.Name` and a pinned source. The fallback at `lifecycle.go:228-230` already carries the source's name, ID and version. Keep today's path only when the pinned environment was deleted. Add a test next to `feat_env_test.go:238`: fork after a version bump and assert that the fork stays on the old number.
- No e2e or server test covers fork versions. `test/e2e/features_test.go:120` checks only the upgrade.

Everything else I found is minor.

## Method

1. **Read.** `sonnet-6.md` and `opus-8.md` (findings, flip criteria, claimed fixes), then the code behind each fix:
   - `internal/cli/cmd_access.go`, `internal/cli/app.go`;
   - `internal/server/snapshots.go`, `internal/server/sandboxes.go`, `internal/server/feat_env.go`, `internal/server/lifecycle.go`.
2. **Build and tests.**
   - I built all four binaries into my scratchpad with the Makefile's flags, not into `bin/`. I did not run `make build`, so the running e2e harness was left alone.
   - `go vet ./...` is clean and `go test ./...` passes (all packages `ok`).
   - I did not run e2e. I read `e2e-run7.log`: `--- PASS`, `ok … 658.713s`, `EXIT=0`. Every group passed, including `orgs`, `sdk`, `cli-fixes`, `boat-sdk-samples`, `process-history`. Only `bootloop` was skipped.
3. **Own cluster**, ports 9970–9972 only, data under `~/.local/share/review-sonnet-7`.
   - One server and one qemu/KVM node (8 vCPU, 16 GiB, 3 sandboxes), image `ubuntu-24.04-crowdvm-v3`, guest binary freshly built.
   - An org `Acme7`, one admin account, scoped API key.
4. **Fix checks, live.** All of them, on the rebuilt server and CLI (see the table).
5. **Independent sweep.** I chose pages and recipes that no earlier review cites, so I started from a count of how often each `.ref/pages/*.md` name appears in `docs/reviews/*.md`.
   - Mechanical (cheap and exhaustive):
     - every command from every `###` heading of `cli-reference.md`, checked against the CLI (`--help`);
     - every `boat …` line in every code block of the 219 pages and `llms-full.txt`, checked for the command and each `--flag`. Only the payment/OAuth ones differ (see "Checked and fine");
     - every function name in `sdks__python`, `sdks__typescript` and `sdks__overview`, checked against `sdk/python/src` and `sdk/typescript/src`.
   - Live, recipe by recipe, run as an agent would (captured, `jq` where the page uses it):
     - `environments__access`, `environments__choose`, `environments__versions`, `environments__manage`: `env new`, `set`, `set-var` (also from stdin), `set-file` (`--from` and stdin), `rm-var`, `rm-file`, `rename`, `rm`, `default`, `upgrade` (all, `--sandbox`, live and stopped), `--no-env` on `new`, `fork` and `resume`, `--environment`, and the `warning` on a safe-for-third-parties environment.
     - `machines__fail-fast`: `new` and `fork` with `--fail-fast` give `no_ready_machine` when the node is full.
     - `long-running-tasks__auto-stop`: `extend --no-auto-stop` and `--hours`.
     - `long-running-tasks__background-commands`: detached, status, offsets, stream, `exec --attach`, `--timeout 0`, `--kill`, the API `DELETE`.
     - `setup`: `--setup-file` with ok and failing scripts, `setupStatus`/`setupError` polling loops, and a resume that does not re-run it.
     - `desktop-streaming__background`: `scp` of a wallpaper, then `xfconf-query` shows it applied.
     - `desktop-streaming__browser-only`: `browser`, `--profile /data/chrome-profile`, and rejection of `/home/user/x`.
     - `windows-apps__install-wine`: Boat's `install-wine.sh` run unchanged as a detached command through the API. It finished with `wine-11.21 (Staging)` and `winetricks`; Notepad opened on the desktop.
     - `data-retention__snapshots-and-monitoring-off`: with both off, `monitoring_off` 409 on processes and metrics, and `snapshots_disabled` on save and fork.
     - `snapshots__download-and-delete`: `snapshot pull` (11,490 files), `latest`, `tree`, the file endpoint for an absolute path (`/etc/hostname`) and a folder (tar), and `saved-snapshots/<name>`.
     - `integrated-agents__conversations`, `platform-guide__agents`: `conversations`, `events`, `steer`, `interrupt`, `prompt --attach`, and the `provider_not_configured` path.
     - `jq` recipes: the `until … .sandbox.state/.hydrated` loops and `id=$(boat fork … --json | jq -r .id)`.
   - Spot checks: the docs MCP server (`tools/list`, `search_docs`), `skill`, the `integrations/` tree. Sonnet-6's sub-agent B ran those in full.
6. **Teardown.** I deleted every sandbox, stopped my server and node by PID, killed the one leftover qemu (an inspect VM of an archived sandbox, with my data-dir path in its argv), made the Go module cache writable and deleted `~/.local/share/review-sonnet-7`. Ports 9970–9979 are free. I did not touch the processes on 9960–9969 or the e2e run.
7. I used no sub-agents.

## Fix status

"live" = reproduced on my cluster. "code" = read only.

| # | Finding | Status | Evidence |
|---|---|---|---|
| sonnet-6 N1 | Captured `host list` prints `_token` URLs as JSON | **F (live)** | `internal/cli/cmd_access.go:553` calls `a.rawOutput(cmd)`. `:560` strips the query from every URL with `noToken`. The table marks at `:575-581` has `(gated)` / `(localhost)` / `(hidden)`. The JSON branch (`:562-564`) carries `url`, `hostUrl` and `pathUrl` without `?_token=`. |
| | | | Live in the sandbox: `X=$(host list)` and `host list \| cat` gave the table `3000  http://127.0.0.1:9970/p/bx_…/3000/  (gated)`. `host list --json` has no `_token` (checked `url`, `hostUrl`, `pathUrl`). `boat host list \| cat` and `crowdvm host list \| cat` match. |
| | | | `POST /sandboxes/{id}/commands {"command":"host list"}` → `stdout:"PORT  URL  TITLE\n3000 … (gated)\n"`. `{"command":"host list --json"}` → no `_token`. `host url 3000` and `host 3000 --private` still return the token URL, as Boat says. After `host hide`, `host list` says `no hosted ports` on stderr, exit 0. |
| | | | The e2e only checks that the output contains `8080` (`test/e2e/basic_test.go:561`). No test asserts the absence of `_token`. |
| opus-8 B1 | `--org personal` hides the account's saved snapshots | **F (live)** | `internal/server/snapshots.go:646-649` takes the explicit scope: an org gives the org wallet, `personal` gives `c.P.User.ID`. Test: `lifecycle_fix_test.go:388`. |
| | | | Live, with `org switch Acme7` active: `--org personal snapshot ID name` → `ready`, exit 0, no 404. `--org personal snapshot info/share/shares/rm NAME` all work. `CROWDVM_ORG=personal` and `BOAT_ORG=personal` do too. `--org personal snapshots` and `info` agree. |
| opus-8 B2 | `--org ORG new --from NAME` deploys the wrong same-named snapshot | **F (live)** | `sandboxes.go:829` passes `org` to `resolveFrom(u, from, org)`. `snapshots.go:981,995-997` try the org first, then personal, then the newest across the user's orgs. |
| | | | Live, personal `dup` has `PERSONAL` in a file and org `dup` has `ORG`. `new --from dup`: `--org personal` → `PERSONAL`, `--org Acme7` → `ORG`, no flag with `Acme7` active → `ORG` (the active wallet, as for save). Also with the active org: `snapshot info dup` → org one, `--org personal` → personal one. Share codes stay unscoped. |
| New check | Tree edit at 12:51 (active org first in `savedLookup`, `snapshots.go:660-665`) | **OK (live)** | I rebuilt, restarted and re-ran the matrix above. This also removes an inconsistency I saw on the first build: no-flag `snapshot info` returned the personal one while `new --from` chose the org one. |
| Regressions | Side effects of the three fixes | **None found** | `host`, `host url`, `host N --private/--public`, `host hide`, `snapshot rm` with and without a scope, `snapshots` listing under both scopes and `new --from` under all three scopes behave. |

## New findings

### Blocking

| ID | What | Evidence |
|---|---|---|
| **B1** | Fork without `--environment` takes the latest version of the source's environment instead of the source's pinned one. | Boat: `cli-reference.md:621`. Code: `internal/server/feat_env.go:1131-1134,1144,1152-1153`. Live: source v10 has `LATE=[]`, fork gets v11 and `LATE=[1]`. Details above. |

### Minor (do not block AGREE)

| ID | What | Evidence |
|---|---|---|
| m1 | `env info --json` and `env list --json` drop the `effective` and `warning` fields the API returns. The text view of `env info` does show the "withheld" notes, and `set-var` prints the warning on stderr. Boat: `environments__access.md:47` ("The `effective` field shows what sandboxes really get… The API answer then carries a `warning`"). `GET /environments` has both. | The CLI decodes into a typed struct (`internal/cli/cmd_env.go:74` emits `client.Environment`, with no `Effective`/`Warning` in `internal/client/types.go`). Live: `crowdvm env list \| jq '.environments[].effective'` → `null`; API `GET /environments` → `effective` and `warning` present. |
| m2 | `crowdvm info --json` omits `noEnv`, `holdsCreatorLogins`, `visibility`, `placement` and more that `GET /sandboxes/{id}` returns. After `resume --no-env` the API says `noEnv:true`; the CLI shows nothing. No Boat page promises a CLI field for these, so this is a gap in the CLI view, not in the API. | `internal/client/types.go:41` `Sandbox` struct. Live diff of `jq keys`. |
| m3 | `crowdvm env set-var`/`set-file` print `warning:` for a safe-for-third-parties environment on stderr only. In `--json` mode (captured), the JSON object has no `warning` key. Boat: "The API answer then carries a `warning`." The API does. | Live. |
| m4 | `crowdvm login --agentid`, `--google`, `onboard --google`, `billing portal`, `billing cancel --keep-disks --yes`: `unknown flag` or `unknown command`, exit 2. All are unsupportable (third-party identity, real payments), so the N/A holds. PARITY CLI-012 / CLI-131 still promise a "clear not supported" message. The text is Cobra's. | Same as sonnet-5 M5 / sonnet-6 M3. |
| m5 | No test covers `host list` without `_token`, `--org personal` snapshot names, or fork environment pinning. | `test/e2e/basic_test.go:561`; no server test for either. |

## Disputed classifications

None of the PARITY rows I ran were mis-classified. ENV-006 (immutable versions, "sandboxes pinned at start") is marked done. It is true for resume and `env upgrade`, but not for fork (B1). ENV-014 and ENV-025 do not mention fork versions, so there is no row for the fork rule. Add one when B1 is fixed.

## What would flip this to AGREE

1. **B1.** `fork` with no `--environment` stays on the source's pinned version number. Add a server test: bump the environment, then fork, then assert `environmentVersion` is unchanged and that the new secret is absent. Add one e2e step or unit test in `feat_env_test.go` next to `:238`.
2. Nothing else blocks.

## Checked and fine

- **Environments.** `--safe-for-third-parties`, the four pass toggles (`--boat-credentials` maps to `passSandboxCredentials`), `--credential-profile`, and `--proxy-ai-credentials` (400 `ai_proxy_off` without the proxy, as Boat says). `set-var` from stdin, `set-file` from `--from` and stdin, `rm-var`, `rm-file`, `rename`, `rm`, `default`. `env upgrade` (all, `--sandbox`; a live sandbox is applied at once and a stopped one on resume). `--no-env` strips `.config/gh`, `.git-credentials`, `.ssh/id_*` and the env secrets on `new`, `fork` and `resume`, one-way. `new --from NAME` takes the default environment, not the source's, and `--environment` works. A deleted pinned environment keeps its sandbox and its forks on the old version.
- **Machine and lifecycle.** `new --setup-file` (done / failed with `exit code 7`, polled with the page's loops, not re-run on resume). `extend --no-auto-stop` and `--hours`. `new --fail-fast` and `fork --fail-fast` return `no_ready_machine` when the node is full. `browser --profile` rules.
- **Commands.** `cwd` relative, `timeoutSeconds` 1–600 (601 → `invalid_timeout`), a 2 s timeout gives `timedOut:true` and `SIGKILL`. Detached: `processId`, status with offsets and `more`, `/stream`, `exec --status/--attach/--kill`, `--timeout 0` exit code passthrough (rc 3), API `DELETE`.
- **Snapshots.** Pull (11,490 files), `latest`, `tree`, file endpoint with an absolute path and a folder (tar), `saved-snapshots/<name>`. Snapshots-off and monitoring-off errors.
- **Desktop and Windows apps.** Wallpaper (applied to `xfdesktop`; note `~/.config/boat/` does not exist until you create it, the same as for Boat's own `curl -o` recipe). Boat's `install-wine.sh` installs Wine 11.21 staging; Notepad shows in `wmctrl -l`.
- **Docs and SDK surface.** Every `###` command in `cli-reference.md` exists. Every `boat …` code-block command and flag exists, except the payment/OAuth/AgentID ones in m4 and the `--boat-credentials` flag, which is hidden from `--help` but accepted. Every helper named on the SDK pages exists in both SDKs (`run_many` is a doc-local sample). The MCP server lists `search_docs`, `read_doc`, `list_docs` and answers a search.
- **Tests.** `go vet`, `go test ./...` and e2e run 7 (`EXIT=0`).

## Not verified

- Windows nodes, VFIO GPU passthrough, real Claude/Codex/Bedrock credentials (so `prompt` was only checked up to `provider_not_configured`; steer/interrupt only for the no-turn error), SMTP, real GitHub or Gitea CI.
- A second node. My cluster had one, so cross-node placement, `fork` on a full node and `--node` were not exercised beyond `no_ready_machine`.
- Python and TypeScript SDK live runs and the integrations' live smokes. I relied on e2e run 7 (`sdk`, `boat-sdk-samples` passed) and sonnet-6's report.
- The dashboard beyond what the API serves.

## TL;DR

DISAGREE, narrowly. All three claimed fixes hold on the current tree, which I rebuilt mid-review after the 12:51 `snapshots.go` edit:
- captured `host list` prints the tokenless `(gated)` table (`cmd_access.go:553-564`);
- `--org personal` and `--org ORG` resolve saved snapshots by name and `new --from` in the right wallet (`snapshots.go:646-649`, `:981-997`, `sandboxes.go:829`).

One blocker remains: a fork without `--environment` gets the latest environment version, not its source's pinned one (`internal/server/feat_env.go:1131-1134,1144`). Boat says "exactly the environment version of its source… never configuration that its source did not have" (`cli-reference.md:621`). Live: source v10 had no `LATE`, fork on v11 had `LATE=1`. It is a one-line server fix plus a test. Four minor items are listed and do not block AGREE. `go test ./...` and e2e run 7 pass.
