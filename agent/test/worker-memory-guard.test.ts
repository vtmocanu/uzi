import assert from "node:assert/strict";
import { test } from "node:test";
import { WorkerMemoryGuard } from "../src/worker-memory-guard.js";
import type { WorkerMemoryReader } from "../src/worker-memory-reader.js";

const settings = {
  enabled: true as const, reserveBytes: 200, hysteresisBytes: 100,
  sampleMs: 100, responseBudgetMs: 100, rearmMs: 100, maxInterventions: 2,
};
type Sample = Extract<ReturnType<WorkerMemoryReader["sample"]>, { available: true }>;
function sample(id: number, time: number, current = 100, overrides: Partial<Sample> = {}): Sample {
  return {
    available: true, sampleId: id, startedAtMs: time, completedAtMs: time,
    currentBytes: current, limitBytes: 1000,
    stat: { anon: 10, shmem: 10, slab_unreclaimable: 10, unevictable: 10 },
    pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
      full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } }, ...overrides,
  };
}
function intervention(guard: WorkerMemoryGuard) {
  assert.equal(guard.evaluate(sample(1, 0, 800), 0).kind, "hard");
  const ticket = guard.beginIntervention();
  assert.ok(ticket);
  return ticket;
}

test("total usage alone crosses H; stats are neither summed nor file charges subtracted", () => {
  const guard = new WorkerMemoryGuard(settings);
  assert.deepEqual(guard.thresholds(1000), { highBytes: 800, quietBytes: 700 });
  const hugeStats = { anon: 900, shmem: 900, slab_unreclaimable: 900, unevictable: 900 };
  assert.equal(guard.evaluate(sample(1, 0, 200, { stat: hugeStats }), 0).kind, "idle");
  assert.equal(guard.evaluate(sample(2, 10, 800), 10).kind, "hard");
});

test("early trigger needs total growth projection, increasing full PSI, and at least one stat growth", () => {
  const grown = sample(2, 100, 750, {
    stat: { anon: 11, shmem: 9, slab_unreclaimable: 9, unevictable: 9 },
    pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
      full: { avg10: 0, avg60: 0, avg300: 0, total: 1 } },
  });
  for (const [change, expected] of [
    [grown, "early"],
    [{ ...grown, stat: sample(1, 0).stat }, "idle"],
    [{ ...grown, pressure: sample(1, 0).pressure }, "idle"],
    [{ ...grown, currentBytes: 650 }, "idle"],
    [{ ...grown, limitBytes: 1100 }, "idle"],
  ] as const) {
    const guard = new WorkerMemoryGuard(settings);
    assert.equal(guard.evaluate(sample(1, 0, 700), 0).kind, "idle");
    assert.equal(guard.evaluate(change, 100).kind, expected);
  }
  const falling = new WorkerMemoryGuard(settings);
  falling.evaluate(sample(1, 0, 780), 0);
  assert.equal(falling.evaluate(grown, 100).kind, "idle");
});

test("early preceding sample must itself be fresh; hard threshold does not require PSI", () => {
  const guard = new WorkerMemoryGuard(settings);
  guard.evaluate(sample(1, 0, 700), 0);
  const next = sample(2, 201, 750);
  next.stat.anon = 20;
  next.pressure.full.total = 5;
  assert.equal(guard.evaluate(next, 201).kind, "idle");
  assert.equal(guard.evaluate(sample(3, 202, 900), 202).kind, "hard");
});

test("settings and limit thresholds fail closed at safe integer/range boundaries", () => {
  for (const patch of [
    { reserveBytes: 0 }, { hysteresisBytes: -1 }, { sampleMs: 1.5 },
    { responseBudgetMs: 2147483648 }, { rearmMs: NaN }, { maxInterventions: 10000 },
    { maxInterventions: 0 }, { reserveBytes: Number.MAX_SAFE_INTEGER + 1 },
  ]) assert.throws(() => new WorkerMemoryGuard({ ...settings, ...patch }));
  const guard = new WorkerMemoryGuard(settings);
  for (const limit of [0, 200, 300, -1, 500.5, Infinity]) assert.throws(() => guard.thresholds(limit));
  assert.equal(guard.evaluate(sample(1, 0, 900, { limitBytes: 300 }), 0).kind, "inhibited");
});

test("stale, incomplete, unlimited, replayed and out of order samples inhibit cancellation", () => {
  const variants = [
    sample(1, 0, 900, { completedAtMs: 201 }),
    sample(1, 10, 900, { completedAtMs: 9 }),
    sample(1, 0, 900, { sampleId: 0 }),
    sample(1, 0, 900, { stat: undefined as unknown as Sample["stat"] }),
    sample(1, 0, 900, { pressure: undefined as unknown as Sample["pressure"] }),
    sample(1, 0, 900, { currentBytes: NaN }),
  ];
  for (const input of variants) assert.equal(new WorkerMemoryGuard(settings).evaluate(input, 201).kind, "inhibited");
  for (const reason of ["unlimited", "absent", "malformed", "unverified", "oversized"] as const) {
    assert.equal(new WorkerMemoryGuard(settings).evaluate({
      available: false, reason, sampleId: 1, startedAtMs: 0, completedAtMs: 0,
    }, 0).kind, "inhibited");
  }
  const guard = new WorkerMemoryGuard(settings);
  guard.evaluate(sample(2, 20), 20);
  assert.equal(guard.evaluate(sample(2, 30, 900), 30).kind, "inhibited");
  assert.equal(guard.evaluate(sample(3, 19, 900), 30).kind, "inhibited");
  assert.equal(guard.evaluate(sample(4, 40, 900), 39).kind, "inhibited");
  // Oldest read, rather than completion, bounds freshness.
  assert.equal(new WorkerMemoryGuard(settings).evaluate(sample(1, 0, 900, { completedAtMs: 200 }), 201).kind, "inhibited");
});

test("single-flight fence requires exact drain and outcome evidence and continuous multi-sample dwell", () => {
  const guard = new WorkerMemoryGuard(settings);
  const ticket = intervention(guard);
  assert.equal(guard.beginIntervention(), undefined);
  assert.equal(guard.confirmDrain({ ...ticket }), false);
  assert.equal(guard.resolveOutcome({ ...ticket }), false);
  guard.resolveOutcome(ticket);
  assert.equal(guard.state.unconfirmedDrain, true);
  guard.evaluate(sample(2, 100), 100);
  guard.evaluate(sample(3, 200), 200);
  assert.equal(guard.state.armed, false);
  guard.confirmDrain(ticket);
  assert.equal(guard.state.pendingIntervention, false);
  assert.equal(guard.evaluate(sample(4, 300), 300).kind, "idle");
  assert.equal(guard.evaluate(sample(5, 400), 400).kind, "rearmed");
  assert.equal(guard.confirmDrain(ticket), false);
  assert.equal(guard.resolveOutcome(ticket), false);
  assert.equal(guard.evaluate(sample(6, 500, 800), 500).kind, "hard");
  assert.ok(guard.beginIntervention());
});

test("incomplete sample, changed limit, quiet-boundary equality and observation gaps reset dwell", () => {
  for (const reset of ["incomplete", "limit", "boundary", "gap"] as const) {
    const guard = new WorkerMemoryGuard(settings);
    const ticket = intervention(guard);
    guard.confirmDrain(ticket);
    guard.resolveOutcome(ticket);
    guard.evaluate(sample(2, 50), 50);
    let nextTime = 100;
    let limitBytes = 1000;
    if (reset === "incomplete") guard.evaluate({ available: false, reason: "absent", sampleId: 3, startedAtMs: 100, completedAtMs: 100 }, 100);
    if (reset === "limit") {
      limitBytes = 1100;
      guard.evaluate(sample(3, 100, 100, { limitBytes }), 100);
    }
    if (reset === "boundary") guard.evaluate(sample(3, 100, 700), 100);
    if (reset === "gap") {
      nextTime = 350;
      guard.evaluate(sample(3, 350), 350);
    }
    assert.equal(guard.evaluate(sample(4, nextTime + 50, 100, { limitBytes }), nextTime + 50).kind, "idle");
    assert.equal(guard.state.armed, false);
    assert.equal(guard.evaluate(sample(5, nextTime + 150, 100, { limitBytes }), nextTime + 150).kind, "rearmed");
  }
});

test("independent run/episode interventions are not lifetime capped; mutated sample cannot rewrite history", () => {
  const guard = new WorkerMemoryGuard({ ...settings, maxInterventions: 1 });
  const ticket = intervention(guard);
  guard.confirmDrain(ticket);
  guard.resolveOutcome(ticket);
  guard.evaluate(sample(2, 10), 10);
  assert.equal(guard.evaluate(sample(3, 110), 110).kind, "rearmed");
  assert.equal(guard.evaluate(sample(4, 120, 900), 120).kind, "hard");
  assert.ok(guard.beginIntervention());
  assert.equal(guard.state.interventions, 2);

  const history = new WorkerMemoryGuard(settings);
  const first = sample(1, 0, 700);
  history.evaluate(first, 0);
  first.currentBytes = 750;
  first.stat.anon = 99;
  const second = sample(2, 100, 750);
  second.stat.anon = 11;
  second.pressure.full.total = 1;
  assert.equal(history.evaluate(second, 100).kind, "early");
});

type Monitor = import("../src/worker-memory-guard.js").WorkerMemoryMonitor;
function barrier<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}
async function until(predicate: () => boolean) {
  for (let attempt = 0; attempt < 100 && !predicate(); attempt++) await Promise.resolve();
  assert.ok(predicate(), "barrier reached within bounded microtask turns");
}
async function fixture(options: {
  autoReserve?: boolean;
  holdNextReserve?: boolean;
  fractional?: boolean;
  sampleMs?: number;
  observe?: ReturnType<import("../src/worker-memory-commands.js").WorkerMemoryCommands["register"]>["observe"];
  cancel?: ReturnType<import("../src/worker-memory-commands.js").WorkerMemoryCommands["register"]>["cancel"];
  drain?: ReturnType<import("../src/worker-memory-commands.js").WorkerMemoryCommands["register"]>["drain"];
  policy?: { version: 1; max_interventions: number };
  count?: number;
  report?: (request: import("../src/protocol.js").MemoryOutcomeRequest) => Promise<import("../src/protocol.js").MemoryReservation>;
} = {}) {
  const { WorkerMemoryMonitor } = await import("../src/worker-memory-guard.js");
  const { WorkerMemoryCommands } = await import("../src/worker-memory-commands.js");
  const commands = new WorkerMemoryCommands();
  const input = { run_id: "11111111-1111-4111-8111-111111111111",
    worker_id: "22222222-2222-4222-8222-222222222222", register_nonce: "nonce",
    claim_generation: 1, memory_episode: 1, flight: {}, lifecycleRevision: 1 };
  const context = commands.begin({ ...input, memory_policy: options.policy, memory_intervention_count: options.count });
  let time = options.fractional ? 0.25 : 0;
  let id = 0;
  let usage = 900;
  let signals = 0;
  let drainSignals = 0;
  const budgets: number[] = [];
  const timerArgs: number[] = [];
  let reads = 0;
  let incarnation = { worker_id: input.worker_id, register_nonce: input.register_nonce, revision: 1 };
  const reservation = barrier<import("../src/protocol.js").MemoryReservation>();
  const nextReservation = barrier<import("../src/protocol.js").MemoryReservation>();
  const requests: import("../src/protocol.js").MemoryReservationRequest[] = [];
  const reports: import("../src/protocol.js").MemoryOutcomeRequest[] = [];
  const feedback: import("../src/protocol.js").WorkerMemoryPressureResult[] = [];
  const preserved: typeof context[] = [];
  const timers = new Set<{ at: number; callback: () => void }>();
  let confirmed = true;
  let reportHeld = false;
  let invalidate = () => {};
  const enroll = (ctx = context, pid = 10, rss = 100) => commands.register(ctx, {
    source: "codex-model-command", toolId: "tool", attemptId: "attempt",
    root: { pid, startTime: 10 }, command: "inert fixture",
    observe: async (budget) => {
      if (budget) { assert.ok(budget.signal); budgets.push(budget.remainingMs); }
      return options.observe ? options.observe(budget) : { available: true, complete: true, startedAtMs: time, completedAtMs: time,
        rows: [{ pid, startTime: 10, rssBytes: rss }] };
    },
    cancel: async (authority) => { assert.ok(authority?.authorized()); signals++; await options.cancel?.(authority); },
    drain: async (budget) => {
      drainSignals++;
      if (budget) { assert.ok(budget.signal); budgets.push(budget.remainingMs); }
      return options.drain ? options.drain(budget) : { confirmed };
    },
  });
  const entry = enroll();
  const ack = (request = requests[0]!, remaining = 1) => ({
    ...request, admitted: true, authorizing: true,
    allowance: { limit: request.policy.max_interventions, used: request.policy.max_interventions - remaining, remaining },
  });
  const monitor: Monitor = new WorkerMemoryMonitor({ ...settings, sampleMs: options.sampleMs ?? settings.sampleMs }, {
    commands, now: () => time, incarnation: () => incarnation,
    subscribeInvalidation: (callback) => { invalidate = callback; return () => {}; },
    sample: () => { reads++; return sample(++id, time, usage); },
    timer: (callback, ms) => { timerArgs.push(ms); const timer = { at: time + ms, callback }; timers.add(timer); return () => timers.delete(timer); },
    reserve: async (request, _signal, ms) => { budgets.push(ms); if (options.fractional) time += 0.25; requests.push(request); return options.holdNextReserve && requests.length > 1 ? nextReservation.promise : options.autoReserve ? ack(request) : reservation.promise; },
    report: async (request) => {
      reports.push(request);
      if (options.report) return options.report(request);
      if (reportHeld) return new Promise(() => {});
      const reserved = requests.find((r) => r.intervention_id === request.intervention_id)!;
      return { ...ack(reserved, Math.min(1, reserved.policy.max_interventions - 1)), authorizing: false, outcome: request.outcome };
    },
    feedback: async (result) => { feedback.push(result); },
    preserve: async (ctx) => { preserved.push(ctx); },
  });
  return { monitor, commands, context, input, entry, enroll, ack, reservation, nextReservation, requests, reports, feedback, preserved,
    budgets, timerArgs, timers, drainSignals: () => drainSignals,
    signals: () => signals, reads: () => reads, invalidate: () => invalidate(),
    rotate: () => { incarnation = { ...incarnation, revision: incarnation.revision + 1 }; },
    usage: (n: number) => { usage = n; },
    drain: (value: boolean) => { confirmed = value; },
    holdReport: () => { reportHeld = true; },
    advance: (ms: number) => { time += ms; for (const timer of timers) if (timer.at <= time) { timers.delete(timer); timer.callback(); } },
  };
}

for (const transition of ["retirement", "owner Resume/new episode", "replacement claim", "registration rotation", "shutdown", "selected-root replacement"] as const) {
  test("delayed ACK: " + transition, async () => {
    const f = await fixture();
    f.monitor.tick();
    await until(() => f.requests.length === 1);
    const original = f.requests[0]!;
    let next: typeof f.context | undefined;
    let root: typeof f.entry | undefined;
    let latch: ReturnType<typeof f.commands.acquireLatch>;
    let stop: Promise<void> | undefined;
    if (transition === "shutdown") stop = f.monitor.stop();
    else if (transition === "selected-root replacement") {
      f.commands.recordDrain(f.entry, { confirmed: true });
      f.commands.unregister(f.entry);
      // Same run replacement is installed with a new token so its admission latch is distinguishable.
      next = f.commands.begin({ ...f.input, lifecycleRevision: 2 });
    } else {
      if (transition === "registration rotation") { f.rotate(); f.invalidate(); }
      else f.commands.retire(f.context);
      next = f.commands.begin({ ...f.input,
        memory_episode: transition === "owner Resume/new episode" ? 2 : 1,
        claim_generation: transition === "replacement claim" ? 2 : 1,
        lifecycleRevision: 2, flight: {} });
    }
    if (next) { root = f.enroll(next, 20); latch = f.commands.acquireLatch(next); }
    f.reservation.resolve(f.ack());
    await f.monitor.settle();
    await stop;
    if (transition === "shutdown") assert.equal(f.reports.length, 0);
    else await until(() => f.reports.length > 0);
    assert.equal(f.signals(), 0);
  assert.equal(f.drainSignals(), 0);
    assert.ok(f.reports.every((r) => r.intervention_id === original.intervention_id && r.outcome === "no_signal"));
    assert.equal(f.monitor.state.unconfirmedDrain, transition !== "selected-root replacement");
    assert.equal(f.commands.remaining(f.context), undefined);
    if (next && root) {
      assert.ok(f.commands.isCurrent(next));
      assert.ok(f.commands.isRegistered(root));
      assert.equal(f.commands.remaining(next), undefined);
      assert.equal(f.commands.admitsCommands(next), false);
      assert.ok(f.commands.releaseLatch(latch!));
    }
    await f.monitor.stop();
  });
}

test("busy fast sampling continues while admission held; hard pressure survives disarmed policy", async () => {
  const f = await fixture();
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  for (let i = 0; i < 5; i++) { f.advance(1); f.monitor.tick(); }
  assert.equal(f.reads(), 6);
  assert.equal(f.requests.length, 1);
  f.reservation.resolve(f.ack());
  await f.monitor.settle();
  assert.equal(f.signals(), 1);
  assert.equal(f.feedback[0]?.outcome, "confirmed_drained");
  assert.equal(f.monitor.state.unresolvedOutcome, false);
  await f.monitor.stop();
});

test("hung outcome report does not delay ACK-authorized cancel; timeout keeps latch", async () => {
  const f = await fixture();
  f.holdReport();
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await until(() => f.reports.length === 1);
  assert.equal(f.signals(), 1);
  assert.equal(f.commands.admitsCommands(f.context), false);
  f.advance(100);
  await f.monitor.settle();
  assert.equal(f.monitor.state.unresolvedOutcome, true);
  await f.monitor.stop();
});

test("single flight unknown drain admits no future victim and supplies safety feedback", async () => {
  const f = await fixture();
  f.drain(false);
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await f.monitor.settle();
  assert.equal(f.feedback[0]?.outcome, "unknown");
  assert.equal(f.feedback[0]?.cancellation_confirmed, false);
  assert.equal(f.monitor.state.unconfirmedDrain, true);
  f.advance(1); f.monitor.tick();
  assert.equal(f.requests.length, 1);
  await f.monitor.stop();
});

test("mismatched tuple fails closed without renewed allowance or invented drain", async () => {
  const f = await fixture();
  f.drain(false);
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve({ ...f.ack(), memory_episode: 99 });
  await f.monitor.settle();
  assert.equal(f.signals(), 0);
  assert.equal(f.drainSignals(), 0);
  assert.equal(f.commands.remaining(f.context), undefined);
  assert.equal(f.monitor.state.unconfirmedDrain, true);
  assert.equal(f.feedback[0]?.outcome, "no_signal");
  assert.deepEqual(f.preserved, [f.context]);
  await f.monitor.stop();
});

test("largest root selected; exact latch released only after outcome resolution and quiet dwell", async () => {
  const f = await fixture();
  const big = f.enroll(f.context, 20, 500);
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await f.monitor.settle();
  assert.equal(f.feedback[0]?.command_id, big.commandId);
  assert.ok(f.commands.isRegistered(f.entry));
  assert.equal(f.commands.admitsCommands(f.context), false);
  f.usage(100); f.advance(1); f.monitor.tick();
  assert.equal(f.commands.admitsCommands(f.context), false);
  f.advance(100); f.monitor.tick();
  assert.equal(f.commands.admitsCommands(f.context), true);
  await f.monitor.stop();
});

test("reservation timeout fences context; late ACK reports original ID without signal or refund", async () => {
  const f = await fixture();
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.advance(100);
  await f.monitor.settle();
  assert.equal(f.signals(), 0);
  assert.equal(f.drainSignals(), 0);
  assert.equal(f.monitor.state.unconfirmedDrain, true);
  assert.equal(f.commands.isCurrent(f.context), false);
  f.reservation.resolve(f.ack());
  await until(() => f.reports.length === 2);
  assert.ok(f.reports.every((r) => r.outcome === "no_signal" && r.intervention_id === f.requests[0]?.intervention_id));
  await f.monitor.stop();
});

test("zero eligible commands authorize none", async () => {
  const f = await fixture();
  f.commands.recordDrain(f.entry, { confirmed: true });
  f.commands.unregister(f.entry);
  f.monitor.tick();
  assert.equal(f.monitor.busy, false);
  assert.equal(f.requests.length, 0);
  await f.monitor.stop();
});

test("old observation after replacement cannot select replacement root", async () => {
  const held = barrier<Parameters<import("../src/worker-memory-commands.js").WorkerMemoryCommands["select"]>[0][number]["observation"]>();
  const f = await fixture({ observe: () => held.promise });
  f.monitor.tick();
  await until(() => f.monitor.busy);
  const next = f.commands.begin({ ...f.input, flight: {}, lifecycleRevision: 2 });
  const replacement = f.enroll(next, 20);
  let joined = false;
  const join = f.commands.prepareOutcome(replacement);
  void join.then(() => { joined = true; });
  held.resolve({ available: true, complete: true, startedAtMs: 0, completedAtMs: 0,
    rows: [{ pid: 10, startTime: 10, rssBytes: 100 }] });
  await f.monitor.settle();
  assert.equal(f.requests.length, 0);
  assert.equal(f.signals(), 0);
  assert.equal(f.drainSignals(), 0);
  assert.ok(f.commands.isRegistered(replacement));
  assert.equal(f.commands.outcome(f.entry), undefined);
  assert.equal(f.commands.outcome(replacement), join);
  assert.equal(joined, false);
  assert.equal(f.commands.drainFailure(f.entry), undefined);
  assert.equal(f.commands.drainFailure(replacement), undefined);
  assert.deepEqual(f.feedback, []);
  await f.monitor.stop();
});

test("old drain after replacement cannot dispose new root or allowance", async () => {
  const held = barrier<{ confirmed: boolean }>();
  let draining = false;
  const f = await fixture({ drain: () => { draining = true; return held.promise; } });
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await until(() => draining);
  const next = f.commands.begin({ ...f.input, flight: {}, lifecycleRevision: 2 });
  const replacement = f.enroll(next, 20);
  const latch = f.commands.acquireLatch(next);
  held.resolve({ confirmed: true });
  await f.monitor.settle();
  assert.equal(f.monitor.state.unconfirmedDrain, false);
  assert.equal(f.monitor.state.unresolvedOutcome, false);
  assert.equal(f.monitor.state.interventions, 1);
  assert.ok(f.commands.isRegistered(replacement));
  assert.equal(f.commands.remaining(next), undefined);
  assert.equal(f.commands.admitsCommands(next), false);
  assert.ok(f.commands.releaseLatch(latch!));
  await f.monitor.stop();
});

test("per-run policy and consumed count are frozen; exhausted run parks without reservation", async () => {
  const policy = { version: 1 as const, max_interventions: 1 };
  const f = await fixture({ policy, count: 1 });
  policy.max_interventions = 2;
  f.monitor.tick();
  await f.monitor.settle();
  assert.equal(f.requests.length, 0);
  assert.equal(f.signals(), 0);
  assert.equal(f.drainSignals(), 0);
  assert.equal(f.context.memory_policy?.max_interventions, 1);
  assert.deepEqual(f.preserved, [f.context]);
  assert.equal(f.feedback[0]?.outcome, "no_signal");
  await f.monitor.stop();
});

test("admitted last allowance cancels first then requests preservation for captured run", async () => {
  const f = await fixture({ policy: { version: 1, max_interventions: 1 } });
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack(undefined, 0));
  await f.monitor.settle();
  assert.equal(f.signals(), 1);
  assert.deepEqual(f.preserved, [f.context]);
  assert.equal(f.commands.isCurrent(f.context), false);
  assert.equal(f.feedback[0]?.outcome, "confirmed_drained");
  await f.monitor.stop();
});

test("latest stale sample inhibits signal even with valid held ACK", async () => {
  const f = await fixture();
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.advance(201); // latest sample is stale without requiring a real timer sleep
  f.reservation.resolve(f.ack());
  await f.monitor.settle();
  assert.equal(f.signals(), 0);
  assert.equal(f.drainSignals(), 0);
  assert.equal(f.monitor.state.unconfirmedDrain, true);
  await f.monitor.stop();
});

test("old outcome ACK after replacement cannot release new latch or overwrite allowance", async () => {
  const held = barrier<import("../src/protocol.js").MemoryReservation>();
  const f = await fixture({ report: () => held.promise });
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await until(() => f.reports.length === 1);
  const next = f.commands.begin({ ...f.input, flight: {}, lifecycleRevision: 2,
    memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: 0 });
  const root = f.enroll(next, 20);
  const latch = f.commands.acquireLatch(next);
  held.resolve({ ...f.ack(), authorizing: false, outcome: "confirmed_drained" });
  await f.monitor.settle();
  assert.equal(f.monitor.state.unresolvedOutcome, false);
  assert.equal(f.monitor.state.armed, false);
  assert.equal(f.commands.isCurrent(next), true);
  assert.equal(f.signals(), 1);
  f.usage(100); f.advance(1); f.monitor.tick(); f.advance(100); f.monitor.tick();
  assert.equal(f.commands.remaining(next), 2);
  assert.ok(f.commands.isRegistered(root));
  assert.equal(f.commands.admitsCommands(next), false);
  assert.ok(f.commands.releaseLatch(latch!));
  await f.monitor.stop();
});

test("additional pre-cancel observation must contain exact selected root start time", async () => {
  let count = 0;
  const f = await fixture({ observe: async () => ({
    available: true, complete: true, startedAtMs: 0, completedAtMs: 0,
    rows: [{ pid: 10, startTime: ++count === 1 ? 10 : 11, rssBytes: 100 }],
  }) });
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await f.monitor.settle();
  assert.equal(f.signals(), 0);
  assert.equal(f.drainSignals(), 0);
  assert.equal(f.feedback[0]?.outcome, "no_signal");
  assert.equal(f.requests.length, 1);
  await f.monitor.stop();
});

for (const patch of [
  { authorizing: "yes" }, { policy: { version: 1, max_interventions: 99 } },
  { allowance: { limit: 2, used: 1, remaining: 2 } }, { outcome: "confirmed_drained", authorizing: true },
]) test("injected malformed ACK rejected: " + JSON.stringify(patch), async () => {
  const f = await fixture();
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve({ ...f.ack(), ...patch } as import("../src/protocol.js").MemoryReservation);
  await f.monitor.settle();
  assert.equal(f.signals(), 0);
  assert.equal(f.drainSignals(), 0);
  assert.equal(f.commands.remaining(f.context), undefined);
  await f.monitor.stop();
});

for (const failure of ["throw", "timeout", "mismatch", "stale", "denied", "quiet"] as const) {
  test("destructive drain is never called without authority: " + failure, async () => {
    const f = await fixture();
    f.monitor.tick();
    await until(() => f.requests.length === 1);
    if (failure === "throw") f.reservation.reject(new Error("API failure"));
    else if (failure === "timeout") f.advance(100);
    else {
      if (failure === "quiet") { f.usage(100); f.advance(1); f.monitor.tick(); }
      if (failure === "stale") f.commands.retire(f.context);
      f.reservation.resolve(failure === "mismatch" ? { ...f.ack(), memory_episode: 99 }
        : failure === "denied" ? { ...f.ack(undefined, 0), admitted: false, authorizing: false } : f.ack());
    }
    await f.monitor.settle();
    assert.equal(f.signals(), 0);
    assert.equal(f.drainSignals(), 0);
    assert.equal(f.monitor.state.unconfirmedDrain, true);
    assert.equal(f.feedback[0]?.outcome, "no_signal");
    f.commands.recordDrain(f.entry, { confirmed: true });
    assert.equal(f.monitor.state.unconfirmedDrain, false);
    assert.equal(f.feedback[0]?.outcome, "no_signal");
    await f.monitor.stop();
  });
}

test("fractional monotonic time supplies positive integer port budgets", async () => {
  const f = await fixture({ fractional: true });
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await f.monitor.settle();
  assert.equal(f.signals(), 1);
  assert.ok(f.budgets.length >= 2);
  assert.ok(f.budgets.every((ms) => Number.isInteger(ms) && ms >= 1 && ms <= 2147483647));
  const result = f.feedback[0]!;
  const request = f.requests[0]!;
  assert.equal(result.run_id, request.run_id);
  assert.equal(result.intervention_id, request.intervention_id);
  assert.equal(result.claim_generation, request.claim_generation);
  assert.equal(result.memory_episode, request.memory_episode);
  assert.equal(result.attempt_id, f.entry.attemptId);
  await f.monitor.stop();
});

test("one millisecond sampling yields with advancing fractional time and stop cancels the wait", async () => {
  const { WorkerMemoryMonitor } = await import("../src/worker-memory-guard.js");
  const { WorkerMemoryCommands } = await import("../src/worker-memory-commands.js");
  let time = 0;
  let reads = 0;
  let readLimitReached = false;
  let runningError: unknown;
  const timers = new Set<{ callback: () => void; ms: number }>();
  const timerArgs: number[] = [];
  const monitor = new WorkerMemoryMonitor({ ...settings, sampleMs: 1 }, {
    commands: new WorkerMemoryCommands(),
    now: () => { time += 0.25; return time; },
    incarnation: () => undefined,
    sample: () => {
      if (++reads > 10) {
        readLimitReached = true;
        throw new Error("sampling read limit");
      }
      return sample(reads, time, 100);
    },
    timer: (callback, ms) => {
      timerArgs.push(ms);
      const timer = { callback, ms };
      timers.add(timer);
      return () => { timers.delete(timer); };
    },
    reserve: async () => { throw new Error("unexpected reservation"); },
    report: async () => { throw new Error("unexpected report"); },
    feedback: async () => { throw new Error("unexpected feedback"); },
    preserve: async () => { throw new Error("unexpected preserve"); },
  });
  const running = monitor.run();
  void running.catch((error: unknown) => { runningError = error; });
  try {
    assert.equal(monitor.run(), running);
    await until(() => timers.size > 0 || readLimitReached);
    assert.deepEqual(timerArgs, [1]);
    assert.equal(reads, 1);
    for (let turn = 0; turn < 30; turn++) await Promise.resolve();
    assert.equal(reads, 1);
    const timer = [...timers][0]!;
    timers.delete(timer);
    timer.callback();
    await until(() => reads === 2 && timers.size === 1);
    assert.deepEqual(timerArgs, [1, 1]);
    const stoppedTimer = [...timers][0]!;
    let joined = false;
    const stopping = monitor.stop().then(() => { joined = true; });
    assert.equal(timers.size, 0, "stop clears the interval before its first await");
    await until(() => joined);
    await stopping;
    await running;
    stoppedTimer.callback();
    monitor.tick();
    assert.equal(monitor.run(), running);
    for (let turn = 0; turn < 30; turn++) await Promise.resolve();
    assert.equal(reads, 2);
    assert.equal(timers.size, 0);
    assert.equal(runningError, undefined);
  } finally {
    await monitor.stop();
  }
});

test("maximum sample interval uses safe timers and stop joins run", async () => {
  const f = await fixture({ sampleMs: 2147483647 });
  f.usage(100);
  const running = f.monitor.run();
  await until(() => f.timerArgs.length > 0);
  assert.deepEqual(f.timerArgs, [2147483647]);
  await f.monitor.stop();
  await running;
  assert.equal(f.timers.size, 0);
});

test("late ACK after joined stop records history without IO, timers or signals", async () => {
  const f = await fixture();
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  await f.monitor.stop();
  const reports = f.reports.length;
  const timers = f.timerArgs.length;
  f.reservation.resolve(f.ack());
  // The late continuation and its history update complete in bounded microtask turns.
  for (let attempt = 0; attempt < 20; attempt++) await Promise.resolve();
  assert.equal(f.monitor.history.at(-1)?.outcome, "no_signal");
  assert.equal(f.reports.length, reports);
  assert.equal(f.timerArgs.length, timers);
  assert.equal(f.signals(), 0);
  assert.equal(f.drainSignals(), 0);
});

for (const replaced of [false, true]) {
  test("held authorized cancellation after pressure clears; replaced=" + replaced, async () => {
    const held = barrier<void>();
    let cancelBudget: Parameters<ReturnType<import("../src/worker-memory-commands.js").WorkerMemoryCommands["register"]>["cancel"]>[0];
    const f = await fixture({ cancel: (budget) => { cancelBudget = budget; return held.promise; } });
    f.enroll(f.context, 30, 1);
    f.monitor.tick();
    await until(() => f.requests.length === 1);
    f.reservation.resolve(f.ack());
    await until(() => f.signals() === 1);
    assert.equal(cancelBudget!.authorized(), true);
    f.usage(100); f.advance(10); f.monitor.tick();
    assert.equal(cancelBudget!.authorized(), false);
    let replacement: typeof f.entry | undefined;
    if (replaced) {
      const next = f.commands.begin({ ...f.input, lifecycleRevision: 2 });
      replacement = f.enroll(next, 20);
    }
    held.resolve();
    await f.monitor.settle();
    assert.equal(f.signals(), 1);
    assert.equal(f.drainSignals(), replaced ? 0 : 1);
    assert.equal(f.feedback[0]?.outcome, replaced ? "unknown" : "confirmed_drained");
    assert.equal(f.monitor.state.unresolvedOutcome, false);
    assert.equal(f.monitor.state.unconfirmedDrain, replaced);
    if (replacement) assert.equal(f.commands.isRegistered(replacement), true);
    await f.monitor.stop();
  });
}

for (const confirmed of [true, false]) {
  test("parked historical outcome needs confirmed stop and quiet dwell before unrelated reservation: " + confirmed, async () => {
    const f = await fixture({ policy: { version: 1, max_interventions: 1 }, holdNextReserve: true });
    f.drain(confirmed);
    f.monitor.tick();
    await until(() => f.requests.length === 1);
    f.reservation.resolve(f.ack(undefined, 0));
    await f.monitor.settle();
    assert.deepEqual(f.preserved, [f.context]);
    assert.equal(f.commands.isCurrent(f.context), false);
    assert.equal(f.monitor.state.unresolvedOutcome, false);
    assert.equal(f.monitor.state.unconfirmedDrain, !confirmed);
    const next = f.commands.begin({ ...f.input, run_id: "44444444-4444-4444-8444-444444444444",
      memory_policy: { version: 1, max_interventions: 2 } });
    const root = f.enroll(next, 20);
    f.usage(100); f.advance(1); f.monitor.tick(); f.advance(100); f.monitor.tick();
    assert.equal(f.monitor.state.armed, confirmed);
    assert.equal(f.commands.remaining(next), 2);
    assert.equal(f.commands.isRegistered(root), true);
    f.usage(900); f.advance(1); f.monitor.tick();
    if (confirmed) {
      await until(() => f.requests.length === 2);
      assert.equal(f.requests[1]?.run_id, next.run_id);
      assert.notEqual(f.requests[1]?.intervention_id, f.requests[0]?.intervention_id);
      assert.equal(f.signals(), 1);
      assert.equal(f.drainSignals(), 1);
      f.nextReservation.resolve(f.ack(f.requests[1]));
      await f.monitor.settle();
      assert.equal(f.signals(), 2);
      assert.equal(f.drainSignals(), 2);
      assert.equal(f.feedback[1]?.outcome, "confirmed_drained");
    } else {
      await f.monitor.settle();
      assert.equal(f.requests.length, 1);
      assert.equal(f.signals(), 1);
    }
    await f.monitor.stop();
  });
}

test("original tool join settles after cancellation routine unregisters selected entry", async () => {
  let f: Awaited<ReturnType<typeof fixture>>;
  f = await fixture({ cancel: async () => {
    assert.equal(f.commands.recordDrain(f.entry, { confirmed: true }), true);
    assert.equal(f.commands.unregister(f.entry), true);
  } });
  const join = f.commands.prepareOutcome(f.entry);
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await f.monitor.settle();
  assert.equal((await join).command_id, f.entry.commandId);
  assert.equal((await join).outcome, "confirmed_drained");
  assert.equal(f.commands.outcome(f.entry), join);
  assert.equal(f.drainSignals(), 0);
  assert.equal(f.monitor.state.unconfirmedDrain, false);
  await f.monitor.stop();
});

for (const phase of ["cancel", "drain"] as const) {
  for (const replaced of [false, true]) {
    test("command cleanup while authorized " + phase + " is held; replaced=" + replaced, async () => {
      const held = barrier<void>();
      let entered = false;
      const wait = () => { entered = true; return held.promise; };
      const f = await fixture(phase === "cancel"
        ? { cancel: wait }
        : { drain: async () => { await wait(); return { confirmed: true }; } });
      const join = f.commands.prepareOutcome(f.entry);
      let settlements = 0;
      void join.then(() => { settlements++; });
      f.monitor.tick();
      await until(() => f.requests.length === 1);
      f.reservation.resolve(f.ack());
      await until(() => entered);
      assert.equal(f.commands.recordDrain(f.entry, { confirmed: true }), true);
      assert.equal(f.commands.unregister(f.entry), true);
      let replacement: typeof f.entry | undefined;
      let latch: ReturnType<typeof f.commands.acquireLatch>;
      let replacementSettled = false;
      if (replaced) {
        const next = f.commands.begin({ ...f.input, flight: {}, lifecycleRevision: 2,
          memory_policy: { version: 1, max_interventions: 2 } });
        replacement = f.enroll(next, f.entry.root.pid);
        latch = f.commands.acquireLatch(next);
        void f.commands.prepareOutcome(replacement).then(() => { replacementSettled = true; });
      }
      held.resolve();
      await f.monitor.settle();
      const result = await join;
      assert.equal(result.outcome, "confirmed_drained");
      assert.equal(result.cancellation_confirmed, true);
      assert.equal(result.command_id, f.entry.commandId);
      assert.equal(f.commands.resolveToolOutcome(f.entry, result), false);
      assert.equal(settlements, 1);
      assert.equal(f.feedback.length, 1);
      assert.equal(f.reports.length, 1);
      assert.equal(f.reports[0]?.outcome, "confirmed_drained");
      assert.equal(f.monitor.history[0]?.outcome, "confirmed_drained");
      assert.equal(f.signals(), 1);
      assert.equal(f.drainSignals(), phase === "cancel" ? 0 : 1);
      assert.equal(f.monitor.state.unconfirmedDrain, false);
      assert.equal(f.monitor.state.unresolvedOutcome, false);
      assert.deepEqual(f.preserved, []);
      if (replacement) {
        assert.equal(f.commands.isRegistered(replacement), true);
        assert.equal(f.commands.hasConfirmedStop(replacement), false);
        assert.equal(f.commands.remaining(replacement.context), 2);
        assert.equal(f.commands.admitsCommands(replacement.context), false);
        assert.equal(replacementSettled, false);
        assert.equal(f.commands.releaseLatch(latch!), true);
      }
      await f.monitor.stop();
    });
  }
}

test("old outcome echo cannot resolve newer held ticket", async () => {
  let original: import("../src/protocol.js").MemoryReservation;
  const f = await fixture({ holdNextReserve: true, report: async (request) => {
    if (!original) original = { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: 1, remaining: 1 } };
    return original;
  } });
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await f.monitor.settle();
  assert.equal(f.monitor.state.unresolvedOutcome, false);
  f.commands.unregister(f.entry);
  f.usage(100); f.advance(1); f.monitor.tick(); f.advance(100); f.monitor.tick();
  const next = f.commands.begin({ ...f.input, lifecycleRevision: 2,
    memory_policy: { version: 1, max_interventions: 2 } });
  const root = f.enroll(next, 20);
  f.usage(900); f.advance(1); f.monitor.tick();
  await until(() => f.requests.length === 2);
  assert.equal(f.signals(), 1);
  f.nextReservation.resolve(f.ack(f.requests[1]));
  await f.monitor.settle();
  assert.equal(f.monitor.state.unresolvedOutcome, true);
  assert.equal(f.commands.remaining(next), 1);
  assert.equal(f.commands.admitsCommands(next), false);
  assert.equal(f.commands.outcome(root) !== undefined, true);
  assert.equal(f.signals(), 2);
  await f.monitor.stop();
});

test("quiet pressure permits current outcome ACK resolution after authorized cancel", async () => {
  const held = barrier<import("../src/protocol.js").MemoryReservation>();
  const f = await fixture({ report: () => held.promise });
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.reservation.resolve(f.ack());
  await until(() => f.reports.length === 1);
  f.usage(100); f.advance(1); f.monitor.tick();
  held.resolve({ ...f.ack(), authorizing: false, outcome: "confirmed_drained" });
  await f.monitor.settle();
  assert.equal(f.monitor.state.unresolvedOutcome, false);
  await f.monitor.stop();
});

test("completed journal is capped without retaining flight, roots or callbacks", async () => {
  const f = await fixture({ autoReserve: true });
  let entry = f.entry;
  for (let pass = 0; pass < 260; pass++) {
    f.usage(900); f.advance(1); f.monitor.tick();
    await f.monitor.settle();
    assert.equal(f.monitor.state.unresolvedOutcome, false);
    f.commands.unregister(entry);
    f.usage(100); f.advance(1); f.monitor.tick(); f.advance(100); f.monitor.tick();
    assert.equal(f.commands.admitsCommands(f.context), true);
    if (pass < 259) entry = f.enroll();
  }
  assert.equal(f.monitor.history.length, 256);
  assert.ok(f.monitor.history.every((item) => item.outcome === "confirmed_drained"));
  assert.equal(JSON.stringify(f.monitor.history).includes("flight"), false);
  assert.deepEqual(Object.keys(f.monitor.history[0]!).sort(), ["outcome", "request", "status"]);
  await f.monitor.stop();
});

test("drain receives the remaining pass deadline and is aborted when it expires", async () => {
  let budget: Parameters<ReturnType<import("../src/worker-memory-commands.js").WorkerMemoryCommands["register"]>["drain"]>[0];
  const f = await fixture({ drain: (value) => { budget = value; return new Promise(() => {}); } });
  f.monitor.tick();
  await until(() => f.requests.length === 1);
  f.advance(20);
  f.reservation.resolve(f.ack());
  await until(() => budget !== undefined);
  assert.equal(budget!.remainingMs, 80);
  assert.equal(budget!.signal.aborted, false);
  assert.equal(budget!.authorized(), true);
  f.advance(80);
  await until(() => f.reports.length === 1);
  await f.monitor.settle();
  assert.equal(budget!.signal.aborted, true);
  assert.equal(budget!.authorized(), false);
  assert.equal(f.feedback[0]?.outcome, "unknown");
  assert.equal(f.monitor.state.unconfirmedDrain, true);
  await f.monitor.stop();
});
