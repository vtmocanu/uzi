import type { RunContext } from "./executor.js";
import type { Milestone } from "./protocol.js";

// Shared with the lead-response stall detector. The first identical completion
// attempt counts as streak 1; a changed fingerprint starts a new streak.
export const STALL_LIMIT = 3;

// Static, content-free reasons safe to persist on the legacy throw fallback.
export const REASON_COMPLETION_NO_PROGRESS =
  "completion blocked: the lead declared the run complete but the frozen completion contract still has unmet milestones, and repeated completion attempts made no progress (the same unmet set, branch head and worktree across attempts). Held for an owner decision rather than shipping an incomplete run.";
export const REASON_COMPLETION_BUDGET_EXHAUSTED =
  "completion budget exhausted: the server flagged this run past its completion budget after a completion attempt. Held for an owner decision rather than continuing to spend budget.";

/** Sort unmet ids without changing the server-provided list used in feedback. */
export function completionAttemptFingerprint(
  unmet: string[],
  head: string | null,
  worktreeFingerprint: string | null,
): string {
  return JSON.stringify([[...unmet].sort(), head, worktreeFingerprint]);
}

/** An unchanged consecutive attempt advances the streak; progress restarts at one. */
export function updateCompletionStreak(
  fingerprint: string,
  lastFingerprint: string | undefined,
  streak: number,
): number {
  return lastFingerprint !== undefined && fingerprint === lastFingerprint
    ? streak + 1
    : 1;
}

/** Enter a verified completion hold only after an attempt has been recorded. */
export async function routeCompletionHold(
  ctx: RunContext,
  reason: string,
  attempted: boolean,
): Promise<boolean> {
  if (attempted && ctx.enterCompletionHold) {
    return await ctx.enterCompletionHold(reason);
  }
  return false;
}

/** Same-session rework text for unmet frozen milestones and optional owner direction. */
export function buildCompletionReworkFollowUp(
  unmet: string[],
  frozen?: Milestone[] | null,
  ownerGuidance?: string,
): string {
  const titleFor = (id: string): string | undefined =>
    frozen?.find((m) => m.id === id)?.title;
  const lines = unmet.map((id) => {
    const t = titleFor(id);
    return t ? `- ${id}: ${t}` : `- ${id}`;
  });
  const base = [
    "Completion check (structural interlock): you signalled done, but the frozen completion",
    "contract still has milestone(s) NOT declared complete:",
    "",
    ...lines,
    "",
    "Finish the remaining milestone(s) and declare each complete (report_progress / signal_done),",
    "or record an explicit decision if one genuinely cannot be completed. Do not signal done again",
    "until every frozen milestone above is complete — an incomplete run cannot open its closing PR.",
  ].join("\n");
  const guidance = ownerGuidance?.trim();
  if (!guidance) return base;
  return [
    base,
    "",
    "The run owner reviewed this completion block and chose to CONTINUE, with direction:",
    "",
    guidance,
  ].join("\n");
}
