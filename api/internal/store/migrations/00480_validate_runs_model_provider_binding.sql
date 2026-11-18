-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #4320: validates the model_provider CHECKs added NOT VALID in 00479 (write-compatible scan).
ALTER TABLE runs VALIDATE CONSTRAINT runs_model_provider_key_mode_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_model_provider_binding_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_model_provider_exclusive_check;

-- +goose Down
-- Validation has no data or schema to undo; 00479 owns the drop.
SELECT 1;
