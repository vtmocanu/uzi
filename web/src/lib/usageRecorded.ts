import type { RunUsage } from "./apiTypes";

// Issue #4209: one predicate for "this usage bundle recorded anything". run_count counts only
// primary work runs, so a helper-only user has run_count 0 yet real spend; and the admin API
// appends zero-usage rows for users that only have run outcomes. Recorded means any lifetime
// token, cost, subscription/unreported run count, or a primary work run.
export function hasRecordedLifetimeUsage(
  lifetime: RunUsage,
  runCount: number,
  subscriptionRunCount: number,
  unreportedRunCount: number,
): boolean {
  return (
    lifetime.input_tokens + lifetime.cache_read_tokens + lifetime.cache_creation_tokens + lifetime.output_tokens !== 0 ||
    lifetime.cost_usd !== 0 ||
    subscriptionRunCount !== 0 ||
    unreportedRunCount !== 0 ||
    runCount > 0
  );
}
