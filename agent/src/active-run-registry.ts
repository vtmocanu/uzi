import type {
  ActiveSnapshot,
  ActiveSnapshotEntry,
  ActiveSnapshotPhase,
  FinalizeResumeEntry,
} from "./protocol.js";
import type { PendingTerminal, PendingPolicy } from "./outbox.js";

/** PRD #1391 Run B M4: the outbox pending-terminal lister the registry reads (a function seam so the
 *  registry never imports the concrete Outbox, keeping its unit tests light). Returns every run with
 *  an installed, un-retired terminal journal. */
export type PendingTerminalLister = () => PendingTerminal[];

/** PRD #1391 Run B M4: the server-side terminal-pending outbox cap getter — the latest valid
 *  `WORKER_OUTBOX_MAX_PENDING` advertisement from register or an ordinary heartbeat. Undefined when an older api did not return it, in
 *  which case the registry treats the cap as 0 (protect every pending outcome via `pending_overflow`,
 *  the cap-independent floor). */
export type OutboxCapGetter = () => number | undefined;

/** Map a durable terminal-journal phase (a free string, `phase_at_journal`) onto the four wire
 *  snapshot phases. The journal always records `running`, but be total-by-construction: any
 *  unrecognised value degrades to `running`. */
function toSnapshotPhase(phase: string): ActiveSnapshotPhase {
  return phase === "awaiting_approval" || phase === "awaiting_input" || phase === "awaiting_followup"
    ? phase
    : "running";
}

/** Deterministic ordering for the fixed listing slots: blocked journals FIRST (so an owner can
 *  usually reach the ones that need a decision), then oldest-first by `since`, then `run_id` for a
 *  stable tiebreak. No Date.now/Math.random — pure over the inputs. */
function blockedFirstOldest(a: PendingTerminal, b: PendingTerminal): number {
  if (a.blocked !== b.blocked) return a.blocked ? -1 : 1;
  if (a.since !== b.since) return a.since - b.since;
  return a.run_id < b.run_id ? -1 : a.run_id > b.run_id ? 1 : 0;
}

/** Oldest-first ordering for the round-robin over omitted entries (blocked and non-blocked alike),
 *  then `run_id` for a stable tiebreak. */
function oldestFirst(a: PendingTerminal, b: PendingTerminal): number {
  if (a.since !== b.since) return a.since - b.since;
  return a.run_id < b.run_id ? -1 : a.run_id > b.run_id ? 1 : 0;
}

/**
 * PRD #1390 M2a / #1391 Run B M4 — the worker-side registry of the runs this worker is CURRENTLY
 * executing (their phases + claim generations), and the source of every {@link ActiveSnapshot} the
 * worker sends on the heartbeat, the run-lane claim, and (M4) the register request.
 *
 * It is a SEPARATE structure from the claim loop's `Set<Promise<void>>` (which is load-bearing for
 * the capacity semaphore, `Promise.race`, and the shutdown drain — see worker.ts, blocker 7): this
 * maps a live run's id to its current phase + the `claim_generation` it was claimed at, written by
 * the run lane (RunRunner) and the judge/review runners as they start, transition, and finish.
 *
 * PRD #1391 Run B M4 folds in the UNSETTLED-TERMINAL half: a run whose terminal outcome is journaled
 * and awaiting replay is listed `terminal_pending: true` (the api then keeps it unclaimable and its
 * generation unbumped), sourced from the outbox pending-lister threaded in at construction. When the
 * exact journal has an authenticated server-settled observation, it remains custody evidence but
 * does not join this set or its overflow count (ADR-2921). Live execution entries remain independent.
 * pending set exceeds the server cap the snapshot sets `pending_overflow: true` and the listed subset
 * ROTATES deterministically (blocked-first fills `cap - 1` slots, the last slot round-robins over
 * every omitted entry oldest-first) so every pending run is leased within `omitted + 1` snapshots and
 * the `active` array never lists more than `cap` pending entries (else the api rejects it whole).
 *
 * `snapshot_epoch` is a single process-monotonic counter (starts at 1, ++ on every build) shared by
 * ALL builds — the heartbeat loop, the claim loop, and the register snapshot — so the api can order
 * snapshots captured independently. A worker restart is a fresh instance, so the counter restarts at
 * 0 and the first build is epoch 1, under the fresh register nonce the api mints.
 */
export class ActiveRunRegistry {
  /** run_id → {phase, claimGeneration} for every live execution (run-lane + judge/review). */
  private readonly runs = new Map<string, { phase: ActiveSnapshotPhase; claimGeneration: number }>();
  /** The single process-monotonic snapshot epoch, shared by every build (starts at 0;
   *  the first build returns 1). */
  private epoch = 0;

  /** PRD #1391 Run B M4: the PERSISTENT round-robin cursor over omitted pending terminals. Advanced
   *  by one on every overflow build, so `omitted[cursor % omitted.length]` cycles through every
   *  omitted entry deterministically (no Date.now/Math.random). Survives across builds; a worker
   *  restart resets it with the fresh instance. */
  private pendingRotation = 0;

  /** PRD #1390 M4 — e2e-ONLY claim-loop pause latch. Set by the runner's env-gated
   *  drop-execution seam (runner.ts, reachable only when UZI_E2E_DROP_ON_SENTINEL is set),
   *  read by the worker's claim loop so a silently-dropped run can be observed sitting
   *  `queued` before any reclaim. Never touched in production (the seam that sets it is
   *  off by default), so `isClaimPaused()` is a constant `false` there. Cleared only by a
   *  fresh worker process (a new registry instance), which the e2e resume does by
   *  recreating the agent. */
  private claimPaused = false;

  /**
   * PRD #1391 Run B M4: the outbox pending-terminal lister + the server cap getter, threaded in at
   * construction (function seams, so the registry never imports the concrete Outbox/WorkerClient).
   * Both optional and undefined in the #1390 concurrency/semaphore unit tests that never journal a
   * terminal — the registry then behaves exactly as a #1390 worker (no pending entries, no overflow).
   */
  constructor(
    private readonly pendingLister?: PendingTerminalLister,
    private readonly capGetter?: OutboxCapGetter,
    private readonly policyLister?: () => PendingPolicy[],
  ) {}

  /** Register a run as executing at `running`, at the generation it was claimed at.
   *  Idempotent per run id (re-registering, e.g. a promoted re-claim serialised behind an
   *  old park, overwrites the prior entry). */
  add(runId: string, claimGeneration: number): void {
    this.runs.set(runId, { phase: "running", claimGeneration });
  }

  /** Update the current phase of a live run (no-op when the run is not registered, so a
   *  late transition after removal never revives an entry). */
  setPhase(runId: string, phase: ActiveSnapshotPhase): void {
    const entry = this.runs.get(runId);
    if (entry) entry.phase = phase;
  }

  /** Drop a run once its execution settles (terminal, or parked-and-returned for a
   *  requeue — it is no longer executing). */
  remove(runId: string): void {
    this.runs.delete(runId);
  }

  /** Number of live executions currently tracked (test/observability helper). */
  get size(): number {
    return this.runs.size;
  }

  /** PRD #1390 M4 (e2e ONLY): latch the claim-loop pause. Called by the runner's drop
   *  seam, which only fires under UZI_E2E_DROP_ON_SENTINEL, so this is unreachable in
   *  production. */
  pauseClaimForE2E(): void {
    this.claimPaused = true;
  }

  /** PRD #1390 M4 (e2e ONLY): whether the claim loop is paused by the drop seam. A
   *  constant `false` in production (nothing ever calls {@link pauseClaimForE2E}). */
  isClaimPausedForE2E(): boolean {
    return this.claimPaused;
  }

  /** Whether this worker is CURRENTLY executing `runId` (PRD #1390 M3, blocker 7). Read
   *  by the claim loop's belt-and-braces duplicate-claim assertion: a returned claim whose
   *  id is already live is refused, never double-executed. The server-side pre-claim dedupe
   *  is the real guard; this is the loud last line of defence. */
  has(runId: string): boolean {
    return this.runs.has(runId);
  }

  /**
   * PRD #1391 Run B M4 — the PRODUCTION claim-loop gate (D7): whether the pending terminal set
   * exceeds what the api will lease (`pending_overflow`). While true, #1390's claim exclusion closes
   * every run this worker owns to every claimant AND the worker's own claim loop stays closed, so no
   * unleased pending outcome ever coexists with a new claim. Distinct from {@link isClaimPausedForE2E}
   * (the e2e drop seam): this reads the REAL pending-vs-cap state. Never advances the epoch or the
   * rotation cursor — a gate read is not a snapshot build. Matches {@link build}'s overflow exactly:
   * `pendingCount > effectiveCap`, where an undefined/0 cap is the cap-independent floor (overflow
   * whenever anything is pending).
   */
  claimsPausedByPendingOverflow(): boolean {
    const count = this.pendingOutcomes(false).length;
    if (count === 0) return false;
    return count > this.effectiveCap();
  }

  /** Issue #2950: the pending terminals a snapshot can usefully lease. A `run_not_owned` journal is
   *  excluded: the api cannot lease a run it no longer attributes to this worker
   *  (UpsertFrozenWorkerActiveRun drops it and logs per snapshot), and its `pending_overflow`
   *  closure only covers runs it attributes to this worker, so listing or counting the entry
   *  protects nothing; counting it would turn an endless resend into an endless claim pause at
   *  cap 0. Physical protection and custody still keep it via the outbox's listPendingTerminals /
   *  hasPendingTerminal, which are unchanged. */
  private leasable(pending: PendingTerminal[]): PendingTerminal[] {
    return pending.filter((p) => p.blocked_reason !== "run_not_owned");
  }

  /** The server cap clamped to a non-negative integer, with undefined/negative/0 collapsing to 0 —
   *  the cap-independent floor where any pending outcome overflows. */
  private effectiveCap(): number {
    const cap = this.capGetter ? this.capGetter() : undefined;
    return cap !== undefined && Number.isFinite(cap) && cap > 0 ? Math.floor(cap) : 0;
  }

  private pendingOutcomes(includeNotOwned = true): PendingTerminal[] {
    const entries: PendingTerminal[] = this.pendingLister?.().filter(entry => entry.server_settled !== true) ?? [];
    for (const p of this.policyLister?.() ?? []) {
      if (p.server_settled) continue;
      entries.push({ run_id: p.run_id, claim_generation: p.delivery.inventory_generation,
        phase: "running", blocked: false, since: p.since });
    }
    // Delivery authority, never original source generation, determines the single wire slot.
    const byRun = new Map<string, PendingTerminal>();
    for (const p of includeNotOwned ? entries : this.leasable(entries)) {
      const previous = byRun.get(p.run_id);
      if (!previous || p.claim_generation > previous.claim_generation) byRun.set(p.run_id, p);
    }
    return [...byRun.values()];
  }

  /**
   * PRD #1391 Run B M4 — the BOOT register snapshot (nonce-exempt): an EMPTY pending subset with
   * `pending_overflow` set whenever ANY terminal outcome is pending, so the api leases (and refuses
   * to re-claim) every pending run this worker owns BEFORE its register-time orphan pass — regardless
   * of the cap, even cap 0, where a non-empty subset would be rejected whole when cap < count. Draws
   * the shared epoch so ordering stays monotonic with the post-register heartbeat/claim snapshots.
   * Deliberately NOT filtered through {@link leasable} (issue #2950): the boot lease stays
   * conservative and sets `pending_overflow` whenever ANY journal is pending, `run_not_owned` included.
   * Issue #1742: `finalizeResume` (the caller's once-captured offered set) rides it as
   * `finalize_resume`, on this register snapshot only.
   */
  buildRegisterSnapshot(finalizeResume: readonly FinalizeResumeEntry[] = []): ActiveSnapshot {
    this.epoch += 1;
    const pending = this.pendingOutcomes();
    return {
      snapshot_epoch: this.epoch,
      active: [],
      pending_overflow: pending.length > 0,
      pending_all: pending.map((p) => ({ run_id: p.run_id, claim_generation: p.claim_generation })),
      // Issue #1742: the finalize-pending set the caller captured for this register. The key is
      // omitted when empty, and build() (heartbeat/claim) never carries it.
      ...(finalizeResume.length > 0
        ? { finalize_resume: finalizeResume.map((e) => ({ run_id: e.run_id, claim_generation: e.claim_generation })) }
        : {}),
    };
  }

  /**
   * Build the next {@link ActiveSnapshot}: increment the shared epoch, project the live registry to
   * wire entries, and fold in the pending-terminal entries (rotated when the set overflows the cap).
   * The `register_nonce` is NOT stamped here — the client stamps it on send from the nonce it
   * captured at register (worker.ts/client.ts).
   */
  build(): ActiveSnapshot {
    this.epoch += 1;
    const pending = this.pendingOutcomes(false);
    const pendingByRun = new Map(pending.map((p) => [p.run_id, p]));

    const active: ActiveSnapshotEntry[] = [];
    // Live executions this worker is running, EXCEPT any that also carry a LEASABLE pending terminal
    // (those are listed below as terminal_pending, and the cap governs them). In practice a run leaves
    // the live registry as it journals its terminal, so an overlap is a brief corner case. A
    // run_not_owned journal is not leasable (filtered above), so a live run sharing its id is listed
    // live at its own generation: the api drops it unless it attributes the run to this worker.
    for (const [runId, entry] of this.runs) {
      if (pendingByRun.has(runId)) continue;
      active.push({
        run_id: runId,
        claim_generation: entry.claimGeneration,
        phase: entry.phase,
        terminal_pending: false,
      });
    }

    // Pending-terminal entries, deterministically selected so the `active` array never lists more
    // than `cap` of them (the api rejects an over-cap snapshot whole).
    for (const p of this.selectPendingForBuild(pending)) {
      active.push({
        run_id: p.run_id,
        claim_generation: p.claim_generation,
        phase: toSnapshotPhase(p.phase),
        terminal_pending: true,
      });
    }

    // pendingOutcomes deduplicates before cap selection and overflow accounting.
    return {
      snapshot_epoch: this.epoch,
      active,
      pending_overflow: pending.length > this.effectiveCap(),
      pending_all: pending.map((p) => ({ run_id: p.run_id, claim_generation: p.claim_generation })),
    };
  }

  /**
   * Select which pending terminals a heartbeat/claim snapshot lists. When they all fit the cap, list
   * every one (blocked-first, oldest-first for a stable order). When they overflow, list `cap - 1`
   * fixed slots blocked-first + one rotating slot that round-robins over every omitted entry
   * oldest-first, advancing the persistent cursor — so every pending run is leased within
   * `omitted + 1` snapshots and no more than `cap` are ever listed. A 0/undefined cap lists NONE (the
   * cap-independent floor: the empty subset + overflow protects them all via #1390's exclusion).
   */
  private selectPendingForBuild(pending: PendingTerminal[]): PendingTerminal[] {
    const count = pending.length;
    if (count === 0) return [];
    const cap = this.effectiveCap();
    if (cap === 0) return []; // overflow-only protection; nothing may be listed
    if (count <= cap) return [...pending].sort(blockedFirstOldest);

    // Overflow: fixed slots (blocked-first) + one rotating slot over the omitted (oldest-first).
    const sorted = [...pending].sort(blockedFirstOldest);
    const fixed = sorted.slice(0, cap - 1);
    const fixedIds = new Set(fixed.map((p) => p.run_id));
    const omitted = pending.filter((p) => !fixedIds.has(p.run_id)).sort(oldestFirst);
    const listed = [...fixed];
    if (omitted.length > 0) {
      const pick = omitted[this.pendingRotation % omitted.length]!;
      this.pendingRotation += 1;
      listed.push(pick);
    }
    return listed;
  }
}
