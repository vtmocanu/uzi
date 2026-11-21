// PRD #3252 D8 (m4): the settings barrier could not prove the runtime applied the requested phase model
// and effort, so the turn was stopped before any provider request. Thrown by the Claude harness, carried
// through the executor's retry arms untouched, and turned into a worker hold by the runner.

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
