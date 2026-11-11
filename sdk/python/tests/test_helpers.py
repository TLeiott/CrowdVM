import time
import unittest

import crowdvm
from crowdvm import (CrowdVM, PromptFailed, SandboxFailed, WaitTimeout, exec_command, follow_command, paginate,
                     read_text, run_command, stop_and_remove, stream_command, stream_events, stream_prompt,
                     wait_for_desktop, wait_for_prompt, wait_for_prompt_done, wait_until_idle, wait_until_ready,
                     write_text)
from .fake_server import FakeServer, Resp, err, ok


def sb(state, **kw):
    return ok({"sandbox": {"id": "bx_x", "name": "n", "state": state, **kw}})


def client(srv, **kw):
    c = CrowdVM(api_key="k", base_url=srv.base, **kw)
    c._sleep = lambda s: None
    return c


class WaitTests(unittest.TestCase):
    def setUp(self):
        self._sleep = time.sleep
        time.sleep = lambda s: None

    def tearDown(self):
        time.sleep = self._sleep

    def test_wait_until_ready(self):
        with FakeServer() as srv:
            srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("provisioning"), sb("cloning"), sb("ready"))
            self.assertEqual(wait_until_ready(client(srv), "bx_x")["state"], "ready")
            self.assertEqual(len(srv.requests), 3)

    def test_wait_until_ready_failure_and_timeout(self):
        with FakeServer() as srv:
            srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("provisioning"), sb("error", error="no capacity"))
            with self.assertRaises(SandboxFailed):
                wait_until_ready(client(srv), "bx_x")
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/sandboxes/bx_x", lambda r: sb("provisioning"))
            with self.assertRaises(WaitTimeout):
                wait_until_ready(client(srv), "bx_x", timeout=0, interval=1)

    def test_wait_until_ready_waits_out_node_offline(self):
        with FakeServer() as srv:
            srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("provisioning"), sb("error", error="node_offline"),
                    sb("error", error="node_offline: node n1 disconnected"), sb("ready"))
            self.assertEqual(wait_until_ready(client(srv), "bx_x")["state"], "ready")
            self.assertEqual(len(srv.requests), 4)
        with FakeServer() as srv:
            srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("error", error="node_offline"), sb("idle"))
            self.assertEqual(wait_until_idle(client(srv), "bx_x")["state"], "idle")

    def test_wait_until_ready_node_offline_fails_at_timeout(self):
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/sandboxes/bx_x", lambda r: sb("error", error="node_offline"))
            with self.assertRaises(SandboxFailed) as cm:
                wait_until_ready(client(srv), "bx_x", timeout=5, interval=1)
            self.assertIn("node_offline", str(cm.exception))
            self.assertGreater(len(srv.requests), 1)

    def test_wait_until_ready_other_error_fails_at_once(self):
        with FakeServer() as srv:
            srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("error", error="boot_failed"), sb("ready"))
            with self.assertRaises(SandboxFailed):
                wait_until_ready(client(srv), "bx_x")
            self.assertEqual(len(srv.requests), 1)

    def test_wait_until_idle_skips_running(self):
        with FakeServer() as srv:
            srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("running"), sb("idle"))
            self.assertEqual(wait_until_idle(client(srv), "bx_x")["state"], "idle")

    def test_wait_for_desktop(self):
        with FakeServer() as srv:
            srv.seq("POST", "/api/v1/sandboxes/bx_x/desktop", err(503, "browser_not_ready", retryAfterSeconds=0),
                    ok({"provisioning": True, "desktopUrl": None}), ok({"desktopUrl": "http://d", "provisioning": False}))
            out = wait_for_desktop(client(srv, max_retries=0), "bx_x")
            self.assertEqual(out.desktop_url, "http://d")
            self.assertEqual(srv.requests[0].query, {"vnc": "1"})

    def test_wait_for_prompt(self):
        run = lambda status, done: ok({"promptRun": {"promptId": "p1", "status": status, "done": done}})
        with FakeServer() as srv:
            srv.seq("GET", "/api/v1/sandboxes/bx_x/prompts/p1", run("queued", False), run("running", False),
                    run("finished", True))
            self.assertEqual(wait_for_prompt(client(srv), "bx_x", "p1").status, "finished")
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/sandboxes/bx_x/prompts/p1", lambda r: run("failed", True))
            self.assertEqual(wait_for_prompt(client(srv), "bx_x", "p1").status, "failed")
            with self.assertRaises(PromptFailed):
                wait_for_prompt_done(client(srv), "bx_x", "p1")


class CommandTests(unittest.TestCase):
    def setUp(self):
        self._sleep = time.sleep
        time.sleep = lambda s: None

    def tearDown(self):
        time.sleep = self._sleep

    def test_exec_command_retries_restoring(self):
        with FakeServer() as srv:
            restoring = err(502, "sandbox_direct_failed", "sandbox_restoring", {"retryable": True})
            srv.seq("POST", "/api/v1/sandboxes/bx_x/commands", *([restoring] * 6), ok({"exitCode": 0, "stdout": "42\n"}))
            # default client would give up after 3 retries; exec_command keeps going
            out = exec_command(client(srv), "bx_x", "python3 -c 'print(6*7)'", cwd="r", timeout_seconds=60)
            self.assertEqual(out.stdout, "42\n")
            self.assertEqual(srv.requests[-1].json, {"command": "python3 -c 'print(6*7)'", "cwd": "r", "timeoutSeconds": 60})

    def test_stream_command(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes/bx_x/commands", lambda r: Resp(200, lines=[
                {"type": "started", "commandId": "c"}, {"type": "stdout", "data": "x"}, {"type": "exit", "exitCode": 0}]))
            frames = list(stream_command(client(srv), "bx_x", "echo x", timeout_seconds=5))
            self.assertEqual([f["type"] for f in frames], ["started", "stdout", "exit"])
            self.assertEqual(srv.requests[0].json, {"command": "echo x", "timeoutSeconds": 5, "stream": True})

    def test_run_command_follows_and_reconnects_without_gap_or_repeat(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes/bx_x/commands", lambda r: ok({"processId": 5, "commandId": "c9"}))
            attach = [
                # first stream: ends abruptly (no terminal frame)
                Resp(200, lines=[{"type": "attached", "processId": 5, "stdoutOffset": 0, "stderrOffset": 0},
                                 {"type": "stdout", "data": "one ", "nextOffset": 4},
                                 {"type": "heartbeat"},
                                 {"type": "stderr", "data": "warn", "nextOffset": 4}]),
                # second: server asks to reconnect after its 10 minutes
                Resp(200, lines=[{"type": "attached", "stdoutOffset": 4, "stderrOffset": 4},
                                 {"type": "stdout", "data": "two ", "nextOffset": 8},
                                 {"type": "reconnect", "stdoutOffset": 8, "stderrOffset": 4}]),
                # third: finishes
                Resp(200, lines=[{"type": "attached", "stdoutOffset": 8, "stderrOffset": 4},
                                 {"type": "stdout", "data": "three", "nextOffset": 13},
                                 {"type": "exit", "status": "exited", "exitCode": 7}]),
            ]
            srv.seq("GET", "/api/v1/sandboxes/bx_x/commands/5/stream", *attach)
            frames = list(run_command(client(srv), "bx_x", "long job", cwd="repo"))
            self.assertEqual(frames[0]["type"], "started")
            self.assertEqual(frames[0]["processId"], 5)
            out = "".join(f["data"] for f in frames if f["type"] == "stdout")
            self.assertEqual(out, "one two three")
            self.assertEqual(frames[-1]["type"], "exit")
            self.assertEqual(frames[-1]["exitCode"], 7)
            self.assertEqual(srv.requests[0].json, {"command": "long job", "cwd": "repo", "detached": True})
            queries = [r.query for r in srv.requests[1:]]
            self.assertEqual(queries, [{"stdoutOffset": "0", "stderrOffset": "0"},
                                       {"stdoutOffset": "4", "stderrOffset": "4"},
                                       {"stdoutOffset": "8", "stderrOffset": "4"}])

    def test_follow_command_error_frame(self):
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/sandboxes/bx_x/commands/2/stream",
                      lambda r: Resp(200, lines=[{"type": "error", "error": "lost", "message": "gone"}]))
            frames = list(follow_command(client(srv), "bx_x", 2))
            self.assertEqual(frames[-1]["type"], "error")

    def test_files(self):
        with FakeServer() as srv:
            srv.route("PUT", "/api/v1/sandboxes/bx_x/files", lambda r: ok({"success": True}))
            srv.route("GET", "/api/v1/sandboxes/bx_x/files", lambda r: ok({"content": "done\n", "encoding": "utf8"}))
            c = client(srv)
            write_text(c, "bx_x", "notes/result.txt", "done\n")
            self.assertEqual(read_text(c, "bx_x", "notes/result.txt"), "done\n")
            self.assertEqual(srv.requests[1].query, {"path": "notes/result.txt", "encoding": "utf8"})


class EventTests(unittest.TestCase):
    def setUp(self):
        self._sleep = time.sleep
        time.sleep = lambda s: None

    def tearDown(self):
        time.sleep = self._sleep

    def test_stream_events_yields_only_new_events_once(self):
        ev = lambda i, ts: {"id": f"e{i}", "type": "response", "timestamp": ts}
        pages = [[ev(2, 2), ev(1, 1)], [ev(2, 2), ev(1, 1)], [ev(4, 4), ev(3, 3), ev(2, 2)], [ev(4, 4), ev(3, 3)]]
        with FakeServer() as srv:
            it = iter(pages)
            srv.route("GET", "/api/v1/sandboxes/bx_x/events", lambda r: ok(
                {"events": next(it, pages[-1]), "pageInfo": {"hasMore": False, "nextCursor": None, "limit": 200}}))
            got = []
            polls = {"n": 0}

            def stop():
                polls["n"] += 1
                return polls["n"] > 3
            for e in stream_events(client(srv), "bx_x", type="response", stop=stop):
                got.append(e["id"])
            self.assertEqual(got, ["e3", "e4"])
            self.assertEqual(srv.requests[0].query["type"], "response")

    def test_stream_prompt(self):
        ev = lambda i: {"id": f"e{i}", "type": "response", "timestamp": i, "data": {"content": f"c{i}"}}
        with FakeServer() as srv:
            event_pages = iter([[], [ev(1)], [ev(2), ev(1)]])
            srv.route("GET", "/api/v1/sandboxes/bx_x/events", lambda r: ok(
                {"events": next(event_pages, []), "pageInfo": {"hasMore": False, "limit": 200}}))
            srv.route("POST", "/api/v1/sandboxes/bx_x/prompt",
                      lambda r: ok({"promptId": "p1", "conversationId": "conv1"}, 202))
            states = iter([False, False, True])
            srv.route("GET", "/api/v1/sandboxes/bx_x/prompts/p1",
                      lambda r: ok({"promptRun": {"promptId": "p1", "status": "running", "done": next(states, True)}}))
            out = [e["data"]["content"] for e in stream_prompt(client(srv), "bx_x", prompt="hi", provider="codex")]
            self.assertEqual(out, ["c1", "c2"])
            post = [r for r in srv.requests if r.method == "POST"][0]
            self.assertEqual(post.json, {"prompt": "hi", "provider": "codex"})
            events = [r for r in srv.requests if r.path.endswith("/events")]
            self.assertEqual(events[-1].query.get("conversation"), "conv1")
            self.assertNotIn("conversation", events[0].query)  # seeded before the conversation is known


class MiscTests(unittest.TestCase):
    def setUp(self):
        self._sleep = time.sleep
        time.sleep = lambda s: None

    def tearDown(self):
        time.sleep = self._sleep

    def test_paginate(self):
        with FakeServer() as srv:
            def sandboxes(r):
                if r.query.get("cursor") == "c2":
                    return ok({"sandboxes": [{"id": "bx_3"}], "pageInfo": {"hasMore": False, "nextCursor": None, "limit": 2}})
                return ok({"sandboxes": [{"id": "bx_1"}, {"id": "bx_2"}], "pageInfo": {"hasMore": True, "nextCursor": "c2", "limit": 2}})
            srv.route("GET", "/api/v1/sandboxes", sandboxes)
            c = client(srv)
            ids = [s["id"] for s in paginate(c.sandboxes, "sandboxes", limit=2, state="running")]
            self.assertEqual(ids, ["bx_1", "bx_2", "bx_3"])
            self.assertEqual(srv.requests[1].query, {"limit": "2", "state": "running", "cursor": "c2"})

    def test_stop_and_remove(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes/bx_x/stop", lambda r: ok({"id": "bx_x", "status": "archiving"}, 202))
            srv.seq("GET", "/api/v1/sandboxes/bx_x", sb("archiving"), sb("archived"))
            srv.route("DELETE", "/api/v1/sandboxes/bx_x", lambda r: ok({"operation": {"id": "op"}}, 202))
            out = stop_and_remove(client(srv), "bx_x")
            self.assertEqual(out.operation.id, "op")
            self.assertEqual([r.method for r in srv.requests], ["POST", "GET", "GET", "DELETE"])
            self.assertEqual(srv.requests[-1].headers["x-crowdvm-confirm-delete"], "bx_x")

    def test_provider_models_and_other_ops_exist(self):
        with FakeServer() as srv:
            srv.route("GET", "/api/v1/provider-models", lambda r: ok({"providers": []}))
            srv.route("POST", "/api/v1/webhooks", lambda r: ok({"webhook": {"id": "wh_1"}, "secret": "whsec_x"}, 201))
            c = client(srv)
            self.assertEqual(c.provider_models().providers, [])
            created = c.create_webhook(name="p", url="https://e/h", events=["sandbox.ready"])
            self.assertEqual(created.secret, "whsec_x")
            self.assertEqual(srv.requests[1].json, {"name": "p", "url": "https://e/h", "events": ["sandbox.ready"]})


if __name__ == "__main__":
    unittest.main()
