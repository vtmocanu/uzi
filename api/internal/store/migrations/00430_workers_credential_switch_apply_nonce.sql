-- +goose Up
-- Issue #4201: the registration nonce stamped at registration by an apply-negotiated worker. Its
-- own migration for the lock-order reason in 00428.
SET LOCAL lock_timeout = '5s';
ALTER TABLE workers ADD COLUMN credential_switch_apply_nonce text;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE workers DROP COLUMN IF EXISTS credential_switch_apply_nonce;
