# CrowdVM HTTP API

Use the CLI when you can. Use the API from code, or when the CLI is not installed.

## Basics

- Base URL: `<server>/api/v1`, e.g. `http://192.168.1.10:8420/api/v1`. Inside a sandbox it
  is `$CROWDVM_API_URL`.
- Auth: `Authorization: Bearer cvm_...` (an API key; a CLI session token `cvms_...` works
  the same). Inside a sandbox use `$CROWDVM_API_KEY`, or `$CROWDVM_TOKEN` for read and
  host only.
- Organization: header `X-CrowdVM-Org: <id or name>` (`X-Boat-Org` works too) or `?org=`.
- Paths, bodies, envelope and error codes follow Boat's v1 API.

Success: `{"ok": true, "type": "sandbox.info", ...}`.
Error: `{"ok": false, "status": 409, "code": "machine_not_running", "message": "...",
"error": {..., "details": {"retryable": ..., "retryAfterSeconds": ...}}, "requestId": "req_..."}`.

## Most useful endpoints

| Method and path | Purpose |
|---|---|
| `GET /server` | Server info, including the sizes it offers |
| `GET /me`, `GET /limits`, `GET /credits` | Account, start limits, credit balance |
| `GET /nodes` | Machines in the pool |
| `POST /sandboxes` | Create. Body e.g. `{"type":"default","ttlSeconds":1800}`. Send `Idempotency-Key` |
| `GET /sandboxes`, `GET /sandboxes/{id}` | List, read (poll `.sandbox.state` until `ready`) |
| `POST /sandboxes/{id}/stop` / `resume` / `fork` | Lifecycle |
| `DELETE /sandboxes/{id}` | Delete. Needs header `X-CrowdVM-Confirm-Delete: <id>` |
| `POST /sandboxes/{id}/commands` | Run `{"command": "...", "cwd"?, "timeoutSeconds"? (1-600, default 30), "env"?}`. `"stream": true` answers NDJSON (`started`, `stdout`, `stderr`, `exit`); `"detached": true` returns a `processId` |
| `GET /sandboxes/{id}/commands/{pid}` | Status and log tails of a detached command |
| `GET /sandboxes/{id}/commands/{pid}/stream` | Attach (NDJSON) |
| `DELETE /sandboxes/{id}/commands/{pid}` | Stop a detached command |
| `GET /sandboxes/{id}/command-history` | Past commands; `/{commandId}/logs` for output |
| `GET\|PUT\|DELETE /sandboxes/{id}/files?path=` | Read, write, delete a file (5 MiB) |
| `GET /sandboxes/{id}/files/list?path=` | List a folder |
| `POST /sandboxes/{id}/files/upload-url`, `download-url` | Signed URLs for large files |
| `GET /sandboxes/{id}/artifacts?path=` | A file, or a folder as `.tar.gz` |
| `POST /sandboxes/{id}/host` | Host a port: `{"port": 3000, "public"?: true, "title"?: "..."}`; returns `url` |
| `POST /sandboxes/{id}/desktop` | Desktop streaming URL |
| `POST /sandboxes/{id}/prompt`, `GET /sandboxes/{id}/events` | Integrated agents |
| `POST /saved-snapshots` | Save a named snapshot; `GET /saved-snapshots` lists them |
| `GET /sandboxes/{id}/snapshots/latest` | Newest snapshot of a sandbox |

Calls into a sandbox wait up to 60 s while it starts, then answer `409 sandbox_starting`.
A stopped sandbox answers `409 machine_not_running`. Too many requests: `429` with
`retryAfterSeconds`.

```sh
API=$CROWDVM_API_BASE/api/v1; AUTH="Authorization: Bearer $CROWDVM_API_KEY"
id=$(curl -s -H "$AUTH" -H 'Content-Type: application/json' -H "Idempotency-Key: $(uuidgen)" \
  -d '{"type":"default","ttlSeconds":1800}' $API/sandboxes | jq -r .sandbox.id)
curl -s -H "$AUTH" $API/sandboxes/$id | jq -r .sandbox.state
curl -s -H "$AUTH" -H 'Content-Type: application/json' -d '{"command":"uname -a"}' $API/sandboxes/$id/commands
curl -s -H "$AUTH" -X POST $API/sandboxes/$id/stop
```

## Full reference and docs, served by the CrowdVM server

| URL | Content |
|---|---|
| `<server>/openapi/crowdvm-v1.yaml` (also `/api/v1/openapi.json`) | OpenAPI spec, generated from the running server's routes |
| `<server>/llms.txt` | Index of the docs, one line per page |
| `<server>/docs/<page>.md` | One docs page as Markdown, e.g. `/docs/sandboxes.md` |
| `<server>/mcp` | Docs MCP server (HTTP): search and read the docs |
| `<server>/api-explorer` | Dashboard page listing every route and the key scope it needs |

Register the docs MCP server, e.g.
`claude mcp add --transport http crowdvm-docs --scope user <server>/mcp` or
`codex mcp add crowdvm-docs --url <server>/mcp`.

## SDKs

Python (`sdk/python`, module `crowdvm`) and TypeScript (`@crowdvm/sdk`) in the CrowdVM
repository mirror Boat's SDK method names and read `CROWDVM_API_BASE` and
`CROWDVM_API_KEY`.
