-- +goose Up
-- PRD #3245: a per-user Codex lane for the judge model (users.codex_judge_model; NULL =
-- inherit) and an instance Codex key (app_settings.codex_judge_model, no seeded row).
--
-- Additive: one nullable column plus data copies. The Claude lane (users.judge_model and the
-- app_settings judge_model row) is left untouched: the copy never moves a value, so an
-- image-only rollback keeps today's behavior. The ALTER takes a brief lock on users, so the
-- statement times out rather than queue behind a long transaction and stall logins.
--
-- The literal id list below MUST equal "curated" in fixtures/codex-models/models.json; the
-- migration LiveDB test iterates agenttmpl.CuratedCodexModels, so an edit to the set before
-- landing reddens it. btrim removes the ASCII whitespace Go's strings.TrimSpace removes.
--
-- The migration number is a draft: it is renumbered above the live head at landing.
SET LOCAL lock_timeout = '5s';

ALTER TABLE users ADD COLUMN codex_judge_model text;

UPDATE users
   SET codex_judge_model = btrim(judge_model, E' \t\n\r\f\v')
 WHERE btrim(judge_model, E' \t\n\r\f\v') IN ('gpt-5.6-sol', 'gpt-6-astra', 'gpt-6-sol', 'gpt-6.1-sol');

INSERT INTO app_settings (key, value, updated_by, updated_at)
SELECT 'codex_judge_model', btrim(value, E' \t\n\r\f\v'), updated_by, updated_at
  FROM app_settings
 WHERE key = 'judge_model'
   AND btrim(value, E' \t\n\r\f\v') IN ('gpt-5.6-sol', 'gpt-6-astra', 'gpt-6-sol', 'gpt-6.1-sol')
ON CONFLICT (key) DO NOTHING;

-- +goose Down
SET LOCAL lock_timeout = '5s';

-- Lossy by design: one field cannot hold both lanes. A non-empty Codex instance value is
-- written to judge_model only when that key is absent or reset to ''; a non-empty Claude
-- value is never overwritten. The per-user Codex lane is dropped with the column.
INSERT INTO app_settings (key, value, updated_by, updated_at)
SELECT 'judge_model', value, updated_by, now()
  FROM app_settings
 WHERE key = 'codex_judge_model' AND value <> ''
ON CONFLICT (key) DO UPDATE
   SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()
 WHERE app_settings.value = '';

DELETE FROM app_settings WHERE key = 'codex_judge_model';

ALTER TABLE users DROP COLUMN codex_judge_model;
