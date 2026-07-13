-- Run-judge queries (PRD #46 M3). The judge is a worker-executed retrospective of a
-- finished run: the API enqueues it at the committed terminal transition, the worker
-- claims it through the normal queue, fetches the reviewed run's trace through a
-- judge-run-scoped endpoint, and posts back a verdict + recommendations.

-- name: CreateJudgeRun :one
-- Enqueue a judge run for a finished target run (Decision 2). Owned by the SAME user
-- as the target (never cross-user). issue_title/description are synthesized — a judge
-- has no issue. The one-active-judge-per-target partial unique index (00057) makes a
-- duplicate raise 23505, which the caller treats as "already being judged" (a no-op).
INSERT INTO runs (user_id, kind, target_run_id, issue_title, issue_description, status)
VALUES (@user_id, 'judge', @target_run_id, @issue_title, @issue_description, 'queued')
RETURNING *;

-- name: GetActiveJudgeRunForWorkerTarget :one
-- Trace/review authorization (Decision 3, audit H1): the caller's worker must own a
-- NON-TERMINAL judge run whose target_run_id is @target_run_id. This is judge-run
-- -scoped, not user-scoped — a worker can only stream the trace of the run its own
-- active judge run is reviewing, never any of its user's runs at will. Returns the
-- judge run so the caller re-asserts target.user_id == judge.user_id independently.
SELECT * FROM runs
WHERE worker_id = @worker_id
  AND kind = 'judge'
  AND target_run_id = @target_run_id
  AND status NOT IN ('completed', 'failed', 'cancelled')
LIMIT 1;

-- name: ListToolResultPayloadsForRun :many
-- The tool_result message payloads of a run, oldest first, capped by @lim — the input
-- to the API-side command-not-found scan (Decision 4). Only tool_result kind; the
-- scan never decides anything, it just flags missing-executable evidence for the judge.
SELECT payload FROM run_messages
WHERE run_id = @run_id AND kind = 'tool_result'
ORDER BY seq ASC
LIMIT @lim;

-- name: ListRunInputsForRun :many
-- The steering log (follow-ups + plan verdicts + cancels) of a run, oldest first,
-- capped by @lim — part of the judge trace (Decision 3).
SELECT id, run_id, kind, body, consumed_at, created_at FROM run_user_inputs
WHERE run_id = @run_id
ORDER BY id ASC
LIMIT @lim;

-- name: UpsertRunReviewWithRecommendations :one
-- Persist the judge's verdict + its recommendations for a target run in ONE atomic
-- statement (Decision 5/8): upsert the review (UNIQUE(target_run_id) makes a re-judge
-- REPLACE rather than 23505), clear the old recommendations, and insert the fresh set
-- from a jsonb array. Doing it in a single CTE avoids a partial write without needing
-- a service-level transaction. The recommendation rows arrive already validated +
-- scrubbed in Go; the table CHECK on category/confidence is the backstop. provenance
-- (produced_by_*) is the same judge run + owner for every row, passed as scalars.
WITH upserted AS (
    INSERT INTO run_reviews (target_run_id, judge_run_id, user_id, verdict, summary_md, judge_model, status)
    VALUES (@target_run_id, @judge_run_id, @user_id, @verdict, @summary_md, @judge_model, @status)
    ON CONFLICT (target_run_id) DO UPDATE
        SET judge_run_id = EXCLUDED.judge_run_id,
            verdict      = EXCLUDED.verdict,
            summary_md   = EXCLUDED.summary_md,
            judge_model  = EXCLUDED.judge_model,
            status       = EXCLUDED.status,
            updated_at   = now()
    RETURNING id
),
cleared AS (
    DELETE FROM review_recommendations WHERE review_id = (SELECT id FROM upserted)
),
inserted AS (
    INSERT INTO review_recommendations
        (review_id, category, target, rationale_md, confidence, produced_by_run_id, produced_by_user_id)
    SELECT (SELECT id FROM upserted), x.category, x.target, x.rationale_md, x.confidence,
           sqlc.narg('produced_by_run_id'), sqlc.narg('produced_by_user_id')
    FROM jsonb_to_recordset(@recommendations::jsonb)
        AS x(category text, target text, rationale_md text, confidence text)
)
SELECT id FROM upserted;

-- name: GetRunReviewForTarget :one
-- The judge's verdict for a target run, for the run-page review panel (Decision 5,
-- M4). Read-side counterpart to the write above; UNIQUE(target_run_id) makes it at
-- most one row. Owner-or-admin visibility is enforced by the caller (GetRunForViewer
-- on the target run) BEFORE this read, not here — this is a plain by-target lookup.
SELECT * FROM run_reviews WHERE target_run_id = @target_run_id;

-- name: ListRecommendationsForReview :many
-- The structured recommendations of a review, oldest-first, for the run-page panel
-- (Decision 5, M4). Served by idx_review_recommendations_review. Every free-text
-- field was scrubbed + capped at the review POST (M3), so the panel renders them as
-- escaped text; this read adds no trust.
SELECT * FROM review_recommendations
WHERE review_id = @review_id
ORDER BY created_at ASC, id ASC;
