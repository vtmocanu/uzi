-- Self-check fixture for scripts/check-migration-additive.sh (issue #1128).
--
-- 🔴 NOT A REAL MIGRATION. It lives under scripts/, NEVER under
-- api/internal/store/migrations/, so goose never applies it. Its job is to prove that a
-- $$ or $tag$ token sitting INSIDE a single-quoted SQL string literal ('...') does NOT open
-- a dollar-quoted literal: the string is opaque content, so the REAL worker-facing drop that
-- FOLLOWS it MUST still be caught -- exactly ONE finding. Without single-quote tracking the
-- stray $$ would open a literal that swallowed every following line up to the next matching
-- $$ (there is none), blanking the real drop; the count would fall to 0 and the self-check
-- exits 2. The drop is placed AFTER the string on purpose so a swallow bug flips the count.

-- +goose Up
COMMENT ON TABLE workers IS 'billing text may contain $$ and $tag$ as literal placeholders';
ALTER TABLE workers DROP COLUMN version;

-- +goose Down
ALTER TABLE workers ADD COLUMN version text NOT NULL DEFAULT '';
