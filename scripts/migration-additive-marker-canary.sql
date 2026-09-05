-- Self-check fixture for scripts/check-migration-additive.sh (issue #1087).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/, NEVER under
-- api/internal/store/migrations/, so goose never applies it. Its job is to prove the
-- allow-drop marker EXEMPTS exactly the table.column it names: the guard scans it and
-- MUST report ZERO findings. If the marker logic is deleted or broken, this worker-facing
-- DROP COLUMN is flagged again, the count becomes 1, and the self-check exits 2.

-- +goose Up
-- migration-additive:allow-drop workers.version
ALTER TABLE workers DROP COLUMN version;

-- +goose Down
ALTER TABLE workers ADD COLUMN version text NOT NULL DEFAULT '';
