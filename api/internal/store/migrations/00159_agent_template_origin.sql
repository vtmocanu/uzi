-- +goose Up
-- PRD #602 (M1): a scope-aware provenance column on agent_templates so boot-time
-- reconciliation (RefreshPristineBuiltin) never clobbers a synced or admin-edited
-- row. It extends PRD #275's boolean `customized` with a third state the boolean
-- cannot express — "not admin-edited, but not owned by the embedded default
-- either" (a synced builtin) — which without it would be overwritten back to the
-- embedded body on the very next boot. See adr/0602-agent-source-repo-sync.md,
-- "Provenance is a scope-aware `origin` column".
--
-- Nullable with no default: global/user rows stay NULL (provenance is a
-- product/admin-template concern), so a plain non-builtin insert needs no origin.
ALTER TABLE agent_templates ADD COLUMN origin TEXT;

-- Backfill existing builtin rows so they satisfy the CHECK added below: an
-- admin-edited builtin (customized=true) is 'admin', an untouched one is
-- 'embedded'. Global/user rows are left NULL.
UPDATE agent_templates SET origin = CASE WHEN customized THEN 'admin' ELSE 'embedded' END WHERE scope = 'builtin';

-- The scope-aware provenance CHECK. This predicate is load-bearing and must match
-- adr/0602-agent-source-repo-sync.md verbatim: embedded/admin are builtin-only,
-- synced is legal on builtin OR global, and a user row is always NULL.
ALTER TABLE agent_templates ADD CONSTRAINT agent_templates_origin_ck CHECK (
     (scope = 'builtin' AND origin IS NOT NULL AND origin IN ('embedded','synced','admin'))
  OR (scope = 'global'  AND (origin IS NULL OR origin = 'synced'))
  OR (scope = 'user'    AND origin IS NULL)
);

-- +goose Down
ALTER TABLE agent_templates DROP CONSTRAINT agent_templates_origin_ck;
ALTER TABLE agent_templates DROP COLUMN origin;
