---
name: release-stable
description: Publish a CrowdVM stable (production) release from this machine (no CI): fast-forwards stable to nightly, runs the VM e2e suite, publishes npm @latest and a GitHub release. Use when asked to release, ship or publish stable or production, promote nightly to stable, or cut version x.y.z. Confirms before publishing. Not for nightlies (that is release-nightly).
---

# Stable: nightly → stable

Publishing is outward-facing and final: an npm version can't be republished
and `stable` only moves forward. Check, show, confirm, then run the script.
How the pipeline works: `docs/RELEASING.md`.

## 1. Preconditions

```sh
git fetch -q origin
v=$(git show origin/nightly:VERSION); echo "$v"    # the version that ships
git show origin/main:VERSION                      # must equal $v
npm view "crowdvm@$v" version                     # must print nothing (a 404 is fine)
git log --oneline origin/stable..origin/nightly   # all of origin/nightly on the first release
```

- `$v` is already on npm: that version was released. Say so and offer the
  bump: set `VERSION` on `main` (ask which number), commit
  `chore(release): bump version to x.y.z`, push, run `/release-nightly`,
  then come back here. Ask before the commit, the push and the nightly.
- main's VERSION differs from nightly's: main was bumped but no nightly
  carries it yet. Offer `/release-nightly` first; don't ship the old number.
- Empty commit range: `stable` already is `nightly`. Say so; re-run only if
  the user wants missing artifacts republished.

## 2. Confirm

Skip this step only when the user's own message said to ship stable or
production ("release stable", "ship 0.2.0"): then run `make release-stable`
right away (e2e included, about 15 minutes) and continue at step 4.

Otherwise get the e2e result before anything irreversible happens:

```sh
scripts/release/release.sh stable --dry-run   # tests, e2e, build; publishes nothing
```

Run it in the background and wait. Then ask, filled in:

> Ready to ship **crowdvm 0.2.0** (stable).
> Commits (origin/stable..origin/nightly, 14): <the `git log --oneline` lines>
> e2e: passed in the dry run  [or: skipped, you asked for --no-e2e]
> Publishes npm crowdvm@latest + @crowdvm/* and GitHub release v0.2.0.
> This can't be undone. Go ahead?

Nothing is published until the user says yes. A "no" or a question: answer it, don't run.

## 3. Publish

```sh
scripts/release/release.sh stable --no-e2e    # e2e already passed on this exact commit
```

First `git fetch -q origin && git rev-parse origin/nightly` must still be the
dry run's commit; if not, start over at step 1. Otherwise `--no-e2e` is only
for when the user explicitly asked to skip e2e, and the confirmation says so.
Run in the background and wait.

Never push `stable` yourself: the pre-push hook refuses it, and only the
script keeps `stable` a strict fast-forward of `nightly`.

## 4. Failures

- "gh is not signed in" / "npm is not signed in": give the user the command
  the script names (`gh auth login`; `npm login` with an account that may
  publish `crowdvm` and `@crowdvm/*`) and stop. No tokens, no workarounds.
- "is already released: bump VERSION on main": step 1, first bullet.
- "stable has commits that are not in nightly": report it; don't fix by hand.
- Anything else: show the failing `==> step` and its output. Re-running is
  safe (published packages and existing releases are skipped); offer it.

## 5. Report

The version, `npm i -g crowdvm`, the release URL (the script's last lines),
and whether e2e ran. Nothing more unless something went wrong.
