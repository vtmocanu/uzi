-- Agent template allocations (PRD #18 M7) ----------------------------------

-- name: ListTemplateAllocationsForViewer :many
-- Every template visible to the viewer, with its allocation state: whether a
-- global-default (shared) row exists, and the viewer's own overlay value (NULL =
-- no overlay). The delivered decision is my_override when present, else
-- global_default (see ListClaimAgentTemplates). Same visibility rule as
-- ListAgentTemplatesForViewer so a user never sees another user's templates.
SELECT t.id, t.name, t.description, t.scope, t.is_builtin,
       (g.template_id IS NOT NULL)::boolean AS global_default,
       uo.enabled AS my_override
FROM agent_templates t
LEFT JOIN agent_template_allocations g  ON g.template_id = t.id AND g.user_id IS NULL
LEFT JOIN agent_template_allocations uo ON uo.template_id = t.id AND uo.user_id = sqlc.arg(viewer_id)
WHERE sqlc.arg(is_admin)::boolean
   OR t.scope IN ('builtin', 'global')
   OR (t.scope = 'user' AND t.user_id = sqlc.arg(viewer_id))
ORDER BY t.scope, t.name;

-- name: ListClaimAgentTemplates :many
-- The templates delivered to a claim for @user_id: visible to the user AND
-- resolved as allocated. A user overlay (enabled) wins; absent an overlay, the
-- global default (user_id NULL) decides; absent both, the template is not
-- delivered. Shared rows are always enabled=true, so COALESCE(g.enabled, false)
-- reduces to "a global-default row exists". Replaces the all-templates claim read.
SELECT t.* FROM agent_templates t
LEFT JOIN agent_template_allocations uo ON uo.template_id = t.id AND uo.user_id = sqlc.arg(user_id)
LEFT JOIN agent_template_allocations g  ON g.template_id = t.id AND g.user_id IS NULL
WHERE (t.scope IN ('builtin', 'global') OR (t.scope = 'user' AND t.user_id = sqlc.arg(user_id)))
  AND CASE WHEN uo.template_id IS NOT NULL THEN uo.enabled ELSE COALESCE(g.enabled, false) END
ORDER BY t.name;

-- name: DeleteSharedTemplateAllocations :exec
-- Clear the whole global-default set (admin replace-set half).
DELETE FROM agent_template_allocations WHERE user_id IS NULL;

-- name: InsertSharedTemplateAllocation :exec
-- Mark a builtin/global template as a global default. Idempotent.
INSERT INTO agent_template_allocations (template_id, user_id, enabled)
VALUES (@template_id, NULL, true)
ON CONFLICT DO NOTHING;

-- name: DeleteUserTemplateAllocations :exec
-- Clear the caller's whole overlay (user replace-set half).
DELETE FROM agent_template_allocations WHERE user_id = @user_id;

-- name: InsertUserTemplateAllocation :exec
-- Set one of the caller's overlay decisions (enabled true = force-on, false =
-- force-off). Callers delete-then-insert for replace semantics, so DO NOTHING on
-- a duplicate (deduped caller-side anyway).
INSERT INTO agent_template_allocations (template_id, user_id, enabled)
VALUES (@template_id, @user_id, @enabled)
ON CONFLICT DO NOTHING;

-- name: SeedSharedTemplateAllocationByName :exec
-- Auto-seed a builtin's global-default row the first time the reconciler inserts
-- that builtin (gated on the insert actually happening, so an admin's later
-- removal is never re-added on a subsequent boot). Keyed by name over the shared
-- namespace so it targets the builtin/global row, never a user template.
INSERT INTO agent_template_allocations (template_id, user_id, enabled)
SELECT id, NULL, true FROM agent_templates WHERE name = @name AND scope <> 'user'
ON CONFLICT DO NOTHING;
