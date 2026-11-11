import assert from "node:assert/strict";
import { test } from "node:test";
import {
  CrowdVM, PromptFailed, SandboxFailed, WaitTimeout, execCommand, followCommand, paginate, readText, runCommand, stopAndRemove,
  streamCommand, streamEvents, streamPrompt, waitForDesktop, waitForPrompt, waitForPromptDone, waitUntilIdle, waitUntilReady, writeText,
} from "../dist/index.js";
import { err, fake, lines, ok } from "./fake.mjs";

const sb = (state, extra = {}) => ok({ sandbox: { id: "bx_x", name: "n", state, ...extra } });
const mk = (srv, o = {}) => new CrowdVM({ apiKey: "k", baseUrl: srv.base, maxRetryDelayMs: 5, ...o });
const FAST = { intervalMs: 1 };
async function withServer(fn) { const srv = await fake(); try { await fn(srv); } finally { await srv.close(); } }

test("waitUntilReady / Idle / failure / timeout", () => withServer(async (srv) => {
  srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("provisioning"), sb("cloning"), sb("ready"));
  assert.equal((await waitUntilReady(mk(srv), "bx_x", FAST)).state, "ready");
  assert.equal(srv.requests.length, 3);
  const s2 = await fake();
  try {
    s2.seq("GET", "/api/v1/sandboxes/bx_x", sb("running"), sb("idle"));
    assert.equal((await waitUntilIdle(mk(s2), "bx_x", FAST)).state, "idle");
    const s3 = await fake();
    try {
      s3.seq("GET", "/api/v1/sandboxes/bx_x", sb("provisioning"), sb("error", { error: "no capacity" }));
      await assert.rejects(() => waitUntilReady(mk(s3), "bx_x", FAST), SandboxFailed);
      const s4 = await fake();
      try {
        s4.route("GET", "/api/v1/sandboxes/bx_x", sb("provisioning"));
        await assert.rejects(() => waitUntilReady(mk(s4), "bx_x", { intervalMs: 5, timeoutMs: 8 }), WaitTimeout);
      } finally { await s4.close(); }
    } finally { await s3.close(); }
  } finally { await s2.close(); }
}));

test("waitUntilReady waits out node_offline", () => withServer(async (srv) => {
  srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("provisioning"), sb("error", { error: "node_offline" }),
    sb("error", { error: "node_offline: node n1 disconnected" }), sb("ready"));
  assert.equal((await waitUntilReady(mk(srv), "bx_x", FAST)).state, "ready");
  assert.equal(srv.requests.length, 4);
  const s2 = await fake();
  try {
    s2.seq("GET", "/api/v1/sandboxes/bx_x", sb("error", { error: "node_offline" }), sb("idle"));
    assert.equal((await waitUntilIdle(mk(s2), "bx_x", FAST)).state, "idle");
  } finally { await s2.close(); }
}));

test("waitUntilReady: node_offline fails at the timeout, other errors at once", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/sandboxes/bx_x", sb("error", { error: "node_offline" }));
  await assert.rejects(() => waitUntilReady(mk(srv), "bx_x", { intervalMs: 2, timeoutMs: 30 }),
    (e) => e instanceof SandboxFailed && /node_offline/.test(e.message));
  assert.ok(srv.requests.length > 1);
  const s2 = await fake();
  try {
    s2.seq("GET", "/api/v1/sandboxes/bx_x", sb("error", { error: "boot_failed" }), sb("ready"));
    await assert.rejects(() => waitUntilReady(mk(s2), "bx_x", FAST), SandboxFailed);
    assert.equal(s2.requests.length, 1);
  } finally { await s2.close(); }
}));

test("waitForDesktop polls until the URL is ready", () => withServer(async (srv) => {
  srv.seq("POST", "/api/v1/sandboxes/bx_x/desktop", err(503, "browser_not_ready", "x", { retryAfterSeconds: 0 }),
    ok({ provisioning: true, desktopUrl: null }), ok({ desktopUrl: "http://d", provisioning: false }));
  const out = await waitForDesktop(mk(srv, { maxRetries: 0 }), "bx_x", FAST);
  assert.equal(out.desktopUrl, "http://d");
  assert.deepEqual(srv.requests[0].query, { vnc: "1" });
}));

test("waitForPrompt / waitForPromptDone", () => withServer(async (srv) => {
  const run = (status, done) => ok({ promptRun: { promptId: "p1", status, done } });
  srv.seq("GET", "/api/v1/sandboxes/bx_x/prompts/p1", run("queued", false), run("running", false), run("finished", true));
  assert.equal((await waitForPrompt(mk(srv), "bx_x", "p1", FAST)).status, "finished");
  const s2 = await fake();
  try {
    s2.route("GET", "/api/v1/sandboxes/bx_x/prompts/p1", run("failed", true));
    assert.equal((await waitForPrompt(mk(s2), "bx_x", "p1", FAST)).status, "failed");
    await assert.rejects(() => waitForPromptDone(mk(s2), "bx_x", "p1", FAST), PromptFailed);
  } finally { await s2.close(); }
}));

test("execCommand resends while the sandbox restores its disk", () => withServer(async (srv) => {
  const restoring = err(502, "sandbox_direct_failed", "sandbox_restoring", { retryable: true });
  srv.seq("POST", "/api/v1/sandboxes/bx_x/commands", ...Array(6).fill(restoring), ok({ exitCode: 0, stdout: "42\n" }));
  const out = await execCommand(mk(srv), "bx_x", "python3 -c 'print(6*7)'", "r", 60);
  assert.equal(out.stdout, "42\n");
  assert.deepEqual(srv.requests.at(-1).json, { command: "python3 -c 'print(6*7)'", cwd: "r", timeoutSeconds: 60 });
  const o2 = await execCommand(mk(srv), "bx_x", "ls", { cwd: "q" });
  assert.equal(o2.exitCode, 0);
  assert.equal(srv.requests.at(-1).json.cwd, "q");
}));

test("streamCommand", () => withServer(async (srv) => {
  srv.route("POST", "/api/v1/sandboxes/bx_x/commands", lines([{ type: "started", commandId: "c" }, { type: "stdout", data: "x" }, { type: "exit", exitCode: 0 }]));
  const types = [];
  for await (const f of streamCommand(mk(srv), "bx_x", "echo x", { timeoutSeconds: 5 })) types.push(f.type);
  assert.deepEqual(types, ["started", "stdout", "exit"]);
  assert.deepEqual(srv.requests[0].json, { command: "echo x", timeoutSeconds: 5, stream: true });
}));

test("runCommand follows and reconnects without gap or repeat", () => withServer(async (srv) => {
  srv.route("POST", "/api/v1/sandboxes/bx_x/commands", ok({ processId: 5, commandId: "c9" }));
  srv.seq("GET", "/api/v1/sandboxes/bx_x/commands/5/stream",
    lines([{ type: "attached", processId: 5, stdoutOffset: 0, stderrOffset: 0 }, { type: "stdout", data: "one ", nextOffset: 4 }, { type: "heartbeat" }, { type: "stderr", data: "warn", nextOffset: 4 }]),
    lines([{ type: "attached", stdoutOffset: 4, stderrOffset: 4 }, { type: "stdout", data: "two ", nextOffset: 8 }, { type: "reconnect", stdoutOffset: 8, stderrOffset: 4 }]),
    lines([{ type: "attached", stdoutOffset: 8, stderrOffset: 4 }, { type: "stdout", data: "three", nextOffset: 13 }, { type: "exit", status: "exited", exitCode: 7 }]));
  const frames = [];
  for await (const f of runCommand(mk(srv), "bx_x", "long job", { cwd: "repo" })) frames.push(f);
  assert.equal(frames[0].type, "started");
  assert.equal(frames[0].processId, 5);
  assert.equal(frames.filter((f) => f.type === "stdout").map((f) => f.data).join(""), "one two three");
  assert.deepEqual([frames.at(-1).type, frames.at(-1).exitCode], ["exit", 7]);
  assert.deepEqual(srv.requests[0].json, { command: "long job", cwd: "repo", detached: true });
  assert.deepEqual(srv.requests.slice(1).map((r) => r.query), [
    { stdoutOffset: "0", stderrOffset: "0" }, { stdoutOffset: "4", stderrOffset: "4" }, { stdoutOffset: "8", stderrOffset: "4" }]);
}));

test("followCommand yields an error frame and stops", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/sandboxes/bx_x/commands/2/stream", lines([{ type: "error", error: "lost", message: "gone" }]));
  const frames = [];
  for await (const f of followCommand(mk(srv), "bx_x", 2)) frames.push(f);
  assert.equal(frames.at(-1).type, "error");
}));

test("readText / writeText", () => withServer(async (srv) => {
  srv.route("PUT", "/api/v1/sandboxes/bx_x/files", ok({ success: true }));
  srv.route("GET", "/api/v1/sandboxes/bx_x/files", ok({ content: "done\n", encoding: "utf8" }));
  const c = mk(srv);
  await writeText(c, "bx_x", "notes/result.txt", "done\n");
  assert.equal(await readText(c, "bx_x", "notes/result.txt"), "done\n");
  assert.deepEqual(srv.requests[1].query, { path: "notes/result.txt", encoding: "utf8" });
}));

test("streamEvents yields only new events once, ends on abort", () => withServer(async (srv) => {
  const ev = (i, ts) => ({ id: `e${i}`, type: "response", timestamp: ts });
  const pages = [[ev(2, 2), ev(1, 1)], [ev(2, 2), ev(1, 1)], [ev(4, 4), ev(3, 3), ev(2, 2)], [ev(4, 4), ev(3, 3)]];
  let n = 0;
  srv.route("GET", "/api/v1/sandboxes/bx_x/events", () => ok({ events: pages[Math.min(n++, pages.length - 1)], pageInfo: { hasMore: false, nextCursor: null, limit: 200 } }));
  const ctl = new AbortController();
  const got = [];
  for await (const e of streamEvents(mk(srv), "bx_x", { type: "response", intervalMs: 1, signal: ctl.signal })) {
    got.push(e.id);
    if (got.length === 2) ctl.abort();
  }
  assert.deepEqual(got, ["e3", "e4"]);
  assert.equal(srv.requests[0].query.type, "response");
}));

test("streamPrompt", () => withServer(async (srv) => {
  const ev = (i) => ({ id: `e${i}`, type: "response", timestamp: i, data: { content: `c${i}` } });
  const pages = [[], [ev(1)], [ev(2), ev(1)]];
  let n = 0;
  srv.route("GET", "/api/v1/sandboxes/bx_x/events", () => ok({ events: pages[Math.min(n++, 2)], pageInfo: { hasMore: false, limit: 200 } }));
  srv.route("POST", "/api/v1/sandboxes/bx_x/prompt", ok({ promptId: "p1", conversationId: "conv1" }, 202));
  const done = [false, false, true];
  let d = 0;
  srv.route("GET", "/api/v1/sandboxes/bx_x/prompts/p1", () => ok({ promptRun: { promptId: "p1", status: "running", done: done[Math.min(d++, 2)] } }));
  const out = [];
  for await (const e of streamPrompt(mk(srv), "bx_x", { prompt: "hi", provider: "codex", intervalMs: 1 })) out.push(e.data.content);
  assert.deepEqual(out, ["c1", "c2"]);
  assert.deepEqual(srv.requests.find((r) => r.method === "POST").json, { prompt: "hi", provider: "codex" });
  const evs = srv.requests.filter((r) => r.path.endsWith("/events"));
  assert.equal(evs.at(-1).query.conversation, "conv1");
  assert.equal(evs[0].query.conversation, undefined);
}));

test("paginate follows cursors", () => withServer(async (srv) => {
  srv.route("GET", "/api/v1/sandboxes", (r) => r.query.cursor === "c2"
    ? ok({ sandboxes: [{ id: "bx_3" }], pageInfo: { hasMore: false, nextCursor: null, limit: 2 } })
    : ok({ sandboxes: [{ id: "bx_1" }, { id: "bx_2" }], pageInfo: { hasMore: true, nextCursor: "c2", limit: 2 } }));
  const c = mk(srv);
  const ids = [];
  for await (const s of paginate((p) => c.sandboxes(p), "sandboxes", { limit: 2, state: "running" })) ids.push(s.id);
  assert.deepEqual(ids, ["bx_1", "bx_2", "bx_3"]);
  assert.deepEqual(srv.requests[1].query, { limit: "2", state: "running", cursor: "c2" });
}));

test("stopAndRemove", () => withServer(async (srv) => {
  srv.route("POST", "/api/v1/sandboxes/bx_x/stop", ok({ id: "bx_x", status: "archiving" }, 202));
  srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("archiving"), sb("archived"));
  srv.route("DELETE", "/api/v1/sandboxes/bx_x", ok({ operation: { id: "op" } }, 202));
  const out = await stopAndRemove(mk(srv), "bx_x", FAST);
  assert.equal(out.operation.id, "op");
  assert.deepEqual(srv.requests.map((r) => r.method), ["POST", "GET", "GET", "DELETE"]);
  assert.equal(srv.requests.at(-1).headers["x-crowdvm-confirm-delete"], "bx_x");
}));
