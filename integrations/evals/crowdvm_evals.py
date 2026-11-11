"""Eval / RL helpers for CrowdVM: one saved snapshot per environment version, one fresh sandbox per episode.

The pattern of Boat's "Evals and RL environments" guide as a small library (stdlib + the `crowdvm` SDK)::

    from crowdvm import CrowdVM
    from crowdvm_evals import prepare_template, run_episodes

    cvm = CrowdVM()
    prepare_template(cvm, "task-env-v1", ["git clone https://github.com/acme/eval-tasks", "pip install -r eval-tasks/requirements.txt"])

    def episode(ep, n):                       # ep: Episode (a started sandbox that is deleted afterwards)
        out = ep.exec(f"python3 eval-tasks/run.py --episode {n}", timeout_seconds=600)
        return n, out.exit_code, out.stdout

    results = run_episodes(cvm, "task-env-v1", episode, range(100), max_workers=20, starts_per_minute=30)

Starts retry on 429 / 503 / capacity answers with the same `Idempotency-Key` (a retry never makes a second
sandbox); every episode sandbox carries a TTL and the label `crowdvm.eval=<run id>` so `cleanup(cvm, run_id)`
can delete what a crashed run left.
"""
from __future__ import annotations

import threading
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from typing import Any, Callable, Dict, Iterable, List, Optional, Sequence

from crowdvm import ApiError, CrowdVM, Obj, SandboxFailed, exec_command, paginate, wait_until_ready

__all__ = ["prepare_template", "start_sandbox", "Episode", "run_episodes", "cleanup", "episode_cost"]

LABEL = "crowdvm.eval"
_RETRY = (429, 503)


def _wait(fn: Callable[[], Any], timeout: float, interval: float, what: str) -> Any:
    deadline = time.monotonic() + timeout
    while True:
        out = fn()
        if out is not None:
            return out
        if time.monotonic() + interval > deadline:
            raise TimeoutError(f"timed out waiting for {what}")
        time.sleep(interval)


def prepare_template(cvm: CrowdVM, name: str, setup: Sequence[str] = (), *, type: str = "default", timeout: float = 900,
                     setup_timeout_seconds: int = 600, interval: float = 2.0, replace: bool = False, **create: Any) -> Obj:
    """Create a sandbox, run the ``setup`` commands, save it as the saved snapshot ``name`` and delete it.

    A saved snapshot never changes, so use one per environment version (``task-env-v1``, ``-v2``).  Raises when a
    setup command fails.  With ``replace=True`` an existing snapshot of that name is removed first."""
    if replace:
        try:
            cvm.delete_saved_snapshot(name)
        except ApiError as e:
            if e.status != 404:
                raise
    sb = start_sandbox(cvm, type=type, ttl_seconds=3600, labels={LABEL: f"template-{name}"}, **create)
    try:
        for cmd in setup:
            out = exec_command(cvm, sb, cmd, timeout_seconds=setup_timeout_seconds)
            if out.get("exitCode") or out.get("timedOut"):
                raise RuntimeError(f"setup command failed ({out.get('exitCode')}): {cmd}\n{out.get('stderr', '')[-500:]}")
        cvm.save_saved_snapshot(sb, name)

        def ready() -> Optional[Obj]:
            snap = cvm.get_saved_snapshot(name)["snapshot"]
            if snap.get("status") == "failed":
                raise RuntimeError(f"saving snapshot {name} failed: {snap}")
            return snap if snap.get("status") == "ready" else None
        return _wait(ready, timeout, interval, f"saved snapshot {name}")
    finally:
        cvm.delete_sandbox(sb, x_ascii_confirm_delete=sb)


def start_sandbox(cvm: CrowdVM, *, attempts: int = 6, wait: bool = True, wait_timeout: float = 600,
                  backoff: float = 5.0, **create: Any) -> str:
    """Create a sandbox (``create`` = the arguments of ``cvm.create``) and wait until it is ready; returns its id.

    Retries up to ``attempts`` times when the start is refused with 429 / 503 (honouring Retry-After; the
    idempotency key stays the same, so a retry never makes a second sandbox) or when the sandbox comes up in
    the ``error`` state because its node had no room at boot (``out_of_capacity``: the failed sandbox is deleted)."""
    key = create.pop("idempotency_key", None) or str(uuid.uuid4())
    for attempt in range(attempts):
        last = attempt == attempts - 1
        try:
            sid = cvm.create(idempotency_key=key, **create)["sandbox"]["id"]
        except ApiError as e:
            if e.status not in _RETRY or last:
                raise
            time.sleep(e.retry_after_seconds if e.retry_after_seconds is not None else min(60.0, 2 ** attempt * backoff))
            continue
        if not wait:
            return sid
        try:
            wait_until_ready(cvm, sid, timeout=wait_timeout)
            return sid
        except SandboxFailed as e:
            cvm.delete_sandbox(sid, x_ascii_confirm_delete=sid)
            if "out_of_capacity" not in str(e) or last:
                raise
            key = str(uuid.uuid4())  # the failed sandbox owns the old key
            time.sleep(min(60.0, 2 ** attempt * backoff))
        except BaseException:
            cvm.delete_sandbox(sid, x_ascii_confirm_delete=sid)
            raise
    raise RuntimeError("unreachable")


class Episode:
    """A sandbox for one episode.  Use as a context manager; the sandbox is deleted on exit (also on errors)."""

    def __init__(self, cvm: CrowdVM, template: Optional[str] = None, *, run_id: str = "adhoc", type: str = "small",
                 ttl_seconds: int = 3600, no_env: bool = True, labels: Optional[Dict[str, str]] = None, **create: Any):
        self.cvm, self.id = cvm, None
        self._args = dict(type=type, ttl_seconds=ttl_seconds, no_env=no_env, labels={LABEL: run_id, **(labels or {})}, **create)
        if template:
            self._args["var_from"] = template

    def __enter__(self) -> "Episode":
        self.id = start_sandbox(self.cvm, **self._args)
        return self

    def __exit__(self, *exc: Any) -> None:
        self.delete()

    def exec(self, command: str, *, cwd: Optional[str] = None, timeout_seconds: int = 600) -> Obj:
        """Run a command and wait (<= 600 s; start longer jobs with ``detached=True`` via ``cvm.command``)."""
        return exec_command(self.cvm, self.id, command, cwd=cwd, timeout_seconds=timeout_seconds)

    def fork(self, **options: Any) -> "Episode":
        """Branch this episode mid-way.  Stop the source first for an exact copy: a fork of a running sandbox
        uses its last background snapshot.  The returned episode is already started."""
        child = Episode.__new__(Episode)
        child.cvm = self.cvm
        child._args = {}
        sid = self.cvm.fork(self.id, idempotency_key=str(uuid.uuid4()), **options)["sandbox"]["id"]
        wait_until_ready(self.cvm, sid, timeout=600)
        child.id = sid
        return child

    def cost(self) -> Obj:
        return episode_cost(self.cvm, self.id)

    def delete(self) -> None:
        sid, self.id = self.id, None
        if sid:
            try:
                self.cvm.delete_sandbox(sid, x_ascii_confirm_delete=sid)
            except ApiError as e:
                if e.status != 404:
                    raise


def episode_cost(cvm: CrowdVM, sandbox_id: str) -> Obj:
    """Usage of a sandbox (``GET /sandboxes/{id}/usage``): run time and credits."""
    return cvm.usage(sandbox_id)


class _Pacer:
    """Allows at most ``per_minute`` starts per minute across threads."""

    def __init__(self, per_minute: Optional[float]):
        self.gap = 60.0 / per_minute if per_minute else 0.0
        self.lock, self.next = threading.Lock(), 0.0

    def wait(self) -> None:
        if not self.gap:
            return
        with self.lock:
            now = time.monotonic()
            at = max(now, self.next)
            self.next = at + self.gap
        time.sleep(max(0.0, at - now))


def run_episodes(cvm: CrowdVM, template: Optional[str], fn: Callable[[Episode, Any], Any], items: Iterable[Any], *,
                 max_workers: int = 20, starts_per_minute: Optional[float] = None, run_id: Optional[str] = None,
                 return_exceptions: bool = False, **episode_options: Any) -> List[Any]:
    """Run ``fn(episode, item)`` for every item, each in a fresh sandbox started from ``template``.

    Results come back in item order.  ``starts_per_minute`` paces the starts under your plan's limit (the API
    answers 429 ``rate_limited`` with ``Retry-After`` above it; starts retry anyway).  With
    ``return_exceptions=True`` a failed episode yields its exception instead of aborting the run."""
    run_id = run_id or uuid.uuid4().hex[:8]
    pacer = _Pacer(starts_per_minute)

    def one(item: Any) -> Any:
        try:
            pacer.wait()
            with Episode(cvm, template, run_id=run_id, **episode_options) as ep:
                return fn(ep, item)
        except Exception as e:  # noqa: BLE001
            if return_exceptions:
                return e
            raise

    with ThreadPoolExecutor(max_workers=max_workers) as pool:
        return list(pool.map(one, list(items)))


def cleanup(cvm: CrowdVM, run_id: Optional[str] = None) -> int:
    """Delete every sandbox left by eval runs (label ``crowdvm.eval``, or of one ``run_id``); returns how many."""
    n = 0
    kw: Dict[str, Any] = {"label": f"{LABEL}={run_id}"} if run_id else {}  # the server filters key=value only
    for sb in list(paginate(cvm.sandboxes, "sandboxes", **kw)):
        if LABEL not in (sb.get("labels") or {}):
            continue
        try:
            cvm.delete_sandbox(sb["id"], x_ascii_confirm_delete=sb["id"])
            n += 1
        except ApiError as e:
            if e.status != 404:
                raise
    return n
