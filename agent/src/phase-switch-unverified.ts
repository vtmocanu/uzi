// PRD #3252 D8 (m4): phase verification failed before prompt release, or a later init contradicted
// the admitted settings. Pre-release failures send no prompt; a contradictory init may follow an
// admitted request. The executor propagates this error without retry and the runner holds the work.

export type PhaseSwitchCause =
  | "model_mismatch"
  | "effort_mismatch"
  | "timeout"
  | "method_missing"
  | "malformed"
  | "no_characterized_expectation";

export interface PhaseSettings {
  model?: string;
  effort?: string;
}

export class PhaseSwitchUnverifiedError extends Error {
  override readonly cause: PhaseSwitchCause;
  constructor(
    cause: PhaseSwitchCause,
    readonly requested: PhaseSettings,
    readonly observed: PhaseSettings,
  ) {
    super(`phase switch unverified: ${cause}`);
    this.name = "PhaseSwitchUnverifiedError";
    this.cause = cause;
  }
}
