// Worker-side contract for the repository variables a claim delivers (PRD 4416 M2).
// The api side is workersvc.ClaimSecrets.RunEnv / RunEnvRedact (api/internal/workersvc/claim.go)
// and apitypes.RunEnvRedactResponse; both wire shapes are pinned by
// fixtures/claims/run-env-claim.json and fixtures/claims/run-env-redact-response.json.
//
// The decoders are bounded and fail closed: any breach throws RunEnvClaimError and the caller
// must treat the claim as unusable rather than deliver a partial set.

import { MIN_SECRET_LEN } from "./redact.js";

/** Protocol capability the worker advertises once it can receive, redact and scan repository
 *  variables. Mirrors capability.RunEnvV1 in api/internal/capability/capability.go. */
export const RUN_ENV_CAPABILITY = "run_env_v1";

/** One delivered variable (an entry of ClaimSecrets.run_env). Secret entries carry usable values. */
export interface RunEnvVar {
  var_id: string;
  scope: "repo" | "user";
  rev: number;
  name: string;
  value: string;
  secret: boolean;
  description: string;
}

/** A secret version delivered for redaction and scanning only (ClaimSecrets.run_env_redact). */
export interface RunEnvRedactEntry {
  var_id: string;
  scope: "repo" | "user";
  rev: number;
  name: string;
  value: string;
}

/** Body of GET /api/worker/runs/{id}/run-env-redact. */
export interface RunEnvRedactResponse {
  protected: boolean;
  versions: RunEnvRedactEntry[];
}

/** The decoded claim fields. */
export interface DecodedRunEnv {
  runEnv: RunEnvVar[];
  runEnvRedact: RunEnvRedactEntry[];
}

export const RUN_ENV_MAX_VARS = 50;
export const RUN_ENV_MAX_VALUE_BYTES = 65536;
export const RUN_ENV_MAX_TOTAL_VALUE_BYTES = 262144;
/** Redact bounds, per scope held: up to 200 retired versions (2097152 bytes of retired history)
 *  plus up to 50 disabled or pending-deletion versions (262144 bytes), so 250 entries and
 *  2097152 + 262144 = 2359296 bytes. M2 delivers the repo scope only; M4's user scope adds another
 *  50 entries and 262144 bytes, to be added here then. */
const RUN_ENV_REDACT_RETIRED_VERSIONS = 200;
const RUN_ENV_REDACT_RETIRED_BYTES = 2097152;
const RUN_ENV_REDACT_INACTIVE_VERSIONS = 50;
const RUN_ENV_REDACT_INACTIVE_BYTES = 262144;
export const RUN_ENV_MAX_REDACT_ENTRIES = RUN_ENV_REDACT_RETIRED_VERSIONS + RUN_ENV_REDACT_INACTIVE_VERSIONS;
export const RUN_ENV_MAX_REDACT_TOTAL_BYTES = RUN_ENV_REDACT_RETIRED_BYTES + RUN_ENV_REDACT_INACTIVE_BYTES;
const RUN_ENV_MAX_REV = 2147483647;
export const RUN_ENV_MAX_DESCRIPTION_BYTES = 1024;

/** Thrown for any claim breach. The message names the field and the rule, never a value. */
export class RunEnvClaimError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "RunEnvClaimError";
  }
}

const NAME_REGEXP = /^[A-Z_][A-Z0-9_]{0,127}$/;
const UUID_REGEXP = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function str(rec: Record<string, unknown>, key: string, where: string): string {
  const v = rec[key];
  if (typeof v !== "string") throw new RunEnvClaimError(`${where}.${key} must be a string`);
  if (v.includes("\u0000")) throw new RunEnvClaimError(`${where}.${key} must not contain NUL`);
  return v;
}

function common(rec: Record<string, unknown>, where: string): { var_id: string; scope: "repo" | "user"; rev: number; name: string; value: string } {
  const varId = str(rec, "var_id", where);
  if (!UUID_REGEXP.test(varId)) throw new RunEnvClaimError(`${where}.var_id must be a uuid`);
  const scope = rec["scope"];
  if (scope !== "repo" && scope !== "user") throw new RunEnvClaimError(`${where}.scope must be "repo" or "user"`);
  const rev = rec["rev"];
  if (typeof rev !== "number" || !Number.isSafeInteger(rev) || rev < 1 || rev > RUN_ENV_MAX_REV) {
    throw new RunEnvClaimError(`${where}.rev must be an integer from 1 to ${RUN_ENV_MAX_REV}`);
  }
  const name = str(rec, "name", where);
  if (!NAME_REGEXP.test(name)) throw new RunEnvClaimError(`${where}.name must match ${NAME_REGEXP.source}`);
  const value = str(rec, "value", where);
  if (Buffer.byteLength(value, "utf8") > RUN_ENV_MAX_VALUE_BYTES) {
    throw new RunEnvClaimError(`${where}.value exceeds ${RUN_ENV_MAX_VALUE_BYTES} bytes`);
  }
  return { var_id: varId, scope, rev, name, value };
}

function checkSecretLength(value: string, where: string): void {
  if (value.length < MIN_SECRET_LEN) {
    throw new RunEnvClaimError(`${where}: a secret value must be at least ${MIN_SECRET_LEN} characters`);
  }
}

function key(varId: string, rev: number): string {
  return `${varId.toLowerCase()}#${rev}`;
}

function decodeRedactList(raw: unknown, field: string, seen: Set<string>): RunEnvRedactEntry[] {
  if (raw === undefined || raw === null) return [];
  if (!Array.isArray(raw)) throw new RunEnvClaimError(`${field} must be an array`);
  if (raw.length > RUN_ENV_MAX_REDACT_ENTRIES) {
    throw new RunEnvClaimError(`${field} has more than ${RUN_ENV_MAX_REDACT_ENTRIES} entries`);
  }
  const out: RunEnvRedactEntry[] = [];
  let total = 0;
  raw.forEach((item, i) => {
    const where = `${field}[${i}]`;
    if (!isRecord(item)) throw new RunEnvClaimError(`${where} must be an object`);
    const c = common(item, where);
    checkSecretLength(c.value, where);
    const k = key(c.var_id, c.rev);
    if (seen.has(k)) throw new RunEnvClaimError(`${where}: duplicate (var_id, rev)`);
    seen.add(k);
    total += Buffer.byteLength(c.value, "utf8");
    if (total > RUN_ENV_MAX_REDACT_TOTAL_BYTES) {
      throw new RunEnvClaimError(`${field} values exceed ${RUN_ENV_MAX_REDACT_TOTAL_BYTES} bytes in total`);
    }
    out.push(c);
  });
  return out;
}

/** Decodes the run-env fields of a claim's `secrets` object. Absent fields decode to empty
 *  arrays. Throws RunEnvClaimError on any breach of the contract or its limits. */
export function decodeRunEnv(secrets: unknown): DecodedRunEnv {
  if (secrets === undefined || secrets === null) return { runEnv: [], runEnvRedact: [] };
  if (!isRecord(secrets)) throw new RunEnvClaimError("secrets must be an object");
  const seen = new Set<string>();
  const envVarIds = new Set<string>();
  const envNames = new Set<string>();

  const rawEnv = secrets["run_env"];
  const runEnv: RunEnvVar[] = [];
  if (rawEnv !== undefined && rawEnv !== null) {
    if (!Array.isArray(rawEnv)) throw new RunEnvClaimError("run_env must be an array");
    if (rawEnv.length > RUN_ENV_MAX_VARS) throw new RunEnvClaimError(`run_env has more than ${RUN_ENV_MAX_VARS} entries`);
    let total = 0;
    rawEnv.forEach((item, i) => {
      const where = `run_env[${i}]`;
      if (!isRecord(item)) throw new RunEnvClaimError(`${where} must be an object`);
      const c = common(item, where);
      const secret = item["secret"];
      if (typeof secret !== "boolean") throw new RunEnvClaimError(`${where}.secret must be a boolean`);
      const description = str(item, "description", where);
      if (Buffer.byteLength(description, "utf8") > RUN_ENV_MAX_DESCRIPTION_BYTES) {
        throw new RunEnvClaimError(`${where}.description exceeds ${RUN_ENV_MAX_DESCRIPTION_BYTES} bytes`);
      }
      if (secret) checkSecretLength(c.value, where);
      const k = key(c.var_id, c.rev);
      if (seen.has(k)) throw new RunEnvClaimError(`${where}: duplicate (var_id, rev)`);
      seen.add(k);
      if (envVarIds.has(c.var_id.toLowerCase())) throw new RunEnvClaimError(`${where}: duplicate var_id`);
      envVarIds.add(c.var_id.toLowerCase());
      if (envNames.has(c.name)) throw new RunEnvClaimError(`${where}: duplicate name`);
      envNames.add(c.name);
      total += Buffer.byteLength(c.value, "utf8");
      if (total > RUN_ENV_MAX_TOTAL_VALUE_BYTES) {
        throw new RunEnvClaimError(`run_env values exceed ${RUN_ENV_MAX_TOTAL_VALUE_BYTES} bytes in total`);
      }
      runEnv.push({ ...c, secret, description });
    });
  }

  const runEnvRedact = decodeRedactList(secrets["run_env_redact"], "run_env_redact", seen);
  return { runEnv, runEnvRedact };
}

/** Decodes the maintenance endpoint body. `protected:false` carries no versions. The same
 *  limits and fail-closed rules as the claim's redact list apply. */
export function decodeRunEnvRedactResponse(body: unknown): RunEnvRedactResponse {
  if (!isRecord(body)) throw new RunEnvClaimError("run-env-redact response must be an object");
  const prot = body["protected"];
  if (typeof prot !== "boolean") throw new RunEnvClaimError("run-env-redact response.protected must be a boolean");
  const versions = decodeRedactList(body["versions"], "versions", new Set<string>());
  if (!prot && versions.length > 0) {
    throw new RunEnvClaimError("run-env-redact response: protected=false must carry no versions");
  }
  if (prot && versions.length === 0) {
    throw new RunEnvClaimError("run-env-redact response: protected=true must carry versions");
  }
  return { protected: prot, versions };
}
