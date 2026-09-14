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
// COST is DELIBERATELY OUT OF SCOPE in C4a: every emitted entry carries the closed marker
// `costStatus: 'unreported'` and NO `costUSD` (present only for metered records). C4b adds the
// subscription/metered price-table semantics. The map/reconciler here price nothing.

import type { CodexThreadTokenUsage, CodexUsageBreakdown } from "./transport.js";

/** The closed cost marker on an emitted Codex `modelUsage` entry. C4a emits ONLY
 *  `'unreported'`; C4b widens this union to `'subscription' | 'metered'`. */
export type CodexCostStatus = "unreported";

/**
 * One per-model entry in the result-frame `modelUsage` map, as emitted for a Codex run. The
 * token fields mirror the existing Claude `modelUsage` entry
 * (`inputTokens`/`outputTokens`/`cacheReadInputTokens`/`cacheCreationInputTokens`) so C1's
 * server fold (`api/internal/workersvc/usage_fold.go`) reads it UNCHANGED, EXTENDED with the
 * closed {@link CodexCostStatus} marker. `inputTokens` here is the UNCACHED input (Claude's
 * `input_tokens` semantics — non-overlapping with the two cache buckets), and
 * `reasoningOutputTokens` is a subset of `outputTokens` (never added to it). `costUSD` is
 * intentionally ABSENT: it is present only for metered records, and C4a prices nothing.
 */
export interface CodexModelUsageEntry {
  readonly inputTokens: number;
  readonly outputTokens: number;
  readonly cacheReadInputTokens: number;
  readonly cacheCreationInputTokens: number;
  readonly reasoningOutputTokens: number;
  readonly costStatus: CodexCostStatus;
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
    this.threads.set(threadId, { model, resumed, maxMagnitude: 0 });
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
      return;
    }
    const m = magnitude(usage.total);
    if (m > acct.maxMagnitude) {
      acct.maxTotal = total;
      acct.maxMagnitude = m;
    }
    // else: a duplicate, stale resume replay or out-of-order note — cannot increase usage.
  }

  /**
   * Aggregate the reconciled per-thread deltas by ACTUAL configured model into the result-frame
   * `modelUsage` shape. Returns `undefined` when no charged usage was reconciled (so the harness
   * omits `modelUsage` entirely, keeping a no-usage terminal byte-identical to before C4a).
   * Every entry carries `costStatus: 'unreported'` and no `costUSD` (C4a prices nothing).
   */
  aggregateByModel(): Record<string, CodexModelUsageEntry> | undefined {
    const byModel = new Map<string, Cumulative>();
    for (const acct of this.threads.values()) {
      if (acct.maxTotal === undefined) continue; // registered but saw no usage note
      const charged = diffClamp(acct.maxTotal, acct.baseline ?? zeroCumulative());
      const agg = byModel.get(acct.model) ?? zeroCumulative();
      addInto(agg, charged);
      byModel.set(acct.model, agg);
    }
    const out: Record<string, CodexModelUsageEntry> = {};
    let any = false;
    for (const [model, c] of byModel) {
      // Uncached input = total input minus the two detail buckets (D5), non-overlapping and
      // clamped >= 0, matching Claude's `input_tokens` (uncached) column semantics.
      const uncached = Math.max(c.inputTokens - c.cachedInputTokens - c.cacheWriteInputTokens, 0);
      // Skip a model whose reconciled delta is entirely zero — a registered thread that
      // produced no chargeable tokens is nothing to fold.
      if (uncached === 0 && c.cachedInputTokens === 0 && c.cacheWriteInputTokens === 0 && c.outputTokens === 0) {
        continue;
      }
      out[model] = {
        inputTokens: uncached,
        outputTokens: c.outputTokens,
        cacheReadInputTokens: c.cachedInputTokens,
        cacheCreationInputTokens: c.cacheWriteInputTokens,
        reasoningOutputTokens: c.reasoningOutputTokens,
        costStatus: "unreported",
      };
      any = true;
    }
    return any ? out : undefined;
  }
}
