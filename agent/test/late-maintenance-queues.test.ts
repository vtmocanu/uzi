import test from "node:test";
import { AsyncLocalStorage } from "node:async_hooks";
import assert from "node:assert/strict";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { GitCache } from "../src/git.js";
import { SinkGate } from "../src/sink-gate.js";
import { RunDiskLocks } from "../src/run-disk-locks.js";
import { nullLogger } from "./helpers.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";

test("late recovery expiry signals an already owned Git action but joins before releasing", async () => {
  const git = new GitCache("/unused", nullLogger());
  let release!: () => void, ready!: () => void, expire!: () => void;
  let now = 0, settled = false, siblingRan = false, childSignal!: AbortSignal;
  let budget: MaintenanceBudget | undefined;
  const held = new Promise<void>(resolve => { ready = resolve; });
  const owned = MaintenanceBudget.runAttempt("owned", () => budget = new MaintenanceBudget(100, () => now), () => {
    expire = AsyncLocalStorage.bind(() => { MaintenanceBudget.observeCancellation(); now = 100; assert.throws(() => budget!.check()); });
    return git.withRecoveryOperation(new AbortController().signal, Infinity, signal => {
      childSignal = signal;
      return git.withBareLock("repo", async () => { ready(); await new Promise<void>(resolve => { release = resolve; }); });
    });
  });
  const result = owned.then(() => { settled = true; }, () => { settled = true; });
  await held;
  const sibling = git.withBareLock("repo", async () => { siblingRan = true; });
  expire();
  await new Promise<void>(resolve => setImmediate(resolve));
  try {
    assert.equal(childSignal.aborted, true);
    assert.equal(settled, false, "owned work must be joined");
    assert.equal(siblingRan, false);
  } finally { release(); await Promise.all([result, sibling]); budget?.dispose(); }
});

test("late expiry leaves queued Codex credential-free settle inert", async () => {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const safety = createCodexExecutionSafety(registry, async () => { throw new Error("unexpected spawn"); });
  let release!: () => void, ready!: () => void, expire!: () => void;
  const held = new Promise<void>(resolve => { ready = resolve; });
  const holder = safety.withBoundary({ boundary: "checkpoint", deadlineMs: 600_000 }, async () => {
    ready(); await new Promise<void>(resolve => { release = resolve; });
  });
  await held;
  let now = 0, settled = false;
  let budget: MaintenanceBudget | undefined;
  const waiter = MaintenanceBudget.runAttempt("settle", () => budget = new MaintenanceBudget(100, () => now), () => {
    expire = AsyncLocalStorage.bind(() => { MaintenanceBudget.observeCancellation(); now = 100; assert.throws(() => budget!.check()); });
    return safety.settleForCredentialFreeCapture(600_000);
  });
  const result = waiter.then(value => { settled = true; return value; });
  expire();
  await new Promise<void>(resolve => setImmediate(resolve));
  try {
    assert.equal(settled, true);
    assert.equal((await result).kind, "incomplete");
    assert.equal(registry.state(), "closed");
  } finally { release(); await holder; budget?.dispose(); }
  assert.equal(registry.state(), "closed");
});

for (const seam of ["git", "sink", "disk", "codex"] as const) test(`late cancellation releases queued ${seam} without FIFO overtake`, async () => {
  const git = new GitCache("/unused", nullLogger());
  const sink = new SinkGate();
  const disk = new RunDiskLocks();
  const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)), async () => { throw new Error("unexpected spawn"); });
  const run = (fn: () => Promise<void>) => seam === "git" ? git.withBareLock("repo", fn)
    : seam === "sink" ? sink.run(fn) : seam === "codex" ? safety.withBoundary({ boundary: "checkpoint", deadlineMs: 600_000 }, fn) : disk.withLock("run", fn);
  let release!: () => void, ready!: () => void;
  const held = new Promise<void>(resolve => { ready = resolve; });
  const holder = run(async () => { ready(); await new Promise<void>(resolve => { release = resolve; }); });
  await held;
  let now = 0, ran = false, siblingRan = false, settled = false;
  let budget: MaintenanceBudget | undefined;
  let expire!: () => void;
  const waiter = MaintenanceBudget.runAttempt("late", () => budget = new MaintenanceBudget(100, () => now), () => {
    expire = AsyncLocalStorage.bind(() => { MaintenanceBudget.observeCancellation(); now = 100; assert.throws(() => budget!.check()); });
    return run(async () => { ran = true; });
  });
  const result = waiter.then(() => { settled = true; }, () => { settled = true; });
  const sibling = run(async () => { siblingRan = true; });
  try {
    expire();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(settled, true, "late cancellation must release queued waiter while holder remains held");
    assert.equal(ran, false);
    assert.equal(siblingRan, false);
  } finally {
    release(); await Promise.all([holder, result, sibling]); budget?.dispose();
  }
  assert.equal(ran, false, "expired callback remains inert after release");
});
