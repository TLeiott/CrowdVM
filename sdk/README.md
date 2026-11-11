# CrowdVM SDKs

Client libraries for the CrowdVM HTTP API (`<server>/api/v1`, a Boat v1 compatible surface). Both are generated from
`.ref/boat-v1.yaml` by `tools/gen.py` (typed models and one method per operation) plus a hand-written core
(transport, retries, streaming, helpers, webhook verification).

| | Package | Path | Style |
| - | - | - | - |
| Python | `crowdvm` | [`python/`](python/README.md) | snake_case, sync (+ asyncio facade), no dependencies |
| TypeScript | `@crowdvm/sdk` | [`typescript/`](typescript/README.md) | camelCase, async, no dependencies |

Configuration: `CROWDVM_API_BASE` (e.g. `http://192.168.1.10:8420/api/v1`) and `CROWDVM_API_KEY` (`cvm_...`).
All methods: [`METHODS.md`](METHODS.md).

Boat SDK compatibility: Boat's import paths (`boat_sdk...` in Python; `@boatdev/sdk` via
`npm install @boatdev/sdk@file:<crowdvm>/sdk/typescript`), Boat's positional request models
(`host_port(sid, HostPortRequest(...))`) and every code sample of Boat's SDK docs (`tools/doc_samples.py`). Boat's parameter names (`x_ascii_confirm_delete` / `xAsciiConfirmDelete`, `x_boat_org` / `xBoatOrg`), the
wrapped request-body forms and `Configuration` / `BoatApi` (+ `ApiClient`, `crowdvm.models` in Python) are accepted; see each README.
`smoke/` holds live smoke scripts (`smoke.py`, `smoke.mjs`) that exercise create, commands (sync / stream / detached + attach),
files, stop, resume, fork and delete against a real server. Adapters for Harbor, Eve, AgentEnv and eval harnesses: [`../integrations`](../integrations/README.md).
