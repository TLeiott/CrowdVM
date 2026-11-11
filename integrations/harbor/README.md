# harbor-crowdvm

Harbor environment adapter for CrowdVM: each Harbor trial runs in one CrowdVM sandbox instead of a local Docker
container. The counterpart of `harbor-box` (`BoxEnvironment`), but on `/api/v1`, with a working
`stop(delete=True)`.

```bash
pip install ./sdk/python ./integrations/harbor        # from the repository root
export CROWDVM_API_KEY=cvm_...                        # crowdvm api-key create harbor --preset ci
export CROWDVM_API_BASE=http://192.168.1.10:8420/api/v1
harbor run -p path/to/task -a claude-code -m anthropic/claude-sonnet-4-5 \
  -e harbor_crowdvm_environment:CrowdVMEnvironment -n 4
```

Check the setup first: `python3 hello_sandbox.py` (creates a sandbox, uploads a file, runs a command, round-trips a
directory, deletes it). Set `HARBOR_WORKDIR=/tmp/work` when the node uses the `process` driver or the sandbox has no sudo.

| Harbor operation | On CrowdVM |
| - | - |
| `start` | `create` (smallest size covering the task's cpus/memory, `noEnv`, TTL, labels `harbor.*`) → wait ready → create the workdir → upload the environment directory (not the `Dockerfile`) |
| `stop(delete)` | `stop`, or `delete` with the confirm header. A failed start deletes its sandbox |
| `exec` | one command; `env`, `user` (via `sudo -n -u`), `timeout_sec`. ≤ 600 s runs synchronously (timeout → exit 124); longer or unlimited runs detached and followed |
| `upload_file` / `download_file` | base64 up to 5 MiB, signed upload/download URLs above |
| `upload_dir` / `download_dir` | tar.gz round trip (copies the *contents* of the directory) |
| `is_file` / `is_dir` / `ensure_dirs` | shell checks; `ensure_dirs` falls back to `sudo` for paths like `/workspace` |

Options of `CrowdVMEnvironment(...)` besides Harbor's: `api_key`, `base_url`, `ttl_seconds` (86400; `None` = no auto-stop),
`no_env` (True), `request_timeout_seconds` (60), `sandbox_type`, `from_snapshot` (create trial sandboxes from a saved
snapshot: prepare one sandbox, `crowdvm snapshot`, reuse it; see [`../evals`](../evals/README.md)), `labels`.

Refused with an error (no false isolation): a prebuilt `docker_image` (so Terminal-Bench 2.0 does not run through the adapter,
same as on Boat; use a saved snapshot instead), Dockerfile `RUN`/`COPY` and Compose (only the `WORKDIR` is read), disabled
internet, GPUs. `BoxEnvironment` is an alias for configs written for `harbor-box`.

Notes: the API accepts a command `cwd` only under `/home/user` or `/tmp`; other workdirs (`/workspace`) are entered with
`cd`. Without Harbor installed, `_harbor_shim.py` supplies `BaseEnvironment`/`ExecResult`/`EnvironmentConfig` so the
tests and `hello_sandbox.py` run; the real Harbor classes are used when `harbor` imports. Not yet run against Harbor itself.

Tests: `python3 -m unittest discover -s tests -t .`
