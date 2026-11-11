#!/usr/bin/env bash
# Run a local CrowdVM cluster on this machine: 1 server + N nodes (default 2),
# each node with its own data dir, all on localhost. For development and e2e.
#   scripts/dev-cluster.sh up [--nodes N] [--driver qemu|process]
#   scripts/dev-cluster.sh down
#   scripts/dev-cluster.sh env     # prints exports for CROWDVM_API_BASE / CROWDVM_API_KEY
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIR=${CROWDVM_DEV_DIR:-$HOME/.local/share/crowdvm-dev/cluster}
PORT=${CROWDVM_DEV_PORT:-8420}
BIN=$ROOT/bin
cmd=${1:-up}; shift || true
NODES=2; DRIVER=qemu
while [ $# -gt 0 ]; do
  case "$1" in --nodes) NODES=$2; shift 2 ;; --driver) DRIVER=$2; shift 2 ;; *) echo "unknown $1"; exit 2 ;; esac
done

up() {
  make -C "$ROOT" build >/dev/null
  mkdir -p "$DIR/logs"
  "$BIN/crowdvm-server" serve --listen "127.0.0.1:$PORT" --data-dir "$DIR/server" \
    --public-url "http://127.0.0.1:$PORT" >"$DIR/logs/server.log" 2>&1 &
  echo $! >"$DIR/server.pid"
  for _ in $(seq 50); do curl -s -o /dev/null "http://127.0.0.1:$PORT/api/v1/server" && break; sleep 0.2; done
  if [ ! -f "$DIR/cli/config.json" ]; then
    "$BIN/crowdvm-server" user create --data-dir "$DIR/server" --username dev --password devpass123 --admin >/dev/null
    echo devpass123 | CROWDVM_CONFIG_DIR="$DIR/cli" "$BIN/crowdvm" login --server "http://127.0.0.1:$PORT" --username dev --password-stdin >/dev/null
  fi
  for i in $(seq 1 "$NODES"); do
    nd="$DIR/node$i"; mkdir -p "$nd"
    if [ ! -f "$nd/config.json" ]; then
      tok=$("$BIN/crowdvm-server" join-token create --data-dir "$DIR/server" | grep -o 'cvj_[a-z0-9]*' | head -1)
      "$BIN/crowdvm-node" join --data-dir "$nd" --server "http://127.0.0.1:$PORT" --token "$tok" --name "dev-node-$i" \
        --driver "$DRIVER" --guest-binary "$BIN/crowdvm-guest" --ui-addr "127.0.0.1:$((PORT + i))" --skip-bench --cpus 4 --memory 12G --disk 400G >/dev/null
    fi
    "$BIN/crowdvm-node" run --data-dir "$nd" >"$DIR/logs/node$i.log" 2>&1 &
    echo $! >"$nd/pid"
  done
  echo "cluster up: server http://127.0.0.1:$PORT, $NODES node(s), driver=$DRIVER, logs in $DIR/logs"
  env_
}

down() {
  for f in "$DIR"/node*/pid "$DIR/server.pid"; do
    [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null || true; rm -f "$f"
  done
  echo "cluster down"
}

env_() {
  echo "export CROWDVM_CONFIG_DIR=$DIR/cli   # CLI signed in as admin 'dev' (password devpass123)"
  echo "export CROWDVM_API_BASE=http://127.0.0.1:$PORT/api/v1"
}

case "$cmd" in up) up ;; down) down ;; env) env_ ;; *) echo "usage: $0 up|down|env"; exit 2 ;; esac
