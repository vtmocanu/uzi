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
// A multi-line value (a PEM key, a JSON credential file) is also split by line: every line of at least
// MIN_SECRET_LEN characters, except a standard PEM marker line (PEM_STANDARD_LABELS, exact match) and a line of
// only ASCII spaces and tabs, is registered for REDACTION and for
// the preserved-patch line check, because a line of it can appear alone in output. A line is never a scan
// needle. The scan instead gets the value as it reads inside a unified diff, where every line after the
// first carries a `+`, `-` or ` ` prefix.

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

/** The standard PEM labels (RFC 7468 and the common OpenSSL/OpenSSH ones). A line that is exactly
 *  `-----BEGIN <label>-----` or `-----END <label>-----` for one of these is public armor and is not registered
 *  alone. Any other label, or any extra character on the line, keeps the line's normal protection. */
export const PEM_STANDARD_LABELS: readonly string[] = [
  "PRIVATE KEY", "RSA PRIVATE KEY", "EC PRIVATE KEY", "DSA PRIVATE KEY", "ENCRYPTED PRIVATE KEY", "OPENSSH PRIVATE KEY",
  "PUBLIC KEY", "RSA PUBLIC KEY", "CERTIFICATE", "TRUSTED CERTIFICATE", "CERTIFICATE REQUEST", "NEW CERTIFICATE REQUEST",
  "X509 CRL", "PKCS7", "CMS", "EC PARAMETERS", "DH PARAMETERS", "ATTRIBUTE CERTIFICATE",
];
const PEM_MARKER_LINES: ReadonlySet<string> = new Set(
  PEM_STANDARD_LABELS.flatMap((label) => [`-----BEGIN ${label}-----`, `-----END ${label}-----`]),
);
/** Only ASCII space and tab: a line of any other (Unicode) space is a normal line form. */
const WHITESPACE_ONLY = /^[ \t]*$/;

function isMultiLine(value: string): boolean {
  return value.includes("\r") || value.includes("\n");
}

/** The lines of a multi-line value that are registered alone: every line at least {@link MIN_SECRET_LEN} long
 *  except a standard PEM marker line (exactly one of {@link PEM_STANDARD_LABELS}) and a line of only ASCII spaces
 *  and tabs. No condition on letter case, token variety, digits or key name. */
function lineForms(value: string): string[] {
  const out: string[] = [];
  for (const line of value.split(LINE_BREAK)) {
    if (line.length < MIN_SECRET_LEN || PEM_MARKER_LINES.has(line) || WHITESPACE_ONLY.test(line)) continue;
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

/** The value as a file may hold it: as given, with its trailing line breaks stripped, and, for a multi-line
 *  value, with every line break normalized to LF (a checkout normalizes CRLF), with and without the trailing
 *  line breaks. A variant shorter than {@link MIN_SECRET_LEN} is dropped, so it derives no encoded form. */
function valueVariants(value: string): string[] {
  const lf = value.replace(/\r\n?/g, "\n");
  return [...new Set([value, value.replace(/[\r\n]+$/, ""), lf, lf.replace(/\n+$/, "")])].filter((v) => v.length >= MIN_SECRET_LEN);
}

/** One variable's scan needle: its NAME (the only text a finding may show) and every registered form of
 *  every secret value it has held. Plain values never appear. */
export interface ScanNeedle {
  readonly name: string;
  readonly forms: readonly string[];
}

/** The most UTF-16 units of forms one claim may register, every distinct form and logger copy counted once
 *  (see {@link RunEnvClaimForms.formChars}). */
export const RUN_ENV_FORM_CHARS_MAX = 40_000_000;

/** The reason text of a claim whose forms passed {@link RUN_ENV_FORM_CHARS_MAX}. */
export const RUN_ENV_FORMS_OVER_BUDGET = "run_env_forms_over_budget";

/** The forms of one claim's secret variables, computed once and shared by every consumer. */
export interface RunEnvClaimForms {
  /** Every form to register for redaction, longest first. When {@link overBudget}, the raw values (and their
   *  trimmed and LF-normalized variants) only. */
  readonly redaction: string[];
  /** The exact-value scan needles, grouped by variable name. Empty when over budget. */
  readonly needles: ScanNeedle[];
  /** Per variable name, the registered line forms of its multi-line values. Empty when over budget. */
  readonly lineForms: ScanNeedle[];
  /** The UTF-16 units of distinct strings counted: every form, and for each form registered for redaction its
   *  JSON-escaped copy when that differs (the copy the logger indexes beside the form). Needle-only forms add no
   *  copy. The count is checked after each value variant's whole-value
   *  forms and after a multi-line value's line and diff forms, so building stops after the group whose forms pass
   *  {@link RUN_ENV_FORM_CHARS_MAX} and the count can exceed the cap by that group's forms. */
  readonly formChars: number;
  /** Building stopped because {@link RUN_ENV_FORM_CHARS_MAX} was passed; the redaction list then holds only each value's raw variants, and the needles and line forms are empty. */
  readonly overBudget: boolean;
}

const named = (sets: ReadonlyMap<string, ReadonlySet<string>>): ScanNeedle[] =>
  [...sets].filter(([, forms]) => forms.size > 0).map(([name, forms]) => ({ name, forms: [...forms] }));

/** The forms of a claim's (or the maintenance endpoint's) secret variables: every live secret and every
 *  run_env_redact entry. Each value's variants and whole-value encodings are built once and feed both the
 *  redaction list and the scan needles. Building stops as soon as the distinct forms pass the budget. */
export function runEnvClaimForms(
  runEnv: readonly ClaimRunEnvVar[] | undefined,
  runEnvRedact: readonly ClaimRunEnvRedact[] | undefined,
): RunEnvClaimForms {
  const entries: Array<{ name: string; value: string }> = [];
  for (const v of runEnv ?? []) if (v.secret) entries.push({ name: v.name, value: v.value });
  for (const r of runEnvRedact ?? []) entries.push({ name: r.name, value: r.value });

  const redaction = new Set<string>();
  const needleSets = new Map<string, Set<string>>();
  const lineSets = new Map<string, Set<string>>();
  const counted = new Set<string>();
  let chars = 0;
  const forName = (sets: Map<string, Set<string>>, name: string): Set<string> => {
    const set = sets.get(name) ?? new Set<string>();
    sets.set(name, set);
    return set;
  };
  const count = (s: string): void => {
    if (counted.has(s)) return;
    counted.add(s);
    chars += s.length;
  };
  const register = (forms: readonly string[], targets: readonly Set<string>[]): void => {
    for (const f of forms) {
      if (f.length < MIN_SECRET_LEN) continue;
      for (const t of targets) t.add(f);
      count(f);
      if (targets.includes(redaction)) {
        const escaped = JSON.stringify(f).slice(1, -1);
        if (escaped !== f) count(escaped);
      }
    }
  };

  for (const { name, value } of entries) {
    const needle = forName(needleSets, name);
    const multi = isMultiLine(value);
    for (const variant of valueVariants(value)) {
      register(wholeValueForms(variant), [redaction, needle]);
      if (chars > RUN_ENV_FORM_CHARS_MAX) return overBudgetForms(entries, chars);
    }
    if (multi) {
      register(lineForms(value), [redaction, forName(lineSets, name)]);
      register(diffShapedForms(value), [needle]);
      if (chars > RUN_ENV_FORM_CHARS_MAX) return overBudgetForms(entries, chars);
    }
  }
  return {
    redaction: [...redaction].sort((a, b) => b.length - a.length),
    needles: named(needleSets),
    lineForms: named(lineSets),
    formChars: chars,
    overBudget: false,
  };
}

/** The distinct names of the claim's multi-line secrets, with no length floor: live `secret: true` run_env
 *  entries and run_env_redact versions whose value holds a CR or LF. A run with any is never given a preserved patch. */
export function multiLineSecretVariables(
  runEnv: readonly ClaimRunEnvVar[] | undefined,
  runEnvRedact: readonly ClaimRunEnvRedact[] | undefined,
): string[] {
  const names = new Set<string>();
  for (const v of runEnv ?? []) if (v.secret && isMultiLine(v.value)) names.add(v.name);
  for (const r of runEnvRedact ?? []) if (isMultiLine(r.value)) names.add(r.name);
  return [...names];
}

function overBudgetForms(entries: ReadonlyArray<{ value: string }>, formChars: number): RunEnvClaimForms {
  const raw = new Set<string>();
  for (const { value } of entries) for (const v of valueVariants(value)) raw.add(v);
  return { redaction: [...raw].sort((a, b) => b.length - a.length), needles: [], lineForms: [], formChars, overBudget: true };
}
