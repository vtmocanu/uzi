-- +goose Up
-- Agent template store: the definitions the UI edits and PRD #4 renders into
-- Claude Code subagent files. `name` is the subagent identity (filename and
-- routing key), so it is UNIQUE and treated as immutable after creation.
-- Builtin rows (is_builtin=true) are seeded from Go-embedded definitions by a
-- startup reconciler; they are editable and resettable but never deletable, so
-- PRD #4 can always rely on the core roles existing.
CREATE TABLE agent_templates (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,          -- kebab-case; immutable after creation
    description TEXT NOT NULL,                 -- single sentence, used for routing
    model       TEXT,                          -- NULL = inherit; else alias or full model ID
    tools       JSONB,                         -- NULL = inherit all; else JSON array allowlist
    prompt_body TEXT NOT NULL,                 -- system prompt body (Markdown)
    is_builtin  BOOLEAN NOT NULL DEFAULT false,
    updated_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE agent_templates;
