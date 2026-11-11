// Live smoke test of the TypeScript SDK against a real server.
//   CROWDVM_API_BASE=http://127.0.0.1:8480/api/v1 CROWDVM_API_KEY=cvm_... node sdk/smoke/smoke.mjs   (after `npm run build` in sdk/typescript)
import assert from "node:assert/strict";
import { CrowdVM, ApiError, execCommand, readText, runCommand, streamCommand, waitUntilReady, writeText } from "../typescript/dist/index.js";

const cvm = new CrowdVM();
const made = [];
const step = (s) => console.log("--", s);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const textOf = (frames) => frames.filter((f) => f.type === "stdout").map((f) => String(f.data)).join("");
async function collect(it) { const out = []; for await (const f of it) out.push(f); return out; }

try {
  step("create");
  const { sandbox: sb } = await cvm.create({ ttlSeconds: 900, noEnv: true, labels: { smoke: "ts" } });
  made.push(sb.id);
  await waitUntilReady(cvm, sb.id, { timeoutMs: 180_000 });

  step("command (sync)");
  const out = await cvm.command({ sandboxId: sb.id, command: "echo hi && echo err >&2; exit 3" });
  assert.deepEqual([out.exitCode, out.stdout.trim(), out.stderr.trim()], [3, "hi", "err"]);
  assert.match((await execCommand(cvm, sb.id, "pwd", "/tmp")).stdout, /\/tmp\s*$/);

  step("command (stream)");
  const frames = await collect(streamCommand(cvm, sb.id, "echo one; sleep 0.2; echo two"));
  assert.match(textOf(frames), /one[\s\S]*two/);
  assert.equal(frames.at(-1).type, "exit");
  assert.equal(frames.at(-1).exitCode, 0);

  step("command (detached + attach / runCommand)");
  const started = await cvm.command({ sandboxId: sb.id, command: "for i in 1 2 3; do echo tick$i; sleep 0.3; done", detached: true, timeoutSeconds: 60 });
  const attached = await collect(cvm.attachCommand({ sandboxId: sb.id, processId: started.processId }));
  assert.match(textOf(attached), /tick3/);
  const st = await cvm.commandStatus({ sandboxId: sb.id, processId: started.processId });
  assert.equal(st.running, false);
  assert.equal(st.exitCode, 0);
  const run = await collect(runCommand(cvm, sb.id, "echo rc-ok; exit 4"));
  assert.ok(run.some((f) => f.type === "exit" && f.exitCode === 4), JSON.stringify(run));

  step("files");
  await writeText(cvm, sb.id, "/home/user/smoke/a.txt", "héllo\n");
  assert.equal(await readText(cvm, sb.id, "/home/user/smoke/a.txt"), "héllo\n");

  step("stop");
  const r = await cvm.stop({ sandboxId: sb.id });
  const stop = r.stop ?? r;
  console.log("   stop:", stop.id, stop.status);
  for (let i = 0; i < 60 && !["stopped", "archived"].includes((await cvm.get({ sandboxId: sb.id })).sandbox.state); i++) await sleep(1000);
  assert.ok(["stopped", "archived"].includes((await cvm.get({ sandboxId: sb.id })).sandbox.state));

  step("resume");
  await cvm.resume({ sandboxId: sb.id });
  await waitUntilReady(cvm, sb.id, { timeoutMs: 180_000 });
  assert.equal(await readText(cvm, sb.id, "/home/user/smoke/a.txt"), "héllo\n");

  step("fork");
  const { sandbox: fk } = await cvm.fork({ sandboxId: sb.id, labels: { smoke: "ts-fork" } });
  made.push(fk.id);
  await waitUntilReady(cvm, fk.id, { timeoutMs: 180_000 });
  assert.equal(await readText(cvm, fk.id, "/home/user/smoke/a.txt"), "héllo\n");

  step("list by label");
  const ls = await cvm.sandboxes({ label: "smoke=ts" });
  assert.ok(ls.sandboxes.some((s) => s.id === sb.id));

  step("delete");
  for (const id of [...made]) { await cvm.deleteSandbox({ sandboxId: id, xAsciiConfirmDelete: id }); made.splice(made.indexOf(id), 1); }
  await assert.rejects(cvm.get({ sandboxId: sb.id }), (e) => e instanceof ApiError && e.status === 404);
  console.log("TYPESCRIPT SMOKE OK");
} catch (e) {
  console.error("TYPESCRIPT SMOKE FAILED"); console.error(e); process.exitCode = 1;
} finally {
  for (const id of made) await cvm.deleteSandbox({ sandboxId: id, xAsciiConfirmDelete: id }).catch((e) => console.error("cleanup failed", id, e.message));
}
