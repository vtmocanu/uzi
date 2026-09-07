-- Self-check fixture for scripts/check-migration-additive.sh (issue #1087).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/, NEVER under
-- api/internal/store/migrations/, so goose never applies it. Its job is to prove the
-- allow-drop marker CANNOT over-exempt: the marker names a DIFFERENT column
-- (workers.created_at) than the one actually dropped (workers.version), so the guard MUST
-- STILL report the drop -- exactly ONE finding. If the match ever loosened to "any marker
-- in the section exempts any drop", this count would fall to 0 and the self-check exits 2.

-- +goose Up
-- migration-additive:allow-drop workers.created_at
ALTER TABLE workers DROP COLUMN version;

-- +goose Down
ALTER TABLE workers ADD COLUMN version text NOT NULL DEFAULT '';
