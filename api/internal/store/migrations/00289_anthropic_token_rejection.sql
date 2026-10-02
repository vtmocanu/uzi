-- +goose Up
ALTER TABLE user_secrets ADD COLUMN anthropic_rejected_at timestamptz;

-- +goose Down
ALTER TABLE user_secrets DROP COLUMN anthropic_rejected_at;
