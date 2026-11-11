# Review sonnet-11: does CrowdVM support every supportable Boat feature?

Reviewer: Claude Sonnet 5.5 (independent). Date: 2026-11-10. Tree: uncommitted working tree on `master`. I built the binaries into my scratchpad with the Makefile flags (`bin/` untouched).

## Verdict: DISAGREE (three blockers, all small and LAN-safe)

All five claimed fixes from `sonnet-10` (B1–B4) and `opus-12` (B1) hold. I checked the dashboard in a real browser and the scoped keys with curl against my own cluster. I found no regression next to them.

My sweep of snapshots, files, SSH/editors and hosting found three documented behaviours that are wrong. All three are guest- or CLI-side and need nothing from the internet.

- **B1.** Boat's "Keep a hostname" recipe (`snapshots__whats-saved.md`) does not work. The guest agent overwrites the hostname after the user's enabled unit sets it (`internal/guest/boot.go:83`, `:222`).
- **B2.** Docker images of a Compose service that is not started by `up` are not saved when the service is profile-gated. The prune asks `docker compose config --images` without `--profile "*"` (`internal/guest/docker.go:330`).
- **B3.** `crowdvm snapshot pull <id> -o DIR` never writes the `docker/` folder that Boat documents for Docker named volumes. The CLI asks for a path the server answers 404 for, and swallows the 404 (`internal/cli/cmd_snapshot.go:334`, `:342`).

## Method

1. **Read.** `sonnet-10.md` and `opus-12.md` (claims and flip criteria), and the earlier review citation counts per `.ref/pages` name. I read `snapshots*.md`, `ssh-access*.md`, `hosting*.md`, and the files API pages plus the `boat-v1.yaml` file endpoints (there are no `files*.md` pages; the file recipes are in `ssh-access.md` and the API reference).
2. **Build and tests.**
   - `go vet ./...` is clean.
   - `go test -count=1` on every package except `test/e2e` is all `ok`.
   - I did not run e2e.
   - I did not run `make build`, because it writes `bin/`. I used the same `go build` commands and flags into my scratchpad.
3. **Own cluster.** Ports 9970 (server), 9971 (node UI). Data dir `~/.local/share/review-sonnet-11` with `HOME` and `CROWDVM_CONFIG_DIR` isolated. One qemu/KVM node: 6 vCPU, 12 GiB, 3 sandboxes, image `ubuntu-24.04-crowdvm-v3`, `net.allow 192.168.178.31/32`. I later set `direct-host 192.168.178.31` and restarted the node (running VMs survived the restart). Users `admin` and `bob`, org `acme`.
4. **Dashboard.** A real browser through the preview tool, signed in as `admin` and then `bob`, driving the org page, the AI credentials page and the sandbox list. I asserted state with `crowdvm org list` and `crowdvm ... ai profile access` after each click.
5. **Sweep.** Boat's recipes run as a coding agent would: CLI, curl and the Python/TypeScript SDK samples of pages the e2e sample runner does not cover (`snapshots__templates`, `snapshots__resume-and-fork`, `snapshots__stop`, `hosting`, `hosting__host-cli`, `ssh-access`, `ssh-access__editors`; the e2e list in `sdk/tools/doc_samples.py:35` has none of them). I ran the runner from my scratchpad with those pages.
6. **Teardown.** I deleted every sandbox, stopped the server and node by saved PID, and confirmed no process with my data dir and nothing on 9970–9979. I deleted my data dir and closed my browser tab. I did not touch the other reviewer's processes, did not run e2e, and used no sub-agents.

## Fix status

| # | Claimed fix | Status | Evidence |
|---|---|---|---|
| sonnet-10 B1 | Organization tab has Transfer and Delete; role dropdown has no `owner` | **Fixed (live, browser)** | `/org` as owner shows "Transfer ownership…" and "Delete organization". Transfer: pick `bob`, a second dialog "Make bob the owner?" confirms; `crowdvm org list` as bob then says `role:"owner"`, and admin shows `member`. I transferred it back with `crowdvm --org acme org transfer admin`. Delete: a throwaway org `tmpdel`, dialog "Type tmpdel to confirm", the button enables, the org leaves `crowdvm org list`. A plain member (bob) sees "Leave organization" and no Delete. The members table role dropdown holds `member` and `admin` only. |
| sonnet-10 B2 | AI profile "Who can use this profile" panel | **Fixed (live, browser)** | `/ai`, viewing `acme`, profile `team-keys`: panel shows "Everyone in acme" and an Edit button. The modal has "Everyone in acme" / "Only the people ticked below" with a member checklist. I chose "only", ticked nobody, saved. Panel then reads "Only these members (and owners and admins): nobody else". `crowdvm --org acme ai profile access team-keys` returns `{"restricted":true}`. |
| sonnet-10 B3 | `view only`/`private`/`shared` labels, `by <creator>`, permission-aware menus | **Fixed (live, browser)** | As admin (creator), rows show `private` (tooltip "Your teammates see it as view only…") and `shared` (a `--no-env` sandbox). As bob, rows show `bx_… · by admin` and `view only` for the two private ones; none for the shared one (API: `use`/`view`). Menus: `view` rows lose Desktop/Browser/Console/SSH/Save/Recover and Resume. `use` and `view` rows keep Fork/Auto-stop/Rename/Delete but `sbx.js:361-363` makes them disabled with "Only its creator can do this." (Boat hides them; I accept disabled-with-reason as equivalent.) |
| sonnet-10 B4 | Viewing selector separate from the billing wallet | **Fixed (live, browser)** | Sidebar has "Viewing" and, below it, "CLI and API keys bill: Personal · Change". Picking `acme` in Viewing left `crowdvm org list` at `active:true` for Personal. `Change` calls `PATCH /orgs/active` (`main.js:119`). The dashboard scopes requests with `X-CrowdVM-Org` (`api.js:14-23`). With a key, `POST /sandboxes` with `X-Boat-Org: acme` and with `X-CrowdVM-Org: acme` both created an acme sandbox and the active wallet stayed Personal. |
| opus-12 B1 | Scoped keys no longer reach other sandboxes' saved snapshots | **Fixed (live)** | Setup: sandbox B saved `rv-secret`; key limited to sandbox A with `sandbox.create,sandbox.read,snapshot.read,snapshot.write,exec`. `GET /sandboxes/B` → 403 `api_key_sandbox_forbidden`. `GET /saved-snapshots` → `snapshots: []`. `GET /saved-snapshots/rv-secret` → 404 `snapshot_not_found`. `POST /sandboxes {"from":"rv-secret"}` → 404. `POST /saved-snapshots {"name":"rv-secret","sandboxId":A}` → 403 (overwrite blocked). `GET …/shares` and `DELETE` → 404. Its own `a-own` saves and lists. An environment-limited key (`--env e1`) is refused the same way. The in-sandbox `CROWDVM_API_KEY` of A lists only A's three saved snapshots and gets 404 for `rv-secret`. Code: `snapshots.go:433`, `:481`, `:770` (`AllowsSandbox`). |

One cosmetic leak (not a blocker): `allowance.used` in `GET /saved-snapshots` counts snapshots the key cannot see (`used:5` with `snapshots:[]`).

## New missing or broken supportable features

### B1. "Keep a hostname" recipe fails (`snapshots__whats-saved.md`, "Keep a hostname")

- **Boat says.** The hostname resets at every resume and fork. To keep one, create an enabled systemd oneshot that runs `hostnamectl set-hostname my-sandbox`. "Enabled services start again after each resume and fork."
- **CrowdVM.** I ran the recipe verbatim (`set-hostname.service`, `systemctl enable --now`). `hostname` printed `my-sandbox`. After `stop`, `resume --type default`, `hostname` printed `a` (the sandbox name). I repeated it on a second resume: `a` again.
- **Evidence from the guest journal.** `set-hostname.service` ran and logged `Hostname set to <my-sandbox> (static)`. `crowdvm-guest.service` started in the same second. `hostnamectl` afterwards: `Static hostname: a`. The guest agent wrote it after the user's unit: `internal/guest/boot.go:83` calls `a.setHostname()`, which writes `/etc/hostname` and calls `Sethostname` (`:222-233`).
- **Why supportable.** Pure guest-side ordering; no network involved.
- **Fix.** Set the hostname in the early step that runs before the agent and before `multi-user.target` (`guest-prepare`, the unit's `ExecStartPre`), or have the agent skip `setHostname` if the hostname already differs from the one it booted with. Add a test that an enabled oneshot's hostname survives a resume.

### B2. Compose images of profile-gated services are not saved (`snapshots__whats-saved.md`, "Docker")

- **Boat says.** Saved: "Images of a Compose project that has a container, also services that you only start with `docker compose run`."
- **CrowdVM.** `internal/guest/docker.go:330` keeps `docker compose -p NAME … config --images` for each project that has a container. Without `--profile "*"` that list omits every service with a `profiles:` entry. Those are the services you usually start only with `docker compose run`.
- **Live.** `~/dk/compose.yaml`: `web` (nginx:alpine, `up -d`) and `tools` (busybox:1.36, `profiles: ["tools"]`, started with `docker compose --profile tools run --rm tools true`). After stop and resume `docker images` kept `nginx:alpine`, `alpine`, `myimg:v1` (has a container), and lost `busybox:1.36`. Inside the sandbox: `docker compose config --images` → `nginx:alpine`; `docker compose --profile "*" config --images` → `busybox:1.36` and `nginx:alpine`.
- **Same run, correct.** `myimg:v1` (built here, has a container) kept; `myimg:v2` (built, no container) pruned; `redis:alpine` (no container) pruned; the `keep-alpine` created container kept alpine; named volume `vol1` data back; the `web` container up again.
- **Why supportable.** Pure guest logic. The image is lost, so a built or private-registry image needs a rebuild or login.
- **Fix.** Add `--profile "*"` to the `config --images` call, and a unit test for a profile-gated service.

### B3. `snapshot pull` writes no `docker/` folder (`snapshots__download-and-delete.md`, "What `pull` downloads")

- **Boat says.** `boat snapshot pull` "writes two folders": `home_user/` and `docker/` (Docker named volumes).
- **CrowdVM.** `internal/cli/cmd_snapshot.go:334` asks the server for `/var/lib/docker/volumes`, which `path.Clean` + `TrimPrefix` turns into `var/lib/docker/volumes`. The server answers `404` for that relative form. `:342` then treats the 404 as "no Docker volumes in this snapshot" and skips it, silently.
- **Live.** Snapshot `snap_3bz2d39n6nk4ts2dga7w` (saved `ss1`, volume `vol1` holds `f`). The API has the volume: `GET /snapshots/{id}/tree` lists `__dockervol__/vol1/f`, `GET …/files?path=__dockervol__/vol1/f` → `dv`. `crowdvm snapshot pull {id} -o DIR` → `files:11542`, and `DIR` holds `home_user` only. `--path /var/lib/docker/volumes` → 404. `--path __dockervol__` writes `DIR/__dockervol__/volumes/vol1/_data/f`, not `docker/`.
- **Why supportable.** A CLI path bug; the server already has the data.
- **Fix.** Ask for `__dockervol__`, and map each volume to `docker/<volume>/` (flatten `volumes/<name>/_data`). Add an e2e check for a volume. `localName` (`cmd_snapshot.go:381-383`) already maps `__dockervol__/` to `docker/` but is never reached.

## Minor (not blocking)

- **m1. `.boatignore` inside `node_modules`.** Boat does not read ignore files inside `node_modules`, `.next`, `target`, `vendor`. In the test, `nm/node_modules/pkg/.boatignore` (`z.txt`) was read: `z.txt` was gone after a stop (`internal/guest/ignore.go:15` lists the dirs; the skip does not hold for a file two levels down). Everything else behaved like Boat: `*.log` with `!important.log`, `bigdir/`, `/build`, `docs/**/tmp` and `**/cache/` anchored to the file's folder, `.git/` always kept, `.gitignore` not read (`ignored.txt` kept), `.boatignore` over `.boxignore`, a `.boatignore` seven folders deep not read.
- **m2. Saved-snapshot `allowance.used`** counts snapshots hidden from a scoped key (above).

## Disputed or accepted classifications

- **SSH-004 / SSH-008 (ADAPTED).** On a node without `direct-host`, `POST /sandboxes/{id}/sshkey` returns `machineIp:null`, `sshEndpoint:null`, plus a `proxyCommand`. Boat's editors script then writes `HostName null`. I accept this: `crowdvm ssh-config <id>` writes a working `ProxyCommand crowdvm tunnel` entry (I connected with it, and used `ssh`, `scp` and `sftp`). With `direct-host` set, Boat's script (verbatim) works: it wrote `HostName 192.168.178.31`, `Port <p>`, `ssh`/`scp`/`sftp` connected with `StrictHostKeyChecking yes`, and after stop and resume the stale entry fails with `Connection refused` and a rerun fixes it. A one-line note in `docs/guide/cli-reference.md` that the default node needs `ssh-config` would help an agent.
- **URL-012 (ADAPTED).** `ufw allow 27015/udp` alone does not publish the UDP port; `crowdvm direct-ports ID 27015/udp 7000` does (UDP and TCP echo both answered on `192.168.178.31` from the host). `host --public` published TCP 7000 by itself (`directEndpoint`). Accepted.
- **Reserved ports.** Boat lists 22, 6080, 8090, 8911, 47984–48010. CrowdVM refuses only 22 and 6080. Hosting 8090, 8911, 47984 and 48010 succeeds. Accepted: those are Boat's own agent ports.
- **Default URL form.** `crowdvm host` prints `http://<server>/p/<id>/<port>/?_token=…`; the host-routed `<sub>-<port>.<domain>` URL is in `--json` as `hostUrl`. Both work (cookie, `_token`, `_port_auth` header, WebSocket 101, chunked SSE one event per second). The path form adds `Content-Security-Policy: sandbox …`. Accepted (no wildcard DNS on a LAN).

## Checked and fine

- **Whats-saved (restore after `stop` + `resume --type default`, which forces a new machine).**
  - Kept: `/home/user`, a second user's `/home/bob2` (mode 750), `/code` (root-level folder), `/etc/mytest.conf`, `/opt`, `/srv`, `/var/lib` changes, an apt-installed `cowsay`, `~/myapp/.git`.
  - Deletions persist: `sudo rm -rf /usr/local/bin/kimi` stays gone.
  - Not kept: `/tmp/*`, `~/.cache/*`, unused images (`busybox`, `redis`).
  - Services and containers: an enabled `marker.service` ran again (`marker.log` +1); the `web` container with a restart policy is up again.
  - `~/.boat/pre-stop` ran on `stop` (marker +1) and did not run on a saved-snapshot save or on `stop --force` (`forced:true` in the reply).
  - Playwright recipe: `sed -i '1i export PLAYWRIGHT_BROWSERS_PATH=…' ~/.bashrc` applied to `exec` (string and argv), `ssh`, `ssh -- bash -s` and an API command.
  - Poll loop: `until [ "$(crowdvm info ID --json | jq -r .sandbox.state)" = stopped ]` ends. Resume of a running sandbox with `{"type":"small"}` → `409 sandbox_running_stop_first`. API resume with `type` and `ttlSeconds` works; `large` on a full node → `out_of_capacity`.
  - Fork of a running sandbox (`--type small --ttl 600`): TTL 600, name `A (fork)`, carried the writes of the last seconds (a superset of Boat).
- **Templates.**
  - Save then deploy; re-save replaces (deploy gets `v2`; an earlier deployed sandbox keeps `v1`).
  - An old share code deploys the new save; the share is gone after `snapshot rm` (`404 snapshot_not_found`); the `daily-$(date +%F)` name saves.
  - A deploy gets the source's per-sandbox variables (`TENANT_ID`, `TOKENX`). The same deploy by another account through a share code gets none (`env | grep` empty, no `authorized_keys`).
  - `Idempotency-Key` retry returns the same sandbox with `idempotentReplay:true`.
  - Team image: `--org acme snapshot`, bob deploys with `--org acme new --from`, bob re-saves the name and removes it as its last saver.
  - `crowdvm snapshots` shows `allowance` with `included`, `used`, `pricePerExtraMonthlyDollars`.
- **Download.** Snapshot file API: absolute `/etc`, `/opt`, `/srv`, `/var/lib`, `/code`, `/home/bob2` paths, relative `proj/…`, `__dockervol__/…`, a folder as tar with `X-Snapshot-*` headers, `Range` → `206`. `latest`, `saved-snapshots/{name}` → `snapshot.snapshotId`.
- **Files API** against `boat-v1.yaml`:
  - A symlink to `/etc/passwd` → `400 invalid_path`; `../../etc/passwd` and absolute outside paths → 400.
  - `/home/user` and `/tmp` cannot be deleted; a directory needs `recursive`; deleting a symlink keeps its target.
  - A move onto an existing file → `409 destination_exists`, `overwrite:true` moves across `/tmp` and creates parents.
  - `search` and `find` (plain, and regex with `%2B`) skip binaries and do not follow symlinks.
  - `replace` per-file results (one missing file reported, not fatal), `newValue:""` deletes, a missing `newValue` → 400.
  - Permissions `600`, folder mode `700`, base64 round-trip.
  - 5 MiB limits both ways (`file_too_large` names the URL endpoints), 6 MB through `artifacts` OK, `ttlSeconds` over 3600 → `400 invalid_ttl`.
- **SSH.**
  - `crowdvm ssh` command forms, `bash -s` with stdin.
  - The editors script verbatim with `direct-host` (see above).
  - Host key refreshed after a resume.
- **Hosting.**
  - Protected URL: 401 without a token, `_token` redirect with a cookie jar, `Cookie: _port_auth`.
  - `--localhost` relay (a server bound to 127.0.0.1), `--public`, replace-a-leaked-token (old token 401, new 200), `host hide` keeps the token (same token after re-host), `host list` `(gated)` and `(localhost)`, `host url`.
  - The in-sandbox `host`, `host list`, `host url`, `host hide` through the commands API (stdout is the URL).
  - Subdomain `PATCH`: `acme-staging` moves the URLs; `ab`, `foo-desktop`, `foo-123`, `Foo_Bar`, `foo--bar` → `400 invalid_subdomain`; a taken name → `409 subdomain_taken`.
  - A WebSocket upgrade through both URL forms (101 plus the first frame), SSE chunks arrive one per second.
- **SDKs.** Every Python and TypeScript sample of `hosting` and `hosting__host-cli` passes (18/18). Failures in `ssh-access`, `snapshots__templates` and `snapshots__resume-and-fork` are the samples' own placeholders and ordering (fake `AAAAC3Nza…` key, `output.zip` that does not exist, `fs` undefined, resume of a running sandbox, `create` right after `save` without polling to `ready`), not product faults.

## Not verified

- Empty-wallet 7-day deletion of extra saved snapshots (needs credit enforcement on), the 14-day retention job, the 3-day forced stop, and the 10-minute same-machine window (only a resume right after a stop, which took 4 s).
- Snap packages reinstalled; Chrome pre-stop example; Windows and macOS nodes.
- The dashboard Snapshots tab (browse and download).
- Start-rate limits (`429 rate_limited`; my server ran with limits off).

## What would flip this to AGREE

1. **B1.** A systemd oneshot that sets the hostname still holds it after a resume and a fork, on a fresh run of Boat's "Keep a hostname" recipe.
2. **B2.** After stop and resume, the image of a profile-gated Compose service of a project that has a container is still present (`config --images` called with `--profile "*"`).
3. **B3.** `crowdvm snapshot pull <id> -o DIR` writes `DIR/docker/<volume>/…` for a snapshot with a named volume, and does not hide a 404 as "no volumes".

m1 and m2 are cheap and would close the gap with Boat's text.
