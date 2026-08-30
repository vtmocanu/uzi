-- Workers -----------------------------------------------------------------

-- name: CreateWorker :one
-- Issue a worker: the plaintext join token is shown once by the caller; only its
-- sha256 (token_hash) is stored. template_declared is the UI-chosen template
-- (PRD #18), NULL when the caller made no choice. anthropic_secret_id is the
-- optional mint-time token binding (PRD #104 M3); NULL means "my owner's default",
-- which is what every worker minted before this was and stays.
--
-- 🔴 anthropic_bind_mode MUST BE NAMED HERE, and its absence was a silent
-- regression. PRD #111 M3 made the MODE decide whether the id is read at all
-- (workerSecretID gates on it first), and this INSERT set five columns without it —
-- so the row took 00088's column default 'default' while carrying a real binding,
-- and every worker minted through `POST /api/workers {"anthropic_token":"..."}`
-- quietly spent the OWNER'S DEFAULT instead. PRD #104 M3's mint-time binding was
-- dead, silently, in every channel — and M1 made it worse: the run records the
-- credential actually opened, so the attribution feature CORROBORATED the wrong
-- answer.
--
-- Written in the SAME statement as the id, for the reason SetWorkerAnthropicSecret
-- gives: mode and id describe one decision, and a row where they disagree is one no
-- resolution rule can rescue. The caller derives the pair (pinned when a label
-- resolved, else default), exactly as PatchWorker does.
INSERT INTO workers (user_id, name, token_hash, template_declared, anthropic_secret_id, anthropic_bind_mode)
VALUES (@user_id, @name, @token_hash, @template_declared, @anthropic_secret_id, @anthropic_bind_mode)
RETURNING *;

-- name: SetWorkerAnthropicSecret :one
-- Point a worker at one of its owner's Anthropic credentials, or clear the binding
-- back to "use my default" (PRD #104 M3, D1). Scoped to the owner so a caller
-- holding a foreign worker id changes nothing and gets no rows — the handler turns
-- that into the same 404 it gives for an unknown worker, never a 403 that would
-- confirm the worker exists.
--
-- Nothing here validates that @anthropic_secret_id belongs to the caller: the
-- composite FK does, in the database (D11). A foreign secret id has no
-- (workers.user_id, id) pair in user_secrets and the UPDATE raises a
-- foreign_key_violation. That is the layer that still holds when the handler's own
-- ownership check is bypassed, which is exactly what M3's acceptance test asserts.
--
-- Takes effect on the worker's NEXT claim — no restart, no re-minted join token,
-- because the token never rides the worker, only each claim response.
--
-- Since PRD #111 M3 it writes the BIND MODE in the same statement, and that is the
-- point rather than a convenience: mode and id describe one decision, so writing
-- them separately would open a window where a worker reads 'pinned' with the old
-- id, or 'default' while still carrying one. One UPDATE makes the pair atomic. The
-- caller is responsible for sending a coherent pair (a NULL id with 'pinned' is
-- legal here and resolves as 'default' per D9 — see 00088 for why no CHECK can
-- enforce the coupling).
UPDATE workers
SET anthropic_secret_id = @anthropic_secret_id,
    anthropic_bind_mode = @anthropic_bind_mode,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: GetWorkerByTokenHash :one
-- Worker auth: Bearer join token → sha256 → this lookup.
SELECT * FROM workers WHERE token_hash = @token_hash;

-- name: GetWorkerByID :one
SELECT * FROM workers WHERE id = @id;

-- name: GetWorkerByIDForUser :one
SELECT * FROM workers WHERE id = @id AND user_id = @user_id;

-- name: ListWorkersByUser :many
-- Worker list for the owning user. Two derived signals (PRD #42 Decision 10):
--   * active_runs counts the worker's NON-CHAT active runs (claimed/running/
--     awaiting_approval/awaiting_input) — the RUN lane that max_concurrent_runs
--     bounds. Chat runs
--     have their own session budget (WORKER_CHAT_SESSIONS) and ClaimRun excludes
--     them, so counting a live chat here would render a false "3/2 runs" over-cap.
--   * busy is the ANY-kind non-terminal signal (a lone active chat still shows the
--     worker as busy), so it is its own EXISTS over every kind — NOT derived from
--     active_runs, which now omits chat.
-- max_concurrent_runs (the advertised cap, NULL when unadvertised) rides on w.*; it
-- is observability only and never enforced.
--
-- anthropic_secret_label is the NAME of the bound credential (PRD #104 M3), NULL
-- for an unbound worker (the overwhelming majority — unbound means "my owner's
-- default"). LEFT JOIN, so a worker whose token was deleted still lists: the FK's
-- ON DELETE SET NULL already cleared the binding, and the join simply finds
-- nothing. It carries the label only — never the ciphertext, which no worker-facing
-- query may select.
SELECT w.*,
       s.label AS anthropic_secret_label,
       EXISTS (
           SELECT 1 FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
       ) AS busy,
       (
           SELECT count(*) FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
             AND r.kind <> 'chat'
       ) AS active_runs,
       -- Roll health (PRD #113 M4), LEFT JOINed so a worker with no report — every
       -- external worker, any hosted worker the controller has not reached, and the
       -- entire fleet under docker-compose where no controller runs — still lists.
       -- The join direction is the tenancy control: worker_upgrade_reports carries no
       -- user_id, so it is reachable only THROUGH this already-per-user query.
       rh.phase              AS roll_phase,
       rh.phase_since        AS roll_phase_since,
       rh.pod_phase          AS roll_pod_phase,
       rh.blocking_container AS roll_blocking_container,
       rh.blocking_reason    AS roll_blocking_reason,
       rh.restart_count      AS roll_restart_count,
       rh.last_exit_code     AS roll_last_exit_code,
       -- observed_at is the API's own receipt time and the ONLY input to freshness.
       -- controller_reported_at is deliberately NOT selected here: it is display-only,
       -- and not handing it to the classifier is how it stays that way.
       rh.observed_at        AS roll_observed_at,
       rh.upgrading_since    AS roll_upgrading_since,
       rh.worker_image_tag   AS roll_worker_image_tag
FROM workers w
LEFT JOIN user_secrets s ON s.id = w.anthropic_secret_id AND s.user_id = w.user_id
LEFT JOIN worker_upgrade_reports rh ON rh.worker_id = w.id
WHERE w.user_id = @user_id
ORDER BY w.created_at ASC;

-- name: RegisterWorker :one
-- Worker announces version + its self-reported template and comes online;
-- heartbeat is stamped now. template_reported is what the image bakes in (PRD
-- #18), NULL when the worker sends none (older image) — stored as-is; drift vs
-- template_declared is surfaced, never rejected. max_concurrent_runs is the worker's
-- advertised slot cap (PRD #42 Decisions 3 & 10), likewise self-reported: NULL when
-- the worker advertises none (an older image, or before the M2 agent sends it), and
-- overwritten to the current report on every register (the fresh-start signal). It is
-- observability only — the server never enforces it.
--
-- It also CLEARS THE INV-5 CEILING ANCHOR AND THE ROLL-HEALTH DIAGNOSTIC BLOCK, and
-- only when the version actually MOVES (PRD #113 M4; the diagnostics half is issue
-- #145). The distinction is the invariant, not a refinement of it:
--
--   a register is evidence the POD CAME BACK.
--   a version MOVE is evidence the ROLL COMPLETED.
--
-- `upgrading_since` means "a roll is in progress", so only the second may end it.
-- Clearing on any register opens an unbounded re-arm path, and it is most available
-- exactly where a stuck worker lives: a crash-looping agent re-registers on every
-- start, so "clear on register" would let the worst-off worker in the fleet reset
-- the ceiling several times a minute, forever.
--
-- THE DIAGNOSTICS RIDE THE SAME EVENT, and for the same reason (issue #145). They
-- describe the pod of the roll that just ended, so a version move is exactly when
-- they stop being true — and a register at an UNCHANGED version must not clear them,
-- because a crash-looping agent re-registers on every start and would blank the row
-- of the one worker whose diagnostics are worth reading, on a loop.
--
-- THIS IS NOT THEIR ONLY EXIT, and an earlier version of this comment said it was.
-- The upsert has no clear ARM, but every non-terminal report WRITES those columns,
-- zeros included — a `rolling` or `stuck` report that carries nothing empties them,
-- and that is correct there, because those phases assert the controller ran the
-- lookup. What this clear uniquely provides is emptying the block WITHOUT a report,
-- which is what a completed roll needs: afterwards the diagnostics describe a pod
-- that is gone and no further report for it may ever arrive. Keep the two ends of
-- that rule in step if either moves.
--
-- It lives in THIS statement, in one round trip with the version write, so the two
-- cannot be separated by a later refactor and cannot interleave with a concurrent
-- report. The three CTEs share one snapshot, so `prev` reads the version as it was
-- BEFORE `upd` writes it — that is what makes the comparison possible at all.
-- A data-modifying CTE runs even though nothing selects from it.
--
-- Deliberately NOT in MarkHostedWorkerTokenDelivered: its guard makes a repeat
-- registration a no-op rather than a re-stamp (correct for token delivery, since a
-- pod rescheduled onto another node presents the same token again), which would
-- swallow exactly the clear we need.
WITH prev AS (
    SELECT workers.id, workers.version AS old_version FROM workers WHERE workers.id = @id
), upd AS (
    UPDATE workers SET
        status              = 'online',
        version             = @version,
        template_reported   = @template_reported,
        -- capabilities is the server-authoritative capability set (PRD #84 M1): the
        -- Filter-ed union of the worker's self-report and its template-derived caps,
        -- computed in Service.Register. Overwritten on every register (the fresh-start
        -- signal), so a worker that stops self-reporting docker loses it here too.
        -- COALESCE guards the NOT NULL column against a nil slice from any caller
        -- (pgx encodes a nil []string as SQL NULL): a nil report stores '{}', not NULL.
        capabilities        = COALESCE(@capabilities::text[], '{}'),
        max_concurrent_runs = sqlc.narg('max_concurrent_runs'),
        -- online_since is the api-owned uptime anchor (PRD #251 M1): PRESERVE it if the
        -- worker is already online with one, else STAMP now() — so a steady stream of
        -- registers never moves it and the first register after an offline gap (or for a
        -- brand-new worker) starts a fresh session. Postgres evaluates the SET RHS against
        -- the OLD row, so workers.status/online_since here read the pre-update tuple (the
        -- same mechanism SetRunRunning's `health = CASE WHEN status='running'` relies on).
        online_since        = CASE WHEN workers.status = 'online' AND workers.online_since IS NOT NULL THEN workers.online_since ELSE now() END,
        -- Clear-on-roll (PRD #422 M3, Decision 7 lifecycle): a rolled/restarted pod
        -- re-registers under the same hosted id, and clearing draining_since here is what
        -- lets a cordoned worker resume claiming after its roll (or a benign restart) —
        -- without it, a drained worker stays cordoned forever. HeartbeatWorker deliberately
        -- does NOT touch draining_since: a draining worker heartbeats and must STAY draining
        -- until it actually rolls.
        draining_since      = NULL,
        last_heartbeat_at   = now(),
        updated_at          = now()
    WHERE workers.id = @id
    RETURNING *
), cleared AS (
    UPDATE worker_upgrade_reports r
       SET upgrading_since    = NULL,
           blocking_container = NULL,
           blocking_reason    = NULL,
           restart_count      = 0,
           last_exit_code     = NULL,
           updated_at         = now()
      FROM prev
     WHERE r.worker_id = prev.id
       -- "There is something to clear", not "the anchor is set". The narrower guard
       -- was equivalent only by an accident of the upsert: a report carrying
       -- diagnostics is `rolling` or `stuck`, and a non-terminal report always stamps
       -- the anchor, so diagnostics-without-anchor could not arise. That coupling is
       -- not a property either statement states, and the moment a terminal report
       -- carries a diagnostic the narrow guard skips the clear silently. Widened so
       -- the clear does not depend on the other query's phase behaviour. The live-DB
       -- test CONSTRUCTS that row (diagnostics present, anchor forced NULL) rather
       -- than waiting for it to become reachable, so the guard is pinned by a test
       -- instead of by the coupling.
       AND (r.upgrading_since IS NOT NULL
            OR r.blocking_container IS NOT NULL
            OR r.blocking_reason IS NOT NULL
            OR r.restart_count <> 0
            OR r.last_exit_code IS NOT NULL)
       -- COMPARE THE RELEASE, NOT THE STRING. Both sides are stripped of SemVer
       -- build metadata (everything from the first '+') before being compared,
       -- because the api's classifier and this clause must agree on what "the
       -- version moved" means and on raw strings they do not:
       --
       --   0.11.7+g1a2b3c4  vs  0.11.7+gdeadbeef
       --     raw text     -> DIFFERENT  (this clause would clear the ceiling)
       --     x/mod/semver -> 0          (the classifier holds it is the same release)
       --
       -- Un-stripped, a worker satisfies "the roll completed" while nothing about its
       -- release changed, so the ceiling re-arms and MaxUpgradingWindow silently
       -- becomes "45 minutes from the most recent re-register". Compose that with a
       -- crash-looping agent — which re-registers on every start — and each restart
       -- buys a fresh window. It points the UNSAFE way: eager clearing LENGTHENS
       -- suppression, in exactly the case INV-5 is the last line of defence for.
       --
       -- The trigger is the scenario the +g<short-sha> stamp exists to expose: a
       -- re-cut tag producing two images at one release with different short shas.
       --
       -- IS DISTINCT FROM, not <>: the old version is NULL for a worker that has
       -- never reported one, and <> would evaluate NULL there and clear nothing —
       -- silently skipping the first register of exactly the worker whose version
       -- just became knowable. split_part(NULL, …) is still NULL, so stripping
       -- preserves that.
       AND split_part(@version::text, '+', 1) IS DISTINCT FROM split_part(prev.old_version, '+', 1)
)
SELECT * FROM upd;

-- name: HeartbeatWorker :one
-- Refresh liveness AND overwrite the worker's latest resource sample (PRD #49). The
-- stats_* columns are written on EVERY heartbeat — including to NULL when the tick
-- carried no stats (a downgraded/older worker or a collector error), so a stale gauge
-- self-clears rather than pinning. DISPLAY-ONLY (Decision 5): written here, read only
-- by the worker DTOs; no claim/scheduling/sweeper query references stats_ (enforced by
-- an M2 regression test). The handler has already validated + clamped these values;
-- this statement stores them verbatim.
UPDATE workers SET
    status                = 'online',
    -- online_since is the api-owned uptime anchor (PRD #251 M1): PRESERVE it if the worker
    -- is already online with one, else STAMP now() — so repeated heartbeats never move it
    -- and the first heartbeat after an offline gap starts a fresh session. Postgres
    -- evaluates the SET RHS against the OLD row, so status/online_since read the pre-update
    -- tuple (the same mechanism SetRunRunning's `health = CASE WHEN status='running'` uses).
    online_since          = CASE WHEN workers.status = 'online' AND workers.online_since IS NOT NULL THEN workers.online_since ELSE now() END,
    last_heartbeat_at     = now(),
    stats_cpu_pct         = sqlc.narg('stats_cpu_pct'),
    stats_mem_bytes       = sqlc.narg('stats_mem_bytes'),
    stats_mem_limit_bytes = sqlc.narg('stats_mem_limit_bytes'),
    stats_source          = sqlc.narg('stats_source'),
    updated_at            = now()
WHERE id = @id
RETURNING *;

-- name: DeleteWorkerForUser :execrows
DELETE FROM workers WHERE id = @id AND user_id = @user_id;

-- name: CountWorkerNonTerminalRuns :one
-- Deletion guard: a worker holding a non-terminal run may not be deleted. The FK
-- is ON DELETE SET NULL, so deleting would orphan such a run — an awaiting_approval
-- run matches no sweep once its worker_id is gone (the stale-worker sweeps key on
-- worker_id), and the one-active-run index then blocks re-running the issue.
-- Scoped by user_id so a cross-tenant delete attempt still 404s (never 409).
SELECT count(*) FROM runs
WHERE worker_id = @worker_id
  AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: CountInProgressRunsForUser :one
-- The Runs nav badge count (PRD #239): the caller's non-terminal runs, scoped to the
-- same kinds the Runs page (ListRunsForUser) shows — chat and judge excluded, so the
-- badge is a strict subset of what /runs lists.
SELECT count(*) FROM runs
WHERE user_id = @user_id
  AND kind NOT IN ('chat', 'judge')
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: MarkStaleWorkersOffline :execrows
-- Sweeper: workers past the heartbeat-stale window go offline. online_since is CLEARED
-- here (PRD #251 M1): an offline worker carries no uptime, so the next online transition
-- starts a fresh anchor rather than reporting a session that spanned the outage.
-- draining_since is DELIBERATELY NOT filtered here (PRD #422 Decision 7): draining is an
-- orthogonal column, so a draining worker whose pod actually dies is still swept offline
-- like any other — do not add a draining predicate.
UPDATE workers SET status = 'offline', online_since = NULL, updated_at = now()
WHERE status = 'online'
  AND (last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff);

-- Runs ---------------------------------------------------------------------

-- name: CreateRun :one
-- Queue a run from a card. The one-non-terminal-run-per-issue partial unique
-- index rejects a second active run for the same issue (23505 → 409).
-- origin_column snapshots the issue's column now, so a failed/cancelled run can
-- be restored to where it started; it is passed even when "" (Open), and only
-- NULL for a caller that cannot resolve it. move_pending_since is stamped in this
-- same INSERT — queued is a status the column automation reacts to (→ In
-- Progress), and the same-statement stamp closes the crash window before the
-- forge move. auto_approve is true only for autopilot-created runs (PRD #19 M4):
-- the worker reads it to resolve the plan gate without a human.
-- repo_id is nullable since PRD #39 (chat runs carry none); the ::uuid cast keeps
-- this INSERT param a non-null uuid.UUID — an issue run always targets a repo.
--
-- wait_on_limit is the PRD #35 opt-in, resolved in the SERVICE layer as
-- COALESCE(explicit request, the owner's users.wait_on_limit default) and passed in
-- explicitly, rather than defaulted in SQL. Naming the column here is what makes an
-- unstamped creation path visible in a diff of THIS file.
--
-- 🔴 IT IS NOT VISIBLE TO THE COMPILER, THOUGH, AND ASSUMING OTHERWISE IS THE TRAP.
-- sqlc generates a PARAMS STRUCT, and a Go struct literal that omits a field
-- compiles happily and yields the zero value — which for a bool is false, i.e. every
-- run from that path silently opted OUT. Measured while writing this: adding the
-- column and regenerating left `go build ./...` fully green with all three call
-- sites unstamped. So the guard here is a TEST that creates a run for an opted-in
-- owner and asserts the flag arrives, per creation path — not the type system.
--
-- 🔴 THE SAME TRAP APPLIES TO PRD #209's SEEDED-PLAN FIELDS BELOW. plan_md,
-- plan_source, agent_source and agent_exclusions are listed explicitly for exactly
-- that reason: a seeded run supplies them at create time (the human gate is the only
-- other writer), and an omitted field would silently ship a plan-less, source-'agent'
-- run — the feature inert with `go build` green. plan_source is NOT NULL DEFAULT
-- 'agent', so a caller that seeds nothing passes 'agent' and behaves byte-identically
-- to a pre-#209 run; only a real --plan-file caller passes 'seeded'. Guarded by a
-- per-creation-path test, never the compiler.
--
-- 🔴 M4's staleness-guard fields (planned_base_commit, require_base_match) are listed
-- here for the SAME reason: a seeded run created with --planned-commit/--require-base
-- supplies them at create time. require_base_match is NOT NULL DEFAULT false, so an
-- omitted param silently opts OUT of the fail-on-divergence behaviour — the exact
-- go-build-green trap the block above describes. planned_base_commit is nullable
-- (sqlc.narg): a run with no planned commit stores NULL and the worker proceeds silently.
--
-- 🔴 issue_comments (PRD #381 D7) is named here for the SAME reason: it is another
-- silently-omittable snapshot param. It is sqlc.narg (nullable jsonb) — an
-- issue-less kind, a comment-less issue, and a connection with an unknown bot id
-- (D9) all store NULL — so an omitted Go struct field would compile green and
-- silently ship NULL for every run. The fetch is centralized in createRun.
--
-- 🔴 review_comments (PRD #700 M2) is named here for the SAME reason: another
-- silently-omittable snapshot param, sqlc.narg (nullable jsonb). Issue runs never
-- carry MR comments (this createRun path always passes NULL); M3's
-- CreateAutoMRReworkRun populates it explicitly for an mr_rework run.
--
-- 🔴 required_capabilities (PRD #84 M2) is NOT a Go struct param: it is copied
-- atomically from the run's repo via a subquery, so the createRun path needs no
-- extra Go read and cannot ship a stale hint. The repo's hint is already Filter-ed
-- against the vocabulary at its write path, so no re-validation is needed here.
-- Repo-less kinds (judge/chat/self_improve) INSERT elsewhere and keep the '{}'
-- column default. Plan inference (M4) later union-merges via a separate UPDATE.
INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, origin_column, move_pending_since, auto_approve, wait_on_limit, mr_rework_enabled, plan_md, plan_source, agent_source, agent_exclusions, planned_base_commit, require_base_match, model, override_subagent_model, issue_comments, review_comments, required_capabilities)
VALUES (@user_id, @repo_id::uuid, @issue_iid, @issue_title, @issue_description, sqlc.narg('origin_column'), now(), @auto_approve, @wait_on_limit, sqlc.narg('mr_rework_enabled'), sqlc.narg('plan_md'), @plan_source, sqlc.narg('agent_source'), sqlc.narg('agent_exclusions')::jsonb, sqlc.narg('planned_base_commit'), @require_base_match, sqlc.narg('model'), @override_subagent_model, sqlc.narg('issue_comments')::jsonb, sqlc.narg('review_comments')::jsonb, COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}'))
RETURNING *;

-- name: GetRunByIDForUser :one
SELECT * FROM runs WHERE id = @id AND user_id = @user_id;

-- name: RunPriorityClass :one
-- Pure scalar eval of fn_run_priority_class (PRD #320 D8) for a single run whose
-- row is already in hand: the display class from the SAME SQL function ClaimRun's
-- ORDER BY ranks by, so pill and claim order can never disagree. No table access.
SELECT fn_run_priority_class(@run_kind::text, sqlc.narg('priority')::smallint, @is_stale::boolean);

-- name: RunPriorityClassForRun :one
-- Display priority class (PRD #320 D9) for ONE queued run by id, from the SAME SQL
-- function ClaimRun's ORDER BY ranks by, so the queued reason the owner sees and the
-- claim order never disagree. Unlike RunPriorityClass (a pure scalar eval for a row
-- already in hand), the health projection ListActiveRunsForHealth carries none of
-- kind/priority/created_at, so this reads them by id — a per-run lookup like
-- RunHasVerdictSinceGateOpened, affordable for the same reason: it runs only behind
-- healthTargetFor's queued-threshold guard, i.e. for ~zero runs per tick.
-- @background_grace_cutoff is the D4 fail-open cutoff (now() - RUN_BACKGROUND_GRACE),
-- built the SAME way service.go builds ClaimRun's cutoff: a demoted run created before
-- it reads as stale -> class `restored` (past grace) rather than `background`.
SELECT fn_run_priority_class(kind, priority, created_at < @background_grace_cutoff)
FROM runs WHERE id = @run_id;

-- name: GetRunByID :one
-- Admin viewer path: fetch any run regardless of owner. The per-run authz check
-- lives in the service, which only reaches this after confirming the viewer is an
-- admin (owners go through GetRunByIDForUser).
SELECT * FROM runs WHERE id = @id;

-- name: GetForgeTypeForRepo :one
-- The forge_type behind a repo, for the run-detail DTO's per-run MR/PR noun (PRD
-- #65 D2). GetRunByID*/GetRunForViewer return a bare runs row (SELECT *) with no
-- forge context; rather than widen those into join rows (a service-layer ripple),
-- the run-detail handler resolves this best-effort from the run's repo_id.
SELECT c.forge_type
FROM repos r
JOIN forge_connections c ON c.id = r.connection_id
WHERE r.id = @repo_id;

-- name: ListRunsForUser :many
-- The user's runs, newest first (Runs index + Agents-status "your runs"), joined
-- to the repo path and the nullable worker name for display. The optional
-- repo_id / issue_iid narrowings (PRD #12 M2) serve the board attention strip
-- (repo scope) and the in-app issue history (repo + issue); when both are NULL
-- this is the unchanged full list. The per-issue narrowing rides the composite
-- index runs (repo_id, issue_iid, created_at DESC).
-- The usage_* columns are the run's rollup totals (PRD #40 M3), LEFT-joined from
-- run_usage_totals so a run with no usage yields NULLs (rendered as absent, never a
-- fake 0). The view already applies the greatest-wins-per-model rollup (Decision 3b).
-- judge_verdict (PRD #98 M4, Decision 7) is a SAFE join: run_reviews.target_run_id is
-- NOT NULL UNIQUE (00059), so this matches at most one review per run — it cannot fan the
-- list out and, being a LEFT JOIN, cannot drop an unjudged run either. NULL means "not
-- judged", which the badge renders as absent rather than as a verdict.
--
-- The join carries its OWN owner predicate (rv.user_id = r.user_id), which is redundant
-- today and deliberately so. Without it the join would be correct only because TWO separate
-- facts both hold: this query filters r.user_id = @user_id, AND a review's user_id always
-- equals its target run's owner (PostReview binds UserID: target.UserID; CreateJudgeRun
-- preserves it). That is correctness derived from an invariant maintained in another file —
-- the same shape as the correlated rv2.user_id the ?run= semi-join was changed away from,
-- and as EXCLUDED.set_via. It costs nothing (the planner already has both columns) and it
-- means a change to how reviews are owned cannot quietly turn this join into a cross-user
-- read. The companion count query (ListJudgeTriageRowsForRuns) is scoped in its own right
-- already.
--
-- The companion judge_todo_count is deliberately NOT here. Joining through
-- review_recommendations WOULD fan out (≤50 recs per review → up to 50 duplicate rows per
-- run, breaking this query's one-row-per-run contract), and counting `todo` in SQL would
-- re-implement the ladder's bottom rung, which #94 Decision 2 categorically forbids — one
-- Go BucketOf, no SQL CASE. The handler fetches the per-rec rows for the runs on the page
-- and buckets them in Go (ListJudgeTriageRowsForRuns).
SELECT sqlc.embed(r), rp.path_with_namespace AS repo_path, w.name AS worker_name,
       c.forge_type,
       i.web_url                 AS issue_web_url,   -- PRD #411: the forge issue's web URL for the run's clickable #<iid> link
       -- PRD #320 D8: the DISPLAY priority class from the ONE SQL function, so the
       -- Runs-list pill and ClaimRun's ORDER BY are the same decision. @background_grace_cutoff
       -- (now − RUN_BACKGROUND_GRACE) is the D4 fail-open flag: a demoted run created
       -- before it reads as stale → class `restored` (past grace) rather than `background`.
       fn_run_priority_class(r.kind, r.priority, r.created_at < @background_grace_cutoff) AS priority_class,
       rv.verdict                AS judge_verdict,
       ru.input_tokens          AS usage_input_tokens,
       ru.cache_read_tokens      AS usage_cache_read_tokens,
       ru.cache_creation_tokens  AS usage_cache_creation_tokens,
       ru.output_tokens          AS usage_output_tokens,
       ru.cost_usd               AS usage_cost_usd
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id   -- forge_type for the per-run MR/PR noun (PRD #65 D2); every repo has a connection
LEFT JOIN issues i ON i.repo_id = r.repo_id AND i.forge_issue_iid = r.issue_iid   -- PRD #411: 1:1 (issues UNIQUE (repo_id, forge_issue_iid)); yields the issue web URL for the run's #<iid> link
LEFT JOIN workers w ON w.id = r.worker_id
LEFT JOIN run_reviews rv
       ON rv.target_run_id = r.id      -- UNIQUE target_run_id → at most one row (PRD #98 M4)
      AND rv.user_id = r.user_id       -- self-standing owner scope; see the note above
LEFT JOIN run_usage_totals ru ON ru.run_id = r.id
WHERE r.user_id = @user_id
  -- Exclude chat AND judge (PRD #46): both are repo-less meta-runs the general Runs
  -- list never shows. self_improve has a real repo and stays visible. The repos
  -- INNER JOIN already drops the repo-less kinds; this predicate is the explicit,
  -- refactor-proof guard (a future LEFT JOIN must not leak judge runs here).
  AND r.kind NOT IN ('chat', 'judge')
  AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id'))
  AND (sqlc.narg('issue_iid')::bigint IS NULL OR r.issue_iid = sqlc.narg('issue_iid'))
ORDER BY r.created_at DESC
-- NOTE: workersvc.runListPageCap mirrors this 200 to size the judge-badge triage fetch
-- (PRD #98 M4). A SQL literal is not importable, so the two are coupled by comment only —
-- raise this without raising that and the badge counts start truncating silently.
LIMIT 200;

-- name: ListPlanRevisionStateForRuns :many
-- The plan-ish message rows ({plan, plan_revising}) for a page of runs, so the
-- "latest by seq is plan_revising ⇒ revising" fold happens in Go (planRevisingSet),
-- mirroring web derivePlanRevision. Backed by run_messages UNIQUE (run_id, seq).
SELECT run_id, seq, kind
FROM run_messages
WHERE run_id = ANY(@run_ids::uuid[])
  AND kind IN ('plan', 'plan_revising')
ORDER BY run_id, seq;

-- name: ListActiveRunsAll :many
-- Admin Agents-status: every non-terminal run across all users, with repo path,
-- worker name, and owner email for the admin overview.
SELECT sqlc.embed(r), rp.path_with_namespace AS repo_path, w.name AS worker_name, u.email AS owner_email,
       c.forge_type,
       i.web_url                 AS issue_web_url,   -- PRD #411: the forge issue's web URL for the run's clickable #<iid> link
       -- PRD #320 D8: the DISPLAY priority class from the ONE SQL function (same as
       -- ListRunsForUser), so the admin overview pill and the claim order agree.
       -- @background_grace_cutoff (now − RUN_BACKGROUND_GRACE) is the D4 fail-open flag.
       fn_run_priority_class(r.kind, r.priority, r.created_at < @background_grace_cutoff) AS priority_class
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id   -- forge_type for the per-run MR/PR noun (PRD #65 D2)
LEFT JOIN issues i ON i.repo_id = r.repo_id AND i.forge_issue_iid = r.issue_iid   -- PRD #411: 1:1 (issues UNIQUE (repo_id, forge_issue_iid)); yields the issue web URL for the run's #<iid> link
LEFT JOIN workers w ON w.id = r.worker_id
JOIN users u ON u.id = r.user_id
WHERE r.status NOT IN ('completed', 'failed', 'cancelled')
  -- Exclude chat AND judge (PRD #46): repo-less meta-runs the admin overview omits.
  -- self_improve has a real repo and stays visible (same rationale as ListRunsForUser).
  AND r.kind NOT IN ('chat', 'judge')
ORDER BY r.created_at DESC
LIMIT 500;

-- name: ListAllWorkers :many
-- Admin Agents-status: every worker with owner email plus the same two derived
-- signals as ListWorkersByUser (PRD #42 Decision 10): busy is the ANY-kind
-- non-terminal EXISTS, active_runs is the NON-CHAT active count (chat runs are
-- excluded so a live chat never inflates "N/M runs" past the run-lane cap). The
-- embedded worker carries max_concurrent_runs (the advertised cap, NULL when
-- unadvertised).
SELECT sqlc.embed(w),
       EXISTS (
           SELECT 1 FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
       ) AS busy,
       (
           SELECT count(*) FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
             AND r.kind <> 'chat'
       ) AS active_runs,
       u.email AS owner_email
FROM workers w
JOIN users u ON u.id = w.user_id
ORDER BY w.created_at DESC;

-- name: GetRunOwnedByWorker :one
-- Worker-endpoint authz: a worker may only touch a run it currently holds.
SELECT * FROM runs WHERE id = @id AND worker_id = @worker_id;

-- name: ClaimRun :one
-- The RUN claim lane (Decision 4): atomic claim of the oldest claimable queued run
-- for the worker's user, EXCLUDING chat runs (which the chat lane claims via
-- ClaimChatRun). A re-queued run prefers its prior worker (own runs sort first, and
-- are the only claimant until the affinity grace lapses — @affinity_cutoff is now
-- minus WORKER_AFFINITY_GRACE); after that any of the user's workers may claim it.
-- FOR UPDATE SKIP LOCKED lets concurrent workers claim disjoint runs without
-- blocking. The kind<>'chat' predicate is what keeps
-- the run lane and the concurrent chat lane from stealing each other's work.
--
-- Per-(worker,run) eligibility (PRD #89 M-allow) now goes through the shared
-- fn_worker_can_claim expression (see migration 00113): a docker-enabled worker
-- (@is_docker_worker) may claim ONLY runs whose repo is on the trusted allowlist
-- (@docker_repo_allowlist), repo-less JUDGE runs are exempt (fail-closed for any
-- future repo-less kind), an empty allowlist is fail-closed, and a non-docker
-- worker short-circuits true. The full docker/judge rationale (why the gate binds
-- at claim, why judge is safe to exempt) lives in that function's comment, so it is
-- stated once and reused for BOTH the claiming worker and each candidate peer below.
--
-- Fleet-aware spread (PRD #216 D3/D4/D7/D8/R3): a busy worker DEFERS a queued run
-- to a strictly-less-loaded, live, eligible peer of the same user rather than
-- claiming it itself, so runs spread across a fleet instead of piling on whichever
-- worker polls first. Resume affinity (worker_id = me) and a run older than
-- @spread_cutoff both BYPASS the spread (D7 fail-open, so the spread can never make
-- a run unclaimable), and a minimum-loaded worker never defers (guaranteeing
-- claimability). Live = a heartbeat at/after @heartbeat_cutoff (D6). The spread
-- clause is fully described inline at the WHERE below.
UPDATE runs SET
    status     = 'claimed',
    status_since = now(),
    worker_id  = @worker_id,
    claimed_at = now(),
    updated_at = now(),
    -- Exit contract (PRD #47 Decision 3): leaving 'queued' clears any health flag
    -- the detector raised (e.g. "no worker online"). health_notified_at is NOT reset.
    health = 'ok', health_reason = NULL, health_since = NULL
WHERE id = (
    SELECT r.id FROM runs r
    WHERE r.user_id = @user_id
      AND r.kind <> 'chat'
      -- PRD #400 Decision 6: a task run is claimable ONLY after the CLI has seeded its
      -- uzi/task/<id> branch and stamped dispatched_at — otherwise a worker could claim
      -- it before the branch exists (the claim-before-seed race). Every non-task kind is
      -- unaffected (dispatched_at is only ever set on a task run).
      AND (r.kind <> 'task' OR r.dispatched_at IS NOT NULL)
      AND r.status = 'queued'
      AND (r.worker_id IS NULL
           OR r.worker_id = @worker_id
           -- Fall open the moment the run's OWN worker stops being a live, non-draining
           -- claim target: a dead (heartbeat-stale) or draining owner will never resume it
           -- (PRD #628 / ADR-628 D3a). Mirrors the fleet-spread peer-liveness test (ADR-216
           -- D6, the `last_heartbeat_at >= @heartbeat_cutoff AND draining_since IS NULL`
           -- block later in this same ClaimRun subquery); reuses that @heartbeat_cutoff param.
           OR NOT EXISTS (
               SELECT 1 FROM workers ow
               WHERE ow.id = r.worker_id
                 AND ow.last_heartbeat_at IS NOT NULL
                 AND ow.last_heartbeat_at >= @heartbeat_cutoff
                 AND ow.draining_since IS NULL)
           -- Generous ceiling bounding the live-but-can't-serve case; @affinity_cutoff is
           -- now now() - WORKER_AFFINITY_CEILING (default 2h), NOT the 2-min grace.
           OR r.updated_at < @affinity_cutoff)
      -- PRD #216 D5: claiming-worker eligibility via the shared expression.
      -- PRD #84 M2 extends it: the run's required_capabilities must be a subset of the
      -- claiming worker's effective caps (@worker_caps ∪ docker), gated by @capability_aware.
      AND fn_worker_can_claim(@is_docker_worker::boolean, @docker_repo_allowlist::uuid[], r.repo_id, r.kind, @worker_caps::text[], r.required_capabilities, @capability_aware::boolean)
      -- PRD #529 Decision 4: an ephemeral worker exists to serve exactly one run and
      -- must never take foreign work — otherwise it could hold a non-owning run when
      -- its bound run terminates, blocking the busy-guarded teardown (M4). So an
      -- ephemeral claimant (@is_ephemeral) matches ONLY its bound run
      -- (@ephemeral_run_id); a non-ephemeral worker short-circuits true and the
      -- (NULL) run id is never compared.
      AND (NOT @is_ephemeral::boolean OR r.id = sqlc.narg('ephemeral_run_id')::uuid)
      -- PRD #216 fleet-aware spread (D3/D4/D7/D8/R3). Defer this run to a peer
      -- ONLY when a strictly-better peer exists. Resume affinity (worker_id = me)
      -- and a run older than @spread_cutoff both BYPASS the spread, so the spread
      -- can never make a run unclaimable (D7 fail-open). NOT EXISTS is two-valued
      -- by construction (no COALESCE to forget). A peer qualifies as strictly
      -- better iff it is live (D6 heartbeat), eligible via the SAME expression
      -- (D5), has an advertised cap (D8: NULL cap is not a deferral target), has a
      -- free slot (D8), and is strictly less loaded by integer cross-multiplication
      -- (R3: peer.active * my.cap < my.active * peer.cap — exact, no float ties).
      -- The peer/my active counts use the SAME definition as the UI's active_runs
      -- (:93-98) so a placement is never contradicted by the displayed load. My cap
      -- and my active count are read from the same snapshot (not racy params); a
      -- NULL my.cap makes the product NULL -> row excluded -> I claim (fail-open);
      -- a 0 active count on me makes the RHS 0 -> no peer qualifies -> I always
      -- claim (a minimum-loaded worker never defers, guaranteeing claimability).
      AND (
          r.worker_id = @worker_id
          OR r.updated_at < @spread_cutoff
          OR NOT EXISTS (
              SELECT 1
              FROM workers p
              CROSS JOIN LATERAL (
                  SELECT count(*) AS active
                  FROM runs pr
                  WHERE pr.worker_id = p.id
                    AND pr.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
                    AND pr.kind <> 'chat'
              ) pa
              WHERE p.user_id = @user_id
                AND p.id <> @worker_id
                AND p.last_heartbeat_at IS NOT NULL
                AND p.last_heartbeat_at >= @heartbeat_cutoff
                -- A draining peer claims nothing (PRD #422 Decision 7), so never DEFER a
                -- run to it — it would never pick the run up.
                AND p.draining_since IS NULL
                -- PRD #529 Decision 4: an ephemeral peer claims ONLY its own bound run
                -- (ClaimRun's claimant clause above), so it would never pick up a
                -- FOREIGN run — same failure mode as the draining-peer guard. Deferring
                -- a foreign run to it would make the run unclaimable. But when r IS the
                -- ephemeral peer's own bound run, that peer is a valid deferral target,
                -- so a busy claimant correctly defers to it — hence the full predicate,
                -- not a bare AND NOT p.ephemeral.
                AND (NOT p.ephemeral OR p.ephemeral_run_id = r.id)
                AND p.max_concurrent_runs IS NOT NULL
                AND fn_worker_can_claim(COALESCE(p.docker_enabled, false), @docker_repo_allowlist::uuid[], r.repo_id, r.kind, p.capabilities, r.required_capabilities, @capability_aware::boolean)
                AND pa.active < p.max_concurrent_runs
                AND pa.active * (SELECT w.max_concurrent_runs FROM workers w WHERE w.id = @worker_id)
                    < (SELECT count(*) FROM runs mr
                        WHERE mr.worker_id = @worker_id
                          AND mr.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
                          AND mr.kind <> 'chat')
                      * p.max_concurrent_runs
          )
      )
    -- Three-level sort (PRD #320 D3): (1) resume affinity — a re-queued run
    -- prefers its prior worker, exactly as before; (2) priority rank —
    -- fn_run_priority slots BETWEEN affinity and FIFO, so an interactive run
    -- (rank 1) beats an earlier-created background judge/self_improve run
    -- (rank 0) and an expedited run (rank 2) beats both; (3) FIFO within a
    -- level. @background_grace_cutoff (now − RUN_BACKGROUND_GRACE) is the D4
    -- fail-open: a demoted run created before it reads as stale, so
    -- fn_run_priority returns normal and background work never starves.
    ORDER BY COALESCE(r.worker_id = @worker_id, false) DESC,
             fn_run_priority(r.kind, r.priority, r.created_at < @background_grace_cutoff) DESC,
             r.created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: GetRunClaimContext :one
-- The repo + connection facts the claim payload needs, alongside the run. The
-- bot PAT (token_ciphertext) is decrypted by the service, never selected in the
-- clear from the DB.
--
-- human_plan_approved is the HUMAN half of the claim payload's plan_approved
-- (PRD #35 Decision 6b); the service ORs it with the run's auto_approve. A resumed
-- run whose plan a human already approved must skip the Phase-1 planning turn and
-- the gate, replaying plan_md instead — otherwise a park-and-resume re-plans,
-- re-parks at awaiting_approval in front of a human who already approved, and can
-- fail with REASON_NO_PLAN when the resumed session declines to re-emit its plan.
--
-- ::boolean is not decoration: sqlc's inference is weaker on EXPRESSIONS than on
-- columns, and an uncast EXISTS(...) types as interface{}, which is unusable as a
-- Go bool (measured on PRD #113 M5's `IS NOT NULL` projection).
--
-- 🔴 THE INVARIANT THIS RELIES ON, WRITTEN HERE BECAUSE THIS IS WHERE IT IS READ.
-- The predicate is SetRunRunning's, whose own comment records an accepted residual:
-- a consumed round-1 approve_plan lets a stale round-2 pre-gate report through. For
-- SetRunRunning that residual hides a gate. Here it would tell the worker to skip
-- Phase 1 and IMPLEMENT AN UNREVIEWED plan_md — the same residual with a materially
-- worse blast radius, which is why it is spelled out rather than inherited.
--
-- It is sound today, and the reason is structural rather than lucky, so it is a
-- property of the QUERY PAIR and not of the worker's loop:
--   * a park is running-only (SetRunLimitWait's positive source guard), and
--   * a revise round sits at awaiting_approval, which SetRunRunning refuses to
--     leave for 'running' unless a consumed approve_plan exists.
-- So the ordinary multi-round revise flow cannot reach a park at all. The one
-- surviving residual is the stale round-2 pre-gate report SetRunRunning's comment
-- already names: if that admits a run to 'running' and it then parks, the resume
-- skips the gate on an unreviewed plan_md. Required invariant, stated so a future
-- change can be checked against it: NO awaiting_approval REPORT REWRITES plan_md
-- AFTER THE CONSUMED approve_plan THAT MADE human_plan_approved TRUE. A tighter
-- derivation is not cheaply available — runs carries no plan_md_set_at to compare
-- consumed_at against, and inventing one is out of this PRD's scope.
--
-- 🔴 PRD #209 adds a FOURTH source of plan_approved beyond the two named above
-- (auto_approve, human_plan_approved): a run born plan_source='seeded' (service.go's
-- third disjunct). The invariant just stated is VACUOUS for such a run — it has no
-- consumed approve_plan, so "no report rewrites plan_md AFTER the approval" cannot be
-- violated, which is exactly why it cannot hold. The seeded run's soundness comes
-- from a different guard: the moment SetRunAwaitingApproval rewrites plan_md with a
-- worker-authored plan it ALSO sets plan_source='agent', so the disjunct that made
-- plan_approved true stops firing at the same instant plan_md stops being the seeded
-- (or create-time-validated-empty-rejected) text. plan_source therefore tracks
-- plan_md's provenance, and the seeded run's plan_approved is sound for the same
-- structural reason the other two are: it is true only while plan_md is the reviewed
-- (here: create-time-supplied) text.
SELECT rp.web_url             AS repo_web_url,
       rp.path_with_namespace AS repo_path,
       rp.forge_project_id,
       rp.default_branch,
       rp.repo_skills_enabled,
       rp.repo_claudemd_enabled,
       rp.repo_devbox_opt_in,
       rp.fold_improve_uzi_backlog,
       -- #66 M8 (D8): the admin per-repo guardrail override discriminator for the
       -- claim backstop (M6, layer 3). A non-NULL reason means Overridden=true, so
       -- the shared evaluator downgrades the waivable "bot is too strong" findings —
       -- never protection_unreadable, which still refuses even an overridden repo.
       rp.guardrail_override_reason,
       c.forge_type,
       c.base_url,
       c.bot_username,
       c.token_ciphertext,
       (EXISTS (SELECT 1 FROM run_user_inputs i
                WHERE i.run_id = r.id
                  AND i.kind = 'approve_plan'
                  AND i.consumed_at IS NOT NULL))::boolean AS human_plan_approved
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id
WHERE r.id = @run_id;

-- name: SetRunAnthropicSecret :execrows
-- Record WHICH Anthropic credential this claim spends (PRD #111 M1). Written by
-- every claim lane — run, judge and chat — after a SUCCESSFUL open, so the
-- recorded id is provably the id whose ciphertext was decrypted (D8) rather than
-- whatever the user's default happened to be a moment later.
--
-- The label is a SNAPSHOT, not a denormalisation to keep in sync: 00086's FK nulls
-- the id when the token is deleted, and a rename rewrites the label in place, so
-- the snapshot is the only thing that keeps a finished run's history readable
-- after either. It is written from the SAME owner-scoped row that produced the id
-- (GetDefaultUserSecretMeta / GetUserSecretMetaByID), never looked up separately.
--
-- Owner-scoped even though the caller only ever passes the run it just claimed.
-- That is the same self-standing-scope argument ListRunsForUser's rv.user_id join
-- predicate makes: without it this write is safe only because of a fact maintained
-- in another file (ClaimRun/ClaimChatRun are user-scoped), and it costs nothing to
-- make it true here instead. A mismatched pair returns 0 rows rather than writing.
-- The FK is the other half — recording a credential the run's owner does not own
-- is rejected by the database, not by this predicate.
--
-- reason is the mode that named the credential; headroom is NULL until M4 has one
-- to record (see 00086). updated_at follows house style and is safe here
-- specifically because the run is 'claimed' at this instant, which holds for BOTH
-- lanes that call this write, not just the run lane:
--   * ClaimRun's affinity predicate reads r.updated_at only for status = 'queued'
--     rows (:406);
--   * ClaimChatRun has its OWN affinity predicate over r.updated_at (chat.sql:72),
--     likewise narrowed to status = 'queued';
--   * ListActiveRunsForHealth deliberately excludes 'claimed' entirely.
-- So no reader of runs.updated_at applies to a claimed run on either lane. Naming
-- one of the two readers, as this comment first did, would have left a reader of
-- the same column unaccounted for while reading as though the set were complete.
-- limit_dead_secret_id is cleared here, unconditionally (PRD #217 M2, D2). It is set
-- at park by SetRunLimitWait to the credential the run just parked on; this — the
-- first claim that successfully RECORDS a credential — is where the one-claim-lived
-- exclusion ends. Clearing on RECORD (rather than on claim start) is what makes it
-- survive a claim that dies before recording, so the retry still excludes the dead one.
UPDATE runs
SET anthropic_secret_id     = @anthropic_secret_id,
    anthropic_secret_label  = @anthropic_secret_label,
    anthropic_select_reason = @anthropic_select_reason,
    anthropic_headroom_pct  = sqlc.narg('anthropic_headroom_pct'),
    limit_dead_secret_id    = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetRunRunning :execrows
-- claimed/awaiting_approval → running, AND running → running: the worker reports
-- this state more than once per run (once on claim, again after checkout with the
-- repo agent roster, again on every session-id/iteration heartbeat), so the
-- statement is idempotent by construction. started_at is stamped once;
-- iteration_count only advances (GREATEST) so a resume never regresses the loop
-- counter. A terminal run (e.g. cancelled) is left untouched → 0 rows → "already
-- terminal".
--
-- The three PRD #37 columns are COALESCE'd against their own value, so a report
-- that omits them (the common case) preserves what a previous report wrote and no
-- caller has to read-modify-write. repo_agents is set once, by the post-checkout
-- report; agent_source/agent_exclusions only by an AUTOPILOT run's report (a
-- human-gated run persists its selection through CreateApprovePlanInput instead).
-- Consequence, deliberate: a worker can never NULL these back out — an empty
-- roster is reported as '[]', which is a distinct, meaningful value.
--
-- awaiting_approval → running is guarded (PRD #44 F2): a `running` report may be
-- retry-delayed up to ~31s, and two pre-gate fire-and-forget `running` reports
-- exist (the post-checkout roster report and the onSessionId report). One of those
-- landing AFTER the awaited awaiting_approval report would silently flip the run
-- back to running, hiding the plan gate with no self-heal (the run then dies on
-- plan-approval timeout). The EXISTS clause lets the transition through ONLY once a
-- consumed approve_plan input exists — i.e. the legitimate post-approval resume
-- report, which by construction is sent after the worker consumed the verdict. A
-- stale pre-gate report (no consumed approve_plan yet) leaves the gate intact.
-- claimed→running and running→running are unaffected (the guard only narrows the
-- awaiting_approval source status); autopilot never enters awaiting_approval.
-- Accepted residual (out of scope, see specs/ai.md): in a multi-round re-gate a
-- consumed round-1 input lets a stale round-2 pre-gate report through.
UPDATE runs SET
    status           = 'running',
    -- Stamped only on ENTRY to running. This statement is ALSO the running→running
    -- heartbeat (claim, post-checkout roster report, and every session-id/iteration
    -- report all send `running`), so an unconditional now() would move the episode clock
    -- on every heartbeat and contradict status_since's contract ("when this run entered
    -- its current status", migration 00163). The CASE keys off the PRE-update status —
    -- Postgres evaluates each SET right-hand side against the OLD row, the same old-row
    -- evaluation the `health` arm below relies on.
    status_since     = CASE WHEN status = 'running' THEN status_since ELSE now() END,
    -- PRD #88: the run is leaving the clarification park, so the question it was
    -- parked on is RESOLVED and its id must not survive.
    --
    -- 🔴 THIS IS ONE OF **TWO** CLEARS. The invariant they jointly carry:
    -- NO SETTER MAY LEAVE A RESOLVED open_question_id BEHIND. The sibling is
    -- the matching clear in SetRunAwaitingApproval, which covers the
    -- M4 PRE-RUN path — a run that parks before it plans reaches the gate with no
    -- intervening `running` report, so this statement is never executed on it (D-AG).
    -- Deleting either clear as redundant re-opens the defect on the path the other
    -- one does not cover, and a third non-terminal destination added later needs its
    -- own. The full argument lives at SetRunAwaitingApproval; this pointer exists so
    -- the invariant is discoverable from EITHER end, which is the only version that
    -- works — a reader who opens only this statement would otherwise learn it is the
    -- sole clear.
    --
    -- A stale id is not inert: the claim payload re-delivers open_question_id on a
    -- resume, so a worker that restarts after an answered park would seed its map
    -- with the OLD id and then re-use it for a genuinely new question. That
    -- degenerates the guard below to "has ever been answered" (PRD #44 F2 again) and
    -- lets the stale answer to the old question satisfy the new one — the two things
    -- identity keying exists to prevent.
    --
    -- Safe to assign here even though the guard below reads the same column: Postgres
    -- evaluates the WHERE and every SET right-hand side against the OLD row, which is
    -- the same mechanism the health CASE arms below already rely on.
    open_question_id = NULL,
    started_at       = COALESCE(started_at, now()),
    iteration_count  = GREATEST(iteration_count, @iteration_count),
    session_id       = COALESCE(sqlc.narg('session_id'), session_id),
    repo_agents      = COALESCE(sqlc.narg('repo_agents')::jsonb, repo_agents),
    agent_source     = COALESCE(sqlc.narg('agent_source'), agent_source),
    agent_exclusions = COALESCE(sqlc.narg('agent_exclusions')::jsonb, agent_exclusions),
    -- PRD #84 M4: persist the plan-time INFERRED requirement set an AUTOPILOT run emits
    -- on this self-contained `running` report. An autopilot run auto-approves its own
    -- plan and never reports awaiting_approval, so SetRunAwaitingApproval's identical
    -- clauses are never reached on it — without these three the inference is silently
    -- lost for every auto-approved run (the sweep uses auto-approve). ALL THREE are
    -- ABSENT-SAFE so the ordinary session-id/iteration heartbeats (which omit them) never
    -- disturb the columns, mirroring SetRunAwaitingApproval byte-for-byte:
    --
    -- required_capabilities is UNION-MERGED (escalation-only): the M2 enqueue seam already
    -- copied the repo's static hint, and inference can only ADD. The COALESCE is
    -- LOAD-BEARING — a nil text[] param encodes SQL NULL and `arr || NULL = NULL` would
    -- WIPE the NOT-NULL column — so an absent param unions with '{}' (no change) and a
    -- present set adds its members, deduped; `<@` is order-independent so it stays unsorted.
    required_capabilities = ARRAY(SELECT DISTINCT unnest(
        required_capabilities || COALESCE(sqlc.narg('inferred_capabilities')::text[], '{}'))),
    -- required_tools is SET, absent-safe: a present set REPLACES (the run's single
    -- authoritative inferred toolchain list), an absent (NULL) param COALESCEs back to the
    -- existing column. The service only passes a non-empty filtered set, so a garbled/empty
    -- report leaves the param nil rather than wiping the column.
    required_tools = COALESCE(sqlc.narg('inferred_tools')::text[], required_tools),
    -- size_class is SET, absent-safe like required_tools: a present (clamped s/m/l) value
    -- REPLACES, an absent (NULL) param COALESCEs back. The service clamps to {s,m,l} before
    -- passing, so a garbled report becomes a nil param (no change) rather than a bad value.
    size_class = COALESCE(sqlc.narg('size_class'), size_class),
    -- PRD #122 M1: the FROZEN milestone list an AUTOPILOT run resolved for itself,
    -- with a SAFETY-NET fallback to milestones_candidate (issue #259). Written
    -- IMMUTABLY — COALESCE keeps the EXISTING value, so a later `running` report can
    -- never overwrite a frozen list. Three sources, in priority order:
    --   1. milestones_frozen — an already-frozen list is never disturbed (immutability).
    --   2. narg('milestones_frozen') — the AUTOPILOT path: the worker sends its resolved
    --      list on the (self-contained) running report, since an autopilot run never
    --      reports awaiting_approval and so has no candidate column set.
    --   3. milestones_candidate — the HUMAN-GATED safety net. CreateApprovePlanInput is
    --      the primary freeze for that path (candidate→frozen at approve), but issue #259
    --      observed that freeze reading a NOT-YET-VISIBLE candidate and freezing NULL,
    --      leaving an approved milestone run with candidate set and frozen NULL — so the
    --      progress UI never lit up. This clause makes the FIRST post-approval running
    --      report re-freeze from the candidate column, closing that gap idempotently. On
    --      the normal path it never freezes a not-yet-approved list: during planning the
    --      candidate column is still NULL, and the WHERE guard below admits
    --      awaiting_approval → running only once an approve_plan input was consumed. In the
    --      one residual that guard DOES admit — a stale round-2 pre-gate report riding a
    --      consumed round-1 approve_plan (see the accepted-residual note on this query's
    --      guard) — it is clause 1, NOT the guard, that keeps the freeze correct: the
    --      round-1 resume already froze round-1's candidate, so the already-frozen list
    --      wins and the stale round-2 candidate cannot overwrite it. The common heartbeat
    --      is likewise a no-op via clause 1.
    milestones_frozen = COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate),
    -- PRD #122 M2 (Decision 5/5b): per-run budget derived SERVER-SIDE from the frozen
    -- milestone count at freeze, written IMMUTABLY. NULL for a 0/1-milestone run so its
    -- budget is byte-for-byte the global default. Count capped at milestone_budget_cap,
    -- wall capped at budget_wall_ceiling_seconds. Frozen source = same COALESCE as above.
    budget_max_iterations = COALESCE(budget_max_iterations,
        CASE WHEN COALESCE(jsonb_array_length(COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate)), 0) <= 1 THEN NULL
             ELSE sqlc.arg('run_max_iterations')::int * LEAST(jsonb_array_length(COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate)), sqlc.arg('milestone_budget_cap')::int) END),
    budget_wall_seconds = COALESCE(budget_wall_seconds,
        CASE WHEN COALESCE(jsonb_array_length(COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate)), 0) <= 1 THEN NULL
             ELSE LEAST(sqlc.arg('run_timeout_seconds')::int * LEAST(jsonb_array_length(COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate)), sqlc.arg('milestone_budget_cap')::int), sqlc.arg('budget_wall_ceiling_seconds')::int) END),
    -- PRD #122 M2 (Decision 3): completed is UNIONED (monotone, dedup); in_progress is
    -- OVERWRITTEN wholesale. NULL param = "not reported this call" → column untouched.
    -- Ids are validated + membership-checked server-side (progressParams) before here.
    -- PRD #265 M1: SetRunCompleted copies this exact milestones_completed union onto the
    -- completion path (signal_done reconciliation). The two sites MUST keep identical
    -- dedup semantics — if you change one, change both.
    milestones_completed = CASE
        WHEN sqlc.narg('milestones_completed')::jsonb IS NULL THEN milestones_completed
        ELSE COALESCE((SELECT jsonb_agg(DISTINCT e)
                       FROM jsonb_array_elements_text(COALESCE(milestones_completed, '[]'::jsonb) || sqlc.narg('milestones_completed')::jsonb) AS e), '[]'::jsonb)
    END,
    milestones_in_progress = CASE
        WHEN sqlc.narg('milestones_in_progress')::jsonb IS NULL THEN milestones_in_progress
        ELSE sqlc.narg('milestones_in_progress')::jsonb
    END,
    -- Exit contract (PRD #47 Decision 3), guarded so it fires only on ENTRY to
    -- running. This statement is also the running→running heartbeat (idempotent),
    -- and an unconditional reset would clear the detector's flag on every heartbeat;
    -- the CASE keys off the pre-update status (Postgres evaluates SET RHS against
    -- the old row), so a claimed/awaiting_approval → running transition resets the
    -- flag while a running → running heartbeat preserves whatever the detector wrote.
    health        = CASE WHEN status = 'running' THEN health        ELSE 'ok'  END,
    health_reason = CASE WHEN status = 'running' THEN health_reason ELSE NULL END,
    health_since  = CASE WHEN status = 'running' THEN health_since  ELSE NULL END,
    -- Issue #783: bank the wall-clock time this run spent parked at a HUMAN GATE so
    -- SweepRunningTimeout's deadline can exclude it (started_at is left untouched, so
    -- run-duration display and the health baselines are unchanged). Keyed on the OLD
    -- row (Postgres evaluates SET RHS against the pre-update tuple, the same mechanism
    -- the status_since/health arms above use): fires only on ENTRY from a park. On the
    -- running->running heartbeat the old status is 'running', so the CASE is 0 and this
    -- never double-counts; status_since is NOT NULL (migration 00163), so the subtraction
    -- is never NULL. Both park->running transitions are guarded below (consumed
    -- approve_plan / question identity), so accumulation only happens on a real resume.
    budget_paused_seconds = budget_paused_seconds
        + CASE WHEN status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int)
               ELSE 0 END,
    updated_at       = now()
WHERE runs.id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled')
  -- limit_wait is excluded EXPLICITLY, and note that the negative guard above does
  -- NOT cover it (PRD #35): a parked run is inside 'NOT IN (terminal)', so without
  -- this clause a `running` heartbeat delivered AFTER the park report — the batcher
  -- retries, and two pre-gate fire-and-forget reports already exist, so reordering
  -- is not hypothetical — would flip limit_wait back to running under a worker whose
  -- execution has already ended. The run would then sit `running` until RUN_TIMEOUT
  -- failed it, with the park silently lost. The inversion is worth naming: the same
  -- negative-guard shape that makes CancelRunServerSide cover a new status for free
  -- is what makes THIS statement dangerous.
  -- Resume is unaffected: a promoted run is 'claimed' when the worker reports running.
  AND status <> 'limit_wait'
  -- pool_wait is the SAME shape of park (PRD #754 M5): now that it is resumable, a
  -- reordered pre-hold `running` report must not un-hold a run the same way it must not
  -- un-park a limit_wait one. The negative predicate above admits it, so it is excluded
  -- explicitly here alongside limit_wait — a held run resumes only via the server-side
  -- promote (reactive or resume-now), which lands it at 'queued' before the worker reports.
  AND status <> 'pool_wait'
  AND (status <> 'awaiting_approval' OR EXISTS (
        SELECT 1 FROM run_user_inputs
        WHERE run_user_inputs.run_id = @id
          AND run_user_inputs.kind = 'approve_plan'
          AND run_user_inputs.consumed_at IS NOT NULL))
  -- awaiting_input → running is guarded the same way and for the same reason
  -- (PRD #88 M1), as a SECOND, INDEPENDENT clause. Never merge the two into
  -- `status NOT IN (...) OR kind IN (...)`: that would let a consumed `answer`
  -- satisfy the PLAN gate and a consumed `approve_plan` satisfy the QUESTION gate,
  -- re-opening #44 F2 sideways.
  --
  -- Unlike the plan clause above, this one is keyed on the question's IDENTITY, not
  -- merely on "an input of this kind was consumed". The plan gate's accepted
  -- multi-round residual does not transfer: re-gating is the rare path for a plan,
  -- while a run may ask QUESTION_MAX (default 5) questions, so a has-ever-been-
  -- answered predicate would protect question 1 and leave 2..N with nothing — a
  -- retry-delayed pre-park `running` report would silently un-park every later
  -- question, which then dies on the deadline having never been surfaced.
  --
  -- runs.open_question_id reads the OLD row here (Postgres evaluates the WHERE, and
  -- the SET right-hand side, against the pre-update tuple) — the same mechanism the
  -- health CASE arms above already rely on. A NULL open_question_id makes the
  -- equality NULL, so the guard blocks: fail-closed.
  AND (status <> 'awaiting_input' OR EXISTS (
        SELECT 1 FROM run_user_inputs
        WHERE run_user_inputs.run_id = @id
          AND run_user_inputs.kind = 'answer'
          AND run_user_inputs.consumed_at IS NOT NULL
          AND run_user_inputs.question_id = runs.open_question_id))
  -- awaiting_followup → running is guarded the SAME way and for the same reason as
  -- awaiting_input above (PRD #517 Decision 7), as a THIRD, INDEPENDENT clause. The
  -- interactive-task park (Decision 3) holds the run in-process at `awaiting_followup`
  -- after signal_done; the worker resumes ONLY when a `follow_up` steering input has
  -- been consumed (`uzi run follow-up`, Decision 4 waiter). Requiring a CONSUMED
  -- follow_up is what ties the wake to the in-process worker on the current claim:
  -- the outer `worker_id = @worker_id` already pins the worker, and this clause pins the
  -- CAUSE — a delayed or duplicate PRE-PARK `running` report (the batcher retries, and
  -- the pre-gate fire-and-forget reports already exist, so reordering is not
  -- hypothetical) carries no consumed follow_up, so it cannot un-park an idle task and
  -- re-arm the wall clock. Kept SEPARATE from the two clauses above, never merged into
  -- a single `status NOT IN (...) OR kind IN (...)`: that would let a consumed `answer`
  -- satisfy the FOLLOWUP gate (and vice-versa), re-opening #44 F2 sideways.
  --
  -- Like awaiting_input this clause IS now keyed on a per-park identity (issue #552 M1):
  -- runs.open_followup_id, a WATERMARK of the highest follow_up the run had already
  -- consumed at the moment it parked. The tie is therefore "a follow_up NEWER than the
  -- watermark was consumed" — i.e. THIS park's follow_up — not "any follow_up was ever
  -- consumed". Without it, on a run that has already iterated (cycle ≥2, an earlier
  -- follow_up consumed) the bare EXISTS always found a consumed follow_up and degraded
  -- to a no-op, so a stale pre-park `running` report un-parked an idle run: a real
  -- awaiting_followup→running STATE CHANGE (the health CASE arms fire their ELSE branch),
  -- re-arming the wall clock on a task sitting in the follow-up waiter.
  --
  -- runs.open_followup_id reads the OLD (pre-update) row here — Postgres evaluates the
  -- WHERE, and every SET right-hand side, against the pre-update tuple — exactly as
  -- runs.open_question_id does in the awaiting_input guard above. The setter's
  -- COALESCE(MAX(id),0) floor stamps 0 (never NULL) on every park, so open_followup_id is
  -- genuinely NULL only for a run that has never parked at all; that NULL COALESCEs to 0
  -- here, so any consumed follow_up clears it: fail-open only in the one case where any
  -- consumed follow_up genuinely IS new.
  --
  -- No clear-on-wake is needed, and a future reader must NOT "add the missing sibling
  -- clear" the way open_question_id needs one. As of issue #559 the watermark is
  -- WORKER-PROVIDED at each park (SetRunAwaitingFollowup) — the max follow_up id the
  -- worker has already DELIVERED — CLAMPED there to the server's max already-consumed
  -- follow_up, with a server-derived fallback to that same max-consumed when the worker
  -- omits it (old worker / first park). Either way it keys on CONSUMED-only rows for its
  -- ceiling: the follow_up that wakes a park is unconsumed until it wakes, so it never
  -- counts toward the watermark that guards its own park, and the next park rolls the
  -- watermark forward to include it. There is nothing to reset between parks. The guard
  -- predicate below (`id > COALESCE(open_followup_id, 0)`) is UNCHANGED by #559.
  AND (status <> 'awaiting_followup' OR EXISTS (
        SELECT 1 FROM run_user_inputs
        WHERE run_user_inputs.run_id = @id
          AND run_user_inputs.kind = 'follow_up'
          AND run_user_inputs.consumed_at IS NOT NULL
          AND run_user_inputs.id > COALESCE(runs.open_followup_id, 0)));

-- name: SetRunAwaitingApproval :execrows
UPDATE runs SET
    status     = 'awaiting_approval',
    status_since = now(),
    plan_md    = @plan_md,
    -- 🔴 PRD #209 D8 SAFETY FIX, carried in the SAME UPDATE that rewrites plan_md.
    -- plan_source describes the row's BIRTH; plan_md is MUTABLE, and this statement is
    -- the mutation. DEFENSE IN DEPTH: if a seeded run ever falls through to the plan
    -- gate — the create-time empty/whitespace rejection plus the worker's own
    -- non-empty-plan guard make it unreachable as this lands (the PRD's original
    -- "scrub reduces the plan to whitespace" trigger cannot fire: secretscrub only
    -- ADDS the "[redacted]" marker, it never empties a non-whitespace plan), but the
    -- guard is one column and protects against any future worker path that does reach
    -- Phase 1 — this statement overwrites its seeded plan_md with the worker's OWN
    -- Phase-1 plan. If plan_source stayed
    -- 'seeded', the plan_approved third disjunct (service.go) would then ship
    -- plan_approved=true over that unreviewed plan_md on the next claim — and via
    -- RequeueRunsOfStaleWorkers (a direct UPDATE that never runs SetRunRunning's
    -- consumed-approve_plan guard) the run re-enters implement with a plan no human
    -- saw and no gate. Setting plan_source = 'agent' here makes the disjunct track
    -- plan_md's PROVENANCE rather than the row's birth: once this worker authored the
    -- plan_md, the run is an ordinary agent-planned run and re-gates like one. This is
    -- the fix the create-time 422-on-empty (service.go) is the OTHER half of — the 422
    -- closes the blank-plan ENTRY path, this closes every other fall-through. Both.
    plan_source = 'agent',
    -- 🔴 PRD #71 M5 SAFETY FIX, symmetric with the plan_source='agent' clear above.
    -- Parking means the run is now awaiting a HUMAN review of plan_md, so the
    -- run.AutoApprove disjunct in the plan_approved derivation (service.go ~1708) must
    -- STOP firing — exactly as plan_source='agent' stops the seeded disjunct for the
    -- same reason. Without it, a PRD #71 auto ci_fix run that PARKS here for CI-config
    -- approval (the M5 forceGate) and is then re-queued by a worker restart (Register
    -- orphan-recovery, service.go) resumes with plan_approved=true — its still-true
    -- auto_approve short-circuits the executor's preApproved path, skipping the gate
    -- with NO human in the loop, and the worker's ciFixHumanApproved initializer then
    -- reads that as approved. Clearing auto_approve here makes the resume re-gate.
    -- Manual runs already carry auto_approve=false (no-op), and a normal autopilot run
    -- never reaches this statement (it short-circuits in gatePlan and never parks), so
    -- the ONLY run this newly affects is the forceGate ci_fix case — the intent.
    auto_approve = false,
    -- PRD #122 M1: the CANDIDATE milestone list this pre-approval report carries.
    -- DIRECT assignment, not COALESCE — the candidate is REPLACED each revision round
    -- (Decision 2), so a fresh awaiting_approval report overwrites the prior proposal.
    -- A report with no milestones passes NULL and clears the candidate, which is
    -- correct: the candidate reflects only the latest proposal. The immutable
    -- frozen list is untouched here (it is written at approve / by autopilot).
    milestones_candidate = sqlc.narg('milestones_candidate')::jsonb,
    -- PRD #84 M4 (unit 4b): persist the plan-time INFERRED requirement set the worker
    -- emits on this report. ALL THREE assignments are ABSENT-SAFE (a nil param must not
    -- disturb the column). ESCALATION-ONLY applies to required_capabilities ALONE:
    -- inference can ADD but never DROP what the M2 repo hint already established, via the
    -- union-merge below. required_tools and size_class are absent-safe SET/REPLACE — a
    -- present value REPLACES the column outright, an absent (nil) param COALESCEs to a
    -- no-op.
    --
    -- required_capabilities is UNION-MERGED, not replaced: the M2 enqueue seam already
    -- copied the repo's static hint onto this run, and the plan-time inference can only
    -- ADD to it (Decision: escalation-only). The COALESCE is LOAD-BEARING — a nil text[]
    -- param encodes SQL NULL, and `arr || NULL = NULL` would WIPE the NOT-NULL column
    -- (the exact pgx trap the register path guards the same way, runtime.sql ~185). So an
    -- ABSENT param unions with '{}' and changes nothing; a present set adds its members,
    -- deduped. The claim predicate uses the order-independent `<@` subset test, so the
    -- merged array is intentionally left unsorted.
    required_capabilities = ARRAY(SELECT DISTINCT unnest(
        required_capabilities || COALESCE(sqlc.narg('inferred_capabilities')::text[], '{}'))),
    -- required_tools is SET, absent-safe: a present set REPLACES (it is the run's single
    -- authoritative inferred toolchain list, not merged with a prior source), and an
    -- absent (NULL) param COALESCEs back to the existing column, leaving it untouched.
    required_tools = COALESCE(sqlc.narg('inferred_tools')::text[], required_tools),
    -- PRD #212: the changed-file list the plan turn produced (git status --porcelain,
    -- run as the RUNNER uid). Absent-safe COALESCE like required_tools, but note the
    -- worker sends this on EVERY awaiting_approval round (empty {} when the plan turn
    -- was clean) so each gate reflects that round's tree; a pre-#212 worker omits it,
    -- sending a nil pointer -> SQL NULL -> COALESCE preserves the column.
    plan_changed_files = COALESCE(sqlc.narg('plan_changed_files')::text[], plan_changed_files),
    -- size_class is SET, absent-safe like required_tools: a present (clamped s/m/l) value
    -- REPLACES the column, and an absent (NULL) param COALESCEs back to the existing value,
    -- leaving it untouched. The service clamps to the {s,m,l} vocabulary before passing it,
    -- so a garbled worker report becomes a nil param (no change) rather than a bad value.
    size_class = COALESCE(sqlc.narg('size_class'), size_class),
    session_id = COALESCE(sqlc.narg('session_id'), session_id),
    -- 🔴 INVARIANT, carried by TWO call sites and by nothing else:
    -- NO SETTER MAY LEAVE A RESOLVED open_question_id BEHIND. The sibling clear is in
    -- SetRunRunning, which covers the mid-run path; this one covers M4's pre-run park,
    -- which reaches the gate without ever executing that statement. A third
    -- non-terminal destination added later re-opens this a third time and nothing
    -- would catch it.
    --
    -- Why this statement needs the clear at all (PRD #88 D-AG, measured): M4 lets the
    -- lead ask BEFORE it plans, so a pre-run park goes awaiting_input →
    -- awaiting_approval with no intervening `running` report — SetRunRunning's clear
    -- is simply never reached on that path. The run then sits at the plan gate still
    -- naming a question that was already answered; a worker death there re-delivers
    -- the stale id on the claim, the worker re-uses it for a genuinely NEW question,
    -- and the old consumed `answer` row satisfies the resume guard. That is the
    -- has-ever-been-answered degeneration the identity keying exists to prevent,
    -- reached through the path M4 added rather than the one B2 fixed.
    --
    -- The server-side clear is the ONLY defence available. The worker seeds its map
    -- from the CLAIM, which is a snapshot taken before its own `running` report — so
    -- even though that report clears the column, the worker is already holding the
    -- stale value. No worker-side guard can distinguish a resolved id from a live one
    -- (a claim reports status 'claimed', never the park).
    --
    -- Sufficient by enumeration: from awaiting_input the only non-terminal
    -- destinations are `running` (cleared in SetRunRunning) and `awaiting_approval`
    -- (here). The terminal three never resume, so a stale id there is inert.
    --
    -- Unlike SetRunRunning's clear there is no WHERE-clause interaction to get wrong:
    -- this statement's WHERE never references open_question_id.
    open_question_id = NULL,
    -- Exit contract (PRD #47 Decision 3): leaving 'running' clears any running-run
    -- flag (stalled/looping/slow). The detector re-evaluates for approval_idle from
    -- this transition's fresh status_since; health_notified_at is preserved.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled')
  -- Symmetric with SetRunRunning's guard (PRD #35): the negative predicate above
  -- admits limit_wait, and a re-delivered gate report must not un-park a run. Kept
  -- even though the current worker cannot reach this ordering, because "the caller
  -- never does that today" is what SetRunRunning's own history disproves.
  --
  -- awaiting_input DELIBERATELY GETS NO SUCH GUARD, and this is the row most likely
  -- to be re-litigated: the two parked statuses are HANDLED by opposite mechanisms at
  -- this one statement, so an audit for consistency reads the asymmetry as a gap.
  -- It is not. limit_wait's only exit is a server-side promotion, so blocking the
  -- transition is right for it. awaiting_input -> awaiting_approval IS the PRD #88 M4
  -- pre-run path (park -> answer -> re-plan -> submit_plan -> gate) and is legitimate,
  -- so we ALLOW the transition and clear the resolved id above instead.
  --
  -- Post-#307 the clear here is a FALLBACK, not the normal path. The worker now emits a
  -- `running` report when a clarification resolves with an answer (agent/src/runner.ts
  -- `askUser` settle), so SetRunRunning normally intervenes on the resume and clears
  -- open_question_id FIRST; by the time this statement runs the id is usually already
  -- NULL. This clear is what still saves the run when that `running` report is dropped,
  -- delayed, or never sent (a worker that dies between consuming the answer and
  -- reporting) — the report is fire-and-forget, so the pre-run gate must not depend on
  -- it having landed. The transition being allowed AND clearing the id keeps the
  -- pre-run path correct in both the report-landed and report-absent cases.
  --
  -- And awaiting_input IS still protected, just not here: its guard is SetRunRunning's
  -- consumed-answer identity predicate, which is where an un-park would actually have
  -- to happen. Said explicitly because "handled by opposite mechanisms" otherwise
  -- invites a reader to hunt for its protection in THIS statement, find none, and
  -- conclude it has none.
  --
  -- Measured, not argued: adding `AND status <> 'awaiting_input'` here reddens
  -- TestSetRunAwaitingApprovalClearsOpenQuestionLiveDB, because the pre-run park can
  -- then never reach the plan gate at all — it wedges every pre-run clarification
  -- permanently. Do not add it.
  --
  -- pool_wait IS excluded (PRD #754 M5), on the same reasoning as limit_wait above and
  -- UNLIKE awaiting_input: a held run's only exit is a server-side promote to 'queued', so
  -- a re-delivered gate report must not flip it to awaiting_approval. The awaiting_input
  -- argument (its legitimate pre-run path passes THROUGH awaiting_approval) does not apply
  -- to a held run, which never gates.
  AND status <> 'limit_wait'
  AND status <> 'pool_wait';

-- name: ClearRunRequiredCapabilities :execrows
-- PRD #84 M4 (unit 4c): the user override ("run without the capability", Decision 12).
-- When the owner approves a plan the capability gate would BLOCK — because plan-time
-- inference (or the repo hint) attached a required capability the owning worker cannot
-- satisfy — this clears the run's inferred/hinted requirement set so the subsequent
-- approve is no longer fenced. v1 clears the WHOLE run set (repo hint + inferred); a
-- hint-vs-inference split is a future refinement (Decision 6/12). No security boundary is
-- crossed: the §300 guardrail still denies docker USE on a daemon-less worker at run time,
-- so clearing the SCHEDULING requirement only removes the approval fence, never the
-- runtime protection.
--
-- Owner-scoped (user_id) AND status-guarded (awaiting_approval only): the clear runs from
-- the owner-authenticated approve path, and a run outside the plan gate is a no-op
-- (0 rows), so a stray override on a running/terminal run changes nothing.
UPDATE runs SET required_capabilities = '{}', updated_at = now()
WHERE id = @id AND user_id = @user_id AND status = 'awaiting_approval';

-- name: SetRunIntentSummary :execrows
-- PRD #362 M1: persist a run's plain-English INTENT summary ("what this run will
-- implement"), posted by the worker after the clone is provisioned and before it
-- plans. A PLAIN UPDATE by run id: the idempotent-on-set decision (skip when
-- summary_intent is already set, so a re-claim/resume does not re-spend the owner's
-- token — Decision 3) lives in the service, which reads the run first for its
-- owner/repo/non-terminal guards anyway. :execrows so the caller can confirm the row
-- exists (a foreign/deleted run updates 0 rows), never for a stale-write guard.
UPDATE runs SET
    summary_intent = @summary_intent,
    updated_at     = now()
WHERE id = @id;

-- name: SetRunPlanSummary :execrows
-- PRD #362 M1: persist a run's PLAN summary + deltas with the Decision 3 stale-write
-- guard. The worker sends the plan_md the summary was generated from; this writes
-- summary_plan/summary_deltas ONLY IF that still matches runs.plan_md, so a slower
-- earlier generation cannot overwrite the summary of a newer, revised plan
-- (last-write-wins by PLAN VERSION, not by completion time — no extra hash column).
-- :execrows returns the rows-affected count so the service detects a stale (0-row)
-- write and rejects it as a conflict, distinct from a run-not-found. Matching on the
-- full plan_md text is intentional (simplest correct guard).
UPDATE runs SET
    summary_plan   = @summary_plan,
    summary_deltas = @summary_deltas::jsonb,
    updated_at     = now()
WHERE id = @id AND plan_md = @expected_plan_md;

-- name: SetRunLimitWait :execrows
-- Park a run until the owner's exhausted Anthropic usage window reopens (PRD #35
-- M2). running → limit_wait, non-terminal: the run keeps its issue, its session,
-- its worker affinity and its message history, and the sweeper promotes it back to
-- queued once retry_not_before passes.
--
-- 🔴 THE SOURCE GUARD IS POSITIVE (status = 'running'), WHICH IS THE FIRST POSITIVE
-- SOURCE GUARD IN THIS FILE'S WORKER-REPORT FAMILY — every sibling above is
-- negative (status NOT IN (terminal)). Do NOT "normalize" it to match them.
-- Negative here would admit three transitions that must never happen and one that
-- would be actively unsafe:
--   * queued/claimed → limit_wait: an affinity-queued run parked by a stale report
--     would leave the sweeper's promotion pass owning a run no worker holds.
--   * awaiting_approval → limit_wait: a run sitting at the plan gate is not
--     spending tokens, so it cannot have hit a limit; parking one would swallow the
--     human's pending approval.
--   * limit_wait → limit_wait: a re-delivered park report would bump
--     limit_wait_count a second time and burn RUN_LIMIT_MAX_WAITS on one event.
-- With the positive guard, every one of those is a 0-row no-op, which the service
-- surfaces to the worker as 409 / applied=false. Re-delivery is therefore
-- idempotent, and the worker's cleanup carve-out keys off the RETURNED STATUS
-- rather than off applied, so a refused park cleans up rather than leaking.
--
-- kind <> 'judge' is Decision 14: a judge run is executed by a different runner
-- with its own error path, its value decays (a judge parked for days is reviewing a
-- run nobody remembers), and it is never re-enqueued. It dies with an explanatory
-- reason instead. The guard lives HERE, in SQL, rather than only in Go, because the
-- Go side is what composes that better death and a bypass would silently park.
-- 'chat' needs no clause: a chat run never reaches SetState's run lane at all.
--
-- retry_not_before is computed in GO and passed in — never derived here from
-- limit_resets_at. It carries the Decision 4 cross-check against this user's own
-- gauge, Decision 6e's pool awareness, jitter, and the RUN_LIMIT_MAX_PARK clamp,
-- none of which SQL is positioned to do. limit_resets_at is the WORKER'S REPORT,
-- kept for display and the M5 Slack line; it is deliberately not the gate, so a
-- compromised worker cannot park a run for years.
--
-- limit_wait_count bumps here rather than in Go so the increment and the transition
-- are one statement: a run cannot end up parked without its budget being spent.
-- It is distinct from requeue_count on purpose (Decision 5) — requeue_count counts
-- worker deaths, this counts limit parks, and a shared counter would let one
-- exhaust the other's budget.
--
-- 🔴 THE HEALTH RESET IS MANDATORY HERE, NOT THE STYLE CHOICE THE PRD CALLED IT.
-- ListActiveRunsForHealth is a POSITIVE allowlist ('queued','running',
-- 'awaiting_approval'), so the PRD #47 detector never revisits a parked run. That
-- is listed as a free win — a park can never read "stalled" — and it cuts the other
-- way too: whatever flag was live at park time would FREEZE for the entire park,
-- because nothing will ever revisit the row to clear it. A run parked while flagged
-- stalled would stay stalled for days, and it is user-visible rather than cosmetic
-- (cmd/uzi's crewStateFor reads health AFTER the terminal and gate checks, so a
-- parked run falls straight into crewStalled). That is Success Criterion 2 failing
-- through the health column instead of the status column. Do NOT "fix" it by adding
-- limit_wait to ListActiveRunsForHealth: stalled/looping/slow all describe a
-- RUNNING agent, and making a park flaggable re-introduces the false alarm
-- Decision 3 banks on avoiding. health_notified_at is NOT reset, matching every
-- other exit contract in this file.
-- limit_dead_secret_id is SET AT PARK to the credential this run was spending (PRD
-- #217 M2, D2): the run's own runs.anthropic_secret_id, passed through in Go so an
-- invalid/NULL secret writes NULL. It is cleared on the first claim that records a
-- credential (SetRunAnthropicSecret sets it back to NULL), so the exclusion lives for
-- one recording claim and no longer. sqlc.narg, never @name — this file's multibyte
-- comment blocks break the @name parser (repo memory).
UPDATE runs SET
    status               = 'limit_wait',
    status_since         = now(),
    limit_resets_at      = sqlc.narg('limit_resets_at'),
    rate_limit_type      = sqlc.narg('rate_limit_type'),
    retry_not_before     = @retry_not_before,
    limit_wait_count     = limit_wait_count + 1,
    limit_dead_secret_id = sqlc.narg('limit_dead_secret_id'),
    session_id           = COALESCE(sqlc.narg('session_id'), session_id),
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at           = now()
WHERE id = @id AND worker_id = @worker_id
  AND status = 'running'
  AND kind <> 'judge';

-- name: PromoteLimitWaitRuns :many
-- The sweeper's promotion pass (PRD #35 M2): limit_wait → queued once the clock
-- passes retry_not_before. Backed by idx_runs_limit_wait_retry, a partial index on
-- exactly this predicate, so the pass costs an index scan over a set that is empty
-- on a healthy instance rather than a seq scan of runs.
--
-- 🔴 THIS IS A SINGLE UPDATE THAT RELEASES EVERY ELIGIBLE ROW IN ONE TICK. Nothing
-- here staggers a wave, so "the sweeper tick already spreads them out" is exactly
-- backwards — this statement is what makes the staggering necessary. The ONLY
-- mechanism spreading a promoted wave across a user's credential pool is the 60-180s
-- jitter baked into retry_not_before at PARK time (workersvc/limitwait.go's
-- limitParkJitter, ADR-35 D4). That is written here as well as there because this is
-- where someone reasons about promotion timing, and removing the jitter as
-- redundant-with-the-tick is the specific mistake available from this file.
--
-- started_at = NULL so the resumed run gets a FRESH RUN_TIMEOUT wall (Decision 6d).
-- Without it, SweepRunningTimeout measures the resumed run against a started_at
-- from before a park that may have lasted days, and the run is failed on its first
-- tick back — the feature would deliver a run that resumes and immediately dies.
--
-- requeue_count is deliberately NOT bumped (Decision 5): it counts worker deaths
-- and this is not one. limit_wait_count already recorded the park, and letting a
-- park consume re-queue budget would fail a run for a reason that never happened.
--
-- session_id, last_seq and worker_id are untouched, exactly as in every other
-- requeue query here: the session is what makes the resume a resume rather than a
-- restart, and worker_id is affinity, so the same disk reclaims the run and its
-- clone if that worker is still alive.
--
-- limit_resets_at / retry_not_before / rate_limit_type are LEFT IN PLACE as
-- history — the run view renders "attempt N, last paused on <window>" from them
-- after the resume. A stale retry_not_before cannot re-fire this statement because
-- the status predicate has already moved.
--
-- Health reset for the same reason SetRunLimitWait carries one, from the other
-- side: the detector's allowlist DOES include 'queued', so it re-evaluates the
-- queued signal from this transition's fresh status_since rather than inheriting
-- anything.
--
-- RETURNING id, user_id, status matches every other sweep transition in this file,
-- so the caller can publish each promotion through the broadcaster/notifier
-- fan-out; a promotion to 'queued' moves the board card to In Progress exactly like
-- a requeue.
UPDATE runs SET
    status     = 'queued',
    status_since = now(),
    started_at = NULL,
    -- Issue #783: the fresh wall discards started_at, so the pause banked against the
    -- OLD baseline must be cleared too — otherwise it over-credits the new deadline.
    budget_paused_seconds = 0,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status = 'limit_wait' AND retry_not_before <= @now
RETURNING id, user_id, status;

-- name: SetRunAwaitingInput :execrows
-- PRD #88 M1: the clarification park. Sibling of SetRunAwaitingApproval, and it
-- carries the same PRD #47 exit contract for the same reason.
--
-- The health clear is LOAD-BEARING, not cosmetic. `awaiting_input` is deliberately
-- NOT in ListActiveRunsForHealth (a parked run is not stalled, and inventing an
-- input_idle arm would widen the health enum across four surfaces to buy a reminder
-- that M3's Slack post already delivers). That omission is only safe BECAUSE the run
-- enters the park with health='ok': a run flagged stalled/looping/slow while running
-- would otherwise carry that flag through the entire park with nothing able to clear
-- it. The two are coupled; a live-DB test pins the pair.
--
-- open_question_id is the question's stable identity, supplied by the worker. A
-- resumed worker re-parking on the SAME question re-stamps the SAME value (it reads
-- it back off the claim), which is what makes the SetRunRunning guard above a no-op
-- across a requeue instead of a silent rejection of an already-submitted answer.
UPDATE runs SET
    status           = 'awaiting_input',
    status_since     = now(),
    open_question_id = @open_question_id,
    session_id       = COALESCE(sqlc.narg('session_id'), session_id),
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: ClearRunMilestonesCompleted :execrows
-- PRD #628 M4: the ONLY non-union writer of milestones_completed. milestones_completed
-- is otherwise a monotone union (SetRunRunning ~:926-930, SetRunCompleted ~:1470-1473 —
-- "MUST stay a UNION"); this targeted clear resets it to empty when a cross-worker
-- re-claim reseeds from the DEFAULT branch (no committed work recovered), so pass-1's
-- stale milestones don't read as "done" while pass-2 re-implements them.
--
-- The status guard is an ALLOWLIST of exactly the ACTIVE-CLAIM states this reset fires in
-- (claimed/running), NOT a denylist of terminals. It must stay a SUBSET of SetRunRunning's
-- admit set (SetRunRunning also conditionally admits awaiting_* states, so this is a strict
-- subset, not a match) — the clear is paired with the SAME-call SetRunRunning that refills
-- the union from empty, so clear ⊆ refill guarantees the clear never empties a run
-- SetRunRunning would then refuse to refill (it refuses a run parked in limit_wait or
-- awaiting_approval). A denylist would let a stale `running` report carrying
-- seeded_from_default empty a parked/awaiting-approval run's list WITHOUT the paired refill —
-- emptying-without-refill. Ownership-guarded too (id + worker_id), so a superseded/zombie
-- worker cannot wipe the current owner's live progress.
UPDATE runs SET milestones_completed = '[]'::jsonb, updated_at = now()
WHERE id = @id AND worker_id = @worker_id
  AND status IN ('claimed', 'running');

-- name: SetRunAwaitingFollowup :execrows
-- PRD #517 M2/M3: the interactive-task park. On signal_done an interactive task run
-- parks here IN-PROCESS (Decision 3) rather than finalizing to `completed`, holding
-- its worker slot, clone and session alive so `uzi run follow-up` can resume the SAME
-- agent session with full context. Sibling of SetRunAwaitingInput/limit_wait, and it
-- carries the same PRD #47 exit contract for the same reason.
--
-- Unlike SetRunAwaitingInput there is NO question requirement: the park is not gated
-- on a clarification question, so it takes no open_question_id (the worker resumes on
-- a `follow_up` steering input, which SetRunRunning's Decision-7 wake guard keys on).
--
-- The health clear is LOAD-BEARING, not cosmetic, exactly as on SetRunAwaitingInput:
-- awaiting_followup is (like awaiting_input) not a stalled/looping state, so a run
-- must enter the park with health='ok' or it would carry a stale flag through the
-- entire park with nothing able to clear it.
--
-- The worker-side kind/interactive guard lives in SetState (workersvc): the park is
-- accepted only for an interactive task run, so this statement stays a plain guarded
-- status write like its siblings. status NOT IN (terminal) makes a report onto an
-- already-terminal run (a cancel raced in) a no-op → 0 rows → "already terminal".
UPDATE runs SET
    status     = 'awaiting_followup',
    status_since = now(),
    session_id = COALESCE(sqlc.narg('session_id'), session_id),
    -- Issue #552 M1 / #559 M1: the park-scoped follow_up watermark. The value is now
    -- WORKER-PROVIDED — the highest follow_up id the worker has ALREADY DELIVERED/applied
    -- to a turn at the moment it parks — and CLAMPED here to the server-derived max
    -- already-consumed follow_up as a safety ceiling (the LEAST(...) below). When the
    -- worker OMITS it (an old worker, or the very first park before anything was
    -- delivered) the COALESCE falls back to that same server-derived max-consumed, so an
    -- absent param is byte-identical to the pre-#559 pure-server behavior.
    --
    -- Why worker-provided: deriving the watermark purely from the server's max-consumed
    -- races a follow_up consumed DURING this park report's DB round-trip — it would fold a
    -- not-yet-applied follow_up into the watermark and permanently strand the run (the
    -- guard then never sees a follow_up NEWER than the watermark). The worker knows exactly
    -- which follow_ups it has applied, so it reports that; a correct worker's last-delivered
    -- id is ALWAYS ≤ max-consumed, so the clamp never bites it. The clamp exists only to
    -- neutralize a buggy huge value that would otherwise strand the run forever.
    --
    -- A later follow_up with a higher id — the one the resuming worker will consume to wake
    -- THIS park — is what SetRunRunning's Decision-7 guard requires to admit
    -- awaiting_followup → running, so the watermark discriminates "THIS park's follow_up"
    -- from "any follow_up ever consumed".
    --
    -- CONSUMED-only (consumed_at IS NOT NULL) remains load-bearing on the server ceiling:
    -- the follow_up that wakes a park is UNCONSUMED until it wakes, so a consumed-only MAX
    -- never advances past it. Monotonicity across re-parks is now ENFORCED by the
    -- GREATEST(COALESCE(open_followup_id, 0), ...) current-value floor below — no longer
    -- merely asserted from the protocol. Issue #817: the old "(or the worker's
    -- last-delivered id, whichever is lower)" clause was exactly what broke it, because a
    -- fresh re-claiming worker reports a present 0 (min of a monotone value and a value
    -- that resets to 0 is not monotone). The watermark simply names the last follow_up
    -- already spent, and anything newer is a genuine new steer.
    -- The GREATEST(0, ...) floor is the LOWER bound the LEAST ceiling does not give:
    -- LEAST only bounds a huge value from above, so a nonsensical NEGATIVE worker value
    -- (e.g. -1) would otherwise pass through and fail-open THIS run's own wake guard
    -- (`id > COALESCE(open_followup_id, 0)` is `id > -1`, true for every positive
    -- bigserial id, so any consumed follow_up wakes it — reopening #558 for that run).
    -- Flooring to 0 maps it to "nothing applied" (the first-park value), matching the
    -- stated "neutralize a buggy value" intent. GREATEST(0, ...) never affects a correct
    -- worker: its last-delivered id is always ≥ 0.
    -- Issue #817: floor the SET at the run's currently-stored value so the watermark
    -- can never REGRESS. The RHS `open_followup_id` reads the PRE-UPDATE (old) row —
    -- the same self-referential SET-RHS pattern this file already uses for
    -- milestones_completed (see SetRunRunning and SetRunCompleted). Strand-free: every
    -- GREATEST operand is ≤ the run's MAX(consumed follow_up id), which is monotone
    -- non-decreasing, and the unconsumed wake follow_up has id > that max, so
    -- `id > open_followup_id` always still holds. SAFETY DEPENDS on run_user_inputs
    -- being append-only and consumed_at set-once: a retention/pruning job that
    -- hard-deletes consumed follow_up rows would let MAX(consumed) drop below a prior
    -- stamp, and this floor — unlike the pre-fix pure-LEAST clamp — would then hold the
    -- watermark too high; such a change must reckon with the wake guard.
    open_followup_id = GREATEST(0, COALESCE(open_followup_id, 0), LEAST(
        COALESCE(sqlc.narg('open_followup_id')::bigint,
                 (SELECT COALESCE(MAX(id), 0) FROM run_user_inputs
                  WHERE run_user_inputs.run_id = @id AND kind = 'follow_up' AND consumed_at IS NOT NULL)),
        (SELECT COALESCE(MAX(id), 0) FROM run_user_inputs
         WHERE run_user_inputs.run_id = @id AND kind = 'follow_up' AND consumed_at IS NOT NULL))),
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRunCompleted :execrows
-- completed is the terminal MR-opened event → Human Review. move_pending_since is
-- stamped here (same statement as the status write) so a crash before the forge
-- move still leaves the reconcile loop a marker to heal from.
UPDATE runs SET
    status             = 'completed',
    status_since       = now(),
    branch             = @branch,
    mr_iid             = @mr_iid,
    -- The MR's web URL as the forge reported it (PRD #65 D8), written here because
    -- the worker that opens the MR is the only thing holding the URL at the moment
    -- it exists. Plain assignment, deliberately matching mr_iid rather than
    -- session_id's COALESCE-narg: the iid and the URL are ONE fact reported by one
    -- worker in one payload, so they must not persist under different conventions.
    -- An old worker omits it (R8) and textParam(nil) lands NULL, which the web
    -- renders via the legacy forgeUrls.ts reconstruction exactly as it did before
    -- the column existed.
    mr_web_url         = @mr_web_url,
    session_id         = COALESCE(sqlc.narg('session_id'), session_id),
    -- fix_verdict carries a ci_fix run's outbound 'not_code' verdict on completion
    -- (PRD #6); NULL for every issue run and for a ci_fix that produced a fix (its
    -- verdict is stamped verified/fix_failed later by the pipeline sync).
    fix_verdict        = COALESCE(sqlc.narg('fix_verdict'), fix_verdict),
    -- The PRD path this run's lead declared it moved (PRD #72 M4), NULL unless an
    -- issue run declared one that validated. Plain assignment for the same reason
    -- mr_web_url above gives: the branch, the MR and the declared path are ONE fact
    -- reported by one worker in one payload, so they must not persist under
    -- different conventions.
    prd_done_path      = @prd_done_path,
    -- issue #279: the report-only marker and the lead's persisted findings.
    -- Plain assignment for the same reason mr_web_url/prd_done_path above give:
    -- the branch, the MR, the declared path AND this completion's report shape are
    -- ONE fact reported by one worker in one payload, so they must not persist
    -- under different conventions. report_only plain-assigns on every completed
    -- transition (false for a normal MR completion — correct, it is the marker for
    -- the no-MR case); report_md plain-assigns (nil→NULL) exactly like prd_done_path.
    report_only        = @report_only,
    report_md          = @report_md,
    -- PRD #634 M3: stamp stop_kind='scope_capped' on an operator scope-truncated completion.
    -- COALESCE-narg (like session_id/fix_verdict above): NULL param leaves any existing
    -- stop_kind untouched, so a normal completion is byte-identical to before.
    stop_kind          = COALESCE(sqlc.narg('stop_kind'), stop_kind),
    -- PRD #265 M1: reconcile the milestone tracker at completion. The lead declares on
    -- signal_done which frozen milestones it finished; the server UNIONs them here so a
    -- run that never emitted a mid-run `report_progress` still lands a truthful tracker.
    -- This CASE is copied VERBATIM from SetRunRunning's milestones_completed union
    -- (see this file's `SetRunRunning`, milestones_completed) and MUST stay a UNION, not
    -- the plain assignment mr_iid/prd_done_path use above: a run that already reported
    -- {m1,m2} via checkpoint and then declares {m3} on signal_done must end at
    -- {m1,m2,m3}, never be overwritten to {m3}. Keep the two union sites' dedup
    -- semantics identical — if you change one, change both. NULL param (nothing declared,
    -- or a non-issue/invalid set dropped by progressParams) leaves the column untouched,
    -- so a no-declaration completion is byte-identical to before.
    milestones_completed = CASE
        WHEN sqlc.narg('milestones_completed')::jsonb IS NULL THEN milestones_completed
        ELSE COALESCE((SELECT jsonb_agg(DISTINCT e)
                       FROM jsonb_array_elements_text(COALESCE(milestones_completed, '[]'::jsonb) || sqlc.narg('milestones_completed')::jsonb) AS e), '[]'::jsonb)
    END,
    -- PRD #265 D4: "in progress" is meaningless on a terminal run, so the snapshot is
    -- cleared on every terminal transition a milestone-bearing (issue) run can reach (an
    -- explicit clear — progressParams' nil-input convention leaves columns untouched, so it
    -- will not happen for free). Same clear appears on SetRunFailed / MarkRunFailedByID /
    -- CancelRunServerSide / FailRunAutoStop / RejectRunServerSide / SweepRunningTimeout and
    -- the stale-worker failers below. (SweepIdleChatRuns also completes runs but is kind=
    -- 'chat'-only, and progressParams gates milestone writes to issue runs, so its snapshot
    -- is always NULL — the clear there would be a no-op and is deliberately omitted.)
    milestones_in_progress = NULL,
    -- Arm the M5 patch marker. Explicit rather than left to the column default,
    -- because SetRunCompleted can in principle run on a row that already carries a
    -- stamp from an earlier terminal transition.
    prd_patch_settled_at = NULL,
    -- issue #329: a completion that supersedes a run_timeout failure must clear the
    -- stale timeout classification. No-op on a normal completion (both already NULL).
    failure_reason     = NULL,
    fail_origin        = NULL,
    move_pending_since = now(),
    finished_at        = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  -- issue #329: a genuine worker completion (it opened the MR) supersedes a
  -- wall-clock RUN_TIMEOUT failure. Scoped to fail_origin='run_timeout' ONLY: a
  -- human 'cancelled' still wins, and a worker's own 'failed'/'worker_lost' is never
  -- overridden. worker_id=@worker_id (SweepRunningTimeout leaves it intact) keeps
  -- this safe — only the still-owning worker can supersede.
  AND (status NOT IN ('completed', 'failed', 'cancelled')
       OR (status = 'failed' AND fail_origin = 'run_timeout'));

-- name: ReconcileRunMR :execrows
-- issue #329: record the MR the worker actually opened, INDEPENDENT of the terminal
-- status label, so a run that opened an MR never reports "MR: none". Non-clobbering
-- (COALESCE keeps an already-recorded value, e.g. the authoritative SetRunCompleted
-- write) and status-agnostic (no status predicate) — it heals a run whose terminal
-- transition landed 'failed'/'cancelled' while the worker still opened an MR. Scoped
-- by worker_id like the terminal writers. Deliberately does NOT touch the watcher-owned
-- MR-state column (the SetRunMRState invariant guarded by TestMRStateIsWatcherOwned);
-- naming that column literally here would itself trip that substring guard.
UPDATE runs SET
    mr_iid     = COALESCE(mr_iid, @mr_iid),
    mr_web_url = COALESCE(mr_web_url, @mr_web_url),
    branch     = COALESCE(branch, @branch),
    updated_at = now()
WHERE id = @id AND worker_id = @worker_id;

-- name: SetRunFailed :execrows
-- failed restores the origin column → move_pending_since stamped in the same
-- statement (same-tx crash-window closure, as for completed).
UPDATE runs SET
    status             = 'failed',
    status_since       = now(),
    failure_reason     = @failure_reason,
    -- PRD #69 M7a: the TRUSTED failure class, always set from Go (the worker-reported
    -- `failed` arm coerces req.fail_origin through the allowlist and defaults a
    -- classless failure to 'agent_failure'; the limit-opt-out non-park path stamps
    -- 'rate_limited'). Never derived from failure_reason, which is never parsed.
    fail_origin        = @fail_origin,
    -- PRD #377 M1: the agent's secret-scrubbed, size-capped branch diff, preserved on a
    -- workflow_scope_missing failure so a human can apply the work the bot PAT could not
    -- push. NULL on every other failed path (only that arm sends a non-nil value).
    preserved_patch    = @preserved_patch,
    session_id         = COALESCE(sqlc.narg('session_id'), session_id),
    move_pending_since = now(),
    finished_at        = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: MarkRunFailedByID :execrows
-- Service-internal fail (e.g. a claim whose secrets are missing/undecryptable):
-- the run was just claimed by this worker but cannot run. failed → origin
-- restore, so it stamps move_pending_since like the other failed paths.
UPDATE runs SET
    status             = 'failed',
    status_since       = now(),
    failure_reason     = @failure_reason,
    -- PRD #69 M7a: the TRUSTED failure class. recoverClaimAssembly maps each infra
    -- sentinel to its own origin (errCredentialUnavailable → 'credential_unavailable',
    -- errToolPackagesRejected → 'provisioning_failed', errGuardrailBlockedClaim →
    -- 'guardrail_blocked') and passes it here, so the class survives the assembly that
    -- would otherwise collapse into one indistinguishable failure_reason.
    fail_origin        = @fail_origin,
    move_pending_since = now(),
    finished_at        = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: CancelRunServerSide :execrows
-- Server-side cancel for a run with no live poller (still queued, or its worker
-- went stale): the user input is not stranded waiting for a GET /inputs poll
-- that will never come. cancelled restores the origin column → stamp. stop_kind is
-- stamped 'cancelled' for uniformity (PRD #33 Decision 3), though isStoppedRun's
-- status='cancelled' branch already treats this run as a deliberate stop.
UPDATE runs SET status = 'cancelled', status_since = now(), stop_kind = 'cancelled', move_pending_since = now(), finished_at = now(),
    -- PRD #503 M3: persist the operator's OPTIONAL cancel reason. @stop_reason binds a
    -- nullable pgtype.Text: an invalid/zero value stores NULL (no reason supplied).
    stop_reason = @stop_reason,
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: GetActiveMRReworkRunForMR :one
-- Resolve the single non-terminal mr_rework run for a (repo, MR). Used by the
-- mid-flight abort (issue #853): when the MR-close watcher sees the MR leave the
-- opened state, the active rework is cancelled so a live worker stops spending.
-- The WHERE is byte-identical to the partial unique index uq_runs_one_active_mr_rework
-- (migration 00167) — that index is the ONLY reason this can be :one; widening it, or
-- narrowing the status set here, would silently break the single-row guarantee.
SELECT * FROM runs
WHERE repo_id = @repo_id::uuid AND mr_iid = @mr_iid
  AND kind = 'mr_rework'
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: CancelRunByWorker :execrows
-- Live-worker cancel transition (PRD #503 M1). When a LIVE worker consumes a cancel
-- verdict it reports `failed`; SetState's failed arm routes HERE off the run's already
-- loaded stop_kind='cancelled' (stamped by CreateStopVerdictInput BEFORE this report)
-- instead of SetRunFailed, so an operator cancellation is not mis-classified as
-- 'agent_failure'. This converges the live cancel path with the server-side
-- CancelRunServerSide: status 'cancelled', fail_origin NULL (a cancel is not a failure,
-- so it is never judged — Gate 0). It is worker-scoped (@worker_id) because SetState holds
-- a worker, not a user, so CancelRunServerSide (user_id-scoped) is unusable from it.
-- stop_kind is left untouched (already 'cancelled'). Terminal-run cleanup + guard mirror
-- SetRunFailed exactly, so a report onto an already-terminal run is a 0-row no-op.
UPDATE runs SET
    status             = 'cancelled',
    status_since       = now(),
    fail_origin        = NULL,
    move_pending_since = now(),
    finished_at        = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: FailRunAutoStop :execrows
-- Server-side auto-stop (PRD #108 M5) for a run whose message writes are in a
-- confirmed permanent-failure loop AND which has no live poller to consume a stop
-- verdict — plus the escalation for a live worker that was sent one and ignored it.
--
-- Modelled on SweepRunningTimeout (a server-side failed transition) and on
-- CancelRunServerSide (which stamps stop_kind in the same statement). failed
-- restores the origin column, so move_pending_since is stamped here exactly as
-- every other server-side failed path does; omit it and the run rots in the wrong
-- board column forever.
--
-- stop_kind is stamped in the SAME statement as the status (PRD #33 Decision 3), so
-- the auto-stop identity can never be lost independently of the transition that
-- created it. It is the ONLY field that survives both halves of this stop: on the
-- live-poller half the worker reports its own terminal state through SetRunFailed,
-- which overwrites failure_reason unconditionally with "run cancelled" and never
-- touches stop_kind. So failure_reason below is decoration and MUST NOT BE PARSED.
--
-- Status-scoped, and the scope is a guard rather than an optimisation: the
-- evaluator re-reads the run before deciding, and this clause closes the race
-- between that read and this write. A run that reached terminal in between is a
-- no-op (rows == 0), which is also why the escalation can fire unconditionally
-- instead of re-testing liveness — the SQL is the guard.
--
-- No user_id predicate, unlike CancelRunServerSide/RejectRunServerSide: those are
-- driven by a request from a user who must be proven to own the run, and this is
-- driven by the server's own sweeper, which has no user to scope to. The
-- authorization that matters happened upstream — every failure that built this
-- run's streak was recorded only after runOwnedByWorker succeeded.
UPDATE runs SET status = 'failed', status_since = now(),
    failure_reason     = @failure_reason,
    stop_kind          = 'auto_stopped',
    -- PRD #69 M7a: the trusted failure class for the auto-stop. Overlaps stop_kind
    -- deliberately (see 00126) so this failed writer, like every other, sets a
    -- non-NULL origin without a consumer joining two columns.
    fail_origin        = 'auto_stopped',
    move_pending_since = now(),
    finished_at        = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id
  AND status NOT IN ('completed', 'failed', 'cancelled')
  -- limit_wait excluded EXPLICITLY (PRD #35) — the third statement in this file to
  -- need it, for the same reason as SetRunRunning and SetRunAwaitingApproval above:
  -- the negative predicate on the line above ADMITS a parked run. Auto-stopping one
  -- would be wrong on the merits, not merely unreachable: its message writes are not
  -- in a permanent-failure loop, they have simply STOPPED, because the worker went
  -- away by design and the server owns the resume.
  --
  -- The reason to spend a clause on an unreachable case is that the invariant is
  -- currently stated in a DIFFERENT PACKAGE from the one that enforces it.
  -- workersvc/autostop.go's `if run.Status != "running"` is the only thing excluding
  -- a park today, and that line's own comment justifies itself entirely in terms of
  -- `queued` and PRD #108's requeue streak — parking is not mentioned there, because
  -- it did not exist when it was written. Someone relaxing that Go line later (to
  -- cover awaiting_approval, say) would make parked runs auto-stoppable with no SQL
  -- backstop and no failing test. This is that backstop, written where the guard is
  -- ENFORCED rather than where it is currently derived.
  AND status <> 'limit_wait'
  -- awaiting_input (PRD #88) is the same shape of park and the argument above
  -- transfers verbatim: equally excluded today by that one Go line, equally unmentioned
  -- in that line's own comment, and equally exposed if someone relaxes it. Equally
  -- inert today, and added for the same reason — a backstop belongs where the guard is
  -- enforced, not where it currently happens to be derived.
  AND status <> 'awaiting_input'
  -- awaiting_followup (PRD #517) is the interactive-task park and the same argument
  -- transfers verbatim once more: it is excluded today only by autostop.go's single
  -- `if run.Status != "running"` line, unmentioned in that line's own comment, and
  -- exposed the day someone relaxes it. An auto-stopped park would be wrong on the
  -- merits — its message writes have STOPPED (the worker parked after signal_done,
  -- awaiting a follow-up), not looped — so this is the SQL backstop for that day.
  AND status <> 'awaiting_followup'
  -- pool_wait (PRD #754 M5) is the fourth park and the same argument transfers verbatim: a
  -- held run's writes have STOPPED (the worker parked on an empty pool, awaiting a pooled
  -- token), not looped, so auto-stopping one would be wrong on the merits. It is excluded
  -- today only by autostop.go's single `if run.Status != "running"` line, unmentioned in
  -- that line's comment, and exposed the day someone relaxes it — so this is its SQL backstop.
  AND status <> 'pool_wait';

-- name: RejectRunServerSide :execrows
-- Server-side plan rejection → failed → origin restore → stamp. stop_kind is
-- stamped 'plan_rejected' in the same statement as the status/failure_reason write
-- (PRD #33 Decision 3), so this failed run is recognised as a deliberate stop
-- regardless of the failure_reason text.
UPDATE runs SET status = 'failed', status_since = now(), stop_kind = 'plan_rejected',
    -- PRD #69 M7a: trusted failure class, overlapping stop_kind deliberately (see 00126).
    fail_origin = 'plan_rejected',
    failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: UpdateRunLastSeq :execrows
-- Advance the message high-water mark (never regresses) AND bump last_activity_at
-- (PRD #47 Decision 2). AppendMessages calls this once per batch that carried a
-- genuinely new (higher-seq) message, so the activity marker rides the existing
-- write for free — no per-tick run_messages aggregate and no standalone
-- activity-bump endpoint. A pure-duplicate re-delivery skips this call (maxSeq not
-- advanced), so last_activity_at reflects real new activity, which is exactly what
-- the stalled signal wants.
UPDATE runs SET last_seq = GREATEST(last_seq, @seq), last_activity_at = now()
WHERE id = @id;

-- Sweeper: run-level timeouts and worker-loss recovery -----------------------

-- name: SweepClaimedNeverStarted :many
-- claimed but never started past the grace window → back to queued (worker_id
-- kept for affinity so the same disk reclaims it). RETURNING id, user_id, status
-- so the sweeper can publish each transition through the broadcaster/notifier
-- fan-out (PRD #25 M3: sweeper-driven transitions were previously silent).
UPDATE runs SET status = 'queued', status_since = now(),
    -- Exit contract (PRD #47 Decision 3): reset on the way to a fresh 'queued'; the
    -- detector re-evaluates the queued signal from this transition's status_since.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status = 'claimed' AND claimed_at < @cutoff
RETURNING id, user_id, status;

-- name: RequeueClaimedRunToQueued :execrows
-- Vault lock race (PRD #32 M3): a run claimed while the owner's vault was
-- unlocked, then locked before assembleClaim could open the Anthropic token.
-- Reset it to queued — NOT failed, which is terminal (MarkRunFailedByID) and
-- would violate "a locked owner's run waits, never fails". worker_id is left
-- intact for resume affinity. Guarded on status='claimed' so a run that a
-- concurrent path already advanced is untouched. Mirrors
-- SweepClaimedNeverStarted but targets exactly one run by id. Stays :execrows
-- (not :many like the sweeps): this runs on the claim path, not the sweeper, and
-- deliberately does not broadcast the claimed→queued transition (matching the
-- reviewed PRD #32 M3 behavior — it is a rare lock-race requeue, not a sweep).
UPDATE runs SET status = 'queued', status_since = now(),
    -- Exit contract (PRD #47 Decision 3): mirrors SweepClaimedNeverStarted — a
    -- 'claimed' run never carries a flag, so this is defensive, but it keeps every
    -- claimed→queued path uniform.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND status = 'claimed';

-- name: SetRunPoolWait :execrows
-- Hold an `auto` run whose token pool is genuinely empty (PRD #754 M4). claimed →
-- pool_wait, NON-TERMINAL and NON-LOCKING: the run keeps its worker_id affinity, and
-- M5 resumes it (reactively when a token is pooled, or manually). This REPLACES M2's
-- interim requeue (RequeueClaimedRunToQueued on errAutoPoolEmpty): the auto lane must
-- never spend the non-pooled owner default and must not hard-fail a holdable run, so
-- a distinct status is the hold rather than churning the queue.
--
-- 🔴 THE SOURCE GUARD IS POSITIVE (status = 'claimed'), like SetRunLimitWait's
-- status='running' and unlike the negative sibling guards above. A held run only ever
-- comes from a JUST-CLAIMED run in assembleClaim (recoverClaimAssembly runs on the
-- claim path, before the worker starts), so 'claimed' is the only legitimate source.
-- Every other transition is a 0-row no-op, so a re-delivered or out-of-order report
-- cannot re-hold a run that a concurrent path already advanced. kind <> 'judge'
-- mirrors SetRunLimitWait: a judge never holds (Decision 14).
--
-- 🔴 THIS IS NOT A USAGE PARK (PRD Decision 9). An empty pool is not a usage-limit
-- event, so the limit-wait budget must be untouched: limit_wait_count is NOT bumped,
-- and limit_resets_at / retry_not_before / rate_limit_type are NOT set. Folding an
-- empty-pool hold into the usage-limit machinery would let a pooling gap consume the
-- RUN_LIMIT_MAX_WAITS budget a genuine limit event needs.
--
-- limit_dead_secret_id is LEFT AS-IS (deliberately not cleared): M3's exclude-relax
-- reads it on resume to know which just-parked credential's window is still closed, so
-- clearing it here would lose the exclusion across the hold.
--
-- started_at = NULL so a later resume gets a FRESH RUN_TIMEOUT wall (Decision 6d, same
-- as PromoteLimitWaitRuns): without it SweepRunningTimeout would measure the resumed
-- run against a started_at from before a hold that may have lasted a long time.
--
-- Health reset (health='ok', health_reason=NULL, health_since=NULL) exactly as
-- SetRunLimitWait does: ListActiveRunsForHealth is a POSITIVE allowlist that never
-- revisits a held run, so whatever flag was live at hold time would freeze for the
-- whole hold with nothing to clear it. The DISTINCT STATUS is the signal — do NOT
-- write a human sentence into health_reason (that would break the detector's
-- single-writer invariant; the UI/CLI render the "add a token to the pool" copy as
-- fixed text keyed on the status). worker_id is kept for resume affinity.
UPDATE runs SET
    status       = 'pool_wait',
    status_since = now(),
    started_at   = NULL,
    -- Issue #783: the fresh wall discards started_at, so the pause banked against the
    -- OLD baseline must be cleared too — otherwise it over-credits the new deadline.
    budget_paused_seconds = 0,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at   = now()
WHERE id = @id AND worker_id = @worker_id
  AND status = 'claimed'
  AND kind <> 'judge';

-- name: ListPoolWaitRuns :many
-- The reactive-resume worklist (PRD #754 M5): every run currently held in pool_wait,
-- oldest first (status_since ASC), so the reactive pass promotes the LONGEST-waiting
-- run of each owner first. The sweeper groups the result by user_id and, for a user
-- whose token pool is now non-empty, promotes exactly the first (oldest) held run it
-- sees for that user — the anti-stampede stagger lives in Go, not here.
--
-- Backed by the partial index idx_runs_pool_wait (`ON runs (status_since) WHERE
-- status = 'pool_wait'`, migration 00166), so this is an index scan of only the held
-- subset — never a sequential scan of the whole (unbounded) runs table — and the
-- oldest-first ORDER BY is index-ordered. The held set is expected to be tiny (a run
-- holds only while an auto owner's whole pool is genuinely empty, a transient state M5
-- resumes out of); the index mirrors limit_wait's idx_runs_limit_wait_retry.
SELECT id, user_id, status_since FROM runs
WHERE status = 'pool_wait'
ORDER BY status_since ASC;

-- name: PromotePoolWaitRun :execrows
-- Owner-scoped promote of ONE held run: pool_wait → queued (PRD #754 M5). Used by BOTH
-- the reactive sweeper pass (which passes the run's own user_id) and the manual
-- `uzi run resume-now` verb (which passes the authenticated caller's id) — the
-- user_id predicate is what makes resume-now unable to promote a foreign run, and what
-- scopes the reactive pass to the run it selected for that user.
--
-- Field handling mirrors PromoteLimitWaitRuns: started_at = NULL so the resumed run gets
-- a FRESH RUN_TIMEOUT wall (Decision 6d) — without it SweepRunningTimeout would measure
-- the resume against a started_at from before a hold that may have lasted a long time, and
-- fail it on its first tick back. Health is reset (health='ok'/NULL/NULL) because the
-- detector's allowlist includes 'queued', so it re-evaluates from this transition's fresh
-- status_since rather than inheriting a flag that froze at hold time (ListActiveRunsForHealth
-- never revisited the held run to clear it). session_id, last_seq and worker_id are left
-- intact for resume affinity, exactly as PromoteLimitWaitRuns leaves them.
--
-- 🔴 THE SOURCE GUARD IS POSITIVE (status = 'pool_wait'): a run that is not currently held
-- is a 0-row no-op, which is exactly what lets resume-now's handler tell "not held" (409)
-- apart from "not yours / absent" (404) by re-reading the run after a 0-row result. The
-- status predicate also makes a re-delivered or racing promote inert once the run has moved.
UPDATE runs SET
    status       = 'queued',
    status_since = now(),
    started_at   = NULL,
    -- Issue #783: the fresh wall discards started_at, so the pause banked against the
    -- OLD baseline must be cleared too — otherwise it over-credits the new deadline.
    budget_paused_seconds = 0,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at   = now()
WHERE id = @id AND user_id = @user_id
  AND status = 'pool_wait';

-- name: SweepRunningTimeout :many
-- running past RUN_TIMEOUT → failed (a hung agent is failed without a human).
-- Stamps move_pending_since so the (forge-free) sweep leaves the isolated
-- reconcile loop a marker to restore the origin column later. Chat runs are exempt
-- (Decision 3): a chat legitimately parks for a long time between turns, so its own
-- idle/turn clocks (SweepIdleChatRuns + the worker-side timers) bound it instead. Judge
-- runs are exempt too (PRD #69 M6 follow-up): M6 stamps started_at on the judge run so it
-- now sits in 'running' during its single trace-fetch + model turn, where before it went
-- claimed→completed and this sweep never saw it. A judge carries no work-run wall budget
-- and is bounded by its own runner-side timeout, so folding it in here would newly fail a
-- slow judge (large trace / slow API) that would otherwise complete.
--
-- Interactive task runs are exempt too (PRD #517 Decision 6, `interactive = false`
-- below). An interactive task is user-paced like a chat: it parks at awaiting_followup
-- between follow-ups and, over many turns, can legitimately be alive far longer than
-- RUN_TIMEOUT. started_at is stamped ONCE and never reset, so on the resume back to
-- 'running' the ORIGINAL started_at is already past the wall budget and the first sweep
-- tick would fail a legitimately-resumed long-lived run — the exact use case the feature
-- exists for. The park itself is already exempt (status <> 'running'); it is the RESUME
-- that re-exposes it, so the exemption must live on the run's kind, not its status. It is
-- instead bounded by the M5 worker idle timeout. `interactive` is NOT NULL DEFAULT false,
-- so this changes NOTHING for non-interactive runs.
--
-- PRD #122 M2 (Decision 5b): the cutoff is now PER-RUN, not a single global @cutoff.
-- A run that froze a scaled budget carries budget_wall_seconds; the sweep honours it,
-- falling back to global_timeout_seconds (RUN_TIMEOUT) for a NULL-budget run — so a
-- 0/1-milestone run is still failed at the global 2h and a seven-milestone run gets its
-- derived 8h. Computed against now rather than a pre-subtracted cutoff so the per-run
-- interval can be applied in SQL.
--
-- The 8h wall CEILING (budget_wall_ceiling_seconds) is NOT re-applied here: it is
-- enforced by the freeze WRITERS (SetRunRunning / CreateApprovePlanInput), which LEAST()
-- it in before persisting. This consumer trusts budget_wall_seconds as an already-capped,
-- server-only, IMMUTABLE value — so a future writer that persists an UNCAPPED budget_wall_seconds
-- would bypass the ceiling here, and the cap must stay at every write path, not be moved to reads.
UPDATE runs SET status = 'failed', status_since = now(), failure_reason = @failure_reason,
    -- PRD #69 M7a: the trusted failure class for a run killed by RUN_TIMEOUT.
    fail_origin = 'run_timeout',
    move_pending_since = now(), finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a timed-out run must not keep a stale ⚠.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status = 'running'
  -- Issue #783: the deadline EXCLUDES budget_paused_seconds (time this run spent parked
  -- at a human gate), so gate-wait does not consume the implementation budget.
  -- budget_paused_seconds is NOT NULL DEFAULT 0 (migration 00173), so it is a plain
  -- additive term that is 0 for a run that never parked.
  AND started_at < (sqlc.arg('now')::timestamptz
        - make_interval(secs => COALESCE(budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                              + budget_paused_seconds))
  AND kind NOT IN ('chat', 'judge')
  AND interactive = false
RETURNING id, user_id, status;

-- name: FailRunsOfStaleWorkersOverCap :many
-- A stale worker's non-terminal run that has already used its re-queue budget →
-- failed instead of re-queued. Stamps move_pending_since (reconcile restores the
-- origin column; the sweep itself never touches the forge — worker-loss recovery
-- must not wait on a down forge).
UPDATE runs SET status = 'failed', status_since = now(), failure_reason = @failure_reason,
    -- PRD #69 M7a: the trusted failure class for an orphaned run whose worker is gone.
    fail_origin = 'worker_lost',
    move_pending_since = now(), finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND requeue_count >= @max_requeues
  AND worker_id IN (
      SELECT id FROM workers WHERE last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff
  )
RETURNING id, user_id, status;

-- name: RequeueRunsOfStaleWorkers :many
-- A stale worker's non-terminal run within its re-queue budget → back to queued
-- (worker_id kept for affinity, requeue_count incremented).
UPDATE runs SET status = 'queued', status_since = now(), requeue_count = requeue_count + 1,
    -- Exit contract (PRD #47 Decision 3): reset on the way back to 'queued'; the
    -- detector re-evaluates the queued signal from this transition's status_since.
    health = 'ok', health_reason = NULL, health_since = NULL,
    -- Issue #783: bank park time before a worker-death requeue -> queued, since started_at
    -- survives the requeue and the later claimed->running resume would not see the park.
    -- awaiting_followup is intentionally excluded: interactive runs are exempt from
    -- SweepRunningTimeout entirely (interactive = false), so they have no wall deadline.
    budget_paused_seconds = budget_paused_seconds
        + CASE WHEN status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int)
               ELSE 0 END,
    updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND requeue_count < @max_requeues
  AND worker_id IN (
      SELECT id FROM workers WHERE last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff
  )
RETURNING id, user_id, status;

-- Register-time orphan recovery (worker-scoped) ------------------------------

-- name: FailWorkerRunsOverCap :many
-- On register a worker declares a fresh start, so any run it still holds is
-- orphaned (its execution is gone). Over its re-queue budget → failed. failed →
-- origin restore, applied by the reconcile loop (register does no forge I/O), so
-- it stamps move_pending_since. RETURNING id so the caller can funnel these
-- committed-terminal (worker-lost) runs into the judge (PRD #46 Decision 2), exactly
-- as the sweeper's FailRunsOfStaleWorkersOverCap does.
UPDATE runs SET status = 'failed', status_since = now(), failure_reason = @failure_reason,
    -- PRD #69 M7a: the trusted failure class for an orphaned run whose worker is gone.
    fail_origin = 'worker_lost',
    move_pending_since = now(), finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND requeue_count >= @max_requeues
RETURNING id;

-- name: RequeueWorkerRuns :execrows
-- Within budget → re-queued to this same worker (affinity), which then re-claims
-- and resumes from the persisted session (handles docker compose down && up).
UPDATE runs SET status = 'queued', status_since = now(), requeue_count = requeue_count + 1,
    -- Exit contract (PRD #47 Decision 3): reset on the way back to 'queued'; the
    -- detector re-evaluates the queued signal from this transition's status_since.
    health = 'ok', health_reason = NULL, health_since = NULL,
    -- Issue #783: bank park time before a worker-death requeue -> queued, since started_at
    -- survives the requeue and the later claimed->running resume would not see the park.
    -- awaiting_followup is intentionally excluded: interactive runs are exempt from
    -- SweepRunningTimeout entirely (interactive = false), so they have no wall deadline.
    budget_paused_seconds = budget_paused_seconds
        + CASE WHEN status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int)
               ELSE 0 END,
    updated_at = now()
WHERE worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND requeue_count < @max_requeues;

-- Messages -----------------------------------------------------------------

-- name: InsertRunMessage :execrows
-- Idempotent seq-numbered append: a re-delivered batch (worker retry) is a
-- no-op on the duplicate (run_id, seq).
INSERT INTO run_messages (run_id, seq, kind, agent, agent_instance, agent_label, payload)
VALUES (@run_id, @seq, @kind, @agent, @agent_instance, @agent_label, @payload)
ON CONFLICT (run_id, seq) DO NOTHING;

-- name: ListRunMessagesAfter :many
-- Replay for a (re)connecting browser: everything after its last-seen seq, in
-- order. The persisted log is authoritative; the WS layer (M5) is only a live
-- cache on top of this.
-- Column order matches the run_messages table order (the two PRD #99 columns were
-- appended by 00075), so sqlc keeps returning store.RunMessage rather than
-- minting a separate ...Row type.
-- TO DO IT RIGHT: new columns must be APPENDED to both this SELECT list and
-- ListRunMessagesForWorkerPage's, in the same order the ALTER TABLE adds them.
-- Diverge and sqlc mints per-query Row types for BOTH, breaking workersvc.Store's
-- []store.RunMessage contract (a compile error at cmd/server/main.go).
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label
FROM run_messages
WHERE run_id = @run_id AND seq > @after_seq
ORDER BY seq ASC;

-- name: ListRunMessagesAfterPage :many
-- Bounded twin of ListRunMessagesAfter for the viewer/CLI paging path (issue #160):
-- everything after a seq, in order, but @lim caps the page so a single response
-- can't be unbounded. Authorization (owner-or-admin) is checked by the caller.
-- Column order is IDENTICAL to ListRunMessagesAfter so the row stays
-- store.RunMessage. New columns must be APPENDED here AND in ListRunMessagesAfter,
-- in the same order the ALTER TABLE adds them — see that query's note.
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label
FROM run_messages
WHERE run_id = @run_id AND seq > @after_seq
ORDER BY seq ASC
LIMIT @lim;

-- Usage accounting (PRD #40) ------------------------------------------------

-- name: UpsertRunUsage :exec
-- Fold one model's usage from a delivered result frame into the run's accounting
-- (Decision 2). The result frame's totals are CUMULATIVE-across-resume (M1's
-- Decision 3 verdict b), so the token/cost columns are monotonic per (run_id,
-- session_id, model) and the merge is GREATEST — never a plain overwrite: a
-- crash-retry that re-delivers an EARLIER frame after a LATER one must not regress
-- the row (that is exactly what makes "re-delivering the whole batch changes
-- nothing" true under verdict b, where a stable session_id can otherwise see two
-- different cumulative snapshots hit the same key). The API calls this for every
-- delivered result frame incl. seq-deduped replays, so at-least-once delivery +
-- this idempotent monotonic merge = correct totals with no crash window.
-- lineage_epoch (PRD #632) is a stamped attribute pinned to first insert (omitted
-- from DO UPDATE SET) — the fresh dropped-resume leg's distinct session_id is the
-- row-splitter, and pinning prevents a late re-fold from re-collapsing legs.
INSERT INTO run_usage (
    run_id, session_id, model, lineage_epoch,
    input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, updated_at
) VALUES (
    @run_id, @session_id, @model, @lineage_epoch,
    @input_tokens, @cache_read_tokens, @cache_creation_tokens, @output_tokens, @cost_usd, now()
)
ON CONFLICT (run_id, session_id, model) DO UPDATE SET
    input_tokens          = GREATEST(run_usage.input_tokens,          EXCLUDED.input_tokens),
    cache_read_tokens     = GREATEST(run_usage.cache_read_tokens,     EXCLUDED.cache_read_tokens),
    cache_creation_tokens = GREATEST(run_usage.cache_creation_tokens, EXCLUDED.cache_creation_tokens),
    output_tokens         = GREATEST(run_usage.output_tokens,         EXCLUDED.output_tokens),
    cost_usd              = GREATEST(run_usage.cost_usd,              EXCLUDED.cost_usd),
    updated_at            = now();

-- name: BumpRunLineageEpoch :exec
-- PRD #632: increment a run's lineage-epoch counter by one. The API calls this once
-- per NEWLY-INSERTED resume_lineage_break status event (dropped-resume signal, #334)
-- so a fresh SDK leg's run_usage rows are stamped with a higher epoch than the prior
-- leg's; the run_usage_totals view then SUMs across epochs instead of MAX-masking the
-- smaller leg. Bumping only for events in `inserted` (never re-deliveries) keeps it
-- idempotent under at-least-once delivery.
UPDATE runs SET lineage_epoch = lineage_epoch + 1, updated_at = now() WHERE id = @id;

-- name: GetRunUsageTotal :one
-- One run's rollup totals (PRD #40 M3), for the run-detail usage strip. Reads the
-- run_usage_totals view (greatest-wins per model, summed across models). Returns NO
-- ROW for a run with no usage — the handler maps pgx.ErrNoRows to "no usage" (absent,
-- never a fake 0), so it does not gate detail visibility on ownership here (the
-- caller has already authorized the viewer).
SELECT input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd
FROM run_usage_totals
WHERE run_id = @run_id;

-- name: SelfUsage :one
-- The requesting user's own usage (PRD #40 M3, GET /api/usage): lifetime totals,
-- last-7-days totals, and the count of their runs that carry usage. Windowed on the
-- run's created_at (laptop scale ≈ when it spent). COALESCE(...,0) so a user with no
-- usage gets zeros + run_count 0 (the client reads run_count==0 as "nothing yet").
-- Chat runs are excluded (belt-and-suspenders: the fold never writes chat rows).
WITH scoped AS (
    SELECT r.created_at,
           t.input_tokens, t.cache_read_tokens, t.cache_creation_tokens, t.output_tokens, t.cost_usd
    FROM run_usage_totals t
    JOIN runs r ON r.id = t.run_id
    WHERE r.user_id = @user_id AND r.kind <> 'chat'
)
SELECT
    COALESCE(SUM(input_tokens), 0)::bigint          AS lifetime_input_tokens,
    COALESCE(SUM(cache_read_tokens), 0)::bigint      AS lifetime_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens), 0)::bigint  AS lifetime_cache_creation_tokens,
    COALESCE(SUM(output_tokens), 0)::bigint          AS lifetime_output_tokens,
    COALESCE(SUM(cost_usd), 0)::numeric              AS lifetime_cost_usd,
    COALESCE(SUM(input_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_input_tokens,
    COALESCE(SUM(cache_read_tokens)      FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens)  FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_creation_tokens,
    COALESCE(SUM(output_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_output_tokens,
    COALESCE(SUM(cost_usd)               FILTER (WHERE created_at >= now() - interval '7 days'), 0)::numeric AS last7_cost_usd,
    count(*)::bigint AS run_count
FROM scoped;

-- name: AdminUsageTotals :one
-- Factory-wide totals across ALL users' runs (PRD #40 M3, GET /api/admin/usage).
-- Same shape as SelfUsage without the user filter; by construction this equals the
-- SUM of the AdminUsagePerUser rows (both read run_usage_totals joined to non-chat
-- runs), which the handler test asserts.
WITH scoped AS (
    SELECT r.created_at,
           t.input_tokens, t.cache_read_tokens, t.cache_creation_tokens, t.output_tokens, t.cost_usd
    FROM run_usage_totals t
    JOIN runs r ON r.id = t.run_id
    WHERE r.kind <> 'chat'
)
SELECT
    COALESCE(SUM(input_tokens), 0)::bigint          AS lifetime_input_tokens,
    COALESCE(SUM(cache_read_tokens), 0)::bigint      AS lifetime_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens), 0)::bigint  AS lifetime_cache_creation_tokens,
    COALESCE(SUM(output_tokens), 0)::bigint          AS lifetime_output_tokens,
    COALESCE(SUM(cost_usd), 0)::numeric              AS lifetime_cost_usd,
    COALESCE(SUM(input_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_input_tokens,
    COALESCE(SUM(cache_read_tokens)      FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens)  FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_creation_tokens,
    COALESCE(SUM(output_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_output_tokens,
    COALESCE(SUM(cost_usd)               FILTER (WHERE created_at >= now() - interval '7 days'), 0)::numeric AS last7_cost_usd,
    count(*)::bigint AS run_count,
    -- The earliest usage-bearing run's creation time, for the factory card's "since
    -- <date>" (PRD #40 M6). NULL when the factory has no usage yet.
    MIN(created_at)::timestamptz AS earliest_run
FROM scoped;

-- name: AdminUsagePerUser :many
-- Per-user lifetime usage rows for the admin factory breakdown (PRD #40 M3). One row
-- per user WITH usage; the client computes each user's share against the factory
-- total. Ordered heaviest-cost first (output tokens tiebreak). Sums the same
-- run_usage_totals as AdminUsageTotals, so the rows sum to the factory lifetime total.
SELECT u.id AS user_id, u.email,
    COALESCE(SUM(t.input_tokens), 0)::bigint          AS input_tokens,
    COALESCE(SUM(t.cache_read_tokens), 0)::bigint      AS cache_read_tokens,
    COALESCE(SUM(t.cache_creation_tokens), 0)::bigint  AS cache_creation_tokens,
    COALESCE(SUM(t.output_tokens), 0)::bigint          AS output_tokens,
    COALESCE(SUM(t.cost_usd), 0)::numeric              AS cost_usd,
    count(t.run_id)::bigint AS run_count
FROM run_usage_totals t
JOIN runs r ON r.id = t.run_id
JOIN users u ON u.id = r.user_id
WHERE r.kind <> 'chat'
GROUP BY u.id, u.email
ORDER BY cost_usd DESC, output_tokens DESC, u.id;

-- Worker chat read surface (PRD #39 M3, Decision 7) --------------------------
-- The chat agent investigates its OWNER'S runs (both kinds) via the worker. These
-- queries are USER_ID-scoped (from the authenticated worker), NEVER a bare run_id
-- lookup — a compromised worker still reads only its own user's runs, and a foreign
-- run id simply returns no row (404). repo_web_url rides along so the handler can
-- build the MR URL; a chat run has no repo, so repo fields are NULL (LEFT JOIN).

-- name: ListRunsForWorkerUser :many
-- Compact list of the worker's user's runs, newest first, bounded by @lim. The
-- chat agent's investigation surface (PRD #39 Decision 7). judge runs are hidden
-- (PRD #46, M1-review carry-forward): a judge is a repo-less internal retrospective
-- with no investigable task, same rationale as excluding it from the general run
-- lists (f55b37e). self_improve stays visible — it is real work with a repo + MR.
-- The judge WORKER reads its own run through the M3 judge-scoped trace path, not
-- this chat surface, so hiding judge here does not affect judging.
SELECT r.id, r.kind, r.status, r.issue_iid, r.issue_title, r.branch, r.mr_iid,
       r.failure_reason, r.created_at, r.updated_at,
       rp.path_with_namespace AS repo_path, rp.web_url AS repo_web_url
FROM runs r
LEFT JOIN repos rp ON rp.id = r.repo_id
WHERE r.user_id = @user_id AND r.kind <> 'judge'
ORDER BY r.created_at DESC
LIMIT @lim;

-- name: GetRunForWorkerUser :one
-- One run's detail, scoped to the worker's user (foreign/unknown id -> no row -> 404).
-- judge runs are excluded here too (see ListRunsForWorkerUser): a chat agent asking
-- for a judge run's detail gets a 404, exactly like an unknown id. self_improve is
-- visible.
SELECT r.id, r.kind, r.status, r.issue_iid, r.issue_title, r.branch, r.mr_iid, r.mr_state,
       r.failure_reason, r.stop_kind, r.fix_verdict, r.iteration_count, r.plan_md,
       r.created_at, r.updated_at,
       rp.path_with_namespace AS repo_path, rp.web_url AS repo_web_url
FROM runs r
LEFT JOIN repos rp ON rp.id = r.repo_id
WHERE r.id = @id AND r.user_id = @user_id AND r.kind <> 'judge';

-- name: ListRunMessagesForWorkerPage :many
-- A bounded page of a run's messages after a seq (the worker read tool's paging).
-- Authorization (the run is the worker's user's) is checked by the caller before
-- this; here @lim caps the page so a single response can't be unbounded.
-- Column order matches the table (see ListRunMessagesAfter) so the row stays
-- store.RunMessage. New columns must be APPENDED here AND in ListRunMessagesAfter,
-- in the same order the ALTER TABLE adds them — see that query's note.
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label
FROM run_messages
WHERE run_id = @run_id AND seq > @after_seq
ORDER BY seq ASC
LIMIT @lim;

-- User inputs (steering) ---------------------------------------------------

-- name: CreateRunInput :one
-- Enqueue a plain steering input (approve_plan / follow_up) for the live worker to
-- consume. This path never touches the runs row — no stop signal, no lock — so a
-- follow-up mid-run is a single cheap insert. Deliberate-stop verdicts go through
-- CreateStopVerdictInput instead (they must stamp runs.stop_kind atomically).
INSERT INTO run_user_inputs (run_id, kind, body)
VALUES (@run_id, @kind, @body)
RETURNING *;

-- name: CreateRunAnswerInput :one
-- PRD #88 M1: enqueue an `answer` for the live worker. A plain insert like
-- CreateRunInput — no stop signal, no cap, no runs-row write — but it additionally
-- persists the QUESTION the answer belongs to, in its own column.
--
-- Why a dedicated column rather than reading it out of the JSON body: SetRunRunning's
-- resume guard has to compare it, and `body::jsonb ->> 'question_id'` inside a
-- predicate is unsafe here. body is bare `text` shared with every other kind
-- (follow_up bodies are prose), the planner is free to evaluate the cast before the
-- `kind = 'answer'` filter that would make it well-formed, and an invalid-JSON body
-- then errors the whole statement rather than failing the row. The server has
-- already parsed and validated the body by the time it calls this, so it writes the
-- extracted value directly.
--
-- @question_id is the id the answer was written AGAINST, validated by the caller
-- against the run's currently-open question before this runs.
INSERT INTO run_user_inputs (run_id, kind, body, question_id)
VALUES (@run_id, 'answer', @body, @question_id)
RETURNING *;

-- name: CountRunReviseInputs :one
-- Plan-revision cap (PRD #41): every persisted revise_plan row for the run counts
-- toward PLAN_MAX_REVISIONS, with NO consumed_at filter — a consumed revise still
-- counts, so the cap is the lifetime number of revisions, not the pending backlog.
-- Read-only reporting view of the same count CreateRunReviseInputIfUnderCap enforces.
SELECT count(*) FROM run_user_inputs WHERE run_id = @run_id AND kind = 'revise_plan';

-- name: CreateRunReviseInputIfUnderCap :one
-- Atomic capped enqueue of a revise_plan (PRD #41): insert ONLY while the run is still
-- under its lifetime revision cap, so two concurrent submits (e.g. web + Slack on the
-- same single-owner gate) cannot both read N-1 and both insert an N+1th row (a
-- count-then-insert TOCTOU). NO consumed_at filter — a consumed revise still counts
-- (same lifetime semantics as CountRunReviseInputs). No row returned = the cap is
-- already reached (or the run row is gone); the caller maps that to
-- ErrReviseCapReached.
--
-- 🔴 THE CAP PREDICATE MUST REFERENCE ONLY COLUMNS OF THE `runs` ROW ITSELF.
-- ANY SUBQUERY IN THAT WHERE REINTRODUCES ISSUE #106.
--
-- That is the entire mechanism, so read it before simplifying this back. Under READ
-- COMMITTED a statement's snapshot is taken when the statement starts. When an UPDATE
-- unblocks on a row lock, EvalPlanQual re-evaluates the qual against the NEW VERSION
-- OF THE LOCKED ROW — that is what makes `revise_count < @max_revisions` see the
-- winner's bump — but a subquery inside that same qual still evaluates under the
-- ORIGINAL statement snapshot. Postgres documents the updating command as seeing an
-- inconsistent snapshot: the effects of concurrent commands on the rows it is
-- updating, but not on other rows.
--
-- This query previously did exactly that: it took the `runs` row FOR UPDATE in a
-- leading CTE and counted `run_user_inputs`, a DIFFERENT table, in the INSERT's WHERE.
-- The lock did not cover the count at all, so a caller that blocked still counted N-1
-- and inserted. Measured 100/100 over-cap with the interleave forced (#106), and the
-- over-cap row is DURABLE. Swapping the FOR UPDATE for a real `UPDATE runs ...
-- RETURNING id` does NOT fix it and was measured failing too: the snapshot is the
-- problem, not the lock strength. Only moving the counted fact onto the locked row
-- fixes it.
--
-- 🔴 AND IT MUST NOT SET updated_at, which is a deliberate deviation from the house
-- style of CreateApprovePlanInput / CreateStopVerdictInput. ListActiveRunsForHealth
-- includes awaiting_approval and selects updated_at; healthTargetFor
-- (workersvc/health.go) times the approval_idle flag off it. A revise lands WHILE the
-- run is awaiting_approval, so a bump here would silently move when a health flag
-- fires. A stop verdict drives the run terminal, where healthTargetFor returns
-- healthOK, so ITS bump cannot change a flag outcome; a revise's can.
--
-- runs.revise_count is the cap's source of truth (00092); count(*) of revise_plan rows
-- is the same fact in its other representation, and CountRunReviseInputs still reads
-- that one on purpose — a reporting view that read the counter would assert only that
-- the counter equals itself. TestReviseCountMatchesRowCountLiveDB pins the two
-- together, and its load-bearing case is that a REFUSED insert must move neither: put
-- the cap predicate on the INSERT instead of the UPDATE and the counter runs away
-- while the rows sit at cap, silently shrinking the run's remaining budget on every
-- rejected attempt.
--
-- OWNERSHIP IS ENFORCED ONE CALL UPSTREAM, NOT HERE. The qual is `runs.id = @run_id`
-- with no user_id, so this statement will bump any run's counter it is handed. The
-- tenant check is workersvc.SubmitInput's `GetRun(ctx, userID, runID)`, which resolves
-- the run owner-scoped before reaching this branch — the house pattern, and identical
-- before the #106 fix. Stated so that dropping that GetRun is visibly load-bearing
-- rather than a tidy-up. Safe today additionally because runs.user_id is never updated
-- anywhere, so a resolved run cannot change owner underneath the write.
--
-- @max_revisions crosses from Go as int32 and PLAN_MAX_REVISIONS is unvalidated at the
-- top end; a configured value at or above 2^31 wraps negative and refuses every revise.
-- Fail-closed, and unchanged by the #106 fix — recorded, not guarded.
--
-- Every column reference in the WHERE and RETURNING is qualified with `runs.`, which
-- the plain shape does not require of Postgres but sqlc does: unqualified, its resolver
-- sees `id` in scope from both `runs` and the INSERT's target `run_user_inputs` and
-- fails the whole file with "column reference \"id\" is ambiguous" — attributed,
-- confusingly, to the NEXT query in the file rather than to this one. The SET target is
-- necessarily bare: Postgres rejects a qualified one outright (`UPDATE t SET t.n = ...`
-- gives `column "t" of relation "t" does not exist`), so "every reference in the UPDATE"
-- is not a rule anyone could follow — it was the wording here until it was measured.
WITH bumped AS (
    UPDATE runs SET revise_count = runs.revise_count + 1
    WHERE runs.id = @run_id AND runs.revise_count < @max_revisions::int
    RETURNING runs.id AS run_id
)
INSERT INTO run_user_inputs (run_id, kind, body)
SELECT bumped.run_id, 'revise_plan', @body
FROM bumped
RETURNING *;

-- name: CreateApprovePlanInput :one
-- Enqueue an approve_plan verdict for the live worker AND record the agent
-- selection it carries, in ONE statement (PRD #37, mirroring CreateStopVerdictInput
-- and for the same reason: workersvc.Store exposes no transaction seam, so the
-- combined statement IS the atomicity). A second, non-transactional UPDATE could
-- leave a run whose worker was told to use the repo agents but whose row does not
-- say so — the run view and the MR marker would then lie about what ran.
--
-- The body carries the canonical JSON encoding of the same selection, because the
-- worker reads it from the input, not from the row. Both are written from the
-- server's re-validated value, never from the client's raw text. A resume that
-- re-enters the gate overwrites both columns with the latest approval (Decision 8b).
WITH selected AS (
    UPDATE runs SET
        agent_source     = @agent_source,
        agent_exclusions = @agent_exclusions,
        -- PRD #122 M1: the SERVER-AUTHORITATIVE freeze for the human path. Copy the
        -- approved candidate into the immutable frozen list at approve time, IDEMPOTENTLY
        -- — COALESCE keeps an already-frozen value, so a double-approve (or a re-gate
        -- resume) never changes a list that was frozen once. A run with no candidate
        -- freezes NULL, which is correct: nothing to approve, nothing frozen.
        milestones_frozen = COALESCE(runs.milestones_frozen, runs.milestones_candidate),
        -- PRD #122 M2 (Decision 5/5b): the per-run budget is derived at the SAME freeze,
        -- from the same COALESCE'd frozen source, and written IDEMPOTENTLY via COALESCE —
        -- a double-approve or a re-gate resume never changes a budget frozen once. NULL for
        -- a 0/1-milestone plan (byte-for-byte the global default). See SetRunRunning for the
        -- autopilot mirror of this compute.
        budget_max_iterations = COALESCE(runs.budget_max_iterations,
            CASE WHEN COALESCE(jsonb_array_length(COALESCE(runs.milestones_frozen, runs.milestones_candidate)), 0) <= 1 THEN NULL
                 ELSE sqlc.arg('run_max_iterations')::int * LEAST(jsonb_array_length(COALESCE(runs.milestones_frozen, runs.milestones_candidate)), sqlc.arg('milestone_budget_cap')::int) END),
        budget_wall_seconds = COALESCE(runs.budget_wall_seconds,
            CASE WHEN COALESCE(jsonb_array_length(COALESCE(runs.milestones_frozen, runs.milestones_candidate)), 0) <= 1 THEN NULL
                 ELSE LEAST(sqlc.arg('run_timeout_seconds')::int * LEAST(jsonb_array_length(COALESCE(runs.milestones_frozen, runs.milestones_candidate)), sqlc.arg('milestone_budget_cap')::int), sqlc.arg('budget_wall_ceiling_seconds')::int) END),
        updated_at       = now()
    WHERE id = @run_id
    RETURNING id
)
INSERT INTO run_user_inputs (run_id, kind, body)
VALUES (@run_id, 'approve_plan', @body)
RETURNING *;

-- name: GetRunMilestoneFreezeSnapshot :one
-- Issue #260 instrumentation: read the live milestone freeze state for a run so the
-- approve-time-freeze log (workersvc.submitApproval) can capture what CreateApprovePlanInput
-- saw at the approve instant — the one place the live value is observable, which static
-- analysis could not settle. Read-only; changes nothing.
SELECT id, milestones_frozen, milestones_candidate, updated_at
FROM runs
WHERE id = $1;

-- name: CreateStopVerdictInput :one
-- Enqueue a deliberate-stop verdict (cancel / reject_plan / stop) for the live worker AND
-- stamp runs.stop_kind in the SAME statement (PRD #33 Decision 3): a data-modifying
-- CTE runs to completion exactly once, so the stop signal can never be lost
-- independently of the input that requested it — which a second, non-transactional
-- UPDATE would risk, reintroducing the failed-vs-stopped bug. workersvc.Store exposes
-- no transaction seam, so this single combined statement IS the atomicity.
--
-- FOUR callers now, not two, and one is not a human verdict: PRD #108 M5's auto-stop
-- evaluator enqueues kind='cancel' with stop_kind='auto_stopped' for a run whose message
-- writes are in a confirmed permanent-failure loop, and PRD #517 M4's graceful `stop`
-- enqueues kind='stop' with stop_kind='stopped'. The MECHANISM is unchanged — every caller
-- stamps, so the stamp stays unconditional (no IS NOT NULL guard and thus no
-- parameter-type-inference pitfall). The stamp lands while the run is still non-terminal
-- (awaiting_approval/running); the client's terminal-guarded isStoppedRun ignores it
-- until the run actually reaches failed/cancelled.
--
-- Auto-stop reuses kind='cancel' ON PURPOSE and this is load-bearing rather than
-- convenient: a steering input kind no worker recognises is LOGGED AND DROPPED by
-- SteeringChannel.route's default arm (verbatim at v0.10.0 and at HEAD), and
-- /inputs is consume-on-read, so the drop is PERMANENT and unacknowledgeable. A new
-- kind would therefore be a silent no-op on exactly the older fleet Phase 2 exists
-- to protect — and would need a second migration for run_user_inputs.kind's CHECK.
-- The distinguishing information rides runs.stop_kind, which the worker never sees.
--
-- PRD #503 M3: @stop_reason is stamped UNCONDITIONALLY here (like @stop_kind), but its
-- VALUE is decided by the Go caller and belongs on the CANCEL and STOP paths only. A cancel
-- passes the operator's optional reason (or NULL when none was given); a graceful stop
-- (PRD #517 M4) likewise passes the operator's optional stop reason; reject_plan passes
-- NULL, because a reject's reason goes to failure_reason via the M2 path and double-writing
-- would contradict that clean split; auto-stop passes NULL, its identity being
-- stop_kind='auto_stopped'. The stamp stays unconditional to avoid the
-- parameter-type-inference pitfall the comment above already warns about.
WITH stamped AS (
    UPDATE runs SET stop_kind = @stop_kind, stop_reason = @stop_reason, updated_at = now()
    WHERE id = @run_id
    RETURNING id
)
INSERT INTO run_user_inputs (run_id, kind, body)
VALUES (@run_id, @kind, @body)
RETURNING *;

-- name: CreateScopeCeilingInput :one
-- PRD #634 M2: set runs.scope_ceiling AND write the kind='scope' audit row in ONE
-- statement (mirroring CreateStopVerdictInput's atomicity — workersvc.Store exposes no
-- transaction seam). The COLUMN is the control channel the worker honors on its ACK;
-- the run_user_inputs row is audit/surfacing ONLY (it is excluded from ConsumeRunInputs
-- so the worker never drains or routes it). Last-writer-wins on the column IS the
-- supersede semantic — a later scope write overwrites the ceiling outright.
--
-- PRD #634 M4: before the new audit row is inserted, mark all PRIOR unsettled scope rows
-- for the run as 'superseded'. This is correct because a data-modifying CTE operates on
-- the SNAPSHOT taken at statement start (Postgres MVCC): the superseded UPDATE sees only
-- the scope rows that existed before this statement ran, never the row the main INSERT
-- adds. So after each submit: all prior scope rows = 'superseded', the newest = NULL
-- (pending, settled to applied/declined at completion by SettleScopeInputDisposition).
WITH superseded AS (
    UPDATE run_user_inputs SET disposition = 'superseded'
    WHERE run_id = @run_id AND kind = 'scope' AND disposition IS NULL
),
capped AS (
    UPDATE runs SET scope_ceiling = @scope_ceiling, updated_at = now()
    WHERE id = @run_id
    RETURNING id
)
INSERT INTO run_user_inputs (run_id, kind, body)
VALUES (@run_id, 'scope', @body)
RETURNING *;

-- name: SettleScopeInputDisposition :execrows
-- PRD #634 M4: settle the still-pending scope audit row(s) at completion. After each
-- CreateScopeCeilingInput superseded its priors, exactly ONE scope row is NULL (the last),
-- so this settles that one. Idempotent (WHERE disposition IS NULL) — a second call is a
-- no-op and it never overwrites an already-settled ('superseded'/'applied') row.
UPDATE run_user_inputs SET disposition = @disposition
WHERE run_id = @run_id AND kind = 'scope' AND disposition IS NULL;

-- name: ConsumeRunInputs :many
-- FIFO consume: mark and return every pending input for the run, oldest first.
-- FOR UPDATE SKIP LOCKED keeps two concurrent polls from returning the same row.
WITH pending AS (
    SELECT p.id FROM run_user_inputs p
    -- PRD #634 M2: the scope audit row (kind='scope') is server-side ONLY — the control it
    -- carries travels as runs.scope_ceiling on the ACK/claim, never through this queue — so
    -- the worker must NEVER drain it. Draining would hit SteeringChannel.route's default arm
    -- and log a spurious "unknown input kind". Everything else consumes as before.
    WHERE p.run_id = @run_id AND p.consumed_at IS NULL AND p.kind <> 'scope'
    ORDER BY p.id ASC
    FOR UPDATE SKIP LOCKED
),
consumed AS (
    UPDATE run_user_inputs u SET consumed_at = now()
    FROM pending WHERE u.id = pending.id
    RETURNING u.id, u.kind, u.body, u.created_at
)
SELECT id, kind, body, created_at FROM consumed ORDER BY id ASC;

-- name: ListFollowUpInputsForRun :many
-- The steer queue for a run, NEWEST FIRST and UNCAPPED (PRD #95 Decision 4, #634): the
-- web + CLI steer queue reads BOTH follow_up rows and operator scope directives
-- (kind IN ('follow_up','scope')). A follow_up's state is derived client-side from
-- consumed_at (NULL → Queued, set → Delivered); a scope row is never consumed, so its
-- state is its disposition (applied/declined/superseded, NULL → pending). Deliberately
-- NOT the judge's ListRunInputsForRun (oldest-first, @lim-capped, all kinds) — that
-- would drop the newest entries behind its cap on a busy/chat run. Owner-scoping is
-- enforced at the run resolve (GetRunByIDForUser), not here.
-- The column list is the table's FULL set (question_id, PRD #88, included and always
-- NULL for a follow_up), which is what keeps sqlc returning the shared RunUserInput
-- model instead of minting a query-specific row type. Dropping a column here is not a
-- local edit: it re-types this query and breaks the workersvc.Store interface, the
-- service signature, the handler and its fake.
SELECT id, run_id, kind, body, consumed_at, created_at, question_id, disposition FROM run_user_inputs
WHERE run_id = @run_id AND kind IN ('follow_up', 'scope')
ORDER BY id DESC;

-- Column automation (PRD #12 M1) ------------------------------------------

-- name: GetRunMoveContext :one
-- The run + connection facts the column automation needs to perform a forge-first
-- label move: the run's status/issue/columns, plus the connection to build a
-- client and the numeric project id UpdateIssueLabels requires. GetRunClaimContext
-- (the sibling) deliberately lacks forge_project_id and the column snapshot, so
-- this is its own query. token_ciphertext is decrypted by the service, never
-- selected in the clear.
--
-- It also carries the facts the M5 terminal-comment hook needs from the same read:
-- auto_approve gates the comment to autopilot runs, mr_iid links the success
-- comment, and rp.web_url builds that merge-request link. Both lifecycle observers
-- (the inline notify and the reconcile loop) already load this row, so the terminal
-- comment rides along without a second query.
SELECT r.status, r.issue_iid, r.repo_id, r.origin_column, r.board_column, r.move_pending_since,
       r.auto_approve, r.mr_iid,
       rp.forge_project_id, rp.web_url AS repo_web_url,
       c.forge_type, c.base_url, c.token_ciphertext
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id
WHERE r.id = @run_id;

-- name: GetRunForgeConnForWorker :one
-- The forge connection facts a WORKER-authenticated run needs to build a driver and
-- read the run's forge (PRD #158 M1): the numeric project id plus the connection
-- (forge_type/base_url/token_ciphertext, decrypted by the service, never selected in
-- the clear). Modeled on GetRunMoveContext but stripped to the connection columns and
-- gated on the worker claim — the r.worker_id predicate makes a run the worker does
-- not currently hold return no row, so a cross-tenant read is a 404, not a leak. A
-- repo-less run has no repos row and returns no row too; the service checks repo_id
-- off the owned run FIRST so it can tell that apart and answer 409.
SELECT rp.forge_project_id,
       c.forge_type, c.base_url, c.token_ciphertext, c.bot_forge_user_id
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id
WHERE r.id = @run_id AND r.worker_id = @worker_id;

-- name: ClaimAutopilotTerminalComment :execrows
-- Atomically claim the single terminal issue comment for an autopilot run (PRD #19
-- M5, Decision 6 record-then-comment). Records the marker FIRST; the caller posts
-- the comment only when this returns 1. The auto_approve + IS NULL guard makes it
-- both the autopilot gate and the concurrency claim: a manual run is never claimed,
-- and of the possibly-racing lifecycle invocations (inline notify vs reconcile
-- retry) exactly one gets the row — the rest read 0 and do not re-post. A crash
-- after this commits but before the forge post loses that one comment, never
-- double-posts.
UPDATE runs SET autopilot_commented_at = now()
WHERE id = @id AND auto_approve = true AND autopilot_commented_at IS NULL;

-- name: ListPendingColumnMoves :many
-- Reconcile-loop candidates: runs with a pending column move that is older than a
-- short grace (so the inline move is not raced) and still inside the 30-minute
-- retry window (older markers have been given up on and are deliberately left
-- set). Only the id is returned — the loop re-reads each run's full context
-- (GetRunMoveContext) immediately before the write to narrow the clobber window
-- against a concurrent manual drag.
SELECT id FROM runs
WHERE move_pending_since IS NOT NULL
  AND move_pending_since <= @grace_cutoff
  AND move_pending_since > @giveup_cutoff
ORDER BY move_pending_since ASC
LIMIT @max_batch;

-- name: ListGaveUpColumnMoves :many
-- Runs whose pending marker crossed the 30-minute give-up boundary during the
-- last reconcile interval, for a one-shot warn log. The marker is deliberately
-- NOT cleared (a silent clear would hide the drift behind a correct-looking
-- badge); the next transition or manual drag clears it.
SELECT r.id, r.repo_id, r.issue_iid, r.status, r.move_pending_since
FROM runs r
WHERE r.move_pending_since IS NOT NULL
  AND r.move_pending_since <= @giveup_cutoff
  AND r.move_pending_since > @prior_cutoff
ORDER BY r.move_pending_since ASC
LIMIT 100;

-- name: RecordRunColumnMove :execrows
-- A successful automation move: record the column just applied (board_column) and
-- clear the pending marker in one statement.
UPDATE runs SET board_column = @board_column, move_pending_since = NULL, updated_at = now()
WHERE id = @id;

-- name: ClearRunMovePending :execrows
-- Clear a run's pending marker without recording a column: used when the move is
-- deliberately skipped (manual drag detected, closed issue, unknown baseline).
UPDATE runs SET move_pending_since = NULL, updated_at = now()
WHERE id = @id;

-- name: ClearIssueRunsMovePending :execrows
-- A manual drag heals it: clear the pending marker for every run of this issue so
-- the reconcile loop stops trying to move a card a human just placed.
UPDATE runs SET move_pending_since = NULL, updated_at = now()
WHERE repo_id = @repo_id::uuid AND issue_iid = @issue_iid AND move_pending_since IS NOT NULL;

-- MR-close watcher (PRD #24) --------------------------------------------------

-- name: SetRunMRState :execrows
-- Record the merge-request state the watcher just observed for this run. This is
-- the ONLY writer of runs.mr_state (the watcher-owned invariant, review finding
-- 11): no run-status path writes it. The run itself stays terminal — closing an
-- MR is review feedback, not a run-status event — so this touches mr_state (and
-- updated_at) only.
UPDATE runs SET mr_state = @mr_state, updated_at = now()
WHERE id = @id;

-- Run health detector (PRD #47) ----------------------------------------------

-- name: ListActiveRunsForHealth :many
-- Every run in a flaggable status (queued / running / awaiting_approval), for the
-- sweeper's per-tick health pass. 'claimed' is deliberately excluded (Decision 8:
-- a wedged checkout is already reclaimed by SweepClaimedNeverStarted at ClaimGrace,
-- tighter than any flag). Chat runs are excluded — they legitimately park between
-- turns and have their own idle machinery, so a health flag would be a false alarm.
-- The detector reads current health + reason to skip a no-op write (and its later
-- broadcast) when nothing changed, and health_since so it can PRESERVE the original
-- flag time when only the reason changes within the same enum (a queued run whose
-- reason flips no-worker → waiting must not reset the UI's "stuck for Xm").
-- PRD #122 M2 (Decision 5b): budget_wall_seconds rides this read so the running-run
-- "slow" clamp uses the run's EFFECTIVE timeout, not the global one — a scaled run must
-- not render slow for its whole extended life. NULL for a run on the global default.
-- PRD #84 M3: repo_id, kind and required_capabilities ride this read so the queued
-- arm can surface a capability-specific "no eligible worker" reason (required caps not
-- a subset of any online worker's effective caps). kind was previously only a WHERE
-- filter; it is projected now so the resolver can branch on it too. No sweeper change —
-- a parked run stays queued and every sweep pass is scoped away from it by construction.
SELECT id, user_id, status, auto_approve,
       started_at, last_activity_at, updated_at, status_since,
       health, health_reason, health_since, health_notified_at,
       budget_wall_seconds,
       repo_id, kind, required_capabilities
FROM runs
WHERE status IN ('queued', 'running', 'awaiting_approval')
  AND kind <> 'chat';

-- name: ListRunToolWindow :many
-- The tail of a running run's tool activity for loop + in-flight detection
-- (Decisions 4 & 9), newest first over the existing (run_id, seq) unique index.
-- Both kinds are fetched in ONE query: tool_use rows feed the loop hash, and the
-- newest tool_use vs the tool_result tool_use_ids answers "is a tool call still in
-- flight?" (which suppresses the stalled signal). @lim is set well above the 12-wide
-- loop window so the fetch comfortably contains it plus the interleaved results.
SELECT seq, kind, payload
FROM run_messages
WHERE run_id = @run_id AND kind IN ('tool_use', 'tool_result')
ORDER BY seq DESC
LIMIT @lim;

-- name: SetRunHealth :execrows
-- The detector's single writer of the health columns (Decision 3). Status-scoped
-- (@status is the status the detector read) so it no-ops if the run left that
-- status between the read and this write — that is what closes the sweeper-vs-worker
-- race (the worker's status write already cleared health via the exit contract; this
-- write then matches zero rows). It deliberately does NOT touch updated_at: the
-- queued and approval-idle age clocks read updated_at, so bumping it here would reset
-- the very signal being flagged.
--
-- health_notified_at is the rolling last-nudge stamp (PRD #47 Decision 7): the
-- sweeper passes it non-NULL ONLY when it emits a nudge-worthy event, so the
-- COALESCE advances it then and preserves it otherwise. It is never cleared here
-- (nor by the exit contract), so it damps DM flapping across episodes and API
-- restarts. The detector remains the single writer of every health column.
UPDATE runs SET
    health             = @health,
    health_reason      = sqlc.narg('health_reason'),
    health_since       = sqlc.narg('health_since'),
    health_notified_at = COALESCE(sqlc.narg('health_notified_at'), health_notified_at)
WHERE id = @id AND status = @status;

-- name: CountOnlineWorkersForUser :one
-- How many of a user's workers are online — the queued-run reason resolver uses it
-- to say "no worker is online" vs "waiting for a worker" (Decision 8). Only called
-- for a queued run already past its threshold, so it is off the hot path.
-- draining_since is DELIBERATELY NOT filtered here (PRD #422 Decision 7): a draining
-- worker keeps status='online' and still counts as an online worker for "no worker is
-- online" purposes — do not add a draining predicate.
SELECT count(*) FROM workers WHERE user_id = @user_id AND status = 'online';

-- name: CountOnlineEligibleWorkersForRepo :one
-- How many of a user's ONLINE workers are ELIGIBLE — per fn_worker_can_claim (migration
-- 00113, extended in 00142) — to claim a run on this repo/kind, i.e. pass BOTH the docker
-- allowlist fence AND the capability subset (worker effective caps ⊇ the run's required set),
-- with capability_aware mirroring the claim path's flag so this count and the claim gate can
-- never disagree on ELIGIBILITY. It is deliberately an ELIGIBILITY count, NOT an
-- availability one: it does NOT exclude draining workers (nor busy ones). That is on
-- purpose — its sole consumer is PRD #361's Docker-allowlist rung (reasonRepoNotDockerAllowed),
-- whose job is to isolate the docker-allowlist fence as the blocker. Excluding draining here
-- would MISATTRIBUTE a transient all-draining fleet (during a worker roll — CountOnlineWorkersForUser
-- keeps draining workers online, so the run still reaches that rung) to the allowlist, printing
-- "repo not Docker-allowlisted" when no docker worker is even involved (issue #512 review finding).
-- Availability (draining / free slots) is a SEPARATE axis owned by the generic busy rungs
-- downstream (CountOnlineWorkersWithFreeSlotForUser, which DOES exclude draining), so a
-- 0 here means the allowlist+capability fence genuinely blocks every online worker regardless
-- of when they next free up — a persistent condition, unlike a roll that clears itself.
-- Params cast EXACTLY as ClaimRun passes them so a green sqlc generate is not mistaken for a
-- query Postgres will accept. The fence-BLIND capability-gap discriminator ("does the fleet
-- HAVE these caps at all") is a separate concern handled upstream by
-- CountOnlineWorkersSatisfyingCaps. Its sole caller is the queued-reason resolver, only for a
-- run already past its health threshold, so it is off the hot path.
SELECT count(*) FROM workers w
WHERE w.user_id = @user_id
  AND w.status = 'online'
  AND fn_worker_can_claim(
        COALESCE(w.docker_enabled, false),
        @docker_repo_allowlist::uuid[],
        @repo_id::uuid,
        @kind::text,
        COALESCE(w.capabilities, '{}')::text[],
        @required_capabilities::text[],
        @capability_aware::boolean);

-- name: ListDockerBlockedReposForUser :many
-- The caller's repo ids that a Docker-allowlist gap is ACTIVELY blocking (PRD #361 M3):
-- an enabled repo with ≥1 of the caller's QUEUED runs, for which the caller has ≥1 online
-- worker but ZERO online workers eligible to claim a repo-bearing run on it — i.e. every
-- online worker is a Docker worker and the repo is not on the docker allowlist. Reuses the
-- fn_worker_can_claim eligibility notion (migration 00113, extended in 00142); for a
-- repo-bearing run the kind is irrelevant (the judge exemption needs repo_id IS NULL), so
-- eligibility is per repo and the kind arg is a placeholder. capability_aware is passed
-- FALSE (worker_caps/required_capabilities empty) so this stays the pure docker→allowlist
-- eligibility notion. The "≥1 online AND zero eligible" pair already implies the
-- repo is not allowlisted (an allowlisted repo makes every worker eligible), so no separate
-- allowlist clause is needed. Requiring ≥1 online worker keeps this distinct from a
-- no-worker-online block (mirrors the M2 queued reason). Drives the Setup chip's info
-- escalation, computed from eligibility directly — independent of the sweeper's
-- health_reason text and health_enabled/threshold gating.
SELECT r.id
FROM repos r
JOIN forge_connections fc ON fc.id = r.connection_id
WHERE fc.user_id = @user_id
  AND r.enabled = true
  AND EXISTS (
    SELECT 1 FROM runs run
    WHERE run.repo_id = r.id AND run.user_id = @user_id AND run.status = 'queued'
  )
  AND EXISTS (
    SELECT 1 FROM workers w
    WHERE w.user_id = @user_id AND w.status = 'online'
  )
  AND NOT EXISTS (
    SELECT 1 FROM workers w
    WHERE w.user_id = @user_id AND w.status = 'online'
      AND fn_worker_can_claim(COALESCE(w.docker_enabled, false), @docker_repo_allowlist::uuid[], r.id, 'task'::text, '{}'::text[], '{}'::text[], false)
  );

-- name: CountOnlineWorkersWithFreeSlotForUser :one
-- How many of a user's ONLINE workers plausibly have room for another run — the
-- queued-run reason resolver (PRD #216) uses it to tell a SATURATED fleet (every
-- online worker at its advertised run-lane cap, so a fleet-aware claim may be
-- deferring this run to a peer that is itself full) from a fleet with an idle
-- worker that simply has not claimed yet. A NULL cap advertises no bound, so such a
-- worker is treated as always having room. Active count uses the SAME run-lane
-- definition as ListWorkersByUser.active_runs (status claimed/running/
-- awaiting_approval/awaiting_input/awaiting_followup, kind <> 'chat'). Only called for a queued run
-- already past its health threshold, so it is off the hot path.
SELECT count(*) FROM workers w
WHERE w.user_id = @user_id
  AND w.status = 'online'
  -- A draining worker has no free slot for NEW work (it claims nothing), so the
  -- queued-run reason resolver must not count it as an idle worker (PRD #422 Decision 7).
  AND w.draining_since IS NULL
  AND (w.max_concurrent_runs IS NULL
       OR (SELECT count(*) FROM runs r
            WHERE r.worker_id = w.id
              AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
              AND r.kind <> 'chat') < w.max_concurrent_runs);

-- name: CountOnlineWorkersSatisfyingCaps :one
-- How many of a user's ONLINE, non-draining workers have EFFECTIVE caps
-- (capabilities ∪ {docker if docker_enabled}) that are a SUPERSET of a given required set,
-- DELIBERATELY IGNORING the docker repo-allowlist fence. This is the capability-GAP
-- discriminator — it answers "does the fleet HAVE these caps at ALL?", NOT "can this run be
-- claimed right now?" — and drives reasonNoEligibleWorker (PRD #84 M3): a 0 here means a
-- capability nothing in the fleet has, distinct from the generic wait. It is EXPLICITLY NOT
-- a claim-time count: it does not apply the docker allowlist fence, so a worker it counts
-- may still be barred from THIS repo. That fence is applied separately by the
-- reasonRepoNotDockerAllowed rung via CountOnlineEligibleWorkersForRepo, which is the true
-- claim-time count. The effective-caps fold is the shared fn_effective_worker_caps
-- (single source since #512 M5, migration 00151) — capabilities plus `docker` when
-- docker_enabled — the SAME function fn_worker_can_claim applies at claim time.
-- draining_since IS NULL mirrors CountOnlineWorkersWithFreeSlotForUser: a draining worker
-- claims nothing, so it cannot be the eligible worker. Only called for a queued run already
-- past its health threshold, so it is off the hot path.
--
-- AND NOT w.ephemeral (PRD #529 M2, Correction B): an ephemeral worker is bound to
-- ONE run (it can claim only its ephemeral_run_id), so it can never satisfy a
-- DIFFERENT run and must not be counted as a general satisfier here — otherwise a
-- run-bound docker worker would make a second docker run read as placeable and its
-- "no eligible worker" reason would wrongly go silent. This is the display detector;
-- the exclusion stays a simple NOT ephemeral (no run_id threading) because the count
-- is only ever "who could run THIS if it were free", and a bound worker never could.
SELECT count(*) FROM workers w
WHERE w.user_id = @user_id
  AND w.status = 'online'
  AND w.draining_since IS NULL
  AND NOT w.ephemeral
  AND @required_capabilities::text[] <@ fn_effective_worker_caps(w.capabilities, COALESCE(w.docker_enabled, false));

-- name: ListUnplaceableQueuedRunsForEphemeral :many
-- The trigger query for the ephemeral auto-provisioner (PRD #529 M2). It returns the
-- queued, non-chat runs for which the api should spin a run-bound ephemeral worker:
-- runs that NOTHING online can satisfy, belonging to a user who opted in, and not
-- already served by an ephemeral worker.
--
-- Each predicate, and why it is here:
--   * u.ephemeral_workers_enabled — the per-user opt-in (users column). The JOIN also
--     drops runs whose owner row is gone. The instance kill-switch is checked in Go
--     before this query runs, so a flag-off pass never reaches here.
--   * r.status = 'queued' AND r.kind <> 'chat' — the pre-claim trigger (Path 1): a run
--     nothing has claimed yet, excluding the chat lane (which never carries capability
--     requirements and is served by ClaimChatRun).
--   * cardinality(r.required_capabilities) > 0 — a run with no capability requirement is
--     never "unplaceable for a capability", so it is not our concern (mirrors
--     health.go's len(RequiredCapabilities) > 0 guard on the display reason).
--   * NOT EXISTS (an online, non-draining, NON-ephemeral worker of the user whose
--     EFFECTIVE caps are a superset of the run's) — the SAME effective-caps fold as
--     CountOnlineWorkersSatisfyingCaps / fn_worker_can_claim (capabilities plus `docker`
--     when docker_enabled), so a run this returns is exactly one no ordinary online
--     worker can claim. `AND NOT w.ephemeral` because a run-bound ephemeral worker
--     serves only its own run and can never satisfy this one.
--   * NOT EXISTS (an ephemeral worker already bound to this run) — the one-per-run skip.
--     The partial UNIQUE index uq_workers_ephemeral_run is the hard guarantee; this
--     predicate is the cheap pre-filter so the common steady state does not churn the
--     provision tx just to hit a 23505 every tick.
--   * (SELECT count(...)) < @max_per_user — cross-user FAIRNESS: exclude runs whose owner
--     is ALREADY at/over the per-user ephemeral cap, so a single opted-in user with ≥
--     @max_rows unplaceable runs cannot monopolize every batch (all @max_rows rejected on
--     the cap, zero progress, recurring forever) and starve other users. The count matches
--     CountEphemeralHostedWorkersForUser exactly (kind = 'hosted' AND ephemeral) so the
--     filter and the authoritative cap agree on what "at cap" means.
--
--     🔴 THIS FILTER IS A FAIRNESS OPTIMIZATION ONLY, NEVER THE CAP. It is an UNLOCKED
--     snapshot read, so it is a TOCTOU by construction — the AUTHORITATIVE cap is still
--     the advisory-locked CountEphemeralHostedWorkersForUser check in provisionOne. A race
--     here can therefore never over-provision: the worst it can do is, harmlessly, let an
--     about-to-be-capped user's run into the batch, where provisionOne then rejects it
--     under the lock. It can also transiently exclude a run whose owner has just dropped
--     below the cap; the next tick surfaces it, so no run is lost.
--
-- ORDER BY r.created_at ASC so the oldest waiting run is provisioned first; LIMIT
-- @max_rows bounds the work per tick.
SELECT r.id, r.user_id, r.required_capabilities
FROM runs r
JOIN users u ON u.id = r.user_id AND u.ephemeral_workers_enabled
WHERE r.status = 'queued'
  AND r.kind <> 'chat'
  AND cardinality(r.required_capabilities) > 0
  AND NOT EXISTS (
      SELECT 1 FROM workers w
      WHERE w.user_id = r.user_id
        AND w.status = 'online'
        AND w.draining_since IS NULL
        AND NOT w.ephemeral
        AND r.required_capabilities <@ (COALESCE(w.capabilities, '{}') || CASE WHEN COALESCE(w.docker_enabled, false) THEN ARRAY['docker'] ELSE ARRAY[]::text[] END)
  )
  AND NOT EXISTS (
      SELECT 1 FROM workers w2
      WHERE w2.ephemeral AND w2.ephemeral_run_id = r.id
  )
  AND (SELECT count(*) FROM workers wc
       WHERE wc.user_id = r.user_id AND wc.kind = 'hosted' AND wc.ephemeral) < @max_per_user::int
ORDER BY r.created_at ASC
LIMIT @max_rows;

-- name: ListSaturationQueuedRunsForEphemeral :many
-- The SATURATION sibling of ListUnplaceableQueuedRunsForEphemeral (issue #747 M1).
-- Where the capability-gap query above fires for runs NOTHING online can satisfy, THIS
-- query fires for the opposite shape: runs that ARE capability-placeable (a capable
-- worker exists) but are slot-BLOCKED because every capable worker is at its run-lane
-- cap — the fleet is saturated for this run. It returns the same columns and drives the
-- same run-bound ephemeral auto-provisioner (PRD #529), for a user who opted in and is
-- not already served by an ephemeral worker.
--
-- Each predicate, and why it is here:
--   * u.ephemeral_workers_enabled — the per-user opt-in (users column). The JOIN also
--     drops runs whose owner row is gone. The instance kill-switch is checked in Go
--     before this query runs, so a flag-off pass never reaches here.
--   * r.status = 'queued' AND r.kind <> 'chat' — the pre-claim trigger: a run nothing has
--     claimed yet, excluding the chat lane (served by ClaimChatRun).
--   * now() - r.status_since > @saturation_delay::interval — the DEBOUNCE. We gate on
--     time-spent-in-queued using runs.status_since (migration 00163), NOT created_at, so a
--     run that has only just been re-queued does not immediately trip a provision: a
--     transiently-full fleet is given @saturation_delay to free a slot on its own before we
--     spend an ephemeral worker on it.
--   * DELIBERATELY NO cardinality(r.required_capabilities) > 0 GUARD (Decision 2). Unlike
--     the capability-gap sibling, a plain zero-capability run MUST qualify here: a saturated
--     fleet blocks a zero-cap run just as it blocks a cap-carrying one. This needs no special
--     case — an empty required set is a subset of EVERY worker's effective caps, so
--     `r.required_capabilities <@ fn_effective_worker_caps(...)` is trivially true and the
--     "a capable worker exists" test below degenerates to "any online non-draining
--     non-ephemeral worker exists", which is exactly right.
--   * EXISTS (≥1 CAPABLE worker) — an online, non-draining, NON-ephemeral worker of the
--     user whose EFFECTIVE caps are a superset of the run's, using the SAME
--     fn_effective_worker_caps fold as CountOnlineWorkersSatisfyingCaps / fn_worker_can_claim
--     (capabilities plus `docker` when docker_enabled). This is what makes the run
--     capability-PLACEABLE and distinguishes this path from the capability-gap sibling.
--     `AND NOT w.ephemeral` because a run-bound ephemeral worker serves only its own run.
--   * NOT EXISTS (a capable worker with a FREE slot) — the SATURATION test. Over the SAME
--     capable-worker set, none has room. The free-slot definition is reused VERBATIM from
--     CountOnlineWorkersWithFreeSlotForUser: the five-status active-run set
--     (claimed/running/awaiting_approval/awaiting_input/awaiting_followup, kind <> 'chat')
--     and `max_concurrent_runs IS NULL OR (active count) < max_concurrent_runs`. A NULL-cap
--     worker advertises UNBOUNDED room, so it always HAS a free slot → the NOT EXISTS is
--     false → the run is NOT slot-blocked and is correctly excluded (that worker will claim
--     it; provisioning an ephemeral would be wrong).
--   * NOT EXISTS (an ephemeral worker already bound to this run) — the one-per-run skip.
--     The partial UNIQUE index uq_workers_ephemeral_run is the hard guarantee; this is the
--     cheap pre-filter so the steady state does not churn the provision tx each tick.
--   * (SELECT count(...)) < @max_per_user — cross-user FAIRNESS: exclude runs whose owner is
--     ALREADY at/over the per-user ephemeral cap (kind = 'hosted' AND ephemeral, matching
--     CountEphemeralHostedWorkersForUser), so one user cannot monopolize every batch and
--     starve others.
--
--     🔴 THIS FILTER IS A FAIRNESS OPTIMIZATION ONLY, NEVER THE CAP. It is an UNLOCKED
--     snapshot read, so it is a TOCTOU by construction — the AUTHORITATIVE cap is still the
--     advisory-locked CountEphemeralHostedWorkersForUser check in provisionOne. A race here
--     can therefore never over-provision: the worst it can do is, harmlessly, let an
--     about-to-be-capped user's run into the batch, where provisionOne then rejects it under
--     the lock. It can also transiently exclude a run whose owner has just dropped below the
--     cap; the next tick surfaces it, so no run is lost.
--
-- ORDER BY r.status_since ASC so the longest-waiting run is provisioned first; note the
-- sibling orders by created_at, but THIS path's clock is status_since (the same column the
-- debounce gates on), so we order by it for consistency. LIMIT @max_rows bounds the work
-- per tick.
SELECT r.id, r.user_id, r.required_capabilities
FROM runs r
JOIN users u ON u.id = r.user_id AND u.ephemeral_workers_enabled
WHERE r.status = 'queued'
  AND r.kind <> 'chat'
  AND now() - r.status_since > @saturation_delay::interval
  AND EXISTS (
      SELECT 1 FROM workers w
      WHERE w.user_id = r.user_id
        AND w.status = 'online'
        AND w.draining_since IS NULL
        AND NOT w.ephemeral
        AND r.required_capabilities <@ fn_effective_worker_caps(w.capabilities, COALESCE(w.docker_enabled, false))
  )
  AND NOT EXISTS (
      SELECT 1 FROM workers w
      WHERE w.user_id = r.user_id
        AND w.status = 'online'
        AND w.draining_since IS NULL
        AND NOT w.ephemeral
        AND r.required_capabilities <@ fn_effective_worker_caps(w.capabilities, COALESCE(w.docker_enabled, false))
        AND (w.max_concurrent_runs IS NULL
             OR (SELECT count(*) FROM runs r2
                  WHERE r2.worker_id = w.id
                    AND r2.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
                    AND r2.kind <> 'chat') < w.max_concurrent_runs)
  )
  AND NOT EXISTS (
      SELECT 1 FROM workers w2
      WHERE w2.ephemeral AND w2.ephemeral_run_id = r.id
  )
  AND (SELECT count(*) FROM workers wc
       WHERE wc.user_id = r.user_id AND wc.kind = 'hosted' AND wc.ephemeral) < @max_per_user::int
ORDER BY r.status_since ASC
LIMIT @max_rows;

-- name: RunHasVerdictSinceGateOpened :one
-- Has the owner already answered THIS approval gate, with the worker yet to act on it
-- (issue #182)? healthTargetFor's awaiting_approval arm asks before flagging
-- approval_idle, and reports waiting_worker instead when the answer is true: a run whose
-- human has responded is waiting on its WORKER, not on its owner. Before this existed the
-- arm timed purely off updated_at, so a user who requested changes at t+50m was still
-- nudged at t+60m to approve a plan they had already responded to.
--
-- @gate_opened_at is runs.status_since AS THE DETECTOR READ IT (issue #190; the SQL param
-- name is unchanged, only the Go call site's source column moved off updated_at).
-- SetRunAwaitingApproval sets status, status_since, plan_md, the health columns and
-- updated_at together, so status_since is both the age clock the arm's threshold guard uses
-- AND this episode's boundary. Passing the value the detector already holds — rather than
-- re-reading runs here — keeps this predicate on the same snapshot as that guard, so the two
-- cannot disagree about which episode they are describing.
--
-- 🔴 NO FLAPPING, AND SINCE #190 BECAUSE THE PREDICATE IS MONOTONE WITHIN AN EPISODE. It
-- was once documented as "monotone within an episode by construction", which was FALSE
-- while the boundary was updated_at: FIVE statements bump runs.updated_at without moving
-- the run out of awaiting_approval — SetRunWaitOnLimit (user-reachable at
-- PUT /api/runs/{id}/wait-on-limit), ClearIssueRunsMovePending (a card drag),
-- RecordRunColumnMove, ClearRunMovePending and SetRunMRState (four of the five carry no
-- status guard at all) — so the boundary could advance mid-episode and the predicate go
-- true → false. Issue #190 moves the boundary onto status_since, which NONE of those five
-- touch (status_since is stamped only by a statement that changes runs.status), so the
-- boundary no longer moves inside a gate episode and the predicate is genuinely monotone.
--
-- Before #190 the no-flapping conclusion rested on a weaker mechanism worth remembering:
-- updated_at was ALSO the arm's threshold clock, so any bump made
-- olderThan(now, updated_at, th.approval) false FIRST and the next tick returned healthOK
-- before this lookup ran at all. #190 points BOTH the boundary here and that threshold
-- clock (health.go's queued/approval arms) at status_since, so the flap is closed at its
-- source rather than masked. Whoever re-points either back onto a column that incidental
-- writers bump re-opens the flap this paragraph says is closed.
--
-- 🔴 `>=`, NOT `>`, IS THE CORRECT BOUNDARY. Under the old updated_at boundary this was
-- strictly load-bearing: CreateApprovePlanInput and CreateStopVerdictInput both
-- `SET updated_at = now()` in the SAME statement that inserts the row, and now() is the
-- transaction timestamp — so created_at and the boundary came out EXACTLY EQUAL, and `>`
-- would have reported "waiting for the plan to be approved" an hour after the owner acted.
-- Under status_since (issue #190) those inserts do NOT move the boundary — only a status
-- transition does — so a verdict's created_at is strictly after the gate opened; `>=` is
-- retained as the defensive boundary, still admitting the equal case should a future status
-- write ever share a verdict's transaction timestamp.
--
-- 🔴 FOUR OF THE SIX LEGAL KINDS (run_user_inputs_kind_check, widened by 00074 and 00092),
-- AND BOTH OMISSIONS ARE DELIBERATE. Read this before "fixing" the list against that CHECK.
-- The rule outlives the list: include exactly the kinds the worker's route()
-- (agent/src/steering.ts) turns into a gate event or a cancel.
--
--   follow_up is EXCLUDED because route() pushes it onto a buffer that never reaches
--   serviceGate. It is a message, not an answer: the gate stays parked and the run IS still
--   waiting on its human. Including it would be worse than a mistimed flag — a follow_up
--   row never ages out of this predicate, so once true it stays true for as long as the
--   gate stays open (nothing clears it but re-opening the gate, which requires someone to
--   answer). A chatty owner would silently disable their own approval nudge indefinitely,
--   through the normal UI. The incidental updated_at bumps described above do not rescue
--   this: since #190 they touch neither this arm's clock nor its boundary (both status_since)
--   at all, and even before #190 they only suppressed the flag via the threshold clock rather
--   than falsifying the predicate — either way the follow_up row keeps the predicate true
--   once set.
--
--   answer is EXCLUDED as structurally unreachable here: submitAnswer
--   (internal/workersvc/service.go) refuses unless the run is 'awaiting_input', and every
--   path into 'awaiting_approval' stamps status_since — so an answer from an earlier park is
--   strictly older than this gate opened and fails the created_at test regardless.
--
-- ACCEPTED RESIDUAL, recorded rather than coded around: an EMPTY-BODY revise_plan flags
-- waiting_worker on a run that is genuinely still waiting for a human. internal/handler's
-- worker input route validates the kind and passes the body through unchecked, while
-- route() drops an empty body without servicing the gate. It already burns a revision-cap
-- slot, reaching it needs a hand-crafted API call, and a kind-specific emptiness clause
-- would put body parsing inside a predicate whose whole value is that it reads one
-- timestamp and one kind.
--
-- NO INDEX AND NO MIGRATION, deliberately. run_user_inputs' only index is
-- idx_run_user_inputs_pending ON (run_id, id) WHERE consumed_at IS NULL (00020) — PARTIAL
-- on pending rows, which this predicate does not filter on, so it cannot be used and this
-- is a sequential scan. That is affordable only because it runs BEHIND the arm's three
-- existing guards (!auto_approve, threshold enabled, past the threshold), i.e. for
-- approximately zero runs per tick. As a projection on ListActiveRunsForHealth it would be
-- one such scan per active run per 15s tick and an index would become mandatory.
--
-- The EXISTS is CAST because sqlc's inference is weaker on expressions than on columns:
-- uncast, the projection arrives in Go as interface{} rather than bool.
SELECT (EXISTS (
    SELECT 1 FROM run_user_inputs
    WHERE run_id = @run_id
      AND kind IN ('approve_plan', 'reject_plan', 'cancel', 'revise_plan')
      AND created_at >= @gate_opened_at
))::boolean AS has_verdict;

-- name: SetRunWaitOnLimit :execrows
-- Flip ONE run's usage-limit opt-in after the fact (PRD #35 Decision 7, the per-run
-- surface the user ruled for on 2026-07-27 in place of a start-run modal).
--
-- Owner-scoped: a run is toggled by the person whose credentials it spends, and the
-- predicate is the write's own authorization rather than a fact maintained
-- elsewhere. A foreign run returns 0 rows, which the handler maps to 404 — never
-- 403, which would confirm the run exists.
--
-- The status guard is CancelRunServerSide's, deliberately reused verbatim: negative,
-- so it covers limit_wait for free and needs no edit for any future non-terminal
-- status. A terminal run is a no-op rather than an error — the toggle changes FUTURE
-- limit behaviour, and a finished run has none.
--
-- 🔴 IT DOES NOT TOUCH status, AND MUST NOT. Flipping the flag OFF on a parked run
-- does not un-park it: Decision 11's cancel is that control, and silently failing a
-- user's run because they changed a preference would destroy work they never asked
-- to lose. Flipping it ON while parked is likewise inert — the run is already
-- parked. The flag is read at the NEXT limit event and at the next claim (the worker
-- re-reads it from the row every time), which is what makes a mid-flight change take
-- effect without this statement needing to reach into the state machine.
UPDATE runs SET wait_on_limit = @wait_on_limit, updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRunMrReworkEnabled :execrows
-- Flip ONE run's MR-rework override after the fact (PRD #841 M1, Decision D2), the
-- per-run surface for the MR review watcher. Owner-scoped exactly like SetRunWaitOnLimit:
-- a foreign run returns 0 rows, which the handler maps to 404 (never 403, which would
-- confirm the run exists). @mr_rework_enabled is a NULLABLE bool (the column is nullable,
-- default-ON via COALESCE(run, owner) IS NOT FALSE): passing NULL clears the override
-- back to inherit, and false/true set an explicit override.
--
-- 🔴 NO STATUS GUARD, and MUST NOT have one (D2). Unlike wait_on_limit — which governs
-- an IN-FLIGHT run, so SetRunWaitOnLimit guards status NOT IN ('completed','failed',
-- 'cancelled') — the MR-rework watcher acts AFTER the run completes, during Human Review
-- while its MR still has open comments. A terminal-status guard would lock the toggle
-- exactly when it matters. No explicit terminal guard for a merged/closed MR is needed
-- either: the write is inert once the MR is no longer open, because ListMRReworkCandidates
-- already excludes any run whose MR has left the opened state.
UPDATE runs SET mr_rework_enabled = @mr_rework_enabled, updated_at = now()
WHERE id = @id AND user_id = @user_id;

-- name: SetRunPriority :execrows
-- Expedite/undo one queued run's manual priority override (PRD #320 D6/D7). Owner-
-- scoped: a foreign run returns 0 rows -> handler 404 (never 403). QUEUED-ONLY:
-- ordering only matters before a run is claimed, so a non-queued run returns 0 rows
-- -> handler 409. Sets ONLY priority (expedite=2, undo=NULL); it deliberately does
-- NOT touch status, and it does NOT bump updated_at, because the #216 fleet-spread
-- and the resume-affinity grace are both keyed on updated_at — bumping it would
-- reset those age clocks and could re-defer the very run being expedited. The D4
-- fail-open is keyed on created_at, so priority is orthogonal to the age clocks.
UPDATE runs SET priority = sqlc.narg('priority')::smallint
WHERE id = @id AND user_id = @user_id AND status = 'queued';
