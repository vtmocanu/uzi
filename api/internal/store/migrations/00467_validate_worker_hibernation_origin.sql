-- +goose Up
-- First statement: each VALIDATE waits for its table lock, so the bound must cover them all.
SET LOCAL lock_timeout = '5s';
-- 00466 added the workers CHECKs as NOT VALID, which defers validating existing rows to
-- this migration. VALIDATE takes SHARE UPDATE EXCLUSIVE: ordinary reads and writes can continue.
ALTER TABLE workers VALIDATE CONSTRAINT workers_hibernation_admit_origin_domain;
ALTER TABLE workers VALIDATE CONSTRAINT workers_hibernation_wake_origin_domain;
ALTER TABLE workers VALIDATE CONSTRAINT workers_hibernation_origin_coherent;

-- +goose Down
-- Validation has no data or schema reversal.
