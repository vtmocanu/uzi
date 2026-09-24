/**
 * Issue #1593: recovery for a gated planning turn that ends with PROSE ONLY — no
 * submit_plan, no ask_user, just lead text. Before this, both executors failed the run
 * outright ("codex plan turn produced no plan" / REASON_NO_PLAN), even though the lead
 * had usually written something a human could act on.
 *
 * The flow is shared by the Codex and Claude executors, per planning-turn invocation
 * (the initial plan turn and every revision turn each get their own budget):
 *
 *  1. First prose-only turn → resume the same session with the FIXED
 *     {@link PLAN_MISSING_NUDGE}. Never interpolated with model text.
 *  2. Second prose-only turn → {@link resolvePlanMissing}: a status card carrying the
 *     lead's bounded, redacted final message as UNTRUSTED DATA, then a worker-authored
 *     park with a fixed question. The owner's guidance resumes planning; a cancel ends
 *     the run; no human (unwired or autopilot) fails with {@link REASON_PLAN_MISSING}.
 *  3. A prose-only turn after that guidance → the status card again, then
 *     {@link REASON_PLAN_MISSING}. No second nudge, no second park.
 *
 * Two invariants hold throughout: nothing is ever DERIVED from the prose (it never becomes
 * a plan, a question or part of a prompt), and an answer is never plan approval — any plan
 * still goes to the approval gate.
 */

import type { RunContext } from "./executor.js";
import { emptyCounts, sanitizeText } from "./sanitize.js";

/** The corrective prompt for the first prose-only turn. Fixed text, never model-derived. */
export const PLAN_MISSING_NUDGE =
  "Your planning turn ended without a structured signal. Submit your implementation plan with submit_plan. If an owner decision is needed first, ask it with ask_user. Do not end the turn with prose only.";

/** The worker-authored park's question header and body. Fixed: the lead's prose is shown
 *  in the feed (the status card), never in the question. */
export const PLAN_MISSING_QUESTION_HEADER = "Plan missing";
export const PLAN_MISSING_QUESTION =
  "The planning turn ended without a plan or a structured question. Review the lead's last message (shown in the feed) and reply with guidance to resume planning, or cancel the run.";

/** The status card's text. The lead's message rides beside it as a separate payload field. */
const PLAN_MISSING_NOTICE =
  "The planning turn ended without a plan or a structured question after a corrective nudge. The lead's last message is attached as untrusted data.";

/** The failure_reason, verbatim. Static and content-free, so safe to persist; the runner
 *  maps it to fail_origin `plan_missing` by exact match. */
export const REASON_PLAN_MISSING =
  "the planning turn ended without a plan or a structured question after a corrective nudge";

/** The cap on the lead's final message as carried on the status card, marker included. */
export const MAX_LEAD_FINAL_MESSAGE_LEN = 4000;

const TRUNCATED_MARKER = "…[truncated]";

// C0 controls except \t (U+0009) and \n (U+000A), DEL, the C1 block, and every Unicode
// format character (Cf: bidi embeddings/overrides/isolates, zero-width marks, BOM). The
// message is rendered to a human as data; none of these belong in it, and the bidi class
// can make it read differently from what it says.
// Matching control characters is the whole job of this pattern.
// eslint-disable-next-line no-control-regex
const CONTROL_OR_FORMAT = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]|\p{Cf}/gu;

/** The shape both executors' turn results share for the prose-only test. */
interface PlanTurnSignals {
  plan?: string;
  questions?: unknown[];
  finalText?: string;
}

/** True when a planning turn produced neither a plan nor a question but DID end with lead
 *  text. A turn with no text at all is not prose-only and keeps its executor's existing
 *  failure. */
export function isProseOnlyPlanTurn(t: PlanTurnSignals): boolean {
  return !t.plan?.trim() && !t.questions?.length && !!t.finalText?.trim();
}

/**
 * Bound the lead's final message for the status card. Order matters: redact FIRST, so a
 * secret straddling the cap is replaced whole rather than leaving an unredacted prefix;
 * then strip NUL/unpaired surrogates and the control/format classes; THEN cap, marker
 * included, without splitting a surrogate pair.
 */
export function boundLeadFinalMessage(text: string, redactText?: (s: string) => string): string {
  const redacted = redactText ? redactText(text) : text;
  const clean = sanitizeText(redacted, emptyCounts()).replace(CONTROL_OR_FORMAT, "");
  if (clean.length <= MAX_LEAD_FINAL_MESSAGE_LEN) return clean;
  let cut = MAX_LEAD_FINAL_MESSAGE_LEN - TRUNCATED_MARKER.length;
  const last = clean.charCodeAt(cut - 1);
  if (last >= 0xd800 && last <= 0xdbff) cut -= 1;
  return clean.slice(0, cut) + TRUNCATED_MARKER;
}

/** The prompt that resumes planning on the owner's guidance. Carries the OWNER's text
 *  only — never the lead's prose or the park's question. */
export function buildPlanMissingGuidancePrompt(guidance: string): string {
  const g = guidance.trim() ? guidance : "(no guidance given)";
  return (
    `The run owner reviewed your last message and gave this guidance:\n\n${g}\n\n` +
    "This guidance is not plan approval. Produce your implementation plan and submit it with submit_plan; " +
    "it will go to the approval gate. If you still need an owner decision, ask it with ask_user. Do not begin implementing."
  );
}

/** Emit the plan-missing status card: the fixed notice plus the lead's bounded final
 *  message as a separate, untrusted payload field. */
export function emitPlanMissingNotice(ctx: RunContext, finalText: string): void {
  ctx.emit({
    kind: "status",
    agent: "worker",
    payload: {
      event: "plan_missing",
      text: PLAN_MISSING_NOTICE,
      lead_final_message: boundLeadFinalMessage(finalText, ctx.redactText),
    },
  });
}

/**
 * The fallback after a nudged turn still ended in prose: surface the status card, then park
 * on the worker-authored question. Throws {@link REASON_PLAN_MISSING} when no human can
 * answer (unwired or autopilot); the caller maps `cancel` to its own cancel reason.
 */
export async function resolvePlanMissing(
  ctx: RunContext,
  finalText: string,
): Promise<{ kind: "guidance"; prompt: string } | { kind: "cancel" }> {
  emitPlanMissingNotice(ctx, finalText);
  if (!ctx.askPlanMissing) throw new Error(REASON_PLAN_MISSING);
  const v = await ctx.askPlanMissing();
  if (v.kind === "unattended") throw new Error(REASON_PLAN_MISSING);
  if (v.kind === "cancel") return { kind: "cancel" };
  ctx.emit({ kind: "answer", agent: "worker", payload: { answers: v.answers } });
  return { kind: "guidance", prompt: buildPlanMissingGuidancePrompt(v.answers.join("\n")) };
}
