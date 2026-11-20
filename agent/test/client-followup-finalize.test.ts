// Issue #4479: the `followup_finalize` state report and the park sequence its awaiting_followup ACK
// carries, against the shared wire fixture. The report is sent whole or not at all; the reply folds
// to admitted, refused(reason), stale_claim or unconfirmed, and anything the client cannot positively
// recognise is unconfirmed.
import { readFileSync } from "node:fs";
import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient, FOLLOWUP_FINALIZE_ADMISSION_FEATURE, RequestError, isFollowupFinalizeUnsupported } from "../src/client.js";
import type { StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const fixture = JSON.parse(readFileSync(new URL("../../fixtures/worker-protocol/followup-finalize.json", import.meta.url), "utf8")) as {
  request: StateRequest;
  park_ack: { http_status: number; followup_park_seq: number };
  success: { http_status: number; applied: boolean; disposition: string; run_status: string };
  refusals: Array<{ http_status: number; applied: boolean; reason: string }>;
  stale_claim: { http_status: number; disposition: string };
};

const RUN = "11111111-2222-4333-8444-555555555555";
const GENERIC_400 = "state must be one of running, awaiting_approval, awaiting_input, awaiting_followup, limit_wait, recovery_wait, paused, pause_failed, credential_switch, credential_switch_failed, credential_switch_held, credential_switch_hold_settled, credential_switch_uncertain, clarification_settled, followup_finalize, completed, failed";

function client(features: string[]): WorkerClient {
  const c = new WorkerClient("http://127.0.0.1:1", "worker-join-token-0123456789", "0.1.0-test", nullLogger(), {
    sleep: async () => {}, terminalRetrySchedule: [1],
  });
  c.protocolFeatures = features;
  return c;
}

const finalize = (extra: Partial<StateRequest> = {}): StateRequest => ({ ...fixture.request, ...extra });
const withFeature = () => client([FOLLOWUP_FINALIZE_ADMISSION_FEATURE]);
const runBody = (status: string) => ({ run: { id: RUN, status } });

describe("client followup_finalize report", () => {
  it("sends the fixture request once, unmodified, and folds the success to admitted", async t => {
    const bodies: unknown[] = [];
    t.mock.method(globalThis, "fetch", async (_url: string, init?: RequestInit) => {
      bodies.push(JSON.parse(String(init?.body)));
      return Response.json({ applied: fixture.success.applied, disposition: fixture.success.disposition, ...runBody(fixture.success.run_status) },
        { status: fixture.success.http_status });
    });
    const ack = await withFeature().reportState(RUN, finalize());
    assert.deepEqual(bodies, [fixture.request]);
    assert.deepEqual(ack.followupFinalize, { kind: "admitted" });
  });

  for (const refusal of fixture.refusals) {
    it(`folds the ${refusal.reason} refusal to refused(reason)`, async t => {
      t.mock.method(globalThis, "fetch", async () =>
        Response.json({ applied: refusal.applied, reason: refusal.reason, ...runBody("awaiting_followup") }, { status: refusal.http_status }));
      const ack = await withFeature().reportState(RUN, finalize());
      assert.deepEqual(ack.followupFinalize, { kind: "refused", reason: refusal.reason });
      assert.equal(ack.applied, false);
    });
  }

  it("folds the stale_claim disposition to stale_claim", async t => {
    t.mock.method(globalThis, "fetch", async () =>
      Response.json({ disposition: fixture.stale_claim.disposition, ...runBody("running") }, { status: fixture.stale_claim.http_status }));
    const ack = await withFeature().reportState(RUN, finalize());
    assert.deepEqual(ack.followupFinalize, { kind: "stale_claim" });
    assert.equal(ack.staleClaim, true);
  });

  const good = { applied: true, disposition: "followup_finalize_admitted", ...runBody("running") };
  for (const [name, status, body] of [
    ["a 200 without applied", 200, { disposition: good.disposition, ...runBody("running") }],
    ["a 200 with applied false", 200, { ...good, applied: false }],
    ["a 200 with a string applied", 200, { ...good, applied: "true" }],
    ["a 200 without the disposition", 200, { applied: true, ...runBody("running") }],
    ["a 200 with another disposition", 200, { ...good, disposition: "clarification_settled" }],
    ["a 200 whose run is not running", 200, { ...good, ...runBody("awaiting_followup") }],
    ["a 200 with an empty body", 200, {}],
    ["a 200 without its run ID", 200, { ...good, run: { status: "running" } }],
    ["a 200 with another run ID", 200, { ...good, run: { id: "aaaaaaaa-2222-4333-8444-555555555555", status: "running" } }],
    ["a 409 without applied", 409, { reason: "followup_pending", ...runBody("awaiting_followup") }],
    ["a 409 with applied true", 409, { applied: true, reason: "followup_pending", ...runBody("awaiting_followup") }],
    ["a 409 without a reason", 409, { applied: false, ...runBody("awaiting_followup") }],
    ["a 409 with an unknown reason", 409, { applied: false, reason: "something_else", ...runBody("awaiting_followup") }],
    ["a 409 with a non-string reason", 409, { applied: false, reason: 7, ...runBody("awaiting_followup") }],
    ["a 409 that carries the admitted disposition", 409, { ...good, applied: false }],
  ] as const) {
    it(`folds ${name} to unconfirmed`, async t => {
      t.mock.method(globalThis, "fetch", async () => Response.json(body, { status }));
      const ack = await withFeature().reportState(RUN, finalize());
      assert.deepEqual(ack.followupFinalize, { kind: "unconfirmed" });
    });
  }

  it("a 5xx is retried within the report and a persistent one propagates as an error, never as a result", async t => {
    let calls = 0;
    t.mock.method(globalThis, "fetch", async () => { calls++; return new Response("down", { status: 503 }); });
    await assert.rejects(withFeature().reportState(RUN, finalize()), RequestError);
    assert.ok(calls >= 1);
  });

  for (const [name, request] of [
    ["a missing generation", finalize({ claim_generation: undefined })],
    ["a zero generation", finalize({ claim_generation: 0 })],
    ["a missing park sequence", finalize({ followup_park_seq: undefined })],
    ["a zero park sequence", finalize({ followup_park_seq: 0 })],
    ["a fractional park sequence", finalize({ followup_park_seq: 1.5 })],
  ] as const) {
    it(`throws before any request for ${name}`, async t => {
      const requests: string[] = [];
      t.mock.method(globalThis, "fetch", async (url: string) => { requests.push(url); return Response.json({}); });
      await assert.rejects(withFeature().reportState(RUN, request), /follow-up finalize requires/);
      assert.deepEqual(requests, []);
    });
  }

  it("throws before any request without the negotiated feature", async t => {
    const requests: string[] = [];
    t.mock.method(globalThis, "fetch", async (url: string) => { requests.push(url); return Response.json({}); });
    await assert.rejects(client([]).reportState(RUN, finalize()), /follow-up finalize requires/);
    assert.deepEqual(requests, []);
  });

  const PRE_FINALIZE_400 = GENERIC_400.replace(" followup_finalize,", "");
  const PRE_UNCERTAIN_400 = PRE_FINALIZE_400.replace(" credential_switch_uncertain,", "");
  assert.notEqual(PRE_FINALIZE_400, GENERIC_400);
  assert.notEqual(PRE_UNCERTAIN_400, PRE_FINALIZE_400);
  for (const [name, text] of [
    ["the generic invalid-state error", GENERIC_400],
    ["the generic invalid-state error of an api before followup_finalize", PRE_FINALIZE_400],
    ["the generic invalid-state error of an api before credential_switch_uncertain", PRE_UNCERTAIN_400],
    ["the strict-decode error of an api that rejects the unknown field", "invalid request body"],
  ] as const) {
    it(`${name} propagates unchanged, is sent once and is recognised as an unsupported api`, async t => {
      const bodies: unknown[] = [];
      t.mock.method(globalThis, "fetch", async (_url: string, init?: RequestInit) => {
        bodies.push(JSON.parse(String(init?.body)));
        return new Response(JSON.stringify({ error: text }), { status: 400 });
      });
      const error = await withFeature().reportState(RUN, finalize()).then(() => undefined, (e: unknown) => e);
      assert.equal(bodies.length, 1, "never re-sent in another form");
      assert.ok(error instanceof RequestError && error.status === 400);
      assert.equal(isFollowupFinalizeUnsupported(error), true);
    });
  }

  for (const [name, text] of [
    ["the generic invalid-state error", PRE_FINALIZE_400],
    ["the strict-decode error", "invalid request body"],
  ] as const) {
    it(`a lost response followed by ${name} in the same call is an uncertain outcome, not an unsupported api`, async t => {
      let calls = 0;
      t.mock.method(globalThis, "fetch", async () => {
        calls++;
        // Attempt 1 may have reached the server and admitted the finalize; its response is lost.
        if (calls === 1) throw new TypeError("fetch failed");
        return new Response(JSON.stringify({ error: text }), { status: 400 });
      });
      const error = await withFeature().reportState(RUN, finalize()).then(() => undefined, (e: unknown) => e);
      assert.equal(calls, 2);
      assert.ok(error instanceof Error && /outcome uncertain/.test(error.message));
      assert.equal(isFollowupFinalizeUnsupported(error), false);
    });
  }

  it("another 400 is not an unsupported api", async t => {
    t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ error: "claim_generation_required" }), { status: 400 }));
    const error = await withFeature().reportState(RUN, finalize()).then(() => undefined, (e: unknown) => e);
    assert.equal(isFollowupFinalizeUnsupported(error), false);
  });
});

describe("client awaiting_followup park sequence", () => {
  const park = (): StateRequest => ({ status: "awaiting_followup", open_followup_id: 0, claim_generation: 3 });
  const ackFor = async (t: TestContext, body: unknown, status = 200) => {
    t.mock.method(globalThis, "fetch", async () => Response.json(body, { status }));
    return client([]).reportState(RUN, park());
  };

  it("reads the fixture's followup_park_seq off a parked ACK", async t => {
    const ack = await ackFor(t, { followup_park_seq: fixture.park_ack.followup_park_seq, ...runBody("awaiting_followup") }, fixture.park_ack.http_status);
    assert.equal(ack.followupParkSeq, fixture.park_ack.followup_park_seq);
  });

  for (const [name, value] of [
    ["zero", 0], ["negative", -2], ["a fraction", 1.5], ["a string", "4"], ["null", null], ["beyond the safe integers", 2 ** 60],
  ] as const) {
    it(`treats ${name} as absent`, async t => {
      const ack = await ackFor(t, { followup_park_seq: value, ...runBody("awaiting_followup") });
      assert.equal(ack.followupParkSeq, undefined);
    });
  }

  it("is absent when the ACK omits it (an older api)", async t => {
    assert.equal((await ackFor(t, runBody("awaiting_followup"))).followupParkSeq, undefined);
  });

  it("is absent when the run is no longer parked or the park was refused", async t => {
    assert.equal((await ackFor(t, { followup_park_seq: 3, ...runBody("running") })).followupParkSeq, undefined);
    assert.equal((await ackFor(t, { followup_park_seq: 3, ...runBody("awaiting_followup") }, 409)).followupParkSeq, undefined);
  });
});
