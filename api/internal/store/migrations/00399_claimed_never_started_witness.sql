-- +goose Up
-- Issue #3859: causal evidence that a worker flight departed before a claimed run ever
-- reported running. Both witnesses are bound to (claim_generation, worker register nonce,
-- snapshot epoch) so a later generation, a re-registration or a stale epoch cannot reuse them.
-- All columns are nullable and additive: NULL means no witness was recorded.
--
-- registration_evidence_*: written by the first running report (RecordRunRegistrationEvidence)
-- with the worker's own (claim_generation, register_nonce, registration epoch).
-- snapshot_departed_*: written inside an accepted active-snapshot replacement that omits the
-- run after an earlier witnessed epoch listed it or registration evidence preceded it.
ALTER TABLE runs
    ADD COLUMN registration_evidence_generation bigint,
    ADD COLUMN registration_evidence_nonce text,
    ADD COLUMN registration_evidence_epoch bigint,
    ADD COLUMN snapshot_departed_generation bigint,
    ADD COLUMN snapshot_departed_nonce text,
    ADD COLUMN snapshot_departed_epoch bigint;

-- +goose Down
ALTER TABLE runs
    DROP COLUMN snapshot_departed_epoch,
    DROP COLUMN snapshot_departed_nonce,
    DROP COLUMN snapshot_departed_generation,
    DROP COLUMN registration_evidence_epoch,
    DROP COLUMN registration_evidence_nonce,
    DROP COLUMN registration_evidence_generation;
