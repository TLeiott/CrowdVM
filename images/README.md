# CrowdVM base images

Provisioning assets for the CrowdVM base images. `internal/node/image` embeds
this directory (`images.go`) and builds the images; the node exposes that as
`crowdvm-node image build|list`.

```
scripts/build.sh <profile>   entry point (cloud-init runcmd), reports on /dev/ttyS1, powers off
scripts/minimal.sh           user, ssh, guest bootstrap unit, network, fast boot
scripts/full.sh              the Boat-like toolset (calls langs.sh and agents.sh)
scripts/langs.sh             Go, Deno, JVM, Ruby, PHP, Erlang/Elixir, .NET, R, clang, nvm
scripts/agents.sh            the other agent CLIs and cua-driver
scripts/tools.sh             "tool=version" lines -> <image>.json
scripts/finalize.sh          cloud-init off, identity wiped, caches cleaned, fstrim
rootfs/<profile>/            files copied verbatim into /
```

## How a build works

1. qcow2 overlay on the Ubuntu 24.04 cloud image (never modified), resized to
   4 GiB (minimal) or 24 GiB (full). The guest agent grows it to the VM disk size.
2. NoCloud seed labelled `cidata`: `xorriso` when installed, else the pure-Go
   `image.WriteISO`. Both paths are tested end to end. The user-data is a
   `#cloud-config` written as JSON. It holds every file of this directory as
   `write_files` under `/var/lib/crowdvm-build`, plus `runcmd: build.sh <profile>`.
3. Headless `qemu-system-x86_64` (q35, KVM, else TCG with `-cpu max`). The
   guest serial console streams to `Options.Log`. A second serial port goes to
   a host file that receives `tool=version` lines and then `CROWDVM-BUILD-OK`
   or `CROWDVM-BUILD-FAILED line N: cmd`. The build fails on timeout, on a
   failure marker, or when the marker is missing. The work dir with
   `console.log` is kept for debugging.
4. `qemu-img convert -c -o compression_type=zstd` creates a standalone
   `<OutDir>/<id>.qcow2` and `<id>.json` (id, profile, version, sizes, sha256,
   accel, build time, tools). Both are written as `.part` and then renamed.

## Profiles

**minimal** (`ubuntu-24.04-crowdvm-minimal-v3`, used for tests; same content as v2)

* User `user` with uid 1000, home `/home/user` and passwordless sudo, in the
  groups `adm sudo kvm systemd-journal`. The cloud image's `ubuntu` user is removed.
* `openssh-server` with socket activation (`ssh.socket`). The image ships no
  host keys. The guest agent makes them on create/fork, and an `ssh.service`
  drop-in runs `ssh-keygen -A` as a fallback.
* `crowdvm-guest.service` (enabled), as in ARCHITECTURE.md "Guest bootstrap".
  `ExecStartPre=/usr/local/lib/crowdvm/guest-prepare` waits up to 30 s for
  `/dev/disk/by-label/CROWDVM`, mounts it read-only at `/run/crowdvm/cfg` and
  installs `crowdvm-guest` atomically into `/usr/local/bin`. `ExecStart` runs
  `crowdvm-guest --config /run/crowdvm/cfg/config.json` as root with
  `Restart=always`, `RestartSec=1` and no start limit. `KillMode=process`
  keeps user commands alive when the agent restarts, and `OOMScoreAdjust=-1000`
  keeps the OOM killer off the agent (the agent resets its children to 0
  and limits `user.slice` to RAM − 512 MiB at boot). Both ISO9660 and FAT
  config drives work: `isofs` autoloads and `vfat` is built in.
* netplan `/etc/netplan/01-crowdvm.yaml`: DHCPv4 on `en*`, `optional: true`.
  The cloud-init netplan file is removed.
* **Firewall (Boat):** `ufw` is enabled with deny incoming, allow outgoing,
  and allow `22/tcp` (ssh) and `7070/tcp` (guest agent). Docker publishes
  ports outside ufw, as usual. `finalize.sh` saves `/etc/ufw` and
  `/etc/default/ufw` to `/usr/local/lib/crowdvm/ufw-default.tar`.
  `crowdvm-firewall-reset.service` runs `/usr/local/lib/crowdvm/firewall-reset`
  on every boot before `ufw.service`, so a resume or a fork never keeps rules
  from a snapshot (Boat: "Snapshots do not keep firewall rules"). The guest
  agent leaves the firewall to this unit. It calls
  `firewall-reset <port>/tcp` only when it listens on a port other than 7070.
  Running `firewall-reset [SPEC ...]` later restores the defaults, allows
  each extra SPEC, and runs `ufw reload` if ufw is running.
* `/home/user` is mode 0750, so other accounts (`crowdvm-browser`) can't read it.
* The hostname is `crowdvm`. `/etc/machine-id` is empty, so every new VM
  gets a fresh one on first boot.
* Locale `en_US.UTF-8` (LANG and LC_ALL), the same as Boat.
* journald is capped at 64 MiB on disk and 32 MiB at runtime.
* cloud-init is disabled (`/etc/cloud/cloud-init.disabled`) and cleaned.
* Packages purged: snapd (pinned so it can't come back), multipath-tools,
  open-iscsi, lxd-installer, apport, popularity-contest, unattended-upgrades,
  landscape, fwupd, packagekit, modemmanager, pollinate, ubuntu-pro-client,
  plymouth, needrestart, udisks2.
* Units masked: wait-online, apt-daily, motd-news, man-db, e2scrub, the
  fstrim timer (prepare-stop runs fstrim), dpkg-db-backup, pcrphase,
  console/keyboard setup.
* Boot: grub timeout 0 even after an unclean shutdown
  (`GRUB_RECORDFAIL_TIMEOUT=0`), `quiet`, serial console on ttyS0.
  `GRUB_FORCE_PARTUUID` boots **without the initramfs**, which saves about
  1.6 s. virtio-blk, virtio-scsi and ext4 are built into the kernel. If a
  boot without the initramfs fails (for example a root disk on AHCI or NVMe),
  Ubuntu's grub uses `panic=-1` and boots with the initramfs on the next try.
  This was tested with an AHCI root disk. **A VM driver must not run qemu with
  `-no-reboot`, or the fallback can't happen.** The qemu driver uses
  virtio-blk and allows reboots.
* Also installed: `growpart`/`resize2fs` (cloud-guest-utils, e2fsprogs),
  curl, gnupg, vim-tiny, nano, ufw.
* apt lists are kept, so `sudo apt-get install -y X` works at once.

**full** (`ubuntu-24.04-crowdvm-v3`): minimal plus `apt-get upgrade` and
Boat's toolset (docs: machines/tools). Everything is system-wide in
`/usr/local` and `/opt`, as on Boat. The exceptions are Rust (rustup for
`user`, as specified), plus nvm and Ruby, which `user` owns so that
`nvm install` and `gem install` work without sudo. Each line resolves to its
newest release. The versions below come from the build on 2026-11-08 and are
recorded in the image's `.json`.

| Tool | Version | Source |
|---|---|---|
| Docker CE + buildx + compose, containerd | 29.9.3 / 0.38.1 / 5.7.0 / 2.4.2 | download.docker.com apt; `user` in `docker` |
| Node.js (latest LTS) + npm, pnpm | 26.13.0 / 12.2.2 / 12.18.0 | nodejs.org tarball in `/usr/local` |
| nvm + Node 22 | nvm 0.41.0, node 22.23.4 | `/usr/local/nvm`, `/etc/profile.d/nvm.sh` (`--no-use`: the default node stays the LTS) |
| Bun | 1.4.4 | GitHub release, **baseline** build (runs without AVX2) |
| Deno | 2.9.8 | GitHub release, `/usr/local/bin` |
| Python 3 + pip + venv + pipx, uv | 3.12.3 / 24.0 / 0.13.7 | Ubuntu; uv installer into `/usr/local/bin` |
| Go | 1.27.3 | go.dev tarball, `/usr/local/go` |
| Rust (rustup, minimal profile + rustfmt, clippy) | 1.99.1 | `~user/.cargo` |
| OpenJDK 11, Maven, Gradle | 11.0.32.1 / 3.8.7 / 8.14.6 | Ubuntu (JDK, Maven); Gradle 8.x zip in `/opt/gradle` (Gradle 9 needs JDK 17) |
| Kotlin | 2.4.21 | GitHub compiler zip, `/opt/kotlinc` |
| Scala 3.3 LTS, sbt 1.x | 3.3.9 / 1.13.1 | GitHub tarballs, `/opt/scala3`, `/opt/sbt` |
| Ruby + gem + bundle | 4.0.9 / 4.0.20 | ruby/ruby-builder prebuilt for 24.04 (its fixed prefix `/opt/hostedtoolcache/Ruby/…`), symlinked into `/usr/local/bin` |
| PHP + composer | 8.4.27 / 2.11.1 | `ppa:ondrej/php` (Boat ships this repo; see Wine below); composer installer, verified |
| Erlang/OTP, Elixir | 25.3 / 1.14.0 | Ubuntu (exactly Boat's versions) |
| .NET SDK | 8.0.133 | Ubuntu `dotnet-sdk-8.0`; `DOTNET_CLI_TELEMETRY_OPTOUT=1` in `/etc/environment` |
| R | 4.6.2 | CRAN apt repo (`r-base-core`, `r-base-dev`) |
| gcc, clang, cmake, ninja | 13.3 / 18.1.3 / 3.28.3 / 1.11.1 | Ubuntu |
| GitHub CLI | 2.103.3 | cli.github.com apt |
| Claude Code, Codex, OpenCode | 2.1.319 / 0.167.0 / 1.19.5 | `npm i -g` |
| pi, Kimi Code (`kimi`), OpenClaw | 1.5.0 / 2.5.2 / 2026.11.2 | `npm i -g @earendil-works/pi-coding-agent @moonshot-ai/kimi-code openclaw` |
| Mistral Vibe (`vibe`, `vibe-acp`), Hermes Agent (`hermes`) | 3.1.0 / 0.19.0 | `uv tool install` into `/opt/uv-tools`, binaries in `/usr/local/bin`, system Python 3.12 |
| Prime Agent (`prime-agent`) | 0.10.2 | official installer with prefix `/usr/local` (launcher `/usr/local/bin/prime-agent`, payload `/usr/local/share/prime-agent`). `user`'s Python kernel venv (`~/.prime/agent`) is pre-warmed. `PRIME_AGENT_TELEMETRY=0` |
| Cursor CLI (`agent`, `cursor-agent`) | 2026.11.05 | the package the official installer downloads, in `/opt/cursor-agent` |
| Cua Driver (`cua-driver`) | 0.44.0 | trycua/cua `cua-driver-rs` Linux binary in `/opt/crowdvm/cua-driver` (+ `/opt/ascii/cua-driver`, `/usr/local/bin/cua-driver`); telemetry off for `user` |
| Google Chrome | 157 | official .deb (+ its apt repo) |
| VS Code | 1.145.2 | official .deb (+ Microsoft apt repo) |
| Ghostty | 1.3.1 | community PPA `mkasberg/ghostty-ubuntu`; no official .deb exists. If the PPA fails, the build goes on and xfce4-terminal is the terminal |
| Xfce 4.18 + xfce4-terminal, TigerVNC (Xvnc) | 1.13.1 | Ubuntu, `--no-install-recommends`, no display manager, no screen locker |
| xdotool, wmctrl, xclip, ffmpeg, scrot, ImageMagick | ffmpeg 6.1.1 | Ubuntu |
| build-essential, git, git-lfs, jq, ripgrep, fd, tmux, htop, strace, sqlite3, … | git 2.43, jq 1.7, rg 14.1 | Ubuntu |

Not installed: Boat's `lux`. It is Ascii's proprietary desktop agent, driven by
their hosted Lux model, and is not published anywhere. v3 ships
`/usr/local/bin/lux` as a pointer instead: it prints the CrowdVM equivalents
(the `computer` MCP tools / cua-driver, xdotool, `ascii-record-desktop`,
`crowdvm prompt`) to stderr and exits 127. There is no apt Node 20 either:
noble ships Node 18, and nvm covers older Node versions. There is no Wine,
the same as Boat (see below).

**Desktop.** `crowdvm-desktop.service` is **enabled**, so the desktop is up
from boot, as on Boat. It runs `Xvnc :0` with VNC on `127.0.0.1:5900` and no
auth (reached through the guest tunnel), plus an Xfce session as `user`.
Blanking and DPMS are off. The size comes from `CROWDVM_DESKTOP_GEOMETRY` in
`/run/crowdvm/desktop.env` (default `1920x1080`). The guest agent's
`/v1/internal/desktop/start` still starts it if it is down, and the guest
sets `DISPLAY=:0` for commands. Chrome has a managed policy that turns off
the default-browser prompt, metrics, promotions and the Privacy Sandbox
prompt. Since v3, `/opt/google/chrome/initial_preferences` sets
`require_eula: false` and `skip_first_run_ui`: Chrome 157 otherwise opens a
"Google Chrome and ChromeOS Additional Terms of Service" dialog on every new
profile, which blocked agents asked to "open example.com" (verified: without
the file the dialog appears; with it, a fresh profile opens the page
directly; `--no-first-run` alone also works, but desktop launches don't pass
it). The guest agent writes the same file at boot on v1/v2 images.

**Computer use (Boat: integrated-agents/computer-use).** `cua-driver.service`
is a systemd **user** unit (`/usr/lib/systemd/user`, enabled with
`--global`). `user` lingers, so the unit starts at boot and again after every
resume or fork. It waits for `/tmp/.X11-unix/X0` and then runs `cua-driver
serve --socket /run/ascii-cua/driver.sock --dangerously-bypass-approvals`.
The bypass is needed because nobody can approve prompts in a headless
sandbox. `/run/ascii-cua` comes from tmpfiles, so it is never stale after a
resume. Harnesses use `cua-driver mcp --socket /run/ascii-cua/driver.sock`
as the stdio MCP server `computer`, and guest-agents writes the per-harness
config. Boat's check commands work unchanged:
`systemctl --user status cua-driver.service` and
`/opt/ascii/cua-driver/cua-driver status --socket /run/ascii-cua/driver.sock`.

**Recording.** `ascii-record-desktop start <file.mp4> | stop | status` and
`crowdvm-record-desktop` (the same tool) run `ffmpeg -f x11grab` at 30 fps
(H.264 MP4) as a transient systemd unit. The unit runs under the caller's
user manager, or as a system unit for root, so the recording outlives the
command that started it. `stop` sends SIGINT, so the MP4 is finalized.

**Browser view (DESK-017).** The system user `crowdvm-browser` (home
`/var/lib/crowdvm-browser` 0700, shell nologin, no sudo, not in `docker`)
runs the browser-only view's Chrome. It can't read `/home/user` (0750). The
guest also mounts a tmpfs over `/home/user` in its namespace.

**In-sandbox CLI.** The image ships no `crowdvm` binary. If the config drive
carries a `crowdvm` file (node option `--cli-binary`), the guest installs it
every boot as `/usr/local/bin/crowdvm` with the symlinks `host`, `boat` and
`box`.

**Wine (Boat: windows-apps/install-wine).** The image ships no Wine. Boat's
`install-wine.sh` runs **unchanged** and installs WineHQ staging with i386
multiarch, mesa Vulkan, winetricks, corefonts, vcrun2022, dxvk and
d3dcompiler_47. Ubuntu's `wine`/`wine64` (v1) would have conflicted with
`winehq-staging`. The ondrej PHP PPA is present, so the script's `libgd3`
pin does what the docs say (PHP's `gd` extension is removed as a side
effect). `CROWDVM_E2E_WINE=1` runs the script plus a 64-bit and a 32-bit
(syswow64) program.

**Skills (AGT-025).** The image carries no skill files. The guest agent
writes the repo's `skills/crowdvm` skill into every harness's skills
directory at each boot (see `internal/guest/README.md`), so v1/v2 images get
it too and a new guest brings new text.

**v3 versus v2:** Chrome `initial_preferences` and the wider policy, and the
`lux` pointer. Everything else (Docker restart after resume/fork, skills,
direct-port firewall rules, not-saved cleanup of forks) is guest behaviour
and works on v2 as well. v2 files are kept.

## Results (this machine: Ryzen 7 5700X, KVM, build VM 8 vCPU / 8 GiB)

| Image | qcow2 (zstd) | Virtual | Build | Boot-to-health, stand-in agent | Boot-to-health, real `crowdvm-guest` |
|---|---|---|---|---|---|
| `ubuntu-24.04-crowdvm-minimal-v3` | 555 MiB | 4 GiB | 49 s | 2.51 s | – |
| `ubuntu-24.04-crowdvm-v3` | 4309 MiB | 24 GiB | 6 min 3 s | – | 2.51 s (2 boots) |
| `ubuntu-24.04-crowdvm-minimal-v2` | 555 MiB | 4 GiB | 51 s | 2.51 s | 2.50–2.51 s (3 boots) |
| `ubuntu-24.04-crowdvm-v2` | 4305 MiB | 24 GiB | 5 min 46 s | 2.50 s | 2.50–3.11 s (3 boots) |
| `ubuntu-24.04-crowdvm-minimal-v1` | 557 MiB | 4 GiB | 47–50 s | 1.9–2.5 s | 2.7–3.2 s |
| `ubuntu-24.04-crowdvm-v1` | 2755 MiB | 24 GiB | 3 min 29 s | 1.9–2.6 s | 2.7–3.4 s |

v2 versus v1 for full: +1550 MiB compressed and +2 min 17 s build. Measured
in a VM, the languages add about 2.7 GB on disk and 170 s, and the agents
plus cua-driver about 2.3 GB and 50 s. The biggest items are .NET, the JDK,
clang, OpenClaw (650 MB), Cursor (540 MB) and Mistral Vibe (320 MB). The
v1 files are kept next to v2.

Boot-to-health is the time from starting qemu to the first `200` on
`GET /v1/health` through hostfwd. The VM has 4 vCPU and 8 GiB, polling is
every ≤0.6 s, and the boot is a first boot from the base image. Minimal v2
reports `kernel 0.44 s + userspace 1.34 s`. On the critical path,
`crowdvm-guest.service` starts after `basic.target` at about 1.2 s, behind
`systemd-resolved`. `ufw.service` (0.5–0.7 s) runs in parallel and is not on
that path. The desktop, the user manager with cua-driver, and Docker start in
parallel with the agent. They don't delay health, but they use CPU in the
first seconds.

Verified on the full v3 image (2026-11-08): `TestE2EBoot` with all 35
checks, the two new ones being a fresh Chrome profile on the desktop that
opens its page with no Terms dialog, and the `lux` pointer (exit 127);
`TestVMGuestCore` (real guest: create, OOM, CLI, tracer, agent restart,
apply-config, browser view, live snapshot, stop/resume, fork); `TestVMDocker`
(containers with no restart policy and with `unless-stopped` run again after a
fork of the running sandbox and after stop + resume, a never-started one stays
stopped, named volume kept, `busybox` and an unused built image dropped while
`alpine` and a built image a container uses stay, build cache 0 B, `~/.cache`
and `.crowdvmignore` matches gone in the fork, the direct-port firewall rule
added and removed).

Verified on the full v2 image (`TestE2EBoot`, all 33 checks pass):

* `docker run hello-world`, buildx, compose, with ufw active. Container
  egress and `-p` publishing with ufw on were checked by hand in a test VM.
* node, npm, pnpm, bun, deno, `nvm ls 22`
* rustc and cargo as `user`
* python3, pip, uv, venv
* as `user` in a login shell: go, java, mvn, gradle, kotlinc, scalac, sbt,
  ruby, gem, bundle, php, composer, elixir, dotnet, R, clang, cmake, ninja
* compiled and ran: Go and Kotlin hello-world, Ruby, PHP, R, Elixir, Deno,
  and a `gem install` as `user`
* gh, claude, codex, opencode, pi, kimi, vibe, hermes, openclaw,
  prime-agent (as `user`), cursor-agent, agent, cua-driver
* no `wine` in the image
* `crowdvm-browser` exists, has no sudo or docker, and can't list `/home/user`
* `google-chrome --version`, plus headless Chrome rendering a page as `user`
* `code --version`, ghostty, Xvnc, xdotool, ffmpeg, scrot, convert
* `crowdvm-desktop` enabled: 1920x1080 display, `xfce4-session` running,
  screenshot taken, VNC on 127.0.0.1:5900
* `cua-driver.service` (user unit) active, and
  `cua-driver call --socket /run/ascii-cua/driver.sock get_screen_size`
  returns 1920x1080
* `ascii-record-desktop start` / `crowdvm-record-desktop status` /
  `ascii-record-desktop stop` produce a 1920-wide MP4

Verified on both v2 images: SSH answers with fresh host keys; user and sudo,
DHCP address 10.0.2.15, en_US.UTF-8, no snap, cloud-init disabled, growpart
present; the config drive is mounted and the agent installed from it; ufw
active with only 22/tcp and 7070/tcp allowed; `firewall-reset` removes an
added rule and leaves ufw active; `crowdvm-firewall-reset` enabled;
`OOMScoreAdjust=-1000` on the guest unit; `/home/user` is 0750.

Verified with `CROWDVM_E2E_WINE=1` on full v2: Boat's `install-wine.sh` runs
unchanged in about 100 s and installs wine-11.21 (Staging), and
`wine cmd /c echo` (64-bit) and `C:\windows\syswow64\cmd.exe` (32-bit) both
run as `user`.

Verified with the real guest and the real CLI on the config drive (full v2,
through the guest's `POST /v1/commands`):

* `/usr/local/bin/crowdvm` is installed with `host`, `boat` and `box`
  symlinks, and `host --help` works
* commands run as `user` with `DISPLAY=:0`, and `wmctrl -l` lists the Xfce
  windows
* the guest has `oom_score_adj` −1000, its commands have 0, and `user.slice`
  has `MemoryMax` = RAM − 512 MiB
* **reboot (= resume/fork semantics):** a `ufw allow 5555/tcp` and a
  hand-edited rule in `/etc/ufw/user.rules` are both gone after the reboot,
  in `ufw status` and in iptables. ufw is active again, and cua-driver and the
  desktop come back by themselves.

## Reproduce

```sh
CROWDVM_E2E=1 CROWDVM_E2E_BUILD=minimal go test -count=1 -run TestE2EBuild -timeout 1h -v ./internal/node/image
CROWDVM_E2E=1 CROWDVM_E2E_BUILD=full    go test -count=1 -run TestE2EBuild -timeout 3h -v ./internal/node/image
CROWDVM_E2E=1 CROWDVM_E2E_IMAGE=~/.local/share/crowdvm-dev/images/ubuntu-24.04-crowdvm-v3.qcow2 \
  go test -count=1 -run TestE2EBoot -timeout 60m -v ./internal/node/image
CROWDVM_E2E=1 CROWDVM_E2E_IMAGE=~/.local/share/crowdvm-dev/images/ubuntu-24.04-crowdvm-v3.qcow2 \
  go test -count=1 -run 'TestVMGuestCore|TestVMDocker' -timeout 60m -v ./internal/guest
```

Optional variables:

* `CROWDVM_E2E_GUEST=<static crowdvm-guest>` tests the real agent (health only).
* `CROWDVM_E2E_CLI=<static crowdvm>` also puts the in-sandbox CLI on the
  config drive. The real guest installs it.
* `CROWDVM_E2E_WINE=1` (full image) also runs Boat's `install-wine.sh`
  unchanged, then 64-bit and 32-bit Windows programs. This takes about 2 minutes here (wine-11.21 Staging).
* `CROWDVM_E2E_WINE_APPS=1` (full image, implies the above) is WIN-005's
  acceptance test: the downloadable apps of Boat's tested-app list (7-Zip,
  Notepad++, WinMerge, x64dbg, Process Explorer, Autoruns, Everything,
  SkiFree) are fetched, started under Wine on the desktop and must open
  their main window (console tools: print their banner). Apps that need a
  sign-up, an installer UI or have no stable URL are listed in
  `internal/node/image/e2e_test.go` (`wineAppChecks`) and stay manual.
* `CROWDVM_E2E_CHECKS='cmd\ncmd'` runs custom checks through the stand-in.
* `CROWDVM_E2E_NO_XORRISO=1` builds with the pure-Go seed writer.
* `CROWDVM_CLOUD_IMAGE` and `CROWDVM_E2E_OUT` set the input image and the
  output directory.

## Known gaps

* **Ghostty comes from a community PPA**, because no official Ubuntu package
  exists.
* **Version differences from Boat's table:** every line installs its newest
  release, while Boat's table is a snapshot from 2026-10-13. Node 26 LTS
  (Boat: 24), Go 1.27, Kotlin 2.4, Gradle 8.14, Ruby 4.0.9 and cmake 3.28
  (Ubuntu; Boat 3.31) all differ. There is no apt Node 20 (noble has 18).
  `lux` is not available (v3 has a `lux` pointer to the equivalents).
* **Hermes comes from PyPI** (0.19.0). NousResearch's own installer tracks
  git main and may be newer.
* **The npm agents run without their install scripts** (npm 12 blocks them
  by default). pi, kimi and openclaw start. OpenClaw's bundled-plugin
  postinstall did not run, and its plugins are untested.
* **cua-driver is the upstream Linux build**, marked pre-release there. It
  runs in unrestricted mode, since nobody can approve prompts in a headless
  sandbox.
* **Builds depend on the network:** they download the latest Node LTS, Bun,
  uv, rustup, Go, Deno, the JVM tools, Ruby, the agent CLIs, cua-driver,
  Chrome and VS Code. Rebuilds are not byte-identical, and the tool versions
  in the JSON record what was installed. Unauthenticated GitHub API calls
  (5 per build) count against the 60/h rate limit.
* **TCG fallback is untested here.** It is implemented, but this machine has
  KVM, and a full build under TCG would be very slow.
* **Docker, the desktop and the user manager start with the machine,** in
  parallel with the agent. They don't delay health, but they use some CPU in
  the first seconds.
