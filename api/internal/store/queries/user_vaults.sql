-- name: GetUserVault :one
-- The user's KEK salt + wrapped DEK. The plaintext DEK is never selected (it is
-- never stored); the caller derives the KEK from the login password and unwraps
-- in memory. pgx.ErrNoRows ⇒ no vault yet, and the first-ever unlock creates one.
SELECT * FROM user_vaults WHERE user_id = $1;

-- name: UpsertUserVault :one
-- Create the vault on first unlock, or (on a future password change) rewrap the
-- DEK under a fresh KEK salt. Only ever stores the wrapped DEK; kek_salt is the
-- per-user Argon2 salt, distinct from users.password_hash's salt so the DB never
-- contains the KEK.
INSERT INTO user_vaults (user_id, kek_salt, wrapped_dek)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE
    SET kek_salt = EXCLUDED.kek_salt,
        wrapped_dek = EXCLUDED.wrapped_dek,
        updated_at = now()
RETURNING *;

-- name: DeleteUserVault :execrows
-- Password reset (PRD #32, Password change / reset): the DEK is unrecoverable by
-- design, so reset must drop the vault together with the user's dek-sealed
-- secrets and force re-entry. Keeping unreadable ciphertext would be a worse bug
-- than the data loss. The ON DELETE CASCADE from users covers account deletion;
-- this is the standalone reset path.
DELETE FROM user_vaults WHERE user_id = $1;
