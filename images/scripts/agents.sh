#!/bin/bash
# Full profile: Boat's coding-agent CLIs and the computer-use driver.
# System-wide in /usr/local and /opt (Boat: "they take no space in your
# snapshots"). claude, codex, opencode come with the Node install in full.sh.
set -Eeuxo pipefail
export TAR_OPTIONS=--no-same-owner
fetch() { curl -fsSL --retry 3 "$@"; }
dl=$(mktemp -d)

# --- npm: pi, Kimi Code, OpenClaw --------------------------------------------
npm install -g --no-fund --no-audit @earendil-works/pi-coding-agent @moonshot-ai/kimi-code openclaw

# --- uv tools (system Python 3.12): Mistral Vibe, Hermes Agent ----------------
export UV_TOOL_DIR=/opt/uv-tools UV_TOOL_BIN_DIR=/usr/local/bin UV_PYTHON_PREFERENCE=only-system
uv tool install --python /usr/bin/python3 mistral-vibe
uv tool install --python /usr/bin/python3 hermes-agent
unset UV_TOOL_DIR UV_TOOL_BIN_DIR UV_PYTHON_PREFERENCE

# --- Prime Agent (official installer; launcher /usr/local/bin/prime-agent) ----
# HOME=/home/user pre-warms user's Python kernel venv (~/.prime/agent), so the
# first prompt works offline; finalize.sh chowns /home/user.
fetch -o "$dl/prime.sh" https://app.primeintellect.ai/prime-agent/install.sh
HOME=/home/user PRIME_AGENT_RUST_PREFIX=/usr/local PRIME_AGENT_UV_BIN_DIR=/usr/local/bin sh "$dl/prime.sh"
chmod -R a+rX /usr/local/share/prime-agent
rm -rf /home/user/.cache
echo PRIME_AGENT_TELEMETRY=0 >>/etc/environment

# --- Cursor CLI (agent / cursor-agent) ------------------------------------------
url=$(fetch https://cursor.com/install | grep -o 'https://downloads.cursor.com/lab/[^"]*/${OS}/${ARCH}/agent-cli-package.tar.gz' | head -1)
url=${url//'${OS}'/linux}
url=${url//'${ARCH}'/x64}
install -d /opt/cursor-agent
fetch "$url" | tar -xz -C /opt/cursor-agent --strip-components=1
ln -sf /opt/cursor-agent/cursor-agent /usr/local/bin/cursor-agent
ln -sf /opt/cursor-agent/cursor-agent /usr/local/bin/agent

# --- Cua Driver (computer use; Linux Rust build) --------------------------------
# Boat paths kept: /opt/ascii/cua-driver/cua-driver, /run/ascii-cua/driver.sock.
cua=$(fetch "https://api.github.com/repos/trycua/cua/releases?per_page=100" |
	jq -r '[.[] | select(.draft == false) | .tag_name | select(test("^cua-driver-rs-v[0-9.]+$"))][0]')
v=${cua#cua-driver-rs-v}
install -d /opt/crowdvm/cua-driver /opt/ascii
fetch "https://github.com/trycua/cua/releases/download/$cua/cua-driver-rs-$v-linux-x86_64-binary.tar.gz" |
	tar -xz -C /opt/crowdvm/cua-driver
test -x /opt/crowdvm/cua-driver/cua-driver
ln -sfn /opt/crowdvm/cua-driver /opt/ascii/cua-driver
ln -sf /opt/crowdvm/cua-driver/cua-driver /usr/local/bin/cua-driver
sudo -iu user cua-driver telemetry disable
# cua-driver.service (user unit, rootfs) runs for every boot of user's manager.
systemctl --global enable cua-driver.service
install -d /var/lib/systemd/linger
touch /var/lib/systemd/linger/user
rm -rf "$dl"
