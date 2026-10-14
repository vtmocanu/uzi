-- +goose Up
CREATE TABLE worker_pending_outcome_identities (
    worker_id uuid NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    claim_generation bigint NOT NULL CHECK (claim_generation >= 0),
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    reported_until timestamptz NOT NULL,
    PRIMARY KEY (worker_id, run_id, claim_generation)
);
CREATE INDEX worker_pending_outcome_identities_expiry_idx
    ON worker_pending_outcome_identities (reported_until);

-- +goose Down
DROP TABLE worker_pending_outcome_identities;
