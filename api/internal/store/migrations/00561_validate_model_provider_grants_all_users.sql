-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_operator_all_users_check;

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- A guarded no-op: the constraint stays as 00560 left it. The guard exists so a rollback to a version
-- between 00560 and 00561 is refused while an all-users definition exists, in step with 00560. The table
-- is locked BEFORE the guard so a write cannot land between the check and the commit. Run a Down with the
-- API stopped: a transaction holding a lock on another table that then touches model_providers can
-- deadlock with this one (PostgreSQL aborts one side, nothing is lost, and lock_timeout bounds the wait).
LOCK TABLE model_providers IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM model_providers WHERE operator_all_users) THEN
        RAISE EXCEPTION 'refusing to roll back 00561: all-users organization definition(s) exist';
    END IF;
END
$$;
-- +goose StatementEnd
