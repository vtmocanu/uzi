-- name: ListAgentTemplates :many
SELECT * FROM agent_templates ORDER BY name;

-- name: GetAgentTemplate :one
SELECT * FROM agent_templates WHERE id = $1;

-- name: GetAgentTemplateByName :one
SELECT * FROM agent_templates WHERE name = $1;

-- name: CreateAgentTemplate :one
-- Admin-created template. is_builtin is always false here; builtins are seeded
-- only by the startup reconciler (InsertBuiltinAgentTemplate).
INSERT INTO agent_templates (name, description, model, tools, prompt_body, updated_by)
VALUES (@name, @description, @model, @tools, @prompt_body, @updated_by)
RETURNING *;

-- name: UpdateAgentTemplate :one
-- Edits the mutable fields. name and is_builtin are immutable and never touched
-- here. Also used by the reset path to re-apply a builtin's embedded definition.
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
-- returns 409 before calling this).
DELETE FROM agent_templates WHERE id = @id AND is_builtin = false;

-- name: InsertBuiltinAgentTemplate :execrows
-- Idempotent seed used by the startup reconciler: insert a missing builtin,
-- never overwrite an existing row (admin edits survive restarts).
INSERT INTO agent_templates (name, description, model, tools, prompt_body, is_builtin)
VALUES (@name, @description, @model, @tools, @prompt_body, true)
ON CONFLICT (name) DO NOTHING;
