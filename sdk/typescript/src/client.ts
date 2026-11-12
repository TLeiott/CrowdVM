import type { OperationSpec, RequestOptions } from "./core.js";
import { APIConnectionError, ApiError, apiErrorFrom } from "./errors.js";
import { GeneratedOperations } from "./operations.js";
import type * as T from "./types.js";

export const VERSION = "0.1.0";
export const DEFAULT_BASE_URL = "http://127.0.0.1:8420/api/v1";

/** Boat SDK style configuration: `new CrowdVM(new Configuration({ basePath, accessToken }))`. */
export class Configuration {
  readonly basePath?: string;
  readonly accessToken?: string;
  constructor(init: { basePath?: string; accessToken?: string; apiKey?: string } = {}) {
    this.basePath = init.basePath;
    this.accessToken = init.accessToken ?? init.apiKey;
  }
}

export interface ClientOptions {
  /** API key (`cvm_...`). Default: `CROWDVM_API_KEY`. */
  apiKey?: string;
  /** e.g. `http://192.168.1.10:8420/api/v1`. Default: `CROWDVM_API_BASE`, else `http://127.0.0.1:8420/api/v1`. */
  baseUrl?: string;
  /** Custom `fetch` (tests, proxies). Default: the global one. */
  fetch?: typeof fetch;
  /** Retries for 429 / 503 / restoring answers (default 3). */
  maxRetries?: number;
  /** Upper bound of one retry wait, in ms (default 60000). */
  maxRetryDelayMs?: number;
  /** Request timeout in ms (default 60000; 0 disables). */
  timeoutMs?: number;
  /** Idle timeout of `attachCommand` streams in ms (default 90000; the server sends a heartbeat every 15 s). */
  streamIdleTimeoutMs?: number;
  /** Extra headers on every request. */
  headers?: Record<string, string>;
}

/** Parameters of {@link CrowdVM.command}: the path parameter plus the `CommandRequest` body. */
export type CommandParams = { sandboxId: string } & T.CommandRequest;

type SendInit = {
  query?: Record<string, unknown>;
  body?: unknown;
  headers?: Record<string, string>;
  accept?: string;
} & RequestOptions;

type Sent = { res: Response; cleanup: () => void };

const NO_RETRY_429 = new Set(["limit_reached", "member_limit_reached", "trial_compute_limit_reached"]);
const isNode = typeof (globalThis as any).process?.versions?.node === "string";

function env(name: string): string | undefined {
  return (globalThis as any).process?.env?.[name];
}

export function normalizeBase(url: string): string {
  url = url.trim().replace(/\/+$/, "");
  try {
    const u = new URL(url);
    if (u.pathname === "" || u.pathname === "/") url += "/api/v1";
  } catch { /* leave as is */ }
  return url;
}

export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const t = setTimeout(() => { signal?.removeEventListener("abort", onAbort); resolve(); }, ms);
    const onAbort = () => { clearTimeout(t); reject(signal!.reason); };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

/** Copy `extra` into `into`; a name replaces an existing header of another case (header names are case-insensitive). */
function mergeHeaders(into: Record<string, string>, extra?: Record<string, string>): void {
  for (const [k, v] of Object.entries(extra ?? {})) {
    for (const old of Object.keys(into)) if (old.toLowerCase() === k.toLowerCase()) delete into[old];
    into[k] = v;
  }
}

/** One percent-encoded path segment; `.` / `..` / empty would change the endpoint, so they are refused. */
function pathSegment(name: string, value: unknown): string {
  const s = String(value);
  if (s === "" || s === "." || s === "..") throw new TypeError(`invalid path parameter "${name}": ${JSON.stringify(s)} is not allowed`);
  return encodeURIComponent(s);
}

function encodeQuery(q: Record<string, unknown>): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(q)) {
    if (v === undefined || v === null) continue;
    if (Array.isArray(v) && k === "label") { for (const item of v) sp.append(k, String(item)); }
    else sp.set(k, Array.isArray(v) ? v.join(",") : String(v));
  }
  return sp.toString();
}

/**
 * Client for the CrowdVM HTTP API (a Boat v1 compatible surface).
 *
 * ```ts
 * const cvm = new CrowdVM();   // CROWDVM_API_KEY / CROWDVM_API_BASE
 * const { sandbox } = await cvm.create({ ttlSeconds: 1800 });
 * ```
 *
 * Every API operation is a method named like Boat's SDK (`create`, `get`, `commandStatus`, ...)
 * taking one flat object: path + query parameters and request-body fields side by side.
 */
export class CrowdVM extends GeneratedOperations {
  readonly apiKey: string;
  readonly baseUrl: string;
  readonly maxRetries: number;
  readonly maxRetryDelayMs: number;
  readonly timeoutMs: number;
  readonly streamIdleTimeoutMs: number;
  readonly headers: Record<string, string>;
  private readonly fetchFn: typeof fetch;

  constructor(options: ClientOptions | Configuration = {}) {
    super();
    if (options instanceof Configuration) options = { apiKey: options.accessToken, baseUrl: options.basePath };
    this.apiKey = options.apiKey ?? env("CROWDVM_API_KEY") ?? "";
    this.baseUrl = normalizeBase(options.baseUrl ?? env("CROWDVM_API_BASE") ?? DEFAULT_BASE_URL);
    this.maxRetries = options.maxRetries ?? 3;
    this.maxRetryDelayMs = options.maxRetryDelayMs ?? 60_000;
    this.timeoutMs = options.timeoutMs ?? 60_000;
    this.streamIdleTimeoutMs = options.streamIdleTimeoutMs ?? 90_000;
    this.headers = { ...options.headers };
    this.fetchFn = options.fetch ?? ((...a) => globalThis.fetch(...a));
  }

  /** A copy of this client with some options changed. */
  withOptions(options: ClientOptions): CrowdVM {
    return new CrowdVM({
      apiKey: this.apiKey, baseUrl: this.baseUrl, maxRetries: this.maxRetries, maxRetryDelayMs: this.maxRetryDelayMs,
      timeoutMs: this.timeoutMs, streamIdleTimeoutMs: this.streamIdleTimeoutMs, headers: this.headers, fetch: this.fetchFn, ...options,
    });
  }

  // ---------------------------------------------------------------- transport
  private retryDelayMs(err: ApiError, attempt: number): number | undefined {
    const ra = err.retryAfterSeconds;
    const backoff = Math.min(this.maxRetryDelayMs, 2 ** attempt * 1000) * (0.5 + Math.random() / 2);
    const wait = (dflt: number) => (ra !== undefined ? ra * 1000 : dflt);
    const msg = err.details?.message ?? err.message;
    if (err.status === 429) {
      if (NO_RETRY_429.has(err.code)) return undefined;
      if (err.code === "rate_limited" && ["hour", "day"].includes(err.details?.window)) return undefined;
      return wait(backoff);
    }
    if (err.status === 503) return err.code === "no_ready_machine" ? undefined : wait(backoff); // failFast: no wait
    if (err.status === 502 && err.code === "sandbox_direct_failed" && msg === "sandbox_restoring") return wait(2000);
    if (err.status === 409 && err.code === "sandbox_starting") return wait(3000);
    if (err.status === 409 && err.code === "idempotency_in_progress") return wait(1000);
    return undefined;
  }

  /** One request with retries. Returns the open response; `cleanup()` releases the timers/listeners. */
  private async send(method: string, path: string, init: SendInit, stream: boolean): Promise<Sent | null> {
    let url = this.baseUrl + path;
    const qs = init.query ? encodeQuery(init.query) : "";
    if (qs) url += "?" + qs;
    const headers: Record<string, string> = { Accept: init.accept ?? "application/json" };
    if (isNode) headers["User-Agent"] = `crowdvm-js/${VERSION}`;
    if (this.apiKey) headers["Authorization"] = `Bearer ${this.apiKey}`;
    mergeHeaders(headers, this.headers);
    let body: string | undefined;
    if (init.body !== undefined) {
      // Sets (Boat's samples pass `events: new Set([...])`) go out as arrays.
      body = JSON.stringify(init.body, (_k, v) => (v instanceof Set ? [...v] : v));
      headers["Content-Type"] = "application/json";
    }
    mergeHeaders(headers, init.headers);
    const maxRetries = init.maxRetries ?? this.maxRetries;
    const timeoutMs = init.timeoutMs ?? this.timeoutMs;

    for (let attempt = 0; ; attempt++) {
      const user = init.signal;
      if (user?.aborted) throw user.reason;
      const ctl = new AbortController();
      const onAbort = () => ctl.abort(user!.reason);
      user?.addEventListener("abort", onAbort, { once: true });
      let timedOut = false;
      const expire = () => { timedOut = true; ctl.abort(); };
      let timer = timeoutMs > 0 ? setTimeout(expire, timeoutMs) : undefined;
      const cleanup = () => { if (timer) clearTimeout(timer); user?.removeEventListener("abort", onAbort); };
      let delay: number | undefined;
      try {
        let res = await this.fetchFn(url, { method, headers, body, signal: ctl.signal });
        if (timer) clearTimeout(timer); // the timeout covers the headers ...
        if (timer && !stream && res.body) {
          // ... and then each silence of the body, not the whole body: a
          // download that keeps arriving is not cut (bugs-r16-sonnet 11).
          timer = setTimeout(expire, timeoutMs);
          const rearm = new TransformStream<Uint8Array, Uint8Array>({
            transform(chunk, c) { clearTimeout(timer); timer = setTimeout(expire, timeoutMs); c.enqueue(chunk); },
            flush() { clearTimeout(timer); },
          });
          res = new Response(res.body.pipeThrough(rearm), { status: res.status, statusText: res.statusText, headers: res.headers });
        }
        if (res.status === 304) { cleanup(); return null; }
        if (!res.ok) throw await apiErrorFrom(res);
        return { res, cleanup };
      } catch (e) {
        cleanup();
        if (e instanceof ApiError) {
          delay = this.retryDelayMs(e, attempt);
          if (delay === undefined || attempt >= maxRetries) throw e;
        } else if (user?.aborted) {
          throw e;
        } else {
          const err = new APIConnectionError(
            timedOut ? `${method} ${url}: timed out after ${timeoutMs} ms` : `${method} ${url}: ${(e as Error)?.message ?? e}`,
            { cause: e });
          if (method !== "GET" || attempt >= maxRetries) throw err;
          delay = Math.min(this.maxRetryDelayMs, 2 ** attempt * 1000) * (0.5 + Math.random() / 2);
        }
      }
      await sleep(Math.min(delay, this.maxRetryDelayMs), init.signal);
    }
  }

  /** Raw escape hatch: `await cvm.request("GET", "/me")` returns the decoded JSON. */
  async request<R = any>(method: string, path: string, init: SendInit = {}): Promise<R> {
    const sent = await this.send(method.toUpperCase(), path, init, false);
    return this.readJson(sent) as R;
  }

  private async readJson(sent: Sent | null): Promise<unknown> {
    if (!sent) return null;
    try {
      const text = await sent.res.text();
      return text.trim() ? JSON.parse(text) : {};
    } catch (e) {
      if (e instanceof SyntaxError) throw new APIConnectionError("invalid JSON in response", { cause: e });
      throw new APIConnectionError(`reading the response failed: ${(e as Error)?.message ?? e}`, { cause: e });
    } finally {
      sent.cleanup();
    }
  }

  private async *lines(sent: Sent, signal?: AbortSignal, idleMs = 0, requireExit = false): AsyncGenerator<any, void, undefined> {
    const reader = sent.res.body!.getReader();
    const dec = new TextDecoder();
    let buf = "";
    const parse = (line: string) => { try { return JSON.parse(line); } catch { return undefined; } };
    let terminal = false;
    const note = (v: any) => { if (v?.type === "exit" || v?.type === "error") terminal = true; return v; };
    try {
      for (;;) {
        let chunk: ReadableStreamReadResult<Uint8Array>;
        let idle: ReturnType<typeof setTimeout> | undefined;
        try {
          chunk = await (idleMs > 0
            ? Promise.race([reader.read(), new Promise<never>((_, reject) => { idle = setTimeout(() => reject(new Error(`no data for ${idleMs} ms`)), idleMs); })])
            : reader.read());
        } catch (e) {
          if (signal?.aborted) throw e;
          throw new APIConnectionError(`stream interrupted: ${(e as Error)?.message ?? e}`, { cause: e });
        } finally {
          if (idle) clearTimeout(idle);
        }
        if (chunk.done) break;
        buf += dec.decode(chunk.value, { stream: true });
        let i: number;
        while ((i = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, i).trim();
          buf = buf.slice(i + 1);
          const v = line ? parse(line) : undefined;
          if (v !== undefined) yield note(v);
        }
      }
      buf += dec.decode();
      const v = buf.trim() ? parse(buf.trim()) : undefined;
      if (v !== undefined) yield note(v);
      if (requireExit && !terminal) throw new APIConnectionError("stream ended before a terminal frame");
    } finally {
      reader.cancel().catch(() => {});
      sent.cleanup();
    }
  }

  // ---------------------------------------------------------------- generated ops
  private split(spec: OperationSpec, params: Record<string, any>) {
    let path = spec.path;
    const used = new Set<string>();
    for (const name of spec.pathParams) {
      if (params[name] === undefined) throw new TypeError(`missing path parameter "${name}"`);
      path = path.replace(`{${name}}`, pathSegment(name, params[name]));
      used.add(name);
    }
    const query: Record<string, unknown> = {};
    for (const name of spec.query) {
      used.add(name);
      if (params[name] !== undefined && params[name] !== null) query[name] = params[name];
    }
    const headers: Record<string, string> = {};
    for (const [name, wire] of Object.entries(spec.headers)) {
      used.add(name);
      const v = params[name];
      if (v != null) headers[wire] = String(v);
    }
    if (spec.confirmDefault && !Object.keys(headers).some((h) => h.toLowerCase().endsWith("confirm-delete"))) {
      headers["X-Ascii-Confirm-Delete"] = headers["X-CrowdVM-Confirm-Delete"] = String(params[spec.confirmDefault]);
    }
    let body: Record<string, unknown> | undefined;
    if (spec.bodyParam) used.add(spec.bodyParam);
    // Boat's TS client spells reserved words with a leading underscore (`_public: true`)
    const rest = Object.fromEntries(Object.entries(params).filter(([k, v]) => !used.has(k) && v !== undefined)
      .map(([k, v]) => [k.startsWith("_") && !(k.slice(1) in params) ? k.slice(1) : k, v]));
    if (spec.bodyParam && params[spec.bodyParam]) Object.assign(rest, params[spec.bodyParam], { ...rest });
    if (spec.body === "none") {
      const extra = Object.keys(rest);
      if (extra.length) throw new TypeError(`unexpected parameter(s): ${extra.join(", ")}`);
    } else {
      body = rest;
    }
    return { path, query, headers, body };
  }

  protected async _op(spec: OperationSpec, params: object, options: RequestOptions = {}): Promise<unknown> {
    const { path, query, headers, body } = this.split(spec, params);
    const sent = await this.send(spec.method, path, { query, headers, body, ...options }, false);
    if (spec.ret === "bytes") {
      if (!sent) return null;
      try { return await sent.res.blob(); } catch (e) {
        throw new APIConnectionError(`reading the response failed: ${(e as Error)?.message ?? e}`, { cause: e });
      } finally { sent.cleanup(); }
    }
    return this.readJson(sent);
  }

  protected async *_ndjson(spec: OperationSpec, params: object, options: RequestOptions = {}): AsyncGenerator<any, void, undefined> {
    const { path, query, headers, body } = this.split(spec, params);
    const sent = await this.send(spec.method, path, { query, headers, body, accept: "application/x-ndjson", ...options }, true);
    if (sent) yield* this.lines(sent, options.signal, spec.path.endsWith("/stream") ? this.streamIdleTimeoutMs : 0);
  }

  // ---------------------------------------------------------------- commands
  /**
   * Run a command (`POST /sandboxes/{id}/commands`).
   * - default: waits and returns a `CommandResponse`
   * - `detached: true`: returns a `CommandStartedResponse` at once
   * - `stream: true`: returns an async iterator of NDJSON frames (see also `streamCommand`)
   */
  command(params: CommandParams & { stream: true; detached?: false | undefined }, options?: RequestOptions): AsyncGenerator<T.CommandStreamFrame, void, undefined>;
  command(params: CommandParams & { detached: true }, options?: RequestOptions): Promise<T.CommandStartedResponse>;
  command(params: CommandParams, options?: RequestOptions): Promise<T.CommandResponse | T.CommandStartedResponse>;
  command(params: CommandParams, options: RequestOptions = {}): any {
    const { sandboxId, ...body } = params;
    let path: string;
    try { path = `/sandboxes/${pathSegment("sandboxId", sandboxId)}/commands`; } catch (e) {
      if (body.stream && !body.detached) return (async function* () { throw e; })();
      return Promise.reject(e);
    }
    if (body.stream && !body.detached) {
      const self = this;
      return (async function* () {
        const sent = await self.send("POST", path, { body, accept: "application/x-ndjson", ...options }, true);
        if (sent) yield* self.lines(sent, options.signal, 0, true);
      })();
    }
    // the HTTP read must outlive the command (<= 600 s) plus the 60 s start wait
    const timeoutMs = options.timeoutMs ?? Math.max(this.timeoutMs, ((body.timeoutSeconds ?? 30) + 90) * 1000);
    return this.send("POST", path, { body, ...options, timeoutMs }, false).then((s) => this.readJson(s));
  }
}

/** Boat-SDK style aliases: `new BoatApi(new Configuration({ basePath, accessToken }))` works unchanged. */
export { CrowdVM as CrowdVMApi, CrowdVM as BoatApi };
