# Review opus-4: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Opus 5.5 (independent, round 3). Date: 2026-11-10. Tree: uncommitted working tree on `master`.

## Verdict: DISAGREE (narrowly)

Nearly everything is now in place. Of the six opus-3 blockers, five are fixed, and I confirmed four of them live on my own KVM node. opus-3 N9 and sonnet-3 N1–N3 are fixed too. Unit tests pass, and the other session's full e2e run ended `EXIT=0` (34 groups passed, `bootloop` skipped).

Two supportable features are still broken, and I reproduced both live:

1. **B1. opus-3 N6 is not fixed: the low-disk cleanup still deletes Docker images built in the sandbox.** The fix tells built images from pulled ones by whether `docker image ls` shows a `sha256:` digest. The sandbox image runs Docker 29.9.3 with the containerd image store, which gives locally built images a digest too. I filled the disk to 100%, and the cleanup deleted `mybuilt:1`, an image built in the sandbox. PARITY LIFE-044 states this cannot happen.
2. **B2 (new). Listing sandboxes with a scoped API key paginates before it filters.** A key limited to some sandboxes or environments, or a tenant key that sees only the sandboxes it created, can get an empty or short page with `hasMore:false` and no cursor, even though it has sandboxes. Live: a key limited to one sandbox got `[]` with `hasMore:false` at `limit=1`, because two newer sandboxes it may not see exist. With the default `limit=100`, this happens once the account has more than 100 newer sandboxes it may not see. That is the normal state for the multi-tenant pattern in Boat's platform guide.

Both fixes are a few lines each.

## Method

1. **Read** opus-3, sonnet-3, the PARITY N/A rows, and the parts of `docs/gaps/*` that the findings touch.
2. **Build and tests.**
   - `make build` is clean.
   - `go test` on every package except `test/e2e`: all OK.
   - I did not run e2e. I read `…/232e4b2f…/scratchpad/e2e-run2.log`, which ends `ok github.com/crowdvm/crowdvm/test/e2e 599.034s` and `EXIT=0`, with 34 `--- PASS` lines and 1 `--- SKIP` (`bootloop`).
3. **Code check** of every opus-3 blocker (N1–N6), opus-3 N9, and sonnet-3 N1–N3, with file:line references below.
4. **Own cluster.** All data was under `~/.local/share/crowdvm-dev/review-opus-4`.
   - `crowdvm-server` on `127.0.0.1:9900` (`--webhook-allow-local`).
   - One `crowdvm-node`: qemu driver, KVM, UI on `:9901`, 6 vCPU / 12 GiB / 3 sandboxes.
   - Image `ubuntu-24.04-crowdvm-v3`, imported and pushed.
   - Driven with the `crowdvm` CLI and curl against `/api/v1`.
   - Teardown: I deleted my sandboxes, apart from the Wine sandbox, as I went. At 10:39:31 my server and node got a shutdown that I did not send. A later restart was cut short too, and my data dir was deleted from outside this review. Afterwards I checked: no process of mine was left, ports 9900/9901 were free, no `qemu-system` was running, and the dir was gone. Both blocking findings were reproduced before this happened.
5. **Independent sweep.** Two read-only Opus sub-reviewers. One took the a–l doc pages, with a script that checked every `boat …` command and flag in `cli-reference.md` against the `--help` of all 270 CrowdVM commands. The other took the m–z pages plus all 122 operations of `boat-v1.yaml`, with their parameters, body fields and enums. I re-checked every finding I kept, in code and, where marked, live.
6. **Live spot checks** of DONE/REQUIRED rows that no earlier review exercised closely (listed below).

## Fix status

F = fixed, P = partly fixed, O = still open.

| # | Finding | Status | Evidence |
|---|---|---|---|
| opus-3 N1 | Snapshot deletion operations never complete | **F (live)** | `internal/server/deletion_ops.go:65-67` treats non-sandbox ops as having no VM. **Live:** stop, then `snapshot delete --all bx_2a9fwjxy` created 3 `kind:"snapshot"` ops. All 3 were `completed/completed` with `attemptCount:1` on the next pass. One targeted the snapshot behind a saved snapshot (`revsave`), and `new --from revsave --count 2` still worked afterwards. |
| opus-3 N2 | 40% of `bx_` ids fail Boat's pattern | **F** | `internal/api/api.go:134` uses `23456789abcdefghjkmnpqrstuvwxyz` with an unbiased draw (`:137-151`), guarded by `internal/api/id_test.go`. Every id I minted live conforms (`bx_2a9fwjxy`, `bx_6pdz9x3f`, `bx_b278jdh2`, `bx_r2k2cc4k`, `bx_kjv23m6c`, `bx_mj7z7cqw`, `bx_cqbe4dht`, …). |
| opus-3 N3 | `run --personal` bills the active org | **F (live)** | `internal/cli/cmd_exec.go:380-383` sends `org:"personal"`. **Live:** with `acme` active, `run --personal --keep -- echo hi` created `bx_b278jdh2`. The DB shows `org_id IS NULL` (personal). Before that, `org migrate` had moved the other sandboxes to `team_…`. |
| opus-3 N4 | Bedrock IAM access keys | **F (code)** | `internal/server/feat_ai.go:1514-1516` sets `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and the optional `AWS_SESSION_TOKEN`. The guest detects them (`internal/guest/agents/harness.go:350`). Test at `feat_ai_test.go:360`. Not run live: there are no AWS credentials here. |
| opus-3 N5 | `org migrate` forces private + logins | **F (live)** | `internal/server/acct_misc.go:241-245` applies the org default and keeps `holds_creator_logins`. **Live:** after `org migrate`, `--no-env` sandbox `bx_6pdz9x3f` came out `visibility:shared, holdsCreatorLogins:false`. A sandbox created with the base environment (holds logins) stayed `private`. |
| opus-3 N6 | Low-disk cleanup deletes built Docker images | **O (live)** | See B1. `internal/guest/sampler.go:184` → `pruneImages(ctx, true)`. The "pulled" test at `internal/guest/docker.go:239` is "has a `sha256:` digest", which built images also have on the shipped Docker. |
| opus-3 N9 | `org members` hides pending invites | **F** | `internal/cli/cmd_admin.go:494-520` merges `GET /orgs/{org}/invites` rows with `status:"pending"` (the server sets `status` at `orgs.go:534`). |
| sonnet-3 N1 | `/healthz` returns the dashboard | **F (live)** | `internal/server/routes.go:23-31`. **Live:** `curl /healthz` → `{"ok":true,"version":"dev"}`. |
| sonnet-3 N2 | Share codes `csh_` instead of `bsh_` | **F** | New codes are `bsh_` (`internal/api/api.go:127`, `snapshots.go:900`). Old `csh_` codes are still accepted (`snapshots.go:976`). |
| sonnet-3 N3 | Steer on a stopped sandbox: wrong code | **F** | `internal/server/passthrough.go:207-210` returns `409 no_running_turn` with `reason:"not_running"`, as at `boat-v1.yaml:7130`. Interrupt keeps `machine_not_running`, which is fine: the spec defines no `no_running_turn` for interrupt. |

## Blocking findings

### B1. Low-disk cleanup still deletes images built in the sandbox (opus-3 N6, not fixed)

- **Boat** (`troubleshooting.md:123`): "boat deletes caches that it can download again: the apt cache, journal logs and **pulled** Docker images that no container uses. boat never deletes your own files."
- **CrowdVM:** at 95% disk, `sampler.go:178-185` calls `pruneImages(ctx, true)`. `imageLines` keeps only rows whose `{{.Digest}}` starts with `sha256:` (`docker.go:235-246`). The unit test (`docker_test.go:113-115`) assumes the classic store, where built images show `<none>`.
- **The shipped sandbox image** runs Docker `29.9.3` with storage driver `overlayfs [[driver-type io.containerd.snapshotter.v1]]`, the containerd image store. On that store a built image also has a digest, and `RepoDigests` too:

  ```
  sha256:9e3b… mybuilt:1   sha256:9e3b…       (docker build)
  sha256:d9e8… alpine:3.20 sha256:d9e8…       (docker pull)
  mybuilt:1  RepoDigests=[mybuilt@sha256:9e3b…]
  ```
- **Live reproduction** on `bx_2a9fwjxy`:
  - Images: `mybuilt:1` (built), `alpine:3.20` and `busybox:1.36` (pulled), and a running container `keepme` on busybox.
  - `sudo fallocate -l 50G /var/tmp/fill` brought `/` to 100%. The server logged `sandbox notice … kind=disk_usage_95`.
  - About 45 s later only `busybox:1.36` was left. **`mybuilt:1` was deleted.** Alpine was deleted too, which is correct.
  - PARITY LIFE-044 says "never images built in the sandbox". That is false on the image CrowdVM ships.
- **Why supportable:** pure guest logic. One option is to treat an image as pulled only when containerd recorded a registry origin for it: the `containerd.io/distribution.source.*` content labels that pulls add and builds don't. Another is to have the guest remember the image ids it saw pulled. When it is unsure, the cleanup should not delete. I did not finish checking the label approach.
- **Mitigating context:** Boat's own snapshot drops images no container uses (`snapshots__whats-saved.md:35`), so an unused built image does not survive a stop on Boat either. The harm is that it disappears while the sandbox is still running, which Boat promises won't happen. The team claimed this as fixed.

### B2. Scoped-key sandbox listing cuts the page before filtering (new)

- **Boat:**
  - `GET /sandboxes` is cursor-paginated ("follow `nextCursor`").
  - `security.md`: a key limited with `--sandbox` "reaches one sandbox only". It should still list that sandbox.
  - The platform guide's tenant pattern lists a tenant's sandboxes with its own key.
- **CrowdVM:**
  - `handleListSandboxes` runs SQL with `LIMIT limit+1` over every sandbox of the owner or org (`internal/server/sandboxes.go:491-503,542-543`).
  - Only then does it drop the rows the key may not see (`sandboxes.go:554`, `apikeys.go:76-91`).
  - `PageResult` (`router.go:398-410`) sets `hasMore` from the filtered list.
  - So the key sees a short or empty page with `hasMore:false` and `nextCursor:null`, and its other sandboxes are unreachable through the list.
  - Affected keys: those limited with `--sandbox`, those limited with `--env`, and keys that see only the sandboxes they created (`CreatedByKey`).
  - `handleListSnapshots` already filters before paginating (`snapshots.go:426-445`).
- **Live:** with key `pgtest3` (`--sandbox bx_b278jdh2 --preset read-only`) and two newer personal sandboxes:

  ```
  GET /sandboxes?limit=1 → []              hasMore:false nextCursor:null
  GET /sandboxes?limit=2 → [bx_b278jdh2]   hasMore:false
  ```
  With the default `limit=100`, the same happens once the account has 101 newer sandboxes the key can't see. Stopped sandboxes count. A platform account that hands out per-tenant keys reaches that quickly.
- **PARITY:** API-060 (REQUIRED) and KEY-015 (REQUIRED) claim this works.
- **Why supportable:** server logic. Push the key's sandbox, environment and creator filter into the SQL `WHERE`, or keep fetching until the page is full.

## Non-blocking findings (fix when convenient)

| ID | What | Evidence |
|---|---|---|
| m1 | `crowdvm billing extra-starts on --yes` fails with `unknown flag: --yes`. Boat documents `--yes` for scripts (`cli-reference.md:1426`, `limits.md:51`). | `internal/cli/cmd_billing.go:114-144` defines only `--monthly-cap`. Reproduced live. Accept `--yes` as a no-op, the way `stop --yes` is. |
| m2 | `prompt --attach` only uploads the file and adds its path to the prompt text. Images are not sent inline to vision models, and `PromptEvent.data.attachments` is never filled. | `internal/cli/cmd_agent.go:93-109`. `client.PromptRequest` has no attachments field (`internal/client/types.go:480-488`). The guest already supports `attachments` (`guest/agents/http.go:32,116,139`). Minor, because harnesses can open `~/attachments/x` by path. `docs/gaps/B.md:37` calls this FIXED. |
| m3 | `PromptEvent.data.citations` and `/me` `user.closureContactEmail` are never produced. | Both are in the spec schema only; no doc page describes them. |
| m4 | The fix for opus-3 N12 (notice noise) was not re-checked. | Out of scope. |

## Live spot checks of rows no earlier review exercised (all passed)

- **ENV private registries (ENV/`environments__private-registries`):** `env set-var base NPM_TOKEN=…`, `env set-file base .npmrc` and `.docker/config.json`. A new sandbox saw `[npm_xxx]`, the `.npmrc` with `${NPM_TOKEN}` unexpanded, and both files with mode `600`.
- **Always-on services (`long-running-tasks__always-on-services`):**
  - `extend --no-auto-stop` cleared `archive_after`.
  - I installed an enabled systemd unit serving `:8080` and ran `host 8080`.
  - After a real stop (`archived` after 6 s) and resume, the unit was `active`. The same URL and the same `_token` served the page, and `host 8080` returned the same token.
- **File API:**
  - `POST /files/replace` gave per-file results, a missing file did not fail the batch, and `replacements:2`.
  - `GET /files?path=` read the file.
  - `GET /files/search?pattern=` and `GET /files/find?pattern=&regex=true` work.
  - `upload-url` PUT of 3 MB and `download-url` GET round-tripped byte for byte.
- **Detached commands:** `exec --detach`, then `exec --attach PID` streamed the output and exited with the process's code (7).
- **Monitoring and snapshots off** (DATA-010/011, CLI-041):
  - `new --type small --no-monitoring --no-snapshots`.
  - `GET /command-history` and `GET /processes` return `409 monitoring_off`.
  - After stop, `resume` and `fork` return `409 snapshots_disabled`.
- **`new --from <saved> --count 2`** created both sandboxes from one request. The one that fit started, and the saved snapshot's Docker image was present.
- **`list` filters:** `crowdvm ls` defaults to running sandboxes, which matches Boat (`boat list` = `--filter r`).
- **Wine (WIN-002/003):** Boat's `install-wine.sh`, copied verbatim from `windows-apps__install-wine.md`, run as Boat documents with `crowdvm ssh ID -- bash -s < install-wine.sh`. **Partly verified.**
  - These steps succeeded: the i386 multiarch, the WineHQ key and sources, the `libgd3` pin, `apt-get install winehq-staging mesa-vulkan-drivers…`, winetricks, `wineboot -i`, and `winetricks corefonts` (every font registered).
  - At 10:39:31 the run was in `vcrun2022` when my server and node received a shutdown that did not come from me. Shortly afterwards my whole data dir was deleted from outside this review, so I could not resume the sandbox to finish the check.
  - Nothing that ran points to a gap.

The sub-reviewers also checked, in code: CLI-060 resume semantics, the DESK-005/006/007 token handling, recover refusals, share/unshare key revocation, ZDR blocking saved snapshots, rate-limit headers, attachment caps, the auth.md self-registration endpoints, OTel and webhook headers and limits, and `ETag`/304 on events. All held.

## Disputed classifications

- **LIFE-044 (ADAPTED):** the claim "never images built in the sandbox" is false on the shipped image (B1).
- **API-060 / KEY-015 (REQUIRED):** scoped-key listing paginates wrongly (B2).
- **AGT-019 / CLI-102 (REQUIRED) and `docs/gaps/B.md:37`:** "images inline … FIXED" covers only the guest half (m2).
- **CLI-120 (ADAPTED):** `extra-starts on --yes` must at least parse (m1).
- **N/A rows:** I read all 26 again and agree with every one: payments, trials, Google/AgentID OAuth, SOC 2 and status page, support channels, Box migration, release channels, documented absences.
- **ADAPTED rows:** I accept the ones opus-3 and sonnet-3 accepted. I have no new objection.

## What would flip this to AGREE

1. **B1:** make the low-disk cleanup keep images built in the sandbox on the containerd image store. Add a test that uses the containerd-store `docker image ls` output, or an e2e step: build, fill the disk, check the image is still there.
2. **B2:** apply the API key's sandbox, environment and creator scope before pagination in `GET /sandboxes`. Add a test: a scoped key with more than `limit` newer sandboxes it can't see still lists its own sandbox, or gets a cursor.

m1–m3 and the PARITY wording would be nice to have. They do not block AGREE.

## Not verified

- Windows nodes.
- VFIO GPU passthrough.
- Real Bedrock IAM, Claude or Codex credentials.
- Real GitHub or Gitea CI.
- SMTP.
- A refused stop with a real save failure.
- The dashboard beyond what the sub-reviewers read in code.
- The containerd-label fix idea for B1. I only got as far as showing the problem.
- The end of the Wine install (`vcrun2022`, `dxvk`, `d3dcompiler_47`). The cluster was shut down from outside this review partway through.

## TL;DR

DISAGREE, narrowly. Five of opus-3's six blockers and all claimed residuals are fixed. I confirmed N1, N3 and N5, plus `/healthz` and the id format, live. Two supportable features are still broken, both reproduced live and both small:
- **B1:** the low-disk cleanup still deletes Docker images built in the sandbox. The shipped Docker 29 uses the containerd store, where built images have digests too.
- **B2:** `GET /sandboxes` with a scoped API key paginates before filtering, so the key can get an empty page with `hasMore:false`.
