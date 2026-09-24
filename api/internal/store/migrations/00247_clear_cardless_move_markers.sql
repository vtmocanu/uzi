-- +goose Up

-- Issue #1482: terminal run writes used to stamp move_pending_since unconditionally, so a
-- run with no board card (issue_iid NULL: judge, chat, ci_fix, prompt, task) kept a pending
-- column-move marker forever. For the repo-less judge and chat runs the reconcile loop cannot
-- load a move context at all, so it logged a false "manual heal" give-up warning 30 minutes
-- later; the repo-bearing ci_fix/prompt/task runs did load one and reached the move and
-- terminal-comment path with no issue (now guarded in reconcileOne). The terminal writers
-- now stamp only card-bearing runs; this clears the markers already left.
UPDATE runs
SET move_pending_since = NULL
WHERE issue_iid IS NULL
  AND move_pending_since IS NOT NULL;

-- +goose Down

-- Data-only cleanup of markers that meant nothing: there is no prior state worth restoring.
SELECT 1;
