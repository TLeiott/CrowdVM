# Bug hunt r3 (opus): live cluster, 2026-11-11

Setup: one `crowdvm-server` on 127.0.0.1:9960 (`--webhook-allow-local`). Two qemu nodes `r3n1` and `r3n2` (UIs on 9961 and 9962), later 12 vCPU, 24 GiB, 60 GiB disk and max 6 sandboxes each. A third node `r3n3` (UI 9963, 6 vCPU, 10 GiB) took the destructive tests, so freezing it with SIGSTOP did not disturb the two sub-agents. Image `ubuntu-24.04-crowdvm-v3` (full) via reflink. Binaries are from `make build` at 09:39. Two sub-agents worked against the same server, one on the SDKs and one on orgs, keys, webhooks and credits. Their findings are marked "(sub-agent)". I re-ran the ones marked "confirmed".

The tree changed while I ran. `orgs.go`, `acct_settings.go`, `acct_misc.go` and `cmd_exec.go` changed at 09:44, after my build. The `orgParam` cross-org fix (sub-agent bug 2 below, also `bugs-r3-sol.md` 1) is in the current tree. I checked every other bug below against the current source.

Everything I started is stopped. The data dir (`~/.local/share/crowdvm-r3opus`) and the two `~/.ssh/crowdvm/*.conf` files that `ssh-config` wrote are deleted.

## Bugs

### 1. A boot the node serves late is adopted, then deleted as an orphan. The sandbox stays "starting" with no machine (stuck)

Where: `internal/server/lifecycle.go:614-631` (`reconcileVM`), with `adoptQueuedMachine` at `:952`.

Repro (2 of 2 runs):
1. `kill -STOP <r3n3 pid>`, then `crowdvm new --node r3n3 --type small --no-wait`.
2. After 45 s the server logs "node could not take the boot; placing it again ... reason=node_offline". The requeue sets `pendingBoot.Cleanup=r3n3`.
3. `kill -CONT`. The node serves the buffered `POST /v1/vms`. `CreateVM` emits a `starting` state event, and the reconnect heartbeat lists the same VM.
4. The server logs "node came back with the machine of a queued rebuild; adopting it".

Observed:
- The node has no VM. Its status API shows `vms: []`, `vm/vms/` is empty and no qemu runs. The node never logged a boot.
- The sandbox stays `state=provisioning` (CLI: `starting`), `node=r3n3`, `health=degraded`, `health_reason=node_offline`, `pendingBoot=null`.
- Nothing changed for 3 min in run 1 and 2.5 min in run 2. Only a later disconnect of the node cleared it, to `error: machine_lost` and then to `node_offline`. A manual `resume` then worked.

Expected: the adopted machine keeps running and the sandbox becomes ready.

Suspected cause: two reports of the same VM (the event and the heartbeat) run `reconcileVM` at the same time. Both load `sb` with `NodeID==""`. The first one adopts. The second one's `UPDATE ... WHERE node_id IS NULL` hits 0 rows, so `adoptQueuedMachine` returns false. Its stale `sb.NodeID` ("") then differs from `nodeID`, and the orphan branch sends `DELETE /v1/vms/{id}`. Fix: reload `sb` after a failed adoption, before the orphan test. Also, nothing ever notices a starting sandbox whose node no longer reports a machine for it.

### 2. Deleting the snapshots of a running sandbox makes its next stop unresumable (data-loss)

Where: `internal/server/snapshots.go:608-637` (`deleteSnapshots`) and `internal/server/stranded.go:18-62` (`snapshotsNeeded`), used by `gcSnapshotsNeeded` at `snapshots.go:367`.

Repro:
1. Run a sandbox for a few minutes so it has periodic layers. Write `bd.txt`.
2. `crowdvm snapshot delete --all bx_8kga9ues --yes`. `crowdvm snapshots bx_8kga9ues` is now `[]` and the head is NULL.
3. `crowdvm stop bx_8kga9ues`, then `crowdvm resume bx_8kga9ues`.

Observed: `resume_failed: The sandbox's disk cannot be restored: snapshot snap_tpfbpqm3jz9agehrgebv no longer exists.` The stop layer `snap_pthx…` (parent `snap_gn6d…`, the live layer) builds on `snap_tpfb…`. The GC purged `snap_tpfb…` (purged_at set, blob deleted) between the delete and the stop. Every file in the sandbox is lost, including what it wrote after the delete. Boat's docs say only "a stopped sandbox with no snapshot has nothing to resume from". A running sandbox keeps its disk, and its stop should be resumable.

Suspected cause: `snapshotsNeeded` keeps only ancestors of non-deleted snapshots, heads, saved snapshots and waiting forks. The chain under a running machine (its `bootParent` and the newest local layer) is not a root. Once the head is cleared, the GC purges the layers the VM's overlay still builds on. Its next live or stop layer then points at purged parents. Either count the running machine's chain as needed, or make the next layer after such a delete a full (flattened) one.

### 3. `crowdvm env access NAME --remove MEMBER` removes nobody and reports success (wrong-result)

Where: `internal/cli/cmd_env.go:388-395` (`updateAccess`).

Repro (org `vteam`, member `shr`):
1. `crowdvm --org vteam env access oe --add shr` returns `members: ["usr_3m4dxymqf6ttzk0z"]`.
2. `crowdvm --org vteam env access oe --remove shr` returns exit 0 and the same `members: ["usr_3m4dxymqf6ttzk0z"]`.
3. `shr` can still start sandboxes with `oe`.

Only `--remove usr_3m4dxymqf6ttzk0z` works. The server stores member user ids, and the CLI compares the given username or email to those ids. A related problem: `--only ''` is a no-op, so the CLI cannot restrict an environment to owners and admins only. Its help ("only owners, admins and these ... emails") implies it can. Restriction itself works: `--only dev` gives `shr` 403 `environment_forbidden`.

### 4. `crowdvm nodes undrain` does not undo a node-side pause, although the node guide says it does (wrong-result)

Where: `docs/guide/node-owners.md:131`; `internal/server/scheduler.go:193` checks `Info.Paused`, which only the node sets.

Repro: `crowdvm-node config set paused true`, then `crowdvm nodes undrain r3n2` returns exit 0 and `disabled:false`. The node stays `paused` (status UI, `config get paused`, `info.paused:true`). A pinned `new --node r3n2 --fail-fast` is cancelled with "node is paused by its owner". `crowdvm nodes` shows no paused flag at top level, so the owner sees an undrained node that takes nothing. Either undrain should also clear the pause, or the guide row should split the two.

### 5. `exec` exits 1 when the remote command dies from a signal (wrong-result)

Where: `internal/cli/cmd_exec.go:105-112` (`exitCode` maps a nil exit code to 1).

Repro: `crowdvm exec ID 'kill -9 $$'` exits 1, and so does `'kill -TERM $$'`. `--json` shows `"exitCode":null,"signal":"SIGKILL"`. `--timeout 0` and `--attach` of a killed detached process behave the same. An agent cannot tell an OOM kill from `exit 1`. A shell would give 137 and 143. The docs say "`exec` exits with the remote exit code".

### 6. Conflicting secret file paths are accepted, and one file is silently not written (cosmetic)

Where: `internal/server/feat_env.go:552` (`envValidateFiles`).

Repro: `env set-file e2 a/b`, then `env set-file e2 a/b/c`. Both return success. In the sandbox `a/b` is a file and `a/b/c` is missing. Only the guest journal says `file /home/user/a/b/c: mkdir /home/user/a/b: not a directory`. A path with a newline (`nl\nx`) is also accepted.

### 7. `crowdvm info --json` drops `directEndpoints` and other fields the API returns (cosmetic)

Where: `internal/client/types.go:41` (`Sandbox` has no `DirectEndpoints`, `Storage`, `ImageID`, `NodeID`, `Placement`).

Repro: with direct ports on, `GET /api/v1/sandboxes/ID` has `"directEndpoints":{"tcp/9964":"127.0.0.1:9964",…}`, but `crowdvm info ID --json` has no such key. It is the same re-encoding problem as sub-agent bug 13 (`api-key list`).

### 8. `crowdvm-node status` prints Go maps (cosmetic)

Where: `cmd/crowdvm-node/main.go:440` and `:456`.

Output: `Benchmark score 690, tier gold  map[cpu:high disk:medium …]` and a DIRECT column of `map[]`.

### 9. `crowdvm-node config set` accepts values the daemon cannot use (cosmetic)

Where: `internal/node/config.go:351-400` (`Set`).

These all save with exit 0: `cpus 1000` (16-thread host), `memory 1K` (stored as 0, which means half the RAM), `snapshot-interval 1s`, `direct-host notanip`, `ui-addr :99999`, `server notaurl`.

### 10. The `delete` error event for a blocked deletion has no `code` (cosmetic)

Where: `internal/cli/cmd_sandbox.go:805`.

The round-2 fix works: it returns after 1 s with exit 1 and an `event:"deletion_blocked"` line. The last line, though, is `{"error":"deletion … is blocked: node_removed …","event":"error"}` without `code` or `status`. The agent guide says error lines carry `code`.

### Sub-agent findings: orgs, keys, credits

11. An `account.admin`-only key can create a never-expiring `*` service-account key (security). Confirmed live: a 1-hour key with `actions:["account.admin"]` got 201 from `POST /orgs/vteam/service-accounts {"preset":"admin","ttl":"never"}` with `scope.actions:["*"]` and `expiresAt:null`. `internal/server/acct_settings.go:263-298` (`createServiceAccount`) never checks that the creating key covers the new key. Still in the current tree.
12. An org-bound key can read, rename and switch other orgs (security). Confirmed live on my 09:39 build. `PATCH /orgs/vteam2` with a `vteam` key renamed vteam2. Fixed in the current tree (`orgs.go:238` `api_key_org_mismatch`).
13. A `read-only` key can switch the account's active wallet (security). Confirmed live: `PATCH /orgs/active {"org":"vteam2"}` returned 200, and `crowdvm org status` then showed vteam2. `internal/server/orgs.go:158` registers it as `account.read`. Still in the current tree.
14. Concurrent credit transfers overdraw the wallet (wrong-result). Confirmed live: 10 parallel transfers of 10 from a balance of 10.40 gave 2 × 200, and the balance went to −9.60. `internal/server/credits.go:267` checks the balance outside the transaction at `:270`.
15. History recorded while a sandbox was private becomes readable by teammates after `share` (leak, sub-agent, not re-run). `feat_history.go:187` tags privacy when the record is pulled from the guest, not when the command ran.
16. A removed member's running org sandbox keeps billing the org wallet (wrong-result, sub-agent). `handleRemoveMember` (`orgs.go`) moves `sandboxes.org_id` but not the open `sandbox_runs.wallet_id`.
17. Huge API key TTLs overflow into already-expired keys (wrong-result, sub-agent): `{"ttl":"106752d"}` gives `expiresAt` in 1739. `internal/server/apikeys.go:224` multiplies before the max check. Unambiguous in code.
18. A bad preset or TTL leaves an orphan `svc-*` member in the org (wrong-result, sub-agent). `acct_settings.go:269-279` creates the user and membership before `parseTTL` and the preset check. Unambiguous in code, and `svc-orph1`/`svc-orph2` existed in my DB.
19. Negative per-member caps are accepted (sub-agent): `concurrentCap:-5` returns 200 and then blocks all starts.
20. A sandbox-limited key can read any key's usage by id (minor leak, sub-agent). `apikeys.go:540` (`handleKeyUsage`) does not check `Restricted()`.
21. Deleting an org drops its wallet balance without a warning (sub-agent, `orgs.go` `handleDeleteOrg`).
22. Cosmetic (sub-agent): `api-key list --json` drops `preset` (`client/types.go`); `crowdvm-server user grant-credits U N --reason x` fails because flags after positional arguments are not parsed; `crowdvm credits` prints a Go map; there is no `crowdvm webhook test`.

### Sub-agent findings: SDKs

23. Filtering on more than one label returns `[]` in both SDKs (wrong-result). `sdk/python/src/crowdvm/client.py:42` and `sdk/typescript/src/client.ts:79` join lists with commas (`label=a=1,b=2`). The server wants repeated parameters. Confirmed in code.
24. Python streams end silently when the server cuts the connection (data-loss for the caller). `client.py:201` treats `readline()==b""` as a normal end, so no exit frame and no exception. TypeScript raises `APIConnectionError`. Confirmed in code.
25. Cancelling a Python async stream raises `ValueError: generator already executing` and blocks `asyncio.run` until the command ends (stuck, sub-agent). `sdk/python/src/crowdvm/aio.py:20,25`.
26. TypeScript streams have no timeout before the response headers arrive (stuck, sub-agent). `client.ts:161` gives streams `timeoutMs=0`.
27. `command(..., timeout_seconds=None)` crashes with `TypeError` in Python (crash). `client.py:284` calls `float(body.get("timeoutSeconds", 30))` on a present `None`. Confirmed in code.
28. Python reports `APIConnectionError: Connection reset` instead of the server's 400 for oversized writes (sub-agent). Also `AsyncCrowdVM.with_options()` returns a sync client (`aio.py:38-49`).

## Round-2 fixes rechecked

- Queued rebuild adopts its returning machine: held. `rb2` was placed by label on r3n3 while r3n2 was full. r3n3 was frozen for 4.5 min, and "rebuilding sandbox after node loss" queued it with no node. After SIGCONT the server logged "adopting it", the sandbox was `ready` on r3n3, only one VM existed, and the md5 of 30 MB written before the last snapshot plus a file written after it both matched. The late-boot variant fails, see bug 1.
- `vm_exists` during a boot: the replace-and-retry path never ran in my sequence, because the orphan delete in bug 1 came first.
- Secret file paths: held. `/home/user/.abs1` maps to `.abs1` and `~/tilde1` to `tilde1`. `/etc/passwd`, `../escape`, `a/../../b`, `.`, `/home/user` and `/home/userx/a` get 400 `invalid_secret_files`. `rm-file` with `/home/user/tilde1` removes the normalized entry. Setting `.abs1` again replaced the earlier entry.
- CLI `delete` at a `blocked` deletion: held, 1 s, exit 1 (but see bug 10).
- `noHeadErr` gives a final `snapshot_lost`: held. A fresh sandbox on r3n3 wrote 1.5 GB and was stopped. r3n3 was frozen while it uploaded the 1.6 GB stop layer, then removed (`snapshotCopiesLeft:1`). `resume` returned 409 `snapshot_lost`, `retryable:false`.
- `abandonedLayer`: not re-run live. `go test ./internal/server -run 'Bughunt|QueuedRebuild|StopAdopts|Abandon|SecretFile|EnvHome'` passed, as did the `internal/cli` delete tests.

## What held

- Environments: versions on every change, `env upgrade` live (variables changed, old secret file removed, new one written, setup not re-run) and stopped (applied on resume). A blocking setup script runs (`setupBlocking` gates only prompts, as in Boat). Repos clone. Rename shows the new name on pinned sandboxes. A pinned sandbox survives `env rm`, stop and resume, and a fork keeps the old variables. Access restriction with `--only USER` returns 403 to others and hides `env info` from them.
- Saved snapshots: `snapshot ID NAME` (twice, which replaces it), `new --from` with `--host` and `--file` (applied server-side even when the CLI was killed while waiting), `--count 3`, a share code used by another account (env secret file, repo and variable are not carried over), `snapshot rm` while a deployed copy exists (the copy stops and resumes with its data), a share code after `rm` (404), and `snapshot info`, `latest`, `tree` (11.5k files in 33 s) and `pull --path` (404 for missing paths).
- Host URLs: token 302 to a path-scoped cookie, 401 without it, and the cookie does not work for another port. `--public` gives 200 without a token. Back to `--private` rotates the token and the old one gets 401. `hide` gives 404, re-host keeps the token, and `*.localhost` host routing works. A stopped sandbox gives 503 with a clear message. Ports 0, 70000 and 22 are refused.
- Direct ports (r3n2 with `direct-host 127.0.0.1`, after the sub-agents finished): TCP 9964 and UDP 9965 echo work, and the direct ssh port works with the CLI key. Replacing the list closes the old ports, `--clear` closes all, and 22 is refused.
- ssh, scp and push: 100 MB scp both ways and 100 MB `scp --api` both ways (md5 equal), recursive `--api` copies with spaces and `ü` (cp semantics for new and existing targets), stdin piping through `ssh` (md5 equal), `ssh` exit code 42.
- exec: exit codes, 124 on timeout, `--timeout 0` streaming, argv mode, `--detach`/`--status`/`--attach` (exit code 5 passed through, stdout and stderr split), `--kill` and a second `--kill`, and 404 for an unknown pid.
- prompt: a codex `gpt-6-luna` turn with `--local-login` (`queued` shows `kind:subscription`, `source:~/.codex/auth.json`). The access token appeared nowhere on the sandbox disk, in `/var/log`, the journal, `history` or `events`, apart from my own sudo grep command line. `--no-wait`, `--attach` (file in `~/attachments`), `interrupt` (also kills the agent's `sleep 120`), `conversations` and `events`. Without credentials it returns 409 `provider_not_configured`.
- Node app: `config set paused true` applies at once and the server refuses placement after the next heartbeat. `nodes drain`/`undrain`. `bench --quick` takes 2.1 s and the server shows the new score and tier within 20 s. The status UI page and `/api/status` work, and POST without the UI header is refused. A restart of both nodes with new capacity kept every VM. A removed node gets 401 and keeps retrying.
- Fork of a running sandbox to a smaller type, fork after its environment was deleted, `run --push` (exit 3, sandbox deleted), and `run` interrupted with SIGINT (sandbox deleted).
- Leaks: after deleting everything, no VMs, overlays or snapshot files remained on r3n1 or r3n2, and the server blob store was empty. Only the one copy on the removed r3n3 remains, matching its `blocked` operation.

## Not verified

- Sub-agent bugs 15, 16, 19, 20, 21, 25, 26 and 28 were not re-run by me.
- `--local-login` with `claude` and with API keys. Webhooks I did not re-check.
- Bug 1 with a pool that has room elsewhere (unpinned). I only ran it pinned to the frozen node.
