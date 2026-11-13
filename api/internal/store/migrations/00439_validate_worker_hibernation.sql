-- +goose Up
-- First statement: each VALIDATE waits for its table lock, so the bound must cover them all.
-- A lock wait past it fails the migration, which can be rerun.
SET LOCAL lock_timeout = '5s';
-- 00436 installed the workers CHECKs without scanning the table under ACCESS EXCLUSIVE.
-- VALIDATE takes SHARE UPDATE EXCLUSIVE instead: ordinary reads and writes can continue.
ALTER TABLE workers VALIDATE CONSTRAINT workers_hibernation_state_check;
ALTER TABLE workers VALIDATE CONSTRAINT workers_hibernation_coherent;
ALTER TABLE workers VALIDATE CONSTRAINT workers_hibernated_at_set;
ALTER TABLE workers VALIDATE CONSTRAINT workers_wake_requested_at_set;
ALTER TABLE workers VALIDATE CONSTRAINT workers_hibernation_text_bounds;

-- +goose Down
-- Validation has no data or schema reversal.
