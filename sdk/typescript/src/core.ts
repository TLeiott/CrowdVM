export interface OperationSpec {
  readonly method: string;
  readonly path: string;
  readonly ret: "json" | "bytes" | "ndjson";
  readonly body: "none" | "optional" | "required";
  readonly pathParams: readonly string[];
  readonly query: readonly string[];
  readonly headers: Readonly<Record<string, string>>;
  readonly confirmDefault?: string;
  /** Boat's name for the wrapped request body, e.g. `forkRequest` (`fork({ sandboxId, forkRequest: { type } })`). */
  readonly bodyParam?: string;
}

/** Per-call options accepted as the last argument of every method. */
export interface RequestOptions {
  /** Abort the request (or stop reading a stream). */
  signal?: AbortSignal;
  /** Overrides the client's request timeout (not applied to streams once they started). */
  timeoutMs?: number;
  /** Overrides the client's retry budget for 429 / 503 / restoring answers. */
  maxRetries?: number;
}
