-- +goose Up
-- Agent template scopes (PRD #18 M6): agent_templates gains the same three-scope
-- model as skills (migration 00040) — builtin (shipped, reconciler-seeded),
-- global (admin-defined, visible to all), user (self-service, visible only to the
-- owner) — plus per-user ownership. is_builtin is KEPT as a compat column
-- (Decision 9): builtin seeding and ResetAgentTemplate keep their exact behavior,
-- and a CHECK ties it to scope so the two can never drift.
ALTER TABLE agent_templates
    ADD COLUMN scope   TEXT NOT NULL DEFAULT 'global' CHECK (scope IN ('builtin','global','user')),
    ADD COLUMN user_id UUID REFERENCES users (id) ON DELETE CASCADE;

-- Existing builtins carry is_builtin=true; migrate that into scope. Everything
-- else was admin-managed and shared → 'global' (the column default).
UPDATE agent_templates SET scope = 'builtin' WHERE is_builtin;

-- scope='user' <=> user_id set (mirrors skills' scope/user_id CHECK), and the
-- compat flag stays in lockstep with the scope so no code path can set one
-- without the other: the reconciler insert sets both; every other write leaves
-- is_builtin at its false default with scope 'global'/'user'.
ALTER TABLE agent_templates
    ADD CONSTRAINT agent_templates_user_scope_ck    CHECK ((scope = 'user') = (user_id IS NOT NULL)),
    ADD CONSTRAINT agent_templates_builtin_scope_ck CHECK (is_builtin = (scope = 'builtin'));

-- Replace the flat UNIQUE(name) with the skills-shaped partial uniques: builtin
-- and global share one namespace (so the reconciler keys on name and delivery
-- precedence is unambiguous); user templates are unique per owner. A user may
-- therefore own a "coder" that coexists with the builtin "coder".
ALTER TABLE agent_templates DROP CONSTRAINT agent_templates_name_key;
CREATE UNIQUE INDEX uq_agent_templates_shared_name ON agent_templates (name) WHERE scope <> 'user';
CREATE UNIQUE INDEX uq_agent_templates_user_name   ON agent_templates (user_id, name) WHERE scope = 'user';

-- +goose Down
DROP INDEX uq_agent_templates_user_name;
DROP INDEX uq_agent_templates_shared_name;
ALTER TABLE agent_templates ADD CONSTRAINT agent_templates_name_key UNIQUE (name);
ALTER TABLE agent_templates
    DROP CONSTRAINT agent_templates_builtin_scope_ck,
    DROP CONSTRAINT agent_templates_user_scope_ck;
ALTER TABLE agent_templates DROP COLUMN user_id;
ALTER TABLE agent_templates DROP COLUMN scope;
