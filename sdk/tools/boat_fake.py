#!/usr/bin/env python3
"""A small stateful stand-in for crowdvm-server (stdlib only).

It answers the endpoints that Boat's SDK doc samples use, with CrowdVM's response shapes, so
`doc_samples.py` can run every sample without VMs.  Sandboxes are ready on the second GET,
commands "run" by echoing a canned output, prompts finish at once.

    python3 sdk/tools/boat_fake.py --port 9510      # prints the base URL, serves until killed
"""
from __future__ import annotations

import argparse
import itertools
import json
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, List, Optional, Tuple
from urllib.parse import parse_qs, urlsplit

KEY = "cvm_fake"
_ALPHA = "23456789abcdefghjkmnpqrstuvwxyz"


class _State:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.seq = itertools.count(1)
        self.sandboxes: Dict[str, Dict[str, Any]] = {}
        self.files: Dict[Tuple[str, str], bytes] = {}
        self.saved: Dict[str, Dict[str, Any]] = {}
        self.events: Dict[str, List[Dict[str, Any]]] = {}
        self.prompts: Dict[str, Dict[str, Any]] = {}
        self.idem: Dict[str, str] = {}
        self.signed: Dict[str, Tuple[str, str]] = {}
        self.requests: List[Tuple[str, str, Any, Dict[str, str]]] = []

    def new_id(self, prefix: str, n: int = 8) -> str:
        i = next(self.seq)
        s = ""
        for _ in range(n):
            s += _ALPHA[i % len(_ALPHA)]
            i //= len(_ALPHA)
        return prefix + s[::-1]


def _sandbox(sid: str, body: Dict[str, Any]) -> Dict[str, Any]:
    typ = body.get("type") or "default"
    vcpu, mem = {"small": (2, 4), "default": (4, 8), "large": (8, 16), "xlarge": (16, 32)}.get(typ, (4, 8))
    return {"id": sid, "name": body.get("name") or "Sandbox", "state": "provisioning", "type": typ,
            "vcpu": vcpu, "memoryGB": mem, "diskGB": 60, "desktopAvailable": False, "snapshotAvailable": False,
            "ttlSeconds": body.get("ttlSeconds", 3600), "noEnv": bool(body.get("noEnv")), "env": body.get("env") or {},
            "createdAt": "2026-11-08T00:00:00Z", "_gets": 0}


def _public(sb: Dict[str, Any]) -> Dict[str, Any]:
    return {k: v for k, v in sb.items() if not k.startswith("_")}


def _ok(typ: str, **body: Any) -> Dict[str, Any]:
    return {"ok": True, "type": typ, **body}


def _err(status: int, code: str, message: str) -> Tuple[int, Dict[str, Any]]:
    return status, {"ok": False, "type": "error", "status": status, "code": code, "message": message,
                    "error": {"code": code, "message": message, "status": status, "details": {}},
                    "requestId": "req_fake"}


class FakeBoat:
    """``with FakeBoat() as fake: fake.base`` -> ``http://127.0.0.1:<port>/api/v1``."""

    def __init__(self, port: int = 0) -> None:
        self.state = _State()
        outer = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a: Any) -> None:
                pass

            def _serve(self) -> None:
                u = urlsplit(self.path)
                n = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(n) if n else b""
                q = {k: v[0] for k, v in parse_qs(u.query).items()}
                try:
                    status, payload, ctype = outer.handle(self.command, u.path, q, self.headers, raw)
                except Exception as e:  # a bug in the fake must be visible
                    status, payload, ctype = 500, {"ok": False, "code": "fake_crash", "message": repr(e)}, None
                if isinstance(payload, list):  # NDJSON frames
                    data = b"".join((json.dumps(x) + "\n").encode() for x in payload)
                    ctype = "application/x-ndjson"
                elif isinstance(payload, bytes):
                    data = payload
                    ctype = ctype or "application/octet-stream"
                else:
                    data = json.dumps(payload).encode()
                    ctype = "application/json"
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = _serve

        self.httpd = ThreadingHTTPServer(("127.0.0.1", port), H)
        self.httpd.daemon_threads = True
        self.root = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        self.base = self.root + "/api/v1"
        self._t = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def __enter__(self) -> "FakeBoat":
        self._t.start()
        return self

    def __exit__(self, *exc: Any) -> None:
        self.httpd.shutdown()
        self.httpd.server_close()

    # ------------------------------------------------------------------ routing
    def handle(self, method: str, path: str, q: Dict[str, str], headers: Any, raw: bytes):
        st = self.state
        body: Any = None
        if raw and path.startswith("/api/v1"):
            try:
                body = json.loads(raw)
            except ValueError:
                return (*_err(400, "invalid_json", "body is not JSON"), None)
        with st.lock:
            st.requests.append((method, path, body, dict(headers.items())))
        m = re.fullmatch(r"/signed/(\w+)", path)
        if m:
            sid, fpath = st.signed.get(m.group(1), (None, None))
            if sid is None:
                return (*_err(403, "invalid_signature", "bad token"), None)
            if method == "PUT":
                st.files[(sid, fpath)] = raw
                return 200, _ok("file.uploaded", path=fpath, size=len(raw)), None
            return 200, st.files.get((sid, fpath), b""), None
        if not path.startswith("/api/v1/"):
            return (*_err(404, "not_found", path), None)
        if headers.get("Authorization") != f"Bearer {KEY}":
            return (*_err(401, "unauthorized", "missing or bad API key"), None)
        path = path[len("/api/v1"):]
        with st.lock:
            res = self.route(method, path, q, headers, body if body is not None else {})
        return (*res, None) if len(res) == 2 else res

    def route(self, method: str, path: str, q: Dict[str, str], headers: Any, body: Any):
        st = self.state
        if (method, path) == ("POST", "/sandboxes"):
            key = headers.get("Idempotency-Key")
            if key and key in st.idem:
                sb = st.sandboxes[st.idem[key]]
                return 202, _ok("sandbox.created", status="provisioning", ttlSeconds=sb["ttlSeconds"], sandbox=_public(sb))
            if body.get("from") and body["from"] not in st.saved:
                return _err(404, "snapshot_not_found", f"no saved snapshot {body['from']}")
            sbs = []
            for _ in range(int(body.get("count") or 1)):
                sid = st.new_id("bx_")
                st.sandboxes[sid] = _sandbox(sid, body)
                sbs.append(_public(st.sandboxes[sid]))
            if key:
                st.idem[key] = sbs[0]["id"]
            extra = {"sandboxes": sbs} if len(sbs) > 1 else {}
            return 202, _ok("sandbox.created", status="provisioning", ttlSeconds=body.get("ttlSeconds", 3600),
                            sandbox=sbs[0], **extra)
        if (method, path) == ("GET", "/sandboxes"):
            return 200, _ok("sandbox.list", sandboxes=[_public(s) for s in st.sandboxes.values()],
                            pageInfo={"hasMore": False, "nextCursor": None})
        if (method, path) == ("GET", "/limits"):
            return 200, _ok("account.limits", machineTypes=[
                {"type": "small", "available": True}, {"type": "default", "available": True},
                {"type": "large", "available": True}, {"type": "xlarge", "available": False, "reason": "node_capacity"}])
        if (method, path) == ("POST", "/saved-snapshots"):
            if body.get("sandboxId") not in st.sandboxes and body.get("sandboxId") != "bx_23456789":
                return _err(404, "sandbox_not_found", "Sandbox not found.")
            st.saved[body["name"]] = {"name": body["name"], "status": "saving", "_gets": 0}
            return 202, _ok("saved_snapshot.saving", snapshot=_public(st.saved[body["name"]]))
        m = re.fullmatch(r"/saved-snapshots/([\w.-]+)", path)
        if m and method == "GET":
            snap = st.saved.get(m.group(1))
            if not snap:
                return _err(404, "not_found", "Saved snapshot not found.")
            snap["_gets"] += 1
            if snap["_gets"] > 1:
                snap["status"] = "ready"
            return 200, _ok("saved_snapshot", snapshot=_public(snap))
        m = re.fullmatch(r"/snapshots/(snap_\w+)/(tree|files)", path)
        if m and method == "GET":
            if m.group(2) == "tree":
                return 200, _ok("snapshot.tree", snapshotId=m.group(1), entries=[{"path": "projects/app/.env", "type": "file", "size": 4}])
            return 200, b"A=1\n", "application/octet-stream"
        m = re.fullmatch(r"/sandboxes/(bx_\w+)(/.*)?", path)
        if not m:
            return _err(404, "not_found", f"no route {method} {path}")
        sid, rest = m.group(1), m.group(2) or ""
        if sid == "bx_23456789" and sid not in st.sandboxes:  # the fixed id of Boat's samples
            st.sandboxes[sid] = dict(_sandbox(sid, {}), state="ready")
        if sid == "bx_f7k2q9hd" and sid not in st.sandboxes:
            st.sandboxes[sid] = dict(_sandbox(sid, {}), state="ready")
        sb = st.sandboxes.get(sid)
        if sb is None:
            return _err(404, "sandbox_not_found", "Sandbox not found.")
        return self.sandbox_route(method, sb, rest, q, headers, body)

    def sandbox_route(self, method: str, sb: Dict[str, Any], rest: str, q: Dict[str, str], headers: Any, body: Any):
        st, sid = self.state, sb["id"]
        if rest == "" and method == "GET":
            sb["_gets"] += 1
            if sb["state"] == "provisioning" and sb["_gets"] > 1:
                sb["state"] = "ready"
            return 200, _ok("sandbox", sandbox=_public(sb))
        if rest == "" and method == "DELETE":
            if headers.get("X-Ascii-Confirm-Delete") != sid and headers.get("X-CrowdVM-Confirm-Delete") != sid:
                return _err(400, "confirmation_required", "Send X-Ascii-Confirm-Delete: <sandbox id>.")
            del st.sandboxes[sid]
            return 202, _ok("sandbox.deleting", operation={"id": st.new_id("del_"), "status": "queued"})
        if rest == "/stop" and method == "POST":
            sb["state"] = "archived"
            return 202, _ok("sandbox.stopping", id=sid, status="archiving", force=bool(body.get("force")))
        if rest == "/resume" and method == "POST":
            if sb["state"] == "ready":
                return 200, _ok("sandbox.ready", id=sid, status="ready", sandbox=_public(sb))
            sb["state"] = "provisioning"
            sb["_gets"] = 0
            return 202, _ok("sandbox.resuming", id=sid, status="provisioning", sandbox=_public(sb))
        if rest == "/fork" and method == "POST":
            key = headers.get("Idempotency-Key")
            if key and key in st.idem:
                fid = st.idem[key]
            else:
                fid = st.new_id("bx_")
                st.sandboxes[fid] = _sandbox(fid, dict(body, type=sb["type"]))
                if key:
                    st.idem[key] = fid
            return 202, _ok("sandbox.forking", id=fid, status="forking", sandbox=_public(st.sandboxes[fid]))
        if rest == "/usage" and method == "GET":
            return 200, _ok("sandbox.usage", sandboxId=sid, sandboxType=sb["type"], billingMultiplier=1,
                            since="2026-11-08T00:00:00Z", until="2026-11-08T00:01:00Z", seconds=60,
                            dollars=0.0006, secondsPerDollar=100000, running=sb["state"] == "ready")
        if sb["state"] == "provisioning" and rest in ("/commands", "/files", "/prompt"):
            sb["state"] = "ready"  # the server waits for a starting sandbox
        if sb["state"] != "ready" and rest in ("/commands", "/files", "/prompt"):
            return _err(409, "sandbox_not_ready", f"Sandbox is {sb['state']}.")
        if rest == "/commands" and method == "POST":
            cmd = body.get("command")
            if not isinstance(cmd, str) or not cmd:
                return _err(400, "invalid_command", "command is required")
            out = self._run(sb, cmd)
            if body.get("detached"):
                pid = next(st.seq)
                sb.setdefault("_procs", {})[pid] = out
                return 200, _ok("command.started", processId=pid, commandId=f"cmd_{pid}", pid=pid)
            if body.get("stream"):
                return 200, [{"type": "started", "processId": 1}, {"type": "stdout", "data": out},
                             {"type": "exit", "exitCode": 0}]
            return 200, _ok("command.completed", exitCode=0, stdout=out, stderr="", timedOut=False,
                            durationMs=5, commandId=f"cmd_{next(st.seq)}")
        m = re.fullmatch(r"/commands/(\d+)(/stream)?", rest)
        if m:
            out = sb.get("_procs", {}).get(int(m.group(1)))
            if out is None:
                return _err(404, "process_not_found", "No such process.")
            if method == "DELETE":
                return 200, _ok("command.stopped", processId=int(m.group(1)))
            if m.group(2):
                return 200, [{"type": "attached", "stdoutOffset": 0, "stderrOffset": 0},
                             {"type": "stdout", "data": out, "nextOffset": len(out)}, {"type": "exit", "exitCode": 0}]
            return 200, _ok("command.status", processId=int(m.group(1)), running=False, status="exited",
                            exitCode=0, stdout=out, stderr="")
        if rest == "/files" and method == "PUT":
            if "path" not in body or "content" not in body:
                return _err(400, "invalid_request", "path and content are required")
            st.files[(sid, body["path"])] = body["content"].encode()
            return 200, _ok("file.written", path=body["path"], size=len(body["content"]))
        if rest == "/files" and method == "GET":
            data = st.files.get((sid, q.get("path", "")))
            if data is None:
                return _err(404, "file_not_found", "No such file.")
            return 200, _ok("file", path=q["path"], content=data.decode(), encoding="utf8")
        if rest in ("/files/upload-url", "/files/download-url") and method == "POST":
            tok = st.new_id("t", 12)
            st.signed[tok] = (sid, body["path"])
            up = rest.endswith("upload-url")
            return 200, _ok("file.url", url=f"{self.root}/signed/{tok}", method="PUT" if up else "GET",
                            path=body["path"], expiresAt="2026-11-09T00:00:00Z")
        if rest == "/host" and method == "POST":
            port = body.get("port")
            if not isinstance(port, int):
                return _err(400, "invalid_port", "port must be an integer")
            access = "public" if body.get("public") else "private"
            url = f"http://{sid[3:]}-{port}.localhost:8420/" + ("" if access == "public" else "?_token=t")
            return 200, _ok("sandbox.host", port=port, url=url, access=access, isProtected=access == "private")
        if rest == "/prompt" and method == "POST":
            if not body.get("prompt"):
                return _err(400, "prompt_required", "prompt is required")
            pid, cid = st.new_id("pr_"), body.get("conversationId") or st.new_id("conv_")
            st.prompts[pid] = {"id": pid, "status": "finished", "done": True, "conversationId": cid}
            evs = st.events.setdefault(sid, [])
            for typ, data in (("prompt", {"content": body["prompt"]}), ("response", {"content": "done."})):
                evs.append({"id": f"ev_{next(st.seq)}", "type": typ, "conversationId": cid, "timestamp": len(evs) + 1,
                            "data": data})
            return 202, _ok("prompt.queued", promptId=pid, conversationId=cid, status="queued")
        m = re.fullmatch(r"/prompts/(\w+)", rest)
        if m and method == "GET":
            run = st.prompts.get(m.group(1))
            return (200, _ok("prompt.run", promptRun=run)) if run else _err(404, "prompt_not_found", "No such prompt.")
        if rest == "/steer" and method == "POST":
            return 200, _ok("sandbox.steer", mode="native", native=True, message=body.get("message"))
        if rest == "/events" and method == "GET":
            evs = list(st.events.get(sid, []))
            if q.get("type"):
                evs = [e for e in evs if e["type"] in q["type"].split(",")]
            if q.get("conversation"):
                evs = [e for e in evs if e["conversationId"] == q["conversation"]]
            if q.get("sort") == "desc":
                evs.reverse()
            return 200, _ok("sandbox.events", events=evs[: int(q.get("limit", 50))],
                            pageInfo={"hasMore": False, "nextCursor": None})
        if rest == "/snapshots/latest" and method == "GET":
            return 200, _ok("snapshot", snapshot={"id": "snap_" + sid[3:], "sandboxId": sid, "status": "completed"})
        return _err(404, "not_found", f"no route {method} /sandboxes/{sid}{rest}")

    @staticmethod
    def _run(sb: Dict[str, Any], cmd: str) -> str:
        if cmd.startswith("host "):
            port = cmd.split()[1]
            return f"http://{sb['id'][3:]}-{port}.localhost:8420/?_token=t\n"
        if "uname" in cmd:
            return "hi\nLinux fake 6.8.0 x86_64 GNU/Linux\n"
        if cmd.startswith("echo "):
            return cmd[5:].split("&&")[0].strip().strip("'\"") + "\n"
        if "print(6*7)" in cmd:
            return "42\n"
        return "ok\n"


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=0)
    a = ap.parse_args()
    with FakeBoat(a.port) as fake:
        print(fake.base, flush=True)
        try:
            while True:
                time.sleep(3600)
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
