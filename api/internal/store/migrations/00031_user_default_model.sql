-- +goose Up

-- Per-user default worker model (PRD #17): the SDK model this user's runs run
-- on. It overrides the lead template's model for the user's own runs and is
-- inherited by their null-model subagents; NULL means inherit (the lead
-- template's model, else the account/SDK default). One nullable scalar, so a
-- column on users rather than a new table (PRD #17 Decision 3).
--
-- NOTE (goose numbering): drafted as 00030, renumbered to 00031 at the landing
-- merge (PRD #5 landed 00030_privilege_report first), per the CLAUDE.md convention.
ALTER TABLE users ADD COLUMN default_model text;

-- +goose Down
ALTER TABLE users DROP COLUMN default_model;
