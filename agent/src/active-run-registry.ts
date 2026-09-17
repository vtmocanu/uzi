import type { ActiveSnapshot, ActiveSnapshotEntry, ActiveSnapshotPhase } from "./protocol.js";

/**
 * PRD #1390 M2a — the worker-side registry of the runs this worker is CURRENTLY
 * executing and their phases, and the source of every {@link ActiveSnapshot} the worker
 * sends on the heartbeat and the run-lane claim.
 *
 * It is a SEPARATE structure from the claim loop's `Set<Promise<void>>` (which is
 * load-bearing for the capacity semaphore, `Promise.race`, and the shutdown drain — see
 * worker.ts, blocker 7): this maps a live run's id to its current phase + the
 * `claim_generation` it was claimed at, written by the run lane (RunRunner) and the
 * judge/review runners as they start, transition, and finish, and read by the worker to
 * build a snapshot.
 *
 * `snapshot_epoch` is a single process-monotonic counter (starts at 1, ++ on every
 * build) shared by ALL builds, so the heartbeat and claim loops — which build
 * independently — draw ordered epochs from one source. A worker restart is a fresh
 * instance, so the counter restarts at 0 and the first build is epoch 1, under the fresh
 * register nonce the api mints (the api resets the stored epoch to 0 under that nonce).
 *
 * For a #1390 worker EVERY entry has `terminal_pending: false` and every snapshot has
 * `pending_overflow: false` — terminal-outcome journaling is #1391's.
 */
export class ActiveRunRegistry {
  /** run_id → {phase, claimGeneration} for every live execution (run-lane + judge/review). */
  private readonly runs = new Map<string, { phase: ActiveSnapshotPhase; claimGeneration: number }>();
  /** The single process-monotonic snapshot epoch, shared by every build (starts at 0;
   *  the first build returns 1). */
  private epoch = 0;

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

  /** Whether this worker is CURRENTLY executing `runId` (PRD #1390 M3, blocker 7). Read
   *  by the claim loop's belt-and-braces duplicate-claim assertion: a returned claim whose
   *  id is already live is refused, never double-executed. The server-side pre-claim dedupe
   *  is the real guard; this is the loud last line of defence. */
  has(runId: string): boolean {
    return this.runs.has(runId);
  }

  /**
   * Build the next {@link ActiveSnapshot}: increment the shared epoch and project the
   * live registry to wire entries. The `register_nonce` is NOT stamped here — the client
   * stamps it on send from the nonce it captured at register (worker.ts/client.ts).
   */
  build(): ActiveSnapshot {
    this.epoch += 1;
    const active: ActiveSnapshotEntry[] = [];
    for (const [runId, entry] of this.runs) {
      active.push({
        run_id: runId,
        claim_generation: entry.claimGeneration,
        phase: entry.phase,
        terminal_pending: false,
      });
    }
    return { snapshot_epoch: this.epoch, active, pending_overflow: false };
  }
}
