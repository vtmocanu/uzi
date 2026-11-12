import assert from "node:assert/strict";
import { test } from "node:test";
import { RequestError, SpendLimitParkUnsupportedError, WorkerClient } from "../src/client.js";
import type { StateRequest } from "../src/protocol.js";
import type { Logger } from "../src/log.js";

const FEATURE = "recovery_cause_anthropic_spend_limit";
const runA = "11111111-1111-4111-8111-111111111111";
const runB = "33333333-3333-4333-8333-333333333333";
const log: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
  child() { return this; },
};
const park = (): StateRequest => ({
  status: "recovery_wait", recovery_cause: "anthropic_spend_limit", claim_generation: 3,
});
function json(value: unknown, status: number) {
  return new Response(JSON.stringify(value), { status });
}
function client() {
  const c = new WorkerClient("http://example.invalid", "fixture", "test", log,
    { terminalRetrySchedule: [0], sleep: async () => {} });
  c.protocolFeatures = [FEATURE];
  return c;
}
const GENERIC_INVALID_STATE =
  "state must be one of running, awaiting_approval, awaiting_input, awaiting_followup, limit_wait, recovery_wait, paused, pause_failed, credential_switch, credential_switch_failed, credential_switch_held, credential_switch_hold_settled, clarification_settled, completed, failed";

test("transport failure then a definite refusal reports an uncertain outcome, not the refusal", async (t) => {
  const api = client();
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => {
    calls++;
    if (calls === 1) throw new TypeError("fetch failed");
    return json({ error: "unauthorized" }, 401);
  });
  await assert.rejects(api.reportState(runA, park()), (err: unknown) => {
    assert.ok(!(err instanceof RequestError), "a plain Error, never a definite refusal");
    assert.equal((err as Error).message, "spend-limit park outcome uncertain");
    return true;
  });
  assert.equal(calls, 2);
});

test("a 404 keeps the feature and a following park for another run is still sent", async (t) => {
  const api = client();
  const sentRuns: string[] = [];
  t.mock.method(globalThis, "fetch", async (url: string) => {
    sentRuns.push(url);
    return url.includes(runA) ? json({ error: "run not found" }, 404) : json({ run: { id: runB, status: "recovery_wait" }, applied: true }, 200);
  });
  await assert.rejects(api.reportState(runA, park()), (err: unknown) => err instanceof RequestError && err.status === 404);
  assert.equal(api.hasFeature(FEATURE), true);
  try {
    await api.reportState(runB, park());
  } catch (err) {
    assert.ok(!(err instanceof SpendLimitParkUnsupportedError), "must not be refused locally");
  }
  assert.equal(sentRuns.length, 2);
  assert.ok(sentRuns[1]!.includes(runB));
});

test("a 400 with a distinct body keeps the feature", async (t) => {
  const api = client();
  t.mock.method(globalThis, "fetch", async () =>
    json({ error: "invalid run state: a job run never parks (recovery_wait)" }, 400));
  await assert.rejects(api.reportState(runA, park()), (err: unknown) => err instanceof RequestError && err.status === 400);
  assert.equal(api.hasFeature(FEATURE), true);
});

test("compat: the generic invalid-state 400 drops the feature", async (t) => {
  const api = client();
  t.mock.method(globalThis, "fetch", async () => json({ error: GENERIC_INVALID_STATE }, 400));
  await assert.rejects(api.reportState(runA, park()), (err: unknown) => err instanceof RequestError);
  assert.equal(api.hasFeature(FEATURE), false);
});

test("compat: a strict-decode invalid request body 400 drops the feature", async (t) => {
  const api = client();
  t.mock.method(globalThis, "fetch", async () => json({ error: "invalid request body" }, 400));
  await assert.rejects(api.reportState(runA, park()), (err: unknown) => err instanceof RequestError);
  assert.equal(api.hasFeature(FEATURE), false);
});
