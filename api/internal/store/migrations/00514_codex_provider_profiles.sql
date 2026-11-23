-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE model_providers DROP CONSTRAINT model_providers_engine_check;
ALTER TABLE model_providers ADD CONSTRAINT model_providers_engine_check CHECK (engine IN ('claude', 'codex'));
ALTER TABLE model_providers ADD CONSTRAINT model_providers_id_engine_key UNIQUE (id, engine);
ALTER TABLE model_provider_revisions ADD COLUMN engine text NOT NULL DEFAULT 'claude';
ALTER TABLE model_provider_revisions ADD CONSTRAINT model_provider_revisions_engine_fkey
    FOREIGN KEY (provider_id, engine) REFERENCES model_providers (id, engine);
ALTER TABLE model_provider_revisions DROP CONSTRAINT model_provider_revisions_protocol_check;
ALTER TABLE model_provider_revisions ADD CONSTRAINT model_provider_revisions_protocol_check
    CHECK ((engine = 'claude' AND protocol = 'anthropic_messages')
        OR (engine = 'codex' AND protocol = 'openai_responses' AND auth_style = 'bearer'));

-- +goose Down
SET LOCAL lock_timeout = '5s';
LOCK TABLE model_providers IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM model_providers WHERE engine = 'codex') THEN
        RAISE EXCEPTION 'refusing to roll back 00514: Codex providers still exist';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE model_provider_revisions DROP CONSTRAINT model_provider_revisions_engine_fkey;
ALTER TABLE model_provider_revisions DROP CONSTRAINT model_provider_revisions_protocol_check;
ALTER TABLE model_provider_revisions DROP COLUMN engine;
ALTER TABLE model_provider_revisions ADD CONSTRAINT model_provider_revisions_protocol_check CHECK (protocol IN ('anthropic_messages'));
ALTER TABLE model_providers DROP CONSTRAINT model_providers_id_engine_key;
ALTER TABLE model_providers DROP CONSTRAINT model_providers_engine_check;
ALTER TABLE model_providers ADD CONSTRAINT model_providers_engine_check CHECK (engine IN ('claude'));
