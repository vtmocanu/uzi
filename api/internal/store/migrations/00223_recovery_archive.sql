-- +goose Up

-- PRD #1296 M1 (D2/D3/D4/D5): the durable-recovery identity, custody and archive
-- storage contract. This migration is purely ADDITIVE — one ADD COLUMN on runs plus
-- four brand-new tables — so an N-1 worker/api reading the pre-existing surface during
-- a rolling release is unaffected (check-migration-additive scans only destructive Up
-- verbs on worker-facing tables; ADD COLUMN and CREATE TABLE are neither).

-- D2: a GENERAL claim-lane counter, incremented once per successful ClaimRun
-- transaction (runtime.sql). NOT NULL DEFAULT 0 with a constant default, so the add is
-- rewrite-free in modern Postgres and every existing row reads 0 (never claimed under
-- the recovery contract). This is DELIBERATELY separate from runs.codex_claim_epoch,
-- which belongs to the Codex credential-capability lifecycle and must not be repurposed.
ALTER TABLE runs ADD COLUMN claim_generation BIGINT NOT NULL DEFAULT 0;

-- D2/D3: a claim-scoped CUSTODY HOLD. Created H-free in the SAME successful ClaimRun
-- transaction as the generation increment (runtime.sql), for a recovery-capable worker
-- on a code-publishing profile. It reserves owner-scoped custody capacity BEFORE model
-- work starts, with no crypto or byte-quota dependency; an archive capture is a later
-- H-bound artifact bound to this hold.
CREATE TABLE recovery_custody_holds (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Owner/run/repo are PLAIN columns, NOT ON DELETE CASCADE FKs (D3): an owner, repo
    -- or account delete must NOT silently cascade a hold away. Explicit disposition is
    -- enforced in M4; a cascade here would destroy the last recovery source unseen.
    user_id uuid NOT NULL,
    repo_id uuid,
    run_id uuid NOT NULL,
    -- The claim generation this hold was created under (runs.claim_generation at claim).
    generation bigint NOT NULL,
    -- Lifecycle: 'open' while custody is active, 'released' on successful
    -- publication/no-output disposition, 'discarded' on explicit owner discard.
    state text NOT NULL,
    -- IMMUTABLE PROVENANCE (D1/D2): the value identity of the worker that took the
    -- claim, used later for AAD/authorization of a post-terminal recovery retry. These
    -- are VALUE columns (no FK) and are NEVER nulled after insert — they must survive the
    -- worker row's deletion so a preauthorized upload can still be authenticated.
    original_worker_id uuid NOT NULL,
    original_worker_identity text NOT NULL,
    -- TWO SEPARATE nullable LIVE FKs for active-custody enforcement (D3). While a hold is
    -- OPEN both are non-null and ON DELETE RESTRICT, so deleting the worker or the run is
    -- blocked (the M4 DELETE guards read these); RELEASE sets both NULL, which drops the
    -- restriction and lets ordinary teardown proceed. Kept distinct from the immutable
    -- provenance above precisely so release can null the live pointers without erasing who
    -- held custody.
    live_worker_id uuid REFERENCES workers(id) ON DELETE RESTRICT,
    live_run_id uuid REFERENCES runs(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    released_at timestamptz
);

-- Owner-scoped open-hold lookups: the custody-admission count in ClaimRun and the
-- CountUnresolvedCustodyHoldsForOwner health-reason read (D4). Partial on state='open'.
CREATE INDEX idx_recovery_custody_holds_owner_open
    ON recovery_custody_holds (user_id) WHERE state = 'open';
-- Per-run hold lookups (release, summary aggregates).
CREATE INDEX idx_recovery_custody_holds_run ON recovery_custody_holds (run_id);
-- The M4 worker-deletion guard reads the live worker FK.
CREATE INDEX idx_recovery_custody_holds_live_worker ON recovery_custody_holds (live_worker_id);

-- D2/D4/D5: an ARCHIVE CAPTURE — a later H-bound immutable artifact under an open hold.
-- H need not exist at claim time; the capture freezes its parent hold, original H and an
-- idempotency identity, and the server mints capture_id (this row's id). The byte
-- manifest is bound ONCE via CAS (manifest_bound) before chunks are accepted.
CREATE TABLE recovery_captures (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- A capture is NOT cascaded by hold release (ON DELETE RESTRICT): releasing custody
    -- must never destroy an already-registered archive.
    hold_id uuid NOT NULL REFERENCES recovery_custody_holds(id) ON DELETE RESTRICT,
    -- Owner/run are plain columns (no ON DELETE CASCADE from owner/repo/run/worker, D3):
    -- deleting a run/owner must not silently drop the capture.
    run_id uuid NOT NULL,
    user_id uuid NOT NULL,
    -- Provenance of the capturing worker. original_worker_id is a plain column (may be
    -- nulled/left dangling once the worker is deleted after release — the capture still
    -- succeeds), while original_worker_identity is the IMMUTABLE value bound into AAD and
    -- is never nulled.
    original_worker_id uuid,
    original_worker_identity text NOT NULL,
    -- source_sha is the original committed head H (required). attempted_head_sha is the
    -- provenance H' of any attempted publication (nullable — recording H' is provenance,
    -- never permission to substitute it for H).
    source_sha text NOT NULL,
    attempted_head_sha text,
    -- Idempotency identity from the worker's durable source journal. UNIQUE per hold, so
    -- retrying the same capture identity returns the same row (ReserveCapture upsert).
    idempotency_key text NOT NULL,
    -- Lifecycle: preparing -> uploading -> available, or needs_action/expired/discarded.
    state text NOT NULL,
    -- Manifest-bound-once via CAS (D2): flips false->true exactly once when the byte
    -- manifest is bound; a retry with the SAME manifest is idempotent, a DIFFERENT
    -- manifest conflicts (BindCaptureManifest).
    manifest_bound bool NOT NULL DEFAULT false,
    byte_size bigint,
    checksum text,
    chunk_count int,
    -- The verified public prerequisite closure the bundle imports against (D5).
    prerequisite_shas text[],
    -- Bounded, sanitized labels (D6): a specific reason/context for a needs_action/expired
    -- capture. Never a secret sample.
    reason text,
    context text,
    -- A ready artifact's expiry begins at durable capture (MarkCaptureReady), not at the
    -- earlier hold/reservation (D4). Pending custody never expires with it.
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Retrying the same capture identity under one hold returns the same record (D2).
    UNIQUE (hold_id, idempotency_key)
);

CREATE INDEX idx_recovery_captures_run_owner ON recovery_captures (run_id, user_id);
CREATE INDEX idx_recovery_captures_hold ON recovery_captures (hold_id);
-- The expiry sweep scans available captures past their expiry (ExpireReadyCaptures).
CREATE INDEX idx_recovery_captures_expiry ON recovery_captures (expires_at) WHERE state = 'available';

-- D4: ordered encrypted byte chunks for one capture. ON DELETE CASCADE from the capture
-- (unlike the hold->capture RESTRICT above): a discarded capture's chunks go with it.
-- sealed is the AAD-bound ciphertext (~1 MiB per chunk); length is the plaintext length.
CREATE TABLE recovery_capture_chunks (
    capture_id uuid NOT NULL REFERENCES recovery_captures(id) ON DELETE CASCADE,
    chunk_index int NOT NULL,
    length int NOT NULL,
    sealed bytea NOT NULL,
    PRIMARY KEY (capture_id, chunk_index)
);

-- +goose Down

DROP TABLE recovery_capture_chunks;
DROP TABLE recovery_captures;
DROP TABLE recovery_custody_holds;
ALTER TABLE runs DROP COLUMN claim_generation;
