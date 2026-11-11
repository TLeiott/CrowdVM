# @crowdvm/eve

[Eve](https://www.npmjs.com/package/eve) sandbox backend for CrowdVM: Eve agents run on CrowdVM sandboxes. The counterpart of
`@asciidev/eve-box` (`asciiBox` is exported as an alias of `crowdvmBox`, so only the import changes).

```ts
// sandbox.ts
import { defineSandbox } from "eve/sandbox";
import { crowdvmBox } from "@crowdvm/eve";

export default defineSandbox({
  backend: crowdvmBox({ apiKey: process.env.CROWDVM_API_KEY!, baseUrl: process.env.CROWDVM_API_BASE, ttlSeconds: 3600 }),
});
```

```bash
cd integrations/eve && npm install && npm run build           # needs sdk/typescript built (its dist/)
CROWDVM_API_KEY=cvm_... CROWDVM_API_BASE=http://<server>:8420/api/v1 node hello-sandbox.mjs
```

| Eve operation | CrowdVM |
| - | - |
| `create` / `resume` | `create` (name `eve-<sessionKey>`, TTL, `noEnv`, `env`, `from` = `templateKey`/`fromSnapshot`, label `eve.session`) + wait ready; `resume` restarts a stopped sandbox from `handle.state` |
| `run` | one command in `/workspace`; ≤ 600 s synchronous, longer detached; `env`, `cwd`, `timeoutMs` |
| `spawn` | detached command; `stdout`/`stderr` async iterables, `wait()`, `kill()` |
| `readTextFile` / `writeTextFile`, `readBinaryFile` / `writeBinaryFile` | file API (utf8 / base64), signed URLs above 5 MiB |
| `removePath` | `rm -rf` (refuses `/workspace` itself) |
| `resolvePath` | relative paths resolve under `/workspace` |

`/workspace` is `~/workspace` of the sandbox user; when the sandbox has sudo a `/workspace` symlink makes the absolute
path work inside commands too. `networkPolicy` other than `"allow-all"` throws `EveSandboxUnsupportedError` (no false isolation).
Options: `apiKey`, `baseUrl`, `name`, `ttlSeconds`, `noEnv`, `env`, `commandTimeoutMs`, `networkPolicy`, `fromSnapshot`, `type`;
`pollMs` is accepted but unused (spawn streams over an attached connection).

The `eve` package is not a dependency: the backend contract is described by structural interfaces in `src/index.ts`, written
from the `@asciidev/eve-box` documentation. Not yet run inside a real Eve project. With the upstream package instead: `asciiBox({ apiKey, baseUrl: "http://<server>:8420/api/box/v1" })`.

Tests: `npm test`
