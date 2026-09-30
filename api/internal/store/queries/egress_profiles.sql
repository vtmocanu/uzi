-- Egress profiles (PRD #1906 M1) --------------------------------------------
-- Admin-managed, named site lists. Every write is admin-only (the cookie-only admin
-- write group); the caller has already normalized and validated hosts and overrides
-- (api/internal/egressprofile). name is the identity a run will reference, so it is
-- immutable: the update and delete key on it and never change it.

-- name: ListEgressProfiles :many
SELECT * FROM egress_profiles ORDER BY name;

-- name: GetEgressProfileByName :one
SELECT * FROM egress_profiles WHERE name = @name;

-- name: CreateEgressProfile :one
INSERT INTO egress_profiles (name, description, hosts, multi_publisher_override, created_by, updated_by)
VALUES (@name, @description, @hosts::text[], @multi_publisher_override::text[], @actor, @actor)
RETURNING *;

-- name: UpdateEgressProfileByName :one
UPDATE egress_profiles
SET description              = @description,
    hosts                    = @hosts::text[],
    multi_publisher_override = @multi_publisher_override::text[],
    updated_by               = @actor,
    updated_at               = now()
WHERE name = @name
RETURNING *;

-- name: DeleteEgressProfileByName :execrows
DELETE FROM egress_profiles WHERE name = @name;
