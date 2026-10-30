// PRD #3244: the worker's single home for lead model/effort precedence (`resolveLeadRequest`) and
// the per-claim reporter that records what the lead agent was configured to use
// (`LeadConfigReporter`, POST /api/worker/runs/{id}/lead-config).
//
// The resolver is pure and is what the executors send AND what the reporter reports, so the record
// is by construction the request. The reporter is best-effort and never throws into a turn: a
// failed, slow or refused report must not change what the run does.

import type { LeadConfigPostOutcome } from "./client.js";
import type { HarnessEffort } from "./harness.js";
import type { Logger } from "./log.js";
import type { ClaimConfig, LeadConfigReportBody, LeadEffortSource, LeadModelSource } from "./protocol.js";

export type { LeadEffortSource, LeadModelSource } from "./protocol.js";

export type LeadPhase = "plan" | "implement";

// ── Resolver ─────────────────────────────────────────────────────────────────

export interface LeadRequestInput {
  harness: "claude" | "codex";
  /** Ignored in H; a later planning-phase child selects planning values by it. */
  phase: LeadPhase;
  config: ClaimConfig | null | undefined;
  /** The lead template's delivered model. Claude passes assembleAgents' leadModel; Codex accepts it
   *  but does not use it yet. */
  templateModel?: string;
  /** Codex: the provider's fixed default model. */
  providerDefaultModel?: string;
}

export interface LeadRequest {
  model?: string;
  modelSource: LeadModelSource;
  effort?: HarnessEffort;
  effortMode: "explicit" | "runtime_default";
  effortSource: LeadEffortSource;
}

/** The lead agent's requested model and effort for one phase. Claude: `default_model || template ||
 *  undefined` (an unset model leaves the SDK key off); Codex: `default_model ?? provider default`. */
export function resolveLeadRequest(input: LeadRequestInput): LeadRequest {
  const { config } = input;
  const claimSource = config?.default_model_source ?? "builtin";
  let model: string | undefined;
  let modelSource: LeadModelSource;
  if (input.harness === "claude") {
    const claimModel = config?.default_model;
    model = claimModel || input.templateModel || undefined;
    modelSource = claimModel ? claimSource : input.templateModel ? "template" : "builtin";
  } else {
    model = config?.default_model ?? input.providerDefaultModel;
    modelSource = config?.default_model !== undefined ? claimSource : "builtin";
  }
  const effort = config?.default_effort as HarnessEffort | undefined;
  return {
    ...(model !== undefined ? { model } : {}),
    modelSource,
    ...(effort ? { effort } : {}),
    effortMode: effort ? "explicit" : "runtime_default",
    effortSource: config?.default_effort_source ?? "builtin",
  };
}

/** Reporting is active only when the claim carries `default_effort_source` (an upgraded API on a
 *  lead-lane claim). An older API, or a non-lead lane, never ships it. */
export function leadConfigActive(config: ClaimConfig | null | undefined): boolean {
  return config?.default_effort_source !== undefined;
}

// ── Reporter ─────────────────────────────────────────────────────────────────

export interface PhaseEntry {
  phase: LeadPhase;
  entry: number;
}

/** What the runtime reported, already in the stored vocabulary (the adapter mapping lives with the
 *  observation unit). All four are nullable on the wire. */
export interface LeadObservation {
  model: string | null;
  modelEvidence: string | null;
  effort: string | null;
  effortEvidence: string | null;
}

export interface LeadConfigClient {
  recordLeadConfig(runId: string, body: LeadConfigReportBody, timeoutMs?: number): Promise<LeadConfigPostOutcome>;
}

export interface LeadConfigTimers {
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

const defaultTimers: LeadConfigTimers = {
  setTimeout: (fn, ms) => {
    const h = setTimeout(fn, ms);
    h.unref?.();
    return h;
  },
  clearTimeout: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
};

/** Server bounds (leadcfg.MaxSeq / MaxPhaseEntry). */
const LEAD_CONFIG_MAX_SEQ = 32;
const LEAD_CONFIG_MAX_ENTRIES = 8;
const LEAD_CONFIG_MAX_OBSERVATIONS_PER_ENTRY = 3;
/** How long enterPhase waits for its row before the turn starts (the post then continues). */
export const LEAD_CONFIG_ENTER_WAIT_MS = 3000;
const LEAD_CONFIG_QUEUE_MAX = 4;
const POST_TIMEOUT_MS = 5000;

const NO_OBSERVATION: LeadObservation = { model: null, modelEvidence: null, effort: null, effortEvidence: null };

interface EntryState {
  entry: number;
  request: LeadRequest;
  /** The observed side of the last row posted (or allocated) for this entry. */
  lastObserved: LeadObservation;
  observationRows: number;
}

interface PhaseState {
  entries: number;
  latest?: EntryState;
  /** Last seq allocated; normal rows use 1..MAX_SEQ-1, MAX_SEQ is reserved for the truncation row. */
  lastSeq: number;
  stopped: boolean;
}

interface QueuedRow {
  body: LeadConfigReportBody;
  settled: Promise<void>;
  settle: () => void;
}

export interface LeadConfigReporterOptions {
  client: LeadConfigClient;
  runId: string;
  claimGeneration: number;
  /** Whether the claim carries `default_effort_source` (see {@link leadConfigActive}). */
  active: boolean;
  logger: Logger;
  timers?: LeadConfigTimers;
}

/** Exactly one per claim generation, constructed by the runner and carried as
 *  `RunContext.leadConfig`; never constructed by an executor, harness or epoch. */
export class LeadConfigReporter {
  private readonly client: LeadConfigClient;
  private readonly runId: string;
  private readonly claimGeneration: number;
  private readonly log: Logger;
  private readonly timers: LeadConfigTimers;
  private disabled: boolean;
  private lastPhase: LeadPhase | undefined;
  private readonly phases: Record<LeadPhase, PhaseState> = {
    plan: { entries: 0, lastSeq: 0, stopped: false },
    implement: { entries: 0, lastSeq: 0, stopped: false },
  };
  private queue: QueuedRow[] = [];
  private inFlight = false;

  constructor(opts: LeadConfigReporterOptions) {
    this.client = opts.client;
    this.runId = opts.runId;
    this.claimGeneration = opts.claimGeneration;
    this.log = opts.logger;
    this.timers = opts.timers ?? defaultTimers;
    this.disabled = !opts.active || !(opts.claimGeneration >= 1);
  }

  /** Whether rows are still being sent (inactive and disabled reporters answer false). */
  get enabled(): boolean {
    return !this.disabled;
  }

  /** Start (or continue) a phase. A new entry and one requested-side row only when the phase differs
   *  from the previous call's phase or on the first call; otherwise the current entry. Awaited by the
   *  executor before the turn starts, bounded to {@link LEAD_CONFIG_ENTER_WAIT_MS}; the post then
   *  continues in the background. Never throws. */
  async enterPhase(phase: LeadPhase, request: LeadRequest): Promise<PhaseEntry> {
    let entry: PhaseEntry;
    let wait: Promise<void> | undefined;
    try {
      const ps = this.phases[phase];
      if (this.lastPhase === phase && ps.latest) {
        return { phase, entry: ps.latest.entry };
      }
      this.lastPhase = phase;
      if (ps.entries >= LEAD_CONFIG_MAX_ENTRIES) {
        // A 9th entry: record that later changes are not kept, once, then stop the phase.
        ps.entries += 1;
        entry = { phase, entry: ps.entries };
        if (ps.latest && !ps.stopped) {
          this.log.warn("lead config: phase entry budget exhausted; recording truncation", { run_id: this.runId, phase });
          wait = this.postTruncation(phase, ps, ps.latest.request, ps.latest.lastObserved);
        }
        ps.stopped = true;
        ps.latest = { entry: ps.entries, request, lastObserved: NO_OBSERVATION, observationRows: 0 };
      } else {
        ps.entries += 1;
        const es: EntryState = { entry: ps.entries, request, lastObserved: NO_OBSERVATION, observationRows: 0 };
        ps.latest = es;
        entry = { phase, entry: es.entry };
        if (!ps.stopped) wait = this.allocateAndPost(phase, ps, es, request, NO_OBSERVATION);
      }
    } catch (err) {
      this.log.warn("lead config: enterPhase failed", { run_id: this.runId, error: errText(err) });
      return { phase, entry: this.phases[phase].latest?.entry ?? 1 };
    }
    if (wait) await this.boundedWait(wait);
    return entry;
  }

  /** Post a runtime observation for `entry` (a full-snapshot row repeating the requested side), only
   *  when `entry` is still the latest of its phase and the observed side differs from the entry's last
   *  row, within the entry's observation budget. Never throws. */
  observe(entry: PhaseEntry, obs: LeadObservation): void {
    try {
      const ps = this.phases[entry.phase];
      const es = ps.latest;
      if (!es || es.entry !== entry.entry) {
        this.log.info("lead config: dropped an observation from a superseded phase entry", {
          run_id: this.runId, phase: entry.phase, entry: entry.entry,
        });
        return;
      }
      if (ps.stopped) return;
      if (sameObservation(es.lastObserved, obs)) return;
      if (es.observationRows >= LEAD_CONFIG_MAX_OBSERVATIONS_PER_ENTRY) {
        this.log.info("lead config: dropped an observation beyond the entry budget", {
          run_id: this.runId, phase: entry.phase, entry: entry.entry,
        });
        return;
      }
      es.observationRows += 1;
      void this.allocateAndPost(entry.phase, ps, es, es.request, obs);
    } catch (err) {
      this.log.warn("lead config: observe failed", { run_id: this.runId, error: errText(err) });
    }
  }

  // ── seq allocation ─────────────────────────────────────────────────────────

  private allocateAndPost(
    phase: LeadPhase,
    ps: PhaseState,
    es: EntryState,
    request: LeadRequest,
    observed: LeadObservation,
  ): Promise<void> | undefined {
    if (ps.lastSeq + 1 >= LEAD_CONFIG_MAX_SEQ) {
      // The next row would need the last seq: it becomes the truncation row carrying the latest known values.
      this.log.warn("lead config: seq budget exhausted; recording truncation", { run_id: this.runId, phase });
      es.lastObserved = observed;
      ps.stopped = true;
      return this.postTruncation(phase, ps, request, observed, es.entry);
    }
    ps.lastSeq += 1;
    es.lastObserved = observed;
    return this.enqueue(this.body(phase, es.entry, ps.lastSeq, false, request, observed));
  }

  private postTruncation(
    phase: LeadPhase,
    ps: PhaseState,
    request: LeadRequest,
    observed: LeadObservation,
    entry = ps.latest?.entry ?? 1,
  ): Promise<void> | undefined {
    ps.lastSeq = LEAD_CONFIG_MAX_SEQ;
    return this.enqueue(this.body(phase, entry, LEAD_CONFIG_MAX_SEQ, true, request, observed));
  }

  private body(
    phase: LeadPhase,
    entry: number,
    seq: number,
    truncated: boolean,
    request: LeadRequest,
    observed: LeadObservation,
  ): LeadConfigReportBody {
    return {
      claim_generation: this.claimGeneration,
      phase,
      phase_entry: entry,
      seq,
      truncated,
      requested_model: request.model || null,
      requested_model_source: request.modelSource,
      requested_effort: request.effort ?? null,
      requested_effort_mode: request.effortMode,
      requested_effort_source: request.effortSource,
      observed_model: observed.model,
      observed_model_evidence: observed.modelEvidence,
      observed_effort: observed.effort,
      observed_effort_evidence: observed.effortEvidence,
    };
  }

  // ── delivery ───────────────────────────────────────────────────────────────

  private enqueue(body: LeadConfigReportBody): Promise<void> | undefined {
    if (this.disabled) return undefined;
    let settle!: () => void;
    const settled = new Promise<void>((resolve) => { settle = resolve; });
    this.queue.push({ body, settled, settle });
    while (this.queue.length > LEAD_CONFIG_QUEUE_MAX) {
      const dropped = this.queue.shift()!;
      this.log.warn("lead config: pending queue full; dropped the oldest row", {
        run_id: this.runId, phase: dropped.body.phase, seq: dropped.body.seq,
      });
      dropped.settle();
    }
    void this.pump();
    return settled;
  }

  private async pump(): Promise<void> {
    if (this.inFlight) return;
    this.inFlight = true;
    try {
      for (;;) {
        const item = this.queue.shift();
        if (!item) return;
        try {
          await this.deliver(item.body);
        } finally {
          item.settle();
        }
        if (this.disabled) this.drainQueue();
      }
    } catch (err) {
      this.log.warn("lead config: delivery failed", { run_id: this.runId, error: errText(err) });
    } finally {
      this.inFlight = false;
    }
  }

  private async deliver(body: LeadConfigReportBody): Promise<void> {
    let outcome = await this.send(body);
    if (outcome.kind === "transport") {
      outcome = await this.send(body); // one retry on a transport error
    }
    switch (outcome.kind) {
      case "ok":
        return;
      case "transport":
        this.log.warn("lead config: report not delivered", { run_id: this.runId, phase: body.phase, seq: body.seq });
        return;
      case "http":
        this.handleRefusal(body, outcome.status, outcome.error);
        return;
    }
  }

  private async send(body: LeadConfigReportBody): Promise<LeadConfigPostOutcome> {
    try {
      return await this.client.recordLeadConfig(this.runId, body, POST_TIMEOUT_MS);
    } catch (error) {
      return { kind: "transport", error };
    }
  }

  private handleRefusal(body: LeadConfigReportBody, status: number, code: string | undefined): void {
    const fields = { run_id: this.runId, phase: body.phase, seq: body.seq, status, ...(code ? { error: code } : {}) };
    if (status === 409 && code === "lead_config_key_conflict") {
      // A conflicting body for an existing key means the worker allocated a key twice: a worker bug.
      this.log.error("lead config: key conflict (worker bug); disabling lead config reporting", fields);
      this.disabled = true;
    } else if (status === 400 || status === 404 || (status === 409 && code === "claim_not_current")) {
      this.log.warn("lead config: report refused; disabling lead config reporting for this claim", fields);
      this.disabled = true;
    } else {
      this.log.warn("lead config: report not accepted", fields);
    }
  }

  private drainQueue(): void {
    for (const item of this.queue.splice(0)) item.settle();
  }

  private boundedWait(wait: Promise<void>): Promise<void> {
    return new Promise<void>((resolve) => {
      const handle = this.timers.setTimeout(resolve, LEAD_CONFIG_ENTER_WAIT_MS);
      void wait.then(() => {
        this.timers.clearTimeout(handle);
        resolve();
      });
    });
  }
}

function sameObservation(a: LeadObservation, b: LeadObservation): boolean {
  return a.model === b.model && a.modelEvidence === b.modelEvidence &&
    a.effort === b.effort && a.effortEvidence === b.effortEvidence;
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
