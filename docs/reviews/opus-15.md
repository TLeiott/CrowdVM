# Review opus-15: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. No sub-agents.

## Verdict: DISAGREE (two blockers)

All four claimed fixes work in the case each was written for. I checked each one live. But the paused-node fix went one step too far and broke a neighbouring path.

- **B1 (regression from the sonnet-12 B1 fix).** A paused node now counts as a node that can *never* host a sandbox. If the only node big enough for a size is paused, `new`, `resume --type` and `fork --type` of that size get a final `409 size_unavailable` ("No node in this pool can host a large sandbox"). Boat's answer is to wait for a machine. Before the fix this case queued. The same node turned *off* still queues (202), so a paused node is now treated as worse than an offline one. The new server test asserts the wrong behaviour.
- **B2.** `org delete` deletes an organization that still has other members. Boat allows it only for "the owner and the last member" (`billing__plan-changes.md:90`). Live, it removed bob's membership without a word and left the org's environments in the database with no owner who can reach them.

My sweep of 15 random, lightly reviewed Boat pages found nothing else that blocks. Thirteen pages pass every recipe I could run. The two billing/trial pages are N/A as classified.

## Method

1. **Read.** `opus-14.md`, `sonnet-12.md`, the fix code, `TestPausedNodeAndImpossibleSize`, `TestCIRunnerSwap`. I also counted how often earlier reviews cite each Boat page.
2. **Build and tests.**
   - I built the four binaries into my scratchpad with `CGO_ENABLED=0`. I skipped `make build` because the team's e2e run15 was running from the repo at the time.
   - `go vet ./...` is clean.
   - `go test -count=1` on all 18 packages except `test/e2e` passes.
   - I did not run e2e.
3. **Own cluster.**
   - `crowdvm-server` on `192.168.178.31:9960`.
   - Node `n1` on UI port 9961 (qemu/KVM, 6 vCPU, 12 GiB, 3 sandboxes, image `ubuntu-24.04-crowdvm-v3`, `net.allow 192.168.178.31/32`).
   - A second node `n2big` on UI port 9962 (16 vCPU, 32 GiB nominal). I used it only to be a paused big node. No VM ever booted on it.
   - All data in `~/.local/share/review-opus-15`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated.
   - Users `admin`, `bob`, `carol`, `dave`. Orgs `acme`, `zeta`.
4. **Sweep.** I took the 50 non-API pages that earlier reviews cite 5 times or fewer. I left out the areas the last rounds covered (desktop, Windows, CI, machines, limits, auto-stop). Then I drew 15 with `shuf --random-source=<(yes 1515)`. I read each page top to bottom and ran its CLI and curl recipes. For `sdks__typescript` I ran the page's TS samples live through a copy of `sdk/tools/doc_samples.py`.
5. **Teardown.**
   - I deleted every sandbox and stopped both nodes and the server by saved PID.
   - One snapshot-inspector qemu (`insp_…`, in my data dir) outlived the node, as VMs do by design. I killed it by PID.
   - Afterwards nothing listened on 9960–9969 and no process used my data dir. I deleted the data dir.
   - I did not touch the other reviewer's cluster (9970/9971) or the team's e2e.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| sonnet-12 B1 | Paused node gets no placements; `failFast` answers 503 | **Fixed, with a regression (B1)** | The node sends `Paused` in its info (`internal/node/node.go:186`). `SetPaused` marks the info dirty (`ui.go:74-81`). `config set paused` on a running node goes to the daemon (`cmd/crowdvm-node/main.go:506-512`). `pickNode` skips a paused node and names it (`internal/server/scheduler.go:193-195`). Live, once the server knew: `new --fail-fast` → `503 no_ready_machine` in 4 ms with no sandbox row, and `resume --fail-fast` → 503 with the sandbox still stopped. Window: the info travels with the next heartbeat (`HeartbeatInterval = 10s`, `node.go:41`). A `--fail-fast` sent 3 s after `config set paused true` still got 202, then `sandbox_cancelled … node is paused by its owner`, and the start counted (`starts.minute.used` went to 1). See m1. |
| sonnet-12 B2 | Impossible size fails fast instead of a silent 15-minute retry | **Fixed (live)** | `neverFitsErr` (`lifecycle_place.go:81-102`) is used by create (`sandboxes.go:971-977`), resume (`lifecycle.go:1243-1244`) and fork (`lifecycle.go:1432-1438`). Live on a 6 vCPU pool: `crowdvm new --type xlarge --json` → exit 1 in 6 ms, `409 size_unavailable`, "Available sizes: small, default", `retryable:false`. Same for `large`. `crowdvm limits` agrees. The client does not retry a 409 (`internal/client/client.go:318`). |
| opus-14 B1 | CI acts on the sandbox whose runner ran the job | **Fixed (code + test)** | `ghJob.RunnerName` (`feat_ci.go:249`). `in_progress` re-points `sandbox_id` to `crowdvm-<sb>` (`:816-818`). `completed` re-points it before the OOM check, the `/debug` keep and the delete (`:845-848`). A 404 deletes only when no other running job uses that sandbox (`:805-807`). `TestCIRunnerSwap` (`feat_ci_test.go:343-377`) swaps the runners of two same-label jobs, finishes 2001 first, and checks that the sandbox still running 2002 survives. It passes. Gitea runners carry the same `crowdvm-<sbID>` name (`:709`). One edge case is left, see m2. Not live (no GitHub). |
| team | Concurrent first creates on a fresh account | **Fixed (live), untested** | `envEnsureBase` takes a per-owner lock around count-and-mint (`feat_env.go:214-226`). Live, on fresh accounts with no call before the burst: carol sent 6 parallel `failFast` creates and dave sent 6 parallel plain creates. All 12 got their expected answer (dave: six 202s). The DB holds exactly one `base` v1 per owner. There is no regression test for the race (`grep` finds none). |
| e2e | run14 / run15 | **run14 predates the fixes; run15 passed** | run14 ended 18:10. The fix files are dated 18:11 to 18:23 (`feat_ci.go` 18:11, `sandboxes.go` 18:17, `lifecycle_place.go`/`scheduler.go`/`feat_env.go` 18:23). run15 started 18:24 and ended `EXIT=0` at 18:48, with 37 PASS and SKIP only for `agent/opencode` and `bootloop`. No e2e test covers a paused node or `size_unavailable`. |

## Blocking findings

### B1. A paused node counts as "can never host" (SIZE-011, `snapshots__resume-and-fork.md:136`, `limits.md:99`)

- **Boat.** "A resume or a fork waits for a machine when none is free." When Boat is out of machines it answers `503 out_of_capacity`, "for a short time … Retry" (`limits.md:99`). A size is never reported as impossible.
- **CrowdVM.**
  - `couldEverPlace` skips `n.Info.Paused` (`internal/server/lifecycle_place.go:65`). It still counts an offline node ("online or not", `:57-58`).
  - So when the only node big enough is paused, create (`sandboxes.go:971`), resume (`lifecycle.go:1243`) and fork (`lifecycle.go:1432`) all go to `neverFitsErr`. That returns a final `409 size_unavailable`, `retryable:false`.
  - `sizeAvailability` skips paused nodes too (`scheduler.go:288`), so `crowdvm limits` says "no node in the pool is big enough for large".
- **Live.**
  - `n1` was 6 vCPU and active. `n2big` was 16 vCPU and paused.
    - `crowdvm new --type large` → `409 size_unavailable`, "No node in this pool can host a large sandbox … Available sizes: small, default".
    - `crowdvm resume <stopped default> --type large` → the same 409. The sandbox stays stopped.
  - Then I stopped `n2big`'s process, so the node was offline and no longer paused. `POST /sandboxes {"type":"large"}` → `202`, queued with `waitingForCapacity`.
  - So an owner who pauses their big node, for example to play a game for an hour, tells every agent that `large` does not exist. The skill then tells the agent to drop to a smaller `--type`. An owner who turns the machine off gets the right behaviour.
  - The two `large` samples of `sdks__typescript` (#12, #14) failed on my cluster with this 409, while `n2big` sat paused.
- **The test encodes it.** `TestPausedNodeAndImpossibleSize` (`internal/server/lifecycle_fix_test.go:961-972`) starts a paused node with the default fake capacity: 16 vCPU and 64 GiB (`servertest/fakenode.go:124`), so `large` fits on it. It adds a 4 vCPU node and asserts that `large` gets `409 size_unavailable`. A paused node that can host `large` should make `large` wait, not fail.
- **Related, same cause, not blocking on its own.** When every node is paused, `couldEverPlace(small)` is false too, so `neverFitsErr` hands back the scheduler's 503 (`out_of_capacity`, `reason: no_matching_node`, "No online node matches the placement requirements"). No sandbox is queued. The CLI retries it silently for up to 15 minutes. That is Boat's own `out_of_capacity` contract, so I accept it. But before the fix, a plain `new` on a paused node queued (sonnet-12 said so), and the message is wrong: the node is online and matches, it is paused. `crowdvm limits` reports "no node is online" for every size in this case (`scheduler.go:285`).
- **Why supportable.** It is a one-line change. Drop `n.Info.Paused ||` from `couldEverPlace` and treat a paused node like an offline one: it may come back. `pickNode` already keeps placements off it, so `failFast` stays correct. Keep `sizeAvailability`'s per-size reason, but say "paused" instead of "not big enough" or "no node is online". Then fix the test to expect a queued 202 for `large` while the big node is paused.

### B2. `org delete` ignores other members (`billing__plan-changes.md:90`)

- **Boat.** "Optional: delete the organization with `boat org delete`. You must be the owner and the last member."
- **CrowdVM.** `handleDeleteOrg` checks only for the owner role and running sandboxes (`internal/server/orgs.go:324-333`). Then it deletes the org row (`:334-347`).
- **Live.**
  - Org `acme` had owner `admin` and member `bob` (joined with an invite code). It held org environments `base` and `agents`, with a variable on `agents`.
  - `crowdvm --org acme org delete --yes` → `{"deleted":true,"id":"acme"}`.
  - Bob's `org list` afterwards shows only `Personal`.
  - `environments` still holds `team_3rzr…|base` and `team_3rzr…|agents`, with no org left to reach them.
  - Boat's rule exists so that the owner cannot pull the shared org out from under other members. Shared resources include environments, saved snapshots ("leaves → org keeps", SNAP-022) and credential profiles.
- **Why supportable.** Add a count of `org_members` other than the caller to the check and answer `409` (for example `org_has_members`, "remove the other members or transfer the org first"). Also delete or archive the org's environments, profiles and saved snapshots in the same transaction instead of leaving them orphaned.

## Minor (not blocking)

- **m1. Pausing takes up to 10 s to reach the server.** In that window a `failFast` create gets `202` and ends in `cancelled` with the start counted, which is the original sonnet-12 B1 symptom. Fix: send a heartbeat with info right after `SetPaused` (`ui.go:74-81`), or have the server refund the start when a node refuses with "paused".
- **m2. CI: a completed job with an empty `runner_name` still deletes the sandbox booted for it** (`feat_ci.go:845-866`). This happens when a queued job is cancelled before any runner took it. If GitHub had already given that sandbox's runner another job, that job dies. The 404 branch guards against this with `runsOtherJob` (`:805`). The `completed` branch does not. It needs GitHub to assign a newer job first and then cancel the older one while it is still queued, which is rare. Fix: when `RunnerName` is empty, delete only if `!ci.runsOtherJob(sb, j.ID)`.
- **m3. `origin: "template"` is never produced.** Boat says the snapshot list of a snapshots-off sandbox created from a source shows the source's snapshots with `origin:"template"` (`data-retention__snapshots-and-monitoring-off.md`, spec enum at `boat-v1.yaml:3553-3556`). Live, `new --from base1-saved` with account `snapshotsOff` gave `snapshots: []`. `snapJSON` hardcodes `"captured"` (`internal/server/snapshots.go:393`). It only affects a listing, but `docs/PARITY.md:611` (DATA-010, REQUIRED) claims it works.
- **m4. An absolute secret-file path is accepted and never written.** `crowdvm env set-file base /home/user/.secretcfg` succeeds, and `env info` lists it. The server prefixes `/home/user/` (`internal/server/feat_env_guest.go:176`) and no file appears. Boat's spec says the path is "relative to the workspace" (`boat-v1.yaml:5797`). The server should answer 400 `invalid_path` or strip a `/home/user/` prefix.
- **m5. `docs/guide/node-owners.md:130-131` still says to restart after `config set paused`.** It now applies live.
- **m6. `crowdvm-server join-token create` prints `--server http://<ip>:8420`** even when the server runs on another port (here 9960). It is an offline command and cannot know, but it could read the stored public URL.
- **m7. The skill's error table does not mention `size_unavailable`** (`skills/crowdvm/SKILL.md:210` lists only `no_ready_machine`). "a xlarge" in the message.

## Sweep results (15 random pages)

| Page | Result | What I ran |
|---|---|---|
| `ssh-access` | Pass (ADAPTED as SSH-004) | `crowdvm ssh` with a command and with `-- bash -s < setup.sh`. `exec --cwd`. `scp` file, `-r` folder, and download. `forward --remote 3000 --local 9965` (200) and `--reverse` (200 from inside). Boat's curl `sshkey` recipe returns `machineIp`/`sshEndpoint` null on a node without `--direct-host`, so the recipe's `ssh user@null` fails. The response carries `proxyCommand`, and plain OpenSSH with my own key, the pinned host key and `ProxyCommand="crowdvm tunnel …"` works. PARITY SSH-004 documents this. Files: `PUT /files` relative path, `/etc/x` → `400 invalid_path`, a 6 MiB JSON body → `400 file_too_large`, `upload-url` with `ttlSeconds` (7 MB PUT, then download and `cmp` equal), PUT to a download URL → 405, `ttlSeconds:7200` → `400 invalid_ttl`, `artifacts?path=` 200. |
| `data-retention__snapshots-and-monitoring-off` | Pass except m3 | `--no-monitoring --no-snapshots`: `processes`, `metrics`, `command-history` → `409 monitoring_off`. Fork and save → `snapshots_disabled`. Stop erases it, and resume → `snapshots_disabled`. `PATCH /account/data-retention` with a `cvm_` key → `403 session_required`. With a CLI session it works. Account `monitoringOff` + `monitoring:true` → `409 monitoring_off_for_account`. A fork of a monitoring-off sandbox has monitoring off, and `monitoring:true` on the fork turns it on. Account `snapshotsOff` makes new sandboxes `snapshots:false`. |
| `environments__access` | Pass | Safe-for-third-parties: a `set-var` warning, `effective` all false, a new sandbox gets `[]` for vars and no CLI token, secret files or repos, but keeps `BOAT_ID`, `ASCII_TOKEN` and the `~/.claude`, `~/.codex`, `~/.config/opencode` defaults. `resume --no-env` removed `id_ed25519`, `.git-credentials`, `gh/hosts.yml`, the Codex and Claude creds, the CLI token and the secret files, and kept `known_hosts`, `.netrc`, `.npmrc`, `.aws/credentials`. `fork --no-env` kept `authorized_keys`, and SSH still worked. `ASCII_TOKEN` → 403 on another sandbox, 200 on its own. `--proxy-ai-credentials` with the proxy off → `400 ai_proxy_off`. Boat's `--boat-credentials` flag works as an alias. |
| `environments__manage` | Pass | `env new/rename/default/list/rm/upgrade`. `--environment nosuch` → `400 unknown_environment` with nothing created. `--environment staging` on new, fork and resume. `upgrade` moved a live sandbox (v2→v3, new value seen). A soft-deleted env keeps its pinned sandbox running and refuses new ones. Org envs: `?org=acme` selects them and the `X-Boat-Org` header does not. A member lists them but gets 403 on POST and PUT. The same name in personal and org: the creator's own env wins (admin `[mine]`, bob `[orgval]`). |
| `environments__private-registries` | Pass | `NPM_TOKEN` + `.npmrc` with a literal `${NPM_TOKEN}`, `pip.conf` (`pip config list` reads it), `.docker/config.json` from `--from`, `UV_INDEX_*`. The files are owned by `user`, mode 600. `docker` runs without sudo. |
| `integrated-agents__conversations` | Pass, as far as I could go without an AI key | An OpenCode prompt queued, gave a `conversationId`, and showed in `conversations` with `running/current/thisShell`. `interrupt --convo` ended it as `interrupted`. `GET /prompts/{id}` matches `PromptRunResponse` (`promptRun.done`). Boat's base64url `<timestamp>:<id>` cursor loop paged correctly (2, 1, then empty). No live model turn: the free OpenCode model never answered. opus-13's sub-agent covered turns with stand-in binaries. |
| `snapshots__stop` | Pass | Boat's `until … = "stopped"` loop finished in 4 s, and the API says `archived`. `~/.boat/pre-stop` ran as `user` in `/home/user` on stop, not on `stop --force`, and not on a saved-snapshot save. `{"force":true}` → `stop.forced:true`. `snapshot <id> customer-42`, `new --from customer-42`, `snapshot rm`. |
| `long-running-tasks` + `__always-on-services` | Pass | Boat's systemd unit verbatim (with a Python server). It is active after stop, `resume --type default` and fork. `host 8080` gave the same URL and token after the resume, and the token link serves the page with a cookie jar. `extend --hours 12` / `--no-auto-stop`. `ps` and `history`. |
| `data-retention__delete` | Pass | A wrong `X-Ascii-Confirm-Delete` → 409. `DELETE /snapshots/{id}` → 202 `bdop_…`, `stage: removing`, `Cache-Control: no-store`. `snapshot delete --all --yes`, after which resume → `409 snapshot_data_expired`. `delete --yes` follows to `completed`. `deletion status`. Then 404. A saved snapshot outlives its source (`new --from` read the file), and removing it → `snapshot_not_found` at once. |
| `ai-credentials` | Pass, no provider traffic | Profiles: `new --copy-from`, `rename`, `rm default` → `409 is_default`, `rm` of a profile an env names → `409 in_use`. A sign-in sent to `set-key` → `400 not_an_api_key`. Accounts: `add-account` (stdin and `{"value"}`), `switch` by `…beef` and by number, and `forget` of the active one switches to the next. A custom API with reserved and `_BASE_URL` names → `invalid_custom_api_name`. `http://` → `invalid_base_url`. Proxy on: the sandbox gets `cvmai_…` placeholders plus `…/ai-proxy/anthropic`, `/openai/v1` and `/custom/STRIPE_KEY` base URLs. `ai block` → 403 from the proxy, and a bogus placeholder → 401. `ai usage`, with and without an id. I sent nothing upstream. |
| `sdks__typescript` | Pass (19/21) | The 2 failures are the `large` samples (#12, #14) hitting B1's 409 on my pool. #19 expects a real agent key. `ResponseError` is exported as an alias of `ApiError`. |
| `billing__plan-changes`, `free-trial`, `compliance` | N/A as classified | BILL-020/021 and SEC-009/010. `crowdvm billing plan 100` → `no_plans` with a pointer to `admin policy`. The one supportable rule on these pages, org delete, is B2. |

## Disputed or accepted classifications

- **SSH-004 (ADAPTED).** I accept it. Boat's curl tab breaks on a node without `--direct-host`, but the response gives `proxyCommand`, and `crowdvm ssh-config` writes a working entry. Adding a one-line note to the migrating guide on Boat's recipe would help.
- **Upload/download URLs go through the server, not the sandbox.** I accept it. There is no size limit in practice (7 MB passed), it is single-direction, and the TTL rules hold.
- **sonnet-12's suggestion "treat paused like drain".** The team followed it into `couldEverPlace` as well as `pickNode`. Drain (`Disabled`) has the same final 409 problem. I do not count drain as a blocker: an admin drains a node to take it away. Pausing is the node owner's day-to-day switch, and the guide lists both as "stop taking new sandboxes" (`node-owners.md:130`), so it is worth fixing both the same way.

## Not verified

- CI against real GitHub or Gitea. I checked opus-14 B1 by reading the code and the fake-GitHub test.
- Live model turns (no AI credential). AI proxy forwarding to a real provider.
- The Docker stop signal at stop time (container state came back after resume).
- Windows nodes. The dashboard in a browser.

## What would flip this to AGREE

1. **B1.** A paused node counts as able to host in `couldEverPlace`, the same as an offline one, so `new`, `resume --type` and `fork --type` of a size that only a paused node fits queue with `waitingForCapacity`. `failFast` still answers `503 no_ready_machine`. `limits` and the 503 detail say "paused", not "not big enough" or "no node is online". `TestPausedNodeAndImpossibleSize` is changed to expect that.
2. **B2.** `org delete` refuses while other members remain (409) and cleans up or archives the org's environments, profiles and saved snapshots. Add a server test with a second member.

m1 and m2 are each a few lines and remove the last start-counting and CI-deletion edge cases.
