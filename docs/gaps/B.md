# Gap audit B: agents, AI credentials, accounts, keys, orgs, credits, webhooks, OTel, retention, CI, adapters, Wine, SDKs, CLI, API, install, dashboard, CrowdVM rows

Read-only audit of every REQUIRED/ADAPTED row in the sections above (N/A rows skipped), against the code on 2026-11-08.
Method: server routes were extracted from every `s.API`/`s.Route`/`HandleFunc` registration. The CLI client (`internal/client`), both SDKs (`sdk/python/src/crowdvm/_ops.py`, `sdk/typescript/src/operations.ts`) and every `api.*` call in `web/js` were matched against them with a script. Feature logic was then read layer by layer (server → node → guest → guest agents, CLI, SDK, web, images).
`internal/node/vm/qemu` and `vm/netstack` were not audited (another agent is changing them).

Checks run: `go build ./...` OK. `go test ./...` 17 packages, all OK. Cross-builds for `crowdvm-node` (windows/amd64) and `crowdvm` (windows, darwin/arm64) OK. Python SDK `unittest` 39/39 OK (run from `sdk/python`, `PYTHONPATH=src`; `pytest` is not installed). TypeScript SDK `node --test` 33/33 OK.

**Totals (530 rows incl. one synthetic "Windows node" row): DONE 401 · PARTIAL 88 · MISSING 41** (before fixes; EVAL-002, SDK-003, SDK-008 fixed since → DONE 404 · PARTIAL 85)

Status key: DONE means it works end to end (one evidence ref). PARTIAL lists exactly what is missing. MISSING means nothing usable exists. API rows for sandbox, file, command and snapshot endpoints are graded on the route and handler being present plus the guest path. Their deeper behaviour belongs to the sections other reviewers own.

---

## Integrated agents

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| AGT-001 | DONE | Image installs only Claude Code, Codex and OpenCode (`images/scripts/full.sh:56`). pi, Prime Agent, Kimi Code and Mistral Vibe are absent. A prompt to them returns `409 provider_not_installed` (`internal/guest/agents/http.go:115`), not a documented code. FIXED (images wave, v2): pi (`@earendil-works/pi-coding-agent`), Kimi Code (`@moonshot-ai/kimi-code`, bin `kimi`), Mistral Vibe (`uv tool` `mistral-vibe`, bins `vibe`/`vibe-acp`) and Prime Agent (official installer, `/usr/local/bin/prime-agent`, user's kernel venv pre-warmed) are in the full image. | images (`full.sh`, `tools.sh`), guest-agents |
| AGT-002 | DONE | `internal/guest/agentsmgr.go:11`, one process per conversation as `user` (`agents/run.go`) | |
| AGT-003 | DONE | `agents/http.go:57-131` (202 `prompt.queued`, `promptId`, `conversationId`) | |
| AGT-004 | PARTIAL → FIXED server (`internal/server/acct_settings.go:97` account `agentDefaults`, applied by `internal/server/acct_prompt.go:176`); web Agents tab open | Aliases work (`harness.go:49`). The canonical id is `prime-agent`, not Boat's `prime`. No per-account "default harness/model": the guest falls back to env `CROWDVM_DEFAULT_PROVIDER`, then `claude` (`http.go:92`). Nothing sets that value from an account setting. | server (account setting → guest env), web (Agents tab), guest-agents |
| AGT-005 | PARTIAL → FIXED (`internal/server/acct_catalog.go:122` admin PUT/DELETE `/admin/provider-models`; Windows serves embedded snapshot `providers_windows.go`) | `GET /provider-models` exists (`server/providers.go:7`), but the catalog is hard-coded in `guest/agents/catalog.go` and cannot be edited by an admin. A Windows-built server returns `[]` (`providers_windows.go:5`). | server (catalog file/DB + admin edit) |
| AGT-006 | FIXED: server (account fast default, `internal/server/acct_prompt.go:176`) + FIXED guest-agents (`fast:true` without a model is checked against the harness default model, `agents/http.go` handlePrompt) | `fast_mode_not_supported` is checked only when `model` is passed (`http.go:104`). With `fast:true` and no model, the harness's default model is not checked. No account "fast default". | guest-agents, server |
| AGT-007 | PARTIAL → FIXED (`internal/server/acct_prompt.go:57`, `details.provider/setupUrl`) | No pre-flight check. `provider_not_configured` is raised only by the AI proxy at request time (`feat_ai_proxy.go:148`). Without the proxy, a missing key fails inside the harness turn. `aiDecorateProviders` (`feat_ai.go`, end) already computes `configured`, but `/prompt` does not use it. | server (`passthrough.go` prompt intercept) |
| AGT-008 | DONE | per-conversation queue `agents/http.go:157` (`drain`) | |
| AGT-009 | DONE | `agents/http.go:458` (RAM-scaled; both env overrides) | |
| AGT-010 | DONE | FIXED (CLI wave): Per-shell: `CROWDVM_SHELL_ID` from the `crowdvm shell-init` function (installer adds it); state in `Profile.Shells` (`cli/app.go` shell/current, `cmd_agent.go` myConversation). | |
| AGT-011 | DONE | `agents/http.go:230` | |
| AGT-012 | DONE | `agents/http.go:219` | |
| AGT-013 | PARTIAL → is_streaming FIXED guest-agents (throttled cumulative partials for Claude/Codex/pi/ACP, memory only, `agents/run.go` partial; events.jsonl compaction `agents/store.go` compactLocked); coordinator persistence = server; stopped-sandbox reads → FIXED server (prompt.event stored; /events /conversations /prompts/{id} served when not running: internal/server/feat_history.go:239,540-690) | Cursor, ETag/304, type and conversation filters are done in the guest (`http.go:262-300`). Events live only on the guest disk, so `GET /events` on a stopped sandbox returns `409 machine_not_running` (`passthrough.go:87`). The mapping requires coordinator persistence. The relayed `prompt.event` is only used for activity (`lifecycle.go:546`). | server (store `prompt.event`; serve `/events`, `/conversations`, `/prompts/{id}` when not running) |
| AGT-014 | DONE | on-disk store `agents/store.go` | |
| AGT-015 | DONE | transcript replay `agents/run.go:209` | |
| AGT-016 | DONE → FIXED guest-agents: Codex via `codex app-server` `turn/steer` (`agents/rpc.go` codexDriver), pi/Prime via RPC `steer` (`agents/pi.go`), Kimi/Vibe fallback via ACP `session/cancel` + resume (`agents/acp.go`); `native-continued` (accepted, unacted → next turn on the same session) and `late` produced (`agents/run.go` steer/execPrompt). Verified with real codex/pi/kimi/vibe/claude/opencode (`TestE2ESteer`) | Codex is `fallback` and pi/Prime are `next-turn` (`catalog.go:50-70`). Boat steers all three natively. The `native-continued` and `late` modes are never produced. | guest-agents |
| AGT-017 | DONE | `agents/http.go:384` | |
| AGT-018 | DONE | `passthrough.go:135` `setAgentState` → activity/auto-stop | |
| AGT-019 | PARTIAL → limits FIXED (CLI wave + FIXED guest-agents 12/25 MB/64 MB `agents/preflight.go` checkAttachments); images inline to Codex (localImage), pi/ACP (base64), Claude/OpenCode (paths) | The CLI uploads to `~/attachments` (`cli/cmd_agent.go:79`). Images go to Codex `-i` and Claude. The attachment limits are not enforced (BILL-015). | cli, guest-agents |
| AGT-020 | DONE → FIXED guest-agents (prompts stay `queued` while `Options.SetupBlocked` = guest `repoSetupBlocking`, `agents/http.go` waitSetup; 202 returned at once) | The server sends `Repo.SetupBlocking` (`feat_env_guest.go:90`, `proto.go:85`), but the guest never reads it, so prompts are not held during blocking setup. | guest-agents (`http.go` handlePrompt waits on `setup` state) |
| AGT-021 | PARTIAL → FIXED (`internal/server/acct_prompt.go:97`: prompts wait up to 10 min while starting) | The server waits up to `StartWait` (60 s), then answers `409 sandbox_starting` (`passthrough.go:72-85`). It does not queue the prompt until the sandbox is ready. | server |
| AGT-022 | DONE → FIXED guest-agents: `computer` registered for all 7 harnesses, rewritten at every agent start and merged into `~/.claude.json`, `~/.codex/config.toml` (marked block), `~/.pi/agent/mcp.json`, `~/.config/opencode/opencode.json`, `~/.prime/agent/settings.json`, `~/.kimi-code/mcp.json`; Vibe gets it in ACP `session/new` (`agents/computer.go` registerComputer). Uses cua-driver (`mcp --socket /run/ascii-cua/driver.sock`) when installed, else the built-in `crowdvm-guest mcp-computer` server (xdotool/ffmpeg/wmctrl/xclip; verified in a full-image VM, incl. OpenCode calling it). Desktop started before each turn (`Options.EnsureDesktop`) | cua-driver is not in the image (`full.sh`, `tools.sh`). Registration happens only if the binary exists, only for claude/codex/opencode, and via CLI flags (`harness.go:63,101,239,342`). There are no per-harness config files for all 7 harnesses, no `cua-driver.service`, and no `/opt/ascii/cua-driver` or `/run/ascii-cua/driver.sock`. **images part FIXED (images wave, v2):** cua-driver binary at `/opt/crowdvm/cua-driver/cua-driver` and `/opt/ascii/cua-driver/cua-driver`. The user unit `cua-driver.service` (enabled globally, linger for `user`) serves on `/run/ascii-cua/driver.sock`, and `cua-driver mcp --socket /run/ascii-cua/driver.sock` was verified (MCP initialize + get_screen_size). Per-harness config files are guest-agents' part. | images, guest-agents |
| AGT-023 | DONE | Depends on AGT-022. FIXED (images wave, v2): cua-driver 0.44 serves the documented tool set (get_window_state, get_desktop_state, click, type_text, browser_*, clipboard_*, …) on the Xvnc desktop. | images |
| AGT-024 | DONE → FIXED guest-agents (`tools.computer_browser_prepare=false` in `opencode.json` and per run, `agents/computer.go`, `agents/harness.go` opencodeEnv) | `computer_browser_prepare:false` is injected only when the driver exists, and via env, not `opencode.json` (`harness.go:342-344`). | guest-agents |
| AGT-025 | PARTIAL → note FIXED guest-agents (Claude/pi/Prime `--append-system-prompt`, Codex `developerInstructions`, OpenCode/Kimi/Vibe first prompt of a session); skill files = images | A platform note goes only to Claude (`--append-system-prompt`, `harness.go:57,97`). No `crowdvm` skill ships in any harness skills dir. | images (skill files), guest-agents |
| AGT-026 | DONE | harness cwd/home `/home/user` (`agentsmgr.go:13`) | |
| AGT-027 | DONE → FIXED guest-agents (`--disallowedTools ListMcpResourcesTool,ReadMcpResourceTool`, `agents/harness.go` claudeCommand) | Native MCP config works. Claude Code MCP resource tools are not turned off. | guest-agents |
| AGT-028 | DONE | native harness behaviour, binaries on PATH | |
| AGT-029 | DONE | no locks; harnesses runnable directly | |
| AGT-030 | DONE | FIXED (CLI wave): CLI: `ai profile sign-in PROFILE CREDENTIAL [TOKEN]` → `PUT …/sign-ins/{cred}` (`cli/cmd_ai.go`, `client.SetCredentialSignIn`). Dashboard part is web. | |
| AGT-031 | MISSING → FIXED (`internal/server/acct_ai_refresh.go:138` renew before start/prompt/proxy; `:265` no refresh tokens into sandboxes) | No token refresh anywhere (`feat_ai_proxy.go`). Refresh tokens are handed to the sandbox (Codex `auth.json`, `KIMI_CODE_REFRESH_TOKEN`; `feat_ai.go:1414-1470`). | server |
| AGT-032 | DONE | `feat_ai.go:128-146` (sealed at rest) | |
| AGT-033 | DONE | `feat_ai.go:1408-1452` | |
| AGT-034 | FIXED: server (`internal/server/feat_ai.go:1495`) + FIXED guest-agents (Claude DeepSeek route ANTHROPIC_BASE_URL/AUTH_TOKEN, Codex `model_providers.deepseek` responses + web search off, `agents/harness.go` claudeEnv/codexCommand) | DeepSeek is only exported as `DEEPSEEK_API_KEY`. Claude Code is not pointed at `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`. Codex has no `model_providers.deepseek` block (only the generic `OPENAI_BASE_URL` provider, `harness.go:234`). | server (`aiPlan`), guest-agents |
| AGT-035 | FIXED: server default Sonnet 4.6 + FIXED guest-agents (Claude on Bedrock maps catalog ids to opus/sonnet/haiku aliases, keeps server ANTHROPIC_MODEL; Codex `amazon-bedrock` (Mantle) provider with `openai.` model ids when no OpenAI credential; "model access not granted" message, `agents/preflight.go` friendlyError) | Env is set (`feat_ai.go:1437-1446`). No restricted Bedrock model list, no default Sonnet, no "model access not granted" error mapping. Codex uses Mantle only when no OpenAI credential exists. | guest-agents, server |
| AGT-036 | PARTIAL → model prefixes FIXED guest-agents (`vendor:model` → `vendor/model` incl. `opencode-go:`, `agents/harness.go` opencodeModel); sign-in preference = server | The proxy prefers sign-in over key for Mistral and xAI (`feat_ai.go:157`). OpenCode Go/pay-as-you-go model-id prefixes are not handled. | guest-agents (`opencodeModel`) |
| AGT-037 | DONE | FIXED (CLI wave): CLI: `ai profile set-key`/`sign-in` print the catalog `getUrl` before asking (`cmd_ai.go` showGetURL). | |
| AGT-038 | DONE | `new --no-env -e` → sandbox env reaches harness (`agentsmgr.go:15`) | |
| AGT-039 | DONE | FIXED (web wave): AI page: agent defaults card (harness/model/fast via `/account-settings`), sign-in connect/replace, account switch/forget, get-key links from the catalog, "environment does not pass AI credentials" warning (`web/js/pages/ai.js`). | |
| AGT-040 | DONE | events, attachments, desktop and snapshots are independent of the harness | |

## AI credentials & proxy

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| AIC-001 | DONE | `feat_ai.go:476` (secrets never returned) | |
| AIC-002 | DONE | `feat_ai.go:229,1347` | |
| AIC-003 | DONE | `feat_ai.go:541` | |
| AIC-004 | DONE | `feat_ai.go:605,654` (`is_default`, `in_use`) | |
| AIC-005 | DONE | `feat_ai.go:128` | |
| AIC-006 | DONE | `feat_ai.go:725-790` (`not_an_api_key`) | |
| AIC-007 | DONE | `feat_env_guest.go:526` `envPushProfile` (called from every key change) | |
| AIC-008 | DONE | `feat_ai.go:849-955` | |
| AIC-009 | PARTIAL → FIXED (`internal/server/acct_ai_refresh.go:138`, renew only while in use) | No renewal at all (see AGT-031). | server |
| AIC-010 | DONE | `feat_ai.go:277` `aiCanManage` | |
| AIC-011 | PARTIAL → FIXED (a /login inside the sandbox — credentials.json with a refresh token — is never overwritten or removed, and new shells unset CLAUDE_CODE_OAUTH_TOKEN/ANTHROPIC_BASE_URL/ANTHROPIC_AUTH_TOKEN and the proxy placeholder key: internal/guest/creds.go:193; real-VM E2E e2e_leftovers_test.go) | Env mapping is done. An in-sandbox `/login` is not detected, and the vars are not suppressed from the next shell. | guest (boot env writer), guest-agents |
| AIC-012 | DONE | `feat_ai.go:967-1050` | |
| AIC-013 | DONE | `feat_ai.go:979` (`CROWDVM_AI_CUSTOM_API_ALLOW_PRIVATE=1`) | |
| AIC-014 | DONE | `feat_ai.go:1381-1407`, `feat_env_guest.go:153` (rotate per start) | |
| AIC-015 | DONE | `feat_ai_proxy.go:81-160` | |
| AIC-016 | MISSING → FIXED (`internal/server/feat_ai_proxy.go` uses `aiActiveFresh`; Claude/ChatGPT built in, Kimi/Grok via `CROWDVM_AI_REFRESH_URL_*`) | The proxy never renews Claude, ChatGPT or Grok sign-ins. | server |
| AIC-017 | DONE | `feat_ai.go:157-167,1407` (Bedrock and Kimi withheld) | |
| AIC-018 | DONE | `feat_ai.go:1112,1393` | |
| AIC-019 | DONE | `feat_ai.go:1081` (credits) | |
| AIC-020 | DONE | `feat_env.go` (`ai_proxy_off`) | |
| AIC-021 | DONE | `feat_ai.go:1264,1319` | |
| AIC-022 | DONE | `feat_ai.go:98-118` | |
| AIC-023 | DONE | `feat_ai.go:1143` | |
| AIC-024 | DONE | FIXED (web wave): sign-in removal uses `DELETE …/keys/{cred}`; per-sandbox "AI usage…" dialog with block/allow (`web/js/sbx.js` aiUsageDialog). | |

## Accounts, sign-in, agent auth

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| AUTH-001 | DONE | `auth.go:364,416` | |
| AUTH-003 | DONE | `auth.go:521,544` (server; the CLI flag is missing, see CLI-013) | |
| AUTH-005 | DONE | FIXED (web wave): Account page lists connections with Make primary / Remove, email and notice address (`pages/account.js`). | |
| AUTH-006 | DONE → FIXED (`internal/server/acct_settings.go:407` `POST /admin/users/{id}/merge`) | FIXED (CLI wave): Server `POST /admin/users/{id}/merge` + CLI `crowdvm admin merge USER --into TARGET`. | |
| AUTH-007 | DONE | `device.go:78-196` | |
| AUTH-008 | DONE | `auth.go:639-652` | |
| AUTH-009 | MISSING → FIXED server (`useCase` in `/account-settings`, applied to the default env `internal/server/acct_misc.go:325`); web/installer open | Web wave: dashboard asks it once on Sandboxes (`PATCH /account-settings {useCase}`, `pages/sandboxes.js`). The onboarding question "own work vs platform for third parties" is not asked anywhere (CLI `onboard`, installer, dashboard), and no account field drives a safe default env. | server, cli, web |
| AUTH-010 | DONE | `device.go:240` | |
| AUTH-011 | PARTIAL → FIXED (`internal/server/acct_misc.go:61`) | Served only at the root and `/api/v1/.well-known/oauth-protected-resource` (`device.go:202-204`). Not under `/api/boat/…` or `/api/crowdvm/…`. | server |
| AUTH-012 | PARTIAL → FIXED (already enforced by `checkAction` claim_required; verified by `acct_test.go` TestAnonymousAgentLimited) | The anonymous identity gets a full `account.read` key (`device.go:302`), so it can read `/orgs`, `/api-keys`, `/ci`, `/ai-usage` and more. Boat allows only `/me` and `/limits`. | server |
| AUTH-013 | DONE | `device.go:330` + `/device` page | |
| AUTH-014 | DONE | `device.go:350,390` | |
| AUTH-015 | DONE | `device.go:317,366` (only device_code; assertions refused) | |
| AUTH-016 | DONE | `/auth/device/approve` is `ActionSession` (`device.go:26-37`) | |
| AUTH-017 | DONE | `router.go` (`resource_metadata`) | |
| AUTH-018 | MISSING → FIXED (`internal/server/acct_billing.go:732`) | No `POST /api/boat/billing/checkout` (or crowdvm alias) answering "no payment required". | server |
| AUTH-020 | PARTIAL | FIXED (web wave): connections, close account and delivery history are in the dashboard. Still missing: per-sandbox metrics (DASH-004, server endpoint). Session-only rules exist (`feat_retention.go:56-59`). The dashboard lacks connections, close account, delivery history and per-sandbox metrics. | web |
| AUTH-021 | DONE | `auth.go:273,435` (suspended), `feat_retention.go:265` (closed) | |
| AUTH-022 | DONE → FIXED (`internal/server/acct_settings.go:263`, org + admin service accounts) | FIXED (CLI wave): Server service accounts + CLI `crowdvm org service-accounts [create|remove]`, `crowdvm admin service-account NAME`. | |
| AUTH-023 | DONE | `account.go:116-125` (server; CLI/web gaps under CLI-130 and DASH-016) | |

## API keys & scopes

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| KEY-001 | DONE | `api/api.go:122` | |
| KEY-002 | PARTIAL → FIXED (`internal/server/acct_guard.go:60`) | `api_key_org_mismatch` exists (`orgs.go`). `403 api_key_org_forbidden` is never raised, so org keys can reach account settings. | server (`auth.go` checkAction) |
| KEY-003 | DONE | `apikeys.go:397` | |
| KEY-004 | DONE | `apikeys.go:39-40,237` | |
| KEY-005 | DONE | `apikeys.go:397-470` | |
| KEY-006 | DONE | `apikeys.go:232` | |
| KEY-007 | PARTIAL → FIXED (`internal/server/acct_misc.go:307`, `PATCH /admin/settings`) | `scopedCreationEnabled` is hard-coded `true` (`apikeys.go:297`). No admin toggle and no typed 503. | server |
| KEY-008 | DONE | `apikeys.go:354-390` | |
| KEY-009 | DONE | `apikeys.go:527` (works after revoke; `agents` always 0) | |
| KEY-010 | DONE | `apikeys.go:508` | |
| KEY-011 | DONE | `apikeys.go:497` | |
| KEY-012 | DONE | `apikeys.go:486` `ownKey` + `canManageKeys` | |
| KEY-013 | DONE | `apikeys.go:15-20` | |
| KEY-014 | DONE | `apikeys.go:27-32` | |
| KEY-015 | DONE | `auth.go:280` checkAction + `AllowsSandbox` | |
| KEY-016 | DONE | `auth.go`, `apikeys.go` | |
| KEY-017 | PARTIAL → FIXED (per-sandbox key + server URL in ~/.config/crowdvm/config.json profile default, removed when withheld, user's own login kept, legacy ~/.config/ascii/boat link: internal/guest/creds.go:96) | The per-sandbox key is minted (`apikeys.go:182`) and exported as `CROWDVM_API_KEY`. It is not written to `~/.config/crowdvm/config.json`, and there is no legacy `~/.config/ascii/boat` symlink. | guest (boot), server |
| KEY-018 | DONE | `feat_env_guest.go:171` | |
| KEY-020 | PARTIAL → FIXED (`internal/server/acct_misc.go:350`) | `pages/keys.js` has presets, actions, sandbox selector, expiry, revoke and usage. The environment selector is always empty because `keyCatalog` returns `environments: []` (`apikeys.go:313`). | server |

## Organizations / teams

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| ORG-001 | DONE | `orgs.go:247` | |
| ORG-002 | DONE | `orgs.go:534-561` (+ SMTP, `crowdvm org invite/join`, `/invite` page) | |
| ORG-003 | DONE | `orgs.go:471,319` | |
| ORG-004 | DONE | `orgs.go:285` | |
| ORG-005 | DONE | `orgs.go:380,428`, `web/js/pages/org.js:54-58` | |
| ORG-006 | DONE | `orgs.go:182` | |
| ORG-007 | DONE | `orgs.go:145,198` | |
| ORG-008 | DONE | `orgs.go:64` | |
| ORG-009 | DONE | `orgs.go:92-140` | |
| ORG-010 | DONE | sandbox JSON `createdBy/visibility/access` (`sandboxes.go`) | |
| ORG-011 | DONE | `passthrough.go:102` | |
| ORG-012 | PARTIAL → FIXED (`internal/server/acct_sshkeys.go:87`) | Unshare only flips visibility (`lifecycle.go:1300-1314`). Teammates' SSH keys are not removed from `authorized_keys`. | server + guest (remove keys) |
| ORG-013 | DONE | `lifecycle.go` (`restartRequired`) | |
| ORG-014 | DONE | `orgs.go:285` | |
| ORG-015 | DONE | `feat_env.go` (`environment_passes_logins`) | |
| ORG-016 | DONE | `sandboxes.go`, `lifecycle.go` | |
| ORG-017 | DONE | `feat_env.go` | |
| ORG-018 | DONE | `orgs.go:330,455` | |
| ORG-019 | PARTIAL → FIXED (`internal/server/acct_billing.go` wallet policies `/admin/policies/{wallet}`) | No per-org limits or credit quota set by an admin. Limits are global config (`Cfg.MaxActive`, `StartsPer*`). | server |
| ORG-020 | PARTIAL → FIXED (per-wallet limits: `ratelimit.go` `startWindowsFor`/`maxActiveFor`) | Counting per org wallet works (`ratelimit.go:130,179`). The limit values are global, not per-org. | server |
| ORG-021 | PARTIAL → FIXED (`internal/server/acct_billing.go:446`, caps always enforced) | Caps are stored and enforced at start (`credits.go:63-72`, `ratelimit.go:193`). Over-cap running sandboxes are never stopped. | server (billing loop) |
| ORG-022 | DONE → FIXED (`internal/server/acct_misc.go:194`, + `/api/boat/billing/move-to-org`) | FIXED (CLI wave): Server `POST /orgs/{org}/move-personal` + CLI `crowdvm org migrate [--transfer-credits N|all]`. | |
| ORG-024 | PARTIAL | FIXED (web wave): per-member caps UI (`pages/org.js`). Still missing: a "Viewing" all-org scope. Org page, wallet switch and visibility chips exist. No per-member caps UI and no "Viewing" all-org scope. | web (`pages/org.js`) |
| ORG-025 | DONE | internal API = `/api/v1` + `/auth` | |

## Billing & limits (→ credits)

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| BILL-001 | DONE | `credits.go:98` | |
| BILL-002 | PARTIAL → FIXED (`internal/server/acct_billing.go:311` monthly quota) | No per-account/org credit quota. Only one-off grants exist. | server |
| BILL-003 | DONE | ledger allows negative (`credits.go:18`) | |
| BILL-005 | DONE | `account.go:222`, `credits.go:249` | |
| BILL-006 | PARTIAL → FIXED (`internal/server/acct_billing.go:397` 24 h grace, notice + mail, early stop) | `enforceCredits` stops as soon as the balance is ≤ 0 (`credits.go:151`). No 24 h grace, no notice/SMTP, no early-stop thresholds. | server |
| BILL-007 | DONE | `credits.go:53` | |
| BILL-008 | DONE | `ratelimit.go:186` | |
| BILL-009 | DONE | `ratelimit.go:150-170` | |
| BILL-010 | MISSING → FIXED (`internal/server/acct_billing.go:212`, `PUT /billing/extra-starts`) | No extra starts at credit cost and no `extra-starts` toggle. | server, cli |
| BILL-011 | PARTIAL → FIXED (`internal/server/acct_billing.go:884` planNudge) | Shape is mostly present (`account.go:20-101`). `planNudge` is absent. | server |
| BILL-013 | DONE | `ratelimit.go:33-70` | |
| BILL-014 | DONE | `scheduler.go` (`out_of_capacity`) | |
| BILL-015 | DONE | FIXED (CLI wave): CLI enforces 12 files / 25 MB each / 64 MB total for `prompt --attach` (guest side is guest-agents). | |
| BILL-016 | DONE | `feat_webhooks.go` (`limit_reached`) | |
| BILL-017 | DONE | `sandboxes.go` (`secondsPerDollar`) | |
| BILL-018 | DONE | `sandboxes.go` (`deletedCost`) | |
| BILL-019 | DONE | FIXED (web wave): Cost column on Sandboxes (`include=cost`), "This month by sandbox" with the deleted-sandboxes line on Credits (`pages/sandboxes.js`, `pages/credits.js`). | |
| BILL-022 | MISSING → FIXED server (`internal/server/acct_billing.go:756`); web open | No per-account restriction policy (max TTL, allowed sizes, saved-snapshot allowance, concurrency). Admin user PATCH only has admin/disabled/password/name (`account.go:184`). | server, cli, web |
| BILL-023 | MISSING → FIXED server (`internal/server/acct_billing.go:376`, `GET /notices`); web open | No low-credit alert. | server, web |
| BILL-025 | DONE | egress not metered | |
| BILL-026 | MISSING → FIXED (`internal/server/acct_billing.go:155`) | No per-account credit cap (only org member caps). | server |

## Webhooks

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| HOOK-001 | DONE | `feat_webhooks.go:110-118` | |
| HOOK-002 | DONE | `feat_webhooks.go:575`, `cli/cmd_admin.go:158` | |
| HOOK-003 | DONE | `feat_webhooks.go` (`whsec_`) | |
| HOOK-004 | DONE | `feat_webhooks.go:551,382` | |
| HOOK-005 | DONE | `feat_webhooks.go` (`limit_reached`) | |
| HOOK-006 | DONE | `feat_webhooks.go:40` | |
| HOOK-007 | DONE | `feat_webhooks.go:109` (EventTx outbox) | |
| HOOK-008 | DONE | `feat_webhooks.go:196` | |
| HOOK-009 | DONE | `feat_webhooks.go:396-412` | |
| HOOK-010 | DONE | `feat_webhooks.go:43-52` | |
| HOOK-011 | DONE | `feat_webhooks.go:742` | |
| HOOK-012 | DONE | FIXED (web wave): Deliveries log per webhook (status, attempts, next retry, HTTP status, payload viewer) and "Send test event" (`pages/webhooks.js`). | |
| HOOK-013 | DONE | hydrated on snapshot stored | |

## OpenTelemetry

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| OTEL-001 | DONE | `feat_otel.go:70-72` | |
| OTEL-002 | DONE | `feat_otel.go:674-690` | |
| OTEL-003 | DONE | `feat_otel.go:582` | |
| OTEL-004 | DONE | LAN/http accepted | |
| OTEL-005 | DONE | `feat_otel.go:449-569` | |
| OTEL-006 | DONE | `feat_otel.go:130-134` | |
| OTEL-007 | DONE | `feat_otel.go:169` | |
| OTEL-008 | DONE | `feat_otel.go:258-300` | |
| OTEL-009 | DONE | `feat_otel.go:463` | |
| OTEL-010 | DONE | `feat_otel.go:9` | |
| OTEL-011 | DONE | FIXED (web wave): Telemetry page: OTLP endpoint + write-only headers, save-and-test result, status/last export/last error, disconnect, what is exported (`web/js/pages/telemetry.js`). | |

## Data retention & deletion

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| DATA-001 | DONE | `feat_retention.go:12,60-61` | |
| DATA-002 | DONE | `lifecycle.go:1323` | |
| DATA-003 | DONE | `snapshots.go:280` | |
| DATA-004 | DONE | `lifecycle.go:1340-1372` + `gcSnapshots` | |
| DATA-005 | DONE | `snapshots.go:388,411,610` | |
| DATA-006 | DONE | immediate erase (allowed: "may be shorter") | |
| DATA-007 | PARTIAL → FIXED (`internal/server/acct_guard.go:60`, every DELETE) | `no-store` is set only on GET deletion-op, retention and webhooks. It is missing on `DELETE /sandboxes/{id}`, `DELETE /snapshots/{id}`, `DELETE /sandboxes/{id}/snapshots` and `DELETE /saved-snapshots/{name}` (`lifecycle.go:1335`, `snapshots.go:388-430`). | server |
| DATA-008 | DONE | `feat_retention.go:104-176` | |
| DATA-009 | MISSING → FIXED (guest/history.go:354 ZDR ignores the stop flush; ZDR also implies no monitoring data) → FIXED server (`GuestConfig.ZeroDataRetention`, `internal/server/acct_guestcfg.go:21`); guest core honours it (tmpfs-only history) | The guest has no ZDR mode, so process output is always stored (`guest/history.go`). | server → `GuestConfig`, guest |
| DATA-010 | DONE | `feat_retention.go:228`, `snapshots_disabled` | |
| DATA-011 | PARTIAL → FIXED guest side (guest/guest.go:156 NoMonitoring: nothing kept on disk, 409 monitoring_off) → FIXED server (`NoMonitoring` also for account monitoring-off/ZDR, `internal/server/acct_guestcfg.go:21`); guest core honours it (tmpfs-only history) | The server hides history and processes (`feat_retention.go:256`). The guest is never told: `GuestConfig` has no monitoring flag, so command history and output are still written to disk. | proto/server/guest |
| DATA-012 | DONE | `feat_retention.go:301-356` (API; UI missing, DASH-011) | |
| DATA-013 | DONE | `feat_retention.go:359` | |
| DATA-014 | PARTIAL → FIXED (`internal/server/acct_audit.go:74`, `GET /audit-events`) | The ledger and `sandbox_runs` (node history) are kept. No audit-event log. | server |
| DATA-015 | MISSING → guest `disk.usage` events FIXED (guest/sampler.go:146); server alerts pending | No disk-full (85/95 %) alerts and no lost-sandbox cause from samples. | server, guest |
| DATA-016 | DONE | FIXED (web wave): Data & privacy card: ZDR (typed confirmation), snapshots off, monitoring off (`pages/account.js`). | |

## CI

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| CI-001 | PARTIAL → PARTIAL kept: Gitea `act_runner` not built (no Gitea API to verify against here; see report) | The GitHub polling controller is done (`feat_ci.go:345-480`). Gitea Actions / `act_runner` is not supported. | server |
| CI-002 | DONE | `feat_ci.go:805` | |
| CI-003 | DONE | `feat_ci.go:304-345` | |
| CI-004 | DONE | JIT runner in VM, `feat_ci.go:669` | |
| CI-005 | PARTIAL → FIXED (internal/server/feat_ci.go:789 OOM in the runner cgroup → job error + "Out of memory on CrowdVM" commit status) | 4 GB swap is done (`feat_ci.go:669-671`). No "Out of memory" annotation on OOM. | server (runner script + GitHub API) |
| CI-006 | DONE | job sandboxes are noEnv | |
| CI-007 | DONE | `ci-<repo>-<job id>` | |
| CI-008 | DONE | API refusal reasons (`feat_ci.go:1164`); CLI and dashboard missing (CI-012/015) | |
| CI-009 | PARTIAL → FIXED (internal/server/feat_ci.go:804 paused/refused jobs post an `error` commit status "Paused by plan limit: …"; check runs need a GitHub App) | Retry/pause logic is done (`feat_ci.go:531-614`). Nothing is posted to GitHub (no check run or annotation "Paused by plan limit"). | server |
| CI-010 | DONE | `feat_ci.go:50` | |
| CI-011 | DONE | `feat_ci.go:940` | |
| CI-012 | DONE | FIXED (CLI wave): `crowdvm ci [--account --repo --state --limit --all]`, `ci repos`, `ci job`, `ci logs [--follow]` (`cli/cmd_ci.go`). | |
| CI-013 | DONE | `feat_ci.go:1038,1046,1068` | |
| CI-014 | DONE | `feat_ci.go:117-121` | |
| CI-015 | DONE | FIXED (web wave): see DASH-010. | |
| CI-016 | DONE | `login --key-stdin`, `run`, `/api/crowdvm/cli/download` | |
| CI-018 | DONE | credits only when enforced | |

## Evals / RL / Harbor / Eve / AgentEnv adapters

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| EVAL-001 | DONE | composition of saved snapshots, `--from`, fork, delete, Idempotency-Key | |
| EVAL-002 | DONE | **Fixed.** `delete_sandbox(..., x_ascii_confirm_delete=…)` works (alias `confirm_delete=` kept); `sdk/python/tests/test_compat.py`, live smoke `sdk/smoke/smoke.py`; episode script run as `integrations/evals/run_episodes_example.py` | |
| EVAL-003 | DONE | label filter `sandboxes.go:508`, noEnv/snapshots/ttl on create (not run against real AgentEnv; `integrations/agentenv/check_provider.py` replays the provider's calls against a live server: OK) | |
| EVAL-004 | DONE | Box alias layer `feat_legacy.go` (`POST /api/box/v1/boxes` with a `cvm_` key checked live; not run against harbor-box itself). `integrations/harbor` is a maintained `/api/v1` adapter (live-tested with a Harbor stand-in, `hello_sandbox.py`) | |
| EVAL-005 | DONE | same layer. `integrations/eve` (`crowdvmBox`, `asciiBox` alias): 6 unit tests + live `hello-sandbox.mjs` (not run inside real Eve / eve-box) | |
| EVAL-006 | DONE | recipe via saved snapshot + exec | |
| EVAL-007 | DONE | no network-policy API exposed | |

## Windows apps (Wine)

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| WIN-001 | PARTIAL | Wine on the desktop works only as far as the desktop does. The "agents drive via computer tools" part is missing (AGT-022). **images part FIXED (images wave, v2):** computer tools available via cua-driver (AGT-022). | images |
| WIN-002 | DONE | The image preinstalls Ubuntu `wine wine64` (`full.sh:40`). Boat ships none, and these likely conflict with `winehq-staging` (`/usr/bin/wine`) in `install-wine.sh`. Untested. FIXED (images wave, v2): no distro Wine in the image. The ondrej PHP PPA is present (so the libgd3 pin is meaningful). Boat's `install-wine.sh` runs unchanged (`CROWDVM_E2E_WINE=1` test). | images (drop distro wine) |
| WIN-003 | DONE | detached commands / `ssh -- bash -s` | |
| WIN-004 | DONE | `xdotool`, `wmctrl` in image (`full.sh:37`) | |
| WIN-005 | PARTIAL → FIXED (CROWDVM_E2E_WINE_APPS=1: 7-Zip, Notepad++, WinMerge, x64dbg, Process Explorer, Autoruns, Everything, SkiFree downloaded, started under Wine, main window asserted: internal/node/image/e2e_test.go:274; apps without a stable download listed there) | No acceptance test for the tested-app list. **images part (images wave, v2):** `CROWDVM_E2E_WINE=1` acceptance test: install-wine.sh unchanged, then 64-bit and 32-bit (syswow64) programs. The per-app list (Notepad++ …) is not automated. | images/tests |
| WIN-006 | DONE | generic snapshot/fork | |
| WIN-007 | DONE | X11 `DISPLAY=:0` | |
| WIN-009 | DONE | Linux guests only | |

## SDKs

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| SDK-001 | PARTIAL | Upstream `boat-sdk` / `@boatdev/sdk` cannot be installed here (no network/pip). Substitute: both CrowdVM SDKs accept Boat's call shapes (`BoatApi`/`Configuration`/`ApiClient`, `create_sandbox_request=`, `CommandRequest`, wrapped `forkRequest`) and `sdk/smoke/smoke.py` + `smoke.mjs` pass against a live server. | sdk/tests (run the real packages once they can be installed) |
| SDK-002 | DONE | `sdk/python`, `sdk/typescript` (122 ops; tests 39/39, 33/33) | |
| SDK-003 | DONE | **Fixed.** `x_ascii_confirm_delete` / `xAsciiConfirmDelete` (alias `confirm_delete` kept), `x_boat_org` / `xBoatOrg` on every op that has the header plus `create`/`fork`, `Configuration`/`ApiClient`/`BoatApi`, wrapped body forms, `placement`/`nested`, `browser()` (123 ops). `sdk/tools/gen.py`, `sdk/python/src/crowdvm/compat.py`, tests 43/43 and 34/34 | |
| SDK-004 | DONE | `helpers.py`, `helpers.ts` | |
| SDK-005 | DONE | `helpers.ts` (`signal`) | |
| SDK-006 | DONE | `var_from`, keyword-only, `actual_instance` | |
| SDK-007 | DONE | `aio.py` | |
| SDK-008 | DONE | **Fixed** with SDK-003: Boat recipes using `x_ascii_confirm_delete=` run. Live smoke scripts `sdk/smoke/` | |
| SDK-009 | MISSING → FIXED (`internal/server/acct_openapi.go:72`) | The coordinator serves no OpenAPI document (`/openapi/crowdvm-v1.yaml`, `/api/v1/openapi.yaml`). | server (embed generated spec) |

## CLI

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| CLI-001 | DONE | `cli/app.go:301,385` | |
| CLI-002 | DONE | `app.go:302`, `client/client.go:369` | |
| CLI-003 | DONE | `app.go:290` | |
| CLI-004 | DONE | `app.go:305` (no-op; there is no updater) | |
| CLI-005 | DONE | `cmd_sandbox.go`, `cmd_agent.go` | |
| CLI-006 | DONE | `app.go:137-150` | |
| CLI-007 | DONE | FIXED (CLI wave): `crowdvm shell-init bash|zsh|fish|powershell` function sets `CROWDVM_SHELL_ID`; current/conversations per shell, profile fallback for scripts; installers add it. | |
| CLI-008 | DONE | `app.go:426-433` | |
| CLI-009 | DONE | FIXED (CLI wave): `ValidArgsFunction` on every ID command: live sandbox ids + `current`/`self`, 15 s cache in `<config>/completion-cache.json` (`cli/complete.go`). | |
| CLI-010 | DONE | `feat_install.go:44-45` | |
| CLI-011 | DONE | FIXED (CLI wave): `onboard`: browser or `--email` sign-in, third-party question once (`--for own|others`; `PATCH /account-settings useCase`, safe default env), credit balance. | |
| CLI-013 | DONE | FIXED (CLI wave): `login/onboard --email [ADDR]` (+ `--code` for agents) → `/auth/email/start|verify` (`cmd_auth.go` emailLogin). | |
| CLI-015 | DONE | `cmd_auth.go:81-104` | |
| CLI-016 | DONE | `cmd_auth.go:123-180` | |
| CLI-017 | DONE | pending-link reuse `cmd_auth.go:130-160` | |
| CLI-018 | DONE | `--new` → `login_replaced` | |
| CLI-019 | DONE | `cmd_auth.go:31-48` | |
| CLI-020 | DONE | device login is the default | |
| CLI-021 | DONE | `cmd_auth.go:191` | |
| CLI-022 | DONE | FIXED (CLI wave): `status` shows credits and enforcement. | |
| CLI-023 | DONE → FIXED server (`internal/server/acct_misc.go:130`); CLI = cli agent | FIXED (CLI wave): `crowdvm feedback MSG|stdin` → `POST /feedback {text,sandboxId}`; `crowdvm admin feedback`; clear `feedback_unavailable` on older servers. | |
| CLI-024 | DONE | FIXED (CLI wave): `crowdvm self-update [--check]` downloads `/downloads/{os}/{arch}/crowdvm`, replaces the binary when the hash differs (Windows: rename-aside). | |
| CLI-025 | MISSING → FIXED (ADAPTED, `internal/cli/update.go`: after a command, at most every 6 h, on a terminal only, a notice when `GET /api/v1/server` reports a newer release; never installs by itself; `--no-update`/`CROWDVM_NO_UPDATE`/`BOAT_NO_UPDATE`/`CI` turn it off. `self-update` runs the download with `--version` and refuses older builds without `--force`. Makefile/dist.sh now stamp `server/node/guest.Version`, which were always "dev". True auto-install needs a version manifest on `/downloads`) | No auto-update. | cli |
| CLI-026 | DONE | `config.go:11,54` | |
| CLI-027 | DONE | `cmd_admin.go:28-52` | |
| CLI-028 | DONE | `cmd_admin.go:55-87` | |
| CLI-029 | DONE | `cmd_admin.go:90-106` | |
| CLI-030 | DONE | `cmd_admin.go:109-125` | |
| CLI-031 | DONE | `cmd_admin.go:145-230` | |
| CLI-032 | DONE | `cmd_admin.go:236-290` | |
| CLI-033 | DONE | `cmd_sandbox.go:42` | |
| CLI-034 | DONE | `cmd_sandbox.go:26` | |
| CLI-035 | DONE | `cmd_sandbox.go:32` | |
| CLI-036 | DONE | `cmd_sandbox.go:33` | |
| CLI-037 | DONE | `cmd_sandbox.go:28` | |
| CLI-038 | DONE | `cmd_sandbox.go:27` | |
| CLI-039 | DONE | `cmd_sandbox.go:29` | |
| CLI-040 | DONE | `cmd_sandbox.go:38` | |
| CLI-041 | DONE | `cmd_sandbox.go:39` | |
| CLI-042 | DONE | `cmd_sandbox.go:30` | |
| CLI-043 | DONE | `cmd_sandbox.go:145` | |
| CLI-044 | DONE | `cmd_sandbox.go:146` | |
| CLI-045 | DONE | `cmd_sandbox.go:147` | |
| CLI-046 | DONE | `cmd_sandbox.go:148` | |
| CLI-047 | DONE | `cmd_sandbox.go:149` | |
| CLI-048 | DONE | `cmd_sandbox.go:150` | |
| CLI-049 | DONE | `cmd_sandbox.go:151` | |
| CLI-050 | DONE | `cmd_sandbox.go:152` | |
| CLI-051 | DONE | `cmd_sandbox.go:209-259` | |
| CLI-052 | DONE | `cmd_sandbox.go:366` | |
| CLI-053 | DONE | `cmd_sandbox.go:385-426` | |
| CLI-054 | DONE | `cmd_sandbox.go:279` | |
| CLI-055 | DONE | `cmd_sandbox.go:430` | |
| CLI-056 | DONE | `cmd_sandbox.go:493` | |
| CLI-057 | DONE | `cmd_sandbox.go:492` | |
| CLI-058 | DONE | `cmd_sandbox.go:648-684` | |
| CLI-059 | DONE | `cmd_sandbox.go:713` | |
| CLI-060 | DONE | `cmd_sandbox.go:512-551` | |
| CLI-061 | DONE | `cmd_sandbox.go:554-555` (`sandboxFlags.bind`) | |
| CLI-062 | DONE | `cmd_sandbox.go:606` | |
| CLI-063 | DONE | `cmd_sandbox.go:559-602` | |
| CLI-064 | DONE | `cmd_agent.go:240-281` | |
| CLI-065 | DONE | FIXED (CLI wave): "this shell" marker uses the per-shell conversation (see CLI-007). | |
| CLI-066 | DONE | `cmd_agent.go:338` | |
| CLI-067 | DONE | `cmd_agent.go:368` | |
| CLI-068 | DONE | FIXED (CLI wave): Key `~/.ssh/crowdvm_ed25519`; an existing `<config>/id_ed25519` (or explicit `CROWDVM_CONFIG_DIR`) keeps being used. `crowdvm ssh-config ID` writes `~/.ssh/crowdvm/<id>.conf`. | |
| CLI-069 | DONE | `cmd_exec.go:23` | |
| CLI-070 | DONE | `cmd_exec.go:94` | |
| CLI-071 | DONE | `cmd_exec.go:95,140` | |
| CLI-072 | DONE | `cmd_exec.go:96` | |
| CLI-073 | DONE | `cmd_exec.go:97-100` | |
| CLI-074 | DONE | server wait (`passthrough.go:72`) | |
| CLI-075 | DONE | `cmd_exec.go:349-458` | |
| CLI-076 | DONE | `cmd_exec.go:449` (`SetInterspersed(false)`) | |
| CLI-077 | DONE | `cmd_exec.go:482` | |
| CLI-078 | DONE | `cmd_exec.go:540-609` | |
| CLI-079 | DONE | `cmd_exec.go:620` | |
| CLI-080 | DONE | `cmd_env.go:29-213` | |
| CLI-081 | DONE | `cmd_env.go:271-319` | |
| CLI-082 | DONE | `cmd_env.go:323-352` + global `--org` | |
| CLI-083 | DONE | `cmd_env.go:146-160` | |
| CLI-084 | DONE | `cmd_env.go:167-186` | |
| CLI-085 | DONE | `cmd_env.go:193-201` | |
| CLI-086 | DONE | `cmd_env.go:213` | |
| CLI-087 | DONE | `cmd_access.go:368-401` | |
| CLI-088 | DONE | `cmd_access.go:179-244` | |
| CLI-089 | DONE | `cmd_access.go:283-330` | |
| CLI-090 | DONE | `cmd_access.go:300-313` | |
| CLI-091 | DONE | FIXED (CLI wave): `forward --reverse` reconnects after drops (gives up after 5 quick failures) (`cmd_access.go` reverseForward). | |
| CLI-092 | DONE | `cmd_access.go:405-443` | |
| CLI-093 | DONE | FIXED (CLI wave): `crowdvm browser ID [--profile PATH] [--no-open]` + `client.Browser`. | |
| CLI-094 | DONE | `cmd_snapshot.go:14-62` | |
| CLI-095 | DONE | `cmd_snapshot.go:66-150` | |
| CLI-096 | DONE | `cmd_snapshot.go:158-205` | |
| CLI-097 | DONE | `cmd_snapshot.go:212-330` | |
| CLI-098 | DONE | `cmd_snapshot.go:352-397` | |
| CLI-099 | DONE | `cmd_agent.go:35` | |
| CLI-100 | DONE | `cmd_agent.go:122-125` | |
| CLI-101 | DONE | `cmd_agent.go:126-127` | |
| CLI-102 | DONE | `cmd_agent.go:128-129` | |
| CLI-103 | DONE | FIXED (CLI wave): `prompt --help` appends the live `/provider-models` catalog with efforts and a "Fast mode (--fast):" list. | |
| CLI-104 | DONE | `cmd_ai.go:52-88` | |
| CLI-105 | DONE | `cmd_ai.go:93` | |
| CLI-106 | DONE | `cmd_ai.go:125-220` | |
| CLI-107 | DONE | `cmd_ai.go:222-258` | |
| CLI-108 | DONE | `cmd_ai.go:264-290` | |
| CLI-109 | DONE | `cmd_auth.go:296-350` | |
| CLI-110 | DONE | `cmd_auth.go:354-385` | |
| CLI-111 | DONE → FIXED server (`internal/server/acct_misc.go:93` login links); CLI = cli agent | FIXED (CLI wave): `dashboard` opens a one-time 15-min link from `POST /auth/login-links` (falls back to `server/`). | |
| CLI-112 | DONE | `cmd_admin.go:296-460` | |
| CLI-113 | DONE | `cmd_admin.go:345-416` | |
| CLI-114 | DONE → FIXED server (move-personal); CLI = cli agent | FIXED (CLI wave): `crowdvm org migrate` → `POST /orgs/{org}/move-personal`. | |
| CLI-115 | DONE | `cmd_admin.go:362-405` | |
| CLI-116 | DONE | `cmd_admin.go:466` | |
| CLI-117 | DONE | `cmd_sandbox.go:613` | |
| CLI-118 | DONE | FIXED (CLI wave): `billing`, `billing status`, `billing --all|--details` (GET `/billing`, falls back to `/credits`). | |
| CLI-119 | DONE → FIXED server (`GET /billing` policy; tiers admin-set); CLI = cli agent | FIXED (CLI wave): `billing plan` shows the wallet policy (admin-set limits; no tiers). Admins: `crowdvm admin policy WALLET --set k=v`. | |
| CLI-120 | DONE → FIXED server (`PUT /billing/extra-starts`); CLI = cli agent | FIXED (CLI wave): `billing extra-starts on|off [--monthly-cap N]` → `PUT /billing/extra-starts`. | |
| CLI-122 | DONE | `wallet` alias (`cmd_auth.go:392`) | |
| CLI-123 | DONE | `cmd_admin.go:504-533` | |
| CLI-124 | DONE | FIXED (CLI wave): `crowdvm ci` group (`cli/cmd_ci.go`). | |
| CLI-125 | DONE | FIXED (CLI wave): `ci connect --token … [--account --yes]`, `ci connect --link ID --yes` (confirm), `ci unlink`. | |
| CLI-126 | DONE | FIXED (CLI wave): `ci public ACCOUNT [on|off] [--users …|--anyone]`. | |
| CLI-127 | DONE | `cmd_exec.go:140`, `app.go:117-170` | |
| CLI-128 | DONE | `cmd_sandbox.go:495` | |
| CLI-129 | DONE | `transfer.go:364` | |
| CLI-130 | DONE | FIXED (CLI wave): `crowdvm admin users [create]|suspend|unsuspend|set-admin|reset-password|merge|service-account|credits grant|policy|feedback|invite-codes [create|revoke]|sandboxes|nodes`; `nodes drain|undrain|update|remove [--force]|join-tokens [revoke]`. | |

## API

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| API-001 | DONE | `account.go:16`, `auth.go:639` | |
| API-002 | DONE | `orgs.go:157` | |
| API-003 | DONE | `orgs.go:159` | |
| API-004 | DONE | `orgs.go:158` | |
| API-005 | DONE | `feat_retention.go:55` | |
| API-006 | DONE | `feat_retention.go:56` | |
| API-007 | DONE | `routes.go:18` | |
| API-008 | PARTIAL → FIXED (planNudge) | `planNudge` missing (BILL-011). | server |
| API-009 | DONE | `feat_env_repos.go:27` | |
| API-010 | DONE | `feat_env_repos.go:28` | |
| API-011 | DONE | `apikeys.go:287` | |
| API-012 | DONE | `apikeys.go:288` | |
| API-013 | DONE | `apikeys.go:290` | |
| API-014 | DONE | `apikeys.go:291` | |
| API-015 | DONE | `apikeys.go:292` | |
| API-016 | DONE | `feat_webhooks.go:110` | |
| API-017 | DONE | `feat_webhooks.go:111` | |
| API-018 | DONE | `feat_webhooks.go:112` | |
| API-019 | DONE | `feat_webhooks.go:113` | |
| API-020 | DONE | `feat_webhooks.go:114` | |
| API-021 | DONE | `feat_webhooks.go:115` | |
| API-022 | DONE | `feat_otel.go:70` | |
| API-023 | DONE | `feat_otel.go:71` | |
| API-024 | DONE | `feat_otel.go:72` | |
| API-025 | DONE | `feat_ci.go:117` | |
| API-026 | DONE | `feat_ci.go:118` | |
| API-027 | DONE | `feat_ci.go:119` | |
| API-028 | DONE | `feat_ci.go:120` | |
| API-029 | DONE | `feat_ci.go:121` | |
| API-030 | DONE | `feat_ai.go:99` | |
| API-031 | DONE | `feat_ai.go:100` | |
| API-032 | DONE | `feat_ai.go:101` | |
| API-033 | DONE | `feat_ai.go:102` | |
| API-034 | DONE | `feat_ai.go:103` | |
| API-035 | DONE | `feat_ai.go:104` | |
| API-036 | DONE | `feat_ai.go:106` | |
| API-037 | DONE | `feat_ai.go:107` | |
| API-038 | DONE | `feat_ai.go:108` | |
| API-039 | DONE | `feat_ai.go:109` | |
| API-040 | DONE | `feat_ai.go:110` | |
| API-041 | DONE | `feat_ai.go:111` | |
| API-042 | DONE | `feat_ai.go:112` | |
| API-043 | DONE | `feat_ai.go:113` | |
| API-044 | DONE | `feat_ai.go:114` | |
| API-045 | DONE | `feat_ai.go:115` | |
| API-046 | DONE | `feat_ai.go:116` | |
| API-047 | DONE | `feat_env.go:102` | |
| API-048 | DONE | `feat_env.go:103` | |
| API-049 | DONE | `feat_env.go:90` | |
| API-050 | DONE | `feat_env.go:91` | |
| API-051 | DONE | `feat_env.go:93` | |
| API-052 | DONE | `feat_env.go:94` | |
| API-053 | DONE | `feat_env.go:95` | |
| API-054 | DONE | `feat_env.go:96` | |
| API-055 | DONE | `feat_env.go:97` | |
| API-056 | DONE | `feat_env.go:98` | |
| API-057 | DONE | `feat_env.go:99` | |
| API-058 | DONE | `feat_env.go:100` | |
| API-059 | DONE | `feat_env.go:101` | |
| API-060 | DONE | `routes.go:21` | |
| API-061 | DONE | `routes.go:22`, `sandboxes.go:815` | |
| API-062 | DONE | `routes.go:23` | |
| API-063 | DONE | `routes.go:24` | |
| API-064 | DONE | `lifecycle.go:1323` | |
| API-065 | DONE | `routes.go:26` | |
| API-066 | DONE | `routes.go:31` | |
| API-067 | DONE | `routes.go:32` | |
| API-068 | DONE | `routes.go:30` | |
| API-069 | DONE | `routes.go:28` | |
| API-070 | DONE | `routes.go:29`, `lifecycle.go:1112` | |
| API-071 | DONE | passthrough → `agents/http.go:57` | |
| API-072 | DONE | passthrough → `agents/http.go:262` (running only, AGT-013) | |
| API-073 | DONE | passthrough → `agents/http.go:230` | |
| API-074 | DONE | passthrough → `agents/http.go:219` | |
| API-075 | DONE | `routes.go:27` | |
| API-076 | DONE | `guest/internal.go:31` | |
| API-077 | DONE | `guest/internal.go:32` | |
| API-078 | DONE | `guest/internal.go:33` | |
| API-079 | DONE | `guest/internal.go:34` | |
| API-080 | DONE | `guest/internal.go:35` | |
| API-081 | DONE | `guest/internal.go:36` | |
| API-082 | DONE | `guest/internal.go:37` | |
| API-083 | DONE | `guest/internal.go:38` | |
| API-084 | DONE | `guest/internal.go:39` | |
| API-085 | DONE | `guest/internal.go:40` | |
| API-086 | DONE | `guest/internal.go:41` | |
| API-087 | DONE | `passthrough.go:63` | |
| API-088 | DONE | `passthrough.go:64` | |
| API-089 | DONE | `guest/internal.go:23` | |
| API-090 | DONE | `guest/internal.go:24` | |
| API-091 | DONE | `guest/internal.go:25` | |
| API-092 | DONE | `guest/internal.go:26` | |
| API-093 | DONE | `guest/internal.go:27` | |
| API-094 | DONE | `guest/internal.go:28` | |
| API-095 | DONE | `guest/internal.go:29` | |
| API-096 | DONE | `guest/internal.go:30` | |
| API-097 | DONE | `guest/internal.go:44` | |
| API-098 | DONE | `agents/http.go:308` | |
| API-099 | DONE | `agents/http.go:384` | |
| API-100 | DONE | `feat_desktop.go:49` | |
| API-101 | DONE | `feat_host.go:111` | |
| API-102 | DONE | `feat_host_ssh.go` | |
| API-103 | DONE | `routes.go:49` | |
| API-104 | DONE | `routes.go:50` | |
| API-105 | DONE | `routes.go:51` | |
| API-106 | DONE | `routes.go:52` | |
| API-107 | DONE | `routes.go:41` | |
| API-108 | DONE | `routes.go:42` | |
| API-109 | DONE | `routes.go:43` | |
| API-110 | DONE | `routes.go:44` | |
| API-111 | DONE | `routes.go:45` | |
| API-112 | DONE | `routes.go:46` | |
| API-113 | DONE | `routes.go:47` | |
| API-114 | DONE | `routes.go:36` | |
| API-115 | DONE | `routes.go:37` | |
| API-116 | DONE | `routes.go:38`, `snapshots.go:411` | |
| API-117 | DONE | `routes.go:39` | |
| API-118 | DONE | `routes.go:33` | |
| API-119 | DONE | `routes.go:40` | |
| API-120 | DONE | `feat_snapfiles.go:54` | |
| API-121 | DONE | `feat_snapfiles.go:55` | |
| API-122 | DONE | `feat_snapfiles.go:56` | |
| API-123 | DONE | `feat_webhooks.go:396` | |
| API-124 | DONE | `internal/api/api.go` envelope | |
| API-125 | PARTIAL → FIXED (`internal/server/acct_prompt.go:97`) | `details.setupUrl` is never set (0 occurrences). | server |
| API-126 | PARTIAL → FIXED (internal/server/passthrough.go:133 `sandbox_restoring` message on node offline / rebuild / recover) | `sandbox_direct_failed` + `retryable` is done (`passthrough.go:117,145`). The `sandbox_restoring` message is never produced. | server, node |
| API-127 | PARTIAL → PARTIAL→ `sandbox_not_promptable` raised (`internal/server/acct_prompt.go:97`); others = lifecycle/node → FIXED where meaningful (internal/server/feat_host_ssh.go:59 `ssh_host_key_unavailable`, internal/server/lifecycle.go `resume_failed`); `org_suspended`/`account_not_ready` have no CrowdVM state to report | Never raised: `ssh_host_key_unavailable`, `sandbox_not_promptable`, `resume_failed`, `org_suspended`, `account_not_ready`. | server |
| API-128 | DONE | `ParsePage` | |
| API-129 | DONE | `account.go:365-427` | |
| API-130 | DONE | `ratelimit.go:63` | |
| API-131 | PARTIAL → FIXED (AGT-005 + in OpenAPI) | Route works, but the catalog is hard-coded and not in any served OpenAPI (AGT-005, SDK-009). | server |
| API-132 | DONE | `feat_desktop.go:50` | |
| API-133 | MISSING → FIXED (`internal/server/acct_openapi.go:72`) | No OpenAPI served. | server |
| API-134 | MISSING → FIXED (`web/js/pages/explorer.js`, `/api-explorer`: renders `/api/v1/openapi.json`, filter, scope chips, "Try it" with the session incl. NDJSON streams, copy as curl) | No API explorer (optional). | web |
| API-135 | DONE | `feat_install.go:44`, `/api/v1/credits` | |
| API-136 | PARTIAL → FIXED (AUTH-011/012) | See AUTH-011 (no `/api/crowdvm/.well-known`) and AUTH-012 (anonymous scope too wide). | server |
| API-137 | PARTIAL → FIXED (`internal/server/feat_webhooks.go:178` box.* events, `internal/server/auth.go:242` box_/boat_ keys) | The rename shim is done (`feat_legacy.go`). `box.*` webhook event names and acceptance of the `box_` key prefix are missing. | server |
| API-139 | DONE | `orgs.go:92` | |
| API-140 | PARTIAL → FIXED (DATA-007) | See DATA-007. | server |

## Install scripts

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| INST-001 | DONE | FIXED (CLI wave): `crowdvm config get|set|unset|list|path` exists; `install.sh` sets the server (warns on failure). | |
| INST-002 | DONE | FIXED (CLI wave): Same in `install.ps1`. | |
| INST-003 | DONE | FIXED (CLI wave): Installers run `crowdvm onboard` when a terminal is available (sign-in + third-party question + credits); `--no-login`/`CROWDVM_NO_LOGIN`. | |
| INST-004 | DONE | FIXED (CLI wave): Installers add the `shell-init` function to bashrc/zshrc/fish conf.d/PowerShell profile (`--no-shell`/`CROWDVM_NO_SHELL`). | |
| INST-005 | DONE | installs into `~/.local/bin` (`install.sh:8`) | |
| INST-006 | DONE | darwin CLI builds served (`feat_install.go:32-33`; cross-compile OK) | |
| INST-007 | DONE | `feat_install.go:112-136` | |
| INST-008 | DONE | `onboard` idempotent (`cmd_auth.go:91-98`) | |
| INST-010 | DONE | `cmd/crowdvm-node` join/bench/config/install-service (`main.go:131-498`) | |
| INST-011 | DONE | `cmd/crowdvm-server/main.go:104-121` | |

## Dashboard

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| DASH-001 | PARTIAL → FIXED server (login links); web open | Local sign-in works (`pages/auth.js`). No one-time 15-min login link from `crowdvm dashboard`. | server, cli, web |
| DASH-002 | DONE | FIXED (web wave): Cost column and size filter (`pages/sandboxes.js`). | |
| DASH-003 | DONE | FIXED (web wave): row menu: Browser (`POST /browser`), Copy SSH command, AI usage (`sbx.js` sandboxMenu). | |
| DASH-004 | MISSING → FIXED (server `feat_history.go` `GET /sandboxes/{id}/metrics`; web `web/js/tabs/metrics.js`: Metrics tab, CPU/memory/disk/I/O pressure, 1 h to 7 d, gaps while stopped, hover + keyboard, table view) → FIXED server (internal/server/feat_history.go:776 `GET /sandboxes/{id}/metrics?from=&to=&step=`, shape in web/README.md); graphs = web | Web wave: still blocked on the server. Samples (CPU/RAM/disk, 30 s, 7 d) are stored by `feat_history.go` but no endpoint returns a series (only the last sample via `/processes` when stopped). Needs e.g. `GET /sandboxes/{id}/metrics?since=` → `[{at,cpuUtilization,memoryUsedBytes,diskUsedBytes}]`. No metrics graphs and no metrics endpoint. | server, web |
| DASH-005 | DONE | FIXED (web wave): snapshot file browser with file/folder/all downloads (`/snapshots/{id}/files`), disk-layer links (`/download`), snapshot delete, select-all/bulk delete of saved snapshots, allowance line (`tabs/snapshots.js`, `pages/snapshots.js`). | |
| DASH-006 | DONE | FIXED (web wave): org access panel, Git hosts card (`/repos/sources`), registry logins, credential-profile picker, CrowdVM-credentials/proxy toggles (`pages/environment.js`, `pages/environments.js`). | |
| DASH-007 | DONE | FIXED (web wave): see AGT-039 / AIC-024. | |
| DASH-008 | DONE | FIXED (web wave): key dialog also falls back to `GET /environments` when the catalog list is empty (`pages/keys.js`). | |
| DASH-009 | DONE | FIXED (web wave): see HOOK-012 and OTEL-011; the signature help now matches the server (`X-CrowdVM-Signature: v1=`, delivery/timestamp headers, 5 s, 8 attempts). | |
| DASH-010 | DONE | FIXED (web wave): CI runners page: setup checklist, connect with a token (+ `?confirm=` links via `/dashboard?tab=ci`), stats + 7-day chart, jobs with filters and live log, problems, linked accounts with repo status, public-repo users, unlink (`pages/ci.js`). | |
| DASH-011 | DONE | FIXED (web wave): connections (list, make primary, remove, email, notice address), Data & privacy, close/reopen/erase with a closed-account state (`pages/account.js`). | |
| DASH-012 | DONE | FIXED (web wave): member caps dialog (`concurrentCap`, `usageCapCredits`) and Caps column (`pages/org.js`). | |
| DASH-013 | DONE | FIXED (web wave): see BILL-019. | |
| DASH-014 | DONE | FIXED (web wave): notice banner above every page from `GET /notices` with dismiss/dismiss-all, plus closed-account and low-credit notices (`main.js` drawNotices). | |
| DASH-015 | DONE | `device.go:207` → `/device` page | |
| DASH-016 | DONE | FIXED (web wave): node menu: drain/resume, rename, labels, remove (force when busy); Admin page with users (create, admin, suspend, password, credit grants, wallet limits), invite codes, retention (`pages/nodes.js`, `pages/admin.js`). | |
| DASH-017 | DONE | `internal/node/ui.go`, `ui.html` | |

## CrowdVM substrate rows (node app, benchmark, trust, Windows nodes)

| ID | Status | Missing piece (precise) | Layer/files to change |
|---|---|---|---|
| SSH-015 | DONE | `cli/cmd_access.go:248`, `passthrough.go:66` | |
| SEC-011 | PARTIAL | Node resource caps are enforced. The network part (`vm/netstack`) is under active change and was not audited. The process driver offers no isolation (dev only). | node |
| SEC-012 | DONE | per-VM bearer token, guest port on node loopback (ARCHITECTURE; `guest/guest.go`) | |
| SEC-014 | PARTIAL → FIXED server (internal/server/scheduler.go:102 `placement.trust` own|trusted, admin node trust) | `placement.node`, labels, tier and categories exist (`scheduler.go:16-24`). No node trust levels and no "only my nodes" placement option. | server, cli, web |
| SIZE-014 | DONE | `placement.nested` + node nested detection; WHPX nodes report none | |
| SIZE-015 | DONE | `internal/node/bench/score.go`, nodes API | |
| SIZE-020 | DONE | `internal/node/bench`, `node/config.go:170`, heartbeat | |
| (Windows node) | PARTIAL | Cross-compiles (`GOOS=windows` OK), WHPX accel (`qemu/sys_windows.go:8`). The service is a logon-time scheduled task only (`node/service/service_windows.go`): it does not run without a logged-in user. Never run on Windows. → FIXED (node): `install-service` registers a real SCM service (automatic start, LocalSystem, restart on failure, `<data>\node.log`), and `--logon-task` keeps the old task (node/service/service_windows.go:30,116). Cross-compiled only, not run on Windows | node |

---

## Client/server mismatches

Every client path was matched against the server's registered routes (method + path, wildcards normalised).

**CLI client (`internal/client`)**: all ~130 request paths match a server route. Org scope goes out as `X-CrowdVM-Org` (`client.go:369`), which the server accepts (`orgs.go:92`). No wrong paths. Coverage gaps (server routes with no client method): `/provider-models`, `/auth/email/start|verify`, `/webhooks/{id}/deliveries`, `/webhooks/{id}/test`, `PUT /credential-profiles/{p}/sign-ins/{cred}`, `/admin/*`, `PATCH|DELETE /nodes/{id}`, `GET /join-tokens`, `/repos/sources*`, `/ci/links/{id}`, `/account/close|reopen|erase`, `POST /sandboxes/{id}/browser`.

**Install scripts → CLI**: `install.sh:33` and `install.ps1:27` run `crowdvm config set server <url>`. No `config` command exists, so the call fails silently and the CLI never learns the server URL (INST-001/002).

**Python SDK (`_ops.py`) and TypeScript SDK (`operations.ts`)**: identical 122-op tables. Every op matches a server route. Boat's `POST /sandboxes/{id}/commands` is hand-written (`client.py:247`, `client.ts:298`). Mismatches were in naming against the Boat SDKs, not in paths (**both fixed**: `x_ascii_confirm_delete`/`xAsciiConfirmDelete` and `x_boat_org`/`xBoatOrg` are accepted; `confirm_delete` stays as an alias):
- `delete_sandbox`, `delete_snapshot`, `delete_sandbox_snapshots` take `confirm_delete=` and send `X-CrowdVM-Confirm-Delete`. Boat code passing `x_ascii_confirm_delete=` gets `TypeError` (`client.py:228`).
- `create`/`fork` have no `x_boat_org` header param (only `org`/`team_id` in the body).

**Dashboard (`web/js`)**:
1. `pages/ai.js:96`: "Remove" on a sign-in calls `DELETE /api/v1/credential-profiles/{p}/accounts/{cred}`. **No such route.** The server only has `DELETE …/keys/{cred}` and `DELETE …/accounts/{cred}/{account}`. The router answers with the generic JSON 404/405. Fix: call `…/keys/{cred}`. **FIXED (web wave)**.
2. `pages/keys.js:71`: the environment selector reads `catalog.scopeResources.environments`, which the server always returns as `[]` (`apikeys.go:313`). **FIXED (web wave)**: falls back to `GET /environments`.
3. All other `api.*` calls (sandboxes, files, console, host, desktop, snapshots, environments, webhooks, keys, org, nodes, credits, invites) match registered routes.

**Server-side oddity** → FIXED (`internal/server/router.go:75` drops stubs covered by a feature): the `notYet` stubs in `routes.go:56-66` without a method (e.g. `/api/v1/webhooks`, `/api/v1/ci/{rest...}`) stay registered next to the features' method-specific routes. Unsupported methods or unknown sub-paths therefore answer `501 not_implemented` instead of `405`/`404`. Harmless but misleading. Drop `notYet` for features that are now registered.

---

## Prioritized fix list (by layer)

Priority: P1 = broken or blocks a REQUIRED flow, P2 = missing REQUIRED feature, P3 = adapted/polish.

### server
1. P1: `/prompt` pre-flight `409 provider_not_configured` using `aiHarnessCreds`/`aiActive` (AGT-007).
2. P1: Restrict anonymous agent identities to `/me` and `/limits` (AUTH-012).
3. P1: Raise `403 api_key_org_forbidden` for org keys on account settings (KEY-002).
4. P2: Persist prompt events on the coordinator and serve `/events`, `/conversations`, `/prompts/{id}` while stopped (AGT-013).
5. P2: Subscription token refresh for Claude, ChatGPT and Grok in the proxy and before start. Stop shipping refresh tokens into sandboxes (AGT-031, AIC-009/016).
6. P2: DeepSeek mapping for Claude Code (`ANTHROPIC_BASE_URL`/`AUTH_TOKEN`) and Codex `model_providers.deepseek`. Bedrock model list and errors (AGT-034/035).
7. P2: Serve an OpenAPI document at `/openapi/crowdvm-v1.yaml` and `/api/v1/openapi.yaml`, including `/provider-models` (SDK-009, API-133). Make the provider catalog admin-editable and present on Windows builds (AGT-005).
8. P2: Credits: 24 h grace + notice before stopping. Per-account/org quotas and limits. Restriction policy. Stop over-cap org members' sandboxes. Extra starts. Per-account cap. Low-credit alert (BILL-002/006/010/022/023/026, ORG-019/020/021).
9. P2: Move personal sandboxes and credits into an org (ORG-022).
10. P2: Unshare removes teammates' SSH keys (ORG-012).
11. P2: `GuestConfig` flags for monitoring-off and ZDR (DATA-009/011). Audit-event log (DATA-014). Disk-full/lost-cause alerts (DATA-015).
12. P2: Service accounts (AUTH-022). Connections add/remove/primary (AUTH-005). Admin account merge (AUTH-006). Onboarding third-party flag (AUTH-009).
13. P2: Missing error codes `ssh_host_key_unavailable`, `sandbox_not_promptable`, `resume_failed`, `org_suspended`, `account_not_ready`, `sandbox_restoring`. `details.setupUrl` (API-125/126/127).
14. P3: `Cache-Control: no-store` on all DELETE deletion responses (DATA-007).
15. P3: Well-known docs under `/api/crowdvm/…` and `/api/boat/…` (AUTH-011). Checkout stub (AUTH-018).
16. P3: `scopedCreationEnabled` admin toggle + 503 (KEY-007). `scopeResources.environments` populated (KEY-020).
17. P3: CI: Gitea Actions, OOM annotation, GitHub check-run "Paused by plan limit" (CI-001/005/009).
18. P3: `planNudge` (BILL-011). Box `box.*` webhook names and `box_` key prefix (API-137). One-time dashboard login link endpoint (CLI-111, DASH-001). Feedback store (CLI-023). Remove stale `notYet` stubs.
19. P3: Node trust levels / "only my nodes" placement (SEC-014).

### node
1. P2: Windows node runs only as a logon task. Offer a real service, or document it, and test on Windows. → FIXED (node/service/service_windows.go).
2. P3: Produce a `sandbox_restoring` signal while the chain is being pulled (API-126).

### guest
1. P2: Write the per-sandbox key to `~/.config/crowdvm/config.json` + `~/.config/ascii/boat/config.json` symlink (KEY-017).
2. P2: Honour monitoring-off / ZDR (no history or output stored) (DATA-009/011).
3. P3: Detect in-sandbox `/login` and suppress `CLAUDE_CODE_OAUTH_TOKEN`/`ANTHROPIC_*` in new shells (AIC-011).

### guest-agents
(guest-agents wave: all items below FIXED; see the rows. Remaining: the `crowdvm` skill files (images), coordinator persistence of events (server). The built-in computer MCP covers a subset of cua-driver's tools; with cua-driver installed (images v2) the full set is used.)
1. P1: Hold prompts while a `SetupBlocking` repo setup runs (AGT-020).
2. P2: Native steering for Codex, pi and Prime; `native-continued`/`late` modes (AGT-016).
3. P2: Computer-use registration for all 7 harnesses via their config files; OpenCode `browser_prepare` off in `opencode.json`; Claude MCP resource tools off (AGT-022/024/027).
4. P2: Attachment limits 12 files / 25 MB / 64 MB (BILL-015). `fast` validated against the default model (AGT-006).
5. P3: Platform note for every harness, not just Claude (AGT-025). OpenCode model prefixes (AGT-036).

### cli
(CLI wave: all FIXED except CLI-025 auto-update, deliberately not built: `/downloads` has no version metadata, so an automatic hash-based update could downgrade a newer local build.)
1. P1: Add `crowdvm config set server` (or change the install scripts) so installs point at the right server (INST-001/002).
2. P2: `ci`, `ci repos|job|logs [--follow]`, `ci connect`, `ci public` (CLI-124/125/126, CI-012).
3. P2: `login/onboard --email` (CLI-013). Onboarding third-party question (CLI-011).
4. P2: `prompt --help` from `/provider-models` (CLI-103). `browser` command (CLI-093).
5. P2: Shell function + per-shell `current` and conversation state (CLI-007/065, AGT-010). Live-id completions with a 15 s cache (CLI-009).
6. P2: `admin` group: users, suspend, credit grants, invite codes, node drain/evict (CLI-130).
7. P3: `self-update` + auto-update (CLI-024/025). `feedback` (CLI-023). `org migrate`, `billing plan|extra-starts|status` (CLI-114/118/119/120). `status` shows credits (CLI-022). Print `getUrl` before asking for a key (AGT-037). `forward --reverse` reconnect (CLI-091). Key path `~/.ssh/crowdvm_ed25519` (CLI-068). Sign-in paste command (AGT-030).

### web
1. P1: Fix `pages/ai.js:96` sign-in removal → `DELETE …/keys/{cred}`.
2. P2: Pages for CI (DASH-010), Telemetry/OTLP + webhook Deliveries log (OTEL-011, HOOK-012, DASH-009), and Account Data & privacy + close/reopen/erase + connections (DATA-016, DASH-011).
3. P2: Agents tab: default harness/model/fast, sign-in paste, account switch/add, get-key links, env-blocks-keys warning (AGT-039, AIC-024, DASH-007).
4. P2: Per-sandbox metrics graphs (needs a server endpoint) and alerts (DASH-004/014).
5. P3: Cost column and Type filter (DASH-002, BILL-019). Browser and AI-usage menu items (DASH-003). Snapshot file download + bulk delete (DASH-005). Env access panel + git-host token (DASH-006). Member caps (DASH-012). Node drain/evict + admin users/grants (DASH-016).

### images
(images wave, v2: items 1, 2 and 4 FIXED. cua-driver 0.44 + `cua-driver.service` (user unit, linger) with `/opt/ascii/cua-driver` and `/run/ascii-cua/driver.sock`; pi, kimi, vibe, prime-agent (+ hermes, openclaw, cursor-agent); no distro Wine, so `install-wine.sh` runs unchanged (wine-11.21 staging, 32+64-bit; `CROWDVM_E2E_WINE=1`). Item 3, the `crowdvm` skill, is LEFT for guest-agents/harness config. The per-app tested list (WIN-005) is not automated.)
1. P2: Ship cua-driver + `cua-driver.service`, with `/opt/ascii/cua-driver` and `/run/ascii-cua/driver.sock` symlinks (AGT-022/023, WIN-001).
2. P2: Install pi, Prime Agent, Kimi Code and Mistral Vibe where publicly installable (AGT-001).
3. P2: Ship the `crowdvm` skill into every harness's skills dir (AGT-025).
4. P2: Drop the preinstalled distro `wine wine64` so `install-wine.sh` (winehq-staging) works unchanged (WIN-002). Add a Wine acceptance test (WIN-005).

### sdk
1. ~~P2: Generate Boat-compatible parameter names~~ **done** (SDK-003/008, EVAL-002).
2. P3: Acceptance test that runs the upstream `boat-sdk` / `@boatdev/sdk` against a test server (SDK-001).
