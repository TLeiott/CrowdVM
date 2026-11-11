#!/bin/bash
# Full profile: the Boat-like toolset on top of minimal.
set -Eeuxo pipefail
export TAR_OPTIONS=--no-same-owner # tarballs (Node) must not leave uid 1001 files
here=$(cd "$(dirname "$0")" && pwd)
apt_get() { apt-get -o DPkg::Lock::Timeout=600 -y "$@"; }
dl=$(mktemp -d)
keyrings=/etc/apt/keyrings
install -d -m 0755 "$keyrings"
arch=$(dpkg --print-architecture)
. /etc/os-release

apt_get install --no-install-recommends software-properties-common

# --- apt repositories: Docker, GitHub CLI, Ghostty (community PPA) -----------
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o "$keyrings/docker.asc"
echo "deb [arch=$arch signed-by=$keyrings/docker.asc] https://download.docker.com/linux/ubuntu $VERSION_CODENAME stable" \
	>/etc/apt/sources.list.d/docker.list
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o "$keyrings/githubcli.gpg"
echo "deb [arch=$arch signed-by=$keyrings/githubcli.gpg] https://cli.github.com/packages stable main" \
	>/etc/apt/sources.list.d/github-cli.list
# Ghostty has no official .deb; mkasberg/ghostty-ubuntu packages it for noble.
ghostty=1
add-apt-repository -y -n ppa:mkasberg/ghostty-ubuntu || ghostty=0

apt_get update
apt_get upgrade

# --- base dev tools, Python, Docker, desktop -----------------------------------
apt_get install --no-install-recommends \
	build-essential pkg-config libssl-dev git git-lfs jq ripgrep fd-find zip unzip xz-utils zstd \
	wget rsync file tree htop tmux screen lsof strace psmisc dnsutils netcat-openbsd openssh-client \
	man-db sqlite3 bash-completion \
	python3 python3-pip python3-venv python3-dev pipx python-is-python3 \
	docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
	gh \
	xfce4 xfce4-terminal xfce4-notifyd dbus-x11 at-spi2-core \
	tigervnc-standalone-server tigervnc-tools xdotool wmctrl xclip xsel x11-utils x11-xserver-utils \
	ffmpeg scrot imagemagick fonts-dejavu fonts-liberation fonts-noto-color-emoji \
	xdg-utils libnss3 libgbm1 libasound2t64
# No Wine: Boat ships none, and Ubuntu's wine would conflict with winehq-staging
# from Boat's install-wine.sh (docs: windows-apps/install-wine).
if [ "$ghostty" = 1 ]; then apt_get install ghostty || ghostty=0; fi
[ "$ghostty" = 1 ] || echo "WARNING: ghostty not installed; xfce4-terminal is the terminal" >&2
usermod -aG docker user

# --- Google Chrome and VS Code (official .debs; they add their own apt repos) -
curl -fsSL -o "$dl/chrome.deb" https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
curl -fsSL -o "$dl/code.deb" "https://update.code.visualstudio.com/latest/linux-deb-x64/stable"
echo "code code/add-microsoft-repo boolean true" | debconf-set-selections
apt_get install "$dl/chrome.deb" "$dl/code.deb"

# --- Node.js (latest LTS, official build) + global CLIs ----------------------
node=$(curl -fsSL https://nodejs.org/dist/index.json | jq -r '[.[] | select(.lts != false)][0].version')
curl -fsSL "https://nodejs.org/dist/$node/node-$node-linux-x64.tar.xz" |
	tar -xJ -C /usr/local --strip-components=1 --exclude='*/CHANGELOG.md' --exclude='*/README.md' --exclude='*/LICENSE' --exclude='*/doc'
npm install -g --no-fund --no-audit npm@latest pnpm \
	@anthropic-ai/claude-code @openai/codex opencode-ai

# --- Bun (baseline build: runs on CPUs without AVX2 too) ----------------------
curl -fsSL -o "$dl/bun.zip" https://github.com/oven-sh/bun/releases/latest/download/bun-linux-x64-baseline.zip
unzip -q -o "$dl/bun.zip" -d "$dl"
install -m 0755 "$dl"/bun-linux-x64-baseline/bun /usr/local/bin/bun
ln -sf bun /usr/local/bin/bunx

# --- uv ----------------------------------------------------------------------
curl -LsSf https://astral.sh/uv/install.sh | env UV_UNMANAGED_INSTALL=/usr/local/bin sh

# --- Rust (rustup, for user) ---------------------------------------------------
sudo -iu user sh -c 'curl -fsSL https://sh.rustup.rs | sh -s -- -y --profile minimal -c rustfmt,clippy'

# --- More languages (Go, Deno, JVM, Ruby, PHP, Erlang/Elixir, .NET, R, clang, nvm)
bash "$here/langs.sh"

# --- Desktop (Xvnc :0 + Xfce), up from boot like Boat's -----------------------
cp -r "$here/../rootfs/full/." /
systemctl enable crowdvm-desktop.service
ln -sf ascii-record-desktop /usr/local/bin/crowdvm-record-desktop
# The browser-only view runs Chrome as this account, which can't read /home/user.
useradd --system --create-home --home-dir /var/lib/crowdvm-browser --shell /usr/sbin/nologin \
	--user-group --comment "CrowdVM browser view" crowdvm-browser
chmod 0700 /var/lib/crowdvm-browser
# Chrome without first-run noise; the default browser for xdg-open. Chrome 15x
# opens an "Additional Terms of Service" dialog on every new profile unless
# initial_preferences says require_eula false (the guest agent writes the same
# file on older images: internal/guest/chrome.go).
install -d /etc/opt/chrome/policies/managed
cat >/etc/opt/chrome/policies/managed/crowdvm.json <<'EOF'
{"DefaultBrowserSettingEnabled": false, "MetricsReportingEnabled": false, "PromotionalTabsEnabled": false,
 "PromotionsEnabled": false, "PrivacySandboxPromptEnabled": false}
EOF
cat >/opt/google/chrome/initial_preferences <<'EOF'
{"distribution": {"require_eula": false, "skip_first_run_ui": true, "suppress_first_run_default_browser_prompt": true}}
EOF
sudo -iu user xdg-settings set default-web-browser google-chrome.desktop || true
# A desktop must never lock or blank.
apt_get purge light-locker xfce4-screensaver xscreensaver 2>/dev/null || true

# --- Coding agents (pi, kimi, vibe, hermes, openclaw, prime-agent, cursor) and cua-driver
bash "$here/agents.sh"

# Docker starts with the machine but never blocks the guest agent.
systemctl enable docker.service containerd.service
rm -rf "$dl"
