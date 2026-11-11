// fetch wrapper for the {ok,type,...} envelope. Cookie session auth; every
// mutating request carries the CSRF header the server requires.

export class ApiError extends Error {
  constructor(status, code, message, details, requestId) {
    super(message || code || `HTTP ${status}`);
    Object.assign(this, { status, code: code || `http_${status}`, details, requestId });
  }
}

let onUnauthorized = () => {};
export const setUnauthorizedHandler = (fn) => { onUnauthorized = fn; };

// The organization the dashboard is viewing (an org id or 'personal'). Sent on
// every dashboard request as X-CrowdVM-Org, so the dashboard never relies on the
// account's active wallet, which belongs to the CLI and API keys.
let orgScope = null;
export const setOrgScope = (org) => { orgScope = org || null; };

function headers(method, body, extra) {
  const h = { Accept: 'application/json', ...(orgScope ? { 'X-CrowdVM-Org': orgScope } : null), ...extra };
  if (method !== 'GET' && method !== 'HEAD') h['X-CrowdVM-CSRF'] = '1';
  if (body !== undefined && !(body instanceof Blob) && !(body instanceof FormData)) h['Content-Type'] = 'application/json';
  return h;
}

async function toError(res) {
  let j = null;
  try { j = await res.json(); } catch { /* not JSON */ }
  const e = j?.error && typeof j.error === 'object' ? j.error : {};
  return new ApiError(res.status, j?.code || e.code || (typeof j?.error === 'string' ? j.error : null),
    j?.message || e.message || res.statusText, e.details || j?.details, j?.requestId);
}

// request('GET', '/api/v1/sandboxes', undefined, {query:{limit:50}})
export async function request(method, path, body, opts = {}) {
  const url = new URL(path, location.origin);
  for (const [k, v] of Object.entries(opts.query || {})) if (v != null && v !== '') url.searchParams.set(k, v);
  let res;
  try {
    res = await fetch(url, {
      method, credentials: 'same-origin', signal: opts.signal,
      headers: headers(method, body, opts.headers),
      body: body === undefined ? undefined : body instanceof Blob || body instanceof FormData ? body : JSON.stringify(body),
    });
  } catch (err) {
    if (err.name === 'AbortError') throw err;
    throw new ApiError(0, 'network_error', 'Cannot reach the CrowdVM server.');
  }
  if (res.status === 401 && !opts.allow401) { onUnauthorized(); throw await toError(res); }
  if (!res.ok) throw await toError(res);
  if (res.status === 204) return { ok: true };
  const ct = res.headers.get('content-type') || '';
  if (!ct.includes('json')) return opts.raw ? res : { ok: true, text: await res.text() };
  const j = await res.json();
  if (j && j.ok === false) throw new ApiError(j.status || res.status, j.code || j.error?.code, j.message || j.error?.message, j.error?.details, j.requestId);
  return j;
}

const v1 = (p) => (p.startsWith('/') ? `/api/v1${p}` : `/api/v1/${p}`);
export const api = {
  get: (p, query, o = {}) => request('GET', v1(p), undefined, { ...o, query }),
  post: (p, body = {}, o) => request('POST', v1(p), body, o),
  put: (p, body = {}, o) => request('PUT', v1(p), body, o),
  patch: (p, body = {}, o) => request('PATCH', v1(p), body, o),
  del: (p, query, o = {}) => request('DELETE', v1(p), undefined, { ...o, query }),
};
export const auth = {
  me: () => request('GET', '/auth/me', undefined, { allow401: true }),
  login: (b) => request('POST', '/auth/login', b, { allow401: true }),
  signup: (b) => request('POST', '/auth/signup', b, { allow401: true }),
  logout: () => request('POST', '/auth/logout', {}),
  approveDevice: (userCode) => request('POST', '/auth/device/approve', { userCode }),
};

// POST and read an NDJSON stream; onFrame(obj) per line. Resolves when the stream ends.
export async function stream(path, body, onFrame, signal) {
  const res = await fetch(v1(path), {
    method: 'POST', credentials: 'same-origin', signal,
    headers: { ...headers('POST', body), Accept: 'application/x-ndjson' },
    body: JSON.stringify(body),
  });
  if (res.status === 401) onUnauthorized();
  if (!res.ok) throw await toError(res);
  const ct = res.headers.get('content-type') || '';
  if (!ct.includes('ndjson') || !res.body) { onFrame({ type: '_result', ...(await res.json()) }); return; }
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += value;
    let i;
    while ((i = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, i).trim(); buf = buf.slice(i + 1);
      if (line) { try { onFrame(JSON.parse(line)); } catch { /* skip bad line */ } }
    }
  }
  if (buf.trim()) { try { onFrame(JSON.parse(buf)); } catch { /* ignore */ } }
}

// Human message for an error, with the code for anything unexpected.
export function errText(e) {
  if (!e) return 'Something went wrong.';
  if (e.name === 'AbortError') return 'Cancelled.';
  if (e instanceof ApiError) return e.message && e.message !== e.code ? e.message : `${e.code} (HTTP ${e.status})`;
  return e.message || String(e);
}
export const isMissing = (e) => e instanceof ApiError && (e.status === 404 || e.status === 501 || e.status === 405);
