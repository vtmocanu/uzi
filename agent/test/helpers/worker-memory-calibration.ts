import fs from "node:fs/promises";
import path from "node:path";
import { Worker } from "node:worker_threads";
import type { WorkerMemoryReader } from "../../src/worker-memory-reader.js";

type Sample = ReturnType<WorkerMemoryReader["sample"]>;
type Available = Extract<Sample, { available: true }>;
type Producer = { step: () => Promise<void>; stop: () => Promise<void> };
type Plan = Readonly<{
  limitBytes: number; ceilingBytes: number; baselineBytes: number;
  allocationBytes: number; overheadBytes: number; chunkBytes: number; rounds: number; windowMs: number;
}>;
type Span = Readonly<{ kind: "sample" | "observe" | "reservation" | "signal" | "drain"; startMs: number; endMs: number }>;
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
 * M6 must supply two supervised command-tree producers through this port, with real
 * cancellation/drain evidence. calibrationThreadPort is only a load fixture.
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

/** Missing cancellation spans remain explicitly unmeasured until real M3/M6
 * adapters provide them. The reserve inequality uses an INPUT budget, not a
 * fabricated cancellation observation or a production tuning recommendation.
 */
export function calibrationReport(input: {
  baseline: readonly Available[]; samples: readonly Available[]; spans: readonly Span[];
  reserveBytes: number; responseBudgetMs: number;
}) {
  validateSamples(input.baseline); validateSamples(input.samples);
  if (input.spans.length > 256) throw new Error("calibration latency bound exceeded");
  if (input.samples[0]!.limitBytes !== input.baseline[0]!.limitBytes
    || !Number.isSafeInteger(input.reserveBytes) || input.reserveBytes <= 0
    || !finite(input.responseBudgetMs) || input.responseBudgetMs <= 0) throw new Error("invalid calibration report");
  const baseline = input.baseline.map((sample) => sample.currentBytes);
  const jitterBytes = Math.max(...baseline) - Math.min(...baseline);
  let growthBytesPerMs = 0, maxSampleGapMs = 0;
  for (let i = 1; i < input.samples.length; i++) {
    const previous = input.samples[i - 1]!, current = input.samples[i]!;
    const elapsed = current.completedAtMs - previous.completedAtMs;
    if (elapsed <= 0) throw new Error("nonmonotonic calibration time");
    maxSampleGapMs = Math.max(maxSampleGapMs, current.startedAtMs - previous.completedAtMs);
    growthBytesPerMs = Math.max(growthBytesPerMs, Math.max(0, current.currentBytes - previous.currentBytes) / elapsed);
  }
  const latencies: Record<Span["kind"], number[]> = { sample: [], observe: [], reservation: [], signal: [], drain: [] };
  for (const sample of input.samples) latencies.sample.push(sample.completedAtMs - sample.startedAtMs);
  for (const span of input.spans) {
    if (!finite(span.startMs) || !finite(span.endMs) || span.endMs < span.startMs || !Object.hasOwn(latencies, span.kind)) {
      throw new Error("invalid calibration latency");
    }
    latencies[span.kind].push(span.endMs - span.startMs);
  }
  const requiredBytes = jitterBytes + growthBytesPerMs * (maxSampleGapMs + input.responseBudgetMs);
  return { fixtureOnly: true, elapsedMs: input.samples.at(-1)!.completedAtMs - input.samples[0]!.startedAtMs,
    jitterBytes, growthBytesPerMs, maxSampleGapMs, latencies,
    netGrowthBytes: input.samples.at(-1)!.currentBytes - input.samples[0]!.currentBytes,
    monotonicGrowth: input.samples.every((sample, index) => index === 0 || sample.currentBytes >= input.samples[index - 1]!.currentBytes),
    responseMeasured: (["observe", "reservation", "signal", "drain"] as const).every((kind) => latencies[kind].length > 0),
    reserveCheck: { reserveBytes: input.reserveBytes, requiredBytes, satisfied: input.reserveBytes > requiredBytes } };
}
