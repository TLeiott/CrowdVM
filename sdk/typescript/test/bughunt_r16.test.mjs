// Round 16 bug-hunt tests: each one fails because of a real SDK bug.
import assert from "node:assert/strict";
import net from "node:net";
import { test } from "node:test";
import { ApiError, CrowdVM, WaitTimeout, waitForDesktop } from "../dist/index.js";
import { err, fake } from "./fake.mjs";

const mk = (srv, o = {}) => new CrowdVM({ apiKey: "k", baseUrl: srv.base, maxRetries: 0, ...o });

// waitForDesktop polls through every 400, 409 and 503 answer. A 400 is the
// caller's mistake (`invalid_theme`, `invalid_...`): it can never become a
// desktop, so the helper keeps asking until its timeout and then throws a
// WaitTimeout that hides the server's reason.
test("bughunt r16: waitForDesktop fails at once on a 400 (invalid_theme), not after the timeout", async () => {
  const srv = await fake();
  try {
    srv.route("POST", "/api/v1/sandboxes/bx_x/desktop", err(400, "invalid_theme", "theme must be light or dark."));
    await assert.rejects(
      () => waitForDesktop(mk(srv), "bx_x", { theme: "neon", intervalMs: 5, timeoutMs: 300 }),
      (e) => e instanceof ApiError && e.code === "invalid_theme" && !(e instanceof WaitTimeout),
    );
  } finally { await srv.close(); }
});

// The request timeout is one deadline for the whole request, body included
// (the timer is cleared only when `stream` is true or in `cleanup()` after
// the body was read). A file download (`artifact`, `getSnapshotFile`: up to
// tens of MiB, or a snapshot tar) that keeps delivering data is cut at
// timeoutMs (default 60 s). The Python SDK's timeout is per socket read and
// does not cut a download that makes progress.
test("bughunt r16: a bytes download that keeps making progress is not cut by the request timeout", async () => {
  const socks = new Set();
  const server = net.createServer((s) => {
    socks.add(s);
    s.on("error", () => {});
    s.once("data", async () => {
      s.write("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n");
      for (let i = 0; i < 8; i++) { s.write("4\r\nabcd\r\n"); await new Promise((r) => setTimeout(r, 100)); }
      s.end("0\r\n\r\n");
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  try {
    const cvm = new CrowdVM({ apiKey: "k", baseUrl: `http://127.0.0.1:${server.address().port}/api/v1`, maxRetries: 0, timeoutMs: 400 });
    const blob = await cvm.artifact({ sandboxId: "bx_x", path: "big.bin" }); // 8 chunks over ~800 ms, a chunk every 100 ms
    assert.equal(blob.size, 32);
  } finally { for (const s of socks) s.destroy(); await new Promise((r) => server.close(r)); }
});
