#!/usr/bin/env sh
# Builds the crowdvm CLI for every supported platform.
# Usage: build-cli.sh VERSION OUT
#   OUT/bin/<os>-<arch>/crowdvm[.exe]           (raw binaries, for npm)
#   OUT/release/crowdvm-VERSION-<os>-<arch>.*   (archives + SHA256SUMS, for GitHub)
set -eu
VERSION=$1
OUT=$2
TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"
rm -rf "$OUT" && mkdir -p "$OUT/bin" "$OUT/release"
for t in $TARGETS; do
  os=${t%/*} arch=${t#*/} ext=""
  [ "$os" = windows ] && ext=".exe"
  dir="$OUT/bin/$os-$arch"
  mkdir -p "$dir"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$dir/crowdvm$ext" ./cmd/crowdvm
  name="crowdvm-$VERSION-$os-$arch"
  stage="$OUT/stage/$name"
  mkdir -p "$stage"
  cp "$dir/crowdvm$ext" LICENSE-npm "$stage/"
  if [ "$os" = windows ]; then
    (cd "$OUT/stage" && python3 -m zipfile -c "../release/$name.zip" "$name")
  else
    tar -C "$OUT/stage" -czf "$OUT/release/$name.tar.gz" "$name"
  fi
done
rm -rf "$OUT/stage"
(cd "$OUT/release" && sha256sum ./* | sed 's| \./| |' > SHA256SUMS)
echo "built crowdvm $VERSION for: $TARGETS"
