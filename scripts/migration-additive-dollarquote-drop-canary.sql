-- Self-check fixture for scripts/check-migration-additive.sh (issue #1128).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/, NEVER under
-- api/internal/store/migrations/, so goose never applies it. Its job is to prove that a
-- DROP COLUMN-shaped fragment INSIDE a PostgreSQL dollar-quoted literal ($$ ... $$) is
-- string content, NOT a real statement: the ALTER TABLE workers DROP COLUMN version; below
-- lives inside a PL/pgSQL function body, so the guard MUST report ZERO findings. Before
-- scan() tracked dollar-quote state the `;` after PERFORM 1 inside the body wrongly cut the
-- buffer so the next fragment began `alter table ...` and was flagged; with the fix the
-- whole body is ignored. If dollar-quote awareness is lost the count rises to 1 and the
-- self-check exits 2. (CodeRabbit finding on PR #1126.)

-- +goose Up
CREATE FUNCTION uzi_migration_additive_dollarquote_drop_canary() RETURNS void AS $$
BEGIN
  PERFORM 1;
  ALTER TABLE workers DROP COLUMN version;
END
$$ LANGUAGE plpgsql;

-- +goose Down
DROP FUNCTION IF EXISTS uzi_migration_additive_dollarquote_drop_canary();
