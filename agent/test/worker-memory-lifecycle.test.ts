import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs/promises";
import path from "node:path";
import { performance } from "node:perf_hooks";
import { Worker } from "../src/worker.js";
import { WorkerClient } from "../src/client.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import type { WorkerMemoryReader } from "../src/worker-memory-reader.js";
import { Outbox } from "../src/outbox.js";
import { StatsCollector } from "../src/stats.js";
import type { Config } from "../src/config.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { IsolatedRunner } from "../src/isolated-runner.js";
import { nullLogger } from "./helpers.js";

const workerId = "22222222-2222-4222-8222-222222222222";
const runId = "11111111-1111-4111-8111-111111111111";
const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 100, rearmMs: 10, maxInterventions: 2 };
function config(memoryGuard: Config["memoryGuard"] = settings): Config {
  return { memoryGuard, workerName: "fixture", workerTemplate: "base", pollIntervalMs: 1,
    heartbeatIntervalMs: 2, chatPollMs: 2, maxConcurrentRuns: 1, chatSessions: 1,
    dockerWiring: {}, dataDir: ".", gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20 } as Config;
}
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => { resolve = done; });
  return { promise, resolve };
}
async function until(predicate: () => boolean) {
  const end = performance.now() + 2000;
  while (!predicate()) {
    assert.ok(performance.now() < end, "bounded fixture wait");
    await new Promise((resolve) => setTimeout(resolve, 2));
  }
}
function build(cfg: Config, client: WorkerClient, memory?: WorkerMemoryRuntime,
  outbox?: Outbox, runnerOverrides: object = {}, isolatedRunner?: IsolatedRunner) {
  const runner = { shutdown: () => {}, resumePendingRecoveries: async () => {}, settlePendingPredecessors: async () => {},
    observeSettlementTerminalAck: async () => {}, ...runnerOverrides } as unknown as RunRunner;
  return new Worker(cfg, client, runner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
    nullLogger(), () => ({ ok: true, missing: [] }), outbox, undefined, undefined, undefined,
    undefined, undefined, undefined,
    (dataDir) => new StatsCollector({ dataDir, processRss: () => 1 }),
    isolatedRunner, undefined, undefined, undefined, undefined, memory);
}
function rig(overrides: object = {}, reader?: Pick<WorkerMemoryReader, "sample">) {
  let samples = 0;
  let reservations = 0;
  let invalidation: (() => void) | undefined;
  let incarnation: WorkerClient["memoryIncarnation"] = { worker_id: workerId, register_nonce: "fixture", revision: 1 };
  const client = {
    get memoryIncarnation() { return incarnation; },
    hasFeature: (feature: string) => feature === "worker_memory_pressure_v1",
    subscribeMemoryInvalidation: (fn: () => void) => { invalidation = fn; return () => { invalidation = undefined; }; },
    revokeMemoryAuthority: () => { incarnation = undefined; invalidation?.(); },
    reserveMemoryIntervention: async () => { reservations++; throw new Error("unenrolled"); },
    reportMemoryInterventionOutcome: async () => { throw new Error("unenrolled"); },
    register: async () => ({ worker_id: workerId }),
    heartbeat: async () => {},
    claimRun: async () => null,
    claimChat: async () => null,
    ...overrides,
  } as unknown as WorkerClient;
  const memory = new WorkerMemoryRuntime(settings, client, nullLogger(), {
    reader: reader ?? { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: ++samples, startedAtMs: time, completedAtMs: time,
        currentBytes: 950, limitBytes: 1000, stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
          full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
  return { client, memory, samples: () => samples, reservations: () => reservations };
}

test("memory samples continue through stalled heartbeat and real boot terminal gate; Claude enrolls nothing", async () => {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/memory-outbox-"));
  const outbox = new Outbox({ root: dir, log: nullLogger(), runMaxBytes: 1 << 20,
    maxBytes: 2 << 20, retentionMs: 10000 });
  await outbox.init();
  await outbox.journalTerminal(runId, 1, "running", 0, { status: "completed" });
  const beat = deferred();
  const boot = deferred();
  let beats = 0, reports = 0, claims = 0;
  const f = rig({
    heartbeat: async () => { beats++; await beat.promise; },
    reportState: async () => { reports++; await boot.promise; return { applied: true, status: "completed" }; },
    claimRun: async () => { claims++; return null; },
  });
  const controller = new AbortController();
  const worker = build(config(), f.client, f.memory, outbox);
  const done = worker.run(controller.signal);
  try {
    await until(() => beats === 1 && reports >= 1);
    const before = f.samples();
    await until(() => f.samples() >= before + 4);
    assert.equal(claims, 0);
    assert.deepEqual(f.memory.commands.eligibleEntries(), []);
    assert.equal(f.reservations(), 0);
    controller.abort();
    const stopped = f.samples();
    assert.equal(f.client.memoryIncarnation, undefined);
    beat.resolve(); boot.resolve();
    await done;
    assert.equal(f.samples(), stopped);
  } finally {
    controller.abort(); beat.resolve(); boot.resolve();
    await done;
    await fs.rm(dir, { recursive: true, force: true });
  }
});

for (const stall of ["startup", "registration"] as const) test("sampling is independent of stalled " + stall, async () => {
  const gate = deferred();
  let entered = false, registrations = 0;
  const f = rig({ register: async () => {
    registrations++;
    if (stall === "registration") { entered = true; await gate.promise; }
    return { worker_id: workerId };
  } });
  const worker = build(config(), f.client, f.memory, undefined, stall === "startup" ? {
    snapshotBootRecoveries: async () => { entered = true; await gate.promise; return []; },
  } : {});
  const controller = new AbortController();
  const done = worker.run(controller.signal);
  try {
    await until(() => entered);
    const before = f.samples();
    await until(() => f.samples() >= before + 4);
    if (stall === "startup") assert.equal(registrations, 0);
    controller.abort();
    assert.equal(f.client.memoryIncarnation, undefined);
  } finally {
    controller.abort(); gate.resolve(); await done;
  }
  const stopped = f.samples();
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(f.samples(), stopped);
});

test("invalid incarnation is fatal even when the API advertises the feature", async () => {
  let registrations = 0;
  const f = rig({ register: async () => {
    registrations++; return { worker_id: workerId };
  } });
  f.client.revokeMemoryAuthority();
  const worker = build(config(), f.client, f.memory);
  await assert.rejects(worker.run(new AbortController().signal), /valid registration incarnation/);
  assert.equal(registrations, 1);
  const stopped = f.samples();
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(f.samples(), stopped);
});

test("sampler failure is fatal and joins the stopped runtime", async () => {
  const f = rig();
  // Dispose the fixture's unused monitor before installing the replacement listener.
  await f.memory.stop();
  const memory = new WorkerMemoryRuntime(settings, f.client, nullLogger(), {
    reader: { sample: () => { throw new Error("sample fixture failure"); } },
  });
  const worker = build(config(), f.client, memory);
  await assert.rejects(worker.run(new AbortController().signal), /sample fixture failure/);
  assert.equal(f.client.memoryIncarnation, undefined);
});

for (const lane of ["run", "isolated"] as const) test("fatal sampler failure cancels active " + lane + " execution before joining", async () => {
  const entered = deferred();
  const cancelled = deferred();
  const drain = deferred();
  const cancel = new AbortController();
  const failure = new Error("active sample fixture failure");
  let failSample = false, claimed = false, finished = false, settled = false;
  let sampleId = 0;
  const events: string[] = [];
  const shutdownObservations: { incarnation: WorkerClient["memoryIncarnation"]; current: boolean }[] = [];
  const active = rig({ claimRun: async () => {
    if (claimed) return null;
    claimed = true;
    return { run_id: runId, kind: "task", claim_generation: 1,
      ...(lane === "isolated" ? { isolated_fetch: {} } : {}) };
  } }, { sample: () => {
    if (failSample) throw failure;
    const time = performance.now();
    return { available: false, reason: "absent", sampleId: ++sampleId,
      startedAtMs: time, completedAtMs: time };
  } });
  const memory = active.memory;
  const context = memory.begin({ run_id: runId, worker_id: workerId, register_nonce: "fixture",
    claim_generation: 1, memory_episode: 0, flight: {}, lifecycleRevision: 1 },
    { feedback: async () => {}, preserve: async () => {} });
  const execution = {
    execute: async () => {
      events.push("execute");
      const aborted = new Promise<void>((resolve) => {
        cancel.signal.addEventListener("abort", () => {
          events.push("cancel");
          cancelled.resolve();
          resolve();
        }, { once: true });
      });
      entered.resolve();
      await aborted;
      await drain.promise;
      finished = true;
      events.push("drained");
    },
    shutdown: () => {
      shutdownObservations.push({ incarnation: active.client.memoryIncarnation,
        current: memory.commands.isCurrent(context) });
      events.push("shutdown");
      cancel.abort();
    },
  };
  const worker = build(config(), active.client, memory, undefined,
    lane === "run" ? execution : { shutdown: () => {
      shutdownObservations.push({ incarnation: active.client.memoryIncarnation,
        current: memory.commands.isCurrent(context) });
      events.push("run shutdown");
    } },
    lane === "isolated" ? execution as unknown as IsolatedRunner : undefined);
  const controller = new AbortController();
  const done = worker.run(controller.signal);
  const outcome = done.then(() => { settled = true; return undefined; },
    (error: unknown) => { settled = true; return error; });
  let deadline: ReturnType<typeof setTimeout> | undefined;
  const bounded = new Promise<never>((_resolve, reject) => {
    deadline = setTimeout(() => reject(new Error("active execution shutdown did not join within 2s")), 2000);
  });
  try {
    await Promise.race([entered.promise, bounded]);
    failSample = true;
    await Promise.race([cancelled.promise, bounded]);
    assert.equal(settled, false, "worker must await the execution's unwind");
    assert.equal(finished, false);
    assert.deepEqual(shutdownObservations, Array.from({ length: lane === "run" ? 1 : 2 },
      () => ({ incarnation: undefined, current: false })));
    drain.resolve();
    assert.equal(await Promise.race([outcome, bounded]), failure, "retain the original sampler error");
    assert.equal(finished, true);
    assert.deepEqual(events, lane === "run"
      ? ["execute", "shutdown", "cancel", "drained"]
      : ["execute", "run shutdown", "shutdown", "cancel", "drained"]);
  } finally {
    if (deadline !== undefined) clearTimeout(deadline);
    controller.abort();
    cancel.abort();
    drain.resolve();
    await outcome;
    await active.memory.stop();
  }
});

test("repeated Worker.run and client registration rotations keep a single sampler", async () => {
  const f = rig();
  const controller = new AbortController();
  const worker = build(config(), f.client, f.memory);
  const done = worker.run(controller.signal);
  assert.equal(worker.run(controller.signal), done);
  assert.equal(f.memory.run(), f.memory.run());
  try {
    await until(() => f.samples() >= 3);
    for (let i = 0; i < 3; i++) { f.client.revokeMemoryAuthority(); await f.client.register("fixture", "base"); }
    const before = f.samples();
    await until(() => f.samples() >= before + 3);
    assert.equal(f.reservations(), 0);
  } finally { controller.abort(); await done; }
});

test("shutdown before run prevents registration and sampling; stop revokes contexts synchronously", async () => {
  let registrations = 0;
  const f = rig({ register: async () => { registrations++; return {}; } });
  const context = f.memory.begin({ run_id: runId, worker_id: workerId, register_nonce: "fixture",
    claim_generation: 1, memory_episode: 0, flight: {}, lifecycleRevision: 1 },
    { feedback: async () => { throw new Error("unexpected"); }, preserve: async () => { throw new Error("unexpected"); } });
  const worker = build(config(), f.client, f.memory);
  const stopped = worker.shutdownMemory();
  assert.equal(f.memory.commands.isCurrent(context), false);
  assert.equal(f.client.memoryIncarnation, undefined);
  await worker.run(new AbortController().signal);
  await stopped;
  assert.equal(f.samples(), 0);
  assert.equal(registrations, 0);
});

for (const failure of ["registration", "boot"] as const) test("failed " + failure + " stops and joins sampler", async () => {
  const f = rig(failure === "registration" ? {
    register: async () => { throw new Error("held network"); },
  } : {});
  const worker = build(config(), f.client, f.memory, undefined, failure === "boot" ? {
    snapshotBootRecoveries: async () => { throw new Error("boot fixture failure"); },
  } : {});
  const controller = new AbortController();
  const done = worker.run(controller.signal);
  if (failure === "registration") {
    await until(() => f.samples() >= 4);
    controller.abort();
    await done;
  } else await assert.rejects(done, /boot fixture failure/);
  const stopped = f.samples();
  assert.equal(f.client.memoryIncarnation, undefined);
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(f.samples(), stopped);
});

for (const memoryGuard of [undefined, { enabled: false as const }, settings]) {
  test("capability and fatal unsupported API: " + (memoryGuard?.enabled ?? "default"), async () => {
    const cfg = config(); cfg.memoryGuard = memoryGuard;
    let registrations = 0;
    let advertised: string[] = [];
    const controller = new AbortController();
    const f = rig({ hasFeature: () => false, register: async (
      _name: string, _template: string, _max: number, _caps: string[], caps: string[],
    ) => { registrations++; advertised = caps; return { worker_id: workerId }; },
    claimRun: async () => { controller.abort(); return null; } });
    const worker = build(cfg, f.client, memoryGuard?.enabled ? f.memory : undefined);
    try {
      const done = worker.run(controller.signal);
      if (memoryGuard?.enabled) await assert.rejects(done, /requires worker_memory_pressure_v1/);
      else await done;
      assert.equal(registrations, 1);
      assert.equal(advertised.includes("worker_memory_pressure_v1"), memoryGuard?.enabled ?? false);
    } finally { controller.abort(); await f.memory.stop(); }
  });
}

test("revocation during register prevents its late response from restoring client memory authority", async (t) => {
  const gate = deferred();
  t.mock.method(globalThis, "fetch", async () => {
    await gate.promise;
    return new Response(JSON.stringify({ worker_id: workerId, register_nonce: "fixture",
      protocol_features: ["worker_memory_pressure_v1"] }), { status: 200 });
  });
  const client = new WorkerClient("http://example.invalid", "fixture", "test", nullLogger());
  const registering = client.register("fixture", "base");
  const memory = new WorkerMemoryRuntime(settings, client, nullLogger(), {
    reader: { sample: () => { throw new Error("must not sample"); } },
  });
  await memory.stop();
  gate.resolve();
  await registering;
  assert.equal(client.memoryIncarnation, undefined);
});
