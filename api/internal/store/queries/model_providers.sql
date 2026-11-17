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
SELECT count(*) FROM model_providers WHERE owner_user_id = $1;

-- name: InsertModelProvider :one
INSERT INTO model_providers (owner_user_id, name, engine, current_revision)
VALUES (@owner_user_id, @name, @engine, 1)
RETURNING id, owner_user_id, name, engine, current_revision, retired_at, created_at;

-- name: InsertModelProviderRevision :exec
-- A revision is immutable: this is the ONLY statement that writes model_provider_revisions rows
-- (there is no UPDATE; TestNoModelProviderRevisionOrKeyAssociationUpdate pins that over every
-- query file). An edit inserts the next revision.
INSERT INTO model_provider_revisions (provider_id, revision, protocol, base_url, auth_style, model_map)
VALUES (@provider_id, @revision, @protocol, @base_url, @auth_style, @model_map);

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
WHERE id = $1 AND owner_user_id = $2 AND retired_at IS NULL
RETURNING current_revision;

-- name: PruneModelProviderRevisions :execrows
-- Deletes the provider's non-current revisions that NO run row references (any status). Its own
-- statement after the edit commit, never inside the edit transaction: a run-create that froze the
-- previous revision before the edit committed is visible to this statement by then.
DELETE FROM model_provider_revisions rev
USING model_providers p
WHERE rev.provider_id = $1
  AND p.id = rev.provider_id
  AND p.owner_user_id = $2
  AND rev.revision <> p.current_revision
  AND NOT EXISTS (
        SELECT 1 FROM runs r
         WHERE r.model_provider_id = rev.provider_id
           AND r.model_provider_revision = rev.revision);

-- name: LockModelProviderForShare :one
-- The freeze lock for run creation (a later milestone): a creator takes this share lock before it
-- reads the current revision, so a concurrent edit (BumpModelProviderRevision) waits until the
-- creator's transaction ends and a run is never bound to a revision that is edited away under it.
SELECT id, current_revision, retired_at FROM model_providers
WHERE id = $1 AND owner_user_id = $2
FOR SHARE;

-- name: RenameModelProvider :execrows
UPDATE model_providers SET name = $3 WHERE id = $1 AND owner_user_id = $2;

-- name: RetireModelProvider :execrows
-- Retiring is idempotent: the first retirement time is kept.
UPDATE model_providers SET retired_at = COALESCE(retired_at, now()) WHERE id = $1 AND owner_user_id = $2;

-- name: DeleteModelProvider :execrows
-- Owner-scoped. Cascades to the provider's revisions, probe results and key. Refused with a
-- foreign-key violation (users_claude_provider_fkey, ON DELETE RESTRICT) while any user has the
-- provider selected as their source; the handler maps that to provider_in_use_as_source.
DELETE FROM model_providers WHERE id = $1 AND owner_user_id = $2;

-- name: ListModelProvidersForOwner :many
SELECT p.id, p.name, p.engine, p.current_revision, p.retired_at, p.created_at,
       r.protocol, r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.disabled_at AS key_disabled_at,
       pr.auth_result AS probe_auth_result, pr.tool_result AS probe_tool_result, pr.probed_at AS probed_at
FROM model_providers p
JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = p.owner_user_id AND k.provider_id = p.id
LEFT JOIN model_provider_probe_results pr ON pr.provider_id = p.id AND pr.revision = p.current_revision
WHERE p.owner_user_id = $1
ORDER BY lower(p.name), p.id;

-- name: GetModelProviderForOwner :one
SELECT p.id, p.name, p.engine, p.current_revision, p.retired_at, p.created_at,
       r.protocol, r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.disabled_at AS key_disabled_at,
       pr.auth_result AS probe_auth_result, pr.tool_result AS probe_tool_result, pr.probed_at AS probed_at
FROM model_providers p
JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = p.owner_user_id AND k.provider_id = p.id
LEFT JOIN model_provider_probe_results pr ON pr.provider_id = p.id AND pr.revision = p.current_revision
WHERE p.id = $1 AND p.owner_user_id = $2;

-- name: InsertProviderKeySecret :one
-- Stores a provider's gateway key (kind anthropic_compatible_key), the ONLY writer of that kind.
-- Deliberately NOT InsertUserSecret: that statement forces the first secret of a kind to be the
-- default and the first anthropic_token into the auto-select pool, and a provider key must be
-- neither (is_default=false, auto_eligible=false), so it never enters a default slot, the poller,
-- the weekly reserve or pool selection. provider_id is written here and by no UPDATE. The composite
-- foreign key (user_id, provider_id) keeps the key under the provider's owner; the unique index
-- user_secrets_one_provider_key allows one key per (user, provider). The label is the provider id.
INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id)
VALUES (@user_id, 'anthropic_compatible_key', @label, false, false, @ciphertext, @sealed_with, @provider_id)
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

-- name: GetProviderKeyForUpdate :one
-- The provider key's identity and enablement, locked inside a mutation transaction.
SELECT id, disabled_at, enablement_rev FROM user_secrets
WHERE user_id = $1 AND provider_id = $2 AND kind = 'anthropic_compatible_key'
FOR UPDATE;

-- name: DeleteProviderKeySecret :execrows
DELETE FROM user_secrets
WHERE user_id = $1 AND provider_id = $2 AND kind = 'anthropic_compatible_key';
