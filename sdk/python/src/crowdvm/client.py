"""The synchronous CrowdVM client (zero dependencies: urllib only)."""
from __future__ import annotations

import copy
import http.client
import json
import os
import random
import socket
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, Iterator, Mapping, Optional, Union

from ._transport import opener
from ._stream import responses, set_read_timeout
from ._ops import OPS, _Operations
from ._types import CommandResponse, CommandStartedResponse, CommandStreamFrame
from ._util import UNSET, Model, Obj, to_camel
from .errors import APIConnectionError, ApiError, make_api_error

DEFAULT_BASE_URL = "http://127.0.0.1:8420/api/v1"
VERSION = "0.1.0"
_NO_RETRY_429 = {"limit_reached", "member_limit_reached", "trial_compute_limit_reached"}


def _normalize_base(url: str) -> str:
    url = url.strip().rstrip("/")
    parts = urllib.parse.urlsplit(url)
    if parts.scheme and parts.netloc and parts.path in ("", "/"):
        url += "/api/v1"
    return url


def _seg(name: str, value: Any) -> str:
    """One percent-encoded path segment; `.` / `..` / empty would change the endpoint, so they are refused."""
    s = str(value)
    if s in ("", ".", ".."):
        raise ValueError(f"invalid path parameter {name!r}: {s!r} is not allowed")
    return urllib.parse.quote(s, safe="")


def _enc_query(query: Mapping[str, Any]) -> str:
    pairs = []
    for k, v in query.items():
        if v is None or v is UNSET:
            continue
        if isinstance(v, bool):
            v = "true" if v else "false"
        elif isinstance(v, (list, tuple, set)):
            if k == "label":
                pairs.extend((k, str(x)) for x in v)
                continue
            v = ",".join(str(x) for x in v)
        pairs.append((k, str(v)))
    return urllib.parse.urlencode(pairs)


class CrowdVM(_Operations):
    """Client for the CrowdVM HTTP API (a Boat v1 compatible surface).

    >>> cvm = CrowdVM()  # CROWDVM_API_KEY / CROWDVM_API_BASE from the environment
    >>> sb = cvm.create(ttl_seconds=1800).sandbox
    >>> cvm.command(sb.id, "echo hi").stdout

    Every operation of the API is a method named after Boat's SDK
    (``create``, ``get``, ``command_status``, ``read_file``, ...).  Path
    parameters are positional; everything else is a keyword argument in
    snake_case (``ttl_seconds=``).  `from` is spelled ``var_from``.
    Extra keyword arguments are sent as-is in the request body.
    """

    def __init__(self, api_key: Optional[str] = None, base_url: Optional[str] = None, *, timeout: float = 60.0,
                 max_retries: int = 3, max_retry_delay: float = 60.0, stream_idle_timeout: float = 90.0, headers: Optional[Mapping[str, str]] = None,
                 user_agent: Optional[str] = None):
        self.api_key = api_key if api_key is not None else os.environ.get("CROWDVM_API_KEY", "")
        self.base_url = _normalize_base(base_url or os.environ.get("CROWDVM_API_BASE") or DEFAULT_BASE_URL)
        self.timeout = timeout
        self.max_retries = max_retries
        self.max_retry_delay = max_retry_delay
        #: read timeout of `attach_command` streams (the server sends a heartbeat every 15 s)
        self.stream_idle_timeout = stream_idle_timeout
        self.headers = dict(headers or {})
        self.user_agent = user_agent or f"crowdvm-python/{VERSION}"
        self._sleep = time.sleep

    # context-manager sugar (nothing to close; kept so `with CrowdVM() as cvm:` works)
    def __enter__(self) -> "CrowdVM":
        return self

    def __exit__(self, *exc: Any) -> None:
        return None

    def with_options(self, **options: Any) -> "CrowdVM":
        """A copy of this client with some settings changed (``max_retries=10``, ``timeout=...``)."""
        clone = copy.copy(self)
        for k, v in options.items():
            if not hasattr(clone, k):
                raise TypeError(f"unknown option {k!r}")
            setattr(clone, k, _normalize_base(v) if k == "base_url" else v)
        return clone

    # ------------------------------------------------------------------ transport
    def _build(self, method: str, path: str, query: Optional[Mapping[str, Any]], body: Any,
               headers: Optional[Mapping[str, str]]) -> urllib.request.Request:
        url = self.base_url + path
        qs = _enc_query(query or {})
        if qs:
            url += "?" + qs
        h = {"Accept": "application/json", "User-Agent": self.user_agent}
        if self.api_key:
            h["Authorization"] = f"Bearer {self.api_key}"
        h.update(self.headers)
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            h["Content-Type"] = "application/json"
        for k, v in (headers or {}).items():
            if v is not None:
                h[k] = v
        return urllib.request.Request(url, data=data, method=method, headers=h)

    def _open(self, req: urllib.request.Request, timeout: Optional[float]) -> Any:
        """Send once.  Raises ApiError for non-2xx and APIConnectionError for network failures."""
        try:
            return opener.open(req, timeout=timeout)
        except urllib.error.HTTPError as e:
            raw = e.read().decode("utf-8", "replace")
            try:
                payload = json.loads(raw)
            except ValueError:
                payload = None
            err = make_api_error(e.code, raw, payload, dict(e.headers.items()) if e.headers else {})
            e.close()
            raise err from None
        except (urllib.error.URLError, OSError, http.client.HTTPException) as e:
            raise APIConnectionError(f"{req.get_method()} {req.full_url}: {e}") from e

    def _retry_delay(self, err: ApiError, attempt: int) -> Optional[float]:
        """Seconds to wait before retrying, or None when retrying cannot help."""
        ra = err.retry_after_seconds
        backoff = min(self.max_retry_delay, 2.0 ** attempt) * (0.5 + random.random() / 2)
        if err.status == 429:
            if err.code in _NO_RETRY_429:
                return None
            if err.code == "rate_limited" and err.details.get("window") in ("hour", "day"):
                return None  # a retry loop does not help against an hour or day limit
            return ra if ra is not None else backoff
        if err.status == 503:
            if err.code == "no_ready_machine":
                return None  # failFast: the caller asked not to wait
            return ra if ra is not None else backoff
        if err.status == 502 and err.code == "sandbox_direct_failed" and (
                err.message == "sandbox_restoring" or err.details.get("message") == "sandbox_restoring"):
            return ra if ra is not None else 2.0  # the command provably did not run
        if err.status == 409 and err.code == "sandbox_starting":
            return ra if ra is not None else 3.0
        if err.status == 409 and err.code == "idempotency_in_progress":
            return ra if ra is not None else 1.0
        return None

    def _send(self, method: str, path: str, *, query: Optional[Mapping[str, Any]] = None, body: Any = None,
              headers: Optional[Mapping[str, str]] = None, timeout: Any = UNSET) -> Any:
        """Send with retries; returns the open response (caller closes it)."""
        req = self._build(method, path, query, body, headers)
        to = self.timeout if timeout is UNSET else timeout
        attempt = 0
        while True:
            try:
                return self._open(req, to)
            except ApiError as e:
                delay = self._retry_delay(e, attempt)
                if delay is None or attempt >= self.max_retries:
                    raise
            except APIConnectionError:
                if method != "GET" or attempt >= self.max_retries:
                    raise
                delay = min(self.max_retry_delay, 2.0 ** attempt) * (0.5 + random.random() / 2)
            self._sleep(min(delay, self.max_retry_delay))
            attempt += 1

    def request(self, method: str, path: str, *, query: Optional[Mapping[str, Any]] = None, body: Any = None,
                headers: Optional[Mapping[str, str]] = None, timeout: Any = UNSET) -> Any:
        """Raw escape hatch: ``cvm.request("GET", "/me")`` returns the decoded JSON (an `Obj`)."""
        resp = self._send(method.upper(), path, query=query, body=body, headers=headers, timeout=timeout)
        return self._decode(resp)

    @staticmethod
    def _read(resp: Any) -> bytes:
        """Read the whole body; a truncated body or a read timeout is an APIConnectionError."""
        try:
            return resp.read()
        except (OSError, http.client.HTTPException) as e:
            raise APIConnectionError(f"reading the response failed: {e}") from e

    @staticmethod
    def _decode(resp: Any) -> Any:
        with resp:
            if resp.status == 304:
                return None
            raw = CrowdVM._read(resp)
        if not raw.strip():
            return Obj()
        try:
            return json.loads(raw, object_hook=Obj)
        except ValueError as e:
            raise APIConnectionError(f"invalid JSON in response: {raw[:200]!r}") from e

    def _lines(self, method: str, path: str, *, query: Optional[Mapping[str, Any]] = None, body: Any = None,
               headers: Optional[Mapping[str, str]] = None, timeout: Any = None, read_timeout: Any = UNSET) -> Iterator[Obj]:
        """Yield the NDJSON lines of a streaming response. ``timeout`` covers connecting and the headers,
        ``read_timeout`` (default: the same) every read after that."""
        h = dict(headers or {})
        h["Accept"] = "application/x-ndjson"
        resp = self._send(method, path, query=query, body=body, headers=h, timeout=timeout)
        if read_timeout is not UNSET and read_timeout != timeout:
            set_read_timeout(resp, read_timeout)
        active = responses.get()
        if active is not None:
            active.append(resp)
        try:
            while True:
                try:
                    line = resp.readline()
                except (OSError, http.client.HTTPException, socket.timeout) as e:
                    raise APIConnectionError(f"stream interrupted: {e}") from e
                if not line:
                    raise APIConnectionError("stream ended before a terminal frame")
                line = line.strip()
                if not line:
                    continue
                try:
                    frame = json.loads(line, object_hook=Obj)
                    yield frame
                    if frame.get("type") in ("exit", "error"):
                        return
                except ValueError:
                    continue
        finally:
            if active is not None:
                active.remove(resp)
            resp.close()

    # ------------------------------------------------------------------ generated ops
    def _op(self, name: str, args: Dict[str, Any]) -> Any:
        spec = OPS[name]
        extra = args.pop("extra", {}) or {}
        path = spec["path"]
        for py, wire in spec["path_params"].items():
            path = path.replace("{" + wire + "}", _seg(py, args[py]))
        query = {wire: args[py] for py, wire in spec["query"].items() if args[py] is not UNSET}
        headers = {}
        for py, wire in spec["headers"].items():
            v = args[py]
            if v is not None:
                headers[wire] = v
        if "confirm_default" in spec and not any(h.lower().endswith("confirm-delete") for h in headers):
            headers["X-Ascii-Confirm-Delete"] = headers["X-CrowdVM-Confirm-Delete"] = args[spec["confirm_default"]]
        body: Any = None
        if spec["body"] != "none":
            wrapped = extra.pop(spec.get("body_param"), None)  # Boat style: create(create_sandbox_request=...)
            req = spec["required_body"]
            if req and isinstance(args[req[0]], Model) and wrapped is None:
                # Boat style: host_port(sandbox_id, HostPortRequest(port=8080)) -- the model sits in the first slot
                wrapped, args[req[0]] = args[req[0]], UNSET
            body = {wire: args[py] for py, wire in spec["body_fields"].items() if args[py] is not UNSET}
            if wrapped:
                body.update({to_camel(k): v for k, v in dict(wrapped).items()})
            body.update(extra)
            missing = [py for py in req if spec["body_fields"][py] not in body]
            if missing:
                raise TypeError(f"{name}() missing required argument(s): {', '.join(missing)}")
        elif extra:
            raise TypeError(f"{name}() got unexpected keyword arguments: {', '.join(extra)}")
        missing = [py for py in spec.get("required_query", ()) if args[py] is UNSET]
        if missing:
            raise TypeError(f"{name}() missing required argument(s): {', '.join(missing)}")
        if spec["ret"] == "ndjson":
            return self._lines(spec["method"], path, query=query, body=body, headers=headers,
                               timeout=self.stream_idle_timeout)
        try:
            resp = self._send(spec["method"], path, query=query, body=body, headers=headers)
        except ApiError as e:
            if e.status == 304:  # If-None-Match matched
                return None
            raise
        if spec["ret"] == "bytes":
            with resp:
                return self._read(resp)
        return self._decode(resp)

    # ------------------------------------------------------------------ commands
    def command(self, sandbox_id: str, command: Union[str, Mapping[str, Any]] = UNSET, *, cwd: Optional[str] = UNSET, timeout_seconds: int = UNSET,
                detached: bool = UNSET, stream: bool = UNSET, command_request: Optional[Mapping[str, Any]] = None,
                **extra: Any) -> Union[CommandResponse, CommandStartedResponse, Iterator[CommandStreamFrame]]:
        """Run a command (``POST /sandboxes/{id}/commands``).

        Returns a ``CommandResponse``; with ``detached=True`` a ``CommandStartedResponse``;
        with ``stream=True`` an iterator of NDJSON frames (see also `crowdvm.stream_command`).
        """
        # Boat style: command(sandbox_id, CommandRequest(command="..."))
        if command is UNSET:
            command = command_request if command_request is not None else UNSET
        if command is UNSET:
            raise TypeError("command() missing required argument: 'command'")
        body: Dict[str, Any] = {to_camel(k): v for k, v in command.items()} if isinstance(command, Mapping) else {"command": command}
        for k, v in (("cwd", cwd), ("timeoutSeconds", timeout_seconds), ("detached", detached), ("stream", stream)):
            if v is not UNSET:
                body[k] = v
        body.update(extra)
        path = f"/sandboxes/{_seg('sandbox_id', sandbox_id)}/commands"
        if body.get("stream") and not body.get("detached"):
            return self._lines("POST", path, body=body, timeout=self.timeout, read_timeout=None)
        # the HTTP read must outlive the command (<= 600 s) plus the 60 s start wait
        timeout = float(body.get("timeoutSeconds") or 30) + 90.0
        if self.timeout is not None:
            timeout = max(self.timeout, timeout)
        return self._decode(self._send("POST", path, body=body, timeout=timeout))
