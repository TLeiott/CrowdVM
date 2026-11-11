// A tiny scriptable HTTP server (node:http) standing in for crowdvm-server. Later routes win over earlier ones.
import { createServer } from "node:http";

export const ok = (body = {}, status = 200, extra = {}) => ({ status, body: { ok: true, type: "test", ...body }, ...extra });
export const err = (status, code, message = "boom", details = {}, extra = {}) => ({
  status,
  body: { ok: false, type: "sandbox.error", status, code, message, error: { code, message, status, details }, requestId: "req_test", ...extra },
});
export const lines = (arr) => ({ status: 200, lines: arr });

export async function fake() {
  const requests = [];
  const routes = [];
  const server = createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const raw = Buffer.concat(chunks).toString();
    const u = new URL(req.url, "http://x");
    const r = { method: req.method, path: u.pathname, query: Object.fromEntries(u.searchParams), headers: req.headers, raw, json: raw ? JSON.parse(raw) : undefined };
    requests.push(r);
    const route = routes.find((x) => x.method === r.method && x.re.test(r.path));
    const resp = route ? route.fn(r) : err(404, "not_found", `no route ${r.method} ${r.path}`);
    if (resp.hang) {
      res.writeHead(200, { "content-type": "application/x-ndjson" });
      res.write('{"type":"attached","stdoutOffset":0,"stderrOffset":0}\n');
      return; // never ends; closeAllConnections() in close() cleans up
    }
    if (resp.lines) {
      res.writeHead(resp.status, { "content-type": "application/x-ndjson" });
      res.end(resp.lines.map((l) => JSON.stringify(l) + "\n").join(""));
    } else if (resp.raw !== undefined) {
      res.writeHead(resp.status, { "content-type": "application/octet-stream" });
      res.end(Buffer.from(resp.raw));
    } else if (resp.body !== undefined) {
      res.writeHead(resp.status, { "content-type": "application/json", ...(resp.headers ?? {}) });
      res.end(JSON.stringify(resp.body));
    } else {
      res.writeHead(resp.status, resp.headers ?? {});
      res.end();
    }
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${server.address().port}/api/v1`;
  return {
    base, requests,
    route(method, pattern, fn) { routes.unshift({ method, re: new RegExp(`^${pattern}$`), fn: typeof fn === "function" ? fn : () => fn }); },
    /** answer successive calls with `responses` (the last repeats) */
    seq(method, pattern, ...responses) { const items = [...responses]; this.route(method, pattern, () => (items.length > 1 ? items.shift() : items[0])); },
    close() { server.closeAllConnections?.(); return new Promise((r) => server.close(r)); },
  };
}
