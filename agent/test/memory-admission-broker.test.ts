import assert from "node:assert/strict";
import { test } from "node:test";
import { PassThrough } from "node:stream";
import { CodexCallbackBroker, CommandDeadlineError, type SpawnCommandOptions } from "../src/codex/broker.js";
import { makeDefaultSpawnCommand } from "../src/codex/codex-executor.js";
import type { CodexRootHandle } from "../src/codex/launcher.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { WorkerMemoryCommands } from "../src/worker-memory-commands.js";

const tick = () => new Promise<void>(resolve => setImmediate(resolve));
function fixture(signal?: AbortSignal, commandWallMs = 1000) {
  const commands = new WorkerMemoryCommands([], { rearmMs: 100, sampleMs: 10, responseBudgetMs: 10 });
  const flight = {};
  const context = commands.begin({
    run_id: "11111111-1111-4111-8111-111111111111", worker_id: "22222222-2222-4222-8222-222222222222",
    register_nonce: "fixture", claim_generation: 1, memory_episode: 1, flight, lifecycleRevision: 1,
    memory_policy: { version: 1, max_interventions: 3 }, memory_intervention_count: 1,
  });
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const allowedTools = new Set(["Bash"]);
  const secretPaths: string[] = [];
  const calls: SpawnCommandOptions[] = [];
  const broker = new CodexCallbackBroker({
    registry, capturedMemory: { commands, context, flight, lifecycleRevision: 1 },
    grants: { role: "lead", phase: "implement", isRoot: true, allowedTools, allowedSkills: new Set() },
    worktreePath: "/tmp/memory-admission-fixture", signal, commandWallMs,
    screenPolicy: { extraSecretPaths: secretPaths },
    spawnCommand: async (_argv, opts) => { calls.push(opts); return { code: 0, stdout: "fixture", stderr: "" }; },
    fileop: { op: async () => ({ ok: true }) }, delegate: async () => ({ ok: true, output: {} }),
  });
  const call = (command = "echo fixture", extra: Record<string, unknown> = {}) => broker.handleToolCall(
    { threadId: "t", turnId: "u", callId: "c" }, "Bash", { command, ...extra }, "root");
  return { commands, context, registry, allowedTools, secretPaths, calls, call };
}

test("#3436 Codex default spawn spends only the original deadline after startup", async t => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 1000 });
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(2));
  const waits: number[] = [];
  let startupDeadline: number | undefined;
  const launch = async (_spec: unknown, deadline?: number): Promise<CodexRootHandle> => {
    startupDeadline = deadline;
    t.mock.timers.tick(25);
    const stdout = new PassThrough(), stderr = new PassThrough();
    return {
      started: { event: "started", supervisorPid: 20, childPid: 21, subreaper: true, nondumpable: true,
        uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
      supervisorPid: 20, transport: { stdin: new PassThrough(), stdout, stderr },
      snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
      observe: async () => ({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" }),
      waitChild: async ms => { waits.push(ms!); stdout.end(); stderr.end(); return { event: "child_exit", code: 0 }; },
      dispose: async () => ({ clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }),
      failed: undefined, whenFailed: new Promise(() => {}),
    };
  };
  const spawn = makeDefaultSpawnCommand(registry, launch, 1000, "/tmp/memory-admission-fixture", {}, "required", undefined, undefined, 1000);
  assert.equal((await spawn(["/bin/sh", "-c", "echo fixture"], { deadlineAt: 1060 })).code, 0);
  assert.equal(startupDeadline, 60);
  assert.deepEqual(waits, [35]);
  assert.equal(registry.hasLiveCommandRoot(), false);
});

test("#3436 Codex default spawn refuses an expired original deadline without a launch", async t => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 1000 });
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(3));
  let launches = 0;
  const spawn = makeDefaultSpawnCommand(registry, async () => { launches++; throw new Error("unexpected launch"); },
    1000, "/tmp/memory-admission-fixture", {}, "required");
  await assert.rejects(spawn(["/bin/sh", "-c", "echo fixture"], { deadlineAt: 1000 }),
    error => error instanceof CommandDeadlineError && error.launched === false);
  assert.equal(launches, 0);
});

test("#3436 Codex reports retired, exhausted and phase-held admission without launching", async () => {
  for (const reason of ["retired", "exhausted", "phase-held"] as const) {
    const h = fixture();
    if (reason === "retired") h.commands.retire(h.context);
    if (reason === "exhausted") h.commands.updateAllowance(h.context, 0);
    if (reason === "phase-held") h.commands.holdPhase(h.context);
    const result = await h.call();
    assert.equal(result.ok, false);
    assert.match(JSON.stringify(result), new RegExp(reason));
    assert.equal(h.calls.length, 0);
  }
});

test("#3436 Codex pressure wait wakes on release and keeps the original command deadline", async t => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 1000 });
  const h = fixture();
  const latch = h.commands.acquireLatch(h.context)!;
  let settled = false;
  const work = h.call("echo fixture", { deadlineAt: 999999, commandWallMs: 999999 }).finally(() => { settled = true; });
  await tick();
  assert.equal(settled, false, "pressure is temporary, so the callback waits");
  assert.equal(h.calls.length, 0);
  t.mock.timers.tick(50);
  h.commands.releaseLatch(latch);
  assert.equal((await work).ok, true);
  assert.equal(h.calls.length, 1);
  assert.equal(h.calls[0]!.deadlineAt, 2000, "waiting must not restart the command clock");
});

test("#3436 Codex still-held pressure returns a reason at the bounded wait limit", async t => {
  t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 1000 });
  const h = fixture();
  h.commands.acquireLatch(h.context);
  const work = h.call();
  await tick();
  t.mock.timers.tick(120);
  const result = await work;
  assert.equal(result.ok, false);
  assert.match(JSON.stringify(result), /pressure-held/);
  assert.match(JSON.stringify(result), /0\.1 s/);
  assert.match(JSON.stringify(result), /remaining: 2/);
  assert.equal(h.calls.length, 0);
  assert.equal(h.commands.pressureHeld(h.context), true, "waiting cannot release the guard latch");
});

for (const interruption of ["retire", "successor", "phase", "exhaust", "cancel", "deadline", "grant", "path", "epoch",
  "phase-after-release", "cancel-after-release", "deadline-after-release", "successor-after-release", "pressure-again"] as const) {
  test("#3436 Codex launches nothing after waiting and " + interruption, async t => {
    t.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 1000 });
    const abort = new AbortController();
    const h = fixture(abort.signal, interruption === "deadline" ? 50 : 1000);
    const latch = h.commands.acquireLatch(h.context)!;
    const work = h.call(interruption === "path" ? "cat /tmp/memory-admission-fixture/private" : undefined);
    await tick();
    if (interruption === "retire") h.commands.retire(h.context);
    if (interruption === "successor") h.commands.begin({ ...h.context, claim_generation: 2, flight: {} });
    if (interruption === "phase") { const phase = h.commands.holdPhase(h.context)!; h.commands.releasePhase(phase); }
    if (interruption === "exhaust") h.commands.updateAllowance(h.context, 0);
    if (interruption === "cancel") abort.abort();
    if (interruption === "deadline") t.mock.timers.tick(50);
    if (interruption === "grant") h.allowedTools.delete("Bash");
    if (interruption === "path") h.secretPaths.push("/tmp/memory-admission-fixture/private");
    if (interruption === "epoch") h.registry.poison({ category: "protocol", message: "fixture epoch closed" });
    if (interruption.endsWith("after-release") || interruption === "pressure-again") {
      h.commands.releaseLatch(latch);
      if (interruption === "phase-after-release") { const phase = h.commands.holdPhase(h.context)!; h.commands.releasePhase(phase); }
      if (interruption === "cancel-after-release") abort.abort();
      if (interruption === "deadline-after-release") t.mock.timers.tick(1000);
      if (interruption === "successor-after-release") h.commands.begin({ ...h.context, claim_generation: 2, flight: {} });
      if (interruption === "pressure-again") h.commands.acquireLatch(h.context);
    }
    h.commands.releaseLatch(latch);
    assert.equal((await work).ok, false);
    assert.equal(h.calls.length, 0, interruption);
  });
}
