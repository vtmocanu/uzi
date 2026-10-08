import assert from "node:assert/strict";
import { test } from "node:test";
import type { WorkerMemoryPressureResult } from "../src/protocol.js";
import { WorkerMemoryCommands } from "../src/worker-memory-commands.js";

const contextInput = () => ({
  run_id: "11111111-1111-4111-8111-111111111111", worker_id: "22222222-2222-4222-8222-222222222222", register_nonce: "nonce",
  claim_generation: 1, memory_episode: 1, flight: {}, lifecycleRevision: 1,
});
type Entry = ReturnType<WorkerMemoryCommands["register"]>;
type Observation = Parameters<WorkerMemoryCommands["select"]>[0][number]["observation"];
const row = (pid: number, rssBytes = 100, startTime = 10) => ({ pid, startTime, rssBytes });
function observation(rows = [row(10)], time = 100): Observation {
  return { available: true, complete: true, startedAtMs: time, completedAtMs: time, rows };
}
function setup() {
  const registry = new WorkerMemoryCommands();
  const context = registry.begin(contextInput());
  return { registry, context };
}
function enroll(registry: WorkerMemoryCommands, context: ReturnType<WorkerMemoryCommands["begin"]>, pid = 10, startTime = 10): Entry {
  return registry.register(context, {
    source: "codex-model-command", toolId: "tool", attemptId: "attempt",
    root: { pid, startTime }, command: "literal inert fixture", observe: async () => observation(),
    cancel: async () => {}, drain: async () => ({ confirmed: false }),
  });
}

test("exact prepared outcome survives unregister and PID reuse without settling replacement", async () => {
  const { registry, context } = setup();
  const old = enroll(registry, context);
  const join = registry.prepareOutcome(old);
  const result: WorkerMemoryPressureResult = {
    run_id: context.run_id, worker_id: context.worker_id, claim_generation: context.claim_generation,
    memory_episode: context.memory_episode, intervention_id: "33333333-3333-4333-8333-333333333333",
    attempt_id: old.attemptId, command_id: old.commandId, tool_id: old.toolId, command: old.command,
    code: "worker_memory_pressure", observed_total_bytes: 900, limit_bytes: 1000, tree_rss_bytes_approx: 100,
    cancellation_confirmed: true, outcome: "confirmed_drained", retry_blocked: true,
    guidance: "Use smaller or serial commands after recovery.",
  };
  registry.recordDrain(old, { confirmed: true });
  assert.equal(registry.unregister(old), true);
  const next = registry.begin({ ...contextInput(), lifecycleRevision: 2 });
  const root = enroll(registry, next, 10, 99);
  let newSettled = false;
  void registry.prepareOutcome(root).then(() => { newSettled = true; });
  for (const patch of [
    { run_id: next.worker_id }, { worker_id: next.run_id }, { claim_generation: 2 },
    { memory_episode: 2 }, { command_id: root.commandId }, { tool_id: "other" },
    { attempt_id: "other" }, { intervention_id: "bad" },
    { observed_total_bytes: -1 }, { tree_rss_bytes_approx: NaN }, { command: "other" },
  ]) assert.equal(registry.resolveToolOutcome(old, { ...result, ...patch }), false);
  assert.equal(registry.resolveToolOutcome({ ...old }, result), false);
  assert.equal(registry.resolveToolOutcome(old, result), true);
  assert.equal(registry.resolveToolOutcome(old, result), false);
  assert.deepEqual(await join, result);
  assert.equal(registry.outcome(old), join);
  assert.equal(newSettled, false);
  assert.equal(registry.isRegistered(root), true);
  assert.equal(registry.isCurrent(next), true);
});

test("context binding requires UUIDs, positive generation and a bounded nonce", () => {
  const registry = new WorkerMemoryCommands();
  for (const patch of [
    { run_id: "run" }, { worker_id: "worker" }, { claim_generation: 0 },
    { claim_generation: -1 }, { claim_generation: Number.MAX_SAFE_INTEGER + 1 },
    { memory_episode: -1 }, { register_nonce: "" }, { register_nonce: "é".repeat(65) },
  ]) assert.throws(() => registry.begin({ ...contextInput(), ...patch }));
  assert.ok(registry.begin({ ...contextInput(), memory_episode: 0, register_nonce: "x".repeat(128) }));
});

test("bounded enumeration fails closed without observing or discarding retained entries", () => {
  const { registry, context } = setup();
  const entries = Array.from({ length: 256 }, (_, i) => enroll(registry, context, i + 1));
  assert.equal(registry.eligibleEntries()?.length, 256);
  assert.ok(Object.isFrozen(registry.eligibleEntries()));
  const extra = enroll(registry, context, 300);
  assert.equal(registry.eligibleEntries(), undefined);
  assert.equal(registry.select([{ entry: extra, observation: observation([row(300)]) }], 100, 200), undefined);
  registry.retire(context);
  assert.equal(registry.eligibleEntries(), undefined);
  registry.recordDrain(extra, { confirmed: false, detail: "unknown" });
  assert.equal(registry.unregister(extra), false);
  registry.recordDrain(extra, { confirmed: true });
  assert.equal(registry.unregister(extra), true);
  assert.deepEqual(registry.eligibleEntries(), []);
  const next = registry.begin(contextInput());
  assert.equal(registry.isRegistered(entries[0]!), false);
  assert.ok(registry.isCurrent(next));
  const protectedRegistry = new WorkerMemoryCommands();
  const protectedContext = protectedRegistry.begin(contextInput());
  const candidate = enroll(protectedRegistry, protectedContext, 400);
  const roots = Array.from({ length: 257 }, (_, i) => protectedRegistry.protect("provider", { pid: i + 1000, startTime: 10 }));
  assert.equal(protectedRegistry.eligibleEntries(), undefined);
  assert.equal(protectedRegistry.select([{ entry: candidate, observation: observation([row(400)]) }], 100, 200), undefined);
  assert.equal(protectedRegistry.unprotect(roots[0]!), true);
  assert.equal(protectedRegistry.select([{ entry: candidate, observation: observation([row(400)]) }], 100, 200)?.entry, candidate);
});

test("context tokens capture identity and revision; same run/PID replacement invalidates synchronously", () => {
  const { registry, context } = setup();
  const entry = enroll(registry, context);
  const latch = registry.acquireLatch(context);
  assert.ok(latch);
  assert.equal(registry.isCurrent({ ...context }), false);
  assert.equal(registry.isCurrent(context, {}, 1), false);
  assert.equal(registry.isCurrent(context, context.flight, 2), false);
  assert.equal(Object.isFrozen(context), true);
  assert.equal(Object.isFrozen(entry), true);
  assert.equal(Object.isFrozen(entry.root), true);
  const next = registry.begin({ ...contextInput(), flight: context.flight });
  assert.equal(registry.isCurrent(context), false);
  assert.equal(registry.isRegistered(entry), false);
  assert.equal(registry.retire(context), false);
  assert.equal(registry.releaseLatch(latch), false);
  assert.equal(registry.admitsCommands(next), true);
  const nextLatch = registry.acquireLatch(next);
  assert.ok(nextLatch);
  assert.equal(registry.releaseLatch({ ...nextLatch }), false);
  assert.equal(registry.releaseLatch(latch), false);
  assert.equal(registry.admitsCommands(next), false);
  assert.equal(registry.releaseLatch(nextLatch), true);
  assert.equal(registry.releaseLatch(nextLatch), false);
});

test("retirement, registration rotation, shutdown and memory park have explicit invalidation seams", () => {
  for (const invalidate of [
    (r: WorkerMemoryCommands, c: ReturnType<WorkerMemoryCommands["begin"]>) => r.retire(c),
    (r: WorkerMemoryCommands, c: ReturnType<WorkerMemoryCommands["begin"]>) => r.memoryPark(c),
    (r: WorkerMemoryCommands) => r.registrationRotated(),
    (r: WorkerMemoryCommands) => r.shutdown(),
  ]) {
    const { registry, context } = setup();
    const entry = enroll(registry, context);
    invalidate(registry, context);
    assert.equal(registry.isCurrent(context), false);
    assert.equal(registry.isRegistered(entry), false);
    assert.equal(registry.acquireLatch(context), undefined);
    assert.equal(registry.unregister(entry), false);
  }
  const { registry } = setup();
  registry.shutdown();
  assert.throws(() => registry.begin(contextInput()));
});

test("enrollment is Codex-only, UUID worker-owned, command redacted before bounded sanitization", () => {
  const secret = ["private", "-fixture-value"].join("");
  const registry = new WorkerMemoryCommands([secret]);
  const context = registry.begin(contextInput());
  const input = {
    source: "codex-model-command" as const, toolId: "tool", attemptId: "attempt",
    root: { pid: 10, startTime: 10 }, command: secret + "\n\u202e" + "x".repeat(2000),
    observe: async () => observation(), cancel: async () => {}, drain: async () => ({ confirmed: false }),
  };
  const entry = registry.register(context, input);
  assert.match(entry.identity, /^[0-9a-f-]{36}$/);
  assert.match(entry.commandId, /^[0-9a-f-]{36}$/);
  assert.notEqual(entry.commandId, registry.register(context, { ...input, commandId: "untrusted" } as typeof input).commandId);
  assert.equal(entry.command.includes(secret), false);
  assert.ok(entry.command.startsWith("***REDACTED***??"));
  assert.ok(entry.command.length <= 1024);
  assert.equal(registry.isRegistered({ ...entry }), false);
  for (const source of ["claude", "provider", "boundary", "sink", "marks"]) {
    assert.throws(() => registry.register(context, { ...input, source: source as typeof input.source }));
  }
  for (const root of [{ pid: 0, startTime: 10 }, { pid: 10, startTime: NaN }]) {
    assert.throws(() => registry.register(context, { ...input, root }));
  }
  const latch = registry.acquireLatch(context);
  assert.ok(latch);
  assert.throws(() => registry.register(context, input));
  assert.equal(registry.acquireLatch(context), undefined);
});

test("failed drain evidence survives retirement; exact confirmed entry alone can unregister", () => {
  const { registry, context } = setup();
  const old = enroll(registry, context);
  registry.recordDrain(old, { confirmed: false, detail: "failed\n" });
  registry.retire(context);
  assert.equal(registry.drainFailure(old), "failed?");
  assert.equal(registry.unregister(old), false);
  const replacement = registry.begin(contextInput());
  const next = enroll(registry, replacement);
  assert.equal(registry.recordDrain({ ...old }, { confirmed: true }), false);
  assert.equal(registry.hasConfirmedStop(old), false);
  registry.recordDrain(old, { confirmed: true });
  assert.equal(registry.unregister(old), true);
  assert.equal(registry.hasConfirmedStop(old), true);
  assert.equal(registry.hasConfirmedStop({ ...old }), false);
  assert.equal(registry.hasConfirmedStop(next), false);
  assert.equal(registry.isRegistered(next), true);
  assert.equal(registry.unregister(next), false);
  assert.equal(registry.recordDrain(old, { confirmed: true }), false);
});

test("largest tree wins with deterministic PID tie and approximate RSS; callbacks are not used to rank", () => {
  const { registry, context } = setup();
  const a = enroll(registry, context, 10);
  const b = enroll(registry, context, 20);
  const first = { entry: a, observation: observation([row(10, 200)]) };
  const second = { entry: b, observation: observation([row(20, 100), row(21, 200)]) };
  assert.equal(registry.select([first, second], 100, 200)?.entry, b);
  assert.equal(registry.select([{ ...first, observation: observation([row(10, 300)]) }, second], 100, 200)?.entry, a);
  const selected = registry.select([second], 100, 200);
  assert.ok(selected);
  assert.equal(selected.rssBytesApprox, 300);
  assert.equal(Object.isFrozen(selected.observation.rows), true);
  assert.equal(registry.checkBeforeCancel(selected, second.observation, 100, 200), true);
  registry.memoryPark(context);
  assert.equal(registry.checkBeforeCancel(selected, second.observation, 100, 200), false);
});

test("unavailable, incomplete, stale, malformed and unattributed candidates are disqualified without signalling", () => {
  const { registry, context } = setup();
  const entry = enroll(registry, context);
  for (const o of [
    { ...observation(), available: false }, { ...observation(), complete: false },
    observation([row(10)], 0), observation([row(11)]),
    observation([row(10, -1)]), observation([row(10, Number.MAX_SAFE_INTEGER + 1)]),
    observation([row(10, 1, 0)]), observation([row(0)]),
    observation([row(10, Number.MAX_SAFE_INTEGER), row(11, 1)]),
    observation(Array.from({ length: 257 }, (_, i) => row(i + 1))),
    { ...observation(), completedAtMs: 101 },
  ]) assert.equal(registry.select([{ entry, observation: o }], 100, 50), undefined);
  assert.equal(registry.select([{ entry: { ...entry }, observation: observation() }], 100, 100), undefined);
  assert.equal(registry.select(Array.from({ length: 257 }, () => ({ entry, observation: observation() })), 100, 100), undefined);
});

test("protected roots come from worker-owned data; marks grant no authority", () => {
  const { registry, context } = setup();
  const entry = enroll(registry, context);
  for (const kind of ["worker", "provider", "boundary", "sink"] as const) {
    const root = registry.protect(kind, { pid: 11, startTime: 10 });
    assert.equal(registry.unprotect({ ...root }), false);
    assert.equal(registry.select([{ entry, observation: observation([row(10), row(11)]) }], 100, 200), undefined);
    assert.equal(registry.unprotect(root), true);
  }
  assert.throws(() => registry.protect("provider", { pid: -1, startTime: 10 }));
  const protectedRoot = registry.protect("provider", { pid: 10, startTime: 99 });
  assert.equal(registry.select([{ entry, observation: observation() }], 100, 200), undefined);
  registry.unprotect(protectedRoot);
  const withMarks = { ...observation([row(11)]), mark: "codex-model-command" };
  assert.equal(registry.select([{ entry, observation: withMarks }], 100, 200), undefined);
});

test("duplicate identity, overlapping trees and PID reuse disqualify affected candidates only", () => {
  const { registry, context } = setup();
  const a = enroll(registry, context, 10);
  const b = enroll(registry, context, 20);
  const c = enroll(registry, context, 30);
  for (const [rowsA, rowsB] of [
    [[row(10), row(11)], [row(20), row(11)]],
    [[row(10), row(11)], [row(20), row(11, 100, 99)]],
    [[row(10), row(20)], [row(20)]],
  ]) {
    assert.ok(rowsA);
    assert.ok(rowsB);
    assert.equal(registry.select([
      { entry: a, observation: observation(rowsA) }, { entry: b, observation: observation(rowsB) },
      { entry: c, observation: observation([row(30, 1)]) },
    ], 100, 200)?.entry, c);
  }
  assert.equal(registry.select([{ entry: a, observation: observation([row(10), row(10)]) }], 100, 200), undefined);
  assert.equal(registry.select([{ entry: a, observation: observation() }, { entry: a, observation: observation() }], 100, 200), undefined);
});

test("root replacement cannot inherit old selection; undrained old root prevents overlapping replacement", () => {
  const { registry, context } = setup();
  const old = enroll(registry, context);
  const selected = registry.select([{ entry: old, observation: observation() }], 100, 200);
  assert.ok(selected);
  assert.equal(registry.checkBeforeCancel(selected, observation([row(10, 100, 99)]), 100, 200), false);
  const replacement = registry.begin(contextInput());
  const next = enroll(registry, replacement, 10, 99);
  assert.equal(registry.checkBeforeCancel(selected, observation(), 100, 200), false);
  assert.equal(registry.select([{ entry: next, observation: observation([row(10, 100, 99)]) }], 100, 200), undefined);
  registry.recordDrain(old, { confirmed: true });
  registry.unregister(old);
  assert.equal(registry.select([{ entry: next, observation: observation([row(10, 100, 99)]) }], 100, 200)?.entry, next);
});

test("all three adapter callbacks are retained on immutable exact entries without invoking real processes", async () => {
  const { registry, context } = setup();
  const calls: string[] = [];
  const entry = registry.register(context, {
    source: "codex-model-command", toolId: "tool", attemptId: "attempt",
    root: { pid: 10, startTime: 10 }, command: "fixture",
    observe: async () => { calls.push("observe"); return observation(); },
    cancel: async () => { calls.push("cancel"); },
    drain: async () => { calls.push("drain"); return { confirmed: true }; },
  });
  registry.select([{ entry, observation: observation() }], 100, 200);
  assert.deepEqual(calls, []);
  assert.equal((await entry.observe()).complete, true);
  await entry.cancel({ authorized: () => true, remainingMs: 100, signal: new AbortController().signal });
  registry.recordDrain(entry, await entry.drain());
  assert.equal(registry.unregister(entry), true);
  assert.deepEqual(calls, ["observe", "cancel", "drain"]);
});

test("tool and attempt IDs reject controls and bidi, and scrub known secrets at enrollment", () => {
  const secret = "fixture-secret";
  const registry = new WorkerMemoryCommands([secret]);
  const context = registry.begin(contextInput());
  const input = {
    source: "codex-model-command" as const, toolId: "tool", attemptId: "attempt",
    root: { pid: 10, startTime: 10 }, command: "fixture",
    observe: async () => observation(), cancel: async () => {}, drain: async () => ({ confirmed: true }),
  };
  for (const id of ["line\nbreak", "tab\tid", "bidi\u202e", "isolate\u2066", "null\u0000", "x".repeat(257)]) {
    assert.throws(() => registry.register(context, { ...input, toolId: id }));
    assert.throws(() => registry.register(context, { ...input, attemptId: id }));
  }
  const entry = registry.register(context, { ...input, toolId: "tool-" + secret, attemptId: "attempt-" + secret });
  assert.equal(entry.toolId.includes(secret), false);
  assert.equal(entry.attemptId.includes(secret), false);
  assert.equal(entry.toolId.startsWith("tool-"), true);
  assert.equal(entry.attemptId.startsWith("attempt-"), true);
});

test("old exact-entry completion evidence leaves replacement context and latch unchanged", () => {
  const { registry, context } = setup();
  const old = enroll(registry, context);
  const next = registry.begin({ ...contextInput(), lifecycleRevision: 2, memory_policy: { version: 1, max_interventions: 2 } });
  const root = enroll(registry, next, 20);
  const latch = registry.acquireLatch(next)!;
  const evidence: Entry[] = [];
  const unsubscribe = registry.subscribeConfirmedDrain((entry) => evidence.push(entry));
  registry.recordDrain(old, { confirmed: true });
  assert.deepEqual(evidence, [old]);
  assert.equal(registry.isCurrent(next), true);
  assert.equal(registry.isRegistered(root), true);
  assert.equal(registry.remaining(next), 2);
  assert.equal(registry.admitsCommands(next), false);
  assert.equal(registry.releaseLatch(latch), true);
  unsubscribe();
});
