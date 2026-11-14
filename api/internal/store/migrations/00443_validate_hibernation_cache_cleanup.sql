-- +goose Up
-- First statement: each VALIDATE waits for its table lock, so the bound must cover them all.
SET LOCAL lock_timeout = '5s';
-- 00442 installed the workers CHECKs without scanning the table under ACCESS EXCLUSIVE.
-- VALIDATE takes SHARE UPDATE EXCLUSIVE instead: ordinary reads and writes can continue.
ALTER TABLE workers VALIDATE CONSTRAINT workers_cleanup_phase_check;
ALTER TABLE workers VALIDATE CONSTRAINT workers_hibernation_cleanup_check;
ALTER TABLE workers VALIDATE CONSTRAINT workers_hibernation_cleanup_reason_bounds;
ALTER TABLE workers VALIDATE CONSTRAINT workers_cleanup_reconcile_seq_check;
ALTER TABLE workers VALIDATE CONSTRAINT workers_cleanup_coherent;
ALTER TABLE workers VALIDATE CONSTRAINT workers_cleanup_started_coherent;
ALTER TABLE workers VALIDATE CONSTRAINT workers_cleanup_unknown_coherent;
ALTER TABLE workers VALIDATE CONSTRAINT workers_cleanup_settled_coherent;
ALTER TABLE workers VALIDATE CONSTRAINT workers_cleanup_fence_none;
ALTER TABLE workers VALIDATE CONSTRAINT workers_cleanup_fence_unsettled;

-- +goose Down
-- Validation has no data or schema reversal.
