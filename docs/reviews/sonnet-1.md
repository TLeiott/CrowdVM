# Review sonnet-1: does CrowdVM support every supportable Boat feature?

**Verdict: DISAGREE.** The gap is narrow. The core product is close to complete, and what is missing is small. But "every feature that is not impossible under the LAN-only constraint" is a strict bar, and I found supportable features that are missing or wrong. Some of them `docs/PARITY.md` claims as delivered.

Reviewer: Sonnet 5.5, independent, read-only apart from this report. Build: `make build` at the repo state of 2026-11-08 ~20:10.

## Method and its limits

1. **API surface.**
   - I compared all 122 operations in `.ref/boat-v1.yaml` and the 118 `api__reference__*` pages with the live server's `GET /openapi/crowdvm-v1.yaml`. The route sets are identical, apart from the four `named-snapshots` aliases, which CrowdVM also has.
   - The server's OpenAPI is generated from its route table with generic bodies. So it only proves that routes exist, not that they conform.
   - To check conformance, I called every one of the 122 operations once with fake ids. None gave a non-JSON 404 or a 405.
   - I called every parameter-less GET with a real sandbox and checked the top-level response against Boat's `required` fields. Zero required fields were missing across about 35 endpoints. CrowdVM adds a few extra fields.
   - The `Sandbox` object has every Boat property except `cost`, plus extras (`node`, `placement`, `imageId` and others).
2. **CLI.**
   - I extracted every `boat …` command and flag from `.ref/pages/cli-reference.md` (370 command lines). I checked each against `crowdvm … --help`.
   - The only unmatched flags were `--google` and `--agentid` (OAuth, so N/A), the payment commands (N/A), and `--org`, which is global.
   - `billing plan --move-personal` and `wallet pay` are N/A or adapted.
3. **SDKs.**
   - I ran the Python SDK through Boat's `ApiClient`/`Configuration`/`BoatApi` shape against a live server. This covered `command`, `stream_command`, `run_command`, detached commands with `command_status`/`stop_command`, `read_text`/`write_text` and `command_history`. All worked.
   - The TypeScript SDK loaded and called `me()` and the sandbox list correctly.
   - `sdk/METHODS.md` lists 124 methods.
4. **Behaviour on a process-driver server and node** (ports 9300/9301/9390):
   - Create, exec, stop, resume and fork.
   - Command history and `ps` while the sandbox is stopped. These work, with `live:false` for `ps`.
   - Webhook delivery. Signed events arrive with the legacy `X-Ascii-*` and the `X-CrowdVM-*` headers, and the `sandbox.archived` payload shape matches Boat.
   - Org create.
   - `/auth.md` agent self-registration text.
   - Hosted-port creation: `host` returns `hostUrl` and `pathUrl`.
   - Signed upload URL.
   - Error codes: `invalid_timeout`, `invalid_type`, `count_requires_from`, `confirmation_required`, `idempotency_key_reused`, `invalid_path`.
   - IP rate-limit headers.
   - `/install`, `/install.ps1`, `/healthz`.
5. **Not verified.**
   - Anything that needs a real VM: QEMU snapshots, Wine, desktop and browser streaming, nested KVM, firewall inside the image, OOM behaviour and the base-image toolset. The process driver has no isolation and no desktop.
   - Mid-review, `/tmp` (tmpfs, shared with other reviewers' e2e runs) filled to 100%. I stopped testing, killed my processes and deleted my cluster directory. The hosted-port routing check (`<id>-<port>.localhost`, `/p/<id>/<port>/`) therefore did not complete. I verified only that the URLs are created.
   - For all of this I fall back on static findings and on what `docs/gaps/*.md` admits.

## Missing or broken supportable features

| # | Boat page | What is missing | Evidence | Severity |
|---|---|---|---|---|
| 1 | `ai-tools` (docs for agents). PARITY DOCS-001 and DOCS-002 claim these as ADAPTED. | The coordinator serves no `llms.txt`, `llms-full.txt`, per-page `.md` docs or docs MCP server. These are static text and one small JSON-RPC endpoint. They are fully supportable on a LAN, and `docs/guide/*.md` already exists. | `GET /llms.txt`, `/llms-full.txt`, `/docs/index.md` and `/docs/quickstart.md` all return 404. `/docs` and `/mcp` return the SPA HTML shell. `POST /mcp` with `tools/list` returns "method not allowed". `grep llms internal/server` has no hits. `docs/gaps/*.md` never checks DOCS-00x. | Medium. The agent-facing docs channel is half of the `ai-tools` page. |
| 2 | `ai-tools` and `integrated-agents__customize`. PARITY DOCS-003 and AGT-025. | The skill pack is not delivered: no `crowdvm` skill in any harness skills dir, no in-sandbox skill, no `skills/` folder in the repo. | `ls skills` fails. `grep -rli skill internal/guest images/rootfs images/scripts` finds nothing. `docs/gaps/A.md` itself says "LEFT: ENV-017 skel defaults and the `crowdvm` skill". | Low to medium. |
| 3 | `concepts`, `api__v1`, `cli-reference` (scp `bx_<id>:/path`). PARITY principle 1 and LIFE-001. | Sandbox ids are `bx_xxxxxxxxxx`, not Boat's `^bx_[23456789a-z…]{8}$` (`boat-v1.yaml:69,203,682,2900`). PARITY says "id keeps `bx_` shape" and that Boat SDKs, AgentEnv, Harbor and Eve work by changing only base URL and key. Any client or schema validator that applies the pattern, or a script that greps `bx_`, rejects CrowdVM ids. | `crowdvm new` returned `bx_xchzznq95a`. `docs/gaps/B.md` lists LIFE-001 as "PARTIAL, not fixed". | Medium for the wire-compat claim. The fix is small: mint `bx_` and accept both. |
| 4 | `api__v1` error table | Documented 400 codes `prompt_required` and `provider_required` are not emitted. The guest uses `invalid_request` ("prompt is required", `internal/guest/agents/http.go:63`). An empty `POST /prompt {}` is answered `409 provider_not_configured` first. `machine_class_plan_required` and `org_suspended` do not exist as codes. The size and wallet policy equivalents are not documented as replacements. | `curl -X POST …/prompt -d '{}'` returns 409. `grep -rn prompt_required internal/` has no hits. | Low. |
| 5 | `machines__fail-fast`, `limits` | When capacity is short, behaviour is inconsistent. A `new` that cannot be placed because vCPU is full stays `provisioning`; the CLI hung until I interrupted it, and each interrupt left an orphan sandbox. When the node's disk budget is short, the start errors the sandbox with `start_failed: node: HTTP 503 out_of_capacity`. So there is no uniform "wait for a machine" (Boat) or immediate `no_ready_machine` (`failFast`). The disk case was probably caused by `/tmp` filling and the low default `--disk-reserve`, so this finding is plausible rather than confirmed. | Observed on my 4-vCPU process node. I did not retest with a different disk budget. | Low and uncertain. |
| 6 | `ci`. PARITY CI-001. | Only the GitHub poller exists. Gitea `act_runner` or any LAN-only CI source is not built. This is the team's own admitted PARTIAL. GitHub Actions needs outbound access to github.com, so CI is partly outside the LAN-only scope anyway. | `docs/gaps/B.md`: "PARTIAL kept: Gitea act_runner not built". | Low. |

## Items I checked and found good

- `/api/v1` path, field, envelope and header conformance on the sampled endpoints.
- The Boat-style SDK shim (`BoatApi`, `Configuration`, `create_sandbox_request=`).
- Webhook signing and headers.
- Idempotency and delete-confirm headers.
- Command history and `ps` while the sandbox is stopped. `docs/gaps/A.md` row 6 (history returns 409 once stopped) and row 20 (`/processes` returns 409 on a stopped sandbox) are stale.
- Org, service-account, invite and admin tooling.
- Agent self-registration (`/auth.md`, `/agent/identity`).
- Installers at `/install` and `/install.ps1`.
- `/healthz`.
- Dashboard at `/dashboard/`.
- Resume (including from an `error` state), fork, signed file URLs and `host`.

## Disputed classifications

- **DOCS-001 and DOCS-002** (`llms.txt`, docs MCP) are classified ADAPTED, meaning "delivered in a LAN form". They are not delivered. I'd move them to REQUIRED/open or implement them. They are not N/A.
- **DOCS-003** (skills) is likewise marked ADAPTED but is not delivered.
- **LIFE-001** and the "wire compatibility first" principle are overstated while ids are `bx_`.
- **Properly N/A** (agree): Google and AgentID sign-in, Stripe, wallet pay, cards, trials, SOC2, regions, legacy Box migration, release channels, support channels.
- **Borderline.** BILL-010 (extra starts, with the invoice option N/A) and the plan and size gating (SIZE-004, `machine_class_plan_required`) are both adapted to admin policy. The error-code names differ (finding 4).
- **`docs/gaps/*.md` as evidence is stale in places** (rows noted above). I did not use them to clear any row.

## Verdict rationale

I found no pervasive gaps. Routes, schemas, CLI, SDKs, webhooks and the lifecycle are close to full Boat parity, and the N/A set is justified. But findings 1 to 3 are supportable, currently missing, and, for 1 and 2, contradicted by PARITY.md's own classification. The question asks for "every" supportable feature, so the answer is DISAGREE. Fixing findings 1 to 4 would make me lean AGREE for the surface I could test. The VM-dependent features remain untested by me.
