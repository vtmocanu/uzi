import type { AnswerVerdict } from "./steering.js";
import type { RunContext } from "./executor.js";

export function parkRunEligible(ctx: RunContext, phase: "plan" | "implement"): boolean {
  return phase === "implement" && (ctx.kind ?? "issue") === "issue" && !ctx.interactive;
}

export function parkRunEnabled(ctx: RunContext, phase: "plan" | "implement"): boolean {
  return parkRunEligible(ctx, phase) && ctx.parkRunSupported === true;
}

export const PARK_RUN_CONTINUATION = "Continue if these answers give you actionable work. If implementation must wait on a dependency and no actionable work remains, call park_run and end your turn.";

/** Accept only owner answers already validated and consumed by the steering channel. */
export function isParkAnswer(verdict: AnswerVerdict): boolean {
  if (verdict.kind !== "answer") return false;
  const answers = verdict.answers.map((answer) => answer.trim()).filter(Boolean);
  return answers.length > 0 && answers.every((answer) => answer === "/park");
}
