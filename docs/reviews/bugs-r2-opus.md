# Bug hunt r2 (opus): live cluster, 2026-11-11

Setup: one `crowdvm-server` on 127.0.0.1:9960. Two qemu nodes (`bn1`, later `bn3` in place of the removed `bn2`), 6 vCPU / 12 GiB / 30 GiB each, max 4 sandboxes. Image `ubuntu-24.04-crowdvm-minimal-v3`. Each node reached the server through its own throttling TCP proxy (40 MB/s per direction, measured: 582 MB image in 14.5 s). Killing a proxy is a network partition.

The tree changed under me at 08:17 local: `lifecycle.go` and `snapshots.go` got `bootParent` and `abandonedLayer`. I rebuilt the server from the current tree and re-ran bug 1 on it. Bugs 2–4 are in code that did not change. One head-move I saw on the old build is fixed by that change, so it is not listed.

## Bugs

### 1. A rebuild waiting for capacity adopts the old machine as "ready" with no node. Afterwards stop leaks the VM. (stuck + leak)

**Where:** `internal/server/lifecycle.go:603` and `:650-651` (`reconcileVM`), `internal/server/lifecycle.go:1001-1006` (`runStop`), `internal/server/lifecycle_place.go:120` and `:339`.

**Repro** (reproduced 3 times, twice on the current build):
1. Start sandbox A on node N1 and let one periodic layer upload. Fill the other node, so the rebuild has to wait for capacity. A small sandbox reserves 51 GiB of disk, and capacity is 30 GiB × 4.
2. Partition N1 (kill its proxy) for more than `NodeLostGrace` (3 min). `rebuildLost` queues the rebuild. `queueBoot` sets `node_id=NULL` and `state=provisioning`, with `waitingForCapacity:true`.
3. Reconnect N1. Its VM of A still runs.

**Observed:**
- `reconcileVM` treats `sb.NodeID == ""` as "this node". The check `sb.NodeID != nodeID && sb.NodeID != ""` lets it through, then `case sb.starting(): s.markReady(sb)` runs.
- A becomes `state=ready, node_id=NULL, health=ok`. A billing run opens with `node_id=''`.
- The leftover `pendingBoot` stays in `extra`, but `placeWaiting` only looks at `provisioning`, so A never gets placed.
- `crowdvm exec` returns 502 `sandbox_direct_failed`: "node … is offline". `crowdvm ssh` returns `machine_not_running`: "has no machine. Resume it first".
- `crowdvm resume` waits on "ready" until it times out. `crowdvm recover` returns `no_machine`: "Resume it instead".
- It stays like this after capacity frees up (checked for 45 s with an empty node).

`crowdvm stop A` then "completes" (`runStop`: `sb.NodeID == ""`, so `finishStop`) and never touches N1. The VM keeps running there. After the stop it kept taking and uploading periodic layers (07:26:00, 07:27:10 and 07:28:21 after a 07:26:12 stop). N1 then refused new boots for capacity, because the server reserved nothing for the leaked VM. A pinned create looped on "node could not take the boot … reason=capacity" every 6 s.

A later `resume` placed A on N1 again and failed with `start_failed: … vm_exists (running)`. Only a second resume, through the error path's `Cleanup`, recovered it. Data survived (f1/f2/f4 and a 300 MB md5 matched).

**Natural variant seen:**
1. A TTL-70 sandbox's node was partitioned. Its rebuild was queued at 09:06:59 and the node came back at 09:07:07.
2. The sandbox went `ready` with no node. The TTL reaper auto-stopped it at once.
3. It ended `archived` with its VM still running on the node. The node then refused another sandbox (`envsb`) for capacity until I deleted the stray one.

**Expected:** a VM report for a sandbox with no node, or one waiting for placement, is an orphan. Delete it, or adopt it by setting `node_id` and clearing `pendingBoot`. Never mark it ready without a node. A stop of a sandbox whose queued boot still has a `Cleanup` node should delete the machine there.

**Suspected cause:** the `sb.NodeID != ""` exemption in `reconcileVM`. Also, `queueBoot` drops the node the old machine lives on; it is only kept as `pendingBoot.Cleanup`.

### 2. A boot whose POST the node handled late fails with `vm_exists` and leaves a healthy VM under an `error` sandbox (stuck)

**Where:** `internal/server/lifecycle.go:460-472` (`boot`) and `:286-313` (`requeueBoot`).

**Repro:**
1. Freeze N1's `crowdvm-node` with SIGSTOP. Then `crowdvm new --node bn1 --no-wait`.
2. After 45 s (`NodeTimeout`) the server drops the session, and the boot's `POST /v1/vms` fails without an HTTP answer. `requeueBoot(offline)` logs "placing it again … reason=node_offline".
3. SIGCONT N1. The node now serves the buffered POST and boots the VM ("guest healthy 3.038s after launch").
4. The server's retry to the same pinned node gets `409 vm_exists`.

**Observed:**
- The sandbox is `state=error, error="start_failed: node: HTTP 409: {…vm_exists… already exists (starting)}"`.
- Meanwhile its VM runs and keeps taking snapshot layers.
- It does not settle on its own (checked 30 s later). `reconcileVM` only recovers `error` when the error is `node_offline`.
- A manual `crowdvm resume` fixes it.

**Expected:** `vm_exists` for the sandbox being booted should adopt the running machine (GET it and `reconcileVM`), or delete it and retry. Also, a requeue after an unanswered POST should carry the old node as `Cleanup`.

**Related, cosmetic:** in the same run, `onNodeOffline` (`lifecycle.go:715`) and `requeueBoot` raced. The sandbox sat in `provisioning` with `error=node_offline, health=degraded` while it waited for placement.

### 3. Env secret files with an absolute path are accepted, then silently never written (wrong-result)

**Where:** `internal/server/feat_env.go:552` (`envValidateFiles` has no path check), `internal/server/feat_env.go:576` (`envHomeRel` returns "" for `/…` and `~…`), `internal/server/feat_env_guest.go:83` and `:429` (such files are skipped).

**Repro:**
1. `echo filedata | crowdvm env set-file e1 /home/user/.secret` returns `{"success":true,"versionNumber":3}`. `crowdvm env info e1` lists `/home/user/.secret`.
2. `crowdvm new --environment e1` gives `$MYVAR` from the same environment, but `~/.secret` is missing. This holds on a fresh boot and after `env upgrade`.
3. With the relative path `.secret2`, the file is written (`-rw------- user`).

**Expected:** reject absolute paths (or map `/home/user/…` to a relative path) with a 400 that says paths are relative to /home/user. Boat's docs say they are relative. `crowdvm env set-file --help` says nothing about it.

### 4. `crowdvm delete` on a permanently blocked deletion hangs 10 min, then reports `"event":"deleted"` (wrong-result)

**Where:** `internal/cli/cmd_sandbox.go:777-797` (`followDeletion`).

**Repro:**
1. Stop a sandbox on N2. Partition N2 while the stop layer uploads.
2. `crowdvm nodes remove bn2 --yes`. This gives `snapshotCopiesLeft: 5`.
3. Resume the sandbox elsewhere, then `crowdvm delete ID --yes --json`.

**Observed:**
- The operation is `status=blocked`, `lastError="node_removed: 5 snapshot copy(ies) stayed on a node that was removed while offline…"`. That error is truthful and permanent.
- The CLI polls every second for exactly 10 minutes (07:52:54 to 08:02:57), then prints `{"event":"deleted",…,"operation":{"status":"blocked"…}}`. By code it returns nil, so exit 0.

**Expected:** treat `blocked` as terminal. Return right away with a non-"deleted" event (or a non-zero exit code) and the reason. An agent loses 10 minutes and gets a success event for a deletion that did not complete.

### 5. `machine_error` keeps `health_reason=node_offline` (cosmetic)

**Where:** `internal/server/nodes.go:832-833`.

**Repro:** crash the node, kill the sandbox's qemu, and restart the node. The sandbox shows `error="machine_error: qemu not running after node restart"` but still `health_reason=node_offline`. The UPDATE rewrites `error` only.

## What held

- **Plain stop while the node is offline:** the stop stays `failing` ("node_offline …"). After `NodeLostGrace` it turns forced (`forced=1`), and the head falls back to the newest stored layer.
- **Node returns after a forced stop:** the leftover VM is deleted. Its unuploaded layers, including one taken after the forced stop, upload and become the head. The resume had every file and the 300 MB md5 matched.
- **Partition during a layer upload:** the layer is re-registered and uploaded on reconnect (`uploadStranded`).
- **Node lost > grace with capacity elsewhere:** `rebuildLost` queues a rebuild from the stored head ("rebuilt" notice).
- **Node crash (SIGKILL) right after qemu launch:** the VM is adopted on restart and the sandbox recovers to `ready`. Exec works.
- **Node back without the machine (404):** `restartLostMachine` boots from `chainTip`, the unuploaded newest layer, on that node. f1, f2 and the 200 MB md5 survived.
- **Node back with the machine dead:** the error becomes `machine_error: qemu not running after node restart`, and resume works.
- **Server SIGKILL mid-stop:** after restart the stop is retried with the same `snapshot_id`. The node's 404 adopts that exact layer (attempts=3). The resume had m5/m6 and the big file.
- **Node SIGTERM mid-stop:** the stop retries after reconnect and completes. Data verified.
- **Removing an offline node that held a stopped sandbox's unstored stop layer:** the head falls back to the newest stored layer (`fallBackFromRemovedNode`), and resume works on the other node. The removed node is refused with 401 when it reconnects.
- **TTL expiry while the node is offline (pinned):** the sandbox stays `node_offline`. On reconnect it recovers and is auto-stopped with a proper stop layer.
- **Races:**
  - resume + delete: deleted, no VM left.
  - fork + stop: the fork has the latest data and the source is archived.
  - fork to the other node + source delete mid-hydration: the lazy chain hydrates, data correct.
  - stop + resume: ends ready, data correct.
- **Named save (`crowdvm snapshot`) with the node partitioned 1 s in:** stays `saving` and becomes `ready` after reconnect. `new --from save2` on the other node had the 200 MB md5.
- **Everyday CLI commands:**
  - `new`, `exec` exit codes (7), `exec --json`, `ssh` exit code (3), `scp` both ways (md5 equal) and `push` (git-aware as documented);
  - `host` (token URL 302, then 200 with the file; 401 without the token; `*.localhost` host header);
  - `snapshot` save/list/tree/pull --path, `new --from` saved snapshot, env variables, `env upgrade`.
- **Leaks after deleting everything:** both nodes have no VMs, disks or snapshot files, and the server blob store is empty. Only the 5 copies recorded on the removed node remain, matching the `blocked` operation.

## Not verified

- A boot interrupted by `nodeFullError{offline}` where `onNodeOffline` wins the race, and the `machine_error` → resume window against the current `abandonedLayer` rule (only checked on the old build).
- `go test ./internal/...` was not run.
