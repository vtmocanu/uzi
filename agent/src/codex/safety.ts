import { CaptureOperationRevokedError, GitBoundaryAbortError, checkCheckpointStop, CheckpointSoftDeadlineError, CheckpointUnsafeCleanupError, waitCheckpointAdmission } from "../checkpoint-stop.js";
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

import { reaperCaptureScope, isClosedReaperRequest, assertClosedReaperRequest, closedReaperDeadline } from "../closed-reaper.js";
import type { SdkAttemptEnv } from "../sdk-env.js";
import { MaintenanceBudget } from "../maintenance-budget.js";
import { assertCaptureRequest, createCaptureContext } from "../git.js";
import { TrustedExecutionRefusal } from "../trusted-execution-refusal.js";
import { CheckpointChildStartupTimeoutError, recordCaptureFailure } from "../harness.js";
import type {
  BoundaryPermit,
  CredentialFreeCaptureContext,
  CompletedCredentialFreeCapture,
  BoundaryProcessHandle,
  BoundaryProcessRequest,
  BoundaryRequest,
  BoundarySink,
  BoundaryStep,
  ChildQuiescence,
  CodexExecutionSafety,
  HarnessError,
  ProcessReap,
  SafeBoundary,
  ToolDisposal,
} from "../harness.js";
import type { CaptureSettlement, ExecutionRegistry, ReapOutcome, RegisteredRoot } from "./registry.js";
import { safeErrorName } from "./registry.js";
import { CodexLaunchError, SupervisedChildExitTimeoutError, classifyCodexLaunchFailure, watchStartupCleanup, type StartupCleanupAuthorization } from "./launcher.js";

/** OS identity of a boundary child. `worker_pat` is the trusted worker identity,
 * also used by credential-free bare Git; `command` is the isolated clone identity.
 * Credential presence is validated separately from identity. */
export type BoundaryActionIdentity = "command" | "worker_pat";

/** The low-level "actually start the boundary-action process" seam. Production
 *  (m4) wires the real M3a supervisor here; tests inject a fake that can simulate a
 *  late/backgrounded child by delaying the returned root's `reap()`. */
export type SpawnRootSeam = (
  argv: readonly string[],
  identity: BoundaryActionIdentity,
  deadlineMs: number,
) => Promise<RegisteredRoot>;

export interface SpawnedBoundaryProcess extends Omit<BoundaryProcessHandle, "completed" | "cancel"> {
  readonly root: RegisteredRoot;
  /** Wait only for the supervised primary child. The safety owner follows it
   * with the registry-owned whole-root reap before exposing completion. */
  waitChild(deadlineMs: number): Promise<{ readonly code: number }>;
}

export type SpawnBoundaryProcessSeam = (
  request: BoundaryProcessRequest,
  deadlineMs: number,
  startupCleanup?: StartupCleanupAuthorization,
) => Promise<SpawnedBoundaryProcess>;

/** The registry-bound seams `withBoundary` drives. Kept injectable so the facade is
 *  testable without a real registry or real processes; {@link createCodexExecutionSafety}
 *  wires them to a live {@link ExecutionRegistry}. */
export interface BoundarySeams {
  quiesce(request: BoundaryRequest): Promise<ChildQuiescence>;
  reap(request: BoundaryRequest, closedEpoch: number): Promise<ProcessReap>;
  dispose(request: BoundaryRequest): Promise<ToolDisposal>;
  spawnRoot: SpawnRootSeam;
  spawnProcess?: SpawnBoundaryProcessSeam;
  /** Arms the boundary deadline: call `fire` once `ms` has elapsed, and return a cancel
   *  the boundary calls at teardown. Absent ⇒ {@link defaultArmDeadline} (a wall-clock
   *  unref'd timer). Tests inject an event-gated trigger so a deadline fires at a chosen
   *  point instead of racing process load. */
  armDeadline?: ArmBoundaryDeadline;
  /** Test absolute clock; paired with armDeadline. */
  now?: () => number;
}

export type ArmBoundaryDeadline = (request: BoundaryRequest, ms: number, fire: () => void) => () => void;

// The production deadline: a wall-clock timer that never holds the process open.
const defaultArmDeadline: ArmBoundaryDeadline = (_request, ms, fire) => {
  const timer = setTimeout(fire, ms);
  timer.unref?.();
  return () => clearTimeout(timer);
};

export type BoundaryActionOutcome =
  | { kind: "settled" }
  | {
      kind: "refused";
      reason: "stale_permit" | "admission_not_closed" | "command_roots_live";
    }
  | { kind: "poisoned"; error: HarnessError };

/** PRD #1171 m4: the NEUTRAL result of the per-sink auth-mode reconciliation. It is the
 *  ONLY thing that crosses back from the executor-owned reconcile closure into the generic
 *  safety code — the runner and the generic `withBoundary` NEVER see `authMode`, a token, an
 *  operation id or a generation. `ready` lets the boundary proceed to quiesce/reap/mint;
 *  `blocked` poisons the epoch and throws `CodexBoundaryError("reconcile")` WITHOUT
 *  quiescing/reaping/minting, so the sink counter stays zero and later publication is
 *  blocked. */
export type ReconcileOutcome =
  | { kind: "ready" }
  | {
      kind: "blocked";
      errors: readonly HarnessError[];
      /** A confirmed vault lock, account unavailability or unresolved refresh outcome defers the
       *  reconcile. This field never grants authority: the boundary still poisons and throws,
       *  and the runner must prove credential-free capture before parking for recovery. */
      deferral?: "vault_locked" | "refresh_unknown" | "account_unavailable";
    };

/** Issue #1766: the result of {@link CodexExecutionSafetyImpl.settleForCredentialFreeCapture}.
 *  `observed_empty` means the poisoned registry had no pending launch, no in-flight callback
 *  and every registered root cleanly reaped at the final check; anything else is
 *  `incomplete` with secret-free errors. */
export type CredentialFreeCaptureSettlement = CaptureSettlement;
export type CredentialFreeCaptureResult<T> =
  | { kind: "observed_empty"; value: T; proof: CompletedCredentialFreeCapture }
  | { kind: "incomplete"; errors: readonly HarnessError[] };

/** PRD #1171 m4: the executor-owned auth-mode reconcile step run BEFORE every Codex
 *  boundary. A subscription run refreshes + durably advances its generation here; an
 *  api_key run performs zero refresh and only re-authorizes. Absent ⇒ no reconcile step
 *  (Claude/tests that do not pass it are unaffected). It NEVER returns a credential — only a
 *  neutral {@link ReconcileOutcome}. The signal is the same one held by the
 *  eventual permit and aborts at the single boundary deadline. */
export type ReconcileBeforeBoundary = (request: BoundaryRequest, signal: AbortSignal) => Promise<ReconcileOutcome>;

/** Issue #1864: caps for {@link CodexBoundaryError.diagnostic}. */
const DIAGNOSTIC_MAX_ERRORS = 3;
const DIAGNOSTIC_ERROR_MAX_CHARS = 160;
/** The whole diagnostic never exceeds this; the runner refuses a longer one. */
const CODEX_BOUNDARY_DIAGNOSTIC_MAX_CHARS = 500;
const SINK_LABELS: Record<BoundarySink, string> = {
  milestone_checkpoint: "milestone checkpoint",
  done_checkpoint: "done checkpoint",
};
/** Issues #1900 and #1914: fixed, secret-free deadline step labels. */
const STEP_LABELS: Record<BoundaryStep, string> = {
  run_quiescence: "run-quiescence",
  fetch_back: "fetch-back",
  default_fetch: "default-fetch",
  secret_scan: "secret-scan",
  base_align: "base-align",
  push: "push",
  completion_permit: "completion-permit",
  pr_description_prepare: "pr-description-prepare",
  mr_create: "mr-create",
  post_mr: "post-mr",
  checkpoint_lock_wait: "checkpoint-lock-wait",
  checkpoint_overlay: "checkpoint-overlay",
  scratch_preflight: "scratch-preflight",
  checkpoint_pack: "checkpoint-pack",
  checkpoint_upload: "checkpoint-upload",
  checkpoint_report: "checkpoint-report",
  checkpoint_sink_wait: "checkpoint-sink-wait",
  checkpoint_import_context: "checkpoint-import-context",
  checkpoint_import_lock_wait: "checkpoint-import-lock-wait",
  checkpoint_import_transport: "checkpoint-import-transport",
  checkpoint_import_poststage: "checkpoint-import-poststage",
  checkpoint_import_premutation: "checkpoint-import-premutation",
  checkpoint_import_mutation: "checkpoint-import-mutation",
  checkpoint_import_readback: "checkpoint-import-readback",
  checkpoint_code_head: "checkpoint-code-head",
  checkpoint_bridge: "checkpoint-bridge",
  checkpoint_post_import: "checkpoint-post-import",
  checkpoint_steer: "checkpoint-steer",
  checkpoint_post_bridge: "checkpoint-post-bridge",
};

function isBoundaryStep(value: unknown): value is BoundaryStep {
  return typeof value === "string" && Object.hasOwn(STEP_LABELS, value);
}

/** Issue #1900: read the caller's active-step probe. A missing probe, a probe that throws or
 *  one that returns anything but a known step yields undefined, so the diagnostic stays as it
 *  was without a step. */
function readStep(request: BoundaryRequest): BoundaryStep | undefined {
  let value: unknown;
  try {
    value = request.activeStep?.();
  } catch {
    return undefined;
  }
  return isBoundaryStep(value) ? value : undefined;
}

function capChars(text: string, max: number): string {
  return text.length <= max ? text : `${text.slice(0, max - 1)}\u2026`;
}

/** One code point in Unicode general category Cc (C0, DEL, C1), Cf (every format character:
 *  zero-width, bidi, soft hyphen, word joiners, U+061C, U+180E, U+FEFF, tag characters) or
 *  Zl/Zp (U+2028, U+2029): the characters that can break, hide or reorder log text. Keep in
 *  step with `CODEX_BOUNDARY_DIAGNOSTIC_CHAR` in agent/src/runner.ts, which rejects the same set. */
const DIAGNOSTIC_CONTROL = /^[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]$/u;

function diagnosticText(raw: unknown): string {
  const text = typeof raw === "string" ? raw : "";
  // Each control-character run becomes one space, so "a\nb" stays two readable words.
  let out = "";
  let inControl = false;
  // Iterate by code point so an astral format character (a tag character) is tested whole.
  for (const ch of text) {
    if (DIAGNOSTIC_CONTROL.test(ch)) {
      if (!inControl) out += " ";
      inControl = true;
    } else {
      out += ch;
      inControl = false;
    }
  }
  return capChars(out.trim(), DIAGNOSTIC_ERROR_MAX_CHARS);
}

/** Issue #1864: the one-line, secret-free boundary diagnostic. It names the stage, the sink
 *  (or boundary) and up to {@link DIAGNOSTIC_MAX_ERRORS} harness error messages, which are
 *  the harness's own secret-free evidence. The action error contributes only its class name:
 *  its message and object can carry arbitrary sink output and never appear. */
function boundaryDiagnostic(
  stage: string,
  errors: readonly HarnessError[],
  actionError: unknown,
  boundary: SafeBoundary | undefined,
  sink: BoundarySink | undefined,
  step: BoundaryStep | undefined,
): string {
  const label =
    sink !== undefined && Object.hasOwn(SINK_LABELS, sink)
      ? SINK_LABELS[sink]
      : typeof boundary === "string" && /^[a-z_]{1,32}$/.test(boundary)
        ? boundary
        : undefined;
  const parts = errors.slice(0, DIAGNOSTIC_MAX_ERRORS).map((e) => diagnosticText(e?.message));
  if (errors.length > DIAGNOSTIC_MAX_ERRORS) parts.push(`+${errors.length - DIAGNOSTIC_MAX_ERRORS} more`);
  if (actionError !== undefined) {
    const name = safeErrorName(actionError);
    parts.push(`action error: ${name === "unknown" ? "Error" : name}`);
  }
  const details = parts.length > 0 ? parts.join("; ") : "no detail";
  // Issue #1900: the step goes in the head, so the error cap below can never hide it.
  const stepLabel = isBoundaryStep(step) ? STEP_LABELS[step] : undefined;
  const head =
    `codex boundary failed at ${stage}${label !== undefined ? ` (${label})` : ""}` +
    `${stepLabel !== undefined ? ` during ${stepLabel}` : ""}`;
  return capChars(`${head}: ${details}`, CODEX_BOUNDARY_DIAGNOSTIC_MAX_CHARS);
}

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
  /** Set when a reconcile block carries a credential deferral.
   *  The registry is still poisoned; the runner may preserve it for later recovery. */
  readonly deferral?: "vault_locked" | "refresh_unknown" | "account_unavailable";
  /** Issue #1864: the boundary and the checkpoint sink that requested it, when known. */
  readonly boundary?: SafeBoundary;
  readonly sink?: BoundarySink;
  /** Issue #1900: the finalize step that was active when the boundary deadline fired, when
   *  the caller reported one. Set only on an `action`-stage deadline failure. */
  readonly step?: BoundaryStep;
  /** Issue #1864: a one-line, secret-free summary of this failure, at most
   *  {@link CODEX_BOUNDARY_DIAGNOSTIC_MAX_CHARS} UTF-16 code units, containing no code point in
   *  Unicode general category Cc, Cf, Zl or Zp (see `DIAGNOSTIC_CONTROL`). `message` stays the bare
   *  `codex boundary failed at <stage>`. */
  readonly diagnostic: string;
  constructor(
    readonly stage: "reconcile" | "quiesce" | "reap" | "action",
    readonly errors: readonly HarnessError[],
    actionError?: unknown,
    deferral?: "vault_locked" | "refresh_unknown" | "account_unavailable",
    context?: { boundary?: SafeBoundary; sink?: BoundarySink; step?: BoundaryStep },
  ) {
    super(
      `codex boundary failed at ${stage}`,
      actionError !== undefined ? { cause: actionError } : undefined,
    );
    this.name = "CodexBoundaryError";
    this.actionError = actionError;
    if (deferral !== undefined) this.deferral = deferral;
    if (context?.boundary !== undefined) this.boundary = context.boundary;
    if (context?.sink !== undefined) this.sink = context.sink;
    if (context?.step !== undefined) this.step = context.step;
    this.diagnostic = boundaryDiagnostic(stage, errors, actionError, context?.boundary, context?.sink, context?.step);
  }
}

// The permit is minted ONLY here, via a cast. Its brand (`boundaryPermitBrand`, a
// module-private `unique symbol` in harness.ts) is COMPILE-TIME discipline only: it
// stops honest code from shaping a permit-typed literal, but it enforces nothing at
// runtime and is defeatable by `as unknown as BoundaryPermit`. The RUNTIME authority
// is the safety owner's HELD state: `spawnBoundaryAction` admits an action only while
// the exact permit object minted for the active boundary is held (see
// runBoundaryAction's stale_permit guard), so a forged permit or a permit retained from
// an earlier boundary cannot reach a sink out of band even though the registry epoch is
// intentionally stable for the whole run.
// This matches harness-contract.md:637 ("the epoch number is descriptive; the
// module-private brand and safety owner's held state enforce authority"). This
// trusted minter is the sole legitimate construction point.
function mintPermit(epoch: number, boundary: BoundaryRequest["boundary"], signal: AbortSignal): BoundaryPermit {
  return { epoch, boundary, signal } as unknown as BoundaryPermit;
}

interface CleanupWitness {
  revision: object;
  valid: boolean;
  joined: boolean;
  roots: number;
  launches: number;
  callbacks: number;
  deadlineLive: () => boolean;
  ownDeadline: () => boolean;
}

// Readers resolve the actual facade identity; a wrapper or copied permit has no entry.
const cleanupReaders = new WeakMap<object, {
  settle: (permit: BoundaryPermit) => Promise<boolean>;
  joined: (permit: BoundaryPermit) => boolean;
  held: (permit: BoundaryPermit) => boolean;
}>();

export function settleHeldCaptureCleanup(safety: object, permit: BoundaryPermit): Promise<boolean> {
  return cleanupReaders.get(safety)?.settle(permit) ?? Promise.resolve(false);
}

/** Saved-result eligibility only. This never authorizes a launch or source operation. */
export function acceptsHeldCaptureCleanup(safety: object, permit: BoundaryPermit): boolean {
  return cleanupReaders.get(safety)?.held(permit) ?? false;
}

export function acceptsJoinedCaptureCleanup(safety: object, permit: BoundaryPermit): boolean {
  return cleanupReaders.get(safety)?.joined(permit) ?? false;
}

export class CodexExecutionSafetyImpl implements CodexExecutionSafety {
  readonly kind = "codex" as const;

  private readonly registry: ExecutionRegistry;
  private readonly seams: BoundarySeams;
  // PRD #1171 m4: the OPTIONAL executor-owned auth-mode reconcile step. Absent ⇒ no
  // reconcile (Claude/tests). Only its neutral {@link ReconcileOutcome} crosses back in.
  private readonly reconcileBeforeBoundary: ReconcileBeforeBoundary | undefined;

  // Serialization tail: each boundary awaits the previous one's release.
  private queueTail: Promise<void> = Promise.resolve();

  // Live only while an action is running under a held permit.
  private heldPermit: BoundaryPermit | undefined;
  private currentDeadlineAt = 0;
  private expireCurrentDeadline: (() => void) | undefined;
  private pendingActions: Promise<unknown>[] = [];
  private readonly cleanupWitnesses = new WeakMap<BoundaryPermit, CleanupWitness>();
  private readonly cleanAbortedCompletions = new WeakSet<Promise<unknown>>();
  private disposing = false;
  private exclusiveReaper: object | undefined;
  private readonly reaperReservations = new WeakSet<BoundaryProcessRequest>();
  private heldCapture: CredentialFreeCaptureContext | undefined;
  private lastCaptureCompletion: CompletedCredentialFreeCapture | undefined;
  private captureReserve: ((request?: BoundaryProcessRequest) => ReturnType<ExecutionRegistry["reserveLaunch"]>) | undefined;

  constructor(
    registry: ExecutionRegistry,
    seams: BoundarySeams,
    reconcileBeforeBoundary?: ReconcileBeforeBoundary,
    private readonly reaperAttempt?: () => SdkAttemptEnv | undefined,
  ) {
    this.registry = registry;
    this.seams = seams;
    this.reconcileBeforeBoundary = reconcileBeforeBoundary;
    cleanupReaders.set(this, {
      settle: permit => this.settleHeldCleanup(permit),
      joined: permit => this.acceptsJoinedCleanup(permit),
      held: permit => this.acceptsHeldCleanup(permit),
    });
  }

  private cleanupValid(permit: BoundaryPermit): CleanupWitness | undefined {
    const witness = this.cleanupWitnesses.get(permit);
    if (!witness) return undefined;
    if (this.disposing || this.registry.registryTablesCleared ||
        witness.revision !== this.registry.cleanupPoisonRevision ||
        !["closed", "poisoned"].includes(this.registry.state())) witness.valid = false;
    return witness.valid ? witness : undefined;
  }

  private cleanupEmpty(): boolean {
    return this.pendingActions.length === 0 && this.registry.pendingLaunchCount() === 0 &&
      this.registry.inFlightCallbackCount() === 0 && !this.registry.hasLiveCommandRoot() &&
      !this.registry.hasUnreapedBoundaryRoot();
  }

  private acceptsJoinedCleanup(permit: BoundaryPermit): boolean {
    const w = this.cleanupValid(permit);
    return !!w && w.joined && this.heldPermit !== permit && this.cleanupEmpty() &&
      w.roots === this.registry.rootCount() && w.launches === this.registry.launchAdmissionCursor() &&
      w.callbacks === this.registry.callbackAdmissionCursor();
  }

  private acceptsHeldCleanup(permit: BoundaryPermit): boolean {
    const w = this.cleanupValid(permit);
    return !!w && this.heldPermit === permit && this.registry.state() === "closed" &&
      w.deadlineLive() && !permit.signal.aborted && this.cleanupEmpty() &&
      w.roots === this.registry.rootCount() && w.launches === this.registry.launchAdmissionCursor() &&
      w.callbacks === this.registry.callbackAdmissionCursor();
  }

  private async settleHeldCleanup(permit: BoundaryPermit): Promise<boolean> {
    if (this.heldPermit !== permit || !this.cleanupValid(permit)) return false;
    await this.drainOwnedActions(permit);
    const w = this.cleanupValid(permit);
    if (w) {
      w.roots = this.registry.rootCount();
      w.launches = this.registry.launchAdmissionCursor();
      w.callbacks = this.registry.callbackAdmissionCursor();
    }
    // No await between this final inventory check and returning the checkpoint.
    return this.acceptsHeldCleanup(permit);
  }

  private poisonOwned(permit: BoundaryPermit, error: HarnessError, benign: boolean): void {
    const w = this.cleanupValid(permit);
    this.registry.poison(error); // Preserve legacy sticky poison/refusal semantics.
    if (w && benign) w.revision = this.registry.cleanupPoisonRevision;
    else if (w) w.valid = false;
  }

  private async drainOwnedActions(permit: BoundaryPermit): Promise<void> {
    // Each batch joins its finite admitted children; a failed sibling never skips others.
    // Startup and child completion retain their existing absolute boundary budgets.
    while (this.pendingActions.length > 0) {
      const batch = this.pendingActions;
      this.pendingActions = [];
      const settlements = await Promise.allSettled(batch);
      for (const [i, settlement] of settlements.entries()) {
        const pending = batch[i];
        if (pending && settlement.status === "rejected") this.poisonOwned(permit, {
          category: "tool", message: "spawnBoundaryAction: child action failed before reap completed",
        }, this.cleanAbortedCompletions.has(pending));
      }
    }
  }

  private now(): number { return (this.seams.now ?? Date.now)(); }

  private remainingMs(deadlineAt: number): number {
    return Math.max(0, Math.ceil(deadlineAt - this.now()));
  }

  /** Observe the late-bound shutdown proof without turning cooperative stop into revocation. */
  private watchShutdown(
    request: BoundaryRequest,
    ceiling: number,
    shrink: (deadlineAt: number) => void,
    expire: () => void,
  ): () => void {
    const stop = request.checkpointStop;
    let deadlineAt = ceiling;
    let armed = false;
    let authority: AbortSignal | undefined;
    let cancel = () => {};
    const observe = () => {
      const proof = stop?.workerShutdown?.();
      if (proof) {
        if (authority !== proof.stopSignal) {
          authority?.removeEventListener("abort", expire);
          authority = proof.stopSignal;
          authority.addEventListener("abort", expire, { once: true });
        }
        const next = Math.min(deadlineAt, proof.deadlineAt);
        if (proof.deadlineAt < ceiling && (!armed || next < deadlineAt)) {
          armed = true;
          deadlineAt = next;
          shrink(next);
          cancel();
          cancel = (this.seams.armDeadline ?? defaultArmDeadline)(request, this.remainingMs(next), expire);
        }
        if (authority.aborted || this.remainingMs(proof.deadlineAt) <= 0) expire();
      }
      try { checkCheckpointStop(stop); }
      catch (error) { if (!(error instanceof CheckpointSoftDeadlineError)) expire(); }
    };
    stop?.signal.addEventListener("abort", observe);
    stop?.lifecycleSignal?.addEventListener("abort", observe);
    stop?.terminalSignal?.addEventListener("abort", observe);
    observe();
    return () => {
      cancel();
      stop?.signal.removeEventListener("abort", observe);
      stop?.lifecycleSignal?.removeEventListener("abort", observe);
      stop?.terminalSignal?.removeEventListener("abort", observe);
      authority?.removeEventListener("abort", expire);
    };
  }

  async withBoundary<T>(
    request: BoundaryRequest,
    action: (permit: BoundaryPermit) => Promise<T>,
  ): Promise<T> {
    // Refusal must precede queue admission and preserve completed capture evidence.
    const state = this.registry.state();
    if (state === "poisoned" || state === "disposed") {
      throw new CodexBoundaryError("quiesce", [{ category: "protocol", message: "codex boundary refused: registry admission unavailable" }],
        undefined, undefined, { boundary: request.boundary, sink: request.sink, step: readStep(request) });
    }
    const deadlineAt = Math.min(this.now() + Math.max(0, request.deadlineMs),
      request.gracefulShutdownCheckpoint?.deadlineAt ?? Infinity);
    // (1) Serialize overlapping acquisitions. Each call parks on the prior tail and
    // installs a fresh release the next caller will await.
    const prior = this.queueTail;
    let release!: () => void;
    const held = new Promise<void>((res) => { release = res; });
    this.queueTail = prior.then(() => held);
    let admissionExpired = false;
    try {
      try {
        if (this.reconcileBeforeBoundary && request.checkpointStop) {
          // A cancelled queue slot cannot certify source cleanup. Keep credential
          // reconciliation behind its predecessor, bounded by the original deadline.
          let cancel = () => {};
          let cancelShutdown = () => {};
          try {
            await MaintenanceBudget.waitFor(Promise.race([prior, new Promise<never>((_resolve, reject) => {
              const refuse = (category: "timeout" | "authorization") => {
                if (admissionExpired) return;
                admissionExpired = true;
                const message = category === "timeout" ? "codex source admission deadline expired" : "codex source admission authority revoked";
                if (this.registry.state() !== "poisoned" && this.registry.state() !== "disposed") {
                  this.registry.poison({ category, message });
                }
                reject(new Error(message));
              };
              cancel = (this.seams.armDeadline ?? defaultArmDeadline)(request, this.remainingMs(deadlineAt), () => refuse("timeout"));
              cancelShutdown = this.watchShutdown(request, deadlineAt, () => {}, () => refuse("authorization"));
            })]));
          } finally { cancel(); cancelShutdown(); }
        } else {
          await MaintenanceBudget.waitFor(waitCheckpointAdmission(prior, request.checkpointStop));
        }
      } catch (error) {
        if (this.registry.isPoisoned() || admissionExpired || this.now() >= deadlineAt || request.gracefulShutdownCheckpoint?.stopSignal.aborted) {
          const errors: readonly HarnessError[] = this.registry.isPoisoned() ? this.registry.poisonErrors()
            : request.gracefulShutdownCheckpoint?.stopSignal.aborted
              ? [{ category: "authorization", message: "codex shutdown checkpoint authority revoked" }]
              : [{ category: "timeout", message: "codex boundary expired in the acquisition queue" }];
          if (this.registry.state() !== "poisoned" && this.registry.state() !== "disposed") {
            this.registry.poison(errors);
          }
          throw new CodexBoundaryError("reconcile", errors, error, undefined, {
            boundary: request.boundary, sink: request.sink, step: readStep(request),
          });
        }
        throw error;
      }
      return await this.runBoundary(request, action, deadlineAt);
    } finally {
      release();
    }
  }

  /**
   * Issue #1766: settle the epoch for a CREDENTIAL-FREE capture after a vault-locked deferral.
   * Serialized behind the same {@link queueTail} as {@link withBoundary}, so it never overlaps a
   * boundary in flight, then delegates to {@link ExecutionRegistry.settleForCapture} under one
   * absolute deadline (the queue wait draws from it). It never reconciles, never mints a
   * permit and never touches completion authority: the registry is poisoned (admission
   * refused) and drained, nothing more.
   *
   * If the deadline passes while still queued behind an earlier boundary, it returns
   * `incomplete` without touching the registry; its queue slot is then released only after
   * that earlier boundary settles, so a later boundary still cannot overlap it.
   */
  settleForCredentialFreeCapture(deadlineMs: number): Promise<CredentialFreeCaptureSettlement> {
    return this.captureOperation(deadlineMs);
  }

  /** The callback is owned by the same settlement serialization slot, never a permit. */
  async withCredentialFreeCapture<T>(
    deadlineMs: number, action: (context: CredentialFreeCaptureContext) => Promise<T>, signal?: AbortSignal,
  ): Promise<CredentialFreeCaptureResult<T>> {
    const result = await this.captureOperation(deadlineMs, action, signal);
    if (result.kind === "incomplete" || "value" in result) return result;
    throw new TrustedExecutionRefusal("capture action completion was not observed");
  }

  private async captureOperation<T>(
    deadlineMs: number, action?: (context: CredentialFreeCaptureContext) => Promise<T>, externalSignal?: AbortSignal,
  ): Promise<CredentialFreeCaptureSettlement | CredentialFreeCaptureResult<T>> {
    const deadlineAt = this.now() + Math.max(0, deadlineMs);
    const budget = MaintenanceBudget.current();
    const budgetSignal = MaintenanceBudget.signal();
    const signal = externalSignal && budgetSignal ? AbortSignal.any([externalSignal, budgetSignal]) : externalSignal ?? budgetSignal;
    const cap = budget?.cap(deadlineMs) ?? (() => this.remainingMs(deadlineAt));
    const remaining = () => signal?.aborted ? 0 : Math.min(cap(), MaintenanceBudget.current()?.remainingMs() ?? Infinity);
    const prior = this.queueTail;
    let release!: () => void;
    this.queueTail = new Promise<void>((res) => {
      release = res;
    });
    let timer: ReturnType<typeof setTimeout> | undefined;
    let queued = true;
    let onAbort: (() => void) | undefined;
    try {
      const turn = await Promise.race([
        prior.then(() => "ready" as const),
        new Promise<"expired">((res) => {
          onAbort = () => res("expired");
          signal?.addEventListener("abort", onAbort, { once: true });
          if (remaining() <= 0) { res("expired"); return; }
          timer = setTimeout(onAbort, remaining());
          timer.unref?.();
        }),
      ]);
      if (timer) clearTimeout(timer);
      if (turn === "expired" || remaining() <= 0) {
        return {
          kind: "incomplete",
          errors: [{ category: "timeout", message: "codex capture settle expired in the boundary queue" }],
        };
      }
      queued = false;
      this.lastCaptureCompletion = undefined;
      const proof = await this.registry.settleForCapture(remaining(), undefined, action !== undefined);
      if (proof.kind === "incomplete") return proof;
      if (remaining() <= 0) return { kind: "incomplete", errors: [{ category: "timeout", message: "codex capture settle expired" }] };
      if (!action) return proof;
      if (!this.seams.spawnProcess) {
        return { kind: "incomplete", errors: [{ category: "protocol", message: "credential-free capture launch wiring unavailable" }] };
      }
      const lane = this.registry.beginCapture(proof);
      const controller = new AbortController();
      let deadlineExpired = false;
      const abort = () => controller.abort(signal?.reason);
      const expire = () => { deadlineExpired = true; controller.abort(); };
      signal?.addEventListener("abort", abort, { once: true });
      const captureDeadlineAt = Math.min(deadlineAt, this.now() + remaining());
      const cancelDeadline = this.seams.armDeadline
        ? this.seams.armDeadline({ boundary: "checkpoint", deadlineMs: remaining() }, remaining(), expire)
        : defaultArmDeadline({ boundary: "checkpoint", deadlineMs: remaining() }, remaining(), expire);
      let active = true;
      let context!: CredentialFreeCaptureContext;
      context = createCaptureContext(controller.signal, captureDeadlineAt,
        request => this.spawnOwnedProcess(context, request),
        () => active && this.heldCapture === context && lane.live());
      this.heldCapture = context;
      this.captureReserve = lane.reserve;
      this.currentDeadlineAt = captureDeadlineAt;
      this.pendingActions = [];
      const reaper = this.createReaperScope(context, captureDeadlineAt,
        () => this.heldCapture === context && lane.live());
      let outcome: { ok: true; value: T } | { ok: false; error: unknown };
      try {
        if (controller.signal.aborted || remaining() <= 0) {
          return { kind: "incomplete", errors: [{ category: "timeout", message: "codex capture expired before action" }] };
        }
        try {
          outcome = { ok: true, value: await reaper.run(() => action(context)) };
        } catch (error) {
          outcome = { ok: false, error };
        }
        // Expire the spawner before draining or releasing the serialization slot.
        // A delayed continuation can never add siblings during cleanup.
        active = false;
        reaper.revoke();
        const errors: HarnessError[] = [];
        // A startup already in flight may append its completion across the await.
        // Each promise is visited once; no new request is admitted after expiry.
        for (let i = 0; i < this.pendingActions.length; i++) {
          try { await this.pendingActions[i]; }
          catch { errors.push({ category: "tool", message: "capture child cleanup failed" }); }
        }
        const childCleanupFailed = errors.length > 0;
        deadlineExpired ||= this.now() >= captureDeadlineAt;
        if (signal?.aborted || deadlineExpired) {
          errors.push({ category: "timeout", message: "codex capture deadline expired" });
        }
        const completed = errors.length === 0 ? lane.complete() : undefined;
        if (!completed && errors.length === 0) errors.push({ category: "protocol", message: "capture completion proof invalidated" });
        if (errors.length) {
          this.registry.poison(errors);
          const error = new CodexBoundaryError("action", errors, outcome.ok ? undefined : outcome.error);
          const actionRevoked = !outcome.ok &&
            (outcome.error instanceof GitBoundaryAbortError || outcome.error instanceof CaptureOperationRevokedError) &&
            outcome.error.isCaptureRevocationFor(context.signal);
          recordCaptureFailure(error, { deadlineExpired, childCleanupFailed, externallyAborted: signal?.aborted === true,
            ...(actionRevoked ? { actionRevoked: true as const } : {}) });
          throw error;
        }
        this.lastCaptureCompletion = completed as CompletedCredentialFreeCapture;
        if (!outcome.ok) throw outcome.error;
        return { kind: "observed_empty", value: outcome.value, proof: this.lastCaptureCompletion };
      } finally {
        reaper.revoke();
        active = false;
        this.heldCapture = undefined;
        this.captureReserve = undefined;
        lane.close();
        controller.abort();
        cancelDeadline();
        signal?.removeEventListener("abort", abort);
      }
    } finally {
      if (onAbort) signal?.removeEventListener("abort", onAbort);
      if (timer) clearTimeout(timer);
      if (queued) void prior.then(release);
      else release();
    }
  }

  private async runBoundary<T>(
    request: BoundaryRequest,
    action: (permit: BoundaryPermit) => Promise<T>,
    deadlineAt: number,
  ): Promise<T> {
    const boundaryAbort = new AbortController();
    const cause = request.gracefulShutdownCheckpoint;
    const stopSignal = cause?.stopSignal;
    const maintenanceSignal = cause ? MaintenanceBudget.signal() : undefined;
    const authorityAbort = new AbortController();
    const revoke = () => {
      if (authorityAbort.signal.aborted) return;
      firedStep = readStep(request);
      authorityAbort.abort();
      this.registry.poison({ category: "authorization", message: "codex shutdown checkpoint authority revoked" });
    };
    const boundarySignal = AbortSignal.any([boundaryAbort.signal, authorityAbort.signal,
      ...[stopSignal, maintenanceSignal].filter((s): s is AbortSignal => s !== undefined)]);
    const requireAuthority = (stage: "reconcile" | "quiesce" | "reap" | "action"): void => {
      if (!stopSignal?.aborted && !authorityAbort.signal.aborted) return;
      const errors: readonly HarnessError[] = [{ category: "authorization", message: "codex shutdown checkpoint authority revoked" }];
      this.registry.poison(errors);
      throw new CodexBoundaryError(stage, errors, undefined, undefined, {
        boundary: request.boundary, sink: request.sink, step: readStep(request),
      });
    };
    // Issue #1864: every boundary failure names the boundary and sink it happened at.
    const where = { boundary: request.boundary, ...(request.sink !== undefined ? { sink: request.sink } : {}) };
    // Convert the public duration to one absolute deadline. Each acquisition stage
    // receives only the remaining budget, and the action receives the same deadline
    // signal. Action bodies must propagate that signal into non-registry I/O; we do
    // not Promise.race and abandon an uncooperative action. The production runner
    // propagates it through finalize/recovery forge + worker HTTP, retry backoff and
    // message drain, while permit-owned processes consume the shrinking budget.
    // This runs after the queue wait. An unusable epoch cannot reconcile credentials;
    // refusal itself must not change poison revision or completed teardown evidence.
    const state = this.registry.state();
    if (state === "poisoned" || state === "disposed") {
      throw new CodexBoundaryError("quiesce", [{ category: "protocol", message: "codex boundary refused: registry admission unavailable" }],
        undefined, undefined, where);
    }
    requireAuthority("reconcile");
    const atEntry = Math.min(this.remainingMs(deadlineAt),
      cause ? (MaintenanceBudget.current()?.remainingMs() ?? Infinity) : Infinity);
    if (atEntry <= 0) {
      const errors: readonly HarnessError[] = [{ category: "timeout", message: "codex boundary expired in the acquisition queue" }];
      this.registry.poison(errors);
      throw new CodexBoundaryError("quiesce", errors, undefined, undefined, where);
    }
    // Issue #1900: record the caller's active step at fire time, before the abort lets the
    // action move on to another step. Only the first fire counts.
    let firedStep: BoundaryStep | undefined;
    const expireDeadline = () => {
      if (!boundaryAbort.signal.aborted) firedStep = readStep(request);
      boundaryAbort.abort();
    };
    const cancelDeadline = (this.seams.armDeadline ?? defaultArmDeadline)(request, atEntry, expireDeadline);
    const requireRemaining = (stage: "reconcile" | "quiesce" | "reap"): void => {
      requireAuthority(stage);
      if (!boundarySignal.aborted && this.remainingMs(deadlineAt) > 0 &&
          (cause ? (MaintenanceBudget.current()?.remainingMs() ?? Infinity) : Infinity) > 0) return;
      const errors: readonly HarnessError[] = [{ category: "timeout", message: `codex boundary deadline expired after ${stage}` }];
      this.registry.poison(errors);
      throw new CodexBoundaryError(stage, errors, undefined, undefined, where);
    };
    let stage: "reconcile" | "quiesce" | "reap" | "action" = "reconcile";
    const cancelShutdown = this.watchShutdown(request, deadlineAt, next => {
      deadlineAt = next;
      if (stage === "action") this.currentDeadlineAt = next;
    }, revoke);
    try {
    // (1.5) PRD #1171 m4: per-sink auth-mode reconciliation, AFTER the serialization queue
    // (in withBoundary) and BEFORE quiesce/reap/mint. A subscription run refreshes + durably
    // advances its generation here; an api_key run re-authorizes with zero refresh. A
    // `blocked` outcome (contended/quarantined/persistence-failure/HTTP error) poisons the
    // epoch and throws WITHOUT quiescing/reaping/minting or running the action — the sink
    // counter stays zero, publication is blocked, and the primary + cleanup evidence are
    // retained separately. Absent seam ⇒ no reconcile step (Claude/tests unaffected). Only
    // the neutral outcome crosses back in; this generic code never sees authMode.
    if (this.reconcileBeforeBoundary) {
      const reconciled = await this.reconcileBeforeBoundary(
        { ...request, deadlineMs: this.remainingMs(deadlineAt) },
        boundarySignal,
      );
      if (reconciled.kind === "blocked") {
        this.registry.poison(reconciled.errors);
        throw new CodexBoundaryError("reconcile", reconciled.errors, undefined, reconciled.deferral, where);
      }
      requireRemaining("reconcile");
      checkCheckpointStop(request.checkpointStop);
    }

    stage = "quiesce";
    // (2) Quiesce child admission. Must be fully quiescent or the sink is uncalled.
    requireRemaining("quiesce");
    checkCheckpointStop(request.checkpointStop);
    const q = await this.seams.quiesce({ ...request, deadlineMs: this.remainingMs(deadlineAt) });
    if (q.kind !== "quiescent") {
      const errors: readonly HarnessError[] =
        q.kind === "incomplete"
          ? q.errors
          : [{ category: "protocol", message: `withBoundary: quiesce returned non-quiescent "${q.kind}"` }];
      this.registry.poison(errors);
      throw new CodexBoundaryError("quiesce", errors, undefined, undefined, where);
    }
    requireRemaining("quiesce");
    const epoch = q.epoch;

    // (3) Reap every registered root. Must be observed-empty AND carry the SAME
    // epoch we just quiesced at; a mismatch means a stale/foreign permit domain.
    stage = "reap";
    const r = await this.seams.reap({ ...request, deadlineMs: this.remainingMs(deadlineAt) }, epoch);
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
      throw new CodexBoundaryError("reap", errors, undefined, undefined, where);
    }
    requireRemaining("reap");
    checkCheckpointStop(request.checkpointStop);

    // (4) Mint the unforgeable matching-epoch permit and (5) run the action while
    // holding it. The permit/closed epoch is held through the full async action and
    // every boundary-action child; release only after actual settlement. The action
    // consumes permit.signal rather than being raced and abandoned at the deadline.
    stage = "action";
    const permit = mintPermit(epoch, request.boundary, boundarySignal);
    this.heldPermit = permit;
    this.cleanupWitnesses.set(permit, {
      revision: this.registry.cleanupPoisonRevision, valid: this.registry.state() === "closed",
      joined: false, roots: 0, launches: 0, callbacks: 0,
      ownDeadline: () => boundaryAbort.signal.aborted && !authorityAbort.signal.aborted &&
        !stopSignal?.aborted && !maintenanceSignal?.aborted,
      deadlineLive: () => !boundarySignal.aborted && !stopSignal?.aborted && !authorityAbort.signal.aborted &&
        this.remainingMs(this.currentDeadlineAt) > 0,
    });
    this.currentDeadlineAt = deadlineAt;
    this.expireCurrentDeadline = expireDeadline;
    this.pendingActions = [];

    const reaper = this.createReaperScope(permit, deadlineAt,
      () => this.heldPermit === permit && this.registry.state() === "closed");
    let outcome: { ok: true; value: T } | { ok: false; error: unknown };
    try {
      outcome = { ok: true, value: await reaper.run(() => action(permit)) };
    } catch (e) {
      outcome = { ok: false, error: e };
    }
    reaper.revoke();
    // Drain in batches until no action remains. Promise.allSettled snapshots an array
    // synchronously, so a single await would miss an action admitted while an earlier
    // batch was settling. There is no await between the empty check and clearing the
    // held permit, therefore a still-later call is refused synchronously below.
    await this.drainOwnedActions(permit);
    const cleanup = this.cleanupValid(permit);
    if (cleanup && this.cleanupEmpty()) {
      cleanup.joined = true;
      cleanup.roots = this.registry.rootCount();
      cleanup.launches = this.registry.launchAdmissionCursor();
      cleanup.callbacks = this.registry.callbackAdmissionCursor();
    }

    this.heldPermit = undefined;
    this.expireCurrentDeadline = undefined;

    if (stopSignal?.aborted || authorityAbort.signal.aborted) {
      this.registry.poison({ category: "authorization", message: "codex shutdown checkpoint authority revoked" });
    } else if (boundarySignal.aborted) {
      this.poisonOwned(permit, {
        category: "timeout",
        message:
          firedStep !== undefined
            ? `codex boundary action deadline exceeded during ${STEP_LABELS[firedStep]}`
            : "codex boundary action deadline exceeded",
      }, boundaryAbort.signal.aborted && !authorityAbort.signal.aborted && !stopSignal?.aborted &&
        !maintenanceSignal?.aborted);
    }

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
        undefined,
        { ...where, step: firedStep ?? readStep(request) },
      );
    }
    if (!outcome.ok) throw outcome.error;
    checkCheckpointStop(request.checkpointStop);
    return outcome.value;
    } catch (error) {
      if (error instanceof CheckpointSoftDeadlineError) {
        // Soft is a clean-stop certificate, never a way around source poison,
        // hard expiry or revoked authority. Preserve the primary and cleanup evidence.
        let revoked = stopSignal?.aborted === true;
        try { checkCheckpointStop(request.checkpointStop); }
        catch (stopError) { if (!(stopError instanceof CheckpointSoftDeadlineError)) revoked = true; }
        const expired = boundarySignal.aborted || this.remainingMs(deadlineAt) <= 0 ||
          (cause ? (MaintenanceBudget.current()?.remainingMs() ?? Infinity) : Infinity) <= 0;
        if (this.registry.isPoisoned() || revoked || expired) {
          if (revoked) this.registry.poison({ category: "authorization", message: "codex shutdown checkpoint authority revoked" });
          else if (expired) this.registry.poison({ category: "timeout", message: "codex boundary deadline expired during soft stop" });
          throw new CodexBoundaryError(stage, this.registry.poisonErrors(), error, undefined,
            { ...where, step: firedStep ?? readStep(request) });
        }
      }
      throw error;
    } finally {
      cancelDeadline();
      cancelShutdown();
    }
  }

  /** A delayed timer must not turn an expired absolute allowance into a new launch. */
  private requireBoundaryProcessBudget(permit: BoundaryPermit | CredentialFreeCaptureContext): number {
    if (this.heldPermit !== permit && this.heldCapture !== permit) {
      throw new TrustedExecutionRefusal("codex boundary process refused: stale permit");
    }
    const remaining = this.remainingMs(this.currentDeadlineAt);
    if (remaining <= 0) this.expireCurrentDeadline?.();
    if (permit.signal.aborted || remaining <= 0) {
      if (!this.registry.isPoisoned()) this.poisonOwned(permit as BoundaryPermit, {
        category: "timeout", message: "codex boundary process deadline exceeded before admission",
      }, this.heldPermit === permit && this.cleanupWitnesses.get(permit as BoundaryPermit)?.ownDeadline() === true);
      throw new TrustedExecutionRefusal("codex boundary process refused: boundary deadline exceeded");
    }
    return remaining;
  }

  async spawnBoundaryProcess(
    permit: BoundaryPermit,
    request: BoundaryProcessRequest,
  ): Promise<BoundaryProcessHandle> {
    if (this.heldPermit !== permit) throw new TrustedExecutionRefusal("codex boundary process refused: stale permit");
    return this.spawnOwnedProcess(permit, request);
  }

  private spawnOwnedProcess(
    permit: BoundaryPermit | CredentialFreeCaptureContext,
    request: BoundaryProcessRequest,
  ): Promise<BoundaryProcessHandle> {
    // Own startup, admission and failed-admission cleanup before invoking the seam.
    let resolve!: (handle: BoundaryProcessHandle) => void;
    let reject!: (error: unknown) => void;
    const startup = new Promise<BoundaryProcessHandle>((done, fail) => { resolve = done; reject = fail; });
    this.pendingActions.push(startup.then(() => undefined, () => undefined));
    void this.startOwnedProcess(permit, request).then(resolve, reject);
    return startup;
  }

  private createReaperScope(
    owner: BoundaryPermit | CredentialFreeCaptureContext,
    deadlineAt: number,
    live: () => boolean,
  ): ReturnType<typeof reaperCaptureScope> {
    const requireEmpty = (ownReservation = false): void => {
      if (this.registry.pendingLaunchCount() !== (ownReservation ? 1 : 0) ||
          this.registry.inFlightCallbackCount() !== 0 || this.registry.hasUnreapedBoundaryRoot()) {
        throw new TrustedExecutionRefusal("reaper requires no live capture children or callbacks");
      }
    };
    return reaperCaptureScope({
      owner, attempt: this.reaperAttempt?.(), signal: owner.signal, deadlineAt, live,
      spawn: request => this.spawnOwnedProcess(owner, request),
      track: pending => { this.pendingActions.push(pending); },
      enterExclusive: () => {
        if (!live() || this.exclusiveReaper) throw new TrustedExecutionRefusal("reaper pipeline admission refused");
        requireEmpty();
        const token = Object.freeze({});
        this.exclusiveReaper = token;
        return token;
      },
      validateExclusive: (token, request) => {
        if (!live() || this.exclusiveReaper !== token ||
            (this.heldCapture !== owner && this.heldPermit !== owner)) {
          throw new TrustedExecutionRefusal("reaper pipeline owner expired");
        }
        // Before reservation require zero; the launcher's final synchronous check
        // excludes only this request's safety-owned, still-pending reservation.
        requireEmpty(request !== undefined && this.reaperReservations.has(request));
      },
      leaveExclusive: token => {
        if (this.exclusiveReaper === token) this.exclusiveReaper = undefined;
      },
    });
  }

  private async startOwnedProcess(
    permit: BoundaryPermit | CredentialFreeCaptureContext,
    request: BoundaryProcessRequest,
  ): Promise<BoundaryProcessHandle> {
    const capture = this.heldCapture === permit ? this.heldCapture : undefined;
    if (isClosedReaperRequest(request)) assertClosedReaperRequest(request, permit.signal, permit);
    else if (this.exclusiveReaper) throw new TrustedExecutionRefusal("reaper pipeline owns child admission");
    else if (capture) assertCaptureRequest(capture, request);
    else if (this.heldPermit !== permit) throw new TrustedExecutionRefusal("codex boundary process refused: stale permit");
    if (permit.signal.aborted) {
      const message = "codex boundary process refused: boundary deadline exceeded";
      throw capture ? new CaptureOperationRevokedError(message, capture.signal) : new TrustedExecutionRefusal(message);
    }
    this.requireBoundaryProcessBudget(permit);
    if (request.timeoutMs !== undefined && (!Number.isFinite(request.timeoutMs) || request.timeoutMs <= 0)) {
      throw new TrustedExecutionRefusal("codex boundary process timeout must be positive and finite");
    }
    if (request.recoverableTimeout && (capture || !("boundary" in permit) || permit.boundary !== "checkpoint")) {
      throw new TrustedExecutionRefusal("recoverable child timeout is checkpoint-only");
    }
    if (!request.argv[0]?.startsWith("/")) {
      const error: HarnessError = { category: "protocol", message: "boundary process executable must be absolute" };
      this.registry.poison(error);
      throw new TrustedExecutionRefusal(error.message);
    }
    if (request.identity === "worker_pat" && !capture) {
      if (this.registry.state() !== "closed") throw new TrustedExecutionRefusal("codex boundary process refused: admission not closed");
      if (this.registry.hasLiveCommandRoot()) throw new TrustedExecutionRefusal("codex boundary process refused: command roots live");
    }
    const reaperRequest = isClosedReaperRequest(request) ? request : undefined;
    const ownedDeadlineAt = Math.min(this.currentDeadlineAt, reaperRequest ? closedReaperDeadline(request) : Infinity);
    const reservation = capture ? this.captureReserve!(reaperRequest) : this.registry.reserveLaunch("boundary_action", reaperRequest);
    if (reservation.kind !== "reserved" || !this.seams.spawnProcess) {
      if (reservation.kind === "reserved") this.registry.cancelReservation(reservation.reservation);
      const error: HarnessError = { category: "protocol", message: "boundary process launch unavailable" };
      this.registry.poison(error);
      throw new TrustedExecutionRefusal(error.message);
    }
    const childDeadlineAt = Math.min(ownedDeadlineAt, this.now() + (request.timeoutMs ?? Infinity));
    const startupCleanup: StartupCleanupAuthorization | undefined = capture || isClosedReaperRequest(request) || request.recoverableTimeout
      ? Object.freeze({ hardDeadlineAt: ownedDeadlineAt, attempt: Object.freeze({}) })
      : undefined;
    const verifiedStartupCleanup = watchStartupCleanup(startupCleanup);
    let hardRemaining: number;
    try { hardRemaining = this.requireBoundaryProcessBudget(permit); }
    catch (error) {
      this.registry.cancelReservation(reservation.reservation);
      throw error;
    }
    let launched: SpawnedBoundaryProcess;
    if (reaperRequest) this.reaperReservations.add(request);
    try {
      const launch = this.seams.spawnProcess(request, Math.min(hardRemaining, Math.max(1, childDeadlineAt - this.now())), startupCleanup);
      launched = await launch;
    } catch (cause) {
      // The seam settles only after its owned cleanup; keep the reservation until then.
      this.registry.cancelReservation(reservation.reservation);
      if (request.recoverableTimeout && !permit.signal.aborted &&
        this.remainingMs(this.currentDeadlineAt) > 0 && verifiedStartupCleanup(cause)) {
        // The launcher and cache have already settled the authentic rejection.
        // This recovery result carries no authority to certify another cleanup.
        throw new CheckpointChildStartupTimeoutError(cause);
      }
      const classification = classifyCodexLaunchFailure(cause);
      const cleanupVerified = verifiedStartupCleanup(cause);
      // Classifications come from launcher-owned evidence, never error fields.
      const unstarted = classification === "profile_quarantine" || classification.startsWith("spawn_");
      if (!cleanupVerified && !unstarted) this.registry.markLaunchCleanupUnconfirmed();
      const error = new CodexLaunchError(classification, cleanupVerified ? "verified" : "unconfirmed", cause);
      this.registry.poison({ category: classification === "started_deadline" ? "timeout" : "tool", message: error.message });
      throw error;
    } finally {
      this.reaperReservations.delete(request);
    }
    const registered = this.registry.registerRoot(reservation.reservation, launched.root);
    if (!registered.ok) {
      this.registry.retainRejectedRoot(reservation.reservation, launched.root);
      await launched.root.dispose(this.remainingMs(Math.min(ownedDeadlineAt, this.currentDeadlineAt))).catch(() => undefined);
      throw new TrustedExecutionRefusal("boundary process failed registry admission");
    }
    const cancellation = new AbortController();
    let finished = false;
    let resolveStop!: () => void;
    const stopped = new Promise<void>((resolve) => { resolveStop = resolve; });
    const onStop = (): void => resolveStop();
    cancellation.signal.addEventListener("abort", onStop, { once: true });
    permit.signal.addEventListener("abort", onStop, { once: true });
    if (permit.signal.aborted) resolveStop();
    let ownerAbortSelected = false;
    let completed!: Promise<{ readonly code: number; readonly softTimedOut?: true }>;
    completed = (async (): Promise<{ readonly code: number; readonly softTimedOut?: true }> => {
      let terminal: { readonly code: number } | undefined;
      let terminalError: unknown;
      try {
        if (this.now() >= childDeadlineAt) {
          if (request.recoverableTimeout && !permit.signal.aborted && this.remainingMs(this.currentDeadlineAt) > 0) {
            throw new SupervisedChildExitTimeoutError(request.timeoutMs);
          }
          const message = "boundary process child deadline exceeded during launch";
          throw capture?.signal.aborted
            ? new CaptureOperationRevokedError(message, capture.signal) : new TrustedExecutionRefusal(message);
        }
        terminal = await this.waitChildOrAbort(
          launched,
          permit.signal,
          cancellation.signal,
          Math.max(1, childDeadlineAt - this.now()),
          capture?.signal,
          () => { ownerAbortSelected = true; },
        );
      } catch (error) {
        terminalError = error;
      }
      const softTimeout = request.recoverableTimeout && terminalError instanceof SupervisedChildExitTimeoutError &&
          !permit.signal.aborted && !cancellation.signal.aborted && this.remainingMs(this.currentDeadlineAt) > 0;
      let disposalFailed = false;
      let disposed = false;
      const dispose = async (): Promise<void> => {
        if (disposed) return;
        disposed = true;
        try {
          await launched.root.dispose(this.remainingMs(Math.min(ownedDeadlineAt, this.currentDeadlineAt)));
        } catch {
          disposalFailed = true;
          this.registry.poison({ category: "tool", message: "boundary process disposal failed" });
        }
      };
      if (softTimeout || cancellation.signal.aborted || permit.signal.aborted) await dispose();
      const reap = this.registry.reapRoot(launched.root, this.remainingMs(Math.min(ownedDeadlineAt, this.currentDeadlineAt)));
      // The completion owner alone disposes and reaps. A stop arriving during
      // natural-exit reap still stops the owned root, without starting a second reap.
      await Promise.race([reap, stopped]);
      if (cancellation.signal.aborted || permit.signal.aborted) await dispose();
      const reaped = await reap;
      if (cancellation.signal.aborted || permit.signal.aborted) await dispose();
      if (disposalFailed) throw new CheckpointUnsafeCleanupError("boundary process disposal failed");
      if (!reaped.ok) throw new CheckpointUnsafeCleanupError("boundary process root did not reap cleanly");
      if (softTimeout && !cancellation.signal.aborted) {
        // The timeout only authorizes a skip after the supervisor has stopped its
        // owned process group and the registry has verified a full-root reap.
        // Keep this work inside the held permit and, for git, its bare lock.
        if (permit.signal.aborted || this.remainingMs(this.currentDeadlineAt) <= 0) {
          throw new TrustedExecutionRefusal("checkpoint child root did not reap cleanly before boundary deadline");
        }
        launched.stdout?.destroy();
        launched.stderr?.destroy();
        return { code: -1, softTimedOut: true };
      }
      // Preserve a child-selected failure even if capture expires before the await
      // catch or during reap. Only the wait owner can identify its own abort.
      if (capture && terminalError !== undefined &&
          !(terminalError instanceof CaptureOperationRevokedError && terminalError.isCaptureRevocationFor(capture.signal))) {
        throw terminalError;
      }
      if (permit.signal.aborted) {
        // The owner may abort after the primary returned while its full-root reap
        // is still pending. A selected independent primary error remains disqualifying.
        const cleanPrimary = ownerAbortSelected || (terminalError === undefined && terminal !== undefined);
        if (cleanPrimary && disposed && !disposalFailed && reaped.ok && this.heldPermit === permit) {
          this.cleanAbortedCompletions.add(completed);
        }
        const message = "boundary process deadline exceeded";
        throw capture ? new CaptureOperationRevokedError(message, capture.signal) : new TrustedExecutionRefusal(message);
      }
      if (cancellation.signal.aborted) {
        launched.stdout?.destroy();
        launched.stderr?.destroy();
        return { code: -1 };
      }
      if (terminalError !== undefined) throw terminalError;
      if (!terminal) throw new TrustedExecutionRefusal("boundary process produced no terminal result");
      return terminal;
    })().finally(() => {
      finished = true;
      cancellation.signal.removeEventListener("abort", onStop);
      permit.signal.removeEventListener("abort", onStop);
    });
    let cancelPromise: Promise<void> | undefined;
    const cancel = (): Promise<void> => {
      if (!finished) cancellation.abort();
      return cancelPromise ??= completed.then(() => undefined);
    };
    // Track synchronously before returning the streams. Even a caller that forgets
    // `completed` cannot release the permit while this process/root is live.
    this.pendingActions.push(completed);
    void completed.catch(() => undefined);
    return {
      stdin: launched.stdin,
      stdout: launched.stdout,
      stderr: launched.stderr,
      completed,
      cancel,
    };
  }

  private waitChildOrAbort(
    launched: SpawnedBoundaryProcess,
    signal: AbortSignal,
    cancellation: AbortSignal,
    deadlineMs: number,
    captureSignal?: AbortSignal,
    onOwnerAbort?: () => void,
  ): Promise<{ readonly code: number }> {
    const abortError = (): TrustedExecutionRefusal => {
      const message = "boundary process deadline exceeded";
      return captureSignal?.aborted
        ? new CaptureOperationRevokedError(message, captureSignal) : new TrustedExecutionRefusal(message);
    };
    if (signal.aborted) { onOwnerAbort?.(); return Promise.reject(abortError()); }
    if (cancellation.aborted) return Promise.resolve({ code: -1 });
    return new Promise((resolve, reject) => {
      const cleanup = (): void => {
        signal.removeEventListener("abort", onAbort);
        cancellation.removeEventListener("abort", onCancel);
      };
      let selected = false;
      const onAbort = (): void => {
        if (selected) return;
        selected = true;
        onOwnerAbort?.();
        cleanup(); reject(abortError());
      };
      const onCancel = (): void => { selected = true; cleanup(); resolve({ code: -1 }); };
      signal.addEventListener("abort", onAbort, { once: true });
      cancellation.addEventListener("abort", onCancel, { once: true });
      launched.waitChild(deadlineMs).then(
        (result) => { selected = true; cleanup(); resolve(result); },
        (error: unknown) => { selected = true; cleanup(); reject(error); },
      );
    });
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
    // Refuse before creating/tracking a promise. Besides making the permit check
    // synchronous with admission, this prevents repeated out-of-boundary calls from
    // accumulating already-settled refusal promises in pendingActions.
    if (this.heldPermit !== permit) {
      return Promise.resolve({ kind: "refused", reason: "stale_permit" });
    }
    const p = this.runBoundaryAction(permit, argv, identity);
    this.pendingActions.push(p);
    return p;
  }

  private async runBoundaryAction(
    permit: BoundaryPermit,
    argv: readonly string[],
    identity: BoundaryActionIdentity,
  ): Promise<BoundaryActionOutcome> {
    // Object identity binds the permit to THIS acquisition. The epoch is stable for
    // the run, so comparing only its number would admit a permit from an earlier
    // checkpoint inside a later credentialed_git/finalize boundary.
    if (this.heldPermit !== permit) {
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
    if (this.exclusiveReaper) throw new TrustedExecutionRefusal("reaper pipeline owns child admission");
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
      root = await this.seams.spawnRoot(argv, identity, this.remainingMs(this.currentDeadlineAt));
    } catch (e) {
      this.registry.cancelReservation(reserved.reservation);
      const error: HarnessError = {
        category: "tool",
        message: `spawnBoundaryAction: spawn failed (${safeErrorName(e)})`,
      };
      this.registry.poison(error);
      return { kind: "poisoned", error };
    }
    const registered = this.registry.registerRoot(reserved.reservation, root);
    if (!registered.ok) {
      // The reservation was unknown/already-settled or the root's kind did not match:
      // the registry has poisoned itself and never admitted this root. Do NOT reap an
      // unadmitted root; best-effort tear the just-spawned process down and surface the
      // poison so the boundary is never treated as clean.
      await root.dispose(this.remainingMs(this.currentDeadlineAt)).catch(() => {});
      return { kind: "poisoned", error: registered.error };
    }
    // Hold the permit until this root reaps its whole descendant set.
    let reap: ReapOutcome;
    try {
      reap = await root.reap(this.remainingMs(this.currentDeadlineAt));
    } catch {
      const error: HarnessError = {
        category: "tool",
        message: "spawnBoundaryAction: root reap rejected before completion",
      };
      this.registry.poison(error);
      return { kind: "poisoned", error };
    }
    if (!reap.ok) {
      this.registry.poison(reap.error);
      return { kind: "poisoned", error: reap.error };
    }
    return { kind: "settled" };
  }

  /** Exact completed acquisition evidence; independent of disposed registry tables.
   * This check grants no launch or ordinary boundary authority. */
  /** Teardown bookkeeping only: a failed action may still have settled its owned roots.
   * This evidence grants neither capture nor normal execution authority. */
  completedCaptureForTeardown(): CompletedCredentialFreeCapture | undefined {
    const proof = this.lastCaptureCompletion;
    return !this.heldCapture && proof && this.registry.acceptsCompletedCapture(proof) ? proof : undefined;
  }

  acceptsCompletedCapture(proof: CompletedCredentialFreeCapture): boolean {
    return this.registry.acceptsCompletedCapture(proof);
  }

  /** Terminal disposal shares the acquisition queue with capture and boundaries.
   * The deadline bounds acquisition only; once owned, disposal joins its cleanup.
   * An expired acquisition retains its slot until the real predecessor settles. */
  async dispose(request: BoundaryRequest): Promise<ToolDisposal> {
    this.disposing = true;
    const deadlineAt = this.now() + Math.max(0, request.deadlineMs);
    const prior = this.queueTail;
    let release!: () => void;
    this.queueTail = new Promise<void>(resolve => { release = resolve; });
    let queued = true;
    let cancelDeadline: (() => void) | undefined;
    try {
      // Only a tail already settled at entry makes disposal idle. Close the
      // observation window after one microtask so a genuinely queued predecessor
      // settling later cannot grant the zero-budget cleanup exception.
      let idle = false;
      let observing = true;
      const ready = prior.then(() => {
        if (observing) idle = true;
        return "ready" as const;
      });
      queueMicrotask(() => { observing = false; });
      const turn = await Promise.race([
        ready,
        new Promise<"expired">(resolve => {
          const remaining = this.remainingMs(deadlineAt);
          if (remaining <= 0) { resolve("expired"); return; }
          cancelDeadline = (this.seams.armDeadline ?? defaultArmDeadline)(
            request, remaining, () => resolve("expired"));
        }),
      ]);
      cancelDeadline?.();
      cancelDeadline = undefined;
      if (!idle && (turn === "expired" || this.remainingMs(deadlineAt) <= 0)) {
        return { kind: "incomplete", errors: [{
          category: "timeout", message: "codex disposal expired in the boundary queue",
        }] };
      }
      queued = false;
      return await this.seams.dispose({ ...request, deadlineMs: this.remainingMs(deadlineAt) });
    } finally {
      cancelDeadline?.();
      if (queued) void prior.then(release);
      else release();
    }
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
  reconcileBeforeBoundary?: ReconcileBeforeBoundary,
  // Optional terminal hook run after the registry's tools are disposed. The terminal
  // boundary owner (the runner, after the last sink) invokes `dispose` exactly at the
  // true terminal — which is AFTER run()'s finally under deferRegistryTeardown — so the
  // executor uses this to evict tokens released by the POST-RUN sink reconciles (which
  // run()'s already-completed finally could not have evicted). Idempotent by contract.
  // It receives the dispose request's absolute deadline (epoch ms, taken as the dispose
  // begins) so bounded terminal work can cap itself by what remains of it.
  onDispose?: (deadlineAt: number) => void | Promise<void>,
  spawnProcess?: SpawnBoundaryProcessSeam,
  // Optional boundary-deadline trigger (see BoundarySeams.armDeadline); absent in
  // production, so the deadline is the default wall-clock timer.
  armDeadline?: ArmBoundaryDeadline,
  now?: () => number,
  internal?: { reaperAttempt: () => SdkAttemptEnv | undefined },
): CodexExecutionSafetyImpl {
  let terminalHookFailure: HarnessError | undefined;
  return new CodexExecutionSafetyImpl(
    registry,
    {
      quiesce: (request) => registry.quiesceChildren(request.deadlineMs),
      reap: (request, closedEpoch) => registry.reapProcesses(request.deadlineMs, closedEpoch),
      dispose: async (request) => {
        const deadlineAt = (now ?? Date.now)() + Math.max(0, request.deadlineMs);
        // `onDispose` (the executor's post-run sink token eviction) must run even if
        // `disposeTools` ever rejects — otherwise those tokens would leak. `disposeTools`
        // catches internally today. Preserve primary failure independently, and latch
        // hook refusal so cleared tables cannot manufacture a later disposed result.
        let outcome: { ok: true; value: ToolDisposal } | { ok: false; error: unknown };
        try { outcome = { ok: true, value: await registry.disposeTools(request.deadlineMs) }; }
        catch (error) { outcome = { ok: false, error }; }
        let hookError: unknown;
        let hookFailed = false;
        try { await onDispose?.(deadlineAt); }
        catch (error) {
          hookError = error;
          hookFailed = true;
          const evidence: HarnessError = { category: "tool",
            message: `terminal cleanup hook failed (${safeErrorName(error)})` };
          terminalHookFailure ??= evidence;
          registry.poison(evidence);
        }
        if (!outcome.ok) {
          if (hookFailed) throw new AggregateError([outcome.error, hookError],
            "codex registry disposal and terminal cleanup failed", { cause: outcome.error });
          throw outcome.error;
        }
        if (terminalHookFailure) return { kind: "incomplete", errors: [
          ...(outcome.value.kind === "incomplete" ? outcome.value.errors : []), terminalHookFailure,
        ] };
        return outcome.value;
      },
      spawnRoot,
      spawnProcess,
      armDeadline,
      now,
    },
    reconcileBeforeBoundary,
    internal?.reaperAttempt,
  );
}
