// Issue #1583 — the PURE projection of a Codex tool callback onto the neutral run-message
// stream. Claude's SDK hands uzi `tool_use`/`tool_result` blocks for free; Codex does not,
// so the harness synthesizes a started/finished {@link HarnessItem} pair per routed root
// callback. Everything that decides WHAT such a pair carries lives here, free of any
// transport or process import (only the broker's pure identifier-safety predicate is reused),
// so it is unit-testable byte for byte:
//
//   - every projected string is SCRUBBED (the caller's redactor) BEFORE it is bounded, so a
//     cut can never split a secret into a fragment the redactor no longer matches;
//   - every projected value is BOUNDED by its JSON-SERIALIZED UTF-8 size (the persisted
//     payload, escapes included), marker included, and cut only on a code-point boundary (no
//     lone surrogate, no introduced U+FFFD);
//   - nesting is walked to a fixed depth and cycles are cut, so a hostile argument shape can
//     never overflow the stack;
//   - ids are NAMESPACED per harness instance and turn, never the provider's raw call id
//     alone, so two runs/epochs/turns that reuse a call id never collide in the lanes.

import { randomBytes } from "node:crypto";

import { isUnsafeIdentifierChar, type CallbackResult } from "./broker.js";
import { emptyCounts, sanitizeText } from "../sanitize.js";

/** The byte cap for one projected tool input or output, measured on its JSON serialization
 *  (the persisted payload, not what the model saw: the broker already bounds shell output at
 *  64 KiB for the model reply). A projected output string's serialization adds only its two
 *  enclosing quotes on top of this. */
export const MAX_PROJECTED_BYTES = 16 * 1024;

// Per-field cap for the display fields kept off an OVERSIZED input, so their sum plus the
// preview stays well inside MAX_PROJECTED_BYTES.
const MAX_DISPLAY_FIELD_BYTES = 1024;
// The preview of an oversized input's serialization.
const MAX_PREVIEW_BYTES = 8 * 1024;
// A tool name is model-influenced; keep it short.
const MAX_NAME_BYTES = 256;
// A delegation label (the model's `description` arg) is display-only; keep it short too.
const MAX_LABEL_BYTES = 256;
// One projected child text/thinking item (a subagent's message), bounded on its JSON-escaped size.
const MAX_PROJECTED_TEXT_BYTES = 64 * 1024;
// The call-id part of a projected id.
const MAX_CALL_ID_CHARS = 64;
// Containers nested deeper than this are replaced by DEPTH_MARKER while scrubbing.
const MAX_SCRUB_DEPTH = 32;
const DEPTH_MARKER = "[depth limit]";
const CYCLE_MARKER = "[cycle]";

// The input fields the run UI renders for a tool_use (description/file_path/path/skill); kept
// individually when the full input is too large to project whole.
const DISPLAY_FIELDS = ["description", "file_path", "path", "skill"] as const;

/** The per-projection string redactor (identity when nothing is registered). */
export type ProjectionScrub = (s: string) => string;

/** Scrub `s` AFTER applying the batcher's own normalization (sanitize.ts: NULs stripped, lone
 *  surrogates replaced). The batcher re-runs that normalization at persist time, and its
 *  redactor does not know the runtime-released Codex tokens; normalizing only there would
 *  re-join a token split by NULs (e.g. UTF-16LE shell output) AFTER this scrub had missed it.
 *  Normalizing first makes the batcher's pass a no-op on projected strings.
 *
 *  The server's display sanitizer (runactivity `sanitize`) additionally strips every control
 *  and format code point from the Detail/label fields it derives, which would re-join a secret
 *  split by a zero-width, soft-hyphen or control char past an exact-substring scrub. So after the
 *  ordinary scrub, its {@link stripUnsafe} view is scrubbed too: when THAT view still hides a
 *  secret, the stripped-and-scrubbed view is returned (its format/control chars are dropped);
 *  otherwise the ordinary scrub is returned, so output keeps its newlines and tabs even when it
 *  also carried an unsplit secret. */
function cleanScrub(s: string, scrub: ProjectionScrub): string {
  const scrubbed = scrub(sanitizeText(s, emptyCounts()));
  const stripped = stripUnsafe(scrubbed);
  if (stripped !== scrubbed) {
    const rescrubbed = scrub(stripped);
    if (rescrubbed !== stripped) return rescrubbed;
  }
  return scrubbed;
}

function codePointBytes(cp: number): number {
  if (cp <= 0x7f) return 1;
  if (cp <= 0x7ff) return 2;
  if (cp <= 0xffff) return 3;
  return 4;
}

function truncationMarker(droppedBytes: number): string {
  return `…[truncated ${droppedBytes} bytes]`;
}

/**
 * Bound `s` to at most `maxBytes` UTF-8 bytes INCLUDING the truncation marker. A string that
 * already fits is returned unchanged. Otherwise the longest code-point-aligned prefix that
 * leaves room for the marker is kept and the marker reports how many UTF-8 bytes were dropped.
 * The cut walks code points (a surrogate pair is one unit), so it never leaves a lone
 * surrogate and never introduces U+FFFD.
 */
export function boundUtf8(s: string, maxBytes: number): string {
  const total = Buffer.byteLength(s, "utf8");
  if (total <= maxBytes) return s;
  // The dropped count is at most `total`, so a marker sized for `total` is never shorter than
  // the final one: reserving it keeps prefix + final marker within maxBytes.
  const budget = maxBytes - Buffer.byteLength(truncationMarker(total), "utf8");
  if (budget < 0) return "";
  let kept = 0;
  let end = 0;
  for (const ch of s) {
    const size = codePointBytes(ch.codePointAt(0) ?? 0);
    if (kept + size > budget) break;
    kept += size;
    end += ch.length;
  }
  return s.slice(0, end) + truncationMarker(total - kept);
}

/** The UTF-8 size of one code point once JSON.stringify has escaped it (quotes excluded). */
function jsonEscapedBytes(cp: number): number {
  if (cp === 0x22 || cp === 0x5c) return 2; // \" and \\
  if (cp === 0x08 || cp === 0x09 || cp === 0x0a || cp === 0x0c || cp === 0x0d) return 2; // \b \t \n \f \r
  if (cp < 0x20) return 6; // \u00XX
  if (cp >= 0xd800 && cp <= 0xdfff) return 6; // a lone surrogate is escaped as \uXXXX
  return codePointBytes(cp);
}

/** The UTF-8 size of `JSON.stringify(s)` without its two enclosing quotes. */
function jsonStringBytes(s: string): number {
  return Buffer.byteLength(JSON.stringify(s), "utf8") - 2;
}

/**
 * Bound `s` so its JSON-escaped form (`JSON.stringify(result)` minus the two quotes) is at most
 * `maxBytes` UTF-8 bytes INCLUDING the truncation marker: backslashes, quotes and control
 * characters count at their escaped size, so the persisted payload honours the cap. The marker
 * reports the dropped UTF-8 bytes of `s`. Cuts on a code-point boundary like {@link boundUtf8}.
 */
export function boundJsonString(s: string, maxBytes: number): string {
  if (jsonStringBytes(s) <= maxBytes) return s;
  const total = Buffer.byteLength(s, "utf8");
  // The marker has no JSON-escaped characters, so its escaped size is its UTF-8 size; one sized
  // for `total` is never shorter than the final one.
  const budget = maxBytes - Buffer.byteLength(truncationMarker(total), "utf8");
  if (budget < 0) return "";
  let escaped = 0;
  let kept = 0;
  let end = 0;
  for (const ch of s) {
    const cp = ch.codePointAt(0) ?? 0;
    const size = jsonEscapedBytes(cp);
    if (escaped + size > budget) break;
    escaped += size;
    kept += codePointBytes(cp);
    end += ch.length;
  }
  return s.slice(0, end) + truncationMarker(total - kept);
}

/** Scrub every string (keys included) of a JSON-shaped value. Containers nested deeper than
 *  {@link MAX_SCRUB_DEPTH} become {@link DEPTH_MARKER}, and a container already on the current
 *  path becomes {@link CYCLE_MARKER}, so the walk is bounded whatever the value's shape. */
function scrubDeep(value: unknown, scrub: ProjectionScrub, depth = 0, path: WeakSet<object> = new WeakSet()): unknown {
  if (typeof value === "string") return cleanScrub(value, scrub);
  if (value === null || typeof value !== "object") return value;
  if (depth >= MAX_SCRUB_DEPTH) return DEPTH_MARKER;
  if (path.has(value)) return CYCLE_MARKER;
  path.add(value);
  try {
    if (Array.isArray(value)) return value.map((v) => scrubDeep(v, scrub, depth + 1, path));
    // defineProperty, not assignment: a JSON `"__proto__"` key stays an ordinary own key instead
    // of re-parenting `out` (which would hide it and let inherited fields leak into the display).
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) {
      Object.defineProperty(out, cleanScrub(k, scrub), {
        value: scrubDeep(v, scrub, depth + 1, path),
        enumerable: true,
        writable: true,
        configurable: true,
      });
    }
    return out;
  } finally {
    path.delete(value);
  }
}

/** JSON.stringify that reports failure (or a non-serializable value) as `undefined`. */
function trySerialize(value: unknown): string | undefined {
  try {
    return JSON.stringify(value);
  } catch {
    return undefined;
  }
}

function safeStringify(value: unknown): string {
  if (typeof value === "string") return value;
  return trySerialize(value) ?? "";
}

/**
 * Project a callback's model-supplied arguments into a bounded `tool_use` input. Every string
 * (keys included) is scrubbed first. When the scrubbed input serializes within
 * {@link MAX_PROJECTED_BYTES} it is returned whole; otherwise only the display fields survive
 * (each bounded), plus a bounded `preview` of the scrubbed serialization and `truncated: true`.
 * Every field is bounded on its JSON-escaped size, and the assembled result is re-measured:
 * `JSON.stringify(result)` never exceeds MAX_PROJECTED_BYTES (fallback `{ truncated: true }`).
 */
export function projectToolInput(args: unknown, scrub: ProjectionScrub): unknown {
  if (args === undefined) return {};
  const scrubbed = scrubDeep(args, scrub);
  const serialized = trySerialize(scrubbed);
  if (serialized === undefined) return { truncated: true };
  if (Buffer.byteLength(serialized, "utf8") <= MAX_PROJECTED_BYTES) return scrubbed;
  const out: Record<string, unknown> = {};
  if (scrubbed !== null && typeof scrubbed === "object" && !Array.isArray(scrubbed)) {
    const fields = scrubbed as Record<string, unknown>;
    for (const key of DISPLAY_FIELDS) {
      const v = fields[key];
      if (typeof v === "string") out[key] = boundJsonString(v, MAX_DISPLAY_FIELD_BYTES);
    }
  }
  out.preview = boundJsonString(serialized, MAX_PREVIEW_BYTES);
  out.truncated = true;
  // The per-field budgets already sum well below the cap; this re-measure keeps the guarantee
  // on the serialized result itself rather than on that arithmetic.
  const final = trySerialize(out);
  if (final === undefined || Buffer.byteLength(final, "utf8") > MAX_PROJECTED_BYTES) return { truncated: true };
  return out;
}

/** Project a settled broker result into a bounded `tool_result` content string: the
 *  stringified output on success, the (already bounded) broker message on failure. Scrubbed
 *  BEFORE bounding, and bounded on its JSON-escaped size, so the persisted
 *  `JSON.stringify(content)` is at most MAX_PROJECTED_BYTES + 2 (the quotes). */
export function projectToolOutput(result: CallbackResult, scrub: ProjectionScrub): string {
  return projectOutputValue(result.ok ? result.output : result.message, scrub);
}

/** Project a raw tool output VALUE (a string passes through, anything else is JSON-stringified)
 *  into a bounded `tool_result` content string: scrubbed BEFORE it is bounded on its JSON-escaped
 *  size, exactly like {@link projectToolOutput}. Used for a delegated child's tool results. */
export function projectOutputValue(value: unknown, scrub: ProjectionScrub): string {
  return boundJsonString(cleanScrub(safeStringify(value), scrub), MAX_PROJECTED_BYTES);
}

/** Project one text/thinking string (a delegated child's message): scrubbed BEFORE it is
 *  bounded on its JSON-escaped size. */
export function projectText(text: string, scrub: ProjectionScrub): string {
  return boundJsonString(cleanScrub(text, scrub), MAX_PROJECTED_TEXT_BYTES);
}

// Unicode general category Cf (format): the broker's predicate lists the zero-width/bidi ranges,
// but not every Cf code point (U+00AD soft hyphen, U+061C, U+180E, U+FFF9..U+FFFB, tags...).
const FORMAT_CHAR = /\p{Cf}/u;

/** Drop every control and bidi/format code point: the broker's `isUnsafeIdentifierChar` set
 *  (C0/DEL/C1 controls incl. TAB/LF/CR, zero-width and bidi chars, U+2028/U+2029) plus every
 *  other Cf code point, a superset of the server display sanitizer's Cc+Cf strip. */
function stripUnsafe(s: string): string {
  let out = "";
  for (const ch of s) {
    if (isUnsafeIdentifierChar(ch.codePointAt(0) ?? 0) || FORMAT_CHAR.test(ch)) continue;
    out += ch;
  }
  return out;
}

/** A bounded, scrubbed display name for a projected tool item. Control and bidi/format code
 *  points (the broker's `isUnsafeIdentifierChar` set: ESC, newline, RLO, ...) are stripped so a
 *  model-chosen name cannot rewrite a terminal or forge a log line; an empty result is
 *  "unknown". The strip runs BEFORE the scrub: stripping after it would re-join a secret that a
 *  zero-width/control char had split past the exact-substring redactor. */
export function projectToolName(name: string | undefined, scrub: ProjectionScrub): string {
  if (name === undefined) return "unknown";
  const safe = cleanScrub(stripUnsafe(name), scrub);
  return safe.length === 0 ? "unknown" : boundUtf8(safe, MAX_NAME_BYTES);
}

/** A bounded, scrubbed display label (a delegation's model-supplied `description`). Unsafe code
 *  points are stripped BEFORE the scrub, for the same reason as {@link projectToolName}; an empty
 *  result stays empty (a label is optional). */
export function projectLabel(label: string, scrub: ProjectionScrub): string {
  return boundUtf8(cleanScrub(stripUnsafe(label), scrub), MAX_LABEL_BYTES);
}

/** A fresh per-harness id nonce: 12 lowercase hex chars. */
export function newProjectionNonce(): string {
  return randomBytes(6).toString("hex");
}

/**
 * The namespaced id of one projected tool pair: `cx-<nonce>-t<turn>-<callIdPart>`. The call id
 * is provider-supplied, so it is restricted to `[A-Za-z0-9_.:-]` (every other char is dropped)
 * and capped at 64 chars; a call id empty after that falls back to `n<counter>`.
 */
export function projectedId(
  nonce: string,
  turnOrdinal: number,
  callId: string,
  counter: number,
  scrub: ProjectionScrub = (s) => s,
): string {
  return `cx-${nonce}-t${turnOrdinal}-${callIdPart(callId, counter, scrub)}`;
}

/**
 * The id of one projected tool pair inside a delegated child: `<dispatchId>/<callIdPart>`, so
 * every child tool id is namespaced under its dispatch (itself namespaced by harness nonce and
 * turn). The call-id part is restricted, scrubbed and capped exactly like {@link projectedId}.
 * The `/` separator is OUTSIDE the call-id alphabet, so a child id can never equal a root id
 * (whose call-id part may itself contain `:`).
 */
export function childProjectedId(
  dispatchId: string,
  callId: string,
  counter: number,
  scrub: ProjectionScrub = (s) => s,
): string {
  return `${dispatchId}/${callIdPart(callId, counter, scrub)}`;
}

/** The provider call id restricted to `[A-Za-z0-9_.:-]`, scrubbed and capped at 64 chars, or
 *  `n<counter>` when nothing survives. */
function callIdPart(callId: string, counter: number, scrub: ProjectionScrub): string {
  // Restrict, scrub (a restrict-joined secret is matched here), restrict again (the redaction
  // marker's `*` is outside the id alphabet), and only THEN cap, so a long secret's prefix can
  // never survive the cut.
  const restrict = (s: string): string => s.replace(/[^A-Za-z0-9_.:-]/g, "");
  const part = restrict(scrub(restrict(callId))).slice(0, MAX_CALL_ID_CHARS);
  return part.length > 0 ? part : `n${counter}`;
}
