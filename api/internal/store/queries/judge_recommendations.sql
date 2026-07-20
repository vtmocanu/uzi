-- The Judge menu's grouped read model (PRD #98 M1, Decision 1). Same JOIN SPINE as
-- #94's ListJudgeTriageRowsForUser (dispositions.sql) — the caller's reviews →
-- recommendations, LEFT JOINed to the filed link + the disposition on the
-- (review_id, category, target) coordinate — but a genuinely NEW, WIDER query: it
-- additionally joins `runs` (for issue_title → run_title) and projects the verdict,
-- the recommendation's confidence/rationale, the rec id, and the filed issue. It is
-- deliberately NOT a superset reuse of the narrow stats query: that one stays a
-- three-column scan so the strip's cost never tracks this page's payload.

-- name: ListJudgeRecommendationRowsForUser :many
-- ONE ROW PER RECOMMENDATION across every review the caller owns, carrying everything
-- the (category, target) grouping needs. Grouping, bucketing, filtering and the group
-- rollup all happen in GO (workersvc.GroupJudgeRecommendations): there is deliberately
-- NO SQL CASE and no GROUP BY here, because the bucket ladder is the one shared Go
-- helper BucketOf (PRD #94 Decision 2) and a SQL ladder would be a second copy of it.
--
-- Nullability: `d.*` and `f.*` come from LEFT JOINs, so sqlc types them nullable even
-- where the column is NOT NULL in its table. The (f.filed_at IS NOT NULL)::bool cast is
-- REQUIRED — without it sqlc types the boolean expression column as interface{} (same
-- note as dispositions.sql). Neither side-table join can fan out: both are UNIQUE on the
-- coordinate. The `runs` join is 1:1 (rv.target_run_id → runs.id, and target_run_id is
-- itself UNIQUE), so it cannot fan out either.
--
-- Order: most-recently-JUDGED review first, then the review's own recommendation order.
-- The Go grouper relies on this — a group's FIRST row is its most-recent occurrence, and
-- that row's rationale_md becomes rationale_preview (Decision 1).
--
-- The leading key is rv.updated_at, NOT rv.created_at, and the difference is observable:
-- UpsertRunReviewWithRecommendations (judge.sql) makes a RE-JUDGE an in-place upsert that
-- rewrites rationale_md and bumps updated_at while LEAVING created_at at the first judging.
-- Ordering by created_at would therefore show a run judged last week ahead of one
-- re-judged five minutes ago, and the preview would quote text the judge has already
-- replaced. created_at stays as the tiebreak so reviews judged in the same instant still
-- order deterministically. Freezing updated_at (or swapping these two keys) breaks
-- TestJudgeBacklogPreviewRecencyLiveDB — which is a live-DB test precisely because this
-- ordering is a property of the SQL and nothing in Go can hold it.
SELECT
    rv.id                          AS review_id,
    rv.target_run_id               AS run_id,
    rv.verdict                     AS verdict,
    r.issue_title                  AS run_title,
    rr.id                          AS rec_id,
    rr.category                    AS category,
    rr.target                      AS target,
    rr.rationale_md                AS rationale_md,
    rr.confidence                  AS confidence,
    d.status                       AS disposition_status,
    d.dismiss_reason               AS dismiss_reason,
    (f.filed_at IS NOT NULL)::bool AS filed_settled,
    f.filed_issue_iid              AS filed_issue_iid,
    f.filed_issue_url              AS filed_issue_url,
    f.filed_at                     AS filed_at
FROM run_reviews rv
JOIN runs r ON r.id = rv.target_run_id
JOIN review_recommendations rr ON rr.review_id = rv.id
LEFT JOIN recommendation_dispositions d
    ON d.review_id = rv.id AND d.category = rr.category AND d.target = rr.target
LEFT JOIN recommendation_filed_issues f
    ON f.review_id = rv.id AND f.category = rr.category AND f.target = rr.target
WHERE rv.user_id = @user_id
  -- The ?run= anchor (the judge notification's /judge?run={id} deep-link) is pushed DOWN
  -- here rather than post-filtered in Go, so an anchored pull reads only the rows it will
  -- return. It is a SEMI-join, not an equality: keep every occurrence of a coordinate that
  -- ALSO occurs in the anchor run, so a group arrived at from a notification still shows
  -- the other runs it recurs in — the whole point of the dedup. The subquery is scoped
  -- rv2.user_id = @user_id, so it can only ever see the CALLER's own reviews: an anchor
  -- naming another user's (or a nonexistent) run matches nothing, with no existence oracle.
  --
  -- The subquery pins @user_id DIRECTLY rather than correlating `rv2.user_id = rv.user_id`.
  -- The correlated form is also correct today — the outer WHERE pins rv.user_id to the
  -- caller, so it can only ever bind to the caller — but it is correct only BECAUSE that
  -- outer filter holds. This repo's guardrail rule is not to lean one layer on another
  -- layer holding, and the outer filter is exactly the kind of thing a future admin or
  -- cross-user view would relax. Pinning directly costs nothing and keeps the anchor
  -- owner-scoped on its own.
  AND (
      sqlc.narg('run_anchor')::uuid IS NULL
      OR EXISTS (
          SELECT 1
          FROM run_reviews rv2
          JOIN review_recommendations rr2 ON rr2.review_id = rv2.id
          WHERE rv2.user_id = @user_id
            AND rv2.target_run_id = sqlc.narg('run_anchor')::uuid
            AND rr2.category = rr.category
            AND rr2.target = rr.target
      )
  )
ORDER BY rv.updated_at DESC, rv.created_at DESC, rv.id DESC, rr.created_at ASC, rr.id ASC
-- A hard row bound: an all-time backlog with ?bucket=all is otherwise unbounded (the PRD's
-- Risks section concedes this). The caller passes cap+1 and reports `truncated` when the
-- extra row comes back, so the cut is exact rather than inferred. Because the order is
-- most-recent-review-first, a cut drops the OLDEST occurrences — never the newest. The
-- triage tally is deliberately NOT computed from these rows (the service reads the #94
-- stats query for that), so the canonical counts stay whole even when this page is cut.
LIMIT @lim;

-- name: ListJudgeTriageRowsForRuns :many
-- The per-recommendation triage facts for a SET of runs — the input to the /runs list's
-- judge_todo_count (PRD #98 M4, Decision 7).
--
-- This exists as a separate query precisely BECAUSE the count must not be computed in the
-- run-list join. Two independent reasons, both from #94: joining review_recommendations
-- into ListRunsForUser would fan it out (≤50 recs per review → up to 50 duplicate run
-- rows), and counting `todo` in SQL would re-implement the ladder's bottom rung
-- (disposition IS NULL AND filed_at IS NULL), which #94 Decision 2 forbids outright. So
-- this returns the same three flat facts the shared Go BucketOf consumes — no CASE, no
-- aggregation — plus the run id to attach the tally to.
--
-- Owner-scoped by rv.user_id, so the caller's own page can never surface another user's
-- recommendation counts even if a run id were somehow spoofed into the list.
--
-- BOUNDED (@lim), like every other enumeration in this PRD. The run list is capped at 200
-- and a review carries ≤50 recommendations (ReviewMaxRecommendations), so a full page tops
-- out around 10,000 rows; the cap is the guardrail for that product, applied here rather
-- than discovered later.
SELECT
    rv.target_run_id               AS run_id,
    d.status                       AS disposition_status,
    (f.filed_at IS NOT NULL)::bool AS filed_settled
FROM run_reviews rv
JOIN review_recommendations rr ON rr.review_id = rv.id
LEFT JOIN recommendation_dispositions d
    ON d.review_id = rv.id AND d.category = rr.category AND d.target = rr.target
LEFT JOIN recommendation_filed_issues f
    ON f.review_id = rv.id AND f.category = rr.category AND f.target = rr.target
WHERE rv.user_id = @user_id
  AND rv.target_run_id = ANY(@run_ids::uuid[])
LIMIT @lim;
