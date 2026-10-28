-- +goose Up
ALTER TABLE runs VALIDATE CONSTRAINT runs_idle_stall_park_count_nonnegative;
ALTER TABLE runs VALIDATE CONSTRAINT runs_recovery_wait_cause_check;

-- +goose Down
-- Validation has no data or schema reversal.
