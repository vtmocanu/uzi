-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE runs DROP CONSTRAINT runs_model_provider_exclusive_check;
ALTER TABLE runs ADD CONSTRAINT runs_model_provider_exclusive_check
    CHECK (model_provider_key_mode IS NULL
       OR (harness IN ('claude', 'codex') AND anthropic_secret_id IS NULL AND codex_secret_id IS NULL
           AND codex_auth_mode IS NULL AND credential_override_secret_id IS NULL)) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM runs WHERE model_provider_id IS NOT NULL AND harness = 'codex') THEN
        RAISE EXCEPTION 'refusing to roll back 00519: Codex provider runs still exist';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE runs DROP CONSTRAINT runs_model_provider_exclusive_check;
ALTER TABLE runs ADD CONSTRAINT runs_model_provider_exclusive_check
    CHECK (model_provider_key_mode IS NULL
       OR (harness = 'claude' AND anthropic_secret_id IS NULL AND codex_secret_id IS NULL
           AND credential_override_secret_id IS NULL));
