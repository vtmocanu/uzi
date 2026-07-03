-- Workers -----------------------------------------------------------------

-- name: CreateWorker :one
-- Issue a worker: the plaintext join token is shown once by the caller; only its
-- sha256 (token_hash) is stored.
INSERT INTO workers (user_id, name, token_hash)
VALUES (@user_id, @name, @token_hash)
RETURNING *;

-- name: GetWorkerByTokenHash :one
-- Worker auth: Bearer join token → sha256 → this lookup.
SELECT * FROM workers WHERE token_hash = @token_hash;

-- name: GetWorkerByID :one
SELECT * FROM workers WHERE id = @id;

-- name: GetWorkerByIDForUser :one
SELECT * FROM workers WHERE id = @id AND user_id = @user_id;

-- name: ListWorkersByUser :many
-- Worker list for the owning user. "busy" is derived here (never stored): a
-- worker is busy when it holds a non-terminal run.
SELECT w.*,
       EXISTS (
           SELECT 1 FROM runs r
           WHERE r.worker_id = w.id
             AND r.status IN ('claimed', 'running', 'awaiting_approval')
       ) AS busy
FROM workers w
WHERE w.user_id = @user_id
ORDER BY w.created_at ASC;

-- name: RegisterWorker :one
-- Worker announces version and comes online; heartbeat is stamped now.
UPDATE workers SET
    status            = 'online',
    version           = @version,
    last_heartbeat_at = now(),
    updated_at        = now()
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

-- name: MarkStaleWorkersOffline :execrows
-- Sweeper: workers past the heartbeat-stale window go offline.
UPDATE workers SET status = 'offline', updated_at = now()
WHERE status = 'online'
  AND (last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff);

-- Runs ---------------------------------------------------------------------

-- name: CreateRun :one
-- Queue a run from a card. The one-non-terminal-run-per-issue partial unique
-- index rejects a second active run for the same issue (23505 → 409).
INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description)
VALUES (@user_id, @repo_id, @issue_iid, @issue_title, @issue_description)
RETURNING *;

-- name: GetRunByIDForUser :one
SELECT * FROM runs WHERE id = @id AND user_id = @user_id;

-- name: GetRunOwnedByWorker :one
-- Worker-endpoint authz: a worker may only touch a run it currently holds.
SELECT * FROM runs WHERE id = @id AND worker_id = @worker_id;

-- name: ClaimRun :one
-- Atomic claim of the oldest claimable queued run for the worker's user. A
-- re-queued run prefers its prior worker (own runs sort first, and are the only
-- claimant until the affinity grace lapses — @affinity_cutoff is now minus
-- WORKER_AFFINITY_GRACE); after that any of the user's workers may claim it.
-- FOR UPDATE SKIP LOCKED lets concurrent workers claim disjoint runs without
-- blocking (multica's queue semantics).
UPDATE runs SET
    status     = 'claimed',
    worker_id  = @worker_id,
    claimed_at = now(),
    updated_at = now()
WHERE id = (
    SELECT r.id FROM runs r
    WHERE r.user_id = @user_id
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
       c.forge_type,
       c.base_url,
       c.bot_username,
       c.token_ciphertext
FROM runs r
JOIN repos rp ON rp.id = r.repo_id
JOIN forge_connections c ON c.id = rp.connection_id
WHERE r.id = @run_id;

-- name: SetRunRunning :execrows
-- claimed/awaiting_approval → running. started_at is stamped once; iteration_count
-- only advances (GREATEST) so a resume never regresses the loop counter. A
-- terminal run (e.g. cancelled) is left untouched → 0 rows → "already terminal".
UPDATE runs SET
    status          = 'running',
    started_at      = COALESCE(started_at, now()),
    iteration_count = GREATEST(iteration_count, @iteration_count),
    updated_at      = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRunAwaitingApproval :execrows
UPDATE runs SET
    status     = 'awaiting_approval',
    plan_md    = @plan_md,
    updated_at = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRunCompleted :execrows
UPDATE runs SET
    status      = 'completed',
    branch      = @branch,
    mr_iid      = @mr_iid,
    finished_at = now(),
    updated_at  = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: SetRunFailed :execrows
UPDATE runs SET
    status         = 'failed',
    failure_reason = @failure_reason,
    finished_at    = now(),
    updated_at     = now()
WHERE id = @id AND worker_id = @worker_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: MarkRunFailedByID :execrows
-- Service-internal fail (e.g. a claim whose secrets are missing/undecryptable):
-- the run was just claimed by this worker but cannot run.
UPDATE runs SET
    status         = 'failed',
    failure_reason = @failure_reason,
    finished_at    = now(),
    updated_at     = now()
WHERE id = @id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: CancelRunServerSide :execrows
-- Server-side cancel for a run with no live poller (still queued, or its worker
-- went stale): the user input is not stranded waiting for a GET /inputs poll
-- that will never come.
UPDATE runs SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: RejectRunServerSide :execrows
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, finished_at = now(), updated_at = now()
WHERE id = @id AND user_id = @user_id
  AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: UpdateRunLastSeq :execrows
-- Advance the message high-water mark (never regresses).
UPDATE runs SET last_seq = GREATEST(last_seq, @seq)
WHERE id = @id;

-- Sweeper: run-level timeouts and worker-loss recovery -----------------------

-- name: SweepClaimedNeverStarted :execrows
-- claimed but never started past the grace window → back to queued (worker_id
-- kept for affinity so the same disk reclaims it).
UPDATE runs SET status = 'queued', updated_at = now()
WHERE status = 'claimed' AND claimed_at < @cutoff;

-- name: SweepRunningTimeout :execrows
-- running past RUN_TIMEOUT → failed (a hung agent is failed without a human).
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, finished_at = now(), updated_at = now()
WHERE status = 'running' AND started_at < @cutoff;

-- name: FailRunsOfStaleWorkersOverCap :execrows
-- A stale worker's non-terminal run that has already used its re-queue budget →
-- failed instead of re-queued.
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, finished_at = now(), updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count >= @max_requeues
  AND worker_id IN (
      SELECT id FROM workers WHERE last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff
  );

-- name: RequeueRunsOfStaleWorkers :execrows
-- A stale worker's non-terminal run within its re-queue budget → back to queued
-- (worker_id kept for affinity, requeue_count incremented).
UPDATE runs SET status = 'queued', requeue_count = requeue_count + 1, updated_at = now()
WHERE status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count < @max_requeues
  AND worker_id IN (
      SELECT id FROM workers WHERE last_heartbeat_at IS NULL OR last_heartbeat_at < @cutoff
  );

-- Register-time orphan recovery (worker-scoped) ------------------------------

-- name: FailWorkerRunsOverCap :execrows
-- On register a worker declares a fresh start, so any run it still holds is
-- orphaned (its execution is gone). Over its re-queue budget → failed.
UPDATE runs SET status = 'failed', failure_reason = @failure_reason, finished_at = now(), updated_at = now()
WHERE worker_id = @worker_id
  AND status IN ('claimed', 'running', 'awaiting_approval')
  AND requeue_count >= @max_requeues;

-- name: RequeueWorkerRuns :execrows
-- Within budget → re-queued to this same worker (affinity), which then re-claims
-- and resumes from the persisted session (handles docker compose down && up).
UPDATE runs SET status = 'queued', requeue_count = requeue_count + 1, updated_at = now()
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

-- User inputs (steering) ---------------------------------------------------

-- name: CreateRunInput :one
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
