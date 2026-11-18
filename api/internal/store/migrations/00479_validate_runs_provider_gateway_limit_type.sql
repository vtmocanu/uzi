-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #4320 M6: validates the widened runs_rate_limit_type_check added NOT VALID in 00478. This is the
-- model_provider gateway-limit vocabulary half; the file touches runs only.
ALTER TABLE runs VALIDATE CONSTRAINT runs_rate_limit_type_check;

-- +goose Down
-- Validation has no data or schema to undo; 00478 owns the narrowing.
SELECT 1;
