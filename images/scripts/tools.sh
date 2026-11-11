#!/bin/bash
# Print "tool=version" for every tool in the image (recorded in <image>.json).
set -uo pipefail
profile=${1:-minimal}
v() { # name, command...: first line of output, "-" stripped of noise
	local out
	out=$("${@:2}" 2>/dev/null | grep -m1 -oE '[0-9]+(\.[0-9]+)+[^ ,)]*') || out=
	[ -n "$out" ] && echo "$1=$out"
}
asuser() { sudo -iu user "$@"; }
. /etc/os-release
echo "ubuntu=$VERSION_ID"
v kernel bash -c 'ls /boot/vmlinuz-* | sort -V | tail -1'
v systemd systemctl --version
v openssh dpkg-query -W -f='${Version}' openssh-server
v python3 python3 --version
v git git --version
v ufw dpkg-query -W -f='${Version}' ufw
[ "$profile" = full ] || exit 0
v docker docker --version
v docker-buildx docker buildx version
v docker-compose docker compose version
v containerd containerd --version
v node node --version
v npm npm --version
v pnpm pnpm --version
v bun bun --version
v uv uv --version
v pip python3 -m pip --version
v rustc asuser rustc --version
v cargo asuser cargo --version
v gh gh --version
v claude claude --version
v codex codex --version
v opencode opencode --version
v pi pi --version
v kimi kimi --version
v vibe vibe --version
v hermes hermes --version
v openclaw openclaw --version
v prime-agent asuser prime-agent --version
v cursor-agent cursor-agent --version
v cua-driver cua-driver --version
v go go version
v deno deno --version
v nvm-node22 asuser bash -lc 'ls /usr/local/nvm/versions/node'
v java bash -c 'java -version 2>&1 | tr -d \"'
v maven mvn -v -B
v gradle bash -c 'basename /opt/gradle/lib/gradle-launcher-*.jar .jar'
v kotlin bash -c 'kotlinc -version 2>&1'
v scala bash -c 'scalac -version 2>&1'
v sbt sbt --script-version
v ruby ruby --version
v php php --version
v composer composer --version
v erlang-otp dpkg-query -W -f='${Version}' erlang-base
v elixir dpkg-query -W -f='${Version}' elixir
v dotnet dotnet --version
v R R --version
v clang clang --version
v cmake cmake --version
v ninja ninja --version
v google-chrome google-chrome --version
v code asuser code --version
v ghostty dpkg-query -W -f='${Version}' ghostty
v xfce4-terminal dpkg-query -W -f='${Version}' xfce4-terminal
v xfce4 dpkg-query -W -f='${Version}' xfce4-session
v tigervnc dpkg-query -W -f='${Version}' tigervnc-standalone-server
v xdotool xdotool --version
v ffmpeg ffmpeg -version
v imagemagick convert -version
v scrot scrot --version
v gcc gcc --version
v make make --version
v jq jq --version
v rg rg --version
exit 0
