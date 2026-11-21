-- Issue #4512 (ADR-4007 T3a): durable per-fire progress for one-time sweeps. The admission
-- statements run inside the run-create transaction, after LockRunScheduleForUser (queries/
-- ci_watch.sql) holds the schedule row FOR UPDATE; each is a fresh READ COMMITTED statement,
-- so it sees every committed admission. Lock order (and why): workersvc/once_fire.go.

-- name: InsertOnceFireHeader :exec
-- Opens the fire's header with the passed cap (NULL = unlimited). An existing header wins:
-- the stored cap, never the passed one, bounds the fire.
INSERT INTO schedule_once_fires (schedule_id, fire_epoch, effective_cap)
VALUES (@schedule_id, @fire_epoch, sqlc.narg('effective_cap'))
ON CONFLICT DO NOTHING;

-- name: GetOnceFireCap :one
-- The stored cap of a fire; NULL (an invalid Int4) is unlimited. pgx.ErrNoRows: no header.
SELECT effective_cap FROM schedule_once_fires
WHERE schedule_id = @schedule_id AND fire_epoch = @fire_epoch;

-- name: CountOnceFireRuns :one
SELECT count(*)::bigint FROM schedule_once_fire_runs
WHERE schedule_id = @schedule_id AND fire_epoch = @fire_epoch;

-- name: OnceFireRunExists :one
SELECT EXISTS (
    SELECT 1 FROM schedule_once_fire_runs
    WHERE schedule_id = @schedule_id AND fire_epoch = @fire_epoch AND issue_iid = @issue_iid
)::boolean;

-- name: InsertOnceFireRun :exec
-- Written in the same transaction as the run it records; run_uuid is that run's id.
INSERT INTO schedule_once_fire_runs (schedule_id, fire_epoch, repo_id, issue_iid, issue_title, web_url, run_uuid)
VALUES (@schedule_id, @fire_epoch, @repo_id, @issue_iid, @issue_title, @web_url, @run_uuid);

-- name: ListOnceFireProgress :many
-- The admitted issues of one fire, in issue order. run_uuid is the immutable run identity.
SELECT issue_iid, issue_title, web_url, run_uuid
FROM schedule_once_fire_runs
WHERE schedule_id = @schedule_id AND fire_epoch = @fire_epoch
ORDER BY issue_iid;

-- name: RecordOnceFirePartial :execrows
-- The partial / held summary of a still-active once schedule's fire: last_fire only, never
-- recent_fires, guarded to the claimed occurrence so a stale fire writes nothing.
UPDATE run_schedules
SET last_fire = @last_fire, updated_at = now()
WHERE id = @id AND fire_epoch = @fire_epoch AND status = 'active' AND timing = 'once';

-- name: AdvanceOnceScheduleGuarded :execrows
-- AdvanceSchedule for a once fire, guarded to the claimed occurrence and a still-active row:
-- zero rows means the fire is stale and nothing is written.
UPDATE run_schedules
SET last_fired_at = @last_fired_at,
    next_fire_at  = sqlc.narg('next_fire_at'),
    status        = @status,
    last_fire     = @last_fire,
    recent_fires  = CASE
      WHEN @last_fire::jsonb @@ '$.matched > 0'
      THEN jsonb_path_query_array(jsonb_build_array(@last_fire::jsonb) || recent_fires, '$[0 to 9]')
      ELSE recent_fires END,
    updated_at    = now()
WHERE id = @id AND fire_epoch = @fire_epoch AND status = 'active';

-- name: AdvanceScheduleGuarded :execrows
-- AdvanceSchedule for a recurring fire, guarded to the claimed fire_epoch: a recurring row's
-- epoch never moves, so this only refuses a row converted to once mid-fire.
UPDATE run_schedules
SET last_fired_at = @last_fired_at,
    next_fire_at  = sqlc.narg('next_fire_at'),
    status        = @status,
    last_fire     = @last_fire,
    recent_fires  = CASE
      WHEN @last_fire::jsonb @@ '$.matched > 0'
      THEN jsonb_path_query_array(jsonb_build_array(@last_fire::jsonb) || recent_fires, '$[0 to 9]')
      ELSE recent_fires END,
    updated_at    = now()
WHERE id = @id AND fire_epoch = @fire_epoch;

-- name: ParkScheduleGuarded :execrows
-- Parks a schedule at status='error' (a permanent fire error), for every timing, guarded to
-- the claimed occurrence and a still-active row. Zero rows means the fire is stale.
UPDATE run_schedules
SET status = 'error', updated_at = now()
WHERE id = @id AND fire_epoch = @fire_epoch AND status = 'active';
