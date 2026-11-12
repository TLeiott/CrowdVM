# Bug hunt r19 (opus): round-18 rechecks, hosted ports, snapshot files, retention and billing, 2026-11-12

Setup: commit `8c27d15`, built with `make build VERSION=0.9.0-r19.1` in my own worktree (`/home/tl/dev/CrowdVM-r19opus`, detached at `8c27d15`). Binaries and all state lived in `~/.local/share/crowdvm-r19opus`. Ports 9960–9969 were free, but I used 9970–9979 so the parallel sparse-upload session could take the lower range. One `crowdvm-server` listened on :9970 with `--public-url http://127.0.0.1:9973`, `--public-domain r19.test`, `--log-level debug`, no start limits, `--enforce-credits`, `--default-image ubuntu-24.04-crowdvm-v4`, `--egress-allow 192.168.178.31:9977,192.168.178.31:9978` and `--node-update-grace 6m`. For about 3 minutes it ran with `--no-guest-channel` so I could make one unencrypted (legacy) sandbox. Two qemu nodes, `r19n1` and `r19n2` (UI on :9971 and :9972), each with 8 vCPU, 20 GiB, 60 GiB disk ×4 overcommit and at most 5 sandboxes, each with its own binary directory. Both ran as transient systemd user units with the real unit's properties (`KillMode=process`, `NotifyAccess=main`, `FileDescriptorStoreMax=1024`, `FileDescriptorStorePreserve=yes`, `CROWDVM_FDSTORE=1`, `Restart=always`, `RestartSec=5`). `r19n1` ran as `crowdvm-node.service` (no such unit existed for this user), so it counts as supervised. `r19n2` ran as `r19opus-n2.service`, with `IOWriteBandwidthMax=80M` set at run time for the slow re-encryption test. Images `ubuntu-24.04-crowdvm-v4`, `-minimal-v3` and `-minimal-v2` came from `~/.local/share/crowdvm-dev/images/`: each was imported on `r19n1` and pushed, and v4 and minimal-v3 were also pulled to `r19n2`. All three get an encrypted data partition in this build, so the only plain disk was the one booted without a guest channel. Node↔server and guest↔server traffic went through a Python token-bucket proxy on :9973 at 40 MB/s, lowered to 3 MB/s while a re-keyed copy's 42 GB first snapshot was uploading (the known sparse-upload issue). Users: `admin` (owns the nodes), `bob`, `carol` and `dave`, each with their own `CROWDVM_CONFIG_DIR` and scratch `HOME`.

For the update test I built one local release from a copy of the source outside the repo (`~/.local/share/crowdvm-r19opus/src4`): `0.9.0-r19.4`, whose `run` fails right after `g.config()` and so stands in for a build that cannot read its config. I published it with `crowdvm-server downloads import` from a hand-made `crowdvm_<v>_linux_amd64/` directory and `manifest.json`. Nothing came from GitHub.

To check the time-based paths without waiting days, I moved timestamps back in my own server database: `account_policy.purge_at` (account purge), `wallet_state.grace_since` (24 h credit grace) and `sandboxes.stopped_at` (14-day retention). Product code was not changed.

Cleanup: see the last section.

## Bugs

### 1. Anyone can take another sandbox's default URL label as their subdomain, and gets that sandbox's URLs once its owner sets a subdomain (security)

Severity: security. A sandbox's hosted URLs use its subdomain, or `bx-<id>` (its id with `_` replaced by `-`) when it has none. `PATCH /sandboxes/{id}` with `subdomain` only checks the format and that no *other subdomain* is equal. It does not refuse a label of the form `bx-<id>`, which is another sandbox's default label. Routing (`sandboxByLabel`) sends `bx-<id>` to the sandbox with that id only while that sandbox has no subdomain, and otherwise falls through to the subdomain lookup. So:
- Another user (any account, no relationship needed) sets their own sandbox's subdomain to `bx-x75sb3gz`. Their `crowdvm host` then prints URLs that actually serve the *victim's* app (wrong for them).
- As soon as the victim sets a subdomain of their own (`crowdvm subdomain s1 bobapp`), every `bx-x75sb3gz-<port>` URL the victim has handed out (public links, bookmarks, webhooks, OAuth redirect URLs, `-desktop`) is served by the attacker's sandbox. Visitors and callers of the victim's old public links get the attacker's app on a URL they trust. While the attacker's sandbox exists, the victim cannot get the label back (`subdomain_taken`), and a subdomain cannot be cleared (the minimum is 3 characters, and `""` and `null` are refused).

Where: `internal/server/sandboxes.go:669` (subdomain validation: no check against the `bx-` id-label space), `internal/server/feat_host.go:592–598` (`sandboxByLabel`: the id label wins only while `sb.Subdomain == ""`, then the subdomain row is used).

Repro:
1. bob: `crowdvm new --type small --name s1`, start a server on 3001 in it, `crowdvm host s1 3001 --public` → `http://bx-x75sb3gz-3001.r19.test:9973/`.
2. carol (unrelated account): `crowdvm new --name c1`, `crowdvm subdomain c1 bx-x75sb3gz` (accepted), start her own server on 3001 (answers `CAROL APP`), `crowdvm host c1 3001 --public`.
3. `curl http://bx-x75sb3gz-3001.r19.test:9973/x`.
4. bob: `crowdvm subdomain s1 bobapp`, then step 3 again.

Observed:
- Step 2: carol's sandbox shows `"subdomain":"bx-x75sb3gz"`, and `host` prints `http://bx-x75sb3gz-3001.r19.test:9973/`.
- Step 3: `hello from 3001 /x` (bob's app, behind carol's printed URL).
- Step 4: `CAROL APP 3001 /x`. `/p/bx-x75sb3gz/3001/x` also answers `CAROL APP`. bob's private-port URL `bx-x75sb3gz-3000…?_token=…` now answers `Port 3000 of this sandbox is not hosted. Run crowdvm host bx_ztejh84r 3000 first.`, which also tells the visitor carol's sandbox id. bob's app is only at `bobapp-3001` now.
- Setting bob's subdomain back to `bx-x75sb3gz` worked only after carol's sandbox was deleted.

Expected: a subdomain that matches `bx-<anything>` (the id-label space) is refused, except a sandbox's own id label. Alternatively, routing always resolves `bx-<id>` to that sandbox (old links then keep working after a rename) and never to a subdomain.

### 2. A save that failed before storing anything still counts toward the saved-snapshot allowance and is charged every day (wrong-result)

Severity: wrong-result. A first save whose upload fails keeps its `saved_snapshots` row with `status='failed'` and no `snapshot_id` (`available: false`). It stores nothing, but the allowance counts every row of the wallet, whatever its status. So the failed save uses up an included slot, it is billed at the extra rate (`1 saved snapshots above the included 0`) by the daily charge, and when the wallet is empty it blocks a real save with `credit_required`. The user is billed for storage that does not exist until they find and `rm` the failed entry.

Where: `internal/server/feat_saved_allowance.go:45` (`savedCount`: `SELECT COUNT(*) FROM saved_snapshots WHERE owner_id=?`) and `:90` (`chargeSavedExtras`: `SELECT owner_id, COUNT(*) FROM saved_snapshots GROUP BY owner_id`): neither filters `status`/`snapshot_id`.

Repro: carol (active org `acme`) saved `k1save` from a running sandbox. Its upload failed (I removed the layer on the node while the 42 GB first snapshot of a re-keyed copy uploaded, a stand-in for any upload failure: a node that loses the file, or a disk error), and the save ended `failed`. admin: `PUT /admin/policies/<acme> {"savedSnapshotAllowance":0}`, then one billing tick.

Observed: `k1save|team_c5…|failed|snapshot_failed: parent snap_4xyer…: …no such file or directory|` (no `snapshot_id`). `crowdvm snapshot list --org acme` allowance: `"used":1,"extra":1,"extraDailyCredits":1.555556`. Ledger: `-1.5555555555555556|usage|1 saved snapshots above the included 0 (2026-11-12)`.

Expected: only saves that hold a disk (`snapshot_id IS NOT NULL`), plus saves in progress for the admission check, count toward the allowance and the daily charge. A failed first save costs nothing.

## Round-18 fixes rechecked live

| r18 item | Result |
|---|---|
| `disk_rekey_pending` (opus 1 / sonnet 1) | Holds. carol: `new --from <bob's share> --new-key --name k1 --node r19n2`. While it re-encrypted, `crowdvm fork k1` and `fork k1 --new-key` both got 409 `disk_rekey_pending` (`retryable`, `retryAfterSeconds: 60`). After `the server dropped the old disk key`, `fork k1 --name kf1` worked: kf1 had k1's key (`v1:5f6WfP4…`), no `rekey_from`, and the marker and the blob's sha256 intact. `crowdvm snapshot k1 ksave` during the re-key (k1 still `provisioning`) saved the *parent* snapshot (bob's `snap_yv3k…`, his key, no `rekey_from`), so a deploy of it is a plain copy of bob's share, which carol could make anyway. Not a bypass. |
| Re-encryption longer than 3 minutes (opus 3) | Holds. With `r19n2` at 80 MB/s writes and the chain fetched lazily at 40 MB/s, preinit ran `re-encrypting: 0% … 11 MB/s, 1h0m52s left` up to `re-encrypted in 10m54s`. The node did not kill the boot, k1 went `ready`, and the server cleared `rekey_from`. Not tried: a boot cut off mid-re-encryption. |
| Member leaving with shared sandboxes (sonnet 2) | Holds. bob (member of carol's `acme`) made a shared `--no-env` sandbox o2. carol and bob each `crowdvm ssh`'d (2 lines in `authorized_keys`). bob had to stop o2 first (`member_has_running_sandboxes`), then left. o2 became personal with `sshKeysRevokePending`. After `crowdvm resume o2` only bob's key was left. |
| Continued conversation, Codex-only user (sonnet 3) | Holds. bob had only an `openai-api-key` (fake; `api.openai.com` etc. pointed at 127.0.0.1 in the VM's `/etc/hosts`, so nothing left the machine). `prompt s1 --provider codex --new` created `ab8e9089…`. `prompt s1 --resume ab8e9089…` without `--provider` was queued with `"provider":"codex"`, not refused by a Claude credential pre-flight. |
| Deploy queued for capacity, saved snapshot removed (opus 4) | Holds. `r19n2` paused. carol: `new --from <s3save code> --node r19n2` waited with `pendingBoot.parent = snap_zxx9…`. bob deleted s3 and ran `snapshot rm s3save`: the snapshot got `deleted_at` but not `purged_at`. After unpausing, d3 booted and `cat ~/m3` printed `s3marker`. |
| Bad node build that cannot read its config (opus 5) | Holds. `crowdvm-node update` on supervised `r19n1` to `0.9.0-r19.4`: three starts printed `error: simulated bad build: cannot read this config`, then `rolled back the updated crowdvm-node: it did not reach the server; restarting into the previous build`. 0.9.0-r19.1 came back, adopted its 3 VMs and reconnected. `update-skip.json`: `["0.9.0-r19.4"]`. |
| Paused node and current node (sonnet 5), copies of a grown sandbox (sonnet 4) | Not rechecked. |

## New ground: what held

- **Hosted ports, transfers:** through a public host-routed URL (behind the 40 MB/s proxy), a 2 GiB download arrived complete with a matching sha256 (1m16s), a 1.5 GB `curl -T` upload arrived with matching size and sha256 (45 s), a 5-minute drip response (1 line every 15 s) completed, and a websocket idle for 400 s still echoed.
- **Websockets:** host-routed, path-routed and private-with-`?_token=` upgrades all worked: a 20 MB binary echo, and 50 MB from the server in one frame. A private port with the `_port_auth` cookie and a foreign `Origin` answered 403 ("does not accept websockets or writes from another site's page").
- **Private/public switching:** without a token: 401. `?_token=` → 302 with an `HttpOnly; SameSite=Lax` host-only cookie. `crowdvm host` without `--public` on a public port mints a new token, and the old public URL answers 401. `host hide` cut an open websocket and an open streaming response at once. The URL then answered 404 until hosted again.
- **Header hygiene:** the proxy strips `crowdvm_session` and `_port_auth` from the `Cookie` header (other cookies pass), and sets `X-Forwarded-*`/`X-Forwarded-Prefix`. Path routing adds `Content-Security-Policy: sandbox …`. An `Authorization` header is forwarded unchanged. Hosted URLs never accept a CrowdVM key, so nothing tells a client to send one; I did not count this as a bug.
- **Snapshot files, encrypted:** `snapshot tree`/`pull` of an encrypted snapshot: 409 `snapshot_encrypted`, as designed.
- **Snapshot files, plain (legacy) sandbox p3:** the inspector booted from the stopped snapshot. `tree` listed 14 491 files including symlinks as `symlink`, a `--path proj` subtree with `depth=1` worked, and `pull --path /home/user/proj` (3 000 small files plus 300 MB) gave identical sha256 and contents. Names with a space and a quote came through. Absolute paths read through the read-only root bind (`/root/r.txt`). Requesting a symlink gives 409 `is_symlink`, a path through a symlink to `/etc` gives 400 `invalid_path`, and the CLI skipped the 3 symlinks that leave the destination. It also skipped `sublink -> ../proj/sub`, which climbs out of the pulled folder and back in. That is conservative and documented in the code, so not a bug.
- **Raw layer downloads of a share:** a share recipient can download the sharer's raw layers by saving their not-yet-booted copy (`saved_by` = them, `snapshot_id` = the sharer's snapshot), and also through `snapshot download` of their own copy, whose chain includes the sharer's layers. With v4 images `/` is an overlay on the encrypted partition (I looked for plaintext markers in the downloaded layer and found none), so this is the known "copies share the source key" / "secrets in deleted blocks of shared snapshots" class, not a new bug.
- **Deletion with an offline node:** with `r19n2` stopped (VMs kept running), `delete c1` (running, `error: node_offline`) and `delete c2` (stuck `archiving`) gave operations `processing|retrying` with `node_offline: … its disk is erased when it reconnects`. When `r19n2` came back, it adopted and immediately removed c1's VM, both operations ended `completed`, and both sandboxes' VMs and overlays were gone.
- **Account closing and purge:** `POST /account/close` (browser session from a `crowdvm dashboard` link) stopped dave's sandbox and revoked his API key (the CLI got 401). With `r19n1` offline I moved `purge_at` into the past. The purge ran within a minute, the deletion operation waited for the node, and after the node came back it completed: 25 of dave's snapshots purged, none left in server blobs or on the node, and his saved snapshot was removed.
- **Retention (enforce):** with p3's `stopped_at` moved back 15 days, `PUT /admin/retention {"runNow":true}` reported `expiredSandboxes: 1` and set `deleted_at` on its snapshots. `resume p3` answered 409 `snapshot_data_expired`. A saved snapshot of p3 (owner deploy) and its share (carol) still deployed with all data, because GC keeps saved layers.
- **Credits and grace:** with dave at 0.05 credits, the `low_credit` notice came at once. At ≤ 0 the `grace_started` notice came, the sandbox kept running, and `new` answered 402 `billing_required`. Org wallet `acme` at 0 with a running sandbox: grace started, and with `grace_since` 25 h back the next tick logged `stopping sandbox: grace period over` and sent the `billing_stopped` notice. `resume` answered 402. After a 5-credit grant, `resume` worked with the data intact and the grace state was cleared. An org sandbox with an empty org wallet is refused at create (402).
- **Saved-snapshot allowance:** with dave's allowance at 1 and his wallet negative, the first save worked. A second name failed with 402 `credit_required (1.5556 credits a day each)`. Re-saving the existing name worked. Bug 2 is the exception.
- **Deleting a re-keyed copy while its 42 GB first snapshot uploads** waits for the upload (`waiting_for_uploads`, the known sparse issue). A save of it (`k1save`) cannot be removed while saving (`save_in_progress`). Both are expected given the known issue.

## Not verified

- **Erasing an account leaves its SSH key in teammates' running shared sandboxes.** `erase` deletes `org_members` and `sandbox_ssh_keys` directly instead of going through `revokeMemberKeys`, so the `authorized_keys` lines stay in the VMs (code read). On guest-channel sandboxes the node's direct ssh port does not work at all (connection reset: the node has no guest token), so I found no way to use such a key without the API. I did not build a legacy (no-channel) cluster with `--direct-host` to try it there. Closing an account (not erasing it) keeps the org membership, so its keys stay by design.
- **Inspector VMs run the snapshot's own boot code** (user systemd units) on a node, outside capacity accounting and billing, for as long as tree/files calls keep them alive (`InspectIdle` 5 min, up to 4 per node). I did not check whether an inspector has network or how its egress is routed, because a test would need an outside destination.
- **The other session's sparse first-snapshot upload** was left alone. It made the re-key tests slow, and I removed one node-side layer by hand to stop a 42 GB upload (that is how bug 2's failed save came about).
- `crowdvm credits transfer` between personal and org wallets, monthly quotas, the `report` retention mode and docker-volume pulls were not run.
- Round-18 sonnet 4 and 5 were not rechecked.

## Cleanup

Deleted every sandbox (`s1`, `o2`, `d3`, `p3`, `o3`, `o4`, `p3d`, `p3b`; earlier `p1`, `p2`, `pc`, `pc2`, `k1`, `kf1`, `c1`, `c2`, `s3`, `o1`, and dave's `d1` through the account purge) and every saved snapshot (`ssave`, `ksave`, `psave`, `pcsave`, `p2save`, `pc2save`, `k1save`, `p3save`, `s3save`, dave's `da`). All 19 deletion operations ended `completed`, all 189 snapshots were purged, and both nodes' `vm/vms` and `vm/disks` held no sandbox files. I stopped both node units with `systemctl --user kill --kill-whom=all` + `stop` + `reset-failed` (no QEMU of mine was left), then the server and the throttle proxy. Nothing of mine listens on 9960–9979, no process matches `crowdvm-r19opus`, and no `crowdvm-node` or `r19opus-*` user unit is loaded. `~/.local/share/crowdvm-r19opus` (binaries, the patched source copy `src4`, `dist4`, node and server dirs, logs, scratch homes) is deleted. The worktree `/home/tl/dev/CrowdVM-r19opus` was removed after this report was copied to `docs/reviews/bugs-r19-opus.md` in the shared checkout (untracked). I touched nothing else there, and did not touch `crowdvm-field`, the e2e or sparse clusters, other worktrees, the user's systemd units or `~/.npmrc`.

## Fixes

Both fixed. Tests: `internal/server/fix_r19_test.go`.

1. **Another sandbox's `bx-<id>` label as subdomain** (`sandboxes.go`): a subdomain starting with `bx-` is refused (400 `invalid_subdomain`) unless it is the sandbox's own id label. Subdomains already taken this way stay until changed.
2. **A failed save counts toward the allowance** (`feat_saved_allowance.go`): the allowance counts saves that hold a disk or are being saved; the daily charge counts saves that hold a disk.

Also in this round: the sparse snapshot upload (bugs-r18-opus 2), from a separate change: a mostly-empty layer is compacted with `qemu-img convert -B` before its upload, so a `--new-key` copy's first layer costs about its data (1.5 GB instead of 52 GB in the live run).
