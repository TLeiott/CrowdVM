"""A tiny scriptable HTTP server (stdlib) standing in for crowdvm-server."""
from __future__ import annotations

import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, List, Optional, Tuple
from urllib.parse import parse_qs, urlsplit


class Req:
    def __init__(self, method: str, path: str, query: dict, headers: dict, raw: bytes):
        self.method, self.path, self.query, self.headers, self.raw = method, path, query, headers, raw

    @property
    def json(self) -> Any:
        return json.loads(self.raw) if self.raw else None


class Resp:
    def __init__(self, status: int = 200, body: Any = None, headers: Optional[dict] = None, raw: Optional[bytes] = None,
                 lines: Optional[List[Any]] = None):
        self.status, self.body, self.headers, self.raw, self.lines = status, body, headers or {}, raw, lines


def ok(body: Any = None, status: int = 200, **kw: Any) -> Resp:
    return Resp(status, {"ok": True, "type": "test", **(body or {})}, **kw)


def err(status: int, code: str, message: str = "boom", details: Optional[dict] = None, **extra: Any) -> Resp:
    body = {"ok": False, "type": "sandbox.error", "status": status, "code": code, "message": message,
            "error": {"code": code, "message": message, "status": status, "details": details or {}},
            "requestId": "req_test", **extra}
    return Resp(status, body)


class FakeServer:
    def __init__(self) -> None:
        self.requests: List[Req] = []
        self.routes: List[Tuple[str, "re.Pattern[str]", Callable[[Req], Resp]]] = []
        outer = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a: Any) -> None:
                pass

            def _serve(self) -> None:
                u = urlsplit(self.path)
                n = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(n) if n else b""
                req = Req(self.command, u.path, {k: v[0] for k, v in parse_qs(u.query).items()},
                          {k.lower(): v for k, v in self.headers.items()}, raw)
                outer.requests.append(req)
                for method, rx, fn in outer.routes:
                    if method == req.method and rx.fullmatch(u.path):
                        resp = fn(req)
                        break
                else:
                    resp = err(404, "not_found", f"no route {req.method} {u.path}")
                self.send_response(resp.status)
                if resp.lines is not None:
                    self.send_header("Content-Type", "application/x-ndjson")
                    body = b"".join((json.dumps(x) + "\n").encode() for x in resp.lines)
                elif resp.raw is not None:
                    self.send_header("Content-Type", "application/octet-stream")
                    body = resp.raw
                elif resp.body is not None:
                    self.send_header("Content-Type", "application/json")
                    body = json.dumps(resp.body).encode()
                else:
                    body = b""
                for k, v in resp.headers.items():
                    self.send_header(k, v)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = _serve

        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.thread = threading.Thread(target=self.httpd.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)

    def route(self, method: str, pattern: str, fn: Callable[[Req], Resp]) -> None:
        self.routes.append((method, re.compile(pattern), fn))

    def seq(self, method: str, pattern: str, *responses: Resp) -> None:
        """Answer successive calls with `responses` (the last one repeats)."""
        items = list(responses)

        def fn(_: Req) -> Resp:
            return items.pop(0) if len(items) > 1 else items[0]
        self.route(method, pattern, fn)

    @property
    def base(self) -> str:
        return f"http://127.0.0.1:{self.httpd.server_address[1]}/api/v1"

    def __enter__(self) -> "FakeServer":
        self.thread.start()
        return self

    def __exit__(self, *exc: Any) -> None:
        self.httpd.shutdown()
        self.httpd.server_close()
