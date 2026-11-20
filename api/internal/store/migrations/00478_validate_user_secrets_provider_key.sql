-- +goose Up
SET LOCAL lock_timeout = '5s';
-- PRD #4320: validates the kind check, the provider foreign key and the provider-key CHECK added
-- NOT VALID in 00477.
ALTER TABLE user_secrets VALIDATE CONSTRAINT user_secrets_kind_check;
ALTER TABLE user_secrets VALIDATE CONSTRAINT user_secrets_provider_fkey;
ALTER TABLE user_secrets VALIDATE CONSTRAINT user_secrets_provider_key_check;

-- +goose Down
-- Validation has no data or schema to undo; 00477 owns the narrowing guard.
SELECT 1;
