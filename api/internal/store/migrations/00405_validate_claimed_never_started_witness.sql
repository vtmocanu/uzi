-- +goose Up
-- 00404 installed widened CHECKs without scanning the hot runs table under ACCESS EXCLUSIVE.
-- VALIDATE takes SHARE UPDATE EXCLUSIVE instead: ordinary reads and writes can continue.
ALTER TABLE runs VALIDATE CONSTRAINT runs_worker_recovery_evidence_check;
ALTER TABLE runs VALIDATE CONSTRAINT runs_recovery_wait_cause_check;

-- +goose Down
-- Validation has no data or schema reversal.
