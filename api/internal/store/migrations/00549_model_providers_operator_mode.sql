-- +goose Up
SET LOCAL lock_timeout = '5s';

-- PRD #4540 M1: an organization-funded shared definition. A shared definition (owner_user_id NULL)
-- may now carry credential_mode 'operator_key' as well as 'per_user_key': its one credential is an
-- organization key held in model_provider_operator_keys (00551), never a recipient's key. The mode
-- check is replaced in one statement so no window exists without it. NOT VALID (no full scan under
-- the lock), validated in 00550. The new engine check keeps an operator definition Claude only; it is
-- NOT VALID for the same reason and validated in 00550.
ALTER TABLE model_providers
    DROP CONSTRAINT model_providers_credential_mode_check,
    ADD CONSTRAINT model_providers_credential_mode_check
        CHECK (credential_mode IN ('per_user_key', 'operator_key')) NOT VALID;
ALTER TABLE model_providers ADD CONSTRAINT model_providers_operator_engine_check
    CHECK (credential_mode IS DISTINCT FROM 'operator_key' OR engine = 'claude') NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Refuse rather than silently destroy or orphan an organization-funded definition. The table is
-- locked BEFORE the guard so an operator definition created concurrently cannot land between the
-- check and the constraint change. Run a Down with the API stopped: a transaction holding a lock on
-- another table that then touches model_providers can deadlock with this one (PostgreSQL aborts one
-- side, nothing is lost, and lock_timeout bounds the wait).
LOCK TABLE model_providers IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM model_providers WHERE credential_mode = 'operator_key') THEN
        RAISE EXCEPTION 'refusing to roll back 00549: operator-key provider definition(s) exist';
    END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE model_providers DROP CONSTRAINT model_providers_operator_engine_check;
-- Restore 00524's check (validated, as 00525 left it) under the same name.
ALTER TABLE model_providers
    DROP CONSTRAINT model_providers_credential_mode_check,
    ADD CONSTRAINT model_providers_credential_mode_check
        CHECK (credential_mode IN ('per_user_key'));
