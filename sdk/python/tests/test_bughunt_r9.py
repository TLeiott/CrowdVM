"""Round 9 bug-hunt tests: each one fails because of a real SDK bug."""
import asyncio
import socket
import threading
import time
import unittest

from crowdvm import AsyncCrowdVM, CrowdVM, APIConnectionError
from .fake_server import FakeServer, Resp, ok, err


class RawServer:
    """Answers every connection with fixed raw bytes (chunks sent with `gap` seconds between them)."""

    def __init__(self, chunks, gap=0.0, close=True):
        self.chunks, self.gap = chunks, gap
        self.sock = socket.socket()
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen(8)
        self.stop = False
        self.conns = []
        threading.Thread(target=self._loop, daemon=True).start()

    @property
    def base(self):
        return f"http://127.0.0.1:{self.sock.getsockname()[1]}/api/v1"

    def _loop(self):
        while not self.stop:
            try:
                c, _ = self.sock.accept()
            except OSError:
                return
            self.conns.append(c)
            threading.Thread(target=self._serve, args=(c,), daemon=True).start()

    def _serve(self, c):
        try:
            buf = b""
            while b"\r\n\r\n" not in buf:
                d = c.recv(65536)
                if not d:
                    return
                buf += d
            head, _, rest = buf.partition(b"\r\n\r\n")
            n = 0
            for ln in head.split(b"\r\n"):
                if ln.lower().startswith(b"content-length:"):
                    n = int(ln.split(b":")[1])
            while len(rest) < n:
                rest += c.recv(65536)
            for ch in self.chunks:
                c.sendall(ch)
                time.sleep(self.gap)
            c.close()
        except OSError:
            pass

    def __enter__(self):
        return self

    def __exit__(self, *a):
        self.stop = True
        self.sock.close()
        for c in self.conns:
            try:
                c.close()
            except OSError:
                pass


class R9Tests(unittest.TestCase):
    def test_truncated_json_body_is_a_connection_error(self):
        head = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n"
        with RawServer([head + b'{"ok":tr']) as srv:
            with self.assertRaises(APIConnectionError):
                CrowdVM(base_url=srv.base, max_retries=0).me()

    def test_truncated_bytes_body_is_a_connection_error(self):
        head = b"HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: 100\r\n\r\n"
        with RawServer([head + b"abc"]) as srv:
            with self.assertRaises(APIConnectionError):
                CrowdVM(base_url=srv.base, max_retries=0).artifact("bx_a", path="x")

    def test_retry_after_garbage_values_do_not_crash(self):
        for val in ("-5", "nan", "inf"):
            with self.subTest(retry_after=val), FakeServer() as srv:
                srv.seq("GET", "/api/v1/me", Resp(429, {"code": "rate_limited"}, headers={"Retry-After": val}),
                        ok({"x": 1}))
                cvm = CrowdVM(base_url=srv.base, max_retry_delay=0.05)
                self.assertEqual(cvm.me().x, 1)

    def test_with_options_base_url_is_normalised_like_the_constructor(self):
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/me", lambda r: ok({"x": 1}))
            root = srv.base[: -len("/api/v1")]
            self.assertEqual(CrowdVM(base_url=root).with_options(base_url=root).base_url, srv.base)
            self.assertEqual(CrowdVM(base_url=srv.base).with_options(base_url=srv.base + "/").base_url, srv.base)

    def test_stream_waits_for_headers_only_as_long_as_timeout(self):
        with FakeServer() as srv:
            def slow(_):
                time.sleep(3)
                return Resp(lines=[{"type": "exit", "exitCode": 0}])
            srv.route("POST", "/api/v1/sandboxes/bx_a/commands", slow)
            cvm = CrowdVM(base_url=srv.base, timeout=0.3, max_retries=0)
            t0 = time.monotonic()
            with self.assertRaises(APIConnectionError):
                list(cvm.command("bx_a", "sleep 100", stream=True))
            self.assertLess(time.monotonic() - t0, 2)

    def test_client_timeout_none_does_not_break_command(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes/bx_a/commands", lambda r: ok({"exitCode": 0}))
            self.assertEqual(CrowdVM(base_url=srv.base, timeout=None).command("bx_a", "true").exitCode, 0)

    def test_dot_segment_path_params_are_not_sent_raw(self):
        with FakeServer() as srv:
            cvm = CrowdVM(base_url=srv.base, max_retries=0)
            for bad in ("..", ".", ""):
                with self.assertRaisesRegex(ValueError, "invalid path parameter"):
                    cvm.get(bad)
                with self.assertRaisesRegex(ValueError, "invalid path parameter"):
                    cvm.command(bad, "x")
            self.assertEqual(srv.requests, [])
            for req in srv.requests:
                self.assertNotIn("..", req.path.split("/"), req.path)
                self.assertNotIn(".", req.path.split("/"), req.path)

    def test_authorization_is_not_forwarded_to_another_origin_on_redirect(self):
        with FakeServer() as other, FakeServer() as srv:
            other.route("GET", "/api/v1/me", lambda r: ok({"x": 1}))
            srv.route("GET", "/api/v1/me", lambda r: Resp(307, None, headers={"Location": other.base + "/me"}))
            CrowdVM("cvm_secret", base_url=srv.base, max_retries=0).me()
            self.assertEqual(len(other.requests), 1)
            self.assertNotIn("authorization", other.requests[0].headers)

    def test_async_client_can_be_copied(self):
        import copy
        a = AsyncCrowdVM(base_url="http://127.0.0.1:1")
        self.assertEqual(copy.copy(a).client.base_url, a.client.base_url)
        self.assertEqual(copy.deepcopy(a).client.base_url, a.client.base_url)

    def test_async_wait_cancelled_by_wait_for_does_not_keep_polling(self):
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/sandboxes/bx_a", lambda r: ok({"sandbox": {"id": "bx_a", "state": "starting"}}))
            cvm = AsyncCrowdVM(base_url=srv.base)

            async def main():
                try:
                    await asyncio.wait_for(cvm.wait_until_ready("bx_a", timeout=4, interval=0.1), 0.3)
                except asyncio.TimeoutError:
                    pass
            t0 = time.monotonic()
            asyncio.run(main())
            self.assertLess(time.monotonic() - t0, 2)


if __name__ == "__main__":
    unittest.main()
