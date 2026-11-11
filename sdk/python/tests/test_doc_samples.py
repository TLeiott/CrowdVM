"""Every Python and TypeScript sample of Boat's SDK docs runs unchanged (apart from the base URL)
against a fake CrowdVM server.  See sdk/tools/doc_samples.py; TypeScript needs node and a built
sdk/typescript/dist and is skipped otherwise."""
import os
import re
import sys
import unittest

TOOLS = os.path.join(os.path.dirname(__file__), "..", "..", "tools")
sys.path.insert(0, os.path.abspath(TOOLS))

import doc_samples  # noqa: E402
from boat_fake import FakeBoat  # noqa: E402

CAMEL = re.compile(r"^[a-z][A-Za-z0-9]*$")


class BoatDocSamples(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not os.path.isdir(os.path.join(doc_samples.ROOT, ".ref", "pages")):
            raise unittest.SkipTest(".ref/pages (Boat's docs) is not checked out")
        cls.fake = FakeBoat().__enter__()
        cls.results = doc_samples.run(fake=cls.fake, report=lambda _: None)

    @classmethod
    def tearDownClass(cls):
        cls.fake.__exit__(None, None, None)

    def test_every_sample_runs(self):
        self.assertGreaterEqual(len(self.results), 50)
        failed = {n: e for n, e in self.results if e and not e.startswith(("SKIP", "EXPECTED"))}
        self.assertEqual(failed, {}, "\n\n".join(f"{n}:\n{e}" for n, e in failed.items()))

    def test_wire_format(self):
        reqs = self.fake.state.requests
        for method, path, body, _ in reqs:
            if isinstance(body, dict) and path.startswith("/api/v1/"):
                bad = [k for k in body if not CAMEL.match(k) and k != "from"]
                self.assertEqual(bad, [], f"{method} {path} sent non-API keys {bad}: {body}")
        hosts = [b for m, p, b, _ in reqs if p.endswith("/host")]
        self.assertTrue(hosts and all(b.get("public") is True and isinstance(b.get("port"), int) for b in hosts), hosts)
        creates = [b for m, p, b, _ in reqs if (m, p) == ("POST", "/api/v1/sandboxes")]
        self.assertIn({"ttlSeconds": None, "noEnv": True, "env": {"TENANT_ID": "acme"}}, creates)
        self.assertIn({"from": "web-stack", "noEnv": True, "env": {"TENANT_ID": "acme"}}, creates)
        deletes = [h for m, p, b, h in reqs if m == "DELETE" and re.fullmatch(r"/api/v1/sandboxes/bx_\w+", p)]
        self.assertTrue(deletes and all(h.get("X-Ascii-Confirm-Delete") for h in deletes))
        stops = [b for m, p, b, _ in reqs if p == "/api/v1/sandboxes/bx_23456789/stop"]
        self.assertTrue(stops and all(b == {"force": True} for b in stops), stops)


if __name__ == "__main__":
    unittest.main()
