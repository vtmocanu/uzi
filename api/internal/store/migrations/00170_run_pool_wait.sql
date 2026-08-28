-- +goose Up

-- PRD #754 M4: 'pool_wait' — a distinct, NON-LOCKING hold for an `auto` run whose
-- token pool is genuinely empty. M2 left an interim where an empty-pool claim
-- requeued the run (a busy requeue); M4 replaces that with a real hold status the
-- claim path transitions to, so the run is visibly waiting for a pooled token rather
-- than churning the queue. Reactive + manual resume land in M5.
--
-- Two schema changes ride ONE migration because they are one feature and must land
-- atomically: the status the run holds in, and the dedup index that must NOT count
-- that hold as an active run (Decision 8, the non-locking property).

-- runs.status: widen to an ELEVENTH value. The ten existing values are carried
-- VERBATIM from the LIVE constraint (last re-added in 00146_interactive_task_runs.sql)
-- — a DROP+ADD that re-derives the list from anything but the live constraint silently
-- deletes whatever it forgets (00092 documents exactly that failure with 'limit_wait').
-- 'pool_wait' is a NON-TERMINAL hold, like limit_wait/awaiting_input/awaiting_followup.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled', 'awaiting_followup',
                      'pool_wait'));

-- uq_runs_one_active_per_issue: EXCLUDE 'pool_wait' from the active set (Decision 8).
-- Unlike every other non-terminal status, a pool_wait hold must NOT lock the issue: a
-- held run is inert (no worker is spending on it, and M5 resumes it independently), so
-- a subsequent manual/board/Slack start for the same issue must not be structurally
-- refused by this index. The manual path's dedup is preserved in Go instead
-- (Service.CreateRun's HasActiveRunForIssue pre-check, which still counts a held run as
-- active). The index body is carried verbatim from the live definition in
-- 00043_ci_fix_runs.sql (kind='issue' scope, negative status guard), with 'pool_wait'
-- added to the excluded set.
DROP INDEX uq_runs_one_active_per_issue;
CREATE UNIQUE INDEX uq_runs_one_active_per_issue
    ON runs (repo_id, issue_iid)
    WHERE kind = 'issue' AND status NOT IN ('completed', 'failed', 'cancelled', 'pool_wait');

-- +goose Down

-- Restore the index WITHOUT the 'pool_wait' exclusion, exactly as 00043 left it.
DROP INDEX uq_runs_one_active_per_issue;
CREATE UNIQUE INDEX uq_runs_one_active_per_issue
    ON runs (repo_id, issue_iid)
    WHERE kind = 'issue' AND status NOT IN ('completed', 'failed', 'cancelled');

-- Narrow the status CHECK back to the ten values that existed immediately before this
-- migration (00146's set). DATA-DEPENDENT and best-effort, copying 00091/00146's
-- honesty: the re-added narrower CHECK FAILS if any run is currently held in
-- 'pool_wait', and this migration then refuses to come down — the correct outcome, since
-- a down that silently stranded a held run would be worse. Goose downs are not run in
-- this deployment (store.Migrate only ever goes up); drain first if you must.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled', 'awaiting_followup'));
