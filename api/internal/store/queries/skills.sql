-- Skills -------------------------------------------------------------------
--
-- SCOPE VOCABULARY (PRD #1909 M6): builtin | global | user | product (skilltmpl.Scopes; the
-- skills_scope_check CHECK). 'product' skills belong to one product and are delivered only to
-- that product's jobs (product_skills.sql, ListProductSkillsForRun); they never appear in a
-- viewer listing, an allocation or a run's allocation union. So every predicate in this file
-- that filters on scope names its scopes in an explicit IN list. A `scope <> 'x'` here would
-- silently admit 'product'.

-- name: ListSkillsForViewer :many
-- Read authz: builtin + global are visible to everyone; a user's own private
-- skills are visible only to that user; admins see all scopes. This is NOT the
-- agent-templates all-shared read — copying that verbatim would leak private
-- user skills.
-- Product skills are in NO branch: not even an admin lists them here (they are read through
-- the admin product route, ListProductSkills).
SELECT * FROM skills
WHERE (sqlc.arg(is_admin)::boolean AND scope IN ('builtin', 'global', 'user'))
   OR scope IN ('builtin', 'global')
   OR (scope = 'user' AND user_id = sqlc.arg(viewer_id))
ORDER BY scope, name;

-- name: GetSkillForViewer :one
-- Single-skill read with the same visibility rule as ListSkillsForViewer.
SELECT * FROM skills
WHERE id = sqlc.arg(id)
  AND ((sqlc.arg(is_admin)::boolean AND scope IN ('builtin', 'global', 'user'))
       OR scope IN ('builtin', 'global')
       OR (scope = 'user' AND user_id = sqlc.arg(viewer_id)));

-- name: GetSkill :one
-- Unfiltered fetch for the write path: the handler loads the row, then applies
-- the scope-based write authz in Go (builtin/global admin-only, user owner-only)
-- and maps an unauthorized user-scope row to 404 so existence never leaks. It does return a
-- product-scope row (the admin product routes share it); every handler that takes a skill id
-- from a caller treats scope 'product' as not found (authorizeSkillWrite, allocatable*).
SELECT * FROM skills WHERE id = $1;

-- name: CreateSkill :one
-- Create a global (admin) or user (owner) skill. scope='builtin' is never
-- created here; builtins are seeded only by the startup reconciler.
INSERT INTO skills (name, description, body, scope, user_id, updated_by)
VALUES (@name, @description, @body, @scope, @user_id, @updated_by)
RETURNING *;

-- name: UpdateSkill :one
-- Edits the mutable fields. name and scope are immutable and never touched here.
-- Also used by the builtin reset path to re-apply the embedded definition. A product skill is
-- changed only by an apply (product_skills.sql), never here.
UPDATE skills
SET description = @description,
    body = @body,
    updated_by = @updated_by,
    updated_at = now()
WHERE id = @id AND scope IN ('builtin', 'global', 'user')
RETURNING *;

-- name: DeleteSkill :execrows
-- Never deletes a builtin (the handler returns 409 first) and never a product skill (only an
-- apply replaces those); the scope guard is belt-and-suspenders.
DELETE FROM skills WHERE id = @id AND scope IN ('global', 'user');

-- name: InsertBuiltinSkill :execrows
-- Idempotent seed used by the startup reconciler: insert a missing builtin,
-- never overwrite an existing row (admin edits survive restarts). Keyed on the
-- builtin's name via the uq_skills_shared_name partial unique index.
INSERT INTO skills (name, description, body, scope)
VALUES (@name, @description, @body, 'builtin')
ON CONFLICT (name) WHERE scope IN ('builtin', 'global') DO NOTHING;

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
  AND s.scope IN ('builtin', 'global', 'user')
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

-- name: SeedSharedSkillAllocationByName :execrows
-- Seed one builtin skill's default shared allocation, the first time the
-- reconciler inserts that skill (PRD #72 M2, Decision 9). Mirrors
-- SeedSharedTemplateAllocationByName, with two deliberate differences.
--
-- :execrows, not :exec. The precedent is :exec and so is structurally incapable
-- of reporting the case Decision 9 requires a warning on. This seed targets a
-- DIFFERENT row than the insert that triggered it — the agent template named
-- @template_name — so an absent template inserts nothing, returns no error, and
-- would log nothing. The row count is the only signal that exists.
--
-- ONE TEMPLATE PER CALL, not `name = ANY(...)`. A set-valued form returning 1 of
-- 2 rows cannot say WHICH target was missing, and "which" is the entire content
-- of the warning.
--
-- Reading a 0: because the caller runs this ONLY when it just inserted the skill,
-- and agent_skill_allocations.skill_id is an FK to that brand-new row, no
-- allocation for this skill can pre-exist. So ON CONFLICT can never be what
-- returned 0 here, and 0 means unambiguously "no such shared agent template".
--
-- Both scope guards are load-bearing, and the skills one is the sharper:
-- uq_skills_shared_name is a PARTIAL unique index (WHERE scope IN ('builtin','global')), so
-- `WHERE name = @skill_name` alone matches every user's private skill of that
-- name too (and every product's) — N rows, each a SHARED (user_id NULL) allocation pointing at
-- a private body. ListRunSkillAllocations' scope predicates would refuse to ship
-- those, but that is the second layer; this must not be the first-layer bug it
-- exists to backstop.
INSERT INTO agent_skill_allocations (template_id, skill_id, user_id)
SELECT t.id, s.id, NULL
FROM agent_templates t, skills s
WHERE t.name = @template_name AND t.scope <> 'user'
  AND s.name = @skill_name    AND s.scope IN ('builtin', 'global')
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
WHERE (a.user_id IS NULL AND s.scope IN ('builtin', 'global'))
   OR (a.user_id = @user_id AND (s.scope IN ('builtin', 'global') OR (s.scope = 'user' AND s.user_id = @user_id)))
ORDER BY at.name, s.name;
