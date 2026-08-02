-- Run-judge queries (PRD #46 M3). The judge is a worker-executed retrospective of a
-- finished run: the API enqueues it at the committed terminal transition, the worker
-- claims it through the normal queue, fetches the reviewed run's trace through a
-- judge-run-scoped endpoint, and posts back a verdict + recommendations.

-- name: CreateJudgeRun :one
-- wait_on_limit is deliberately NOT stamped here: a judge run NEVER parks
-- (PRD #35 Decision 14), and the column's DEFAULT false is the whole mechanism.
-- SetRunLimitWait additionally carries `AND kind <> 'judge'`, so this is the second
-- of two independent guards rather than the only one. Stated because an unstamped
-- column among three stamped siblings otherwise reads as the omission it is not.
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

-- name: GetActiveJudgeRunForTarget :one
-- The ACTIVE judge run for a target, for the run page's pending-judge signal (PRD #119
-- M1). This is GetActiveJudgeRunForWorkerTarget minus the worker scope: no worker owns
-- this read, it answers "is a judge already coming for this run?" for whoever can see
-- the target (owner-or-admin, enforced by GetRunForViewer BEFORE this read, never here).
--
-- The WHERE predicate is the uq_runs_one_active_judge_per_target partial unique index
-- (00058) with its INDEXED COLUMN SPELLED OUT — not a literal copy of the index, which
-- is `ON runs (target_run_id) WHERE kind = 'judge' AND status NOT IN
-- ('completed','failed','cancelled')`. This is that partial WHERE term for term, PLUS
-- the equality on the key column (target_run_id = @target_run_id) that the index carries
-- by being keyed on it rather than by predicating on it. The two are therefore
-- EQUIVALENT in the sense that matters: a row this query returns is exactly a row that
-- would make a fresh judge insert for the same target raise 23505, and no row returned
-- means no 23505. That equivalence is the whole point of the query and is load-bearing:
-- the UI must show "pending" in PRECISELY the set of states where a manual "Run judge"
-- click would hit the index and 23505, and offer the button in precisely the set where
-- the click is the legitimate way to start one. Paraphrase the predicate and the two
-- sets drift apart — the panel either hides a button that would have worked, or offers
-- one that can only produce an error toast, which is the confusion #119 exists to
-- remove. TestJudgeQueriesLiveDB pins the equivalence directly: an active judge found;
-- a completed/failed/cancelled one not; a judge on another target not; and a NON-judge
-- run carrying this target_run_id not.
--
-- Because the index is UNIQUE on target_run_id over exactly that partial predicate, at
-- most one row can ever match; LIMIT 1 is belt-and-braces, not a real narrowing. Only
-- the three columns the panel needs are projected — this is a UI signal, not the judge
-- machinery, so it deliberately does not return the whole run row the way the
-- worker-scoped query does.
SELECT id, status, created_at FROM runs
WHERE kind = 'judge'
  AND target_run_id = @target_run_id
  AND status NOT IN ('completed', 'failed', 'cancelled')
LIMIT 1;

-- name: ListToolTraceForRun :many
-- The tool trace of a run — the agent's tool_use invocations AND their tool_result
-- output — oldest first, capped by @lim. Input to the API-side command-not-found
-- scan (Decision 4) and to its later-ran-green suppression (PRD #121 M3). The scan
-- never decides anything, it just flags missing-executable evidence for the judge.
--
-- WIDENED from tool_result-only (was ListToolResultPayloadsForRun). The COMMAND the
-- agent typed lives ONLY in tool_use ({id, name, input}); tool_result carries
-- {tool_use_id, content, is_error} and no command at all. A successful `tsc --noEmit`
-- prints NOTHING, so no heuristic over tool_result text can tell that tsc ran — the
-- invocation side is the only place that evidence exists.
--
-- seq is projected, not just payload, because "X later ran green" is an ORDERING
-- claim. It used to be expressible only as "a larger slice index", correct solely by
-- virtue of this ORDER BY and unassertable by the pure scan function, which received
-- a bare [][]byte. Shaped exactly like ListRunToolWindow (runtime.sql), ASC not DESC.
SELECT seq, kind, payload FROM run_messages
WHERE run_id = @run_id AND kind IN ('tool_use', 'tool_result')
ORDER BY seq ASC
LIMIT @lim;

-- name: ListRunInputsForRun :many
-- The steering log (follow-ups + plan verdicts + cancels + PRD #88 answers) of a run,
-- oldest first, capped by @lim — part of the judge trace (Decision 3).
--
-- The column list is the table's FULL set (question_id, PRD #88, included), which is
-- what keeps sqlc returning the shared RunUserInput model rather than minting a
-- query-specific row type — dropping one re-types this query and breaks the
-- workersvc.Store interface and its fakes.
SELECT id, run_id, kind, body, consumed_at, created_at, question_id FROM run_user_inputs
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
