// Compile-time checks of the public surface (never executed). `npm test` runs tsc over this file.
import {
  ApiError, CrowdVM, NotFoundError, constructEvent, execCommand, followCommand, paginate, readText, runCommand,
  streamCommand, streamEvents, streamPrompt, verifyWebhook, waitForPrompt, waitUntilReady, writeText,
} from "../src/index.js";
import type { CommandResponse, CommandStartedResponse, Sandbox, WebhookEvent } from "../src/index.js";
import { verifyWebhookSync } from "../src/node.js";

export async function check(): Promise<void> {
  const cvm = new CrowdVM({ apiKey: "cvm_x", baseUrl: "http://10.0.0.5:8420/api/v1" });
  const created = await cvm.create({ ttlSeconds: 1800, from: "web-stack", noEnv: true, idempotencyKey: "k", env: { A: "1" } });
  const sandboxId: string = created.sandbox!.id;
  const sb: Sandbox = (await cvm.get({ sandboxId })).sandbox;
  const state: "ready" | "error" | string = sb.state;
  void state;

  const out = (await cvm.command({ sandboxId, command: "echo hi" })) as CommandResponse;
  const code: number | null = out.exitCode;
  const started: CommandStartedResponse = await cvm.command({ sandboxId, command: "sleep 9", detached: true });
  const pid: number = started.processId;
  for await (const f of cvm.command({ sandboxId, command: "ls", stream: true })) {
    if (f.type === "stdout") process_(f.data ?? "");
  }
  for await (const f of runCommand(cvm, sandboxId, "make", { cwd: "repo" })) {
    if (f.type === "exit") process_(String(f.exitCode));
    if (f.type === "started") process_(String(f.processId));
  }
  for await (const f of streamCommand(cvm, sandboxId, "ls", { timeoutSeconds: 5 })) void f.type;
  for await (const f of followCommand(cvm, sandboxId, pid)) void f.type;
  await execCommand(cvm, sandboxId, "ls", "repo", 600);
  await execCommand(cvm, sandboxId, "ls", { cwd: "repo" });

  await cvm.stop({ sandboxId });
  await cvm.stop({ sandboxId, force: true });
  await cvm.resume({ sandboxId, type: "large", noEnv: true });
  await cvm.fork({ sandboxId, idempotencyKey: "x", ttlSeconds: 900 });
  await cvm.deleteSandbox({ sandboxId });
  await cvm.deleteSandbox({ sandboxId, confirmDelete: sandboxId });
  await cvm.hostPort({ sandboxId, port: 8000, public: true });
  await cvm.writeFile({ sandboxId, path: "a", content: "b", encoding: "base64" });
  const blob: Blob = await cvm.artifact({ sandboxId, path: "a" });
  void blob;
  const up = await cvm.createUploadUrl({ sandboxId, path: "x" });
  void up.url;
  await cvm.prompt({ sandboxId, provider: "codex", prompt: "go" });
  await cvm.prompt({ sandboxId, prompt: "no provider needed" });
  await cvm.steer({ sandboxId, message: "skip", conversation: "c1" });
  await cvm.events({ sandboxId, limit: 50, type: "prompt,response" });
  await cvm.commandStatus({ sandboxId, processId: 1, tailBytes: 10 });
  await cvm.createWebhook({ name: "n", url: "https://x", events: ["sandbox.ready"] });
  await cvm.saveSavedSnapshot({ sandboxId, name: "web-stack" });
  await cvm.createEnvironment({ name: "e" } as any);
  await cvm.me();
  await cvm.limits({});
  await cvm.sandboxes({ state: "running", limit: 10 }, { signal: AbortSignal.timeout(1000) });
  for await (const s of paginate<any, "sandboxes", Sandbox>((p) => cvm.sandboxes(p), "sandboxes", { limit: 100 })) void s.id;
  for await (const e of streamEvents(cvm, sandboxId, { type: "response", signal: new AbortController().signal })) void e.type;
  for await (const e of streamPrompt(cvm, sandboxId, { provider: "codex", prompt: "hi" })) void e.type;
  const run = await waitForPrompt(cvm, sandboxId, "p");
  void run.status;
  await waitUntilReady(cvm, sandboxId, { timeoutMs: 1000 });
  await writeText(cvm, sandboxId, "a", "b");
  const text: string = await readText(cvm, sandboxId, "a");
  void text;

  try {
    await cvm.get({ sandboxId: "bx_missing" });
  } catch (e) {
    if (e instanceof NotFoundError) void e.code;
    if (e instanceof ApiError) void [e.status, e.details, e.requestId, e.retryAfterSeconds, e.response.status];
  }
  const ev: WebhookEvent = await constructEvent("{}", new Headers(), "whsec_x");
  void ev.data.sandbox.id;
  await verifyWebhook(new Uint8Array(), { "x-crowdvm-delivery": "a" }, ["new", "old"], { toleranceSeconds: 60 });
  verifyWebhookSync("{}", new Headers(), "s");

  // @ts-expect-error missing required path parameter
  await cvm.get({});
  // @ts-expect-error unknown state literal
  const bad: Sandbox["state"] = "banana";
  void bad;
  void code;
}

declare function process_(s: string): void;

// README snippets that touch response fields
export async function readmeFields(cvm: CrowdVM, sandboxId: string): Promise<void> {
  const usage = await cvm.usage({ sandboxId });
  void [usage.seconds, usage.sandboxType, usage.dollars.toFixed(4)];
  const latest = await cvm.getLatestSandboxSnapshot({ sandboxId });
  const tree = await cvm.getSnapshotTree({ snapshotId: latest.snapshot!.id });
  void tree;
  const blob: Blob = await cvm.getSnapshotFile({ snapshotId: latest.snapshot!.id, path: "x" });
  void blob;
  let snap = (await cvm.getSavedSnapshot({ name: "web-stack" })).snapshot;
  void [snap.status];
  for (const t of (await cvm.limits({})).machineTypes ?? []) void [t.type, t.available, t.reason];
  const info = await cvm.get({ sandboxId });
  void [info.sandbox.type, info.sandbox.vcpu, info.sandbox.memoryGB];
  const status = await cvm.commandStatus({ sandboxId, processId: 1 });
  void [status.running, status.status, status.exitCode, status.stdout];
  const events = await cvm.events({ sandboxId, limit: 50 });
  for (const e of events!.events) void e.type;
  const hosted = await cvm.hostPort({ sandboxId, port: 1, public: true });
  void hosted.url;
  const w = await cvm.createWebhook({ name: "n", url: "u", events: ["sandbox.ready"] });
  void [w.secret, w.webhook.id];
  void (await cvm.providerModels()).providers;
}
