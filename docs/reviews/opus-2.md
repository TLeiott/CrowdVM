# Review opus-2: does CrowdVM cover every LAN-supportable Boat feature?

## Verdict: DISAGREE (narrowly)

CrowdVM covers almost all of Boat. All 122 operations in `.ref/boat-v1.yaml` are routed, every real `boat` subcommand exists as a `crowdvm` subcommand, and nearly every workflow I ran as a coding agent behaved the way Boat's docs describe. I still have to answer DISAGREE. One lifecycle path silently loses data where Boat refuses. Two documented CLI commands always fail. A few small features that work fine on a LAN are not shipped, even though `docs/PARITY.md` says they are.

None of the gaps is architectural. I'd expect all of them to be fixed in a day or two.

## Method

I did not use `docs/PARITY.md`, `docs/gaps/*.md` or `docs/E2E.md` as evidence. I read them only at the end, to find claims to dispute.

1. `make build`, then my own cluster: `crowdvm-server` on `127.0.0.1:9200` and two qemu nodes (`rev-node-1`, `rev-node-2`, 4 vCPU / 8 GiB each, UI ports 9201/9202). The base image `ubuntu-24.04-crowdvm-v2.qcow2` was added with `image import` and `image push`. Two local accounts (`rev` admin, `bob`) and one org (`Acme`). At most 3 VMs ran at once.
2. API coverage. A script hit every Boat operation with dummy ids and recorded the status and error code. All 122 answered with a real handler: 200/201/202, a validation 400, or a domain 404 such as `sandbox not found`. None answered 501 or an unknown-route 404.
3. CLI coverage. I pulled every `boat <cmd> [sub] --flag` from the code blocks of all 219 pages and checked it against `crowdvm ... --help`. The only misses are Stripe commands (`billing portal|cancel`, `wallet pay|card|auto-top-up`) and OAuth flags (`--google`, `--agentid`). `env set --boat-credentials` works as a hidden alias of `--crowdvm-credentials`.
4. Behaviour. I worked through quickstart, cli-reference and the guides in the order an agent would meet them. I ran each workflow against real VMs and compared the output with the docs. The test list is below.

### What I ran and saw working

- **Lifecycle.** `new` with `--name --env --setup-file --file --host 3000 --host 8080:public --ttl` took 4 s to ready. Setup ran as `user` and `setupStatus` went to `done`. Also tested: `stop`, `stop --status <op>`, `resume --type default --ttl -e` (it moved to the other node), `resume --no-wait` followed straight by `exec` (3 s), `fork --wait --type -e --ttl` (`-e` replaced the per-sandbox vars as documented), `extend --hours/--no-auto-stop`, `rename`, `recover` (refused with `sandbox_is_reachable` on a healthy box), `delete` and `deletion status`, `--fail-fast` (`no_ready_machine`, 503), and TTL auto-stop after 60 s. A `--no-snapshots` sandbox could not be resumed after its stop.
- **Run work.** `exec` with exit codes, `--cwd`, `--timeout` (124), `--timeout 0` streaming, `--stream`, `--detach/--status/--attach/--kill`. `history` list/output/`--stderr`/`--follow`/`--mode process`, `ps` (also on a stopped sandbox, from the last sample), and `run --push -e`, which deleted its sandbox. `push` is git-aware and keeps symlinks and exec bits. `scp` up, down and `-r`, `ssh` with a command, argv and stdin script, `ssh-config`, `forward` and `forward --reverse`. Files API: read, write, list, info, search, find, replace, move, permissions, folder, delete, signed upload/download URLs and `artifacts`.
- **URLs.** Private `_token` redirect plus cookie, `Cookie: _port_auth`, `--public`, in-sandbox `host`, `host list|url|hide`, `--localhost` relay, the subdomain rename through `PATCH` (old host stops resolving) and WebSocket upgrade through `/p/<id>/<port>/`.
- **Desktop.** `desktop` returns `desktopUrl`/`viewerUrl`. `browser --profile /data/chrome-profile` starts a kiosk Chrome on its own display. `ascii-record-desktop start|status|stop` writes an MP4, and `~/.config/boat/wallpaper.png` is applied. noVNC replaces Moonlight, which I accept as ADAPTED.
- **Snapshots.** `snapshot ID NAME`, `snapshot list|info|share|shares`, a share code deployed by the second account with no owner vars, `new --from NAME --count 2`, `snapshots`, `snapshot latest|tree|pull --path`, and `snapshot self` from inside a sandbox. State persists across stop, resume and fork, and enabled systemd units come back.
- **Environments.** `env new|set-var` (stdin)`|set-file|set|upgrade|access --only/--everyone`, an org environment refused to a non-listed member, `new --environment <unknown>` refused before any VM is made, a repo added by git URL and cloned at its base branch, and `--safe-for-third-parties` plus `env upgrade`. The upgrade removed the vars, secret files and `CROWDVM_API_KEY` from a live sandbox, and its in-sandbox CLI then got `api_key_action_forbidden` on create.
- **Integrated agents.** I used OpenCode with its free default model, since it needs no key. `prompt --new`, two conversations in parallel, `--attach` (saved to `~/attachments`, and the model read it), `--resume <id>`, `conversations`, `events --convo`, `steer` (mode `fallback`, as Boat documents for OpenCode) and `interrupt --convo`. Steering one conversation left the others alone.
- **AI credentials.** I restarted the server with `CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE=1`, ran `ai profile set-api default MOCKAPI --base-url http://127.0.0.1:9213/mock` and `ai proxy on`. The sandbox got `MOCKAPI=cvmai_...` and `MOCKAPI_BASE_URL=http://10.0.2.2:9200/ai-proxy/custom/MOCKAPI`. My receiver saw `Authorization: Bearer realsecret`. `ai block` made the proxy return 403, `ai unblock` restored it, and `ai usage <id>` counted the requests. Direct access from the guest to host loopback failed (`curl` rc 7).
- **Account and teams.** `api-key create --sandbox --actions` was enforced: another sandbox got `api_key_sandbox_forbidden` and `stop` got `api_key_action_forbidden`. Also checked: `--preset ci` cannot list or create keys, `rotate` invalidates the old secret, `revoke`, and `usage`. Webhooks deliver `archived`, `hydrated` and `ready`, and every `X-Ascii-Signature` verified with `HMAC_SHA256(secret, id.ts.body)`. The OTLP test export plus metrics and logs carried my custom header. Org create, invite and join worked, and shared/private semantics match `organizations__shared-sandboxes`: a sandbox holding the creator's logins stays private, `share` returns `restartRequired: true`, the member gets `use` only after a creator-side restart, and a member cannot stop a sandbox the creator made private. ZDR enabled through the API deletes a sandbox when it stops and queues the ones already archived. `--no-monitoring` gives `monitoring_off` on `history` and `ps`.
- **Agent onboarding.** `/auth.md`, `/.well-known/oauth-protected-resource`, anonymous `POST /agent/identity`, and `login --json` returning a device URL and code.
- **Other.** Nested KVM (`/dev/kvm` in the guest), raw direct ports after setting a node `direct-host`, the Python SDK smoke test (`sdk/smoke/smoke.py`: `PYTHON SMOKE OK`), and node daemon restarts with VMs still running.

## Supportable features that are missing or broken

Ordered by severity.

### 1. Resume after deleting all snapshots boots a blank disk and reports `ready`

Boat pages: `snapshots__download-and-delete`, `cli-reference#boat-resume-id`. Boat says a resume needs a completed snapshot, and once the snapshot data is gone, "Resume and fork fail with `409 snapshot_data_expired`".

CrowdVM accepts the resume, boots the base image and reports success. The user's files are gone and nothing says so.

```
$ crowdvm snapshot delete --all bx_b152f3w66x --yes      # sandbox stopped
$ crowdvm info bx_b152f3w66x   → {"state":"archived","snapshotAvailable":false}
$ crowdvm resume bx_b152f3w66x → {"event":"ready",...}
$ crowdvm exec bx_b152f3w66x -- 'cat ~/persist.txt; ls ~/proj'
cat: /home/user/persist.txt: No such file or directory
ls: cannot access '/home/user/proj': No such file or directory
```

Cause: `snapshot_data_expired` is only returned when retention set `sb.Extra["snapshotDataExpired"]` (`internal/server/feat_retention.go:264-266`). After an explicit delete, `handleResume` calls `chainRestorable(snapshotChain(sb.HeadSnapshotID))` on an empty chain (`internal/server/lifecycle.go:1087`) and goes ahead. Fork goes through the same check. An agent that deletes old snapshots to save space and resumes later gets a fresh VM and no error, which is the worst way for this to fail.

### 2. `crowdvm data-retention enable` always fails

Boat page: `cli-reference#boat-data-retention`, `data-retention__zero-data-retention`.

```
$ crowdvm data-retention enable --yes
{"code":"confirmation_required","error":"To enable zero data retention, send confirmation: \"delete archived sandbox data\".", "status":400}
```

The CLI sends `"enable zero data retention"` (`internal/cli/cmd_admin.go:624`). The server expects Boat's phrase `"delete archived sandbox data"` (`internal/server/feat_retention.go:30`). The API, the dashboard (`web/js/pages/account.js:154`) and the SDKs send the right phrase, so only the CLI path is broken. With the right phrase sent through the API, ZDR worked.

### 3. `crowdvm org default-visibility` without an argument errors

Boat page: `cli-reference#boat-org-default-visibility` ("shows the current value"), `organizations__shared-sandboxes`.

```
$ crowdvm org default-visibility
{"code":"no_changes","error":"Pass name or newSandboxesPrivate.","status":400}
```

The show branch sends an empty `PATCH /orgs/{org}` (`internal/cli/cmd_admin.go:583`) where it should read `newSandboxesPrivate` from `GET /orgs`. Setting the value works.

### 4. No `crowdvm` skill in the harness skills directories

Boat page: `integrated-agents__customize` ("Every harness on a sandbox already has a skills directory with the `boat` skill in it"), `ai-tools`.

```
$ crowdvm exec <id> -- 'ls ~/.claude/skills ~/.codex/skills ~/.pi/agent/skills ~/.config/opencode/skills ~/.prime/agent/skills ~/.kimi-code/skills'
(all six empty)
```

Nothing named `SKILL.md` exists under `internal/`, `images/` or `cmd/`, and the repo has no `skills/` folder. `docs/PARITY.md` AGT-025 and DOCS-003 say a `crowdvm` skill ships in the image. The platform system prompt does exist (`internal/guest/agents/harness.go:110`, `--append-system-prompt`). The skill is the missing half, and an agent running inside a sandbox has no packaged guide to the in-sandbox `crowdvm` and `host` commands.

### 5. No docs for agents: llms.txt, per-page .md, docs MCP server, skill pack

Boat page: `ai-tools` §2, which lists the MCP server, `llms.txt`, `llms-full.txt` and `<page>.md` as the way to give an agent the docs, plus `npx skills add ...`.

```
GET /llms.txt              → 404
GET /llms-full.txt         → 404
GET /docs/quickstart.md    → 404
POST /mcp (tools/list)     → "method not allowed"   (GET /mcp is the dashboard SPA fallback)
```

All of this can be served from the coordinator on a LAN. PARITY rows DOCS-001/002/003 are marked ADAPTED with an implementation plan, but none of it exists. `crowdvm agent-instructions` covers the AGENTS.md snippet (DOCS-004), and the CLI help is detailed, so this matters less than items 1 to 3. It still matters for a product built for agents.

### 6. `host --public` advertises a raw endpoint the guest firewall blocks

Boat page: `hosting#raw-tcp-or-udp-ports`: "`boat host <id> <port> --public` also opens that TCP port on the IP address". `cli-reference#boat-host-id-port`: "The command opens the sandbox firewall".

On a node with `direct-host` set:

```
$ crowdvm host bx_db3nwmsws6 8000 --public  → {..., "directEndpoint":"127.0.0.1:8000"}
$ curl -m 5 http://127.0.0.1:8000/          → timed out, 0 bytes
$ crowdvm exec bx_db3nwmsws6 -- 'sudo ufw status'   → only 22/tcp and 7070/tcp allowed
$ crowdvm exec bx_db3nwmsws6 -- 'sudo ufw allow 8000/tcp'; curl http://127.0.0.1:8000/  → raw-8000
```

The node listens, but `host` never runs `ufw allow`. The comment in `internal/server/feat_direct.go:10` says this is on purpose ("the guest firewall applies"), and that contradicts Boat's documented behaviour. A related gap: Boat's UDP recipe (`ufw allow 27015/udp`, then connect to `ip:27015`) only works in CrowdVM after `PUT /sandboxes/{id}/direct-ports`, which has no CLI command and is not in the user guide's CLI section.

### 7. `push --to` outside `/home/user` fails

Boat page: `cli-reference#boat-push-id-dir` ("`boat push bx_... ./api --to /srv/api` ... boat creates it if it does not exist").

```
$ crowdvm push bx_b152f3w66x ./proj --to /srv/api
{"error":"extract on the sandbox failed: mkdir: cannot create directory '/srv/api': Permission denied","event":"error"}
```

The extract runs `mkdir -p` and `tar` as `user` (`internal/cli/transfer.go:177`). Boat's own example fails.

### 8. Cosmetic CLI output

These don't block parity, but an agent parsing JSON will trip on them.

- `crowdvm env new <name>` prints `{"id":"","name":"",...}`. `POST /environments` returns the environment list and the CLI decodes it as a single environment (`internal/client/account.go:366`).
- The `crowdvm run` summary line has no cost (`exit 0 · 4s · small · bx_... deleted`). Boat prints `$0.0140` there, and the credits equivalent is available.
- `push` reported `"files":4` for 5 files when one of them was a symlink.

## Disputed N/A and ADAPTED classifications

I checked the 26 N/A rows in `docs/PARITY.md`. Every one is payments, third-party OAuth or SaaS (Google, AgentID), Boat-legacy migration, or operator process, so I don't dispute any N/A row. Within the owner's rules, credits replacing billing, local accounts replacing OAuth, LAN host-routed URLs replacing public HTTPS, noVNC replacing Moonlight and CI polling with a PAT replacing the GitHub App all keep the feature's purpose.

I dispute the ADAPTED rows whose stated adaptation was never built:

| Row | Claim | Reality |
| - | - | - |
| AGT-025, DOCS-003 | `crowdvm` skill in every harness skills dir, repo `skills/` folder | Not shipped (item 4) |
| DOCS-001 | Coordinator serves `/llms.txt`, `/llms-full.txt`, `/docs/<page>.md` | 404 (item 5) |
| DOCS-002 | Docs MCP server on the coordinator | Absent (item 5) |
| URL-012 | "`host --public` also opens the TCP port" | Port published on the node but blocked by the guest firewall (item 6) |
| §4 preamble and AIC-013 | "custom-API targets may be private or `http://`" | Refused by default with `invalid_base_url`. Only the undocumented env var `CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE=1` allows them (`internal/server/feat_ai.go:988-996`). The row's "when the admin enables it" fits, but there is no `serve` flag and the user guide doesn't mention it. A LAN admin won't find it. |

## Not verified

I could not run these here. They are neither confirmed nor refuted.

- CI runners. They need a GitHub token and a real workflow. I only read the controller design (`internal/server/feat_ci.go:1-12`, polling instead of a GitHub App, which I accept as ADAPTED).
- Claude Code, Codex, pi, Prime and Kimi harnesses, and subscription sign-ins. I did not use real provider keys. Native steer modes and computer-use tools are untested.
- Wine (`windows-apps`), the Android emulator, the TypeScript SDK, upstream `harbor-box`, `@asciidev/eve-box` and AgentEnv. The Box alias `GET /api/box/v1/boxes` answers.
- Dashboard UI interaction. I only checked that a page module exists for each Boat dashboard tab (`web/js/pages/*.js`).
- Email-code login over SMTP, and a Windows node.
- `stop` refused-save handling and `stop --force`.

## Environment notes

- Another session's scratchpad filled the `/tmp` tmpfs during my run (`/tmp/claude-1000/-home-tl-dev-CrowdVM/77ca4ba5-...`, about 25 GB). I left it alone. From then on I wrote command output under my cluster directory instead.
- A concurrent e2e run (`~/.local/share/crowdvm-dev/e2e/run-1108-201315-*`) shared the machine. My cluster used ports 9200 to 9202 and 9210 to 9213 only.
