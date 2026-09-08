// PRD #1171 (M3, milestone 3) — provider-terminal field normalization.
//
// A Codex `turn/completed` (run lane) and an advice `turn/completed` (advice lane)
// both carry UNTRUSTED provider fields — a free-form `status` string, a `turn.error`
// string that can be secret-bearing, and a `turn.usage` object of unbounded size and
// shape. If those flowed verbatim into a published run message (via harness-reducer)
// they could leak a secret or a multi-megabyte blob. This module is the SINGLE SOURCE
// OF TRUTH that bounds and redacts them, so the run decoder and the advice decoder
// cannot diverge: both call these pure helpers instead of hand-rolling their own
// closed vocabulary. No provider text is ever echoed out of here.
//
// The three normalizers are PURE (no I/O, no logging, allocation-bounded) so they are
// trivially unit-testable and safe to call on every terminal frame.

import type { HarnessUsage } from "../harness.js";

/** The CLOSED set of provider statuses we are willing to echo as a display `subtype`.
 *  Any status outside it collapses to the fixed token {@link UNKNOWN_STATUS}, so an
 *  arbitrary (or oversize/attacker-shaped) provider string never reaches a run message. */
const STATUS_ALLOWLIST: ReadonlySet<string> = new Set([
  "completed",
  "failed",
  "cancelled",
  "interrupted",
  "incomplete",
  "error",
  "timeout",
]);

/** The fixed token substituted for any status outside {@link STATUS_ALLOWLIST}. */
const UNKNOWN_STATUS = "unknown";

/** Cap on the number of numeric usage keys RETAINED — NOT a loop-iteration bound. The loop
 *  still walks every raw entry (a `break` fires only once 32 numeric keys are kept, which a
 *  key-heavy non-numeric object never reaches), so what keeps the scan finite is the raw
 *  input being size-bounded UPSTREAM by the transport's per-frame/queue ceilings, not this
 *  cap. Usage is provider-controlled, and a single Codex turn legitimately reports far fewer
 *  than this, so 32 is generous headroom for the retained subset while staying finite. */
const MAX_USAGE_KEYS = 32;

/**
 * Map a raw provider `status` to a closed `{ subtype, outcome }`. `outcome` is
 * `"success"` ONLY for the exact status `"completed"`; every other value (including
 * `undefined`) is fail-closed `"failed"`. `subtype` is the status verbatim IF it is in
 * the closed {@link STATUS_ALLOWLIST}, else the fixed token `"unknown"` — an arbitrary
 * provider string is NEVER echoed.
 */
export function normalizeCodexStatus(rawStatus: string | undefined): { subtype: string; outcome: "success" | "failed" } {
  const outcome: "success" | "failed" = rawStatus === "completed" ? "success" : "failed";
  const subtype = rawStatus !== undefined && STATUS_ALLOWLIST.has(rawStatus) ? rawStatus : UNKNOWN_STATUS;
  return { subtype, outcome };
}

/**
 * Build the terminal `errors` array WITHOUT reading any raw provider text. On success it
 * is empty; on failure it is a single fixed diagnostic derived ONLY from the already-
 * CLOSED `subtype` (which is either an allowlisted token or `"unknown"`). It deliberately
 * never reads or embeds the raw `turn.error`, so a secret-bearing provider error string
 * cannot leak through the run message.
 */
export function normalizeCodexTerminalErrors(subtype: string, outcome: "success" | "failed"): string[] {
  if (outcome === "success") return [];
  return [`codex turn ended with status: ${subtype}`];
}

/**
 * Bound and redact a raw provider `usage` object. Returns `undefined` when `rawUsage` is
 * not a plain object; otherwise returns `{ basis, tokens: {}, wire: { usage } }` where
 * `usage` is a FRESHLY-BUILT numeric-only subset of the raw entries — every key whose
 * value is a finite number `>= 0` is kept (dropping strings/objects/arrays/booleans/
 * negatives/NaN/Infinity), up to {@link MAX_USAGE_KEYS} keys. The raw object is NEVER
 * returned, so a secret-shaped string field or a huge nested blob cannot ride out.
 *
 * `tokens` is intentionally left empty: the exact Codex token field names are unconfirmed
 * offline, so mapping them into the neutral `tokens` shape is deferred to a later packaged
 * milestone; until then only the bounded numeric wire subset is retained.
 */
export function normalizeCodexUsage(rawUsage: unknown, basis: "call" | "turn"): HarnessUsage | undefined {
  if (rawUsage === null || typeof rawUsage !== "object" || Array.isArray(rawUsage)) return undefined;
  const bounded: Record<string, number> = {};
  let kept = 0;
  for (const [key, value] of Object.entries(rawUsage as Record<string, unknown>)) {
    if (kept >= MAX_USAGE_KEYS) break;
    if (typeof value === "number" && Number.isFinite(value) && value >= 0) {
      bounded[key] = value;
      kept += 1;
    }
  }
  return { basis, tokens: {}, wire: { usage: bounded } };
}
