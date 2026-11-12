#!/usr/bin/env sh
# Cross-compile release archives into dist/.
set -eu
VERSION=${1:-dev}
P=github.com/crowdvm/crowdvm/internal
LDFLAGS="-s -w -X main.version=$VERSION -X $P/server.Version=$VERSION -X $P/node.Version=$VERSION -X $P/guest.Version=$VERSION"
rm -rf dist && mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o dist/crowdvm-guest ./cmd/crowdvm-guest
# in-sandbox CLI for the node's config drive (crowdvm-node --cli-binary default)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o dist/crowdvm-linux-amd64 ./cmd/crowdvm
for target in linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}; arch=${target#*/}; ext=""; [ "$os" = windows ] && ext=".exe"
  out="dist/crowdvm_${VERSION}_${os}_${arch}"; mkdir -p "$out"
  for cmd in crowdvm crowdvm-server crowdvm-node; do
    # the node app only targets Linux and Windows hosts
    [ "$cmd" = crowdvm-node ] && [ "$os" = darwin ] && continue
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags "$LDFLAGS" -o "$out/$cmd$ext" "./cmd/$cmd"
  done
  [ "$os" != darwin ] && cp dist/crowdvm-guest dist/crowdvm-linux-amd64 "$out/"
  if [ "$os" = windows ]; then
    # zip is often not installed; Python's zipfile writes the same archive
    if command -v zip >/dev/null; then (cd dist && zip -qr "$(basename "$out").zip" "$(basename "$out")")
    else (cd dist && python3 -m zipfile -c "$(basename "$out").zip" "$(basename "$out")"); fi
  else tar -C dist -czf "$out.tar.gz" "$(basename "$out")"; fi
done
(cd dist && sha256sum ./*.tar.gz ./*.zip > SHA256SUMS)
# manifest.json: the version and each binary's checksum in the downloads
# layout (<os>/<arch>/<name>); `crowdvm-server downloads import` keeps it
# and `crowdvm-node update` reads the version from it.
{
  printf '{"version": "%s", "files": {' "$VERSION"; sep=""
  for f in dist/crowdvm_"$VERSION"_*/*; do
    osarch=$(basename "$(dirname "$f")"); osarch=${osarch#crowdvm_"$VERSION"_}
    printf '%s\n  "%s/%s/%s": {"sha256": "%s", "size": %s}' "$sep" "${osarch%_*}" "${osarch#*_}" "$(basename "$f")" \
      "$(sha256sum "$f" | cut -d' ' -f1)" "$(wc -c < "$f" | tr -d ' ')"
    sep=","
  done
  printf '\n}}\n'
} > dist/manifest.json
echo "built dist/ for $VERSION"
