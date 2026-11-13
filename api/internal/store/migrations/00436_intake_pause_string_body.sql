-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Keep the existing function identity and grants while removing parsed-body
-- dependencies that block defaulted ADD COLUMN on patched PostgreSQL minors (#4273).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_run_admitted_during_intake_pause(r runs) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE AS $$
 SELECT r.claim_generation > 0 OR r.kind = 'cross_check';
$$;
-- +goose StatementEnd

-- +goose Down
-- Intentionally keep the compatible body. Restoring BEGIN ATOMIC would block
-- future runs table alterations; migration 00358 still owns function removal.
