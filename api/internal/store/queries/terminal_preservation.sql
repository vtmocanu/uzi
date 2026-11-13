-- Issue #4007: a terminal run's preservation ref refs/uzi-terminal/<run-id>
-- (run_terminal_preservations) and the per-generation contribution records pushed into it
-- (run_terminal_contributions), migrations 00432 and 00433. The guard triggers enforce the state
-- machines; every writer is a CAS on the expected state, so a lost race reads as 0 rows.
-- Lock order, wherever more than one is taken: worker, run, hold, preservation, contribution.

-- name: InsertTerminalPreservation :one
-- Insert-or-load by run_id. A conflict returns the EXISTING row, unchanged (the no-op update locks
-- it for the rest of the transaction), so the caller compares the returned identity against its own
-- and refuses on a mismatch rather than overwriting it.
INSERT INTO run_terminal_preservations (run_id, user_id, repo_id, ref, lifetime_at_bind)
VALUES (@run_id, @user_id, @repo_id, @ref, @lifetime_at_bind)
ON CONFLICT (run_id) DO UPDATE SET updated_at = run_terminal_preservations.updated_at
RETURNING *;

-- name: GetTerminalPreservationByRun :one
SELECT * FROM run_terminal_preservations WHERE run_id = @run_id;

-- name: InsertTerminalContribution :one
-- Insert-or-load by (run_id, generation), with the same conflict contract as
-- InsertTerminalPreservation. A row is born prepared, or recorded for an abandonment disposition.
INSERT INTO run_terminal_contributions
    (preservation_id, run_id, generation, worker_id, user_id, hold_id, authority, tip, prior_tip,
     coverage_digest, manifest, disposition, state)
VALUES (@preservation_id, @run_id, @generation, @worker_id, @user_id, sqlc.narg('hold_id'), @authority, @tip,
        sqlc.narg('prior_tip'), @coverage_digest, @manifest, @disposition,
        CASE WHEN @disposition::text = 'preserved' THEN 'prepared' ELSE 'recorded' END)
ON CONFLICT (run_id, generation) DO UPDATE SET updated_at = run_terminal_contributions.updated_at
RETURNING *;

-- name: GetTerminalContributionByRunGeneration :one
SELECT * FROM run_terminal_contributions WHERE run_id = @run_id AND generation = @generation;

-- name: ListTerminalContributionsForRun :many
-- Every contribution of the run, newest generation first.
SELECT * FROM run_terminal_contributions WHERE run_id = @run_id ORDER BY generation DESC;

-- name: ListTerminalPreservationsDue :many
-- The sweep's due list: rows whose next check has come (on the database clock) in a state that
-- still has work, oldest first. Served by idx_run_terminal_preservations_due.
SELECT * FROM run_terminal_preservations
WHERE next_check_at <= now() AND state IN ('bound', 'publishing', 'live', 'retiring')
ORDER BY next_check_at ASC, id ASC
LIMIT @lim::int;

-- name: CountLiveTerminalPreservationsForRepo :one
-- The repo-removal guard: rows whose live pointer references one of the repo's runs (exactly
-- the rows the runs.repo_id cascade would RESTRICT on). Owner-scoped through the connection.
SELECT count(*) FROM run_terminal_preservations s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE s.repo_id = @repo_id::uuid AND r.repo_id = @repo_id::uuid AND c.user_id = @user_id::uuid;

-- name: ListLiveTerminalRefsForRepo :many
-- What the repo-removal 409 names: the run, its preservation state and the terminal ref.
SELECT s.run_id, s.state, s.ref
FROM run_terminal_preservations s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE s.repo_id = @repo_id::uuid AND r.repo_id = @repo_id::uuid AND c.user_id = @user_id::uuid
ORDER BY s.created_at ASC, s.run_id ASC
LIMIT @lim::int;

-- name: CountLiveTerminalPreservationsForConnection :one
-- The forge-connection-removal guard: live rows on any of the connection's repos.
SELECT count(*) FROM run_terminal_preservations s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE c.id = @connection_id::uuid AND c.user_id = @user_id::uuid;

-- name: ListLiveTerminalRefsForConnection :many
-- What the forge-connection-removal 409 names.
SELECT s.run_id, s.state, s.ref
FROM run_terminal_preservations s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE c.id = @connection_id::uuid AND c.user_id = @user_id::uuid
ORDER BY s.created_at ASC, s.run_id ASC
LIMIT @lim::int;

-- ---------------------------------------------------------------------------------------------
-- Writers (m3). Every state or tip writer is a CAS: the expected state (and the old tip) are in the
-- WHERE, so a lost race and a guard-refused change (the guards RETURN NULL) both read as zero rows.
-- The caller rolls back on zero rows, re-reads under the run lock and logs "illegal transition" when
-- the row is still in the state it expected.
-- ---------------------------------------------------------------------------------------------

-- name: GetTerminalPreservationForUpdate :one
-- The locked read of the run's preservation (lock order: worker, run, hold, preservation,
-- contribution). The tx2 outcome writers take it after the run lock.
SELECT * FROM run_terminal_preservations WHERE run_id = @run_id FOR UPDATE;

-- name: MarkTerminalPreservationPublishing :one
-- bound, not_published, live or publishing -> publishing, setting the live pointer FIRST (before
-- the contribution's prepared -> invoked marker, whose guard needs the preservation publishing).
UPDATE run_terminal_preservations SET
    state = 'publishing', live_run_id = run_id, updated_at = now()
WHERE id = @id AND state = @expected_state::text
  AND tip IS NOT DISTINCT FROM sqlc.narg('expected_tip')::text
RETURNING *;

-- name: MarkTerminalPreservationNotPublished :one
-- bound -> not_published: the abandonment of a run that never had a ref.
UPDATE run_terminal_preservations SET state = 'not_published', updated_at = now()
WHERE id = @id AND state = 'bound' AND tip IS NULL
RETURNING *;

-- name: SettleTerminalPreservationLive :one
-- publishing -> live at a new tip: the tip advances only to the tip of a published contribution of
-- this preservation prepared against the old tip (the guard), and first_published_at is set once.
UPDATE run_terminal_preservations SET
    state = 'live', tip = @tip, first_published_at = COALESCE(first_published_at, now()),
    last_error = NULL, attempts = 0, next_check_at = @next_check_at, updated_at = now()
WHERE id = @id AND state = 'publishing'
  AND tip IS NOT DISTINCT FROM sqlc.narg('expected_tip')::text
RETURNING *;

-- name: SettleTerminalPreservationRefused :one
-- publishing -> live (it had a tip: the ref is as before) or not_published (it had none: no ref was
-- created), after the contribution was recorded refused. The text is already scrubbed and bounded.
UPDATE run_terminal_preservations SET
    state = CASE WHEN tip IS NULL THEN 'not_published' ELSE 'live' END,
    live_run_id = CASE WHEN tip IS NULL THEN NULL ELSE live_run_id END,
    last_error = sqlc.narg('last_error')::text, next_check_at = @next_check_at, updated_at = now()
WHERE id = @id AND state = 'publishing'
  AND tip IS NOT DISTINCT FROM sqlc.narg('expected_tip')::text
RETURNING *;

-- name: ScheduleTerminalPreservationCheck :execrows
-- A single-row scheduling write (no state, no tip, so it needs no run lock): when the sweep next
-- looks at the row, and the scrubbed outcome text of the last attempt. A NULL text keeps the
-- existing last_error (a scheduling write must not erase the last recorded failure).
UPDATE run_terminal_preservations SET
    next_check_at = @next_check_at, last_error = COALESCE(sqlc.narg('last_error')::text, last_error), updated_at = now()
WHERE id = @id;

-- name: MarkTerminalContributionInvoked :one
-- prepared -> invoked: the single winner of a push is the request this returns a row to (and whose
-- transaction then commits).
UPDATE run_terminal_contributions SET state = 'invoked', create_invoked_at = now(), updated_at = now()
WHERE id = @id AND state = 'prepared'
RETURNING *;

-- name: RecordTerminalContributionPublished :one
-- invoked or publish_unknown -> published: the ref was read back at the tip.
UPDATE run_terminal_contributions SET state = 'published', published_at = now(), updated_at = now()
WHERE id = @id AND state = @expected_state::text AND state IN ('invoked', 'publish_unknown')
RETURNING *;

-- name: RecordTerminalContributionRefused :one
-- invoked or publish_unknown -> refused, with the refusal reason.
UPDATE run_terminal_contributions SET state = 'refused', refusal_reason = @refusal_reason, updated_at = now()
WHERE id = @id AND state = @expected_state::text AND state IN ('invoked', 'publish_unknown')
RETURNING *;

-- name: RecordTerminalContributionUnknown :one
-- invoked -> publish_unknown: the outcome could not be proven, reconcile only.
UPDATE run_terminal_contributions SET state = 'publish_unknown', updated_at = now()
WHERE id = @id AND state = 'invoked'
RETURNING *;
