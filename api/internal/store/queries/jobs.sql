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
-- job_protocol is the PRD #1909 M1 rollout stamp (capability.JobProtocolFiles): a stamped job is
-- claimable only by a worker that also advertises 'job_files_v1'.
INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, budget_wall_seconds, trigger_source, harness, job_protocol)
VALUES (@run_id, @user_id, 'job', @job_type, @issue_title, @issue_description, true, '{}', sqlc.narg('budget_wall_seconds'), 'manual', @harness, sqlc.narg('job_protocol')::smallint)
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

-- name: GetJobOriginForRun :one
-- The origin of a job run for the run-detail read (PRD #1908 D-D): the untrusted requested_by_label
-- and the creating product's NAME (NULL for a uzc_-created job). NOT caller-scoped: the run-read
-- authorization (owner or admin) was decided by the handler before this runs.
SELECT o.requested_by_label, p.name AS product_name
  FROM job_origins o
  LEFT JOIN products p ON p.id = o.product_id
 WHERE o.run_id = @run_id;

-- name: ListJobInputSizesForRun :many
-- The run-detail view of a job's inputs: name and byte size only (the content can be 1 MiB and is
-- not part of the run detail). Bounded by @lim; ordinal order. Same authorization note as
-- GetJobOriginForRun.
SELECT name, octet_length(content_md)::int AS size_bytes
  FROM job_inputs
 WHERE run_id = @run_id
 ORDER BY ordinal ASC
 LIMIT @lim;

-- name: GetJobResultForRun :one
SELECT status, report_md
  FROM job_results
 WHERE run_id = @run_id;

-- name: ListJobFindingsForRun :many
SELECT ordinal, severity, message_md, url, file, line
  FROM job_findings
 WHERE run_id = @run_id
 ORDER BY ordinal ASC;

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

-- name: ClearJobResultForRun :exec
-- Drops a job run's result and findings in ONE statement. Claim assembly runs it for every job
-- claim: job_results is keyed by run_id and a requeued job (worker death, never-started sweep) is
-- claimed again, so a result an EARLIER flight posted would otherwise satisfy the no-result
-- invariant for a later flight that posts nothing. The claim generation was bumped by ClaimRun
-- before assembly, so an older flight can no longer write a result after this clear (the ingest
-- fences on the generation under the run row lock).
WITH f AS (DELETE FROM job_findings jf WHERE jf.run_id = @run_id::uuid)
DELETE FROM job_results jr WHERE jr.run_id = @run_id::uuid;

-- name: InsertJobFinding :exec
INSERT INTO job_findings (run_id, ordinal, severity, message_md, url, file, line)
VALUES (@run_id, @ordinal, @severity, @message_md, sqlc.narg('url'), sqlc.narg('file'), sqlc.narg('line'));

-- name: FailJobRunWithoutResult :execrows
-- The no-result invariant (PRD #1908): a job run the worker reports `completed` while no
-- job_results row exists is failed instead, fail_origin 'job_no_result'. workersvc runs it just
-- before SetRunCompleted for a kind='job' run: 1 row means the run failed here; 0 rows means a
-- result exists (or the run is not this worker's live job) and SetRunCompleted proceeds exactly as
-- for any other kind. The SET list mirrors SetRunFailed's terminal columns.
--
-- The probe and the transition are ONE statement, but that alone does not serialise them against
-- a result ingest: under READ COMMITTED a result committing between this statement's snapshot and
-- its write would be missed. The race-free property comes from the caller: setState requires
-- claim_generation on every job state report (ErrMissingClaimGeneration otherwise), so this runs
-- on the FOR UPDATE fenced path, holding the run row lock that SubmitJobResult also takes before
-- it writes the result. Do not call it on an unfenced path.
--
-- The live-status guard matches SetRunCompleted's job arm: only a claimed or running job (never
-- queued, paused or terminal) with an unreleased claim.
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
  AND status IN ('claimed', 'running')
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
-- transaction serialises with the worker's own register/heartbeat. Two shapes, told apart by
-- last_heartbeat_at, the ONLY registration signal the stale sweep does not clear (RegisterWorker
-- stamps it and MarkStaleWorkersOffline leaves it; the uptime anchor column is display-only, PRD
-- #251, is nulled by that sweep, and is deliberately not read here):
--   (a) the worker REGISTERED AT LEAST ONCE (last_heartbeat_at IS NOT NULL), online or since gone
--       stale, and does not satisfy ClaimRun's job clause: a docker worker, or one whose
--       protocol_capabilities lack 'job_runner_v1' (an old worker image). ClaimRun will never
--       let it claim, and the gap trigger would provision another one forever. The lacking
--       capability is job_runner_v1 or, for a job stamped with runs.job_protocol (created with
--       files support, PRD #1909 M1), job_files_v1.
--       cause = 'no_job_capable_worker'.
--   (b) the worker NEVER registered by the provision deadline (last_heartbeat_at IS NULL and
--       created_at older than @deadline_cutoff): the shape ReapEphemeralWorkers would delete
--       silently, leaving the still-queued job to be re-provisioned every deadline.
--       cause = 'ephemeral_worker_never_registered'.
-- A worker holding ANY non-terminal run is left alone (the ReapEphemeralWorkers busy guard), and
-- so is one under an OPEN custody hold (the DeleteEphemeralWorkerForRun custody skip): the
-- worker row is the last local source and its delete would hit the hold's RESTRICT FK.
SELECT w.id AS worker_id, r.id AS run_id, r.user_id AS user_id,
       CASE WHEN w.last_heartbeat_at IS NULL THEN 'ephemeral_worker_never_registered'
            ELSE 'no_job_capable_worker' END::text AS cause
  FROM workers w
  JOIN runs r ON r.id = w.ephemeral_run_id AND r.kind = 'job'
 WHERE w.ephemeral
   AND (
        (w.last_heartbeat_at IS NOT NULL
         AND NOT (NOT COALESCE(w.docker_enabled, false)
                  AND 'job_runner_v1' = ANY(w.protocol_capabilities)
                  -- PRD #1909 M1: a stamped job (runs.job_protocol) also needs 'job_files_v1'.
                  AND (r.job_protocol IS NULL OR 'job_files_v1' = ANY(w.protocol_capabilities))))
        OR (w.last_heartbeat_at IS NULL AND w.created_at < @deadline_cutoff)
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

-- name: ListRevokedProductJobs :many
-- PRD #1908 D14 product-revoke sweep. The non-terminal kind='job' runs whose authorization
-- has been withdrawn, oldest first, at most @batch per pass:
--   * the creating product token was EXPLICITLY revoked (single revoke, "Revoke all" or admin
--     revoke, all of which set product_tokens.revoked; a future revoke path such as the PRD
--     #1910 OAuth grant revoke must set product_tokens.revoked too, or this sweep misses it);
--   * the origin's product is disabled or soft-deleted (products_deleted_is_disabled makes a
--     deleted product always disabled, so the deleted_at clause is belt-and-braces);
--   * the owner is deactivated (any job, including one created with a uzc_ token).
-- Token EXPIRY is deliberately absent: an expired, unrevoked token never cancels a job that was
-- authorized while it was valid. A uzc_-created job has NULL product_id and NULL
-- product_token_id, so the LEFT JOINs give it no revoked/disabled signal and only the owner
-- clause can select it. A NON-queued job that already has a cancel for its CURRENT claim is
-- skipped: an unconsumed one is in flight for a poller to consume, and one consumed under the
-- current claim_generation has been handed to the runner, so a re-run of the pass is a no-op
-- rather than a second input, and a job still winding down after consuming its cancel cannot
-- hold a batch slot pass after pass (with more than @batch such jobs, the oldest would refill
-- every batch and starve newer revoked jobs of their cancel). A cancel consumed under an EARLIER
-- claim does not count: a requeued and reclaimed job gets a fresh one. A QUEUED job is never skipped: it has no poller, so a pending cancel left by the
-- stale-worker requeue (RequeueRunsOfStaleWorkers) would otherwise strand it queued forever; the
-- pass takes the server-side queued cancel for it.
SELECT r.id, r.user_id
  FROM runs r
  JOIN users u ON u.id = r.user_id
  JOIN job_origins o ON o.run_id = r.id
  LEFT JOIN product_tokens t ON t.id = o.product_token_id
  LEFT JOIN products p ON p.id = o.product_id
 WHERE r.kind = 'job'
   AND r.status NOT IN ('completed', 'failed', 'cancelled')
   AND (COALESCE(t.revoked, false)
        OR COALESCE(NOT p.enabled, false)
        OR p.deleted_at IS NOT NULL
        OR NOT u.is_active)
   AND (r.status = 'queued'
        OR NOT EXISTS (SELECT 1 FROM run_user_inputs i
                        WHERE i.run_id = r.id AND i.kind = 'cancel'
                          AND (i.consumed_at IS NULL OR i.consumed_claim_generation = r.claim_generation)))
 ORDER BY r.created_at, r.id
 LIMIT @batch::int;
