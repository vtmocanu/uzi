-- +goose Up
-- Dormant planning state only. No enrollment defaults, backfill, or contract ledger.
-- The foreign keys briefly lock runs; time out rather than queue behind ClaimRun writers.
SET LOCAL lock_timeout = '5s';
CREATE TABLE semantic_planning (
 run_id uuid PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 enrollment jsonb NOT NULL,
 revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0 AND revision <= 128),
 fingerprint text,
 candidate jsonb,
 review_state jsonb
);
CREATE TABLE semantic_snapshots (
 run_id uuid NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 hash text NOT NULL,
 raw bytea NOT NULL,
 PRIMARY KEY (run_id, hash),
 CHECK (octet_length(raw) <= 262144)
);
-- Metadata is content-addressed separately so equal bytes from different
-- authorities deduplicate without losing provenance or completeness.
CREATE TABLE semantic_source_snapshots (
 run_id uuid NOT NULL,
 identity_hash text NOT NULL,
 source_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('issue', 'prd', 'plan')),
 hash text NOT NULL,
 provenance text NOT NULL,
 completeness text NOT NULL CHECK (completeness IN ('complete', 'unknown')),
 PRIMARY KEY (run_id, identity_hash),
 FOREIGN KEY (run_id, hash) REFERENCES semantic_snapshots(run_id, hash) ON DELETE CASCADE
);
CREATE TABLE semantic_candidate_history (
 run_id uuid NOT NULL REFERENCES semantic_planning(run_id) ON DELETE CASCADE,
 revision bigint NOT NULL CHECK (revision BETWEEN 1 AND 128),
 candidate jsonb NOT NULL,
 PRIMARY KEY (run_id, revision)
);
-- Snapshots and candidates retain history for the existing run lifetime.
-- +goose Down
DROP TABLE semantic_candidate_history;
DROP TABLE semantic_source_snapshots;
DROP TABLE semantic_snapshots;
DROP TABLE semantic_planning;
