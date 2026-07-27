// PRD #35 (usage-limit park): the pure half of every web surface for a run parked
// on an Anthropic usage limit — the window vocabulary, the resume countdown, and
// the per-run toggle's inertness rule. Framework-free and unit-tested in isolation,
// the same split runBadge.ts uses and for the same reason: RunView needs routing, a
// live stream and a dozen API mocks to mount, so logic left inside it is only ever
// reachable through the component.
//
// 🔴 THERE ARE TWO FIELDS CALLED `rate_limit_type` AND THEY HAVE DIFFERENT TRUST
// LEVELS. That is the whole reason this module exists rather than one inline
// lookup, and getting them backwards is a real (if modest) XSS-adjacent hole in
// one direction and a lie to the user in the other:
//
//   1. `Run.rate_limit_type` — the run ROW's column, on the DTO. The server
//      allowlists it (`workersvc.CoerceRateLimitType`): anything it does not
//      recognise is stored as the literal `"unknown"`, so no worker-controlled byte
//      ever reaches this column. It is a safe enum. Use `runWindowLabel`.
//
//   2. The `rate_limit_type` inside a `limit_wait` / `limit_hit` RUN_MESSAGE
//      payload. The server does NOT touch this. `run_messages` payloads are
//      worker-authored JSON and the only server-side processing is a NUL strip and
//      a rune cap — the allowlist in (1) is on a different write path entirely and
//      cannot reach here. It is arbitrary worker text. Use `feedWindowLabel`.
//
// The difference in treatment follows from that and not from taste. (1) may render
// an unrecognised value verbatim, because a NEWER SERVER can legitimately ship an
// SDK member this web build has not heard of and silently dropping it would hide
// the one fact the user asked for. (2) may not, because "a value this build has not
// heard of" and "a hostile string" are indistinguishable on that path — so it is
// resolved through a closed lookup and anything else renders as nothing at all,
// leaving the surrounding sentence still true.

import { isTerminalRun } from "./api";
import { stripUnsafeChars } from "./safeText";

/**
 * The SDK's `SDKRateLimitInfo.rateLimitType` vocabulary, plus the server's
 * `"unknown"` coercion, mapped to the words a human reads.
 *
 * Mirrors `rateLimitTypes` in `api/internal/workersvc/limitwait.go`, which is the
 * authority — an SDK pin bump that adds a member moves that list (and migration
 * 00090's CHECK) first, and this map follows. Being behind is SAFE by construction
 * on both paths: the DTO path falls back to the raw value, the feed path to no
 * clause at all.
 *
 * `unknown` is a real member, not an absence: the server writes it when a worker
 * reports a type outside the set, so "we know a limit was hit and do not know which
 * window" is a fact worth its own words rather than a blank.
 */
export const RATE_LIMIT_WINDOW_LABELS: Readonly<Record<string, string>> = {
  five_hour: "5-hour window",
  seven_day: "7-day window",
  seven_day_opus: "7-day Opus window",
  seven_day_sonnet: "7-day Sonnet window",
  seven_day_overage_included: "7-day window (overage included)",
  overage: "overage limit",
  unknown: "usage window",
};

/**
 * The label for a SERVER-ALLOWLISTED type (a run row's `rate_limit_type`).
 *
 * Returns null only for a genuinely absent value. An unrecognised one is rendered
 * verbatim (control/format characters stripped, per safeText) rather than dropped:
 * the column cannot hold worker free text, so the honest reading of a value this
 * build does not know is "a newer server knows something I do not", and showing it
 * lets the user act on it.
 */
export function runWindowLabel(type: string | null | undefined): string | null {
  if (type == null || type === "") return null;
  const known = RATE_LIMIT_WINDOW_LABELS[type];
  if (known) return known;
  const raw = stripUnsafeChars(type).trim();
  return raw === "" ? null : raw;
}

/**
 * The label for an UNTRUSTED type off a run-message payload.
 *
 * Closed lookup, neutral fallback: a value outside the vocabulary yields null and
 * the caller omits the clause entirely. It never echoes the input — not even
 * escaped, not even truncated — because there is no allowlist behind this field and
 * every consumer of this function is rendering a sentence that stays correct
 * without the window name.
 *
 * Takes `unknown` rather than `string` on purpose: the payload is decoded JSON, so
 * the field can be a number, an object, or absent, and a `string`-typed parameter
 * would push a cast onto every call site — which is where the check would rot.
 */
export function feedWindowLabel(type: unknown): string | null {
  if (typeof type !== "string") return null;
  return RATE_LIMIT_WINDOW_LABELS[type] ?? null;
}

/**
 * The `resets_at` off a run-message payload, as epoch milliseconds, or null.
 *
 * **The agreed shape is an ISO STRING**, and it is OMITTED rather than null when the
 * reset is unknown, so "unknown" is one shape on the wire rather than two. The
 * worker normalizes the SDK's own bare number (which carries a seconds-vs-
 * milliseconds ambiguity) before it is emitted, so this never re-derives that.
 *
 * The numeric arm below is therefore NOT the contract — it is a guard, and the
 * distinction matters to anyone tempted to delete it as dead code. This field is
 * worker-authored and reaches the database through nothing but a NUL strip and a
 * rune cap, so "the emitter promises an ISO string" is a statement about the worker
 * we ship, not about the bytes that can arrive. A number that fell through would
 * otherwise render as an authoritative-looking 1970, which is worse than no clause
 * at all. Applying the same `< 10^12` threshold the worker uses costs four lines and
 * removes that outcome.
 *
 * *(An earlier version of this comment asserted the wire carries a number and the
 * string was the odd case. That was true of a draft of the contract and false of the
 * one that shipped; it is corrected here rather than left to mislead the next reader
 * into "fixing" the string arm.)*
 *
 * Returns null for anything else, INCLUDING a numerically valid but absurd instant
 * — the caller's sentence is written to stand without the clause, so a missing reset
 * costs a detail while a wrong one costs the user's trust in the whole row.
 */
export function parseFeedInstant(v: unknown): number | null {
  if (typeof v === "number") {
    if (!Number.isFinite(v) || v <= 0) return null;
    // Same threshold the worker normalizes on: below 10^12 the value cannot be
    // milliseconds (that epoch is 2001) so it is seconds.
    return v < 1e12 ? v * 1000 : v;
  }
  if (typeof v === "string") {
    const ms = Date.parse(v);
    return Number.isFinite(ms) ? ms : null;
  }
  return null;
}

/**
 * The resume countdown: how long until `retry_not_before`, coarsened to two units.
 *
 * NOT `formatElapsed` (runBadge.ts) and NOT `formatDuration` (RunEvent.tsx), both of
 * which top out at hours. A park is clamped by RUN_LIMIT_MAX_PARK, whose documented
 * worst case is measured in DAYS, so an 8-day wait would read "192h 0m" through the
 * one and "11520m 00s" through the other. It also drops to seconds near zero, where
 * a run about to resume is the one moment the exact number matters.
 *
 * Returns null once the instant has passed — the caller then says "resuming shortly"
 * rather than counting up into a negative, because the promotion pass runs on a
 * ticker and a run whose clock has expired is waiting on the next tick, not late.
 * null is also the answer for an unparseable timestamp, so a malformed value degrades
 * to the same honest phrasing instead of "NaNh NaNm".
 */
export function formatCountdown(toIso: string | null | undefined, nowMs: number): string | null {
  if (!toIso) return null;
  const target = Date.parse(toIso);
  if (!Number.isFinite(target)) return null;
  const ms = target - nowMs;
  if (ms <= 0) return null;
  const s = Math.ceil(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

/**
 * Whether the per-run "wait on limit" toggle is live for a run.
 *
 * 🔴 THE TOGGLE IS ABOUT THE NEXT LIMIT, NEVER THE CURRENT STATUS, and the name
 * invites the opposite reading. Flipping it OFF on an already-parked run does not
 * un-park it, cancel it, or fail it — cancel is a separate control. Flipping it ON
 * while parked is a no-op on this park. So `limit_wait` is deliberately an ENABLED
 * status here, which looks wrong at a glance and is not: a user who parked by
 * accident wants to stop the NEXT park, and the server's endpoint is guarded by the
 * same negative predicate as CancelRunServerSide, which admits it for free.
 *
 * Terminal runs are inert because there is no future limit to have an opinion about;
 * the server would no-op the write anyway (its guard is authoritative), so this is
 * the UI agreeing with it rather than enforcing it.
 */
export function canToggleWaitOnLimit(status: string): boolean {
  return !isTerminalRun(status);
}
