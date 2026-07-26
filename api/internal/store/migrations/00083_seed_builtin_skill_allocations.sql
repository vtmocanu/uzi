-- +goose Up
-- Backfill the default shared allocation for `ci-cd-norms` (PRD #72 M2,
-- Decision 9).
--
-- Why a migration and not reconciler logic. ReconcileBuiltinSkills seeds a
-- builtin's defaults only on the boot that INSERTS it, mirroring
-- ReconcileBuiltinTemplates' `n > 0` gate and for the same stated reason: a
-- default an admin later removes must stay removed. `ci-cd-norms`'s row
-- already exists on every live instance, so its insert returns 0 rows forever and
-- the reconciler can never reach it. A migration runs exactly once, which
-- preserves that property; a reconciler special case would resurrect the
-- allocation on every boot after an admin deleted it. Same precedent 00049 set
-- when it backfilled the template-allocation defaults.
--
-- On a FRESH database this inserts nothing and that is correct, not a bug: the
-- skills table is empty at migration time (builtins are reconciler-seeded AFTER
-- migrations, per 00049's note), so the reconciler's own seed is what allocates
-- there. The two paths are disjoint by construction.
--
-- Decision 9's "a migration preserves the same property" is OVERSTATED for this
-- direction, and the Down block below already concedes the same limit for itself.
-- The Up preserves seed-once for every FUTURE removal, but it overrides one past
-- one: an admin who manually allocated `ci-cd-norms` and then deliberately
-- un-allocated it gets it back. Very narrow — no admin can have removed an
-- AUTO-seeded default here, since never having had one is the gap M2 closes — so
-- it can only override a manual removal of a manual allocation. Stated because a
-- reader checking Decision 9 against this file should not have to rediscover it.
--
-- The target list duplicates skilltmpl's defaultAllocations map, knowingly — SQL
-- cannot read Go. Keep them in sync by hand; the map is the source of truth for
-- fresh instances and this is the one-off catch-up for existing ones.
--
-- Both scope guards matter. uq_skills_shared_name is PARTIAL (WHERE scope
-- <> 'user'), so an unguarded `s.name = 'ci-cd-norms'` would also match every
-- user who happens to own a private skill of that name and create a SHARED
-- (user_id NULL) allocation pointing at their private body. Same shape for a
-- user-owned template named `coder`. ON CONFLICT DO NOTHING makes a re-run a
-- no-op against uq_allocations.
INSERT INTO agent_skill_allocations (template_id, skill_id, user_id)
SELECT t.id, s.id, NULL
FROM agent_templates t, skills s
WHERE s.name = 'ci-cd-norms' AND s.scope <> 'user'
  AND t.name IN ('coder', 'reviewer') AND t.scope <> 'user'
ON CONFLICT DO NOTHING;

-- +goose Down
-- Removes only the shared rows this migration could have created. It cannot
-- distinguish those from an identical allocation an admin made by hand before the
-- upgrade — an unavoidable property of reversing a data seed, and the reason this
-- direction is for development, not for a live rollback.
DELETE FROM agent_skill_allocations a
USING agent_templates t, skills s
WHERE a.template_id = t.id
  AND a.skill_id = s.id
  AND a.user_id IS NULL
  AND s.name = 'ci-cd-norms' AND s.scope <> 'user'
  AND t.name IN ('coder', 'reviewer') AND t.scope <> 'user';
