"""Verify the signature of a webhook delivery.

CrowdVM signs ``HMAC_SHA256(secret, delivery_id + "." + timestamp + "." + raw_body)`` and sends it
as ``v1=<hex>``.  Header names: ``X-CrowdVM-Delivery`` / ``-Timestamp`` / ``-Signature`` / ``-Event``
/ ``-Attempt`` (Boat's ``X-Ascii-*`` names are accepted too).

    from crowdvm.webhooks import construct_event
    event = construct_event(raw_body, request.headers, secret)   # raises WebhookVerificationError
"""
from __future__ import annotations

import hmac
import json
import re
import time
from hashlib import sha256
from typing import Any, Iterable, Mapping, Optional, Union

from ._util import Obj
from .errors import CrowdVMError

PREFIXES = ("x-crowdvm-", "x-ascii-")
_HEX64 = re.compile(r"[a-fA-F0-9]{64}")


class WebhookVerificationError(CrowdVMError):
    """The delivery is not authentic (or too old)."""


def _lower_headers(headers: Any) -> dict:
    items = headers.items() if hasattr(headers, "items") else headers
    return {str(k).lower(): v for k, v in items}


def _get(h: dict, name: str) -> str:
    for p in PREFIXES:
        v = h.get(p + name)
        if v:
            return str(v)
    return ""


def sign_webhook(secret: str, delivery_id: str, timestamp: Union[int, str], raw_body: Union[bytes, str]) -> str:
    """The ``X-CrowdVM-Signature`` value for a delivery (``v1=<hex>``).  Handy for tests."""
    body = raw_body.encode() if isinstance(raw_body, str) else raw_body
    mac = hmac.new(secret.encode(), f"{delivery_id}.{timestamp}.".encode() + body, sha256).hexdigest()
    return "v1=" + mac


def verify_webhook(raw_body: Union[bytes, str], headers: Any, secret: Union[str, Iterable[str]], *,
                   tolerance: float = 300.0, now: Optional[float] = None) -> bool:
    """True when the signature matches ``secret`` (a string, or several while a secret rotates)
    and the timestamp is within ``tolerance`` seconds.  Pass the *raw* request body."""
    h = _lower_headers(headers)
    delivery, timestamp = _get(h, "delivery"), _get(h, "timestamp")
    supplied = _get(h, "signature").removeprefix("v1=")
    try:
        if abs((time.time() if now is None else now) - float(timestamp)) > tolerance:
            return False
    except ValueError:
        return False
    if not _HEX64.fullmatch(supplied):
        return False
    secrets = [secret] if isinstance(secret, str) else list(secret)
    ok = False
    for s in secrets:  # check all, no early exit
        expected = sign_webhook(s, delivery, timestamp, raw_body)[3:]
        ok |= hmac.compare_digest(supplied.lower(), expected)
    return ok


def construct_event(raw_body: Union[bytes, str], headers: Any, secret: Union[str, Iterable[str]], *,
                    tolerance: float = 300.0, now: Optional[float] = None) -> Obj:
    """Verify, then parse the delivery body.  Dedupe on ``event["id"]`` (delivery is at-least-once)."""
    if not verify_webhook(raw_body, headers, secret, tolerance=tolerance, now=now):
        raise WebhookVerificationError("invalid webhook signature or timestamp")
    return json.loads(raw_body, object_hook=Obj)
