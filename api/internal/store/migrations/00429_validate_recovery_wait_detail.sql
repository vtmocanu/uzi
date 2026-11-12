-- +goose Up
-- 00428 installed the CHECK without scanning the hot runs table under ACCESS EXCLUSIVE.
-- VALIDATE takes SHARE UPDATE EXCLUSIVE instead: ordinary reads and writes can continue.
ALTER TABLE runs VALIDATE CONSTRAINT runs_recovery_wait_detail_check;

-- +goose Down
-- Validation has no data or schema reversal.
