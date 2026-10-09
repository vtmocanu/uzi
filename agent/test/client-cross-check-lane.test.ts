import assert from "node:assert/strict";
import { it } from "node:test";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";
import type { ActiveSnapshot } from "../src/protocol.js";

const snapshot: ActiveSnapshot = {
  snapshot_epoch: 7,
  pending_overflow: false,
  active: [{ run_id: "lead", claim_generation: 3, phase: "awaiting_approval", terminal_pending: false }],
};
const client = () => new WorkerClient("http://example.com", "worker-token", "test", nullLogger());

for (const slots of [0, 1, 16]) {
  it(`registration sends explicit max_cross_check_slots=${slots}`, async (t) => {
    let body: unknown;
    t.mock.method(globalThis, "fetch", async (_url: string | URL | Request, init?: RequestInit) => {
      body = JSON.parse(String(init?.body));
      return Response.json({ worker_id: "w" });
    });
    await client().register("worker", undefined, 2, undefined, ["cross_check_v1"], undefined, slots);
    assert.deepEqual(body, { name: "worker", version: "test", max_concurrent_runs: 2,
      max_cross_check_slots: slots, protocol_capabilities: ["cross_check_v1"] });
  });
}

it("dedicated claim uses the run snapshot with the registration nonce and fenced decoder", async (t) => {
  const calls: Array<{ url: string; body: unknown }> = [];
  t.mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(url), body: JSON.parse(String(init?.body)) });
    if (String(url).endsWith("/register")) {
      return Response.json({ worker_id: "w", register_nonce: "nonce", protocol_features: ["active_run_snapshot", "recovery_inventory_v1"] });
    }
    return Response.json({ run_id: "check", kind: "cross_check", claim_generation: 4, inventory_guarded: true },
      { headers: { "X-Uzi-Claim-Kind": "cross_check" } });
  });
  const c = client();
  await c.register("worker");
  assert.equal((await c.claimCrossCheck(snapshot))?.claim_generation, 4);
  assert.deepEqual(calls[1], { url: "http://example.com/api/worker/runs/claim?lane=cross_check",
    body: { active_snapshot: { ...snapshot, register_nonce: "nonce" } } });
});

it("dedicated idle claim omits unnegotiated snapshots and returns null on 204", async (t) => {
  t.mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
    assert.equal(String(url), "http://example.com/api/worker/runs/claim?lane=cross_check");
    assert.deepEqual(JSON.parse(String(init?.body)), {});
    return new Response(null, { status: 204 });
  });
  assert.equal(await client().claimCrossCheck(snapshot), null);
});

it("dedicated claim retains marker consistency and inventory guard validation", async (t) => {
  let response = Response.json({ run_id: "check", kind: "task" },
    { headers: { "X-Uzi-Claim-Kind": "cross_check" } });
  t.mock.method(globalThis, "fetch", async () => response);
  await assert.rejects(client().claimCrossCheck(), /marker and kind disagree/);
  response = Response.json({ run_id: "check", kind: "cross_check", inventory_guarded: true, claim_generation: 0 },
    { headers: { "X-Uzi-Claim-Kind": "cross_check" } });
  await assert.rejects(client().claimCrossCheck(), /inventory guard claim/);
});

it("dedicated cross-check claims retain the bounded response decoder", async (t) => {
  let cancelled = false;
  t.mock.method(globalThis, "fetch", async () => new Response(new ReadableStream<Uint8Array>({
    start(controller) { controller.enqueue(Buffer.alloc(2 * 1024 * 1024 + 1, " ")); },
    cancel() { cancelled = true; },
  }), { headers: { "X-Uzi-Claim-Kind": "cross_check" } }));
  await assert.rejects(client().claimCrossCheck(), /response body exceeds 2097152 bytes/);
  assert.equal(cancelled, true);
});

it("unmarked dedicated valid oversized response closes the stream and throws the 2MiB limit", async (t) => {
  let cancelled = false;
  let sent = false;
  const body = Buffer.from(JSON.stringify({ run_id: "check", kind: "cross_check", padding: "x".repeat(8 * 1024 * 1024) }));
  t.mock.method(globalThis, "fetch", async () => new Response(new ReadableStream<Uint8Array>({
    pull(controller) {
      if (sent) controller.close();
      else {
        sent = true;
        controller.enqueue(body);
      }
    },
    cancel() { cancelled = true; },
  }, { highWaterMark: 0 })));
  await assert.rejects(client().claimCrossCheck(), /response body exceeds 2097152 bytes/);
  assert.equal(cancelled, true);
});

it("aborted dedicated claims never start transport", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => { assert.fail("unexpected transport"); });
  await assert.rejects(client().claimCrossCheck(snapshot, AbortSignal.abort()), { name: "AbortError" });
  assert.equal(fetch.mock.callCount(), 0);
});
