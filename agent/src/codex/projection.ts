// Issue #1583 — the PURE projection of a Codex tool callback onto the neutral run-message
// stream. Claude's SDK hands uzi `tool_use`/`tool_result` blocks for free; Codex does not,
// so the harness synthesizes a started/finished {@link HarnessItem} pair per routed root
// callback. Everything that decides WHAT such a pair carries lives here, free of any
// transport, broker or process import, so it is unit-testable byte for byte:
//
//   - every projected string is SCRUBBED (the caller's redactor) BEFORE it is bounded, so a
//     cut can never split a secret into a fragment the redactor no longer matches;
//   - every projected string is BOUNDED in UTF-8 bytes, marker included, and cut only on a
//     code-point boundary (no lone surrogate, no introduced U+FFFD);
//   - ids are NAMESPACED per harness instance and turn, never the provider's raw call id
//     alone, so two runs/epochs/turns that reuse a call id never collide in the lanes.

import { randomBytes } from "node:crypto";

import type { CallbackResult } from "./broker.js";

/** The byte cap for one projected tool input or output (the persisted payload, not what the
 *  model saw: the broker already bounds shell output at 64 KiB for the model reply). */
export const MAX_PROJECTED_BYTES = 16 * 1024;

// Per-field cap for the display fields kept off an OVERSIZED input, so their sum plus the
// preview stays well inside MAX_PROJECTED_BYTES.
const MAX_DISPLAY_FIELD_BYTES = 1024;
// The preview of an oversized input's serialization.
const MAX_PREVIEW_BYTES = 8 * 1024;
// A tool name is model-influenced; keep it short.
const MAX_NAME_BYTES = 256;
// The call-id part of a projected id.
const MAX_CALL_ID_CHARS = 64;

// The input fields the run UI renders for a tool_use (description/file_path/path/skill); kept
// individually when the full input is too large to project whole.
const DISPLAY_FIELDS = ["description", "file_path", "path", "skill"] as const;

/** The per-projection string redactor (identity when nothing is registered). */
export type ProjectionScrub = (s: string) => string;

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

function scrubDeep(value: unknown, scrub: ProjectionScrub): unknown {
  if (typeof value === "string") return scrub(value);
  if (Array.isArray(value)) return value.map((v) => scrubDeep(v, scrub));
  if (value !== null && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) out[scrub(k)] = scrubDeep(v, scrub);
    return out;
  }
  return value;
}

function safeStringify(value: unknown): string {
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value) ?? "";
  } catch {
    return "";
  }
}

/**
 * Project a callback's model-supplied arguments into a bounded `tool_use` input. Every string
 * (keys included) is scrubbed first. When the scrubbed input serializes within
 * {@link MAX_PROJECTED_BYTES} it is returned whole; otherwise only the display fields survive
 * (each bounded), plus a bounded `preview` of the scrubbed serialization and `truncated: true`.
 */
export function projectToolInput(args: unknown, scrub: ProjectionScrub): unknown {
  if (args === undefined) return {};
  const scrubbed = scrubDeep(args, scrub);
  const serialized = safeStringify(scrubbed);
  if (Buffer.byteLength(serialized, "utf8") <= MAX_PROJECTED_BYTES) return scrubbed;
  const out: Record<string, unknown> = {};
  if (scrubbed !== null && typeof scrubbed === "object" && !Array.isArray(scrubbed)) {
    const fields = scrubbed as Record<string, unknown>;
    for (const key of DISPLAY_FIELDS) {
      const v = fields[key];
      if (typeof v === "string") out[key] = boundUtf8(v, MAX_DISPLAY_FIELD_BYTES);
    }
  }
  out.preview = boundUtf8(serialized, MAX_PREVIEW_BYTES);
  out.truncated = true;
  return out;
}

/** Project a settled broker result into a bounded `tool_result` content string: the
 *  stringified output on success, the (already bounded) broker message on failure. Scrubbed
 *  BEFORE bounding. */
export function projectToolOutput(result: CallbackResult, scrub: ProjectionScrub): string {
  const text = result.ok ? safeStringify(result.output) : result.message;
  return boundUtf8(scrub(text), MAX_PROJECTED_BYTES);
}

/** A bounded, scrubbed display name for a projected tool item. */
export function projectToolName(name: string | undefined, scrub: ProjectionScrub): string {
  if (name === undefined || name.length === 0) return "unknown";
  return boundUtf8(scrub(name), MAX_NAME_BYTES);
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
export function projectedId(nonce: string, turnOrdinal: number, callId: string, counter: number): string {
  const part = callId.replace(/[^A-Za-z0-9_.:-]/g, "").slice(0, MAX_CALL_ID_CHARS);
  return `cx-${nonce}-t${turnOrdinal}-${part.length > 0 ? part : `n${counter}`}`;
}
