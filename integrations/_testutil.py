"""Test helpers shared by the adapters: loads the SDK's stdlib fake server (sdk/python/tests/fake_server.py)."""
import importlib.util
import os
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
SDK_SRC = os.path.join(ROOT, "sdk", "python", "src")
if SDK_SRC not in sys.path:
    sys.path.insert(0, SDK_SRC)

_spec = importlib.util.spec_from_file_location("_cvm_fake_server", os.path.join(ROOT, "sdk", "python", "tests", "fake_server.py"))
fake_server = importlib.util.module_from_spec(_spec)
sys.modules["_cvm_fake_server"] = fake_server
_spec.loader.exec_module(fake_server)
FakeServer, Resp, err, ok = fake_server.FakeServer, fake_server.Resp, fake_server.err, fake_server.ok
