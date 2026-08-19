-- +goose Up

-- Per-user run-summary model override (PRD #362 M2): the SDK model this user's
-- ISSUE runs generate their plain-English summaries on. NULL means inherit the
-- instance summary_model (else the compiled-in default, "haiku"); a set value wins
-- for that user's runs only. Mirrors the per-user judge_model column (00125) — one
-- nullable scalar, so a column on users rather than a new table. Resolution is
-- user-value-wins at issue-run claim assembly (Decision 8).
--
-- NOTE (goose numbering): drafted as 00132 above the M1 head (00131_run_summaries);
-- renumber to the next free number above the live head at landing, per the
-- CLAUDE.md convention.
ALTER TABLE users ADD COLUMN summary_model text;

-- +goose Down
ALTER TABLE users DROP COLUMN summary_model;
