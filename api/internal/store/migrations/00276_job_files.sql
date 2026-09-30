-- +goose Up

-- PRD #1909 M1: job files. Bounded, encrypted, short-lived file storage for the /api/v1 job
-- surface, kept in Postgres in the recovery-archive shape (a metadata row plus sealed ~1 MiB
-- chunks), a shared stored-file budget that lets job files and recovery archives count against
-- one ceiling, and the rollout gate column that keeps a new-protocol job away from an old worker.
--
-- The migration number is a draft: it is renumbered above the live head at landing.

-- 1. job_files: one row per stored file. Inputs are uploaded by the job's owner (state
-- 'unattached' until a job create attaches them); outputs are uploaded by the worker that
-- holds the run. Lifecycle:
--   reserved   -> admitted (quota reserved at the DECLARED size), bytes not yet committed.
--   unattached -> an input whose bytes are committed but no job references it yet (TTL).
--   attached   -> referenced by a live (non-terminal) job.
--   available  -> the job is terminal; the file is downloadable until expires_at.
--   expired    -> the bytes are gone (chunks deleted); the row stays as an honest tombstone.
-- Every state except 'expired' counts toward the quotas (PRD #1909 D4).
--
-- sha256 / storage_name / content_type are NULL only while the row is 'reserved': a streamed
-- upload learns them from the bytes. storage_name is `<sha256>.<ext>`, derived from the content
-- and the DETECTED type, never from the uploader (D6); display_name is sanitised metadata only:
-- no path separators, no control characters and no Unicode format (Cf), line (Zl) or paragraph
-- (Zp) separator characters (bidi overrides and isolates, zero-width characters, the BOM). The
-- service's validate applies the complete Cf/Zl/Zp test; the CHECK here is the backstop over the
-- ranges that matter. job_output_refusals.display_name carries the same CHECK.
CREATE TABLE job_files (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    product_id      uuid REFERENCES products ON DELETE CASCADE,
    run_id          uuid REFERENCES runs ON DELETE CASCADE,
    direction       text NOT NULL CHECK (direction IN ('input', 'output')),
    -- The claim generation that uploaded an output; NULL for inputs.
    claim_generation bigint,
    storage_name    text CHECK (storage_name ~ '^[0-9a-f]{64}\.[a-z0-9]{1,8}$'),
    display_name    text NOT NULL
        CHECK (char_length(display_name) BETWEEN 1 AND 255
               AND display_name !~ '[/\\]'
               AND display_name !~ '[[:cntrl:]]'
               AND display_name !~ '[\u00AD\u061C\u180E\u200B-\u200F\u2028-\u202E\u2060-\u206F\uFEFF\uFFF9-\uFFFB]'),
    content_type    text CHECK (content_type IN (
        'application/pdf', 'image/png', 'image/jpeg', 'text/plain', 'text/markdown',
        'text/csv', 'application/json', 'text/html',
        'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
        'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet')),
    -- The declared size while 'reserved' (the reservation is exactly this), the streamed size
    -- once committed. The DB cap is a backstop; the service enforces the configured caps.
    byte_size       bigint NOT NULL CHECK (byte_size >= 0),
    sha256          text CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    chunk_count     int NOT NULL DEFAULT 0 CHECK (chunk_count >= 0),
    state           text NOT NULL CHECK (state IN ('reserved', 'unattached', 'attached', 'available', 'expired')),
    expires_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- Content identity is known from the moment bytes are committed.
    CONSTRAINT job_files_committed_has_identity
        CHECK (state IN ('reserved', 'expired')
               OR (sha256 IS NOT NULL AND storage_name IS NOT NULL AND content_type IS NOT NULL)),
    -- An output always belongs to a run; an input belongs to one only once attached.
    CONSTRAINT job_files_output_has_run CHECK (direction = 'input' OR run_id IS NOT NULL),
    CONSTRAINT job_files_attached_has_run CHECK (state <> 'attached' OR run_id IS NOT NULL)
);

-- The quota sums (per owner, per state) and the owner listings.
CREATE INDEX idx_job_files_user_state ON job_files (user_id, state);
-- A run's files: the per-job caps, the result listing, the attach and terminal-transition sweeps.
CREATE INDEX idx_job_files_run ON job_files (run_id) WHERE run_id IS NOT NULL;
-- The expiry sweep and the recovery reclaim order scan files that can expire by time.
CREATE INDEX idx_job_files_expiry ON job_files (expires_at) WHERE state IN ('unattached', 'available');
-- One output per (run, claim generation, name, content): the duplicate guard behind the output
-- route's idempotency lookup. Two concurrent uploads of the same content under the same name (a
-- worker retry that overlaps its first attempt) cannot both be admitted: the second Reserve hits
-- this index and the service answers with the stored file (or "still being stored"). It covers the
-- 'reserved' state too because the worker route declares the sha256 at admission; an expired file
-- (a tombstone) does not block a new one.
CREATE UNIQUE INDEX uq_job_files_output_content
    ON job_files (run_id, claim_generation, display_name, sha256)
    WHERE direction = 'output' AND state <> 'expired';
-- The reservation-release sweep scans stale reservations.
CREATE INDEX idx_job_files_reserved ON job_files (created_at) WHERE state = 'reserved';

-- 2. job_file_chunks: ordered AAD-sealed chunks of one file (the recovery_capture_chunks shape).
-- sealed is the ciphertext (~1 MiB plaintext per chunk); length is the plaintext length. The
-- AAD binds file id, owner, index and length, so a chunk cannot be replayed into another file.
CREATE TABLE job_file_chunks (
    file_id     uuid NOT NULL REFERENCES job_files ON DELETE CASCADE,
    chunk_index int  NOT NULL CHECK (chunk_index >= 0),
    length      int  NOT NULL CHECK (length > 0),
    sealed      bytea NOT NULL,
    PRIMARY KEY (file_id, chunk_index)
);

-- 3. job_output_refusals: an output the worker offered that the quotas or the type allowlist
-- refused. The job still completes; its result lists these (PRD #1909 D4). Bounded per job by
-- the output file cap in the service. The cascade removes them with the run.
CREATE TABLE job_output_refusals (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       uuid NOT NULL REFERENCES runs ON DELETE CASCADE,
    display_name text NOT NULL
        CHECK (char_length(display_name) BETWEEN 1 AND 255
               AND display_name !~ '[/\\]'
               AND display_name !~ '[[:cntrl:]]'
               AND display_name !~ '[\u00AD\u061C\u180E\u200B-\u200F\u2028-\u202E\u2060-\u206F\uFEFF\uFFF9-\uFFFB]'),
    byte_size    bigint NOT NULL CHECK (byte_size >= 0),
    reason       text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 200),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_job_output_refusals_run ON job_output_refusals (run_id);

-- 4. recovery_captures.reserved_bytes: the size a recovery upload's admission reserved, stamped
-- in the short admission transaction that runs under the stored-files lock. The shared sums
-- count 'available' captures by byte_size and 'preparing'/'uploading' captures by this column
-- (NULL counts as zero), so a chunk stream that runs outside the lock is still accounted for.
ALTER TABLE recovery_captures ADD COLUMN reserved_bytes bigint;
ALTER TABLE recovery_captures ADD CONSTRAINT recovery_captures_reserved_bytes_check
    CHECK (reserved_bytes IS NULL OR reserved_bytes >= 0);

-- 5. runs.job_protocol: the ROLLOUT GATE. NULL for every existing row and every non-job run;
-- CreateJobRun stamps 2 (the job_files_v1 protocol). ClaimRun's job clause lets a NULL row be
-- claimed by any job_runner_v1 worker and a stamped row only by a worker that also advertises
-- 'job_files_v1', so an api rolled ahead of the worker fleet never hands a job that needs files
-- to an image that cannot handle them.
ALTER TABLE runs ADD COLUMN job_protocol smallint;
ALTER TABLE runs ADD CONSTRAINT runs_job_protocol_check
    CHECK (job_protocol IS NULL OR (kind = 'job' AND job_protocol >= 2));

-- +goose Down

ALTER TABLE runs DROP CONSTRAINT runs_job_protocol_check;
ALTER TABLE runs DROP COLUMN job_protocol;
ALTER TABLE recovery_captures DROP CONSTRAINT recovery_captures_reserved_bytes_check;
ALTER TABLE recovery_captures DROP COLUMN reserved_bytes;
DROP TABLE job_output_refusals;
DROP TABLE job_file_chunks;
DROP TABLE job_files;
