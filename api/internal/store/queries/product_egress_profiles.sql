-- Product site-list allowance (PRD #1976 M1) ---------------------------------
-- Which egress profiles (site lists) a product's tokens may name on job create. Writes are
-- admin-only (the cookie-only admin write group); the job-create check reads
-- ProductEgressProfileAllowed.

-- name: ListProductEgressProfiles :many
SELECT p.name, p.description, pep.created_at
  FROM product_egress_profiles pep
  JOIN egress_profiles p ON p.id = pep.egress_profile_id
 WHERE pep.product_id = @product_id
 ORDER BY p.name;

-- name: AddProductEgressProfile :exec
-- Idempotent: re-allowing an already-allowed list is a no-op and keeps the original created_by/at.
INSERT INTO product_egress_profiles (product_id, egress_profile_id, created_by)
VALUES (@product_id, @egress_profile_id, sqlc.narg('created_by'))
ON CONFLICT (product_id, egress_profile_id) DO NOTHING;

-- name: RemoveProductEgressProfile :execrows
DELETE FROM product_egress_profiles
 WHERE product_id = @product_id AND egress_profile_id = @egress_profile_id;

-- name: ProductEgressProfileAllowed :one
-- True only when the allowance row exists AND the product is enabled and not soft-deleted, so a
-- disabled or deleted product is refused every list even if rows survive.
SELECT EXISTS (
    SELECT 1
      FROM product_egress_profiles pep
      JOIN products pr ON pr.id = pep.product_id
     WHERE pep.product_id = @product_id
       AND pep.egress_profile_id = @egress_profile_id
       AND pr.enabled
       AND pr.deleted_at IS NULL
)::boolean;
