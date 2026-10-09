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
// `service_tier`, and in the pinned Codex version a catalog `default_service_tier` (gpt-6-sol lists `priority`)
// is applied only by the interactive TUI, never by app-server request building, so the request
// carries none. OpenAI then processes it at the API PROJECT's configured tier, which uzi cannot
// see: a project set to priority is billed above these rates. The pinned Codex version
// app-server `thread/tokenUsage/updated` notification (source commit
// 01fc69f4026735edfdf6789820549727a4867b11, `codex-rs/app-server-protocol/src/protocol/v2/
// thread.rs`, as exposed by the pinned Codex version) carries NO service-tier field on either `total` or `last`, so the tier is NOT
// observable here. Per D5's explicit fallback ("if the tier isn't observable in the pinned
// protocol, price as Standard only for the two known models"), we price the known models as
// Standard and leave every unknown model `unreported`. A future protocol that surfaces the tier
// must gate a non-Standard tier (Batch/Flex/Fast) to `unreported` here.

import { readFileSync } from "node:fs";
import type { CodexUsageBreakdown } from "./transport.js";
import table from "./codex-pricing.json" with { type: "json" };
import { validateCodexPricing, validateCodexPricingBytes } from "./codex-pricing-validation.js";

validateCodexPricingBytes(readFileSync(new URL("./codex-pricing.json", import.meta.url)));
const validatedTable = validateCodexPricing(table);
const TABLE = validatedTable.models;

/**
 * The pinned price-table version. Recorded WITH the table (D5): the rates below were verified
 * against the official OpenAI API pricing table and model pages on 2026-09-13; the `gpt-6-sol` row was
 * added from its official model page on 2026-09-23. The GPT-6.1 Sol row was added
 * from https://developers.openai.com/api/docs/models/gpt-6.1-sol on 2026-10-01. A re-verification
 * that changes any rate MUST bump this id and, for a promotional row, its review boundary.
 */
export const CODEX_PRICE_TABLE_VERSION = validatedTable.version;

/**
 * The per-response input-tier boundary (D5): the rates change for the WHOLE response when its
 * TOTAL input tokens EXCEED this value. "272K" is 272,000 tokens. The comparison is strict
 * (`inputTokens > threshold`): a response at exactly 272,000 input is still the LOW tier.
 */
export const CODEX_INPUT_TIER_THRESHOLD_TOKENS = validatedTable.input_tier_threshold_tokens;

/**
 * The `gpt-5.6-sol` promotional review boundary (D5), as a UTC calendar date. The official model
 * page states the promotional pricing is available at least THROUGH 2026-11-21; on or after this
 * date, `gpt-5.6-sol` api-key cost becomes `unreported` until a maintainer re-verifies and
 * re-versions the row. `gpt-6-astra` is unaffected.
 */
export const SOL_PROMO_REVIEW_DATE = TABLE["gpt-5.6-sol"]?.promo_review_date;

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
  const rates = Object.hasOwn(TABLE, model) ? TABLE[model] : undefined;
  if (rates === undefined) return undefined; // unknown model — never priced as Standard
  if (rates.promo_review_date !== undefined && promoReviewPassed(rates.promo_review_date, now)) {
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
    (uncached * tier.uncached_input +
      cached * tier.cached_input +
      cacheWrite * tier.cache_write +
      last.outputTokens * tier.output) /
    1_000_000;
  return usd;
}
