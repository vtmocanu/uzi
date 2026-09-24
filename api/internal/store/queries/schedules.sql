-- Scheduled runs (PRD #241). run_schedules is the durable, time-driven origin of a
-- run; these statements own its lifecycle (create/get/list/update/pause/delete), the
-- claim + advance path a due-gate poller drives, and the two read-side helpers the
-- firing code needs (sweep candidate issues, active-run guard).

-- name: CreateRunSchedule :one
-- Insert an owner-supplied schedule. Nullable columns ride sqlc.narg; server-managed
-- columns (id, status, last_fired_at, created_at, updated_at) take their defaults.
-- harness (PRD #1429 M4a, D2) is the schedule's per-run harness pin, sqlc.narg — NULL =
-- implicit (D11 resolves at fire time), 'claude'/'codex' = an explicit selection frozen onto
-- every fired run (M2 already honors a stored pin; this is the write path). The handler
-- validates the enum before insert; clone/add-repo copy the source row's column so a
-- derived row never drops the pin, mirroring credential_override below exactly.
-- credential_override_mode / credential_override_secret_id (PRD #1247 M6) are the
-- schedule's per-run credential override (D5), both sqlc.narg — NULL/NULL = inherit,
-- byte-identical to a pre-#1247 schedule. The handler validates + resolves the pair
-- through the one validator before insert; clone/add-repo copy the source row's columns
-- so a derived row never drops the override.
INSERT INTO run_schedules (
    user_id, repo_id, target, issue_iid, labels, prompt,
    timing, cron_expr, run_at, timezone, next_fire_at,
    auto_approve, wait_on_limit, mr_rework_enabled, enabled, max_issues, guidance, model, output_mode, override_subagent_model,
    sibling_group_id, harness, credential_override_mode, credential_override_secret_id
) VALUES (
    @user_id, @repo_id, @target, sqlc.narg('issue_iid'), sqlc.narg('labels'), sqlc.narg('prompt'),
    @timing, sqlc.narg('cron_expr'), sqlc.narg('run_at'), @timezone, sqlc.narg('next_fire_at'),
    @auto_approve, @wait_on_limit, sqlc.narg('mr_rework_enabled'), @enabled, sqlc.narg('max_issues'), sqlc.narg('guidance'), sqlc.narg('model'), sqlc.narg('output_mode'), @override_subagent_model,
    sqlc.narg('sibling_group_id'), sqlc.narg('harness'), sqlc.narg('credential_override_mode'), sqlc.narg('credential_override_secret_id')
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
    auto_approve, wait_on_limit, mr_rework_enabled, enabled, max_issues, model, output_mode
) VALUES (
    @user_id, @repo_id, @target, @catalog_slug, 'default', false,
    NULL, NULL, NULL, NULL,
    'recurring', @cron_expr, @timezone, @next_fire_at,
    @auto_approve, @wait_on_limit, sqlc.narg('mr_rework_enabled'), true, sqlc.narg('max_issues'), sqlc.narg('model'), sqlc.narg('output_mode')
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
-- output_mode is likewise reset to the catalog baseline (PRD #929 M1): a prompt default can
-- carry an owner-editable output mode, so the resolved catalog value is passed in and written
-- here (nil for a non-prompt default, which stores NULL = inherit).
-- credential_override_mode / credential_override_secret_id (PRD #1247 M6) are cleared to NULL
-- (inherit): a default now carries an owner-editable per-run credential override, so a Reset
-- must drop it back to the catalog baseline (inherit), same as guidance/override_subagent_model.
-- harness (PRD #1429 M4a) is likewise cleared to NULL (implicit D11 at fire time): a default
-- now carries an owner-editable harness pin, so a Reset drops it back to the catalog baseline
-- (no pin), same as credential_override_mode/secret_id above.
-- Written as SQL literals (no param) because the catalog baseline is always inherit.
-- next_fire_at is recomputed in Go from the catalog cron+timezone and passed in.
UPDATE run_schedules
SET cron_expr     = @cron_expr,
    timezone      = @timezone,
    model         = sqlc.narg('model'),
    auto_approve  = @auto_approve,
    wait_on_limit = @wait_on_limit,
    mr_rework_enabled = sqlc.narg('mr_rework_enabled'),
    max_issues    = sqlc.narg('max_issues'),
    guidance      = NULL,
    output_mode   = sqlc.narg('output_mode'),
    override_subagent_model = false,
    harness       = NULL,
    credential_override_mode = NULL,
    credential_override_secret_id = NULL,
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
    mr_rework_enabled = sqlc.narg('mr_rework_enabled'),
    max_issues    = sqlc.narg('max_issues'),
    guidance      = sqlc.narg('guidance'),
    model         = sqlc.narg('model'),
    output_mode   = sqlc.narg('output_mode'),
    override_subagent_model = @override_subagent_model,
    harness       = sqlc.narg('harness'),
    credential_override_mode = sqlc.narg('credential_override_mode'),
    credential_override_secret_id = sqlc.narg('credential_override_secret_id'),
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
--
-- Eligibility is filtered BEFORE the LIMIT (issue #1543): a label selector such as
-- ["bug"] also matches issues that are not uzi's work, and when eligibility was only
-- checked per-row afterwards a long ineligible oldest-first prefix consumed every
-- scan-window slot and starved the eligible issues behind it. The predicate mirrors
-- ListAutopilotCandidateIssues (autopilot.sql): the configured @uzi_label (string
-- membership via jsonb_exists, labels are strings) OR assignment to the uzi-bot (the
-- same numeric-containment + `@bot_id > 0` form as the assigned selector). The assigned
-- selector is eligible by construction, so it short-circuits. This supersedes the
-- issue #416 decision (prds/done/416-sweep-backfill-skipped-slots.md) to keep
-- eligibility out of SQL, which was forced by the then-needed PRD-link body check; PRD
-- #764 replaced that check with a cached-label check and PRD #767 added cached bot
-- assignment, so eligibility is now answerable from the issues cache. createRun stays
-- the authoritative per-row gate, catching a cache change between this SELECT and the
-- run create.
SELECT forge_issue_iid, author
FROM issues
WHERE repo_id = @repo_id AND state = 'opened'
  AND (
    (@selector::text = 'label' AND labels @> @labels::jsonb)
    OR (@selector::text = 'assigned' AND @bot_id::bigint > 0 AND assignee_ids @> to_jsonb(@bot_id::bigint))
  )
  AND (
    @selector::text = 'assigned'
    OR jsonb_exists(labels, @uzi_label::text)
    OR (@bot_id::bigint > 0 AND assignee_ids @> to_jsonb(@bot_id::bigint))
  )
ORDER BY forge_issue_iid ASC
LIMIT sqlc.narg('max_issues');

-- name: CountSweepCandidateIssues :one
-- The truncation probe for the sweep fire outcome's Capped flag (PRD #308 M1), over the
-- same selector as ListSweepCandidateIssues WITHOUT the max_issues LIMIT. It runs for
-- every LABEL sweep (the ineligible_matched diagnostic covers the whole backlog, cap or
-- not) and for an assigned sweep only when a cap is set; Capped is derived only when a cap
-- is set (a NULL cap can never truncate). @selector/@bot_id
-- discriminate the label vs. assigned kinds exactly as in ListSweepCandidateIssues — the
-- assigned branch uses the numeric-containment form `assignee_ids @> to_jsonb(@bot_id::bigint)`
-- (NOT jsonb_exists, which is string-only), guarded by `@bot_id > 0` (PRD #767 M4/R3).
--
-- One statement, two columns (issue #1543):
--   - eligible: the selector matches that ALSO pass the eligibility predicate
--     ListSweepCandidateIssues filters on before its LIMIT (@uzi_label OR bot
--     assignment; the assigned selector is eligible by construction). This is the
--     list's unlimited size, so fireSweep compares it against the (capped) candidate
--     set to know the cap truncated newer eligible issues.
--   - matched: every open selector match, eligible or not. `matched - eligible` is the
--     aggregate ineligible_matched diagnostic: selector matches that are not uzi's work
--     and are no longer fetched, across the whole backlog rather than one scan window.
-- The eligibility predicate must stay byte-identical to ListSweepCandidateIssues' so
-- the two agree on what "eligible" means.
SELECT
  count(*) FILTER (WHERE
    @selector::text = 'assigned'
    OR jsonb_exists(labels, @uzi_label::text)
    OR (@bot_id::bigint > 0 AND assignee_ids @> to_jsonb(@bot_id::bigint))
  ) AS eligible,
  count(*) AS matched
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
-- harness (PRD #1429 M1, was #1332 M5A / D2) is now the @harness PARAMETER supplied by the M5B
-- create seam (workersvc.createRunAtomic), not the SQL literal 'claude'. A schedule pin is an
-- explicit selection resolved at fire time; a null pin resolves from the then-current default
-- (D2). M2 wires that real value. Every current caller passes string(HarnessClaude) as a stopgap.
-- credential_override_mode / credential_override_secret_id (PRD #1247 M1) are the
-- per-run credential override (D1/D5), both sqlc.narg — NULL = inherit, byte-identical
-- to a pre-#1247 prompt run. M1 always passes NULL; M6 wires the schedule's stored
-- override onto the fired run through this seam.
INSERT INTO runs (
    user_id, repo_id, kind, issue_title, issue_description, schedule_id, auto_approve, wait_on_limit, mr_rework_enabled, model, override_subagent_model, required_capabilities, trigger_source, harness, credential_override_mode, credential_override_secret_id
) VALUES (
    @user_id, @repo_id::uuid, 'prompt', @issue_title, @issue_description, @schedule_id::uuid, @auto_approve, @wait_on_limit, sqlc.narg('mr_rework_enabled'), sqlc.narg('model'), @override_subagent_model,
    COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}'), 'schedule', @harness, sqlc.narg('credential_override_mode'), sqlc.narg('credential_override_secret_id')
)
RETURNING *;
