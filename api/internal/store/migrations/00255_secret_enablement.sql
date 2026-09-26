-- +goose Up
ALTER TABLE user_secrets ADD COLUMN disabled_at timestamptz;
ALTER TABLE user_secrets ADD COLUMN enablement_rev bigint NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN claimed_worker_nonce text;
ALTER TABLE runs ADD COLUMN credential_disable_released_worker_id uuid;
CREATE INDEX runs_credential_disabled_promoter_idx ON runs (user_id, status_since, id)
    WHERE status = 'paused' AND hold_reason = 'credential_disabled';

-- +goose Down
DROP INDEX runs_credential_disabled_promoter_idx;
ALTER TABLE runs DROP COLUMN credential_disable_released_worker_id;
ALTER TABLE runs DROP COLUMN claimed_worker_nonce;
ALTER TABLE user_secrets DROP COLUMN enablement_rev;
ALTER TABLE user_secrets DROP COLUMN disabled_at;
