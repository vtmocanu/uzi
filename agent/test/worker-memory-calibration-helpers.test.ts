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

function reportInput() {
  const binding = { reportId: "report", legId: "leg", interventionId: "intervention",
    treeId: "tree", toolId: "tool", reservationId: "reservation" };
  const span = (startMs: number, endMs: number) => ({ binding, startMs, endMs });
  return { baseline: [sample(1, 100), sample(2, 110), sample(3, 105)],
    samples: [sample(4, 200), sample(5, 300), sample(6, 500)], reserveBytes: 1024 * MiB - 300,
    response: { provenance: "synthetic" as "synthetic" | "measured", reportId: "report", legs: [{
      binding, hardThresholdBytes: 300,
      hardBracket: { lastBelowSampleId: 4, firstAtOrAboveSampleId: 5, lowerMs: 40, upperMs: 51 },
      spans: { observe: span(51, 53), reservation: span(54, 57), signal: span(58, 59),
        drain: span(60, 70), cleanup: span(70, 75) },
      ack: { binding, atMs: 57, accepted: true as const },
      exit: { binding, atMs: 70, confirmed: true as const },
      cleanup: { binding, confirmed: true as const }, oomKillsBefore: 0, oomKillsAfter: 0,
      oomBefore: 0, oomAfter: 0, oomGroupKillsBefore: 0, oomGroupKillsAfter: 0,
    }] } };
}

test("synthetic bound response never claims real measurement; raw growth and strict reserve include bracket uncertainty once", () => {
  const input = reportInput(), report = calibrationReport(input);
  assert.equal(report.elapsedMs, 21);
  assert.equal(report.jitterBytes, 10);
  assert.equal(report.peakPositiveGrowthRateBytesPerMs, 20);
  assert.equal(report.maxSampleGapMs, 9);
  assert.equal(report.netGrowthBytes, 300);
  assert.equal(report.monotonicGrowth, true);
  assert.equal(report.responseMeasured, false);
  assert.equal(report.accepted, false);
  assert.equal(report.legs[0]!.observationToDrainMs, 19);
  assert.equal(report.legs[0]!.drainMinusBracketLowerMs, 30);
  assert.equal(report.conservativeStageSumMs, 30);
  assert.deepEqual(report.legs[0]!.stageLatenciesMs, { crossingToObserveEnd: 13, reservation: 4, signal: 2, drain: 11 });
  assert.equal(report.worstEndToEndLatencyMs, 30);
  assert.deepEqual(report.reserveCheck, { reserveBytes: input.reserveBytes, requiredBytes: 610, satisfied: true });
  const equality = reportInput();
  equality.reserveBytes = 610;
  const h = 1024 * MiB - equality.reserveBytes;
  equality.response.legs[0]!.hardThresholdBytes = h;
  equality.samples = [sample(4, h - 100), sample(5, h), sample(6, h + 200)];
  assert.equal(calibrationReport(equality).reserveCheck.requiredBytes, 610);
  assert.equal(calibrationReport(equality).reserveCheck.satisfied, false);
  equality.samples[2] = sample(6, equality.samples[2]!.currentBytes - 1);
  assert.equal(calibrationReport(equality).reserveCheck.satisfied, true);
  input.response.provenance = "measured"; // Contract branch only; no real measurement performed by this test.
  assert.equal(calibrationReport(input).accepted, true);
  assert.throws(() => calibrationReport({ ...input, response: undefined! }), /invalid calibration report/);
});

test("measured response fails closed on absent, misordered or mismatched bracket/spans/ACK/exit/cleanup/OOM evidence", () => {
  const mutations: ((input: ReturnType<typeof reportInput>) => void)[] = [
    (i) => { i.response.legs = []; },
    (i) => { i.response.legs[0]!.hardBracket.lowerMs = 41; },
    (i) => { i.response.legs[0]!.hardBracket.upperMs = 50; },
    (i) => { i.response.legs[0]!.hardBracket.lastBelowSampleId = 3; },
    (i) => { i.response.legs[0]!.hardBracket.firstAtOrAboveSampleId = 6; },
    (i) => { i.response.legs[0]!.hardThresholdBytes = 100; },
    (i) => { i.response.legs[0]!.spans.observe = undefined!; },
    (i) => { i.response.legs[0]!.spans.observe.startMs = 39; }, // early PSI cannot replace hard acceptance
    (i) => { i.response.legs[0]!.spans.drain.startMs = 58; },
    (i) => { i.response.legs[0]!.spans.drain.endMs = Number.NaN; },
    (i) => { i.response.legs[0]!.spans.signal.binding = { ...i.response.legs[0]!.binding, toolId: "other" }; },
    (i) => { i.response.reportId = "other"; },
    (i) => { i.response.legs[0]!.ack = undefined!; },
    (i) => { i.response.legs[0]!.ack.atMs = 60; },
    (i) => { i.response.legs[0]!.exit = undefined!; },
    (i) => { i.response.legs[0]!.exit.atMs = 69; },
    (i) => { i.response.legs[0]!.cleanup = undefined!; },
    (i) => { i.response.legs[0]!.oomKillsAfter = 1; },
    (i) => { i.response.legs[0]!.oomAfter = 1; },
    (i) => { i.response.legs[0]!.oomGroupKillsAfter = 1; },
    (i) => { i.samples[1]!.limitBytes *= 2; },
    (i) => { i.samples.reverse(); },
  ];
  for (const mutate of mutations) {
    const input = reportInput(); input.response.provenance = "measured"; mutate(input);
    assert.throws(() => calibrationReport(input));
  }
  for (const key of ["reportId", "legId", "interventionId", "treeId", "toolId", "reservationId"] as const) {
    const input = reportInput();
    input.response.legs[0]!.spans.drain.binding = { ...input.response.legs[0]!.binding, [key]: "other" };
    assert.throws(() => calibrationReport(input), /mismatched/);
  }
});

test("reservation ACK must end reservation before signal; signal-time ACK is invalid", () => {
  const input = reportInput(), leg = input.response.legs[0]!;
  assert.equal(calibrationReport(input).legs.length, 1);
  for (const atMs of [53, 54, 56, 58, 59]) {
    leg.ack.atMs = atMs;
    assert.throws(() => calibrationReport(input), /ACK/);
  }
  leg.ack.atMs = leg.spans.reservation.endMs;
  assert.equal(calibrationReport(input).legs.length, 1);
});

test("H is derived from a positive reserve strictly below the finite limit", () => {
  for (const reserveBytes of [0, -1, 1024 * MiB, 1024 * MiB + 1, 611]) {
    assert.throws(() => calibrationReport({ ...reportInput(), reserveBytes }));
  }
});

test("all OOM counters require natural before/after values and zero event deltas", () => {
  for (const [before, after] of [
    ["oomBefore", "oomAfter"], ["oomKillsBefore", "oomKillsAfter"],
    ["oomGroupKillsBefore", "oomGroupKillsAfter"],
  ] as const) {
    for (const [start, end] of [[0, 1], [1, 0], [-1, -1], [0.5, 0.5], [0, NaN], [0, undefined]]) {
      const input = reportInput(), leg = input.response.legs[0]!;
      leg[before] = start!;
      leg[after] = end!;
      assert.throws(() => calibrationReport(input), /OOM/);
    }
    const input = reportInput();
    input.response.legs[0]![before] = 2;
    input.response.legs[0]![after] = 2;
    assert.equal(calibrationReport(input).legs.length, 1);
  }
});

test("growth uses peak positive raw sample pair, including after a decrease", () => {
  const input = reportInput();
  input.samples = [sample(4, 200), sample(5, 300), sample(6, 250), sample(7, 650)];
  const report = calibrationReport(input);
  assert.equal(report.peakPositiveGrowthRateBytesPerMs, 40);
  assert.equal(report.monotonicGrowth, false);
  assert.equal(report.reserveCheck.requiredBytes, 1210);
});

test("early PSI is retained as context and cannot substitute for the hard crossing response", () => {
  const input = reportInput(), leg = input.response.legs[0]!;
  const earlyPsiObservation = { binding: leg.binding, startMs: 35, endMs: 36 };
  const report = calibrationReport({ ...input, response: { ...input.response, legs: [{ ...leg, earlyPsiObservation }] } });
  assert.deepEqual(report.legs[0]!.earlyPsiObservation, earlyPsiObservation);
  assert.equal(report.worstEndToEndLatencyMs, 30);
  assert.throws(() => calibrationReport({ ...input, response: { ...input.response,
    legs: [{ ...leg, earlyPsiObservation, spans: { ...leg.spans, observe: earlyPsiObservation } }] } }));
});

test("two ordered legs at the same H use adjacent crossings after prior cleanup and retain stage maxima", () => {
  const input = reportInput(), first = input.response.legs[0]!;
  const binding = { ...first.binding, legId: "leg2", interventionId: "intervention2",
    treeId: "tree2", toolId: "tool2", reservationId: "reservation2" };
  const span = (startMs: number, endMs: number) => ({ binding, startMs, endMs });
  const second = { ...first, binding,
    hardBracket: { lastBelowSampleId: 8, firstAtOrAboveSampleId: 9, lowerMs: 80, upperMs: 91 },
    spans: { observe: span(91, 92), reservation: span(92, 110), signal: span(110, 112),
      drain: span(112, 115), cleanup: span(115, 116) },
    ack: { binding, atMs: 110, accepted: true as const }, exit: { binding, atMs: 115, confirmed: true as const },
    cleanup: { binding, confirmed: true as const } };
  input.samples.push(sample(8, 250), sample(9, 400));
  input.response.legs.push(second);
  const report = calibrationReport(input);
  assert.equal(report.worstEndToEndLatencyMs, 35);
  assert.equal(report.conservativeStageSumMs, 44);
  assert.equal(report.reserveCheck.requiredBytes, 710);
  input.response.legs.reverse();
  assert.throws(() => calibrationReport(input), /misordered|bracket/);
});
