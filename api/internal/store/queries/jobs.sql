-- Repo-less job runs (PRD #1908). The service layer is workersvc/jobs.go. Every read here is
-- caller-scoped IN THE WHERE CLAUSE: the run's owner must be the caller's user, and a
-- product-token caller (a non-NULL product_id) additionally needs job_origins.product_id to be
-- its product. A row that fails either predicate is indistinguishable from no row.

-- name: LockJobCreate :exec
-- Serializes one user's job creates (the per-user cap on non-terminal job runs). Under READ
-- COMMITTED two concurrent creates would each count against their own snapshot and both pass a
-- cap that was true when each looked; the create transaction therefore runs this lock, then
-- CountActiveJobRunsForUser, then CreateJobRun. Same reasoning as LockProductTokenMint.
--
-- Two-int advisory lock: class 1970956898 = 0x757A6A62 ("uzjb"), the value of
-- store.JobCreateLockClass in migrate.go; TestJobCreateLockClassMatchesSQL pins this literal to
-- it. XACT-scoped: released on commit or rollback, so it must run on a transaction-bound
-- Queries (on a bare pool it would release immediately).
SELECT pg_advisory_xact_lock(
    1970956898,
    hashtext(sqlc.arg(user_id)::uuid::text)
);

-- name: CountActiveJobRunsForUser :one
-- The cap count: the user's job runs that have not reached a terminal status. Run under
-- LockJobCreate, in the create transaction.
SELECT count(*)
  FROM runs
 WHERE user_id = $1
   AND kind = 'job'
   AND status NOT IN ('completed', 'failed', 'cancelled');

-- name: GetProductJobPolicy :one
-- The job types a product's tokens may create. Only an enabled, non-deleted product yields a
-- row: any other product reads as no row, which the caller treats as "not allowed".
SELECT allowed_job_types
  FROM products
 WHERE id = $1
   AND enabled
   AND deleted_at IS NULL;

-- name: CreateJobRun :one
-- The dedicated insert for a kind='job' run (PRD #1908): repo-less, issue-less, branchless,
-- queued, no plan gate. auto_approve is baked true (there is no gate to wait at) and
-- required_capabilities is '{}' (the worker gate is the protocol capability, not this array).
-- issue_title carries the caller's title and issue_description the prompt, the same columns
-- prompt/task runs use. harness is the @harness parameter from the atomic create seam.
INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, budget_wall_seconds, trigger_source, harness)
VALUES (@run_id, @user_id, 'job', @job_type, @issue_title, @issue_description, true, '{}', sqlc.narg('budget_wall_seconds'), 'manual', @harness)
RETURNING *;

-- name: CreateJobInput :exec
INSERT INTO job_inputs (run_id, ordinal, name, content_md)
VALUES (@run_id, @ordinal, @name, @content_md);

-- name: CreateJobOrigin :one
INSERT INTO job_origins (run_id, product_id, product_token_id, requested_by_label)
VALUES (@run_id, sqlc.narg('product_id'), sqlc.narg('product_token_id'), sqlc.narg('requested_by_label'))
RETURNING created_at;

-- name: GetJobForCaller :one
SELECT r.id, r.job_type, r.status, r.issue_title AS title, r.failure_reason, r.fail_origin,
       r.budget_wall_seconds, r.created_at, r.started_at, r.finished_at, r.updated_at,
       o.product_id, o.product_token_id, o.requested_by_label
  FROM runs r
  JOIN job_origins o ON o.run_id = r.id
 WHERE r.id = @run_id
   AND r.kind = 'job'
   AND r.user_id = @user_id
   AND (sqlc.narg('product_id')::uuid IS NULL OR o.product_id = sqlc.narg('product_id')::uuid);

-- name: ListJobsForCaller :many
-- Newest first, keyset on (created_at, id) descending. A NULL cursor starts at the newest job.
SELECT r.id, r.job_type, r.status, r.issue_title AS title, r.failure_reason, r.fail_origin,
       r.budget_wall_seconds, r.created_at, r.started_at, r.finished_at, r.updated_at,
       o.product_id, o.product_token_id, o.requested_by_label
  FROM runs r
  JOIN job_origins o ON o.run_id = r.id
 WHERE r.kind = 'job'
   AND r.user_id = @user_id
   AND (sqlc.narg('product_id')::uuid IS NULL OR o.product_id = sqlc.narg('product_id')::uuid)
   AND (sqlc.narg('cursor_created_at')::timestamptz IS NULL
        OR (r.created_at, r.id) < (sqlc.narg('cursor_created_at')::timestamptz, sqlc.narg('cursor_id')::uuid))
 ORDER BY r.created_at DESC, r.id DESC
 LIMIT @lim;

-- name: GetJobResultForCaller :one
SELECT jr.status, jr.report_md, jr.created_at, jr.updated_at
  FROM job_results jr
  JOIN runs r ON r.id = jr.run_id
  JOIN job_origins o ON o.run_id = r.id
 WHERE jr.run_id = @run_id
   AND r.kind = 'job'
   AND r.user_id = @user_id
   AND (sqlc.narg('product_id')::uuid IS NULL OR o.product_id = sqlc.narg('product_id')::uuid);

-- name: ListJobFindingsForCaller :many
SELECT f.ordinal, f.severity, f.message_md, f.url, f.file, f.line
  FROM job_findings f
  JOIN runs r ON r.id = f.run_id
  JOIN job_origins o ON o.run_id = r.id
 WHERE f.run_id = @run_id
   AND r.kind = 'job'
   AND r.user_id = @user_id
   AND (sqlc.narg('product_id')::uuid IS NULL OR o.product_id = sqlc.narg('product_id')::uuid)
 ORDER BY f.ordinal ASC;

-- name: UpsertJobResult :exec
-- The worker's job-result ingest (PRD #1908): the run's one structured result, idempotent on
-- run_id so a retried POST replaces the earlier body. Runs in the ingest transaction, after the
-- run row was locked FOR UPDATE and the kind and claim generation were checked in Go.
INSERT INTO job_results (run_id, status, report_md)
VALUES (@run_id, @status, @report_md)
ON CONFLICT (run_id) DO UPDATE
   SET status     = EXCLUDED.status,
       report_md  = EXCLUDED.report_md,
       updated_at = now();

-- name: DeleteJobFindingsForRun :exec
-- Replace semantics: the ingest transaction drops the run's earlier findings, then re-inserts the
-- new set (InsertJobFinding), so a second POST never leaves the first body's findings behind.
DELETE FROM job_findings WHERE run_id = @run_id;

-- name: InsertJobFinding :exec
INSERT INTO job_findings (run_id, ordinal, severity, message_md, url, file, line)
VALUES (@run_id, @ordinal, @severity, @message_md, sqlc.narg('url'), sqlc.narg('file'), sqlc.narg('line'));

-- name: FailJobRunWithoutResult :execrows
-- The no-result invariant (PRD #1908): a job run the worker reports `completed` while no
-- job_results row exists is failed instead, fail_origin 'job_no_result'. ONE statement, so the
-- result probe and the transition cannot interleave with a result ingest. workersvc runs it just
-- before SetRunCompleted for a kind='job' run: 1 row means the run failed here; 0 rows means a
-- result exists (or the run is not this worker's live, non-terminal job) and SetRunCompleted
-- proceeds exactly as for any other kind. The SET list mirrors SetRunFailed's terminal columns.
UPDATE runs SET
    status             = 'failed',
    status_since       = now(),
    failure_reason     = @failure_reason,
    fail_origin        = 'job_no_result',
    session_id         = COALESCE(sqlc.narg('session_id'), session_id),
    finished_at        = now(),
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at         = now()
WHERE id = @id AND worker_id = @worker_id
  AND kind = 'job'
  AND status NOT IN ('completed', 'failed', 'cancelled')
  AND claim_released_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM job_results jr WHERE jr.run_id = runs.id);

-- name: ListJobMessagesForCaller :many
-- The narrowed, caller-safe projection of a job's run_messages: ONLY the human-readable kinds
-- (text, status, error) and ONLY their payload text. tool_use / tool_result / thinking /
-- plan / user_message payloads are never selected, so raw tool input or output cannot reach
-- a product caller through this read. The Go layer scrubs the text again before returning it.
SELECT m.seq, m.created_at, m.kind, COALESCE(m.payload->>'text', '')::text AS text
  FROM run_messages m
  JOIN runs r ON r.id = m.run_id
  JOIN job_origins o ON o.run_id = r.id
 WHERE m.run_id = @run_id
   AND m.seq > @after_seq
   AND m.kind IN ('text', 'status', 'error')
   AND r.kind = 'job'
   AND r.user_id = @user_id
   AND (sqlc.narg('product_id')::uuid IS NULL OR o.product_id = sqlc.narg('product_id')::uuid)
 ORDER BY m.seq ASC
 LIMIT @lim;

-- name: ListJobInputsForClaim :many
-- The named text inputs of a job, in ordinal order, for claim assembly. NOT caller-scoped: the
-- claim path runs as the worker that just claimed the run (the run id comes from the claimed
-- row, never from a request).
SELECT name, content_md
  FROM job_inputs
 WHERE run_id = @run_id
 ORDER BY ordinal ASC;

-- name: LockUnservableEphemeralJobWorkers :many
-- PRD #1908 D-A2. The ephemeral workers bound to a kind='job' run that can never serve it,
-- locked FOR UPDATE (worker rows, id order) so the fail-and-delete that follows in the same
-- transaction serialises with the worker's own register/heartbeat. Two shapes:
--   (a) the worker registered and is online, but its protocol_capabilities lack 'job_runner_v1'
--       (an old worker image): ClaimRun's job clause will never let it claim, and the gap
--       trigger would provision another one forever. cause = 'no_job_capable_worker'.
--   (b) the worker never registered by the provision deadline (online_since IS NULL and
--       created_at older than @deadline_cutoff): the same shape ReapEphemeralWorkers would
--       delete silently, leaving the still-queued job to be re-provisioned every deadline.
--       cause = 'ephemeral_worker_never_registered'.
-- A worker holding ANY non-terminal run is left alone (the ReapEphemeralWorkers busy guard), and
-- so is one under an OPEN custody hold (the DeleteEphemeralWorkerForRun custody skip): the
-- worker row is the last local source and its delete would hit the hold's RESTRICT FK.
SELECT w.id AS worker_id, r.id AS run_id, r.user_id AS user_id,
       CASE WHEN w.online_since IS NULL THEN 'ephemeral_worker_never_registered'
            ELSE 'no_job_capable_worker' END::text AS cause
  FROM workers w
  JOIN runs r ON r.id = w.ephemeral_run_id AND r.kind = 'job'
 WHERE w.ephemeral
   AND (
        (w.online_since IS NOT NULL
         AND w.status = 'online'
         AND NOT ('job_runner_v1' = ANY(w.protocol_capabilities)))
        OR (w.online_since IS NULL AND w.created_at < @deadline_cutoff)
   )
   AND NOT EXISTS (
       SELECT 1 FROM runs br
        WHERE br.worker_id = w.id
          AND br.status NOT IN ('completed', 'failed', 'cancelled')
   )
   AND NOT EXISTS (
       SELECT 1 FROM recovery_custody_holds h
        WHERE h.live_worker_id = w.id AND h.state = 'open'
   )
 ORDER BY w.id
   FOR UPDATE OF w;

-- name: FailUnservedJobRun :many
-- PRD #1908 D-A2: fail a job whose run-bound ephemeral worker can never serve it. The race
-- guard is load-bearing: `status = 'queued' AND worker_id IS NULL` means a capable worker that
-- claimed the job in the meantime keeps it (this then matches nothing and the caller deletes
-- only the stale ephemeral row). Server-derived failure: fail_origin is one of the two
-- server-only values (@fail_origin is checked by runs_fail_origin_check), stamped in the same
-- statement, and the transition is returned so the caller can fan it out.
UPDATE runs SET status = 'failed', status_since = now(), failure_reason = @failure_reason,
    fail_origin = @fail_origin::text,
    finished_at = now(),
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE id = @run_id
  AND kind = 'job'
  AND status = 'queued'
  AND worker_id IS NULL
RETURNING id, user_id, status;

-- name: FailJobsPastWallDeadline :many
-- PRD #1908 D-E: the server backstop for a job's wall clock. A job never parks (the wall-park
-- passes exclude it), so the runner aborts itself at budget_wall_seconds and reports failed;
-- this fails a claimed or running job whose deadline plus @grace_seconds has passed, for a
-- runner that died or wedged. The deadline is the same shape the wall passes use
-- (budget_wall_seconds, else the global timeout, plus banked pause and any extension/finalize
-- term, all zero for a job in practice) measured from started_at, else claimed_at.
UPDATE runs SET status = 'failed', status_since = now(), failure_reason = @failure_reason,
    fail_origin = 'run_timeout',
    finished_at = now(),
    milestones_in_progress = NULL,
    milestones_agents = NULL,
    pause_requested_at = NULL, pause_mode = NULL, pause_after_count = NULL,
    credential_switch_requested_at = NULL, credential_switch_generation = NULL,
    health = 'ok', health_reason = NULL, health_since = NULL,
    updated_at = now()
WHERE kind = 'job'
  AND status IN ('claimed', 'running')
  AND COALESCE(started_at, claimed_at) < (sqlc.arg('now')::timestamptz
        - make_interval(secs => COALESCE(budget_wall_seconds, sqlc.arg('global_timeout_seconds')::int)
                              + budget_paused_seconds
                              + budget_extension_seconds
                              + budget_finalize_seconds
                              + sqlc.arg('grace_seconds')::int))
RETURNING id, user_id, status;
