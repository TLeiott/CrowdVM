# API and SDKs

Everything the CLI and the dashboard do goes through the HTTP API at
`http://<server>:8420/api/v1`. It follows Boat's v1 API: same paths, request and
response bodies, envelope and error codes. CrowdVM adds nodes, credits, admin and a
few other routes.

## Find your way around

- **API explorer:** the dashboard page `/api-explorer` lists every route this server
  serves, with the API key scope each one needs. "Send" runs a request as you, with
  your dashboard session; "Copy as curl" gives the same call with an API key.
- **OpenAPI:** `/openapi/crowdvm-v1.yaml` or `/api/v1/openapi.json`. The server
  generates it from its route table, so it always matches the running version.

## Authentication

```sh
curl -H "Authorization: Bearer $CROWDVM_API_KEY" http://192.168.1.10:8420/api/v1/sandboxes
```

- API keys start with `cvm_`. Create them with `crowdvm api-key create` or in the
  dashboard (API keys). Pick a preset (`read-only`, `full-sandbox`, `ci`, `admin`) or a
  list of actions, and optionally limit a key to some sandboxes or environments.
- A CLI login stores a session token (`cvms_…`) that works the same way.
- To act for an organization, add `X-CrowdVM-Org: <org id or name>` (Boat's
  `X-Boat-Org` works too) or `?org=`.

## Requests and responses

Successful responses are JSON objects with `ok: true` and a `type`:

```json
{"ok": true, "type": "sandbox.info", "sandbox": {"id": "bx_qzdsd908dm", "state": "ready", "...": "..."}}
```

Errors carry the HTTP status, a stable `code` and a message:

```json
{"ok": false, "status": 409, "code": "machine_not_running", "message": "…",
 "error": {"code": "machine_not_running", "message": "…", "status": 409, "details": {}}, "requestId": "req_…"}
```

`details.retryable` and `details.retryAfterSeconds` say whether and when to retry.

- `POST /sandboxes` takes an `Idempotency-Key` header, so a retried create does not
  start a second sandbox.
- Deleting a sandbox needs the header `X-CrowdVM-Confirm-Delete: <id>`.
- `POST /sandboxes/{id}/commands` with `{"command": "...", "stream": true}` answers
  with NDJSON frames (`started`, `stdout`, `stderr`, `exit`) as the command runs.
  `{"detached": true}` returns a process id at once.
- Calls into a sandbox wait up to 60 s while it starts, then answer
  `409 sandbox_starting`. A stopped sandbox answers `409 machine_not_running`.
- The server limits each IP to 10,000 requests per minute by default (`429`).

## A minimal session

```sh
API=http://192.168.1.10:8420/api/v1
AUTH="Authorization: Bearer $CROWDVM_API_KEY"
id=$(curl -s -H "$AUTH" -H 'Content-Type: application/json' -H "Idempotency-Key: $(uuidgen)" \
  -d '{"type":"default","ttlSeconds":1800}' $API/sandboxes | jq -r .sandbox.id)
curl -s -H "$AUTH" $API/sandboxes/$id | jq .sandbox.state          # poll until "ready"
curl -s -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"command":"uname -a"}' $API/sandboxes/$id/commands | jq .
curl -s -H "$AUTH" -X POST $API/sandboxes/$id/stop
```

## SDKs

Both SDKs live in this repository, mirror Boat's SDK method names, and read
`CROWDVM_API_BASE` and `CROWDVM_API_KEY` from the environment.

**Python** (`sdk/python`, Python 3.10+, no dependencies):

```python
from crowdvm import CrowdVM, wait_until_ready

cvm = CrowdVM()
sb = cvm.create(ttl_seconds=1800).sandbox
wait_until_ready(cvm, sb.id)
print(cvm.command(sb.id, "uname -a").stdout)
cvm.stop(sb.id)
```

**TypeScript** (`sdk/typescript`, package `@crowdvm/sdk`):

```ts
import { CrowdVM } from '@crowdvm/sdk';

const cvm = new CrowdVM();
```

The READMEs in `sdk/python` and `sdk/typescript` list the helpers (waiting, streaming,
files, agent prompts) and the options.

## Webhooks

`crowdvm webhook create` (or the dashboard) registers a URL for sandbox events. Each
delivery carries `X-CrowdVM-Delivery` (the event id), `X-CrowdVM-Timestamp` (unix
seconds) and `X-CrowdVM-Signature: v1=<hex>`, where the hex is HMAC-SHA256 with the
webhook's secret over `<delivery>.<timestamp>.<raw body>`. Compare in constant time,
and reject old timestamps and repeated delivery ids. The same headers also go out as
`X-Ascii-*` for Boat receivers.

## Integrated agents

`POST /sandboxes/{id}/prompt` sends a prompt to Claude Code, Codex, OpenCode or
another harness inside the sandbox; `GET /sandboxes/{id}/events` reads what it did.
The CLI equivalents are `crowdvm prompt`, `crowdvm events`, `crowdvm steer` and
`crowdvm interrupt`.

The body may carry `credentials`, a map of env vars for that turn's harness process
only. `crowdvm prompt --local-login` fills it with the caller's own login:

```json
{"prompt": "Fix the build", "provider": "codex", "model": "gpt-6-luna",
 "credentials": {"CHATGPT_ACCESS_TOKEN": "eyJ…", "CHATGPT_ACCOUNT_ID": "…"}}
```

| Harness | Accepted credentials |
|---|---|
| Claude Code | `CLAUDE_CODE_OAUTH_TOKEN` (subscription access token) or `ANTHROPIC_API_KEY` |
| Codex | `CHATGPT_ACCESS_TOKEN` + `CHATGPT_ACCOUNT_ID` (ChatGPT access token; Codex then talks to `https://chatgpt.com/backend-api/codex` directly) or `OPENAI_API_KEY` |
| OpenCode, pi | vendor API keys (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, …) |

- Send access tokens only, never a refresh or id token: the sandbox cannot refresh
  them, so the caller stays the only refresher. Check the expiry before sending; the
  CLI refuses a subscription token with less than about 2 minutes left
  (`local_login_expired`).
- A request credential counts for the `provider_not_configured` pre-flight, and wins
  for its turn over the sandbox's own credentials and AI-proxy route (a Claude sign-in
  drops the sandbox's `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`
  and Bedrock/Vertex switches).
- The server only forwards it. The guest keeps it in memory for the turn and never
  writes it to disk, events, history or logs; it is gone when the turn ends. A steer
  that continues the turn reuses it; the next prompt must send it again.
