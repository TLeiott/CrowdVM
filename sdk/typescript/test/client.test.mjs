import assert from "node:assert/strict";
import { test } from "node:test";
import { ApiError, AuthenticationError, CrowdVM, NotFoundError, RateLimitError, ServiceUnavailableError, APIConnectionError } from "../dist/index.js";
import { err, fake, lines, ok } from "./fake.mjs";

const SB = { id: "bx_abcdefgh", name: "t", state: "ready" };
const mk = (srv, o = {}) => new CrowdVM({ apiKey: "cvm_test", baseUrl: srv.base, maxRetryDelayMs: 5, ...o });
async function withServer(fn) { const srv = await fake(); try { await fn(srv); } finally { await srv.close(); } }

test("create: flat params, idempotency header, auth header", () => withServer(async (srv) => {
  srv.route("POST", "/api/v1/sandboxes", ok({ sandbox: SB }, 202));
  const out = await mk(srv).create({ ttlSeconds: 1800, from: "web-stack", noEnv: true, idempotencyKey: "k1", env: { A: "1" }, labels: { s: "x" } });
  const r = srv.requests[0];
  assert.equal(r.headers.authorization, "Bearer cvm_test");
  assert.equal(r.headers["idempotency-key"], "k1");
  assert.deepEqual(r.json, { ttlSeconds: 1800, from: "web-stack", noEnv: true, env: { A: "1" }, labels: { s: "x" } });
  assert.equal(out.sandbox.id, "bx_abcdefgh");
}));

test("ttlSeconds null is sent; empty body is {}", () => withServer(async (srv) => {
  srv.route("POST", "/api/v1/sandboxes", ok({ sandbox: SB }, 202));
  const c = mk(srv);
  await c.create({ ttlSeconds: null });
  await c.create();
  assert.deepEqual(srv.requests[0].json, { ttlSeconds: null });
  assert.deepEqual(srv.requests[1].json, {});
}));

test("base URL normalisation and env configuration", () => {
  assert.equal(new CrowdVM({ baseUrl: "http://10.0.0.5:8420/" }).baseUrl, "http://10.0.0.5:8420/api/v1");
  assert.equal(new CrowdVM({ baseUrl: "http://h/api/v1" }).baseUrl, "http://h/api/v1");
  process.env.CROWDVM_API_KEY = "cvm_env"; process.env.CROWDVM_API_BASE = "http://h:1/api/v1";
  const c = new CrowdVM();
  delete process.env.CROWDVM_API_KEY; delete process.env.CROWDVM_API_BASE;
  assert.deepEqual([c.apiKey, c.baseUrl], ["cvm_env", "http://h:1/api/v1"]);
});

test("delete confirmation header defaults to the id", () => withServer(async (srv) => {
  srv.route("DELETE", "/api/v1/sandboxes/bx_x", ok({}, 202));
  srv.route("DELETE", "/api/v1/snapshots/snap_1", ok({}, 202));
  const c = mk(srv);
  await c.deleteSandbox({ sandboxId: "bx_x" });
  await c.deleteSnapshot({ snapshotId: "snap_1" });
  await c.deleteSandbox({ sandboxId: "bx_x", confirmDelete: "bx_x" });
  assert.equal(srv.requests[0].headers["x-crowdvm-confirm-delete"], "bx_x");
  assert.equal(srv.requests[1].headers["x-crowdvm-confirm-delete"], "snap_1");
}));

test("query encoding, path quoting, bodies", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/sandboxes/bx_x/commands/7", ok({ running: false, exitCode: 0 }));
  srv.route("GET", "/api/v1/sandboxes/bx_x/events", ok({ events: [] }));
  srv.route("GET", "/api/v1/saved-snapshots/web%20stack", ok({ snapshot: { status: "ready" } }));
  srv.route("PUT", "/api/v1/sandboxes/bx_x/files", ok({ success: true }));
  srv.route("POST", "/api/v1/sandboxes/bx_x/host", ok({ url: "http://u" }));
  const c = mk(srv);
  assert.equal((await c.commandStatus({ sandboxId: "bx_x", processId: 7, tailBytes: 100, stdoutOffset: 0 })).exitCode, 0);
  assert.deepEqual(srv.requests[0].query, { tailBytes: "100", stdoutOffset: "0" });
  await c.events({ sandboxId: "bx_x", limit: 5, type: ["prompt", "response"], sort: "asc", cursor: null });
  assert.deepEqual(srv.requests[1].query, { limit: "5", type: "prompt,response", sort: "asc" });
  assert.equal((await c.getSavedSnapshot({ name: "web stack" })).snapshot.status, "ready");
  await c.writeFile({ sandboxId: "bx_x", path: "a.txt", content: "hi", encoding: "base64" });
  assert.deepEqual(srv.requests[3].json, { path: "a.txt", content: "hi", encoding: "base64" });
  await c.hostPort({ sandboxId: "bx_x", port: 8000, public: true });
  assert.deepEqual(srv.requests[4].json, { port: 8000, public: true });
}));

test("bodyless operations reject unknown params; missing path params throw", () => withServer(async (srv) => {
  const c = mk(srv);
  await assert.rejects(() => c.get({ sandboxId: "bx_x", bogus: 1 }), TypeError);
  await assert.rejects(() => c.get({}), TypeError);
}));

test("errors are typed", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/sandboxes/bx_missing", err(404, "not_found", "no such sandbox"));
  srv.route("GET", "/api/v1/me", err(401, "unauthorized"));
  srv.route("GET", "/api/v1/limits", { status: 500, body: undefined });
  const c = mk(srv);
  const e = await c.get({ sandboxId: "bx_missing" }).catch((x) => x);
  assert.ok(e instanceof NotFoundError && e instanceof ApiError);
  assert.deepEqual([e.status, e.code, e.message, e.requestId], [404, "not_found", "no such sandbox", "req_test"]);
  assert.equal(e.response.status, 404);
  assert.ok(await c.me().catch((x) => x) instanceof AuthenticationError);
  const e500 = await c.limits({}).catch((x) => x);
  assert.equal(e500.status, 500);
}));

test("retries 503 honoring retryAfterSeconds", () => withServer(async (srv) => {
  srv.seq("GET", "/api/v1/limits", err(503, "out_of_capacity", "x", { retryAfterSeconds: 0.01 }), err(503, "out_of_capacity", "x", {}, { retryAfterSeconds: 0.02 }), ok({ canStart: true }));
  const t = Date.now();
  assert.equal((await mk(srv, { maxRetryDelayMs: 1000 }).limits({})).canStart, true);
  assert.equal(srv.requests.length, 3);
  assert.ok(Date.now() - t >= 25, "waited the requested time");
}));

test("429: ip limit and minute window retry; hour window / limit_reached do not", () => withServer(async (srv) => {
  srv.seq("POST", "/api/v1/sandboxes", err(429, "IP_RATE_LIMIT_EXCEEDED", "x", {}, { retryAfter: 0 }),
    err(429, "rate_limited", "x", { window: "minute", limit: 12 }, { retryAfterSeconds: 0 }), ok({ sandbox: SB }, 202));
  await mk(srv).create();
  assert.equal(srv.requests.length, 3);
  const s2 = await fake();
  try {
    s2.route("POST", "/api/v1/sandboxes", err(429, "rate_limited", "x", { window: "hour" }));
    assert.ok(await mk(s2).create().catch((x) => x) instanceof RateLimitError);
    assert.equal(s2.requests.length, 1);
  } finally { await s2.close(); }
  const s3 = await fake();
  try {
    s3.route("POST", "/api/v1/sandboxes", err(429, "limit_reached"));
    await assert.rejects(() => mk(s3).create(), RateLimitError);
    assert.equal(s3.requests.length, 1);
  } finally { await s3.close(); }
}));

test("retries are bounded", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/me", err(503, "out_of_capacity", "x", { retryAfterSeconds: 0 }));
  await assert.rejects(() => mk(srv, { maxRetries: 2 }).me(), ServiceUnavailableError);
  assert.equal(srv.requests.length, 3);
  await assert.rejects(() => mk(srv, { maxRetries: 0 }).me(), ServiceUnavailableError);
  assert.equal(srv.requests.length, 4);
}));

test("retries restoring / starting, but not an ambiguous 502", () => withServer(async (srv) => {
  srv.seq("POST", "/api/v1/sandboxes/bx_x/commands",
    err(502, "sandbox_direct_failed", "sandbox_restoring", { retryable: true, message: "sandbox_restoring" }, {}),
    err(409, "sandbox_starting"), ok({ exitCode: 0, stdout: "hi\n" }));
  // the server gave no retryAfter, so the built-in waits (2 s / 3 s) apply: shorten them with a tiny cap
  const out = await mk(srv, { maxRetryDelayMs: 5 }).command({ sandboxId: "bx_x", command: "echo hi" });
  assert.equal(out.stdout, "hi\n");
  assert.equal(srv.requests.length, 3);
  const s2 = await fake();
  try {
    s2.route("POST", "/api/v1/sandboxes/bx_x/commands", err(502, "sandbox_direct_failed", "connection reset", { retryable: true }));
    const e = await mk(s2).command({ sandboxId: "bx_x", command: "make deploy" }).catch((x) => x);
    assert.equal(e.retryable, true);
    assert.equal(s2.requests.length, 1);
  } finally { await s2.close(); }
}));

test("artifact is a Blob; 304 gives null", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/sandboxes/bx_x/artifacts", { status: 200, raw: new Uint8Array([0, 1, 98, 105, 110]) });
  srv.route("GET", "/api/v1/sandboxes/bx_x/events", { status: 304 });
  const c = mk(srv);
  const blob = await c.artifact({ sandboxId: "bx_x", path: "out.bin" });
  assert.deepEqual([...new Uint8Array(await blob.arrayBuffer())], [0, 1, 98, 105, 110]);
  assert.equal(await c.events({ sandboxId: "bx_x", ifNoneMatch: '"etag"' }), null);
  assert.equal(srv.requests[1].headers["if-none-match"], '"etag"');
}));

test("command: sync, detached, stream", () => withServer(async (srv) => {
  srv.route("POST", "/api/v1/sandboxes/bx_x/commands", (r) => {
    if (r.json.stream) return lines([{ type: "started", commandId: "c1" }, { type: "stdout", data: "a" }, { type: "stderr", data: "b" }, { type: "exit", exitCode: 3 }]);
    if (r.json.detached) return ok({ processId: 9, pid: 123, command: r.json.command });
    return ok({ exitCode: 0, stdout: "ok", stderr: "", timedOut: false });
  });
  const c = mk(srv);
  const r = await c.command({ sandboxId: "bx_x", command: "ls", cwd: "repo", timeoutSeconds: 600 });
  assert.deepEqual([r.exitCode, r.timedOut], [0, false]);
  assert.deepEqual(srv.requests[0].json, { command: "ls", cwd: "repo", timeoutSeconds: 600 });
  assert.equal((await c.command({ sandboxId: "bx_x", command: "sleep 1", detached: true })).processId, 9);
  const frames = [];
  for await (const f of c.command({ sandboxId: "bx_x", command: "x", stream: true })) frames.push(f);
  assert.deepEqual(frames.map((f) => f.type), ["started", "stdout", "stderr", "exit"]);
  assert.equal(frames.at(-1).exitCode, 3);
}));

test("request() raw escape hatch, withOptions, custom fetch, connection error", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/custom", ok({ x: 1 }));
  const c = mk(srv);
  assert.equal((await c.request("GET", "/custom")).x, 1);
  assert.equal(c.withOptions({ maxRetries: 9 }).maxRetries, 9);
  let called = 0;
  const c2 = new CrowdVM({ apiKey: "k", baseUrl: "http://nowhere.invalid/api/v1", fetch: async (url) => { called++; return new Response(JSON.stringify({ ok: true, via: String(url) }), { status: 200 }); } });
  assert.match((await c2.me()).via, /nowhere\.invalid\/api\/v1\/me$/);
  assert.equal(called, 1);
  const dead = new CrowdVM({ apiKey: "k", baseUrl: "http://127.0.0.1:1/api/v1", maxRetries: 0 });
  await assert.rejects(() => dead.me(), APIConnectionError);
}));

test("abort signal cancels a request", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/me", ok({}));
  const ctl = new AbortController();
  ctl.abort();
  await assert.rejects(() => mk(srv).me({ signal: ctl.signal }));
  assert.equal(srv.requests.length, 0);
}));

test("provider models and webhooks ops exist", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/provider-models", ok({ providers: [] }));
  srv.route("POST", "/api/v1/webhooks", ok({ webhook: { id: "wh_1" }, secret: "whsec_x" }, 201));
  const c = mk(srv);
  assert.deepEqual((await c.providerModels()).providers, []);
  assert.equal((await c.createWebhook({ name: "p", url: "https://e/h", events: ["sandbox.ready"] })).secret, "whsec_x");
}));

test("attachCommand stream idle timeout", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/sandboxes/bx_x/commands/1/stream", () => ({ status: 200, hang: true }));
  const c = mk(srv, { streamIdleTimeoutMs: 100 });
  await assert.rejects(async () => { for await (const _ of c.attachCommand({ sandboxId: "bx_x", processId: 1 })) { /* nothing arrives */ } }, APIConnectionError);
}));

test("Boat names: xAsciiConfirmDelete, confirmDelete alias, xBoatOrg, Configuration/BoatApi, wrapped body", () => withServer(async (srv) => {
  const { BoatApi, Configuration } = await import("../dist/index.js");
  srv.route("DELETE", "/api/v1/sandboxes/bx_x", ok({}, 202));
  srv.route("POST", "/api/v1/sandboxes", ok({ sandbox: SB }, 202));
  srv.route("POST", "/api/v1/sandboxes/bx_x/fork", ok({ sandbox: SB }, 202));
  srv.route("POST", "/api/v1/sandboxes/bx_x/browser", ok({ browserUrl: "http://b" }));
  const boat = new BoatApi(new Configuration({ basePath: srv.base, accessToken: "cvm_t" }));
  await boat.deleteSandbox({ sandboxId: "bx_x", xAsciiConfirmDelete: "bx_x" });
  await boat.deleteSandbox({ sandboxId: "bx_x", confirmDelete: "bx_x" });
  assert.equal(srv.requests[0].headers["x-ascii-confirm-delete"], "bx_x");
  assert.equal(srv.requests[0].headers["x-crowdvm-confirm-delete"], undefined);
  assert.equal(srv.requests[0].headers.authorization, "Bearer cvm_t");
  assert.equal(srv.requests[1].headers["x-crowdvm-confirm-delete"], "bx_x");
  await boat.create({ xBoatOrg: "acme", ttlSeconds: 60, nested: true, placement: { labels: { rack: "a" } } });
  await boat.fork({ sandboxId: "bx_x", xBoatOrg: "acme", forkRequest: { type: "small" }, placement: { gpu: true } });
  assert.equal(srv.requests[2].headers["x-boat-org"], "acme");
  assert.deepEqual(srv.requests[2].json, { ttlSeconds: 60, nested: true, placement: { labels: { rack: "a" } } });
  assert.equal(srv.requests[3].headers["x-boat-org"], "acme");
  assert.deepEqual(srv.requests[3].json, { type: "small", placement: { gpu: true } });
  assert.equal((await boat.browser({ sandboxId: "bx_x", profile: "/data/p" })).browserUrl, "http://b");
}));

test("Boat spellings: hostPort _public, ResponseError.response.json()", () => withServer(async (srv) => {
  srv.route("POST", "/api/v1/sandboxes/bx_x/host", ok({ url: "http://u" }));
  srv.route("GET", "/api/v1/sandboxes/bx_missing", err(404, "sandbox_not_found"));
  const c = mk(srv);
  await c.hostPort({ sandboxId: "bx_x", port: 8000, _public: true });
  assert.deepEqual(srv.requests[0].json, { port: 8000, public: true });
  const { ResponseError } = await import("../dist/index.js");
  const e = await c.get({ sandboxId: "bx_missing" }).then(() => null, (x) => x);
  assert.ok(e instanceof ResponseError);
  assert.equal(e.response.status, 404);
  assert.equal((await e.response.json()).code, "sandbox_not_found");
}));

test("a Set in a body goes out as an array (Boat's webhook sample)", async () => {
  let got;
  const fetch = async (_url, init) => { got = JSON.parse(init.body); return new Response(JSON.stringify({ ok: true, webhook: { id: "wh_1" }, secret: "s" }), { status: 201, headers: { "content-type": "application/json" } }); };
  const { CrowdVM } = await import("../dist/index.js");
  const c = new CrowdVM({ apiKey: "k", baseUrl: "http://x/api/v1", fetch });
  await c.createWebhook({ name: "p", url: "https://e/h", events: new Set(["sandbox.ready", "sandbox.error"]) });
  assert.deepEqual(got.events, ["sandbox.ready", "sandbox.error"]);
});

test("failFast's 503 no_ready_machine is not retried", () => withServer(async (srv) => {
  srv.route("POST", "/api/v1/sandboxes", err(503, "no_ready_machine", "no machine is ready"));
  await assert.rejects(mk(srv).create({ failFast: true }), (e) => e.code === "no_ready_machine");
  assert.equal(srv.requests.length, 1);
}));

test("r3 repeated labels", async () => {
 let url;
 const c = new CrowdVM({fetch: async (u) => {url = new URL(u); return Response.json({sandboxes: []});}});
 await c.sandboxes({label: ["a=1", "b=2"]});
 assert.deepEqual(url.searchParams.getAll("label"), ["a=1", "b=2"]);
});

test("r3 stream response headers time out", async () => {
 const c = new CrowdVM({timeoutMs: 20, maxRetries: 0, fetch: async (_url, init) => new Promise((_resolve,reject) => {
 init.signal.addEventListener("abort", () => reject(new Error("aborted")), {once:true});
 })});
 await assert.rejects(async () => {for await (const _ of c.command({sandboxId:"bx_a",command:"sleep 10",stream:true})) {}}, APIConnectionError);
});
