import assert from "node:assert/strict";
import { it, type TestContext } from "node:test";
import { Worker } from "../src/worker.js";
import type { Outbox, PendingFinalize } from "../src/outbox.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { recordingLogger } from "./worker-sweep-rig.js";

const A = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const B = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";
const entry = (run_id = A, claim_generation = 1): PendingFinalize => ({ run_id, claim_generation });
type Candidate = { entry: PendingFinalize; identity: Readonly<object> };
type Sweep = { sweepPendingFinalizes(signal?: AbortSignal, frozen?: readonly Candidate[],
  lower?: ReadonlySet<Readonly<object>>): Promise<void> };
const flush = () => new Promise<void>(resolve => setImmediate(resolve));
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(yes => { resolve = yes; });
  return { promise, resolve };
}

function rig(t: TestContext, entries = [entry()]) {
  let now = 1_000_000;
  t.mock.method(Date, "now", () => now);
  t.mock.method(Math, "random", () => 1);
  const records = new Map<string, Candidate>();
  const key = (e: PendingFinalize) => `${e.run_id}:${e.claim_generation}`;
  const add = (e: PendingFinalize) => records.set(key(e), { entry: e, identity: {} });
  entries.forEach(add);
  const hidden = new Set<string>();
  const queries: string[] = [];
  const deleted: PendingFinalize[] = [];
  const logger = recordingLogger();
  let predicate: (id: string, gen: number) => Promise<boolean> = async () => true;
  const outbox = {
    listPendingFinalizeGenerations: (includeTerminal = false) => [...records.values()]
      .filter(c => includeTerminal || !hidden.has(c.entry.run_id)).map(c => c.entry),
    finalizeRecordIdentity: (id: string, gen: number) => records.get(key(entry(id, gen)))?.identity,
    retireFinalizeIfEligible: async (id: string, gen: number, eligible: () => boolean,
      signal: AbortSignal, identity: Readonly<object>) => {
      const e = entry(id, gen);
      if (signal.aborted || !eligible() || hidden.has(id) || records.get(key(e))?.identity !== identity) return false;
      records.delete(key(e));
      deleted.push(e);
      return true;
    },
  } as unknown as Outbox;
  const client = {
    capturePublicationCompletionRetirement: (): Readonly<object> | undefined => { queries.push("capture"); return undefined; },
    getRunOwnership: async () => { queries.push("ownership"); return { status: "completed", claim_generation: 999 }; },
  };
  const newWorker = () => new Worker({} as Config, client as unknown as WorkerClient, {
    isExecuting: () => { queries.push("live"); return false; },
    recoveryFinalAcknowledged: async () => { queries.push("ack"); return false; },
    retireRecoveryReport: async () => { queries.push("report"); return { retired: false }; },
    recoveryInventoryPending: async (id: string, gen: number) => {
      queries.push(`${id}:${gen}`); return predicate(id, gen);
    },
  } as unknown as RunRunner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
  logger.log, () => ({ ok: true, missing: [] }), outbox) as unknown as Sweep;
  const w = newWorker();
  return { w, newWorker, client, records, hidden, queries, deleted, add, logger,
    setPredicate: (fn: typeof predicate) => { predicate = fn; },
    advance: (ms: number) => { now += ms; },
    now: () => now,
    failures: () => logger.info.filter(log => log.fields.transition === "failed"),
  };
}

for (const random of [0, 1]) {
  it(`retained outcomes back off through the cap with jitter endpoint ${random}`, async t => {
    const r = rig(t);
    t.mock.method(Math, "random", () => random);
    const delays = random === 0
      ? [12000, 24000, 48000, 96000, 192000, 384000, 480000, 480000]
      : [15000, 30000, 60000, 120000, 240000, 480000, 600000, 600000];
    for (const [index, delay] of delays.entries()) {
      await r.w.sweepPendingFinalizes();
      const log = r.failures().at(-1)!.fields;
      assert.equal(log.attempt_count, index + 1);
      assert.equal(log.next_eligible_time, r.now() + delay);
      const queries = r.queries.length;
      const logs = r.logger.info.length;
      r.advance(delay - 1);
      await r.w.sweepPendingFinalizes();
      assert.equal(r.queries.length, queries, "cooling invokes no authority or live predicates");
      assert.equal(r.logger.info.length, logs, "skipping emits no heartbeat log");
      r.advance(1);
    }
    assert.equal(r.records.size, 1);
    assert.equal(r.deleted.length, 0);
    assert.equal(new Set(r.failures().map(log => log.fields.record_identity)).size, 1);
    r.setPredicate(async () => false);
    await r.w.sweepPendingFinalizes();
    assert.deepEqual(r.deleted, [entry()]);
    assert.equal(r.logger.info.at(-1)!.fields.transition, "retired");
  });
}

it("synchronous completion capture exceptions retain and cool before retirement retries", async t => {
  const r = rig(t);
  let captureFails = true;
  let captures = 0;
  t.mock.method(r.client, "capturePublicationCompletionRetirement", () => {
    captures++;
    if (captureFails) throw new Error("completion capture unavailable");
    return undefined;
  });
  r.setPredicate(async () => false);
  await assert.doesNotReject(r.w.sweepPendingFinalizes());
  assert.equal(captures, 1);
  assert.equal(r.records.size, 1);
  assert.deepEqual(r.deleted, []);
  assert.equal(r.failures().length, 1);
  const queries = [...r.queries];
  captureFails = false;
  await r.w.sweepPendingFinalizes();
  assert.equal(captures, 1, "changed capture conditions do not bypass cooldown");
  assert.deepEqual(r.queries, queries, "cooling invokes no authority or live predicates");
  assert.equal(r.failures().length, 1);
  r.advance(15000);
  await r.w.sweepPendingFinalizes();
  assert.equal(captures, 2);
  assert.deepEqual(r.deleted, [entry()]);
  assert.equal(r.records.size, 0);
  assert.equal(r.failures().length, 1);
});

it("exceptions count once and retirement followed by replacement starts fresh", async t => {
  const r = rig(t);
  r.setPredicate(async () => { throw new Error("custody unavailable"); });
  await r.w.sweepPendingFinalizes();
  assert.equal(r.failures().length, 1);
  await r.w.sweepPendingFinalizes();
  assert.equal(r.failures().length, 1);
  r.advance(15000);
  r.setPredicate(async () => false);
  await r.w.sweepPendingFinalizes();
  assert.equal(r.deleted.length, 1);
  r.add(entry());
  r.setPredicate(async () => true);
  await r.w.sweepPendingFinalizes();
  assert.deepEqual(r.failures().map(log => log.fields.attempt_count), [1, 1]);
  assert.notEqual(r.failures()[0]!.fields.record_identity, r.failures()[1]!.fields.record_identity);
});

it("replacement, disappearance and ABA reset only the changed exact identity", async t => {
  const r = rig(t, [entry(), entry(A, 2), entry(B)]);
  await r.w.sweepPendingFinalizes();
  const original = r.records.get(`${A}:1`)!;
  r.add(entry());
  await r.w.sweepPendingFinalizes();
  assert.equal(r.failures().length, 4, "only the replacement retries immediately");
  r.records.delete(`${A}:1`);
  await r.w.sweepPendingFinalizes();
  r.records.set(`${A}:1`, original);
  await r.w.sweepPendingFinalizes();
  assert.equal(r.failures().length, 5, "a removed then reappearing identity starts fresh");
  assert.equal(r.failures().at(-1)!.fields.attempt_count, 1);
  assert.equal(new Set(r.failures().map(log => log.fields.record_identity)).size, 5);
});

it("hidden records and frozen subsets preserve cooldown and fairness", async t => {
  const entries = Array.from({ length: 20 }, (_, n) => entry(A, n + 1));
  const r = rig(t, entries);
  await r.w.sweepPendingFinalizes();
  assert.equal(r.failures().length, 16);
  r.hidden.add(A);
  await r.w.sweepPendingFinalizes();
  await r.w.sweepPendingFinalizes(undefined, []);
  r.hidden.clear();
  const queries = r.queries.length;
  await r.w.sweepPendingFinalizes(undefined, [r.records.get(`${A}:1`)!]);
  assert.equal(r.queries.length, queries, "frozen scope does not reset cooldown");
  r.advance(15000);
  r.queries.length = 0;
  await r.w.sweepPendingFinalizes();
  const custody = r.queries.filter(q => q.startsWith(A));
  assert.deepEqual(custody.slice(0, 4), [17, 18, 19, 20].map(gen => `${A}:${gen}`),
    "unattempted generations retain priority after hidden and frozen passes");
});

it("eligible siblings beyond sixteen cooling records proceed; runs, generations and restart are independent", async t => {
  const r = rig(t, Array.from({ length: 16 }, (_, n) => entry(A, n + 1)));
  await r.w.sweepPendingFinalizes();
  r.add(entry(A, 17));
  r.add(entry(B));
  r.setPredicate(async () => false);
  await r.w.sweepPendingFinalizes();
  assert.deepEqual(r.deleted, [entry(A, 17), entry(B)]);
  assert.equal(r.records.size, 16);
  await r.newWorker().sweepPendingFinalizes();
  assert.equal(r.records.size, 0, "a new Worker starts without cooldown history");
});

it("deadline counts once, retains process quarantine and forbids late deletion", async t => {
  const r = rig(t);
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const held = deferred<boolean>();
  let calls = 0;
  r.setPredicate(async () => { calls++; return held.promise; });
  const pass = r.w.sweepPendingFinalizes();
  await flush();
  t.mock.timers.tick(1000);
  await pass;
  assert.equal(r.failures().length, 1);
  assert.equal(r.failures()[0]!.fields.next_eligible_time, r.now() + 15000);
  r.advance(15000);
  await r.w.sweepPendingFinalizes();
  await r.newWorker().sweepPendingFinalizes();
  assert.equal(calls, 1, "quarantine outlives cooldown and Worker instances");
  held.resolve(false);
  await flush();
  assert.equal(r.deleted.length, 0, "expired authority cannot authorize deletion");
  assert.equal(r.failures().length, 1, "late settlement cannot count again");
  r.setPredicate(async () => false);
  await r.w.sweepPendingFinalizes();
  assert.deepEqual(r.deleted, [entry()]);
});

it("late settlement cannot restore removed state or overwrite a new identity", async t => {
  const r = rig(t);
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const held = deferred<boolean>();
  r.setPredicate(async () => held.promise);
  const pass = r.w.sweepPendingFinalizes();
  await flush();
  t.mock.timers.tick(1000);
  await pass;
  r.records.clear();
  await r.w.sweepPendingFinalizes();
  r.add(entry());
  held.resolve(false);
  await flush();
  r.setPredicate(async () => true);
  await r.w.sweepPendingFinalizes();
  assert.deepEqual(r.failures().map(log => log.fields.attempt_count), [1, 1]);
  assert.notEqual(r.failures()[0]!.fields.record_identity, r.failures()[1]!.fields.record_identity);
  assert.equal(r.deleted.length, 0);
});

it("shutdown cancels the waiter without scheduling retry work", async t => {
  const r = rig(t);
  const held = deferred<boolean>();
  r.setPredicate(async () => held.promise);
  const controller = new AbortController();
  const pass = r.w.sweepPendingFinalizes(controller.signal);
  await flush();
  controller.abort();
  await pass;
  assert.equal(r.failures().length, 0);
  const count = r.queries.length;
  await r.w.sweepPendingFinalizes(controller.signal);
  assert.equal(r.queries.length, count);
  held.resolve(false);
  await flush();
  assert.equal(r.deleted.length, 0);
  assert.equal(r.failures().length, 0);
});
