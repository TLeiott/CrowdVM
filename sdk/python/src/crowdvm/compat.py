"""Boat-SDK style entry points: ``Configuration``, ``ApiClient``, ``BoatApi`` and the ``models`` namespace.

Code written for ``boat_sdk`` runs against CrowdVM unchanged: this distribution also installs a
``boat_sdk`` package (``boat_sdk.api.boat_api``, ``boat_sdk.models``, ``boat_sdk.exceptions``, ...)
that maps to these classes.  Only the host changes::

    from boat_sdk import ApiClient, Configuration, wait_until_ready
    from boat_sdk.api.boat_api import BoatApi
    from boat_sdk.models import CommandRequest, CreateSandboxRequest

    with ApiClient(Configuration(host="http://192.168.1.10:8420/api/v1", access_token="cvm_...")) as client:
        boat = BoatApi(client)
        sb = boat.create(create_sandbox_request=CreateSandboxRequest(ttl_seconds=1800)).sandbox
        wait_until_ready(boat, sb.id)
        out = boat.command(sb.id, CommandRequest(command="echo hi")).actual_instance

``BoatApi`` methods take their positional arguments in the order of Boat's generated SDK
(``delete_sandbox(x_ascii_confirm_delete, sandbox_id)``, ``host_port(sandbox_id, host_port_request)``).
A call that does not fit that order (``create_webhook(url, events)``) runs with CrowdVM's own order.
"""
from __future__ import annotations

import functools
import os
from collections.abc import Mapping
from typing import Any, Callable, Optional

from ._ops import BOAT_SIGNATURES
from .client import CrowdVM


class Configuration:
    def __init__(self, host: Optional[str] = None, access_token: Optional[str] = None, api_key: Any = None,
                 **_ignored: Any):
        if isinstance(api_key, Mapping):  # openapi-generator style: api_key={"bearerAuth": "..."}
            api_key = next(iter(api_key.values()), None)
        self.host = host
        self.access_token = access_token or api_key


class ApiClient:
    def __init__(self, configuration: Optional[Configuration] = None, *_args: Any, **_kwargs: Any):
        self.configuration = configuration or Configuration()

    def __enter__(self) -> "ApiClient":
        return self

    def __exit__(self, *exc: Any) -> None:
        return None

    def close(self) -> None:
        return None


def _boat_method(name: str, sig: list) -> Callable[..., Any]:
    native = getattr(CrowdVM, name)
    names = [n for n, _ in sig]
    required = [n for n, req in sig if req]

    @functools.wraps(native)
    def method(self: CrowdVM, *args: Any, **kwargs: Any) -> Any:
        if not args or len(args) > len(names):
            return native(self, *args, **kwargs)
        bound = dict(zip(names, args))
        fits = not (bound.keys() & kwargs.keys()) and all(
            isinstance(v, Mapping) for n, v in bound.items() if n.endswith("_request"))
        if fits:
            merged = {**kwargs, **bound}
            fits = all(n in merged for n in required if not n.endswith("_request"))
        return native(self, **merged) if fits else native(self, *args, **kwargs)

    return method


class BoatApi(CrowdVM):
    """`CrowdVM` that is built from an `ApiClient` like Boat's generated ``BoatApi``."""

    def __init__(self, api_client: Optional[ApiClient] = None, **options: Any):
        cfg = (api_client or ApiClient()).configuration
        api_key = options.pop("api_key", None) or cfg.access_token or os.environ.get("BOAT_API_KEY")
        super().__init__(api_key, options.pop("base_url", None) or cfg.host or os.environ.get("BOAT_API_BASE"), **options)


for _name, _sig in BOAT_SIGNATURES.items():
    if _sig and hasattr(CrowdVM, _name):
        setattr(BoatApi, _name, _boat_method(_name, _sig))
