# Releasing

Releases run on a developer machine. There is no CI service (GitHub Actions
is disabled for this repository), so releasing costs nothing: GitHub Releases
and the npm registry are free for public projects.

```
main ──release nightly──▶ nightly ──release stable──▶ stable
 dev                      npm @nightly                npm @latest
                          GitHub pre-release          GitHub release
```

- **main**: day-to-day work.
- **nightly**: only ever fast-forwarded to `main` by `release.sh nightly`.
  Version `<next>-nightly.<yyyymmdd>.<commit count>`.
- **stable**: only ever fast-forwarded to `nightly` by `release.sh stable`.
  Version: the `VERSION` file.

Pushing to `nightly` or `stable` by hand is refused by the pre-push hook
(`make hooks` sets it up; the release targets do it too), and GitHub forbids
force-pushing or deleting them.

## Commands

```sh
make release-nightly                         # main → nightly, about 5 minutes
make release-stable                          # nightly → stable, with the VM e2e suite
scripts/release/release.sh nightly --e2e     # nightly with the e2e suite too
scripts/release/release.sh stable --dry-run  # test and build only, publish nothing
```

In Claude Code, the `release-nightly` and `release-stable` skills do the same
("release nightly", "ship stable"); `release-stable` confirms before it publishes.

Needs: `go`, `node`, `gh` signed in (`gh auth login`) and npm signed in with
an account that can publish `crowdvm` and `@crowdvm/*` (`npm login`).

## What a release does (`scripts/release/release.sh`)

1. Fast-forwards the target branch to the source branch on origin, in a
   temporary worktree (your checkout is not touched). If the target has
   commits the source does not, it stops.
2. Runs the unit tests (Go, TypeScript and Python SDKs), and for stable the
   VM e2e suite.
3. Builds the CLI for linux, macOS and Windows on x64 and arm64
   (`build-cli.sh`) and lays out the npm packages (`npm-pack.mjs`).
4. Pushes the branch, publishes to npm (`publish.sh`): `@crowdvm/cli-<os>-<arch>`
   (one binary each), `crowdvm` (the command; it runs the right binary) and
   `@crowdvm/sdk`. Then the GitHub Release `v<version>` with the archives and
   `SHA256SUMS` (`github-release.sh`). Nightlies are pre-releases, and only
   the newest 7 are kept.
5. Deletes the temporary worktree and build output.

Re-running after a failure is safe: published versions are skipped and an
existing release gets its missing files.

The server and the node app are not published: they stay internal.

## Server and node binaries

`make dist VERSION=v0.2.0` (`scripts/dist.sh`) builds every binary into
`dist/` and writes `dist/manifest.json`: the version and each binary's sha256
and size, keyed like the downloads (`<os>/<arch>/<name>`). On the server,
`crowdvm-server downloads import dist` copies the binaries and the manifest;
`GET /downloads` then lists the version and every file's sha256. Nodes install
the release with `crowdvm-node update` (or Update node on their local page),
and `crowdvm self-update` checks the CLI against the same checksums.

## Cutting a stable release

1. On `main`, set `VERSION` (for example `0.2.0`), commit and push.
2. `make release-nightly`: nightlies now count up to `0.2.0-nightly.…`.
3. When a nightly is good: `make release-stable` ships `0.2.0`.

## Installing

```sh
npm i -g crowdvm            # stable
npm i -g crowdvm@nightly    # nightly
npm i @crowdvm/sdk          # TypeScript client (@nightly for nightly)
```

A CLI installed with npm updates through npm (`crowdvm self-update` says the
command). The npm packages are under `LICENSE-npm`: free for personal and
other non-commercial use; commercial use needs permission.
