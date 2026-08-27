// Anthropic usage-limit detection (PRD #35 Decision 1).
//
// PURE by construction: no I/O, no clock of its own, no SDK import. Every entry
// point takes the frames and the current time as arguments, so both executors can
// share one classifier and neither owns it — and so the whole thing is testable
// against hand-written frames, which matters because the shapes below come from
// `sdk.d.ts` rather than from a frame anyone has observed in production.
//
// ── WHY THIS PINS TO STRUCTURED FIELDS AND NEVER TO TEXT ─────────────────────
// The string "Claude AI usage limit reached|<epoch>" is a Claude Code PRODUCT
// string and does not exist in @anthropic-ai/claude-agent-sdk (verified by grep of
// the installed bundle's sdk.mjs and bridge.mjs at the pinned 0.3.219: zero hits in
// both). Classifying provider limits by matching error text is fragile — a
// provider rewording a message must not silently disable the feature — so we
// deliberately pin to structured fields. Everything below reads a declared field.
//
// ── WHAT THE SDK GIVES US ────────────────────────────────────────────────────
// `SDKRateLimitEvent` (`type: 'rate_limit_event'`) is a top-level member of the
// SDKMessage union, so it arrives on the ordinary query iterator and needs no
// special subscription — it is simply one of the frame types `mapSdkMessage`
// currently drops on its `default` arm. It carries `SDKRateLimitInfo`:
// `status: 'allowed' | 'allowed_warning' | 'rejected'`, an optional `resetsAt`,
// and an optional `rateLimitType` over six members.
//
// The result frame carries `terminal_reason` (a 19-member union), of which
// `blocking_limit` and `rapid_refill_breaker` name a usage-limit death outright.
//
// ── THE TRANSIENT / SUSTAINED BOUNDARY ───────────────────────────────────────
// The SDK absorbs transient 429s itself with bounded internal retries that honour
// `retry-after` (the `api_retry` frames). Those never reach us as a death, and this
// module must never classify one: it looks only at frames from a turn that
// TERMINATED. A run whose limit cleared inside the SDK's own retry budget simply
// completes, exactly as today.

/** How the SDK reported the limit window, verbatim. */
export interface RateLimitObservation {
  /** 'allowed' | 'allowed_warning' | 'rejected' in the typings; kept as a string
   *  because an SDK bump may add a member and an unknown value must degrade to
   *  "not rejected" rather than throwing. */
  status: string;
  /** `resetsAt` normalized to epoch MILLISECONDS, or undefined when the frame
   *  carried nothing usable. See normalizeResetsAt for why this is not a straight
   *  copy of the SDK field. */
  resetsAtMs?: number;
  /** The SDK's `rateLimitType`, verbatim and UNVALIDATED. The server owns the
   *  allowlist (Decision 4); validating here as well would put the authoritative
   *  copy of the vocabulary on the untrusted side of the wire. */
  rateLimitType?: string;
}

/**
 * A turn that died because the owner's Anthropic usage window is exhausted.
 *
 * Distinct from a plain Error so the runner can branch on it without matching on
 * message text — the same discipline the detection itself follows.
 */
export class LimitReachedError extends Error {
  readonly resetsAtMs?: number;
  readonly rateLimitType?: string;

  constructor(opts: { resetsAtMs?: number; rateLimitType?: string; detail?: string }) {
    super(`Anthropic usage limit reached${opts.detail ? `: ${opts.detail}` : ""}`);
    this.name = "LimitReachedError";
    this.resetsAtMs = opts.resetsAtMs;
    this.rateLimitType = opts.rateLimitType;
  }
}

/**
 * Normalize the SDK's `resetsAt` to epoch milliseconds.
 *
 * The typings declare a bare `number` with NO unit, and the two plausible units
 * differ by a factor of 1000 — read a seconds value as milliseconds and the reset
 * lands in January 1970, read milliseconds as seconds and it lands in the year
 * 50000+. Neither is recoverable downstream, so the guess is made once, here.
 *
 * The threshold is 10^12 ms (2001-09-09). Any real reset is minutes-to-days from
 * now, so a millisecond value is ~1.7×10^12 and a second value is ~1.7×10^9 —
 * three orders of magnitude apart, with the boundary sitting in a range no live
 * value can occupy from either direction.
 *
 * Returns undefined for anything non-finite or non-positive. It deliberately does
 * NOT reject a past timestamp: "past" is only meaningful against a clock, and the
 * caller supplies that.
 */
export function normalizeResetsAt(value: unknown): number | undefined {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) return undefined;
  return value < 1e12 ? Math.round(value * 1000) : Math.round(value);
}

/** True for an SDK `rate_limit_event` frame. */
function isRateLimitEvent(message: unknown): message is { rate_limit_info?: unknown } {
  return !!message && typeof message === "object" && (message as { type?: unknown }).type === "rate_limit_event";
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" ? (v as Record<string, unknown>) : undefined;
}

/**
 * Reads `rate_limit_event` frames off the turn's message stream and keeps the
 * LATEST observation.
 *
 * Latest-wins, not first-wins: the SDK emits one of these whenever the limit
 * picture changes, so within a turn the newest frame is the one describing the
 * state the turn died in. An earlier `allowed` followed by a `rejected` must
 * classify on the `rejected`, and an earlier `rejected` followed by an `allowed`
 * (the limit cleared mid-turn) must not park.
 *
 * Deliberately a tiny stateful object rather than a fold over the whole stream:
 * the executor already iterates each frame exactly once for `mapSdkMessage`, and a
 * second pass would mean buffering the turn.
 */
export class RateLimitObserver {
  private observation: RateLimitObservation | undefined;

  /** Feed one SDK message. Non-rate-limit frames are ignored. */
  observe(message: unknown): void {
    if (!isRateLimitEvent(message)) return;
    const info = asRecord((message as { rate_limit_info?: unknown }).rate_limit_info);
    if (!info) return;
    const status = info["status"];
    if (typeof status !== "string") return;
    const rateLimitType = info["rateLimitType"];
    this.observation = {
      status,
      resetsAtMs: normalizeResetsAt(info["resetsAt"]),
      rateLimitType: typeof rateLimitType === "string" ? rateLimitType : undefined,
    };
  }

  /** The most recent usable observation of this turn, if any. */
  get latest(): RateLimitObservation | undefined {
    return this.observation;
  }
}

/** The two `TerminalReason` members that name a usage-limit death (sdk.d.ts). */
const LIMIT_TERMINAL_REASONS = new Set(["blocking_limit", "rapid_refill_breaker"]);

/**
 * Classify a TERMINATED turn's result frame as a usage-limit death, or not.
 *
 * Returns the normalized limit facts when it is one, and undefined otherwise.
 * Undefined is the safe answer everywhere: it means "fail this run as today".
 *
 * Precedence, per Decision 1:
 *
 *   (a) `terminal_reason ∈ {blocking_limit, rapid_refill_breaker}` — PRIMARY. The
 *       SDK is naming the cause outright, so no corroboration is required and a
 *       missing or stale reset only costs us a fallback park schedule.
 *
 *   (b) otherwise, the latest `rate_limit_event` with `status: 'rejected'`, and
 *       ONLY when corroborated by a reset in the FUTURE. Without that clause a
 *       `rejected` observed early in a long turn would park a run that actually
 *       died of something unrelated later — `error_max_turns` being the case the
 *       PRD names.
 *
 * 🔴 A SUCCESS RESULT NEVER PARKS, even carrying `blocking_limit`.
 * `terminal_reason` is declared on `SDKResultSuccess` as well as on
 * `SDKResultError` (sdk.d.ts), so the combination is REPRESENTABLE, and this is
 * the PRD's open question. The reading taken here is that a turn which produced a
 * result is not a failure whatever its terminal_reason says, and parking one would
 * discard completed work to wait out a window. It is asserted by a test rather than
 * left implicit, so changing the reading is a failing test rather than a silent
 * behaviour change.
 *
 * Note this is NOT the same test as `isErrorResult`, and the difference is load-
 * bearing: that helper treats `{subtype: 'success', is_error: true}` as an error
 * (a shape the executor already routes down its failure path, with `errorSubtype`
 * reading literally "success"), and so does this. What is excluded here is a
 * success result that is genuinely NOT flagged as an error.
 */
export function classifyLimitFailure(
  resultFrame: unknown,
  latest: RateLimitObservation | undefined,
  nowMs: number,
): { resetsAtMs?: number; rateLimitType?: string } | undefined {
  const frame = asRecord(resultFrame);
  if (!frame || frame["type"] !== "result") return undefined;
  // A clean success is not a failure, whatever terminal_reason it carries.
  if (frame["subtype"] === "success" && frame["is_error"] !== true) return undefined;

  const futureReset = latest?.resetsAtMs !== undefined && latest.resetsAtMs > nowMs ? latest.resetsAtMs : undefined;

  const terminalReason = frame["terminal_reason"];
  if (typeof terminalReason === "string" && LIMIT_TERMINAL_REASONS.has(terminalReason)) {
    // Take the reset only when it is still ahead of us. A past reset is worse than
    // no reset: the server would compute a retry_not_before already in the past and
    // promote the run straight back into the same exhausted window.
    return { resetsAtMs: futureReset, rateLimitType: latest?.rateLimitType };
  }

  // ⚠ SPECIFIED BEHAVIOUR WITH A KNOWN RESIDUAL — NOT AN OVERSIGHT, DO NOT "FIX".
  //
  // The corroboration is a FUTURE RESET AND NOTHING ELSE. The death's subtype is not
  // consulted at all. So:
  //
  //     rejected + PAST reset   + error_max_turns  ->  does NOT park
  //     rejected + FUTURE reset + error_max_turns  ->  PARKS
  //
  // What prevents classification in the first line is the ELAPSED RESET, not the fact
  // that the death was unrelated. It is worth being precise about which, because the
  // residual is not a corner case: a five_hour window's reset is in the future for
  // most of five hours, so ANY unrelated death observed after a `rejected` inside
  // that window parks the run. Decision 1 specifies staleness as the discriminator
  // and this implements exactly that; the PRD's own "an unrelated death must not
  // park" phrasing is true only of the elapsed-reset subset.
  //
  // Narrowing it — excluding subtypes we believe unrelated — is policy the spec does
  // not state, and it was deliberately not added on the implementer's own judgement.
  // It was raised with the lead and the ruling was to keep it as written.
  //
  // Recorded this explicitly because the whole design principle of this module is
  // that a change of reading is a FAILING TEST rather than a silent behaviour change.
  // A future reader who "tidies" this into a subtype allowlist would be making
  // precisely the silent change the principle exists to prevent.
  if (latest?.status === "rejected" && futureReset !== undefined) {
    return { resetsAtMs: futureReset, rateLimitType: latest.rateLimitType };
  }

  return undefined;
}

/**
 * Human-readable tail for a feed line, e.g. "five_hour; resets at 2026-07-27T21:00:00.000Z".
 *
 * Display only, and deliberately NOT the failure_reason for an opted-out run: that
 * sentence is COMPOSED BY THE SERVER from its own allowlisted enum (Decision 8). If
 * the worker composed it, an unvalidated `rateLimitType` would ride into the run row
 * as free text and the "a worker cannot smuggle a non-enum rate_limit_type past the
 * server" criterion would be false — the field would simply have taken a different
 * route into the same place.
 */
export function describeLimit(limit: { resetsAtMs?: number; rateLimitType?: string }): string {
  const type = limit.rateLimitType ?? "unknown";
  if (limit.resetsAtMs === undefined) return `${type}; reset time unknown`;
  return `${type}; resets at ${new Date(limit.resetsAtMs).toISOString()}`;
}
