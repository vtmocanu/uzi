-- +goose Up
ALTER TABLE user_secrets ADD COLUMN anthropic_rejected_at timestamptz;
ALTER TABLE user_secrets ADD COLUMN anthropic_success_generation bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE user_secrets DROP COLUMN anthropic_success_generation;
ALTER TABLE user_secrets DROP COLUMN anthropic_rejected_at;
