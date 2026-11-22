// The strings registered for redaction on behalf of a run's secret variables (PRD 4416 M2).
//
// A secret variable's value can reach run output re-encoded: JSON-escaped inside a tool result, percent-
// encoded in a URL, or base64-embedded in a larger string (a docker auth entry base64("user:password"), an
// HTTP Basic header). Plain (non-secret) variables are NOT registered: they are meant to be readable.
//
// Base64 embeds a value at an unknown byte offset, so the same value encodes three different ways
// depending on its offset modulo 3. For k = 0, 1, 2 the value is encoded behind k zero bytes, and only
// the characters whose six bits lie wholly inside the value bytes are kept: the leading characters that
// mix in the padding or the preceding bytes, and the trailing characters that mix in the following
// bytes or the final padding, depend on data the worker cannot know and are dropped. The same three
// alignments are also kept in the URL-safe alphabet.
//
// A multi-line value (a PEM key, a JSON credential file) is also split by line, but only for REDACTION: a
// line of it can appear alone in output and must be masked there. A line is never a scan needle, because a
// public line (`"type": "service_account",`) would hit every file that happens to carry it. The scan
// instead gets the value as it reads inside a unified diff, where every line after the first carries a
// `+`, `-` or ` ` prefix.

import { MIN_SECRET_LEN, providerKeyForms } from "./redact.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "./protocol.js";

/** The base64 characters that depend only on the value bytes when the value sits `offset` bytes into a
 *  base64 group (offset 0, 1 or 2). May be empty for a short value. */
function base64Interior(value: Buffer, offset: number): string {
  const padded = Buffer.concat([Buffer.alloc(offset), value]).toString("base64");
  const first = Math.ceil((8 * offset) / 6);
  const end = Math.floor((8 * (offset + value.length)) / 6);
  return padded.slice(first, end);
}

/** The whole-value forms of one secret: every encoding {@link providerKeyForms} lists (raw, JSON, JSON with
 *  `\/`, HTML-safe, percent-encoded) plus the three standard and the three URL-safe base64 alignments. */
function wholeValueForms(value: string): string[] {
  const forms = providerKeyForms(value);
  const bytes = Buffer.from(value, "utf8");
  for (let offset = 0; offset < 3; offset++) {
    const std = base64Interior(bytes, offset);
    forms.push(std, std.replaceAll("+", "-").replaceAll("/", "_"));
  }
  return forms;
}

const LINE_BREAK = /\r\n|\r|\n/;
const PEM_ARMOR = /^-----(BEGIN|END) [^-]+-----$/;

function isMultiLine(value: string): boolean {
  return value.includes("\r") || value.includes("\n");
}

/** The lines of a multi-line value that may be registered alone: long enough, and not PEM armor (the
 *  `-----BEGIN ...-----` lines are public text shared by every key). */
function lineForms(value: string): string[] {
  const out: string[] = [];
  for (const line of value.split(LINE_BREAK)) {
    if (line.length < MIN_SECRET_LEN || PEM_ARMOR.test(line)) continue;
    out.push(line, JSON.stringify(line).slice(1, -1));
  }
  return out;
}

/** A multi-line value as it reads inside a unified diff: its lines joined by each line break followed by one
 *  diff prefix (`+`, `-` or a space), with LF and CRLF breaks. Empty when the value is one line. */
function diffShapedForms(value: string): string[] {
  const lines = value.replace(/[\r\n]+$/, "").split(LINE_BREAK);
  if (lines.length < 2) return [];
  const out: string[] = [];
  for (const prefix of ["+", "-", " "]) {
    out.push(lines.join(`\n${prefix}`), lines.join(`\r\n${prefix}`));
  }
  return out;
}

function usable(forms: readonly string[]): string[] {
  return [...new Set(forms.filter((f) => f.length >= MIN_SECRET_LEN))];
}

/** The value as a file may hold it: as given, with its trailing line breaks stripped (a command
 *  substitution drops them, and a token often carries one it was never meant to), and, for a multi-line
 *  value, with every line break normalized to LF (a checkout normalizes CRLF), with and without the trailing
 *  line breaks. A variant shorter than {@link MIN_SECRET_LEN} is dropped, so it derives no encoded form. */
function valueVariants(value: string): string[] {
  const lf = value.replace(/\r\n?/g, "\n");
  return [...new Set([value, value.replace(/[\r\n]+$/, ""), lf, lf.replace(/\n+$/, "")])].filter((v) => v.length >= MIN_SECRET_LEN);
}

function variantForms(value: string): string[] {
  return valueVariants(value).flatMap(wholeValueForms);
}

/** Every form of one secret value to register for redaction: the whole-value forms of each variant of the
 *  value, plus each line of a multi-line value (raw and JSON-escaped). Forms shorter than
 *  {@link MIN_SECRET_LEN} are dropped. */
function redactionFormsOf(value: string): string[] {
  return usable([...variantForms(value), ...(isMultiLine(value) ? lineForms(value) : [])]);
}

/** Every form of one secret value to search for: the whole-value forms of each variant of the value, plus
 *  the diff-shaped forms of a multi-line value. Never a single line of a multi-line value. */
function scanFormsOf(value: string): string[] {
  return usable([...variantForms(value), ...(isMultiLine(value) ? diffShapedForms(value) : [])]);
}

/** One variable's scan needle: its NAME (the only text a finding may show) and every registered form of
 *  every secret value it has held. Plain values never appear. */
export interface ScanNeedle {
  readonly name: string;
  readonly forms: readonly string[];
}

/** The exact-value scan needles for a claim's (or the maintenance endpoint's) secret variables, grouped by
 *  name so a hit names the variable. A name with no usable form (every value too short) is dropped. */
export function runEnvScanNeedles(
  runEnv: readonly ClaimRunEnvVar[] | undefined,
  runEnvRedact: readonly ClaimRunEnvRedact[] | undefined,
): ScanNeedle[] {
  const byName = new Map<string, Set<string>>();
  const add = (name: string, value: string): void => {
    const set = byName.get(name) ?? new Set<string>();
    for (const f of scanFormsOf(value)) set.add(f);
    byName.set(name, set);
  };
  for (const v of runEnv ?? []) if (v.secret) add(v.name, v.value);
  for (const r of runEnvRedact ?? []) add(r.name, r.value);
  return [...byName]
    .filter(([, forms]) => forms.size > 0)
    .map(([name, forms]) => ({ name, forms: [...forms] }));
}

/** The deduplicated registration list for a claim's secret variables: every live secret and every
 *  run_env_redact entry. Longest first, so a longer form is replaced before a shorter one inside it. */
export function runEnvRedactionForms(
  runEnv: readonly ClaimRunEnvVar[] | undefined,
  runEnvRedact: readonly ClaimRunEnvRedact[] | undefined,
): string[] {
  const out = new Set<string>();
  for (const v of runEnv ?? []) if (v.secret) for (const f of redactionFormsOf(v.value)) out.add(f);
  for (const r of runEnvRedact ?? []) for (const f of redactionFormsOf(r.value)) out.add(f);
  return [...out].sort((a, b) => b.length - a.length);
}
