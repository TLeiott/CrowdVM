import sys
import time
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(HERE.parent), str(HERE.parent.parent)]
from _testutil import FakeServer, err, ok  # noqa: E402
from crowdvm import ApiError, CrowdVM  # noqa: E402
import crowdvm_evals as ev  # noqa: E402


def client(srv):
    c = CrowdVM(api_key="k", base_url=srv.base)
    c._sleep = lambda s: None
    return c


class Sandboxes:
    """Fake backend: create -> ready sandbox; counts creates and deletes."""

    def __init__(self, srv):
        self.srv, self.n, self.deleted, self.created, self.fail_boot = srv, 0, [], [], 0
        srv.route("POST", "/api/v1/sandboxes", self.create)
        srv.route("GET", r"/api/v1/sandboxes/bx_\d+", self.get)
        srv.route("DELETE", r"/api/v1/sandboxes/bx_\d+", self.delete)

    def create(self, r):
        self.n += 1
        self.created.append(r)
        return ok({"sandbox": {"id": f"bx_{self.n}", "state": "provisioning"}}, 202)

    def get(self, r):
        sid = r.path.rsplit("/", 1)[1]
        if self.fail_boot > 0:
            self.fail_boot -= 1
            return ok({"sandbox": {"id": sid, "state": "error", "error": "start_failed: node: HTTP 503: out_of_capacity"}})
        return ok({"sandbox": {"id": sid, "state": "ready"}})

    def delete(self, r):
        self.deleted.append((r.path.rsplit("/", 1)[1], r.headers.get("x-ascii-confirm-delete")))
        return ok({"operation": {}}, 202)


class EvalTests(unittest.TestCase):
    def setUp(self):
        self._sleep = time.sleep
        time.sleep = lambda s: None

    def tearDown(self):
        time.sleep = self._sleep

    def test_start_retries_503_with_the_same_idempotency_key(self):
        with FakeServer() as srv:
            sb = Sandboxes(srv)
            sb.srv.routes.clear()
            attempts = []

            def create(r):
                attempts.append(r.headers["idempotency-key"])
                return err(503, "out_of_capacity") if len(attempts) < 3 else ok({"sandbox": {"id": "bx_1", "state": "ready"}}, 202)
            srv.route("POST", "/api/v1/sandboxes", create)
            srv.route("GET", "/api/v1/sandboxes/bx_1", lambda r: ok({"sandbox": {"id": "bx_1", "state": "ready"}}))
            cvm = client(srv); cvm.max_retries = 0
            self.assertEqual(ev.start_sandbox(cvm, type="small"), "bx_1")
            self.assertEqual(len(attempts), 3)
            self.assertEqual(len(set(attempts)), 1)

    def test_start_gives_up_on_other_errors(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes", lambda r: err(400, "invalid_type"))
            with self.assertRaises(ApiError):
                ev.start_sandbox(client(srv))

    def test_boot_out_of_capacity_is_deleted_and_retried(self):
        with FakeServer() as srv:
            sb = Sandboxes(srv)
            sb.fail_boot = 1
            sid = ev.start_sandbox(client(srv), type="small")
            self.assertEqual((sid, sb.n), ("bx_2", 2))
            self.assertEqual(sb.deleted, [("bx_1", "bx_1")])
            self.assertNotEqual(sb.created[0].headers["idempotency-key"], sb.created[1].headers["idempotency-key"])

    def test_episode_creates_from_template_and_always_deletes(self):
        with FakeServer() as srv:
            sb = Sandboxes(srv)
            with self.assertRaises(RuntimeError):
                with ev.Episode(client(srv), "task-env-v1", run_id="r1", ttl_seconds=60):
                    raise RuntimeError("episode crashed")
            body = sb.created[0].json
            self.assertEqual((body["from"], body["noEnv"], body["ttlSeconds"], body["labels"]), ("task-env-v1", True, 60, {"crowdvm.eval": "r1"}))
            self.assertEqual(sb.deleted, [("bx_1", "bx_1")])

    def test_run_episodes_keeps_order_and_collects_exceptions(self):
        with FakeServer() as srv:
            sb = Sandboxes(srv)

            def fn(ep, n):
                if n == 2:
                    raise ValueError("bad episode")
                return n * 10
            out = ev.run_episodes(client(srv), "t", fn, range(4), max_workers=2, return_exceptions=True)
            self.assertEqual([o for o in out if not isinstance(o, Exception)], [0, 10, 30])
            self.assertIsInstance(out[2], ValueError)
            self.assertEqual(len(sb.deleted), 4)
            with self.assertRaises(ValueError):
                ev.run_episodes(client(srv), "t", fn, [2])

    def test_prepare_template_runs_setup_saves_and_deletes(self):
        with FakeServer() as srv:
            sb = Sandboxes(srv)
            cmds, saved = [], []
            srv.route("POST", r"/api/v1/sandboxes/bx_1/commands", lambda r: (cmds.append(r.json["command"]), ok({"exitCode": 0}))[1])
            srv.route("POST", "/api/v1/saved-snapshots", lambda r: (saved.append(r.json), ok({"snapshot": {"status": "saving"}}, 202))[1])
            srv.route("GET", "/api/v1/saved-snapshots/task-env-v1", lambda r: ok({"snapshot": {"name": "task-env-v1", "status": "ready"}}))
            snap = ev.prepare_template(client(srv), "task-env-v1", ["echo a", "echo b"], interval=0)
            self.assertEqual(cmds, ["echo a", "echo b"])
            self.assertEqual(saved, [{"sandboxId": "bx_1", "name": "task-env-v1"}])
            self.assertEqual(snap["status"], "ready")
            self.assertEqual(sb.deleted, [("bx_1", "bx_1")])

    def test_prepare_template_fails_on_a_bad_setup_command(self):
        with FakeServer() as srv:
            sb = Sandboxes(srv)
            srv.route("POST", r"/api/v1/sandboxes/bx_1/commands", lambda r: ok({"exitCode": 2, "stderr": "nope"}))
            with self.assertRaisesRegex(RuntimeError, "setup command failed"):
                ev.prepare_template(client(srv), "x", ["false"])
            self.assertEqual(len(sb.deleted), 1)

    def test_cleanup_deletes_labelled_sandboxes_only(self):
        with FakeServer() as srv:
            sb = Sandboxes(srv)
            srv.route("GET", "/api/v1/sandboxes", lambda r: ok({"sandboxes": [
                {"id": "bx_1", "labels": {"crowdvm.eval": "r1"}}, {"id": "bx_2", "labels": {"other": "x"}}, {"id": "bx_3", "labels": {"crowdvm.eval": "r2"}}]}))
            self.assertEqual(ev.cleanup(client(srv)), 2)
            self.assertEqual([d[0] for d in sb.deleted], ["bx_1", "bx_3"])

    def test_pacer_spaces_starts(self):
        p = ev._Pacer(60)  # one per second
        t = [0.0]
        self.assertEqual(p.gap, 1.0)
        p.wait(); first = p.next
        p.wait()
        self.assertAlmostEqual(p.next - first, 1.0, places=1)


if __name__ == "__main__":
    unittest.main()
