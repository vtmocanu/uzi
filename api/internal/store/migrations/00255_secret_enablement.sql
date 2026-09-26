-- +goose Up
ALTER TABLE user_secrets ADD COLUMN disabled_at timestamptz;
ALTER TABLE user_secrets ADD COLUMN enablement_rev bigint NOT NULL DEFAULT 0;
CREATE INDEX runs_credential_disabled_promoter_idx ON runs (user_id, id)
    WHERE status = 'paused' AND hold_reason = 'credential_disabled';

-- +goose Down
DROP INDEX runs_credential_disabled_promoter_idx;
ALTER TABLE user_secrets DROP COLUMN enablement_rev;
ALTER TABLE user_secrets DROP COLUMN disabled_at;
