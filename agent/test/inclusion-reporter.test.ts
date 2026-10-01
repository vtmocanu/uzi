// Issue #1800: InclusionReporter driven directly with a stub client, so the stop, the tick backoff
// and the streak are pinned deterministically (no wall-clock settle windows).
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { RequestError, type WorkerClient } from "../src/client.js";
import { INCLUSION_ROUTE_MISSING_BACKOFF_MAX_TICKS, INCLUSION_ROUTE_MISSING_LIMIT, InclusionReporter } from "../src/inclusion-reporter.js";
import { nullLogger } from "./helpers.js";

const PATH = "/api/worker/runs/r/inputs/included";
const untyped404 = (): RequestError => new RequestError("POST", PATH, 404, '{"error":"not found"}');
const typedStale404 = (): RequestError => new RequestError("POST", PATH, 404, '{"error":"run not found","reason":"stale"}');
const unavailable503 = (): RequestError => new RequestError("POST", PATH, 503, '{"error":"down"}');

type Outcome = RequestError | { active: boolean };
interface Call {
  ids: number[];
  flush: number;
}

/** A stub whose k-th request (0-based) gets `script(k)`; records each request with the flush index. */
function setup(script: (call: number) => Outcome) {
  const calls: Call[] = [];
  const state = { flush: 0 };
  const client = {
    async includeInputs(_runId: string, ids: number[], _generation: number) {
      const outcome = script(calls.length);
      calls.push({ ids: [...ids], flush: state.flush });
      if (outcome instanceof RequestError) throw outcome;
      return outcome;
    },
  } as unknown as WorkerClient;
  const reporter = new InclusionReporter(client, "r", 1, nullLogger());
  /** Run `n` flushes, numbering them from the current index. */
  const flushes = async (n: number): Promise<void> => {
    for (let i = 0; i < n; i++) {
      await reporter.flush();
      state.flush++;
    }
  };
  return { reporter, calls, flushes };
}

/** The flush indices a permanent 404 produces, from the backoff rule alone: send, then skip. */
function expectedSendFlushes(count: number): number[] {
  const out: number[] = [];
  let at = 0;
  for (let streak = 1; streak <= count; streak++) {
    out.push(at);
    at += 1 + Math.min(2 ** (streak - 1), INCLUSION_ROUTE_MISSING_BACKOFF_MAX_TICKS);
  }
  return out;
}

describe("InclusionReporter untyped 404/405", () => {
  it("stops after exactly INCLUSION_ROUTE_MISSING_LIMIT requests and sends nothing for a later id", async () => {
    const { reporter, calls, flushes } = setup(() => untyped404());
    reporter.mark(1);
    await flushes(200);
    assert.strictEqual(calls.length, INCLUSION_ROUTE_MISSING_LIMIT);
    assert.strictEqual(reporter.pending, false, "the queue is cleared on stop");
    reporter.mark(2);
    assert.strictEqual(reporter.pending, false, "a later id is not queued");
    await flushes(200);
    assert.strictEqual(calls.length, INCLUSION_ROUTE_MISSING_LIMIT);
  });

  it("backs off in poll ticks: requests land on flushes 0, 2, 5, 10, 19, 36, ...", async () => {
    const { reporter, calls, flushes } = setup(() => untyped404());
    reporter.mark(1);
    await flushes(200);
    const at = calls.map((c) => c.flush);
    assert.deepStrictEqual(at.slice(0, 5), [0, 2, 5, 10, 19], "skips 1, 2, 4, 8 between sends");
    assert.deepStrictEqual(at, expectedSendFlushes(INCLUSION_ROUTE_MISSING_LIMIT), "capped after the 5th");
    assert.strictEqual(at[5]! - at[4]!, 1 + INCLUSION_ROUTE_MISSING_BACKOFF_MAX_TICKS);
  });

  it("a client that 404s LIMIT-1 times then accepts gets the id sent and settled, with no stop", async () => {
    const { reporter, calls, flushes } = setup((k) => (k < INCLUSION_ROUTE_MISSING_LIMIT - 1 ? untyped404() : { active: true }));
    reporter.mark(1);
    await flushes(200);
    assert.strictEqual(calls.length, INCLUSION_ROUTE_MISSING_LIMIT);
    assert.strictEqual(reporter.pending, false, "the id settled");
    reporter.mark(2);
    assert.strictEqual(reporter.pending, true, "not stopped: a later id is still accepted");
    await flushes(1);
    assert.deepStrictEqual(calls[calls.length - 1]!.ids, [2]);
  });

  it("a success resets the streak, so a later run of 404s gets the full allowance again", async () => {
    const answers: Outcome[] = [];
    for (let i = 0; i < INCLUSION_ROUTE_MISSING_LIMIT - 1; i++) answers.push(untyped404());
    answers.push({ active: true });
    const { reporter, calls, flushes } = setup((k) => answers[k] ?? untyped404());
    reporter.mark(1);
    await flushes(200);
    assert.strictEqual(reporter.pending, false);
    const before = calls.length;
    reporter.mark(2);
    await flushes(400);
    assert.strictEqual(calls.length - before, INCLUSION_ROUTE_MISSING_LIMIT, "a fresh full streak before stopping");
  });

  it("a 404 once then accepted: the id is sent again and settled", async () => {
    for (const status of [404, 405]) {
      const { reporter, calls, flushes } = setup((k) => (k === 0 ? new RequestError("POST", PATH, status, "") : { active: true }));
      reporter.mark(7);
      await flushes(5);
      assert.deepStrictEqual(calls.map((c) => c.ids), [[7], [7]]);
      assert.strictEqual(reporter.pending, false);
    }
  });
});

describe("InclusionReporter other outcomes", () => {
  it("stops after a receipt saying the claim is inactive; a later marked id is never sent", async () => {
    const { reporter, calls, flushes } = setup(() => ({ active: false }));
    reporter.mark(1);
    await flushes(1);
    reporter.mark(2);
    await flushes(50);
    assert.strictEqual(calls.length, 1);
    assert.strictEqual(reporter.pending, false);
  });

  it("stops after one typed stale 404", async () => {
    const { reporter, calls, flushes } = setup(() => typedStale404());
    reporter.mark(1);
    await flushes(50);
    reporter.mark(2);
    await flushes(50);
    assert.strictEqual(calls.length, 1);
    assert.strictEqual(reporter.pending, false);
  });

  it("a transient 503 keeps the id queued and retries on the next flush", async () => {
    const { reporter, calls, flushes } = setup((k) => (k === 0 ? unavailable503() : { active: true }));
    reporter.mark(1);
    await flushes(1);
    assert.strictEqual(reporter.pending, true);
    await flushes(1);
    assert.strictEqual(calls.length, 2);
    assert.strictEqual(reporter.pending, false);
  });
});

describe("InclusionReporter.drain", () => {
  it("makes at most 3 attempts against a 503", async () => {
    const { reporter, calls } = setup(() => unavailable503());
    reporter.mark(1);
    await reporter.drain();
    assert.strictEqual(calls.length, 3);
    assert.strictEqual(reporter.pending, true, "left for the next claim's requeue");
  });

  it("ignores the tick backoff: right after a 404 it still sends immediately", async () => {
    const { reporter, calls, flushes } = setup((k) => (k === 0 ? untyped404() : { active: true }));
    reporter.mark(1);
    await flushes(1); // 404: the next flush would be skipped
    await reporter.drain();
    assert.strictEqual(calls.length, 2);
    assert.strictEqual(reporter.pending, false);
  });
});
