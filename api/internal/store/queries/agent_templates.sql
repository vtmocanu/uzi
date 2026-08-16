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
-- never touched here. This is the admin-edit path (PRD #275): the handler now
-- passes @customized explicitly rather than the query hardcoding it. A builtin
-- whose submitted content is byte-identical (per agenttmpl.SameContent) to the
-- shipped builtin is stored customized=false, so saving the shipped body is
-- idempotent with Reset (issue #339) and the row keeps tracking future shipped
-- changes via the boot-time pristine refresh; every other write passes
-- customized=true, opting a builtin out of that refresh until it is Reset. The
-- reset path uses ResetBuiltinAgentTemplate instead so a reset returns to
-- pristine (customized=false) rather than marking it.
UPDATE agent_templates
SET description = @description,
    model = @model,
    tools = @tools,
    prompt_body = @prompt_body,
    updated_by = @updated_by,
    customized = @customized,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: ResetBuiltinAgentTemplate :one
-- Reset-to-default path (PRD #275): re-apply a builtin's embedded definition AND
-- return the row to pristine (customized=false) so it resumes tracking upstream
-- shipped changes on future boots. Distinct from UpdateAgentTemplate, which marks
-- the row customized. updated_by/updated_at record who reset it and when.
UPDATE agent_templates
SET description = @description,
    model = @model,
    tools = @tools,
    prompt_body = @prompt_body,
    updated_by = @updated_by,
    customized = false,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: DeleteAgentTemplate :execrows
-- Non-builtins only; the is_builtin guard is belt-and-suspenders (the handler
-- returns 409 before calling this, and applies scope-based authz first).
DELETE FROM agent_templates WHERE id = @id AND is_builtin = false;

-- name: InsertBuiltinAgentTemplate :execrows
-- Idempotent seed used by the startup reconciler: insert a MISSING builtin only;
-- an existing row is left untouched here (DO NOTHING). Applying a shipped body
-- change to an already-seeded row is a SEPARATE concern handled by
-- RefreshPristineBuiltin (PRD #275) — pristine rows track upstream, admin-edited
-- rows are preserved — kept a distinct statement so this insert stays the sole
-- trigger for the default-allocation seed. scope='builtin' is set explicitly so it
-- satisfies builtin_scope_ck; the conflict target is the shared-namespace partial
-- unique (uq_agent_templates_shared_name), NOT the bare (name) — a user-scoped
-- template of the same name must never block a builtin seed.
INSERT INTO agent_templates (name, description, model, tools, prompt_body, is_builtin, scope)
VALUES (@name, @description, @model, @tools, @prompt_body, true, 'builtin')
ON CONFLICT (name) WHERE scope <> 'user' DO NOTHING;

-- name: RefreshPristineBuiltin :execrows
-- Boot-time delivery of shipped builtin-prompt improvements to PRISTINE rows only
-- (PRD #275 M4b). Run per builtin by the reconciler AFTER InsertBuiltinAgentTemplate,
-- as a SEPARATE statement (not ON CONFLICT DO UPDATE) so the insert-only default-
-- allocation seed — gated on the insert's own rowcount — is never triggered by a
-- refresh. Only scope='builtin' + customized=false (pristine, admin never touched)
-- rows are updated; admin-customized rows and user/global same-name rows are left
-- exactly as they are. The IS DISTINCT FROM content guard makes the write (and the
-- updated_at bump) a no-op unless the embedded body actually changed, so an
-- unchanged builtin is not rewritten on every boot.
UPDATE agent_templates
SET description = @description,
    model = @model,
    tools = @tools,
    prompt_body = @prompt_body,
    updated_at = now()
WHERE name = @name AND scope = 'builtin' AND customized = false
  AND (description, model, tools, prompt_body)
      IS DISTINCT FROM (@description, @model, @tools, @prompt_body);
