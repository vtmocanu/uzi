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
             AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
       ) AS busy,
       (
           SELECT count(*) FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
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
        max_concurrent_runs = sqlc.narg('max_concurrent_runs'),
        -- online_since is the api-owned uptime anchor (PRD #251 M1): PRESERVE it if the
        -- worker is already online with one, else STAMP now() — so a steady stream of
        -- registers never moves it and the first register after an offline gap (or for a
        -- brand-new worker) starts a fresh session. Postgres evaluates the SET RHS against
        -- the OLD row, so workers.status/online_since here read the pre-update tuple (the
        -- same mechanism SetRunRunning's `health = CASE WHEN status='running'` relies on).
        online_since        = CASE WHEN workers.status = 'online' AND workers.online_since IS NOT NULL THEN workers.online_since ELSE now() END,
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
INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, origin_column, move_pending_since, auto_approve, wait_on_limit, plan_md, plan_source, agent_source, agent_exclusions, planned_base_commit, require_base_match)
VALUES (@user_id, @repo_id::uuid, @issue_iid, @issue_title, @issue_description, sqlc.narg('origin_column'), now(), @auto_approve, @wait_on_limit, sqlc.narg('plan_md'), @plan_source, sqlc.narg('agent_source'), sqlc.narg('agent_exclusions')::jsonb, sqlc.narg('planned_base_commit'), @require_base_match)
RETURNING *;

-- name: GetRunByIDForUser :one
SELECT * FROM runs WHERE id = @id AND user_id = @user_id;

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
       rv.verdict                AS judge_verdict,
       ru.input_tokens          AS usage_input_tokens,
       ru.cache_read_tokens      AS usage_cache_read_tokens,
       ru.cache_creation_tokens  AS usage_cache_creation_tokens,
       ru.output_tokens          AS usage_output_tokens,
       ru.cost_usd               AS usage_cost_usd
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id   -- forge_type for the per-run MR/PR noun (PRD #65 D2); every repo has a connection
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

-- name: ListActiveRunsAll :many
-- Admin Agents-status: every non-terminal run across all users, with repo path,
-- worker name, and owner email for the admin overview.
SELECT sqlc.embed(r), rp.path_with_namespace AS repo_path, w.name AS worker_name, u.email AS owner_email,
       c.forge_type
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id   -- forge_type for the per-run MR/PR noun (PRD #65 D2)
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
             AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
       ) AS busy,
       (
           SELECT count(*) FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
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
-- blocking (multica's queue semantics). The kind<>'chat' predicate is what keeps
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
      AND r.status = 'queued'
      AND (r.worker_id IS NULL
           OR r.worker_id = @worker_id
           OR r.updated_at < @affinity_cutoff)
      -- PRD #216 D5: claiming-worker eligibility via the shared expression.
      AND fn_worker_can_claim(@is_docker_worker::boolean, @docker_repo_allowlist::uuid[], r.repo_id, r.kind)
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
                    AND pr.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
                    AND pr.kind <> 'chat'
              ) pa
              WHERE p.user_id = @user_id
                AND p.id <> @worker_id
                AND p.last_heartbeat_at IS NOT NULL
                AND p.last_heartbeat_at >= @heartbeat_cutoff
                AND p.max_concurrent_runs IS NOT NULL
                AND fn_worker_can_claim(COALESCE(p.docker_enabled, false), @docker_repo_allowlist::uuid[], r.repo_id, r.kind)
                AND pa.active < p.max_concurrent_runs
                AND pa.active * (SELECT w.max_concurrent_runs FROM workers w WHERE w.id = @worker_id)
                    < (SELECT count(*) FROM runs mr
                        WHERE mr.worker_id = @worker_id
                          AND mr.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
                          AND mr.kind <> 'chat')
                      * p.max_concurrent_runs
          )
      )
    ORDER BY COALESCE(r.worker_id = @worker_id, false) DESC, r.created_at ASC
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
       rp.default_branch,
       rp.repo_skills_enabled,
       rp.repo_claudemd_enabled,
       rp.repo_devbox_opt_in,
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
          AND run_user_inputs.question_id = runs.open_question_id));

-- name: SetRunAwaitingApproval :execrows
UPDATE runs SET
    status     = 'awaiting_approval',
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
    -- this transition's fresh updated_at; health_notified_at is preserved.
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
  AND status <> 'limit_wait';

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
-- queued signal from this transition's fresh updated_at rather than inheriting
-- anything.
--
-- RETURNING id, user_id, status matches every other sweep transition in this file,
-- so the caller can publish each promotion through the broadcaster/notifier
-- fan-out; a promotion to 'queued' moves the board card to In Progress exactly like
-- a requeue.
UPDATE runs SET
    status     = 'queued',
    started_at = NULL,
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
    open_question_id = @open_question_id,
    session_id       = COALESCE(sqlc.narg('session_id'), session_id),
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
    move_pending_since = now(),
    finished_at        = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRunFailed :execrows
-- failed restores the origin column → move_pending_since stamped in the same
-- statement (same-tx crash-window closure, as for completed).
UPDATE runs SET
    status             = 'failed',
    failure_reason     = @failure_reason,
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
    failure_reason     = @failure_reason,
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
UPDATE runs SET status = 'cancelled', stop_kind = 'cancelled', move_pending_since = now(), finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
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
UPDATE runs SET status = 'failed',
    failure_reason     = @failure_reason,
    stop_kind          = 'auto_stopped',
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
  AND status <> 'awaiting_input';

-- name: RejectRunServerSide :execrows
-- Server-side plan rejection → failed → origin restore → stamp. stop_kind is
-- stamped 'plan_rejected' in the same statement as the status/failure_reason write
-- (PRD #33 Decision 3), so this failed run is recognised as a deliberate stop
-- regardless of the failure_reason text.
UPDATE runs SET status = 'failed', stop_kind = 'plan_rejected', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
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
UPDATE runs SET status = 'queued',
    -- Exit contract (PRD #47 Decision 3): reset on the way to a fresh 'queued'; the
    -- detector re-evaluates the queued signal from this transition's updated_at.
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
UPDATE runs SET status = 'queued',
    -- Exit contract (PRD #47 Decision 3): mirrors SweepClaimedNeverStarted — a
    -- 'claimed' run never carries a flag, so this is defensive, but it keeps every
    -- claimed→queued path uniform.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND status = 'claimed';

-- name: SweepRunningTimeout :many
-- running past RUN_TIMEOUT → failed (a hung agent is failed without a human).
-- Stamps move_pending_since so the (forge-free) sweep leaves the isolated
-- reconcile loop a marker to restore the origin column later. Chat runs are exempt
-- (Decision 3): a chat legitimately parks for a long time between turns, so its own
-- idle/turn clocks (SweepIdleChatRuns + the worker-side timers) bound it instead.
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
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a timed-out run must not keep a stale ⚠.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status = 'running'
  AND started_at < (sqlc.arg('now')::timestamptz - make_interval(secs => COALESCE(budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)))
  AND kind <> 'chat'
RETURNING id, user_id, status;

-- name: FailRunsOfStaleWorkersOverCap :many
-- A stale worker's non-terminal run that has already used its re-queue budget →
-- failed instead of re-queued. Stamps move_pending_since (reconcile restores the
-- origin column; the sweep itself never touches the forge — worker-loss recovery
-- must not wait on a down forge).
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
  AND requeue_count >= @max_requeues
  AND worker_id IN (
      SELECT id FROM workers WHERE last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff
  )
RETURNING id, user_id, status;

-- name: RequeueRunsOfStaleWorkers :many
-- A stale worker's non-terminal run within its re-queue budget → back to queued
-- (worker_id kept for affinity, requeue_count incremented).
UPDATE runs SET status = 'queued', requeue_count = requeue_count + 1,
    -- Exit contract (PRD #47 Decision 3): reset on the way back to 'queued'; the
    -- detector re-evaluates the queued signal from this transition's updated_at.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
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
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
  AND requeue_count >= @max_requeues
RETURNING id;

-- name: RequeueWorkerRuns :execrows
-- Within budget → re-queued to this same worker (affinity), which then re-claims
-- and resumes from the persisted session (handles docker compose down && up).
UPDATE runs SET status = 'queued', requeue_count = requeue_count + 1,
    -- Exit contract (PRD #47 Decision 3): reset on the way back to 'queued'; the
    -- detector re-evaluates the queued signal from this transition's updated_at.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
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
INSERT INTO run_usage (
    run_id, session_id, model,
    input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, updated_at
) VALUES (
    @run_id, @session_id, @model,
    @input_tokens, @cache_read_tokens, @cache_creation_tokens, @output_tokens, @cost_usd, now()
)
ON CONFLICT (run_id, session_id, model) DO UPDATE SET
    input_tokens          = GREATEST(run_usage.input_tokens,          EXCLUDED.input_tokens),
    cache_read_tokens     = GREATEST(run_usage.cache_read_tokens,     EXCLUDED.cache_read_tokens),
    cache_creation_tokens = GREATEST(run_usage.cache_creation_tokens, EXCLUDED.cache_creation_tokens),
    output_tokens         = GREATEST(run_usage.output_tokens,         EXCLUDED.output_tokens),
    cost_usd              = GREATEST(run_usage.cost_usd,              EXCLUDED.cost_usd),
    updated_at            = now();

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

-- name: CreateStopVerdictInput :one
-- Enqueue a deliberate-stop verdict (cancel / reject_plan) for the live worker AND
-- stamp runs.stop_kind in the SAME statement (PRD #33 Decision 3): a data-modifying
-- CTE runs to completion exactly once, so the stop signal can never be lost
-- independently of the input that requested it — which a second, non-transactional
-- UPDATE would risk, reintroducing the failed-vs-stopped bug. workersvc.Store exposes
-- no transaction seam, so this single combined statement IS the atomicity.
--
-- THREE callers since PRD #108 M5, not two, and the third is not a human verdict:
-- the auto-stop evaluator enqueues kind='cancel' with stop_kind='auto_stopped' for a
-- run whose message writes are in a confirmed permanent-failure loop. The MECHANISM
-- is unchanged — every caller stamps, so the stamp stays unconditional (no
-- IS NOT NULL guard and thus no parameter-type-inference pitfall) — only the
-- enumeration in this comment was wrong, and it is corrected in the same commit that
-- made it wrong. The stamp lands while the run is still non-terminal
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
WITH stamped AS (
    UPDATE runs SET stop_kind = @stop_kind, updated_at = now()
    WHERE id = @run_id
    RETURNING id
)
INSERT INTO run_user_inputs (run_id, kind, body)
VALUES (@run_id, @kind, @body)
RETURNING *;

-- name: ConsumeRunInputs :many
-- FIFO consume: mark and return every pending input for the run, oldest first.
-- FOR UPDATE SKIP LOCKED keeps two concurrent polls from returning the same row.
WITH pending AS (
    SELECT p.id FROM run_user_inputs p
    WHERE p.run_id = @run_id AND p.consumed_at IS NULL
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
-- The follow-up steer queue for a run, NEWEST FIRST and UNCAPPED (PRD #95 Decision 4):
-- the web + CLI steer queue reads exactly kind='follow_up', with delivery status derived
-- client-side from consumed_at (NULL → Queued, set → Delivered). Deliberately NOT the
-- judge's ListRunInputsForRun (oldest-first, @lim-capped, all kinds) — that would drop
-- the newest follow-ups behind its cap on a busy/chat run. Owner-scoping is enforced at
-- the run resolve (GetRunByIDForUser), not here.
-- The column list is the table's FULL set (question_id, PRD #88, included and always
-- NULL for a follow_up), which is what keeps sqlc returning the shared RunUserInput
-- model instead of minting a query-specific row type. Dropping a column here is not a
-- local edit: it re-types this query and breaks the workersvc.Store interface, the
-- service signature, the handler and its fake.
SELECT id, run_id, kind, body, consumed_at, created_at, question_id FROM run_user_inputs
WHERE run_id = @run_id AND kind = 'follow_up'
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
       c.forge_type, c.base_url, c.token_ciphertext
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
SELECT id, user_id, status, auto_approve,
       started_at, last_activity_at, updated_at,
       health, health_reason, health_since, health_notified_at,
       budget_wall_seconds
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
SELECT count(*) FROM workers WHERE user_id = @user_id AND status = 'online';

-- name: CountOnlineWorkersWithFreeSlotForUser :one
-- How many of a user's ONLINE workers plausibly have room for another run — the
-- queued-run reason resolver (PRD #216) uses it to tell a SATURATED fleet (every
-- online worker at its advertised run-lane cap, so a fleet-aware claim may be
-- deferring this run to a peer that is itself full) from a fleet with an idle
-- worker that simply has not claimed yet. A NULL cap advertises no bound, so such a
-- worker is treated as always having room. Active count uses the SAME run-lane
-- definition as ListWorkersByUser.active_runs (status claimed/running/
-- awaiting_approval/awaiting_input, kind <> 'chat'). Only called for a queued run
-- already past its health threshold, so it is off the hot path.
SELECT count(*) FROM workers w
WHERE w.user_id = @user_id
  AND w.status = 'online'
  AND (w.max_concurrent_runs IS NULL
       OR (SELECT count(*) FROM runs r
            WHERE r.worker_id = w.id
              AND r.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input')
              AND r.kind <> 'chat') < w.max_concurrent_runs);

-- name: RunHasVerdictSinceGateOpened :one
-- Has the owner already answered THIS approval gate, with the worker yet to act on it
-- (issue #182)? healthTargetFor's awaiting_approval arm asks before flagging
-- approval_idle, and reports waiting_worker instead when the answer is true: a run whose
-- human has responded is waiting on its WORKER, not on its owner. Before this existed the
-- arm timed purely off updated_at, so a user who requested changes at t+50m was still
-- nudged at t+60m to approve a plan they had already responded to.
--
-- @gate_opened_at is runs.updated_at AS THE DETECTOR READ IT. SetRunAwaitingApproval sets
-- status, plan_md, the health columns and updated_at together, so that one column is both
-- the age clock the arm's threshold guard uses AND this episode's boundary. Passing the
-- value the detector already holds — rather than re-reading runs here — keeps this
-- predicate on the same snapshot as that guard, so the two cannot disagree about which
-- episode they are describing.
--
-- 🔴 NO FLAPPING — BUT NOT BECAUSE THE PREDICATE IS MONOTONE. It was first documented
-- here as "monotone within an episode by construction", and that is FALSE. FIVE
-- statements bump runs.updated_at without moving the run out of awaiting_approval:
-- SetRunWaitOnLimit (user-reachable at PUT /api/runs/{id}/wait-on-limit),
-- ClearIssueRunsMovePending (a card drag), RecordRunColumnMove, ClearRunMovePending and
-- SetRunMRState. Four of the five carry no status guard at all. So this predicate CAN go
-- true → false inside one gate episode.
--
-- The no-flapping conclusion survives, for a different reason, and the reason is worth
-- knowing because it is what a future change could break: updated_at is ALSO the arm's
-- threshold clock. Any bump therefore makes olderThan(now, updated_at, th.approval) false
-- FIRST, so the next tick returns healthOK before this lookup is reached at all. There is
-- no direct waiting_worker → approval_idle edge; the reachable path is
-- waiting_worker → ok → (one full threshold later) approval_idle, at the far end of which
-- #182's symptom returns. That reachability is a separate issue, not this query's to fix.
--
-- Whoever separates the clock from the episode boundary re-opens the flap this paragraph
-- says is closed. That is the load-bearing sentence, not "monotone".
--
-- 🔴 `>=`, NOT `>`, IS LOAD-BEARING. CreateApprovePlanInput and CreateStopVerdictInput both
-- `SET updated_at = now()` in the SAME statement that inserts the row, and now() is the
-- transaction timestamp — so created_at and updated_at come out EXACTLY EQUAL. Under `>`,
-- an undelivered approve-with-selection or an undelivered cancel would still report
-- "waiting for the plan to be approved" an hour after the owner acted.
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
--   through the normal UI. The updated_at bumps described above do not rescue this: they
--   suppress the flag via the threshold clock rather than falsifying the predicate, so the
--   run reports healthOK instead, which is equally silent.
--
--   answer is EXCLUDED as structurally unreachable here: submitAnswer
--   (internal/workersvc/service.go) refuses unless the run is 'awaiting_input', and every
--   path into 'awaiting_approval' stamps updated_at — so an answer from an earlier park is
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
