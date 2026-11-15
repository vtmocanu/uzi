-- +goose Up
-- 00454 installed the widened CHECK without scanning the hot runs table under ACCESS EXCLUSIVE.
-- VALIDATE takes SHARE UPDATE EXCLUSIVE instead: ordinary reads and writes can continue.
ALTER TABLE runs VALIDATE CONSTRAINT runs_health_check;

-- +goose Down
-- Validation has no data or schema reversal.
