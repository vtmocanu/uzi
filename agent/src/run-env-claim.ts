// Fail-closed decoder for the run-env part of a claim (PRD #4416, "Claim" and "Worker").
// Mirrors the Go types in api/internal/workersvc/run_env_types.go and the limits in
// api/internal/runenv/values.go (MaxVarsPerScope, MaxScopeValueBytes, MaxValueBytes,
// MaxRetiredVersions, MaxRetiredValueBytes, MinSecretLen). Pure: it is wired into the claim
// client by a later unit. Error messages name the field and the rule, never a value.

import type { ClaimRunEnvRedact, ClaimRunEnvVar, RunEnvRedactResponse } from "./protocol.js";
import { RUN_ENV_NAME_REGEXP } from "./run-env-names.js";

/** The protocol capability a worker advertises once it can consume `run_env`. */
export const RUN_ENV_V1 = "run_env_v1";

// Pinned to fixtures/run-env/limits.json (and to the Go constants) by test/run-env-claim.test.ts.
/** Live variables per scope. */
export const RUN_ENV_MAX_LIVE = 50;
export const RUN_ENV_MAX_VALUE_BYTES = 65536;
/** Live value bytes per scope. */
export const RUN_ENV_MAX_LIVE_BYTES = 262144;
export const RUN_ENV_MAX_DESCRIPTION_BYTES = 1024;
/** Retired versions per scope (the Go MaxRetiredVersions / MaxRetiredValueBytes). */
const RUN_ENV_MAX_RETIRED = 200;
const RUN_ENV_MAX_RETIRED_BYTES = 2097152;
/** The redact list also carries disabled, pending-deletion and shadowed live versions, which
 *  count under the live limits, so it is bounded per scope at retired + live. */
export const RUN_ENV_MAX_REDACT = RUN_ENV_MAX_RETIRED + RUN_ENV_MAX_LIVE;
export const RUN_ENV_MAX_REDACT_BYTES = RUN_ENV_MAX_RETIRED_BYTES + RUN_ENV_MAX_LIVE_BYTES;
/** Scopes the decoder accepts; every cap above applies to each scope separately. */
const SCOPE_COUNT = 2;
/** Equal to MIN_SECRET_LEN in redact.ts (not exported there; test/run-env-claim.test.ts pins the
 *  two together by behaviour). Counted as a JS string length (UTF-16 code units), as redact.ts does. */
export const RUN_ENV_MIN_SECRET_LEN = 8;

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

/** Decodes the fields shared by live and redact entries; also returns the value's UTF-8 byte length. */
function common(raw: unknown, where: string): { v: ClaimRunEnvRedact; bytes: number; o: Record<string, unknown> } {
  const o = record(raw, where);
  const var_id = str(o, "var_id", where);
  if (!UUID_RE.test(var_id)) fail(`${where}.var_id`, "must be a UUID");
  const scope = str(o, "scope", where);
  if (scope !== "repo" && scope !== "user") fail(`${where}.scope`, 'must be "repo" or "user"');
  const rev = o.rev;
  if (typeof rev !== "number" || !Number.isSafeInteger(rev) || rev < 1) fail(`${where}.rev`, "must be a positive safe integer");
  const name = str(o, "name", where);
  if (!RUN_ENV_NAME_REGEXP.test(name)) fail(`${where}.name`, "must match ^[A-Z_][A-Z0-9_]{0,127}$");
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

/** Per-scope running count and byte total for one list. */
class ScopeBudget {
  private readonly counts = new Map<string, number>();
  private readonly bytes = new Map<string, number>();
  constructor(
    private readonly where: string,
    private readonly maxCount: number,
    private readonly maxBytes: number,
  ) {}
  add(scope: string, bytes: number): void {
    const n = (this.counts.get(scope) ?? 0) + 1;
    if (n > this.maxCount) fail(this.where, `must have at most ${this.maxCount} entries per scope`);
    const b = (this.bytes.get(scope) ?? 0) + bytes;
    if (b > this.maxBytes) fail(this.where, `values must total at most ${this.maxBytes} bytes per scope`);
    this.counts.set(scope, n);
    this.bytes.set(scope, b);
  }
}

function decodeRedact(raw: unknown, where: string, seen: Set<string>): ClaimRunEnvRedact[] {
  const arr = list(raw, where, RUN_ENV_MAX_REDACT * SCOPE_COUNT);
  const budget = new ScopeBudget(where, RUN_ENV_MAX_REDACT, RUN_ENV_MAX_REDACT_BYTES);
  const items: ClaimRunEnvRedact[] = [];
  arr.forEach((e, i) => {
    const w = `${where}[${i}]`;
    const { v, bytes } = common(e, w);
    requireMaskable(v.value, w);
    const key = `${v.var_id.toLowerCase()}#${v.rev}`;
    if (seen.has(key)) fail(w, "duplicate (var_id, rev)");
    seen.add(key);
    budget.add(v.scope, bytes);
    items.push(v);
  });
  return items;
}

/** Decodes `secrets.run_env` and `secrets.run_env_redact` (the `secrets` object's own fields, in
 *  `raw`). Absent fields decode to empty arrays; any violation throws RunEnvClaimError. */
export function decodeRunEnvSecrets(raw: unknown): { runEnv: ClaimRunEnvVar[]; runEnvRedact: ClaimRunEnvRedact[] } {
  const o = record(raw, "secrets");
  const seen = new Set<string>();
  const liveArr = list(o.run_env, "run_env", RUN_ENV_MAX_LIVE * SCOPE_COUNT);
  const budget = new ScopeBudget("run_env", RUN_ENV_MAX_LIVE, RUN_ENV_MAX_LIVE_BYTES);
  const names = new Set<string>();
  const runEnv: ClaimRunEnvVar[] = [];
  liveArr.forEach((e, i) => {
    const w = `run_env[${i}]`;
    const { v, bytes, o: eo } = common(e, w);
    if (typeof eo.secret !== "boolean") fail(`${w}.secret`, "must be a boolean");
    const description = str(eo, "description", w);
    if (Buffer.byteLength(description, "utf8") > RUN_ENV_MAX_DESCRIPTION_BYTES) {
      fail(`${w}.description`, `must be at most ${RUN_ENV_MAX_DESCRIPTION_BYTES} bytes`);
    }
    if (description.includes("\u0000")) fail(`${w}.description`, "must not contain NUL");
    if (eo.secret) requireMaskable(v.value, w);
    if (names.has(v.name)) fail(`${w}.name`, "duplicate live name");
    names.add(v.name);
    const key = `${v.var_id.toLowerCase()}#${v.rev}`;
    if (seen.has(key)) fail(w, "duplicate (var_id, rev)");
    seen.add(key);
    budget.add(v.scope, bytes);
    runEnv.push({ ...v, secret: eo.secret, description });
  });
  const runEnvRedact = decodeRedact(o.run_env_redact, "run_env_redact", seen);
  return { runEnv, runEnvRedact };
}

/** Decodes the run-env redact maintenance response. `protected: false` must carry no versions. */
export function decodeRunEnvRedactResponse(raw: unknown): Required<Pick<RunEnvRedactResponse, "protected">> & { versions: ClaimRunEnvRedact[] } {
  const o = record(raw, "response");
  if (typeof o.protected !== "boolean") fail("protected", "must be a boolean");
  const items = decodeRedact(o.versions, "versions", new Set());
  if (!o.protected && items.length > 0) fail("versions", "must be absent or empty when protected is false");
  return { protected: o.protected, versions: items };
}
