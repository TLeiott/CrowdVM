# CrowdVM secure layer

Design for isolating sandboxes from the machines that host them, and those machines
from the sandboxes. Written against the code as of 2026-11-11 (commit f2ef8d4).
Nothing here is implemented yet.

## 1. Summary

The node (a colleague's PC) today holds everything about a sandbox in plaintext:
the config drive with all secrets, the guest API token, the disk, the snapshots,
the console. The design below turns the node into a **dumb pipe and a disk of
ciphertext**:

1. **The guest dials the server itself** over pinned TLS, redeems a one-time boot
   ticket, fetches its config and keeps a multiplexed control channel open. The server
   runs exec/files/tunnels/events over that channel. The node never holds secrets,
   never proxies guest API traffic, and the `127.0.0.1:<port>` forward to the guest
   API disappears.
2. **The sandbox's writable state lives on an encrypted data disk** (LUKS2 inside
   the guest, key held by the server, delivered over the channel, never on the node).
   The base image stays plaintext and shared; the root filesystem is an overlayfs of
   base + encrypted upper. Snapshot chains are built from the data disk, so every
   layer on node disk and in the server's blob store is ciphertext. Thin
   provisioning, lazy chains, forks and compaction keep working: they move bytes.
3. **Node hardening in tiers**: no filesystem sockets for QMP and the NIC (fd
   passing, kept across node restarts with systemd's fd store), no core dumps,
   Landlock + no_new_privs around QEMU, and an optional system install with its own
   uid. A node advertises its tier; users can require one.
4. **Egress through the server over WireGuard** (guest kernel WireGuard, server-side
   `wireguard-go` on the gVisor stack CrowdVM already ships). The node sees one
   encrypted UDP flow to the server. Exit at the server by default; exit at the
   creator's CLI as a later option; node egress stays as a fallback.

What this does not do, and no software can do on consumer hardware: stop a node
owner who is root and willing to modify CrowdVM or dump the VM's memory. Section 4
has the wording for the docs.

## 2. Where things stand (verified in the code)

| Asset | Today | Where |
|---|---|---|
| Env, secret files, SSH keys, registry logins, AI proxy token, guest API token | plaintext on node disk | `vms/<id>/config.iso` and `vms/<id>/vm.json` (0600, but the node user *is* the owner); `internal/node/vm/qemu/qemu.go:969-985`, `:123` |
| Guest API (exec as root, files, tunnel) | reachable from the node at `127.0.0.1:<port>` with the token from `vm.json` | `qemu.go:1166`, `internal/node/api.go:156-193` sets `Authorization` itself |
| All server↔guest traffic (command output, file contents, ssh, desktop) | plaintext through the node's reverse proxy | `internal/node/api.go` `handleGuest`, `handleTunnel`; `internal/server/passthrough.go` |
| Disk, snapshot layers, lazy cache | plaintext qcow2 on node; plaintext blobs on server | `internal/node/vm/qemu/store.go`, `internal/node/transfer.go`, `internal/server/store/blobs.go` |
| Guest RAM | **dumpable by the node user without root or ptrace**: `qmp.sock` has no auth (`pmemsave`, `dump-guest-memory`, `migrate exec:`, `send-key`, `blockdev-*`); and `kill -SEGV <qemu>` produces a full core under systemd-coredump (`LimitCORE=infinity` on the user unit on this machine), readable with `coredumpctl` by the same uid | `qemu.go` `qmpEndpoint`, `sys_linux.go` |
| Guest NIC | `net.sock` is a path socket; whoever stops the node can attach and become the guest's network | `qemu.go:1173` |
| Serial console | `console.log` appended forever, guest kernel + systemd output | `qemu.go:1201` |
| Snapshot blobs on the server | any node token can `GET /node/v1/blobs/snapshot/<id>` for any id; no check that the node hosts that sandbox | `internal/server/nodes.go:718` |
| Server↔node transport | plain HTTP/websocket unless `--tls-cert`; tailnet encrypts, LAN does not | `docs/guide/security.md` |
| What the guest runs | QEMU, KVM, the image bytes and the guest binary all come from the node; the image is sha256-checked on download and never again | `guest-prepare`, `transfer.go` |

The last row is the root of the problem: **there is no root of trust inside the VM
that the node does not control.** Every design below is bounded by that.

## 3. Threat model

Attacker classes:

- **A. Curious node owner.** Uses their own account on their own machine: reads the
  data dir, connects to localhost ports and sockets, runs `coredumpctl`, `qemu-nbd`,
  `gdb`. Does not use root for the attack and does not modify CrowdVM's files or
  binaries. (They have sudo; using it to read `/proc/<pid>/mem` or change sysctls
  moves them to B.)
- **B. Active node owner.** Root. Modifies the node binary, the image, the guest
  binary, QEMU; dumps RAM; runs a fake guest.
- **C. Malicious guest.** Root inside the VM. Wants the node machine, its LAN or
  tailnet, other sandboxes, the node process, the server's node API, or to make the
  node's IP do things.
- **D. Network observer** between node and server, or between node and the internet.
- **E. Another node** (a node token from a different machine).
- **S. Server operator.** Trusted by design: holds every key. Not addressed.

What each class gets, today and after all phases:

| Class wants | Today | After |
|---|---|---|
| A: secrets (env, tokens, keys) | `cat` a file | Nothing on disk. Only via guest RAM, which needs ptrace (Yama blocks it), a core dump (disabled) or a QMP/NIC socket (none on the filesystem). Practically: B. |
| A: run commands in the sandbox | `curl 127.0.0.1:<port>` with the token | No listener, no token. |
| A: read the disk / snapshots | `qemu-nbd -c` on the qcow2 | Ciphertext. Key only in guest RAM and on the server. |
| A: read guest traffic | attach to `net.sock`, read `console.log` | WireGuard to the server: ciphertext. Console carries no sandbox content. |
| B: anything | yes | **yes.** Dump QEMU memory, patch the guest, MITM the pinned TLS by editing the pin. Tamper-evident in a few cases (ticket reuse), never prevented. |
| C: node files, LAN, other VMs | only through a QEMU/KVM bug; after escape = the owner's account, QMP of every sibling VM | after escape: Landlock-confined to its own VM dir, no TCP, no signals to the node, siblings unreachable; hardened install: a separate uid. Network: unchanged (netstack already blocks host/LAN/tailnet); with server egress the node policy shrinks to "server only". |
| C: the server | public API with its sandbox token (intended); node API needs a node token | same; plus the blob API scoped to the hosting node (E). |
| D: node↔server | plaintext on LAN | guest↔server TLS and WireGuard: nothing readable. Node↔server control (heartbeats, blobs) stays as today; blobs are ciphertext. |
| E: other nodes' snapshots | any id | only sandboxes placed on that node. |

Hardware TEE (AMD SEV-SNP, Intel TDX) is the only thing that moves class B out of
the "yes" column: it encrypts guest memory with a key the host cannot read and lets
the guest attest its boot measurements to the server, so the server encrypts secrets
to a VM it has verified. Neither test machine (Ryzen 5700X, Skylake laptop) has it.
Keep the design TEE-ready (the guest already dials the server and the server already
decides what to hand it) and add an attested tier when EPYC/Xeon hardware shows up.

## 4. The honest limit (text for `docs/guide/security.md`)

> **What the node owner can and cannot do.** Nothing about your sandbox is stored in
> plaintext on the node: not its configuration, not its disk, not its snapshots, not
> its traffic. The node's normal tools cannot talk to your sandbox's agent, read
> what it does, or see where it connects. A node owner who wants in has to modify
> their CrowdVM installation or dump the VM's memory as root. That is a deliberate
> act, not a `cat`. CrowdVM cannot prevent it on ordinary PCs; only confidential-
> computing hardware (AMD SEV-SNP, Intel TDX) can, and CrowdVM does not use it yet.
> Where tampering is detectable (a boot ticket redeemed twice, a channel that
> reconnects from the wrong place) the sandbox gets a security event and your
> dashboard shows it. For work that cannot tolerate even that, pin sandboxes to nodes
> you trust (`--node`, `--node-label owner=…`) or to nodes with the `hardened` tier.

## 5. Design

### 5.1 Principle

The node is a relay for bytes it cannot read, and a store for bytes it cannot
decrypt. Everything that needs to be secret is held by exactly two parties: the
server and the guest. Every secret path between them is encrypted end-to-end with
keys the node never sees. The node keeps only what it needs to run QEMU: image,
ciphertext layers, a boot stub, and a per-boot ops token for freeze/stop.

### 5.2 Boot ticket and config fetch

**Config drive after:** `config.json` becomes `proto.BootStub`:

```go
type BootStub struct {
    SandboxID string   `json:"sandboxId"`
    Boot      BootKind `json:"boot"`
    FirstBoot bool     `json:"firstBoot"`
    Server    string   `json:"server"`   // "10.0.2.2:8422" (gateway forward to the server's guest port)
    Pin       string   `json:"pin"`      // base64 SHA-256 of the server's guest-channel SPKI
    Ticket    string   `json:"ticket"`   // one-time, 32 random bytes, server-issued
    OpsToken  string   `json:"opsToken"` // node-generated, authorizes the node ops subset only (5.3)
    DataDisk  bool     `json:"dataDisk"` // 5.4: unlock /dev/vdb and mount the overlay root
    Legacy    *GuestConfig `json:"legacy,omitempty"` // old nodes/servers only, see compat
}
```

**Server:** new listener `--guest-port` (default main port + 2, 8422), TLS 1.3 only,
certificate self-signed from `guest-tls.key` in the data dir (generated once; the
SPKI hash is the pin). Independent of `--tls-cert`, so a plain-HTTP LAN server still
gets an encrypted guest channel. Endpoints:

| Endpoint | Body | Answer |
|---|---|---|
| `POST /guest/v1/boot` | `{ticket}` | `{session, dataKey, wg?: {...}}`. session: 32 random bytes, valid for this boot; dataKey: 64 bytes for LUKS (5.4) |
| `GET /guest/v1/config` | `Authorization: Bearer <session>` | the full `proto.GuestConfig` (env, secret files, repos, keys, AI proxy, scrub, …), **minus** `Token` |
| `GET /guest/v1/connect` | Upgrade `crowdvm-guest`, bearer session | yamux session (5.3) |

Ticket rules (table `boot_tickets`: sandbox_id, node_id, boot_seq, hash, issued_at,
redeemed_at, redeemed_from): issued by the server in `startSandbox`
(`internal/server/lifecycle.go:429`) in place of `randToken(40)`; one per boot;
valid 15 minutes; single use. Second redemption: 409, `security.ticket_reused` event
on the sandbox (owner notice + webhook), sandbox stays as it is. Restart
(`POST /v1/vms/{id}/restart`) carries a fresh stub. Warm pool: `adopt` passes a stub,
nothing else. BootInspect: a stub with a 2-minute ticket.

**What this buys:** no secret is ever written on the node. A ticket in `config.iso`
is worthless 200 ms after boot and worthless after 15 minutes if unused; a node that
redeems it first blocks the real guest, which fails loudly instead of running with
stale config. **What it does not buy:** a node that replaces the pin in the stub and
runs a MITM reads everything (B). A node that runs a fake guest reads everything (B).

**Why not** guest-generated keys + "server encrypts to the guest": same trust
(the pubkey arrives through the node), more code, and it still needs a channel for
the reply. Pinned TLS from the guest gives the same guarantee with stdlib
`VerifyPeerCertificate` and lets the rest of the control plane ride on it.

**Why not** vTPM/measured boot: swtpm runs on the node as the node user; its
measurements are whatever the node says. Signed images: the node verifies the
signature today (sha256) and can still edit the file afterwards. TOFU guest
identity: survives across boots only on the disk, which the node can read until
5.4, and after 5.4 needs the key the identity would protect. None of these move an
attacker from B to "cannot".

### 5.3 Guest control channel (dial-home)

After `boot`, the guest opens `GET /guest/v1/connect` and serves its existing HTTP
mux (`internal/guest/internal.go`) on streams the server opens, with `yamux` exactly
as node↔server does today (`internal/server/nodes.go`, `internal/node/node.go`). The
server keeps `guests map[sandboxID]*GuestConn` next to `nodes`. The guest redials
with backoff for the life of the boot (session bearer kept in `/run/crowdvm/session`,
tmpfs, 0600). A new boot has a new ticket, a new session and a new channel.

Server-side routing: everything that today does `node.Do("…/guest/<rest>")` or
`mux.DialUpgrade(st, …/tunnel)` goes through `s.guests[id]` when present:
passthrough (`passthrough.go`), tunnels (ssh, `crowdvm tunnel`, public URLs,
desktop, browser), events stream, history export, apply-config, scrub, setup-status,
prepare-stop for server-initiated stops. Without a guest session (legacy guest, or
the channel is down) the server falls back to the node path if the node still
offers it, else 409 `guest_unreachable`.

**Node ops subset.** The node still needs to coordinate snapshots and stops without
the server in the loop. The guest serves a second, tiny listener on the NIC
(`10.0.2.15:7071`), reached only from the gateway through the netstack
(`vm.NICDialer` already does this for direct ports), authorized by `OpsToken`:

| Kept for the node | Moved to the server channel |
|---|---|
| `GET /v1/health` (version, setup status, reposCloning) | `commands`, `files`, `tunnel`, `events/stream`, `history/*`, `apply-config`, `scrub`, `setup-status`, `desktop/start`, `browser/*`, `sshkey`, `prompt`, … |
| `POST /v1/internal/{prepare-stop,sync,freeze,thaw,playbook/finish}` | |
| `PUT /v1/internal/firewall` | |
| `POST /v1/internal/adopt` (body: a `BootStub`) | |

Nothing in that subset returns sandbox content or takes input that changes it beyond
what QMP `stop`/`quit` already allow. The `hostfwd`/loopback forward for the guest
API goes away entirely (`qemu.go:1166`, `spawn` listener, `GuestAddr`). The `-netdev
user` slirp fallback keeps the old path for broken setups and is marked insecure, as
it is today.

**Interactions.** ssh, `crowdvm tunnel`, desktop VNC, public URLs: all are
`/v1/tunnel?port=N` streams, now carried guest→server over TLS, server→user as today.
The node is not on the path. **Direct ports** (`direct-host`) enter the guest NIC in
plaintext by design; keep the feature, document that the node sees that traffic, and
disable it for sandboxes that ask for `--secure`.

**Bandwidth/latency:** the bytes already travel node→server; they now start one hop
earlier (inside the VM) and are TLS-framed. No measurable change.

### 5.4 Encrypted data disk and overlay root

**Layout per VM** (qemu driver):

```
/dev/vda  base image qcow2, read-only (throwaway overlay for safety, discarded at stop, never uploaded)
/dev/vdb  data.qcow2: per-sandbox thin overlay chain, virtual size = diskGiB; contents = LUKS2
/dev/vdc  config drive (as today)
```

**Inside the guest** (image v4): an initramfs-tools `local-top` hook (~40 lines of
shell) mounts the config drive, runs `crowdvm-guest unlock` from it (the binary
ships with every boot, as today), which: configures `eth0` statically (addresses are
constants: `10.0.2.15/24` via `10.0.2.2`, no DHCP needed), dials `Server` with
`Pin`, redeems `Ticket`, writes the session to `/run/crowdvm/session` and pipes
`dataKey` to `cryptsetup open --key-file=-` (`luksFormat` + `mkfs.ext4` on first
boot, detected by the missing LUKS header). Then:

```
mount -o ro /dev/vda      /lower
mount /dev/mapper/crowdvm-data /data        # ext4, allow_discards, no_read_workqueue,no_write_workqueue
mount -t overlay overlay -o lowerdir=/lower,upperdir=/data/upper,workdir=/data/work /root
mount --bind /data/home   /root/home/user   # ext4 directly: Docker overlay2 refuses an overlayfs backing fs
mount --bind /data/docker /root/var/lib/docker
mount --bind /data/crowdvm /root/var/lib/crowdvm
```

initramfs-tools moves `/run` into the new root, so the session survives into
`crowdvm-guest.service`. `apt install`, `/etc`, `/opt`, `/usr/local`, journald,
`/tmp`: all land in the upper on the encrypted disk. Nothing persistent touches
`/dev/vda`. The image needs `cryptsetup-initramfs`, `overlay` and `virtio_net` in the
initrd; the hook is a no-op without `dataDisk` in the stub, so one image serves both
modes.

**Keys.** `dataKey` is generated per sandbox at create, stored in `sandboxes`
sealed with the existing `env-secrets.key` AEAD (`feat_env_crypto.go`), copied to a
fork (shared ciphertext layers need the same key), given to a BootInspect VM with its
ticket. Rotation = none in v1 (a fork is the way to re-key: new sandbox, copy over
the channel).

**Snapshots, chains, forks, lazy fetch, compaction**: unchanged in mechanism; the
driver's "disk" is now `data.qcow2`. `blockdev-snapshot-sync`, `block-stream`,
`qemu-img rebase`, `query-blockstats` (write detection), the http+copy-on-read lazy
graph, `Supersedes`, upload sha256: all operate on the data chain. The base image
stays a plain, shared, deduplicated blob. Discards: `fstrim` inside the guest →
ext4 → dm-crypt (`allow_discards`) → virtio-blk → qcow2 unmap, so thin provisioning
and "not saved" cleanup keep their effect. `detect-zeroes=unmap` stops mattering
(ciphertext has no zero runs); harmless.

**Server blob store**: holds ciphertext layers + plaintext images. A server backup
without `env-secrets.key` reveals nothing of sandboxes (true today for env secrets,
now for disks too). The server operator can decrypt everything; that is the trust
model.

**Performance** (laptop, Skylake): AES-XTS with AES-NI is 2–4 GB/s per core, far
above what a qcow2 chain over a laptop SSD delivers; dm-crypt adds one memcpy and a
queue hop (the two `no_*_workqueue` flags remove the hop). Overlayfs on `/` costs a
lookup on first open; `/home/user` and Docker bypass it. Expect no visible change;
measure with `fio` before/after (red team R2).

**Alternative rejected: qcow2-native LUKS** (`encrypt.format=luks`, key via
`-object secret`). Simpler (no image change, `qemu-img` understands the chain) and
gives the same at-rest protection on node and server. But the key must pass through
the node process and sit in QEMU's memory *and* the node's; the node code becomes the
place where a leak (log, `vm.json`, QMP `object-add` args) happens, and the design
goal "node code never handles a sandbox secret" is lost. Keep it as the fallback if
the initramfs work slips: it would be phase 2-lite, a day of work, with "protects at
rest, not at runtime" in the docs.

**Alternative rejected: encrypt the whole root.** Kills thin provisioning (the base
would be re-encrypted per sandbox) and dedupe. Not worth it; the base image is
public anyway.

**Compat/migration.** Image v3 sandboxes keep plaintext root overlays forever
(`secure.disk=false` in the API). New sandboxes on v4 images default to the data
disk. Later, `crowdvm snapshot convert`: a conversion VM mounts the base read-only
at `/base` and runs `rsync -aHAX --compare-dest=/base / /data/upper` (copies only
files that differ from the base = the overlay upper; deletions versus the base are
not preserved, documented). Not in scope for v1.

### 5.5 Node hardening (QEMU and the node process)

Tiers advertised in `NodeInfo.Trust`: `basic` (today), `hardened` (user service with
the measures below), `isolated` (system install, own uid). Scheduler constraint
`--node-trust hardened|isolated` on `crowdvm new`; `--secure` = `hardened` +
channel + data disk + server egress, failing placement when unavailable.

**Linux user service (`hardened`), no root needed:**

| Measure | How | Closes |
|---|---|---|
| No path sockets | QMP: `-chardev socket,id=qmp,fd=N` from a `socketpair`; NIC: `-netdev stream,addr.type=fd,addr.str=N`. Both fds (and the net.sock side) stored with `sd_pid_notify_with_fds(FDSTORE=1)`, `FileDescriptorStoreMax=64` in the unit, recovered from `LISTEN_FDS` on node restart; VMs keep surviving node restarts. Fallback (no systemd, `FDSTORE` refused): path sockets as today, tier stays `basic`. | A: QMP RAM dumps, keystroke injection, NIC takeover |
| No core dumps | `prlimit(pid, RLIMIT_CORE, 0)` on QEMU right after `Start`, and on the node itself at startup (systemd-coredump honours a zero limit and refuses the dump). | A: `kill -SEGV` + `coredumpctl` |
| ptrace | Yama 1 is the distro default; the node refuses to advertise `hardened` when `ptrace_scope=0`. | A: `gdb -p`, `/proc/pid/mem` |
| Landlock + no_new_privs | QEMU is exec'd through `crowdvm-node exec-qemu` (re-exec of the node binary) which applies a ruleset: read on the image, read-write on `vms/<id>/` and the sandbox's layer files, read on `/usr`, `/lib*`, `/etc`, rw on `/dev/kvm`, `/dev/vhost-*` if used, `/dev/dri/renderD*` for virgl, `/dev/vfio/*` for passthrough; TCP connect only to the lazy blob proxy port (ABI 4), no bind; `LANDLOCK_SCOPE_SIGNAL` and `LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET` (ABI 6, kernel ≥ 6.12). `PR_SET_NO_NEW_PRIVS` first. Existing `-sandbox on,…` seccomp stays. | C after escape: reading the owner's home, other VMs' dirs, QMP of siblings, signalling the node, opening sockets |
| Console | `-serial` to a node-side ring buffer (last 256 KiB in memory via a unix socket chardev fd) and to `console.log` only when a boot fails; guest agent logs to journald only. | A: reading console output; disk growth |
| Memory on disk | document: hibernation and swap write guest RAM to the node's disk; recommend encrypted swap/FDE for node owners; `hardened` reports `swapEncrypted` from `/proc/swaps` + `dm` type as a hint. | A: swap/hibernation image |
| Status UI and logs | the node UI lists sandbox ids and states only (it does today); netstack "blocked" lines log destination IPs: keep (it is the owner's firewall log), rate-limited as now. | metadata leaks |

**System install (`isolated`), one `sudo`:** `crowdvm-node install-service --system`
creates user `crowdvm` (data dir `/var/lib/crowdvm`, 0700), runs the node and QEMU
as that uid with `LimitMEMLOCK=infinity` so QEMU can run with `-overcommit
mem-lock=on` (guest RAM never swapped), `ProtectHome=yes`, `PrivateTmp`,
`NoNewPrivileges`. The owner's account cannot read the data dir, connect to anything,
or signal the processes without sudo. Per-VM uids (`-run-with user=`) need
CAP_SETUID; later.

**Windows:** `-sandbox` and Landlock do not exist; QEMU runs under WHPX. QMP is
loopback TCP, open to every local user: add `-object tls-creds-x509,id=qmptls,
dir=<data>\qmp-tls,endpoint=server,verify-peer=on` with a node-generated CA, client
cert in memory for the node. Meaningful only in the SCM-service install
(LocalSystem; the data dir ACL'd to SYSTEM and Administrators), which is already the
recommended Windows install: that is Windows's `hardened`. Per-user installs stay
`basic`. The guest-side measures (channel, data disk, WireGuard) are node-OS
independent, so a Windows node gets the at-rest and in-transit protection for free.

### 5.6 Egress

| Option | Node sees | Exit IP | CLI offline | Bandwidth | Latency | Effort |
|---|---|---|---|---|---|---|
| (i) node egress (today) | everything, plaintext; DNS | node owner's | n/a | node uplink | best | 0 |
| (ii) guest WireGuard → server, exit at server | one UDP flow: sizes, timing, volume | the server's | n/a | node uplink + server uplink ×2 | +1 hop (LAN 1 ms, tailnet 5–30 ms) | 3–5 days |
| (iii) through server to the creator's CLI, exit there | as (ii) | the creator's | traffic stops or falls back | as (ii) + creator's home uplink | +2 hops | (ii) + 2 days |
| (iv) WireGuard guest → CLI directly, server relays bytes | as (ii) | the creator's | same | same | +2 hops | (iii) + rekey dance |
| (v) HTTPS proxy / SOCKS from the guest | TLS to the server | server | n/a | as (ii) | +1 | 1 day, but not transparent: UDP, DNS, raw TCP leak or break |

**Pick (ii), build the exit as an interface, add (iii) later.** Reasons: the guest
kernel has WireGuard; it is transparent (Docker, UDP, ICMP, DNS all follow the
default route); the server already depends on gVisor netstack and has the forwarders
and the egress `Policy`; liability moves from the colleague's home IP to the
org's server, where it belongs; and the org gets one place to set and log egress
policy. (iii) is a routing choice on top: an exit is "something that accepts IP
packets for sandbox X"; the server is one, a connected CLI is another.

**Server side.** `wireguard-go` (`golang.zx2c4.com/wireguard`, new dependency) with a
custom `tun.Device` that feeds the gVisor `channel.Endpoint`; move the forwarders and
`Policy` from `internal/node/vm/netstack` to `internal/netstack` with a pluggable
link (QEMU stream frames or WireGuard IP packets). One WG device, UDP listener
`--egress-port` (default 8423), one peer per running sandbox with a `/32` in
`10.200.0.0/16`, `AllowedIPs` = that `/32`. DNS: the stack answers on its own
tunnel IP and relays to the server's resolvers (same code as `dns.go`). Policy on
the server: the same zero-value `Policy` (internet yes; the server's loopback, LAN,
tailnet, link-local no), the server's own API pinholed, plus org-level
allow/deny from the dashboard. No root: wireguard-go's default bind is a plain UDP
socket.

**Guest side.** `wireguard-tools` in image v4. After `config`, the agent: generates
a keypair, `POST /guest/v1/wg {pubkey}` → `{serverPub, endpoint: "10.0.2.2:8423",
address, dns}`; `ip link add wg0 type wireguard`, `wg set`, `ip addr`, `ip route add
default dev wg0 metric 50` (the pinhole to `10.0.2.2` stays on-link via eth0, so no
loop), `/etc/resolv.conf` → the tunnel DNS, MTU 1380. Docker containers follow the
guest's default route. Keys are exchanged over the pinned channel, so the node cannot
MITM the tunnel (B excepted).

**Node side.** `GatewayForwards` grows a UDP map (`10.0.2.2:8423` → server:8423,
relayed with the existing `relayUDP`). For `egress=server` the server sends
`VMSpec.Net.Allow = [server]` so the node's netstack passes **only** the pinholes:
a kill switch that holds even if `wg0` is down or the guest removes it.

**Policy and defaults.** Sandbox field `egress: node|server|client` (API, `crowdvm
new --egress`, environment default, server default `--egress-default`). Default
`server` once the server has WireGuard enabled; `node` otherwise. `client` with
`clientFallback: server|none`; `none` means traffic blackholes while the CLI is
offline (the control channel still works; agents that need the net stall).

**What the node still sees with (ii):** that a VM exists and talks UDP to the
server; packet sizes and timing; total volume. No destinations, no DNS, no content.
Padding is not worth it.

**What the server now sees:** every byte of sandbox traffic, which it sees today
for control traffic anyway. Note it in the docs.

### 5.7 Transparency

- Sandbox API/CLI: `security: {channel: "e2e"|"node", disk: "encrypted"|"plain",
  egress: "server"|"node"|"client", nodeTrust: "basic"|"hardened"|"isolated"}`;
  `crowdvm nodes` shows the tier.
- Security events on the sandbox (webhooks, notices, dashboard): `security.ticket_reused`,
  `security.channel_replaced` (a second connect with the same session from a
  different netstack flow), `security.node_tier_dropped` (a node stopped meeting its
  tier while hosting the sandbox).
- Attestation-lite: the guest reports in `boot` the sha256 of its own binary, the
  kernel cmdline and the image id it sees; the server compares with what it shipped
  and raises `security.image_mismatch`. A tampered guest can lie, so this catches
  drift and sloppiness, not B. Say so in the docs.

### 5.8 Also fix now (independent of phases)

- Scope `GET /node/v1/blobs/snapshot/{id}` and `GET /node/v1/snapshots/{id}` to
  snapshots in the chain of a sandbox placed on the requesting node, or that the node
  uploaded (`nodes.go:718`).
- `vm.json`: stop persisting `Spec.Guest` beyond the stub.
- Node: never log request paths of guest calls (none after 5.3); keep it that way.

### 5.9 Not doing

- Client-held disk keys (server blind): sandboxes run while the CLI is offline, so
  the server must be able to start them. Possible later as an opt-in with "resume
  needs your CLI online".
- Per-packet padding, traffic shaping.
- vTPM, Secure Boot chain inside the VM: no root of trust without a TEE.
- Encrypting node↔server control traffic beyond what the admin's TLS or tailnet gives:
  after the phases it carries no sandbox content.

## 6. Phases

Ordered by value per day. One engineer. Estimates assume the e2e suite is used as the
regression net.

| Phase | Days | Removes | Needs |
|---|---|---|---|
| 0 Hygiene | 0.5 | cross-node blob reads, core dumps, console growth | nothing |
| 1a Boot ticket + config fetch | 1 | all secrets from node disk; guest token from the node | server guest port, guest `boot`/`config`, node stub |
| 1b Dial-home channel | 1–2 | node as proxy; hostfwd; plaintext exec/files/tunnels through the node | 1a |
| 2 Encrypted data disk | 3–4 | plaintext disks and snapshots on node and server | image v4, 1a |
| 3 Node hardening | 2–3 | QMP/NIC sockets, escape blast radius; adds tiers | nothing (parallel to 2) |
| 4 Server egress (WireGuard) | 3–5 | node sees traffic; node IP liability | 1b |
| 5 Client exit, convert, attested tier | later | | 4, 2, hardware |

### Phase 0: hygiene (half a day)

- `internal/server/nodes.go` `handleBlobGet`, `handleSnapshotMeta`: allow only when
  the snapshot's sandbox has `node_id` = requester, or the snapshot's `location`/upload
  node = requester, or kind = image.
- `internal/node/vm/qemu/sys_linux.go`: after `cmd.Start`, `unix.Prlimit(pid,
  RLIMIT_CORE, {0,0})`; in `cmd/crowdvm-node` at start the same for self. Unit
  test: read `/proc/<pid>/limits`.
- `console.log`: truncate to the last 1 MiB on launch (full ring buffer comes in
  phase 3).
- Tests: existing; red team R1 (node side) partially.
- Does not protect: anything else.

### Phase 1a: boot ticket and config fetch (one day)

Changes:
- `internal/proto`: add `BootStub`; `VMSpec.Stub *BootStub` (additive);
  `NodeInfo.GuestChannel bool`. `GuestConfig` keeps `Token` for legacy.
- `internal/server`: `guest_channel.go`: listener on `--guest-port`, cert in
  `guest-tls.{key,crt}`, pin computation; `POST /guest/v1/boot`, `GET
  /guest/v1/config`; table `boot_tickets` (migration); `lifecycle.go:429`: when
  `node.Info.GuestChannel`, send `Stub` and keep `Guest` empty except `SandboxID`,
  `Boot`, `FirstBoot`, `ListenAddr`; else legacy. The guest API token is still
  needed for the node proxy until 1b: the server generates it, hands it to the guest
  in `config`, and sets `Authorization` itself in `passthrough.go`/`nodes.go`
  (`handleGuest` on the node stops setting it when the token is absent from the spec).
- `internal/node`: write the stub as `config.json`; `GatewayForwards[guestPort]`;
  `Pinholes` += server host:guestPort; `vm.json` holds no token; `handleGuest`
  passes the server's `Authorization` through.
- `internal/guest`: `--config` may be a stub: dial `Server` with pin, redeem,
  fetch config, then run as today. `Restart=always` restarts: reuse
  `/run/crowdvm/session`.
- Compat: old node (no `GuestChannel`) → legacy spec; old guest binary cannot exist
  with a new node (the node ships it). Old server + new node: the node sees a full
  `Guest` and no `Stub`, writes it as today.
- Tests: unit: ticket single use, expiry, pin mismatch fails the dial; e2e suite
  unchanged in behaviour; new e2e: `config.iso` contains no `env`, `vm.json` no
  `token`, `curl 127.0.0.1:<port>/v1/commands` without a token → 401.
- Does not protect: the node still relays the HTTP plaintext (reads command output,
  file contents); RAM; disk; traffic.

### Phase 1b: dial-home channel (one to two days)

- `internal/guest`: `GET /guest/v1/connect` upgrade → `yamux.Server` over the TLS
  conn, serve `a.Handler()` on accepted streams; second mux on `10.0.2.15:7071` with
  the ops subset and `OpsToken`; drop `:7070`.
- `internal/server`: `GuestConn` registry; `guestDo`, `guestUpgrade`; switch
  `passthrough.go`, `lifecycle*.go`, `feat_history.go`, `feat_desktop.go`,
  `feat_host.go`, `nodes.go:198` to prefer the guest session. Node fallback for
  legacy specs.
- `internal/node`: remove the hostfwd forward and the loopback listener in `spawn`;
  `GuestAddr` returns the NIC ops address; probes/prepare-stop/freeze use it with
  `OpsToken`; events are no longer forwarded by the node for channel guests.
- Protocol: `proto.OpsToken`, upgrade token `crowdvm-guest`.
- Tests: e2e suite (all guest features now cross the channel); kill the channel
  mid-stream and check reconnect; node restart keeps the VM and the channel (the
  guest redials); red team R1 complete.
- Does not protect: RAM via QMP/coredump/ptrace; disks; traffic.

### Phase 2: encrypted data disk (three to four days)

- `images/scripts/minimal.sh` + `images/rootfs/minimal/usr/share/initramfs-tools/
  {hooks,scripts/local-top}/crowdvm`: packages `cryptsetup-initramfs
  wireguard-tools`, modules `overlay virtio_net isofs`, the hook. `finalize.sh`
  rebuilds the initrd. Image id `…-v4`.
- `internal/guest`: `unlock` subcommand (static net config, dial, redeem, key to
  stdout) and the mount sequence; `boot.go` growpart/resize2fs → `cryptsetup resize`
  + `resize2fs` on the data device; `prepare-stop`/`fstrim` target `/data`.
- `internal/node/vm/qemu`: two drives; the chain/overlay/snapshot/compaction code
  points at `data.qcow2`; the root gets a throwaway overlay deleted on stop;
  `lazy.go` graph on the data drive; `inspect.go` passes a stub.
- `internal/server`: `data_key` column sealed with `envAEAD`; set at create; copied
  on fork; delivered in `boot`; `secure.disk` in the API.
- Compat: `VMSpec.DataDisk bool` (additive); server sets it only for v4 images and
  chains that started encrypted (new column on `sandboxes`); v3 sandboxes unchanged.
- Tests: e2e `bootloop` (create → fork → stop → resume on the other node with lazy
  chain → fork); `files` and `snapshot` groups; `qemu-img check` on uploaded layers;
  `fio` 4k random rw inside the guest before/after; first boot time before/after.
- Does not protect: RAM; B; the base image (public anyway).

### Phase 3: node hardening (two to three days, parallel to 2)

- `internal/node/vm/qemu/sys_linux.go`: socketpairs for QMP and NIC, `ExtraFiles`,
  `-chardev socket,fd=`, `-netdev stream,addr.type=fd`; fd store via
  `sd_notify` (`NOTIFY_SOCKET`, `FDSTORE=1`, `FDNAME=<sandbox>`); recovery from
  `LISTEN_FDS`/`LISTEN_FDNAMES` in `Driver.adopt`; unit file gets
  `FileDescriptorStoreMax=256`. Fallback to path sockets with a log line and tier
  `basic`.
- `cmd/crowdvm-node` hidden `exec-qemu` command: `PR_SET_NO_NEW_PRIVS`, Landlock
  ruleset (`golang.org/x/sys/unix` has `LandlockCreateRuleset`, `LandlockAddRule`,
  `LandlockRestrictSelf`), then `syscall.Exec`. Paths come from the driver as flags.
  Probe the ABI at start; advertise which scopes apply.
- Console ring buffer; `NodeInfo.Trust`; scheduler constraint (`lifecycle_place.go`)
  and `--node-trust`/`--secure` flags; `install-service --system`.
- Windows: `tls-creds-x509` for QMP in service mode.
- Tests: unit tests for the Landlock path list; an e2e `escape` group that runs
  `/bin/sh` through `exec-qemu` with a VM's ruleset and asserts it cannot read
  `~/.ssh`, cannot open a sibling's `vms/<id>`, cannot `connect()` to the node UI,
  cannot `kill -0` the node; node restart keeps VMs (fd store); red team R3.
- Does not protect: B; KVM bugs that give kernel code execution (Landlock is a
  user-space sandbox).

### Phase 4: server egress (three to five days)

- `internal/netstack` (moved from `internal/node/vm/netstack`, link made pluggable);
  `internal/server/egress.go`: wireguard-go device over a `tun.Device` bridge to the
  stack, peers per sandbox, DNS relay, `Policy` with org rules;
  `POST /guest/v1/wg`.
- Guest: `wg0` setup after config; `egress` from `GuestConfig` (additive field).
- Node: UDP gateway forward; per-VM `Net.Allow` kill switch from `VMSpec.Net`.
- API: `egress` on create/update, environment default, server flag; docs.
- Tests: e2e `egress` group: inside the guest `curl https://ifconfig.me` returns the
  server's IP; `tcpdump` on the node (as root, test-only) shows only UDP to the
  server; `dig` resolves; `apt update`, `docker pull`, `npm install` sizes/timing
  logged for before/after; wg down → nothing leaves (kill switch); red team R4.
- Does not protect: the server sees everything (as today for control traffic);
  timing/volume at the node.

### Phase 5 and later

Client exit (CLI as peer of the server's stack over its websocket), `snapshot
convert`, per-VM uids, attested tier (SEV-SNP: `-object sev-snp-guest`, the `boot`
endpoint verifies the attestation report before releasing `dataKey`; the design
above needs no protocol change for it).

## 7. Red-team plan

Five rounds, each after the matching phase. Run (a) as root inside a sandbox, (b) on
the node as the node user and, separately, as root. "Expect" is after the secure
layer is in place for that round. Everything (a) also makes a good permanent e2e
test.

### Round 1 (after phase 1b): secrets and control

Node side, as the node user:

```sh
D=~/.local/share/crowdvm/node; V=$D/vms/<id>
mkdir /tmp/cfg && sudo mount -o loop,ro $V/config.iso /tmp/cfg; cat /tmp/cfg/config.json
#   expect: sandboxId, boot, server, pin, ticket, opsToken only; no env/secret/token
grep -c -E 'token|env|secret' $V/vm.json          # expect: 0 (only the stub)
ss -ltnp | grep qemu; ss -ltnp | grep crowdvm-node  # expect: no 127.0.0.1:<port> to a guest; only 8421 and lazy proxy
curl -s http://127.0.0.1:<any>/v1/commands         # expect: connection refused
# replay the ticket from config.iso against the server's guest port:
curl -sk --resolve x:8422:<server-ip> https://x:8422/guest/v1/boot -d '{"ticket":"…"}'
#   expect: 409 ticket_used, and a security.ticket_reused event on the sandbox
# node ops endpoint with the ops token:
# (needs a client on the netstack; use `crowdvm-node debug dial <id> 7071`) POST /v1/commands → expect 404
grep -i -E 'authorization|CROWDVM_API_KEY' $D/node.log $V/qemu.log  # expect: nothing
```

Node side, as root (document the result; nothing here is expected to be blocked):

```sh
gdb -p $(pgrep -f crowdvm-<id>) -batch -ex 'dump memory /tmp/ram 0x7f0000000000 0x7f0000100000'
grep -a -o 'CROWDVM_TOKEN=[a-z0-9_]*' /tmp/ram          # expect: found (class B; goes in the docs)
```

Guest side:

```sh
curl -s http://10.0.2.2:8421/ ; curl -s http://10.0.2.2:8420/node/v1/info      # expect: refused; 401
nmap -p- 10.0.2.2 10.0.2.3 10.0.2.1                                             # expect: only 8420, 8422 (8423 later) on .2, 53 on .3
curl -s -H "Authorization: Bearer $CROWDVM_TOKEN" $CROWDVM_API_URL/sandboxes     # expect: only this sandbox; create → 403
curl -s -H "Authorization: Bearer $CROWDVM_TOKEN" $CROWDVM_API_URL/../node/v1/blobs/image/x  # expect: 401
```

### Round 2 (after phase 2): disks and snapshots

```sh
# node user
sudo modprobe nbd; sudo qemu-nbd -r -c /dev/nbd0 $D/store/snapshots/<layer>.qcow2
sudo blkid /dev/nbd0                      # expect: TYPE="crypto_LUKS"; no ext4
strings $V/lazy-cache.qcow2 | grep -c -i -E 'BEGIN .*PRIVATE|CROWDVM_' # expect: 0
# throwaway root overlay: nothing persistent
qemu-img info $V/root-overlay.qcow2 | grep 'disk size'   # expect: a few MiB at most
# server data dir
strings <server data>/blobs/snapshot/<id> | grep -c CROWDVM_   # expect: 0
# fork and resume on the other node still work (e2e), inspector reads files of a stopped sandbox (e2e `files` on snapshot)
# console
grep -c -E 'CROWDVM_|Bearer' $V/console.log               # expect: 0 even after `echo $CROWDVM_TOKEN > /dev/kmsg` in the guest? No: that is the guest's own choice; document that the console is not private, and that the agent never writes there.
```

Guest side: `cryptsetup status crowdvm-data` shows aes-xts-plain64; `fio --name=r
--rw=randrw --bs=4k --size=1G --runtime=30 --filename=/home/user/f` within 10 % of
the phase-1 number; `df /`, `df /home/user`, `docker run hello-world` all work;
`apt install sl` survives stop/resume.

### Round 3 (after phase 3): QEMU and the node process

```sh
# node user
ls $V/*.sock                                    # expect: none
find / -path /proc -prune -o -type s -user $USER -print 2>/dev/null | grep -i -E 'qmp|net'   # expect: none
kill -SEGV $(pgrep -f crowdvm-<id>); coredumpctl list | tail -1   # expect: "Resource limits disable core dumping" / no dump; VM restarts from last snapshot
gdb -p $(pgrep -f crowdvm-<id>)                 # expect: ptrace: Operation not permitted (Yama 1)
cat /proc/$(pgrep -f crowdvm-<id>)/cmdline | tr '\0' ' ' | grep -c -E 'secret|key'   # expect: 0
# as root: everything works (document). Also: systemctl --user restart crowdvm-node → VMs stay running, channel reconnects within 10 s.
```

Escape simulation (no real escape needed): run a shell with the VM's Landlock ruleset:

```sh
crowdvm-node exec-qemu --vm <id> -- /bin/sh      # debug mode of the wrapper
cat ~/.ssh/id_ed25519; ls ../<other id>/; cat ~/.local/share/crowdvm/node/config.json   # expect: EACCES, EACCES, EACCES (nodeToken unreadable)
python3 -c 'import socket;socket.create_connection(("127.0.0.1",8421))'               # expect: EPERM (Landlock TCP)
kill -0 $(pgrep crowdvm-node)                    # expect: EPERM (signal scope)
```

### Round 4 (after phase 4): network, from the guest

```sh
curl -s https://ifconfig.me                       # expect: server's public IP, not the node's
# on the node, as root, test-only:
tcpdump -ni any 'not port 22' -c 200 | awk '{print $3,$5}' | sort | uniq -c   # expect: only <node>:* → <server>:8423 UDP (+8420/8422 TCP)
# kill switch
ip link del wg0; curl -m 5 https://1.1.1.1         # expect: timeout (node netstack allows only the server pinholes)
# classic tricks against the node netstack (wg0 down, so they hit it)
nmap -sn 192.168.0.0/24 100.64.0.0/10 169.254.0.0/16  # expect: nothing
curl -m 3 http://169.254.169.254/; curl -m 3 http://[fe80::1%eth0]/ ; ping6 2606:4700::1111   # expect: fail, no IPv6 offered
dig @10.0.2.3 rebind.example +short                 # a name that resolves to 192.168.1.1: then curl it → expect: blocked by IP at connect time, not at resolve time
# with wg0 up the same resolve goes to the server's stack: expect the same block there
arping -c3 -S 10.0.2.2 10.0.2.15; dhclient -r && dhclient eth0 (with a rogue DHCP server on eth0)   # expect: netstack ignores foreign DHCP/ARP, guest keeps 10.0.2.15
ping -s 65000 10.0.2.2; hping3 --frag -d 2000 10.0.2.2 -p 8420   # expect: no crash; fragments dropped (frag && DHCP/ICMP paths) and oversized frames drop the connection, VM keeps running
nc -u 10.0.2.2 8423 < /dev/urandom (flood)           # expect: node UDP relay stays up; flow cap (MaxFlows) holds
# node API with a leaked join token (ask the admin for one): POST /node/v1/register from the guest → expect: works (nodes are not restricted by source); document: keep join tokens admin-only, consider refusing registration from sandbox tunnel IPs
```

Also from the guest with wg0 up: `curl http://10.0.2.2:8420/api/v1/…` still works
(pinhole), the server's LAN (`192.168.*` of the server) is unreachable through the
tunnel, DNS answers come from the tunnel IP, `docker run alpine wget -qO- ifconfig.me`
shows the server's IP, `nc -u` to a public UDP echo works, MTU: `ping -M do -s 1400
8.8.8.8` fails and `-s 1300` works (document the MTU).

### Round 5: full adversary and cross-tenant

- As root on the node: `virsh`-style memory read via `/proc/<qemu>/mem` → secrets
  found. `sudo` edit of `config.iso` pin + a MITM with `socat openssl-listen` →
  config readable. Fake guest: boot the image with a modified `crowdvm-guest` that
  prints its config → readable. These three are the documented limit.
- Node A with its node token requests node B's layers: `curl -H "Authorization:
  Bearer $nodeTokenA" $SERVER/node/v1/blobs/snapshot/<B's id>` → 403.
- Guest with its sandbox token tries another sandbox's `files`, `tunnel`, `desktop`
  → 403/404; tries `api-key create` → 403.
- Timing: measure what a node sees for `curl https://github.com` vs `npm install` on
  the WireGuard flow, to write the "sizes and timing" line in the docs honestly.
- Laptop sleep/hibernate with a running VM: check `/swapfile`/hibernation image for
  guest strings; write the recommendation.

## 8. Decisions for Tim

1. Phase order: 1a → 1b → 3 → 2 → 4, or 2 before 3? My pick: 3 before 2, because
   QMP and core dumps are the only class-A path into RAM and cost two days;
   the disk is harder and today's A already needs `qemu-nbd`.
2. Default egress once phase 4 exists: `server`. Requires the server's uplink to
   carry sandbox downloads; a per-org switch for small servers.
3. `--secure` as a single flag (hardened node + channel + encrypted disk + server
   egress, fail placement otherwise) versus separate flags. My pick: the single
   flag plus the individual fields in the API for those who care.
4. Whether `isolated` (system install) becomes the recommended Linux install in the
   node-owner guide. My pick: yes, with the user service kept for "try it out".
