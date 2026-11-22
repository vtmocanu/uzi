// Diagnostic-name sanitizing. This file has no imports.

/** The character ceiling on a sanitized diagnostic `name`, mirroring broker.ts's
 *  MAX_DIAGNOSTIC_CHARS. An over-long name is truncated with an ellipsis. */
const MAX_DIAGNOSTIC_NAME_CHARS = 120;

/** True for a code point that must not survive in a human-facing diagnostic `name`:
 *  a C0 control (0x00-0x1F, incl. TAB/LF/CR/ESC), DEL (0x7F), a C1 control
 *  (0x80-0x9F), or a Unicode bidi/format control (zero-width, line/paragraph
 *  separators, bidi embeddings/overrides/isolates, word joiner, BOM). MIRRORS
 *  broker.ts `isUnsafeIdentifierChar`. A code-point scan on purpose: oxlint's
 *  `no-control-regex` is denied, so a control-char regex is not an option. */
export function isUnsafeNameChar(cp: number): boolean {
  if (cp <= 0x1f) return true; // C0 controls incl. TAB/LF/CR/ESC
  if (cp === 0x7f) return true; // DEL
  if (cp >= 0x80 && cp <= 0x9f) return true; // C1 controls
  if (cp >= 0x200b && cp <= 0x200f) return true; // ZWSP..RLM
  if (cp === 0x2028 || cp === 0x2029) return true; // line / paragraph separators
  if (cp >= 0x202a && cp <= 0x202e) return true; // bidi embeddings / overrides
  if (cp >= 0x2060 && cp <= 0x2064) return true; // word joiner..invisible separator
  if (cp >= 0x2066 && cp <= 0x206f) return true; // bidi isolates + deprecated format
  return cp === 0xfeff; // BOM / ZWNBSP
}

/** Sanitize an untrusted, user-authored diagnostic `name` (an unknown tool/skill/
 *  effort/model identifier) so a control/newline/ANSI/bidi sequence can never forge
 *  or terminal-rewrite a downstream log/report line, whichever future consumer renders
 *  the diagnostic. Replaces every control and bidi/format code point (see
 *  {@link isUnsafeNameChar}) with the visible replacement char U+FFFD, then bounds the
 *  length. MIRRORS broker.ts `safeId`; pure and allocation-bounded (one code-point pass
 *  + one slice). Only the human `name` is routed through this — never the machine-facing
 *  `kind`/`role` shape or the tool canonicalization. */
export function sanitizeName(s: string): string {
  let out = "";
  for (const ch of s) {
    const cp = ch.codePointAt(0) ?? 0;
    out += isUnsafeNameChar(cp) ? "�" : ch;
  }
  return out.length > MAX_DIAGNOSTIC_NAME_CHARS
    ? `${out.slice(0, MAX_DIAGNOSTIC_NAME_CHARS)}…`
    : out;
}
