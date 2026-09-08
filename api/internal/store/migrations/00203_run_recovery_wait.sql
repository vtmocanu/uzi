-- +goose Up

-- RC1/RC2 (issue #1197): 'recovery_wait' — a transient-recovery park for a run that
-- reported a positively-empty SDK turn after bounded in-process retries. It is modelled
-- CLOSELY on 'limit_wait' (PRD #35): a NON-TERMINAL, worker-held park that the sweeper
-- promotes back to 'queued' once a server-owned exponential backoff elapses. The two
-- differ where it matters: recovery_wait is NOT a usage limit, it has NO lifetime cap
-- and NO terminal branch — a park always becomes promotable again after its capped
-- backoff, so the run auto-resumes repeatedly until it recovers or the owner cancels.
--
-- Three schema changes ride ONE migration because they are one feature and must land
-- atomically: the status the run holds in, the backoff-count column, and the retry stamp
-- (plus the partial promotion index over it).

-- runs.status: widen to a TWELFTH value. The eleven existing values are carried VERBATIM
-- from the LIVE constraint (last re-added in 00170_run_pool_wait.sql) — a DROP+ADD that
-- re-derives the list from anything but the live constraint silently deletes whatever it
-- forgets (00092 documents exactly that failure with 'limit_wait'). 'recovery_wait' is a
-- NON-TERMINAL park, like limit_wait/awaiting_input/awaiting_followup/pool_wait.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled', 'awaiting_followup',
                      'pool_wait', 'recovery_wait'));

-- recovery_wait_count is the backoff shaper (issue #1197). It is bumped once per park by
-- SetRunRecoveryWait, in the SAME statement as the transition, so recoveryParkFallbackFor
-- reads the count BEFORE this park (0 on the first) — the same 0-based convention
-- limit_wait_count uses. It shapes the exponential-backoff curve ONLY and is NEVER a cap:
-- there is no RUN_RECOVERY_MAX_WAITS and no branch that fails the run on a high count.
-- NOT NULL DEFAULT 0 so every existing run reads 0.
ALTER TABLE runs ADD COLUMN recovery_wait_count int NOT NULL DEFAULT 0;

-- recovery_retry_not_before is the promotion gate: PromoteRecoveryWaitRuns brings a run
-- back to 'queued' once now() passes it. Computed in GO (now + capped exponential backoff
-- + jitter) and passed in, mirroring how limit_wait's retry_not_before is computed. NULL
-- for a run that never parked; a NULL is never promoted (NULL <= now is UNKNOWN), which is
-- harmless here since a park always writes a finite value.
ALTER TABLE runs ADD COLUMN recovery_retry_not_before timestamptz;

-- The promotion pass (PromoteRecoveryWaitRuns) sweeps
-- `status = 'recovery_wait' AND recovery_retry_not_before <= now()` on every tick. Partial
-- on the status so the index holds only parked runs — a set that is empty on a healthy
-- instance — rather than one entry per run ever created. Mirrors idx_runs_limit_wait_retry
-- (00091), the equivalent index for the usage-limit park's promotion pass.
CREATE INDEX idx_runs_recovery_wait_retry
    ON runs (recovery_retry_not_before)
    WHERE status = 'recovery_wait';

-- +goose Down

DROP INDEX idx_runs_recovery_wait_retry;
ALTER TABLE runs DROP COLUMN recovery_retry_not_before;
ALTER TABLE runs DROP COLUMN recovery_wait_count;

-- Narrow the status CHECK back to the eleven values that existed immediately before this
-- migration (00170's set). DATA-DEPENDENT and best-effort, copying 00170/00091's honesty:
-- the re-added narrower CHECK FAILS if any run is currently held in 'recovery_wait', and
-- this migration then refuses to come down — the correct outcome, since a down that
-- silently stranded a parked run would be worse. Goose downs are not run in this
-- deployment (store.Migrate only ever goes up); drain first if you must.
ALTER TABLE runs DROP CONSTRAINT IF EXISTS runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled', 'awaiting_followup',
                      'pool_wait'));
