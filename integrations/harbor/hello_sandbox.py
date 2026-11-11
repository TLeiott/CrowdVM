"""Check your setup: one sandbox through the adapter, a file, a command, a directory round trip, stop + delete.

    CROWDVM_API_KEY=cvm_... CROWDVM_API_BASE=http://<server>:8420/api/v1 python hello_sandbox.py
"""
import asyncio
import os
import sys
import tempfile
from pathlib import Path

try:
    from harbor.models.task.config import EnvironmentConfig
    from harbor.models.trial.paths import TrialPaths
except ImportError:  # Harbor not installed: use the stand-ins
    from _harbor_shim import EnvironmentConfig, TrialPaths
from harbor_crowdvm_environment import CrowdVMEnvironment


WORKDIR = os.environ.get("HARBOR_WORKDIR", "/workspace")  # creating /workspace needs sudo in the sandbox; /tmp/work works anywhere


async def main() -> int:
    with tempfile.TemporaryDirectory() as raw:
        tmp = Path(raw)
        env_dir = tmp / "environment"
        env_dir.mkdir()
        (env_dir / "Dockerfile").write_text(f"FROM ubuntu:24.04\nWORKDIR {WORKDIR}\n")
        (env_dir / "seed.txt").write_text("seeded\n")
        trial_paths = TrialPaths(tmp / "trial")
        trial_paths.mkdir()
        env = CrowdVMEnvironment(environment_dir=env_dir, environment_name="harbor-hello", session_id="hello-session",
                                 trial_paths=trial_paths, task_env_config=EnvironmentConfig(workdir=WORKDIR),
                                 ttl_seconds=600)
        await env.start(force_build=False)
        try:
            await env.upload_file(__file__, f"{WORKDIR}/hello_sandbox.py")
            result = await env.exec("cat seed.txt && echo 'hello from CrowdVM' > notes.txt && cat notes.txt && uname -a")
            print("exit:", result.return_code)
            print(result.stdout)
            assert await env.is_file(f"{WORKDIR}/hello_sandbox.py") and await env.is_dir(WORKDIR)
            await env.download_dir(WORKDIR, tmp / "out")
            assert (tmp / "out" / "notes.txt").read_text() == "hello from CrowdVM\n"
            return result.return_code
        finally:
            await env.stop(delete=True)


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
