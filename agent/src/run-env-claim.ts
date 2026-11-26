// Fail-closed decoder for the run-env part of a claim (PRD 4416 M2, "Claim" and "Worker").
// Pure: it validates and copies, never logs, never touches the environment, and every error
// message names a field and a rule but never a value. The api side of the contract is
// workersvc.ClaimRunEnvVar / ClaimRunEnvRedact / RunEnvRedactResponse; the shared fixtures are
// fixtures/claims/run-env-claim.json and fixtures/claims/run-env-redact-response.json.

import type { ClaimRunEnvRedact, ClaimRunEnvVar, RunEnvRedactResponse } from "./protocol.js";
import { MIN_SECRET_LEN } from "./redact.js";

/** The protocol capability a worker advertises to be sent run_env and run_env_redact. */
export const RUN_ENV_CAPABILITY = "run_env_v1";

// Decoder bounds. Each is derived from the store limits in api/internal/runenv/values.go and pinned to
// them through fixtures/run-env/claim-limits.json (api/internal/runenv/claim_limits_test.go and
// test/run-env-claim.test.ts), so a legal claim is never rejected.
//
// run_env carries one scope in M2 (repo): at most MaxVarsPerScope variables and MaxScopeValueBytes of
// values. M4 adds the user scope and widens this bound to two scopes.
export const RUN_ENV_MAX_VARS = 50;
export const RUN_ENV_MAX_VALUE_BYTES = 65536;
export const RUN_ENV_MAX_TOTAL_VALUE_BYTES = 262144;
// run_env_redact is every secret version this run may still print: the retired-version budget
// (MaxRetiredVersions = 200 versions, MaxRetiredValueBytes = 2097152 bytes) plus the disabled and
// pending-deletion secrets that are not in run_env, which count against the per-scope limits
// (MaxVarsPerScope = 50 variables, MaxScopeValueBytes = 262144 bytes). The factor 2 covers the repo and
// the user scope (M4), so 200 + 2 * 50 = 300 entries and 2097152 + 2 * 262144 = 2621440 bytes.
export const RUN_ENV_MAX_REDACT_ENTRIES = 300;
export const RUN_ENV_MAX_REDACT_TOTAL_BYTES = 2621440;
export const RUN_ENV_MAX_DESCRIPTION_BYTES = 1024;
/** Largest revision the api stores (int32). */
export const RUN_ENV_MAX_REV = 2147483647;

const NAME_RE = /^[A-Z_][A-Z0-9_]{0,127}$/;
const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** A claim's run-env payload violated the contract. The message never carries a value. */
class RunEnvClaimError extends Error {
  constructor(message: string) {
    super(`run_env claim: ${message}`);
    this.name = "RunEnvClaimError";
  }
}

function fail(path: string, rule: string): never {
  throw new RunEnvClaimError(`${path}: ${rule}`);
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// A lone surrogate cannot be encoded as UTF-8, so it never round-trips through the environment.
const LONE_SURROGATE_RE = /\p{Cs}/u;

function bytes(s: string): number {
  return Buffer.byteLength(s, "utf8");
}

function str(rec: Record<string, unknown>, key: string, path: string): string {
  const v = rec[key];
  if (typeof v !== "string") fail(`${path}.${key}`, "must be a string");
  if (LONE_SURROGATE_RE.test(v)) fail(`${path}.${key}`, "must be valid Unicode (no lone surrogates)");
  return v;
}

/** Fields shared by both entry shapes, with the secret floor applied when `secret` is true. */
function decodeCommon(
  raw: unknown,
  path: string,
  secret: boolean,
): { var_id: string; scope: "repo" | "user"; rev: number; name: string; value: string; rec: Record<string, unknown> } {
  if (!isRecord(raw)) fail(path, "must be an object");
  const var_id = str(raw, "var_id", path);
  if (!UUID_RE.test(var_id)) fail(`${path}.var_id`, "must be a UUID");
  const scope = str(raw, "scope", path);
  if (scope !== "repo" && scope !== "user") fail(`${path}.scope`, "must be repo or user");
  const rev = raw.rev;
  if (typeof rev !== "number" || !Number.isSafeInteger(rev) || rev < 1 || rev > RUN_ENV_MAX_REV) {
    fail(`${path}.rev`, `must be a positive integer of at most ${RUN_ENV_MAX_REV}`);
  }
  const name = str(raw, "name", path);
  if (!NAME_RE.test(name)) fail(`${path}.name`, "must match ^[A-Z_][A-Z0-9_]{0,127}$");
  const value = str(raw, "value", path);
  if (value.includes("\u0000")) fail(`${path}.value`, "must not contain NUL");
  if (secret && value.length < MIN_SECRET_LEN) fail(`${path}.value`, `a secret must be at least ${MIN_SECRET_LEN} characters`);
  return { var_id, scope, rev, name, value, rec: raw };
}

function list(raw: unknown, key: string, max: number): unknown[] {
  if (raw === undefined) return [];
  if (!Array.isArray(raw)) fail(key, "must be an array");
  if (raw.length > max) fail(key, `at most ${max} entries`);
  return raw;
}

function decodeRedactList(raw: unknown, key: string): ClaimRunEnvRedact[] {
  const entries = list(raw, key, RUN_ENV_MAX_REDACT_ENTRIES);
  const seen = new Set<string>();
  let total = 0;
  const out: ClaimRunEnvRedact[] = [];
  entries.forEach((e, i) => {
    const path = `${key}[${i}]`;
    const { var_id, scope, rev, name, value } = decodeCommon(e, path, true);
    const id = `${var_id}#${rev}`;
    if (seen.has(id)) fail(path, "duplicate (var_id, rev)");
    seen.add(id);
    const b = bytes(value);
    if (b > RUN_ENV_MAX_VALUE_BYTES) fail(`${path}.value`, `at most ${RUN_ENV_MAX_VALUE_BYTES} bytes`);
    total += b;
    if (total > RUN_ENV_MAX_REDACT_TOTAL_BYTES) fail(key, `values total at most ${RUN_ENV_MAX_REDACT_TOTAL_BYTES} bytes`);
    out.push({ var_id, scope, rev, name, value });
  });
  return out;
}

/** Decodes secrets.run_env and secrets.run_env_redact from the raw `secrets` object, throwing a
 *  RunEnvClaimError on any violation. Absent fields decode to empty lists. */
export function decodeRunEnvClaim(raw: unknown): { runEnv: ClaimRunEnvVar[]; runEnvRedact: ClaimRunEnvRedact[] } {
  if (!isRecord(raw)) fail("secrets", "must be an object");
  const entries = list(raw.run_env, "run_env", RUN_ENV_MAX_VARS);
  const seen = new Set<string>();
  const names = new Set<string>();
  let total = 0;
  const runEnv: ClaimRunEnvVar[] = [];
  entries.forEach((e, i) => {
    const path = `run_env[${i}]`;
    if (!isRecord(e)) fail(path, "must be an object");
    if (typeof e.secret !== "boolean") fail(`${path}.secret`, "must be a boolean");
    const { var_id, scope, rev, name, value, rec } = decodeCommon(e, path, e.secret);
    const description = str(rec, "description", path);
    if (bytes(description) > RUN_ENV_MAX_DESCRIPTION_BYTES) fail(`${path}.description`, `at most ${RUN_ENV_MAX_DESCRIPTION_BYTES} bytes`);
    const id = `${var_id}#${rev}`;
    if (seen.has(id)) fail(path, "duplicate (var_id, rev)");
    seen.add(id);
    if (names.has(name)) fail(`${path}.name`, "duplicate name");
    names.add(name);
    const b = bytes(value);
    if (b > RUN_ENV_MAX_VALUE_BYTES) fail(`${path}.value`, `at most ${RUN_ENV_MAX_VALUE_BYTES} bytes`);
    total += b;
    if (total > RUN_ENV_MAX_TOTAL_VALUE_BYTES) fail("run_env", `values total at most ${RUN_ENV_MAX_TOTAL_VALUE_BYTES} bytes`);
    runEnv.push({ var_id, scope, rev, name, value, secret: e.secret, description });
  });
  const runEnvRedact = decodeRedactList(raw.run_env_redact, "run_env_redact");
  // run_env_redact holds the versions that are not in run_env; a version in both is a contract break.
  runEnvRedact.forEach((r, i) => {
    if (seen.has(`${r.var_id}#${r.rev}`)) fail(`run_env_redact[${i}]`, "(var_id, rev) is already in run_env");
  });
  return { runEnv, runEnvRedact };
}

/** Decodes the body of the run-env redact maintenance endpoint. protected:false must carry no
 *  versions and protected:true at least one. Throws a RunEnvClaimError on any violation. */
export function decodeRunEnvRedactResponse(raw: unknown): RunEnvRedactResponse {
  if (!isRecord(raw)) fail("response", "must be an object");
  if (typeof raw.protected !== "boolean") fail("protected", "must be a boolean");
  const versions = decodeRedactList(raw.versions, "versions");
  if (!raw.protected) {
    if (versions.length > 0) fail("versions", "must be empty when protected is false");
    return { protected: false };
  }
  // Fail closed: a protected run with no versions cannot be proven free of secrets by an empty needle set.
  if (versions.length === 0) fail("versions", "must be non-empty when protected is true");
  return { protected: true, versions };
}
