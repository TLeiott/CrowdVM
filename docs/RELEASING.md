# Releasing

Three branches, one direction:

```
main ──PR──▶ nightly ──PR──▶ stable
 dev          npm @nightly     npm @latest
              GitHub pre-release   GitHub release
```

- **main**: day-to-day work. Every push and pull request runs the tests (`ci`).
- **nightly**: takes pull requests from `main` only. Each merge releases a
  nightly: `crowdvm@<next>-nightly.<yyyymmdd>.<run>`.
- **stable**: takes pull requests from `nightly` only. Each merge releases
  `VERSION` as `crowdvm@<VERSION>` (dist-tag `latest`).

Both release branches only change through pull requests (no direct pushes, no
force pushes), and a pull request merges only when `branch-flow` (right
source branch) and `tests` pass. Use **Create a merge commit**: squash merges
would make every later promotion conflict.

## What a release does (`.github/workflows/release.yml`)

1. Runs the unit tests (Go, TypeScript and Python SDKs).
2. Builds the CLI for linux, macOS and Windows on x64 and arm64.
3. Publishes to npm with provenance: `@crowdvm/cli-<os>-<arch>` (one binary
   each), `crowdvm` (the command; picks the right binary) and
   `@crowdvm/sdk`. Skipped, with a warning, until the `NPM_TOKEN` secret exists.
4. Creates the GitHub Release `v<version>` with the archives and `SHA256SUMS`.
   Nightlies are pre-releases; only the newest 7 are kept.
5. Deletes old workflow runs (keeps the newest 10). No artifacts or caches
   are stored, and run logs are kept for 1 day.

The server and the node app are not published: they stay internal. The VM
e2e suite needs KVM and the sandbox image, so run `make e2e` locally before
promoting to stable.

## Cutting a stable release

1. On `main`, set `VERSION` to the release version (for example `0.2.0`).
2. Open a pull request `main → nightly`, merge it: nightlies now count up to
   `0.2.0-nightly.…`.
3. When a nightly is good, open `nightly → stable`, merge it: `0.2.0` ships.
   The pull request already fails if `VERSION` was released before.

A failed release can be re-run (Actions → release → Run workflow on that
branch): anything already published is skipped.

## Installing

```sh
npm i -g crowdvm            # stable
npm i -g crowdvm@nightly    # nightly
npm i @crowdvm/sdk          # TypeScript client (@nightly for nightly)
```

A CLI installed with npm updates through npm (`crowdvm self-update` says the
command). The npm packages are under `LICENSE-npm`: free for personal and
other non-commercial use.
