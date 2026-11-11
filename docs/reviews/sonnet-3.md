# Review sonnet-3: round-1 findings re-verified, plus an independent sweep

Reviewer: Claude Sonnet 5.5 (independent, round 2). Date: 2026-11-10. Tree: uncommitted working tree on `master`. Files kept changing while I reviewed (the team was editing until 09:48). Where a result depends on timing I say so.

## Verdict: AGREE

CrowdVM now supports every Boat feature I could find that is supportable under its constraints (LAN and Tailscale only, self-hosted, no payments, no third-party OAuth, built on one machine).

- I checked every missing/broken finding in the four round-1 reviews (41 items) against the current code, and reproduced about half of them live on a QEMU/KVM node. Each one is fixed, accepted as ADAPTED with a stated reason, or cannot be tested here (Windows). One has a small residue: the error codes on a stopped sandbox (N3). The `csh_` share-code prefix (N2) is a new sweep finding, not a round-1 one.
- The independent sweep found no missing or broken feature that works on a LAN and that I would call blocking. It found small residual items, listed below.
- `make build`, `go vet` and the unit tests pass. The full e2e run (not mine) ended `EXIT=0` after 611 s.
- Four round-1 findings are fixed in the current tree but had broken my first live run, so I re-verified them on rebuilt binaries: sandbox id alphabet, deletion operations, `run --personal` and Bedrock IAM keys (the last two are only opus-3's list, not mine; I checked them in source only).

A flip back to DISAGREE needs a regression of one of the round-1 fixes, or a supportable Boat feature that I did not look at. I could not look at the items under "Not verified".

## Method

1. **Read** the four round-1 reviews and listed every missing/broken finding (41 items: opus-1 19, opus-2 9, sonnet-1 6, sonnet-2 7).
2. **Static check** of each finding in the current code, with file:line.
3. **Builds and tests:** `make build`, `go vet ./...`, `go test` on everything except `test/e2e` (all pass), plus `go test ./internal/api ./internal/server ./internal/cli ./skills` again after the late edits. I read the e2e log (ends `EXIT=0`) and did not run e2e.
4. **Two live clusters of my own** (isolated dirs under `~/.local/share/crowdvm-dev/`, ports 9810-9813, qemu driver, image `ubuntu-24.04-crowdvm-v2`):
   - Cluster 1 used the binaries from `make build` (taken at 09:33). It had two users, a node with `direct-host` set later, and a webhook receiver.
   - Cluster 2 used binaries built into my own directory after the 09:44-09:48 edits. It re-checked ids and deletion operations.
   - Both were torn down. I removed my data dirs. I killed only processes I started (matched by pid file and data dir).
5. **Mechanical sweeps:**
   - Every `boat <cmd> --flag` found in the 219 pages' code blocks (665 command lines) against `crowdvm … --help`.
   - Live GET responses (39 endpoints) validated against `.ref/boat-v1.yaml` response schemas with my own small validator.
   - Boat's own SDK doc samples run verbatim against the live cluster (`sdk/tools/doc_samples.py`, live mode: 52 of 56 pass; the 4 failures are my node being too small, see below).
6. **Agent-facing surface:** docs endpoints, docs MCP server, the `crowdvm` skill, `agent-instructions`, the SDK shim, one real OpenCode prompt, and the PARITY ADAPTED and N/A rows read in full.
7. I added and then deleted one throwaway test file in `internal/api/` to measure the id alphabet. I changed no other repo file.

## Round-1 findings: status

Legend: **Fixed** = confirmed in code and, where marked LIVE, reproduced on my cluster. **Partly** = main behaviour fixed, a detail remains.

### opus-1

| # | Finding | Status | Evidence |
|---|---|---|---|
| 1 | Refused stop makes the sandbox unusable | Fixed | Pre-flight live snapshot before prepare-stop, `archiving` only after it succeeds: `internal/server/lifecycle.go:857-870` and `:838-845`. Meter pause and notice: `lifecycle_pause.go:101-118`. Test: `review_fixes_test.go:26`. The save-failure path itself I did not trigger live. |
| 2 | Unresponsive sandbox: no billing pause, no notice | Fixed (LIVE) | `lifecycle.go:573-575` calls `notAnswering` (`lifecycle_pause.go:141`). Live: stopped the guest agent for 50 s, saw `degraded guest_unreachable`, `billingPaused: not_answering`, a `not_answering` notice, then `sandbox.degraded` and `sandbox.recovered` at my webhook receiver. |
| 3 | Node-loss webhooks (`sandbox.error`, `sandbox.ready`) | Fixed | `lifecycle.go:609-612` emits `degraded` and `error`; `lifecycle.go:553-558` emits `ready` (reason `node_returned`) and `recovered`. Test `review_fixes_test.go:113`. I did not take a node offline live. |
| 4 | Sandbox ids were `bx_`+10 and the alphabet differed | Fixed, after my first live run | My first run (09:34) minted ids with `0`/`1` in them: 4017 of 10000 failed Boat's pattern `^bx_[23456789abcdefghjkmnpqrstuvwxyz]{8}$`, and the spec uses that pattern on 45 path parameters. The team then changed `internal/api/api.go:131-148` (alphabet `23456789abcdefghjkmnpqrstuvwxyz`, unbiased draw) and added `internal/api/id_test.go`. Re-verified on rebuilt binaries: unit test passes and 8 live ids all conform. Residual: share codes are still `csh_` (see N2). |
| 5 | Python SDK: Boat's positional calls and module paths fail | Fixed (LIVE) | `sdk/python/src/crowdvm/compat.py:1-20` and `sdk/python/src/boat_sdk/`. Live: Boat's quickstart script, `host_port(sid, HostPortRequest(...))`, `write_text`/`read_text`, `create_upload_url`, `stream_command`, detached `command_status`, `stop`, `delete_sandbox(sandbox_id=…, x_ascii_confirm_delete=…)` all worked against the server, with Boat's import paths unchanged. |
| 6 | No `llms.txt`, `/docs/*.md`, docs MCP, `crowdvm` skill | Fixed (LIVE) | `internal/server/feat_docs.go:60-67`, `skills/crowdvm/`. Live: `/llms.txt` 200, `/llms-full.txt` 200 (160 KB), `/docs/quickstart.md` 200, `/docs/cli-reference.md` 200, `/mcp` `initialize`, `tools/list` and `tools/call search_docs` answer. In the VM `~/.claude/skills`, `~/.codex/skills`, `~/.agents/skills` and `~/.config/opencode/skills` each hold `crowdvm/` (written at boot by `internal/guest/skills.go`). `crowdvm skill install` and `agent-instructions` exist. |
| 7 | Any API key can rotate/revoke itself | Fixed (LIVE) | `internal/server/apikeys.go:490-505` (`ownKey`). Live: a read-only key got `403 api_key_action_forbidden` on rotate and on revoke of itself. |
| 8 | Live snapshots keep `~/.cache` | Fixed (LIVE) | `internal/guest/boot.go:422-450` (`cleanUnsavedAtBoot`). Live: wrote `~/.cache/rev/f`, forked the running sandbox, the fork had no cache file; same after stop/resume. |
| 9 | `snapshot pull <sandboxId>` returns 404 | Fixed (LIVE) | `internal/cli/cmd_snapshot.go:~270-295`. Live: pulled 11488 files from a stopped sandbox by id. |
| 10 | Deletion ops report `completed` too early | Fixed (LIVE) | `internal/server/deletion_ops.go:1-18, 60-112`: completes only after the node confirms and no snapshot copy is left, otherwise `processing`/`retrying`, `blocked` after 24 h. Live on rebuilt binaries: `snapshot delete --all` on a stopped sandbox, a single-snapshot delete and a sandbox delete all went to `completed/completed`. opus-3 reported snapshot ops stuck in `processing/retrying`; the file changed at 09:44 and I could not reproduce it. |
| 11 | Org environment wins a name clash | Fixed | `internal/server/feat_env.go:1161-1190`: owners are `[user, org]`, so the user's own wins. |
| 12 | Webhook URLs unrestricted (loopback/metadata) | Fixed | `internal/server/outbound_guard.go:1-50`, used at `feat_webhooks.go:672,753` and `feat_otel.go:648`; admin flag `--webhook-allow-local`. Test `review_fixes_test.go:282`. Not run live: my server was started with the allow-local flag. |
| 13 | Raw ports / OpenSSH only on `--direct-host`; `host --public` did not open the firewall | Fixed (LIVE) | `internal/guest/firewall.go:1-60`, `internal/server/feat_direct.go:1-17`. Live: with `direct-host` set, `host 8000 --public` returned `directEndpoint 127.0.0.1:38957`, `curl` got 200, and `ufw status` showed `8000/tcp ALLOW`. `sshEndpoint` was populated. Default-off stays (see D3). |
| 14 | Error codes/reasons differ from the spec | Partly | Steer `delivery_failed`: `internal/guest/agents/http.go:407-410`. Tree `inventory_too_large`: `feat_snapfiles.go:410-415`. `machine_not_running` for a stopped sandbox follows `troubleshooting.md`; the spec and `readiness.md` say `sandbox_not_ready`, so Boat contradicts itself. Steer/interrupt on a stopped sandbox still returns `machine_not_running`, not `no_running_turn`/`not_running` (see N3). |
| 15 | `/limits` fields | Fixed | `internal/server/acct_billing.go:883-917` (`startWindowFull` object only while full, `startLimitsWithWallet`). |
| 16 | Orgs: admin invite, private history visible, OTel limit | Fixed | Owner-only invite `orgs.go:547`; history hidden for rows recorded while private `feat_history.go:185-204,343-415`; OTel limit `feat_otel.go:456` uses the wallet limit. Not run live. |
| – | Top-level `nested: true` unused | Fixed | `review_fixes_test.go:513`. |
| – | `new --help` renders `--environment crowdvm env list` | Fixed (LIVE) | Help now says `(see: crowdvm env list)`. |
| – | `docs/E2E.md` B1-B6 stale | Fixed | `docs/E2E.md:99-116,172-177`. |

### opus-2

| # | Finding | Status | Evidence |
|---|---|---|---|
| 1 | Resume after deleting all snapshots boots a blank disk | Fixed (LIVE) | `internal/server/lifecycle.go:1150-1157` (resume) and `:1307-1310` (fork). Live: `snapshot delete --all`, then `resume` and `fork` both returned `409 snapshot_data_expired`. |
| 2 | `data-retention enable` always fails | Fixed (LIVE) | CLI sends Boat's phrase (test `internal/cli/review_fixes_test.go:39`); server also accepts the old one (`feat_retention.go:131-133`). Live: `enable --yes` worked. |
| 3 | `org default-visibility` with no argument errors | Fixed (LIVE) | `internal/cli/cmd_admin.go:565-584`. Live: prints `defaultVisibility` with `--org` or an active org. |
| 4 | No `crowdvm` skill | Fixed (LIVE) | See opus-1 #6. |
| 5 | No docs for agents | Fixed (LIVE) | See opus-1 #6. |
| 6 | `host --public` blocked by guest firewall | Fixed (LIVE) | See opus-1 #13. |
| 7 | `push --to` outside `/home/user` fails | Fixed (LIVE) | `internal/cli/transfer.go:179-182` (sudo mkdir/chown). Live: `push … --to /srv/api` created the folder owned by `user`. |
| 8 | `env new` prints empty ids; `run` summary has no cost; `push` file count | Fixed / not checked | `env new` returns a populated environment (`internal/client/account.go:367-381`, live). `run --json` summary has `credits` and `dollars` (`cmd_exec.go:420-441`, live). The `push` file count with symlinks: not checked. |
| – | AIC-013 custom-API private hosts hidden behind an undocumented env var | Fixed | Flag `--ai-custom-api-allow-private` at `cmd/crowdvm-server/main.go:132`, documented in `docs/guide/getting-started.md:43`. |

### sonnet-1

| # | Finding | Status | Evidence |
|---|---|---|---|
| 1 | No llms.txt / docs MCP | Fixed (LIVE) | See opus-1 #6. |
| 2 | No skill pack | Fixed (LIVE) | See opus-1 #6. |
| 3 | `bx_` ids | Fixed | See opus-1 #4. |
| 4 | `prompt_required`, `provider_required`, `machine_class_plan_required`, `org_suspended` missing | Fixed | `internal/server/acct_prompt.go:118,122`, `internal/guest/agents/http.go:63`, `acct_billing.go:837-839`, `orgs_suspend.go:56`. |
| 5 | Capacity behaviour inconsistent | Fixed | `lifecycle_place.go:1-12`: without `failFast` a full pool queues the boot; a size no node could ever hold fails at once. Test `review_fixes_test.go:177`. Consistent with what I saw live: `type=large` on a 6-vCPU node gave an immediate `503 out_of_capacity`. |
| 6 | CI only for GitHub | Fixed | `internal/server/feat_ci.go:14` adds Gitea Actions. Test `review_fixes_test.go:409` uses a fake Gitea. Not tested against a real Gitea or GitHub. |

### sonnet-2

| # | Finding | Status | Evidence |
|---|---|---|---|
| M1 | Docker containers don't restart after stop/resume | Fixed (LIVE) | `internal/guest/docker.go:81-190` records running containers and restarts them (`boot.go:458`). Live: a container with no restart policy and one with `unless-stopped` came back after stop/resume and after a live fork; a container that had already exited stayed stopped. |
| M2 | `--gpu` is placement only | Fixed in code | `internal/node/vm/qemu/gpu.go:14-45` (VFIO passthrough), guide `docs/guide/cli-for-agents.md:87-97`. I did not pass my GPU through, so not verified. |
| M3 | Docs for agents absent | Fixed (LIVE) | See opus-1 #6. |
| M4 | `lux` absent | Fixed in the image recipe | `images/rootfs/full/usr/local/bin/lux` (pointer script, exit 127). My image is v2, which predates it, so `lux` was still "not found" live. See N5. |
| M5 | Chrome Terms dialog | Fixed in the image recipe and guest | `images/scripts/full.sh:83-96`, `internal/guest/chrome.go`. Not re-run live (desktop). |
| M6 | Subscription sign-in is paste-only | Accepted as ADAPTED | See D1. |
| – | Windows nodes untested | Unchanged | Cross-compiles; no Windows machine here. Allowed by the "built on one machine" constraint. |

## New findings from the independent sweep

None blocks the verdict. All are small and supportable on a LAN.

| ID | Where | What | Evidence | Why it is supportable |
|---|---|---|---|---|
| N1 | PARITY SEC-009 | The N/A row says the coordinator "exposes `/healthz`". It does not. `GET /healthz` returns the dashboard HTML with status 200. Round-1 sonnet-1 listed `/healthz` as working, probably for the same reason. | `grep healthz internal cmd` finds only docs; live `curl /healthz` returns `text/html`. | A 3-line JSON route; or fix the row. Boat's status page itself is N/A. |
| N2 | `internal/api/api.go:127` vs PARITY.md:318 (SNAP-024, REQUIRED) | Share codes are `csh_…`; the PARITY row, its principle 1 and Boat's docs say `bsh_…`. No Boat client validates share-code prefixes (the spec has no pattern for it), so nothing breaks. | `docs/guide/migrating-from-boat.md:26` lists `csh_`. | Either rename the prefix, or reclassify SNAP-024 as ADAPTED. |
| N3 | `internal/server/passthrough.go:44,168` | `steer` and `interrupt` on a stopped sandbox return `409 machine_not_running`; Boat's spec says `409 no_running_turn` with `reason: not_running`. | `.ref/boat-v1.yaml:7130`. | Map that case in the steer/interrupt route. |
| N4 | `internal/cli/update.go` (CLI-025) | Boat's CLI installs updates itself, at most every 6 h. CrowdVM prints a notice and keeps `self-update`. | `docs/gaps/B.md:335`. | The server already reports a newer release. The team gave a reason (a hash-based auto-install could downgrade a newer local build). I accept it as ADAPTED. |
| N5 | `images/` | The `lux` pointer and the Chrome `initial_preferences` live in image v3 (full). Only `…-v3` minimal exists in `~/.local/share/crowdvm-dev/images/`, so the full v3 image is not in the dev image dir. Everything else works on v2 because the guest agent applies it. | `ls ~/.local/share/crowdvm-dev/images/`; `images/README.md:203-236`. | Rebuild and keep the full v3 image next to v2. |
| N6 | `docs/PARITY.md` ORG-024 / DASH-012 | The dashboard has an active-organization select (`web/js/main.js:166-178`) but no "Viewing" dropdown that filters the sandbox list by owner/organization. `docs/gaps/B.md:164` still lists this as missing. | `grep -ri viewing web/js` finds nothing. | Pure front-end filter over `GET /sandboxes`. Cosmetic: the CLI and API already return everything the user may see. |

## Disputed classifications

- **AGT-030 (ADAPTED, paste-only subscription sign-in):** I accept it. A dashboard "Sign in" button would drive each vendor's own OAuth login, which is third-party OAuth. The row now says exactly what is built and what is not (`docs/PARITY.md:407`).
- **SSH-004/007/008 (ADAPTED):** accepted. `crowdvm ssh-config` plus the ProxyCommand tunnel gives VS Code and Cursor a working Remote-SSH config. Plain OpenSSH to `sshEndpoint` needs a node with `--direct-host`; default-off is a reasonable safety choice for contributor machines. I verified both ways live.
- **URL-012 (ADAPTED):** accepted. A raw port maps to a random node port (`127.0.0.1:38957` for guest port 8000), not the same number on the node IP, because several sandboxes share one node address.
- **URL-020 / SNAP-009 (ADAPTED, guest firewall cannot cut the agent off):** accepted, and it is better than Boat. Live, `ufw insert 1 deny in 7070/tcp` was undone by the guest, and the sandbox stayed healthy. Boat marks such a sandbox degraded.
- **N/A rows (26):** I read all of them. Every one is payments, Google/AgentID OAuth, trials, SOC 2/SLA/support, legacy-Box migration or documentation-only text. I dispute none, except that SEC-009's replacement claim is wrong (N1).
- **CLI-121 / BILL-020 (`billing cancel --keep-disks`):** N/A stays defensible: it only exists inside a paid subscription.

## Spot checks that passed live

- Create (4 s to ready), exec, stop, resume, fork, saved snapshot, `run --json` summary, SSH and `scp`, `ps`, host URLs, `push` and `--to`.
- Docker restart policy after stop/resume/fork; `~/.cache` and `/tmp` not saved.
- Guest firewall recipe from `hosting__firewall`: a blocked `runner` account cannot reach the internet, and `user` and the agent still work.
- A real OpenCode prompt created a file in the sandbox (`opencode/beam-free`).
- Docs endpoints and MCP; `/install`, `/install.ps1`, `/auth.md`, `/.well-known/oauth-protected-resource`, `/openapi/crowdvm-v1.yaml`, `/api/v1/openapi.yaml`, the dashboard.
- Live GET responses against the Boat response schemas: no missing required fields. The only violations were `/me` `connectionMethod: "password"` (the local-account adaptation) and a `null` `reason` on snapshot tree.
- Boat's SDK doc samples, live: 52 of 56 pass (1 Python and 1 TypeScript are documented expected refusals, because they need a real agent key). The 4 failures (`sdks__python#12,#14`, `sdks__typescript#12,#14`) create a `large` sandbox, which needs 8 vCPU; my node had 6, so the server correctly answers an immediate `503 out_of_capacity`. They passed in the in-process run (56/56).

## Not verified

- GPU passthrough (VFIO) and `gpu.virgl`; my one GPU was in use for the desktop.
- Windows nodes (no Windows machine).
- CI against real GitHub or Gitea (fakes only).
- Real vendor credentials (Claude, Codex, subscriptions, Bedrock), and Claude Code/Codex harness steer modes.
- A failed-save stop on a real node (unit test only), a node going offline while sandboxes run (unit test only), and the webhook SSRF guard against a live listener.
- Wine, the Android emulator and the dashboard UI beyond a page-per-tab check.
- Items opus-3 lists as open that I checked only in source: `run --personal` now sets `org: personal` (`cmd_exec.go:380-382`); Bedrock IAM keys now map to `AWS_ACCESS_KEY_ID` and friends (`feat_ai.go:1514`); `org migrate` privacy and the low-disk Docker cleanup (`guest/docker.go`) were edited at 09:44-09:45, and I did not re-test them.

## What would flip AGREE to DISAGREE

- Any regression of the fixed items above. The cheapest guards are `internal/api/id_test.go` (id pattern), `internal/server/review_fixes_test.go` (refused stop, node-loss events, deletion ops, outbound guard, nested placement, Gitea CI), and the live e2e groups for Docker restart, direct ports and fork cleanup.
- A supportable Boat feature in one of the "Not verified" areas turning out to be missing.

Small things worth doing next, none blocking: N1 (add `/healthz` or correct SEC-009), N2 (`bsh_` or reclassify), N3 (steer/interrupt on a stopped sandbox), N5 (keep the v3 image in the dev dir), and N6 (a "Viewing" filter).

## TL;DR

AGREE. Every round-1 finding is fixed or accepted (about half reproduced live), and the independent sweep found only small residual items (N1-N6) with no blocker. The largest late fix was the sandbox id alphabet (40% of ids failed Boat's pattern until the 09:44 edit). GPU passthrough, Windows nodes and real vendor or CI integrations were not verifiable here.
