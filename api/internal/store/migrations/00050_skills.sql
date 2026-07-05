-- +goose Up
-- Agent skills: named Markdown playbooks (SKILL.md) whose name+description sit
-- cheaply in an agent's context and whose body loads on demand (progressive
-- disclosure). Three server scopes: builtin (shipped with uzi, seeded like the
-- builtin agent templates), global (admin-defined, visible to all), user
-- (self-service, visible to the owner). The SKILL.md frontmatter is synthesized
-- at delivery, so only the body is stored here.
CREATE TABLE skills (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,        -- kebab-case ^[a-z0-9][a-z0-9-]{0,63}$; immutable after creation (skill identity)
    description TEXT NOT NULL,        -- one line; always in context — this is what the model routes on
    body        TEXT NOT NULL,        -- SKILL.md markdown body (frontmatter synthesized at delivery, never stored)
    scope       TEXT NOT NULL CHECK (scope IN ('builtin','global','user')),
    user_id     UUID REFERENCES users (id) ON DELETE CASCADE,
    updated_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((scope = 'user') = (user_id IS NOT NULL))
);

-- A builtin and a global can never share a name (both are shared scopes, so the
-- reconciler keys on (name, scope='builtin') and delivery precedence stays
-- unambiguous); user skills are unique per owner.
CREATE UNIQUE INDEX uq_skills_shared_name ON skills (name) WHERE scope <> 'user';
CREATE UNIQUE INDEX uq_skills_user_name   ON skills (user_id, name) WHERE scope = 'user';

-- Allocation of a skill to an agent template. user_id NULL = a shared,
-- admin-managed allocation; a non-NULL user_id is that user's private overlay.
CREATE TABLE agent_skill_allocations (
    template_id UUID NOT NULL REFERENCES agent_templates (id) ON DELETE CASCADE,
    skill_id    UUID NOT NULL REFERENCES skills (id) ON DELETE CASCADE,
    user_id     UUID REFERENCES users (id) ON DELETE CASCADE,  -- NULL = shared (admin-managed); else that user's private overlay
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Intentionally no surrogate PK: this unique index is the row identity. COALESCE
-- folds a NULL (shared) user_id to a fixed sentinel so a shared row and a user
-- overlay for the same (template, skill) can coexist while duplicates cannot.
CREATE UNIQUE INDEX uq_allocations ON agent_skill_allocations
    (template_id, skill_id, COALESCE(user_id, '00000000-0000-0000-0000-000000000000'));

-- Opt-in: load skills from the repo's own .claude/skills at run time. Default
-- off; the repo owner asserts the repo's review discipline per repo. Skills only
-- — the repo's hooks/settings/commands are never loaded.
ALTER TABLE repos ADD COLUMN repo_skills_enabled BOOLEAN NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE repos DROP COLUMN repo_skills_enabled;
DROP TABLE agent_skill_allocations;
DROP TABLE skills;
