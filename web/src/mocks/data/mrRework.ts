// ── PRD #700 M6: MR-review-watcher differential fixture ──────────────────────
// Enumerates every branch the PRD names so the mock exercises each and the
// completeness test (mockApi.mrRework.test.ts) fails the moment one is dropped:
//   - opted-in : the per-user opt-in default-ON state (mr_rework_enabled null/absent).
//   - opted-out: an explicit false, which stops the watcher for this user.
//   - reworking: an mr_rework run folding review comments onto the open MR
//                (mockRuns "run-mr-rework").
//   - capped   : the per-MR automatic cap hit, surfaced on the run itself: RunView's
//                "Automatic rework: N of M cycles used · stopped" line (mockRuns
//                "run-rework-capped", PRD #1202), NOT a new watch-state endpoint
//                (out of scope, PRD #700 Decision 13 / M6). The halt used to be backed
//                by an invented inbox row; the inbox is retired (PRD #1650).
export type MrReworkBranch = "opted-in" | "opted-out" | "reworking" | "capped";

export interface MrReworkFixtureBranch {
  branch: MrReworkBranch;
  description: string;
  // Present for the two settings branches: the mr_rework_enabled value that
  // represents it (null = default-ON). Absent for the run branches.
  settingsValue?: boolean | null;
  // Present for the run branches: the mockRuns id backing it.
  fixtureId?: string;
}

export const mockMrReworkFixture: MrReworkFixtureBranch[] = [
  {
    branch: "opted-in",
    description: "Default ON: a null/absent mr_rework_enabled reads as enabled.",
    settingsValue: null,
  },
  {
    branch: "opted-out",
    description: "An explicit false opts this user out; the watcher skips their MRs.",
    settingsValue: false,
  },
  {
    branch: "reworking",
    description: "An mr_rework run folding review comments onto the existing branch/MR.",
    fixtureId: "run-mr-rework",
  },
  {
    branch: "capped",
    description: "Per-MR automatic cap hit: the run view reads \"cycles used · stopped\".",
    fixtureId: "run-rework-capped",
  },
];
