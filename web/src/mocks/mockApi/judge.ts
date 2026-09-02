import type {
  Disposition,
  JudgeBacklog,
  JudgeBacklogBucket,
  JudgeCategoryStats,
  JudgeDispositionCoord,
  JudgeDispositionResult,
  JudgeDispositionScope,
  JudgeOccurrence,
  JudgeRecommendationGroup,
  JudgeSettledMember,
  PendingJudge,
  RecommendationCategory,
  ReviewVerdict,
  Run,
  TriageCounts,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { coordKey } from "../../lib/judge";
import { mockPendingJudges, mockReviews, type MockReview } from "../data";
import { getRun, nextRunId } from "../store";
import { delay, mockScenario, requireSession } from "./shared";

export const reviews: MockReview[] = mockReviews.map((r) => ({
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

export const judgeApi = {
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
};
