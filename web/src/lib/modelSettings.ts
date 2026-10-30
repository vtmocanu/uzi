// Settings-surface source labels (PRD #3245 §7), keyed by (field, token): the words a
// settings page shows for where a model value comes from. Defined in TS so production
// code stays independent of the repo-root fixtures; modelSettings.test.ts pins this
// table to fixtures/model-settings/contract.json row for row, the same way the Go
// CLI's modelSettingSource is pinned. Run-level lead labels are a different table (fixtures/lead-config).
export const MODEL_SETTING_SOURCE_LABELS: readonly { field: string; token: string; label: string }[] = [
  { field: "model", token: "user_default", label: "your choice" },
  { field: "model", token: "instance_default", label: "instance default" },
  { field: "model", token: "builtin", label: "built-in" },
  { field: "effort", token: "user_default", label: "your choice" },
  { field: "effort", token: "builtin", label: "built-in" },
  { field: "effort", token: "runtime_default", label: "runtime default" },
];

// modelSettingSourceLabel renders a (field, token) provenance pair; an unfamiliar pair
// (a newer server, another child's field) reads "unknown" rather than leaking a token.
export function modelSettingSourceLabel(field: string, token: string): string {
  return MODEL_SETTING_SOURCE_LABELS.find((r) => r.field === field && r.token === token)?.label ?? "unknown";
}
