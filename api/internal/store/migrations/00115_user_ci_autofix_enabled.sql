-- +goose Up
ALTER TABLE users ADD COLUMN ci_autofix_enabled BOOLEAN NOT NULL DEFAULT false;
-- +goose Down
ALTER TABLE users DROP COLUMN ci_autofix_enabled;
