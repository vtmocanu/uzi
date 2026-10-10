-- Issue #2545: the run_held_publications lifecycle and the hold-side queries of a failed run's
-- held-work publication (migration 00321). The held_publication_guard trigger enforces the state
-- machine; every transition below is a CAS on the expected state, so a lost race reads as 0 rows.
-- Lock order, wherever more than one is taken: worker, run, hold, publication.

-- name: UpsertHeldPublication :one
-- Insert-or-load by (run_id, generation). A conflict returns the EXISTING row, unchanged (the
-- no-op update locks it for the rest of the transaction), so the caller compares the returned
-- identity against its own and refuses on a mismatch rather than overwriting it.
INSERT INTO run_held_publications
    (run_id, generation, hold_id, user_id, repo_id, worker_id, live_run_id, ref, tip, coverage_digest, state)
VALUES (@run_id, @generation, @hold_id, @user_id, @repo_id, @worker_id, @run_id, @ref, @tip, @coverage_digest, 'prepared')
ON CONFLICT (run_id, generation) DO UPDATE SET updated_at = run_held_publications.updated_at
RETURNING *;

-- name: GetHeldPublication :one
SELECT * FROM run_held_publications WHERE id = @id;

-- name: GetHeldPublicationByRunGeneration :one
SELECT * FROM run_held_publications WHERE run_id = @run_id AND generation = @generation;

-- name: LockHeldPublicationByRunGeneration :one
-- The publication lock of the worker, run, hold, publication order.
SELECT * FROM run_held_publications WHERE run_id = @run_id AND generation = @generation FOR UPDATE;

-- name: MarkHeldCreateInvoked :one
-- The monotonic "may have invoked" marker. Exactly one caller wins (the row it returns); a
-- loser reconciles only. The caller commits this BEFORE any network mutation.
UPDATE run_held_publications
SET create_invoked_at = now(), state = 'invoked', attempts = attempts + 1, updated_at = now()
WHERE id = @id AND state = 'prepared' AND create_invoked_at IS NULL
RETURNING *;

-- name: RecordHeldCreateOutcome :one
-- Record a create outcome (or a reconcile result) from invoked or create_unknown. created sets
-- ref_created_at and clears the error; refused clears the live pointer (no ref was created, so
-- the row no longer pins its run) and records refusal_reason; create_unknown keeps the pointer
-- and schedules the next reconcile. Any other target matches no row.
UPDATE run_held_publications
SET state = @state::text,
    live_run_id = CASE WHEN @state::text = 'refused' THEN NULL ELSE live_run_id END,
    ref_created_at = CASE WHEN @state::text = 'created' THEN COALESCE(ref_created_at, now()) ELSE ref_created_at END,
    refusal_reason = CASE WHEN @state::text = 'refused' THEN sqlc.narg(refusal_reason)::text ELSE refusal_reason END,
    last_error = CASE WHEN @state::text = 'created' THEN NULL ELSE left(sqlc.narg(last_error)::text, 512) END,
    next_attempt_at = CASE WHEN @state::text = 'create_unknown' THEN sqlc.narg(next_attempt_at)::timestamptz ELSE NULL END,
    attempts = attempts + 1,
    updated_at = now()
WHERE id = @id AND state IN ('invoked', 'create_unknown')
  AND @state::text IN ('created', 'refused', 'create_unknown')
RETURNING *;

-- name: GetHeldPublicationHold :one
-- The guarded hold of (run, generation) for the exact owner and original worker, unlocked: the
-- replay read and the pre-body gate. The locked read is GetFinalInventoryHold.
SELECT * FROM recovery_custody_holds
WHERE run_id = @run_id AND user_id = @user_id AND original_worker_id = @worker_id::uuid
  AND generation = @generation AND inventory_guarded;

-- name: CountOpenHoldsForRun :one
-- The cleanup exclusion: no ref is deleted while ANY hold of the run is open, whatever its
-- generation.
SELECT count(*) FROM recovery_custody_holds WHERE run_id = @run_id AND state = 'open';

-- name: ReleaseHeldPublicationHold :execrows
-- Step B's first write (the hold is updated BEFORE the publication: the hold guard needs the
-- publication in created and the publication guard needs the hold released). The hold guard
-- makes the final decision (failed run, generation pin, excluded origins, publication binding)
-- and refuses with 0 rows.
UPDATE recovery_custody_holds h SET
    state = 'released', live_worker_id = NULL, live_run_id = NULL,
    final_disposition = 'held_publication', release_evidence = 'held_publication',
    final_publication_id = @publication_id::uuid, final_source_sha = @tip::text,
    final_coverage_digest = @coverage_digest::text, released_at = now(), updated_at = now()
WHERE h.id = @id AND h.run_id = @run_id AND h.user_id = @user_id
  AND h.generation = @generation AND h.original_worker_id = @worker_id::uuid
  AND h.live_worker_id = @worker_id::uuid AND h.inventory_guarded AND h.state = 'open';

-- name: AcknowledgeHeldPublication :one
-- Step B's second write: created -> acknowledged and the retention clock. An earlier
-- expires_at is kept. Matches no row unless the hold was released against this publication
-- (held_publication_guard).
UPDATE run_held_publications
SET state = 'acknowledged', acknowledged_at = now(),
    expires_at = LEAST(COALESCE(expires_at, @expires_at::timestamptz), @expires_at::timestamptz),
    next_attempt_at = NULL, updated_at = now()
WHERE id = @id AND state = 'created'
RETURNING *;

-- name: ListHeldPublicationsDueExpiry :many
-- The expiry lane: the only lane that deletes an ACKNOWLEDGED publication. Owner expiry sets
-- expires_at to the expiry time, so both reasons are one predicate.
SELECT * FROM run_held_publications
WHERE state = 'acknowledged' AND expires_at <= @now::timestamptz
ORDER BY expires_at ASC, id ASC
LIMIT @lim::int;

-- name: ListHeldPublicationsDueOrphan :many
-- The orphan lane: a created ref that was never acknowledged and whose run has no open hold.
-- It never selects an acknowledged row.
SELECT p.* FROM run_held_publications p
WHERE p.state = 'created'
  AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id = p.run_id AND h.state = 'open')
ORDER BY p.updated_at ASC, p.id ASC
LIMIT @lim::int;

-- name: ListHeldPublicationsDueReconcile :many
-- The reconcile lane: unresolved creates and deletes. It lists and records outcomes only, so it
-- is allowed while a hold is open. An invoked row with no schedule is due once grace_seconds
-- passed since the marker, so a still-pending first create is never raced.
SELECT * FROM run_held_publications
WHERE state IN ('invoked', 'create_unknown', 'delete_unknown')
  AND COALESCE(next_attempt_at, create_invoked_at + make_interval(secs => @grace_seconds::float8), created_at)
      <= @now::timestamptz
ORDER BY COALESCE(next_attempt_at, create_invoked_at, created_at) ASC, id ASC
LIMIT @lim::int;

-- name: RecordHeldDeleteOutcome :execrows
-- Record a delete attempt or a reconcile result. deleted and abandoned clear the live pointer
-- (the ref is gone, or is a foreign tip that is never touched); delete_unknown keeps it and
-- schedules the next attempt. The expected source states are the caller's CAS.
UPDATE run_held_publications
SET state = @state::text,
    live_run_id = CASE WHEN @state::text IN ('deleted', 'abandoned') THEN NULL ELSE live_run_id END,
    deleted_at = CASE WHEN @state::text = 'deleted' THEN COALESCE(deleted_at, now()) ELSE deleted_at END,
    refusal_reason = CASE WHEN @state::text = 'abandoned' THEN 'foreign_tip' ELSE refusal_reason END,
    last_error = CASE WHEN @state::text = 'deleted' THEN NULL ELSE left(sqlc.narg(last_error)::text, 512) END,
    next_attempt_at = CASE WHEN @state::text = 'delete_unknown' THEN sqlc.narg(next_attempt_at)::timestamptz ELSE NULL END,
    attempts = attempts + 1,
    updated_at = now()
WHERE id = @id AND state = ANY(@from_states::text[])
  AND @state::text IN ('deleted', 'abandoned', 'delete_unknown');

-- name: OwnerExpireHeldPublications :many
-- The owner's early expiry: every ACKNOWLEDGED publication of the caller's run becomes due now.
-- Filtered on user_id (the owner), so a foreign run matches nothing, and on state, so a
-- publication that is not acknowledged (or is already deleting) is never touched.
UPDATE run_held_publications
SET owner_expired_at = COALESCE(owner_expired_at, now()),
    expires_at = LEAST(COALESCE(expires_at, now()), now()),
    updated_at = now()
WHERE run_id = @run_id AND user_id = @user_id AND state = 'acknowledged'
RETURNING *;

-- name: ListHeldPublicationsForRun :many
-- The owner surface: every held publication of the caller's run, newest generation first.
SELECT * FROM run_held_publications
WHERE run_id = @run_id AND user_id = @user_id
ORDER BY generation DESC;

-- name: CountLiveHeldPublicationsForRepo :one
-- The repo-removal guard: rows whose live pointer references one of the repo's runs (exactly
-- the rows the runs.repo_id cascade would RESTRICT on). Owner-scoped through the connection.
SELECT count(*) FROM run_held_publications s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE r.repo_id = @repo_id::uuid AND c.user_id = @user_id::uuid;

-- name: ListLiveHeldPublicationRefsForRepo :many
-- What the repo-removal 409 names: the run, its publication state and the held ref.
SELECT s.run_id, s.state, s.ref
FROM run_held_publications s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE r.repo_id = @repo_id::uuid AND c.user_id = @user_id::uuid
ORDER BY s.created_at ASC, s.run_id ASC
LIMIT @lim::int;

-- name: CountLiveHeldPublicationsForConnection :one
-- The forge-connection-removal guard: live rows on any of the connection's repos.
SELECT count(*) FROM run_held_publications s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE c.id = @connection_id::uuid AND c.user_id = @user_id::uuid;

-- name: ListLiveHeldPublicationRefsForConnection :many
-- What the forge-connection-removal 409 names.
SELECT s.run_id, s.state, s.ref
FROM run_held_publications s
JOIN runs r ON r.id = s.live_run_id
JOIN repos p ON p.id = r.repo_id
JOIN forge_connections c ON c.id = p.connection_id
WHERE c.id = @connection_id::uuid AND c.user_id = @user_id::uuid
ORDER BY s.created_at ASC, s.run_id ASC
LIMIT @lim::int;
