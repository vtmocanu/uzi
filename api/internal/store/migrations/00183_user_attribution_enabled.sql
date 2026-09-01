-- +goose Up
ALTER TABLE users ADD COLUMN attribution_enabled BOOLEAN NOT NULL DEFAULT true;
-- +goose Down
ALTER TABLE users DROP COLUMN attribution_enabled;
