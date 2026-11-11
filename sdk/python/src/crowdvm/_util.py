"""Small shared pieces: the UNSET sentinel and `Obj`, a dict with attribute access."""
from __future__ import annotations

import json
from typing import Any, Mapping, Optional


class _Unset:
    """Marks a keyword argument that was not passed (so `None` can mean JSON null)."""

    _inst = None

    def __new__(cls):
        if cls._inst is None:
            cls._inst = super().__new__(cls)
        return cls._inst

    def __repr__(self) -> str:
        return "UNSET"

    def __bool__(self) -> bool:
        return False


UNSET: Any = _Unset()


def to_camel(name: str) -> str:
    if name == "var_from":
        return "from"
    name = name.rstrip("_")
    head, *rest = name.split("_")
    return head + "".join(p[:1].upper() + p[1:] for p in rest)


def _norm(name: str) -> str:
    return name.replace("_", "").lower()


class Obj(dict):
    """A JSON object that also allows attribute access.

    ``out.exit_code``, ``out.exitCode`` and ``out["exitCode"]`` all work, as do
    ``info.sandbox.memory_gb`` / ``memoryGB``.  Nested objects are `Obj` too.
    Keys that clash with `dict` methods (``items``, ``keys``, ``values``, ...)
    must be read with ``obj["items"]``.
    """

    __slots__ = ()

    def __getattr__(self, name: str) -> Any:
        if name.startswith("__"):
            raise AttributeError(name)
        if name in self:
            return self[name]
        camel = to_camel(name)
        if camel in self:
            return self[camel]
        want = _norm(name)
        for key in self:
            if isinstance(key, str) and _norm(key) == want:
                return self[key]
        raise AttributeError(name)

    @property
    def actual_instance(self) -> "Obj":
        """Compatibility with generated Boat clients: the response itself."""
        return self

    def to_dict(self) -> dict:
        return _plain(self)


def _plain(v: Any) -> Any:
    if isinstance(v, dict):
        return {k: _plain(x) for k, x in v.items()}
    if isinstance(v, list):
        return [_plain(x) for x in v]
    return v




class Model(Obj):
    """A request model in Boat SDK style: ``CreateSandboxRequest(ttl_seconds=1800, var_from="snap")``.

    It is the JSON body itself (camelCase keys).  Fields passed explicitly as ``None`` are sent as
    JSON ``null`` (``ttl_seconds=None`` means "no auto-stop"), like Boat's generated models.
    """

    __slots__ = ()

    def __init__(self, **fields: Any):
        super().__init__({to_camel(k): v for k, v in fields.items() if v is not UNSET})

    @classmethod
    def from_dict(cls, obj: Optional[Mapping[str, Any]]) -> Any:
        if obj is None:
            return None
        m = cls()
        m.update(obj)
        return m

    @classmethod
    def from_json(cls, text: str) -> Any:
        return cls.from_dict(json.loads(text))

    def to_json(self) -> str:
        return json.dumps(_plain(self))

    def to_str(self) -> str:
        return repr(self)
