import assert from "node:assert/strict";
import { test } from "node:test";
import { asciiBox, crowdvmBox, EveSandboxUnsupportedError, resolvePath } from "../dist/index.js";
import { err, fake, lines, ok } from "./fake.mjs";

const SB = { id: "bx_eve00001", state: "ready" };
const S = "/api/v1/sandboxes/bx_eve00001";
async function withServer(fn) { const srv = await fake(); try { await fn(srv); } finally { await srv.close(); } }
function backend(srv, o = {}) {
  srv.route("POST", "/api/v1/sandboxes", ok({ sandbox: SB }, 202));
  srv.route("GET", S, ok({ sandbox: SB }));
  srv.route("POST", `${S}/commands`, ok({ exitCode: 0, stdout: "", stderr: "" }));
  return crowdvmBox({ apiKey: "cvm_k", baseUrl: srv.base, ...o });
}

test("resolvePath is rooted at /workspace; asciiBox is an alias", () => {
  assert.equal(resolvePath("a/b.txt"), "/workspace/a/b.txt");
  assert.equal(resolvePath("/etc/x"), "/etc/x");
  assert.equal(resolvePath("a/../../b"), "/b");
  assert.equal(asciiBox, crowdvmBox);
});

test("create: name, ttl, noEnv, env, labels, template; workspace prepared", () => withServer(async (srv) => {
  const b = backend(srv, { ttlSeconds: 900, noEnv: true, env: { TOKEN: "t" } });
  const h = await b.create({ templateKey: "my-stack", sessionKey: "s1", runtimeContext: {} });
  const r = srv.requests[0];
  assert.deepEqual({ ...r.json }, { name: "eve-s1", ttlSeconds: 900, noEnv: true, env: { TOKEN: "t" }, from: "my-stack", labels: { "eve.session": "s1" } });
  assert.ok(r.headers["idempotency-key"]);
  assert.match(srv.requests.at(-1).json.command, /mkdir -p ~\/workspace/);
  assert.deepEqual(h.state, { sandboxId: "bx_eve00001" });
  // name function, null ttl
  const b2 = backend(srv, { name: (i) => `n-${i.sessionKey}`, ttlSeconds: null });
  await b2.create({ sessionKey: "k" });
  const second = srv.requests.filter((x) => x.path === "/api/v1/sandboxes" && x.method === "POST")[1];
  assert.equal(second.json.name, "n-k");
  assert.equal(second.json.ttlSeconds, null);
}));

test("network policies other than allow-all are refused", () => withServer(async (srv) => {
  assert.throws(() => crowdvmBox({ apiKey: "k", baseUrl: srv.base, networkPolicy: "deny-all" }), EveSandboxUnsupportedError);
  const h = await backend(srv).create({ sessionKey: "s" });
  await h.useSessionFn({ networkPolicy: "allow-all" });
  await assert.rejects(h.useSessionFn({ networkPolicy: { allow: ["example.com"] } }), EveSandboxUnsupportedError);
}));

test("run: cwd defaults to the workspace, env exported, timeout; text files", () => withServer(async (srv) => {
  const h = await backend(srv).create({ sessionKey: "s" });
  const s = await h.useSessionFn();
  srv.requests.length = 0;
  const r = await s.run({ command: "ls", env: { A: "x y" }, timeoutMs: 5000 });
  assert.equal(r.exitCode, 0);
  const body = srv.requests[0].json;
  assert.equal(body.cwd, "workspace");
  assert.equal(body.timeoutSeconds, 5);
  assert.equal(body.command, "export A='x y'; ls");
  await s.run({ command: "pwd", cwd: "/etc" });
  assert.equal(srv.requests[1].json.command, "cd '/etc' && pwd");
  srv.route("PUT", `${S}/files`, ok({ success: true }));
  srv.route("GET", `${S}/files`, ok({ content: "hi", encoding: "utf8", path: "x" }));
  await s.writeTextFile({ path: "dir/f.txt", content: "hi" });
  const put = srv.requests.at(-1);
  assert.deepEqual(put.json, { path: "workspace/dir/f.txt", content: "hi", encoding: "utf8" });
  assert.equal(await s.readTextFile({ path: "/workspace/dir/f.txt" }), "hi");
  assert.equal(srv.requests.at(-1).query.path, "workspace/dir/f.txt");
}));

test("spawn streams stdout/stderr and reports the exit code; kill stops the process", () => withServer(async (srv) => {
  const h = await backend(srv).create({ sessionKey: "s" });
  const s = await h.useSessionFn();
  srv.route("POST", `${S}/commands`, ok({ processId: 9, commandId: "cmd_1" }, 202));
  srv.route("GET", `${S}/commands/9/stream`, lines([
    { type: "attached", stdoutOffset: 0, stderrOffset: 0 },
    { type: "stdout", data: "a", nextOffset: 1 }, { type: "stderr", data: "b", nextOffset: 1 },
    { type: "stdout", data: "c", nextOffset: 2 }, { type: "exit", exitCode: 3 }]));
  srv.route("DELETE", `${S}/commands/9`, ok({}));
  const p = await s.spawn({ command: "job", env: { X: "1" } });
  assert.equal(p.pid, 9);
  assert.equal(srv.requests.find((r) => r.json?.detached).json.command, "export X='1'; job");
  let out = "", err = "";
  await Promise.all([(async () => { for await (const c of p.stdout) out += c; })(), (async () => { for await (const c of p.stderr) err += c; })()]);
  assert.deepEqual([out, err, (await p.wait()).exitCode], ["ac", "b", 3]);
  await p.kill();
  assert.ok(srv.requests.some((r) => r.method === "DELETE" && r.path === `${S}/commands/9`));
}));

test("removePath refuses the workspace root; resume restarts a stopped sandbox", () => withServer(async (srv) => {
  const b = backend(srv);
  const h = await b.create({ sessionKey: "s" });
  const s = await h.useSessionFn();
  await assert.rejects(s.removePath({ path: "/workspace" }), /refusing/);
  await s.removePath({ path: "old" });
  assert.match(srv.requests.at(-1).json.command, /^rm -rf -- 'workspace\/old'$/);
  let state = "stopped";
  srv.route("GET", S, () => ok({ sandbox: { ...SB, state } }));
  srv.route("POST", `${S}/resume`, () => { state = "ready"; return ok({ sandbox: SB }, 202); });
  const h2 = await b.resume({ sandboxId: "bx_eve00001" });
  assert.equal(h2.sandboxId, "bx_eve00001");
  assert.ok(srv.requests.some((r) => r.path === `${S}/resume`));
}));
