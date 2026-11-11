---
name: release
description: Release CrowdVM from this machine (no CI). "release nightly" promotes main to nightly and publishes npm @nightly plus a GitHub pre-release; "release stable" promotes nightly to stable and publishes npm @latest plus a GitHub release. Use when asked to release, publish, ship a nightly or a stable version, or bump VERSION for a release.
---

# Release CrowdVM

Releases run locally with `scripts/release/release.sh`. There is no CI
service: GitHub Actions is disabled for this repo, so nothing is billed.

```
main ──release nightly──▶ nightly ──release stable──▶ stable
                          npm @nightly                npm @latest
                          GitHub pre-release          GitHub release
```

## Before releasing

1. The working tree is committed and `main` is pushed: a release takes
   `origin/main` (nightly) or `origin/nightly` (stable), never local edits.
2. Sign-ins: `gh auth status` must work, and `npm whoami` must name an
   account allowed to publish `crowdvm` and the `@crowdvm` scope. If either
   fails, stop and tell the user which login to run (`gh auth login`,
   `npm login`); do not try to work around it.
3. For **stable**: `VERSION` on `main` must be a version not released yet
   (`node scripts/release/version.mjs stable` prints it or fails). To ship a
   new version, set `VERSION` on `main`, commit, push, release nightly, then
   release stable.

## Releasing

- Nightly: `make release-nightly` (or `scripts/release/release.sh nightly`).
  About 5 minutes. Add `--e2e` to also run the VM e2e suite.
- Stable: `make release-stable`. Runs the VM e2e suite (about 20 minutes);
  `--no-e2e` only when the user explicitly asks to skip it.
- `--dry-run` tests and builds without pushing or publishing: use it first
  when the release scripts changed.

Run it in the background (it takes minutes) and wait for it. Never push to
`nightly` or `stable` by hand: the pre-push hook refuses it, and the release
script is what keeps them strictly downstream of `main`.

## After releasing

Report the version, the npm install command (`npm i -g crowdvm@nightly` or
`npm i -g crowdvm`) and the release URL the script prints. If it failed,
show the failing step's output. Re-running is safe: published packages and
existing releases are skipped or completed.

## How it works

`docs/RELEASING.md` describes the branches, versions, packages and the
license of the npm packages (`LICENSE-npm`).
