-- Job files (PRD #1909 M1). The service layer is workersvc/jobfiles.go; the shared lock and
-- byte sums are store/storedfiles.go. Bytes live in Postgres as AAD-sealed chunks, in the shape
-- of recovery_capture_chunks.
--
-- Every quota read here runs UNDER store.LockStoredFiles (the owner key, then the shared key), in
-- the transaction that then inserts or stamps the reservation. That lock, not a snapshot rule, is
-- what stops two concurrent admissions from both passing a check that only one fits.

-- name: SumStoredFileBytes :one
-- The four sums an admission compares against its quotas. Job bytes count every job_files state
-- except 'expired' (reserved, unattached, attached and available all hold quota). Recovery bytes
-- count 'available' captures by their bound byte_size and 'preparing'/'uploading' captures by the
-- reserved_bytes their upload admission stamped (NULL counts as zero); needs_action, expired and
-- discarded captures stop counting. @exclude_capture_id leaves the capture being admitted out of
-- its own sum (a NULL id excludes nothing).
SELECT
  COALESCE((SELECT sum(f.byte_size) FROM job_files f
             WHERE f.user_id = @user_id AND f.state <> 'expired'), 0)::bigint AS owner_job_bytes,
  COALESCE((SELECT sum(f.byte_size) FROM job_files f
             WHERE f.state <> 'expired'), 0)::bigint AS instance_job_bytes,
  COALESCE((SELECT sum(CASE WHEN c.state = 'available' THEN COALESCE(c.byte_size, 0)
                            ELSE COALESCE(c.reserved_bytes, 0) END)
              FROM recovery_captures c
             WHERE c.user_id = @user_id
               AND c.state IN ('available', 'preparing', 'uploading')
               AND (sqlc.narg('exclude_capture_id')::uuid IS NULL OR c.id <> sqlc.narg('exclude_capture_id')::uuid)), 0)::bigint AS owner_recovery_bytes,
  COALESCE((SELECT sum(CASE WHEN c.state = 'available' THEN COALESCE(c.byte_size, 0)
                            ELSE COALESCE(c.reserved_bytes, 0) END)
              FROM recovery_captures c
             WHERE c.state IN ('available', 'preparing', 'uploading')
               AND (sqlc.narg('exclude_capture_id')::uuid IS NULL OR c.id <> sqlc.narg('exclude_capture_id')::uuid)), 0)::bigint AS instance_recovery_bytes;

-- name: GetOwnedJobRun :one
-- Reservation's ownership check: the run must be a kind='job' run of @user_id. A plain read, taken
-- BEFORE the stored-files advisory keys so a runs row lock held elsewhere can never stall the
-- instance-wide admission queue. runs.user_id and runs.kind never change, and the job_files.run_id
-- foreign key stops a concurrent delete of the run under the insert. Zero rows: the run is not the
-- caller's, or not a job.
SELECT id FROM runs WHERE id = @id AND user_id = @user_id AND kind = 'job';

-- name: SumCommittedSharedBytes :one
-- The bytes actually on the books, for the bind-time reclaim. committed_bytes is every job file
-- that is neither 'expired' nor 'reserved' (available/attached, verified bytes) plus every
-- 'available' recovery capture by its bound byte_size. It deliberately leaves out BOTH kinds of
-- unverified declared bytes: in-flight capture reservations (preparing/uploading) and job files
-- still in state 'reserved' (size declared at admission, nothing verified yet). A reclaim sized
-- from committed_bytes therefore can never be inflated by bytes another party only declared.
-- reserved_job_bytes returns the 'reserved' job-file bytes separately: the caller does not size a
-- reclaim from them, but must leave that room free, because a job-file Write never re-checks the
-- budget. @exclude_capture_id leaves the capture being bound out (it is not 'available' yet, so
-- this is belt and braces).
SELECT (
  COALESCE((SELECT sum(f.byte_size) FROM job_files f WHERE f.state NOT IN ('expired', 'reserved')), 0)
  + COALESCE((SELECT sum(COALESCE(c.byte_size, 0)) FROM recovery_captures c
               WHERE c.state = 'available'
                 AND (sqlc.narg('exclude_capture_id')::uuid IS NULL OR c.id <> sqlc.narg('exclude_capture_id')::uuid)), 0)
)::bigint AS committed_bytes,
COALESCE((SELECT sum(f.byte_size) FROM job_files f WHERE f.state = 'reserved'), 0)::bigint AS reserved_job_bytes;

-- name: SumRunJobFiles :one
-- One run's live (non-expired) files of one direction: the per-job file-count and byte caps. The
-- two files the SERVER generates from a job's stored result (report.md and findings.json, see
-- workersvc.SubmitJobResult) are not the job's own outputs and do not count against the per-job
-- caps; the worker output route refuses those two names (case-insensitively), and the server writes
-- exactly these two constants, so the match here is EXACT (no lower(): a locale-dependent fold such as
-- U+0130 would exempt a name the Go predicate, strings.EqualFold, does not treat as reserved).
-- They still count toward the owner, instance and shared-budget sums.
SELECT count(*)::bigint AS file_count, COALESCE(sum(byte_size), 0)::bigint AS total_bytes
  FROM job_files
 WHERE run_id = @run_id AND direction = @direction AND state <> 'expired'
   AND NOT (direction = 'output' AND display_name IN ('report.md', 'findings.json'));

-- name: ReserveJobFile :one
-- Admission: the row in state 'reserved' at the DECLARED size, before any chunk is written.
-- sha256 is set only when the caller declared one (checked against the streamed bytes).
INSERT INTO job_files (user_id, product_id, run_id, direction, claim_generation, display_name, byte_size, sha256, state)
VALUES (@user_id, sqlc.narg('product_id'), sqlc.narg('run_id'), @direction, sqlc.narg('claim_generation'),
        @display_name, @byte_size, sqlc.narg('sha256'), 'reserved')
RETURNING *;

-- name: LockReservedJobFile :one
-- Row-locks a reservation for the streaming write. Zero rows: it is gone (released or swept) or
-- already committed.
SELECT * FROM job_files WHERE id = @id AND state = 'reserved' FOR UPDATE;

-- name: InsertJobFileChunk :exec
INSERT INTO job_file_chunks (file_id, chunk_index, length, sealed)
VALUES (@file_id, @chunk_index, @length, @sealed);

-- name: FinalizeJobFile :one
-- Commits a reservation: the streamed size, the content identity and the resting state. An
-- input with no run yet rests 'unattached' with the upload TTL; an output (or an input already
-- bound to a run) rests 'attached' with no expiry until the run ends.
UPDATE job_files
   SET state = @state, byte_size = @byte_size, sha256 = @sha256, storage_name = @storage_name,
       content_type = @content_type, chunk_count = @chunk_count,
       expires_at = sqlc.narg('expires_at'), updated_at = now()
 WHERE id = @id AND state = 'reserved'
RETURNING *;

-- name: GetJobFileForOwner :one
-- Owner-scoped read: a file that is not the caller's reads as no row.
SELECT * FROM job_files WHERE id = @id AND user_id = @user_id;

-- name: GetJobFileChunk :one
SELECT length, sealed FROM job_file_chunks WHERE file_id = @file_id AND chunk_index = @chunk_index;

-- name: ReleaseJobFileReservation :execrows
-- The reservation goes back the moment its upload fails. Only a 'reserved' row of this owner is
-- ever deleted; a committed file is never touched.
DELETE FROM job_files WHERE id = @id AND user_id = @user_id AND state = 'reserved';

-- name: ReleaseStaleJobFileReservations :execrows
-- The backstop for a reservation whose request died without releasing it (a crash, a lost
-- context): reserved rows older than @cutoff. Chunks cascade, and a reserved row has none (the
-- streaming write commits its chunks and the finalize together). A row a live Write holds FOR
-- UPDATE is SKIPPED, never waited on: the sweep holds the shared advisory key, so waiting on a
-- slow writer would stall every admission behind it. The skipped row is picked up next tick.
DELETE FROM job_files
 WHERE id IN (SELECT f.id FROM job_files f
               WHERE f.state = 'reserved' AND f.created_at < @cutoff
                 FOR UPDATE SKIP LOCKED);

-- name: SettleTerminalJobFiles :execrows
-- Attached files of a terminal job become downloadable 'available' files with their retention
-- clock starting at the run's end.
UPDATE job_files f
   SET state = 'available',
       expires_at = COALESCE(r.finished_at, now()) + make_interval(secs => @retention_seconds::float8),
       updated_at = now()
  FROM runs r
 WHERE f.run_id = r.id
   AND f.state = 'attached'
   AND r.status IN ('completed', 'failed', 'cancelled');

-- name: ExpireJobFiles :one
-- The retention sweep: unattached and available files past expires_at flip to 'expired' AND lose
-- their chunks in the one statement (the ExpireReadyCaptures shape), so an expired file stops
-- costing bytes while its row stays as an honest tombstone. Never touches reserved or attached.
-- The UPDATE itself carries the state and expiry predicate (re-evaluated against the locked row
-- under READ COMMITTED) and the chunk delete follows only the rows it RETURNED, so a file whose
-- state changed concurrently (an attach) is neither expired nor stripped of its chunks. The result
-- is the number of files expired.
WITH upd AS (
    UPDATE job_files SET state = 'expired', updated_at = now()
     WHERE state IN ('unattached', 'available') AND expires_at IS NOT NULL AND expires_at < @now
    RETURNING id
),
del AS (
    DELETE FROM job_file_chunks WHERE file_id IN (SELECT id FROM upd)
)
SELECT count(*)::bigint AS expired FROM upd;

-- name: ReclaimJobFilesForRecovery :one
-- Recovery admission (PRD #1909 D2): free at least @need bytes of job files, in this order: files
-- already past their expiry (unattached or available), then the oldest 'available' files. NEVER
-- an attached file (its job is live), a reserved one (an upload is in flight), or an unattached
-- one still inside its TTL. Picked files lose their chunks and flip to 'expired' in one
-- statement; the result is the bytes actually freed (possibly less than @need: the caller
-- re-checks the budget). Runs under store.LockStoredFiles, so nothing races the sums.
WITH cand AS (
    SELECT f.id, f.byte_size,
           COALESCE(sum(f.byte_size) OVER (
               ORDER BY (f.state IN ('unattached', 'available') AND f.expires_at IS NOT NULL AND f.expires_at < @now::timestamptz) DESC,
                        f.created_at, f.id) - f.byte_size, 0) AS bytes_before
      FROM job_files f
     WHERE (f.state IN ('unattached', 'available') AND f.expires_at IS NOT NULL AND f.expires_at < @now::timestamptz)
        OR f.state = 'available'
),
picked AS (
    SELECT id FROM cand WHERE bytes_before < @need::bigint
),
upd AS (
    -- The state predicate is repeated on the UPDATE so a file whose state changed since the
    -- snapshot (an attach) is left alone; the chunk delete follows only the RETURNED rows.
    UPDATE job_files SET state = 'expired', updated_at = now()
     WHERE id IN (SELECT id FROM picked)
       AND ((state IN ('unattached', 'available') AND expires_at IS NOT NULL AND expires_at < @now::timestamptz)
            OR state = 'available')
    RETURNING id, byte_size
),
del AS (
    DELETE FROM job_file_chunks WHERE file_id IN (SELECT id FROM upd)
)
SELECT COALESCE(sum(byte_size), 0)::bigint AS freed_bytes FROM upd;

-- name: SumReclaimableJobFileBytes :one
-- The bytes ReclaimJobFilesForRecovery COULD free right now: the same selection rule (files past
-- their expiry, then every 'available' file; never attached, reserved or an unattached file inside
-- its TTL) as a READ-ONLY sum. Recovery admission checks the budget against this without
-- destroying anything; the reclaim itself waits for a verified upload.
SELECT COALESCE(sum(f.byte_size), 0)::bigint AS reclaimable_bytes
  FROM job_files f
 WHERE (f.state IN ('unattached', 'available') AND f.expires_at IS NOT NULL AND f.expires_at < @now::timestamptz)
    OR f.state = 'available';

-- name: StampCaptureReservation :execrows
-- Recovery admission (PRD #1909 D4): the upload's reserved size, and the 'uploading' state that
-- makes the shared sums count it while the chunk stream runs outside the lock. Also the retry
-- path: a needs_action capture retried gets its reservation counted again, and its stale failure
-- reason is cleared. A capture that is available, expired or discarded is not stampable (zero rows).
UPDATE recovery_captures
   SET state = 'uploading', reserved_bytes = @reserved_bytes, reason = NULL, updated_at = now()
 WHERE id = @id AND state IN ('preparing', 'uploading', 'needs_action');

-- name: AttachInputJobFiles :many
-- Job create (PRD #1909 D5): attach uploaded inputs to a new job in ONE guarded statement. A row
-- matches only when it is the caller's own (user_id), was uploaded through the same product
-- (IS NOT DISTINCT FROM: a user-token upload, NULL, attaches only for a user-token caller, and a
-- product's upload only for that product), is an 'unattached' input and has not expired. The
-- caller compares the number of rows returned with the number of distinct ids it sent: every
-- mismatch (unknown id, another owner's or product's file, an attached, expired or output file)
-- reads the same. Runs under store.LockStoredFiles, in the create transaction: the reclaim and
-- expiry statements re-check state, but rely on that lock order. The file leaves its upload TTL:
-- expires_at is NULL while attached, and the sweep settles it when the job ends.
UPDATE job_files
   SET run_id = @run_id, state = 'attached', expires_at = NULL, updated_at = now()
 WHERE id = ANY(@ids::uuid[])
   AND user_id = @user_id
   AND product_id IS NOT DISTINCT FROM sqlc.narg('product_id')::uuid
   AND direction = 'input'
   AND state = 'unattached'
   AND expires_at > now()
RETURNING id, byte_size;

-- name: ListJobInputFilesForClaim :many
-- The attached input files of a job, for the claim manifest (PRD #1909 D8). NOT caller-scoped: the
-- claim path runs as the worker that just claimed the run and the run id comes from the claimed
-- row. Only files attached to THIS run, direction 'input', state 'attached'; a stable order so a
-- re-claim renders the same manifest.
SELECT id, storage_name, display_name, byte_size, sha256, content_type
  FROM job_files
 WHERE run_id = @run_id AND direction = 'input' AND state = 'attached'
 ORDER BY created_at ASC, id ASC;

-- name: FindJobOutputFile :one
-- Output upload idempotency (PRD #1909 M4): a retried POST of the same content under the same
-- display name for the same run and claim generation finds the file the first attempt stored, so
-- the retry stores nothing twice and is not charged against the per-job caps again. Only a
-- committed file ('attached' while the job runs, 'available' after) matches; a reservation still
-- streaming does not.
SELECT * FROM job_files
 WHERE run_id = @run_id AND direction = 'output' AND claim_generation = @claim_generation
   AND state IN ('attached', 'available')
   AND sha256 = @sha256 AND display_name = @display_name
 ORDER BY created_at ASC, id ASC
 LIMIT 1;

-- name: InsertJobOutputRefusal :execrows
-- Records one refused output (PRD #1909 D4). Bounded: nothing is inserted once the run already
-- has @max_rows refusal rows (the per-job output file cap), and an identical refusal (same name,
-- size and reason) is recorded once, so a retried refused upload does not fill the table. The
-- result is the number of rows inserted (0 or 1); a refusal that is not recorded is still a
-- refusal. The bound is checked in the statement, not under a lock: uploads for one run are
-- sequential from its worker, and concurrent ones can overshoot by at most the upload slot count.
-- The rows the server's own output generation owns (post_id IS NOT NULL: its generation_pending
-- marker and outcome, InsertGeneratedOutputMarker) are neither counted against the bound nor
-- matched by the duplicate check: they are at most two per run and are not the worker's refusals, so
-- neither can crowd the other out.
-- FENCED: the row is inserted only while @claim_generation is the run's CURRENT generation and the
-- claim is not released, so a stale flight's refusal that arrives after a re-claim has cleared the
-- outputs (ClearRunOutputs) is dropped instead of surviving into the new flight.
INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason)
SELECT @run_id::uuid, @display_name::text, @byte_size::bigint, @reason::text
 WHERE EXISTS (SELECT 1 FROM runs
                WHERE id = @run_id::uuid AND claim_generation = @claim_generation::bigint
                  AND claim_released_at IS NULL)
   AND (SELECT count(*) FROM job_output_refusals
         WHERE run_id = @run_id::uuid AND post_id IS NULL) < @max_rows::bigint
   AND NOT EXISTS (SELECT 1 FROM job_output_refusals
                    WHERE run_id = @run_id::uuid AND post_id IS NULL
                      AND display_name = @display_name::text
                      AND byte_size = @byte_size::bigint AND reason = @reason::text);

-- name: NextJobOutputPostID :one
-- The id of one job-result post (PRD #1909 M4): drawn inside the result transaction AFTER the run
-- row is locked FOR UPDATE, so the posts of one run take ascending ids in commit order (the lock is
-- held from the draw to the commit). The generation of an older post uses that order to see it has
-- been superseded (workersvc.startJobResultOutputs).
SELECT nextval('job_output_post_seq')::bigint;

-- name: InsertGeneratedOutputMarker :execrows
-- Writes the generation_pending marker of one generated output (PRD #1909 M4), owned by the post
-- @post_id. NOT bounded by the per-run refusal cap (InsertJobOutputRefusal): a run has at most one
-- marker or outcome row per reserved name, because every post first deletes the earlier post's rows
-- (DeleteGeneratedOutputRefusals) in the same transaction. FENCED on the claim generation like the
-- other refusal writes.
INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason, post_id)
SELECT @run_id::uuid, @display_name::text, 0, @reason::text, @post_id::bigint
 WHERE EXISTS (SELECT 1 FROM runs
                WHERE id = @run_id::uuid AND claim_generation = @claim_generation::bigint
                  AND claim_released_at IS NULL);

-- name: DeleteJobOutputFilesForRun :execrows
-- Re-claim (PRD #1909 M4): drops every output row of the run, in any state, so the new flight
-- starts with none of an earlier flight's. The chunks cascade. A row an in-flight write of the
-- earlier flight holds FOR UPDATE is SKIPPED, never waited on: the claim response must not block
-- on an old flight's write. The skipped row is dropped by that write's own post-write fence
-- (workersvc.StoreJobOutput, dropStaleOutput) as soon as it commits.
DELETE FROM job_files
 WHERE id IN (SELECT f.id FROM job_files f
               WHERE f.run_id = @run_id AND f.direction = 'output'
                 FOR UPDATE SKIP LOCKED);

-- name: DeleteJobOutputRefusalsForRun :execrows
DELETE FROM job_output_refusals
 WHERE id IN (SELECT r.id FROM job_output_refusals r WHERE r.run_id = @run_id FOR UPDATE SKIP LOCKED);

-- name: DeleteGeneratedOutputRefusals :execrows
-- Clears every refusal row the server's own output generation owns (post_id IS NOT NULL: the
-- generation_pending marker, a generation_failed / generation_timeout row, or a refusal the
-- generation recorded such as file_too_large or a quota) for the given names, whichever post wrote
-- it (PRD #1909 M4). NOT the worker's own rows: a worker-reported drop and the reserved_name
-- refusal carry no post_id and stay. Used when a result is (re-)posted: the new post takes over the
-- names, so an earlier post's rows go with it. FENCED: a no-op unless @claim_generation is the
-- run's CURRENT generation and the claim is not released.
DELETE FROM job_output_refusals
 WHERE run_id = @run_id::uuid
   AND display_name = ANY(@display_names::text[])
   AND post_id IS NOT NULL
   AND EXISTS (SELECT 1 FROM runs
                WHERE id = @run_id::uuid AND claim_generation = @claim_generation::bigint
                  AND claim_released_at IS NULL);

-- name: ClearGeneratedOutputRefusal :execrows
-- Deletes the row post @post_id owns for one name, once its file is stored. Scoped to the post: an
-- older post's generation finishing after a newer post took over the names matches nothing, so it
-- cannot clear the newer post's marker (the file of a superseded post is not what the marker
-- promises). Fenced like the insert.
DELETE FROM job_output_refusals
 WHERE run_id = @run_id::uuid
   AND display_name = @display_name::text
   AND post_id = @post_id::bigint
   AND EXISTS (SELECT 1 FROM runs
                WHERE id = @run_id::uuid AND claim_generation = @claim_generation::bigint
                  AND claim_released_at IS NULL);

-- name: SettleGeneratedOutputRefusal :execrows
-- Turns the row post @post_id owns for a name (its generation_pending marker) into the outcome of
-- the generation (a refusal reason), in place (PRD #1909 M4). 0 rows means the post was superseded
-- (a newer post deleted its rows) or the claim is stale: the caller records nothing, because the
-- newer post's own marker says the file is missing. Fenced on the claim generation like the insert.
UPDATE job_output_refusals
   SET reason = @reason::text, byte_size = @byte_size::bigint
 WHERE run_id = @run_id::uuid
   AND display_name = @display_name::text
   AND post_id = @post_id::bigint
   AND EXISTS (SELECT 1 FROM runs
                WHERE id = @run_id::uuid AND claim_generation = @claim_generation::bigint
                  AND claim_released_at IS NULL);

-- name: HasGeneratedOutputMarker :one
-- Whether post @post_id still owns a row for the name: false means a newer post of the run took
-- over the names (or a re-claim cleared them), so the post's generation is superseded and must not
-- store its file (PRD #1909 M4).
SELECT EXISTS (SELECT 1 FROM job_output_refusals
                WHERE run_id = @run_id::uuid AND display_name = @display_name::text
                  AND post_id = @post_id::bigint)::boolean;

-- name: DeleteStaleGeneratedJobOutput :execrows
-- A re-posted job result replaces the files the server generated from it (report.md and
-- findings.json): the same-name file of this run and claim generation whose content differs is
-- dropped so the new content can be stored. A same-content file is left alone (the unique
-- (run, generation, name, sha256) index makes the re-store a no-op). The chunks cascade.
DELETE FROM job_files
 WHERE id IN (SELECT f.id FROM job_files f
               WHERE f.run_id = @run_id AND f.direction = 'output'
                 AND f.claim_generation = @claim_generation AND f.display_name = @display_name
                 AND f.state IN ('attached', 'available')
                 AND f.sha256 IS DISTINCT FROM @sha256::text
                 FOR UPDATE SKIP LOCKED);

-- name: DeleteJobOutputFile :execrows
-- Drops one output of a run whose upload finished after its claim went stale (the fence re-check
-- after the write). Scoped to the run and the uploading generation.
DELETE FROM job_files
 WHERE id = @id AND run_id = @run_id AND direction = 'output' AND claim_generation = @claim_generation;

-- name: ListJobFilesForRun :many
-- The files of one job for the result and GET /jobs/{id}/files (PRD #1909 M5): its attached inputs
-- and its outputs, in every state a caller can observe (a run's files are never 'reserved' once
-- committed, and 'unattached' files have no run). NOT caller-scoped: the service has already
-- resolved the job through GetJobForCaller.
--
-- source_url (PRD #1909 D7) is set ONLY for an OUTPUT whose sha256 equals the non-empty sha256 of
-- an 'allowed' fetch recorded in run_fetches for THIS SAME run (rf.run_id = f.run_id). run_fetches
-- is written by the fetch service from the forwarded run credential, so nothing the agent or the
-- worker claims about a file's origin reaches it. A refused fetch has no bytes and an empty
-- sha256, and an expired file with no retained hash never matches. The earliest such fetch wins
-- (created_at, id), so the answer is stable; the final URL (after redirects) is the origin when the
-- fetch recorded one.
SELECT f.id, f.display_name, COALESCE(f.content_type, '')::text AS content_type, f.byte_size,
       COALESCE(f.sha256, '')::text AS sha256, f.direction, f.state, f.expires_at,
       COALESCE(src.source_url, '')::text AS source_url
  FROM job_files f
  LEFT JOIN LATERAL (
        SELECT COALESCE(NULLIF(rf.final_url, ''), rf.url) AS source_url
          FROM run_fetches rf
         WHERE f.direction = 'output'
           AND rf.run_id = f.run_id
           AND rf.verdict = 'allowed'
           AND rf.sha256 <> ''
           AND rf.sha256 = f.sha256
         ORDER BY rf.created_at, rf.id
         LIMIT 1) src ON true
 WHERE f.run_id = @run_id
   AND f.state IN ('attached', 'available', 'expired')
 ORDER BY f.created_at, f.id
 LIMIT @max_rows;

-- name: ListJobOutputRefusalsForRun :many
-- Every refusal row of a run (the worker's drops, upload refusals and the server's generation
-- markers and outcomes), oldest first. Bounded: the per-run refusal cap plus at most two
-- generation rows, so @max_rows is only a backstop. Not caller-scoped (see ListJobFilesForRun).
SELECT display_name, byte_size, reason
  FROM job_output_refusals
 WHERE run_id = @run_id
 ORDER BY created_at, id
 LIMIT @max_rows;

-- name: ListJobSourcesForRun :many
-- The source log of one job run (PRD #1909 D7): this run's run_fetches rows, oldest first. The
-- columns a product needs; the fetch service wrote them, and its text fields are escaped and
-- bounded at write time. Not caller-scoped (see ListJobFilesForRun).
SELECT url, final_url, verdict, reason, http_status, content_type, bytes, sha256, finished_at
  FROM run_fetches
 WHERE run_id = @run_id
 ORDER BY created_at, id
 LIMIT @max_rows;

-- name: GetJobFileForCaller :one
-- GET /files/{id} ownership (PRD #1909 M5). The caller must own the file (user_id), and:
--   * a file with NO run yet (an 'unattached' input) is visible only to the product it was uploaded
--     through (product_id IS NOT DISTINCT FROM the caller's: a uzc_ caller, NULL, sees only files
--     uploaded with no product, a uzp_ caller only its own product's), the model
--     AttachInputJobFiles uses;
--   * a file of a run (an attached input, or an output, stored with no product because the worker
--     has none) is visible exactly as its job is, the GetJobForCaller predicate: the job is a
--     kind='job' run of the caller and, for a product caller, originated from that product. The
--     listing on GET /jobs/{id}/files uses the same predicate through the job, so a file it lists
--     is one the caller can fetch.
-- A reserved (not yet committed) file is never returned. Every mismatch reads as no row.
SELECT f.* FROM job_files f
 WHERE f.id = @id
   AND f.user_id = @user_id
   AND f.state <> 'reserved'
   AND ((f.run_id IS NULL
         AND f.product_id IS NOT DISTINCT FROM sqlc.narg('product_id')::uuid)
        OR (f.run_id IS NOT NULL
            AND EXISTS (SELECT 1 FROM runs r
                          JOIN job_origins o ON o.run_id = r.id
                         WHERE r.id = f.run_id
                           AND r.kind = 'job'
                           AND r.user_id = @user_id
                           AND (sqlc.narg('product_id')::uuid IS NULL
                                OR o.product_id = sqlc.narg('product_id')::uuid))));
