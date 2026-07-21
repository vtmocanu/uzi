-- name: UpsertUserSecret :one
-- Insert or rotate a user's secret of a given kind. sealed_with records which key
-- sealed the ciphertext ('master' for the legacy box, 'dek' for the vault); the
-- caller sets it to match how it produced @ciphertext. Returns metadata only
-- (never the ciphertext) so callers cannot accidentally serialize it.
INSERT INTO user_secrets (user_id, kind, ciphertext, sealed_with)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, kind) DO UPDATE
    SET ciphertext = EXCLUDED.ciphertext,
        sealed_with = EXCLUDED.sealed_with,
        updated_at = now()
RETURNING kind, created_at, updated_at;

-- name: GetUserSecretCiphertext :one
-- Fetch the sealed ciphertext + how it was sealed, for decryption at agent-run
-- time (PRD #4/#32). sealed_with tells the vault whether to open under the master
-- box (legacy 'master') or the per-user DEK ('dek').
SELECT ciphertext, sealed_with FROM user_secrets WHERE user_id = $1 AND kind = $2;

-- name: ListUserSecretsMeta :many
-- Metadata-only listing for the current user; never selects ciphertext.
SELECT kind, created_at, updated_at
FROM user_secrets
WHERE user_id = $1
ORDER BY kind;

-- name: DeleteUserSecret :execrows
DELETE FROM user_secrets WHERE user_id = $1 AND kind = $2;

-- name: CountMasterSealedSecrets :one
-- Admin migration-progress signal (PRD #32): how many stored user secrets still
-- use the legacy master-key sealing — i.e. owners who have not unlocked since the
-- vault rolled out. New saves are born 'dek' and lazy rewrap flips old rows on the
-- owner's next unlock, so this trends to zero; a persistently non-zero count flags
-- dormant accounts an operator may want to nudge.
SELECT count(*) FROM user_secrets WHERE sealed_with = 'master';

-- name: ListMasterSealedSecrets :many
-- The user's still-legacy secrets, for lazy rewrap on unlock (PRD #32): the vault
-- opens each with the master box, reseals under the DEK, and flips it to 'dek'
-- (RewrapUserSecret). Selects the ciphertext because rewrap must decrypt it; the
-- rows are only ever handed to the vault, never serialized out. Empty once a user
-- has been fully migrated, so the steady-state unlock does no rewrap work.
--
-- Selects id because RewrapUserSecret is keyed on it (PRD #104 D10): the rewrap
-- loop reseals one row's plaintext and must write it back to THAT row, which is
-- only expressible once the row it opened carries an identity.
SELECT id, kind, ciphertext FROM user_secrets
WHERE user_id = $1 AND sealed_with = 'master';

-- name: RewrapUserSecret :execrows
-- Lazy migration (PRD #32): re-seal a legacy master-key-sealed secret under the
-- owner's vault DEK on their first unlock, flipping sealed_with 'master' → 'dek'
-- in one statement. Guarded on the current sealed_with so a concurrent unlock
-- cannot double-rewrap (and cannot clobber a DEK-sealed row with stale bytes).
-- This does NOT un-leak a token that ever existed master-sealed — an operator
-- may have snapshotted the DB first; rotation is the real fix. It only improves
-- the at-rest posture going forward.
--
-- Keyed on id, not (user_id, kind) (PRD #104 D10). The by-kind form was a silent
-- data-loss bug the moment a user could hold two secrets of one kind: the rewrap
-- loop opens row 1, reseals it, and the UPDATE matched EVERY master-sealed row of
-- that kind — overwriting siblings 2..N with row 1's bytes, after which the
-- remaining iterations matched nothing and the loop reported success. user_id is
-- kept in the predicate as a defensive scope (an id alone is already unique; this
-- makes a caller that hands us another user's id a no-op rather than a write).
UPDATE user_secrets
SET ciphertext = @ciphertext, sealed_with = 'dek', updated_at = now()
WHERE id = @id AND user_id = @user_id AND sealed_with = 'master';
