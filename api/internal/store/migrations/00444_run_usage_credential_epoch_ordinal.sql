-- +goose Up
-- Issue #4201: per-leg credential epoch attribution; NULL means "the claim's base epoch". Its own
-- migration for the lock-order reason in 00443.
SET LOCAL lock_timeout = '5s';
ALTER TABLE run_usage ADD COLUMN credential_epoch_ordinal integer;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE run_usage DROP COLUMN IF EXISTS credential_epoch_ordinal;
