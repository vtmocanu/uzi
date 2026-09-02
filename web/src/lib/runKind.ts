// RUN_KINDS is the web mirror of runs.kind, in DB runs_kind_check order. The
// authoritative set is the DB CHECK / api/internal/runkind; this list is pinned to
// the shared fixtures/run-kinds/registry.json by runKindContract.test.ts.
export const RUN_KINDS = ["issue", "ci_fix", "chat", "judge", "self_improve", "prompt", "task", "mr_rework"] as const;
export type RunKind = (typeof RUN_KINDS)[number];

// runKindLabel maps an issue-less run's kind to a short human label for the chip
// (PRD #411). The issue-less kinds that reach the runs list are task, ci_fix,
// prompt, and mr_rework (PRD #700 — CreateAutoMRReworkRun leaves issue_iid NULL, so
// it renders the chip, not a forge #anchor); others degrade to the raw kind with
// underscores spaced.
export function runKindLabel(kind: string): string {
  switch (kind) {
    case "ci_fix":
      return "ci fix";
    case "mr_rework":
      return "MR rework";
    default:
      // Equivalent to kind.replaceAll("_", " ") — a regex global replace because
      // the web tsconfig's lib target predates String.prototype.replaceAll (ES2021).
      return kind.replace(/_/g, " ");
  }
}

// isJudgeEligible mirrors the enqueue allowlist: only issue / ci_fix runs are judged.
// The set is HARD-CODED because production web cannot read the repo-root fixture
// (fixtures/run-kinds/registry.json) at runtime; runKindContract.test.ts is the pin
// that keeps this in lockstep with the fixture's judge_eligible list.
export function isJudgeEligible(kind: string): boolean {
  return kind === "issue" || kind === "ci_fix";
}
