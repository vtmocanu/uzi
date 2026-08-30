// The in-browser mock implementation of the API client. Same surface, same
// response shapes, zero network: every method resolves from the in-memory store
// after a small jittered delay (so loading states render believably). Board
// moves, template CRUD, worker tokens, run inputs — all work locally.

import {
  type AdminBlockedRepos,
  type AutoStatus,
  type BindMode,
  type AgentSelectionInput,
  type AgentSourceApplyResult,
  type AgentSourceView,
  type AgentTemplate,
  type AgentTemplateInput,
  type AllocatedSkill,
  type AllocationsInput,
  type AppSettings,
  type Board,
  type BoardPrefs,
  type BuiltinDefinition,
  type Chat,
  type CliAuthRequestMeta,
  type CliToken,
  type CliTokenScope,
  type Card,
  type CreatedIssue,
  type Disposition,
  type IssueDraft,
  type IncidentalFinding,
  type IncidentalFindingBacklog,
  type IncidentalFindingBucket,
  type IncidentalFindingFileResult,
  type IncidentalFindingIssueDraft,
  type JudgeBacklog,
  type JudgeBacklogBucket,
  type JudgeCategoryStats,
  type JudgeDispositionCoord,
  type JudgeDispositionResult,
  type JudgeSettledMember,
  type JudgeDispositionScope,
  type JudgeOccurrence,
  type JudgeRecommendationGroup,
  type ReviewVerdict,
  type Memory,
  type Notification,
  type NotificationList,
  type PendingJudge,
  type PrivilegeReport,
  type ProjectSyncOwnerKind,
  type ProjectSyncStatus,
  type RecommendationCategory,
  type Run,
  type RunPriority,
  type CatalogEntry,
  type Schedule,
  type ScheduleInput,
  type SchedulePreviewInput,
  type RunMessage,
  type SettingSource,
  type SettingsResponse,
  type SlackLink,
  type UpdateSettingsPayload,
  type RunInputKind,
  type SecretMeta,
  type Skill,
  type SkillCreateInput,
  type SkillUpdateInput,
  type TemplateAllocation,
  type TemplateAllocationsInput,
  type TriageCounts,
  type ToolAllowlistEntry,
  type ToolAllowlistWriteInput,
  type User,
  type UserSettings,
  type UserSettingsPatch,
} from "../lib/api";
// ApiError / isTerminalRun are imported from their own leaf modules (not the
// `../lib/api` barrel) so this mock-mode client introduces no runtime import
// edge back to lib/api.ts — the api → mockApi → api cycle behind issue #165.
import { ApiError } from "../lib/apiError";
import { isTerminalRun } from "../lib/runStatus";
import { isTheme, resolveTheme } from "../lib/theme";
import { coordKey, recommendationLabel, verdictLabel } from "../lib/judge";
import { bodyError, descriptionError, SKILL_NAME_RE } from "../lib/skills";
import { CAPABILITY_VOCABULARY } from "../lib/capabilityVocabulary";
import {
  LIVE_RUN_ID,
  MOCK_CLI_AUTH_REQUEST_ID,
  mockAdmin,
  mockBlockedRepoMeta,
  mockAdminRateLimits,
  mockAdminWorkers,
  mockAllocations,
  mockBuildInfo,
  mockCliAuthRequest,
  mockCliTokens,
  mockMemories,
  mockConnection,
  mockForgeConfig,
  mockMyRateLimitsByUser,
  mockMyTokenRateLimits,
  mockAgentSource,
  mockFindings,
  type MockFinding,
  mockNotifications,
  mockOtherRunOwners,
  type MockNotification,
  mockRepos,
  mockPendingJudges,
  mockReviews,
  type MockReview,
  mockRepoToolProfiles,
  mockRunInputs,
  mockSecrets,
  mockShippedBuiltins,
  mockSkills,
  mockTemplates,
  mockToolAllowlist,
  mockUsers,
  mockWorkers,
  runListItem,
} from "./data";
import { ensureLive, handleInput, scheduleChatReply, startNewRun } from "./engine";
import { appendMessage, getProposal, getRun, nextRunId, patchRun, putProposal, state } from "./store";

const jitter = () => 90 + Math.random() * 180;
const delay = <T>(value: T, ms = jitter()): Promise<T> =>
  new Promise((resolve) => setTimeout(() => resolve(value), ms));

function requireSession(): User {
  if (!state.session) throw new ApiError(401, "authentication required");
  return state.session;
}

// ── Settings persistence (demo build) ────────────────────────────────────────
// The mock persists ONLY the settings maps to localStorage so a hard reload of
// the demo keeps the picked theme (and labels / worker model) instead of snapping
// back to seed — making no-flash + persistence witnessable end to end in the
// sanctioned preview vehicle. Runs, issues, workers, secrets etc. are
// deliberately NOT persisted. Versioned + shape-checked: a blob from an older
// seed schema (or a corrupt one) is discarded and re-seeded, never served, so
// stale demo state can't outlive a seed-schema change.
// Bumped to v2 for PRD #47 (the six health_* keys joined AppSettings): a stale v1
// blob lacks them, so discarding it re-seeds a complete shape.
// Bumped to v3 for PRD #69 M4 (judge_enforce_all / judge_cooldown_seconds /
// judge_daily_budget joined AppSettings, judge_model joined UserSettings): a stale v2
// blob lacks them, so discarding it re-seeds a complete shape.
const MOCK_SETTINGS_KEY = "uzi.mock.v3";
const SEED_USER_SETTINGS: UserSettings = {
  default_model: null,
  default_effort: null,
  judge_model: null,
  summary_model: null,
  theme: null,
  sidebar_token_ids: [],
  // PRD #700 M6: MR review watcher per-user opt-in. null = the default-ON state;
  // an explicit false opts the account out.
  mr_rework_enabled: null,
};
const SEED_APP_SETTINGS: AppSettings = {
  autopilot_label: "autopilot",
  // PRD #764: the single run-eligibility label.
  uzi_label: "uzi",
  default_theme: "ember",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  // PRD #69: enforced mode off, spend guards at their server defaults (cooldown 60s,
  // budget 0 = unlimited).
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
  // PRD #529 / #649 M1: ephemeral worker auto-provisioning instance kill-switch, default OFF.
  ephemeral_workers_enabled: "false",
  // PRD #362 Decision 8: the run-summary generator model, haiku by default.
  summary_model: "haiku",
  health_enabled: "true",
  health_stall_seconds: "300",
  health_slow_seconds: "2700",
  health_queued_seconds: "600",
  health_approval_seconds: "3600",
  health_nudge_cooldown_seconds: "1800",
  docker_repo_allowlist: "",
  // PRD #84 M2: capability-aware scheduling kill-switch, default ON.
  capability_aware_scheduling: "true",
  // Issue #534 M2: GitHub Projects v2 sync instance kill-switch, default OFF.
  github_project_sync_enabled: "false",
  // PRD #685: instance branding config, all string-space. Fresh installs are
  // unbranded (app_logo_mode "default", brand_mode "none").
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_keep_name: "true",
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: "false",
};

interface PersistedSettings {
  v: 1;
  userSettings: UserSettings;
  appSettings: AppSettings;
}

// isPersistedSettings validates the version AND the shape (key presence + value
// types) so only a blob matching the current schema is trusted; anything else
// falls through to a fresh seed.
function isPersistedSettings(p: unknown): p is PersistedSettings {
  if (typeof p !== "object" || p === null) return false;
  const o = p as Record<string, unknown>;
  if (o.v !== 1) return false;
  const us = o.userSettings;
  const as = o.appSettings;
  if (typeof us !== "object" || us === null || typeof as !== "object" || as === null) return false;
  const u = us as Record<string, unknown>;
  const a = as as Record<string, unknown>;
  const okUser =
    (u.default_model === null || typeof u.default_model === "string") &&
    // Optional so a pre-#617 blob stays valid; absent reads as inherit, and the SEED
    // provides it going forward (like judge_model).
    (u.default_effort === undefined || u.default_effort === null || typeof u.default_effort === "string") &&
    // Optional so a pre-#69 blob stays valid; absent reads as inherit.
    (u.judge_model === undefined || u.judge_model === null || typeof u.judge_model === "string") &&
    // Optional so a pre-#362 blob stays valid; absent reads as inherit.
    (u.summary_model === undefined || u.summary_model === null || typeof u.summary_model === "string") &&
    (u.theme === null || typeof u.theme === "string") &&
    // Optional so a pre-#700 blob stays valid; absent/null reads as the default-ON state.
    (u.mr_rework_enabled === undefined ||
      u.mr_rework_enabled === null ||
      typeof u.mr_rework_enabled === "boolean") &&
    // Optional so a pre-feature blob stays valid; absent reads as default-only.
    (u.sidebar_token_ids === undefined ||
      (Array.isArray(u.sidebar_token_ids) &&
        u.sidebar_token_ids.every((id) => typeof id === "string")));
  const okApp =
    typeof a.autopilot_label === "string" &&
    // PRD #764: accept legacy blobs that predate this field (undefined) — it is filled
    // from the seed default ("uzi") on load — but reject a malformed non-string.
    (a.uzi_label === undefined || typeof a.uzi_label === "string") &&
    typeof a.default_theme === "string" &&
    // PRD #649: accept legacy blobs that predate this field (undefined), but reject a
    // malformed non-string so a bad localStorage blob can't violate the AppSettings contract.
    (a.ephemeral_workers_enabled === undefined || typeof a.ephemeral_workers_enabled === "string") &&
    typeof a.slack_enabled === "string" &&
    typeof a.public_base_url === "string" &&
    typeof a.judge_enabled === "string" &&
    typeof a.judge_model === "string" &&
    typeof a.judge_enforce_all === "string" &&
    typeof a.judge_cooldown_seconds === "string" &&
    typeof a.judge_daily_budget === "string" &&
    // Optional so a pre-#362 blob stays valid; a missing summary_model is filled
    // from the seed default ("haiku") on load.
    (a.summary_model === undefined || typeof a.summary_model === "string") &&
    typeof a.health_enabled === "string" &&
    typeof a.health_stall_seconds === "string" &&
    typeof a.health_slow_seconds === "string" &&
    typeof a.health_queued_seconds === "string" &&
    typeof a.health_approval_seconds === "string" &&
    typeof a.health_nudge_cooldown_seconds === "string" &&
    typeof a.docker_repo_allowlist === "string";
  return okUser && okApp;
}

function loadSettings(): { userSettings: UserSettings; appSettings: AppSettings } {
  try {
    const raw = localStorage.getItem(MOCK_SETTINGS_KEY);
    if (raw) {
      const parsed: unknown = JSON.parse(raw);
      if (isPersistedSettings(parsed)) {
        return {
          // Merge over the seed so a pre-#362 blob (no summary_model on either
          // side) still yields a complete shape; the persisted values win where present.
          userSettings: { ...SEED_USER_SETTINGS, ...parsed.userSettings },
          appSettings: { ...SEED_APP_SETTINGS, ...parsed.appSettings },
        };
      }
    }
  } catch {
    // Storage unavailable (private mode) or a corrupt/legacy blob: re-seed.
  }
  return { userSettings: { ...SEED_USER_SETTINGS }, appSettings: { ...SEED_APP_SETTINGS } };
}

// persistSettings write-throughs the current settings maps. Called from the
// putMySettings / updateSettings mock handlers after they mutate.
function persistSettings(): void {
  try {
    const blob: PersistedSettings = { v: 1, userSettings, appSettings };
    localStorage.setItem(MOCK_SETTINGS_KEY, JSON.stringify(blob));
  } catch {
    // Storage unavailable: the demo still works in-memory for this session.
  }
}

const loadedSettings = loadSettings();

// Mutable copies of seed collections (CRUD operates on these).
let templates: AgentTemplate[] = mockTemplates.map((t) => ({ ...t }));
let users: User[] = mockUsers.map((u) => ({ ...u }));
let notifications: MockNotification[] = mockNotifications.map((n) => ({ ...n }));
// Incidental-findings coordinates (PRD #333 M7). Mutable copy so file/dismiss persist in a
// demo session; the seed stays pristine so a module reload re-seeds a clean backlog.
const findings: MockFinding[] = mockFindings.map((f) => ({ ...f, labels: [...f.labels], run_ids: [...f.run_ids] }));

// findingDTO projects a mock coordinate to the wire DTO, omitting the optional keys exactly as
// the server's `omitempty` tags do (a null finding_id / iid / resolved_at is simply absent).
function findingDTO(f: MockFinding): IncidentalFinding {
  return {
    ...(f.finding_id ? { finding_id: f.finding_id } : {}),
    location: f.location,
    repo_id: f.repo_id,
    repo_path: f.repo_path,
    status: f.status,
    last_title: f.last_title,
    seen_in_runs: f.seen_in_runs,
    ...(f.filed_issue_iid != null ? { filed_issue_iid: f.filed_issue_iid } : {}),
    ...(f.filed_issue_url ? { filed_issue_url: f.filed_issue_url } : {}),
    ...(f.resolved_at ? { resolved_at: f.resolved_at } : {}),
  };
}

// matchFindingBucket maps a disposition status to the ?bucket= filter (D7): to_file shows only
// open, filed/dismissed show their own status, all shows everything (the transient `filing` is
// invisible to to_file, exactly like the server).
function matchFindingBucket(status: MockFinding["status"], bucket: IncidentalFindingBucket): boolean {
  switch (bucket) {
    case "to_file":
      return status === "open";
    case "filed":
      return status === "filed";
    case "dismissed":
      return status === "dismissed";
    case "all":
      return true;
    default:
      return false;
  }
}
const reviews: MockReview[] = mockReviews.map((r) => ({
  ...r,
  recommendations: r.recommendations.map((x) => ({ ...x })),
  filed_issues: r.filed_issues.map((x) => ({ ...x })),
  dispositions: r.dispositions.map((x) => ({ ...x })),
}));
// Same mutable-copy pattern for the active judges (PRD #119), keyed by target run id:
// the seed stays pristine so a reload of the module re-seeds a clean demo, and this
// copy is what getRunReview reads and rerunJudge guards against.
const pendingJudges: Record<string, PendingJudge> = Object.fromEntries(
  Object.entries(mockPendingJudges).map(([id, p]) => [id, { ...p }]),
);

// recomputeTriage buckets a review's recommendations through the SAME precedence
// ladder as the server (dismissed > done > filed[settled] > todo), so the mock's
// per-review counts and the global stats can never drift from the panel (PRD #94 D#2).
// false_positives is the not_an_issue sub-count of dismissed.
function recomputeTriage(review: MockReview): TriageCounts {
  const dispByCoord = new Map(review.dispositions.map((d) => [coordKey(d.category, d.target), d]));
  const filedCoords = new Set(review.filed_issues.map((f) => coordKey(f.category, f.target)));
  const counts: TriageCounts = { total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 };
  for (const rec of review.recommendations) {
    counts.total += 1;
    const d = dispByCoord.get(coordKey(rec.category, rec.target));
    if (d?.status === "dismissed") {
      counts.dismissed += 1;
      if (d.reason === "not_an_issue") counts.false_positives += 1;
    } else if (d?.status === "done") {
      counts.done += 1;
    } else if (filedCoords.has(coordKey(rec.category, rec.target))) {
      counts.filed += 1;
    } else {
      counts.todo += 1;
    }
  }
  return counts;
}

// reviewDTO deep-clones a review for the wire and derives its server-computed fields:
// triage (via the ladder) and the disposition list filtered to coordinates that still
// have a current recommendation (D#7 — an orphaned disposition is inert, never sent).
function reviewDTO(review: MockReview): MockReview {
  const recCoords = new Set(review.recommendations.map((r) => coordKey(r.category, r.target)));
  return {
    ...review,
    recommendations: review.recommendations.map((x) => ({ ...x })),
    filed_issues: review.filed_issues.map((x) => ({ ...x })),
    dispositions: review.dispositions
      .filter((d) => recCoords.has(coordKey(d.category, d.target)))
      // set_via is STRIPPED here, deliberately. It is a mock-side extension of the stored
      // disposition (PRD #98 B3) because the run-page DispositionDTO carries no provenance —
      // only the Judge menu's occurrence does. A spread would leak it onto
      // GET /runs/{id}/review, where the real API sends no such field, and the mock would be
      // lying about the wire: a future RunView provenance feature would work in demo mode
      // and have nothing to read in production.
      .map(({ set_via: _setVia, ...d }) => ({ ...d })),
    triage: recomputeTriage(review),
  };
}

// bucketOf is the ladder itself (dismissed > done > filed(settled) > todo), and it mirrors
// workersvc.BucketOf(dispositionStatus string, filedSettled bool) argument for argument.
// The signature is the point: the server's ladder is a function of TWO SQL-resolved
// values, so a version of it that takes a mock review object cannot be handed the server's
// input and cannot be compared against it. This one can — fixtures/judge-fidelity feeds
// the identical (disposition_status, filed_settled) pair to both.
export function bucketOf(dispositionStatus: string | null, filedSettled: boolean): JudgeBacklogBucket {
  if (dispositionStatus === "dismissed") return "dismissed";
  if (dispositionStatus === "done") return "done";
  if (filedSettled) return "filed";
  return "todo";
}

// bucketOfRec resolves ONE recommendation's coordinate against a mock review's dispositions
// and filed issues, then defers to bucketOf. It is the mock's stand-in for the SQL join —
// the two LEFT JOINs that produce disposition_status and filed_settled — and deliberately
// contains no precedence of its own, so the ladder exists exactly once on this side.
function bucketOfRec(review: MockReview, category: string, target: string): JudgeBacklogBucket {
  const disp = review.dispositions.find((d) => d.category === category && d.target === target);
  const filed = review.filed_issues.some((f) => f.category === category && f.target === target);
  return bucketOf(disp?.status ?? null, filed);
}

// computeTriage sums the per-recommendation ladder over EVERY review the caller owns —
// the canonical /me/judge/stats aggregate the nav badge, the strip, and the Judge page's
// tabs all read (never tallied off a filtered/grouped list). The demo owns three reviews.
function computeTriage(): TriageCounts {
  const total: TriageCounts = { total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 };
  for (const review of reviews) {
    const t = recomputeTriage(review);
    total.total += t.total;
    total.todo += t.todo;
    total.filed += t.filed;
    total.done += t.done;
    total.dismissed += t.dismissed;
    total.false_positives += t.false_positives;
  }
  return total;
}

// computeCategoryStats recomputes the per-category GROUP count MATRIX the server's
// category-stats aggregate serves (PRD #270): for each triage bucket, the number of groups in
// that bucket per category, plus an `all` slice that is the whole-backlog per-category count.
//
// It reuses the grouper deliberately now — the count is TAB-SCOPED, so it must roll up the
// same (todo if any open member, else the group's highest settled rung) that the backlog rows
// do. It runs over the raw, UNCAPPED rows (backlogRowsFromReviews, never the capped path) with
// no bucket or category filter, so the matrix is comparable to the server's uncapped aggregate
// and never understates under truncation. It is anchor-aware: with a run anchor it semi-joins
// exactly as computeBacklog does (keep a group iff it recurs in the anchor run, keeping all of
// its occurrences).
//
// GROUPS, never rows: groupJudgeRecommendations already dedups by (category, target) across
// reviews, so a coordinate recurring across several reviews counts ONCE. Each group is tallied
// into its OWN bucket and into `all`, so `todo+filed+done+dismissed == all` per category holds
// by construction. All five bucket keys are always present (each `{}` when empty). The count is
// TRIAGE-VARIANT: a disposition moves a group between buckets, so it is recomputed on demand.
function computeCategoryStats(runAnchor = ""): JudgeCategoryStats {
  let groups = groupJudgeRecommendations(backlogRowsFromReviews());
  if (runAnchor) {
    groups = groups.filter((g) => g.occurrences.some((o) => o.run_id === runAnchor));
  }
  const matrix: Record<string, Record<string, number>> = {
    todo: {},
    filed: {},
    done: {},
    dismissed: {},
    all: {},
  };
  for (const g of groups) {
    for (const bucketKey of [g.bucket, "all"]) {
      matrix[bucketKey][g.category] = (matrix[bucketKey][g.category] ?? 0) + 1;
    }
  }
  return { counts_by_bucket: matrix };
}

// RATIONALE_PREVIEW_MAX mirrors the server's RationalePreviewMaxRunes (280), and the count
// is in RUNES — Array.from iterates code points, exactly as Go's []rune(s) does in
// workersvc.rationalePreview. `s.length` / `s.slice` count UTF-16 code units, which is a
// different number for anything outside the BMP.
//
// This was a live divergence, not a hypothetical, and it produced THREE distinct defects
// (measured against the Go implementation, PRD #98 seam 6):
//   1. a different answer to "was this cut" — at 200 rocket emoji the server returns the
//      string whole and the code-unit mock truncated it;
//   2. a different cut LENGTH when both cut — at 300 emoji the server yields 281 runes and
//      the code-unit mock yielded 141;
//   3. a LONE SURROGATE when the cut landed mid-pair — precisely the broken glyph
//      judge_backlog.go:64-67 says the rune count exists to prevent.
// Pinned by fixtures/judge-fidelity, cases preview-multibyte-cut and
// preview-multibyte-no-cut; the second is the one that can tell this version from the
// code-unit one, because past 280 runes both cut in the same place.
const RATIONALE_PREVIEW_MAX = 280;
function rationalePreview(s: string): string {
  const runes = Array.from(s);
  if (runes.length <= RATIONALE_PREVIEW_MAX) return s;
  // The character class is SPELLED OUT to match Go's TrimRight cutset " \t\r\n" exactly,
  // and it is a SEPARATE divergence from the rune count above — switching to Array.from
  // does nothing for it. JS `\s` is much wider than that cutset: it also matches U+00A0
  // (NBSP), U+FEFF, U+2028, U+2029, U+000B, U+000C and the U+2000-U+200A run. Measured:
  // with rune 280 padded to each of NBSP / U+FEFF / U+2028, the server KEEPS the character
  // and `\s` stripped it, so the two previews differed by one rune with no cut-position
  // disagreement at all. Pinned by fixtures/judge-fidelity, case preview-trim-boundary.
  return runes.slice(0, RATIONALE_PREVIEW_MAX).join("").replace(/[ \t\r\n]+$/, "") + "…";
}

const BUCKET_RANK: Record<string, number> = { dismissed: 3, done: 2, filed: 1, todo: 0 };
const RANK_BUCKET: JudgeBacklogBucket[] = ["todo", "filed", "done", "dismissed"];

// JudgeBacklogRow is ONE ROW of the server's grouped read — the JSON shape of
// store.ListJudgeRecommendationRowsForUserRow. The keys are BYTE-IDENTICAL to that
// generated struct's json tags on purpose, because this type is the wire between the two
// implementations: fixtures/judge-fidelity/cases.json is decoded straight into the Go
// struct on one side and cast to this on the other, so the two graders are fed the same
// bytes rather than two hand-built adapters.
//
// Note which fields are JOIN OUTPUTS: disposition_status, set_via, filed_settled and the
// three filed-issue columns arrive already resolved from SQL. The mock's own review
// objects never reach groupJudgeRecommendations; backlogRowsFromReviews is the mock's
// stand-in for the join and it runs first.
export type JudgeBacklogRow = {
  review_id: string;
  run_id: string;
  verdict: string;
  run_title: string;
  rec_id: string;
  category: RecommendationCategory;
  target: string;
  rationale_md: string;
  confidence: JudgeOccurrence["confidence"];
  disposition_status: string | null;
  // dismiss_reason is projected by the query but the grouper never reads it (only the #94
  // triage tally does), so it is optional here — matching the Go decoder, which accepts a
  // fixture row that omits the key.
  dismiss_reason?: string | null;
  set_via: string | null;
  filed_settled: boolean;
  filed_issue_iid: number | null;
  filed_issue_url: string | null;
  filed_at: string | null;
};

// groupJudgeRecommendations mirrors workersvc.GroupJudgeRecommendations one-to-one: dedup
// the flat rows by (category, target), roll up (todo if any member is open, else the
// highest member rung), and sort by frequency (run_count DESC, then open_count DESC).
//
// It expects the query's order — most-recently-JUDGED review first — so a group's FIRST
// row is its most-recent occurrence and supplies rationale_preview.
//
// The ?run= anchor is NOT here, and neither is the row cap: on the server both live in SQL
// (a coordinate-level semi-join and a LIMIT that cuts rows BEFORE grouping), so a mock that
// put them inside the grouper would be comparable to nothing. computeBacklog applies its
// anchor around this call, and says there why that is not the same algorithm.
export function groupJudgeRecommendations(rows: JudgeBacklogRow[]): JudgeRecommendationGroup[] {
  const byCoord = new Map<string, JudgeRecommendationGroup>();
  const runsSeen = new Map<string, Set<string>>();
  const topRung = new Map<string, number>();

  for (const row of rows) {
    const key = coordKey(row.category, row.target);
    const b = bucketOf(row.disposition_status, row.filed_settled);
    const occ: JudgeOccurrence = {
      run_id: row.run_id,
      run_title: row.run_title,
      review_id: row.review_id,
      rec_id: row.rec_id,
      verdict: row.verdict as ReviewVerdict,
      confidence: row.confidence,
      bucket: b,
      // Provenance rides alongside the bucket, because both a hand-marked and an
      // auto-done are bucket "done" (PRD #98 Decision 6 / review B3). Omitted rather than
      // nulled when absent, matching Go's `json:"set_via,omitempty"` on a "" string.
      ...(row.set_via ? { set_via: row.set_via as JudgeOccurrence["set_via"] } : {}),
      // filed_settled is the gate, not the presence of an iid — the same test
      // workersvc.filedIssueRef makes. filed_settled is `(f.filed_at IS NOT NULL)` in SQL,
      // so the ?? fallbacks below are unreachable through the query; they exist because Go
      // reads .Int64/.String off a NULL pgtype as the zero value rather than erroring, and
      // a fixture that hand-wrote that combination must not diverge over it.
      ...(row.filed_settled
        ? {
            filed_issue: {
              issue_iid: row.filed_issue_iid ?? 0,
              issue_url: row.filed_issue_url ?? "",
              filed_at: row.filed_at ?? "",
            },
          }
        : {}),
    };
    let g = byCoord.get(key);
    if (!g) {
      g = {
        category: row.category,
        target: row.target,
        bucket: "todo",
        open_count: 0,
        run_count: 0,
        // The first row of a group is its most-recent occurrence (query order).
        rationale_preview: rationalePreview(row.rationale_md),
        occurrences: [],
      };
      byCoord.set(key, g);
      runsSeen.set(key, new Set());
      topRung.set(key, 0);
    }
    g.occurrences.push(occ);
    if (b === "todo") g.open_count += 1;
    const seen = runsSeen.get(key)!;
    if (!seen.has(occ.run_id)) {
      seen.add(occ.run_id);
      g.run_count += 1;
    }
    topRung.set(key, Math.max(topRung.get(key)!, BUCKET_RANK[b]));
  }

  const groups = [...byCoord.values()];
  for (const g of groups) {
    const key = coordKey(g.category, g.target);
    g.bucket = g.open_count > 0 ? "todo" : RANK_BUCKET[topRung.get(key)!];
  }
  // Array.prototype.sort has been REQUIRED to be stable since ES2019, so ties keep the
  // first-seen (most-recent-first) order the query established — the same guarantee Go
  // buys with sort.SliceStable. The difference is where it comes from: here it is a
  // language guarantee, there it is a call-site choice that can be edited away.
  groups.sort((a, b) => b.run_count - a.run_count || b.open_count - a.open_count);
  return groups;
}

// filterGroups applies the ?bucket= filter to the grouped rows, mirroring
// workersvc.filterGroups: it matches the GROUP ROLLUP, so "todo" is exactly
// "open_count >= 1" and the settled rungs are mutually exclusive with it.
//
// Go additionally treats an EMPTY bucket as unfiltered, a route-level default this side
// cannot express — JudgeBacklogBucket is a closed union with no "" member — so that one
// branch is out of the fixture's reach. It is reachable in Go only from an internal caller
// that passes "", never from the wire.
export function filterGroups(
  groups: JudgeRecommendationGroup[],
  bucket: JudgeBacklogBucket,
): JudgeRecommendationGroup[] {
  return bucket === "all" ? [...groups] : groups.filter((g) => g.bucket === bucket);
}

// backlogRowsFromReviews flattens the mock's review objects into the server's row shape,
// in the query's order (rv.updated_at DESC — a re-judge counts as recent). This is the
// mock's stand-in for the SQL join, and it is deliberately OUTSIDE the grouper: the two
// LEFT JOINs that resolve disposition_status / filed_settled are the part seam 6 cannot
// compare, because on the server they are SQL and here they are two array lookups.
function backlogRowsFromReviews(): JudgeBacklogRow[] {
  const ordered = [...reviews].sort((a, b) => b.updated_at.localeCompare(a.updated_at));
  const rows: JudgeBacklogRow[] = [];
  for (const review of ordered) {
    for (const rec of review.recommendations) {
      const disp = review.dispositions.find((d) => d.category === rec.category && d.target === rec.target);
      const filed = review.filed_issues.find((f) => f.category === rec.category && f.target === rec.target);
      rows.push({
        review_id: review.id,
        run_id: review.target_run_id,
        verdict: review.verdict,
        run_title: getRun(review.target_run_id)?.issue_title ?? "",
        rec_id: rec.id,
        category: rec.category,
        target: rec.target,
        rationale_md: rec.rationale_md,
        confidence: rec.confidence,
        disposition_status: disp?.status ?? null,
        dismiss_reason: disp?.reason ?? null,
        set_via: disp?.set_via ?? null,
        // The mock has no unsettled-claim state: an entry in filed_issues IS a settled
        // link, which is what the query's (f.filed_at IS NOT NULL) computes.
        filed_settled: filed !== undefined,
        filed_issue_iid: filed?.issue_iid ?? null,
        filed_issue_url: filed?.issue_url ?? null,
        filed_at: filed?.filed_at ?? null,
      });
    }
  }
  return rows;
}

// MOCK_BACKLOG_MAX_ROWS is the demo's stand-in for the server's JudgeBacklogMaxRows (2000).
// It is small because the demo is: data.ts carries 11 rows across 3 reviews, and 6 is the
// value that cuts through the MIDDLE of a recurring coordinate rather than at a group
// boundary. A cut at a group boundary would only remove whole groups, which is the easy half
// of the state and not the half the banner is warning about.
export const MOCK_BACKLOG_MAX_ROWS = 6;

// capBacklogRows mirrors the cut in JudgeRecommendationBacklog (judge_backlog.go): rows are
// cut BEFORE grouping, and `truncated` says the cut actually removed something.
//
// THE ORDER IS THE ENTIRE POINT, and flipping a boolean instead would demo a lie. The banner
// means "there are groups you are not seeing, and a coordinate that did not come back is
// UNKNOWN rather than settled". A `truncated: true` sitting above COMPLETE data shows that
// warning over a screen that is in fact the whole truth, which teaches the reader the
// opposite of what the state means. Cutting rows first is also what makes a SURVIVING group
// under-report run_count and possibly roll up to the wrong bucket -- the subtlety the flag
// exists to warn about, and the reason the state is worth demoing at all.
//
// The comparison is `>` and not `>=` on purpose: a backlog of exactly `max` rows is NOT
// truncated. That off-by-one is the same one the server buys by reading `max + 1` rows, so
// that a full page is distinguishable from an exactly-full one without a second COUNT.
export function capBacklogRows(
  rows: JudgeBacklogRow[],
  max: number,
): { rows: JudgeBacklogRow[]; truncated: boolean } {
  if (rows.length <= max) return { rows, truncated: false };
  return { rows: rows.slice(0, max), truncated: true };
}

// backlogMaxRows returns the row cap in force. There is none by default, so an ordinary demo
// visitor can never reach this state by accident; the `truncated-backlog` demo scenario turns
// it on (`?mock=truncated-backlog`, or the uzi_mock_scenario localStorage key -- the same
// PRD #45 mechanism the OIDC demo states use). The scenario is a single string, so this state
// and the OIDC ones are mutually exclusive by construction; nothing needs both.
//
// WHY A DELIBERATE TOGGLE, rather than a permanently-capped demo or a test-only hook. M3's
// requirement is that the mock renders EVERY state, and truncation is the one a person most
// needs to SEE, because it is the only state in which the screen is not the truth -- and the
// one whose CLI remedy was measured outright false earlier in this PRD. A test-only hook
// satisfies "every state has a test" while leaving a human clicking the demo unable to reach
// it; a permanent cap would make the demo permanently wrong about everything else.
function backlogMaxRows(): number {
  return mockScenario() === "truncated-backlog" ? MOCK_BACKLOG_MAX_ROWS : Number.POSITIVE_INFINITY;
}

// computeBacklog assembles GET /me/judge/recommendations out of the pieces above, in the
// server's order: join (backlogRowsFromReviews) → category filter → cap → group → anchor →
// bucket filter. triage is the canonical aggregate, NEVER tallied from the returned groups.
function computeBacklog(
  bucket: JudgeBacklogBucket,
  runAnchor: string,
  categories: string[] = [],
): JudgeBacklog {
  // ?category= is a ROW filter applied BEFORE the cap, mirroring the SQL predicate that sits
  // before the LIMIT (PRD #235). Filtering here — not on the grouped output after the cap —
  // is the whole point: a category whose only rows sit past the cap would otherwise be
  // entirely off-page, so filtering groups post-cap reintroduces the exact off-page bug the
  // server design avoids. Empty/absent categories = all rows (a no-op).
  const rows = backlogRowsFromReviews();
  const selected = categories.length
    ? rows.filter((r) => categories.includes(r.category))
    : rows;
  // The cap sits between the (category-filtered) rows and the grouper, which is where the
  // server's LIMIT sits.
  const capped = capBacklogRows(selected, backlogMaxRows());
  let groups = groupJudgeRecommendations(capped.rows);
  // ?run= anchor: a coordinate-level semi-join — keep a group iff it recurs in the anchor
  // run, but keep ALL its occurrences (so a notification still shows the other runs it
  // recurs in). A foreign/unknown run matches nothing → empty, no existence oracle.
  //
  // This filters GROUPS after grouping; the server filters ROWS before it, inside the
  // query's WHERE. The two read as equivalent and are NOT the same algorithm, which is why
  // the anchor is excluded from the fidelity fixture (see fixtures/judge-fidelity/README.md)
  // and belongs to the e2e leg instead.
  if (runAnchor) {
    groups = groups.filter((g) => g.occurrences.some((o) => o.run_id === runAnchor));
  }

  return {
    bucket,
    run: runAnchor,
    groups: filterGroups(groups, bucket),
    truncated: capped.truncated,
    // triage is the canonical /me/judge/stats aggregate and is deliberately NOT affected by
    // the cut, exactly as on the server, where it comes from a separate query with no LIMIT.
    // That divergence is not a bug to reconcile: it is what the truncated state LOOKS like.
    // The badge keeps saying how many coordinates are actually open while the page shows
    // fewer, and a reader who trusted the page would be wrong.
    triage: computeTriage(),
  };
}

// Monotonic iid for issues the preview files (PRD #68), above the seeded #71.
let nextFiledIssueIid = 90;

// mockIssueDraft mirrors the server's deterministic templating (PRD #68 M2): the
// category→repo default resolved against the connected repos (an empty default → mock
// state D), the fenced body, the server-side `uzi` label (PRD #764), and a provenance
// line. Faithful enough for the preview to render every state, not a byte-for-byte copy
// of the Go renderer (its fence/strip/scan is unit-tested there).
function mockIssueDraft(
  runId: string,
  rec: MockReview["recommendations"][number],
  review: MockReview,
): IssueDraft {
  const label = recommendationLabel(rec.category);
  const enabledRepoIds = new Set(repos.filter((r) => r.enabled).map((r) => r.id));
  let default_repo_id = "";
  let default_note = "";
  if (rec.category === "improve_agent" || rec.category === "add_agent") {
    const rid = getRun(runId)?.repo_id ?? "";
    if (enabledRepoIds.has(rid)) {
      default_repo_id = rid;
      default_note =
        "Defaulted to the judged run's repo — repo agents live in its .claude/agents/. Pick any repo you have connected.";
    } else {
      default_note = "The judged run's repo isn't one you've connected. Pick the repo to file this against.";
    }
  } else {
    // PRD #590 M2: the uzi-own-repo default now comes from the caller's enabled
    // self_improve default schedule (server-side); the preview does not model that
    // schedule, so it renders mock state D (no default, pick a repo).
    default_note =
      "No uzi repo is configured on this instance (or it isn't one you've connected), so there's no default. Pick the repo to file this against.";
  }
  const description = [
    "## What the judge found",
    "",
    "````",
    rec.rationale_md,
    "````",
    "",
    "## Context",
    "",
    `- Recommendation: **${label}**${rec.target ? " — `" + rec.target + "`" : ""}${
      rec.confidence ? ` (${rec.confidence} confidence)` : ""
    }`,
    `- Verdict on the judged run: **${verdictLabel(review.verdict)}**`,
    "",
    "## Judge's summary of the run",
    "",
    "````",
    review.summary_md,
    "````",
    "",
    "---",
    "Opened by uzi on behalf of @vlad, from a run retrospective. The quoted text above is LLM-authored and unverified.",
  ].join("\n");
  return {
    default_repo_id,
    title: rec.target ? `${label}: ${rec.target}` : label,
    description,
    labels: ["uzi"],
    provenance: `from vlad's worker, run ${runId.slice(0, 8)}`,
    default_note,
  };
}
// Agent-source demo state (PRD #602 M5): a deep clone so mutations (config save,
// sync, apply) do not leak back into the shared fixture. The config half is edited
// through updateSettings' agent_source_* arm; the status/staged halves by sync/apply.
let agentSource: AgentSourceView = structuredClone(mockAgentSource);

// Persisted REMOTE facts from the last update check (PRD #702 M4), mirroring the
// server's persist-facts/derive-at-read split: the update-check writes these, and
// getAgentSource DERIVES update_available/latest_ref from them + the LIVE config at
// read time — so a pin bump or apply self-clears the badge with no new egress. Null
// until a check has run, so a fresh install shows no badge.
let agentSourceRemote: { latestRef: string; tipSha: string; checkedAt: string } | null = null;

// parseAgentSourceSemver mirrors the server's re-prefix + IsValid guard (Decision 4):
// a non-`v`-prefixed or malformed ref is not a comparable semver (null), never treated
// as equal. Returns the [major, minor, patch] triple for a valid vX.Y.Z, else null.
function parseAgentSourceSemver(ref: string): [number, number, number] | null {
  const m = /^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/.exec(ref.trim());
  if (!m) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

// compareAgentSourceSemver returns >0 when a is newer than b (numeric per-component,
// so v1.10.0 sorts ABOVE v1.2.0 — a lexical compare gets this wrong, the discriminating
// case Decision 4 calls out).
function compareAgentSourceSemver(a: [number, number, number], b: [number, number, number]): number {
  for (let i = 0; i < 3; i++) {
    if (a[i] !== b[i]) return a[i] - b[i];
  }
  return 0;
}

// deriveAgentSourceStatus computes the update-available fields at read time from the
// stored remote facts + the live config (Decision 6), mirroring the server:
//   - no check has run (remote null) → no update fields (no badge);
//   - 40-hex SHA pin → never an update;
//   - tag-pinned (valid semver ref) → update iff the newest remote semver tag is strictly
//     greater, naming it in latest_ref;
//   - branch-pinned/empty ref → update ("moved", latest_ref empty) iff the remote tip
//     differs from last_applied_sha.
function deriveAgentSourceStatus(): AgentSourceView["status"] {
  const base = agentSource.status;
  const remote = agentSourceRemote;
  if (!remote) return base;
  const ref = agentSource.config.ref.trim();
  let updateAvailable = false;
  let latestRef = "";
  if (/^[0-9a-f]{40}$/i.test(ref)) {
    // SHA-pinned: an immutable pin is intentionally frozen — no signal.
    updateAvailable = false;
  } else {
    const pinned = parseAgentSourceSemver(ref);
    if (pinned) {
      const cand = parseAgentSourceSemver(remote.latestRef);
      if (cand && compareAgentSourceSemver(cand, pinned) > 0) {
        updateAvailable = true;
        latestRef = remote.latestRef;
      }
    } else if (
      remote.tipSha &&
      remote.tipSha.toLowerCase() !== (base.last_applied_sha ?? "").toLowerCase()
    ) {
      // branch-pinned/empty: the advertised tip moved past what is applied. Compared
      // case-INSENSITIVELY to mirror the server's DeriveUpdate (strings.EqualFold), so a
      // mixed-case fixture can't drift the mock from the real derive.
      updateAvailable = true;
    }
  }
  return {
    ...base,
    update_available: updateAvailable,
    latest_ref: latestRef || undefined,
    update_checked_at: remote.checkedAt,
  };
}

// agentSourceView returns the current view with the update-available fields DERIVED at
// read time (Decision 6) — every read path returns this, so the badge is consistent.
function agentSourceView(): AgentSourceView {
  const clone = structuredClone(agentSource);
  clone.status = deriveAgentSourceStatus();
  return clone;
}

// mockAllowedAgentSourceHosts mirrors the server's SSRF allowlist: a save of a URL
// whose host is not on it is rejected with the same shaped 400 the real
// UpdateSettings surfaces. Empty URL (disable) is always allowed.
const mockAllowedAgentSourceHosts = ["github.com", "gitlab.com"];
let secrets: SecretMeta[] = mockSecrets.map((s) => ({ ...s }));

// requireUnlockedVault mirrors the real API: sealing a token needs the vault
// unlocked (PRD #32), so every create/rotate path throws the same 409 the SPA
// turns into an unlock prompt.
function requireUnlockedVault(): void {
  if (!state.vaultUnlocked) {
    throw new ApiError(409, "vault is locked; unlock it with your password, then save again", {
      code: "vault_locked",
    });
  }
}
let userSettings: UserSettings = loadedSettings.userSettings;
let workers = mockWorkers.map((w) => ({ ...w }));
let connections = [{ ...mockConnection }];
let repos = mockRepos.map((r) => ({ ...r }));
// PRD #534: a GitHub connection plus two GitHub repos so the Boards "Project
// sync" cell renders and its linked/unlinked panel states are exercisable under
// VITE_UZI_MOCK=1. gitlab (conn-1) stays FIRST so it remains the demo's default
// selection and the existing gitlab-oriented flows are untouched — select the
// GitHub bot to reach these rows. The sync cell keys off the SELECTED
// connection's forge_type, so both repos read as GitHub once conn-gh is picked.
connections = [
  ...connections,
  {
    ...mockConnection,
    id: "conn-gh",
    forge_type: "github",
    base_url: "https://github.com",
    bot_username: "uzi-bot-gh",
  },
];
repos = [
  ...repos,
  {
    ...repos[0],
    id: "repo-gh-linked",
    connection_id: "conn-gh",
    path_with_namespace: "vtmocanu/gh-linked",
    web_url: "https://github.com/vtmocanu/gh-linked",
    pipeline: null,
    guardrail_override: null,
    guardrail_blocked: false,
  },
  {
    ...repos[0],
    id: "repo-gh-unlinked",
    connection_id: "conn-gh",
    path_with_namespace: "vtmocanu/gh-unlinked",
    web_url: "https://github.com/vtmocanu/gh-unlinked",
    pipeline: null,
    guardrail_override: null,
    guardrail_blocked: false,
  },
];
// PRD #534: in-memory GitHub Projects v2 links, keyed by repo id. Seeded with
// repo-gh-linked so the "linked" readout is visible; repo-gh-unlinked is left
// absent so the provision/adopt state shows. Mirrors the server: getStatus 404s
// an unlinked repo, provision/adopt set the entry, disable deletes it.
const githubProjectLinks = new Map<string, ProjectSyncStatus>([
  [
    "repo-gh-linked",
    {
      project_number: 42,
      owned_by_uzi: true,
      last_synced_at: new Date(Date.now() - 5 * 60_000).toISOString(),
      last_error: null,
      item_count: 7,
    },
  ],
]);
// PRD #557: per-repo board visibility, keyed by repo id. A linked board defaults
// to private (false) — GitHub creates ProjectV2 boards private — and the toggle
// round-trips a mutable value here so the vitest toggle case is non-vacuous.
const githubProjectVisibility = new Map<string, boolean>();

// ── Scheduled runs (PRD #241) demo fixtures + helpers ──────────────────────
// schedulePreviewCap mirrors the server's clamp on the preview N (PRD #241 M4).
const schedulePreviewCap = 10;
let scheduleSeq = 700;
const nextScheduleId = () => `sch-${(scheduleSeq++).toString(36)}`;

// The server caps owner guidance at 8 KiB (MaxGuidanceBytes) and 422s an oversize value on
// every write path (validateScheduleConfig, shared by create and update). The Textarea's
// maxLength caps CHARACTERS, not UTF-8 BYTES, so a multibyte value can pass the input yet
// exceed the byte cap — validate bytes on both mock write paths so mock mode reproduces the
// production 422 rather than silently accepting oversized guidance.
const MAX_GUIDANCE_BYTES = 8 * 1024;
function assertGuidanceWithinCap(guidance: string | null | undefined): void {
  if (guidance != null && new TextEncoder().encode(guidance).length > MAX_GUIDANCE_BYTES) {
    throw new ApiError(422, "guidance is too large");
  }
}

// mockScheduleFires computes the next N fire instants (UTC ISO) for a 5-field
// cron string. It handles the canonical preset shapes (specific min/hour, `1-5`,
// single dow, `*/N` steps) — enough for the demo + tests — and returns [] for
// anything it does not understand (a day-of-month/month restriction), which the
// UI renders as an empty preview exactly as a real invalid cron would.
function mockScheduleFires(cron: string, n: number, from = new Date()): string[] {
  const fields = cron.trim().split(/\s+/);
  if (fields.length !== 5) return [];
  const [minF, hrF, domF, monF, dowF] = fields;
  if (domF !== "*" || monF !== "*") return [];
  const expand = (f: string, max: number): number[] => {
    if (f === "*") return Array.from({ length: max + 1 }, (_, i) => i);
    const step = /^\*\/(\d{1,2})$/.exec(f);
    if (step) {
      const s = Number(step[1]);
      const out: number[] = [];
      for (let i = 0; i <= max; i += s) out.push(i);
      return out;
    }
    const range = /^(\d{1,2})-(\d{1,2})$/.exec(f);
    if (range) {
      const out: number[] = [];
      for (let i = Number(range[1]); i <= Number(range[2]); i++) out.push(i);
      return out;
    }
    if (/^\d{1,2}$/.test(f)) return [Number(f)];
    return [];
  };
  const minutes = expand(minF, 59);
  const hours = expand(hrF, 23);
  const dows = dowF === "*" ? null : expand(dowF, 7).map((d) => d % 7);
  if (minutes.length === 0 || hours.length === 0) return [];
  const out: string[] = [];
  const start = new Date(Date.UTC(from.getUTCFullYear(), from.getUTCMonth(), from.getUTCDate()));
  for (let day = 0; day < 400 && out.length < n; day++) {
    const base = new Date(start.getTime() + day * 86_400_000);
    if (dows && !dows.includes(base.getUTCDay())) continue;
    for (const h of hours) {
      for (const mi of minutes) {
        const t = Date.UTC(base.getUTCFullYear(), base.getUTCMonth(), base.getUTCDate(), h, mi);
        if (t > from.getTime() && out.length < n) out.push(new Date(t).toISOString());
      }
    }
  }
  return out.slice(0, n);
}

// scheduleDTO recomputes the live next-fire preview at read time, exactly as the
// server does — the list and the modal preview then agree by construction.
function scheduleDTO(s: Schedule): Schedule {
  let nextFires: string[] = [];
  let nextFireAt: string | null = null;
  if (s.status === "active" && s.enabled) {
    if (s.timing === "recurring") {
      nextFires = mockScheduleFires(s.cron_expr, 3);
      nextFireAt = nextFires[0] ?? null;
    } else if (s.run_at && new Date(s.run_at).getTime() > Date.now()) {
      nextFireAt = s.run_at;
    }
  }
  return { ...s, next_fire_at: nextFireAt, next_fires: nextFires };
}

const daysFromNow = (d: number, h: number, m = 0): string => {
  const t = new Date();
  t.setUTCHours(h, m, 0, 0);
  t.setUTCDate(t.getUTCDate() + d);
  return t.toISOString();
};

// The owner's user-authored schedules (origin='user'). Materialized default rows
// (origin='default') are appended below from the catalog fixture; keeping the two
// lists separate lets the user rows stay free of the three origin fields, which the
// map injects uniformly.
const userSchedules: Omit<
  Schedule,
  "origin" | "catalog_slug" | "customized" | "sibling_group_id" | "baked_guidance"
>[] = [
  {
    id: "sch-7kd2", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "sweep", issue_iid: null, labels: null, prompt: "",
    timing: "recurring", cron_expr: "0 2 * * 1-5", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(-1, 2), auto_approve: true, wait_on_limit: true,
    max_issues: 1,
    guidance: "Keep the diff small and add a failing test first.",
    model: "fable",
    override_subagent_model: true,
    enabled: true, status: "active", created_at: daysFromNow(-14, 9),
    updated_at: daysFromNow(-1, 2), next_fires: [],
    // Fired on time, started nothing: the one candidate within the cap was skipped
    // for a benign reason, and `capped` says there were older candidates behind it —
    // the amber cell + the cap hint (Goal 2). The whole point of PRD #308.
    last_fire: {
      fired_at: daysFromNow(-1, 2), matched: 1, capped: true,
      started: [],
      skips: [
        {
          issue_iid: 96,
          title: "Mid-run worker restart discards all un-pushed commits on resume",
          reason: "not_eligible",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/96",
        },
      ],
    },
  },
  {
    id: "sch-3bf1", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "issue", issue_iid: 142, labels: null, prompt: "",
    timing: "recurring", cron_expr: "0 3 * * *", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(0, 3), auto_approve: false, wait_on_limit: true,
    max_issues: null,
    guidance: "Prefer the smallest change that closes the issue; no new deps.",
    model: null,
    override_subagent_model: false,
    enabled: true, status: "active", created_at: daysFromNow(-9, 10),
    updated_at: daysFromNow(0, 3), next_fires: [],
    // A healthy fire: it started the run it matched (green "1 started").
    last_fire: {
      fired_at: daysFromNow(0, 3), matched: 1, capped: false,
      started: [
        {
          issue_iid: 142,
          run_id: "3f1a2b7c-9d4e-4a1b-8c6d-1e2f3a4b5c6d",
          title: "RunKind (TypeScript) omits 'chat', which the DB CHECK allows",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/142",
        },
      ],
      skips: [],
    },
  },
  {
    id: "sch-9qm4", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "issue", issue_iid: 158, labels: null, prompt: "",
    timing: "once", cron_expr: "", run_at: daysFromNow(1, 9),
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: null, auto_approve: true, wait_on_limit: false,
    max_issues: null,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true, status: "active", created_at: daysFromNow(-1, 20),
    updated_at: daysFromNow(-1, 20), next_fires: [], last_fire: null,
  },
  {
    id: "sch-pr0m", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "prompt", issue_iid: null, labels: null,
    prompt: "hunt for flaky tests and open an MR",
    timing: "recurring", cron_expr: "0 9 * * 1", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(-7, 9), auto_approve: true, wait_on_limit: false,
    max_issues: null,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true, status: "active", created_at: daysFromNow(-21, 11),
    updated_at: daysFromNow(-7, 9), next_fires: [], last_fire: null,
  },
  {
    id: "sch-zt88", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas-api",
    target: "sweep", issue_iid: null, labels: ["bug"], prompt: "",
    timing: "recurring", cron_expr: "0 */6 * * *", run_at: null,
    timezone: "UTC", next_fire_at: null,
    last_fired_at: daysFromNow(-3, 18), auto_approve: true, wait_on_limit: false,
    max_issues: 3,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: false, status: "active", created_at: daysFromNow(-30, 8),
    updated_at: daysFromNow(-3, 18), next_fires: [],
    // A healthy sweep: every matched candidate started a run (green "3 started",
    // each pairing issue ↔ run in the expanded panel).
    last_fire: {
      fired_at: daysFromNow(-3, 18), matched: 3, capped: false,
      started: [
        { issue_iid: 124, run_id: "a20b4e51-77c8-4d2a-9f10-2b3c4d5e6f70", title: "web: judge free text renders without Unicode Cf stripping", web_url: "https://gitlab.example.com/vtmocanu/atlas-api/-/issues/124" },
        { issue_iid: 139, run_id: "c7d5f0a2-1e34-4b56-88a9-0c1d2e3f4a5b", title: "Poller sync timeouts against forge-fake in the e2e stack" },
        { issue_iid: 151, run_id: "e91f6b03-42d7-4c88-b1a2-3c4d5e6f7a80", title: "Board card CI badge flickers on refetch" },
      ],
      skips: [],
    },
  },
  {
    // A parked schedule (status='error'): the last fire failed and the scheduler
    // stopped advancing it, so the list shows the red "parked" badge and an "error"
    // Next-run pill. Demoing this state is the whole reason it's a seed row.
    id: "sch-er0r", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "issue", issue_iid: 173, labels: null, prompt: "",
    timing: "recurring", cron_expr: "30 1 * * *", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(-1, 1, 30), auto_approve: true, wait_on_limit: false,
    max_issues: null,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true, status: "error", created_at: daysFromNow(-12, 15),
    updated_at: daysFromNow(-1, 1, 30), next_fires: [], last_fire: null,
  },
];

// The builtin default-jobs catalog (PRD #589), mirroring
// api/internal/schedtmpl/catalog/*.md. A prompt entry carries the file body as
// `prompt` (labels/guidance empty, max_issues 0); a sweep entry carries its selector
// `labels` plus the body as `guidance` (prompt empty). auto_approve/wait_on_limit are
// the fixed run flags every default is seeded with (schedtmpl.AutoApprove/WaitOnLimit).
const scheduleCatalog: CatalogEntry[] = [
  {
    slug: "test-improvement",
    name: "Weekly test improvement",
    description: "Weekly pass that finds one under-tested area and strengthens its tests.",
    target: "prompt", cron: "0 8 * * 1", timezone: "UTC", model: "",
    prompt:
      "Spend this run improving the project's automated tests. Pick ONE area that is meaningfully under-tested — a module with thin coverage, an important branch with no assertion, or a bug-prone path — and add focused, genuinely useful tests for it. Prefer a small number of high-value tests over many shallow ones, and run the project's test suite to confirm your additions pass.\n\nMake every test earn its place: prove each assertion is non-vacuous by identifying a plausible defect that would make it fail (if you sanity-check by mutating the code under test, do it in a throwaway copy and never a production file, and change it to a value another case already produces, not a fresh sentinel); assert the observable end-state, not an intermediate call; prefer positive assertions over negative ones; never weaken or delete an existing assertion to make a suite pass; and do not re-touch a test another recent run just changed — pick a different area so parallel runs do not collide.\n\nGuardrail: change TEST files only — no production (non-test) file; a behavior needing a production change to be testable is out of scope (pick another area), and a real production bug found while testing is reported, not fixed. Commit your new tests and open a merge request; if nothing worthwhile this week an empty week is acceptable — do not invent low-value tests to hit a number — open no MR and leave a note on what you looked at.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "docs-hygiene",
    name: "Docs hygiene",
    description: "Weekly sweep for mechanical documentation defects — dead links, stale references, drift.",
    target: "prompt", cron: "0 3 * * 1", timezone: "UTC", model: "",
    prompt:
      "Audit the project's documentation for mechanical defects: broken or moved links, references to files or commands that no longer exist, stale frontmatter, and obvious typos. Focus on correctness, not rewriting for style. Verify each problem against the actual repository before fixing it; when a broken link has more than one plausible target, describe it rather than guessing the repoint.\n\nApply the mechanical corrections and open a merge request. Guardrail: mechanical fixes only (links, stale refs, frontmatter, typos), documentation files only — no prose rewrites and no source/build/CI/agent-config edits, keep the diff to mechanical corrections. If there is nothing to fix, open no MR and leave a note.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "bug-hunt",
    name: "Bug hunt — deep audit",
    description: "Deep audit of one subsystem for correctness bugs, confirmed by a reviewer and an auditor.",
    target: "prompt", cron: "0 4 * * 3", timezone: "UTC", model: "",
    prompt:
      "Pick ONE subsystem and audit it deeply for real correctness bugs: unhandled errors, race conditions, off-by-one and boundary mistakes, incorrect edge-case handling, and broken invariants. For every candidate bug, construct the concrete input or state that triggers it and confirm the wrong behavior by reading the code carefully; have a reviewer and an auditor confirm each finding before you rely on it, and discard anything you cannot substantiate.\n\nFor the single highest-confidence bug, apply the smallest correct fix backed by a deterministic test that would have caught it (fails reliably before, passes after; skip the test only for a non-code fix or a genuinely contrived reproduction, and say why), commit it, and open one merge request. If you find no clearly-real bug, open no MR and leave your audit notes as a report.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "feature-bingo",
    name: "Feature bingo",
    description: "Weekly brainstorm that proposes one concrete new feature and opens an MR adding it as an idea file.",
    target: "prompt", cron: "0 3 * * 2", timezone: "UTC", model: "fable",
    prompt:
      "Brainstorm ONE concrete, genuinely useful new feature or improvement for this project. Ground it in what the codebase actually does: name the problem it solves, sketch how it would work, and note roughly where it would live and what it would touch.\n\nFirst read the existing files under the `ideas/` folder to avoid duplicates, and check the codebase so you do not propose something that already exists. Write your proposal to a single new idea file under the `ideas/` folder at the repository root, commit it, and open a merge request titled `bingo: <feature>`. If nothing worthwhile comes to mind, open no MR and leave a note.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "bug-triage",
    name: "Bug triage sweep",
    description: "Daily sweep over open issues labelled \"bug\", starting a run for the oldest few.",
    target: "sweep", cron: "0 2 * * *", timezone: "UTC", model: "",
    prompt: "", labels: ["bug"], max_issues: 3, auto_approve: true, wait_on_limit: true,
    guidance:
      "Triage the sweep's bug issue. Reproduce or confirm the reported problem, find its root cause, and fix it if the fix is small and well-contained; otherwise document the diagnosis and the minimal reproduction so a maintainer can act. Keep changes scoped to the bug at hand and back any fix with a test that would have caught it.",
  },
  {
    slug: "planned-sweep",
    name: "Planned-work sweep",
    description: "Daily sweep over open issues labelled \"Planned\", starting a run for the oldest few.",
    target: "sweep", cron: "0 2 * * *", timezone: "UTC", model: "",
    prompt: "", labels: ["Planned"], max_issues: 3, auto_approve: true, wait_on_limit: true,
    guidance:
      "Implement the sweep's planned-work issue. Treat the issue description (and any linked spec) as the specification, deliver the change end to end with tests, and run the project's gate before finishing. Keep the work scoped to what the issue asks for and stop to report if it turns out to depend on something not yet in place.",
  },
  {
    slug: "assigned-sweep",
    name: "Assigned-work sweep",
    description: "Daily sweep over open issues assigned to the uzi bot account, starting a run for the oldest few.",
    target: "sweep", cron: "0 2 * * *", timezone: "UTC", model: "",
    prompt: "", labels: null, max_issues: 3, auto_approve: true, wait_on_limit: true,
    guidance:
      "Implement the sweep's assigned issue. This sweep selects by assignee rather than a label, so there is no selector label to match. Treat the issue description (and any linked spec) as the specification, deliver the change end to end with tests, and run the project's gate before finishing. Keep the work scoped to what the issue asks for and stop to report if it turns out to depend on something not yet in place.",
  },
  {
    // A self_improve entry (PRD #590) carries neither a prompt nor labels/guidance: the
    // orchestration lead resolves its own tracking issue at fire time. So it edits like a
    // prompt default (cadence/model only) but has no baked text to show.
    slug: "self-improve",
    name: "Self-improvement",
    description: "Autonomous self-improvement — audit uzi's own codebase and open one improvement MR per cycle.",
    target: "self_improve", cron: "0 4 */2 * *", timezone: "UTC", model: "",
    prompt: "", labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
];

// materializeDefault builds an origin='default' schedule row from a catalog entry, as
// the server does when the owner enables a default on a repo (PRD #589 M2): the
// resolved catalog values are carried on the row so the modal can show the baked
// prompt read-only, even though the real DB stores NULL for those columns.
function materializeDefault(
  entry: CatalogEntry,
  repoId: string,
  id: string,
  over: Partial<Schedule> = {},
): Schedule {
  const repo = mockRepos.find((r) => r.id === repoId);
  const now = new Date().toISOString();
  return {
    id,
    repo_id: repoId,
    repo_path: repo?.path_with_namespace ?? "",
    target: entry.target,
    issue_iid: null,
    labels: entry.target === "sweep" && entry.labels ? [...entry.labels] : null,
    prompt: entry.target === "prompt" ? entry.prompt : "",
    timing: "recurring",
    cron_expr: entry.cron,
    run_at: null,
    timezone: entry.timezone,
    next_fire_at: null,
    last_fired_at: null,
    last_fire: null,
    auto_approve: entry.auto_approve,
    wait_on_limit: entry.wait_on_limit,
    max_issues: entry.target === "sweep" ? entry.max_issues : null,
    // Owner OVERLAY (issue #675): null by default; a seed sets it via `...over`.
    guidance: null,
    // Resolved catalog guidance for a sweep default, shown read-only (issue #675).
    baked_guidance: entry.target === "sweep" ? entry.guidance || null : null,
    model: entry.model || null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "default",
    catalog_slug: entry.slug,
    customized: false,
    // Defaults never group (grouping is a custom-row view concept, PRD #636 Decision 2).
    sibling_group_id: null,
    created_at: now,
    updated_at: now,
    next_fires: [],
    ...over,
  };
}

const catalogBySlug = (slug: string): CatalogEntry | undefined =>
  scheduleCatalog.find((e) => e.slug === slug);

// Seed a few materialized defaults so the Default-jobs UX is visible under
// VITE_UZI_MOCK=1: bug-triage enabled on TWO repos (Layout A — one summary row
// expanding to two per-repo sub-rows, one active + one paused so the resume
// affordance shows), and docs-hygiene enabled + customized on one repo (the
// "customized" indicator + a prominent Reset).
const seededDefaults: Schedule[] = [
  materializeDefault(catalogBySlug("bug-triage")!, "repo-uzi", "sch-def-bt-uzi", {
    last_fired_at: daysFromNow(-1, 2),
    last_fire: {
      fired_at: daysFromNow(-1, 2), matched: 2, capped: false,
      started: [
        { issue_iid: 96, run_id: "b1c0ffee-0000-0000-0000-000000000096", title: "Worker restart drops commits", web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/96" },
        { issue_iid: 88, run_id: "b1c0ffee-0000-0000-0000-000000000088", title: "Race in the run poller", web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/88" },
      ],
      skips: [],
    },
  }),
  // Already materialized on repo-atlas but PAUSED: re-enabling is a server no-op that
  // returns this paused row, so the UI must offer resume here, never a fresh "enable".
  materializeDefault(catalogBySlug("bug-triage")!, "repo-atlas", "sch-def-bt-atlas", {
    enabled: false,
  }),
  // A customized default (owner shifted the cadence): the "customized" indicator + a
  // prominent Reset that restores the catalog cron.
  materializeDefault(catalogBySlug("docs-hygiene")!, "repo-uzi", "sch-def-dh-uzi", {
    cron_expr: "0 6 * * 1",
    customized: true,
  }),
  // A sweep default carrying an owner GUIDANCE OVERLAY (issue #675): its baked_guidance
  // is the catalog planned-sweep text while `guidance` is a distinct, owner-set overlay,
  // so the modal must show the two independently. The two values MUST differ so a test
  // cannot pass vacuously against the old single-field shape. planned-sweep on repo-ledger
  // is used by no other seed/test, so this adds no enablement collision.
  materializeDefault(catalogBySlug("planned-sweep")!, "repo-ledger", "sch-def-ps-overlay", {
    guidance: "prefer a failing test first, then the smallest fix",
    customized: true,
  }),
];

let schedules: Schedule[] = [
  ...userSchedules.map(
    (s): Schedule => ({
      ...s,
      origin: "user",
      catalog_slug: null,
      customized: false,
      sibling_group_id: null,
      // Baked catalog guidance is default-sweep-only (issue #675); a user row never has it.
      baked_guidance: null,
    }),
  ),
  ...seededDefaults,
];

// The forge labels that already exist on each repo (PRD #589 M4, sweep-label WARN).
// repo-atlas is deliberately missing "bug" and "Planned" so enabling bug-triage /
// planned-sweep there surfaces the missing-label warning + the "Create label" confirm.
const repoLabels: Record<string, string[]> = {
  "repo-uzi": ["bug", "Planned", "PRD", "enhancement"],
  "repo-atlas": ["PRD", "enhancement"],
  "repo-payments": ["PRD"],
};

let skills: Skill[] = mockSkills.map((s) => ({ ...s }));
let allocations: Record<string, { shared: string[]; mine: string[] }> = Object.fromEntries(
  Object.entries(mockAllocations).map(([k, v]) => [k, { shared: [...v.shared], mine: [...v.mine] }]),
);
let appSettings: AppSettings = loadedSettings.appSettings;
// PRD #685: whether a logo asset exists for each slot. The demo tracks only
// presence (a bool), never bytes — mirroring how the public /api/branding read
// exposes app_logo_present/brand_logo_present. Fresh install: neither uploaded.
const brandingAssets: { app: boolean; brand: boolean } = { app: false, brand: false };
// Slack secret tokens (PRD #25) are write-only: the demo tracks only whether one
// is configured, never a value, mirroring the real API's `secrets` map. There is
// no ENV overlay in the demo, so every key's source is db/default.
const slackSecrets: Record<string, boolean> = { slack_bot_token: false, slack_app_token: false };

// The current user's Slack linking state (PRD #25 M3). The demo starts unlinked;
// setting an override moves it to "pending" (a real deployment would then DM the
// target a Confirm card), and there is no inbound socket here to confirm it.
let slackLink: Omit<SlackLink, "state" | "workspace"> = { member_id: null, notify: true, resolved_id: null, confirmed: false };

// slackLinkResponse derives the state field the real API returns, so the mock and
// the server never disagree on how member_id/resolved_id/confirmed map to a state.
// workspace mirrors the admin slack_status: the demo has no real socket, so Slack
// is always "disabled" (see settingsResponse), which maps to "unconfigured" here.
function slackLinkResponse(): { slack: SlackLink } {
  const state: SlackLink["state"] = !slackLink.resolved_id
    ? "unlinked"
    : slackLink.confirmed
      ? "confirmed"
      : "pending";
  return { slack: { ...slackLink, state, workspace: "unconfigured" } };
}

// settingsResponse builds the admin SettingsResponse from the mock's current
// mockScenario reads a demo scenario from ?mock= (or the uzi_mock_scenario
// localStorage key) so MOCK_MODE demo builds and manual QA can reach the PRD #45
// OIDC UX, which is otherwise hidden (OIDC off / password on). Unknown/absent keeps
// the original behavior. Wrapped in try/catch for any non-browser context.
function mockScenario(): string {
  try {
    const q = new URLSearchParams(window.location.search).get("mock");
    if (q) return q;
    return window.localStorage.getItem("uzi_mock_scenario") ?? "";
  } catch {
    return "";
  }
}

interface OidcDemo {
  oidcEnabled: boolean;
  providerName: string;
  passwordLoginEnabled: boolean;
  oidcStatus: string;
  passwordless: boolean; // has_password === false → the passphrase-create banner shows
}

// oidcDemo maps the scenario to the OIDC fields the auth-config, session, and
// settings responses expose. Scenarios: "oidc" (SSO alongside password),
// "oidc-degraded" (admin status degraded), "sso-only" (SSO only, password form
// hidden). Default: OIDC off, password on — the original demo behavior.
function oidcDemo(): OidcDemo {
  switch (mockScenario()) {
    case "oidc":
      return { oidcEnabled: true, providerName: "Keycloak", passwordLoginEnabled: true, oidcStatus: "ok", passwordless: true };
    case "oidc-degraded":
      return { oidcEnabled: true, providerName: "Keycloak", passwordLoginEnabled: true, oidcStatus: "degraded", passwordless: true };
    case "sso-only":
      return { oidcEnabled: true, providerName: "Keycloak", passwordLoginEnabled: false, oidcStatus: "ok", passwordless: true };
    default:
      return { oidcEnabled: false, providerName: "SSO", passwordLoginEnabled: true, oidcStatus: "disabled", passwordless: false };
  }
}

// state: readable non-secret values, per-secret configured flags, and per-key
// sources (all db/default — the demo has no ENV overlay).
function settingsResponse(): SettingsResponse {
  const sources: Record<string, SettingSource> = {};
  for (const key of Object.keys(appSettings)) sources[key] = "db";
  for (const key of Object.keys(slackSecrets)) sources[key] = slackSecrets[key] ? "db" : "default";
  // The demo has no real socket, so Slack is always "disabled" here.
  return {
    settings: { ...appSettings },
    secrets: { ...slackSecrets },
    sources,
    slack_status: "disabled",
    oidc_status: oidcDemo().oidcStatus,
    oidc_provider_name: oidcDemo().providerName,
  };
}

// boardResponse clones a board fixture for return. Cards are shallow-cloned so a caller
// mutating the response never touches the stored fixture.
function boardResponse(b: Board): { board: Board } {
  return {
    board: {
      ...b,
      cards: b.cards.map((c) => ({ ...c })),
    },
  };
}
let templateCounter = 0;
let workerCounter = 0;
let skillCounter = 0;
// Tool allowlist + per-repo profiles (PRD #18 M4).
let toolAllowlist: ToolAllowlistEntry[] = mockToolAllowlist.map((e) => ({ ...e }));
const repoToolProfiles = new Map<string, string[]>(
  Object.entries(mockRepoToolProfiles).map(([k, v]) => [k, [...v]]),
);
let toolEntryCounter = 0;

// ── CLI tokens + browser-login requests (PRD #64 M6) ─────────────────────────
// Tokens are owner-attributed (user_id) so every read/write is scoped to the
// session user, mirroring the real endpoints (`WHERE user_id=$1`). user_id is
// mock-internal — stripped before responding, since the wire CliToken has none.
type OwnedCliToken = CliToken & { user_id: string };
let cliTokens: OwnedCliToken[] = mockCliTokens.map((t) => ({ ...t }));
let cliTokenCounter = 0;
const stripOwner = ({ user_id: _user_id, ...t }: OwnedCliToken): CliToken => t;

// Agent memory (PRD #90). user_id is mock-internal (the wire Memory carries none —
// the server owner-scopes it), stripped before responding.
type OwnedMemory = Memory & { user_id: string };
let memories: OwnedMemory[] = mockMemories.map((m) => ({ ...m }));
const stripMemoryOwner = ({ user_id: _user_id, ...m }: OwnedMemory): Memory => m;
// The seeded consent request; approve/deny flip its status in place.
const cliAuthRequests = new Map<string, CliAuthRequestMeta & { user_code: string }>([
  [MOCK_CLI_AUTH_REQUEST_ID, { ...mockCliAuthRequest }],
]);

// crockford32 excludes I, L, O, U. normalizeMockUserCode folds a human-typed code
// to the canonical stored form, mirroring the server (uppercase; hyphens/spaces
// dropped; O→0, I/L→1) so a careful-but-imperfect read still matches.
const CROCKFORD32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
function normalizeMockUserCode(s: string): string {
  let out = "";
  for (const r of s.toUpperCase()) {
    if (r === "O") out += "0";
    else if (r === "I" || r === "L") out += "1";
    else if (CROCKFORD32.includes(r)) out += r;
  }
  return out;
}

// Template allocations (PRD #18 M7). Global defaults are seeded for every
// builtin/global template (no empty-means-all cliff); the per-user overlay maps
// a template id to a forced on/off decision.
const templateGlobalDefaults = new Set<string>(
  templates.filter((t) => t.scope !== "user").map((t) => t.id),
);
const templateOverrides = new Map<string, Map<string, boolean>>();

// The reserved lead names mirror the server's leadNameRe / worker LEAD_NAME_RE.
const LEAD_NAME_RE = /^(lead|orchestrator)$/i;

// visibleSkills mirrors the real read: admins see every scope, everyone else
// sees builtin ∪ global ∪ their own user skills.
function visibleSkills(me: User): Skill[] {
  return skills.filter((s) => me.is_admin || s.scope !== "user" || s.user_id === me.id);
}

// visibleTemplates mirrors the real read: builtin ∪ global ∪ own user templates
// (admins see all).
function visibleTemplates(me: User): AgentTemplate[] {
  return templates.filter((t) => me.is_admin || t.scope !== "user" || t.user_id === me.id);
}

// shippedBuiltin is the mock's BuiltinByName: the definition this "release"
// carries under a name, or undefined for a builtin a later release dropped.
function shippedBuiltin(name: string): BuiltinDefinition | undefined {
  return mockShippedBuiltins.find((d) => d.name === name);
}

// sameContent mirrors the server's agenttmpl.SameContent over the four mutable
// columns. It is a SECOND IMPLEMENTATION of that comparison and that is
// deliberate: the mock has no server to ask, and a hard-coded flag would make
// every drift the fixture claims unfalsifiable. The rules it must keep in step
// with, each of which the server states its reason for:
//
//   - name is never compared (it is the lookup key);
//   - tools is order-SENSITIVE (the order is rendered), and null and [] both mean
//     inherit-all so they compare equal;
//   - description and prompt_body are compared exactly, never trimmed.
export function sameContent(row: AgentTemplate, def: BuiltinDefinition): boolean {
  const a = row.tools ?? [];
  const b = def.tools ?? [];
  return (
    row.description === def.description &&
    (row.model ?? "") === (def.model ?? "") &&
    row.prompt_body === def.prompt_body &&
    a.length === b.length &&
    a.every((t, i) => t === b[i])
  );
}

// withDrift stamps the computed differs_from_builtin onto a row on its way out,
// so the mock answers the same question the server does rather than serving a
// stored flag. False for anything with no shipped counterpart: a non-builtin
// scope (including a user row that merely shares a builtin's NAME) and a builtin
// this release no longer ships.
function withDrift(t: AgentTemplate): AgentTemplate {
  if (t.scope !== "builtin") return { ...t, differs_from_builtin: false };
  const def = shippedBuiltin(t.name);
  if (!def) return { ...t, differs_from_builtin: false };
  return { ...t, differs_from_builtin: !sameContent(t, def) };
}

// templateAllocationView resolves each visible template's allocation state for
// me: overlay wins, else the global default.
function templateAllocationView(me: User): TemplateAllocation[] {
  const overlay = templateOverrides.get(me.id) ?? new Map<string, boolean>();
  return visibleTemplates(me).map((t) => {
    const globalDefault = templateGlobalDefaults.has(t.id);
    const myOverride = overlay.has(t.id) ? (overlay.get(t.id) as boolean) : null;
    return {
      id: t.id,
      name: t.name,
      description: t.description,
      scope: t.scope,
      is_builtin: t.is_builtin,
      global_default: globalDefault,
      my_override: myOverride,
      effective: myOverride ?? globalDefault,
    };
  });
}

function toAllocated(id: string): AllocatedSkill | null {
  const s = skills.find((x) => x.id === id);
  return s ? { skill_id: s.id, name: s.name, description: s.description, scope: s.scope } : null;
}

function allocationView(templateId: string): { shared: AllocatedSkill[]; mine: AllocatedSkill[] } {
  const a = allocations[templateId] ?? { shared: [], mine: [] };
  const map = (ids: string[]) => ids.map(toAllocated).filter((x): x is AllocatedSkill => x !== null);
  return { shared: map(a.shared), mine: map(a.mine) };
}

function listRunsFor(): Run[] {
  return [...state.runs.values()].sort((a, b) => b.created_at.localeCompare(a.created_at));
}

// notifDTO maps an internal mock notification row to the API shape, attaching the
// owner block only for the admin all-view (own-scope rows carry no owner), exactly
// like the server's two query paths.
function notifDTO(n: MockNotification, includeOwner: boolean): Notification {
  return {
    id: n.id,
    kind: n.kind,
    payload: n.payload,
    run_id: n.run_id,
    review_id: n.review_id,
    read_at: n.read_at,
    created_at: n.created_at,
    ...(includeOwner
      ? { owner: { id: n.user_id, email: n.owner_email, display_name: n.owner_display_name } }
      : {}),
  };
}

// sessionBody is the auth/session bootstrap payload: the signed-in user, the
// current instance labels (PRD #19 M2, PRD #764: the single `uzi` label and the
// autopilot label), and the three resolved theme fields (PRD #21), mirroring the real
// API so the mocked SPA resolves them the same way.
function sessionBody() {
  return {
    user: requireSession(),
    uzi_label: appSettings.uzi_label,
    autopilot_label: appSettings.autopilot_label,
    theme: resolveTheme(userSettings.theme, appSettings.default_theme),
    theme_override: userSettings.theme,
    default_theme: appSettings.default_theme,
    // A passwordless (OIDC) demo user has no vault yet, so the SPA shows the
    // passphrase-create banner; a password demo user keeps the existing behavior.
    vault: oidcDemo().passwordless
      ? { unlocked: false, exists: false }
      : { unlocked: state.vaultUnlocked, exists: true },
    has_password: !oidcDemo().passwordless,
    // Judge consent (PRD #69 M4), resolved exactly like the server: enforced only when
    // BOTH the kill-switch and enforce-all are on (Gate-2-wins), and the effective model
    // is user-value-wins over the instance value, which falls back to opus.
    judge_enforced_by_admin: appSettings.judge_enabled === "true" && appSettings.judge_enforce_all === "true",
    effective_judge_model:
      (userSettings.judge_model ?? "").trim() !== ""
        ? (userSettings.judge_model as string)
        : appSettings.judge_model.trim() !== ""
          ? appSettings.judge_model
          : "opus",
  };
}

// rejectInvisibleLabel mirrors the server's validateSecretLabel Cf rule (PRD #111).
//
// The mock is this repo's BROWSABLE SPEC, and until this existed it accepted labels
// production rejects — which is how a browser pass managed to store a bidi-override
// label and demonstrate F12 against a build that was supposed to make it impossible.
// A mock that disagrees with the API about what is valid teaches the wrong lesson and
// leaves the new error copy with nowhere to be seen.
//
// Control characters are not re-checked here: the real validator rejects them too,
// but they cannot be typed into the field, so the Cf half is the one a demo exercises.
function rejectInvisibleLabel(label: string): void {
  if (/\p{Cf}/u.test(label)) {
    throw new ApiError(
      400,
      "Label must not contain invisible formatting characters (zero-width spaces and joiners, bidirectional overrides, the byte-order mark): they let two different tokens look identical, or make a label read as a different account. This also rules out multi-part emoji such as 👨‍👩‍👧, which are joined by one of these characters, so use a plain name instead",
    );
  }
}

// pooledFixtureStatus is each demo token's eligibility WHEN POOLED, so toggling one
// off and on again returns it to the state its fixture describes instead of
// flattening every token to `eligible`.
const pooledFixtureStatus: Record<string, AutoStatus> = {
  "sec-never-polled": "no_reading",
  "sec-low": "below_threshold",
};

export const mockApi = {
  // ── Auth: instant and fake. Any credentials sign in as the admin. ──────────
  // The session bootstrap carries the instance labels alongside the user, mirroring
  // the real API (PRD #19 M2), so the mocked SPA resolves them the same way.
  register: async (email: string, _password: string, displayName: string) => {
    state.session = { ...mockAdmin, email, display_name: displayName || mockAdmin.display_name };
    return delay(sessionBody());
  },
  login: async (email: string, _password: string) => {
    // Persona switch for the demo: logging in as a seeded non-admin (e.g.
    // mira@uzi.local) signs in AS that user, so the non-admin rendering paths
    // (no Global create, view-only builtin/global, own-skills-only) are
    // browser-checkable. Any other email is the admin, as before.
    const persona = users.find((u) => u.email === email.trim().toLowerCase());
    state.session = persona ? { ...persona } : { ...mockAdmin, email: email || mockAdmin.email };
    return delay(sessionBody());
  },
  // Demo mode has registration open and unrestricted. The OIDC fields follow the
  // scenario toggle (default off; ?mock=oidc / sso-only enable SSO) — PRD #45 N6.
  authConfig: async () => {
    const d = oidcDemo();
    return delay({
      registration_enabled: true,
      allowed_email_domains: [],
      oidc_enabled: d.oidcEnabled,
      oidc_provider_name: d.providerName,
      password_login_enabled: d.passwordLoginEnabled,
    });
  },
  // The in-browser demo build has no server; report "demo" to match the header pill.
  // A real SemVer, not "demo" (PRD #113 M5). Upgrade classification compares against
  // this, and a non-SemVer control-plane version turns classification OFF entirely — so
  // the literal "demo" made every badge and the whole Fleet panel unreachable in demo
  // mode. The demo-mode signal does not live here: AppShell renders a separate "demo"
  // pill, so nothing is lost by making this comparable.
  // PRD #113 M6. Computed LIVE from the demo worker list rather than hardcoded, so the
  // badge actually clears when a worker is deleted — web-ux needs to see it appear AND
  // clear, and a constant would only ever show the first half.
  workerUpgradeSummary: async () => {
    const attention = workers.filter(
      (w) => w.upgrade_status === "upgrade_failed" || w.upgrade_status === "outdated",
    ).length;
    return delay({ attention, target_release: "0.4.2" });
  },

  // The FULLY-STAMPED fixture (PRD #175), so the demo shows the popover with every
  // row present — a `dev` build here would hide the three fields this PRD exists to
  // add, in the build whose whole job is to show them off.
  //
  // KNOWN CONSEQUENCE, worth stating rather than leaving to be rediscovered: this
  // is the default for every VITE_UZI_MOCK=1 run, so a browser pass sees the
  // STAMPED shape unless someone swaps this line. The degraded shapes are covered
  // in BuildInfoPopover.test.tsx (mockBuildInfoUnstamped = the laptop's three-key
  // body, mockBuildInfoNoUptime = the struct-literal Handler's two-key one); to see
  // either in a browser, point this line at it. `typeof realApi` cannot enforce any
  // of it, since every field but version and founded is optional.
  version: async () => delay(mockBuildInfo),
  // PRD #685: public branding read. Coerces the string-space settings to the typed
  // (bool) chrome shape and derives the two *_present flags from the tracked assets.
  branding: async () =>
    delay({
      app_logo_mode: appSettings.app_logo_mode,
      app_logo_preset: appSettings.app_logo_preset,
      app_logo_present: brandingAssets.app,
      app_logo_keep_name: appSettings.app_logo_keep_name === "true",
      brand_mode: appSettings.brand_mode,
      brand_company: appSettings.brand_company,
      brand_placement: appSettings.brand_placement,
      brand_plaque: appSettings.brand_plaque === "true",
      brand_logo_present: brandingAssets.brand,
    }),
  // Admin logo upload/delete. The demo enforces the same type allowlist and 256 KiB
  // cap the server does, then records presence (never bytes).
  uploadBrandingLogo: async (slot: string, file: File) => {
    requireSession();
    if (slot !== "app" && slot !== "brand") {
      throw new ApiError(404, "unknown logo slot");
    }
    const allowed = ["image/png", "image/webp", "image/svg+xml"];
    if (!allowed.includes(file.type)) {
      throw new ApiError(400, "logo must be a PNG, WebP or SVG image");
    }
    if (file.size > 262144) {
      throw new ApiError(400, "logo must be at most 262144 bytes");
    }
    brandingAssets[slot] = true;
    await delay(undefined, 40);
  },
  deleteBrandingLogo: async (slot: string) => {
    requireSession();
    if (slot !== "app" && slot !== "brand") {
      throw new ApiError(404, "unknown logo slot");
    }
    brandingAssets[slot] = false;
    return delay({ status: "ok" });
  },
  logout: async () => {
    state.session = null;
    return delay({ status: "ok" });
  },
  me: async () => {
    if (!state.session) throw new ApiError(401, "authentication required");
    return delay(sessionBody(), 40);
  },

  // ── Admin: users ────────────────────────────────────────────────────────────
  listUsers: async () => delay({ users: users.map((u) => ({ ...u })) }),
  setUserActive: async (id: string, isActive: boolean) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    u.is_active = isActive;
    return delay({ user: { ...u } });
  },

  // ── Admin: instance settings (PRD #19) ───────────────────────────────────────
  // Mirrors the server's Decision 8 validation so the demo surfaces the same
  // rejection messages the real API would.
  getSettings: async () => delay(settingsResponse()),
  // Demo is fully DEK-sealed (no legacy rows), so the admin migration notice is
  // hidden; the wiring is still exercised by the AdminSettings unit test.
  vaultMigration: async () => delay({ master_sealed: 0 }),

  // ── Agent source (PRD #602 M5) ───────────────────────────────────────────────
  getAgentSource: async () => {
    requireSession();
    // Derive update_available/latest_ref at read time from the stored remote facts +
    // the live config (PRD #702 M4), so a bump/apply self-clears the badge with no egress.
    return delay({ agent_source: agentSourceView() });
  },
  // "Sync now": re-runs the reconcile. In the mock it refreshes the sync timestamp
  // and marks the fetch healthy, leaving the staged snapshot pending for review. A
  // sync against no configured URL records an error rather than staging (mirrors the
  // server, which never fetches an empty URL).
  syncAgentSource: async () => {
    requireSession();
    const now = new Date().toISOString();
    if (agentSource.config.url.trim() === "") {
      agentSource.status = {
        ...agentSource.status,
        last_sync_at: now,
        last_sync_status: "error",
        last_sync_error: "No repository URL is configured.",
      };
      agentSource.staged = null;
    } else {
      agentSource.status = {
        ...agentSource.status,
        last_sync_at: now,
        last_sync_status: "ok",
        last_sync_error: undefined,
      };
      if (agentSource.staged) agentSource.staged.fetched_at = now;
    }
    return delay({ agent_source: agentSourceView() });
  },
  // Approve-and-apply. expected_sha binds the reviewed snapshot: a mismatch (or
  // nothing pending) is the 409 the real handler returns for ErrStaleApproval, so the
  // admin re-reviews rather than applying blind.
  applyAgentSource: async (expectedSha: string) => {
    requireSession();
    const staged = agentSource.staged;
    if (!staged || !staged.pending || staged.fetched_sha !== expectedSha) {
      throw new ApiError(409, "the staged snapshot changed since you reviewed it; re-review before applying");
    }
    // Tally the outcome from the diff, mirroring agentsource.ApplyResult.
    let applied = 0;
    let unchanged = 0;
    let conflicts = 0;
    let deprovisioned = 0;
    for (const d of staged.diff) {
      if (d.action === "add" || d.action === "override") applied++;
      else if (d.action === "unchanged") unchanged++;
      else if (d.action === "conflict") conflicts++;
      else if (d.action === "remove") deprovisioned++;
    }
    const skippedParse = staged.roles.filter((r) => !r.ok).length;
    const now = new Date().toISOString();
    agentSource.status = {
      ...agentSource.status,
      last_applied_at: now,
      last_applied_sha: staged.fetched_sha,
    };
    staged.pending = false;
    const result: AgentSourceApplyResult = {
      sha: staged.fetched_sha,
      applied,
      unchanged,
      conflicts,
      deprovisioned,
      skipped_parse: skippedParse,
      already_applied: false,
      message: "applied",
    };
    return delay({ result });
  },

  // Resolve the latest semver tag for a supplied (possibly unsaved) URL (PRD #702 M3).
  // Mirrors the real endpoint: an empty URL is a 400, a host off the SSRF allowlist is
  // the same 400 the ls-remote path refuses with (the resolve is anonymous, so it only
  // works against a public, allowlisted source). On success it returns a plausible
  // semver tag — v1.10.0 for the canonical skills repo, which sorts ABOVE a lexical
  // v1.2.0 (the discriminating fixture). Any other allowlisted URL returns the same.
  resolveAgentSourceLatest: async (url: string) => {
    requireSession();
    const trimmed = url.trim();
    if (trimmed === "") throw new ApiError(400, "url is required");
    let host = "";
    try {
      const parsed = new URL(trimmed);
      if (parsed.protocol !== "https:") throw new Error("scheme");
      host = parsed.hostname;
    } catch {
      throw new ApiError(400, "URL is not in the agent-source allowlist");
    }
    if (!mockAllowedAgentSourceHosts.includes(host)) {
      throw new ApiError(400, "URL is not in the agent-source allowlist");
    }
    return delay({ latest_ref: "v1.10.0" });
  },

  // Update check (PRD #702 M4): ls-remotes the CONFIGURED source with its sealed
  // credential and PERSISTS the remote facts (latest semver tag + tip sha + checked-at),
  // mirroring the server's persist-facts split. getAgentSource then DERIVES
  // update_available/latest_ref from these + the live config, so the badge self-clears
  // after a bump. The mock simulates a source ahead of the default v1.2.0 pin: a latest
  // tag of v1.10.0 (which sorts above v1.2.0 — the discriminating fixture) and a moved tip.
  updateCheckAgentSource: async () => {
    requireSession();
    const now = new Date().toISOString();
    if (agentSource.config.url.trim() === "") {
      // No configured source to check — record the failure in the last_sync_error slot
      // the update check reuses (per the server), so the card surfaces it.
      agentSource.status = {
        ...agentSource.status,
        last_sync_status: "error",
        last_sync_error: "No repository URL is configured.",
      };
      return delay({ agent_source: agentSourceView() });
    }
    // Persist the remote facts; the derived fields are computed at read time.
    agentSourceRemote = {
      latestRef: "v1.10.0",
      tipSha: "a1b2c3d4e5f60718293a",
      checkedAt: now,
    };
    agentSource.status = {
      ...agentSource.status,
      last_sync_status: agentSource.status.last_sync_status ?? "ok",
      last_sync_error: undefined,
    };
    return delay({ agent_source: agentSourceView() });
  },

  updateSettings: async (updates: UpdateSettingsPayload) => {
    // Secret tokens are write-only: validated + recorded as configured, never
    // merged into the readable settings (mirrors the real structural exclusion).
    const nonSecret: Partial<AppSettings> = {};
    for (const [key, raw] of Object.entries(updates)) {
      const value = raw ?? "";
      if (key === "slack_bot_token" || key === "slack_app_token") {
        const prefix = key === "slack_bot_token" ? "xoxb-" : "xapp-";
        if (!value.startsWith(prefix)) {
          throw new ApiError(400, `${key}: token must start with ${prefix}`);
        }
        slackSecrets[key] = true;
        continue;
      }
      // Agent-source config (PRD #602 M5) routes to the dedicated agent-source state,
      // not the readable settings — the card reads it back through getAgentSource.
      if (key === "agent_source_credential") {
        // Write-only secret: an empty value leaves it unchanged; any value marks it
        // configured. Never merged into the readable settings.
        if (value.trim() !== "") agentSource.config.credential_configured = true;
        continue;
      }
      if (key === "agent_source_repo_url") {
        const url = value.trim();
        if (url !== "") {
          // Mirror the server's write-time SSRF-allowlist gate (and its https-only rule).
          let host = "";
          try {
            const parsed = new URL(url);
            if (parsed.protocol !== "https:") throw new Error("scheme");
            host = parsed.hostname;
          } catch {
            throw new ApiError(400, "agent_source_repo_url: must be an https URL");
          }
          if (!mockAllowedAgentSourceHosts.includes(host)) {
            throw new ApiError(400, "URL is not in the agent-source allowlist");
          }
        }
        agentSource.config.url = url;
        if (url === "") {
          // Deconfiguring the source clears its sync history and any staged snapshot
          // — there is nothing left to sync from or review, so the card returns to
          // its clean "never synced / nothing staged" fresh-install shape.
          agentSource.config.enabled = false;
          agentSource.status = {};
          agentSource.staged = null;
        } else if (agentSource.staged) {
          // Keep the staged snapshot's source_url coherent with the configured URL.
          agentSource.staged.source_url = url;
        }
        continue;
      }
      if (key === "agent_source_ref") {
        agentSource.config.ref = value.trim();
        if (agentSource.staged) agentSource.staged.source_ref = value.trim();
        continue;
      }
      if (key === "agent_source_folder") {
        // PRD #702 M1: repo-relative subfolder. Empty resolves to the default
        // (".claude/agents") server-side; a configured value normalizes away a
        // single trailing slash, mirroring the Cache reader.
        const folder = value.trim();
        agentSource.config.folder = folder === "" ? ".claude/agents" : folder.replace(/\/$/, "");
        continue;
      }
      if (key === "agent_source_enabled") {
        if (value !== "true" && value !== "false") {
          throw new ApiError(400, "agent_source_enabled: must be \"true\" or \"false\"");
        }
        agentSource.config.enabled = value === "true";
        continue;
      }
      if (key === "agent_source_interval") {
        const v = value.trim();
        if (v !== "" && !/^\d+(ns|us|µs|ms|s|m|h)$/.test(v)) {
          throw new ApiError(400, "agent_source_interval: must be a Go duration (e.g. 1h)");
        }
        agentSource.config.interval = v || "1h";
        continue;
      }
      // default_theme routes to the theme registry, not the label rules (PRD #21).
      if (key === "default_theme") {
        if (!isTheme(value)) throw new ApiError(400, `default_theme: unknown theme: "${value}"`);
        nonSecret.default_theme = value;
        continue;
      }
      // slack_enabled / judge_enabled / … are strict bools, not labels — without this
      // arm they would fall through to the label rules and fail open on "yes"/"maybe".
      if (
        key === "slack_enabled" ||
        key === "judge_enabled" ||
        key === "judge_enforce_all" ||
        key === "ephemeral_workers_enabled"
      ) {
        if (value !== "true" && value !== "false") {
          throw new ApiError(400, `${key}: must be "true" or "false"`);
        }
        (nonSecret as Record<string, string>)[key] = value;
        continue;
      }
      // judge_cooldown_seconds: 0 (off) or 60..86400 seconds; judge_daily_budget: 0
      // (unlimited) or 1..10000 runs (PRD #69 M5, mirroring the server validators).
      if (key === "judge_cooldown_seconds") {
        if (!/^\d+$/.test(value)) throw new ApiError(400, "judge_cooldown_seconds: must be a whole number");
        const n = Number(value);
        if (n !== 0 && (n < 60 || n > 86400)) {
          throw new ApiError(400, "judge_cooldown_seconds: must be 0 (off) or between 60 and 86400");
        }
        nonSecret.judge_cooldown_seconds = String(n);
        continue;
      }
      if (key === "judge_daily_budget") {
        if (!/^\d+$/.test(value)) throw new ApiError(400, "judge_daily_budget: must be a whole number");
        const n = Number(value);
        if (n < 0 || n > 10000) {
          throw new ApiError(400, "judge_daily_budget: must be 0 (unlimited) or between 1 and 10000 judge runs");
        }
        nonSecret.judge_daily_budget = String(n);
        continue;
      }
      // judge_model (PRD #46) and summary_model (PRD #362) are model aliases: non-empty
      // single token, mirroring the server's PRD #17 ValidateModel rules.
      if (key === "judge_model" || key === "summary_model") {
        if (value.trim() === "") throw new ApiError(400, `${key}: must not be empty`);
        if (/\s/.test(value)) throw new ApiError(400, `${key}: must be a single token with no spaces`);
        (nonSecret as Record<string, string>)[key] = value;
        continue;
      }
      // public_base_url must be http(s) (PRD #25).
      if (key === "public_base_url") {
        if (!/^https?:\/\/.+/.test(value)) {
          throw new ApiError(400, "public_base_url: must use http or https");
        }
        nonSecret.public_base_url = value;
        continue;
      }
      // PRD #685 branding config. Enum keys are checked against their allowed sets;
      // the two flags are strict bools; brand_company is free text (may be "", may
      // contain commas — mirrors the server's dedicated termsafe validator), capped
      // at 64 runes.
      if (key === "app_logo_mode") {
        if (value !== "default" && value !== "custom" && value !== "preset") {
          throw new ApiError(400, 'app_logo_mode: must be "default", "custom" or "preset"');
        }
        nonSecret.app_logo_mode = value;
        continue;
      }
      // app_logo_preset (PRD #780): a web-catalog slug. Mirrors the server's
      // validateBrandingSlug SHAPE gate — empty is allowed ("no preset"), any other
      // value must be a short lowercase slug. Membership against the catalog is NOT
      // checked here (the web catalog is the source of truth; unknown slugs degrade).
      if (key === "app_logo_preset") {
        if (value !== "" && !/^[a-z][a-z0-9-]{0,31}$/.test(value)) {
          throw new ApiError(400, "app_logo_preset: must be a short lowercase slug (a-z, 0-9, hyphen; 32 chars max)");
        }
        nonSecret.app_logo_preset = value;
        continue;
      }
      if (key === "brand_mode") {
        if (value !== "none" && value !== "text" && value !== "logo") {
          throw new ApiError(400, 'brand_mode: must be "none", "text" or "logo"');
        }
        nonSecret.brand_mode = value;
        continue;
      }
      if (key === "brand_placement") {
        if (value !== "below" && value !== "topright") {
          throw new ApiError(400, 'brand_placement: must be "below" or "topright"');
        }
        nonSecret.brand_placement = value;
        continue;
      }
      if (key === "app_logo_keep_name" || key === "brand_plaque") {
        if (value !== "true" && value !== "false") {
          throw new ApiError(400, `${key}: must be "true" or "false"`);
        }
        (nonSecret as Record<string, string>)[key] = value;
        continue;
      }
      if (key === "brand_company") {
        if ([...value].length > 64) {
          throw new ApiError(400, "brand_company: must be at most 64 characters");
        }
        nonSecret.brand_company = value;
        continue;
      }
      if (key !== "autopilot_label" && key !== "uzi_label") {
        throw new ApiError(400, `unknown setting: ${key}`);
      }
      if (!value || value.trim() === "") throw new ApiError(400, `${key}: must not be empty`);
      if (value.length > 64) throw new ApiError(400, `${key}: must be at most 64 characters`);
      if (value.includes(",")) throw new ApiError(400, `${key}: must not contain a comma`);
      (nonSecret as Record<string, string>)[key] = value;
    }
    const merged = { ...appSettings, ...nonSecret };
    // The `uzi` and autopilot labels must differ (PRD #764), mirroring the server's
    // ValidateMerged.
    if (merged.uzi_label === merged.autopilot_label) {
      throw new ApiError(400, "uzi_label and autopilot_label must differ");
    }
    appSettings = merged;
    persistSettings();
    return delay(settingsResponse());
  },

  // ── Autopilot opt-in (PRD #19 M3) ────────────────────────────────────────────
  setAutopilotEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.autopilot_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },

  // ── Usage-limit default (PRD #35 M3) ─────────────────────────────────────────
  // 🔴 TOUCHES THE USER ROW ONLY. It must not walk `state.runs` "helpfully" applying
  // the new default: the flag is copied onto a run at CREATION, so a sweep would
  // silently undo every per-run override the user had made — including on the run
  // they are looking at. The demo has to teach that these are two separate controls,
  // because that is the thing about this feature people get wrong.
  setWaitOnLimit: async (enabled: boolean) => {
    const u = requireSession();
    u.wait_on_limit = enabled;
    return delay({ user: { ...u } }, 200);
  },

  // ── Run-judge opt-in (PRD #46) ───────────────────────────────────────────────
  // Own-user (session identity, never a body id, mirroring the server's audit H3).
  setJudgeEnabled: async (enabled: boolean, anthropicToken?: string | null) => {
    const u = requireSession();
    u.judge_enabled = enabled;
    // Three-way, like the server: undefined leaves the binding, null clears it, a
    // label binds it (400 when it names nothing).
    if (anthropicToken !== undefined) {
      if (anthropicToken === null || anthropicToken.trim() === "") {
        u.judge_anthropic_secret_id = null;
        u.judge_anthropic_secret_label = null;
      } else {
        const secret = secrets.find(
          (x) =>
            x.kind === "anthropic_token" &&
            x.label.toLowerCase() === anthropicToken.trim().toLowerCase(),
        );
        if (!secret) throw new ApiError(400, "no Anthropic token with that label");
        u.judge_anthropic_secret_id = secret.id;
        u.judge_anthropic_secret_label = secret.label;
      }
    }
    return delay({ user: { ...u } }, 200);
  },
  // Admin per-user toggle: target from the id argument (the path on the server).
  setUserJudgeEnabled: async (id: string, enabled: boolean) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    u.judge_enabled = enabled;
    return delay({ user: { ...u } });
  },

  // ── CI-autofix opt-in (PRD #71) ──────────────────────────────────────────────
  // Own-user (session identity, never a body id, mirroring the server).
  setCIAutofixEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.ci_autofix_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },
  // Admin per-user toggle: target from the id argument (the path on the server).
  setUserCIAutofixEnabled: async (id: string, enabled: boolean) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    u.ci_autofix_enabled = enabled;
    return delay({ user: { ...u } });
  },

  // ── Ephemeral-workers opt-in (PRD #649) ──────────────────────────────────────
  // Own-user (session identity, never a body id, mirroring the server).
  setEphemeralWorkersEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.ephemeral_workers_enabled = enabled;
    // Persist on the backing users[] record too, not just the session copy, so the
    // opt-in survives a mock logout/login (login() re-copies from users[]).
    const stored = users.find((x) => x.id === u.id);
    if (stored) stored.ephemeral_workers_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },

  // ── Notifications inbox (PRD #46 M2) ─────────────────────────────────────────
  // Own view filters to the session user; { all: true } shows everyone but only
  // for an admin (else 403, like the server). `unread` is always the caller's own
  // count. Rows come back newest-first, paginated.
  listNotifications: async (params?: { all?: boolean; limit?: number; offset?: number }): Promise<NotificationList> => {
    const me = requireSession();
    const all = params?.all ?? false;
    if (all && !me.is_admin) throw new ApiError(403, "admin only");
    const limit = Math.min(Math.max(params?.limit ?? 30, 1), 100);
    const offset = Math.max(params?.offset ?? 0, 0);
    const scope = all ? notifications : notifications.filter((n) => n.user_id === me.id);
    const sorted = [...scope].sort((a, b) => b.created_at.localeCompare(a.created_at));
    const page = sorted.slice(offset, offset + limit).map((n) => notifDTO(n, all));
    const unread = notifications.filter((n) => n.user_id === me.id && !n.read_at).length;
    return delay({ notifications: page, unread, total: scope.length });
  },
  unreadNotificationCount: async () => {
    const me = requireSession();
    return delay({ unread: notifications.filter((n) => n.user_id === me.id && !n.read_at).length }, 40);
  },
  // Runs-in-progress count for the Runs nav badge (PRD #239). Counted LIVE from the
  // fixtures the same way the real endpoint counts rows: non-terminal runs, excluding
  // chat/judge kinds (Decision 1 + Decision 4), so the demo build shows a real number
  // that moves as runs start and finish rather than a hardcoded constant.
  runsInProgressCount: async () => {
    // Other users' runs are excluded exactly as the real /me/runs/in-progress-count
    // is caller-scoped — the badge counts YOUR queue, not the factory's.
    const count = [...state.runs.values()].filter(
      (r) =>
        !isTerminalRun(r.status) &&
        r.kind !== "chat" &&
        r.kind !== "judge" &&
        !(r.id in mockOtherRunOwners),
    ).length;
    return delay({ count }, 40);
  },
  markNotificationRead: async (id: string) => {
    const me = requireSession();
    // Ownership is the (id, user_id) match — a foreign or unknown id is a 404,
    // exactly like the server's query.
    const n = notifications.find((x) => x.id === id && x.user_id === me.id);
    if (!n) throw new ApiError(404, "notification not found");
    if (!n.read_at) n.read_at = new Date().toISOString();
    return delay({ notification: notifDTO(n, false) });
  },

  // ── Secrets ─────────────────────────────────────────────────────────────────
  listSecrets: async () =>
    delay({
      // Default first, then by label — the order the server's query returns.
      secrets: [...secrets]
        .sort((a, b) =>
          a.is_default === b.is_default ? a.label.localeCompare(b.label) : a.is_default ? -1 : 1,
        )
        .map((s) => ({ ...s })),
    }),
  putAnthropicToken: async (_token: string) => {
    // Mirror the real API: a locked vault cannot seal a new token (PRD #32).
    requireUnlockedVault();
    const now = new Date().toISOString();
    // The D14 alias rotates the DEFAULT, or creates the first one labelled
    // "default" — exactly what UpsertDefaultUserSecret does server-side.
    const existing = secrets.find((s) => s.kind === "anthropic_token" && s.is_default);
    if (existing) {
      existing.updated_at = now;
      return delay({ secret: { ...existing } });
    }
    const created: SecretMeta = {
      id: `sec-${Math.random().toString(36).slice(2, 8)}`,
      kind: "anthropic_token",
      label: "default",
      is_default: true,
      // A new token is never pooled (PRD #111 D2) — mirror the server default or
      // the mock teaches the wrong lesson.
      auto_eligible: false,
      created_at: now,
      updated_at: now,
    };
    secrets.push(created);
    return delay({ secret: { ...created } });
  },

  // ── Token CRUD (PRD #104 M2) ───────────────────────────────────────────────
  createAnthropicToken: async (_token: string, label: string, isDefault: boolean) => {
    requireUnlockedVault();
    const trimmed = label.trim();
    if (trimmed === "") throw new ApiError(400, "label must not be empty");
    rejectInvisibleLabel(trimmed);
    const anthropic = () => secrets.filter((s) => s.kind === "anthropic_token");
    if (anthropic().some((s) => s.label.toLowerCase() === trimmed.toLowerCase())) {
      throw new ApiError(409, "a token with that label already exists");
    }
    // The server FORCES a user's first token to be the default whatever the body
    // asks (the invisible-token hazard); mirror that here or the mock teaches the
    // wrong lesson.
    const first = anthropic().length === 0;
    const wantDefault = isDefault || first;
    if (wantDefault) anthropic().forEach((s) => (s.is_default = false));
    const now = new Date().toISOString();
    const created: SecretMeta = {
      id: `sec-${Math.random().toString(36).slice(2, 8)}`,
      kind: "anthropic_token",
      label: trimmed,
      is_default: wantDefault,
      auto_eligible: false,
      created_at: now,
      updated_at: now,
    };
    secrets.push(created);
    return delay({ secret: { ...created } });
  },
  patchAnthropicToken: async (
    id: string,
    body: { label?: string; default?: boolean; token?: string },
  ) => {
    const row = secrets.find((s) => s.id === id);
    if (!row) throw new ApiError(404, "token not found");
    if (body.token !== undefined) requireUnlockedVault();
    if (body.default === false) {
      throw new ApiError(400, "cannot clear the default; set another token as default instead");
    }
    if (body.label !== undefined) {
      const trimmed = body.label.trim();
      if (trimmed === "") throw new ApiError(400, "label must not be empty");
      rejectInvisibleLabel(trimmed);
      if (
        secrets.some(
          (s) => s.id !== id && s.kind === row.kind && s.label.toLowerCase() === trimmed.toLowerCase(),
        )
      ) {
        throw new ApiError(409, "a token with that label already exists");
      }
      row.label = trimmed;
    }
    if (body.default === true) {
      secrets.filter((s) => s.kind === row.kind).forEach((s) => (s.is_default = false));
      row.is_default = true;
    }
    row.updated_at = new Date().toISOString();
    return delay({ secret: { ...row } });
  },
  // The auto-selection pool toggle (PRD #111 M2). It also re-derives the token's
  // live eligibility, because in the mock that is the only way the chip beside the
  // toggle can move — and a toggle whose visible consequence never changes is the
  // silent no-op the real feature exists to make visible.
  setTokenAutoEligible: async (id: string, autoEligible: boolean) => {
    const row = secrets.find((s) => s.id === id);
    if (!row) throw new ApiError(404, "token not found");
    row.auto_eligible = autoEligible;
    row.updated_at = new Date().toISOString();
    const meter = mockMyTokenRateLimits.find((t) => t.secret_id === id);
    if (meter) {
      meter.auto_eligible = autoEligible;
      // Opting OUT is always `not_pooled` — that gate comes first server-side too.
      // Opting IN restores the token's OWN fixture state rather than hard-coding
      // `eligible` (web-ux F2): the four states the feature exists for — never
      // polled, stale, no usage data, low headroom — were unreachable in the demo
      // because this line asserted every pooled token is pickable, which is the very
      // thing the chip exists to disprove.
      //
      // This does NOT re-implement the gate. The real status is autoselect.Classify's
      // answer, computed server-side; this restores a fixture value, which is why it
      // lives here and not in lib/rateLimits.ts.
      meter.auto_status = autoEligible ? (pooledFixtureStatus[id] ?? "eligible") : "not_pooled";
    }
    return delay({ secret: { ...row } });
  },
  deleteAnthropicTokenById: async (id: string) => {
    const row = secrets.find((s) => s.id === id);
    if (!row) throw new ApiError(404, "token not found");
    const siblings = secrets.filter((s) => s.kind === row.kind);
    // D6: the default may not be deleted while others exist — promote first.
    if (row.is_default && siblings.length > 1) {
      throw new ApiError(
        409,
        "cannot delete the default token while other tokens exist; set another token as default first",
      );
    }
    secrets = secrets.filter((s) => s.id !== id);
    // The real schema CASCADES: migrations 00078/00079 hang composite FKs off
    // user_secrets (user_id, id) with ON DELETE SET NULL, so deleting a bound token
    // unbinds its workers and the judge rather than orphaning them. Without this the
    // mock left workers reading "spends console-key" forever — and with one token
    // left the picker is hidden, so there was no way to correct it. Two reasons that
    // matters beyond tidiness: the shipped Dockerfile.mock demo was showing D5's own
    // promise being broken, and D5's cascade otherwise has schema-level evidence
    // only. Mirrored here so a browser can prove the behaviour end to end.
    workers.forEach((w) => {
      if (w.anthropic_secret_id === id) {
        w.anthropic_secret_id = null;
        w.anthropic_secret_label = null;
      }
    });
    // `state.session` is a COPY, not a reference into `users`, so both have to be
    // swept or the cascade would be invisible to /me — which is the read every
    // judge surface actually uses.
    [...users, state.session].forEach((u) => {
      if (u && u.judge_anthropic_secret_id === id) {
        u.judge_anthropic_secret_id = null;
        u.judge_anthropic_secret_label = null;
      }
    });
    return delay(null);
  },

  // ── Vault (PRD #32) ───────────────────────────────────────────────────────────
  // Any non-empty password unlocks in the demo (there is no real crypto); an empty
  // password is treated as the "wrong password" 403 so the banner's error path is
  // browsable.
  vaultUnlock: async (password: string) => {
    if (password.trim() === "") throw new ApiError(403, "incorrect password");
    state.vaultUnlocked = true;
    return delay(null, 150);
  },
  // Passphrase-create (PRD #45): min length 12, then the demo vault is unlocked.
  vaultCreatePassphrase: async (passphrase: string) => {
    if (passphrase.length < 12) throw new ApiError(400, "passphrase must be at least 12 characters");
    state.vaultUnlocked = true;
    return delay(null, 150);
  },
  vaultLock: async () => {
    state.vaultUnlocked = false;
    return delay(null, 100);
  },
  vaultStatus: async () => delay({ unlocked: state.vaultUnlocked }, 40),
  deleteAnthropicToken: async () => {
    // D14: the kind-path alias 409s for a multi-token user — they delete by id.
    const anthropic = secrets.filter((s) => s.kind === "anthropic_token");
    if (anthropic.length > 1) {
      throw new ApiError(
        409,
        "you have multiple tokens; delete a specific one by id (DELETE /api/me/secrets/anthropic_token/{id})",
      );
    }
    secrets = secrets.filter((s) => s.kind !== "anthropic_token");
    return delay(null);
  },
  getMySettings: async () => delay({ settings: { ...userSettings } }),
  putMySettings: async (patch: UserSettingsPatch) => {
    // PATCH-like: apply only the fields present in the body, mirroring the real
    // handler so a theme-only save never clears the model and vice versa.
    if (patch.default_model !== undefined) {
      const trimmed = patch.default_model?.trim() ?? "";
      userSettings = { ...userSettings, default_model: trimmed === "" ? null : trimmed };
    }
    if (patch.default_effort !== undefined) {
      // Closed enum (PRD #617 M5): blank clears to inherit; any other value must be one
      // of the five SDK levels, mirroring the server's ValidateEffort.
      const trimmed = patch.default_effort?.trim() ?? "";
      if (trimmed !== "" && !["low", "medium", "high", "xhigh", "max"].includes(trimmed)) {
        throw new ApiError(400, "effort must be one of low, medium, high, xhigh, max");
      }
      userSettings = { ...userSettings, default_effort: trimmed === "" ? null : trimmed };
    }
    if (patch.judge_model !== undefined) {
      // Same rules as default_model (PRD #69 M2): blank clears to inherit, a value with
      // internal whitespace is rejected, mirroring the server's ValidateModel.
      const trimmed = patch.judge_model?.trim() ?? "";
      if (trimmed !== "" && /\s/.test(trimmed)) {
        throw new ApiError(400, "judge_model must be a single token with no spaces");
      }
      userSettings = { ...userSettings, judge_model: trimmed === "" ? null : trimmed };
    }
    if (patch.summary_model !== undefined) {
      // Same rules as judge_model (PRD #362 M2): blank clears to inherit, a value with
      // internal whitespace is rejected, mirroring the server's ValidateModel.
      const trimmed = patch.summary_model?.trim() ?? "";
      if (trimmed !== "" && /\s/.test(trimmed)) {
        throw new ApiError(400, "summary_model must be a single token with no spaces");
      }
      userSettings = { ...userSettings, summary_model: trimmed === "" ? null : trimmed };
    }
    if (patch.theme !== undefined) {
      const t = patch.theme?.trim() ?? "";
      if (t !== "" && !isTheme(t)) throw new ApiError(400, `unknown theme: "${t}"`);
      userSettings = { ...userSettings, theme: t === "" ? null : t };
    }
    if (patch.sidebar_token_ids !== undefined) {
      // Whole-set replace, mirroring how the real handler would treat a list
      // value; null clears back to default-only. Ids are stored as given — a
      // stale id (deleted token) is harmless, it just matches nothing.
      userSettings = { ...userSettings, sidebar_token_ids: patch.sidebar_token_ids ?? [] };
    }
    if (patch.mr_rework_enabled !== undefined) {
      // Tri-state (PRD #700 M6): present-false opts out, present-true re-enables,
      // present-null clears back to the default-ON state (stored as null). Mirrors
      // the server treating an absent/null value as ON.
      userSettings = { ...userSettings, mr_rework_enabled: patch.mr_rework_enabled ?? null };
    }
    persistSettings();
    return delay({ settings: { ...userSettings } });
  },

  // ── Slack linking (PRD #25 M3) ───────────────────────────────────────────────
  getMySlack: async () => delay(slackLinkResponse()),
  setMySlackNotify: async (notify: boolean) => {
    slackLink = { ...slackLink, notify };
    return delay(slackLinkResponse());
  },
  setMySlackOverride: async (memberId: string | null) => {
    const member = memberId?.trim() ?? "";
    if (member === "") {
      // Clear the override: fall back to email auto-match (nothing resolved here).
      slackLink = { ...slackLink, member_id: null, resolved_id: null, confirmed: false };
    } else {
      if (!/^[A-Za-z0-9]{1,64}$/.test(member)) throw new ApiError(400, "invalid Slack member ID");
      // A set resets confirmation: the target must Confirm before content flows.
      slackLink = { ...slackLink, member_id: member, resolved_id: member, confirmed: false };
    }
    return delay(slackLinkResponse());
  },
  testMySlackDM: async () => {
    if (!slackLink.resolved_id) throw new ApiError(400, "no linked Slack account to send a test DM to");
    return delay({ status: "sent" });
  },
  getSlackStatus: async () => delay({ slack_status: "disabled" }),

  // ── Agent templates ─────────────────────────────────────────────────────────
  listAgentTemplates: async () => delay({ templates: templates.map(withDrift) }),
  getAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    return delay({ template: withDrift(t) });
  },
  // The shipped definition behind a builtin row, mirroring the server's status
  // matrix: 400 for a row with no shipped counterpart (including a user template
  // that merely shares a builtin's name) and 409 for a builtin this release no
  // longer ships — the state the UI reads as "do not offer Reset".
  getBuiltinAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    if (t.scope !== "builtin") {
      throw new ApiError(400, "only builtin templates have a shipped definition");
    }
    const def = shippedBuiltin(t.name);
    if (!def) throw new ApiError(409, "no builtin definition to reset to");
    return delay({ builtin: { ...def } });
  },
  createAgentTemplate: async (input: AgentTemplateInput) => {
    const me = requireSession();
    if (!input.name || !/^[a-z0-9]+(-[a-z0-9]+)*$/.test(input.name)) {
      throw new ApiError(400, "name must be kebab-case");
    }
    if (LEAD_NAME_RE.test(input.name)) {
      throw new ApiError(400, "name is reserved for the built-in lead orchestrator");
    }
    // Blank scope defaults to global (the pre-M6 admin create).
    const scope = input.scope ?? "global";
    if (scope === "global" && !me.is_admin) {
      throw new ApiError(403, "only admins can create global templates");
    }
    if (scope !== "global" && scope !== "user") {
      throw new ApiError(400, "scope must be 'global' or 'user'");
    }
    // Name uniqueness: shared names are unique across builtin+global; a user's
    // names are unique to that user (they may reuse a builtin/global name).
    const clash =
      scope === "user"
        ? templates.some((t) => t.scope === "user" && t.user_id === me.id && t.name === input.name)
        : templates.some((t) => t.scope !== "user" && t.name === input.name);
    if (clash) {
      throw new ApiError(409, "a template with this name already exists");
    }
    const now = new Date().toISOString();
    const t: AgentTemplate = {
      id: `t-custom-${++templateCounter}`,
      name: input.name,
      description: input.description,
      model: input.model,
      tools: input.tools,
      prompt_body: input.prompt_body,
      is_builtin: false,
      scope,
      user_id: scope === "user" ? me.id : null,
      updated_by: me.email,
      created_at: now,
      updated_at: now,
      // Never a builtin, so never drifted. Recomputed on read anyway.
      differs_from_builtin: false,
    };
    templates.push(t);
    // A new global template is a global default from creation (removable).
    if (scope === "global") templateGlobalDefaults.add(t.id);
    return delay({ template: withDrift(t) });
  },
  getTemplateAllocations: async () => delay({ templates: templateAllocationView(requireSession()) }),
  setTemplateAllocations: async (input: TemplateAllocationsInput) => {
    const me = requireSession();
    if (input.global_default_ids === undefined && input.my_overrides === undefined) {
      throw new ApiError(400, "provide global_default_ids and/or my_overrides");
    }
    const canSee = (id: string) => visibleTemplates(me).some((t) => t.id === id);
    if (input.global_default_ids !== undefined) {
      if (!me.is_admin) throw new ApiError(403, "only admins can set global default allocations");
      for (const id of input.global_default_ids) {
        const t = templates.find((x) => x.id === id);
        if (!t || t.scope === "user") {
          throw new ApiError(400, "only builtin or global templates can be global defaults");
        }
      }
      templateGlobalDefaults.clear();
      for (const id of input.global_default_ids) templateGlobalDefaults.add(id);
    }
    if (input.my_overrides !== undefined) {
      for (const o of input.my_overrides) {
        if (!canSee(o.template_id)) throw new ApiError(400, "one or more templates are not allocatable");
      }
      const overlay = new Map<string, boolean>();
      for (const o of input.my_overrides) overlay.set(o.template_id, o.enabled);
      templateOverrides.set(me.id, overlay);
    }
    return delay({ templates: templateAllocationView(me) });
  },
  updateAgentTemplate: async (id: string, input: AgentTemplateInput) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    t.description = input.description;
    t.model = input.model;
    t.tools = input.tools;
    t.prompt_body = input.prompt_body;
    t.updated_by = requireSession().email;
    t.updated_at = new Date().toISOString();
    return delay({ template: withDrift(t) });
  },
  deleteAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    if (t.is_builtin) throw new ApiError(409, "builtin templates cannot be deleted");
    templates = templates.filter((x) => x.id !== id);
    return delay(null);
  },
  resetAgentTemplate: async (id: string) => {
    const t = templates.find((x) => x.id === id);
    if (!t) throw new ApiError(404, "template not found");
    if (!t.is_builtin) throw new ApiError(400, "only builtins can be reset");
    // The reset target is the SHIPPED definition, not the seeded row. Those were
    // the same object before #201 M4a, which is why a "reset" could never clear a
    // badge: it restored the drifted seed. A builtin this release no longer ships
    // has nothing to reset to and answers 409, exactly as the server does.
    const shipped = shippedBuiltin(t.name);
    if (!shipped) throw new ApiError(409, "no builtin definition to reset to");
    Object.assign(t, {
      description: shipped.description,
      model: shipped.model,
      tools: shipped.tools,
      prompt_body: shipped.prompt_body,
      // Reset restores the embedded default in every sense, including provenance:
      // a previously synced/admin builtin becomes 'embedded' again (PRD #602 M5).
      origin: "embedded",
      updated_at: new Date().toISOString(),
    });
    return delay({ template: withDrift(t) });
  },

  // ── Agent skills (PRD #16) ────────────────────────────────────────────────
  listSkills: async () => delay({ skills: visibleSkills(requireSession()).map((s) => ({ ...s })) }),
  getSkill: async (id: string) => {
    const me = requireSession();
    const s = skills.find((x) => x.id === id);
    if (!s || (!me.is_admin && s.scope === "user" && s.user_id !== me.id)) {
      throw new ApiError(404, "skill not found");
    }
    return delay({ skill: { ...s } });
  },
  createSkill: async (input: SkillCreateInput) => {
    const me = requireSession();
    const name = input.name.trim();
    if (!SKILL_NAME_RE.test(name)) {
      throw new ApiError(400, "name must be kebab-case (lowercase letters, digits, hyphens; max 64 chars)");
    }
    if (input.scope === "global") {
      if (!me.is_admin) throw new ApiError(403, "only admins can create global skills");
    } else if (input.scope !== "user") {
      throw new ApiError(400, "scope must be 'global' or 'user'");
    }
    const descErr = descriptionError(input.description);
    if (descErr) throw new ApiError(400, descErr);
    const bErr = bodyError(input.body);
    if (bErr) throw new ApiError(400, bErr);
    const clash = skills.some((s) =>
      s.name === name &&
      (input.scope === "user" ? s.scope === "user" && s.user_id === me.id : s.scope !== "user"),
    );
    if (clash) throw new ApiError(409, "a skill with that name already exists");
    const now = new Date().toISOString();
    const s: Skill = {
      id: `skill-custom-${++skillCounter}`,
      name,
      description: input.description.trim(),
      body: input.body,
      scope: input.scope,
      user_id: input.scope === "user" ? me.id : null,
      updated_by: me.email,
      created_at: now,
      updated_at: now,
    };
    skills.push(s);
    return delay({ skill: { ...s } }, 300);
  },
  updateSkill: async (id: string, input: SkillUpdateInput) => {
    const me = requireSession();
    const s = skills.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "skill not found");
    if (s.scope === "builtin" || s.scope === "global") {
      if (!me.is_admin) throw new ApiError(403, "you do not have permission to modify this skill");
    } else if (s.user_id !== me.id) {
      throw new ApiError(me.is_admin ? 403 : 404, me.is_admin ? "you do not have permission to modify this skill" : "skill not found");
    }
    const descErr = descriptionError(input.description);
    if (descErr) throw new ApiError(400, descErr);
    const bErr = bodyError(input.body);
    if (bErr) throw new ApiError(400, bErr);
    s.description = input.description.trim();
    s.body = input.body;
    s.updated_by = me.email;
    s.updated_at = new Date().toISOString();
    return delay({ skill: { ...s } });
  },
  deleteSkill: async (id: string) => {
    const me = requireSession();
    const s = skills.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "skill not found");
    if (s.scope === "builtin") throw new ApiError(409, "builtin skills cannot be deleted; reset them instead");
    if (s.scope === "global") {
      if (!me.is_admin) throw new ApiError(403, "you do not have permission to modify this skill");
    } else if (s.user_id !== me.id) {
      throw new ApiError(me.is_admin ? 403 : 404, me.is_admin ? "you do not have permission to modify this skill" : "skill not found");
    }
    skills = skills.filter((x) => x.id !== id);
    return delay(null);
  },
  resetSkill: async (id: string) => {
    const me = requireSession();
    const s = skills.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "skill not found");
    if (s.scope !== "builtin") throw new ApiError(400, "only builtin skills can be reset");
    if (!me.is_admin) throw new ApiError(403, "you do not have permission to modify this skill");
    const shipped = mockSkills.find((x) => x.id === id)!;
    s.description = shipped.description;
    s.body = shipped.body;
    s.updated_by = me.email;
    s.updated_at = new Date().toISOString();
    return delay({ skill: { ...s } });
  },
  getTemplateSkills: async (id: string) => {
    requireSession();
    if (!templates.some((t) => t.id === id)) throw new ApiError(404, "template not found");
    return delay({ allocations: allocationView(id) });
  },
  setTemplateSkills: async (id: string, input: AllocationsInput) => {
    const me = requireSession();
    if (!templates.some((t) => t.id === id)) throw new ApiError(404, "template not found");
    if (input.shared_skill_ids === undefined && input.my_skill_ids === undefined) {
      throw new ApiError(400, "provide shared_skill_ids and/or my_skill_ids");
    }
    const a = allocations[id] ?? { shared: [], mine: [] };
    if (input.shared_skill_ids !== undefined) {
      if (!me.is_admin) throw new ApiError(403, "only admins can set shared skill allocations");
      for (const sid of input.shared_skill_ids) {
        const sk = skills.find((x) => x.id === sid);
        if (!sk || (sk.scope !== "builtin" && sk.scope !== "global")) {
          throw new ApiError(400, "one or more skills are not allocatable");
        }
      }
      a.shared = [...new Set(input.shared_skill_ids)];
    }
    if (input.my_skill_ids !== undefined) {
      for (const sid of input.my_skill_ids) {
        const sk = skills.find((x) => x.id === sid);
        const ok = sk && (sk.scope === "builtin" || sk.scope === "global" || (sk.scope === "user" && sk.user_id === me.id));
        if (!ok) throw new ApiError(400, "one or more skills are not allocatable");
      }
      a.mine = [...new Set(input.my_skill_ids)];
    }
    allocations[id] = a;
    return delay({ allocations: allocationView(id) });
  },

  // ── Forge ───────────────────────────────────────────────────────────────────
  forgeConfig: async () => delay({ ...mockForgeConfig, allowed_base_urls: [...mockForgeConfig.allowed_base_urls] }),
  listConnections: async () => delay({ connections: connections.map((c) => ({ ...c })) }),
  createConnection: async (baseUrl: string, _token: string, forgeType = "gitlab") => {
    const conn = {
      ...mockConnection,
      id: `conn-${Date.now()}`,
      base_url: baseUrl,
      forge_type: forgeType,
      created_at: new Date().toISOString(),
      last_verified_at: new Date().toISOString(),
      // A freshly connected bot is unchecked until the first privilege check.
      privilege_status: null,
      privilege_checked_at: null,
      privilege_report: null,
    };
    connections = [conn];
    return delay({ connection: { ...conn } }, 600);
  },
  verifyConnection: async (id: string) => {
    const c = connections.find((x) => x.id === id);
    if (!c) throw new ApiError(404, "connection not found");
    c.last_verified_at = new Date().toISOString();
    return delay({ connection: { ...c } }, 500);
  },
  // Mirrors the real save path (PRD #19 M3): a collision on the same host is a hard
  // 409, an unknown username still saves but returns a warning (verified-or-warned),
  // and "" clears the mapping.
  updateConnection: async (id: string, humanUsername: string) => {
    const c = connections.find((x) => x.id === id);
    if (!c) throw new ApiError(404, "connection not found");
    const username = humanUsername.trim();
    if (username) {
      const clash = connections.some(
        (x) => x.id !== id && x.base_url === c.base_url && x.human_username === username,
      );
      if (clash) {
        throw new ApiError(409, "that forge username is already mapped by another user on this host");
      }
    }
    c.human_username = username || null;
    // Demo the warning branch for an obviously-fake username without a live forge.
    const warning =
      username && username.toLowerCase() === "ghost"
        ? "Saved, but no forge account with this username was found — double-check it matches your own forge username."
        : undefined;
    return delay({ connection: { ...c }, ...(warning ? { warning } : {}) }, 400);
  },
  privilegeCheck: async (id: string) => {
    const c = connections.find((x) => x.id === id);
    if (!c) throw new ApiError(404, "connection not found");
    const now = new Date().toISOString();
    const report: PrivilegeReport = {
      checked_at: now,
      status: "ok",
      token: { scopes: ["api"], active: true, violations: [], warnings: [] },
      repos: repos
        .filter((r) => r.enabled)
        .map((r) => ({
          repo_id: r.id,
          path: r.path_with_namespace,
          role: "write",
          member: true,
          findings: [],
        })),
    };
    c.privilege_status = "ok";
    c.privilege_checked_at = now;
    c.privilege_report = report;
    return delay({ report }, 500);
  },
  deleteConnection: async (id: string) => {
    connections = connections.filter((x) => x.id !== id);
    return delay(null);
  },
  listProjects: async (_connectionId: string) => delay({ repos: repos.map((r) => ({ ...r })) }, 350),

  listRepos: async () => delay({ repos: repos.filter((r) => r.enabled).map((r) => ({ ...r })) }),
  setRepoEnabled: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    // PRD #345 M2: mirror the server, which runs the enable guardrail (privcheck)
    // ONLY on the enable path (forge.go SetRepoEnabled). A refused enable returns
    // 422 { error, violations[] }; a disable is never gated. This makes the
    // refused-enable UX (Repos.tsx) reproducible under VITE_UZI_MOCK=1.
    if (enabled && r.guardrail_blocked) {
      const violations = mockBlockedRepoMeta[id]?.block_messages ?? [
        "this repository cannot be enabled until its guardrail violations are resolved",
      ];
      throw new ApiError(422, "repository cannot be enabled — guardrail violations", { violations });
    }
    r.enabled = enabled;
    return delay({ repo: { ...r } });
  },
  // Explicit per-repo remove (PRD #357). Mirrors deleteConnection: drops the repo
  // from the in-memory list so demo/browser mode reflects the deletion. Enforces
  // the server's disabled-only guard (409 on an enabled repo) so the flow behaves
  // like the real endpoint without any /api/ call escaping the mock.
  deleteRepo: async (id: string) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    if (r.enabled) throw new ApiError(409, "disable this repo before removing it");
    repos = repos.filter((x) => x.id !== id);
    return delay(null);
  },
  setRepoSkillsEnabled: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_skills_enabled = enabled;
    return delay({ repo: { ...r } });
  },
  setRepoClaudemdEnabled: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_claudemd_enabled = enabled;
    return delay({ repo: { ...r } });
  },
  // Trusted-repo master control (PRD #246): sets whichever of the two trust flags
  // are present in one call, mirroring the server's atomic both-flags path.
  setRepoTrustFlags: async (
    id: string,
    flags: { repo_skills_enabled?: boolean; repo_claudemd_enabled?: boolean },
  ) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    if (flags.repo_skills_enabled !== undefined) r.repo_skills_enabled = flags.repo_skills_enabled;
    if (flags.repo_claudemd_enabled !== undefined) r.repo_claudemd_enabled = flags.repo_claudemd_enabled;
    return delay({ repo: { ...r } });
  },
  setRepoDevboxOptIn: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_devbox_opt_in = enabled;
    return delay({ repo: { ...r } });
  },
  // PRD #686: per-repo self-improve dogfooding capability. Owner-or-admin on the
  // server; the mock just flips the flag and echoes the repo.
  setRepoFoldImproveUziBacklog: async (id: string, enabled: boolean) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.repo_fold_improve_uzi_backlog = enabled;
    return delay({ repo: { ...r } });
  },
  // PRD #84 M2: static per-repo capability hint. Mirrors the server's
  // capability.Filter to the {docker, jvm} vocabulary so only valid names persist.
  setRepoRequiredCapabilities: async (id: string, caps: string[]) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    const vocab = new Set<string>(CAPABILITY_VOCABULARY);
    r.required_capabilities = caps.filter((c) => vocab.has(c));
    return delay({ repo: { ...r } });
  },
  // PRD #66 M9 (D8): admin per-repo guardrail override. Requires a non-empty reason
  // (mirrors the server 400). Setting it clears guardrail_blocked in the demo (the
  // override downgrades the waivable findings); revoking re-arms it as blocked.
  setRepoGuardrailOverride: async (id: string, reason: string) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    if (reason.trim() === "") throw new ApiError(400, "a non-empty reason is required to override the guardrail");
    r.guardrail_override = { reason: reason.trim(), by: "you@example.com", at: new Date().toISOString() };
    r.guardrail_blocked = false;
    return delay({ repo: { ...r } });
  },
  clearRepoGuardrailOverride: async (id: string) => {
    const r = repos.find((x) => x.id === id);
    if (!r) throw new ApiError(404, "repo not found");
    r.guardrail_override = null;
    // Re-arm the guardrail: absent the override, the repo is blocked again iff its
    // underlying findings are still waivable blocks (mock stand-in: seeded block
    // messages). This is what makes Revoke round-trip back to "runs blocked".
    r.guardrail_blocked = (mockBlockedRepoMeta[id]?.block_messages.length ?? 0) > 0;
    return delay({ repo: { ...r } });
  },

  // ── GitHub Projects v2 sync (PRD #534) ───────────────────────────────────────
  // Read the repo's link status. Mirrors the server: a linked repo returns its
  // status object; an unlinked one 404s ("project sync not enabled for this repo"),
  // the same 404 the server uses to hide existence from a non-owner.
  getProjectSyncStatus: async (id: string) => {
    const link = githubProjectLinks.get(id);
    if (!link) throw new ApiError(404, "project sync not enabled for this repo");
    return delay({ ...link });
  },
  // Owner type for the Adopt-first Provision nudge (PRD #576 M1). The mock treats
  // every repo as org-owned so Provision stays available in the demo/offline mode.
  getProjectSyncOwnerType: async (id: string) => {
    void id;
    return delay<{ owner_type: "User" | "Organization" }>({ owner_type: "Organization" });
  },
  // Provision a fresh project: record a uzi-owned link and return the created status.
  provisionProjectSync: async (
    id: string,
    body: { owner_kind: ProjectSyncOwnerKind; title?: string },
  ) => {
    void body;
    githubProjectLinks.set(id, {
      project_number: 1000 + githubProjectLinks.size,
      owned_by_uzi: true,
      last_synced_at: null,
      last_error: null,
      item_count: 0,
    });
    return delay({ status: "provisioned" });
  },
  // Adopt an existing project by number: record an adopted (not uzi-owned) link.
  adoptProjectSync: async (
    id: string,
    body: { project_number: number; owner_kind: ProjectSyncOwnerKind },
  ) => {
    githubProjectLinks.set(id, {
      project_number: body.project_number,
      owned_by_uzi: false,
      last_synced_at: null,
      last_error: null,
      item_count: 0,
    });
    return delay({ status: "linked" });
  },
  // Re-seed an already-linked board (PRD #576 M3). A 404 when not linked, mirroring
  // the server's not-linked sentinel; otherwise a no-op idempotent re-seed.
  resyncProjectSync: async (id: string) => {
    if (!githubProjectLinks.has(id))
      throw new ApiError(404, "this repo has no linked project to resync");
    return delay({ status: "resynced" });
  },
  // Safe column auto-create (PRD #576 M6): create a fresh uzi-owned field with all the
  // repo's columns and switch the link to it. In the mock, clear the unmatched set so the
  // panel reflects "all columns now sync".
  autocreateProjectSyncColumns: async (id: string) => {
    const link = githubProjectLinks.get(id);
    if (!link)
      throw new ApiError(404, "this repo has no linked project to auto-create columns for");
    githubProjectLinks.set(id, { ...link, unmatched_columns: [] });
    return delay({ status: "columns_created" });
  },
  // Unlink the repo from its project (empty 204 body).
  disableProjectSync: async (id: string) => {
    githubProjectLinks.delete(id);
    githubProjectVisibility.delete(id);
    return delay(null);
  },

  // ── Board access: visibility + write-only sharing (PRD #557) ────────────────
  // Read the linked board's public flag. Mirrors the server: an unlinked repo
  // 404s (same existence-hiding 404 as the status route); a linked one defaults
  // to private until the toggle flips it.
  getProjectSyncVisibility: async (id: string) => {
    if (!githubProjectLinks.has(id))
      throw new ApiError(404, "project sync not enabled for this repo");
    return delay({ public: githubProjectVisibility.get(id) ?? false });
  },
  // Flip the board's public flag. The value is stored so the toggle round-trips.
  setProjectSyncVisibility: async (id: string, isPublic: boolean) => {
    githubProjectVisibility.set(id, isPublic);
    return delay({ public: isPublic });
  },
  // Grant a GitHub user Reader access (empty 204 body). The designated login
  // "nouser" 422s (ErrProjectSyncUserNotFound) so the bad-username inline-error
  // vitest case exercises a real failure, not a resolved success.
  shareProjectSync: async (id: string, username: string) => {
    void id;
    if (username.trim() === "nouser")
      throw new ApiError(422, "no github user with that username");
    return delay(null);
  },
  // Revoke a GitHub user's access (empty 204 body).
  unshareProjectSync: async (id: string, username: string) => {
    void id;
    void username;
    return delay(null);
  },

  // ── Tool allowlist + repo tool profiles (PRD #18 M4) ─────────────────────────
  listToolAllowlist: async () => delay({ allowlist: toolAllowlist.map((e) => ({ ...e })) }),
  createToolAllowlistEntry: async (input: ToolAllowlistWriteInput) => {
    const name = (input.name ?? "").trim();
    if (name === "") throw new ApiError(400, "name is required");
    if (toolAllowlist.some((e) => e.name === name)) throw new ApiError(409, "that package is already on the allowlist");
    const now = new Date().toISOString();
    const entry: ToolAllowlistEntry = {
      id: `tal-${++toolEntryCounter}`,
      name,
      pinned_version: input.pinned_version?.trim() || null,
      note: input.note?.trim() || null,
      updated_by: requireSession().id,
      created_at: now,
      updated_at: now,
    };
    toolAllowlist = [...toolAllowlist, entry].sort((a, b) => a.name.localeCompare(b.name));
    return delay({ entry: { ...entry } });
  },
  updateToolAllowlistEntry: async (id: string, input: ToolAllowlistWriteInput) => {
    const entry = toolAllowlist.find((e) => e.id === id);
    if (!entry) throw new ApiError(404, "allowlist entry not found");
    entry.pinned_version = input.pinned_version?.trim() || null;
    entry.note = input.note?.trim() || null;
    entry.updated_at = new Date().toISOString();
    return delay({ entry: { ...entry } });
  },
  deleteToolAllowlistEntry: async (id: string) => {
    toolAllowlist = toolAllowlist.filter((e) => e.id !== id);
    return delay(null);
  },
  getRepoToolProfile: async (repoId: string) => {
    if (!repos.some((r) => r.id === repoId)) throw new ApiError(404, "repo not found");
    return delay({ packages: [...(repoToolProfiles.get(repoId) ?? [])] });
  },
  setRepoToolProfile: async (repoId: string, packages: string[]) => {
    if (!repos.some((r) => r.id === repoId)) throw new ApiError(404, "repo not found");
    // Mirror the server's allowlist validation so the demo rejects the same way.
    const allowed = new Set<string>();
    for (const e of toolAllowlist) allowed.add(e.pinned_version ? `${e.name}@${e.pinned_version}` : e.name);
    const rejected = packages.filter((p) => !allowed.has(p));
    if (rejected.length > 0) throw new ApiError(400, "these packages are not on the allowlist: " + rejected.join(", "));
    const cleaned = [...new Set(packages)].sort();
    repoToolProfiles.set(repoId, cleaned);
    return delay({ packages: cleaned });
  },

  // ── Board ───────────────────────────────────────────────────────────────────
  getBoard: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    return delay(boardResponse(b));
  },
  configureColumns: async (repoId: string, columns: { label_name: string }[]) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    b.columns = columns.map((c, i) => ({ label_name: c.label_name, position: i }));
    const names = new Set(b.columns.map((c) => c.label_name));
    for (const card of b.cards) if (card.column && !names.has(card.column)) card.column = "";
    return delay(boardResponse(b));
  },
  // Manual board order (PRD #102 M5). This is a SECOND IMPLEMENTATION of the server's
  // freeze, so it is a contract, not a convenience: mockApi.reorder.test.ts pins the
  // four behaviours below one case each, because a fixture that only walks the happy
  // path agrees with a broken mock on everything it covers.
  //
  // The demo board has no evicted iid and no unlisted open card, so a snapshot-style
  // fixture would pass against a mock missing (2) and (3) entirely.
  //
  //   1. cards are reordered to the submitted iid order;
  //   2. an iid not on the board is SKIPPED, not thrown on (the server no-ops per iid,
  //      because an eviction can land between a client's render and its submit);
  //   3. open cards absent from the list fall to the end in iid order (the mirror of
  //      the server's ClearBoardOrderExcept nulling them, plus its NULLS-LAST read);
  //   4. closed cards are untouched and keep their place.
  reorderBoard: async (repoId: string, iids: number[]) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    if (iids.length > 0) {
      const byIID = new Map(b.cards.map((c) => [c.iid, c]));
      const seen = new Set<number>();
      const ordered: Card[] = [];
      for (const iid of iids) {
        const card = byIID.get(iid);
        // (2) unknown iid: skip. (Also skips a duplicate, matching the server's dedupe.)
        //
        // KNOWN DIVERGENCE, recorded rather than left to be discovered (review M5-7):
        // this also skips a CLOSED card, and SetBoardOrderPositions does not — the
        // server would happily rank one it was handed. Unreachable from the product,
        // because dropIntent filters closed cards out before the request is built, so
        // neither side ever sees one. Kept on the mock side because it is the safer
        // half of the divergence and because the demo board contains closed cards: a
        // hand-built mock-mode request that ranked one would render it in the Closed
        // lane at a rank, which is exactly the state Decision 7b forbids. If the server
        // ever gains its own filter, delete this clause rather than adding a second.
        if (!card || card.closed || seen.has(iid)) continue;
        seen.add(iid);
        ordered.push(card);
      }
      // (3) + (4): everything not named keeps a NULL position server-side, which reads
      // back after the positioned rows, in iid order. Closed cards live here too and so
      // are never given a rank.
      const rest = b.cards.filter((c) => !seen.has(c.iid)).sort((x, y) => x.iid - y.iid);
      b.cards = [...ordered, ...rest];
    }
    return delay(boardResponse(b));
  },
  moveIssue: async (repoId: string, iid: number, toColumn: string) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    const to = toColumn === "open" ? "" : toColumn;
    const columnNames = b.columns.map((c) => c.label_name);
    // Preserve every non-column label (incl. `uzi`) and set the new column label.
    card.labels = [...card.labels.filter((l) => !columnNames.includes(l)), ...(to ? [to] : [])];
    card.column = to;
    card.conflict = false;
    return delay({ card: { ...card } }, 320);
  },
  // Promote (PRD #102 M6, Decision 15; PRD #764): add the configured `uzi` label,
  // apply-only and idempotent. Refuses uzi's own self-improvement tracker the way the
  // server does (Decision 13a), so the demo build cannot show a promote the real API
  // would 422.
  promoteIssue: async (repoId: string, iid: number) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    if (card.labels.includes("uzi-self-improve")) {
      throw new ApiError(422, "this issue is uzi's own self-improvement tracker and cannot be promoted");
    }
    const label = appSettings.uzi_label;
    if (!card.labels.includes(label)) card.labels = [label, ...card.labels];
    return delay({ card: { ...card } }, 320);
  },
  getIssue: async (repoId: string, iid: number) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    // IssueDetail is the card fields (minus latest_run) plus a live description.
    // Synthesize one consistent with has_prd_link so the PRD badge lines up with what
    // the description shows (a linked PRD is optional — PRD #764).
    const { latest_run: _latestRun, ...rest } = card;
    const description = card.has_prd_link
      ? `## Summary\n\nImplement the change described in the linked PRD.\n\nSee \`prds/${iid}-feature.md\` for the full specification.`
      : "This issue has no linked `prds/*.md` file. A PRD is optional — label the issue `uzi` and a run can still be started from it.";
    // bot_forge_user_id rides the issue detail (PRD #767 M5), from the board's single
    // connection, so the issue view evaluates the same "uzi OR assigned-to-bot" predicate
    // as the board card — assignee_ids comes through on ...rest.
    return delay({ issue: { ...rest, assignee_ids: rest.assignee_ids ?? [], description, bot_forge_user_id: b.bot_forge_user_id ?? 0 } });
  },
  syncRepo: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    return delay(boardResponse(b), 650);
  },
  createIssue: async (repoId: string, title: string, description: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    const iid = Math.max(0, ...b.cards.map((c) => c.iid)) + 1;
    const card = {
      iid,
      title,
      state: "opened",
      labels: [appSettings.uzi_label],
      assignee_ids: [] as number[],
      web_url: `${b.web_url}/-/issues/${iid}`,
      forge_type: "gitlab",
      author: requireSession().display_name?.toLowerCase() ?? "you",
      has_prd_link: /prds\/[\w.-]+\.md/.test(description),
      column: "",
      closed: false,
      conflict: false,
      // A just-created issue is the most recently updated thing on the board, so it
      // must lead in "Last updated" mode rather than sinking on a zero value.
      forge_updated_at: new Date().toISOString(),
      latest_run: null,
      pipeline: null,
    };
    b.cards.unshift(card);
    return delay({ card: { ...card } }, 450);
  },

  // Per-user, per-repo board preferences (PRD #196 M3). A SECOND IMPLEMENTATION of the
  // server contract, so it persists across calls within the session and matches the
  // wire shape exactly: null extra_labels = "not customised" (fall back to the admin
  // default), an array (incl. []) = the user's absolute set (Decision 9).
  getBoardPrefs: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    // No row yet reads as the pristine default rather than being seeded, so a later
    // reset back to null and "never touched" stay indistinguishable to the client.
    const prefs = state.boardPrefs.get(repoId) ?? { extra_labels: null, show_all: false };
    return delay<BoardPrefs>({ extra_labels: prefs.extra_labels, show_all: prefs.show_all });
  },
  setBoardPrefs: async (repoId: string, prefs: BoardPrefs) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    // Loose validation for mock-mode parity with the server: each extra label must be
    // a non-empty, comma-free, ≤64-char string; an over-cap list is clamped rather
    // than rejected. null (not customised) is preserved as the sentinel.
    let extraLabels: string[] | null = null;
    if (Array.isArray(prefs.extra_labels)) {
      const cleaned: string[] = [];
      for (const raw of prefs.extra_labels) {
        const l = String(raw).trim();
        if (l === "" || l.includes(",") || l.length > 64) continue;
        if (!cleaned.includes(l)) cleaned.push(l);
      }
      extraLabels = cleaned.slice(0, 64);
    }
    const stored: BoardPrefs = { extra_labels: extraLabels, show_all: Boolean(prefs.show_all) };
    state.boardPrefs.set(repoId, stored);
    return delay<BoardPrefs>({ extra_labels: stored.extra_labels, show_all: stored.show_all }, 320);
  },

  // ── Workers ─────────────────────────────────────────────────────────────────
  listWorkers: async () => delay({ workers: workers.map((w) => ({ ...w })) }),
  createWorker: async (name: string, template?: string) => {
    const w = {
      id: `w-new-${++workerCounter}`,
      name,
      status: "offline",
      busy: false,
      // Hand-run: the user starts this container themselves, so it has no size
      // (PRD #58). provisionHostedWorker below is the other kind.
      kind: "external" as const,
      hosted_size: null,
      // No runs and no advertised cap until the worker registers (PRD #42).
      active_runs: 0,
      max_concurrent_runs: null,
      // Declared at issuance; reported stays null until the worker registers.
      template_declared: template ?? null,
      template_reported: null,
      version: null,
      // No version reported until the worker registers, so nothing to compare
      // against the control-plane release (PRD #113).
      upgrade_status: "unknown" as const,
      upgrade_detail: null,
      upgrade_target: "" as const,
      upgrade_blocking_container: null,
      upgrade_blocking_reason: null,
      upgrade_last_exit_code: null,
      last_heartbeat_at: null,
      created_at: new Date().toISOString(),
      // No resource sample until the worker heartbeats (PRD #49) → no gauges yet.
      stats_cpu_pct: null,
      stats_mem_bytes: null,
      stats_mem_limit_bytes: null,
      stats_source: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_bind_mode: "default" as const,
      draining_since: null,
    };
    workers.push(w);
    const token = `uzi_wk_${Array.from(crypto.getRandomValues(new Uint8Array(18)), (b) => b.toString(16).padStart(2, "0")).join("")}`;
    return delay({ worker: { ...w }, token });
  },
  deleteWorker: async (id: string) => {
    workers = workers.filter((w) => w.id !== id);
    return delay(null);
  },
  // PRD #104 M3: rebind a worker to a named token, or clear it with null. Mirrors
  // the real route's label→id resolution and its 400 for an unknown label, so the
  // picker's error path is browsable.
  setWorkerBindMode: async (id: string, mode: BindMode, label: string | null) => {
    const w = workers.find((x) => x.id === id);
    if (!w) throw new ApiError(404, "worker not found");
    // Mirrors the server's refusal of a contradictory pair, so the picker's error
    // path is browsable in the mock rather than only in production.
    if (mode !== "pinned" && label !== null && label.trim() !== "") {
      throw new ApiError(400, "anthropic_token must be null when anthropic_bind_mode is default or auto");
    }
    if (mode !== "pinned") {
      w.anthropic_bind_mode = mode;
      w.anthropic_secret_id = null;
      w.anthropic_secret_label = null;
      return delay({ worker: { ...w } });
    }
    if (label === null || label.trim() === "") {
      throw new ApiError(400, "anthropic_bind_mode=pinned requires a token label in anthropic_token");
    }
    const secret = secrets.find(
      (x) => x.kind === "anthropic_token" && x.label.toLowerCase() === label.trim().toLowerCase(),
    );
    if (!secret) throw new ApiError(400, "no Anthropic token with that label");
    w.anthropic_bind_mode = "pinned";
    w.anthropic_secret_id = secret.id;
    w.anthropic_secret_label = secret.label;
    return delay({ worker: { ...w } });
  },

  // Hosted workers (PRD #58). The demo is the only place M5 can be seen working: on a
  // real stack WORKER_HOSTING_ENABLED is off by default, and turning it on gets you a
  // worker that sits offline forever, because the controller that would run its pod is
  // M3's. So hosting is hardcoded ON here — a demo of a feature that renders nothing
  // is not a demo — and quota 2 against one seeded hosted worker puts the whole
  // journey three clicks away: provision → 2 of 2 → the button disables → delete →
  // it enables again.
  // Quota 5 against FOUR seeded hosted workers (PRD #496 added two cordoned demo
  // workers, w-cordon-eu and w-cordon-idle, on top of the two PRD #113 M5 seeded). The
  // load-bearing property is unchanged and is why the numbers moved together: there is
  // exactly ONE slot of headroom, so web-ux can still drive provision -> at quota ->
  // button disables -> delete -> it enables again, which is the only way to prove the
  // client-side gate RELEASES rather than merely starting disabled.
  //
  // The second seeded worker is the failed roller, which the demo previously could not
  // show at all — so a browser pass could only ever validate the healthy path.
  hostedConfig: async () => delay({ enabled: true, quota: 5, ephemeral_enabled: true }),
  provisionHostedWorker: async (template: string, size: string, docker = false, name?: string) => {
    const w = {
      id: `w-hosted-${++workerCounter}`,
      // Empty name → the server derives one from template + size (handler's
      // derivedHostedWorkerName), now AWS-style `base.l-<4-hex>`: dot notation,
      // lowercase t-shirt letter, random hex suffix. The M5 form sends none, so this
      // is the live path. The real suffix is crypto/rand; the mock stands in a
      // counter-derived 4-hex from workerCounter (already ++'d for the id) to stay
      // deterministic-enough for tests — a plausible suffix, not byte-for-byte parity.
      name:
        name?.trim() ||
        `${template}.${size.toLowerCase()}-${(workerCounter & 0xffff).toString(16).padStart(4, "0")}`,
      // Offline until the controller starts the pod and it registers — the same
      // lifecycle a hand-run worker has, just with the controller doing the running.
      status: "offline",
      busy: false,
      active_runs: 0,
      max_concurrent_runs: null,
      kind: "hosted" as const,
      hosted_size: size,
      docker,
      template_declared: template,
      template_reported: null,
      version: null,
      // No version reported until the worker registers, so nothing to compare
      // against the control-plane release (PRD #113).
      upgrade_status: "unknown" as const,
      upgrade_detail: null,
      upgrade_target: "" as const,
      upgrade_blocking_container: null,
      upgrade_blocking_reason: null,
      upgrade_last_exit_code: null,
      last_heartbeat_at: null,
      created_at: new Date().toISOString(),
      stats_cpu_pct: null,
      stats_mem_bytes: null,
      stats_mem_limit_bytes: null,
      stats_source: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_bind_mode: "default" as const,
      draining_since: null,
    };
    workers.push(w);
    // { worker } and NOTHING ELSE. Do not mint a token here the way createWorker does
    // above: the real endpoint's transaction returns none, its response cannot carry
    // one, and a mock that invents one documents a contract the server structurally
    // cannot honor. TypeScript will not catch it (delay() infers its own T, so an
    // extra field type-checks clean) — mockApi.hosted.test.ts is what does.
    return delay({ worker: { ...w } });
  },

  // ── Runs ────────────────────────────────────────────────────────────────────
  createRun: async (repoId: string, issueIid: number) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === issueIid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    const active = [...state.runs.values()].some(
      (r) => r.repo_id === repoId && r.issue_iid === issueIid && !["completed", "failed", "cancelled"].includes(r.status),
    );
    if (active) throw new ApiError(409, "a run is already in progress for this issue");
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: repoId,
      forge_type: "gitlab",
      mr_web_url: null,
      issue_web_url: null,
      kind: "issue",
      issue_iid: issueIid,
      issue_title: card.title,
      issue_description: "See the linked PRD.",
      title: null,
      resume_of_run_id: null,
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
      branch: null,
      model: null,
      override_subagent_model: false,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      stop_reason: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      pipeline_ref: null,
      pipeline_web_url: null,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      // PRD #122: a freshly synthesised run has no milestones yet — all null, so it
      // renders on the null-fallback path (the iteration badge, no checklist).
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      budget_max_iterations: null,
      budget_wall_seconds: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_select_reason: null,
      anthropic_headroom_pct: null,
      wait_on_limit: false,
      limit_resets_at: null,
      retry_not_before: null,
      limit_wait_count: 0,
      rate_limit_type: null,
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    startNewRun(run.id);
    return delay({ run: { ...run } }, 350);
  },
  createCIFixRun: async (repoId: string, ref: string) => {
    if (!state.boards.get(repoId)) throw new ApiError(404, "repo not found");
    const active = [...state.runs.values()].some(
      (r) => r.repo_id === repoId && r.kind === "ci_fix" && r.pipeline_ref === ref && !["completed", "failed", "cancelled"].includes(r.status),
    );
    if (active) throw new ApiError(409, "an active CI-fix run already exists for this ref");
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: repoId,
      forge_type: "gitlab",
      mr_web_url: null,
      issue_web_url: null,
      kind: "ci_fix",
      issue_iid: null,
      issue_title: `Fix CI: ${ref} pipeline`,
      issue_description: `Diagnose and fix the failed pipeline for \`${ref}\`.`,
      title: null,
      resume_of_run_id: null,
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
      branch: null,
      model: null,
      override_subagent_model: false,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      stop_reason: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      pipeline_ref: ref,
      pipeline_web_url: `https://gitlab.example.com/myorg/uzi/-/pipelines/4242`,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      // PRD #122: a freshly synthesised run has no milestones yet — all null, so it
      // renders on the null-fallback path (the iteration badge, no checklist).
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      budget_max_iterations: null,
      budget_wall_seconds: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_select_reason: null,
      anthropic_headroom_pct: null,
      wait_on_limit: false,
      limit_resets_at: null,
      retry_not_before: null,
      limit_wait_count: 0,
      rate_limit_type: null,
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    startNewRun(run.id);
    return delay({ run: { ...run } }, 350);
  },
  listRuns: async (params?: {
    repoId?: string;
    issueIid?: number;
    // Mirrors the real client's passive-poll flag (#331); the mock does no real
    // fetch, so the marker has no effect here beyond keeping the types compatible.
    passive?: boolean;
  }) =>
    delay({
      runs: listRunsFor()
        // Chat conversations ride runs but have their own page (PRD #39), and judge
        // is a repo-less meta-run — both are excluded here exactly as the real
        // ListRunsForUser excludes them (`kind NOT IN ('chat','judge')`, PRD #239 D4).
        .filter((r) => r.kind !== "chat" && r.kind !== "judge")
        // Caller-scoped, like the real ListRunsForUser: other demo users' runs
        // (mockOtherRunOwners) belong to the admin all-users list only.
        .filter((r) => !(r.id in mockOtherRunOwners))
        .filter((r) => (params?.repoId ? r.repo_id === params.repoId : true))
        .filter((r) => (params?.issueIid != null ? r.issue_iid === params.issueIid : true))
        .map((r) => runListItem(r)),
    }),
  // PRD #40: token/cost usage. Static demo figures — enough to populate the
  // dashboard's "Your usage" and (admin) factory cards + per-user table.
  getUsage: async () =>
    delay({
      lifetime: { input_tokens: 1_610_000, cache_read_tokens: 16_100_000, cache_creation_tokens: 240_000, output_tokens: 710_000, cost_usd: 26.4 },
      last_7_days: { input_tokens: 280_000, cache_read_tokens: 2_800_000, cache_creation_tokens: 40_000, output_tokens: 120_000, cost_usd: 4.55 },
      run_count: 23,
    }),
  getAdminUsage: async () =>
    delay({
      factory: {
        lifetime: { input_tokens: 5_400_000, cache_read_tokens: 53_900_000, cache_creation_tokens: 900_000, output_tokens: 2_400_000, cost_usd: 88.15 },
        last_7_days: { input_tokens: 900_000, cache_read_tokens: 9_100_000, cache_creation_tokens: 120_000, output_tokens: 410_000, cost_usd: 14.9 },
        run_count: 79,
      },
      users: [
        { user_id: "u-maria", email: "maria@example.com", usage: { input_tokens: 2_490_000, cache_read_tokens: 22_400_000, cache_creation_tokens: 400_000, output_tokens: 1_020_000, cost_usd: 37.83 }, run_count: 31 },
        { user_id: "u-vlad", email: "vlad@example.com", usage: { input_tokens: 1_610_000, cache_read_tokens: 16_100_000, cache_creation_tokens: 240_000, output_tokens: 710_000, cost_usd: 26.4 }, run_count: 23 },
        { user_id: "u-andrei", email: "andrei@example.com", usage: { input_tokens: 1_010_000, cache_read_tokens: 13_600_000, cache_creation_tokens: 210_000, output_tokens: 550_000, cost_usd: 19.71 }, run_count: 19 },
        { user_id: "u-dana", email: "dana@example.com", usage: { input_tokens: 290_000, cache_read_tokens: 3_500_000, cache_creation_tokens: 50_000, output_tokens: 120_000, cost_usd: 4.21 }, run_count: 6 },
      ],
      earliest_run: "2026-05-12T09:00:00Z",
    }),
  // ── Claude rate limits (PRD #53) ───────────────────────────────────────────
  // The caller's own reading follows the persona (a demo login as a seeded
  // non-admin shows danger / unavailable / no-token); the admin table covers every
  // row state. Percentages only — no token material ever appears here.
  getMyRateLimits: async () => {
    const me = requireSession();
    return delay({ tokens: mockMyRateLimitsByUser[me.id] ?? mockMyTokenRateLimits }, 60);
  },
  getAdminRateLimits: async () => delay({ users: mockAdminRateLimits.map((u) => ({ ...u })) }, 60),
  getRun: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (id === LIVE_RUN_ID) ensureLive(id);
    // Mirror the server's run-detail read (PRD #37 M4-fix): own_agents is resolved
    // here from the owner's templates (lead stripped), so the plan gate's "My agent
    // templates" card has chips in mock mode without a separate fetch.
    const own_agents = templates
      .filter((t) => !LEAD_NAME_RE.test(t.name))
      .map((t) => ({ name: t.name, description: t.description }));
    return delay({ run: { ...run, own_agents } }, 60);
  },
  // PRD #35: flip this run's usage-limit opt-in. Mirrors the server's guard — the
  // same NEGATIVE predicate the cancel path uses — so a terminal run is refused and
  // `limit_wait` is admitted for free.
  //
  // 🔴 IT MUST NOT TOUCH `status`. A parked run stays parked with its clock intact;
  // this changes what happens at the NEXT limit. A mock that helpfully un-parked the
  // run would teach the demo (and anyone testing against it) the one wrong thing
  // about this control.
  setRunWaitOnLimit: async (id: string, enabled: boolean) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (isTerminalRun(run.status)) throw new ApiError(409, "this run has already finished");
    patchRun(id, { wait_on_limit: enabled });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // PRD #841: set (or clear) a run's per-run MR-review-rework override. Mirrors the
  // server: owner-scoped (the demo caller owns every non-other-user run) and — unlike
  // setRunWaitOnLimit — NO terminal-status guard (D2), because the watcher acts after
  // the run completes, so the toggle stays live on a completed run whose MR is still
  // open. `null` clears back to inherit.
  setRunMrRework: async (id: string, enabled: boolean | null) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    patchRun(id, { mr_rework_enabled: enabled });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // Issue #754: resume an auto-lane run parked at `pool_wait` right now. Mirrors the
  // server: owner-scoped (the demo caller owns every non-other-user run) and
  // pool_wait-ONLY — a 409 ("run is not waiting for a pooled token") on any other
  // status, including a run already resumed to `queued` (so a second click 409s, the
  // idempotent-ish contract the panel's inline "no longer waiting" note relies on).
  // On success the run moves to `queued`, which is what un-parks it.
  resumeRunNow: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.status !== "pool_wait")
      throw new ApiError(409, "run is not waiting for a pooled token");
    patchRun(id, { status: "queued", updated_at: new Date().toISOString() });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // PRD #320 M6: bump this run to the front of the queue, or clear that override.
  // Mirrors the server: owner-scoped (the demo caller owns every non-other-user run)
  // and QUEUED-ONLY (409 on a non-queued run, exactly like the real endpoint). Clearing
  // the override returns the run to its NATURAL class — "background" for the kinds that
  // demote (judge/self_improve), "normal" otherwise — since the mock has no live rank
  // machinery; the "restored" grace state is a seed, not something undo produces here.
  expediteRun: async (id: string, expedite: boolean) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.status !== "queued") throw new ApiError(409, "run is not queued");
    const natural: RunPriority =
      run.kind === "self_improve" || run.kind === "judge" ? "background" : "normal";
    patchRun(id, { priority: expedite ? "expedited" : natural });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // ── Run judge review (PRD #46 M4, PRD #119) ────────────────────────────────
  // The two-key envelope the server emits: BOTH keys always present, either nullable
  // and independent of the other. A pending judge over no review is the auto-judge
  // case; a pending judge over a review is a re-judge in flight.
  getRunReview: async (id: string) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === id);
    const pending = pendingJudges[id];
    return delay(
      {
        review: review ? reviewDTO(review) : null,
        pending_judge: pending ? { ...pending } : null,
      },
      60,
    );
  },

  // ── Triage a recommendation (PRD #94) ──────────────────────────────────────
  // Owner-only local upserts on the coordinate — no token spend, no forge write.
  // The panel refetches the review after each, so triage/stale re-read via reviewDTO.
  setDisposition: async (
    runId: string,
    recId: string,
    status: "done" | "dismissed",
    reason?: "wont_do" | "not_an_issue",
  ) => {
    requireSession();
    if (!getRun(runId)) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === runId);
    const rec = review?.recommendations.find((x) => x.id === recId);
    if (!review || !rec) throw new ApiError(404, "recommendation not found");
    // Enum validation, mirroring the server: reason required iff dismissed.
    if (status === "dismissed") {
      if (reason !== "wont_do" && reason !== "not_an_issue") {
        throw new ApiError(400, "reason is required for a dismissal (wont_do | not_an_issue)");
      }
    } else if (status === "done") {
      if (reason !== undefined) throw new ApiError(400, "reason must be omitted for a done disposition");
    } else {
      throw new ApiError(400, "status must be 'done' or 'dismissed'");
    }
    // Idempotent upsert on the coordinate; a set re-stamps set_at and clears stale.
    //
    // set_via is EXPLICITLY cleared, and the explicitness is the point (PRD #98 Decision 6,
    // mirroring dispositions.sql's literal NULL). This is a HUMAN write, so the provenance
    // must stop saying "the system inferred it": overriding an issue-close auto-done has to
    // read "✓ Done", not "Done via #91". Object.assign copies only the keys `next` HAS, so
    // omitting this would leave a stale set_via on the existing row and the mock would demo
    // exactly the misattribution the server's literal NULL exists to prevent.
    const next: Disposition & { set_via?: "issue_close" | "denied_cli" } = {
      category: rec.category,
      target: rec.target,
      status,
      reason: status === "dismissed" ? (reason as "wont_do" | "not_an_issue") : "",
      set_at: new Date().toISOString(),
      stale: false,
      set_via: undefined,
    };
    const existing = review.dispositions.find((d) => d.category === rec.category && d.target === rec.target);
    if (existing) Object.assign(existing, next);
    else review.dispositions.push(next);
    return delay(null, 120); // 204 No Content
  },
  deleteDisposition: async (runId: string, recId: string) => {
    requireSession();
    if (!getRun(runId)) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === runId);
    const rec = review?.recommendations.find((x) => x.id === recId);
    if (!review || !rec) throw new ApiError(404, "recommendation not found");
    // Idempotent: dropping an absent coordinate is a no-op success — mirrors the
    // client's soft-404 handling, so Undo never surfaces a loud error in the demo.
    review.dispositions = review.dispositions.filter(
      (d) => !(d.category === rec.category && d.target === rec.target),
    );
    return delay(null, 120);
  },
  getJudgeStats: async () => {
    requireSession();
    // Canonical aggregate over every review the caller owns (the same tally the Judge
    // page's tabs and the nav badge read — see computeTriage).
    return delay(computeTriage(), 60);
  },
  getJudgeCategoryStats: async (run?: string) => {
    requireSession();
    // Canonical per-category GROUP count MATRIX over every review the caller owns — bucket →
    // category → group count, uncapped and anchor-aware, tab-scoped and triage-variant (see
    // computeCategoryStats). `run` is the deep-link anchor, threaded through the same semi-join
    // the backlog uses.
    return delay(computeCategoryStats(run ?? ""), 60);
  },

  // ── Judge menu — cross-run backlog + bulk disposition (PRD #98 M3) ───────────
  getJudgeBacklog: async (bucket: JudgeBacklogBucket = "todo", run?: string, categories?: string[]) => {
    requireSession();
    return delay(computeBacklog(bucket, run ?? "", categories ?? []), 80);
  },
  bulkSetJudgeDisposition: async (
    items: JudgeDispositionCoord[],
    status: "done" | "dismissed",
    reason?: "wont_do" | "not_an_issue",
    scope: JudgeDispositionScope = "open",
  ) => {
    requireSession();
    if (items.length === 0) throw new ApiError(400, "items required");
    if (status === "dismissed") {
      if (reason !== "wont_do" && reason !== "not_an_issue") {
        throw new ApiError(400, "invalid status or reason");
      }
    } else if (status === "done") {
      if (reason !== undefined) throw new ApiError(400, "invalid status or reason");
    } else {
      throw new ApiError(400, "invalid status or reason");
    }
    if (scope !== "open" && scope !== "all") throw new ApiError(400, "invalid scope");
    // Dedup coordinates before the cap check (the cap counts distinct work).
    const want = new Map<string, JudgeDispositionCoord>();
    for (const it of items) want.set(coordKey(it.category, it.target), it);
    if (want.size > 100) throw new ApiError(400, "too many items");

    // Fan out: for every review, upsert a disposition on each member coordinate that the
    // request names and the scope selects (scope=open → only members the ladder buckets as
    // todo). `updated` counts distinct (review_id, category, target) TRIPLES actually
    // written — so it can be LOWER than the recommendations a group spans.
    //
    // `settled` collects an undo ADDRESS per written triple, from the same pass that decides
    // membership — the mock must model this faithfully, because the whole point of the field
    // is that it can DIFFER from what a client would compute: a member the scope skips must
    // not appear here (PRD #98 review BLK-UNDO).
    const writtenTriples = new Set<string>();
    const settled: JudgeSettledMember[] = [];
    for (const review of reviews) {
      for (const rec of review.recommendations) {
        const key = coordKey(rec.category, rec.target);
        if (!want.has(key)) continue;
        if (scope === "open" && bucketOfRec(review, rec.category, rec.target) !== "todo") continue;
        if (!writtenTriples.has(`${review.id} ${key}`)) {
          settled.push({ run_id: review.target_run_id, rec_id: rec.id });
        }
        // set_via explicitly cleared: a bulk group action is a HUMAN write too, so it must
        // drop any issue-close provenance rather than inherit it (see the single-coordinate
        // path above for why Object.assign makes the omission a live bug, not a tidiness nit).
        const next: Disposition & { set_via?: "issue_close" | "denied_cli" } = {
          category: rec.category,
          target: rec.target,
          status,
          reason: status === "dismissed" ? (reason as "wont_do" | "not_an_issue") : "",
          set_at: new Date().toISOString(),
          stale: false,
          set_via: undefined,
        };
        const existing = review.dispositions.find((d) => d.category === rec.category && d.target === rec.target);
        if (existing) Object.assign(existing, next);
        else review.dispositions.push(next);
        writtenTriples.add(`${review.id} ${key}`);
      }
    }

    // Re-read at bucket=all (so a just-dismissed group still returns with its new rollup),
    // narrowed to the acted-on coordinates — the shape the page re-renders rows from.
    const backlog = computeBacklog("all", "");
    const acted = new Set(want.keys());
    const groups = backlog.groups.filter((g) => acted.has(coordKey(g.category, g.target)));
    const result: JudgeDispositionResult = {
      updated: writtenTriples.size,
      settled,
      groups,
      // Carried through from the re-read, exactly as BulkSetDispositions does
      // (judge_bulk_disposition.go: `Truncated: backlog.Truncated`). It is NOT independently
      // computed here: the re-read is bounded by the same cap, so a second source for this
      // flag would be a second implementation of the cut, and the two could disagree about
      // the very response they are describing.
      truncated: backlog.truncated,
      triage: backlog.triage,
    };
    return delay(result, 120);
  },

  // ── Incidental Findings backlog (PRD #333 M7) ───────────────────────────────
  listFindings: async (bucket: IncidentalFindingBucket = "to_file", repo?: string, run?: string) => {
    const me = requireSession();
    const mine = findings.filter((f) => f.user_id === me.id);
    const byRepo = repo ? mine.filter((f) => f.repo_id === repo) : mine;
    // open_count is the D8 nav-badge count: open coordinates scoped by the ?repo= filter but NOT
    // the bucket or run — so it is stable across a tab switch, exactly like the server meta.
    const openCount = byRepo.filter((f) => f.status === "open").length;
    const byRun = run ? byRepo.filter((f) => f.run_ids.includes(run)) : byRepo;
    const rows = byRun.filter((f) => matchFindingBucket(f.status, bucket)).map(findingDTO);
    const backlog: IncidentalFindingBacklog = {
      bucket,
      repo: repo ?? "",
      run: run ?? "",
      open_count: openCount,
      findings: rows,
    };
    return delay(backlog, 80);
  },
  findingIssueDraft: async (id: string) => {
    const me = requireSession();
    const f = findings.find((x) => x.finding_id === id && x.user_id === me.id);
    if (!f) throw new ApiError(404, "finding not found");
    const draft: IncidentalFindingIssueDraft = {
      title: f.last_title,
      description: f.description_md,
      location: f.location,
      labels: [...f.labels],
      provenance: `Found by a run while working on ${f.repo_path}.`,
    };
    return delay(draft, 80);
  },
  fileFinding: async (id: string, body?: { title?: string; description?: string; labels?: string[] }) => {
    const me = requireSession();
    const f = findings.find((x) => x.finding_id === id && x.user_id === me.id);
    if (!f) throw new ApiError(404, "finding not found");
    // Claim-first: only an `open` coordinate can be filed — a second file is the 409 the stale
    // card / stale backlog row handles gracefully (the guarded claim, D4).
    if (f.status !== "open") throw new ApiError(409, "this finding is already filed or being filed");
    const iid = 900 + (parseInt(id.replace(/\D/g, ""), 10) || 0);
    f.status = "filed";
    f.filed_issue_iid = iid;
    f.resolved_at = new Date().toISOString();
    const res: IncidentalFindingFileResult = {
      issue: {
        iid,
        web_url: `https://gitlab.example.com/${f.repo_path}/-/issues/${iid}`,
        title: body?.title ?? f.last_title,
      },
    };
    return delay(res, 120);
  },
  dismissFinding: async (id: string, reason: "wont_do" | "not_an_issue") => {
    const me = requireSession();
    if (reason !== "wont_do" && reason !== "not_an_issue") {
      throw new ApiError(400, "reason must be wont_do or not_an_issue");
    }
    const f = findings.find((x) => x.finding_id === id && x.user_id === me.id);
    if (!f) throw new ApiError(404, "finding not found");
    if (f.status !== "open") {
      throw new ApiError(409, "cannot dismiss (already filed, being filed, or already dismissed)");
    }
    f.status = "dismissed";
    f.resolved_at = new Date().toISOString();
    return delay({ status: "dismissed", reason }, 80);
  },

  // ── File a forge issue from a recommendation (PRD #68 M4 preview) ────────────
  getIssueDraft: async (runId: string, recId: string) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === runId);
    const rec = review?.recommendations.find((x) => x.id === recId);
    if (!review || !rec) throw new ApiError(404, "recommendation not found");
    return delay({ draft: mockIssueDraft(runId, rec, review) }, 80);
  },
  fileIssue: async (
    runId: string,
    recId: string,
    body: { repo_id: string; title: string; description: string },
  ) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === runId);
    const rec = review?.recommendations.find((x) => x.id === recId);
    if (!review || !rec) throw new ApiError(404, "recommendation not found");
    if (review.filed_issues.some((f) => f.category === rec.category && f.target === rec.target)) {
      throw new ApiError(409, "this recommendation already has an issue, or one is being filed");
    }
    const repo = repos.find((r) => r.id === body.repo_id);
    if (!repo) throw new ApiError(404, "repo not found");
    // Demo hook for mock state E (forge rejected): filing against the atlas repo, which
    // the demo treats as write-protected, surfaces the draft-stays-open error path.
    if (repo.path_with_namespace.includes("atlas")) {
      throw new ApiError(502, "could not create the issue on the forge: the forge rejected the request (403)");
    }
    const iid = nextFiledIssueIid++;
    const web_url = `${repo.web_url}/-/issues/${iid}`;
    // Persist the link so a reload shows the filed row (mock C), just like the real API.
    review.filed_issues.push({
      category: rec.category,
      target: rec.target,
      issue_iid: iid,
      issue_url: web_url,
      filed_at: new Date().toISOString(),
    });
    const issue: CreatedIssue = { iid, web_url, title: body.title };
    return delay({ issue }, 200);
  },
  rerunJudge: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.status !== "completed" && run.status !== "failed") {
      throw new ApiError(422, "this run cannot be judged");
    }
    if (run.kind !== "issue" && run.kind !== "ci_fix") {
      throw new ApiError(422, "this run cannot be judged");
    }
    // Server parity with uq_runs_one_active_judge_per_target (PRD #119): a target that
    // already has an active judge cannot get a second one, and the real API answers
    // that with a 409 carrying exactly this message. Keeping it here is what makes the
    // panel's TOCTOU backstop (409 → re-fetch → converge to pending, no error banner)
    // exercisable in mock mode instead of only against a live database.
    if (pendingJudges[id]) {
      throw new ApiError(409, "a judge run is already in progress for this run");
    }
    // A mock judge run: no worker executes it, so the seeded review is unchanged —
    // the panel just shows the in-flight note. Cloning the target run yields a
    // valid Run shape for the envelope.
    const judge: Run = { ...run, id: nextRunId(), kind: "judge", status: "queued" };
    // Register it as the target's ACTIVE judge, mirroring the row the real POST inserts
    // (`queued` → the DTO's `scheduled`). Without this the mock told two lies at once:
    // the next getRunReview kept answering pending_judge:null, so a re-run left the
    // button disabled by the local flag but still LABELLED "Re-run judge" where the real
    // server flips it to "Judge scheduled" on the very next poll; and nothing ever
    // populated pendingJudges from the UI, so the 409 branch below — the panel's TOCTOU
    // absorb path — was unreachable in mock mode and could not be demoed at all.
    // The demo consequence is that the button stays disabled until a reload re-seeds the
    // module. That is FAITHFUL: a real judge holds the target's active slot until it
    // reaches a terminal status, and no mock worker will ever finish this one.
    pendingJudges[id] = { state: "scheduled", enqueued_at: new Date().toISOString() };
    return delay({ run: judge }, 120);
  },
  getRunMessages: async (id: string, afterSeq = 0) => {
    const log = state.messages.get(id);
    if (!log) throw new ApiError(404, "run not found");
    return delay({ messages: log.filter((m) => m.seq > afterSeq).map((m) => ({ ...m })) }, 60);
  },
  // PRD #95 steer queue (M2 seeds demo data across delivery states so M3's
  // SteerQueueCard renders every chip). A run with no sample inputs returns an empty
  // queue; a missing run 404s (which the card treats as "no queue", never an error).
  getRunInputs: async (id: string) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    const inputs = (mockRunInputs[id] ?? []).map((i) => ({ ...i }));
    return delay({ inputs }, 60);
  },
  submitRunInput: async (
    id: string,
    kind: RunInputKind,
    body = "",
    selection?: AgentSelectionInput,
    overrideCapabilities?: boolean,
  ) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    // PRD #88: the engine returns the refusals the real api answers with (a 409 for an
    // answer to a question that has moved on, a 400 for a malformed body) rather than
    // resolving 200 over a no-op. A mock that swallows a refusal is how a surface ends up
    // built against a laxer contract than the one that ships.
    const rejection = handleInput(id, kind, body);
    if (rejection) throw new ApiError(rejection.status, rejection.message);
    // PRD #37: mirror the selection onto the run row so the mock's read-only
    // post-approval view has something to show.
    if (kind === "approve_plan" && selection) {
      patchRun(id, { agent_source: selection.source, agent_exclusions: selection.exclusions });
    }
    // PRD #84 M4 4c/4d: the "run without the capability" override clears the run's inferred
    // required_capabilities before approving, mirroring the server (the false-positive
    // correction). required_tools/size_class are display-only and untouched.
    if (kind === "approve_plan" && overrideCapabilities) {
      patchRun(id, { required_capabilities: [] });
    }
    return delay({ server_side: false }, 150);
  },

  adminListWorkers: async () => delay({ workers: mockAdminWorkers.map((w) => ({ ...w })) }),
  // PRD #66 M9 (D8): the admin cross-user blocked-repos list. DERIVED from the shared,
  // mutable repos state (not a static deep-copy) joined with mockBlockedRepoMeta for the
  // owner/reasons the wire Repo doesn't carry — so an Allow or Revoke on that shared
  // state (setRepoGuardrailOverride / clearRepoGuardrailOverride) round-trips into this
  // list, and the repo an admin allows here actually exists in the mutation's target.
  // block_messages is never null (always []), matching the server contract. Typed as
  // the AdminBlockedRepos envelope — the mock is a second implementation of that wire
  // contract, so the annotation keeps the two shapes in lockstep.
  adminListBlockedRepos: async (): Promise<AdminBlockedRepos> =>
    delay({
      checks_unknown: false,
      repos: repos
        .filter((r) => r.guardrail_blocked || r.guardrail_override)
        .map((r) => {
          const meta = mockBlockedRepoMeta[r.id];
          return {
            id: r.id,
            path: r.path_with_namespace,
            owner_id: meta?.owner_id ?? mockAdmin.id,
            owner_email: meta?.owner_email ?? mockAdmin.email,
            forge_type: meta?.forge_type ?? mockConnection.forge_type,
            blocked: r.guardrail_blocked,
            // Emit the underlying reasons only while the repo is actually blocked; an
            // overridden-clean repo lists [] (the override downgraded them). Never null.
            block_messages: r.guardrail_blocked ? [...(meta?.block_messages ?? [])] : [],
            guardrail_override: r.guardrail_override ? { ...r.guardrail_override } : null,
            privilege_status: meta?.privilege_status ?? mockConnection.privilege_status,
            privilege_checked_at: meta?.privilege_checked_at ?? mockConnection.privilege_checked_at,
          };
        }),
    }),
  adminListRuns: async () =>
    delay({
      runs: listRunsFor()
        .filter((r) => r.kind !== "chat")
        .filter((r) => !["completed", "failed", "cancelled"].includes(r.status))
        // Owner attribution: the mock's owner column is mockOtherRunOwners; every
        // other run belongs to the session admin. Before this map existed, EVERY
        // row here was stamped with the session email — the demo factory list was
        // 100% "mine", the exact duplication amendment 2 removes.
        .map((r) => runListItem(r, mockOtherRunOwners[r.id] ?? requireSession().email)),
    }),

  // ── Chat (PRD #39) — real M1 wire ─────────────────────────────────────────
  listChats: async () =>
    delay({
      chats: [...state.runs.values()].filter((r) => r.kind === "chat").map((r) => chatDTO(r)),
      max_turns: CHAT_MAX_TURNS, // the envelope constant, not per-chat
    }),
  createChat: async (message: string) => {
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: null,
      forge_type: "",
      mr_web_url: null,
      issue_web_url: null,
      kind: "chat",
      issue_iid: null,
      issue_title: truncateChatTitle(message),
      issue_description: "",
      title: truncateChatTitle(message),
      resume_of_run_id: null,
      status: "running",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: "w-laptop",
      branch: null,
      model: null,
      override_subagent_model: false,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      stop_reason: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      pipeline_ref: null,
      pipeline_web_url: null,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      // PRD #122: a freshly synthesised run has no milestones yet — all null, so it
      // renders on the null-fallback path (the iteration badge, no checklist).
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      budget_max_iterations: null,
      budget_wall_seconds: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_select_reason: null,
      anthropic_headroom_pct: null,
      wait_on_limit: false,
      limit_resets_at: null,
      retry_not_before: null,
      limit_wait_count: 0,
      rate_limit_type: null,
      claimed_at: now,
      started_at: now,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    state.messages.set(run.id, []);
    appendMessage(run.id, "user_message", null, { text: message });
    scheduleChatReply(run.id, message);
    return delay({ run: { ...run } }, 300);
  },
  sendChatMessage: async (id: string, message: string) => {
    const run = getRun(id);
    if (!run || run.kind !== "chat") throw new ApiError(404, "chat not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "this conversation has ended");
    }
    appendMessage(id, "user_message", null, { text: message });
    scheduleChatReply(id, message);
    return delay({ server_side: false }, 150);
  },
  endChat: async (id: string) => {
    const run = getRun(id);
    if (!run || run.kind !== "chat") throw new ApiError(404, "chat not found");
    patchRun(id, { status: "completed", finished_at: new Date().toISOString() });
    return delay({ server_side: false }, 200);
  },
  continueChat: async (id: string) => {
    const src = getRun(id);
    if (!src || src.kind !== "chat") throw new ApiError(404, "chat not found");
    const now = new Date().toISOString();
    const run: Run = {
      ...src,
      id: nextRunId(),
      status: "running",
      resume_of_run_id: id,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    state.messages.set(run.id, []);
    appendMessage(run.id, "status", null, { text: "continuing the conversation on your worker" });
    return delay({ run: { ...run } }, 250);
  },
  confirmProposal: async (chatId: string, proposalId: string) => {
    const p = getProposal(proposalId);
    if (!p || p.run_id !== chatId) throw new ApiError(404, "proposal not found");
    if (p.status !== "pending") throw new ApiError(409, "proposal already resolved");
    // Mark resolved (a re-confirm 409s); the confirm response is the created issue.
    putProposal({ ...p, status: "confirmed" });
    const iid = 200 + Math.floor(Math.random() * 800);
    const issue: CreatedIssue = {
      iid,
      web_url: `https://gitlab.example.com/${p.repo_path ?? "grp/proj"}/-/issues/${iid}`,
      title: p.title,
    };
    // The created-issue link is appended to the conversation (Decision 8).
    appendMessage(chatId, "text", "chat", { text: `Created issue #${iid}: ${issue.web_url}` });
    return delay({ issue }, 350);
  },
  dismissProposal: async (chatId: string, proposalId: string) => {
    const p = getProposal(proposalId);
    if (!p || p.run_id !== chatId) throw new ApiError(404, "proposal not found");
    putProposal({ ...p, status: "dismissed" });
    appendMessage(chatId, "status", null, { text: "proposal dismissed — nothing written to the forge" });
    return delay(null, 200); // 204 No Content
  },
  startRunFromChat: async (repoPath: string, _issueIid: number) => {
    // PRD #191 M5: start a run from a chat's start-run card. Repo paths aren't modelled
    // in the mock state, so it resolves the first seeded board+card and mints a queued
    // issue run via the same path as createRun; the real endpoint applies the
    // PRD/ownership gate keyed by repo_path.
    const repoId = [...state.boards.keys()][0];
    const card = repoId ? state.boards.get(repoId)?.cards[0] : undefined;
    if (!repoId || !card) throw new ApiError(404, `repo ${repoPath} not found`);
    return mockApi.createRun(repoId, card.iid);
  },

  // PRD #322 M1: cancel a run from a chat's cancel card. run_id is untrusted; the real
  // endpoint re-resolves ownership/terminality server-side via SubmitInput(cancel), so
  // the mock reproduces its refusals — a missing run is 404, an already-terminal one 409
  // — rather than resolving 202 over a no-op.
  cancelRunFromChat: async (runId: string) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "run has already finished");
    }
    handleInput(runId, "cancel", "");
    return delay({ server_side: true }, 150);
  },

  // PRD #322 M3: steer a run from a chat's steer card with a human-edited follow-up.
  // run_id + message are untrusted; the real endpoint re-resolves ownership/terminality
  // via SubmitInput(follow_up), which additionally refuses a CHAT run (issue-runs-only),
  // so the mock reproduces its refusals — a missing run is 404, a terminal one 409, and a
  // chat-run target 409 — a follow_up on an issue run succeeds.
  steerRunFromChat: async (runId: string, message: string) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "run has already finished");
    }
    if (run.kind === "chat") {
      throw new ApiError(409, "steering applies to issue runs, not chats");
    }
    handleInput(runId, "follow_up", message);
    return delay({ server_side: true }, 150);
  },

  // ── CLI tokens (PRD #64 M6) ────────────────────────────────────────────────
  // Mirrors the cookie-only CRUD: list carries no value, mint returns the
  // plaintext once, admin_ro is admin-only, revoked rows stay (the incident
  // trail), and revoke-all is idempotent.
  listCliTokens: async () => {
    const me = requireSession();
    // Only the caller's own tokens — the real endpoint filters `WHERE user_id=$1`,
    // so as a non-admin persona you must not see the admin's tokens.
    return delay({ tokens: cliTokens.filter((t) => t.user_id === me.id).map(stripOwner) });
  },
  createCliToken: async (name: string, scope: CliTokenScope) => {
    const me = requireSession();
    const trimmed = name.trim();
    if (trimmed === "") throw new ApiError(400, "name must be non-empty and at most 200 characters");
    if (scope !== "user" && scope !== "admin_ro") throw new ApiError(400, "scope must be 'user' or 'admin_ro'");
    if (scope === "admin_ro" && !me.is_admin) {
      throw new ApiError(403, "admin access required to mint an admin-scoped token");
    }
    const cls = scope === "admin_ro" ? "uza" : "uzc";
    const body = Array.from(crypto.getRandomValues(new Uint8Array(24)), (b) => b.toString(16).padStart(2, "0")).join("");
    const now = new Date().toISOString();
    const row: OwnedCliToken = {
      id: `cli-new-${++cliTokenCounter}`,
      user_id: me.id,
      name: trimmed,
      token_prefix: `${cls}_${body.slice(0, 4)}`,
      scope,
      revoked: false,
      created_at: now,
      last_used_at: null,
      last_used_ip: null,
      // Expiry matrix (static mint path): a user token never expires; an admin_ro
      // token is bounded to 90 days.
      expires_at: scope === "admin_ro" ? new Date(Date.now() + 90 * 86_400_000).toISOString() : null,
    };
    cliTokens = [row, ...cliTokens];
    return delay({ token: `${cls}_${body}`, cli_token: stripOwner(row) }, 200);
  },
  revokeCliToken: async (id: string) => {
    const me = requireSession();
    // Owner-scoped: a foreign id is a 404, exactly like the server.
    const t = cliTokens.find((x) => x.id === id && x.user_id === me.id && !x.revoked);
    if (!t) throw new ApiError(404, "token not found");
    t.revoked = true;
    return delay(null);
  },
  revokeAllCliTokens: async () => {
    const me = requireSession();
    // Only the caller's tokens, mirroring `WHERE user_id=$1`.
    cliTokens = cliTokens.map((t) => (t.user_id === me.id ? { ...t, revoked: true } : t));
    return delay(null);
  },

  // ── CLI browser-login consent flow (PRD #64 M6) ────────────────────────────
  // getCliAuthRequest never returns the user_code (the human types it from their
  // terminal — the anti-phishing property). approve validates the typed code.
  getCliAuthRequest: async (id: string) => {
    requireSession();
    const req = cliAuthRequests.get(id);
    if (!req) throw new ApiError(404, "request not found");
    const status =
      req.status === "pending" && Date.parse(req.expires_at) <= Date.now() ? "expired" : req.status;
    return delay({ client_desc: req.client_desc, status, expires_at: req.expires_at });
  },
  approveCliAuth: async (requestId: string, userCode: string, scope: CliTokenScope) => {
    const me = requireSession();
    if (scope !== "user" && scope !== "admin_ro") throw new ApiError(400, "scope must be 'user' or 'admin_ro'");
    if (scope === "admin_ro" && !me.is_admin) {
      throw new ApiError(403, "admin access required to approve an admin-scoped login");
    }
    const req = cliAuthRequests.get(requestId);
    if (!req) throw new ApiError(404, "request not found");
    if (Date.parse(req.expires_at) <= Date.now()) throw new ApiError(410, "request expired");
    if (req.status !== "pending") throw new ApiError(409, "request is no longer pending");
    if (normalizeMockUserCode(userCode) !== req.user_code) {
      throw new ApiError(400, "the code you entered does not match");
    }
    req.status = "approved";
    return delay({ status: "approved" });
  },
  denyCliAuth: async (requestId: string) => {
    requireSession();
    const req = cliAuthRequests.get(requestId);
    if (!req) throw new ApiError(404, "request not found");
    if (req.status !== "pending" || Date.parse(req.expires_at) <= Date.now()) {
      throw new ApiError(409, "request is no longer pending");
    }
    req.status = "denied";
    return delay({ status: "denied" });
  },

  // ── Agent memory (PRD #90 M6) ──────────────────────────────────────────────
  // list is owner-scoped + newest-first (the real endpoint filters `WHERE
  // user_id=$1 ORDER BY created_at DESC`); delete is an owner-scoped purge.
  listMemory: async () => {
    const me = requireSession();
    const mine = memories
      .filter((m) => m.user_id === me.id)
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
      .map(stripMemoryOwner);
    return delay({ memories: mine });
  },
  deleteMemory: async (id: string) => {
    const me = requireSession();
    // Owner-scoped: a foreign id is a 404, exactly like the server.
    const m = memories.find((x) => x.id === id && x.user_id === me.id);
    if (!m) throw new ApiError(404, "memory not found");
    memories = memories.filter((x) => x.id !== id);
    return delay(null);
  },

  // ── Scheduled runs (PRD #241) ──────────────────────────────────────────────
  listSchedules: async () => {
    requireSession();
    return delay(schedules.map(scheduleDTO));
  },
  createSchedule: async (repoId: string, input: ScheduleInput) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    assertGuidanceWithinCap(input.guidance);
    const target = input.target ?? "issue";
    const timing = input.timing ?? "recurring";
    const now = new Date().toISOString();
    const s: Schedule = {
      id: nextScheduleId(),
      repo_id: repoId,
      repo_path: repo.path_with_namespace,
      target,
      issue_iid: target === "issue" ? (input.issue_iid ?? null) : null,
      labels: target === "sweep" && input.labels && input.labels.length ? input.labels : null,
      prompt: target === "prompt" ? (input.prompt ?? "") : "",
      timing,
      cron_expr: timing === "recurring" ? (input.cron_expr ?? "") : "",
      run_at: timing === "once" ? (input.run_at ?? null) : null,
      timezone: input.timezone || "UTC",
      next_fire_at: null,
      last_fired_at: null,
      last_fire: null,
      auto_approve: input.auto_approve ?? true,
      wait_on_limit: input.wait_on_limit ?? true,
      // Sweep-only; new sweeps default to 10 (mirrors the server), unlimited otherwise.
      max_issues: target === "sweep" ? (input.max_issues ?? 10) : null,
      // Guidance on issue/sweep only; null (none) for prompt (re-nulled per target).
      guidance: target === "issue" || target === "sweep" ? (input.guidance ?? null) : null,
      // Baked catalog guidance is a default-sweep-only field (issue #675); null for a user row.
      baked_guidance: null,
      // Model applies to ALL targets (unlike guidance); null = inherit.
      model: input.model ?? null,
      // PRD #305: omitted ≡ false (replace-semantics), default off.
      override_subagent_model: input.override_subagent_model ?? false,
      enabled: input.enabled ?? true,
      status: "active",
      // A create always makes a user-authored row (PRD #589): defaults are born only
      // via enableCatalogSchedule, and a clone via cloneSchedule.
      origin: "user",
      catalog_slug: null,
      customized: false,
      // A bare create sends no group id (standalone). Multi-repo fan-out (PRD #636 M2)
      // stamps a shared id via the create input; a single-repo create stays NULL here.
      sibling_group_id: null,
      created_at: now,
      updated_at: now,
      next_fires: [],
    };
    schedules = [s, ...schedules];
    return delay(scheduleDTO(s), 250);
  },
  getSchedule: async (id: string) => {
    requireSession();
    const s = schedules.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "schedule not found");
    return delay(scheduleDTO(s));
  },
  updateSchedule: async (id: string, input: ScheduleInput) => {
    requireSession();
    const cur = schedules.find((x) => x.id === id);
    if (!cur) throw new ApiError(404, "schedule not found");
    // A catalog default is catalog-owned (PRD #589): the server's patchDefaultScheduleConfig
    // 400s ANY default patch whose body carries a catalog-owned field. Mirror that here so
    // the mock and the server agree — the drift that hid the buildDefaultInput `timing` bug.
    // The rejected set is target/prompt/labels/timing/repo_id/issue_iid (run_at too, but the
    // modal never sends it on a default). Guidance is the exception for a PROMPT default
    // (issue #662) and a SWEEP default (issue #675): it is owner-editable there (overlaid on
    // the catalog prompt/guidance at fire time), so allow it for prompt+sweep defaults and
    // keep rejecting it for issue/self_improve defaults. The 400 message mirrors the server's
    // per-target locked-set string. Only cron/tz/model/flags/max_issues (+prompt/sweep-default
    // guidance) edit.
    if (cur.origin === "default") {
      const guidanceEditable = cur.target === "prompt" || cur.target === "sweep";
      if (
        input.target !== undefined ||
        input.prompt !== undefined ||
        input.labels !== undefined ||
        (!guidanceEditable && input.guidance !== undefined) ||
        input.timing !== undefined ||
        (input.repo_id !== undefined && input.repo_id !== "") ||
        input.issue_iid !== undefined ||
        input.run_at !== undefined
      ) {
        throw new ApiError(
          400,
          guidanceEditable
            ? "a default schedule's prompt, labels, target, timing and repo are catalog-owned and cannot be edited"
            : "a default schedule's prompt, labels, guidance, target, timing and repo are catalog-owned and cannot be edited",
        );
      }
    }
    assertGuidanceWithinCap(input.guidance);
    const m: Schedule = { ...cur };
    if (input.target !== undefined) m.target = input.target;
    if (input.timing !== undefined) m.timing = input.timing;
    if (input.issue_iid !== undefined) m.issue_iid = input.issue_iid;
    if (input.labels !== undefined) m.labels = input.labels.length ? input.labels : null;
    if (input.prompt !== undefined) m.prompt = input.prompt;
    if (input.cron_expr !== undefined) m.cron_expr = input.cron_expr;
    if (input.run_at !== undefined) m.run_at = input.run_at;
    if (input.timezone !== undefined) m.timezone = input.timezone;
    if (input.auto_approve !== undefined) m.auto_approve = input.auto_approve;
    if (input.wait_on_limit !== undefined) m.wait_on_limit = input.wait_on_limit;
    // Replace-semantics: apply when the key is present (explicit null = unlimited).
    if (input.max_issues !== undefined) m.max_issues = input.max_issues;
    // Same replace-semantics for guidance (explicit null/"" clears to none).
    if (input.guidance !== undefined) m.guidance = input.guidance;
    // Model applies to all targets, so it is not re-nulled per target below.
    if (input.model !== undefined) m.model = input.model;
    // PRD #305: replace-semantics, applied when the key is present.
    if (input.override_subagent_model !== undefined)
      m.override_subagent_model = input.override_subagent_model;
    if (input.enabled !== undefined) m.enabled = input.enabled;
    // PRD #344 Feature A: a non-empty repo_id that differs from the current one repoints the
    // schedule. Mirror the server: an issue-target schedule is rejected (422, D4 restrict);
    // an unknown/unowned repo is a 404; otherwise move repo_id and refresh repo_path.
    if (input.repo_id !== undefined && input.repo_id !== "" && input.repo_id !== cur.repo_id) {
      if (m.target === "issue")
        throw new ApiError(422, "repointing an issue-target schedule is not supported; delete and recreate it against the new repo");
      const repo = repos.find((r) => r.id === input.repo_id);
      if (!repo) throw new ApiError(404, "repo not found");
      m.repo_id = repo.id;
      m.repo_path = repo.path_with_namespace;
    }
    // Re-null the fields the (possibly changed) target/timing does not use, so the
    // stored shape matches the DB's field-presence CHECK.
    m.issue_iid = m.target === "issue" ? m.issue_iid : null;
    m.labels = m.target === "sweep" ? m.labels : null;
    m.max_issues = m.target === "sweep" ? m.max_issues : null;
    // A prompt DEFAULT keeps its owner guidance (issue #662); a USER prompt schedule still
    // nulls it (guidance is issue/sweep-only for user rows, catalog-owned/baked otherwise).
    m.guidance =
      m.target === "issue" || m.target === "sweep" || (m.origin === "default" && m.target === "prompt")
        ? m.guidance
        : null;
    m.prompt = m.target === "prompt" ? m.prompt : "";
    m.cron_expr = m.timing === "recurring" ? m.cron_expr : "";
    m.run_at = m.timing === "once" ? m.run_at : null;
    // A default row that edits an editable field away from its catalog values reads as
    // "customized" (PRD #589 M2), which lights the indicator and Reset. Only the enable
    // pause-flip leaves it untouched (a bare { enabled } PATCH changes nothing else).
    if (m.origin === "default" && m.catalog_slug) {
      const entry = catalogBySlug(m.catalog_slug);
      if (entry) {
        m.customized =
          m.cron_expr !== entry.cron ||
          m.timezone !== entry.timezone ||
          (m.model ?? "") !== entry.model ||
          m.auto_approve !== entry.auto_approve ||
          m.wait_on_limit !== entry.wait_on_limit ||
          (m.target === "sweep" && (m.max_issues ?? 0) !== entry.max_issues) ||
          // Owner guidance-overlay divergence for a prompt default (issue #662) or a sweep
          // default (issue #675): the OVERLAY (m.guidance) is null until the owner sets one,
          // so any non-empty overlay flips customized. The baked catalog guidance is NOT
          // compared (only the overlay's presence matters), matching the server.
          ((m.target === "prompt" || m.target === "sweep") && (m.guidance ?? "") !== "");
      }
    }
    m.updated_at = new Date().toISOString();
    schedules = schedules.map((x) => (x.id === id ? m : x));
    return delay(scheduleDTO(m));
  },
  deleteSchedule: async (id: string) => {
    requireSession();
    const s = schedules.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "schedule not found");
    schedules = schedules.filter((x) => x.id !== id);
    return delay(null);
  },
  runScheduleNow: async (id: string) => {
    requireSession();
    const s = schedules.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "schedule not found");
    // The demo does not spin up a live worker run; it reports one fired, matching
    // the seam's typical single-run outcome for a pinned issue / prompt.
    const runId = nextRunId();
    return delay(
      {
        created: 1,
        run_ids: [runId],
        matched: 1,
        capped: false,
        started: [{ issue_iid: s.issue_iid, run_id: runId, title: s.prompt || `#${s.issue_iid ?? ""}` }],
        skips: [],
      },
      250,
    );
  },
  previewSchedule: async (input: SchedulePreviewInput) => {
    requireSession();
    const n = Math.min(Math.max(input.n ?? 3, 1), schedulePreviewCap);
    if (input.timing === "once") {
      const fires = input.run_at && new Date(input.run_at).getTime() > Date.now()
        ? [new Date(input.run_at).toISOString()]
        : input.run_at
          ? [new Date(input.run_at).toISOString()]
          : [];
      return delay({ fires }, 80);
    }
    return delay({ fires: mockScheduleFires(input.cron_expr ?? "", n) }, 80);
  },

  // ── Default scheduled jobs (PRD #589) ──────────────────────────────────────
  listScheduleCatalog: async () => {
    requireSession();
    // Derive enablements from the live default rows so enable/reset/clone stay in
    // sync (presence of a default row for (repo, slug) IS the enablement).
    const enablements = schedules
      .filter((s) => s.origin === "default" && s.catalog_slug)
      .map((s) => ({
        repo_id: s.repo_id,
        slug: s.catalog_slug as string,
        schedule_id: s.id,
        enabled: s.enabled,
      }));
    return delay({ entries: scheduleCatalog.map((e) => ({ ...e, labels: e.labels ? [...e.labels] : null })), enablements });
  },
  enableCatalogSchedule: async (repoId: string, slug: string, timezone?: string) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const entry = catalogBySlug(slug);
    if (!entry) throw new ApiError(404, "catalog entry not found");
    // Idempotent (server 200): an already-materialized (repo, slug) — even paused —
    // returns its existing row untouched, never a fresh enable. Mirroring the server's
    // ON CONFLICT DO NOTHING, a re-enable ignores any timezone override (issue #660).
    const existing = schedules.find(
      (s) => s.origin === "default" && s.catalog_slug === slug && s.repo_id === repoId,
    );
    if (existing) return delay(scheduleDTO(existing));
    const s = materializeDefault(entry, repoId, nextScheduleId());
    // On the fresh-materialize path only, an optional detected browser timezone (issue
    // #660) overrides the catalog zone; an empty/absent tz keeps the catalog zone. Mirror
    // the production handler: trim, and reject an invalid IANA name (Intl throws a
    // RangeError on both a bogus name and the "Local" sentinel) with a 400.
    const tz = timezone?.trim();
    if (tz) {
      try {
        Intl.DateTimeFormat(undefined, { timeZone: tz });
      } catch {
        throw new ApiError(400, "invalid timezone");
      }
      s.timezone = tz;
    }
    schedules = [s, ...schedules];
    return delay(scheduleDTO(s), 200);
  },
  resetSchedule: async (id: string) => {
    requireSession();
    const cur = schedules.find((x) => x.id === id);
    if (!cur) throw new ApiError(404, "schedule not found");
    if (cur.origin !== "default" || !cur.catalog_slug)
      throw new ApiError(422, "only a default schedule can be reset");
    const entry = catalogBySlug(cur.catalog_slug);
    if (!entry) throw new ApiError(404, "catalog entry not found");
    // Restore the editable fields to the catalog values, keep the pause flag + repo,
    // and clear customized. Prompt/labels/guidance are already the resolved catalog
    // values on a default row, so re-materializing is exact.
    const restored = materializeDefault(entry, cur.repo_id, cur.id, {
      enabled: cur.enabled,
      last_fired_at: cur.last_fired_at,
      last_fire: cur.last_fire,
      created_at: cur.created_at,
    });
    schedules = schedules.map((x) => (x.id === id ? restored : x));
    return delay(scheduleDTO(restored));
  },
  cloneSchedule: async (id: string, repoId?: string) => {
    requireSession();
    const src = schedules.find((x) => x.id === id);
    if (!src) throw new ApiError(404, "schedule not found");
    const targetRepoId = repoId && repoId.trim() !== "" ? repoId : src.repo_id;
    const repo = repos.find((r) => r.id === targetRepoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const now = new Date().toISOString();
    // A user-owned copy with the source's resolved fields baked in and catalog_slug
    // cleared, so its prompt/labels/guidance become editable (origin='user').
    const clone: Schedule = {
      ...src,
      id: nextScheduleId(),
      repo_id: repo.id,
      repo_path: repo.path_with_namespace,
      origin: "user",
      catalog_slug: null,
      customized: false,
      enabled: true,
      status: "active",
      last_fired_at: null,
      last_fire: null,
      created_at: now,
      updated_at: now,
      next_fires: [],
      // A clone is always a user row, which never carries the read-only baked catalog
      // guidance (issue #675). For a SWEEP default, fold the baked catalog guidance into
      // the editable guidance and discard the owner overlay, mirroring the server clone.
      guidance:
        src.origin === "default" && src.target === "sweep" ? src.baked_guidance : src.guidance,
      baked_guidance: null,
    };
    schedules = [clone, ...schedules];
    return delay(scheduleDTO(clone), 200);
  },
  addScheduleRepo: async (id: string, repoId: string) => {
    requireSession();
    // Owner-scoped, origin='user' sources only (PRD #636 Decision 5), matching the server
    // (handler/schedules.go AddScheduleRepo): a foreign/absent source 404s, but a non-user
    // source is a 409 ("only a custom schedule can add a repo; clone a default first"),
    // mirroring ResetSchedule's origin-mismatch conflict — NOT a 404.
    const src = schedules.find((x) => x.id === id);
    if (!src) throw new ApiError(404, "schedule not found");
    if (src.origin !== "user")
      throw new ApiError(409, "only a custom schedule can add a repo; clone a default schedule first");
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    // Coalesce a group id onto the source (the server does this under a row lock so two
    // racing calls settle on one id; a single-writer mock just assigns it once).
    const groupId = src.sibling_group_id ?? crypto.randomUUID();
    if (!src.sibling_group_id) {
      schedules = schedules.map((x) => (x.id === src.id ? { ...x, sibling_group_id: groupId } : x));
    }
    // The partial unique index (sibling_group_id, repo_id) rejects a second sibling on a
    // repo already in the group — an idempotent-safe 409, no duplicate row created.
    if (schedules.some((x) => x.sibling_group_id === groupId && x.repo_id === repoId)) {
      throw new ApiError(409, "that schedule is already on that repo");
    }
    const now = new Date().toISOString();
    // Copy the source's current config onto the new repo as an independent sibling. The
    // server copies the SOURCE's `enabled` (cur.Enabled) — so a repo added from a PAUSED
    // custom schedule yields a PAUSED sibling — while `status` takes the CreateRunSchedule
    // default ("active"), the same fresh-row status the clone/materialize paths use.
    const sibling: Schedule = {
      ...src,
      id: nextScheduleId(),
      repo_id: repo.id,
      repo_path: repo.path_with_namespace,
      sibling_group_id: groupId,
      enabled: src.enabled,
      status: "active",
      last_fired_at: null,
      last_fire: null,
      created_at: now,
      updated_at: now,
      next_fires: [],
    };
    schedules = [sibling, ...schedules];
    return delay(scheduleDTO(sibling), 200);
  },
  checkRepoLabels: async (repoId: string, labels: string[]) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const have = repoLabels[repoId] ?? [];
    const missing = [...new Set(labels.map((l) => l.trim()).filter(Boolean))].filter(
      (l) => !have.includes(l),
    );
    return delay({ missing }, 150);
  },
  ensureRepoLabels: async (repoId: string, labels: string[]) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const ensured = [...new Set(labels.map((l) => l.trim()).filter(Boolean))];
    const have = repoLabels[repoId] ?? (repoLabels[repoId] = []);
    for (const l of ensured) if (!have.includes(l)) have.push(l);
    return delay({ ensured }, 200);
  },
};

const CHAT_MAX_TURNS = 50;

// chatDTO derives the chatListDTO shape from a chat run + its message log: the
// title (the run's chat title, else derived from the first user turn), the
// user-turn count, and last activity from the newest message (PRD #39 wire). No
// max_turns here — that rides the list envelope as an instance constant.
function chatDTO(run: Run): Chat {
  const msgs: RunMessage[] = state.messages.get(run.id) ?? [];
  const firstUser = msgs.find((m) => m.kind === "user_message");
  const derived = (firstUser?.payload as { text?: string } | null)?.text;
  const title = run.title ?? (derived ? truncateChatTitle(derived) : run.issue_title || null);
  const turnCount = msgs.reduce((n, m) => (m.kind === "user_message" ? n + 1 : n), 0);
  return {
    id: run.id,
    title,
    status: run.status,
    turn_count: turnCount,
    resume_of_run_id: run.resume_of_run_id,
    last_message_at: msgs[msgs.length - 1]?.created_at ?? null,
    created_at: run.created_at,
    updated_at: run.updated_at,
  };
}

function truncateChatTitle(s: string): string {
  const t = s.trim().replace(/\s+/g, " ");
  return t.length > 60 ? `${t.slice(0, 59)}…` : t;
}

// A run patch helper other mock surfaces can use (kept for symmetry/tests).
export { patchRun };
