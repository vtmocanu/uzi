-- +goose Up
SET LOCAL lock_timeout = '5s';
-- Issue #3639: one row per worker_memory_pressure park, written in the park transaction. It
-- records the released (worker, register nonce) pair, the worker's settlement receipt once one
-- is accepted, and whether the owner's Resume lifted the D19 released-pair exclusion for that
-- one park. worker_id has no foreign key, like runs.released_worker_id.
CREATE TABLE memory_park_releases (
    run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    claim_generation bigint NOT NULL CHECK (claim_generation > 0),
    memory_episode bigint NOT NULL CHECK (memory_episode >= 0),
    worker_id uuid NOT NULL,
    register_nonce text NOT NULL CHECK (length(register_nonce) BETWEEN 1 AND 128),
    released_at timestamptz NOT NULL,
    settlement bytea CHECK (octet_length(settlement) <= 524288),
    settlement_sha256 bytea CHECK (octet_length(settlement_sha256) = 32),
    settled_at timestamptz,
    lifted_at timestamptz,
    superseded_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, claim_generation),
    CHECK ((settlement IS NULL) = (settlement_sha256 IS NULL) AND (settlement IS NULL) = (settled_at IS NULL)),
    CHECK (lifted_at IS NULL OR settlement IS NOT NULL),
    CHECK (superseded_at IS NULL OR lifted_at IS NOT NULL)
);

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE memory_park_releases;
