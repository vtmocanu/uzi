-- Self-check fixture for scripts/check-migration-additive.sh (issue #1128).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/, NEVER under
-- api/internal/store/migrations/, so goose never applies it. Its job is to prove that a
-- $tag$ token sitting immediately after identifier characters (foo$tag$) is identifier text
-- in PostgreSQL -- where $ is a legal identifier char -- and NOT a dollar-quote opener, so it
-- must NOT open a literal that swallows the REAL worker-facing drop that FOLLOWS it -- exactly
-- ONE finding. Without the identifier-boundary check the scanner matches the $tag$ as an
-- opener; being unterminated, dqtag stays set across lines and strip_literals() blanks the
-- real ALTER TABLE workers DROP COLUMN version; below, dropping the count to 0 and exiting 2.
-- The drop is placed AFTER the foo$tag$ line on purpose so a swallow bug flips the count.
-- (CodeRabbit finding on PR #1375.)

-- +goose Up
SELECT foo$tag$ FROM workers;
ALTER TABLE workers DROP COLUMN version;

-- +goose Down
ALTER TABLE workers ADD COLUMN version text NOT NULL DEFAULT '';
