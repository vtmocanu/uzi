-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_credential_mode_check;
ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_shared_mode_check;
ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_shared_engine_check;
ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_scope_owner_check;
ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_owner_not_nil_check;
-- The validated scope_owner_check already proves the column is never NULL, so PostgreSQL skips the
-- table scan for SET NOT NULL.
ALTER TABLE model_providers ALTER COLUMN scope_owner_id SET NOT NULL;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE model_providers ALTER COLUMN scope_owner_id DROP NOT NULL;
