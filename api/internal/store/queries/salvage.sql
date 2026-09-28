-- PRD #1867 M2: the run_salvage lifecycle (migration 00264). The sweep enqueues failed,
-- checkpoint-published runs, promotes their branch-scoped checkpoint ref to a run-scoped
-- salvage ref, and later expires it. The live_run_id pointer (ON DELETE RESTRICT) is held
-- while a remote ref may exist and cleared when the row settles; the CHECKs in 00264 enforce
-- that, so every transition below that clears it is guarded on the source state.

-- name: ListSalvageCandidates :many
-- Failed, checkpoint-eligible runs with a recorded checkpoint tip on an enabled forge kind,
-- finished inside the window and not yet recorded. Plan-rejected runs keep today's immediate
-- delete, so they are excluded here. Oldest first; bounded by lim.
SELECT r.id, r.user_id, p.id AS repo_id, r.kind, r.issue_iid,
       r.checkpoint_tip::text AS checkpoint_tip, r.fail_origin, c.forge_type
FROM runs r
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE r.status = 'failed'
  AND r.fail_origin IS DISTINCT FROM 'plan_rejected'
  AND r.checkpoint_tip IS NOT NULL
  AND ((r.kind = 'issue' AND r.issue_iid IS NOT NULL) OR r.kind = 'self_improve')
  AND c.forge_type = ANY(@forges::text[])
  AND r.finished_at >= @since::timestamptz
  AND NOT EXISTS (SELECT 1 FROM run_salvage s WHERE s.run_id = r.id)
ORDER BY r.finished_at ASC, r.id ASC
LIMIT @lim::int;

-- name: InsertRunSalvage :execrows
-- Record a candidate. 'pending' carries live_run_id = run_id (the RESTRICT pointer, which
-- fails with 23503 if the run was deleted first, leaving no row); 'skipped_secret' carries
-- NULL. Only those two states can be inserted: any other state inserts nothing (0 rows),
-- so a row can reach a later state only through the transitions below. Idempotent on
-- run_id: a second insert is a 0-row no-op.
INSERT INTO run_salvage (run_id, user_id, repo_id, forge_type, branch, tip, live_run_id, state)
SELECT @run_id::uuid, @user_id::uuid, @repo_id::uuid, @forge_type::text, @branch::text,
       @tip::text, sqlc.narg(live_run_id)::uuid, @state::text
WHERE @state::text IN ('pending', 'skipped_secret')
ON CONFLICT (run_id) DO NOTHING;

-- name: GetRunSalvage :one
SELECT * FROM run_salvage WHERE run_id = @run_id::uuid;

-- name: ListSalvageDuePending :many
-- The promotion pass: pending rows, least recently touched first (a failed attempt bumps
-- updated_at, so retries rotate). A pending row whose created salvage ref is already due
-- for expiry (ListSalvageDueExpiry at the same now) is excluded, so no row is in both lists.
SELECT * FROM run_salvage
WHERE state = 'pending'
  AND (salvage_created_at IS NULL OR expires_at > @now::timestamptz)
ORDER BY updated_at ASC, run_id ASC
LIMIT @lim::int;

-- name: ListSalvageDueExpiry :many
-- The expiry pass: created salvage refs still live whose expiry has passed.
SELECT * FROM run_salvage
WHERE salvage_created_at IS NOT NULL
  AND state IN ('pending', 'promoted')
  AND expires_at <= @now::timestamptz
ORDER BY updated_at ASC, run_id ASC
LIMIT @lim::int;

-- name: RecordSalvageCreated :execrows
-- The salvage ref is confirmed at the tip. Sets salvage_created_at and expires_at once (a
-- repeat call, e.g. the idempotent same-tip path after a partial success, keeps the first
-- values). Only on a pending row.
UPDATE run_salvage
SET expires_at = CASE WHEN salvage_created_at IS NULL THEN @expires_at::timestamptz ELSE expires_at END,
    salvage_created_at = COALESCE(salvage_created_at, @created_at::timestamptz),
    updated_at = now()
WHERE run_id = @run_id::uuid AND state = 'pending';

-- name: RecordSalvageAttemptFailed :execrows
-- A failed promotion attempt on a pending row: bump attempts and record the bounded error.
-- At the cap the row becomes 'failed' and drops its live pointer, but ONLY when no salvage
-- ref was created; a created salvage ref stays pending (and live) until it expires.
UPDATE run_salvage
SET attempts = attempts + 1,
    last_error = left(@last_error::text, 512),
    state = CASE WHEN attempts + 1 >= @attempt_cap::int AND salvage_created_at IS NULL
                 THEN 'failed' ELSE state END,
    live_run_id = CASE WHEN attempts + 1 >= @attempt_cap::int AND salvage_created_at IS NULL
                       THEN NULL ELSE live_run_id END,
    updated_at = now()
WHERE run_id = @run_id::uuid AND state = 'pending';

-- name: MarkSalvagePromoted :execrows
-- The branch ref is gone (deleted, or CAS proved another owner moved it) after a confirmed
-- salvage create. Only from pending with a created salvage ref; the pointer stays set.
UPDATE run_salvage
SET state = 'promoted',
    promoted_at = @promoted_at::timestamptz,
    last_error = NULL,
    updated_at = now()
WHERE run_id = @run_id::uuid AND state = 'pending' AND salvage_created_at IS NOT NULL;

-- name: SettleSalvage :execrows
-- Settle a live row into a terminal state and clear the live pointer: 'unavailable' or
-- 'refused' (no salvage ref was created), 'disabled' (the forge left UZI_SALVAGE_FORGES;
-- the caller CAS-deleted any created salvage ref and the branch ref first) or 'expired'
-- (the salvage ref was CAS-deleted or confirmed absent). Any other target state matches no
-- row (0 rows). Settling a row with a created salvage ref to 'unavailable' or 'refused'
-- violates run_salvage_created_keeps_live_check (23514): a public ref is never forgotten.
-- So a created row that a later Promote finds unavailable or refused is settled by the
-- caller as 'expired', and only after its salvage ref is confirmed deleted or absent.
UPDATE run_salvage
SET state = @state::text,
    live_run_id = NULL,
    updated_at = now()
WHERE run_id = @run_id::uuid
  AND state IN ('pending', 'promoted')
  AND @state::text IN ('unavailable', 'refused', 'expired', 'disabled');

-- name: RecordSalvageExpireFailed :execrows
-- A failed CAS delete of an expired salvage ref: record the bounded error and retry on a
-- later sweep. The row keeps its state and live pointer.
UPDATE run_salvage
SET attempts = attempts + 1,
    last_error = left(@last_error::text, 512),
    updated_at = now()
WHERE run_id = @run_id::uuid
  AND salvage_created_at IS NOT NULL
  AND state IN ('pending', 'promoted');

-- name: CountLiveSalvageForRepo :one
-- The repo-removal guard: salvage rows whose live pointer references one of the repo's runs
-- (exactly the rows the runs.repo_id cascade would RESTRICT on). Owner-scoped through the
-- repo's connection, so a foreign repo id counts nothing.
SELECT count(*) FROM run_salvage s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE r.repo_id = @repo_id::uuid AND c.user_id = @user_id::uuid;

-- name: ListLiveSalvageRefsForRepo :many
-- The refs the repo-removal 409 names, per live row: salvage_ref is the run-scoped salvage
-- ref once created ('' before), retained_ref the branch-scoped checkpoint ref while the row
-- is still pending ('' once promoted), since a pending row's branch ref may still exist even
-- after its salvage ref was created. Owner-scoped like the count. Bounded by lim rows.
SELECT s.run_id,
       (CASE WHEN s.salvage_created_at IS NOT NULL
             THEN 'refs/uzi-salvage/' || s.run_id::text ELSE '' END)::text AS salvage_ref,
       (CASE WHEN s.state = 'pending'
             THEN 'refs/uzi-checkpoints/' || s.branch ELSE '' END)::text AS retained_ref
FROM run_salvage s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE r.repo_id = @repo_id::uuid AND c.user_id = @user_id::uuid
ORDER BY s.created_at ASC, s.run_id ASC
LIMIT @lim::int;

-- name: CountLiveSalvageForConnection :one
-- The forge-connection-removal guard: live salvage rows on any of the connection's repos.
-- Owner-scoped so a foreign connection id counts nothing (the delete then 404s).
SELECT count(*) FROM run_salvage s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE c.id = @connection_id::uuid AND c.user_id = @user_id::uuid;

-- name: ListLiveSalvageRefsForConnection :many
-- The refs the forge-connection-removal 409 names (see ListLiveSalvageRefsForRepo).
SELECT s.run_id,
       (CASE WHEN s.salvage_created_at IS NOT NULL
             THEN 'refs/uzi-salvage/' || s.run_id::text ELSE '' END)::text AS salvage_ref,
       (CASE WHEN s.state = 'pending'
             THEN 'refs/uzi-checkpoints/' || s.branch ELSE '' END)::text AS retained_ref
FROM run_salvage s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE c.id = @connection_id::uuid AND c.user_id = @user_id::uuid
ORDER BY s.created_at ASC, s.run_id ASC
LIMIT @lim::int;
