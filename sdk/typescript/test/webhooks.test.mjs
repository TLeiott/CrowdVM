import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { test } from "node:test";
import { WebhookVerificationError, constructEvent, signWebhook, verifyWebhook } from "../dist/index.js";
import { signWebhookSync, verifyWebhookSync } from "../dist/node.js";

const SECRET = "whsec_test";
const ID = "evt_" + "a".repeat(32);
const NOW = 1_800_000_000;
const BODY = JSON.stringify({ id: ID, type: "sandbox.ready", createdAt: "2026-01-01T00:00:00Z", data: { sandbox: { id: "bx_x", name: "n" }, previousState: "cloning", state: "ready" } });

async function headers(secret = SECRET, ts = NOW, prefix = "X-CrowdVM-", body = BODY) {
  return { [prefix + "Delivery"]: ID, [prefix + "Timestamp"]: String(ts), [prefix + "Signature"]: await signWebhook(secret, ID, ts, body), [prefix + "Event"]: "sandbox.ready", [prefix + "Attempt"]: "1" };
}

test("matches the documented algorithm", async () => {
  const expected = createHmac("sha256", SECRET).update(`${ID}.${NOW}.`).update(BODY).digest("hex");
  assert.equal((await headers())["X-CrowdVM-Signature"], "v1=" + expected);
  assert.equal(signWebhookSync(SECRET, ID, NOW, BODY), "v1=" + expected);
});

test("verifies (WebCrypto and Node sync), accepts Headers / Boat names / bytes", async () => {
  const h = await headers();
  assert.equal(await verifyWebhook(BODY, h, SECRET, { now: NOW }), true);
  assert.equal(verifyWebhookSync(BODY, h, SECRET, { now: NOW }), true);
  assert.equal(await verifyWebhook(new TextEncoder().encode(BODY), new Headers(h), SECRET, { now: NOW }), true);
  assert.equal(await verifyWebhook(BODY, await headers(SECRET, NOW, "x-ascii-"), SECRET, { now: NOW }), true);
  assert.equal(verifyWebhookSync(BODY, await headers(SECRET, NOW, "X-Ascii-"), SECRET, { now: NOW }), true);
  assert.equal(await verifyWebhook(BODY, { "x-crowdvm-delivery": [ID], "x-crowdvm-timestamp": String(NOW), "x-crowdvm-signature": h["X-CrowdVM-Signature"] }, SECRET, { now: NOW }), true);
  const ev = await constructEvent(BODY, h, SECRET, { now: NOW });
  assert.deepEqual([ev.type, ev.data.sandbox.id, ev.data.state], ["sandbox.ready", "bx_x", "ready"]);
});

test("rejects bad signatures, tampered bodies, replays, malformed headers", async () => {
  for (const verify of [(b, h, s, o) => verifyWebhook(b, h, s, o), async (b, h, s, o) => verifyWebhookSync(b, h, s, o)]) {
    assert.equal(await verify(BODY, await headers("whsec_other"), SECRET, { now: NOW }), false);
    assert.equal(await verify(BODY + " ", await headers(), SECRET, { now: NOW }), false);
    assert.equal(await verify(BODY, await headers(SECRET, NOW - 301), SECRET, { now: NOW }), false);
    assert.equal(await verify(BODY, await headers(SECRET, NOW - 299), SECRET, { now: NOW }), true);
    assert.equal(await verify(BODY, { ...(await headers()), "X-CrowdVM-Signature": "v1=nothex" }, SECRET, { now: NOW }), false);
    assert.equal(await verify(BODY, {}, SECRET, { now: NOW }), false);
    assert.equal(await verify(BODY, { ...(await headers()), "X-CrowdVM-Timestamp": "abc" }, SECRET, { now: NOW }), false);
  }
  await assert.rejects(async () => constructEvent(BODY, await headers("x"), SECRET, { now: NOW }), WebhookVerificationError);
});

test("secret rotation: accept the old and the new secret", async () => {
  assert.equal(await verifyWebhook(BODY, await headers("old"), ["new", "old"], { now: NOW }), true);
  assert.equal(verifyWebhookSync(BODY, await headers("old"), ["new", "old"], { now: NOW }), true);
  assert.equal(await verifyWebhook(BODY, await headers("other"), ["new", "old"], { now: NOW }), false);
});
