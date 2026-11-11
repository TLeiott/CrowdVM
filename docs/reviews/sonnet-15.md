# Review sonnet-15: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. I used no sub-agents.

## Verdict: AGREE

I found no blocker. The opus-16 B1 leak is fixed, both sonnet-14 minors are fixed, and the stop-then-resume race did not reproduce in 15 rapid cycles. I judged the product as it stands now (source last changed 20:22:54; I built after that and nothing changed while I tested).

My three end-to-end angles show nothing missing that a LAN-only, self-hosted, no-payments pool could support:

- **(a) Quickstart.** Every step works as a new user would follow it.
- **(b) API.** 35 operations of `boat-v1.yaml` all behave as the spec describes, once I sent the shapes the spec asks for.
- **(c) Node owner.** Join, benchmark, pause/resume, the UI, and removing a node all work, with the small rough edges listed under Minor.

## Method

1. **Read.** `opus-16.md` and the first part of `sonnet-14.md`. I did not repeat their detailed sweeps. I read the fix code: `markReady` (`internal/server/lifecycle_secure.go:253-275`), `sizeAvailability` (`scheduler.go:279-312`), the `VMStopped` branch (`lifecycle.go:611-616`), and the new `TestDeleteErasesPulledChain` (`snapshots_test.go:187`).
2. **Build and tests.**
   - I built the four binaries with the `make build` commands into my scratchpad, so I did not rewrite `bin/` under the other reviewer.
   - `go vet ./...` is clean. `go test -count=1` on every package except `test/e2e` is all `ok`, including `internal/server` (42 s).
   - I did not run e2e. The e2e run18 log ended `EXIT=0`, as the brief says. The B1 fix and its test came after that run (20:22), so e2e has not covered them.
3. **Own cluster.**
   - Server on 127.0.0.1:9970 (limits off, `--snapshot-interval 20`).
   - Node `n1` (UI 9971): qemu/KVM; I imported and pushed image `ubuntu-24.04-crowdvm-v3`.
   - Node `n2` (UI 9972): pulled the image from the server. Later removed (c).
   - Node `n3` (UI 9972, after n2 was gone): added for the offline-delete check.
   - Users `admin` and `bob`, `HOME` and `CROWDVM_CONFIG_DIR` isolated, data in `~/.local/share/review-sonnet-15`.
4. **Teardown.** I stopped n1, n3 and the server by saved PID, then checked that no process used my data dir and that nothing listened on 9970–9979. I deleted the data dir. I touched no process that was not mine; the other reviewer's server stayed up.

## Fix status

| # | Claimed fix | Status | Evidence (live unless noted) |
|---|---|---|---|
| opus-16 B1 | A lazily fetched chain is recorded as locations of its new node, so deletes reach it | **Fixed** | Exact repro: `new --node n2`, write `SECRET-W-DATA`, `stop --wait`, `resume --node n1`. n1 log: `lazy boot, 2 of 2 chain layers remote`, then `chain hydrated (2 layers)` before ready. This time `snapshot_locations` lists both layers on n1 and on n2 (opus-16 saw n2 only). After `stop`, `delete`: the op went `completed`, and both nodes' `vm/disks` held only the base image and `vm/snapshots` was empty. The fix is `markReady` adding the chain as locations when it sets `hydrated` itself (`lifecycle_secure.go:268-274`). The unit test `TestDeleteErasesPulledChain` covers lazy and non-lazy nodes and passes. |
| opus-16 B1 variants | Same leak for deploy and fork | **Fixed** | Saved snapshot `tpl` deployed on the other node (T), and a live fork of A onto the other node (F). Both held the right data (`gen1`, sha256 OK). After deleting T, F, A and `snapshot rm tpl`, n1 and n2 were back to the base image only. The server showed 48/48 snapshot rows purged, 0 locations, and every deletion op `completed`. |
| B1 offline | Deletion waits for a node that holds a copy | **Works** | I made V on n3, stopped it, killed n3 and deleted V. The op sat at `processing` / `retrying`, and `crowdvm delete` kept waiting. When I restarted n3 it went to `completed` and n3's `vm/snapshots` was emptied. |
| sonnet-14 n1 | A queued `new --from NAME` that is stopped and resumed boots the saved snapshot | **Fixed** | I paused both nodes, ran `new --from sx --no-wait`, then `stop`, then `resume` (still queued), then unpaused. It reached `ready` and `cat ~/d.txt` gave `SAVED-DATA`, not an empty disk. |
| sonnet-14 n2 / opus-16 m3 | `limits` says "paused", not "too small", when only a paused node could host a size | **Fixed** | n1 at 16 vCPU / 40 GiB and paused, n2 small and active: `machineTypes` for `large` says "only a node its owner paused can host large (a request waits until it is resumed)". `xlarge` still says "no node in the pool is big enough", which is true. `new --type large --no-wait` queued; after `paused false` it reached `ready` on n1 (`nproc` = 8) with no restart. |
| Stop-then-resume race | A late `stopped` event no longer fails a boot that has started | **Holds (not reproduced)** | 15 cycles on one sandbox, alternating `stop --wait` and plain `stop`, each followed at once by `resume` (every third with `--node n2`). 0 failures; data and sha256 intact. The guard is `lifecycle.go:611-616`. No unit test pins it (still opus-16 m2). The race did not reproduce for opus-16 or sonnet-14 either, so this is evidence of no regression, not proof the race is closed. |

## Angle (a): the quickstart as a new user

Account `bob` (non-admin), fresh config dir. `crowdvm` replaces `boat`.

- `curl $SERVER/install` serves the installer script. A new `crowdvm new` printed a ready sandbox: `default` = 4 vCPU, 8 GB (matches Boat's table), 1-hour auto-stop.
- `exec current "echo hello"` gave `hello`. `ssh current -- "hostname; nproc"` worked (`nproc` = 4).
- `stop current` then `resume current` ran as written.
- `prompt current "add tests"` with no AI key gave `409 provider_not_configured`, the same code as Boat's quickstart. (Prompting with a real key is covered by e2e `agent` / `ai-proxy`, which I did not rerun.)
- `api-key create my-app --preset ci` printed a `cvm_…` key. With a full-access key, `POST /sandboxes` then `POST /sandboxes/{id}/commands` ran the curl flow.

## Angle (b): API sample

35 calls with curl and a `cvm_` key (`/api/v1` paths, `Authorization: Bearer`), across `me`, `limits`, `repos`, `ci`, `webhooks`, `telemetry`, `credential-profiles`, `ai-proxy`, `ai-access`, `secrets`, `environments`, `sandboxes`, `files`, `commands`, `command-history`, `processes`, `host`, `sshkey`, `desktop`, `fork`, `named-snapshots`, `saved-snapshots`, `shares`, `snapshots/latest`, `snapshots`, `usage`, `data-retention`, and deletion operations.

| Operation | Result |
|---|---|
| `GET /me`, `/limits`, `/repos`, `/ci`, `/ai-proxy`, `/account/data-retention`, `/telemetry`, `/snapshots`, `/named-snapshots` | 200 |
| `POST /webhooks`, `…/rotate`, `GET`, `DELETE` | 201 / 200 / 200 / 200 (new secret returned; old one valid 10 min) |
| `PUT /telemetry` | 200 with an endpoint on a LAN address |
| `POST /credential-profiles`, `POST /secrets`, `POST /environments`, `PUT /environments/{id}/vars/K1` | 201 / 200 / 200 / 200 |
| `PATCH /sandboxes/{id}`, `POST …/fork`, `PUT …/ai-access` | 200 / 202 / 200 |
| `PUT …/files`, `GET …/files`, `POST …/files/replace`, `GET …/files/find` | 200 (written, read back `gamma beta`, replaced 1, found) |
| `POST …/commands`, `GET …/command-history`, `GET …/processes` | 200 |
| `POST …/host`, `POST …/sshkey`, `POST …/desktop`, `GET …/usage` | 200 |
| `POST /named-snapshots`, `POST /saved-snapshots`, `POST /saved-snapshots/{name}/shares`, `GET /saved-snapshots/{name}`, `GET …/snapshots/latest` | 202 / 202 / 201 / 200 / 200 |
| `DELETE /sandboxes/{id}` with `X-Ascii-Confirm-Delete`, then `GET /deletion-operations/{id}` | 202 (`pending`), then `completed` |

Some first tries returned 400 because I guessed request bodies. I took the shapes from `boat-v1.yaml` (for example `files/find` needs `pattern`, `files/replace` needs `paths`, `pattern`, `newValue`, and `sshkey` needs `key`) and the calls then passed. The 400s were my errors, not defects. Two refusals are intended constraints: a webhook URL on loopback (`invalid_url`: the page says LAN and Tailscale only) and a delete without the confirmation header (`confirmation_required`).

## Angle (c): node owner

- **Join and benchmark.** `crowdvm-node join` benchmarked each machine (CPU, memory, disk, network, internet, GPU, virtualization) and printed `score 744, tier gold`. `crowdvm nodes` lists capacity, categories, free, reserved and the benchmark. The `--cpus`, `--memory`, `--disk` and `--max-sandboxes` caps show up as capacity.
- **Pause and resume.** `config set paused true|false --data-dir …` took effect live on a running node (queued boots waited, then started within seconds). `docs/guide/node-owners.md:130-131` still says "and restart"; it is wrong in the safe direction (m3).
- **Node UI and status.** `crowdvm-node status` showed connection, benchmark, usage and the VM list (a running `large` 8 vCPU / 16384 MiB). `GET http://127.0.0.1:9971/` served the page and `/api/status` returned JSON including `paused`.
- **Removing a node.**
  - `crowdvm nodes remove <id> --yes` refused with `409 node_busy` while two sandboxes ran. `--force` returned `node.deleted`.
  - The node daemon then logs `401` and retries with back-off. That is harmless.
  - The sandbox that was running (Y) went to `error`/`node_offline`, then to `stopped`; the stopped one (X) stayed `archived`. Both resumed on the remaining node with their data (`X-DATA`, `Y-DATA`) from the server's copies, with `resume --node n1`.

## New missing or broken supportable features

None blocking. Nothing a Boat page describes is missing, and nothing I tried broke.

## Minor (not blocking, all cosmetic or hygiene)

- **m1. A pin to a removed node outlives the node.** Placement `{"node":"n2"}` stays on the sandbox row after `nodes remove`, so a plain `resume` gives `503 no_ready_machine` and the sandbox stays stopped. `resume --node n1` fixes it, but nothing tells the user to. Dropping or ignoring a pin to a deleted node would be kinder (`scheduler.go:96-98`). Boat has no node concept, so this is CrowdVM-only.
- **m2. `crowdvm nodes info|drain|undrain|remove` take only the node id.** A name gives `404 not_found`, while `--node` for placement accepts a name. `docs/guide/node-owners.md:129-133` writes `<node>`, which reads as "id or name".
- **m3. `node-owners.md:130-131` still says "and restart"** after `config set paused`. I saw it apply live.
- **m4. No unit test for the stale-`stopped` guard** (`lifecycle.go:611-616`), as opus-16 noted. B1 does now have one.
- **m5. A removed node keeps the sandbox data on its owner's disk.** After `nodes remove --force`, nothing erases the node's layers because the server forgot the node. The owner can delete them, and the node owner is the one who removed it, but on a shared pool a "removed" node still holds other people's plaintext layers until its owner cleans up. Not a Boat feature; worth a line in `node-owners.md`.
- **m6. Still open from opus-16:** m1 (superseded `stop` and `live` layers stay up to 24 h), m4 (`org delete` leaves the org's environments), m6 (the cap message says "you"), m7 (re-saving a snapshot keeps the old `createdAt`). I did not recheck these.

## Disputed classifications

None. I accept the earlier classifications of unsupportable features (public internet exposure, real payments, third-party OAuth sign-in). I did not re-derive the PARITY checklist.

## Not verified

- The e2e suite against the 20:22 build. The brief forbids running it, and run18 predates the B1 fix.
- A Windows node, `install-service` and the Windows service. This machine is Linux only.
- Integrated agents with a real AI key (e2e covers this with OpenCode's free model).
- Opus-16 m1, m4, m6 and m7, and whether the 20:22 edits changed anything beyond `markReady`. I read `markReady` and its test, not the whole diff.

## What would change the verdict

It would flip to DISAGREE if any of these reproduces:

- A deleted sandbox's layers stay on a node after its deletion op says `completed`.
- A stop-then-resume, resume, or queued deploy boots an empty disk or fails with `machine_stopped`.
- `crowdvm limits` or placement misreports a paused node.

I could not reproduce any of them.
