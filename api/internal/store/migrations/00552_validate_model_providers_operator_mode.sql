-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_credential_mode_check;
ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_operator_engine_check;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- A guarded no-op: the constraints stay as 00551 left them. The guard exists so a rollback to a
-- version between 00551 and 00552 is refused while an operator definition exists, in step with 00551.
-- The table is locked BEFORE the guard so a definition cannot land between the check and the commit.
-- Run a Down with the API stopped: a transaction holding a lock on another table that then touches
-- model_providers can deadlock with this one (PostgreSQL aborts one side, nothing is lost, and
-- lock_timeout bounds the wait).
LOCK TABLE model_providers IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM model_providers WHERE credential_mode = 'operator_key') THEN
        RAISE EXCEPTION 'refusing to roll back 00552: operator-key provider definition(s) exist';
    END IF;
END
$$;
-- +goose StatementEnd
