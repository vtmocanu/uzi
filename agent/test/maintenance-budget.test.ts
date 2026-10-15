import test from "node:test";
import assert from "node:assert/strict";
import { MaintenanceBudget } from "../src/maintenance-budget.js";

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
