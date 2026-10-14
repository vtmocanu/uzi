-- +goose Up
-- Owner-confirmed discard of a worker-held run_not_owned terminal outcome (issue #2984).
-- Two brand-new tables, additive only. The migration number is a draft: it is renumbered above
-- the live head at landing.
--
-- worker_blocked_outcomes: what a worker REPORTS about its blocked journals (wholesale replaced
-- by PUT /api/worker/blocked-outcomes). It is a claim by the holding worker, never proof; the
-- owner confirms against it and the worker recomputes the manifest before it deletes anything.
-- worker_outcome_discards: the owner's directive. confirmed_by is audit only and is NEVER used
-- for authorization (authorization joins workers.user_id and runs.user_id at request time), so
-- it carries no foreign key and survives a user delete.
SET LOCAL lock_timeout = '5s';

CREATE TABLE worker_blocked_outcomes (
    worker_id uuid NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    -- No FK: the run may be deleted while the worker still holds the journal.
    run_id uuid NOT NULL,
    claim_generation bigint NOT NULL CHECK (claim_generation >= 0 AND claim_generation <= 9007199254740991),
    journal_sha256 text NOT NULL CHECK (journal_sha256 ~ '^[0-9a-f]{64}$'),
    blocked_reason text NOT NULL CHECK (blocked_reason IN
        ('completion_permit_mismatch', 'gap_unrecoverable', 'reserve_exhausted', 'run_not_owned')),
    loss_complete boolean NOT NULL,
    -- Empty exactly when the report is incomplete.
    loss_sha256 text NOT NULL DEFAULT '' CHECK (loss_sha256 = '' OR loss_sha256 ~ '^[0-9a-f]{64}$'),
    incomplete_reason text NOT NULL DEFAULT '',
    loss jsonb,
    reported_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (worker_id, run_id, claim_generation),
    CONSTRAINT worker_blocked_outcomes_shape CHECK (
        (loss_complete AND loss IS NOT NULL AND loss_sha256 <> '' AND incomplete_reason = '')
        OR (NOT loss_complete AND loss IS NULL AND loss_sha256 = '' AND incomplete_reason <> '')),
    CONSTRAINT worker_blocked_outcomes_loss_size CHECK (loss IS NULL OR pg_column_size(loss) <= 2097152)
);
-- Retention sweep: rows not re-reported for 30 days.
CREATE INDEX idx_worker_blocked_outcomes_reported_at ON worker_blocked_outcomes (reported_at);

CREATE TABLE worker_outcome_discards (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    worker_id uuid NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    run_id uuid NOT NULL,
    claim_generation bigint NOT NULL CHECK (claim_generation >= 0 AND claim_generation <= 9007199254740991),
    journal_sha256 text NOT NULL CHECK (journal_sha256 ~ '^[0-9a-f]{64}$'),
    loss_sha256 text NOT NULL CHECK (loss_sha256 ~ '^[0-9a-f]{64}$'),
    confirmed_by uuid NOT NULL,
    evidence text NOT NULL CHECK (evidence = 'informed_decision'),
    state text NOT NULL CHECK (state IN
        ('authorized', 'executing', 'completed', 'completed_unknown', 'refused', 'expired', 'cancelled')),
    -- While authorized it equals expires_at; /begin sets it to now + the 120s lease.
    lease_until timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    -- Present exactly in the final states.
    result_reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT worker_outcome_discards_result_reason CHECK (
        (state IN ('completed', 'completed_unknown', 'refused', 'expired', 'cancelled')) = (result_reason IS NOT NULL))
);
-- At most one live directive per journal. INSERT ... ON CONFLICT repeats this predicate.
CREATE UNIQUE INDEX uq_worker_outcome_discards_active
    ON worker_outcome_discards (worker_id, run_id, claim_generation)
    WHERE state IN ('authorized', 'executing');
-- Retention sweep over final directives, and the sweep's due-expiry scan.
CREATE INDEX idx_worker_outcome_discards_final_updated
    ON worker_outcome_discards (updated_at)
    WHERE state IN ('completed', 'completed_unknown', 'refused', 'expired', 'cancelled');
CREATE INDEX idx_worker_outcome_discards_live
    ON worker_outcome_discards (expires_at, lease_until)
    WHERE state IN ('authorized', 'executing');

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE worker_outcome_discards;
DROP TABLE worker_blocked_outcomes;
