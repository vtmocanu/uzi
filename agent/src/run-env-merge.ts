// Merge of the repo/user environment variables into a harness environment (PRD #4416, "Worker").
//
// Variables are the LOWEST precedence input: they are folded in after the provisioned tool env, only
// when the key is absent, the name passes the shared name rules (the worker re-checks every name
// against the fixture) and the key is not protected. The caller applies its worker-set keys after
// this, so they always win. Each dropped name is reported through `onDrop` with the rule, never the value.

import { runEnvNameViolation } from "./run-env-names.js";

/** The part of a claim variable the environment builders need. */
export interface RunEnvVar {
  readonly name: string;
  readonly value: string;
}

/** One variable not added to the environment, and the rule that kept it out (value never included). */
export interface RunEnvDrop {
  readonly name: string;
  readonly rule: string;
}

export type RunEnvDropHandler = (drop: RunEnvDrop) => void;

/** Adds each variable of `runEnv` to `env` unless a rule applies; returns nothing, mutates `env`.
 *  `isProtected` is the harness's protected-key predicate. */
export function foldRunEnv(
  env: Record<string, string | undefined>,
  runEnv: readonly RunEnvVar[] | undefined,
  isProtected: (key: string) => boolean,
  onDrop?: RunEnvDropHandler,
): void {
  if (!runEnv) return;
  for (const { name, value } of runEnv) {
    const violation = runEnvNameViolation(name);
    if (violation !== null) {
      onDrop?.({ name, rule: `name rule: ${violation}` });
    } else if (isProtected(name)) {
      onDrop?.({ name, rule: "protected worker key: the worker sets it for this harness" });
    } else if (Object.hasOwn(env, name)) {
      onDrop?.({ name, rule: "already set by the worker environment (a variable never overrides a key already present)" });
    } else {
      env[name] = value;
    }
  }
}
