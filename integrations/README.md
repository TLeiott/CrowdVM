# CrowdVM integrations

Adapters that let eval, RL and agent frameworks run on CrowdVM sandboxes. Each talks to CrowdVM's `/api/v1`
(through the SDKs in [`../sdk`](../sdk/README.md)); the server also serves Boat's legacy Box API at `/api/box/v1`,
so the upstream `harbor-box` and `@asciidev/eve-box` packages work unchanged with a changed base URL.

| Dir | What | Language | Upstream tool |
| - | - | - | - |
| [`harbor/`](harbor/README.md) | `CrowdVMEnvironment` for `harbor run -e harbor_crowdvm_environment:CrowdVMEnvironment` | Python | Harbor ≥ 0.14 (`harbor-box` counterpart) |
| [`eve/`](eve/README.md) | `crowdvmBox()` Eve sandbox backend | TypeScript | Eve (`@asciidev/eve-box` counterpart) |
| [`agentenv/`](agentenv/README.md) | Config + conformance check for AgentEnv's built-in `boat` provider | Python | AgentEnv |
| [`evals/`](evals/README.md) | `prepare_template`, `run_episodes`, `cleanup`: Boat's evals/RL recipe as a library | Python | any harness |

Status: every adapter has stdlib/`node:test` unit tests against a fake server and was run against a real
`crowdvm-server` + node (process driver). None of them was run against the real upstream packages (Harbor, Eve,
AgentEnv are not installed here): where an upstream base class is needed, the adapters use a thin
protocol-compatible stand-in (`harbor/_harbor_shim.py`, the structural interfaces in `eve/src/index.ts`).

Run all tests: `integrations/run_tests.sh` (needs `sdk/typescript` built: `cd sdk/typescript && npm run build`).

## Using the upstream packages unchanged (Box API)

```bash
# harbor-box 0.1.2 (reads BOX_API_KEY; a CrowdVM key works)
export BOX_API_KEY=cvm_...
# BoxEnvironment(..., base_url="http://<server>:8420/api/box/v1")
# @asciidev/eve-box:  asciiBox({ apiKey: "cvm_...", baseUrl: "http://<server>:8420/api/box/v1" })
```

The Box alias keeps Boat's behaviour, including `DELETE /boxes/{id}` needing the confirm header: `harbor-box`
0.1.2 cannot delete (use `stop(delete=False)` and clean up with `crowdvm delete`), which `harbor/` fixes.
