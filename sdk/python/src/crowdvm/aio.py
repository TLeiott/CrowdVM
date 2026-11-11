"""An asyncio facade: every blocking call runs in a thread (``asyncio.to_thread``)."""
from __future__ import annotations

import asyncio
import functools
import inspect
import threading
from typing import Any, AsyncIterator, Optional

from . import helpers
from .client import CrowdVM
from ._stream import cancelled, responses, interrupt

_HELPERS = {n for n in dir(helpers) if not n.startswith("_") and inspect.isfunction(getattr(helpers, n))
            and next(iter(inspect.signature(getattr(helpers, n)).parameters), "") in ("client", "fn")} - {"paginate"}
_DONE = object()


async def _agen(gen: Any) -> AsyncIterator[Any]:
    active = []
    token = responses.set(active)
    pending = None
    try:
        while True:
            pending = asyncio.create_task(asyncio.to_thread(next, gen, _DONE))
            item = await asyncio.shield(pending)
            if item is _DONE:
                return
            yield item
    finally:
        for resp in list(active):
            interrupt(resp)
        if pending is not None:
            try:
                await pending
            except Exception:
                pass
        gen.close()
        responses.reset(token)


class AsyncCrowdVM:
    """``await cvm.create(...)``, ``async for frame in cvm.run_command(...)``.

    Same methods and arguments as :class:`CrowdVM`, plus the helpers (``wait_until_ready``,
    ``exec_command``, ``run_command``, ...) as methods that omit the client argument."""

    def __init__(self, *args: Any, client: Optional[CrowdVM] = None, **kwargs: Any):
        self.client = client or CrowdVM(*args, **kwargs)

    def __getattr__(self, name: str) -> Any:
        if name == "client" or name.startswith("__"):  # copy / pickle probe an instance that has no state yet
            raise AttributeError(name)
        if name in _HELPERS:
            target = functools.partial(getattr(helpers, name), self.client)
        else:
            target = getattr(self.client, name)
        if not callable(target):
            return target

        def call(*a: Any, **kw: Any) -> Any:
            if inspect.isgeneratorfunction(getattr(target, "func", target)) or name in ("attach_command",):
                return _agen(target(*a, **kw))

            async def run() -> Any:
                stop = threading.Event()
                token = cancelled.set(stop)  # copied into the worker thread: polling helpers stop when we are cancelled
                try:
                    out = await asyncio.to_thread(target, *a, **kw)
                except asyncio.CancelledError:
                    stop.set()
                    raise
                finally:
                    cancelled.reset(token)
                if inspect.isgenerator(out):  # e.g. command(stream=True)
                    return _agen(out)
                return out
            return run()
        return call

    def with_options(self, **kwargs: Any) -> "AsyncCrowdVM":
        return AsyncCrowdVM(client=self.client.with_options(**kwargs))

    async def __aenter__(self) -> "AsyncCrowdVM":
        return self

    async def __aexit__(self, *exc: Any) -> None:
        return None
