# Running the server on a Raspberry Pi

`crowdvm-server` is one static Go binary with its database (SQLite) built in, so a
Raspberry Pi 5 can run it for a small team. A fresh server idles at about 30 MB of
memory. Sandboxes run on the nodes, not on the server, so most of the server's work is
bookkeeping. Three things decide how hard the Pi works:

| Load | Why | What to do |
|---|---|---|
| Disk | The server keeps uploaded snapshots and base images, so it can rebuild a sandbox when its node is gone | Put the data directory on an SSD (NVMe HAT or USB 3), never the SD card |
| Network | Snapshots move through the server; by default sandboxes reach the internet through it too | Wired Ethernet. If it is not enough, send sandbox internet traffic out through the nodes (see below) |
| CPU | TLS, the dashboard and heartbeats (every node, every 10 s) | Nothing to do: a Pi 5 has hardware AES |

## With Docker

The repository has a `Dockerfile` (amd64 and arm64) and a `compose.yaml`:

```sh
git clone https://github.com/TLeiott/CrowdVM && cd CrowdVM
sudo mkdir -p /mnt/ssd/crowdvm && sudo chown 65532:65532 /mnt/ssd/crowdvm   # the image runs as uid 65532
cat > .env <<'ENV'
CROWDVM_PUBLIC_URL=http://192.168.1.10:8420
CROWDVM_DATA=/mnt/ssd/crowdvm
CROWDVM_NODE_EGRESS=1
ENV
docker compose up -d
docker compose exec crowdvm-server crowdvm-server user create --username admin --password '...' --admin
```

Compose reads `.env` for every command, `exec` included. Without `CROWDVM_DATA` the data
goes to a named volume in Docker's data root, so that root should be on the SSD as well.
`CROWDVM_NODE_EGRESS=1` is a choice, not a requirement; read
[Sandbox internet traffic](#sandbox-internet-traffic) before you keep it.

The container uses the host's network (`network_mode: host`): the server listens on the
Pi's port 8420 directly, sees real client addresses for its rate limits and public URL
rules, and knows the Pi's own networks when it checks sandbox traffic.

To build the arm64 image on a faster machine instead: `docker buildx build --platform
linux/arm64 -t crowdvm-server .` (the build cross-compiles, so it runs at native speed).

The compose file caps the container at 1 GB and sets `GOMEMLIMIT=600MiB`, so the Go
runtime collects garbage before memory grows. (If `docker info` warns "No memory limit
support", add `cgroup_enable=memory` to `/boot/firmware/cmdline.txt` and reboot;
`GOMEMLIMIT` works either way.) `docker compose stop` gives the server 30 s to finish
requests and write its usage records. Admin commands (`user`, `join-token`,
`invite`) run inside the container with `docker compose exec crowdvm-server
crowdvm-server ...`.

## Binaries for nodes

New nodes install `crowdvm-node` from the server (`curl http://<server>:8420/install | sh
-s -- --node`), and nodes update from it, so the server needs the release binaries. Build
them on any machine with Go (`make dist` writes `dist/`; on the Pi it takes a while) and
import them next to the compose file:

```sh
make dist
docker compose run --rm -v "$PWD/dist:/dist:ro" crowdvm-server downloads import /dist
docker compose restart
```

Repeat this for every release you roll out ([Updates and compatibility](updates.md#server-operators)).

## Sandbox internet traffic

By default a sandbox's internet traffic leaves through the server: it never shows a node
owner's address, and node owners do not see it ([Security model](security.md#the-sandboxs-network)).
On a Pi that traffic is usually the largest load. `CROWDVM_NODE_EGRESS=1` in `.env` (as
in the setup above) sends it out through each sandbox's node instead. The Pi then carries
none of it, but each node owner sees the traffic of the sandboxes on their machine
(destinations, DNS lookups, anything unencrypted), and it leaves from their address. Use
it when your node owners trust each other and the people who use the sandboxes.
`CROWDVM_NODE_EGRESS=0` (the compose default) keeps it on the server. After a change,
`docker compose up -d`; running sandboxes keep their old route until they start again.
Outside Docker the same setting is `crowdvm-server serve --node-egress`.

## How the scheduler spreads sandboxes

Placement is computed from what the server already holds in memory, so it costs the Pi
almost nothing. Every heartbeat carries the node machine's live load: CPU busy, memory
available and, on Linux, pressure stall information (PSI). The scheduler then:

1. keeps the hard limits: a sandbox only goes where its size fits what the owner
   shares, and placement rules (`--gpu`, `--min-tier`, labels, trust) still apply;
2. skips a host that is short of memory (its owner's programs use it);
3. prefers cool hosts: a host whose CPU is busy, or whose tasks stall on CPU or memory,
   gets a sandbox only when no other node fits, unless it holds the sandbox's newest
   snapshot (resuming there beats moving the disk through the server);
4. among the rest, picks the one with the most room left on its tightest resource,
   counting both booked sizes and real use, plus a bonus for faster nodes, for a node
   that already holds the sandbox's snapshot (no download from the server) and for
   one that has its base image.

Host CPU is smoothed: it counts at once when it rises, and only slowly when it falls, so
a short quiet moment does not draw new sandboxes onto a busy machine. `crowdvm nodes
info <node>` shows the load the scheduler sees (`load`), and `--log-level debug` logs
every placement decision. Nodes older than this feature send no load; the scheduler
places on them by booked sizes alone.
