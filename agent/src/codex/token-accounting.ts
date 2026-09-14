// PRD #1332 (M5A, C4a) — Codex per-thread/per-model token accounting.
//
// The production Codex registry retains NO model identity and drops the app-server's
// `thread/tokenUsage/updated` notifications as generic activity, so a Codex run emits no
// per-model usage. This module is the missing accounting spine: an IMMUTABLE
// `threadId -> configured model` map plus a per-thread cumulative-snapshot reconciler that
// the {@link CodexHarness} feeds every typed usage notification (root AND demuxed child) and
// reads at each turn terminal to build the result-frame `modelUsage`.
//
// RECONCILIATION (D5), stated as invariants the tests pin:
//   - `total` is the thread's CUMULATIVE usage; `last` is the single most-recent response.
//     Token totals are derived from `total` (dedup/recovery-safe), NEVER by summing `last`.
//   - Each thread has a STARTING SNAPSHOT (baseline). A FRESH thread (thread/start) starts at
//     zero, so its whole cumulative is this leg's work and a MISSED intermediate update is
//     recovered by the later cumulative. A RESUMED thread (thread/resume) baselines at the FULL
//     restored cumulative (`total`) observed on its FIRST note — NOT `total - last`. The pinned
//     app-server replays an initial `thread/tokenUsage/updated` on resume whose `last` is the
//     PRIOR leg's final response (non-zero, a subset already in `total`; verified at commit
//     657a993… — token_usage_replay.rs + protocol.rs append_last_usage), so a `total - last`
//     baseline would fall below the prior cumulative and DOUBLE-COUNT that final response on the
//     next note. Baselining at the full `total` charges only genuinely-new post-resume deltas and
//     can never inflate (D5). Only post-baseline deltas are charged, so a resumed leg never
//     re-reports the prior leg's usage.
//   - A DUPLICATE, STALE or OUT-OF-ORDER note cannot increase usage: a note is adopted only
//     when its cumulative MAGNITUDE strictly exceeds the running max, and it then replaces the
//     whole breakdown (so a stale note cannot inflate a single bucket).
//   - UNKNOWN / unregistered threads are dropped, NEVER attributed to the root model.
//   - Reasoning tokens are a subset of output and are carried, never added to output twice.
//
// COST (C4b, D5): each emitted entry now carries a closed `costStatus` marker and, ONLY for a
// `metered` entry, a numeric `costUSD`. There are three mutually-exclusive semantics, chosen by
// the RUN's per-run authMode plus the reconciliation and pricing outcome (see {@link
// CodexPricingContext} and {@link aggregateByModel}):
//   - subscription run  → every entry `subscription`, NO costUSD (no per-token charge exists);
//   - api-key run       → `metered` with the summed per-response price WHEN every observed
//                         response for the model priced AND reconciled; otherwise `unreported`;
//   - no pricing context (a token-only caller / the pre-C4b default) → every entry `unreported`.
// TOKEN TOTALS are unchanged from C4a — they always come from the cumulative `total` deltas, never
// from the per-response `last` sum — so a model that fails to price still RETAINS its tokens as an
// `unreported` entry rather than losing them.

import type { CodexThreadTokenUsage, CodexUsageBreakdown } from "./transport.js";
import type { HarnessCost } from "../harness.js";
import { priceCodexResponse } from "./codex-pricing.js";

/** The closed cost marker on an emitted Codex `modelUsage` entry (C4b, D5): `subscription` (a
 *  subscription run, no per-token charge), `metered` (a fully-priced api-key run, carries
 *  `costUSD`), or `unreported` (unknown/stale/inconsistent — tokens retained, no dollar figure). */
export type CodexCostStatus = "subscription" | "metered" | "unreported";

/**
 * One per-model entry in the result-frame `modelUsage` map, as emitted for a Codex run. The
 * token fields mirror the existing Claude `modelUsage` entry
 * (`inputTokens`/`outputTokens`/`cacheReadInputTokens`/`cacheCreationInputTokens`) so C1's
 * server fold (`api/internal/workersvc/usage_fold.go`) reads it UNCHANGED, EXTENDED with the
 * closed {@link CodexCostStatus} marker. `inputTokens` here is the UNCACHED input (Claude's
 * `input_tokens` semantics — non-overlapping with the two cache buckets), and
 * `reasoningOutputTokens` is a subset of `outputTokens` (never added to it). `costUSD` is present
 * ONLY on a `metered` entry (a fully-priced api-key run); it is ABSENT for `subscription` and
 * `unreported`, matching the server's `resultModelUsage` decode where a missing `costUSD` reads as
 * a numeric-zero placeholder that the closed `costStatus` distinguishes from a real $0.
 */
export interface CodexModelUsageEntry {
  readonly inputTokens: number;
  readonly outputTokens: number;
  readonly cacheReadInputTokens: number;
  readonly cacheCreationInputTokens: number;
  readonly reasoningOutputTokens: number;
  readonly costStatus: CodexCostStatus;
  readonly costUSD?: number;
}

/** The per-run pricing inputs {@link CodexUsageAccountant.aggregateByModel} needs to project a
 *  closed {@link CodexCostStatus} (C4b, D5). `authMode` is the RUN's immutable credential mode;
 *  `now` is an INJECTED clock (never a bare `new Date()` inside the accountant/table) so the Sol
 *  promotional review boundary is deterministic in tests. Omitting this context entirely keeps the
 *  pre-C4b behavior: every entry `unreported` with no `costUSD`. */
export interface CodexPricingContext {
  readonly authMode: "subscription" | "api_key";
  readonly now: Date;
}

/** A mutable per-bucket cumulative accumulator (the six wire buckets). */
interface Cumulative {
  inputTokens: number;
  cachedInputTokens: number;
  cacheWriteInputTokens: number;
  outputTokens: number;
  reasoningOutputTokens: number;
  totalTokens: number;
}

function zeroCumulative(): Cumulative {
  return {
    inputTokens: 0,
    cachedInputTokens: 0,
    cacheWriteInputTokens: 0,
    outputTokens: 0,
    reasoningOutputTokens: 0,
    totalTokens: 0,
  };
}

function toCumulative(b: CodexUsageBreakdown): Cumulative {
  return {
    inputTokens: b.inputTokens,
    cachedInputTokens: b.cachedInputTokens,
    cacheWriteInputTokens: b.cacheWriteInputTokens,
    outputTokens: b.outputTokens,
    reasoningOutputTokens: b.reasoningOutputTokens,
    totalTokens: b.totalTokens,
  };
}

/** A monotonic scalar for a cumulative breakdown, used to decide whether a note is NEWER than
 *  the running max (and so should be adopted). Uses the reported `totalTokens` but never trusts
 *  it below `inputTokens + outputTokens`, so a provider that under-reports/omits `totalTokens`
 *  still advances rather than freezing after the first note. */
function magnitude(b: CodexUsageBreakdown): number {
  return Math.max(b.totalTokens, b.inputTokens + b.outputTokens);
}

/** Per-bucket `max(a[k] - b[k], 0)` — a non-negative delta of two cumulatives. */
function diffClamp(a: Cumulative, b: Cumulative): Cumulative {
  return {
    inputTokens: Math.max(a.inputTokens - b.inputTokens, 0),
    cachedInputTokens: Math.max(a.cachedInputTokens - b.cachedInputTokens, 0),
    cacheWriteInputTokens: Math.max(a.cacheWriteInputTokens - b.cacheWriteInputTokens, 0),
    outputTokens: Math.max(a.outputTokens - b.outputTokens, 0),
    reasoningOutputTokens: Math.max(a.reasoningOutputTokens - b.reasoningOutputTokens, 0),
    totalTokens: Math.max(a.totalTokens - b.totalTokens, 0),
  };
}

function addInto(dst: Cumulative, src: Cumulative): void {
  dst.inputTokens += src.inputTokens;
  dst.cachedInputTokens += src.cachedInputTokens;
  dst.cacheWriteInputTokens += src.cacheWriteInputTokens;
  dst.outputTokens += src.outputTokens;
  dst.reasoningOutputTokens += src.reasoningOutputTokens;
  dst.totalTokens += src.totalTokens;
}

/** Per-bucket sum of the observed per-response `last` breakdowns of one thread. */
function sumResponses(responses: readonly CodexUsageBreakdown[]): Cumulative {
  const acc = zeroCumulative();
  for (const r of responses) addInto(acc, toCumulative(r));
  return acc;
}

/**
 * The D5 "usage replay ambiguity" reconciliation for ONE thread: do the observed per-response
 * `last` breakdowns account for the cumulative token delta this leg is charging? Cost is the SUM
 * of per-response prices, but token totals come from the cumulative `total` delta; if a response's
 * `tokenUsage/updated` note was dropped, the cumulative still moved by it while its `last` was
 * never observed, so `sum(last) != delta` on the priced buckets. Reconciling on the four PRICED
 * buckets (uncached input is derived from input/cached/cacheWrite, and output) is what lets a
 * missed response degrade the model to `unreported` while its tokens are retained. `totalTokens`
 * and `reasoningOutputTokens` are intentionally NOT reconciled: they never enter the price, and a
 * provider that under-reports `totalTokens` must not spuriously fail an otherwise-clean leg.
 */
function responsesReconcileDelta(responseSum: Cumulative, delta: Cumulative): boolean {
  return (
    responseSum.inputTokens === delta.inputTokens &&
    responseSum.cachedInputTokens === delta.cachedInputTokens &&
    responseSum.cacheWriteInputTokens === delta.cacheWriteInputTokens &&
    responseSum.outputTokens === delta.outputTokens
  );
}

/** The reconciliation state for one authorized thread. */
interface ThreadAccount {
  /** The IMMUTABLE configured model for this thread (root or child). */
  readonly model: string;
  /** True when the thread was resumed (thread/resume): it baselines at the pre-resume
   *  cumulative rather than zero, so prior-leg usage is not re-charged. */
  readonly resumed: boolean;
  /** The starting snapshot; established at the first observed note (undefined until then). */
  baseline?: Cumulative;
  /** The running per-bucket max of `total`; undefined until the first note. */
  maxTotal?: Cumulative;
  /** The magnitude of {@link maxTotal}, for the newer-than test. */
  maxMagnitude: number;
  /** The observed per-response `last` breakdowns of THIS leg, one per ADOPTED note — the pricing
   *  basis (C4b, D5). A note is recorded here on exactly the same gate that advances {@link
   *  maxTotal}, so a duplicate/stale/out-of-order note is neither charged nor priced twice. A
   *  RESUMED thread's FIRST note is the prior leg's replayed snapshot (its `last` is a prior
   *  response already counted), so it establishes the baseline WITHOUT being recorded here. */
  readonly responses: CodexUsageBreakdown[];
}

/**
 * Per-run Codex usage accountant (one per {@link CodexHarness} instance; it persists across
 * turns and is NEVER reset, because the root thread's cumulative spans turns and children from
 * earlier turns must stay aggregated). NOT thread-safe in the concurrency sense — the harness
 * feeds it from its single-consumer notification loop.
 */
export class CodexUsageAccountant {
  private readonly threads = new Map<string, ThreadAccount>();

  /** Register the IMMUTABLE `threadId -> model` mapping. The FIRST registration wins: a repeat
   *  call for a known thread is ignored, so a stray/duplicate registration cannot rebind a
   *  thread's model. `resumed` distinguishes a resumed root (baselines at the pre-resume
   *  cumulative) from a fresh root/child (baselines at zero). */
  registerThread(threadId: string, model: string, resumed: boolean): void {
    if (threadId.length === 0 || this.threads.has(threadId)) return;
    this.threads.set(threadId, { model, resumed, maxMagnitude: 0, responses: [] });
  }

  /**
   * Reconcile one `thread/tokenUsage/updated` notification. An UNKNOWN thread is dropped (never
   * attributed to the root). The first note establishes the baseline (zero for a fresh thread,
   * the full restored cumulative `total` for a resumed one — see the module header on why NOT
   * `total - last`); later notes advance the cumulative max ONLY when strictly newer, so
   * duplicates/stale/out-of-order notes cannot increase usage.
   */
  record(threadId: string, usage: CodexThreadTokenUsage): void {
    const acct = this.threads.get(threadId);
    if (acct === undefined) return; // unknown / unregistered thread — never attributed
    const total = toCumulative(usage.total);
    if (acct.maxTotal === undefined) {
      // A RESUMED thread baselines at the FULL restored cumulative (`total`) of its first
      // observed note — NOT `total - last`. VERIFIED against the pinned app-server (commit
      // 657a993…): on resume `thread_lifecycle.rs` replays an initial `thread/tokenUsage/updated`
      // carrying the restored `TokenUsageInfo` (`token_usage_replay.rs`
      // send_thread_token_usage_update_to_connection), whose `last` is the PRIOR leg's final
      // response (non-zero — `protocol.rs` append_last_usage sets `last_token_usage = last`), a
      // subset already counted in `total`. So a `total - last` baseline falls BELOW the prior
      // cumulative and DOUBLE-COUNTS that final response on the next note. Baselining at the full
      // `total` charges only genuinely-new post-resume deltas and can never inflate (D5). A fresh
      // thread starts at zero (its whole cumulative is this leg's work).
      acct.baseline = acct.resumed ? toCumulative(usage.total) : zeroCumulative();
      acct.maxTotal = total;
      acct.maxMagnitude = magnitude(usage.total);
      // Record the first note's `last` as a priced response ONLY for a FRESH thread (its whole
      // cumulative is this leg's work). A RESUMED thread's first note replays the prior leg's
      // final response into `last`; it is already counted upstream and sits below the baseline, so
      // recording it would over-count cost and break reconciliation — it is deliberately skipped.
      if (!acct.resumed) acct.responses.push(usage.last);
      return;
    }
    const m = magnitude(usage.total);
    if (m > acct.maxMagnitude) {
      acct.maxTotal = total;
      acct.maxMagnitude = m;
      // A genuinely newer note: `last` is this note's single most-recent response — record it as a
      // priced response. Mirrors the magnitude gate so a duplicate/stale/out-of-order note (which
      // does NOT advance the max) is never priced twice.
      acct.responses.push(usage.last);
    }
    // else: a duplicate, stale resume replay or out-of-order note — cannot increase usage or cost.
  }

  /**
   * Aggregate the reconciled per-thread deltas by ACTUAL configured model into the result-frame
   * `modelUsage` shape. Returns `undefined` when no charged usage was reconciled (so the harness
   * omits `modelUsage` entirely, keeping a no-usage terminal byte-identical to before C4a).
   *
   * TOKEN buckets are always the cumulative `total` deltas (unchanged from C4a). COST (C4b, D5)
   * depends on `pricing`:
   *   - `pricing` omitted            → every entry `unreported`, no `costUSD` (token-only caller).
   *   - `authMode: 'subscription'`   → every entry `subscription`, no `costUSD` (no per-token
   *                                    charge exists; even an unknown model is subscription).
   *   - `authMode: 'api_key'`        → per model, CONSERVATIVE DOMINANCE: the entry is `metered`
   *                                    with the SUMMED per-response price ONLY when EVERY observed
   *                                    response of EVERY thread on that model both reconciles to
   *                                    its cumulative delta AND prices; if ANY response is
   *                                    unpriceable (unknown model, Sol boundary, bad cache split)
   *                                    or ANY thread's responses do not reconcile (a missed
   *                                    update), the WHOLE model entry is `unreported` with its
   *                                    tokens RETAINED and no `costUSD`.
   */
  aggregateByModel(pricing?: CodexPricingContext): Record<string, CodexModelUsageEntry> | undefined {
    interface ModelAgg {
      tokens: Cumulative;
      /** True until a thread on this model fails to reconcile or has an unpriceable response.
       *  Only consulted on an api-key run. */
      priceable: boolean;
      /** Summed per-response price across every thread on this model (api-key run only). */
      costUSD: number;
    }
    const byModel = new Map<string, ModelAgg>();
    const priceApiKey = pricing !== undefined && pricing.authMode === "api_key";
    for (const acct of this.threads.values()) {
      if (acct.maxTotal === undefined) continue; // registered but saw no usage note
      const charged = diffClamp(acct.maxTotal, acct.baseline ?? zeroCumulative());
      const agg = byModel.get(acct.model) ?? { tokens: zeroCumulative(), priceable: true, costUSD: 0 };
      addInto(agg.tokens, charged);
      if (priceApiKey && agg.priceable) {
        // Reconcile THIS thread's observed responses to ITS OWN cumulative delta before pricing:
        // a dropped note (missed response) leaves sum(last) below the delta, so the model degrades
        // to `unreported` while its tokens are retained (never fabricate a price for an unobserved
        // response). A reconciled thread prices every response independently (the >272K tier is
        // per response); any single unpriceable response taints the whole model (dominance).
        if (!responsesReconcileDelta(sumResponses(acct.responses), charged)) {
          agg.priceable = false;
        } else {
          for (const r of acct.responses) {
            const price = priceCodexResponse(acct.model, r, pricing.now);
            if (price === undefined) {
              agg.priceable = false;
              break;
            }
            agg.costUSD += price;
          }
        }
      }
      byModel.set(acct.model, agg);
    }
    const out: Record<string, CodexModelUsageEntry> = {};
    let any = false;
    for (const [model, agg] of byModel) {
      const c = agg.tokens;
      // Uncached input = total input minus the two detail buckets (D5), non-overlapping and
      // clamped >= 0, matching Claude's `input_tokens` (uncached) column semantics.
      const uncached = Math.max(c.inputTokens - c.cachedInputTokens - c.cacheWriteInputTokens, 0);
      // Skip a model whose reconciled delta is entirely zero — a registered thread that
      // produced no chargeable tokens is nothing to fold.
      if (uncached === 0 && c.cachedInputTokens === 0 && c.cacheWriteInputTokens === 0 && c.outputTokens === 0) {
        continue;
      }
      const base = {
        inputTokens: uncached,
        outputTokens: c.outputTokens,
        cacheReadInputTokens: c.cachedInputTokens,
        cacheCreationInputTokens: c.cacheWriteInputTokens,
        reasoningOutputTokens: c.reasoningOutputTokens,
      } as const;
      if (pricing === undefined) {
        out[model] = { ...base, costStatus: "unreported" };
      } else if (pricing.authMode === "subscription") {
        out[model] = { ...base, costStatus: "subscription" };
      } else if (agg.priceable) {
        out[model] = { ...base, costStatus: "metered", costUSD: agg.costUSD };
      } else {
        out[model] = { ...base, costStatus: "unreported" };
      }
      any = true;
    }
    return any ? out : undefined;
  }
}

/**
 * The RUN-level {@link HarnessCost} for a Codex terminal, folded from the emitted per-model
 * entries with the D5 unreported-dominant rollup. A subscription run is `subscription` regardless
 * of usage (the auth mode has no per-token charge). An api-key run is `metered` (with the summed
 * `costUSD`) ONLY when there is at least one entry and EVERY entry is metered; any `unreported`
 * entry — or no usage at all — makes the run `unreported`, so a numeric zero is never presented as
 * a real metered $0.
 */
export function deriveCodexRunCost(
  entries: Record<string, CodexModelUsageEntry> | undefined,
  authMode: "subscription" | "api_key",
): HarnessCost {
  if (authMode === "subscription") return { kind: "subscription" };
  if (entries === undefined) return { kind: "unreported" };
  let totalUSD = 0;
  let any = false;
  let allMetered = true;
  for (const entry of Object.values(entries)) {
    any = true;
    if (entry.costStatus === "metered" && typeof entry.costUSD === "number") {
      totalUSD += entry.costUSD;
    } else {
      allMetered = false;
    }
  }
  if (any && allMetered) return { kind: "metered", usd: totalUSD, source: "price_table" };
  return { kind: "unreported" };
}
