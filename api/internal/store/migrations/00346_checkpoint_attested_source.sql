-- +goose Up

-- Issue #3194: --require-base exact-source attestation. A worker flight whose base guard strictly
-- accepted sends the packed source OID of the checkpoint it publishes; the api freezes it with the
-- tip at admission (checkpoint_publish_attempts.attested_source) and stores it on the run only
-- inside the same successful runs.checkpoint_tip write (runs.checkpoint_attested_source). The run
-- value is SET-ONLY: a publish without an attestation leaves it unchanged, nothing clears it. The
-- claim re-delivers it so a continuation may adopt exactly that source. Both columns are NULL for
-- legacy publishers and every row that predates this migration. Purely additive.
ALTER TABLE runs ADD COLUMN checkpoint_attested_source text
    CHECK (checkpoint_attested_source IS NULL OR checkpoint_attested_source ~ '^([0-9a-f]{40}|[0-9a-f]{64})$');
ALTER TABLE checkpoint_publish_attempts ADD COLUMN attested_source text
    CHECK (attested_source IS NULL OR attested_source ~ '^([0-9a-f]{40}|[0-9a-f]{64})$');

-- +goose Down
ALTER TABLE checkpoint_publish_attempts DROP COLUMN attested_source;
ALTER TABLE runs DROP COLUMN checkpoint_attested_source;
