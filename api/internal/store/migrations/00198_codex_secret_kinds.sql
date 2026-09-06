-- +goose Up

-- Codex credentials foundation (PRD #1147 M1), STORE/SCHEMA only — ships DARK, no
-- execution path reads any of this yet. Two new user_secrets kinds join the one
-- 00010 opened the table with:
--   'openai_api_key' — a static OpenAI API key the user pastes, no subscription
--                      lifecycle behind it.
--   'codex_auth'     — a Codex subscription login whose authoritative state lives
--                      in codex_provider_account (00199) and whose per-alias link
--                      status lives in codex_credential_state (00200).
-- 00010 auto-named the column-level CHECK `user_secrets_kind_check`, so the ALTER
-- pair drops and re-adds under that exact name. 00087's
-- user_secrets_auto_eligible_kind_check is left untouched: auto-eligibility stays
-- meaningful only for anthropic_token, and neither codex kind opts into the pool.
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_kind_check;
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_kind_check
    CHECK (kind IN ('anthropic_token', 'openai_api_key', 'codex_auth'));

-- The codex kinds share ONE default across both of them: a user has a single
-- "active Codex credential", whether that is a static openai_api_key or a
-- subscription codex_auth. 00077's user_secrets_one_default_key is per-(user_id,
-- kind) and would let a user keep an openai_api_key default AND a codex_auth default
-- simultaneously — this partial index collapses the two codex kinds into one default
-- slot per user. anthropic_token keeps its own separate default via 00077's index,
-- so an anthropic default and a codex default coexist fine.
CREATE UNIQUE INDEX user_secrets_codex_one_default_key
    ON user_secrets (user_id)
    WHERE is_default AND kind IN ('openai_api_key', 'codex_auth');

-- +goose Down
DROP INDEX user_secrets_codex_one_default_key;
ALTER TABLE user_secrets DROP CONSTRAINT user_secrets_kind_check;
-- A downgrade removes the Codex feature, so it must remove its credentials too:
-- re-adding an anthropic_token-only CHECK while any openai_api_key/codex_auth row still
-- exists would fail the whole transaction. Under goose's strict reverse-order down, 00200's
-- Down has ALREADY dropped codex_credential_state (and 00199's Down dropped
-- codex_provider_account) before this DELETE runs, so there are no state or account rows
-- left to worry about — this DELETE only removes the now-orphaned user_secrets rows so the
-- narrowed CHECK below can be re-added.
DELETE FROM user_secrets WHERE kind IN ('openai_api_key', 'codex_auth');
ALTER TABLE user_secrets ADD CONSTRAINT user_secrets_kind_check
    CHECK (kind IN ('anthropic_token'));
