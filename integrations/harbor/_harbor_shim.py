"""Thin stand-ins for the parts of Harbor that `harbor_crowdvm_environment` needs.

Used only when the real `harbor` package cannot be imported (unit tests, or checking the adapter
without installing Harbor).  The shapes follow Harbor's `BaseEnvironment` / `ExecResult` /
`EnvironmentConfig` / `TrialPaths` as of Harbor 0.14+:

    BaseEnvironment(environment_dir, environment_name, session_id, trial_paths, task_env_config, ...)
    await env.start(force_build) / stop(delete) / exec(command, cwd, env, timeout_sec, user)
    await env.upload_file / upload_dir / download_file / download_dir / is_file / is_dir / ensure_dirs
"""
from __future__ import annotations

import enum
import logging
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Optional


class EnvironmentType(str, enum.Enum):
    DOCKER = "docker"


@dataclass
class ExecResult:
    stdout: Optional[str] = None
    stderr: Optional[str] = None
    return_code: int = 0


@dataclass
class EnvironmentConfig:
    docker_image: Optional[str] = None
    workdir: Optional[str] = None
    allow_internet: bool = True
    cpus: int = 1
    memory_mb: int = 2048
    storage_mb: int = 10240
    gpus: int = 0


class TrialPaths:
    def __init__(self, trial_dir: Path):
        self.trial_dir = Path(trial_dir)

    def mkdir(self) -> None:
        self.trial_dir.mkdir(parents=True, exist_ok=True)


class BaseEnvironment:
    def __init__(self, environment_dir: Path, environment_name: str, session_id: str, trial_paths: Any,
                 task_env_config: Any, logger: Optional[logging.Logger] = None, *args: Any, **kwargs: Any):
        self.environment_dir = Path(environment_dir)
        self.environment_name = environment_name
        self.session_id = session_id
        self.trial_paths = trial_paths
        self.task_env_config = task_env_config
        self.logger = logger or logging.getLogger(type(self).__name__)
        self._validate_definition()

    @property
    def is_mounted(self) -> bool:
        return False

    @property
    def supports_gpus(self) -> bool:
        return False

    @property
    def can_disable_internet(self) -> bool:
        return False
