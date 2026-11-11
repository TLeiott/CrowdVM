import sys
import time
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path[:0] = [str(HERE.parent), str(HERE.parent.parent)]
from _testutil import FakeServer, err, ok  # noqa: E402
from crowdvm import ApiError, CrowdVM  # noqa: E402
import check_provider as cp  # noqa: E402


class ProviderTests(unittest.TestCase):
    def test_smallest_fitting_size(self):
        self.assertEqual(cp.pick_size(2, 4, 51), "small")
        self.assertEqual(cp.pick_size(2, 4, 52), "large")   # small/default have 51 GB
        self.assertEqual(cp.pick_size(4, 8, 51), "default")
        self.assertEqual(cp.pick_size(16, 32, 251), "xlarge")
        with self.assertRaises(ValueError):
            cp.pick_size(32, 8, 10)

    def test_create_retries_limits_with_one_idempotency_key_and_gives_up_after_six(self):
        real = time.sleep
        time.sleep = lambda s: None
        try:
            with FakeServer() as srv:
                keys = []

                def create(r):
                    keys.append(r.headers["idempotency-key"])
                    return err(429, "rate_limited", details={"window": "minute"}) if len(keys) < 4 else ok({"sandbox": {"id": "bx_1"}}, 202)
                srv.route("POST", "/api/v1/sandboxes", create)
                cvm = CrowdVM(api_key="k", base_url=srv.base, max_retries=0)
                self.assertEqual(cp.create_with_retries(cvm, type="small")["id"], "bx_1")
                self.assertEqual(len(set(keys)), 1)
            with FakeServer() as srv:
                srv.route("POST", "/api/v1/sandboxes", lambda r: err(503, "out_of_capacity"))
                cvm = CrowdVM(api_key="k", base_url=srv.base, max_retries=0)
                with self.assertRaises(ApiError):
                    cp.create_with_retries(cvm)
                self.assertEqual(len([r for r in srv.requests]), 6)
        finally:
            time.sleep = real


if __name__ == "__main__":
    unittest.main()
