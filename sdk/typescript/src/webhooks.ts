/**
 * Verify the signature of a webhook delivery (WebCrypto; works in Node >= 18, browsers, edge runtimes).
 *
 * CrowdVM signs `HMAC_SHA256(secret, deliveryId + "." + timestamp + "." + rawBody)` and sends it as
 * `v1=<hex>`. Header names: `X-CrowdVM-Delivery` / `-Timestamp` / `-Signature` / `-Event` / `-Attempt`
 * (Boat's `X-Ascii-*` names are accepted too).
 *
 * ```ts
 * const event = await constructEvent(rawBody, req.headers, process.env.WEBHOOK_SECRET!);
 * ```
 */
import type { WebhookEvent } from "./types.js";
import { CrowdVMError } from "./errors.js";

export type HeadersLike = Headers | Record<string, string | string[] | undefined> | Iterable<[string, string]>;
export type BodyLike = string | Uint8Array | ArrayBuffer;

export interface VerifyOptions {
  /** Maximum age / skew of the timestamp in seconds (default 300). */
  toleranceSeconds?: number;
  /** "Now" in unix seconds (tests). */
  now?: number;
}

export class WebhookVerificationError extends CrowdVMError {}

const PREFIXES = ["x-crowdvm-", "x-ascii-"];

/** Read a header from a `Headers`, a Node `IncomingHttpHeaders`-style object, or an iterable of pairs. */
export function getHeader(headers: HeadersLike, name: string): string {
  const lower = name.toLowerCase();
  const h = headers as any;
  if (typeof h?.get === "function") return String(h.get(lower) ?? "");
  const entries: Iterable<[string, any]> = typeof h?.[Symbol.iterator] === "function" ? h : Object.entries(h ?? {});
  for (const [k, v] of entries) if (String(k).toLowerCase() === lower) return String(Array.isArray(v) ? v[0] : v ?? "");
  return "";
}

function webhookHeader(headers: HeadersLike, name: string): string {
  for (const p of PREFIXES) {
    const v = getHeader(headers, p + name);
    if (v) return v;
  }
  return "";
}

function toBytes(b: BodyLike): Uint8Array {
  if (typeof b === "string") return new TextEncoder().encode(b);
  return b instanceof Uint8Array ? b : new Uint8Array(b);
}

async function subtle(): Promise<SubtleCrypto> {
  const g = (globalThis as any).crypto?.subtle;
  if (g) return g;
  const mod = "node:crypto"; // Node 18 has no global `crypto`; the variable keeps bundlers away
  return (await import(mod)).webcrypto.subtle;
}

function message(deliveryId: string, timestamp: string | number, rawBody: BodyLike): Uint8Array {
  const prefix = new TextEncoder().encode(`${deliveryId}.${timestamp}.`);
  const body = toBytes(rawBody);
  const out = new Uint8Array(prefix.length + body.length);
  out.set(prefix); out.set(body, prefix.length);
  return out;
}

async function hmacKey(secret: string, usage: "sign" | "verify"): Promise<CryptoKey> {
  return (await subtle()).importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, [usage]);
}

const hex = (b: ArrayBuffer) => [...new Uint8Array(b)].map((x) => x.toString(16).padStart(2, "0")).join("");

/** The `X-CrowdVM-Signature` value for a delivery (`v1=<hex>`). Handy for tests. */
export async function signWebhook(secret: string, deliveryId: string, timestamp: string | number, rawBody: BodyLike): Promise<string> {
  const sig = await (await subtle()).sign("HMAC", await hmacKey(secret, "sign"), message(deliveryId, timestamp, rawBody) as BufferSource);
  return "v1=" + hex(sig);
}

/**
 * True when the signature matches `secret` (a string, or several while a secret rotates) and the
 * timestamp is within the tolerance. Pass the *raw* request body, before any JSON parsing.
 */
export async function verifyWebhook(rawBody: BodyLike, headers: HeadersLike, secret: string | string[], o: VerifyOptions = {}): Promise<boolean> {
  const delivery = webhookHeader(headers, "delivery");
  const timestamp = webhookHeader(headers, "timestamp");
  const supplied = webhookHeader(headers, "signature").replace(/^v1=/, "");
  const issuedAt = Number(timestamp);
  const now = o.now ?? Date.now() / 1000;
  if (!timestamp || !Number.isFinite(issuedAt) || Math.abs(now - issuedAt) > (o.toleranceSeconds ?? 300)) return false;
  if (!/^[a-f0-9]{64}$/i.test(supplied)) return false;
  const sig = Uint8Array.from(supplied.match(/../g)!.map((h) => parseInt(h, 16)));
  const msg = message(delivery, timestamp, rawBody);
  const s = await subtle();
  let ok = false;
  for (const secretValue of Array.isArray(secret) ? secret : [secret]) {
    // `subtle.verify` compares in constant time
    if (await s.verify("HMAC", await hmacKey(secretValue, "verify"), sig as BufferSource, msg as BufferSource)) ok = true;
  }
  return ok;
}

/** Verify, then parse the delivery body. Dedupe on `event.id` (delivery is at-least-once). */
export async function constructEvent(rawBody: BodyLike, headers: HeadersLike, secret: string | string[], o: VerifyOptions = {}): Promise<WebhookEvent> {
  if (!(await verifyWebhook(rawBody, headers, secret, o))) throw new WebhookVerificationError("invalid webhook signature or timestamp");
  return JSON.parse(typeof rawBody === "string" ? rawBody : new TextDecoder().decode(toBytes(rawBody)));
}
