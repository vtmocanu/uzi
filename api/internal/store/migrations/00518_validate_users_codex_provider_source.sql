-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE users VALIDATE CONSTRAINT users_codex_provider_fkey;
-- +goose Down
SET LOCAL lock_timeout = '5s';
SELECT 1;
