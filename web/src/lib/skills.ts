// Client-side skill helpers shared by the Skills page, the allocation panel, and
// the mock API. The server (api/internal/handler/skills.go + skilltmpl) stays
// authoritative on every rule here; these mirror it only to preempt a round-trip
// and to phrase the failure for a human. NameRe and the single-line description
// rule are copied verbatim from skilltmpl.NameRe and the handler's hasControlChar
// check.

import type { SkillScope } from "./api";

// Mirrors skilltmpl.NameRe (`^[a-z0-9][a-z0-9-]{0,63}$`): kebab-case, starts with
// a letter or digit, max 64 chars. Immutable after creation, so it is validated
// on create only.
export const SKILL_NAME_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;

// Mirrors the server default SKILL_MAX_BYTES (config.go). The server env can
// override it, so this drives a soft client hint only — a save is still the
// authority on the real limit.
export const SKILL_MAX_BYTES = 65536;

// Control chars (incl. newlines) would break out of the single synthesized
// frontmatter line, the same guard the server's hasControlChar enforces. Built
// from a string of \u escapes so this source file stays ASCII-only.
const CONTROL_CHAR_RE = new RegExp("[\\u0000-\\u001f\\u007f]");

// byteLength counts UTF-8 bytes, matching the server's len(body) check (Go
// strings are bytes). A multibyte char therefore contributes its real cost.
export function byteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

export function skillNameError(name: string): string | null {
  const n = name.trim();
  if (!n) return "Name is required.";
  if (!SKILL_NAME_RE.test(n)) {
    return "Use kebab-case: lowercase letters, digits, and hyphens, starting with a letter or digit (max 64 characters).";
  }
  return null;
}

export function descriptionError(description: string): string | null {
  const d = description.trim();
  if (!d) return "Description is required.";
  if (CONTROL_CHAR_RE.test(description)) {
    return "Description must be a single line (no line breaks or control characters).";
  }
  return null;
}

export function bodyError(body: string): string | null {
  if (body.trim() === "") return "Body is required.";
  const n = byteLength(body);
  if (n > SKILL_MAX_BYTES) {
    return `Body is ${n.toLocaleString()} bytes, over the ${SKILL_MAX_BYTES.toLocaleString()}-byte limit.`;
  }
  return null;
}

// SCOPE_LABEL is the human name for each scope, used on badges and group headings.
export const SCOPE_LABEL: Record<SkillScope, string> = {
  builtin: "Builtin",
  global: "Global",
  user: "Mine",
};

// scopeBadgeTone maps a scope to a ui Badge tone (kept here so the page and the
// allocation panel stay visually consistent).
export function scopeBadgeTone(scope: SkillScope): "brand" | "info" | "neutral" {
  switch (scope) {
    case "builtin":
      return "brand";
    case "global":
      return "info";
    default:
      return "neutral";
  }
}
