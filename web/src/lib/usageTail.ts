import type { UsageTail } from "./apiTypes";
import { formatCost } from "./formatTokens";
import { stripUnsafeChars } from "./safeText";

// Issue #2014 (ADR-2014): display rules for the estimated usage tail of an interrupted
// Claude session. The tail is shown APART from the metered total and never summed into it.

/** Plain-words text for the closed coverage-reason set the server emits. */
const REASON_TEXT: Record<string, string> = {
  leg_not_closed: "the last session was cut off before it reported its total",
  ordinal_gap: "some model calls were never received",
  output_not_final: "output tokens of the last call were still streaming",
  superseded_uncertain: "it is unclear whether a later total already counted some calls",
  records_dropped: "some usage records were dropped",
  record_cap_reached: "the per-run usage record limit was reached",
  unresolved: "coverage could not be determined",
};

/** One coverage reason in plain words. An unknown reason (a newer server) is shown raw,
 *  stripped of control and bidi characters so it cannot reorder or hide surrounding text. */
export function coverageReasonText(reason: string): string {
  // Object.hasOwn: a reason named "constructor" or "__proto__" must not hit the prototype.
  if (Object.prototype.hasOwnProperty.call(REASON_TEXT, reason)) return REASON_TEXT[reason];
  return stripUnsafeChars(reason);
}

/** An all-zero tail with complete coverage is what every normally finished run has: nothing
 *  to say, so the block is hidden. Anything else (tokens, or a partial coverage even with
 *  zero tokens) is shown. */
export function tailVisible(t: UsageTail | null | undefined): t is UsageTail {
  if (!t) return false;
  const tokens = t.input_tokens + t.cache_read_tokens + t.cache_creation_tokens + t.output_tokens;
  return tokens > 0 || t.coverage !== "complete";
}

/** "~$1.23 estimated", "<$0.01 estimated" or "cost unknown". A null cost is never rendered as $0. */
export function tailCostText(costUsd: number | null, status: string): string {
  if (status !== "estimated" || costUsd === null || !Number.isFinite(costUsd)) return "cost unknown";
  // A real but sub-half-cent cost must not round to "~$0.00", which reads as nothing spent.
  if (costUsd > 0 && costUsd < 0.005) return "<$0.01 estimated";
  return `~${formatCost(costUsd)} estimated`;
}
