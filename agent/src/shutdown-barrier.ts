import type { AuthoritySnapshot, AuthorityRuntime } from "./authority-runtime.js";
import type { GitCache } from "./git.js";
import type { Logger } from "./log.js";

// issue #4066 - the worker shutdown barrier.
//
// Before the barrier a graceful worker exit trusted that every authority settlement was durable once worker.run
// returned. It is not: a settlement can be queued behind a holder of A, spliced out of the queue by a drain that has
// not finished, applied inline by code detached from the transaction that owned it, or owed by a child or a pin
// continuation the caller's deadline abandoned. The barrier joins all of that for real, seals new authority
// transactions, drains what is queued and reports ONE decision (`worker_shutdown_result`): drained, or incomplete
// with fixed reasons. It never cancels a join and never throws. An incomplete result never certifies anything: any
// unresolved record keeps its protections (a successor still refuses it), while a write already started may still
// complete after the diagnostic; incomplete can also occur with no enrolled operation.
//
// Failure model. A settlement is durable once its slot was written with writeDurable and the directory sync
// returned. The proofs are process-crash proofs; a power loss before that sync can lose the rename (the previous
// `running` slot survives) or keep it (the slot is one ahead of its head and the next transaction completes it).
// Neither is re-proven here.

/** The fixed set of reasons a shutdown can be incomplete, in the order they are reported. Never free text. */
export const SHUTDOWN_REASONS = [
  "deadline",
  "prerequisite_pending",
  "producer_pending",
  "inline_apply_in_flight",
  "open_settlement",
  "settlement_failed",
  "bare_slot_end_failed",
  "enrollment_uncertain",
  "rollback_failed",
  "queued_settlement",
  "queued_source_end",
  "apply_exhausted",
  "drain_in_flight",
  "transaction_in_flight",
  "unsettled_delete",
  "pin_continuation",
] as const;

export type ShutdownReason = (typeof SHUTDOWN_REASONS)[number];

/** The decision logged once as `worker_shutdown_result`: no paths, footprints or error text. */
export interface ShutdownResult {
  result: "drained" | "incomplete";
  reasons: ShutdownReason[];
  counts: Record<string, number>;
  /** At most 16 operation UUIDs. */
  operations: string[];
  elapsed_ms: number;
  budget_ms: number;
}

/** What the caller acts on: incomplete always carries exit code 1. */
export interface ShutdownGate {
  result: ShutdownResult["result"];
  exitCode: 0 | 1;
}

export type ShutdownPhase = "prerequisites_joined" | "sealed" | "final_drain_started";

/** Tests only: production-inert observation points. */
export interface ShutdownTestHooks {
  onShutdownPhase?: (phase: ShutdownPhase) => void | Promise<void>;
}

export type ShutdownLog = Pick<Logger, "info" | "warn">;

/** The ledger as the observer reads it; an {@link AuthoritySnapshot} satisfies it. */
export type ShutdownSnapshot = Pick<AuthoritySnapshot, "counts" | "reasons" | "operations">;

export interface ShutdownResultDeadlineOptions {
  /** Absolute wall-clock deadline in ms since the epoch. */
  deadlineAt: number;
  log: ShutdownLog;
  /** Read at the deadline only. May be unbound (undefined) when the authority runtime does not exist yet. */
  snapshot?: () => ShutdownSnapshot | undefined;
}

export interface ShutdownResultDeadlineHandle {
  clear(): void;
}

const MAX_OPERATIONS = 16;

interface Observer {
  readonly deadlineAt: number;
  readonly armedAt: number;
  readonly log: ShutdownLog;
  readonly snapshot: (() => ShutdownSnapshot | undefined) | undefined;
  timer: NodeJS.Timeout | undefined;
  /** Set once a decision was logged (by the deadline or by the barrier). */
  decided: boolean;
  /** The barrier reached its final drain: the prerequisites are joined, so the observer no longer reports prerequisite_pending. It stays live (first decision wins). */
  finalDrainStarted: boolean;
}

let observer: Observer | undefined;
/** The snapshot source the barrier binds once it has an authority runtime (see {@link finishWorkerShutdown}). */
let boundSnapshot: (() => ShutdownSnapshot) | undefined;
const finishing = new WeakMap<AuthorityRuntime, Promise<ShutdownGate>>();

const orderedReasons = (reasons: Iterable<ShutdownReason>): ShutdownReason[] => {
  const set = new Set(reasons);
  return SHUTDOWN_REASONS.filter((reason) => set.has(reason));
};

const gateOf = (result: ShutdownResult["result"]): ShutdownGate => ({ result, exitCode: result === "drained" ? 0 : 1 });

function logDecision(log: ShutdownLog, decision: ShutdownResult): void {
  const fields = { ...decision };
  if (decision.result === "drained") log.info("worker_shutdown_result", fields);
  else log.warn("worker_shutdown_result", fields);
}

/**
 * Armed by the caller at the first SIGTERM/SIGINT, or at barrier entry when no signal came (idempotent: a later
 * call changes nothing). One ref'd timer: if the barrier has not decided by `deadlineAt` it logs
 * `worker_shutdown_result` incomplete (with `prerequisite_pending` unless the final drain had already started, plus
 * what the ledger shows), sets process.exitCode = 1 and marks the decision. It never cancels a join:
 * the barrier keeps joining and the process still exits only when it is done.
 */
export function armShutdownResultDeadline(options: ShutdownResultDeadlineOptions): ShutdownResultDeadlineHandle {
  if (observer === undefined) {
    const armed: Observer = {
      deadlineAt: options.deadlineAt, armedAt: Date.now(), log: options.log, snapshot: options.snapshot,
      timer: undefined, decided: false, finalDrainStarted: false,
    };
    armed.timer = setTimeout(() => {
      armed.timer = undefined;
      if (armed.decided) return;
      armed.decided = true;
      let snapshot: ShutdownSnapshot | undefined;
      try { snapshot = (armed.snapshot ?? boundSnapshot)?.(); } catch { snapshot = undefined; }
      const decision: ShutdownResult = {
        result: "incomplete",
        reasons: orderedReasons(["deadline", ...(armed.finalDrainStarted ? [] : ["prerequisite_pending" as const]), ...(snapshot?.reasons ?? [])]),
        counts: snapshot?.counts ?? {},
        operations: (snapshot?.operations ?? []).slice(0, MAX_OPERATIONS),
        elapsed_ms: Date.now() - armed.armedAt,
        budget_ms: armed.deadlineAt - armed.armedAt,
      };
      process.exitCode = 1;
      try { logDecision(armed.log, decision); } catch { /* a failing logger must not mask the exit code */ }
    }, Math.max(0, options.deadlineAt - Date.now()));
    observer = armed;
  }
  const mine = observer;
  return {
    clear(): void {
      if (mine.timer !== undefined) { clearTimeout(mine.timer); mine.timer = undefined; }
      if (observer === mine) observer = undefined;
    },
  };
}

export interface FinishWorkerShutdownOptions {
  authority: AuthorityRuntime;
  git?: GitCache;
  deadlineAt: number;
  log: ShutdownLog;
  testHooks?: ShutdownTestHooks;
}

/**
 * The one shutdown sequence main.ts and the crash-proof fixture both run, after the caller joined worker.run:
 *  1. beginShutdown (fixes the budget);
 *  2. join the prerequisites for real - registered producers, GitCache.pendingWork() (unsettled runner-uid deletes and
 *     abandoned pin continuations, each through its authority settle), every outermost transaction and every inline
 *     apply - with no timeout (the observer reports past the deadline, nothing is cancelled);
 *  3. seal (a new outermost transaction is refused with AuthorityShutdownRefusal);
 *  4. drain what is queued (the wait is bounded by the deadline; an apply already running is not, so the observer
 *     stays live through this step and reports at the deadline if the drain has not decided);
 *  5. log the decision once (unless the observer already did), incomplete => process.exitCode = 1;
 *  6. join whatever is still running without starting anything.
 * Never throws; a second call for the same runtime returns the first call's result.
 */
export function finishWorkerShutdown(options: FinishWorkerShutdownOptions): Promise<ShutdownGate> {
  const existing = finishing.get(options.authority);
  if (existing !== undefined) return existing;
  const run = runBarrier(options).catch((): ShutdownGate => { process.exitCode = 1; return gateOf("incomplete"); });
  finishing.set(options.authority, run);
  return run;
}

async function runBarrier(options: FinishWorkerShutdownOptions): Promise<ShutdownGate> {
  const { authority, git, deadlineAt, log } = options;
  const phase = async (name: ShutdownPhase): Promise<void> => {
    const hook = options.testHooks?.onShutdownPhase;
    if (hook !== undefined) await hook(name);
  };
  const external = (): { unsettled_delete: number; pin_continuation: number } => ({
    unsettled_delete: git?.unsettledDeleteCount() ?? 0, pin_continuation: git?.pendingArchivedPinContinuations() ?? 0,
  });
  const startedAt = Date.now();
  boundSnapshot = () => authority.snapshot(external());
  const watching = observer;
  authority.beginShutdown(deadlineAt);

  let outcome: ShutdownResult | undefined;
  try {
    // 2. Prerequisites: joined for real. A new one can appear while another is joined, so loop to a quiet point.
    for (;;) {
      await authority.joinPrerequisites();
      await git?.pendingWork();
      if (!authority.prerequisitesPending() && (git?.unsettledDeleteCount() ?? 0) === 0 &&
          (git?.pendingArchivedPinContinuations() ?? 0) === 0) break;
    }
    await phase("prerequisites_joined");
    // 3. Seal.
    authority.sealForShutdown();
    await phase("sealed");
    // 4. The final drain. The observer stays live: whichever of the drain and the observer decides first logs once.
    if (watching !== undefined) watching.finalDrainStarted = true;
    await phase("final_drain_started");
    const drained = await authority.drainForShutdown({ deadlineAt, external });
    const reasons = new Set<ShutdownReason>(drained.reasons);
    if (drained.result === "incomplete" && Date.now() >= deadlineAt) reasons.add("deadline");
    const from = watching?.armedAt ?? startedAt;
    outcome = {
      result: drained.result, reasons: orderedReasons(reasons), counts: drained.counts,
      operations: drained.operations.slice(0, MAX_OPERATIONS),
      elapsed_ms: Date.now() - from, budget_ms: deadlineAt - from,
    };
  } catch {
    outcome = {
      result: "incomplete", reasons: ["prerequisite_pending"], counts: {}, operations: [],
      elapsed_ms: Date.now() - startedAt, budget_ms: deadlineAt - startedAt,
    };
  }

  // 5. One decision. When the deadline observer already reported, that report stands and the exit code is 1.
  let gate: ShutdownGate;
  if (watching?.decided === true) {
    gate = gateOf("incomplete");
  } else {
    if (watching !== undefined) watching.decided = true;
    try { logDecision(log, outcome); } catch { /* the decision below still stands */ }
    gate = gateOf(outcome.result);
  }
  if (gate.exitCode === 1) process.exitCode = 1;
  authority.markShutdownDecided();
  if (watching !== undefined) {
    if (watching.timer !== undefined) { clearTimeout(watching.timer); watching.timer = undefined; }
    if (observer === watching) observer = undefined;
  }
  // 6. The decision is out: join the stragglers (a late settle stays queued; nothing new starts).
  await authority.joinAfterDecision().catch(() => undefined);
  await git?.pendingWork().catch(() => undefined);
  return gate;
}
