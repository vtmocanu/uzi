// failOriginLabel maps a run's typed fail_origin to a human label for failure context,
// including the dashboard's last failed run (#2399). The keys track the closed
// failorigin.go vocabulary
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
  provider_policy_refusal: "provider safety-policy refusal",
  plan_rejected: "plan rejected",
  auto_stopped: "auto stopped",
  workflow_scope_missing: "workflow scope missing",
  finalize_base_align_conflict: "finalize base-align conflict",
  push_secret_blocked: "push secret blocked",
  forge_unreachable: "forge unreachable",
  gate_presentation_refused: "plan gate refused",
  history_rewritten: "history rewritten",
  task_undispatched: "task undispatched",
  plan_missing: "plan missing",
  worker_residue_blocked: "worker residue blocked",
  skills_plugin_load_failed: "skills plugin load failed",
  data_volume_full: "data volume full",
  no_job_capable_worker: "no job-capable worker",
  ephemeral_worker_never_registered: "ephemeral worker never registered",
  job_no_result: "job reported no result",
  unknown: "unknown",
};

export function failOriginLabel(origin: string): string {
  return labels[origin] ?? origin;
}
