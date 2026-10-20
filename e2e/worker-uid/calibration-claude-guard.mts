// Guard-driven leg of the Node assert diagnostic calibration (issue #2471). Measurement only.
//
// Runs ONE workload command as a guarded Claude command: the REAL ClaudeCommandLane (driven as the
// SDK does, lane.hook then lane.handle), the REAL launcher (launchRunnerCommandRoot with the image
// supervisor at this container's own uid, the single-uid posture), a REAL WorkerMemoryCommands
// context, the REAL WorkerMemoryReader on this container's cgroup and the REAL WorkerMemoryMonitor.
// Only the reservation ack service is a local fixture (LOCALFIXTURE, as in calibration-fixture.mts).
//
// Nothing here is a production threshold. `highBytes` (a fixed fraction of the cgroup limit),
// the 250 ms sampling period and the 5 s response budget are MEASUREMENT parameters chosen so the
// guard has room to act before the cgroup limit; the result reports what happened at them. A guard
// that did not act in time, or an OOM kill of the command, is a recorded outcome, never an error.
// Harness failures (lane or launcher construction, unreadable baseline, unconfirmed cleanup) throw.
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import { createServer } from "node:http";
import { performance } from "node:perf_hooks";
import { GUARDED_COMMAND_TOOL } from "../../agent/src/claude-command/grants.js";
import { ClaudeCommandLane } from "../../agent/src/claude-command/lane.js";
import type { CapturedMemoryContext } from "../../agent/src/codex/broker.js";
import { launchRunnerCommandRoot, type RunnerCommandRoot } from "../../agent/src/codex/launcher.js";
import type { MemoryReservation, MemoryReservationRequest, WorkerMemoryPressureResult } from "../../agent/src/protocol.js";
import { WorkerMemoryCommands } from "../../agent/src/worker-memory-commands.js";
import { WorkerMemoryMonitor } from "../../agent/src/worker-memory-guard.js";
import { WorkerMemoryReader } from "../../agent/src/worker-memory-reader.js";

export const SAMPLE_MS = 250;
export const RESPONSE_BUDGET_MS = 5000;
export const REARM_MS = 500;
export const HYSTERESIS_BYTES = 64 * 1024 ** 2;
// Measurement parameter: the guard's high-water mark is this fraction of the cgroup limit.
export const HIGH_FRACTION = 0.5;
export const MAX_SERIES = 200;
export const BASELINE_SAMPLES = 3;
export const COMMAND_DEADLINE_MS = 15_000;
// Lane join bound at the end of the leg (quiesce) and for the monitor's own settle.
export const QUIESCE_MS = 8000;
export const SETTLE_MS = 8000;
const SPILL_CAP_BYTES = 64 * 1024;
const CGROUP = "/sys/fs/cgroup";
const PSI_FILE = CGROUP + "/memory.pressure";
const ZERO_PSI = "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n";
const META = "claudecode/toolUseId";

export type GuardedOutcome =
  | "guard_acted" // the lane returned the guard's worker_memory_pressure result for the attributed tool use
  | "oom_before_guard" // the cgroup OOM killer acted and the guard returned no pressure result
  | "deadline" // the command hit its own deadline
  | "exited_before_guard" // the workload ran (its own report is in the output) and ended on its own without the guard acting
  // Harness faults, never measurements: calibration.py parse_guarded rejects every one of them.
  | "launch_failed" // "Command could not be started"
  | "result_mismatch" // "memory_result_mismatch"
  | "supervision_failed" // "Command supervision failed"
  | "enroll_failed" // "could not be placed under memory supervision"
  | "admission_closed" // memory-guard admission closed without the guard's pressure result
  | "workload_not_started" // the command ended but the workload never reported (exit 127, import error, ...)
  | "other";

export interface GuardedLegInput {
  /** The shell command text the guarded lane runs (the generic workload). */
  command: string;
  limitBytes: number;
  /** Absolute scratch directory the lane runs commands in (writable by this uid). */
  worktree: string;
  /** Env for the command (PATH, HOME, TMPDIR...). */
  env: Record<string, string>;
}

const sleep = (ms: number) => new Promise<void>((done) => setTimeout(done, ms));
const round = (value: number, digits = 1) => Math.round(value * 10 ** digits) / 10 ** digits;

// Same algorithm and bounds as the reader's own default bounded read (the fixture uses it too):
// at most `maximum` + 1 sentinel byte, so oversized input is still detected by the reader.
function readBounded(file: string, maximum: number): Uint8Array {
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

function readText(file: string): string | null {
  try { return fs.readFileSync(file, "utf8").slice(0, 4096); } catch { return null; }
}
function counterOf(text: string | null, key: string): number | null {
  const match = text?.match(new RegExp("^" + key + " (\\d+)$", "m"));
  return match ? Number(match[1]) : null;
}

/** Process ids visible in this container other than this process (cleanup evidence; the tsx loader's esbuild helper predates the call and is excluded by the caller). */
function otherPids(): number[] {
  const mine = process.pid;
  return fs.readdirSync("/proc").filter((name) => /^[1-9][0-9]*$/.test(name)).map(Number).filter((pid) => pid !== mine);
}
function pidExists(pid: number): boolean {
  try { process.kill(pid, 0); return true; } catch (error) { return (error as NodeJS.ErrnoException).code === "EPERM"; }
}

type Entry = NonNullable<ReturnType<WorkerMemoryCommands["eligibleEntries"]>>[number];

export async function measureGuardedLeg(input: GuardedLegInput) {
  const t0 = performance.now();
  const rel = (value: number) => round(value - t0);
  const now = () => performance.now();
  let psiSynthesized = false;
  // Everything but memory.pressure is the real read. Only an ABSENT memory.pressure (a kernel
  // without PSI) is substituted, and the result says so; PSI is never claimed in that case.
  const reader = new WorkerMemoryReader({ readBytes: (file, max) => {
    try { return readBounded(file, max); } catch (error) {
      if (file === PSI_FILE && (error as NodeJS.ErrnoException).code === "ENOENT") {
        psiSynthesized = true;
        return new TextEncoder().encode(ZERO_PSI);
      }
      throw error;
    }
  } });

  const series: [number, number, number][] = [];
  let sampleCount = 0, unavailableSamples = 0, firstUnavailable: unknown = null;
  let limitBytes = 0, highBytes = 0;
  const found: { crossing: { sampleId: number; completedMs: number; startedMs: number; currentBytes: number } | null } = { crossing: null };
  type Sample = ReturnType<WorkerMemoryReader["sample"]>;
  const sample = (): Sample => {
    const value = reader.sample();
    sampleCount++;
    if (!value.available) {
      unavailableSamples++;
      firstUnavailable ??= { sampleId: value.sampleId, reason: value.reason };
      return value;
    }
    if (series.length < MAX_SERIES) series.push([value.sampleId, rel(value.completedAtMs), value.currentBytes]);
    if (highBytes > 0 && !found.crossing && value.currentBytes >= highBytes) {
      found.crossing = { sampleId: value.sampleId, completedMs: rel(value.completedAtMs), startedMs: rel(value.startedAtMs), currentBytes: value.currentBytes };
    }
    return value;
  };

  // Baseline before anything launches: this harness's own residency only.
  const baseline: number[] = [];
  const eventsBefore = readText(CGROUP + "/memory.events");
  for (let i = 0; i < BASELINE_SAMPLES; i++) {
    const value = sample();
    if (!value.available) throw new Error("harness: cgroup reader unavailable at baseline: " + JSON.stringify(firstUnavailable));
    baseline.push(value.currentBytes);
    limitBytes = value.limitBytes;
    await sleep(SAMPLE_MS);
  }
  assert.equal(limitBytes, input.limitBytes, "cgroup memory.max must equal the requested limit");
  const baselineBytes = Math.max(...baseline);
  highBytes = Math.floor(limitBytes * HIGH_FRACTION);
  const reserveBytes = limitBytes - highBytes;
  const settings = { enabled: true as const, reserveBytes, hysteresisBytes: HYSTERESIS_BYTES, sampleMs: SAMPLE_MS,
    responseBudgetMs: RESPONSE_BUDGET_MS, rearmMs: REARM_MS, maxInterventions: 2 };
  assert.ok(baselineBytes < highBytes - HYSTERESIS_BYTES, "baseline must sit below the quiet mark");

  const commands = new WorkerMemoryCommands();
  const flight = {};
  const identity = { run_id: randomUUID(), worker_id: randomUUID(), register_nonce: randomUUID(), claim_generation: 1, memory_episode: 0 };
  const policy = { version: 1 as const, max_interventions: 2 };
  const context = commands.begin({ ...identity, flight, lifecycleRevision: 1, memory_policy: policy, memory_intervention_count: 0 });
  const captured: CapturedMemoryContext = { commands, context, flight, lifecycleRevision: 1 };

  // LOCALFIXTURE reservation ack service on loopback (this container has no other network).
  const marks: Record<string, number | undefined> = {};
  const exchanges: { path: string; startMs: number; endMs: number }[] = [];
  let reserveRequest: MemoryReservationRequest | undefined;
  const service = createServer((req, res) => {
    let body = "", used = 0;
    req.on("data", (chunk: Buffer) => { used += chunk.length; if (used > 4096) req.destroy(); else body += chunk; });
    req.on("end", () => {
      try {
        assert.equal(req.method, "POST");
        assert.ok(req.url === "/reservation" || req.url === "/outcome");
        const parsed = JSON.parse(body);
        const base = { ...parsed }; delete base.outcome;
        for (const key of Object.keys(identity) as (keyof typeof identity)[]) assert.equal(base[key], identity[key]);
        if (reserveRequest) assert.deepEqual(base, reserveRequest); else reserveRequest = base;
        const ack = { ...base, admitted: true, authorizing: req.url === "/reservation", policy,
          allowance: { limit: 2, used: 1, remaining: 1 }, ...(req.url === "/outcome" ? { outcome: parsed.outcome } : {}) };
        res.writeHead(200, { "Content-Type": "application/json" }); res.end(JSON.stringify(ack));
      } catch { res.writeHead(400); res.end("{}"); }
    });
  });
  await new Promise<void>((resolve) => service.listen(0, "127.0.0.1", resolve));
  const address = service.address();
  assert.ok(address && typeof address !== "string");
  const post = async (route: string, payload: unknown, signal: AbortSignal) => {
    const startMs = now();
    const response = await fetch("http://127.0.0.1:" + address.port + route, { method: "POST", body: JSON.stringify(payload), signal });
    assert.equal(response.status, 200);
    const ack = await response.json() as MemoryReservation;
    exchanges.push({ path: route, startMs: rel(startMs), endMs: rel(now()) });
    return ack;
  };

  const feedback: { atMs: number; result: WorkerMemoryPressureResult }[] = [];
  let preserved = 0;
  const monitor = new WorkerMemoryMonitor(settings, {
    now, commands, incarnation: () => ({ worker_id: identity.worker_id, register_nonce: identity.register_nonce, revision: 1 }),
    sample,
    reserve: async (request, signal) => {
      marks["reserveRequestMs"] ??= rel(now());
      const ack = await post("/reservation", request, signal);
      marks["reserveGrantMs"] ??= rel(now());
      return ack;
    },
    report: async (request, signal) => {
      const ack = await post("/outcome", request, signal);
      marks["outcomeAckMs"] ??= rel(now());
      return ack;
    },
    feedback: async (result) => { feedback.push({ atMs: rel(now()), result }); },
    preserve: async () => { preserved++; },
  });
  const unsubscribe = commands.subscribeConfirmedDrain(() => { marks["confirmedDrainMs"] ??= rel(now()); });

  // Time the supervisor dispose calls: the guard's cancel (and the lane's own cleanup) go through it.
  const disposals: { startMs: number; endMs: number | null; clean: boolean | null }[] = [];
  const launch = async (spec: Parameters<typeof launchRunnerCommandRoot>[0]): Promise<RunnerCommandRoot> => {
    const root = await launchRunnerCommandRoot(spec, { deadlines: { started: 10_000 } });
    const handle = new Proxy(root.handle, {
      get(target, prop) {
        if (prop === "dispose") {
          return async (ms?: number) => {
            const record = { startMs: rel(now()), endMs: null as number | null, clean: null as boolean | null };
            disposals.push(record);
            const value = await target.dispose(ms);
            record.endMs = rel(now()); record.clean = value.clean;
            return value;
          };
        }
        const value = Reflect.get(target, prop, target);
        return typeof value === "function" ? value.bind(target) : value;
      },
    });
    return { handle, child: root.child };
  };

  const trips: string[] = [];
  const lane = new ClaudeCommandLane({
    worktree: input.worktree, cliEnv: input.env, secretPaths: [], dockerWired: false, secrets: [],
    log: { debug() {}, info() {}, warn() {} }, capturedMemory: () => captured,
    tripTurn: (error) => { trips.push(error.name); },
    launch, graceMs: 1000, cleanupMs: 5000, spillCapBytes: SPILL_CAP_BYTES,
  });
  lane.beginTurn();

  let tickCount = 0, tickError: string | null = null;
  const interval = setInterval(() => {
    tickCount++;
    try { monitor.tick(); } catch (error) { tickError ??= String(error); }
  }, SAMPLE_MS);
  monitor.tick(); tickCount++;

  const processesBeforeCall = new Set(otherPids());
  // One model call, exactly as the SDK drives the lane: the PreToolUse hook, then the handler.
  const toolUseId = "toolu_calibration_" + randomUUID().slice(0, 8);
  const args = { command: input.command, timeout: COMMAND_DEADLINE_MS };
  const callStartMs = now();
  const hook = await lane.hook({
    hook_event_name: "PreToolUse", tool_name: GUARDED_COMMAND_TOOL, tool_input: args, tool_use_id: toolUseId,
    session_id: "calibration", transcript_path: "/calibration", cwd: input.worktree,
  } as Parameters<ClaudeCommandLane["hook"]>[0], toolUseId, { signal: new AbortController().signal });
  assert.deepEqual(hook, {}, "the hook admits the call");
  const call = lane.handle(args, { _meta: { [META]: toolUseId }, signal: undefined });

  // The tree the command built, for the cleanup check (best effort; the recorded root is exact).
  const seenPids = new Set<number>();
  const supervisorRoots = new Set<number>();
  const watcher = (async () => {
    let entry: Entry | undefined;
    for (let i = 0; i < 100 && !entry; i++) { entry = commands.eligibleEntries()?.[0]; if (!entry) await sleep(50); }
    if (!entry) return;
    marks["enrolledMs"] = rel(now());
    supervisorRoots.add(entry.root.pid);
    seenPids.add(entry.root.pid);
    try {
      const observation = await entry.observe({ remainingMs: 2000, signal: AbortSignal.timeout(2000) });
      for (const row of observation.rows) seenPids.add(row.pid);
    } catch { /* the guard observes on its own; this only widens the cleanup check */ }
  })();

  const result = await call;
  const callEndMs = now();
  await watcher;
  const resultText = result.content.map((block) => block.text ?? "").join("");

  // The monitor's own settle (reservation, drain confirmation, outcome report) and a quiet stop.
  let settled = true;
  await Promise.race([monitor.settle().then(() => undefined), sleep(SETTLE_MS).then(() => { settled = false; })]);
  clearInterval(interval);
  let quiesceConfirmed = false, quiesceError: string | null = null;
  const liveCountBeforeQuiesce = lane.liveCount;
  try { await lane.quiesce(QUIESCE_MS); quiesceConfirmed = true; } catch (error) { quiesceError = String(error); }
  await monitor.stop();
  unsubscribe();
  service.closeAllConnections();
  await new Promise<void>((resolve) => service.close(() => resolve()));
  sample();

  let pressure: WorkerMemoryPressureResult | null = null;
  try {
    const parsed = JSON.parse(resultText);
    if (parsed && typeof parsed === "object" && parsed.code === "worker_memory_pressure") pressure = parsed as WorkerMemoryPressureResult;
  } catch { pressure = null; }

  const eventsAfter = readText(CGROUP + "/memory.events");
  const oomKillBefore = counterOf(eventsBefore, "oom_kill"), oomKillAfter = counterOf(eventsAfter, "oom_kill");
  const peakText = readText(CGROUP + "/memory.peak");
  const peak = Number(peakText?.trim());
  const currentAfter = Number(readText(CGROUP + "/memory.current")?.trim());
  const oomKillDelta = oomKillBefore !== null && oomKillAfter !== null ? oomKillAfter - oomKillBefore : null;

  const crossing = found.crossing;
  const guardDisposal = disposals.find((d) => marks["reserveGrantMs"] !== undefined && d.startMs >= marks["reserveGrantMs"]!);
  const cancelStartMs = guardDisposal?.startMs ?? null;
  const confirmedDrainMs = marks["confirmedDrainMs"] ?? null;
  const diff = (a: number | null | undefined, b: number | null | undefined) => (a === null || a === undefined || b === null || b === undefined ? null : round(b - a));
  const timeline = {
    crossingSampleId: crossing?.sampleId ?? null,
    crossingSampleStartedMs: crossing?.startedMs ?? null,
    crossingSampleCompletedMs: crossing?.completedMs ?? null,
    crossingCurrentBytes: crossing?.currentBytes ?? null,
    reserveRequestMs: marks["reserveRequestMs"] ?? null,
    reserveGrantMs: marks["reserveGrantMs"] ?? null,
    cancelStartMs,
    confirmedDrainMs,
    outcomeAckMs: marks["outcomeAckMs"] ?? null,
    callStartMs: rel(callStartMs),
    callEndMs: rel(callEndMs),
    enrolledMs: marks["enrolledMs"] ?? null,
  };
  const budget = {
    crossingToReserveRequestMs: diff(timeline.crossingSampleCompletedMs, timeline.reserveRequestMs),
    reserveRoundTripMs: diff(timeline.reserveRequestMs, timeline.reserveGrantMs),
    grantToCancelMs: diff(timeline.reserveGrantMs, cancelStartMs),
    cancelToConfirmedDrainMs: diff(cancelStartMs, confirmedDrainMs),
    crossingToConfirmedDrainMs: diff(timeline.crossingSampleCompletedMs, confirmedDrainMs),
    crossingToToolResultMs: diff(timeline.crossingSampleCompletedMs, timeline.callEndMs),
    sampleMs: SAMPLE_MS, responseBudgetMs: RESPONSE_BUDGET_MS,
  };
  let peakGrowthBytesPerSecond = 0;
  for (let i = 1; i < series.length; i++) {
    const [, t1, b1] = series[i - 1]!, [, t2, b2] = series[i]!;
    if (t2 > t1) peakGrowthBytesPerSecond = Math.max(peakGrowthBytesPerSecond, Math.round(((b2 - b1) / (t2 - t1)) * 1000));
  }
  // The workload child prints one JSON report line (calibration-diagnostic.mts runWorkload) when it
  // finishes, so that line is the evidence it actually started and ran; an exit with none (exit 127,
  // an import error) is a harness fault.
  const workloadReported = /"outcome":"(passed|assertion|other_error)"/.test(resultText);
  const outcome: GuardedOutcome = pressure ? "guard_acted"
    : oomKillDelta ? "oom_before_guard"
    : resultText.startsWith("Command timed out") ? "deadline"
    : resultText.startsWith("Exit code: ") ? (workloadReported ? "exited_before_guard" : "workload_not_started")
    : resultText.startsWith("Command could not be started") ? "launch_failed"
    : resultText.startsWith("memory_result_mismatch") ? "result_mismatch"
    : resultText.startsWith("Command supervision failed") ? "supervision_failed"
    : resultText.includes("could not be placed under memory supervision") ? "enroll_failed"
    : resultText.includes("memory-guard admission closed") ? "admission_closed" : "other";

  const remainingPids = otherPids().filter((pid) => !processesBeforeCall.has(pid));
  const commandPids = [...seenPids];
  const cleanup = {
    liveCountBeforeQuiesce, liveCountAfter: lane.liveCount, quiesceConfirmed, quiesceError, monitorSettled: settled,
    laneRecordedRootsAfter: lane.recordedRoots().length, poisoned: lane.poisoned, trips,
    registryEligibleEntries: commands.eligibleEntries()?.length ?? null,
    commandPids, commandPidsGone: commandPids.every((pid) => !pidExists(pid)),
    supervisorRootPids: [...supervisorRoots], otherProcessPidsRemaining: remainingPids,
    oomKillBefore, oomKillAfter, oomKillDelta, memoryPeakBytes: Number.isFinite(peak) ? peak : null,
    memoryCurrentAfterBytes: Number.isFinite(currentAfter) ? currentAfter : null,
  };
  if (tickError) throw new Error("harness: monitor tick failed: " + tickError);
  if (!quiesceConfirmed) throw new Error("harness: lane quiesce unconfirmed: " + quiesceError);
  if (unavailableSamples > 0 && series.length === 0) throw new Error("harness: no available samples");

  return {
    leg: "guarded" as const,
    measurementParameters: { note: "calibration measurement parameters, not production thresholds",
      highFraction: HIGH_FRACTION, highBytes, reserveBytes, hysteresisBytes: HYSTERESIS_BYTES, sampleMs: SAMPLE_MS,
      responseBudgetMs: RESPONSE_BUDGET_MS, rearmMs: REARM_MS, commandDeadlineMs: COMMAND_DEADLINE_MS, maxInterventions: 2 },
    psiSynthesized,
    baselineBytes, limitBytes,
    growth: { series, sampleCount, seriesTruncated: sampleCount - unavailableSamples > series.length, unavailableSamples,
      firstUnavailable, peakSeriesBytes: series.reduce((m, s) => Math.max(m, s[2]), 0), peakGrowthBytesPerSecond },
    timeline, budget,
    disposals, tickCount,
    tool: { toolUseId, isError: result.isError ?? null, outcome, pressure,
      pressureToolIdMatches: pressure ? pressure.tool_id === toolUseId : null,
      head: pressure ? null : resultText.slice(0, 300) },
    feedbackEvents: feedback.length, preserved,
    monitorHistory: monitor.history.map((item) => ({ status: item.status, outcome: item.outcome ?? null })),
    cleanup,
  };
}
