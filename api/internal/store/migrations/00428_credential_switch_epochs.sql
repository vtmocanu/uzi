-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Issue #4201: the in-place Claude credential switch apply journals each applied switch as a
-- child epoch of the claim it happened in. run_credential_epochs (00233) keeps one row per claim
-- generation; this table holds the ordinal 1..n switches applied inside one generation, each
-- tagged with the switch operation id (UNIQUE, so a retried apply is idempotent) and the
-- message_seq at which the new credential took effect. old_cumulative optionally snapshots the
-- usage totals of the credential being replaced.
CREATE TABLE run_credential_switch_epochs (
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    claim_generation bigint NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal >= 1),
    op_id uuid NOT NULL UNIQUE,
    worker_id uuid,
    secret_id uuid REFERENCES user_secrets(id) ON DELETE SET NULL,
    secret_enablement_rev bigint NOT NULL,
    label text,
    select_reason text,
    message_seq integer NOT NULL,
    old_cumulative jsonb CHECK (old_cumulative IS NULL OR jsonb_typeof(old_cumulative) = 'object'),
    applied_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, claim_generation, ordinal)
);
CREATE INDEX run_credential_switch_epochs_secret_idx ON run_credential_switch_epochs (secret_id);

-- Per-epoch attribution of messages and usage rows; NULL means "the claim's base epoch".
ALTER TABLE run_messages ADD COLUMN credential_epoch_ordinal integer;
ALTER TABLE run_usage ADD COLUMN credential_epoch_ordinal integer;

-- The registration nonce the worker presented with its last apply acknowledgement.
ALTER TABLE workers ADD COLUMN credential_switch_apply_nonce text;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE workers DROP COLUMN IF EXISTS credential_switch_apply_nonce;
ALTER TABLE run_usage DROP COLUMN IF EXISTS credential_epoch_ordinal;
ALTER TABLE run_messages DROP COLUMN IF EXISTS credential_epoch_ordinal;
DROP TABLE IF EXISTS run_credential_switch_epochs;
