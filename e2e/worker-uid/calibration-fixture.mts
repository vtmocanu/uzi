// Real private-cgroup fixture. This entrypoint is launched only by calibration.py.
import assert from "node:assert/strict";
import fs from "node:fs";
import { mkdir, readFile, rename, rm, statfs, writeFile } from "node:fs/promises";
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { performance } from "node:perf_hooks";
import { CodexCallbackBroker, type CapturedMemoryContext } from "../../agent/src/codex/broker.js";
import { makeDefaultSpawnCommand } from "../../agent/src/codex/codex-executor.js";
import { launchCodexEffectRoot, type CodexRootHandle, type SpawnSupervisor } from "../../agent/src/codex/launcher.js";
import { boundedCommandWait } from "../../agent/src/codex/memory-command.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../../agent/src/codex/registry.js";
import { WorkerMemoryCommands } from "../../agent/src/worker-memory-commands.js";
import { WorkerMemoryMonitor } from "../../agent/src/worker-memory-guard.js";
import { WorkerMemoryReader } from "../../agent/src/worker-memory-reader.js";
import type { MemoryReservation, MemoryReservationRequest, WorkerMemoryPressureResult } from "../../agent/src/protocol.js";
import { calibrationPlan, calibrationReport, calibrationSchedulingReport, type CalibrationSchedulingObservation, type CalibrationBinding, type CalibrationLegResponse } from "../../agent/test/helpers/worker-memory-calibration.js";
import { commandTreeCalibration, type CalibrationIntervention, type CommandStep, type EnrolledCalibrationProducer } from "../../agent/test/helpers/worker-memory-command-driver.js";

// Six baseline reads plus load, owned cleanup and fresh quiet dwell at 250ms,
// with two boundary ticks. Evidence is refused at the bound, never truncated.
const MAX_SAMPLES = 6 + Math.ceil((30000 + 30000 + 5000) / 250) + 2;
const RAW_SAMPLE_BYTES = 6144, REPORT_BYTES = 8 * 1024 * 1024;
const root = "/proof", scratch = root + "/.uzi/scratch", disk = "/calibration-disk";
const now = () => performance.now();
type Sample = ReturnType<WorkerMemoryReader["sample"]>;
type Available = Extract<Sample, { available: true }>;
// These fixed primaries never exit normally. The real supervisor disposes them
// with SIGKILL and reports 128 + signal; an error exit is not pressure cancellation.
export function expectedPrimaryExit(disposalRequested: boolean, code: unknown): boolean {
  return disposalRequested && code === 137;
}
function deferred<T>() {
  let resolve!: (v: T) => void, reject!: (e: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  void promise.catch(() => {});
  return { promise, resolve, reject };
}
async function until(predicate: () => Promise<boolean> | boolean, ms: number) {
  const deadline = now() + ms;
  // At most ceil(ms/10) file/owned-handle checks. A failure aborts this wait.
  for (let n = 0; n <= Math.ceil(ms / 10) && now() < deadline; n++) {
    if (await predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error("fixture readiness/step deadline");
}
function readBounded(file: string, maximum: number) {
  const fd = fs.openSync(file, "r");
  try {
    const bytes = Buffer.alloc(maximum + 1);
    let used = 0;
    while (used < bytes.length) {
      const count = fs.readSync(fd, bytes, used, bytes.length - used, null);
      if (!count) break;
      used += count;
    }
    return bytes.subarray(0, used);
  } finally { fs.closeSync(fd); }
}
function counters(raw: string) {
  const entries = raw.trim().split("\n").map((line) => line.split(/\s+/));
  const result: Record<string, number> = {};
  for (const [key, value, extra] of entries) {
    assert.ok(key && value && !extra && /^[0-9]+$/.test(value) && !(key in result));
    result[key] = Number(value);
    assert.ok(Number.isSafeInteger(result[key]));
  }
  return result;
}
export function captureEventCounters(raw: string): { events: Record<string, number> | null; failure: string | null } {
  let events: Record<string, number>;
  try {
    events = counters(raw);
  } catch (error) {
    return { events: null, failure: String(error) };
  }
  // Strict token parsing precedes completeness: retain known counters on refusal.
  for (const key of ["oom", "oom_kill", "oom_group_kill"]) {
    if (!(key in events)) return { events, failure: "missing event " + key };
  }
  return { events, failure: null };
}
export const calibrationUnits = {
  memory: "bytes", monotonicTiming: "milliseconds", growth: "bytes_per_millisecond",
  psiAverages: "percent", psiTotal: "microseconds", events: "count",
  processStartIdentity: "opaque_kernel_start_ticks",
};
export function plannedChargeClasses(plan: ReturnType<typeof calibrationPlan>) {
  assert.ok([16, 32].includes(plan.rounds) && plan.chunkBytes === 1024 ** 2
    && plan.limitBytes === (plan.rounds / 16) * 1024 ** 3
    && plan.allocationBytes === 8 * plan.rounds * plan.chunkBytes, "invalid fixed charge budget");
  return ["touched_anonymous", "dirty_disk", "fsynced_read_file_cache", "tmpfs_shmem"].map((kind) => ({
    kind, producerCount: 2, chunkBytes: plan.chunkBytes, maxRoundsPerProducer: plan.rounds,
    combinedBudgetBytes: 2 * plan.rounds * plan.chunkBytes,
  }));
}
type EventRecord = { sample: { sampleId: number }; events: Partial<Record<string, number>> | null };
export function oomEventEvidence(records: readonly EventRecord[]) {
  const first = records[0], last = records.at(-1);
  if (!first || !last) return null;
  const before: Record<string, number | null> = {}, after: Record<string, number | null> = {};
  const delta: Record<string, number | null> = {};
  for (const key of ["oom", "oom_kill", "oom_group_kill"]) {
    const a = first.events?.[key] ?? null, b = last.events?.[key] ?? null;
    assert.ok([a, b].every((v) => v === null || (Number.isSafeInteger(v) && v >= 0)), "invalid OOM counter");
    before[key] = a; after[key] = b;
    delta[key] = a === null || b === null ? null : b - a;
  }
  return { fromSampleId: first.sample.sampleId, toSampleId: last.sample.sampleId, before, after, delta };
}
type TreeMetadata = Pick<EnrolledCalibrationProducer, "treeId" | "identity" | "enrollment">;

// Serialization only: callers supply captured metadata, never registry entries or contexts.
export function projectFailedEvidence(facts: {
  samplingSchedule?: unknown;
  trees: (TreeMetadata | null)[];
  exits: { index: number; atMs: number; code: number }[];
  disposals: { index: number; startMs: number; endMs: number; clean: boolean; evidence: unknown }[];
  feedback: unknown;
  brokerReplies: unknown[];
  serviceRecords: unknown[];
  acknowledgements: unknown[];
  observations: unknown[];
  signalWrites: unknown[];
  schedule: unknown[];
  survival: unknown;
  selectedIndex: number | undefined;
  binding: CalibrationBinding | undefined;
  monitorState: unknown;
  history: unknown;
  leg: CalibrationLegResponse | undefined;
  reportId: string;
  cleanupStart: number | undefined;
  cleanupEnd: number | undefined;
}) {
  return {
    samplingSchedule: facts.samplingSchedule ?? null,
    trees: [0, 1].map((index) => {
      const tree = facts.trees[index];
      return tree ? { treeId: tree.treeId, identity: tree.identity, enrollment: tree.enrollment,
        exits: facts.exits.filter((e) => e.index === index),
        disposals: facts.disposals.filter((d) => d.index === index) } : null;
    }),
    settlement: {
      feedback: facts.feedback, broker: facts.selectedIndex === undefined ? null : facts.brokerReplies[facts.selectedIndex] ?? null,
      brokerReplies: facts.brokerReplies, serviceRecords: facts.serviceRecords, acknowledgements: facts.acknowledgements,
      observations: facts.observations, signalWrites: facts.signalWrites, schedule: facts.schedule,
      survival: facts.survival, selectedIndex: facts.selectedIndex ?? null, binding: facts.binding ?? null,
      monitorState: facts.monitorState, history: facts.history, exits: facts.exits, disposals: facts.disposals,
    },
    response: facts.leg ? { provenance: "measured", reportId: facts.reportId, legs: [facts.leg] } : null,
    cleanup: { confirmed: null, startMs: facts.cleanupStart ?? null, endMs: facts.cleanupEnd ?? null },
  };
}
// Bounded by MAX_SAMPLES at the caller. Invalid evidence refuses completion;
// a high sample resets dwell, while a gap rejects the remaining contiguous run.
export function freshPostCleanupDwell(samples: Sample[], cleanupEnd: number, atMs: number, guard: {
  reserveBytes: number; hysteresisBytes: number; sampleMs: number; responseBudgetMs: number; rearmMs: number;
}, limitBytes: number): boolean {
  const maximumGap = guard.sampleMs + guard.responseBudgetMs;
  const quiet = limitBytes - guard.reserveBytes - guard.hysteresisBytes;
  if (!Number.isFinite(cleanupEnd) || !Number.isFinite(atMs) || atMs < cleanupEnd
    || !Number.isSafeInteger(limitBytes) || limitBytes <= 0 || quiet <= 0) return false;
  let first: Sample | undefined, previous: Sample | undefined, count = 0;
  let contiguous = true;
  for (const value of samples) {
    if (value.startedAtMs <= cleanupEnd) continue;
    if (!value.available || value.limitBytes !== limitBytes
      || !Number.isSafeInteger(value.currentBytes) || value.currentBytes < 0
      || !Number.isSafeInteger(value.sampleId) || value.sampleId <= 0
      || !Number.isFinite(value.startedAtMs) || !Number.isFinite(value.completedAtMs)
      || value.completedAtMs < value.startedAtMs || value.completedAtMs > atMs
      || value.completedAtMs - value.startedAtMs > maximumGap
      || (previous && (value.sampleId <= previous.sampleId || value.startedAtMs < previous.completedAtMs))) return false;
    if (value.currentBytes >= quiet) { first = undefined; count = 0; contiguous = true; }
    else {
      if (previous && first && value.startedAtMs - previous.completedAtMs > maximumGap) contiguous = false;
      first ??= value;
      count++;
    }
    previous = value;
  }
  return !!first && !!previous && contiguous && count >= 3
    && previous.completedAtMs - first.completedAtMs >= guard.rearmMs
    && atMs - previous.startedAtMs <= maximumGap;
}
export async function fixture(manifestFile: string, fixtureId: string) {
  const manifest = JSON.parse(await readFile(manifestFile, "utf8"));
  const expected = manifest.fixtures.find((item: { fixtureId: string }) => item.fixtureId === fixtureId);
  assert.ok(expected, "host-bound fixture");
  const report: Record<string, unknown> = {
    schemaVersion: 1, ...manifest.identity, ...expected, provenance: "measured",
    status: "FAILED", reasons: [], observed: null, guard: null, plan: null,
    units: calibrationUnits, plannedChargeClasses: null, oomEvents: null, rawSamples: [], finalCleanup: null,
    samplingSchedule: { periodMs: 250, anchorMs: null, rows: [], maxPositiveLatenessMs: null, maxAbsoluteJitterMs: null },
    baseline: [], samples: [], response: null, derivation: null, trees: [],
    settlement: null, cleanup: null, outcome: { status: "failed", reasons: ["not settled"] },
    clock: { source: "node:performance", unit: "ms", timeOrigin: performance.timeOrigin },
    bounds: { maxSamples: MAX_SAMPLES, rawSampleBytes: RAW_SAMPLE_BYTES, reportBytes: REPORT_BYTES,
      samplePeriodMs: 250, loadMs: 30000, cleanupMs: 30000 },
    signalMeaning: "application supervisor disposal control write", ackService: "LOCALFIXTURE",
  };
  const reasons: string[] = [];
  const rawSamples: { sample: Sample; raw: Record<string, string | null>; events: Record<string, number> | null }[] = [];
  const baseline: Available[] = [], samples: Available[] = [];
  const schedulingObservations: CalibrationSchedulingObservation[] = [];
  let schedulingAnchor: number | null = null;
  let samplingSchedule: ReturnType<typeof calibrationSchedulingReport> | undefined;
  let requested: Omit<CalibrationSchedulingObservation, "sampleId"> | undefined;
  let raw: Record<string, string | null> = {}, rawBytes = 0;
  const reader = new WorkerMemoryReader({ readBytes: (file, max) => {
    try {
      const bytes = readBounded(file, max);
      rawBytes += bytes.length;
      if (rawBytes > RAW_SAMPLE_BYTES) throw new Error("raw sample byte bound exceeded");
      raw[file] = new TextDecoder("utf8", { fatal: true }).decode(bytes);
      return bytes; // Identical default paths, budget, bytes and production parsing.
    } catch (error) { raw[file] = null; throw error; }
  } });
  function sample() {
    if (rawSamples.length >= MAX_SAMPLES) throw new Error("sample bound exceeded");
    raw = {}; rawBytes = 0;
    const value = reader.sample();
    // Capture real reader identity/times even if availability or event checks fail.
    if (requested) schedulingObservations.push({ ...requested, sampleId: value.sampleId });
    let events: Record<string, number> | null = null, eventFailure: string | null = null;
    try {
      const bytes = readBounded("/sys/fs/cgroup/memory.events", RAW_SAMPLE_BYTES - rawBytes);
      rawBytes += bytes.length;
      if (rawBytes > RAW_SAMPLE_BYTES) throw new Error("raw events byte bound exceeded");
      raw["/sys/fs/cgroup/memory.events"] = bytes.toString("utf8");
      ({ events, failure: eventFailure } = captureEventCounters(bytes.toString("utf8")));
    } catch (error) { eventFailure = String(error); }
    if (eventFailure) reasons.push("events unavailable: " + eventFailure);
    rawSamples.push({ sample: value, raw, events });
    if (!value.available || !events || eventFailure) throw new Error("actual default reader unavailable: " + JSON.stringify(value));
    assert.equal(value.limitBytes, expected.limitBytes);
    return value;
  }
  const commands = new WorkerMemoryCommands();
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const handles: CodexRootHandle[] = [], expectedExit = [false, false];
  const failures = [deferred<never>(), deferred<never>()];
  const replies: Promise<Awaited<ReturnType<CodexCallbackBroker["handleToolCall"]>>>[] = [];
  const aborts = [new AbortController(), new AbortController()];
  const observations: { index: number; startMs: number; endMs: number; observation: unknown }[] = [];
  const signalWrites: { index: number; startMs: number; endMs: number }[] = [];
  const exits: { index: number; atMs: number; code: number }[] = [];
  const disposals: { index: number; startMs: number; endMs: number; clean: boolean; evidence: unknown }[] = [];
  const dirs = [0, 1].map((n) => ({ control: scratch + "/control-" + n, disk: disk + "/tree-" + n, shmem: scratch + "/tree-" + n }));
  let monitor: WorkerMemoryMonitor | undefined, interval: ReturnType<typeof setInterval> | undefined;
  let service: ReturnType<typeof createServer> | undefined;
  let cleanupStart: number | undefined, cleanupEnd: number | undefined, selectedIndex: number | undefined;
  let binding: CalibrationBinding | undefined, leg: CalibrationLegResponse | undefined;
  const intervention = deferred<CalibrationIntervention>(), feedback = deferred<WorkerMemoryPressureResult>();
  const drain = deferred<{ binding: CalibrationBinding; identity: { pid: number; startIdentity: string }; confirmed: true }>();
  const serviceRecords: unknown[] = [];
  const acknowledgements: { path: string; startMs: number; endMs: number; status: number; ack: MemoryReservation }[] = [];
  const treeMetadata: (TreeMetadata | null)[] = [null, null];
  const brokerReplies: unknown[] = [null, null];
  const schedule: unknown[] = [];
  let survival: unknown = null, actualFeedback: WorkerMemoryPressureResult | null = null;
  let fatal: unknown, driverCompleted = false;
  try {
    assert.equal(process.platform, "linux");
    assert.equal(process.getuid?.(), 10003);
    const status = await readFile("/proc/self/status", "utf8");
    for (const name of ["CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"]) {
      const value = status.match(new RegExp("^" + name + ":\\s+([0-9a-f]+)$", "m"))?.[1];
      assert.ok(value); assert.equal(BigInt("0x" + value), 0n);
    }
    assert.match(status, /^NoNewPrivs:\s+1$/m);
    assert.equal(await readFile("/proc/self/cgroup", "utf8"), "0::/\n");
    assert.notEqual((await statfs(disk)).type, 0x01021994);
    assert.equal((await statfs(scratch)).type, 0x01021994);
    report.observed = { uid: 10003, noNewPrivs: true, capsZero: true, membership: "0::/",
      limitBytes: Number((await readFile("/sys/fs/cgroup/memory.max", "utf8")).trim()),
      diskType: (await statfs(disk)).type, shmemType: (await statfs(scratch)).type };
    for (let i = 0; i < 3; i++) { baseline.push(sample()); await new Promise((resolve) => setTimeout(resolve, 250)); }
    // Prerequisites and full fixed budget are verified BEFORE either command tree starts.
    let plan = calibrationPlan({ dedicated: true, baseline, windowMs: 30000 });
    report.plan = plan;
    report.plannedChargeClasses = plannedChargeClasses(plan);
    let high = plan.baselineBytes + 32 * 1024 ** 2;
    const guard = { enabled: true as const, reserveBytes: plan.limitBytes - high, hysteresisBytes: 8 * 1024 ** 2,
      sampleMs: 250, responseBudgetMs: 5000, rearmMs: 500, maxInterventions: 2 };
    assert.ok(0 < high - guard.hysteresisBytes && high < plan.limitBytes / 2);
    report.guard = guard;
    const flight = {};
    const context = commands.begin({ ...manifest.context, flight, lifecycleRevision: 1,
      memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: 0 });
    const captured: CapturedMemoryContext = { commands, context, flight, lifecycleRevision: 1 };
    let reserveRequest: MemoryReservationRequest | undefined;
    service = createServer((req, res) => {
      let body = "", used = 0;
      const timer = setTimeout(() => req.destroy(new Error("LOCALFIXTURE request timeout")), 1000);
      req.on("data", (chunk: Buffer) => { used += chunk.length; if (used > 4096) req.destroy(); else body += chunk; });
      req.on("end", () => {
        clearTimeout(timer);
        try {
          assert.equal(req.method, "POST");
          assert.ok(req.url === "/reservation" || req.url === "/outcome");
          const input = JSON.parse(body);
          const base = { ...input }; delete base.outcome;
          for (const key of ["run_id", "worker_id", "register_nonce", "claim_generation", "memory_episode"]) {
            assert.equal(base[key], manifest.context[key]);
          }
          assert.deepEqual(base.policy, { version: 1, max_interventions: 2 });
          assert.match(base.intervention_id, /^[0-9a-f-]{36}$/);
          if (reserveRequest) assert.deepEqual(base, reserveRequest); else reserveRequest = base;
          if (req.url === "/outcome") assert.equal(input.outcome, "confirmed_drained");
          const ack = { ...base, admitted: true, authorizing: req.url === "/reservation",
            policy: { version: 1, max_interventions: 2 }, allowance: { limit: 2, used: 1, remaining: 1 },
            ...(req.url === "/outcome" ? { outcome: input.outcome } : {}) };
          serviceRecords.push({ request: input, ack, path: req.url, atMs: now() });
          res.writeHead(200, { "Content-Type": "application/json" }); res.end(JSON.stringify(ack));
        } catch { res.writeHead(400); res.end("{}"); }
      });
      req.on("error", () => clearTimeout(timer));
      req.on("close", () => clearTimeout(timer));
    });
    await new Promise<void>((resolve) => service!.listen(0, "127.0.0.1", resolve));
    const address = service.address(); assert.ok(address && typeof address !== "string");
    const serviceUrl = "http://127.0.0.1:" + address.port;
    async function request(route: string, input: unknown, signal: AbortSignal) {
      const startMs = now();
      const response = await fetch(serviceUrl + route, { method: "POST", body: JSON.stringify(input), signal });
      assert.equal(response.status, 200);
      const ack = await response.json() as MemoryReservation;
      acknowledgements.push({ path: route, startMs, endMs: now(), status: response.status, ack });
      return ack;
    }
    const launch = async (spec: Parameters<typeof launchCodexEffectRoot>[0]) => {
      const index = handles.length; assert.ok(index < 2);
      const spawnSupervisor: SpawnSupervisor = (command, args, options) => {
        const child = spawn(command, [...args], { ...options, stdio: [...options.stdio] });
        const control = child.stdio[3]; assert.ok(control && "write" in control);
        const write = control.write.bind(control);
        control.write = ((...args: Parameters<typeof control.write>) => {
          const message = JSON.parse(String(args[0]));
          const startMs = now();
          const result = write(...args);
          if (message.op === "dispose") signalWrites.push({ index, startMs, endMs: now() });
          return result;
        }) as typeof control.write;
        return child;
      };
      // Same executed M5a capless profile; no claim of a UID transition.
      const handle = await launchCodexEffectRoot({ ...spec, command: "/usr/local/bin/uzi-codex-command-sandbox" }, {
        env: { UZI_UID_SPLIT: "1" }, spawnSupervisor,
        helperBinsForTest: { supervisor: "/usr/local/bin/uzi-codex-supervisor", crossCheckSandbox: "" },
        deadlines: { started: 5000 },
      });
      handles.push(handle);
      void handle.whenFailed.then((error) => failures[index]!.reject(error), (error) => failures[index]!.reject(error));
      const exit = handle.waitChild(70000);
      void exit.then((value) => {
        exits.push({ index, atMs: now(), code: value.code });
        if (!expectedPrimaryExit(expectedExit[index]!, value.code)) {
          failures[index]!.reject(new Error("unexpected command primary exit"));
        }
      }, (error) => failures[index]!.reject(error));
      return { ...handle, get failed() { return handle.failed; },
        observe: async (ms?: number) => {
          await until(async () => fs.existsSync(dirs[index]!.control + ".ready"), 5000);
          const startMs = now(), value = await handle.observe(ms);
          observations.push({ index, startMs, endMs: now(), observation: value });
          return value;
        },
        dispose: async (ms?: number) => {
          // Arm expected exit before invoking the REAL cancel/ordinary cleanup.
          expectedExit[index] = true;
          const startMs = now(), value = await handle.dispose(ms);
          disposals.push({ index, startMs, endMs: now(), clean: value.clean, evidence: value });
          if (!value.clean) failures[index]!.reject(new Error("unconfirmed disposal"));
          return value;
        },
        waitChild: () => exit,
      };
    };
    const spawnCommand = makeDefaultSpawnCommand(registry, launch, 5000, root,
      { PATH: "/usr/local/bin:/usr/bin:/bin" }, "off", undefined, undefined, 70000, undefined, captured);
    const broker = new CodexCallbackBroker({ registry, capturedMemory: captured, worktreePath: root, spawnCommand,
      grants: { role: "coder", phase: "implement", isRoot: true, allowedTools: new Set(["Bash"]), allowedSkills: new Set() },
      fileop: { op: async () => ({ ok: false }) }, delegate: async () => ({ ok: false }) });
    for (let index = 0; index < 2; index++) {
      const dir = dirs[index]!;
      await mkdir(dir.disk, { recursive: true }); await mkdir(dir.shmem, { recursive: true });
      const quote = (v: string) => "'" + v.replaceAll("'", "'\\''") + "'";
      const command = [process.execPath, "--import", "/proof/agent/node_modules/tsx/dist/loader.mjs",
        "/proof/e2e/worker-uid/calibration-producer.mts", dir.control, dir.disk, dir.shmem, String(plan.rounds)].map(quote).join(" ");
      const reply = broker.handleToolCall({ threadId: expected.fixtureId, turnId: expected.legId,
        callId: "calibration-" + index }, "Bash", { command }, "root");
      void reply.then((value) => { brokerReplies[index] = value; }, (error) => {
        brokerReplies[index] = { failure: String(error) };
        failures[index]!.reject(error);
      });
      replies.push(reply);
      await until(() => handles.length === index + 1 && commands.eligibleEntries()?.length === index + 1, 5000);
      const entry = commands.eligibleEntries()![index]!;
      treeMetadata[index] = { treeId: expected.treeIds[index],
        identity: { pid: entry.primary!.pid, startIdentity: entry.primary!.startTime },
        enrollment: { run_id: context.run_id, worker_id: context.worker_id, claim_generation: context.claim_generation,
          memory_episode: context.memory_episode, attempt_id: entry.attemptId, command_id: entry.commandId,
          tool_id: entry.toolId, command: entry.command } };
    }
    // Idle enrolled primaries are part of the pre-load baseline and full budget.
    baseline.length = 0;
    for (let i = 0; i < 3; i++) { baseline.push(sample()); await new Promise((resolve) => setTimeout(resolve, 250)); }
    plan = calibrationPlan({ dedicated: true, baseline, windowMs: 30000 });
    high = plan.baselineBytes + 32 * 1024 ** 2;
    guard.reserveBytes = plan.limitBytes - high;
    assert.ok(0 < high - guard.hysteresisBytes && high < plan.ceilingBytes);
    report.plan = plan; report.guard = guard;
    report.plannedChargeClasses = plannedChargeClasses(plan);
    const entries = commands.eligibleEntries()!; assert.equal(entries.length, 2);
    const producerPair = entries.map((_entry, index): EnrolledCalibrationProducer => {
      const metadata = treeMetadata[index]!;
      let step = 0;
      return { ...metadata,
        unexpectedFailure: failures[index]!.promise,
        step: async () => {
          const current = ++step;
          const startMs = now();
          const scheduled: { index: number; step: number; startMs: number; endMs: number | null; kind: string | null } =
            { index, step: current, startMs, endMs: null, kind: null };
          schedule.push(scheduled);
          let cancelled: WorkerMemoryPressureResult | undefined;
          // No producer IO is abandoned: the file waiter itself joins cancellation
          // only after exact actual tool feedback and confirmed selected drain.
          const cancellation = feedback.promise.then(async (value) => {
            if (index !== selectedIndex) return;
            await drain.promise;
            const reply = await replies[index]!;
            assert.equal(reply.ok, false);
            if (reply.ok) throw new Error("missing selected tool failure");
            assert.deepEqual(reply.memoryPressure, value);
            cancelled = value;
          });
          void cancellation.catch((error) => failures[index]!.reject(error));
          await writeFile(dirs[index]!.control + ".next", JSON.stringify({ step: current }));
          await rename(dirs[index]!.control + ".next", dirs[index]!.control);
          await until(async () => {
            if (cancelled) return true;
            try {
              const ack = JSON.parse(await readFile(dirs[index]!.control + ".ack", "utf8"));
              if (ack.step === current - 1) {
                assert.equal(Object.keys(ack).length, 1);
                return false;
              }
              assert.equal(ack.step, current, "producer ACK must match the in-flight step");
              return now() - startMs >= 500;
            } catch (error) {
              if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
              throw error;
            }
          }, 5000);
          if (selectedIndex === index) {
            await boundedCommandWait(cancellation, 5000);
            assert.ok(cancelled, "selected in-flight step joins exact cancellation");
          }
          const result: CommandStep = cancelled ? { kind: "cancelled", feedback: cancelled } : { kind: "completed" };
          scheduled.endMs = now(); scheduled.kind = result.kind;
          return result;
        },
        proveLive: async (saved) => {
          const startMs = now(), observation = await handles[index]!.observe(5000);
          observations.push({ index, startMs, endMs: now(), observation });
          assert.equal(observation.state, "complete");
          if (observation.state !== "complete") throw new Error("sibling proof unavailable");
          assert.deepEqual(observation.root, { pid: saved.pid, startTime: saved.startIdentity });
          assert.ok(observation.processes.some((p) => p.pid === saved.pid && p.startTime === saved.startIdentity));
          survival = { index, identity: saved, atMs: now(), observation };
          return saved;
        },
        stop: async () => {
          cleanupStart ??= now(); expectedExit[index] = true; aborts[index]!.abort();
          const startMs = now(), value = await handles[index]!.dispose(5000);
          disposals.push({ index, startMs, endMs: now(), clean: value.clean, evidence: value });
          assert.equal(value.clean, true);
          await boundedCommandWait(replies[index]!, 5000);
          await Promise.all([rm(dirs[index]!.disk, { recursive: true }), rm(dirs[index]!.shmem, { recursive: true })]);
        },
        forceStop: async () => {
          const startMs = now(), value = await handles[index]!.dispose(5000);
          disposals.push({ index, startMs, endMs: now(), clean: value.clean, evidence: value });
          assert.equal(value.clean, true);
        },
      };
    }) as [EnrolledCalibrationProducer, EnrolledCalibrationProducer];
    const unsubscribe = commands.subscribeConfirmedDrain((entry) => {
      const index = entries.indexOf(entry);
      if (index !== selectedIndex || !binding) return;
      drain.resolve({ binding, identity: producerPair[index]!.identity, confirmed: true });
    });
    monitor = new WorkerMemoryMonitor(guard, {
      now, commands, incarnation: () => ({ worker_id: context.worker_id, register_nonce: context.register_nonce, revision: 1 }),
      sample: () => {
        const value = sample(); samples.push(value);
        if (value.currentBytes + plan.overheadBytes > plan.ceilingBytes) throw new Error("safety ceiling reached");
        return value;
      },
      reserve: async (input, signal) => {
        const index = entries.findIndex((entry) => commands.outcome(entry) !== undefined);
        assert.ok(index === 0 || index === 1);
        selectedIndex = index;
        binding = { reportId: expected.reportId, legId: expected.legId, interventionId: input.intervention_id,
          treeId: producerPair[index]!.treeId, toolId: entries[index]!.toolId, reservationId: input.intervention_id };
        const actualFeedback = feedback.promise.then(async (result) => {
          const reply = await replies[index]!;
          assert.equal(reply.ok, false);
          if (reply.ok) throw new Error("selected broker returned success");
          assert.deepEqual(reply.memoryPressure, result);
          return result;
        });
        intervention.resolve({ binding, selected: index as 0 | 1, feedback: actualFeedback, confirmedDrain: drain.promise });
        const ack = await request("/reservation", input, signal);
        return ack;
      },
      report: async (input, signal) => request("/outcome", input, signal),
      feedback: async (value) => { actualFeedback = value; feedback.resolve(value); },
      preserve: async () => { throw new Error("unexpected preservation"); },
    });
    let requestedOrdinal = 0;
    const tick = () => {
      requested = { requestedOrdinal, dueAtMs: schedulingAnchor! + requestedOrdinal * 250, dispatchAtMs: now() };
      requestedOrdinal++;
      try { monitor!.tick(); } catch (error) { fatal = error; failures[0]!.reject(error); }
      finally { requested = undefined; }
    };
    schedulingAnchor = now();
    tick();
    interval = setInterval(tick, 250);
    await commandTreeCalibration(plan, { producers: producerPair, intervention: intervention.promise });
    driverCompleted = true; cleanupEnd = now();
    await monitor.settle();
    assert.equal(fatal, undefined);
    assert.ok(binding && selectedIndex !== undefined);
    const selected = selectedIndex;
    const ack = acknowledgements.filter((item) => item.path === "/reservation");
    const outcomes = acknowledgements.filter((item) => item.path === "/outcome");
    assert.equal(ack.length, 1); assert.equal(outcomes.length, 1);
    assert.equal(outcomes[0]!.ack.outcome, "confirmed_drained");
    const highIndex = samples.findIndex((s) => s.currentBytes >= high);
    const low = samples[highIndex - 1], above = samples[highIndex];
    assert.ok(low && above, "real hard crossing required; early PSI cannot replace it");
    const observe = observations.find((o) => o.index === selected && o.startMs >= above.completedAtMs);
    const signal = signalWrites.filter((s) => s.index === selected);
    const disposal = disposals.find((d) => d.index === selected);
    assert.ok(observe && disposal && signal.length === 1 && cleanupStart !== undefined);
    const before = rawSamples[0]!.events!, after = rawSamples.at(-1)!.events!;
    const span = (startMs: number, endMs: number) => ({ binding: binding!, startMs, endMs });
    assert.equal(disposal.clean, true);
    leg = { binding, hardThresholdBytes: high,
      hardBracket: { lastBelowSampleId: low.sampleId, firstAtOrAboveSampleId: above.sampleId,
        lowerMs: low.startedAtMs, upperMs: above.completedAtMs },
      spans: { observe: span(observe.startMs, observe.endMs), reservation: span(ack[0]!.startMs, ack[0]!.endMs),
        signal: span(signal[0]!.startMs, signal[0]!.endMs), drain: span(signal[0]!.endMs, disposal.endMs),
        cleanup: span(cleanupStart, cleanupEnd) },
      ack: { binding, atMs: ack[0]!.endMs, accepted: true },
      exit: { binding, atMs: disposal.endMs, confirmed: true },
      cleanup: { binding, confirmed: true },
      oomBefore: before.oom!, oomAfter: after.oom!, oomKillsBefore: before.oom_kill!, oomKillsAfter: after.oom_kill!,
      oomGroupKillsBefore: before.oom_group_kill!, oomGroupKillsAfter: after.oom_group_kill! };
    // Keep the existing interval until monitor state AND strictly post-cleanup evidence agree.
    await until(() => {
      if (fatal !== undefined) throw fatal;
      return monitor!.state.armed && !monitor!.state.pendingIntervention
        && freshPostCleanupDwell(samples, cleanupEnd!, now(), guard, plan.limitBytes);
    }, 5000);
    clearInterval(interval); interval = undefined;
    const response = { provenance: "measured" as const, reportId: expected.reportId, legs: [leg] };
    const derivation = calibrationReport({ baseline, samples, reserveBytes: guard.reserveBytes, response });
    assert.equal(derivation.accepted, true);
    report.samplingSchedule = samplingSchedule = calibrationSchedulingReport(samples, {
      periodMs: 250, anchorMs: schedulingAnchor, observations: schedulingObservations,
    });
    report.response = response; report.derivation = derivation;
    report.trees = producerPair.map((p, index) => ({ treeId: p.treeId, identity: p.identity, enrollment: p.enrollment,
      exits: exits.filter((e) => e.index === index), disposals: disposals.filter((d) => d.index === index).slice(0, 1) }));
    report.settlement = { feedback: await feedback.promise, broker: await replies[selected]!, serviceRecords, acknowledgements,
      monitorState: monitor.state, history: monitor.history, survival, schedule, observations, signalWrites, selectedIndex: selected };
    report.cleanup = { confirmed: true, startMs: cleanupStart, endMs: cleanupEnd };
    report.status = "SUCCESS_COMPLETE"; report.outcome = { status: "failed", reasons: ["worker_memory_pressure"], successor: "aborted" };
    unsubscribe();
  } catch (error) { reasons.push(String(error)); }
  finally {
    if (interval) clearInterval(interval);
    const startMs = now();
    // Two exact owned handles, parallel <=5s disposal then <=5s joins. One failure
    // does not suppress sibling cleanup. Failed cleanup prevents host next-start.
    for (let i = 0; i < 2; i++) { expectedExit[i] = true; aborts[i]!.abort(); }
    const cleaned = await Promise.allSettled(handles.map(async (handle, index) => {
      const startMs = now(), value = await handle.dispose(5000);
      disposals.push({ index, startMs, endMs: now(), clean: value.clean, evidence: value });
      return value;
    }));
    const joined = await Promise.allSettled(replies.map((reply) => boundedCommandWait(reply, 5000)));
    if (cleaned.some((r) => r.status !== "fulfilled" || !r.value.clean) || joined.some((r) => r.status !== "fulfilled")) {
      reasons.push("owned cleanup unconfirmed");
    }
    if (monitor) { try { await monitor.stop(); } catch (error) { reasons.push("monitor cleanup: " + String(error)); } }
    if (service) { service.closeAllConnections(); await new Promise<void>((resolve) => service!.close(() => resolve())); }
    if (handles.length) { try { sample(); } catch (error) { reasons.push("final sample: " + String(error)); } }
    report.finalCleanup = { startMs, endMs: now(), handles: handles.length,
      clean: cleaned.map((r) => r.status === "fulfilled" ? r.value : { unknown: true, failure: String(r.reason) }),
      joined: joined.map((r) => r.status), driverCompleted };
    report.baseline = baseline; report.samples = samples; report.rawSamples = rawSamples;
    const oomEvents = oomEventEvidence(rawSamples);
    report.oomEvents = oomEvents;
    if (oomEvents && Object.values(oomEvents.delta).some((v) => v !== null && v < 0)) {
      reasons.push("OOM counter decreased");
    }
    report.reasons = reasons;
    if (reasons.length) report.status = "FAILED";
    if (report.status === "FAILED") {
      Object.assign(report, projectFailedEvidence({
        samplingSchedule: samplingSchedule ?? { periodMs: 250, anchorMs: schedulingAnchor, rows: schedulingObservations,
          maxPositiveLatenessMs: null, maxAbsoluteJitterMs: null },
        trees: treeMetadata, exits, disposals, feedback: actualFeedback, brokerReplies,
        serviceRecords, acknowledgements, observations, signalWrites, schedule, survival,
        selectedIndex, binding, monitorState: monitor?.state ?? null, history: monitor?.history ?? null,
        leg, reportId: expected.reportId, cleanupStart, cleanupEnd,
      }));
    }
  }
  const json = JSON.stringify(report);
  if (Buffer.byteLength(json) > REPORT_BYTES - 256) throw new Error("complete report exceeds byte bound");
  console.log("UZI_MEMORY_CALIBRATION_REPORT_BEGIN\n" + json + "\nUZI_MEMORY_CALIBRATION_REPORT_END");
  return report.status === "SUCCESS_COMPLETE" ? 0 : 1;
}
if (process.argv[1]?.endsWith("/calibration-fixture.mts")) {
  process.exitCode = await fixture(process.argv[2]!, process.argv[3]!);
}
