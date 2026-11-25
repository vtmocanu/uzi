-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #4322 M1a: validates the widened judge_skips.reason CHECK added NOT VALID in 00532.
ALTER TABLE judge_skips VALIDATE CONSTRAINT judge_skips_reason_check;

-- +goose Down
-- Validation has no data or schema to undo; 00532 owns the narrowing.
SELECT 1;
