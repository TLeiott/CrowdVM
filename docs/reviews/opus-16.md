# Review opus-16: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. No sub-agents.

## Verdict: DISAGREE (one blocker)

All three claimed fixes hold. I checked each one live, and the saved-snapshot fix also on the tree as it stood at 20:15. GC deletes merged layers once nothing needs them. Saved-snapshot deletion, deletion operations and fork/resume chains show no regression.

One blocker, found while checking that disk use stays bounded:

- **B1. After a cross-node resume, deleted sandbox data stays on the new node, and the deletion operation still says `completed`.** When a node fetches a sandbox's chain lazily and the fetch ends before the guest is ready (the usual case on a LAN), the server never records that node as holding those layers. GC then never deletes them there. Live: a sandbox made on node n2big, resumed on n1, then deleted. Its `bdop_…` went to `completed` and the server purged all 4 snapshot rows, but n1 still holds both layers with the sandbox's file in plaintext. Boat: `completed` means "boat erased every snapshot of it" (`data-retention__delete.md:70`). PARITY DATA-003 claims node caches are included. On a crowdsourced pool the node owner is a third party, so this is a privacy problem, not just disk space.

## Method

1. **Read.** `opus-15.md`, `sonnet-13.md`, the fix code (`store.go`, `lazy.go`, `transfer.go`, `snapshots.go` GC, `lifecycle_place.go`, `orgs.go`), `testDeployAfterMerge`, and e2e run16/17/18.
2. **Build and tests.**
   - `make build` and `go vet ./...` are clean.
   - `go test -count=1` on every package except `test/e2e` is all `ok` (19 packages).
   - I did not run e2e.
   - The tree changed while I worked (`internal/server/lifecycle.go` and `lifecycle_fix_test.go` at 20:03, `test/e2e/features_test.go` at 19:47). So I rebuilt from the current tree at ~20:13 and re-ran the core snapshot sequence and the B1 repro on it.
3. **Own cluster.**
   - `crowdvm-server` on 127.0.0.1:9960, `--snapshot-interval 20` to force frequent layers.
   - Node `n1`: qemu/KVM, UI 9961, 6 vCPU, 12 GiB, 3 sandboxes, image `ubuntu-24.04-crowdvm-v3` imported and pushed.
   - Node `n2big`: UI 9962, 16 vCPU, 32 GiB, pulled the image from the server on first use.
   - Data in `~/.local/share/review-opus-16`, with `HOME` and `CROWDVM_CONFIG_DIR` isolated. Users `admin` and `bob`.
4. **Sweep.** I ranked Boat pages by how often earlier reviews cite them, then took the least-cited ones that opus-15 had not swept. I also ran the **real** third-party AgentEnv and Eve packages from PyPI/npm against my server, not CrowdVM's stand-ins.
5. **Teardown.**
   - I deleted every sandbox and saved snapshot, then stopped both nodes and the server by saved PID.
   - Afterwards no qemu used my data dir and nothing listened on 9960–9969. I deleted the data dir.
   - I touched no other process or cluster. The only repo file I edited is this report.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| sonnet-13 B1 | A saved-snapshot deploy survives a merge of its layer | **Fixed (live, twice: 19:21 and 20:13 builds)** | The node no longer deletes superseded layers (`internal/node/transfer.go:193-195`). `useLocal` names a kept file, even one marked deleted, as the overlay's backing file and un-deletes it (`internal/node/vm/qemu/store.go:153-167`). `createLazy` uses it (`lazy.go:88`), and `ImportSnapshot` keeps the record (`store.go:217-218`). **Sonnet's sequence, live:** save `base1` from A; deploy B; stop/resume B until `bx_k3tap4xh: merged 3 layers`. The saved layer was absorbed: the new level-1 layer has `parent=None`. Then deploy D: its overlay's backing file is the real kept file `disks/bx_z9ynzamd-8f9857ce.qcow2`. Stop/resume ×2 kept `base`+`second`. Save `next1` from D, deploy E, stop/resume: data intact. **Variants:** resume the saver A (whose layers build on the merged saved layer); re-save `base1` from A while D runs on the old version, then stop/resume D (old data) and deploy F (new data); node restart after a merge, then deploy and stop/resume; deploy the n1-made `tpl` on n2big, merge there, resume on n1. All pass. 12 rapid `stop --wait` → `resume --node n1` cycles and 6 fresh-deploy → stop → resume cycles: 0 failures. |
| sonnet-13 B1 (GC) | Merged layers are deleted by the server's GC | **Works for merged layers. Imported layers leak: see B1** | Periodic layers that a merge supersedes are soft-deleted (`internal/server/snapshots.go:110`, `:179`). GC walks the needed set and sends `DELETE /v1/snapshots/{id}` to every recorded location (`snapshots.go:245-335`). Live, I backdated my snapshot rows 48 h and ran retention (`PUT /admin/retention {"runNow":true}`): node records went 48 → 25, and the server purged 79 of 104. Afterwards every sandbox resumed with its data and both saved snapshots deployed. Then I removed both saved snapshots and deleted every sandbox: the server had 197/197 purged and 0 locations, and the node was back to the image only (4.2 GiB, the baseline). |
| opus-15 B1 | A paused node can still host later: requests wait | **Fixed (live)** | `couldEverPlace` counts a paused node (`internal/server/lifecycle_place.go:65-67`). `TestPausedNodeAndImpossibleSize` now expects a queued 202 (`lifecycle_fix_test.go:969`). Live, with n1 (6 vCPU) active and n2big (16 vCPU) paused: `POST /sandboxes {"type":"large"}` → 202, `waitingForCapacity:true`, "Waiting for a node with room for a large sandbox". `POST /sandboxes/{id}/resume {"type":"large"}` → 202, queued. `failFast` → `503 no_ready_machine`. `crowdvm new --type large` waited (no 409). After `config set paused false` on n2big, the queued `large` reached `ready` on n2big within ~20 s, and `nproc` = 8. Cosmetic leftover in m3. |
| opus-15 B2 | `org delete` refuses while other members remain | **Fixed (live)** | `handleDeleteOrg` counts members (`internal/server/orgs.go:328-333`). Live, with acme = admin + bob: `org delete --yes` → `409 org_has_members`, and both stay members. After `DELETE /orgs/{org}/members/{bob}`: `{"deleted":true}`. The leftover org environments are m4. |
| e2e | run16 / run17 / run18 | **run17 red, run18 green** | run17 (19:20) failed `orgs` and `saved-snapshots`. `orgs` was a stale test that still expected the delete to succeed (`features_test.go:545` in that build), since updated (`:544-546`). `saved-snapshots`: on its first stop/resume, `testDeployAfterMerge` got `resume … → sandbox_error machine_stopped` 1 ms after the resume started (`lifecycle_test.go:365`; run17 server log: "sandbox failed" at 24.819, resume 202 at 24.821, and the node never got a boot request). The 20:03 change to the `VMStopped` branch ignores a stale `stopped` event while the sandbox is starting (`internal/server/lifecycle.go:611-616`), which matches this race. I could not reproduce it in 18 tries on either build. run18 (19:50, before that change) passed with `EXIT=0`, `testDeployAfterMerge` included. No unit test covers the 20:03 guard. |

## Blocking finding

### B1. A lazily fetched chain is never tracked on its new node: deleted data stays there (`data-retention__delete.md:58-70`, PARITY DATA-003)

- **What Boat promises.** A delete returns `bdop_…`. "boat erases the stored data in the background, when nothing reads it any more." `completed` means "boat erased every snapshot of it" (`data-retention__delete.md:58`, `:70`). CrowdVM's PARITY DATA-003 (`docs/PARITY.md:604`) says an operation completes only when "every snapshot copy (server blob and node caches) is erased".
- **What happens (current tree, live).**
  1. `new --node n2big` (W). `exec "echo SECRET-W-DATA > ~/secret.txt"`. `stop --wait`.
  2. `resume W --node n1`. n1 log: `lazy boot, 2 of 2 chain layers remote`, then `chain hydrated (2 layers)` 120 ms later, before the guest was ready.
  3. `snapshot_locations` for W's two layers: `node_buxekgg2ar5d` (n2big) only, never n1.
  4. `stop --wait`, `delete --yes`. The sandbox's `bdop_…` → `completed`. The server shows 4/4 of W's snapshots purged.
  5. n1 still has `disks/snap-snap_2rf24ujk2yc3yqum5dzf.qcow2` and `disks/snap-snap_sppfgs2q75phxndknjer.qcow2`, with live (not deleted) records in `snapshots/`. `strings` finds `SECRET-W-DATA` in both. n1's record count went 5 → 7.
  - Earlier, on the 19:21 build, the same leak hit sandbox T (made on n2big, resumed on n1): four layers totalling 89 MB stayed on n1 after T, its 5 deletion ops and all its server rows were gone.
  - A node restart keeps them: `gcOrphans` keeps files that a live record names (`store.go:424-478`).
- **Cause (from the code).**
  - On a lazy-chain node, `ensureChain` skips `addLocation`, because the node "becomes a location when hydrated" (`internal/server/lifecycle.go:480-481`).
  - Only `markHydrated` records those locations, and it returns early unless `hydrated` flips from 0 to 1 (`internal/server/lifecycle_secure.go:298-305`).
  - But `markReady` already sets `hydrated=1` whenever the node does not report `Hydrating` at ready time (`lifecycle_secure.go:256-260`). That is the normal case when a small chain arrives over a LAN faster than the guest boots.
  - So the chain on the new node is never a location, GC never sends that node a `DELETE` (`snapshots.go:302-327`), and the deletion op completes on the server's word alone.
  - This predates the snapshot fix: the node never deleted non-superseded imported layers on its own. It affects every multi-node pool. Each cross-node resume, deploy or fork leaks its chain onto the target node for good.
- **Why it is supportable on a LAN.** Nothing external is involved. Either fix works:
  1. In `markReady`, when it sets `hydrated` itself, record `snapshotChain(sb.HeadSnapshotID)` as locations of `sb.NodeID`, as `markHydrated` does.
  2. Have `ensureChain` add the location once the node has imported each layer, lazy or not.

  A periodic inventory reconcile would also catch leftovers from before the fix. The node can report its snapshot ids, and the server can delete those that are purged.
- **Test.** A server test with a lazy node that reports hydrated before ready: resume a sandbox there, delete it, and assert that the node received `DELETE /v1/snapshots/{id}` for each chain layer. An e2e check in `cluster`: after a cross-node resume and delete, the target node holds no layer of the deleted sandbox.

## Minor (not blocking)

- **m1. Superseded `stop` and `live` layers stay until retention (24 h).** Only `periodic` layers are soft-deleted on merge (`snapshots.go:110`, `:179`). Now that the node keeps superseded layers (`transfer.go:193`), a merged level-1/2 layer and the stop/live layers it absorbed both sit on the node for up to `olderHours` (default 24). Disk use stays bounded: retention purged them in my backdated run. But a busy stop/resume day costs roughly double the disk on the node. Marking superseded non-head `stop`/`live` layers deleted on merge would bring the node back to the pre-fix footprint. The GC already keeps any that a saved snapshot or fork needs.
- **m2. No regression test for the 20:03 stale-`stopped` guard** (`lifecycle.go:611-616`). run17 shows the race is real; a servertest that delivers a late `stopped` event while the sandbox is `provisioning` would pin it.
- **m3. `crowdvm limits` says "no node in the pool is big enough for large" while the big node is only paused** (`internal/server/scheduler.go:291-306`). The "paused" reason is set only while no unpaused node has been seen, and the small unpaused node overwrites it. opus-15 asked for "paused" here.
- **m4. `org delete` leaves the org's environments behind** (`orgs.go:338-350`). Live: `team_6wnj…|base` and `|shared` stay in `environments`, with no org that can reach them. They hold encrypted secrets. Boat's page says nothing about them, so this is data hygiene, not parity.
- **m5. Still open from opus-15.**
  - `docs/guide/node-owners.md:130-131` still says "and restart" after `config set paused`.
  - An absolute secret-file path is still accepted (`env set-file vtest /etc/passwd2` → success).
  - `origin` is always `"captured"` (`snapshots.go:393`).
  - The opus-15 m7 item is fixed: `skills/crowdvm/SKILL.md:211` lists `size_unavailable`.
- **m6. The cap message addresses the wrong person.** When an owner resumes a member's shared sandbox and hits that member's cap, the owner reads "The organization owner capped **you** at 1 sandboxes at once" (`internal/server/ratelimit.go:205`). The behaviour is right: Boat counts the creator's cap.
- **m7. Re-saving a saved snapshot under the same name keeps the old `createdAt`.** I re-saved `base1` at ~18:29 and it still says `18:22:39`. Cosmetic.

## Sweep results

| Page / area | Result | What I ran |
|---|---|---|
| `integrations__agentenv` (1 earlier citation) | **Pass with the real package** | `pip install 'agentenv-framework[boat]'` (0.9.1500, uses `boat-sdk` 1.13) in a scratch venv. `.agentenv/config.toml` with `api_url = "http://127.0.0.1:9960/api/v1"` and a `crowdvm api-key create agentenv --preset ci` key. Boat's `agent-env run hello --sandbox boat` → deploy_sandbox, load_artifact, verify_sandbox all passed in 3.7 s, "Tore down 1 sandbox". |
| `integrations__eve` (1) | **Pass with the real package** | `npm install @asciidev/eve-box` (0.1.6). Boat's `hello-sandbox` example with `baseUrl: …/api/box/v1`: exit 0, file round trip, `uname`, `resolvePath` → `/workspace/notes/hello.txt`. Also `writeBinaryFile`/`readBinaryFile` (bytes 0,1,2,255), `spawn` with streamed stdout and exit 3. `useSessionFn({networkPolicy:"deny-all"})` is refused by the package. `removePath` without `recursive` leaves a directory, which is the package's plain `rm`. |
| `api-keys__store-rotate-revoke` (4) | Pass | A `ci` key creates a sandbox, and its own revoke or rotate → `403 api_key_action_forbidden`. An admin key can rotate, which Boat allows (`api-keys__create-and-list.md:98`). CLI-session rotate: old secret → 401, new → 200. `api-key usage --verbose`. `revoke --yes` → unknown flag, as Boat says. After revoke → 401. |
| `organizations__seats-and-limits` (3) | Pass (seats and plans N/A) | Per-member `concurrentCap:1` → bob's second `new` → `429 member_limit_reached`. The owner resuming bob's shared no-env sandbox while bob is at his cap → 429, counted on the creator as Boat says. Lowering bob's cap to 0 stopped his running org sandbox in ~30 s. The owner's own org sandboxes are not capped. |
| `data-retention__zero-data-retention` (3) | Pass | `GET /account/data-retention` with an API key. `PATCH` with a key → `403 session_required`. `crowdvm data-retention enable --yes` via the CLI session. The saved snapshot was removed, a new save → `409 zero_data_retention`, and stopped sandboxes were deleted. A stop under ZDR deletes. `/me` shows `zeroDataRetention`. Other members are unaffected. |
| `environments__versions` (low) | Pass | `set-var` and `set-file` make new versions. A running sandbox keeps v3. `env upgrade --sandbox X` moved X only (`upgradedLive:1`, FOO changed, the removed secret file was deleted from disk). Z was untouched. `env upgrade` then upgraded the stopped Y (`upgradedStopped:1`), and Y got v5 on resume. |
| `data-retention__delete` (regression) | Pass on one node, **fails across nodes (B1)** | `snapshot delete --all <id> --yes` on the saver of a saved snapshot: 44/44 ops `completed`, resume → `409 snapshot_data_expired`, and deploys of the saved snapshot stop and resume with their data. Removing a saved snapshot and then the sandboxes purged everything on one node. |
| Fork/resume chains (regression) | Pass | Live fork of H → K. Delete H. Stop K. Fork the stopped K → L. Delete K. L has H's and K's files across stop/resume. |
| `organizations__shared-sandboxes` | Pass | `--org X ls` shows your own sandboxes plus the org's, as Boat says (`organizations__shared-sandboxes.md:7`). A sandbox still holding its creator's logins stays private until its next start (`cli-reference.md:1400`). |

## Disputed or accepted classifications

- **ZDR deletes the stopped sandbox, not only its disk.** I accept it. Boat says a ZDR stop "queues the history for deletion with the rest of the sandbox" (`long-running-tasks__command-history.md:109`), and resume and fork are impossible anyway. PARITY DATA-008 documents the same reading.
- **Member removal only through the account API and dashboard.** I accept it. Boat has no CLI command for it either (`organizations.md:37`, `:107`). The `org_has_members` message points to the dashboard.
- **Superseded stop/live layers kept for 24 h (m1).** I don't count it as missing a feature. The growth is bounded, and I checked live that retention purges them.

## Not verified

- e2e on the current tree (after the 20:03 change). run19 was running when I finished.
- The run17 race itself: I could not reproduce it in 18 tries, so the 20:03 guard has not been tested live.
- CI against real GitHub or Gitea. Live model turns. Windows nodes. The dashboard in a browser.
- `harbor-box` with the real package. I did not try it.

## What would flip this to AGREE

1. **B1.** After a cross-node resume (also a deploy or fork onto a node that lacked the chain), the target node is recorded as a location of every layer it imported, even when the chain hydrated before ready. Deleting the sandbox then erases those layers on that node before its deletion op reports `completed`. Add a server test for hydrated-before-ready and an e2e check that the target node holds no layer of a deleted sandbox. Optionally, add a one-time inventory reconcile to remove layers already leaked on existing nodes.
2. A green e2e on the tree that includes the 20:03 `VMStopped` guard, ideally with a unit test for the late-`stopped` race (m2).
