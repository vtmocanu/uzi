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
