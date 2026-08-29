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
    auto_approve, wait_on_limit, enabled, max_issues, guidance, model, override_subagent_model,
    sibling_group_id
) VALUES (
    @user_id, @repo_id, @target, sqlc.narg('issue_iid'), sqlc.narg('labels'), sqlc.narg('prompt'),
    @timing, sqlc.narg('cron_expr'), sqlc.narg('run_at'), @timezone, sqlc.narg('next_fire_at'),
    @auto_approve, @wait_on_limit, @enabled, sqlc.narg('max_issues'), sqlc.narg('guidance'), sqlc.narg('model'), @override_subagent_model,
    sqlc.narg('sibling_group_id')
)
RETURNING *;

-- name: CreateDefaultSchedule :one
-- Enable a builtin default scheduled job (PRD #589 M2) on a repo for an owner. A
-- default-origin row stores ONLY its editable fields (cron_expr, timezone, model,
-- auto_approve, wait_on_limit, max_issues) plus target, catalog_slug and provenance;
-- its prompt (prompt target) and labels+guidance (sweep target) live in the builtin
-- catalog and are resolved in Go at fire time by catalog_slug (Decision 2), so they are
-- stored NULL here. It is always recurring, always origin='default', always customized=false
-- on first enable.
--
-- ON CONFLICT ... DO NOTHING makes enable idempotent per (user_id, repo_id, catalog_slug):
-- the partial unique index uq_run_schedules_default_per_repo backs the inference, and a
-- second enable of the same job on the same repo inserts nothing and returns no row (the
-- handler then reads the existing row via GetDefaultScheduleForRepoSlug and returns 200).
INSERT INTO run_schedules (
    user_id, repo_id, target, catalog_slug, origin, customized,
    issue_iid, labels, prompt, guidance,
    timing, cron_expr, timezone, next_fire_at,
    auto_approve, wait_on_limit, enabled, max_issues, model
) VALUES (
    @user_id, @repo_id, @target, @catalog_slug, 'default', false,
    NULL, NULL, NULL, NULL,
    'recurring', @cron_expr, @timezone, @next_fire_at,
    @auto_approve, @wait_on_limit, true, sqlc.narg('max_issues'), sqlc.narg('model')
)
ON CONFLICT (user_id, repo_id, catalog_slug) WHERE origin = 'default' DO NOTHING
RETURNING *;

-- name: GetDefaultScheduleForRepoSlug :one
-- The idempotency companion to CreateDefaultSchedule: fetch an owner's existing default
-- schedule for a (repo, catalog_slug) so a repeat enable returns the row the DO NOTHING
-- insert declined to duplicate. Owner-scoped by user_id.
SELECT * FROM run_schedules
WHERE user_id = @user_id AND repo_id = @repo_id AND catalog_slug = @catalog_slug
  AND origin = 'default';

-- name: ResetDefaultSchedule :one
-- Restore a default-origin schedule's editable fields to the catalog defaults and clear
-- the customized flag (PRD #589 M2). Owner-scoped and gated on origin='default' so a user
-- row can never be reset through this path (the handler 409s that case before calling).
-- The prompt/labels stay NULL (catalog-owned); guidance is explicitly cleared to NULL here
-- because a prompt default can carry owner-editable guidance (issue #662) and a Reset must
-- drop it back to the catalog baseline. override_subagent_model is likewise reset to the
-- catalog baseline (false) because a default now carries it as an owner-editable run option
-- (issue #691). Both are written as SQL literals rather than left to the column's DB DEFAULT:
-- a Reset is an UPDATE, so the DEFAULT never re-applies and the field must be set explicitly.
-- next_fire_at is recomputed in Go from the catalog cron+timezone and passed in.
UPDATE run_schedules
SET cron_expr     = @cron_expr,
    timezone      = @timezone,
    model         = sqlc.narg('model'),
    auto_approve  = @auto_approve,
    wait_on_limit = @wait_on_limit,
    max_issues    = sqlc.narg('max_issues'),
    guidance      = NULL,
    override_subagent_model = false,
    next_fire_at  = @next_fire_at,
    customized    = false,
    status        = 'active',
    updated_at    = now()
WHERE id = @id AND user_id = @user_id AND origin = 'default'
RETURNING *;

-- name: ListEnabledDefaultsForUser :many
-- The per-repo enablement state for the catalog-listing endpoint (PRD #589 M2): every
-- default-origin schedule the owner has, so the handler can mark which (repo_id, slug)
-- pairs are enabled and surface the backing schedule id. Owner-scoped by user_id.
SELECT id, repo_id, catalog_slug, enabled FROM run_schedules
WHERE user_id = @user_id AND origin = 'default';

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
    repo_id       = @repo_id,
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
    guidance      = sqlc.narg('guidance'),
    model         = sqlc.narg('model'),
    override_subagent_model = @override_subagent_model,
    customized    = @customized,
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

-- name: ResumeRecurringSchedule :one
-- Resume a recurring schedule (enabled-only PATCH, enabled→true): re-arm next_fire_at to
-- the next future cron occurrence AND set enabled in a SINGLE write, so no crash window
-- between two writes can leave an overdue next_fire_at behind (the exact bug of issue #396).
-- status is deliberately NOT touched: a pause/resume is status-orthogonal and must not
-- un-park a status='error' schedule (unlike UpdateRunSchedule, which revives to 'active').
UPDATE run_schedules
SET enabled = @enabled, next_fire_at = @next_fire_at, updated_at = now()
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: DeleteRunSchedule :execrows
-- Owner-scoped delete; execrows lets the handler tell a real delete from a foreign id.
DELETE FROM run_schedules WHERE id = @id AND user_id = @user_id;

-- name: CoalesceScheduleSiblingGroup :one
-- The add-repo source step (PRD #636 M1, Decision 5): ensure the owner's user-origin source
-- schedule has a sibling group id, race-safely, WITHOUT touching updated_at when the source
-- is already grouped (PRD #638 P3). The UPDATE ALWAYS fires (its WHERE does not gate on
-- sibling_group_id), which is what makes it race-safe: when two add-repo calls hit the same
-- standalone source under READ COMMITTED, the second blocks on the row lock, and after the
-- first commits its UPDATE re-fires under EvalPlanQual against the now-committed row, so
-- RETURNING yields the first caller's committed group id rather than a stale NULL. The
-- COALESCE keeps an already-set id (a source that was already grouped is returned unchanged),
-- and the CASE reads the PRE-UPDATE sibling_group_id (Postgres evaluates a SET expression's
-- column references against the old row) so updated_at is bumped ONLY on the standalone→grouped
-- transition — a repeated add-repo onto an already-grouped source no longer moves its
-- updated_at. Owner-scoped and gated on origin='user' — a foreign, absent or default-origin
-- source matches no row and the query returns NONE (the handler 404s it via pgx.ErrNoRows).
UPDATE run_schedules
SET sibling_group_id = COALESCE(sibling_group_id, @new_group),
    updated_at       = CASE WHEN sibling_group_id IS NULL THEN now() ELSE updated_at END
WHERE id = @id AND user_id = @user_id AND origin = 'user'
RETURNING sibling_group_id;

-- name: ClearSingletonSiblingGroup :execrows
-- Delete hygiene (PRD #636 M1, Decision 3): after a sibling delete drops a group to exactly
-- one live member, clear the group id off that sole survivor so it renders as a plain
-- standalone row. Owner-scoped; the subquery count gates the write to the exactly-one case,
-- so a group with ≥2 members is left untouched. Best-effort — the load-bearing guarantee is
-- the view collapse (M3), which needs no DB write (the repo-disconnect CASCADE runs no app code).
UPDATE run_schedules rs
SET sibling_group_id = NULL,
    updated_at       = now()
WHERE rs.user_id = @user_id AND rs.sibling_group_id = @group_id
  AND (SELECT count(*) FROM run_schedules s
       WHERE s.user_id = @user_id AND s.sibling_group_id = @group_id) = 1;

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
--
-- It also writes last_fire (PRD #308 M2): the serialized summary of THIS fire
-- (matched/started/skipped + typed reasons). This is the ONLY write site for last_fire —
-- the park/transient paths never advance, so a parked/transient fire keeps the prior
-- last_fire (Decision 5). last_fire is a jsonb column, so the param is []byte; passing
-- nil writes SQL NULL (the caller does this when the summary could not be serialized, so
-- a serialization hiccup never wedges the cadence).
UPDATE run_schedules
SET last_fired_at = @last_fired_at,
    next_fire_at  = sqlc.narg('next_fire_at'),
    status        = @status,
    last_fire     = @last_fire,
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
-- issues in a repo chosen by the schedule's selector kind (PRD #767 M4). @selector
-- discriminates between the two kinds:
--   - 'label': match issues carrying ALL of @labels (jsonb containment @> matches rows
--     whose labels array is a superset). The caller passes a jsonb array of labels — an
--     empty selector is resolved to the uzi label in Go before calling (PRD #764).
--   - 'assigned': match issues assigned to the uzi-bot account, by NUMERIC membership of
--     @bot_id in assignee_ids. Note the form is `assignee_ids @> to_jsonb(@bot_id::bigint)`
--     (numeric containment) and NOT jsonb_exists, which is string-only and never matches a
--     JSON number (PRD #767 R3, jsonb numeric-membership trap). The `@bot_id > 0` guard
--     mirrors the autopilot path: an unresolved/zero bot id must never match every assigned
--     issue. The assigned branch ignores @labels (the caller passes '[]' to keep the cast
--     valid).
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
  AND (
    (@selector::text = 'label' AND labels @> @labels::jsonb)
    OR (@selector::text = 'assigned' AND @bot_id::bigint > 0 AND assignee_ids @> to_jsonb(@bot_id::bigint))
  )
ORDER BY forge_issue_iid ASC
LIMIT sqlc.narg('max_issues');

-- name: CountSweepCandidateIssues :one
-- The truncation probe for the sweep fire outcome's Capped flag (PRD #308 M1): the total
-- number of open issues in the repo matching the same selector as ListSweepCandidateIssues,
-- WITHOUT the max_issues LIMIT. fireSweep compares this against the (capped) candidate set
-- it fetched to know the cap truncated newer eligible issues. It is called only when the
-- schedule carries a set cap (a NULL cap can never truncate → Capped stays false), so the
-- extra count never runs on the unbounded path. @selector/@bot_id discriminate the label
-- vs. assigned kinds exactly as in ListSweepCandidateIssues — the assigned branch uses the
-- numeric-containment form `assignee_ids @> to_jsonb(@bot_id::bigint)` (NOT jsonb_exists,
-- which is string-only), guarded by `@bot_id > 0` (PRD #767 M4/R3).
SELECT count(*)
FROM issues
WHERE repo_id = @repo_id AND state = 'opened'
  AND (
    (@selector::text = 'label' AND labels @> @labels::jsonb)
    OR (@selector::text = 'assigned' AND @bot_id::bigint > 0 AND assignee_ids @> to_jsonb(@bot_id::bigint))
  );

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
    user_id, repo_id, kind, issue_title, issue_description, schedule_id, auto_approve, wait_on_limit, model, override_subagent_model, required_capabilities
) VALUES (
    @user_id, @repo_id::uuid, 'prompt', @issue_title, @issue_description, @schedule_id::uuid, @auto_approve, @wait_on_limit, sqlc.narg('model'), @override_subagent_model,
    COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}')
)
RETURNING *;
