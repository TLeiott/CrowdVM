/**
 * Eve sandbox backend for CrowdVM: `crowdvmBox(options)`, the counterpart of `@asciidev/eve-box`'s `asciiBox()`.
 *
 * ```ts
 * import { defineSandbox } from "eve/sandbox";
 * import { crowdvmBox } from "@crowdvm/eve";
 * export default defineSandbox({ backend: crowdvmBox({ apiKey: process.env.CROWDVM_API_KEY!, ttlSeconds: 3600 }) });
 * ```
 *
 * It talks to `/api/v1` through `@crowdvm/sdk`.  Eve's backend types are described by the small structural
 * interfaces below (the `eve` package is not a dependency): `create` -> handle -> `useSessionFn` -> session
 * with run / spawn / read|write text|binary files / removePath / resolvePath.
 *
 * Eve's `/workspace` is `~/workspace` of the sandbox user (a `/workspace` symlink is added when the sandbox
 * has sudo), so paths inside commands and through the file calls agree.  Only the `allow-all` network policy
 * is supported; anything else throws {@link EveSandboxUnsupportedError} instead of faking isolation.
 */
import { CrowdVM, execCommand, followCommand, waitUntilReady } from "@crowdvm/sdk";

export const WORKSPACE = "/workspace";
const INLINE_LIMIT = 5 * 1024 * 1024;
const SYNC_LIMIT_S = 600;

export class EveSandboxUnsupportedError extends Error {
  constructor(message: string) { super(message); this.name = "EveSandboxUnsupportedError"; }
}

export type NetworkPolicy = "allow-all" | "deny-all" | { allow?: string[]; [k: string]: unknown } | string;

export interface CrowdVMBoxOptions {
  /** API key (`cvm_...`). Default: `CROWDVM_API_KEY` (then `BOX_API_KEY`). */
  apiKey?: string;
  /** e.g. `http://192.168.1.10:8420/api/v1`. Default: `CROWDVM_API_BASE`. */
  baseUrl?: string;
  /** Sandbox name, or `(input) => name`. Default `eve-<sessionKey>`. */
  name?: string | ((input: CreateInput) => string);
  /** Auto-stop in seconds (default 3600, `null` = never). */
  ttlSeconds?: number | null;
  /** Keep your account secrets out of the sandbox (use this when other people steer the agent). */
  noEnv?: boolean;
  /** Environment variables of every sandbox (merged over your account variables). */
  env?: Record<string, string>;
  /** Accepted for option compatibility; `spawn` streams over an attached connection instead of polling. */
  pollMs?: number;
  /** Timeout of blocking `run()` commands (default 600 s, longer ones run detached). */
  commandTimeoutMs?: number;
  /** Initial network policy; only `"allow-all"` (or none) is supported. */
  networkPolicy?: NetworkPolicy;
  /** Create sandboxes from this saved snapshot (also what `templateKey` selects when given). */
  fromSnapshot?: string;
  /** Machine size (`small`, `default`, `large`, `xlarge`). */
  type?: "small" | "default" | "large" | "xlarge";
  /** An existing client (tests). */
  client?: CrowdVM;
}

export interface CreateInput {
  templateKey?: string | null;
  sessionKey: string;
  runtimeContext?: unknown;
}

export interface RunInput { command: string; cwd?: string; env?: Record<string, string>; timeoutMs?: number }
export interface RunResult { exitCode: number; stdout: string; stderr: string; timedOut?: boolean }
export interface SpawnedProcess {
  pid: number;
  stdout: AsyncIterable<string>;
  stderr: AsyncIterable<string>;
  wait(): Promise<{ exitCode: number | null }>;
  kill(): Promise<void>;
}
export interface EveSandboxSession {
  run(input: RunInput): Promise<RunResult>;
  spawn(input: RunInput): Promise<SpawnedProcess>;
  readTextFile(input: { path: string }): Promise<string>;
  writeTextFile(input: { path: string; content: string }): Promise<void>;
  readBinaryFile(input: { path: string }): Promise<Uint8Array>;
  writeBinaryFile(input: { path: string; content: Uint8Array }): Promise<void>;
  removePath(input: { path: string; recursive?: boolean; force?: boolean }): Promise<void>;
  resolvePath(path: string): string;
}
export interface EveSandboxHandle {
  sandboxId: string;
  /** Serializable state for {@link CrowdVMBackend.resume}. */
  state: { sandboxId: string };
  useSessionFn(options?: { networkPolicy?: NetworkPolicy }): Promise<EveSandboxSession>;
  destroy(): Promise<void>;
}
export interface CrowdVMBackend {
  readonly name: "crowdvm";
  create(input: CreateInput): Promise<EveSandboxHandle>;
  resume(state: { sandboxId: string }): Promise<EveSandboxHandle>;
}

const q = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`;
const envOf = (name: string) => (globalThis as any).process?.env?.[name] as string | undefined;

/** Close-enough POSIX normalisation (no node:path so the package also loads in other runtimes). */
function normalize(p: string): string {
  const out: string[] = [];
  for (const part of p.split("/")) {
    if (part === "" || part === ".") continue;
    if (part === "..") out.pop(); else out.push(part);
  }
  return "/" + out.join("/");
}

export function resolvePath(p: string): string {
  return normalize(p.startsWith("/") ? p : `${WORKSPACE}/${p}`);
}

/** The path as the sandbox API wants it: `/workspace/x` -> `workspace/x` (relative to /home/user). */
function apiPath(p: string): string {
  const abs = resolvePath(p);
  return abs === WORKSPACE ? "workspace" : abs.startsWith(WORKSPACE + "/") ? abs.slice(1) : abs;
}

function checkPolicy(policy: NetworkPolicy | undefined): void {
  if (policy === undefined || policy === "allow-all") return;
  throw new EveSandboxUnsupportedError(
    `CrowdVM enforces no network policies (got ${JSON.stringify(policy)}); only "allow-all" is supported. ` +
    "Run this agent on a backend that supports the policy.");
}

class Queue implements AsyncIterable<string> {
  private items: string[] = [];
  private waiter?: () => void;
  private done = false;
  push(s: string) { this.items.push(s); this.waiter?.(); }
  end() { this.done = true; this.waiter?.(); }
  async *[Symbol.asyncIterator](): AsyncGenerator<string, void, undefined> {
    for (;;) {
      if (this.items.length) { yield this.items.shift()!; continue; }
      if (this.done) return;
      await new Promise<void>((r) => { this.waiter = r; });
    }
  }
}

export function crowdvmBox(options: CrowdVMBoxOptions = {}): CrowdVMBackend {
  checkPolicy(options.networkPolicy);
  const cvm = options.client ?? new CrowdVM({
    apiKey: options.apiKey ?? envOf("CROWDVM_API_KEY") ?? envOf("BOX_API_KEY"), baseUrl: options.baseUrl, maxRetries: 6,
  });

  function handle(sandboxId: string): EveSandboxHandle {
    const exec = (command: string, timeoutSeconds?: number) => execCommand(cvm, sandboxId, command, { timeoutSeconds });

    async function bytesOut(path: string): Promise<Uint8Array> {
      const info = await cvm.getFileInfo({ sandboxId, path });
      if ((info.size ?? 0) <= INLINE_LIMIT) {
        const r = await cvm.readFile({ sandboxId, path, encoding: "base64" });
        return Uint8Array.from(Buffer.from(r.content, "base64"));
      }
      const { url } = await cvm.createDownloadUrl({ sandboxId, path });
      return new Uint8Array(await (await fetch(url)).arrayBuffer());
    }

    async function bytesIn(path: string, data: Uint8Array): Promise<void> {
      const parent = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
      if (parent) await exec(`mkdir -p ${q(parent)}`);
      if (data.length <= INLINE_LIMIT) {
        await cvm.writeFile({ sandboxId, path, content: Buffer.from(data).toString("base64"), encoding: "base64" });
        return;
      }
      const { url } = await cvm.createUploadUrl({ sandboxId, path });
      const res = await fetch(url, { method: "PUT", body: data as any });
      if (!res.ok) throw new Error(`upload of ${path} failed: ${res.status}`);
    }

    function script(input: RunInput): { command: string; cwd?: string } {
      let command = input.command;
      if (input.env) command = Object.entries(input.env).map(([k, v]) => `export ${k}=${q(String(v))}; `).join("") + command;
      const cwd = input.cwd ? apiPath(input.cwd) : "workspace";
      const ok = !cwd.startsWith("/") || cwd.startsWith("/home/user/") || cwd.startsWith("/tmp/") || cwd === "/tmp";
      return ok ? { command, cwd } : { command: `cd ${q(cwd)} && ${command}` };
    }

    const session: EveSandboxSession = {
      resolvePath,
      async run(input) {
        const ms = input.timeoutMs ?? options.commandTimeoutMs;
        const s = script(input);
        if (ms === undefined || ms <= SYNC_LIMIT_S * 1000) {
          const r = await execCommand(cvm, sandboxId, s.command, { cwd: s.cwd, timeoutSeconds: ms === undefined ? SYNC_LIMIT_S : Math.max(1, Math.ceil(ms / 1000)) });
          return { exitCode: r.timedOut ? 124 : (r.exitCode ?? 0), stdout: r.stdout ?? "", stderr: r.stderr ?? "", timedOut: !!r.timedOut };
        }
        const p = await session.spawn({ ...input, command: `timeout ${Math.ceil(ms / 1000)} bash -c ${q(input.command)}` });
        let stdout = "", stderr = "";
        const pumps = [(async () => { for await (const c of p.stdout) stdout += c; })(), (async () => { for await (const c of p.stderr) stderr += c; })()];
        const { exitCode } = await p.wait();
        await Promise.all(pumps);
        return { exitCode: exitCode ?? 1, stdout, stderr, timedOut: exitCode === 124 };
      },
      async spawn(input) {
        const s = script(input);
        const started = (await cvm.command({ sandboxId, command: s.command, cwd: s.cwd, detached: true }, { maxRetries: 20 })) as { processId: number };
        const out = new Queue(), err = new Queue();
        const finished = (async () => {
          let exitCode: number | null = null;
          try {
            for await (const f of followCommand(cvm, sandboxId, started.processId)) {
              if (f.type === "stdout") out.push(String(f.data));
              else if (f.type === "stderr") err.push(String(f.data));
              else if (f.type === "exit") exitCode = f.exitCode;
              else if (f.type === "error") err.push(String(f.message ?? f.error ?? ""));
            }
          } finally { out.end(); err.end(); }
          return { exitCode };
        })();
        return {
          pid: started.processId, stdout: out, stderr: err, wait: () => finished,
          kill: async () => { await cvm.stopCommand({ sandboxId, processId: started.processId }).catch(() => undefined); },
        };
      },
      async readTextFile({ path }) { return (await cvm.readFile({ sandboxId, path: apiPath(path), encoding: "utf8" })).content; },
      async writeTextFile({ path, content }) {
        const p = apiPath(path);
        const parent = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
        if (parent) await exec(`mkdir -p ${q(parent)}`);
        await cvm.writeFile({ sandboxId, path: p, content, encoding: "utf8" });
      },
      readBinaryFile: ({ path }) => bytesOut(apiPath(path)),
      writeBinaryFile: ({ path, content }) => bytesIn(apiPath(path), content),
      async removePath({ path, recursive = true, force = true }) {
        const abs = resolvePath(path);
        if (abs === "/" || abs === WORKSPACE) throw new Error(`refusing to remove ${abs}`);
        const r = await exec(`rm ${recursive ? "-r" : ""}${force ? "f" : ""} -- ${q(apiPath(path))}`);
        if (r.exitCode) throw new Error(`removePath ${path}: ${r.stderr}`);
      },
    };

    return {
      sandboxId, state: { sandboxId },
      async useSessionFn(o) { checkPolicy(o?.networkPolicy); return session; },
      async destroy() { await cvm.deleteSandbox({ sandboxId, xAsciiConfirmDelete: sandboxId }); },
    };
  }

  return {
    name: "crowdvm",
    async create(input) {
      const name = typeof options.name === "function" ? options.name(input) : options.name ?? `eve-${input.sessionKey}`;
      const from = input.templateKey ?? options.fromSnapshot ?? undefined;
      const { sandbox } = await cvm.create({
        name, type: options.type, ttlSeconds: options.ttlSeconds === undefined ? 3600 : options.ttlSeconds, noEnv: options.noEnv ?? false,
        env: options.env, from, labels: { "eve.session": input.sessionKey },
        idempotencyKey: `eve-${input.sessionKey}-${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`,
      });
      try {
        await waitUntilReady(cvm, sandbox.id);
        await execCommand(cvm, sandbox.id, `mkdir -p ~/workspace && { sudo -n ln -sfn "$HOME/workspace" ${WORKSPACE} 2>/dev/null || true; }`);
      } catch (e) {
        await cvm.deleteSandbox({ sandboxId: sandbox.id, xAsciiConfirmDelete: sandbox.id }).catch(() => undefined);
        throw e;
      }
      return handle(sandbox.id);
    },
    async resume(state) {
      const sb = (await cvm.get({ sandboxId: state.sandboxId })).sandbox;
      if (!["ready", "idle", "running", "provisioning", "provisioned", "cloning"].includes(sb.state)) {
        await cvm.resume({ sandboxId: sb.id, ttlSeconds: options.ttlSeconds === undefined ? 3600 : options.ttlSeconds });
      }
      await waitUntilReady(cvm, sb.id);
      return handle(sb.id);
    },
  };
}

/** Name used by `@asciidev/eve-box`, so existing sandbox definitions only change their import. */
export const asciiBox = crowdvmBox;
