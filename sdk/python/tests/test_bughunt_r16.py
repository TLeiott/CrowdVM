"""Round 16 bug-hunt tests: each one fails because of a real SDK bug."""
import unittest

from crowdvm import ApiError, CrowdVM, WaitTimeout, wait_for_desktop
from .fake_server import FakeServer, err


class BughuntR16Tests(unittest.TestCase):
    def test_bughunt_r16_wait_for_desktop_fails_at_once_on_a_400(self):
        """A 400 (`invalid_theme`) can never become a desktop, but the helper polls through it until its
        timeout and raises a WaitTimeout that hides the server's reason."""
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes/bx_x/desktop",
                      lambda r: err(400, "invalid_theme", "theme must be light or dark."))
            c = CrowdVM(api_key="k", base_url=srv.base, max_retries=0)
            with self.assertRaises(ApiError) as cm:
                wait_for_desktop(c, "bx_x", timeout=0.3, interval=0.01, theme="neon")
            self.assertNotIsInstance(cm.exception, WaitTimeout)
            self.assertEqual(cm.exception.code, "invalid_theme")


if __name__ == "__main__":
    unittest.main()
