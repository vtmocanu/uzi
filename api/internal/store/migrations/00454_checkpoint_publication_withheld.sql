-- +goose Up
-- First statement: the constraint swap below waits on runs locks, so the bound must cover it.
-- A lock wait past it fails the migration, which can be rerun.
SET LOCAL lock_timeout = '5s';
-- Issue #4257: a worker's content-free report that checkpoint publication is withheld by a
-- secret-scan restriction. The fact is fenced by (worker, claim generation, register nonce,
-- status_since) and ordered by the worker's report_seq; the health detector projects it as
-- 'publication_withheld'. Nothing here holds finding content.
ALTER TABLE runs
 ADD COLUMN checkpoint_withheld_since timestamptz,
 ADD COLUMN checkpoint_withheld_status_since timestamptz,
 ADD COLUMN checkpoint_withheld_worker_id uuid,
 ADD COLUMN checkpoint_withheld_generation bigint,
 ADD COLUMN checkpoint_withheld_nonce text,
 ADD COLUMN checkpoint_withheld_report_seq bigint;
ALTER TABLE runs DROP CONSTRAINT runs_health_check;
ALTER TABLE runs ADD CONSTRAINT runs_health_check CHECK
 (health IN ('ok', 'stalled', 'looping', 'slow', 'waiting_worker', 'approval_idle', 'waiting_account_cooldown', 'publication_withheld')) NOT VALID;
-- The CHECK is installed NOT VALID so no runs scan happens under ACCESS EXCLUSIVE; 00455 validates it.

-- +goose Down
SET LOCAL lock_timeout = '5s';
-- Serialise against live run writers while the Down rewrites rows and narrows the CHECK; a held
-- lock fails the rollback after 5s rather than queueing every runs writer behind it.
LOCK TABLE runs IN ACCESS EXCLUSIVE MODE;
UPDATE runs SET health = 'ok', health_reason = NULL, health_since = NULL
 WHERE health = 'publication_withheld';
ALTER TABLE runs DROP CONSTRAINT runs_health_check;
ALTER TABLE runs ADD CONSTRAINT runs_health_check CHECK
 (health IN ('ok', 'stalled', 'looping', 'slow', 'waiting_worker', 'approval_idle', 'waiting_account_cooldown'));
ALTER TABLE runs
 DROP COLUMN checkpoint_withheld_since,
 DROP COLUMN checkpoint_withheld_status_since,
 DROP COLUMN checkpoint_withheld_worker_id,
 DROP COLUMN checkpoint_withheld_generation,
 DROP COLUMN checkpoint_withheld_nonce,
 DROP COLUMN checkpoint_withheld_report_seq;
