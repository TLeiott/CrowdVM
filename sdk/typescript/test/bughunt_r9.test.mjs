// Round 9 bug-hunt tests: each one fails because of a real SDK bug.
import assert from "node:assert/strict";
import net from "node:net";
import { test } from "node:test";
import { APIConnectionError, CrowdVM } from "../dist/index.js";
import { fake, ok } from "./fake.mjs";

/** Answers every connection with raw bytes (one write per chunk, `gap` ms apart), then closes. */
async function raw(chunks, gap = 0) {
  const socks = new Set();
  const server = net.createServer((s) => {
    socks.add(s);
    s.on("error", () => {});
    let buf = Buffer.alloc(0);
    let sent = false;
    s.on("data", async (d) => {
      buf = Buffer.concat([buf, d]);
      if (sent || !buf.includes("\r\n\r\n")) return;
      sent = true;
      for (const c of chunks) { s.write(c); await new Promise((r) => setTimeout(r, gap)); }
      s.end();
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  return {
    base: `http://127.0.0.1:${server.address().port}/api/v1`,
    close() { for (const s of socks) s.destroy(); return new Promise((r) => server.close(r)); },
  };
}
const chunked = (parts) => ["HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n",
  ...parts.map((p) => `${Buffer.byteLength(p).toString(16)}\r\n${p}\r\n`), "0\r\n\r\n"];

test("a command stream that ends without an exit frame is an error, as in Python", async () => {
  const srv = await raw(chunked(['{"type":"started"}\n', '{"type":"stdout","data":"partial"}\n']));
  try {
    const cvm = new CrowdVM({ baseUrl: srv.base, maxRetries: 0 });
    const seen = [];
    await assert.rejects(async () => { for await (const f of cvm.command({ sandboxId: "bx_a", command: "x", stream: true })) seen.push(f.type); }, APIConnectionError);
  } finally { await srv.close(); }
});

test("a truncated body of a bytes operation is an APIConnectionError", async () => {
  const srv = await raw(["HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 100\r\n\r\nabc"]);
  try {
    const cvm = new CrowdVM({ baseUrl: srv.base, maxRetries: 0 });
    await assert.rejects(() => cvm.artifact({ sandboxId: "bx_a", path: "x" }), APIConnectionError);
  } finally { await srv.close(); }
});

test("path parameters that are dot segments or empty are refused before sending", async () => {
  const srv = await fake();
  try {
    srv.route("GET", "/api/v1/.*", ok({}));
    const cvm = new CrowdVM({ baseUrl: srv.base, maxRetries: 0 });
    for (const bad of ["..", ".", ""]) {
      await assert.rejects(() => cvm.get({ sandboxId: bad }), /invalid path parameter/);
      await assert.rejects(() => cvm.command({ sandboxId: bad, command: "x" }), /invalid path parameter/);
    }
    assert.equal(srv.requests.length, 0);
    await cvm.get({ sandboxId: "a/b c" });
    assert.equal(srv.requests[0].path, "/api/v1/sandboxes/a%2Fb%20c");
  } finally { await srv.close(); }
});

test("a per-call header replaces a client default header of another case", async () => {
  const srv = await fake();
  try {
    srv.route("POST", "/api/v1/sandboxes", ok({ sandbox: { id: "bx_a" } }, 202));
    const cvm = new CrowdVM({ baseUrl: srv.base, maxRetries: 0, headers: { "x-boat-org": "org_default" } });
    await cvm.create({ xBoatOrg: "org_other" });
    assert.equal(srv.requests[0].headers["x-boat-org"], "org_other");
  } finally { await srv.close(); }
});
