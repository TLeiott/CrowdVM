#!/usr/bin/env python3
"""Live smoke test of the Python SDK against a real server.

    CROWDVM_API_BASE=http://127.0.0.1:8480/api/v1 CROWDVM_API_KEY=cvm_... PYTHONPATH=sdk/python/src python3 sdk/smoke/smoke.py

Creates a sandbox, runs commands (sync, streaming, detached + attach), moves files, stop/resume/fork/delete.
"""
import sys

from crowdvm import (ApiError, CrowdVM, exec_command, read_text, run_command, stream_command, wait_until_ready,
                     write_text)

cvm = CrowdVM()
made = []


def step(name):
    print(f"-- {name}", flush=True)


def expect(cond, what):
    if not cond:
        raise AssertionError(what)


try:
    step("create")
    sb = cvm.create(ttl_seconds=900, no_env=True, labels={"smoke": "py"}).sandbox
    made.append(sb.id)
    wait_until_ready(cvm, sb.id, timeout=180)
    expect(cvm.get(sb.id).sandbox.state in ("ready", "running", "idle"), "ready")

    step("command (sync)")
    out = cvm.command(sb.id, "echo hi && echo err >&2; exit 3").actual_instance
    expect((out.exit_code, out.stdout.strip(), out.stderr.strip()) == (3, "hi", "err"), out)
    out = exec_command(cvm, sb.id, "pwd", cwd="/tmp").actual_instance
    expect(out.stdout.strip().endswith("/tmp"), out)

    step("command (stream)")
    frames = list(stream_command(cvm, sb.id, "echo one; sleep 0.2; echo two"))
    text = "".join(str(f.data) for f in frames if f.type == "stdout")
    expect("one" in text and "two" in text, frames)
    expect(frames[-1].type == "exit" and frames[-1].exit_code == 0, frames[-1])

    step("command (detached + attach / run_command)")
    started = cvm.command(sb.id, "for i in 1 2 3; do echo tick$i; sleep 0.3; done", detached=True, timeout_seconds=60)
    pid = started.process_id
    frames = list(cvm.attach_command(sb.id, pid))
    expect("tick3" in "".join(str(f.data) for f in frames if f.type == "stdout"), frames)
    st = cvm.command_status(sb.id, pid)
    expect(st.running is False and st.exit_code == 0, st)
    frames = list(run_command(cvm, sb.id, "echo rc-ok; exit 4"))
    expect(any(f.type == "exit" and f.exit_code == 4 for f in frames), frames)

    step("files")
    write_text(cvm, sb.id, "/home/user/smoke/a.txt", "héllo\n")
    expect(read_text(cvm, sb.id, "/home/user/smoke/a.txt") == "héllo\n", "round trip")
    fl = cvm.list_files(sb.id, path="/home/user/smoke")
    expect(any(getattr(f, "name", "") == "a.txt" for f in (fl.get("files") or fl.get("entries") or [])), fl)

    step("stop")
    r = cvm.stop(sb.id)
    stop = r.get("stop") or r
    print("   stop:", {k: stop.get(k) for k in ("id", "status")})
    if stop.get("id"):
        print("   get_stop:", cvm.get_stop(sb.id, stop["id"]).get("stop", {}).get("status"))
    wait = 0
    while cvm.get(sb.id).sandbox.state not in ("stopped", "archived") and wait < 60:
        import time; time.sleep(1); wait += 1
    expect(cvm.get(sb.id).sandbox.state in ("stopped", "archived"), "stopped")

    step("resume")
    cvm.resume(sb.id)
    wait_until_ready(cvm, sb.id, timeout=180)
    expect(read_text(cvm, sb.id, "/home/user/smoke/a.txt") == "héllo\n", "file survived stop/resume")

    step("fork")
    fk = cvm.fork(sb.id, labels={"smoke": "py-fork"}).sandbox
    made.append(fk.id)
    wait_until_ready(cvm, fk.id, timeout=180)
    expect(read_text(cvm, fk.id, "/home/user/smoke/a.txt") == "héllo\n", "fork has the file")

    step("list by label")
    ids = [s.id for s in cvm.sandboxes(label="smoke=py").sandboxes]
    expect(sb.id in ids, ids)

    step("delete")
    for i in list(made):
        cvm.delete_sandbox(i, x_ascii_confirm_delete=i)
        made.remove(i)
    try:
        cvm.get(sb.id)
        print("   (sandbox still visible after delete; deleting is asynchronous)")
    except ApiError as e:
        expect(e.status == 404, e)
    print("PYTHON SMOKE OK")
except BaseException:
    print("PYTHON SMOKE FAILED", file=sys.stderr)
    raise
finally:
    for i in made:
        try:
            cvm.delete_sandbox(i, x_ascii_confirm_delete=i)
        except Exception as e:  # noqa: BLE001
            print("cleanup failed", i, e, file=sys.stderr)
