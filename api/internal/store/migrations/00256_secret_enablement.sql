-- +goose Up
ALTER TABLE user_secrets ADD COLUMN disabled_at timestamptz;
ALTER TABLE user_secrets ADD COLUMN enablement_rev bigint NOT NULL DEFAULT 0;
-- PRD #1732 D13: the credential revision a reading was polled at. A gauge row is
-- current only while it equals user_secrets.enablement_rev, so a reading from
-- before a disable is never shown or auto-selected after the re-enable. Existing
-- rows stamp 0, which is every existing credential's revision.
ALTER TABLE anthropic_rate_limits ADD COLUMN enablement_rev bigint NOT NULL DEFAULT 0;
CREATE INDEX runs_credential_disabled_promoter_idx ON runs (user_id, status_since, id)
    WHERE status = 'paused' AND hold_reason = 'credential_disabled';

-- +goose Down
DROP INDEX runs_credential_disabled_promoter_idx;
ALTER TABLE anthropic_rate_limits DROP COLUMN enablement_rev;
ALTER TABLE user_secrets DROP COLUMN enablement_rev;
ALTER TABLE user_secrets DROP COLUMN disabled_at;
