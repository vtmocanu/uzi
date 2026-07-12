-- Workers -----------------------------------------------------------------

-- name: CreateWorker :one
-- Issue a worker: the plaintext join token is shown once by the caller; only its
-- sha256 (token_hash) is stored. template_declared is the UI-chosen template
-- (PRD #18), NULL when the caller made no choice.
INSERT INTO workers (user_id, name, token_hash, template_declared)
VALUES (@user_id, @name, @token_hash, @template_declared)
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
--     awaiting_approval) — the RUN lane that max_concurrent_runs bounds. Chat runs
--     have their own session budget (WORKER_CHAT_SESSIONS) and ClaimRun excludes
--     them, so counting a live chat here would render a false "3/2 runs" over-cap.
--   * busy is the ANY-kind non-terminal signal (a lone active chat still shows the
--     worker as busy), so it is its own EXISTS over every kind — NOT derived from
--     active_runs, which now omits chat.
-- max_concurrent_runs (the advertised cap, NULL when unadvertised) rides on w.*; it
-- is observability only and never enforced.
SELECT w.*,
       EXISTS (
           SELECT 1 FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval')
       ) AS busy,
       (
           SELECT count(*) FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval')
             AND r.kind <> 'chat'
       ) AS active_runs
FROM workers w
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
UPDATE workers SET
    status              = 'online',
    version             = @version,
    template_reported   = @template_reported,
    max_concurrent_runs = sqlc.narg('max_concurrent_runs'),
    last_heartbeat_at   = now(),
    updated_at          = now()
WHERE id = @id
RETURNING *;

-- name: HeartbeatWorker :one
UPDATE workers SET
    status            = 'online',
    last_heartbeat_at = now(),
    updated_at        = now()
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

-- name: MarkStaleWorkersOffline :execrows
-- Sweeper: workers past the heartbeat-stale window go offline.
UPDATE workers SET status = 'offline', updated_at = now()
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
INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, origin_column, move_pending_since, auto_approve)
VALUES (@user_id, @repo_id::uuid, @issue_iid, @issue_title, @issue_description, sqlc.narg('origin_column'), now(), @auto_approve)
RETURNING *;

-- name: GetRunByIDForUser :one
SELECT * FROM runs WHERE id = @id AND user_id = @user_id;

-- name: GetRunByID :one
-- Admin viewer path: fetch any run regardless of owner. The per-run authz check
-- lives in the service, which only reaches this after confirming the viewer is an
-- admin (owners go through GetRunByIDForUser).
SELECT * FROM runs WHERE id = @id;

-- name: ListRunsForUser :many
-- The user's runs, newest first (Runs index + Agents-status "your runs"), joined
-- to the repo path and the nullable worker name for display. The optional
-- repo_id / issue_iid narrowings (PRD #12 M2) serve the board attention strip
-- (repo scope) and the in-app issue history (repo + issue); when both are NULL
-- this is the unchanged full list. The per-issue narrowing rides the composite
-- index runs (repo_id, issue_iid, created_at DESC).
SELECT sqlc.embed(r), rp.path_with_namespace AS repo_path, w.name AS worker_name
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
LEFT JOIN workers w ON w.id = r.worker_id
WHERE r.user_id = @user_id
  AND r.kind <> 'chat'
  AND (sqlc.narg('repo_id')::uuid IS NULL OR r.repo_id = sqlc.narg('repo_id'))
  AND (sqlc.narg('issue_iid')::bigint IS NULL OR r.issue_iid = sqlc.narg('issue_iid'))
ORDER BY r.created_at DESC
LIMIT 200;

-- name: ListActiveRunsAll :many
-- Admin Agents-status: every non-terminal run across all users, with repo path,
-- worker name, and owner email for the admin overview.
SELECT sqlc.embed(r), rp.path_with_namespace AS repo_path, w.name AS worker_name, u.email AS owner_email
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
LEFT JOIN workers w ON w.id = r.worker_id
JOIN users u ON u.id = r.user_id
WHERE r.status NOT IN ('completed', 'failed', 'cancelled')
  AND r.kind <> 'chat'
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
             AND r.status IN ('claimed', 'running', 'awaiting_approval')
       ) AS busy,
       (
           SELECT count(*) FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval')
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
    ORDER BY COALESCE(r.worker_id = @worker_id, false) DESC, r.created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: GetRunClaimContext :one
-- The repo + connection facts the claim payload needs, alongside the run. The
-- bot PAT (token_ciphertext) is decrypted by the service, never selected in the
-- clear from the DB.
SELECT rp.web_url             AS repo_web_url,
       rp.path_with_namespace AS repo_path,
       rp.default_branch,
       rp.repo_skills_enabled,
       rp.repo_devbox_opt_in,
       c.forge_type,
       c.base_url,
       c.bot_username,
       c.token_ciphertext
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id
WHERE r.id = @run_id;

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
UPDATE runs SET
    status           = 'running',
    started_at       = COALESCE(started_at, now()),
    iteration_count  = GREATEST(iteration_count, @iteration_count),
    session_id       = COALESCE(sqlc.narg('session_id'), session_id),
    repo_agents      = COALESCE(sqlc.narg('repo_agents')::jsonb, repo_agents),
    agent_source     = COALESCE(sqlc.narg('agent_source'), agent_source),
    agent_exclusions = COALESCE(sqlc.narg('agent_exclusions')::jsonb, agent_exclusions),
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
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRunAwaitingApproval :execrows
UPDATE runs SET
    status     = 'awaiting_approval',
    plan_md    = @plan_md,
    session_id = COALESCE(sqlc.narg('session_id'), session_id),
    -- Exit contract (PRD #47 Decision 3): leaving 'running' clears any running-run
    -- flag (stalled/looping/slow). The detector re-evaluates for approval_idle from
    -- this transition's fresh updated_at; health_notified_at is preserved.
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
    session_id         = COALESCE(sqlc.narg('session_id'), session_id),
    -- fix_verdict carries a ci_fix run's outbound 'not_code' verdict on completion
    -- (PRD #6); NULL for every issue run and for a ci_fix that produced a fix (its
    -- verdict is stamped verified/fix_failed later by the pipeline sync).
    fix_verdict        = COALESCE(sqlc.narg('fix_verdict'), fix_verdict),
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
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: RejectRunServerSide :execrows
-- Server-side plan rejection → failed → origin restore → stamp. stop_kind is
-- stamped 'plan_rejected' in the same statement as the status/failure_reason write
-- (PRD #33 Decision 3), so this failed run is recognised as a deliberate stop
-- regardless of the failure_reason text.
UPDATE runs SET status = 'failed', stop_kind = 'plan_rejected', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
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
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- Exit contract (PRD #47 Decision 3): a timed-out run must not keep a stale ⚠.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status = 'running' AND started_at < @cutoff AND kind <> 'chat'
RETURNING id, user_id, status;

-- name: FailRunsOfStaleWorkersOverCap :many
-- A stale worker's non-terminal run that has already used its re-queue budget →
-- failed instead of re-queued. Stamps move_pending_since (reconcile restores the
-- origin column; the sweep itself never touches the forge — worker-loss recovery
-- must not wait on a down forge).
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval')
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
WHERE status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count < @max_requeues
  AND worker_id IN (
      SELECT id FROM workers WHERE last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff
  )
RETURNING id, user_id, status;

-- Register-time orphan recovery (worker-scoped) ------------------------------

-- name: FailWorkerRunsOverCap :execrows
-- On register a worker declares a fresh start, so any run it still holds is
-- orphaned (its execution is gone). Over its re-queue budget → failed. failed →
-- origin restore, applied by the reconcile loop (register does no forge I/O), so
-- it stamps move_pending_since.
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, move_pending_since = now(), finished_at = now(),
    -- Exit contract (PRD #47 Decision 3): a terminal run carries no health flag.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count >= @max_requeues;

-- name: RequeueWorkerRuns :execrows
-- Within budget → re-queued to this same worker (affinity), which then re-claims
-- and resumes from the persisted session (handles docker compose down && up).
UPDATE runs SET status = 'queued', requeue_count = requeue_count + 1,
    -- Exit contract (PRD #47 Decision 3): reset on the way back to 'queued'; the
    -- detector re-evaluates the queued signal from this transition's updated_at.
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count < @max_requeues;

-- Messages -----------------------------------------------------------------

-- name: InsertRunMessage :execrows
-- Idempotent seq-numbered append: a re-delivered batch (worker retry) is a
-- no-op on the duplicate (run_id, seq).
INSERT INTO run_messages (run_id, seq, kind, agent, payload)
VALUES (@run_id, @seq, @kind, @agent, @payload)
ON CONFLICT (run_id, seq) DO NOTHING;

-- name: ListRunMessagesAfter :many
-- Replay for a (re)connecting browser: everything after its last-seen seq, in
-- order. The persisted log is authoritative; the WS layer (M5) is only a live
-- cache on top of this.
SELECT id, run_id, seq, kind, agent, payload, created_at
FROM run_messages
WHERE run_id = @run_id AND seq > @after_seq
ORDER BY seq ASC;

-- Worker chat read surface (PRD #39 M3, Decision 7) --------------------------
-- The chat agent investigates its OWNER'S runs (both kinds) via the worker. These
-- queries are USER_ID-scoped (from the authenticated worker), NEVER a bare run_id
-- lookup — a compromised worker still reads only its own user's runs, and a foreign
-- run id simply returns no row (404). repo_web_url rides along so the handler can
-- build the MR URL; a chat run has no repo, so repo fields are NULL (LEFT JOIN).

-- name: ListRunsForWorkerUser :many
-- Compact list of the worker's user's runs, newest first, bounded by @lim.
SELECT r.id, r.kind, r.status, r.issue_iid, r.issue_title, r.branch, r.mr_iid,
       r.failure_reason, r.created_at, r.updated_at,
       rp.path_with_namespace AS repo_path, rp.web_url AS repo_web_url
FROM runs r
LEFT JOIN repos rp ON rp.id = r.repo_id
WHERE r.user_id = @user_id
ORDER BY r.created_at DESC
LIMIT @lim;

-- name: GetRunForWorkerUser :one
-- One run's detail, scoped to the worker's user (foreign/unknown id -> no row -> 404).
SELECT r.id, r.kind, r.status, r.issue_iid, r.issue_title, r.branch, r.mr_iid, r.mr_state,
       r.failure_reason, r.stop_kind, r.fix_verdict, r.iteration_count, r.plan_md,
       r.created_at, r.updated_at,
       rp.path_with_namespace AS repo_path, rp.web_url AS repo_web_url
FROM runs r
LEFT JOIN repos rp ON rp.id = r.repo_id
WHERE r.id = @id AND r.user_id = @user_id;

-- name: ListRunMessagesForWorkerPage :many
-- A bounded page of a run's messages after a seq (the worker read tool's paging).
-- Authorization (the run is the worker's user's) is checked by the caller before
-- this; here @lim caps the page so a single response can't be unbounded.
SELECT id, run_id, seq, kind, agent, payload, created_at
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
-- no transaction seam, so this single combined statement IS the atomicity. It is used
-- ONLY for the two stop verdicts, so the stamp is unconditional (no IS NOT NULL guard
-- and thus no parameter-type-inference pitfall). The stamp lands while the run is
-- still non-terminal (awaiting_approval/running); the client's terminal-guarded
-- isStoppedRun ignores it until the run actually reaches failed/cancelled.
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
SELECT id, user_id, status, auto_approve,
       started_at, last_activity_at, updated_at,
       health, health_reason, health_since
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
-- the very signal being flagged. health_notified_at is owned by the Slack path, not
-- written here.
UPDATE runs SET
    health        = @health,
    health_reason = sqlc.narg('health_reason'),
    health_since  = sqlc.narg('health_since')
WHERE id = @id AND status = @status;

-- name: CountOnlineWorkersForUser :one
-- How many of a user's workers are online — the queued-run reason resolver uses it
-- to say "no worker is online" vs "waiting for a worker" (Decision 8). Only called
-- for a queued run already past its threshold, so it is off the hot path.
SELECT count(*) FROM workers WHERE user_id = @user_id AND status = 'online';
