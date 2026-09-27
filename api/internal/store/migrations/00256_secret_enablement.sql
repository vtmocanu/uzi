-- +goose Up
ALTER TABLE user_secrets ADD COLUMN disabled_at timestamptz;
ALTER TABLE user_secrets ADD COLUMN enablement_rev bigint NOT NULL DEFAULT 0;
-- PRD #1732 D13: the credential revision a reading was polled at. A gauge row is
-- current only while it equals user_secrets.enablement_rev, so a reading from
-- before a disable is never shown or auto-selected after the re-enable. Existing
-- rows stamp 0, which is every existing credential's revision.
ALTER TABLE anthropic_rate_limits ADD COLUMN enablement_rev bigint NOT NULL DEFAULT 0;
-- The Codex sibling (PRD #1732 D13, M4): a Codex reading describes an ACCOUNT, so it is
-- stamped with the (alias id:enablement_rev, ...) list of the account's linked aliases it
-- was polled under (the poll's fenced enablement_sig). It is current only while every
-- currently ENABLED linked alias appears in it at its current revision. NULL marks a row
-- written before this column existed, when every credential was at revision 0; readers
-- treat it as covering exactly the revision-0 aliases, so no backfill is needed. That
-- makes a NULL row tolerate a newly linked rev-0 alias, while a stamped row does not
-- (the new alias's id is not in its list): the stamped reading is not current until the
-- next successful poll re-stamps it.
ALTER TABLE codex_account_rate_limits ADD COLUMN enablement_sig text;
CREATE INDEX runs_credential_disabled_promoter_idx ON runs (user_id, status_since, id)
    WHERE status = 'paused' AND hold_reason = 'credential_disabled';

-- +goose Down
DROP INDEX runs_credential_disabled_promoter_idx;
ALTER TABLE codex_account_rate_limits DROP COLUMN enablement_sig;
ALTER TABLE anthropic_rate_limits DROP COLUMN enablement_rev;
ALTER TABLE user_secrets DROP COLUMN enablement_rev;
ALTER TABLE user_secrets DROP COLUMN disabled_at;
