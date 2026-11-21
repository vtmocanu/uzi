// Folds a run's user-defined environment variables (PRD 4416 M2) into an agent command env.
//
// Only the lead lane's two env builders call this (buildSdkEnv for the Claude SDK executor and
// buildCommandEnv for the Codex command identity). User variables have the LOWEST precedence: they are
// folded in after the provisioned tool env and before every worker-set key, a variable is added only when
// its name passes the shared name rules, is not a protected key and is not already set, and every name
// that is dropped is reported (never its value).

import { sanitizeName } from "./codex/render.js";
import type { ClaimRunEnvVar } from "./protocol.js";
import { runEnvNameViolation } from "./run-env-names.js";

/** The run's variables plus the sink for names that are not injected. */
export interface RunEnvInjection {
  readonly vars: readonly ClaimRunEnvVar[];
  /** Called once per dropped variable with its name and the rule it broke. Must never receive a value. */
  readonly onDrop?: (name: string, rule: string) => void;
}

/** Folds `injection.vars` into `env`. `isProtected` names the keys the builder pins. */
export function foldRunEnv(
  env: Record<string, string | undefined>,
  injection: RunEnvInjection | undefined,
  isProtected: (key: string) => boolean,
): void {
  if (injection === undefined) return;
  for (const v of injection.vars) {
    const violation = runEnvNameViolation(v.name);
    if (violation !== null) {
      injection.onDrop?.(v.name, violation);
      continue;
    }
    if (isProtected(v.name)) {
      injection.onDrop?.(v.name, "protected worker key");
      continue;
    }
    if (Object.hasOwn(env, v.name)) {
      injection.onDrop?.(v.name, "already set by the worker or the tool profile");
      continue;
    }
    env[v.name] = v.value;
  }
}

/** The run event text for a dropped variable. The name is attacker-influenced, so it is sanitized. */
export function runEnvDropText(name: string, rule: string): string {
  return `run variable ${sanitizeName(name)} not injected: ${rule}`;
}
