// Text helpers for the repository variables a run receives (PRD 4416 M2): the sanitiser applied
// to user-authored description and plain-value text before it reaches a prompt, and the drop
// reporter the environment builders use. User text is untrusted: a description can carry ANSI
// sequences, line breaks and bidi controls. prompt.ts renders the block (buildRunEnvBlock).

import { isUnsafeNameChar, sanitizeName } from "./codex/render.js";
import type { RunEnvInput } from "./run-env-merge.js";
import type { RunEnvVar } from "./run-env-claim.js";

/** The longest rendered description or plain value, in UTF-8 bytes. */
export const RUN_ENV_PROMPT_MAX_BYTES = 1024;

const ESC = 0x1b;
const BEL = 0x07;
const REPLACEMENT = "�";
const ELLIPSIS = "…";

/** Removes ANSI escape sequences whole: CSI (`ESC [ ... final 0x40-0x7E`), OSC (`ESC ] ...` up to
 *  BEL or `ESC \`; an unterminated one removes the rest of the text) and the two-character
 *  `ESC x`. A malformed CSI leaves its ESC for the replacement step. A scan rather than a regex
 *  because oxlint denies control characters in a regex. */
function stripAnsi(s: string): string {
  let out = "";
  let i = 0;
  while (i < s.length) {
    if (s.charCodeAt(i) !== ESC) { out += s[i]; i++; continue; }
    const next = s.charCodeAt(i + 1);
    if (Number.isNaN(next)) { out += s[i]; i++; continue; }
    if (next === 0x5b) { // CSI
      let j = i + 2;
      while (j < s.length && s.charCodeAt(j) >= 0x20 && s.charCodeAt(j) <= 0x3f) j++;
      const fin = s.charCodeAt(j);
      if (fin >= 0x40 && fin <= 0x7e) { i = j + 1; continue; }
      out += s[i]; i++; // malformed: keep the ESC so it becomes U+FFFD
      continue;
    }
    if (next === 0x5d) { // OSC
      let j = i + 2;
      while (j < s.length) {
        const c = s.charCodeAt(j);
        if (c === BEL) { j++; break; }
        if (c === ESC && s.charCodeAt(j + 1) === 0x5c) { j += 2; break; }
        j++;
      }
      i = j;
      continue;
    }
    if (next === ESC) { out += s[i]; i++; continue; }
    i += 2; // ESC x
  }
  return out;
}

/** Cuts `s` to at most `maxBytes` of UTF-8 on a code point boundary, ending in an ellipsis when cut. */
function boundBytes(s: string, maxBytes: number): string {
  if (Buffer.byteLength(s, "utf8") <= maxBytes) return s;
  const budget = maxBytes - Buffer.byteLength(ELLIPSIS, "utf8");
  let out = "";
  let used = 0;
  for (const ch of s) {
    const n = Buffer.byteLength(ch, "utf8");
    if (used + n > budget) break;
    out += ch;
    used += n;
  }
  return out + ELLIPSIS;
}

/** Makes user-authored text safe to render in a prompt line: ANSI sequences removed whole, every
 *  control and bidi/format code point replaced with U+FFFD, then bounded to 1024 bytes. */
export function sanitizeRunEnvPromptText(s: string): string {
  let out = "";
  for (const ch of stripAnsi(s)) {
    out += isUnsafeNameChar(ch.codePointAt(0) ?? 0) ? REPLACEMENT : ch;
  }
  return boundBytes(out, RUN_ENV_PROMPT_MAX_BYTES);
}

/** Renders a plain value as a JSON string literal with every code point the sanitiser would
 *  replace written as \uXXXX, so the value stays exact and nothing renders as a control. */
export function renderPlainValue(value: string): string {
  const cut = boundBytes(value, RUN_ENV_PROMPT_MAX_BYTES);
  let out = "";
  for (const ch of JSON.stringify(cut)) {
    const cp = ch.codePointAt(0) ?? 0;
    out += isUnsafeNameChar(cp) ? `\\u${cp.toString(16).padStart(4, "0")}` : ch;
  }
  return out;
}

/** One list line of the prompt block for `v`. A secret's value is never read here. */
export function renderRunEnvLine(v: RunEnvVar): string {
  const parts = [`- ${sanitizeName(v.name)} (${v.scope} ${v.secret ? "secret" : "plain"})`];
  const description = sanitizeRunEnvPromptText(v.description);
  if (description !== "") parts.push(`: ${description}`);
  if (!v.secret) parts.push(` | value: ${renderPlainValue(v.value)}`);
  return parts.join("");
}

/** A RunEnvInput whose drops are emitted once per name through `say` (a worker status line).
 *  The name passes sanitizeName; no value is ever included. Undefined for an empty set. */
export function runEnvInput(vars: readonly RunEnvVar[] | undefined, say: (text: string) => void): RunEnvInput | undefined {
  if (!vars || vars.length === 0) return undefined;
  const reported = new Set<string>();
  return {
    vars,
    onDrop: (name, rule) => {
      if (reported.has(name)) return;
      reported.add(name);
      say(`repository variable ${sanitizeName(name)} was not set: ${rule}`);
    },
  };
}
