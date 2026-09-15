-- Self-check fixture for scripts/check-migration-additive.sh (issue #1128).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/, NEVER under
-- api/internal/store/migrations/, so goose never applies it. Its job is to prove that an
-- allow-drop marker sitting on its OWN line INSIDE a PostgreSQL dollar-quoted literal
-- ($$ ... $$) is string content, NOT a standalone SQL comment, and so must NOT exempt a
-- later REAL drop. The marker below names workers.version from inside a DO $$ ... $$ block,
-- yet the guard MUST STILL report the real ALTER TABLE workers DROP COLUMN version; that
-- FOLLOWS the block -- exactly ONE finding. The expected finding is placed AFTER the block
-- on purpose: if scan() ever loses dollar-quote awareness the in-body marker would populate
-- the allow set and the count would fall to 0, while a bug that left the literal stuck open
-- would blank the real drop and the count would fall to 0 too -- either way != 1, so the
-- self-check exits 2. (CodeRabbit finding on PR #1126.)

-- +goose Up
DO $$
BEGIN
  -- migration-additive:allow-drop workers.version
  PERFORM 1;
END
$$;
ALTER TABLE workers DROP COLUMN version;

-- +goose Down
ALTER TABLE workers ADD COLUMN version text NOT NULL DEFAULT '';
