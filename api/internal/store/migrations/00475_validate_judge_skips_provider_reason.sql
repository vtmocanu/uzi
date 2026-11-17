-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #4320 M4: validates the widened judge_skips.reason CHECK added NOT VALID in 00474.
ALTER TABLE judge_skips VALIDATE CONSTRAINT judge_skips_reason_check;

-- +goose Down
-- Validation has no data or schema to undo; 00474 owns the narrowing.
SELECT 1;
