-- Skills -------------------------------------------------------------------

-- name: ListSkillsForViewer :many
-- Read authz: builtin + global are visible to everyone; a user's own private
-- skills are visible only to that user; admins see all scopes. This is NOT the
-- agent-templates all-shared read — copying that verbatim would leak private
-- user skills.
SELECT * FROM skills
WHERE sqlc.arg(is_admin)::boolean
   OR scope IN ('builtin', 'global')
   OR (scope = 'user' AND user_id = sqlc.arg(viewer_id))
ORDER BY scope, name;

-- name: GetSkillForViewer :one
-- Single-skill read with the same visibility rule as ListSkillsForViewer.
SELECT * FROM skills
WHERE id = sqlc.arg(id)
  AND (sqlc.arg(is_admin)::boolean
       OR scope IN ('builtin', 'global')
       OR (scope = 'user' AND user_id = sqlc.arg(viewer_id)));

-- name: GetSkill :one
-- Unfiltered fetch for the write path: the handler loads the row, then applies
-- the scope-based write authz in Go (builtin/global admin-only, user owner-only)
-- and maps an unauthorized user-scope row to 404 so existence never leaks.
SELECT * FROM skills WHERE id = $1;

-- name: CreateSkill :one
-- Create a global (admin) or user (owner) skill. scope='builtin' is never
-- created here; builtins are seeded only by the startup reconciler.
INSERT INTO skills (name, description, body, scope, user_id, updated_by)
VALUES (@name, @description, @body, @scope, @user_id, @updated_by)
RETURNING *;

-- name: UpdateSkill :one
-- Edits the mutable fields. name and scope are immutable and never touched here.
-- Also used by the builtin reset path to re-apply the embedded definition.
UPDATE skills
SET description = @description,
    body = @body,
    updated_by = @updated_by,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: DeleteSkill :execrows
-- Never deletes a builtin (the handler returns 409 first); the scope guard is
-- belt-and-suspenders.
DELETE FROM skills WHERE id = @id AND scope <> 'builtin';

-- name: InsertBuiltinSkill :execrows
-- Idempotent seed used by the startup reconciler: insert a missing builtin,
-- never overwrite an existing row (admin edits survive restarts). Keyed on the
-- builtin's name via the uq_skills_shared_name partial unique index.
INSERT INTO skills (name, description, body, scope)
VALUES (@name, @description, @body, 'builtin')
ON CONFLICT (name) WHERE scope <> 'user' DO NOTHING;

-- Skill allocations --------------------------------------------------------

-- name: ListAllocationsForTemplateForViewer :many
-- Allocation read authz: the shared rows (admin-managed) plus ONLY the caller's
-- own overlay rows — never another user's overlay, not even for an admin. Shared
-- rows sort first.
SELECT a.template_id, a.skill_id, a.user_id, a.created_at,
       s.name AS skill_name, s.description AS skill_description, s.scope AS skill_scope
FROM agent_skill_allocations a
JOIN skills s ON s.id = a.skill_id
WHERE a.template_id = sqlc.arg(template_id)
  AND (a.user_id IS NULL OR a.user_id = sqlc.arg(viewer_id))
ORDER BY (a.user_id IS NOT NULL), s.name;

-- name: DeleteSharedAllocations :exec
DELETE FROM agent_skill_allocations WHERE template_id = @template_id AND user_id IS NULL;

-- name: DeleteUserAllocations :exec
DELETE FROM agent_skill_allocations WHERE template_id = @template_id AND user_id = @user_id;

-- name: InsertSharedAllocation :exec
INSERT INTO agent_skill_allocations (template_id, skill_id, user_id)
VALUES (@template_id, @skill_id, NULL)
ON CONFLICT DO NOTHING;

-- name: InsertUserAllocation :exec
INSERT INTO agent_skill_allocations (template_id, skill_id, user_id)
VALUES (@template_id, @skill_id, @user_id)
ON CONFLICT DO NOTHING;

-- name: ListRunSkillAllocations :many
-- Every skill allocated to any agent template for this run's owner: the shared
-- rows (user_id NULL, admin-managed, all users) plus this user's private overlay
-- rows, joined to the skill body. Feeds claim assembly (the per-run union, the
-- per-template scoping, and the precedence/cap drops). A skill allocated to a
-- template both as shared and as this user's overlay yields two rows; assembly
-- dedupes by (template, skill). Ordered for a stable claim payload.
--
-- Defense-in-depth (auditor M3 Low): the scope predicates make a private body
-- unshippable even if a future handler bug wrote a bad allocation row. A shared
-- row (user_id NULL) may only carry a builtin/global skill; a user's overlay row
-- may only carry a builtin/global skill or that SAME user's own user skill. The
-- M2 handler already enforces this at write time; this is the second layer (the
-- shared-branch blast radius is all users, so the belt-and-suspenders is
-- warranted).
SELECT at.name AS template_name,
       s.id    AS skill_id,
       s.name  AS skill_name,
       s.description,
       s.body,
       s.scope
FROM agent_skill_allocations a
JOIN agent_templates at ON at.id = a.template_id
JOIN skills s ON s.id = a.skill_id
WHERE (a.user_id IS NULL AND s.scope <> 'user')
   OR (a.user_id = @user_id AND (s.scope <> 'user' OR s.user_id = @user_id))
ORDER BY at.name, s.name;
