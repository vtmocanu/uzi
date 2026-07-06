// Pure, framework-free ci_fix verdict-chip logic (PRD #6). Kept out of RunView so
// the mapping is unit-tested in isolation, the same split runBadge/pipelineBadge use.

import type { FixVerdict } from "./api";
import type { BadgeTone } from "./runBadge";

export interface FixVerdictChip {
  label: string;
  tone: BadgeTone;
  title: string;
}

/**
 * fixVerdictChip maps a ci_fix run's verdict to its run-view header chip:
 *  - verified   → the fix MR's latest pipeline passed (ok)
 *  - fix_failed → it failed (danger)
 *  - not_code   → the agent judged the failure not a code problem (neutral)
 *  - null on a TERMINAL run → "unverified" (its post-fix pipeline has not concluded,
 *    or the fix branch aged out of the watch window) — honest, not fabricated.
 *  - null on a non-terminal run → no chip yet (the run is still working).
 */
export function fixVerdictChip(verdict: FixVerdict | null, terminal: boolean): FixVerdictChip | null {
  switch (verdict) {
    case "verified":
      return { label: "verified ✓", tone: "ok", title: "The fix MR's latest pipeline passed" };
    case "fix_failed":
      return { label: "fix failed ✗", tone: "danger", title: "The fix MR's latest pipeline failed" };
    case "not_code":
      return {
        label: "not a code problem",
        tone: "neutral",
        title: "The agent judged the failure not fixable by a code change; no MR was opened",
      };
    case null:
      return terminal
        ? { label: "unverified", tone: "warning", title: "No post-fix pipeline has concluded yet" }
        : null;
  }
}
