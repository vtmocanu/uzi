-- +goose Up
ALTER TABLE repos ADD COLUMN ignored_inferred_capabilities text[] NOT NULL DEFAULT '{}';
-- New runs capture the repo policy in the same INSERT snapshot as the declared hint.
-- Empty defaults preserve existing runs and repo-less run kinds.
-- Keep the existing function identity and grants while removing parsed-body
-- dependencies that block defaulted ADD COLUMN on patched PostgreSQL minors (#4273).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION fn_run_admitted_during_intake_pause(r runs) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE AS $$
 SELECT r.claim_generation > 0 OR r.kind = 'cross_check';
$$;
-- +goose StatementEnd

ALTER TABLE runs ADD COLUMN ignored_inferred_capabilities text[] NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE runs DROP COLUMN ignored_inferred_capabilities;
ALTER TABLE repos DROP COLUMN ignored_inferred_capabilities;
