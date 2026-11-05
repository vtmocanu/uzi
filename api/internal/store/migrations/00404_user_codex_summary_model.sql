-- +goose Up

-- Per-user Codex-lane run-summary model override (PRD #3248): the model this
-- user's Codex ISSUE runs generate their plain-English summaries on. NULL means
-- inherit the instance codex_summary_model (else the compiled-in default,
-- "gpt-6-luna"). summary_model (00132) stays the Claude lane. A set value wins
-- for that user's runs only; resolution is user-value-wins at issue-run claim
-- assembly. Mirrors the per-user codex_judge_model column.
--
-- NOTE (goose numbering): drafted as 00404 above the live head (00403); renumber
-- to the next free number above the live head at landing, per the CLAUDE.md
-- convention.
ALTER TABLE users ADD COLUMN codex_summary_model text;

-- +goose Down
ALTER TABLE users DROP COLUMN codex_summary_model;
