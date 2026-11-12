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
