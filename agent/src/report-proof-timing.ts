/** Wall-clock bounds of the report-retirement proof and the worker finalize sweep.
 *
 * Production values are fixed. The accessors exist as narrow seams so a test can lend a positive
 * stage headroom with `t.mock.method` (restored by the test runner) instead of racing the host's
 * speed. Callers read an accessor ONCE per operation so the computed deadline and the timer that
 * enforces it use the same value. Nothing here is configurable through env or config. */
export const reportProofTiming = {
  /** `retireRecoveryReport`: budget of one archived-report proof (also capped by the caller's deadline). */
  proofBudgetMs: (): number => 1_000,
  /** `retryPendingFinalize`: how long one finalize authority may run before it is abandoned. */
  finalizeAuthorityMs: (): number => 1_000,
  /** `sweepPendingFinalizes`: deadline and timer of one whole finalize pass. */
  finalizePassMs: (): number => 5_000,
};
