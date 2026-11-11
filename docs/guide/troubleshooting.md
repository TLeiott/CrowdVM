# Troubleshooting

Start with these three commands. They answer most questions:

```sh
crowdvm status          # which server, which account, which config file
crowdvm notices         # what the server wants you to know: low credits, stopped or failing sandboxes, full disks
crowdvm nodes           # which nodes are online, how full they are
```

Every API error has a `code`. The CLI prints it, and `--json` puts it in the last line.

## Sandboxes

**`crowdvm new` waits and nothing happens.** No online node has room for the size
you asked for. Without `--fail-fast` the server waits up to 15 minutes for capacity
(`waitingForCapacity: true` in `crowdvm info`). Try a smaller `--type`, drop
placement flags (`--gpu`, `--min-tier`, `--node`), or stop sandboxes you no longer
need. `crowdvm nodes` shows free capacity.

**The sandbox is `degraded`.** The node stopped hearing from the VM (three missed
health checks), or the node itself answers slowly. It often recovers by itself. If not:
`crowdvm recover ID` power-cycles it.

**The sandbox is `error` after its node went away.** The server waits 3 minutes for
the node, then rebuilds the sandbox on another node from its latest snapshot. If no
other node has the snapshot chain or room, it stays in `error`; `crowdvm resume ID`
tries again.

| Code | Meaning | Fix |
|---|---|---|
| `sandbox_starting` | Still booting | Retry in a few seconds |
| `sandbox_securing` | The server removes secrets before handing the sandbox over (`--no-env`, a saved snapshot from someone else) | Retry; after 3 failures the sandbox goes to `error` |
| `machine_not_running` | Stopped | `crowdvm resume ID` |
| `snapshot_data_expired` | The stopped sandbox's snapshots were deleted (or expired), so there is no disk to resume or fork | Deploy a saved snapshot or create a new sandbox |
| `resume_failed` | The sandbox is in `error` and never completed a snapshot; a resume would boot an empty disk | `crowdvm recover ID` |
| `machine_class_plan_required` (403) | An admin's size policy does not allow this `--type` | Pick a size from `crowdvm limits`, or ask an admin |
| `org_suspended` (403) | An admin suspended the organization; it cannot start sandboxes | Ask an admin; personal sandboxes still work |
| `no_ready_machine` | No capacity and `--fail-fast` was set | See above |
| `billing_required` (402) | The wallet is out of credits and the server enforces credits | Ask an admin for credits |
| `agent_outdated` | The node boots an old guest agent | Stop and resume the sandbox; the node owner updates `crowdvm-node` |
| `provider_not_configured` | `crowdvm prompt` for a harness without credentials | Add a key or sign-in under AI credentials, or `crowdvm ai` |
| `rate_limited` (429) | Too many requests or starts | Wait `retryAfterSeconds` |

## Connecting

**`crowdvm ssh` hangs or fails.** It runs OpenSSH through `crowdvm tunnel`, which goes
through the server. Check that `ssh` is installed and that `crowdvm exec ID -- true`
works. `crowdvm ssh-config ID` writes an entry for VS Code and `rsync`.

**The desktop says "Could not connect".** Desktop links work for 10 minutes and only
while the sandbox runs. Open the desktop again from the dashboard, or get a new link
with `crowdvm desktop ID`. A desktop that is still starting shows "Starting the
desktop session…" for up to a minute.

## Public URLs

| Symptom | Cause | Fix |
|---|---|---|
| 502 "Nothing answered on port N" | The app is not running, or listens on another port | Start it; `crowdvm ps ID` |
| "Blocked request. This host is not allowed." | Vite rejects the Host header | Add the domain to `server.allowedHosts`, see [Public URLs](public-urls.md#dev-servers-that-check-the-host-header) |
| The `nip.io` name does not resolve | No public DNS, or the router blocks answers with private IPs | Use `--public-domain` with your own wildcard DNS, or the `/p/<id>/<port>/` URL |
| 401 on a private URL | The token is missing or the cookie expired | Open the full URL again: `crowdvm host url ID PORT` |
| 404 on a host-routed URL | The port is not hosted, or the label is wrong | `crowdvm host list ID` |

## Nodes

**The node does not show up or is offline.** Run `crowdvm-node status`. The node dials
the server URL from its config (`crowdvm-node config get server`); that address must
be reachable from the node, and it must match the server's `--public-url` host for
sandboxes to reach the server. Check the log: `journalctl --user -u crowdvm-node -f` on
Linux, `node.log` in the data directory on Windows.

**`/dev/kvm` permission denied.** `sudo usermod -aG kvm $USER`, log out and in again,
then restart the node. Without KVM the node runs VMs in software and is ranked bronze.

**Windows VMs are very slow.** Turn on "Windows Hypervisor Platform" in Windows
features and reboot. `crowdvm-node bench` then reports `whpx` as the accelerator.

**No base image.** A node without the image downloads it from the server before the
first boot. If the server has none either, build it on one node and push it
(`crowdvm-node image build`, then `image push`); see
[Getting started](getting-started.md#3-add-a-node-on-linux).

**The node refuses sandboxes although it looks idle.** It keeps `disk-reserve` (10G)
free and refuses new VMs when less than 2 GiB of headroom is left. Free disk space or
lower `disk`/`disk-reserve`. A node with `paused` set or drained by an admin takes no
new sandboxes either.

**A sandbox cannot reach a service on my LAN.** That is the default egress policy.
The node owner can open single addresses with `crowdvm-node config set net.allow`
([Node owner guide](node-owners.md#network-policy)).

## The CLI

**`not_signed_in`.** `crowdvm login`, or set `CROWDVM_API_BASE` and `CROWDVM_API_KEY`.

**Several servers or accounts.** Use profiles: `crowdvm login --profile work`, then
`--profile work` or `CROWDVM_PROFILE=work` per command.

**"A newer crowdvm is available".** Run `crowdvm self-update`. It installs the
server's build only when it is newer than yours. `--no-update` silences the check.
