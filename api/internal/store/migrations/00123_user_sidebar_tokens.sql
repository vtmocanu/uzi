-- +goose Up

-- Which NON-DEFAULT Anthropic tokens the user chose to surface as rate meters
-- on the sidebar rail (web round-3 IA feedback: an explicit per-token choice,
-- not an automatic pick). The default token always shows and is never listed
-- here. NULL and '{}' both read as default-only, so an existing row needs no
-- backfill and an older server that never writes the column behaves as before.
-- A uuid[] column on users rather than a join table, following theme (00041)
-- and default_model (00031): it is a per-user UI preference, not a property of
-- the credential — a stale id (its token since deleted) simply matches nothing
-- when the web joins against the live token list.
--
-- NOTE (goose numbering): drafted as 00123 — the next free number above the
-- live head at drafting time — and renumbered at the landing merge if another
-- migration lands first, per the CLAUDE.md convention.
ALTER TABLE users ADD COLUMN sidebar_token_ids uuid[];

-- +goose Down
ALTER TABLE users DROP COLUMN sidebar_token_ids;
