// Folds a run's user-defined environment variables (PRD 4416 M2) into an agent command env.
//
// Only the lead lane's two env builders call this (buildSdkEnv for the Claude SDK executor and
// buildCommandEnv for the Codex command identity). User variables have the LOWEST precedence: they are
// folded in after the provisioned tool env and before every worker-set key, a variable is added only when
// its name passes the shared name rules, is not a protected key and is not already set, and every name
// that is dropped is reported (never its value).

import { sanitizeName } from "./diagnostic-name.js";
import type { ClaimRunEnvVar } from "./protocol.js";
import { runEnvNameViolation } from "./run-env-names.js";

/** The run's variables plus the sink for names that are not injected. */
export interface RunEnvInjection {
  readonly vars: readonly ClaimRunEnvVar[];
  /** The set the executor computed once for this run (runEnvInjectedSet over the builder's pre-fold keys).
   *  When present the fold applies exactly it and reports its drops, so the env and every prompt agree by
   *  construction; absent, the fold computes the set over the keys the env holds at the call. */
  readonly set?: RunEnvInjectedSet;
  /** Called once per dropped variable with its name and the rule it broke. Must never receive a value. */
  readonly onDrop?: (name: string, rule: string) => void;
}

/** A variable that is not injected, with the rule it broke (never its value). */
export interface RunEnvDropped {
  readonly name: string;
  readonly rule: string;
}

/** Which of `vars` a builder injects and which it drops. */
export interface RunEnvInjectedSet {
  readonly injected: ClaimRunEnvVar[];
  readonly dropped: RunEnvDropped[];
}

/** The ONE source of the injection rules: a variable is injected unless its name breaks the shared name
 *  rules, is a key the builder protects, or is already set (in `preFoldKeys`, the keys the builder's env holds
 *  before the fold, or by an earlier variable of the same list). Pure: it reads no env and has no effect, so
 *  an executor can compute the injected set once per executor run, before any prompt is built, from the same
 *  pre-fold keys its builder folds over. The order of `vars` is kept. */
export function runEnvInjectedSet(
  vars: readonly ClaimRunEnvVar[],
  preFoldKeys: Iterable<string>,
  isProtected: (key: string) => boolean,
): RunEnvInjectedSet {
  const taken = new Set(preFoldKeys);
  const injected: ClaimRunEnvVar[] = [];
  const dropped: RunEnvDropped[] = [];
  for (const v of vars) {
    const violation = runEnvNameViolation(v.name);
    if (violation !== null) {
      dropped.push({ name: v.name, rule: violation });
    } else if (isProtected(v.name)) {
      dropped.push({ name: v.name, rule: "protected worker key" });
    } else if (taken.has(v.name)) {
      dropped.push({ name: v.name, rule: "already set by the worker or the tool profile" });
    } else {
      taken.add(v.name);
      injected.push(v);
    }
  }
  return { injected, dropped };
}

/** Folds the injection into `env`. `isProtected` names the keys the builder pins. Returns the variables
 *  that were injected, in order: a variable that was dropped is not in the list. The set is
 *  `injection.set` when given, else {@link runEnvInjectedSet} over the keys `env` holds at the call. */
export function foldRunEnv(
  env: Record<string, string | undefined>,
  injection: RunEnvInjection | undefined,
  isProtected: (key: string) => boolean,
): ClaimRunEnvVar[] {
  if (injection === undefined) return [];
  const { injected, dropped } = injection.set ?? runEnvInjectedSet(injection.vars, Object.keys(env), isProtected);
  if (injection.set !== undefined) {
    // Defence in depth: every injected entry of a precomputed set must be one the fold would inject (valid name, not protected,
    // not already set, not duplicated). Check every entry before
    // touching `env`, so a bad set leaves it unchanged. The error names the variable, never its value.
    const seen = new Set<string>();
    for (const v of injected) {
      if (runEnvNameViolation(v.name) !== null || isProtected(v.name) || Object.hasOwn(env, v.name) || seen.has(v.name)) {
        throw new Error(`run env: precomputed injected set holds ${sanitizeName(v.name)}, which the fold would not inject`);
      }
      seen.add(v.name);
    }
  }
  for (const d of dropped) injection.onDrop?.(d.name, d.rule);
  for (const v of injected) env[v.name] = v.value;
  return injected;
}

/** The run event text for a dropped variable. The name is attacker-influenced, so it is sanitized. */
export function runEnvDropText(name: string, rule: string): string {
  return `run variable ${sanitizeName(name)} not injected: ${rule}`;
}
