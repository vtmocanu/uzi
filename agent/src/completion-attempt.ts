import type { RunContext } from "./executor.js";
import type { Milestone } from "./protocol.js";
import { COMPLETION_CONTRACT_GUIDANCE, renderFollowUpBlock } from "./prompt.js";

// Shared with the lead-response stall detector. The first identical completion
// attempt counts as streak 1; a changed fingerprint starts a new streak.
export const STALL_LIMIT = 3;

export type CompletionQuestionTrigger =
  | { kind: "lead_request"; reason: string }
  | { kind: "no_progress" };

// Static, content-free reasons safe to persist on the legacy throw fallback.
export const REASON_COMPLETION_LEAD_REQUESTED =
  "completion blocked: the lead requested an owner decision while the frozen completion contract still has unmet milestones. Held for an owner decision rather than shipping an incomplete run.";
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

/** Request a completion hold after an attempt; true includes unresolved delivery retained by the runner. */
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

/** Worker-authored rework; an answered question requires a fresh server check, even without prose. */
export function buildCompletionReworkFollowUp(
  unmet: string[],
  frozen?: Milestone[] | null,
  answer?: { kind: "answered"; guidance?: string },
  ownerDecisionEligible = false,
): string {
  if (answer?.kind === "answered") {
    return [
      "The run owner answered the completion question. The frozen completion scope may have been revised.",
      "Do not infer a formal decision from the owner text or require the previous unmet milestone list.",
      ...renderFollowUpBlock(answer.guidance ?? ""),
      "",
      "Worker instruction: call `signal_done` again for a server-authoritative completion recheck.",
      "The worker and server decide whether completion and publication are eligible.",
    ].join("\n");
  }
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
    ...(ownerDecisionEligible ? [
      "If any criterion needs an owner decision, use the owner-decision request below.",
      "Otherwise finish the required work and call `signal_done` for a server-authoritative recheck.",
    ] : [
      "Do not signal done again until every frozen milestone above is complete.",
    ]),
  ].join("\n");
  return ownerDecisionEligible ? `${base}\n\n${COMPLETION_CONTRACT_GUIDANCE}` : base;
}
