-- Canary fixture for scripts/check-migration-numbering.sh (PRD #500 M1).
--
-- 🔴 NOT A REAL MIGRATION. Sibling of 00001_a.sql. Both files share the number prefix
-- 00001 on purpose so the numbering guard's duplicate detector has a collision to find
-- when it scans this canary directory. See 00001_a.sql for the full rationale.

-- +goose Up
SELECT 1;

-- +goose Down
SELECT 1;
