#!/usr/bin/env python3
"""Generate the typed models and the operation tables/methods of both SDKs
from the Boat OpenAPI file (.ref/boat-v1.yaml).  Dev-only: needs PyYAML.

    python3 sdk/tools/gen.py

Writes:
    sdk/python/src/crowdvm/_types.py   TypedDicts for every component schema
    sdk/python/src/crowdvm/_ops.py     operation table + generated client methods
    sdk/typescript/src/types.ts        interfaces / type aliases
    sdk/typescript/src/operations.ts   operation table + generated client methods

The generated files are committed; the SDKs never read the YAML at runtime.
"""
import json
import keyword
import os
import re
import sys

import yaml

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
SPEC = os.path.join(ROOT, ".ref", "boat-v1.yaml")
PY_OUT = os.path.join(ROOT, "sdk", "python", "src", "crowdvm")
TS_OUT = os.path.join(ROOT, "sdk", "typescript", "src")

spec = yaml.safe_load(open(SPEC))
SCHEMAS = spec["components"]["schemas"]
PARAMS = spec["components"]["parameters"]

# Operations written by hand in the clients (they need custom return handling).
MANUAL_OPS = {"command"}
# Ops added on top of the spec (the Boat docs mention them; the YAML omits them).
EXTRA_OPS = [
    dict(operationId="providerModels", method="get", path="/provider-models",
         summary="List the agent providers, their models and reasoning efforts",
         params=[], body=None, ret_type="ProviderModelsResponse"),
    dict(operationId="browser", method="post", path="/sandboxes/{sandboxId}/browser",
         summary="Get a browser-only (kiosk Chrome) streaming URL",
         params=[], body={"schema": {"$ref": "#/components/schemas/BrowserRequest"}, "required": False},
         ret_type="BrowserResponse", ret_schema={"$ref": "#/components/schemas/BrowserResponse"}),
]
# CrowdVM request fields the Boat spec does not know (per operation id).
EXTRA_BODY_PROPS = {
    "create": {"nested": {"type": "boolean", "description": "Expose /dev/kvm (nested virtualization)."},
                      "placement": {"$ref": "#/components/schemas/Placement"}},
    "fork": {"placement": {"$ref": "#/components/schemas/Placement"}},
    "resume": {"placement": {"$ref": "#/components/schemas/Placement"}},
}
# Operations that take the X-Boat-Org header although the spec does not list it.
EXTRA_ORG_HEADER = {"fork"}
EXTRA_SCHEMAS = {
    "Placement": {"type": "object", "description": "Hard requirements on the node that hosts the sandbox (CrowdVM extension).",
                  "properties": {"gpu": {"type": "boolean"}, "nested": {"type": "boolean"},
                                 "labels": {"type": "object", "additionalProperties": {"type": "string"}},
                                 "minTier": {"type": "string", "enum": ["bronze", "silver", "gold", "platinum"]},
                                 "categories": {"type": "object", "additionalProperties": {"type": "string"}},
                                 "node": {"type": "string"}}},
    "BrowserRequest": {"type": "object", "properties": {"profile": {"type": "string", "description": "Absolute path of the Chrome profile folder in the sandbox."}}},
    "BrowserResponse": {"type": "object", "additionalProperties": True,
                        "properties": {"ok": {"type": "boolean"}, "type": {"type": "string"}, "browserUrl": {"type": "string"},
                                       "viewerUrl": {"type": "string"}, "wsUrl": {"type": "string"}, "token": {"type": "string"},
                                       "profile": {"type": "string"}, "expiresAt": {"type": "string"},
                                       "expiresInSeconds": {"type": "integer"}, "mode": {"type": "string"}}},
    "ProviderModelsResponse": {"type": "object", "additionalProperties": True,
                               "properties": {"ok": {"type": "boolean"}, "type": {"type": "string"},
                                              "providers": {"type": "array", "items": {"type": "object", "additionalProperties": True}}}},
}
SCHEMAS.update(EXTRA_SCHEMAS)

HEADER_RENAME = {}
SKIP_HEADERS = set()
# `confirm_delete` / `confirmDelete` stays as a CrowdVM alias of Boat's `x_ascii_confirm_delete` / `xAsciiConfirmDelete`.
CONFIRM_ALIAS_HEADER = "X-CrowdVM-Confirm-Delete"
RET_BYTES = {"artifact", "getSnapshotFile"}
RET_NDJSON = {"attachCommand"}
# The spec lists these as required, the docs say they are optional (server default applies).
RELAX_REQUIRED = {"prompt": {"provider"}}
CONFIRM_DEFAULT = {"deleteSandbox": "sandboxId", "deleteSnapshot": "snapshotId",
                   "deleteSandboxSnapshots": "sandboxId"}


def rebrand(text):
    if not text:
        return ""
    text = re.sub(r"\bBoat\b", "CrowdVM", text)
    text = re.sub(r"\bboat\b", "crowdvm", text)
    text = text.replace("BOAT_", "CROWDVM_").replace("boat_", "cvm_")
    return text


def first_para(text, limit=220):
    text = rebrand((text or "").strip())
    text = re.sub(r"\s+", " ", text.split("\n\n")[0])
    if len(text) > limit:
        text = text[: limit - 1].rsplit(" ", 1)[0] + "…"
    return text


def snake(name):
    s = re.sub(r"(?<=[a-z0-9])([A-Z])", r"_\1", name)
    s = re.sub(r"([A-Z]+)([A-Z][a-z])", r"\1_\2", s)
    return s.lower().replace("-", "_")


def camel_from_header(h):
    return snake(h.replace("X-", "", 1) if h.startswith("X-") else h)


def pascal(name):
    return name[0].upper() + name[1:]


def resolve(s):
    while isinstance(s, dict) and "$ref" in s:
        s = SCHEMAS[s["$ref"].split("/")[-1]]
    return s


# ---------------------------------------------------------------- TypeScript types
IDENT = re.compile(r"^[A-Za-z_$][A-Za-z0-9_$]*$")


def ts_key(k):
    return k if IDENT.match(k) else json.dumps(k)


def ts_paren(t):
    return f"({t})" if (" | " in t or " & " in t) else t


def ts_type(s, indent=0):
    if s is None or s is True or s == {}:
        return "unknown"
    if "$ref" in s:
        return s["$ref"].split("/")[-1]
    if "allOf" in s:
        return " & ".join(ts_paren(ts_type(x, indent)) for x in s["allOf"])
    for k in ("oneOf", "anyOf"):
        if k in s:
            return " | ".join(ts_paren(ts_type(x, indent)) for x in s[k])
    if "const" in s:
        return json.dumps(s["const"])
    t = s.get("type")
    if isinstance(t, list):
        parts = []
        for tt in t:
            if tt == "null":
                parts.append("null")
            else:
                parts.append(ts_type({**s, "type": tt}, indent))
        return " | ".join(parts)
    if "enum" in s:
        return " | ".join(json.dumps(v) for v in s["enum"])
    if t in ("integer", "number"):
        return "number"
    if t == "string":
        return "string"
    if t == "boolean":
        return "boolean"
    if t == "null":
        return "null"
    if t == "array":
        item = ts_paren(ts_type(s.get("items"), indent))
        if s.get("uniqueItems"):  # Boat's samples pass a Set here (sent as an array)
            return item + "[] | Set<" + item + ">"
        return item + "[]"
    if t == "object" or "properties" in s:
        return ts_object(s, indent)
    return "unknown"


def ts_object(s, indent=0, docs=False):
    props = s.get("properties", {})
    req = set(s.get("required", []))
    ap = s.get("additionalProperties")
    pad = "  " * (indent + 1)
    lines = []
    for k, v in props.items():
        if docs and v.get("description"):
            lines.append(f"{pad}/** {first_para(v['description']).replace('*/', '*\\/')} */")
        lines.append(f"{pad}{ts_key(k)}{'' if k in req else '?'}: {ts_type(v, indent + 1)};")
    if ap is True or (ap is None and not props):
        lines.append(f"{pad}[key: string]: unknown;")
    elif isinstance(ap, dict):
        if props:
            lines.append(f"{pad}[key: string]: unknown;")
        else:
            return f"Record<string, {ts_type(ap, indent)}>"
    if not lines:
        return "Record<string, never>"
    return "{\n" + "\n".join(lines) + "\n" + "  " * indent + "}"


def gen_ts_types():
    out = ["// Generated by sdk/tools/gen.py from the OpenAPI file. Do not edit.\n"]
    for name, s in SCHEMAS.items():
        desc = first_para(s.get("description", ""))
        if desc:
            out.append(f"/** {desc.replace('*/', '*\\/')} */")
        if s.get("type") == "object" and s.get("properties") and "allOf" not in s:
            out.append(f"export interface {name} {ts_object(s, 0, docs=True)}\n")
        else:
            out.append(f"export type {name} = {ts_type(s)};\n")
    return "\n".join(out)


# ---------------------------------------------------------------- Python types
class PyTypes:
    def __init__(self):
        self.classes = []   # rendered source chunks
        self.names = set()
        self.emit = True    # False once the types module is written: inline objects become plain dicts

    def unique(self, base):
        n, i = base, 2
        while n in self.names:
            n = f"{base}{i}"
            i += 1
        self.names.add(n)
        return n

    def flatten(self, s):
        """Merge refs/allOf into (props, required) or None when not a plain object."""
        s = resolve(s)
        if "allOf" in s:
            props, req = {}, set()
            for part in s["allOf"]:
                f = self.flatten(part)
                if f is None:
                    return None
                props.update(f[0]); req |= f[1]
            return props, req
        if s.get("type") == "object" or "properties" in s:
            return s.get("properties", {}), set(s.get("required", []))
        return None

    def typ(self, s, ctx):
        if s is None or s is True or s == {}:
            return "Any"
        if "$ref" in s:
            return s["$ref"].split("/")[-1]
        if "allOf" in s:
            f = self.flatten(s)
            if f is None or not self.emit:
                return "Dict[str, Any]"
            return self.emit_td(self.unique(ctx), f[0], f[1], s.get("additionalProperties"))
        for k in ("oneOf", "anyOf"):
            if k in s:
                return "Union[" + ", ".join(self.typ(x, ctx + "Option") for x in s[k]) + "]"
        if "const" in s:
            return f"Literal[{json.dumps(s['const'])}]"
        t = s.get("type")
        if isinstance(t, list):
            parts = [("None" if tt == "null" else self.typ({**s, "type": tt}, ctx)) for tt in t]
            return "Union[" + ", ".join(parts) + "]" if len(parts) > 1 else parts[0]
        if "enum" in s:
            return "Literal[" + ", ".join(json.dumps(v) for v in s["enum"]) + "]"
        if t == "integer":
            return "int"
        if t == "number":
            return "float"
        if t == "string":
            return "str"
        if t == "boolean":
            return "bool"
        if t == "null":
            return "None"
        if t == "array":
            return f"List[{self.typ(s.get('items'), ctx + 'Item')}]"
        if t == "object" or "properties" in s:
            props = s.get("properties")
            if not props or not self.emit:
                ap = s.get("additionalProperties")
                if isinstance(ap, dict):
                    return f"Dict[str, {self.typ(ap, ctx + 'Value')}]"
                return "Dict[str, Any]"
            return self.emit_td(self.unique(ctx), props, set(s.get("required", [])), s.get("additionalProperties"))
        return "Any"

    def emit_td(self, name, props, req, ap=None, doc=""):
        self.names.add(name)
        fields = {k: self.typ(v, name + pascal(re.sub(r"[^A-Za-z0-9]", "", k) or "X")) for k, v in props.items()}
        reqf = {k: v for k, v in fields.items() if k in req}
        optf = {k: v for k, v in fields.items() if k not in req}
        simple = all(IDENT.match(k) and not keyword.iskeyword(k) for k in fields)
        docline = f'    """{doc}"""\n' if doc else ""
        if simple:
            src = []
            base = "TypedDict"
            if reqf:
                src.append(f"class _{name}Required(TypedDict):\n" + "".join(f"    {k}: {t}\n" for k, t in reqf.items()))
                base = f"_{name}Required"
            if optf or not reqf:
                body = "".join(f"    {k}: {t}\n" for k, t in optf.items()) or "    pass\n"
                src.append(f"class {name}({base}, total=False):\n{docline}{body}")
            else:
                src[-1] = src[-1].replace(f"_{name}Required", name)
            chunk = "\n\n".join(src)
        else:
            items = ", ".join(f"{json.dumps(k)}: {json.dumps(t)}" for k, t in fields.items())
            chunk = f"{name} = TypedDict({json.dumps(name)}, {{{items}}}, total=False)"
        self.classes.append(chunk)
        return name


def gen_py_types():
    pt = PyTypes()
    aliases = []
    for name in SCHEMAS:
        pt.names.add(name)
    for name, s in SCHEMAS.items():
        desc = first_para(s.get("description", "")).replace('"""', "'''").replace("\\", "/")
        f = pt.flatten(s)
        if f is not None and (s.get("type") == "object" or "allOf" in s or "properties" in s):
            pt.emit_td(name, f[0], f[1], s.get("additionalProperties"), desc)
        else:
            aliases.append(f"{name} = {pt.typ(s, name)}")
    pt.emit = False
    header = ('"""Typed shapes of the CrowdVM API (generated by sdk/tools/gen.py; do not edit).\n\n'
              'Responses are plain dicts at runtime (with attribute access, see `crowdvm.Obj`);\n'
              'these TypedDicts describe their keys for type checkers.\n"""\n'
              "from __future__ import annotations\n\n"
              "from typing import Any, Dict, List, Literal, Union\n"
              "from typing import TypedDict\n\n")
    return header + "\n\n".join(pt.classes) + "\n\n\n" + "\n".join(aliases) + "\n", pt


# ---------------------------------------------------------------- operations
def collect_ops():
    ops = []
    for path, item in spec["paths"].items():
        for method, op in item.items():
            if method not in ("get", "post", "put", "patch", "delete"):
                continue
            plist, boat_order = [], []
            for pr in item.get("parameters", []) + op.get("parameters", []):
                if "$ref" in pr:
                    pr = PARAMS[pr["$ref"].split("/")[-1]]
                plist.append(pr)
            # Boat's generated SDKs list the operation's own parameters first, then the path item's
            for pr in op.get("parameters", []) + item.get("parameters", []):
                pr = PARAMS[pr["$ref"].split("/")[-1]] if "$ref" in pr else pr
                if pr["name"] not in boat_order:
                    boat_order.append(pr["name"])
            body = None
            rb = op.get("requestBody")
            if rb and "application/json" in rb.get("content", {}):
                body = {"schema": rb["content"]["application/json"]["schema"], "required": bool(rb.get("required"))}
            ok = {c: r for c, r in op["responses"].items() if c.startswith("2")}
            ret_type, ret_schema = None, None
            for c, r in ok.items():
                cc = r.get("content", {}) if isinstance(r, dict) else {}
                if "application/json" in cc:
                    ret_schema = cc["application/json"]["schema"]
                    ret_type = ts_type(ret_schema)
            if op["operationId"] in EXTRA_ORG_HEADER and not any(p["name"] == "X-Boat-Org" for p in plist):
                plist.append({"name": "X-Boat-Org", "in": "header", "required": False, "schema": {"type": "string"}})
            ops.append(dict(operationId=op["operationId"], method=method, path=path,
                            summary=op.get("summary", op["operationId"]), params=plist, body=body, ret_type=ret_type,
                            ret_schema=ret_schema, boat_order=boat_order))
    for e in EXTRA_OPS:
        e = dict(e); e["path"] = e["path"]
        ops.append(e)
    return ops


def prep_op(op, pt):
    """Normalise an operation into the table used by both generators."""
    path_params = [m for m in re.findall(r"\{(\w+)\}", op["path"])]
    query, headers = [], []
    for pr in op["params"]:
        if pr["in"] == "query":
            query.append(pr)
        elif pr["in"] == "header" and pr["name"] not in SKIP_HEADERS and pr["name"] != "Authorization":
            headers.append(pr)
    body_props, body_req, body_mode = {}, set(), "none"
    if op["body"]:
        body_mode = "required" if op["body"]["required"] else "optional"
        f = pt.flatten(op["body"]["schema"])
        if f:
            body_props, body_req = f
            body_req = set(body_req) - RELAX_REQUIRED.get(op["operationId"], set())
    body_props = dict(body_props, **EXTRA_BODY_PROPS.get(op["operationId"], {}))
    body_param = None
    if op["body"]:
        # Boat's generators name an inline body <OperationId>Request
        body_param = (op["body"]["schema"]["$ref"].split("/")[-1] if "$ref" in op["body"]["schema"]
                      else pascal(op["operationId"]) + "Request")
    # a body field of the same name (e.g. `org` on create) wins over the query parameter
    query = [q for q in query if q["name"] not in body_props]
    ret = "json"
    if op["operationId"] in RET_BYTES:
        ret = "bytes"
    elif op["operationId"] in RET_NDJSON:
        ret = "ndjson"
    return dict(op, path_params=path_params, query=query, headers=headers, body_props=body_props,
                body_req=body_req, body_mode=body_mode, ret=ret, body_param=body_param)


def py_ident(name):
    n = snake(name)
    if name == "from":
        return "var_from"
    return n + "_" if keyword.iskeyword(n) or n in ("self", "extra") else n


def hdr_name(h):
    return HEADER_RENAME.get(h, h)


def boat_signature(o):
    """Parameter names of the method in Boat's generated Python SDK, in its positional order:
    required parameters, the required body model, optional parameters, the optional body model."""
    params = {pr["name"]: pr for pr in o["params"]}
    def name(p):
        return snake(p) if params[p]["in"] == "header" else py_ident(p)
    req = [name(p) for p in o.get("boat_order", []) if p in params and params[p].get("required")]
    opt = [name(p) for p in o.get("boat_order", []) if p in params and not params[p].get("required")]
    if o.get("body_param"):
        (req if o["body_mode"] == "required" else opt).append(snake(o["body_param"]))
    return [[n, True] for n in req] + [[n, False] for n in opt]


def gen_py_ops(pt):
    boat_sigs = {snake(o["operationId"]): boat_signature(prep_op(o, pt)) for o in collect_ops()}
    ops = [prep_op(o, pt) for o in collect_ops() if o["operationId"] not in MANUAL_OPS]
    table, methods = [], []
    for o in ops:
        name = snake(o["operationId"])
        pnames = {}  # python name -> (kind, wire)
        sig_pos, sig_req_kw, sig_kw = [], [], []
        for p in o["path_params"]:
            pnames[py_ident(p)] = ("path", p)
            sig_pos.append(f"{py_ident(p)}: str" if p not in ("processId",) else f"{py_ident(p)}: int")
        # required body props are positional-or-keyword after the path params
        req_body = [k for k in o["body_props"] if k in o["body_req"]]
        opt_body = [k for k in o["body_props"] if k not in o["body_req"]]
        # With a body model, required body fields may come from the model instead
        # (Boat style: `host_port(sandbox_id, HostPortRequest(port=8080))`); `_op` checks them.
        dflt = " = UNSET" if o["body_param"] else ""
        for k in req_body:
            n = py_ident(k)
            if n in pnames:
                raise SystemExit(f"collision {o['operationId']}: {n}")
            pnames[n] = ("body", k)
            sig_pos.append(f"{n}: {pt.typ(o['body_props'][k], 'X')}{dflt}")
        for pr in o["query"]:
            n = py_ident(pr["name"])
            if n in pnames:
                raise SystemExit(f"collision {o['operationId']}: {n}")
            pnames[n] = ("query", pr["name"])
            t = pt.typ(pr["schema"], "Q")
            if pr.get("required"):
                sig_pos.append(f"{n}: {t}{dflt if req_body else ''}")
            else:
                sig_kw.append(f"{n}: {t} = UNSET")
        for pr in o["headers"]:
            n = snake(pr["name"])
            pnames[n] = ("header", hdr_name(pr["name"]))
            sig_kw.append(f"{n}: str | None = None")
        if o["operationId"] in CONFIRM_DEFAULT:
            pnames["confirm_delete"] = ("header", CONFIRM_ALIAS_HEADER)
            sig_kw.append("confirm_delete: str | None = None")
        for k in opt_body:
            n = py_ident(k)
            if n in pnames:
                raise SystemExit(f"collision {o['operationId']}: {n}")
            pnames[n] = ("body", k)
            sig_kw.append(f"{n}: {pt.typ(o['body_props'][k], 'X').replace('Union[', 'Union[')} = UNSET")
        mapping = {"path": {}, "query": {}, "header": {}, "body": {}}
        for n, (kind, wire) in pnames.items():
            mapping[kind][n] = wire
        entry = dict(method=o["method"].upper(), path=o["path"], ret=o["ret"], body=o["body_mode"],
                     path_params=mapping["path"], query=mapping["query"], headers=mapping["header"],
                     body_fields=mapping["body"], required_body=[py_ident(k) for k in req_body],
                     required_query=[py_ident(pr["name"]) for pr in o["query"] if pr.get("required")])
        if o["operationId"] in CONFIRM_DEFAULT:
            entry["confirm_default"] = py_ident(CONFIRM_DEFAULT[o["operationId"]])
        if o["body_param"]:
            entry["body_param"] = snake(o["body_param"])
        table.append(f"    {json.dumps(name)}: {repr(entry)},")
        sig = ["self"] + sig_pos + (["*"] if sig_kw else []) + sig_kw + ["**extra: Any"]
        ret = pt.typ(o.get("ret_schema"), "R") if o.get("ret_schema") else "Any"
        ret = "bytes" if o["ret"] == "bytes" else ("Iterator[CommandAttachFrame]" if o["ret"] == "ndjson" else ret)
        summary = rebrand(o["summary"]).replace('"""', "'")
        methods.append(
            f"    def {name}({', '.join(sig)}) -> {ret}:\n"
            f'        """{summary}  ``{o["method"].upper()} {o["path"]}``"""\n'
            f"        return self._op({json.dumps(name)}, locals())\n")
    src = ('"""Operation table and generated client methods (generated by sdk/tools/gen.py; do not edit)."""\n'
           "from __future__ import annotations\n\n"
           "from typing import Any, Dict, Iterator, List, Literal, Union\n\n"
           "from ._types import *  # noqa: F401,F403\n"
           "from ._types import CommandAttachFrame\n"
           "from ._util import UNSET\n\n\n"
           "OPS: Dict[str, Dict[str, Any]] = {\n" + "\n".join(table) + "\n}\n\n"
           "# Parameter names (and whether required) in the positional order of Boat's generated SDK.\n"
           "BOAT_SIGNATURES: Dict[str, List[Any]] = {\n"
           + "\n".join(f"    {json.dumps(k)}: {json.dumps(v).replace('true', 'True').replace('false', 'False')}," for k, v in boat_sigs.items())
           + "\n}\n\n\n"
           "class _Operations:\n"
           '    """One method per API operation. Mixed into :class:`crowdvm.CrowdVM`."""\n\n'
           "    def _op(self, name: str, args: Dict[str, Any]) -> Any:  # pragma: no cover - set by CrowdVM\n"
           "        raise NotImplementedError\n\n" + "\n".join(methods))
    return src, [snake(o["operationId"]) for o in ops]


def ts_param_type(o, pt_unused=None):
    """Params interface for a TS method."""
    parts = []
    for p in o["path_params"]:
        t = "number" if p == "processId" else "string"
        parts.append(f"{p}: {t};")
    for pr in o["query"]:
        t = ts_type(pr["schema"])
        if pr["name"] in ("type",) and "string" in t and o["operationId"] == "events":
            t = "string | string[]"
        parts.append(f"{ts_key(pr['name'])}{'' if pr.get('required') else '?'}: {t};")
    for pr in o["headers"]:
        parts.append(f"{ts_key(camel_hdr(pr['name']))}?: string;")
    if o["operationId"] in CONFIRM_DEFAULT:
        parts.append("confirmDelete?: string;")
    if o.get("body_param"):
        bp = o["body_param"]
        parts.append(f"{bp[0].lower() + bp[1:]}?: {bp if bp in SCHEMAS else 'Record<string, unknown>'};")
    own = "{ " + " ".join(parts) + " }" if parts else "{}"
    body = ""
    if o["body_props"]:
        props = o["body_props"]
        req = o["body_req"]
        bparts = []
        for k, v in props.items():
            bparts.append(f"{ts_key(k)}{'' if k in req else '?'}: {ts_type(v)};")
            if k in TS_RESERVED:  # Boat's TS client spells a reserved word with a leading underscore
                bparts.append(f"_{k}?: {ts_type(v)};")
        body = " & { " + " ".join(bparts) + " }"
    elif o["body"]:
        body = " & Record<string, unknown>"
    return own + body


TS_RESERVED = {"public", "private", "protected", "package", "interface", "implements", "static"}


def camel_hdr(h):
    return {"Idempotency-Key": "idempotencyKey", "If-None-Match": "ifNoneMatch"}.get(
        h, re.sub(r"-(\w)", lambda m: m.group(1).upper(), h.lower()))


def gen_ts_ops(pt):
    ops = [prep_op(o, pt) for o in collect_ops() if o["operationId"] not in MANUAL_OPS]
    table, methods = [], []
    for o in ops:
        name = o["operationId"]
        entry = dict(method=o["method"].upper(), path=o["path"], ret=o["ret"], body=o["body_mode"],
                     pathParams=o["path_params"], query=[pr["name"] for pr in o["query"]],
                     headers={camel_hdr(pr["name"]): hdr_name(pr["name"]) for pr in o["headers"]})
        if name in CONFIRM_DEFAULT:
            entry["confirmDefault"] = CONFIRM_DEFAULT[name]
            entry["headers"]["confirmDelete"] = CONFIRM_ALIAS_HEADER
        if o["body_param"]:
            entry["bodyParam"] = o["body_param"][0].lower() + o["body_param"][1:]
        table.append(f"  {name}: {json.dumps(entry)},")
        ptype = ts_param_type(o)
        has_required = bool(o["path_params"]) or any(pr.get("required") for pr in o["query"]) or bool(o["body_req"])
        param = f"params: {ptype}" + ("" if has_required else " = {}")
        if ptype == "{}":
            param = "params: {} = {}"
        ret = o["ret_type"] or "unknown"
        if o["ret"] == "bytes":
            ret = "Blob"
        elif o["ret"] == "ndjson":
            ret = "AsyncGenerator<CommandAttachFrame, void, undefined>"
        elif ret.count("\n"):
            ret = "unknown"
        wrapper = f"Promise<{ret}>" if o["ret"] != "ndjson" else ret
        summary = rebrand(o["summary"]).replace("*/", "*\\/")
        if o["ret"] == "ndjson":
            methods.append(f"  /** {summary} (`{o['method'].upper()} {o['path']}`) */\n"
                           f"  {name}({param}, options?: RequestOptions): {wrapper} {{\n"
                           f"    return this._ndjson(OPERATIONS.{name}, params, options) as {wrapper};\n  }}\n")
        else:
            methods.append(f"  /** {summary} (`{o['method'].upper()} {o['path']}`) */\n"
                           f"  {name}({param}, options?: RequestOptions): {wrapper} {{\n"
                           f"    return this._op(OPERATIONS.{name}, params, options) as {wrapper};\n  }}\n")
    src = ("// Generated by sdk/tools/gen.py from the OpenAPI file. Do not edit.\n"
           'import type * as T from "./types.js";\n'
           'import type { OperationSpec, RequestOptions } from "./core.js";\n\n'
           "export const OPERATIONS = {\n" + "\n".join(table) + "\n} as const satisfies Record<string, OperationSpec>;\n\n"
           "export abstract class GeneratedOperations {\n"
           "  protected abstract _op(spec: OperationSpec, params: object, options?: RequestOptions): Promise<unknown>;\n"
           "  protected abstract _ndjson(spec: OperationSpec, params: object, options?: RequestOptions): AsyncGenerator<any, void, undefined>;\n\n"
           + "\n".join(methods) + "}\n")
    # qualify type names with the T. namespace
    names = sorted(SCHEMAS, key=len, reverse=True)
    pat = re.compile(r"(?<![\w.\"'])(" + "|".join(map(re.escape, names)) + r")(?![\w\"'])")
    head, rest = src.split("export abstract class", 1)
    rest = "\n".join(l if l.lstrip().startswith("/**") else pat.sub(r"T.\1", l) for l in rest.split("\n"))
    return head + "export abstract class" + rest, [o["operationId"] for o in ops]


def gen_methods_md():
    pt = PyTypes()
    pt.emit = False
    rows = ["# SDK methods", "", "Every operation of the CrowdVM API. Python is snake_case, synchronous; TypeScript is camelCase, async,",
            "one flat params object. Generated by `sdk/tools/gen.py`.", "",
            "| Python | TypeScript | Endpoint | Does |", "| - | - | - | - |"]
    ops = [prep_op(o, pt) for o in collect_ops()]
    for o in ops:
        n = o["operationId"]
        rows.append(f"| `{snake(n)}` | `{n}` | `{o['method'].upper()} {o['path']}` | {rebrand(o['summary'])} |")
    return "\n".join(rows) + "\n"


def main():
    py_types, pt = gen_py_types()
    open(os.path.join(PY_OUT, "_types.py"), "w").write(py_types)
    py_ops, py_names = gen_py_ops(pt)
    open(os.path.join(PY_OUT, "_ops.py"), "w").write(py_ops)
    open(os.path.join(TS_OUT, "types.ts"), "w").write(gen_ts_types())
    ts_ops, ts_names = gen_ts_ops(pt)
    open(os.path.join(TS_OUT, "operations.ts"), "w").write(ts_ops)
    open(os.path.join(ROOT, "sdk", "METHODS.md"), "w").write(gen_methods_md())
    print(f"python: {len(py_names)} generated operations; typescript: {len(ts_names)}")


if __name__ == "__main__":
    main()
