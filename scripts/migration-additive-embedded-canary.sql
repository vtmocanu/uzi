-- Self-check fixture for scripts/check-migration-additive.sh (issue #1087).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/, NEVER under
-- api/internal/store/migrations/, so goose never applies it. Its job is to prove that only
-- a STANDALONE `-- migration-additive:allow-drop <tbl>.<col>` comment line exempts a drop:
-- a marker INSIDE a string literal, or TRAILING a destructive statement on the same line,
-- must NOT populate the allow set. Both forms below name workers.version, yet the guard MUST
-- STILL report the drop -- exactly ONE finding. If the standalone-comment anchor is ever
-- dropped (the token matched anywhere in the line), this count falls to 0 and the
-- self-check exits 2. (CodeRabbit finding on PR #1126.)

-- +goose Up
-- A marker inside a string literal must NOT exempt anything.
INSERT INTO seed (note) VALUES ('migration-additive:allow-drop workers.version');
-- A marker trailing the destructive statement must NOT exempt it either.
ALTER TABLE workers DROP COLUMN version; -- migration-additive:allow-drop workers.version

-- +goose Down
ALTER TABLE workers ADD COLUMN version text NOT NULL DEFAULT '';
