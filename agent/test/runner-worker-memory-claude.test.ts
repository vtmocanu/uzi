import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { performance } from "node:perf_hooks";
import type { Executor } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// Issue #2471 M3: a guard-bound runner enrols Claude claims (no Codex secret) too.

for (const outcome of ["false", "throw", "confirmed"] as const) {
  test(`capture policy: persisted Claude memory exhaustion / ${outcome}`, { timeout: 60_000 }, async t => {
    const mask = process.umask(0o022);
    t.after(() => { process.umask(mask); });
    const f = await setup(30_000);
    f.claim.memory_intervention_count = 2;
    let entries = 0, settlements = 0, preservationSettlements = 0, captures = 0, publications = 0;
    let flight: { sinkGate: { isHeld(): boolean };
      workerMemory?: { operation?: Promise<void>; settled?: boolean } } | undefined;
    const begin = f.memory.begin.bind(f.memory);
    t.mock.method(f.memory, "begin", (...args: Parameters<typeof begin>) => {
      const context = begin(...args);
      flight = context.flight as typeof flight;
      return context;
    });
    let clone = "", accessorReached = false;
    let callbackFailure: unknown;
    const session = path.join(homeDir, f.claim.run_id);
    const marker = git.commitWipMarker.bind(git);
    t.mock.method(git, "commitWipMarker", async (...args: Parameters<typeof marker>) => {
      captures++; return marker(...args);
    });
    const publish = client.publishCheckpoint.bind(client);
    t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof publish>) => {
      publications++; return publish(...args);
    });
    const executor: Executor = {
      run: async ctx => {
        entries++;
        clone = ctx.worktreePath;
        fs.mkdirSync(session, { recursive: true, mode: 0o700 });
        fs.writeFileSync(path.join(session, "session"), "original session", { mode: 0o600 });
        fs.writeFileSync(path.join(clone, "ONLY_COPY.txt"), "memory work");
        api.setOwnershipStatus(f.claim.run_id, "running", 7);
        try {
          assert.equal(typeof ctx.capturedMemory, "function");
          assert.throws(() => ctx.capturedMemory!(), /retained recovery/i);
          accessorReached = true;
        } catch (error) { callbackFailure = error; throw error; }
        throw new Error("original execution stopped after exhausted accessor");
      },
      settleWorkerCommands: async () => {
        if (!accessorReached) return { confirmed: true };
        settlements++;
        if (flight?.workerMemory?.operation && !flight.workerMemory.settled && flight.sinkGate.isHeld()) {
          preservationSettlements++;
        }
        if (outcome === "throw") throw new Error("memory command protocol failure");
        return { confirmed: outcome === "confirmed" };
      },
    };
    const runner = runnerWith(() => ({ executor, homeDir: session }), fakeGitlab().gitlab,
      undefined, nullLogger(), { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 30_000, checkpointIntervalMs: 0 });
    try {
      assert.equal(f.claim.secrets.codex, undefined);
      await runner.execute(f.claim);
      if (callbackFailure) throw callbackFailure;
      assert.equal(entries, 1);
      assert.equal(accessorReached, true, "real persisted-exhaustion accessor created the preservation hold");
      assert.ok(settlements > 0);
      assert.ok(preservationSettlements > 0, "the pending preservation operation reached settlement inside the real sink gate");
      assert.equal(api.states.some(s => ["failed", "completed"].includes(s.body.status)), false);
      const parks = api.states.filter(s => s.body.recovery_cause === "worker_memory_pressure");
      if (outcome === "confirmed") {
        assert.ok(captures > 0 && publications > 0, "confirmed control reaches preservation capture/publication");
        assert.equal(parks.length, 1);
      } else {
        assert.deepEqual({ captures, publications }, { captures: 0, publications: 0 });
        assert.equal(parks.length, 0);
        assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
        assert.equal(fs.readFileSync(path.join(clone, "ONLY_COPY.txt"), "utf8"), "memory work");
        assert.equal(fs.readFileSync(path.join(session, "session"), "utf8"), "original session");
      }
    } finally { runner.shutdown(); await f.memory.stop(); process.umask(mask); }
  });
}

const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };

async function setup(responseBudgetMs = settings.responseBudgetMs) {
  api.memoryRegisterNonce = "claude-memory-registration";
  api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence"]);
  await client.register("memory-claude-fixture", "base");
  // A Claude claim: the Codex secret is absent, the memory fields are the guard's.
  const claim: ClaimResponse = { ...gitlabClaim(2471), claim_generation: 7,
    memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: 0 };
  api.setOwnershipStatus(claim.run_id, "running", 7);
  const memory = new WorkerMemoryRuntime({ ...settings, responseBudgetMs }, client, nullLogger(), {
    reader: { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: 1, startedAtMs: time, completedAtMs: time,
        currentBytes: 0, limitBytes: 1000,
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
          full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
  return { memory, claim };
}

test("a Claude claim on a guard-bound runner is enrolled; a missing memory field is refused before the executor", async () => {
  const f = await setup();
  let calls = 0;
  let accessor: unknown, secrets: unknown, subscribe: unknown;
  const executor: Executor = { run: async ctx => {
    calls++;
    accessor = ctx.capturedMemory;
    secrets = ctx.guardedCommandSecrets;
    subscribe = ctx.onMemoryAuthorityLost;
    throw new Error("fixture end");
  } };
  const r = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  try {
    assert.equal(f.claim.secrets.codex, undefined, "the fixture is a Claude claim");
    // Red: the pre-change gate returned early for a Claude claim and ran it unenrolled; an
    // incomplete memory field must now stop it before executor admission.
    for (const bad of [{ memory_policy: undefined }, { memory_policy: { version: 1 as const, max_interventions: 0 } },
      { memory_episode: undefined }]) {
      await r.execute({ ...f.claim, ...bad });
      assert.equal(calls, 0, "an incomplete Claude claim must stop before executor admission");
    }
    // Green: the complete claim reaches the executor with the guard's seams.
    await r.execute(f.claim);
    assert.equal(calls, 1);
    assert.equal(typeof accessor, "function");
    assert.equal(typeof subscribe, "function");
    assert.ok(Array.isArray(secrets) && secrets.includes(f.claim.secrets.forge_pat), "the run's redaction secrets ride the ctx");
  } finally { await f.memory.stop(); }
});

test("a runner without a memory runtime leaves a Claude claim unenrolled", async () => {
  const f = await setup();
  let accessor: unknown = "unset", subscribe: unknown = "unset", secrets: unknown = "unset";
  const executor: Executor = { run: async ctx => {
    accessor = ctx.capturedMemory;
    subscribe = ctx.onMemoryAuthorityLost;
    secrets = ctx.guardedCommandSecrets;
    throw new Error("fixture end");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab).execute(f.claim);
    assert.equal(accessor, undefined);
    assert.equal(subscribe, undefined);
    assert.equal(secrets, undefined);
  } finally { await f.memory.stop(); }
});

test("only the memory invalidation tells the executor; cancellation retires memory without telling it", async () => {
  const f = await setup();
  const monitoring = f.memory.run();
  const told: Error[] = [];
  let afterRetire = -1;
  let afterUnsubscribe = -1;
  const executor: Executor = { run: async ctx => {
    const off = ctx.onMemoryAuthorityLost!(stop => { told.push(stop); });
    const captured = ctx.capturedMemory!()!;
    // A flight cancel retires the memory context through its own listener: no invalidation.
    (captured.flight as { cancel: AbortController }).cancel.abort();
    await new Promise(resolve => setTimeout(resolve, 10));
    afterRetire = told.length;
    client.revokeMemoryAuthority();
    const afterRevoke = told.length;
    off();
    client.revokeMemoryAuthority();
    afterUnsubscribe = told.length;
    assert.equal(afterRevoke, 1);
    throw new Error("fixture end");
  } };
  const r = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  try {
    await r.execute(f.claim);
    assert.equal(afterRetire, 0, "retirement alone must not tell the executor");
    assert.equal(told.length, 1, "the invalidation told it exactly once");
    assert.equal(afterUnsubscribe, 1, "an unsubscribed listener hears nothing more");
    assert.equal(told[0]!.constructor.name, "RetainedRecoveryStop");
    assert.match(told[0]!.message, /retained recovery stopped/);
  } finally { await f.memory.stop(); await monitoring.catch(() => undefined); }
});

test("credential capture joins summary and confirms guarded commands before source access with the remaining budget", async () => {
  const f = await setup();
  const r = runnerWith(() => ({ executor: {} as Executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  const owner = r as unknown as {
    runFreeCapture(claim: ClaimResponse, flight: unknown, deadlineAt: number, signal: undefined,
      action: () => Promise<string>): Promise<{ kind: string; value?: string }>;
  };
  let sourceCalls = 0;
  const settle = (executor: Executor, deadlineAt = Date.now() + 500) => owner.runFreeCapture(f.claim, {
    executor, survivingTickGroups: [], sinkGate: { run: (action: () => Promise<unknown>) => action() },
  }, deadlineAt, undefined, async () => { sourceCalls++; return "source"; });
  try {
    let deadlines: number[] = [];
    const unconfirmed: Executor = { run: async () => { throw new Error("unused"); },
      settleWorkerCommands: async deadline => { deadlines.push(deadline); return { confirmed: false }; } };
    assert.equal((await settle(unconfirmed)).kind, "incomplete");
    assert.equal(sourceCalls, 0);
    assert.equal(deadlines.length, 1);
    assert.ok(deadlines[0]! > 0 && deadlines[0]! <= 500);
    const throwing: Executor = { run: async () => { throw new Error("unused"); },
      settleWorkerCommands: async () => { throw new Error("boom"); } };
    assert.equal((await settle(throwing)).kind, "incomplete");
    assert.equal(sourceCalls, 0);

    let releaseSummary!: () => void, releaseCommands!: () => void;
    let commandsEntered!: () => void;
    const summary = new Promise<void>(resolve => { releaseSummary = resolve; });
    const commands = new Promise<void>(resolve => { releaseCommands = resolve; });
    const entered = new Promise<void>(resolve => { commandsEntered = resolve; });
    const clock = Date.now;
    const original = clock();
    let now = original;
    let summaryBudget = 0;
    const confirmed: Executor = { run: async () => { throw new Error("unused"); },
      closeAndSettlePlanSummary: async budget => { summaryBudget = budget!; await summary; },
      settleWorkerCommands: async deadline => { deadlines.push(deadline); commandsEntered(); await commands; return { confirmed: true }; } };
    deadlines = [];
    Date.now = () => now;
    try {
      const pending = settle(confirmed, original + 500);
      await Promise.resolve();
      assert.equal(sourceCalls, 0, "pending summary prevents source work");
      now += 125;
      releaseSummary();
      await entered;
      assert.equal(summaryBudget, 500);
      assert.deepEqual(deadlines, [375], "command settle consumes the original remaining budget");
      assert.equal(sourceCalls, 0, "pending commands prevent source work");
      releaseCommands();
      assert.deepEqual(await pending, { kind: "observed_empty", value: "source" });
    } finally { Date.now = clock; releaseSummary(); releaseCommands(); }
    assert.equal(sourceCalls, 1);
    assert.equal((await settle({ run: async () => { throw new Error("unused"); } })).kind, "observed_empty");
    assert.equal(sourceCalls, 2);
  } finally { await f.memory.stop(); }
});

/** Run a Claude claim whose executor is told the memory invalidation and ends with `ending(stop)`. */
async function invalidatedRun(ending: (stop: Error) => Error) {
  const f = await setup();
  const monitoring = f.memory.run();
  let told: Error | undefined;
  const executor: Executor = { run: async ctx => {
    ctx.onMemoryAuthorityLost!(stop => { told = stop; });
    ctx.capturedMemory!();
    client.revokeMemoryAuthority();
    // Asserted outside the run: an assertion thrown here would itself report `failed`, making the control vacuous.
    throw ending(told ?? new Error("invalidation not delivered"));
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory }).execute(f.claim);
    return { failed: api.states.some(s => s.body.status === "failed"), told: told !== undefined };
  } finally { await f.memory.stop(); await monitoring.catch(() => undefined); }
}

test("a Claude executor that rejects with the carried retained stop lands in the retained-custody arm, not a failure", async () => {
  // The carrier rethrows exactly the stop the runner delivered.
  const r = await invalidatedRun(stop => stop);
  assert.ok(r.told, "the invalidation reached the executor");
  assert.equal(r.failed, false, "the retained stop reports no model failure");
});

test("a Claude executor that rebuilds the error after the invalidation is reported as a failure (the control)", async () => {
  const r = await invalidatedRun(() => new Error("rebuilt after the invalidation"));
  assert.ok(r.told, "the invalidation reached the executor");
  assert.equal(r.failed, true);
});
