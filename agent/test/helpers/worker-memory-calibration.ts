import fs from "node:fs/promises";
import path from "node:path";
import { Worker } from "node:worker_threads";
import type { WorkerMemoryReader } from "../../src/worker-memory-reader.js";

type Sample = ReturnType<WorkerMemoryReader["sample"]>;
type Available = Extract<Sample, { available: true }>;
type Producer = { step: () => Promise<void>; stop: () => Promise<void> };
export type CalibrationPlan = Readonly<{
  limitBytes: number; ceilingBytes: number; baselineBytes: number;
  allocationBytes: number; overheadBytes: number; chunkBytes: number; rounds: number; windowMs: number;
}>;
type Plan = CalibrationPlan;
const MiB = 1024 * 1024;
function finite(value: number): boolean { return Number.isFinite(value) && value >= 0; }
function validateSamples(samples: readonly Available[], minimum = 3): void {
  if (samples.length > 256) throw new Error("calibration sample bound exceeded");
  if (samples.length < minimum) throw new Error("calibration needs at least three samples");
  for (let i = 0; i < samples.length; i++) {
    const sample = samples[i]!, prev = samples[i - 1];
    if (!sample.available || !Number.isSafeInteger(sample.limitBytes) || sample.limitBytes <= 0
      || !Number.isSafeInteger(sample.currentBytes) || sample.currentBytes < 0
      || !Number.isSafeInteger(sample.sampleId) || sample.sampleId < 1
      || !finite(sample.startedAtMs) || !finite(sample.completedAtMs) || sample.completedAtMs < sample.startedAtMs
      || (prev && (sample.sampleId <= prev.sampleId || sample.startedAtMs < prev.completedAtMs
        || sample.limitBytes !== prev.limitBytes))) throw new Error("invalid calibration sample sequence");
  }
}

/** Dedicated later 1Gi/2Gi fixtures only, never production recommendations.
 * Budget includes two producers, four disjoint charge classes and fixed overhead
 * for their buffers/threads and sequential bounded thread churn.
 */
export function calibrationPlan(input: {
  dedicated: boolean; baseline: readonly Available[]; windowMs: number;
}): Plan {
  validateSamples(input.baseline);
  const limitBytes = input.baseline[0]!.limitBytes;
  if (!input.dedicated || ![1024 * MiB, 2048 * MiB].includes(limitBytes)
    || !Number.isInteger(input.windowMs) || input.windowMs < 1 || input.windowMs > 30000) {
    throw new Error("dedicated finite 1Gi/2Gi fixture and <=30s window required");
  }
  const baselineBytes = Math.max(...input.baseline.map((sample) => sample.currentBytes));
  const overheadBytes = 64 * MiB, ceilingBytes = limitBytes / 2;
  const rounds = limitBytes === 1024 * MiB ? 16 : 32, chunkBytes = MiB;
  const allocationBytes = 2 * 4 * rounds * chunkBytes;
  if (baselineBytes + allocationBytes + overheadBytes > ceilingBytes) {
    throw new Error("baseline plus allocations and producer overhead exceeds 50% limit");
  }
  return Object.freeze({ limitBytes, baselineBytes, overheadBytes, ceilingBytes,
    rounds, chunkBytes, allocationBytes, windowMs: input.windowMs });
}

/** Two concurrent producers in one <=30s window. A failure aborts siblings.
 * All startup/step/cleanup ports must honor abort and stop by owned handle.
 * The real port below terminates its owned thread; fake tests exercise the public ports.
 * Cleanup joins startup and step work after abort and handle termination; its duration
 * is not a measured cancellation span or part of the load window.
 * Command-tree acceptance uses commandTreeCalibration in worker-memory-command-driver.ts.
 * calibrationThreadPort is only load scaffolding; thread termination cannot
 * establish command-tree cancellation acceptance.
 */
export async function calibrationLoad(plan: Plan, ports: {
  now: () => number; sample: () => Sample; signal?: AbortSignal;
  producer: (index: 0 | 1, plan: Plan, signal: AbortSignal) => Promise<Producer>;
}): Promise<readonly Available[]> {
  if (![1024 * MiB, 2048 * MiB].includes(plan.limitBytes) || plan.ceilingBytes !== plan.limitBytes / 2
    || plan.chunkBytes !== MiB || plan.rounds !== (plan.limitBytes === 1024 * MiB ? 16 : 32)
    || plan.allocationBytes !== 8 * plan.rounds * plan.chunkBytes
    || !Number.isSafeInteger(plan.baselineBytes) || plan.baselineBytes < 0
    || !Number.isSafeInteger(plan.overheadBytes) || plan.overheadBytes < 64 * MiB
    || plan.baselineBytes + plan.allocationBytes + plan.overheadBytes > plan.ceilingBytes
    || !Number.isInteger(plan.windowMs) || plan.windowMs < 1 || plan.windowMs > 30000) {
    throw new Error("invalid bounded calibration plan");
  }
  const controller = new AbortController(), producers: Producer[] = [], samples: Available[] = [];
  const started = ports.now();
  if (!finite(started)) throw new Error("invalid calibration clock");
  const abort = () => controller.abort();
  ports.signal?.addEventListener("abort", abort, { once: true });
  const timer = setTimeout(() => controller.abort(), plan.windowMs);
  const interrupted = new Promise<never>((_, reject) => {
    controller.signal.addEventListener("abort", () => reject(new Error(ports.signal?.aborted
      ? "calibration cancelled" : "calibration window expired")), { once: true });
  });
  void interrupted.catch(() => {});
  const sample = (remainingBytes = 0) => {
    const now = ports.now();
    if (!finite(now) || now < started || controller.signal.aborted || now - started >= plan.windowMs) throw new Error("calibration window expired");
    const value = ports.sample();
    if (!value.available || value.limitBytes !== plan.limitBytes || value.currentBytes + remainingBytes + plan.overheadBytes > plan.ceilingBytes) {
      throw new Error("calibration sample unavailable or safety ceiling reached");
    }
    samples.push(value);
    validateSamples(samples, 1);
  };
  const starts: Promise<Producer>[] = [], steps: Promise<void>[] = [];
  let failure: { error: unknown } | undefined;
  let cleanupFailures: unknown[] = [];
  try {
    if (ports.signal?.aborted) { abort(); await interrupted; }
    // Hard sample prerequisite before starting any producer.
    sample(plan.allocationBytes);
    for (const index of [0, 1] as const) starts.push((async () => {
      const producer = await ports.producer(index, plan, controller.signal);
      producers.push(producer);
      return producer;
    })());
    const pair = await Promise.race([Promise.all(starts), interrupted]);
    for (let round = 0; round < plan.rounds; round++) {
      sample(8 * (plan.rounds - round) * plan.chunkBytes);
      const roundSteps = pair.map((producer) => Promise.resolve().then(() => {
        if (controller.signal.aborted) throw new Error("calibration cancelled");
        return producer.step();
      }));
      steps.push(...roundSteps); // <=64 step promises across the two producers
      await Promise.race([Promise.all(roundSteps), interrupted]);
      sample();
    }
  } catch (error) {
    failure = { error };
  } finally {
    clearTimeout(timer);
    ports.signal?.removeEventListener("abort", abort);
    controller.abort();
    await Promise.allSettled(starts);
    const stops = await Promise.allSettled(producers.map((producer) => Promise.resolve().then(() => producer.stop())));
    await Promise.allSettled(steps);
    cleanupFailures = stops.flatMap((result) => result.status === "rejected" ? [result.reason] : []);
  }
  if (cleanupFailures.length) throw new AggregateError([
    ...(failure ? [failure.error] : []), ...cleanupFailures,
  ], "calibration cleanup failed");
  if (failure) throw failure.error;
  return Object.freeze(samples);
}

/** Real load port for a FUTURE dedicated fixture. Requires distinct disk + tmpfs
 * before any producer starts; no process-table or host-credential inspection.
 * Only two producer threads, <=32 step messages each, four sequential churn
 * threads each. Ordinary fake tests never invoke it.
 */
export async function calibrationThreadPort(dirs: { disk: string; shmem: string }) {
  const [disk, shmem] = await Promise.all([fs.statfs(dirs.disk), fs.statfs(dirs.shmem)]);
  if (disk.type === 0x01021994 || shmem.type !== 0x01021994) throw new Error("disk and verified tmpfs prerequisites required");
  return async (_index: 0 | 1, plan: Plan, signal: AbortSignal): Promise<Producer> => {
    if (signal.aborted) throw new Error("calibration aborted");
    const diskDir = await fs.mkdtemp(path.join(dirs.disk, "memory-load-"));
    let shmemDir: string;
    try { shmemDir = await fs.mkdtemp(path.join(dirs.shmem, "memory-load-")); }
    catch (error) { await fs.rm(diskDir, { recursive: true, force: true }); throw error; }
    let worker: Worker;
    try {
      worker = new Worker(new URL("../fixtures/worker-memory-load.cjs", import.meta.url), {
        workerData: { diskDir, shmemDir, rounds: plan.rounds, chunkBytes: plan.chunkBytes },
        resourceLimits: { maxOldGenerationSizeMb: 16, maxYoungGenerationSizeMb: 4, stackSizeMb: 1 },
      });
    } catch (error) {
      await Promise.all([fs.rm(diskDir, { recursive: true, force: true }), fs.rm(shmemDir, { recursive: true, force: true })]);
      throw error;
    }
    let stop: Promise<void> | undefined, failed = false;
    const pending = new Set<{ resolve: () => void; reject: (error: Error) => void }>();
    const fail = () => {
      failed = true;
      for (const wait of pending) wait.reject(new Error("calibration producer stopped"));
      pending.clear();
    };
    worker.on("message", () => { for (const wait of pending) wait.resolve(); pending.clear(); });
    worker.on("error", fail); worker.on("exit", fail);
    const shutdown = (): Promise<void> => stop ??= (async () => {
      fail();
      await worker.terminate();
      await Promise.all([fs.rm(diskDir, { recursive: true, force: true }), fs.rm(shmemDir, { recursive: true, force: true })]);
    })();
    const abort = () => { void shutdown().catch(() => {}); };
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) { await shutdown(); throw new Error("calibration aborted"); }
    return {
      step: () => new Promise<void>((resolve, reject) => {
        if (stop || failed || pending.size) { reject(new Error("calibration producer closed or busy")); return; }
        pending.add({ resolve, reject }); worker.postMessage("step");
      }),
      stop: async () => { signal.removeEventListener("abort", abort); await shutdown(); },
    };
  };
}

/** Correlated response evidence uses one monotonic clock (ms). Synthetic
 * fixtures exercise arithmetic and never satisfy measurement acceptance. */
export type CalibrationBinding = Readonly<{
  reportId: string; legId: string; interventionId: string; treeId: string; toolId: string; reservationId: string;
}>;
export type CalibrationSpan = Readonly<{ binding: CalibrationBinding; startMs: number; endMs: number }>;
export type CalibrationLegResponse = Readonly<{
  binding: CalibrationBinding; hardThresholdBytes: number;
  hardBracket: { lastBelowSampleId: number; firstAtOrAboveSampleId: number; lowerMs: number; upperMs: number };
  spans: Record<"observe" | "reservation" | "signal" | "drain" | "cleanup", CalibrationSpan>;
  earlyPsiObservation?: CalibrationSpan;
  ack: { binding: CalibrationBinding; atMs: number; accepted: true };
  exit: { binding: CalibrationBinding; atMs: number; confirmed: true };
  cleanup: { binding: CalibrationBinding; confirmed: true };
  oomKillsBefore: number; oomKillsAfter: number;
  oomBefore: number; oomAfter: number;
  oomGroupKillsBefore: number; oomGroupKillsAfter: number;
}>;
export type CalibrationMeasuredResponse = Readonly<{
  provenance: "measured" | "synthetic"; reportId: string; legs: readonly CalibrationLegResponse[];
}>;
export type CalibrationReportInput = Readonly<{
  baseline: readonly Available[]; samples: readonly Available[];
  reserveBytes: number; response: CalibrationMeasuredResponse;
}>;
export type CalibrationSchedulingObservation = Readonly<{
  sampleId: number; requestedOrdinal: number; dueAtMs: number; dispatchAtMs: number;
}>;
export type CalibrationSchedulingInput = Readonly<{
  periodMs: number; anchorMs: number; observations: readonly CalibrationSchedulingObservation[];
}>;
/** Test-only periodic reads, excluding manual probes. Ordinals are contiguous from
 * zero; producer and checker compute dueAtMs as anchorMs + ordinal * periodMs.
 * At most 256 rows, no retries; any missing/corrupt row fails the whole report.
 * Scheduling milliseconds are evidence only, never reserve-formula byte jitter. */
export function calibrationSchedulingReport(samples: readonly Available[], input: CalibrationSchedulingInput) {
  validateSamples(samples, 1);
  if (!Number.isSafeInteger(input.periodMs) || input.periodMs <= 0 || !finite(input.anchorMs)
    || input.observations.length !== samples.length) throw new Error("invalid calibration scheduling input");
  const rows = samples.map((sample, index) => {
    const observation = input.observations[index];
    const dueAtMs = input.anchorMs + index * input.periodMs;
    if (!observation || observation.sampleId !== sample.sampleId || observation.requestedOrdinal !== index
      || !finite(dueAtMs) || observation.dueAtMs !== dueAtMs || !finite(observation.dispatchAtMs)
      || observation.dispatchAtMs > sample.startedAtMs
      || (index > 0 && observation.dispatchAtMs < samples[index - 1]!.completedAtMs)) {
      throw new Error("missing, misordered or mismatched scheduling observation");
    }
    return { ...observation, startedAtMs: sample.startedAtMs, completedAtMs: sample.completedAtMs,
      latenessMs: sample.startedAtMs - dueAtMs };
  });
  return { periodMs: input.periodMs, anchorMs: input.anchorMs, rows,
    maxPositiveLatenessMs: Math.max(0, ...rows.map((row) => row.latenessMs)),
    maxAbsoluteJitterMs: Math.max(...rows.map((row) => Math.abs(row.latenessMs))) };
}
export type CalibrationSamplePair = Readonly<{
  fromSampleId: number; toSampleId: number; rawDeltaBytes: number;
  elapsedMs: number; positiveRateBytesPerMs: number; readGapMs: number;
}>;
const bindingKeys = ["reportId", "legId", "interventionId", "treeId", "toolId", "reservationId"] as const;
function sameBinding(a: CalibrationBinding, b: CalibrationBinding): boolean {
  return !!a && !!b && bindingKeys.every((key) => typeof a[key] === "string" && a[key].length > 0 && a[key] === b[key]);
}
export function calibrationReport(input: CalibrationReportInput) {
  validateSamples(input.baseline); validateSamples(input.samples);
  const response = input.response;
  if (!response || !["measured", "synthetic"].includes(response.provenance) || !response.reportId
    || !response.legs?.length || response.legs.length > 32
    || ![1024 * MiB, 2048 * MiB].includes(input.samples[0]!.limitBytes)
    || input.samples[0]!.sampleId <= input.baseline.at(-1)!.sampleId
    || input.samples[0]!.limitBytes !== input.baseline[0]!.limitBytes
    || input.samples[0]!.startedAtMs < input.baseline.at(-1)!.completedAtMs
    || !Number.isSafeInteger(input.reserveBytes) || input.reserveBytes <= 0
    || input.reserveBytes >= input.samples[0]!.limitBytes) throw new Error("invalid calibration report");
  // Strict comparisons retain the earliest sample/pair on ties.
  const min = input.baseline.reduce((a, b) => b.currentBytes < a.currentBytes ? b : a);
  const max = input.baseline.reduce((a, b) => b.currentBytes > a.currentBytes ? b : a);
  const baselineJitter = { minSampleId: min.sampleId, minBytes: min.currentBytes,
    maxSampleId: max.sampleId, maxBytes: max.currentBytes };
  const jitterBytes = max.currentBytes - min.currentBytes;
  const samplePairs: CalibrationSamplePair[] = input.samples.slice(1).map((current, index) => {
    const previous = input.samples[index]!;
    const elapsedMs = current.completedAtMs - previous.completedAtMs;
    if (elapsedMs <= 0) throw new Error("nonmonotonic calibration time");
    const rawDeltaBytes = current.currentBytes - previous.currentBytes;
    return { fromSampleId: previous.sampleId, toSampleId: current.sampleId, rawDeltaBytes, elapsedMs,
      positiveRateBytesPerMs: Math.max(0, rawDeltaBytes) / elapsedMs,
      readGapMs: current.startedAtMs - previous.completedAtMs };
  });
  const peakPositiveGrowthPair = samplePairs.reduce((a, b) => b.positiveRateBytesPerMs > a.positiveRateBytesPerMs ? b : a);
  const maxSampleGapPair = samplePairs.reduce((a, b) => b.readGapMs > a.readGapMs ? b : a);
  const peakPositiveGrowthRateBytesPerMs = peakPositiveGrowthPair.positiveRateBytesPerMs;
  const maxSampleGapMs = maxSampleGapPair.readGapMs;
  const seen = new Set<string>();
  let previousCleanupEndMs = input.baseline.at(-1)!.completedAtMs;
  const legs = response.legs.map((leg) => {
    const b = leg.binding, bracket = leg.hardBracket, spans = leg.spans;
    if (!sameBinding(b, b) || b.reportId !== response.reportId || seen.has(b.legId)
      || !Number.isSafeInteger(leg.hardThresholdBytes) || leg.hardThresholdBytes <= 0
      || leg.hardThresholdBytes !== input.samples[0]!.limitBytes - input.reserveBytes) throw new Error("invalid leg binding or threshold");
    seen.add(b.legId);
    const highIndex = input.samples.findIndex((sample) => sample.startedAtMs >= previousCleanupEndMs
      && sample.currentBytes >= leg.hardThresholdBytes);
    const low = input.samples[highIndex - 1], high = input.samples[highIndex];
    if (!low || !high || !bracket || low.currentBytes >= leg.hardThresholdBytes
      || bracket.lastBelowSampleId !== low.sampleId || bracket.firstAtOrAboveSampleId !== high.sampleId
      || bracket.lowerMs !== low.startedAtMs || bracket.upperMs !== high.completedAtMs) {
      throw new Error("missing or mismatched hard crossing bracket");
    }
    if (bracket.lowerMs < previousCleanupEndMs) throw new Error("misordered calibration legs");
    const earlyPsi = leg.earlyPsiObservation;
    if (earlyPsi && (!sameBinding(b, earlyPsi.binding) || !finite(earlyPsi.startMs) || !finite(earlyPsi.endMs)
      || earlyPsi.endMs < earlyPsi.startMs || earlyPsi.endMs > bracket.lowerMs)) throw new Error("invalid early PSI observation");
    let end = bracket.upperMs;
    for (const kind of ["observe", "reservation", "signal", "drain", "cleanup"] as const) {
      const span = spans?.[kind];
      if (!span || !sameBinding(b, span.binding) || !finite(span.startMs) || !finite(span.endMs)
        || span.startMs < end || span.endMs < span.startMs) throw new Error("missing, misordered or mismatched response span");
      end = span.endMs;
    }
    if (!leg.ack || !sameBinding(b, leg.ack.binding) || leg.ack.accepted !== true
      || !finite(leg.ack.atMs) || leg.ack.atMs !== spans.reservation.endMs || leg.ack.atMs > spans.signal.startMs
      || !leg.exit || !sameBinding(b, leg.exit.binding) || leg.exit.confirmed !== true
      || !finite(leg.exit.atMs) || leg.exit.atMs < spans.drain.startMs || leg.exit.atMs !== spans.drain.endMs
      || !leg.cleanup || !sameBinding(b, leg.cleanup.binding) || leg.cleanup.confirmed !== true
      || [[leg.oomKillsBefore, leg.oomKillsAfter], [leg.oomBefore, leg.oomAfter],
        [leg.oomGroupKillsBefore, leg.oomGroupKillsAfter]].some(([before, after]) =>
        !Number.isSafeInteger(before) || before! < 0 || !Number.isSafeInteger(after) || after! < 0 || after !== before)) throw new Error("unconfirmed ACK, exit, cleanup or OOM delta");
    previousCleanupEndMs = spans.cleanup.endMs;
    return { binding: b, hardBracket: bracket, earlyPsiObservation: earlyPsi,
      observationToDrainMs: spans.drain.endMs - spans.observe.startMs,
      drainMinusBracketLowerMs: spans.drain.endMs - bracket.lowerMs,
      stageLatenciesMs: {
        crossingToObserveEnd: spans.observe.endMs - bracket.lowerMs,
        reservation: spans.reservation.endMs - spans.observe.endMs,
        signal: spans.signal.endMs - spans.reservation.endMs,
        drain: spans.drain.endMs - spans.signal.endMs,
      } };
  });
  // Bracket lower already includes sampling uncertainty; do not add the sample gap.
  const worstEndToEndLatencyMs = Math.max(...legs.map((leg) => leg.drainMinusBracketLowerMs));
  // Sum of per-stage maxima across legs is a separate conservative estimate;
  // end-to-end acceptance uses the correlated worst single leg above.
  const conservativeStageSumMs = (["crossingToObserveEnd", "reservation", "signal", "drain"] as const)
    .reduce((sum, kind) => sum + Math.max(...legs.map((leg) => leg.stageLatenciesMs[kind])), 0);
  const requiredBytes = peakPositiveGrowthRateBytesPerMs * worstEndToEndLatencyMs + jitterBytes;
  const satisfied = input.reserveBytes > requiredBytes;
  return { fixtureOnly: true, provenance: response.provenance, reportId: response.reportId, legs,
    elapsedMs: input.samples.at(-1)!.completedAtMs - input.samples[0]!.startedAtMs,
    baselineJitter, samplePairs, peakPositiveGrowthPair, maxSampleGapPair,
    jitterBytes, peakPositiveGrowthRateBytesPerMs, maxSampleGapMs, worstEndToEndLatencyMs, conservativeStageSumMs,
    netGrowthBytes: input.samples.at(-1)!.currentBytes - input.samples[0]!.currentBytes,
    monotonicGrowth: input.samples.every((sample, index) => index === 0 || sample.currentBytes >= input.samples[index - 1]!.currentBytes),
    responseMeasured: response.provenance === "measured",
    accepted: response.provenance === "measured" && satisfied,
    reserveCheck: { reserveBytes: input.reserveBytes, requiredBytes, satisfied } };
}
