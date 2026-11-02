// The Codex per-child turn budget: the one authoritative default, its resolution, the
// compact label shown to the model and the run feed, and the two prompt appends that tell
// the Codex lead and its children the effective budget. Dependency-free on purpose: the
// shared prompt module imports it, so it must not pull in the executor or delegation code.

/** Default per-child wall-clock turn deadline (ms). The single source for the delegation
 *  runner and the executor. */
export const DEFAULT_CHILD_TURN_DEADLINE_MS = 15 * 60 * 1000;

/** The effective child turn deadline: the caller's override when given, else the default. */
export function resolveChildTurnDeadlineMs(override?: number): number {
  return override ?? DEFAULT_CHILD_TURN_DEADLINE_MS;
}

/** A compact, bounded label (at most 12 characters) for a child turn budget:
 *  `15-minute`, `45-second` or `30 ms`. Invalid or huge values collapse to a fixed form. */
export function formatChildTurnBudget(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "unknown";
  if (ms % 60_000 === 0 && ms / 60_000 <= 9999) return `${ms / 60_000}-minute`;
  if (ms % 1000 === 0 && ms / 1000 <= 99_999) return `${ms / 1000}-second`;
  if (ms <= 999_999_999) return `${Math.ceil(ms)} ms`;
  return "very long";
}

/** Lead guidance: what the per-child budget is and how to react to a `child_timeout`. */
export function codexLeadChildBudgetAppend(ms: number): string {
  const budget = formatChildTurnBudget(ms);
  return [
    "Child turn budget:",
    `- Each spawn_agent child has a ${budget} per-child turn budget. Child setup and command execution consume it; it is not the 60-minute command bound. Owner cancel, the end of your turn or the run wall may stop a child earlier.`,
    "- Size each assignment to finish within it.",
    "- After a child_timeout, inventory partial work (worktree status and diff, the child's reported artifacts) before choosing a smaller executable reassignment or a deliberate takeover. Do not blindly replay edits and do not retry automatically.",
    "- Lead-authored changes still need independent committed-range review.",
  ].join("\n");
}

/** Child guidance: the budget it runs under and the truthful partial-handoff rule. */
export function codexChildBudgetAppend(ms: number): string {
  const budget = formatChildTurnBudget(ms);
  return [
    "Turn budget:",
    `- Your turn has a ${budget} budget, consumed by setup and command execution; it is not the command bound. Cancel or the run wall may stop it earlier.`,
    "- Do not start validation you cannot reasonably finish within it. Instead return a truthful partial handoff: changed files, checks actually run with results, remaining work.",
    "- A partial handoff is not a passed check or a completed milestone.",
    "- The budget never waives a required check or permits background work.",
  ].join("\n");
}
