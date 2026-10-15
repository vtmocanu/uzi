import assert from "node:assert/strict";
import { test } from "node:test";
import { PassThrough } from "node:stream";
import path from "node:path";
import {
  CodexCallbackBroker, isBrokerModelCommand,
  type CapturedMemoryContext, type SpawnCommandSeam, type SpawnCommandOptions,
} from "../src/codex/broker.js";
import { makeDefaultSpawnCommand } from "../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { SupervisedChildExitTimeoutError, type CodexRootHandle, type ObservationOutcome } from "../src/codex/launcher.js";
import { WorkerMemoryCommands } from "../src/worker-memory-commands.js";
import { WorkerMemoryMonitor } from "../src/worker-memory-guard.js";
import type { WorkerMemoryPressureResult } from "../src/protocol.js";
import { projectToolOutput } from "../src/codex/projection.js";

const WT = path.resolve(".");
const rt = { threadId: "trusted-thread", turnId: "trusted-turn", callId: "trusted-call" };
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}
function captured(): CapturedMemoryContext {
  const commands = new WorkerMemoryCommands(["private-fixture"]);
  const flight = {};
  const context = commands.begin({
    run_id: "11111111-1111-4111-8111-111111111111", worker_id: "22222222-2222-4222-8222-222222222222",
    register_nonce: "nonce", claim_generation: 1, memory_episode: 0, flight, lifecycleRevision: 1,
  });
  return { commands, context, flight, lifecycleRevision: 1 };
}
function broker(spawnCommand: SpawnCommandSeam, registry: ExecutionRegistry, capturedMemory?: CapturedMemoryContext) {
  return new CodexCallbackBroker({
    registry, spawnCommand, capturedMemory, worktreePath: WT,
    grants: { role: "coder", phase: "implement", isRoot: true, allowedTools: new Set(["Bash"]), allowedSkills: new Set() },
    fileop: { op: async () => ({ ok: false }) }, delegate: async () => ({ ok: false }),
  });
}
const ticks = "18446744073709551615";
function fakeRoot(supervisorPid = 50, rss = 100) {
  const child = deferred<{ event: "child_exit"; code: number }>();
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  let disposes = 0;
  let observations = 0;
  let observeHook: (() => Promise<ObservationOutcome>) | undefined;
  let disposeHeld = false;
  const disposal = deferred<Awaited<ReturnType<CodexRootHandle["dispose"]>>>();
  let clean = true;
  let endOutput = true;
  let evidence: ObservationOutcome = {
    event: "observe", id: 1, state: "complete", supervisor: { pid: 50, startTime: ticks },
    root: { pid: 51, startTime: ticks }, processes: [{ pid: 51, startTime: ticks, ppid: 50, rss: 100 }],
  };
  evidence = { ...evidence, supervisor: { pid: supervisorPid, startTime: ticks },
    root: { pid: supervisorPid + 1, startTime: ticks },
    processes: [{ pid: supervisorPid + 1, startTime: ticks, ppid: supervisorPid, rss }] };
  const handle: CodexRootHandle = {
    started: { event: "started", supervisorPid, childPid: supervisorPid + 1, uid: 10003, subreaper: true,
      nondumpable: true, liveCapsZero: true, capBoundingSet: "0x0", noNewPrivs: true },
    supervisorPid, transport: { stdin: new PassThrough(), stdout, stderr },
    failed: undefined, whenFailed: new Promise(() => {}),
    snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
    observe: async () => { observations++; return observeHook ? observeHook() : evidence; },
    waitChild: async () => child.promise,
    dispose: async () => {
      disposes++;
      if (disposeHeld) return disposal.promise;
      if (endOutput) { stdout.end(); stderr.end(); }
      return clean ? { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }
        : { clean: false, reason: "fixture unconfirmed" };
    },
  };
  return { handle, child, observations: () => observations, disposes: () => disposes,
    observeWith: (hook: () => Promise<ObservationOutcome>) => { observeHook = hook; },
    hangDisposal: () => { disposeHeld = true; },
    completeDisposal: () => {
      stdout.end(); stderr.end();
      disposal.resolve({ clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } });
    },
    change: (next: ObservationOutcome) => { evidence = next; },
    unconfirmed: () => { clean = false; }, hangOutput: () => { endOutput = false; },
    endOutput: () => { stdout.destroy(); stderr.destroy(); } };
}
async function until(predicate: () => boolean) {
  // At most 200 attempts / two seconds; a failure stops this test, not sibling tests.
  for (let i = 0; i < 200; i++) {
    if (predicate()) return;
    await new Promise((done) => setTimeout(done, 10));
  }
  assert.fail("fixture readiness deadline");
}
function resultFor(entry: ReturnType<WorkerMemoryCommands["register"]>): WorkerMemoryPressureResult {
  return {
    run_id: entry.context.run_id, worker_id: entry.context.worker_id, claim_generation: entry.context.claim_generation,
    memory_episode: entry.context.memory_episode, intervention_id: "33333333-3333-4333-8333-333333333333",
    tool_id: entry.toolId, attempt_id: entry.attemptId, command_id: entry.commandId, command: entry.command,
    code: "worker_memory_pressure", observed_total_bytes: 900, limit_bytes: 1000, tree_rss_bytes_approx: 100,
    outcome: "confirmed_drained", cancellation_confirmed: true, retry_blocked: true,
    guidance: "Use smaller or serial commands after recovery.",
  };
}

test("broker mints screened runtime metadata; model args forgery and refused screening have no authority", async () => {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const seen: SpawnCommandOptions[] = [];
  const b = broker(async (_, opts) => { seen.push(opts); return { code: 0, stdout: "", stderr: "" }; }, registry);
  await b.handleToolCall(rt, "Bash", {
    command: "printf fixture", modelCommand: { ...rt, commandId: "forged" }, threadId: "forged", attemptId: "forged",
  }, "root");
  const metadata = seen[0]?.modelCommand;
  assert.ok(isBrokerModelCommand(metadata), "only screened broker metadata has membership");
  assert.equal(metadata.threadId, rt.threadId);
  assert.equal(metadata.turnId, rt.turnId);
  assert.equal(metadata.callId, rt.callId);
  assert.notEqual(metadata.attemptId, "forged");
  assert.equal(isBrokerModelCommand({ ...metadata }), false);
  const denied = await b.handleToolCall({ ...rt, callId: "denied" }, "Bash", { command: "env" }, "root");
  assert.equal(denied.ok, false);
  assert.equal(seen.length, 1, "screen refusal never reaches spawn");
});

test("broker metadata cannot enroll into a different or absent captured context", async () => {
  const target = captured();
  let launches = 0;
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const spawn = makeDefaultSpawnCommand(registry, async () => {
    launches++;
    const root = fakeRoot();
    root.child.resolve({ event: "child_exit", code: 0 });
    return root.handle;
  }, 100, WT, {}, "off", undefined, undefined, 1000, undefined, target);
  for (const source of [captured(), undefined]) {
    const b = broker(spawn, new ExecutionRegistry(newLocalExecutionEpoch(2)), source);
    const result = await b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
    assert.equal(result.ok, false, "a different or absent captured flight is refused");
  }
  assert.equal(launches, 0, "metadata minted outside the captured flight cannot start or enroll a tree");
  assert.equal(target.commands.eligibleEntries()?.length, 0);
});

test("captured-context/admission seam refuses new shells while held and after retirement", async () => {
  const c = captured();
  let spawns = 0;
  const b = broker(async () => { spawns++; return { code: 0, stdout: "", stderr: "" }; },
    new ExecutionRegistry(newLocalExecutionEpoch(1)), c);
  const latch = c.commands.acquireLatch(c.context)!;
  const held = await b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
  assert.equal(held.ok, false, "captured-context/admission refuses new commands under memory latch");
  c.commands.releaseLatch(latch);
  c.commands.retire(c.context);
  const stale = await b.handleToolCall({ ...rt, callId: "stale" }, "Bash", { command: "printf fixture" }, "root");
  assert.equal(stale.ok, false);
  assert.equal(spawns, 0, "retired captured flight cannot start a command");
});

test("retired captured flight rejects a shell without invoking its spawn seam", async () => {
  const c = captured();
  let spawns = 0;
  const b = broker(async () => { spawns++; return { code: 0, stdout: "", stderr: "" }; },
    new ExecutionRegistry(newLocalExecutionEpoch(1)), c);
  c.commands.retire(c.context);
  const result = await b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
  assert.equal(result.ok, false, "retired captured flight cannot admit a model shell");
  assert.equal(spawns, 0, "retired captured flight never reaches spawn");
});

test("trusted model command is enrolled before the executor waits for ordinary completion", async () => {
  const c = captured();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const root = fakeRoot();
  let enrollmentAtWait = -1;
  const handle: CodexRootHandle = {
    ...root.handle,
    waitChild: async () => {
      enrollmentAtWait = c.commands.eligibleEntries()?.length ?? 0;
      root.child.resolve({ event: "child_exit", code: 0 });
      return root.handle.waitChild();
    },
  };
  const b = broker(makeDefaultSpawnCommand(registry, async () => handle,
    100, WT, {}, "off", undefined, undefined, 1000, undefined, c), registry, c);
  const result = await b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
  assert.equal(result.ok, true);
  assert.equal(enrollmentAtWait, 1, "trusted model shell must acquire enrollment before ordinary completion");
  assert.equal(c.commands.eligibleEntries()?.length, 0, "confirmed cleanup retires enrollment");
});

test("enrollment seam excludes internal probes, cloned metadata and no captured context", async () => {
  const c = captured();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const root = fakeRoot();
  root.child.resolve({ event: "child_exit", code: 0 });
  const spawn = makeDefaultSpawnCommand(registry, async () => root.handle, 100, WT, {}, "off", undefined, undefined, 1000, undefined, c);
  await spawn(["/bin/true"], {});
  assert.equal(root.observations(), 0, "internal probe shares seam but never enrolls");
  assert.equal(c.commands.eligibleEntries()?.length, 0);

  let cloned: SpawnCommandOptions["modelCommand"];
  const mint = broker(async (_, opts) => {
    cloned = { ...opts.modelCommand! };
    return { code: 0, stdout: "", stderr: "" };
  }, new ExecutionRegistry(newLocalExecutionEpoch(3)), c);
  await mint.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
  const cloneRoot = fakeRoot();
  cloneRoot.child.resolve({ event: "child_exit", code: 0 });
  const cloneRegistry = new ExecutionRegistry(newLocalExecutionEpoch(4));
  await makeDefaultSpawnCommand(cloneRegistry, async () => cloneRoot.handle, 100, WT, {}, "off", undefined, undefined, 1000, undefined, c)(
    ["/bin/true"], { modelCommand: cloned });
  assert.equal(cloneRoot.observations(), 0, "cloned trusted metadata loses enrollment authority");

  const noContext = fakeRoot();
  noContext.child.resolve({ event: "child_exit", code: 0 });
  const registry2 = new ExecutionRegistry(newLocalExecutionEpoch(2));
  const b = broker(makeDefaultSpawnCommand(registry2, async () => noContext.handle, 100, WT, {}, "off"), registry2);
  await b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
  assert.equal(noContext.observations(), 0, "no trusted captured metadata means no enrollment");
});

test("enrollment and exact tooljoin survive primary exit and output cleanup race", async () => {
  const c = captured();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const root = fakeRoot();
  root.hangOutput();
  const b = broker(makeDefaultSpawnCommand(registry, async () => root.handle, 100, WT, {}, "off", undefined, undefined, 1000, undefined, c), registry, c);
  const reply = b.handleToolCall(rt, "Bash", { command: "printf private-fixture" }, "root");
  await until(() => c.commands.eligibleEntries()?.length === 1);
  const entry = c.commands.eligibleEntries()![0]!;
  assert.equal(entry.root.startTime, ticks, "enrollment preserves maximum uint64 ticks");
  assert.equal(entry.primary?.pid, 51);
  const prepared = c.commands.prepareOutcome(entry);
  root.child.resolve({ event: "child_exit", code: 7 });
  await until(() => root.disposes() === 1);
  const expected = resultFor(entry);
  assert.equal(c.commands.resolveToolOutcome(entry, expected), true);
  assert.deepEqual(await prepared, expected);
  const result = await reply;
  assert.equal(result.ok, false);
  if (result.ok) assert.fail("memory result expected");
  assert.deepEqual(result.memoryPressure, expected, "tooljoin returns the exact matching structured pressure result");
  assert.equal(result.code, "worker_memory_pressure", "capture timeout and primary failure cannot flatten memory outcome");
  assert.ok(!JSON.stringify(result).includes("private-fixture"));
  assert.deepEqual(JSON.parse(projectToolOutput(result, (s) => s)), expected);
  assert.equal(root.disposes(), 1);
  assert.equal(c.commands.eligibleEntries()?.length, 0);
  root.endOutput();
});

test("production monotonic monitor selects and cancels the enrolled adapter, leaving sibling live", async () => {
  const c = captured();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const root = fakeRoot();
  const sibling = c.commands.begin({ ...c.context, run_id: "44444444-4444-4444-8444-444444444444", flight: {} });
  const b = broker(makeDefaultSpawnCommand(registry, async () => root.handle, 100, WT, {}, "off", undefined, undefined, 1000, undefined, c), registry, c);
  const reply = b.handleToolCall(rt, "Bash", { command: "printf private-fixture" }, "root");
  await until(() => c.commands.eligibleEntries()?.length === 1);
  let reservations = 0;
  const monitor = new WorkerMemoryMonitor({
    enabled: true, reserveBytes: 200, hysteresisBytes: 100,
    sampleMs: 100, responseBudgetMs: 1000, rearmMs: 100, maxInterventions: 2,
  }, {
    commands: c.commands, now: () => performance.now(),
    incarnation: () => ({ worker_id: c.context.worker_id, register_nonce: c.context.register_nonce, revision: 1 }),
    sample: () => {
      const time = performance.now();
      const psi = { avg10: 0, avg60: 0, avg300: 0, total: 0 };
      return { available: true, sampleId: 1, startedAtMs: time, completedAtMs: time,
        currentBytes: 900, limitBytes: 1000, stat: { anon: 10, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
        pressure: { some: psi, full: psi } };
    },
    reserve: async (request) => { reservations++; return { ...request, admitted: true, authorizing: true,
      allowance: { limit: 2, used: 1, remaining: 1 } }; },
    report: async (request) => ({ ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: 1, remaining: 1 } }),
    feedback: async () => {}, preserve: async () => assert.fail("allowance remains"),
  });
  try {
    monitor.tick();
    await monitor.settle();
    // Complete the fixture even on the unfixed clock so no command promise leaks.
    root.child.resolve({ event: "child_exit", code: 0 });
    const result = await reply;
    assert.equal(reservations, 1, "monitor using performance.now must select adapter evidence");
    assert.equal(result.ok, false);
    if (result.ok) assert.fail("pressure reply expected");
    assert.equal(result.memoryPressure?.outcome, "confirmed_drained");
    assert.equal(root.disposes(), 1);
    assert.ok(c.commands.admitsCommands(sibling), "sibling retains current admission");
  } finally { await monitor.stop(); }
});

test("fresh cancellation rechecks supervisor ticks and context before disposal", async () => {
  const c = captured();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const root = fakeRoot();
  const b = broker(makeDefaultSpawnCommand(registry, async () => root.handle, 100, WT, {}, "off", undefined, undefined, 1000, undefined, c), registry, c);
  const reply = b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
  await until(() => c.commands.eligibleEntries()?.length === 1);
  const entry = c.commands.eligibleEntries()![0]!;
  root.change({ event: "observe", id: 2, state: "complete", supervisor: { pid: 50, startTime: "18446744073709551614" },
    root: { pid: 51, startTime: ticks }, processes: [{ pid: 51, startTime: ticks, ppid: 50, rss: 100 }] });
  await assert.rejects(entry.cancel({ remainingMs: 100, signal: new AbortController().signal, authorized: () => true }), /stale/);
  assert.equal(root.disposes(), 0, "PID reuse never signals an enrolled supervisor replacement");
  c.commands.retire(c.context);
  await assert.rejects(entry.cancel({ remainingMs: 100, signal: new AbortController().signal, authorized: () => true }), /stale/);
  assert.equal(root.disposes(), 0);
  root.child.resolve({ event: "child_exit", code: 0 });
  await reply;
});

test("failed or retired initial enrollment boundedly reaps the already launched root", async () => {
  for (const mode of ["unavailable", "timeout", "retired", "held", "aborted"] as const) {
    const c = captured();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const root = fakeRoot();
    const signal = new AbortController();
    root.observeWith(async () => {
      if (mode === "timeout") return new Promise(() => {});
      if (mode === "retired") c.commands.retire(c.context);
      if (mode === "held") c.commands.acquireLatch(c.context);
      if (mode === "aborted") signal.abort();
      return { event: "observe", id: 1, state: "unavailable", reason: "unreadable" };
    });
    const b = new CodexCallbackBroker({
      registry, capturedMemory: c, signal: signal.signal, worktreePath: WT,
      spawnCommand: makeDefaultSpawnCommand(registry, async () => root.handle, 30, WT, {}, "off", undefined, undefined, 1000, undefined, c),
      grants: { role: "coder", phase: "implement", isRoot: true, allowedTools: new Set(["Bash"]), allowedSkills: new Set() },
      fileop: { op: async () => ({ ok: false }) }, delegate: async () => ({ ok: false }),
    });
    const result = await b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
    assert.equal(result.ok, false, mode);
    assert.equal(root.disposes(), 1, mode);
    assert.equal(registry.hasLiveCommandRoot(), false, mode);
    assert.equal(c.commands.eligibleEntries()?.length, 0, mode);
  }
});

test("current signal and authority are revalidated after a delayed observation", async () => {
  for (const mode of ["aborted", "revoked", "retired"] as const) {
    const c = captured();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const root = fakeRoot();
    const b = broker(makeDefaultSpawnCommand(registry, async () => root.handle, 100, WT, {}, "off", undefined, undefined, 1000, undefined, c), registry, c);
    const reply = b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
    await until(() => c.commands.eligibleEntries()?.length === 1);
    const entry = c.commands.eligibleEntries()![0]!;
    const held = deferred<ObservationOutcome>();
    root.observeWith(() => held.promise);
    const signal = new AbortController();
    let authorized = true;
    const cancellation = entry.cancel({ remainingMs: 100, signal: signal.signal, authorized: () => authorized });
    if (mode === "aborted") signal.abort();
    if (mode === "revoked") authorized = false;
    if (mode === "retired") c.commands.retire(c.context);
    held.resolve({ event: "observe", id: 2, state: "complete", supervisor: entry.root,
      root: entry.primary!, processes: [{ ...entry.primary!, ppid: 50, rss: 100 }] });
    await assert.rejects(cancellation, /stale/);
    assert.equal(root.disposes(), 0, mode);
    root.child.resolve({ event: "child_exit", code: 0 });
    await reply;
  }
});

test("broker scrubs only a pressure result matching this exact callback", async () => {
  for (const matching of [false, true]) {
    const c = captured();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    let scrubs = 0;
    const b = new CodexCallbackBroker({
      registry, capturedMemory: c, worktreePath: WT,
      spawnCommand: async (_, opts) => {
        const m = opts.modelCommand!;
        const entry = c.commands.register(c.context, {
          source: "codex-model-command", toolId: m.toolId, attemptId: m.attemptId,
          root: { pid: 50, startTime: ticks }, command: m.command,
          observe: async () => { throw new Error("unused"); }, cancel: async () => {}, drain: async () => ({ confirmed: false }),
        }, m.commandId);
        return { code: 137, stdout: "", stderr: "", memoryPressure: {
          ...resultFor(entry), command_id: matching ? entry.commandId : "55555555-5555-4555-8555-555555555555",
        } };
      },
      scrubToolResult: (result) => { scrubs++; return { ...result, command: "scrubbed callback" }; },
      grants: { role: "coder", phase: "implement", isRoot: true, allowedTools: new Set(["Bash"]), allowedSkills: new Set() },
      fileop: { op: async () => ({ ok: false }) }, delegate: async () => ({ ok: false }),
    });
    const result = await b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
    assert.equal(result.ok, false);
    if (result.ok) assert.fail("pressure denial expected");
    assert.equal(result.code, matching ? "worker_memory_pressure" : "memory_result_mismatch");
    assert.equal(scrubs, matching ? 1 : 0);
    assert.equal(result.memoryPressure?.command, matching ? "scrubbed callback" : undefined);
  }
});

test("disposal and prepared-outcome waits are bounded and uncertain roots remain retained", async () => {
  for (const mode of ["disposal", "outcome"] as const) {
    const c = captured();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const root = fakeRoot();
    if (mode === "disposal") root.hangDisposal();
    const b = broker(makeDefaultSpawnCommand(registry, async () => root.handle, 30, WT, {}, "off", undefined, undefined, 1000, undefined, c), registry, c);
    const reply = b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
    await until(() => c.commands.eligibleEntries()?.length === 1);
    const entry = c.commands.eligibleEntries()![0]!;
    if (mode === "outcome") c.commands.prepareOutcome(entry);
    root.child.resolve({ event: "child_exit", code: 0 });
    const result = await reply;
    assert.equal(result.ok, false, mode);
    assert.equal(root.disposes(), 1);
    if (mode === "disposal") {
      assert.equal(c.commands.hasConfirmedStop(entry), false);
      assert.equal(c.commands.unregister(entry), false);
      assert.equal(registry.hasLiveCommandRoot(), true);
    }
    root.endOutput();
  }
});

test("unclean disposal retains root and exact memory outcome rather than generic failure", async () => {
  const c = captured();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const root = fakeRoot();
  root.unconfirmed();
  const b = broker(makeDefaultSpawnCommand(registry, async () => root.handle, 100, WT, {}, "off", undefined, undefined, 1000, undefined, c), registry, c);
  const reply = b.handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
  await until(() => c.commands.eligibleEntries()?.length === 1);
  const entry = c.commands.eligibleEntries()![0]!;
  c.commands.prepareOutcome(entry);
  c.commands.resolveToolOutcome(entry, { ...resultFor(entry), cancellation_confirmed: false, outcome: "unknown" });
  assert.equal(root.disposes(), 0);
  root.child.resolve({ event: "child_exit", code: 0 });
  const result = await reply;
  assert.equal(result.ok, false);
  if (result.ok) assert.fail("memory result expected");
  assert.equal(result.memoryPressure?.outcome, "unknown");
  assert.equal(c.commands.hasConfirmedStop(entry), false);
  assert.equal(c.commands.unregister(entry), false, "unconfirmed root remains retained");
  assert.equal(registry.hasLiveCommandRoot(), true);
});

test("pressure feedback never grants ordinary disposal for denied revoked or retired monitor authority", async () => {
  for (const mode of ["denied", "revoked", "retired"] as const) {
    const c = captured();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const root = fakeRoot();
    const sibling = fakeRoot(60, 1);
    const siblingContext = c.commands.begin({ ...c.context, run_id: "44444444-4444-4444-8444-444444444444", flight: {} });
    const siblingCapture = { ...c, context: siblingContext, flight: siblingContext.flight };
    const siblingRegistry = new ExecutionRegistry(newLocalExecutionEpoch(2));
    const siblingReply = broker(makeDefaultSpawnCommand(siblingRegistry, async () => sibling.handle, 100, WT, {}, "off",
      undefined, undefined, 1000, undefined, siblingCapture), siblingRegistry, siblingCapture)
      .handleToolCall({ ...rt, callId: "sibling" }, "Bash", { command: "printf sibling" }, "root");
    const reply = broker(makeDefaultSpawnCommand(registry, async () => root.handle, 100, WT, {}, "off",
      undefined, undefined, 1000, undefined, c), registry, c)
      .handleToolCall({ ...rt, callId: "call-private-fixture" }, "Bash", { command: "printf private-fixture" }, "root");
    await until(() => c.commands.eligibleEntries()?.length === 2);
    let revision = 1;
    let feedback: WorkerMemoryPressureResult | undefined;
    const monitor = new WorkerMemoryMonitor({
      enabled: true, reserveBytes: 200, hysteresisBytes: 100, sampleMs: 100,
      responseBudgetMs: 1000, rearmMs: 100, maxInterventions: 2,
    }, {
      commands: c.commands, now: () => performance.now(),
      incarnation: () => ({ worker_id: c.context.worker_id, register_nonce: c.context.register_nonce, revision }),
      sample: () => {
        const time = performance.now();
        const psi = { avg10: 0, avg60: 0, avg300: 0, total: 0 };
        return { available: true, sampleId: 1, startedAtMs: time, completedAtMs: time,
          currentBytes: 900, limitBytes: 1000, stat: { anon: 10, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
          pressure: { some: psi, full: psi } };
      },
      reserve: async (request) => {
        if (mode === "revoked") revision++;
        if (mode === "retired") c.commands.retire(c.context);
        return { ...request, admitted: mode !== "denied", authorizing: mode !== "denied",
          allowance: { limit: 2, used: mode === "denied" ? 2 : 1, remaining: mode === "denied" ? 0 : 1 } };
      },
      report: async (request) => ({ ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
        allowance: { limit: 2, used: 1, remaining: 1 } }),
      feedback: async (result) => { feedback = result; }, preserve: async () => {},
    });
    try {
      monitor.tick();
      await monitor.settle();
      assert.equal(feedback?.outcome, "no_signal", mode);
      await new Promise((done) => setImmediate(done));
      assert.equal(root.disposes(), 0, mode);
      assert.equal(registry.hasLiveCommandRoot(), true, mode);
      assert.equal(sibling.disposes(), 0, "unselected sibling survives");
      root.child.resolve({ event: "child_exit", code: 0 });
      const result = await reply;
      assert.equal(result.ok, false);
      if (result.ok) assert.fail("pressure feedback expected");
      assert.equal(result.code, "worker_memory_pressure", "original callback accepts redacted display IDs");
      assert.deepEqual(result.memoryPressure, feedback);
      assert.ok(!JSON.stringify(result).includes("private-fixture"));
      assert.ok(!projectToolOutput(result, (s) => s).includes("private-fixture"));
      assert.equal(root.disposes(), 1, "ordinary primary exit permits cleanup");
      assert.equal(sibling.disposes(), 0);
    } finally {
      root.child.resolve({ event: "child_exit", code: 0 });
      sibling.child.resolve({ event: "child_exit", code: 0 });
      await Promise.all([reply, siblingReply, monitor.stop()]);
    }
  }
});

test("timed out owned reap reconciles late clean completion without reviving its epoch or losing historical outcome", async () => {
  const c = captured();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const unaffected = new ExecutionRegistry(newLocalExecutionEpoch(2));
  const root = fakeRoot();
  root.hangDisposal();
  const reply = broker(makeDefaultSpawnCommand(registry, async () => root.handle, 30, WT, {}, "off",
    undefined, undefined, 1000, undefined, c), registry, c)
    .handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
  await until(() => c.commands.eligibleEntries()?.length === 1);
  const entry = c.commands.eligibleEntries()![0]!;
  const historical = c.commands.prepareOutcome(entry);
  const expected = { ...resultFor(entry), outcome: "unknown" as const, cancellation_confirmed: false };
  c.commands.resolveToolOutcome(entry, expected);
  root.child.resolve({ event: "child_exit", code: 0 });
  const result = await reply;
  assert.equal(result.ok, false);
  if (result.ok) assert.fail("pressure result expected");
  assert.deepEqual(result.memoryPressure, expected);
  assert.equal(registry.isPoisoned(), true);
  assert.equal(registry.reserveLaunch("command").kind, "denied");
  assert.equal(unaffected.reserveLaunch("command").kind, "reserved");
  assert.equal(registry.hasLiveCommandRoot(), true);
  assert.equal(c.commands.hasConfirmedStop(entry), false);
  const successor = c.commands.begin({ ...c.context, memory_episode: 1, flight: {} });
  const latch = c.commands.acquireLatch(successor)!;
  root.completeDisposal();
  await until(() => !registry.hasLiveCommandRoot());
  assert.equal(c.commands.hasConfirmedStop(entry), true);
  assert.equal(c.commands.recordDrain(entry, { confirmed: false }), false, "exact original entry was removed");
  assert.equal(c.commands.isCurrent(successor), true);
  assert.equal(c.commands.admitsCommands(successor), false, "late completion cannot change successor latch");
  c.commands.releaseLatch(latch);
  assert.deepEqual(await historical, expected);
  assert.equal(registry.isPoisoned(), true);
  assert.equal(registry.reserveLaunch("command").kind, "denied");
  // Retired entries count toward the observation cap. Fill it to prove the old entry is gone.
  for (let i = 0; i < 256; i++) c.commands.register(successor, {
    source: "codex-model-command", toolId: "fill-" + i, attemptId: "attempt", command: "fixture",
    root: { pid: 100 + i, startTime: "10" }, observe: async () => { throw new Error("unused"); },
    cancel: async () => {}, drain: async () => ({ confirmed: false }),
  });
  assert.equal(c.commands.eligibleEntries()?.length, 256);
});

test("full adapter refuses a mismatched callback result after private ID binding", async () => {
  const c = captured();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const root = fakeRoot();
  const spawn = makeDefaultSpawnCommand(registry, async () => root.handle, 100, WT, {}, "off",
    undefined, undefined, 1000, undefined, c);
  const reply = broker(async (argv, opts) => {
    const result = await spawn(argv, opts);
    return { ...result, memoryPressure: result.memoryPressure ? { ...result.memoryPressure, tool_id: "other-callback" } : undefined };
  }, registry, c).handleToolCall({ ...rt, callId: "call-private-fixture" }, "Bash", { command: "printf fixture" }, "root");
  await until(() => c.commands.eligibleEntries()?.length === 1);
  const entry = c.commands.eligibleEntries()![0]!;
  c.commands.prepareOutcome(entry);
  c.commands.resolveToolOutcome(entry, resultFor(entry));
  root.child.resolve({ event: "child_exit", code: 0 });
  const result = await reply;
  assert.equal(result.ok, false);
  if (result.ok) assert.fail("mismatch expected");
  assert.equal(result.code, "memory_result_mismatch");
  assert.equal(result.memoryPressure, undefined);
  assert.ok(!JSON.stringify(result).includes("private-fixture"));
});


test("plain command timeout closes registry admission without captured memory context", async () => {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const root = fakeRoot();
  root.hangDisposal();
  root.child.resolve({ event: "child_exit", code: 0 });
  try {
    const reply = await broker(makeDefaultSpawnCommand(registry, async () => root.handle, 30, WT, {}, "off"),
      registry).handleToolCall(rt, "Bash", { command: "printf fixture" }, "root");
    assert.equal(reply.ok, false);
    assert.equal(registry.hasLiveCommandRoot(), true);
    assert.equal(registry.isPoisoned(), true, "uncertain cleanup must close admission without an adapter");
    assert.equal(registry.reserveLaunch("command").kind, "denied");
  } finally {
    root.completeDisposal();
    await until(() => !registry.hasLiveCommandRoot());
  }
  assert.equal(registry.isPoisoned(), true, "late ordinary cleanup cannot reopen admission");
});


test("long_running selected victim retains pressure outcome when effective deadline becomes ready during disposal and outcome join", async () => {
  const c = captured();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const root = fakeRoot();
  root.hangDisposal();
  let waitMs: number | undefined;
  let expire!: () => void;
  const timed = new Promise<never>((_resolve, reject) => { expire = () => reject(new SupervisedChildExitTimeoutError(7000)); });
  const handle = { ...root.handle, waitChild: (ms?: number) => { waitMs = ms; return timed; } };
  const reply = broker(makeDefaultSpawnCommand(registry, async () => handle, 1000, WT, {}, "off",
    undefined, undefined, 1000, 7000, c), registry, c)
    .handleToolCall(rt, "Bash", { command: "printf fixture", long_running: true }, "root");
  await until(() => c.commands.eligibleEntries()?.length === 1 && waitMs !== undefined);
  const entry = c.commands.eligibleEntries()![0]!;
  let expected: WorkerMemoryPressureResult | undefined;
  const resolveOutcome = c.commands.resolveToolOutcome.bind(c.commands);
  c.commands.resolveToolOutcome = (selected, result) => {
    assert.equal(selected, entry); expected = result; return true;
  };
  const monitor = new WorkerMemoryMonitor({
    enabled: true, reserveBytes: 200, hysteresisBytes: 100, sampleMs: 100,
    responseBudgetMs: 1000, rearmMs: 100, maxInterventions: 2,
  }, {
    commands: c.commands, now: () => performance.now(),
    incarnation: () => ({ worker_id: c.context.worker_id, register_nonce: c.context.register_nonce, revision: 1 }),
    sample: () => {
      const now = performance.now();
      const psi = { avg10: 0, avg60: 0, avg300: 0, total: 0 };
      return { available: true, sampleId: 1, startedAtMs: now, completedAtMs: now,
        currentBytes: 900, limitBytes: 1000, stat: { anon: 10, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
        pressure: { some: psi, full: psi } };
    },
    reserve: async request => ({ ...request, admitted: true, authorizing: true,
      allowance: { limit: 2, used: 1, remaining: 1 } }),
    report: async request => ({ ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: 1, remaining: 1 } }),
    feedback: async () => {}, preserve: async () => assert.fail("allowance remains"),
  });
  try {
    monitor.tick();
    await until(() => root.disposes() === 1);
    assert.equal(waitMs, 7000, "opt-in effective bound reaches the supervisor");
    expire();
    await new Promise(resolve => setImmediate(resolve));
    root.completeDisposal();
    await until(() => expected !== undefined);
    // The deadline is already ready while ordinary cleanup joins the prepared result.
    assert.ok(c.commands.outcome(entry));
    assert.equal(resolveOutcome(entry, expected!), true);
    const result = await reply;
    assert.equal(result.ok, false);
    if (result.ok) assert.fail("pressure result required");
    assert.equal(result.code, "worker_memory_pressure");
    assert.deepEqual(result.memoryPressure, expected);
    assert.equal(result.memoryPressure?.outcome, "confirmed_drained");
    assert.equal(root.disposes(), 1);
    await monitor.settle();
  } finally {
    root.completeDisposal(); expire(); await monitor.stop(); await reply;
  }
});
