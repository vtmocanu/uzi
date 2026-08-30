// Human labels for the typed schedule skip reasons (PRD #308 M4).
//
// The wire carries the machine sentinel (schedsvc.SkipReason); the fire-outcome UI
// shows a human label instead of the raw string. The map is an EXHAUSTIVE
// Record<ScheduleSkipReason, string> on purpose: a new reason added to the union
// (which the scheduleSkipReasons.test.ts drift guard forces to mirror Go) with no
// label here is a tsc error, never a silent fall-through to the raw sentinel.

import type { ScheduleSkipReason } from "./api";

export const SCHEDULE_SKIP_REASON_LABELS: Record<ScheduleSkipReason, string> = {
  not_eligible: "not eligible",
  already_running: "already running",
  description_too_large: "description too large",
  fetch_failed: "fetch failed",
  vault_locked: "vault locked",
  self_improve_mr_cap_reached: "self-improve MR cap reached",
  open_mr_exists: "issue already has an open MR",
};

export function scheduleSkipReasonLabel(reason: ScheduleSkipReason): string {
  return SCHEDULE_SKIP_REASON_LABELS[reason];
}
