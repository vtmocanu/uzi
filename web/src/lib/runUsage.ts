// Client-derived run usage (PRD #40 Decision 5 + 11). The run view builds its
// usage strip, per-phase table, per-agent table, and finish lines from the message
// stream — NOT from run_usage (that table feeds the cheap list/dashboard rollups).
// Full replay is unbounded, so this reduction is complete even for a failed or
// cancelled run whose frames landed before it died.
//
// TWO data paths, deliberately different (Decision 3 verdict b + Decision 11):
//   • Result frames (kind status/error, payload.event === "result") carry
//     CUMULATIVE-across-resume usage, so per-phase figures are DELTAS against a
//     running high-water mark and the run total is the sum of those clamped deltas.
//     duration_ms / num_turns are PER-INVOCATION (they read different CLI state),
//     so they are taken raw per phase and summed for the total.
//   • Assistant frames carry the API call's PER-CALL message.usage, attached by the
//     worker to exactly one emitted message per SDK frame. Those SUM directly, per
//     agent. This is a PURE reduction over the seq-deduped message list, recomputed
//     from state — never an incremental accumulator, which would double-count on the
//     ws→reconnect→REST-replay overlap.
//
// 🔴 THE RESULT-FRAME PATH READS `modelUsage`, PER MODEL — NEVER the frame's
// top-level `usage` or `total_cost_usd` (issue #195). Both readings exist on the
// wire (mapResult in agent/src/sdk-messages.ts forwards both, unguarded), and on the
// SDK pin this ships against they DISAGREE — measured on run 84b6a933 seq 173, where
// top-level `usage` read input 23 / output 16,854 / cache_read 653,081 against Σ
// modelUsage's 5,259 / 41,763 / 2,134,247: low on every field, by 2.5x on output and
// 3.3x on cache_read (and by 229x on input, which is the smallest column and so the
// least visible). Every other surface (run_usage, the board, `uzi run list`,
// /api/usage, /api/admin/usage) folds `modelUsage` in foldRunUsage
// (api/internal/workersvc/service.go), so reading `usage` here made the run page
// disagree with every rollup of itself.
//
// PRD #40 Decision 3 asserted the two "cannot diverge". That assertion is now true
// by MECHANISM rather than by assumption: this module reads the same field the
// server folds, and fixtures/run-usage/ pins the agreement from both sides
// (web/src/lib/runUsageContract.test.ts + api/internal/workersvc/run_usage_contract_test.go).
//
// WHAT THE SERVER'S TOTAL ACTUALLY IS, since everything below is defined against it.
// `UpsertRunUsage` stores GREATEST per (run_id, session_id, model), and
// `00063_run_usage_totals_view.sql` then reads MAX over the SESSIONS per model and
// SUMs across models — its own comment: "a plain SUM over run_usage would multiply
// the snapshots whenever session_id evolves". So, per column:
//
//     server run total = Σ over models of  MAX over ALL frames of nonNegTokens(value)
//
// PER-MODEL STATE IS THE LOAD-BEARING PART, and summing `modelUsage` per frame is
// NOT enough. `modelUsage` is not a model-stable map across frames: a model present
// in an earlier frame can be ABSENT from a later one (measured: haiku appears in
// 84b6a933's first result frame and is gone from its last, and 17 live runs show the
// shape). A per-frame sum telescopes to the LAST frame's models only and silently
// loses every vanished one. So cumulative state is kept PER MODEL and PER COLUMN.
//
// THE MARK IS A RUNNING MAX, NOT THE LAST-SEEN VALUE, and that distinction is the
// whole parity claim. With mᵢ = max(mᵢ₋₁, vᵢ) and m₀ = 0, the clamped delta
// max(0, vᵢ − mᵢ₋₁) is IDENTICALLY mᵢ − mᵢ₋₁, so the phase deltas telescope to
// max(0, max vᵢ) — the same expression the server computes, not an approximation of
// it. The equality holds for NON-MONOTONIC sequences too, so there is no
// monotonicity caveat to state. Last-seen does not: on [5000, 1000, 3000] it reports
// 7000 where the server holds 5000.
//
// THREE THINGS FLOOR A NEGATIVE, AND THEY ARE NOT REDUNDANT — they cover different
// OUTPUT SURFACES, which is why every attempt to state this as a count has been wrong:
//
//   tokens() at read   floors the VALUE  → protects both surfaces, so removing it
//                                          alone is unobservable
//   the m₀ = 0 seed    floors the MARK   → protects `modelTotals`, via mergeMax
//   clampDelta         floors the DELTA  → protects `total` and the phase rows
//
// Measured on one frame carrying `inputTokens: -5`, all four variants:
//
//   baseline                      total.fresh 0   modelTotals.input  0
//   tokens() floor removed        total.fresh 0   modelTotals.input  0
//   ?? cur only                   total.fresh 0   modelTotals.input  0
//   tokens() removed AND ?? cur   total.fresh 0   modelTotals.input -5   <- lands
//
// So no single-guard mutation reddens THE NEGATIVE-CLAMP TEST — do not widen that to
// "unobservable", since removing `clampDelta`'s floor reddens 6 named tests — and
// `total` never moves at all; only `modelTotals` does, and only when `tokens()` and
// the seed go together. Keep all three. Do not call them "redundant" or "independent":
// `clampDelta` is additionally load-bearing for the high-water identity itself, which
// is why its reds are the running-max cases rather than only sign-related ones.
//
// Five versions of this paragraph were wrong, and the reason is worth more than the
// conclusion: every one of them asked HOW MANY guards there are instead of WHICH
// SURFACE each defends. The seed's flooring role is also invisible until you look at
// `modelTotals` — a surface this change itself added — so the earlier "the seed is not
// a negative clamp" was true of the only output that existed when it was written.
//
// THE PRICE, which parity cannot settle and which is user-visible: a phase that does
// not exceed the previous high-water mark renders 0 tokens ALONGSIDE nonzero turns
// and duration. On [5000, 1000, 3000] the third phase reads 0, not 2000. The server
// has no per-phase view, so neither reading is "correct" there — this is a deliberate
// choice that the run TOTAL is the number every other surface shows and a post-reset
// phase row may read 0. Stated here rather than left to be discovered.
//
// A result frame with NO `modelUsage` (or an empty one) is SKIPPED ENTIRELY: no
// phase row, nothing counted. That mirrors foldRunUsage's `len(p.ModelUsage) == 0`
// guard exactly. There is deliberately no fallback to top-level `usage` — a fallback
// would reintroduce the very divergence this reading exists to close. Consequence,
// tested below: a zero-work frame (`modelUsage: {}` — one exists in the live DB)
// produces no phase row and therefore does not consume a phase LABEL either.
//
// TWO shapes reach that outcome, not one. The second is a map whose only entry has an
// EMPTY model id (`{"": {…}}`): the server folds the frame and then skips the entry
// (`if model == ""`), so it writes zero rows, while this reader skips the frame. The
// TOTALS still agree — both sides record nothing — and the only observable difference
// is the lost phase row, which is the same accepted consequence as the empty map.
// Measured 2026-08-02 on both sides. A mixed `{"": {…}, "good": {…}}` is not affected:
// both fold the frame and write one row.
//
// The skip is a `continue`, and that is load-bearing rather than stylistic: a real
// result frame carries top-level `usage` ALONGSIDE `modelUsage`, so a skip that fell
// through would drop the frame into the per-agent branch below and fold the run's
// CUMULATIVE total into one agent's per-call sum — a worse bug than the one this
// change fixes. Every exit from the result-frame branch is a `continue` for that
// reason.
//
// Cache/creation fields pass through null-coalescing on the assistant path, since
// BetaUsage's are nullable (ModelUsage's are not). This module is pure + unit-tested;
// the React components are thin.

import type { RunMessage } from "./api";

export interface PhaseUsage {
  /** seq of the result-frame message, so the finish line can look up its delta. */
  seq: number;
  /** "Plan" for the first phase, "Implement · iteration N" after. */
  label: string;
  /** Per-invocation, taken raw from the frame (not cumulative). */
  turns: number;
  durationMs: number;
  /** Per-phase DELTAS: Σ over the frame's `modelUsage` models, per COLUMN, of (this
   *  frame's cumulative − that model's running HIGH-WATER MARK), each clamped at 0.
   *  Not a whole-frame difference, and not a difference against the previous frame:
   *  see the header for why the high-water mark is what makes the run total equal the
   *  server's, and for the per-phase cost that buys (a post-reset phase reads 0). */
  fresh: number; // Δ inputTokens + Δ cacheCreationInputTokens, differenced apart
  cached: number; // cacheReadInputTokens
  out: number; // outputTokens
  costUsd: number; // Σ per-model costUSD deltas, never the frame's total_cost_usd
  /** An error result frame (a failed/cancelled phase still spent tokens). */
  isError: boolean;
}

export interface AgentUsage {
  agent: string;
  fresh: number;
  cached: number;
  out: number;
  /** PRD #93 Decision 4: how many counted usage frames carried each model. A COUNT
   *  map, not a Set, because the primary is the most frequent one and the tie-break
   *  needs the frequencies. Empty for a pre-feature run (no frame carried a model).
   *  Always a NULL-PROTOTYPE object — see `newModelCounts`. */
  modelCounts: Record<string, number>;
  /** The derived primary: highest count, ties broken lexicographically ascending.
   *  null when no frame carried a model — never fabricated from the strip's init
   *  model, which is the run's main-thread model, not this agent's (Decision 6). */
  model: string | null;
  /** Distinct models beyond `model` (two distinct models → 1, rendered "+1"). */
  otherModels: number;
}

export interface RunUsage {
  /** True once any result frame carried per-model usage — gates the CONFIRMED/billed
   *  surface (phases, total, modelTotals). A pre-feature run has none → the confirmed
   *  strip/tables never render, never a fabricated 0. Renamed from `hasUsage` (issue
   *  #237): the live token surface below has its own gate, `hasLiveTokens`. */
  hasConfirmed: boolean;
  phases: PhaseUsage[];
  /** One row per model, five columns each — the client's copy of this run's
   *  `run_usage` rows, which is the granularity the server actually stores. `total`
   *  below exposes only THREE token aggregates (it folds input and cache_creation
   *  into `fresh`), so this is the only surface at which all four token columns and
   *  a per-model cost can be compared against the server. Sorted by model id.
   *
   *  NO PRODUCTION READER YET, deliberately — it exists so the cross-language
   *  contract can assert at the server's own granularity, which no aggregate can.
   *  PRD #194 M1 (the live per-model cost estimate) is the intended first consumer.
   *  Not dead code to sweep. */
  modelTotals: ModelTotal[];
  total: {
    fresh: number;
    cached: number;
    out: number;
    costUsd: number;
    turns: number;
    durationMs: number;
    phaseCount: number;
  };
  /** cached / (fresh + cached) in [0,1] — the UNROUNDED truth.
   *
   *  NOT what the strip renders: `Math.round(this * 100)` reads "100% from cache" at
   *  99.6%, which is the band real runs sit in. `cacheDisplayPct` is the display
   *  transform and the panel calls that instead, so this currently has no production
   *  reader. Kept because it is the exact figure and the only one safe to do further
   *  arithmetic on — a display percentage clamped into [1,99] is not. */
  cacheHitRatio: number;
  /** The model from the init frame, if one was seen. */
  model: string | null;
  agents: AgentUsage[];
  agentTotal: { fresh: number; cached: number; out: number };
  /** Distinct models across ALL agents, sorted ascending — the per-agent table's
   *  "Attributed total" cell (one model → that string, >1 → "N models", none → —). */
  agentModels: string[];
  /** Per-result-frame delta, keyed by seq (the finish line reads its own phase). */
  phaseUsageBySeq: Map<number, PhaseUsage>;

  // ── The LIVE aggregate (issue #237) ──────────────────────────────────────────
  // A SEPARATE surface from the confirmed ones above, summed from the assistant
  // frames' per-call `usage` and DEDUPED by `(agent_instance, usage)`. It is what the
  // run page shows WHILE a phase is still running, before any result frame has
  // confirmed a billed total. `agent_instance` is null on the lead lane, so two
  // byte-identical lead calls collapse to one record — the #194-validated key. A
  // subagent invocation carries a distinct `agent_instance`, so its calls never
  // collapse into another lane's. This dedup is applied ONLY here; the confirmed
  // per-agent sum (`agents`/`agentTotal`) counts every frame raw and is unchanged.
  //
  // 🔴 THERE IS DELIBERATELY NO `out` AND NO COST FIELD on any live aggregate. Per-call
  // `output_tokens` is a message_start snapshot that captures only 1-4% of the true
  // output, so a live output figure would be wrong by ~25-100x, and a live cost derived
  // from it would repeat issue #194's self-contradiction (a "cost" that shrinks as the
  // real output grows). Only the two INPUT-side columns — fresh (input +
  // cache_creation) and cached (cache_read) — are live-trustworthy, so only they are
  // exposed.

  /** True when the deduped live input tokens (fresh + cached) are POSITIVE — not merely
   *  when a usage record was seen, so an all-zero `usage` frame never renders a fabricated
   *  0 live table. Gates the LIVE surface, the way `hasConfirmed` gates the billed one —
   *  the two are independent (a run mid-phase has live tokens and no confirmed total; a
   *  pre-feature run has neither). */
  hasLiveTokens: boolean;
  /** The deduped live run total. NO `out`, NO cost — see the header above. */
  liveTotal: { fresh: number; cached: number };
  /** One row per model that co-gated a live usage frame (the same per-frame `model`
   *  read the confirmed per-agent path uses). Sorted by model id ascending, codepoint
   *  compare like `modelTotals`. NO `out`, NO cost. Keyed via a `Map`, not a `{}`,
   *  because a model id is untrusted worker payload (same rationale as `agentMap`). */
  liveByModel: { model: string; fresh: number; cached: number }[];
  /** One row per agent (lane), keyed by `agent ?? "lead"`, in insertion order. NO
   *  `out`, NO cost. Untrusted-key `Map` for the same reason as `liveByModel`. */
  liveByAgent: { agent: string; fresh: number; cached: number }[];
}

/**
 * The integer percentage the strip renders as "X% from cache", and the width of the
 * bar's cached segment (the warn segment takes `100 - X`, so the two still sum to
 * exactly 100 at every width).
 *
 * `Math.round(ratio * 100)` alone is wrong at both ends, and the top end is the one
 * users hit: real runs sit at 97-99.6% cache, so 99.6 rounded up renders
 * "100% from cache" beside a zero-width warn segment while fresh tokens plainly exist
 * in the same panel. The invariant is therefore stated as a property rather than as a
 * rounding mode — **fresh > 0 must never display 100** — and its mirror is enforced
 * too, since flooring alone would report "0% from cache" for a small but real cache
 * ratio, which is the same lie pointed the other way.
 *
 *   cached == 0            → 0    (nothing cached; the bar is all warn)
 *   fresh == 0, cached > 0 → 100  (genuinely everything from cache)
 *   both > 0               → clamped into [1, 99], never either endpoint
 */
export function cacheDisplayPct(total: { fresh: number; cached: number }): number {
  if (total.cached <= 0) return 0;
  if (total.fresh <= 0) return 100;
  const pct = Math.round((total.cached / (total.fresh + total.cached)) * 100);
  return Math.min(99, Math.max(1, pct));
}

function rec(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" ? (v as Record<string, unknown>) : undefined;
}
function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}
function str(v: unknown): string | undefined {
  return typeof v === "string" ? v : undefined;
}

/** A BetaUsage / NonNullableUsage shape (nullable cache fields → 0). Used by the
 *  PER-CALL assistant path only; result frames read `modelUsage` (see readModelUsage). */
function readUsage(u: unknown): { fresh: number; cached: number; out: number } | undefined {
  const r = rec(u);
  if (!r) return undefined;
  return {
    fresh: num(r["input_tokens"]) + num(r["cache_creation_input_tokens"]),
    cached: num(r["cache_read_input_tokens"]),
    out: num(r["output_tokens"]),
  };
}

/**
 * One model's cumulative figures off a result frame, ONE FIELD PER `run_usage` COLUMN.
 *
 * `input` and `cacheCreation` are kept APART even though every display adds them into
 * `fresh`, because the server GREATESTs them as two independent columns
 * (`00063_run_usage_totals_view.sql` takes `MAX(input_tokens)` and
 * `MAX(cache_creation_tokens)` separately). MAX(a)+MAX(b) is not MAX(a+b), so tracking
 * the sum as one column would diverge the moment the two move in opposite directions —
 * `input` falling while `cacheCreation` rises reports 250 where the server holds 300.
 * Tested; not hypothetical arithmetic.
 */
interface ModelFigures {
  input: number; // inputTokens
  cacheCreation: number; // cacheCreationInputTokens
  cached: number; // cacheReadInputTokens
  out: number; // outputTokens
  costUsd: number; // costUSD, quantized to microdollars (see quantizeCost)
}

const ZERO_MODEL: ModelFigures = { input: 0, cacheCreation: 0, cached: 0, out: 0, costUsd: 0 };

/** One model's run-total, per column — the client's copy of a `run_usage` row. */
export interface ModelTotal extends ModelFigures {
  model: string;
}

/** Mirrors `nonNegTokens` (service.go): a negative count is clamped at fold time, not
 *  merely at the delta, because a fresh (run, session, model) key inserts verbatim. */
const tokens = (v: unknown): number => Math.max(0, num(v));

/** The `numeric(12,6)` ceiling, and `numericUSD`'s clamp target (service.go). */
const MAX_COST_USD = 999999.999999;

/**
 * Mirror `numericUSD` (service.go): clamp NaN/negative to 0, clamp above the column
 * ceiling, then quantize to microdollars. The quantization is applied HERE, per model
 * per frame, because that is where the server applies it — before storage, not to the
 * total — so the client's per-model cost is the NEAREST DOUBLE to the stored
 * `numeric(12,6)` decimal, and the run total then carries only float-summation noise
 * (~1e-14) instead of rounding error that competes with any sane tolerance. ("Nearest
 * double", not "bit-equal to the decimal": a double is never bit-equal to a decimal.
 * The nearest-double property is the weaker claim and it is the one that licenses the
 * exact `toBe` comparisons in the tests, since both sides parse the same decimal.)
 *
 * `+Infinity` DIVERGES, and it is reachable: `JSON.parse("1e400")` yields it, so a JSON
 * literal on the wire is enough — "no SDK emits it" was the wrong reason to dismiss it.
 * The divergence is also bigger than a cost mismatch. Measured 2026-08-02 against the
 * real `resultUsagePayload`: `{"m":{"costUSD":1e400}}` makes Go's json.Unmarshal fail
 * with *cannot unmarshal number 1e400 into ... float64*, so the server SKIPS THE WHOLE
 * FRAME, while this client writes a row with cost 0. Not coded, because reproducing
 * Go's numeric-range rejection in JS costs more than it buys — see readModelUsage's
 * divergence list, where this is one of the two known rows.
 */
function quantizeCost(v: unknown): number {
  const c = Math.min(Math.max(0, num(v)), MAX_COST_USD);
  return Math.round(c * 1e6) / 1e6;
}

/** `maxUsageModelRunes` (service.go) — the run_usage composite-PK cap, in CODE POINTS. */
const MAX_MODEL_ID_CODE_POINTS = 200;

/**
 * Mirror `truncateRunes(model, maxUsageModelRunes)`, which is `string([]rune(s)[:n])`:
 * Go runes are CODE POINTS, so this must cut on `Array.from`, never on `String.slice`,
 * whose indices are UTF-16 code units. On astral input `.slice(0, 200)` keeps only 100
 * code points and ends on a lone surrogate — unencodable as UTF-8, so Go's own
 * `[]rune` round-trip would turn it into U+FFFD and the two sides would key the row
 * differently. Without the cap at all, two distinct over-long ids collide onto one
 * server row and merge with GREATEST while the client keeps them apart and SUMS.
 *
 * Hostile input only (a real model id is ~35 bytes), but it is what makes the parity
 * claim unconditional rather than "for well-formed workers", which is the weaker claim
 * this whole change exists to retire.
 */
function capModelID(model: string): string {
  const cps = Array.from(model);
  return cps.length <= MAX_MODEL_ID_CODE_POINTS ? model : cps.slice(0, MAX_MODEL_ID_CODE_POINTS).join("");
}

/**
 * Read a result frame's `modelUsage` map (issue #195). The field names are the SDK's
 * camelCase `ModelUsage` shape as forwarded on the wire, matching `resultModelUsage`
 * in api/internal/workersvc/service.go byte for byte — the two readers are pinned
 * against one recorded frame set by fixtures/run-usage/.
 *
 * Returns undefined when there is nothing to fold, which the caller treats as "skip
 * this frame entirely":
 *   • no `modelUsage` key / not an object  → foldRunUsage's `len(p.ModelUsage) == 0`
 *   • `modelUsage: {}`                     → SAME GUARD. The server keys on the map
 *     being EMPTY, not on the field being absent, so these two must take the same
 *     branch. A real `modelUsage: {}` frame exists in the live DB (run e2d7427b seq
 *     318), so this is reachable rather than defensive.
 *   • an entry that is a non-null non-object (number, string, boolean, ARRAY) → Go's
 *     json.Unmarshal fails on the WHOLE payload, so the server folds nothing for that
 *     frame; poisoning the frame here rather than dropping the one bad entry is what
 *     keeps the two populations equal. Arrays need the explicit check: `typeof [] ===
 *     "object"`, so `rec` would otherwise accept one as an all-zero model.
 *
 * A `null` entry is NOT a poison — it folds as an all-zero model. That is not
 * defensive leniency, it is what Go does: unmarshalling `null` into a struct is a
 * documented no-op that returns no error, so the key lands with the zero value.
 * Measured 2026-08-02 against the real `resultUsagePayload`:
 *
 *   {"good":{"inputTokens":10},"bad":null}  Go FOLDS 2 rows, bad all-zero
 *   {"good":{"inputTokens":10},"bad":[]}    Go SKIPS THE FRAME (cannot unmarshal array)
 *   {"good":{"inputTokens":10},"bad":5}     Go SKIPS THE FRAME
 *
 * Treating `null` as a poison was the earlier behaviour and it was the costly
 * direction: one null entry discarded a frame including its VALID models, which the
 * server had already folded.
 *
 * KNOWN DIVERGENCE, deliberately not coded. Stated as ONE RULE rather than a list,
 * because the list was written with two rows and there are at least six, and an
 * enumeration in a comment goes stale silently while a rule does not:
 *
 *   ANY FIELD whose JSON type Go cannot decode into its Go type — a string, boolean,
 *   array, non-integer number, or out-of-range float — makes Go skip the WHOLE FRAME,
 *   while `num()` coerces it to 0 (or keeps it, if it is a number) and we fold.
 *   `null` is the sole exception and AGREES, because Go decodes it as the zero value.
 *
 * Measured on both sides 2026-08-02: `inputTokens` of `"5"`, `[]`, `true`, `1.5` and
 * `costUSD` of `1e400`, `"x"` all skip the frame server-side and fold here; only
 * `inputTokens: null` agrees. Note the costs are NOT uniform, which is why this is a
 * decision and not an oversight: `Number.isInteger` would catch `1.5` in one call, no
 * dearer than the array guard below, whereas int64/float64 RANGE has no cheap JS
 * equivalent. Neither is coded, so the whole family stays one rule.
 *
 * This is ENTRY-level versus FIELD-level, and the distinction is why the array guard
 * below is code while these are a comment: a non-object ENTRY is a shape every other
 * variant of already poisons the frame, so letting arrays through would have left the
 * rule reading "every non-object entry poisons the frame, except arrays". A bad FIELD
 * inside a well-formed entry is a different question, and this is its answer.
 *
 * An empty model id is skipped per-entry, exactly as the fold's `if model == ""` is —
 * counting it would add a row the server never wrote.
 *
 * A model id is UNTRUSTED worker payload data, which is why the accumulator it keys is
 * a `Map` and not an object: on a plain `{}` a model named `__proto__` writes through
 * the INHERITED prototype setter, so the entry vanishes and the object's own prototype
 * is reassigned, while one named `constructor`/`toString` reads back the inherited
 * FUNCTION, making every subsequent delta NaN and poisoning the run total through
 * `phases.reduce`. `Object.create(null)` is NOT subject to either — with no inherited
 * `__proto__` accessor the write is an ordinary own property (measured: `own=true`,
 * `keys=['__proto__']`, prototype still null), which is exactly why `newModelCounts`
 * above relies on it. `Map` is chosen here because it needs no such explanation and
 * matches `agentMap` below, not because the null-prototype form is unsafe.
 */
function readModelUsage(v: unknown): Map<string, ModelFigures> | undefined {
  const r = rec(v);
  if (!r) return undefined;
  const out = new Map<string, ModelFigures>();
  for (const [rawModel, entry] of Object.entries(r)) {
    if (Array.isArray(entry)) return undefined; // Go cannot unmarshal an array here
    // `null` is not a poison: Go decodes it into the zero struct. `rec` rejects it
    // (it is falsy), so ZERO_MODEL stands in for the fields below.
    const e = entry === null ? {} : rec(entry);
    if (!e) return undefined; // number / string / boolean → Go fails the whole payload
    if (rawModel === "") continue;
    // Cap BEFORE keying, so two over-long ids that the server collides onto one row
    // collide here too — and then merge by running MAX rather than summing.
    const model = capModelID(rawModel);
    const cur: ModelFigures = {
      input: tokens(e["inputTokens"]),
      cacheCreation: tokens(e["cacheCreationInputTokens"]),
      cached: tokens(e["cacheReadInputTokens"]),
      out: tokens(e["outputTokens"]),
      costUsd: quantizeCost(e["costUSD"]),
    };
    const prior = out.get(model);
    out.set(model, prior ? mergeMax(prior, cur) : cur);
  }
  return out.size > 0 ? out : undefined;
}

/** Column-wise MAX. Per COLUMN, never per record: the server's GREATEST is applied to
 *  each column independently, so a stored row can be a combination of values that
 *  appeared together in no single frame, and "keep whichever frame had the larger
 *  total" would not reproduce it. */
function mergeMax(a: ModelFigures, b: ModelFigures): ModelFigures {
  return {
    input: Math.max(a.input, b.input),
    cacheCreation: Math.max(a.cacheCreation, b.cacheCreation),
    cached: Math.max(a.cached, b.cached),
    out: Math.max(a.out, b.out),
    costUsd: Math.max(a.costUsd, b.costUsd),
  };
}

function isResultFrame(m: RunMessage): boolean {
  if (m.kind !== "status" && m.kind !== "error") return false;
  return rec(m.payload)?.["event"] === "result";
}

const clampDelta = (cur: number, prev: number): number => Math.max(0, cur - prev);

/** An agent row mid-reduction: the model display is derived once, after the fold. */
type AgentAcc = Omit<AgentUsage, "model" | "otherModels">;

/**
 * The per-agent model tally, keyed by a model id that is UNTRUSTED payload data (it
 * comes off the wire, from the worker's frame mapper). A plain `{}` inherits
 * `Object.prototype`, so a model literally named `constructor` / `toString` /
 * `valueOf` would read back the inherited FUNCTION — `(counts[m] ?? 0) + 1` becomes
 * a string, `primaryModel`'s numeric sort goes NaN, and frequency ordering silently
 * dies — while `__proto__` would make the write a no-op and lose the count entirely.
 * A null-prototype map has no inherited keys, so every model id is just a key.
 * `Object.entries` / `Object.keys` / spread all read own enumerable props only, so
 * the rest of the module is unaffected.
 */
function newModelCounts(): Record<string, number> {
  return Object.create(null) as Record<string, number>;
}

/**
 * PRD #93 Decision 4: the primary model is the one seen on the most counted frames;
 * equal counts break lexicographically ascending, so the result is deterministic
 * regardless of frame order. `otherModels` is what the "+K" suffix renders.
 */
function primaryModel(counts: Record<string, number>): { model: string | null; otherModels: number } {
  const entries = Object.entries(counts);
  if (entries.length === 0) return { model: null, otherModels: 0 };
  // Plain codepoint compare (not localeCompare): the tie-break must not vary by locale.
  entries.sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  return { model: entries[0][0], otherModels: entries.length - 1 };
}

/**
 * Reduce a run's message list into its usage surfaces. Pure: same messages →
 * same result, so React just re-runs it as the stream grows (Decision 9 live
 * fold-in needs no accumulator).
 */
export function deriveRunUsage(messages: RunMessage[]): RunUsage {
  const phases: PhaseUsage[] = [];
  const phaseUsageBySeq = new Map<number, PhaseUsage>();
  const agentMap = new Map<string, AgentAcc>();
  let model: string | null = null;

  // The LIVE aggregate (issue #237), DEDUPED by `(agent_instance, usage)`. All state
  // is local to this one call — the reduction stays pure and idempotent under the
  // ws→reconnect→REST-replay overlap, exactly like the confirmed accumulators. The
  // dedup NEVER touches the raw `agents`/`agentTotal` sum above: those count every
  // frame. `Map` (not `{}`) for both bucket maps because agent/model ids are untrusted
  // payload (same rationale as `agentMap`/`readModelUsage`).
  const seenLiveKeys = new Set<string>();
  const liveTotal = { fresh: 0, cached: 0 };
  const liveByAgentMap = new Map<string, { fresh: number; cached: number }>();
  const liveByModelMap = new Map<string, { fresh: number; cached: number }>();

  // Each model's RUNNING HIGH-WATER MARK, per column, to difference into per-phase
  // deltas. Not the last-seen value: see the header for why the two are not the same
  // fold, and why only this one equals the server's. Entries are only ever added or
  // raised, never removed, so a model absent from a frame contributes a 0 delta and
  // keeps its mark (issue #195).
  //
  // ZERO_MODEL is the seed because the identity `Σ max(0, vᵢ − mᵢ₋₁) = max(0, max vᵢ)`
  // needs m₀ = 0. It also floors the MARK (see the header's surface table). Seeding
  // from the first frame instead (`?? cur`) makes EVERY MODEL'S first frame difference
  // against itself and contribute 0.
  //
  // That is per MODEL, not per RUN, and the difference is most of the effect. On the
  // recorded fixture `?? cur` costs 326,462 tokens — frame 1's own 188,763, PLUS the
  // 137,699 sonnet contributes when it first appears in frame 2. So the run's first
  // frame is only 58% of it; "drops the entire first frame" is the narrower claim this
  // sentence's own mechanism refutes, and it read that way here until 2026-08-02.
  //
  // THE CHEAPEST GUARD ON THIS SEED IS `tsc`, NOT A TEST. Removing it removes the
  // last reference to ZERO_MODEL, so `?? cur` fails typecheck outright — `TS6133:
  // 'ZERO_MODEL' is declared but its value is never read` — and in a real gate run
  // the mutation never reaches vitest at all. (esbuild does not enforce
  // `noUnusedLocals`, so a vitest-only run still executes; forcing it past tsc with
  // a `void ZERO_MODEL;` is what makes the behavioural measurement below possible.)
  //
  // Forced past that, the discriminator is cited as a SHAPE rather than a tally,
  // because a count of reddened tests goes stale silently every time this file gains
  // one — three people measured 13, 14 and 15 for this same mutation, each correct
  // for their own tree and run scope. Under `?? cur`: the CONTRACT FIXTURE reddens on
  // the run total (1,836,071 against the recorded 2,162,533 — the 326,462 decomposed
  // above), the per-model rows still PASS because the marks themselves are unaffected,
  // and the NEGATIVE-CLAMP test stays GREEN. That last one is what shows the seed is
  // not what defends `total` against a negative; it defends the MARK.
  const prevByModel = new Map<string, ModelFigures>();
  let implIteration = 0;

  for (const m of messages) {
    const payload = rec(m.payload);

    // Model heartbeat (system init frame).
    if (m.kind === "status" && payload?.["event"] === "init" && model === null) {
      model = str(payload["model"]) ?? null;
    }

    if (isResultFrame(m)) {
      // A result frame NEVER falls through to the per-agent branch below, whether or
      // not it folded: its usage is the run's cumulative total, not one call's.
      const mu = readModelUsage(payload?.["modelUsage"]);
      if (!mu) continue; // no per-model usage to fold → skip, exactly as the server does
      const isError = m.kind === "error";
      const label = phases.length === 0 ? "Plan" : `Implement · iteration ${++implIteration}`;
      // Sum the PER-MODEL, PER-COLUMN deltas against each model's running high-water
      // mark, then raise the mark. Because mᵢ = max(mᵢ₋₁, vᵢ), the delta max(0, vᵢ −
      // mᵢ₋₁) is identically mᵢ − mᵢ₋₁, so the phase deltas telescope to the final
      // mark exactly — which IS the server's per-model MAX. Cost comes from the same
      // per-model figures, never from the frame's `total_cost_usd`: that field has the
      // identical hole (on 84b6a933 it is low by the vanished model's $0.005242
      // exactly), just at cents rather than at a factor, which is why nobody caught it
      // by reconciling cost.
      let fresh = 0;
      let cached = 0;
      let out = 0;
      let costUsd = 0;
      // `modelID`, not `model`: the outer `model` is the run's init-frame model, a
      // different thing entirely, and shadowing it here is the one name in this
      // function that costs a reader a double-take.
      for (const [modelID, cur] of mu) {
        const prev = prevByModel.get(modelID) ?? ZERO_MODEL;
        // input and cacheCreation are differenced APART and only then added into the
        // phase's `fresh`, because the server maxes them as two independent columns.
        fresh += clampDelta(cur.input, prev.input) + clampDelta(cur.cacheCreation, prev.cacheCreation);
        cached += clampDelta(cur.cached, prev.cached);
        out += clampDelta(cur.out, prev.out);
        costUsd += clampDelta(cur.costUsd, prev.costUsd);
        prevByModel.set(modelID, mergeMax(prev, cur));
      }
      const phase: PhaseUsage = {
        seq: m.seq,
        label,
        turns: num(payload?.["num_turns"]),
        durationMs: num(payload?.["duration_ms"]),
        fresh,
        cached,
        out,
        costUsd,
        isError,
      };
      phases.push(phase);
      phaseUsageBySeq.set(m.seq, phase);
      continue;
    }

    // Per-agent: assistant-frame per-call usage (never a result frame — that would
    // fold the cumulative total into the sum). One usage record per SDK frame.
    if (payload && "usage" in payload) {
      const u = readUsage(payload["usage"]);
      if (u) {
        const agent = m.agent ?? "lead";
        const acc = agentMap.get(agent) ?? { agent, fresh: 0, cached: 0, out: 0, modelCounts: newModelCounts() };
        acc.fresh += u.fresh;
        acc.cached += u.cached;
        acc.out += u.out;
        // PRD #93 Decision 2: `model` is CO-GATED with `usage` by the worker — it
        // rides the same surviving frame — so it is read only here, inside the
        // counted branch. A model-only frame therefore never creates an agent row,
        // and the strip's init model (a different path) never leaks in.
        const fm = str(payload["model"]);
        if (fm) acc.modelCounts[fm] = (acc.modelCounts[fm] ?? 0) + 1;
        agentMap.set(agent, acc);

        // LIVE surface (issue #237): a SEPARATE dedup pass on the SAME frame. The raw
        // `acc.*` above already counted this frame unconditionally; the live buckets
        // count it only once per `(agent_instance, usage)` signature. The key is built
        // from the RAW usage fields (not the folded `u`), so two calls collapse only
        // when their four wire columns are byte-identical on the same lane.
        const rawUsage = rec(payload["usage"]) ?? {};
        const liveKey = JSON.stringify([
          m.agent_instance,
          num(rawUsage["input_tokens"]),
          num(rawUsage["cache_read_input_tokens"]),
          num(rawUsage["cache_creation_input_tokens"]),
          num(rawUsage["output_tokens"]),
        ]);
        if (!seenLiveKeys.has(liveKey)) {
          seenLiveKeys.add(liveKey);
          liveTotal.fresh += u.fresh;
          liveTotal.cached += u.cached;
          const la = liveByAgentMap.get(agent) ?? { fresh: 0, cached: 0 };
          la.fresh += u.fresh;
          la.cached += u.cached;
          liveByAgentMap.set(agent, la);
          if (fm) {
            const lm = liveByModelMap.get(fm) ?? { fresh: 0, cached: 0 };
            lm.fresh += u.fresh;
            lm.cached += u.cached;
            liveByModelMap.set(fm, lm);
          }
        }
      }
    }
  }

  const total = phases.reduce(
    (t, p) => ({
      fresh: t.fresh + p.fresh,
      cached: t.cached + p.cached,
      out: t.out + p.out,
      costUsd: t.costUsd + p.costUsd,
      turns: t.turns + p.turns,
      durationMs: t.durationMs + p.durationMs,
      phaseCount: t.phaseCount + 1,
    }),
    { fresh: 0, cached: 0, out: 0, costUsd: 0, turns: 0, durationMs: 0, phaseCount: 0 },
  );

  const inTotal = total.fresh + total.cached;
  const agents: AgentUsage[] = [...agentMap.values()].map((a) => ({ ...a, ...primaryModel(a.modelCounts) }));
  const agentTotal = agents.reduce(
    (t, a) => ({ fresh: t.fresh + a.fresh, cached: t.cached + a.cached, out: t.out + a.out }),
    { fresh: 0, cached: 0, out: 0 },
  );
  // Run-wide distinct models: every model any agent was seen on, not just primaries.
  const agentModels = [...new Set(agents.flatMap((a) => Object.keys(a.modelCounts)))].sort();
  // The final high-water marks ARE the client's copy of the run's run_usage rows —
  // one per model, five columns each. Sorted by model id so the order is deterministic
  // rather than insertion-ordered off the wire.
  const modelTotals: ModelTotal[] = [...prevByModel.entries()]
    .map(([m, f]) => ({ model: m, ...f }))
    .sort((a, b) => (a.model < b.model ? -1 : a.model > b.model ? 1 : 0));

  // Live buckets → arrays. Models sorted by id ascending (codepoint compare, like
  // modelTotals); agents in insertion order (the lanes as first seen on the stream).
  const liveByModel = [...liveByModelMap.entries()]
    .map(([m, f]) => ({ model: m, fresh: f.fresh, cached: f.cached }))
    .sort((a, b) => (a.model < b.model ? -1 : a.model > b.model ? 1 : 0));
  const liveByAgent = [...liveByAgentMap.entries()].map(([a, f]) => ({ agent: a, fresh: f.fresh, cached: f.cached }));

  return {
    hasConfirmed: phases.length > 0,
    phases,
    total,
    modelTotals,
    cacheHitRatio: inTotal > 0 ? total.cached / inTotal : 0,
    model,
    agents,
    agentTotal,
    agentModels,
    phaseUsageBySeq,
    // POSITIVE tokens, not merely a seen record: `readUsage` returns {0,0,0} for any
    // `usage` that is just an object (even `usage: {}`), so gating on record presence
    // would flip this true for an all-zero frame and render a fabricated 0 live table —
    // the exact thing the confirmed gate refuses (and what #194 died of). Only the two
    // live-trustworthy input columns count toward "is there anything to show".
    hasLiveTokens: liveTotal.fresh + liveTotal.cached > 0,
    liveTotal,
    liveByModel,
    liveByAgent,
  };
}
