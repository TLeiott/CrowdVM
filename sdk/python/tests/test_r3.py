import asyncio
import http.client
import threading
import unittest
from unittest.mock import patch
from urllib.parse import parse_qs
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from crowdvm import CrowdVM, AsyncCrowdVM, APIConnectionError, ApiError
from crowdvm.client import _enc_query
from .fake_server import FakeServer, Resp, ok


class R3Tests(unittest.TestCase):
    def test_repeated_labels(self):
        self.assertEqual(parse_qs(_enc_query({'label': ['a=1', 'b=2']})), {'label': ['a=1', 'b=2']})

    def test_stream_eof_requires_exit(self):
        with FakeServer() as srv:
            srv.route('POST', '/api/v1/sandboxes/bx_a/commands', lambda r: Resp(lines=[{'type': 'stdout', 'data': 'partial'}]))
            with self.assertRaises(APIConnectionError):
                list(CrowdVM(base_url=srv.base).command('bx_a', 'x', stream=True))

    def test_command_null_timeout(self):
        with FakeServer() as srv:
            srv.route('POST', '/api/v1/sandboxes/bx_a/commands', lambda r: ok({'exitCode': 0}))
            out = CrowdVM(base_url=srv.base).command('bx_a', 'true', timeout_seconds=None)
            self.assertEqual(out.exitCode, 0)
            self.assertIsNone(srv.requests[0].json['timeoutSeconds'])

    def test_async_with_options(self):
        cvm = AsyncCrowdVM(api_key='old').with_options(api_key='new')
        self.assertIsInstance(cvm, AsyncCrowdVM)
        self.assertEqual(cvm.client.api_key, 'new')

    def test_async_cancel_blocked_stream(self):
        started = threading.Event()
        closed = threading.Event()
        class H(BaseHTTPRequestHandler):
            def log_message(self, *a): pass
            def do_POST(self):
                self.rfile.read(int(self.headers['Content-Length']))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"type":"stdout","data":"ready"}\n')
                self.wfile.flush()
                # Wait for the client to close, without sending a terminal frame.
                started.set()
                self.connection.settimeout(2)
                self.rfile.read(1)
                closed.set()
        srv = ThreadingHTTPServer(('127.0.0.1', 0), H)
        worker = threading.Thread(target=srv.serve_forever, daemon=True)
        worker.start()
        async def main():
            cvm = AsyncCrowdVM(base_url=f'http://127.0.0.1:{srv.server_port}')
            async def consume():
                async for _ in await cvm.command('bx_a', 'sleep 100', stream=True): pass
            task = asyncio.create_task(consume())
            while not started.is_set(): await asyncio.sleep(.01)
            await asyncio.sleep(.03)
            task.cancel()
            with self.assertRaises(asyncio.CancelledError):
                await asyncio.wait_for(task, 1)
        try:
            asyncio.run(main())
            self.assertTrue(closed.wait(1), 'stream socket was not closed')
        finally:
            srv.shutdown()
            srv.server_close()

    def test_early_write_error_keeps_server_response(self):
        # Force the body write to fail after the request headers. The HTTP
        # response must still be read and decoded as the server's API error.
        with FakeServer() as srv:
            from .fake_server import err
            srv.route('PUT', '/api/v1/sandboxes/bx_a/files', lambda r: err(400, 'file_too_large'))
            original = http.client.HTTPConnection.request
            def write_then_reset(conn, *args, **kwargs):
                original(conn, *args, **kwargs)
                raise ConnectionResetError('peer rejected write')
            with patch.object(http.client.HTTPConnection, 'request', write_then_reset):
                with self.assertRaises(ApiError) as ctx:
                    CrowdVM(base_url=srv.base).write_file('bx_a', 'big', 'content')
            self.assertEqual(ctx.exception.status, 400)
            self.assertEqual(ctx.exception.code, 'file_too_large')
