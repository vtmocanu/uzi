// The strings registered for redaction on behalf of a run's secret variables (PRD 4416 M2).
//
// A secret variable's value can reach run output re-encoded: JSON-escaped inside a tool result, or
// base64-embedded in a larger string (a docker auth entry base64("user:password"), an HTTP Basic
// header). Plain (non-secret) variables are NOT registered: they are meant to be readable.
//
// Base64 embeds a value at an unknown byte offset, so the same value encodes three different ways
// depending on its offset modulo 3. For k = 0, 1, 2 the value is encoded behind k zero bytes, and only
// the characters whose six bits lie wholly inside the value bytes are kept: the leading characters that
// mix in the padding or the preceding bytes, and the trailing characters that mix in the following
// bytes or the final padding, depend on data the worker cannot know and are dropped.

import { MIN_SECRET_LEN } from "./redact.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "./protocol.js";

/** The base64 characters that depend only on the value bytes when the value sits `offset` bytes into a
 *  base64 group (offset 0, 1 or 2). May be empty for a short value. */
function base64Interior(value: Buffer, offset: number): string {
  const padded = Buffer.concat([Buffer.alloc(offset), value]).toString("base64");
  const first = Math.ceil((8 * offset) / 6);
  const end = Math.floor((8 * (offset + value.length)) / 6);
  return padded.slice(first, end);
}

/** Every form of one secret value to register: the value, its JSON-escaped form when different, and the
 *  three base64 alignments. Forms shorter than {@link MIN_SECRET_LEN} are dropped. */
export function secretForms(value: string): string[] {
  const forms = [value];
  const json = JSON.stringify(value).slice(1, -1);
  if (json !== value) forms.push(json);
  const bytes = Buffer.from(value, "utf8");
  for (let offset = 0; offset < 3; offset++) forms.push(base64Interior(bytes, offset));
  return forms.filter((f) => f.length >= MIN_SECRET_LEN);
}

/** One variable's scan needle: its NAME (the only text a finding may show) and every registered form of
 *  every secret value it has held. Plain values never appear. */
export interface ScanNeedle {
  readonly name: string;
  readonly forms: readonly string[];
}

/** The exact-value scan needles for a claim's (or the maintenance endpoint's) secret variables, grouped by
 *  name so a hit names the variable. Same forms as {@link runEnvRedactionForms}; a name with no usable form
 *  (every value too short) is dropped. */
export function runEnvScanNeedles(
  runEnv: readonly ClaimRunEnvVar[] | undefined,
  runEnvRedact: readonly ClaimRunEnvRedact[] | undefined,
): ScanNeedle[] {
  const byName = new Map<string, Set<string>>();
  const add = (name: string, value: string): void => {
    const set = byName.get(name) ?? new Set<string>();
    for (const f of secretForms(value)) set.add(f);
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
  for (const v of runEnv ?? []) if (v.secret) for (const f of secretForms(v.value)) out.add(f);
  for (const r of runEnvRedact ?? []) for (const f of secretForms(r.value)) out.add(f);
  return [...out].sort((a, b) => b.length - a.length);
}
