# Updates and compatibility

CrowdVM has three kinds of programs that update separately: the server, the nodes
and the clients (the `crowdvm` CLI and the SDKs). This page says how each one learns
about an update, how to install it, and what the server does with a node or client
that is too old.

## Generations

Every build carries a compatibility **generation**, a number compiled in from
`internal/compat` in the source: one for nodes and one for the API. Release versions
(`0.2.0`, `0.2.0-nightly.20261112.1`) only say that a newer build exists. The
generation says whether an older build still works:

| A peer whose generation is... | is | What happens |
|---|---|---|
| at or above *Required* | current | nothing (a newer release may still be offered) |
| below *Required* | outdated | it keeps working for a grace period, and is told to update |
| below *Min* | incompatible | the server refuses it with `426 Upgrade Required` |

A breaking change or a critical (security) fix raises the generation in the same
commit. It is first only *Required*; a later release may raise *Min* to it. So
upgrading the server never refuses nodes or clients at once. And while no online node
is current, outdated nodes keep taking sandboxes and their grace period does not
start: the pool keeps working until its first node is updated.

Upgrade in this order: the server (with its downloads, see
[Server operators](#server-operators)), then the nodes, then the CLIs.

### The first rollout: nodes from before generations

Nodes built before generations existed send none, so the server counts them as
generation 0 and outdated, even those that already have the secure layer. They have no
`crowdvm-node update` and no auto-update: install them again, once, on each machine:

```sh
curl -fsSL http://<server>:8420/install | sh -s -- --node
crowdvm-node install-service
```

Their running sandboxes survive this like any node restart. For this one rollout,
reinstall the nodes before you upgrade the server: a new node works with the old
server, and the pool never has an outdated phase. If the server goes first, nothing
stops (see above), and the server, the dashboard and the owners' notices show the
command. From then on, nodes update themselves.

## Nodes

The server judges each node by the generation it sends when it connects:

- **Update available**: a newer release is in the server's downloads.
  `crowdvm-node status`, the node's local page and the dashboard say so. Nothing else
  changes.
- **Outdated** (misses a required update): the node gets no new sandboxes at once,
  and updates itself if it can (see below). If it is still outdated 10 minutes later,
  its owner gets a notice (and an email when one is set up), and so do the owners of
  the sandboxes on it. Sandboxes still running there keep running until the grace
  period ends (`--node-update-grace`, 72 hours by
  default). Then they are stopped with their disks saved and resumed on other nodes,
  like an [evacuation](node-owners.md#taking-the-machine-back). Updating the node ends
  all of this; the evacuation cannot be cancelled otherwise.
- **Incompatible**: the server refuses the connection. The node's status shows the
  reason and the command. Its sandboxes are rebuilt elsewhere like those of any node
  that went offline.

### Updating a node

```sh
crowdvm-node update            # install the server's newer build, then restart the service
crowdvm-node update --check    # only say whether there is one (exit 1 if so)
crowdvm-node update --rollback # go back to the build before the last update
```

The update downloads `crowdvm-node` and the guest agent from the server's
`/downloads`, checks each against the sha256 the server lists (this proves the
download is complete, not who built it: the node trusts its server), keeps the old files
as `*.old`, refreshes the systemd unit if the new build needs a different one, and
restarts the service. Running sandboxes survive the restart. They get the new guest
agent at their next stop and resume.

By default a node updates itself when the server says it misses a required update,
so it never loses its sandboxes to the grace period
(`crowdvm-node config set auto-update required`). `always` also installs optional
updates. `off` leaves every update to you.

## CLI and SDKs

The CLI checks the server's release at most every 6 hours and prints one line when a
newer `crowdvm` is available. `crowdvm self-update` installs it (an npm install says
which `npm i -g` command to run instead).

The CLI and SDKs also send their generation with every request:

- **Outdated**: the server adds `X-Crowdvm-Client-Status: outdated; latest=<version>`.
  The CLI prints a line after each command (only on a terminal, never with `--json`).
  The SDKs warn once per process.
- **Incompatible**: the server answers `426 client_outdated`. The CLI prints the
  update command.

Other HTTP clients (curl, browsers) send no generation and are never refused.

## Server operators

- `crowdvm-server downloads import <dist dir>` publishes a release to the nodes and
  CLIs, together with its `manifest.json`. The version in the manifest is what
  "update available" compares against. When a release raises a required generation,
  import it and restart the server with its new binary in one step: nodes that are
  told to update must find the new build in the downloads.
- `--node-update-grace 72h` sets how long an outdated node keeps its sandboxes.
- `GET /api/v1/server` reports the server's `gen`, the API generations it accepts
  (`apiGen`, `requiredApiGen`, `minApiGen`) and the `latest` release.
- The dashboard's Nodes page marks nodes that need an update.
- Going back to an older server after this one ran: the older binary would take the
  outdated nodes' evacuations for ones their owners started, and run them. Remove them
  first: `sqlite3 <data dir>/crowdvm.db "DELETE FROM node_evacuations WHERE reason='outdated'"`.
