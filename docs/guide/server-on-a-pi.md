# Running the server on a Raspberry Pi

`crowdvm-server` is one static Go binary with its database (SQLite) built in, so a
Raspberry Pi 5 can run it for a small team. A fresh server idles at about 30 MB of
memory. Sandboxes run on the nodes, not on the server, so most of the server's work is
bookkeeping. Three things decide how hard the Pi works:

| Load | Why | What to do |
|---|---|---|
| Disk | The server keeps uploaded snapshots and base images, so it can rebuild a sandbox when its node is gone | Put the data directory on an SSD (NVMe HAT or USB 3), never the SD card |
| Network | Snapshots move through the server; by default sandboxes reach the internet through it too | Wired Ethernet; the Pi setup sends sandbox internet traffic out through the nodes (see below) |
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

To build the arm64 image on a faster machine instead: `docker buildx build --platform
linux/arm64 -t crowdvm-server .` (the build cross-compiles, so it runs at native speed).

The compose file caps the container at 1 GB and sets `GOMEMLIMIT=600MiB`, so the Go
runtime collects garbage before memory grows. Admin commands (`user`, `join-token`,
`invite`) run inside the container with `docker compose exec crowdvm-server
crowdvm-server ...`.

## Sandbox internet traffic

A plain `crowdvm-server serve` sends a sandbox's internet traffic out through the server,
so it never shows a node owner's address and node owners do not see it
([Security model](security.md#the-sandboxs-network)). On a Pi that traffic would be the
largest load, so the Docker setup turns it around by default: `CROWDVM_NODE_EGRESS=1`
sends it out through each sandbox's node. Set `CROWDVM_NODE_EGRESS=0` in `.env` (then
`docker compose up -d`) to route it through the server again, for example when the
nodes belong to people who should not carry each other's traffic. Outside Docker the
same setting is `crowdvm-server serve --node-egress` or the environment variable.

## How the scheduler spreads sandboxes

Placement is computed from what the server already holds in memory, so it costs the Pi
almost nothing. Every heartbeat carries the node machine's live load: CPU busy, memory
available and, on Linux, pressure stall information (PSI). The scheduler then:

1. keeps the hard limits: a sandbox only goes where its size fits what the owner
   shares, and placement rules (`--gpu`, `--min-tier`, labels, trust) still apply;
2. skips a host that is short of memory (its owner's programs use it);
3. prefers cool hosts: a host whose CPU is busy, or whose tasks stall on CPU or memory,
   gets a sandbox only when no other node fits;
4. among the rest, picks the one with the most room left on its tightest resource,
   counting both booked sizes and real use, plus a bonus for faster nodes, for a node
   that already holds the sandbox's snapshot (no download from the server) and for
   one that has its base image.

Host CPU is smoothed: it counts at once when it rises, and only slowly when it falls, so
a short quiet moment does not draw new sandboxes onto a busy machine. `crowdvm nodes
info <node>` shows the load the scheduler sees (`load`), and `--log-level debug` logs
every placement decision. Nodes older than this feature send no load; the scheduler
places on them by booked sizes alone.
