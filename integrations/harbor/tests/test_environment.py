import asyncio
import base64
import os
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(HERE.parent), str(HERE.parent.parent)]
from _testutil import FakeServer, Resp, err, ok  # noqa: E402
from _harbor_shim import EnvironmentConfig, TrialPaths  # noqa: E402
from harbor_crowdvm_environment import CrowdVMEnvironment, dockerfile_workdir, pick_size  # noqa: E402

SB = {"id": "bx_harbor1", "state": "ready"}
CMD = "/api/v1/sandboxes/bx_harbor1/commands"


def run(coro):
    return asyncio.run(coro)


class Fixture:
    def __init__(self, srv, cfg=None, **kw):
        self.tmp = tempfile.TemporaryDirectory()
        t = Path(self.tmp.name)
        (t / "environment").mkdir()
        (t / "environment" / "Dockerfile").write_text("FROM a\nWORKDIR /ignored\nFROM b\nWORKDIR /app\nRUN nope\n")
        (t / "environment" / "seed.txt").write_text("seed")
        tp = TrialPaths(t / "trial"); tp.mkdir()
        self.env = CrowdVMEnvironment(environment_dir=t / "environment", environment_name="e", session_id="sess1",
                                      trial_paths=tp, task_env_config=cfg or EnvironmentConfig(), api_key="cvm_k",
                                      base_url=srv.base, **kw)
        self.env._cvm._sleep = lambda s: None


def over(srv, method, pattern, fn):
    """Route that wins over the ones registered before (FakeServer matches the first route)."""
    srv.route(method, pattern, fn)
    srv.routes.insert(0, srv.routes.pop())


def started(srv):
    srv.route("POST", "/api/v1/sandboxes", lambda r: ok({"sandbox": SB}, 202))
    srv.route("GET", "/api/v1/sandboxes/bx_harbor1", lambda r: ok({"sandbox": SB}))
    srv.route("POST", CMD, lambda r: ok({"exitCode": 0, "stdout": "", "stderr": ""}))
    srv.route("PUT", "/api/v1/sandboxes/bx_harbor1/files", lambda r: ok({"success": True}))


class EnvironmentTests(unittest.TestCase):
    def test_helpers(self):
        self.assertEqual(pick_size(1, 2048), "small")
        self.assertEqual(pick_size(4, 8192), "default")
        self.assertEqual(pick_size(2, 9000), "large")
        with self.assertRaises(ValueError):
            pick_size(64, 1)
        with tempfile.TemporaryDirectory() as d:
            (Path(d) / "Dockerfile").write_text("FROM a\nWORKDIR /one\nFROM b\nworkdir /two\n")
            self.assertEqual(dockerfile_workdir(Path(d)), "/two")

    def test_refusals(self):
        with FakeServer() as srv:
            for cfg in (EnvironmentConfig(docker_image="ubuntu:24.04"), EnvironmentConfig(allow_internet=False),
                        EnvironmentConfig(gpus=1)):
                with self.assertRaises(RuntimeError):
                    Fixture(srv, cfg)

    def test_start_creates_no_env_sandbox_and_uploads_environment(self):
        with FakeServer() as srv:
            started(srv)
            fx = Fixture(srv, ttl_seconds=600)
            run(fx.env.start())
            create = srv.requests[0]
            self.assertEqual(create.json["type"], "small")
            self.assertTrue(create.json["noEnv"])
            self.assertEqual(create.json["ttlSeconds"], 600)
            self.assertEqual(create.json["labels"]["harbor.session"], "sess1")
            self.assertTrue(create.headers["idempotency-key"])
            self.assertEqual(fx.env._workdir, "/app")  # last WORKDIR of the last stage
            writes = [r for r in srv.requests if r.method == "PUT"]
            self.assertEqual(len(writes), 1)
            self.assertEqual(base64.b64decode(writes[0].json["content"]), b"seed")
            self.assertEqual(writes[0].json["path"], "/app/seed.txt")

    def test_exec_env_user_cwd_and_timeout(self):
        with FakeServer() as srv:
            started(srv)
            fx = Fixture(srv, EnvironmentConfig(workdir="/app"))
            run(fx.env.start())
            bodies = []
            over(srv, "POST", CMD, lambda r: (bodies.append(r.json), ok({"exitCode": 7, "stdout": "o", "stderr": "e"}))[1])
            res = run(fx.env.exec("make test", env={"A": "x y"}, timeout_sec=30, user="root"))
            self.assertEqual((res.return_code, res.stdout, res.stderr), (7, "o", "e"))
            self.assertEqual(bodies[0]["timeoutSeconds"], 30)
            self.assertIn("sudo -n -u root", bodies[0]["command"])
            self.assertIn("cd /app && ", bodies[0]["command"])  # /app is outside the allowed cwd roots
            self.assertNotIn("cwd", bodies[0])
            run(fx.env.exec("true", cwd="/tmp/x", timeout_sec=5))
            self.assertEqual(bodies[1]["cwd"], "/tmp/x")
            # timed out
            over(srv, "POST", CMD, lambda r: ok({"exitCode": 0, "timedOut": True, "stdout": "", "stderr": ""}))
            self.assertEqual(run(fx.env.exec("sleep 99", timeout_sec=1)).return_code, 124)

    def test_long_command_runs_detached_and_follows(self):
        with FakeServer() as srv:
            started(srv)
            fx = Fixture(srv, EnvironmentConfig(workdir="/tmp/w"))
            run(fx.env.start())
            bodies = []

            def cmd(r):
                bodies.append(r.json)
                return ok({"processId": 5, "commandId": "cmd_1"}, 202)
            over(srv, "POST", CMD, cmd)
            srv.route("GET", CMD + "/5/stream", lambda r: Resp(200, lines=[
                {"type": "attached", "stdoutOffset": 0, "stderrOffset": 0},
                {"type": "stdout", "data": "a", "nextOffset": 1}, {"type": "stderr", "data": "b", "nextOffset": 1},
                {"type": "exit", "exitCode": 3}]))
            res = run(fx.env.exec("long job", timeout_sec=1800))
            self.assertEqual((res.stdout, res.stderr, res.return_code), ("a", "b", 3))
            self.assertTrue(bodies[0]["detached"])
            self.assertIn("timeout 1800 bash -c", bodies[0]["command"])

    def test_big_files_use_upload_url_and_stop_deletes(self):
        with FakeServer() as srv:
            started(srv)
            sink = FakeServer()
            sink.route("PUT", "/blob", lambda r: (sink.__dict__.setdefault("got", []).append(len(r.raw)), Resp(200, {}))[1])
            with sink:
                srv.route("POST", "/api/v1/sandboxes/bx_harbor1/files/upload-url",
                          lambda r: ok({"url": sink.base.replace("/api/v1", "/blob"), "method": "PUT", "path": r.json["path"]}))
                fx = Fixture(srv, EnvironmentConfig(workdir="/tmp/w"))
                run(fx.env.start())
                big = Path(fx.tmp.name) / "big.bin"
                big.write_bytes(os.urandom(5 * 1024 * 1024 + 1))
                run(fx.env.upload_file(big, "/tmp/w/big.bin"))
                self.assertEqual(sink.got, [5 * 1024 * 1024 + 1])
            deletes = []
            srv.route("DELETE", "/api/v1/sandboxes/bx_harbor1", lambda r: (deletes.append(r.headers), ok({"operation": {}}, 202))[1])
            run(fx.env.stop(delete=True))
            self.assertEqual(deletes[0]["x-ascii-confirm-delete"], "bx_harbor1")
            run(fx.env.stop(delete=True))  # second stop is a no-op
            self.assertEqual(len(deletes), 1)

    def test_start_failure_cleans_up(self):
        with FakeServer() as srv:
            started(srv)
            over(srv, "GET", "/api/v1/sandboxes/bx_harbor1", lambda r: ok({"sandbox": {**SB, "state": "error", "error": "boom"}}))
            deletes = []
            srv.route("DELETE", "/api/v1/sandboxes/bx_harbor1", lambda r: (deletes.append(1), ok({"operation": {}}, 202))[1])
            fx = Fixture(srv)
            with self.assertRaises(Exception):
                run(fx.env.start())
            self.assertEqual(deletes, [1])


if __name__ == "__main__":
    unittest.main()
