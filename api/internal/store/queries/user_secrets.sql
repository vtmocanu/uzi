-- name: InsertUserSecret :one
-- Store a NEW secret for a user. sealed_with records which key sealed the
-- ciphertext ('master' for the legacy box, 'dek' for the vault); the caller sets it
-- to match how it produced @ciphertext. Returns metadata only (never the
-- ciphertext) so callers cannot accidentally serialize it.
--
-- An INSERT rather than an upsert since PRD #104 D10: the old form was keyed on
-- ON CONFLICT (user_id, kind), which named the very unique index 00077 drops, and
-- which encoded the retired assumption that a user has at most one secret per kind.
-- The caller names a label and there is no implicit "replace whatever was there".
--
-- @want_default IS A REQUEST, NOT A GUARANTEE — read this before calling, and note
-- the parameter is deliberately not named is_default. A user's FIRST secret of a
-- kind is forced to is_default regardless of what the caller asks for, because a
-- first token that is not the default is INVISIBLE:
-- the four existence gates (anthropic_rate_limits.sql's has_token and
-- UserHasAnthropicToken, autopilot.sql's has_anthropic_token, and the seed's
-- ListUserSecretsMeta) are EXISTS queries with no is_default filter, while every
-- resolution path is by-kind AND is_default. Such a row makes the UI say "Set" and
-- the gates green-light runs that then fail on credential-unavailable, with nothing
-- logged as wrong.
--
--   @want_default | user has no row of this kind | user already has one
--   --------------|------------------------------|----------------------
--   false         | forced TRUE (bug prevented)  | stays false
--   true          | TRUE                         | unique violation, loudly
--
-- So a caller can be silently overridden on exactly one case, and never on the case
-- that matters to a multi-token surface: every non-first row is the caller's
-- decision, which is all M2 needs to own set-default. Verified against Postgres 17
-- with 00077's indexes in place.
--
-- This does NOT replace D12's transaction. It converts one silent failure into
-- either the right answer or a loud one; serialising the set-default swap and the
-- delete-default check is a different job, and still M2's.
INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
VALUES (@user_id, @kind, @label,
        -- Scoped to (user_id, kind), never to user_id alone: the partial unique
        -- index this defends is per-(user_id, kind), and under D9's future
        -- 'openai_token' kind a user-scoped test would create their first openai
        -- token non-default merely because they hold an anthropic one — the exact
        -- invisible-token bug, one kind over.
        @want_default::boolean
            OR NOT EXISTS (SELECT 1 FROM user_secrets WHERE user_id = @user_id AND kind = @kind),
        @ciphertext, @sealed_with)
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
-- What does NOT happen, because it is the obvious guess and it is wrong: a user
-- holding a non-default row labelled 'default' PLUS a differently-labelled default
-- does not collide with the label index. Postgres resolves the arbiter first during
-- speculative insertion, finds the conflict, takes DO UPDATE, and never inserts a
-- tuple — so the label index is never consulted. Measured on Postgres 17 with
-- 00077's indexes: with (console-key, is_default) + (default, not default), this
-- statement returns label='console-key' carrying the new value and leaves the row
-- labelled 'default' untouched. Correct under D14 — console-key IS the default.
--
-- What DOES raise is the mirror image: a row labelled 'default' exists and NOTHING
-- is is_default. There is no arbiter conflict, so the insert proceeds into the
-- label index and hits:
--
--   ERROR: duplicate key value violates unique constraint "user_secrets_user_kind_label_key"
--
-- which surfaces as a 500 from PutAnthropicToken. That is D12's "tokens exist with
-- no default" state. It is UNREACHABLE in M1 — every create path forces the first
-- token default and nothing can clear the flag — and becomes reachable the moment
-- M2 ships set-default or delete-default. M2's FOR UPDATE transaction is what has
-- to make it unreachable again; until then this comment is the warning. (A
-- no-default user whose rows are all labelled something else is fine: the insert
-- simply creates a new default labelled 'default'.)
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

-- name: GetUserSecretIDByLabel :one
-- Resolve a user-facing label to the credential it names, case-insensitively to
-- match the unique index 00077 put on (user_id, kind, lower(label)) — `Console` and
-- `console` are the same token, so the CLI and the mint-time label accept either.
-- pgx.ErrNoRows means the user has no token by that name, which callers report as
-- "unknown label", never as a server error.
SELECT id FROM user_secrets
WHERE user_id = @user_id AND kind = @kind AND lower(label) = lower(@label);

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
