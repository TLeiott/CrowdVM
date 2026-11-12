# Security model

CrowdVM runs other people's code on machines that belong to your colleagues. Both
sides take a risk, and the protections differ for each. This page says what CrowdVM
protects and what it does not.

## For people who use sandboxes

### What a sandbox isolates

Each sandbox is its own VM with its own kernel, disk and network stack. Sandboxes do
not share files or processes, and one sandbox cannot reach another one over the
network. Inside the sandbox you are root (`sudo` needs no password), so the boundary
is between sandboxes and between accounts, not between processes in one sandbox.

### What the node owner can and cannot see

A sandbox runs on someone's machine. CrowdVM keeps the node out of the sandbox as far
as software on ordinary PCs can:

| The node owner | Today |
|---|---|
| reads your configuration (environment variables, secret files, SSH keys, registry and AI logins) | **no.** The node gets a boot stub; the VM fetches its configuration from the server over TLS pinned to the server's key |
| runs commands in the sandbox, reads or writes its files, opens tunnels to it | **no.** The sandbox's API is served only on its channel to the server; the node may only check its health and pause or stop it |
| sees what the sandbox does on the network | **no.** TCP and DNS go through the channel and leave from the server. The node sees one encrypted connection, its timing and volume |
| reads the sandbox's memory | not with normal tools: no core dumps, no debugger access (Linux `ptrace` restrictions), and under the node's systemd service the VM's control and network sockets exist only as file descriptors kept by systemd, with no path anyone could connect to (a node started by hand keeps them as files in its data directory). **Yes as root** |
| reads the sandbox's disk and snapshots | **no** (images from v4 on): everything the sandbox writes lands on an encrypted partition (LUKS2, AES-XTS). Its key comes from the server over the channel at boot and never passes through the node. The node and the server's snapshot store hold only ciphertext and the public base image |
| changes CrowdVM or the VM image to get in | **yes**: whoever has root on a machine controls the VMs on it |

The last row is the limit. A node owner who wants in has to modify their CrowdVM
installation or read the running VM's memory as root, which is a deliberate act, not a
`cat`. Only confidential-computing hardware (AMD SEV-SNP, Intel TDX) closes it;
CrowdVM does not use it yet. What CrowdVM does is make the attempts visible:

- Every boot of a sandbox has a one-time ticket, which the node passes to the VM. When
  a ticket is presented a second time, the sandbox's owner gets a notice ("Possible
  tampering on a node").
- When a sandbox reboots, it tells the server first, and its node gets a new ticket. A
  sandbox that goes down without that (a kernel panic, `sysrq`, or a node that kills it
  to get a ticket of its own) stays down, and its owner gets a notice ("Stopped after an
  unexpected restart"). `crowdvm recover` starts it again with its disk as it was (`crowdvm resume` would go back to its last snapshot).

Sandboxes from images before v4 (`ubuntu-24.04-crowdvm-v3` and older) keep an
unencrypted disk. A snapshot of an encrypted sandbox can be resumed or forked only on
nodes recent enough to fetch its key, and the server cannot list or download its files
while it is stopped (`crowdvm snapshot tree` and `crowdvm snapshot pull` answer
`snapshot_encrypted`: resume the sandbox first).

- Only put secrets into a sandbox when you trust the owners of the nodes it can land on.
- Pin sensitive work to nodes you trust: `crowdvm new --node <name>` or
  `--node-label owner=alice` (node owners set labels with `crowdvm nodes update`).
- `crowdvm nodes` and the dashboard's Nodes page show who owns each node.
- `crowdvm new --no-env` starts a sandbox with none of your secrets.
- AI credentials can go through the server's AI proxy (environment setting "Proxy AI
  credentials"), so the provider key never enters the VM. The sandbox gets a
  placeholder that only works through the proxy.

### The sandbox's network

A sandbox reaches the internet through the server, not through the node that hosts it:

- TCP connections and DNS lookups go through the sandbox's encrypted channel and leave
  from the server's network, so they show the server's address, not the node owner's.
- The server refuses connections to private (LAN), loopback, link-local, CGNAT and
  Tailscale addresses and cloud metadata services. The server's own API stays
  reachable, and the server operator can open more on the server's networks, such
  as an internal git server: `crowdvm-server serve --egress-allow 10.0.5.0/24,192.168.1.20:22`.
- UDP other than DNS (QUIC, for example) is refused, and `ping` does not work.
  Programs fall back to TCP.
- If root in the sandbox removes CrowdVM's firewall rules, the sandbox only cuts itself
  off: the node lets it reach nothing but the server.
- Direct ports (`direct-host` nodes) enter the sandbox through the node, which sees
  that traffic.

The server operator can turn this off with `crowdvm-server serve --node-egress`; the
sandboxes then reach the internet through their nodes, as described in
[For node owners](#network).

### What a sandbox holds

| Credential | Where | Removed by |
|---|---|---|
| Sandbox token (`CROWDVM_TOKEN`, `ASCII_TOKEN`) | environment | nothing; it can only read this sandbox and host its ports |
| Your API key (`CROWDVM_API_KEY`), if the environment passes it | environment | `--no-env`, environment setting |
| Environment variables and secret files | environment, files | `--no-env` |
| Git host tokens, registry logins | git, `gh` and Docker config in the home directory | `--no-env` |
| Claude, Codex and other AI logins | `~/.claude`, `~/.codex`, … | `--no-env`, AI proxy |

### Who can reach a sandbox

- You, with your session or an API key whose scope covers the sandbox.
- Members of your organization, when the sandbox is shared (`crowdvm share ID`).
- Anyone holding a token URL:

| URL | Valid for |
|---|---|
| Private public URL (`?_token=`) | while the port is hosted; hiding and hosting it again keeps the token |
| `--public` public URL | anyone who can reach the server |
| Desktop link (`crowdvm desktop`) | 10 minutes to connect; an open stream keeps running; ends when the sandbox stops |
| Public desktop (`crowdvm desktop --public`) | until the sandbox stops |
| Signed file URL (`files/download-url`, `files/upload-url`) | 1 second to 1 hour, as requested |

### One sandbox per end user

If your product runs code on behalf of your own users (an agent per customer, a
playground, a grading service), give **each end user their own sandbox** and never
reuse it for someone else. CrowdVM does not clean a sandbox between users: files,
shell history, browser profiles, Docker images and running processes stay. A previous
user can also leave something behind on purpose.

A pattern that works:

1. Prepare one sandbox with your stack and save it: `crowdvm snapshot ID base-stack`.
2. Per end user: `crowdvm new --from base-stack --no-env`, with a per-sandbox API key
   (`crowdvm api-key create user-42 --sandbox <id> --preset full-sandbox`) if your backend calls
   the API for that user.
3. When the user is done: `crowdvm delete <id> --yes`. Do not stop and resume it for
   the next user.

Treat everything that comes out of such a sandbox (files, command output, URLs) as
untrusted input.

### What the command history keeps

Process history records what processes in ssh sessions and in agent runs write to a
terminal, a pipe or a socket, as Boat does. That can include text a program sends to a
local service, such as the Docker CLI's requests to the Docker daemon. CrowdVM blanks
the values of credential headers (`Authorization`, `X-Registry-Auth`, `X-Api-Key`,
`Cookie`) before they are stored, but other secrets a program prints or sends stay in
the history like any other output. Create the sandbox with `--no-monitoring` to keep no
history at all.

### Your account

- A password is 8 to 72 bytes long. A longer one is refused with 400.
- Changing a password (`POST /auth/password`), or having an admin reset it, signs the
  account out everywhere else. The session that changed it stays signed in.
- An email address counts only after you prove it. Setting one with `PATCH /auth/me`
  (or giving one at signup) mails a 6-digit code to that address, and the address is
  stored once you send the code to `POST /auth/me/email/verify` with
  `{"email": "...", "code": "123456"}`. Until then the account has no such email, so it
  cannot be used for email login or for approving an agent that names that address.
  Emails an administrator sets are stored as given.
- Usernames cannot start with `usr_`, which user ids use.

## For node owners

A node runs VMs for everyone on the server. CrowdVM limits what those VMs can do to
your machine:

- **Hardware virtualization.** Each sandbox is a QEMU VM (KVM on Linux, Windows
  Hypervisor Platform on Windows). A guest reaches your machine only through a bug in
  QEMU or the hypervisor. Keep both up to date.
- **No access to your files.** The VM sees its own disk and a small read-only config
  drive, nothing else.
- **Resource caps.** VMs get only the CPUs, RAM and disk you share
  (see [Node owner guide](node-owners.md#capacity)). The node keeps 10 GiB of disk free
  by default and refuses new VMs before it runs out.
- **Network isolation.** See below.

The `process` driver (`--driver process`) runs sandboxes as plain processes on your
machine without any isolation. It exists for tests. Never use it on a node that others
can schedule on.

### Network

By default a sandbox's internet traffic does not leave from your machine: it goes
through an encrypted connection to the CrowdVM server and out from there (see
[The sandbox's network](#the-sandboxs-network)). Your node lets such a VM reach only
the server, and its DNS resolver does not answer it.

When the server runs with `--node-egress`, or for sandboxes on older nodes, the VM uses
a private network stack inside the node process, and outbound traffic goes through
ordinary sockets of your machine after an egress check:

| Destination | Default |
|---|---|
| The internet | allowed |
| The CrowdVM server (its host and port) | allowed |
| This machine (loopback and its own addresses) | blocked |
| LAN ranges (10/8, 172.16/12, 192.168/16) | blocked |
| Tailscale and CGNAT (100.64/10) | blocked |
| Link-local, incl. cloud metadata (169.254.169.254) | blocked |
| Azure WireServer (168.63.129.16) | blocked |
| Multicast, IPv6 ULA and link-local | blocked |

Nothing can connect into a VM from outside: SSH, public URLs and the desktop all go
through the server and the node's tunnel. Change the policy with `crowdvm-node config
set` (see [Node owner guide](node-owners.md#network-policy)). `--net slirp` turns the
isolation off and is only a fallback for broken setups.

In that mode traffic leaves from your IP address, and anything a sandbox does on the
internet looks like it came from your network.

### What your machine keeps of a sandbox

- **Not stored:** the sandbox's configuration and secrets, its boot ticket (it stays in
  the node process's memory), and any token that opens its API. The node keeps an
  operations token that only checks the VM's health and pauses or stops it.
- **Stored:** the VM's disk and snapshot layers, which hold the public base image and
  the sandbox's writes as ciphertext (images v4 and later; older images write in the
  clear), and its serial console (trimmed to the last 256 KiB at each start). CrowdVM's processes write no core dumps, so a crashed VM
  leaves none of its memory on your disk. Swap and hibernation can still write VM
  memory to disk; use encrypted swap if that matters to you.

### Taking your machine back

See [Node owner guide](node-owners.md#taking-the-machine-back).

- `crowdvm-node config set paused true` stops new sandboxes from landing on it at once;
  running ones keep running.
- `crowdvm nodes drain <node>` does the same from any machine.
- `crowdvm nodes evacuate <node>` gets the machine back without losing anyone's disk:
  after a grace period (10 minutes by default) its sandboxes are stopped, their disks
  saved, and resumed on other nodes.
- Stopping the node daemon takes its sandboxes offline. After 3 minutes the server
  rebuilds them on other nodes from their latest snapshots. A `stop` of such a sandbox
  waits those 3 minutes for the node, so a node that comes back in time saves its
  newest disk; `stop --force` keeps the last stored snapshot at once.

## For the server operator

- The server stores secrets encrypted with keys in its data directory: environment
  variables and secret files with `env-secrets.key`; AI credentials, telemetry headers,
  webhook secrets, each boot's guest configuration and **the disk keys of encrypted
  sandboxes** with `secret.key`. Back up the whole data directory (the keys,
  `guest-tls.pem`, the database and the blobs) while the server is stopped, and keep
  the keys as private as the database. Without `secret.key` every encrypted sandbox's
  data is lost for good; the server refuses to start a new key while such sandboxes
  exist. `guest-tls.pem` is the key sandboxes pin: a new one makes running sandboxes
  unreachable until each is recovered (`crowdvm recover`).
- Sandboxes reach the internet **through the server** by default: all their downloads
  use the server's uplink and show its address, and a server restart cuts their open
  connections. `--node-egress` sends it out through the nodes again (the node then sees
  the sandbox's traffic). `--egress-allow` opens hosts on the server's own networks.
- Sandboxes connect to the server with an HTTP upgrade (`Upgrade: crowdvm-guest`). A
  reverse proxy in front of the server must pass it on, as for WebSockets (Caddy and
  nginx with `proxy_set_header Upgrade $http_upgrade` do; Cloudflare's proxy does not).
  Nodes give their sandboxes the server URL they use themselves, so a node that joined
  through the LAN keeps working when the public URL sits behind a proxy.
- Upgrading: `crowdvm-node update` also updates the node's systemd unit, so systemd
  keeps its VMs' control sockets (systemd 254 or later). Once a sandbox on image v4 exists, do
  not go back to a server or node release without the secure layer: its disk can only
  be unlocked through the guest channel.
- Without `--tls-cert`/`--tls-key` the API is plain HTTP. API keys and session cookies
  cross the network unencrypted, so run the server on a network you trust, or put it
  on a tailnet, which encrypts traffic between machines.
- Sign-up is closed by default. Node join tokens are admin-only unless `--open-nodes`.
