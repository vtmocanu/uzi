// Registered forms of the secret repository variables a run holds (PRD 4416 M2, "Registered forms").
// The run's redactors match exact strings, so each secret is registered in the shapes it takes when
// a tool echoes or stores it: the value itself; its JSON-escaped form (log.ts scrubs after
// JSON.stringify, so a value holding a quote, a backslash or a newline would otherwise be missed);
// and the three base64 alignments of the value, each trimmed to the characters that do not depend
// on the bytes next to it, so the secret is caught inside any base64 stream (the user:password
// entry a registry login stores, an HTTP Basic header). Plain values are never registered.

import { MIN_SECRET_LEN } from "./redact.js";
import type { RunEnvRedactEntry, RunEnvVar } from "./run-env-claim.js";

/** The forms of one secret value; forms shorter than MIN_SECRET_LEN are dropped (an exact match on
 *  a short string would corrupt unrelated output). Deduplicated, in a stable order. */
export function registeredForms(value: string): string[] {
  const forms: string[] = [value];
  const escaped = JSON.stringify(value).slice(1, -1);
  if (escaped !== value) forms.push(escaped);
  const bytes = Buffer.from(value, "utf8");
  for (let k = 0; k <= 2; k++) {
    const b64 = Buffer.concat([Buffer.alloc(k), bytes]).toString("base64");
    // Output character i holds bits [6i, 6i+6) of the stream; the value spans bits [8k, 8k+8n).
    // Keep a character only when all six of its bits fall inside that span.
    const first = Math.ceil((8 * k) / 6);
    const end = Math.floor((8 * (k + bytes.length)) / 6);
    if (end > first) forms.push(b64.slice(first, end));
  }
  return [...new Set(forms.filter((f) => f.length >= MIN_SECRET_LEN))];
}

/** Every registered form of every secret in a claim's run_env and run_env_redact. Secret entries
 *  only; plain values are not redacted. */
export function runEnvSecretForms(secrets: {
  run_env?: readonly RunEnvVar[];
  run_env_redact?: readonly RunEnvRedactEntry[];
}): string[] {
  const values = new Set<string>();
  for (const v of secrets.run_env ?? []) if (v.secret) values.add(v.value);
  for (const v of secrets.run_env_redact ?? []) values.add(v.value);
  const out = new Set<string>();
  for (const value of values) for (const f of registeredForms(value)) out.add(f);
  return [...out];
}
