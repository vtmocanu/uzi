-- Self-improvement engine (PRD #46 Decisions 9-11, M5) ------------------------

-- name: CreateSelfImproveRun :one
-- Queue the autonomous self-improvement run against uzi's own repo (Decision 10).
-- Issue-shaped (repo_id + issue_iid from the tracking issue, satisfying the
-- self_improve kind-shape CHECK), auto_approve=true (autopilot-style: no human plan
-- gate), kind='self_improve'.
--
-- The plan is still emitted as a `plan` run_message and is inspectable on the feed,
-- but runs.plan_md stays NULL for every self_improve run: SetRunAwaitingApproval
-- (runtime.sql) is the ONLY writer of that column in the whole schema, and the
-- autopilot branch of the worker's gatePlan reports {status:"running"}, never
-- entering awaiting_approval. This comment used to say plan_md was "stored", which
-- would send any reader of the column to a silent no-op on exactly the mode with no
-- human in the loop (corrected 2026-07-26, PRD #121 M3).
--
-- Shaped like CreateCIFixRun — a dedicated insert, NOT createRun, because the normal path
-- requires the issue to be in the poller cache and to carry a PRD link, neither of
-- which a just-filed tracking issue has (review B2); the engine snapshots
-- title/description directly. origin_column stays NULL — the tracking issue's
-- board move is a known, accepted side effect driven by runlifecycle, not snapped
-- here. The uq_runs_one_active_self_improve partial index admits at most one
-- non-terminal self_improve instance-wide (23505 → ErrActiveSelfImproveExists), so
-- Boot re-runs and any future replica never double-create onto the fixed branch.
INSERT INTO runs (
    user_id, repo_id, kind, issue_iid, issue_title, issue_description, auto_approve
) VALUES (
    @user_id, @repo_id::uuid, 'self_improve', @issue_iid, @issue_title, @issue_description, true
)
RETURNING *;

-- name: CountActiveSelfImproveRuns :one
-- Whether a self_improve run is still active (Decision 9 tick skip). The unique
-- index is the hard guard at insert; this cheap count lets the engine skip early
-- (with a notification, no silent stall) before doing any forge work.
SELECT count(*) FROM runs
WHERE kind = 'self_improve' AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: ListOpenImproveUziRecommendations :many
-- The unaddressed improve_uzi backlog the engine folds into the planning prompt
-- (Decision 11). Served by idx_review_recommendations_improve_uzi_open (partial on
-- category='improve_uzi' AND addressed_by_run_id IS NULL). Oldest first so the
-- longest-waiting items lead; @lim caps the block the prompt carries so a large
-- backlog can't blow the prompt budget.
--
-- TWO coordinate exclusions, either of which drops the recommendation from the backlog:
--
--   PRD #68 Decision 12: any coordinate with a row in recommendation_filed_issues — a
--   CLAIMED-OR-FILED NOT EXISTS (the row EXISTING is the exclusion, NOT
--   filed_at IS NOT NULL), so a hand-filed OR mid-filing improve_uzi issue and this
--   backlog never cover the same coordinate twice; a reverted (deleted) claim re-includes
--   it next cycle.
--
--   PRD #94 Decision 9: any coordinate with a row in recommendation_dispositions — a
--   dismissed improve_uzi is a human "no", a done one is handled, so either way the engine
--   must not fold it into its aggregated tracking issue. Row-existence is the exclusion
--   (ANY disposition, regardless of status/reason); Undo (row deleted) re-includes it next
--   cycle.
--
-- Both are the only mechanism — a partial index cannot cross tables; each side-table's
-- UNIQUE (review_id, category, target) index serves its correlated lookup.
SELECT rr.id, rr.target, rr.rationale_md, rr.confidence, rr.created_at
FROM review_recommendations rr
WHERE rr.category = 'improve_uzi' AND rr.addressed_by_run_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM recommendation_filed_issues f
      WHERE f.review_id = rr.review_id
        AND f.category  = rr.category
        AND f.target    = rr.target
  )
  AND NOT EXISTS (
      SELECT 1 FROM recommendation_dispositions d
      WHERE d.review_id = rr.review_id
        AND d.category  = rr.category
        AND d.target    = rr.target
  )
ORDER BY rr.created_at ASC
LIMIT @lim;

-- name: MarkImproveUziRecommendationsAddressed :execrows
-- Stamp the exact backlog rows the engine folded into a run as addressed by that
-- run (Decision 11). Marks by the precise id set the engine listed (not "all open")
-- so a recommendation that arrived between the list and this write is left for the
-- next cycle — the set the run carries and the set it clears stay identical. The
-- addressed_by_run_id IS NULL guard keeps a concurrent stamp idempotent.
-- addressed_by_run_id is only ever set here, only on improve_uzi rows.
UPDATE review_recommendations
SET addressed_by_run_id = @addressed_by_run_id
WHERE id = ANY(@ids::uuid[]) AND category = 'improve_uzi' AND addressed_by_run_id IS NULL;
