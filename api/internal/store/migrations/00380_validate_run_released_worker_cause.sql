-- +goose Up

-- Validate runs_released_worker_cause_check added NOT VALID in 00379. VALIDATE CONSTRAINT takes
-- only a SHARE UPDATE EXCLUSIVE lock (reads and writes proceed). Both columns are new and NULL
-- on every existing row, so the backlog satisfies it by construction.
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs VALIDATE CONSTRAINT runs_released_worker_cause_check;

-- +goose Down

-- There is no VALIDATE inverse, so restore the pre-00380 state (constraint present but NOT
-- VALID) by dropping and re-adding it; the CHECK body matches 00379 verbatim.
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs DROP CONSTRAINT runs_released_worker_cause_check;
ALTER TABLE runs ADD CONSTRAINT runs_released_worker_cause_check
    CHECK ((released_worker_cause IS NULL) = (released_worker_cause_at IS NULL)
        AND (released_worker_cause IS NULL
            OR released_worker_cause IN ('wall', 'worker_memory_pressure', 'worker_requeue_exhausted'))) NOT VALID;
