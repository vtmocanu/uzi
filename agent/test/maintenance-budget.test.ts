import test from "node:test";
import assert from "node:assert/strict";
import { MaintenanceBudget } from "../src/maintenance-budget.js";

test("producer clamp caps a long parent and keeps nested ordinary scopes", async t => {
  let now = 0;
  const parent = new MaintenanceBudget(600_000, () => now);
  t.after(() => parent.dispose());
  await assert.rejects(MaintenanceBudget.run(parent, () => MaintenanceBudget.runProducer(async child => {
    assert.equal(child.remainingMs(), 120_000);
    assert.equal(MaintenanceBudget.current(), child);
    MaintenanceBudget.run(parent, () => assert.equal(MaintenanceBudget.current(), child));
    now = 120_000;
    assert.throws(() => child.check(), /maintenance deadline/);
    assert.equal(parent.signal.aborted, false);
  }, { now: () => now })), /maintenance deadline/);
});

test("producer clamp inherits short parent and later pending attempt cancellation", async t => {
  let now = 0;
  const short = new MaintenanceBudget(20, () => now);
  const late = new MaintenanceBudget(30, () => now);
  t.after(() => { short.dispose(); late.dispose(); });
  await MaintenanceBudget.run(short, () => MaintenanceBudget.runProducer(async child => {
    assert.equal(child.remainingMs(), 20);
  }, { now: () => now }));
  let observe!: () => void;
  await MaintenanceBudget.runAttempt("pending", () => late, async () => {
    // Capture the parent continuation before entering the producer scope.
    const trigger = new Promise<void>(resolve => { observe = resolve; });
    const observer = trigger.then(() => { MaintenanceBudget.observeCancellation(); now = 30; assert.throws(() => late.check()); });
    const producer = MaintenanceBudget.runProducer(async child => {
      assert.equal(child.remainingMs(), 120_000);
      observe();
      await observer;
      assert.equal(child.signal.aborted, true);
    }, { now: () => now });
    await assert.rejects(producer, /maintenance deadline/);
  });
});

test("producer waits for owned action cleanup on cancellation and removes listeners", async () => {
  const controller = new AbortController();
  let cleaned = false;
  await assert.rejects(MaintenanceBudget.runProducer(async child => {
    controller.abort(new Error("cancelled"));
    assert.equal(child.signal.aborted, true);
    await Promise.resolve();
    cleaned = true;
  }, { signal: controller.signal }), /cancelled/);
  assert.equal(cleaned, true);
});

test("producer observes late cancellation in its own continuation without replacing the clamp", async t => {
  let now = 0, factories = 0;
  const parent = new MaintenanceBudget(20, () => now);
  t.after(() => parent.dispose());
  await assert.rejects(MaintenanceBudget.runAttempt("late-producer", () => {
    factories++;
    return parent;
  }, () => MaintenanceBudget.runProducer(async child => {
    assert.equal(MaintenanceBudget.observeCancellation(), child);
    assert.equal(factories, 1);
    assert.equal(child.remainingMs(), 20);
    now = 20;
    child.check();
  }, { now: () => now })), /maintenance deadline/);
  assert.equal(factories, 1);
});

test("producer checks tighter parent clock before its abort timer runs", async t => {
  let parentNow = 0;
  const parent = new MaintenanceBudget(50, () => parentNow);
  t.after(() => parent.dispose());
  await assert.rejects(MaintenanceBudget.run(parent, () => MaintenanceBudget.runProducer(async child => {
    parentNow = 50;
    child.check();
  }, { now: () => 0 })), /maintenance deadline/);
});

test("monotonic cumulative budget survives wall jumps, repeats and disposal", async t => {
  let now = 10;
  const budget = new MaintenanceBudget(undefined, () => now);
  t.after(() => budget.dispose());
  const deadline = budget.deadline;
  t.mock.method(Date, "now", () => -9_000_000);
  await MaintenanceBudget.run(budget, async () => {
    assert.equal(MaintenanceBudget.current(), budget);
    now += 300_000;
    budget.check();
    assert.equal(budget.remainingMs(), 300_000);
    t.mock.method(Date, "now", () => 9_000_000_000);
    await MaintenanceBudget.run(budget, async () => {
      assert.equal(MaintenanceBudget.current(), budget);
      assert.equal(budget.deadline, deadline);
      assert.equal(budget.remainingMs(), 300_000);
    });
    budget.dispose();
    now += 300_000;
    assert.equal(budget.remainingMs(), 0);
    assert.throws(() => budget.check(), /maintenance deadline/);
    assert.equal(budget.signal.aborted, true);
    assert.throws(() => MaintenanceBudget.run(budget, () => {}), /maintenance deadline/);
  });
  assert.equal(MaintenanceBudget.current(), undefined);
});

test("nested and concurrent run scopes retain object identity and isolate chains", async t => {
  const a = new MaintenanceBudget(), b = new MaintenanceBudget();
  t.after(() => { a.dispose(); b.dispose(); });
  await Promise.all([a, b].map(budget => MaintenanceBudget.run(budget, async () => {
    await Promise.resolve();
    assert.equal(MaintenanceBudget.current(), budget);
    MaintenanceBudget.run(budget === a ? b : a, () => assert.equal(MaintenanceBudget.current(), budget));
  })));
  assert.equal(MaintenanceBudget.current(), undefined);
});

test("RPC wait aborts and cleans up one late result", async t => {
  let now = 0, complete!: (value: string) => void;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  const rpc = new Promise<string>(resolve => { complete = resolve; });
  let cleaned = 0;
  const waiting = budget.raceIO(() => rpc, value => { assert.equal(value, "late"); cleaned++; });
  await Promise.resolve();
  now = 100;
  assert.throws(() => budget.check(), /maintenance deadline/);
  await assert.rejects(waiting, /maintenance deadline/);
  complete("late");
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(cleaned, 1);
  await assert.rejects(budget.raceIO(async () => "never"), /maintenance deadline/);
});

test("RPC checks budget after action even before timer fires", async t => {
  let now = 0;
  const budget = new MaintenanceBudget(100, () => now);
  t.after(() => budget.dispose());
  let cleaned = 0;
  await assert.rejects(budget.raceIO(async () => { now = 100; return "late"; }, () => { cleaned++; }), /maintenance deadline/);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(cleaned, 1);
});

test("a Git-style active budget scope retains attempt identity for nested isolation", async t => {
  const first = new MaintenanceBudget();
  const second = new MaintenanceBudget();
  t.after(() => { first.dispose(); second.dispose(); });
  await MaintenanceBudget.runAttempt("first:1", () => first, async () => {
    MaintenanceBudget.observeCancellation();
    await MaintenanceBudget.run(first, async () => {
      assert.equal(MaintenanceBudget.isAttempt("first:1"), true);
      await MaintenanceBudget.runAttempt("second:1", () => second, async () => {
        assert.equal(MaintenanceBudget.current(), undefined);
        assert.equal(MaintenanceBudget.observeCancellation(), second);
      });
      assert.equal(MaintenanceBudget.current(), first);
    });
  });
});

test("late attempt activation reaches awaiting callers once and isolates run and generation", async t => {
  const budget = new MaintenanceBudget();
  t.after(() => budget.dispose());
  let factories = 0, actions = 0;
  await MaintenanceBudget.runAttempt("run:1", () => { factories++; return budget; }, async () => {
    actions++;
    assert.equal(MaintenanceBudget.current(), undefined);
    await MaintenanceBudget.runAttempt("run:1", () => { throw new Error("renewed"); }, async () => {
      await Promise.resolve();
      MaintenanceBudget.observeCancellation();
    });
    assert.equal(MaintenanceBudget.current(), budget);
    for (const identity of ["other:1", "run:2"]) {
      await MaintenanceBudget.runAttempt(identity, () => { throw new Error("unused"); }, async () => {
        assert.equal(MaintenanceBudget.current(), undefined);
      });
    }
    assert.equal(MaintenanceBudget.observeCancellation(), budget);
  });
  assert.equal(factories, 1);
  assert.equal(actions, 1);
  assert.equal(MaintenanceBudget.current(), undefined);
});

test("operation cancellation forfeits only its inert waiter, retaining the original owner", async () => {
  const stop = new AbortController();
  let release!: (value: string) => void;
  let ownerSettled = false;
  const owner = new Promise<string>(resolve => { release = resolve; });
  void owner.then(() => { ownerSettled = true; });
  const waiting = MaintenanceBudget.waitFor(owner, stop.signal);
  const rejected = assert.rejects(waiting, /operation stopped/);
  try {
    stop.abort(new Error("operation stopped"));
    await rejected;
    assert.equal(ownerSettled, false);
  } finally { release("owner result"); await Promise.allSettled([owner, waiting, rejected]); }
  assert.equal(await owner, "owner result");
});

test("operation signal preserves ambient maintenance fences and late activation", async t => {
  let now = 0;
  const budget = new MaintenanceBudget(10, () => now);
  t.after(() => budget.dispose());
  const operation = new AbortController();
  let release!: () => void;
  const owner = new Promise<void>(resolve => { release = resolve; });
  try {
    await MaintenanceBudget.runAttempt("operation:1", () => budget, async () => {
      const waiting = MaintenanceBudget.waitFor(owner, operation.signal);
      const rejected = assert.rejects(waiting, /maintenance deadline/);
      MaintenanceBudget.observeCancellation();
      now = 10;
      assert.throws(() => budget.check(), /maintenance deadline/);
      await rejected;
      assert.equal(operation.signal.aborted, false);
    });
  } finally { release(); await owner; }
});

test("attempt activation inherits an expired explicit cap without renewing", t => {
  let now = 0, factories = 0;
  const budget = new MaintenanceBudget(10, () => now);
  t.after(() => budget.dispose());
  MaintenanceBudget.run(budget, () => {
    now = 10;
    assert.throws(() => MaintenanceBudget.runAttempt("run:1", () => { factories++; return budget; }, () => {}), /maintenance deadline/);
  });
  assert.equal(factories, 0);
});
