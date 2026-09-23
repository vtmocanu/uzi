// PRD #1332 (M5A, C4b) — the versioned Codex Standard price table and per-response pricing (D5).
//
// This module is a PURE, dated input: it prices ONE upstream response's usage breakdown (`last`)
// against a pinned per-model rate table and returns a dollar figure, or `undefined` when the
// response is not confidently priceable (unknown model, an impossible cache split, or the Sol
// promotional review boundary has passed). It NEVER decides token totals (those come from the
// cumulative `total` deltas the {@link CodexUsageAccountant} reconciles) and it holds NO state.
//
// SERVICE TIER (D5): the >272K threshold, the four bucket rates and the whole table assume the
// OpenAI **Standard** service tier as an ESTIMATION POLICY, not an observed fact. Uzi sets no
// `service_tier`, and in pinned 0.156.1 a catalog `default_service_tier` (gpt-6-sol lists `priority`)
// is applied only by the interactive TUI, never by app-server request building, so the request
// carries none. OpenAI then processes it at the API PROJECT's configured tier, which uzi cannot
// see: a project set to priority is billed above these rates. The pinned 0.156.1
// app-server `thread/tokenUsage/updated` notification (source commit
// b412ff32c417f855c2b2d1581b77058eed87c84b, `codex-rs/app-server-protocol/src/protocol/v2/
// thread.rs`, unchanged since 0.153.2) carries NO service-tier field on either `total` or `last`, so the tier is NOT
// observable here. Per D5's explicit fallback ("if the tier isn't observable in the pinned
// protocol, price as Standard only for the two known models"), we price the known models as
// Standard and leave every unknown model `unreported`. A future protocol that surfaces the tier
// must gate a non-Standard tier (Batch/Flex/Fast) to `unreported` here.

import type { CodexUsageBreakdown } from "./transport.js";

/**
 * The pinned price-table version. Recorded WITH the table (D5): the rates below were verified
 * against the official OpenAI API pricing table and model pages on 2026-09-13; the `gpt-6-sol` row was
 * added from its official model page on 2026-09-23. A re-verification
 * that changes any rate MUST bump this id and, for a promotional row, its review boundary.
 */
export const CODEX_PRICE_TABLE_VERSION = "openai-standard-2026-09-23";

/**
 * The per-response input-tier boundary (D5): the rates change for the WHOLE response when its
 * TOTAL input tokens EXCEED this value. "272K" is 272,000 tokens. The comparison is strict
 * (`inputTokens > threshold`): a response at exactly 272,000 input is still the LOW tier.
 */
export const CODEX_INPUT_TIER_THRESHOLD_TOKENS = 272_000;

/**
 * The `gpt-5.6-sol` promotional review boundary (D5), as a UTC calendar date. The official model
 * page states the promotional pricing is available at least THROUGH 2026-11-21; on or after this
 * date, `gpt-5.6-sol` api-key cost becomes `unreported` until a maintainer re-verifies and
 * re-versions the row. `gpt-6-astra` is unaffected.
 */
export const SOL_PROMO_REVIEW_DATE = "2026-11-21";

/** The four Standard bucket rates for one tier, in USD per 1,000,000 tokens. */
interface TierRates {
  readonly uncachedInput: number;
  readonly cachedInput: number;
  readonly cacheWrite: number;
  readonly output: number;
}

/** A model's Standard rates, split at {@link CODEX_INPUT_TIER_THRESHOLD_TOKENS}. `promoReviewDate`
 *  is set only for a row whose pricing is promotional and must fail closed to `unreported` once the
 *  review boundary passes. */
interface ModelRates {
  readonly low: TierRates;
  readonly high: TierRates;
  readonly promoReviewDate?: string;
}

/**
 * The `openai-standard-2026-09-23` table: PRD D5's `openai-standard-2026-09-13` rows plus `gpt-6-sol`
 * (official model page, 2026-09-23: >272K input is 2x input and cache, 1.5x output). USD / 1,000,000 tokens:
 *
 *   gpt-6-astra   input <= 272K:  uncached 10.00  cached 1.00  cacheWrite 12.50  output 50.00
 *   gpt-6-astra   input  > 272K:  uncached 20.00  cached 2.00  cacheWrite 25.00  output 75.00
 *   gpt-5.6-sol   input <= 272K:  uncached  4.00  cached 0.40  cacheWrite  5.00  output 20.00
 *   gpt-5.6-sol   input  > 272K:  uncached  8.00  cached 0.80  cacheWrite 10.00  output 30.00
 *   gpt-6-sol     input <= 272K:  uncached  2.00  cached 0.20  cacheWrite  2.50  output 10.00
 *   gpt-6-sol     input  > 272K:  uncached  4.00  cached 0.40  cacheWrite  5.00  output 15.00
 */
const TABLE: Readonly<Record<string, ModelRates>> = {
  "gpt-6-astra": {
    low: { uncachedInput: 10.0, cachedInput: 1.0, cacheWrite: 12.5, output: 50.0 },
    high: { uncachedInput: 20.0, cachedInput: 2.0, cacheWrite: 25.0, output: 75.0 },
  },
  "gpt-5.6-sol": {
    low: { uncachedInput: 4.0, cachedInput: 0.4, cacheWrite: 5.0, output: 20.0 },
    high: { uncachedInput: 8.0, cachedInput: 0.8, cacheWrite: 10.0, output: 30.0 },
    promoReviewDate: SOL_PROMO_REVIEW_DATE,
  },
  "gpt-6-sol": {
    low: { uncachedInput: 2.0, cachedInput: 0.2, cacheWrite: 2.5, output: 10.0 },
    high: { uncachedInput: 4.0, cachedInput: 0.4, cacheWrite: 5.0, output: 15.0 },
  },
};

/** True once `now` is on or after the UTC calendar date `reviewDate` (`YYYY-MM-DD`). The boundary
 *  is that date's UTC midnight, so any instant on the review date itself is on-or-after it. */
function promoReviewPassed(reviewDate: string, now: Date): boolean {
  const boundaryMs = Date.parse(`${reviewDate}T00:00:00Z`);
  return Number.isFinite(boundaryMs) && now.getTime() >= boundaryMs;
}

/**
 * Price ONE upstream response's usage breakdown (`last`) against the pinned Standard table (D5).
 * Returns the dollar cost, or `undefined` when the response is NOT confidently priceable:
 *   - the model is not in the table (unknown model → `unreported`);
 *   - the model's promotional review boundary has passed at `now` (Sol on/after 2026-11-21);
 *   - the two cache detail buckets exceed the total input (`cached + cacheWrite > input`), an
 *     impossible/hostile split that makes uncached input undefined.
 * The `> 272K` tier is decided on the response's TOTAL input tokens and changes ALL four rates for
 * that entire response. Output already INCLUDES reasoning — `outputTokens` is priced once and
 * `reasoningOutputTokens` is NEVER added again. `now` is an injected clock (never a bare
 * `new Date()`) so the Sol boundary is deterministic in tests.
 */
export function priceCodexResponse(model: string, last: CodexUsageBreakdown, now: Date): number | undefined {
  const rates = TABLE[model];
  if (rates === undefined) return undefined; // unknown model — never priced as Standard
  if (rates.promoReviewDate !== undefined && promoReviewPassed(rates.promoReviewDate, now)) {
    return undefined; // stale promotional row — fail closed until re-verified/re-versioned
  }
  const input = last.inputTokens;
  const cached = last.cachedInputTokens;
  const cacheWrite = last.cacheWriteInputTokens;
  // FIRST validate the cache split (D5): the two detail buckets are subsets of the total input.
  // If they exceed it, this response is not priceable rather than fabricating a negative uncached.
  if (cached + cacheWrite > input) return undefined;
  const uncached = Math.max(input - cached - cacheWrite, 0);
  const tier = input > CODEX_INPUT_TIER_THRESHOLD_TOKENS ? rates.high : rates.low;
  const usd =
    (uncached * tier.uncachedInput +
      cached * tier.cachedInput +
      cacheWrite * tier.cacheWrite +
      last.outputTokens * tier.output) /
    1_000_000;
  return usd;
}
