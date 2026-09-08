import type { Run } from "./api";

// The run kinds whose worker opens an MR the MR-review-rework watcher can act on.
// Widened past `issue` by PRD #908 to include `prompt` and `self_improve` runs (they
// open MRs too), so the toggle and the on-demand rework affordance follow the same set.
const MR_REWORK_KINDS = ["issue", "prompt", "self_improve"] as const;

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
 * Visible when the run is a kind that opens a watcher-reworkable MR (`issue`, `prompt`
 * or `self_improve` — widened from issue-only by PRD #908) AND its MR is either not yet
 * observed (`null`) or currently `opened`.
 */
export function canToggleMrRework(run: Pick<Run, "kind" | "mr_state">): boolean {
  return (
    (MR_REWORK_KINDS as readonly string[]).includes(run.kind) &&
    (run.mr_state == null || run.mr_state === "opened")
  );
}

/**
 * Whether the on-demand "Rework now" action (PRD #1202) is offerable for a run: a
 * reworkable KIND (issue/prompt/self_improve) that has COMPLETED and still owns an
 * OPEN MR the watcher would act on. Stricter than canToggleMrRework — which stays
 * visible while the MR is merely unobserved (`null`) — because a manual rework needs a
 * concrete open MR to fold onto, so it requires `status === "completed"`, a real
 * `mr_iid`, and `mr_state === "opened"`.
 */
export function canReworkNow(
  run: Pick<Run, "kind" | "status" | "mr_iid" | "mr_state">,
): boolean {
  return (
    (MR_REWORK_KINDS as readonly string[]).includes(run.kind) &&
    run.status === "completed" &&
    run.mr_iid != null &&
    run.mr_state === "opened"
  );
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
