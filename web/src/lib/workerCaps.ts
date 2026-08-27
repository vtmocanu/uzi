// The "effective worker capabilities" docker fold, single-sourced on the web side here
// (issue #512 M5). It is the web mirror of SQL's fn_effective_worker_caps (migration
// 00151) and Go's capability.EffectiveWorkerCaps: a worker's stored capabilities unioned
// with `docker` when it is docker-enabled. The worker DTO carries `docker` and
// `capabilities` as INDEPENDENT signals, so a provision-time docker worker whose
// self-report has not landed still satisfies a docker requirement — without this fold the
// readiness panel would show a false "unmet" for a plan the approve gate accepts.
//
// Returns a Set so callers can test membership directly (RunView's unmetCaps filters the
// run's required caps against it). Unlike the non-dedup SQL/Go folds the Set naturally
// collapses a `docker` already present in capabilities, which is harmless — every consumer
// treats the fold as a set.
export function effectiveWorkerCaps(
  capabilities: readonly string[] | undefined,
  dockerEnabled: boolean,
): Set<string> {
  const caps = new Set(capabilities ?? []);
  if (dockerEnabled) caps.add("docker");
  return caps;
}
