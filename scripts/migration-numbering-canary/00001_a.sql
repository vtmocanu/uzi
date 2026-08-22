-- Canary fixture for scripts/check-migration-numbering.sh (PRD #500 M1).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/migration-numbering-canary/, NEVER
-- under api/internal/store/migrations/, so goose's `//go:embed migrations/*.sql` never
-- reaches it and it is never applied. Its only job is to prove the numbering guard's
-- duplicate detector is live: this file and its sibling 00001_b.sql SHARE the number
-- prefix 00001, so the identical extraction the guard runs over the real corpus MUST
-- report that collision here. Zero collisions means the detector is blind (a changed
-- pattern, a mangled canary), so a "clean" corpus would mean nothing and the self-check
-- exits 2.

-- +goose Up
SELECT 1;

-- +goose Down
SELECT 1;
