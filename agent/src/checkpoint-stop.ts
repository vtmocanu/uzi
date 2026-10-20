import { TrustedExecutionRefusal } from "./trusted-execution-refusal.js";
import type { CheckpointChildStartupTimeoutError, GracefulShutdownCheckpoint } from "./harness.js";

/** Internal done-checkpoint cancellation; this carries no credential authority. */
export interface CheckpointStop {
  readonly signal: AbortSignal;
  /** Captured lifecycle fence; only typed cooperative reasons or shutdown proof explain abort. */
  readonly lifecycleSignal?: AbortSignal;
  /** Independent terminal fence, including delivery after a spent pause lifecycle. */
  readonly terminalSignal?: AbortSignal;
  /** Owner supplies the typed cooperative lifecycle discriminator without a steering dependency. */
  readonly cooperativeLifecycleReason?: (reason: unknown) => boolean;
  /** Late-bound SteeringChannel.workerShutdown proof, for admission classification only. */
  readonly workerShutdown?: () => GracefulShutdownCheckpoint | undefined;
  /** Test clock shared with the owning hard allowance. */
  readonly now?: () => number;
  readonly authorityRevoked?: () => boolean;
}

/** A cooperative stop is skippable only after all owned work has settled cleanly. */
export class CheckpointSoftDeadlineError extends Error {
  readonly checkpointBoundary = "soft";
  constructor(cause?: CheckpointChildStartupTimeoutError) {
    super(cause === undefined
      ? "checkpoint publication soft deadline exceeded"
      : "checkpoint publication soft deadline exceeded (started_deadline; cleanup verified)",
    cause === undefined ? undefined : { cause });
    this.name = "AbortError";
  }
}

/** Shared hard Git boundary discriminator; independent of the Codex implementation. */
export class GitBoundaryAbortError extends Error {
  readonly checkpointBoundary = "hard";
  constructor(message: string, cause?: unknown) {
    super(message, { cause });
    this.name = "AbortError";
  }
}

/** Cancellation without verified disposal and full-root reap is never skippable. */
export class CheckpointUnsafeCleanupError extends TrustedExecutionRefusal {
  readonly checkpointBoundary = "unsafe_cleanup";
  constructor(message: string, cause?: unknown) {
    super(message, { cause });
  }
}

export function checkCheckpointStop(stop?: CheckpointStop): void {
  if (!stop) return;
  const shutdown = stop.workerShutdown?.();
  const now = stop.now ?? Date.now;
  if (shutdown?.stopSignal.aborted || (shutdown && now() >= shutdown.deadlineAt) ||
      stop.authorityRevoked?.() || (stop.terminalSignal?.aborted && !shutdown) ||
      (stop.lifecycleSignal?.aborted && !shutdown &&
        !stop.cooperativeLifecycleReason?.(stop.lifecycleSignal.reason))) {
    throw new GitBoundaryAbortError("checkpoint authority revoked");
  }
  if (stop.signal.aborted) throw new CheckpointSoftDeadlineError();
}

/** Forfeit admission only. The caller must retain its predecessor in its queue tail. */
export async function waitCheckpointAdmission(prior: Promise<void>, stop?: CheckpointStop): Promise<void> {
  checkCheckpointStop(stop);
  if (!stop) return prior;
  let onAbort!: () => void;
  try {
    await Promise.race([prior, new Promise<never>((_resolve, reject) => {
      onAbort = () => {
        try { checkCheckpointStop(stop); } catch (error) { reject(error); }
      };
      stop.signal.addEventListener("abort", onAbort, { once: true });
      if (stop.signal.aborted) onAbort();
    })]);
    checkCheckpointStop(stop);
  } finally {
    stop.signal.removeEventListener("abort", onAbort);
  }
}

/** Typed pause or worker shutdown can explain lifecycle abort only before credential work.
 * Neither explanation authorizes a release or refresh. */
export function cleanCheckpointAdmission(stop: CheckpointStop | undefined, lifecycle?: AbortSignal): boolean {
  if (!stop?.signal.aborted) return false;
  const shutdown = stop.workerShutdown?.();
  const now = stop.now ?? Date.now;
  if (stop.authorityRevoked?.() || (stop.terminalSignal?.aborted && !shutdown) ||
      shutdown?.stopSignal.aborted || (shutdown && now() >= shutdown.deadlineAt)) return false;
  return [stop.lifecycleSignal, lifecycle].every(signal =>
    !signal?.aborted || shutdown !== undefined ||
    stop.cooperativeLifecycleReason?.(signal.reason) === true);
}

/** Preserve hard/unsafe errors across advisory catches, including legacy Codex names. */
export function isCheckpointBoundaryFailure(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  const boundary = (error as { checkpointBoundary?: unknown }).checkpointBoundary;
  return boundary === "soft" || boundary === "hard" || boundary === "unsafe_cleanup" ||
    error.name === "CodexBoundaryError" || error instanceof TrustedExecutionRefusal;
}
