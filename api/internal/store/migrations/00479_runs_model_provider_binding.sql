-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4320 M1: the per-run model provider binding columns, frozen at run creation by a later
-- milestone. Deliberately NO foreign keys: a revoked provider or key must not null or block a
-- bound run's row, and a FK would also lock the busy parent tables. This file touches runs only.
-- The CHECKs are NOT VALID here and validated in 00480.
ALTER TABLE runs
    ADD COLUMN model_provider_id uuid,
    ADD COLUMN model_provider_revision integer,
    ADD COLUMN model_provider_key_mode text,
    ADD COLUMN model_provider_key_secret_id uuid,
    ADD COLUMN model_provider_key_rev bigint,
    ADD COLUMN model_provider_limit_streak integer NOT NULL DEFAULT 0;

ALTER TABLE runs ADD CONSTRAINT runs_model_provider_key_mode_check
    CHECK (model_provider_key_mode IS NULL OR model_provider_key_mode IN ('user_key')) NOT VALID;
-- All five binding columns are set together or not at all.
ALTER TABLE runs ADD CONSTRAINT runs_model_provider_binding_check
    CHECK ((model_provider_id IS NULL) = (model_provider_revision IS NULL)
       AND (model_provider_id IS NULL) = (model_provider_key_mode IS NULL)
       AND (model_provider_id IS NULL) = (model_provider_key_secret_id IS NULL)
       AND (model_provider_id IS NULL) = (model_provider_key_rev IS NULL)) NOT VALID;
-- A provider-bound run is a Claude run and carries no vendor credential.
ALTER TABLE runs ADD CONSTRAINT runs_model_provider_exclusive_check
    CHECK (model_provider_key_mode IS NULL
       OR (harness = 'claude' AND anthropic_secret_id IS NULL AND codex_secret_id IS NULL
           AND credential_override_secret_id IS NULL)) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently drop a bound run's frozen provider binding (the 00198 convention):
-- the run would read back as a vendor run. The table is locked BEFORE the guard so a binding
-- created concurrently cannot land between the check and the column drop (ACCESS EXCLUSIVE blocks
-- every reader and writer, and is taken up front so no lock upgrade can deadlock).
-- The guard seq-scans runs: 00481's Down has already dropped idx_runs_model_provider by the
-- time this runs (Downs run in reverse), so no index serves the check; a one-off downgrade can pay it.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM runs WHERE model_provider_id IS NOT NULL) THEN
        RAISE EXCEPTION 'refusing to roll back 00479: run(s) are still bound to a model provider; delete them explicitly before downgrading';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE runs
    DROP CONSTRAINT runs_model_provider_exclusive_check,
    DROP CONSTRAINT runs_model_provider_binding_check,
    DROP CONSTRAINT runs_model_provider_key_mode_check,
    DROP COLUMN model_provider_limit_streak,
    DROP COLUMN model_provider_key_rev,
    DROP COLUMN model_provider_key_secret_id,
    DROP COLUMN model_provider_key_mode,
    DROP COLUMN model_provider_revision,
    DROP COLUMN model_provider_id;
