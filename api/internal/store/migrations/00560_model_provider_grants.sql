-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4540 M2: recipient grants for an organization-funded definition. operator_all_users makes every
-- user a recipient of the definition (the default is no recipients); model_provider_grants names
-- individual recipients. The all-users CHECK is NOT VALID (no full scan under the lock) and validated in
-- 00561. A plain equality would let a personal row with a NULL credential_mode pass, hence IS NOT DISTINCT FROM.
ALTER TABLE model_providers ADD COLUMN operator_all_users boolean NOT NULL DEFAULT false;
ALTER TABLE model_providers ADD CONSTRAINT model_providers_operator_all_users_check
    CHECK (NOT operator_all_users OR credential_mode IS NOT DISTINCT FROM 'operator_key') NOT VALID;

-- The grants table is new and empty, so no backfill and no CONCURRENTLY index. Who granted and when lives
-- in provider_audit, not here. The user_id index serves the user-delete cascade and per-user reads, which
-- would otherwise scan every grant row.
CREATE TABLE model_provider_grants (
    provider_id uuid NOT NULL REFERENCES model_providers(id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (provider_id, user_id)
);
CREATE INDEX model_provider_grants_user_id_idx ON model_provider_grants (user_id);

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently destroy a recipient grant or an all-users switch. Every table the guard
-- reads or the drops touch is locked BEFORE the guard, parents first: model_providers, users, then the
-- grants table (whose foreign keys reference both). ACCESS EXCLUSIVE conflicts with every other lock mode
-- and is held to commit, so no grant or all-users write can land between the guard and the drops. Run a
-- Down with the API stopped: a transaction holding a lock on users or grants that then touches
-- model_providers can deadlock with this one (PostgreSQL aborts one side, nothing is lost, and
-- lock_timeout bounds the wait).
LOCK TABLE model_providers IN ACCESS EXCLUSIVE MODE;
LOCK TABLE users IN ACCESS EXCLUSIVE MODE;
LOCK TABLE model_provider_grants IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM model_provider_grants) THEN
        RAISE EXCEPTION 'refusing to roll back 00560: recipient grant(s) exist';
    END IF;
    IF EXISTS (SELECT 1 FROM model_providers WHERE operator_all_users) THEN
        RAISE EXCEPTION 'refusing to roll back 00560: all-users organization definition(s) exist';
    END IF;
END
$$;
-- +goose StatementEnd
-- No CASCADE: an unexpected dependent surfaces as an error. The index goes with the table.
DROP TABLE model_provider_grants;
ALTER TABLE model_providers DROP CONSTRAINT model_providers_operator_all_users_check;
ALTER TABLE model_providers DROP COLUMN operator_all_users;
