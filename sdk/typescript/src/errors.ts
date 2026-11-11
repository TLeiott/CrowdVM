/** Errors thrown by the SDK. */

export class CrowdVMError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options as ErrorOptions);
    this.name = new.target.name;
  }
}

/** The server could not be reached, or the connection broke mid-response. */
export class APIConnectionError extends CrowdVMError {}
/** A `wait*` helper ran out of time. */
export class WaitTimeout extends CrowdVMError {}
/** The sandbox entered `error` / `cancelled` while we waited. */
export class SandboxFailed extends CrowdVMError {
  constructor(public readonly sandbox: { id?: string; state?: string; error?: string | null }) {
    super(`sandbox ${sandbox.id} is ${sandbox.state}: ${sandbox.error ?? ""}`);
  }
}
/** A prompt run ended as `failed` or `interrupted`. */
export class PromptFailed extends CrowdVMError {
  constructor(public readonly run: { promptId?: string; status?: string }) {
    super(`prompt ${run.promptId} ${run.status}`);
  }
}

/** A non-2xx answer. `status`, `code`, `message`, `details`, `requestId` come from the error envelope. */
export class ApiError extends CrowdVMError {
  readonly status: number;
  readonly code: string;
  readonly details: Record<string, any>;
  readonly requestId?: string;
  /** The parsed JSON body (or `undefined` when it was not JSON). */
  readonly payload: any;
  /** The raw text of the body. */
  readonly body: string;
  readonly headers: Headers;
  /** The `Response`, with its body still readable once. Boat-SDK compatibility: `error.response.status`,
   * `await error.response.json()`. */
  readonly response: Response;

  constructor(res: Response, body: string) {
    let payload: any;
    try { payload = JSON.parse(body); } catch { payload = undefined; }
    const err = payload && typeof payload.error === "object" && payload.error ? payload.error : {};
    const code = String(payload?.code ?? err.code ?? `http_${res.status}`);
    const message = String(payload?.message ?? err.message ?? (body.slice(0, 300) || `HTTP ${res.status}`));
    super(`${res.status} ${code}: ${message}`);
    this.status = res.status;
    this.code = code;
    this.message = message;
    this.details = err.details ?? payload?.details ?? {};
    this.requestId = payload?.requestId;
    this.payload = payload;
    this.body = body;
    this.headers = res.headers;
    this.response = new Response(body, { status: res.status, statusText: res.statusText, headers: res.headers });
  }

  /** The wait the server asked for, in seconds (`retryAfterSeconds`, `retryAfter` or `Retry-After`). */
  get retryAfterSeconds(): number | undefined {
    for (const src of [this.details, this.payload]) {
      for (const key of ["retryAfterSeconds", "retryAfter"]) {
        const v = src?.[key];
        if (typeof v === "number" && Number.isFinite(v)) return v;
      }
    }
    const h = Number(this.headers.get("retry-after"));
    return this.headers.has("retry-after") && Number.isFinite(h) ? h : undefined;
  }

  /** `error.details.retryable` as sent by `502 sandbox_direct_failed`. */
  get retryable(): boolean {
    return this.details?.retryable === true;
  }
}

/** Boat-SDK compatibility alias. */
export { ApiError as ResponseError };

export class BadRequestError extends ApiError {}
export class AuthenticationError extends ApiError {}
export class PaymentRequiredError extends ApiError {}
export class PermissionDeniedError extends ApiError {}
export class NotFoundError extends ApiError {}
export class ConflictError extends ApiError {}
export class RateLimitError extends ApiError {}
export class ServerError extends ApiError {}
export class ServiceUnavailableError extends ServerError {}

const BY_STATUS: Record<number, typeof ApiError> = {
  400: BadRequestError, 401: AuthenticationError, 402: PaymentRequiredError, 403: PermissionDeniedError,
  404: NotFoundError, 409: ConflictError, 429: RateLimitError, 503: ServiceUnavailableError,
};

export async function apiErrorFrom(res: Response): Promise<ApiError> {
  const body = await res.text().catch(() => "");
  const Cls = BY_STATUS[res.status] ?? (res.status >= 500 ? ServerError : ApiError);
  return new Cls(res, body);
}
