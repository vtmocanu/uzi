-- +goose Up

-- Validate the widened status CHECK from 00206 under a write-compatible lock,
-- separately from the schema changes in that migration.
ALTER TABLE runs VALIDATE CONSTRAINT runs_status_check;

-- +goose Down

-- Restore the pre-validation state. The subsequent 00206 Down narrows the domain.
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'claimed', 'running', 'awaiting_approval', 'awaiting_input',
                      'limit_wait', 'completed', 'failed', 'cancelled', 'awaiting_followup',
                      'pool_wait', 'paused', 'recovery_wait')) NOT VALID;
