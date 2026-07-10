-- name: UpsertUserSecret :one
-- Insert or rotate a user's secret of a given kind. Returns metadata only
-- (never the ciphertext) so callers cannot accidentally serialize it.
INSERT INTO user_secrets (user_id, kind, ciphertext)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, kind) DO UPDATE
    SET ciphertext = EXCLUDED.ciphertext,
        updated_at = now()
RETURNING kind, created_at, updated_at;

-- name: GetUserSecretCiphertext :one
-- Fetch the sealed ciphertext for decryption (used by PRD #4 at agent-run time).
SELECT ciphertext FROM user_secrets WHERE user_id = $1 AND kind = $2;

-- name: ListUserSecretsMeta :many
-- Metadata-only listing for the current user; never selects ciphertext.
SELECT kind, created_at, updated_at
FROM user_secrets
WHERE user_id = $1
ORDER BY kind;

-- name: DeleteUserSecret :execrows
DELETE FROM user_secrets WHERE user_id = $1 AND kind = $2;

-- name: RewrapUserSecret :execrows
-- Lazy migration (PRD #32): re-seal a legacy master-key-sealed secret under the
-- owner's vault DEK on their first unlock, flipping sealed_with 'master' → 'dek'
-- in one statement. Guarded on the current sealed_with so a concurrent unlock
-- cannot double-rewrap (and cannot clobber a DEK-sealed row with stale bytes).
-- This does NOT un-leak a token that ever existed master-sealed — an operator
-- may have snapshotted the DB first; rotation is the real fix. It only improves
-- the at-rest posture going forward.
UPDATE user_secrets
SET ciphertext = @ciphertext, sealed_with = 'dek', updated_at = now()
WHERE user_id = @user_id AND kind = @kind AND sealed_with = 'master';
