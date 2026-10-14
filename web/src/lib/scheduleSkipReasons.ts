// Human labels for the typed schedule skip reasons (PRD #308 M4).
//
// The wire carries the machine sentinel (schedsvc.SkipReason); the fire-outcome UI
// shows a human label instead of the raw string. The map is an EXHAUSTIVE
// Record<ScheduleSkipReason, string> on purpose: a new reason added to the union
// (which the scheduleSkipReasons.test.ts drift guard forces to mirror Go) with no
// label here is a tsc error, never a silent fall-through to the raw sentinel.

import type { ScheduleSkipReason } from "./api";

export const SCHEDULE_SKIP_REASON_LABELS: Record<ScheduleSkipReason, string> = {
  config_not_supported: "configuration not supported",
  not_eligible: "not eligible",
  already_running: "already running",
  description_too_large: "description too large",
  fetch_failed: "fetch failed",
  vault_locked: "vault locked",
  self_improve_mr_cap_reached: "self-improve MR cap reached",
  open_mr_exists: "issue already has an open MR",
  codex_override_conflict: "Codex harness cannot use an Anthropic credential override",
  harness_label_conflict: "issue harness label conflicts with schedule pin",
  harness_label_invalid: "invalid issue harness labels",
  schedules_paused: "all schedules paused",
  no_usable_credential: "no usable credential",
  credential_disabled: "pinned credential is disabled",
  ci_green: "CI is green",
  ci_already_handled: "failure already handled",
  ci_halted_no_progress: "halted: same failure after a repair attempt",
  ci_halted_attempt_cap: "halted: repair attempt limit reached",
  ci_watched_not_seen: "watched workflow not seen",
  ci_unsupported: "forge has no CI API",
  no_default_branch: "repo has no default branch",
  ci_capacity: "watched workflow limit reached",
  ci_evidence_limited: "CI evidence too large to read",
  ci_not_failed: "newest execution neither failed nor passed",
  ci_config_changed: "schedule changed during the fire",
};

export function scheduleSkipReasonLabel(reason: ScheduleSkipReason): string {
  return SCHEDULE_SKIP_REASON_LABELS[reason];
}
