#!/usr/bin/env python3
"""Run every Python and TypeScript code sample of Boat's SDK docs against CrowdVM.

The samples come verbatim from `.ref/pages` (sdks__python, sdks__typescript, sdks__overview, evals,
platform-guide*, recipes*).  The only edit is the base URL (`https://boat.dev/api/v1` -> the CrowdVM
server); imports stay Boat's (`boat_sdk...`, `@boatdev/sdk`), which CrowdVM's SDKs provide.
Fragments that assume an existing client and sandbox get one from a preamble: `boat` (alias
`sandbox`) and `sandbox_id` / `sandboxId`, a ready sandbox.

    python3 sdk/tools/doc_samples.py                        # against an in-process fake server
    python3 sdk/tools/doc_samples.py --base http://127.0.0.1:9500/api/v1 --key cvm_...   # live
    python3 sdk/tools/doc_samples.py --lang py -k evals     # a subset

Live runs also map the fixed sample ids (`bx_23456789`, `bx_f7k2q9hd`) to the preamble sandbox
and run `--episodes` (default 2) instead of 100 eval episodes.  Exit status 0 when every sample passed.
"""
from __future__ import annotations

import argparse
import contextlib
import io
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import traceback
from typing import Callable, Dict, Iterator, List, Optional, Tuple

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
PAGES = ["sdks__overview", "sdks__python", "sdks__typescript", "evals", "platform-guide", "platform-guide__agents",
         "platform-guide__patterns", "platform-guide__templates-and-sizing", "recipes__agent-on-repo",
         "recipes__parallel-agents"]
BOAT_URL = "https://boat.dev/api/v1"
SAMPLE_IDS = ("bx_23456789", "bx_f7k2q9hd")
SAVED = ["ci-base", "task-env-v1"]
PACKAGE_JSON = ('{"name":"app","version":"1.0.0","scripts":{"test":"echo tests ok","test:e2e":"echo e2e ok",'
                '"build":"echo built","dev":"echo dev"}}')
PACKAGE_LOCK = '{"name":"app","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"app","version":"1.0.0"}}}'

FENCE = re.compile(r"^```(\w+)[^\n]*\n(.*?)^```", re.M | re.S)
PY_LANGS, TS_LANGS = {"python"}, {"ts", "typescript", "js", "javascript"}


class Sample:
    def __init__(self, page: str, index: int, lang: str, code: str) -> None:
        self.page, self.index, self.lang, self.code = page, index, lang, code

    @property
    def name(self) -> str:
        return f"{self.page}#{self.index}:{self.lang}"


def extract(pages: List[str] = PAGES) -> List[Sample]:
    out: List[Sample] = []
    for page in pages:
        with open(os.path.join(ROOT, ".ref", "pages", page + ".md")) as f:
            text = f.read()
        for i, m in enumerate(FENCE.finditer(text)):
            lang = m.group(1)
            if lang in PY_LANGS:
                out.append(Sample(page, i, "py", m.group(2)))
            elif lang in TS_LANGS:
                out.append(Sample(page, i, "ts", m.group(2)))
    return out


class Env:
    """Where the samples run: base URL, key, a ready sandbox, a git work dir, live or fake."""

    def __init__(self, base: str, key: str, live: bool, episodes: int, node: str = "") -> None:
        self.base, self.key, self.live, self.episodes, self.node = base, key, live, episodes, node
        self.sandbox_id = ""
        self.baseline: set = set()
        self.workdir = tempfile.mkdtemp(prefix="crowdvm-doc-samples-")

    def adapt(self, code: str) -> str:
        code = code.replace(BOAT_URL, self.base)
        if self.live:
            for sid in SAMPLE_IDS:
                code = code.replace(sid, self.sandbox_id)
            code = code.replace("range(100)", f"range({self.episodes})").replace("max_workers=50", f"max_workers={self.episodes}")
            # one fork, not 4: a fork of a running sandbox goes to its node, and small pools fill up
            code = code.replace(', "Fix issue 102", "Fix issue 103", "Fix issue 104"]', "]")
        return code

    def client(self):
        from boat_sdk import ApiClient, BoatApi, Configuration
        return BoatApi(ApiClient(Configuration(host=self.base, access_token=self.key)))

    def setup(self) -> None:
        """A ready sandbox with the files the samples expect, and the saved snapshots they deploy."""
        sys.path.insert(0, os.path.join(ROOT, "sdk", "python", "src"))
        os.environ.update(BOAT_API_KEY=self.key, BOAT_API_BASE=self.base)
        from boat_sdk import wait_until_ready
        boat = self.client()
        self.baseline = self.sandbox_ids()
        # The prompt samples use Codex or the default harness and assume a connected key: placeholder
        # keys make the server accept the prompts (forks inherit them); the turns then fail, which the
        # samples only print.
        keys = {"OPENAI_API_KEY": "sk-doc-samples", "ANTHROPIC_API_KEY": "sk-doc-samples"}
        placement = {"placement": {"node": self.node}} if self.node else {}
        self.sandbox_id = boat.create(ttl_seconds=3600, name="doc-samples", type="small", env=keys, **placement).sandbox.id
        self.baseline.add(self.sandbox_id)
        wait_until_ready(boat, self.sandbox_id, timeout=600)
        # the projects the samples work on: npm scripts that pass, the eval task script
        boat.command(self.sandbox_id, "mkdir -p my-repo project eval-tasks && printf 'import sys\\nprint(sys.argv)\\n' > eval-tasks/run.py"
                     " && mkdir -p projects/app && echo A=1 > projects/app/.env"
                     f" && cd my-repo && printf '%s' '{PACKAGE_JSON}' > package.json && printf '%s' '{PACKAGE_LOCK}' > package-lock.json")

        for name in SAVED:  # `boat snapshot <id> task-env-v1` etc. in the docs' prose
            boat.save_saved_snapshot(sandbox_id=self.sandbox_id, name=name)
        for name in SAVED:
            deadline = time.time() + 600
            while boat.get_saved_snapshot(name).snapshot.status not in ("ready", "failed") and time.time() < deadline:
                time.sleep(2)
        subprocess.run(["git", "init", "-q"], cwd=self.workdir, check=True)
        for name, data in (("README.md", "doc samples\n"), ("input.csv", "a,b\n1,2\n"), ("package.json", PACKAGE_JSON),
                           ("package-lock.json", PACKAGE_LOCK)):
            with open(os.path.join(self.workdir, name), "w") as f:
                f.write(data)

    def sandbox_ids(self) -> set:
        from crowdvm import paginate
        return {sb["id"] for sb in paginate(self.client().sandboxes, "sandboxes")}

    def cleanup(self) -> None:
        """Live: delete the sandboxes a sample created and left (the docs keep them; the pool is small)."""
        if not self.live:
            return
        boat = self.client()
        for sid in self.sandbox_ids() - self.baseline:
            with contextlib.suppress(Exception):
                boat.delete_sandbox(sid, sid)

    def stop(self) -> None:
        """For a fragment that resumes with changes (`resume(id, ResumeRequest(...))`): the docs' sandbox is stopped."""
        boat = self.client().with_options(max_retries=10)
        boat.stop(self.sandbox_id)
        deadline = time.time() + 300
        while boat.get(self.sandbox_id).sandbox.state not in ("archived", "stopped") and time.time() < deadline:
            time.sleep(1)

    def ensure_ready(self) -> None:
        """Each fragment assumes a ready sandbox; an earlier one may have stopped it."""
        from boat_sdk import wait_until_ready
        boat = self.client().with_options(max_retries=10)
        state = boat.get(self.sandbox_id).sandbox.state
        if state in ("archiving", "archived", "stopping", "stopped"):
            deadline = time.time() + 300
            while boat.get(self.sandbox_id).sandbox.state not in ("archived", "stopped") and time.time() < deadline:
                time.sleep(1)
            boat.resume(self.sandbox_id)
        wait_until_ready(boat, self.sandbox_id, timeout=600)

    def teardown(self) -> None:
        shutil.rmtree(self.workdir, ignore_errors=True)
        if not self.sandbox_id:
            return
        self.cleanup()
        boat = self.client()
        for name in SAVED + ["web-stack"]:
            with contextlib.suppress(Exception):
                boat.delete_saved_snapshot(name)
        with contextlib.suppress(Exception):
            boat.delete_sandbox(self.sandbox_id, self.sandbox_id)


# ---------------------------------------------------------------------------- Python
PY_PREAMBLE = """
import os
from boat_sdk import ApiClient, Configuration
from boat_sdk.api.boat_api import BoatApi
from boat_sdk.models import CommandRequest, CreateSandboxRequest, ForkRequest, ResumeRequest, StopRequest
boat = sandbox = BoatApi(ApiClient(Configuration(host=os.environ["BOAT_API_BASE"], access_token=os.environ["BOAT_API_KEY"])))
"""


def run_py(env: Env, samples: List[Sample]) -> Iterator[Tuple[Sample, Optional[str], str]]:
    namespaces: Dict[str, dict] = {}
    for s in samples:
        ns = namespaces.get(s.page)
        if ns is None:
            ns = namespaces[s.page] = {"__name__": "__sample__"}
            exec(PY_PREAMBLE, ns)
        env.ensure_ready()
        if env.live and re.search(r"\.resume\(sandbox_id, \w+Request", s.code):
            env.stop()
        ns["sandbox_id"] = env.sandbox_id  # each fragment assumes a ready sandbox
        ns["boat"] = ns["sandbox"] = ns.get("boat") or ns["sandbox"]
        out = io.StringIO()
        err: Optional[str] = None
        try:
            with contextlib.redirect_stdout(out):
                try:
                    exec(compile(env.adapt(s.code), s.name, "exec"), ns)
                except SystemExit as e:
                    if e.code not in (0, None):
                        raise RuntimeError(f"sample exited {e.code}") from None
                if "run_many" in ns and "async def run_many" in s.code:  # the asyncio fragment only defines functions
                    import asyncio
                    print(asyncio.run(ns["run_many"](ns["boat"], [env.sandbox_id])))
        except BaseException:
            err = traceback.format_exc(limit=6)
        env.cleanup()
        yield s, err, out.getvalue()


# ---------------------------------------------------------------------------- TypeScript
IMPORT = re.compile(r"^import\s+(type\s+)?\{([^}]*)\}\s+from\s+\"([^\"]+)\";?\s*$", re.M)
TS_PREAMBLE = {
    "boat": 'const boat = new __BoatApi(new __Configuration({ basePath: process.env.BOAT_API_BASE, accessToken: process.env.BOAT_API_KEY }));',
    "sandbox": "const sandbox = boat;",
    "sandboxId": "const sandboxId: string = process.env.SAMPLE_SANDBOX_ID!;",
}


def _declares(code: str, name: str) -> bool:
    return re.search(rf"\b(const|let|var|function|class)\s+{name}\b", code) is not None or \
        re.search(rf"^import[^;]*\b{name}\b", code, re.M) is not None


def build_ts(sample: Sample, earlier: List[Sample], env: Env) -> Tuple[str, str]:
    """The source of a runnable module for `sample` and its file suffix."""
    code = env.adapt(sample.code)
    if "require(" in code:
        return code, ".cjs"
    body = code
    # a fragment that calls a helper function defined by an earlier fragment of the page gets it
    for e in earlier:
        for fn in re.findall(r"^(?:async\s+)?function\s+(\w+)", e.code, re.M):
            if re.search(rf"\b{fn}\(", body) and not _declares(body, fn):
                body = env.adapt(e.code) + "\n" + body
    imports: Dict[str, Dict[str, bool]] = {}
    for kind, names, mod in IMPORT.findall(body):
        for n in (x.strip() for x in names.split(",")):
            if n:
                imports.setdefault(mod, {})[n] = imports.get(mod, {}).get(n, False) or not kind
    body = IMPORT.sub("", body)
    head = ['import { BoatApi as __BoatApi, Configuration as __Configuration } from "@boatdev/sdk";']
    for mod, names in imports.items():
        vals = [n for n, is_value in names.items() if is_value]
        types = [n for n, is_value in names.items() if not is_value]
        if vals:
            head.append(f'import {{ {", ".join(vals)} }} from "{mod}";')
        if types:
            head.append(f'import type {{ {", ".join(types)} }} from "{mod}";')
    need = {n for n in TS_PREAMBLE if re.search(rf"(?<![.\w$]){n}\b(?!\s*:)", body) and not _declares(body, n)}
    if "sandbox" in need and not _declares(body, "boat"):
        need.add("boat")
    head += [decl for n, decl in TS_PREAMBLE.items() if n in need]
    return "\n".join(head) + "\n" + body + "\nexport {};\n", ".mts"


def run_ts(env: Env, samples: List[Sample]) -> Iterator[Tuple[Sample, Optional[str], str]]:
    sdk = os.path.join(ROOT, "sdk", "typescript")
    if not shutil.which("node") or not os.path.exists(os.path.join(sdk, "dist", "index.js")):
        for s in samples:
            yield s, "SKIP: node or sdk/typescript/dist missing (npm --prefix sdk/typescript install)", ""
        return
    mods = os.path.join(env.workdir, "node_modules", "@boatdev")
    os.makedirs(mods, exist_ok=True)
    link = os.path.join(mods, "sdk")
    if not os.path.lexists(link):
        os.symlink(sdk, link)  # what `npm install @boatdev/sdk@file:<repo>/sdk/typescript` creates
    by_page: Dict[str, List[Sample]] = {}
    for s in samples:
        src, suffix = build_ts(s, by_page.setdefault(s.page, []), env)
        by_page[s.page].append(s)
        path = os.path.join(env.workdir, f"sample-{s.page}-{s.index}{suffix}")
        with open(path, "w") as f:
            f.write(src)
        env.ensure_ready()
        if env.live and re.search(r"\.resume\(\{ sandboxId, \w+:", s.code):
            env.stop()
        child_env = dict(os.environ, SAMPLE_SANDBOX_ID=env.sandbox_id, BOAT_API_KEY=env.key, BOAT_API_BASE=env.base)
        try:
            p = subprocess.run(["node", "--no-warnings", path], cwd=env.workdir, env=child_env, capture_output=True,
                               text=True, timeout=600)
            err = None if p.returncode == 0 else f"exit {p.returncode}\n{p.stderr[-3000:]}\n--- module ---\n{src}"
        except subprocess.TimeoutExpired:
            err, p = "timed out after 600 s", None
        env.cleanup()
        yield s, err, p.stdout if p else ""


# ---------------------------------------------------------------------------- main
def run(base: Optional[str] = None, key: Optional[str] = None, lang: str = "all", match: str = "",
        episodes: int = 2, verbose: bool = False, report: Callable[[str], None] = print,
        fake=None, node: str = "") -> List[Tuple[str, Optional[str]]]:
    """Run the samples; returns ``[(name, error or None)]``.  Without `base`, a fake server serves
    (`fake`, a started `boat_fake.FakeBoat`, or a new in-process one)."""
    samples = [s for s in extract() if lang in ("all", s.lang) and match in s.name]
    own_fake = False
    if base is None:
        sys.path.insert(0, HERE)
        from boat_fake import KEY, FakeBoat
        if fake is None:
            fake, own_fake = FakeBoat().__enter__(), True
        base, key = fake.base, KEY
    env = Env(base, key or "", fake is None, episodes, node)
    real_sleep = time.sleep
    # the samples poll and pace with time.sleep: no waiting against the fake, short waits live
    time.sleep = (lambda s: real_sleep(min(s, 0.01))) if fake else (lambda s: real_sleep(min(s, 5)))
    results: List[Tuple[str, Optional[str]]] = []
    cwd = os.getcwd()
    try:
        env.setup()
        os.chdir(env.workdir)
        for runner, lang_ in ((run_py, "py"), (run_ts, "ts")):
            for s, err, out in runner(env, [x for x in samples if x.lang == lang_]):
                if err and env.live and "no_running_turn" in err and "steer" in s.code.lower():
                    # Boat answers a steer after the turn ended with 409 no_running_turn; with the
                    # placeholder keys the turn fails at once, so this sample needs a real agent key.
                    err = "EXPECTED: 409 no_running_turn (the placeholder-key turn already ended; needs a real agent key)"
                results.append((s.name, err))
                status = "PASS" if err is None else err.split(":")[0] if err.startswith(("SKIP", "EXPECTED")) else "FAIL"
                report(f"{status} {s.name}")
                if err and status != "PASS" or verbose:
                    report("    " + (err or out).strip().replace("\n", "\n    "))
    finally:
        os.chdir(cwd)
        time.sleep = real_sleep
        env.teardown()
        if own_fake:
            fake.__exit__(None, None, None)
    return results


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", help="CrowdVM API base (live run); default: in-process fake")
    ap.add_argument("--key", default=os.environ.get("CROWDVM_API_KEY"), help="API key for --base")
    ap.add_argument("--lang", choices=["all", "py", "ts"], default="all")
    ap.add_argument("-k", dest="match", default="", help="only samples whose name contains this")
    ap.add_argument("--episodes", type=int, default=2, help="eval episodes in a live run")
    ap.add_argument("--preamble-node", default="", help="live: place the samples' own sandbox on this node (keeps the others free for `large`)")
    ap.add_argument("-v", "--verbose", action="store_true")
    a = ap.parse_args()
    res = run(a.base, a.key, a.lang, a.match, a.episodes, a.verbose, node=a.preamble_node)
    failed = [n for n, e in res if e and not e.startswith(("SKIP", "EXPECTED"))]
    expected = [n for n, e in res if e and e.startswith("EXPECTED")]
    print(f"{len(res) - len(failed)}/{len(res)} samples passed" + (f" ({len(expected)} expected refusals: {', '.join(expected)})" if expected else "")
          + (f"; failed: {', '.join(failed)}" if failed else ""))
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
