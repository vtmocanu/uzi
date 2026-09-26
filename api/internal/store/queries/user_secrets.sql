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
INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with)
VALUES (@user_id, @kind, @label,
        -- Scoped to (user_id, kind), never to user_id alone: the partial unique
        -- index this defends is per-(user_id, kind), and under D9's future
        -- 'openai_token' kind a user-scoped test would create their first openai
        -- token non-default merely because they hold an anthropic one — the exact
        -- invisible-token bug, one kind over.
        @want_default::boolean
            OR NOT EXISTS (SELECT 1 FROM user_secrets WHERE user_id = @user_id AND kind = @kind AND is_default),
        -- auto_eligible (PRD #111 D2 / issue #804): a user's FIRST/SOLE anthropic_token
        -- is born opted INTO the auto-select pool, so a single-token owner's auto-mode
        -- ephemeral workers have a non-empty pool to spend and never park in pool_wait.
        -- Token #2+ stays opt-in false (the reserved-console-key hazard is multi-token).
        -- Reuses the SAME first-token subquery as is_default just above; the
        -- @kind = 'anthropic_token' guard keeps 00087's CHECK
        -- (NOT auto_eligible OR kind = 'anthropic_token') satisfied for a future
        -- non-anthropic first token.
        (@kind = 'anthropic_token' AND NOT EXISTS (SELECT 1 FROM user_secrets WHERE user_id = @user_id AND kind = @kind)),
        @ciphertext, @sealed_with)
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

-- name: RotateUserSecret :one
-- Replace one stored secret's value in place, keyed on its id (PRD #104 D10) and
-- scoped to its owner so a caller holding a foreign id writes nothing. The label
-- and is_default flag are deliberately untouched: rotating a credential's value is
-- not the same operation as renaming it or changing which one is the default.
UPDATE user_secrets
SET ciphertext = $3, sealed_with = $4, updated_at = now()
WHERE id = $1 AND user_id = $2
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

-- name: UpsertDefaultUserSecret :one
-- Compatibility PUT rotates an enabled default or inserts a new one. The caller
-- holds the user mutation lock and first promotes an existing enabled row when
-- the slot is empty. A disabled row labelled 'default' keeps its ciphertext and
-- preferences; the new row receives a distinct label in that case. The conflict
-- update refuses disabled defaults, even if a stale caller reaches this query.
INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with)
VALUES ($1, $2, CASE WHEN EXISTS (
            SELECT 1 FROM user_secrets
            WHERE user_id = $1 AND kind = $2 AND lower(label) = 'default'
        ) THEN 'default-' || gen_random_uuid()::text ELSE 'default' END, true,
        -- auto_eligible (PRD #111 D2 / issue #804): born opted into the auto-select
        -- pool ONLY when this is the user's first token of the kind — the SAME
        -- first-token guard InsertUserSecret uses. The INSERT branch also handles
        -- an empty slot containing only disabled rows; these existing credentials
        -- must not make the new token automatically pool eligible. The kind guard
        -- keeps 00087's CHECK satisfied.
        ($2 = 'anthropic_token' AND NOT EXISTS (SELECT 1 FROM user_secrets WHERE user_id = $1 AND kind = $2)),
        $3, $4)
ON CONFLICT (user_id, kind) WHERE is_default DO UPDATE
    SET ciphertext = EXCLUDED.ciphertext,
        sealed_with = EXCLUDED.sealed_with,
        updated_at = now()
        -- Preserve the existing opt-in/opt-out state on rotation.
    WHERE user_secrets.disabled_at IS NULL
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

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

-- name: GetEnabledUserSecretForKind :one
-- Pick an enabled row for compatibility PUT when the slot has no default.
-- The caller holds the mutation lock; selection and promotion share its transaction.
SELECT id FROM user_secrets
WHERE user_id = @user_id AND kind = @kind AND disabled_at IS NULL
ORDER BY created_at, id LIMIT 1;

-- name: GetDefaultUserSecretID :one
-- Resolve "which row is this user's default secret of this kind". The by-kind write
-- paths that used to address a row implicitly now resolve it here first, and M2/M3
-- reuse it wherever a default has to be named rather than assumed. pgx.ErrNoRows
-- means the user has no secret of the kind at all.
SELECT id FROM user_secrets
WHERE user_id = $1 AND kind = $2 AND is_default;

-- name: GetDefaultUserSecretMeta :one
-- Resolve the user's default secret of a kind to its id AND its label, in one
-- owner-scoped read (PRD #111 M1, D8). This is GetDefaultUserSecretID plus the
-- label, and the two exist side by side rather than one replacing the other
-- because their callers want different things: the poller and the secrets handler
-- want only the id, the claim path wants the label too so the run can SNAPSHOT
-- which account it spent.
--
-- The predicate is character-identical to GetUserSecretCiphertext's
-- (WHERE user_id AND kind AND is_default, :112) on purpose. D8 replaces
-- "open the default" with "resolve the default, then open THAT id", and the
-- equivalence of the two resolutions is what makes that a no-op for behavior: same
-- row, same sealed_with, same kind-derived DEK AAD. The partial unique index from
-- 00077 makes at most one row match, which is what keeps this :one.
--
-- Both columns come from ONE owner-scoped row on purpose. Resolving the id here
-- and then looking the label up separately by id would be a cross-tenant read with
-- no owner predicate; there is deliberately no such second query to reach for.
--
-- pgx.ErrNoRows means the user has no secret of the kind at all, and the claim
-- path MUST keep mapping that to its existing "no Anthropic token configured for
-- this user" credential failure — a token-less user's run has always failed that
-- way, and leaking a different error from here would silently rewrite
-- runs.failure_reason for a case that has not changed.
SELECT id, label FROM user_secrets
WHERE user_id = $1 AND kind = $2 AND is_default;

-- name: GetUserSecretMetaByID :one
-- The by-id counterpart of GetDefaultUserSecretMeta (PRD #111 M1): the label of
-- ONE named credential, for the same snapshot, on the bound paths (a worker's
-- binding, the judge lane's).
--
-- Owner-scoped in the predicate exactly as GetUserSecretCiphertextByID is
-- (WHERE id AND user_id, :127), so a caller supplying another user's secret id
-- gets no rows rather than that user's label. That matters more here than it looks:
-- the id is opened by secretopen.OpenByID, which re-scopes and would refuse a
-- foreign row anyway — but this query runs FIRST, and without the predicate the
-- claim would fail with another user's label already in hand at the exact point
-- M1 records it and M5 renders it.
--
-- Deliberately NOT filtered on kind, matching GetUserSecretCiphertextByID: the
-- open reads the row's own kind for the DEK AAD, and adding a kind predicate here
-- would change what an (already impossible) mis-kinded binding fails WITH, for no
-- gain. pgx.ErrNoRows means "not this user's secret, or gone", which the claim path
-- maps to the same credential failure OpenByID's ErrNoSecret produces today.
--
-- SECURITY HARDENING (PRD #1147 audit): kind rides along additively so the bind-time
-- kind↔auth-mode check can verify the credential's actual kind matches the auth mode
-- the binding was frozen with (a codex_auth alias bound as an api_key, or an
-- openai_api_key bound as a subscription, is a contradiction the audit found unchecked).
-- The predicate stays unfiltered on kind for the reason above; only the SELECT list grows.
SELECT id, label, kind FROM user_secrets
WHERE id = $1 AND user_id = $2;

-- name: GetUserSecretMetaByIDOfKind :one
-- The kind-SCOPED by-id meta lookup (PRD #1247 M1): the per-run credential override
-- resolves a specific credential and must confirm it is an anthropic_token before
-- opening it. GetUserSecretMetaByID above is owner-scoped but deliberately NOT
-- kind-scoped (its comment explains why the worker/judge bind lanes keep it that way),
-- so a separate query carries the kind predicate rather than weakening those lanes. Used
-- by the override open path in claimSecretID and by validateCredentialOverride as
-- defense in depth; a wrong-kind or foreign id returns pgx.ErrNoRows, which the caller
-- maps to the same "unavailable" credential failure a foreign id already produces (D9).
SELECT id, label, kind FROM user_secrets
WHERE id = @id AND user_id = @user_id AND kind = @kind;

-- name: GetUserSecretIDByLabel :one
-- Resolve a user-facing label to the credential it names, case-insensitively to
-- match the unique index 00077 put on (user_id, kind, lower(label)) — `Console` and
-- `console` are the same token, so the CLI and the mint-time label accept either.
-- pgx.ErrNoRows means the user has no token by that name, which callers report as
-- "unknown label", never as a server error.
SELECT id FROM user_secrets
WHERE user_id = @user_id AND kind = @kind AND lower(label) = lower(@label);

-- name: ListUserSecretsMeta :many
-- Metadata-only listing for the current user; never selects ciphertext. Retained
-- for the seed's create-only existence check (it needs only kind); the id-bearing
-- listing GET /api/me/secrets uses is ListUserSecretsForKind.
SELECT kind, created_at, updated_at
FROM user_secrets
WHERE user_id = $1
ORDER BY kind;

-- name: ListUserSecretsForKind :many
-- The user's tokens of one kind, for GET /api/me/secrets (PRD #104 M2). Carries the
-- id (M1's secretDTO deliberately had none; without it a multi-token list is
-- indistinguishable rows), the label + is_default the UI renders, and the
-- timestamps — never the ciphertext. Default first, then by label, so the credential
-- a user's unbound workers spend against leads the list.
--
-- auto_eligible (PRD #111 M2) rides along: it is the D2 opt-in, a flag the owner
-- set, and belongs with the rest of a token's metadata. It is NOT the token's live
-- eligibility — whether the selector could actually pick it right now additionally
-- depends on its gauge reading, which this query deliberately does not join (see
-- ListRateLimitsForUser, which does).
SELECT id, kind, label, is_default, auto_eligible, created_at, updated_at
FROM user_secrets
WHERE user_id = @user_id AND kind = @kind
ORDER BY is_default DESC, lower(label) ASC;

-- name: GetUserSecretForUpdate :one
-- Lock and read ONE of the user's secrets inside a mutation transaction (PRD #104
-- M2/D12). FOR UPDATE holds the row until the transaction ends; combined with the
-- per-(user,kind) advisory lock the mutation takes first, it is what lets
-- set-default's clear-then-set and delete-default's guard read a stable picture.
-- Owner-scoped, so a foreign id is pgx.ErrNoRows (a 404), never another user's row.
SELECT id, kind, label, is_default, auto_eligible, disabled_at, enablement_rev FROM user_secrets
WHERE id = @id AND user_id = @user_id
FOR UPDATE;

-- name: CountUserSecretsForKind :one
-- How many tokens of a kind the user holds, read inside the mutation transaction
-- for D6's "is this the last one?" check. Under the advisory lock this count is
-- stable: no concurrent create can add a row until this transaction commits.
SELECT count(*) FROM user_secrets
WHERE user_id = @user_id AND kind = @kind;

-- name: ClearDefaultUserSecret :execrows
-- Clear the user's current default of a kind (PRD #104 M2). The first half of the
-- set-default swap and of create-as-default; run under the advisory lock so it
-- cannot race a concurrent promote into two defaults (which the partial unique index
-- would reject anyway, but the lock makes the swap atomic rather than a caught
-- violation). Affects the single default row, or 0 when there is none.
UPDATE user_secrets SET is_default = false, updated_at = now()
WHERE user_id = @user_id AND kind = @kind AND is_default;

-- name: SetUserSecretDefault :one
-- Make ONE secret the user's default (PRD #104 M2). The second half of the swap,
-- after ClearDefaultUserSecret; owner-scoped. Returns metadata for the response.
-- The caller has already verified ownership via GetUserSecretForUpdate under the
-- lock; the SQL guard also refuses a disabled target.
UPDATE user_secrets SET is_default = true, updated_at = now()
WHERE id = @id AND user_id = @user_id AND disabled_at IS NULL
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

-- name: RenameUserSecret :one
-- Rename ONE of the user's secrets (PRD #104 M2). Owner-scoped. A label colliding
-- (case-insensitively) with another of the user's tokens of the kind raises a
-- unique violation on user_secrets_user_kind_label_key, which the handler maps to a
-- 409 — renaming does NOT touch is_default, so it needs no lock for the default
-- invariant (only the label index, which enforces itself).
UPDATE user_secrets SET label = @label, updated_at = now()
WHERE id = @id AND user_id = @user_id
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

-- name: SetUserSecretAutoEligible :one
-- Opt ONE of the user's tokens into or out of the auto-selection pool (PRD #111 M2,
-- D2). Owner-scoped in its own predicate, exactly as RenameUserSecret is, so a
-- caller holding a foreign id writes nothing and reads nothing back — the handler's
-- ownership check and this predicate are two independent layers, and the migration's
-- kind CHECK is a third.
--
-- Returns the same metadata shape as RenameUserSecret / SetUserSecretDefault so all
-- three feed one secretMeta builder; the shared shape is why the Go structs are
-- convertible to each other at the call site, and why a column added to one must be
-- added to all of them.
--
-- Idempotent: setting the value it already has affects one row and changes nothing
-- but updated_at, which is the right answer for a toggle a UI may re-send.
--
-- The kind predicate is vacuous today (anthropic_token is user_secrets' only kind)
-- and is here for the day it is not: the ROUTE already asserts the kind in its path
-- (/anthropic_token/{id}), so a request naming a secret of some other kind is a
-- mismatch the handler should answer 404 to. Without this it would instead reach
-- 00087's CHECK and surface as a 500. That makes the CHECK argument stronger rather
-- than weaker — the constraint stays the backstop, and the handler stops depending
-- on it to produce a user-facing error.
UPDATE user_secrets SET auto_eligible = @auto_eligible, updated_at = now()
WHERE id = @id AND user_id = @user_id AND kind = @kind
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

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

-- name: UserHasAutoEligibleAnthropicToken :one
-- True iff the user has opted at least one Anthropic token into the auto-select pool
-- (PRD #111 M2 D2). The ephemeral provisioner (issue #804) reads this to decide a
-- burst worker's bind mode: `auto` when the pool is non-empty, else `default`, so an
-- auto worker never parks a run in pool_wait on an empty pool (autoselect.ReasonPoolEmpty).
SELECT EXISTS (
    SELECT 1 FROM user_secrets
    WHERE user_id = @user_id AND kind = 'anthropic_token' AND auto_eligible
);

-- name: ListSecretEnablement :many
SELECT id, kind, label, is_default, auto_eligible, created_at, updated_at,
       disabled_at, enablement_rev FROM user_secrets
WHERE user_id = @user_id ORDER BY kind, is_default DESC, lower(label);

-- name: GetSecretEnablement :one
SELECT id, kind, label, is_default, auto_eligible, created_at, updated_at,
       disabled_at, enablement_rev
FROM user_secrets WHERE id = @id AND user_id = @user_id;

-- name: SetSecretEnablement :one
UPDATE user_secrets
SET disabled_at = CASE WHEN @enabled::boolean THEN NULL ELSE now() END,
    enablement_rev = enablement_rev + 1, updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND (disabled_at IS NULL) <> @enabled::boolean
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at,
          disabled_at, enablement_rev;

-- name: CountEnabledSecretSlot :one
SELECT count(*) FROM user_secrets
WHERE user_id = @user_id AND disabled_at IS NULL
  AND (kind = @kind OR (@kind IN ('codex_auth', 'openai_api_key')
       AND kind IN ('codex_auth', 'openai_api_key')));

-- name: GetEnabledSecretReplacement :one
SELECT id, kind FROM user_secrets
WHERE id = @id AND user_id = @user_id AND disabled_at IS NULL
  AND (kind = @kind OR (@kind IN ('codex_auth', 'openai_api_key')
       AND kind IN ('codex_auth', 'openai_api_key')));

-- name: HasSecretDefaultSlot :one
SELECT EXISTS (SELECT 1 FROM user_secrets WHERE user_id = @user_id AND is_default
 AND (kind = @kind OR (@kind IN ('codex_auth', 'openai_api_key')
 AND kind IN ('codex_auth', 'openai_api_key'))));

-- name: PageSecretDependentWorkers :many
SELECT w.id, w.name FROM workers w WHERE w.user_id = @user_id AND w.anthropic_secret_id = @secret_id
 AND w.id > @after_id::uuid ORDER BY w.id LIMIT @page_size::int;

-- name: PageSecretDependentSchedules :many
SELECT sch.id, sch.target FROM run_schedules sch WHERE sch.user_id = @user_id
 AND sch.credential_override_secret_id = @secret_id
 AND sch.id > @after_id::uuid ORDER BY sch.id LIMIT @page_size::int;

-- name: PageSecretDependentRuns :many
SELECT r.id, r.status FROM runs r WHERE r.user_id = @user_id
 AND r.status NOT IN ('completed', 'failed', 'cancelled')
 AND (r.anthropic_secret_id = @secret_id OR r.codex_secret_id = @secret_id OR r.credential_override_secret_id = @secret_id)
 AND r.id > @after_id::uuid ORDER BY r.id LIMIT @page_size::int;

-- name: PageSecretDependentSiblings :many
SELECT s.id, s.label FROM codex_credential_state c
 JOIN codex_credential_state sibling ON sibling.provider_account_id = c.provider_account_id
 JOIN user_secrets s ON s.id = sibling.user_secret_id
 WHERE c.user_secret_id = @secret_id AND c.user_id = @user_id AND c.provider_account_id IS NOT NULL
 AND s.id <> @secret_id AND s.disabled_at IS NULL
 AND s.id > @after_id::uuid ORDER BY s.id LIMIT @page_size::int;

-- name: GetSecretDependents :one
SELECT
 (SELECT count(*) FROM workers w WHERE w.user_id = $1 AND w.anthropic_secret_id = $2::uuid)::bigint AS workers,
 (SELECT count(*) FROM run_schedules sch WHERE sch.user_id = $1 AND sch.credential_override_secret_id = $2)::bigint AS schedules,
 (SELECT count(*) FROM runs r WHERE r.user_id = $1
    AND r.status NOT IN ('completed', 'failed', 'cancelled')
    AND (r.anthropic_secret_id = $2 OR r.codex_secret_id = $2 OR r.credential_override_secret_id = $2))::bigint AS runs,
 (SELECT count(*) FROM users u WHERE u.id = $1 AND u.judge_anthropic_secret_id = $2)::bigint AS judge,
 (SELECT count(*) FROM codex_credential_state c
    JOIN codex_credential_state sibling ON sibling.provider_account_id = c.provider_account_id
    JOIN user_secrets s ON s.id = sibling.user_secret_id
    WHERE c.user_secret_id = $2 AND c.user_id = $1
      AND c.provider_account_id IS NOT NULL AND s.id <> $2 AND s.disabled_at IS NULL)::bigint AS enabled_siblings;
