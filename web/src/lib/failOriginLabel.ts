// failOriginLabel maps a run's typed fail_origin to a human label for the dashboard's
// "Top causes" line (PRD #1293 M2). The keys track the closed failorigin.go vocabulary
// (server-coerced, and mirrored by the runs.fail_origin CHECK constraint) plus "unknown",
// which the API keys a NULL fail_origin as (rows failed before the column existed).
//
// Forward-compatible by design: a future migration can widen the vocabulary before this
// map knows the key, so an unrecognised origin renders with its raw name rather than being
// dropped or blanked. Keep this the ONLY fail_origin label map on the web side.
const labels: Record<string, string> = {
  provisioning_failed: "provisioning failed",
  credential_unavailable: "credential unavailable",
  guardrail_blocked: "guardrail blocked",
  rate_limited: "rate limited",
  run_timeout: "run timeout",
  worker_lost: "worker lost",
  agent_failure: "agent failure",
  plan_rejected: "plan rejected",
  auto_stopped: "auto stopped",
  workflow_scope_missing: "workflow scope missing",
  finalize_base_align_conflict: "finalize base-align conflict",
  push_secret_blocked: "push secret blocked",
  forge_unreachable: "forge unreachable",
  history_rewritten: "history rewritten",
  plan_missing: "plan missing",
  unknown: "unknown",
};

export function failOriginLabel(origin: string): string {
  return labels[origin] ?? origin;
}
