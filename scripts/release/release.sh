#!/usr/bin/env bash
# Local release: no CI service, no billing. Runs on this machine.
#
#   release.sh nightly   main    → nightly, npm @nightly, GitHub pre-release
#   release.sh stable    nightly → stable,  npm @latest,  GitHub release
#
# Steps: the target branch fast-forwards to the source branch (so it only
# ever holds what the source had), the tests run on exactly that commit, the
# CLI is built for every platform, the branch is pushed, then npm and the
# GitHub Release are published. Everything happens in a temporary worktree
# and build dir that are removed at the end. Re-running after a failure is
# safe: what is already published is skipped.
#
# Options:
#   --e2e        also run the VM e2e suite (default for stable)
#   --no-e2e     skip it (also for stable)
#   --dry-run    test and build only: no push, no npm, no GitHub Release
set -euo pipefail

channel=${1:-}
shift || true
case "$channel" in
  nightly) source=main target=nightly tag=nightly e2e=0 ;;
  stable) source=nightly target=stable tag=latest e2e=1 ;;
  *) echo "usage: $0 nightly|stable [--e2e|--no-e2e] [--dry-run]" >&2; exit 2 ;;
esac
dry=0
for a in "$@"; do
  case "$a" in
    --e2e) e2e=1 ;;
    --no-e2e) e2e=0 ;;
    --dry-run) dry=1 ;;
    *) echo "unknown option $a" >&2; exit 2 ;;
  esac
done

root=$(git rev-parse --show-toplevel)
cd "$root"
say() { printf '\n==> %s\n' "$*"; }
die() { printf 'release: %s\n' "$*" >&2; exit 1; }

say "Checking prerequisites"
command -v go >/dev/null || die "go is not installed"
command -v node >/dev/null || die "node is not installed"
command -v gh >/dev/null || die "the GitHub CLI (gh) is not installed"
if [ "$dry" = 0 ]; then
  gh auth status >/dev/null 2>&1 || die "gh is not signed in: run gh auth login"
  npm whoami >/dev/null 2>&1 || die "npm is not signed in: run npm login (an account that may publish crowdvm and @crowdvm/*)"
fi
git fetch --quiet origin
git rev-parse --verify --quiet "origin/$source" >/dev/null || die "origin/$source does not exist"
# A local copy of the source branch must not hold unpushed or unpulled work
# (the release takes origin/$source). No local copy (often nightly) is fine.
if local=$(git rev-parse --verify --quiet "refs/heads/$source") && [ "$local" != "$(git rev-parse "origin/$source")" ]; then
  die "$source here and on origin differ: push or pull $source first (a release takes origin/$source)"
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/crowdvm-release.XXXXXX")
cleanup() {
  git -C "$root" worktree remove --force "$work/tree" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

say "Moving $target to origin/$source (fast-forward only)"
if git rev-parse --verify --quiet "origin/$target" >/dev/null; then
  git worktree add --quiet --detach "$work/tree" "origin/$target"
  git -C "$work/tree" merge --quiet --ff-only "origin/$source" ||
    die "$target has commits that are not in $source: it must only ever receive $source (fix by hand)"
else
  git worktree add --quiet --detach "$work/tree" "origin/$source" # first release creates the branch
fi
commit=$(git -C "$work/tree" rev-parse HEAD)
if [ "$(git rev-parse --verify --quiet "origin/$target" || true)" = "$commit" ]; then
  echo "$target is already at $source ($commit): re-releasing that commit"
fi

cd "$work/tree"
run=$(git rev-list --count HEAD)
version=$(node scripts/release/version.mjs "$channel" "$run")
say "crowdvm $version ($channel) from ${commit:0:12}"

say "Tests"
go vet ./...
go test -count=1 ./...
(cd sdk/typescript && npm ci --no-audit --no-fund --silent && npm test --silent)
(cd sdk/python && python3 -m unittest -q)
if [ "$e2e" = 1 ]; then
  say "VM e2e suite (about 20 minutes)"
  CROWDVM_E2E=1 go test -count=1 -timeout 120m ./test/e2e/
fi

say "Building the CLI for every platform"
scripts/release/build-cli.sh "$version" "$work/out"
node scripts/release/npm-pack.mjs "$version" "$work/out" >/dev/null

if [ "$dry" = 1 ]; then
  say "Dry run: built $version; nothing pushed or published"
  ls "$work/out/release"
  exit 0
fi

say "Pushing $target"
CROWDVM_RELEASE=1 git push --quiet origin "$commit:refs/heads/$target"

say "Publishing to npm (dist-tag $tag)"
scripts/release/publish.sh "$tag" "$work/out"

say "GitHub Release v$version"
scripts/release/github-release.sh "$version" "$channel" "$work/out" "$commit"

say "Released crowdvm $version"
echo "  npm i -g crowdvm@$tag"
echo "  https://github.com/TLeiott/CrowdVM/releases/tag/v$version"
