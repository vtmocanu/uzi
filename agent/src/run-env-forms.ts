// Registered forms of a run-env secret (PRD #4416, "Registered forms").
//
// A secret value can leave the worker in more than one spelling: raw, JSON-escaped (agent/src/log.ts
// scrubs after JSON.stringify), or inside a base64 stream (a Docker config `auth` entry is
// base64("user:password"), an HTTP Basic header is the same). This derives every spelling the
// redactors should register. Pure; plain (non-secret) values are never passed in.

import { MIN_SECRET_LEN } from "./redact.js";
import type { ClaimRunEnvVar, ClaimSecrets } from "./protocol.js";

/** The characters of base64(k zero bytes + utf8(value)) that depend only on value bytes: the leading
 *  characters touching the zero padding and the trailing ones touching the end are dropped, so the
 *  result is found inside a longer stream whatever bytes sit before or after the value. */
function base64Alignment(value: string, k: number): string {
  const encoded = Buffer.concat([Buffer.alloc(k), Buffer.from(value, "utf8")]).toString("base64").replace(/=+$/, "");
  const bits = 8 * (k + Buffer.byteLength(value, "utf8"));
  return encoded.slice(Math.ceil((8 * k) / 6), Math.floor(bits / 6));
}

/** Every form to register for the given secret values: the value, its JSON-escaped form when it
 *  differs, and the three base64 alignments; forms shorter than the masking floor are dropped; deduped. */
export function runEnvSecretForms(secrets: string[]): string[] {
  const forms = new Set<string>();
  for (const value of secrets) {
    const candidates = [value];
    const json = JSON.stringify(value).slice(1, -1);
    if (json !== value) candidates.push(json);
    for (let k = 0; k < 3; k++) candidates.push(base64Alignment(value, k));
    for (const f of candidates) if (f.length >= MIN_SECRET_LEN) forms.add(f);
  }
  return [...forms];
}

/** The registered forms of one variable name (every version of it the run holds), for the exact-value
 *  scan, which names the variable on a hit. Carries a value-derived form, so it never leaves the worker. */
export interface RunEnvVarForms {
  readonly name: string;
  readonly forms: readonly string[];
}
export type RunEnvNamedForms = readonly RunEnvVarForms[];

/** Groups secret values by variable name into scan forms; a name with no scannable form is dropped. */
export function runEnvNamedForms(entries: ReadonlyArray<{ name: string; value: string }>): RunEnvNamedForms {
  const byName = new Map<string, string[]>();
  for (const e of entries) {
    const list = byName.get(e.name) ?? [];
    list.push(e.value);
    byName.set(e.name, list);
  }
  const out: RunEnvVarForms[] = [];
  for (const [name, values] of byName) {
    const forms = runEnvSecretForms(values);
    if (forms.length > 0) out.push({ name, forms });
  }
  return out;
}

/** {@link claimRunEnvSecretForms} grouped by variable name, for the exact-value scan. */
export function claimRunEnvNamedForms(secrets: Pick<ClaimSecrets, "run_env" | "run_env_redact">): RunEnvNamedForms {
  const entries: Array<{ name: string; value: string }> = [];
  for (const v of secrets.run_env ?? []) if ((v as ClaimRunEnvVar).secret) entries.push(v);
  for (const v of secrets.run_env_redact ?? []) entries.push(v);
  return runEnvNamedForms(entries);
}

/** The forms to register for a claim: every `secret: true` live entry plus every redact-only entry.
 *  Plain values are never registered. */
export function claimRunEnvSecretForms(secrets: Pick<ClaimSecrets, "run_env" | "run_env_redact">): string[] {
  const values: string[] = [];
  for (const v of secrets.run_env ?? []) if ((v as ClaimRunEnvVar).secret) values.push(v.value);
  for (const v of secrets.run_env_redact ?? []) values.push(v.value);
  return runEnvSecretForms(values);
}
