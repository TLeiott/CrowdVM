/**
 * Node-only entry (`@crowdvm/sdk/node`): synchronous webhook verification with `node:crypto`.
 *
 * ```ts
 * import { verifyWebhookSync } from "@crowdvm/sdk/node";
 * if (!verifyWebhookSync(rawBody, req.headers, secret)) return res.status(401).end();
 * ```
 */
import { createHmac, timingSafeEqual } from "node:crypto";
import { getHeader } from "./webhooks.js";
import type { BodyLike, HeadersLike, VerifyOptions } from "./webhooks.js";

export type { BodyLike, HeadersLike, VerifyOptions };

function header(headers: HeadersLike, name: string): string {
  return getHeader(headers, "x-crowdvm-" + name) || getHeader(headers, "x-ascii-" + name);
}

const bytes = (b: BodyLike): Uint8Array =>
  typeof b === "string" ? new TextEncoder().encode(b) : b instanceof Uint8Array ? b : new Uint8Array(b);

/** The `X-CrowdVM-Signature` value for a delivery (`v1=<hex>`). */
export function signWebhookSync(secret: string, deliveryId: string, timestamp: string | number, rawBody: BodyLike): string {
  return "v1=" + createHmac("sha256", secret).update(`${deliveryId}.${timestamp}.`).update(bytes(rawBody)).digest("hex");
}

/** Synchronous variant of `verifyWebhook` (constant-time compare via `timingSafeEqual`). */
export function verifyWebhookSync(rawBody: BodyLike, headers: HeadersLike, secret: string | string[], o: VerifyOptions = {}): boolean {
  const delivery = header(headers, "delivery");
  const timestamp = header(headers, "timestamp");
  const supplied = header(headers, "signature").replace(/^v1=/, "");
  const issuedAt = Number(timestamp);
  const now = o.now ?? Date.now() / 1000;
  if (!timestamp || !Number.isFinite(issuedAt) || Math.abs(now - issuedAt) > (o.toleranceSeconds ?? 300)) return false;
  if (!/^[a-f0-9]{64}$/i.test(supplied)) return false;
  const got = Uint8Array.from(supplied.match(/../g)!.map((h) => parseInt(h, 16)));
  let ok = false;
  for (const s of Array.isArray(secret) ? secret : [secret]) {
    const expected = Uint8Array.from(signWebhookSync(s, delivery, timestamp, rawBody).slice(3).match(/../g)!.map((h) => parseInt(h, 16)));
    if (timingSafeEqual(got, expected)) ok = true;
  }
  return ok;
}
