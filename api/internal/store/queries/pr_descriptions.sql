-- PRD #1798 D9: the staged / bound / acknowledged PR-description artifact. Every writer below
-- runs inside workersvc's fenced transaction, which first locks the run row
-- (GetRunOwnedByWorkerForUpdate) and checks the live claim_generation; the statements here add
-- the run_id / state guards that keep a version from being rebound or republished.

-- name: InsertPrDescriptionVersion :one
-- Stage one pending version for the run's snapshot. mr_iid is set only when the PR already
-- exists (a refresh); otherwise it is bound later by BindPrDescriptionVersion.
INSERT INTO pr_description_versions (
    run_id, claim_generation, repo_id, mr_iid, fields, size,
    base_sha, head_sha, target_branch, source
) VALUES (
    @run_id, @claim_generation, @repo_id, sqlc.narg('mr_iid'), @fields, sqlc.narg('size'),
    @base_sha, @head_sha, @target_branch, @source
)
RETURNING *;

-- name: GetPrDescriptionVersionForRunForUpdate :one
-- The version a bind/ack names, scoped to the calling run and row-locked for the transaction.
SELECT * FROM pr_description_versions WHERE id = @id AND run_id = @run_id FOR UPDATE;

-- name: BindPrDescriptionVersion :one
-- Bind a pending version to its PR and record the hash of the exact region text the renderer
-- will write. Only a pending version binds; an mr_iid already set must match (the service
-- checks that before calling, this guard is the backstop).
UPDATE pr_description_versions
SET mr_iid = @mr_iid::bigint, rendered_region_sha256 = @rendered_region_sha256::text
WHERE id = @id AND run_id = @run_id AND state = 'pending'
  AND (mr_iid IS NULL OR mr_iid = @mr_iid::bigint)
RETURNING *;

-- name: EnsurePrDescription :exec
-- Create the per-PR row on first bind; an existing row (an earlier run's PR) is kept as is.
INSERT INTO pr_descriptions (repo_id, mr_iid) VALUES (@repo_id, @mr_iid)
ON CONFLICT (repo_id, mr_iid) DO NOTHING;

-- name: GetPrDescription :one
SELECT * FROM pr_descriptions WHERE repo_id = @repo_id AND mr_iid = @mr_iid;

-- name: GetPrDescriptionForUpdate :one
SELECT * FROM pr_descriptions WHERE repo_id = @repo_id AND mr_iid = @mr_iid FOR UPDATE;

-- name: GetPrDescriptionVersionByID :one
SELECT * FROM pr_description_versions WHERE id = @id;

-- name: FindPrDescriptionVersionByRegionHash :one
-- The newest version of one PR in the given state whose rendered region hash equals the hash
-- the worker observed on the forge. Backs lost-ack recovery (state pending) and the
-- renderer's "markers but no published version" classification.
SELECT * FROM pr_description_versions
WHERE repo_id = @repo_id AND mr_iid = @mr_iid::bigint AND state = @state::text
  AND rendered_region_sha256 = @rendered_region_sha256::text
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: LatestBoundPrDescriptionVersionForRun :one
-- The most recent version this run bound to a PR: how a re-claimed issue run (whose runs.mr_iid
-- is never set while it is held) finds the PR it already opened.
SELECT * FROM pr_description_versions
WHERE run_id = @run_id AND mr_iid IS NOT NULL
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: MarkPrDescriptionVersionPublished :execrows
UPDATE pr_description_versions
SET state = 'published', published_at = now()
WHERE id = @id AND state = 'pending';

-- name: MarkPrDescriptionVersionAbandoned :execrows
UPDATE pr_description_versions
SET state = 'abandoned'
WHERE id = @id AND state = 'pending';

-- name: SetPrDescriptionPublished :execrows
-- Compare-and-swap publish: point the PR at the version and advance lock_version, only when the
-- caller's expected lock_version is still current.
UPDATE pr_descriptions
SET published_version_id = @published_version_id::uuid, last_outcome = 'published',
    lock_version = lock_version + 1, updated_at = now()
WHERE repo_id = @repo_id AND mr_iid = @mr_iid AND lock_version = @expected_lock_version;

-- name: SetPrDescriptionOutcome :execrows
-- Compare-and-swap for a skipped or failed write: record the outcome only. published_version_id
-- is deliberately untouched, so it always names what is actually on the forge.
UPDATE pr_descriptions
SET last_outcome = @last_outcome::text, lock_version = lock_version + 1, updated_at = now()
WHERE repo_id = @repo_id AND mr_iid = @mr_iid AND lock_version = @expected_lock_version;

-- name: RecoverPrDescriptionLostAck :execrows
-- Lost-ack recovery (D9 step 4): the forge carries a region whose hash equals a pending
-- version's, so that version is what is published. Runs under the same row lock as the ack
-- that follows it; it does not advance lock_version (the ack itself does, once).
UPDATE pr_descriptions
SET published_version_id = @published_version_id::uuid, last_outcome = 'published', updated_at = now()
WHERE repo_id = @repo_id AND mr_iid = @mr_iid;
