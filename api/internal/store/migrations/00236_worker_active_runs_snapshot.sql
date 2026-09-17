-- +goose Up

-- PRD #1390 M2a: the worker active-run snapshot seam. Additive only, so an N-1 worker
-- during a rolling release keeps working: the new worker_active_runs table starts empty,
-- every existing worker row carries snapshot_epoch=0 / a NULL nonce / pending_overflow=false,
-- and every existing run leaves stale_requeue_generation NULL — today's behaviour, back-compat
-- by construction. The api only ever populates these from a worker that advertised the
-- active_run_snapshot protocol feature (D7), so an old worker never touches them.
--
-- worker_active_runs is the per-(worker, run) snapshot the worker reports on every heartbeat
-- and run-lane claim (D3): the run-lane attempts it is executing, each with the generation it
-- was claimed at, the phase the worker sees it in, and (for #1391) a terminal-pending lease.
-- The api stores the latest snapshot per worker by atomic full replacement under the worker's
-- epoch + register nonce. The table is empty at creation, so its phase CHECK is inline (an
-- empty table validates instantly — no NOT VALID + sibling VALIDATE step needed, unlike a
-- CHECK added to a populated table).
CREATE TABLE worker_active_runs (
    worker_id UUID NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    run_id UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    -- The generation the attempt was claimed at (runs.claim_generation, 00223). The exact
    -- generation is what makes re-adoption and claim-dedupe fence precisely (D4/D8).
    claim_generation BIGINT NOT NULL,
    -- The status the attempt is in from the worker's point of view. Judge/review attempts
    -- are always listed with phase 'running' (they hold slots). Closed to the run-lane's
    -- four live phases.
    phase TEXT NOT NULL CHECK (phase IN ('running', 'awaiting_approval', 'awaiting_input', 'awaiting_followup')),
    -- terminal_pending (#1391): the attempt's outcome is journaled on the worker but not yet
    -- accepted by the api. While terminal_pending_until > now() the run is under a server-side
    -- lease (D11) that every terminal writer honours, so a journaled outcome is never overtaken.
    terminal_pending BOOLEAN NOT NULL DEFAULT false,
    terminal_pending_until TIMESTAMPTZ NULL,
    -- The worker-process-monotonic snapshot counter this row came from (D3): it lets the api
    -- order snapshots captured independently by the heartbeat and claim loops and discard a
    -- delayed older one.
    snapshot_epoch BIGINT NOT NULL,
    -- The snapshot's own capture time (D3): ClaimRun's freshness test reads this, not the
    -- worker's heartbeat, so a worker whose snapshots fail validation cannot keep stale rows
    -- protected by mere liveness.
    reported_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (worker_id, run_id)
);

-- ClaimRun probes the table by run to decide whether any worker lists this run at its
-- current generation (D8), so the (run_id, claim_generation) index backs the dedupe read.
CREATE INDEX idx_worker_active_runs_run_gen ON worker_active_runs (run_id, claim_generation);

-- workers.snapshot_epoch: the highest snapshot epoch this worker has reported, so a stale
-- (lower-or-equal-epoch) snapshot is discarded on replacement. DEFAULT 0 is the reset value
-- a fresh register writes under the new nonce.
ALTER TABLE workers ADD COLUMN snapshot_epoch BIGINT NOT NULL DEFAULT 0;
-- workers.snapshot_register_nonce: the per-registration nonce every snapshot must echo (D3).
-- It must survive an api restart — that is the very moment it is checked — so it lives on the
-- worker row, not in memory. NULL for a worker that never registered under this feature.
ALTER TABLE workers ADD COLUMN snapshot_register_nonce TEXT;
-- workers.pending_overflow: the worker has more pending outcomes than it may list (#1391),
-- so its row-level leases cannot cover them all. While set, a worker-level closure stands in
-- for the missing row-level leases (D11). Refreshed by every valid flagged snapshot.
ALTER TABLE workers ADD COLUMN pending_overflow BOOLEAN NOT NULL DEFAULT false;
-- workers.pending_overflow_until: the closure's expiry — the dead-worker backstop, on the same
-- clock as the terminal-pending leases. Cleared by a valid unflagged snapshot or by expiry.
ALTER TABLE workers ADD COLUMN pending_overflow_until TIMESTAMPTZ;

-- runs.stale_requeue_generation: the generation the stale-worker requeue charged (D2 provenance).
-- The heartbeat re-adoption refunds the requeue charge only when this equals the generation being
-- restored; the restore and every claim clear it, so a legitimate earlier loss is never refunded.
-- NULL for a run the stale-worker requeue never charged.
ALTER TABLE runs ADD COLUMN stale_requeue_generation BIGINT;

-- +goose Down

-- True inverse of the additive Up (goose downs are not run in this deployment; store.Migrate
-- only ever goes up). Drop the columns in reverse, then the table (its index goes with it).
ALTER TABLE runs DROP COLUMN IF EXISTS stale_requeue_generation;

ALTER TABLE workers DROP COLUMN IF EXISTS pending_overflow_until;
ALTER TABLE workers DROP COLUMN IF EXISTS pending_overflow;
ALTER TABLE workers DROP COLUMN IF EXISTS snapshot_register_nonce;
ALTER TABLE workers DROP COLUMN IF EXISTS snapshot_epoch;

DROP TABLE worker_active_runs;
