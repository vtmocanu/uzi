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
       k.id AS key_secret_id, k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev,
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
       k.id AS key_secret_id, k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev,
       pr.auth_result AS probe_auth_result, pr.tool_result AS probe_tool_result, pr.probed_at AS probed_at
FROM model_providers p
JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = p.owner_user_id AND k.provider_id = p.id
LEFT JOIN model_provider_probe_results pr ON pr.provider_id = p.id AND pr.revision = p.current_revision
WHERE p.id = $1 AND p.owner_user_id = $2;

-- name: ListRunProviderNames :many
-- The current name of the provider each given run was bound to (PRD #4320 run surfaces). The join
-- matches the provider only under the RUN'S owner (the composite owner key), so a name can never
-- cross users; a run with no binding, or whose provider row has since been deleted (runs carry no
-- foreign key to model_providers), returns no row.
SELECT r.id AS run_id, p.name
FROM runs r
JOIN model_providers p ON p.id = r.model_provider_id AND p.owner_user_id = r.user_id
WHERE r.id = ANY(@run_ids::uuid[]);

-- name: UpsertModelProviderProbeResult :execrows
-- Stores the latest Test probe result for the provider's CURRENT revision only (PRD #4320 section
-- 8): the row is written only while the owner's provider still has that revision as its current
-- one, so a probe that raced an edit (or a retire/delete) writes nothing and returns 0 rows. One
-- row per (provider, revision); a later probe replaces it.
INSERT INTO model_provider_probe_results (provider_id, revision, auth_result, tool_result, probed_at)
SELECT p.id, p.current_revision, @auth_result, @tool_result, now()
FROM model_providers p
WHERE p.id = @provider_id AND p.owner_user_id = @owner_user_id AND p.current_revision = @revision
ON CONFLICT (provider_id, revision) DO UPDATE
SET auth_result = EXCLUDED.auth_result, tool_result = EXCLUDED.tool_result, probed_at = EXCLUDED.probed_at;

-- name: InsertProviderKeySecret :one
-- Stores a provider's gateway key; derives its kind from the owned provider engine.
-- Deliberately NOT InsertUserSecret: that statement forces the first secret of a kind to be the
-- default and the first anthropic_token into the auto-select pool, and a provider key must be
-- neither (is_default=false, auto_eligible=false), so it never enters a default slot, the poller,
-- the weekly reserve or pool selection. provider_id is written here and by no UPDATE. The composite
-- foreign key (user_id, provider_id) keeps the key under the provider's owner; the unique index
-- user_secrets_one_provider_key allows one key per (user, provider). The label is the provider id.
INSERT INTO user_secrets (user_id, kind, label, is_default, auto_eligible, ciphertext, sealed_with, provider_id)
SELECT @user_id, CASE p.engine WHEN 'claude' THEN 'anthropic_compatible_key' WHEN 'codex' THEN 'openai_compatible_key' END,
       @label, false, false, @ciphertext, @sealed_with, @provider_id
FROM model_providers p
WHERE p.id = @provider_id AND p.owner_user_id = @user_id
RETURNING id, kind, label, is_default, auto_eligible, created_at, updated_at;

-- name: GetProviderKeyForUpdate :one
-- The provider key's identity and enablement, locked inside a mutation transaction.
SELECT k.id, k.disabled_at, k.enablement_rev FROM user_secrets k
JOIN model_providers p ON p.id = k.provider_id AND p.owner_user_id = k.user_id
WHERE k.user_id = $1 AND k.provider_id = $2
  AND k.kind = CASE p.engine WHEN 'claude' THEN 'anthropic_compatible_key' WHEN 'codex' THEN 'openai_compatible_key' END
FOR UPDATE OF k;

-- name: DeleteProviderKeySecret :execrows
DELETE FROM user_secrets
WHERE user_id = $1 AND provider_id = $2 AND kind IN ('anthropic_compatible_key', 'openai_compatible_key');

-- name: GetUserClaudeProviderSource :one
-- The owner's Claude provider source with everything run creation needs to judge it usable in ONE
-- read (PRD #4320 M4): the setting, the provider, its CURRENT revision and the provider's key row.
-- Every join is owner-scoped, so a stored foreign id can never resolve. A NULL setting returns a
-- row whose provider columns are all NULL (no rows only for an unknown user). Plain read: the freeze
-- takes LockSecretMutationShared and then LockModelProviderForShare around its own re-read.
SELECT u.claude_provider_id AS provider_id,
       p.engine, p.current_revision, p.retired_at,
       r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.kind AS key_kind, k.provider_id AS key_provider_id,
       k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev
FROM users u
LEFT JOIN model_providers p ON p.id = u.claude_provider_id AND p.owner_user_id = u.id
LEFT JOIN model_provider_revisions r ON r.provider_id = p.id AND r.revision = p.current_revision
LEFT JOIN user_secrets k ON k.user_id = u.id AND k.provider_id = p.id AND k.kind = 'anthropic_compatible_key'
WHERE u.id = $1;

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
-- row and the key row FOR SHARE; DeleteModelProvider cascades the revision rows BEFORE the key row,
-- so a BLOCKING revision lock here against a provider delete mid-cascade is a lock cycle (40P01,
-- which the finishers do not retry). NOWAIT turns that contention into 55P03, which finishRunClaim /
-- finishChatClaim already retry as a bounded whole-transaction retry. A missing row is
-- pgx.ErrNoRows (stale).
SELECT r.provider_id, r.revision, r.protocol, r.base_url, r.auth_style, r.model_map, p.engine
FROM model_provider_revisions r
JOIN model_providers p ON p.id = r.provider_id
WHERE r.provider_id = $1 AND r.revision = $2 AND p.owner_user_id = $3
FOR KEY SHARE OF r NOWAIT;

-- name: GetModelProviderRevisionForOwner :one
-- A frozen revision of an owner's provider (inherited creation re-checks the URL against the
-- allowlist). Owner-scoped. FOR KEY SHARE OF r only protects the prune-FIRST ordering (a prune that
-- already took the row's key lock makes this read wait, then find the row gone): it is released with
-- the transaction and does not by itself keep the revision. What keeps PruneModelProviderRevisions off
-- a revision is the source run row's own reference (model_provider_revision), which stays true while
-- run rows are never deleted.
SELECT r.provider_id, r.revision, r.protocol, r.base_url, r.auth_style, r.model_map, p.engine
FROM model_provider_revisions r
JOIN model_providers p ON p.id = r.provider_id
WHERE r.provider_id = $1 AND r.revision = $2 AND p.owner_user_id = $3
FOR KEY SHARE OF r;

-- name: SetUserClaudeProviderSource :one
-- Sets or clears (NULL) the owner's Claude provider source. The CASE/narg shape of
-- SetUserHarnessModels, not COALESCE: a NULL provider_id CLEARS the setting. The composite foreign
-- key (users_claude_provider_fkey) rejects a provider owned by another user. Usability is checked by
-- the caller, which holds the owner's secret-mutation lock.
UPDATE users
SET claude_provider_id = CASE WHEN @set_source::bool THEN sqlc.narg('provider_id')::uuid ELSE claude_provider_id END
WHERE id = @id
RETURNING claude_provider_id;

-- name: GetUserProviderSource :one
SELECT u.claude_provider_id AS provider_id,
       p.engine, r.protocol, p.current_revision, p.retired_at,
       r.base_url, r.auth_style, r.model_map,
       k.id AS key_secret_id, k.kind AS key_kind, k.provider_id AS key_provider_id,
       k.disabled_at AS key_disabled_at, k.enablement_rev AS key_enablement_rev
FROM users u
LEFT JOIN model_providers p ON p.id = u.claude_provider_id AND p.owner_user_id = u.id AND p.engine = @engine
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
-- Owner and engine must match for a non-null selection; clearing is explicit.
UPDATE users u
SET claude_provider_id = CASE WHEN @engine::text = 'claude' THEN sqlc.narg('provider_id')::uuid ELSE claude_provider_id END,
    codex_provider_id = CASE WHEN @engine::text = 'codex' THEN sqlc.narg('provider_id')::uuid ELSE codex_provider_id END
WHERE u.id = @id AND @engine::text IN ('claude', 'codex')
  AND (sqlc.narg('provider_id')::uuid IS NULL OR EXISTS (
      SELECT 1 FROM model_providers p WHERE p.id = sqlc.narg('provider_id')::uuid
        AND p.owner_user_id = u.id AND p.engine = @engine::text))
RETURNING claude_provider_id, codex_provider_id;
