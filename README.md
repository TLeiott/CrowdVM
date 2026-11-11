# CrowdVM

Crowdsourced cloud VMs for coding agents, on your own machines.

CrowdVM is a self-hosted, open-source take on [Boat](https://boat.dev): persistent
Ubuntu VMs with Docker, a desktop and coding agents, driven by a CLI, an HTTP API
and SDKs. Instead of a cloud, the VMs run on **nodes**: the Linux and Windows
machines on your LAN or Tailnet that have the CrowdVM node app installed. Each
node benchmarks itself, shares part of its CPU, memory and disk, and hosts
sandboxes for everyone on the server. Your agents get more compute than the box
they run on.

```
  agents / you ──crowdvm CLI, SDKs, dashboard──▶ crowdvm-server ◀──── crowdvm-node (PC #1, Linux)
                                                  (coordinator) ◀──── crowdvm-node (PC #2, Windows)
                                                                ◀──── crowdvm-node (laptop, ...)
                                                     each node runs sandboxes as QEMU/KVM VMs
```

The API mirrors Boat's v1 API (`/api/v1`) and the `crowdvm` CLI mirrors `boat`,
so tools written for Boat need little more than a new base URL. Scope today:
LAN and Tailscale. Public-internet hosting is not a goal yet.

## What you get

- **Sandboxes**: full Ubuntu 24.04 VMs (small / default / large / xlarge) with
  systemd, Docker, Chrome, VS Code, Rust, Node.js, Bun, Python, gh and Claude
  Code, Codex and OpenCode preinstalled. Boot to ready takes about 3 s with KVM.
- **Lifecycle**: create, stop (disk saved and the node freed), resume on any node,
  live fork of a running sandbox, recover, auto-stop TTL, always-on, saved
  snapshots with share codes, and snapshot file browsing while stopped.
- **Run work**: sync, streaming and detached commands with attach, command
  history, a files API, SSH/SCP/port forwarding through the server, public URLs
  for ports, and a virtual desktop in the browser.
- **Integrated agents**: prompt Claude Code, Codex, OpenCode and others inside a
  sandbox, read their events, steer and interrupt them, and run many
  conversations in parallel. An AI credentials proxy keeps provider keys out of
  the VM.
- **Teams**: accounts, API keys with scopes, organizations, environments
  (variables, secret files, repos, registry logins, setup scripts), webhooks,
  OpenTelemetry export, data retention, and CI runners.
- **Crowd**: node benchmark and categorization (CPU, memory, disk, network, GPU,
  virtualization → score and tier), capacity limits set by each owner, a
  scheduler with placement rules (`gpu`, `nested`, labels, minimum tier), and a
  credits ledger in place of billing: users spend credits, node owners earn them.
- **Isolation for contributors**: every VM runs behind a userspace network stack
  with an egress policy. Guests reach the internet and the CrowdVM server, but
  not the host, its LAN or its tailnet.

The [user guide](docs/guide/README.md) covers setup, the CLI for agents, public URLs,
the security model and running a node. `docs/PARITY.md` maps every Boat feature to
its CrowdVM status.

## Quick start (one machine)

Requirements: Go 1.27+, QEMU (`qemu-system-x86_64`, `qemu-img`), and `/dev/kvm`
on Linux or the Windows Hypervisor Platform on Windows.

```sh
make build                                   # bin/crowdvm, crowdvm-server, crowdvm-node, crowdvm-guest

# 1. server
bin/crowdvm-server serve --listen :8420 &
bin/crowdvm-server user create --username me --password '<password>' --admin

# 2. a node (repeat on every machine that should share compute)
bin/crowdvm-server join-token create          # prints a cvj_... token
bin/crowdvm-node join --server http://<server-ip>:8420 --token cvj_...
bin/crowdvm-node image build --cloud-image noble-server-cloudimg-amd64.img   # or: image pull
bin/crowdvm-node run                          # or: crowdvm-node install-service

# 3. use it
bin/crowdvm login --server http://<server-ip>:8420
id=$(bin/crowdvm new --json | jq -r 'select(.event=="ready") | .id')
bin/crowdvm exec "$id" -- uname -a
bin/crowdvm ssh "$id"
bin/crowdvm stop "$id"
```

Other machines install the CLI or the node app from the server:

```sh
curl -fsSL http://<server-ip>:8420/install | sh             # CLI
curl -fsSL http://<server-ip>:8420/install | sh -s -- --node # CLI + node app
irm http://<server-ip>:8420/install.ps1 | iex               # Windows
```

The dashboard is at `http://<server-ip>:8420/`; its API explorer (`/api-explorer`)
lists and runs every API route. For coding agents,
`crowdvm agent-instructions` prints a short guide to paste into `AGENTS.md`.

`scripts/dev-cluster.sh up --nodes 2` starts a local server with two nodes for
development.

## Repository layout

| Path | What |
|---|---|
| `cmd/crowdvm`, `internal/cli`, `internal/client` | CLI and Go client |
| `cmd/crowdvm-server`, `internal/server` | coordinator: API, accounts, scheduler, snapshots, features |
| `cmd/crowdvm-node`, `internal/node` | node app: benchmark, VM drivers (QEMU, process), userspace network, images |
| `cmd/crowdvm-guest`, `internal/guest` | agent inside each VM: commands, files, desktop, integrated agents |
| `web/` | dashboard (embedded in the server) |
| `sdk/python`, `sdk/typescript` | SDKs |
| `images/` | base image provisioning |
| `docs/` | user guide (`docs/guide`), architecture, Boat parity checklist, gap audits |

## Development

```sh
make test                      # go vet + unit tests (no root, no KVM needed)
make e2e                       # real-VM end-to-end tests (needs KVM, CROWDVM_E2E=1)
(cd sdk/python && python3 -m unittest)
(cd sdk/typescript && npm install && npm test)
```

Read `docs/ARCHITECTURE.md` before changing the wire contracts in
`internal/proto` and `internal/api`.


## License

Proprietary, all rights reserved. Viewing only: no use, copying, modification,
distribution, commercial use or AI training without written permission. See
[LICENSE](LICENSE).
