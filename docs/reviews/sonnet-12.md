# Review sonnet-12: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. I used no sub-agents.

## Verdict: DISAGREE (two blockers, both small and LAN-safe)

All five fixes claimed from `sonnet-11` (B1–B3) and `opus-13` (B1) hold in my live test. I found no regression next to them.

My sweep of sizes and capacity, auto-stop, limits and credits, the node app and the `crowdvm` skill found two behaviours that are wrong. Neither needs anything from the internet.

- **B1.** A node whose owner **paused** it (`crowdvm-node config set paused true`, or the node UI's "Pause new sandboxes") is still chosen by the scheduler. A `failFast` create or fork is answered `202`. The sandbox then ends in state `cancelled` and the attempt counts as a start. Boat promises `503 no_ready_machine`, nothing created, nothing counted (`machines__fail-fast.md`). Pausing is the documented way to stop taking sandboxes (`docs/guide/node-owners.md`, "Day to day").
- **B2.** `crowdvm new --type large` (the skill's core recipe) and `crowdvm new --type xlarge` hang silently when no node in the pool is big enough. The server knows (`GET /limits` says `no node in the pool is big enough for xlarge`), but `POST /sandboxes` answers a retryable `503 out_of_capacity` with the reason `n1: full`, and the CLI retries it quietly for up to 15 minutes.

## Method

1. **Read.** `sonnet-11.md` and `opus-13.md`, `docs/PARITY.md` (SIZE, RUN, BILL, LIFE rows), `docs/guide/node-owners.md`, the skill and `crowdvm agent-instructions`, and Boat's `machines*.md`, `long-running-tasks__auto-stop.md`, `limits.md`.
2. **Build and tests.**
   - `make build` is clean, and `go vet ./...` is clean.
   - `go test -count=1` on every package except `test/e2e` passes, with 0 FAIL.
   - I did not run e2e.
3. **Own cluster.**
   - One server on 127.0.0.1:9970.
   - One qemu/KVM node on UI port 9971, image `ubuntu-24.04-crowdvm-v3`.
   - I started it with the join flow, with a full benchmark. I first gave it 6 vCPU, 12 GiB and 3 sandboxes, and later 10 vCPU, 26 GiB and 4 sandboxes, with a node restart. Running VMs survived the restart.
   - Data dir `~/.local/share/review-sonnet-12`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated.
   - Users `admin` and `bob`, org `acme`. I later restarted the server with `--enforce-credits`.
   - A second node (`n2`, `--skip-bench`) was only joined, to read its default capacity, then removed.
4. **Fix checks.** Live on this cluster. The dashboard was **not** opened in a browser (see "Not verified").
5. **Sweep.** Boat's `machines`, `machines__change-size`, `machines__fail-fast`, `long-running-tasks__auto-stop` and `limits` pages, run on my cluster. I also ran the `crowdvm` skill's recipes as an agent would, on live sandboxes.
6. **Teardown.** I deleted every sandbox, stopped the node and server by saved PID, and confirmed nothing listens on 9970–9979 and no qemu or crowdvm process of mine is left. I deleted my data dir (it held a read-only Go module cache created by `go test` under my isolated `HOME`; I had to `chmod -R u+w` first). I edited no repo file except this report. I did not run e2e and did not touch the other reviewer's processes.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| sonnet-11 B1 | Boat's "Keep a hostname" recipe survives resume | **Fixed (live)** | I ran the recipe: `set-hostname.service` with `hostnamectl set-hostname my-sandbox`, `systemctl enable --now`. After `stop`, then `resume --type default` (a new machine), `hostname` and `hostnamectl` both said `my-sandbox`, not the sandbox name. Code: `setHostname` returns early when an enabled unit of the user sets it (`internal/guest/boot.go:228`, `userSetsHostname`). I did not re-test a fork. |
| sonnet-11 B2 | Images of profile-gated Compose services are saved | **Fixed (live)** | `compose.yaml` with `web` (nginx:alpine, `up -d`) and `tools` (busybox:1.36, `profiles: ["tools"]`, `docker compose --profile tools run --rm tools true`). After stop and resume, `docker images` showed `nginx:alpine` and `busybox:1.36`. `alpine:latest` (from `docker run --rm`, no container, no Compose project) was pruned, as it should be. Code: `--profile "*"` at `internal/guest/docker.go:330`. |
| sonnet-11 B3 | `snapshot pull` writes `docker/` | **Fixed (live)** | `crowdvm snapshot pull <id> -o DIR` → `DIR/home_user` and `DIR/docker/vol1/f` (content `dv`). Code: `internal/cli/cmd_snapshot.go:334-340` asks the tree for `__dockervol__/<name>` entries. |
| opus-13 B1 | Dashboard Saved snapshots: list and row actions use the same wallet | **Fixed (live, API level)** | With `X-CrowdVM-Org` (the header `web/js/api.js:17-21` sends), `GET /saved-snapshots` listed `["rv-personal"]` viewing Personal, `["rv-org"]` viewing the org, and both with no header. `…/shares` answered 200 for rows in the list and 404 for rows outside it, in every view. `DELETE /saved-snapshots/rv-org` in the Personal view is 404, as the row is not in that view. Code: `handleListSaved` scopes by header (`internal/server/snapshots.go:778-787`) and matches `savedLookupAny` (`:647-663`). |

## New missing or broken supportable features

### B1. A paused node still gets placements, and `failFast` breaks (SIZE-011, `machines__fail-fast.md`)

- **Boat says.** With `failFast` the call answers in about 1.5 s with `202` or `503 no_ready_machine`. On `503`: "boat does not create a sandbox or a fork … You pay nothing. The attempt does not count against your start limits."
- **CrowdVM.**
  - Only the node knows it is paused (`internal/node/vms.go:152`, `node is paused by its owner`). The server never learns it: `paused` is not in the node info (`crowdvm nodes --json` has no such key) and the scheduler does not read it.
  - It therefore places the sandbox on the paused node, and the node refuses the boot.
  - With `failFast`, the server has already answered `202`. `abortBoot` then moves the sandbox to `cancelled` (`internal/server/lifecycle_place.go:142-150`), after `recordStart` ran (`internal/server/sandboxes.go:984`).
- **Live.**
  - I paused the node with `POST /api/pause {"paused":true}` on the node UI (it works only with the `X-Crowdvm-Ui` header, which is the CSRF guard) and read `crowdvm-node config get paused` → `true`.
  - `POST /sandboxes {"failFast":true,"type":"small"}` → `HTTP 202`.
  - Seconds later: `GET /sandboxes/bx_m9gtdr7c` → `state:"cancelled"`, `error:"no_ready_machine: the node had no room … node is paused by its owner"`.
  - `crowdvm new --fail-fast` printed `sandbox_cancelled: …` instead of `no_ready_machine`.
  - The `starts` table gained a row for each cancelled attempt, so they count against start limits. Two cancelled attempts left two rows, next to the two real starts.
  - Contrast: a full pool, a drained node (`crowdvm nodes drain`) and `--min-tier platinum` all answer an instant `503 no_ready_machine` with no row.
  - Without `failFast`, a create on a paused node queues until the owner resumes (that part is fine).
- **Why supportable.** It is plain bookkeeping. The node already knows the flag; it only has to put it in its info or heartbeat. Nothing external is involved.
- **Fix.** Send `paused` in the node info, treat it like `drain` in the scheduler (and in `GET /limits` `machineTypes`: "no node is online" is also the reason shown for a drained node), and show it in `crowdvm nodes`. Add a server test for `failFast` against a paused node.

### B2. A size no node can ever host hangs the CLI, with a wrong reason (SIZE-001/011, `machines.md`; skill "Core loop")

- **Boat says.** All four sizes always exist. The skill's first recipe is `crowdvm new --type large --json`, and the skill says `no_ready_machine`: "Retry, use a smaller `--type`".
- **CrowdVM.**
  - On a pool where every node is smaller than the size (a laptop node: 6 vCPU and 12 GiB cannot host `large`, 8/16), `crowdvm limits` already says `{"type":"large","available":false,"reason":"no node in the pool is big enough for large"}` (`internal/server/scheduler.go:292`).
  - `POST /sandboxes {"type":"large"}` answers at once, `503 out_of_capacity`, `"No node has room for a large sandbox right now."`, with `details.nodes: ["n1: full"]`, `retryAfterSeconds: 30`. `couldEverPlace` is false (`internal/server/sandboxes.go:974-982`), but the code and reason say "full right now", and the sandbox is not named impossible.
  - `internal/client/client.go:317-318` retries every `503` except `no_ready_machine`, so the CLI waits and retries.
- **Live.** `timeout 20 crowdvm new --type xlarge --json` (xlarge needs 16 vCPU; my node had 10) printed nothing for 20 s (JSON mode shows no `waiting` event either: that event only appears for a sandbox that was accepted). It ended with `{"code":"interrupted"}` from my `timeout`. The same call with `--fail-fast` returns `no_ready_machine` in 4 ms. The first run on my 6 vCPU node did the same for `large`.
- **Why supportable.** An immediate, non-retryable answer needs only data the server already has.
- **Fix.** When `couldEverPlace` is false, answer a non-retryable error (for example `409 type_unavailable`, or the same `no_ready_machine` with `details.reason:"no node in the pool is big enough for large"`, `retryable:false`) naming the largest size the pool offers. The skill's error table can then say "use a smaller `--type`". The `nodes` detail should say `too small`, not `full`.

## Minor (not blocking)

- **m1. `type_too_small` can miss a fresh write.**
  - The check (`internal/server/lifecycle.go:1224`) reads the guest's last disk sample (`internal/server/lifecycle_notify.go:66`). The guest sends a sample every 30 s, not at stop.
  - I wrote 58 GB on a `large`, stopped it within 30 s, and `resume --type default` was accepted (70 GB on a 51 GB size; the sandbox keeps the 119 GB virtual disk). With a sample older than 30 s the same call was refused with `409 type_too_small`, correctly.
  - Boat's own text says "let the sandbox make a new snapshot, and try again", so staleness is accepted there too. A `df` sample at prepare-stop would close it.
- **m2. `crowdvm info`/`list --json` drop the `waitingForCapacity`, `statusMessage` and `waitDeadline` fields** that `GET /sandboxes/{id}` returns for a queued boot. A queued fork shows only `state:"cloning"` through the CLI. The API has them.
- **m3. `crowdvm nodes info`, `update`, `drain` take only the node id.** `node-owners.md` writes `<node>`, and a node owner knows the name. `--node n1` on `new` accepts the name; `crowdvm nodes info n1` answers `Node not found`.
- **m4. `crowdvm limits` shows `restrictions.restricted: false` while `allowedSizes`, `maxTtlSeconds` are set** (policy set by `admin policy`). The caps themselves work.
- **m5. The skill says "every command takes `--json` (on automatically when piped)" and errors end with `{"event":"error"…}`.** `crowdvm exec` prints a plain `error: …` when piped without `--json` (its stdout is the command's output); with `--json` or `--json` before the subcommand it prints JSON. `crowdvm push` of a non-git folder that contains a unix socket fails with `archive/tar: sockets not supported` instead of skipping it.
- **m6. `crowdvm new --from NAME --count 2` prints `created` lines and no `ready`.** The skill's `jq 'select(.event=="ready")'` pattern finds nothing. The sandboxes do come up.
- **m7. Re-running `crowdvm-node bench --quick` replaces the full benchmark on the server** (platinum 775 → gold 675, from a lower disk reading). The guide says quick is "less accurate", so this is documented.
- **m8. `crowdvm-node config set cpus 10` is silent.** The guide says restart to apply; the command could say so.
- **m9. `GET /limits` still shows `grace.active:true` for about 20 s after a grant** (it cleared on the next tick). The `grace_started` notice is not dismissed.

## Disputed or accepted classifications

- **SIZE-004 / BILL-006 (ADAPTED).** Allowed-sizes policy: `default` for an admin policy of `allowedSizes=small` → `403 machine_class_plan_required` with `details.policy:"admin"`, `allowedSizes`. `limits.machineTypes` marks `not_allowed_by_policy`. Max TTL: `ttlSeconds:600` over a 120 s cap → `auto_stop_required`. Both accepted as the stand-in for Boat's plan gates.
- **BILL-008/009 (REQUIRED).** `maxActive=1` → `429 limit_reached` with `details.activeSandboxes/limit`. `startsPerMinute=3` → `429 rate_limited` with `window`, `limit`, `cause:"plan"`, `nextStartAt`, `retryAfterSeconds` and a `Retry-After` header. A `limit_reached` attempt is not counted. Accepted. I did not run `member_limit_reached`, `team_member_cap_reached` or `IP_RATE_LIMIT_EXCEEDED` live (unit tests exist: `orgs_test.go`, `misc_test.go`).
- **BILL-006 (ADAPTED).** With `--enforce-credits`, a wallet pushed to −0.21 credits got a `low_credit` notice and a `grace_started` notice ("24 hours … then they are stopped (with a snapshot)"). Running sandboxes kept running. `limits` read `canStart:false, blockedReason:"billing_required"` and `grace.active:true`. `new`, `new --fail-fast` and `resume` answered `402 billing_required`. After `admin credits grant 5`, starts worked. Accepted as a credits version of Boat's flow. I did not wait 24 hours for the forced stop.
- **SIZE-006 (REQUIRED).** `df -h /` reads 60 G, then the filesystem grows after boot to about 119 G for `large` (a few seconds). Boat's "51 GB / 111 GB" is the user data limit with the OS excluded. Accepted. After a shrink, the sandbox keeps the bigger virtual disk, because a qcow2 cannot shrink (hence m1).
- **SIZE-012 (warm pool).** `warm-pool 0` by default and I did not turn it on. Accepted: create took about 4 s.

## Checked and fine

- **Sizes.**
  - `small`/`default`/`large` give 2/4/8 vCPU and 4/8/16 GB (`nproc`, `free -g`) with `billingMultiplier` 0.5/1/2.
  - `stop` then `resume --type large` and `--type default` changed the size on the same id (about 3 s). `/dev/kvm` is in the guest.
  - `type_too_small` refuses 74.3 GB onto 51 GB and leaves the sandbox stopped and unchanged (the `resume` that follows works at the old size).
  - A `failFast` call on a full pool, with `--min-tier platinum`, `--category cpu=extreme`, `--node nosuch` or `--gpu` (the node has no passthrough) → instant `503 no_ready_machine`. `--nested` and `--node n1` placed. `--node-label` placed after `nodes update --label room=2`.
  - A drained node: new → `no_ready_machine`, running sandboxes kept running, `undrain` restored it.
  - A fork of a running default sandbox on a full node waits (`waitingForCapacity:true` and the message through the API).
- **Auto-stop.**
  - `ttlSeconds:null` → `archiveAfter:null` and kept over `PATCH`.
  - `99999999` is capped to 2592000 (30 days).
  - `0` and negative → `400 invalid_ttl`.
  - `extend --ttl 20` stopped the sandbox at the deadline (the state moved within about 3 s) and the data was still there after a resume.
  - `resume --ttl 600` set the deadline. Extend sets, not adds.
  - A fork of a no-auto-stop sandbox got 3600 s.
  - **A running agent prompt moves the deadline:** I set `ttl 30`, started an OpenCode prompt that ran `sleep 70`, and `archiveAfter` moved by 30 s at each deadline (17:09:13 → 17:09:45 → 17:10:15 → 17:10:47). The sandbox did not stop.
- **Limits and credits.** See "Disputed or accepted". Also: `crowdvm usage` and `GET /sandboxes/{id}/usage` return seconds with the multiplier, `credits` and `dollars`, and `?include=cost,deletedCost` works. The `earn` and `usage` ledger rows pair up per node.
- **Node app.**
  - The join benchmark (cpu, memory, disk, network, internet, gpu, virtualization) gives a score and tier.
  - Defaults match the guide: half the CPUs and RAM and 90 % of usable disk (`--skip-bench` join of `n2`: 8 vCPU, 31 GiB, 132 GiB).
  - `config get/set` validates keys and numbers. `config set` changes apply after a restart. A node restart kept the running VMs.
  - The UI shows status, pause and re-run-benchmark controls and image push/pull/import; mutating calls need the `X-Crowdvm-Ui` header.
  - `crowdvm-node status` and `crowdvm nodes` show usage, capacity and disk.
- **The skill and `agent-instructions`, as an agent.**
  - `status --check` exits 1 when not signed in and 0 when signed in.
  - `login --key-stdin` and `CROWDVM_API_BASE`+`CROWDVM_API_KEY` both sign in.
  - `new … | jq 'select(.event=="ready")|.id'`, `push` (git-aware: tracked and untracked-not-ignored files, no `node_modules`), `exec --cwd --timeout 0` (exit code 0/7, 124 on a 30 s timeout), `--detach`/`--status`/`--kill`, `exec --push`, `run --push --type small` (4 s, "bx_… deleted"), `scp ID:path`, `history`, `ps`, `ssh "cmd"`, `host … --json` and `--public`, `forward --remote 3000 --local 9975` (200), `extend --hours 4`.
  - `fork --wait`, `snapshot`, `new --from NAME --count 2`, `snapshot latest` + `snapshot pull`.
  - `stop`/`resume` kept `~/app` and lost the `.crowdvmignore` folder and `/tmp`. `~/.crowdvm/pre-stop` ran on `stop` and not on `stop --force`.
  - Inside the sandbox, `host`, `boat` and `box` exist, and `crowdvm info`, `host 3000`, `extend self`, `snapshot self` and `host list` work.

## Not verified

- The dashboard in a browser. The fixes are checked at the API with the dashboard's own header. I read `web/js/pages/snapshots.js:50-100` (no per-row wallet is passed).
- `crowdvm-node install-service` (it would install a unit on this machine), Windows and macOS nodes, a real GPU passthrough, a second node and cross-node resume.
- The 3-minute node-offline rebuild, the 3-day forced stop, the 14-day retention, and the 24-hour credit grace ending.
- `member_limit_reached`, `team_member_cap_reached`, extra starts, and the IP rate limit, live.
- A fork's hostname after the recipe (only resume was run).
- e2e (not run, by instruction).

## What would flip this to AGREE

1. **B1.** A paused node is not placed on. `failFast` against it answers a real `503 no_ready_machine` with no sandbox row and no start recorded. `crowdvm nodes` shows `paused`.
2. **B2.** A size that no node in the pool can host gets a non-retryable, correctly worded error at once. `crowdvm new --type large` on a pool of small nodes exits in a second with a message naming the sizes the pool offers, not a silent 15-minute retry.

m1–m9 are cheap and would close the gap between the docs and the code.

## TL;DR

DISAGREE, on two small blockers:
- **B1.** A paused node still receives placements, so `failFast` returns `202` and then a `cancelled` sandbox that counts as a start (`internal/node/vms.go:152`, `internal/server/lifecycle_place.go:142`, `internal/server/sandboxes.go:984`).
- **B2.** A size no node can host hangs `crowdvm new` silently, with a retryable `out_of_capacity` "n1: full" (`internal/server/sandboxes.go:974`, `internal/client/client.go:317`).

All four claimed fixes (hostname recipe, profile-gated Compose images, `snapshot pull` `docker/`, the dashboard saved-snapshot list) are verified live. Auto-stop (including the prompt that moves the deadline), size change and `type_too_small`, limits, the credits grace and `402`, the node join and config, and the skill's recipes all work as documented.
