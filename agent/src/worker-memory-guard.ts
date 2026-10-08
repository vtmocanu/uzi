import { randomUUID } from "node:crypto";
import type { WorkerMemoryCommands } from "./worker-memory-commands.js";
import type { MemoryIncarnation, MemoryOutcome, MemoryReservation, MemoryReservationRequest, MemoryOutcomeRequest, WorkerMemoryPressureResult } from "./protocol.js";
import type { Config } from "./config.js";
import type { WorkerMemoryReader } from "./worker-memory-reader.js";

type Settings = Extract<NonNullable<Config["memoryGuard"]>, { enabled: true }>;
type Sample = ReturnType<WorkerMemoryReader["sample"]>;
type Available = Extract<Sample, { available: true }>;
type Intervention = Readonly<{ sequence: number }>;
type Decision = { kind: "inhibited" | "idle" | "hard" | "early" | "rearmed"; highBytes?: number; quietBytes?: number };

const STAT_FIELDS = ["anon", "shmem", "slab_unreclaimable", "unevictable"] as const;
function natural(n: number): boolean { return Number.isSafeInteger(n) && n >= 0; }
function positive(n: number): boolean { return natural(n) && n > 0; }

/** Pure total-usage policy. No timers, signals, process inspection or reservation IO.
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
      || !sample.stat || !sample.pressure?.full || !sample.pressure.some) return false;
    if (!STAT_FIELDS.every((field) => natural(sample.stat[field]))) return false;
    return [sample.pressure.full, sample.pressure.some].every((psi) => natural(psi.total)
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
    this.previous = {
      ...sample, stat: { ...sample.stat },
      pressure: { some: { ...sample.pressure.some }, full: { ...sample.pressure.full } },
    };
    if (!this.armed) {
      const ready = this.intervention !== undefined && this.drained && this.resolved;
      if (!ready || sample.currentBytes >= quietBytes) this.dwell = undefined;
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
        this.dwell = ready && sample.currentBytes < quietBytes ? sample.completedAtMs : undefined;
      }
      return { kind: "idle", ...thresholds };
    }
    if (this.state.pendingIntervention) return { kind: "inhibited", ...thresholds };
    if (sample.currentBytes >= highBytes) {
      this.proposal = true;
      return { kind: "hard", ...thresholds };
    }
    if (previous && previous.limitBytes === sample.limitBytes
      && now - previous.startedAtMs <= this.config.sampleMs + this.config.responseBudgetMs
      && sample.completedAtMs > previous.completedAtMs
      && sample.pressure.full.total > previous.pressure.full.total
      && STAT_FIELDS.some((field) => sample.stat[field] > previous.stat[field])) {
      const totalGrowth = Math.max(0, sample.currentBytes - previous.currentBytes);
      const rate = totalGrowth / (sample.completedAtMs - previous.completedAtMs);
      if (sample.currentBytes + rate * this.config.responseBudgetMs >= highBytes) {
        this.proposal = true;
        return { kind: "early", ...thresholds };
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
  entry: Entry;
  incarnation: MemoryIncarnation;
  sample: Available;
}>;
type Journal = Readonly<{ request: Readonly<MemoryReservationRequest>; status: "pending" | "ambiguous" | "admitted" | "denied"; outcome?: MemoryOutcome }>;

/** Worker-owned async orchestration. Each IO races an independent deadline even
 * when a port ignores its signal. A pass observes at most 256 enrolled trees;
 * failed observations disqualify siblings independently. No fallback after selection.
 */
export class WorkerMemoryMonitor {
  private readonly guard: WorkerMemoryGuard;
  private readonly pressure: WorkerMemoryGuard;
  private latest: Available | undefined;
  private applicable = false;
  private closed = false;
  private pending: Promise<void> | undefined;
  private readonly controllers = new Set<AbortController>();
  private readonly reports = new Set<Promise<void>>();
  private readonly journal: Journal[] = [];
  private held: { ticket: Intervention; latch: NonNullable<ReturnType<Commands["acquireLatch"]>>; entry: Entry; authority: Authority } | undefined;
  private unsubscribe: (() => void) | undefined;
  private readonly unsubscribeDrain: () => void;
  private running: Promise<void> | undefined;
  private interruptWait: (() => void) | undefined;

  constructor(private readonly config: Settings, private readonly ports: {
    sample: () => Sample;
    now: () => number;
    commands: Commands;
    incarnation: () => MemoryIncarnation | undefined;
    subscribeInvalidation?: (callback: () => void) => () => void;
    reserve: (request: MemoryReservationRequest, signal: AbortSignal, remainingMs: number) => Promise<MemoryReservation>;
    report: (request: MemoryOutcomeRequest, signal: AbortSignal, remainingMs: number) => Promise<MemoryReservation>;
    feedback: (result: WorkerMemoryPressureResult, context: Context) => Promise<void>;
    preserve: (context: Context) => Promise<void>;
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

  private timer(callback: () => void, ms: number): () => void {
    if (this.ports.timer) return this.ports.timer(callback, ms);
    const handle = setTimeout(callback, ms);
    return () => clearTimeout(handle);
  }

  private abort(): void { for (const controller of this.controllers) controller.abort(); }

  private async bounded<T>(work: (signal: AbortSignal, ms: number) => Promise<T>, deadline: number): Promise<T> {
    const ms = Math.floor(deadline - this.ports.now());
    if (!Number.isSafeInteger(ms) || ms < 1 || ms > 2147483647 || this.closed) throw new Error("memory deadline");
    const controller = new AbortController();
    this.controllers.add(controller);
    let clear = () => {};
    let onAbort = () => {};
    const timeout = new Promise<never>((_, reject) => {
      onAbort = () => reject(new Error("memory IO interrupted"));
      controller.signal.addEventListener("abort", onAbort, { once: true });
      clear = this.timer(() => controller.abort(), ms);
    });
    try { return await Promise.race([Promise.resolve().then(() => {
      if (controller.signal.aborted) throw new Error("memory IO interrupted");
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
    const input = this.ports.sample();
    const now = this.ports.now();
    const pressure = this.pressure.evaluate(input, now);
    this.applicable = pressure.kind === "hard" || pressure.kind === "early";
    this.latest = pressure.kind !== "inhibited" && input.available ? {
      ...input, stat: { ...input.stat }, pressure: { some: { ...input.pressure.some }, full: { ...input.pressure.full } },
    } : undefined;
    const decision = this.guard.evaluate(input, now);
    if (decision.kind === "rearmed" && this.held) {
      this.ports.commands.releaseLatch(this.held.latch);
      this.held = undefined;
    }
    if (this.pending || (decision.kind !== "hard" && decision.kind !== "early")) return;
    const entries = this.ports.commands.eligibleEntries();
    if (!entries?.length) return;
    const deadline = now + this.config.responseBudgetMs;
    this.pending = this.intervene(entries, deadline).catch(() => {}).finally(() => { this.pending = undefined; });
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
      && this.ports.commands.isCurrent(a.context)
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

  private async report(a: Authority, outcome: MemoryOutcome, ticket?: Intervention): Promise<void> {
    this.record(a, "ambiguous", outcome);
    try {
      const ack = await this.bounded((signal, ms) => this.ports.report({ ...a.request, outcome }, signal, ms),
        this.ports.now() + this.config.responseBudgetMs);
      // Historical proof resolves only this held ticket; it never changes a context or allowance.
      if (this.validAck(ack, a, outcome) && ticket && this.held?.ticket === ticket
        && this.held.entry === a.entry && this.held.authority === a) this.guard.resolveOutcome(ticket);
    } catch { /* unresolved outcome retains the global fence */ }
  }

  private historical(a: Authority): void {
    if (this.closed) { this.record(a, "ambiguous", "no_signal"); return; }
    const task = this.report(a, "no_signal");
    this.reports.add(task);
    void task.finally(() => this.reports.delete(task));
  }

  private async finish(a: Authority, selected: Selection, outcome: MemoryOutcome, preserve: boolean): Promise<void> {
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
    this.ports.commands.resolveToolOutcome(a.entry, result);
    // Ports receive bounded, scrubbed worker-owned metadata, never raw errors/observations.
    try { await this.bounded(() => this.ports.feedback(result, a.context), this.ports.now() + this.config.responseBudgetMs); } catch {}
    if (preserve && this.ports.commands.memoryPark(a.context)) {
      try { await this.bounded(() => this.ports.preserve(a.context), this.ports.now() + this.config.responseBudgetMs); } catch {}
    }
  }

  private async intervene(entries: readonly Entry[], deadline: number): Promise<void> {
    const commands = this.ports.commands;
    const candidates = await Promise.all(entries.map(async (entry) => {
      try { return { entry, observation: await this.bounded((signal, remainingMs) => entry.observe({ signal, remainingMs }), deadline) }; }
      catch { return undefined; }
    }));
    if (this.closed || !this.applicable) return;
    const selected = commands.select(candidates.filter((value) => value !== undefined), this.ports.now(), this.config.responseBudgetMs);
    if (!selected) return;
    const entry = selected.entry;
    const incarnation = this.ports.incarnation();
    if (!this.latest || !incarnation || incarnation.worker_id !== entry.context.worker_id || incarnation.register_nonce !== entry.context.register_nonce) return;
    const a: Authority = Object.freeze({
      context: entry.context, entry, incarnation: Object.freeze({ ...incarnation }),
      sample: Object.freeze({ ...this.latest, stat: Object.freeze({ ...this.latest.stat }),
        pressure: Object.freeze({ some: Object.freeze({ ...this.latest.pressure.some }), full: Object.freeze({ ...this.latest.pressure.full }) }) }),
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
    await this.finish(a, selected, outcome, preserve);
    await this.report(a, outcome, ticket);
  }
}
