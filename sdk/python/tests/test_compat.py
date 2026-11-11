import unittest

from crowdvm import ApiClient, BoatApi, Configuration, wait_until_ready
from crowdvm.models import CommandRequest, CreateSandboxRequest, ForkRequest
from .fake_server import FakeServer, ok

SB = {"id": "bx_abcdefgh", "name": "t", "state": "ready"}


class BoatCompatTests(unittest.TestCase):
    def test_x_ascii_confirm_delete_and_alias(self):
        with FakeServer() as srv:
            srv.route("DELETE", "/api/v1/sandboxes/bx_x", lambda r: ok({"operation": {}}, 202))
            srv.route("DELETE", "/api/v1/snapshots/snap_1", lambda r: ok({}, 202))
            srv.route("DELETE", "/api/v1/sandboxes/bx_x/snapshots", lambda r: ok({"operations": []}, 202))
            cfg = Configuration(host=srv.base, access_token="cvm_t")
            with ApiClient(cfg) as client:
                boat = BoatApi(client)
                boat.delete_sandbox(sandbox_id="bx_x", x_ascii_confirm_delete="bx_x")
                boat.delete_snapshot("snap_1", confirm_delete="snap_1")
                boat.delete_sandbox_snapshots("bx_x", x_ascii_confirm_delete="bx_x")
            h = [r.headers for r in srv.requests]
            self.assertEqual(h[0]["x-ascii-confirm-delete"], "bx_x")
            self.assertNotIn("x-crowdvm-confirm-delete", h[0])
            self.assertEqual(h[1]["x-crowdvm-confirm-delete"], "snap_1")
            self.assertEqual(h[2]["x-ascii-confirm-delete"], "bx_x")
            self.assertEqual(h[0]["authorization"], "Bearer cvm_t")

    def test_x_boat_org_on_create_and_fork(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes", lambda r: ok({"sandbox": SB}, 202))
            srv.route("POST", "/api/v1/sandboxes/bx_x/fork", lambda r: ok({"sandbox": SB}, 202))
            boat = BoatApi(ApiClient(Configuration(host=srv.base, access_token="k")))
            boat.create(x_boat_org="acme", ttl_seconds=60)
            boat.fork("bx_x", x_boat_org="acme", type="small")
            self.assertEqual(srv.requests[0].headers["x-boat-org"], "acme")
            self.assertEqual(srv.requests[1].headers["x-boat-org"], "acme")
            self.assertEqual(srv.requests[1].json, {"type": "small"})

    def test_wrapped_request_models_and_command(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes", lambda r: ok({"sandbox": SB}, 202))
            srv.route("POST", "/api/v1/sandboxes/bx_x/fork", lambda r: ok({"sandbox": SB}, 202))
            srv.route("POST", "/api/v1/sandboxes/bx_x/commands", lambda r: ok({"exitCode": 0, "stdout": "hi\n"}))
            boat = BoatApi(ApiClient(Configuration(host=srv.base, access_token="k")))
            boat.create(create_sandbox_request=CreateSandboxRequest(ttl_seconds=1800, var_from="web", placement={"nested": True}))
            self.assertEqual(srv.requests[0].json, {"ttlSeconds": 1800, "from": "web", "placement": {"nested": True}})
            boat.fork("bx_x", fork_request=ForkRequest(fail_fast=True))
            self.assertEqual(srv.requests[1].json, {"failFast": True})
            out = boat.command("bx_x", CommandRequest(command="echo hi", timeout_seconds=5)).actual_instance
            self.assertEqual(out.exit_code, 0)
            self.assertEqual(srv.requests[2].json, {"command": "echo hi", "timeoutSeconds": 5})

    def test_placement_nested_browser(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes", lambda r: ok({"sandbox": SB}, 202))
            srv.route("POST", "/api/v1/sandboxes/bx_x/resume", lambda r: ok({"sandbox": SB}, 202))
            srv.route("POST", "/api/v1/sandboxes/bx_x/browser", lambda r: ok({"browserUrl": "http://b"}))
            boat = BoatApi(ApiClient(Configuration(host=srv.base, access_token="k")))
            boat.create(nested=True, placement={"labels": {"rack": "a"}})
            boat.resume("bx_x", placement={"gpu": True})
            self.assertEqual(boat.browser("bx_x", profile="/data/p").browser_url, "http://b")
            self.assertEqual(srv.requests[0].json, {"nested": True, "placement": {"labels": {"rack": "a"}}})
            self.assertEqual(srv.requests[1].json, {"placement": {"gpu": True}})
            self.assertEqual(srv.requests[2].json, {"profile": "/data/p"})


class BoatPositionalModelTests(unittest.TestCase):
    """Boat's method table passes request models positionally (opus-1 review #5)."""

    def setUp(self):
        self.srv = FakeServer().__enter__()
        self.srv.route("POST", "/api/v1/sandboxes/bx_x/host", lambda r: ok({"url": "http://u"}))
        self.srv.route("PUT", "/api/v1/sandboxes/bx_x/files", lambda r: ok({"path": "a"}))
        self.srv.route("DELETE", "/api/v1/sandboxes/bx_x", lambda r: ok({"operation": {}}, 202))
        self.srv.route("DELETE", "/api/v1/environments/env_1/secret-files", lambda r: ok({}))
        self.srv.route("POST", "/api/v1/webhooks", lambda r: ok({"webhook": {}, "secret": "s"}, 201))
        self.srv.route("POST", "/api/v1/sandboxes/bx_x/resume", lambda r: ok({"sandbox": SB}, 202))

    def tearDown(self):
        self.srv.__exit__(None, None, None)

    def test_plain_client_accepts_models(self):
        from crowdvm import CrowdVM
        from crowdvm.models import FileWriteRequest, HostPortRequest
        cvm = CrowdVM("k", self.srv.base)
        cvm.host_port("bx_x", HostPortRequest(port=8080, public=True))
        cvm.write_file("bx_x", FileWriteRequest(path="a", content="b"))
        cvm.write_file("bx_x", "a", "b")
        self.assertEqual([r.json for r in self.srv.requests],
                         [{"port": 8080, "public": True}, {"path": "a", "content": "b"}, {"path": "a", "content": "b"}])
        with self.assertRaisesRegex(TypeError, "content"):
            cvm.write_file("bx_x", "a")

    def test_boat_positional_order_and_native_fallback(self):
        from boat_sdk.api.boat_api import BoatApi as B
        from boat_sdk.models import ResumeRequest, WebhookCreateRequest
        boat = B(ApiClient(Configuration(host=self.srv.base, access_token="k")))
        boat.delete_sandbox("bx_x", "bx_x")                       # (x_ascii_confirm_delete, sandbox_id)
        boat.delete_environment_secret_file(".env", "env_1")      # (path, environment_id)
        boat.create_webhook(WebhookCreateRequest(url="http://h", events=["sandbox.ready"]))
        boat.create_webhook("http://h", ["sandbox.ready"])        # CrowdVM order still works
        boat.resume("bx_x", ResumeRequest(no_env=True))
        r = self.srv.requests
        self.assertEqual(r[0].headers["x-ascii-confirm-delete"], "bx_x")
        self.assertEqual(r[1].query, {"path": ".env"})
        self.assertEqual(r[2].json, {"url": "http://h", "events": ["sandbox.ready"]})
        self.assertEqual(r[3].json, r[2].json)
        self.assertEqual(r[4].json, {"noEnv": True})

    def test_model_classes(self):
        from boat_sdk.models.create_sandbox_request import CreateSandboxRequest as C
        m = C.from_dict({"from": "web", "noEnv": True})
        self.assertEqual(m["from"], "web")
        self.assertEqual(C(ttl_seconds=None).to_json(), '{"ttlSeconds": null}')
        self.assertIsInstance(m, C)


if __name__ == "__main__":
    unittest.main()
