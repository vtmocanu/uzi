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
-- wait_on_limit (PRD #35) comes from the OWNER's default: like ci_fix, this run is
-- created by an engine tick with no user in the loop. It parks (Decision 14) — it is
-- long, repo-ful, auto_approve and expensive, exactly the run whose loss to a
-- five-hour window hurts most. A parked one keeps holding
-- uq_runs_one_active_self_improve, which is correct: the engine's documented
-- behaviour on a blocked tick is "a cycle is still in flight, skip", and that is
-- precisely true of a parked run. Cancel-while-parked is the escape hatch.
--
-- model / override_subagent_model (PRD #590 M1): the schedule-driven fire path carries the
-- schedule's per-schedule model override (sqlc.narg('model'), NULL => inherit the owner
-- default at claim assembly) and the "apply model also to agents" opt-in
-- (@override_subagent_model, a plain bool). The bespoke engine passes nil/false, freezing
-- NULL/false onto its run exactly as before this column pair was threaded through.
INSERT INTO runs (
    user_id, repo_id, kind, issue_iid, issue_title, issue_description, auto_approve, wait_on_limit, model, override_subagent_model, required_capabilities
) VALUES (
    @user_id, @repo_id::uuid, 'self_improve', @issue_iid, @issue_title, @issue_description, true, @wait_on_limit, sqlc.narg('model'), @override_subagent_model,
    -- required_capabilities (PRD #84 M2, issue #512 M1): a self_improve run is REPO-BEARING
    -- (it targets uzi's own repo, the likeliest to require docker to build/test), so it must
    -- inherit the repo's capability hint like every other repo-bearing path — else with
    -- capability_aware ON a base worker claims it and fails mid-run. Inherit atomically via
    -- subquery reusing @repo_id, so no new Go struct field. Same expression CreateRun uses.
    COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}')
)
RETURNING *;

-- name: CountActiveSelfImproveRunsForRepo :one
-- The per-repo pre-check the schedule-driven fire path uses (PRD #590 M1): whether a
-- self_improve run is still active FOR THIS REPO. The instance-wide CountActiveSelfImproveRuns
-- above is too coarse now that a self_improve schedule can be enabled per repo — the
-- per-repo uq_runs_one_active_self_improve partial index (00158) is the hard guard at insert,
-- and this cheap count lets fireSelfImprove skip early (benign, advances the schedule) before
-- doing any forge work.
SELECT count(*) FROM runs
WHERE kind = 'self_improve' AND repo_id = @repo_id::uuid
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: ListOpenImproveUziRecommendationsForUser :many
-- The owner-scoped improve_uzi backlog (PRD #590 M1; the instance-wide sibling was deleted
-- with the engine in M2, so this is now the only improve_uzi backlog query). The unaddressed
-- improve_uzi recommendations with two coordinate exclusions (recommendation_filed_issues,
-- PRD #68 Decision 12; recommendation_dispositions, PRD #94 Decision 9), restricted to ONE
-- user's reviews via a JOIN to run_reviews (rv.user_id, the owner anchor — a review and its
-- recommendations are never cross-user; modeled on ListKnownImproveUziTargetsForUser in
-- judge_known_targets.sql). The schedule-driven fire path is per-owner, so it folds only that
-- owner's backlog into the run. Oldest first (@lim caps the block the prompt carries).
SELECT rr.id, rr.target, rr.rationale_md, rr.confidence, rr.created_at
FROM review_recommendations rr
JOIN run_reviews rv ON rv.id = rr.review_id
WHERE rr.category = 'improve_uzi' AND rr.addressed_by_run_id IS NULL
  AND rv.user_id = @user_id::uuid
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

-- name: RecentSelfImproveMRRunsForRepo :many
-- The repo's most-recent self_improve runs that opened an MR (mr_iid set),
-- bounded (PRD #686 D12). Feeds the forge-sourced open-MR cap (D10) and the picker's
-- open-MR context (D11/M10). "Open" is resolved LIVE from the forge per row, NOT from
-- runs.mr_state (unreliable for this multi-MR-per-tracking-issue lane — see D12).
SELECT id, mr_iid, branch, plan_md, issue_description
FROM runs
WHERE kind = 'self_improve' AND repo_id = @repo_id::uuid AND mr_iid IS NOT NULL
ORDER BY created_at DESC
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
