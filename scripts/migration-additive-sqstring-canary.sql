-- Self-check fixture for scripts/check-migration-additive.sh (issue #1128).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/, NEVER under
-- api/internal/store/migrations/, so goose never applies it. Its job is to prove that a
-- $$ or $tag$ token sitting INSIDE a single-quoted SQL string literal ('...') does NOT open
-- a dollar-quoted literal; that a backslash-escaped quote inside a PostgreSQL escape string
-- (E'can\'t') does NOT close the literal early and reopen it over what follows; and that an
-- identifier ending in $E immediately before a string (foo$E'a\') is identifier text plus a
-- PLAIN string, NOT an E-string, so its backslash is literal and does not consume the closing
-- quote: the strings are opaque content, so the REAL worker-facing drop that FOLLOWS them MUST
-- still be caught -- exactly ONE finding. Without single-quote tracking the stray $$ would open
-- a literal that swallowed every following line up to the next matching $$ (there is none);
-- without E-string escape tracking the \' would close the E-string early, a later lone quote
-- would reopen it, and the real drop would be swallowed; and if the E-string boundary class
-- excluded $ (drifting from the dollar-quote opener's class), foo$E would be misread as an
-- E-string whose \' likewise swallows the drop. Any of those bugs blanks the real drop, the
-- count falls to 0 and the self-check exits 2. All three lines are placed BEFORE the drop on
-- purpose so a swallow bug flips the count.

-- +goose Up
COMMENT ON TABLE workers IS 'billing text may contain $$ and $tag$ as literal placeholders';
COMMENT ON TABLE workers IS E'an escaped quote \' here must not reopen the string';
SELECT foo$E'a\' FROM workers;
ALTER TABLE workers DROP COLUMN version;

-- +goose Down
ALTER TABLE workers ADD COLUMN version text NOT NULL DEFAULT '';
