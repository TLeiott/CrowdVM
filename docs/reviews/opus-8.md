# Review opus-8: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, round 7). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE

Both earlier blockers are fixed, and I reproduced both fixes live:
- **opus-7 B1:** `--org personal` now reaches the server. With an org active, `crowdvm --org personal new` bills the personal account, and `--org personal api-key create` makes an account key. `CROWDVM_ORG=personal` and `BOAT_ORG=personal` do the same.
- **sonnet-5 N1:** `X=$(host url N)`, `$(host N)` and the commands-API `.stdout` now give the bare URL.

The six claimed opus-7 minor fixes (n-1, n-3, n-4, n-7, n-8, n-9) also hold.

Two supportable, documented behaviours are wrong, and I reproduced both live. Both come from resolving a saved snapshot by name under an explicit org scope:

1. **B1. Under `--org personal` (or `CROWDVM_ORG`/`BOAT_ORG=personal`), the account's own saved snapshots can't be found by name.**
   - The server's `savedLookup` treats any explicit scope as "look only in that org". When the scope resolves to personal (`org == nil`), it returns `404 snapshot_not_found` without checking the personal wallet (`internal/server/snapshots.go:640-653`).
   - Until this round the CLI dropped `personal`, so these requests fell through to the personal-first path. Now the CLI sends it (`internal/cli/app.go:419-421`).
   - Live: `crowdvm --org personal snapshot <id> rv8saved` saves the snapshot, then reports `404 snapshot_not_found` and exits 1. `snapshot info`, `share`, `shares` and `rm` all 404 on that personal snapshot. Without `--org`, the same commands find it.
   - Boat documents `--org` as a global flag for any command, with env `BOAT_ORG` (`cli-reference.md:38`), and `personal` as a valid reference (`organizations__active-wallet.md:25`).
   - This is a regression from the opus-7 B1 fix. The fix is one branch: when the explicit scope resolves to personal, look the name up under `c.P.User.ID`.
2. **B2. `--org <org> new --from NAME` ignores the org and deploys a same-named personal (or other-org) snapshot.**
   - Boat's team-image recipe is `boat --org acme new --from web-stack` (`snapshots__templates.md:206`).
   - CrowdVM's create calls `resolveFrom(u, req.From)` without the scope it has already resolved (`internal/server/sandboxes.go:829`). `resolveFrom` takes the personal name first, then the most recently updated name across all the user's orgs (`internal/server/snapshots.go:984-987`).
   - Live: personal `rv8saved` (`snap_prvw…`) and acme8 `rv8saved` (`snap_57sd…`). `POST /sandboxes` with `X-CrowdVM-Org: acme8 {"from":"rv8saved"}` billed acme8 but deployed the **personal** saved snapshot (`source_id = ss_smsf…`, owner `usr_…`).
   - Found by my sub-reviewer, who reproduced it with an in-process test. I re-ran it on my own cluster.

One function that resolves a saved snapshot by name in a scope (explicit org → that org, explicit personal → personal, none → personal then orgs), used by both create and the by-name endpoints, fixes both. e2e run 6 passed (`EXIT=0`), and my sweep found two more minor items.

## Method

1. **Read** opus-7 and sonnet-5 (blockers, minor fixes, flip criteria), then the code behind each claimed fix:
   - `internal/cli/app.go`, `internal/cli/cmd_access.go`, `internal/cli/cmd_admin.go`;
   - `internal/client/client.go`;
   - `internal/server/orgs.go`, `internal/server/apikeys.go`, `internal/server/snapshots.go`, `internal/server/sandboxes.go`;
   - `internal/guest/tracer_bpf.go`, `docs/guide/security.md`, `web/js/pages/sandbox.js`, `docs/PARITY.md`.
2. **Regression hunt.** I listed every code path that reads `c.Org` in the CLI (`cmd_admin.go:305,322,591`, `cmd_ai.go:165`, `cmd_ci.go:180,238`). I also listed every server path that sees an explicit org reference: `Ctx.Scope`, `resolveOrgRef`, `savedLookup`, `envScopeOf`, `handleCreateKey`, and the two admin `SELECT … FROM orgs WHERE name=?` paths. Then I checked how each one handles the value `personal`. Only `savedLookup` mishandles it (B1).
   - For the `host` output change, I grepped the e2e harness, unit tests, skill, guides, integrations and SDKs for callers that parse `host`/`host url` output. The e2e calls that need JSON pass `--json` explicitly (`test/e2e/basic_test.go:478,531`). The captured bare-URL path is now tested (`basic_test.go:562-567`, `internal/cli/cli_more_test.go:97-101`). Nothing else parses it.
3. **Build and tests.**
   - I built all four binaries (`CGO_ENABLED=0`) into my scratchpad, not `bin/`, so the running e2e was left alone.
   - `go vet ./...` is clean, and `go test ./...` passes (`test/e2e` skips without `CROWDVM_E2E`).
   - I did not run e2e. I read `…/232e4b2f…/scratchpad/e2e-run6.log`: `--- PASS: TestE2E (662.00s)`, `EXIT=0`. Every group passed, including `host`, `orgs`, `apikeys`, `saved-snapshots`, `sdk` and `boat-sdk-samples` (187.82 s). Only `bootloop` was skipped.
4. **Own cluster** under `~/.local/share/review-opus-8`.
   - `crowdvm-server` on `127.0.0.1:9960` and one `crowdvm-node` (qemu, KVM, UI on `127.0.0.1:9961`, 6 vCPU / 10 GiB / 2 sandboxes), image `ubuntu-24.04-crowdvm-v3`, guest binary freshly built.
   - Sandboxes: `bx_3fd6xr8f` (personal, long-lived), `bx_4faaqcxg` (org control), `bx_ayckdu58` (snapshots-off fork), plus two short-lived `--from` deploys (`bx_ypyem9p8`, `bx_yrqagw7p`).
5. **Independent sweep.**
   - One read-only Opus sub-reviewer took the 15 pages that no earlier review cites by name: `environments__secrets`, `environments__repositories`, `environments__private-registries`, `snapshots__resume-and-fork`, `snapshots__templates`, `long-running-tasks__background-commands`, `recipes__parallel-agents`, `recipes__agent-on-repo`, `integrated-agents__harnesses-and-models`, `integrated-agents__steering`, `organizations__seats-and-limits`, `organizations__move-personal-plan`, `hosting__how-it-works`, `platform-guide__templates-and-sizing` and `data-retention__close-account`. I re-checked every finding I kept.
   - Myself, live:
     - `desktop-streaming__drive-and-record` (recording, Chrome plus `wmctrl`);
     - `machines__change-size` (resume `--type`, the 409 on a running sandbox, Boat's `until … .sandbox.state` loop);
     - `api-keys__store-rotate-revoke` (`usage --verbose`, `api_key_expired`, who may rotate);
     - `login --key-stdin --json`, `scp` both ways, ssh stdin piping;
     - `organizations__shared-sandboxes` (what `list` shows in org scope).
   - Myself, mechanical:
     - every backticked snake_case code in `llms-full.txt`, grepped in the server and SDKs;
     - the webhook event enum of `boat-v1.yaml` against the events the server emits;
     - live `GET /limits`, `/me` and `/sandboxes/{id}` validated against their spec response schemas (required keys and enums).

## Fix status

F = fixed. "live" = reproduced on my node. "code" = read only.

| # | Finding | Status | Evidence |
|---|---|---|---|
| opus-7 B1 | `--org personal` dropped by the CLI | **F (live), but see new B1** | `internal/cli/app.go:419-421` sends the value as is. `client.go:369-370,402-403` put it in `X-CrowdVM-Org` and `?org=`, and `resolveOrgRef` maps it to personal (`orgs.go:66`). Live, with `org switch acme8` active: `crowdvm --org personal new --type small` → `bx_3fd6xr8f`, `team:null`. Control `crowdvm new` → `bx_4faaqcxg`, `team:acme8`. `crowdvm --org personal api-key create persk8` → `org:null`, and `GET /account/data-retention` **200**. Plain `api-key create orgk8` → `org:team_…`, **403**. `CROWDVM_ORG=personal` and `BOAT_ORG=personal` → `org:null`. `crowdvm --org personal org` → `active: Personal, override:true`. Server tests: `apikeys_test.go:31-39`. There is no CLI-level test of `--org personal` (the opus-7 flip criterion asked for one). |
| sonnet-5 N1 | Captured `host` / `host url` print JSON | **F (live)** | `cmd_access.go:516,589` call `a.rawOutput(cmd)` (`app.go:251-256`). Live in the sandbox: `X=$(host url 3001)` → `[http://127.0.0.1:9960/p/bx_3fd6xr8f/3001/?_token=…]`, `Y=$(host 3002)` → bare URL, `boat host url 3001 \| cat` → bare URL, and `host url 3001 --json` → the object. `POST /sandboxes/{id}/commands {"command":"host url 3001"}` → `stdout:"http://…?_token=…\n"`. |
| opus-7 n-1 | `host --json` lacks `sandboxId`/`localhost` | **F (live)** | `crowdvm host bx_3fd6xr8f 3003 --json` → `sandboxId`, `port`, `access`, `isProtected`, `localhost:false`. |
| opus-7 n-3 | Socket capture keeps credential headers | **F (code)** | `tracer_bpf.go:216-228` (`redactCreds` blanks `Authorization`, `X-Registry-Auth`, `X-Api-Key`, `Cookie` and others), test `tracer_bpf_test.go:279-282`, note in `docs/guide/security.md:76-82`. |
| opus-7 n-4 | Stopped snapshots-off fork shows `snapshotAvailable:true` | **F (live)** | `--org personal fork bx_3fd6xr8f --no-snapshots`, then `stop` → `{"state":"stopped","snapshots":false,"snapshotAvailable":false}`. |
| opus-7 n-7 | Dashboard says auto-stop is "after last activity" | **F (code)** | `web/js/pages/sandbox.js:92`: "after start or resume". |
| opus-7 n-8 | `api-key list --json` drops `org` | **F (live)** | `crowdvm api-key list --json` → `org` on each key (`team_…` or `null`). |
| opus-7 n-9 | KEY-001 note "no legacy prefixes" | **F (code)** | `PARITY.md:482`: "`boat_` and `box_` are accepted in place of `cvm_`". |
| Regressions | Side effects of these fixes | **One (new B1)** | See B1. No other path I traced mishandles `personal`. An org key can't use `X-CrowdVM-Org: personal` to make an account key: org keys get `api_key_org_forbidden` on key creation (live). |

## Blocking findings

### B1. Explicit `personal` scope hides the account's saved snapshots (SNAP-015, ORG-007, CLI global `--org`)

**What Boat documents:**
- `cli-reference.md:38`: `--org <ID_OR_NAME>`, a global flag, "Run this command against an organization wallet (env: `BOAT_ORG`)".
- `organizations__active-wallet.md:25`: `personal` is a valid reference and means your own account.
- `cli-reference.md:357,1369`: `boat --org personal …` is how you act on your account while an org is sticky.

**What CrowdVM does:**
- `savedLookup` (`internal/server/snapshots.go:638-653`) sets `explicit` when there is any `?org=`, `?teamId=` or org header. It then calls `c.Scope("")`, and `if org == nil { return nil, notFound }`. So an explicit `personal` never looks in the personal wallet.
- Five handlers use it: get (`:775`), delete (`:865`), create share (`:892`), list shares (`:912`) and revoke share (`:938`).
- The CLI's save path polls with `GetSavedSnapshot` after the 202 (`internal/cli/cmd_snapshot.go:119`), so the save itself reports a 404.
- This used to work by accident: the CLI dropped `personal` before this round's fix. The SDKs always passed it through, so API callers using `x_boat_org="personal"` were already affected.

**Live** (with `org switch acme8` active; `rv8saved` is a personal saved snapshot):

| Command | Result |
|---|---|
| `crowdvm --org personal snapshot bx_3fd6xr8f rv8saved` | `{"code":"snapshot_not_found","status":404}`, **exit 1**. The snapshot was saved anyway: `crowdvm snapshots` lists `rv8saved ready`. |
| `crowdvm --org personal snapshot info rv8saved` | 404, exit 1 |
| `crowdvm --org personal snapshot share rv8saved` | exit 1 |
| `crowdvm --org personal snapshot shares rv8saved` | 404 |
| `crowdvm --org personal snapshot rm rv8saved` | 404, exit 1, the snapshot stays |
| Same commands without `--org` | Found (`rv8saved ready`) |
| `crowdvm --org personal snapshots` | Lists it (`handleListSaved` uses `Scope` directly), so the list and the get disagree. |

**Why it matters:** an agent or CI job that exports `BOAT_ORG=personal` (or `CROWDVM_ORG=personal`) to pin its wallet can't save, inspect, share or remove its own saved snapshots. The save reports failure after it succeeded, so a retry loop will save again and again. It is server-side scoping only, and nothing about it depends on the LAN.

**Fix:** in `savedLookup`, when the explicit scope resolves to `nil`, look up `s.savedByName(c.P.User.ID, name)` instead of returning 404. Add a test: with an org active, `X-CrowdVM-Org: personal` and `GET /snapshots/saved/{name}` on a personal snapshot → 200.

### B2. `--org <org> new --from NAME` deploys the wrong saved snapshot (SNAP-018, team images)

**What Boat documents** (`snapshots__templates.md:200-214`):
- A snapshot saved under an organization is a team image.
- `boat --org acme new --from web-stack` is how "any member deploys it".
- Personal and org snapshots can share a name. Boat even creates that case itself: "Saved snapshots that you saved before 2026-10-22 under `--org` stay personal. Save them again under `--org` to make them team images" (`:214`).

**What CrowdVM does:**
- `handleCreateSandbox` resolves the wallet (`org, err := c.Scope(orgRef)`, `sandboxes.go:757`) and then ignores it for the source: `src, err = s.resolveFrom(u, req.From)` (`sandboxes.go:829`).
- `resolveFrom` tries `savedByName(u.ID, from)` (personal) first. Otherwise it takes `… owner_id IN (SELECT org_id FROM org_members WHERE user_id=?) ORDER BY updated_at DESC LIMIT 1` (`snapshots.go:984-987`), which is the newest of that name in *any* of the user's orgs.
- The table allows the collision: `UNIQUE(owner_id, name)` (`internal/server/migrations.go:329`), and a test says personal and org names "do not collide" (`internal/server/lifecycle_fix_test.go:382`).

**Live** (`org switch acme8` active):
1. `crowdvm snapshot bx_3fd6xr8f rv8saved` saved personal `ss_smsfnzhq97831cv9`, `snap_prvw…`.
2. `crowdvm --org acme8 snapshot bx_3fd6xr8f rv8saved` saved org `ss_a0n43dddha44j05m`, `snap_57sd…`.
3. `POST /sandboxes` with `X-CrowdVM-Org: acme8`, `{"from":"rv8saved"}` → `bx_yrqagw7p`, `team: acme8`, but `sandboxes.source_id = ss_smsfnzhq97831cv9`, the **personal** one.

A member in two orgs that both have a `base` image gets whichever was saved last, whatever `--org` says.

**Why it matters:** a teammate deploys their own personal disk, with their logins, instead of the team image, and nothing reports an error. Or they get another org's image. It is server-side scoping only and doesn't depend on the LAN.

**Fix:** pass the create's scope into the name lookup and share the lookup with `savedLookup`. Share codes (`csh_…`/`bsh_…`) stay unscoped.

## Non-blocking findings

| ID | What | Evidence |
|---|---|---|
| n-a | Still no CLI-level test of `--org personal` for `new` or `api-key create`. Only the server side is tested (`internal/server/apikeys_test.go:31-39`). The opus-7 flip criterion asked for one. | `grep personal internal/cli/*_test.go`: no `--org personal` case. |
| n-b | Captured `host list` still prints full token URLs in JSON (sonnet-5 M2, not claimed fixed). Boat: "`host list` does not print the access token" (`hosting__host-cli.md:146-148`). | Live: `host list \| cat` in the sandbox printed `?_token=…` for both ports. `cmd_access.go:557-559`. |
| n-c | `crowdvm snapshot rm` doesn't take `--yes` ("unknown flag"). It doesn't prompt either, so scripts that pass `--yes` (as for `delete`) fail with exit 2. Boat documents no `--yes` for snapshot removal, so this is not a parity gap. | Live. |
| n-d | `crowdvm --org acme billing plan 20 --move-personal` fails with `unknown flag: --move-personal`, exit 2 (`organizations__move-personal-plan.md:17`). The move itself exists as `org migrate` (`POST /orgs/{org}/move-personal`), and CLI-119/ORG-022 are ADAPTED. Accepting the documented form (ignore or report the tier, then run `org migrate`) would be trivial. | `internal/cli/cmd_billing.go:92` (sub-reviewer, run against the built CLI). |
| carried | sonnet-5 M1 (org-name case in `org default-visibility`), M3 (`status --check` with keys lacking `account.read`), M4 (`api-key usage` doesn't count refused requests), M5 (no "not supported" message for `login --google`, `wallet pay`, `billing portal`), M6 (Python 3.9). opus-7 n-2, n-5, n-6, n-10 to n-13 and the opus-6/sonnet-4 carry-overs. None was claimed fixed, and I didn't re-check them. | See sonnet-5 and opus-7. |

## Checked and fine

- `organizations__shared-sandboxes.md:7-10`: in org scope, `list` shows your own sandboxes plus the org's (`sandboxes.go:497-499`). `--org personal list` shows only your own.
- `machines__change-size`: `resume --type` on a running sandbox → `409 sandbox_running_stop_first`. Boat's `until [ "$(… info … --json | jq -r .sandbox.state)" = "stopped" ]` loop ended in 4 s, and `resume --type default` gave 4 vCPU / 7 GiB with the data kept.
- `desktop-streaming__drive-and-record`: `ascii-record-desktop start|status|stop` produced a 10 s MP4 (601 KB, `ffprobe` OK). The `google-chrome-stable … & wmctrl -a Chrome` recipe worked, and `scp` downloaded the file.
- `api-keys__store-rotate-revoke`: `usage --verbose` gives requests and created-resource totals. `409 api_key_expired` on rotate exists (`apikeys.go:530`). Rotate and revoke rules match `api-keys__create-and-list.md:98`.
- `login --key-stdin --json` (with both `<<<` and `printf |`). `scp` up and down. `ssh ID bash -s` with stdin. Plain `exec` doesn't forward stdin, which matches Boat (`setup.md:153`: "To pipe a script … use SSH").
- The webhook event enum (6 events) is all emitted.
- The live `/limits`, `/me` and `/sandboxes/{id}` match their spec schemas. The one enum miss is `/me.user.connectionMethod: "password"`, which is the accepted no-OAuth adaptation.
- The `sandbox_*` error codes that exist only in the Box→boat rename table (`migrating-from-box__renamed-names.md`, "Error code values") are not described anywhere else, so they aren't features. Trial codes are N/A (BILL-021).

## Disputed classifications

- **ORG-007 / CLI global `--org`**: done for sandboxes and keys, but not for saved-snapshot lookups (B1).
- **SNAP-015** (`PARITY.md:309`): a save under an explicit personal scope reports 404 (B1).
- **URL-014** (`PARITY.md:264`): the `host url`/`host` part is now right. The `host list` "no tokens" part still fails when captured (n-b, minor).
- **SNAP-018** (`PARITY.md:312`, deploy `--from <name>`): marked REQUIRED/done, but it ignores `--org` (B2).
- **SNAP-023** (teammate deploy scrub), *open question, not counted*. The sub-reviewer noted that `foreignDeployScrub` (`internal/server/lifecycle_secure.go:351-359`) also removes environment repo folders and `authorized_keys` on a teammate deploy. Boat's team-image list (`snapshots__templates.md:213`) names logins, SSH keys and environment secret files, and says "Files that you cloned yourself stay". Whether env-cloned repos stay is unclear in Boat's text, so I don't count it.
- **ENV `add-repo owner/name`** for a public repo outside the git source's `/user/repos` gives `400 unknown_repository` unless a URL is passed (`internal/server/feat_env_repos.go:404-417`). The docs are ambiguous, so this is not counted either.
- The N/A and ADAPTED rows I touched (BILL-021, AUTH-019, RUN-008, CLI-119, ORG-022, KEY-001) keep reasons that trace to a stated constraint. I accept them, as earlier rounds did.

## What would flip this to AGREE

1. **B1:** with an org active and `--org personal` / `CROWDVM_ORG=personal` / `BOAT_ORG=personal`, `snapshot NAME`, `snapshot info`, `share`, `shares`, `unshare` and `rm` find personal saved snapshots, and the save exits 0.
2. **B2:** `--org <org> new --from NAME` (and `X-CrowdVM-Org`/`?org=`/body `org`) deploys that org's saved snapshot, `--org personal … --from` deploys the personal one, and only a request with no scope falls back to "personal, then orgs". An org key resolves names in its own org.
3. A server test for each: same-named personal and org snapshots, an org active, each scope, for create and for `GET /snapshots/saved/{name}`.

The n-items would be nice to have and don't block AGREE.

## Not verified

- Windows nodes, VFIO GPU passthrough, real Claude/Codex/Bedrock credentials, real GitHub/Gitea CI, SMTP, `--direct-host`.
- `snapshot unshare` under `--org personal`. It uses the same `savedLookup` (`snapshots.go:938`), so it is inferred, not run.
- The dashboard beyond the code I read.

## Teardown

I deleted every sandbox I created (`bx_3fd6xr8f`, `bx_4faaqcxg`, `bx_ayckdu58`, `bx_ypyem9p8`, `bx_yrqagw7p`). The `bx_ypyem9p8` deploy waited in `starting` because I had resized the other sandbox and the node lacked the 4 GiB for `small`; that is expected without `--fail-fast`. I then stopped my node and server by PID, and confirmed that no QEMU, node or server of mine was left and ports 9960–9969 were free. Last, I removed `~/.local/share/review-opus-8` and my scratchpad binaries. I touched no other process and joined no other server.

## TL;DR

DISAGREE. Both earlier blockers are fixed, and I reproduced both live:
- `--org personal` now bills the personal account and makes account keys;
- `$(host url N)` gives the bare URL.

The six claimed minor fixes also hold, `go test` passes, and e2e run 6 passed. Two blockers remain, both in name lookup of saved snapshots under an explicit org scope:
- **B1** (regression from the `--org personal` fix): under `--org personal`, your own saved snapshots 404 on info, share and rm, and a save reports failure after it succeeded (`internal/server/snapshots.go:640-653`).
- **B2:** `--org acme new --from NAME` ignores the org and deploys a same-named personal snapshot (`internal/server/sandboxes.go:829`, `snapshots.go:984-987`).

One shared, scope-aware lookup fixes both.
