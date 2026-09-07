// PRD #1171 (M3, milestone 1, first unit): the `CodexExecutionSafety` facade and
// the trusted boundary-action lane.
//
// `withBoundary` is the single gate every durability/publication sink (checkpoint,
// park, shutdown recovery, terminal/finalize, PAT-bearing git) passes through on a
// Codex-selected run. It (1) serializes overlapping boundary acquisitions, (2)
// quiesces child admission, (3) reaps every registered root, and only then (4)
// mints an UNFORGEABLE matching-epoch `BoundaryPermit` and (5) runs the sink's
// action while holding that permit through the FULL async action and all of its
// children. Any failure before the action leaves the sink UNCALLED and poisons the
// epoch; any incomplete settlement during the action keeps the epoch closed and
// poisoned rather than reporting "clean".
//
// It performs NO real `child_process` work: the actual boundary-action spawn is the
// injected {@link SpawnRootSeam} (the real one lands in m4 behind the M3a
// supervisor); everything here is unit-testable with fakes.

import type {
  BoundaryPermit,
  BoundaryRequest,
  ChildQuiescence,
  CodexExecutionSafety,
  HarnessError,
  ProcessReap,
  ToolDisposal,
} from "../harness.js";
import type { ExecutionRegistry, RegisteredRoot } from "./registry.js";

/** Which OS identity a boundary action runs as. `worker_pat` is a PAT-bearing
 *  (credentialed) action (e.g. `git push`); `command` is the credential-free
 *  command identity. The distinction drives the [R3-2] ordering guard below. */
export type BoundaryActionIdentity = "command" | "worker_pat";

/** The low-level "actually start the boundary-action process" seam. Production
 *  (m4) wires the real M3a supervisor here; tests inject a fake that can simulate a
 *  late/backgrounded child by delaying the returned root's `reap()`. */
export type SpawnRootSeam = (
  argv: readonly string[],
  identity: BoundaryActionIdentity,
  deadlineMs: number,
) => Promise<RegisteredRoot>;

/** The registry-bound seams `withBoundary` drives. Kept injectable so the facade is
 *  testable without a real registry or real processes; {@link createCodexExecutionSafety}
 *  wires them to a live {@link ExecutionRegistry}. */
export interface BoundarySeams {
  quiesce(request: BoundaryRequest): Promise<ChildQuiescence>;
  reap(request: BoundaryRequest, closedEpoch: number): Promise<ProcessReap>;
  dispose(request: BoundaryRequest): Promise<ToolDisposal>;
  spawnRoot: SpawnRootSeam;
}

export type BoundaryActionOutcome =
  | { kind: "settled" }
  | {
      kind: "refused";
      reason: "stale_permit" | "admission_not_closed" | "command_roots_live";
    }
  | { kind: "poisoned"; error: HarnessError };

/** Thrown by `withBoundary` when the boundary cannot be established (quiesce/reap
 *  failed) or the action's own children left the epoch poisoned. The sink was
 *  never run, or ran but its cleanup was incomplete; either way the caller must
 *  NOT treat the boundary as clean. */
export class CodexBoundaryError extends Error {
  /** The action body's OWN thrown error, preserved when the action threw AND the
   *  epoch was also left poisoned. Primary-failure evidence must not be discarded
   *  behind the cleanup/poison evidence in {@link errors} (harness-contract.md:632
   *  "preserve primary failure and cleanup evidence separately"). It is also set as
   *  the standard `cause`. Undefined when the action itself did not throw. */
  readonly actionError?: unknown;
  constructor(
    readonly stage: "quiesce" | "reap" | "action",
    readonly errors: readonly HarnessError[],
    actionError?: unknown,
  ) {
    super(
      `codex boundary failed at ${stage}`,
      actionError !== undefined ? { cause: actionError } : undefined,
    );
    this.name = "CodexBoundaryError";
    this.actionError = actionError;
  }
}

// The permit is minted ONLY here, via a cast. Its brand (`boundaryPermitBrand`, a
// module-private `unique symbol` in harness.ts) is COMPILE-TIME discipline only: it
// stops honest code from shaping a permit-typed literal, but it enforces nothing at
// runtime and is defeatable by `as unknown as BoundaryPermit`. The RUNTIME authority
// is the safety owner's HELD state: `spawnBoundaryAction` admits an action only while
// `heldEpoch` is set AND `permit.epoch === heldEpoch` (see runBoundaryAction's
// stale_permit guard), so a forged or stale permit cannot reach a sink out of band.
// This matches harness-contract.md:637 ("the epoch number is descriptive; the
// module-private brand and safety owner's held state enforce authority"). This
// trusted minter is the sole legitimate construction point.
function mintPermit(epoch: number, boundary: BoundaryRequest["boundary"]): BoundaryPermit {
  return { epoch, boundary } as unknown as BoundaryPermit;
}

export class CodexExecutionSafetyImpl implements CodexExecutionSafety {
  readonly kind = "codex" as const;

  private readonly registry: ExecutionRegistry;
  private readonly seams: BoundarySeams;

  // Serialization tail: each boundary awaits the previous one's release.
  private queueTail: Promise<void> = Promise.resolve();

  // Live only while an action is running under a held permit.
  private heldEpoch: number | undefined;
  private currentDeadlineMs = 0;
  private pendingActions: Promise<unknown>[] = [];

  constructor(registry: ExecutionRegistry, seams: BoundarySeams) {
    this.registry = registry;
    this.seams = seams;
  }

  async withBoundary<T>(
    request: BoundaryRequest,
    action: (permit: BoundaryPermit) => Promise<T>,
  ): Promise<T> {
    // (1) Serialize overlapping acquisitions. Each call parks on the prior tail and
    // installs a fresh release the next caller will await.
    const prior = this.queueTail;
    let release!: () => void;
    this.queueTail = new Promise<void>((res) => {
      release = res;
    });
    try {
      await prior;
      return await this.runBoundary(request, action);
    } finally {
      release();
    }
  }

  private async runBoundary<T>(
    request: BoundaryRequest,
    action: (permit: BoundaryPermit) => Promise<T>,
  ): Promise<T> {
    // (2) Quiesce child admission. Must be fully quiescent or the sink is uncalled.
    const q = await this.seams.quiesce(request);
    if (q.kind !== "quiescent") {
      const errors: readonly HarnessError[] =
        q.kind === "incomplete"
          ? q.errors
          : [{ category: "protocol", message: `withBoundary: quiesce returned non-quiescent "${q.kind}"` }];
      this.registry.poison(errors);
      throw new CodexBoundaryError("quiesce", errors);
    }
    const epoch = q.epoch;

    // (3) Reap every registered root. Must be observed-empty AND carry the SAME
    // epoch we just quiesced at; a mismatch means a stale/foreign permit domain.
    const r = await this.seams.reap(request, epoch);
    if (r.kind !== "observed_empty" || r.epoch !== epoch) {
      const errors: readonly HarnessError[] =
        r.kind === "incomplete"
          ? r.errors
          : [
              {
                category: "protocol",
                message: `withBoundary: reap not observed-empty for epoch ${epoch} (kind="${r.kind}")`,
              },
            ];
      this.registry.poison(errors);
      throw new CodexBoundaryError("reap", errors);
    }

    // (4) Mint the unforgeable matching-epoch permit and (5) run the action while
    // holding it. The permit/closed epoch is held through the full async action and
    // every boundary-action child; release only after actual settlement.
    const permit = mintPermit(epoch, request.boundary);
    this.heldEpoch = epoch;
    this.currentDeadlineMs = request.deadlineMs;
    this.pendingActions = [];

    let outcome: { ok: true; value: T } | { ok: false; error: unknown };
    try {
      outcome = { ok: true, value: await action(permit) };
    } catch (e) {
      outcome = { ok: false, error: e };
    }
    // Wait for every boundary-action child spawned during the action, even one the
    // action body did not itself await. A timed-out/late child poisons here.
    await Promise.allSettled(this.pendingActions);

    this.heldEpoch = undefined;
    this.pendingActions = [];

    if (this.registry.state() === "poisoned") {
      // Preserve BOTH the cleanup/poison evidence AND the action's own thrown error
      // (when it threw). The poison evidence explains why the boundary is not clean;
      // the action error is the primary failure and must not be swallowed
      // (harness-contract.md:632 "preserve primary failure and cleanup evidence
      // separately").
      throw new CodexBoundaryError(
        "action",
        this.registry.poisonErrors(),
        outcome.ok ? undefined : outcome.error,
      );
    }
    if (!outcome.ok) throw outcome.error;
    return outcome.value;
  }

  /**
   * Start a trusted boundary-action subprocess under a held permit. Reserves a
   * `boundary_action` root in the registry BEFORE the injected spawn, then holds the
   * permit until that root reaps `ECHILD`(+`__WALL`); a late child that outlives the
   * action body therefore keeps the permit held until it settles, and a timed-out
   * child poisons the epoch.
   *
   * [R3-2] PAT-bearing ordering: a `worker_pat` action refuses to spawn unless
   * admission is closed AND every command-kind root has already reaped, so a
   * model-authorized command process can never be alive to read the PAT out of the
   * git child's environment. The guard consults the registry directly, independent
   * of whatever the reap seam reported.
   *
   * Returns synchronously-tracked so `withBoundary` holds the permit across it even
   * if the action body forgot to await.
   */
  spawnBoundaryAction(
    permit: BoundaryPermit,
    argv: readonly string[],
    identity: BoundaryActionIdentity,
  ): Promise<BoundaryActionOutcome> {
    const p = this.runBoundaryAction(permit, argv, identity);
    this.pendingActions.push(p);
    return p;
  }

  private async runBoundaryAction(
    permit: BoundaryPermit,
    argv: readonly string[],
    identity: BoundaryActionIdentity,
  ): Promise<BoundaryActionOutcome> {
    // Permit must match the currently held boundary epoch.
    if (this.heldEpoch === undefined || permit.epoch !== this.heldEpoch) {
      return { kind: "refused", reason: "stale_permit" };
    }
    // [R3-2] guard for PAT-bearing actions.
    if (identity === "worker_pat") {
      if (this.registry.state() !== "closed") {
        return { kind: "refused", reason: "admission_not_closed" };
      }
      if (this.registry.hasLiveCommandRoot()) {
        return { kind: "refused", reason: "command_roots_live" };
      }
    }
    // Reserve the boundary_action root BEFORE spawning anything.
    const reserved = this.registry.reserveLaunch("boundary_action");
    if (reserved.kind !== "reserved") {
      const error: HarnessError = {
        category: "protocol",
        message: "spawnBoundaryAction: boundary-action launch reservation denied",
      };
      this.registry.poison(error);
      return { kind: "poisoned", error };
    }
    let root: RegisteredRoot;
    try {
      root = await this.seams.spawnRoot(argv, identity, this.currentDeadlineMs);
    } catch (e) {
      this.registry.cancelReservation(reserved.reservation);
      const error: HarnessError = {
        category: "tool",
        message: `spawnBoundaryAction: spawn failed (${e instanceof Error ? e.name : "unknown"})`,
      };
      this.registry.poison(error);
      return { kind: "poisoned", error };
    }
    this.registry.registerRoot(reserved.reservation, root);
    // Hold the permit until this root reaps its whole descendant set.
    const reap = await root.reap(this.currentDeadlineMs);
    if (!reap.ok) {
      this.registry.poison(reap.error);
      return { kind: "poisoned", error: reap.error };
    }
    return { kind: "settled" };
  }

  /** Terminal tool disposal. Delegated to the registry-bound dispose seam; not part
   *  of the {@link CodexExecutionSafety} interface, but the terminal boundary owner
   *  calls it after the final action settles. */
  dispose(request: BoundaryRequest): Promise<ToolDisposal> {
    return this.seams.dispose(request);
  }
}

/**
 * Wire a {@link CodexExecutionSafetyImpl} to a live {@link ExecutionRegistry}: the
 * quiesce/reap/dispose seams delegate to the registry's boundary transitions, and
 * the caller supplies only the low-level process-spawn seam (fake in tests, the
 * real M3a supervisor in m4). This is the sole production construction point.
 */
export function createCodexExecutionSafety(
  registry: ExecutionRegistry,
  spawnRoot: SpawnRootSeam,
): CodexExecutionSafetyImpl {
  return new CodexExecutionSafetyImpl(registry, {
    quiesce: (request) => registry.quiesceChildren(request.deadlineMs),
    reap: (request, closedEpoch) => registry.reapProcesses(request.deadlineMs, closedEpoch),
    dispose: (request) => registry.disposeTools(request.deadlineMs),
    spawnRoot,
  });
}
