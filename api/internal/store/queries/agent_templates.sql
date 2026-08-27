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
    -- Keep a builtin's origin in lockstep with customized (PRD #602 M1): a genuine
    -- admin edit is 'admin', saving the shipped body byte-for-byte returns it to
    -- 'embedded' (idempotent with Reset). A non-builtin (global/user) row keeps its
    -- existing origin — NULL — which the CHECK requires, so this never sets an
    -- illegal origin on it.
    origin = CASE WHEN scope = 'builtin' THEN (CASE WHEN @customized::boolean THEN 'admin' ELSE 'embedded' END) ELSE origin END,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: ResetBuiltinAgentTemplate :one
-- Reset-to-default path (PRD #275): re-apply a builtin's embedded definition AND
-- return the row to pristine (customized=false) so it resumes tracking upstream
-- shipped changes on future boots. Distinct from UpdateAgentTemplate, which marks
-- the row customized. updated_by/updated_at record who reset it and when. The
-- scope='builtin' guard (PRD #602 M4) makes this in-query consistent with the other
-- synced-apply statements (all carry a scope/origin guard): it sets origin='embedded',
-- which the CHECK permits only on a builtin row, so keying on a non-builtin id must
-- match zero rows rather than attempt an illegal write. Both callers only ever pass a
-- builtin id (the admin reset endpoint validates is_builtin first; the M4 de-provision
-- reset routes only a scope='builtin' row here), so this changes no behavior.
UPDATE agent_templates
SET description = @description,
    model = @model,
    tools = @tools,
    prompt_body = @prompt_body,
    updated_by = @updated_by,
    customized = false,
    origin = 'embedded',
    updated_at = now()
WHERE id = @id AND scope = 'builtin'
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
-- template of the same name must never block a builtin seed. origin='embedded'
-- marks it as owned by the shipped default (PRD #602 M1), so RefreshPristineBuiltin
-- can keep tracking it; without it a freshly-seeded builtin would have NULL origin
-- and violate agent_templates_origin_ck.
INSERT INTO agent_templates (name, description, model, tools, prompt_body, is_builtin, scope, origin)
VALUES (@name, @description, @model, @tools, @prompt_body, true, 'builtin', 'embedded')
ON CONFLICT (name) WHERE scope <> 'user' DO NOTHING;

-- name: ApplySyncedOverrideBuiltin :one
-- Apply a synced role over an EXISTING builtin row (PRD #602 M4, case "override"):
-- overwrite the four mutable columns with the synced role's fields and flip origin
-- to 'synced'. customized is forced false — a synced override is not an admin edit,
-- so the row stays reset-able to embedded (ResetBuiltinAgentTemplate) and its
-- shipped-default compare uses the synced source. Keyed by id AND scope='builtin'
-- as belt-and-suspenders; the applier only calls this for a row it read as builtin.
UPDATE agent_templates
SET description = @description,
    model = @model,
    tools = @tools,
    prompt_body = @prompt_body,
    origin = 'synced',
    customized = false,
    updated_by = @updated_by,
    updated_at = now()
WHERE id = @id AND scope = 'builtin'
RETURNING *;

-- name: InsertSyncedGlobalTemplate :one
-- Insert a synced-only NEW role (PRD #602 M4, case "add"): a deletable/de-provision-
-- able global row, NOT a builtin (the widened 00159 CHECK permits origin='synced' on
-- scope='global'). is_builtin stays false to satisfy builtin_scope_ck. The caller
-- seeds a default allocation (SeedSharedTemplateAllocationByName) right after so the
-- role actually reaches a claim — table presence alone does not run it.
INSERT INTO agent_templates (name, description, model, tools, prompt_body, is_builtin, scope, origin, updated_by)
VALUES (@name, @description, @model, @tools, @prompt_body, false, 'global', 'synced', @updated_by)
RETURNING *;

-- name: UpdateSyncedGlobalTemplate :one
-- Update an existing synced-only global role in place (PRD #602 M4, case
-- "update-synced-global"): the four mutable columns change, origin STAYS 'synced'.
-- Keyed on name+scope='global'+origin='synced' so it can NEVER touch an admin's
-- global row (origin NULL) that happens to share the name — that case is classified
-- a conflict and skipped, never routed here.
UPDATE agent_templates
SET description = @description,
    model = @model,
    tools = @tools,
    prompt_body = @prompt_body,
    updated_by = @updated_by,
    updated_at = now()
WHERE name = @name AND scope = 'global' AND origin = 'synced'
RETURNING *;

-- name: DeleteSyncedGlobalTemplate :execrows
-- De-provision a synced-only global role removed from the source (PRD #602 M4, case
-- "remove"): delete the row, which cascades its allocation. Scoped to
-- scope='global'+origin='synced' so an admin-authored global (origin NULL) is never
-- deleted even under a name collision.
DELETE FROM agent_templates WHERE name = @name AND scope = 'global' AND origin = 'synced';

-- name: RefreshPristineBuiltin :execrows
-- Boot-time delivery of shipped builtin-prompt improvements to PRISTINE rows only
-- (PRD #275 M4b). Run per builtin by the reconciler AFTER InsertBuiltinAgentTemplate,
-- as a SEPARATE statement (not ON CONFLICT DO UPDATE) so the insert-only default-
-- allocation seed — gated on the insert's own rowcount — is never triggered by a
-- refresh. Only scope='builtin' + customized=false (pristine, admin never touched)
-- rows are updated; admin-customized rows and user/global same-name rows are left
-- exactly as they are. The IS DISTINCT FROM content guard makes the write (and the
-- updated_at bump) a no-op unless the embedded body actually changed, so an
-- unchanged builtin is not rewritten on every boot. The added origin='embedded'
-- guard (PRD #602 M1) makes a synced (origin='synced') or admin-edited
-- (origin='admin') builtin row structurally unreachable by the boot refresh, so
-- sync and boot-reconcile never fight over the same row. origin is deliberately
-- NOT in the SET list nor the IS DISTINCT FROM tuple: this query only ever refreshes
-- an already-embedded row, which keeps its origin unchanged.
UPDATE agent_templates
SET description = @description,
    model = @model,
    tools = @tools,
    prompt_body = @prompt_body,
    updated_at = now()
WHERE name = @name AND scope = 'builtin' AND customized = false AND origin = 'embedded'
  AND (description, model, tools, prompt_body)
      IS DISTINCT FROM (@description, @model, @tools, @prompt_body);
