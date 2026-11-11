# @crowdvm/sdk (TypeScript / JavaScript SDK)

TypeScript client for [CrowdVM](../../docs/ARCHITECTURE.md): persistent Ubuntu VMs for coding agents, running on
machines of your LAN or Tailnet. The HTTP API mirrors Boat's v1 API, and so does this SDK: same method names
(`create`, `command`, `commandStatus`, `readFile`, ...), same helpers (`waitUntilReady`, `runCommand`, ...).

* Node 20.19+ (both `import` and `require()`) and browsers, **zero runtime dependencies** (global `fetch`), ESM with `.d.ts` types.
* Covers every endpoint of the OpenAPI file, fully typed (generated from it).
* NDJSON streams are async iterators. Retries `429` / `503` (honoring `retryAfterSeconds`) and commands refused
  while the sandbox restores its disk.

## Install

```bash
cd sdk/typescript && npm install && npm run build     # builds dist/
npm install /path/to/CrowdVM/sdk/typescript           # in your project
```

## Configure

```bash
export CROWDVM_API_BASE="http://192.168.1.10:8420/api/v1"   # your crowdvm-server
export CROWDVM_API_KEY="cvm_..."                            # crowdvm api-key create my-app --preset ci
```

```ts
import { CrowdVM } from "@crowdvm/sdk";

const cvm = new CrowdVM();   // reads both variables; or new CrowdVM({ apiKey, baseUrl })
```

* Every API operation is a method on `cvm` taking **one flat object**: path parameters, query parameters and
  request-body fields side by side, e.g. `cvm.fork({ sandboxId, type: "small", idempotencyKey })`. Names are camelCase.
* Responses are the parsed JSON (typed). Every method accepts `{ signal, timeoutMs, maxRetries }` as a second argument.
* Helpers are functions taking the client first: `waitUntilReady`, `waitUntilIdle`, `waitForDesktop`, `waitForPrompt`,
  `waitForPromptDone`, `streamEvents`, `streamPrompt`, `stopAndRemove`, `readText`, `writeText`, `execCommand`,
  `streamCommand`, `runCommand`, `followCommand`, `paginate`.
* `cvm.request("GET", "/some/path")` is a raw escape hatch; `cvm.withOptions({ maxRetries: 10 })` returns a tweaked copy.
* Options: `new CrowdVM({ apiKey, baseUrl, fetch, maxRetries, maxRetryDelayMs, timeoutMs, headers })`.

## Create, run a command, stop

```ts
import { CrowdVM, waitUntilReady } from "@crowdvm/sdk";
import type { CommandResponse } from "@crowdvm/sdk";

const cvm = new CrowdVM({ apiKey: process.env.CROWDVM_API_KEY!, baseUrl: process.env.CROWDVM_API_BASE! });

const created = await cvm.create({ ttlSeconds: 1800 });
const sandboxId = created.sandbox!.id;
try {
  await waitUntilReady(cvm, sandboxId);
  const result = (await cvm.command({ sandboxId, command: "echo hi && uname -a" })) as CommandResponse;
  console.log(result.exitCode);
  console.log(result.stdout);
} finally {
  await cvm.stop({ sandboxId });
}
```

* `command()` returns `CommandResponse | CommandStartedResponse`. With `detached: true` TypeScript narrows it to
  `CommandStartedResponse`; with `stream: true` it returns an async iterator of frames.
* To delete the sandbox and its snapshots instead of a stop: `cvm.deleteSandbox({ sandboxId })` (the
  confirmation headers default to the id; Boat's `xAsciiConfirmDelete` is accepted, `confirmDelete` is the CrowdVM alias).

Boat SDK code runs unchanged, imports included: install this package under Boat's name,
`npm install @boatdev/sdk@file:<crowdvm>/sdk/typescript`, and `import { BoatApi } from "@boatdev/sdk"` (or `require`)
resolves to it. `new BoatApi(new Configuration({ basePath, accessToken }))`, Boat's `_public: true` spelling on
`hostPort`, `ResponseError` with a readable `error.response` (`await error.response.json()`),
`xBoatOrg` (the `X-Boat-Org` header) on every operation that has it and on `create` / `fork`, and the wrapped body form
`fork({ sandboxId, forkRequest: { type: "small" } })`. CrowdVM extras: `placement` (`create` / `resume` / `fork`), `nested`
(`create`), and `browser({ sandboxId, profile })`.

## Run commands

A synchronous command times out after 30 seconds by default; set `timeoutSeconds` up to 600. For longer commands use
[`runCommand`](#run-a-command-with-no-time-limit). A command sent while the sandbox starts waits up to 60 s.

```ts
import { execCommand } from "@crowdvm/sdk";

const result = (await cvm.command({ sandboxId, command: "npm test", cwd: "my-repo", timeoutSeconds: 600 })) as CommandResponse;
console.log(result.exitCode, result.timedOut, result.stdout, result.stderr);

// Same call, but it keeps resending a command that was refused while the sandbox restored its disk.
const again = await execCommand(cvm, sandboxId, "node -v");
```

### Stream the output

```ts
import { streamCommand } from "@crowdvm/sdk";

for await (const frame of streamCommand(cvm, sandboxId, "npm test", { cwd: "my-repo", timeoutSeconds: 600 })) {
  if (frame.type === "stdout" || frame.type === "stderr") process.stdout.write(String(frame.data));
  if (frame.type === "exit") console.log("exit code:", frame.exitCode);
  if (frame.type === "error") console.error(frame.message);
}
```

Pass `signal` (an `AbortSignal`) in the options to stop reading early.

### Run a command with no time limit

`runCommand` starts the command in the background, then yields its output until it ends. It reconnects by itself,
with no gap and no repeat.

```ts
import { runCommand } from "@crowdvm/sdk";

for await (const frame of runCommand(cvm, sandboxId, "npm run test:e2e", { cwd: "my-repo" })) {
  if (frame.type === "stdout" || frame.type === "stderr") process.stdout.write(frame.data);
  if (frame.type === "exit") console.log("exit code:", frame.exitCode);
}
```

* The first frame is `{ type: "started", processId }`.
* A loop that stops early leaves the command running; stop it with `cvm.stopCommand({ sandboxId, processId })`.
* `followCommand(cvm, sandboxId, processId)` follows a command that you started with `detached: true`.

### Run a server in the background

```ts
const started = await cvm.command({ sandboxId, command: "npm run build", detached: true });   // CommandStartedResponse
let status = await cvm.commandStatus({ sandboxId, processId: started.processId });
while (status.running) {
  await new Promise((r) => setTimeout(r, 2000));
  status = await cvm.commandStatus({ sandboxId, processId: started.processId });
}
console.log(status.status, status.exitCode, status.stdout);
```

`attachCommand`, `stopCommand`, `commandHistory`, `getCommand`, `commandLogs` and `processes` complete the group.

## Files

```ts
import { readText, writeText } from "@crowdvm/sdk";

await writeText(cvm, sandboxId, "notes/result.txt", "done\n");
console.log(await readText(cvm, sandboxId, "notes/result.txt"));
```

Paths are relative to `/home/user`. `readFile` / `writeFile` also accept `encoding: "base64"`. For files of any size,
get a signed URL and send the bytes with `fetch`:

```ts
import { readFile } from "node:fs/promises";

const up = await cvm.createUploadUrl({ sandboxId, path: "data/input.csv" });
await fetch(up.url, { method: up.method, body: await readFile("input.csv") });

const down = await cvm.createDownloadUrl({ sandboxId, path: "data/input.csv" });
const bytes = await (await fetch(down.url)).arrayBuffer();
```

`artifact()` returns a `Blob`. Also: `listFiles`, `getFileInfo`, `searchFiles`, `findInFiles`, `createFolder`, `moveFile`,
`setFilePermissions`, `replaceInFiles`, `deleteFile`.

### Copy a local folder in

```ts
import { execFileSync } from "node:child_process";
import { lstatSync } from "node:fs";
import { join } from "node:path";
import { execCommand } from "@crowdvm/sdk";
import type { CrowdVM } from "@crowdvm/sdk";

const isFileOrLink = (p: string) => { try { return !lstatSync(p).isDirectory(); } catch { return false; } };

async function push(cvm: CrowdVM, sandboxId: string, folder: string, remote = "repo") {
  const listed = execFileSync("git", ["ls-files", "-z", "--cached", "--others", "--exclude-standard"], { cwd: folder })
    .toString().split("\0").filter((name) => name && isFileOrLink(join(folder, name)));
  const packed = execFileSync("tar", ["--null", "--no-recursion", "-czf", "-", "-T", "-"], {
    cwd: folder, input: listed.join("\0"), maxBuffer: 4 * 1024 ** 3,
  });
  const up = await cvm.createUploadUrl({ sandboxId, path: "push.tgz" });
  await fetch(up.url, { method: up.method, body: packed });
  await execCommand(cvm, sandboxId, `mkdir -p ${remote} && tar xzf push.tgz -C ${remote} && rm push.tgz`, undefined, 600);
}
```

## Run one job and delete the sandbox

```ts
import { runCommand, waitUntilReady } from "@crowdvm/sdk";

const created = await cvm.create({ from: "ci-base", type: "large", ttlSeconds: 3600 });
const sandboxId = created.sandbox!.id;
let code: unknown;
try {
  await waitUntilReady(cvm, sandboxId);
  await push(cvm, sandboxId, ".");
  for await (const frame of runCommand(cvm, sandboxId, "npm ci && npm test", { cwd: "repo" })) {
    if (frame.type === "stdout" || frame.type === "stderr") process.stdout.write(frame.data);
    if (frame.type === "exit") code = frame.exitCode;
  }
  const usage = await cvm.usage({ sandboxId });
  console.log(`exit ${code} · ${usage.seconds}s · ${usage.sandboxType} · ${usage.dollars.toFixed(4)} credits`);
} finally {
  await cvm.deleteSandbox({ sandboxId });
}
process.exitCode = typeof code === "number" ? code : 1;
```

## Public URL for a port

```ts
await cvm.command({ sandboxId, command: "python3 -m http.server 8000", detached: true });
const hosted = await cvm.hostPort({ sandboxId, port: 8000, public: true });
console.log(hosted.url);
```

## Machine size, environment variables, no-env

```ts
const created = await cvm.create({ type: "large", ttlSeconds: 3600 });      // small | default | large | xlarge
const info = await cvm.get({ sandboxId: created.sandbox!.id });
console.log(info.sandbox.type, info.sandbox.vcpu, info.sandbox.memoryGB);

await cvm.create({ ttlSeconds: 3600, env: { DATABASE_URL: "postgres://...", FEATURE_FLAG: "1" } });   // per-sandbox env
await cvm.create({ ttlSeconds: 1800, noEnv: true });                                                  // none of your secrets
await cvm.resume({ sandboxId, type: "xlarge" });                                                      // resize: stop, then resume

for (const t of (await cvm.limits({})).machineTypes ?? []) console.log(t.type, t.available ? "ok" : t.reason);
```

## Saved snapshots and forks

```ts
import { randomUUID } from "node:crypto";

await cvm.saveSavedSnapshot({ sandboxId, name: "web-stack" });
let snap = (await cvm.getSavedSnapshot({ name: "web-stack" })).snapshot;
while (snap.status !== "ready" && snap.status !== "failed") {
  await new Promise((r) => setTimeout(r, 5000));
  snap = (await cvm.getSavedSnapshot({ name: "web-stack" })).snapshot;
}

const fromTemplate = await cvm.create({ idempotencyKey: randomUUID(), from: "web-stack", ttlSeconds: 900 });
const forked = await cvm.fork({ sandboxId, idempotencyKey: randomUUID(), ttlSeconds: 900 });
```

`idempotencyKey` makes `create` and `fork` safe to retry. Snapshots: `listSnapshots`, `listSandboxSnapshots`,
`getLatestSandboxSnapshot`, `getSnapshotTree`, `getSnapshotFile`, `getSnapshotDownload`, `deleteSnapshot`...

## Prompt an integrated agent

```ts
import { waitForPrompt } from "@crowdvm/sdk";

const queued = await cvm.prompt({ sandboxId, provider: "codex", prompt: "Run the tests and fix the failures." });
await cvm.steer({ sandboxId, message: "Skip the integration tests." });     // add an instruction to the running turn

const run = await waitForPrompt(cvm, sandboxId, queued.promptId);           // waitForPromptDone throws PromptFailed unless it finished
console.log(run.status);

const events = await cvm.events({ sandboxId, limit: 50, type: "prompt,response" });
for (const event of events.events) console.log(event.type);
```

`provider` is optional (server default). Pass `new: true` for a new conversation or `conversationId` to continue one.
`interrupt({ sandboxId, conversation })` discards the running turn; `steer` keeps it running. `providerModels()` lists models.

### Stream the agent's work

```ts
import { streamPrompt } from "@crowdvm/sdk";

for await (const event of streamPrompt(cvm, sandboxId, { provider: "codex", prompt: "Run pwd and ls." })) {
  if (event.type !== "response") continue;
  const data = (event as any).data;
  if (data?.content) process.stdout.write(data.content);
}
```

For all events of a sandbox use `streamEvents(cvm, sandboxId, { type: "prompt,response", signal })` and stop it with an
`AbortController` (the events endpoint is polled, so it is not a true stream).

## Browse a stopped sandbox

```ts
const latest = await cvm.getLatestSandboxSnapshot({ sandboxId });
const tree = await cvm.getSnapshotTree({ snapshotId: latest.snapshot!.id });
const blob = await cvm.getSnapshotFile({ snapshotId: latest.snapshot!.id, path: "projects/app/.env" });   // Blob
```

## Pagination

```ts
import { paginate } from "@crowdvm/sdk";

for await (const sb of paginate((p) => cvm.sandboxes(p), "sandboxes", { state: "running", limit: 200 })) console.log(sb.id);
```

## Errors and retries

A non-2xx answer rejects with an `ApiError` (`ResponseError` is an alias) carrying `status`, `code`, `message`,
`details`, `requestId`, `body` and the `response`. Subclasses: `BadRequestError`, `AuthenticationError`,
`PaymentRequiredError`, `PermissionDeniedError`, `NotFoundError`, `ConflictError`, `RateLimitError`, `ServerError`,
`ServiceUnavailableError`. Network failures reject with `APIConnectionError`; helpers throw `WaitTimeout`,
`SandboxFailed`, `PromptFailed`. All extend `CrowdVMError`.

```ts
import { NotFoundError } from "@crowdvm/sdk";

try {
  await cvm.get({ sandboxId: "bx_missing" });
} catch (error) {
  if (error instanceof NotFoundError) console.error(error.status, error.code, error.requestId);
}
```

The client retries (default `maxRetries: 3`, jittered backoff capped by `maxRetryDelayMs`):

* `503` (`out_of_capacity`, ...) after the `retryAfterSeconds` of the body; never `no_ready_machine`, the answer to `failFast`;
* `429` `IP_RATE_LIMIT_EXCEEDED` and `rate_limited` on the *minute* window. Not retried: hour/day windows,
  `limit_reached`, `member_limit_reached`, `trial_compute_limit_reached`;
* `502 sandbox_direct_failed` with message `sandbox_restoring` and `409 sandbox_starting` (the command did not run);
* `409 idempotency_in_progress`; connection failures of `GET` requests.

A `502` that may have run your command is never resent automatically (check `error.retryable`).

## Webhooks

```ts
import { constructEvent, verifyWebhook } from "@crowdvm/sdk";

// in your HTTP handler: the RAW body (string or bytes) and the request headers
const event = await constructEvent(rawBody, req.headers, process.env.WEBHOOK_SECRET!);   // throws WebhookVerificationError
if (event.type === "sandbox.ready") console.log(event.data.sandbox.id);
```

`verifyWebhook(rawBody, headers, secret)` resolves to a boolean. It uses WebCrypto (Node 20.19+, browsers, edge). Pass an array of
secrets while one rotates. Timestamps older than 5 minutes are rejected (`toleranceSeconds`). `Headers`, Node header
objects and `[name, value]` pairs are all accepted; `X-CrowdVM-Delivery|Timestamp|Signature` are read (Boat's
`X-Ascii-*` too). Deduplicate on `event.id`: delivery is at-least-once. `signWebhook(...)` makes signatures for tests.

Node only, synchronous (`node:crypto`, constant-time compare):

```ts
import { verifyWebhookSync } from "@crowdvm/sdk/node";

if (!verifyWebhookSync(rawBody, req.headers, secret)) return res.status(401).end();
```

Create endpoints with `cvm.createWebhook({ name, url, events: ["sandbox.ready", "sandbox.error"] })`.

## All methods

See [`../METHODS.md`](../METHODS.md) (generated from the OpenAPI file: Python name, TypeScript name, endpoint).

## Development

```bash
cd sdk/typescript
npm install                 # TypeScript only (devDependency)
npm test                    # tsc build + type-surface check + node --test against a local fake server
python3 ../tools/gen.py     # regenerate src/types.ts and src/operations.ts from .ref/boat-v1.yaml (needs PyYAML)
```
