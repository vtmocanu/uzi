-- Per-run fetch credential, admission and source log (PRD #1906 M3) --------------
-- Schema and rules: migration 00270_run_fetch.sql. The Go side is api/internal/fetchctl
-- (Begin / Complete / the stale sweep) and workersvc's claim-time mint.

-- name: GetEgressProfileByID :one
SELECT * FROM egress_profiles WHERE id = @id;

-- name: SetRunEgressSnapshotOnce :exec
-- The claim-time snapshot, written only at the first claim: a later claim finds it set and
-- this matches no row (the immutability trigger would refuse a change anyway).
UPDATE runs SET egress_snapshot = @snapshot::jsonb
WHERE id = @id AND egress_snapshot IS NULL;

-- name: GetRunEgressSnapshot :one
SELECT egress_snapshot FROM runs WHERE id = @id;

-- name: MintRunFetchCredential :execrows
-- Mint (or, on a re-claim, rotate) the run's credential. Only while the run is still in the
-- claim that asked for it (status claimed or running, the same claim_generation, still
-- profile-bound): the run row is locked FOR SHARE first, so a concurrent cancel either
-- committed before (no row, nothing minted) or waits for this and then revokes what it
-- minted through the status trigger. Counters are run totals and are never reset here.
WITH r AS (
    SELECT id FROM runs
    WHERE id = @run_id
      AND claim_generation = @claim_generation
      AND status IN ('claimed', 'running')
      AND egress_profile_id IS NOT NULL
    FOR SHARE
)
INSERT INTO run_fetch_credentials (run_id, token_hash, claim_generation)
SELECT r.id, @token_hash, @claim_generation FROM r
ON CONFLICT (run_id) DO UPDATE
SET token_hash       = EXCLUDED.token_hash,
    claim_generation = EXCLUDED.claim_generation,
    revoked_at       = NULL,
    updated_at       = now();

-- name: ReleasePriorGenerationFetchReservations :one
-- At a re-claim's mint: the open reservations of an earlier claim belong to fetches whose
-- credential no longer exists, so they are released now rather than holding concurrency
-- slots until the stale sweep. Returns how many were released.
WITH rel AS (
    UPDATE run_fetch_reservations fr
    SET settled = true, settled_at = now()
    WHERE fr.run_id = @run_id AND fr.claim_generation < @claim_generation AND NOT fr.settled
    RETURNING fr.bytes
), upd AS (
    UPDATE run_fetch_credentials c
    SET reserved_bytes = c.reserved_bytes - (SELECT COALESCE(sum(bytes), 0)::bigint FROM rel),
        inflight       = c.inflight - (SELECT count(*)::bigint FROM rel),
        updated_at     = now()
    WHERE c.run_id = @run_id AND EXISTS (SELECT 1 FROM rel)
    RETURNING 1
)
SELECT count(*)::bigint FROM rel;

-- name: LockFetchCredentialByHash :one
-- Begin's credential lookup. The credential row is the admission lock: every Begin for a
-- run serializes here. The run row is read, not locked (locking it would invert the order
-- the revoke trigger takes: runs, then this row).
SELECT c.run_id,
       c.claim_generation,
       c.revoked_at,
       r.status,
       r.claim_generation AS run_claim_generation,
       r.egress_snapshot
FROM run_fetch_credentials c
JOIN runs r ON r.id = c.run_id
WHERE c.token_hash = @token_hash
FOR UPDATE OF c;

-- name: BumpFetchAttempt :execrows
-- Counts one admission request against the attempts cap. It runs, and commits, even when
-- the reservation below is then refused, so refused requests are bounded too.
UPDATE run_fetch_credentials
SET attempts = attempts + 1, updated_at = now()
WHERE run_id = @run_id AND attempts < @max_attempts::bigint;

-- name: ReserveFetch :execrows
-- The atomic reservation (Decision 3): the per-file maximum, one file slot and one
-- concurrency slot, only if every run total still has room.
UPDATE run_fetch_credentials
SET reserved_bytes = reserved_bytes + @max_file_bytes::bigint,
    files          = files + 1,
    inflight       = inflight + 1,
    updated_at     = now()
WHERE run_id = @run_id
  AND reserved_bytes + used_bytes + @max_file_bytes::bigint <= @max_run_bytes::bigint
  AND files < @max_run_files::bigint
  AND inflight < @max_inflight::bigint;

-- name: GetFetchCounters :one
SELECT reserved_bytes, used_bytes, files, inflight, attempts
FROM run_fetch_credentials WHERE run_id = @run_id;

-- name: InsertFetchReservation :one
INSERT INTO run_fetch_reservations (run_id, claim_generation, bytes)
VALUES (@run_id, @claim_generation, @bytes)
RETURNING id;

-- name: GetFetchCredentialRunByHash :one
-- Complete's credential lookup: the run and the generation the token belongs to. A revoked
-- credential still resolves, so an attempt admitted before the run ended is still logged.
SELECT run_id, claim_generation FROM run_fetch_credentials WHERE token_hash = @token_hash;

-- name: LockFetchReservation :one
-- The reservation, only when it belongs to the credential's run.
SELECT * FROM run_fetch_reservations
WHERE id = @id AND run_id = @run_id
FOR UPDATE;

-- name: RunFetchLoggedForReservation :one
SELECT EXISTS (SELECT 1 FROM run_fetches WHERE reservation_id = @reservation_id)::boolean;

-- name: SettleFetchReservation :exec
UPDATE run_fetch_reservations SET settled = true, settled_at = now() WHERE id = @id;

-- name: ReconcileFetchCounters :exec
-- Complete's settlement. release_bytes / release_inflight are the reservation's bytes and 1
-- when the reservation was still open (0 when the sweep already released it); used_bytes is
-- what the fetch returned; refund_files is 1 for a refused attempt, whose file slot goes back.
UPDATE run_fetch_credentials
SET reserved_bytes = reserved_bytes - @release_bytes::bigint,
    inflight       = inflight - @release_inflight::bigint,
    used_bytes     = used_bytes + @used_bytes::bigint,
    files          = files - @refund_files::bigint,
    updated_at     = now()
WHERE run_id = @run_id;

-- name: InsertRunFetch :exec
INSERT INTO run_fetches (
    run_id, reservation_id, url, final_url, verdict, reason, http_status,
    content_type, bytes, sha256, started_at, finished_at
) VALUES (
    @run_id, @reservation_id, @url, @final_url, @verdict, @reason, @http_status,
    @content_type, @bytes, @sha256, @started_at, @finished_at
);

-- name: ReleaseStaleFetchReservations :one
-- The fetch_reservations_stale sweep: a reservation older than @cutoff belongs to a fetch
-- that crashed or lost its Complete, so its bytes and concurrency slot go back. The file
-- slot stays counted (the outcome is unknown). A late Complete still logs the attempt.
WITH rel AS (
    UPDATE run_fetch_reservations fr
    SET settled = true, settled_at = now()
    WHERE NOT fr.settled AND fr.created_at < @cutoff
    RETURNING fr.run_id, fr.bytes
), agg AS (
    SELECT run_id, sum(bytes)::bigint AS b, count(*)::bigint AS n FROM rel GROUP BY run_id
), upd AS (
    UPDATE run_fetch_credentials c
    SET reserved_bytes = c.reserved_bytes - agg.b,
        inflight       = c.inflight - agg.n,
        updated_at     = now()
    FROM agg
    WHERE c.run_id = agg.run_id
    RETURNING 1
)
SELECT count(*)::bigint FROM rel;

-- name: ListRunFetches :many
-- The owner read. The caller has already checked ownership.
SELECT * FROM run_fetches
WHERE run_id = @run_id
ORDER BY created_at, id
LIMIT @max_rows;
