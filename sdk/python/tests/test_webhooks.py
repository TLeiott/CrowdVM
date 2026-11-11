import json
import unittest

from crowdvm import WebhookVerificationError, construct_event, sign_webhook, verify_webhook

SECRET = "whsec_test"
BODY = json.dumps({"id": "evt_" + "a" * 32, "type": "sandbox.ready", "createdAt": "2026-01-01T00:00:00Z",
                   "data": {"sandbox": {"id": "bx_x", "name": "n"}, "previousState": "cloning", "state": "ready"}}).encode()
NOW = 1_800_000_000


def headers(secret=SECRET, ts=NOW, prefix="X-CrowdVM-", body=BODY):
    return {prefix + "Delivery": "evt_" + "a" * 32, prefix + "Timestamp": str(ts),
            prefix + "Signature": sign_webhook(secret, "evt_" + "a" * 32, ts, body), prefix + "Event": "sandbox.ready",
            prefix + "Attempt": "1"}


class WebhookTests(unittest.TestCase):
    def test_valid(self):
        self.assertTrue(verify_webhook(BODY, headers(), SECRET, now=NOW))
        ev = construct_event(BODY, headers(), SECRET, now=NOW)
        self.assertEqual((ev.type, ev.data.sandbox.id, ev.data.state), ("sandbox.ready", "bx_x", "ready"))

    def test_boat_header_names_and_case_insensitive(self):
        h = {k.lower(): v for k, v in headers(prefix="X-Ascii-").items()}
        self.assertTrue(verify_webhook(BODY, h, SECRET, now=NOW))

    def test_matches_documented_algorithm(self):
        import hashlib, hmac
        expected = hmac.new(SECRET.encode(), b"evt_" + b"a" * 32 + b"." + str(NOW).encode() + b"." + BODY, hashlib.sha256).hexdigest()
        self.assertEqual(headers()["X-CrowdVM-Signature"], "v1=" + expected)

    def test_rejects(self):
        self.assertFalse(verify_webhook(BODY, headers("whsec_other"), SECRET, now=NOW))
        self.assertFalse(verify_webhook(BODY + b" ", headers(), SECRET, now=NOW))  # modified body
        self.assertFalse(verify_webhook(BODY, headers(ts=NOW - 301), SECRET, now=NOW))  # replay
        self.assertTrue(verify_webhook(BODY, headers(ts=NOW - 299), SECRET, now=NOW))
        h = headers(); h["X-CrowdVM-Signature"] = "v1=nothex"
        self.assertFalse(verify_webhook(BODY, h, SECRET, now=NOW))
        self.assertFalse(verify_webhook(BODY, {}, SECRET, now=NOW))
        h = headers(); h["X-CrowdVM-Timestamp"] = "abc"
        self.assertFalse(verify_webhook(BODY, h, SECRET, now=NOW))
        with self.assertRaises(WebhookVerificationError):
            construct_event(BODY, headers("x"), SECRET, now=NOW)

    def test_rotation_accepts_old_and_new(self):
        self.assertTrue(verify_webhook(BODY, headers("old"), ["new", "old"], now=NOW))
        self.assertFalse(verify_webhook(BODY, headers("other"), ["new", "old"], now=NOW))


if __name__ == "__main__":
    unittest.main()
