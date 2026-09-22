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

import type { HarnessErrorCategory, HarnessUsage } from "../harness.js";

/** The CLOSED classification both harness lanes surface for a Codex terminal error: a
 *  fixed `display` token (or the literal `"unknown"`), its harness `category`, and an
 *  optional bounded `httpStatus`. Shared so the run decoder and the advice decoder cannot
 *  diverge on the shape. Never carries raw provider text. */
export interface CodexErrorClassification {
  classification: string;
  category: HarnessErrorCategory;
  httpStatus?: number;
}

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
 * CLOSED `subtype` (which is either an allowlisted token or `"unknown"`) and, when given,
 * the already-CLOSED `info` classification from {@link normalizeCodexErrorInfo}. It
 * deliberately never reads or embeds the raw `turn.error`, so a secret-bearing provider
 * error string cannot leak through the run message.
 *
 * `info` is OPTIONAL and BACKWARD-COMPATIBLE: a 2-arg call is byte-identical to before.
 */
export function normalizeCodexTerminalErrors(
  subtype: string,
  outcome: "success" | "failed",
  info?: { classification: string; httpStatus?: number },
): string[] {
  if (outcome === "success") return [];
  if (info === undefined) return [`codex turn ended with status: ${subtype}`];
  return [`codex turn ended with status: ${subtype} (${formatCodexClassification(info)})`];
}

/** One entry in {@link CODEX_ERROR_INFO_MAP}: the wire tag's echo `display` (a fixed literal,
 *  never the raw value), its harness `category`, its wire `shape` (a bare `"scalar"` string
 *  vs. an externally-`"tagged"` one-key object), and `http: true` for the four HTTP-transport
 *  tagged variants whose one-key value object may carry a bounded `httpStatusCode`. */
interface CodexErrorInfoEntry {
  shape: "scalar" | "tagged";
  display: string;
  category: HarnessErrorCategory;
  http?: boolean;
}

/** The CLOSED map of the pinned Codex 0.153.2 `codexErrorInfo` enum (13 scalar + 5 tagged)
 *  from the exact lower-camel wire tag to its {@link CodexErrorInfoEntry}. A tag ABSENT from
 *  this map, or present with a shape that does not match the wire form, collapses to the
 *  fixed `"unknown"` classification — an arbitrary/attacker-shaped provider tag is NEVER
 *  echoed, and only a range-gated `httpStatusCode` is ever extracted (for the four `http`
 *  entries). `message` / `additionalDetails` / `misalignment` / `turnKind` and any other raw
 *  value are never read. */
const CODEX_ERROR_INFO_MAP: ReadonlyMap<string, CodexErrorInfoEntry> = new Map<string, CodexErrorInfoEntry>([
  // Scalar variants: a bare lower-camel tag string.
  ["unauthorized", { shape: "scalar", display: "unauthorized", category: "authentication" }],
  ["usageLimitExceeded", { shape: "scalar", display: "usageLimitExceeded", category: "rate_limit" }],
  ["rateLimitExceeded", { shape: "scalar", display: "rateLimitExceeded", category: "rate_limit" }],
  ["sessionBudgetExceeded", { shape: "scalar", display: "sessionBudgetExceeded", category: "rate_limit" }],
  ["contextWindowExceeded", { shape: "scalar", display: "contextWindowExceeded", category: "model" }],
  ["serverOverloaded", { shape: "scalar", display: "serverOverloaded", category: "transport" }],
  ["internalServerError", { shape: "scalar", display: "internalServerError", category: "transport" }],
  ["badRequest", { shape: "scalar", display: "badRequest", category: "unknown" }],
  ["sandboxError", { shape: "scalar", display: "sandboxError", category: "unknown" }],
  ["cyberPolicy", { shape: "scalar", display: "cyberPolicy", category: "unknown" }],
  ["misalignmentPolicyViolation", { shape: "scalar", display: "misalignmentPolicyViolation", category: "unknown" }],
  ["threadRollbackFailed", { shape: "scalar", display: "threadRollbackFailed", category: "unknown" }],
  ["other", { shape: "scalar", display: "other", category: "unknown" }],
  // Tagged variants: a one-key externally-tagged object `{ "<tag>": { ... } }`.
  ["httpConnectionFailed", { shape: "tagged", display: "httpConnectionFailed", category: "transport", http: true }],
  ["responseTooManyFailedAttempts", { shape: "tagged", display: "responseTooManyFailedAttempts", category: "transport", http: true }],
  ["responseStreamConnectionFailed", { shape: "tagged", display: "responseStreamConnectionFailed", category: "transport", http: true }],
  ["responseStreamDisconnected", { shape: "tagged", display: "responseStreamDisconnected", category: "transport", http: true }],
  // activeTurnNotSteerable carries a turnKind payload that MUST be ignored (no http, no status).
  ["activeTurnNotSteerable", { shape: "tagged", display: "activeTurnNotSteerable", category: "unknown" }],
]);

/**
 * Normalize an UNTRUSTED Codex `codexErrorInfo` (a field inside `TurnError`) into a CLOSED
 * `{ classification, category, httpStatus? }`, or `undefined` when `raw == null`.
 *
 * The wire form is EITHER a bare lower-camel scalar tag string OR a one-key externally-tagged
 * object `{ "<tag>": { ... } }`. The classification written out is ALWAYS `entry.display` from
 * a {@link CODEX_ERROR_INFO_MAP} hit whose `shape` matches the input form, or the literal
 * `"unknown"` — an explicit lookup + shape check BEFORE any echo (never `tag ?? "unknown"`).
 * `httpStatus` is extracted ONLY for the four `http` tagged entries, and ONLY when the
 * one-key value object's `httpStatusCode` is an integer in `[100, 599]`. No `message` /
 * `additionalDetails` / `misalignment` / `turnKind` / other raw value is ever read. Pure and
 * allocation-bounded (the own-key check never iterates an unbounded structure).
 */
export function normalizeCodexErrorInfo(raw: unknown): CodexErrorClassification | undefined {
  if (raw == null) return undefined;

  // Scalar wire form: a bare lower-camel tag string. Echo only on a scalar-shaped map hit.
  if (typeof raw === "string") {
    const entry = CODEX_ERROR_INFO_MAP.get(raw);
    if (entry !== undefined && entry.shape === "scalar") {
      return { classification: entry.display, category: entry.category };
    }
    return { classification: "unknown", category: "unknown" };
  }

  // Tagged wire form: a non-null, non-array object with EXACTLY ONE own enumerable key.
  if (typeof raw === "object" && !Array.isArray(raw)) {
    const keys = Object.keys(raw as Record<string, unknown>);
    const key = keys.length === 1 ? keys[0] : undefined;
    if (key !== undefined) {
      const entry = CODEX_ERROR_INFO_MAP.get(key);
      if (entry !== undefined && entry.shape === "tagged") {
        const result: CodexErrorClassification = {
          classification: entry.display,
          category: entry.category,
        };
        if (entry.http === true) {
          const value = (raw as Record<string, unknown>)[key];
          if (value !== null && typeof value === "object" && !Array.isArray(value)) {
            const status = (value as Record<string, unknown>).httpStatusCode;
            if (typeof status === "number" && Number.isInteger(status) && status >= 100 && status <= 599) {
              result.httpStatus = status;
            }
          }
        }
        return result;
      }
    }
    return { classification: "unknown", category: "unknown" };
  }

  // Any other shape (array, number, boolean) — never echoed.
  return { classification: "unknown", category: "unknown" };
}

/** Choose the classification to surface: prefer a RECOGNIZED (non-"unknown") one — the
 *  notification first, then the terminal turn.error fallback — else whichever exists
 *  (an "unknown", or undefined when neither is present). */
export function pickCodexClassification(
  fromNotification: CodexErrorClassification | undefined,
  fromTerminal: CodexErrorClassification | undefined,
): CodexErrorClassification | undefined {
  if (fromNotification !== undefined && fromNotification.classification !== "unknown") return fromNotification;
  if (fromTerminal !== undefined && fromTerminal.classification !== "unknown") return fromTerminal;
  return fromNotification ?? fromTerminal;
}

/**
 * Render the human classification suffix (WITHOUT surrounding parens) shared by both
 * harnesses so they display identically: just `info.classification`, or
 * `` `${classification}; http ${httpStatus}` `` when a bounded status is present.
 */
export function formatCodexClassification(info: { classification: string; httpStatus?: number }): string {
  return info.httpStatus !== undefined ? `${info.classification}; http ${info.httpStatus}` : info.classification;
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
