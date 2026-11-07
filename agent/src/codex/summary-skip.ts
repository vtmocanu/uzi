// Issue #3248 M3: the run-feed note for a Codex intent or plan summary skipped because the run's
// Codex credential could not be used. Leaf module: the eight fixed sentences, one per
// (pass, reason), so no provider or model text can reach the feed. The reason is decided by the
// model pass from typed evidence (model-pass.ts ModelPassCredentialError), never from message text.

import type { SummarySkipReason } from "../summary-runner.js"; // type-only: erased at runtime

export type SummarySkipPass = "intent" | "plan";

const REASON_TEXT: Record<SummarySkipReason, string> = {
  refresh_denied: "the run's Codex credential needed a refresh, which summaries may not do.",
  vault_locked: "the run owner's vault is locked.",
  account_unavailable: "the run's Codex account is unavailable.",
  credential_rejected: "the run's Codex credential could not be used.",
};

const PASS_PREFIX: Record<SummarySkipPass, string> = {
  intent: "Intent summary skipped:",
  plan: "Plan summary skipped:",
};

/** The `status` payload for one skipped summary: fixed text plus the closed machine fields. */
export function summarySkipPayload(pass: SummarySkipPass, reason: SummarySkipReason): {
  text: string; event: "summary_skipped"; pass: SummarySkipPass; reason: SummarySkipReason;
} {
  return { text: `${PASS_PREFIX[pass]} ${REASON_TEXT[reason]} The run continues.`, event: "summary_skipped", pass, reason };
}
