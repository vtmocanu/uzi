// Fail-closed decoder for the run-env part of a claim (PRD #4416, "Claim" and "Worker").
// Mirrors the Go types in api/internal/workersvc/run_env_types.go and the limits in
// api/internal/runenv/values.go (MaxVarsPerScope, MaxScopeValueBytes, MaxValueBytes,
// MaxRetiredVersions, MaxRetiredValueBytes, MinSecretLen). Pure: it is wired into the claim
// client by a later unit. Error messages name the field and the rule, never a value.

import type { ClaimRunEnvRedact, ClaimRunEnvVar, RunEnvRedactResponse } from "./protocol.js";

/** The protocol capability a worker advertises once it can consume `run_env`. */
export const RUN_ENV_V1 = "run_env_v1";

export const RUN_ENV_MAX_LIVE = 50;
export const RUN_ENV_MAX_VALUE_BYTES = 65536;
export const RUN_ENV_MAX_LIVE_BYTES = 262144;
export const RUN_ENV_MAX_REDACT = 200;
export const RUN_ENV_MAX_REDACT_BYTES = 2097152;
/** Equal to MIN_SECRET_LEN in redact.ts (not exported there; test/run-env-claim.test.ts pins the
 *  two together by behaviour). Counted as a JS string length (UTF-16 code units), as redact.ts does. */
export const RUN_ENV_MIN_SECRET_LEN = 8;

const NAME_RE = /^[A-Z_][A-Z0-9_]{0,127}$/;
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const LONE_SURROGATE_RE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;

export class RunEnvClaimError extends Error {
  constructor(message: string) {
    super(`run_env claim: ${message}`);
    this.name = "RunEnvClaimError";
  }
}

function fail(where: string, rule: string): never {
  throw new RunEnvClaimError(`${where}: ${rule}`);
}

function record(raw: unknown, where: string): Record<string, unknown> {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) fail(where, "must be an object");
  return raw as Record<string, unknown>;
}

function str(o: Record<string, unknown>, key: string, where: string): string {
  const v = o[key];
  if (typeof v !== "string") fail(`${where}.${key}`, "must be a string");
  return v;
}

/** Decodes the fields shared by live and redact entries; returns the value's UTF-8 byte length. */
function common(raw: unknown, where: string): { v: ClaimRunEnvRedact; bytes: number; o: Record<string, unknown> } {
  const o = record(raw, where);
  const var_id = str(o, "var_id", where);
  if (!UUID_RE.test(var_id)) fail(`${where}.var_id`, "must be a UUID");
  const scope = str(o, "scope", where);
  if (scope !== "repo" && scope !== "user") fail(`${where}.scope`, 'must be "repo" or "user"');
  const rev = o.rev;
  if (typeof rev !== "number" || !Number.isInteger(rev) || rev < 1) fail(`${where}.rev`, "must be a positive integer");
  const name = str(o, "name", where);
  if (!NAME_RE.test(name)) fail(`${where}.name`, "must match ^[A-Z_][A-Z0-9_]{0,127}$");
  const value = str(o, "value", where);
  if (value.includes("\u0000")) fail(`${where}.value`, "must not contain NUL");
  if (LONE_SURROGATE_RE.test(value)) fail(`${where}.value`, "must be valid UTF-8");
  const bytes = Buffer.byteLength(value, "utf8");
  if (bytes > RUN_ENV_MAX_VALUE_BYTES) fail(`${where}.value`, `must be at most ${RUN_ENV_MAX_VALUE_BYTES} bytes`);
  return { v: { var_id, scope, rev, name, value }, bytes, o };
}

function requireMaskable(value: string, where: string): void {
  if (value.length < RUN_ENV_MIN_SECRET_LEN) {
    fail(`${where}.value`, `a secret must be at least ${RUN_ENV_MIN_SECRET_LEN} characters so it can be masked`);
  }
}

function list(raw: unknown, where: string, max: number): unknown[] {
  if (raw === undefined) return [];
  if (!Array.isArray(raw)) fail(where, "must be an array");
  if (raw.length > max) fail(where, `must have at most ${max} entries`);
  return raw;
}

function decodeRedact(raw: unknown, where: string, seen: Set<string>): { items: ClaimRunEnvRedact[]; bytes: number } {
  const arr = list(raw, where, RUN_ENV_MAX_REDACT);
  const items: ClaimRunEnvRedact[] = [];
  let total = 0;
  arr.forEach((e, i) => {
    const w = `${where}[${i}]`;
    const { v, bytes } = common(e, w);
    requireMaskable(v.value, w);
    const key = `${v.var_id.toLowerCase()}#${v.rev}`;
    if (seen.has(key)) fail(w, "duplicate (var_id, rev)");
    seen.add(key);
    total += bytes;
    if (total > RUN_ENV_MAX_REDACT_BYTES) fail(where, `values must total at most ${RUN_ENV_MAX_REDACT_BYTES} bytes`);
    items.push(v as ClaimRunEnvRedact);
  });
  return { items, bytes: total };
}

/** Decodes `secrets.run_env` and `secrets.run_env_redact` (the `secrets` object's own fields, in
 *  `raw`). Absent fields decode to empty arrays; any violation throws RunEnvClaimError. */
export function decodeRunEnvSecrets(raw: unknown): { runEnv: ClaimRunEnvVar[]; runEnvRedact: ClaimRunEnvRedact[] } {
  const o = record(raw, "secrets");
  const seen = new Set<string>();
  const liveArr = list(o.run_env, "run_env", RUN_ENV_MAX_LIVE);
  const runEnv: ClaimRunEnvVar[] = [];
  let total = 0;
  liveArr.forEach((e, i) => {
    const w = `run_env[${i}]`;
    const { v, bytes, o: eo } = common(e, w);
    if (typeof eo.secret !== "boolean") fail(`${w}.secret`, "must be a boolean");
    const description = str(eo, "description", w);
    if (eo.secret) requireMaskable(v.value, w);
    const key = `${v.var_id.toLowerCase()}#${v.rev}`;
    if (seen.has(key)) fail(w, "duplicate (var_id, rev)");
    seen.add(key);
    total += bytes;
    if (total > RUN_ENV_MAX_LIVE_BYTES) fail("run_env", `values must total at most ${RUN_ENV_MAX_LIVE_BYTES} bytes`);
    runEnv.push({ ...v, secret: eo.secret, description });
  });
  const { items } = decodeRedact(o.run_env_redact, "run_env_redact", seen);
  return { runEnv, runEnvRedact: items };
}

/** Decodes the run-env redact maintenance response. `protected: false` must carry no versions. */
export function decodeRunEnvRedactResponse(raw: unknown): Required<Pick<RunEnvRedactResponse, "protected">> & { versions: ClaimRunEnvRedact[] } {
  const o = record(raw, "response");
  if (typeof o.protected !== "boolean") fail("protected", "must be a boolean");
  const { items } = decodeRedact(o.versions, "versions", new Set());
  if (!o.protected && items.length > 0) fail("versions", "must be absent or empty when protected is false");
  return { protected: o.protected, versions: items };
}
