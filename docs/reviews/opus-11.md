# Review opus-11: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (five blockers, all small fixes)

The claimed fixes mostly hold:
- `env rename` now shows the new name on pinned sandboxes (sonnet-8 B1).
- The `env --json` `effective`/`warning` fields, `/openapi/boat-v1.yaml`, org names in the OWNER column and the `~/.config/boat/` folder are all there.
- A secret file inside a repository folder no longer stops the clone (opus-10 B1). Create, live `env upgrade`, fork and resume all end with `.git`, the repository's files and the secret file in place.

But:
- the opus-10 fix is incomplete: the ready gate still ignores that repository;
- Boat's wallpaper recipe still does not change the desktop, although the folder now exists;
- the team's own e2e run 9 fails in `environments` (`EXIT=1`).

I traced that e2e failure to a race between repository clones and the guest's network, and reproduced it live on 3 of 7 creates.

**B1. Environment repositories are often not cloned: the clone runs before the guest has a network.**
- Boat, `environments__repositories.md:5`: "Each new sandbox then clones it into `/home/user/<repository name>`." PARITY LIFE-009 (REQUIRED): "At ready: … env repos cloned".
- `crowdvm-guest.service` starts `After=local-fs.target systemd-udevd.service` (`images/rootfs/minimal/etc/systemd/system/crowdvm-guest.service:6`). `BootBackground` runs `runRepos` right away (`internal/guest/boot.go:203`), with no wait for a route and no retry.
- Live, journal of a failed create:
  - `crowdvm-guest … listening on [::]:7070` at 1.327 s;
  - `enp0s3: DHCPv4 address 10.0.2.15/24 … acquired` at 1.404 s;
  - `/var/lib/crowdvm/setup.json`: `"status":"failed","error":"git clone: … fatal: unable to connect to 192.168.178.31: … errno=Network is unreachable"`.
- 3 of my 7 creates lost a repository this way:
  - with and without a secret file;
  - small repository (`app.git`) and large repository (`big.git`, 242 MB);
  - the other 4 cloned fine.
- e2e run 9 fails at the same place: `features_test.go:105` "repo cloned before ready", `cat: /home/user/proj/README.md: No such file or directory`.
- Nothing reports it:
  - `info --json` has `setupStatus:null` and no repository status;
  - the error lives only in the guest's `setup.json`.
- Nothing repairs it:
  - resume does not clone (`clone=false`, `boot.go:203`);
  - `env upgrade` clones only repositories whose path is new (`internal/guest/apply.go:301-305`).
- Supportable on a LAN: guest ordering. Fix options:
  - wait (bounded) for a default route before the clones, or order the unit after `network-online.target` for that phase only;
  - and/or retry a clone that fails with a connect error.

**B2. The opus-10 fix is incomplete: with a secret file in the repository folder, the sandbox reports ready before the clone.**
- `missingRepos` still asks whether the folder exists (`internal/guest/boot.go:138-145`, `os.Stat(a.real(repoVirtPath(r)))`), not whether `.git` exists (as `runRepos` now does, `internal/guest/setup.go:319`).
- The secret file has just created the folder (`boot.go:107-109`), so `missingRepos` returns false and `clonesFinished()` runs at `boot.go:127-129`. Health then says `reposCloning:false` before `runRepos` starts, and the node opens the clone gate at once (`internal/node/vms.go:381`).
- Live: environment `bigs` (`big.git` plus `big/backend/.env`). The node logged `ready=true` in the same millisecond as `guest healthy`, with no "waiting for the environment's repositories" line. Right after `ready`, `ls ~/big` showed `backend` only (`NO_GIT`).
- The control case (`bigc`, no secret file, on a create where the network came up in time) logged "waiting for the environment's repositories before ready" and became ready 13 s later with the clone in place.
- That clone also failed later because of B1, so B2 is shown by code plus the missing gate wait.
- Why it matters: this is exactly Boat's `.env`-in-a-clone recipe (`environments__secrets.md:107`). An agent that starts at `ready` sees an empty repository until the clone lands.
- Fix: one line. Test for `.git` in `missingRepos`, the same check as `setup.go:319`.

**B3. Boat's wallpaper recipe still does nothing: the guest writes the setting to the wrong D-Bus session.**
- Boat, `desktop-streaming__background.md:7-8`: "Put a PNG file at `~/.config/boat/wallpaper.png` … The desktop shows it at once." PARITY DESK-013 (REQUIRED).
- The folder now exists (`internal/guest/creds.go:97-98`), and `crowdvm scp wp.png ID:/home/user/.config/boat/wallpaper.png` returns rc 0, even on a `--no-env` sandbox.
- But the screen stays at the default colour `srgb(29,39,58)`, whether I check by `scrot` or `import -window root`, and with `~/.config/crowdvm/wallpaper.png` too.
- Cause:
  - The image's session script runs `dbus-launch --exit-with-session startxfce4` (`images/rootfs/full/usr/local/lib/crowdvm/desktop-session:16`), so `xfdesktop` listens on a private bus (`/tmp/dbus-…`).
  - The guest's `ensureBus` prefers `/run/user/1000/bus` (`internal/guest/desktop.go:168-176`).
  - `applyWallpaper` (`desktop.go:577-610`) therefore writes `last-image` into a second `xfconfd` (two were running, pids 817 and 1053). That instance said `last-image = …/.config/boat/wallpaper.png`. On xfdesktop's bus it was still `/usr/share/backgrounds/xfce/xfce-shapes.svg`.
- Proof: running the same `xfconf-query -s` with xfdesktop's `DBUS_SESSION_BUS_ADDRESS` turned the screen `#FF0000` at once.
- Fix options: read the bus address from the running `xfdesktop` (or from the session script, written to a file), or start the session on `/run/user/$UID/bus`.

**B4. `--fail-fast` answers after 20 s, not ~1.5 s: the CLI and SDKs retry the 503.**
- Boat, `machines__fail-fast.md:20`: "The call then answers in about 1.5 seconds", and `:32`: "Retry, or fall back, immediately." PARITY SIZE-011 and CLI-042 (REQUIRED, "~1.5 s").
- The server is right: `POST /sandboxes {"failFast":true}` → 503 `no_ready_machine` in 2 ms (sub-agent).
- The client is not: `internal/client/client.go:317` treats every 503 as retryable, and `MaxRetries` is 4 (`:63`), honouring `retryAfterSeconds:5`.
- Live, node full (8/8 vCPU): `time crowdvm new --fail-fast --no-env --json` → `no_ready_machine` after **20.026 s**. The sub-agent saw the same for `fork --fail-fast`.
- The TypeScript SDK retries every 503 (`sdk/typescript/src/client.ts:136`), and the Python SDK documents the same (`sdk/python/README.md:10`). Boat's own fallback sample (`machines__fail-fast.md:54,65`) therefore waits about 15 s.
- Fix: do not retry `no_ready_machine` (or any 503) when the request set `failFast`.

**B5. A fork that waits for capacity breaks when its source is deleted.**
- Boat, `cli-reference.md:508-510`: "boat keeps snapshot data that something else also uses … A sandbox that you **forked** from this one … restores from the same physical snapshot objects."
- Live, which I hit by accident:
  - I forked `bx_9t22eq7r` (the CLI returned `bx_jfagktn2`, `created`), then deleted the source while the fork was still queued (the node was full).
  - The server logged `live snapshot failed, forking from the last snapshot … vm not found`. The source's head snapshot `snap_53sat…` had been purged one second earlier (`purged_at` 14:41:36.085).
  - The fork went to `error`: `qemu-img info http://127.0.0.1:41893/b/snapshot/snap_53sat…: … 404`.
- Cause: a live fork records its `head_snapshot_id` only when it starts (`internal/server/lifecycle.go:416`). Until then nothing holds the source's chain: the GC roots are only the heads of undeleted sandboxes and saved snapshots (`internal/server/snapshots.go:271-275`). The fallback then uses a purged snapshot (`lifecycle.go:336-342`).
- A fork whose VM is already running is protected. My clean retry (fork, then delete the source at once) kept the fork, and the delete waited about 4 min for the layer merge. So the gap is the queued or pre-start window, which is common on a busy crowdsourced pool.
- Fix: set the fork's `head_snapshot_id` to the source's head when the fork row is created (replace it with the live snapshot later), or count queued forks' sources as GC roots.

## Method

1. **Read.**
   - `opus-10.md` and `sonnet-8.md` (blockers, minor items, flip criteria), and the citation counts of every `.ref/pages/*.md` name in `docs/reviews/` and `docs/gaps/`.
   - The code behind each claimed fix:
     - `internal/guest/setup.go` (`runRepos` `:302-410`, `mergeInto` `:418`, `setup_merge_test.go`);
     - `boot.go` (`Boot`, `missingRepos`, `BootBackground`) and `apply.go` (live config);
     - `internal/server/feat_env.go:778-780` (rename) with its test `feat_env_test.go:231-237`;
     - `internal/client/types.go:822-823`, `internal/cli/cmd_snapshot.go:451-464`, `internal/cli/cmd_agent.go:294-305`;
     - `internal/server/acct_openapi.go:36-37`, `internal/guest/creds.go:97-98`, `desktop.go`.
2. **Build and tests.**
   - I built all four binaries into my scratchpad with the Makefile's flags (I did not overwrite `bin/` while e2e was running).
   - `go test ./...` passes.
   - I did not run e2e. I read `e2e-run9.log`: **`--- FAIL: TestE2E/environments`** at `features_test.go:105` ("repo cloned before ready"), every other group passes, `bootloop` is skipped, `EXIT=1`.
3. **Own cluster**, ports 9960 (server) and 9961 (node UI), plus `git daemon` on `192.168.178.31:9965`. Data was in `~/.local/share/review-opus-11`.
   - One qemu/KVM node (8 vCPU / 16 GiB / 4 sandboxes), `net.allow 192.168.178.31/32`, image `ubuntu-24.04-crowdvm-v3`, freshly built guest.
   - Users `alice` (admin) and `bob` (for the sub-agent). Org `acme11`.
   - Bare repositories `app.git` (`backend/app.py`, `backend/.env.example`), `lib.git` (`cfg/app.yml`) and `big.git` (242 MB).
4. **Fix checks, live** (table below).
5. **Independent sweep.**
   - **Me, live:**
     - repository clone timing and readiness (B1, B2);
     - fork and delete ordering (B5);
     - the desktop wallpaper path end to end, with screenshots (B3);
     - `--fail-fast` timing (B4, confirming the sub-agent).
   - **Sub-agent** (Opus, live on my cluster as `bob`), on the least-cited pages:
     - `windows-apps__*` (all five), `recipes__agent-on-repo`, `machines__fail-fast`;
     - `data-retention__delete`, `data-retention__snapshots-and-monitoring-off`, `long-running-tasks__background-commands`.
     - It found B4, which I reproduced, plus m1 and m2.

## Fix status

"live" = reproduced on my cluster.

| # | Claim | Status | Evidence |
|---|---|---|---|
| opus-10 B1 | A secret file under a repository folder no longer stops the clone | **Partly fixed (live)** | `setup.go:319` skips only when `.git` exists. When the folder is already there it clones into `<repo>.crowdvm-clone` and merges, and existing files win (`:323-329`, `:350-356`, `mergeInto` `:418`). Unit test `setup_merge_test.go`. |
| | | | Live create, env `r11` = `app.git` + `app/backend/.env`: `~/app` holds `.git`, `README.md`, `backend/app.py`, `backend/.env.example` and `backend/.env` (`SECRET=xyz`, mode 600, owner `user`). `git status`: `?? backend/.env`. No leftover `.crowdvm-clone`. |
| | | | **But** the ready gate ignores that repository (B2, `boot.go:140`). |
| | live `env upgrade` | **F (live)** | Added `lib.git` and secret `lib/cfg/local.env`, then `env upgrade r11 --sandbox ID` → `upgradedLive:1`. `~/lib` holds `.git`, `lib.txt`, `cfg/app.yml` and `cfg/local.env`, status `done`. |
| | regressions: fork, resume, re-boot | **None found (live)** | **Fork:** `lib` (with `.git`) kept its local edit and an untracked file, with no re-clone. `app`, whose clone had failed in the source (B1), was cloned and merged in the fork (`?? backend/.env`). |
| | | | **Stop/resume of the fork:** both repositories unchanged, no `.crowdvm-clone`, nothing re-cloned (`clone=false` on resume). |
| | | | **Small change:** a fork now re-clones a repository folder whose `.git` the user deleted (it used to skip any existing folder), keeping the user's files. I accept it (m4). |
| sonnet-8 B1 | `env rename` shows on pinned sandboxes | **F (live)** | `feat_env.go:780` updates `sandboxes.environment` by `environment_id`. Test `feat_env_test.go:231-237`. Live: `env rename r11 r11b`, then `info --json` → `"environment":"r11b","environmentVersion":5` on a running sandbox and on a just-resumed fork. `ls --json` and text `info` (`env: r11b v5`) match, `env list` shows `r11b … v5:2`, and a new fork → `r11b` v5. |
| sonnet-8 m1 | `env --json` has `effective`/`warning` | **F (live)** | `types.go:822-823`. `env list --json` → `effective:{…}`, `warning:null`; `env info` carries `effective`. |
| opus-10 m1 | `~/.config/boat/` exists for the wallpaper recipe | **Folder fixed, recipe still broken (live)** | `creds.go:97-98`. The folder exists (also on `--no-env`), and `scp` to it works. The desktop never shows the image (B3). |
| opus-10 m2 | `/openapi/boat-v1.yaml` alias | **F (live)** | `acct_openapi.go:37`. `/openapi/boat-v1.yaml` 200 `application/yaml`, `.json` 200. |
| opus-10 m4 | Org names in OWNER | **F (live)** | `cmd_snapshot.go:451-464`. `snapshot list`: `img11  acme11  ready …`. |
| opus-10 m9 | `crowdvm events` tool lines | **F (code)** | `cmd_agent.go:296-303` reads `use.name`, then falls back to `name`/`type`, and appends ` (error)` on `result.is_error`. Not run live (no agent credentials). |

## New findings

### Blocking

| ID | What | Evidence |
|---|---|---|
| **B1** | Env repositories intermittently not cloned (`Network is unreachable`), silently, never retried | `internal/guest/boot.go:203`, `images/rootfs/minimal/etc/systemd/system/crowdvm-guest.service:6`. Live: 3 of 7 creates; e2e run 9 fails at `test/e2e/features_test.go:105`. |
| **B2** | Secret file in a repository folder → ready before the clone (opus-10 fix incomplete) | `internal/guest/boot.go:138-145` (folder check) vs `internal/guest/setup.go:319` (`.git` check); gate `internal/node/vms.go:381`. |
| **B3** | Wallpaper recipe has no visible effect (wrong D-Bus) | `internal/guest/desktop.go:168-176`, `:577-610`; `images/rootfs/full/usr/local/lib/crowdvm/desktop-session:16`. Live screenshots. |
| **B4** | `--fail-fast` takes 20 s in the CLI (~15 s in the SDKs) | `internal/client/client.go:317`, `:63`; `sdk/typescript/src/client.ts:136`. Live 20.026 s. |
| **B5** | Deleting a source breaks its queued fork (snapshot purged) | `internal/server/lifecycle.go:336-342`, `:416`; `internal/server/snapshots.go:271-275`. Live: `bx_jfagktn2` → `error`, 404 on `snap_53sat…`. |

### Minor (do not block AGREE)

| ID | What | Evidence |
|---|---|---|
| m1 | (sub-agent) Snapshots are never listed with `origin:"template"`. Boat `data-retention__snapshots-and-monitoring-off.md:19` says a snapshots-off sandbox made from a source lists the source's snapshots that way; PARITY DATA-010 claims it. | `internal/server/snapshots.go:392` hard-codes `"captured"`. Live: `new --from wine-apps --no-snapshots` → `[]` (the sub-agent notes it had deleted one underlying snapshot first; the code never emits `template` either way). |
| m2 | (sub-agent) A timed-out synchronous command returns `timedOut:true` without `retryable:false` (`long-running-tasks__background-commands.md:224`; PARITY CMD-002). | Code and live. |
| m3 | A failed repository clone is invisible through the API: `setupStatus`/`setupError` stay `null`, and no per-repository status reaches `info`. Boat documents no per-repository field, so this is not a parity gap, but it is why B1 goes unnoticed. | Live. |
| m4 | A fork now re-clones into a repository folder that has no `.git` (the user deleted it) and merges the clone in. A file in the folder where the clone has a directory makes the merge fail (`os.Rename` → ENOTDIR in `mergeInto`). Rare. | `setup.go:319`, `:418-438`. Code. |
| m5 | A sandbox delete that must wait for a fork's layer merge took about 4 min, with no progress line. Correct (Boat says it keeps shared data), just slow. | Live. |

## Disputed classifications

- **LIFE-009** (`PARITY.md:81`, REQUIRED, done): not true while B1 and B2 are open.
- **ENV-021 / ENV-027**: the "secret file inside a clone" case now works, apart from readiness (B2).
- **DESK-013** (`PARITY.md:238`): third round. The folder is fixed, but the watch writes to a session bus the desktop does not use (B3).
- **SIZE-011 / CLI-042** (`PARITY.md:138`, `:729`): "~1.5 s" holds for the raw API only (B4).
- **DATA-010** and **CMD-002**: as claimed, not met (m1, m2). Small.
- **Windows apps** (sub-agent): Boat's verbatim `install-wine.sh` installed `wine-11.21 (Staging)`. The Sysinternals recipe, saving a Wine snapshot and `new --from` it all worked. No dispute.
- I accept the N/A rows (payments, third-party identity, operator facts) and the ADAPTED rows I touched (ENV-027 git URL instead of a GitHub app; `owner/repo` without a connected source → `unknown_repository`).

## What would flip this to AGREE

1. **B1:** repository clones wait for networking (bounded) or retry connect failures. Ten creates in a row with an environment repository all clone, and e2e `environments` passes.
2. **B2:** `missingRepos` checks `.git`. With a secret file `<repo>/x/.env`, `ready` comes after the clone; add a guest test.
3. **B3:** after `scp` to `~/.config/boat/wallpaper.png` the desktop shows the image within a few seconds; deleting it brings back the default.
4. **B4:** `crowdvm new --fail-fast` (and fork, resume, SDK create) returns `no_ready_machine` in ≤ 2 s on a full pool.
5. **B5:** a fork queued behind capacity survives `delete` of its source, and starts with the source's data.

m1 and m2 are cheap and would close PARITY rows that claim more than the code does.

## Checked and fine

- **Secret files and repositories:**
  - modes and owner;
  - merge keeps repository files and secret files side by side;
  - live upgrade adds a repository with its secret file;
  - fork keeps local edits;
  - resume leaves no temporary folders;
  - `env rename` on running, resumed and newly forked sandboxes.
- **Readiness gate** for a repository whose folder does not exist (13 s wait, then ready with the clone).
- **Fork safety:** a running fork survives deletion of its source; the delete waits for the layer merge.
- **Small claims:** `/openapi/boat-v1.{yaml,json}`; OWNER column names; `env list --json` `effective`/`warning`.
- **Sub-agent, live:**
  - Wine install and apps, snapshot and `new --from`;
  - the agent-on-repo recipe (`env add-repo … --branch`, `env set` flags, `prompt` → `provider_not_configured`, `extend`, `steer` → `no_running_turn`, `events --follow`);
  - delete with confirmation header, `bdop_` operations, `deletion status`, `snapshot delete --all`;
  - snapshots off (`snapshots_disabled`), monitoring off (`monitoring_off`), account policy (`session_required`, `monitoring_off_for_account`);
  - background commands: detach, status, attach, kill, `--timeout 0`, stream offsets and heartbeat, SIGTERM then SIGKILL, `invalid_timeout`, state after a guest restart.

## Not verified

- Windows nodes, GPU passthrough, real agent credentials (so no live `events` tool lines), SMTP, the dashboard, a second node.
- B1's exact rate on other hardware. On this machine it was 3 of 7; it depends on how fast DHCP finishes against the guest agent.
- B2's "ready with an empty repository" on a create where the network was up: every `bigs` create I ran hit B1 first. The gate's skip is certain from the code, and the node log shows no wait.
- `resume --fail-fast` and SDK `failFast` live (code read only).

## Teardown

- I deleted every sandbox I created, the `img11` saved snapshot and the environments `r11b`, `bigs` and `bigc`.
  - The sub-agent deleted its sandboxes, snapshot and key, and confirmed `ls` and `snapshot list` were empty.
  - The org `acme11` went with the data dir.
- I stopped my node (pid 341123), server (341015) and `git daemon` (347654) by PID, and checked that no qemu or other process had my data dir in its argv. Ports 9960–9969 are free.
- I deleted `~/.local/share/review-opus-11`.
- I touched no other process (the server and node on 9970/9971 belong to the other reviewer) and joined no other server.
- I edited no repo file except this report.

## TL;DR

DISAGREE, with five blockers.
- **Holding:** rename, `env --json`, the OpenAPI alias, OWNER names, events code, and the merge clone (create, live upgrade, fork, resume).
- **New:**
  - the guest clones environment repositories before it has a network (3 of 7 creates failed, silently, and e2e run 9 fails on it);
  - the ready gate still ignores a repository whose folder holds a secret file;
  - the wallpaper recipe writes to the wrong D-Bus, so the desktop never changes;
  - `--fail-fast` takes 20 s because the client retries the 503;
  - deleting a source breaks a fork that is still queued.
