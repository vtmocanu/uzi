-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runs VALIDATE CONSTRAINT runs_model_provider_exclusive_check;
-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT 1;
