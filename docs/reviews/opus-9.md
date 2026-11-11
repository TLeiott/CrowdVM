# Review opus-9: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, round 8). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE

All five claimed fixes hold, and I reproduced each one live:
- **opus-8 B1:** `--org personal` (and `CROWDVM_ORG`/`BOAT_ORG=personal`) now finds personal saved snapshots by name. Save exits 0, and `info`, `shares` and `rm` work.
- **opus-8 B2:** `--org ORG new --from NAME` deploys that org's image. I checked 8 combinations of active wallet and `--org`, reading a marker file in each deployed disk.
- **sonnet-6 N1:** `$(host list)`, `host list | cat`, the commands API and `host list --json` print no `_token`. The text forms print the `(gated)`/`(localhost)` table.
- `snapshot rm --yes` and `--org ORG billing plan N --move-personal` work.

My sweep (one sub-reviewer on the 12 non-API pages no earlier review names, plus a mechanical check of every `boat … --flag` in the docs against the CLI) found only minor items (n-1 to n-5).

One blocker remains. It is the third face of the same scoping bug, and it shows up in the case the B1/B2 fixes left out: **an active organization set with `org switch`, with no `--org` on the command.**

**B1. With an active org, `snapshot NAME`, `snapshot info`, `share`, `shares`, `unshare` and `rm` act on your personal snapshot of that name, while the save and `new --from` use the org's.**
- `new --from` (`resolveFrom`, `internal/server/snapshots.go:974-997`) and the save (`handleSaveSaved`, `:783-816`, via `c.Scope`) both treat the active org as the wallet.
- `savedLookup` (`snapshots.go:638-664`) counts only an explicit `?org=`/header/org key as a scope. Without one it looks up personal first. So under `org switch acme`, the by-name endpoints pick a different snapshot from the one the save just wrote and the one `new --from` deploys.
- Live, with personal, acme9 and beta9 snapshots all named `img9` and `org switch acme9` active:
  - `crowdvm snapshot bx_… img9` saved into acme9 (status `saving`). The CLI's poll (`internal/cli/cmd_snapshot.go:119`, no scope) got the personal `img9` back, printed `ready` with the personal `snapshotId`, and exited 0 while acme9's save was still running.
  - `crowdvm snapshot share img9` made a share code for the **personal** snapshot (`shares` under `--org personal` lists it; under `--org acme9` it is empty).
  - `crowdvm snapshot rm img9 --yes` deleted the **personal** snapshot. acme9's and beta9's stayed.
  - `crowdvm new --from img9` deployed acme9's disk (marker `acme`).
- Boat: "A save goes to your active organization, or to the one that you pass with `--org`" (`snapshots__templates.md:182`). The active wallet "is one setting on the account. The CLI and account API keys use it" (`organizations__active-wallet.md:13`).

## Method

1. **Read** opus-8 and sonnet-6 (blockers, n-items, flip criteria), then the code behind each claimed fix:
   - `internal/server/snapshots.go` (`savedLookup`, `resolveFrom`, `handleListSaved`, `savedJSON`, `savedManage`);
   - `internal/server/orgs.go` (`Ctx.Scope`), `internal/server/sandboxes.go:829`;
   - `internal/cli/app.go:417-423`, `cmd_snapshot.go`, `cmd_access.go:546-590`, `cmd_billing.go:92-175`;
   - the server test `internal/server/lifecycle_fix_test.go:369-420`.
2. **Build and tests.** I built all four binaries (`CGO_ENABLED=0`, guest for linux/amd64) into my scratchpad, not `bin/`, so the running e2e was left alone. `go vet ./...` is clean, and `go test ./...` passes (`test/e2e` skips without `CROWDVM_E2E`). I did not run e2e. When I finished, `e2e-run7.log` had not ended yet (no `EXIT=` line). It had no `FAIL` lines and had reached `boat-sdk-samples`.
3. **Own cluster** under `~/.local/share/review-opus-9`.
   - `crowdvm-server` on `127.0.0.1:9960` and one `crowdvm-node` (qemu/KVM, UI `127.0.0.1:9961`, 6 vCPU / 12 GiB / 3 sandboxes), image `ubuntu-24.04-crowdvm-v3`, guest binary freshly built.
   - Users `alice` (admin) and `bob`. Orgs `acme9` and `beta9`, both owned by alice.
   - One long-lived source sandbox `bx_kztd5ez3`. I saved `img9` from it three times (personal, `--org acme9`, `--org beta9`), writing a different `~/marker` before each save, so every deploy shows which disk it got.
4. **Fix matrix** (see the next section): 8 by-name GETs over the API (active wallet personal/acme9 × scope none/personal/acme9/beta9), 8 `new --from img9` deploys with the marker read back, the CLI save/info/shares/rm under `--org personal` and `BOAT_ORG=personal`, an org key, bob through a share code, and the active-org CLI path.
5. **Independent sweep.**
   - One read-only Opus sub-reviewer took the 12 non-API pages that no earlier review names: `environments__access`, `environments__choose`, `environments__versions`, `integrated-agents__conversations`, `billing__plan-changes`, `desktop-streaming__background`, `desktop-streaming__browser-only`, `windows-apps__run-apps`, `windows-apps__agents-and-forks`, `windows-apps__troubleshooting`, `platform-guide__agents` and `sdks__overview`. I re-checked every finding I kept.
   - Myself, mechanical: I pulled every `boat <cmd> [sub] … --flag` from `llms-full.txt` (255 pairs) and checked each flag against `crowdvm <cmd> [sub] --help`. Every miss was a false positive (a flag of the command run inside `exec`/`ssh`, or a subcommand flag such as `ai profile new --copy-from`) or an accepted N/A (`login --google`/`--agentid`, `billing cancel --keep-disks`). `env set --boat-credentials` is accepted as an alias.
   - Myself, live: `hosting__firewall` (the per-uid `runner` block: `runner` gets curl rc 7, `runner` to localhost gets 200, `user` gets 200, health stays `ok`), `long-running-tasks__auto-stop` (`ttlSeconds:null` on create and patch, the 30-day cap, `extend --hours 12` counts from now, fork defaults to 3600).

## Fix status

F = fixed. "live" = reproduced on my cluster.

| # | Finding | Status | Evidence |
|---|---|---|---|
| opus-8 B1 | `--org personal` can't find personal saved snapshots by name | **F (live)** | `snapshots.go:640-654`: an explicit scope that resolves to personal now uses `c.P.User.ID`. Live with `org switch acme9`: `crowdvm --org personal snapshot bx_kztd5ez3 img9` → `ready`, **rc 0**. `--org personal snapshot info img9` → the personal `snapshotId`. `--org personal snapshot shares img9` → 200. `BOAT_ORG=personal snapshot info img9` → the personal one. API `X-CrowdVM-Org: personal` → personal under both active wallets. Test: `lifecycle_fix_test.go:387-390`. |
| opus-8 B2 | `--org acme new --from NAME` deploys the personal snapshot | **F (live)** | `sandboxes.go:829` passes the scope and `resolveFrom` tries `org.ID` first (`snapshots.go:987-990`). The 8 deploys: active personal → none: `personal`, `--org personal`: `personal`, `--org acme9`: `acme`, `--org beta9`: `beta`. Active acme9 → none: `acme`, `--org personal`: `personal`, `--org acme9`: `acme`, `--org beta9`: `beta`. The wallet matched every time. Test: `lifecycle_fix_test.go:391-398`. |
| sonnet-6 N1 | Captured `host list` prints `_token` URLs | **F (live)** | `cmd_access.go:553` calls `a.rawOutput(cmd)`, and `:558-560` strips the token from `url`/`hostUrl`/`pathUrl` before both branches. Live in the sandbox: `L=$(host list)` → the table with `(gated)`, `(gated) (localhost)` and a public row, with no token. `host list \| cat` and `POST /commands {"command":"host list"}` → the same table. `host list --json` → no `_token`. `host url 3000` still prints the token. Outside, `crowdvm host list ID --json \| grep -c _token` → 0. |
| opus-8 n-c | `snapshot rm --yes` unknown flag | **F (live)** | `cmd_snapshot.go:184`. `snapshot rm img9 --yes` → `{"status":"deleted"}`, rc 0. |
| opus-8 n-d | `billing plan N --move-personal` unknown flag | **F (live)** | `cmd_billing.go:96-121,175`. `crowdvm --org acme9 billing plan 20 --move-personal` → `org.personal_moved`, `moved.sandboxes:1`, and the running sandbox now bills acme9 (still `ready`). Without an org: a clear usage error. `billing plan 20` alone → `no_plans`. |

### Regression checks (neighbouring paths)

| Path | Result |
|---|---|
| Share codes | Fine. An `--org acme9 snapshot share img9` code let bob (not in any org) deploy acme9's disk (marker `acme2`) into his personal wallet. Bob's `new --from img9` → `snapshot_not_found`. |
| Org keys | Fine. An acme9 org key: `GET /saved-snapshots/img9` → acme9's. The list shows only acme9. `X-CrowdVM-Org: personal` → `403 api_key_org_mismatch`. `POST /sandboxes {"from":"img9"}` deployed acme9's disk. |
| Saved-snapshot listing | Works (personal plus every org you are in, `snapshots.go:747-773`), but see n-1: the CLI can't tell same-named entries apart. |
| Explicit scope with a name missing from that org | Inconsistent, see n-2. |
| Active org, no `--org` | **Broken, see B1.** |

## Blocking finding

### B1. Under an active org, by-name saved-snapshot commands target the personal snapshot (SNAP-015, SNAP-022, ORG-007)

**What Boat documents:**
- `organizations__active-wallet.md:9-13`: every account has one active wallet. "A request that names no wallet bills the active wallet." "The CLI and account API keys use it."
- `snapshots__templates.md:182`: "A save goes to your active organization, or to the one that you pass with `--org`."
- `snapshots__templates.md:200-212`: a snapshot saved under an organization belongs to it. "The member who saved it, or an owner or admin of the organization, can remove it or make a share code."

**What CrowdVM does:**
- The server keeps the active org on the account (`users.active_org`), and `Ctx.Scope` returns it when the request names no wallet (`internal/server/orgs.go:131-141`).
- The save writes to that wallet (`handleSaveSaved`, `snapshots.go:783-816`). `new --from` looks in it first (`resolveFrom(u, req.From, org)`, `sandboxes.go:829`, `snapshots.go:987-990`).
- `savedLookup` (`snapshots.go:638-664`) sets `explicit` only for `?org=`, `?teamId=`, an org header or an org key. An active org doesn't count, so it searches personal first, then the newest match in any org. Five handlers use it: get `:776`, delete `:866`, create share `:893`, list shares `:913`, revoke share `:939`.
- The CLI's save polls `GetSavedSnapshot(name)` with no scope (`internal/cli/cmd_snapshot.go:119`).

**Live** (`org switch acme9`, no `--org`; personal, acme9 and beta9 each have an `img9` with different content):

| Command | Result |
|---|---|
| `GET /saved-snapshots/img9` (account key, no header) | the **personal** one (`ss_adna…`), while `X-CrowdVM-Org: acme9` gives `ss_fjqs…` |
| `crowdvm snapshot info img9` | the personal `snapshotId` `snap_7qvp…` |
| `crowdvm snapshot bx_kztd5ez3 img9` | The save went to acme9 (`GET …?org=acme9` → `status:"saving"`, new `updatedAt`). The CLI printed the **personal** snapshot (`snap_7qvp…`, `ready`, its old `createdAt`) and exited 0 while acme9's save was still running. |
| `crowdvm snapshot share img9` | the share is on the **personal** snapshot (`shares` under `--org personal` → `shr_7280…`; under `--org acme9` → `[]`) |
| `crowdvm snapshot rm img9 --yes` | `{"status":"deleted"}`. The **personal** `img9` is gone. acme9's (`snap_er2r…`) and beta9's remain. |
| `crowdvm new --from img9` | deploys acme9's disk (marker `acme`) |

**Why it matters:**
- `org switch` is how Boat says to work in an organization (`organizations__active-wallet.md:33-37`).
- A member who does that and runs the template recipe (`snapshot ID web-stack`, then `snapshot share web-stack` or `snapshot rm web-stack`) shares or deletes the wrong snapshot, and nothing reports an error. The delete can't be undone.
- The save reports `ready` before the team image is saved, so a script's next `new --from` gets the previous team image.
- It is server-side scoping only and doesn't depend on the LAN.
- The server tests cover only explicit scopes (`lifecycle_fix_test.go:369-420`). No test sets an active org.

**Fix:** in `savedLookup`, use the wallet that `c.Scope("")` returns whenever it is an org, whether the org was named or is the active one. Keep "personal, then orgs" only when the scope is personal and not explicit. That is the same order `resolveFrom` already uses, so one helper can serve both. Add a server test: same-named personal and org snapshots, `PATCH /orgs/active {org}`, then GET/DELETE/share by name with no header → the org's.

## Non-blocking findings

| ID | What | Evidence |
|---|---|---|
| n-1 | The CLI can't tell same-named saved snapshots apart. With three `img9` (personal, acme9, beta9), `crowdvm snapshots` prints three identical `img9 ready bx_kztd5ez3` rows, and `--json` has no `org`/`orgName`/`id`. The server sends `org`, `orgName`, `id` and `savedBy` (`snapshots.go:685-704`), but `client.SavedSnapshot` drops them (`internal/client/types.go:581-590`). Boat's spec has no org field on `SavedSnapshot`, so this is usability, not parity. It does make B1 harder to notice. | Live. |
| n-2 | An explicit scope falls back in create but not in lookup. With `only9` saved only under acme9, `crowdvm --org beta9 snapshot info only9` → `404 snapshot_not_found`, but `crowdvm --org beta9 new --from only9` deploys acme9's team image into a beta9 sandbox (`resolveFrom` falls back to personal, then any org, `snapshots.go:991-996`). A beta9 member who isn't in acme9 may then see that disk (org sandboxes can be shared). Boat says nothing about this case, so I don't count it. Suggest: an explicit org scope resolves names only in that org. | Live, `bx_gu87yaqs`. |
| n-3 | Boat's "block outgoing UDP for `user`" recipe (`hosting__firewall.md:44-48`) runs `sudo wg show wg0 endpoints`, and the sandbox has no `wg` (`sudo: wg: command not found`). The recipe's goal still holds, because CrowdVM's control path doesn't use a guest WireGuard tunnel (URL-020 ADAPTED). The ACCEPT line just fails on an empty `-d`. Cosmetic. | Live. |
| carried | sonnet-6 M1–M6, opus-8 n-a (no CLI-level `--org personal` test) and the older carry-overs. None was claimed fixed, and I didn't re-check them. | See sonnet-6 and opus-8. |

| n-4 | Boat's wallpaper recipes write to `~/.config/boat/wallpaper.png` (`desktop-streaming__background.md:13,20,26,34`). The guest watches that path (`internal/guest/desktop.go:537-541`), but nothing creates `~/.config/boat/`. Only `~/.config/crowdvm/` exists. | Found by my sub-reviewer, reproduced live: `ls -d ~/.config/boat` → no such directory, and Boat's `curl -fsSL -o ~/.config/boat/wallpaper.png …` → **rc 23**. A plain `scp` to that path fails the same way. Fix: create the directory in the image or at boot. Cosmetic feature, so minor. |
| n-5 | `sdks__overview.md:242-243` generates a client from `…/openapi/boat-v1.yaml`. CrowdVM serves only `/openapi/crowdvm-v1.yaml` and `/api/v1/openapi.yaml` (`internal/server/acct_openapi.go:36`). | Found by my sub-reviewer, reproduced live: `/openapi/boat-v1.yaml` → **404**, the other two → 200. SDK-009's ADAPTED reason is only branding. A one-route alias, the way `BOAT_*` env names are kept. Minor. |

The sub-reviewer's other 12 pages (environments access/choose/versions, conversations, plan changes, browser-only, Windows apps, platform-guide agents, SDK overview) came back clean. Examples: `env set --boat-credentials` is a hidden alias, `--proxy-ai-credentials` gives `ai_proxy_off`, `env upgrade --sandbox`, `events --follow --convo`, `interrupt`/`steer --convo`, `browser --profile`, and the `boat_sdk`/`BoatApi` shims. It accepted BILL-020, ENV-015/016/019, WIN-006 and WIN-008.

## Checked and fine

- `hosting__firewall`: ufw is active. The per-uid `runner` recipe blocks `runner` (curl rc 7) and keeps `runner` → localhost (200), `user` → internet (200) and sandbox health `ok`.
- `long-running-tasks__auto-stop`: create and `PATCH` with `ttlSeconds:null` → `archiveAfter:null`. `ttlSeconds:99999999` is capped to `2592000`. `extend --hours 12` → `archiveAfter` = now + 12 h. A fork with no `ttlSeconds` → `3600`.
- Every `boat … --flag` pair in `llms-full.txt` exists on the matching `crowdvm` command or is an accepted N/A (see Method).

## Disputed classifications

- **SNAP-015 / SNAP-022 / ORG-007** (`PARITY.md:309,316,513`): marked done, but under an active org the by-name commands act on the personal snapshot, and the save reports the wrong one (B1).
- **SNAP-018** (`PARITY.md:312`): fine now for every scope I tried. The cross-org fallback is n-2.
- **URL-014** (`PARITY.md:264`): fine now.
- **SDK-009** (`PARITY.md:680`, ADAPTED): the reason is branding, not a constraint. Serve `/openapi/boat-v1.yaml` as an alias too (n-5).
- **DESK-013** (`PARITY.md:238`): the watch is there, but Boat's recipe fails because the directory is missing (n-4).
- **URL-019/URL-020** (ADAPTED): accepted. The guest firewall can't cut the node's control path, and the Boat recipe still achieves its goal (n-3).

## What would flip this to AGREE

1. **B1:** with `org switch ORG` active and no `--org`, `snapshot ID NAME` waits for and prints ORG's snapshot. `snapshot info/share/shares/unshare/rm NAME` act on ORG's `NAME` when it exists. The same holds for `GET/DELETE /saved-snapshots/{name}` and its share routes with an account key and no header.
2. A server test for that: an active org, same-named personal and org snapshots, no header.

The n-items don't block AGREE.

## Not verified

- Windows nodes, VFIO GPU passthrough, real Claude/Codex/Bedrock credentials, real GitHub/Gitea CI, SMTP, `--direct-host`, the dashboard.
- `snapshot unshare` under an active org. It goes through the same `savedLookup` (`snapshots.go:939`), so it is inferred, not run.
- The final result of e2e run 7 (not finished when I stopped).

## Teardown

- I deleted every sandbox I created: `bx_kztd5ez3`, the 8 matrix deploys, bob's `bx_gr6th6ry`, the org-key deploy `bx_u9n3wmqp`, the fork `bx_3qc57yrd`, `bx_ysbszkhu` and `bx_gu87yaqs`. `list` was empty for both alice and bob.
- I stopped my node and server by PID, then confirmed that none of my processes or QEMU VMs were left and that ports 9960–9969 were free. The QEMU VMs still running belong to the e2e run and the other reviewer, and I didn't touch them.
- I removed `~/.local/share/review-opus-9` and my scratchpad binaries and files.
- I touched no other process and joined no other server.

## TL;DR

DISAGREE. All five claimed fixes hold live:
- `--org personal` finds personal saved snapshots;
- `--org ORG new --from NAME` deploys ORG's image (8/8 scope combinations correct);
- `$(host list)` prints the tokenless `(gated)` table;
- `snapshot rm --yes` and `billing plan N --move-personal` work.

Share codes and org keys didn't regress. One blocker remains, in the case the fixes left out: with `org switch ORG` active and no `--org`, `snapshot info/share/rm NAME` act on the *personal* `NAME`, and `snapshot ID NAME` saves to ORG but reports the personal one as `ready`. Meanwhile `new --from NAME` deploys ORG's. Live, `snapshot rm img9 --yes` deleted the wrong snapshot. The cause is that `savedLookup` ignores the active org (`internal/server/snapshots.go:638-664`), while the save and `resolveFrom` honour it.
