-- +goose Up

-- Per-user default reasoning effort (PRD #617): the Agent SDK effort level this
-- user's runs use (low|medium|high|xhigh|max). NULL means inherit — we omit the
-- SDK `effort` key entirely, so the SDK default (`high`) applies unchanged. One
-- nullable scalar, so a column on users rather than a new table (sibling of
-- default_model 00031, PRD #17 Decision 3).
--
-- NOTE (goose numbering): number assigned at the landing merge; renumber to the
-- next free number above the live head if it drifts, per the CLAUDE.md convention.
ALTER TABLE users ADD COLUMN default_effort text;

-- +goose Down
ALTER TABLE users DROP COLUMN default_effort;
