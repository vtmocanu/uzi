-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs VALIDATE CONSTRAINT runs_recovery_wait_detail_check;

-- +goose Down
-- Validation has no data or schema to undo; 00462 owns the narrowing guard.
SELECT 1;
