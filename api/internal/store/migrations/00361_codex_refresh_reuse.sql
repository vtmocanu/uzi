-- +goose Up
ALTER TABLE codex_refresh_intent
    ADD COLUMN provider_error_at timestamptz,
    ADD COLUMN reuse_attempted_at timestamptz;

-- +goose Down
ALTER TABLE codex_refresh_intent
    DROP COLUMN reuse_attempted_at,
    DROP COLUMN provider_error_at;
