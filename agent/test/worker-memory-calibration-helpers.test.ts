import assert from "node:assert/strict";
import { test } from "node:test";
import { calibrationPlan, calibrationLoad, calibrationReport, calibrationThreadPort } from "./helpers/worker-memory-calibration.js";
import type { WorkerMemoryReader } from "../src/worker-memory-reader.js";

const MiB = 1024 * 1024;
function sample(id: number, usage = 64 * MiB, limit = 1024 * MiB): Extract<ReturnType<WorkerMemoryReader["sample"]>, { available: true }> {
  return { available: true, sampleId: id, startedAtMs: id * 10, completedAtMs: id * 10 + 1,
    currentBytes: usage, limitBytes: limit, stat: { anon: usage, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
    pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 }, full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
}
const baseline = [sample(1), sample(2), sample(3)];

test("dedicated 1Gi/2Gi budgets account for baseline, four charge classes, two producers and overhead", () => {
  for (const limit of [1024 * MiB, 2048 * MiB]) {
    const plan = calibrationPlan({ dedicated: true, baseline: [sample(1, 64 * MiB, limit), sample(2, 65 * MiB, limit), sample(3, 64 * MiB, limit)], windowMs: 30000 });
    assert.equal(plan.baselineBytes, 65 * MiB);
    assert.equal(plan.allocationBytes, limit === 1024 * MiB ? 128 * MiB : 256 * MiB);
    assert.ok(plan.baselineBytes + plan.allocationBytes + plan.overheadBytes <= limit / 2);
    assert.ok(Object.isFrozen(plan));
  }
  for (const input of [
    { dedicated: false, baseline, windowMs: 100 },
    { dedicated: true, baseline, windowMs: 30001 },
    { dedicated: true, baseline, windowMs: 0 },
    { dedicated: true, baseline: [sample(1, 400 * MiB), sample(2, 400 * MiB), sample(3, 400 * MiB)], windowMs: 100 },
    { dedicated: true, baseline: [sample(1, 1, 0), sample(2, 1, 0), sample(3, 1, 0)], windowMs: 100 },
    { dedicated: true, baseline: [sample(2), sample(1), sample(3)], windowMs: 100 },
  ]) assert.throws(() => calibrationPlan(input));
});

test("load steps both producers concurrently and always stops exact handles, with no real allocation", async () => {
  const plan = calibrationPlan({ dedicated: true, baseline, windowMs: 1000 });
  let id = 0, steps = 0, active = 0, peak = 0;
  const stopped: number[] = [], started: number[] = [];
  const samples = await calibrationLoad(plan, {
    now: () => 0, sample: () => sample(++id, 64 * MiB + steps * 4 * MiB),
    producer: async (index) => {
      started.push(index);
      return { step: async () => {
        peak = Math.max(peak, ++active);
        await Promise.resolve(); steps++; active--;
      }, stop: async () => { stopped.push(index); } };
    },
  });
  assert.deepEqual(started, [0, 1]);
  assert.deepEqual(stopped, [0, 1]);
  assert.equal(peak, 2);
  assert.equal(steps, 32);
  assert.equal(samples.length, 33);
  assert.equal(samples.at(-1)!.currentBytes, 192 * MiB);
});

test("unavailable, drifted and unsafe samples are hard prerequisites, not skips; no producer starts", async () => {
  const plan = calibrationPlan({ dedicated: true, baseline, windowMs: 1000 });
  for (const value of [
    { available: false as const, reason: "unlimited" as const, sampleId: 1, startedAtMs: 0, completedAtMs: 1 },
    sample(1, 64 * MiB, 2048 * MiB), sample(1, 400 * MiB),
  ]) {
    let starts = 0;
    await assert.rejects(calibrationLoad(plan, { now: () => 0, sample: () => value,
      producer: async () => { starts++; throw new Error("must not start"); } }), /sample unavailable|safety ceiling/);
    assert.equal(starts, 0);
  }
  await assert.rejects(calibrationThreadPort({ disk: "/nonexistent-calibration-fixture", shmem: "/nonexistent-calibration-fixture" }));
});

test("one failed producer aborts its sibling and joins both; timeout aborts owned work", async () => {
  for (const timeout of [false, true]) {
    const plan = calibrationPlan({ dedicated: true, baseline, windowMs: timeout ? 5 : 1000 });
    let id = 0, aborted = 0, stopped = 0;
    await assert.rejects(calibrationLoad(plan, { now: () => 0, sample: () => sample(++id),
      producer: async (index, _plan, signal) => {
        signal.addEventListener("abort", () => { aborted++; });
        return { step: () => index === 0 && !timeout ? Promise.reject(new Error("producer fixture failure"))
          : new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true })),
        stop: async () => { stopped++; } };
      },
    }), timeout ? /window expired/ : /producer fixture failure/);
    assert.equal(aborted, 2); assert.equal(stopped, 2);
  }
});

test("caller cancellation rejects and joins late startup and in-flight steps before returning", async () => {
  for (const duringStartup of [true, false]) {
    const controller = new AbortController();
    const plan = calibrationPlan({ dedicated: true, baseline, windowMs: 1000 });
    let id = 0, stopped = 0, joined = 0;
    const releases: (() => void)[] = [];
    const done = calibrationLoad(plan, { signal: controller.signal, now: () => 0,
      sample: () => sample(++id),
      producer: async () => {
        if (duringStartup) await new Promise<void>((resolve) => {
          controller.signal.addEventListener("abort", () => resolve(), { once: true });
        });
        return {
          step: async () => {
            await new Promise<void>((resolve) => { releases.push(resolve); });
            joined++;
          },
          stop: async () => { stopped++; for (const resolve of releases) resolve(); },
        };
      },
    });
    if (!duringStartup) {
      for (let turn = 0; turn < 100 && releases.length < 2; turn++) await Promise.resolve();
      assert.equal(releases.length, 2);
    }
    controller.abort();
    await assert.rejects(done, /cancelled/);
    assert.equal(stopped, 2);
    assert.equal(joined, duringStartup ? 0 : 2);
  }
});

test("already cancelled load starts nothing and cleanup failure rejects", async () => {
  const plan = calibrationPlan({ dedicated: true, baseline, windowMs: 1000 });
  const controller = new AbortController(); controller.abort();
  let starts = 0, id = 0;
  await assert.rejects(calibrationLoad(plan, { signal: controller.signal, now: () => 0,
    sample: () => sample(++id), producer: async () => { starts++; throw new Error("unexpected"); },
  }), /cancelled/);
  assert.equal(starts, 0);
  let stopped = 0;
  await assert.rejects(calibrationLoad(plan, { now: () => 0, sample: () => sample(++id),
    producer: async () => ({
      step: async () => { throw new Error("step failed"); },
      stop: async () => { stopped++; throw new Error("stop failed"); },
    }),
  }), /cleanup failed/);
  assert.equal(stopped, 2);
});

test("report derives monotonic growth, jitter, elapsed and strict reserve inequality; missing response remains unmeasured", () => {
  const input = { baseline: [sample(1, 100), sample(2, 110), sample(3, 105)],
    samples: [sample(4, 200), sample(5, 300), sample(6, 500)], spans: [],
    reserveBytes: 400, responseBudgetMs: 10 };
  const report = calibrationReport(input);
  assert.equal(report.elapsedMs, 21);
  assert.equal(report.jitterBytes, 10);
  assert.equal(report.growthBytesPerMs, 20);
  assert.equal(report.maxSampleGapMs, 9);
  assert.equal(report.netGrowthBytes, 300);
  assert.equal(report.monotonicGrowth, true);
  assert.deepEqual(report.latencies, { sample: [1, 1, 1], observe: [], reservation: [], signal: [], drain: [] });
  assert.equal(report.responseMeasured, false);
  assert.deepEqual(report.reserveCheck, { reserveBytes: 400, requiredBytes: 390, satisfied: true });
  assert.equal(calibrationReport({ ...input, reserveBytes: 390 }).reserveCheck.satisfied, false);
  // Explicit supplied fake spans exercise arithmetic, never claim a real cancellation.
  const measured = calibrationReport({ ...input, spans: [
    { kind: "observe", startMs: 10, endMs: 12 }, { kind: "reservation", startMs: 12, endMs: 15 },
    { kind: "signal", startMs: 15, endMs: 16 }, { kind: "drain", startMs: 16, endMs: 20 },
  ] });
  assert.equal(measured.responseMeasured, true);
  assert.deepEqual(measured.latencies.drain, [4]);
  assert.throws(() => calibrationReport({ ...input, spans: [{ kind: "drain", startMs: 20, endMs: 10 }] }));
});
