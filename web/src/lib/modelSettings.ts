// Settings-surface source labels (PRD #3245 §7), keyed by (field, token): the words a
// settings page shows for where a model value comes from. Defined in TS so production
// code stays independent of the repo-root fixtures; modelSettings.test.ts pins this
// table to fixtures/model-settings/contract.json row for row, the same way the Go
// CLI's modelSettingSource is pinned. Run-level lead labels are a different table (fixtures/lead-config).
import type { EffortSettingCell } from "./apiTypes";

export const MODEL_SETTING_SOURCE_LABELS: readonly { field: string; token: string; label: string }[] = [
  { field: "model", token: "user_default", label: "your choice" },
  { field: "model", token: "instance_default", label: "instance default" },
  { field: "model", token: "builtin", label: "built-in" },
  { field: "effort", token: "user_default", label: "your choice" },
  { field: "effort", token: "builtin", label: "built-in" },
  { field: "effort", token: "runtime_default", label: "runtime default" },
  { field: "effort", token: "catalog_default", label: "catalog default" },
  { field: "model", token: "schedule_pin", label: "schedule pin" },
  { field: "model", token: "template", label: "lead template" },
  { field: "model", token: "cross_check_pin", label: "checker pin" },
  { field: "model", token: "cross_check_worker_default", label: "worker default" },
  { field: "effort", token: "cross_check_pin", label: "checker pin" },
  { field: "effort", token: "cross_check_default", label: "cross-check default" },
  { field: "harness", token: "run_override", label: "your choice for this run" },
  { field: "harness", token: "schedule_pin", label: "schedule pin" },
  { field: "harness", token: "user_default", label: "your default harness" },
  { field: "harness", token: "automatic", label: "automatic" },
  { field: "harness", token: "chosen_at_create", label: "chosen when the run is created" },
  { field: "credential", token: "chosen_at_create", label: "chosen when the run is created" },
  { field: "credential", token: "claim_selection", label: "chosen when the run starts" },
  { field: "credential", token: "auto_selection", label: "automatic selection" },
  { field: "credential", token: "owner_default", label: "your default credential" },
  { field: "credential", token: "token_override", label: "token chosen for this run" },
  { field: "credential", token: "codex_default_credential", label: "your default Codex credential" },
  { field: "note", token: "schedule_pin_ignored", label: "the schedule's model pin does not apply to this harness" },
  { field: "note", token: "sdk_default", label: "the SDK decides" },
  { field: "note", token: "not_configurable", label: "not configurable" },
  { field: "note", token: "not_sent", label: "not sent" },
  { field: "note", token: "user_default_harness_unusable", label: "your default harness has no usable credential" },
  { field: "note", token: "issue_labels_may_select_harness", label: "issue labels may select the harness" },
  { field: "note", token: "repo_agents_not_predicted", label: "repository agents are not predicted" },
  { field: "note", token: "token_fixed", label: "the token is fixed" },
  { field: "note", token: "instance_settings_unavailable", label: "instance settings are unavailable" },
  { field: "note", token: "automatic_weekly_reset", label: "chosen by the soonest weekly reset" },
  { field: "note", token: "overflow", label: "may move to the other harness near its limit" },
];

// codexRuntimeDefaultLabel is the Codex effort select's "Runtime default" option text
// (issue #3251): what the Codex model catalog applies when no level is sent. Mirrors the
// CLI's codexRuntimeDefaultResolved. Anything that is not a well-formed known/varies
// default (absent cell, unknown, unrecognised state, a non-string or empty effort/model/version)
// reads as unknown, so a malformed payload never renders "undefined" or "null".
export function codexRuntimeDefaultLabel(cell: EffortSettingCell | undefined): string {
  const runtime = modelSettingSourceLabel("effort", "runtime_default");
  const prefix = runtime.charAt(0).toUpperCase() + runtime.slice(1);
  const catalog = modelSettingSourceLabel("effort", "catalog_default");
  const d = cell?.catalog_default;
  if (
    !d || (d.state !== "known" && d.state !== "varies") ||
    typeof d.effort !== "string" || typeof d.model !== "string" || typeof d.runtime_version !== "string" ||
    d.effort === "" || d.model === "" || d.runtime_version === ""
  ) {
    return `${prefix} · ${catalog} unknown until a Codex run reports it`;
  }
  const effort =
    d.effort === "none" ? "none (no reasoning level requested)"
    : d.effort === "ultra" ? "ultra (Codex substitutes another level)"
    : d.effort;
  const base = `${prefix} · ${catalog}: ${effort} (${d.model}, Codex ${d.runtime_version})`;
  return d.state === "varies" ? `${base} · may differ on workers still on another version` : base;
}

// modelSettingSourceLabel renders a (field, token) provenance pair; an unfamiliar pair
// (a newer server, another child's field) reads "unknown" rather than leaking a token.
export function modelSettingSourceLabel(field: string, token: string): string {
  return MODEL_SETTING_SOURCE_LABELS.find((r) => r.field === field && r.token === token)?.label ?? "unknown";
}
