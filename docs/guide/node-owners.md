# Node owner guide

A node is a machine that lends CPU, RAM and disk to the pool. You decide how much,
and you can take it back at any time. Setup is in
[Getting started](getting-started.md#3-add-a-node-on-linux); this page covers
running a node.

## Benchmark, categories and tier

`crowdvm-node join` benchmarks the machine and sends the result to the server (with
`--skip-bench`, the first start does it). Run it again with `crowdvm-node bench` (`--quick` takes under 5 seconds and is less
accurate). It measures:

| Dimension | Measure | medium from | high from | extreme from |
|---|---|---|---|---|
| cpu | multi-thread work units/s | 8,000 | 20,000 | 60,000 |
| memory | total GiB | 8 | 32 | 128 |
| disk | sequential write MB/s | 300 | 1,000 | 3,000 |
| network | min(down, up) Mbps to the server | 100 | 1,000 | 5,000 |
| gpu | VRAM GiB | 4 | 8 | 20 |

Below "medium" a dimension is "low". Disk is also "low" with less than 50 GiB free;
network is "none" when it was not measured; gpu is "none" without a GPU. For scale: a
Ryzen 7 5700X does about 43,000 CPU units/s, a 4-core laptop about 12,000.

The overall **score** (0 to 1000) is a weighted mean: cpu 35 %, memory 20 %, disk 20 %,
network 15 %, gpu 10 %. Each part is log-scaled between a floor and a ceiling. Without
hardware virtualization the score is halved (software emulation) or quartered (none).

| Tier | Score |
|---|---|
| bronze | below 250, or no hardware virtualization |
| silver | 250 to 499 |
| gold | 500 to 749 |
| platinum | 750 and up |

Users ask for nodes with `crowdvm new --min-tier gold` or `--category cpu=high`. The
scheduler also places by `--gpu` (a GPU you pass through, see below), `--nested` (KVM
inside the sandbox), `--node` and `--node-label`. Set labels with `crowdvm nodes update <node> --label room=2 --label
gpu=a100` (this replaces all labels; `--label ""` removes them all).

## Capacity

By default a node shares half its logical CPUs, half its RAM and 90 % of the disk
space its data directory can use. Change it with `join`/`run` flags or later:

```sh
crowdvm-node config set cpus 8
crowdvm-node config set memory 24G
crowdvm-node config set disk 300G
crowdvm-node config set max-sandboxes 4      # default: cpus / 2
crowdvm-node config get                       # all settings
```

Restart the daemon to apply (`systemctl --user restart crowdvm-node`, or restart the
Windows service). Running sandboxes keep running through the restart; the node picks
them up again when it is back. Pass `""` to go back to a default. (A node installed
before this behavior: run `crowdvm-node install-service` once to update its unit.)

Disks are thin: a sandbox only uses the space it wrote. The node therefore accepts
sandbox disk sizes that add up to `disk × disk-overcommit` (default 4). It still
refuses new VMs, and stops downloading snapshots, before it eats into `disk-reserve`
(default 10G of free space on your drive).

## Network policy

Sandboxes reach the internet and the CrowdVM server, but not your machine, your LAN or
your tailnet ([Security model](security.md#network) has the full table). To open more:

```sh
crowdvm-node config set net.allow-lan true                  # all private ranges: RFC 1918, 100.64/10, IPv6 ULA
crowdvm-node config set net.allow "192.168.1.50/32,10.0.5.0/24"
crowdvm-node config set net.deny "203.0.113.0/24"           # block parts of the internet
```

`net.allow` and `net.deny` take comma-separated CIDRs or addresses; the longest
matching prefix wins. Your machine's own addresses only open with an exact `/32`.
The change applies after a restart. Prefer opening one host over `net.allow-lan`.

## GPUs

The benchmark lists your GPUs, but a sandbox only gets one you pass through to it.
`crowdvm new --gpu` lands only on nodes with a free passed-through GPU, one sandbox per
GPU. Passthrough uses VFIO, so the GPU belongs to the sandbox while it runs: your desktop
can't use it, so lend a second GPU, not the one your screen runs on. Linux only.

1. Turn the IOMMU on: VT-d (Intel) or AMD-Vi/IOMMU in the firmware, and boot with
   `intel_iommu=on iommu=pt` (Intel) or `amd_iommu=on iommu=pt` (AMD).
2. Bind every device of the GPU's IOMMU group (usually the GPU and its HDMI audio) to
   `vfio-pci`, e.g. `sudo driverctl set-override 0000:0b:00.0 vfio-pci` and the same for
   `0000:0b:00.1`. `lspci -D` lists addresses; `ls /sys/bus/pci/devices/0000:0b:00.0/iommu_group/devices`
   shows the group.
3. Give the node's user the group device (`/dev/vfio/<group>`, e.g. a udev rule
   `SUBSYSTEM=="vfio", OWNER="<user>"`) and an unlimited memlock limit: VFIO pins all
   RAM of the sandbox (`LimitMEMLOCK=infinity` in the service unit, or
   `/etc/security/limits.conf`).
4. `crowdvm-node config set gpu.passthrough 0000:0b:00.0` (comma-separate several GPUs)
   and restart the node.

The node checks all of this when it starts. If anything is missing it does not start
and says which device and what to change, for example
`gpu.passthrough 0000:0b:00.0: it is bound to nvidia, not vfio-pci`. Unset it with
`crowdvm-node config set gpu.passthrough ""`.

**3D desktops without passthrough:** `crowdvm-node config set gpu.virgl on` gives every
sandbox a virtio-gpu with OpenGL that renders on your GPU (virgl). The sandbox sees
`/dev/dri/renderD128`; your GPU stays yours. It gives OpenGL/EGL, not CUDA, and does
not count as `--gpu`. Needs a render node your user can open and a QEMU with
virglrenderer; the node checks both at start.

## Credits

Credits replace billing. One credit is one hour of a `default` sandbox. A running
sandbox spends its size's rate per hour (small 0.5, default 1, large 2, xlarge about
5.6) from the wallet of whoever started it, and the owner of the node that hosts it
earns that amount times the server's earn rate (default 1, `--node-earn-rate`).

```sh
crowdvm credits                  # balance and recent entries
crowdvm nodes info <node>        # what a node hosts and earned
```

The dashboard's Credits & usage page shows the daily balance of earned and spent
credits. Admins can grant credits (`crowdvm-server user grant-credits`). Unless the
server runs with `--enforce-credits`, a negative balance blocks nothing.

## Day to day

| Task | Command |
|---|---|
| See what runs here | `crowdvm-node status`, or the status page at `http://127.0.0.1:8421` |
| Stop taking new sandboxes | `crowdvm-node config set paused true` (applies at once; running sandboxes keep running), or `crowdvm nodes drain <node>` |
| Take new sandboxes again | Clear a local pause on the node with `crowdvm-node config set paused false`; clear a server drain with `crowdvm nodes undrain <node>` |
| Remove the node | `crowdvm nodes remove <node>` (`--force` while it still hosts sandboxes), then `crowdvm-node uninstall-service` |
| Rename it | `crowdvm nodes update <node> --name lab-gpu` |

While it hosts sandboxes, a Linux node keeps the machine from going to sleep on its
own (a systemd "sleep" inhibitor, listed by `systemd-inhibit --list`). Closing a
laptop's lid still suspends it, and a node without sandboxes sleeps as usual. Turn it
off with `crowdvm-node config set keep-awake false` and a restart.

Sandboxes on a node that goes offline for more than 3 minutes are rebuilt on other
nodes from their latest snapshot. A sandbox pinned to that node (`--node`, labels) is
not: it waits for the node, and its owner gets a notice. `crowdvm resume <id> --node
<other>` moves it from its latest snapshot. Data written after that snapshot (up to about a
minute) is lost, so drain a node before you shut it down for long.

Removing a node that is online erases the snapshot copies it holds that the server
no longer needs or also stores. Removing it while it is offline cannot erase
anything: deletions of data it held then report `blocked` with `node_removed` in
`lastError`. Wipe its data dir by hand in that case (uninstalling the service does
not).

Node data lives in `--data-dir` (default `~/.local/share/crowdvm/node`, on Windows
`%LOCALAPPDATA%\CrowdVM\node`): images, cached snapshots and the disks of running VMs. None of it
is encrypted. Treat the folder as other people's data.
