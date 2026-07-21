-- name: InsertUserSecret :one
-- Store a NEW secret for a user. sealed_with records which key sealed the
-- ciphertext ('master' for the legacy box, 'dek' for the vault); the caller sets it
-- to match how it produced @ciphertext. Returns metadata only (never the
-- ciphertext) so callers cannot accidentally serialize it.
--
-- A plain INSERT since PRD #104 D10: the old form was an upsert keyed on
-- ON CONFLICT (user_id, kind), which named the very unique index 00077 drops, and
-- which encoded the retired assumption that a user has at most one secret per kind.
-- The caller must name a label and decide is_default; there is no implicit
-- "replace whatever was there".
INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, kind, label, is_default, created_at, updated_at;

-- name: RotateUserSecret :one
-- Replace one stored secret's value in place, keyed on its id (PRD #104 D10) and
-- scoped to its owner so a caller holding a foreign id writes nothing. The label
-- and is_default flag are deliberately untouched: rotating a credential's value is
-- not the same operation as renaming it or changing which one is the default.
UPDATE user_secrets
SET ciphertext = $3, sealed_with = $4, updated_at = now()
WHERE id = $1 AND user_id = $2
RETURNING id, kind, label, is_default, created_at, updated_at;

-- name: UpsertDefaultUserSecret :one
-- The kind-path compatibility alias (PRD #104 D14): PUT /api/me/secrets/{kind}
-- rotates the user's DEFAULT secret of that kind, or creates their first one
-- labelled 'default'. Deliberately ONE statement rather than a resolve-then-write
-- pair: the old ON CONFLICT (user_id, kind) form made two concurrent saves safe,
-- and a read-then-write would turn that into a unique violation (500) on a path
-- that has never had one.
--
-- The arbiter is the partial unique index from 00077 — (user_id, kind) WHERE
-- is_default — matched by repeating its predicate, so the conflict target is "this
-- user's default of this kind" whatever that row happens to be labelled. Only the
-- value moves on conflict; an existing default keeps its label.
--
-- Reachable failure, and it is the correct one: a user who already holds a
-- NON-default secret labelled 'default' plus a differently-labelled default (only
-- possible once M2 can create and re-point tokens) makes this INSERT collide with
-- the label index, which is not the arbiter, so it raises a unique violation rather
-- than silently rotating the wrong credential. Such a user is a multi-token user
-- and is expected to use the id-keyed routes.
INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
VALUES ($1, $2, 'default', true, $3, $4)
ON CONFLICT (user_id, kind) WHERE is_default DO UPDATE
    SET ciphertext = EXCLUDED.ciphertext,
        sealed_with = EXCLUDED.sealed_with,
        updated_at = now()
RETURNING id, kind, label, is_default, created_at, updated_at;

-- name: GetUserSecretCiphertext :one
-- Fetch the sealed ciphertext + how it was sealed, for decryption at agent-run
-- time (PRD #4/#32). sealed_with tells the vault whether to open under the master
-- box (legacy 'master') or the per-user DEK ('dek').
--
-- "The user's secret of this kind" now means "their DEFAULT secret of this kind"
-- (PRD #104 D14): with UNIQUE (user_id, kind) gone, by-kind alone no longer
-- identifies a row, and every unbound consumer wants the default. The partial
-- unique index makes at most one row match, so this stays :one. Callers that want a
-- SPECIFIC credential use GetUserSecretCiphertextByID.
SELECT ciphertext, sealed_with FROM user_secrets
WHERE user_id = $1 AND kind = $2 AND is_default;

-- name: GetUserSecretCiphertextByID :one
-- Fetch one specific secret by identity, for the bound-credential resolution M3
-- (worker binding) and M4 (judge lane) hang off secretopen.OpenByID.
--
-- Scoped to the owner in the predicate, so a caller that supplies another user's
-- secret id gets no rows rather than that user's credential. user_id is also
-- SELECTed so the Go side can re-assert ownership on the returned row: the schema
-- (M3's composite FK), this predicate, and that check are three independent layers,
-- and the cost of the redundant ones is a column and an if (PRD #104 D11).
--
-- kind rides along because the DEK AAD is user_id||kind — the opener needs the
-- row's own kind, not one the caller guessed.
SELECT user_id, kind, ciphertext, sealed_with FROM user_secrets
WHERE id = $1 AND user_id = $2;

-- name: GetDefaultUserSecretID :one
-- Resolve "which row is this user's default secret of this kind". The by-kind write
-- paths that used to address a row implicitly now resolve it here first, and M2/M3
-- reuse it wherever a default has to be named rather than assumed. pgx.ErrNoRows
-- means the user has no secret of the kind at all.
SELECT id FROM user_secrets
WHERE user_id = $1 AND kind = $2 AND is_default;

-- name: ListUserSecretsMeta :many
-- Metadata-only listing for the current user; never selects ciphertext.
SELECT kind, created_at, updated_at
FROM user_secrets
WHERE user_id = $1
ORDER BY kind;

-- name: DeleteUserSecret :execrows
-- Delete ONE secret, keyed on its id (PRD #104 D10) and scoped to its owner. The
-- previous by-kind form deleted every row of the kind for the user, which was a
-- no-op distinction while a user could only hold one and a whole-credential-set
-- wipe the moment they could hold two. Callers holding only a kind resolve the
-- default with GetDefaultUserSecretID first.
DELETE FROM user_secrets WHERE id = $1 AND user_id = $2;

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
