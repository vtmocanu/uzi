-- Run progress hint queries (issue #2602, M2): the run-detail "may be blocked by"
-- hint. Read only on GET /api/runs/{id}, only for a run in awaiting_input.

-- name: RunQuestionAnswerExists :one
-- True when ANY answer for the run's open question was submitted, applied or not: the
-- owner may have answered while the status still reads awaiting_input, before the
-- worker consumes it, and the hint must not show then. Served by
-- idx_run_user_inputs_answer (migration 00315).
SELECT EXISTS (
    SELECT 1 FROM run_user_inputs
    WHERE run_id = @run_id
      AND kind = 'answer'
      AND question_id = @question_id
)::boolean AS answered;

-- name: GetMaybeBlockingRun :one
-- The newest non-terminal run, of the SAME OWNER and repo as the parked run, that
-- the parked run's question mentions: an issue run on one of @iids, or an
-- mr_rework / ci_fix run whose pipeline_ref is one of @refs (agent/issue-<n>).
-- @owner is the parked run's owner, never the viewer (an admin may view another
-- user's run), so a hint can never name a run its owner cannot see. A live issue
-- run's branch stays NULL until its terminal report, so branch is not a key.
-- pool_wait counts as live (uq_runs_one_active_per_issue excludes it, so the issue
-- arm uses idx_runs_issue_history instead). Both arms are bounded by the caller to at
-- most 5 values; one OR query, LIMIT 1.
SELECT id FROM runs
WHERE user_id = @owner
  AND repo_id = @repo_id::uuid
  AND id <> @run_id
  AND status NOT IN ('completed', 'failed', 'cancelled')
  AND (
        (kind = 'issue' AND issue_iid = ANY(@iids::bigint[]))
     OR (kind IN ('mr_rework', 'ci_fix') AND pipeline_ref = ANY(@refs::text[]))
  )
ORDER BY created_at DESC
LIMIT 1;
