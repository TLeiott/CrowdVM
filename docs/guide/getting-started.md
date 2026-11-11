# Getting started

A CrowdVM pool has three parts:

- one **server** (`crowdvm-server`): accounts, API, dashboard, scheduler, snapshot store;
- one or more **nodes** (`crowdvm-node`): Linux or Windows machines that run sandboxes as QEMU VMs;
- **clients**: the `crowdvm` CLI, the dashboard at `http://<server>:8420/`, the API and the SDKs.

Every node and every client must reach the server's address. Nodes dial out to the
server, so a node behind NAT or a home router works without port forwarding.

## 1. Build

You need Go 1.27 or newer.

```sh
make build                  # bin/crowdvm, bin/crowdvm-server, bin/crowdvm-node, bin/crowdvm-guest
make dist VERSION=v0.1.0    # optional: binaries for every OS/arch in dist/, for /install and /downloads
```

## 2. Start the server

```sh
bin/crowdvm-server serve --listen :8420 --public-url http://192.168.1.10:8420
bin/crowdvm-server user create --username alice --password '<password>' --admin
bin/crowdvm-server downloads import dist     # lets other machines install from this server
```

- `--public-url` is the address users, nodes and sandboxes use. Set it to an address
  every machine can reach (a LAN IP or a Tailscale IP). Without it the server picks its
  LAN IP.
- Sign-up is closed by default: the first user, then invite codes
  (`crowdvm-server invite create`). `--open-signup` lets anyone sign up.
- New accounts get 10 credits (`--signup-credits`). Credits are tracked but not enforced
  unless you pass `--enforce-credits`. See [Node owner guide](node-owners.md#credits).
- Data lives in `--data-dir` (default `~/.local/share/crowdvm/server`). Back it up.
- `--tls-cert` and `--tls-key` turn on HTTPS. Without them everything is plain HTTP, so keep
  the server on a network you trust (LAN or tailnet).
- Webhook and OpenTelemetry URLs may point at LAN and Tailscale addresses, but not at the
  server's own loopback, link-local or cloud-metadata addresses. `--webhook-allow-local`
  allows those too (for a receiver on the server machine).
- Custom AI APIs (`crowdvm ai profile set-api`) must be public HTTPS hosts unless you pass
  `--ai-custom-api-allow-private`, which also allows `http://`, ports and LAN hosts.

Run `crowdvm-server serve -h` for every flag (rate limits, SMTP for login codes, public
URL domain, snapshot interval).

## 3. Add a node on Linux

Requirements: x86-64, QEMU (`qemu-system-x86_64`, `qemu-img`) and a writable `/dev/kvm`
(`sudo usermod -aG kvm $USER`, then log in again). A node without KVM works but is slow
and is always ranked bronze.

```sh
curl -fsSL http://192.168.1.10:8420/install | sh -s -- --node   # crowdvm, crowdvm-node, crowdvm-guest
crowdvm-server join-token create                                 # on the server; or: crowdvm nodes join-token (admins)
crowdvm-node join --server http://192.168.1.10:8420 --token cvj_...
crowdvm-node install-service                                     # systemd user unit; or run it in the foreground: crowdvm-node run
```

`join` benchmarks the machine and registers it (`--skip-bench` defers the benchmark to the first start). By default the node
shares half the CPUs and half the RAM. Change that with `join` flags (`--cpus 8
--memory 16G --disk 200G`) or later with `crowdvm-node config set`. See
[Node owner guide](node-owners.md).

The first node also needs the base image. Build it once from Ubuntu's cloud image and
push it to the server; other nodes download it from the server when they need it:

```sh
curl -LO https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
crowdvm-node image build --cloud-image noble-server-cloudimg-amd64.img
crowdvm-node image list
crowdvm-node image push ubuntu-24.04-crowdvm-v<ver>
```

`crowdvm-node status` shows the connection, the shared capacity and the running VMs.
A local status page runs at `http://127.0.0.1:8421`.

## 4. Add a node on Windows

Requirements: x86-64 Windows 10/11, [QEMU for Windows](https://qemu.weilnetz.de) on
the `PATH`, and the **Windows Hypervisor Platform** feature turned on (Settings → Optional
features → More Windows features). Without it VMs run in software emulation.

In PowerShell:

```powershell
$env:CROWDVM_NODE=1; irm http://192.168.1.10:8420/install.ps1 | iex
crowdvm-node join --server http://192.168.1.10:8420 --token cvj_...
crowdvm-node install-service            # in an elevated prompt: a Windows service, runs without a logged-in user
crowdvm-node install-service --logon-task   # no elevation: starts when you log in
```

The service logs to `node.log` in the node's data directory. Remove it with
`crowdvm-node uninstall-service`.

## 5. Use it from any machine

```sh
curl -fsSL http://192.168.1.10:8420/install | sh     # Linux and macOS
irm http://192.168.1.10:8420/install.ps1 | iex       # Windows
crowdvm login                                         # opens the browser to approve this device
crowdvm new                                           # waits until the sandbox is ready
crowdvm ssh bx_...
```

The installer saves the server URL, adds a `crowdvm` shell function (each terminal
keeps its own current sandbox) and runs `crowdvm onboard`. Coding agents: see
[The CLI for agents](cli-for-agents.md).

## Tailscale

CrowdVM works the same over a tailnet. Use the server's Tailscale address everywhere:

```sh
crowdvm-server serve --listen :8420 --public-url http://100.101.102.103:8420
crowdvm-node join --server http://100.101.102.103:8420 --token cvj_...
crowdvm login --server http://100.101.102.103:8420
```

- Every node and every user must be on the tailnet (or on the server's LAN).
- A MagicDNS name (`http://crowdvm-server:8420`) works for the API, but MagicDNS has no
  wildcard records, so public URLs of the form `<id>-<port>.<domain>` need another
  domain. The default, `<server-ip>.nip.io`, works with a Tailscale IP as long as clients
  can resolve public DNS. Otherwise use the path form `http://<server>/p/<id>/<port>/`.
  See [Public URLs](public-urls.md).
- Sandboxes cannot reach tailnet addresses (`100.64.0.0/10`) except the CrowdVM server.
  See [Security model](security.md#network).
- `crowdvm-node join --direct-host 100.x.y.z` publishes per-sandbox SSH ports on the
  node's Tailscale IP, so `ssh` can skip the server hop.

## A local cluster for development

```sh
scripts/dev-cluster.sh up --nodes 2                    # server on :8420 plus two nodes, data in ~/.local/share/crowdvm-dev
scripts/dev-cluster.sh up --nodes 1 --driver process   # no QEMU: "VMs" are host processes, without isolation
scripts/dev-cluster.sh down
```
