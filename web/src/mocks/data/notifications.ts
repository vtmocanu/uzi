import { daysAgo, minsAgo } from "./time";
import { mockAdmin } from "./users";

// ── Notifications inbox (PRD #46 M2) ─────────────────────────────────────────
// The mock keeps the full row (incl. user_id + owner) so listNotifications can
// filter to the caller (own view) or show everyone (admin all-view), exactly like
// the API. payload follows the { title, body } convention the inbox renders; the
// judge is the seeded tenant. A couple of rows belong to another user so the admin
// all-view has cross-owner content to show.
export interface MockNotification {
  id: string;
  user_id: string;
  kind: string;
  payload: Record<string, unknown>;
  run_id: string | null;
  review_id: string | null;
  read_at: string | null;
  created_at: string;
  owner_email: string;
  owner_display_name: string | null;
}

export const mockNotifications: MockNotification[] = [
  {
    id: "ntf-1",
    user_id: mockAdmin.id,
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: issues — 2 recommendations, incl. a missing worker tool" },
    run_id: "run-done",
    review_id: null,
    read_at: null,
    created_at: minsAgo(6),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
  {
    // A NON-judge row (PRD #98 M5). It is here so demo mode renders every state the inbox
    // now has: this row deep-links to /runs/{id} while its judge neighbours deep-link to
    // /judge?run={id}, and by sitting BETWEEN ntf-1 and ntf-2 it also breaks the judge run
    // — leaving one ungrouped judge row above it and a grouped pair below. Without it the
    // demo showed exactly one of the three states, and the grouping is what would have
    // hidden the retarget from anyone looking.
    id: "ntf-mr",
    user_id: mockAdmin.id,
    kind: "mr_merged",
    payload: { title: "Merge request merged", body: "!42 — add rg to the worker image" },
    run_id: "run-done",
    review_id: null,
    read_at: null,
    created_at: minsAgo(20),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
  {
    id: "ntf-2",
    user_id: mockAdmin.id,
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: ideal — nothing to change" },
    run_id: "run-live",
    review_id: null,
    read_at: null,
    created_at: minsAgo(58),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
  {
    id: "ntf-3",
    user_id: mockAdmin.id,
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: ok — one template tweak suggested" },
    run_id: null,
    review_id: null,
    read_at: daysAgo(1),
    created_at: daysAgo(1),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
  {
    id: "ntf-4",
    user_id: "u-mira",
    kind: "judge_review",
    payload: { title: "Run review ready", body: "verdict: issues — worker missing `jq`" },
    run_id: null,
    review_id: null,
    read_at: null,
    created_at: minsAgo(120),
    owner_email: "mira@uzi.local",
    owner_display_name: "Alex Rivera",
  },
  {
    // The coalesced incidental-findings ping (PRD #333 M6/M7). Its payload carries the per-run
    // { count, repo_path, run_id } the FindingNotificationRow renders, and it deep-links to
    // /findings?run=<run_id> — a DIFFERENT destination from the judge/run rows above, so demo
    // mode exercises the kind-conditional notificationLink.
    id: "ntf-find",
    user_id: mockAdmin.id,
    kind: "incidental_finding",
    payload: {
      run_id: "run-live",
      repo_id: "repo-uzi",
      repo_path: "vtmocanu/uzi",
      count: 2,
      finding_ids: ["find-1", "find-2"],
    },
    run_id: "run-live",
    review_id: null,
    read_at: null,
    created_at: minsAgo(9),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
  {
    // PRD #700 M6: the "capped" branch of the MR-review watcher. Hitting the per-MR
    // rework cap (default 5) is NOT a new run/watch-state endpoint (out of scope) —
    // it halts and flags the card via the existing inbox halt notification, mirroring
    // M3's ci-autofix halt path. Deep-links to the reworking run.
    id: "ntf-mr-rework-capped",
    user_id: mockAdmin.id,
    kind: "mr_rework_capped",
    payload: {
      title: "MR rework capped",
      body: "!57 hit the per-MR rework cap (5) — review the remaining comments by hand.",
    },
    run_id: "run-mr-rework",
    review_id: null,
    read_at: null,
    created_at: minsAgo(4),
    owner_email: mockAdmin.email,
    owner_display_name: mockAdmin.display_name,
  },
];

// ── PRD #700 M6: MR-review-watcher differential fixture ──────────────────────
// Enumerates every branch the PRD names so the mock exercises each and the
// completeness test (mockApi.mrRework.test.ts) fails the moment one is dropped:
//   - opted-in : the per-user opt-in default-ON state (mr_rework_enabled null/absent).
//   - opted-out: an explicit false, which stops the watcher for this user.
//   - reworking: an mr_rework run folding review comments onto the open MR
//                (mockRuns "run-mr-rework").
//   - capped   : the per-MR cap hit, surfaced via the halt inbox notification
//                (mockNotifications "ntf-mr-rework-capped"), NOT a new watch-state
//                endpoint (out of scope, PRD Decision 13 / M6).
export type MrReworkBranch = "opted-in" | "opted-out" | "reworking" | "capped";

export interface MrReworkFixtureBranch {
  branch: MrReworkBranch;
  description: string;
  // Present for the two settings branches: the mr_rework_enabled value that
  // represents it (null = default-ON). Absent for the run/notification branches.
  settingsValue?: boolean | null;
  // Present for the run/notification branches: the fixture id backing it.
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
    description: "Per-MR cap hit — flagged via the halt inbox notification (M3).",
    fixtureId: "ntf-mr-rework-capped",
  },
];
