"""Request models with Boat SDK spelling: ``CreateSandboxRequest(ttl_seconds=1800, var_from="snap")``.

Every schema name of the API works (``from crowdvm.models import HostPortRequest``).  An instance is
the JSON body itself (a dict with camelCase keys and attribute access) and has ``from_dict``,
``to_dict`` and ``to_json`` like Boat's generated models.  Pass it where Boat's docs do:
``host_port(sandbox_id, HostPortRequest(port=8080))`` or ``create(create_sandbox_request=...)``.
"""
from __future__ import annotations

from typing import Any, Dict, Type

from . import _types
from ._util import Model

__all__ = ["Model"]
_classes: Dict[str, Type[Model]] = {}


def __getattr__(name: str) -> Type[Model]:
    if name.startswith("_") or not name[:1].isupper() or not (hasattr(_types, name) or name.endswith("Request")):
        raise AttributeError(name)
    if name not in _classes:
        _classes[name] = type(name, (Model,), {"__slots__": (), "__module__": __name__})
    return _classes[name]
