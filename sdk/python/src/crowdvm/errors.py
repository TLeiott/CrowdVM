"""Exceptions raised by the SDK."""
from __future__ import annotations

import math
from typing import Any, Mapping, Optional


class CrowdVMError(Exception):
    """Base class of every error raised by this package."""


class APIConnectionError(CrowdVMError):
    """The server could not be reached, or the connection broke mid-response."""


class WaitTimeout(CrowdVMError, TimeoutError):
    """A `wait_*` helper ran out of time."""


class SandboxFailed(CrowdVMError):
    """The sandbox entered the `error` (or `cancelled`) state while we waited."""

    def __init__(self, sandbox: Any):
        self.sandbox = sandbox
        super().__init__(f"sandbox {sandbox.get('id')} is {sandbox.get('state')}: {sandbox.get('error')}")


class PromptFailed(CrowdVMError):
    """A prompt run finished as `failed` or `interrupted`."""

    def __init__(self, run: Any):
        self.run = run
        super().__init__(f"prompt {run.get('promptId')} {run.get('status')}")


def _wait_ok(v: float) -> bool:
    """A usable Retry-After: finite and not negative."""
    return math.isfinite(v) and v >= 0


class ApiError(CrowdVMError):
    """A non-2xx answer.  ``status``, ``code``, ``message``, ``details`` and
    ``request_id`` come from the error envelope; ``body`` is the raw text."""

    def __init__(self, status: int, body: str = "", payload: Optional[Mapping[str, Any]] = None,
                 headers: Optional[Mapping[str, str]] = None):
        self.status = status
        self.body = body
        self.payload = dict(payload) if isinstance(payload, Mapping) else {}
        self.headers = dict(headers or {})
        err = self.payload.get("error")
        err = err if isinstance(err, Mapping) else {}
        self.code: str = str(self.payload.get("code") or err.get("code") or f"http_{status}")
        self.message: str = str(self.payload.get("message") or err.get("message") or body[:300] or f"HTTP {status}")
        details = err.get("details") or self.payload.get("details") or {}
        self.details: dict = dict(details) if isinstance(details, Mapping) else {}
        self.request_id: Optional[str] = self.payload.get("requestId")
        super().__init__(f"{status} {self.code}: {self.message}")

    @property
    def retry_after_seconds(self) -> Optional[float]:
        """The wait the server asked for (``retryAfterSeconds``, ``retryAfter`` or ``Retry-After``)."""
        for src in (self.details, self.payload):
            for key in ("retryAfterSeconds", "retryAfter"):
                v = src.get(key)
                if isinstance(v, (int, float)) and not isinstance(v, bool) and _wait_ok(v):
                    return float(v)
        h = {k.lower(): v for k, v in self.headers.items()}.get("retry-after")
        try:
            return float(h) if h is not None and _wait_ok(float(h)) else None
        except ValueError:
            return None

    @property
    def retryable(self) -> bool:
        """``error.details.retryable`` as sent by `502 sandbox_direct_failed`."""
        return bool(self.details.get("retryable"))


# Boat-compatible alias.
ApiException = ApiError


class BadRequestError(ApiError): ...
class AuthenticationError(ApiError): ...
class PaymentRequiredError(ApiError): ...
class PermissionDeniedError(ApiError): ...
class NotFoundError(ApiError): ...
class ConflictError(ApiError): ...
class RateLimitError(ApiError): ...
class ServerError(ApiError): ...
class ServiceUnavailableError(ServerError): ...


_BY_STATUS = {400: BadRequestError, 401: AuthenticationError, 402: PaymentRequiredError, 403: PermissionDeniedError,
              404: NotFoundError, 409: ConflictError, 429: RateLimitError, 503: ServiceUnavailableError}


def make_api_error(status: int, body: str, payload: Any, headers: Optional[Mapping[str, str]]) -> ApiError:
    cls = _BY_STATUS.get(status) or (ServerError if status >= 500 else ApiError)
    return cls(status, body, payload, headers)
