/**
 * Helpers that combine several API calls (mirrors the helpers of Boat's SDK).
 * Each takes the client as its first argument: `await runCommand(cvm, sandboxId, "make")`.
 */
import { sleep } from "./client.js";
import type { CrowdVM } from "./client.js";
import { APIConnectionError, ApiError, PromptFailed, SandboxFailed, WaitTimeout } from "./errors.js";
import type * as T from "./types.js";

export interface WaitOptions {
  /** Give up after this long (default depends on the helper; `0` / `Infinity` = never). */
  timeoutMs?: number;
  /** Poll interval (default 2000). */
  intervalMs?: number;
  signal?: AbortSignal;
}

const READY = ["ready", "idle", "running"];
const FAILED = ["error", "cancelled"];
const PROMPT_TERMINAL = ["finished", "failed", "interrupted"];

async function poll<R>(fn: () => Promise<R | undefined>, what: string, o: WaitOptions, defaultTimeoutMs: number): Promise<R> {
  const timeout = o.timeoutMs ?? defaultTimeoutMs;
  const interval = o.intervalMs ?? 2000;
  const deadline = timeout > 0 && Number.isFinite(timeout) ? Date.now() + timeout : Infinity;
  for (;;) {
    const out = await fn();
    if (out !== undefined) return out;
    if (Date.now() + interval > deadline) throw new WaitTimeout(`timed out waiting for ${what}`);
    await sleep(interval, o.signal);
  }
}

// ------------------------------------------------------------------ waiting
/**
 * Wait for one of `states`. A node restart shows as `error` with `node_offline` for a few seconds and the
 * sandbox comes back: that is waited out; at the timeout it throws `SandboxFailed` with that error.
 */
async function waitState(cvm: CrowdVM, sandboxId: string, states: string[], what: string, o: WaitOptions, defaultTimeoutMs: number): Promise<T.Sandbox> {
  let offline: T.Sandbox | undefined;
  try {
    return await poll(async () => {
      const sb = (await cvm.get({ sandboxId }, { signal: o.signal })).sandbox;
      if (states.includes(sb.state)) return sb;
      offline = undefined;
      if (FAILED.includes(sb.state)) {
        if (sb.state !== "error" || !String(sb.error ?? "").startsWith("node_offline")) throw new SandboxFailed(sb);
        offline = sb;
      }
      return undefined;
    }, what, o, defaultTimeoutMs);
  } catch (e) {
    if (e instanceof WaitTimeout && offline) throw new SandboxFailed(offline);
    throw e;
  }
}

/** Wait until the sandbox is `ready`, `idle` or `running`; returns it. Throws `SandboxFailed` / `WaitTimeout`. An `error` of `node_offline` (the node restarts) is waited out and only fails at the timeout. */
export function waitUntilReady(cvm: CrowdVM, sandboxId: string, o: WaitOptions = {}): Promise<T.Sandbox> {
  return waitState(cvm, sandboxId, READY, `sandbox ${sandboxId} to be ready`, o, 300_000);
}

/** Wait until the sandbox is `ready` or `idle` (no integrated-agent turn running). */
export function waitUntilIdle(cvm: CrowdVM, sandboxId: string, o: WaitOptions = {}): Promise<T.Sandbox> {
  return waitState(cvm, sandboxId, ["ready", "idle"], `sandbox ${sandboxId} to be idle`, o, 600_000);
}

/** Ask for the VNC desktop URL until the desktop is prepared. */
export function waitForDesktop(cvm: CrowdVM, sandboxId: string, o: WaitOptions & { theme?: "light" | "dark"; publicAccess?: boolean } = {}): Promise<T.DesktopResponse> {
  const { theme, publicAccess } = o;
  return poll(async () => {
    try {
      const out = await cvm.desktop({ sandboxId, vnc: 1, theme, publicAccess }, { signal: o.signal });
      return out.provisioning || !out.desktopUrl ? undefined : out;
    } catch (e) {
      if (e instanceof ApiError && [400, 409, 503].includes(e.status)) return undefined;
      throw e;
    }
  }, `the desktop of ${sandboxId}`, o, 120_000);
}

/** Wait until a prompt run is done; returns its `PromptRun` (read `.status`). */
export function waitForPrompt(cvm: CrowdVM, sandboxId: string, promptId: string, o: WaitOptions = {}): Promise<T.PromptRun> {
  return poll(async () => {
    const run = (await cvm.promptRunStatus({ sandboxId, promptId }, { signal: o.signal })).promptRun;
    return run.done || PROMPT_TERMINAL.includes(run.status) ? run : undefined;
  }, `prompt ${promptId}`, o, 0);
}

/** Like {@link waitForPrompt}, but throws `PromptFailed` unless the run `finished`. */
export async function waitForPromptDone(cvm: CrowdVM, sandboxId: string, promptId: string, o: WaitOptions = {}): Promise<T.PromptRun> {
  const run = await waitForPrompt(cvm, sandboxId, promptId, o);
  if (run.status !== "finished") throw new PromptFailed(run);
  return run;
}

// ------------------------------------------------------------------ files
/** Read a UTF-8 text file (path relative to `/home/user`). */
export async function readText(cvm: CrowdVM, sandboxId: string, path: string): Promise<string> {
  return (await cvm.readFile({ sandboxId, path, encoding: "utf8" })).content;
}

/** Write a UTF-8 text file (path relative to `/home/user`). */
export function writeText(cvm: CrowdVM, sandboxId: string, path: string, content: string): Promise<T.FileWriteResponse> {
  return cvm.writeFile({ sandboxId, path, content, encoding: "utf8" });
}

// ------------------------------------------------------------------ commands
export interface CommandOptions {
  cwd?: string;
  timeoutSeconds?: number;
  signal?: AbortSignal;
}

/**
 * Run a command and wait for it. Resends it while the sandbox restores its disk (the command
 * provably did not run) or is still starting.
 * `execCommand(cvm, id, "ls", "repo", 600)` and `execCommand(cvm, id, "ls", { cwd: "repo" })` both work.
 */
export function execCommand(cvm: CrowdVM, sandboxId: string, command: string, cwdOrOptions?: string | CommandOptions, timeoutSeconds?: number): Promise<T.CommandResponse> {
  const o: CommandOptions = typeof cwdOrOptions === "object" && cwdOrOptions ? cwdOrOptions : { cwd: cwdOrOptions, timeoutSeconds };
  return cvm.command(
    { sandboxId, command, cwd: o.cwd, timeoutSeconds: o.timeoutSeconds ?? timeoutSeconds },
    { signal: o.signal, maxRetries: Math.max(cvm.maxRetries, 20) },
  ) as Promise<T.CommandResponse>;
}

/** Yield the NDJSON frames of a command (`started`, `stdout`/`stderr`, then `exit` or `error`). Limited to 600 s. */
export function streamCommand(cvm: CrowdVM, sandboxId: string, command: string, o: CommandOptions = {}): AsyncGenerator<T.CommandStreamFrame, void, undefined> {
  return cvm.command({ sandboxId, command, cwd: o.cwd, timeoutSeconds: o.timeoutSeconds, stream: true }, { signal: o.signal });
}

/** A frame of {@link followCommand} / {@link runCommand}. */
export type RunFrame =
  | { type: "started"; processId: number; commandId?: string }
  | { type: "stdout" | "stderr"; data: string; nextOffset?: number }
  | { type: "exit"; status?: string; exitCode: number | null; signal?: string | null }
  | { type: "error"; error?: string; message?: string };

/** Follow a detached command until it ends, reconnecting by itself with no gap and no repeat. */
export async function* followCommand(
  cvm: CrowdVM, sandboxId: string, processId: number,
  o: { stdoutOffset?: number; stderrOffset?: number; maxReconnects?: number; signal?: AbortSignal } = {},
): AsyncGenerator<RunFrame, void, undefined> {
  let so = o.stdoutOffset ?? 0;
  let se = o.stderrOffset ?? 0;
  let failures = 0;
  for (;;) {
    let ended = false;
    try {
      for await (const f of cvm.attachCommand({ sandboxId, processId, stdoutOffset: so, stderrOffset: se }, { signal: o.signal })) {
        if (f.type === "stdout" || f.type === "stderr") {
          if (typeof f.nextOffset === "number") { if (f.type === "stdout") so = f.nextOffset; else se = f.nextOffset; }
          failures = 0;
          yield f as RunFrame;
        } else if (f.type === "attached") {
          so = f.stdoutOffset ?? so; se = f.stderrOffset ?? se;
        } else if (f.type === "reconnect") {
          so = f.stdoutOffset ?? so; se = f.stderrOffset ?? se;
          ended = true;
          break;
        } else if (f.type === "exit" || f.type === "error") {
          yield f as RunFrame;
          return;
        }
      }
      if (!ended) throw new APIConnectionError("stream closed before the command ended");
    } catch (e) {
      if (!(e instanceof APIConnectionError) || o.signal?.aborted) throw e;
      if (++failures > (o.maxReconnects ?? 20)) throw e;
      await sleep(Math.min(5000, 100 * 2 ** Math.min(failures - 1, 6)), o.signal);
    }
  }
}

/**
 * Run a command with no time limit: start it detached, then follow it. The first frame is
 * `{ type: "started", processId }`; the last is `exit` (with `exitCode`) or `error`.
 * Stopping the loop early leaves the command running (`cvm.stopCommand({ sandboxId, processId })`).
 */
export async function* runCommand(cvm: CrowdVM, sandboxId: string, command: string, o: { cwd?: string; signal?: AbortSignal } = {}): AsyncGenerator<RunFrame, void, undefined> {
  const started = (await cvm.command(
    { sandboxId, command, cwd: o.cwd, detached: true },
    { signal: o.signal, maxRetries: Math.max(cvm.maxRetries, 20) },
  )) as T.CommandStartedResponse;
  yield { type: "started", processId: started.processId, commandId: started.commandId };
  yield* followCommand(cvm, sandboxId, started.processId, { signal: o.signal });
}

// ------------------------------------------------------------------ events and prompts
class EventPoller {
  seen = new Set<string>();
  primed = false;
  constructor(private cvm: CrowdVM, private sandboxId: string, public type?: string | string[], public conversation?: string, private signal?: AbortSignal) {}

  private async fetch(): Promise<T.SandboxEvent[]> {
    const out: T.SandboxEvent[] = [];
    let cursor: string | undefined;
    for (let page = 0; page < 20; page++) {
      const res = await this.cvm.events({ sandboxId: this.sandboxId, limit: 200, sort: "desc", cursor, type: this.type, conversation: this.conversation }, { signal: this.signal });
      const fresh = (res?.events ?? []).filter((e) => !this.seen.has(e.id as string));
      out.push(...fresh);
      const info = res?.pageInfo;
      if (!info?.hasMore || !info.nextCursor || (this.primed && fresh.length === 0)) break;
      cursor = info.nextCursor;
    }
    return out.sort((a, b) => (a.timestamp ?? 0) - (b.timestamp ?? 0));
  }

  async seed(): Promise<void> {
    for (const e of await this.fetch()) if (e.id !== undefined) this.seen.add(e.id);
    this.primed = true;
  }

  async poll(): Promise<T.SandboxEvent[]> {
    const fresh = await this.fetch();
    for (const e of fresh) if (e.id !== undefined) this.seen.add(e.id);
    this.primed = true;
    return fresh;
  }
}

/**
 * Follow the events of a sandbox (polls `GET /events`; the endpoint is not a stream). Yields only new
 * events unless `replay: true`. Stop it with an `AbortSignal`.
 */
export async function* streamEvents(
  cvm: CrowdVM, sandboxId: string,
  o: { type?: string | string[]; conversation?: string; intervalMs?: number; replay?: boolean; signal?: AbortSignal } = {},
): AsyncGenerator<T.SandboxEvent, void, undefined> {
  const poller = new EventPoller(cvm, sandboxId, o.type, o.conversation, o.signal);
  try {
    if (!o.replay) await poller.seed();
    while (!o.signal?.aborted) {
      yield* await poller.poll();
      await sleep(o.intervalMs ?? 1000, o.signal);
    }
  } catch (e) {
    if (!o.signal?.aborted) throw e; // an abort ends the stream quietly
  }
}

/** Parameters of `cvm.prompt` without the sandbox id. */
export type PromptOptions = Omit<Parameters<CrowdVM["prompt"]>[0], "sandboxId"> & { intervalMs?: number; signal?: AbortSignal };

/** Queue a prompt and yield the events of its conversation until the prompt is done. */
export async function* streamPrompt(cvm: CrowdVM, sandboxId: string, params: PromptOptions): AsyncGenerator<T.SandboxEvent, void, undefined> {
  const { intervalMs = 1000, signal, ...body } = params;
  const poller = new EventPoller(cvm, sandboxId, undefined, body.conversationId ?? undefined, signal);
  await poller.seed();
  const queued = await cvm.prompt({ sandboxId, ...body }, { signal });
  if (queued.conversationId) poller.conversation = queued.conversationId;
  for (;;) {
    const done = (await cvm.promptRunStatus({ sandboxId, promptId: queued.promptId }, { signal })).promptRun.done;
    yield* await poller.poll();
    if (done) return;
    await sleep(intervalMs, signal);
  }
}

/** Stop the sandbox, wait until it is archived, then delete it and its snapshots. */
export async function stopAndRemove(cvm: CrowdVM, sandboxId: string, o: WaitOptions & { force?: boolean } = {}): Promise<T.DeletionOperationResponse> {
  try {
    await cvm.stop({ sandboxId, force: o.force });
  } catch (e) {
    if (!(e instanceof ApiError) || e.status !== 409) throw e;
  }
  await poll(async () => {
    const sb = (await cvm.get({ sandboxId }, { signal: o.signal })).sandbox;
    return ["archived", "error", "cancelled"].includes(sb.state) ? sb : undefined;
  }, `sandbox ${sandboxId} to stop`, o, 300_000);
  return cvm.deleteSandbox({ sandboxId });
}

// ------------------------------------------------------------------ pagination
/** Yield every item of a cursor-paginated list: `paginate(cvm.sandboxes.bind(cvm), "sandboxes", { state: "running" })`. */
export async function* paginate<P extends { cursor?: string | null }, K extends string, I = any>(
  fn: (params: P) => Promise<any>, key: K, params: Omit<P, "cursor"> = {} as any,
): AsyncGenerator<I, void, undefined> {
  let cursor: string | undefined;
  for (;;) {
    const res = await fn({ ...params, cursor } as P);
    yield* (res?.[key] ?? []) as I[];
    const next = res?.pageInfo?.nextCursor ?? res?.nextCursor;
    const more = res?.pageInfo?.hasMore ?? !!next;
    if (!more || !next) return;
    cursor = next;
  }
}
