-- PRD #1296 M1 (D2/D4/D5): the durable-recovery store contract. These queries are the
-- COMPLETE store-query layer M2 (upload/owner API), M3 (worker capture/retry) and M5
-- (owner UX) consume, frozen here so those parallel milestones never edit this file.
-- Custody holds are opened atomically with the claim (runtime.sql ClaimRun); everything
-- below operates on the already-open hold and its captures.

-- name: ReserveCapture :one
-- D2: reserve an immutable capture in state 'preparing', bound to the run's OPEN custody
-- hold taken by THIS worker. The hold is resolved in-SQL (fail-closed: no open hold owned
-- by @original_worker_id -> zero rows -> pgx.ErrNoRows -> not authorized), which enforces
-- the D2 invariant "a capture requires the original worker's open hold" at the store, not
-- only at the handler. Retrying the SAME capture identity (hold_id, idempotency_key) is
-- idempotent: ON CONFLICT bumps updated_at and returns the existing record, so a lost ACK
-- re-reserves the same capture_id. A changed source_sha under the same key does NOT
-- overwrite the frozen source (the conflict keeps the original row's columns).
INSERT INTO recovery_captures
    (hold_id, run_id, user_id, original_worker_id, original_worker_identity,
     source_sha, attempted_head_sha, idempotency_key, state)
SELECT h.id, @run_id, @user_id, @original_worker_id, @original_worker_identity::text,
       @source_sha, sqlc.narg('attempted_head_sha'), @idempotency_key, 'preparing'
FROM recovery_custody_holds h
WHERE h.run_id = @run_id
  AND h.user_id = @user_id
  AND h.original_worker_id = @original_worker_id
  AND h.state = 'open'
ORDER BY h.created_at DESC
LIMIT 1
ON CONFLICT (hold_id, idempotency_key) DO UPDATE SET updated_at = now()
RETURNING *;

-- name: BindCaptureManifest :one
-- D2/D4: compare-and-set the byte manifest ONCE. The first bind (manifest_bound=false)
-- always wins; a retry with the SAME byte_size+checksum is idempotent (the second
-- disjunct matches and re-stamps updated_at); a DIFFERENT manifest under the same
-- capture_id matches neither disjunct and returns zero rows (a conflict the caller must
-- surface, never an overwrite of bound bytes). Manifest size/checksum/prerequisites need
-- not exist at reserve time; this binds them before chunks are accepted.
UPDATE recovery_captures
SET manifest_bound = true,
    byte_size = @byte_size,
    checksum = @checksum,
    chunk_count = @chunk_count,
    prerequisite_shas = @prerequisite_shas,
    updated_at = now()
WHERE id = @id
  AND (manifest_bound = false OR (byte_size = @byte_size AND checksum = @checksum))
RETURNING *;

-- name: InsertCaptureChunk :exec
-- D4: one ordered, AAD-sealed byte chunk. Inserted inside the single bounded upload
-- transaction M2 owns, alongside the ready transition, so chunks and ready become durable
-- together. sealed is the ciphertext; length is the plaintext byte count for that chunk.
INSERT INTO recovery_capture_chunks (capture_id, chunk_index, length, sealed)
VALUES (@capture_id, @chunk_index, @length, @sealed);

-- name: MarkCaptureReady :one
-- D4: the atomic ready transition to 'available', stamping the expiry that begins at
-- durable capture (never at the earlier reservation). Gated on a bound manifest so a
-- capture can never be advertised ready without its verified byte identity (returns zero
-- rows otherwise).
UPDATE recovery_captures
SET state = 'available', expires_at = @expires_at, updated_at = now()
WHERE id = @id AND manifest_bound = true
RETURNING *;

-- name: MarkCaptureState :one
-- D4: move a capture to a non-ready lifecycle state (uploading / needs_action /
-- discarded) with an optional bounded reason. Used to record a bounded, sanitized
-- oversized/quota/transport/integrity reason on needs_action, or to mark an upload in
-- progress. reason is nullable (sqlc.narg) so a state with no reason clears it.
UPDATE recovery_captures
SET state = @state, reason = sqlc.narg('reason'), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: GetCaptureForOwner :one
-- D6: strict owner-scoped by-id read of one capture. The (id, run_id, user_id) triple is
-- the owner-authorization seam every metadata/download path funnels through — an admin
-- viewing a foreign run is refused because user_id will not match.
SELECT * FROM recovery_captures
WHERE id = @id AND run_id = @run_id AND user_id = @user_id;

-- name: ListCapturesForRunOwner :many
-- D7: every retained capture for one run, owner-scoped, oldest-first. Feeds the run's
-- Recovery archives section (all states shown, not gated on availability).
SELECT * FROM recovery_captures
WHERE run_id = @run_id AND user_id = @user_id
ORDER BY created_at;

-- name: ListCaptureChunks :many
-- D4: the ordered chunk inventory for a download stream. The caller reads chunks in
-- chunk_index order, decrypts each with its AAD, and streams the plaintext without
-- buffering the whole bundle. Owner authorization is enforced upstream by
-- GetCaptureForOwner before this read.
SELECT capture_id, chunk_index, length, sealed
FROM recovery_capture_chunks
WHERE capture_id = @capture_id
ORDER BY chunk_index;

-- name: GetRecoverySummaryForRun :one
-- D7: the per-run recovery aggregate, returning ONE row even when the run has zero
-- captures (aggregate query over recovery_captures = single group). `supported` is true
-- iff any custody hold ever existed for the run (a hold means a recovery-capable worker
-- claimed it; its absence is the legacy/unsupported case M5 renders honestly).
-- `has_open_hold` is the pending-custody signal. The per-state counts let M5 render the
-- section without gating on captures.length>=1. The EXISTS results are cast to boolean so
-- sqlc types them as usable bools (an uncast EXISTS types as interface{}).
SELECT
    (EXISTS (SELECT 1 FROM recovery_custody_holds h
        WHERE h.run_id = @run_id AND h.user_id = @user_id))::boolean AS supported,
    (EXISTS (SELECT 1 FROM recovery_custody_holds h
        WHERE h.run_id = @run_id AND h.user_id = @user_id AND h.state = 'open'))::boolean AS has_open_hold,
    count(c.id) AS capture_count,
    count(*) FILTER (WHERE c.state = 'preparing') AS preparing_count,
    count(*) FILTER (WHERE c.state = 'uploading') AS uploading_count,
    count(*) FILTER (WHERE c.state = 'available') AS available_count,
    count(*) FILTER (WHERE c.state = 'needs_action') AS needs_action_count,
    count(*) FILTER (WHERE c.state = 'expired') AS expired_count,
    count(*) FILTER (WHERE c.state = 'discarded') AS discarded_count
FROM recovery_captures c
WHERE c.run_id = @run_id AND c.user_id = @user_id;

-- name: ReleaseCustodyForRun :execrows
-- D3: the RELEASE mechanism. Nulls both live FKs (dropping the ON DELETE RESTRICT that
-- blocks worker/run teardown), flips state to 'released' and stamps released_at, for every
-- OPEN hold on the run. Idempotent: a second call moves zero rows (no open holds remain).
-- Captures survive (hold_id -> capture is ON DELETE RESTRICT, and this never deletes the
-- hold). The immutable provenance columns are untouched.
UPDATE recovery_custody_holds
SET live_worker_id = NULL, live_run_id = NULL, state = 'released',
    released_at = now(), updated_at = now()
WHERE run_id = @run_id AND state = 'open';

-- name: DiscardCaptureForOwner :execrows
-- D7: owner-initiated explicit discard of one capture. Deletes its byte chunks (freeing
-- storage) and marks the capture 'discarded' in ONE statement — the data-modifying del CTE
-- always runs to completion, and :execrows reports the capture UPDATE's row count (1 when
-- owned, 0 for a foreign/absent id). The capture row is retained (audit), only its bytes go.
WITH owned AS (
    SELECT rc.id AS capture_id FROM recovery_captures rc
    WHERE rc.id = @id AND rc.run_id = @run_id AND rc.user_id = @user_id
),
del AS (
    DELETE FROM recovery_capture_chunks
    WHERE capture_id IN (SELECT owned.capture_id FROM owned)
)
UPDATE recovery_captures c
SET state = 'discarded', updated_at = now()
WHERE c.id = @id AND c.run_id = @run_id AND c.user_id = @user_id;

-- name: ExpireReadyCaptures :execrows
-- D4: the expiry sweep. Moves 'available' captures past their expires_at to 'expired'.
-- Scoped to available+expired-at-past so it NEVER touches an open hold, a pending
-- (preparing/uploading) capture, or a needs_action capture — a ready artifact's TTL must
-- not expire uncaptured custody.
UPDATE recovery_captures
SET state = 'expired', updated_at = now()
WHERE state = 'available' AND expires_at IS NOT NULL AND expires_at < @now;
