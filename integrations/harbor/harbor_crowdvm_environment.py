"""Harbor environment adapter for CrowdVM (the counterpart of `harbor-box`'s `BoxEnvironment`).

Runs Harbor trials in CrowdVM sandboxes instead of local Docker containers::

    export CROWDVM_API_KEY=cvm_...
    export CROWDVM_API_BASE=http://192.168.1.10:8420/api/v1
    harbor run -p path/to/task -a claude-code -m anthropic/claude-sonnet-4-5 \\
        -e harbor_crowdvm_environment:CrowdVMEnvironment -n 4

It talks to CrowdVM's `/api/v1` through the `crowdvm` Python SDK (stdlib only).  Unlike `harbor-box` 0.1.2
it deletes its sandbox on ``stop(delete=True)``.  Not supported, and refused with an error: a prebuilt
``docker_image``, Docker builds/Compose, disabled internet, GPUs.  Every sandbox is a *no-env* sandbox
by default (none of your secrets).

If `harbor` is not installed, small stand-ins from `_harbor_shim.py` are used (tests, dry runs).
"""
from __future__ import annotations

import asyncio
import base64
import io
import math
import os
import re
import shlex
import tarfile
import tempfile
import urllib.request
import uuid
from pathlib import Path
from typing import Any, Dict, Optional

from crowdvm import ApiError, CrowdVM, exec_command, run_command, wait_until_ready

try:  # the real Harbor
    from harbor.environments.base import BaseEnvironment, ExecResult  # type: ignore
    try:
        from harbor.models.environment_type import EnvironmentType  # type: ignore
    except ImportError:  # pragma: no cover
        EnvironmentType = None  # type: ignore
except ImportError:  # unit tests / no Harbor installed
    from _harbor_shim import BaseEnvironment, EnvironmentType, ExecResult  # type: ignore

__all__ = ["CrowdVMEnvironment", "BoxEnvironment"]

INLINE_LIMIT = 5 * 1024 * 1024  # bigger files go through signed upload/download URLs
SYNC_LIMIT = 600                 # longest synchronous command; longer ones run detached
# smallest size that covers (vCPU, memory MiB): see docs for the sizes
SIZES = (("small", 2, 4096), ("default", 4, 8192), ("large", 8, 16384), ("xlarge", 16, 32768))


def pick_size(cpus: int, memory_mb: int) -> str:
    for name, c, m in SIZES:
        if cpus <= c and memory_mb <= m:
            return name
    raise ValueError(f"no CrowdVM size has {cpus} vCPU and {memory_mb} MiB memory (max xlarge: 16 vCPU, 32 GiB)")


def dockerfile_workdir(environment_dir: Path) -> Optional[str]:
    """The WORKDIR of the last stage of environment/Dockerfile (nothing else of it is run)."""
    path = Path(environment_dir) / "Dockerfile"
    if not path.is_file():
        return None
    found = None
    for line in path.read_text(errors="replace").splitlines():
        m = re.match(r"\s*WORKDIR\s+(\S+)", line, re.I)
        if m:
            found = m.group(1)
        elif re.match(r"\s*FROM\s", line, re.I):
            found = None
    return found


class CrowdVMEnvironment(BaseEnvironment):
    """Harbor environment backed by one CrowdVM sandbox per trial.

    Options (besides Harbor's standard arguments): ``api_key`` (default ``CROWDVM_API_KEY``), ``base_url``
    (default ``CROWDVM_API_BASE``), ``ttl_seconds`` (auto-stop, default 86400, ``None`` = off),
    ``no_env`` (default True), ``request_timeout_seconds`` (60), ``sandbox_type`` (default: smallest size
    that fits the task's cpus/memory), ``from_snapshot`` (create trial sandboxes from this saved snapshot),
    ``labels``.
    """

    def __init__(self, *args: Any, api_key: Optional[str] = None, base_url: Optional[str] = None,
                 ttl_seconds: Optional[int] = 86400, no_env: bool = True, request_timeout_seconds: float = 60,
                 sandbox_type: Optional[str] = None, from_snapshot: Optional[str] = None,
                 labels: Optional[Dict[str, str]] = None, **kwargs: Any):
        # Configs written for harbor-box keep working with their BOX_* variables.
        api_key = api_key or os.environ.get("CROWDVM_API_KEY") or os.environ.get("BOX_API_KEY")
        base_url = base_url or os.environ.get("CROWDVM_API_BASE") or os.environ.get("BOX_API_BASE")
        self._cvm = CrowdVM(api_key, base_url, timeout=request_timeout_seconds, max_retries=6)
        self._ttl_seconds, self._no_env = ttl_seconds, no_env
        self._sandbox_type, self._from_snapshot, self._labels = sandbox_type, from_snapshot, dict(labels or {})
        self._sandbox_id: Optional[str] = None
        self._workdir = "/workspace"
        super().__init__(*args, **kwargs)
        self._workdir = (getattr(self.task_env_config, "workdir", None) or dockerfile_workdir(self.environment_dir)
                         or "/workspace")

    # ---- Harbor metadata
    @staticmethod
    def type() -> Any:
        return getattr(EnvironmentType, "DOCKER", None) if EnvironmentType else None

    @property
    def is_mounted(self) -> bool:
        return False

    @property
    def supports_gpus(self) -> bool:
        return False

    @property
    def can_disable_internet(self) -> bool:
        return False

    def _validate_definition(self) -> None:
        cfg = self.task_env_config
        if getattr(cfg, "docker_image", None):
            raise RuntimeError(f"CrowdVM sandboxes cannot run a prebuilt Docker image ({cfg.docker_image}); "
                               "install the task's dependencies in a saved snapshot and use from_snapshot=...")
        if getattr(cfg, "allow_internet", True) is False:
            raise RuntimeError("CrowdVM cannot disable internet access for a sandbox (no network policies).")
        if getattr(cfg, "gpus", 0):
            raise RuntimeError("CrowdVM sandboxes have no GPUs.")

    # ---- helpers
    @property
    def sandbox_id(self) -> str:
        if not self._sandbox_id:
            raise RuntimeError("the environment is not started")
        return self._sandbox_id

    async def _call(self, fn: Any, *args: Any, **kwargs: Any) -> Any:
        return await asyncio.to_thread(fn, *args, **kwargs)

    def _abs(self, path: str) -> str:
        wd = self._workdir.rstrip("/")
        if path.startswith("/") or path == wd or path.startswith(wd + "/"):
            return path
        return f"{wd}/{path}"

    # ---- lifecycle
    async def start(self, force_build: bool = False) -> None:
        cfg = self.task_env_config
        size = self._sandbox_type or pick_size(getattr(cfg, "cpus", 1) or 1, getattr(cfg, "memory_mb", 2048) or 2048)
        body: Dict[str, Any] = dict(type=size, no_env=self._no_env, ttl_seconds=self._ttl_seconds,
                                    labels={"harbor.session": self.session_id, "harbor.environment": self.environment_name,
                                            **self._labels})
        if self._from_snapshot:
            body["var_from"] = self._from_snapshot
        created = await self._call(self._cvm.create, idempotency_key=str(uuid.uuid4()), **body)
        self._sandbox_id = created["sandbox"]["id"]
        try:
            await self._call(wait_until_ready, self._cvm, self._sandbox_id, timeout=300)
            await self.ensure_dirs(self._workdir)
            env_dir = Path(self.environment_dir)
            for p in (sorted(env_dir.iterdir()) if env_dir.is_dir() else []):
                if p.name != "Dockerfile":
                    target = f"{self._workdir.rstrip('/')}/{p.name}"
                    await (self.upload_dir(p, target) if p.is_dir() else self.upload_file(p, target))
        except BaseException:
            await self.stop(delete=True)  # never leave a half-started sandbox running
            raise

    async def stop(self, delete: bool = False) -> None:
        sid, self._sandbox_id = self._sandbox_id, None
        if not sid:
            return
        try:
            if delete:
                await self._call(self._cvm.delete_sandbox, sid, x_ascii_confirm_delete=sid)
            else:
                await self._call(self._cvm.stop, sid)
        except ApiError as e:
            if e.status != 404:
                raise

    # ---- commands
    @staticmethod
    def _script(command: str, env: Optional[Dict[str, str]], user: Any) -> str:
        script = command
        if env:
            script = "".join(f"export {k}={shlex.quote(str(v))}; " for k, v in env.items()) + script
        if user not in (None, "", "user", 1000, "1000"):
            script = f"sudo -n -u {shlex.quote(str(user))} bash -lc {shlex.quote(script)}"
        return script

    def _run_sync(self, script: str, cwd: str, timeout_sec: Optional[float]) -> ExecResult:
        # the API accepts a cwd only under /home/user or /tmp; elsewhere (/workspace, /) cd inside the command
        if not cwd.startswith("/") or cwd.startswith(("/home/user/", "/tmp/")) or cwd in ("/home/user", "/tmp"):
            kw: Dict[str, Any] = {"cwd": cwd}
        else:
            script, kw = f"cd {shlex.quote(cwd)} && {script}", {}
        return self._run(script, kw, timeout_sec)

    def _run(self, script: str, kw: Dict[str, Any], timeout_sec: Optional[float]) -> ExecResult:
        if timeout_sec is not None and timeout_sec <= SYNC_LIMIT:
            r = exec_command(self._cvm, self.sandbox_id, script, timeout_seconds=max(1, math.ceil(timeout_sec)), **kw)
            if r.get("timedOut"):
                return ExecResult(stdout=r.get("stdout"), stderr=(r.get("stderr") or "") + "\n[timed out]", return_code=124)
            return ExecResult(stdout=r.get("stdout"), stderr=r.get("stderr"), return_code=r.get("exitCode", 0))
        if timeout_sec is not None:  # longer than the synchronous limit: run detached, enforce with timeout(1)
            script = f"timeout {math.ceil(timeout_sec)} bash -c {shlex.quote(script)}"
        out, err, code = [], [], 1
        for f in run_command(self._cvm, self.sandbox_id, script, **kw):
            t = f.get("type")
            if t == "stdout":
                out.append(str(f.get("data", "")))
            elif t == "stderr":
                err.append(str(f.get("data", "")))
            elif t == "exit":
                code = f.get("exitCode", 0)
            elif t == "error":
                err.append(str(f.get("message", "")))
        return ExecResult(stdout="".join(out), stderr="".join(err), return_code=code)

    async def exec(self, command: str, cwd: Optional[str] = None, env: Optional[Dict[str, str]] = None,
                   timeout_sec: Optional[float] = None, user: Any = None, **_: Any) -> ExecResult:
        return await self._call(self._run_sync, self._script(command, env, user), cwd or self._workdir, timeout_sec)

    async def _sh(self, command: str) -> ExecResult:
        return await self._call(self._run, command, {}, 120)

    # ---- files
    def _put(self, local: Path, target: str) -> None:
        data = Path(local).read_bytes()
        parent = os.path.dirname(target) or "/"
        mk = exec_command(self._cvm, self.sandbox_id, f"mkdir -p {shlex.quote(parent)}")
        if mk.get("exitCode"):
            raise RuntimeError(f"cannot create {parent}: {mk.get('stderr')}")
        if len(data) <= INLINE_LIMIT:
            self._cvm.write_file(self.sandbox_id, target, base64.b64encode(data).decode(), encoding="base64")
            return
        url = self._cvm.create_upload_url(self.sandbox_id, path=target)["url"]
        req = urllib.request.Request(url, data=data, method="PUT")
        urllib.request.urlopen(req, timeout=600).close()

    def _get(self, source: str, local: Path) -> None:
        Path(local).parent.mkdir(parents=True, exist_ok=True)
        info = self._cvm.get_file_info(self.sandbox_id, path=source)
        if info.get("size", 0) <= INLINE_LIMIT:
            res = self._cvm.read_file(self.sandbox_id, source, encoding="base64")
            Path(local).write_bytes(base64.b64decode(res["content"]))
            return
        url = self._cvm.create_download_url(self.sandbox_id, path=source)["url"]
        with urllib.request.urlopen(url, timeout=600) as r:
            Path(local).write_bytes(r.read())

    async def upload_file(self, source_path: Any, target_path: str) -> None:
        await self._call(self._put, Path(source_path), self._abs(str(target_path)))

    async def download_file(self, source_path: str, target_path: Any) -> None:
        await self._call(self._get, self._abs(str(source_path)), Path(target_path))

    async def upload_dir(self, source_dir: Any, target_dir: str) -> None:
        """Copy the *contents* of ``source_dir`` into ``target_dir``."""
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tf:
            tf.add(str(source_dir), arcname=".")
        target = self._abs(str(target_dir))
        with tempfile.NamedTemporaryFile(suffix=".tgz") as tmp:
            tmp.write(buf.getvalue()); tmp.flush()
            remote = f".crowdvm-up-{uuid.uuid4().hex[:8]}.tgz"
            await self._call(self._put, Path(tmp.name), remote)
        r = await self._sh(f"mkdir -p {shlex.quote(target)} && tar xzf {remote} -C {shlex.quote(target)}; rc=$?; rm -f {remote}; exit $rc")
        if r.return_code:
            raise RuntimeError(f"upload_dir to {target} failed: {r.stderr}")

    async def download_dir(self, source_dir: str, target_dir: Any) -> None:
        src = self._abs(str(source_dir))
        remote = f".crowdvm-down-{uuid.uuid4().hex[:8]}.tgz"
        r = await self._sh(f"tar czf {remote} -C {shlex.quote(src)} .")
        if r.return_code:
            raise RuntimeError(f"download_dir from {src} failed: {r.stderr}")
        with tempfile.TemporaryDirectory() as tmp:
            local = Path(tmp) / "d.tgz"
            await self._call(self._get, remote, local)
            await self._sh(f"rm -f {remote}")
            Path(target_dir).mkdir(parents=True, exist_ok=True)
            with tarfile.open(local) as tf:
                tf.extractall(target_dir, filter="data")

    async def is_file(self, path: str) -> bool:
        return (await self._sh(f"test -f {shlex.quote(self._abs(str(path)))}")).return_code == 0

    async def is_dir(self, path: str) -> bool:
        return (await self._sh(f"test -d {shlex.quote(self._abs(str(path)))}")).return_code == 0

    async def ensure_dirs(self, *dirs: str) -> None:
        for d in dirs:
            p = shlex.quote(self._abs(str(d)))
            # /workspace-style paths need root once: fall back to sudo, then hand the folder to the sandbox user
            r = await self._sh(f"mkdir -p {p} 2>/dev/null || {{ sudo -n mkdir -p {p} && sudo -n chown \"$(id -u):$(id -g)\" {p}; }}")
            if r.return_code:
                raise RuntimeError(f"cannot create {d}: {r.stderr}")


#: `-e harbor_crowdvm_environment:BoxEnvironment` also works, for configs written for harbor-box.
BoxEnvironment = CrowdVMEnvironment
