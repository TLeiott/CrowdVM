# End-to-end suite

`test/e2e` runs CrowdVM for real on one machine: it builds the binaries, starts
a server and two QEMU/KVM nodes, imports the base image, and drives every major
feature group of `docs/PARITY.md` through the `crowdvm` CLI (as a coding agent
would), plus direct API and SDK calls where the CLI does not apply.

The groups run in four **lanes** at the same time, each lane on a private
cluster (server + 2 nodes) of its own; a lane runs its groups in order, each
one alone on its cluster (`lanes` in `test/e2e/e2e_test.go`). Groups that
depend on each other share a lane (`webhooks` → `lifecycle` → `otel`). A lane's
cluster starts when its first selected group runs, so `-run` with one group
starts one cluster.

## Run it

```sh
make e2e                                   # build + whole suite (60 min timeout)
CROWDVM_E2E=1 go test -count=1 -timeout 60m -v ./test/e2e/
CROWDVM_E2E=1 go test -count=1 -timeout 30m -v -run 'TestE2E/(exec|files)$' ./test/e2e/
```

Needs `/dev/kvm`, `qemu-system-x86_64`, `qemu-img`, `git`, `python3`, `node`
(with `sdk/typescript/node_modules` for the TypeScript SDK), internet access
from guests (DNS/HTTPS checks and the OpenCode free model), and ~75 GB free on
the disk holding the run directories (each cluster copies the base image to its
server and both nodes, ~13 GB; `snapshots` writes ~11 GB more, stored twice).
`CROWDVM_E2E_CLUSTERS=2` needs about half of that and takes about twice as long.

| Variable | Meaning |
|---|---|
| `CROWDVM_E2E=1` | required; otherwise the suite is skipped |
| `CROWDVM_E2E_IMAGE` | base image qcow2 (default `~/.local/share/crowdvm-dev/images/ubuntu-24.04-crowdvm-v2.qcow2`) |
| `CROWDVM_E2E_DIR` | parent of the per-run directory (default `~/.local/share/crowdvm-dev/e2e`; not tmpfs) |
| `CROWDVM_E2E_KEEP=1` | keep the run directory (data dirs, logs) afterwards |
| `CROWDVM_E2E_CLUSTERS=N` | run the lanes on N clusters (default 4, one per lane); lanes that share a cluster run one after the other, `1` runs every group on one cluster |
| `CROWDVM_E2E_BOOTLOOP=N` | the opt-in `bootloop` group boots N VMs: create → live fork → stop → resume on the other node (lazy chain) → stop → fork, ≤ 4 VMs at a time; a failed boot's serial console, qemu log and disk layers go to `<CROWDVM_E2E_DIR>/bootloop-failed/` (`-run 'TestE2E/bootloop'`) |
| `CROWDVM_E2E_BOOTLOOP_MODE=create` | `bootloop` with creates only (4 workers) |

On failure the logs (`server.log`, `e2e-node-1.log`, `e2e-node-2.log`,
`cli-*.log`) of the failing lane are copied to `$TMPDIR/crowdvm-e2e-logs-<run>`.

## What the harness does

Per lane, in its own `<run>` directory (`run-<time>-l<lane>-*`):

1. `go build` of `crowdvm`, `crowdvm-server`, `crowdvm-node` and `crowdvm-guest`
   (CGO off) into `<run>/bin`.
2. Starts host-side receivers: webhook receiver (verifies `X-CrowdVM-Signature`),
   OTLP/HTTP collector, and a fake Anthropic vendor (the server's AI proxy is
   pointed at it with `CROWDVM_AI_UPSTREAM_ANTHROPIC`).
3. `crowdvm-server serve` on a free `127.0.0.1` port with its own data dir,
   `--public-domain localhost`, start limits off.
4. Admin user + `crowdvm login` into an isolated `CROWDVM_CONFIG_DIR` (and `HOME`),
   plus an admin API key for direct API calls.
5. Two nodes: `crowdvm nodes join-token` → `crowdvm-node join` (full benchmark),
   qemu driver, own data dir and UI port, 8 vCPU / 18 GiB / 2 sandboxes each
   (≤ 4 VMs at once). Node 2 allows this host's LAN address (`net.allow <ip>/32`)
   for the git-daemon repo; node 1 keeps the default egress policy.
6. `crowdvm-node image import` + `image push` on node 1, `image pull` on node 2,
   then `crowdvm-node run` for both. The clusters take turns for this step, lane 1
   first (four copies at once took 2.5 min for all, one at a time ~20 s each).

`snapshots` moves node 1 behind a TCP proxy to the server (`nodeLink`) and slows
its uploads to 16 MB/s while it races a stop, a node restart or a fork against
an upload, so that the race is the same however busy the disk is. Node 1 goes
back to the server's address after the group.

Teardown (when the lane ends, SIGINT/SIGTERM, and 45 s before the `go test` deadline):
SIGTERM then SIGKILL each process group, then SIGKILL every process whose argv
contains the run directory (QEMU runs in its own process group and outlives
the node), then remove the run directory. A new run also kills leftovers of
crashed earlier runs; a run whose `go test` process is still alive (its
`owner.pid`) is left alone, so two checkouts or agents can share `CROWDVM_E2E_DIR`. Every process gets `Pdeathsig=SIGKILL`.

## Groups

Sub-tests of `TestE2E` (`-run 'TestE2E/<group>'`), in the order of the original
single-cluster suite; the lanes are in `test/e2e/e2e_test.go`:

| Group | Covers |
|---|---|
| nodes | join + benchmark, `nodes` (score, tier, categories, capacity), `nodes info/update` (labels), drain/undrain, `crowdvm-node status`, node UI |
| create | sizes (small, default), `--ttl` + auto-stop, `-e`, `--file`, `--setup-file` (effect + `setupStatus`), `--node` pin, `--node-label`, unsatisfiable placement with `--fail-fast` (xlarge, min-tier, label, gpu+category), list/rename/extend/`--no-auto-stop` |
| exec | sync, exit codes, stderr, argv form, `--cwd`, timeout 124, `--stream`, `--timeout 0`, `crowdvm run` (exit code, sandbox removed), `--detach`/`--status`/`--attach`/`--kill`, `ps`, `history` (list, output, `--mode`), command-history API |
| files | every files endpoint (read/write utf8+base64, list, info, folder, move, permissions, replace, search, find, raw 6 MiB, signed upload/download URLs, tamper, path escape, delete), `scp` (both ways, `-r`, `--api`), git-aware `push`, `exec --push` |
| ssh | `ssh ID -- cmd`, `ssh … bash -s` with stdin, `ssh-config --print`, `tunnel ID 22`, `forward` (local) and `forward --reverse` |
| host | private URL (token → 302 + cookie → 200, 401 without/wrong token), host routing `<id>-<port>.localhost`, `--public`, `subdomain`, firewall deny/allow (403), reserved port, list/url/hide (404), 502 without listener |
| desktop | `desktop --no-open`, viewer page, VNC over the desktop websocket (RFB banner, bogus token refused), screenshot PNG, input actions, invalid action 400, status; `browser` URL + websocket + Chrome as `crowdvm-browser` |
| network | guest cannot reach a host listener via 10.0.2.2 or any host address (LAN, tailnet, bridges), nor 169.254.169.254; reaches the server at `10.0.2.2:<port>`, DNS and HTTPS on the internet |
| webhooks | create (secret), test delivery + signature, rotate, deliveries, update/remove; `telemetry set` (test export) |
| lifecycle | ready webhook, live fork on the same node, stop (archived webhook, no hydrated), resume (hydrated webhook), history/processes while stopped, exec refused, resume on the other node (chain pull), `/tmp` not kept, fork of a stopped sandbox onto the other node, SIGSTOP qemu → degraded (webhook) → `recover` → ready + recovered webhook, delete + deletion operation |
| snapshots | `snapshots`, `snapshot latest/tree/pull --path`, `snapshot pull <sandboxId>` (newest snapshot), snapshot files API (file, folder tar, 404), download chunks, `snapshot delete --all` — all while stopped; then `resume` (CLI) and `resume`/`fork` (API) answer `409 snapshot_data_expired` and the sandbox stays archived |
| saved-snapshots | `snapshot ID NAME`, list/info, own deploy (`--from` + `--host`), share code, deploy by another account with credential scrub (env secret file, `~/.ssh/id_*`, `authorized_keys`, `.git-credentials`, gh hosts, env vars), shares/unshare (revoked code refused), rm |
| environments | env vars, secret file (mode 600), repo from a local bare repo via `git daemon`, repo setup script, local path refused, live `env upgrade` (vars changed, file removed), `--no-env` |
| agent | OpenCode free model prompt (events, conversations), steer + interrupt of a long turn, parallel conversations, events/conversations while stopped |
| ai-proxy | placeholder env in the guest, real key absent from the guest, proxied call reaches the fake vendor with the real key, bad placeholder 401, usage with tokens, block (403) / unblock |
| apikeys | sandbox-scoped key (own sandbox OK, other 403 `api_key_sandbox_forbidden`, missing action 403), CLI with the key, read-only preset, list/usage/rotate/revoke, sandbox token in the in-sandbox CLI |
| orgs | create, invite/join, members, org credits, private org sandbox, share/unshare, personal scope refused, org delete |
| credits | balance, admin grant, `usage`, billing/limits, transfer, node earnings |
| legacy-box | `/api/box/v1/boxes` create/poll/command/delete (confirmation header), deprecation headers |
| sdk | Python and TypeScript SDK: create, wait, command, write/read file, stop and remove |
| cli-fixes | review fixes in the CLI: `push --to /srv/api` (outside `/home/user`, symlink counted), `env new` JSON, `run` summary with credits, `org default-visibility` show/set, `data-retention enable --yes` (fresh account), `skill install` / `skill path` |
| docker-resume | containers that ran (with and without a restart policy) run again after stop + resume; one stopped before the stop stays stopped; a profile-gated Compose service's image is kept; `snapshot pull` writes named volumes to `docker/<vol>/`; Boat's set-hostname.service recipe keeps the hostname |
| disk-cleanup | at 95% disk the guest deletes an unused pulled image but keeps an image built in the sandbox and one a container uses (containerd image store) |
| agent-docs | `/llms.txt`, `/llms-full.txt`, a `/docs/<page>.md` linked from the index, docs MCP server (`initialize`, `tools/list`, `search_docs`), the `crowdvm` skill in the harness skills folders inside a sandbox |
| boat-sdk-samples | every Python and TypeScript sample of Boat's SDK, evals, platform-guide and recipes pages, verbatim apart from the base URL (`sdk/tools/doc_samples.py`; needs `.ref/pages`) |
| otel | OTLP logs for lifecycle events (ready, archived, degraded) and periodic metrics; `telemetry unset` |
| process-history | eBPF output capture: `process` records for `ssh ID -- cmd` output, a pty inside an ssh session, and a background pipe writer the session left running; an `exec`'d command has no `process` record. `CROWDVM_E2E_OVERHEAD=1` also logs the cost of 2M one-byte pipe writes with the tracer on and off |

## Results

**Parallel lanes, 2026-11-12** (same machine, other agents' clusters running too):
13 m 41 s for the whole suite, all groups PASS. The free OpenCode model did not
answer from the guests that day, so `agent` (8 m) and `process-history` (6 m)
each waited out its 6 min timeout and skipped that row; without it the suite
takes about 7–8 min (lane 4: setup 1 m 41 s + 5 m 35 s of groups; lane 1:
setup 33 s + `snapshots` 5 m 53 s). The single-cluster suite took 17 min on
2026-11-11, and would take ~29 min with the same OpenCode timeouts.

Single cluster, earlier:

Latest runs: 2026-11-08 evening, this machine (Ryzen 7 5700X, 16 threads, 62 GB,
KVM, QEMU 11.1.3, image `ubuntu-24.04-crowdvm-v2`), after the fixes for the parity
reviews (`docs/reviews/*.md`). Cluster setup takes 30–45 s; the whole suite about
15 min (the Boat SDK samples take 6–8 min of it).

| Group | Result | Time | Note |
|---|---|---|---|
| nodes | PASS | 20 s | B2 fixed |
| create | PASS | 1 m 36 s | B1 fixed (includes ~60 s for the 45 s TTL auto-stop) |
| exec | PASS | 18 s | |
| files | PASS | 1 s | |
| ssh | PASS | 22 s | |
| host | PASS | 1 s | |
| desktop | PASS | 1 s | |
| network | PASS | 1 s | |
| webhooks | PASS | 0 s | server runs with `--webhook-allow-local` (receivers on 127.0.0.1) |
| lifecycle | PASS | 55–64 s | live fork 4 s |
| snapshots | PASS | 12 s | incl. `snapshot pull <sandboxId>` and `409 snapshot_data_expired` |
| saved-snapshots | PASS | 18 s | |
| environments | PASS | 11 s | |
| agent | PASS | 19–22 s | OpenCode `opencode/beam-free` |
| ai-proxy | PASS | 7 s | |
| apikeys | PASS | 5 s | B4 fixed |
| orgs | PASS | 11 s | B6 fixed |
| credits | PASS | 0 s | B5 fixed |
| legacy-box | PASS | 4 s | |
| sdk | PASS | 13–17 s | |
| cli-fixes | PASS | 9 s | |
| docker-resume | PASS | 45 s | |
| agent-docs | PASS | 0 s | |
| boat-sdk-samples | PASS | 6–8 min | 54/56 pass, 2 expected refusals (see below), 0 failed |
| otel | PASS | 0 s | |

Run history of the evening: run 1 failed webhooks/lifecycle/otel (the new loopback
block on webhook and OTLP URLs; fixed in the harness with `--webhook-allow-local`),
cli-fixes (two wrong assertions in the new test) and boat-sdk-samples (pool too small
for a `large` sandbox and for the recipe's forks; the group now stops the shared
sandbox and runs one fork). Run 2: everything passed except one sample (the fork
placement). Run 3: everything passed except `create` and one sample, both a VM whose
guest agent never became healthy (`boot timeout: guest agent not healthy after 3m0s`,
2 of ~60 boots; see "Flaky" below); a re-run of those groups passed `create` and
`boat-sdk-samples` (56/56, 2 expected).

**Boat SDK samples, live.** The two prompt-and-steer samples (`sdks__python` "Prompt an
integrated agent" and its TypeScript twin) are reported `EXPECTED`: without a real Codex
key the turn fails at once, and Boat answers the steer that follows with
`409 no_running_turn`, as documented. Everything else ran with only the base URL changed.
The same samples also pass against a process-driver server (Python 30/30, TypeScript
26/26) and against the fake server of `sdk/python/tests/test_doc_samples.py` (56/56).

**Boot timeouts (fixed).** About 1 in 30–60 QEMU boots of a create or fork never
became healthy (`boot timeout: guest agent not healthy after 3m0s`). The guest agent
was listening, but systemd-networkd had put the NIC into `failed` and the guest had
no IPv4 address: networkd reads `/etc/machine-id` for its DHCP client ID (DUID-EN)
when it sets up the link, and the agent's create/fork step deleted the file and
re-created it at that same moment of the boot ("Failed to set DUID-EN: No such file
or directory"). The agent now replaces the file with one rename
(`internal/guest/boot.go` `replaceMachineID`). The node logs the serial console tail
on a boot timeout. `bootloop` before the fix: 7 stuck in ~470 creates (v2); after:
302 creates (v2), 120 mixed boots (v3-minimal) and 70 (v3), 0 failed.

The checks of the bugs below say `REGRESSION Bn` in their message.

Observations that are by design (not bugs):

* Stopping a node (SIGTERM) leaves its QEMU processes running (own process
  group); the harness kills them by argv.
* Snapshot browsing starts an inspector VM (`insp_…`) that stays up to 5 min
  after its last use (`InspectIdle`), so it counts toward the machine's VMs.
* Sharing an org sandbox that holds the creator's logins makes it view-only for
  members until a stop + resume (the test covers that path; see B6 for the CLI).
* `env add-repo` refuses local paths and `file://` (`unknown_repository`); the
  guest clones by itself, so a LAN git server needs `net.allow <ip>/32` on the node.

## Product bugs found

All six are fixed (verified in code on 2026-11-08 and by the run below):

| Bug | Fix |
|---|---|
| B1 | the node follows guest events with `?since=` on (re)connect: `internal/node/vms.go:469` |
| B2 | `MaxSandboxes` summed into capacity and free: `internal/server/nodes.go:861,866` |
| B3 | `HostPortResult` has `hostUrl` / `pathUrl`: `internal/client/types.go:447-448` |
| B4 | `finishLogin` accepts a scoped key that cannot read the account: `internal/cli/cmd_auth.go:394-398` |
| B5 | `credits transfer --to <user>` moves credits to that user's wallet: `internal/server/credits.go:244-250` |
| B6 | `crowdvm share` prints the restart note: `internal/cli/cmd_sandbox.go:722` |

The original reports follow.

### B1. `setupStatus` stays `pending` although the setup script ran (create)

* **Repro:** `crowdvm new --setup-file setup.sh` where `setup.sh` finishes fast
  (e.g. `echo ok > ~/setup-done`). Poll `GET /api/v1/sandboxes/{id}`.
* **Observed:** the script ran (`~/setup-done` exists; guest
  `/var/lib/crowdvm/setup.json` says `"setupStatus":"done"` ~80 ms after
  boot), but the API reports `setupStatus: "pending"` forever.
* **Expected:** `setupStatus: "done"` (or `failed` + `setupError`).
* **Cause (suspected):** the guest emits `setup.status` on its event hub, but
  the node subscribes to `GET /v1/internal/events/stream` **without `?since=`**
  (`internal/node/vms.go` `followGuestEvents`, ~line 418), so events emitted
  before the node's stream is connected (or during a reconnect) are dropped;
  the hub only replays when `since` is given (`internal/guest/events.go`
  `subscribe`). The server never re-reads `GET /v1/internal/setup-status`.
  Fix: pass `since` (VM start / last event time) on (re)connect, or have the
  server read setup-status once the guest is ready. Other early guest events
  (`repo.setup`, `prompt.event` right after boot) can be lost the same way.

### B2. `GET /nodes` summary: `capacity.maxSandboxes` / `free.maxSandboxes` always 0 (nodes)

* **Repro:** two online nodes with `--max-sandboxes 2`; `crowdvm nodes --json`.
* **Observed:** `summary.capacity.maxSandboxes = 0`, `summary.free.maxSandboxes = 0`.
* **Expected:** 4 (sum of online nodes), free = sum of per-node free.
* **Suspected file:** `internal/server/nodes.go` (node list handler, ~line 855):
  VCPU/MemoryMiB/DiskGiB are summed, `MaxSandboxes` is not.

### B3 (minor). `crowdvm host --json` drops `hostUrl` / `pathUrl` / `title` / `firewall` (host)

* **Repro:** `crowdvm host ID 8080 --json`.
* **Observed:** `{port,url,isProtected,access}` only; the API response has
  `hostUrl` and `pathUrl` (both routings) but the CLI cannot show the other one
  (`host list` does show them).
* **Suspected file:** `internal/client/types.go` `HostPortResult` (fields missing).

### B4 (minor). `crowdvm login` refuses sandbox-scoped API keys (apikeys)

* **Repro:** `crowdvm api-key create agent --sandbox bx_X --actions sandbox.read,exec`
  (the example in `crowdvm api-key create --help`), then
  `crowdvm login --server URL --key-stdin <<< "$KEY"`.
* **Observed:** exit 1, `403 api_key_action_forbidden: The API key does not allow account.read.`;
  nothing is saved. The same key works through `CROWDVM_API_KEY`.
* **Expected:** the key is saved (login can fall back when `GET /me` is
  forbidden, e.g. show the key's scope instead of the user).
* **Suspected file:** `internal/cli/cmd_auth.go` `finishLogin` (~line 391): any
  `Me()` error aborts the login.

### B5 (minor). `crowdvm credits transfer --to <user>` is documented but refused (credits)

* **Repro:** `crowdvm credits transfer --to bob --amount 2` (bob is another user).
* **Observed:** `403 not_org_member: You are not a member of an organization named bob.`
* **Expected:** per `crowdvm credits transfer --help` ("target wallet: user or org
  id/name") the credits move to bob's wallet, or the help says orgs only.
  Personal → org transfers work.
* **Suspected file:** `internal/server/credits.go` `handleCreditTransfer`
  (`resolveOrgRef` only resolves orgs) or the flag help in `internal/cli/cmd_billing.go`.

### B6 (minor). `crowdvm share` hides that teammates stay view-only until a restart (orgs)

* **Repro:** in an org, `crowdvm new` (default environment: the sandbox holds the
  creator's logins), `crowdvm share ID`, then a member runs `crowdvm exec ID ...`.
* **Observed:** the CLI prints a plain success (`shared`); the member still gets
  `403 sandbox_private` until the creator stops and resumes the sandbox. The API
  answer has `restartRequired: true` and a message saying exactly that, but the
  CLI drops both.
* **Expected:** the CLI shows the message / `restartRequired` (JSON and text).
* **Suspected files:** `internal/client/types.go` `SandboxAction` (no
  `restartRequired`/`message` fields), `internal/cli/cmd_sandbox.go` `simpleAction`.
