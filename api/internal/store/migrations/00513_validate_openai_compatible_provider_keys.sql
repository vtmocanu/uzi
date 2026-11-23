-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE user_secrets VALIDATE CONSTRAINT user_secrets_kind_check;
ALTER TABLE user_secrets VALIDATE CONSTRAINT user_secrets_provider_key_check;
ALTER TABLE user_secrets VALIDATE CONSTRAINT user_secrets_provider_default_check;

-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT 1;
