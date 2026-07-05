-- +goose Up

-- Per-user theme override (PRD #21): the UI theme this user picked, overriding
-- the instance default. NULL means "use the instance default" (which itself
-- falls back to ember). One nullable scalar, so a column on users rather than a
-- new table — the same shape as default_model (PRD #17 Decision 3).
--
-- NOTE (goose numbering): drafted as 00040 above the live head; renumbered to
-- the next free number at the landing merge, per the CLAUDE.md convention.
ALTER TABLE users ADD COLUMN theme text;

-- +goose Down
ALTER TABLE users DROP COLUMN theme;
