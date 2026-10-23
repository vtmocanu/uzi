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
import { reportProofTiming } from "../src/report-proof-timing.js";

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
  const warnings: Array<{ msg: string; fields: Record<string, unknown> }> = [];
  logger.log.warn = (msg, fields) => { warnings.push({ msg, fields: fields ?? {} }); };
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
  const runner = {
    isExecuting: () => { queries.push("live"); return false; },
    recoveryFinalAcknowledged: async () => { queries.push("ack"); return false; },
    retireRecoveryReport: async () => { queries.push("report"); return { retired: false }; },
    recoveryInventoryPending: async (id: string, gen: number) => {
      queries.push(`${id}:${gen}`); return predicate(id, gen);
    },
  };
  const newWorker = () => new Worker({} as Config, client as unknown as WorkerClient,
    runner as unknown as RunRunner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
    logger.log, () => ({ ok: true, missing: [] }), outbox) as unknown as Sweep;
  const w = newWorker();
  return { w, newWorker, client, runner, outbox, records, hidden, queries, deleted, add, logger, warnings,
    setPredicate: (fn: typeof predicate) => { predicate = fn; },
    advance: (ms: number) => { now += ms; },
    now: () => now,
    failures: () => logger.info.filter(log => log.fields.transition === "failed"),
  };
}

// Copy this file unchanged and select this name on ee190c042: only existing Worker/outbox
// interfaces and call counts are used, so baseline fails on a premature retry.
it("baseline-compatible primary: timed-out false authority respects consecutive cooldowns through saturation", async t => {
  const r = rig(t);
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let calls = 0;
  let held = deferred<boolean>();
  r.setPredicate(async () => { calls++; return held.promise; });
  try {
    for (const random of [0, 1]) {
      t.mock.method(Math, "random", () => random);
      const w = r.newWorker();
      const delays = random === 0
        ? [12000, 24000, 48000, 96000, 192000, 384000, 480000, 480000]
        : [15000, 30000, 60000, 120000, 240000, 480000, 600000, 600000];
      for (const delay of delays) {
        held = deferred<boolean>();
        const before = calls;
        const pass = w.sweepPendingFinalizes();
        await flush();
        assert.equal(calls, before + 1, "exact expiry starts one bounded attempt");
        r.advance(1000);
        t.mock.timers.tick(1000);
        await pass;
        held.resolve(false);
        await flush();
        assert.equal(calls, before + 1, "late false settlement starts no extra attempt");
        const queries = r.queries.length;
        r.advance(delay - 1);
        await w.sweepPendingFinalizes();
        assert.equal(calls, before + 1, "timed-out authority must not retry before cooldown expiry");
        assert.equal(r.queries.length, queries, "cooldown precedes every predicate");
        r.advance(1);
      }
    }
  } finally {
    held.resolve(false);
    await flush();
  }
});

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

for (const change of ["removed", "replaced"] as const) {
  it(`registration wave skips a frozen identity ${change} while the first sixteen await inventory`, async t => {
    const r = rig(t, Array.from({ length: 17 }, (_, n) => entry(A, n + 1)));
    const frozen = [...r.records.values()];
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const held = deferred<boolean>();
    let calls = 0;
    r.setPredicate(async () => ++calls <= 16 ? held.promise : true);
    const key = `${A}:17`;
    const state = r.w as unknown as {
      readonly finalizeRetries: ReadonlyMap<string, unknown>;
      readonly finalizeAttempts: ReadonlyMap<string, unknown>;
    };
    const pass = r.w.sweepPendingFinalizes(undefined, frozen, new Set<Readonly<object>>());
    try {
      await flush();
      assert.equal(calls, 16, "the first wave holds sixteen inventory calls");
      assert.equal(r.queries.filter(q => q === "capture").length, 16);
      if (change === "removed") r.records.delete(key);
      else r.add(entry(A, 17));
    } finally {
      held.resolve(true);
      await pass;
    }
    assert.deepEqual({
      retry: state.finalizeRetries.has(key),
      fairness: state.finalizeAttempts.has(key),
      created: r.logger.info.filter(log =>
        log.fields.claim_generation === 17 && log.fields.transition === "created").length,
      captures: r.queries.filter(q => q === "capture").length,
      inventory: r.queries.filter(q => q === key).length,
    }, { retry: false, fairness: false, created: 0, captures: 16, inventory: 0 },
    "the stale frozen candidate creates no state or completion-capture query");
    assert.equal(r.failures().length, 16);
    if (change === "removed") r.add(entry(A, 17));
    const fresh = r.records.get(key)!;
    assert.notEqual(fresh.identity, frozen[16]!.identity);
    await r.w.sweepPendingFinalizes();
    assert.equal(calls, 17, "only the fresh identity attempts while the first sixteen cool");
    assert.equal(r.queries.filter(q => q === "capture").length, 17);
    assert.equal(r.queries.filter(q => q === key).length, 1);
    assert.equal(r.failures().length, 17);
    const failures = r.failures().filter(log => log.fields.claim_generation === 17);
    assert.equal(failures.length, 1);
    assert.equal(failures[0]!.fields.attempt_count, 1, "the fresh identity inherits no failure history");
    assert.equal(r.logger.info.filter(log =>
      log.fields.claim_generation === 17 && log.fields.transition === "created").length, 1);
  });
}

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

for (const operation of [
  "recovery-inventory pending", "ownership lookup", "final-ACK check",
  "recovery-report retirement", "queued conditional deletion",
  "completion notification via queued deletion", "completion notification via report",
] as const) {
  it(`authority expiry snapshots the active ${operation} before late settlement`, async t => {
    const r = rig(t);
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const held = deferred<void>();
    r.setPredicate(async () => false);
    const notification = operation.startsWith("completion notification");
    t.mock.method(r.client, "capturePublicationCompletionRetirement", () => {
      // Give the initial capture its own elapsed time; none belongs to the next operation.
      r.advance(125);
      t.mock.timers.tick(125);
      return notification ? {} : undefined;
    });
    if (operation === "final-ACK check") {
      t.mock.method(r.runner, "recoveryFinalAcknowledged", async () => {
        await held.promise;
        return false;
      });
    } else if (operation === "recovery-report retirement") {
      t.mock.method(r.runner, "retireRecoveryReport", async () => {
        await held.promise;
        return { retired: false };
      });
    } else if (operation === "recovery-inventory pending") {
      r.setPredicate(async () => { await held.promise; return false; });
    } else if (operation === "ownership lookup") {
      t.mock.method(r.client, "getRunOwnership", async () => {
        await held.promise;
        return { status: "completed", claim_generation: 999 };
      });
    } else if (operation === "queued conditional deletion") {
      t.mock.method(r.outbox, "retireFinalizeIfEligible", async () => {
        await held.promise;
        return false;
      });
    } else {
      // Exercise both notification call sites independently.
      if (operation.endsWith("via report")) {
        t.mock.method(r.runner, "retireRecoveryReport", async () =>
          ({ retired: true, authority: "archive" }));
      } else {
        t.mock.method(r.runner, "recoveryFinalAcknowledged", async () => true);
      }
      Object.assign(r.runner, {
        notifyPublicationCompletionRetired: async () => { await held.promise; },
      });
    }
    try {
      const pass = r.w.sweepPendingFinalizes();
      await flush();
      assert.equal(r.warnings.length, 0, "no diagnostic before expiry");
      r.advance(875);
      t.mock.timers.tick(875);
      // Move time again before catch processing; the diagnostic must retain timer-time values.
      r.advance(77);
      await pass;
      assert.equal(r.warnings.length, 1);
      const warning = r.warnings[0]!;
      assert.equal(warning.msg, "outbox: finalize retry retained a generation");
      const snapshot = {
        authority_step: notification ? "completion notification" : operation,
        authority_step_elapsed_ms: 875,
        authority_attempt_elapsed_ms: 1000,
      };
      for (const [key, value] of Object.entries(snapshot)) assert.equal(warning.fields[key], value);
      const saved = { ...warning.fields };
      const logs = r.logger.info.length;
      await r.w.sweepPendingFinalizes();
      assert.equal(r.warnings.length, 1, "skipped heartbeats emit no deadline warning");
      if (r.records.size) assert.equal(r.logger.info.length, logs, "cooling emits no heartbeat log");
      else assert.equal(r.logger.info.at(-1)!.fields.transition, "removed", "completed unlink is reconciled");
      held.resolve();
      await flush();
      assert.deepEqual(warning.fields, saved, "late progress cannot alter the expiry snapshot");
      assert.equal(r.warnings.length, 1);
    } finally {
      held.resolve();
      await flush();
    }
  });
}

for (const cancellation of ["shutdown", "pass"] as const) {
  it(`${cancellation} cancellation has no authority deadline diagnostic`, async t => {
    const r = rig(t);
    t.mock.timers.enable({ apis: ["setTimeout"] });
    t.mock.method(reportProofTiming, "finalizePassMs", () => 500);
    const held = deferred<boolean>();
    r.setPredicate(async () => held.promise);
    const controller = new AbortController();
    try {
      const pass = r.w.sweepPendingFinalizes(controller.signal);
      await flush();
      if (cancellation === "shutdown") controller.abort();
      else {
        r.advance(500);
        t.mock.timers.tick(500);
      }
      await pass;
      assert.equal(r.warnings.length, 1);
      assert.equal(r.warnings[0]!.fields.authority_step, undefined);
      assert.equal(r.warnings[0]!.fields.authority_step_elapsed_ms, undefined);
      assert.equal(r.warnings[0]!.fields.authority_attempt_elapsed_ms, undefined);
      assert.equal(r.failures().length, cancellation === "shutdown" ? 0 : 1);
    } finally {
      held.resolve(false);
      await flush();
    }
  });
}

for (const evidence of ["status", "generation", "inventory", "ACK", "report"] as const) {
  it(`changed ${evidence} evidence is first queried at cooldown expiry`, async t => {
    const r = rig(t);
    r.setPredicate(async () => false);
    let changed = false;
    let reads = 0;
    let evidenceReads = 0;
    let acknowledged = false;
    t.mock.method(r.client, "getRunOwnership", async () => {
      reads++;
      return {
        status: evidence === "status" && !changed ? "running" : "completed",
        claim_generation: evidence === "generation" && !changed ? 0 : 999,
      };
    });
    if (evidence === "inventory") r.setPredicate(async () => { reads++; return !changed; });
    if (evidence === "ACK") t.mock.method(r.runner, "recoveryFinalAcknowledged", async () => {
      reads++;
      evidenceReads++;
      acknowledged = changed;
      return acknowledged;
    });
    if (evidence === "report") t.mock.method(r.runner, "retireRecoveryReport", async () => {
      reads++;
      evidenceReads++;
      if (changed) r.records.delete(`${A}:1`);
      return { retired: changed };
    });
    // ACK must be observed before unlink; report retirement cannot fall back to unlink.
    if (evidence === "ACK" || evidence === "report") {
      t.mock.method(r.outbox, "retireFinalizeIfEligible", async () => {
        if (evidence !== "ACK" || !acknowledged) return false;
        r.records.delete(`${A}:1`);
        return true;
      });
    }
    await r.w.sweepPendingFinalizes();
    assert.equal(r.failures().length, 1);
    changed = true;
    const before = reads;
    const evidenceBefore = evidenceReads;
    const queries = [...r.queries];
    r.advance(14999);
    await r.w.sweepPendingFinalizes();
    assert.equal(reads, before);
    assert.equal(evidenceReads, evidenceBefore);
    assert.deepEqual(r.queries, queries, "all queries are skipped while cooling");
    r.advance(1);
    await r.w.sweepPendingFinalizes();
    assert.ok(reads > before);
    if (evidence === "ACK" || evidence === "report") {
      assert.equal(evidenceReads, evidenceBefore + 1, "the named evidence is queried at eligibility");
    }
    assert.equal(r.records.size, 0, "the observed evidence permits actual record retirement");
    assert.equal(r.logger.info.at(-1)!.fields.transition, "retired");
  });
}

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
