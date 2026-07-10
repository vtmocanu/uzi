-- Tool allowlist (PRD #18 M4) ----------------------------------------------

-- name: ListToolAllowlist :many
-- The full allowlist. Read by admins (management), by the repo package picker
-- (which options a profile may pick), and at write/claim validation.
SELECT * FROM tool_allowlist ORDER BY name;

-- name: CreateToolAllowlistEntry :one
INSERT INTO tool_allowlist (name, pinned_version, note, updated_by)
VALUES (@name, @pinned_version, @note, @updated_by)
RETURNING *;

-- name: UpdateToolAllowlistEntry :one
-- name is immutable (package identity); only the version policy + note change.
UPDATE tool_allowlist
SET pinned_version = @pinned_version,
    note           = @note,
    updated_by     = @updated_by,
    updated_at     = now()
WHERE id = @id
RETURNING *;

-- name: DeleteToolAllowlistEntry :execrows
DELETE FROM tool_allowlist WHERE id = @id;

-- Repo tool profiles (PRD #18 M4) ------------------------------------------

-- name: GetRepoToolProfile :one
-- The profile packages for (user, repo). No ownership join: the caller supplies
-- the run/repo owner's user_id (claim assembly reads the run owner's; the owner
-- read path has already authorized the repo). Absent ⇒ no rows ⇒ no provisioning.
SELECT * FROM repo_tool_profiles WHERE user_id = @user_id AND repo_id = @repo_id;

-- name: UpsertRepoToolProfileForOwner :one
-- Owner-only write: inserts/updates the profile ONLY when repo_id belongs to a
-- connection owned by user_id (the SELECT yields a row only then), so a non-owner
-- or unknown id writes nothing → no rows → 404 in the handler. packages is the
-- caller's already-allowlist-validated list.
INSERT INTO repo_tool_profiles (user_id, repo_id, packages)
SELECT @user_id, r.id, @packages
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = @repo_id AND c.user_id = @user_id
ON CONFLICT (user_id, repo_id) DO UPDATE
    SET packages = EXCLUDED.packages, updated_at = now()
RETURNING *;
