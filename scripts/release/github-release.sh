#!/usr/bin/env sh
# Creates the GitHub Release vVERSION at COMMIT with the CLI archives and
# SHA256SUMS. Nightlies are pre-releases, and only the newest KEEP of them
# stay (older ones and their tags are deleted). Needs GH_TOKEN.
# Usage: github-release.sh VERSION CHANNEL OUT COMMIT
set -eu
VERSION=$1 CHANNEL=$2 OUT=$3 COMMIT=$4
KEEP=${KEEP_NIGHTLIES:-7}
TAG="v$VERSION"
if gh release view "$TAG" >/dev/null 2>&1; then
  echo "release $TAG exists; uploading any missing assets"
  gh release upload "$TAG" "$OUT"/release/* --clobber
else
  flags="--latest"
  [ "$CHANNEL" = nightly ] && flags="--prerelease --latest=false"
  # shellcheck disable=SC2086
  gh release create "$TAG" "$OUT"/release/* --target "$COMMIT" --title "crowdvm $VERSION" $flags \
    --notes "crowdvm $VERSION ($CHANNEL).

Install: \`npm i -g crowdvm@$( [ "$CHANNEL" = nightly ] && echo nightly || echo latest )\`, or download the archive for your platform below (verify it with SHA256SUMS).

Free for personal and other non-commercial use; see LICENSE-npm in the archive."
fi
if [ "$CHANNEL" = nightly ]; then
  gh release list --limit 200 --json tagName,isPrerelease,createdAt \
    --jq '[.[] | select(.isPrerelease and (.tagName | test("-nightly\\.")))] | sort_by(.createdAt) | reverse | .['"$KEEP"':] | .[].tagName' |
    while read -r old; do
      echo "pruning old nightly $old"
      gh release delete "$old" --yes --cleanup-tag
    done
fi
