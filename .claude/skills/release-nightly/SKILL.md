---
name: release-nightly
description: Publish a CrowdVM nightly from this machine (no CI): fast-forwards nightly to main, publishes npm @nightly and a GitHub pre-release. Use when asked to release, ship, cut or publish a nightly, or to move main to nightly. Not for stable or production releases (that is release-stable).
---

# Nightly: main → nightly

`scripts/release/release.sh nightly` does everything (fast-forward, tests,
build, push, npm, GitHub pre-release) from `origin/main`, in about 5 minutes.
How the pipeline works: `docs/RELEASING.md`.

## Preconditions

```sh
git fetch -q origin && git status --short && git rev-list --left-right --count main...origin/main
```

- Uncommitted changes, or `main` ahead of `origin/main`: stop. The release
  ships `origin/main`, so this work would be left out. Show it and ask:
  commit and push it first, or release without it? Never commit or push unasked.
- `main` behind `origin/main`: `git pull --ff-only` (the script refuses otherwise).
- The checked-out branch doesn't matter; the script uses a temporary worktree.

## Run

```sh
make release-nightly          # or: scripts/release/release.sh nightly --e2e
```

Add `--e2e` only when the user asks for the VM e2e suite (about 20 minutes
more). Run it in the background (Bash `run_in_background`) and wait for it
to finish; don't poll with `sleep` and don't start other work meanwhile.

Never push `nightly` yourself: the pre-push hook refuses it, and only the
script keeps `nightly` a strict fast-forward of `main`.

## Failures

- "gh is not signed in" / "npm is not signed in": tell the user the command
  the script names (`gh auth login`, or `npm login` with an account that may
  publish `crowdvm` and `@crowdvm/*`) and stop. No tokens, no workarounds,
  no `--dry-run` as a substitute.
- "nightly has commits that are not in main": report it; don't fix by hand.
- Anything else: show the failing `==> step` and its output. Re-running is
  safe (published packages and existing releases are skipped), so offer to
  re-run once the cause is fixed.

## Report

Three lines from the script's tail, nothing more unless something went wrong:
the version (`0.1.0-nightly.20261111.42`), `npm i -g crowdvm@nightly`, and
the release URL.
