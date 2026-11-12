import { randomUUID } from "node:crypto";
import { MemoryWaitTimeout, type WorkerMemoryCommands } from "./worker-memory-commands.js";
import type { MemoryIncarnation, MemoryOutcome, MemoryReservation, MemoryReservationRequest, MemoryOutcomeRequest, WorkerMemoryPressureResult } from "./protocol.js";
import type { Config } from "./config.js";
import type { WorkerMemoryReader } from "./worker-memory-reader.js";

type Settings = Extract<NonNullable<Config["memoryGuard"]>, { enabled: true }>;
type Sample = ReturnType<WorkerMemoryReader["sample"]>;
type Available = Extract<Sample, { available: true }>;
type Intervention = Readonly<{ sequence: number }>;
/** Why a hard/early proposal fired; diagnostic only, grants no authority. */
type Evidence = Readonly<{ workingSetBytes: number } | {
  previousSampleId: number; previousStartedAtMs: number; previousCompletedAtMs: number; previousWorkingSetBytes: number;
  intervalMs: number; growthBytes: number; rateBytesPerMs: number; projectedBytes: number;
  psiFullTotalPrevious: number; psiFullTotalCurrent: number; grownFields: readonly string[];
}>;
type Decision = { kind: "inhibited" | "idle" | "hard" | "early" | "rearmed"; highBytes?: number; quietBytes?: number; evidence?: Evidence };

const STAT_FIELDS = ["anon", "shmem", "slab_unreclaimable", "unevictable"] as const;
function natural(n: number): boolean { return Number.isSafeInteger(n) && n >= 0; }
function positive(n: number): boolean { return natural(n) && n > 0; }

type Pressure = Available["pressure"];
type RealPressure = Extract<Pressure, { some: unknown }>;

/** Working set: memory.current minus inactive file cache, clamped at zero.
 * An estimate of non-reclaimable pressure; it is not an OOM predictor. */
export function workingSetBytes(sample: Pick<Available, "currentBytes"> & { stat: Pick<Available["stat"], "inactive_file"> }): number {
  return Math.max(0, sample.currentBytes - sample.stat.inactive_file);
}

/** Exactly the explicit PSI-unavailable marker; any other shape is not one. */
export function isMarker(p: unknown): p is Extract<Pressure, { unavailable: unknown }> {
  if (typeof p !== "object" || p === null) return false;
  const keys = Object.keys(p);
  const reason = (p as { unavailable?: unknown }).unavailable;
  return keys.length === 1 && keys[0] === "unavailable" && (reason === "absent" || reason === "unsupported");
}

/** Real PSI rows: an object with the some and full rows (and nothing else is checked here). */
export function realPressure(p: Pressure | undefined): RealPressure | undefined {
  return p !== undefined && typeof p === "object" && p !== null && "some" in p && "full" in p ? p : undefined;
}

/** Copies the marker or both rows so caller-owned samples cannot rewrite history. */
function copyPressure(p: Pressure, freeze = false): Pressure {
  const out: Pressure = "unavailable" in p ? { unavailable: p.unavailable }
    : { some: freeze ? Object.freeze({ ...p.some }) : { ...p.some }, full: freeze ? Object.freeze({ ...p.full }) : { ...p.full } };
  return freeze ? Object.freeze(out) : out;
}

type StatDetail = NonNullable<Available["statDetail"]>;
/** Frozen deep copy. An uncopyable statDetail is kept by reference so observe() never throws; a deterministic
 * copy failure then also fails the diagnostics build, which reports unavailable:"error". */
function copyDetail(d: StatDetail): StatDetail {
  try { return Object.freeze({ ...d, missing: Object.freeze([...d.missing]), malformed: Object.freeze([...d.malformed]) }); }
  catch { return d; }
}
/** Deep-frozen copy of an available sample (stat, pressure and statDetail included). */
function snapshot(s: Available): Available {
  return Object.freeze({ ...s, stat: Object.freeze({ ...s.stat }), pressure: copyPressure(s.pressure, true),
    ...(s.statDetail ? { statDetail: copyDetail(s.statDetail) } : {}) });
}
type DecisionRecord = Readonly<{ kind: Decision["kind"]; highBytes?: number; quietBytes?: number; evidence?: Evidence }>;
function decisionRecord(d: Decision): DecisionRecord {
  return Object.freeze({ kind: d.kind,
    ...(d.highBytes !== undefined ? { highBytes: d.highBytes } : {}),
    ...(d.quietBytes !== undefined ? { quietBytes: d.quietBytes } : {}),
    ...(d.evidence !== undefined ? { evidence: d.evidence } : {}) });
}
type Trigger = Readonly<{ sample: Available; decision: DecisionRecord }>;
type Tree = Readonly<{ rssBytesApprox: number; startedAtMs: number; completedAtMs: number; candidates: number; failures: number | undefined; rejections: unknown }>;
type FailureClass = "observation_timed_out" | "observation_interrupted" | "observation_threw";
/** Errors this guard created for an abort that its own deadline timer did not fire. */
const interruptions = new WeakSet<Error>();
const OBSERVATION_DETAILS: ReadonlySet<string> = new Set(["unreadable", "oversize", "stale", "timeout", "busy", "transport", "identity_mismatch", "retired"]);
type RejectionRow = Readonly<{ run_id: string; command_id: string; reasons: readonly string[]; observation_detail?: string; rss_bytes_approx: number | { unavailable: "unknown" } }>;
type RejectionRecord = Readonly<{ selected_run_id: string | null; selected_command_id: string | null; candidate_count: number; rejections: unknown }>;

function failureClass(error: unknown): FailureClass {
  if (error instanceof MemoryWaitTimeout) return "observation_timed_out";
  return error instanceof Error && interruptions.has(error) ? "observation_interrupted" : "observation_threw";
}
/** Allowlisted, validated adapter detail; a hostile or unknown value reads "unknown". Absent stays absent. */
function observationDetail(observation: unknown): string | undefined {
  try {
    const detail = (observation as { detail?: unknown } | undefined)?.detail;
    if (detail === undefined) return undefined;
    return typeof detail === "string" && OBSERVATION_DETAILS.has(detail) ? detail : "unknown";
  } catch { return "unknown"; }
}
function rejectionRow(runId: string, commandId: string, reasons: readonly string[], observation: unknown, rss?: number): RejectionRow {
  const detail = observationDetail(observation);
  return { run_id: runId, command_id: commandId, reasons: [...reasons], ...(detail !== undefined ? { observation_detail: detail } : {}),
    rss_bytes_approx: rss === undefined ? { unavailable: "unknown" } : rss };
}
type Observed = Readonly<{ startedAtMs: number; completedAtMs: number; rows: readonly { rssBytes: number }[] }>;
type Diag = Readonly<{ trigger?: Trigger; tree: Tree; cancel?: Observed }>;

function deepFreeze<T>(value: T): T {
  if (typeof value === "object" && value !== null && !Object.isFrozen(value)) {
    Object.freeze(value);
    for (const child of Object.values(value)) deepFreeze(child);
  }
  return value;
}

function section(s: Available, d: DecisionRecord): Record<string, unknown> {
  const e = d.evidence as Record<string, unknown> | undefined;
  const p = s.pressure;
  const detail = s.statDetail;
  return {
    decision: d.kind, sample_id: s.sampleId, started_at_ms: s.startedAtMs, completed_at_ms: s.completedAtMs,
    current_bytes: s.currentBytes, working_set_bytes: workingSetBytes(s), limit_bytes: s.limitBytes,
    high_bytes: d.highBytes, quiet_bytes: d.quietBytes,
    stat: { anon: s.stat.anon, shmem: s.stat.shmem, slab_unreclaimable: s.stat.slab_unreclaimable,
      unevictable: s.stat.unevictable, inactive_file: s.stat.inactive_file },
    stat_detail: detail ? { ...detail, missing: [...detail.missing], malformed: [...detail.malformed] } : { unavailable: "absent" },
    psi: "unavailable" in p ? { unavailable: p.unavailable } : { some: { ...p.some }, full: { ...p.full } },
    ...(e && "previousSampleId" in e ? { early: {
      previous_sample_id: e.previousSampleId, previous_started_at_ms: e.previousStartedAtMs,
      previous_completed_at_ms: e.previousCompletedAtMs, previous_working_set_bytes: e.previousWorkingSetBytes,
      interval_ms: e.intervalMs, growth_bytes: e.growthBytes, rate_bytes_per_ms: e.rateBytesPerMs,
      projected_bytes: e.projectedBytes, psi_full_total_previous: e.psiFullTotalPrevious,
      psi_full_total_current: e.psiFullTotalCurrent, grown_fields: [...(e.grownFields as string[])],
    } } : {}),
  };
}

/** Pure working-set policy (memory.current minus inactive file). Early intervention
 * additionally needs real PSI on consecutive samples. No timers, signals, process inspection or reservation IO.
 * beginIntervention is a synchronous single-flight seam, not an ACK/reservation adapter.
 * The global unconfirmed-drain fence survives invalid samples and limit changes.
 * Intervention count is diagnostic; episode allowance belongs to the durable API.
 */
export class WorkerMemoryGuard {
  private readonly config: Settings;
  private previous: Available | undefined;
  private lastId = 0;
  private lastStart = -1;
  private lastComplete = -1;
  private lastNow = -1;
  private limit: number | undefined;
  private dwell: number | undefined;
  private armed = true;
  private count = 0;
  private intervention: Intervention | undefined;
  private drained = false;
  private resolved = false;
  private proposal = false;

  constructor(config: Settings) {
    if (config.enabled !== true || !positive(config.reserveBytes) || !positive(config.hysteresisBytes)
      || !positive(config.sampleMs) || config.sampleMs > 2147483647
      || !positive(config.responseBudgetMs) || config.responseBudgetMs > 2147483647
      || !positive(config.rearmMs) || config.rearmMs > 2147483647
      || !positive(config.maxInterventions) || config.maxInterventions > 9999) throw new Error("invalid memory guard settings");
    this.config = Object.freeze({ ...config });
  }

  get state(): Readonly<{ armed: boolean; pendingIntervention: boolean; unconfirmedDrain: boolean; unresolvedOutcome: boolean; interventions: number }> {
    return Object.freeze({
      armed: this.armed, pendingIntervention: this.intervention !== undefined && (!this.drained || !this.resolved),
      unconfirmedDrain: this.intervention !== undefined && !this.drained,
      unresolvedOutcome: this.intervention !== undefined && !this.resolved, interventions: this.count,
    });
  }

  thresholds(limitBytes: number): Readonly<{ highBytes: number; quietBytes: number }> {
    const highBytes = limitBytes - this.config.reserveBytes;
    const quietBytes = highBytes - this.config.hysteresisBytes;
    if (!positive(limitBytes) || !(0 < quietBytes && quietBytes < highBytes && highBytes < limitBytes)) {
      throw new Error("incompatible memory limit");
    }
    return Object.freeze({ highBytes, quietBytes });
  }

  private valid(sample: Sample, now: number): sample is Available {
    const ordered = positive(sample.sampleId) && sample.sampleId > this.lastId
      && Number.isFinite(sample.startedAtMs) && sample.startedAtMs >= 0
      && sample.startedAtMs >= this.lastStart && Number.isFinite(sample.completedAtMs)
      && sample.completedAtMs >= sample.startedAtMs && sample.completedAtMs >= this.lastComplete
      && Number.isFinite(now) && now >= this.lastNow && now >= sample.completedAtMs;
    // Preserve high-water marks even for unavailable or stale attempts. Replays never repair dwell.
    if (positive(sample.sampleId)) this.lastId = Math.max(this.lastId, sample.sampleId);
    if (Number.isFinite(sample.startedAtMs)) this.lastStart = Math.max(this.lastStart, sample.startedAtMs);
    if (Number.isFinite(sample.completedAtMs)) this.lastComplete = Math.max(this.lastComplete, sample.completedAtMs);
    if (Number.isFinite(now)) this.lastNow = Math.max(this.lastNow, now);
    if (!ordered || now - sample.startedAtMs > this.config.sampleMs + this.config.responseBudgetMs
      || sample.available !== true || !natural(sample.currentBytes) || !positive(sample.limitBytes)
      || !sample.stat || !sample.pressure) return false;
    if (!STAT_FIELDS.every((field) => natural(sample.stat[field])) || !natural(sample.stat.inactive_file)) return false;
    if (isMarker(sample.pressure)) return true;
    const rows = sample.pressure as Partial<RealPressure>;
    if (!rows.full || !rows.some || Object.keys(sample.pressure).length !== 2) return false;
    return [rows.full, rows.some].every((psi) => natural(psi.total)
      && [psi.avg10, psi.avg60, psi.avg300].every((avg) => Number.isFinite(avg) && avg >= 0 && avg <= 100));
  }

  evaluate(sample: Sample, now: number): Decision {
    this.proposal = false;
    if (!this.valid(sample, now)) {
      this.previous = undefined;
      this.dwell = undefined;
      return { kind: "inhibited" };
    }
    let thresholds: ReturnType<WorkerMemoryGuard["thresholds"]>;
    try { thresholds = this.thresholds(sample.limitBytes); }
    catch {
      this.previous = undefined;
      this.dwell = undefined;
      this.limit = undefined;
      return { kind: "inhibited" };
    }
    const { highBytes, quietBytes } = thresholds;
    if (this.limit !== sample.limitBytes) {
      this.previous = undefined;
      this.dwell = undefined;
      this.limit = sample.limitBytes;
    }
    const previous = this.previous;
    // Copy nested fields: caller-owned samples cannot rewrite policy history.
    this.previous = { ...sample, stat: { ...sample.stat }, pressure: copyPressure(sample.pressure) };
    const workingSet = workingSetBytes(sample);
    if (!this.armed) {
      const ready = this.intervention !== undefined && this.drained && this.resolved;
      if (!ready || workingSet >= quietBytes) this.dwell = undefined;
      else if (this.dwell === undefined) this.dwell = sample.completedAtMs;
      else if (sample.completedAtMs > this.dwell && sample.completedAtMs - this.dwell >= this.config.rearmMs
        && previous !== undefined
        && sample.startedAtMs - previous.completedAtMs <= this.config.sampleMs + this.config.responseBudgetMs) {
        this.armed = true;
        this.intervention = undefined;
        this.dwell = undefined;
        return { kind: "rearmed", ...thresholds };
      }
      // A gap in observations cannot establish continuous low usage.
      if (previous === undefined || sample.startedAtMs - previous.completedAtMs > this.config.sampleMs + this.config.responseBudgetMs) {
        this.dwell = ready && workingSet < quietBytes ? sample.completedAtMs : undefined;
      }
      return { kind: "idle", ...thresholds };
    }
    if (this.state.pendingIntervention) return { kind: "inhibited", ...thresholds };
    if (workingSet >= highBytes) {
      this.proposal = true;
      return { kind: "hard", ...thresholds, evidence: Object.freeze({ workingSetBytes: workingSet }) };
    }
    const currentPsi = realPressure(sample.pressure);
    const previousPsi = previous && realPressure(previous.pressure);
    if (previous && currentPsi && previousPsi && previous.limitBytes === sample.limitBytes
      && now - previous.startedAtMs <= this.config.sampleMs + this.config.responseBudgetMs
      && sample.completedAtMs > previous.completedAtMs
      && currentPsi.full.total > previousPsi.full.total
      && STAT_FIELDS.some((field) => sample.stat[field] > previous.stat[field])) {
      const totalGrowth = Math.max(0, workingSet - workingSetBytes(previous));
      const rate = totalGrowth / (sample.completedAtMs - previous.completedAtMs);
      const projectedBytes = workingSet + rate * this.config.responseBudgetMs;
      if (projectedBytes >= highBytes) {
        this.proposal = true;
        return { kind: "early", ...thresholds, evidence: Object.freeze({
          previousSampleId: previous.sampleId, previousStartedAtMs: previous.startedAtMs,
          previousCompletedAtMs: previous.completedAtMs, previousWorkingSetBytes: workingSetBytes(previous),
          intervalMs: sample.completedAtMs - previous.completedAtMs, growthBytes: totalGrowth,
          rateBytesPerMs: rate, projectedBytes,
          psiFullTotalPrevious: previousPsi.full.total, psiFullTotalCurrent: currentPsi.full.total,
          grownFields: Object.freeze(STAT_FIELDS.filter((field) => sample.stat[field] > previous.stat[field])),
        }) };
      }
    }
    return { kind: "idle", ...thresholds };
  }

  beginIntervention(): Intervention | undefined {
    if (!this.proposal || !this.armed || this.intervention) return undefined;
    this.proposal = false;
    this.armed = false;
    this.dwell = undefined;
    this.drained = false;
    this.resolved = false;
    const intervention = Object.freeze({ sequence: ++this.count });
    this.intervention = intervention;
    return intervention;
  }

  confirmDrain(intervention: Intervention): boolean {
    if (this.intervention !== intervention) return false;
    this.drained = true;
    return true;
  }

  resolveOutcome(intervention: Intervention): boolean {
    if (this.intervention !== intervention) return false;
    if (!this.resolved) this.dwell = undefined;
    this.resolved = true;
    return true;
  }
}

type Commands = WorkerMemoryCommands;
type Entry = ReturnType<Commands["register"]>;
type Context = Entry["context"];
type Selection = NonNullable<ReturnType<Commands["select"]>>;
type Timer = (callback: () => void, ms: number) => () => void;
type Authority = Readonly<{
  request: Readonly<MemoryReservationRequest>;
  context: Context;
  phaseRevision: number;
  entry: Entry;
  incarnation: MemoryIncarnation;
  sample: Available;
  decision: DecisionRecord;
}>;
export type SampleWait = "sample" | "timeout" | "aborted";
type Journal = Readonly<{ request: Readonly<MemoryReservationRequest>; status: "pending" | "ambiguous" | "admitted" | "denied"; outcome?: MemoryOutcome }>;

/** Worker-owned async orchestration. Each IO races an independent deadline even
 * when a port ignores its signal. A pass observes at most 256 enrolled trees;
 * failed observations disqualify siblings independently. No fallback after selection.
 */
export class WorkerMemoryMonitor {
  private readonly guard: WorkerMemoryGuard;
  private readonly pressure: WorkerMemoryGuard;
  private latest: Available | undefined;
  private latestDecision: DecisionRecord | undefined;
  private applicable = false;
  private closed = false;
  private pending: Promise<void> | undefined;
  private readonly controllers = new Set<AbortController>();
  private readonly reports = new Set<Promise<void>>();
  private readonly journal: Journal[] = [];
  private lastNoSelection: string | undefined;
  private held: { ticket: Intervention; latch: NonNullable<ReturnType<Commands["acquireLatch"]>>; entry: Entry; authority: Authority } | undefined;
  private unsubscribe: (() => void) | undefined;
  private readonly unsubscribeDrain: () => void;
  private running: Promise<void> | undefined;
  private interruptWait: (() => void) | undefined;
  private sampleSeq = 0;
  private validSeq = 0;
  private readonly sampleWaiters = new Set<(outcome: SampleWait | undefined) => void>();

  constructor(private readonly config: Settings, private readonly ports: {
    sample: () => Sample;
    now: () => number;
    commands: Commands;
    incarnation: () => MemoryIncarnation | undefined;
    subscribeInvalidation?: (callback: () => void) => () => void;
    reserve: (request: MemoryReservationRequest, signal: AbortSignal, remainingMs: number) => Promise<MemoryReservation>;
    report: (request: MemoryOutcomeRequest, signal: AbortSignal, remainingMs: number, context: Context) => Promise<MemoryReservation>;
    feedback: (result: WorkerMemoryPressureResult, context: Context, signal: AbortSignal, remainingMs: number) => Promise<void>;
    preserve: (context: Context, signal: AbortSignal, remainingMs: number) => Promise<void>;
    prepareFeedback?: (context: Context, result: WorkerMemoryPressureResult, diagnostics?: unknown) => void;
    preparePreservation?: (context: Context, result: WorkerMemoryPressureResult) => void;
    preservationBudgetMs?: (context: Context) => number;
    /** Best-effort log of rejected candidates; receives allowlisted scalar fields only and grants no authority. */
    rejections?: (record: RejectionRecord) => void;
    timer?: Timer;
  }) {
    this.config = Object.freeze({ ...config });
    this.guard = new WorkerMemoryGuard(config);
    this.pressure = new WorkerMemoryGuard(config);
    this.unsubscribeDrain = ports.commands.subscribeConfirmedDrain((entry) => {
      const held = this.held;
      if (held?.entry === entry) this.guard.confirmDrain(held.ticket);
    });
    this.unsubscribe = ports.subscribeInvalidation?.(() => {
      ports.commands.registrationRotated();
      this.abort();
    });
  }

  get history(): readonly Journal[] { return Object.freeze([...this.journal]); }
  get state() { return this.guard.state; }
  get busy(): boolean { return this.pending !== undefined; }

  /** Read-only view of the tick counters. `validSeq` equals `seq` only when the latest tick produced an
   *  available, non-inhibited sample; a smaller `validSeq` means the current sample is not usable. */
  get observation(): Readonly<{ seq: number; validSeq: number; armed: boolean; pendingIntervention: boolean }> {
    const { armed, pendingIntervention } = this.guard.state;
    return Object.freeze({ seq: this.sampleSeq, validSeq: this.validSeq, armed, pendingIntervention });
  }

  /** Waits for a tick newer than `afterSeq`. Holds no context, latch or authority and begins no capture;
   *  the timer and listener are always released. A stopped monitor or an aborted signal yields "aborted". */
  waitSample(afterSeq: number, deadline: number, signal?: AbortSignal): Promise<SampleWait> {
    if (this.closed || signal?.aborted) return Promise.resolve("aborted");
    if (this.sampleSeq > afterSeq) return Promise.resolve("sample");
    const ms = Math.floor(deadline - this.ports.now());
    if (!(ms >= 1)) return Promise.resolve("timeout");
    return new Promise<SampleWait>((resolve) => {
      let clear = () => {};
      const finish = (outcome: SampleWait | undefined) => {
        clear();
        signal?.removeEventListener("abort", onAbort);
        this.sampleWaiters.delete(listener);
        resolve(outcome ?? "aborted");
      };
      const onAbort = () => finish("aborted");
      const listener = (outcome: SampleWait | undefined) => finish(outcome);
      this.sampleWaiters.add(listener);
      signal?.addEventListener("abort", onAbort, { once: true });
      clear = this.timer(() => finish("timeout"), Math.min(ms, 2147483647));
    });
  }

  private timer(callback: () => void, ms: number): () => void {
    if (this.ports.timer) return this.ports.timer(callback, ms);
    const handle = setTimeout(callback, ms);
    return () => clearTimeout(handle);
  }

  private abort(): void { for (const controller of this.controllers) controller.abort(); }

  private async bounded<T>(work: (signal: AbortSignal, ms: number) => Promise<T>, deadline: number): Promise<T> {
    const ms = Math.floor(deadline - this.ports.now());
    if (!Number.isSafeInteger(ms) || ms < 1 || ms > 2147483647 || this.closed) {
      throw this.closed ? new Error("memory deadline") : new MemoryWaitTimeout("memory deadline");
    }
    const controller = new AbortController();
    this.controllers.add(controller);
    let clear = () => {};
    let onAbort = () => {};
    // Only this call's own deadline timer sets the flag; stop and rotation abort without it.
    let timedOut = false;
    const interrupted = (): Error => {
      if (timedOut) return new MemoryWaitTimeout("memory IO interrupted");
      const error = new Error("memory IO interrupted");
      interruptions.add(error);
      return error;
    };
    const timeout = new Promise<never>((_, reject) => {
      onAbort = () => reject(interrupted());
      controller.signal.addEventListener("abort", onAbort, { once: true });
      clear = this.timer(() => { timedOut = true; controller.abort(); }, ms);
    });
    try { return await Promise.race([Promise.resolve().then(() => {
      if (controller.signal.aborted) throw interrupted();
      return work(controller.signal, ms);
    }), timeout]); }
    finally {
      clear();
      controller.signal.removeEventListener("abort", onAbort);
      this.controllers.delete(controller);
      controller.abort();
    }
  }

  /** Synchronous fast lane: never awaits network, observation, cancellation or reports. */
  tick(): void {
    if (this.closed) return;
    let fresh = false;
    try { this.observe(() => { fresh = true; }); }
    finally {
      this.sampleSeq++;
      if (fresh) this.validSeq = this.sampleSeq;
      for (const waiter of this.sampleWaiters) waiter("sample");
    }
  }

  private observe(markFresh: () => void): void {
    const input = this.ports.sample();
    const now = this.ports.now();
    const pressure = this.pressure.evaluate(input, now);
    this.applicable = pressure.kind === "hard" || pressure.kind === "early";
    this.latest = pressure.kind !== "inhibited" && input.available ? snapshot(input) : undefined;
    this.latestDecision = this.latest ? decisionRecord(pressure) : undefined;
    if (this.latest) markFresh();
    const decision = this.guard.evaluate(input, now);
    if (decision.kind === "rearmed" && this.held) {
      this.ports.commands.releaseLatch(this.held.latch);
      this.held = undefined;
    }
    if (this.pending || (decision.kind !== "hard" && decision.kind !== "early")) return;
    const entries = this.ports.commands.eligibleEntries();
    if (!entries?.length) return;
    const deadline = now + this.config.responseBudgetMs;
    // In-memory trigger copy for diagnostics only; the deadline and authority path are unchanged.
    // Defensive: snapshot() already succeeded on this input above; a failure here only drops the trigger record.
    let trigger: Trigger | undefined;
    try { trigger = input.available ? Object.freeze({ sample: snapshot(input), decision: decisionRecord(decision) }) : undefined; }
    catch { trigger = undefined; }
    this.pending = this.intervene(entries, deadline, trigger).catch(() => {}).finally(() => { this.pending = undefined; });
  }

  run(): Promise<void> {
    return this.running ??= this.loop();
  }

  private async loop(): Promise<void> {
    while (!this.closed) {
      this.tick();
      if (this.closed) break;
      await new Promise<void>((resolve) => {
        const clear = this.timer(() => {
          this.interruptWait = undefined;
          resolve();
        }, this.config.sampleMs);
        this.interruptWait = () => {
          clear();
          this.interruptWait = undefined;
          resolve();
        };
      });
    }
  }

  async stop(): Promise<void> {
    this.closed = true;
    this.interruptWait?.();
    for (const waiter of this.sampleWaiters) waiter(undefined);
    this.ports.commands.shutdown(); // invalidate authority before the first await
    this.unsubscribe?.();
    this.unsubscribe = undefined;
    this.unsubscribeDrain();
    this.abort();
    await Promise.allSettled([...(this.pending ? [this.pending] : []), ...(this.running ? [this.running] : []), ...this.reports]);
  }

  /** Join seam for Worker and deterministic port tests; every task joined here is bounded. */
  async settle(): Promise<void> { await this.pending; }

  private contextCurrent(a: Authority): boolean {
    const incarnation = this.ports.incarnation();
    return !this.closed
      && this.ports.commands.phaseAdmits(a.context, a.phaseRevision)
      && incarnation?.worker_id === a.incarnation.worker_id
      && incarnation?.register_nonce === a.incarnation.register_nonce
      && incarnation?.revision === a.incarnation.revision;
  }

  private current(a: Authority): boolean {
    const sample = this.latest;
    return this.contextCurrent(a) && this.ports.commands.isRegistered(a.entry)
      && this.applicable && sample !== undefined
      && this.ports.now() >= sample.completedAtMs
      && this.ports.now() - sample.startedAtMs <= this.config.sampleMs + this.config.responseBudgetMs;
  }

  private validAck(ack: MemoryReservation, a: Authority, outcome?: MemoryOutcome): boolean {
    if (!ack || typeof ack !== "object") return false;
    for (const field of ["run_id", "worker_id", "register_nonce", "claim_generation", "memory_episode", "intervention_id"] as const) {
      if (ack[field] !== a.request[field]) return false;
    }
    const p = ack.policy;
    const b = ack.allowance;
    return p?.version === 1 && p.max_interventions === a.request.policy.max_interventions
      && !!b && b.limit === p.max_interventions && natural(b.used) && b.used <= b.limit
      && natural(b.remaining) && b.remaining === b.limit - b.used
      && typeof ack.admitted === "boolean" && typeof ack.authorizing === "boolean"
      && (ack.admitted ? b.used > 0 : b.used === b.limit)
      && (ack.outcome === undefined || ["no_signal", "unknown", "confirmed_drained"].includes(ack.outcome))
      && (!ack.authorizing || (ack.admitted && ack.outcome === undefined))
      && (outcome === undefined ? true : ack.outcome === outcome && !ack.authorizing);
  }

  private record(a: Authority, status: Journal["status"], outcome?: MemoryOutcome): void {
    const record = Object.freeze({ request: a.request, status, ...(outcome ? { outcome } : {}) });
    const existing = this.journal.findIndex((item) => item.request === a.request);
    if (existing >= 0) this.journal[existing] = record;
    else this.journal.push(record);
    if (this.journal.length > 256) {
      const completed = this.journal.findIndex((item) => item.outcome !== undefined);
      if (completed >= 0) this.journal.splice(completed, 1);
    }
  }

  private async report(a: Authority, outcome: MemoryOutcome, ticket?: Intervention, attempts = 3): Promise<boolean> {
    this.record(a, "ambiguous", outcome);
    let confirmed = false;
    const accept = (ack: MemoryReservation) => {
      if (!this.validAck(ack, a, outcome)) return;
      confirmed = true;
      // Historical proof resolves only the original held ticket, with no authority gain.
      if (ticket && this.held?.ticket === ticket && this.held.entry === a.entry
        && this.held.authority === a) this.guard.resolveOutcome(ticket);
    };
    // Each fresh producer has its own budget. Failure permits the next identical replay;
    // late producers remain observed, but are never joined without a deadline.
    for (let attempt = 0; attempt < attempts && !confirmed && !this.closed; attempt++) {
      try {
        await this.bounded((signal, ms) => {
          const producer = this.ports.report({ ...a.request, outcome }, signal, ms, a.context);
          void producer.then(accept, () => {});
          return producer;
        }, this.ports.now() + this.config.responseBudgetMs);
      } catch { /* unresolved outcome retains the global fence */ }
    }
    return confirmed;
  }

  private historical(a: Authority): void {
    if (this.closed) { this.record(a, "ambiguous", "no_signal"); return; }
    const task = this.report(a, "no_signal").then(() => {});
    this.reports.add(task);
    void task.finally(() => this.reports.delete(task));
  }

  /** memory_diagnostics v1 from in-memory records only: no IO, no port or reader calls. Missing values are marked, never zero. */
  private diagnostics(a: Authority, diag: Diag | undefined): unknown {
    if (!diag) throw new Error("no diagnostics");
    const { trigger, tree, cancel } = diag;
    const cfg = this.config;
    return deepFreeze({
      version: 1, metric: "working_set_v1", clock: "monotonic_ms",
      settings: { reserve_bytes: cfg.reserveBytes, hysteresis_bytes: cfg.hysteresisBytes, sample_ms: cfg.sampleMs,
        response_budget_ms: cfg.responseBudgetMs, rearm_ms: cfg.rearmMs, max_interventions: cfg.maxInterventions },
      trigger: trigger ? section(trigger.sample, trigger.decision) : { unavailable: "absent" },
      authority: section(a.sample, a.decision),
      authority_same_as_trigger: trigger !== undefined && trigger.sample.sampleId === a.sample.sampleId,
      tree: {
        selected_rss_bytes_approx: tree.rssBytesApprox, selected_started_at_ms: tree.startedAtMs,
        selected_completed_at_ms: tree.completedAtMs, candidates: tree.candidates, observation_failures: tree.failures ?? { unavailable: "error" },
        rejections: tree.rejections,
        cancel_check: cancel ? {
          rss_bytes_approx: cancel.rows.reduce((sum, row) => sum + row.rssBytes, 0),
          started_at_ms: cancel.startedAtMs, completed_at_ms: cancel.completedAtMs,
        } : { unavailable: "not_observed" },
      },
    });
  }

  private async finish(a: Authority, selected: Selection, outcome: MemoryOutcome, preserve: boolean, diag?: Diag): Promise<Readonly<{ result: WorkerMemoryPressureResult; preserved: boolean }>> {
    const sample = a.sample;
    const result: WorkerMemoryPressureResult = Object.freeze({
      run_id: a.request.run_id, worker_id: a.request.worker_id, intervention_id: a.request.intervention_id,
      claim_generation: a.request.claim_generation, memory_episode: a.request.memory_episode,
      attempt_id: a.entry.attemptId,
      code: "worker_memory_pressure", command_id: a.entry.commandId, tool_id: a.entry.toolId,
      command: a.entry.command, observed_total_bytes: sample.currentBytes,
      limit_bytes: sample.limitBytes, tree_rss_bytes_approx: selected.rssBytesApprox,
      cancellation_confirmed: outcome === "confirmed_drained", outcome, retry_blocked: true,
      guidance: "Use smaller or serial commands after recovery.",
    });
    const preserveCapturedFlight = preserve;
    // Retain feed intent for every outcome before tool completion and bounded IO.
    // Captured settlement belongs to this flight even after cancellation authority retires.
    let diagnostics: unknown;
    try { diagnostics = this.diagnostics(a, diag); } catch { diagnostics = Object.freeze({ version: 1, unavailable: "error" }); }
    try { this.ports.prepareFeedback?.(a.context, result, diagnostics); } catch {}
    if (preserveCapturedFlight) {
      // The owner sets clone/session retention intent synchronously, before feedback
      // can unblock tool completion. memoryPark only retires local authority; it
      // proves nothing about API park acceptance or preservation completion.
      this.preparePreservation(a, result);
      // Preparation may itself retire this exact context. The captured owner
      // attempt remains owed; retirement never redirects it to a successor.
    }
    this.ports.commands.resolveToolOutcome(a.entry, result);
    // Ports receive bounded, scrubbed worker-owned metadata, never raw errors/observations.
    // A bounded wait is not proof that a sink which ignores abort has stopped.
    try { await this.bounded((signal, ms) => this.ports.feedback(result, a.context, signal, ms), this.ports.now() + this.config.responseBudgetMs); } catch {}
    if (preserveCapturedFlight) {
      // The fallback is for pure monitor ports. Runtime enrollment requires an
      // independent budget supplied by the captured runner owner.
      await this.preserve(a);
    }
    return Object.freeze({ result, preserved: preserveCapturedFlight });
  }

  private preparePreservation(a: Authority, result: WorkerMemoryPressureResult): void {
    try { this.ports.preparePreservation?.(a.context, result); }
    catch { /* The captured owner operation remains owed even if preparation fails. */ }
    finally { this.ports.commands.memoryPark(a.context); }
  }

  private async preserve(a: Authority): Promise<void> {
    const budget = this.ports.preservationBudgetMs?.(a.context) ?? this.config.responseBudgetMs;
    try { await this.bounded((signal, ms) => this.ports.preserve(a.context, signal, ms), this.ports.now() + budget); } catch {}
  }

  /** Allowlisted rows built from copied strings, numbers and closed enums only; any failure yields the error marker. */
  private rejectionRows(
    captures: readonly { entry: Entry }[], failed: readonly (FailureClass | undefined)[],
    observed: readonly { entry: Entry; observation: unknown }[], admitted: readonly { entry: Entry; observation: unknown }[],
    rejected: ReturnType<Commands["selectWithRejections"]>["rejected"],
  ): unknown {
    try {
      const rows: RejectionRow[] = [];
      captures.forEach(({ entry }, index) => {
        const failure = failed[index];
        if (failure) rows.push(rejectionRow(entry.context.run_id, entry.commandId, [failure], undefined));
      });
      const kept = new Set<unknown>(admitted);
      for (const item of observed) {
        if (!kept.has(item)) rows.push(rejectionRow(item.entry.context.run_id, item.entry.commandId, ["phase"], item.observation));
      }
      for (const r of rejected) rows.push(rejectionRow(r.runId, r.commandId, r.reasons, admitted[r.index]?.observation, r.rssBytesApprox));
      return deepFreeze(rows);
    } catch { return Object.freeze({ unavailable: "error" }); }
  }

  /** At most one best-effort log record per attempt. A repeated identical no-selection record is skipped. */
  private logRejections(rejections: unknown, selected: Selection | undefined, candidateCount: number): void {
    const log = this.ports.rejections;
    if (!log) return;
    try {
      if (Array.isArray(rejections) && rejections.length === 0) { this.lastNoSelection = undefined; return; }
      if (selected) this.lastNoSelection = undefined;
      else {
        const serialized = JSON.stringify(rejections);
        if (serialized === this.lastNoSelection) return;
        this.lastNoSelection = serialized;
      }
      log({ selected_run_id: selected?.entry.context.run_id ?? null, selected_command_id: selected?.entry.commandId ?? null,
        candidate_count: candidateCount, rejections });
    } catch { /* diagnostics are best effort */ }
  }

  private async intervene(entries: readonly Entry[], deadline: number, trigger?: Trigger): Promise<void> {
    const commands = this.ports.commands;
    const captures = entries.map((entry) => ({ entry, phaseRevision: commands.phaseRevision(entry.context) }));
    // Parallel to captures; diagnostic only, never read by the authority path.
    const failed: (FailureClass | undefined)[] = captures.map(() => undefined);
    const candidates = await Promise.all(captures.map(async ({ entry, phaseRevision }, index) => {
      try { return { entry, phaseRevision, observation: await this.bounded((signal, remainingMs) => entry.observe({ signal, remainingMs }), deadline) }; }
      catch (error) {
        try { failed[index] = failureClass(error); } catch { /* diagnostics are best effort */ }
        return undefined;
      }
    }));
    if (this.closed || !this.applicable) return;
    const observed = candidates.filter((value) => value !== undefined);
    const admitted = observed.filter(({ entry, phaseRevision }) => commands.phaseAdmits(entry.context, phaseRevision));
    const { selection: selected, rejected } = commands.selectWithRejections(admitted, this.ports.now(), this.config.responseBudgetMs);
    // Synchronous, contained copy: it cannot change the selection. The warning is written only after the
    // deadline-bound steps, since a blocked stderr write must not spend the intervention budget.
    const rejections = this.rejectionRows(captures, failed, observed, admitted, rejected);
    try {
      if (!selected) return;
      const entry = selected.entry;
      const incarnation = this.ports.incarnation();
      if (!this.latest || !this.latestDecision || !incarnation || incarnation.worker_id !== entry.context.worker_id || incarnation.register_nonce !== entry.context.register_nonce) return;
      const tree: Tree = Object.freeze({
        rssBytesApprox: selected.rssBytesApprox, startedAtMs: selected.observation.startedAtMs,
        completedAtMs: selected.observation.completedAtMs, candidates: candidates.length,
        // Rejected/timed-out observations and adapter responses without complete evidence both count as failures.
        // Diagnostic only: a throwing observation leaves the count unknown and never aborts the intervention.
        failures: (() => {
          try { return candidates.length - observed.filter(({ observation }) => observation?.available === true && observation.complete === true).length; }
          catch { return undefined; }
        })(),
        rejections,
      });
      const a: Authority = Object.freeze({
        context: entry.context, phaseRevision: selected.phaseRevision, entry, incarnation: Object.freeze({ ...incarnation }),
        sample: snapshot(this.latest), decision: this.latestDecision,
        request: Object.freeze({
          run_id: entry.context.run_id, worker_id: entry.context.worker_id, register_nonce: entry.context.register_nonce,
          claim_generation: entry.context.claim_generation, memory_episode: entry.context.memory_episode,
          intervention_id: randomUUID(), policy: Object.freeze({ ...(entry.context.memory_policy ?? { version: 1, max_interventions: this.config.maxInterventions }) }),
        }),
      });
      if (!this.current(a)) return;
      const latch = commands.acquireLatch(a.context);
      if (!latch) return;
      const ticket = this.guard.beginIntervention();
      if (!ticket) { commands.releaseLatch(latch); return; }
      this.held = { ticket, latch, entry, authority: a };
      void commands.prepareOutcome(entry);
      this.record(a, "pending");
      let outcome: MemoryOutcome = "no_signal";
      let preserve = true;
      let awaiting = true;
      let cancelStarted = false;
      let cancelObservation: Observed | undefined; // stays undefined when admission is denied or fails
      try {
        if (commands.remaining(a.context) === 0) throw new Error("memory allowance exhausted");
        const ack = await this.bounded((signal, ms) => {
          const request = this.ports.reserve(a.request, signal, ms);
          void request.then(() => { if (!awaiting) this.historical(a); }, () => {});
          return request;
        }, deadline);
        awaiting = false;
        if (!this.validAck(ack, a) || !this.current(a)) throw new Error("invalid memory authority");
        commands.updateAllowance(a.context, ack.allowance.remaining);
        this.record(a, ack.admitted ? "admitted" : "denied");
        if (!ack.admitted || !ack.authorizing || commands.remaining(a.context) === undefined) throw new Error("memory admission denied");
        preserve = ack.allowance.remaining === 0;
        const observation = await this.bounded((signal, remainingMs) => entry.observe({ signal, remainingMs }), deadline);
        const authorized = () => this.ports.now() < deadline && this.current(a)
          && commands.checkBeforeCancel(selected, observation, this.ports.now(), this.config.responseBudgetMs);
        if (!authorized()) throw new Error("stale memory authority");
        cancelObservation = observation; // recorded only once checkBeforeCancel validated its rows
        outcome = "unknown"; // invoking cancellation can have an ambiguous signal result
        await this.bounded((signal, remainingMs) => {
          if (!authorized()) throw new Error("stale memory authority");
          cancelStarted = true;
          return entry.cancel(Object.freeze({ authorized: () => !signal.aborted && authorized(), remainingMs, signal }));
        }, deadline);
        if (!commands.hasConfirmedStop(entry)) {
          // Once cancellation completes, falling usage does not revoke exact-tree drain authority.
          const ownsSelected = () => this.ports.now() < deadline && this.contextCurrent(a)
            && commands.checkBeforeCancel(selected, observation, this.ports.now(), this.config.responseBudgetMs);
          if (!ownsSelected()) throw new Error("retired cancellation");
          const drain = await this.bounded((signal, remainingMs) => {
            const authorized = () => !signal.aborted && ownsSelected();
            if (!authorized()) throw new Error("stale memory authority");
            return entry.drain({ signal, remainingMs, authorized });
          }, deadline);
          commands.recordDrain(entry, drain);
        }
      } catch { awaiting = false; preserve = true; }
      // Direct proof settles only an invoked cancellation, including command cleanup during either await.
      // It grants no new authority and cannot turn a no-signal attempt into a cancellation.
      if (cancelStarted && commands.hasConfirmedStop(entry)) outcome = "confirmed_drained";
      const prepared = await this.finish(a, selected, outcome, preserve || outcome !== "confirmed_drained", { trigger, tree, cancel: cancelObservation });
      const confirmed = await this.report(a, outcome, ticket);
      if (!confirmed && !prepared.preserved) {
        this.preparePreservation(a, prepared.result);
        await this.preserve(a);
      }
      // Park can release the claim; the exact historical replay grants no authority.
      if (!confirmed && !this.closed) await this.report(a, outcome, ticket, 1);
    } finally {
      this.logRejections(rejections, selected, candidates.length);
    }
  }
}
