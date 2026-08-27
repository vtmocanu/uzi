-- +goose Up

-- Per-user judge model override (PRD #69 M2): the SDK model this user's JUDGE
-- runs run on. NULL means inherit the instance judge_model (else the compiled-in
-- default); a set value wins for that user's judge runs only. Mirrors the
-- per-user default_model column (00031) — one nullable scalar, so a column on
-- users rather than a new table. Resolution is user-value-wins at judge-claim
-- assembly (Decision 5).
--
-- NOTE (goose numbering): drafted as 00067 in the PRD; renumbered to the next
-- free number above the live head (00124_swap_terraform_opentofu) at landing,
-- per the CLAUDE.md convention.
ALTER TABLE users ADD COLUMN judge_model text;

-- +goose Down
ALTER TABLE users DROP COLUMN judge_model;
