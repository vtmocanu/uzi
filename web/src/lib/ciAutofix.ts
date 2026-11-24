import type { Run } from "./api";

// Run kinds whose branch the CI-autofix watcher can act on (issue #4617).
const CI_AUTOFIX_KINDS = ["issue", "ci_fix", "prompt", "self_improve"] as const;

/**
 * Whether the per-run "automatically fix CI failures" toggle is offered for a run: a kind
 * that owns a branch the CI watcher can fix, with a repo. Deliberately not gated on mr_state
 * (the server's candidate query does not gate on MR state either).
 */
export function canToggleCiAutofix(run: Pick<Run, "kind" | "repo_id">): boolean {
  return (CI_AUTOFIX_KINDS as readonly string[]).includes(run.kind) && run.repo_id != null;
}

/**
 * The effective per-run CI-autofix value for display: the run's own override, else the
 * owner's Settings default (`UserSettings.ci_autofix_enabled`), else ON.
 */
export function effectiveCiAutofix(
  run: Pick<Run, "ci_autofix_enabled">,
  userDefault: boolean | null | undefined,
): boolean {
  return run.ci_autofix_enabled ?? userDefault ?? true;
}
