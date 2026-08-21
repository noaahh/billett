import assert from "node:assert/strict";
import test from "node:test";

import worker from "./index.js";

const receipt = [
  "From: Swiss Federal Railways <noreply@example.com>",
  "To: journeys@example.com",
  "Subject: EasyRide Kaufquittung - Ihre Fahrt",
  "Message-ID: <receipt@example.com>",
  "",
  "Receipt body",
].join("\r\n");

test("posts a matching receipt as a raw authenticated email", async (context) => {
  const originalFetch = globalThis.fetch;
  context.after(() => { globalThis.fetch = originalFetch; });
  let request;
  globalThis.fetch = async (url, options) => {
    request = { url, options };
    return new Response('{"created":true}', { status: 201 });
  };

  const message = emailMessage(receipt, "EasyRide Kaufquittung - Ihre Fahrt");
  await worker.email(message, environment());

  assert.equal(request.url, "https://ingest.example.com/api/inbound-email");
  assert.equal(request.options.method, "POST");
  assert.equal(request.options.headers.Authorization, "Bearer inbound-secret");
  assert.equal(request.options.headers["Content-Type"], "message/rfc822");
  assert.equal(new TextDecoder().decode(request.options.body), receipt);
  assert.equal(message.rejection, undefined);
});

test("forwards non-receipt messages to the optional fallback", async () => {
  const message = emailMessage("Verification", "Gmail forwarding confirmation");
  await worker.email(message, environment({ FALLBACK_FORWARD_TO: "owner@example.com" }));

  assert.equal(message.forwardedTo, "owner@example.com");
  assert.equal(message.rejection, undefined);
});

test("accepts a normal SBB online-purchase subject", async (context) => {
  const originalFetch = globalThis.fetch;
  context.after(() => { globalThis.fetch = originalFetch; });
  let called = false;
  globalThis.fetch = async () => {
    called = true;
    return new Response('{"created":true}', { status: 201 });
  };

  const message = emailMessage(receipt, "Ihr Online-Kauf bei der SBB");
  await worker.email(message, environment());

  assert.equal(called, true);
  assert.equal(message.rejection, undefined);
});

test("rejects delivery to an unexpected recipient", async () => {
  const message = emailMessage(receipt, "EasyRide Kaufquittung", "other@example.com");
  await worker.email(message, environment());

  assert.equal(message.rejection, "This address is not configured for journey receipts");
});

test("rejects a receipt subject from an untrusted sender domain", async () => {
  const message = emailMessage(receipt, "EasyRide Kaufquittung", "journeys@example.com", "attacker@example.net");
  await worker.email(message, environment());

  assert.equal(message.rejection, "Receipt sender domain is not allowed");
});

function environment(overrides = {}) {
  return {
    BILLETT_INBOUND_URL: "https://ingest.example.com/api/inbound-email",
    BILLETT_INBOUND_TOKEN: "inbound-secret",
    ALLOWED_TO: "journeys@example.com",
    ALLOWED_FROM_DOMAINS: "example.com",
    FALLBACK_FORWARD_TO: "",
    ...overrides,
  };
}

function emailMessage(raw, subject, to = "journeys@example.com", from = "noreply@example.com") {
  return {
    to,
    from,
    raw: new Response(raw).body,
    rawSize: new TextEncoder().encode(raw).byteLength,
    headers: new Headers({ subject }),
    setReject(reason) { this.rejection = reason; },
    async forward(recipient) { this.forwardedTo = recipient; },
  };
}
