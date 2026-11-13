-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Issue #4201. The ordinal columns and the worker nonce are in 00439..00441, one busy table per
-- migration; this file only creates the table, whose run_id foreign key locks runs. The secret_id
-- foreign key (which locks user_secrets) is 00443: holding runs while waiting on user_secrets cycles
-- with a token delete, whose ON DELETE SET NULL cascade takes them in the other order.
--
-- The in-place Claude credential switch apply journals each applied switch as a
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
    secret_id uuid, -- FK to user_secrets added in 00432 (one busy parent table per migration)
    secret_enablement_rev bigint NOT NULL,
    label text,
    select_reason text,
    message_seq integer NOT NULL,
    old_cumulative jsonb CHECK (old_cumulative IS NULL OR jsonb_typeof(old_cumulative) = 'object'),
    applied_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, claim_generation, ordinal)
);
CREATE INDEX run_credential_switch_epochs_secret_idx ON run_credential_switch_epochs (secret_id);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE IF EXISTS run_credential_switch_epochs;
