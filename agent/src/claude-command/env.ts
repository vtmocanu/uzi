// Env for a model command on a guard-bound Claude flight (issue #2471, M1). The command runs
// at the same runner uid as the Claude CLI, so this is parity with native Bash, not isolation:
// it only keeps worker-side authority and the provider credential out of the command's env,
// while the run-attempt markers stay so descendants still attribute to the attempt.

import { PROVIDER_CREDENTIAL_VARS } from "../sdk-spawn.js";
import { WORKER_SPAWN_ENV } from "../worker-spawn-mark.js";

const WORKER_TOKEN_PREFIX = "UZI_WORKER_TOKEN";
const API_URL_ENV = "UZI_API_URL";

/**
 * A copy of the Claude CLI's env for a guarded command: no provider credential, no worker
 * spawn mark, no key starting with `UZI_WORKER_TOKEN` (the token and its file variant) and no
 * `UZI_API_URL`. Undefined values are dropped. Throws when `cliEnv` is not an env object, so a
 * caller can never fall back to inheriting the worker's own env.
 */
export function guardedCommandEnv(cliEnv: Readonly<Record<string, string | undefined>>): Record<string, string> {
  if (typeof cliEnv !== "object" || cliEnv === null || Array.isArray(cliEnv)) {
    throw new TypeError("guardedCommandEnv requires an explicit env object");
  }
  const denied = new Set<string>([...PROVIDER_CREDENTIAL_VARS, WORKER_SPAWN_ENV, API_URL_ENV]);
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(cliEnv)) {
    if (value === undefined || denied.has(key) || key.startsWith(WORKER_TOKEN_PREFIX)) continue;
    out[key] = value;
  }
  return out;
}
