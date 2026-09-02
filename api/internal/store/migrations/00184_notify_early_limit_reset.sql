-- +goose Up
ALTER TABLE users ADD COLUMN notify_early_limit_reset BOOLEAN NOT NULL DEFAULT true;
-- +goose Down
ALTER TABLE users DROP COLUMN notify_early_limit_reset;
