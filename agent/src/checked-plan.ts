// The checked plan-gate contract both lead executors (sdk-executor.ts, codex/codex-executor.ts)
// consume: the validated server bundle of an APPROVED cross-check, the implement-prompt context
// that carries it, and the bound on automatic revision rounds. One copy, so the two harnesses
// cannot drift on what "exactly the approved candidate" means.

import type { Milestone } from "./protocol.js";
import { TrustedExecutionRefusal } from "./trusted-execution-refusal.js";

/** The most automatic (checker-requested) revision rounds a plan gate accepts (PRD #2150). */
const MAX_AUTOMATIC_ROUND = 4;

/**
 * Validate the whole checked-approval bundle before adopting any of it. The digest is an opaque
 * server SHA-256 identity, not a locally recomputed proof of approval; the bundle must belong to
 * the claim generation this execution holds. Throws TrustedExecutionRefusal on any defect.
 */
export function validateCheckedPlanBundle(
  canonical: { plan: string; milestones: Milestone[]; candidate_digest: string; claimGeneration: number } | undefined,
  executionGeneration: number | undefined,
): { plan: string; milestones: Milestone[] } {
  if (
    !canonical || typeof canonical !== "object" ||
    typeof canonical.plan !== "string" || !canonical.plan.trim() ||
    !Array.isArray(canonical.milestones) ||
    !Array.from(canonical.milestones).every((m) =>
      m !== null && typeof m === "object" &&
      typeof m.id === "string" && m.id.trim().length > 0 &&
      typeof m.title === "string" && m.title.trim().length > 0
    ) ||
    typeof canonical.candidate_digest !== "string" ||
    canonical.candidate_digest.length !== 64 ||
    !/^[0-9a-f]{64}$/.test(canonical.candidate_digest) ||
    !Number.isSafeInteger(canonical.claimGeneration) || canonical.claimGeneration <= 0 ||
    !Number.isSafeInteger(executionGeneration) || (executionGeneration ?? 0) <= 0 ||
    canonical.claimGeneration !== executionGeneration
  ) throw new TrustedExecutionRefusal("invalid checked plan approval bundle");
  // Server values verbatim, including nested keys and explicit [].
  return { plan: canonical.plan, milestones: canonical.milestones };
}

/** The implement-prompt prefix that repeats the approved server contract on every attempt: an
 *  interrupted first attempt may not have delivered it to the model or updated the old session.
 *  The entire approved milestone list is serialized, including nested values and explicit []. */
export function checkedImplementationContext(plan: string, milestones: unknown): string {
  return `The following server contract is explicitly approved for implementation and supersedes the local plan in this session. Follow its prose and full milestone contract.\n\n${plan}\n\n<approved_milestone_contract>\n${JSON.stringify(milestones)}\n</approved_milestone_contract>\n\n`;
}

/** Validate one automatic revision round: strictly increasing and within the bound. Returns it. */
export function nextAutomaticRound(round: number, previous: number): number {
  if (!Number.isInteger(round) || round <= previous || round > MAX_AUTOMATIC_ROUND)
    throw new TrustedExecutionRefusal("invalid automatic plan revision round");
  return round;
}
