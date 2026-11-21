-- +goose Up
-- First statement: the ALTER below waits for the agent_templates ACCESS EXCLUSIVE lock, so the
-- bound covers it. A lock wait past it fails the migration, which can be rerun.
SET LOCAL lock_timeout = '5s';

-- PRD #3258: per-harness reasoning effort pins on a template, beside model and codex_model.
-- Nullable text columns; NULL means no pin. The CHECK mirrors agenttmpl.EffortLevels and
-- deliberately omits 'runtime' (a user preference value, never a template pin). The columns are
-- new and all NULL, so adding the CHECKs costs one verification scan of a small table (no
-- rewrite) and they are not added NOT VALID.
ALTER TABLE agent_templates
 ADD COLUMN effort text,
 ADD COLUMN codex_effort text,
 ADD CONSTRAINT agent_templates_effort_ck CHECK (effort IN ('low', 'medium', 'high', 'xhigh', 'max')),
 ADD CONSTRAINT agent_templates_codex_effort_ck CHECK (codex_effort IN ('low', 'medium', 'high', 'xhigh', 'max'));

-- +goose Down
-- Down discards stored effort pins. Rollback edge: an image rolled back to a binary that predates
-- these columns has a SameContent that ignores effort, so an admin save of a builtin on the old
-- image leaves customized=false; a later upgraded boot may then refresh the effort back to the
-- shipped NULL. The same edge holds during a rolling deploy, while old and new pods share the
-- migrated schema.
SET LOCAL lock_timeout = '5s';
ALTER TABLE agent_templates
 DROP CONSTRAINT agent_templates_codex_effort_ck,
 DROP CONSTRAINT agent_templates_effort_ck,
 DROP COLUMN codex_effort,
 DROP COLUMN effort;
