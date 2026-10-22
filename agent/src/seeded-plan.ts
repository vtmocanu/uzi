import type { RunContext } from "./executor.js";

/**
 * Issue #3272: the one trusted predicate for "the run's owner supplied this plan at
 * creation". It reads only worker/server-decided claim fields, never issue, repo, plan
 * or follow-up text. A seeded run with a blank plan, or one not marked plan-approved,
 * goes through the plan gate as before and is not covered.
 */
export function isSeededOwnerPlan(ctx: Pick<RunContext, "seeded" | "planApproved" | "approvedPlan">): boolean {
  return ctx.seeded === true && ctx.planApproved === true && !!ctx.approvedPlan?.trim();
}

/**
 * Issue #3272: the single wording shared by the seeded lead system-prompt variant and the
 * Codex seeded turn framing. It never says the plan was approved (PRD #209 Decision A).
 */
export const SEEDED_PLAN_RULES = [
  "The owner of this run supplied this plan when they created the run, and this run has no uzi plan gate.",
  "`save_draft_plan`, plan-validator waves and `submit_plan` do not apply, and the skipped plan gate is not a",
  "dependency, so it is never a reason to call `park_run`. Review or approval status that the plan states about",
  "itself (for example \"still under review\" or \"awaiting approval\") is not by itself a precondition this run",
  "waits on. An explicit instruction in the task (for example \"obtain external review before changing code\") is a",
  "real requirement: fulfil it, or clarify it with `ask_user`, and never silently waive it. Never claim that a",
  "review or approval happened.",
].join(" ");
