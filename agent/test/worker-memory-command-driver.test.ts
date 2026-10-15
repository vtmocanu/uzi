import assert from "node:assert/strict";
import { test } from "node:test";
import { commandTreeCalibration } from "./helpers/worker-memory-command-driver.js";
import type { CalibrationIntervention, CommandStep, EnrolledCalibrationProducer } from "./helpers/worker-memory-command-driver.js";
import { calibrationPlan } from "./helpers/worker-memory-calibration.js";
import type { WorkerMemoryReader } from "../src/worker-memory-reader.js";
import type { WorkerMemoryPressureResult } from "../src/protocol.js";

function deferred<T>() {
  let resolve!: (value: T) => void, reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function fixture(windowMs = 1000) {
  const baseline = [1, 2, 3].map((sampleId) => ({
    available: true, sampleId, startedAtMs: sampleId, completedAtMs: sampleId,
    currentBytes: 64 * 1024 ** 2, limitBytes: 1024 ** 3,
    stat: { anon: 0, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
    pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 }, full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } },
  } satisfies Extract<ReturnType<WorkerMemoryReader["sample"]>, { available: true }>));
  const plan = calibrationPlan({ dedicated: true, baseline, windowMs });
  const selected = deferred<CalibrationIntervention>(), feedback = deferred<WorkerMemoryPressureResult>();
  const drain = deferred<Awaited<CalibrationIntervention["confirmedDrain"]>>();
  const steps = [deferred<CommandStep>(), deferred<CommandStep>()];
  const failures = [deferred<never>(), deferred<never>()];
  const events: string[] = [];
  const binding = { reportId: "report", legId: "leg", interventionId: "intervention",
    treeId: "tree0", toolId: "tool0", reservationId: "reservation" };
  const pair = [0, 1].map((index): EnrolledCalibrationProducer => ({
    treeId: "tree" + index, identity: { pid: 100 + index, startIdentity: "start" + index },
    enrollment: { run_id: "run", worker_id: "worker", claim_generation: 1, memory_episode: 0,
      attempt_id: "attempt" + index, command_id: "command" + index, tool_id: "tool" + index, command: "fixture" },
    unexpectedFailure: failures[index]!.promise,
    step: async (_plan, signal) => {
      events.push("step" + index);
      signal.addEventListener("abort", () => { events.push("abort" + index); }, { once: true });
      return steps[index]!.promise;
    },
    proveLive: async (saved) => { events.push("live" + index); return { ...saved }; },
    stop: async () => { events.push("stop" + index); steps[index]!.resolve({ kind: "completed" }); },
    forceStop: async () => { events.push("force" + index); steps[index]!.resolve({ kind: "completed" }); },
  })) as unknown as [EnrolledCalibrationProducer, EnrolledCalibrationProducer];
  const actual: WorkerMemoryPressureResult = { ...pair[0].enrollment, intervention_id: "intervention",
    code: "worker_memory_pressure", observed_total_bytes: 100, limit_bytes: 1024 ** 3,
    tree_rss_bytes_approx: 50, cancellation_confirmed: true, outcome: "confirmed_drained",
    retry_blocked: true, guidance: "Use smaller or serial commands after recovery." };
  const select = () => selected.resolve({ binding, selected: 0, feedback: feedback.promise, confirmedDrain: drain.promise });
  const confirm = () => {
    feedback.resolve(actual);
    drain.resolve({ binding, identity: pair[0].identity, confirmed: true });
    steps[0]!.resolve({ kind: "cancelled", feedback: { ...actual } });
  };
  const start = async () => {
    const done = commandTreeCalibration(plan, { producers: pair, intervention: selected.promise });
    // Attach immediately: failure tests must not manufacture unhandled rejections.
    void done.catch(() => {});
    for (let i = 0; i < 20 && events.length < 2; i++) await Promise.resolve();
    assert.deepEqual(events, ["step0", "step1"]);
    return { done };
  };
  return { plan, pair, events, steps, failures, feedback, drain, binding, actual, select, confirm, start };
}

test("expected selected cancellation needs matching actual tool feedback AND confirmed drain; sibling live proof precedes global cleanup", async () => {
  const f = fixture(), { done } = await f.start();
  f.select();
  f.steps[0]!.resolve({ kind: "cancelled", feedback: f.actual });
  for (let i = 0; i < 20; i++) await Promise.resolve();
  assert.deepEqual(f.events, ["step0", "step1"]); // no premature stop, abort or proof
  f.feedback.resolve(f.actual);
  for (let i = 0; i < 20; i++) await Promise.resolve();
  assert.deepEqual(f.events, ["step0", "step1"]);
  f.drain.resolve({ binding: f.binding, identity: f.pair[0].identity, confirmed: true });
  const result = await done;
  assert.equal(result.sibling.pid, 101);
  assert.deepEqual(f.events, ["step0", "step1", "live1", "abort0", "abort1", "stop0", "stop1"]);
});

test("unexpected step rejection, unexpected exit and unselected cancellation fail; sibling is proved before cleanup", async () => {
  for (const mode of ["step", "exit", "cancel"] as const) {
    const f = fixture(), { done } = await f.start();
    if (mode === "step") f.steps[0]!.reject(new Error("unexpected step"));
    if (mode === "exit") f.failures[0]!.reject(new Error("unexpected exit"));
    if (mode === "cancel") f.steps[0]!.resolve({ kind: "cancelled", feedback: f.actual });
    await assert.rejects(done, /unexpected/);
    assert.ok(f.events.indexOf("live1") < f.events.indexOf("abort0"));
    assert.ok(f.events.indexOf("live1") < f.events.indexOf("stop1"));
  }
});

test("mismatched actual feedback, drain identity and binding fail even with cancellation-shaped step", async () => {
  for (const mode of ["feedback", "stepFeedback", "identity", "binding"] as const) {
    const f = fixture(), { done } = await f.start();
    f.select();
    f.feedback.resolve(mode === "feedback" ? { ...f.actual, command_id: "other" } : f.actual);
    f.steps[0]!.resolve({ kind: "cancelled", feedback: mode === "stepFeedback" ? { ...f.actual, intervention_id: "other" } : f.actual });
    f.drain.resolve({ binding: mode === "binding" ? { ...f.binding, reservationId: "other" } : f.binding,
      identity: mode === "identity" ? { ...f.pair[0].identity, startIdentity: "reused" } : f.pair[0].identity, confirmed: true });
    await assert.rejects(done, /mismatched actual/);
  }
});

test("failed sibling proof precedes cleanup, joins both handles and rejects the leg", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  for (const mode of ["mismatch", "reject", "timeout"] as const) {
    const f = fixture();
    f.pair[1] = { ...f.pair[1], proveLive: async (saved) => {
      f.events.push("proof1");
      if (mode === "reject") throw new Error("proof fixture");
      if (mode === "timeout") await new Promise<void>(() => {});
      return { ...saved, startIdentity: "reused" };
    } };
    const { done } = await f.start(); f.select(); f.confirm();
    for (let i = 0; i < 100 && !f.events.includes("proof1"); i++) await Promise.resolve();
    assert.deepEqual(f.events, ["step0", "step1", "proof1"]);
    if (mode === "timeout") t.mock.timers.tick(5000);
    await assert.rejects(done, /sibling proof failed; no next leg/);
    assert.deepEqual(f.events, ["step0", "step1", "proof1", "abort0", "abort1", "stop0", "stop1"]);
  }
});

test("cleanup error forces both owned joins, rejects and permits no successful next leg", async () => {
  const f = fixture();
  f.pair[0] = { ...f.pair[0], stop: async () => { f.events.push("stop0"); throw new Error("cleanup fixture"); } };
  const { done } = await f.start(); f.select(); f.confirm();
  await assert.rejects(done, /cleanup failed; no next leg/);
  assert.ok(f.events.includes("stop1"));
  assert.deepEqual(f.events.slice(-2), ["force0", "force1"]);
});

test("completed selected step cannot substitute for selected cancellation", async () => {
  const f = fixture(), { done } = await f.start();
  f.select(); f.feedback.resolve(f.actual);
  f.drain.resolve({ binding: f.binding, identity: f.pair[0].identity, confirmed: true });
  f.steps[0]!.resolve({ kind: "completed" });
  await assert.rejects(done, /mismatched actual/);
});

test("load timeout halts work and proves sibling before aborting and joining both handles", async () => {
  const f = fixture(5), { done } = await f.start();
  await assert.rejects(done, /load window expired/);
  assert.deepEqual(f.events, ["step0", "step1", "live1", "abort0", "abort1", "stop0", "stop1"]);
});

test("selected intervention does not excuse unexpected sibling failure", async () => {
  const f = fixture(), { done } = await f.start();
  f.select(); f.steps[1]!.reject(new Error("unexpected sibling failure")); f.confirm();
  await assert.rejects(done, /unexpected sibling failure|unexpected producer failure/);
});

test("unjoined graceful stop triggers bounded forced joins on both exact handles", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const f = fixture(), stopped = deferred<void>();
  const force = f.pair[0].forceStop;
  f.pair[0] = { ...f.pair[0], stop: async () => { f.events.push("stop0"); await stopped.promise; },
    forceStop: async () => { await force(); stopped.resolve(); } };
  const { done } = await f.start(); f.select(); f.confirm();
  for (let i = 0; i < 100 && !f.events.includes("stop1"); i++) await Promise.resolve();
  assert.ok(f.events.includes("stop1"));
  t.mock.timers.tick(15000);
  await assert.rejects(done, /cleanup failed; no next leg/);
  assert.deepEqual(f.events.slice(-2), ["force0", "force1"]);
});
