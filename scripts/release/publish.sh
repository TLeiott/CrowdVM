#!/usr/bin/env sh
# Publishes the npm packages of one release (npm-pack.mjs output) with a
# dist-tag (latest = stable, nightly). Platform packages go first: the
# crowdvm package depends on them. A version already on npm is skipped, so a
# failed release can simply run again.
# Usage: publish.sh TAG OUT     (needs NODE_AUTH_TOKEN or an npm login)
set -eu
TAG=$1
NPM=$2/npm
PROVENANCE=""
[ "${GITHUB_ACTIONS:-}" = true ] && PROVENANCE="--provenance"
for dir in "$NPM"/cli-* "$NPM/crowdvm" "$NPM/sdk"; do
  name=$(node -p "require('$dir/package.json').name")
  version=$(node -p "require('$dir/package.json').version")
  if npm view "$name@$version" version >/dev/null 2>&1; then
    echo "skip $name@$version (already published)"
    continue
  fi
  (cd "$dir" && npm publish --access public --tag "$TAG" $PROVENANCE)
  echo "published $name@$version ($TAG)"
done
