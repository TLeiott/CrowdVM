import asyncio
import unittest

from crowdvm import AsyncCrowdVM
from .fake_server import FakeServer, Resp, ok


class AsyncTests(unittest.TestCase):
    def test_async_calls_and_streams(self):
        with FakeServer() as srv:
            srv.route("POST", "/api/v1/sandboxes", lambda r: ok({"sandbox": {"id": "bx_a", "state": "provisioning"}}, 202))
            srv.route("POST", "/api/v1/sandboxes/bx_a/commands", lambda r: ok({"processId": 1}) if r.json.get("detached") else Resp(
                200, lines=[{"type": "stdout", "data": "hi"}, {"type": "exit", "exitCode": 0}]))
            srv.route("GET", "/api/v1/sandboxes/bx_a/commands/1/stream",
                      lambda r: Resp(200, lines=[{"type": "stdout", "data": "x", "nextOffset": 1}, {"type": "exit", "exitCode": 0}]))
            srv.route("GET", "/api/v1/sandboxes/bx_a", lambda r: ok({"sandbox": {"id": "bx_a", "state": "ready"}}))

            async def main():
                cvm = AsyncCrowdVM(api_key="k", base_url=srv.base)
                created = await cvm.create(ttl_seconds=60)
                sb = await cvm.wait_until_ready(created.sandbox.id)
                frames = [f async for f in await cvm.command("bx_a", "echo", stream=True)]
                ran = [f["type"] async for f in cvm.run_command("bx_a", "x")]
                return sb["state"], [f["type"] for f in frames], ran
            state, frames, ran = asyncio.run(main())
            self.assertEqual(state, "ready")
            self.assertEqual(frames, ["stdout", "exit"])
            self.assertEqual(ran, ["started", "stdout", "exit"])


if __name__ == "__main__":
    unittest.main()
