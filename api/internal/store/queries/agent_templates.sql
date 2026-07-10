-- name: ListAgentTemplates :many
-- Unfiltered list of every template (all scopes). Claim assembly uses it until
-- M7 replaces it with the allocation-resolving list; the HTTP list handler uses
-- the viewer-scoped query below so a user never sees another user's templates.
SELECT * FROM agent_templates ORDER BY name;

-- name: ListAgentTemplatesForViewer :many
-- Read authz mirror of ListSkillsForViewer: builtin + global are visible to
-- everyone; a user's own templates are visible only to that user; admins see all
-- scopes. Copying the all-shared ListAgentTemplates read verbatim would leak
-- private user templates.
SELECT * FROM agent_templates
WHERE sqlc.arg(is_admin)::boolean
   OR scope IN ('builtin', 'global')
   OR (scope = 'user' AND user_id = sqlc.arg(viewer_id))
ORDER BY scope, name;

-- name: GetAgentTemplate :one
-- Unfiltered fetch for the write path: the handler loads the row, then applies
-- the scope-based write authz in Go (builtin/global admin-only, user owner-only)
-- and maps an unauthorized user-scope row to 404 so existence never leaks.
SELECT * FROM agent_templates WHERE id = $1;

-- name: GetAgentTemplateForViewer :one
-- Single-template read with the same visibility rule as ListAgentTemplatesForViewer.
SELECT * FROM agent_templates
WHERE id = sqlc.arg(id)
  AND (sqlc.arg(is_admin)::boolean
       OR scope IN ('builtin', 'global')
       OR (scope = 'user' AND user_id = sqlc.arg(viewer_id)));

-- name: GetSharedAgentTemplateByName :one
-- Shared-namespace lookup for the reconciler's shadow-warning classification.
-- Scoped to scope <> 'user' so it is unique (uq_agent_templates_shared_name):
-- post-00047 a bare name is NOT unique, and a user's same-name template could
-- otherwise win the QueryRow and trigger a false "builtin shadowed" warning.
SELECT * FROM agent_templates WHERE name = $1 AND scope <> 'user';

-- name: CreateAgentTemplate :one
-- Create a global (admin) or user (owner) template. scope='builtin' is never
-- created here (is_builtin stays false, kept in lockstep by the builtin_scope_ck);
-- builtins are seeded only by the startup reconciler (InsertBuiltinAgentTemplate).
INSERT INTO agent_templates (name, description, model, tools, prompt_body, scope, user_id, updated_by)
VALUES (@name, @description, @model, @tools, @prompt_body, @scope, @user_id, @updated_by)
RETURNING *;

-- name: UpdateAgentTemplate :one
-- Edits the mutable fields. name, scope, user_id and is_builtin are immutable and
-- never touched here. Also used by the reset path to re-apply a builtin's
-- embedded definition.
UPDATE agent_templates
SET description = @description,
    model = @model,
    tools = @tools,
    prompt_body = @prompt_body,
    updated_by = @updated_by,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: DeleteAgentTemplate :execrows
-- Non-builtins only; the is_builtin guard is belt-and-suspenders (the handler
-- returns 409 before calling this, and applies scope-based authz first).
DELETE FROM agent_templates WHERE id = @id AND is_builtin = false;

-- name: InsertBuiltinAgentTemplate :execrows
-- Idempotent seed used by the startup reconciler: insert a missing builtin,
-- never overwrite an existing row (admin edits survive restarts). scope='builtin'
-- is set explicitly so it satisfies builtin_scope_ck; the conflict target is the
-- shared-namespace partial unique (uq_agent_templates_shared_name), NOT the bare
-- (name) — a user-scoped template of the same name must never block a builtin seed.
INSERT INTO agent_templates (name, description, model, tools, prompt_body, is_builtin, scope)
VALUES (@name, @description, @model, @tools, @prompt_body, true, 'builtin')
ON CONFLICT (name) WHERE scope <> 'user' DO NOTHING;
