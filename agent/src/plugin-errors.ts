// Issue #1888: the SDK's system/init frame reports plugins that failed to load in
// `plugin_errors` (claude-agent-sdk 0.3.284+; the key is omitted when there are none). A run
// whose skills plugin did not load would otherwise work silently without the skills it
// selected. This module decodes that off-contract, model-adjacent field fail-closed and renders
// a bounded, redacted, log-safe description of it.

import { sanitizeForLog } from "./run-quiescence.js";

/** The failure-reason prefix (and fail_origin) of a skills run stopped because its skills
 *  plugin reported load errors at session init. runner.ts failOriginForReason maps it. */
export const REASON_SKILLS_PLUGIN_LOAD_FAILED = "skills_plugin_load_failed";

/** One plugin load error from the init frame, normalized: every field is a string. `type` is an
 *  open set upstream; it is displayed as-is and never branched on. */
export interface PluginLoadError {
  plugin: string;
  type: string;
  message: string;
  path?: string;
}

const UNNAMED_PLUGIN = "(unnamed plugin)";
const MALFORMED_TYPE = "malformed-entry";
const NO_DETAIL = "(no usable error detail)";

function malformedEntry(): PluginLoadError {
  return {
    plugin: UNNAMED_PLUGIN,
    type: MALFORMED_TYPE,
    message: "(malformed plugin error entry)",
  };
}

function strOr(v: unknown, fallback: string): string {
  return typeof v === "string" ? v : fallback;
}

/**
 * Decode `plugin_errors` fail-closed. Returns undefined ONLY when the key is absent (undefined),
 * null, or an empty array: the frame reports no errors. Anything else yields at least one
 * entry, so an off-contract frame can never let a skills run proceed as if clean:
 * - a non-empty array keeps its length, one entry per element; string fields are kept, any
 *   missing or non-string field gets a fixed placeholder (`path` is kept only when a string);
 * - a non-object element, or any other present non-array value, becomes a generic entry.
 */
export function parsePluginErrors(raw: unknown): PluginLoadError[] | undefined {
  if (raw === undefined || raw === null) return undefined;
  if (!Array.isArray(raw)) return [malformedEntry()];
  if (raw.length === 0) return undefined;
  return raw.map((el: unknown): PluginLoadError => {
    if (el === null || typeof el !== "object" || Array.isArray(el)) return malformedEntry();
    const rec = el as Record<string, unknown>;
    const entry: PluginLoadError = {
      plugin: strOr(rec["plugin"], UNNAMED_PLUGIN),
      type: strOr(rec["type"], MALFORMED_TYPE),
      message: strOr(rec["message"], NO_DETAIL),
    };
    if (typeof rec["path"] === "string") entry.path = rec["path"];
    return entry;
  });
}

const SHOWN_ENTRIES = 3;
const MAX_DESCRIPTION = 360;

/** Redact first, then strip control/bidi code points and cap: a secret straddling the cap is
 *  replaced whole before the cut, so no prefix of it survives. */
function field(text: string, max: number, redact: (s: string) => string): string {
  return sanitizeForLog(redact(text), max);
}

/**
 * A bounded, log-safe description of plugin load errors for a status line or failure reason:
 * `plugin (type): message [path: ...]` for the first three entries, then `; and N more`. Every
 * field is redacted, then sanitized and capped (plugin 80, type 40, message 200, path 160; a
 * truncated field may exceed its figure by the 3-character `...` marker). The whole string is
 * at most 360 characters, marker and `and N more` tail included.
 */
export function describePluginErrors(
  errors: readonly PluginLoadError[],
  redact: (s: string) => string = (s) => s,
): string {
  const parts = errors.slice(0, SHOWN_ENTRIES).map((e) => {
    let s = `${field(e.plugin, 80, redact)} (${field(e.type, 40, redact)}): ${field(e.message, 200, redact)}`;
    if (e.path !== undefined) s += ` [path: ${field(e.path, 160, redact)}]`;
    return s;
  });
  const more = errors.length > SHOWN_ENTRIES ? `; and ${errors.length - SHOWN_ENTRIES} more` : "";
  // Every piece is already sanitized, so this re-pass only bounds (code-point safe), keeping the
  // `and N more` tail intact. The cap leaves room for sanitizeForLog's `...` marker.
  return sanitizeForLog(parts.join("; "), MAX_DESCRIPTION - 3 - more.length) + more;
}
