"""Helpers that combine several API calls (mirrors the helpers of Boat's SDK).

Each takes the client as its first argument, e.g. ``run_command(cvm, sandbox_id, "make")``.
"""
from __future__ import annotations

import re
import time
from collections.abc import Mapping
from typing import Any, Callable, Dict, Iterator, List, Optional, Set

from ._stream import cancelled
from ._util import UNSET, Obj
from .client import CrowdVM
from .errors import APIConnectionError, ApiError, PromptFailed, SandboxFailed, WaitTimeout

READY_STATES = ("ready", "idle", "running")


def _snake(name: str) -> str:
    return "var_from" if name == "from" else re.sub(r"(?<=[a-z0-9])([A-Z])", r"_\1", name).lower()


FAILED_STATES = ("error", "cancelled")
PROMPT_TERMINAL = ("finished", "failed", "interrupted")


def _poll(fn: Callable[[], Any], interval: float, timeout: Optional[float], what: str) -> Any:
    """Call ``fn`` until it returns something other than UNSET."""
    deadline = None if timeout is None else time.monotonic() + timeout
    stop = cancelled.get()
    while True:
        if stop is not None and stop.is_set():
            raise InterruptedError("cancelled")
        out = fn()
        if out is not UNSET:
            return out
        if deadline is not None and time.monotonic() + interval > deadline:
            raise WaitTimeout(f"timed out waiting for {what}")
        if stop is not None:
            if stop.wait(interval):
                raise InterruptedError("cancelled")
        else:
            time.sleep(interval)


# ------------------------------------------------------------------ waiting
def _wait_state(client: CrowdVM, sandbox_id: str, states: Any, timeout: Optional[float], interval: float,
                what: str) -> Obj:
    """Wait for one of ``states``. A node restart shows as `error` with ``node_offline`` for a few
    seconds and the sandbox comes back: that is waited out; at the timeout it fails with that error."""
    offline: List[Any] = []

    def check() -> Any:
        sb = client.get(sandbox_id)["sandbox"]
        if sb["state"] in states:
            return sb
        offline[:] = []
        if sb["state"] in FAILED_STATES:
            if sb["state"] != "error" or not str(sb.get("error") or "").startswith("node_offline"):
                raise SandboxFailed(sb)
            offline.append(sb)
        return UNSET
    try:
        return _poll(check, interval, timeout, what)
    except WaitTimeout:
        if offline:
            raise SandboxFailed(offline[0]) from None
        raise


def wait_until_ready(client: CrowdVM, sandbox_id: str, *, timeout: Optional[float] = 300.0,
                     interval: float = 2.0) -> Obj:
    """Wait until the sandbox is `ready`, `idle` or `running`; return the sandbox.

    Raises `SandboxFailed` when it enters `error` / `cancelled`, `WaitTimeout` on timeout.
    An `error` of `node_offline` (the node restarts) is waited out and only fails at the timeout.
    """
    return _wait_state(client, sandbox_id, READY_STATES, timeout, interval, f"sandbox {sandbox_id} to be ready")


def wait_until_idle(client: CrowdVM, sandbox_id: str, *, timeout: Optional[float] = 600.0,
                    interval: float = 2.0) -> Obj:
    """Wait until the sandbox is `ready` or `idle` (no integrated-agent turn is running)."""
    return _wait_state(client, sandbox_id, ("ready", "idle"), timeout, interval, f"sandbox {sandbox_id} to be idle")


def wait_for_desktop(client: CrowdVM, sandbox_id: str, *, timeout: Optional[float] = 120.0, interval: float = 2.0,
                     **desktop_options: Any) -> Obj:
    """Ask for the VNC desktop URL until the desktop is prepared; return the `DesktopResponse`."""
    desktop_options.setdefault("vnc", 1)

    def check() -> Any:
        try:
            out = client.desktop(sandbox_id, **desktop_options)
        except ApiError as e:
            # machine_not_running / starting / browser_not_ready. A 400 (bad
            # theme or profile) never becomes a desktop (bugs-r16-sonnet 10).
            if e.status in (409, 503):
                return UNSET
            raise
        if out.get("provisioning") or not out.get("desktopUrl"):
            return UNSET
        return out
    return _poll(check, interval, timeout, f"the desktop of {sandbox_id}")


def wait_for_prompt(client: CrowdVM, sandbox_id: str, prompt_id: str, *, timeout: Optional[float] = None,
                    interval: float = 2.0) -> Obj:
    """Wait until a prompt run is done; return its `PromptRun` (read ``.status``)."""
    def check() -> Any:
        run = client.prompt_run_status(sandbox_id, prompt_id)["promptRun"]
        return run if run.get("done") or run.get("status") in PROMPT_TERMINAL else UNSET
    return _poll(check, interval, timeout, f"prompt {prompt_id}")


def wait_for_prompt_done(client: CrowdVM, sandbox_id: str, prompt_id: str, *, timeout: Optional[float] = None,
                         interval: float = 2.0) -> Obj:
    """Like `wait_for_prompt`, but raises `PromptFailed` unless the run `finished`."""
    run = wait_for_prompt(client, sandbox_id, prompt_id, timeout=timeout, interval=interval)
    if run.get("status") != "finished":
        raise PromptFailed(run)
    return run


# ------------------------------------------------------------------ files
def read_text(client: CrowdVM, sandbox_id: str, path: str) -> str:
    """Read a UTF-8 text file (path relative to ``/home/user``)."""
    return client.read_file(sandbox_id, path, encoding="utf8")["content"]


def write_text(client: CrowdVM, sandbox_id: str, path: str, content: str) -> Obj:
    """Write a UTF-8 text file (path relative to ``/home/user``)."""
    return client.write_file(sandbox_id, path, content, encoding="utf8")


# ------------------------------------------------------------------ commands
def exec_command(client: CrowdVM, sandbox_id: str, command: str, *, cwd: Optional[str] = None,
                 timeout_seconds: Optional[int] = None, detached: bool = False) -> Obj:
    """Run a command and wait for it.  Resends it while the sandbox restores its disk
    (the command provably did not run) and while the sandbox is still starting."""
    kw: Dict[str, Any] = {}
    if cwd is not None:
        kw["cwd"] = cwd
    if timeout_seconds is not None:
        kw["timeout_seconds"] = timeout_seconds
    if detached:
        kw["detached"] = True
    return client.with_options(max_retries=max(client.max_retries, 20)).command(sandbox_id, command, **kw)


def stream_command(client: CrowdVM, sandbox_id: str, command: str, *, cwd: Optional[str] = None,
                   timeout_seconds: Optional[int] = None) -> Iterator[Obj]:
    """Yield the NDJSON frames of a command (`started`, `stdout`/`stderr`, then `exit` or `error`).
    Limited to ``timeout_seconds`` (<= 600); use `run_command` for longer jobs."""
    kw: Dict[str, Any] = {"stream": True}
    if cwd is not None:
        kw["cwd"] = cwd
    if timeout_seconds is not None:
        kw["timeout_seconds"] = timeout_seconds
    yield from client.command(sandbox_id, command, **kw)


def follow_command(client: CrowdVM, sandbox_id: str, process_id: int, *, stdout_offset: int = 0,
                   stderr_offset: int = 0, max_reconnects: int = 20) -> Iterator[Obj]:
    """Follow a detached command until it ends.  Yields `stdout` / `stderr` frames, then one
    `exit` (or `error`) frame.  Reconnects by itself, with no gap and no repeat."""
    so, se = stdout_offset, stderr_offset
    failures = 0
    while True:
        try:
            for frame in client.attach_command(sandbox_id, process_id, stdout_offset=so, stderr_offset=se):
                t = frame.get("type")
                if t in ("stdout", "stderr"):
                    nxt = frame.get("nextOffset")
                    if nxt is not None:
                        if t == "stdout":
                            so = nxt
                        else:
                            se = nxt
                    failures = 0
                    yield frame
                elif t == "attached":
                    so = frame.get("stdoutOffset", so)
                    se = frame.get("stderrOffset", se)
                elif t == "reconnect":
                    so = frame.get("stdoutOffset", so)
                    se = frame.get("stderrOffset", se)
                    break
                elif t in ("exit", "error"):
                    yield frame
                    return
            else:
                raise APIConnectionError("stream closed before the command ended")
        except APIConnectionError:
            failures += 1
            if failures > max_reconnects:
                raise
            time.sleep(min(5.0, 0.1 * 2 ** min(failures - 1, 6)))


def run_command(client: CrowdVM, sandbox_id: str, command: str, *, cwd: Optional[str] = None) -> Iterator[Obj]:
    """Run a command with no time limit: start it detached, then follow it.

    The first frame is ``{"type": "started", "processId": ...}``; then `stdout` / `stderr`
    frames; the last is `exit` (with ``exitCode``) or `error`.  Breaking out of the loop
    leaves the command running: stop it with ``client.stop_command(sandbox_id, process_id)``."""
    started = exec_command(client, sandbox_id, command, cwd=cwd, detached=True)
    yield Obj(type="started", processId=started["processId"], commandId=started.get("commandId"))
    yield from follow_command(client, sandbox_id, started["processId"])


# ------------------------------------------------------------------ events and prompts
class _EventPoller:
    """Polls `GET /events`, yielding each event once (the endpoint is not a stream)."""

    def __init__(self, client: CrowdVM, sandbox_id: str, type: Optional[str], conversation: Optional[str]):
        self.client, self.sandbox_id = client, sandbox_id
        self.query: Dict[str, Any] = {}
        if type:
            self.query["type"] = type if isinstance(type, str) else ",".join(type)
        if conversation:
            self.query["conversation"] = conversation
        self.seen: Set[str] = set()
        self.primed = False

    def _fetch(self) -> List[Obj]:
        out: List[Obj] = []
        cursor: Any = UNSET
        for _ in range(20):  # pages, newest first
            res = self.client.events(self.sandbox_id, limit=200, sort="desc", cursor=cursor, **self.query)
            page = res.get("events", [])
            fresh = [e for e in page if e.get("id") not in self.seen]
            out.extend(fresh)
            info = res.get("pageInfo") or {}
            if not info.get("hasMore") or not info.get("nextCursor") or (self.primed and not fresh):
                break
            cursor = info["nextCursor"]
        out.sort(key=lambda e: e.get("timestamp") or 0)
        return out

    def seed(self) -> None:
        for e in self._fetch():
            if e.get("id") is not None:
                self.seen.add(e["id"])
        self.primed = True

    def poll(self) -> List[Obj]:
        new = self._fetch()
        for e in new:
            if e.get("id") is not None:
                self.seen.add(e["id"])
        self.primed = True
        return new


def stream_events(client: CrowdVM, sandbox_id: str, *, type: Optional[str] = None, conversation: Optional[str] = None,
                  interval: float = 1.0, replay: bool = False,
                  stop: Optional[Callable[[], bool]] = None) -> Iterator[Obj]:
    """Follow the events of a sandbox (polls `GET /events`).  Yields only new events unless
    ``replay=True``.  Ends when ``stop()`` returns true or the generator is closed."""
    poller = _EventPoller(client, sandbox_id, type, conversation)
    if not replay:
        poller.seed()
    while not (stop and stop()):
        yield from poller.poll()
        time.sleep(interval)


def stream_prompt(client: CrowdVM, sandbox_id: str, prompt_request: Any = None, *, prompt: Optional[str] = None,
                  interval: float = 1.0, **options: Any) -> Iterator[Obj]:
    """Queue a prompt and yield the events of its conversation until the prompt is done.

    Pass a ``PromptRequest`` (Boat style) or ``prompt=`` plus the arguments of ``client.prompt``
    (``provider=``, ``model=``, ``new=``...)."""
    if isinstance(prompt_request, Mapping):
        options = {**{_snake(k): v for k, v in prompt_request.items()}, **options}
    elif prompt_request is not None:
        prompt = prompt_request
    prompt = options.pop("prompt", prompt)
    if prompt is None:
        raise TypeError("stream_prompt() needs a PromptRequest or prompt=")
    poller = _EventPoller(client, sandbox_id, options.pop("type", None), options.get("conversation_id"))
    poller.seed()
    queued = client.prompt(sandbox_id, prompt, **options)
    conv = queued.get("conversationId")
    if conv:
        poller.query["conversation"] = conv
    prompt_id = queued["promptId"]
    while True:
        done = client.prompt_run_status(sandbox_id, prompt_id)["promptRun"].get("done")
        yield from poller.poll()
        if done:
            return
        time.sleep(interval)


def stop_and_remove(client: CrowdVM, sandbox_id: str, *, timeout: Optional[float] = 300.0, interval: float = 2.0,
                    force: bool = False) -> Obj:
    """Stop the sandbox, wait until it is archived, then delete it and its snapshots."""
    try:
        client.stop(sandbox_id, force=True) if force else client.stop(sandbox_id)
    except ApiError as e:
        if e.status != 409:
            raise

    def check() -> Any:
        sb = client.get(sandbox_id)["sandbox"]
        return sb if sb["state"] in ("archived", "error", "cancelled") else UNSET
    _poll(check, interval, timeout, f"sandbox {sandbox_id} to stop")
    return client.delete_sandbox(sandbox_id)


# ------------------------------------------------------------------ pagination
def paginate(fn: Callable[..., Any], key: str, *args: Any, **kwargs: Any) -> Iterator[Obj]:
    """Yield every item of a cursor-paginated list: ``paginate(cvm.sandboxes, "sandboxes", state="running")``."""
    cursor: Any = UNSET
    while True:
        res = fn(*args, cursor=cursor, **kwargs)
        yield from res.get(key, [])
        nxt = (res.get("pageInfo") or {}).get("nextCursor") or res.get("nextCursor")
        more = (res.get("pageInfo") or {}).get("hasMore", bool(nxt))
        if not (more and nxt):
            return
        cursor = nxt
