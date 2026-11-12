#!/usr/bin/env sh
# CrowdVM installer for Linux and macOS, served by a CrowdVM server:
#   curl -fsSL http://<server>:8420/install | sh                 # CLI only
#   curl -fsSL http://<server>:8420/install | sh -s -- --node    # CLI + node app
# Options: --no-shell (do not add the shell function), --no-login (do not sign in).
# CROWDVM_AGENT_SETUP=1/0 answers the coding-agent setup question without asking.
# The server replaces __CROWDVM_SERVER__ with its public URL when serving.
set -eu
SERVER="${CROWDVM_SERVER:-__CROWDVM_SERVER__}"
BIN_DIR="${CROWDVM_BIN_DIR:-$HOME/.local/bin}"
WITH_NODE=0
NO_SHELL="${CROWDVM_NO_SHELL:-0}"
NO_LOGIN="${CROWDVM_NO_LOGIN:-0}"
for a in "$@"; do
  case "$a" in
    --node) WITH_NODE=1 ;;
    --no-shell) NO_SHELL=1 ;;
    --no-login) NO_LOGIN=1 ;;
    --server=*) SERVER="${a#--server=}" ;;
    --bin-dir=*) BIN_DIR="${a#--bin-dir=}" ;;
    *) echo "unknown option: $a" >&2; exit 2 ;;
  esac
done
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;; esac
mkdir -p "$BIN_DIR"
fetch() { fetch_as "$1" "$os/$arch" "$1"; }
fetch_as() { # name os/arch target
  curl -fsSL "$SERVER/downloads/$2/$1" -o "$BIN_DIR/$3.tmp" && chmod +x "$BIN_DIR/$3.tmp" && mv "$BIN_DIR/$3.tmp" "$BIN_DIR/$3"
}
echo "Installing crowdvm from $SERVER into $BIN_DIR"
fetch crowdvm
if [ "$WITH_NODE" = 1 ]; then
  [ "$os" = linux ] || { echo "the node app runs on Linux and Windows only" >&2; exit 1; }
  fetch crowdvm-node
  # The guest agent and the in-sandbox CLI run inside the (x86_64) VMs,
  # whatever the host's arch.
  fetch_as crowdvm-guest linux/amd64 crowdvm-guest
  [ "$arch" = amd64 ] || fetch_as crowdvm linux/amd64 crowdvm-linux-amd64
  command -v qemu-system-x86_64 >/dev/null || echo "note: install QEMU (qemu-system-x86_64, qemu-img) to host sandboxes"
  [ -w /dev/kvm ] || echo "note: /dev/kvm is not writable; add yourself to the kvm group for fast VMs"
  echo "Next: crowdvm-node join --server $SERVER --token <join token> && crowdvm-node install-service"
fi
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "Add $BIN_DIR to your PATH." ;; esac
CLI="$BIN_DIR/crowdvm"
"$CLI" config set server "$SERVER" >/dev/null || echo "warning: could not save the server URL; run: crowdvm config set server $SERVER" >&2

# Shell function: gives each terminal its own "current" sandbox (crowdvm shell-init).
add_init() { # file shell create
  [ -f "$1" ] || [ "$3" = 1 ] || return 0
  grep -qs "crowdvm.*shell-init" "$1" && return 0
  mkdir -p "$(dirname "$1")"
  if [ "$2" = fish ]; then
    printf '\n# crowdvm: per-shell current sandbox\ntest -x "%s"; and "%s" shell-init fish | source\n' "$CLI" "$CLI" >> "$1"
  else
    printf '\n# crowdvm: per-shell current sandbox\n[ -x "%s" ] && eval "$("%s" shell-init %s)"\n' "$CLI" "$CLI" "$2" >> "$1"
  fi
  echo "Added the crowdvm shell function to $1"
}
if [ "$NO_SHELL" = 0 ]; then
  sh_name=$(basename "${SHELL:-sh}")
  add_init "$HOME/.bashrc" bash "$([ "$sh_name" = bash ] && echo 1)"
  add_init "${ZDOTDIR:-$HOME}/.zshrc" zsh "$([ "$sh_name" = zsh ] && echo 1)"
  if [ "$sh_name" = fish ] || command -v fish >/dev/null 2>&1; then
    add_init "${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/crowdvm.fish" fish 1
  fi
fi

# Sign in when a person is at a terminal (curl | sh keeps stdin busy: use /dev/tty).
TTY=/dev/null
(: </dev/tty) 2>/dev/null && TTY=/dev/tty
if [ "$NO_LOGIN" = 0 ] && [ "$TTY" = /dev/tty ]; then
  "$CLI" onboard </dev/tty || echo "Sign in later with: crowdvm login"
  next="Open a new terminal to use the shell function."
else
  next="Run: crowdvm onboard   (open a new terminal first to load the shell function)"
fi

# Coding agents (Claude Code, Codex, OpenCode, Cursor): offer the crowdvm skill and
# rule. Asks on the terminal; without one (or in CI) it prints the command.
# CROWDVM_AGENT_SETUP=1 sets up without asking, CROWDVM_AGENT_SETUP=0 skips.
echo
"$CLI" skill setup --json=false <"$TTY" || echo "Set up coding agents later with: crowdvm skill setup"
echo "Done. $next"
