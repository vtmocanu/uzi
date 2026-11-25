-- name: LockModelProviderOwner :exec
-- Serializes one owner's provider creates so the per-owner cap (20, retired providers included)
-- cannot be passed twice by two concurrent creates under READ COMMITTED: the create transaction
-- runs this lock first, then CountModelProvidersForOwner, then InsertModelProvider. Same
-- reasoning as LockJobCreate.
--
-- Two-int advisory lock: class 1970957680 = 0x757A6D70 ("uzmp"), the value of
-- store.ModelProviderOwnerLockClass in migrate.go; TestModelProviderOwnerLockClassMatchesSQL pins
-- this literal to it. XACT-scoped: it must run on a transaction-bound Queries (on a bare pool it
-- would release immediately). Lock order in the handler: the secret-mutation lock first
-- (withSecretLock), then this one.
SELECT pg_advisory_xact_lock(
    1970957680,
    hashtext(sqlc.arg(owner_user_id)::uuid::text)
);

-- name: CountModelProvidersForOwner :one
-- The cap count: every provider the owner has ever created and not deleted, retired included
-- (retiring must not free a slot, or create/retire cycles would be unbounded). Run under
-- LockModelProviderOwner.
SELECT count(*) FROM model_providers WHERE owner_user_id = @owner_user_id::uuid;

-- name: InsertModelProvider :one
-- A personal definition: scope_owner_id is the owner (the shared scope is the nil UUID, written only
-- by InsertSharedModelProvider). credential_mode stays NULL: it is set exactly on shared rows.
INSERT INTO model_providers (owner_user_id, name, engine, current_revision, scope_owner_id)
VALUES (@owner_user_id::uuid, @name, @engine, 1, @owner_user_id::uuid)
RETURNING id, owner_user_id, name, engine, current_revision, retired_at, created_at, credential_mode, scope_owner_id;

-- name: InsertModelProviderRevision :exec
-- A revision is immutable: this is the ONLY statement that writes model_provider_revisions rows
-- (there is no UPDATE; TestNoModelProviderRevisionOrKeyAssociationUpdate pins that over every
-- query file). An edit inserts the next revision.
INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map, engine)
SELECT p.id, @revision, @protocol, @base_url, @auth_style, @model_map, p.engine
FROM model_providers p WHERE p.id = @provider_id;

-- name: GetModelProviderRevision :one
SELECT provider_id, revision, protocol, base_url, auth_style, model_map, created_at
FROM model_provider_revisions
WHERE provider_id = $1 AND revision = $2;

-- name: BumpModelProviderRevision :one
-- Advancing current_revision takes the provider row's exclusive lock (RenameModelProvider takes it
-- too, so in an edit the provider row is row-locked by one of them before the revision insert), so
-- it waits for any run-create that holds LockModelProviderForShare and blocks the next run-create
-- until the edit commits. Edits run under the per-user secret-mutation lock; a run-create must take
-- LockSecretMutationShared before FOR SHARE on the provider row, or the two lock orders cycle. The
-- caller then inserts the revision this returns. Owner-scoped; a retired provider is not edited
-- (0 rows).
UPDATE model_providers
SET current_revision = current_revision + 1
WHERE id = @id AND owner_user_id = @owner_user_id::uuid AND retired_at IS NULL
RETURNING current_revision;

-- name: PruneModelProviderRevisions :execrows
-- Deletes the provider's non-current revisions that NO run row references (any status). Its own
-- statement after the edit commit, never inside the edit transaction: a run-create that froze the
-- previous revision before the edit committed is visible to this statement by then.
DELETE FROM model_provider_revisions rev
USING model_providers p
WHERE rev.provider_id = @provider_id
  AND p.id = rev.provider_id
  AND p.owner_user_id = @owner_user_id::uuid
  AND rev.revision <> p.current_revision
  AND NOT EXISTS (
        SELECT 1 FROM runs r
         WHERE r.model_provider_id = rev.provider_id
           AND r.model_provider_revision = rev.revision);

-- name: LockModelProviderForShare :one
-- The freeze lock for run creation: a creator takes this share lock before it reads the current
-- revision, so a concurrent edit (BumpModelProviderRevision, BumpSharedModelProviderRevision) waits
-- until the creator's transaction ends and a run is never bound to a revision that is edited away
-- under it. owner_user_id is the CALLER: the provider must be visible to them, their own or a shared
-- per-user-key definition.
SELECT p.id, p.current_revision, p.retired_at FROM model_providers p
WHERE p.id = @id
  AND (p.owner_user_id = @owner_user_id::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
FOR SHARE OF p;

-- name: RenameModelProvider :execrows
UPDATE model_providers SET name = @name WHERE id = @id AND owner_user_id = @owner_user_id::uuid;

-- name: RetireModelProvider :execrows
-- Retiring is idempotent: the first retirement time is kept.
UPDATE model_providers SET retired_at = COALESCE(retired_at, now()) WHERE id = @id AND owner_user_id = @owner_user_id::uuid;

-- name: DeleteModelProvider :execrows
-- Owner-scoped. Cascades to the provider's revisions, probe results and key. Refused with a
-- foreign-key violation (users_claude_provider_fkey, ON DELETE RESTRICT) while any user has the
-- provider selected as their source; the handler maps that to provider_in_use_as_source.
DELETE FROM model_providers WHERE id = @id AND owner_user_id = @owner_user_id::uuid;

-- name: ListModelProvidersForOwner :many
-- Owner-only: the owner's own definitions with their key and the Test evidence for that exact key
-- (the evidence row joins only while its provider revision and key identity and enablement revision
-- are all current). Shared definitions are read through ListVisibleProviders.
SELECT p.id, p.name, p.engine, p.current_revision, p.retired_at, p.created_at,
       r.protocol, r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev,
       pr.auth_result AS probe_auth_result, pr.tool_result AS probe_tool_result, pr.probed_at AS probed_at
FROM model_providers p
JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = p.owner_user_id AND k.provider_id = p.id
LEFT JOIN model_provider_key_test_results pr ON pr.provider_id = p.id AND pr.user_id = p.owner_user_id
     AND pr.revision = p.current_revision AND pr.key_secret_id = k.id AND pr.key_enablement_rev = k.enablement_rev
WHERE p.owner_user_id = @owner_user_id::uuid
ORDER BY lower(p.name), p.id;

-- name: GetModelProviderForOwner :one
SELECT p.id, p.name, p.engine, p.current_revision, p.retired_at, p.created_at,
       r.protocol, r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev,
       pr.auth_result AS probe_auth_result, pr.tool_result AS probe_tool_result, pr.probed_at AS probed_at
FROM model_providers p
JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = p.owner_user_id AND k.provider_id = p.id
LEFT JOIN model_provider_key_test_results pr ON pr.provider_id = p.id AND pr.user_id = p.owner_user_id
     AND pr.revision = p.current_revision AND pr.key_secret_id = k.id AND pr.key_enablement_rev = k.enablement_rev
WHERE p.id = @id AND p.owner_user_id = @owner_user_id::uuid;

-- name: ListRunProviderNames :many
-- The current name of the provider each given run was bound to (PRD #4320 run surfaces). The join
-- matches the provider only when it is visible to the RUN'S owner: their own definition or a shared
-- per-user-key one. Another user's private definition never resolves, so a name can never cross
-- users; a run with no binding, or whose provider row has since been deleted (runs carry no foreign
-- key to model_providers), returns no row.
SELECT r.id AS run_id, p.name
FROM runs r
JOIN model_providers p ON p.id = r.model_provider_id
 AND (p.owner_user_id = r.user_id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
WHERE r.id = ANY(@run_ids::uuid[]);

-- name: UpsertModelProviderProbeResult :execrows
-- LEGACY provider-wide cache, no longer written or read by the current API: Test evidence now lives
-- in model_provider_key_test_results (UpsertProviderKeyTestResult). Kept for the image-only rollback
-- test, which proves an older API image's upsert still works against the retained table.
-- Stores the latest Test probe result for the provider's CURRENT revision only (PRD #4320 section
-- 8): the row is written only while the owner's provider still has that revision as its current
-- one, so a probe that raced an edit (or a retire/delete) writes nothing and returns 0 rows. One
-- row per (provider, revision); a later probe replaces it.
INSERT INTO model_provider_probe_results (provider_id, revision, auth_result, tool_result, probed_at)
SELECT p.id, p.current_revision, @auth_result, @tool_result, now()
FROM model_providers p
WHERE p.id = @provider_id AND p.owner_user_id = @owner_user_id::uuid AND p.current_revision = @revision
ON CONFLICT (provider_id, revision) DO UPDATE
SET auth_result = EXCLUDED.auth_result, tool_result = EXCLUDED.tool_result, probed_at = EXCLUDED.probed_at;

-- name: InsertProviderKeySecret :one
-- Stores the CALLER's gateway key for a definition visible to them (their own, or a shared per-user-key
-- one); derives its kind from the definition's engine. The key is always the caller's, never the
-- admin's: user_id is the caller and provider_scope_id is the definition's scope (the caller's own id,
-- or the nil UUID for a shared Claude definition).
-- Deliberately NOT InsertUserSecret: that statement forces the first secret of a kind to be the
-- default and the first anthropic_token into the auto-select pool, and a provider key must be
-- neither (is_default=false, auto_eligible=false), so it never enters a default slot, the poller,
-- the weekly reserve or pool selection. provider_id and provider_scope_id are written here and by no
-- UPDATE. The composite foreign key (provider_scope_id, provider_id) keeps the key under a scope the
-- caller may use, and user_secrets_provider_scope_check pins that scope to the caller's id or, for a
-- Claude key, the shared scope; the unique index user_secrets_one_provider_key allows one key per
-- (user, provider). The label is the provider id.
INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id, provider_scope_id)
SELECT @user_id::uuid, CASE p.engine WHEN 'claude' THEN 'anthropic_compatible_key' WHEN 'codex' THEN 'openai_compatible_key' END,
       @label, false, false, @ciphertext, @sealed_with, @provider_id, p.scope_owner_id
FROM model_providers p
WHERE p.id = @provider_id
  AND (p.owner_user_id = @user_id::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

-- name: GetProviderKeyForUpdate :one
-- The caller's provider key identity and enablement, locked inside a mutation transaction. user_id
-- is the CALLER; the definition must be visible to them (their own or a shared per-user-key one).
SELECT k.id, k.disabled_at, k.enablement_rev FROM user_secrets k
JOIN model_providers p ON p.id = k.provider_id
 AND (p.owner_user_id = k.user_id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
WHERE k.user_id = $1 AND k.provider_id = $2
  AND k.kind = CASE p.engine WHEN 'claude' THEN 'anthropic_compatible_key' WHEN 'codex' THEN 'openai_compatible_key' END
FOR UPDATE OF k;

-- name: DeleteProviderKeySecret :execrows
DELETE FROM user_secrets
WHERE user_id = $1 AND provider_id = $2 AND kind IN ('anthropic_compatible_key', 'openai_compatible_key');

-- name: GetProviderBindingKeyByID :one
-- A bound run's frozen key, re-read at inherited creation: the secret row by id, owner-scoped.
SELECT id, kind, provider_id, disabled_at, enablement_rev FROM user_secrets
WHERE id = $1 AND user_id = $2;

-- name: LockProviderBindingKeyForShareNowait :one
-- The claim finisher's in-transaction re-check of a PROVIDER-BOUND run's frozen key (PRD #4320
-- module 5, D9), taken under the exact-claim run lock. FOR SHARE blocks a concurrent rotation,
-- disable or delete (which lock the row FOR UPDATE under the owner's secret mutation lock) until the
-- claim decision commits; NOWAIT means the finisher never waits on that writer while holding the
-- run row: a held row is 55P03 and finishRunClaim / finishChatClaim retry the whole transaction.
-- Owner-scoped; a missing row is pgx.ErrNoRows (the key was deleted: stale).
SELECT id, kind, provider_id, disabled_at, enablement_rev FROM user_secrets
WHERE id = $1 AND user_id = $2
FOR SHARE NOWAIT;

-- name: LockProviderBindingRevisionForKeyShareNowait :one
-- The claim finisher's revision read (PRD #4320 module 5, D9): the same SELECT as
-- GetModelProviderRevisionForOwner but FOR KEY SHARE OF r NOWAIT. The finisher already holds the run
-- row and the key row FOR SHARE; a provider delete cascades the revision and key rows in either
-- order, so a BLOCKING revision lock here against a delete mid-cascade can be a lock cycle (40P01,
-- which the finishers do not retry). NOWAIT turns that contention into 55P03, which finishRunClaim /
-- finishChatClaim already retry as a bounded whole-transaction retry. A missing row is
-- pgx.ErrNoRows (stale).
SELECT r.provider_id, r.revision, r.protocol, r.base_url, r.auth_style, r.model_map, p.engine
FROM model_provider_revisions r
JOIN model_providers p ON p.id = r.provider_id
WHERE r.provider_id = @provider_id AND r.revision = @revision
  AND (p.owner_user_id = @owner_user_id::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
FOR KEY SHARE OF r NOWAIT;

-- name: GetModelProviderRevisionForOwner :one
-- A frozen revision of a provider visible to the CALLER (owner_user_id is the caller: their own
-- definition or a shared per-user-key one); inherited creation re-checks the URL against the
-- allowlist. FOR KEY SHARE OF r only protects the prune-FIRST ordering (a prune that
-- already took the row's key lock makes this read wait, then find the row gone): it is released with
-- the transaction and does not by itself keep the revision. What keeps PruneModelProviderRevisions off
-- a revision is the source run row's own reference (model_provider_revision), which stays true while
-- run rows are never deleted.
SELECT r.provider_id, r.revision, r.protocol, r.base_url, r.auth_style, r.model_map, p.engine
FROM model_provider_revisions r
JOIN model_providers p ON p.id = r.provider_id
WHERE r.provider_id = @provider_id AND r.revision = @revision
  AND (p.owner_user_id = @owner_user_id::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
FOR KEY SHARE OF r;

-- name: GetUserProviderSource :one
-- The caller's selected source for one engine with everything run creation needs to judge it usable
-- in ONE read: the setting, the provider, its CURRENT revision and the CALLER's key row for it. The
-- Claude arm resolves the caller's own definition or a shared per-user-key one; the Codex arm stays
-- owner-only (a shared definition is never a Codex source). The key join is always the caller's own
-- (k.user_id = u.id), so a shared definition never resolves another user's key. A NULL setting
-- returns a row whose provider columns are all NULL (no rows only for an unknown user). Plain read:
-- the freeze takes LockSecretMutationShared and then LockModelProviderForShare around its own re-read.
SELECT u.claude_provider_id AS provider_id,
       p.engine, r.protocol, p.current_revision, p.retired_at,
       r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.kind AS key_kind, k.provider_id AS key_provider_id,
       k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev
FROM users u
LEFT JOIN model_providers p ON p.id = u.claude_provider_id
 AND (p.owner_user_id = u.id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key')) AND p.engine = @engine
LEFT JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = u.id AND k.provider_id = p.id AND k.kind = CASE p.engine WHEN 'claude' THEN 'anthropic_compatible_key' WHEN 'codex' THEN 'openai_compatible_key' END
WHERE u.id = @id AND @engine::text = 'claude'
UNION ALL
SELECT u.codex_provider_id AS provider_id,
       p.engine, r.protocol, p.current_revision, p.retired_at,
       r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.kind AS key_kind, k.provider_id AS key_provider_id,
       k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev
FROM users u
LEFT JOIN model_providers p ON p.id = u.codex_provider_id AND p.owner_user_id = u.id AND p.engine = @engine
LEFT JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = u.id AND k.provider_id = p.id AND k.kind = CASE p.engine WHEN 'claude' THEN 'anthropic_compatible_key' WHEN 'codex' THEN 'openai_compatible_key' END
WHERE u.id = @id AND @engine::text = 'codex';


-- name: GetUserProviderLaneSources :one
-- One owner snapshot, without opening keys or resolving either source.
SELECT claude_provider_id, codex_provider_id FROM users WHERE id = $1;

-- name: SetUserProviderSource :one
-- Sets or clears (NULL) the caller's source for one engine; a NULL provider_id CLEARS the setting and
-- the engine must be named. The Claude branch accepts a definition visible to the caller (their own
-- or a shared per-user-key one) and writes claude_provider_scope_id from that definition in the same
-- statement (NULL when clearing); the Codex branch accepts only the caller's own Codex definition and
-- never touches the Claude scope. The composite foreign keys (users_claude_provider_fkey,
-- users_codex_provider_fkey) independently reject a definition the caller may not use. Usability is
-- checked by the caller, which holds the user's secret-mutation lock.
UPDATE users u
SET claude_provider_id = CASE WHEN @engine::text = 'claude' THEN sqlc.narg('provider_id')::uuid ELSE claude_provider_id END,
    claude_provider_scope_id = CASE WHEN @engine::text = 'claude'
        THEN (SELECT sp.scope_owner_id FROM model_providers sp WHERE sp.id = sqlc.narg('provider_id')::uuid)
        ELSE claude_provider_scope_id END,
    codex_provider_id = CASE WHEN @engine::text = 'codex' THEN sqlc.narg('provider_id')::uuid ELSE codex_provider_id END
WHERE u.id = @id AND @engine::text IN ('claude', 'codex')
  AND (sqlc.narg('provider_id')::uuid IS NULL
       OR (@engine::text = 'claude' AND EXISTS (
            SELECT 1 FROM model_providers p WHERE p.id = sqlc.narg('provider_id')::uuid
              AND (p.owner_user_id = u.id OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
              AND p.engine = 'claude'))
       OR (@engine::text = 'codex' AND EXISTS (
            SELECT 1 FROM model_providers p WHERE p.id = sqlc.narg('provider_id')::uuid
              AND p.owner_user_id = u.id AND p.engine = 'codex')))
RETURNING claude_provider_id, codex_provider_id;

-- name: ListVisibleProviders :many
-- Every definition visible to the CALLER: their own (either engine) and the shared per-user-key
-- catalogue (Claude only). The key and the Test evidence are always the CALLER's: the key join is
-- (caller, definition, the engine's key kind) and the evidence joins only while its provider revision,
-- key identity and key enablement revision are all current, so a rotated or edited definition reads as
-- not tested and another user's key or result never appears. owner_user_id is NULL on a shared row.
SELECT p.id, p.name, p.engine, p.current_revision, p.retired_at, p.created_at,
       p.credential_mode, p.owner_user_id,
       r.protocol, r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev,
       t.auth_result AS probe_auth_result, t.tool_result AS probe_tool_result, t.probed_at AS probed_at
FROM model_providers p
JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = @user_id::uuid AND k.provider_id = p.id
     AND k.kind = CASE p.engine WHEN 'claude' THEN 'anthropic_compatible_key' WHEN 'codex' THEN 'openai_compatible_key' END
LEFT JOIN model_provider_key_test_results t ON t.provider_id = p.id AND t.user_id = @user_id::uuid
     AND t.revision = p.current_revision AND t.key_secret_id = k.id AND t.key_enablement_rev = k.enablement_rev
WHERE (p.owner_user_id = @user_id::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
ORDER BY lower(p.name), p.id;

-- name: GetVisibleProviderForUse :one
-- One definition visible to the CALLER, with the same columns and caller-bound key and evidence joins
-- as ListVisibleProviders. A private definition of another user is no row (404 to the caller).
SELECT p.id, p.name, p.engine, p.current_revision, p.retired_at, p.created_at,
       p.credential_mode, p.owner_user_id,
       r.protocol, r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev,
       t.auth_result AS probe_auth_result, t.tool_result AS probe_tool_result, t.probed_at AS probed_at
FROM model_providers p
JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = @user_id::uuid AND k.provider_id = p.id
     AND k.kind = CASE p.engine WHEN 'claude' THEN 'anthropic_compatible_key' WHEN 'codex' THEN 'openai_compatible_key' END
LEFT JOIN model_provider_key_test_results t ON t.provider_id = p.id AND t.user_id = @user_id::uuid
     AND t.revision = p.current_revision AND t.key_secret_id = k.id AND t.key_enablement_rev = k.enablement_rev
WHERE p.id = @id
  AND (p.owner_user_id = @user_id::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'));

-- name: LockVisibleProviderForKeyMutation :one
-- Enable, disable and delete take the visible definition's FOR KEY SHARE lock before the
-- caller's key row. It blocks a definition delete, and retired definitions remain eligible.
-- Key PUT instead takes LockModelProviderForShare to fence retirement and edits.
SELECT p.id, p.engine, p.current_revision, p.retired_at, p.credential_mode, p.owner_user_id
FROM model_providers p
WHERE p.id = @id
  AND (p.owner_user_id = @user_id::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
FOR KEY SHARE OF p;

-- name: LockVisibleProviderForEvidence :one
-- First half of the Test evidence fence, run in one short transaction AFTER the probe returns (no lock
-- spans the dial): the definition must still be visible to the CALLER, still at the probed revision and
-- not retired. FOR SHARE (not KEY SHARE) so a revision bump, which takes FOR NO KEY UPDATE, waits for
-- this transaction instead of committing past it. Followed by LockProviderKeyForEvidence, then
-- UpsertProviderKeyTestResult.
SELECT p.id, p.engine, p.current_revision
FROM model_providers p
WHERE p.id = @id
  AND (p.owner_user_id = @user_id::uuid OR (p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'))
  AND p.current_revision = @revision
  AND p.retired_at IS NULL
FOR SHARE OF p;

-- name: LockProviderKeyForEvidence :one
-- Second half of the Test evidence fence: the caller's key row for this definition, locked FOR SHARE,
-- only while it is the exact key the probe used (same row id and enablement revision) and enabled. A
-- rotation, disable or delete that committed first finds no row here; one still in flight holds the row
-- FOR UPDATE, so this waits for it and re-checks (READ COMMITTED), which also finds no row. Identity
-- is the exact key row id and provider id: it takes no visibility predicate because the provider lock
-- just before it already proved visibility.
SELECT k.id, k.enablement_rev FROM user_secrets k
WHERE k.id = @key_id AND k.user_id = @user_id AND k.provider_id = @provider_id
  AND k.enablement_rev = @key_rev AND k.disabled_at IS NULL
FOR SHARE;

-- name: UpsertProviderKeyTestResult :exec
-- Records the Test evidence for one (provider, user): the exact tuple the probe ran against. Called only
-- after LockVisibleProviderForEvidence and LockProviderKeyForEvidence both returned a row in the same
-- transaction; the foreign keys (provider revision, user, user key) are the structural backstop.
INSERT INTO model_provider_key_test_results (provider_id, user_id, revision, key_secret_id, key_enablement_rev, auth_result, tool_result, probed_at)
VALUES (@provider_id, @user_id, @revision, @key_secret_id, @key_enablement_rev, @auth_result, @tool_result, now())
ON CONFLICT (provider_id, user_id) DO UPDATE
SET revision = EXCLUDED.revision, key_secret_id = EXCLUDED.key_secret_id,
    key_enablement_rev = EXCLUDED.key_enablement_rev, auth_result = EXCLUDED.auth_result,
    tool_result = EXCLUDED.tool_result, probed_at = EXCLUDED.probed_at;

-- name: LockSharedModelProviderCatalogue :exec
-- Serializes shared-definition creates so the shared cap (retired definitions included) cannot be passed
-- twice by two concurrent admins under READ COMMITTED: create runs this lock, then
-- CountSharedModelProviders, then InsertSharedModelProvider. The same advisory class as
-- LockModelProviderOwner (1970957680, store.ModelProviderOwnerLockClass), keyed by the nil UUID, which
-- no real owner can use (model_providers_owner_not_nil_check). XACT-scoped: it must run on a
-- transaction-bound Queries. Shared admin edits take NO per-user secret-mutation lock, and no lock
-- other than the definition row's own.
-- Authorized caller: the admin create path (cookie + CSRF) only.
SELECT pg_advisory_xact_lock(
    1970957680,
    hashtext('00000000-0000-0000-0000-000000000000')
);

-- name: CountSharedModelProviders :one
-- The shared cap count: retired definitions included (retiring must not free a slot). Run under
-- LockSharedModelProviderCatalogue. Authorized caller: the admin create path.
SELECT count(*) FROM model_providers WHERE owner_user_id IS NULL AND credential_mode = 'per_user_key';

-- name: InsertSharedModelProvider :one
-- A shared definition: NULL owner, per-user-key mode, the nil shared scope, Claude only. The CHECKs
-- (model_providers_shared_mode_check, _shared_engine_check, _scope_owner_check) enforce the same shape.
-- Authorized caller: the admin create path.
INSERT INTO model_providers (owner_user_id, name, engine, current_revision, credential_mode, scope_owner_id)
VALUES (NULL, @name, 'claude', 1, 'per_user_key', '00000000-0000-0000-0000-000000000000'::uuid)
RETURNING id, owner_user_id, name, engine, current_revision, retired_at, created_at, credential_mode, scope_owner_id;

-- name: LockSharedModelProviderForEdit :one
-- The edit transaction's first statement for a shared definition: FOR NO KEY UPDATE takes the same lock
-- the bump and the rename would, so the order is fixed up front and a concurrent run-create share lock
-- (LockModelProviderForShare) waits for the edit. No per-user lock is involved. A retired definition is
-- returned so the handler can refuse it with the right error. Authorized caller: admin edit.
SELECT p.id, p.name, p.current_revision, p.retired_at FROM model_providers p
WHERE p.id = @id AND p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'
FOR NO KEY UPDATE OF p;

-- name: LockSharedModelProviderForDelete :one
-- The delete transaction's first statement for a shared definition: FOR UPDATE conflicts with every other
-- row lock (including the KEY SHARE of key mutations and the FOR SHARE of run creation and evidence), so
-- the selection and key counts that follow cannot change before the delete commits. Authorized caller:
-- admin delete.
SELECT p.id, p.name, p.current_revision, p.retired_at FROM model_providers p
WHERE p.id = @id AND p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'
FOR UPDATE OF p;

-- name: BumpSharedModelProviderRevision :one
-- Advances a shared definition's current_revision (the caller then inserts that revision). A retired
-- definition is not edited (0 rows). Authorized caller: admin edit, after LockSharedModelProviderForEdit.
UPDATE model_providers
SET current_revision = current_revision + 1
WHERE id = @id AND owner_user_id IS NULL AND credential_mode = 'per_user_key' AND retired_at IS NULL
RETURNING current_revision;

-- name: RenameSharedModelProvider :execrows
-- Authorized caller: admin edit. A case-insensitive name collision is a unique violation
-- (model_providers_shared_name_lower_key).
UPDATE model_providers SET name = @name
WHERE id = @id AND owner_user_id IS NULL AND credential_mode = 'per_user_key';

-- name: RetireSharedModelProvider :execrows
-- Idempotent: the first retirement time is kept. Authorized caller: admin retire.
UPDATE model_providers SET retired_at = COALESCE(retired_at, now())
WHERE id = @id AND owner_user_id IS NULL AND credential_mode = 'per_user_key';

-- name: PruneSharedModelProviderRevisions :execrows
-- Deletes a shared definition's non-current revisions that NO run row references, as
-- PruneModelProviderRevisions does for a personal one: its own statement after the edit commit.
-- Authorized caller: admin edit.
DELETE FROM model_provider_revisions rev
USING model_providers p
WHERE rev.provider_id = @id
  AND p.id = rev.provider_id
  AND p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'
  AND rev.revision <> p.current_revision
  AND NOT EXISTS (
        SELECT 1 FROM runs r
         WHERE r.model_provider_id = rev.provider_id
           AND r.model_provider_revision = rev.revision);

-- name: DeleteSharedProviderKeys :many
-- Deletes every user's key attached to one shared definition and returns the ids, so the delete's
-- warning and audit record the exact count under the same provider lock. The shared scope is the nil
-- UUID, which only a Claude gateway key may carry. The cascading foreign keys then remove their Test
-- evidence. Authorized caller: admin delete, after LockSharedModelProviderForDelete and a zero
-- CountSharedProviderSelections.
DELETE FROM user_secrets
WHERE provider_id = @id::uuid AND provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid
RETURNING id;

-- name: CountSharedProviderSelections :one
-- How many users have this shared definition selected as their Claude source. A delete is refused
-- while it is above zero (users_claude_provider_fkey is ON DELETE RESTRICT as the structural backstop).
-- Authorized caller: admin delete.
SELECT count(*) FROM users
WHERE claude_provider_id = @id::uuid AND claude_provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid;

-- name: DeleteSharedModelProvider :execrows
-- Cascades to the definition's revisions and legacy probe rows. Refused with a foreign-key violation
-- while any user has it selected. Authorized caller: admin delete.
DELETE FROM model_providers WHERE id = @id AND owner_user_id IS NULL AND credential_mode = 'per_user_key';

-- name: ListSharedModelProvidersAdmin :many
-- The shared catalogue as an admin sees it: the current revision's fields and two counts, never a key
-- or a per-user result. Authorized caller: admin list (cookie, or the admin-read token).
SELECT p.id, p.name, p.engine, p.current_revision, p.retired_at, p.created_at,
       r.protocol, r.base_url, r.auth_style, r.model_map,
       COALESCE(su.n, 0)::bigint AS selected_user_count,
       COALESCE(ak.n, 0)::bigint AS attached_key_count
FROM model_providers p
JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN (SELECT claude_provider_id AS pid, count(*) AS n FROM users
            WHERE claude_provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid
            GROUP BY claude_provider_id) su ON su.pid = p.id
LEFT JOIN (SELECT provider_id AS pid, count(*) AS n FROM user_secrets
            WHERE provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid
            GROUP BY provider_id) ak ON ak.pid = p.id
WHERE p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key'
ORDER BY lower(p.name), p.id;

-- name: GetSharedModelProviderAdmin :one
-- One shared definition as ListSharedModelProvidersAdmin returns it. Authorized caller: admin get.
SELECT p.id, p.name, p.engine, p.current_revision, p.retired_at, p.created_at,
       r.protocol, r.base_url, r.auth_style, r.model_map,
       COALESCE(su.n, 0)::bigint AS selected_user_count,
       COALESCE(ak.n, 0)::bigint AS attached_key_count
FROM model_providers p
JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN (SELECT claude_provider_id AS pid, count(*) AS n FROM users
            WHERE claude_provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid
            GROUP BY claude_provider_id) su ON su.pid = p.id
LEFT JOIN (SELECT provider_id AS pid, count(*) AS n FROM user_secrets
            WHERE provider_scope_id = '00000000-0000-0000-0000-000000000000'::uuid
            GROUP BY provider_id) ak ON ak.pid = p.id
WHERE p.id = @id AND p.owner_user_id IS NULL AND p.credential_mode = 'per_user_key';
