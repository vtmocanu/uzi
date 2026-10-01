-- Product skill sets (PRD #1909 M6). Schema and rationale: migrations/00278_product_skills.sql.
--
-- skills_token_sealed is the sealed read-only clone token. The products queries use
-- `SELECT *` / `RETURNING *`, so it is IN store.Product: every DTO that renders a product is an
-- explicit field list (apitypes.ProductDTO, apitypes.ProductSkillsDTO) and never embeds the row,
-- and TestProductSkillsTokenNeverInAnyResponseLiveDB pins that over every admin product route.
--
-- SCOPE PREDICATES. A skills query that filters on scope uses an explicit IN list, never `<>`:
-- the vocabulary now has four members and a `<> 'user'` read would silently include 'product'
-- (skilltmpl.Scopes is the Go mirror of the CHECK; see queries/skills.sql).

-- name: UpdateProductSkillsSource :one
-- Edit a live product's skills source. Each of repo url, ref and sealed token is a NULLABLE
-- argument (NULL keeps the current value); clear_token NULLs the sealed token (and wins over a
-- supplied one). A soft-deleted product is never edited (no row).
UPDATE products
   SET skills_repo_url = COALESCE(sqlc.narg(skills_repo_url)::text, skills_repo_url),
       skills_ref = COALESCE(sqlc.narg(skills_ref)::text, skills_ref),
       skills_token_sealed = CASE
           WHEN sqlc.arg(clear_token)::boolean THEN NULL
           ELSE COALESCE(sqlc.narg(skills_token_sealed)::bytea, skills_token_sealed)
       END,
       updated_at = now()
 WHERE id = sqlc.arg(id)
   AND deleted_at IS NULL
RETURNING *;

-- name: UpsertProductSkillStage :exec
-- Replace the product's ONE staged snapshot. Runs under GetProductForUpdate's row lock in the
-- syncing transaction, so it serializes with an apply of the previous stage.
INSERT INTO product_skill_staged (product_id, source_sha, staged_by, skills, dropped)
VALUES (sqlc.arg(product_id), sqlc.arg(source_sha), sqlc.arg(staged_by), sqlc.arg(skills)::jsonb, sqlc.arg(dropped)::jsonb)
ON CONFLICT (product_id) DO UPDATE
   SET source_sha = EXCLUDED.source_sha,
       staged_by = EXCLUDED.staged_by,
       staged_at = now(),
       skills = EXCLUDED.skills,
       dropped = EXCLUDED.dropped;

-- name: GetProductSkillStage :one
SELECT * FROM product_skill_staged WHERE product_id = $1;

-- name: DeleteProductSkillStage :exec
-- Discard the product's staged snapshot (its source changed, or it was just applied).
DELETE FROM product_skill_staged WHERE product_id = $1;

-- name: DeleteProductSkills :execrows
-- Drop every applied skill of one product: the first half of an apply's replace.
DELETE FROM skills WHERE scope = 'product' AND product_id = $1;

-- name: InsertProductSkill :exec
INSERT INTO skills (name, description, body, scope, product_id, updated_by)
VALUES (@name, @description, @body, 'product', @product_id, @updated_by);

-- name: MarkProductSkillsApplied :exec
UPDATE products
   SET skills_applied_sha = @sha,
       skills_applied_by = @applied_by,
       skills_applied_at = now(),
       updated_at = now()
 WHERE id = @id;

-- name: ListProductSkills :many
-- The approved (applied) skills of one product: the admin view and nothing else. Never
-- reachable from a listing a non-admin can call (ListSkillsForViewer excludes the scope).
SELECT * FROM skills WHERE scope = 'product' AND product_id = $1 ORDER BY name;

-- name: ListProductSkillsForRun :many
-- The claim-time read (PRD #1909 D9): the approved skills of the product that started this JOB
-- run, and only that product's. The product comes from job_origins, never from the caller or
-- the run's owner, so:
--   * a uzc_ job (job_origins.product_id NULL) joins nothing;
--   * a non-job run has no job_origins row and joins nothing;
--   * a disabled or soft-deleted product delivers nothing (p.enabled, p.deleted_at).
-- Only an APPLIED skill is a skills row: a staged snapshot lives in product_skill_staged and is
-- never read here. Ordered by name for a stable claim payload.
SELECT s.id, s.name, s.description, s.body
  FROM job_origins o
  JOIN products p ON p.id = o.product_id
  JOIN skills s ON s.scope = 'product' AND s.product_id = p.id
 WHERE o.run_id = $1
   AND p.enabled
   AND p.deleted_at IS NULL
 ORDER BY s.name;
