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
       -- retaining_unpublished_work (PRD #1296 M4, D4): does this worker hold any OPEN
       -- custody hold? A held worker consumes NO active run/LLM slot (so it is not counted
       -- in busy/active_runs above), but it is NOT free capacity — it still counts against
       -- the per-owner hosted quota, and the owner surface distinguishes it. A top-level
       -- EXISTS types as a plain Go bool here, exactly like the busy column above.
       EXISTS (
           SELECT 1 FROM recovery_custody_holds h
           WHERE h.live_worker_id = w.id AND h.state = 'open'
       ) AS retaining_unpublished_work,
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
        -- protocol_capabilities is the server-authoritative PROTOCOL capability set (PRD
        -- #1226 M1, D2): the FilterProtocol-ed set the worker self-reports, kept SEPARATE
        -- from capabilities so a protocol string never enters the scheduler vocabulary.
        -- Overwritten on every register (the fresh-start signal), so a worker that stops
        -- self-reporting the protocol loses it here too — which correctly makes it unable
        -- to claim an interlocked run. COALESCE guards the NOT NULL column against a nil
        -- slice from any caller (pgx encodes a nil []string as SQL NULL): a nil report
        -- stores '{}', not NULL.
        protocol_capabilities = COALESCE(@protocol_capabilities::text[], '{}'),
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
        -- Reset the disk-pressure debounce streak (PRD #837 M4): a register is a FRESH
        -- pod incarnation, so a prior incarnation's streak must never carry forward — a
        -- rolled/restarted worker starts clean and must re-earn its >=2-heartbeat streak
        -- before the poll re-derives disk_pressure. (Distinct from HeartbeatWorker's
        -- increment/reset CASE: this is reset-on-action, unconditional.)
        stats_disk_pressure_streak = 0,
        -- PRD #1390 M2a: rotate the register nonce every snapshot must echo and RESET the
        -- snapshot epoch to 0 under it (D3). A fresh worker process starts its epoch at 1, so
        -- resetting to 0 here means its very first post-register snapshot (epoch 1) is accepted
        -- while any delayed high-epoch snapshot from the PREVIOUS process is rejected by the
        -- now-stale nonce — not by epoch. The nonce must live on the row (not in memory)
        -- because it is checked exactly when an api restart has forgotten everything else.
        snapshot_register_nonce = @snapshot_register_nonce,
        snapshot_epoch      = 0,
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
    -- Per-volume disk sample (PRD #837 M1), same write-every-tick-incl-NULL discipline
    -- as the mem columns; display-only.
    stats_disk_nix_bytes        = sqlc.narg('stats_disk_nix_bytes'),
    stats_disk_nix_total_bytes  = sqlc.narg('stats_disk_nix_total_bytes'),
    stats_disk_data_bytes       = sqlc.narg('stats_disk_data_bytes'),
    stats_disk_data_total_bytes = sqlc.narg('stats_disk_data_total_bytes'),
    -- Disk-pressure debounce streak (PRD #837 M4). Increment (bounded to 100 so a
    -- perpetually-full worker can't overflow the counter) when THIS tick's sample is
    -- over threshold, else reset to 0 — so a single under-threshold (or absent) sample
    -- breaks the streak. @disk_over_threshold is a NON-narg bool computed in the Go
    -- service (diskOverThreshold): a nil/absent stats sample lands here as false, which
    -- correctly resets. The poll derives disk_pressure = streak>=2 AND fresh; this column
    -- is display/lifecycle-only and never a scheduling input (Decision 5).
    stats_disk_pressure_streak = CASE
        WHEN @disk_over_threshold::boolean THEN LEAST(workers.stats_disk_pressure_streak + 1, 100)
        ELSE 0
    END,
    updated_at            = now()
WHERE id = @id
RETURNING *;

-- name: GetWorkerForUpdate :one
-- PRD #1390 M2a: lock the worker row FOR UPDATE at the top of the Register transaction, in
-- the canonical worker-row lock order shared with the stale-worker passes' `locked` CTE and
-- with HeartbeatWorker's own UPDATE. Holding it across the nonce rotation + snapshot persist +
-- orphan fail/requeue is what serialises Register against a concurrent heartbeat or stale
-- sweep, so a run's D11 lease is read against a settled snapshot rather than a torn one.
SELECT * FROM workers WHERE id = @id FOR UPDATE;

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
-- Repo-less kinds (judge/chat) INSERT elsewhere and keep the '{}' column default;
-- self_improve also INSERTs elsewhere (selfimprove.sql) but is repo-bearing and copies
-- its repo's hint the same way. Plan inference (M4) later union-merges via a separate
-- UPDATE.
--
-- 🔴 completion_contract_version (PRD #1226 M1, D1) is listed here for the SAME reason as
-- the fields above: it is a silently-omittable nullable param (sqlc.narg). createRun reads
-- the completion_interlock_rollout switch (default ON; an explicit "false" is the admin
-- kill-switch; a cold read error counts as off) and passes 1 ONLY when it is on AND the run
-- is not seeded, whether its resolved harness is Claude or Codex, stamping the run as
-- INTERLOCKED before its first claim; a switch-off or seeded run passes NULL and stays
-- the explicit legacy state. An omitted Go struct field would compile green
-- and silently ship NULL for every run (the feature inert), so a per-path test guards it,
-- not the compiler. Stamped BEFORE the first claim on purpose: approval does not re-claim,
-- so stamping only at approval would make the D2 hard claim clause vacuous for the
-- plan-phase worker. The contract CONTENT (completion_contract/contract_revision) is still
-- absent here — it is frozen with milestones_frozen at approval / the first running report.
-- 🔴 harness (PRD #1429 M1, was #1332 M5A / D2) is now the @harness PARAMETER supplied by
-- the M5B create seam (workersvc.createRunAtomic), not the SQL literal 'claude': the atomic
-- create transaction resolves D11 and passes the resolved harness here so a Codex row and
-- its binding freeze commit together. M2/M3 wire the real per-origin value; every current
-- caller passes string(HarnessClaude) as a mechanical stopgap, byte-identical to today.
--
-- 🔴 credential_override_mode / credential_override_secret_id (PRD #1247 M1) are the
-- silently-omittable per-run credential override (D1), both sqlc.narg — NULL = inherit
-- the worker binding, byte-identical to a pre-#1247 run. Every M1 caller passes NULL;
-- a real user choice is wired in M2 (create) / M6 (schedule). Named explicitly per this
-- query's own "name every column" convention so an unstamped path is visible in a diff
-- of THIS file, and guarded by a per-path test rather than the compiler (the narg trap
-- above applies identically).
INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, origin_column, move_pending_since, auto_approve, wait_on_limit, mr_rework_enabled, plan_md, plan_source, agent_source, agent_exclusions, planned_base_commit, require_base_match, model, override_subagent_model, issue_comments, review_comments, required_capabilities, trigger_source, completion_contract_version, harness, credential_override_mode, credential_override_secret_id)
VALUES (@user_id, @repo_id::uuid, @issue_iid, @issue_title, @issue_description, sqlc.narg('origin_column'), now(), @auto_approve, @wait_on_limit, sqlc.narg('mr_rework_enabled'), sqlc.narg('plan_md'), @plan_source, sqlc.narg('agent_source'), sqlc.narg('agent_exclusions')::jsonb, sqlc.narg('planned_base_commit'), @require_base_match, sqlc.narg('model'), @override_subagent_model, sqlc.narg('issue_comments')::jsonb, sqlc.narg('review_comments')::jsonb, COALESCE((SELECT rp.required_capabilities FROM repos rp WHERE rp.id = @repo_id::uuid), '{}'), @trigger_source, sqlc.narg('completion_contract_version'), @harness, sqlc.narg('credential_override_mode'), sqlc.narg('credential_override_secret_id'))
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
-- Usage is deliberately NOT joined here (issue #1620). It used to be a LEFT JOIN of
-- run_usage_totals, but under pgx's cached prepared statements PostgreSQL switched to a
-- GENERIC plan that could not push r.id into the view: it nested-looped the view once
-- per run row, re-folding all of run_usage each time (13.9 s vs 60 ms custom). The
-- handler now reads the page's rollup totals with ONE ListRunUsageTotalsForRuns call
-- over the page's run ids; a run with no usage is absent from that result (rendered as
-- absent, never a fake 0).
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
       -- issue #1418: does this run have an available recovery capture to export? The
       -- capture half of the read-path landing_state derivation (workersvc.DeriveLandingState),
       -- owner-scoped and riding idx_recovery_captures_run_owner (run_id, user_id). The
       -- correlated EXISTS is cast to boolean so sqlc types it as a usable bool (an uncast
       -- EXISTS types as interface{}); the inner alias c is the subquery's recovery_captures,
       -- distinct from the outer forge_connections c.
       (EXISTS (SELECT 1 FROM recovery_captures c WHERE c.run_id = r.id AND c.user_id = r.user_id AND c.state = 'available'))::boolean AS has_available_capture
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id   -- forge_type for the per-run MR/PR noun (PRD #65 D2); every repo has a connection
LEFT JOIN issues i ON i.repo_id = r.repo_id AND i.forge_issue_iid = r.issue_iid   -- PRD #411: 1:1 (issues UNIQUE (repo_id, forge_issue_iid)); yields the issue web URL for the run's #<iid> link
LEFT JOIN workers w ON w.id = r.worker_id
LEFT JOIN run_reviews rv
       ON rv.target_run_id = r.id      -- UNIQUE target_run_id → at most one row (PRD #98 M4)
      AND rv.user_id = r.user_id       -- self-standing owner scope; see the note above
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

-- name: LatestPlanSeqForRun :one
-- The seq of the run's latest plan-gate frame ({plan, plan_revising}), for the awaiting_approval
-- resume_phase (PRD #1247 M5, D13). 0 when the run has emitted no plan frame yet. Backed by
-- run_messages UNIQUE (run_id, seq).
SELECT COALESCE(MAX(seq), 0)::bigint AS seq
FROM run_messages
WHERE run_id = @run_id::uuid
  AND kind IN ('plan', 'plan_revising');

-- name: LatestToolUseForRuns :many
-- The newest tool_use frame per run for a page of runs (PRD #1064 D3, current_activity):
-- DISTINCT ON (run_id) with ORDER BY run_id, seq DESC yields exactly one row per run —
-- its greatest-seq tool_use — which runactivity.FromFrame folds into the "now" line. A
-- run with no tool_use frame returns no row (⇒ null current_activity). Backed by the
-- partial index idx_run_messages_tool_use_seq (run_id, seq DESC) WHERE kind = 'tool_use'
-- (migration 00186), so the per-run first row is one index seek rather than a walk back
-- over the trailing non-tool_use frames the UNIQUE (run_id, seq) index would force.
SELECT DISTINCT ON (run_id) run_id, seq, kind, agent, agent_instance, agent_label, payload, created_at
FROM run_messages
WHERE run_id = ANY(@run_ids::uuid[])
  AND kind = 'tool_use'
ORDER BY run_id, seq DESC;

-- name: LiveLaneFramesForRun :many
-- PRD #1353: newest tool_use frame per live subagent instance for a run (DISTINCT ON
-- agent_instance, greatest seq). milestonelanes.Derive folds each into a lane and back-joins
-- it to its Agent dispatch by agent_instance.
SELECT DISTINCT ON (agent_instance) agent_instance, seq, kind, agent, agent_label, payload, created_at
FROM run_messages
WHERE run_id = @run_id::uuid
  AND agent_instance IS NOT NULL
  AND kind = 'tool_use'
ORDER BY agent_instance, seq DESC;

-- name: LeadAgentDispatchFramesForRun :many
-- PRD #1353: the lead-lane Agent dispatch tool_use frames (payload {id, input:{subagent_type,
-- description}}) that carry each live instance's [<id>] milestone tag + label. Full payload is
-- needed (id + input.description); bounded by the lead's dispatch count.
SELECT seq, payload, created_at
FROM run_messages
WHERE run_id = @run_id::uuid
  AND agent_instance IS NULL
  AND kind = 'tool_use'
  AND payload->>'name' = 'Agent'
ORDER BY seq;

-- name: LeadDispatchCompletionIDsForRun :many
-- PRD #1353: the tool_use_ids of lead-lane tool_result frames — a dispatch whose id appears here
-- has COMPLETED (its subagent returned). Projects ONLY the id, never the (possibly large) tool
-- output content, since Derive reads only the id.
SELECT DISTINCT (payload->>'tool_use_id')::text AS tool_use_id
FROM run_messages
WHERE run_id = @run_id::uuid
  AND agent_instance IS NULL
  AND kind = 'tool_result'
  AND payload->>'tool_use_id' IS NOT NULL;

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
--
-- Roll health (PRD #1484 M1/M2), LEFT JOINed exactly as ListWorkersByUser does so the
-- admin fleet view and the cross-user health checks (fleet.roll, fleet.disk) can read
-- the controller's per-worker roll signal instead of the always-null placeholder the
-- old admin list carried. A worker with no report — every external worker, any hosted
-- worker the controller has not reached, and the whole fleet under docker-compose where
-- no controller runs — still lists (LEFT JOIN). observed_at is the API's own receipt
-- time and the only freshness input; controller_reported_at is deliberately NOT
-- selected (it is display-only and must not reach a freshness classifier). The full set
-- of roll columns ListWorkersByUser selects — INCLUDING pod_phase and upgrading_since —
-- is carried (M2) so AdminListWorkers' row-to-DTO mapper builds the SAME RollSignal and
-- classifies upgrade_status/blocking fields byte-identically to GET /api/workers, rather
-- than diverging on the INV-5 ceiling (upgrading_since) or the rolling-detail (pod_phase).
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
       u.email AS owner_email,
       rh.phase              AS roll_phase,
       rh.phase_since        AS roll_phase_since,
       rh.pod_phase          AS roll_pod_phase,
       rh.blocking_container AS roll_blocking_container,
       rh.blocking_reason    AS roll_blocking_reason,
       rh.restart_count      AS roll_restart_count,
       rh.last_exit_code     AS roll_last_exit_code,
       rh.observed_at        AS roll_observed_at,
       rh.upgrading_since    AS roll_upgrading_since,
       rh.worker_image_tag   AS roll_worker_image_tag
FROM workers w
JOIN users u ON u.id = w.user_id
LEFT JOIN worker_upgrade_reports rh ON rh.worker_id = w.id
ORDER BY w.created_at DESC;

-- name: GetRunOwnedByWorker :one
-- Worker-endpoint authz: a worker may only touch a run it currently holds.
SELECT * FROM runs WHERE id = @id AND worker_id = @worker_id;

-- name: GetRunOrphanIdentity :one
-- Orphan-classification read (issue #1319): DISTINCT from GetRunOwnedByWorker. Scoped to
-- the worker's OWNER (user) and the claimant's repo, NOT worker_id, so a terminal owner
-- that moved workers is still found (Gap 2). pipeline_id + pipeline_ref are required to
-- derive the owner's ci_fix clone branch exactly.
SELECT status, repo_id, kind, issue_iid, branch, pipeline_ref, pipeline_id
FROM runs WHERE id = @id AND user_id = @user_id AND repo_id = @repo_id;

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
--
-- PRD #1296 M1 (D2/D3): the claim now runs as leading CTEs before the UPDATE so the hold
-- is opened atomically in the SAME statement. `target` selects + LOCKS the single claimable
-- run ONCE (carrying the columns the hold needs); `hold` opens the H-free custody hold for a
-- recovery-capable worker on a code-publishing profile, reading FROM target so it inserts
-- iff a run was actually claimable; then the UPDATE claims that same target row. The final
-- statement is `UPDATE runs ... RETURNING *`, so the :one result is still store.Run — the
-- hold is a side effect. target and hold share target's single snapshot/lock, so the hold's
-- t.claim_generation + 1 equals the UPDATE's own increment.
WITH target AS (
    SELECT r.id, r.user_id, r.repo_id, r.kind, r.claim_generation FROM runs r
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
           -- Hold the pin whenever the run's OWN worker ROW still exists AND it can
           -- still resume the run — i.e. it is either DRAINING (cordoned for an image
           -- roll: same worker row, same PVC, new pod on the far side) OR its heartbeat
           -- is fresh. Fall open ONLY when the row is GONE (teardown — DeleteWorkerForUser
           -- / ReapEphemeralWorkers delete the row API-side BEFORE the controller's kube
           -- teardown, so `teardown ⟺ row absent`) or the owner is heartbeat-STALE AND
           -- NOT draining (death/hang — ADR-628 D3a's protected case). This is PRD #1030
           -- M2: the roll-vs-teardown discriminator with no new column. The old rule
           -- required `draining_since IS NULL` in the EXISTS, so a roll-cordoned owner
           -- (draining) failed the test → NOT EXISTS became TRUE → a parked run fell open
           -- and a live peer stole it cold across the roll (run #1009). We deliberately
           -- hold on `draining regardless of heartbeat` because the ~2 min pod-swap edge
           -- exceeds the 45s @heartbeat_cutoff, so a heartbeat-gated hold would wrongly
           -- fall open mid-swap (accepted tradeoff D7: a draining+dead owner then holds
           -- until the @affinity_cutoff ceiling).
           OR NOT EXISTS (
               SELECT 1 FROM workers ow
               WHERE ow.id = r.worker_id
                 AND (ow.draining_since IS NOT NULL
                      OR (ow.last_heartbeat_at IS NOT NULL
                          AND ow.last_heartbeat_at >= @heartbeat_cutoff)))
           -- Generous ceiling bounding the live-but-can't-serve case; @affinity_cutoff is
           -- now now() - WORKER_AFFINITY_CEILING (default 2h), NOT the 2-min grace.
           OR r.updated_at < @affinity_cutoff)
      -- PRD #1030 M2 / PRD #422 Decision 7: a DRAINING claimant claims NOTHING NEW. It
      -- reaches ClaimRun now (the workersvc early return that made it claim nothing was
      -- removed so it can re-claim its OWN promoted run through a roll), but it must be
      -- scoped to runs it already owns — never a new/unclaimed/fallen-open run. When the
      -- claimant is not draining this is a no-op; when it is, only its own runs qualify.
      AND (NOT @claimant_draining::boolean OR r.worker_id = @worker_id)
      -- PRD #216 D5: claiming-worker eligibility via the shared expression.
      -- PRD #84 M2 extends it: the run's required_capabilities must be a subset of the
      -- claiming worker's effective caps (@worker_caps ∪ docker), gated by @capability_aware.
      AND fn_worker_can_claim(@is_docker_worker::boolean, @docker_repo_allowlist::uuid[], r.repo_id, r.kind, @worker_caps::text[], r.required_capabilities, @capability_aware::boolean)
      -- PRD #1296 M1 (D2/D4): custody-admission. Block the claim when the run's OWNER
      -- already holds >= @custody_hold_limit UNRESOLVED (state='open') custody holds — the
      -- owner has too much unpublished work awaiting recovery disposition. A gated claim
      -- matches no row, so the run stays queued (the health resolver surfaces a distinct
      -- custody-limit reason against this SAME predicate in a later milestone). This is an
      -- OWNER-scoped admission gate (D4: the instance byte ceiling must never become a
      -- global stop-claiming switch), placed alongside the existing eligibility checks. A
      -- non-positive @custody_hold_limit DISABLES the gate (limit "<= 0" means unlimited),
      -- so it never blocks an ordinary claim; the production caller always passes the
      -- configured positive default (workersvc.custodyHoldLimit, 8).
      --
      -- Issue #1751 / ADR-1751: CONTINUATION EXEMPTION. A requeued run that was already
      -- claimed before (claim_generation >= 1) AND still holds its OWN open custody hold
      -- (same owner, same run_id) is exempt from the custody-count admission check: it is
      -- continuing work the owner already has in custody, not new work. The continuation may
      -- create another generation hold (the hold CTE below opens one per claim), but is exempt
      -- from custody-count admission. A fresh run (generation 0), or one whose own holds are
      -- all released/discarded, still faces the cap. The #1318 overshoot under concurrent
      -- claims still stands: this is an admission gate, not a strict ceiling.
      --
      -- The exemption is BOUNDED PER RUN: it holds only while the run's OWN open-hold count
      -- is below @custody_hold_limit (and at least one). Without the bound a claimed run that never
      -- reports running is swept back to queued by SweepClaimedNeverStarted (no requeue_count
      -- bump, no hold release), reclaims under the exemption, opens another generation hold,
      -- and loops forever; the owner cap used to stop that loop. Once a single run holds
      -- @custody_hold_limit open holds of its own, it faces the owner cap like new work. The
      -- same exemption expression is mirrored by GetCustodyAdmissionForRun (health reason) and
      -- GetCustodyAggregateForOwner.blocked_runs, so the pill, the aggregate and the claim
      -- agree.
      AND (@custody_hold_limit::int <= 0
           OR (r.claim_generation >= 1
               AND EXISTS (SELECT 1 FROM recovery_custody_holds oh
                             WHERE oh.user_id = r.user_id AND oh.run_id = r.id AND oh.state = 'open')
               AND (SELECT count(*) FROM recovery_custody_holds oh2
                      WHERE oh2.user_id = r.user_id AND oh2.run_id = r.id AND oh2.state = 'open') < @custody_hold_limit::int)
           OR (SELECT count(*) FROM recovery_custody_holds ch
                 WHERE ch.user_id = @user_id AND ch.state = 'open') < @custody_hold_limit::int)
      -- PRD #1226 M1 (D2): the NON-BYPASSABLE completion-protocol claim clause. An
      -- INTERLOCKED run (completion_contract_version IS NOT NULL) may be claimed ONLY by a
      -- worker whose SELF-REPORTED protocol_capabilities contain 'completion_interlock_v1';
      -- a LEGACY run (version IS NULL) is unaffected and claimable by any worker. This clause
      -- is a DEDICATED, standalone predicate INTENTIONALLY OUTSIDE fn_worker_can_claim,
      -- required_capabilities, ClearRunRequiredCapabilities and the @capability_aware
      -- kill-switch: an owner capability override or the kill-switch can neutralize the
      -- ordinary required-capability match, but NONE of them may authorize a worker that does
      -- not implement the completion protocol. @worker_protocol_caps is the claiming worker's
      -- stored workers.protocol_capabilities column, passed by the Go caller.
      AND (r.completion_contract_version IS NULL
           OR 'completion_interlock_v1' = ANY(@worker_protocol_caps::text[]))
      -- PRD #1332 M5A (D3): the NON-BYPASSABLE Codex-harness claim clause, MIRRORING the
      -- completion-protocol clause directly above. A CODEX-INDICATING run may be claimed ONLY by a
      -- worker whose SELF-REPORTED protocol_capabilities contain 'codex_harness_v1' (advertised only
      -- after a successful runtime-receipt probe); a NON-Codex run is unaffected. "Codex-indicating"
      -- checks ALL THREE binding facts — harness='codex' OR codex_material_revision IS NOT NULL OR
      -- codex_secret_id IS NOT NULL — so the gate FAILS CLOSED on inconsistent or legacy data: even
      -- if C1's coherence CHECK normally implies harness='codex', a row whose harness were somehow
      -- unset while a binding sentinel survives (e.g. the alias FK nulls codex_secret_id but leaves
      -- codex_material_revision) is still gated, never silently shipping its Codex credential block to
      -- an old worker that ignores the unknown JSON and runs Claude. Like the completion clause this is
      -- a DEDICATED, standalone predicate INTENTIONALLY OUTSIDE fn_worker_can_claim,
      -- required_capabilities, ClearRunRequiredCapabilities and the @capability_aware kill-switch:
      -- none of those may authorize a worker that cannot run Codex. Reuses the SAME
      -- @worker_protocol_caps param the completion clause reads (the claimant's stored
      -- workers.protocol_capabilities, passed by the Go caller).
      AND (NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
           OR 'codex_harness_v1' = ANY(@worker_protocol_caps::text[]))
      -- Interlocked Codex turns require the newer completion loop, independently of overrides.
      AND (r.completion_contract_version IS NULL
           OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
           OR 'codex_completion_interlock_v1' = ANY(@worker_protocol_caps::text[]))
      -- PRD #1551 M4 (D6): the NON-BYPASSABLE custom-Codex-model claim clause, a SIBLING of the
      -- codex-harness clause directly above. A Codex run whose EFFECTIVE worker-root model is a
      -- CUSTOM (non-curated) id may be claimed ONLY by a worker whose protocol_capabilities contain
      -- 'codex_custom_model_v1'. The effective root follows D4 precedence rendered in SQL: a frozen
      -- r.model that is itself a curated id wins (curated ⇒ no new requirement), otherwise the
      -- owner's default_codex_model lane. Task-review (review_target_run_id IS NOT NULL), judge and
      -- chat are EXEMPT — assembly reads no Codex lane for them — so the exemption is keyed on
      -- review_target_run_id/kind, NEVER on all of kind='task'. NULL-safe via COALESCE: a NULL
      -- r.model AND NULL lane make the effective root NULL, so the inner NOT(... = ANY ...) is NULL
      -- and COALESCE(...,false) is false ⇒ not custom ⇒ no new requirement. Same standalone posture
      -- as the harness clause (OUTSIDE fn_worker_can_claim, required_capabilities and the
      -- capability_aware kill-switch); the effective-root expression is written identically here,
      -- in the peer mirror below, and in CountOnlineWorkersClaimableForRun.
      AND (
          NOT (
              r.harness = 'codex'
              AND r.kind NOT IN ('judge', 'chat')
              AND r.review_target_run_id IS NULL
              AND COALESCE(
                  NOT ((CASE WHEN r.model = ANY(@codex_curated_models::text[]) THEN r.model
                             ELSE (SELECT u.default_codex_model FROM users u WHERE u.id = r.user_id) END)
                       = ANY(@codex_curated_models::text[])),
                  false)
          )
          OR 'codex_custom_model_v1' = ANY(@worker_protocol_caps::text[])
      )
      -- PRD #1590 M2 (D2, amendment A1): keep a Codex subscription run queued while
      -- its SAME alias's account authority is on hold (D1's hold class):
      --   (1) quarantine: the linked account is quarantined and is still the run's
      --       frozen identity at its frozen credential_revision. A run with no frozen
      --       identity (codex_account_key NULL: its alias was unlinked at create, and
      --       nothing freezes it later) fails evalCodexReleasePredicate with
      --       ErrCodexAccountKeyUnfrozen whatever the account does, so it is not held
      --       (D1 holds only what can resume); like a different identity or a bumped
      --       revision, the claim proceeds and fails in assembly as before;
      --   (2) re-login in flight: past first link, a newer login on the alias is
      --       staging/failed (the PATCH cleared the link, so (1) cannot see it);
      --   (3) A1: past first link, that newer login already linked back to the
      --       frozen identity at the frozen credential_revision (D5 re-admits it;
      --       until then the run's material_revision is stale).
      -- This is the SQL twin of classifyCodexClaimAuthority; a shared fixture table
      -- pins the two against each other. The frozen identity is stored only as the
      -- Go-encoded JSON array codex_account_key (00202, codexAccountKey), and no SQL
      -- encoder exists, so it is DECODED with a guarded ::jsonb cast and compared
      -- structurally to the account's tuple; an undecodable key never matches. The
      -- gate is run-level, independent of worker capabilities, and precedes the
      -- custody-opening hold CTE. The LEFT JOIN preserves an unlinked alias
      -- (including a first login); every lookup stays scoped to this run's owner.
      -- The SAME predicate text appears in the peer mirror below, in
      -- ParkQueuedCodexAccountUnavailablePage and in ListActiveRunsForHealth's
      -- codex_account_gated; a store test pins the copies byte-identical after
      -- whitespace normalisation.
      AND NOT (
          r.harness = 'codex'
          AND r.codex_auth_mode = 'subscription'
          AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
          AND EXISTS (
              SELECT 1 FROM codex_credential_state ccs
              LEFT JOIN codex_provider_account cpa
                  ON cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id
              WHERE ccs.user_secret_id = r.codex_secret_id
                AND ccs.user_id = r.user_id
                AND (
                    (cpa.coord_state = 'quarantined'
                     AND cpa.credential_revision = r.codex_account_revision
                     AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                              THEN r.codex_account_key::jsonb
                                   = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                              ELSE false END)
                    OR (r.codex_account_key IS NOT NULL
                        AND ccs.material_revision > r.codex_material_revision
                        AND (ccs.status IN ('staging', 'failed')
                             OR (ccs.status = 'linked'
                                 AND cpa.credential_revision = r.codex_account_revision
                                 AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                                          THEN r.codex_account_key::jsonb
                                               = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                                          ELSE false END)))
                )
          )
      )
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
                -- PRD #1226 M1 (D2): MIRROR the non-bypassable completion-protocol clause for
                -- the peer, or fleet-spread could DEFER an interlocked run to an INCAPABLE peer
                -- that would never be able to claim it — making the run unclaimable. Reads the
                -- peer's OWN workers.protocol_capabilities column directly (no Go param, unlike
                -- the claimant's @worker_protocol_caps above).
                AND (r.completion_contract_version IS NULL
                     OR 'completion_interlock_v1' = ANY(p.protocol_capabilities))
                -- PRD #1332 M5A (D3): MIRROR the non-bypassable Codex-harness clause for the peer, or
                -- fleet-spread could DEFER a CODEX-INDICATING run to an INCAPABLE peer that could never
                -- claim it (its OWN Codex claim clause above blocks it) — making the run permanently
                -- unclaimable by being preferred. Same all-three "Codex-indicating" test on r, reading
                -- the peer's OWN workers.protocol_capabilities column directly (no Go param, unlike the
                -- claimant's @worker_protocol_caps).
                AND (NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                     OR 'codex_harness_v1' = ANY(p.protocol_capabilities))
                AND (r.completion_contract_version IS NULL
                     OR NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)
                     OR 'codex_completion_interlock_v1' = ANY(p.protocol_capabilities))
                -- PRD #1551 M4 (D6): MIRROR the non-bypassable custom-Codex-model clause for the peer, or
                -- fleet-spread could DEFER a CUSTOM-root Codex run to an INCAPABLE peer that could never
                -- claim it (its OWN custom-model clause above blocks it) — making the run permanently
                -- unclaimable by being preferred. The effective-root expression is written IDENTICALLY to
                -- the claimant clause, reading the peer's OWN workers.protocol_capabilities (no Go param).
                AND (
                    NOT (
                        r.harness = 'codex'
                        AND r.kind NOT IN ('judge', 'chat')
                        AND r.review_target_run_id IS NULL
                        AND COALESCE(
                            NOT ((CASE WHEN r.model = ANY(@codex_curated_models::text[]) THEN r.model
                                       ELSE (SELECT u.default_codex_model FROM users u WHERE u.id = r.user_id) END)
                                 = ANY(@codex_curated_models::text[])),
                            false)
                    )
                    OR 'codex_custom_model_v1' = ANY(p.protocol_capabilities)
                )
                -- PRD #1590 M2 (D2, A1): mirror the claimant's account gate so a
                -- busy worker never defers this run to a peer that cannot claim it.
                AND NOT (
                    r.harness = 'codex'
                    AND r.codex_auth_mode = 'subscription'
                    AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
                    AND EXISTS (
                        SELECT 1 FROM codex_credential_state ccs
                        LEFT JOIN codex_provider_account cpa
                            ON cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id
                        WHERE ccs.user_secret_id = r.codex_secret_id
                          AND ccs.user_id = r.user_id
                          AND (
                              (cpa.coord_state = 'quarantined'
                               AND cpa.credential_revision = r.codex_account_revision
                               AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                                        THEN r.codex_account_key::jsonb
                                             = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                                        ELSE false END)
                              OR (r.codex_account_key IS NOT NULL
                                  AND ccs.material_revision > r.codex_material_revision
                                  AND (ccs.status IN ('staging', 'failed')
                                       OR (ccs.status = 'linked'
                                           AND cpa.credential_revision = r.codex_account_revision
                                           AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                                                    THEN r.codex_account_key::jsonb
                                                         = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                                                    ELSE false END)))
                          )
                    )
                )
                AND pa.active < p.max_concurrent_runs
                AND pa.active * (SELECT w.max_concurrent_runs FROM workers w WHERE w.id = @worker_id)
                    < (SELECT count(*) FROM runs mr
                        WHERE mr.worker_id = @worker_id
                          AND mr.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
                          AND mr.kind <> 'chat')
                      * p.max_concurrent_runs
          )
      )
      -- PRD #1390 M3 (D3, D8): global, pre-claim snapshot dedupe — three exclusions that run
      -- INSIDE candidate selection, BEFORE the generation increment and before the `hold` CTE
      -- opens custody, so a run a fresh snapshot lists is never returned to any claimant (including
      -- a sibling past the affinity ceiling) and no side effect fires for it.
      --
      -- (1) Fresh-snapshot / terminal-pending exclusion: never claim a run a FRESH snapshot lists
      -- as active at its CURRENT generation. Freshness is the snapshot's OWN reported_at (a worker
      -- whose snapshots fail validation must not keep stale rows protected by its liveness), within
      -- @snapshot_fresh_cutoff = now() - (WORKER_HEARTBEAT_STALE + WORKER_HEARTBEAT_INTERVAL). A
      -- terminal-pending lease (a.terminal_pending AND still unexpired) excludes regardless of
      -- freshness (#1391's journaled outcome outlives the heartbeat, D11). Worker-scoped
      -- (a.worker_id = r.worker_id) for consistency with the sweep predicates, and sound because a
      -- run's snapshot row is only ever its owner's.
      AND NOT EXISTS (
          SELECT 1 FROM worker_active_runs a
          WHERE a.run_id = r.id AND a.worker_id = r.worker_id
            AND a.claim_generation = r.claim_generation
            -- PRD #1497 M1 (D16): a RELEASED flight's fresh snapshot must NOT block the reclaim of a
            -- server-parked run — the release is exactly the signal the old flight is over.
            AND r.claim_released_at IS NULL
            AND (a.reported_at >= @snapshot_fresh_cutoff
                 OR (a.terminal_pending AND a.terminal_pending_until > now())))
      -- (2) Request-array exclusion (fact 7): the claimant's OWN request snapshot excludes its
      -- listed runs at the CURRENT generation, so the exclusion holds BEFORE the first heartbeat
      -- persists the rows exclusion (1) reads. @request_active_ids / @request_active_gens are
      -- index-aligned pairs; the two-array unnest is spelled as a WITH ORDINALITY zip because
      -- sqlc's analyzer cannot type a multi-argument unnest(a, b) (see judge_bulk_disposition.sql).
      -- Empty arrays (no request snapshot, or the no-snapshot path) match nothing → no exclusion.
      AND NOT EXISTS (
          SELECT 1
          FROM unnest(@request_active_ids::uuid[]) WITH ORDINALITY AS req_id(id, ord)
          JOIN unnest(@request_active_gens::bigint[]) WITH ORDINALITY AS req_gen(gen, ord)
               ON req_gen.ord = req_id.ord
          WHERE req_id.id = r.id AND req_gen.gen = r.claim_generation)
      -- (3) Overflow closure (D11): never claim a run whose OWNER is under an unexpired
      -- pending_overflow closure — an outcome the worker could not list has no row of its own to
      -- lease, so the worker-level closure stands in for the row-level lease. An unassigned run
      -- (r.worker_id IS NULL) has no owner row, so it is never closed here; the claimant-side half
      -- (a flagged worker refused every claim, unassigned included) is the service-level guard.
      AND NOT EXISTS (SELECT 1 FROM workers w WHERE w.id = r.worker_id AND w.pending_overflow_until > now())
      -- PRD #1497 M1 (D19): a server-side wall park records the incarnation it released
      -- (released_worker_id + released_worker_nonce, captured under the park's lock). That EXACT
      -- incarnation cannot reclaim its own parked run — otherwise a dead/incapable/unresponsive
      -- worker that came back would win the run it could not park. A DIFFERENT worker, or the SAME
      -- worker RE-REGISTERED (registration rotates snapshot_register_nonce, so it is a new safe
      -- incarnation), is not excluded, which is what keeps a single-worker deployment live. Rows with
      -- no released pair (released_worker_id IS NULL) are always claimable — the leading IS NULL arm
      -- is REQUIRED because `NULL = @worker_id` is unknown, not false, so a bare NOT(...) would
      -- wrongly exclude every ordinary run for a claimant whose nonce is also NULL.
      AND (r.released_worker_id IS NULL
           OR r.released_worker_id <> @worker_id
           OR r.released_worker_nonce IS DISTINCT FROM
                (SELECT ow.snapshot_register_nonce FROM workers ow WHERE ow.id = @worker_id))
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
),
hold AS (
    -- PRD #1296 M1 (D2/D3): open the H-free custody hold atomically with the claim, for a
    -- RECOVERY-CAPABLE worker (@recovery_capable, derived from the worker's advertised
    -- recovery_archive_v1 protocol capability) on one of the six code-publishing profiles.
    -- Reads FROM `target`, so it inserts exactly one hold iff a run was actually claimable
    -- (an admission-gated or idle claim produces no `target` row and thus no hold). Both live
    -- FKs point at the claimed run + claiming worker (ON DELETE RESTRICT while open), and the
    -- immutable provenance columns record the taking worker's identity for a later
    -- AAD-authenticated post-terminal recovery retry. t.claim_generation + 1 is the SAME
    -- value the UPDATE below sets (same locked row, same snapshot).
    INSERT INTO recovery_custody_holds
        (id, user_id, repo_id, run_id, generation, state,
         original_worker_id, original_worker_identity, live_worker_id, live_run_id,
         created_at, updated_at)
    SELECT gen_random_uuid(), t.user_id, t.repo_id, t.id, t.claim_generation + 1, 'open',
           @worker_id, @worker_identity::text, @worker_id, t.id, now(), now()
    FROM target t
    WHERE @recovery_capable::boolean
      AND t.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
    RETURNING 1
)
UPDATE runs SET
    status     = 'claimed',
    status_since = now(),
    worker_id  = @worker_id,
    claimed_at = now(),
    updated_at = now(),
    -- PRD #1296 M1 (D2): the general claim-lane counter, incremented once per successful
    -- claim. Returned in the claim payload; the hold above binds the identical value.
    claim_generation = claim_generation + 1,
    -- PRD #1247 M5 (D3): CLOSE the released-claim fence window. A held-state credential
    -- switch requeues the run with claim_released_at set (ReleaseCredentialSwitch), which
    -- makes the generation fence reject every report from the OLD flight. Reclaiming the
    -- run opens a fresh generation, so the fence must be cleared in the SAME atomic UPDATE
    -- that bumps claim_generation — otherwise a reclaim would immediately reject the NEW
    -- flight's own first report. A run that was never released has claim_released_at NULL
    -- already, so this is a harmless no-op on the ordinary claim path.
    claim_released_at = NULL,
    -- PRD #1497 M1 (D19): a successful claim clears the released-incarnation marker a server-side
    -- wall park recorded — the run now has a fresh, live owner, so the exclusion has done its job.
    -- (The excluded incarnation itself never reaches this UPDATE: the target predicate above bars it.)
    released_worker_id = NULL,
    released_worker_nonce = NULL,
    -- PRD #1390 M3 (D2 hygiene): clear the stale-requeue provenance on every fresh claim, alongside
    -- the generation bump. The refund in ReadoptRunsFromSnapshot fires only when
    -- stale_requeue_generation = claim_generation, so leaving a stale value here could refund a
    -- requeue_count charged against a generation this claim has already replaced.
    stale_requeue_generation = NULL,
    -- Exit contract (PRD #47 Decision 3): leaving 'queued' clears any health flag
    -- the detector raised (e.g. "no worker online"). health_notified_at is NOT reset.
    health = 'ok', health_reason = NULL, health_since = NULL
WHERE id = (SELECT id FROM target)
RETURNING *;

-- name: GetCustodyAdmissionForRun :one
-- Issue #1751 / ADR-1751: the per-run custody-admission facts the health resolver reads so
-- its reasonCustodyLimit pill agrees with ClaimRun. open_holds is the owner's UNRESOLVED
-- (state='open') hold count, the same count ClaimRun's custody clause compares against
-- @custody_hold_limit; continuation_exempt is ClaimRun's continuation exemption, byte-for-byte
-- the same expression: the run was claimed before (claim_generation >= 1) AND its OWN open
-- custody-hold count (owner-scoped) is at least 1 and below @custody_hold_limit. The upper bound
-- stops a run that the never-started sweep keeps requeueing from opening holds forever (see
-- ClaimRun). An exempt run is never blocked by the custody cap. A run that does not exist (or
-- belongs to another owner), or a non-positive limit, yields continuation_exempt = false.
SELECT
    (SELECT count(*) FROM recovery_custody_holds h
        WHERE h.user_id = @user_id::uuid AND h.state = 'open')::bigint AS open_holds,
    COALESCE((SELECT (r.claim_generation >= 1
                      AND EXISTS (SELECT 1 FROM recovery_custody_holds oh
                                    WHERE oh.user_id = r.user_id AND oh.run_id = r.id AND oh.state = 'open')
                      AND (SELECT count(*) FROM recovery_custody_holds oh2
                             WHERE oh2.user_id = r.user_id AND oh2.run_id = r.id AND oh2.state = 'open') < @custody_hold_limit::int)
                FROM runs r
                WHERE r.id = @run_id::uuid AND r.user_id = @user_id::uuid), false)::boolean AS continuation_exempt;

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
-- AFTER THE APPLIED approve_plan THAT MADE human_plan_approved TRUE. A tighter
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
SELECT r.checkpoint_tip,
       rp.web_url             AS repo_web_url,
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
                  AND i.applied_at IS NOT NULL))::boolean AS human_plan_approved
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id AND c.user_id = r.user_id -- #1688: owner-scoped token
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

-- name: RecordRunCredentialEpoch :exec
-- Append the attribution-journal row for one claim (PRD #1247 M1, D7/D14): one row per
-- (run_id, claim_generation), written by recordRunCredential right after SetRunAnthropicSecret
-- on a successful open. claim_generation is the run's EXISTING generation (00223, PRD #1349) —
-- this PRD never increments it — so re-recording the same claim (a retry after a crash before
-- the payload shipped) is idempotent: the composite PK conflicts and DO UPDATE refreshes the
-- snapshot fields in place rather than duplicating the epoch. secret_id/label/select_reason are
-- the credential the claim actually spent; applied_at defaults to now() and is refreshed on a
-- re-record so it names the last apply of that generation.
INSERT INTO run_credential_epochs (run_id, claim_generation, secret_id, label, select_reason, applied_at)
VALUES (@run_id, @claim_generation, sqlc.narg('secret_id'), sqlc.narg('label'), sqlc.narg('select_reason'), now())
ON CONFLICT (run_id, claim_generation) DO UPDATE
SET secret_id     = EXCLUDED.secret_id,
    label         = EXCLUDED.label,
    select_reason = EXCLUDED.select_reason,
    applied_at    = EXCLUDED.applied_at;

-- name: ListRunCredentialEpochs :many
-- The applied-switch history for one run (PRD #1247 M1, D7): every claim's credential
-- epoch, oldest generation first, for the run-detail DTO's credential_epochs and for the
-- M1 live-DB attribution test. Owner-scoped through the run join so a caller cannot read
-- another user's journal.
SELECT e.run_id, e.claim_generation, e.secret_id, e.label, e.select_reason, e.applied_at
FROM run_credential_epochs e
JOIN runs r ON r.id = e.run_id
WHERE e.run_id = @run_id AND r.user_id = @user_id
ORDER BY e.claim_generation ASC;

-- name: GetPriorRunCredentialEpoch :one
-- The epoch IMMEDIATELY BEFORE @claim_generation for a run (PRD #1247 M9, task c step 5): the
-- highest-generation epoch strictly below the current claim's generation. recordRunCredential
-- consults it to detect an APPLIED credential switch — if a prior epoch exists AND its secret_id
-- differs from the token the current claim spent, this claim is a switch and earns a
-- 'credential_switch' run message. pgx.ErrNoRows means there is NO prior epoch (a first claim), so
-- no switch. NOT owner-scoped: the caller is inside the claim transaction with the run row locked,
-- and this reads the run's OWN journal by run_id — the owner-scoped read is ListRunCredentialEpochs.
SELECT run_id, claim_generation, secret_id, label, select_reason, applied_at
FROM run_credential_epochs
WHERE run_id = @run_id AND claim_generation < @claim_generation
ORDER BY claim_generation DESC
LIMIT 1;

-- name: SetRunCredentialOverride :execrows
-- Write the per-run credential override columns for `uzi run set-token` (PRD #1247 M4,
-- D4). It is the FIRST write in every writable-state branch of the verb (queued /
-- limit_wait / pool_wait / recovery_wait / paused), before the state-specific
-- transition, and is idempotent: re-writing the same override is harmless, and a
-- transition that then finds 0 rows leaves the override written but the status unchanged.
--
-- @mode and @secret_id are BOTH nullable (inherit clears both to NULL). A pinned override
-- carries a mode + id; auto/default carry the mode with a NULL id; inherit carries NULL /
-- NULL. The caller resolves them through the one validator (validateCredentialOverride)
-- FIRST, so this statement never sees an unvalidated mode. It does NOT touch status /
-- status_since — it only re-points which credential the next claim spends — so it is not
-- one of the status_since-pairing writers.
--
-- Owner-scoped (user_id): a foreign run is a 0-row no-op, exactly like the other set-token
-- writes, so the verb cannot re-point a run the caller does not own. The handler has
-- already read the run owner-scoped (GetRunByIDForUser) before reaching here, so a 0-row
-- result at this point means a concurrent delete/transfer, not a missing owner check.
UPDATE runs SET
    credential_override_mode = @mode,
    credential_override_secret_id = @secret_id,
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
    -- PRD #1226 M5: the completion-QUESTION marker is RESOLVED when the run resumes to running
    -- (the owner's continue was accepted and the worker picked up the answer), so it must not
    -- survive — the SAME "no setter leaves a resolved park marker behind" discipline as
    -- open_question_id directly above (both mark a resolved question). Cleared UNCONDITIONALLY:
    -- a running run never carries the marker, so a running → running heartbeat re-clears an
    -- already-NULL column (a no-op), and the completion-question window can only resolve to
    -- running (here) or the hold (SetRunCompletionHold).
    completion_question_at = NULL,
    -- PRD #1226 M5 (D7): the completion hold is OVER the instant the worker reports running
    -- again — a resumed completion-blocked run (paused → queued → claimed → running, or the
    -- live awaiting_input window resuming in place) is once more executing, so the hold
    -- ANNOTATIONS must not survive it. This is the D7 "clears the hold on the FIRST accepted
    -- running report": ResumePausedRun deliberately leaves them set (the decision is not yet
    -- acted on), and here the first running report clears them — the SAME "no setter leaves a
    -- resolved park marker behind" discipline as open_question_id above. Cleared
    -- UNCONDITIONALLY (not CASE'd on entry): a running → running heartbeat re-clears columns
    -- that are already NULL on a running run, so it is a no-op there, and any resume path that
    -- reaches running must end the hold.
    --
    -- ONLY hold_reason/hold_captured_head are cleared here — they are paused-run annotations,
    -- never set on a running run, so this is a no-op on a heartbeat and the real clear happens on
    -- the resume→running transition. completion_budget_exhausted_at is deliberately NOT cleared
    -- here: it is the M4/D3 one-shot SERVED steer flag, ARMED by StampCompletionBudgetExhausted on
    -- a `status='running'` run and read off the SAME running-report ACK the worker routes to the
    -- completion hold (RunDTO.CompletionBudgetExhausted). Clearing it in this statement would
    -- disarm the steer with the very report meant to carry it — SetState calls SetRunRunning and
    -- THEN re-reads the row for the ACK, so a clear here always ACKs budgetExhausted=false and the
    -- worker could never enter the hold on the server's steer. Its ONLY clear is SetRunCompletionHold
    -- (the worker actually entering the hold — the D3 "a stale ack cannot re-arm the steer" contract)
    -- or an owner decision acting on it. On a resume-from-hold it is already NULL (SetRunCompletionHold
    -- cleared it on hold entry); on a normal running heartbeat a set flag is the ACTIVE steer that MUST
    -- survive to reach the worker's ACK.
    hold_reason                    = NULL,
    hold_captured_head             = NULL,
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
    -- PRD #1226 M1 (D1): freeze the STRUCTURAL COMPLETION CONTRACT at the SAME point the
    -- AUTOPILOT path freezes milestones_frozen (the FIRST report that resolves a milestone
    -- list), IDEMPOTENTLY and by the SAME rules as the human approve path
    -- (CreateApprovePlanInput). The freeze condition is TIED to the milestone freeze: the run
    -- is INTERLOCKED (completion_contract_version IS NOT NULL), the contract is not yet frozen
    -- (completion_contract IS NULL), AND the resolved milestone source is present — the SAME
    -- 3-way COALESCE(milestones_frozen, narg, milestones_candidate) that milestones_frozen
    -- above freezes from. The milestone-source guard is LOAD-BEARING: an autopilot run reports
    -- `running` at claim time BEFORE it resolves its milestones, and without this guard that
    -- early report would freeze an EMPTY contract that a later milestone-carrying report could
    -- never correct (the contract-IS-NULL idempotency would already be spent). Postgres
    -- evaluates every SET RHS against the OLD row, so completion_contract/version and the
    -- COALESCE source here read the pre-update tuple — the same mechanism the milestones_frozen
    -- immutability COALESCE relies on. The Go caller (runningStateParams) builds
    -- @completion_contract from that same resolved source. contract_revision is set to 1 in the
    -- SAME condition so revision and contract are always frozen together, never one without the other.
    completion_contract = CASE
        WHEN completion_contract_version IS NOT NULL AND completion_contract IS NULL
             AND COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate) IS NOT NULL
        THEN sqlc.narg('completion_contract')::jsonb
        ELSE completion_contract END,
    contract_revision = CASE
        WHEN completion_contract_version IS NOT NULL AND completion_contract IS NULL
             AND COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate) IS NOT NULL
        THEN 1
        ELSE contract_revision END,
    -- PRD #122 M2 (Decision 5/5b): per-run budget derived SERVER-SIDE from the frozen
    -- milestone count at freeze, written IMMUTABLY. NULL for a 0/1-milestone run so its
    -- budget is byte-for-byte the global default. Count capped at milestone_budget_cap,
    -- wall capped at budget_wall_ceiling_seconds. Frozen source = same COALESCE as above.
    -- Issue #1181: mirror of the CreateApprovePlanInput size_class floor for the AUTOPILOT
    -- path. The count<=1 arm floors an 'l' run instead of dropping to NULL; 's'/'m'/'' stay
    -- NULL. Read COALESCE(sqlc.narg('size_class'), size_class) — this statement SETs size_class
    -- (line above) and Postgres evaluates SET RHS against the OLD row, so bare size_class would
    -- miss the size_class this self-contained running report carries at the freeze instant. The
    -- count>=2 arm is unchanged. If you change this, change CreateApprovePlanInput too.
    budget_max_iterations = COALESCE(budget_max_iterations,
        CASE WHEN COALESCE(jsonb_array_length(COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate)), 0) <= 1
                 THEN CASE COALESCE(sqlc.narg('size_class'), size_class)
                          WHEN 'l' THEN sqlc.arg('run_max_iterations')::int * sqlc.arg('size_budget_factor_l')::int
                          ELSE NULL END
             ELSE sqlc.arg('run_max_iterations')::int * LEAST(jsonb_array_length(COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate)), sqlc.arg('milestone_budget_cap')::int) END),
    budget_wall_seconds = COALESCE(budget_wall_seconds,
        CASE WHEN COALESCE(jsonb_array_length(COALESCE(milestones_frozen, sqlc.narg('milestones_frozen')::jsonb, milestones_candidate)), 0) <= 1
                 THEN CASE COALESCE(sqlc.narg('size_class'), size_class)
                          WHEN 'l' THEN LEAST(sqlc.arg('run_timeout_seconds')::int * sqlc.arg('size_budget_factor_l')::int, sqlc.arg('budget_wall_ceiling_seconds')::int)
                          ELSE NULL END
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
    -- PRD #1224 M2 (Decision 6): the per-milestone agent attribution moves WITH the
    -- validated in_progress write — keyed on the SAME milestones_in_progress narg gate, NOT
    -- its own presence. So a report that does not validly update in_progress leaves this
    -- column untouched; a report that DOES update in_progress overwrites this with the
    -- validated attribution subset the service passes ('[]' when none survive, clearing a
    -- departed lane's stale entry). The service (M3) only sets the milestones_agents param
    -- when it also writes in_progress; a nil param here with a present in_progress means
    -- "in_progress advanced, no attribution survived" → '[]'.
    milestones_agents = CASE
        WHEN sqlc.narg('milestones_in_progress')::jsonb IS NULL THEN milestones_agents
        ELSE COALESCE(sqlc.narg('milestones_agents')::jsonb, '[]'::jsonb)
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
  -- recovery_wait is the SAME shape of park (issue #1197): a reordered pre-park `running`
  -- report must not un-park a transient-recovery hold, exactly as for limit_wait/pool_wait.
  -- The negative predicate above admits it, so it is excluded explicitly here — a
  -- recovery-parked run resumes only via PromoteRecoveryWaitRuns, which lands it at 'queued'
  -- before the worker reports.
  AND status <> 'recovery_wait'
  -- paused (PRD #1190) needs the same explicit exclusion, for the same
  -- reason: a paused run's worker has EXITED (it freed its slot), so a reordered pre-park
  -- `running` heartbeat that landed after SetRunPaused would flip paused back to running
  -- under a worker that is gone, and the run would sit ownerless until RUN_TIMEOUT. The
  -- negative predicate above admits it; a resume lands it at 'queued' server-side before any
  -- worker reports, so this never blocks a legitimate resume.
  AND status <> 'paused'
  AND (status <> 'awaiting_approval' OR EXISTS (
        SELECT 1 FROM run_user_inputs
        WHERE run_user_inputs.run_id = @id
          AND run_user_inputs.kind = 'approve_plan'
          AND run_user_inputs.applied_at IS NOT NULL))
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
          AND run_user_inputs.applied_at IS NOT NULL
          AND run_user_inputs.question_id = runs.open_question_id))
  -- awaiting_followup → running is guarded the SAME way and for the same reason as
  -- awaiting_input above (PRD #517 Decision 7), as a THIRD, INDEPENDENT clause. The
  -- interactive-task park (Decision 3) holds the run in-process at `awaiting_followup`
  -- after signal_done; the worker resumes ONLY when a `follow_up` steering input has
  -- been applied (`uzi run follow-up`, Decision 4 waiter). Requiring an APPLIED
  -- follow_up is what ties the wake to the in-process worker on the current claim:
  -- the outer `worker_id = @worker_id` already pins the worker, and this clause pins the
  -- CAUSE — a delayed or duplicate PRE-PARK `running` report (the batcher retries, and
  -- the pre-gate fire-and-forget reports already exist, so reordering is not
  -- hypothetical) carries no applied follow_up, so it cannot un-park an idle task and
  -- re-arm the wall clock. Kept SEPARATE from the two clauses above, never merged into
  -- a single `status NOT IN (...) OR kind IN (...)`: that would let an applied `answer`
  -- satisfy the FOLLOWUP gate (and vice-versa), re-opening #44 F2 sideways.
  --
  -- Like awaiting_input this clause IS now keyed on a per-park identity (issue #552 M1):
  -- runs.open_followup_id, a WATERMARK of the highest follow_up the run had already
  -- applied at the moment it parked. The tie is therefore "a follow_up NEWER than the
  -- watermark was applied" — i.e. THIS park's follow_up — not "any follow_up was ever
  -- applied". Without it, on a run that has already iterated (cycle ≥2, an earlier
  -- follow_up applied) the bare EXISTS always found an applied follow_up and degraded
  -- to a no-op, so a stale pre-park `running` report un-parked an idle run: a real
  -- awaiting_followup→running STATE CHANGE (the health CASE arms fire their ELSE branch),
  -- re-arming the wall clock on a task sitting in the follow-up waiter.
  --
  -- runs.open_followup_id reads the OLD (pre-update) row here — Postgres evaluates the
  -- WHERE, and every SET right-hand side, against the pre-update tuple — exactly as
  -- runs.open_question_id does in the awaiting_input guard above. The setter's
  -- COALESCE(MAX(id),0) floor stamps 0 (never NULL) on every park, so open_followup_id is
  -- genuinely NULL only for a run that has never parked at all; that NULL COALESCEs to 0
  -- here, so any applied follow_up clears it: fail-open only in the one case where any
  -- applied follow_up genuinely IS new.
  --
  -- No clear-on-wake is needed, and a future reader must NOT "add the missing sibling
  -- clear" the way open_question_id needs one. As of issue #559 the watermark is
  -- WORKER-PROVIDED at each park (SetRunAwaitingFollowup) — the max follow_up id the
  -- worker has already DELIVERED — CLAMPED there to the server's max already-applied
  -- follow_up, with a server-derived fallback to that same max-applied when the worker
  -- omits it (old worker / first park). Either way it keys on APPLIED-only rows for its
  -- ceiling: the follow_up that wakes a park is unapplied until it wakes, so it never
  -- counts toward the watermark that guards its own park, and the next park rolls the
  -- watermark forward to include it. There is nothing to reset between parks. The guard
  -- predicate below (`id > COALESCE(open_followup_id, 0)`) is UNCHANGED by #559.
  AND (status <> 'awaiting_followup' OR EXISTS (
        SELECT 1 FROM run_user_inputs
        WHERE run_user_inputs.run_id = @id
          AND run_user_inputs.kind = 'follow_up'
          AND run_user_inputs.applied_at IS NOT NULL
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
    -- The VALUE is decided in Go (planMilestonesParam, issue #1626). On a LEGACY run
    -- a report with no milestones passes NULL and clears the candidate (the candidate
    -- reflects only the latest proposal). On an INTERLOCKED run the FIRST plan-bearing
    -- report (stored plan_md still NULL) with no milestones passes the explicit '[]' (so
    -- approval freezes an empty contract). A later milestone-less report re-presenting
    -- the SAME plan (its NUL-stripped plan_md equals the stored one) passes a stored
    -- candidate back unchanged; a revise round with a DIFFERENT plan and no milestones
    -- resets a non-empty candidate to '[]', so the superseded plan's list never freezes.
    -- Over a NULL candidate (an earlier list was rejected) the result stays NULL: the
    -- rejection is sticky and fail-closed. A rejected (invalid) list passes NULL on
    -- either kind. The immutable frozen list is untouched here (it is written at
    -- approve / by autopilot).
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
  AND status <> 'pool_wait'
  -- A stale plan report must not replace a recovery park's preserved plan/session
  -- or bypass PromoteRecoveryWaitRuns (verified by the #1197 stale-gate regression).
  AND status <> 'recovery_wait'
  -- paused IS excluded (PRD #1190), on the same reasoning as limit_wait/pool_wait above and
  -- UNLIKE awaiting_input: a paused run's only exit is a server-side resume to 'queued', so a
  -- stale/re-delivered gate report must not un-pause it into awaiting_approval under a worker
  -- that has already exited. A pause is only ever requested on a running run, so no legitimate
  -- awaiting_approval report is expected from a paused row.
  AND status <> 'paused';

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

-- name: SetRunAutopilotPlan :execrows
-- RC1 (issue #1197): durably persist an AUTOPILOT run's approved plan_md on its
-- self-contained `running` report (the autopilot gate never enters awaiting_approval,
-- so SetRunAwaitingApproval — the other plan_md writer — never runs for it). A guarded,
-- idempotent write whose affected-row count PROVES the intended plan is stored: rows>0
-- ⟺ @plan_md is now the durable plan; rows=0 ⟺ refusal (no mutation).
--
-- The four positive guards, all load-bearing (GetRunOwnedByWorker filters only
-- id+worker_id and runOwnedByWorker adds no state check, so this query is the ONLY
-- protection):
--   * status IN ('claimed','running') — the legitimate running-report source only;
--     refuses a terminal (completed/failed/cancelled) or parked (limit_wait/pool_wait/
--     recovery_wait/awaiting_*) row atomically, e.g. a concurrent cancel/park that won
--     the race. No plan_md/plan_source/updated_at mutation on a refused row.
--   * auto_approve = true — true ⟹ no human ever gated this run (auto_approve is monotone
--     true→false, cleared only at the human plan gate in SetRunAwaitingApproval). A
--     human-approved / force-gated run carries auto_approve=false and is refused.
--   * plan_source = 'agent' — positive provenance allowlist: never overwrite a
--     user-authored seeded ('seeded') plan.
--   * plan_md IS NULL OR plan_md = @plan_md — write-once, but a re-send of the SAME body
--     matches idempotently (a retry succeeds; a DIFFERENT body on an already-set row is
--     refused).
UPDATE runs SET
    plan_md     = @plan_md,
    plan_source = 'agent',
    updated_at  = now()
WHERE id = @id AND worker_id = @worker_id
  AND status IN ('claimed', 'running')
  AND auto_approve = true
  AND plan_source = 'agent'
  AND (plan_md IS NULL OR plan_md = @plan_md);

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
  AND kind <> 'judge'
  -- PRD #1247 M5a-1 rework (reviewer NB1): the per-query generation fence, the SAME nil-guarded
  -- shape as InsertRunMessage. A CAPABILITY worker stamps claim_generation on the park report;
  -- a stale report from an OLD flight — reclaimed to a NEW generation under same-worker affinity
  -- (status 'running' and worker_id both still match, so the status guard alone does NOT exclude
  -- it), or against a released claim — matches 0 rows here, so a fenced-out park cannot clobber
  -- the reclaiming flight's run. A legacy worker (NULL generation) parks unconditionally,
  -- unchanged. sqlc.narg, never @name (this file's multibyte comment blocks break the @name parser).
  -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim is
  -- rejected even for a generation-less (legacy) report; a live claim still honours a NULL generation.
  AND claim_released_at IS NULL
  AND (sqlc.narg('claim_generation')::bigint IS NULL
       OR claim_generation = sqlc.narg('claim_generation')::bigint);

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
    -- PRD #1147 F7 (defense-in-depth): revoke the per-claim Codex capability on park→queued.
    -- A promoted run has no live owner until it is re-claimed, so any capability minted for
    -- the prior claim must not survive the requeue; clearing the hash and bumping the epoch
    -- supersedes it, mirroring the claimed→queued revocation sites.
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status = 'limit_wait' AND retry_not_before <= @now
RETURNING id, user_id, status;

-- name: PromoteLimitWaitRunNow :execrows
-- The SINGLE-ROW early promote of ONE owner's limit_wait run for `uzi run set-token`
-- (PRD #1247 M4, D4). The owner chose a new credential for THIS run, so it is returned to
-- `queued` at once WITHOUT waiting for retry_not_before — that stamp gates the sweeper's
-- PromoteLimitWaitRuns pass, not this deliberate owner action, so this statement does NOT
-- carry the `retry_not_before <= @now` guard.
--
-- 🔴 ITS MUTATION SET MUST STAY EXACTLY PromoteLimitWaitRuns' SET CLAUSE, column for
-- column, and a column-parity test (store/promote_limit_wait_now_parity_test.go) pins the
-- two queries together, naming every mutated column. The reasons each column moves are
-- documented on PromoteLimitWaitRuns above and are not repeated here; the short of it:
-- started_at = NULL gives the resumed run a FRESH wall (Decision 6d) so it cannot time out
-- on its first report, budget_paused_seconds = 0 clears the pause banked against the old
-- baseline, the codex cap is revoked + epoch bumped (PRD #1147 F7), and health is reset so
-- the detector re-evaluates from the fresh status_since.
--
-- 🔴 AND IT MUST PRESERVE worker_id, session_id, last_seq, EVERY limit field,
-- limit_dead_secret_id, retry_not_before and both retry counters — none of them appear in
-- the SET clause, so they are left in place. That is load-bearing: claimExclude
-- (secretchoice.go) keeps EXCLUDING the still-dead token while its window
-- (retry_not_before) is closed, so the early-promoted run's next `auto` claim re-picks the
-- newly-chosen credential rather than the one it just exhausted. limit_wait_count is left
-- as history (this is not a new park), so RUN_LIMIT_MAX_WAITS still bounds thrash.
--
-- Owner- and status-scoped (user_id + status = 'limit_wait'): a run that moved out of
-- limit_wait between the verb's read and this write is a 0-row no-op the service surfaces
-- as a 409 (raced), and a foreign run can never be promoted.
UPDATE runs SET
    status     = 'queued',
    status_since = now(),
    started_at = NULL,
    budget_paused_seconds = 0,
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id AND status = 'limit_wait';

-- name: ListLimitWaitReeval :many
-- The duration-time auto-failover worklist (PRD #1247 M3, D8): every run STILL parked
-- in limit_wait whose window has NOT yet reopened (retry_not_before > @now) and that
-- recorded a dead credential (limit_dead_secret_id IS NOT NULL), OLDEST park first
-- (status_since ASC). It is the read half of the second limit-wait promoter that sits
-- beside PromoteLimitWaitRuns: Decision 6e extended from park-time to park-duration, so
-- a token that pools or a gauge that turns eligible AFTER the park is re-asked every
-- tick instead of the run sleeping to retry_not_before regardless.
--
-- It projects exactly what the two pure policies in Go need and no more:
-- effectiveNextClaimMode reads kind / credential_override_mode /
-- credential_override_secret_id / worker_id plus the recorded worker's bind mode, and
-- claimExclude reads limit_dead_secret_id / retry_not_before. The recorded worker's
-- anthropic_bind_mode rides a LEFT JOIN (projected as worker_bind_mode) so the Go side
-- needs no per-run worker fetch; a NULL/absent worker leaves it NULL, which
-- effectiveNextClaimMode maps to `unknown` (and unknown is skipped, the safe direction).
--
-- No promotion happens here: a due run is LOWERED to now() by LowerLimitWaitRetryNow and
-- the existing PromoteLimitWaitRuns performs the actual status transition. Backed by the
-- same idx_runs_limit_wait_retry partial index PromoteLimitWaitRuns reads, so the set is
-- empty on a healthy instance. @now is the sweep's own clock.
SELECT
    r.id,
    r.user_id,
    r.kind,
    r.credential_override_mode,
    r.credential_override_secret_id,
    r.worker_id,
    r.limit_dead_secret_id,
    r.retry_not_before,
    w.anthropic_bind_mode AS worker_bind_mode
FROM runs r
LEFT JOIN workers w ON w.id = r.worker_id
WHERE r.status = 'limit_wait'
  AND r.retry_not_before > @now
  AND r.limit_dead_secret_id IS NOT NULL
ORDER BY r.status_since ASC;

-- name: LowerLimitWaitRetryNow :execrows
-- The write half of the D8 duration-time re-evaluation pass (PRD #1247 M3): lower ONE
-- still-parked limit_wait run's retry_not_before to now() so the immediately-following
-- PromoteLimitWaitRuns pass (which the re-eval pass runs BEFORE in Sweep) brings it back
-- to queued — the same tick once that pass's clock has reached the lowered stamp, else the
-- next tick. It does NOT transition the status itself — the mutation set of the resume
-- (fresh wall, health reset, codex-cap revoke) lives in PromoteLimitWaitRuns and must not
-- be duplicated here.
--
-- Owner-scoped (user_id) and status-guarded (status = 'limit_wait') so a run that moved
-- out of limit_wait between the list and this write is a 0-row no-op, and a lowering can
-- never touch a foreign run. It writes ONLY retry_not_before + updated_at, leaving every
-- limit field, limit_dead_secret_id and both retry counters in place, so claimExclude
-- keeps its answer and the resumed claim re-picks correctly.
UPDATE runs SET retry_not_before = now(), updated_at = now()
WHERE id = @id AND user_id = @user_id AND status = 'limit_wait';

-- name: SetRunRecoveryWait :execrows
-- Park a run in the TRANSIENT-RECOVERY hold (issue #1197). running -> recovery_wait,
-- non-terminal: the run keeps its issue, its session, its worker affinity, its message
-- history and its plan_md, and the sweeper (PromoteRecoveryWaitRuns) promotes it back to
-- queued once recovery_retry_not_before passes. It is modelled CLOSELY on SetRunLimitWait
-- but is a DISTINCT park with a distinct purpose:
--
-- 🔴 recovery_wait IS NOT A USAGE LIMIT. A worker enters it after a positively-empty SDK
-- turn survived bounded in-process retries — a transient recovery, not credential
-- exhaustion. So this query MUST NOT touch any limit/credential gauge: limit_wait_count,
-- limit_resets_at, retry_not_before, rate_limit_type and limit_dead_secret_id are ALL left
-- untouched. Its own recovery_wait_count/recovery_retry_not_before are the only park
-- bookkeeping, and recovery_wait_count is a backoff SHAPER, never a cap (there is no
-- lifetime-park cap and no terminal branch — a park always becomes promotable again).
--
-- 🔴 THE SOURCE GUARD IS POSITIVE (status = 'running'), exactly as SetRunLimitWait's, and
-- for the same reasons: a negative guard would admit queued/claimed -> recovery_wait (a
-- stale report parking a run no worker holds), awaiting_approval -> recovery_wait
-- (swallowing a pending human approval), and recovery_wait -> recovery_wait (a re-delivered
-- report bumping the backoff count a second time on one event). With the positive guard
-- each is a 0-row no-op, surfaced to the worker as 409 / applied=false, so re-delivery is
-- idempotent and the worker's cleanup carve-out keys off the RETURNED STATUS.
--
-- kind <> 'judge' mirrors SetRunLimitWait (Decision 14): a judge run is executed by a
-- different runner with its own error path and is never re-enqueued.
--
-- recovery_retry_not_before is computed in GO and passed in (now + capped exponential
-- backoff + jitter; see workersvc/recoverywait.go), never derived here.
--
-- recovery_wait_count bumps HERE, in the same statement as the transition, so a run cannot
-- end up parked without its backoff shaper advancing. It is distinct from limit_wait_count
-- and requeue_count on purpose — a shared counter would let one park's budget leak into
-- another's curve.
--
-- The health reset is MANDATORY for the same reason SetRunLimitWait's is: a parked run is
-- absent from ListActiveRunsForHealth (a POSITIVE allowlist), so whatever flag was live at
-- park time would FREEZE for the whole park with nothing able to clear it. Do NOT "fix" it
-- by adding recovery_wait to that allowlist: stalled/looping/slow describe a RUNNING agent.
-- plan_md and worker_id are PRESERVED (worker_id is in the WHERE, never the SET). This is a
-- NON-TERMINAL transition, so the run's checkpoint is NOT deleted (that fires only on
-- terminal transitions).
--
-- PRD #1392 M1 (D9): this is the UNTYPED park (empty turn, and #1088's provider park once
-- it adopts recovery_wait). It CLEARS recovery_wait_cause to NULL — a later untyped park on
-- a run that forge-parked earlier must REPLACE the typed cause, not coalesce it, so its
-- surface reads the generic wording and its forge cap counter is not consulted. It does NOT
-- touch forge_park_count: that lifetime counter belongs to the forge park alone (fact 7 /
-- D2), so an empty-turn park neither increments nor resets it (a run keeps its forge-park
-- lifetime count through a later empty-turn park). The forge park has its own writer,
-- ParkRunForgeUnreachable, which sets the cause and bumps forge_park_count.
UPDATE runs SET
    status                    = 'recovery_wait',
    status_since              = now(),
    recovery_wait_count       = recovery_wait_count + 1,
    recovery_wait_cause       = NULL,
    recovery_retry_not_before = @retry_not_before,
    session_id                = COALESCE(sqlc.narg('session_id'), session_id),
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at                = now()
WHERE id = @id AND worker_id = @worker_id
  AND status = 'running'
  AND kind <> 'judge'
  -- PRD #1247 M5a-1 rework (reviewer NB1): the per-query generation fence, identical to
  -- SetRunLimitWait's and the SAME nil-guarded shape as InsertRunMessage. A stale report from an
  -- OLD flight — reclaimed to a NEW generation under same-worker affinity, or against a released
  -- claim — matches 0 rows, so a fenced-out park cannot clobber the reclaiming flight's run. A
  -- legacy worker (NULL generation) parks unconditionally, unchanged. sqlc.narg, never @name (the
  -- multibyte comment blocks break the @name parser).
  -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim is
  -- rejected even for a generation-less (legacy) report; a live claim still honours a NULL generation.
  AND claim_released_at IS NULL
  AND (sqlc.narg('claim_generation')::bigint IS NULL
       OR claim_generation = sqlc.narg('claim_generation')::bigint);

-- name: ParkRunForgeUnreachable :one
-- PRD #1392 M1 (D2/D3): the FORGE pre-clone park writer. It is the typed sibling of
-- SetRunRecoveryWait — same 'recovery_wait' transition, same backoff-shaping
-- recovery_wait_count bump, same health-trio reset and session_id COALESCE, same positive
-- source guard (status = 'running') — with two additions that are the whole point:
--
--   - recovery_wait_cause = 'forge_unreachable' — the TYPED cause the surface renders the
--     forge wording and the cap counter off of. Distinct from the empty-turn park, which
--     writes NULL (D9).
--   - forge_park_count = forge_park_count + 1 — the FORGE-ONLY lifetime counter the cap
--     (RUN_FORGE_UNREACHABLE_MAX_PARKS) decides on. Bumped HERE, in the same statement as
--     the transition, so a run cannot park without its cap counter advancing. Separate from
--     recovery_wait_count (the backoff shaper) so the empty-turn park keeps its no-lifetime-cap
--     contract (fact 7 / D2).
--
-- RETURNING * so the service (SetState's forge-park transaction) reads back the incremented
-- forge_park_count and the stamped recovery_retry_not_before for the ack. Run INSIDE the
-- park transaction after the run row is FOR UPDATE locked and its status/generation verified
-- in Go, so the positive guard here is the belt-and-braces backstop rather than the race
-- barrier (the row lock is). recovery_retry_not_before is computed in Go (now + capped
-- exponential backoff + jitter; recoverywait.go recoveryParkFallbackFor/recoveryParkJitter),
-- exactly as setRecoveryWait does.
UPDATE runs SET
    status                    = 'recovery_wait',
    status_since              = now(),
    recovery_wait_count       = recovery_wait_count + 1,
    recovery_wait_cause       = 'forge_unreachable',
    forge_park_count          = forge_park_count + 1,
    recovery_retry_not_before = @retry_not_before,
    session_id                = COALESCE(sqlc.narg('session_id'), session_id),
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at                = now()
WHERE id = @id AND worker_id = @worker_id
  AND status = 'running'
  AND kind <> 'judge'
RETURNING *;

-- name: LockOpenCustodyHoldsForRunWorkerGeneration :many
-- PRD #1392 M1 (D3): the forge park's EXACT-hold cardinality lock. Returns the ids of every
-- OPEN custody hold on @run_id at @generation held live by @worker_id, FOR UPDATE, so the
-- park transaction can (a) count them — it requires EXACTLY ONE eligible hold, because the
-- exact release (ReleaseCustodyHoldExact) trusts the supplied generation and zero/several is
-- an unsettleable custody state that must refuse the park (409 custody_unsettled) — and (b)
-- hold the row lock across the release, so a concurrent reconciler release cannot slip in
-- between the count and the release and turn a verified single hold into zero. Scoped to the
-- LIVE holder (live_worker_id), matching ReleaseCustodyHoldExact's own predicate so the
-- count and the release agree on the same rows.
SELECT id FROM recovery_custody_holds
WHERE run_id = @run_id
  AND generation = @generation
  AND live_worker_id = @worker_id::uuid
  AND state = 'open'
FOR UPDATE;

-- name: PromoteRecoveryWaitRuns :many
-- The sweeper's transient-recovery promotion pass (issue #1197): recovery_wait -> queued
-- once the clock passes recovery_retry_not_before. Backed by idx_runs_recovery_wait_retry,
-- a partial index on exactly this predicate, so the pass costs an index scan over a set
-- that is empty on a healthy instance rather than a seq scan of runs. Mirrors
-- PromoteLimitWaitRuns field-for-field.
--
-- Unlike limit_wait there is NO lifetime cap: recovery_wait_count is left in place (it
-- keeps shaping the next park's backoff) and this promotion always fires once the stamp
-- passes, so the run auto-resumes repeatedly at the capped cadence until it recovers or the
-- owner cancels (CancelRunServerSide's negative admit-set covers a recovery_wait run).
--
-- started_at = NULL so the resumed run gets a FRESH RUN_TIMEOUT wall, and
-- budget_paused_seconds = 0 so the pause banked against the OLD baseline is not
-- over-credited — both exactly as PromoteLimitWaitRuns does. NULL <= @now is UNKNOWN, so a
-- run whose recovery_retry_not_before is NULL is never promoted. Every timer-driven park
-- writes a finite stamp; both writers of cause codex_account_unavailable
-- (ParkRunCodexAccountUnavailable and ParkQueuedCodexAccountUnavailablePage) write NULL. The
-- cause fence below is what keeps this timer promoter off that cause, whatever its stamp: that
-- hold is resumed only by the account (PromoteCodexAccountWaitRun, PRD #1590 D3).
--
-- session_id, worker_id and recovery_retry_not_before are left in place as affinity/history
-- exactly as PromoteLimitWaitRuns leaves limit_wait's. A stale recovery_retry_not_before
-- cannot re-fire this statement because the status predicate has already moved.
UPDATE runs SET
    status     = 'queued',
    status_since = now(),
    started_at = NULL,
    budget_paused_seconds = 0,
    -- Revoke the per-claim Codex capability on park->queued, exactly as PromoteLimitWaitRuns:
    -- a promoted run has no live owner until re-claimed, so clearing the hash and bumping the
    -- epoch supersedes any capability minted for the prior claim.
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status = 'recovery_wait' AND recovery_wait_cause IS DISTINCT FROM 'codex_account_unavailable'
  AND recovery_retry_not_before <= @now
RETURNING id, user_id, status;

-- name: PromoteRecoveryWaitRunNow :execrows
-- The SINGLE-ROW early promote of ONE owner's recovery_wait run for `uzi run set-token`
-- (PRD #1247 M4, D4). Mirrors PromoteRecoveryWaitRuns' mutation set field-for-field, but
-- for one owner+run and WITHOUT the recovery_retry_not_before guard — the owner chose a
-- new credential, so the run is returned to `queued` at once rather than sleeping to the
-- capped recovery cadence.
--
-- started_at = NULL (fresh wall, so it cannot time out on its first report),
-- budget_paused_seconds = 0, the codex cap revoked + epoch bumped, health reset — all
-- exactly as PromoteRecoveryWaitRuns. session_id, worker_id, recovery_wait_count and
-- recovery_retry_not_before are LEFT IN PLACE as affinity/history (recovery_wait carries no
-- lifetime cap and this is not a new park). recovery_wait is not a usage limit, so there is
-- no limit_dead_secret_id to preserve here.
--
-- Owner- and status-scoped (user_id + status = 'recovery_wait'): a run that moved out of
-- recovery_wait between the verb's read and this write is a 0-row no-op the service
-- surfaces as a 409 (raced), and a foreign run can never be promoted.
UPDATE runs SET
    status     = 'queued',
    status_since = now(),
    started_at = NULL,
    budget_paused_seconds = 0,
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id AND status = 'recovery_wait'
  AND recovery_wait_cause IS DISTINCT FROM 'codex_account_unavailable';

-- name: SetRunPaused :execrows
-- Park a run on the owner's explicit request (PRD #1190 M1). running -> paused,
-- NON-TERMINAL: the run keeps its issue, its session, its worker affinity and its message
-- history, and the owner promotes it back to queued ON DEMAND via ResumePausedRun (there is
-- no server-side clock, unlike the limit park's retry_not_before). Mirrors SetRunLimitWait's
-- shape and inherits its reasons:
--
-- THE SOURCE GUARD IS POSITIVE (status = 'running'), exactly as SetRunLimitWait's is and for
-- the same reasons: only a running run holds the live worker whose park report this is, so
-- queued/claimed/awaiting_*/limit_wait/pool_wait -> paused are all 0-row no-ops the service
-- surfaces as 409 / applied=false. Re-delivery is idempotent, and the worker's cleanup
-- carve-out keys off the RETURNED status, so a refused park cleans up rather than leaking.
--
-- THE PENDING-REQUEST GUARD (pause_requested_at IS NOT NULL) is the entry-side analog of
-- ADR-1190 I3's <> 'paused' stale-report guards on SetRunRunning/SetRunAwaitingApproval: a
-- delayed 'paused' report must not park a run whose pending request was already CLEARED — by a
-- CancelPauseInput withdrawal, the pause_failed ClearPauseRequest, or a terminal transition —
-- between the worker deciding to park and this UPDATE landing. Without it a withdrawn pause
-- could still park the run on the in-flight report; with it that report is a 0-row no-op.
--
-- The pending-pause columns are CLEARED here — the request has now been CONSUMED (the run
-- reached the park it asked for), so nothing re-arms the ACK after a resume. This is one of
-- the four sites that clear them (with CancelPauseInput on withdrawal, ClearPauseRequest on a
-- failed publish, and the terminal transitions); the INVOLUNTARY parks deliberately do NOT
-- clear them, so a pending pause survives a limit/pool park and re-arms on the first running
-- report after promotion.
--
-- THE HEALTH RESET IS MANDATORY, for SetRunLimitWait's reason: ListActiveRunsForHealth is a
-- positive allowlist that never revisits a park, so a flag live at park time would freeze for
-- the whole pause. No counter is bumped: a voluntary pause spends no budget. session_id is
-- COALESCE'd (sqlc.narg) so an omitting report preserves it.
UPDATE runs SET
    status             = 'paused',
    status_since       = now(),
    session_id         = COALESCE(sqlc.narg('session_id'), session_id),
    pause_requested_at = NULL,
    pause_mode         = NULL,
    pause_after_count  = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status = 'running'
  AND pause_requested_at IS NOT NULL
  -- PRD #1497 M1 (D14/D15): an owner-pause park can never consume a system 'wall' request. The
  -- wall park is its own fenced transition (SetRunWallPark); this owner-pause report parks only a
  -- 'milestone'/'now' request, so a delayed owner-pause report cannot settle a wall request that
  -- overwrote it.
  AND pause_mode IN ('milestone', 'now');

-- name: SetRunCompletionHold :one
-- Park an OWNED, INTERLOCKED run on the completion interlock's dedicated HOLD transition
-- (PRD #1226 M4, D6). This is a SIBLING of SetRunPaused, NOT a widening of it: the owner-pause
-- park (SetRunPaused) and this completion hold are two different reasons a run reaches `paused`,
-- and each keeps its own guard so neither can be reached by the other's report. running OR
-- awaiting_input -> paused, NON-TERMINAL.
--
-- THE GUARD admits ONLY an owned interlocked run in `running` OR `awaiting_input` (unlike
-- SetRunPaused, whose source is `running` + a pending pause request) that has RECORDED AT LEAST
-- ONE completion attempt (completion_attempts > 0). awaiting_input is admitted because the M5
-- completion-question path can leave a still-live interlocked run in that status when the worker
-- decides to hold. completion_contract_version IS NOT NULL keeps a legacy run out (it never
-- interlocks). 0 rows (the guard fails) is the ACK the worker's park order reads: a non-`paused`
-- ack means the run must be RETAINED LIVE, never cleaned up.
--
-- hold_reason is set to the single reason this milestone defines, 'completion_blocked'; #1229
-- adds further reasons additively (the column is unconstrained text). hold_captured_head records
-- the EXACT head the worker captured at hold time (sqlc.narg — nullable; an empty captured head
-- is allowed via pgconv.TextOrNull).
--
-- open_question_id is CLEARED, following the "NO SETTER MAY LEAVE A RESOLVED open_question_id
-- BEHIND" convention (see SetRunRunning / SetRunAwaitingApproval): the (worker-authored, M5)
-- completion question the run may have parked on is resolved by this hold, so its id must not
-- survive. completion_question_at (the PRD #1226 M5 completion-QUESTION marker) is CLEARED for
-- the SAME reason and by the SAME convention: entering the hold resolves the completion
-- question the run was parked on, so the marker must not survive alongside a resolved
-- open_question_id. completion_budget_exhausted_at is CLEARED because the worker acting on the
-- served steer is exactly the D3 "clears it so a stale ack cannot re-arm" contract.
--
-- It deliberately does NOT clear completion_attempts / latest_completion_attempt (M5's
-- honest-state UI reads them). For a NON-wall row it does NOT touch the pending-pause columns
-- (pause_requested_at / pause_mode / pause_after_count) — this is not an owner pause, so
-- SetRunPaused's consume-the-request semantics do not apply and are left untouched.
--
-- PRD #1497 M1 (D14/D18): the EXCEPTION is a row whose pending pause was a system 'wall' request
-- overtaken by this run's first completion attempt. The completion hold WINS that race, so it
-- SETTLES the wall request in the same statement: it clears the three pause columns (the CASE on
-- the OLD pause_mode leaves a milestone/now/NULL request untouched) and the leading `consumed_wall`
-- CTE settles the run's unapplied kind='pause' body='wall' input, so a resumed flight
-- is never handed a stale wall abort. An ACKed receipt may already have consumed_at set;
-- applied_at remains NULL until worker delivery or this server settlement.
--
-- THE HEALTH RESET is mandatory for SetRunPaused's reason: ListActiveRunsForHealth is a positive
-- allowlist that never revisits a park, so a flag live at hold time would freeze for the whole
-- hold. session_id is COALESCE'd (sqlc.narg) so an omitting report preserves it.
WITH consumed_wall AS (
    UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
    WHERE u.kind = 'pause' AND u.body = 'wall' AND u.applied_at IS NULL
      AND EXISTS (
          SELECT 1 FROM runs r
          WHERE r.id = u.run_id AND r.id = @id AND r.worker_id = @worker_id
            AND r.pause_mode = 'wall'
            AND r.status IN ('running', 'awaiting_input')
            AND r.completion_contract_version IS NOT NULL
            AND r.completion_attempts > 0
            AND r.claim_released_at IS NULL
            AND (sqlc.narg('claim_generation')::bigint IS NULL
                 OR r.claim_generation = sqlc.narg('claim_generation')::bigint))
)
UPDATE runs SET
    status                         = 'paused',
    status_since                   = now(),
    session_id                     = COALESCE(sqlc.narg('session_id'), runs.session_id),
    hold_reason                    = 'completion_blocked',
    hold_captured_head             = sqlc.narg('hold_captured_head'),
    open_question_id               = NULL,
    completion_question_at         = NULL,
    completion_budget_exhausted_at = NULL,
    -- PRD #1497 M1 (D14): settle a wall request overtaken by the completion attempt; the CASE reads
    -- the OLD pause_mode, so a milestone/now/NULL pending request is left exactly as before.
    -- Outer refs are qualified `runs.` because the leading consumed_wall CTE puts run_user_inputs in
    -- the analyzer's outer name scope (bare `id`/`status` would read ambiguous — RecordCompletionAttempt).
    pause_requested_at             = CASE WHEN runs.pause_mode = 'wall' THEN NULL ELSE runs.pause_requested_at END,
    pause_mode                     = CASE WHEN runs.pause_mode = 'wall' THEN NULL ELSE runs.pause_mode END,
    pause_after_count              = CASE WHEN runs.pause_mode = 'wall' THEN NULL ELSE runs.pause_after_count END,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at                     = now()
WHERE runs.id = @id AND runs.worker_id = @worker_id
  AND runs.status IN ('running', 'awaiting_input')
  AND runs.completion_contract_version IS NOT NULL
  AND runs.completion_attempts > 0
  -- PRD #1247 M5: the per-query generation fence, the SAME nil-guarded shape as InsertRunMessage.
  -- A CAPABILITY worker stamps claim_generation on its park order; a STALE report from an OLD
  -- flight (its claim RELEASED by a held-state switch, or SUPERSEDED by a reclaim — status
  -- 'running'/'awaiting_input' and worker_id can both still match, so the guards above do not
  -- exclude it) matches 0 rows here, which the service maps to the existing applied=false path
  -- (the worker retains the reclaimed flight's run live). A legacy worker (NULL generation) holds
  -- unconditionally, unchanged. sqlc.narg, never @name (this file's multibyte comments break @name).
  -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim is
  -- rejected even for a generation-less (legacy) report; a live claim still honours a NULL generation.
  AND runs.claim_released_at IS NULL
  AND (sqlc.narg('claim_generation')::bigint IS NULL
       OR runs.claim_generation = sqlc.narg('claim_generation')::bigint)
RETURNING runs.*;

-- name: ResumePausedRun :one
-- Owner-scoped resume of ONE paused run (PRD #1190 M1): paused -> queued, the on-demand
-- counterpart to PromoteLimitWaitRuns but with the GATE-PARK accounting (Decision 2), NOT the
-- limit park's fresh-wall reset. The user_id predicate is what makes it unable to resume a
-- foreign run.
--
-- THE BUDGET RULE IS THE GATE-PARK ONE (issue #783), the OPPOSITE of PromoteLimitWaitRuns
-- (Decision 6d): started_at is KEPT (never NULL'd) and the parked wall-clock time is BANKED
-- into budget_paused_seconds, so the run resumes with exactly the remaining budget it had and
-- SweepRunningTimeout's deadline excludes the pause. A fresh wall would be an uncapped
-- extension; consuming budget would punish the owner for waiting. status_since is NOT NULL
-- (migration 00163), so the banked interval is never NULL.
--
-- session_id, last_seq, started_at and worker_id are UNTOUCHED (resume affinity: the same disk
-- + session reclaim the run if the pinned worker is alive; ADR-628's affinity leg falls open to
-- any live worker once it is stale). requeue_count is NOT bumped — a resume is not a worker
-- death. codex_cap_hash/codex_claim_epoch are revoked/bumped exactly as the other park->queued
-- transitions do (PRD #1147 F7): a queued run has no live owner until re-claimed. Health is
-- reset because the detector's allowlist includes 'queued'.
--
-- THE SOURCE GUARD IS POSITIVE (status = 'paused'): a run that is not paused is a 0-row no-op,
-- which lets the resume handler tell "not paused" (409) from "not yours / absent" (404).
-- RETURNING id, user_id, status matches PromoteLimitWaitRuns so the caller can publish the
-- resume through the broadcaster/notifier fan-out.
--
-- PRD #1497 M1 (D6/D7/D19): this resume now serves the wall park too.
--   * A 'completion_blocked' hold resumes ONLY through its own decision endpoint (D6): the
--     owner-facing/credential callers pass @allow_completion_blocked_hold = false, which refuses
--     it (0 rows); resumeCompletionBlocked passes true. The completion path leaves hold_reason set
--     (SetRunRunning clears it on the first running report, as PRD #1226).
--   * A 'budget_exhausted' wall park resumes only WITH remaining budget (D7): a bare resume of an
--     out-of-time run is refused with the extend command; @global_timeout_seconds feeds the
--     three-term total for a NULL-budget run. On resume of a budget_exhausted row hold_reason and
--     hold_captured_head are CLEARED (the CASE reads the OLD hold_reason).
--   * A SERVER-parked row (claim_released_at IS NOT NULL, only ParkRunsAtWall leaves this on a
--     paused row) resumes WITHOUT its worker and BARS that worker from reclaiming (D19): worker_id
--     is nulled so the ownership guard on every report writer rejects the old flight, and the
--     released_worker_* pair ParkRunsAtWall captured is PRESERVED (never re-read here — the park
--     has no expiry, so the worker may be a new safe incarnation by now). A worker-side park
--     (claim_released_at IS NULL) KEEPS worker_id for the same-worker resume, as PRD #1190.
-- All guards use IS DISTINCT FROM / OR so a NULL hold_reason (an owner pause) always resumes.
UPDATE runs SET
    status                = 'queued',
    status_since          = now(),
    budget_paused_seconds = budget_paused_seconds
        + GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int),
    -- D19: drop worker affinity for a server-side park; keep it for a worker-side park.
    worker_id = CASE WHEN claim_released_at IS NOT NULL THEN NULL ELSE worker_id END,
    -- D7: a resumed budget_exhausted row leaves the wall hold behind.
    hold_reason = CASE WHEN hold_reason = 'budget_exhausted' THEN NULL ELSE hold_reason END,
    hold_captured_head = CASE WHEN hold_reason = 'budget_exhausted' THEN NULL ELSE hold_captured_head END,
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at            = now()
WHERE id = @id AND user_id = @user_id
  AND status = 'paused'
  -- D6: refuse a completion hold unless the completion decision endpoint opts in.
  AND (hold_reason IS DISTINCT FROM 'completion_blocked' OR @allow_completion_blocked_hold::boolean)
  -- D7: refuse a budget_exhausted park with no remaining budget (extend is the way back).
  -- Remaining = three-term total - (elapsed active run-time) = total - ((status_since - started_at) - budget_paused_seconds).
  AND (hold_reason IS DISTINCT FROM 'budget_exhausted'
       OR (COALESCE(budget_wall_seconds, @global_timeout_seconds::int) + budget_extension_seconds + budget_finalize_seconds)
          - (GREATEST(0, EXTRACT(EPOCH FROM (status_since - started_at))::int) - budget_paused_seconds) > 0)
RETURNING id, user_id, status;

-- name: SetRunWallPark :one
-- PRD #1497 M1 (D4/D14/D15): the worker's own capture-first wall park. A NEW worker-authored,
-- FENCED transition for the `wall_park` report. running -> paused, NON-TERMINAL, hold_reason
-- 'budget_exhausted'. It admits BOTH the request-driven park (the worker dropped its turn on the
-- 'wall' steering input) and the worker's own PRE-attempt REASON_WALL trip (which arrives before any
-- request exists), so it derives NOTHING from the pending request.
--
-- THE DEADLINE IS THE SOLE AUTHORITY (D15): a pending 'wall' request is NOT sufficient, because an
-- owner extension moves the deadline while LEAVING pause_mode = 'wall' on the row. So a row that is
-- no longer past its (three-term) deadline — the extended-in-the-window case — matches 0 rows; the
-- handler answers the row's current status and the worker restarts the turn on the lifted wall.
-- completion_attempts = 0 is the D14 backstop: once a first completion attempt exists the completion
-- hold owns the row, so this refuses (0 rows) and the worker routes to routeCompletionHold instead.
--
-- THE FENCE is the tightened #1497 shape (D16): claim_released_at IS NULL is a standalone conjunct,
-- so a released OLD flight's late wall_park is rejected even without a stamped generation; a live
-- claim honours a NULL generation (legacy compatibility). hold_captured_head records the verified
-- head (sqlc.narg — NULL for the degraded park). The leading CTE consumes the wall input (D18) so a
-- resumed flight is never handed a stale abort; its EXISTS mirrors this UPDATE's WHERE. session_id is
-- COALESCE'd so an omitting report preserves it; health reset for SetRunPaused's allowlist reason.
WITH consumed_wall AS (
    UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
    WHERE u.kind = 'pause' AND u.body = 'wall' AND u.applied_at IS NULL
      AND EXISTS (
          SELECT 1 FROM runs r
          WHERE r.id = u.run_id AND r.id = @id AND r.worker_id = @worker_id
            AND r.status = 'running'
            AND r.completion_attempts = 0
            AND r.claim_released_at IS NULL
            AND r.started_at < (sqlc.arg('now')::timestamptz
                  - make_interval(secs => COALESCE(r.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                                        + r.budget_paused_seconds
                                        + r.budget_extension_seconds
                                        + r.budget_finalize_seconds))
            AND (sqlc.narg('claim_generation')::bigint IS NULL
                 OR r.claim_generation = sqlc.narg('claim_generation')::bigint))
)
UPDATE runs SET
    status             = 'paused',
    status_since       = now(),
    session_id         = COALESCE(sqlc.narg('session_id'), runs.session_id),
    hold_reason        = 'budget_exhausted',
    hold_captured_head = sqlc.narg('hold_captured_head'),
    pause_requested_at = NULL,
    pause_mode         = NULL,
    pause_after_count  = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
-- Outer refs are qualified `runs.` because the leading consumed_wall CTE puts run_user_inputs in the
-- analyzer's outer name scope (bare `id`/`status` would read ambiguous — RecordCompletionAttempt).
WHERE runs.id = @id AND runs.worker_id = @worker_id
  AND runs.status = 'running'
  AND runs.completion_attempts = 0
  AND runs.started_at < (sqlc.arg('now')::timestamptz
        - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                              + runs.budget_paused_seconds
                              + runs.budget_extension_seconds
                              + runs.budget_finalize_seconds))
  AND runs.claim_released_at IS NULL
  AND (sqlc.narg('claim_generation')::bigint IS NULL
       OR runs.claim_generation = sqlc.narg('claim_generation')::bigint)
RETURNING runs.*;

-- name: RecordWallParkCapturedHead :execrows
-- PRD #1497 M1: a late `wall_park` head after the SERVER already parked the row. The server park
-- leaves hold_captured_head NULL; when the worker's own capture later lands (it published/verified
-- after the server had already parked it), this records the head WITHOUT changing any status. It
-- checks the generation but DELIBERATELY NOT claim_released_at: that admits exactly the flight the
-- server parked (whose claim IS released) and excludes any older flight on the same worker (a lower
-- generation), and it only ever ADDS information (the work is captured after all). Idempotent:
-- hold_captured_head IS NULL means it fires at most once.
UPDATE runs SET hold_captured_head = @head, updated_at = now()
WHERE id = @id
  AND worker_id = @worker_id
  AND claim_generation = sqlc.arg('claim_generation')::bigint
  AND status = 'paused'
  AND hold_reason = 'budget_exhausted'
  AND hold_captured_head IS NULL;

-- name: ReleaseCredentialSwitch :execrows
-- PRD #1247 M5 (D3/D4/D14): the held-state credential-switch RELEASE transition. After the
-- worker's local two-phase release (quiesce -> verified capture -> teardown) it reports
-- {status:"credential_switch", claim_generation}; in ONE fenced statement this requeues the
-- held run so its next claim spends the already-written override.
--
-- THE WHERE IS THE FENCE. It requires the run still be at the reported generation
-- (claim_generation = @generation), with an UNRELEASED claim (claim_released_at IS NULL), owned
-- by the reporting worker (worker_id), and in one of the HELD states. A stale redelivery (the
-- claim was released, or a reclaim bumped the generation) matches 0 rows; the caller
-- distinguishes an already-applied/already-reclaimed redelivery (idempotent success) from an
-- unexpected state by re-reading the run (releaseCredentialSwitch).
--
-- The claim_released_at IS NULL conjunct is DEFENSE IN DEPTH, not the pin of the idempotent
-- redelivery test: both redelivery cases are already excluded INDEPENDENTLY — an already-released
-- run is 'queued' (excluded by the status IN (...) clause below), and a reclaimed run is at a
-- higher generation (excluded by claim_generation = @generation). What the conjunct alone catches
-- is the ARTIFICIAL state a released claim still in a held status (which the normal flow never
-- produces), pinned by TestReleaseCredentialSwitchReleasedConjunctLiveDB, which forces that state
-- directly and asserts a same-generation release is refused.
--
-- THE BUDGET RULE IS THE GATE-PARK/PAUSE ONE (Open Question 3, D14): started_at is KEPT and the
-- held gap is BANKED into budget_paused_seconds, exactly like ResumePausedRun — a mid-run switch
-- is the owner's choice, not an external park, so a fresh wall would let repeated switches extend
-- a run without bound. claim_released_at = now() ARMS the fence so every later report from the
-- old flight is rejected until ClaimRun reclaims and clears it. The switch stamp
-- (credential_switch_requested_at/_generation) is deliberately KEPT on this release transition — it
-- is visible as "released, awaiting reclaim" (D14). It is cleared by any TERMINAL transition now
-- (PRD #1247 D11 fix round, beside the pause-clears). The successful-APPLICATION clear at the next
-- epoch write on reclaim (D14) is NOT yet implemented — deferred to issue #1422 (M9); until it
-- lands the stamp lingers past a same-run reclaim (a stale DTO state only, no signal leak). codex
-- cap/epoch are revoked/bumped like every other park->queued transition; health is reset because
-- 'queued' is on the detector's allowlist. Status_since is NOT NULL (migration 00163), so the
-- banked interval is never NULL.
UPDATE runs SET
    status                = 'queued',
    status_since          = now(),
    claim_released_at     = now(),
    budget_paused_seconds = budget_paused_seconds
        + GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int),
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at            = now()
WHERE id = @id AND worker_id = @worker_id
  AND claim_generation = @generation
  AND claim_released_at IS NULL
  AND status IN ('running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup');

-- name: StampHeldCredentialSwitch :execrows
-- PRD #1247 M5 (D4, BLOCKING-1 rework): the held-state credential switch for the owner's
-- `uzi run set-token` verb on a run a worker currently holds (running/awaiting_*), written in ONE
-- atomic, fenced UPDATE — the override columns AND the switch stamp together. This REPLACES the
-- prior two-write path (SetRunCredentialOverride then a `id + user_id`-only stamp), which could
-- interleave with a concurrent release/reclaim/cancel/second-switch and leave the override written
-- but the stamp fenced out (or vice versa) while the verb still returned 200. The switch is
-- REQUESTED (visible as "requested", D14); the holding worker's next inputs poll (M5a-2) reads the
-- signal and drives the local release. It does NOT change status — the run stays in its held state
-- until the worker releases.
--
-- THE WHERE IS THE FENCE. Owner-scoped (user_id, so a foreign run is a 0-row no-op) AND fenced on
-- the EXACT live claim — worker_id, claim_generation = @generation, claim_released_at IS NULL, in
-- one of the held states — mirroring ReleaseCredentialSwitch / ClearCredentialSwitchByWorker.
-- @generation is the run's current claim_generation, so the switch stamp targets the CURRENT
-- claim and ReleaseCredentialSwitch's fence matches it. EXACTLY ONE row is expected; a 0-row
-- result means the run left the held state, the generation advanced, or the claim was released
-- between the caller's read and this write (a raced release/reclaim) → the caller returns
-- ErrCredentialSwitchRaced and NOTHING is written (this UPDATE matched no row).
UPDATE runs SET
    credential_override_mode       = @mode,
    credential_override_secret_id  = @secret_id,
    credential_switch_requested_at = now(),
    credential_switch_generation   = @generation,
    updated_at                     = now()
WHERE id = @id
  AND user_id = @user_id
  AND worker_id = @worker_id
  AND claim_generation = @generation
  AND claim_released_at IS NULL
  AND status IN ('running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup');

-- name: ClearCredentialSwitchByWorker :execrows
-- PRD #1247 M5 (D3/D14): a bounded capture-failure give-up (a credential_switch_failed worker
-- report) clears the pending switch stamp WITHOUT changing status — the run keeps running on its
-- current token. Fenced to the CURRENT claim: only the worker holding the exact generation, with
-- the claim NOT released and a stamp actually pending, may clear it, so a stale/superseded/foreign
-- report clears nothing. Idempotent: a second delivery affects 0 rows (already cleared).
UPDATE runs
SET credential_switch_requested_at = NULL,
    credential_switch_generation = NULL,
    updated_at = now()
WHERE id = @id
  AND worker_id = @worker_id
  AND claim_generation = @generation
  AND claim_released_at IS NULL
  AND credential_switch_requested_at IS NOT NULL;

-- name: ClearPauseRequest :execrows
-- Clear a pending pause request WITHOUT parking (PRD #1190 M1), for the worker's
-- `pause_failed` report: the worker could not publish the checkpoint, so the run STAYS running
-- and the request is withdrawn (Decision 8 — a failed publish means no park). Keyed on id AND
-- worker_id so only the worker holding the run can clear it; it does not touch status. One of
-- the four sites that clear the pending-pause columns (with SetRunPaused, CancelPauseInput and
-- the terminal transitions).
--
-- PRD #1247 M5: the per-query generation fence, the SAME nil-guarded shape as InsertRunMessage /
-- SetRunLimitWait. A CAPABILITY worker stamps claim_generation on its `pause_failed` report; a
-- STALE report from an OLD flight — its claim RELEASED by a held-state switch (claim_released_at
-- set) or SUPERSEDED by a reclaim (claim_generation advanced) — matches 0 rows here, so it can
-- NOT clear the NEW flight's pending pause request. A legacy worker (NULL generation) clears
-- unconditionally, byte-identical to before. sqlc.narg, never @name (this file's multibyte
-- comment blocks break the @name parser).
UPDATE runs SET
    pause_requested_at = NULL,
    pause_mode         = NULL,
    pause_after_count  = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  -- PRD #1497 M1 (D18): a failed owner-pause publish must not clear a SYSTEM 'wall' request that
  -- overwrote it (IS DISTINCT FROM because pause_mode is NULL on a no-pending-pause row).
  AND pause_mode IS DISTINCT FROM 'wall'
  -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim is
  -- rejected even for a generation-less (legacy) report; a live claim still honours a NULL generation.
  AND claim_released_at IS NULL
  AND (sqlc.narg('claim_generation')::bigint IS NULL
       OR claim_generation = sqlc.narg('claim_generation')::bigint);

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
--
-- completion_question_at is the PRD #1226 M5 completion-QUESTION discriminator marker. The
-- caller passes a non-NULL timestamp ONLY when the worker's report flags a COMPLETION
-- question (SetState maps req.CompletionQuestion → now()); an ordinary PRD #88 ask_user
-- clarification passes NULL, so the column stays NULL and the behavior is byte-identical to
-- the pre-marker park. It is assigned UNCONDITIONALLY (not COALESCE'd) so a re-park as an
-- ordinary question after a completion question clears a stale marker — but a resumed worker
-- re-parking on the SAME completion question re-stamps a fresh now(). completionQuestionOpen
-- (the decision endpoint's admit condition) and completionPhaseRule key on this marker, so an
-- ordinary clarification on an interlocked post-attempt run no longer reads as a completion
-- window and an owner completion-continue can never resolve the wrong question.
UPDATE runs SET
    status           = 'awaiting_input',
    status_since     = now(),
    open_question_id = @open_question_id,
    completion_question_at = sqlc.narg('completion_question_at'),
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
    -- already-applied follow_up as a safety ceiling (the LEAST(...) below). When the
    -- worker OMITS it (an old worker, or the very first park before anything was
    -- delivered) the COALESCE falls back to that same server-derived max-applied, so an
    -- absent param uses the same applied-row ceiling as the worker-provided path.
    --
    -- Why worker-provided: deriving the watermark purely from the server's max-applied
    -- races a follow_up applied DURING this park report's DB round-trip — it would fold a
    -- follow_up the worker has not yet included in its park watermark and permanently strand the run (the
    -- guard then never sees a follow_up NEWER than the watermark). The worker knows exactly
    -- which follow_ups it has applied, so it reports that; a correct worker's last-delivered
    -- id is ALWAYS ≤ max-applied, so the clamp never bites it. The clamp exists only to
    -- neutralize a buggy huge value that would otherwise strand the run forever.
    --
    -- A later follow_up with a higher id — the one the resuming worker will apply to wake
    -- THIS park — is what SetRunRunning's Decision-7 guard requires to admit
    -- awaiting_followup → running, so the watermark discriminates "THIS park's follow_up"
    -- from "any follow_up ever applied".
    --
    -- APPLIED-only (applied_at IS NOT NULL) remains load-bearing on the server ceiling:
    -- the follow_up that wakes a park is UNAPPLIED until it wakes, so an applied-only MAX
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
    -- bigserial id, so any applied follow_up wakes it — reopening #558 for that run).
    -- Flooring to 0 maps it to "nothing applied" (the first-park value), matching the
    -- stated "neutralize a buggy value" intent. GREATEST(0, ...) never affects a correct
    -- worker: its last-delivered id is always ≥ 0.
    -- Issue #817: floor the SET at the run's currently-stored value so the watermark
    -- can never REGRESS. The RHS `open_followup_id` reads the PRE-UPDATE (old) row —
    -- the same self-referential SET-RHS pattern this file already uses for
    -- milestones_completed (see SetRunRunning and SetRunCompleted). Strand-free: every
    -- GREATEST operand is ≤ the run's MAX(applied follow_up id), which is monotone
    -- non-decreasing, and the unapplied wake follow_up has id > that max, so
    -- `id > open_followup_id` always still holds. SAFETY DEPENDS on run_user_inputs
    -- being append-only and applied_at set-once: a retention/pruning job that
    -- hard-deletes applied follow_up rows would let MAX(applied) drop below a prior
    -- stamp, and this floor — unlike the pre-fix pure-LEAST clamp — would then hold the
    -- watermark too high; such a change must reckon with the wake guard.
    open_followup_id = GREATEST(0, COALESCE(open_followup_id, 0), LEAST(
        COALESCE(sqlc.narg('open_followup_id')::bigint,
                 (SELECT COALESCE(MAX(id), 0) FROM run_user_inputs
                  WHERE run_user_inputs.run_id = @id AND kind = 'follow_up' AND applied_at IS NOT NULL)),
        (SELECT COALESCE(MAX(id), 0) FROM run_user_inputs
         WHERE run_user_inputs.run_id = @id AND kind = 'follow_up' AND applied_at IS NOT NULL))),
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
    -- PRD #1224 M2 (Decision 7): the per-milestone agent attribution rides the SAME
    -- terminal clear as in_progress above — dead run, no in-progress lanes, no attribution.
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause. Clearing the three
    -- pause columns on every terminal transition makes the design true at the root:
    -- a run that completes, fails or is cancelled while carrying an owner's pending
    -- pause never leaves a stale "pause requested" ACK/chip on a dead run. A no-op
    -- for a run with no pending pause (the columns are already NULL). The other
    -- terminal writers (SetRunFailed / MarkRunFailedByID / CancelRunServerSide /
    -- CancelRunByWorker / FailRunAutoStop / RejectRunServerSide / SweepRunningTimeout
    -- and the stale-worker failers below) clear them for the same reason.
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
    -- Arm the M5 patch marker. Explicit rather than left to the column default,
    -- because SetRunCompleted can in principle run on a row that already carries a
    -- stamp from an earlier terminal transition.
    prd_patch_settled_at = NULL,
    -- issue #329: a completion that supersedes a run_timeout failure must clear the
    -- stale timeout classification. No-op on a normal completion (both already NULL).
    failure_reason     = NULL,
    fail_origin        = NULL,
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END,
    finished_at        = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  -- PRD #1497 M1 (DEVIATION-3): a legacy (nil-generation, non-interlocked) `completed` from an
  -- old flight must never complete a run the wall-park sweep just server-parked. ParkRunsAtWall
  -- leaves the row status='paused' with claim_released_at set and KEEPS worker_id (informational),
  -- so a stale worker_id-only completion would otherwise satisfy the guard below and walk the park
  -- back to 'completed'. SetState's Go wrapper fences a generation-STAMPING report, but a
  -- generation-less legacy report is honoured by that nil-guarded fence and the best-effort Go
  -- status check in SetState is a TOCTOU — so the guard lives in SQL, mirroring SetRunRunning's
  -- own `status <> 'paused'` exclusion. A live legacy `completed` always runs on a running,
  -- non-released row (the interlocked path is completeRunWithPermit, fenced on its own locked row),
  -- so this never blocks a legitimate completion.
  AND status <> 'paused' AND claim_released_at IS NULL
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
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END,
    finished_at        = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled')
  -- PRD #1247 M5a-1 rework (m6): the per-query generation fence, the SAME nil-guarded shape as
  -- UpdateRunLastSeq/InsertRunMessage. limit_wait (non-park + forge-park DEGRADED) callers skip
  -- the outer FOR UPDATE fence, so when a generation is supplied the fail applies ONLY to the
  -- still-held run at that exact generation: a late gen-G report matches 0 rows against a run
  -- released after G (claim_released_at set) or reclaimed to G+1 (generation moved on), so it
  -- cannot clobber the reclaiming flight. nil = legacy/outer-lock-fenced callers, unchanged.
  -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim is
  -- rejected even for a generation-less (legacy) report; a live claim still honours a NULL generation.
  AND claim_released_at IS NULL
  AND (sqlc.narg('claim_generation')::bigint IS NULL
       OR claim_generation = sqlc.narg('claim_generation')::bigint);

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
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END,
    finished_at        = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
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
UPDATE runs SET status = 'cancelled', status_since = now(), stop_kind = 'cancelled', move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END, finished_at = now(),
    -- PRD #503 M3: persist the operator's OPTIONAL cancel reason. @stop_reason binds a
    -- nullable pgtype.Text: an invalid/zero value stores NULL (no reason supplied).
    stop_reason = @stop_reason,
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: RunHasPendingOutcomeLease :one
-- PRD #1391 Run B M3d (D13): does this run currently have a terminal outcome journaled and
-- leased on its owning worker? This is the POSITIVE form of the D11 claim-exclusion predicate
-- (see SweepClaimedNeverStarted / FailRunAutoStop, whose negative `NOT EXISTS(...) AND NOT
-- EXISTS(...)` PROTECT such a run). True when EITHER the run's owning worker holds an unexpired
-- terminal_pending lease for it at the run's EXACT current claim_generation (the executor
-- journaled a terminal outcome and is gone), OR the owning worker is under an unexpired
-- pending_overflow (M4's rotation left this run unlisted, but the worker-level closure stands in
-- for the missing row-level lease). Chat is excluded (D6/D10): chat has no claim generation and
-- never journals a terminal outcome, so it is never pending. Reused by hasLivePoller (a run this
-- returns true for has no live poller for ITSELF — its executor no longer exists) and by the
-- owner cancel's confirmation gate + atomic no-live-poller branch.
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE runs.id = @id
      AND runs.kind <> 'chat'
      AND (EXISTS (SELECT 1 FROM worker_active_runs a
                   WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                     AND a.terminal_pending_until > now()
                     AND a.claim_generation = runs.claim_generation)
           OR EXISTS (SELECT 1 FROM workers w
                      WHERE w.id = runs.worker_id AND w.pending_overflow_until > now()))
);

-- name: CancelRunServerSideWithPendingOutcome :execrows
-- PRD #1391 Run B M3d (D13): the atomic owner-scoped cancel of a run whose executor journaled a
-- terminal (esp. blocked) outcome on its worker, when the owner explicitly discards it. This is
-- the resolution for a pending outcome the api permanently refuses — a silent unconditional
-- CancelRunServerSide would trade a visible stall for a lost outcome, so this variant carries the
-- SAME pending-outcome predicate the confirmation gate enforced (RunHasPendingOutcomeLease's
-- positive form) INSIDE the UPDATE: a Go-side check followed by the plain CancelRunServerSide
-- would race a lease clear or a re-claim and cancel a fresh generation on stale evidence. The
-- UPDATE is one row-locking statement (it re-evaluates the predicate against the latest committed
-- run row under READ COMMITTED / EvalPlanQual), so a replayed SetState and this cancel resolve on
-- the run's row lock, not on stale reads: if the replayed SetState commits `completed`/`failed`
-- first, this matches 0 rows (status NOT IN protects it); if this wins, the replay's no-op 409
-- returns `cancelled`, the journal retires and completion side effects never fire. Field-for-field
-- identical to CancelRunServerSide's terminal cleanup; only the WHERE differs.
UPDATE runs SET status = 'cancelled', status_since = now(), stop_kind = 'cancelled', move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END, finished_at = now(),
    stop_reason = @stop_reason,
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.id = @id AND runs.user_id = @user_id
  AND runs.status NOT IN ('completed', 'failed', 'cancelled')
  AND runs.kind <> 'chat'
  AND (EXISTS (SELECT 1 FROM worker_active_runs a
               WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                 AND a.terminal_pending_until > now()
                 AND a.claim_generation = runs.claim_generation)
       OR EXISTS (SELECT 1 FROM workers w
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now()));

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

-- name: NewestRunForMR :one
-- The single newest run uzi opened against a (repo, MR), any kind and any status.
-- The forge-view pulls list joins each open PR to its newest uzi run through this
-- (repo_id, mr_iid) lookup so a `↳ run` link needs no second query (PRD #1255 D4).
-- Unlike GetActiveMRReworkRunForMR above it drops the kind/status filters — the newest
-- run of ANY kind is the link. The sole caller (newestRunIDForMR) needs only that newest
-- row, and ListPulls repeats this lookup per open PR, so it is bounded to one row with
-- LIMIT 1 rather than materializing every matching run. The (repo_id, mr_iid) index
-- (migration 00167) serves the filter; no-row is pgx.ErrNoRows, mapped to "no link".
SELECT * FROM runs
WHERE repo_id = @repo_id::uuid AND mr_iid = @mr_iid
ORDER BY created_at DESC
LIMIT 1;

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
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END,
    finished_at        = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SupersedeRunByWorker :execrows
-- Issue #1117: a LIVE mr_rework worker whose finalize push was rejected non-fast-forward
-- because a concurrent same-branch writer (a human / uzi-watcher landing review fixes)
-- advanced the MR branch under it reports `failed` + branch_moved. SetState's failed arm
-- routes HERE (guarded on kind='mr_rework') instead of SetRunFailed, so a benign, expected
-- race is NOT mis-classified as 'agent_failure' (and is not judged — status 'cancelled',
-- Gate 0). Distinct from CancelRunByWorker in that it STAMPS stop_kind='branch_moved' +
-- a static stop_reason in the same statement (branch_moved has no pre-stamp, unlike a
-- CreateStopVerdictInput cancel). Terminal cleanup + guard mirror CancelRunByWorker, so a
-- report onto an already-terminal run is a 0-row no-op.
UPDATE runs SET
    status             = 'cancelled',
    stop_kind          = 'branch_moved',
    stop_reason        = 'The MR branch was advanced by a concurrent writer, so this rework was superseded and not applied. The branch and the concurrent commits are intact.',
    status_since       = now(),
    fail_origin        = NULL,
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END,
    finished_at        = now(),
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
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
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END,
    finished_at        = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE runs.id = @id
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
  AND status <> 'pool_wait'
  -- recovery_wait (issue #1197) needs the same guard: a
  -- recovery-parked run's writes have STOPPED (the worker parked after a positively-empty
  -- turn, awaiting the server-owned backoff promotion), not looped, so auto-stopping one
  -- would be wrong on the merits. It is excluded today only by autostop.go's single
  -- `if run.Status != "running"` line, unmentioned in that line's comment, and exposed the
  -- day someone relaxes it — so this is its SQL backstop.
  AND status <> 'recovery_wait'
  -- paused (PRD #1190) needs the same guard: a
  -- paused run's writes have STOPPED (the worker parked on the owner's request and freed its
  -- slot), not looped, so auto-stopping one would be wrong on the merits. It is excluded today
  -- only by autostop.go's single `if run.Status != "running"` line, unmentioned in that line's
  -- comment, and exposed the day someone relaxes it — so this is its SQL backstop.
  AND status <> 'paused'
  -- PRD #1390 D11: the persist-failure auto-stop honours the terminal-pending lease +
  -- pending_overflow closure like every other terminal writer — a run whose outcome is
  -- journaled on the worker (#1391) must not be auto-stopped out from under the pending
  -- replay while its lease holds. Chat is exempt from the PROTECTION (D10):
  -- `kind = 'chat' OR NOT EXISTS(...)`.
  AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())));

-- name: RejectRunServerSide :execrows
-- Server-side plan rejection → failed → origin restore → stamp. stop_kind is
-- stamped 'plan_rejected' in the same statement as the status/failure_reason write
-- (PRD #33 Decision 3), so this failed run is recognised as a deliberate stop
-- regardless of the failure_reason text.
UPDATE runs SET status = 'failed', status_since = now(), stop_kind = 'plan_rejected',
    -- PRD #69 M7a: trusted failure class, overlapping stop_kind deliberately (see 00126).
    fail_origin = 'plan_rejected',
    failure_reason = @failure_reason, move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END, finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
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
--
-- PRD #1247 M5 (D3): FENCED like InsertRunMessage. When the caller carries a claim generation
-- (@claim_generation NOT NULL) the high-water bump lands ONLY while the run is at that
-- generation with an unreleased claim, so a released/reclaimed old flight cannot advance
-- last_seq (which would strand the reclaiming flight's re-emitted seqs behind a stale mark). A
-- legacy caller (NULL) advances unconditionally, byte-identical to before.
UPDATE runs SET last_seq = GREATEST(last_seq, @seq), last_activity_at = now()
WHERE id = @id
  -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim is
  -- rejected even for a generation-less (legacy) report; a live claim still honours a NULL generation.
  AND claim_released_at IS NULL
  AND (sqlc.narg('claim_generation')::bigint IS NULL
       OR claim_generation = sqlc.narg('claim_generation')::bigint);

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
    -- Codex claim-capability revocation (PRD #1147 M2): this is the sweeper's
    -- claimed→queued path (RequeueClaimedRunToQueued mirrors it) — losing the claim
    -- must revoke the per-claim capability here too, or a run swept back to queued
    -- would keep a live cap_hash replayable on its next claim. Clear the hash and
    -- bump the epoch; a harmless no-op for non-codex runs (cap_hash already NULL).
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    updated_at = now()
WHERE status = 'claimed' AND claimed_at < @cutoff
  -- PRD #1390 D11: a `claimed` run whose initial `running` report never landed while its worker
  -- journaled a terminal outcome and leased it (or whose owner is pending_overflow) must not be
  -- reset to `queued` — that would invite a re-claim that overtakes the pending replay. Chat is
  -- exempt from the PROTECTION (D10): `kind = 'chat' OR NOT EXISTS(...)`.
  AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
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
    -- Codex claim-capability revocation (PRD #1147 M2): losing the claim revokes the
    -- per-claim capability immediately — clear the hash and bump the epoch so a stale
    -- capability minted under this claim can never be replayed. A harmless no-op for a
    -- non-codex run, whose codex_cap_hash is already NULL.
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    updated_at = now()
WHERE id = @id AND status = 'claimed';

-- name: ParkRunCodexAccountUnavailable :one
-- Called under the exact-claim row lock, after settling this generation's hold.
-- recovery_retry_not_before is cleared: this cause is resumed by the account, never by the
-- timer (PromoteRecoveryWaitRuns skips it). The SET list is
-- ParkQueuedCodexAccountUnavailablePage's (the other writer of this cause) plus one extra
-- column: this exact-claim park also rewrites worker_id to the newest open custody holder (D4),
-- while the queued park leaves worker_id alone.
UPDATE runs SET
    status = 'recovery_wait', recovery_wait_cause = 'codex_account_unavailable',
    status_since = now(), recovery_retry_not_before = NULL,
    started_at = NULL, budget_paused_seconds = 0,
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    worker_id = COALESCE((
        SELECT h.live_worker_id FROM recovery_custody_holds h
        WHERE h.run_id = runs.id AND h.state = 'open'
        ORDER BY h.generation DESC, h.created_at DESC LIMIT 1
    ), runs.worker_id),
    updated_at = now()
WHERE runs.id = @id AND runs.worker_id = @worker_id AND runs.claim_generation = @claim_generation
  AND runs.status = 'claimed' AND runs.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
RETURNING *;

-- name: ParkQueuedCodexAccountUnavailablePage :one
-- PRD #1590 M2 (D2, A1): the park_codex_account_unavailable sweeper pass. Moves QUEUED Codex
-- subscription runs that ClaimRun's account gate excludes to recovery_wait with cause
-- codex_account_unavailable, so the hold is visible instead of an invisible queued wait.
--
-- BOUNDED BY ROWS EXAMINED, not rows updated. `page` is a MATERIALIZED keyset page over the
-- partial index idx_runs_codex_sub_queued (00253): at most @page_cap queued Codex subscription
-- runs with id > @after_id, in id order. The LIMIT applies BEFORE the alias/account predicate
-- is evaluated, so a large queue in which few runs are gated still costs one page per tick; the
-- service advances its in-memory cursor to last_scanned_id and wraps to the start (uuid.Nil)
-- once a page comes back shorter than the cap.
--
-- `locked` takes the page's still-queued rows FOR UPDATE SKIP LOCKED: a row a concurrent
-- claim or writer holds is skipped this tick and retried after the cursor wraps. The UPDATE
-- then rechecks status='queued', the hold-opening kinds and the shared account predicate (the
-- byte-identical copy of ClaimRun's gate, pinned by a store test) on the locked row.
--
-- The SET list is the M1 exact-claim park's, minus the worker_id affinity rewrite: a queued run
-- has no claim of this pass's making, so claim_generation and worker_id are left alone and no
-- custody hold is opened or released. Both writers clear recovery_retry_not_before because this
-- cause is resumed by the account, never by the timer (PromoteRecoveryWaitRuns skips it).
--
-- Returns one row even when nothing parks: page_size (rows examined), last_scanned_id (the nil
-- uuid on an empty page) and the parked ids.
WITH page AS MATERIALIZED (
    SELECT runs.id FROM runs
    WHERE runs.status = 'queued' AND runs.harness = 'codex' AND runs.codex_auth_mode = 'subscription'
      AND runs.id > @after_id::uuid
    ORDER BY runs.id
    LIMIT @page_cap::int
),
locked AS (
    SELECT runs.id FROM runs
    WHERE runs.id IN (SELECT page.id FROM page) AND runs.status = 'queued'
    ORDER BY runs.id
    FOR UPDATE SKIP LOCKED
),
parked AS (
    UPDATE runs r SET
        status = 'recovery_wait', recovery_wait_cause = 'codex_account_unavailable',
        status_since = now(), recovery_retry_not_before = NULL,
        started_at = NULL, budget_paused_seconds = 0,
        codex_cap_hash = NULL, codex_claim_epoch = r.codex_claim_epoch + 1,
        health = 'ok', health_reason = NULL, health_since = NULL,
        updated_at = now()
    WHERE r.id IN (SELECT locked.id FROM locked)
      AND r.status = 'queued'
      AND r.harness = 'codex'
      AND r.codex_auth_mode = 'subscription'
      AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
      AND EXISTS (
          SELECT 1 FROM codex_credential_state ccs
          LEFT JOIN codex_provider_account cpa
              ON cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id
          WHERE ccs.user_secret_id = r.codex_secret_id
            AND ccs.user_id = r.user_id
            AND (
                (cpa.coord_state = 'quarantined'
                 AND cpa.credential_revision = r.codex_account_revision
                 AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                          THEN r.codex_account_key::jsonb
                               = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                          ELSE false END)
                OR (r.codex_account_key IS NOT NULL
                    AND ccs.material_revision > r.codex_material_revision
                    AND (ccs.status IN ('staging', 'failed')
                         OR (ccs.status = 'linked'
                             AND cpa.credential_revision = r.codex_account_revision
                             AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                                      THEN r.codex_account_key::jsonb
                                           = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                                      ELSE false END)))
            )
      )
    RETURNING r.id
)
SELECT (SELECT count(*) FROM page)::bigint AS page_size,
       COALESCE((SELECT page.id FROM page ORDER BY page.id DESC LIMIT 1),
                '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS last_scanned_id,
       COALESCE((SELECT array_agg(parked.id ORDER BY parked.id) FROM parked), '{}')::uuid[] AS parked_ids;

-- name: ListCodexAccountWaitRunsPage :many
-- PRD #1590 M3 (D3): one keyset page of the promote_codex_account_available pass. The ids of at
-- most @page_cap runs held in recovery_wait with cause codex_account_unavailable, with id past
-- the service's in-memory cursor, in id order, over the partial index
-- idx_runs_codex_account_wait (00253). A pure read: every decision is re-made per run under
-- lock (LockCodexAccountWaitRunForUpdate and the alias/account FOR SHARE NOWAIT pair), so a run
-- that moved after this list is simply skipped.
SELECT id FROM runs
WHERE status = 'recovery_wait' AND recovery_wait_cause = 'codex_account_unavailable'
  AND id > @after_id::uuid
ORDER BY id
LIMIT @page_cap::int;

-- name: LockCodexAccountWaitRunForUpdate :one
-- PRD #1590 M3 (D3): the first lock of the per-run promotion transaction, taken BEFORE the
-- alias and then the account (run -> alias -> account). Status and cause are re-checked here,
-- so a run cancelled or promoted since the page was listed is pgx.ErrNoRows. SKIP LOCKED: the
-- sweeper never waits on a run row another transaction holds (a cancel, say); that run is
-- retried on a later tick. No writer in the re-login path touches the run row. NO KEY UPDATE,
-- the level PromoteCodexAccountWaitRun's own UPDATE takes: it changes no column a foreign key
-- can reference, so the lock need not block FOR KEY SHARE (a child row's FK check).
SELECT * FROM runs
WHERE id = @id AND status = 'recovery_wait' AND recovery_wait_cause = 'codex_account_unavailable'
FOR NO KEY UPDATE SKIP LOCKED;

-- name: PromoteCodexAccountWaitRun :execrows
-- PRD #1590 M3 (D3): the account-driven promotion recovery_wait -> queued, run inside the
-- per-run transaction after evalCodexReleasePredicate passed on a GetRunCodexAuthContext read
-- taken under the run, alias and account locks. The SET list is PromoteRecoveryWaitRuns' field
-- for field (fresh RUN_TIMEOUT wall, banked pause cleared, capability revoked and epoch bumped,
-- health reset); worker_id, session_id and recovery_wait_count stay as affinity and history,
-- as there. Fenced on status and the cause, so it can promote only this hold.
UPDATE runs SET
    status     = 'queued',
    status_since = now(),
    started_at = NULL,
    budget_paused_seconds = 0,
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND status = 'recovery_wait' AND recovery_wait_cause = 'codex_account_unavailable';

-- name: FailClaimAssemblyExact :execrows
-- Terminal claim assembly failure, fenced to the same locked claim (PRD #1590 D2). The
-- SET list mirrors MarkRunFailedByID field for field, including #1482's card-only
-- move_pending_since stamp (a card-less judge or prompt run keeps it NULL). It adds only
-- the claim-capability revoke and the exact-claim WHERE.
UPDATE runs SET
    status = 'failed', status_since = now(), failure_reason = @failure_reason,
    fail_origin = @fail_origin,
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END,
    finished_at = now(),
    milestones_in_progress = NULL, milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    updated_at = now()
WHERE id = @id AND worker_id = @worker_id AND claim_generation = @claim_generation
  AND status = 'claimed';

-- name: RequeueClaimAssemblyExact :execrows
-- PRD #1590 D2: the transient claim-assembly outcomes (vault locked, custom-model capability
-- missing, empty auto pool), fenced to the exact locked claim. @pool_wait=false is the
-- RequeueClaimedRunToQueued field set (claimed -> queued, started_at kept). @pool_wait=true
-- is the only writer of the empty-pool hold (PRD #754 M4): claimed -> pool_wait, a
-- non-terminal, non-locking hold that M5 resumes. The pool_wait arm:
--   - gives a later resume a FRESH RUN_TIMEOUT wall (started_at NULL, Decision 6d) and clears
--     the pause banked against the old baseline (budget_paused_seconds 0, issue #783);
--   - is NOT a usage park (Decision 9): limit_wait_count, limit_resets_at, retry_not_before
--     and rate_limit_type are untouched, and limit_dead_secret_id is kept because M3's
--     exclude-relax reads it on resume;
--   - never holds a judge (`kind <> 'judge'`, Decision 14).
-- Both arms keep the POSITIVE source guard (status = 'claimed'), revoke the claim's Codex
-- capability (PRD #1147 F7), reset health (the status itself is the signal; never write a
-- sentence into health_reason) and keep worker_id for resume affinity.
UPDATE runs SET
    status = CASE WHEN @pool_wait::boolean THEN 'pool_wait' ELSE 'queued' END,
    status_since = now(),
    started_at = CASE WHEN @pool_wait::boolean THEN NULL ELSE started_at END,
    budget_paused_seconds = CASE WHEN @pool_wait::boolean THEN 0 ELSE budget_paused_seconds END,
    health = 'ok', health_reason = NULL, health_since = NULL,
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    updated_at = now()
WHERE id = @id AND worker_id = @worker_id AND claim_generation = @claim_generation
  AND status = 'claimed'
  AND (NOT @pool_wait::boolean OR kind <> 'judge');

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
--
-- limit_dead_secret_id and retry_not_before are projected so the reactive pass can ask
-- the SAME question the re-claim asks — autoselect.Floor(cands, claimExclude(run)) —
-- rather than the exclude-blind PoolNonEmpty it once used (PRD #1247 M3, the pool-
-- promoter fix). Early promotion (the set-token verb and D8) can leave a pool_wait run
-- carrying a FUTURE retry_not_before whose sole pooled token is its own dead credential;
-- without these two columns the pass would resume it every tick only for it to re-hold.
SELECT id, user_id, status_since, limit_dead_secret_id, retry_not_before FROM runs
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
    -- PRD #1147 F7 (defense-in-depth): revoke the per-claim Codex capability on
    -- pool_wait→queued. A promoted run has no live owner until it is re-claimed, so any
    -- capability minted for the prior claim must not survive the requeue; clearing the hash
    -- and bumping the epoch supersedes it, mirroring the claimed→queued revocation sites.
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at   = now()
WHERE id = @id AND user_id = @user_id
  AND status = 'pool_wait';

-- name: SweepTaskNeverDispatched :many
-- PRD #400 Decision 6 orphan reaper (issue #1367): a kind='task' (handoff) run is created
-- status='queued' with dispatched_at NULL and is claimable only once the CLI seeds its
-- uzi/task/<id> branch and stamps dispatched_at (DispatchTaskRun). If the push or the dispatch
-- call never lands (push failure, dispatch UPDATE never commits, client crash/SIGKILL), the row
-- is queued+undispatched forever — ClaimRun never offers it (kind<>'task' OR dispatched_at IS
-- NOT NULL) and no other sweep touches it. Past the dispatch grace window, terminalize it.
-- No D11 terminal-pending-lease carve-out is needed: an undispatched task was never claimed
-- (worker_id NULL, no worker_active_runs row), so the lease/pending_overflow predicates are
-- vacuously false. status='queued' AND dispatched_at IS NULL makes this the single winner against
-- a racing DispatchTaskRun (which now also guards status='queued'): exactly one conditional
-- UPDATE matches the row.
UPDATE runs SET status = 'failed', status_since = now(), failure_reason = @failure_reason,
    fail_origin = 'task_undispatched',
    finished_at = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag or in-progress snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE kind = 'task' AND status = 'queued' AND dispatched_at IS NULL AND created_at < @cutoff
RETURNING id, user_id, status;

-- PRD #1497 M1: the wall-clock sweep no longer FAILS a run at its deadline — it PARKS it. The old
-- SweepRunningTimeout (`UPDATE runs SET status='failed' ... fail_origin='run_timeout'`) is REPLACED
-- by two statements over the SAME per-run deadline: RequestWallParks (file a system pause request on
-- a run whose worker is alive and speaks the wall-park protocol, so the worker drops the turn and
-- captures the tree) and ParkRunsAtWall (park the row server-side when the worker is dead, incapable,
-- or unresponsive). Neither ever stamps 'failed'. The reasoning the old statement carried about the
-- per-run interval, the 8h ceiling and the #1189 extension term is preserved below and still holds.
--
-- The DEADLINE is now the THREE-TERM total (PRD #1497): COALESCE(budget_wall_seconds,
-- global_timeout_seconds) + budget_paused_seconds + budget_extension_seconds + budget_finalize_seconds.
--   * Per-run interval (PRD #122 M2 Decision 5b): a scaled-budget run carries budget_wall_seconds;
--     a NULL-budget run falls back to global_timeout_seconds (RUN_TIMEOUT). Computed against now so
--     the per-run interval applies in SQL.
--   * The 8h wall CEILING (budget_wall_ceiling_seconds) is NOT re-applied here — it is enforced by
--     the freeze WRITERS (SetRunRunning / CreateApprovePlanInput). This consumer trusts
--     budget_wall_seconds as an already-capped, server-only, IMMUTABLE value.
--   * budget_extension_seconds (PRD #1189) and budget_finalize_seconds (PRD #1497 Stop allowance)
--     are HUMAN/SYSTEM-granted additive terms OUTSIDE the ceiling, both NOT NULL DEFAULT 0.
--   * budget_paused_seconds (issue #783) EXCLUDES time parked at a human gate, so gate-wait does not
--     consume the implementation budget. NOT NULL DEFAULT 0.
--
-- Kind exemptions (unchanged): chat and judge are exempt (a chat parks long between turns, a judge
-- carries no wall budget), and interactive task runs are exempt (`interactive = false`) because an
-- interactive task is user-paced and its original started_at is past the wall on every resume.

-- name: RequestWallParks :many
-- File a SYSTEM-authored pause request (mode 'wall') on each past-deadline, timed, running run whose
-- worker is ALIVE (a heartbeat inside worker_stale_cutoff) AND advertises 'wall_park_v1', and that
-- does not already carry a 'wall' request. In the SAME statement it inserts the kind='pause'
-- body='wall' steering input (the CreatePauseInput shape) the worker drains within seconds to drop
-- its turn and take the capture-first park. It OVERWRITES a pending owner 'milestone'/'now' request
-- (both park; the wall wins the wording). No kind allowlist beyond the sweep's own — mr_rework and
-- ci_fix are requested too (D10).
--
-- The #1226 completion-interlock carve-out is KEPT (AND NOT (completion_attempts > 0 AND live
-- worker)): a post-attempt live row is left for StampCompletionBudgetExhausted, not wall-requested.
-- The two #1390 terminal-pending guards are KEPT: a journaled-outcome run is not disturbed.
WITH requested AS (
    UPDATE runs SET
        pause_requested_at = now(),
        pause_mode         = 'wall',
        pause_after_count  = NULL,
        updated_at         = now()
    WHERE status = 'running'
      AND started_at < (sqlc.arg('now')::timestamptz
            - make_interval(secs => COALESCE(budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                                  + budget_paused_seconds
                                  + budget_extension_seconds
                                  + budget_finalize_seconds))
      AND kind NOT IN ('chat', 'judge')
      AND interactive = false
      -- idempotent across ticks: a row already carrying a 'wall' request is not re-requested.
      AND pause_mode IS DISTINCT FROM 'wall'
      -- worker ALIVE and speaks the wall-park protocol.
      AND worker_id IN (
          SELECT w.id FROM workers w
          WHERE w.last_heartbeat_at IS NOT NULL
            AND w.last_heartbeat_at >= sqlc.arg('worker_stale_cutoff')::timestamptz
            AND 'wall_park_v1' = ANY(w.protocol_capabilities)
      )
      -- PRD #1226 M3 (D3): completion-interlock carve-out (KEPT). A post-attempt run with a LIVE
      -- worker is spared here and steered by StampCompletionBudgetExhausted instead.
      AND NOT (
          completion_attempts > 0
          AND worker_id IN (
              SELECT w.id FROM workers w
              WHERE w.last_heartbeat_at IS NOT NULL
                AND w.last_heartbeat_at >= sqlc.arg('worker_stale_cutoff')::timestamptz
          )
      )
      -- PRD #1390 D11: terminal-pending lease + pending_overflow closure (KEPT). Chat is inert here
      -- (excluded by kind above) but the predicate shape is kept identical to the sweep's.
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
    RETURNING id, user_id, status
),
wall_input AS (
    -- The steering input the worker drains (D3/D18); the CreatePauseInput shape. Consumed later by
    -- whichever statement settles the wall request (SetRunWallPark / ParkRunsAtWall / the hold /
    -- CreateExtendInput).
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT id, 'pause', 'wall' FROM requested
)
SELECT id, user_id, status FROM requested;

-- name: ParkRunsAtWall :many
-- Park server-side (D5) each past-deadline, timed, running run whose worker cannot or did not park
-- it: the worker heartbeat is STALE, the worker LACKS 'wall_park_v1', or a 'wall' request has gone
-- UNANSWERED for @grace_seconds. It runs BEFORE RequeueRunsOfStaleWorkers so a dead worker's
-- out-of-time run parks once instead of being requeued, re-claimed and re-cloned only to park.
--
-- The affected worker rows are locked FOR UPDATE in id order in the `locked` CTE (the
-- RequeueRunsOfStaleWorkers shape), so the live-vs-stale decision serialises with HeartbeatWorker
-- and the heartbeat/capability/nonce are read under the same lock. The park:
--   * status='paused', status_since=now(), hold_reason='budget_exhausted', hold_captured_head=NULL,
--     claim_released_at=now() (D16: arms the claim fence so every later report from the old flight
--     is rejected), clears the pause columns and consumes the wall input (D18), codex cap/epoch
--     revoked/bumped, health reset.
--   * released_worker_id/released_worker_nonce capture the LOCKED worker's incarnation (D19): the
--     resume drops affinity, and only a re-registered same worker or a peer may reclaim.
--   * KEEPS worker_id (informational; the panel names it), session_id, last_seq, started_at,
--     claim_generation; DELETES the run's worker_active_runs snapshot rows.
-- Same deadline, #1226 carve-out and #1390 terminal-pending guards as RequestWallParks.
WITH locked AS (
    SELECT w.id, w.last_heartbeat_at, w.protocol_capabilities, w.snapshot_register_nonce
    FROM workers w
    WHERE EXISTS (
        SELECT 1 FROM runs r
        WHERE r.worker_id = w.id
          AND r.status = 'running'
          AND r.kind NOT IN ('chat', 'judge')
          AND r.interactive = false
          AND r.started_at < (sqlc.arg('now')::timestamptz
                - make_interval(secs => COALESCE(r.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                                      + r.budget_paused_seconds
                                      + r.budget_extension_seconds
                                      + r.budget_finalize_seconds)))
    ORDER BY w.id
    FOR UPDATE
),
parked AS (
    UPDATE runs SET
        status                = 'paused',
        status_since          = now(),
        hold_reason           = 'budget_exhausted',
        hold_captured_head    = NULL,
        claim_released_at     = now(),
        pause_requested_at    = NULL,
        pause_mode            = NULL,
        pause_after_count     = NULL,
        codex_cap_hash        = NULL,
        codex_claim_epoch     = codex_claim_epoch + 1,
        health = 'ok', health_reason = NULL, health_since = NULL,
        -- D19: the incarnation that failed to park, read under the same lock as the classification.
        released_worker_id    = runs.worker_id,
        released_worker_nonce = l.snapshot_register_nonce,
        updated_at            = now()
    FROM locked l
    WHERE runs.worker_id = l.id
      AND runs.status = 'running'
      AND runs.kind NOT IN ('chat', 'judge')
      AND runs.interactive = false
      AND runs.started_at < (sqlc.arg('now')::timestamptz
            - make_interval(secs => COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                                  + runs.budget_paused_seconds
                                  + runs.budget_extension_seconds
                                  + runs.budget_finalize_seconds))
      -- PRD #1226 M3 (D3): completion-interlock carve-out (KEPT). A post-attempt run with a LIVE
      -- worker is spared; a post-attempt run whose worker went STALE is NOT protected and parks.
      AND NOT (
          runs.completion_attempts > 0
          AND EXISTS (SELECT 1 FROM workers w
                      WHERE w.id = runs.worker_id AND w.last_heartbeat_at IS NOT NULL
                        AND w.last_heartbeat_at >= sqlc.arg('worker_stale_cutoff')::timestamptz)
      )
      -- PRD #1390 D11: terminal-pending lease + pending_overflow closure (KEPT).
      AND (runs.kind = 'chat'
           OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                           WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                             AND a.terminal_pending_until > now()
                             AND a.claim_generation = runs.claim_generation)
               AND NOT EXISTS (SELECT 1 FROM workers w
                               WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
      -- Park when ANY of: the worker heartbeat is stale/absent; the worker lacks 'wall_park_v1'
      -- (COALESCE so a NULL cap array reads as incapable, not unknown); a 'wall' request has been
      -- unanswered for the grace window.
      AND (
          l.last_heartbeat_at IS NULL
          OR l.last_heartbeat_at < sqlc.arg('worker_stale_cutoff')::timestamptz
          OR NOT ('wall_park_v1' = ANY(COALESCE(l.protocol_capabilities, '{}'::text[])))
          OR (runs.pause_mode = 'wall'
              AND runs.pause_requested_at < now() - make_interval(secs => sqlc.arg('grace_seconds')::int))
      )
    RETURNING runs.id, runs.user_id, runs.status
),
snap_del AS (
    -- D16: drop the parked run's snapshot rows so a released flight's fresh heartbeat cannot keep
    -- the run unclaimable past the reclaim.
    DELETE FROM worker_active_runs a USING parked p WHERE a.run_id = p.id
),
wall_input_consumed AS (
    -- D18: settle the wall input in the same statement, so ConsumeRunInputs never hands the next
    -- flight a stale 'wall' abort.
    UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
    FROM parked p
    WHERE u.run_id = p.id AND u.kind = 'pause' AND u.body = 'wall' AND u.applied_at IS NULL
)
SELECT id, user_id, status FROM parked;

-- name: StampCompletionBudgetExhausted :execrows
-- PRD #1226 M4 (D3): the server-side served `budget_exhausted` steer. It stamps the post-attempt,
-- live-worker rows the wall-park sweep DELIBERATELY SPARES via its completion-interlock carve-out
-- (the `AND NOT (completion_attempts > 0 AND worker_id IN <live-heartbeat>)` clause). Those spared
-- rows are past their wall budget but held live by a working lead, and they must not run forever:
-- this stamp arms a ONE-SHOT served flag (completion_budget_exhausted_at) that the worker reads off
-- its running-report ACK (the SAME delivery the pause_requested flag rides, surfaced as
-- RunDTO.CompletionBudgetExhausted) and then routes to the completion hold — steering the live lead
-- INTO the verified hold rather than terminal-failing it out from under a live worker.
--
-- PRD #1497 M1: the deadline MIRRORS the wall-park sweep's THREE-TERM total and its carve-out
-- (completion_attempts > 0 AND the live-heartbeat worker subquery keyed on the SAME
-- worker_stale_cutoff), applied POSITIVELY here so the stamp set is the set the sweep excluded. It
-- is NOT a byte-for-byte complement of a single sweep statement any more — the sweep is now
-- RequestWallParks + ParkRunsAtWall, which additionally join on wall_park_v1 and heartbeat state
-- that this steer does not — but the DEADLINE and the completion carve-out are the same, which is
-- the property that matters: a post-attempt live run is steered into the hold at the SAME instant a
-- pre-attempt run would be parked. The total is
-- COALESCE(budget_wall_seconds, global_timeout_seconds) + budget_extension_seconds +
-- budget_finalize_seconds + budget_paused_seconds. Before #1497 this statement added
-- budget_paused_seconds but NOT budget_extension_seconds, so an EXTENDED post-attempt run was
-- steered into the hold at its ORIGINAL deadline (the stale-deadline bug this fix closes);
-- budget_finalize_seconds is the new #1497 term. completion_contract_version IS NOT NULL keeps a
-- legacy run out (it never interlocks and so is never a candidate for the hold).
--
-- It is ONE-SHOT: `completion_budget_exhausted_at IS NULL` means a run is stamped at most once per
-- exhaustion. The worker acting on the served steer CLEARS the flag — SetRunCompletionHold sets
-- completion_budget_exhausted_at back to NULL (the D3 "clears it so a stale ACK cannot re-arm"
-- contract) — so a running-report ACK that echoes a since-consumed flag can never re-arm the steer.
UPDATE runs SET completion_budget_exhausted_at = now(), updated_at = now()
WHERE status = 'running'
  AND started_at < (sqlc.arg('now')::timestamptz
        - make_interval(secs => COALESCE(budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                              + budget_extension_seconds
                              + budget_finalize_seconds
                              + budget_paused_seconds))
  AND kind NOT IN ('chat', 'judge')
  AND interactive = false
  AND completion_attempts > 0
  AND completion_contract_version IS NOT NULL
  AND completion_budget_exhausted_at IS NULL
  AND worker_id IN (
      SELECT w.id FROM workers w
      WHERE w.last_heartbeat_at IS NOT NULL
        AND w.last_heartbeat_at >= sqlc.arg('worker_stale_cutoff')::timestamptz
  );

-- name: ClearCompletionBudgetExhausted :execrows
-- PRD #1226 M5 (D3): a NEW OWNER DECISION clears the served `budget_exhausted` steer
-- (completion_budget_exhausted_at). D3's clears are "acting on it, parking, or a new owner
-- decision"; StampCompletionBudgetExhausted arms the one-shot flag and SetRunCompletionHold
-- (the worker acting on it / parking) clears it — this is the third clear, the owner's
-- CONTINUE decision (ContinueCompletionDecision). It is unconditional and id-scoped: on the
-- paused branch the flag is already NULL (SetRunCompletionHold cleared it on hold entry), so
-- this is a no-op there; on the LIVE awaiting_input completion-question branch the flag may
-- still be set (the worker was steered into the completion question before entering a hold),
-- and clearing it prevents the resumed worker being immediately re-steered into the hold off a
-- since-consumed ACK. Owner scoping is enforced by the caller (GetRun read) before this runs.
UPDATE runs SET completion_budget_exhausted_at = NULL, updated_at = now() WHERE id = @id;

-- name: FailRunsOfStaleWorkersOverCap :many
-- A stale worker's non-terminal run that has already used its re-queue budget →
-- failed instead of re-queued. Stamps move_pending_since (reconcile restores the
-- origin column; the sweep itself never touches the forge — worker-loss recovery
-- must not wait on a down forge).
--
-- PRD #1390 M1 (D9): the FAIL path requires TWO consecutive stale windows —
-- @fail_cutoff = now() - 2*WORKER_HEARTBEAT_STALE (the Go caller computes it) — so a run
-- that was requeued once and then hits a partition just over one window is not terminated
-- before a heartbeat can re-adopt it; terminal cannot be undone. The stale workers are
-- locked FIRST in a `locked` CTE that takes each worker row FOR UPDATE ordered by id (the
-- canonical lock order), so this statement serialises with HeartbeatWorker and re-checks
-- staleness after a concurrent heartbeat commits — the race where a heartbeat lands between
-- the staleness read and the terminal write is closed.
--
-- PRD #1390 M1 (D11): a run under an unexpired terminal-pending lease for its CURRENT
-- generation is NOT failed (an outcome is journaled on the worker, #1391), and neither is
-- any run owned by a worker flagged pending_overflow (the worker-level closure for outcomes
-- it could not list). Chat is exempt from the lease/overflow PROTECTION (D10) — it is still
-- failed as before — so the guard is `kind = 'chat' OR NOT EXISTS(...)`, never a top-level
-- kind <> 'chat'. The lease/overflow expiries (one clock) are the dead-worker backstop.
WITH locked AS (
    SELECT workers.id FROM workers
    WHERE workers.last_heartbeat_at IS NULL OR workers.last_heartbeat_at < @fail_cutoff
    ORDER BY workers.id
    FOR UPDATE
)
UPDATE runs SET status = 'failed', status_since = now(), failure_reason = @failure_reason,
    -- PRD #69 M7a: the trusted failure class for an orphaned run whose worker is gone.
    fail_origin = 'worker_lost',
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END, finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND requeue_count >= @max_requeues
  AND worker_id IN (SELECT id FROM locked)
  -- PRD #1390 D11: terminal-pending lease + pending_overflow closure, chat-exempt (D10).
  AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
RETURNING id, user_id, status;

-- name: RequeueRunsOfStaleWorkers :many
-- A stale worker's non-terminal run within its re-queue budget → back to queued
-- (worker_id kept for affinity, requeue_count incremented).
--
-- PRD #1390 M1 (D9): the stale workers are locked FIRST in a `locked` CTE that takes each
-- worker row FOR UPDATE ordered by id (the canonical lock order shared with the over-cap
-- fail above), so this statement serialises with HeartbeatWorker. The REQUEUE keeps the
-- single stale window (@cutoff); only the over-cap FAIL waits for the second one (D9).
WITH locked AS (
    SELECT workers.id FROM workers
    WHERE workers.last_heartbeat_at IS NULL OR workers.last_heartbeat_at < @cutoff
    ORDER BY workers.id
    FOR UPDATE
)
UPDATE runs SET status = 'queued', status_since = now(), requeue_count = requeue_count + 1,
    -- Exit contract (PRD #47 Decision 3): reset on the way back to 'queued'; the
    -- detector re-evaluates the queued signal from this transition's status_since.
    health = 'ok', health_reason = NULL, health_since = NULL,
    -- PRD #1390 M2b (D2): record the generation this stale-worker requeue charged, so the
    -- heartbeat re-adoption can refund the requeue only when the same generation is restored
    -- (a legitimate earlier loss is never refunded). The restore and every ClaimRun clear it.
    stale_requeue_generation = claim_generation,
    -- Issue #783: bank park time before a worker-death requeue -> queued, since started_at
    -- survives the requeue and the later claimed->running resume would not see the park.
    -- awaiting_followup is intentionally excluded: interactive runs are exempt from
    -- SweepRunningTimeout entirely (interactive = false), so they have no wall deadline.
    budget_paused_seconds = budget_paused_seconds
        + CASE WHEN status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int)
               ELSE 0 END,
    -- Codex claim-capability revocation (PRD #1147 M2): a stale worker losing its runs
    -- revokes their per-claim capabilities immediately — clear the hash and bump the
    -- epoch. A harmless no-op for a non-codex run, whose codex_cap_hash is already NULL.
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND requeue_count < @max_requeues
  AND worker_id IN (SELECT id FROM locked)
  -- PRD #1390 D11: terminal-pending lease + pending_overflow closure, chat-exempt (D10).
  AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
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
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END, finished_at = now(),
    -- PRD #265 D4: "in progress" is meaningless on a terminal run; clear the snapshot.
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    -- PRD #1190 M1: a terminal run carries no pending pause (root-cause clear; see SetRunCompleted).
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL, -- PRD #1247 D11 fix round: a terminal run settles a pending held switch (PRD #1190 pause-clear pattern) so the DTO never sticks at credential_switch:"requested" and PendingCredentialSwitchSignal (status-agnostic) can never signal a dead run
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND requeue_count >= @max_requeues
  -- PRD #1390 D11: register's orphan fail honours the terminal-pending lease + pending_overflow
  -- closure exactly as the stale-worker passes do (chat-exempt, D10) — a fresh worker process
  -- must not fail its own run whose outcome is journaled and about to be replayed (#1391).
  AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
RETURNING id;

-- name: RequeueWorkerRuns :many
-- Within budget → re-queued to this same worker (affinity), which then re-claims
-- and resumes from the persisted session (handles docker compose down && up).
--
-- PRD #1390 M2a: RETURNING id so Register can publish each requeue transition post-commit
-- (via publishSwept) exactly as the sweeper's RequeueRunsOfStaleWorkers twin already does —
-- closing the gap where a register-time requeue reached no live channel.
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
    -- Codex claim-capability revocation (PRD #1147 M2): re-queuing a worker's runs
    -- revokes their per-claim capabilities immediately — clear the hash and bump the
    -- epoch. A harmless no-op for a non-codex run, whose codex_cap_hash is already NULL.
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    updated_at = now()
WHERE runs.worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
  AND requeue_count < @max_requeues
  -- PRD #1390 D11: register's orphan requeue honours the terminal-pending lease + pending_overflow
  -- closure exactly as the stale-worker passes do (chat-exempt, D10) — a fresh worker process
  -- must not requeue its own run whose outcome is journaled and about to be replayed (#1391).
  AND (runs.kind = 'chat'
       OR (NOT EXISTS (SELECT 1 FROM worker_active_runs a
                       WHERE a.run_id = runs.id AND a.worker_id = runs.worker_id AND a.terminal_pending
                         AND a.terminal_pending_until > now()
                         AND a.claim_generation = runs.claim_generation)
           AND NOT EXISTS (SELECT 1 FROM workers w
                           WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())))
RETURNING id;

-- Worker active-run snapshot (PRD #1390 M2a) --------------------------------

-- name: DeleteWorkerActiveRuns :execrows
-- Full-replacement delete for the heartbeat/claim snapshot path (D3): drop every row of the
-- worker before re-inserting the validated set, so an attempt that ended (dropped from the
-- snapshot) leaves no row behind. Register uses DeleteOrdinaryWorkerActiveRunsNotIn instead,
-- because it must PRESERVE leased rows across a restart.
DELETE FROM worker_active_runs WHERE worker_id = @worker_id;

-- name: DeleteOrdinaryWorkerActiveRunsNotIn :execrows
-- Register-path delete (PRD #1390 M2a): remove only ORDINARY (terminal_pending = false) rows
-- whose run is not in the register-carried snapshot, PRESERVING every row under a
-- terminal-pending lease (its generation is what protects #1391's boot replay from the orphan
-- pass that follows). @keep_run_ids is the snapshot's run-id set; an empty set deletes every
-- ordinary row (a register with no listed live attempts). The kept rows are re-upserted right
-- after, so this only removes ordinary rows the snapshot no longer names.
DELETE FROM worker_active_runs
WHERE worker_id = @worker_id
  AND terminal_pending = false
  AND run_id <> ALL(@keep_run_ids::uuid[]);

-- name: UpsertWorkerActiveRun :execrows
-- Insert (or replace) one validated snapshot entry, OWNERSHIP-ENFORCED in SQL (D3): the row is
-- written only when the run is actually `worker_id = @worker_id`, so a buggy or hostile worker
-- can never describe — and thereby suppress a sibling's claim on, or lease — a run it does not
-- own. An entry that fails the EXISTS is silently dropped (0 rows affected); the Go caller logs
-- it. terminal_pending_until is stamped now() + the lease for a pending entry and NULL for a
-- live one; reported_at is this snapshot's capture time (now()), which ClaimRun's freshness
-- test reads. ON CONFLICT keeps the (worker_id, run_id) primary key a full upsert so the
-- register path's preserved-then-re-listed rows refresh cleanly.
INSERT INTO worker_active_runs (
    worker_id, run_id, claim_generation, phase,
    terminal_pending, terminal_pending_until, snapshot_epoch, reported_at
)
SELECT @worker_id, @run_id, @claim_generation, @phase,
       @terminal_pending,
       CASE WHEN @terminal_pending::boolean
            THEN now() + make_interval(secs => @lease_seconds::int)
            ELSE NULL END,
       @snapshot_epoch, now()
-- PRD #1497 M1 (D16): a RELEASED flight cannot refresh a snapshot row — otherwise a server-parked
-- run's old flight could keep the run unclaimable by its own heartbeat past the reclaim.
WHERE EXISTS (SELECT 1 FROM runs r WHERE r.id = @run_id AND r.worker_id = @worker_id AND r.claim_released_at IS NULL)
ON CONFLICT (worker_id, run_id) DO UPDATE SET
    claim_generation       = EXCLUDED.claim_generation,
    phase                  = EXCLUDED.phase,
    terminal_pending       = EXCLUDED.terminal_pending,
    terminal_pending_until = EXCLUDED.terminal_pending_until,
    snapshot_epoch         = EXCLUDED.snapshot_epoch,
    reported_at            = EXCLUDED.reported_at;

-- name: SetWorkerSnapshotState :execrows
-- Apply the snapshot's worker-row effects (PRD #1390 M2a): advance snapshot_epoch to the
-- accepted snapshot's epoch and set the pending_overflow closure. A flagged snapshot stamps
-- pending_overflow_until = now() + the lease (the worker-level stand-in for the row-level
-- leases it could not express, D11); an unflagged one clears both, which is what a valid
-- unflagged snapshot uses to reopen claiming before expiry.
UPDATE workers SET
    snapshot_epoch         = @snapshot_epoch,
    pending_overflow       = @pending_overflow,
    pending_overflow_until = CASE WHEN @pending_overflow::boolean
                                  THEN now() + make_interval(secs => @lease_seconds::int)
                                  ELSE NULL END,
    updated_at             = now()
WHERE id = @id;

-- name: ListActiveRunsForWorkers :many
-- PRD #1390 M2c: the reported active runs (run_id, phase, generation) for a set of workers, for
-- the worker-list DTO overlay. Batched over a worker-id set so the two list endpoints read every
-- worker's rows in one round-trip (no N+1). Ordered by (worker_id, run_id) so the overlay can
-- group by worker in one pass and each worker's entries render in a stable order.
SELECT worker_id, run_id, phase, claim_generation
FROM worker_active_runs
WHERE worker_id = ANY(@worker_ids::uuid[])
ORDER BY worker_id, run_id;

-- Heartbeat reconciliation (PRD #1390 M2b) ----------------------------------

-- name: LockOwnedRunsByIDs :many
-- PRD #1390 M2b (blocker 1, canonical lock order step b): lock the runs the heartbeat's snapshot
-- lists, owned by this worker, in deterministic id order, BEFORE the snapshot replace and the
-- reconciliation writes. Rows returned are ignored; the statement exists for its FOR UPDATE.
SELECT id FROM runs WHERE id = ANY(@run_ids::uuid[]) AND worker_id = @worker_id ORDER BY id FOR UPDATE;

-- name: ReadoptRunsFromSnapshot :many
-- PRD #1390 M2b (D5, D2): restore a `queued` run-lane run the worker still lists as a LIVE entry
-- (terminal_pending = false) at the SAME generation to its listed phase. This is a DIRECT status
-- write (not SetRunRunning), correct because the held-state content columns (open_question_id,
-- plan candidates, completion/follow-up identity) survived the stale requeue untouched (fact 4),
-- so the gate is restored by status alone. The queued interval is banked into budget_paused_seconds
-- only for the two approval/input phases (as the stale requeue did for the park). The requeue
-- refund (requeue_count - 1, floored at 0) fires ONLY when stale_requeue_generation = claim_generation
-- (D2: the stale requeue charged THIS exact generation); a NULL/mismatched provenance never refunds.
-- stale_requeue_generation is cleared after. claim_released_at IS NULL is #1247's fence (a run the
-- credential switch released must not be revived). Held-state content columns are UNTOUCHED here.
UPDATE runs r SET
    status = a.phase,
    status_since = now(),
    health = 'ok', health_reason = NULL, health_since = NULL,
    budget_paused_seconds = r.budget_paused_seconds
        + CASE WHEN a.phase IN ('awaiting_approval','awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - r.status_since))::int)
               ELSE 0 END,
    requeue_count = CASE WHEN r.stale_requeue_generation = r.claim_generation
                         THEN GREATEST(r.requeue_count - 1, 0) ELSE r.requeue_count END,
    stale_requeue_generation = NULL,
    updated_at = now()
FROM worker_active_runs a
WHERE a.worker_id = @worker_id AND a.run_id = r.id AND a.terminal_pending = false
  AND r.worker_id = @worker_id
  AND r.status = 'queued'
  AND r.kind <> 'chat'
  AND r.claim_generation = a.claim_generation
  AND r.claim_released_at IS NULL
RETURNING r.id, r.user_id, r.status;

-- name: FailRunsMissingFromSnapshot :many
-- PRD #1390 M2b (SC2, over cap): a run-lane `running` run this worker OWNS but no longer lists (its
-- execution is lost) — past the fence, and out of re-queue budget — is FAILED (fail-first with the
-- requeue twin below). Its SET list mirrors FailRunsOfStaleWorkersOverCap (fail_origin='worker_lost',
-- the pause/switch/milestone clears, health reset, move_pending_since for the reconcile origin
-- restore). Held states are never targeted (status = 'running' only). Chat is a target restriction
-- (kind <> 'chat', D10) — these writers only ever touch run-lane runs. @missing_cutoff is the stale
-- window plus one heartbeat interval (D4); @max_requeues is RUN_MAX_REQUEUES.
UPDATE runs SET status = 'failed', status_since = now(), failure_reason = @failure_reason,
    fail_origin = 'worker_lost',
    move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END, finished_at = now(),
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE runs.worker_id = @worker_id
  AND runs.kind <> 'chat'                                   -- D10 (run-lane only; chat has its own sweeps)
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL                        -- #1247 fence
  AND runs.status_since < @missing_cutoff                   -- fence: stale window + one heartbeat interval, D4
  AND runs.requeue_count >= @max_requeues
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a        -- ABSENT (or a different generation) from the snapshot
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a        -- D11 terminal-pending lease (worker-scoped, defense-in-depth)
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w                   -- D11 pending_overflow closure (worker-level, ESSENTIAL)
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())
RETURNING id, user_id, status;

-- name: RequeueRunsMissingFromSnapshot :many
-- PRD #1390 M2b (SC2, under cap): the requeue twin of FailRunsMissingFromSnapshot — a `running`
-- run-lane run this worker OWNS but no longer lists, past the fence and within budget, is REQUEUED
-- through the existing requeue path. Its SET list mirrors RequeueRunsOfStaleWorkers (health reset,
-- park-time bank for approval/input — a no-op here since only status='running' is targeted, codex
-- cap revocation, requeue_count++). It does NOT set stale_requeue_generation: a genuine loss is
-- never refunded (D2). Chat is a target restriction (kind <> 'chat', D10).
UPDATE runs SET status = 'queued', status_since = now(), requeue_count = requeue_count + 1,
    health = 'ok', health_reason = NULL, health_since = NULL,
    budget_paused_seconds = budget_paused_seconds
        + CASE WHEN status IN ('awaiting_approval', 'awaiting_input')
               THEN GREATEST(0, EXTRACT(EPOCH FROM (now() - status_since))::int)
               ELSE 0 END,
    codex_cap_hash = NULL, codex_claim_epoch = codex_claim_epoch + 1,
    updated_at = now()
WHERE runs.worker_id = @worker_id
  AND runs.kind <> 'chat'                                   -- D10 (run-lane only; chat has its own sweeps)
  AND runs.status = 'running'
  AND runs.claim_released_at IS NULL                        -- #1247 fence
  AND runs.status_since < @missing_cutoff                   -- fence: stale window + one heartbeat interval, D4
  AND runs.requeue_count < @max_requeues
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a        -- ABSENT (or a different generation) from the snapshot
                  WHERE a.worker_id = @worker_id AND a.run_id = runs.id
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM worker_active_runs a        -- D11 terminal-pending lease (worker-scoped, defense-in-depth)
                  WHERE a.worker_id = runs.worker_id AND a.run_id = runs.id
                    AND a.terminal_pending AND a.terminal_pending_until > now()
                    AND a.claim_generation = runs.claim_generation)
  AND NOT EXISTS (SELECT 1 FROM workers w                   -- D11 pending_overflow closure (worker-level, ESSENTIAL)
                  WHERE w.id = runs.worker_id AND w.pending_overflow_until > now())
RETURNING id, user_id, status;

-- Messages -----------------------------------------------------------------

-- name: InsertRunMessage :one
-- Idempotent seq-numbered append: a re-delivered batch (worker retry) is a
-- no-op on the duplicate (run_id, seq).
--
-- PRD #1247 M5 (D3): when the caller carries a claim generation (@claim_generation NOT NULL —
-- a capability worker stamps it on every message batch), the append is FENCED atomically. It
-- lands ONLY while the run is still at that generation with an UNRELEASED claim, so a message
-- batch from a released or reclaimed OLD flight persists nothing (the row-guard is checked in
-- the same statement as the insert, no TOCTOU). PRD #1497 M1 (D16) tightened this: the released
-- window now closes for a generation-less (legacy) caller too — a NULL-generation batch still
-- requires the run's claim to be UNRELEASED (claim_released_at IS NULL), so a released OLD flight
-- persists nothing whether or not it stamps a generation, and only a live claim honours a NULL
-- generation. ClaimRun clears the flag, so the reclaiming flight is unaffected. Preferring this
-- per-query guard over a FOR UPDATE tx per message keeps the hot append path a single round-trip.
--
-- BLOCKING-4 rework: return BOTH `inserted` (did this call add a row) AND `generation_live` (did
-- the fence predicate hold), computed in the SAME statement snapshot as the insert's WHERE, so
-- the caller can tell a benign duplicate (generation_live, not inserted — the row is already
-- persisted at the live generation) from a FENCE REJECTION (NOT generation_live — the batch is
-- from a released/reclaimed OLD flight and persisted nothing). Both used to surface as :execrows
-- == 0, so the caller advanced its high-water mark and folded usage over STALE frames. A legacy
-- (NULL generation) caller sees generation_live = TRUE on a LIVE claim and FALSE on a RELEASED one
-- (PRD #1497 M1, D16): the released window closes for a generation-less report too.
--
-- PRD #1247 M9 (D7): claim_generation is now also PERSISTED in the row's own column, not only used
-- in the fence WHERE. A capability worker's stamped generation lands on the frame, so both the
-- incremental fold and a refold attribute each frame to the epoch that produced it (the join
-- run_messages.claim_generation -> run_credential_epochs). A legacy caller (NULL narg) stores NULL,
-- byte-identical to before, and the fence behaviour is unchanged (the WHERE still reads the narg).
WITH ins AS (
    INSERT INTO run_messages (run_id, seq, kind, agent, agent_instance, agent_label, payload, claim_generation)
    SELECT @run_id, @seq, @kind, @agent, @agent_instance, @agent_label, @payload, sqlc.narg('claim_generation')::bigint
    -- PRD #1497 M1 (D16): claim_released_at IS NULL is REQUIRED even for a generation-less (legacy)
    -- report, so a released OLD flight persists nothing whether or not it stamps a generation; a live
    -- claim still honours a NULL generation (the compatibility contract). ClaimRun clears the flag,
    -- so the reclaiming flight is unaffected.
    WHERE EXISTS (SELECT 1 FROM runs r
                  WHERE r.id = @run_id
                    AND r.claim_released_at IS NULL
                    AND (sqlc.narg('claim_generation')::bigint IS NULL
                         OR r.claim_generation = sqlc.narg('claim_generation')::bigint))
    ON CONFLICT (run_id, seq) DO NOTHING
    RETURNING 1 AS one
)
SELECT
    EXISTS (SELECT 1 FROM ins) AS inserted,
    EXISTS (SELECT 1 FROM runs r
            WHERE r.id = @run_id
              AND r.claim_released_at IS NULL
              AND (sqlc.narg('claim_generation')::bigint IS NULL
                   OR r.claim_generation = sqlc.narg('claim_generation')::bigint)) AS generation_live;

-- name: CountCredentialSwitchMessages :one
-- Idempotency guard for the applied-switch run message (PRD #1247 M9, task c step 6): how many
-- 'credential_switch' messages already exist for this run at this exact claim generation. Keyed on
-- (run_id, claim_generation) so a same-generation retry after a crash (the epoch DO UPDATE
-- re-records, and this claim re-assembles) finds the already-inserted message and does NOT emit a
-- duplicate. > 0 ⇒ skip the insert. claim_generation is the message's persisted column (M9's
-- InsertRunMessage stamp), so this matches only messages minted for THIS generation.
SELECT count(*) FROM run_messages
WHERE run_id = @run_id AND kind = 'credential_switch'
  AND claim_generation = @claim_generation::bigint;

-- name: MaxRunMessageSeq :one
-- The run's current highest message seq, or 0 when it has none (PRD #1247 M9, task c step 7): the
-- gapless collision-recovery re-read. A worker's InsertRunMessage is NOT serialized by the run-row
-- FOR UPDATE lock the switch-message transaction holds, so a worker frame can land at last_seq+1
-- between the caller's read and its own insert; on that ON CONFLICT the caller re-reads MAX(seq)
-- here and retries at max+1, leaving no gap and losing no worker frame. COALESCE(...,0)::int keeps
-- the return an int32 for a run with no messages yet.
SELECT COALESCE(MAX(seq), 0)::int FROM run_messages WHERE run_id = @run_id;

-- name: CountRunMessagesThrough :one
-- The terminal fence's contiguity probe (PRD #1391 Run B M3c, D3): how many DISTINCT stored
-- message seqs fall in [1..through] for this run. Backed by the run_messages UNIQUE (run_id, seq)
-- index, so the count is an index-only range scan. A fully-contiguous run has count == through; a
-- run with any hole in [1..through] has count < through, which is exactly what SetState refuses a
-- terminal transition on (ErrMessagesPending) — the high-water last_seq alone cannot see a hole
-- BELOW it, so the terminal fence needs this count, not just runs.last_seq. Modeled on
-- MaxRunMessageSeq; ::int keeps the return an int32.
SELECT count(seq)::int FROM run_messages WHERE run_id = @run_id AND seq BETWEEN 1 AND @through::int;

-- name: RunMessageGaps :many
-- The hardened message-gaps read (PRD #1391 Run B M3c): the MISSING seq ranges in [1..through]
-- as bounded {first,last} pairs, after a keyset @cursor, ordered by seq, at most @lim of them.
-- Authorization is part of THIS statement and snapshot: the run must belong to @worker_id at its
-- current unreleased @claim_generation. A preliminary ownership query would leave a TOCTOU window
-- where a same-worker reclaim increments the generation before this query reads the newer flight's
-- gaps. `authorized` is empty for any stale/foreign/released claim, so the statement returns ZERO
-- rows. For an authorized run with no gaps, the final LEFT JOIN returns one all-NULL sentinel row;
-- the service uses that distinction to return an empty page rather than ErrRunNotOwned.
--
-- KEYSET pagination only — NO OFFSET, NO generate_series, NO materialisation of `through` rows:
-- the gaps are derived from the PRESENT rows via LAG over the (run_id, seq) index, so the scan is
-- bounded by what is stored (at most the run's message count), never by the size of `through`.
--
-- Each interior/leading gap is CLOSED by the present row immediately after it: for a present
-- `seq` whose predecessor (LAG) is `prev`, the hole [prev+1, seq-1] exists iff seq - prev > 1.
-- The TRAILING gap (max present seq .. through) has no closing present row, so a sentinel row at
-- through+1 is UNION-ed in to close it exactly like every interior gap. The keyset is the CLOSER
-- (the right-neighbor seq): a gap is emitted only when its closer > @cursor, and next_cursor is
-- that closer, so the next page continues strictly after the last one with no overlap and no gap
-- re-emitted. The present set is bounded below by @cursor (seq >= @cursor) so a large cursor scans
-- only the index tail; @cursor doubles as the LAG seed so the first closer after the cursor gets
-- the correct predecessor. @cursor = 0 (the first page) admits every gap, including the leading
-- one [1, min_present-1]. The trailing gap is uniquely the one whose `last` == through.
WITH authorized AS (
    SELECT 1 AS ok FROM runs r
    WHERE r.id = @run_id
      AND r.worker_id = @worker_id
      AND r.claim_released_at IS NULL
      AND r.claim_generation = @claim_generation
),
present AS (
    SELECT m.seq FROM run_messages m
    CROSS JOIN authorized
    WHERE m.run_id = @run_id AND m.seq BETWEEN 1 AND @through::int AND m.seq >= @cursor::int
    UNION ALL
    SELECT (@through::int) + 1 FROM authorized
),
edges AS (
    SELECT seq AS closer,
           COALESCE(LAG(seq) OVER (ORDER BY seq), @cursor::int) AS prev
    FROM present
),
gaps AS (
    SELECT (prev + 1)::int AS gap_first, (closer - 1)::int AS gap_last, closer::int AS next_cursor
    FROM edges
    WHERE closer - prev > 1 AND closer > @cursor::int
    ORDER BY closer ASC
    LIMIT @lim::int
)
SELECT gaps.gap_first, gaps.gap_last, gaps.next_cursor
FROM authorized
LEFT JOIN gaps ON true
ORDER BY gaps.next_cursor ASC NULLS LAST;

-- name: ListRunMessagesAfter :many
-- Replay for a (re)connecting browser: everything after its last-seen seq, in
-- order. The persisted log is authoritative; the WS layer (M5) is only a live
-- cache on top of this.
-- Column order matches the run_messages table order (the two PRD #99 columns were
-- appended by 00075, claim_generation by PRD #1247's 00233), so sqlc keeps returning
-- store.RunMessage rather than minting a separate ...Row type.
-- TO DO IT RIGHT: new columns must be APPENDED to both this SELECT list and
-- ListRunMessagesForWorkerPage's, in the same order the ALTER TABLE adds them.
-- Diverge and sqlc mints per-query Row types for BOTH, breaking workersvc.Store's
-- []store.RunMessage contract (a compile error at cmd/server/main.go).
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation
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
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation
FROM run_messages
WHERE run_id = @run_id AND seq > @after_seq
ORDER BY seq ASC
LIMIT @lim;

-- name: ListRunMessagesBeforePage :many
-- Backward twin of ListRunMessagesAfterPage for the TUI's tail-first/backfill paging
-- (PRD #1137): the newest @lim messages with seq < @before_seq, returned DESC. The
-- service reverses to ascending in Go (D3) — NOT in a SQL subquery — so the row stays
-- store.RunMessage. Authorization (owner-or-admin) is checked by the caller.
-- Column order is IDENTICAL to ListRunMessagesAfter and ListRunMessagesAfterPage so
-- the row stays store.RunMessage. New columns must be APPENDED to ALL THREE of these
-- queries in the same order the ALTER TABLE adds them — see ListRunMessagesAfter's
-- note. Diverge and sqlc mints a per-query Row type, breaking that []store.RunMessage.
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation
FROM run_messages
WHERE run_id = @run_id AND seq < @before_seq
ORDER BY seq DESC
LIMIT @lim;

-- Usage accounting (PRD #40) ------------------------------------------------

-- name: UpsertRunUsage :exec
-- Fold one model's usage from a delivered result frame into the run's accounting
-- (Decision 2). Each result frame is ONE SDK query() LEG; how that leg's modelUsage
-- reads is now stamped per row by usage_basis (ADR-1562, amending ADR-1079):
--   * 'per_leg' — the ADR-1079 semantics: the value is ONLY this leg (every pre-#1562
--     frame, every Codex frame, the stub executor). lineage_epoch is part of the key and
--     each leg lands in its own row; the view SUMs those legs.
--   * 'session_cumulative' — the SDK >= 0.3.277 reading (Claude only): the value is the
--     RUNNING TOTAL of the session this leg continued, so the view high-water-folds the
--     legs of one (model, lineage_index) instead of summing them.
-- GREATEST here is for RE-DELIVERY idempotency — a crash-retry that re-delivers the SAME
-- leg (same run_id/session_id/model/lineage_epoch) must not regress the row — and for the
-- case of MULTIPLE cumulative result frames inside ONE process (a multi-turn query()
-- reporting running totals under one epoch); it is NEVER a cross-leg collapse. The pre-#1079
-- under-count was distinct legs sharing runs.session_id merging at the old three-column key;
-- keying per lineage_epoch fixed that, and #1562's usage_basis fixes the opposite over-count
-- a resumed session's cumulative frames caused when summed. The API calls this for every
-- delivered result frame incl. seq-deduped replays, so at-least-once delivery + this
-- idempotent monotonic merge = correct totals with no crash window. The run_usage_totals
-- view (00244) MAXes within (run_id, model, lineage_epoch), then per (run_id, model,
-- lineage_index) sums per_leg legs and high-water-folds session_cumulative legs, then SUMs
-- across models.
--
-- ADR-1562: usage_basis and lineage_index ride the fold. usage_basis upgrades ONE-WAY on
-- conflict — once any frame of a leg reads 'session_cumulative' the row stays cumulative,
-- so an unmarked (per_leg) frame arriving before or after a marked one for the same leg (a
-- run in flight across the #1562 deploy) never downgrades it back to a summed reading.
-- lineage_index is a pure function of (run_id, seq) like lineage_epoch (the fold derives it
-- from the persisted init frames via CountRunLineageRestartsBefore), so on conflict the two
-- sides are equal and the existing value is kept.
--
-- PRD #1332 M5A (D2/D5): harness and cost_status ride the fold. harness is the run's
-- immutable, run-derived harness (foldUsageFrames sources it from runs.harness, never a
-- worker field), so on conflict the two sides are always equal and the existing value is
-- kept. cost_status resolves conservatively: EQUAL statuses retain; ANY disagreement (which
-- necessarily includes any existing/incoming 'unreported' paired with a different status,
-- and two-of-{metered,subscription,unreported}) resolves to 'unreported'. cost_usd is
-- GREATEST only when the RESOLVED status is 'metered' (which requires both sides already
-- 'metered'), else 0 — so a redelivery can never combine an unreported/subscription status
-- with a positive dollar amount, and the result always satisfies 00226's
-- run_usage_nonmetered_zero_check (cost_status='metered' OR cost_usd=0).
-- PRD #1247 M9 (D7): claim_generation is PROVENANCE ONLY — the epoch of the frames that produced
-- this leg — never part of the leg key (00233's PK stays (run_id, session_id, model, lineage_epoch);
-- it is NOT added to ON CONFLICT). On conflict it is COALESCE(existing, EXCLUDED): an established
-- non-null provenance is NEVER clobbered by a later same-leg frame (a straggler re-delivery, or the
-- rare case where a later frame of the same (session_id, model, lineage_epoch) leg carries a
-- different generation), while a first frame whose existing value is NULL adopts the incoming one.
-- Legacy NULL frames leave the column NULL. run_usage.claim_generation is DERIVED OUTPUT of the fold,
-- never its evidence — the frame stamp on run_messages is the evidence.
INSERT INTO run_usage (
    run_id, session_id, model, lineage_epoch,
    input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, harness, cost_status, usage_basis, lineage_index, claim_generation, updated_at
) VALUES (
    @run_id, @session_id, @model, @lineage_epoch,
    @input_tokens, @cache_read_tokens, @cache_creation_tokens, @output_tokens, @cost_usd, @harness, @cost_status, @usage_basis, @lineage_index, sqlc.narg('claim_generation')::bigint, now()
)
ON CONFLICT (run_id, session_id, model, lineage_epoch) DO UPDATE SET
    input_tokens          = GREATEST(run_usage.input_tokens,          EXCLUDED.input_tokens),
    cache_read_tokens     = GREATEST(run_usage.cache_read_tokens,     EXCLUDED.cache_read_tokens),
    cache_creation_tokens = GREATEST(run_usage.cache_creation_tokens, EXCLUDED.cache_creation_tokens),
    output_tokens         = GREATEST(run_usage.output_tokens,         EXCLUDED.output_tokens),
    -- The run's harness is immutable, so existing == EXCLUDED on conflict; keep existing.
    harness               = run_usage.harness,
    -- usage_basis upgrades ONE-WAY (ADR-1562): once either side reads 'session_cumulative'
    -- the leg is cumulative for good, so a stray per_leg frame never downgrades a resumed
    -- leg back to a summed reading (the in-flight-across-deploy case). Both-per_leg stays
    -- per_leg.
    usage_basis           = CASE
                                WHEN run_usage.usage_basis = 'session_cumulative' OR EXCLUDED.usage_basis = 'session_cumulative' THEN 'session_cumulative'
                                ELSE 'per_leg'
                            END,
    -- lineage_index is a pure function of (run_id, seq), so existing == EXCLUDED on
    -- conflict; keep existing (mirrors lineage_epoch, which is in the key).
    lineage_index         = run_usage.lineage_index,
    -- Provenance is set-once: keep an established non-null generation, never clobber it with a
    -- later same-leg frame's EXCLUDED (M9, D7). A NULL existing value adopts EXCLUDED.
    claim_generation      = COALESCE(run_usage.claim_generation, EXCLUDED.claim_generation),
    cost_status           = CASE
                                WHEN run_usage.cost_status = EXCLUDED.cost_status THEN run_usage.cost_status
                                ELSE 'unreported'
                            END,
    -- GREATEST only when the resolved status is 'metered' (recomputed inline — the SET-list
    -- expressions all read the OLD row's run_usage.* and EXCLUDED.*, so order is irrelevant),
    -- else 0 to keep a non-metered row at the numeric placeholder.
    cost_usd              = CASE
                                WHEN (CASE WHEN run_usage.cost_status = EXCLUDED.cost_status THEN run_usage.cost_status ELSE 'unreported' END) = 'metered'
                                    THEN GREATEST(run_usage.cost_usd, EXCLUDED.cost_usd)
                                ELSE 0
                            END,
    updated_at            = now();

-- name: CountRunInitFramesBefore :one
-- The leg index of a result frame (PRD #1079): the position-absolute count of persisted
-- `init` status frames of this run with a lower seq. The worker persists the SDK's
-- system/init message as a status frame with payload.event='init' at the start of EVERY
-- query() call (agent/src/sdk-messages.ts, the system/subtype-init branch — NOT mapResult,
-- which maps the result frame), so this counts the legs that preceded the frame. It is a pure function of
-- (run_id, seq): a re-delivered frame recomputes the same value and lands in the same
-- run_usage row whatever else has landed since. Backed by idx_run_messages_init (00188),
-- so it is O(legs), not O(messages). Its sibling CountRunLineageRestartsBefore (ADR-1562)
-- counts the SESSION a leg belongs to (lineage_index) off the same index.
SELECT COUNT(*) FROM run_messages
WHERE run_id = @run_id AND kind = 'status'
  AND payload->>'event' = 'init' AND seq < @seq;

-- name: CountRunLineageRestartsBefore :one
-- The lineage (session) index of a result frame (ADR-1562): the count of persisted `init`
-- status frames of this run that are flagged fresh_session:true and have a lower seq,
-- EXCLUDING the run's FIRST init. lineage_epoch (CountRunInitFramesBefore) counts the LEG;
-- this counts the SESSION a leg belongs to. Lineage 0 is the run's initial session, whether
-- or not its first init is flagged (a fresh session that starts BEFORE any prior one is not a
-- restart) — hence the `seq > MIN(seq)` exclusion of the run's own first init. Each fresh
-- session that starts after a prior one opens lineage 1, 2, … The worker stamps
-- fresh_session:true on the init frame of an SDK process that did NOT continue the requested
-- session (no resume requested, or the SDK's init session_id differs from the one requested).
-- Like CountRunInitFramesBefore it is a pure function of (run_id, seq): a re-delivered frame
-- recomputes the same value and lands in the same run_usage row. Backed by the SAME partial
-- index idx_run_messages_init (00188) — the outer count and the MIN subquery both scan only
-- the run's init frames, so it is O(legs), not O(messages).
SELECT COUNT(*) FROM run_messages rm
WHERE rm.run_id = @run_id AND rm.kind = 'status'
  AND rm.payload->>'event' = 'init'
  AND rm.payload->>'fresh_session' = 'true'
  AND rm.seq < @seq
  AND rm.seq > (
      SELECT MIN(first_init.seq) FROM run_messages first_init
      WHERE first_init.run_id = @run_id AND first_init.kind = 'status' AND first_init.payload->>'event' = 'init'
  );

-- name: GetRunUsageTotal :one
-- One run's rollup totals (PRD #40 M3), for the run-detail usage strip. Reads the
-- run_usage_totals view (greatest-wins per model, summed across models). Returns NO
-- ROW for a run with no usage — the handler maps pgx.ErrNoRows to "no usage" (absent,
-- never a fake 0), so it does not gate detail visibility on ownership here (the
-- caller has already authorized the viewer).
--
-- PRD #1332 M5A (D2): the per-run folded cost_status rides along so a reader can tell a real
-- metered dollar total from a subscription/unreported one that cost_usd cannot represent.
-- M5A adds no public DTO field for it (the response shape stays unchanged); M5B consumes it.
SELECT input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status
FROM run_usage_totals
WHERE run_id = @run_id;

-- name: ListRunUsageTotalsForRuns :many
-- The rollup totals (PRD #40 M3) for a PAGE of runs, keyed by run_id (issue #1620). The
-- Runs list (ListRunsForUser) reads its usage here instead of LEFT-joining the view, and
-- the handler maps each row onto its run; a run with no usage returns NO row (absent,
-- never a fake 0), exactly like GetRunUsageTotal. cost_status (PRD #1429 M1, D7) rides
-- along so a subscription/unreported placeholder 0 never reads as a real dollar total.
--
-- Why this shape survives a generic plan: the qual is a PARAMETER on run_id, the view's
-- outermost GROUP BY key and the leading key of every inner GROUP BY / window PARTITION BY,
-- so PostgreSQL pushes it through all the nested derived tables down to an index scan of
-- run_usage_pkey (run_id, session_id, model, lineage_epoch) whether the plan is custom or
-- generic. Only the page's legs are folded, never the whole table.
SELECT run_id, input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status
FROM run_usage_totals
WHERE run_id = ANY(@run_ids::uuid[]);

-- name: SelfUsage :one
-- The requesting user's own usage (PRD #40 M3, GET /api/usage): lifetime totals,
-- last-7-days totals, and the count of their runs that carry usage. Windowed on the
-- run's created_at (laptop scale ≈ when it spent). COALESCE(...,0) so a user with no
-- usage gets zeros + run_count 0 (the client reads run_count==0 as "nothing yet").
-- Chat runs are excluded (belt-and-suspenders: the fold never writes chat rows).
-- PRD #1332 M5A (D2): each dollar sum now travels with subscription/unreported RUN COUNTS
-- for the SAME window, so no partial dollar total can read as complete — a run is counted by
-- its folded per-run cost_status from the view. M5A adds no public DTO field for these
-- counts (the /api/usage response shape stays unchanged); M5B consumes them.
--
-- Issue #1620: scoped drives from the user's runs and reads the view through a CROSS JOIN
-- LATERAL keyed on t.run_id = r.id, rather than a plain JOIN of the view. A plain join
-- lets the planner fold the WHOLE view (every user's run_usage) and hash-join it, or under
-- a generic plan nested-loop the full fold per run row; the lateral makes run_id a per-row
-- qual that pushes down to run_usage_pkey, so only this user's legs are folded. CROSS (not
-- LEFT) keeps the inner-join semantics: a run with no usage contributes no row, so run_count
-- still counts only runs that carry usage. The LIMIT 1 is a semantic no-op (the view's
-- outermost GROUP BY run_id yields at most one row per run) kept as a FENCE: without it the
-- planner may pull the lateral subquery back up into a plain join and lose the per-row
-- pushdown this rewrite exists for.
WITH scoped AS (
    SELECT r.created_at, t.cost_status,
           t.input_tokens, t.cache_read_tokens, t.cache_creation_tokens, t.output_tokens, t.cost_usd
    FROM runs r
    CROSS JOIN LATERAL (
        SELECT t.cost_status, t.input_tokens, t.cache_read_tokens, t.cache_creation_tokens,
               t.output_tokens, t.cost_usd
        FROM run_usage_totals t
        WHERE t.run_id = r.id
        LIMIT 1
    ) t
    WHERE r.user_id = @user_id AND r.kind <> 'chat'
)
SELECT
    COALESCE(SUM(input_tokens), 0)::bigint          AS lifetime_input_tokens,
    COALESCE(SUM(cache_read_tokens), 0)::bigint      AS lifetime_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens), 0)::bigint  AS lifetime_cache_creation_tokens,
    COALESCE(SUM(output_tokens), 0)::bigint          AS lifetime_output_tokens,
    COALESCE(SUM(cost_usd), 0)::numeric              AS lifetime_cost_usd,
    count(*) FILTER (WHERE cost_status = 'subscription')::bigint AS lifetime_subscription_run_count,
    count(*) FILTER (WHERE cost_status = 'unreported')::bigint   AS lifetime_unreported_run_count,
    COALESCE(SUM(input_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_input_tokens,
    COALESCE(SUM(cache_read_tokens)      FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens)  FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_creation_tokens,
    COALESCE(SUM(output_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_output_tokens,
    COALESCE(SUM(cost_usd)               FILTER (WHERE created_at >= now() - interval '7 days'), 0)::numeric AS last7_cost_usd,
    count(*) FILTER (WHERE cost_status = 'subscription' AND created_at >= now() - interval '7 days')::bigint AS last7_subscription_run_count,
    count(*) FILTER (WHERE cost_status = 'unreported'   AND created_at >= now() - interval '7 days')::bigint AS last7_unreported_run_count,
    count(*)::bigint AS run_count
FROM scoped;

-- name: AdminUsageTotals :one
-- Factory-wide totals across ALL users' runs (PRD #40 M3, GET /api/admin/usage).
-- Same shape as SelfUsage without the user filter; by construction this equals the
-- SUM of the AdminUsagePerUser rows (both read run_usage_totals joined to non-chat
-- runs), which the handler test asserts.
-- PRD #1332 M5A (D2): the factory-wide dollar sums carry subscription/unreported RUN COUNTS
-- for both windows, mirroring SelfUsage, so a partial dollar total can never read as
-- complete. M5A adds no public DTO field for the counts; M5B consumes them.
WITH scoped AS (
    SELECT r.created_at, t.cost_status,
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
    count(*) FILTER (WHERE cost_status = 'subscription')::bigint AS lifetime_subscription_run_count,
    count(*) FILTER (WHERE cost_status = 'unreported')::bigint   AS lifetime_unreported_run_count,
    COALESCE(SUM(input_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_input_tokens,
    COALESCE(SUM(cache_read_tokens)      FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens)  FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_cache_creation_tokens,
    COALESCE(SUM(output_tokens)          FILTER (WHERE created_at >= now() - interval '7 days'), 0)::bigint  AS last7_output_tokens,
    COALESCE(SUM(cost_usd)               FILTER (WHERE created_at >= now() - interval '7 days'), 0)::numeric AS last7_cost_usd,
    count(*) FILTER (WHERE cost_status = 'subscription' AND created_at >= now() - interval '7 days')::bigint AS last7_subscription_run_count,
    count(*) FILTER (WHERE cost_status = 'unreported'   AND created_at >= now() - interval '7 days')::bigint AS last7_unreported_run_count,
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
-- PRD #1332 M5A (D2): each user's lifetime dollar sum carries subscription/unreported RUN
-- COUNTS so a partial dollar total cannot read as complete. Lifetime-only here (this row is
-- the admin per-user lifetime breakdown; the windowed counts live in AdminUsageTotals). M5A
-- adds no public DTO field for the counts; M5B consumes them.
SELECT u.id AS user_id, u.email,
    COALESCE(SUM(t.input_tokens), 0)::bigint          AS input_tokens,
    COALESCE(SUM(t.cache_read_tokens), 0)::bigint      AS cache_read_tokens,
    COALESCE(SUM(t.cache_creation_tokens), 0)::bigint  AS cache_creation_tokens,
    COALESCE(SUM(t.output_tokens), 0)::bigint          AS output_tokens,
    COALESCE(SUM(t.cost_usd), 0)::numeric              AS cost_usd,
    count(*) FILTER (WHERE t.cost_status = 'subscription')::bigint AS subscription_run_count,
    count(*) FILTER (WHERE t.cost_status = 'unreported')::bigint   AS unreported_run_count,
    count(t.run_id)::bigint AS run_count
FROM run_usage_totals t
JOIN runs r ON r.id = t.run_id
JOIN users u ON u.id = r.user_id
WHERE r.kind <> 'chat'
GROUP BY u.id, u.email
ORDER BY cost_usd DESC, output_tokens DESC, u.id;

-- Failed-run rate outcome aggregates (PRD #1293 M1) -------------------------
-- These count over `runs` DIRECTLY, NOT run_usage_totals (D1): a run that fails at
-- provisioning / credential lookup / guardrail has no usage row, so a rate computed
-- over the usage join would systematically hide the infra failures this number exists
-- to surface. The predicate matches what the Runs page lists: terminal runs only
-- (status IN ('completed','failed','cancelled')) and kind NOT IN ('chat','judge') (D2 —
-- a chat/judge failure is not a factory failure). Windowed on created_at (D3), the same
-- axis as the usage 7-day figures, so the two 7-day numbers on one card describe the
-- same set of runs. plan_rejected is split out of failed (D4): rejecting a plan is the
-- owner's decision, kept in the denominator and its own bar segment but out of the
-- numerator. Every computed column carries an explicit ::bigint cast (the file's
-- convention). Invariants the handler/live-DB tests assert:
-- finished == completed + cancelled + plan_rejected + failed, and
-- sum(fail_origins) == failed. The per-origin breakdown is a jsonb_object_agg scalar
-- subquery INSIDE each outcome query (issue #1451), not a second query per scope: one
-- statement reads one snapshot, so the breakdown and the `failed` count always describe
-- the same rows. (It used to be a separate :many query folded in Go, which let a run
-- turning terminal between the two reads skew the sum.) sqlc types the jsonb column as
-- []byte; the handler json-decodes it into the fail_origins map.

-- name: SelfRunOutcomes :one
-- The requesting user's own run outcome counts for BOTH windows (PRD #1293 M1).
-- created_at/user_id are qualified runs.* because the needs_landing correlated subquery
-- brings recovery_captures (which also has created_at/user_id) into the analyzer's scope
-- (issue #1418); status/fail_origin/preserved_patch are unique to runs, so they stay bare.
SELECT
    count(*)::bigint                                                                       AS lifetime_finished,
    count(*) FILTER (WHERE status = 'completed')::bigint                                   AS lifetime_completed,
    count(*) FILTER (WHERE status = 'cancelled')::bigint                                   AS lifetime_cancelled,
    count(*) FILTER (WHERE status = 'failed' AND fail_origin = 'plan_rejected')::bigint    AS lifetime_plan_rejected,
    count(*) FILTER (WHERE status = 'failed' AND fail_origin IS DISTINCT FROM 'plan_rejected')::bigint AS lifetime_failed,
    count(*) FILTER (WHERE runs.created_at >= now() - interval '7 days')::bigint           AS last7_finished,
    count(*) FILTER (WHERE status = 'completed' AND runs.created_at >= now() - interval '7 days')::bigint AS last7_completed,
    count(*) FILTER (WHERE status = 'cancelled' AND runs.created_at >= now() - interval '7 days')::bigint AS last7_cancelled,
    count(*) FILTER (WHERE status = 'failed' AND fail_origin = 'plan_rejected' AND runs.created_at >= now() - interval '7 days')::bigint AS last7_plan_rejected,
    count(*) FILTER (WHERE status = 'failed' AND fail_origin IS DISTINCT FROM 'plan_rejected' AND runs.created_at >= now() - interval '7 days')::bigint AS last7_failed,
    -- needs_landing (issue #1418): the SUB-CUT of the `failed` count whose per-run
    -- landing_state derives to needs_landing (workersvc.DeriveLandingState) — a human-landable
    -- fail_origin (@landable_origins, the Go-owned set, never spelled here) whose committed work
    -- is recoverable (an available recovery capture, owner-scoped on idx_recovery_captures_run_owner,
    -- OR a preserved_patch). The correlated EXISTS resolves to the OUTER `runs` row; a JOIN would
    -- fan out and corrupt the sibling counts, so it stays a subquery.
    count(*) FILTER (
        WHERE status = 'failed'
          AND fail_origin = ANY(@landable_origins::text[])
          AND (EXISTS (SELECT 1 FROM recovery_captures c
                       WHERE c.run_id = runs.id AND c.user_id = runs.user_id AND c.state = 'available')
               OR preserved_patch IS NOT NULL)
    )::bigint AS lifetime_needs_landing,
    count(*) FILTER (
        WHERE status = 'failed'
          AND fail_origin = ANY(@landable_origins::text[])
          AND (EXISTS (SELECT 1 FROM recovery_captures c
                       WHERE c.run_id = runs.id AND c.user_id = runs.user_id AND c.state = 'available')
               OR preserved_patch IS NOT NULL)
          AND runs.created_at >= now() - interval '7 days'
    )::bigint AS last7_needs_landing,
    -- fail_origins (issue #1451): the per-origin breakdown of the `failed` count, in THIS
    -- statement. One statement is one snapshot, so sum(fail_origins) == failed by
    -- construction; a separate origins query could see a run that turned terminal `failed`
    -- after the counts were read. NULL fail_origin buckets as 'unknown'; an empty group
    -- yields '{}' (never NULL). Returned as a jsonb object, decoded in the handler.
    (SELECT COALESCE(jsonb_object_agg(o.origin, o.cnt), '{}')::jsonb
        FROM (SELECT COALESCE(r2.fail_origin, 'unknown') AS origin, count(*) AS cnt
              FROM runs r2
              WHERE r2.user_id = @user_id
              AND r2.status = 'failed' AND r2.fail_origin IS DISTINCT FROM 'plan_rejected'
              AND r2.kind NOT IN ('chat', 'judge')
              GROUP BY 1) o) AS lifetime_fail_origins,
    (SELECT COALESCE(jsonb_object_agg(o.origin, o.cnt), '{}')::jsonb
        FROM (SELECT COALESCE(r2.fail_origin, 'unknown') AS origin, count(*) AS cnt
              FROM runs r2
              WHERE r2.user_id = @user_id
              AND r2.status = 'failed' AND r2.fail_origin IS DISTINCT FROM 'plan_rejected'
              AND r2.kind NOT IN ('chat', 'judge')
              AND r2.created_at >= now() - interval '7 days'
              GROUP BY 1) o) AS last7_fail_origins
FROM runs
WHERE runs.user_id = @user_id
  AND status IN ('completed', 'failed', 'cancelled')
  AND kind NOT IN ('chat', 'judge');

-- name: AdminRunOutcomes :one
-- Factory-wide run outcome counts for BOTH windows (PRD #1293 M1); same shape as
-- SelfRunOutcomes without the user filter. created_at is qualified runs.* because the
-- needs_landing correlated subquery brings recovery_captures (also has created_at) into
-- the analyzer's scope (issue #1418); status/fail_origin/preserved_patch stay bare.
SELECT
    count(*)::bigint                                                                       AS lifetime_finished,
    count(*) FILTER (WHERE status = 'completed')::bigint                                   AS lifetime_completed,
    count(*) FILTER (WHERE status = 'cancelled')::bigint                                   AS lifetime_cancelled,
    count(*) FILTER (WHERE status = 'failed' AND fail_origin = 'plan_rejected')::bigint    AS lifetime_plan_rejected,
    count(*) FILTER (WHERE status = 'failed' AND fail_origin IS DISTINCT FROM 'plan_rejected')::bigint AS lifetime_failed,
    count(*) FILTER (WHERE runs.created_at >= now() - interval '7 days')::bigint           AS last7_finished,
    count(*) FILTER (WHERE status = 'completed' AND runs.created_at >= now() - interval '7 days')::bigint AS last7_completed,
    count(*) FILTER (WHERE status = 'cancelled' AND runs.created_at >= now() - interval '7 days')::bigint AS last7_cancelled,
    count(*) FILTER (WHERE status = 'failed' AND fail_origin = 'plan_rejected' AND runs.created_at >= now() - interval '7 days')::bigint AS last7_plan_rejected,
    count(*) FILTER (WHERE status = 'failed' AND fail_origin IS DISTINCT FROM 'plan_rejected' AND runs.created_at >= now() - interval '7 days')::bigint AS last7_failed,
    -- needs_landing (issue #1418): SUB-CUT of `failed`; see SelfRunOutcomes for the shape.
    -- @landable_origins is the Go-owned human-landable set; the correlated EXISTS resolves to
    -- the OUTER `runs` row (no JOIN, which would corrupt the sibling counts).
    count(*) FILTER (
        WHERE status = 'failed'
          AND fail_origin = ANY(@landable_origins::text[])
          AND (EXISTS (SELECT 1 FROM recovery_captures c
                       WHERE c.run_id = runs.id AND c.user_id = runs.user_id AND c.state = 'available')
               OR preserved_patch IS NOT NULL)
    )::bigint AS lifetime_needs_landing,
    count(*) FILTER (
        WHERE status = 'failed'
          AND fail_origin = ANY(@landable_origins::text[])
          AND (EXISTS (SELECT 1 FROM recovery_captures c
                       WHERE c.run_id = runs.id AND c.user_id = runs.user_id AND c.state = 'available')
               OR preserved_patch IS NOT NULL)
          AND runs.created_at >= now() - interval '7 days'
    )::bigint AS last7_needs_landing,
    -- fail_origins (issue #1451): same single-snapshot shape as SelfRunOutcomes, factory-wide.
    (SELECT COALESCE(jsonb_object_agg(o.origin, o.cnt), '{}')::jsonb
        FROM (SELECT COALESCE(r2.fail_origin, 'unknown') AS origin, count(*) AS cnt
              FROM runs r2
              WHERE r2.status = 'failed' AND r2.fail_origin IS DISTINCT FROM 'plan_rejected'
              AND r2.kind NOT IN ('chat', 'judge')
              GROUP BY 1) o) AS lifetime_fail_origins,
    (SELECT COALESCE(jsonb_object_agg(o.origin, o.cnt), '{}')::jsonb
        FROM (SELECT COALESCE(r2.fail_origin, 'unknown') AS origin, count(*) AS cnt
              FROM runs r2
              WHERE r2.status = 'failed' AND r2.fail_origin IS DISTINCT FROM 'plan_rejected'
              AND r2.kind NOT IN ('chat', 'judge')
              AND r2.created_at >= now() - interval '7 days'
              GROUP BY 1) o) AS last7_fail_origins
FROM runs
WHERE status IN ('completed', 'failed', 'cancelled')
  AND kind NOT IN ('chat', 'judge');

-- name: AdminRunOutcomesPerUser :many
-- Per-user LIFETIME outcome counts for the admin factory breakdown (PRD #1293 M1, D5).
-- Joins users so an outcome-only user (every run died before spending, so no usage row)
-- still has an email to render; the handler merges this by user id against the usage
-- rows. Lifetime-only, matching the admin per-user table's lifetime figures.
SELECT u.id AS user_id, u.email,
    count(*)::bigint                                                                       AS finished,
    count(*) FILTER (WHERE r.status = 'completed')::bigint                                 AS completed,
    count(*) FILTER (WHERE r.status = 'cancelled')::bigint                                 AS cancelled,
    count(*) FILTER (WHERE r.status = 'failed' AND r.fail_origin = 'plan_rejected')::bigint AS plan_rejected,
    count(*) FILTER (WHERE r.status = 'failed' AND r.fail_origin IS DISTINCT FROM 'plan_rejected')::bigint AS failed,
    -- needs_landing (issue #1418): SUB-CUT of `failed`; see SelfRunOutcomes for the shape.
    -- Lifetime-only, matching this query's other lifetime-only counts. The alias here is `r`,
    -- so the correlated EXISTS resolves to the OUTER `r` row (no JOIN — that would corrupt the
    -- sibling per-user counts).
    count(*) FILTER (
        WHERE r.status = 'failed'
          AND r.fail_origin = ANY(@landable_origins::text[])
          AND (EXISTS (SELECT 1 FROM recovery_captures c
                       WHERE c.run_id = r.id AND c.user_id = r.user_id AND c.state = 'available')
               OR r.preserved_patch IS NOT NULL)
    )::bigint AS needs_landing,
    -- fail_origins (issue #1451): this user's LIFETIME per-origin breakdown of `failed`, in
    -- this statement (one snapshot => sum(fail_origins) == failed by construction). Correlated
    -- on u.id; an empty group yields '{}'. See SelfRunOutcomes for the shape.
    (SELECT COALESCE(jsonb_object_agg(o.origin, o.cnt), '{}')::jsonb
        FROM (SELECT COALESCE(r2.fail_origin, 'unknown') AS origin, count(*) AS cnt
              FROM runs r2
              WHERE r2.user_id = u.id
              AND r2.status = 'failed' AND r2.fail_origin IS DISTINCT FROM 'plan_rejected'
              AND r2.kind NOT IN ('chat', 'judge')
              GROUP BY 1) o) AS fail_origins
FROM runs r
JOIN users u ON u.id = r.user_id
WHERE r.status IN ('completed', 'failed', 'cancelled')
  AND r.kind NOT IN ('chat', 'judge')
GROUP BY u.id, u.email
ORDER BY u.id;

-- History usage refold (PRD #1079 M3) ---------------------------------------
-- A boot one-shot re-folds every PRE-MIGRATION terminal non-chat run through the
-- SAME foldUsageFrames the incremental path uses, replacing the collapsed
-- (run, session, model) rows the old MAX-per-model key produced with correct
-- per-leg rows. 00188 added runs.usage_refolded (DEFAULT true, set false for every
-- non-chat row that existed at migration time), which scopes the job to history and
-- makes it converge: a post-migration run is born refolded and never selected here.

-- name: ListRunUsageFrames :many
-- A run's full status/error frame history in seq order — the exact frame shape
-- foldUsageFrames folds (never tool/text traffic). Column order matches
-- ListRunMessagesAfter so sqlc keeps returning store.RunMessage. status carries both
-- the `init` markers CountRunInitFramesBefore counts and the success result frames;
-- error carries a failed turn's result frame. This is a few dozen rows per run.
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation
FROM run_messages
WHERE run_id = @run_id AND kind IN ('status', 'error')
ORDER BY seq ASC;

-- name: ListRunsPendingUsageRefold :many
-- One batch of TERMINAL pre-migration runs still awaiting refold, oldest first. Chat
-- rows were never set usage_refolded=false (00188), so no kind filter is needed here;
-- restricting to terminal statuses keeps the refold from racing an in-flight fold (a
-- straggler re-delivery after a refold is a GREATEST no-op at the position-absolute
-- epoch, but only terminal runs are ever folded here). A run that is pre-migration and
-- still RUNNING at boot is deliberately NOT selected until it finishes — the awaiting
-- count below is what keeps the one-shot alive to catch it in-process.
-- @exclude_ids lets the caller skip runs that already failed to refold within the
-- current drain cycle so the batch advances past a poison row; id <> ALL('{}'::uuid[])
-- is TRUE for every row, so an empty exclusion list excludes nothing. The COALESCE
-- makes a NULL param behave as the empty array: pgx encodes a nil []uuid.UUID slice as
-- SQL NULL, and id <> ALL(NULL) is NULL (not TRUE), which would silently drop EVERY
-- pending run — so guard against a nil caller here rather than trust every call site to
-- pass a non-nil slice.
SELECT * FROM runs
WHERE NOT usage_refolded AND status IN ('completed', 'failed', 'cancelled')
  AND id <> ALL(COALESCE(@exclude_ids::uuid[], '{}'::uuid[]))
ORDER BY created_at
LIMIT @lim;

-- name: CountRunsAwaitingUsageRefold :one
-- Every run still marked un-refolded, TERMINAL OR NOT. This is the boot one-shot's
-- STOP condition: a pre-migration run still running at boot is usage_refolded=false
-- but not yet terminal, so the ticker must outlive it (re-checking until this reaches
-- zero) rather than stop when the terminal-only pending batch empties.
SELECT COUNT(*) FROM runs WHERE NOT usage_refolded;

-- name: MarkRunUsageRefolded :exec
-- Marks a run's usage as refolded per leg (PRD #1079 M3), inside RefoldRunUsage's
-- transaction so the delete+fold+mark commit atomically. Once true the ticker never
-- selects the run again and the incremental fold stays its only usage writer.
-- Deliberately does NOT touch updated_at: this is a one-off backfill over historical
-- terminal runs, and RunDTO exposes runs.updated_at, so bumping it would make every
-- pre-migration run appear recently updated in the API/UI. Refold is invisible to recency.
UPDATE runs SET usage_refolded = true WHERE id = @id;

-- name: DeleteRunUsage :exec
-- Clears a run's run_usage rows so RefoldRunUsage can re-fold from the frame history
-- without the old collapsed rows lingering. Runs in the same transaction as the fold;
-- a refold of a run with no result frames simply leaves zero rows behind.
DELETE FROM run_usage WHERE run_id = @run_id;

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
SELECT id, run_id, seq, kind, agent, payload, created_at, agent_instance, agent_label, claim_generation
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
        -- PRD #1226 M1 (D1): freeze the STRUCTURAL COMPLETION CONTRACT at the SAME approve
        -- freeze as milestones_frozen, IDEMPOTENTLY. The freeze condition is TIED to the
        -- milestone freeze: the run is INTERLOCKED (completion_contract_version IS NOT NULL,
        -- stamped at CreateRun when the interlock switch was on), the contract is not yet frozen
        -- (completion_contract IS NULL), AND the resolved milestone source is present — the SAME
        -- COALESCE(milestones_frozen, milestones_candidate) milestones_frozen above freezes from.
        -- So a double-approve or re-gate resume never re-freezes, a non-interlocked run keeps
        -- NULL, and a run with no candidate freezes no contract (consistent with milestones_frozen
        -- staying NULL — the interlock still holds via the version). The Go caller (submitApproval)
        -- builds @completion_contract from that same source. contract_revision is set to 1 in the
        -- SAME condition so revision and contract are always frozen together, never one alone.
        completion_contract = CASE
            WHEN runs.completion_contract_version IS NOT NULL AND runs.completion_contract IS NULL
                 AND COALESCE(runs.milestones_frozen, runs.milestones_candidate) IS NOT NULL
            THEN sqlc.narg('completion_contract')::jsonb
            ELSE runs.completion_contract END,
        contract_revision = CASE
            WHEN runs.completion_contract_version IS NOT NULL AND runs.completion_contract IS NULL
                 AND COALESCE(runs.milestones_frozen, runs.milestones_candidate) IS NOT NULL
            THEN 1
            ELSE runs.contract_revision END,
        -- PRD #122 M2 (Decision 5/5b): the per-run budget is derived at the SAME freeze,
        -- from the same COALESCE'd frozen source, and written IDEMPOTENTLY via COALESCE —
        -- a double-approve or a re-gate resume never changes a budget frozen once. NULL for
        -- a 0/1-milestone plan (byte-for-byte the global default). See SetRunRunning for the
        -- autopilot mirror of this compute.
        -- Issue #1181: the count<=1 arm no longer drops straight to NULL. A LARGE-repo run
        -- (size_class='l') floors to run_max_iterations*size_budget_factor_l iters and
        -- LEAST(run_timeout*size_budget_factor_l, ceiling) wall, so a large gated run whose
        -- lead wrote milestones as PROSE (0 structured milestones) still gets a size-scaled
        -- budget rather than the 5-iter/2h global default. 's'/'m'/'' stay NULL (unchanged,
        -- byte-for-byte the pre-feature default). Read bare runs.size_class: this statement
        -- does NOT SET size_class (it was persisted by the pre-gate SetRunAwaitingApproval
        -- report), so the OLD-row value is the committed one. The count>=2 arm is unchanged.
        -- See SetRunRunning for the autopilot mirror (which reads COALESCE(narg,size_class)).
        budget_max_iterations = COALESCE(runs.budget_max_iterations,
            CASE WHEN COALESCE(jsonb_array_length(COALESCE(runs.milestones_frozen, runs.milestones_candidate)), 0) <= 1
                     THEN CASE runs.size_class
                              WHEN 'l' THEN sqlc.arg('run_max_iterations')::int * sqlc.arg('size_budget_factor_l')::int
                              ELSE NULL END
                 ELSE sqlc.arg('run_max_iterations')::int * LEAST(jsonb_array_length(COALESCE(runs.milestones_frozen, runs.milestones_candidate)), sqlc.arg('milestone_budget_cap')::int) END),
        budget_wall_seconds = COALESCE(runs.budget_wall_seconds,
            CASE WHEN COALESCE(jsonb_array_length(COALESCE(runs.milestones_frozen, runs.milestones_candidate)), 0) <= 1
                     THEN CASE runs.size_class
                              WHEN 'l' THEN LEAST(sqlc.arg('run_timeout_seconds')::int * sqlc.arg('size_budget_factor_l')::int, sqlc.arg('budget_wall_ceiling_seconds')::int)
                              ELSE NULL END
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
--
-- Issue #1399: the write is refused on a terminal run IN SQL, not only by the caller's
-- unlocked status read. A submit that read `running` can block on the row lock of a
-- concurrent terminal transition (forge park, SetState), which settles the run's pending
-- scope rows after it commits; without a guard the blocked UPDATE then proceeded and left a
-- fresh pending scope row on the failed/cancelled run. Under READ COMMITTED the blocked
-- UPDATE re-checks its WHERE against the committed row version (EvalPlanQual), so the
-- `status NOT IN (...)` predicate refuses it once the lock holder commits terminal. A refused
-- write matches 0 rows in `capped`: `superseded` is gated on EXISTS (capped) so it settles
-- nothing, and the INSERT selects from `capped` so it writes nothing and yields
-- pgx.ErrNoRows (the CreateExtendInput shape), which the service maps to ErrRunTerminal.
WITH capped AS (
    UPDATE runs SET scope_ceiling = @scope_ceiling, updated_at = now()
    WHERE runs.id = @run_id
      AND runs.status NOT IN ('completed', 'failed', 'cancelled')
    RETURNING runs.id
),
superseded AS (
    UPDATE run_user_inputs SET disposition = 'superseded'
    WHERE run_user_inputs.run_id = @run_id AND run_user_inputs.kind = 'scope'
      AND run_user_inputs.disposition IS NULL
      AND EXISTS (SELECT 1 FROM capped)
)
INSERT INTO run_user_inputs (run_id, kind, body)
SELECT capped.id, 'scope', @body FROM capped
RETURNING *;

-- name: SettleScopeInputDisposition :execrows
-- PRD #634 M4: settle the still-pending scope audit row(s) at completion. After each
-- CreateScopeCeilingInput superseded its priors, exactly ONE scope row is NULL (the last),
-- so this settles that one. Idempotent (WHERE disposition IS NULL) — a second call is a
-- no-op and it never overwrites an already-settled ('superseded'/'applied') row.
UPDATE run_user_inputs SET disposition = @disposition
WHERE run_id = @run_id AND kind = 'scope' AND disposition IS NULL;

-- name: CreatePauseInput :one
-- Request a pause on a running run AND write the kind='pause' audit row in ONE statement
-- (PRD #1190 M1), mirroring CreateScopeCeilingInput's atomicity (workersvc.Store exposes no
-- transaction seam). The UPDATE sets the three pending-pause columns; pause_after_count is
-- len(milestones_completed) captured NOW — the floor the milestone mode waits to exceed. The
-- run STAYS running (Decision 3): a pending pause is a flag, not a status.
--
-- The predicate is the kind/interactive/status ALLOWLIST (Decisions 6/7): only a running,
-- non-interactive issue/task/prompt/self_improve run accepts a pause. A 0-row result is
-- disambiguated in Go (re-read the run: status != running -> ErrPauseNotRunning; kind or
-- interactive outside the allowlist -> ErrPauseNotSupported). On 0 rows the INSERT selects
-- from the empty CTE and writes NO audit row, so a refused pause leaves no trace.
--
-- A second pause while one is pending REPLACES the mode (the UPDATE overwrites), so a `now`
-- escalates a pending `milestone`; each accepted request still writes its own audit row.
--
-- The FINAL statement is the audit INSERT with RETURNING (CreateScopeCeilingInput's shape),
-- selecting from the UPDATE CTE — so a REFUSED request (UPDATE matched 0 rows) inserts 0 rows
-- and yields pgx.ErrNoRows, which the service maps to the 409 classes. On success the returned
-- run_user_inputs row is unused (the service re-reads the already-fetched run for the DTO).
WITH paused_req AS (
    UPDATE runs SET
        pause_requested_at = now(),
        pause_mode         = @mode,
        pause_after_count  = COALESCE(jsonb_array_length(runs.milestones_completed), 0),
        updated_at         = now()
    WHERE runs.id = @id AND runs.status = 'running'
      AND runs.kind IN ('issue', 'task', 'prompt', 'self_improve')
      AND runs.interactive = false
      -- PRD #1497 M1 (D18): an owner cannot overwrite the system's 'wall' request (IS DISTINCT FROM
      -- because pause_mode is NULL on a no-pending-pause row and NULL <> 'wall' is unknown). A 0-row
      -- result surfaces as the 409 "the run is parking because it reached its time limit".
      AND runs.pause_mode IS DISTINCT FROM 'wall'
    RETURNING runs.id
)
INSERT INTO run_user_inputs (run_id, kind, body)
SELECT paused_req.id, 'pause', @mode FROM paused_req
RETURNING *;

-- name: CancelPauseInput :one
-- Withdraw a pending pause AND write the kind='pause_cancel' audit row in ONE statement
-- (PRD #1190 M1), mirroring CreatePauseInput/CreateScopeCeilingInput. Clears the three
-- pending-pause columns where a request is actually pending on a still-running run; a 0-row
-- result (no pause pending, or the run is no longer running) is surfaced by Go as 409 "no
-- pause is pending". The FINAL statement is the audit INSERT with RETURNING (CreatePauseInput's
-- shape): a 0-row UPDATE yields pgx.ErrNoRows, which the service maps to the 409.
WITH cancelled AS (
    UPDATE runs SET
        pause_requested_at = NULL,
        pause_mode         = NULL,
        pause_after_count  = NULL,
        updated_at         = now()
    WHERE runs.id = @id AND runs.pause_requested_at IS NOT NULL AND runs.status = 'running'
      -- PRD #1497 M1 (D18): an owner cannot withdraw the system's 'wall' request (IS DISTINCT FROM
      -- because pause_mode is NULL on a no-pending-pause row). A 0-row result surfaces as the 409.
      AND runs.pause_mode IS DISTINCT FROM 'wall'
    RETURNING runs.id
)
INSERT INTO run_user_inputs (run_id, kind, body)
SELECT cancelled.id, 'pause_cancel', NULL::text FROM cancelled
RETURNING *;

-- name: CreateExtendInput :one
-- PRD #1189 M1: grant a run more wall-clock time AND write the kind='extend' audit row in
-- ONE statement, mirroring CreatePauseInput/CreateScopeCeilingInput's atomicity (workersvc.Store
-- exposes no transaction seam). The UPDATE ADDS @secs to budget_extension_seconds (additive —
-- the frozen budget_wall_seconds is never touched); the run_user_inputs row is audit/surfacing
-- ONLY (excluded from ConsumeRunInputs, so the worker never drains or routes it — the extension
-- is served to the worker as runs.budget_extension_seconds on the ACK/claim instead).
--
-- The predicate is the same non-terminal + timed-kind allowlist the sweep uses PLUS the cap:
-- a completed/failed/cancelled run, a chat/judge/interactive run (which never times out), or a
-- request that would push the total extension past @cap all match 0 rows. A 0-row result is
-- disambiguated in Go by re-reading the already-fetched run (terminal -> ErrRunTerminal; untimed
-- kind -> ErrExtendNotTimed; else -> ErrExtensionCapExceeded). On 0 rows the INSERT selects from
-- the empty CTE and writes NO audit row, so a refused extend leaves no trace.
--
-- disposition = 'applied' at insert (the CHECK from 00162 admits it): an extension takes effect
-- in the SAME statement, so there is nothing left to settle later — a NULL would leave the row
-- "pending" in `uzi run inputs` forever. The final RETURNING is a scalar over the CTE, COALESCEd
-- and cast so sqlc types it as a plain int32 (the new total); on a refusal the INSERT returns 0
-- rows and yields pgx.ErrNoRows, and the COALESCE default is never actually observed.
WITH extended AS (
    UPDATE runs SET budget_extension_seconds = budget_extension_seconds + sqlc.arg('secs')::int,
                    -- PRD #1497 M1 (D18): an extension on a running row with a pending SYSTEM 'wall'
                    -- request VOIDS the request (the owner bought time) — clear the three pause
                    -- columns; the CASE reads the OLD pause_mode so a milestone/now/NULL owner
                    -- request is left exactly as before. (A paused wall-parked row has pause_mode
                    -- already NULL and is extended via ExtendAndResumeWallPark, not this statement.)
                    pause_requested_at = CASE WHEN pause_mode = 'wall' THEN NULL ELSE pause_requested_at END,
                    pause_mode         = CASE WHEN pause_mode = 'wall' THEN NULL ELSE pause_mode END,
                    pause_after_count  = CASE WHEN pause_mode = 'wall' THEN NULL ELSE pause_after_count END,
                    updated_at = now()
    WHERE id = sqlc.arg('id')
      AND status NOT IN ('completed', 'failed', 'cancelled')
      AND kind NOT IN ('chat', 'judge')
      AND interactive = false
      AND budget_extension_seconds + sqlc.arg('secs')::int <= sqlc.arg('cap')::int
    RETURNING id, budget_extension_seconds
),
-- PRD #1497 M1 (D18): settle the voided wall request's input too, so ConsumeRunInputs never hands
-- the resumed flight a stale 'wall' abort. An ACKed wall receipt can already have
-- consumed_at set; applied_at stays NULL until delivery or this settlement.
consumed_wall AS (
    UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
    FROM extended e
    WHERE u.run_id = e.id AND u.kind = 'pause' AND u.body = 'wall' AND u.applied_at IS NULL
)
INSERT INTO run_user_inputs (run_id, kind, body, disposition)
SELECT sqlc.arg('id'), 'extend', sqlc.narg('body'), 'applied' FROM extended
RETURNING COALESCE((SELECT budget_extension_seconds FROM extended), 0)::int AS budget_extension_seconds;

-- name: ExtendAndResumeWallPark :one
-- PRD #1497 M1 (D7): the owner's Extend on a budget_exhausted wall park, in ONE statement — add the
-- extension AND resume, so an extension has one purpose and is one action. It:
--   1. adds @secs to budget_extension_seconds under the SAME cap guard as CreateExtendInput (a
--      request past @cap matches 0 rows -> ErrExtensionCapExceeded, worded for the park);
--   2. BANKS THE OVERRUN into budget_paused_seconds so the extension buys exactly @secs of ACTIVE
--      run-time however late the park landed (D7): overrun = active-at-park - total-before-extension,
--      and the parked-wait interval is banked too (ResumePausedRun's rule), so the two banks leave the
--      run resuming with precisely @secs of budget (proof: active_now = active_at_park - overrun =
--      total_before, remaining = (total_before + @secs) - total_before = @secs);
--   3. does ResumePausedRun's transition — queued, codex cap/epoch, health reset, CLEAR
--      hold_reason/hold_captured_head, and the D19 worker rule (a SERVER-parked row, claim_released_at
--      set, resumes with worker_id NULL and PRESERVES released_worker_*; a worker-side park keeps
--      worker_id);
--   4. consumes the wall input (D18) and writes the `extend` and `resume` audit rows.
-- total_before and active-at-park read the OLD row (SET RHS is pre-update): budget_extension_seconds
-- in total_before is the pre-extension value, budget_finalize_seconds is included (a run may have been
-- Stopped then re-parked). Refuses (0 rows) a row that is not a budget_exhausted paused hold, or over
-- the cap. Returns the new total extension (CreateExtendInput's scalar shape); the handler re-reads
-- the run for the deadline + resumed:true response.
WITH extended AS (
    -- Outer refs qualified `runs.` because the sibling data-modifying CTEs (on run_user_inputs) put
    -- that table in the analyzer's shared name scope; a bare `id` would read ambiguous.
    UPDATE runs SET
        budget_extension_seconds = runs.budget_extension_seconds + sqlc.arg('secs')::int,
        budget_paused_seconds = runs.budget_paused_seconds
            + GREATEST(0, (GREATEST(0, EXTRACT(EPOCH FROM (runs.status_since - runs.started_at))::int) - runs.budget_paused_seconds)
                          - (COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                             + runs.budget_extension_seconds + runs.budget_finalize_seconds))
            + GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int),
        status = 'queued',
        status_since = now(),
        worker_id = CASE WHEN runs.claim_released_at IS NOT NULL THEN NULL ELSE runs.worker_id END,
        hold_reason = NULL,
        hold_captured_head = NULL,
        codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
        health = 'ok', health_reason = NULL, health_since = NULL,
        updated_at = now()
    WHERE runs.id = sqlc.arg('id') AND runs.user_id = sqlc.arg('user_id')
      AND runs.status = 'paused'
      AND runs.hold_reason = 'budget_exhausted'
      AND runs.budget_extension_seconds + sqlc.arg('secs')::int <= sqlc.arg('cap')::int
    RETURNING budget_extension_seconds
),
consumed_wall AS (
    UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
    WHERE u.run_id = sqlc.arg('id') AND u.kind = 'pause' AND u.body = 'wall' AND u.applied_at IS NULL
      AND EXISTS (SELECT 1 FROM extended)
),
extend_audit AS (
    -- The audit rows use sqlc.arg('id') for run_id and `FROM extended` only for the row count (the
    -- CreateExtendInput shape), so no bare `id` collides with run_user_inputs.id in the INSERT scope.
    INSERT INTO run_user_inputs (run_id, kind, body, disposition)
    SELECT sqlc.arg('id'), 'extend', sqlc.narg('body'), 'applied' FROM extended
),
resume_audit AS (
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT sqlc.arg('id'), 'resume', NULL::text FROM extended
)
SELECT COALESCE((SELECT budget_extension_seconds FROM extended), 0)::int AS budget_extension_seconds;

-- name: StopWallPark :one
-- PRD #1497 M1 (D9): the owner's Stop on a budget_exhausted wall park of a milestone ISSUE run with
-- at least one completed milestone, in ONE statement — cap the scope at the completed count and grant
-- a fixed, one-time finalize allowance, then resume so the re-claimed worker finalizes into a merge
-- request exactly as a stop on a running run does. It:
--   1. sets scope_ceiling = @scope_ceiling (the completed count, the CreateScopeCeilingInput shape),
--      marking any prior unsettled scope rows superseded;
--   2. grants budget_finalize_seconds = 1800 WHERE budget_finalize_seconds = 0 — the once-only marker
--      AND the accounting: it lives OUTSIDE budget_extension_seconds so the owner cap never sees it,
--      and a second Stop matches 0 rows (allowance already used) -> the handler answers 409;
--   3. banks the overrun + parked interval so the run resumes with exactly 1800s of finalize budget
--      (the same two-bank rule as ExtendAndResumeWallPark; budget_finalize_seconds is OLD (=0) in the
--      total-before, so the grant is the whole new headroom);
--   4. does ResumePausedRun's transition incl. the D19 worker rule, consumes the wall input (D18),
--      and writes the `scope` audit row (disposition NULL, body naming the ceiling + the 1800s grant)
--      and a `resume` row.
-- Refuses (0 rows) a non-issue kind, an interactive run, zero completed milestones, a non-budget_
-- exhausted hold, or an allowance already granted. Returns the run id; the handler re-reads for the
-- response.
WITH stopped AS (
    -- Outer refs qualified `runs.` for the shared-scope reason above (sibling run_user_inputs CTEs).
    UPDATE runs SET
        scope_ceiling = sqlc.arg('scope_ceiling')::int,
        budget_finalize_seconds = 1800,
        budget_paused_seconds = runs.budget_paused_seconds
            + GREATEST(0, (GREATEST(0, EXTRACT(EPOCH FROM (runs.status_since - runs.started_at))::int) - runs.budget_paused_seconds)
                          - (COALESCE(runs.budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                             + runs.budget_extension_seconds + runs.budget_finalize_seconds))
            + GREATEST(0, EXTRACT(EPOCH FROM (now() - runs.status_since))::int),
        status = 'queued',
        status_since = now(),
        worker_id = CASE WHEN runs.claim_released_at IS NOT NULL THEN NULL ELSE runs.worker_id END,
        hold_reason = NULL,
        hold_captured_head = NULL,
        codex_cap_hash = NULL, codex_claim_epoch = runs.codex_claim_epoch + 1,
        health = 'ok', health_reason = NULL, health_since = NULL,
        updated_at = now()
    WHERE runs.id = sqlc.arg('id') AND runs.user_id = sqlc.arg('user_id')
      AND runs.status = 'paused'
      AND runs.hold_reason = 'budget_exhausted'
      AND runs.kind = 'issue'
      AND runs.interactive = false
      AND runs.budget_finalize_seconds = 0
      AND COALESCE(jsonb_array_length(runs.milestones_completed), 0) >= 1
    RETURNING id
),
superseded AS (
    -- Gated on EXISTS(stopped) (matching the sibling consumed_wall/scope_audit/resume_audit CTEs):
    -- a TOCTOU where the run left the eligible state (0 stopped rows) must NOT supersede a prior
    -- pending `scope` audit row, or a later finalize's disposition-settle would find nothing to
    -- settle. Computed AFTER `stopped` so it can reference it. Postgres runs every CTE against the
    -- one pre-statement snapshot, so this never sees (and never supersedes) scope_audit's new row.
    UPDATE run_user_inputs SET disposition = 'superseded'
    WHERE run_user_inputs.run_id = sqlc.arg('id') AND run_user_inputs.kind = 'scope' AND run_user_inputs.disposition IS NULL
      AND EXISTS (SELECT 1 FROM stopped)
),
consumed_wall AS (
    UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
    WHERE u.run_id = sqlc.arg('id') AND u.kind = 'pause' AND u.body = 'wall' AND u.applied_at IS NULL
      AND EXISTS (SELECT 1 FROM stopped)
),
scope_audit AS (
    -- sqlc.arg('id') for run_id, `FROM stopped` only for the row count — no bare `id`/`run_id`
    -- collides with run_user_inputs' own columns in the INSERT scope.
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT sqlc.arg('id'), 'scope', sqlc.narg('body') FROM stopped
),
resume_audit AS (
    INSERT INTO run_user_inputs (run_id, kind, body)
    SELECT sqlc.arg('id'), 'resume', NULL::text FROM stopped
)
SELECT stopped.id FROM stopped;

-- name: ConsumeRunInputs :many
-- FIFO consume: apply and return every unapplied worker input for the run, oldest first.
-- FOR UPDATE SKIP LOCKED keeps two concurrent polls from returning the same row.
-- ACKed but unapplied inputs transfer to a legacy worker after a claim handoff.
WITH pending AS (
    SELECT p.id, (p.consumed_at IS NULL)::boolean AS first_consumption FROM run_user_inputs p
    -- PRD #634 M2: the scope audit row (kind='scope') is server-side ONLY — the control it
    -- carries travels as runs.scope_ceiling on the ACK/claim, never through this queue — so
    -- the worker must NEVER drain it. Draining would hit SteeringChannel.route's default arm
    -- and log a spurious "unknown input kind". PRD #1190 adds 'resume' to that server-only
    -- set (the resume endpoint writes it as an audit row; the run's return to 'queued' is a
    -- server-side transition, not a worker steering input). PRD #1226 M5 (D7) adds
    -- 'completion_decision' for the SAME reason: the owner continue-decision is a dedicated
    -- endpoint's AUDIT row — its control travels via a SEPARATE input the worker DOES drain
    -- (paused branch: a 'follow_up' the resumed claim's pullFollowUp reads, alongside the
    -- paused → queued transition through ResumePausedRun; live awaiting_input branch: an
    -- 'answer' that resolves the worker's completion-question await), never this raw row — so
    -- draining it would likewise hit the default arm. PRD #1189 adds 'extend' for the SAME
    -- reason: the extend audit row is server-only exactly like 'scope' — its control travels
    -- as runs.budget_extension_seconds on the ACK/claim, never through this queue — so the
    -- worker must never drain or route it either. 'pause' and
    -- 'pause_cancel' are NOT excluded — the worker DOES consume them (the `now` abort and the
    -- flag clear). Everything else consumes as before.
    WHERE p.run_id = @run_id AND p.applied_at IS NULL AND p.kind NOT IN ('scope', 'resume', 'completion_decision', 'extend')
    ORDER BY p.id ASC
    LIMIT 1000
    FOR UPDATE SKIP LOCKED
),
consumed AS (
    UPDATE run_user_inputs u SET consumed_at = COALESCE(u.consumed_at, now()), applied_at = now()
    FROM pending WHERE u.id = pending.id
    RETURNING u.id, u.kind, u.body, u.created_at
)
SELECT consumed.id, consumed.kind, consumed.body, consumed.created_at, pending.first_consumption
FROM consumed JOIN pending USING (id) ORDER BY consumed.id ASC;

-- name: ListReplayRunInputs :many
SELECT id, kind, body, created_at FROM run_user_inputs
WHERE run_id = @run_id AND applied_at IS NULL
  AND kind NOT IN ('scope', 'resume', 'completion_decision', 'extend')
ORDER BY id ASC
LIMIT 1000;

-- name: LockRunForInputReceipt :one
SELECT id, status, worker_id, claim_generation, claim_released_at, credential_switch_requested_at,
       credential_switch_generation
FROM runs WHERE id = @run_id FOR UPDATE;

-- name: ListInputReceiptRows :many
SELECT id, kind, body, created_at, consumed_at, consumed_claim_generation, consumed_worker_id, applied_at
FROM run_user_inputs WHERE run_id = @run_id AND id = ANY(@ids::bigint[])
  AND kind NOT IN ('scope', 'resume', 'completion_decision', 'extend')
ORDER BY id ASC;

-- name: AckRunInputRows :many
UPDATE run_user_inputs SET consumed_at = COALESCE(consumed_at, now()), consumed_claim_generation = @claim_generation,
    consumed_worker_id = @worker_id
WHERE run_id = @run_id AND id = ANY(@ids::bigint[]) AND applied_at IS NULL
RETURNING id, kind, body, created_at;

-- name: ApplyRunInputRows :execrows
UPDATE run_user_inputs SET applied_at = now()
WHERE run_id = @run_id AND id = ANY(@ids::bigint[])
  AND consumed_claim_generation = @claim_generation AND consumed_worker_id = @worker_id
  AND consumed_at IS NOT NULL AND applied_at IS NULL;

-- name: ListConsumedFollowUpInputsForRun :many
-- Issue #1660: the run's ALREADY-CONSUMED follow_up inputs, oldest first, for a worker to
-- rehydrate its operator constraints on every claim (first and re-claim), so a follow-up a
-- previous claim consumed still reaches the subagents this claim dispatches. follow_up only:
-- answer, revise_plan, scope, pause and the rest are not operator constraints. READ ONLY and
-- UNCAPPED like ListFollowUpInputsForRun: the worker fits the set into its prompt budget and
-- must not lose an entry here. Worker-ownership is enforced at the run resolve
-- (GetRunOwnedByWorker), not here. Pending rows are excluded: the live /inputs drain delivers
-- those, and the worker de-duplicates the two by id. Received rows are included whether or not
-- they are applied (issue #1673): an ACKed-but-unapplied follow-up from a prior claim is already
-- a constraint, and a subagent dispatched before the live GET/ACK replays it must carry it. The
-- replay still reaches the lead once, by id. Ordered by id, the same rule as the /inputs FIFO
-- (ConsumeRunInputs), so the worker keeps the server's order as is.
SELECT id, body, created_at FROM run_user_inputs
WHERE run_id = @run_id AND kind = 'follow_up' AND consumed_at IS NOT NULL
ORDER BY id ASC;

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
SELECT id, run_id, kind, body, consumed_at, created_at, question_id, disposition, consumed_claim_generation, consumed_worker_id, applied_at FROM run_user_inputs
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
JOIN forge_connections c ON c.id = rp.connection_id AND c.user_id = r.user_id -- #1688: owner-scoped token
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
JOIN forge_connections c ON c.id = rp.connection_id AND c.user_id = r.user_id -- #1688: owner-scoped token
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
-- Only a run with a board card (issue_iid set) is ever stamped: every terminal writer
-- sets move_pending_since = CASE WHEN issue_iid IS NOT NULL THEN now() END (issue
-- #1482), so a judge/chat/ci_fix/prompt/task run never enters this loop or the
-- give-up warning below, whose "manual heal" cannot exist for a card-less run.
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
-- the ONLY SQL statement that writes runs.mr_state (the watcher-owned invariant,
-- review finding 11): no run-status path writes it. It now has TWO Go callers over
-- DISJOINT run sets — SyncMRStates (issue-lane runs, board-coupled) and
-- SyncBoardFreeMRStates (issue-less MR-bearing runs, board-free) — PRD #908; each
-- records the state for its own lane through this one statement. The two lanes never
-- overlap: an issue-less run never matches the board lane's issues JOIN, and
-- prompt/self_improve are excluded by its CTE (see recordMRState). The run itself
-- stays terminal — closing an MR is review feedback, not a run-status event — so
-- this touches mr_state (and updated_at) only.
UPDATE runs SET mr_state = @mr_state, updated_at = now()
WHERE id = @id;

-- name: SetRunCheckpointTip :execrows
-- Record the branch tip the worker last checkpoint-published for this run. Unlike
-- SetRunMRState this deliberately does NOT bump updated_at: checkpoint_tip is
-- internal bookkeeping written on every publish, and perturbing updated_at would
-- disturb the claim-affinity ordering (ClaimRun reads r.updated_at for queued rows)
-- for a value no user ever sees.
--
-- PRD #1190: also stamp checkpoint_tip_at (a separate timestamp, not updated_at) so the
-- pause UI can name the age of the last checkpoint — the "work since the last checkpoint"
-- a `now` pause discards. It is written on every publish alongside checkpoint_tip.
UPDATE runs SET checkpoint_tip = @checkpoint_tip, checkpoint_tip_at = now() WHERE id = @id;

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
-- PRD #1170: budget_wall_seconds, budget_paused_seconds and interactive ride this read
-- so the running-run near-timeout arm mirrors SweepRunningTimeout's exact clock — active
-- running time is wall clock since started_at MINUS budget_paused_seconds, measured against
-- the run's EFFECTIVE timeout (budget_wall_seconds when frozen, else the global RUN_TIMEOUT).
-- budget_wall_seconds is NULL for a run on the global default. interactive lets the arm skip
-- interactive runs, which SweepRunningTimeout never times out.
-- PRD #1189: budget_extension_seconds rides this read too so the near-timeout arm measures
-- against the EXTENDED effective timeout — the extension moves the 85% line and the flag
-- clears on the next tick once the arm no longer fires.
-- PRD #84 M3: repo_id, kind and required_capabilities ride this read so the queued
-- arm can surface a capability-specific "no eligible worker" reason (required caps not
-- a subset of any online worker's effective caps). kind was previously only a WHERE
-- filter; it is projected now so the resolver can branch on it too. No sweeper change —
-- a parked run stays queued and every sweep pass is scoped away from it by construction.
-- PRD #1226 M1: completion_contract_version rides this read so the queued arm can surface a
-- completion-capability reason for an INTERLOCKED run (version non-null) that no online worker
-- implements the completion protocol — the non-bypassable claim clause can never be satisfied.
-- PRD #1332 M5A (D3): harness, codex_material_revision and codex_secret_id ride this read so the
-- queued arm can surface a Codex-capability reason for a CODEX-INDICATING run (any of the three set)
-- that no online worker advertises 'codex_harness_v1' — the non-bypassable Codex claim clause can
-- never be satisfied. The resolver checks all three, mirroring the claim gate's fail-closed test.
-- PRD #1391 M5 (owner-gated outbox reason): worker_id rides this read so the running-run stalled arm
-- applies reasonOutboxQueued ONLY when the run's CURRENT owning worker is the same worker that
-- reported the outbox depth. Without it a cross-tenant worker (or a stale runIndex entry left by a
-- reclaim-during-outage) could flip a genuinely-stalled run to the reassuring "queued" reason.
-- worker_id is NULL for an unclaimed run (ON DELETE SET NULL), so the arm requires it be non-null.
-- PRD #1497 M1: budget_finalize_seconds rides this read so the near-timeout arm measures against
-- the full three-term effective timeout; released_worker_id rides it so the queued arm can surface
-- the "restart worker <name>" reason when a server-side wall park barred the only capable incarnation.
-- PRD #1551 M4 (D6): codex_custom_root is a SQL-computed boolean projecting ClaimRun's
-- custom-Codex-model predicate onto each row, so the queued arm can surface a custom-Codex-capability
-- reason for a run whose EFFECTIVE worker-root model is a CUSTOM (non-curated) id that no online
-- worker advertising codex_custom_model_v1 can claim. The effective-root expression is written
-- IDENTICALLY to ClaimRun (a frozen curated runs.model wins, otherwise the owner's default_codex_model
-- lane; NULL-safe via COALESCE), and the exemption is keyed on review_target_run_id/kind (task-review,
-- judge and chat are exempt), NEVER on all of kind='task'. Cast ::boolean so sqlc types it as a usable
-- bool (an expression is interface{} without the cast, per .claude/rules/go.md).
-- PRD #1590 M2 (D2, A1): codex_account_gated projects ClaimRun's Codex account gate onto each
-- row (the SAME predicate text, evaluated over a self-join aliased r so the copies stay
-- byte-identical), so the queued arm names the account hold instead of a worker reason for a
-- queued run the gate excludes in the window before the park_codex_account_unavailable pass
-- moves it to recovery_wait.
SELECT id, user_id, status, auto_approve,
       started_at, last_activity_at, updated_at, status_since,
       health, health_reason, health_since, health_notified_at,
       budget_wall_seconds, budget_paused_seconds, budget_extension_seconds, budget_finalize_seconds, interactive,
       repo_id, kind, dispatched_at, required_capabilities, completion_contract_version,
       harness, codex_material_revision, codex_secret_id, worker_id, released_worker_id,
       (runs.harness = 'codex'
        AND runs.kind NOT IN ('judge', 'chat')
        AND runs.review_target_run_id IS NULL
        AND COALESCE(
            NOT ((CASE WHEN runs.model = ANY(@codex_curated_models::text[]) THEN runs.model
                       ELSE (SELECT u.default_codex_model FROM users u WHERE u.id = runs.user_id) END)
                 = ANY(@codex_curated_models::text[])),
            false))::boolean AS codex_custom_root,
       (EXISTS (
           SELECT 1 FROM runs r
           WHERE r.id = runs.id
             AND r.harness = 'codex'
             AND r.codex_auth_mode = 'subscription'
             AND r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
             AND EXISTS (
                 SELECT 1 FROM codex_credential_state ccs
                 LEFT JOIN codex_provider_account cpa
                     ON cpa.id = ccs.provider_account_id AND cpa.user_id = r.user_id
                 WHERE ccs.user_secret_id = r.codex_secret_id
                   AND ccs.user_id = r.user_id
                   AND (
                       (cpa.coord_state = 'quarantined'
                        AND cpa.credential_revision = r.codex_account_revision
                        AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                                 THEN r.codex_account_key::jsonb
                                      = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                                 ELSE false END)
                       OR (r.codex_account_key IS NOT NULL
                           AND ccs.material_revision > r.codex_material_revision
                           AND (ccs.status IN ('staging', 'failed')
                                OR (ccs.status = 'linked'
                                    AND cpa.credential_revision = r.codex_account_revision
                                    AND CASE WHEN pg_input_is_valid(r.codex_account_key, 'jsonb')
                                             THEN r.codex_account_key::jsonb
                                                  = jsonb_build_array(cpa.provider_user_id, cpa.workspace_account_id)
                                             ELSE false END)))
                   )
             )
       ))::boolean AS codex_account_gated
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

-- name: ListRunLeadToolWindow :many
-- The tail of a running run's LEAD lane for in-flight detection only (Decision 9,
-- issue #1394). ListRunToolWindow above mixes the lead's rows with every nested
-- subagent's, so a subagent's completed calls hid the lead's still-open parent
-- `Agent` dispatch and a working run read as stalled. This query keeps
-- the lead lane alone (agent_instance IS NULL: nested frames carry the SDK's
-- parent_tool_use_id there, migration 00075) and adds the lead's lifecycle
-- boundaries, the `init` status and the `result` status/error, so the Go side can
-- stop its scan at the start of the current claim/query leg and never count an
-- orphaned call from an earlier leg as in flight. Loop detection keeps reading
-- ListRunToolWindow unchanged. The Go side re-checks kind and payload event
-- itself rather than trusting this filter alone.
SELECT seq, kind, payload
FROM run_messages
WHERE run_id = @run_id AND agent_instance IS NULL
  AND (kind IN ('tool_use', 'tool_result')
       OR (kind IN ('status', 'error') AND payload->>'event' IN ('init', 'result')))
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

-- name: CountOnlineWorkersClaimableForRun :one
-- PRD #1497 M1 (D19): how many of the run's owner's workers could ACTUALLY claim THIS ONE run right
-- now. Unlike CountOnlineEligibleWorkersForRepo (a static-eligibility count that ignores draining and
-- busy workers, unchanged), this mirrors ClaimRun's FULL per-worker conjunction for one run, because
-- the earlier eligibility rungs do not COMPOSE: one worker can pass the capability rung while another
-- passes the Codex rung and no single worker can claim. It feeds the new queued-health rung — when it
-- is 0 and the run's released pair names an online worker, the reason is "restart worker <name>".
--
-- The conjunction, matching ClaimRun: live (last_heartbeat_at >= @heartbeat_cutoff), not draining, a
-- free slot (NULL cap = unbounded), fn_worker_can_claim (the docker-allowlist + capability-subset
-- fence, capability_aware mirroring the claim), the non-bypassable completion_interlock_v1 and
-- codex_harness_v1 protocol clauses, the ephemeral binding (an ephemeral worker claims ONLY its bound
-- run), and NOT the run's released incarnation (D19: the exact worker+nonce a server park excluded,
-- with the leading IS NULL arm so an ordinary run counts every worker). Active count uses the SAME
-- run-lane definition as ClaimRun's fleet spread.
SELECT count(*)
FROM runs run
JOIN workers w ON w.user_id = run.user_id
CROSS JOIN LATERAL (
    SELECT count(*) AS active FROM runs pr
    WHERE pr.worker_id = w.id
      AND pr.status IN ('claimed', 'running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')
      AND pr.kind <> 'chat'
) wa
WHERE run.id = @run_id
  AND w.last_heartbeat_at IS NOT NULL
  AND w.last_heartbeat_at >= @heartbeat_cutoff
  AND w.draining_since IS NULL
  AND (w.max_concurrent_runs IS NULL OR wa.active < w.max_concurrent_runs)
  AND fn_worker_can_claim(
        COALESCE(w.docker_enabled, false),
        @docker_repo_allowlist::uuid[],
        run.repo_id,
        run.kind,
        COALESCE(w.capabilities, '{}')::text[],
        run.required_capabilities,
        @capability_aware::boolean)
  AND (run.completion_contract_version IS NULL
       OR 'completion_interlock_v1' = ANY(w.protocol_capabilities))
  AND (NOT (run.harness = 'codex' OR run.codex_material_revision IS NOT NULL OR run.codex_secret_id IS NOT NULL)
       OR 'codex_harness_v1' = ANY(w.protocol_capabilities))
  AND (run.completion_contract_version IS NULL
       OR NOT (run.harness = 'codex' OR run.codex_material_revision IS NOT NULL OR run.codex_secret_id IS NOT NULL)
       OR 'codex_completion_interlock_v1' = ANY(w.protocol_capabilities))
  -- PRD #1551 M4 (D6): MIRROR ClaimRun's non-bypassable custom-Codex-model clause, so this
  -- claimable count and the claim gate never disagree. The effective-root expression is written
  -- IDENTICALLY to ClaimRun (run.model/curated-else-lane, NULL-safe via COALESCE), reading the
  -- candidate worker's OWN protocol_capabilities.
  AND (
      NOT (
          run.harness = 'codex'
          AND run.kind NOT IN ('judge', 'chat')
          AND run.review_target_run_id IS NULL
          AND COALESCE(
              NOT ((CASE WHEN run.model = ANY(@codex_curated_models::text[]) THEN run.model
                         ELSE (SELECT u.default_codex_model FROM users u WHERE u.id = run.user_id) END)
                   = ANY(@codex_curated_models::text[])),
              false)
      )
      OR 'codex_custom_model_v1' = ANY(w.protocol_capabilities)
  )
  AND (NOT w.ephemeral OR w.ephemeral_run_id = run.id)
  AND (run.released_worker_id IS NULL
       OR run.released_worker_id <> w.id
       OR run.released_worker_nonce IS DISTINCT FROM w.snapshot_register_nonce);

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
  -- AND NOT w.ephemeral (PRD #529 M2, Correction B; issue #1624): an ephemeral worker is
  -- bound to ONE run (it can claim only its ephemeral_run_id), so it never has a free
  -- slot for ANOTHER run, even when idle, with a NULL cap, or below an advertised cap.
  -- Counting it would make a saturated fleet read as having an idle worker.
  AND NOT w.ephemeral
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

-- name: CountOnlineWorkersSatisfyingProtocol :one
-- PRD #1226 M1: how many of a user's ONLINE, non-draining, non-ephemeral workers self-report
-- the 'completion_interlock_v1' PROTOCOL capability (workers.protocol_capabilities). This is the
-- completion-interlock analogue of CountOnlineWorkersSatisfyingCaps: it answers "does the fleet
-- have ANY worker that implements the completion protocol?", NOT "can THIS run be claimed right
-- now?". It drives the queued-reason resolver's completion-capability rung (reasonNoCompletion
-- CapableWorker): a 0 here for an INTERLOCKED queued run means every online worker predates the
-- protocol, so the run's NON-BYPASSABLE claim clause (ClaimRun's `'completion_interlock_v1' =
-- ANY(@worker_protocol_caps)`) can never be satisfied — a persistent block distinct from the
-- generic wait, an ordinary capability gap, or the docker-allowlist fence.
--
-- It reads workers.protocol_capabilities DIRECTLY (the server-authoritative FilterProtocol'd
-- column), NOT the effective-caps fold: the completion protocol is a self-reported protocol
-- capability, deliberately OUTSIDE required_capabilities / the docker union / the capability-aware
-- kill-switch (see ClaimRun's dedicated clause). draining_since IS NULL and NOT w.ephemeral mirror
-- CountOnlineWorkersSatisfyingCaps for the same reasons (a draining worker claims nothing; an
-- ephemeral worker is bound to one run and can never satisfy a different one). Only called for an
-- interlocked queued run already past its health threshold, so it is off the hot path.
SELECT count(*) FROM workers w
WHERE w.user_id = @user_id
  AND w.status = 'online'
  AND w.draining_since IS NULL
  AND NOT w.ephemeral
  AND 'completion_interlock_v1' = ANY(w.protocol_capabilities);

-- name: CountOnlineWorkersSatisfyingCodexHarness :one
-- PRD #1332 M5A (D3): how many of a user's ONLINE, non-draining, non-ephemeral workers self-report
-- the 'codex_harness_v1' PROTOCOL capability (workers.protocol_capabilities). This is the
-- Codex-harness analogue of CountOnlineWorkersSatisfyingProtocol: it answers "does the fleet have
-- ANY worker that can execute a Codex run?", NOT "can THIS run be claimed right now?". It drives the
-- queued-reason resolver's Codex-capability rung (reasonNoCodexCapableWorker): a 0 here for a
-- CODEX-INDICATING queued run means every online worker predates the probe/receipt, so the run's
-- NON-BYPASSABLE claim clause (ClaimRun's `'codex_harness_v1' = ANY(@worker_protocol_caps)`) can
-- never be satisfied — a persistent block distinct from the generic wait, an ordinary capability
-- gap, the completion-interlock block, or the docker-allowlist fence.
--
-- It reads workers.protocol_capabilities DIRECTLY (the server-authoritative FilterProtocol'd
-- column), NOT the effective-caps fold: the Codex harness is a self-reported protocol capability,
-- deliberately OUTSIDE required_capabilities / the docker union / the capability-aware kill-switch
-- (see ClaimRun's dedicated clause). draining_since IS NULL and NOT w.ephemeral mirror
-- CountOnlineWorkersSatisfyingProtocol for the same reasons (a draining worker claims nothing; an
-- ephemeral worker is bound to one run and can never satisfy a different one). Only called for a
-- Codex-indicating queued run already past its health threshold, so it is off the hot path.
SELECT count(*) FROM workers w
WHERE w.user_id = @user_id
  AND w.status = 'online'
  AND w.draining_since IS NULL
  AND NOT w.ephemeral
  AND 'codex_harness_v1' = ANY(w.protocol_capabilities);

-- name: CountOnlineWorkersSatisfyingCodexCompletion :one
-- Static protocol intersection for an interlocked Codex run. Ignore free slots and the
-- released incarnation: those are transient availability constraints handled later.
SELECT count(*) FROM workers w
WHERE w.user_id = @user_id
  AND w.status = 'online'
  AND w.draining_since IS NULL
  AND NOT w.ephemeral
  AND 'completion_interlock_v1' = ANY(w.protocol_capabilities)
  AND 'codex_harness_v1' = ANY(w.protocol_capabilities)
  AND 'codex_completion_interlock_v1' = ANY(w.protocol_capabilities)
  AND (NOT @custom_root::boolean OR 'codex_custom_model_v1' = ANY(w.protocol_capabilities));

-- name: CountOnlineWorkersSatisfyingCustomCodex :one
-- PRD #1551 M4 (D6): how many of a user's ONLINE, non-draining, non-ephemeral workers self-report
-- BOTH the 'codex_harness_v1' AND 'codex_custom_model_v1' PROTOCOL capabilities. This is the
-- custom-Codex-model analogue of CountOnlineWorkersSatisfyingCodexHarness: it answers "does the
-- fleet have ANY worker that can execute a CUSTOM Codex root model?", NOT "can THIS run be claimed
-- right now?". It drives the queued-reason resolver's custom-Codex rung (reasonNoCustomCodexCapable
-- Worker): a 0 here for a CUSTOM-root queued run means every online worker predates the passthrough
-- renderer, so the run's NON-BYPASSABLE custom-model claim clause (ClaimRun's
-- 'codex_custom_model_v1' = ANY(@worker_protocol_caps)) can never be satisfied — a persistent block
-- distinct from the plain Codex-harness gap, the generic wait, or the docker-allowlist fence.
--
-- It requires codex_harness_v1 too because a custom-root run must first pass the Codex-harness gate:
-- a worker with codex_custom_model_v1 but not codex_harness_v1 cannot claim it (both ClaimRun
-- clauses apply), and such a worker cannot exist in practice (the agent advertises the pair
-- together), but the AND keeps this count exactly the set that could actually claim. Reads
-- workers.protocol_capabilities DIRECTLY, like its siblings; draining_since IS NULL and NOT
-- w.ephemeral for the same reasons. Only called for a custom-root queued run already past its
-- health threshold, so it is off the hot path.
SELECT count(*) FROM workers w
WHERE w.user_id = @user_id
  AND w.status = 'online'
  AND w.draining_since IS NULL
  AND NOT w.ephemeral
  AND 'codex_harness_v1' = ANY(w.protocol_capabilities)
  AND 'codex_custom_model_v1' = ANY(w.protocol_capabilities);

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

-- name: GetRunOwnedByWorkerForUpdate :one
-- PRD #1226 M2 (D4): the completion transaction's row lock. completeRunWithPermit opens a
-- pgx transaction and SELECTs the run FOR UPDATE through this so two concurrent completed
-- reports (a retry after response loss) serialize — the second blocks until the first
-- commits and then sees the terminal row. Worker-scoped like GetRunOwnedByWorker: a run the
-- worker does not hold returns pgx.ErrNoRows.
SELECT * FROM runs WHERE id = @id AND worker_id = @worker_id FOR UPDATE;

-- name: RecordCompletionAttempt :one
-- PRD #1226 M2 (D4) + M3: record ONE gated completion attempt and return the run's new attempt
-- count, ATOMICALLY and BOUNDED. Fired on the permit path's missing-milestones denial (with a
-- head, no worktree fingerprint, no declaration) and by the same-lead nudge endpoint (with head,
-- worktree fingerprint AND the lead's declared milestones_completed). In one statement it:
--   1. inserts the attempt row (`ins`) GUARDED to an INTERLOCKED run OWNED by the worker
--      (completion_contract_version IS NOT NULL); the INSERT ... SELECT ... FROM runs yields no
--      row and inserts nothing when the guard fails. Data-modifying CTEs always run to
--      completion even when the primary query does not read them.
--   2. PRUNES the run's attempt log to the most recent N: it keeps the 49 newest EXISTING
--      rows (the prune subquery reads the pre-INSERT snapshot, so the just-inserted row is not
--      visible to it and can never be pruned) and this insert adds one, so the table lands at
--      <= 50 rows per run — the bound that keeps the log from growing without limit. Gated on
--      EXISTS(ins) so it only fires when the attempt was actually recorded.
--   3. UNION-MERGES the lead's declared milestones_completed into runs.milestones_completed
--      (PRD #1226 M3): the M3 review flagged the "agent must persist its declaration before the
--      attempt" ordering hazard, so the attempt endpoint now persists it in the SAME statement.
--      The CASE is copied VERBATIM from SetRunRunning's milestones_completed union (monotone,
--      DISTINCT dedup); a NULL @milestones_completed (the permit path, which carries no
--      declaration) leaves the column untouched, byte-identical to before. The ids are
--      subset-validated against the frozen list SERVER-SIDE (progressParams) before this call.
--   4. increments runs.completion_attempts (the SweepRunningTimeout carve-out reads it) and
--      overwrites runs.latest_completion_attempt with the current summary (server now()), under
--      the SAME guard so a non-owning/legacy call changes nothing. @unmet is the SERVER
--      recompute done in Go AFTER the same union above (computeUnmetCriteria over the merged
--      set), so the persisted unmet is consistent with the persisted milestones_completed. The
--      final UPDATE takes the runs row lock (serializing concurrent attempts) and returns 0
--      rows -> pgx.ErrNoRows when the guard fails — the caller's "not recorded" signal.
--
-- The outer references are qualified `runs.` on purpose: sqlc pulls the `ins` CTE (named in
-- EXISTS below) into the outer name scope, so a bare `id`/`completion_attempts` would collide
-- with ins's RETURNING column and trip its "column reference is ambiguous" analyzer.
WITH ins AS (
    INSERT INTO run_completion_attempts (run_id, contract_revision, unmet, head, worktree_fingerprint)
    SELECT r.id, sqlc.narg('contract_revision'), @unmet::jsonb, sqlc.narg('head'), sqlc.narg('worktree_fingerprint')
    FROM runs r
    WHERE r.id = @run_id AND r.worker_id = @worker_id AND r.completion_contract_version IS NOT NULL
      -- PRD #1247 M5: the per-query generation fence, the SAME nil-guarded shape as InsertRunMessage.
      -- A CAPABILITY worker stamps claim_generation; a STALE attempt from an OLD flight (its claim
      -- RELEASED by a held-state switch, or SUPERSEDED by a reclaim) inserts NOTHING here, so with the
      -- UPDATE's EXISTS(ins) gate below it records no attempt (0 rows -> pgx.ErrNoRows -> the caller's
      -- ErrCompletionStaleClaim). A legacy worker (NULL generation) records unconditionally, unchanged.
      -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim
      -- is rejected even for a generation-less (legacy) report; a live claim still honours a NULL gen.
      AND r.claim_released_at IS NULL
      AND (sqlc.narg('claim_generation')::bigint IS NULL
           OR r.claim_generation = sqlc.narg('claim_generation')::bigint)
    RETURNING run_completion_attempts.id
),
pruned AS (
    DELETE FROM run_completion_attempts a
    WHERE a.run_id = @run_id
      AND EXISTS (SELECT 1 FROM ins)
      AND a.id NOT IN (
          SELECT b.id FROM run_completion_attempts b
          WHERE b.run_id = @run_id
          ORDER BY b.created_at DESC, b.id DESC
          LIMIT 49
      )
    RETURNING a.id
)
UPDATE runs SET
    completion_attempts = runs.completion_attempts + 1,
    -- PRD #1226 M3: union-merge the lead's declaration (verbatim SetRunRunning semantics).
    milestones_completed = CASE
        WHEN sqlc.narg('milestones_completed')::jsonb IS NULL THEN runs.milestones_completed
        ELSE COALESCE((SELECT jsonb_agg(DISTINCT e)
                       FROM jsonb_array_elements_text(COALESCE(runs.milestones_completed, '[]'::jsonb) || sqlc.narg('milestones_completed')::jsonb) AS e), '[]'::jsonb)
    END,
    latest_completion_attempt = jsonb_build_object(
        'unmet', @unmet::jsonb,
        'head', sqlc.narg('head')::text,
        'worktree_fingerprint', sqlc.narg('worktree_fingerprint')::text,
        'at', now()
    ),
    updated_at = now()
WHERE runs.id = @run_id AND runs.worker_id = @worker_id AND runs.completion_contract_version IS NOT NULL
  AND EXISTS (SELECT 1 FROM ins)
  -- PRD #1247 M5: mirror the ins CTE's generation fence on the counter/summary UPDATE too, so a
  -- fenced-out attempt updates NOTHING as well as inserting nothing (0 rows -> pgx.ErrNoRows ->
  -- ErrCompletionStaleClaim). Belt-and-suspenders beside EXISTS(ins): the ins fence already stops
  -- the insert, but pinning the same predicate here keeps the whole statement stale-safe.
  -- PRD #1497 M1 (D16): claim_released_at IS NULL is a STANDALONE conjunct, so a released claim is
  -- rejected even for a generation-less (legacy) report; a live claim still honours a NULL generation.
  AND runs.claim_released_at IS NULL
  AND (sqlc.narg('claim_generation')::bigint IS NULL
       OR runs.claim_generation = sqlc.narg('claim_generation')::bigint)
RETURNING runs.completion_attempts;

-- name: UpsertCompletionPermit :one
-- PRD #1226 M2 (D4/D5): idempotently ISSUE a structural completion permit bound to the exact
-- identity (run_id, contract_revision, head). The UNIQUE (run_id, contract_revision, head) is
-- the idempotency key: repeating an UNCHANGED request lands on the conflict path, which does a
-- deliberate no-op UPDATE (branch = its own value) purely so RETURNING yields the PRE-EXISTING
-- row — it NEVER resets consumed_at or issued_at, so a re-request returns the SAME permit
-- rather than re-issuing or un-consuming one. audit is NULL and finding_ids is '{}' under
-- profile=structural (reserved for #1231). issued_by_worker_id is the claim fence.
--
-- DO UPDATE SET branch = EXCLUDED.branch, issued_by_worker_id = EXCLUDED.issued_by_worker_id
-- REBINDS the permit to the REQUESTING worker on conflict. The idempotency contract still
-- holds: a re-request by the SAME worker for the SAME (run_id, contract_revision, head)
-- carries the same branch and worker id, so it changes nothing and RETURNING yields the
-- PRE-EXISTING row. The rebind matters on an A->B REQUEUE: worker A issued a permit for
-- (run, revision, head), the run requeued to worker B in a claimable state, and B re-requests
-- for the same identity. Without the rebind the row keeps A's issued_by_worker_id, so B's
-- completeRunWithPermit (which fences on B's id) can never find its permit -> the run is
-- PERMANENTLY non-terminal. Rebinding issued_by_worker_id to EXCLUDED (B) hands the permit to
-- whoever last requested it.
--
-- It touches ONLY branch and issued_by_worker_id — issued_at, consumed_at, audit and
-- finding_ids are all PRESERVED (omitted columns keep their existing values). Leaving
-- issued_at untouched preserves the M2 idempotency contract (a re-request returns the SAME
-- permit rather than re-issuing one). Not touching consumed_at is provably correct: consumed_at
-- is NULL on every ON CONFLICT path here, because consume+complete are atomic in
-- completeRunWithPermit (so consumed ⇔ status='completed'), and a completed run's re-request is
-- rejected at loadClaimedInterlockedRun's status gate (completion_permit.go) BEFORE it ever
-- reaches this upsert. (EXCLUDED.<col>, not the target table by name, because sqlc's analyzer
-- treats a target-table self-reference in a DO UPDATE SET value as ambiguous — so do NOT
-- introduce a `CASE ... run_completion_permits.issued_at ...` to condition on the existing row.)
--
-- audit and finding_ids are DELIBERATELY OMITTED from the insert: their column defaults (NULL
-- and '{}') are EXACTLY the structural values, and #1231 fills them by adding them here. This
-- also keeps the statement in the plain @param / EXCLUDED shape sqlc's ON-CONFLICT analyzer
-- accepts (a COALESCE(narg::text[], '{}') in the VALUES list trips its column resolver).
INSERT INTO run_completion_permits (
    run_id, contract_revision, branch, head, issued_by_worker_id
) VALUES (
    @run_id, @contract_revision, @branch, @head, sqlc.narg('issued_by_worker_id')
)
ON CONFLICT (run_id, contract_revision, head) DO UPDATE
    SET branch = EXCLUDED.branch, issued_by_worker_id = EXCLUDED.issued_by_worker_id
RETURNING *;

-- name: GetUnconsumedCompletionPermit :one
-- PRD #1226 M2 (D4/D5): fetch the UNCONSUMED permit for the exact identity, FOR UPDATE, inside
-- the completion transaction. issued_by_worker_id fences it to the reporting worker (the claim
-- fence). branch binds the permit to the worker-reported source branch so a permit issued for
-- branch A at head H cannot complete a report for branch B at the same head H. No row
-- (identity/head/revision/branch mismatch, already consumed, or a different worker) returns
-- pgx.ErrNoRows, which completeRunWithPermit reads as "no matching permit -> the gated
-- completion stays non-terminal".
SELECT * FROM run_completion_permits
WHERE run_id = @run_id AND contract_revision = @contract_revision AND head = @head
  AND branch = @branch
  AND issued_by_worker_id = @issued_by_worker_id
  AND consumed_at IS NULL
FOR UPDATE;

-- name: GetConsumedCompletionPermit :one
-- PRD #1226 M2 (D4): the retry-after-response-loss probe. When a completed report arrives for
-- an ALREADY-terminal run, completeRunWithPermit checks whether THIS worker's permit for the
-- identity was already consumed (i.e. we completed it once and the response was lost); if so it
-- returns idempotent success instead of a spurious denial. branch binds the probe to the
-- worker-reported source branch, the same identity component the unconsumed lookup uses. FOR
-- UPDATE under the same run-row lock so the check serializes with a concurrent first completion.
SELECT * FROM run_completion_permits
WHERE run_id = @run_id AND contract_revision = @contract_revision AND head = @head
  AND branch = @branch
  AND issued_by_worker_id = @issued_by_worker_id
  AND consumed_at IS NOT NULL
FOR UPDATE;

-- name: ConsumeCompletionPermit :execrows
-- PRD #1226 M2 (D4): consume the permit inside the completion transaction — stamp consumed_at
-- once. Guarded on the worker fence and consumed_at IS NULL so a double-consume matches 0 rows;
-- the caller requires exactly 1 row affected before writing `completed`.
UPDATE run_completion_permits SET consumed_at = now()
WHERE id = @id AND issued_by_worker_id = @issued_by_worker_id AND consumed_at IS NULL;

-- name: GetRunByIDForUpdate :one
-- PRD #1227 M1: the owner-decision transaction's row lock. DecideCompletion opens a pgx
-- transaction and SELECTs the run FOR UPDATE through this so a partial/accept decision
-- serializes against a racing decision (or a completeRunWithPermit consume) on the same run —
-- the FOR UPDATE row lock is the mutex, exactly as GetRunOwnedByWorkerForUpdate is for the
-- completion transaction. DecideCompletion is OWNER-SCOPED: it re-checks ownership against the
-- LOCKED row (locked.user_id == caller) after the lock, so a foreign caller — including an
-- admin_ro Bearer, which keeps IsAdmin — is hidden as ErrRunNotFound (never a write), preserving
-- the read-only ceiling. An absent run returns pgx.ErrNoRows (-> ErrRunNotFound).
SELECT * FROM runs WHERE id = @id FOR UPDATE;

-- name: BumpContractRevision :one
-- PRD #1227 M1: create contract revision N+1 for an owner decision (partial/accept). It writes
-- the NEW revision and the revised jsonb contract ATOMICALLY, guarded so it fires ONLY for an
-- interlocked, frozen run at the EXPECTED revision — the optimistic fence that makes a decision
-- idempotent/conflict-safe under the FOR UPDATE lock: contract_revision = @expected_revision is
-- the compare-and-set, so a concurrent bump that already moved the revision matches 0 rows
-- (pgx.ErrNoRows -> ErrCompletionRevisionConflict). completion_contract_version IS NOT NULL keeps
-- a legacy run out; completion_contract IS NOT NULL keeps a split-state (never-frozen) run out.
UPDATE runs SET
    contract_revision = @new_revision,
    completion_contract = @completion_contract::jsonb,
    updated_at = now()
WHERE id = @run_id
  AND completion_contract_version IS NOT NULL
  AND completion_contract IS NOT NULL
  AND contract_revision = @expected_revision
RETURNING contract_revision;

-- name: InvalidatePriorCompletionPermits :execrows
-- PRD #1227 M1: after a decision bumps the contract revision to @new_revision, every UNCONSUMED
-- permit issued against an OLDER revision is stale — a completion report could otherwise consume
-- one and complete against a scope the owner just changed. Stamp consumed_at on each so the
-- completion transaction's GetUnconsumedCompletionPermit lookup (consumed_at IS NULL) can never
-- match it; the worker must re-request a permit at the new revision. Returns the count invalidated.
UPDATE run_completion_permits SET consumed_at = now()
WHERE run_id = @run_id AND consumed_at IS NULL AND contract_revision < @new_revision;

-- ════════════════════════════════════════════════════════════════════════════════════════
-- Admin health checks (PRD #1484 M1). Each is an indexed count/min over a table the api
-- already holds; healthsvc composes the server-authored summary and applies the named
-- thresholds. board.drift reuses ListGaveUpColumnMoves and custody.holds reuses
-- ListOwnersOverCustodyLimit, so only these four are new.
-- ════════════════════════════════════════════════════════════════════════════════════════

-- name: ListOwnersWaitingNoCapacity :many
-- health fleet.capacity: the owners who have at least one run parked in
-- health='waiting_worker' AND own ZERO usable workers — online (fresh heartbeat), not
-- draining. Per owner the OLDEST health_since (the wait's start); healthsvc applies the
-- 5-minute danger threshold. The heartbeat-freshness definition (last_heartbeat_at >=
-- @heartbeat_cutoff, cutoff = now - WORKER_HEARTBEAT_STALE) mirrors the "online worker"
-- window the recovery/claim queries use, so this stays truthful even when the controller
-- is silent (a heartbeat-based signal, not status-based). The conjunction with "zero
-- usable workers" is what makes waiting_worker a CAPACITY failure rather than one of its
-- other causes (vault locked, custody limit, all workers busy) — those surface through
-- queue.waiting by age instead.
SELECT r.user_id,
       min(r.health_since)::timestamptz AS oldest_health_since
FROM runs r
WHERE r.health = 'waiting_worker'
  AND r.health_since IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM workers w
      WHERE w.user_id = r.user_id
        AND w.draining_since IS NULL
        AND w.last_heartbeat_at IS NOT NULL
        AND w.last_heartbeat_at >= @heartbeat_cutoff
  )
GROUP BY r.user_id;

-- name: OldestWaitingWorkerRun :one
-- health queue.waiting: the oldest health_since across every run in
-- health='waiting_worker', or NULL when none is waiting. healthsvc applies warn >= 10 min
-- and danger >= 30 min. This is the sole reader of the age; the writer (detectRunHealth)
-- is gated by health_enabled, so when that setting is off the check reports unknown, not
-- ok, rather than reading this NULL as "nothing waiting".
SELECT min(health_since)::timestamptz AS oldest_health_since
FROM runs
WHERE health = 'waiting_worker';

-- name: OldestUndispatchedTaskRun :one
-- health queue.undispatched: the oldest created_at across every task run stuck queued
-- with no dispatch (the #1367 failure class), or NULL when none exists. healthsvc applies
-- danger when older than 10 min. The kind='task' scope is load-bearing: dispatched_at is
-- only ever set on a task run, so unscoped the predicate would match every queued run.
SELECT min(created_at)::timestamptz AS oldest_created_at
FROM runs
WHERE kind = 'task' AND status = 'queued' AND dispatched_at IS NULL;

-- name: CountUsersPausedWithEnabledSchedules :one
-- health schedules.paused: how many users have a pause-all in force AND own at least one
-- ENABLED schedule — the set whose labelled issues look queued forever. A pause is in
-- force when schedules_paused is true OR schedules_paused_until is still ahead of @now.
-- The HAVING filter counts only enabled schedules, so a user who paused but has only
-- disabled schedules does not count. healthsvc warns when the count is >= 1.
SELECT count(*) FROM (
    SELECT u.id
    FROM users u
    JOIN run_schedules rs ON rs.user_id = u.id
    WHERE u.schedules_paused = true
       OR (u.schedules_paused_until IS NOT NULL AND u.schedules_paused_until > @now)
    GROUP BY u.id
    HAVING count(*) FILTER (WHERE rs.enabled) >= 1
) paused_users;
