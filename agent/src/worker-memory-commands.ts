import { randomUUID } from "node:crypto";
import type { MemoryPolicy, WorkerMemoryPressureResult } from "./protocol.js";
import { makeTextRedactor } from "./redact.js";
import { sanitizeForLog } from "./run-quiescence.js";

type Root = Readonly<{ pid: number; startTime: number }>;
type ContextInput = {
  run_id: string;
  worker_id: string;
  register_nonce: string;
  claim_generation: number;
  memory_episode: number;
  flight: object;
  lifecycleRevision: number;
  memory_policy?: MemoryPolicy;
  memory_intervention_count?: number;
};
type Context = Readonly<ContextInput & { identity: string }>;
type Latch = Readonly<{ context: Context; identity: string }>;
type Observation = {
  available: boolean;
  complete: boolean;
  startedAtMs: number;
  completedAtMs: number;
  rows: readonly (Root & { rssBytes: number })[];
};
type Budget = Readonly<{ remainingMs: number; signal: AbortSignal }>;
type Entry = Readonly<{
  identity: string;
  context: Context;
  source: "codex-model-command";
  toolId: string;
  attemptId: string;
  commandId: string;
  root: Root;
  command: string;
  observe: (budget?: Budget) => Promise<Observation>;
  cancel: (authority: Budget & Readonly<{ authorized: () => boolean }>) => Promise<void>;
  drain: (budget?: Budget & Readonly<{ authorized: () => boolean }>) => Promise<{ confirmed: boolean; detail?: string }>;
}>;
type Selection = Readonly<{ entry: Entry; rssBytesApprox: number; observation: Observation }>;
type ProtectedRoot = Readonly<{ identity: string; kind: "worker" | "provider" | "boundary" | "sink"; root: Root }>;

function uuid(value: string): boolean {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value)
    && value !== "00000000-0000-0000-0000-000000000000";
}
function positive(n: number): boolean { return Number.isSafeInteger(n) && n > 0; }
function rootValid(root: Root): boolean { return positive(root.pid) && positive(root.startTime); }
function key(root: Root): string { return `${root.pid}:${root.startTime}`; }
function fresh(o: Observation, now: number, age: number): boolean {
  return o.available === true && o.complete === true && Number.isFinite(now) && now >= 0
    && Number.isFinite(age) && age > 0 && Number.isFinite(o.startedAtMs)
    && o.startedAtMs >= 0 && Number.isFinite(o.completedAtMs)
    && o.startedAtMs <= o.completedAtMs && o.completedAtMs <= now
    && now - o.startedAtMs <= age && Array.isArray(o.rows)
    && o.rows.length > 0 && o.rows.length <= 256;
}

/** Worker-owned enrollment only. This registry neither launches nor inspects processes.
 * Retired entries and failed drain evidence survive authority invalidation.
 */
export class WorkerMemoryCommands {
  private readonly contexts = new Map<string, Context>();
  private readonly latches = new Map<Context, Latch>();
  private readonly entries = new Map<string, Entry>();
  private readonly drained = new WeakSet<Entry>();
  private readonly failures = new Map<Entry, string>();
  private readonly protectedRoots = new Set<ProtectedRoot>();
  private closed = false;
  private readonly drainListeners = new Set<(entry: Entry) => void>();

  subscribeConfirmedDrain(callback: (entry: Entry) => void): () => void {
    this.drainListeners.add(callback);
    return () => { this.drainListeners.delete(callback); };
  }
  private readonly outcomes = new WeakMap<Entry, { promise: Promise<WorkerMemoryPressureResult>; resolve: (result: WorkerMemoryPressureResult) => void; settled: boolean }>();
  private readonly allowances = new WeakMap<Context, number>();

  remaining(context: Context): number | undefined { return this.allowances.get(context); }

  updateAllowance(context: Context, remaining: number): boolean {
    if (!this.isCurrent(context)) return false;
    this.allowances.set(context, remaining);
    return true;
  }

  prepareOutcome(entry: Entry): Promise<WorkerMemoryPressureResult> {
    const existing = this.outcomes.get(entry);
    if (existing) return existing.promise;
    let resolve!: (result: WorkerMemoryPressureResult) => void;
    const promise = new Promise<WorkerMemoryPressureResult>((done) => { resolve = done; });
    this.outcomes.set(entry, { promise, resolve, settled: false });
    return promise;
  }

  outcome(entry: Entry): Promise<WorkerMemoryPressureResult> | undefined { return this.outcomes.get(entry)?.promise; }

  resolveToolOutcome(entry: Entry, result: WorkerMemoryPressureResult): boolean {
    const slot = this.outcomes.get(entry);
    if (!slot || slot.settled || !result
      || result.run_id !== entry.context.run_id || result.worker_id !== entry.context.worker_id
      || result.claim_generation !== entry.context.claim_generation || result.memory_episode !== entry.context.memory_episode
      || result.command_id !== entry.commandId || result.tool_id !== entry.toolId || result.attempt_id !== entry.attemptId
      || !uuid(result.intervention_id) || result.code !== "worker_memory_pressure" || result.command !== entry.command
      || ![result.observed_total_bytes, result.tree_rss_bytes_approx].every((n) => Number.isSafeInteger(n) && n >= 0)
      || !positive(result.limit_bytes) || !["no_signal", "unknown", "confirmed_drained"].includes(result.outcome)
      || result.cancellation_confirmed !== (result.outcome === "confirmed_drained") || result.retry_blocked !== true
      || result.guidance !== "Use smaller or serial commands after recovery.") return false;
    // The exact prepared slot belongs to the original caller even after unregister.
    slot.settled = true;
    slot.resolve(Object.freeze({ ...result }));
    return true;
  }

  constructor(private readonly secrets: readonly string[] = []) {}

  begin(input: ContextInput): Context {
    if (this.closed || !uuid(input.run_id) || !uuid(input.worker_id) || typeof input.register_nonce !== "string" || !input.register_nonce || Buffer.byteLength(input.register_nonce) > 128
      || !Number.isSafeInteger(input.claim_generation) || input.claim_generation <= 0
      || !Number.isSafeInteger(input.memory_episode) || input.memory_episode < 0
      || !Number.isSafeInteger(input.lifecycleRevision) || input.lifecycleRevision < 0
      || typeof input.flight !== "object" || input.flight === null) throw new Error("invalid memory context");
    const policy = input.memory_policy;
    if (policy && (policy.version !== 1 || !positive(policy.max_interventions) || policy.max_interventions > 9999)) throw new Error("invalid memory policy");
    if (input.memory_intervention_count !== undefined && (!Number.isSafeInteger(input.memory_intervention_count)
      || input.memory_intervention_count < 0 || input.memory_intervention_count > 9999 || (policy && input.memory_intervention_count > policy.max_interventions))) throw new Error("invalid memory count");
    const old = this.contexts.get(input.run_id);
    if (old) this.retire(old);
    const context = Object.freeze({ ...input, ...(policy ? { memory_policy: Object.freeze({ ...policy }) } : {}), identity: randomUUID() });
    if (policy) this.allowances.set(context, policy.max_interventions - (input.memory_intervention_count ?? 0));
    this.contexts.set(context.run_id, context);
    return context;
  }

  isCurrent(context: Context, flight = context.flight, revision = context.lifecycleRevision): boolean {
    return !this.closed && this.contexts.get(context.run_id) === context
      && context.flight === flight && context.lifecycleRevision === revision;
  }

  retire(context: Context): boolean {
    if (!this.isCurrent(context)) return false;
    this.contexts.delete(context.run_id);
    this.latches.delete(context);
    return true;
  }

  memoryPark(context: Context): boolean { return this.retire(context); }

  registrationRotated(): void {
    this.contexts.clear();
    this.latches.clear();
  }

  shutdown(): void {
    this.registrationRotated();
    this.closed = true;
  }

  acquireLatch(context: Context): Latch | undefined {
    if (!this.isCurrent(context) || this.latches.has(context)) return undefined;
    const latch = Object.freeze({ context, identity: randomUUID() });
    this.latches.set(context, latch);
    return latch;
  }

  releaseLatch(latch: Latch): boolean {
    if (!this.isCurrent(latch.context) || this.latches.get(latch.context) !== latch) return false;
    this.latches.delete(latch.context);
    return true;
  }

  admitsCommands(context: Context): boolean {
    return this.isCurrent(context) && !this.latches.has(context);
  }

  register(context: Context, input: Omit<Entry, "identity" | "context" | "command" | "root" | "commandId"> & {
    command: string; root: Root;
  }): Entry {
    if (!this.admitsCommands(context) || input.source !== "codex-model-command"
      || !rootValid(input.root) || ![input.toolId, input.attemptId].every((id) => typeof id === "string" && id.length > 0
        && Buffer.byteLength(id) <= 256 && !/[\p{Cc}\u061c\u200e\u200f\u2028-\u202e\u2066-\u2069]/u.test(id))
      || typeof input.observe !== "function" || typeof input.cancel !== "function"
      || typeof input.drain !== "function") throw new Error("untrusted memory command");
    const redact = makeTextRedactor([...this.secrets]);
    const toolId = redact(input.toolId);
    const attemptId = redact(input.attemptId);
    if (Buffer.byteLength(toolId) > 256 || Buffer.byteLength(attemptId) > 256) throw new Error("untrusted memory command IDs");
    const entry: Entry = Object.freeze({
      source: input.source, toolId, attemptId, commandId: randomUUID(),
      observe: input.observe, cancel: input.cancel, drain: input.drain,
      identity: randomUUID(), context, root: Object.freeze({ ...input.root }),
      command: sanitizeForLog(makeTextRedactor([...this.secrets])(input.command), 1021),
    });
    this.entries.set(entry.identity, entry);
    return entry;
  }

  isRegistered(entry: Entry): boolean {
    return this.entries.get(entry.identity) === entry && this.isCurrent(entry.context)
      && !this.drained.has(entry);
  }

  recordDrain(entry: Entry, result: { confirmed: boolean; detail?: string }): boolean {
    if (this.entries.get(entry.identity) !== entry) return false;
    if (result.confirmed === true) {
      this.drained.add(entry);
      this.failures.delete(entry);
      for (const callback of this.drainListeners) callback(entry);
    } else {
      this.failures.set(entry, sanitizeForLog(makeTextRedactor([...this.secrets])(result.detail ?? "unconfirmed drain")));
    }
    return true;
  }

  /** Direct recordDrain confirmation for this exact entry survives unregister without retaining it. */
  hasConfirmedStop(entry: Entry): boolean { return this.drained.has(entry); }

  drainFailure(entry: Entry): string | undefined { return this.failures.get(entry); }

  unregister(entry: Entry): boolean {
    if (this.entries.get(entry.identity) !== entry || !this.drained.has(entry)) return false;
    this.entries.delete(entry.identity);
    this.failures.delete(entry);
    return true;
  }

  protect(kind: ProtectedRoot["kind"], root: Root): ProtectedRoot {
    if (!["worker", "provider", "boundary", "sink"].includes(kind) || !rootValid(root)) throw new Error("invalid protected root");
    const protectedRoot = Object.freeze({ identity: randomUUID(), kind, root: Object.freeze({ ...root }) });
    this.protectedRoots.add(protectedRoot);
    return protectedRoot;
  }

  unprotect(root: ProtectedRoot): boolean { return this.protectedRoots.delete(root); }

  /** Bound before copying or observing; retained retired entries count toward the cap. */
  eligibleEntries(): readonly Entry[] | undefined {
    if (this.entries.size > 256 || this.protectedRoots.size > 256) return undefined;
    const eligible: Entry[] = [];
    for (const entry of this.entries.values()) if (this.isRegistered(entry)) eligible.push(entry);
    return Object.freeze(eligible);
  }

  /** At most 256 observations and 256 rows per observation. Every overlap invalidates
   * all affected candidates; malformed siblings cannot prevent valid independent work.
   * RSS is approximate and is never cgroup usage or a drain/snapshot measurement.
   */
  select(candidates: readonly { entry: Entry; observation: Observation }[], now: number, maxAgeMs: number): Selection | undefined {
    if (candidates.length > 256 || this.eligibleEntries() === undefined) return undefined;
    const protectedPids = new Set<number>();
    for (const root of this.protectedRoots) protectedPids.add(root.root.pid);
    const enrolledPids = new Map<number, Entry[]>();
    for (const entry of this.entries.values()) {
      if (this.drained.has(entry)) continue;
      const owners = enrolledPids.get(entry.root.pid) ?? [];
      owners.push(entry);
      enrolledPids.set(entry.root.pid, owners);
    }
    const invalid = new Set<number>();
    const owners = new Map<string, number[]>();
    const pidStarts = new Map<number, Set<number>>();
    const totals = new Map<number, number>();
    const entryOwners = new Map<Entry, number[]>();
    candidates.forEach(({ entry, observation }, index) => {
      const enrolled = entryOwners.get(entry) ?? [];
      enrolled.push(index);
      entryOwners.set(entry, enrolled);
      if (!this.isRegistered(entry) || !fresh(observation, now, maxAgeMs)) invalid.add(index);
      if (!Array.isArray(observation.rows) || observation.rows.length > 256) return;
      let total = 0;
      let hasRoot = false;
      const seen = new Set<string>();
      for (const row of observation.rows) {
        if (!row || !rootValid(row) || !Number.isSafeInteger(row.rssBytes) || row.rssBytes < 0) {
          invalid.add(index);
          continue;
        }
        const identity = key(row);
        if (seen.has(identity)) invalid.add(index);
        seen.add(identity);
        const indices = owners.get(identity) ?? [];
        indices.push(index);
        owners.set(identity, indices);
        const starts = pidStarts.get(row.pid) ?? new Set<number>();
        starts.add(row.startTime);
        pidStarts.set(row.pid, starts);
        if (identity === key(entry.root)) hasRoot = true;
        // A PID collision with protected worker data is conservative, including reuse.
        if (protectedPids.has(row.pid)
          || enrolledPids.get(row.pid)?.some((other) => other !== entry)) invalid.add(index);
        total += row.rssBytes;
        if (!Number.isSafeInteger(total)) invalid.add(index);
      }
      if (!hasRoot) invalid.add(index);
      totals.set(index, total);
    });
    for (const indices of entryOwners.values()) if (indices.length > 1) for (const index of indices) invalid.add(index);
    for (const [identity, indices] of owners) {
      const pid = Number(identity.split(":")[0]);
      if (indices.length > 1 || (pidStarts.get(pid)?.size ?? 0) > 1) for (const index of indices) invalid.add(index);
    }
    const valid: Selection[] = [];
    candidates.forEach(({ entry, observation }, index) => {
      const rssBytesApprox = totals.get(index);
      if (!invalid.has(index) && rssBytesApprox !== undefined) {
        valid.push(Object.freeze({ entry, rssBytesApprox, observation: Object.freeze({
          ...observation, rows: Object.freeze(observation.rows.map((row) => Object.freeze({ ...row }))),
        }) }));
      }
    });
    valid.sort((a, b) => b.rssBytesApprox - a.rssBytesApprox
      || a.entry.root.pid - b.entry.root.pid || a.entry.root.startTime - b.entry.root.startTime
      || a.entry.identity.localeCompare(b.entry.identity));
    return valid[0];
  }

  /** The next adapter must obtain a fresh complete observation immediately before cancel.
   * Exact entry/context and root identity are checked again; no PID-only authority.
   */
  checkBeforeCancel(selection: Selection, observation: Observation, now: number, maxAgeMs: number): boolean {
    return this.isRegistered(selection.entry)
      && this.select([{ entry: selection.entry, observation }], now, maxAgeMs) !== undefined;
  }
}
