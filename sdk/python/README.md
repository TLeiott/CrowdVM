# crowdvm (Python SDK)

Python client for [CrowdVM](../../docs/ARCHITECTURE.md): persistent Ubuntu VMs for coding agents, running on
machines of your LAN or Tailnet. The HTTP API mirrors Boat's v1 API, and so does this SDK: same method names
(`create`, `command`, `command_status`, `read_file`, ...), same helpers (`wait_until_ready`, `run_command`, ...).

* Python 3.10+, **no runtime dependencies** (`urllib`), synchronous, plus an optional asyncio facade.
* Covers every endpoint of the OpenAPI file (sandboxes, commands, files, public URLs, snapshots, saved snapshots,
  environments, integrated agents, AI credentials, webhooks, API keys, OTel, CI, data retention).
* Retries `429` / `503` (honoring `retryAfterSeconds`) and commands refused while the sandbox restores its disk.

## Install

```bash
python -m pip install ./sdk/python        # or: pip install -e ./sdk/python
```

## Configure

```bash
export CROWDVM_API_BASE="http://192.168.1.10:8420/api/v1"   # your crowdvm-server
export CROWDVM_API_KEY="cvm_..."                            # crowdvm api-key create my-app --preset ci
```

```python
from crowdvm import CrowdVM

cvm = CrowdVM()   # reads both variables; or CrowdVM(api_key="cvm_...", base_url="http://host:8420")
```

* Every API operation is a method on `cvm`, named like Boat's SDK (snake_case). Path parameters are positional;
  everything else is a keyword argument in snake_case: `cvm.create(ttl_seconds=1800, no_env=True)`.
  `from` is spelled `var_from` (a Python keyword). Unknown keyword arguments are sent as-is in the request body.
* Responses are plain dicts with attribute access: `out.exit_code`, `out.exitCode` and `out["exitCode"]` all work,
  also nested (`created.sandbox.id`, `info.sandbox.memory_gb`). Keys that clash with `dict` methods (`items`,
  `keys`, ...) need `obj["items"]`. Type hints for every shape are in `crowdvm._types` (TypedDicts).
* Helpers are module functions taking the client first: `wait_until_ready`, `wait_until_idle`, `wait_for_desktop`,
  `wait_for_prompt`, `wait_for_prompt_done`, `stream_events`, `stream_prompt`, `stop_and_remove`, `read_text`,
  `write_text`, `exec_command`, `stream_command`, `run_command`, `follow_command`, `paginate`.
* `cvm.request("GET", "/some/path")` is a raw escape hatch; `cvm.with_options(max_retries=10)` returns a tweaked copy.

## Create, run a command, stop

```python
import os
from crowdvm import CrowdVM, wait_until_ready

cvm = CrowdVM(api_key=os.environ["CROWDVM_API_KEY"], base_url=os.environ["CROWDVM_API_BASE"])

created = cvm.create(ttl_seconds=1800)
sandbox_id = created.sandbox.id
try:
    wait_until_ready(cvm, sandbox_id)
    out = cvm.command(sandbox_id, "echo hi && uname -a")
    print(out.exit_code)
    print(out.stdout)
finally:
    cvm.stop(sandbox_id)
```

To delete the sandbox and its snapshots instead of a stop: `cvm.delete_sandbox(sandbox_id)` (the confirmation headers
default to the id; Boat's `x_ascii_confirm_delete=` is accepted, `confirm_delete=` is the CrowdVM alias).

### Code written for the Boat SDK

Boat SDK code runs unchanged; only the host changes. This package also installs `boat_sdk` (`boat_sdk`,
`boat_sdk.api.boat_api`, `boat_sdk.models` and `boat_sdk.models.<snake_name>`, `boat_sdk.exceptions`,
`boat_sdk.api_client`, `boat_sdk.configuration`, `boat_sdk.rest`), so Boat's imports resolve to CrowdVM. Do not install
Boat's own `boat-sdk` next to it.

* `BoatApi` methods take positional arguments in the order of Boat's method table: `host_port(sandbox_id,
  HostPortRequest(port=8000, public=True))`, `write_file(sandbox_id, FileWriteRequest(...))`, `resume(sandbox_id,
  ResumeRequest(...))`, `delete_sandbox(x_ascii_confirm_delete, sandbox_id)`, `delete_environment_secret_file(path,
  environment_id)`. A call that does not fit Boat's order runs with CrowdVM's (`create_webhook(url, events)`).
* The plain `CrowdVM` client also takes a request model in the first body slot (`cvm.host_port(sid, HostPortRequest(...))`).
* Request models (`boat_sdk.models.X` = `crowdvm.models.X`) are the JSON body with attribute access and `from_dict`,
  `to_dict`, `to_json`. A field passed as `None` is sent as JSON `null` (`CreateSandboxRequest(ttl_seconds=None)` = no
  auto-stop).
* `x_ascii_confirm_delete=`, `x_boat_org=`, `create(create_sandbox_request=...)`, `command(...).actual_instance` and
  `ApiException` (`.status`, `.body`) work as in Boat. `Configuration` also reads `BOAT_API_KEY` / `BOAT_API_BASE`.

```python
from boat_sdk import ApiClient, Configuration, wait_until_ready
from boat_sdk.api.boat_api import BoatApi
from boat_sdk.models import CommandRequest, CreateSandboxRequest, HostPortRequest

with ApiClient(Configuration(host="http://192.168.1.10:8420/api/v1", access_token="cvm_...")) as client:
    boat = BoatApi(client)
    sb = boat.create(create_sandbox_request=CreateSandboxRequest(ttl_seconds=1800)).sandbox
    wait_until_ready(boat, sb.id)
    print(boat.command(sb.id, CommandRequest(command="echo hi")).actual_instance.stdout)
    print(boat.host_port(sb.id, HostPortRequest(port=8000, public=True)).url)
    boat.delete_sandbox(sb.id, sb.id)
```

Every Python and TypeScript sample of Boat's SDK, evals, platform-guide and recipes pages runs against CrowdVM with
only the base URL changed: `python3 sdk/tools/doc_samples.py` (a fake server; `tests/test_doc_samples.py`) or
`python3 sdk/tools/doc_samples.py --base http://<server>:<port>/api/v1 --key cvm_...` (live).

CrowdVM extras on create / resume / fork: `placement={"nested": True, "labels": {...}, "node": ...}` (and `nested=True` on
create). `browser(sandbox_id, profile=...)` returns the kiosk-browser viewer URL.

## Run commands

A synchronous command times out after 30 seconds by default; set `timeout_seconds` up to 600. For longer
commands use [`run_command`](#run-a-command-with-no-time-limit). A command sent while the sandbox starts waits up to 60 s.

```python
from crowdvm import exec_command

out = cvm.command(sandbox_id, "npm test", cwd="my-repo", timeout_seconds=600)
print(out.exit_code, out.timed_out, out.stdout, out.stderr)

# Same call, but it keeps resending a command that was refused while the sandbox restored its disk.
out = exec_command(cvm, sandbox_id, "python3 -c 'print(6*7)'")
```

### Stream the output

```python
from crowdvm import stream_command

for frame in stream_command(cvm, sandbox_id, "npm test", cwd="my-repo", timeout_seconds=600):
    if frame["type"] in ("stdout", "stderr"):
        print(frame["data"], end="")
    elif frame["type"] == "exit":
        print("exit code:", frame["exitCode"])
    elif frame["type"] == "error":
        print("error:", frame["message"])
```

### Run a command with no time limit

`run_command` starts the command in the background, then yields its output until it ends. It reconnects by itself,
with no gap and no repeat.

```python
from crowdvm import run_command

for frame in run_command(cvm, sandbox_id, "npm run test:e2e", cwd="my-repo"):
    if frame["type"] in ("stdout", "stderr"):
        print(frame["data"], end="")
    elif frame["type"] == "exit":
        print("exit code:", frame["exitCode"])
```

* The first frame is `{"type": "started", "processId": ...}`.
* Breaking out of the loop leaves the command running; stop it with `cvm.stop_command(sandbox_id, process_id)`.
* `follow_command(cvm, sandbox_id, process_id)` follows a command that you started with `detached=True`.

### Run a server in the background

```python
import time

started = cvm.command(sandbox_id, "npm run build", detached=True)
while True:
    status = cvm.command_status(sandbox_id, started.process_id)
    if not status.running:
        break
    time.sleep(2)
print(status.status, status.exit_code, status.stdout)
```

`attach_command`, `stop_command`, `command_history`, `get_command`, `command_logs` and `processes` complete the group.

## Files

```python
from crowdvm import read_text, write_text

write_text(cvm, sandbox_id, "notes/result.txt", "done\n")
print(read_text(cvm, sandbox_id, "notes/result.txt"))
```

Paths are relative to `/home/user`. `read_file` / `write_file` also accept `encoding="base64"` for binary content.
For files of any size, get a signed URL and use any HTTP client:

```python
import urllib.request

up = cvm.create_upload_url(sandbox_id, "data/input.csv")
with open("input.csv", "rb") as f:
    urllib.request.urlopen(urllib.request.Request(up.url, data=f.read(), method=up.method))

down = cvm.create_download_url(sandbox_id, "data/input.csv")
data = urllib.request.urlopen(down.url).read()
```

The URLs expire; treat them as secrets. `list_files`, `get_file_info`, `search_files`, `find_in_files`,
`create_folder`, `move_file`, `set_file_permissions`, `replace_in_files`, `delete_file` and `artifact` are there too.

### Copy a local folder in

```python
import io, os, subprocess, tarfile, urllib.request
from crowdvm import exec_command

def push(cvm, sandbox_id: str, folder: str, remote: str = "repo"):
    listed = subprocess.run(["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
                            cwd=folder, capture_output=True, check=True).stdout.decode().split("\0")
    packed = io.BytesIO()
    with tarfile.open(fileobj=packed, mode="w:gz") as tar:
        for name in listed:
            path = os.path.join(folder, name)
            if name and os.path.lexists(path) and (os.path.islink(path) or not os.path.isdir(path)):
                tar.add(path, arcname=name, recursive=False)
    up = cvm.create_upload_url(sandbox_id, "push.tgz")
    urllib.request.urlopen(urllib.request.Request(up.url, data=packed.getvalue(), method=up.method))
    exec_command(cvm, sandbox_id, f"mkdir -p {remote} && tar xzf push.tgz -C {remote} && rm push.tgz", timeout_seconds=600)
```

## Run one job and delete the sandbox

```python
from crowdvm import run_command, wait_until_ready

created = cvm.create(var_from="ci-base", type="large", ttl_seconds=3600)
sandbox_id = created.sandbox.id
code = None
try:
    wait_until_ready(cvm, sandbox_id)
    push(cvm, sandbox_id, ".")
    for frame in run_command(cvm, sandbox_id, "npm ci && npm test", cwd="repo"):
        if frame["type"] in ("stdout", "stderr"):
            print(frame["data"], end="")
        elif frame["type"] == "exit":
            code = frame["exitCode"]
    usage = cvm.usage(sandbox_id)
    print(f"exit {code} · {usage.seconds}s · {usage.sandbox_type} · {usage.dollars:.4f} credits")
finally:
    cvm.delete_sandbox(sandbox_id)
raise SystemExit(code if isinstance(code, int) else 1)
```

## Public URL for a port

```python
cvm.command(sandbox_id, "python3 -m http.server 8000", detached=True)
hosted = cvm.host_port(sandbox_id, 8000, public=True)
print(hosted.url)
```

## Machine size, environment variables, no-env

```python
created = cvm.create(type="large", ttl_seconds=3600)            # small | default | large | xlarge
info = cvm.get(created.sandbox.id)
print(info.sandbox.type, info.sandbox.vcpu, info.sandbox.memory_gb)

cvm.create(ttl_seconds=3600, env={"DATABASE_URL": "postgres://...", "FEATURE_FLAG": "1"})   # per-sandbox env
cvm.create(ttl_seconds=1800, no_env=True)                                                  # none of your secrets
cvm.resume(sandbox_id, type="xlarge")                                                      # resize: stop, then resume

for t in cvm.limits().machine_types:
    print(t.type, "ok" if t.available else t.reason)
```

## Saved snapshots and forks

```python
import time, uuid

cvm.save_saved_snapshot(sandbox_id=sandbox_id, name="web-stack")
while cvm.get_saved_snapshot("web-stack").snapshot.status not in ("ready", "failed"):
    time.sleep(5)

created = cvm.create(idempotency_key=str(uuid.uuid4()), var_from="web-stack", ttl_seconds=900)
forked = cvm.fork(sandbox_id, idempotency_key=str(uuid.uuid4()), ttl_seconds=900)
```

`idempotency_key` makes `create` and `fork` safe to retry. Snapshots: `list_snapshots`, `list_sandbox_snapshots`,
`get_latest_sandbox_snapshot`, `get_snapshot_tree`, `get_snapshot_file`, `get_snapshot_download`, `delete_snapshot`...

## Prompt an integrated agent

```python
from crowdvm import wait_for_prompt

queued = cvm.prompt(sandbox_id, "Run the tests and fix the failures.", provider="codex")
cvm.steer(sandbox_id, "Skip the integration tests.")        # add an instruction to the running turn

run = wait_for_prompt(cvm, sandbox_id, queued.prompt_id)
print(run.status)                                            # wait_for_prompt_done raises PromptFailed unless it finished

for event in cvm.events(sandbox_id, limit=50, type="prompt,response").events:
    print(event.type, event.get("conversationId"))
```

`provider` is optional (server default). Pass `new=True` for a new conversation or `conversation_id=` to continue one.
`interrupt(sandbox_id)` discards the running turn; `steer` keeps it running. `provider_models()` lists models.

### Stream the agent's work

```python
from crowdvm import stream_prompt

for event in stream_prompt(cvm, sandbox_id, prompt="Run pwd and ls.", provider="codex"):
    if event.type == "response" and event.data.get("content"):
        print(event.data["content"], end="")
```

`stream_events(cvm, sandbox_id, type="prompt,response", stop=lambda: done)` follows every event of a sandbox
(the events endpoint is polled, so it is not a true stream).

## Browse a stopped sandbox

```python
latest = cvm.get_latest_sandbox_snapshot(sandbox_id)
tree = cvm.get_snapshot_tree(latest.snapshot.id)
data = cvm.get_snapshot_file(latest.snapshot.id, path="projects/app/.env")   # bytes
```

## Pagination

```python
from crowdvm import paginate

for sb in paginate(cvm.sandboxes, "sandboxes", state="running", limit=200):
    print(sb.id)
```

## Errors and retries

Non-2xx answers raise `ApiError` (alias `ApiException`) with `status`, `code`, `message`, `details`, `request_id`,
`body`; subclasses: `BadRequestError`, `AuthenticationError`, `PaymentRequiredError`, `PermissionDeniedError`,
`NotFoundError`, `ConflictError`, `RateLimitError`, `ServerError`, `ServiceUnavailableError`. Network failures raise
`APIConnectionError`; helpers raise `WaitTimeout`, `SandboxFailed`, `PromptFailed`. All derive from `CrowdVMError`.

```python
from crowdvm import NotFoundError

try:
    cvm.get("bx_missing")
except NotFoundError as e:
    print(e.status, e.code, e.request_id)
```

The client retries (default `max_retries=3`, backoff with jitter, capped by `max_retry_delay`):

* `503` (`out_of_capacity`, ...) after the `retryAfterSeconds` of the body; never `no_ready_machine`, the answer to `failFast`;
* `429` `IP_RATE_LIMIT_EXCEEDED` and `rate_limited` on the *minute* window. Not retried: hour/day windows,
  `limit_reached`, `member_limit_reached`, `trial_compute_limit_reached`;
* `502 sandbox_direct_failed` with message `sandbox_restoring` and `409 sandbox_starting` (the command did not run);
* `409 idempotency_in_progress`; connection failures of `GET` requests.

A `502` that may have run your command is never resent automatically (check `e.retryable`).

## Webhooks

```python
from crowdvm.webhooks import construct_event, verify_webhook, WebhookVerificationError

# in your HTTP handler; pass the RAW body (bytes) and the request headers
event = construct_event(raw_body, request.headers, os.environ["WEBHOOK_SECRET"])   # raises on a bad signature
if event.type == "sandbox.ready":
    print(event.data.sandbox.id)
```

`verify_webhook(raw_body, headers, secret) -> bool` does only the check. Pass a list of secrets while one rotates.
Timestamps older than 5 minutes are rejected (`tolerance=`). Headers `X-CrowdVM-Delivery|Timestamp|Signature` are read
(Boat's `X-Ascii-*` too). Deduplicate on `event.id`: delivery is at-least-once. `sign_webhook(...)` makes
signatures for your own tests. Create endpoints with `cvm.create_webhook(name=..., url=..., events=[...])`.

## asyncio

```python
import asyncio
from crowdvm import AsyncCrowdVM

async def main():
    cvm = AsyncCrowdVM()
    sb = (await cvm.create(ttl_seconds=600)).sandbox
    await cvm.wait_until_ready(sb.id)
    async for frame in cvm.run_command(sb.id, "make test"):
        print(frame)
    await cvm.stop_and_remove(sb.id)

asyncio.run(main())
```

Every call runs in a thread (`asyncio.to_thread`); helpers are methods without the client argument; streams are async iterators.

## All methods

See [`../METHODS.md`](../METHODS.md) (generated from the OpenAPI file: Python name, TypeScript name, endpoint).

## Development

```bash
cd sdk/python && python3 -m unittest          # stdlib only; tests run against a local fake HTTP server
python3 sdk/tools/gen.py                      # regenerate _types.py / _ops.py (and the TS twins) from .ref/boat-v1.yaml; needs PyYAML
```
