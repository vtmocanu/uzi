import { it } from "node:test";
import assert from "node:assert/strict";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { CodexExecutor } from "../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";

import { nullLogger } from "./helpers.js";

const log = nullLogger();
function executor() {
  return new CodexExecutor(log, "/unused", {} as ConstructorParameters<typeof CodexExecutor>[2]);
}
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
function safety(reg: ExecutionRegistry) {
  return createCodexExecutionSafety(reg, async () => { throw new Error("spawn forbidden"); },
    async () => { throw new Error("credential reconciliation forbidden"); });
}

for (const jump of [-1_000_000, 1_000_000]) it(`cumulative summary/drain/installer/drain budget with wall jump ${jump}`, async () => {
  let now = 0;
  const budget = new MaintenanceBudget(100, () => now);
  const exec = executor();
  const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const facade = safety(reg);
  exec.safety = facade;
  const seen: number[] = [];
  exec.closeAndSettlePlanSummary = async ms => { seen.push(ms!); now += 20; };
  facade.settleForCredentialFreeCapture = async ms => {
    assert.equal(MaintenanceBudget.current(), budget);
    seen.push(ms);
    now += 20;
    return { kind: "observed_empty" };
  };
  const install = deferred();
  Object.assign(exec, { depsInstall: { abort: new AbortController(), cancellation: new AbortController().signal,
    promise: install.promise.then(() => { now += 20; }), reported: false, noted: false } });
  const original = Date.now;
  try {
    await MaintenanceBudget.run(budget, async () => {
      Date.now = () => original() + jump;
      const pending = exec.settleForCredentialFreeCapture(90, "cancel");
      await Promise.resolve();
      install.resolve();
      assert.equal((await pending).kind, "observed_empty");
      assert.deepEqual(seen, [90, 70, 30]);
      assert.equal((await exec.settleForCredentialFreeCapture(100, "cancel")).kind, "incomplete");
      assert.equal(MaintenanceBudget.current(), budget);
    });
  } finally { Date.now = original; budget.dispose(); }
});

it("real summary joins consume the ambient cap before no-epoch cancel proof", async () => {
  let now = 0;
  const budget = new MaintenanceBudget(100, () => now);
  const exec = executor();
  const done = deferred();
  const ctrl = new AbortController();
  const flight = { ctrl, done: done.promise, delivered: false, abandoned: false, closed: false, generation: 1 };
  Object.assign(exec, { planFlights: new Set([flight]) });
  try {
    await MaintenanceBudget.run(budget, async () => {
      const capture = exec.settleForCredentialFreeCapture(20, "cancel");
      assert.equal(ctrl.signal.aborted, true);
      now = 21;
      done.resolve();
      assert.equal((await capture).kind, "incomplete");
      assert.equal(flight.abandoned, true);
      assert.equal(flight.closed, true);
    });
  } finally { done.resolve(); budget.dispose(); }
});

it("last root returns after deadline: incomplete even when cleanly reaped", async () => {
  let now = 0;
  const budget = new MaintenanceBudget(100, () => now);
  const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const reservation = reg.reserveLaunch("command");
  assert.equal(reservation.kind, "reserved");
  if (reservation.kind !== "reserved") return;
  reg.registerRoot(reservation.reservation, { kind: "command", dispose: async () => {},
    reap: async ms => { assert.equal(ms, 10); now = 11; return { ok: true }; } });
  try {
    await MaintenanceBudget.run(budget, async () => {
      assert.equal((await reg.settleForCapture(10)).kind, "incomplete");
      assert.equal(reg.reserveLaunch("command").kind, "denied");
    });
  } finally { budget.dispose(); }
});

it("expired queued drain never acts after holder release and preserves FIFO", async () => {
  let now = 0;
  const budget = new MaintenanceBudget(1000, () => now);
  const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const facade = createCodexExecutionSafety(reg, async () => { throw new Error("unused"); });
  const entered = deferred();
  const holder = deferred();
  const active = facade.withBoundary({ boundary: "checkpoint", deadlineMs: 1000 }, async () => {
    entered.resolve(); await holder.promise;
  });
  await entered.promise;
  let calls = 0;
  const original = reg.settleForCapture.bind(reg);
  reg.settleForCapture = async ms => { calls++; return original(ms); };
  try {
    await MaintenanceBudget.run(budget, async () => {
      const queued = facade.settleForCredentialFreeCapture(1000);
      now = 1001;
      assert.throws(() => budget.check(), /maintenance deadline exhausted/);
      assert.equal((await queued).kind, "incomplete");
    });
    let laterRan = false;
    const later = facade.withBoundary({ boundary: "checkpoint", deadlineMs: 1000 }, async () => { laterRan = true; });
    await Promise.resolve();
    assert.equal(laterRan, false);
    holder.resolve();
    await active;
    await later;
    assert.equal(laterRan, true);
    assert.equal(calls, 0);
  } finally { holder.resolve(); await active; budget.dispose(); }
});

it("unknown operation duration cannot establish an empty registry proof", async () => {
  const budget = new MaintenanceBudget();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  try {
    await MaintenanceBudget.run(budget, async () => {
      assert.equal((await registry.settleForCapture(Number.NaN)).kind, "incomplete");
    });
  } finally { budget.dispose(); }
});

it("no epoch cancel proof after summary expiry is incomplete; repeated attempts share object", async () => {
  let now = 0;
  const budget = new MaintenanceBudget(100, () => now);
  const exec = executor();
  exec.closeAndSettlePlanSummary = async () => { now += 60; };
  try {
    await MaintenanceBudget.run(budget, async () => {
      assert.equal((await exec.settleForCredentialFreeCapture(50, "cancel")).kind, "incomplete");
      assert.equal((await exec.settleForCredentialFreeCapture(100, "cancel")).kind, "incomplete");
      assert.equal(MaintenanceBudget.current(), budget);
    });
  } finally { budget.dispose(); }
});
