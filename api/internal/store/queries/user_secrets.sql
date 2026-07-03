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
