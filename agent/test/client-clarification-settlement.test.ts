// Issue #3985: the `clarification_settled` state report. The discriminator is sent whole or not at
// all: without the negotiated feature, a positive claim generation and the settlement binding the
// client throws before any request, and it never strips the discriminator and re-sends the report
// as an unqualified `running`.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient, CLARIFICATION_REVISION_SETTLEMENT_FEATURE } from "../src/client.js";
import type { StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const RUN = "11111111-2222-4333-8444-555555555555";
const SETTLEMENT = { question_id: "q-1", revision_input_id: 4, gate_revision: 2 };

function client(features: string[]): WorkerClient {
  const c = new WorkerClient("http://127.0.0.1:1", "worker-join-token-0123456789", "0.1.0-test", nullLogger(), {
    sleep: async () => {}, terminalRetrySchedule: [1],
  });
  c.protocolFeatures = features;
  return c;
}

function settle(extra: Partial<StateRequest> = {}): StateRequest {
  return { status: "clarification_settled", claim_generation: 3, clarification_settlement: SETTLEMENT, ...extra };
}

describe("client clarification_settled report", () => {
  it("throws before any request without the negotiated feature", async t => {
    const requests: string[] = [];
    t.mock.method(globalThis, "fetch", async (url: string) => { requests.push(url); return Response.json({ run: { status: "running" } }); });
    await assert.rejects(client([]).reportState(RUN, settle()), /clarification settlement requires/);
    assert.deepEqual(requests, [], "no request, and no unqualified second request");
  });

  for (const [name, body] of [
    ["a missing claim generation", settle({ claim_generation: undefined })],
    ["a zero claim generation", settle({ claim_generation: 0 })],
    ["a non-integer claim generation", settle({ claim_generation: 1.5 })],
    ["a missing settlement", settle({ clarification_settlement: undefined })],
  ] as const) {
    it(`throws before any request for ${name}`, async t => {
      const requests: string[] = [];
      t.mock.method(globalThis, "fetch", async (url: string) => { requests.push(url); return Response.json({ run: { status: "running" } }); });
      await assert.rejects(client([CLARIFICATION_REVISION_SETTLEMENT_FEATURE]).reportState(RUN, body), /clarification settlement requires/);
      assert.deepEqual(requests, []);
    });
  }

  it("sends the report once, unmodified and without a registration nonce, and parses the settled disposition", async t => {
    const bodies: Record<string, unknown>[] = [];
    t.mock.method(globalThis, "fetch", async (_url: string, init?: RequestInit) => {
      bodies.push(JSON.parse(String(init?.body)));
      return Response.json({ run: { status: "awaiting_approval" }, disposition: "clarification_settled" });
    });
    const ack = await client([CLARIFICATION_REVISION_SETTLEMENT_FEATURE]).reportState(RUN, settle());
    assert.equal(bodies.length, 1);
    assert.deepEqual(bodies[0], settle());
    assert.equal(ack.applied, true);
    assert.equal(ack.status, "awaiting_approval");
    assert.equal(ack.clarificationSettled, true);
  });

  it("a 200 without the disposition is not a settlement", async t => {
    t.mock.method(globalThis, "fetch", async () => Response.json({ run: { status: "awaiting_approval" } }));
    const ack = await client([CLARIFICATION_REVISION_SETTLEMENT_FEATURE]).reportState(RUN, settle());
    assert.equal(ack.clarificationSettled, undefined);
  });

  it("a 409 refusal surfaces as an unapplied ack carrying the run's real status and never the disposition", async t => {
    const bodies: unknown[] = [];
    t.mock.method(globalThis, "fetch", async (_url: string, init?: RequestInit) => {
      bodies.push(JSON.parse(String(init?.body)));
      return Response.json({ run: { status: "running" }, disposition: "clarification_settled" }, { status: 409 });
    });
    const ack = await client([CLARIFICATION_REVISION_SETTLEMENT_FEATURE]).reportState(RUN, settle());
    assert.equal(bodies.length, 1, "a refusal is not retried as another status");
    assert.equal(ack.applied, false);
    assert.equal(ack.status, "running");
    assert.equal(ack.clarificationSettled, undefined);
  });

  it("a strict-decode 400 propagates and is never re-sent without the discriminator", async t => {
    const bodies: Record<string, unknown>[] = [];
    t.mock.method(globalThis, "fetch", async (_url: string, init?: RequestInit) => {
      bodies.push(JSON.parse(String(init?.body)));
      return new Response(JSON.stringify({ error: "invalid request body" }), { status: 400 });
    });
    await assert.rejects(client([CLARIFICATION_REVISION_SETTLEMENT_FEATURE]).reportState(RUN, settle()));
    assert.equal(bodies.length, 1);
    assert.equal(bodies.every(b => b.status === "clarification_settled" && b.clarification_settlement !== undefined), true);
  });
});
