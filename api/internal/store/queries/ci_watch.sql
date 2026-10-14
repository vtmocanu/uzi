-- CI episode transitions use a transaction-bound Queries.WithTx. Lock the parent
-- first, then admit/read the episode in a SECOND statement (fresh READ COMMITTED
-- snapshot). All starts, halts and resets take parent then episode in this order.
-- Config edits and target conversions retain episodes; only schedule deletion cascades.

-- name: LockRepoForCIWatch :one
-- Share-locks the repo row and returns its default branch. Taken BEFORE
-- LockRunScheduleForUser in every ci_watch start, halt and reset transaction (lock order:
-- pinned secret, repo, schedule, episode, run): repo deletion locks the repo and then
-- cascades to its schedules, so taking the schedule first would cycle with it. pgx.ErrNoRows
-- means the repo is gone.
SELECT default_branch FROM repos WHERE id = @id FOR SHARE;

-- name: LockRunScheduleForUser :one
SELECT * FROM run_schedules WHERE id = @id AND user_id = @user_id FOR UPDATE;

-- name: EnsureCIWatchEpisode :one
-- Requires LockRunScheduleForUser in the same transaction. Admission has a lifetime
-- maximum of 1024 distinct keys; existing keys still return at capacity. No eviction.
-- pgx.ErrNoRows means foreign/missing schedule or unseen key at capacity.
WITH inserted AS (
    INSERT INTO ci_watch_episodes (schedule_id, workflow_key)
    SELECT s.id, @workflow_key::text FROM run_schedules s
    WHERE s.id = @schedule_id AND s.user_id = @user_id
      AND (SELECT count(*) FROM ci_watch_episodes e WHERE e.schedule_id = s.id) < 1024
    ON CONFLICT (schedule_id, workflow_key) DO NOTHING
    RETURNING *
)
SELECT * FROM inserted
UNION ALL
SELECT e.* FROM ci_watch_episodes e
JOIN run_schedules s ON s.id = e.schedule_id
WHERE e.schedule_id = @schedule_id AND s.user_id = @user_id
  AND e.workflow_key = @workflow_key::text
  AND NOT EXISTS (SELECT 1 FROM inserted);

-- name: LockCIWatchEpisode :one
SELECT e.* FROM ci_watch_episodes e
JOIN run_schedules s ON s.id = e.schedule_id
WHERE e.schedule_id = @schedule_id AND s.user_id = @user_id AND e.workflow_key = @workflow_key
FOR UPDATE OF e;

-- name: GetCIWatchEpisode :one
SELECT e.* FROM ci_watch_episodes e
JOIN run_schedules s ON s.id = e.schedule_id
WHERE e.schedule_id = @schedule_id AND s.user_id = @user_id AND e.workflow_key = @workflow_key;

-- name: UpdateCIWatchEpisode :one
-- Halt/observation write after a locked recheck by the caller.
UPDATE ci_watch_episodes e SET
    last_handled_run_id = sqlc.narg('last_handled_run_id'),
    observed_created_at = sqlc.narg('observed_created_at'),
    observed_updated_at = sqlc.narg('observed_updated_at'),
    observed_run_id = sqlc.narg('observed_run_id'),
    last_signature = sqlc.narg('last_signature'),
    attempts = @attempts,
    halted_reason = sqlc.narg('halted_reason'),
    updated_at = now()
WHERE e.schedule_id = @schedule_id AND e.workflow_key = @workflow_key
  AND EXISTS (SELECT 1 FROM run_schedules s WHERE s.id = e.schedule_id AND s.user_id = @user_id)
RETURNING e.*;

-- name: StartCIWatchEpisode :one
-- Run insert and this update share the same transaction; caller rechecks freshness,
-- dedupe, halt and attempt cap under LockCIWatchEpisode before either write.
UPDATE ci_watch_episodes e SET
    last_handled_run_id = @observed_run_id::bigint,
    observed_created_at = sqlc.narg('observed_created_at'),
    observed_updated_at = sqlc.narg('observed_updated_at'),
    observed_run_id = @observed_run_id::bigint,
    last_signature = @last_signature,
    attempts = attempts + 1,
    updated_at = now()
WHERE e.schedule_id = @schedule_id AND e.workflow_key = @workflow_key
  AND EXISTS (SELECT 1 FROM run_schedules s WHERE s.id = e.schedule_id AND s.user_id = @user_id)
RETURNING e.*;

-- name: ResetCIWatchEpisode :one
-- A fresh green observation advances the watermark and clears loop history.
UPDATE ci_watch_episodes e SET
    last_handled_run_id = @observed_run_id::bigint,
    observed_created_at = sqlc.narg('observed_created_at'),
    observed_updated_at = sqlc.narg('observed_updated_at'),
    observed_run_id = @observed_run_id::bigint,
    last_signature = NULL, attempts = 0, halted_reason = NULL, updated_at = now()
WHERE e.schedule_id = @schedule_id AND e.workflow_key = @workflow_key
  AND EXISTS (SELECT 1 FROM run_schedules s WHERE s.id = e.schedule_id AND s.user_id = @user_id)
RETURNING e.*;

-- name: ListCIWatchEpisodes :many
-- Byte-ordered exclusive keyset cursor; NULL starts the first page. Default/max 100.
-- U3 probes the next page (limit 1) before emitting the last key as next_cursor.
SELECT e.* FROM ci_watch_episodes e
JOIN run_schedules s ON s.id = e.schedule_id
WHERE e.schedule_id = @schedule_id AND s.user_id = @user_id
  AND (sqlc.narg('after_key')::text IS NULL OR e.workflow_key > sqlc.narg('after_key')::text COLLATE "C")
ORDER BY e.workflow_key
LIMIT LEAST(100, GREATEST(1, COALESCE(sqlc.narg('page_size')::integer, 100)));

-- name: HasCIWatchEpisodes :one
SELECT EXISTS (
    SELECT 1 FROM ci_watch_episodes e JOIN run_schedules s ON s.id = e.schedule_id
    WHERE e.schedule_id = @schedule_id AND s.user_id = @user_id
)::boolean;
