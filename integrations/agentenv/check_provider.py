#!/usr/bin/env python3
"""Does this CrowdVM server serve what AgentEnv's built-in `boat` sandbox provider needs?

AgentEnv needs no adapter: point its `boat` provider at CrowdVM with `api_url` (see config.toml.example).
This script replays the provider's documented calls with the CrowdVM SDK, so you can check a server
(or a new key) before a long run:

  1. smallest size that covers the requested CPU / memory / disk (`pick_size`)
  2. create: that `type`, `noEnv: true`, `snapshots: false`, `ttlSeconds`, labels `agentenv.managed=true`
     and `agentenv.attr.*`, `hostedPorts` for exposed ports; retried up to 6 times on 429 / 503
  3. wait until ready, run a command, host a port, read the URL
  4. find the run's sandboxes by label and delete them (crash cleanup), delete with the `X-Ascii-Confirm-Delete` header

    CROWDVM_API_KEY=cvm_... CROWDVM_API_BASE=http://<server>:8420/api/v1 python3 check_provider.py
"""
from __future__ import annotations

import os
import sys
import time
import uuid

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "..", "sdk", "python", "src"))
from crowdvm import ApiError, CrowdVM, exec_command, wait_until_ready  # noqa: E402

# (name, vCPU, memory GiB, disk GB): the table of the AgentEnv docs
SIZES = (("small", 2, 4, 51), ("default", 4, 8, 51), ("large", 8, 16, 111), ("xlarge", 16, 32, 251))
MANAGED = "agentenv.managed"


def pick_size(cpu: float, memory_gib: float, disk_gb: float) -> str:
    for name, c, m, d in SIZES:
        if cpu <= c and memory_gib <= m and disk_gb <= d:
            return name
    raise ValueError("request is bigger than xlarge")


def create_with_retries(cvm: CrowdVM, attempts: int = 6, **body):
    key = str(uuid.uuid4())  # the same key on a retry never makes a second sandbox
    for attempt in range(attempts):
        try:
            return cvm.create(idempotency_key=key, **body)["sandbox"]
        except ApiError as e:
            if e.status not in (429, 503) or attempt == attempts - 1:
                raise
            time.sleep(min(60, 2 ** attempt))


def main() -> int:
    cvm = CrowdVM()
    run = uuid.uuid4().hex[:8]
    ok = True

    def check(what: str, cond: bool) -> None:
        nonlocal ok
        print(("ok    " if cond else "FAIL  ") + what)
        ok &= bool(cond)

    ids = []
    try:
        size = pick_size(cpu=2, memory_gib=4, disk_gb=40)
        check("smallest fitting size is small", size == "small" and pick_size(3, 6, 60) == "large")
        sb = create_with_retries(
            cvm, type=size, no_env=True, snapshots=False, ttl_seconds=7200,
            labels={MANAGED: "true", "agentenv.attr.run": run, "agentenv.attr.env": "check"},
            hosted_ports=[{"port": 8080}])
        ids.append(sb["id"])
        wait_until_ready(cvm, sb["id"], timeout=300)
        got = cvm.get(sb["id"])["sandbox"]
        check("labels round-trip", got["labels"].get(MANAGED) == "true" and got["labels"].get("agentenv.attr.run") == run)
        check("noEnv honoured", got.get("noEnv") is True)
        check("size, snapshots off, ttl honoured", (got.get("type"), got.get("snapshots"), got.get("ttlSeconds")) == (size, False, 7200))
        out = exec_command(cvm, sb["id"], "echo agentenv-ok")
        check("command runs", out["stdout"].strip() == "agentenv-ok")
        host = cvm.host_port(sb["id"], port=8080, public=False)
        check("hosted port has a URL", bool(host.get("url")))
        found = [s["id"] for s in cvm.sandboxes(label=f"{MANAGED}=true")["sandboxes"]]
        check("find by label (crash cleanup)", sb["id"] in found)
        try:
            cvm.request("DELETE", f"/sandboxes/{sb['id']}")
            refused = False
        except ApiError as e:
            refused = e.status == 409
        check("delete without confirm header is refused", refused)
        try:
            cvm.delete_sandbox(sb["id"], x_ascii_confirm_delete=sb["id"])
        finally:
            ids.remove(sb["id"])
        try:
            cvm.get(sb["id"])
            gone = False
        except ApiError as e:
            gone = e.status == 404
        check("delete removes the sandbox", gone)
    finally:
        for i in ids:
            try:
                cvm.delete_sandbox(i)
            except ApiError:
                pass
    print("AGENTENV CHECK " + ("OK" if ok else "FAILED"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
