-- +goose Up
-- Issue #3639: why the released_worker_* pair (D19) was recorded, so Admin Health can word the
-- queued reason for a memory-pressure park differently from a wall park.
--   released_worker_cause     'wall' | 'worker_memory_pressure' | 'worker_requeue_exhausted';
--                             NULL = legacy or unknown (no backfill)
--   released_worker_cause_at  written by the same statement now() as claim_released_at
-- Readers trust the cause only when released_worker_cause_at equals claim_released_at. A writer
-- that predates these columns (a rollback) moves or clears claim_released_at without touching
-- the stamp, which makes the cause stale, and readers then fall back to the generic wording.
-- There is deliberately no CHECK tying the cause to a non-null pair: an old ClaimRun clears the
-- pair but not the cause. The CHECK is added NOT VALID and validated in 00380 (two-step pattern
-- of 00367/00368).
--
-- The migration number is a draft: renumber the pair together above the live head at landing.
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs
    ADD COLUMN released_worker_cause text,
    ADD COLUMN released_worker_cause_at timestamptz,
    ADD CONSTRAINT runs_released_worker_cause_check
        CHECK ((released_worker_cause IS NULL) = (released_worker_cause_at IS NULL)
            AND (released_worker_cause IS NULL
                OR released_worker_cause IN ('wall', 'worker_memory_pressure', 'worker_requeue_exhausted'))) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs
    DROP COLUMN IF EXISTS released_worker_cause_at,
    DROP COLUMN IF EXISTS released_worker_cause;
