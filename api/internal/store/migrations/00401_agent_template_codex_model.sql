-- +goose Up
ALTER TABLE agent_templates ADD COLUMN codex_model TEXT;

-- Move only known Codex role pins; preserve provenance and timestamps.
UPDATE agent_templates
SET codex_model = btrim(model, E' \t\n\r\f\013'), model = NULL
WHERE btrim(model, E' \t\n\r\f\013') IN
    ('gpt-6-astra', 'gpt-5.6-sol', 'gpt-6-luna', 'gpt-6-sol', 'gpt-6.1-sol');

-- +goose Down
-- A row with both pins loses its Codex pin on rollback.
-- A Codex pin created after upgrade moves into model when model is NULL:
-- the old binary sends it to the Claude SDK on Claude runs. This deliberately
-- preserves Codex continuity after a schema rollback.
UPDATE agent_templates SET model = codex_model
WHERE model IS NULL AND codex_model IS NOT NULL;
ALTER TABLE agent_templates DROP COLUMN codex_model;
