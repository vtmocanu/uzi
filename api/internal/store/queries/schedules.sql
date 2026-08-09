-- Scheduled runs (PRD #241). run_schedules is the durable, time-driven origin of a
-- run; these statements own its lifecycle (create/get/list/update/pause/delete), the
-- claim + advance path a due-gate poller drives, and the two read-side helpers the
-- firing code needs (sweep candidate issues, active-run guard).

-- name: CreateRunSchedule :one
-- Insert an owner-supplied schedule. Nullable columns ride sqlc.narg; server-managed
-- columns (id, status, last_fired_at, created_at, updated_at) take their defaults.
INSERT INTO run_schedules (
    user_id, repo_id, target, issue_iid, labels, prompt,
    timing, cron_expr, run_at, timezone, next_fire_at,
    auto_approve, wait_on_limit, enabled, max_issues
) VALUES (
    @user_id, @repo_id, @target, sqlc.narg('issue_iid'), sqlc.narg('labels'), sqlc.narg('prompt'),
    @timing, sqlc.narg('cron_expr'), sqlc.narg('run_at'), @timezone, sqlc.narg('next_fire_at'),
    @auto_approve, @wait_on_limit, @enabled, sqlc.narg('max_issues')
)
RETURNING *;

-- name: GetRunSchedule :one
-- Unscoped fetch by id (server-internal: the claimer/firing path already holds a
-- schedule it owns by construction).
SELECT * FROM run_schedules WHERE id = @id;

-- name: GetRunScheduleForUser :one
-- Owner-scoped fetch for the get/patch/delete handlers: a user may only see their own
-- schedules, so the WHERE carries user_id and a foreign id returns no row.
SELECT * FROM run_schedules WHERE id = @id AND user_id = @user_id;

-- name: ListRunSchedulesForUser :many
-- The owner's schedules, newest first.
SELECT * FROM run_schedules WHERE user_id = @user_id ORDER BY created_at DESC;

-- name: UpdateRunSchedule :one
-- Owner-scoped edit of the mutable fields. A foreign id matches no row and returns
-- none, so the handler cannot edit another user's schedule. next_fire_at is recomputed
-- in Go from the new timing and passed here.
--
-- A config edit also REVIVES the schedule to status='active'. Without this, a terminal
-- 'fired' once-schedule or a 'error'-parked schedule (repo was disconnected) could be
-- edited to a valid future config that is stored but never claimed — ClaimDueSchedules
-- gates on status='active' — a silently dead reschedule. Editing the config is the
-- explicit act of putting a schedule back into service, so it clears the terminal/error
-- state; enabled (the pause flag) is orthogonal and untouched here.
UPDATE run_schedules
SET target        = @target,
    issue_iid     = sqlc.narg('issue_iid'),
    labels        = sqlc.narg('labels'),
    prompt        = sqlc.narg('prompt'),
    timing        = @timing,
    cron_expr     = sqlc.narg('cron_expr'),
    run_at        = sqlc.narg('run_at'),
    timezone      = @timezone,
    next_fire_at  = sqlc.narg('next_fire_at'),
    auto_approve  = @auto_approve,
    wait_on_limit = @wait_on_limit,
    max_issues    = sqlc.narg('max_issues'),
    status        = 'active',
    updated_at    = now()
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: SetRunScheduleEnabled :one
-- Pause/resume: owner-scoped flip of the enabled flag (which the claim index keys on).
UPDATE run_schedules
SET enabled = @enabled, updated_at = now()
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: DeleteRunSchedule :execrows
-- Owner-scoped delete; execrows lets the handler tell a real delete from a foreign id.
DELETE FROM run_schedules WHERE id = @id AND user_id = @user_id;

-- name: ClaimDueSchedules :many
-- The due-gate poll. Returns every enabled, active schedule whose next_fire_at has
-- passed, locked FOR UPDATE SKIP LOCKED as defense-in-depth (Decision 1) so that IF a
-- second instance ever ran, concurrent claimers would partition the due set rather
-- than contend. The scheduler is wired single-instance and does NOT hold a surrounding
-- transaction across the fire+advance (it must not keep a tx open across the forge
-- GetIssue HTTP call), so the row locks release when this SELECT auto-commits; the real
-- backstop against a duplicate run is the per-run one-active unique index, not the lock.
SELECT * FROM run_schedules
WHERE enabled AND status = 'active'
  AND next_fire_at IS NOT NULL AND next_fire_at <= now()
ORDER BY next_fire_at
FOR UPDATE SKIP LOCKED;

-- name: AdvanceSchedule :one
-- Move a fired schedule to its next state: a recurring schedule to its next
-- next_fire_at (status stays 'active'), or a once schedule to status='fired' with
-- next_fire_at NULL so the due index no longer holds it. Kept separate from the claim
-- so the firing code decides the next fire.
UPDATE run_schedules
SET last_fired_at = @last_fired_at,
    next_fire_at  = sqlc.narg('next_fire_at'),
    status        = @status,
    updated_at    = now()
WHERE id = @id
RETURNING *;

-- name: SetRunScheduleStatus :one
-- The error path: park a schedule at status='error' (or back to 'active') without
-- touching the fire bookkeeping.
UPDATE run_schedules
SET status = @status, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: ListSweepCandidateIssues :many
-- The sweep sibling of ListAutopilotCandidateIssues (autopilot.sql): open cached
-- issues in a repo that carry ALL of the selected labels. The caller passes a jsonb
-- array of labels (an empty selector is resolved to the PRD label in Go before
-- calling), and jsonb containment (@>) matches rows whose labels array is a superset.
-- author rides along for the same adder→author attribution fallback the autopilot
-- path uses.
--
-- LIMIT sqlc.narg('max_issues') is the per-schedule sweep cap (PRD #274 M2, Decision 2):
-- sqlc renders a NULL narg as an unlimited LIMIT, so a NULL max_issues preserves today's
-- unbounded behaviour for free, and the ORDER BY forge_issue_iid ASC above makes LIMIT N
-- a deterministic oldest-first batch. The narg FUNCTION form (not @max_issues) is
-- deliberate — see .claude/rules/go.md on the runtime-comment byte-offset gotcha.
SELECT forge_issue_iid, author
FROM issues
WHERE repo_id = @repo_id AND state = 'opened'
  AND labels @> @labels::jsonb
ORDER BY forge_issue_iid ASC
LIMIT sqlc.narg('max_issues');

-- name: HasActiveRunForSchedule :one
-- Whether a non-terminal prompt run already exists for this schedule. The pre-check
-- the firing code uses to swallow a re-fire while a prior prompt run is still live,
-- alongside the uq_runs_one_active_prompt_per_schedule structural backstop.
SELECT count(*) > 0 AS active
FROM runs
WHERE schedule_id = @schedule_id
  AND kind = 'prompt'
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: CreatePromptRun :one
-- The scheduler's dedicated insert for a kind='prompt' run (PRD #241): repo-ful,
-- issue-less, always stamped with the originating schedule_id so
-- uq_runs_one_active_prompt_per_schedule dedups concurrent live runs. Modeled on
-- CreateSelfImproveRun (selfimprove.sql): a direct INSERT, not createRun, because a
-- prompt run has no forge issue and no PRD link. auto_approve and wait_on_limit come
-- straight from the schedule (the owner set them there), so unlike the engine runs
-- this path does not fall back to the owner's default.
INSERT INTO runs (
    user_id, repo_id, kind, issue_title, issue_description, schedule_id, auto_approve, wait_on_limit
) VALUES (
    @user_id, @repo_id::uuid, 'prompt', @issue_title, @issue_description, @schedule_id::uuid, @auto_approve, @wait_on_limit
)
RETURNING *;
