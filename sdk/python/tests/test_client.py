import unittest

from crowdvm import (ApiError, AuthenticationError, CrowdVM, NotFoundError, Obj, RateLimitError,
                     ServiceUnavailableError)
from .fake_server import FakeServer, Resp, err, ok

SB = {"id": "bx_abcdefgh", "name": "t", "state": "ready", "memoryGB": 8, "vcpu": 4}


def client(srv, **kw):
    c = CrowdVM(api_key="cvm_test", base_url=srv.base, **kw)
    c._sleep = lambda s: None
    return c


class ClientTests(unittest.TestCase):
    def test_create_flat_kwargs_idempotency_and_attr_access(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes", lambda r: ok({"sandbox": SB}, 202))
            cvm = client(srv)
            out = cvm.create(ttl_seconds=1800, var_from="web-stack", no_env=True, idempotency_key="k1",
                             env={"A_B": "1"}, labels={"session_id": "x"})
            req = srv.requests[0]
            self.assertEqual(req.headers["authorization"], "Bearer cvm_test")
            self.assertEqual(req.headers["idempotency-key"], "k1")
            self.assertEqual(req.json, {"ttlSeconds": 1800, "from": "web-stack", "noEnv": True,
                                        "env": {"A_B": "1"}, "labels": {"session_id": "x"}})
            self.assertEqual(out.sandbox.id, "bx_abcdefgh")
            self.assertEqual(out.sandbox.memory_gb, 8)
            self.assertEqual(out["sandbox"]["vcpu"], 4)
            self.assertIs(out.actual_instance, out)
            self.assertIsInstance(out.sandbox, Obj)

    def test_ttl_none_is_sent_as_null_and_empty_body_is_json(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes", lambda r: ok({"sandbox": SB}, 202))
            cvm = client(srv)
            cvm.create(ttl_seconds=None)
            self.assertEqual(srv.requests[0].json, {"ttlSeconds": None})
            cvm.create()
            self.assertEqual(srv.requests[1].json, {})

    def test_base_url_gets_api_v1_appended(self):
        self.assertEqual(CrowdVM(base_url="http://10.0.0.5:8420/").base_url, "http://10.0.0.5:8420/api/v1")
        self.assertEqual(CrowdVM(base_url="http://h/api/v1").base_url, "http://h/api/v1")

    def test_env_configuration(self):
        import os
        old = dict(os.environ)
        try:
            os.environ["CROWDVM_API_KEY"] = "cvm_env"
            os.environ["CROWDVM_API_BASE"] = "http://h:1/api/v1"
            c = CrowdVM()
            self.assertEqual((c.api_key, c.base_url), ("cvm_env", "http://h:1/api/v1"))
        finally:
            os.environ.clear(); os.environ.update(old)

    def test_delete_confirmation_header_defaults_to_id(self):
        with FakeServer() as srv:
            srv.route("DELETE", "/api/v1/sandboxes/bx_x", lambda r: ok({"operation": {}}, 202))
            srv.route("DELETE", "/api/v1/snapshots/snap_1", lambda r: ok({}, 202))
            cvm = client(srv)
            cvm.delete_sandbox("bx_x")
            cvm.delete_snapshot("snap_1")
            self.assertEqual(srv.requests[0].headers["x-crowdvm-confirm-delete"], "bx_x")
            self.assertEqual(srv.requests[1].headers["x-crowdvm-confirm-delete"], "snap_1")

    def test_query_encoding_path_quoting_and_positional_args(self):
        with FakeServer() as srv:
            srv.route("GET", r"/api/v1/sandboxes/bx_x/commands/7", lambda r: ok({"running": False, "exitCode": 0}))
            srv.route("GET", r"/api/v1/sandboxes/bx_x/events", lambda r: ok({"events": []}))
            srv.route("GET", r"/api/v1/saved-snapshots/web%20stack", lambda r: ok({"snapshot": {"status": "ready"}}))
            cvm = client(srv)
            st = cvm.command_status("bx_x", 7, tail_bytes=100, stdout_offset=0)
            self.assertEqual(st.exit_code, 0)
            self.assertEqual(srv.requests[0].query, {"tailBytes": "100", "stdoutOffset": "0"})
            cvm.events("bx_x", limit=5, type=["prompt", "response"], sort="asc")
            self.assertEqual(srv.requests[1].query, {"limit": "5", "type": "prompt,response", "sort": "asc"})
            self.assertEqual(cvm.get_saved_snapshot("web stack").snapshot.status, "ready")

    def test_required_body_positional(self):
        with FakeServer() as srv:
            srv.route("PUT", "/api/v1/sandboxes/bx_x/files", lambda r: ok({"success": True}))
            srv.route("POST", "/api/v1/sandboxes/bx_x/prompt", lambda r: ok({"promptId": "p1"}, 202))
            srv.route("POST", "/api/v1/sandboxes/bx_x/host", lambda r: ok({"url": "http://u"}))
            cvm = client(srv)
            cvm.write_file("bx_x", "a.txt", "hi", encoding="base64")
            self.assertEqual(srv.requests[0].json, {"path": "a.txt", "content": "hi", "encoding": "base64"})
            cvm.prompt("bx_x", "do it", provider="codex", new=True)
            self.assertEqual(srv.requests[1].json, {"prompt": "do it", "provider": "codex", "new": True})
            cvm.host_port("bx_x", 8000, public=True)
            self.assertEqual(srv.requests[2].json, {"port": 8000, "public": True})

    def test_extra_kwargs_pass_through_and_bodyless_ops_reject_them(self):
        with FakeServer() as srv:
            srv.route("PATCH", "/api/v1/sandboxes/bx_x", lambda r: ok({"sandbox": SB}))
            cvm = client(srv)
            cvm.update("bx_x", name="n", someNewField=1)
            self.assertEqual(srv.requests[0].json, {"name": "n", "someNewField": 1})
            with self.assertRaises(TypeError):
                cvm.get("bx_x", bogus=1)

    def test_errors(self):
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/sandboxes/bx_missing", lambda r: err(404, "not_found", "no such sandbox"))
            srv.route("GET", "/api/v1/me", lambda r: err(401, "unauthorized"))
            srv.route("GET", "/api/v1/limits", lambda r: Resp(500, None, raw=None))
            cvm = client(srv)
            with self.assertRaises(NotFoundError) as cm:
                cvm.get("bx_missing")
            e = cm.exception
            self.assertEqual((e.status, e.code, e.message, e.request_id), (404, "not_found", "no such sandbox", "req_test"))
            self.assertIsInstance(e, ApiError)
            with self.assertRaises(AuthenticationError):
                cvm.me()
            with self.assertRaises(ApiError) as cm:
                cvm.limits()
            self.assertEqual(cm.exception.status, 500)

    def test_retry_503_honors_retry_after_seconds(self):
        with FakeServer() as srv:
            srv.seq("GET", "/api/v1/limits",
                    err(503, "out_of_capacity", details={"retryAfterSeconds": 7}),
                    err(503, "out_of_capacity", retryAfterSeconds=3), ok({"canStart": True}))
            cvm = client(srv)
            sleeps = []
            cvm._sleep = sleeps.append
            self.assertTrue(cvm.limits().can_start)
            self.assertEqual(sleeps, [7.0, 3.0])
            self.assertEqual(len(srv.requests), 3)

    def test_retry_429_ip_limit_and_minute_window_but_not_hour(self):
        with FakeServer() as srv:
            srv.seq("POST", "/api/v1/sandboxes",
                    err(429, "IP_RATE_LIMIT_EXCEEDED", retryAfter=2),
                    err(429, "rate_limited", details={"window": "minute", "limit": 12}, retryAfterSeconds=1),
                    ok({"sandbox": SB}, 202))
            cvm = client(srv)
            sleeps = []
            cvm._sleep = sleeps.append
            cvm.create()
            self.assertEqual(sleeps[0], 2.0)
            self.assertEqual(len(srv.requests), 3)
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes", lambda r: err(429, "rate_limited", details={"window": "hour"}))
            with self.assertRaises(RateLimitError):
                client(srv).create()
            self.assertEqual(len(srv.requests), 1)
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes", lambda r: err(429, "limit_reached"))
            with self.assertRaises(RateLimitError):
                client(srv).create()
            self.assertEqual(len(srv.requests), 1)

    def test_retries_are_bounded(self):
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/me", lambda r: err(503, "out_of_capacity", retryAfterSeconds=0))
            with self.assertRaises(ServiceUnavailableError):
                client(srv, max_retries=2).me()
            self.assertEqual(len(srv.requests), 3)
            n = len(srv.requests)
            with self.assertRaises(ServiceUnavailableError):
                client(srv, max_retries=0).me()
            self.assertEqual(len(srv.requests), n + 1)

    def test_retry_restoring_and_starting(self):
        with FakeServer() as srv:
            srv.seq("POST", "/api/v1/sandboxes/bx_x/commands",
                    err(502, "sandbox_direct_failed", "sandbox_restoring", {"retryable": True, "message": "sandbox_restoring"}),
                    err(409, "sandbox_starting"), ok({"exitCode": 0, "stdout": "hi\n"}))
            out = client(srv).command("bx_x", "echo hi")
            self.assertEqual(out.stdout, "hi\n")
            self.assertEqual(len(srv.requests), 3)
        with FakeServer() as srv:  # a 502 that may have run is NOT resent
            srv.route("POST", "/api/v1/sandboxes/bx_x/commands",
                      lambda r: err(502, "sandbox_direct_failed", "connection reset", {"retryable": True}))
            with self.assertRaises(ApiError) as cm:
                client(srv).command("bx_x", "make deploy")
            self.assertTrue(cm.exception.retryable)
            self.assertEqual(len(srv.requests), 1)

    def test_bytes_and_304(self):
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/sandboxes/bx_x/artifacts", lambda r: Resp(200, raw=b"\x00\x01bin"))
            srv.route("GET", "/api/v1/sandboxes/bx_x/events", lambda r: Resp(304))
            cvm = client(srv)
            self.assertEqual(cvm.artifact("bx_x", "out.bin"), b"\x00\x01bin")
            self.assertEqual(srv.requests[0].query, {"path": "out.bin"})
            self.assertIsNone(cvm.events("bx_x", if_none_match='"etag"'))
            self.assertEqual(srv.requests[1].headers["if-none-match"], '"etag"')

    def test_command_sync_detached_and_stream(self):
        with FakeServer() as srv:
            def cmd(r):
                b = r.json
                if b.get("stream"):
                    return Resp(200, lines=[{"type": "started", "commandId": "c1"}, {"type": "stdout", "data": "a"},
                                            {"type": "stderr", "data": "b"}, {"type": "exit", "exitCode": 3}])
                if b.get("detached"):
                    return ok({"processId": 9, "pid": 123, "command": b["command"]})
                return ok({"exitCode": 0, "stdout": "ok", "stderr": "", "timedOut": False})
            srv.route("POST", "/api/v1/sandboxes/bx_x/commands", cmd)
            cvm = client(srv)
            r = cvm.command("bx_x", "ls", cwd="repo", timeout_seconds=600)
            self.assertEqual((r.exit_code, r.timed_out), (0, False))
            self.assertEqual(srv.requests[0].json, {"command": "ls", "cwd": "repo", "timeoutSeconds": 600})
            self.assertEqual(cvm.command("bx_x", "sleep 1", detached=True).process_id, 9)
            frames = list(cvm.command("bx_x", "x", stream=True))
            self.assertEqual([f.type for f in frames], ["started", "stdout", "stderr", "exit"])
            self.assertEqual(frames[-1].exit_code, 3)

    def test_raw_request_and_with_options(self):
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/custom", lambda r: ok({"x": 1}))
            cvm = client(srv)
            self.assertEqual(cvm.request("GET", "/custom").x, 1)
            c2 = cvm.with_options(max_retries=9)
            self.assertEqual((c2.max_retries, cvm.max_retries), (9, 3))
            with self.assertRaises(TypeError):
                cvm.with_options(nope=1)

    def test_attach_stream_idle_timeout(self):
        import time
        from crowdvm import APIConnectionError
        with FakeServer() as srv:
            def slow(r):
                time.sleep(0.5)
                return Resp(200, lines=[{"type": "exit", "exitCode": 0}])
            srv.route("GET", "/api/v1/sandboxes/bx_x/commands/1/stream", slow)
            cvm = client(srv, stream_idle_timeout=0.1)
            with self.assertRaises(APIConnectionError):
                list(cvm.attach_command("bx_x", 1))

    def test_connection_error(self):
        from crowdvm import APIConnectionError
        cvm = CrowdVM(api_key="k", base_url="http://127.0.0.1:1/api/v1", max_retries=0)
        with self.assertRaises(APIConnectionError):
            cvm.me()


if __name__ == "__main__":
    unittest.main()
