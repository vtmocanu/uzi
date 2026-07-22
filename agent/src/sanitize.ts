/**
 * Worker-side stripping of the codepoints Postgres `jsonb` cannot represent.
 *
 * **This is defense in depth and explicitly NOT the mechanism** (PRD #108 Decision
 * 1). The authoritative strip is server-side in `workersvc.AppendMessages`, because
 * a worker-only fix protects only workers running the new image. Design nothing on
 * the assumption this ran.
 *
 * Two classes, and only two — `\n`, `\t` and ANSI escapes are legal in `jsonb` and
 * load-bearing in tool output, so stripping the wider control class would mangle
 * every log the UI renders:
 *
 *  - **U+0000.** The incident's trigger: a headless Chromium's stderr carried 84
 *    NUL bytes (HarfBuzz spew) into a `tool_result`, and `jsonb` rejected the
 *    insert with SQLSTATE 22P05.
 *  - **Unpaired surrogates U+D800-U+DFFF.** `jsonb` rejects these just as hard, and
 *    they are the realistic *Node-side* trigger: `JSON.stringify` emits a lone
 *    `\udXXX` escape whenever a JS string was sliced mid-surrogate-pair, so any
 *    worker-side truncation of tool output can mint one. Replaced with U+FFFD
 *    rather than dropped, so the text keeps its shape.
 *
 * Well-formed surrogate PAIRS are left exactly as they are — every emoji and every
 * astral-plane character is a pair, and mangling those would corrupt ordinary text.
 */

/** U+FFFD REPLACEMENT CHARACTER. */
const REPLACEMENT = "\ufffd";

/**
 * True if the string contains anything worth scanning. Well-formed pairs match too
 * (their halves are in the surrogate range), so this is a cheap over-approximation:
 * it decides whether to walk, never what to replace.
 */
const NEEDS_SCAN = /[\u0000\ud800-\udfff]/;

export interface SanitizeCounts {
  /** U+0000 codepoints removed. */
  nul: number;
  /** Unpaired surrogate halves replaced with U+FFFD. */
  surrogate: number;
}

export function emptyCounts(): SanitizeCounts {
  return { nul: 0, surrogate: 0 };
}

export function countsTotal(c: SanitizeCounts): number {
  return c.nul + c.surrogate;
}

/**
 * Strip NULs and replace unpaired surrogates in one string. Returns the input by
 * identity when there is nothing to do, so the overwhelmingly common message pays
 * one regex test and no allocation.
 */
export function sanitizeText(s: string, counts: SanitizeCounts): string {
  if (!NEEDS_SCAN.test(s)) return s;
  let out = "";
  for (let i = 0; i < s.length; i++) {
    const code = s.charCodeAt(i);
    if (code === 0) {
      counts.nul += 1;
      continue;
    }
    if (code >= 0xd800 && code <= 0xdbff) {
      // A high half is only legal immediately before a low half.
      const next = i + 1 < s.length ? s.charCodeAt(i + 1) : 0;
      if (next >= 0xdc00 && next <= 0xdfff) {
        out += s[i]! + s[i + 1]!;
        i += 1;
        continue;
      }
      counts.surrogate += 1;
      out += REPLACEMENT;
      continue;
    }
    if (code >= 0xdc00 && code <= 0xdfff) {
      // A low half not consumed by the branch above is unpaired by construction.
      counts.surrogate += 1;
      out += REPLACEMENT;
      continue;
    }
    out += s[i];
  }
  return out;
}

/**
 * Deep-sanitize a payload: every string VALUE and every object KEY. Keys matter as
 * much as values — a NUL in a key breaks the `jsonb` insert exactly the same way,
 * and a key is just as reachable from tool output as a value.
 *
 * Structure, numbers, booleans and nulls pass through untouched.
 */
export function sanitizePayload(
  payload: Record<string, unknown>,
  counts: SanitizeCounts,
): Record<string, unknown> {
  return walk(payload, counts) as Record<string, unknown>;
}

function walk(value: unknown, counts: SanitizeCounts): unknown {
  if (typeof value === "string") return sanitizeText(value, counts);
  if (Array.isArray(value)) return value.map((v) => walk(v, counts));
  if (value && typeof value === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      out[sanitizeText(k, counts)] = walk(v, counts);
    }
    return out;
  }
  return value;
}
