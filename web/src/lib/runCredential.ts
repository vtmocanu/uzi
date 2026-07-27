// Rendering for WHICH Anthropic credential a run spent and WHY (PRD #111 M5, D20).
//
// Why this is a module and not three lines in the component: the mode is the answer
// to a question the label cannot reach. An auto pick and a default fallback can name
// the SAME token, so "console-key" says which account paid and leaves "why that
// account" unanswered — and PRD #104's compatibility path creates a row labelled
// literally `default`, so the label is not even a reliable hint at the mode.
//
// 🔴 EVERY STRING HERE RENDERS A SERVER ANSWER. Nothing in this file derives a mode,
// a headroom or an eligibility from anything else — the same rule lib/rateLimits.ts
// states for auto_status, for the same reason: the server's record is what actually
// spent the money, and a UI that reconstructed it would eventually disagree with it
// while looking authoritative.

import type { Run, SelectReason } from "./api";

export interface CredentialDescription {
  /** The mode phrase shown after the label, e.g. "auto, 62% headroom". Empty for a
   *  run claimed before PRD #111 M1, which recorded no mode — the bare label is the
   *  truthful rendering there and a guessed "default" would assert something nothing
   *  knows. */
  mode: string;
  /** The hover explanation. Always present, because the mode phrase is necessarily
   *  terse and "default (auto: no fresh usage readings)" is not self-explaining. */
  hint: string;
  /** How much attention the state deserves, and the rule behind it is the one the
   *  settings page already uses (web-ux F4): AMBER where the selector SKIPPED the
   *  token, INFO where it still picked one but the news is worth reading, NEUTRAL
   *  where nothing is unusual.
   *
   *  `best_of_pool` is info rather than neutral (F17) because the tone ordering was
   *  inverted without it: a run whose pool is nearly exhausted — the fixture is 8%
   *  headroom — rendered identically to a healthy `auto, 62% headroom`, while
   *  `pool_stale`, where the run completed exactly as configured-minus-auto, wore
   *  amber. Info also matches `low headroom` on the settings page, which is the same
   *  condition seen from the token side. */
  tone: "neutral" | "info" | "warning";
  /** Whether to link to the page where this can be acted on. ONE rule, so the two
   *  cannot drift: link iff the tone is not neutral. Nothing unusual, nowhere to go. */
  linked: boolean;
  /** True when the credential has since been deleted (web-ux F8). The id is a live
   *  FK the server nulls on delete (00086's SET NULL) while the snapshotted label
   *  survives, so this is the NORMAL shape of a historical run — the difference
   *  between "go look at this token" and "this token is gone". */
  deleted: boolean;
}

// REASON_PHRASES is EXHAUSTIVE over SelectReason by type, so a ninth reason
// server-side without a rendering here fails `npm run typecheck` rather than painting
// a blank chip. That coupling is the whole reason SelectReason is a union rather than
// a bare string — and it is also why the wire type is `SelectReason | string`: the
// exhaustiveness must bind this map, not lie about what the API can send.
//
// The three FALLBACK reasons are phrased as `default (auto: …)` rather than as their
// own mode, because that is what actually happened: the worker is configured for auto,
// the selector declined, and the owner's default paid. A user reading a bare "default"
// on a worker they set to auto would reasonably think the setting had been lost.
type Phrase = { mode: string; hint: string; tone?: "info" | "warning" };

const REASON_PHRASES: Record<SelectReason, Phrase> = {
  default: {
    mode: "default",
    hint: "No binding named a credential, so this run spent your default Anthropic token.",
  },
  pinned: {
    mode: "pinned",
    hint: "The worker that claimed this run is pinned to this token, so that is what it spent.",
  },
  judge: {
    // Its own word, not "pinned": a user told "pinned" goes looking at Settings →
    // Workers for a binding that does not exist. This choice was made by their judge
    // setting, on a different page.
    mode: "judge binding",
    hint: "Review runs follow your judge-lane token setting rather than the claiming worker's, so they can be billed separately.",
  },
  auto: {
    mode: "auto",
    hint: "Auto-selection picked this token from your pool as the one with the most rate-limit headroom.",
  },
  best_of_pool: {
    // Named rather than folded into `auto`: every pooled token was under the headroom
    // floor and the emptiest was picked anyway. The run worked and the pool is nearly
    // exhausted, which is a thing to know rather than an error — hence info, not
    // neutral and not amber (F17).
    mode: "auto (best of pool)",
    hint: "Every token in your pool was below the headroom threshold, so auto-selection spent the least-consumed of them rather than falling back. Your pool is nearly exhausted.",
    tone: "info",
  },
  pool_empty: {
    // Pure configuration, and fixed in one click on the page this links to.
    mode: "default (auto: no tokens in the pool)",
    hint: "This worker is set to auto, but no token is opted into the pool, so the run spent your default token. Opt one in on Settings → Anthropic tokens.",
    tone: "warning",
  },
  pool_stale: {
    // HEDGED, per F16. This can be the user's half (tokens uzi has never managed to
    // poll) or the system's (the poller is disabled — R2), and the copy cannot tell
    // which from here. The previous wording asserted the user's half by sending them
    // to the eligibility chips, which is wrong advice half the time.
    mode: "default (auto: no fresh usage readings)",
    hint: "This worker is set to auto, but no pooled token had a current usage reading to rank on, so the run spent your default token. Either uzi has not managed to read those tokens — the eligibility chips on Settings → Anthropic tokens will say — or usage polling is switched off for this instance.",
    tone: "warning",
  },
  open_failed: {
    // NOT a configuration problem (F16). A ciphertext would not decrypt, so the
    // action is to ROTATE that credential — inspecting eligibility chips tells the
    // user nothing, because the token was eligible; it was the OPEN that failed. The
    // rotate form is on the same page, so the link target is unchanged.
    mode: "default (auto: the chosen token would not open)",
    hint: "Auto-selection picked a token and its stored value could not be decrypted, so the run spent your default token rather than failing. Re-paste that token on Settings → Anthropic tokens to fix it.",
    tone: "warning",
  },
};

/** SELECT_REASONS is the vocabulary AT RUNTIME, derived from the exhaustive Record
 *  above rather than hand-typed beside it.
 *
 *  🔴 THE DERIVATION IS THE GUARD, and a hand-written mirror was measurably not one.
 *  A `SelectReason` union is erased at runtime, so a cross-language test needs SOME
 *  runtime list — and the first version of this was a second literal array with
 *  `satisfies readonly SelectReason[]`. That constrains the array to CONTAIN only
 *  valid members and says nothing about it containing ALL of them, so a ninth reason
 *  added to the union and forgotten in the array left the migration guard green.
 *  Measured: mutating the union alone survived the whole suite.
 *
 *  Object.keys of a `Record<SelectReason, …>` cannot have that gap: the Record is
 *  exhaustive by typecheck, so the keys are exactly the union, and there is no second
 *  place to forget. */
export const SELECT_REASONS = Object.keys(REASON_PHRASES) as SelectReason[];

/** describeCredential turns a run's four credential fields into what the chip shows.
 *
 *  Takes the RUN rather than loose arguments so a caller cannot pass the headroom of
 *  one run beside the reason of another, and so adding a fifth field later is one
 *  signature change instead of every call site. */
export function describeCredential(
  run: Pick<
    Run,
    | "anthropic_secret_id"
    | "anthropic_secret_label"
    | "anthropic_select_reason"
    | "anthropic_headroom_pct"
  >,
): CredentialDescription {
  const deleted = run.anthropic_secret_id == null;
  const reason = run.anthropic_select_reason;
  if (reason == null || reason === "") {
    return {
      mode: "",
      hint: "The Anthropic credential this run's claim spent.",
      tone: "neutral",
      linked: false,
      deleted,
    };
  }
  const known = REASON_PHRASES[reason as SelectReason];
  if (!known) {
    // An UNRECOGNISED value renders as itself rather than being dropped or guessed at
    // — the API ships separately from this bundle, and inventing a rendering for a
    // mode this build has never heard of is the exact lie the whole mechanism exists
    // to prevent. Same stance as autoStatusChip.
    return {
      mode: reason,
      hint: `This uzi build does not recognise the selection mode “${reason}”. It is shown as-is rather than guessed at.`,
      tone: "neutral",
      linked: false,
      deleted,
    };
  }
  // The headroom rides only where the server measured one. It is the RAW headroom the
  // user's own meters show, never the in-flight-penalised rank the selector ordered
  // on, which appears nowhere else in the product.
  const pct = run.anthropic_headroom_pct;
  const mode = pct == null ? known.mode : `${known.mode}, ${pct}% headroom`;
  const tone = known.tone ?? "neutral";
  return { mode, hint: known.hint, tone, linked: tone !== "neutral", deleted };
}
