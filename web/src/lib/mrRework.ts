import type { Run } from "./api";

/**
 * Whether the per-run "auto-rework this MR's review comments" toggle is live for a run
 * (PRD #841 D2). Unlike wait_on_limit — whose toggle governs an in-flight run and so is
 * gated on a non-terminal status — the MR-review-rework watcher acts AFTER the run
 * completes, during Human Review, once the MR has gained review comments. So the toggle
 * must stay visible on a COMPLETED run whose MR is still open, and disappear only once
 * the MR merges or closes (the watcher's candidate query excludes any `mr_state` other
 * than `opened`, so a write past that point is inert anyway — this is the UI agreeing
 * with the server, not enforcing it).
 *
 * Visible when the run is an `issue` run (only issue runs open the MRs the watcher
 * reworks) AND its MR is either not yet observed (`null`) or currently `opened`.
 */
export function canToggleMrRework(run: Pick<Run, "kind" | "mr_state">): boolean {
  return run.kind === "issue" && (run.mr_state == null || run.mr_state === "opened");
}

/**
 * The effective per-run MR-review-rework value for display (PRD #841): the run's own
 * override wins, else the owner's Settings default, else ON (the feature is default-ON).
 * `userDefault` is `UserSettings.mr_rework_enabled` (null = the user never overrode the
 * default-ON state). Kept as an explicit tri-state coalesce so the "inherit shows the
 * user default" case is one place, testable, and never collapses null into false.
 */
export function effectiveMrRework(
  run: Pick<Run, "mr_rework_enabled">,
  userDefault: boolean | null | undefined,
): boolean {
  return run.mr_rework_enabled ?? userDefault ?? true;
}
