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

### The node owner can read your sandbox

A sandbox runs on someone's machine. Whoever controls that machine can read the VM's
disk and memory, including your code, environment variables and tokens. CrowdVM cannot
prevent this; it is how crowdsourced compute works.

- Only put secrets into a sandbox when you trust the owners of the nodes it can land on.
- Pin sensitive work to nodes you trust: `crowdvm new --node <name>` or
  `--node-label owner=alice` (node owners set labels with `crowdvm nodes update`).
- `crowdvm nodes` and the dashboard's Nodes page show who owns each node.
- `crowdvm new --no-env` starts a sandbox with none of your secrets.
- AI credentials can go through the server's AI proxy (environment setting "Proxy AI
  credentials"), so the provider key never enters the VM. The sandbox gets a
  placeholder that only works through the proxy.

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

Every VM gets a private network stack inside the node process. Outbound traffic goes
through ordinary sockets of your machine after an egress check:

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

Traffic still leaves from your IP address. Anything a sandbox does on the internet
looks like it came from your network.

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
  variables and secret files with `env-secrets.key`; AI credentials, telemetry headers
  and webhook secrets with `secret.key`. Back up the whole data directory (both keys,
  the database and the blobs) while the server is stopped, and keep the keys as private
  as the database: a backup without them cannot decrypt those secrets.
- Without `--tls-cert`/`--tls-key` the API is plain HTTP. API keys and session cookies
  cross the network unencrypted, so run the server on a network you trust, or put it
  on a tailnet, which encrypts traffic between machines.
- Sign-up is closed by default. Node join tokens are admin-only unless `--open-nodes`.
