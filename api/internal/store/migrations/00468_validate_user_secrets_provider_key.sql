-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #4320: validates the model_provider key CHECK added NOT VALID in 00467.
ALTER TABLE user_secrets VALIDATE CONSTRAINT user_secrets_provider_key_check;

-- +goose Down
-- Validation has no data or schema to undo; 00467 owns the narrowing guard.
SELECT 1;
