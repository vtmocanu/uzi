-- name: GetUserVault :one
-- The user's KEK salt + wrapped DEK. The plaintext DEK is never selected (it is
-- never stored); the caller derives the KEK from the login password and unwraps
-- in memory. pgx.ErrNoRows ⇒ no vault yet, and the first-ever unlock creates one.
SELECT * FROM user_vaults WHERE user_id = $1;

-- name: CreateUserVaultIfAbsent :one
-- First-unlock vault creation, race-safe. Two concurrent first-unlocks for the
-- same user both run this; ON CONFLICT DO NOTHING lets exactly one insert win and
-- returns no row (pgx.ErrNoRows) to the loser, who re-reads the winner's row and
-- unwraps it with the same password — so the cached DEK always equals the
-- persisted one (fixes the check-then-act race where each request would otherwise
-- cache a different DEK than the DB holds). Deliberately DO NOTHING, never DO
-- UPDATE: overwriting a live wrapped_dek would orphan every secret sealed under
-- the previous DEK. kek_salt is the per-user Argon2 salt, distinct from
-- users.password_hash's salt so the DB never contains the KEK.
INSERT INTO user_vaults (user_id, kek_salt, wrapped_dek)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO NOTHING
RETURNING *;

-- name: DeleteUserVault :execrows
-- Drops ONLY the vault row (there is no FK cascade user_vaults → user_secrets).
-- Intended for the future password-reset flow, where the DEK is unrecoverable by
-- design: that flow MUST also delete this user's sealed_with='dek' user_secrets
-- rows and force re-entry — keeping unreadable ciphertext would be a worse bug
-- than the data loss. Reset itself is out of scope (PRD #32); this is the
-- primitive it will build on. Account deletion is already handled by the
-- ON DELETE CASCADE from users.
DELETE FROM user_vaults WHERE user_id = $1;
